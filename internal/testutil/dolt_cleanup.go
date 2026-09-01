package testutil

import (
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

type cleanupTB interface {
	Helper()
	Cleanup(func())
	Errorf(string, ...any)
	Logf(string, ...any)
}

// ReapOwnedDoltOnCleanup registers test cleanup for Dolt servers whose metadata
// and process args prove they belong to townRoot. It never kills by broad name or
// port, so production Dolt is protected when tests run inside a real workspace.
func ReapOwnedDoltOnCleanup(t testing.TB, townRoot string) {
	reapOwnedDoltOnCleanup(t, townRoot, doltserver.ReapOwnedTestServers)
}

func reapOwnedDoltOnCleanup(t cleanupTB, townRoot string, reap func(string) (int, error)) {
	t.Helper()
	t.Cleanup(func() {
		stopped, err := reap(townRoot)
		if err != nil {
			t.Errorf("owned Dolt cleanup failed: %v", err)
			return
		}
		if stopped > 0 {
			t.Logf("stopped %d owned Dolt sql-server process(es)", stopped)
		}
	})
}

// DoltTestMainExitCode makes shared Dolt setup or teardown failure fail the package.
func DoltTestMainExitCode(testCode int, setupErr, cleanupErr error) int {
	if testCode != 0 {
		return testCode
	}
	if setupErr != nil || cleanupErr != nil {
		return 1
	}
	return testCode
}
