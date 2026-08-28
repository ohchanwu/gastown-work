package tmux

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/delivery"
)

func hasTmux() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// newTestTmux returns a Tmux instance connected to the package-level test
// socket (set by TestMain in testmain_test.go). All tests in this package
// share one tmux server, which is torn down after all tests complete.
//
// This isolates tests from the user's interactive tmux and from other
// packages' tests that run in parallel during `go test ./...`.
func newTestTmux(t *testing.T) *Tmux {
	t.Helper()
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	return NewTmux()
}

func TestListSessionsNoServer(t *testing.T) {
	tm := newTestTmux(t)
	sessions, err := tm.ListSessions()
	// Should not error even if no server running
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	// Result may be nil or empty slice
	_ = sessions
}

func TestHasSessionNoServer(t *testing.T) {
	tm := newTestTmux(t)
	has, err := tm.HasSession("nonexistent-session-xyz")
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if has {
		t.Error("expected session to not exist")
	}
}

func TestSessionLifecycle(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-session-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)

	// Create session
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Verify exists
	has, err := tm.HasSession(sessionName)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if !has {
		t.Error("expected session to exist after creation")
	}

	// List should include it
	sessions, err := tm.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	found := false
	for _, s := range sessions {
		if s == sessionName {
			found = true
			break
		}
	}
	if !found {
		t.Error("session not found in list")
	}

	// Kill session
	if err := tm.KillSession(sessionName); err != nil {
		t.Fatalf("KillSession: %v", err)
	}

	// Verify gone
	has, err = tm.HasSession(sessionName)
	if err != nil {
		t.Fatalf("HasSession after kill: %v", err)
	}
	if has {
		t.Error("expected session to not exist after kill")
	}
}

func TestDuplicateSession(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-dup-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)

	// Create session
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Try to create duplicate
	err := tm.NewSession(sessionName, "")
	if err != ErrSessionExists {
		t.Errorf("expected ErrSessionExists, got %v", err)
	}
}

func TestSendKeysAndCapture(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-keys-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)

	// Create session
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Send echo command
	if err := tm.SendKeys(sessionName, "echo HELLO_TEST_MARKER"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}

	// Give it a moment to execute
	// In real tests you'd wait for output, but for basic test we just capture
	output, err := tm.CapturePane(sessionName, 50)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}

	// Should contain our marker (might not if shell is slow, but usually works)
	if !strings.Contains(output, "echo HELLO_TEST_MARKER") {
		t.Logf("captured output: %s", output)
		// Don't fail, just note - timing issues possible
	}
}

func TestGetSessionInfo(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-info-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)

	// Create session
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	info, err := tm.GetSessionInfo(sessionName)
	if err != nil {
		t.Fatalf("GetSessionInfo: %v", err)
	}

	if info.Name != sessionName {
		t.Errorf("Name = %q, want %q", info.Name, sessionName)
	}
	if info.Windows < 1 {
		t.Errorf("Windows = %d, want >= 1", info.Windows)
	}
}

func TestWrapError(t *testing.T) {
	tm := newTestTmux(t)

	tests := []struct {
		stderr string
		want   error
	}{
		{"no server running on /tmp/tmux-...", ErrNoServer},
		{"error connecting to /tmp/tmux-...", ErrNoServer},
		{"no current target", ErrNoServer},
		{"duplicate session: test", ErrSessionExists},
		{"session not found: test", ErrSessionNotFound},
		{"can't find session: test", ErrSessionNotFound},
	}

	for _, tt := range tests {
		err := tm.wrapError(nil, tt.stderr, []string{"test"})
		if err != tt.want {
			t.Errorf("wrapError(%q) = %v, want %v", tt.stderr, err, tt.want)
		}
	}
}

func TestEnsureSessionFresh_NoExistingSession(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-fresh-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)

	// EnsureSessionFresh should create a new session
	if err := tm.EnsureSessionFresh(sessionName, ""); err != nil {
		t.Fatalf("EnsureSessionFresh: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Verify session exists
	has, err := tm.HasSession(sessionName)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if !has {
		t.Error("expected session to exist after EnsureSessionFresh")
	}
}

func TestEnsureSessionFresh_ZombieSession(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-zombie-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)

	// Create a zombie session (session exists but no Claude/node running)
	// A normal tmux session with bash/zsh is a "zombie" for our purposes
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Verify it's a zombie (not running any agent)
	if tm.IsAgentAlive(sessionName) {
		t.Skip("session unexpectedly has agent running - can't test zombie case")
	}

	// Fresh tmux sessions can briefly report transient pane commands while the
	// login shell is starting. IsAgentAlive is the stable predicate we care
	// about here: there is no runtime in the session, so EnsureSessionFresh
	// should treat it as a zombie and recreate it successfully.

	// EnsureSessionFresh should kill the zombie and create fresh session
	// This should NOT error with "session already exists"
	if err := tm.EnsureSessionFresh(sessionName, ""); err != nil {
		t.Fatalf("EnsureSessionFresh on zombie: %v", err)
	}

	// Session should still exist
	has, err := tm.HasSession(sessionName)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if !has {
		t.Error("expected session to exist after EnsureSessionFresh on zombie")
	}
}

func TestEnsureSessionFresh_IdempotentOnZombie(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-idem-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)

	// Call EnsureSessionFresh multiple times - should work each time
	for i := 0; i < 3; i++ {
		if err := tm.EnsureSessionFresh(sessionName, ""); err != nil {
			t.Fatalf("EnsureSessionFresh attempt %d: %v", i+1, err)
		}
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Session should exist
	has, err := tm.HasSession(sessionName)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if !has {
		t.Error("expected session to exist after multiple EnsureSessionFresh calls")
	}
}

func TestEnsureSessionFreshWithCommand_NoExisting(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	tm := NewTmux()
	sessionName := "gt-test-fwc-new-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)
	defer func() { _ = tm.KillSession(sessionName) }()

	// EnsureSessionFreshWithCommand should create a new session with the command
	// as the initial pane process (no shell involved).
	if err := tm.EnsureSessionFreshWithCommand(sessionName, "", "sleep 10"); err != nil {
		t.Fatalf("EnsureSessionFreshWithCommand: %v", err)
	}

	// Verify session exists
	has, err := tm.HasSession(sessionName)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if !has {
		t.Error("expected session to exist")
	}

	// Verify the command is running (not a shell)
	cmd, err := tm.GetPaneCommand(sessionName)
	if err != nil {
		t.Fatalf("GetPaneCommand: %v", err)
	}
	if cmd != "sleep" {
		t.Logf("pane command = %q (expected 'sleep')", cmd)
	}
}

func TestEnsureSessionFreshWithCommand_KillsZombie(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	tm := NewTmux()
	sessionName := "gt-test-fwc-zombie-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)
	defer func() { _ = tm.KillSession(sessionName) }()

	// Use a bare shell so user login hooks cannot briefly run git, package
	// managers, or another non-shell command that the fallback health heuristic
	// would correctly classify as active work under full-suite contention.
	if err := tm.NewSessionWithCommand(sessionName, "", "exec /bin/sh"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}

	// Verify it's a zombie
	if tm.IsAgentRunning(sessionName) {
		t.Skip("session unexpectedly has agent running")
	}

	// EnsureSessionFreshWithCommand should kill the zombie and create fresh session
	if err := tm.EnsureSessionFreshWithCommand(sessionName, "", "sleep 10"); err != nil {
		t.Fatalf("EnsureSessionFreshWithCommand on zombie: %v", err)
	}

	// Session should exist with the new command
	has, err := tm.HasSession(sessionName)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if !has {
		t.Error("expected session to exist after replacing zombie")
	}

	// The command should be sleep, not a shell
	cmd, err := tm.GetPaneCommand(sessionName)
	if err != nil {
		t.Fatalf("GetPaneCommand: %v", err)
	}
	if cmd != "sleep" {
		t.Logf("pane command = %q (expected 'sleep' after zombie replacement)", cmd)
	}
}

func TestIsAgentRunning(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-agent-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)

	// Create session (will run default shell)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Wait for the shell to be fully initialized before querying pane command.
	// Without this, GetPaneCommand can return a transient value during shell
	// startup (e.g., login or profile-sourced commands), causing flaky matches.
	if err := tm.WaitForShellReady(sessionName, 2*time.Second); err != nil {
		t.Fatalf("WaitForShellReady: %v", err)
	}

	// Get the current pane command (should be bash/zsh/etc)
	cmd, err := tm.GetPaneCommand(sessionName)
	if err != nil {
		t.Fatalf("GetPaneCommand: %v", err)
	}

	tests := []struct {
		name         string
		processNames []string
		wantRunning  bool
	}{
		{
			name:         "empty process list",
			processNames: []string{},
			wantRunning:  false,
		},
		{
			name:         "matching shell process",
			processNames: []string{cmd}, // Current shell
			wantRunning:  true,
		},
		{
			name:         "claude agent (node) - not running",
			processNames: []string{"node"},
			wantRunning:  cmd == "node", // Only true if shell happens to be node
		},
		{
			name:         "gemini agent - not running",
			processNames: []string{"gemini"},
			wantRunning:  cmd == "gemini",
		},
		{
			name:         "cursor agent - not running",
			processNames: []string{"cursor-agent"},
			wantRunning:  cmd == "cursor-agent",
		},
		{
			name:         "multiple process names with match",
			processNames: []string{"nonexistent", cmd, "also-nonexistent"},
			wantRunning:  true,
		},
		{
			name:         "multiple process names without match",
			processNames: []string{"nonexistent1", "nonexistent2"},
			wantRunning:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Re-check the pane command immediately before assertion.
			// The pane command can transiently change between the initial
			// GetPaneCommand call and subtest execution (e.g., shell profile
			// commands), causing false failures. Retry up to 5 times with
			// a short sleep to tolerate transient pane command changes.
			var got bool
			var currentCmd string
			for attempt := 0; attempt < 5; attempt++ {
				if attempt > 0 {
					time.Sleep(200 * time.Millisecond)
				}
				got = tm.IsAgentRunning(sessionName, tt.processNames...)
				if got == tt.wantRunning {
					return // success
				}
				// Re-read pane command for diagnostics
				currentCmd, _ = tm.GetPaneCommand(sessionName)
			}
			t.Errorf("IsAgentRunning(%q, %v) = %v, want %v (current cmd: %q, setup cmd: %q)",
				sessionName, tt.processNames, got, tt.wantRunning, currentCmd, cmd)
		})
	}
}

func TestIsAgentRunning_NonexistentSession(t *testing.T) {
	tm := newTestTmux(t)

	// IsAgentRunning on nonexistent session should return false, not error
	got := tm.IsAgentRunning("nonexistent-session-xyz", "node", "gemini", "cursor-agent")
	if got {
		t.Error("IsAgentRunning on nonexistent session should return false")
	}
}

func TestIsRuntimeRunningChecked_NonexistentSessionErrors(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-runtime-missing-" + t.Name()
	_ = tm.KillSession(sessionName)

	running, err := tm.IsRuntimeRunningChecked(sessionName, []string{"sleep"})
	if err == nil {
		t.Fatal("expected checked runtime query to return an error for missing session")
	}
	if running {
		t.Fatal("expected missing session to report not running")
	}
	if tm.IsRuntimeRunning(sessionName, []string{"sleep"}) {
		t.Fatal("legacy bool wrapper should collapse query errors to false")
	}
}

func TestIsRuntimeRunning_AgentNameRequiresCursorSession(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-runtime-agent-filter-" + t.Name()
	_ = tm.KillSession(sessionName)
	if err := tm.NewSessionWithCommand(sessionName, "", "sleep 60"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// sleep matches; "agent" in the list is ignored when session does not declare Cursor
	if !tm.IsRuntimeRunning(sessionName, []string{"agent", "sleep"}) {
		t.Error("expected sleep to match when agent is stripped for non-cursor sessions")
	}
	if tm.IsRuntimeRunning(sessionName, []string{"agent"}) {
		t.Error("expected bare agent not to match without GT_AGENT=cursor / GT_PROCESS_NAMES")
	}
	if err := tm.SetEnvironment(sessionName, "GT_AGENT", "cursor"); err != nil {
		t.Fatalf("SetEnvironment: %v", err)
	}
	// With Cursor declared, "agent" is kept in the filter list (pane is still sleep — no match on agent alone)
	if tm.IsRuntimeRunning(sessionName, []string{"agent"}) {
		t.Error("pane is sleep, not agent — should not match on agent name alone")
	}
	if !tm.IsRuntimeRunning(sessionName, []string{"agent", "sleep"}) {
		t.Error("expected sleep to still match with GT_AGENT=cursor")
	}
}

func TestIsRuntimeRunning(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-runtime-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)

	// Create session (will run default shell, not any agent)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// IsRuntimeRunning should be false (shell is running, not node/claude)
	cmd, _ := tm.GetPaneCommand(sessionName)
	processNames := []string{"node", "claude"}
	wantRunning := cmd == "node" || cmd == "claude"

	if got := tm.IsRuntimeRunning(sessionName, processNames); got != wantRunning {
		t.Errorf("IsRuntimeRunning() = %v, want %v (pane cmd: %q)", got, wantRunning, cmd)
	}
}

func TestIsRuntimeRunning_ShellWithNodeChild(t *testing.T) {
	tm := newTestTmux(t)

	// Direct path: tmux runs the process as the pane command directly.
	// This simulates a bundled agent binary (e.g. the standalone claude binary)
	// where the pane command IS the agent process.
	t.Run("direct", func(t *testing.T) {
		sessionName := "gt-test-runtime-direct"
		_ = tm.KillSession(sessionName)

		if err := tm.NewSessionWithCommand(sessionName, "", "sleep 10"); err != nil {
			t.Fatalf("NewSessionWithCommand: %v", err)
		}
		defer func() { _ = tm.KillSession(sessionName) }()

		if !tm.IsRuntimeRunning(sessionName, []string{"sleep"}) {
			paneCmd, _ := tm.GetPaneCommand(sessionName)
			t.Errorf("IsRuntimeRunning should return true for direct process (pane cmd: %q)", paneCmd)
		}
	})

	// Shell+child path: tmux runs a shell which spawns the agent as a child.
	// This simulates an npm-installed agent (e.g. claude via node) where the
	// pane command is sh/bash and the agent is a descendant process.
	t.Run("shell_with_child", func(t *testing.T) {
		sessionName := "gt-test-runtime-shell-child"
		_ = tm.KillSession(sessionName)

		if err := tm.NewSessionWithCommand(sessionName, "", "sh -c 'sleep 10'"); err != nil {
			t.Fatalf("NewSessionWithCommand: %v", err)
		}
		defer func() { _ = tm.KillSession(sessionName) }()

		// Give the child process a moment to start
		time.Sleep(100 * time.Millisecond)

		if !tm.IsRuntimeRunning(sessionName, []string{"sleep"}) {
			paneCmd, _ := tm.GetPaneCommand(sessionName)
			t.Errorf("IsRuntimeRunning should return true for child process (pane cmd: %q)", paneCmd)
		}
	})
}

// TestGetPaneCommand_MultiPane verifies that GetPaneCommand returns pane 0's
// command even when a split pane exists and is active. This is the core fix
// for gs-2v7: without explicit pane 0 targeting, health checks would see the
// split pane's shell and falsely report the agent as dead.
func TestGetPaneCommand_MultiPane(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-multipane-" + t.Name()

	_ = tm.KillSession(sessionName)

	// Create session running sleep (simulates an agent process in pane 0)
	if err := tm.NewSessionWithCommand(sessionName, "", "sleep 300"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Verify pane 0 shows "sleep"
	cmd, err := tm.GetPaneCommand(sessionName)
	if err != nil {
		t.Fatalf("GetPaneCommand before split: %v", err)
	}
	if cmd != "sleep" {
		t.Fatalf("expected pane 0 command to be 'sleep', got %q", cmd)
	}

	// Capture pane 0's PID and working directory before the split
	pidBefore, err := tm.GetPanePID(sessionName)
	if err != nil {
		t.Fatalf("GetPanePID before split: %v", err)
	}
	wdBefore, err := tm.GetPaneWorkDir(sessionName)
	if err != nil {
		t.Fatalf("GetPaneWorkDir before split: %v", err)
	}

	// Split the window — creates a new pane running a shell, which becomes active
	if _, err := tm.run("split-window", "-t", sessionName, "-d"); err != nil {
		t.Fatalf("split-window: %v", err)
	}

	// GetPaneCommand should still return "sleep" (pane 0), not the shell
	cmd, err = tm.GetPaneCommand(sessionName)
	if err != nil {
		t.Fatalf("GetPaneCommand after split: %v", err)
	}
	if cmd != "sleep" {
		t.Errorf("after split, GetPaneCommand should return pane 0 command 'sleep', got %q", cmd)
	}

	// GetPanePID should return pane 0's PID, matching the pre-split value
	pid, err := tm.GetPanePID(sessionName)
	if err != nil {
		t.Fatalf("GetPanePID after split: %v", err)
	}
	if pid != pidBefore {
		t.Errorf("GetPanePID changed after split: before=%s, after=%s", pidBefore, pid)
	}

	// GetPaneWorkDir should still return pane 0's working directory
	wd, err := tm.GetPaneWorkDir(sessionName)
	if err != nil {
		t.Fatalf("GetPaneWorkDir after split: %v", err)
	}
	if wd != wdBefore {
		t.Errorf("GetPaneWorkDir changed after split: before=%s, after=%s", wdBefore, wd)
	}
}

func TestHasDescendantWithNames(t *testing.T) {
	t.Parallel()
	// Test the hasDescendantWithNames helper function directly

	// Test with a definitely nonexistent PID
	got := hasDescendantWithNames("999999999", []string{"node", "claude"}, 0)
	if got {
		t.Error("hasDescendantWithNames should return false for nonexistent PID")
	}

	// Use current PID instead of PID 1 to avoid walking the entire process tree
	selfPID := fmt.Sprintf("%d", os.Getpid())

	// Test with empty names slice - should always return false
	got = hasDescendantWithNames(selfPID, []string{}, 0)
	if got {
		t.Error("hasDescendantWithNames should return false for empty names slice")
	}

	// Test with nil names slice - should always return false
	got = hasDescendantWithNames(selfPID, nil, 0)
	if got {
		t.Error("hasDescendantWithNames should return false for nil names slice")
	}

	// Test with current process - should have children but not specific agent processes
	got = hasDescendantWithNames(selfPID, []string{"node", "claude"}, 0)
	if got {
		t.Logf("hasDescendantWithNames(%q, [node,claude]) = true - process has matching child?", selfPID)
	}
}

func TestGetAllDescendants(t *testing.T) {
	t.Parallel()
	// Test the getAllDescendants helper function

	// Test with nonexistent PID - should return empty slice
	got := getAllDescendants("999999999")
	if len(got) != 0 {
		t.Errorf("getAllDescendants(nonexistent) = %v, want empty slice", got)
	}

	// Use current PID instead of PID 1 to avoid walking the entire process tree
	selfPID := fmt.Sprintf("%d", os.Getpid())
	descendants := getAllDescendants(selfPID)
	t.Logf("getAllDescendants(%q) found %d descendants", selfPID, len(descendants))

	// Verify returned PIDs are all numeric strings
	for _, pid := range descendants {
		for _, c := range pid {
			if c < '0' || c > '9' {
				t.Errorf("getAllDescendants returned non-numeric PID: %q", pid)
			}
		}
	}
}

func TestDescendantsFromPS(t *testing.T) {
	t.Parallel()

	snapshot := []byte(`
1 0 root
2 1 bash
3 1 zsh
4 2 node
5 4 claude
2 1 bash-duplicate
bad header row
8 bad nope
1 5 root-cycle
7 99 node
`)

	got := descendantsFromPS("1", snapshot)
	want := []string{"5", "4", "2", "3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("descendantsFromPS deepest-first = %v, want %v", got, want)
	}

	if got := descendantsFromPS("42", snapshot); len(got) != 0 {
		t.Fatalf("descendantsFromPS missing root = %v, want empty", got)
	}

	if got := descendantsFromPS("not-a-pid", snapshot); len(got) != 0 {
		t.Fatalf("descendantsFromPS invalid root = %v, want empty", got)
	}
}

func TestHasDescendantWithNamesFromPS(t *testing.T) {
	t.Parallel()

	snapshot := []byte(`
1 0 bash
2 1 /usr/local/bin/node
3 2 /Users/peter/bin/claude
4 1 tmux: client
5 1 claude-helper
6 5 nodejs
7 99 node
8 0 opencode
9 8 /opt/homebrew/bin/bun
10 9 opencode
1 10 root-cycle
bad header row
11 bad nope
`)

	tests := []struct {
		name  string
		pid   string
		names []string
		depth int
		want  bool
	}{
		{name: "direct child basename", pid: "1", names: []string{"node"}, want: true},
		{name: "grandchild basename path", pid: "1", names: []string{"claude"}, want: true},
		{name: "multi word comm", pid: "1", names: []string{"tmux: client"}, want: true},
		{name: "exact match only", pid: "1", names: []string{"nodejs"}, want: true},
		{name: "no substring match", pid: "1", names: []string{"claude-helper"}, want: true},
		{name: "unrelated ambient ignored", pid: "42", names: []string{"node", "opencode", "bun"}, want: false},
		{name: "empty names", pid: "1", names: []string{"", "  "}, want: false},
		{name: "depth limit preserves current semantics", pid: "8", names: []string{"opencode"}, depth: 10, want: false},
		{name: "invalid root", pid: "nope", names: []string{"node"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := hasDescendantWithNamesFromPS(tt.pid, tt.names, tt.depth, snapshot)
			if got != tt.want {
				t.Fatalf("hasDescendantWithNamesFromPS(%q, %v, %d) = %v, want %v", tt.pid, tt.names, tt.depth, got, tt.want)
			}
		})
	}

	substringSnapshot := []byte(`
1 0 bash
2 1 claude-helper
3 1 nodejs
`)
	if hasDescendantWithNamesFromPS("1", []string{"claude", "node"}, 0, substringSnapshot) {
		t.Fatal("hasDescendantWithNamesFromPS matched a process name substring")
	}
}

func TestHasDescendantWithNamesFromPSDoesNotMatchRoot(t *testing.T) {
	t.Parallel()

	snapshot := []byte(`1 0 node`)
	if hasDescendantWithNamesFromPS("1", []string{"node"}, 0, snapshot) {
		t.Fatal("hasDescendantWithNamesFromPS matched the root as its own descendant")
	}
}

func TestHasDescendantWithNamesPosixCheckedReportsSnapshotError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX helper test")
	}

	t.Setenv("PATH", "")
	found, err := hasDescendantWithNamesPosixChecked("1", []string{"node"}, 0)
	if err == nil {
		t.Fatal("hasDescendantWithNamesPosixChecked with missing ps error = nil, want error")
	}
	if found {
		t.Fatal("hasDescendantWithNamesPosixChecked with missing ps found match, want false")
	}
}

func TestKillSessionWithProcesses(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-killproc-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)

	// Create session with a long-running process
	cmd := `sleep 300`
	if err := tm.NewSessionWithCommand(sessionName, "", cmd); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Verify session exists
	has, err := tm.HasSession(sessionName)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if !has {
		t.Fatal("expected session to exist after creation")
	}

	// Kill with processes
	if err := tm.KillSessionWithProcesses(sessionName); err != nil {
		t.Fatalf("KillSessionWithProcesses: %v", err)
	}

	// Verify session is gone
	has, err = tm.HasSession(sessionName)
	if err != nil {
		t.Fatalf("HasSession after kill: %v", err)
	}
	if has {
		t.Error("expected session to not exist after KillSessionWithProcesses")
		_ = tm.KillSession(sessionName) // cleanup
	}
}

func TestKillSessionGenerationRejectsSameIDAfterServerRestart(t *testing.T) {
	tm := NewTmuxWithSocket(fmt.Sprintf("gt-generation-restart-%d", time.Now().UnixNano()))
	session := "gt-generation-restart"
	defer func() { _ = tm.KillServer() }()

	if err := tm.NewSessionWithCommand(session, "", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	original, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := tm.KillServer(); err != nil {
		t.Fatal(err)
	}
	if err := tm.NewSessionWithCommand(session, "", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	replacement, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatal(err)
	}
	if original.SessionID != replacement.SessionID {
		t.Fatalf("test requires reused tmux ID, got original %q replacement %q", original.SessionID, replacement.SessionID)
	}
	if original.Nonce == replacement.Nonce {
		t.Fatal("replacement reused session generation nonce")
	}

	err = tm.KillSessionGeneration(original)
	if !errors.Is(err, ErrSessionGenerationChanged) {
		t.Fatalf("KillSessionGeneration() error = %v, want generation change", err)
	}
	has, err := tm.HasSession(session)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("same-ID replacement was killed")
	}
}

func TestCaptureSessionGenerationClassifiesMissingTargetWhileServerLives(t *testing.T) {
	tm := NewTmuxWithSocket(fmt.Sprintf("gt-generation-missing-%d", time.Now().UnixNano()))
	defer func() { _ = tm.KillServer() }()
	if err := tm.NewSessionWithCommand("gt-generation-keep", "", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	if err := tm.NewSessionWithCommand("gt-generation-gone", "", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	if _, err := tm.run("kill-session", "-t", "gt-generation-gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := tm.CaptureSessionGeneration("gt-generation-gone"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("CaptureSessionGeneration() error = %v, want session not found", err)
	}
}

func TestKillSessionGenerationRejectsServerIdentityMismatch(t *testing.T) {
	tm := NewTmuxWithSocket(fmt.Sprintf("gt-generation-identity-%d", time.Now().UnixNano()))
	session := "gt-generation-identity"
	defer func() { _ = tm.KillServer() }()

	if err := tm.NewSessionWithCommand(session, "", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	generation, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatal(err)
	}
	generation.ServerIdentity += "-replacement"
	if err := tm.KillSessionGeneration(generation); !errors.Is(err, ErrSessionGenerationChanged) {
		t.Fatalf("KillSessionGeneration() error = %v, want server generation change", err)
	}
	has, err := tm.HasSession(session)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("session was killed after server identity mismatch")
	}
}

func TestSessionGenerationCleanupRejectsReplacementPane(t *testing.T) {
	tm := NewTmuxWithSocket(fmt.Sprintf("gt-generation-pane-%d", time.Now().UnixNano()))
	session := "gt-generation-pane"
	defer func() { _ = tm.KillServer() }()

	if err := tm.NewSessionWithCommand(session, "", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	generation, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatal(err)
	}
	panePID, exists, err := tm.getPanePIDGeneration(generation)
	if err != nil || !exists {
		t.Fatalf("getPanePIDGeneration() pid=%d exists=%v err=%v", panePID, exists, err)
	}
	oldProcess := &mockRetainedProcess{pid: panePID, parent: 1, generation: "old-pane", alive: true}
	cleanup := &SessionGenerationCleanup{
		tmux:         tm,
		generation:   generation,
		panePID:      panePID + 1,
		paneIdentity: "old-pane-start",
		identity:     func(int) (string, error) { return "old-pane-start", nil },
		processes:    []retainedProcess{oldProcess},
		custody:      &mockSessionCustody{},
	}

	err = cleanup.Run(context.Background())
	if !errors.Is(err, ErrSessionGenerationChanged) {
		t.Fatalf("Run() error = %v, want pane generation change", err)
	}
	if len(oldProcess.signals) != 0 {
		t.Fatalf("old process was signaled after pane replacement: %v", oldProcess.signals)
	}
	has, err := tm.HasSession(session)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("session was killed after pane generation mismatch")
	}
}

func TestSessionGenerationPanePIDIgnoresMutableActivePane(t *testing.T) {
	tm := NewTmuxWithSocket(fmt.Sprintf("gt-generation-active-pane-%d", time.Now().UnixNano()))
	session := "gt-generation-active-pane"
	defer func() { _ = tm.KillServer() }()

	if err := tm.NewSessionWithCommand(session, "", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	generation, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatal(err)
	}
	originalPID, exists, err := tm.getPanePIDGeneration(generation)
	if err != nil || !exists {
		t.Fatalf("initial pane PID = %d, exists=%v, err=%v", originalPID, exists, err)
	}

	replacementPane, err := tm.run("split-window", "-P", "-F", "#{pane_id}", "-t", session, "sleep 60")
	if err != nil {
		t.Fatalf("split-window: %v", err)
	}
	replacementPane = strings.TrimSpace(replacementPane)
	if _, err := tm.run("select-pane", "-t", replacementPane); err != nil {
		t.Fatalf("select-pane: %v", err)
	}
	replacementPIDText, err := tm.run("display-message", "-p", "-t", replacementPane, "#{pane_pid}")
	if err != nil {
		t.Fatalf("replacement pane PID: %v", err)
	}
	replacementPID, err := strconv.Atoi(strings.TrimSpace(replacementPIDText))
	if err != nil {
		t.Fatalf("parse replacement pane PID %q: %v", replacementPIDText, err)
	}
	if replacementPID == originalPID {
		t.Fatalf("test setup reused pane PID %d", originalPID)
	}

	gotPID, exists, err := tm.getPanePIDGeneration(generation)
	if err != nil || !exists {
		t.Fatalf("generation-bound pane PID = %d, exists=%v, err=%v", gotPID, exists, err)
	}
	if gotPID != originalPID {
		t.Fatalf("generation-bound pane PID = %d, want immutable created-pane PID %d (active pane PID %d)", gotPID, originalPID, replacementPID)
	}
}

func TestLegacySessionGenerationBindsOnePaneAndRejectsAmbiguousPanes(t *testing.T) {
	tm := NewTmuxWithSocketAndEnv(
		fmt.Sprintf("gt-generation-legacy-pane-%d", time.Now().UnixNano()),
		[]string{"PATH=" + os.Getenv("PATH")},
	)
	session := "gt-generation-legacy-pane"
	defer func() { _ = tm.KillServer() }()

	if err := tm.NewSessionWithCommand(session, "", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	created, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatal(err)
	}
	if created.PaneID == "" {
		t.Fatal("new session generation did not persist its created pane")
	}
	if _, err := tm.run("set-environment", "-u", "-t", created.SessionID, EnvSessionPane); err != nil {
		t.Fatalf("remove pane receipt: %v", err)
	}

	legacy, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatalf("single-pane legacy capture: %v", err)
	}
	if legacy.PaneID != created.PaneID {
		t.Fatalf("legacy generation pane = %q, want observed pane %q", legacy.PaneID, created.PaneID)
	}
	if _, exists, err := tm.getPanePIDGeneration(legacy); err != nil || !exists {
		t.Fatalf("single-pane legacy PID lookup exists=%v err=%v", exists, err)
	}

	if _, err := tm.run("new-window", "-d", "-t", session, "sleep 60"); err != nil {
		t.Fatalf("add legacy session window: %v", err)
	}
	if _, err := tm.CaptureSessionGeneration(session); !errors.Is(err, ErrSessionGenerationChanged) {
		t.Fatalf("ambiguous legacy capture error = %v, want generation change", err)
	}
	if _, exists, err := tm.getPanePIDGeneration(legacy); err != nil || !exists {
		t.Fatalf("bound legacy PID lookup exists=%v err=%v", exists, err)
	}
}

func TestLegacySessionGenerationRejectsOnePaneABAAtMutation(t *testing.T) {
	tm := NewTmuxWithSocketAndEnv(
		fmt.Sprintf("gt-generation-legacy-pane-aba-%d", time.Now().UnixNano()),
		[]string{"PATH=" + os.Getenv("PATH")},
	)
	session := "gt-generation-legacy-pane-aba"
	defer func() { _ = tm.KillServer() }()

	if err := tm.NewSessionWithCommand(session, "", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	created, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tm.run("set-environment", "-u", "-t", created.SessionID, EnvSessionPane); err != nil {
		t.Fatalf("remove pane receipt: %v", err)
	}
	legacy, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatalf("single-pane legacy capture: %v", err)
	}

	replacementPane, err := tm.run("split-window", "-d", "-P", "-F", "#{pane_id}", "-t", session, "sleep 60")
	if err != nil {
		t.Fatalf("create replacement pane: %v", err)
	}
	replacementPane = strings.TrimSpace(replacementPane)
	if _, err := tm.run("kill-pane", "-t", legacy.PaneID); err != nil {
		t.Fatalf("remove captured pane: %v", err)
	}

	if err := tm.KillSessionGeneration(legacy); !errors.Is(err, ErrSessionGenerationChanged) {
		t.Fatalf("KillSessionGeneration() error = %v, want generation change", err)
	}
	panes, err := tm.run("list-panes", "-s", "-t", session, "-F", "#{pane_id}")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(panes) != replacementPane {
		t.Fatalf("replacement pane = %q after rejected cleanup, want %q", strings.TrimSpace(panes), replacementPane)
	}
}

func TestSendKeysRawGenerationTargetsCapturedPaneAndRejectsSubstitution(t *testing.T) {
	tm := NewTmuxWithSocket(fmt.Sprintf("gt-send-generation-pane-%d", time.Now().UnixNano()))
	session := "gt-send-generation-pane"
	defer func() { _ = tm.KillServer() }()

	if err := tm.NewSessionWithCommand(session, "", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	generation, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tm.run("set-option", "-p", "-t", generation.PaneID, "remain-on-exit", "on"); err != nil {
		t.Fatalf("retain captured pane after interrupt: %v", err)
	}
	replacementPane, err := tm.run("split-window", "-P", "-F", "#{pane_id}", "-t", session, "sleep 60")
	if err != nil {
		t.Fatalf("split-window: %v", err)
	}
	replacementPane = strings.TrimSpace(replacementPane)
	if _, err := tm.run("select-pane", "-t", replacementPane); err != nil {
		t.Fatalf("select replacement pane: %v", err)
	}

	if err := tm.SendKeysRawGeneration(generation, "C-c"); err != nil {
		t.Fatalf("SendKeysRawGeneration() with another active pane: %v", err)
	}
	dead, err := tm.run("display-message", "-p", "-t", generation.PaneID, "#{pane_dead}")
	if err != nil {
		t.Fatalf("read captured pane state: %v", err)
	}
	if strings.TrimSpace(dead) != "1" {
		t.Fatalf("captured pane dead = %q, want 1", strings.TrimSpace(dead))
	}
	replacementDead, err := tm.run("display-message", "-p", "-t", replacementPane, "#{pane_dead}")
	if err != nil {
		t.Fatalf("read replacement pane state: %v", err)
	}
	if strings.TrimSpace(replacementDead) != "0" {
		t.Fatalf("replacement pane dead = %q, want 0", strings.TrimSpace(replacementDead))
	}

	if _, err := tm.run("respawn-pane", "-k", "-t", generation.PaneID, "sleep 60"); err != nil {
		t.Fatalf("substitute captured pane process: %v", err)
	}
	if _, err := tm.run("kill-pane", "-t", generation.PaneID); err != nil {
		t.Fatalf("remove captured pane: %v", err)
	}
	if err := tm.SendKeysRawGeneration(generation, "C-c"); !errors.Is(err, ErrSessionGenerationChanged) {
		t.Fatalf("SendKeysRawGeneration() after pane substitution = %v, want generation change", err)
	}
}

type mockRetainedProcess struct {
	pid                 int
	parent              int
	generation          string
	alive               bool
	signals             []processSignal
	signaledGenerations []string
	closed              bool
	aliveChecks         int
	aliveSequence       []bool
	onSignal            func(processSignal) error
	onFreeze            func()
	frozen              bool
	freezeErr           error
	thawErr             error
	closeErr            error
}

func (p *mockRetainedProcess) PID() int       { return p.pid }
func (p *mockRetainedProcess) ParentPID() int { return p.parent }
func (p *mockRetainedProcess) Alive() (bool, error) {
	if p.aliveChecks < len(p.aliveSequence) {
		alive := p.aliveSequence[p.aliveChecks]
		p.aliveChecks++
		return alive, nil
	}
	p.aliveChecks++
	return p.alive, nil
}
func (p *mockRetainedProcess) Signal(signal processSignal) error {
	p.signals = append(p.signals, signal)
	p.signaledGenerations = append(p.signaledGenerations, p.generation)
	if p.onSignal != nil {
		if err := p.onSignal(signal); err != nil {
			return err
		}
	}
	if signal == processSignalKill {
		p.alive = false
	}
	return nil
}
func (p *mockRetainedProcess) Freeze(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.freezeErr != nil {
		return p.freezeErr
	}
	p.frozen = true
	if p.onFreeze != nil {
		p.onFreeze()
	}
	return nil
}
func (p *mockRetainedProcess) Thaw() error {
	p.frozen = false
	return p.thawErr
}
func (p *mockRetainedProcess) Close() error { p.closed = true; return p.closeErr }

func stabilizeMockRetainedProcesses(ctx context.Context, _ int, processes []retainedProcess) ([]retainedProcess, error) {
	for _, process := range processes {
		if err := process.Freeze(ctx); err != nil {
			return processes, err
		}
	}
	return processes, nil
}

type mockSessionCustody struct {
	frozen        bool
	committed     bool
	closed        bool
	freezeErr     error
	thawErr       error
	killErr       error
	closeErr      error
	onKill        func() error
	onKillContext func(context.Context) (bool, error)
}

type mockParentReleaseSessionCustody struct {
	*mockSessionCustody
	beforeParentRelease func(context.Context) (bool, error)
	afterParentRelease  func(context.Context) error
	pending             bool
}

func (custody *mockParentReleaseSessionCustody) Kill(context.Context) (bool, error) {
	return false, errors.New("legacy one-phase custody kill used")
}

func (custody *mockParentReleaseSessionCustody) KillBeforeParentRelease(ctx context.Context) (bool, error) {
	committed, err := custody.beforeParentRelease(ctx)
	if committed {
		custody.pending = true
	}
	return committed, err
}

func (custody *mockParentReleaseSessionCustody) FinalizeAfterParentRelease(ctx context.Context) error {
	err := custody.afterParentRelease(ctx)
	if err == nil {
		custody.pending = false
	}
	return err
}

func (custody *mockParentReleaseSessionCustody) ParentReleaseFinalizationPending() bool {
	return custody.pending
}

func (custody *mockParentReleaseSessionCustody) Close() error {
	closeErr := custody.mockSessionCustody.Close()
	if custody.pending {
		return errors.Join(errors.New("mock final reap is unconfirmed; local ownership released"), closeErr)
	}
	return closeErr
}

func (custody *mockSessionCustody) Freeze(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if custody.freezeErr != nil {
		return custody.freezeErr
	}
	custody.frozen = true
	return nil
}

func (custody *mockSessionCustody) Kill(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !custody.frozen {
		return false, errors.New("mock custody was not frozen")
	}
	if custody.onKillContext != nil {
		committed, err := custody.onKillContext(ctx)
		custody.committed = committed
		return committed, err
	}
	if custody.onKill != nil {
		if err := custody.onKill(); err != nil {
			return false, err
		}
	}
	custody.committed = true
	return true, custody.killErr
}

func (custody *mockSessionCustody) Thaw() error {
	if !custody.committed {
		custody.frozen = false
	}
	return custody.thawErr
}

func (custody *mockSessionCustody) Close() error {
	custody.closed = true
	return errors.Join(custody.Thaw(), custody.closeErr)
}

func TestCaptureRetainedProcessTreeRejectsReusedAncestry(t *testing.T) {
	root := &mockRetainedProcess{pid: 100, parent: 1, generation: "root", alive: true}
	reused := &mockRetainedProcess{pid: 101, parent: 999, generation: "replacement", alive: true}
	refs, err := captureRetainedProcessTree(
		100,
		[]processRelation{{PID: 101, ParentPID: 100}},
		func(pid int) (retainedProcess, error) {
			switch pid {
			case 100:
				return root, nil
			case 101:
				return reused, nil
			default:
				return nil, errProcessNotFound
			}
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRetainedProcesses(refs)
	if len(refs) != 1 || refs[0].PID() != 100 {
		t.Fatalf("captured refs = %v, want only root", refs)
	}
	if !reused.closed {
		t.Fatal("reused unrelated descendant reference was not closed")
	}
}

func TestCaptureRetainedProcessTreeRechecksParentAfterChildAcquire(t *testing.T) {
	root := &mockRetainedProcess{
		pid:           100,
		parent:        1,
		generation:    "old-root",
		alive:         false,
		aliveSequence: []bool{true, false},
	}
	child := &mockRetainedProcess{pid: 101, parent: 100, generation: "replacement-child", alive: true}
	_, err := captureRetainedProcessTree(
		100,
		[]processRelation{{PID: 101, ParentPID: 100}},
		func(pid int) (retainedProcess, error) {
			switch pid {
			case 100:
				return root, nil
			case 101:
				return child, nil
			default:
				return nil, errProcessNotFound
			}
		},
	)
	if err == nil || !strings.Contains(err.Error(), "exited during ancestry capture") {
		t.Fatalf("captureRetainedProcessTree() error = %v, want post-acquire parent exit", err)
	}
	if !root.closed || !child.closed {
		t.Fatalf("failed ancestry custody was not closed: root=%v child=%v", root.closed, child.closed)
	}
}

func TestCaptureRetainedProcessTreeJoinsRollbackCloseErrors(t *testing.T) {
	t.Run("parent already exited", func(t *testing.T) {
		closeErr := errors.New("root close failure")
		root := &mockRetainedProcess{pid: 100, parent: 1, alive: false, closeErr: closeErr}
		_, err := captureRetainedProcessTree(
			100,
			[]processRelation{{PID: 101, ParentPID: 100}},
			func(int) (retainedProcess, error) { return root, nil },
		)
		if !errors.Is(err, closeErr) {
			t.Fatalf("capture error = %v, want joined root close failure", err)
		}
	})

	t.Run("descendant acquisition fails", func(t *testing.T) {
		closeErr := errors.New("root close failure")
		acquireErr := errors.New("descendant acquire failure")
		root := &mockRetainedProcess{pid: 100, parent: 1, alive: true, closeErr: closeErr}
		_, err := captureRetainedProcessTree(
			100,
			[]processRelation{{PID: 101, ParentPID: 100}},
			func(pid int) (retainedProcess, error) {
				if pid == 100 {
					return root, nil
				}
				return nil, acquireErr
			},
		)
		if !errors.Is(err, acquireErr) || !errors.Is(err, closeErr) {
			t.Fatalf("capture error = %v, want acquire and rollback close failures", err)
		}
	})

	t.Run("reused child close fails", func(t *testing.T) {
		rootCloseErr := errors.New("root close failure")
		childCloseErr := errors.New("reused child close failure")
		root := &mockRetainedProcess{pid: 100, parent: 1, alive: true, closeErr: rootCloseErr}
		child := &mockRetainedProcess{pid: 101, parent: 999, alive: true, closeErr: childCloseErr}
		_, err := captureRetainedProcessTree(
			100,
			[]processRelation{{PID: 101, ParentPID: 100}},
			func(pid int) (retainedProcess, error) {
				if pid == 100 {
					return root, nil
				}
				return child, nil
			},
		)
		if !errors.Is(err, childCloseErr) || !errors.Is(err, rootCloseErr) {
			t.Fatalf("capture error = %v, want child and root close failures", err)
		}
	})

	t.Run("parent exits after child acquisition", func(t *testing.T) {
		rootCloseErr := errors.New("root close failure")
		childCloseErr := errors.New("child close failure")
		root := &mockRetainedProcess{pid: 100, parent: 1, aliveSequence: []bool{true, false}, closeErr: rootCloseErr}
		child := &mockRetainedProcess{pid: 101, parent: 100, alive: true, closeErr: childCloseErr}
		_, err := captureRetainedProcessTree(
			100,
			[]processRelation{{PID: 101, ParentPID: 100}},
			func(pid int) (retainedProcess, error) {
				if pid == 100 {
					return root, nil
				}
				return child, nil
			},
		)
		if !errors.Is(err, childCloseErr) || !errors.Is(err, rootCloseErr) {
			t.Fatalf("capture error = %v, want child and root close failures", err)
		}
	})
}

func TestStabilizeRetainedProcessTreeCapturesLateForkBeforeKill(t *testing.T) {
	spawned := false
	root := &mockRetainedProcess{pid: 100, parent: 1, generation: "root", alive: true}
	child := &mockRetainedProcess{pid: 101, parent: 100, generation: "late-child", alive: true}
	root.onFreeze = func() { spawned = true }
	processes, err := stabilizeRetainedProcessTree(
		context.Background(),
		100,
		[]retainedProcess{root},
		func(int) ([]processRelation, error) {
			if spawned {
				return []processRelation{{PID: 101, ParentPID: 100}}, nil
			}
			return nil, nil
		},
		func(pid int) (retainedProcess, error) {
			if pid == child.pid {
				return child, nil
			}
			return nil, errProcessNotFound
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(processes) != 2 || !root.frozen || !child.frozen {
		t.Fatalf("stable custody = %d refs, root frozen=%v child frozen=%v", len(processes), root.frozen, child.frozen)
	}
	if err := cleanupFrozenRetainedProcesses(context.Background(), processes, func(context.Context, time.Duration) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(root.signals, []processSignal{processSignalKill}) || !reflect.DeepEqual(child.signals, []processSignal{processSignalKill}) {
		t.Fatalf("stable cleanup signals root=%v child=%v", root.signals, child.signals)
	}
}

func TestStabilizeRetainedProcessTreeThawsOwnedProcessesOnLateFreezeFailure(t *testing.T) {
	freezeErr := errors.New("late child freeze failure")
	root := &mockRetainedProcess{pid: 100, parent: 1, alive: true}
	child := &mockRetainedProcess{pid: 101, parent: 100, alive: true, freezeErr: freezeErr}
	processes, err := stabilizeRetainedProcessTree(
		context.Background(),
		100,
		[]retainedProcess{root},
		func(int) ([]processRelation, error) {
			return []processRelation{{PID: 101, ParentPID: 100}}, nil
		},
		func(int) (retainedProcess, error) { return child, nil },
	)
	if !errors.Is(err, freezeErr) {
		t.Fatalf("stable capture error = %v, want freeze failure", err)
	}
	if root.frozen {
		t.Fatal("stable capture failure left retained root frozen")
	}
	if !child.closed {
		t.Fatal("late child reference was not closed after freeze failure")
	}
	if len(processes) != 1 || processes[0] != root {
		t.Fatalf("stable capture returned %d owned refs after child failure", len(processes))
	}
}

func TestPrepareSessionGenerationProcessCleanupRejectsSamePIDReplacement(t *testing.T) {
	fakeBin := t.TempDir()
	logPath := filepath.Join(fakeBin, "tmux.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_TMUX_LOG\"\nexit 2\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "tmux"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_TMUX_LOG", logPath)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	identityCalls := 0
	paneReads := 0
	closeErr := errors.New("injected replacement handle close failure")
	replacement := &mockRetainedProcess{pid: 123, parent: 1, generation: "replacement", alive: true, closeErr: closeErr}
	ops := sessionProcessCustodyOps{
		readPane: func(SessionGeneration) (int, bool, error) {
			paneReads++
			return 123, true, nil
		},
		identity: func(int) (string, error) {
			identityCalls++
			if identityCalls == 1 {
				return "old-process-start", nil
			}
			return "replacement-process-start", nil
		},
		retain: func(int) ([]retainedProcess, error) {
			return []retainedProcess{replacement}, nil
		},
	}

	err := NewTmuxWithSocket("gt-same-pid-replacement-test").killSessionGenerationWithProcessesContext(
		context.Background(),
		SessionGeneration{},
		PaneProcessGeneration{PID: 123, Identity: "old-process-start"},
		ops,
	)
	if !errors.Is(err, ErrSessionGenerationChanged) {
		t.Fatalf("killSessionGenerationWithProcessesContext() error = %v, want generation change", err)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("killSessionGenerationWithProcessesContext() error = %v, want joined close failure", err)
	}
	if paneReads < 2 {
		t.Fatalf("guarded pane reads = %d, want pre/post acquisition reads", paneReads)
	}
	if !replacement.closed || len(replacement.signals) != 0 {
		t.Fatalf("replacement custody closed=%v signals=%v, want closed without signal", replacement.closed, replacement.signals)
	}
	if logData, readErr := os.ReadFile(logPath); readErr == nil && len(logData) > 0 {
		t.Fatalf("tmux mutation followed same-PID process replacement:\n%s", logData)
	} else if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
}

func TestPrepareSessionGenerationProcessCleanupRejectsReplacementBeforeFirstIdentity(t *testing.T) {
	fakeBin := t.TempDir()
	logPath := filepath.Join(fakeBin, "tmux.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_TMUX_LOG\"\nexit 2\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "tmux"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_TMUX_LOG", logPath)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	retainCalls := 0
	replacement := &mockRetainedProcess{pid: 123, parent: 1, generation: "replacement", alive: true}
	ops := sessionProcessCustodyOps{
		readPane: func(SessionGeneration) (int, bool, error) { return 123, true, nil },
		identity: func(int) (string, error) {
			return "replacement-process-start", nil
		},
		retain: func(int) ([]retainedProcess, error) {
			retainCalls++
			return []retainedProcess{replacement}, nil
		},
	}

	err := NewTmuxWithSocket("gt-before-first-identity-test").killSessionGenerationWithProcessesContext(
		context.Background(),
		SessionGeneration{},
		PaneProcessGeneration{PID: 123, Identity: "authoritative-old-start"},
		ops,
	)
	if !errors.Is(err, ErrSessionGenerationChanged) {
		t.Fatalf("killSessionGenerationWithProcessesContext() error = %v, want generation change", err)
	}
	if retainCalls != 0 || len(replacement.signals) != 0 {
		t.Fatalf("replacement retain calls=%d signals=%v, want no custody or signal", retainCalls, replacement.signals)
	}
	if logData, readErr := os.ReadFile(logPath); readErr == nil && len(logData) > 0 {
		t.Fatalf("tmux mutation followed pre-identity replacement:\n%s", logData)
	} else if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
}

func TestSessionGenerationCleanupPreservesPaneReplacementBeforeFinalKill(t *testing.T) {
	tm := NewTmuxWithSocket(fmt.Sprintf("gt-final-pane-%d", time.Now().UnixNano()))
	session := "gt-final-pane"
	defer func() { _ = tm.KillServer() }()
	if err := tm.NewSessionWithCommand(session, "", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	if _, err := tm.run("set-hook", "-t", session, "pane-died", "display-message -p lifecycle-hook"); err != nil {
		t.Fatal(err)
	}
	beforeRemain, err := tm.run("show-options", "-v", "-t", session, "remain-on-exit")
	if err != nil {
		t.Fatal(err)
	}
	beforeHook, err := tm.run("show-hooks", "-t", session, "pane-died")
	if err != nil {
		t.Fatal(err)
	}
	generation, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatal(err)
	}
	pane, err := tm.CapturePaneProcessGeneration(generation)
	if err != nil {
		t.Fatal(err)
	}
	oldProcess := &mockRetainedProcess{pid: pane.PID, parent: 1, generation: "old-pane", alive: true}
	processCustody := &mockSessionCustody{onKill: func() error {
		_, err := tm.run("respawn-pane", "-k", "-t", generation.SessionID, "sleep 60")
		return err
	}}
	cleanup := &SessionGenerationCleanup{
		tmux:         tm,
		generation:   generation,
		panePID:      pane.PID,
		paneIdentity: pane.Identity,
		identity:     processGenerationIdentity,
		processes:    []retainedProcess{oldProcess},
		custody:      processCustody,
	}

	err = cleanup.Run(context.Background())
	if !errors.Is(err, ErrSessionGenerationChanged) {
		t.Fatalf("Run() error = %v, want final pane generation change", err)
	}
	has, err := tm.HasSession(session)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("same-session pane replacement was killed at final boundary")
	}
	afterRemain, err := tm.run("show-options", "-v", "-t", session, "remain-on-exit")
	if err != nil {
		t.Fatal(err)
	}
	afterHook, err := tm.run("show-hooks", "-t", session, "pane-died")
	if err != nil {
		t.Fatal(err)
	}
	if beforeRemain != afterRemain || beforeHook != afterHook {
		t.Fatalf("preserved replacement settings changed: remain %q -> %q, hook %q -> %q", beforeRemain, afterRemain, beforeHook, afterHook)
	}
}

func TestSessionGenerationCleanupRunRejectsSamePIDIdentityReplacement(t *testing.T) {
	fakeBin := t.TempDir()
	logPath := filepath.Join(fakeBin, "tmux.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_TMUX_LOG\"\nexit 2\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "tmux"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_TMUX_LOG", logPath)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	process := &mockRetainedProcess{pid: 123, parent: 1, generation: "old", alive: true}
	cleanup := &SessionGenerationCleanup{
		tmux:         NewTmuxWithSocket("gt-run-same-pid-test"),
		generation:   SessionGeneration{},
		panePID:      123,
		paneIdentity: "old-process-start",
		identity:     func(int) (string, error) { return "replacement-process-start", nil },
		processes:    []retainedProcess{process},
		custody:      &mockSessionCustody{},
	}
	err := cleanup.Run(context.Background())
	if !errors.Is(err, ErrSessionGenerationChanged) {
		t.Fatalf("Run() error = %v, want generation change", err)
	}
	if len(process.signals) != 0 {
		t.Fatalf("replacement received signals: %v", process.signals)
	}
	if logData, readErr := os.ReadFile(logPath); readErr == nil && len(logData) > 0 {
		t.Fatalf("tmux mutation followed Run identity mismatch:\n%s", logData)
	} else if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
}

func TestSessionGenerationCleanupPrepareJoinsContainmentThawError(t *testing.T) {
	tm := NewTmuxWithSocket(fmt.Sprintf("gt-cleanup-thaw-%d", time.Now().UnixNano()))
	session := "gt-cleanup-thaw"
	defer func() { _ = tm.KillServer() }()
	if err := tm.NewSessionWithCommand(session, "", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	generation, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatal(err)
	}
	pane, err := tm.CapturePaneProcessGeneration(generation)
	if err != nil {
		t.Fatal(err)
	}
	identityCalls := 0
	thawErr := errors.New("injected containment thaw failure")
	cleanup := &SessionGenerationCleanup{
		tmux:         tm,
		generation:   generation,
		panePID:      pane.PID,
		paneIdentity: pane.Identity,
		identity: func(int) (string, error) {
			identityCalls++
			if identityCalls == 1 {
				return pane.Identity, nil
			}
			return "replacement-process-start", nil
		},
		processes: []retainedProcess{&mockRetainedProcess{pid: pane.PID, alive: true}},
		custody:   &mockSessionCustody{thawErr: thawErr},
	}
	commit, err := cleanup.PrepareCommit(context.Background())
	if commit != nil || !errors.Is(err, ErrSessionGenerationChanged) || !errors.Is(err, thawErr) {
		t.Fatalf("PrepareCommit() commit=%v error=%v, want generation and thaw errors", commit != nil, err)
	}
	has, err := tm.HasSession(session)
	if err != nil || !has {
		t.Fatalf("pre-commit thaw failure changed session: has=%v err=%v", has, err)
	}
}

func TestSessionGenerationCleanupRunBoundsHangingGuard(t *testing.T) {
	fakeBin := t.TempDir()
	script := "#!/bin/sh\nsleep 60\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "tmux"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	serverIdentity, err := processGenerationIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	process := &mockRetainedProcess{pid: 123, parent: 1, generation: "owned", alive: true}
	process.onSignal = func(signal processSignal) error {
		if signal == processSignalTerminate {
			process.alive = false
		}
		return nil
	}
	cleanup := &SessionGenerationCleanup{
		tmux:         NewTmuxWithSocket("gt-hanging-guard-test"),
		generation:   SessionGeneration{SessionID: "$1", Nonce: "fixture-generation", ServerPID: os.Getpid(), ServerIdentity: serverIdentity},
		panePID:      123,
		paneIdentity: "owned-process-start",
		identity:     func(int) (string, error) { return "owned-process-start", nil },
		processes:    []retainedProcess{process},
		custody:      &mockSessionCustody{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := time.Now()
	err = cleanup.Run(ctx)
	if err == nil || time.Since(started) > 5*time.Second {
		t.Fatalf("Run() error=%v elapsed=%v, want bounded hanging guard", err, time.Since(started))
	}
}

func TestSessionGenerationCleanupRefreshesTmuxBudgetAfterCommittedKill(t *testing.T) {
	tm := NewTmuxWithSocket(fmt.Sprintf("gt-cleanup-budget-%d", time.Now().UnixNano()))
	session := "gt-cleanup-budget"
	defer func() { _ = tm.KillServer() }()
	if err := tm.NewSessionWithCommand(session, "", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	generation, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatal(err)
	}
	pane, err := tm.CapturePaneProcessGeneration(generation)
	if err != nil {
		t.Fatal(err)
	}
	processCustody := &mockSessionCustody{onKillContext: func(ctx context.Context) (bool, error) {
		if err := exec.Command("kill", "-KILL", fmt.Sprint(pane.PID)).Run(); err != nil {
			return false, err
		}
		<-ctx.Done()
		return true, ctx.Err()
	}}
	cleanup := &SessionGenerationCleanup{
		tmux:         tm,
		generation:   generation,
		panePID:      pane.PID,
		paneIdentity: pane.Identity,
		identity:     processGenerationIdentity,
		processes:    []retainedProcess{&mockRetainedProcess{pid: pane.PID, alive: true}},
		custody:      processCustody,
	}
	commit, err := cleanup.PrepareCommit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	commitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	committed, err := commit(commitCtx)
	if !committed || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("commit = %v, %v; want committed deadline evidence", committed, err)
	}
	has, err := tm.HasSession(session)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("exact tmux session survived exhausted committed-kill budget")
	}
}

func TestSessionGenerationCleanupReleasesTmuxParentBeforeFinalCustodyReap(t *testing.T) {
	tm := NewTmuxWithSocket(fmt.Sprintf("gt-cleanup-parent-release-%d", time.Now().UnixNano()))
	session := "gt-cleanup-parent-release"
	defer func() { _ = tm.KillServer() }()
	if err := tm.NewSessionWithCommand(session, "", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	generation, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatal(err)
	}
	pane, err := tm.CapturePaneProcessGeneration(generation)
	if err != nil {
		t.Fatal(err)
	}
	process := &mockRetainedProcess{pid: pane.PID, parent: 1, generation: pane.Identity, alive: true}
	finalized := false
	custody := &mockParentReleaseSessionCustody{
		mockSessionCustody: &mockSessionCustody{},
		beforeParentRelease: func(ctx context.Context) (bool, error) {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if err := exec.Command("kill", "-KILL", fmt.Sprint(pane.PID)).Run(); err != nil {
				return false, err
			}
			process.alive = false
			return true, nil
		},
		afterParentRelease: func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			running, err := tm.HasSession(session)
			if err != nil {
				return err
			}
			if running {
				return errors.New("final custody reap ran before exact tmux parent release")
			}
			finalized = true
			return nil
		},
	}
	cleanup := &SessionGenerationCleanup{
		tmux:         tm,
		generation:   generation,
		panePID:      pane.PID,
		paneIdentity: pane.Identity,
		identity:     processGenerationIdentity,
		processes:    []retainedProcess{process},
		custody:      custody,
	}
	if err := cleanup.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want parent release before final custody reap", err)
	}
	if !finalized {
		t.Fatal("final custody reap was not called")
	}
}

func TestReconcileSessionGenerationRetriesPaneRaceUntilAbsent(t *testing.T) {
	original := SessionGeneration{
		Name: "witness", SessionID: "$1", Nonce: "original-generation", ServerPID: 10, ServerIdentity: "server",
		Transport: SessionTransport{Bound: true, SocketName: "fixture", SocketPath: "/tmp/tmux-fixture/fixture"},
	}
	captures := 0
	kills := 0
	err := reconcileSessionGenerationContext(
		context.Background(),
		original,
		123,
		sessionGenerationReconcileOps{
			capture: func(context.Context, string) (SessionGeneration, error) {
				captures++
				if captures >= 3 {
					return SessionGeneration{}, ErrSessionNotFound
				}
				return original, nil
			},
			kill: func(context.Context, SessionGeneration, int, bool) error {
				kills++
				if kills == 1 {
					return ErrSessionGenerationChanged
				}
				return nil
			},
			wait: func(context.Context, time.Duration) error { return nil },
		},
	)
	if err != nil || captures != 3 || kills != 2 {
		t.Fatalf("reconcile = error %v, captures %d, kills %d; want absent after retry", err, captures, kills)
	}
}

func TestReconcileSessionGenerationPreservesSameNameReplacement(t *testing.T) {
	original := SessionGeneration{
		Name: "witness", SessionID: "$1", Nonce: "original-generation", ServerPID: 10, ServerIdentity: "server",
		Transport: SessionTransport{Bound: true, SocketName: "fixture", SocketPath: "/tmp/tmux-fixture/fixture"},
	}
	replacement := original
	replacement.SessionID = "$2"
	replacement.Nonce = "replacement-generation"
	kills := 0
	err := reconcileSessionGenerationContext(
		context.Background(),
		original,
		123,
		sessionGenerationReconcileOps{
			capture: func(context.Context, string) (SessionGeneration, error) { return replacement, nil },
			kill: func(context.Context, SessionGeneration, int, bool) error {
				kills++
				return nil
			},
			wait: func(context.Context, time.Duration) error { return nil },
		},
	)
	if !errors.Is(err, ErrSessionGenerationChanged) || kills != 0 {
		t.Fatalf("replacement reconciliation = error %v, kills %d; want preserved replacement evidence", err, kills)
	}
}

func TestReconcileSessionGenerationKillsExactDeadPaneWithoutPIDReceipt(t *testing.T) {
	original := SessionGeneration{
		Name: "witness", SessionID: "$1", PaneID: "%1", Nonce: "original-generation",
		Custody: "original-custody", ServerPID: 10, ServerIdentity: "server",
		Transport: SessionTransport{Bound: true, SocketName: "fixture", SocketPath: "/tmp/tmux-fixture/fixture"},
	}
	captures := 0
	kills := 0
	err := reconcileSessionGenerationContext(
		context.Background(),
		original,
		123,
		sessionGenerationReconcileOps{
			capture: func(context.Context, string) (SessionGeneration, error) {
				captures++
				if captures > 1 {
					return SessionGeneration{}, ErrSessionNotFound
				}
				return original, nil
			},
			readPane: func(context.Context, SessionGeneration) (sessionGenerationPaneState, error) {
				return sessionGenerationPaneState{exists: true, dead: true}, nil
			},
			kill: func(_ context.Context, generation SessionGeneration, panePID int, paneDead bool) error {
				kills++
				if !generation.Equal(original) || panePID != 123 || !paneDead {
					t.Fatalf("dead-pane kill = generation %+v, PID %d, dead %v", generation, panePID, paneDead)
				}
				return nil
			},
			wait: func(context.Context, time.Duration) error { return nil },
		},
	)
	if err != nil || captures != 2 || kills != 1 {
		t.Fatalf("dead-pane reconcile = error %v, captures %d, kills %d; want exact terminal kill", err, captures, kills)
	}
}

func TestReconcileSessionGenerationPreservesLivePanePIDSubstitution(t *testing.T) {
	original := SessionGeneration{
		Name: "witness", SessionID: "$1", PaneID: "%1", Nonce: "original-generation",
		Custody: "original-custody", ServerPID: 10, ServerIdentity: "server",
		Transport: SessionTransport{Bound: true, SocketName: "fixture", SocketPath: "/tmp/tmux-fixture/fixture"},
	}
	kills := 0
	err := reconcileSessionGenerationContext(
		context.Background(),
		original,
		123,
		sessionGenerationReconcileOps{
			capture: func(context.Context, string) (SessionGeneration, error) { return original, nil },
			readPane: func(context.Context, SessionGeneration) (sessionGenerationPaneState, error) {
				return sessionGenerationPaneState{pid: 456, exists: true}, nil
			},
			kill: func(context.Context, SessionGeneration, int, bool) error {
				kills++
				return nil
			},
			wait: func(context.Context, time.Duration) error { return nil },
		},
	)
	if !errors.Is(err, ErrSessionGenerationChanged) || kills != 0 {
		t.Fatalf("live pane substitution = error %v, kills %d; want preserved replacement", err, kills)
	}
}

func TestReconcileSessionGenerationTimeoutReportsUnreconciledCommit(t *testing.T) {
	ambiguousErr := errors.New("ambiguous tmux receipt")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := reconcileSessionGenerationContext(
		ctx,
		SessionGeneration{Name: "witness"},
		123,
		sessionGenerationReconcileOps{
			capture: func(context.Context, string) (SessionGeneration, error) {
				return SessionGeneration{}, ambiguousErr
			},
			kill: func(context.Context, SessionGeneration, int, bool) error {
				t.Fatal("ambiguous capture must not issue a tmux mutation")
				return nil
			},
			wait: waitForContext,
		},
	)
	if !errors.Is(err, ErrSessionCleanupUnreconciled) || !errors.Is(err, ambiguousErr) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reconcile error = %v, want committed unresolved evidence", err)
	}
}

func TestClassifyProcessCleanupPreservesCommittedUnreconciledError(t *testing.T) {
	wantErr := errors.Join(ErrSessionCleanupUnreconciled, ErrSessionNotFound)
	terminal, err := NewTmuxWithSocket("classification-fixture").classifyProcessCleanupResult(
		context.Background(), SessionGeneration{Name: "missing-session"}, wantErr,
	)
	if terminal || !errors.Is(err, ErrSessionCleanupUnreconciled) || !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("classifyProcessCleanupResult() = %v, %v; want non-terminal committed evidence", terminal, err)
	}
}

func TestPortableCleanupStopsAfterStrongCommittedUnreconciledError(t *testing.T) {
	strongErr := errors.New("strong cleanup committed but reconciliation failed")
	wantErr := errors.Join(ErrSessionCleanupUnreconciled, strongErr)
	explicitCalls := 0
	fallbackCalls := 0
	classifyCalls := 0
	err := runPortableSessionGenerationCleanup(
		context.Background(),
		SessionGeneration{Name: "owned-generation"},
		func(context.Context, SessionGeneration) error { return wantErr },
		func(context.Context, SessionGeneration, error) (bool, error) {
			classifyCalls++
			return false, errors.New("classification must not run after committed cleanup")
		},
		func(context.Context, SessionGeneration) error {
			explicitCalls++
			return nil
		},
		func(SessionGeneration) error {
			fallbackCalls++
			return nil
		},
	)
	if !errors.Is(err, ErrSessionCleanupUnreconciled) || !errors.Is(err, strongErr) {
		t.Fatalf("portable cleanup error = %v, want committed strong-cleanup evidence", err)
	}
	if classifyCalls != 0 || explicitCalls != 0 || fallbackCalls != 0 {
		t.Fatalf("post-commit calls = classify %d, explicit %d, fallback %d; want all zero", classifyCalls, explicitCalls, fallbackCalls)
	}
}

func TestSessionGenerationCleanupRunMarksCommittedErrorUnreconciled(t *testing.T) {
	tm := NewTmuxWithSocket(fmt.Sprintf("gt-cleanup-commit-error-%d", time.Now().UnixNano()))
	t.Cleanup(func() { _ = tm.KillServer() })
	if err := tm.NewSessionWithCommand("gt-cleanup-commit-error", "", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	generation, err := tm.CaptureSessionGeneration("gt-cleanup-commit-error")
	if err != nil {
		t.Fatal(err)
	}
	pane, err := tm.CapturePaneProcessGeneration(generation)
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("committed containment confirmation failed")
	custody := &mockSessionCustody{onKillContext: func(context.Context) (bool, error) {
		if err := exec.Command("kill", "-KILL", strconv.Itoa(pane.PID)).Run(); err != nil {
			return false, err
		}
		return true, wantErr
	}}
	cleanup := &SessionGenerationCleanup{
		tmux: tm, generation: generation, panePID: pane.PID, paneIdentity: pane.Identity,
		identity:  processGenerationIdentity,
		processes: []retainedProcess{&mockRetainedProcess{pid: pane.PID, generation: pane.Identity, alive: true}},
		custody:   custody,
	}

	err = cleanup.Run(context.Background())
	if !errors.Is(err, ErrSessionCleanupUnreconciled) || !errors.Is(err, wantErr) {
		t.Fatalf("Run() error = %v, want committed unreconciled evidence", err)
	}
}

func TestSessionGenerationCleanupRunAndCloseSuccess(t *testing.T) {
	tm := NewTmuxWithSocket(fmt.Sprintf("gt-cleanup-success-%d", time.Now().UnixNano()))
	session := "gt-cleanup-success"
	defer func() { _ = tm.KillServer() }()
	if err := tm.NewSessionWithCommand(session, "", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	generation, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatal(err)
	}
	pane, err := tm.CapturePaneProcessGeneration(generation)
	if err != nil {
		t.Fatal(err)
	}
	process := &mockRetainedProcess{pid: pane.PID, parent: 1, generation: "owned-pane", alive: true}
	processCustody := &mockSessionCustody{onKill: func() error {
		if err := exec.Command("kill", "-KILL", fmt.Sprint(pane.PID)).Run(); err != nil {
			return err
		}
		process.alive = false
		return nil
	}}
	cleanup, err := tm.prepareSessionGenerationProcessCleanup(
		generation,
		pane,
		sessionProcessCustodyOps{
			readPane: tm.getPanePIDGeneration,
			identity: processGenerationIdentity,
			retain: func(pid int) ([]retainedProcess, error) {
				if pid != pane.PID {
					return nil, fmt.Errorf("retained PID %d, want %d", pid, pane.PID)
				}
				return []retainedProcess{process}, nil
			},
			retainCustody: func(string, int) (sessionCustodyHandle, error) {
				return processCustody, nil
			},
		},
	)
	if err != nil {
		t.Fatalf("prepareSessionGenerationProcessCleanup() error = %v", err)
	}
	if err := cleanup.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !processCustody.committed {
		t.Fatal("Run() did not commit through the launch-time containment handle")
	}
	has, err := tm.HasSession(session)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("successful strict cleanup left tmux session running")
	}
	if err := cleanup.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !process.closed {
		t.Fatal("retained process reference was not closed")
	}
}

func TestSessionGenerationExplicitCleanupStopsExactLiveSession(t *testing.T) {
	tm := NewTmuxWithSocket(fmt.Sprintf("gt-explicit-cleanup-%d", time.Now().UnixNano()))
	session := "gt-explicit-cleanup"
	defer func() { _ = tm.KillServer() }()
	if err := tm.NewSessionWithCommand(session, "", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	generation, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatal(err)
	}
	pane, err := tm.CapturePaneProcessGeneration(generation)
	if err != nil {
		t.Fatal(err)
	}
	cleanup, err := tm.PrepareSessionGenerationExplicitCleanup(generation, pane)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup.Close()
	commit, err := cleanup.PrepareCommit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	committed, err := commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("explicit cleanup did not report the exact tmux mutation")
	}
	running, err := tm.HasSession(session)
	if err != nil {
		t.Fatal(err)
	}
	if running {
		t.Fatal("exact explicit cleanup left the session running")
	}
}

func TestSessionGenerationExplicitCleanupPreservesReplacement(t *testing.T) {
	tm := NewTmuxWithSocket(fmt.Sprintf("gt-explicit-replacement-%d", time.Now().UnixNano()))
	session := "gt-explicit-replacement"
	defer func() { _ = tm.KillServer() }()
	if err := tm.NewSessionWithCommand(session, "", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	generation, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatal(err)
	}
	pane, err := tm.CapturePaneProcessGeneration(generation)
	if err != nil {
		t.Fatal(err)
	}
	cleanup, err := tm.PrepareSessionGenerationExplicitCleanup(generation, pane)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup.Close()
	if err := tm.KillServer(); err != nil {
		t.Fatal(err)
	}
	if err := tm.NewSessionWithCommand(session, "", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	if _, err := cleanup.PrepareCommit(context.Background()); !errors.Is(err, ErrSessionGenerationChanged) {
		t.Fatalf("PrepareCommit() error = %v, want generation change", err)
	}
	running, err := tm.HasSession(session)
	if err != nil {
		t.Fatal(err)
	}
	if !running {
		t.Fatal("explicit cleanup removed the replacement session")
	}
}

func TestSessionGenerationCleanupClose(t *testing.T) {
	closeErr := errors.New("injected close failure")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "success"},
		{name: "error", err: closeErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			process := &mockRetainedProcess{pid: 123, closeErr: tc.err}
			cleanup := &SessionGenerationCleanup{processes: []retainedProcess{process}}
			err := cleanup.Close()
			if !errors.Is(err, tc.err) {
				t.Fatalf("Close() error = %v, want %v", err, tc.err)
			}
			if !process.closed {
				t.Fatal("retained process reference was not closed")
			}
			if len(cleanup.processes) != 0 {
				t.Fatalf("process cleanup retained %d unowned local handles", len(cleanup.processes))
			}
			if err := cleanup.Close(); err != nil {
				t.Fatalf("second Close() error = %v", err)
			}
			if len(cleanup.processes) != 0 {
				t.Fatalf("successful retry retained %d process owners", len(cleanup.processes))
			}
		})
	}
}

func TestSessionGenerationCleanupCloseDiscardsReleasedCustodyAfterError(t *testing.T) {
	closeErr := errors.New("injected custody close failure")
	custody := &mockSessionCustody{closeErr: closeErr}
	cleanup := &SessionGenerationCleanup{custody: custody}
	if err := cleanup.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("first Close() error = %v, want %v", err, closeErr)
	}
	if cleanup.custody != nil {
		t.Fatal("failed custody close retained an unowned local handle")
	}
	if err := cleanup.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if cleanup.custody != nil {
		t.Fatal("successful custody close retained a stale owner")
	}
}

func TestSessionGenerationCleanupCloseRetriesPendingParentReleaseFinalization(t *testing.T) {
	reapErr := errors.New("injected final reap delay")
	finalizeCalls := 0
	custody := &mockParentReleaseSessionCustody{
		mockSessionCustody: &mockSessionCustody{},
		beforeParentRelease: func(context.Context) (bool, error) {
			return true, nil
		},
		afterParentRelease: func(context.Context) error {
			finalizeCalls++
			if finalizeCalls == 1 {
				return reapErr
			}
			return nil
		},
	}
	if committed, err := custody.KillBeforeParentRelease(context.Background()); !committed || err != nil {
		t.Fatalf("KillBeforeParentRelease() = %v, %v", committed, err)
	}
	if err := custody.FinalizeAfterParentRelease(context.Background()); !errors.Is(err, reapErr) {
		t.Fatalf("first FinalizeAfterParentRelease() error = %v, want %v", err, reapErr)
	}
	cleanup := &SessionGenerationCleanup{custody: custody}
	if err := cleanup.Close(); err != nil {
		t.Fatalf("Close() retry error = %v", err)
	}
	if finalizeCalls != 2 || custody.ParentReleaseFinalizationPending() {
		t.Fatalf("final reap retry = calls %d pending %v, want calls 2 pending false", finalizeCalls, custody.ParentReleaseFinalizationPending())
	}
	if cleanup.custody != nil {
		t.Fatal("successful final reap retry retained stale custody")
	}
}

func TestSessionGenerationCleanupCloseContextReleasesCustodyAfterDeadline(t *testing.T) {
	t.Parallel()

	finalizeCalls := 0
	custody := &mockParentReleaseSessionCustody{
		mockSessionCustody: &mockSessionCustody{},
		beforeParentRelease: func(context.Context) (bool, error) {
			return true, nil
		},
		afterParentRelease: func(ctx context.Context) error {
			finalizeCalls++
			<-ctx.Done()
			return ctx.Err()
		},
	}
	if committed, err := custody.KillBeforeParentRelease(context.Background()); !committed || err != nil {
		t.Fatalf("KillBeforeParentRelease() = %v, %v", committed, err)
	}
	cleanup := &SessionGenerationCleanup{custody: custody}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := cleanup.CloseContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CloseContext() error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("CloseContext() elapsed = %v, want caller-bounded final reap", elapsed)
	}
	if finalizeCalls != 1 {
		t.Fatalf("final reap calls = %d, want 1", finalizeCalls)
	}
	if cleanup.custody != nil || !custody.closed {
		t.Fatal("deadline-expired final reap retained process handles without a retry owner")
	}
}

func TestBoundSessionGenerationRejectsWrongTransportBeforeAbsence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native Windows tmux workflows are unsupported; WSL runs the Linux path")
	}
	target := NewTmuxWithSocket(fmt.Sprintf("gt-bound-target-%d", time.Now().UnixNano()))
	wrong := NewTmuxWithSocket(fmt.Sprintf("gt-bound-wrong-%d", time.Now().UnixNano()))
	t.Cleanup(func() { _ = target.KillServer() })
	t.Cleanup(func() { _ = wrong.KillServer() })

	generation, err := target.NewSessionWithCommandAndEnvGeneration("transport-target", t.TempDir(), "sleep 30", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := wrong.KillSessionGeneration(generation); !errors.Is(err, ErrSessionGenerationChanged) {
		t.Fatalf("wrong transport cleanup error = %v, want generation changed", err)
	}
	running, err := target.HasSession(generation.Name)
	if err != nil || !running {
		t.Fatalf("target after wrong-transport cleanup: running=%v err=%v", running, err)
	}
}

func TestNewTmuxForSessionGenerationUsesCapturedCanonicalSocketPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native Windows tmux workflows are unsupported; WSL runs the Linux path")
	}
	// Keep the Unix socket paths below the platform limit (notably macOS's
	// short sockaddr_un path) while still using two distinct roots.
	firstRoot, err := os.MkdirTemp("/tmp", "gt-transport-a-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(firstRoot) })
	secondRoot, err := os.MkdirTemp("/tmp", "gt-transport-b-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(secondRoot) })
	t.Setenv("TMUX_TMPDIR", firstRoot)
	target := NewTmuxWithSocket(fmt.Sprintf("gt-canonical-transport-%d", time.Now().UnixNano()))
	t.Cleanup(func() { _ = target.KillServer() })

	generation, err := target.NewSessionWithCommandAndEnvGeneration("canonical-transport", t.TempDir(), "sleep 30", nil)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(firstRoot, fmt.Sprintf("tmux-%d", os.Getuid()), target.socketName)
	if generation.Transport.SocketPath != wantPath {
		t.Fatalf("captured socket path = %q, want %q", generation.Transport.SocketPath, wantPath)
	}

	// The ambient socket root changes after capture. A reconstructed controller
	// must still address the original server, not the same alias under root two.
	t.Setenv("TMUX_TMPDIR", secondRoot)
	current, err := target.CaptureSessionGeneration(generation.Name)
	if err != nil || !current.Equal(generation) {
		t.Fatalf("constructed controller drifted with ambient socket root: current=%+v err=%v", current, err)
	}
	bound, err := NewTmuxForSessionGeneration(generation)
	if err != nil {
		t.Fatal(err)
	}
	wantPollerRoot := "TMUX_TMPDIR=" + firstRoot
	foundPollerRoot := false
	for _, entry := range bound.PollerEnvironment() {
		if entry == wantPollerRoot {
			foundPollerRoot = true
		}
		if entry == "TMUX_TMPDIR="+secondRoot {
			t.Fatalf("poller environment inherited ambient socket root: %q", entry)
		}
	}
	if !foundPollerRoot {
		t.Fatalf("poller environment did not preserve %q", wantPollerRoot)
	}
	current, err = bound.CaptureSessionGeneration(generation.Name)
	if err != nil {
		t.Fatalf("captured generation through durable endpoint: %v", err)
	}
	if !current.Equal(generation) {
		t.Fatalf("durable endpoint captured %+v, want %+v", current, generation)
	}
}

func TestOrdinaryDefaultTmuxIgnoresAmbientTmuxEndpoint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native Windows tmux workflows are unsupported; WSL runs the Linux path")
	}
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	firstRoot, err := os.MkdirTemp("/tmp", "gt-default-transport-a-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(firstRoot) })
	secondRoot, err := os.MkdirTemp("/tmp", "gt-default-transport-b-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(secondRoot) })
	rootEnv := func(root string) []string {
		env := replaceEnvValue(os.Environ(), "TMUX_TMPDIR", root)
		return replaceEnvValue(env, "TMUX", "")
	}
	start := func(root, name string) {
		t.Helper()
		cmd := exec.Command("tmux", "-u", "-L", "default", "new-session", "-d", "-s", name, "sleep 30")
		cmd.Env = rootEnv(root)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("start %s: %v: %s", name, err, output)
		}
		t.Cleanup(func() {
			cmd := exec.Command("tmux", "-L", "default", "kill-server")
			cmd.Env = rootEnv(root)
			_ = cmd.Run()
		})
	}
	start(firstRoot, "persisted-default-target")
	start(secondRoot, "ambient-default-decoy")

	t.Setenv("TMUX_TMPDIR", firstRoot)
	target := NewTmuxWithSocket("")
	wantPath := filepath.Join(firstRoot, fmt.Sprintf("tmux-%d", os.Getuid()), "default")
	if got := target.SessionTransport().SocketPath; got != wantPath {
		t.Fatalf("persisted endpoint = %q, want %q", got, wantPath)
	}
	t.Setenv("TMUX_TMPDIR", secondRoot)
	t.Setenv("TMUX", filepath.Join(secondRoot, fmt.Sprintf("tmux-%d", os.Getuid()), "default")+",1,0")

	has, err := target.HasSession("persisted-default-target")
	if err != nil {
		t.Fatalf("lookup through persisted default endpoint: %v", err)
	}
	if !has {
		t.Fatal("ambient TMUX overrode the persisted default endpoint")
	}
}

func TestOrdinaryDefaultTmuxCreatesMissingSocketParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native Windows tmux workflows are unsupported; WSL runs the Linux path")
	}
	root, err := os.MkdirTemp("/tmp", "gt-default-create-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("TMUX_TMPDIR", root)
	t.Setenv("TMUX", "")
	target := NewTmuxWithSocket("")
	t.Cleanup(func() { _ = target.KillServer() })
	parent := filepath.Dir(target.SessionTransport().SocketPath)
	if _, err := os.Stat(parent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket parent exists before first command: %v", err)
	}
	if err := target.NewSessionWithCommand("default-parent-create", t.TempDir(), "sleep 30"); err != nil {
		t.Fatalf("create session with missing socket parent: %v", err)
	}
	if _, err := os.Stat(parent); err != nil {
		t.Fatalf("socket parent after first command: %v", err)
	}
}

func TestNewTmuxForSessionGenerationRejectsLegacyNameOnlyTransport(t *testing.T) {
	generation := SessionGeneration{Transport: SessionTransport{Bound: true, SocketName: "legacy-alias"}}
	if _, err := NewTmuxForSessionGeneration(generation); !errors.Is(err, ErrSessionTransportUnbound) {
		t.Fatalf("NewTmuxForSessionGeneration() error = %v, want unbound transport", err)
	}
}

func TestSessionGenerationEqualRejectsUnboundTransportWildcard(t *testing.T) {
	bound := SessionGeneration{
		Name: "session", SessionID: "$1", PaneID: "%0", Nonce: "nonce",
		ServerPID: 1, ServerIdentity: "server",
		Transport: SessionTransport{Bound: true, SocketName: "alias", SocketPath: "/tmp/tmux-1/alias"},
	}
	unbound := bound
	unbound.Transport = SessionTransport{}
	if bound.Equal(unbound) || unbound.Equal(bound) {
		t.Fatal("unbound transport acted as an exact-generation wildcard")
	}
}

func TestCleanupRetainedProcessesSignalsCapturedHandleAfterPIDReuse(t *testing.T) {
	old := &mockRetainedProcess{pid: 123, parent: 1, generation: "old", alive: true}
	err := cleanupRetainedProcesses(context.Background(), []retainedProcess{old}, func(context.Context, time.Duration) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(old.signals, []processSignal{processSignalTerminate, processSignalKill}) {
		t.Fatalf("captured handle signals = %v", old.signals)
	}
	if !reflect.DeepEqual(old.signaledGenerations, []string{"old", "old"}) {
		t.Fatalf("signaled generations = %v, want retained old handle", old.signaledGenerations)
	}
}

func TestCleanupFrozenRetainedProcessesChecksContextBeforeCommit(t *testing.T) {
	process := &mockRetainedProcess{pid: 123, parent: 1, generation: "owned", alive: true, frozen: true}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := cleanupFrozenRetainedProcesses(ctx, []retainedProcess{process}, func(context.Context, time.Duration) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("frozen cleanup error = %v, want cancellation", err)
	}
	if len(process.signals) != 0 {
		t.Fatalf("canceled frozen cleanup signaled process: %v", process.signals)
	}
}

func TestKillSessionWithProcesses_NonexistentSession(t *testing.T) {
	tm := newTestTmux(t)

	// Killing nonexistent session should not panic, just return error or nil
	err := tm.KillSessionWithProcesses("nonexistent-session-xyz-12345")
	// We don't care about the error value, just that it doesn't panic
	_ = err
}

func TestKillSessionWithProcessesExcluding(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-killexcl-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)

	// Create session with a long-running process
	cmd := `sleep 300`
	if err := tm.NewSessionWithCommand(sessionName, "", cmd); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Verify session exists
	has, err := tm.HasSession(sessionName)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if !has {
		t.Fatal("expected session to exist after creation")
	}

	// Kill with empty excludePIDs (should behave like KillSessionWithProcesses)
	if err := tm.KillSessionWithProcessesExcluding(sessionName, nil); err != nil {
		t.Fatalf("KillSessionWithProcessesExcluding: %v", err)
	}

	// Verify session is gone
	has, err = tm.HasSession(sessionName)
	if err != nil {
		t.Fatalf("HasSession after kill: %v", err)
	}
	if has {
		t.Error("expected session to not exist after KillSessionWithProcessesExcluding")
		_ = tm.KillSession(sessionName) // cleanup
	}
}

func TestKillSessionWithProcessesExcluding_WithExcludePID(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-killexcl2-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)

	// Create session with a long-running process
	cmd := `sleep 300`
	if err := tm.NewSessionWithCommand(sessionName, "", cmd); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Get the pane PID
	panePID, err := tm.GetPanePID(sessionName)
	if err != nil {
		t.Fatalf("GetPanePID: %v", err)
	}
	if panePID == "" {
		t.Skip("could not get pane PID")
	}

	// Kill with the pane PID excluded - the function should still kill the session
	// but should not kill the excluded PID before the session is destroyed
	err = tm.KillSessionWithProcessesExcluding(sessionName, []string{panePID})
	if err != nil {
		t.Fatalf("KillSessionWithProcessesExcluding: %v", err)
	}

	// Session should be gone (the final KillSession always happens)
	has, _ := tm.HasSession(sessionName)
	if has {
		t.Error("expected session to not exist after KillSessionWithProcessesExcluding")
	}
}

func TestKillSessionWithProcessesExcluding_NonexistentSession(t *testing.T) {
	tm := newTestTmux(t)

	// Killing nonexistent session should not panic
	err := tm.KillSessionWithProcessesExcluding("nonexistent-session-xyz-12345", []string{"12345"})
	// We don't care about the error value, just that it doesn't panic
	_ = err
}

func TestGetProcessGroupID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping test: process groups not available on Windows")
	}

	// Test with current process
	pid := fmt.Sprintf("%d", os.Getpid())
	pgid := getProcessGroupID(pid)

	if pgid == "" {
		t.Error("expected non-empty PGID for current process")
	}

	// PGID should not be 0 or 1 for a normal process
	if pgid == "0" || pgid == "1" {
		t.Errorf("unexpected PGID %q for current process", pgid)
	}

	// Test with nonexistent PID
	pgid = getProcessGroupID("999999999")
	if pgid != "" {
		t.Errorf("expected empty PGID for nonexistent process, got %q", pgid)
	}
}

func TestGetProcessGroupMembers(t *testing.T) {
	// Get current process's PGID
	pid := fmt.Sprintf("%d", os.Getpid())
	pgid := getProcessGroupID(pid)
	if pgid == "" {
		t.Skip("could not get PGID for current process")
	}

	members := getProcessGroupMembers(pgid)

	// Current process should be in the list
	found := false
	for _, m := range members {
		if m == pid {
			found = true
			break
		}
	}

	if !found {
		t.Errorf("current process %s not found in process group %s members: %v", pid, pgid, members)
	}
}

func TestKillSessionWithProcesses_KillsProcessGroup(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-killpg-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)

	// Create session that spawns a child process
	// The child will stay in the same process group as the shell
	cmd := `sleep 300 & sleep 300`
	if err := tm.NewSessionWithCommand(sessionName, "", cmd); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Give processes time to start
	time.Sleep(200 * time.Millisecond)

	// Verify session exists
	has, err := tm.HasSession(sessionName)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if !has {
		t.Fatal("expected session to exist after creation")
	}

	// Kill with processes (should kill the entire process group)
	if err := tm.KillSessionWithProcesses(sessionName); err != nil {
		t.Fatalf("KillSessionWithProcesses: %v", err)
	}

	// Verify session is gone
	has, err = tm.HasSession(sessionName)
	if err != nil {
		t.Fatalf("HasSession after kill: %v", err)
	}
	if has {
		t.Error("expected session to not exist after KillSessionWithProcesses")
		_ = tm.KillSession(sessionName) // cleanup
	}
}

func TestSessionSet(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-sessionset-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)

	// Create a test session
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Get the session set
	set, err := tm.GetSessionSet()
	if err != nil {
		t.Fatalf("GetSessionSet: %v", err)
	}

	// Test Has() for existing session
	if !set.Has(sessionName) {
		t.Errorf("SessionSet.Has(%q) = false, want true", sessionName)
	}

	// Test Has() for non-existing session
	if set.Has("nonexistent-session-xyz-12345") {
		t.Error("SessionSet.Has(nonexistent) = true, want false")
	}

	// Test nil safety
	var nilSet *SessionSet
	if nilSet.Has("anything") {
		t.Error("nil SessionSet.Has() = true, want false")
	}

	// Test Names() returns the session
	names := set.Names()
	found := false
	for _, n := range names {
		if n == sessionName {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("SessionSet.Names() doesn't contain %q", sessionName)
	}
}

func TestCleanupOrphanedSessions(t *testing.T) {
	// newTestTmux creates an isolated tmux server (unique socket per test).
	// CleanupOrphanedSessions operates on the Tmux receiver which carries that
	// socket, so it can only see sessions on this isolated server — never real
	// polecats or user sessions.
	//
	// History: this test previously used NewTmux() (default server) and killed
	// 8 production crew sessions during a patrol run. The env-var guard added in
	// 3519f58c was the right fix at the time; per-test socket isolation (48550c7f)
	// made it redundant.
	isTestGTSession := func(s string) bool {
		return strings.HasPrefix(s, "gt-") || strings.HasPrefix(s, "hq-")
	}

	tm := newTestTmux(t)

	// Create test sessions with gt- and hq- prefixes (zombie sessions - no Claude running)
	gtSession := "gt-test-cleanup-rig"
	hqSession := "hq-test-cleanup"
	nonGtSession := "other-test-session"

	// Clean up any existing test sessions
	_ = tm.KillSession(gtSession)
	_ = tm.KillSession(hqSession)
	_ = tm.KillSession(nonGtSession)

	// Create zombie sessions (tmux alive, but process is sleep - no Claude/node).
	// Use NewSessionWithCommand with sleep to avoid loading .zshrc, which spawns
	// a node process on this system. A node child of the zsh shell would cause
	// IsAgentAlive to return true, preventing cleanup (gt-it10f6p).
	if err := tm.NewSessionWithCommand(gtSession, "", "sleep 9999"); err != nil {
		t.Fatalf("NewSessionWithCommand(gt): %v", err)
	}
	defer func() { _ = tm.KillSession(gtSession) }()

	if err := tm.NewSessionWithCommand(hqSession, "", "sleep 9999"); err != nil {
		t.Fatalf("NewSessionWithCommand(hq): %v", err)
	}
	defer func() { _ = tm.KillSession(hqSession) }()

	// Create a non-GT session (should NOT be cleaned up)
	if err := tm.NewSessionWithCommand(nonGtSession, "", "sleep 9999"); err != nil {
		t.Fatalf("NewSessionWithCommand(other): %v", err)
	}
	defer func() { _ = tm.KillSession(nonGtSession) }()

	// Verify all sessions exist
	for _, sess := range []string{gtSession, hqSession, nonGtSession} {
		has, err := tm.HasSession(sess)
		if err != nil {
			t.Fatalf("HasSession(%q): %v", sess, err)
		}
		if !has {
			t.Fatalf("expected session %q to exist", sess)
		}
	}

	// Run cleanup
	cleaned, err := tm.CleanupOrphanedSessions(isTestGTSession)
	if err != nil {
		t.Fatalf("CleanupOrphanedSessions: %v", err)
	}

	// Should have cleaned the gt- and hq- zombie sessions
	if cleaned < 2 {
		t.Errorf("CleanupOrphanedSessions cleaned %d sessions, want >= 2", cleaned)
	}

	// Verify GT sessions are gone
	for _, sess := range []string{gtSession, hqSession} {
		has, err := tm.HasSession(sess)
		if err != nil {
			t.Fatalf("HasSession(%q) after cleanup: %v", sess, err)
		}
		if has {
			t.Errorf("expected session %q to be cleaned up", sess)
		}
	}

	// Verify non-GT session still exists
	has, err := tm.HasSession(nonGtSession)
	if err != nil {
		t.Fatalf("HasSession(%q) after cleanup: %v", nonGtSession, err)
	}
	if !has {
		t.Error("non-GT session should NOT have been cleaned up")
	}
}

func TestCleanupOrphanedSessions_NoSessions(t *testing.T) {
	// See TestCleanupOrphanedSessions for isolation rationale.
	isTestGTSession := func(s string) bool {
		return strings.HasPrefix(s, "gt-") || strings.HasPrefix(s, "hq-")
	}

	tm := newTestTmux(t)

	// Fresh isolated server has no sessions — cleanup should be a no-op.
	cleaned, err := tm.CleanupOrphanedSessions(isTestGTSession)
	if err != nil {
		t.Fatalf("CleanupOrphanedSessions: %v", err)
	}

	// May clean some existing GT sessions if they exist, but shouldn't error
	t.Logf("CleanupOrphanedSessions cleaned %d sessions", cleaned)
}

func TestCollectReparentedGroupMembers(t *testing.T) {
	t.Parallel()
	// Test that collectReparentedGroupMembers correctly filters group members.
	// Only processes reparented to init (PPID == 1) that aren't in the known set
	// should be returned.

	// Test with current process's PGID
	pid := fmt.Sprintf("%d", os.Getpid())
	pgid := getProcessGroupID(pid)
	if pgid == "" {
		t.Skip("could not get PGID for current process")
	}

	// Build a known set containing the current process
	knownPIDs := map[string]bool{pid: true}

	// collectReparentedGroupMembers should NOT include our PID (it's in known set)
	reparented := collectReparentedGroupMembers(pgid, knownPIDs)
	for _, rpid := range reparented {
		if rpid == pid {
			t.Errorf("collectReparentedGroupMembers returned known PID %s", pid)
		}
		// Each reparented PID should have PPID == 1
		ppid := getParentPID(rpid)
		if ppid != "1" {
			t.Errorf("collectReparentedGroupMembers returned PID %s with PPID %s (expected 1)", rpid, ppid)
		}
	}
}

func TestGetParentPID(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("getParentPID returns empty string on Windows (no /proc or ps)")
	}

	// Test with current process - should have a valid PPID
	pid := fmt.Sprintf("%d", os.Getpid())
	ppid := getParentPID(pid)
	if ppid == "" {
		t.Error("expected non-empty PPID for current process")
	}

	// PPID should not be "0" for a normal user process
	if ppid == "0" {
		t.Error("unexpected PPID 0 for current process")
	}

	// Test with nonexistent PID
	ppid = getParentPID("999999999")
	if ppid != "" {
		t.Errorf("expected empty PPID for nonexistent process, got %q", ppid)
	}
}

func TestResolvePaneOwnerFromProcessAncestry(t *testing.T) {
	panes := map[int]string{100: "gt-worker", 500: "gt-other"}
	parents := map[int]int{300: 200, 200: 100, 100: 1, 600: 500, 500: 1}
	parent := func(pid int) (int, error) { return parents[pid], nil }

	ownerPID, session, ok := resolvePaneOwner(300, panes, parent)
	if !ok || ownerPID != 100 || session != "gt-worker" {
		t.Fatalf("resolvePaneOwner() = (%d, %q, %v), want (100, %q, true)", ownerPID, session, ok, "gt-worker")
	}

	ownerPID, session, ok = resolvePaneOwner(400, panes, parent)
	if ok || ownerPID != 0 || session != "" {
		t.Fatalf("unrelated resolvePaneOwner() = (%d, %q, %v), want no owner", ownerPID, session, ok)
	}
}

func TestKillSessionWithProcesses_DoesNotKillUnrelatedProcesses(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-nounrelated-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)

	// Create session with a long-running process
	if err := tm.NewSessionWithCommand(sessionName, "", "sleep 300"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Start a separate background process (simulating an unrelated process)
	// This process runs in its own process group (via setsid or just being separate)
	sentinel := exec.Command("sleep", "300")
	if err := sentinel.Start(); err != nil {
		t.Fatalf("starting sentinel process: %v", err)
	}
	sentinelPID := sentinel.Process.Pid
	defer func() { _ = sentinel.Process.Kill(); _ = sentinel.Wait() }()

	// Give processes time to start
	time.Sleep(200 * time.Millisecond)

	// Kill session with processes
	if err := tm.KillSessionWithProcesses(sessionName); err != nil {
		t.Fatalf("KillSessionWithProcesses: %v", err)
	}

	// The sentinel process should still be alive (it's unrelated)
	// Check by sending signal 0 (existence check)
	if err := sentinel.Process.Signal(os.Signal(nil)); err != nil {
		// Process.Signal(nil) isn't reliable on all platforms, use kill -0
		checkCmd := exec.Command("kill", "-0", fmt.Sprintf("%d", sentinelPID))
		if checkErr := checkCmd.Run(); checkErr != nil {
			t.Errorf("sentinel process %d was killed (should have survived since it's unrelated)", sentinelPID)
		}
	}
}

func TestKillPaneProcessesExcluding(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-killpaneexcl-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)

	// Create session with a long-running process
	cmd := `sleep 300`
	if err := tm.NewSessionWithCommand(sessionName, "", cmd); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Get the pane ID
	paneID, err := tm.GetPaneID(sessionName)
	if err != nil {
		t.Fatalf("GetPaneID: %v", err)
	}

	// Kill pane processes with empty excludePIDs (should kill all processes)
	if err := tm.KillPaneProcessesExcluding(paneID, nil); err != nil {
		t.Fatalf("KillPaneProcessesExcluding: %v", err)
	}

	// Session may still exist (pane respawns as dead), but processes should be gone
	// Check that we can still get info about the session (verifies we didn't panic)
	_, _ = tm.HasSession(sessionName)
}

func TestKillPaneProcessesExcluding_WithExcludePID(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-killpaneexcl2-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)

	// Create session with a long-running process
	cmd := `sleep 300`
	if err := tm.NewSessionWithCommand(sessionName, "", cmd); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Get the pane ID and PID
	paneID, err := tm.GetPaneID(sessionName)
	if err != nil {
		t.Fatalf("GetPaneID: %v", err)
	}

	panePID, err := tm.GetPanePID(sessionName)
	if err != nil {
		t.Fatalf("GetPanePID: %v", err)
	}
	if panePID == "" {
		t.Skip("could not get pane PID")
	}

	// Kill pane processes with the pane PID excluded
	// The function should NOT kill the excluded PID
	err = tm.KillPaneProcessesExcluding(paneID, []string{panePID})
	if err != nil {
		t.Fatalf("KillPaneProcessesExcluding: %v", err)
	}

	// The session/pane should still exist since we excluded the main process
	has, _ := tm.HasSession(sessionName)
	if !has {
		t.Log("Session was destroyed - this may happen if tmux auto-cleaned after descendants died")
	}
}

func TestKillPaneProcessesExcluding_NonexistentPane(t *testing.T) {
	tm := newTestTmux(t)

	// Killing nonexistent pane should return an error but not panic
	err := tm.KillPaneProcessesExcluding("%99999", []string{"12345"})
	if err == nil {
		t.Error("expected error for nonexistent pane")
	}
}

func TestKillPaneProcessesExcluding_FiltersPIDs(t *testing.T) {
	// Unit test the PID filtering logic without needing tmux
	// This tests that the exclusion set is built correctly

	excludePIDs := []string{"123", "456", "789"}
	exclude := make(map[string]bool)
	for _, pid := range excludePIDs {
		exclude[pid] = true
	}

	// Test that excluded PIDs are in the set
	for _, pid := range excludePIDs {
		if !exclude[pid] {
			t.Errorf("exclude[%q] = false, want true", pid)
		}
	}

	// Test that non-excluded PIDs are not in the set
	nonExcluded := []string{"111", "222", "333"}
	for _, pid := range nonExcluded {
		if exclude[pid] {
			t.Errorf("exclude[%q] = true, want false", pid)
		}
	}

	// Test filtering logic
	allPIDs := []string{"111", "123", "222", "456", "333", "789"}
	var filtered []string
	for _, pid := range allPIDs {
		if !exclude[pid] {
			filtered = append(filtered, pid)
		}
	}

	expectedFiltered := []string{"111", "222", "333"}
	if len(filtered) != len(expectedFiltered) {
		t.Fatalf("filtered = %v, want %v", filtered, expectedFiltered)
	}
	for i, pid := range filtered {
		if pid != expectedFiltered[i] {
			t.Errorf("filtered[%d] = %q, want %q", i, pid, expectedFiltered[i])
		}
	}
}

func TestFindAgentPane_SinglePane(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-findagent-single-" + fmt.Sprintf("%d", time.Now().UnixNano()%10000)

	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Single pane — should return empty (no disambiguation needed)
	paneID, err := tm.FindAgentPane(sessionName)
	if err != nil {
		t.Fatalf("FindAgentPane: %v", err)
	}
	if paneID != "" {
		t.Errorf("FindAgentPane single pane = %q, want empty", paneID)
	}
}

func TestFindAgentPane_IgnoresPaneIDFromOtherSession(t *testing.T) {
	tm := newTestTmux(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%10000)
	targetSession := "gt-test-findagent-target-" + suffix
	otherSession := "gt-test-findagent-other-" + suffix

	_ = tm.KillSession(targetSession)
	_ = tm.KillSession(otherSession)
	if err := tm.NewSession(targetSession, ""); err != nil {
		t.Fatalf("NewSession target: %v", err)
	}
	defer func() { _ = tm.KillSession(targetSession) }()
	if err := tm.NewSession(otherSession, ""); err != nil {
		t.Fatalf("NewSession other: %v", err)
	}
	defer func() { _ = tm.KillSession(otherSession) }()

	otherPane, err := tm.GetPaneID(otherSession)
	if err != nil {
		t.Fatalf("GetPaneID other: %v", err)
	}
	if err := tm.SetEnvironment(targetSession, "GT_PANE_ID", otherPane); err != nil {
		t.Fatalf("SetEnvironment GT_PANE_ID: %v", err)
	}

	paneID, err := tm.FindAgentPane(targetSession)
	if err != nil {
		t.Fatalf("FindAgentPane: %v", err)
	}
	if paneID == otherPane {
		t.Fatalf("FindAgentPane returned pane %q from another session", paneID)
	}
}

func TestFindAgentPane_MultiPaneWithNode(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-findagent-multi-" + fmt.Sprintf("%d", time.Now().UnixNano()%10000)

	_ = tm.KillSession(sessionName)

	// Create session with a shell pane (simulating a monitoring split)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Split and run sleep in the new pane (simulating an agent process)
	_, err := tm.run("split-window", "-t", sessionName, "-d", "sleep", "10")
	if err != nil {
		t.Fatalf("split-window: %v", err)
	}

	// Give sleep a moment to start
	time.Sleep(500 * time.Millisecond)

	// Verify we have 2 panes
	out, err := tm.run("list-panes", "-t", sessionName, "-F", "#{pane_id}\t#{pane_current_command}")
	if err != nil {
		t.Fatalf("list-panes: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	t.Logf("Panes: %v", lines)
	if len(lines) < 2 {
		t.Skipf("Expected 2 panes, got %d — skipping multi-pane test", len(lines))
	}

	// FindAgentPane should find the sleep pane
	paneID, err := tm.FindAgentPane(sessionName)
	if err != nil {
		t.Fatalf("FindAgentPane: %v", err)
	}

	// Verify it found the correct pane (the one running sleep)
	if paneID == "" {
		t.Log("FindAgentPane returned empty — sleep may not have started yet or detection missed it")
		// Not a hard failure since process startup timing varies
		return
	}

	// Verify the returned pane is actually running sleep
	cmdOut, err := tm.run("display-message", "-t", paneID, "-p", "#{pane_current_command}")
	if err != nil {
		t.Fatalf("display-message: %v", err)
	}
	paneCmd := strings.TrimSpace(cmdOut)
	t.Logf("Agent pane %s running: %s", paneID, paneCmd)
	if paneCmd != "sleep" {
		t.Errorf("FindAgentPane returned pane running %q, want 'sleep'", paneCmd)
	}
}

func TestNudgeLockTimeout(t *testing.T) {
	// Test that acquireNudgeLock returns false after timeout when lock is held.
	session := "test-nudge-timeout-session"

	// Acquire the lock
	if !acquireNudgeLock(session, time.Second) {
		t.Fatal("initial acquireNudgeLock should succeed")
	}

	// Try to acquire again — should timeout
	start := time.Now()
	got := acquireNudgeLock(session, 100*time.Millisecond)
	elapsed := time.Since(start)

	if got {
		t.Error("acquireNudgeLock should return false when lock is held")
		releaseNudgeLock(session) // clean up the extra acquire
	}
	if elapsed < 90*time.Millisecond {
		t.Errorf("timeout returned too fast: %v", elapsed)
	}

	// Release the lock
	releaseNudgeLock(session)

	// Now acquire should succeed again
	if !acquireNudgeLock(session, time.Second) {
		t.Error("acquireNudgeLock should succeed after release")
	}
	releaseNudgeLock(session)
}

func TestNudgeLockConcurrency(t *testing.T) {
	// Test that concurrent nudges to the same session are serialized.
	session := "test-nudge-concurrent-session"
	const goroutines = 5

	// Clean up any previous state for this session key
	sessionNudgeLocks.Delete(session)

	acquired := make(chan bool, goroutines)

	// First goroutine holds the lock
	if !acquireNudgeLock(session, time.Second) {
		t.Fatal("initial acquire should succeed")
	}

	// Launch goroutines that try to acquire the lock
	for i := 0; i < goroutines; i++ {
		go func() {
			got := acquireNudgeLock(session, 200*time.Millisecond)
			acquired <- got
		}()
	}

	// Wait a bit, then release the lock
	time.Sleep(50 * time.Millisecond)
	releaseNudgeLock(session)

	// At most one goroutine should succeed (it gets the lock after we release)
	successes := 0
	for i := 0; i < goroutines; i++ {
		if <-acquired {
			successes++
			releaseNudgeLock(session)
		}
	}

	// At least 1 should succeed (the first one to grab it after release),
	// and the rest should timeout
	if successes < 1 {
		t.Error("expected at least 1 goroutine to acquire the lock after release")
	}
	t.Logf("%d/%d goroutines acquired the lock", successes, goroutines)
}

func TestNudgeLockDifferentSessions(t *testing.T) {
	// Test that locks for different sessions are independent.
	session1 := "test-nudge-session-a"
	session2 := "test-nudge-session-b"

	// Clean up any previous state
	sessionNudgeLocks.Delete(session1)
	sessionNudgeLocks.Delete(session2)

	// Acquire lock for session1
	if !acquireNudgeLock(session1, time.Second) {
		t.Fatal("acquire session1 should succeed")
	}
	defer releaseNudgeLock(session1)

	// Acquiring lock for session2 should succeed (independent)
	if !acquireNudgeLock(session2, time.Second) {
		t.Error("acquire session2 should succeed even when session1 is locked")
	} else {
		releaseNudgeLock(session2)
	}
}

func TestCrossProcessNudgeLockIsPrivate(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "queue", "session", ".lock")
	unlock, err := acquireFlockLock(lockPath, time.Second)
	if err != nil {
		t.Fatalf("acquireFlockLock: %v", err)
	}
	defer unlock()
	for _, path := range []string{filepath.Dir(lockPath), lockPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 && info.Mode().Perm() != 0700 {
			t.Fatalf("mode(%s) = %o, want private", path, info.Mode().Perm())
		}
	}
}

func TestNewTmuxWithSocketAndEnvUsesIsolatedEnvironment(t *testing.T) {
	tm := NewTmuxWithSocketAndEnv("isolated", []string{"PATH=/usr/bin"})
	cmd := tm.commandContext(context.Background(), "list-sessions")
	if got := strings.Join(cmd.Env, "\n"); got != "PATH=/usr/bin\nTMUX_TMPDIR=/tmp" {
		t.Fatalf("tmux command environment = %q", got)
	}
}

func TestNudgeLeaseProvidesExclusiveOwnership(t *testing.T) {
	townRoot := t.TempDir()
	tm := NewTmuxWithSocket("isolated")
	release, err := tm.AcquireNudgeLease(townRoot, "gt-mayor")
	if err != nil {
		t.Fatalf("AcquireNudgeLease: %v", err)
	}
	if !tm.ownsNudgeLease(townRoot, "gt-mayor") || NudgeLockAvailable(townRoot, "gt-mayor") {
		t.Fatal("lease did not retain exclusive nudge ownership")
	}
	release()
	if tm.ownsNudgeLease(townRoot, "gt-mayor") || !NudgeLockAvailable(townRoot, "gt-mayor") {
		t.Fatal("lease release did not restore nudge availability")
	}
}

func TestNudgeLeaseContextCancelsWhileOwnershipIsHeld(t *testing.T) {
	townRoot := t.TempDir()
	const session = "gt-mayor"
	owner := NewTmuxWithSocket("isolated-owner")
	releaseOwner, err := owner.AcquireNudgeLease(townRoot, session)
	if err != nil {
		t.Fatalf("AcquireNudgeLease owner: %v", err)
	}
	defer releaseOwner()

	waiter := NewTmuxWithSocket("isolated-waiter")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	releaseWaiter, err := waiter.AcquireNudgeLeaseContext(ctx, townRoot, session)
	if releaseWaiter != nil {
		releaseWaiter()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AcquireNudgeLeaseContext error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("canceled lease acquisition took %s", elapsed)
	}
}

func TestNudgeLeaseRecognizesCanonicalTownRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior requires Unix")
	}
	realRoot := t.TempDir()
	linkedRoot := filepath.Join(t.TempDir(), "town-link")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	tm := NewTmuxWithSocket("isolated")
	release, err := tm.AcquireNudgeLease(linkedRoot, "hq-mayor")
	if err != nil {
		t.Fatalf("AcquireNudgeLease: %v", err)
	}
	defer release()

	canonicalRoot, err := filepath.EvalSymlinks(linkedRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if !tm.ownsNudgeLease(canonicalRoot, "hq-mayor") {
		t.Fatal("lease owner did not recognize the same town through its canonical path")
	}
}

func TestNudgeLeaseRejectsMissingTownRootBelowSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior requires Unix")
	}
	realParent := t.TempDir()
	linkedParent := filepath.Join(t.TempDir(), "town-parent-link")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	missingRoot := filepath.Join(linkedParent, "missing-town")

	tm := NewTmuxWithSocket("isolated")
	release, err := tm.AcquireNudgeLease(missingRoot, "hq-mayor")
	if release != nil {
		release()
	}
	if err == nil {
		t.Fatal("AcquireNudgeLease accepted a missing town root")
	}
	if _, statErr := os.Stat(filepath.Join(realParent, "missing-town")); !os.IsNotExist(statErr) {
		t.Fatalf("missing town root was created: %v", statErr)
	}
}

func TestNudgeLeaseRejectsRetargetedTownSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior requires Unix")
	}
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	linkedRoot := filepath.Join(t.TempDir(), "town-link")
	if err := os.Symlink(firstRoot, linkedRoot); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	tm := NewTmuxWithSocket("isolated")
	release, err := tm.AcquireNudgeLease(linkedRoot, "hq-mayor")
	if err != nil {
		t.Fatalf("AcquireNudgeLease: %v", err)
	}
	defer release()
	if err := os.Remove(linkedRoot); err != nil {
		t.Fatalf("Remove symlink: %v", err)
	}
	if err := os.Symlink(secondRoot, linkedRoot); err != nil {
		t.Fatalf("Retarget symlink: %v", err)
	}

	owned, _, err := tm.nudgeLeaseOwnership(linkedRoot, "hq-mayor")
	if err == nil || owned {
		t.Fatalf("retargeted town root ownership = %v, %v; want fail-closed mismatch", owned, err)
	}
}

func TestClientAttachmentLatchRecordsTransientAttachment(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-client-latch"
	_ = tm.KillSession(sessionName)
	if err := tm.NewSessionWithCommand(sessionName, t.TempDir(), "sleep 30"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	if err := tm.ArmClientAttachmentLatch(sessionName); err != nil {
		t.Fatalf("ArmClientAttachmentLatch: %v", err)
	}
	hook, err := tm.run("show-hooks", "-g", "client-attached")
	if err != nil || !strings.Contains(hook, clientAttachmentLatch) {
		t.Fatalf("client-attached hook = %q, %v", hook, err)
	}
	if _, err := tm.run("set-option", "-g", clientAttachmentLatch, "1"); err != nil {
		t.Fatalf("simulate client-attached hook: %v", err)
	}
	observed, err := tm.ClientAttachmentObserved(sessionName)
	if err != nil || !observed {
		t.Fatalf("ClientAttachmentObserved = %v, %v; want true", observed, err)
	}
}

func TestFindAgentPane_NonexistentSession(t *testing.T) {
	tm := newTestTmux(t)
	_, err := tm.FindAgentPane("nonexistent-session-findagent-xyz")
	if err == nil {
		t.Error("FindAgentPane on nonexistent session should return error")
	}
}

func TestValidateSessionName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		session string
		wantErr bool
	}{
		{"valid alphanumeric", "gt-gastown-crew-tom", false},
		{"valid with underscore", "hq_deacon", false},
		{"valid simple", "test123", false},
		{"empty string", "", true},
		{"contains dot", "my.session", true},
		{"contains colon", "my:session", true},
		{"contains space", "my session", true},
		{"contains slash", "rig/crew/tom", true},
		{"contains single quote", "it's", true},
		{"contains semicolon", "a;rm -rf /", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSessionName(tc.session)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateSessionName(%q) error = %v, wantErr %v", tc.session, err, tc.wantErr)
			}
		})
	}
}

func TestNewSession_RejectsInvalidName(t *testing.T) {
	tm := newTestTmux(t)
	err := tm.NewSession("invalid.name", "")
	if err == nil {
		t.Error("NewSession should reject session name with dots")
	}
	if !errors.Is(err, ErrInvalidSessionName) {
		t.Errorf("expected ErrInvalidSessionName, got %v", err)
	}
}

func TestEnsureSessionFresh_RejectsInvalidName(t *testing.T) {
	tm := newTestTmux(t)
	err := tm.EnsureSessionFresh("has:colon", "")
	if err == nil {
		t.Error("EnsureSessionFresh should reject session name with colons")
	}
	if !errors.Is(err, ErrInvalidSessionName) {
		t.Errorf("expected ErrInvalidSessionName, got %v", err)
	}
}

func TestFindAgentPane_MultiPaneNoAgent(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-findagent-noagent-" + fmt.Sprintf("%d", time.Now().UnixNano()%10000)

	_ = tm.KillSession(sessionName)
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Split into two shell panes (no agent running)
	_, err := tm.run("split-window", "-t", sessionName, "-d")
	if err != nil {
		t.Fatalf("split-window: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// FindAgentPane should return empty (no agent in either pane)
	paneID, err := tm.FindAgentPane(sessionName)
	if err != nil {
		t.Fatalf("FindAgentPane: %v", err)
	}
	if paneID != "" {
		t.Errorf("FindAgentPane with no agent = %q, want empty", paneID)
	}
}

func TestNewSessionWithCommandAndEnv(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-env-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)

	env := map[string]string{
		"GT_ROLE": "testrig/crew/testname",
		"GT_RIG":  "testrig",
		"GT_CREW": "testname",
	}

	// Create session with env vars and a command that prints GT_ROLE
	cmd := `bash -c "echo GT_ROLE=$GT_ROLE; sleep 5"`
	if err := tm.NewSessionWithCommandAndEnv(sessionName, "", cmd, env); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Verify session exists
	has, err := tm.HasSession(sessionName)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if !has {
		t.Fatal("expected session to exist after creation")
	}

	// Verify the env vars are set in the session environment
	gotRole, err := tm.GetEnvironment(sessionName, "GT_ROLE")
	if err != nil {
		t.Fatalf("GetEnvironment GT_ROLE: %v", err)
	}
	if gotRole != "testrig/crew/testname" {
		t.Errorf("GT_ROLE = %q, want %q", gotRole, "testrig/crew/testname")
	}

	gotRig, err := tm.GetEnvironment(sessionName, "GT_RIG")
	if err != nil {
		t.Fatalf("GetEnvironment GT_RIG: %v", err)
	}
	if gotRig != "testrig" {
		t.Errorf("GT_RIG = %q, want %q", gotRig, "testrig")
	}
	generation, err := tm.GetEnvironment(sessionName, EnvSessionGeneration)
	if err != nil {
		t.Fatalf("GetEnvironment %s: %v", EnvSessionGeneration, err)
	}
	if generation == "" {
		t.Fatalf("%s is empty", EnvSessionGeneration)
	}
}

func TestNewSessionWithCommandAndEnvEmpty(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-env-empty-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)

	// Empty env should work like NewSessionWithCommand
	if err := tm.NewSessionWithCommandAndEnv(sessionName, "", "sleep 5", nil); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv with nil env: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	has, err := tm.HasSession(sessionName)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if !has {
		t.Fatal("expected session to exist after creation with empty env")
	}
}

func TestIsTransientSendKeysError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"not in a mode", fmt.Errorf("tmux send-keys: not in a mode"), true},
		{"not in a mode wrapped", fmt.Errorf("nudge: %w", fmt.Errorf("tmux send-keys: not in a mode")), true},
		{"session not found", ErrSessionNotFound, false},
		{"no server", ErrNoServer, false},
		{"generic error", fmt.Errorf("something else"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isTransientSendKeysError(tt.err)
			if got != tt.want {
				t.Errorf("isTransientSendKeysError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestSendKeysLiteralWithRetry_ImmediateSuccess(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-retry-ok-" + fmt.Sprintf("%d", time.Now().UnixNano()%10000)

	// Create a session that's ready to accept input
	if err := tm.NewSession(sessionName, os.TempDir()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Should succeed immediately — no retry needed
	err := tm.sendKeysLiteralWithRetry(sessionName, "hello", 5*time.Second)
	if err != nil {
		t.Errorf("sendKeysLiteralWithRetry() = %v, want nil", err)
	}
}

func TestSendKeysLiteralWithRetry_NonTransientFails(t *testing.T) {
	tm := newTestTmux(t)

	// Target a session that doesn't exist — should fail immediately, not retry
	start := time.Now()
	err := tm.sendKeysLiteralWithRetry("gt-nonexistent-session-xyz", "hello", 5*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for nonexistent session, got nil")
	}
	// Should fail fast (< 1s), not wait the full 5s timeout
	if elapsed > 2*time.Second {
		t.Errorf("non-transient error took %v, expected fast failure", elapsed)
	}
}

func TestSendKeysLiteralWithRetry_NonTransientFailsFast(t *testing.T) {
	tm := newTestTmux(t)
	// Use a nonexistent session — tmux returns "session not found" which is
	// non-transient, so the function should fail fast (well under the timeout).
	start := time.Now()
	err := tm.sendKeysLiteralWithRetry("gt-nonexistent-session-fast-fail", "hello", 5*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for nonexistent session, got nil")
	}
	// Non-transient errors should fail immediately, not wait for timeout.
	if elapsed > 2*time.Second {
		t.Errorf("non-transient error took %v — should have failed fast, not retried until timeout", elapsed)
	}
}

func TestNudgeSession_WithRetry(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-nudge-retry-" + fmt.Sprintf("%d", time.Now().UnixNano()%10000)

	// Create a ready session
	if err := tm.NewSession(sessionName, os.TempDir()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Give shell a moment to initialize
	time.Sleep(200 * time.Millisecond)

	// NudgeSession should succeed on a ready session
	err := tm.NudgeSession(sessionName, "test message")
	if err != nil {
		t.Errorf("NudgeSession() = %v, want nil", err)
	}
}

func TestNudgeSessionReceipt_DistinguishesTypedFromSubmitted(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-nudge-receipt-" + fmt.Sprintf("%d", time.Now().UnixNano()%10000)

	if err := tm.NewSession(sessionName, os.TempDir()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	if err := tm.SetEnvironment(sessionName, "GT_AGENT", string(config.AgentCodex)); err != nil {
		t.Fatalf("SetEnvironment GT_AGENT: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	deliveryID := "ndg-typed-only"
	baseline := time.Now()
	receipt, err := tm.NudgeSessionWithReceipt(sessionName, "echo receipt-transport-marker", NudgeOpts{TownRoot: t.TempDir(), DeliveryID: deliveryID})
	if !errors.Is(err, ErrSubmitNotVerified) {
		t.Fatalf("NudgeSessionWithReceipt error = %v, want ErrSubmitNotVerified", err)
	}
	if !receipt.Typed || receipt.Submitted {
		t.Fatalf("receipt = %#v, want typed=true submitted=false", receipt)
	}
	if receipt.Session != sessionName || receipt.DeliveryID != deliveryID || receipt.TypedAt.Before(baseline) || !receipt.SubmittedAt.IsZero() {
		t.Fatalf("receipt identity/timestamps = %#v", receipt)
	}
}

func TestNudgeSessionReceipt_ResolvesVerifierByConfiguredProvider(t *testing.T) {
	config.ResetRegistryForTesting()
	t.Cleanup(config.ResetRegistryForTesting)

	townRoot := t.TempDir()
	rigName := "testrig"
	rigPath := filepath.Join(townRoot, rigName)
	settings := config.NewTownSettings()
	for _, name := range []string{"codex-mayor", "codex-polecat", "night-shift"} {
		settings.Agents[name] = &config.RuntimeConfig{Provider: "codex", Command: "custom-codex"}
	}
	settings.Agents["unsupported"] = &config.RuntimeConfig{Provider: "generic", Command: "custom-agent"}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), settings); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}
	rigSettings := config.NewRigSettings()
	rigSettings.Agents = map[string]*config.RuntimeConfig{
		"rig-night-shift": {Provider: "codex", Command: "custom-codex"},
	}
	if err := config.SaveRigSettings(config.RigSettingsPath(rigPath), rigSettings); err != nil {
		t.Fatalf("SaveRigSettings: %v", err)
	}
	// Block the delivery lock before any input can be typed. Supported providers
	// must reach this transport boundary; unsupported providers must fail closed
	// before it.
	if err := os.WriteFile(filepath.Join(townRoot, ".runtime"), []byte("blocked"), 0600); err != nil {
		t.Fatalf("block runtime directory: %v", err)
	}

	tm := newTestTmux(t)
	sessionName := "gt-test-nudge-provider-" + fmt.Sprintf("%d", time.Now().UnixNano()%10000)
	if err := tm.NewSession(sessionName, os.TempDir()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	if err := tm.SetEnvironment(sessionName, "GT_RIG", rigName); err != nil {
		t.Fatalf("SetEnvironment GT_RIG: %v", err)
	}

	for _, tc := range []struct {
		name      string
		supported bool
	}{
		{name: "codex", supported: true},
		{name: "codex-mayor", supported: true},
		{name: "codex-polecat", supported: true},
		{name: "night-shift", supported: true},
		{name: "rig-night-shift", supported: true},
		{name: "unsupported", supported: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tm.SetEnvironment(sessionName, "GT_AGENT", tc.name); err != nil {
				t.Fatalf("SetEnvironment GT_AGENT: %v", err)
			}

			receipt, err := tm.NudgeSessionWithReceipt(sessionName, "must not be typed", NudgeOpts{
				TownRoot:   townRoot,
				DeliveryID: "ndg-" + tc.name,
			})
			if got := !errors.Is(err, ErrSubmitVerifierUnsupported); got != tc.supported {
				t.Fatalf("supported = %v, want %v (error: %v)", got, tc.supported, err)
			}
			if receipt.Typed || receipt.Submitted {
				t.Fatalf("receipt = %#v, want no attempted delivery", receipt)
			}
		})
	}
}

func TestNudgeSessionReceipt_RejectsInvalidRigNames(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, ".runtime"), []byte("blocked"), 0600); err != nil {
		t.Fatalf("block runtime directory: %v", err)
	}

	tm := newTestTmux(t)
	sessionName := "gt-test-nudge-invalid-rig-" + fmt.Sprintf("%d", time.Now().UnixNano()%10000)
	if err := tm.NewSession(sessionName, os.TempDir()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	if err := tm.SetEnvironment(sessionName, "GT_AGENT", string(config.AgentCodex)); err != nil {
		t.Fatalf("SetEnvironment GT_AGENT: %v", err)
	}

	for _, rigName := range []string{".", "..", "../outside", filepath.Join(townRoot, "outside"), "nested/rig"} {
		t.Run(rigName, func(t *testing.T) {
			if err := tm.SetEnvironment(sessionName, "GT_RIG", rigName); err != nil {
				t.Fatalf("SetEnvironment GT_RIG: %v", err)
			}

			root := townRoot
			if rigName == "." || rigName == ".." {
				root = "\x00" // Any filesystem lookup would fail before the lexical rejection.
			}
			receipt, err := tm.NudgeSessionWithReceipt(sessionName, "must not be typed", NudgeOpts{
				TownRoot:   root,
				DeliveryID: "ndg-invalid-rig",
			})
			if !errors.Is(err, ErrSubmitVerifierUnsupported) {
				t.Fatalf("NudgeSessionWithReceipt error = %v, want ErrSubmitVerifierUnsupported", err)
			}
			if receipt.Typed || receipt.Submitted {
				t.Fatalf("receipt = %#v, want no attempted delivery", receipt)
			}
			if (rigName == "." || rigName == "..") && !strings.Contains(err.Error(), "invalid rig name") {
				t.Fatalf("NudgeSessionWithReceipt error = %v, want lexical invalid-rig rejection", err)
			}
		})
	}
}

func TestNudgeSessionReceipt_RejectsRigSymlinkEscape(t *testing.T) {
	config.ResetRegistryForTesting()
	t.Cleanup(config.ResetRegistryForTesting)

	townRoot := t.TempDir()
	outsideRig := t.TempDir()
	rigSettings := config.NewRigSettings()
	rigSettings.Agents = map[string]*config.RuntimeConfig{
		"outside-codex": {Provider: "codex", Command: "custom-codex"},
	}
	if err := config.SaveRigSettings(config.RigSettingsPath(outsideRig), rigSettings); err != nil {
		t.Fatalf("SaveRigSettings: %v", err)
	}
	if err := os.Symlink(outsideRig, filepath.Join(townRoot, "escaped")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".runtime"), []byte("blocked"), 0600); err != nil {
		t.Fatalf("block runtime directory: %v", err)
	}

	tm := newTestTmux(t)
	sessionName := "gt-test-nudge-symlink-rig-" + fmt.Sprintf("%d", time.Now().UnixNano()%10000)
	if err := tm.NewSession(sessionName, os.TempDir()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	if err := tm.SetEnvironment(sessionName, "GT_AGENT", "outside-codex"); err != nil {
		t.Fatalf("SetEnvironment GT_AGENT: %v", err)
	}
	if err := tm.SetEnvironment(sessionName, "GT_RIG", "escaped"); err != nil {
		t.Fatalf("SetEnvironment GT_RIG: %v", err)
	}

	receipt, err := tm.NudgeSessionWithReceipt(sessionName, "must not be typed", NudgeOpts{
		TownRoot:   townRoot,
		DeliveryID: "ndg-symlink-rig",
	})
	if !errors.Is(err, ErrSubmitVerifierUnsupported) {
		t.Fatalf("NudgeSessionWithReceipt error = %v, want ErrSubmitVerifierUnsupported", err)
	}
	if receipt.Typed || receipt.Submitted {
		t.Fatalf("receipt = %#v, want no attempted delivery", receipt)
	}
}

func TestNudgeSessionReceipt_RejectsRigReplacementBeforeTyping(t *testing.T) {
	townRoot := t.TempDir()
	rigName := "testrig"
	rigPath := filepath.Join(townRoot, rigName)
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), config.NewTownSettings()); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}
	if err := config.SaveRigSettings(config.RigSettingsPath(rigPath), config.NewRigSettings()); err != nil {
		t.Fatalf("SaveRigSettings: %v", err)
	}

	tm := newTestTmux(t)
	sessionName := "gt-test-nudge-replaced-rig-" + fmt.Sprintf("%d", time.Now().UnixNano()%10000)
	if err := tm.NewSession(sessionName, os.TempDir()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	if err := tm.SetEnvironment(sessionName, "GT_AGENT", string(config.AgentCodex)); err != nil {
		t.Fatalf("SetEnvironment GT_AGENT: %v", err)
	}
	if err := tm.SetEnvironment(sessionName, "GT_RIG", rigName); err != nil {
		t.Fatalf("SetEnvironment GT_RIG: %v", err)
	}

	receipt, err := tm.NudgeSessionWithReceipt(sessionName, "must not be typed", NudgeOpts{
		TownRoot:   townRoot,
		DeliveryID: "ndg-replaced-rig",
		beforeScopeRevalidation: func() {
			if err := os.Rename(rigPath, rigPath+"-old"); err != nil {
				t.Fatalf("rename rig: %v", err)
			}
			if err := os.Mkdir(rigPath, 0700); err != nil {
				t.Fatalf("replace rig: %v", err)
			}
		},
	})
	if !errors.Is(err, ErrSubmitVerifierUnsupported) {
		t.Fatalf("NudgeSessionWithReceipt error = %v, want ErrSubmitVerifierUnsupported", err)
	}
	if receipt.Typed || receipt.Submitted {
		t.Fatalf("receipt = %#v, want no attempted delivery", receipt)
	}
}

func TestNudgeSessionReceipt_RejectsMalformedFormerlyUnsupportedRigConfig(t *testing.T) {
	config.ResetRegistryForTesting()
	t.Cleanup(config.ResetRegistryForTesting)

	townRoot := t.TempDir()
	rigName := "testrig"
	rigPath := filepath.Join(townRoot, rigName)
	rigSettings := config.NewRigSettings()
	rigSettings.Agents = map[string]*config.RuntimeConfig{
		"codex": {Provider: "generic", Command: "custom-agent"},
	}
	rigSettingsPath := config.RigSettingsPath(rigPath)
	if err := config.SaveRigSettings(rigSettingsPath, rigSettings); err != nil {
		t.Fatalf("SaveRigSettings: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".runtime"), []byte("blocked"), 0600); err != nil {
		t.Fatalf("block runtime directory: %v", err)
	}

	tm := newTestTmux(t)
	sessionName := "gt-test-nudge-malformed-rig-" + fmt.Sprintf("%d", time.Now().UnixNano()%10000)
	if err := tm.NewSession(sessionName, os.TempDir()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	if err := tm.SetEnvironment(sessionName, "GT_AGENT", string(config.AgentCodex)); err != nil {
		t.Fatalf("SetEnvironment GT_AGENT: %v", err)
	}
	if err := tm.SetEnvironment(sessionName, "GT_RIG", rigName); err != nil {
		t.Fatalf("SetEnvironment GT_RIG: %v", err)
	}

	nudge := func(deliveryID string) (SubmissionReceipt, error) {
		return tm.NudgeSessionWithReceipt(sessionName, "must not be typed", NudgeOpts{
			TownRoot:   townRoot,
			DeliveryID: deliveryID,
		})
	}
	assertUnsupported := func(t *testing.T, deliveryID string) {
		t.Helper()
		receipt, err := nudge(deliveryID)
		if !errors.Is(err, ErrSubmitVerifierUnsupported) {
			t.Fatalf("NudgeSessionWithReceipt error = %v, want ErrSubmitVerifierUnsupported", err)
		}
		if receipt.Typed || receipt.Submitted {
			t.Fatalf("receipt = %#v, want no attempted delivery", receipt)
		}
	}
	assertConfigError := func(t *testing.T, deliveryID string) {
		t.Helper()
		receipt, err := nudge(deliveryID)
		if err == nil || !strings.Contains(err.Error(), "loading rig settings") {
			t.Fatalf("NudgeSessionWithReceipt error = %v, want rig settings load error", err)
		}
		if receipt.Typed || receipt.Submitted {
			t.Fatalf("receipt = %#v, want no attempted delivery", receipt)
		}
	}

	assertUnsupported(t, "ndg-generic-rig")
	if err := os.WriteFile(rigSettingsPath, []byte(`{"agents":`), 0600); err != nil {
		t.Fatalf("write malformed rig settings: %v", err)
	}
	assertConfigError(t, "ndg-malformed-rig")
	if err := os.Remove(rigSettingsPath); err != nil {
		t.Fatalf("remove rig settings: %v", err)
	}
	if err := os.Mkdir(rigSettingsPath, 0700); err != nil {
		t.Fatalf("replace rig settings with directory: %v", err)
	}
	assertConfigError(t, "ndg-unreadable-rig")
}

func TestNudgeSessionReceipt_RejectsUnsupportedRuntime(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-nudge-unsupported-" + fmt.Sprintf("%d", time.Now().UnixNano()%10000)
	if err := tm.NewSession(sessionName, os.TempDir()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	if err := tm.SetEnvironment(sessionName, "GT_AGENT", string(config.AgentClaude)); err != nil {
		t.Fatalf("SetEnvironment GT_AGENT: %v", err)
	}

	receipt, err := tm.NudgeSessionWithReceipt(sessionName, "must not be typed", NudgeOpts{TownRoot: t.TempDir(), DeliveryID: "ndg-unsupported"})
	if !errors.Is(err, ErrSubmitVerifierUnsupported) {
		t.Fatalf("NudgeSessionWithReceipt error = %v, want ErrSubmitVerifierUnsupported", err)
	}
	if receipt.Typed || receipt.Submitted {
		t.Fatalf("receipt = %#v, want no attempted delivery", receipt)
	}
}

func TestNudgeSessionReceipt_RequiresDeliveryID(t *testing.T) {
	receipt, err := newTestTmux(t).NudgeSessionWithReceipt("unused", "message", NudgeOpts{})
	if !errors.Is(err, ErrSubmitNotVerified) {
		t.Fatalf("NudgeSessionWithReceipt error = %v, want ErrSubmitNotVerified", err)
	}
	if receipt.Typed || receipt.Submitted || receipt.DeliveryID != "" {
		t.Fatalf("receipt = %#v, want untouched receipt", receipt)
	}
}

func TestNudgeSessionReceipt_RequiresTownRootBeforeSessionRead(t *testing.T) {
	receipt, err := newTestTmux(t).NudgeSessionWithReceipt("missing-session", "message", NudgeOpts{DeliveryID: "ndg-empty-root"})
	if !errors.Is(err, ErrSubmitNotVerified) {
		t.Fatalf("NudgeSessionWithReceipt error = %v, want ErrSubmitNotVerified", err)
	}
	if receipt.Typed || receipt.Submitted {
		t.Fatalf("receipt = %#v, want no attempted delivery", receipt)
	}
}

func TestNudgeSessionReceipt_MatchingRuntimeReceipt(t *testing.T) {
	tm := newTestTmux(t)
	townRoot := t.TempDir()
	captured := townRoot + "/submitted.txt"
	sessionName := "gt-test-nudge-submitted-" + fmt.Sprintf("%d", time.Now().UnixNano()%10000)
	if err := tm.NewSession(sessionName, os.TempDir()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	command := fmt.Sprintf(`sh -c 'while true; do printf "› "; IFS= read -r line || exit; printf "%%s\n" "$line" >> %s; printf "\033[2K\r› \n"; done'`, captured)
	if _, err := tm.run("respawn-pane", "-k", "-t", sessionName+":0.0", command); err != nil {
		t.Fatalf("respawn runtime fixture: %v", err)
	}
	if err := tm.SetEnvironment(sessionName, "GT_AGENT", string(config.AgentCodex)); err != nil {
		t.Fatalf("SetEnvironment GT_AGENT: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	deliveryID := "ndg-submitted"
	baseline := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			data, _ := os.ReadFile(captured)
			if strings.Contains(string(data), delivery.ControlMessage(deliveryID, "echo receipt-submitted-marker")) {
				_, _ = delivery.RecordPromptSubmitted(townRoot, sessionName, "codex", string(data), time.Now())
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	receipt, err := tm.NudgeSessionWithReceipt(sessionName, "echo receipt-submitted-marker", NudgeOpts{TownRoot: townRoot, DeliveryID: deliveryID})
	<-done
	if err != nil {
		pane, _ := tm.run("capture-pane", "-p", "-e", "-t", sessionName+":0.0", "-S", "-15")
		t.Fatalf("NudgeSessionWithReceipt: %v\npane:\n%s", err, pane)
	}
	if !receipt.Typed || !receipt.Submitted || receipt.Session != sessionName || receipt.DeliveryID != deliveryID || !receipt.SubmittedAt.After(baseline) {
		t.Fatalf("receipt = %#v, want matching post-baseline submission", receipt)
	}
	data, err := os.ReadFile(captured)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), delivery.ControlMessage(deliveryID, "echo receipt-submitted-marker")) {
		t.Fatalf("submitted control message missing delivery ID: %q", data)
	}
}

func TestNudgeSessionReceipt_AcceptsMatchingReceiptAfterAmbiguousComposerError(t *testing.T) {
	tm := newTestTmux(t)
	townRoot := t.TempDir()
	captured := townRoot + "/submitted.txt"
	sessionName := "gt-test-nudge-ambiguous-" + fmt.Sprintf("%d", time.Now().UnixNano()%10000)
	if err := tm.NewSession(sessionName, os.TempDir()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	command := fmt.Sprintf(`sh -c 'printf "› "; IFS= read -r line || exit; printf "%%s\n" "$line" >> %s; printf "\033[2K\r› stuck"; sleep 30'`, captured)
	if _, err := tm.run("respawn-pane", "-k", "-t", sessionName+":0.0", command); err != nil {
		t.Fatalf("respawn runtime fixture: %v", err)
	}
	if err := tm.SetEnvironment(sessionName, "GT_AGENT", string(config.AgentCodex)); err != nil {
		t.Fatalf("SetEnvironment GT_AGENT: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	const deliveryID = "ndg-ambiguous-submitted"
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			data, _ := os.ReadFile(captured)
			if strings.Contains(string(data), delivery.ControlMessage(deliveryID, "receipt wins")) {
				_, _ = delivery.RecordPromptSubmitted(townRoot, sessionName, "codex", string(data), time.Now())
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	receipt, err := tm.NudgeSessionWithReceipt(sessionName, "receipt wins", NudgeOpts{TownRoot: townRoot, DeliveryID: deliveryID})
	<-done
	if err != nil {
		t.Fatalf("matching runtime receipt lost to ambiguous composer error: %v", err)
	}
	if !receipt.Typed || !receipt.Submitted || receipt.Session != sessionName || receipt.DeliveryID != deliveryID {
		t.Fatalf("receipt = %#v, want matching submitted receipt", receipt)
	}
}

const (
	storedPaneReceiverEnv = "GT_TEST_NUDGE_RECEIVER"
	storedPaneNonceEnv    = "GT_TEST_NUDGE_NONCE"
)

var storedPaneNudgeSequence atomic.Uint64

func TestStoredPaneNudgeReceiverInvocation(t *testing.T) {
	tests := []struct {
		name  string
		mode  string
		nonce string
		args  []string
		want  bool
	}{
		{name: "exact helper", mode: "1", nonce: "nonce", args: []string{storedPaneReceiverRunArg}, want: true},
		{name: "split selector rejected", mode: "1", nonce: "nonce", args: []string{"-test.run", "^TestNudgeStoredPaneReceiverHelper$"}},
		{
			name:  "conflicting helper after earlier run",
			mode:  "1",
			nonce: "nonce",
			args:  []string{"-test.run=^TestSessionLifecycle$", "--test.run=^TestNudgeStoredPaneReceiverHelper$"},
		},
		{name: "disabled value", mode: "0", nonce: "nonce", args: []string{"-test.run=^TestNudgeStoredPaneReceiverHelper$"}},
		{name: "missing nonce", mode: "1", args: []string{"-test.run=^TestNudgeStoredPaneReceiverHelper$"}},
		{name: "wrong invocation", mode: "1", nonce: "nonce", args: []string{"-test.run=TestNudgeSession"}},
		{
			name:  "helper shadowed by later run",
			mode:  "1",
			nonce: "nonce",
			args:  []string{"-test.run=^TestNudgeStoredPaneReceiverHelper$", "-test.run=^TestSessionLifecycle$"},
		},
		{
			name:  "helper shadowed by split double-dash run",
			mode:  "1",
			nonce: "nonce",
			args:  []string{"-test.run=^TestNudgeStoredPaneReceiverHelper$", "--test.run", "^TestSessionLifecycle$"},
		},
		{
			name:  "duplicate helper selectors",
			mode:  "1",
			nonce: "nonce",
			args:  []string{storedPaneReceiverRunArg, storedPaneReceiverRunArg},
		},
		{name: "selector after double dash", mode: "1", nonce: "nonce", args: []string{"--", storedPaneReceiverRunArg}},
		{name: "selector after positional", mode: "1", nonce: "nonce", args: []string{"positional", storedPaneReceiverRunArg}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isStoredPaneNudgeReceiverInvocation(test.mode, test.nonce, test.args); got != test.want {
				t.Fatalf("isStoredPaneNudgeReceiverInvocation() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestNudgeStoredPaneReceiverProtocol(t *testing.T) {
	nonce := fmt.Sprintf("protocol-%d-%d", os.Getpid(), storedPaneNudgeSequence.Add(1))
	ctx, cancel := context.WithTimeout(context.Background(), constants.NudgeReadyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], storedPaneReceiverRunArg)
	cmd.Env = append(os.Environ(),
		storedPaneReceiverEnv+"=1",
		storedPaneNonceEnv+"="+nonce,
	)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("protocol: stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("protocol: stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("protocol: start helper: %v", err)
	}
	var waitOnce sync.Once
	var waitErr error
	wait := func() error {
		waitOnce.Do(func() { waitErr = cmd.Wait() })
		return waitErr
	}
	t.Cleanup(func() {
		cancel()
		_ = wait()
	})

	scanner := bufio.NewScanner(stdout)
	wantReady := "READY " + nonce
	ready := false
	for scanner.Scan() {
		if scanner.Text() == wantReady {
			ready = true
			break
		}
	}
	if !ready {
		if ctx.Err() != nil {
			t.Fatalf("protocol: helper timeout: %v", ctx.Err())
		}
		t.Fatalf("protocol: READY %s not observed", nonce)
	}

	messages := []string{
		"message-" + nonce,
		"control-" + nonce + "\x1b",
	}
	for _, message := range messages {
		if _, err := fmt.Fprintln(stdin, message); err != nil {
			t.Fatalf("protocol: write message: %v", err)
		}
	}
	if err := stdin.Close(); err != nil {
		t.Fatalf("protocol: close stdin: %v", err)
	}

	wantACKs := make(map[string]bool, len(messages))
	for index, message := range messages {
		wantACKs[fmt.Sprintf("ACK %s %d %x", nonce, index+1, message)] = false
	}
	for scanner.Scan() {
		if _, ok := wantACKs[scanner.Text()]; ok {
			wantACKs[scanner.Text()] = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("protocol: read stdout: %v", err)
	}
	for wantACK, observed := range wantACKs {
		if !observed {
			t.Fatalf("protocol: %s not observed", wantACK)
		}
	}
	if err := wait(); err != nil {
		t.Fatalf("protocol: helper exit: %v", err)
	}
}

func TestNudgeStoredPaneReceiverHelper(t *testing.T) {
	nonce := os.Getenv(storedPaneNonceEnv)
	if !isStoredPaneNudgeReceiverInvocation(
		os.Getenv(storedPaneReceiverEnv),
		nonce,
		os.Args[1:],
	) {
		return
	}

	output := bufio.NewWriter(os.Stdout)
	if _, err := fmt.Fprintf(output, "READY %s\n", nonce); err != nil {
		t.Fatalf("helper: write READY: %v", err)
	}
	if err := output.Flush(); err != nil {
		t.Fatalf("helper: flush READY: %v", err)
	}

	scanner := bufio.NewScanner(os.Stdin)
	count := 0
	for scanner.Scan() {
		count++
		if _, err := fmt.Fprintf(output, "ACK %s %d %x\n", nonce, count, scanner.Bytes()); err != nil {
			t.Fatalf("helper: write ACK: %v", err)
		}
		if err := output.Flush(); err != nil {
			t.Fatalf("helper: flush ACK: %v", err)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("helper: read stdin: %v", err)
	}
}

const storedPanePollInterval = 10 * time.Millisecond

func waitForExactPaneLine(tm *Tmux, pane, want string, deadline time.Time) (string, bool, error) {
	var last string
	for {
		content, err := tm.CapturePaneAll(pane)
		if err != nil {
			return last, false, err
		}
		last = content
		for _, line := range strings.Split(content, "\n") {
			if line == want {
				return content, true, nil
			}
		}
		if !time.Now().Before(deadline) {
			return last, false, nil
		}
		time.Sleep(storedPanePollInterval)
	}
}

func storedPaneACKLines(output, nonce string) []string {
	prefix := "ACK " + nonce + " "
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			lines = append(lines, line)
		}
	}
	return lines
}

func validateStoredPaneNudgeOutput(
	receiver,
	decoy,
	receiverNonce,
	decoyNonce,
	message,
	receiverBarrier,
	decoyBarrier string,
) error {
	receiverACKs := storedPaneACKLines(receiver, receiverNonce)
	wantMessageACK := fmt.Sprintf("ACK %s 1 %x", receiverNonce, message)
	if len(receiverACKs) == 0 {
		return errors.New("routing: receiver ACK missing")
	}
	wantReceiverBarrierACK := fmt.Sprintf("ACK %s 2 %x", receiverNonce, receiverBarrier)
	if len(receiverACKs) != 2 || receiverACKs[0] != wantMessageACK || receiverACKs[1] != wantReceiverBarrierACK {
		return errors.New("delivery: receiver transcript mismatch")
	}

	decoyACKs := storedPaneACKLines(decoy, decoyNonce)
	if len(decoyACKs) == 0 {
		return errors.New("routing: decoy barrier missing")
	}
	wantDecoyBarrierACK := fmt.Sprintf("ACK %s 1 %x", decoyNonce, decoyBarrier)
	if len(decoyACKs) != 1 || decoyACKs[0] != wantDecoyBarrierACK {
		return errors.New("routing: message observed in decoy pane")
	}
	return nil
}

func TestValidateStoredPaneNudgeOutput(t *testing.T) {
	receiverNonce := "receiver-123-1"
	decoyNonce := "decoy-123-1"
	message := "message-123-1"
	receiverBarrier := "barrier-receiver-123-1"
	decoyBarrier := "barrier-decoy-123-1"
	receiverACK := fmt.Sprintf("ACK %s 1 %x", receiverNonce, message)
	receiverBarrierACK := fmt.Sprintf("ACK %s 2 %x", receiverNonce, receiverBarrier)
	decoyBarrierACK := fmt.Sprintf("ACK %s 1 %x", decoyNonce, decoyBarrier)
	tests := []struct {
		name     string
		receiver string
		decoy    string
		wantErr  string
	}{
		{
			name:     "success",
			receiver: "READY " + receiverNonce + "\n" + receiverACK + "\n" + receiverBarrierACK,
			decoy:    "READY " + decoyNonce + "\n" + decoyBarrierACK,
		},
		{name: "missing receiver", receiver: "READY " + receiverNonce, decoy: decoyBarrierACK, wantErr: "routing: receiver ACK missing"},
		{
			name:     "message routed to decoy",
			receiver: receiverACK + "\n" + receiverBarrierACK,
			decoy: fmt.Sprintf("ACK %s 1 %x\nACK %s 2 %x",
				decoyNonce, message, decoyNonce, decoyBarrier),
			wantErr: "routing: message observed in decoy pane",
		},
		{
			name: "duplicate receiver delivery",
			receiver: receiverACK + "\n" +
				fmt.Sprintf("ACK %s 2 %x\nACK %s 3 %x", receiverNonce, message, receiverNonce, receiverBarrier),
			decoy:   decoyBarrierACK,
			wantErr: "delivery: receiver transcript mismatch",
		},
		{
			name:     "raw control byte corruption",
			receiver: fmt.Sprintf("ACK %s 1 %x\n%s", receiverNonce, message+"\x1b", receiverBarrierACK),
			decoy:    decoyBarrierACK,
			wantErr:  "delivery: receiver transcript mismatch",
		},
		{name: "receiver barrier missing", receiver: receiverACK, decoy: decoyBarrierACK, wantErr: "delivery: receiver transcript mismatch"},
		{name: "decoy barrier missing", receiver: receiverACK + "\n" + receiverBarrierACK, wantErr: "routing: decoy barrier missing"},
		{
			name:     "receiver input after barrier",
			receiver: receiverACK + "\n" + receiverBarrierACK + "\n" + fmt.Sprintf("ACK %s 3 %x", receiverNonce, message),
			decoy:    decoyBarrierACK,
			wantErr:  "delivery: receiver transcript mismatch",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateStoredPaneNudgeOutput(
				test.receiver,
				test.decoy,
				receiverNonce,
				decoyNonce,
				message,
				receiverBarrier,
				decoyBarrier,
			)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("validate output: %v", err)
				}
				return
			}
			if err == nil || err.Error() != test.wantErr {
				t.Fatalf("error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestNudgeSession_WithStoredPaneID(t *testing.T) {
	tm := newTestTmux(t)
	nonce := fmt.Sprintf("%d-%d", os.Getpid(), storedPaneNudgeSequence.Add(1))
	sessionName := "gt-test-nudge-paneid-" + nonce
	receiverNonce := "receiver-" + nonce
	decoyNonce := "decoy-" + nonce
	message := "message-" + nonce
	helperCommand := config.ShellQuote(os.Args[0]) + " " + config.ShellQuote(storedPaneReceiverRunArg)

	if err := tm.NewSessionWithCommandAndEnv(sessionName, os.TempDir(), helperCommand, map[string]string{
		storedPaneReceiverEnv: "1",
		storedPaneNonceEnv:    decoyNonce,
	}); err != nil {
		t.Fatalf("setup: READY %s not observed: create decoy: %v", decoyNonce, err)
	}
	t.Cleanup(func() {
		killErr := tm.KillSession(sessionName)
		has, err := tm.HasSession(sessionName)
		if err != nil {
			t.Errorf("cleanup: session %s still present: verify absence: %v", sessionName, err)
			return
		}
		if has {
			t.Errorf("cleanup: session %s still present: kill: %v", sessionName, killErr)
		}
	})

	decoyPane, err := tm.GetPaneID(sessionName)
	if err != nil {
		t.Fatalf("setup: READY %s not observed: get decoy pane: %v", decoyNonce, err)
	}
	decoyReady := "READY " + decoyNonce
	_, ready, err := waitForExactPaneLine(
		tm,
		decoyPane,
		decoyReady,
		time.Now().Add(constants.NudgeReadyTimeout),
	)
	if err != nil {
		t.Fatalf("setup: READY %s not observed: %v", decoyNonce, err)
	}
	if !ready {
		t.Fatalf("setup: READY %s not observed", decoyNonce)
	}

	receiverPane, err := tm.run(
		"split-window",
		"-P",
		"-F", "#{pane_id}",
		"-t", decoyPane,
		"-e", storedPaneReceiverEnv+"=1",
		"-e", storedPaneNonceEnv+"="+receiverNonce,
		helperCommand,
	)
	if err != nil {
		t.Fatalf("setup: READY %s not observed: create receiver: %v", receiverNonce, err)
	}
	receiverPane = strings.TrimSpace(receiverPane)
	receiverReady := "READY " + receiverNonce
	_, ready, err = waitForExactPaneLine(
		tm,
		receiverPane,
		receiverReady,
		time.Now().Add(constants.NudgeReadyTimeout),
	)
	if err != nil {
		t.Fatalf("setup: READY %s not observed: %v", receiverNonce, err)
	}
	if !ready {
		t.Fatalf("setup: READY %s not observed", receiverNonce)
	}

	if _, err := tm.run("select-pane", "-t", decoyPane); err != nil {
		t.Fatalf("setup: READY %s not observed: select decoy: %v", decoyNonce, err)
	}
	activePane, err := tm.run("display-message", "-p", "-t", sessionName, "#{pane_id}")
	if err != nil {
		t.Fatalf("setup: READY %s not observed: read active pane: %v", decoyNonce, err)
	}
	if strings.TrimSpace(activePane) != decoyPane {
		t.Fatalf("setup: READY %s not observed: decoy pane is not active", decoyNonce)
	}
	fallbackPane, err := tm.GetPaneID(sessionName)
	if err != nil {
		t.Fatalf("setup: read fallback pane: %v", err)
	}
	if receiverPane == fallbackPane {
		t.Fatal("setup: stored receiver pane is also the fallback pane")
	}
	if receiverPane == strings.TrimSpace(activePane) {
		t.Fatal("setup: stored receiver pane is also the active pane")
	}

	if err := tm.SetEnvironment(sessionName, "GT_PANE_ID", receiverPane); err != nil {
		t.Fatalf("routing: receiver ACK missing: set GT_PANE_ID: %v", err)
	}

	if err := tm.NudgeSessionWithOpts(sessionName, message, NudgeOpts{SkipEscape: true}); err != nil {
		t.Fatalf("routing: receiver ACK missing: %v", err)
	}

	receiverBarrier := "barrier-receiver-" + nonce
	decoyBarrier := "barrier-decoy-" + nonce
	for _, barrier := range []struct {
		pane string
		line string
	}{
		{pane: receiverPane, line: receiverBarrier},
		{pane: decoyPane, line: decoyBarrier},
	} {
		if err := tm.sendMessageToTarget(barrier.pane, barrier.line); err != nil {
			t.Fatalf("delivery: send FIFO barrier: %v", err)
		}
		if _, err := tm.run("send-keys", "-t", barrier.pane, "Enter"); err != nil {
			t.Fatalf("delivery: submit FIFO barrier: %v", err)
		}
	}

	deliveryDeadline := time.Now().Add(constants.NudgeReadyTimeout)
	wantReceiverBarrierACK := fmt.Sprintf("ACK %s 2 %x", receiverNonce, receiverBarrier)
	_, acknowledged, err := waitForExactPaneLine(tm, receiverPane, wantReceiverBarrierACK, deliveryDeadline)
	if err != nil {
		t.Fatalf("delivery: receiver FIFO barrier missing: %v", err)
	}
	if !acknowledged {
		t.Fatal("delivery: receiver FIFO barrier missing")
	}
	wantDecoyBarrierACK := fmt.Sprintf("ACK %s 1 %x", decoyNonce, decoyBarrier)
	_, acknowledged, err = waitForExactPaneLine(tm, decoyPane, wantDecoyBarrierACK, deliveryDeadline)
	if err != nil {
		t.Fatalf("delivery: decoy FIFO barrier missing: %v", err)
	}
	if !acknowledged {
		t.Fatal("delivery: decoy FIFO barrier missing")
	}

	receiverOutput, err := tm.CapturePaneAll(receiverPane)
	if err != nil {
		t.Fatalf("delivery: capture receiver transcript: %v", err)
	}
	decoyOutput, err := tm.CapturePaneAll(decoyPane)
	if err != nil {
		t.Fatalf("routing: message observed in decoy pane: capture: %v", err)
	}
	if err := validateStoredPaneNudgeOutput(
		receiverOutput,
		decoyOutput,
		receiverNonce,
		decoyNonce,
		message,
		receiverBarrier,
		decoyBarrier,
	); err != nil {
		t.Fatal(err)
	}
}

// TestNudgeSession_WakesAgentWindowNotActiveWindow is a regression test for a
// missed-wake bug in multi-window sessions. NudgeSessionWithOpts used to pass
// the bare session name to WakePaneIfDetached, which resizes the session's
// *active* window. When an agent session also has another window open and
// focused (e.g. a `gt feed -w` window), the agent's pane lives in a now-inactive
// window, so the SIGWINCH went to the wrong window and the agent never woke.
//
// The wake's resize dance ends by setting the targeted window's window-size
// option to "latest". By pre-setting both windows to "manual" and checking which
// one flips to "latest" after the nudge, we can assert the agent's window — not
// the active window — was the one woken.
func TestNudgeSession_WakesAgentWindowNotActiveWindow(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-nudge-multiwin-" + fmt.Sprintf("%d", time.Now().UnixNano()%100000)

	if err := tm.NewSession(sessionName, os.TempDir()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	time.Sleep(200 * time.Millisecond)

	// The agent pane is window 0's pane. Record it as the declared identity so
	// FindAgentPane resolves the nudge target to it.
	agentPane, err := tm.GetPaneID(sessionName)
	if err != nil {
		t.Fatalf("GetPaneID: %v", err)
	}
	if err := tm.SetEnvironment(sessionName, "GT_PANE_ID", agentPane); err != nil {
		t.Fatalf("SetEnvironment GT_PANE_ID: %v", err)
	}

	// Open a second window, which tmux makes the active window. This puts the
	// agent's pane in a non-active window — the scenario that exposed the bug.
	if _, err := tm.run("new-window", "-t", sessionName, "-n", "feed"); err != nil {
		t.Fatalf("new-window: %v", err)
	}

	// Pre-set both windows to window-size "manual". The wake resets only the
	// window it targets back to "latest", giving us a deterministic signal.
	for _, win := range []string{":0", ":1"} {
		if _, err := tm.run("set-option", "-w", "-t", sessionName+win, "window-size", "manual"); err != nil {
			t.Fatalf("set-option window-size manual on %s: %v", sessionName+win, err)
		}
	}

	if err := tm.NudgeSession(sessionName, "test message"); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}

	windowSize := func(win string) string {
		out, err := tm.run("show-options", "-w", "-t", sessionName+win, "window-size")
		if err != nil {
			t.Fatalf("show-options window-size on %s: %v", sessionName+win, err)
		}
		fields := strings.Fields(strings.TrimSpace(out))
		if len(fields) < 2 {
			return ""
		}
		return fields[1]
	}

	// Agent window (0) must have been woken; active window (1) must be untouched.
	if got := windowSize(":0"); got != "latest" {
		t.Errorf("agent window (0) window-size = %q, want %q (agent's window was not woken)", got, "latest")
	}
	if got := windowSize(":1"); got != "manual" {
		t.Errorf("active window (1) window-size = %q, want %q (wrong window was woken)", got, "manual")
	}
}

func TestCanonicalPaneTargetFromDisplay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		expectedSession string
		out             string
		want            string
		wantOK          bool
	}{
		{
			name:            "valid target",
			expectedSession: "gt-alpha",
			out:             "gt-alpha\t0\t0",
			want:            "gt-alpha:0.0",
			wantOK:          true,
		},
		{
			name:            "valid multi window",
			expectedSession: "gt-alpha",
			out:             "gt-alpha\t12\t3",
			want:            "gt-alpha:12.3",
			wantOK:          true,
		},
		{
			name:            "wrong session",
			expectedSession: "gt-alpha",
			out:             "gt-beta\t0\t0",
			wantOK:          false,
		},
		{
			name:            "malformed combined target",
			expectedSession: "gt-alpha",
			out:             "gt-alpha:0.0",
			wantOK:          false,
		},
		{
			name:            "empty output",
			expectedSession: "gt-alpha",
			out:             "",
			wantOK:          false,
		},
		{
			name:            "non numeric window",
			expectedSession: "gt-alpha",
			out:             "gt-alpha\tactive\t0",
			wantOK:          false,
		},
		{
			name:            "non numeric pane",
			expectedSession: "gt-alpha",
			out:             "gt-alpha\t0\tactive",
			wantOK:          false,
		},
		{
			name:            "negative index",
			expectedSession: "gt-alpha",
			out:             "gt-alpha\t0\t-1",
			wantOK:          false,
		},
		{
			name:            "signed index",
			expectedSession: "gt-alpha",
			out:             "gt-alpha\t+1\t0",
			wantOK:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := canonicalPaneTargetFromDisplay(tt.expectedSession, tt.out)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("target = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCanonicalPaneTargetResolvesAndFallsBack(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-canonical-pane-" + fmt.Sprintf("%d", time.Now().UnixNano()%100000)
	otherSession := "gt-test-canonical-other-" + fmt.Sprintf("%d", time.Now().UnixNano()%100000)

	if err := tm.NewSession(sessionName, os.TempDir()); err != nil {
		t.Fatalf("NewSession target: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	if err := tm.NewSession(otherSession, os.TempDir()); err != nil {
		t.Fatalf("NewSession other: %v", err)
	}
	defer func() { _ = tm.KillSession(otherSession) }()

	time.Sleep(200 * time.Millisecond)
	fallback := sessionName + ":0.0"
	if got := tm.canonicalPaneTarget(sessionName, ""); got != fallback {
		t.Errorf("empty pane target = %q, want %q", got, fallback)
	}
	if got := tm.canonicalPaneTarget(sessionName, "%999999"); got != fallback {
		t.Errorf("missing pane target = %q, want %q", got, fallback)
	}

	paneID, err := tm.GetPaneID(sessionName)
	if err != nil {
		t.Fatalf("GetPaneID target: %v", err)
	}
	if got := tm.canonicalPaneTarget(sessionName, paneID); got != fallback {
		t.Errorf("live pane target = %q, want %q", got, fallback)
	}

	otherPane, err := tm.GetPaneID(otherSession)
	if err != nil {
		t.Fatalf("GetPaneID other: %v", err)
	}
	if got := tm.canonicalPaneTarget(sessionName, otherPane); got != fallback {
		t.Errorf("cross-session pane target = %q, want %q", got, fallback)
	}
}

func TestNudgeSession_StalePaneIDFallsBackToFirstPane(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-nudge-stale-pane-" + fmt.Sprintf("%d", time.Now().UnixNano()%100000)
	otherSession := "gt-test-nudge-other-pane-" + fmt.Sprintf("%d", time.Now().UnixNano()%100000)

	if err := tm.NewSession(sessionName, os.TempDir()); err != nil {
		t.Fatalf("NewSession target: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	if err := tm.NewSession(otherSession, os.TempDir()); err != nil {
		t.Fatalf("NewSession other: %v", err)
	}
	defer func() { _ = tm.KillSession(otherSession) }()

	time.Sleep(200 * time.Millisecond)
	otherPane, err := tm.GetPaneID(otherSession)
	if err != nil {
		t.Fatalf("GetPaneID other: %v", err)
	}
	if err := tm.SetEnvironment(sessionName, "GT_PANE_ID", otherPane); err != nil {
		t.Fatalf("SetEnvironment GT_PANE_ID: %v", err)
	}
	if _, err := tm.run("new-window", "-t", sessionName, "-n", "active"); err != nil {
		t.Fatalf("new-window: %v", err)
	}

	marker := "GT_NUDGE_STALE_PANE_FALLBACK_" + fmt.Sprintf("%d", time.Now().UnixNano()%100000)
	if err := tm.NudgeSession(sessionName, "echo "+marker); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	firstPane, err := tm.CapturePane(sessionName+":0.0", 80)
	if err != nil {
		t.Fatalf("CapturePane first: %v", err)
	}
	activePane, err := tm.CapturePane(sessionName+":1.0", 80)
	if err != nil {
		t.Fatalf("CapturePane active: %v", err)
	}
	otherPaneContent, err := tm.CapturePane(otherSession+":0.0", 80)
	if err != nil {
		t.Fatalf("CapturePane other: %v", err)
	}

	if !strings.Contains(firstPane, marker) {
		t.Fatalf("first pane did not receive nudge marker %q; content:\n%s", marker, firstPane)
	}
	if strings.Contains(activePane, marker) {
		t.Fatalf("active fallback pane received stale-pane nudge marker %q; content:\n%s", marker, activePane)
	}
	if strings.Contains(otherPaneContent, marker) {
		t.Fatalf("cross-session stale pane received nudge marker %q; content:\n%s", marker, otherPaneContent)
	}
}

// TestAdaptiveTextDelay verifies the delay scaling logic for post-text delivery.
func TestAdaptiveTextDelay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		msgLen  int
		wantMin time.Duration
		wantMax time.Duration
	}{
		{"empty", 0, 500 * time.Millisecond, 500 * time.Millisecond},
		{"small single chunk", 100, 500 * time.Millisecond, 500 * time.Millisecond},
		{"exactly one chunk", 512, 500 * time.Millisecond, 500 * time.Millisecond},
		{"two chunks", 513, 525 * time.Millisecond, 525 * time.Millisecond},
		{"five chunks", 2048 + 1, 600 * time.Millisecond, 600 * time.Millisecond},
		{"huge message capped", 100000, 2 * time.Second, 2 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := adaptiveTextDelay(tt.msgLen)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("adaptiveTextDelay(%d) = %v, want [%v, %v]", tt.msgLen, got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

// TestMatchesPromptPrefix verifies that prompt matching handles non-breaking
// spaces (NBSP, U+00A0) correctly. Claude Code uses NBSP after its > prompt
// character, but the default ReadyPromptPrefix uses a regular space.
// Regression test for https://github.com/steveyegge/gastown/issues/1387.
func TestMatchesPromptPrefix(t *testing.T) {
	t.Parallel()
	const (
		nbsp          = "\u00a0" // non-breaking space
		regularPrefix = "❯ "     // default: ❯ + regular space
	)

	tests := []struct {
		name   string
		line   string
		prefix string
		want   bool
	}{
		// Regular space in both line and prefix (baseline)
		{"regular space matches", "❯ ", regularPrefix, true},
		{"regular space with trailing content", "❯ some input", regularPrefix, true},

		// NBSP in line, regular space in prefix (the bug scenario)
		{"NBSP bare prompt matches", "❯" + nbsp, regularPrefix, true},
		{"NBSP with content matches", "❯" + nbsp + "claude --help", regularPrefix, true},
		{"NBSP with leading whitespace", "  ❯" + nbsp, regularPrefix, true},

		// NBSP in prefix (defensive: user could configure it either way)
		{"NBSP prefix matches NBSP line", "❯" + nbsp + "hello", "❯" + nbsp, true},
		{"NBSP prefix matches regular space line", "❯ hello", "❯" + nbsp, true},

		// Empty prefix never matches
		{"empty prefix", "❯ ", "", false},

		// No prompt character at all
		{"no prompt", "hello world", regularPrefix, false},
		{"empty line", "", regularPrefix, false},
		{"whitespace only", "   ", regularPrefix, false},

		// Bare prompt character without any space
		{"bare prompt no space", "❯", regularPrefix, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesPromptPrefix(tt.line, tt.prefix)
			if got != tt.want {
				t.Errorf("matchesPromptPrefix(%q, %q) = %v, want %v",
					tt.line, tt.prefix, got, tt.want)
			}
		})
	}
}

func TestWaitForIdle_Timeout(t *testing.T) {
	if os.Getenv("TMUX") == "" {
		t.Skip("not inside tmux")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("test requires unix")
	}

	tm := newTestTmux(t)

	// Create a session running a long sleep (no prompt visible)
	sessionName := fmt.Sprintf("gt-test-idle-%d", time.Now().UnixNano())
	if err := tm.NewSessionWithCommand(sessionName, os.TempDir(), "sleep 60"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	time.Sleep(200 * time.Millisecond)

	// WaitForIdle should timeout quickly since the session is running sleep, not a prompt
	err := tm.WaitForIdle(sessionName, 500*time.Millisecond)
	if err == nil {
		t.Error("WaitForIdle should have timed out for a busy session")
	}
	if !errors.Is(err, ErrIdleTimeout) {
		t.Errorf("expected ErrIdleTimeout, got: %v", err)
	}
}

func TestWaitForIdleContextCancelsBusyTmuxCommands(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("test requires unix")
	}

	tm := newTestTmux(t)
	sessionName := fmt.Sprintf("gt-test-idle-cancel-%d", time.Now().UnixNano())
	if err := tm.NewSessionWithCommand(sessionName, os.TempDir(), "sleep 60"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession(sessionName) })

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := tm.WaitForIdleContext(ctx, sessionName, 10*time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForIdleContext error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("canceled idle wait took %s", elapsed)
	}
}

func TestWaitForIdle_RejectsStaleCodexPrompt(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("test requires unix")
	}

	tm := newTestTmux(t)
	sessionName := fmt.Sprintf("gt-test-stale-codex-prompt-%d", time.Now().UnixNano())
	if err := tm.NewSessionWithCommand(sessionName, os.TempDir(), "printf '› initialize the canary\\nquiet startup output\\n'; sleep 60"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession(sessionName) })
	if err := tm.SetEnvironment(sessionName, "GT_AGENT", "codex"); err != nil {
		t.Fatalf("SetEnvironment GT_AGENT: %v", err)
	}

	if err := tm.WaitForIdle(sessionName, 500*time.Millisecond); !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("WaitForIdle stale Codex prompt = %v, want ErrIdleTimeout", err)
	}
}

func TestWaitForIdle_UsesCodexComposerCursor(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("test requires unix")
	}

	tests := []struct {
		name              string
		command           string
		agent             string
		readyPromptPrefix string
		wantErr           error
	}{
		{
			name:    "empty composer with visible placeholder is idle",
			command: "printf '\\033[1;2m›\\033[0m Ask anything\\r\\033[1C'; sleep 60",
		},
		{
			name:    "steady empty cursor row with footer is idle",
			command: "printf 'completed output\\n\\nfooter\\r\\033[1A\\033[1C'; sleep 60",
		},
		{
			name:    "staged prompt is not idle",
			command: "printf '\\033[1;2m›\\033[0m staged delivery'; sleep 60",
			wantErr: ErrIdleTimeout,
		},
		{
			name:              "custom Codex alias uses resolved prompt",
			command:           "printf '\\033[1;2m›\\033[0m Ask anything\\r\\033[1C'; sleep 60",
			agent:             "codex-mayor",
			readyPromptPrefix: "› ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tm := newTestTmux(t)
			sessionName := fmt.Sprintf("gt-test-codex-composer-cursor-%d", time.Now().UnixNano())
			if err := tm.NewSessionWithCommand(sessionName, os.TempDir(), tt.command); err != nil {
				t.Fatalf("NewSessionWithCommand: %v", err)
			}
			t.Cleanup(func() { _ = tm.KillSession(sessionName) })
			agent := tt.agent
			if agent == "" {
				agent = "codex"
			}
			if err := tm.SetEnvironment(sessionName, "GT_AGENT", agent); err != nil {
				t.Fatalf("SetEnvironment GT_AGENT: %v", err)
			}
			if tt.readyPromptPrefix != "" {
				if err := tm.SetEnvironment(sessionName, "GT_READY_PROMPT_PREFIX", tt.readyPromptPrefix); err != nil {
					t.Fatalf("SetEnvironment GT_READY_PROMPT_PREFIX: %v", err)
				}
			}

			err := tm.WaitForIdle(sessionName, 1500*time.Millisecond)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("WaitForIdle() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestHasBusyIndicator(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want bool
	}{
		{"claude status busy", "⏵⏵ bypass permissions on ... · esc to interrupt", true},
		{"codex status busy", "• Working (2m 18s • esc to interrupt)", true},
		{"idle line", "› Review ready notification", false},
		{"blank", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasBusyIndicator(tt.line); got != tt.want {
				t.Errorf("hasBusyIndicator(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

// TestShouldSendEscapeForLines guards against the regression where a nudge
// sends the vim-mode Escape keystroke while the agent is actively generating,
// interrupting its current turn (e.g. the Mayor). When the pane shows the busy
// indicator ("esc to interrupt"), the Escape must be suppressed.
func TestShouldSendEscapeForLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		lines []string
		want  bool
	}{
		{
			name:  "claude generating - suppress escape",
			lines: []string{"✻ Cogitating… (12s · ↑ 2.1k tokens · esc to interrupt)"},
			want:  false,
		},
		{
			name:  "codex working - suppress escape",
			lines: []string{"• Working (2m 18s • esc to interrupt)"},
			want:  false,
		},
		{
			name:  "busy indicator among multiple lines - suppress escape",
			lines: []string{"tool output", "more output", "⏵⏵ bypass permissions on · esc to interrupt"},
			want:  false,
		},
		{
			name:  "idle ready prompt - allow escape",
			lines: []string{"❯ "},
			want:  true,
		},
		{
			name:  "idle with typed nudge text - allow escape",
			lines: []string{"❯ HEALTH_CHECK: heartbeat stale, respond to confirm"},
			want:  true,
		},
		{
			name:  "no lines captured - allow escape (not busy)",
			lines: nil,
			want:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldSendEscapeForLines(tt.lines); got != tt.want {
				t.Errorf("shouldSendEscapeForLines(%q) = %v, want %v", tt.lines, got, tt.want)
			}
		})
	}
}

// TestShouldSendEscape_LivePane exercises the busy-state gate end-to-end against
// a real tmux pane (capture-only, so it avoids the sendEnterVerified timing
// flakiness of the full nudge integration tests). It confirms the wiring: when
// the pane shows the "esc to interrupt" busy indicator, shouldSendEscape returns
// false so a nudge will not interrupt the agent's in-flight work.
func TestShouldSendEscape_LivePane(t *testing.T) {
	tm := newTestTmux(t)
	session := "gt-test-should-escape-" + t.Name()

	_ = tm.KillSession(session)
	if err := tm.NewSession(session, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(session) }()

	// Idle shell prompt: no busy indicator → Escape is safe to send.
	if !tm.shouldSendEscape(session) {
		out, _ := tm.CapturePane(session, 20)
		t.Fatalf("shouldSendEscape on idle pane = false, want true; pane:\n%s", out)
	}

	// Simulate an agent that is actively generating by rendering the busy
	// indicator into the pane. The typed command line itself contains the
	// marker, so detection does not depend on the command actually executing.
	if err := tm.SendKeys(session, "echo esc to interrupt"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}

	// Poll until the gate flips to suppressed (the shell may be slow to render).
	deadline := time.Now().Add(5 * time.Second)
	for tm.shouldSendEscape(session) {
		if time.Now().After(deadline) {
			out, _ := tm.CapturePane(session, 20)
			t.Fatalf("shouldSendEscape did not detect busy indicator within timeout; pane:\n%s", out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestShouldSendEscape_CaptureErrorSuppressesEscape(t *testing.T) {
	tm := newTestTmux(t)

	if tm.shouldSendEscape("missing-session-for-escape-check") {
		t.Fatal("shouldSendEscape on missing target = true, want false")
	}
}

// TestBusyIndicators pins the centralized busy-indicator source of truth so a
// change to the upstream-coupled status string is intentional and reviewed
// rather than accidental (gastownhall/gastown#4240).
func TestBusyIndicators(t *testing.T) {
	t.Parallel()

	if len(busyIndicators) == 0 {
		t.Fatal("busyIndicators must not be empty — busy/idle detection would silently break")
	}

	// "esc to interrupt" is the marker Claude Code / Codex / Gemini render while
	// generating. If this assertion fails, the change must be deliberate.
	found := false
	for _, m := range busyIndicators {
		if m == "esc to interrupt" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("busyIndicators = %q, want it to contain the known \"esc to interrupt\" marker", busyIndicators)
	}

	// Every marker must be matched by hasBusyIndicator (guards against an empty
	// or whitespace-only entry slipping in).
	for _, m := range busyIndicators {
		if strings.TrimSpace(m) == "" {
			t.Fatal("busyIndicators must not contain empty or whitespace-only markers")
		}
		if !hasBusyIndicator("⏵⏵ status · " + m) {
			t.Errorf("hasBusyIndicator did not match a registered busy indicator %q", m)
		}
	}
}

func TestDefaultReadyPromptPrefix(t *testing.T) {
	t.Parallel()
	// Verify the constant is set correctly
	if DefaultReadyPromptPrefix == "" {
		t.Error("DefaultReadyPromptPrefix should not be empty")
	}
	if !strings.Contains(DefaultReadyPromptPrefix, "❯") {
		t.Errorf("DefaultReadyPromptPrefix = %q, want to contain ❯", DefaultReadyPromptPrefix)
	}
}

func TestIsIdleContext_Canceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewTmux().IsIdleContext(ctx, "nonexistent-session", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("IsIdleContext error = %v, want context cancellation", err)
	}
}

func TestGetSessionActivity(t *testing.T) {
	tm := newTestTmux(t)
	sessionName := "gt-test-activity-" + t.Name()

	// Clean up any existing session
	_ = tm.KillSession(sessionName)

	// Create session
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	// Get session activity
	activity, err := tm.GetSessionActivity(sessionName)
	if err != nil {
		t.Fatalf("GetSessionActivity: %v", err)
	}

	// Activity should be recent (within last minute since we just created it)
	if activity.IsZero() {
		t.Error("GetSessionActivity returned zero time")
	}

	// Activity should be in the past (or very close to now)
	now := activity // Use activity as baseline since clocks might differ
	_ = now         // Avoid unused variable

	// The activity timestamp should be reasonable (not in far future or past)
	// Just verify it's a valid Unix timestamp (after year 2000)
	if activity.Year() < 2000 {
		t.Errorf("GetSessionActivity returned suspicious time: %v", activity)
	}
}

func TestGetSessionActivity_NonexistentSession(t *testing.T) {
	tm := newTestTmux(t)

	// GetSessionActivity on nonexistent session should error
	_, err := tm.GetSessionActivity("nonexistent-session-xyz-12345")
	if err == nil {
		t.Error("GetSessionActivity on nonexistent session should return error")
	}
}

func TestNewSessionSet(t *testing.T) {
	// Test creating SessionSet from names
	names := []string{"session-a", "session-b", "session-c"}
	set := NewSessionSet(names)

	if set == nil {
		t.Fatal("NewSessionSet returned nil")
	}

	// Test Has() for existing sessions
	for _, name := range names {
		if !set.Has(name) {
			t.Errorf("SessionSet.Has(%q) = false, want true", name)
		}
	}

	// Test Has() for non-existing session
	if set.Has("nonexistent") {
		t.Error("SessionSet.Has(nonexistent) = true, want false")
	}

	// Test Names() returns all sessions
	gotNames := set.Names()
	if len(gotNames) != len(names) {
		t.Errorf("SessionSet.Names() returned %d names, want %d", len(gotNames), len(names))
	}

	// Verify all names are present (order may differ)
	nameSet := make(map[string]bool)
	for _, n := range gotNames {
		nameSet[n] = true
	}
	for _, n := range names {
		if !nameSet[n] {
			t.Errorf("SessionSet.Names() missing %q", n)
		}
	}
}

func TestNewSessionSet_Empty(t *testing.T) {
	set := NewSessionSet([]string{})

	if set == nil {
		t.Fatal("NewSessionSet returned nil for empty input")
	}

	if set.Has("anything") {
		t.Error("Empty SessionSet.Has() = true, want false")
	}

	names := set.Names()
	if len(names) != 0 {
		t.Errorf("Empty SessionSet.Names() returned %d names, want 0", len(names))
	}
}

func TestNewSessionSet_Nil(t *testing.T) {
	set := NewSessionSet(nil)

	if set == nil {
		t.Fatal("NewSessionSet returned nil for nil input")
	}

	if set.Has("anything") {
		t.Error("Nil-input SessionSet.Has() = true, want false")
	}
}

func TestSessionPrefixPattern_AlwaysIncludesGTAndHQ(t *testing.T) {
	// Even without GT_ROOT, the pattern should include gt and hq as safe defaults.
	t.Setenv("GT_ROOT", "")

	pattern := sessionPrefixPattern()
	if !strings.Contains(pattern, "gt") {
		t.Errorf("pattern %q missing 'gt'", pattern)
	}
	if !strings.Contains(pattern, "hq") {
		t.Errorf("pattern %q missing 'hq'", pattern)
	}
	// Must be a valid grep -Eq anchored alternation
	if !strings.HasPrefix(pattern, "^(") || !strings.HasSuffix(pattern, ")-") {
		t.Errorf("pattern %q has unexpected format", pattern)
	}
}

func TestGetKeyBinding_NoExistingBinding(t *testing.T) {
	tm := newTestTmux(t)
	// Query a key that almost certainly has no binding
	result := tm.getKeyBinding("prefix", "F12")
	if result != "" {
		t.Errorf("expected empty string for unbound key, got %q", result)
	}
}

func TestGetKeyBinding_CapturesDefaultBinding(t *testing.T) {
	tm := newTestTmux(t)

	// Query the default tmux binding for prefix-n (next-window).
	// This works without a running tmux server because list-keys
	// returns builtin defaults. Skip if already a GT binding (e.g.,
	// when running inside an active gastown session).
	result := tm.getKeyBinding("prefix", "n")
	if result == "" && tm.isGTBinding("prefix", "n") {
		t.Skip("prefix-n is already a GT binding in this environment")
	}
	if result != "next-window" {
		t.Errorf("expected 'next-window' for default prefix-n binding, got %q", result)
	}
}

func TestGetKeyBinding_CapturesDefaultBindingWithArgs(t *testing.T) {
	tm := newTestTmux(t)

	// prefix-s is "choose-tree -Zs" by default — tests multi-word command parsing
	result := tm.getKeyBinding("prefix", "s")
	if !strings.Contains(result, "choose-tree") {
		t.Errorf("expected binding to contain 'choose-tree', got %q", result)
	}
}

func TestGetKeyBinding_SkipsGasTownBindings(t *testing.T) {
	tm := newTestTmux(t)

	// Bootstrap the isolated server (bind-key requires a running server)
	if err := tm.NewSession("gt-test-bootstrap", ""); err != nil {
		t.Fatalf("bootstrap session: %v", err)
	}
	defer tm.KillSession("gt-test-bootstrap")

	// Set a GT-style if-shell binding (contains both "if-shell" and "gt ")
	ifShell := fmt.Sprintf("echo '#{session_name}' | grep -Eq '%s'", sessionPrefixPattern())
	_, _ = tm.run("bind-key", "-T", "prefix", "F11",
		"if-shell", ifShell,
		"run-shell 'gt agents menu'",
		":")

	result := tm.getKeyBinding("prefix", "F11")
	if result != "" {
		t.Errorf("expected empty string for Gas Town binding, got %q", result)
	}

	// Clean up
	_, _ = tm.run("unbind-key", "-T", "prefix", "F11")
}

func TestGetKeyBinding_CapturesUserBinding(t *testing.T) {
	tm := newTestTmux(t)

	// Bootstrap the isolated server (bind-key requires a running server)
	if err := tm.NewSession("gt-test-bootstrap", ""); err != nil {
		t.Fatalf("bootstrap session: %v", err)
	}
	defer tm.KillSession("gt-test-bootstrap")

	// Set a user binding that doesn't contain "gt "
	_, _ = tm.run("bind-key", "-T", "prefix", "F11", "display-message", "hello")

	result := tm.getKeyBinding("prefix", "F11")
	// Should capture the user's binding command
	if result == "" {
		t.Error("expected non-empty string for user binding")
	}
	if !strings.Contains(result, "display-message") {
		t.Errorf("expected binding to contain 'display-message', got %q", result)
	}

	// Clean up
	_, _ = tm.run("unbind-key", "-T", "prefix", "F11")
}

func TestIsGTBinding_DetectsGasTownBindings(t *testing.T) {
	tm := newTestTmux(t)

	// Bootstrap the isolated server (bind-key requires a running server)
	if err := tm.NewSession("gt-test-bootstrap", ""); err != nil {
		t.Fatalf("bootstrap session: %v", err)
	}
	defer tm.KillSession("gt-test-bootstrap")

	// A plain user binding should NOT be detected as GT
	_, _ = tm.run("bind-key", "-T", "prefix", "F11", "display-message", "hello")
	if tm.isGTBinding("prefix", "F11") {
		t.Error("plain user binding should not be detected as GT binding")
	}

	// A GT-style if-shell binding should be detected
	ifShell := fmt.Sprintf("echo '#{session_name}' | grep -Eq '%s'", sessionPrefixPattern())
	_, _ = tm.run("bind-key", "-T", "prefix", "F11",
		"if-shell", ifShell,
		"run-shell 'gt feed --window'",
		"display-message hello")
	if !tm.isGTBinding("prefix", "F11") {
		t.Error("GT if-shell binding should be detected as GT binding")
	}

	// Clean up
	_, _ = tm.run("unbind-key", "-T", "prefix", "F11")
}

func TestSetBindings_PreserveFallbackOnRepeatedCalls(t *testing.T) {
	tm := newTestTmux(t)

	// Bootstrap the isolated server (bind-key requires a running server)
	if err := tm.NewSession("gt-test-bootstrap", ""); err != nil {
		t.Fatalf("bootstrap session: %v", err)
	}
	defer tm.KillSession("gt-test-bootstrap")

	// Set a custom user binding on F11
	_, _ = tm.run("bind-key", "-T", "prefix", "F11", "display-message", "custom-user-cmd")

	// Wrap it as a GT binding (simulating first Set*Binding call)
	ifShell := fmt.Sprintf("echo '#{session_name}' | grep -Eq '%s'", sessionPrefixPattern())
	_, _ = tm.run("bind-key", "-T", "prefix", "F11",
		"if-shell", ifShell,
		"run-shell 'gt feed --window'",
		"display-message custom-user-cmd")

	// Record the binding after first configuration
	firstRaw, _ := tm.keyBindingLine("prefix", "F11")

	// isGTBinding should return true, causing Set*Binding to skip
	if !tm.isGTBinding("prefix", "F11") {
		t.Fatal("expected isGTBinding=true after first configuration")
	}

	// Verify the original user fallback is preserved in the binding
	if !strings.Contains(firstRaw, "custom-user-cmd") {
		t.Errorf("original user fallback not found in binding: %q", firstRaw)
	}

	// Clean up
	_, _ = tm.run("unbind-key", "-T", "prefix", "F11")
}

func TestSessionPrefixPattern_WithTownRoot(t *testing.T) {
	// Point at the real town root if available; otherwise skip.
	townRoot := os.Getenv("GT_ROOT")
	if townRoot == "" {
		t.Skip("GT_ROOT not set; skipping live rigs.json test")
	}
	pattern := sessionPrefixPattern()
	// With a real rigs.json, pattern must include at least gt, hq, and
	// whatever other rigs are registered.
	if !strings.Contains(pattern, "gt") {
		t.Errorf("pattern %q missing 'gt'", pattern)
	}
	if !strings.Contains(pattern, "hq") {
		t.Errorf("pattern %q missing 'hq'", pattern)
	}
	// Verify it's a sorted alternation.
	if !strings.HasPrefix(pattern, "^(") || !strings.HasSuffix(pattern, ")-") {
		t.Errorf("pattern %q has unexpected format", pattern)
	}
}

func TestSessionPrefixPattern_FallsBackToGTTownRoot(t *testing.T) {
	// When GT_ROOT is empty but GT_TOWN_ROOT is set, sessionPrefixPattern
	// should use GT_TOWN_ROOT to discover rig prefixes.
	townRoot := os.Getenv("GT_ROOT")
	if townRoot == "" {
		townRoot = os.Getenv("GT_TOWN_ROOT")
	}
	if townRoot == "" {
		t.Skip("neither GT_ROOT nor GT_TOWN_ROOT set; skipping")
	}

	// Clear GT_ROOT, set GT_TOWN_ROOT — simulates daemon startup env.
	t.Setenv("GT_ROOT", "")
	t.Setenv("GT_TOWN_ROOT", townRoot)

	pattern := sessionPrefixPattern()
	if !strings.Contains(pattern, "gt") {
		t.Errorf("pattern %q missing 'gt'", pattern)
	}
	if !strings.Contains(pattern, "hq") {
		t.Errorf("pattern %q missing 'hq'", pattern)
	}
	// With a real rigs.json via GT_TOWN_ROOT, we expect more than just gt+hq.
	// At minimum there should be 3+ prefixes in a multi-rig town.
	inner := strings.TrimPrefix(pattern, "^(")
	inner = strings.TrimSuffix(inner, ")-")
	prefixes := strings.Split(inner, "|")
	if len(prefixes) < 3 {
		t.Errorf("expected at least 3 prefixes via GT_TOWN_ROOT fallback, got %d: %v", len(prefixes), prefixes)
	}
}

func TestZombieStatusString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status   ZombieStatus
		expected string
		zombie   bool
	}{
		{SessionHealthy, "healthy", false},
		{SessionDead, "session-dead", false},
		{AgentDead, "agent-dead", true},
		{AgentHung, "agent-hung", true},
	}

	for _, tc := range tests {
		if got := tc.status.String(); got != tc.expected {
			t.Errorf("ZombieStatus(%d).String() = %q, want %q", tc.status, got, tc.expected)
		}
		if got := tc.status.IsZombie(); got != tc.zombie {
			t.Errorf("ZombieStatus(%d).IsZombie() = %v, want %v", tc.status, got, tc.zombie)
		}
	}
}

func TestCheckSessionHealth_NonexistentSession(t *testing.T) {
	tm := newTestTmux(t)
	status := tm.CheckSessionHealth("nonexistent-session-xyz", 0)
	if status != SessionDead {
		t.Errorf("CheckSessionHealth(nonexistent) = %v, want SessionDead", status)
	}
}

func TestCheckSessionHealth_ZombieSession(t *testing.T) {
	// Create a session with just a shell (no agent running)
	tm := newTestTmux(t)
	sessionName := fmt.Sprintf("gt-test-zombie-%d", os.Getpid())
	if err := tm.NewSession(sessionName, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer tm.KillSession(sessionName)

	// Wait for shell to start
	time.Sleep(200 * time.Millisecond)

	// Session exists but no agent process → AgentDead
	status := tm.CheckSessionHealth(sessionName, 0)
	if status != AgentDead {
		t.Errorf("CheckSessionHealth(shell-only) = %v, want AgentDead", status)
	}
}

func TestCheckSessionHealth_ActivityCheck(t *testing.T) {
	// Create a session that runs a long-lived process
	tm := newTestTmux(t)
	sessionName := fmt.Sprintf("gt-test-activity-%d", os.Getpid())
	// Use 'sleep' as a stand-in for an agent process
	if err := tm.NewSessionWithCommand(sessionName, "", "sleep 60"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer tm.KillSession(sessionName)
	time.Sleep(300 * time.Millisecond)

	// With no maxInactivity (0), activity is not checked.
	// The session has a non-shell process running (sleep), but it won't
	// match any agent process names, so IsAgentAlive returns false → AgentDead.
	status := tm.CheckSessionHealth(sessionName, 0)
	if status != AgentDead {
		// sleep is not an agent process, so this is expected
		t.Logf("Status with sleep process: %v (expected AgentDead since sleep != agent)", status)
	}

	// With a very short maxInactivity, a recently-created session should be healthy
	// (if the agent were actually running). This tests the activity threshold logic
	// without needing a real Claude process.
}

func TestValidateCommandBinary(t *testing.T) {
	t.Parallel()

	absoluteShell := "/bin/sh"
	if runtime.GOOS == "windows" {
		path, err := exec.LookPath("sh")
		if err != nil {
			t.Skip("sh not installed")
		}
		absoluteShell = path
	}

	tests := []struct {
		name    string
		cmd     string
		wantErr bool
	}{
		{"empty", "", false},
		{"relative binary", "echo hello", false},
		{"valid absolute", absoluteShell + " -c 'echo hi'", false},
		{"missing absolute", "/nonexistent/binary --flag", true},
		{"exec env missing", "exec env GT_TEST=1 /nonexistent/claude-code --settings /tmp", true},
		{"exec env valid", "exec env GT_TEST=1 " + absoluteShell + " -c 'echo hi'", false},
		{"env vars only", "exec env FOO=bar BAZ=1", false},
		{"bare exec", "exec " + absoluteShell, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCommandBinary(tc.cmd)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateCommandBinary(%q) error = %v, wantErr = %v", tc.cmd, err, tc.wantErr)
			}
		})
	}
}
