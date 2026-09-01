package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/testutil"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "nudge-poller" {
		os.Exit(runTestNudgePoller(os.Args[2]))
	}

	// Start an ephemeral Dolt container for this package's tests.
	// convoy_manager_test.go calls setupTestStore which sets BEADS_TEST_MODE=1,
	// causing the beads SDK to create testdb_<hash> databases. By routing
	// those to an isolated container (via BEADS_DOLT_PORT), the databases are
	// destroyed when the container is terminated at cleanup —
	// preventing orphan accumulation in the shared production Dolt data dir.
	//
	// When Docker is unavailable, Dolt-needing tests self-skip via
	// setupTestStore → beadsdk.Open failure. Non-Dolt tests still run, but the
	// package remains failed because setup was incomplete. (fixes gt-kw4449)
	setupErr := testutil.EnsureDoltContainerForTestMain()
	if setupErr != nil {
		fmt.Fprintf(os.Stderr, "daemon TestMain: Dolt container unavailable (%v), Dolt-dependent tests will skip\n", setupErr)
	}

	// Isolate tmux sessions on a package-specific socket.
	// handler_test.go creates tmux.NewTmux() instances that query has-session;
	// polecat_health_test.go uses fake tmux stubs but still constructs Tmux
	// instances. Routing all of these to an isolated socket prevents
	// interference with the user's tmux and other packages' tests.
	var tmuxSocket string
	if _, err := exec.LookPath("tmux"); err == nil {
		tmuxSocket = fmt.Sprintf("gt-test-daemon-%d", os.Getpid())
		tmux.SetDefaultSocket(tmuxSocket)
	}

	code := m.Run()

	if tmuxSocket != "" {
		_ = exec.Command("tmux", "-L", tmuxSocket, "kill-server").Run()
		socketPath := filepath.Join(tmux.SocketDir(), tmuxSocket)
		_ = os.Remove(socketPath)
	}
	cleanupErr := testutil.TerminateDoltContainer()
	if cleanupErr != nil {
		fmt.Fprintf(os.Stderr, "daemon TestMain: Dolt cleanup failed: %v\n", cleanupErr)
	}
	os.Exit(testutil.DoltTestMainExitCode(code, setupErr, cleanupErr))
}

func runTestNudgePoller(session string) int {
	townRoot := os.Getenv("GT_TOWN_ROOT")
	if townRoot == "" || session == "" {
		return 2
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if nudge.StopRequested(townRoot, session) {
			return 0
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 2
}
