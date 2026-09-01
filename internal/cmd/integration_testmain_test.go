//go:build integration

package cmd

import (
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

func TestMain(m *testing.M) {
	// Force sequential test execution to avoid bd file locks on Windows.
	_ = flag.Set("test.parallel", "1")
	flag.Parse()

	// Start an ephemeral Dolt container for this package's integration tests.
	// Tests like TestAgentWorktreesStayClean and TestBeadsRoutingFromTownRoot
	// spawn gt/bd subprocesses that create databases (e.g., "tr", "hq").
	// By routing to an isolated container (via GT_DOLT_PORT), those databases
	// are destroyed when the container is terminated at cleanup —
	// preventing orphan accumulation in the shared production Dolt data dir.
	if err := testutil.EnsureDoltContainerForTestMain(); err != nil {
		fmt.Fprintf(os.Stderr, "integration TestMain: dolt setup: %v\n", err)
		os.Exit(testutil.DoltTestMainExitCode(0, err, nil))
	}

	code := m.Run()

	cleanupErr := testutil.TerminateDoltContainer()
	if cleanupErr != nil {
		fmt.Fprintf(os.Stderr, "integration TestMain: dolt cleanup: %v\n", cleanupErr)
	}
	os.Exit(testutil.DoltTestMainExitCode(code, nil, cleanupErr))
}
