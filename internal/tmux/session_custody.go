package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/uuid"
	"github.com/steveyegge/gastown/internal/config"
)

const (
	maxSessionCustodyPaths      = 64
	maxSessionCustodyPathsBytes = 16 * 1024
)

// EncodeSessionCustodyPaths validates and serializes the trusted launcher's
// filesystem allowlist. Linux consumes it inside the namespace init; keeping
// the encoder platform-neutral lets lifecycle code remain portable.
func EncodeSessionCustodyPaths(paths []string) (string, error) {
	unique := make(map[string]struct{}, len(paths))
	cleaned := make([]string, 0, len(paths))
	for _, path := range paths {
		path = filepath.Clean(strings.TrimSpace(path))
		if !filepath.IsAbs(path) || path == string(filepath.Separator) {
			return "", fmt.Errorf("session custody allowlist path is not a bounded absolute path: %q", path)
		}
		if _, exists := unique[path]; exists {
			continue
		}
		unique[path] = struct{}{}
		cleaned = append(cleaned, path)
	}
	if len(cleaned) == 0 || len(cleaned) > maxSessionCustodyPaths {
		return "", fmt.Errorf("session custody allowlist has %d paths", len(cleaned))
	}
	encoded, err := json.Marshal(cleaned)
	if err != nil {
		return "", fmt.Errorf("encoding session custody allowlist: %w", err)
	}
	if len(encoded) > maxSessionCustodyPathsBytes {
		return "", fmt.Errorf("session custody allowlist is %d bytes", len(encoded))
	}
	return string(encoded), nil
}

// ErrSessionCustodyUnsupported means that the pane was not launched inside a
// verifiable OS-owned containment boundary. Destructive zombie replacement
// must fail closed rather than fall back to ancestry scanning.
var ErrSessionCustodyUnsupported = errors.New("generation-bound session containment is unavailable")

// SessionCustodyLaunchSupported reports whether this platform can launch the
// trusted parent-side broker required for destructive lifecycle work.
func SessionCustodyLaunchSupported() bool { return sessionCustodyLaunchSupported() }

type sessionCustodyHandle interface {
	Freeze(context.Context) error
	Kill(context.Context) (bool, error)
	Thaw() error
	Close() error
}

// sessionCustodyParentReleaseHandle splits committed teardown around release of
// the exact tmux generation. On Linux, tmux is the supervisor's parent and may
// need to process kill-session before the exited supervisor can be reaped.
type sessionCustodyParentReleaseHandle interface {
	KillBeforeParentRelease(context.Context) (bool, error)
	FinalizeAfterParentRelease(context.Context) error
	ParentReleaseFinalizationPending() bool
}

// WrapSessionCommandWithCustody wraps a long-lived agent command with the
// hidden launcher that establishes an OS-owned containment boundary before the
// command can fork. Unsupported platforms return the command unchanged; their
// later zombie cleanup therefore fails closed.
func WrapSessionCommandWithCustody(gtBinary, command string) (wrapped, custody string, err error) {
	if !sessionCustodyLaunchSupported() {
		return command, "", nil
	}
	if !filepath.IsAbs(gtBinary) {
		return "", "", errors.New("session custody launcher requires an absolute gt binary path")
	}
	if strings.TrimSpace(command) == "" {
		return "", "", errors.New("session custody launcher requires a command")
	}
	custody = uuid.NewString()
	wrapped = "exec " + config.ShellQuote(gtBinary) + " session-custody --id " + config.ShellQuote(custody) + " -- " + config.ShellQuote(command)
	return wrapped, custody, nil
}

// CanWrapCurrentExecutable reports whether this process is the installed gt
// binary rather than a Go test binary. Tests may inject a wrapper explicitly.
func CanWrapCurrentExecutable(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return base == "gt" || (runtime.GOOS == "windows" && base == "gt.exe")
}

// RunSessionCustodyCommand is the implementation of the hidden launcher.
func RunSessionCustodyCommand(custody, command string) error {
	return RunSessionCustodyCommandWithBroker(custody, command, func([]string) error {
		return errors.New("session broker command policy is unavailable")
	})
}

// RunSessionCustodyCommandWithBroker launches the contained workload and
// serves only commands approved by validate through the trusted outer broker.
func RunSessionCustodyCommandWithBroker(custody, command string, validate SessionBrokerValidator) error {
	return RunSessionCustodyCommandWithBrokerPolicy(custody, command, validate, nil)
}

// RunSessionCustodyCommandWithBrokerPolicy also accepts a narrow policy for
// workers that must finish after the requesting session begins teardown.
func RunSessionCustodyCommandWithBrokerPolicy(custody, command string, validate SessionBrokerValidator, detach SessionBrokerDetachPolicy) error {
	return RunSessionCustodyCommandWithBrokerExecutorPolicy(custody, command, validate, detach, nil)
}

// RunSessionCustodyCommandWithBrokerExecutorPolicy also permits a narrow
// trusted-parent executor for commands that must never re-enter public Cobra.
func RunSessionCustodyCommandWithBrokerExecutorPolicy(custody, command string, validate SessionBrokerValidator, detach SessionBrokerDetachPolicy, execute SessionBrokerExecutor) error {
	if !validSessionGenerationRe.MatchString(custody) {
		return errors.New("invalid session custody token")
	}
	if inherited := os.Getenv(EnvSessionCustody); inherited != custody {
		return fmt.Errorf("session custody token does not match inherited %s", EnvSessionCustody)
	}
	if strings.TrimSpace(command) == "" {
		return errors.New("session custody command is empty")
	}
	if validate == nil {
		return errors.New("session broker validator is unavailable")
	}
	return runSessionWithCustody(custody, command, validate, detach, execute)
}

// RunSessionCustodyInit enters the trusted namespace-init path selected by the
// hidden internal command. Ordinary invocations are rejected.
func RunSessionCustodyInit() error {
	requested, err := runSessionCustodyInit()
	if !requested {
		return errors.New("session custody init was not requested by a trusted launcher")
	}
	return err
}
