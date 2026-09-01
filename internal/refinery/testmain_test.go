package refinery

import (
	"fmt"
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

func TestMain(m *testing.M) {
	code := m.Run()
	cleanupErr := testutil.TerminateDoltContainer()
	if cleanupErr != nil {
		fmt.Fprintf(os.Stderr, "refinery TestMain: Dolt cleanup failed: %v\n", cleanupErr)
	}
	os.Exit(testutil.DoltTestMainExitCode(code, nil, cleanupErr))
}
