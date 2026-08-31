// poller.go provides a background nudge-queue poller for agents that lack
// turn-boundary drain hooks (e.g., Gemini, Codex). Claude Code drains its
// queue via the UserPromptSubmit hook on every turn. Other runtimes have no
// equivalent hook, so queued nudges would sit undelivered forever.
//
// The poller runs as a background goroutine launched by crew/manager.Start().
// It polls the queue every PollInterval, waits for the agent to be idle, then
// drains and injects the formatted nudges via tmux NudgeSession.
//
// Lifecycle: StartPoller() → background loop → StopPoller() (or session death).
// A PID file at <townRoot>/.runtime/nudge_poller/<session>.pid allows Stop()
// to clean up even if the original manager has been replaced.
package nudge

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/util"
)

// Poller tuning defaults (overridable via flags or tests).
var (
	// DefaultPollInterval is how often the poller checks the queue.
	DefaultPollInterval = "10s"
	// DefaultIdleTimeout is how long to wait for the agent to become idle
	// before skipping this poll cycle and trying again next interval.
	DefaultIdleTimeout = "3s"
)

// pollerPidDir returns the directory for poller PID files.
func pollerPidDir(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "nudge_poller")
}

// pollerPidFile returns the PID file path for a session's poller.
func pollerPidFile(townRoot, session string) string {
	safe := strings.ReplaceAll(session, "/", "_")
	return filepath.Join(pollerPidDir(townRoot), safe+".pid")
}

func pollerStopFile(townRoot, session string) string {
	return pollerPidFile(townRoot, session) + ".stop"
}

// StartPoller launches a background `gt nudge-poller <session>` process.
// The process is detached (Setpgid) so it survives the caller's exit.
// Returns the PID of the launched process, or an error.
func StartPoller(townRoot, session string) (int, error) {
	return startPoller(townRoot, session, nil)
}

// StartPollerWithEnv launches a poller with an explicit child environment.
// Isolated callers use this to preserve their private tmux/config boundary.
func StartPollerWithEnv(townRoot, session string, env []string) (int, error) {
	return startPoller(townRoot, session, env)
}

// StartPollerWithExecutable launches a poller from the exact executable supplied
// by an isolated caller. This keeps the recorded process identity stable when
// the caller itself is a test binary rather than gt.
func StartPollerWithExecutable(townRoot, session, executable string, env []string) (int, error) {
	if strings.TrimSpace(executable) == "" {
		return 0, errors.New("poller executable is empty")
	}
	launcher := func(townRoot, session string, env []string) (pollerLaunch, error) {
		return launchPollerExecutable(executable, townRoot, session, env)
	}
	return startPollerWithLauncher(townRoot, session, env, launcher, os.WriteFile)
}

// StopRequested reports whether the session's cooperative stop generation is set.
func StopRequested(townRoot, session string) bool {
	data, err := os.ReadFile(pollerStopFile(townRoot, session))
	if err != nil {
		return false
	}
	current, err := os.ReadFile(pollerPidFile(townRoot, session))
	return err == nil && bytes.Equal(data, current)
}

func startPoller(townRoot, session string, env []string) (int, error) {
	return startPollerWithLauncher(townRoot, session, env, launchPoller, os.WriteFile)
}

type pollerLaunch struct {
	pid       int
	identity  pollerIdentity
	release   func() error
	terminate func() error
}

type pollerIdentity struct {
	StartTime  string
	Command    string
	Generation string
	Transport  string
}

type pollerRecord struct {
	PID      int
	Identity pollerIdentity
	Session  string
	Legacy   bool
}

// PollerGeneration identifies the exact ownership record observed by a caller.
// Its fields are intentionally private so only this package can interpret or
// mutate poller custody.
type PollerGeneration struct {
	record []byte
	exists bool
}

func startPollerWithLauncher(
	townRoot, session string,
	env []string,
	launcher func(string, string, []string) (pollerLaunch, error),
	writePID func(string, []byte, os.FileMode) error,
) (int, error) {
	return startPollerWithLauncherStatus(townRoot, session, env, launcher, writePID, pollerStatus)
}

func startPollerWithLauncherStatus(
	townRoot, session string,
	env []string,
	launcher func(string, string, []string) (pollerLaunch, error),
	writePID func(string, []byte, os.FileMode) error,
	status func(string, string) (int, bool, error),
) (int, error) {
	pidDir := pollerPidDir(townRoot)
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		return 0, fmt.Errorf("creating poller pid dir: %w", err)
	}

	startLock, err := lockPoller(townRoot, session)
	if err != nil {
		return 0, err
	}
	defer func() { _ = startLock.Unlock() }()
	env = effectivePollerEnv(env)
	effectiveTransport := normalizePollerTransport(env)

	// Reconcile liveness before enforcing transport custody. A positively dead
	// pre-transport or mismatched record is safe for status to remove; only a
	// live owner may block reuse on an unprovable transport.
	if pid, alive, statusErr := status(townRoot, session); statusErr != nil {
		return 0, statusErr
	} else if alive {
		data, readErr := os.ReadFile(pollerPidFile(townRoot, session))
		if readErr != nil {
			return 0, fmt.Errorf("reading live poller ownership: %w", readErr)
		}
		record, parseErr := parsePollerRecord(string(data))
		if parseErr != nil {
			return 0, fmt.Errorf("parsing live poller ownership: %w", parseErr)
		}
		if record.Identity.Transport == "" || record.Identity.Transport != effectiveTransport {
			return 0, errors.New("poller transport mismatch; preserving ownership")
		}
		return pid, nil // already running
	}
	_ = os.Remove(pollerStopFile(townRoot, session))

	launched, err := launcher(townRoot, session, env)
	if err != nil {
		return 0, err
	}

	// Write PID file for later cleanup.
	pidPath := pollerPidFile(townRoot, session)
	if launched.identity.StartTime == "" || launched.identity.Command == "" || launched.identity.Generation == "" || session == "" {
		return 0, cleanupLaunchedPoller(launched, errors.New("nudge-poller identity unavailable"))
	}
	launched.identity.Transport = effectiveTransport
	record := formatPollerRecord(launched.pid, launched.identity, session)
	if err := writePID(pidPath, []byte(record), 0644); err != nil {
		persistErr := fmt.Errorf("persisting nudge-poller PID: %w", err)
		if launched.terminate == nil {
			return 0, persistErr
		}
		if terminateErr := launched.terminate(); terminateErr != nil {
			return 0, errors.Join(persistErr, fmt.Errorf("terminating untracked nudge-poller: %w", terminateErr))
		}
		return 0, persistErr
	}
	if launched.release != nil {
		_ = launched.release()
	}

	return launched.pid, nil
}

func effectivePollerEnv(env []string) []string {
	if env == nil {
		return tmux.NewTmux().PollerEnvironment()
	}
	return env
}

func normalizePollerTransport(env []string) string {
	var town, tmux string
	for _, entry := range env {
		if strings.HasPrefix(entry, "GT_TOWN_SOCKET=") {
			town = strings.TrimPrefix(entry, "GT_TOWN_SOCKET=")
		}
		if strings.HasPrefix(entry, "GT_TMUX_SOCKET=") {
			tmux = strings.TrimPrefix(entry, "GT_TMUX_SOCKET=")
		}
	}
	if town == "" {
		town = tmux
	}
	if tmux == "" {
		tmux = town
	}
	return town + "\x00" + tmux
}

func lockPoller(townRoot, session string) (*flock.Flock, error) {
	lockPath := pollerPidFile(townRoot, session) + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		return nil, fmt.Errorf("creating nudge-poller lock dir: %w", err)
	}
	startLock := flock.New(lockPath)
	if err := startLock.Lock(); err != nil {
		return nil, fmt.Errorf("acquiring nudge-poller start lock: %w", err)
	}
	return startLock, nil
}

func lockPollerContext(ctx context.Context, townRoot, session string) (*flock.Flock, error) {
	lockPath := pollerPidFile(townRoot, session) + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		return nil, fmt.Errorf("creating nudge-poller lock dir: %w", err)
	}
	startLock := flock.New(lockPath)
	locked, err := startLock.TryLockContext(ctx, pollerExitInterval)
	if err != nil {
		return nil, fmt.Errorf("acquiring nudge-poller start lock: %w", err)
	}
	if !locked {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("acquiring nudge-poller start lock: lock unavailable")
	}
	return startLock, nil
}

func launchPoller(townRoot, session string, env []string) (pollerLaunch, error) {
	// Find the gt binary.
	gtBin, err := os.Executable()
	if err != nil {
		return pollerLaunch{}, fmt.Errorf("finding gt binary: %w", err)
	}
	return launchPollerExecutable(gtBin, townRoot, session, env)
}

func launchPollerExecutable(gtBin, townRoot, session string, env []string) (pollerLaunch, error) {
	cmd := buildPollerCommand(gtBin, townRoot, session, env)
	var generation [16]byte
	if _, err := rand.Read(generation[:]); err != nil {
		return pollerLaunch{}, fmt.Errorf("creating poller generation: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return pollerLaunch{}, fmt.Errorf("starting nudge-poller: %w", err)
	}
	identity := pollerIdentityForProcess(cmd.Process.Pid)
	identity.Generation = fmt.Sprintf("%x", generation[:])

	return pollerLaunch{
		pid:      cmd.Process.Pid,
		identity: identity,
		release:  cmd.Process.Release,
		terminate: func() error {
			killErr := cmd.Process.Kill()
			waitErr := cmd.Wait()
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				waitErr = nil
			}
			if errors.Is(killErr, os.ErrProcessDone) {
				killErr = nil
			}
			return errors.Join(killErr, waitErr)
		},
	}, nil
}

func formatPollerRecord(pid int, identity pollerIdentity, sessionName string) string {
	if identity.Generation == "" {
		return ""
	}
	b, _ := json.Marshal(pollerRecord{PID: pid, Identity: identity, Session: sessionName})
	return string(b) + "\n"
}

func formatPollerRecordValue(record pollerRecord) string {
	return formatPollerRecord(record.PID, record.Identity, record.Session)
}

func parsePollerRecord(value string) (pollerRecord, error) {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, "{") {
		pid, err := strconv.Atoi(trimmed)
		if err != nil || pid <= 0 {
			return pollerRecord{}, errors.New("invalid nudge-poller ownership record")
		}
		return pollerRecord{PID: pid, Legacy: true}, nil
	}
	var record pollerRecord
	if err := json.Unmarshal([]byte(trimmed), &record); err != nil || record.PID <= 0 || record.Identity.StartTime == "" || record.Identity.Command == "" || record.Identity.Generation == "" || record.Session == "" {
		return pollerRecord{}, errors.New("invalid nudge-poller ownership record")
	}
	return record, nil
}

func pollerIdentityForProcess(pid int) pollerIdentity {
	start, err := session.ProcessStartTime(pid)
	if err != nil {
		return pollerIdentity{}
	}
	cmd := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid))
	util.SetDetachedProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return pollerIdentity{}
	}
	return pollerIdentity{StartTime: strings.TrimSpace(start), Command: strings.TrimSpace(string(out))}
}

func pollerIdentityMatches(identity pollerIdentity, record pollerRecord, sessionName string) bool {
	if record.Session != sessionName || identity.StartTime != record.Identity.StartTime || strings.TrimSpace(identity.Command) != strings.TrimSpace(record.Identity.Command) {
		return false
	}
	return pollerCommandMatches(identity.Command, sessionName)
}

func pollerCommandMatches(command, sessionName string) bool {
	fields := strings.Fields(command)
	return len(fields) >= 3 && filepath.Base(fields[len(fields)-3]) == "gt" && fields[len(fields)-2] == "nudge-poller" && fields[len(fields)-1] == sessionName
}

func cleanupLaunchedPoller(launched pollerLaunch, cause error) error {
	if launched.terminate == nil {
		return cause
	}
	if err := launched.terminate(); err != nil {
		return errors.Join(cause, fmt.Errorf("terminating untracked nudge-poller: %w", err))
	}
	return cause
}

func buildPollerCommand(gtBin, townRoot, session string, env []string) *exec.Cmd {
	cmd := exec.Command(gtBin, "nudge-poller", session)
	cmd.Dir = townRoot
	if env != nil {
		cmd.Env = append([]string(nil), env...)
	}
	cmd.Stdout = nil // discard
	cmd.Stderr = nil // discard
	util.SetDetachedProcessGroup(cmd)
	return cmd
}

// StopPoller terminates the nudge-poller for a session, if running.
func StopPoller(townRoot, session string) error {
	return stopPoller(townRoot, session, nil)
}

// CapturePollerGeneration records the exact poller ownership generation for a
// later generation-bound stop. Absence is also captured so a newly created
// poller cannot be mistaken for the inspected generation.
func CapturePollerGeneration(townRoot, session string) (PollerGeneration, error) {
	lock, err := lockPoller(townRoot, session)
	if err != nil {
		return PollerGeneration{}, err
	}
	defer func() { _ = lock.Unlock() }()

	data, err := os.ReadFile(pollerPidFile(townRoot, session))
	if err != nil {
		if os.IsNotExist(err) {
			return PollerGeneration{}, nil
		}
		return PollerGeneration{}, fmt.Errorf("reading poller ownership generation: %w", err)
	}
	return PollerGeneration{record: append([]byte(nil), data...), exists: true}, nil
}

// StopPollerGeneration stops only the exact ownership generation previously
// captured by CapturePollerGeneration.
func StopPollerGeneration(townRoot, session string, generation PollerGeneration) error {
	return stopPoller(townRoot, session, &generation)
}

// StopPollerGenerationContext stops the exact captured generation while
// honoring cancellation during lock acquisition and exit confirmation.
func StopPollerGenerationContext(ctx context.Context, townRoot, session string, generation PollerGeneration) error {
	lock, err := lockPollerContext(ctx, townRoot, session)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	return stopPollerWithExpectedGenerationOpsLocked(
		townRoot, session, &generation,
		os.ReadFile, pollerProcessAlive, lookupPollerIdentity,
		func(data []byte) error { return os.WriteFile(pollerStopFile(townRoot, session), data, 0600) },
		func(pid int, record pollerRecord) error { return waitPollerExitContext(ctx, pid, record) },
		func(path string, data []byte) error {
			return quarantinePollerRecord(path, data, func(destination string) error { return os.Rename(path, destination) })
		},
		os.Remove,
	)
}

type pollerTransitionOps struct {
	read       func(string) ([]byte, error)
	alive      func(int) bool
	identity   func(int) (pollerIdentity, error)
	stop       func([]byte) error
	wait       func(context.Context, int, pollerRecord) error
	quarantine func(string, []byte) error
	remove     func(string) error
}

// PollerReplacementCommit performs a prepared replacement and reports whether
// its first destructive signal was issued. A false result is still pre-commit
// and must preserve the old poller generation.
type PollerReplacementCommit func(context.Context) (bool, error)

// PollerReplacementOutcome separates the irreversible commit fact from proof
// that the old session reached a terminal state. A committed but non-terminal
// cleanup must preserve the exact poller generation for later reconciliation.
type PollerReplacementOutcome struct {
	Committed bool
	Terminal  bool
}

type PollerReplacementOutcomeCommit func(context.Context) (PollerReplacementOutcome, error)

var ErrPollerPreservedAfterCommittedCleanup = errors.New("poller preserved after committed cleanup without terminal session reconciliation")

// ReplaceBeforeStoppingPollerGenerationContext keeps the exact poller alive
// through nondestructive replacement preparation. Once the returned commit
// function issues its first destructive signal, exact-poller reconciliation is
// mandatory even if later cleanup or verification reports an error.
func ReplaceBeforeStoppingPollerGenerationContext(
	ctx context.Context,
	townRoot, session string,
	generation PollerGeneration,
	postCommitTimeout time.Duration,
	prepare func(context.Context) (PollerReplacementCommit, error),
) error {
	return replaceBeforeStoppingPollerGenerationContext(
		ctx,
		townRoot,
		session,
		generation,
		postCommitTimeout,
		pollerTransitionOps{
			read:     os.ReadFile,
			alive:    pollerProcessAlive,
			identity: lookupPollerIdentity,
			stop: func(data []byte) error {
				return os.WriteFile(pollerStopFile(townRoot, session), data, 0600)
			},
			wait: waitPollerExitContext,
			quarantine: func(path string, data []byte) error {
				return quarantinePollerRecord(path, data, func(destination string) error { return os.Rename(path, destination) })
			},
			remove: os.Remove,
		},
		prepare,
	)
}

// ReplaceBeforeStoppingPollerGenerationOutcomeContext gives committed session
// work and exact-poller reconciliation independent budgets. The poller stops
// only after the commit reports terminal old-session reconciliation.
func ReplaceBeforeStoppingPollerGenerationOutcomeContext(
	ctx context.Context,
	townRoot, session string,
	generation PollerGeneration,
	commitTimeout, pollerTimeout time.Duration,
	prepare func(context.Context) (PollerReplacementOutcomeCommit, error),
) error {
	return replaceBeforeStoppingPollerGenerationOutcomeContext(
		ctx,
		townRoot,
		session,
		generation,
		commitTimeout,
		pollerTimeout,
		pollerTransitionOps{
			read:     os.ReadFile,
			alive:    pollerProcessAlive,
			identity: lookupPollerIdentity,
			stop: func(data []byte) error {
				return os.WriteFile(pollerStopFile(townRoot, session), data, 0600)
			},
			wait: waitPollerExitContext,
			quarantine: func(path string, data []byte) error {
				return quarantinePollerRecord(path, data, func(destination string) error { return os.Rename(path, destination) })
			},
			remove: os.Remove,
		},
		prepare,
	)
}

func replaceBeforeStoppingPollerGenerationContext(
	ctx context.Context,
	townRoot, session string,
	generation PollerGeneration,
	postCommitTimeout time.Duration,
	ops pollerTransitionOps,
	prepare func(context.Context) (PollerReplacementCommit, error),
) error {
	return replaceBeforeStoppingPollerGenerationOutcomeContext(
		ctx,
		townRoot,
		session,
		generation,
		postCommitTimeout,
		postCommitTimeout,
		ops,
		func(ctx context.Context) (PollerReplacementOutcomeCommit, error) {
			commit, err := prepare(ctx)
			if err != nil || commit == nil {
				return nil, err
			}
			return func(commitCtx context.Context) (PollerReplacementOutcome, error) {
				committed, commitErr := commit(commitCtx)
				return PollerReplacementOutcome{Committed: committed, Terminal: committed}, commitErr
			}, nil
		},
	)
}

func replaceBeforeStoppingPollerGenerationOutcomeContext(
	ctx context.Context,
	townRoot, session string,
	generation PollerGeneration,
	commitTimeout, pollerTimeout time.Duration,
	ops pollerTransitionOps,
	prepare func(context.Context) (PollerReplacementOutcomeCommit, error),
) error {
	lock, err := lockPollerContext(ctx, townRoot, session)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	if err := validatePollerGenerationLocked(townRoot, session, generation, ops); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	commit, err := prepare(ctx)
	if err != nil {
		return err
	}
	if commit == nil {
		return errors.New("replacement preparation returned no commit operation")
	}
	// Commit and exact-poller reconciliation each get a fresh post-commit
	// budget. Caller cancellation cannot strand a killed session under a stale
	// poller, and a slow committed cleanup cannot consume the stop budget.
	if commitTimeout <= 0 {
		commitTimeout = pollerReconcileTimeout
	}
	if pollerTimeout <= 0 {
		pollerTimeout = pollerReconcileTimeout
	}
	commitCtx, cancelCommit := context.WithTimeout(context.Background(), commitTimeout)
	outcome, replacementErr := commit(commitCtx)
	cancelCommit()
	if !outcome.Committed {
		if replacementErr == nil {
			return errors.New("replacement commit returned without issuing a destructive signal")
		}
		return replacementErr
	}
	if !outcome.Terminal {
		return errors.Join(replacementErr, ErrPollerPreservedAfterCommittedCleanup)
	}
	reconcileCtx, cancelReconcile := context.WithTimeout(context.Background(), pollerTimeout)
	defer cancelReconcile()
	stopErr := stopPollerWithExpectedGenerationOpsLocked(
		townRoot,
		session,
		&generation,
		ops.read,
		ops.alive,
		ops.identity,
		ops.stop,
		func(pid int, record pollerRecord) error { return ops.wait(reconcileCtx, pid, record) },
		ops.quarantine,
		ops.remove,
	)
	return errors.Join(replacementErr, stopErr)
}

func validatePollerGenerationLocked(
	townRoot, session string,
	generation PollerGeneration,
	ops pollerTransitionOps,
) error {
	data, err := ops.read(pollerPidFile(townRoot, session))
	if err != nil {
		if os.IsNotExist(err) {
			if generation.exists {
				return errors.New("poller ownership advanced before replacement; preserving current generation")
			}
			return nil
		}
		return fmt.Errorf("reading poller ownership before replacement: %w", err)
	}
	if !generation.exists || !bytes.Equal(data, generation.record) {
		return errors.New("poller ownership advanced before replacement; preserving current generation")
	}
	record, err := parsePollerRecord(string(data))
	if err != nil {
		return fmt.Errorf("parsing poller ownership before replacement: %w", err)
	}
	if !ops.alive(record.PID) {
		return nil
	}
	current, err := ops.identity(record.PID)
	if err != nil {
		if !ops.alive(record.PID) {
			return nil
		}
		return fmt.Errorf("validating poller identity before replacement: %w", err)
	}
	if record.Legacy || !pollerIdentityMatches(current, record, session) {
		return errors.New("poller identity changed before replacement; preserving current generation")
	}
	return nil
}

func stopPoller(townRoot, session string, expected *PollerGeneration) error {
	return stopPollerWithExpectedGenerationOps(townRoot, session, expected, os.ReadFile, pollerProcessAlive, lookupPollerIdentity,
		func(data []byte) error { return os.WriteFile(pollerStopFile(townRoot, session), data, 0600) }, waitPollerExit,
		func(path string, data []byte) error {
			return quarantinePollerRecord(path, data, func(destination string) error { return os.Rename(path, destination) })
		}, os.Remove)
}

// StopPollerBeforeReplacement prevents session replacement until the old
// poller generation has relinquished custody.
func StopPollerBeforeReplacement(stop func() error, replace func() error) error {
	if err := stop(); err != nil {
		return err
	}
	return replace()
}

const (
	pollerExitTimeout      = 2 * time.Second
	pollerExitInterval     = 50 * time.Millisecond
	pollerReconcileTimeout = pollerExitTimeout + time.Second
)

func stopPollerWithGenerationOps(
	townRoot, sessionName string,
	readPID func(string) ([]byte, error),
	alive func(int) bool,
	identity func(int) (pollerIdentity, error),
	stop func([]byte) error,
	waitExit func(int, pollerRecord) error,
	quarantine func(string, []byte) error,
	remove func(string) error,
) error {
	return stopPollerWithExpectedGenerationOps(townRoot, sessionName, nil, readPID, alive, identity, stop, waitExit, quarantine, remove)
}

func stopPollerWithExpectedGenerationOps(
	townRoot, sessionName string,
	expected *PollerGeneration,
	readPID func(string) ([]byte, error),
	alive func(int) bool,
	identity func(int) (pollerIdentity, error),
	stop func([]byte) error,
	waitExit func(int, pollerRecord) error,
	quarantine func(string, []byte) error,
	remove func(string) error,
) error {
	lock, err := lockPoller(townRoot, sessionName)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	return stopPollerWithExpectedGenerationOpsLocked(
		townRoot, sessionName, expected,
		readPID, alive, identity, stop, waitExit, quarantine, remove,
	)
}

func stopPollerWithExpectedGenerationOpsLocked(
	townRoot, sessionName string,
	expected *PollerGeneration,
	readPID func(string) ([]byte, error),
	alive func(int) bool,
	identity func(int) (pollerIdentity, error),
	stop func([]byte) error,
	waitExit func(int, pollerRecord) error,
	quarantine func(string, []byte) error,
	remove func(string) error,
) error {
	pidPath := pollerPidFile(townRoot, sessionName)
	data, err := readPID(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			if expected != nil && expected.exists {
				return errors.New("poller ownership advanced before stop; preserving current generation")
			}
			return nil
		}
		return fmt.Errorf("reading poller ownership: %w", err)
	}
	if expected != nil && (!expected.exists || !bytes.Equal(data, expected.record)) {
		return errors.New("poller ownership advanced before stop; preserving current generation")
	}
	record, err := parsePollerRecord(string(data))
	if err != nil {
		return fmt.Errorf("parsing poller ownership: %w", err)
	}
	if !alive(record.PID) {
		if err := remove(pidPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing dead poller ownership: %w", err)
		}
		return nil
	}
	current, err := identity(record.PID)
	if err != nil {
		if !alive(record.PID) {
			if removeErr := remove(pidPath); removeErr != nil && !os.IsNotExist(removeErr) {
				return fmt.Errorf("removing dead poller ownership: %w", removeErr)
			}
			return nil
		}
		return fmt.Errorf("validating poller identity: %w", err)
	}
	if record.Legacy {
		if !pollerCommandMatches(current.Command, sessionName) {
			return fmt.Errorf("legacy poller identity mismatch for session %s; preserving ownership", sessionName)
		}
		return fmt.Errorf("legacy poller ownership requires separately verified migration; preserving PID %d", record.PID)
	}
	if !pollerIdentityMatches(current, record, sessionName) {
		if quarantineErr := quarantine(pidPath, data); quarantineErr != nil {
			return errors.Join(errors.New("poller identity mismatch"), quarantineErr)
		}
		return errors.New("poller identity mismatch; ownership quarantined")
	}
	if err := stop(data); err != nil {
		return fmt.Errorf("publishing cooperative stop request for poller (pid %d): %w", record.PID, err)
	}
	if err := waitExit(record.PID, record); err != nil {
		return fmt.Errorf("waiting for poller (pid %d) to exit: %w", record.PID, err)
	}
	latest, err := readPID(pidPath)
	if err != nil {
		return fmt.Errorf("rereading poller ownership before removal: %w", err)
	}
	if !bytes.Equal(latest, data) {
		return errors.New("poller ownership changed before removal; preserving replacement record")
	}
	stopPath := pollerStopFile(townRoot, sessionName)
	if stopData, readErr := os.ReadFile(stopPath); readErr == nil && bytes.Equal(stopData, data) {
		if removeErr := remove(stopPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("removing poller stop request: %w", removeErr)
		}
	}
	if err := remove(pidPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing poller ownership: %w", err)
	}
	return nil
}

func quarantinePollerRecord(pidPath string, data []byte, rename func(string) error) error {
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	return rename(pidPath + ".stale-" + digest[:16])
}

func lookupPollerIdentity(pid int) (pollerIdentity, error) {
	identity := pollerIdentityForProcess(pid)
	if identity.StartTime == "" || identity.Command == "" {
		return pollerIdentity{}, errors.New("process identity unavailable")
	}
	return identity, nil
}

func waitPollerExit(pid int, record pollerRecord) error {
	return waitPollerExitWithOpsContext(context.Background(), pid, record, pollerProcessAlive, lookupPollerIdentity)
}

func waitPollerExitContext(ctx context.Context, pid int, record pollerRecord) error {
	return waitPollerExitWithOpsContext(ctx, pid, record, pollerProcessAlive, lookupPollerIdentity)
}

func waitPollerExitWithOps(
	pid int,
	record pollerRecord,
	alive func(int) bool,
	identity func(int) (pollerIdentity, error),
) error {
	return waitPollerExitWithOpsContext(context.Background(), pid, record, alive, identity)
}

func waitPollerExitWithOpsContext(
	ctx context.Context,
	pid int,
	record pollerRecord,
	alive func(int) bool,
	identity func(int) (pollerIdentity, error),
) error {
	timer := time.NewTimer(pollerExitTimeout)
	defer timer.Stop()
	ticker := time.NewTicker(pollerExitInterval)
	defer ticker.Stop()
	for {
		if !alive(pid) {
			return nil
		}
		if current, err := identity(pid); err == nil && !pollerIdentityMatches(current, record, record.Session) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("exit confirmation timeout")
		case <-ticker.C:
		}
	}
}

// pollerAlive checks if a poller is running for the given session.
// Returns the PID and whether the process is alive.
func pollerStatus(townRoot, session string) (int, bool, error) {
	return pollerStatusWithOps(townRoot, session, os.ReadFile, pollerProcessAlive, lookupPollerIdentity,
		func(path string, data []byte) error {
			return quarantinePollerRecord(path, data, func(destination string) error { return os.Rename(path, destination) })
		}, os.Remove)
}

func pollerStatusWithOps(
	townRoot, session string,
	readPID func(string) ([]byte, error),
	alive func(int) bool,
	identity func(int) (pollerIdentity, error),
	quarantine func(string, []byte) error,
	remove func(string) error,
) (int, bool, error) {
	pidPath := pollerPidFile(townRoot, session)

	data, err := readPID(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("reading poller ownership: %w", err)
	}

	record, err := parsePollerRecord(string(data))
	if err != nil {
		return 0, false, fmt.Errorf("parsing poller ownership: %w", err)
	}

	if !alive(record.PID) {
		_ = remove(pidPath)
		return 0, false, nil
	}
	current, err := identity(record.PID)
	if err != nil {
		if !alive(record.PID) {
			_ = remove(pidPath)
			return 0, false, nil
		}
		return record.PID, true, fmt.Errorf("validating poller identity: %w", err)
	}
	if record.Legacy {
		if !pollerCommandMatches(current.Command, session) {
			return record.PID, true, fmt.Errorf("legacy poller identity mismatch for session %s; preserving ownership", session)
		}
		return record.PID, true, fmt.Errorf("legacy poller ownership requires separately verified migration; preserving PID %d", record.PID)
	} else if !pollerIdentityMatches(current, record, session) {
		if err := quarantine(pidPath, data); err != nil {
			return 0, false, fmt.Errorf("quarantining poller ownership: %w", err)
		}
		return 0, false, nil
	}
	return record.PID, true, nil
}

func pollerAlive(townRoot, session string) (int, bool) {
	pid, alive, _ := pollerStatus(townRoot, session)
	return pid, alive
}

// Watcher provides a filesystem-event-driven interface to the nudge queue.
// This is an ACP-safe alternative to polling and is preferred for long-running
// watchers like ACP Propeller.
type Watcher struct {
	townRoot string
	session  string
	dir      string
	closed   chan struct{}
	wg       sync.WaitGroup
	events   chan struct{}
}

// NewWatcher creates a new watcher for the given town root and session.
// The watcher observes nudge queue writes and signals via the Events() channel.
func NewWatcher(townRoot, session string) (*Watcher, error) {
	dir := queueDir(townRoot, session)
	// Ensure the directory exists so watch can start immediately.
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("creating nudge queue dir: %w", err)
	}

	w := &Watcher{
		townRoot: townRoot,
		session:  session,
		dir:      dir,
		closed:   make(chan struct{}),
		events:   make(chan struct{}, 1), // buffer one signal for coalescing
	}

	w.wg.Add(1)
	go w.watch()
	return w, nil
}

// Events returns a channel that receives a struct{} when the queue may have
// changed. Multiple changes within a short window are coalesced.
func (w *Watcher) Events() <-chan struct{} {
	return w.events
}

// Close stops the watcher and releases resources.
func (w *Watcher) Close() error {
	select {
	case <-w.closed:
		return fmt.Errorf("watcher already closed")
	default:
	}
	close(w.closed)
	w.wg.Wait()
	return nil
}

func (w *Watcher) watch() {
	defer w.wg.Done()

	// Use fsnotify directly.
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		// Log but don't block; fallback behavior is explicit in callers.
		fmt.Fprintf(os.Stderr, "nudge watcher init failed for %s: %v\n", w.dir, err)
		return
	}
	defer func() { _ = watcher.Close() }()

	// Watch the directory.
	if err := watcher.Add(w.dir); err != nil {
		fmt.Fprintf(os.Stderr, "nudge watcher failed to add dir %s: %v\n", w.dir, err)
		return
	}

	// Coalescing window.
	coalesceTimer := time.NewTicker(100 * time.Millisecond)
	defer coalesceTimer.Stop()

	pending := false
	for {
		select {
		case <-w.closed:
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			// Only care about file creation/modification in the queue dir
			if event.Op&(fsnotify.Create|fsnotify.Write) != 0 {
				// Filter: only .json files in the queue directory
				if strings.HasSuffix(event.Name, ".json") && filepath.Dir(event.Name) == w.dir {
					pending = true
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			fmt.Fprintf(os.Stderr, "nudge watcher error: %v\n", err)
		case <-coalesceTimer.C:
			if pending {
				pending = false
				select {
				case w.events <- struct{}{}:
				default:
				}
			}
		}
	}
}

// WatcherForSession returns a Watcher for a specific session or an error if
// creation fails (e.g., filesystem issues). Callers should handle cleanup.
func WatcherForSession(townRoot, session string) (*Watcher, error) {
	return NewWatcher(townRoot, session)
}
