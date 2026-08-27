package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// Tests for the two-step session creation (new-session + respawn-pane) and
// checkSessionAfterCreate health check introduced to eliminate blank windows.

// TestNewSessionWithCommand_BadBinary verifies that NewSessionWithCommand returns
// an error when the command binary doesn't exist, instead of leaving a dead session.
func TestNewSessionWithCommand_BadBinary(t *testing.T) {
	tm := newTestTmux(t)
	session := "gt-test-badbinary-" + t.Name()
	_ = tm.KillSession(session)
	defer func() { _ = tm.KillSession(session) }()

	err := tm.NewSessionWithCommand(session, "", "/nonexistent/binary --flag")
	if err == nil {
		// checkSessionAfterCreate should have caught this
		t.Error("NewSessionWithCommand should return error for missing binary")
	}
}

// TestNewSessionWithCommand_BadWorkDir verifies workDir validation rejects
// non-existent directories before creating the session.
func TestNewSessionWithCommand_BadWorkDir(t *testing.T) {
	tm := newTestTmux(t)
	session := "gt-test-badworkdir-" + t.Name()
	_ = tm.KillSession(session)
	defer func() { _ = tm.KillSession(session) }()

	err := tm.NewSessionWithCommand(session, "/tmp/gastown-nonexistent-dir-99999", "echo hello")
	if err == nil {
		t.Error("NewSessionWithCommand should return error for non-existent workDir")
	}
}

func TestNewSessionWithCommandAndEnv_InvalidInputPreservesOtherSocketSession(t *testing.T) {
	session := "gt-test-owned-" + strings.ToLower(t.Name())
	other := NewTmuxWithSocket("default")
	_ = other.KillSession(session)
	if err := other.NewSession(session, ""); err != nil {
		t.Fatalf("create other-socket session: %v", err)
	}
	t.Cleanup(func() { _ = other.KillSession(session) })

	owner := newTestTmux(t)
	_ = owner.KillSession(session)
	err := owner.NewSessionWithCommandAndEnv(session, t.TempDir()+"/missing", "sleep 10", nil)
	if err == nil {
		t.Fatal("expected invalid work directory error")
	}
	if running, err := other.HasSession(session); err != nil || !running {
		t.Fatalf("other-socket session removed after validation failure: running=%v err=%v", running, err)
	}
}

func TestNewSessionWithCommandAndEnv_ValidInputPreservesOtherSocketSession(t *testing.T) {
	session := "gt-test-owned-valid"
	owner := newTestTmux(t)
	warmup := "gt-test-owner-warmup"
	_ = owner.KillSession(warmup)
	if err := owner.NewSession(warmup, ""); err != nil {
		t.Fatalf("warm owner socket: %v", err)
	}
	t.Cleanup(func() { _ = owner.KillSession(warmup) })

	other := NewTmuxWithSocket("default")
	_ = other.KillSession(session)
	if err := other.NewSession(session, ""); err != nil {
		t.Fatalf("create other-socket session: %v", err)
	}
	t.Cleanup(func() { _ = other.KillSession(session) })

	_ = owner.KillSession(session)
	if err := owner.NewSessionWithCommandAndEnv(session, t.TempDir(), "sleep 10", nil); err != nil {
		t.Fatalf("create owner session: %v", err)
	}
	t.Cleanup(func() { _ = owner.KillSession(session) })

	if running, err := other.HasSession(session); err != nil || !running {
		t.Fatalf("other-socket session removed by unrelated creation: running=%v err=%v", running, err)
	}
}

func TestNewSessionWithCommandAndEnvGenerationReturnsExactReceipt(t *testing.T) {
	tm := newTestTmux(t)
	session := "gt-test-generation-receipt"
	_ = tm.KillSession(session)
	t.Cleanup(func() { _ = tm.KillSession(session) })

	generation, err := tm.NewSessionWithCommandAndEnvGeneration(session, t.TempDir(), "sleep 10", nil)
	if err != nil {
		t.Fatalf("create session with generation receipt: %v", err)
	}
	observed, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatalf("capture created session generation: %v", err)
	}
	if !generation.Equal(observed) {
		t.Fatalf("creation receipt = %+v, observed = %+v", generation, observed)
	}
}

func TestStartTransientSessionWithCommandAndEnvPersistsCreatedPane(t *testing.T) {
	t.Setenv(EnvSessionPane, "0")
	tm := newTestTmux(t)
	target := fmt.Sprintf("gt-transient-pane-target-%d", time.Now().UnixNano())
	finalizer := fmt.Sprintf("gt-transient-pane-finalizer-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = tm.KillSession(target)
		_ = tm.KillSession(finalizer)
	})
	if err := tm.NewSessionWithCommand(target, t.TempDir(), "sleep 30"); err != nil {
		t.Fatal(err)
	}
	generation, err := tm.StartTransientSessionWithCommandAndEnv(
		finalizer,
		t.TempDir(),
		"sleep 30",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := tm.CaptureSessionGeneration(generation.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !generation.Equal(observed) {
		t.Fatalf("transient generation = %+v, observed = %+v", generation, observed)
	}
}

func TestStartTransientSessionWithCommandAndEnvSurvivesLastOwnedSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("transient host-session proof uses POSIX shell commands")
	}
	tm := newTestTmux(t)
	target := fmt.Sprintf("gt-transient-target-%d", time.Now().UnixNano())
	finalizer := fmt.Sprintf("gt-transient-finalizer-%d", time.Now().UnixNano())
	proof := filepath.Join(t.TempDir(), "finalized")
	if err := tm.NewSessionWithCommandAndEnv(target, t.TempDir(), "sleep 30", nil); err != nil {
		t.Fatal(err)
	}
	command := strings.Join([]string{
		"tmux -L " + config.ShellQuote(tm.socketName) + " kill-session -t " + config.ShellQuote(target),
		"printf '%s' \"$GT_TRANSIENT_PROOF\" > " + config.ShellQuote(proof),
	}, "; ")
	if _, err := tm.StartTransientSessionWithCommandAndEnv(
		finalizer,
		t.TempDir(),
		command,
		map[string]string{"GT_TRANSIENT_PROOF": "complete"},
	); err != nil {
		t.Fatalf("StartTransientSessionWithCommandAndEnv: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		payload, readErr := os.ReadFile(proof)
		targetRunning, targetErr := tm.HasSession(target)
		if readErr == nil && targetErr == nil && !targetRunning && string(payload) == "complete" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	payload, _ := os.ReadFile(proof)
	targetRunning, _ := tm.HasSession(target)
	t.Fatalf("transient host session did not finish after target removal: target_running=%v proof=%q", targetRunning, payload)
}

func TestCleanupFailedSessionGenerationPreservesSameNameReplacement(t *testing.T) {
	tm := newTestTmux(t)
	session := "gt-test-failed-start-replacement"
	_ = tm.KillSession(session)
	t.Cleanup(func() { _ = tm.KillSession(session) })

	original, err := tm.NewSessionWithCommandAndEnvGeneration(session, t.TempDir(), "sleep 10", nil)
	if err != nil {
		t.Fatalf("create original generation: %v", err)
	}
	if err := tm.KillSessionGeneration(original); err != nil {
		t.Fatalf("retire original generation: %v", err)
	}
	replacement, err := tm.NewSessionWithCommandAndEnvGeneration(session, t.TempDir(), "sleep 10", nil)
	if err != nil {
		t.Fatalf("create replacement generation: %v", err)
	}
	if replacement.Equal(original) {
		t.Fatal("replacement unexpectedly reused original exact generation")
	}

	if err := tm.CleanupFailedSessionGeneration(original); err != nil {
		t.Fatalf("cleanup terminal original generation: %v", err)
	}
	observed, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatalf("capture preserved replacement: %v", err)
	}
	if !observed.Equal(replacement) {
		t.Fatalf("cleanup changed replacement: got %+v, want %+v", observed, replacement)
	}
}

func TestProcessCleanupGenerationChangeRequiresTmuxReplacement(t *testing.T) {
	tm := newTestTmux(t)
	session := "gt-test-process-change-classification"
	_ = tm.KillSession(session)
	t.Cleanup(func() { _ = tm.KillSession(session) })

	original, err := tm.NewSessionWithCommandAndEnvGeneration(session, t.TempDir(), "sleep 10", nil)
	if err != nil {
		t.Fatalf("create original generation: %v", err)
	}
	for _, cleanupErr := range []error{ErrSessionGenerationChanged, ErrSessionNotFound, ErrNoServer} {
		terminal, classifiedErr := tm.classifyProcessCleanupResult(
			context.Background(),
			original,
			cleanupErr,
		)
		if terminal || !errors.Is(classifiedErr, cleanupErr) {
			t.Fatalf("live exact generation with %v classified terminal=%v err=%v", cleanupErr, terminal, classifiedErr)
		}
	}

	if err := tm.KillSessionGeneration(original); err != nil {
		t.Fatalf("retire original generation: %v", err)
	}
	replacement, err := tm.NewSessionWithCommandAndEnvGeneration(session, t.TempDir(), "sleep 10", nil)
	if err != nil {
		t.Fatalf("create replacement generation: %v", err)
	}
	if replacement.Equal(original) {
		t.Fatal("replacement unexpectedly reused original generation")
	}
	terminal, classifiedErr := tm.classifyProcessCleanupResult(
		context.Background(),
		original,
		ErrSessionGenerationChanged,
	)
	if !terminal || !errors.Is(classifiedErr, ErrSessionGenerationChanged) {
		t.Fatalf("replacement classified terminal=%v err=%v", terminal, classifiedErr)
	}
}

func TestNewSessionWithCommandAndEnvContext_EntersWorkDirWhenServerCWDIsUnlinked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unlinking a process working directory is a Unix-specific regression")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is required")
	}

	socket := fmt.Sprintf("gt-test-unlinked-cwd-%d", time.Now().UnixNano())
	tm := NewTmuxWithSocket(socket)
	serverCWD := filepath.Join(t.TempDir(), "server-cwd")
	if err := os.Mkdir(serverCWD, 0o700); err != nil {
		t.Fatalf("create server cwd: %v", err)
	}
	sentinel := "gt-test-unlinked-cwd-sentinel"
	start := exec.Command("tmux", "-u", "-L", socket, "new-session", "-d", "-s", sentinel, "sleep 30")
	start.Dir = serverCWD
	if output, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start isolated tmux server: %v: %s", err, output)
	}
	t.Cleanup(func() { _ = tm.KillServer() })

	if err := os.Remove(serverCWD); err != nil {
		t.Fatalf("unlink server cwd: %v", err)
	}

	workDir := filepath.Join(t.TempDir(), "work dir 'quoted'")
	if err := os.Mkdir(workDir, 0o700); err != nil {
		t.Fatalf("create target work directory: %v", err)
	}
	session := "gt-test-unlinked-cwd-target"
	t.Cleanup(func() { _ = tm.KillSessionWithProcesses(session) })

	if err := tm.NewSessionWithCommandAndEnvContext(context.Background(), session, workDir, "pwd -P; exec sleep 30", nil); err != nil {
		t.Fatalf("create session from unlinked server cwd: %v", err)
	}
	got, err := tm.CapturePaneAll(session)
	if err != nil {
		t.Fatalf("capture pane cwd: %v", err)
	}
	want, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		t.Fatalf("resolve target work directory: %v", err)
	}
	if !strings.Contains(strings.ReplaceAll(got, "\n", ""), want) {
		t.Fatalf("pane output does not contain cwd %q: %q", want, got)
	}
}

func TestNewSessionWithCommandAndEnvContext_CancellationCleansCreatedSession(t *testing.T) {
	tm := newTestTmux(t)
	creator, ok := any(tm).(interface {
		NewSessionWithCommandAndEnvContext(context.Context, string, string, string, map[string]string) error
	})
	if !ok {
		t.Fatal("Tmux does not provide cancellation-aware session creation")
	}

	session := "gt-test-create-cancel-" + strings.ToLower(t.Name())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()
	err := creator.NewSessionWithCommandAndEnvContext(ctx, session, t.TempDir(), "sleep 10", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("creation error = %v, want context deadline exceeded", err)
	}
	// Exact process-aware cleanup owns an independent bounded budget after the
	// caller expires. This may include the normal graceful process-exit period,
	// so assert the cleanup budget plus scheduler allowance rather than the old
	// name-only sub-second teardown contract.
	maxElapsed := failedSessionCreationCleanupTimeout + time.Second
	if elapsed := time.Since(started); elapsed > maxElapsed {
		t.Fatalf("cancellation took %v, want <= %v", elapsed, maxElapsed)
	}
	if running, err := tm.HasSession(session); err != nil || running {
		t.Fatalf("cancelled creation left session behind: running=%v err=%v", running, err)
	}
}

func TestNewSessionWithCommandAndEnvContext_CancellationCleansDetachedChild(t *testing.T) {
	tm := newTestTmux(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required to launch a detached child")
	}
	mkfifo, err := exec.LookPath("mkfifo")
	if err != nil {
		t.Skip("mkfifo is required for the detached-child readiness handshake")
	}

	unrelated := "gt-test-create-cancel-unrelated"
	_ = tm.KillSession(unrelated)
	if err := tm.NewSessionWithCommand(unrelated, t.TempDir(), "sleep 30"); err != nil {
		t.Fatalf("create unrelated session: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSessionWithProcesses(unrelated) })

	session := "gt-test-create-cancel-detached"
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	barrier := filepath.Join(t.TempDir(), "final-check.fifo")
	barrierOnce := filepath.Join(t.TempDir(), "final-check-once")
	pidFIFO := filepath.Join(t.TempDir(), "child-pid.fifo")
	for _, fifo := range []string{barrier, pidFIFO} {
		if output, err := exec.Command(mkfifo, fifo).CombinedOutput(); err != nil {
			t.Fatalf("create readiness FIFO %s: %v: %s", fifo, err, output)
		}
	}
	type fifoResult struct {
		payload []byte
		err     error
	}
	readFIFO := func(path string) <-chan fifoResult {
		result := make(chan fifoResult, 1)
		go func() {
			payload, err := os.ReadFile(path)
			result <- fifoResult{payload: payload, err: err}
		}()
		return result
	}
	childReady := readFIFO(pidFIFO)
	finalBoundary := readFIFO(barrier)
	wrapperDir := t.TempDir()
	wrapper := fmt.Sprintf(`#!/bin/sh
case " $* " in
	  *" remain-on-exit off "*)
    if mkdir %q 2>/dev/null; then
      printf 'ready\n' > %q
      exec /bin/sleep 30
    fi
    ;;
esac
exec %q "$@"
`, barrierOnce, barrier, realTmux)
	if err := os.WriteFile(filepath.Join(wrapperDir, "tmux"), []byte(wrapper), 0o755); err != nil {
		t.Fatalf("write tmux barrier wrapper: %v", err)
	}
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	script := fmt.Sprintf(`import subprocess; p = subprocess.Popen(["sleep", "30"], start_new_session=True); ready = open(%q, "w"); ready.write(str(p.pid)); ready.close(); p.wait()`, pidFIFO)
	command := fmt.Sprintf("%q -c %q", python, script)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Cleanup(func() { _ = tm.KillSessionWithProcesses(session) })

	errCh := make(chan error, 1)
	go func() {
		errCh <- tm.NewSessionWithCommandAndEnvContext(ctx, session, t.TempDir(), command, nil)
	}()

	var childPID string
	select {
	case ready := <-childReady:
		childPID = strings.TrimSpace(string(ready.payload))
		if ready.err != nil || childPID == "" {
			t.Fatalf("detached-child readiness handshake = %q, err %v", ready.payload, ready.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for detached-child readiness handshake")
	}
	select {
	case ready := <-finalBoundary:
		if ready.err != nil || strings.TrimSpace(string(ready.payload)) != "ready" {
			t.Fatalf("final creation boundary handshake = %q, err %v", ready.payload, ready.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for final creation boundary handshake")
	}

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("creation error = %v, want context canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for bounded failed-creation cleanup")
	}

	deadline := time.Now().Add(2 * time.Second)
	for exec.Command("kill", "-0", childPID).Run() == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := exec.Command("kill", "-0", childPID).Run(); err == nil {
		t.Fatalf("attempt-owned detached child process %s survived cancelled creation", childPID)
	}
	if running, err := tm.HasSession(session); err != nil || running {
		t.Fatalf("attempt-owned tmux session survived cancelled creation: running=%v err=%v", running, err)
	}
	if running, err := tm.HasSession(unrelated); err != nil || !running {
		t.Fatalf("unrelated session removed by cancelled creation: running=%v err=%v", running, err)
	}
}

func TestWaitForRuntimeReadyContext_CancellationStopsDelay(t *testing.T) {
	tm := newTestTmux(t)
	waiter, ok := any(tm).(interface {
		WaitForRuntimeReadyContext(context.Context, string, *config.RuntimeConfig, time.Duration) error
	})
	if !ok {
		t.Fatal("Tmux does not provide cancellation-aware runtime readiness")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rc := &config.RuntimeConfig{Tmux: &config.RuntimeTmuxConfig{ReadyDelayMs: 30_000}}
	started := time.Now()
	err := waiter.WaitForRuntimeReadyContext(ctx, "unused", rc, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("WaitForRuntimeReadyContext error = %v, want cancellation", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("cancellation took %v, want <= 250ms", elapsed)
	}
}

// TestNewSessionWithCommand_ExecEnvBadBinary verifies the exact gastown polecat
// startup pattern (exec env VAR=val binary) returns an error for missing binaries.
func TestNewSessionWithCommand_ExecEnvBadBinary(t *testing.T) {
	tm := newTestTmux(t)
	session := "gt-test-execenv-bad-" + t.Name()
	_ = tm.KillSession(session)
	defer func() { _ = tm.KillSession(session) }()

	cmd := `exec env GT_TEST=1 GT_ROLE=test /nonexistent/claude-code --settings /tmp`
	err := tm.NewSessionWithCommand(session, "", cmd)
	if err == nil {
		t.Error("NewSessionWithCommand should return error for exec env with missing binary")
	}
}

// TestNewSessionWithCommand_Success verifies a valid command runs and produces output.
func TestNewSessionWithCommand_Success(t *testing.T) {
	tm := newTestTmux(t)
	session := "gt-test-success-" + t.Name()
	_ = tm.KillSession(session)
	defer func() { _ = tm.KillSession(session) }()

	err := tm.NewSessionWithCommand(session, "", `sh -c 'echo "SESSION_OK"; sleep 10'`)
	if err != nil {
		t.Fatalf("NewSessionWithCommand failed: %v", err)
	}

	time.Sleep(500 * time.Millisecond)
	output, _ := tm.CapturePane(session, 50)
	if !strings.Contains(output, "SESSION_OK") {
		t.Errorf("expected output to contain SESSION_OK, got: %q", strings.TrimSpace(output))
	}
}

// TestNewSessionWithCommand_ExecEnvSuccess verifies the exec env pattern works
// with a real binary.
func TestNewSessionWithCommand_ExecEnvSuccess(t *testing.T) {
	tm := newTestTmux(t)
	session := "gt-test-execenv-ok-" + t.Name()
	_ = tm.KillSession(session)
	defer func() { _ = tm.KillSession(session) }()

	cmd := `exec env GT_RIG=testrig GT_POLECAT=testcat sleep 5`
	err := tm.NewSessionWithCommand(session, t.TempDir(), cmd)
	if err != nil {
		t.Fatalf("NewSessionWithCommand failed: %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	paneCmd, _ := tm.GetPaneCommand(session)
	if paneCmd != "sleep" {
		t.Errorf("expected pane command 'sleep' (exec replaced shell), got %q", paneCmd)
	}
}

// TestNewSessionWithCommand_Duplicate verifies duplicate session creation is rejected.
func TestNewSessionWithCommand_Duplicate(t *testing.T) {
	tm := newTestTmux(t)
	session := "gt-test-dup-" + t.Name()
	_ = tm.KillSession(session)
	defer func() { _ = tm.KillSession(session) }()

	if err := tm.NewSessionWithCommand(session, "", "sleep 10"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	err := tm.NewSessionWithCommand(session, "", "sleep 10")
	if err == nil {
		t.Error("duplicate session creation should fail")
	}
}

// TestNewSessionWithCommand_Concurrent verifies multiple sessions can be created
// concurrently without errors.
func TestNewSessionWithCommand_Concurrent(t *testing.T) {
	tm := newTestTmux(t)
	n := 5
	base := "gt-test-concurrent-"

	for i := 0; i < n; i++ {
		_ = tm.KillSession(base + string(rune('a'+i)))
	}
	defer func() {
		for i := 0; i < n; i++ {
			_ = tm.KillSession(base + string(rune('a'+i)))
		}
	}()

	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			errs <- tm.NewSessionWithCommand(base+string(rune('a'+idx)), "", "sleep 5")
		}(i)
	}

	var failures int
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			failures++
			t.Logf("concurrent create %d: %v", i, err)
		}
	}
	if failures > 0 {
		t.Errorf("%d/%d concurrent session creations failed", failures, n)
	}
}

// TestWaitForCommand_Timeout verifies WaitForCommand returns an error when the
// pane command remains a shell (agent never started).
func TestWaitForCommand_Timeout(t *testing.T) {
	tm := newTestTmux(t)
	session := "gt-test-waitcmd-" + t.Name()
	_ = tm.KillSession(session)
	defer func() { _ = tm.KillSession(session) }()

	if err := tm.NewSessionWithCommand(session, "", "bash"); err != nil {
		t.Fatalf("session creation: %v", err)
	}

	err := tm.WaitForCommand(session, []string{"bash", "zsh", "sh"}, 500*time.Millisecond)
	if err == nil {
		t.Error("WaitForCommand should timeout when shell is still running")
	}
}

// TestSanitizeNudgeMessage verifies control character stripping.
func TestSanitizeNudgeMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"passthrough", "hello world", "hello world"},
		{"strips ESC", "hello\x1bworld", "helloworld"},
		{"strips CR", "hello\rworld", "helloworld"},
		{"tab to space", "hello\tworld", "hello world"},
		{"preserves newline", "hello\nworld", "hello\nworld"},
		{"preserves unicode", "hello 世界", "hello 世界"},
		{"strips BS", "hello\x08world", "helloworld"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeNudgeMessage(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeNudgeMessage(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestContainsRewindIndicators verifies detection of Claude Code's Rewind menu.
func TestContainsRewindIndicators(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"empty", "", false},
		{"normal prompt", "❯ hello world", false},
		{"busy indicator", "⏵⏵ Running tool... esc to interrupt", false},
		{"rewind with enter and esc", "Rewind\nPress Enter to select, Esc to go back", true},
		{"rewind case insensitive", "rewind history\nenter to continue\nesc to exit", true},
		{"enter to continue + esc to exit", "Some UI\nEnter to continue\nEsc to exit", true},
		{"enter to accept + esc to cancel", "Enter to accept changes\nEsc to cancel", true},
		{"enter to select + esc to cancel", "Choose a checkpoint:\nEnter to select\nEsc to cancel", true},
		{"only rewind no actions", "Rewind history shown here", false},
		{"only enter no esc", "Enter to continue", false},
		{"only esc no enter", "Esc to exit", false},
		{"conversation mentioning rewind", "User said: please rewind the video\n❯ ", false},
		{"partial match no pair", "Enter to continue\nSome other text", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := containsRewindIndicators(tt.content)
			if got != tt.want {
				t.Errorf("containsRewindIndicators(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

// TestSendMessageToTarget_Chunking verifies that long messages are chunked.
func TestSendMessageToTarget_Chunking(t *testing.T) {
	tm := newTestTmux(t)
	session := "gt-test-chunk-" + t.Name()
	_ = tm.KillSession(session)
	defer func() { _ = tm.KillSession(session) }()

	// Use cat to receive input
	if err := tm.NewSessionWithCommand(session, "", "cat"); err != nil {
		t.Fatalf("session creation: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	// Send a message longer than typical chunk size
	msg := strings.Repeat("A", 600)
	err := tm.sendMessageToTarget(session, msg)
	if err != nil {
		t.Fatalf("sendMessageToTarget: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	output, _ := tm.CapturePane(session, 50)
	// Count A's in output (may be split across lines)
	count := strings.Count(output, "A")
	if count < 500 {
		t.Errorf("expected ~600 A's in output, got %d (message may have been truncated)", count)
	}
}
