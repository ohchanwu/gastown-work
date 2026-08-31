package cmd

import (
	"os"
	"runtime"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/steveyegge/gastown/internal/beads"
)

func TestResumeMoleculeCleanupPreservesRecoveredFormulaRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("real Dolt fixture is not supported on Windows")
	}
	port, err := strconv.Atoi(os.Getenv("GT_DOLT_PORT"))
	if err != nil || os.Getenv("GT_TEST_ISOLATED") != "1" {
		t.Skip("isolated Dolt listener is required")
	}
	bd := beads.NewIsolatedWithPort(t.TempDir(), port)
	if err := bd.Init("gt"); err != nil {
		t.Fatal(err)
	}
	pinned, err := bd.Create(beads.CreateOptions{Title: "work", Type: "task", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := bd.Create(beads.CreateOptions{Title: "stale molecule", Type: "molecule", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	staleChild, err := bd.Create(beads.CreateOptions{Title: "stale step", Type: "task", Priority: 2, Parent: stale.ID})
	if err != nil {
		t.Fatal(err)
	}
	preserved, err := bd.Create(beads.CreateOptions{Title: "recovered molecule", Type: "molecule", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, rootID := range []string{stale.ID, preserved.ID} {
		if err := bd.AddDependency(rootID, pinned.ID); err != nil {
			t.Fatal(err)
		}
	}
	cleanup, keep, err := bd.CaptureMoleculeCleanupReceiptsPreserving(pinned.ID, []string{stale.ID}, []string{preserved.ID})
	if err != nil {
		t.Fatal(err)
	}
	expectedMolecule := ""
	if _, err := bd.DetachMoleculeWithAudit(pinned.ID, beads.DetachOptions{
		Operation: "burn", Agent: "test", Reason: "test crash before cleanup",
		ExpectedMolecule: &expectedMolecule, ExpectedAssignee: &pinned.Assignee,
		ExpectedStatus: &pinned.Status, ExpectedDescription: &pinned.Description,
		AttemptID: uuid.NewString(), CleanupMolecules: cleanup, PreservedMolecules: keep,
		CleanupCloseReason: "burned",
	}); err != nil {
		t.Fatal(err)
	}
	resumed, closed, err := resumeMoleculeCleanupIfPresent(bd, pinned.ID, "burn", "burned")
	if err != nil || !resumed || closed != 1 {
		t.Fatalf("cleanup resume = resumed %v closed %d err %v", resumed, closed, err)
	}
	for _, id := range []string{stale.ID, staleChild.ID} {
		issue, err := bd.Show(id)
		if err != nil || issue.Status != string(beads.StatusClosed) {
			t.Fatalf("cleaned %s = (%+v, %v), want closed", id, issue, err)
		}
	}
	if issue, err := bd.Show(preserved.ID); err != nil || issue.Status == string(beads.StatusClosed) {
		t.Fatalf("preserved root = (%+v, %v), want open", issue, err)
	}
	if err := bd.VerifyMoleculeRootBond(pinned.ID, preserved.ID); err != nil {
		t.Fatalf("preserved ownership bond: %v", err)
	}
}
