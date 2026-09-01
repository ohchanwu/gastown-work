package convoy

import (
	"fmt"
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

func TestMain(m *testing.M) {
	// Start an ephemeral Dolt container for this package's tests.
	// setupTestStore sets BEADS_TEST_MODE=1, which causes the beads SDK
	// to create testdb_<hash> databases. By routing those to an isolated
	// container (via BEADS_DOLT_PORT), the databases are destroyed when the
	// container is terminated at cleanup — preventing orphan
	// accumulation in the shared production Dolt data dir.
	if err := testutil.EnsureDoltContainerForTestMain(); err != nil {
		fmt.Fprintf(os.Stderr, "convoy TestMain: setup failed: %v\n", err)
		os.Exit(testutil.DoltTestMainExitCode(0, err, nil))
	}

	code := m.Run()
	cleanupErr := testutil.TerminateDoltContainer()
	if cleanupErr != nil {
		fmt.Fprintf(os.Stderr, "convoy TestMain: cleanup failed: %v\n", cleanupErr)
	}
	os.Exit(testutil.DoltTestMainExitCode(code, nil, cleanupErr))
}
