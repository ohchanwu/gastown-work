package beads

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func isolatedMoleculeCleanupBeads(t *testing.T) *Beads {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("real Dolt fixture is not supported on Windows")
	}
	portText := strings.TrimSpace(os.Getenv("GT_TEST_DOLT_PORT"))
	if portText == "" && os.Getenv("GT_TEST_ISOLATED") == "1" {
		portText = strings.TrimSpace(os.Getenv("GT_DOLT_PORT"))
	}
	port, err := strconv.Atoi(portText)
	if portText == "" || err != nil || port < 1 || port > 65535 {
		t.Skipf("GT_TEST_DOLT_PORT is absent or invalid: %q", portText)
	}
	bd := NewIsolatedWithPort(t.TempDir(), port)
	if err := bd.Init("gt"); err != nil {
		t.Fatalf("bd init: %v", err)
	}
	return bd
}

func createMoleculeCleanupFixture(t *testing.T, bd *Beads) (pinned, root, child *Issue) {
	t.Helper()
	var err error
	pinned, err = bd.Create(CreateOptions{Title: "work", Type: "task", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	root, err = bd.Create(CreateOptions{Title: "molecule", Type: "molecule", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	child, err = bd.Create(CreateOptions{Title: "step", Type: "task", Priority: 2, Parent: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := bd.AddDependency(root.ID, pinned.ID); err != nil {
		t.Fatal(err)
	}
	return pinned, root, child
}

func addMoleculeCleanupDependency(t *testing.T, bd *Beads, from, to, dependencyType string) {
	t.Helper()
	err := mutateAndDoltCommit(bd.getResolvedBeadsDir(), "test molecule cleanup dependency "+uuid.NewString(), func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO dependencies
			(id, issue_id, depends_on_issue_id, type, created_by)
			VALUES (?, ?, ?, ?, 'test')`, uuid.NewString(), from, to, dependencyType)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func recordMoleculeCleanupAttempt(t *testing.T, bd *Beads, pinned *Issue, receipts []MoleculeCleanupReceipt) string {
	t.Helper()
	db, err := openDoltSQL(bd.getResolvedBeadsDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var updatedAt time.Time
	if err := db.QueryRow("SELECT updated_at FROM issues WHERE id = ?", pinned.ID).Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	attemptID := uuid.NewString()
	if err := bd.LogDetachAudit(DetachAuditEntry{
		Operation: "burn", PinnedBeadID: pinned.ID, AttemptID: attemptID, State: "committed",
		CommitMessage: "test cleanup " + attemptID, ResultingStatus: pinned.Status,
		ResultingAssignee: pinned.Assignee, ResultingDescription: pinned.Description, ResultingGeneration: cleanupGeneration(updatedAt),
		CleanupMolecules: receipts, CleanupCloseReason: "burned",
	}); err != nil {
		t.Fatal(err)
	}
	return attemptID
}

func recordLiteralVersion1MoleculeCleanupAttempt(t *testing.T, bd *Beads, pinned *Issue, receipts []MoleculeCleanupReceipt) string {
	t.Helper()
	db, err := openDoltSQL(bd.getResolvedBeadsDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var oid string
	if err := db.QueryRow("SELECT commit_hash FROM dolt_log ORDER BY date DESC LIMIT 1").Scan(&oid); err != nil {
		t.Fatal(err)
	}
	attemptID := uuid.NewString()
	data, err := json.Marshal(map[string]any{
		"operation":             "burn",
		"pinned_bead_id":        pinned.ID,
		"attempt_id":            attemptID,
		"state":                 "committed",
		"commit_message":        "historical v1 cleanup " + attemptID,
		"commit_oid":            oid,
		"resulting_status":      pinned.Status,
		"resulting_assignee":    pinned.Assignee,
		"resulting_description": pinned.Description,
		"cleanup_molecules":     receipts,
		"cleanup_close_reason":  "burned",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "resulting_generation") {
		t.Fatal("literal v1 fixture unexpectedly contains resulting_generation")
	}
	f, err := os.OpenFile(filepath.Join(bd.getResolvedBeadsDir(), "audit.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return attemptID
}

func legacyV2MoleculeCleanupReceipts(t *testing.T, bd *Beads, receipts []MoleculeCleanupReceipt) []MoleculeCleanupReceipt {
	t.Helper()
	db, err := openDoltSQL(bd.getResolvedBeadsDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	legacy := make([]MoleculeCleanupReceipt, len(receipts))
	for i, receipt := range receipts {
		legacy[i] = receipt
		legacy[i].GraphVersion = 2
		legacy[i].RootShapeHash = ""
		legacy[i].Members = append([]MoleculeCleanupIssueReceipt(nil), receipt.Members...)
		members := make(map[string]bool, len(receipt.Members))
		for j := range legacy[i].Members {
			member := &legacy[i].Members[j]
			members[member.ID] = true
			snapshot, err := readCleanupIssueSnapshot(context.Background(), conn, member.ID)
			if err != nil {
				t.Fatal(err)
			}
			member.SnapshotHash = cleanupHash(member.Status, member.ContentHash, snapshot.updatedAt.UTC().Format(time.RFC3339Nano), snapshot.closeReason)
		}
		legacy[i].Ownership = nil
		for _, edge := range receipt.Ownership {
			if members[edge.To] {
				legacy[i].Ownership = append(legacy[i].Ownership, edge)
			}
		}
	}
	return legacy
}

func legacyV1MoleculeCleanupReceipts(t *testing.T, bd *Beads, receipts []MoleculeCleanupReceipt) []MoleculeCleanupReceipt {
	t.Helper()
	legacy := legacyV2MoleculeCleanupReceipts(t, bd, receipts)
	for i := range legacy {
		legacy[i].GraphVersion = 1
		legacy[i].Ownership = nil
	}
	return legacy
}

func TestApplyMoleculeCleanupReceiptsClosesExactGraphAtomically(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	pinned, root, child := createMoleculeCleanupFixture(t, bd)
	receipts, err := bd.CaptureMoleculeCleanupReceipts(pinned.ID, []string{root.ID})
	if err != nil {
		t.Fatal(err)
	}
	attemptID := recordMoleculeCleanupAttempt(t, bd, pinned, receipts)
	closed, err := bd.ApplyMoleculeCleanupReceipts(attemptID, "burned")
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Fatalf("children closed = %d, want 1", closed)
	}
	for _, id := range []string{root.ID, child.ID} {
		issue, err := bd.Show(id)
		if err != nil {
			t.Fatal(err)
		}
		if issue.Status != string(StatusClosed) {
			t.Fatalf("%s status = %q, want closed", id, issue.Status)
		}
	}
	if closed, err := bd.ApplyMoleculeCleanupReceipts(attemptID, "burned"); err != nil || closed != 0 {
		t.Fatalf("idempotent cleanup = (%d, %v), want (0, nil)", closed, err)
	}
}

func TestApplyMoleculeCleanupReceiptsReplaysVersion2Journal(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	pinned, root, child := createMoleculeCleanupFixture(t, bd)
	receipts, err := bd.CaptureMoleculeCleanupReceipts(pinned.ID, []string{root.ID})
	if err != nil {
		t.Fatal(err)
	}
	attemptID := recordMoleculeCleanupAttempt(t, bd, pinned, legacyV2MoleculeCleanupReceipts(t, bd, receipts))
	closed, err := bd.ApplyMoleculeCleanupReceipts(attemptID, "burned")
	if err != nil || closed != 1 {
		t.Fatalf("version 2 cleanup replay = (%d, %v), want (1, nil)", closed, err)
	}
	for _, id := range []string{root.ID, child.ID} {
		issue, err := bd.Show(id)
		if err != nil || issue.Status != string(StatusClosed) {
			t.Fatalf("version 2 cleaned %s = (%+v, %v), want closed", id, issue, err)
		}
	}
	if closed, err := bd.ApplyMoleculeCleanupReceipts(attemptID, "burned"); err != nil || closed != 0 {
		t.Fatalf("version 2 idempotent replay = (%d, %v), want (0, nil)", closed, err)
	}
}

func TestApplyMoleculeCleanupReceiptsReplaysVersion1Journal(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	pinned, root, child := createMoleculeCleanupFixture(t, bd)
	receipts, err := bd.CaptureMoleculeCleanupReceipts(pinned.ID, []string{root.ID})
	if err != nil {
		t.Fatal(err)
	}
	attemptID := recordLiteralVersion1MoleculeCleanupAttempt(t, bd, pinned, legacyV1MoleculeCleanupReceipts(t, bd, receipts))
	closed, err := bd.ApplyMoleculeCleanupReceipts(attemptID, "burned")
	if err != nil || closed != 1 {
		t.Fatalf("version 1 cleanup replay = (%d, %v), want (1, nil)", closed, err)
	}
	for _, id := range []string{root.ID, child.ID} {
		issue, err := bd.Show(id)
		if err != nil || issue.Status != string(StatusClosed) {
			t.Fatalf("version 1 cleaned %s = (%+v, %v), want closed", id, issue, err)
		}
	}
}

func TestApplyMoleculeCleanupReceiptsRecognizesVersion1LostAcknowledgement(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	pinned, root, _ := createMoleculeCleanupFixture(t, bd)
	receipts, err := bd.CaptureMoleculeCleanupReceipts(pinned.ID, []string{root.ID})
	if err != nil {
		t.Fatal(err)
	}
	legacyAttemptID := recordLiteralVersion1MoleculeCleanupAttempt(t, bd, pinned, legacyV1MoleculeCleanupReceipts(t, bd, receipts))
	currentAttemptID := recordMoleculeCleanupAttempt(t, bd, pinned, receipts)
	if _, err := bd.ApplyMoleculeCleanupReceipts(currentAttemptID, "burned"); err != nil {
		t.Fatal(err)
	}
	if closed, err := bd.ApplyMoleculeCleanupReceipts(legacyAttemptID, "burned"); err != nil || closed != 0 {
		t.Fatalf("version 1 lost-ACK replay = (%d, %v), want (0, nil)", closed, err)
	}
}

func TestApplyMoleculeCleanupReceiptsRejectsVersion1SnapshotABA(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	pinned, root, child := createMoleculeCleanupFixture(t, bd)
	receipts, err := bd.CaptureMoleculeCleanupReceipts(pinned.ID, []string{root.ID})
	if err != nil {
		t.Fatal(err)
	}
	attemptID := recordLiteralVersion1MoleculeCleanupAttempt(t, bd, pinned, legacyV1MoleculeCleanupReceipts(t, bd, receipts))
	drifted := "temporary drift"
	if err := bd.Update(pinned.ID, UpdateOptions{Description: &drifted}); err != nil {
		t.Fatal(err)
	}
	if err := bd.Update(pinned.ID, UpdateOptions{Description: &pinned.Description}); err != nil {
		t.Fatal(err)
	}
	if closed, err := bd.ApplyMoleculeCleanupReceipts(attemptID, "burned"); err == nil {
		t.Fatalf("version 1 ABA cleanup closed %d children without exact generation proof", closed)
	}
	for _, id := range []string{root.ID, child.ID} {
		issue, err := bd.Show(id)
		if err != nil {
			t.Fatal(err)
		}
		if issue.Status == string(StatusClosed) {
			t.Fatalf("version 1 ABA cleanup closed %s", id)
		}
	}
}

func TestCompareAndClearUnbondedMoleculeAttachmentRejectsConcurrentBond(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	pinned, err := bd.Create(CreateOptions{Title: "work", Type: "task", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	root, err := bd.Create(CreateOptions{Title: "molecule", Type: "molecule", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	description := "attached_molecule: " + root.ID + "\n\nKeep this body."
	if err := bd.Update(pinned.ID, UpdateOptions{Description: &description}); err != nil {
		t.Fatal(err)
	}
	pinned, err = bd.Show(pinned.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldHook := beforeUnbondedMoleculeAttachmentCAS
	t.Cleanup(func() { beforeUnbondedMoleculeAttachmentCAS = oldHook })
	bondDone := make(chan error, 1)
	beforeUnbondedMoleculeAttachmentCAS = func() error {
		go func() {
			bondDone <- mutateAndDoltCommit(bd.getResolvedBeadsDir(), "concurrent molecule ownership bond "+uuid.NewString(), func(ctx context.Context, conn *sql.Conn) error {
				_, err := conn.ExecContext(ctx, `INSERT INTO dependencies
					(id, issue_id, depends_on_issue_id, type, created_by)
					VALUES (?, ?, ?, 'blocks', 'test')`, uuid.NewString(), root.ID, pinned.ID)
				return err
			})
		}()
		select {
		case err := <-bondDone:
			bondDone <- err
			return err
		case <-time.After(time.Second):
			return errors.New("concurrent bond insert did not complete")
		}
	}
	newDescription := SetAttachmentFields(&Issue{Description: pinned.Description}, nil)
	if err := bd.CompareAndClearUnbondedMoleculeAttachment(pinned.ID, pinned.Status, pinned.Assignee, pinned.Description, newDescription); err == nil {
		t.Fatal("metadata pointer cleared across a concurrent ownership bond")
	}
	select {
	case err := <-bondDone:
		if err != nil {
			t.Fatalf("concurrent bond insert: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent bond insert remained blocked after cleanup rollback")
	}
	current, err := bd.Show(pinned.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Description != pinned.Description {
		t.Fatalf("metadata pointer changed after bond race: %q", current.Description)
	}
	if err := bd.VerifyMoleculeRootBond(pinned.ID, root.ID); err != nil {
		t.Fatalf("concurrent ownership bond was not preserved: %v", err)
	}
}

func TestCaptureFormulaMoleculeGraphKeepsOneWriterGeneration(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	root, err := bd.Create(CreateOptions{Title: "formula", Type: "molecule", Priority: 2, Description: "root", Actor: "test-operation", Ephemeral: true})
	if err != nil {
		t.Fatal(err)
	}
	child, err := bd.Create(CreateOptions{Title: "step", Type: "task", Priority: 2, Description: "before", Parent: root.ID, Actor: "test-operation", Ephemeral: true})
	if err != nil {
		t.Fatal(err)
	}
	oldHook := beforeFormulaMoleculeSnapshotContinue
	t.Cleanup(func() { beforeFormulaMoleculeSnapshotContinue = oldHook })
	beforeFormulaMoleculeSnapshotContinue = func() error {
		after := "after"
		return bd.Update(child.ID, UpdateOptions{Description: &after})
	}
	snapshot, err := bd.CaptureFormulaMoleculeGraph(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	var snapshotDescription string
	for _, issue := range snapshot.Issues {
		if issue.ID == child.ID {
			snapshotDescription = issue.Description
		}
	}
	if snapshotDescription != "before" {
		t.Fatalf("formula snapshot mixed writer generations: child description %q", snapshotDescription)
	}
	current, err := bd.Show(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Description != "after" {
		t.Fatalf("concurrent writer did not commit: child description %q", current.Description)
	}
}

func TestApplyMoleculeCleanupReceiptsRevalidatesPostStateCustody(t *testing.T) {
	run := func(t *testing.T, preserve bool, drift func(*testing.T, *Beads, *Issue, *Issue)) {
		t.Helper()
		bd := isolatedMoleculeCleanupBeads(t)
		pinned, root, _ := createMoleculeCleanupFixture(t, bd)
		var preserved *Issue
		var cleanup, keep []MoleculeCleanupReceipt
		var err error
		if preserve {
			preserved, err = bd.Create(CreateOptions{Title: "preserved molecule", Type: "molecule", Priority: 2})
			if err != nil {
				t.Fatal(err)
			}
			if err := bd.AddDependency(preserved.ID, pinned.ID); err != nil {
				t.Fatal(err)
			}
			cleanup, keep, err = bd.CaptureMoleculeCleanupReceiptsPreserving(pinned.ID, []string{root.ID}, []string{preserved.ID})
		} else {
			cleanup, err = bd.CaptureMoleculeCleanupReceipts(pinned.ID, []string{root.ID})
		}
		if err != nil {
			t.Fatal(err)
		}
		attemptID := uuid.NewString()
		expectedMolecule := ""
		if _, err := bd.DetachMoleculeWithAudit(pinned.ID, DetachOptions{
			Operation: "burn", ExpectedMolecule: &expectedMolecule, ExpectedAssignee: &pinned.Assignee,
			ExpectedStatus: &pinned.Status, ExpectedDescription: &pinned.Description, AttemptID: attemptID,
			CleanupMolecules: cleanup, PreservedMolecules: keep, CleanupCloseReason: "burned",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := bd.ApplyMoleculeCleanupReceipts(attemptID, "burned"); err != nil {
			t.Fatal(err)
		}
		drift(t, bd, pinned, preserved)
		if _, err := bd.ApplyMoleculeCleanupReceipts(attemptID, "burned"); err == nil {
			t.Fatal("idempotent cleanup replay accepted drifted terminal custody")
		}
	}

	t.Run("pinned generation", func(t *testing.T) {
		run(t, false, func(t *testing.T, bd *Beads, pinned, _ *Issue) {
			description := "replacement custody"
			if err := bd.Update(pinned.ID, UpdateOptions{Description: &description}); err != nil {
				t.Fatal(err)
			}
		})
	})
	t.Run("new authorized root", func(t *testing.T) {
		run(t, false, func(t *testing.T, bd *Beads, pinned, _ *Issue) {
			foreign, err := bd.Create(CreateOptions{Title: "late molecule", Type: "molecule", Priority: 2})
			if err != nil {
				t.Fatal(err)
			}
			if err := bd.AddDependency(foreign.ID, pinned.ID); err != nil {
				t.Fatal(err)
			}
		})
	})
	t.Run("preserved root", func(t *testing.T) {
		run(t, true, func(t *testing.T, bd *Beads, _ *Issue, preserved *Issue) {
			description := "changed preserved root"
			if err := bd.Update(preserved.ID, UpdateOptions{Description: &description}); err != nil {
				t.Fatal(err)
			}
		})
	})
}

func TestApplyMoleculeCleanupReceiptsPreservesExplicitAuthorizedRoot(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	pinned, stale, child := createMoleculeCleanupFixture(t, bd)
	preserved, err := bd.Create(CreateOptions{Title: "recovered molecule", Type: "molecule", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := bd.AddDependency(preserved.ID, pinned.ID); err != nil {
		t.Fatal(err)
	}
	cleanup, keep, err := bd.CaptureMoleculeCleanupReceiptsPreserving(pinned.ID, []string{stale.ID}, []string{preserved.ID})
	if err != nil {
		t.Fatal(err)
	}
	attemptID := uuid.NewString()
	expectedMolecule := ""
	if _, err := bd.DetachMoleculeWithAudit(pinned.ID, DetachOptions{
		Operation: "burn", ExpectedMolecule: &expectedMolecule, ExpectedAssignee: &pinned.Assignee,
		ExpectedStatus: &pinned.Status, ExpectedDescription: &pinned.Description, AttemptID: attemptID,
		CleanupMolecules: cleanup, PreservedMolecules: keep, CleanupCloseReason: "burned",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := bd.ApplyMoleculeCleanupReceipts(attemptID, "burned"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{stale.ID, child.ID} {
		issue, err := bd.Show(id)
		if err != nil || issue.Status != string(StatusClosed) {
			t.Fatalf("cleaned %s = (%+v, %v), want closed", id, issue, err)
		}
	}
	if issue, err := bd.Show(preserved.ID); err != nil || issue.Status == string(StatusClosed) {
		t.Fatalf("preserved root = (%+v, %v), want open", issue, err)
	}
	if err := bd.VerifyMoleculeRootBond(pinned.ID, preserved.ID); err != nil {
		t.Fatalf("preserved ownership bond: %v", err)
	}
}

func TestApplyMoleculeCleanupReceiptsRequiresDurableDetachAuthority(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	_, root, child := createMoleculeCleanupFixture(t, bd)
	if _, err := bd.ApplyMoleculeCleanupReceipts(uuid.NewString(), "burned"); err == nil {
		t.Fatal("cleanup accepted an attempt without a durable detach authority")
	}
	for _, id := range []string{root.ID, child.ID} {
		issue, err := bd.Show(id)
		if err != nil {
			t.Fatal(err)
		}
		if issue.Status == string(StatusClosed) {
			t.Fatalf("%s closed without durable cleanup authority", id)
		}
	}
}

func TestCaptureMoleculeCleanupReceiptsRejectsUnbondedUnattachedRoot(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	pinned, err := bd.Create(CreateOptions{Title: "work", Type: "task", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	root, err := bd.Create(CreateOptions{Title: "molecule", Type: "molecule", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bd.CaptureMoleculeCleanupReceipts(pinned.ID, []string{root.ID}); err == nil {
		t.Fatal("captured destructive cleanup authority for an unbonded, unattached root")
	}
	if got, err := bd.Show(root.ID); err != nil || got.Status == string(StatusClosed) {
		t.Fatalf("unbonded root changed = (%+v, %v)", got, err)
	}
}

func TestCaptureMoleculeCleanupReceiptsRejectsUnbondedAttachedRoot(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	root, err := bd.Create(CreateOptions{Title: "molecule", Type: "molecule", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := bd.Create(CreateOptions{
		Title: "work", Type: "task", Priority: 2,
		Description: "attached_molecule: " + root.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bd.CaptureMoleculeCleanupReceipts(pinned.ID, []string{root.ID}); err == nil {
		t.Fatal("captured destructive cleanup authority from attachment metadata without a bond")
	}
}

func TestVerifyMoleculeRootBondRequiresExactMoleculeOwnership(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	pinned, root, _ := createMoleculeCleanupFixture(t, bd)
	if err := bd.VerifyMoleculeRootBond(pinned.ID, root.ID); err != nil {
		t.Fatalf("exact molecule bond rejected: %v", err)
	}
	generation, err := bd.MoleculeRootGeneration(root.ID)
	if err != nil || generation == "" {
		t.Fatalf("molecule generation = (%q, %v)", generation, err)
	}
	description := "mutable metadata"
	if err := bd.Update(root.ID, UpdateOptions{Description: &description}); err != nil {
		t.Fatal(err)
	}
	if after, err := bd.MoleculeRootGeneration(root.ID); err != nil || after != generation {
		t.Fatalf("mutable metadata changed molecule generation = (%q, %v), want %q", after, err, generation)
	}
	otherOwner, err := bd.Create(CreateOptions{Title: "other work", Type: "task", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := bd.AddDependency(root.ID, otherOwner.ID); err != nil {
		t.Fatal(err)
	}
	if err := bd.VerifyMoleculeRootBond(pinned.ID, root.ID); err == nil {
		t.Fatal("molecule root with a second outgoing owner accepted")
	}
	foreign, err := bd.Create(CreateOptions{Title: "foreign molecule", Type: "molecule", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := bd.VerifyMoleculeRootBond(pinned.ID, foreign.ID); err == nil {
		t.Fatal("unbonded molecule root accepted")
	}
	addMoleculeCleanupDependency(t, bd, pinned.ID, foreign.ID, "blocks")
	if err := bd.VerifyMoleculeRootBond(pinned.ID, foreign.ID); err == nil {
		t.Fatal("reverse ownership edge accepted")
	}
	ordinary, err := bd.Create(CreateOptions{Title: "ordinary issue", Type: "task", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := bd.AddDependency(ordinary.ID, pinned.ID); err != nil {
		t.Fatal(err)
	}
	if err := bd.VerifyMoleculeRootBond(pinned.ID, ordinary.ID); err == nil {
		t.Fatal("ordinary bonded issue accepted as a molecule root")
	}
}

func TestCaptureMoleculeCleanupReceiptsRejectsNonOwningRelations(t *testing.T) {
	for _, dependencyType := range []string{"related", "tracks", "discovered-from"} {
		t.Run(dependencyType, func(t *testing.T) {
			bd := isolatedMoleculeCleanupBeads(t)
			pinned, err := bd.Create(CreateOptions{Title: "work", Type: "task", Priority: 2})
			if err != nil {
				t.Fatal(err)
			}
			root, err := bd.Create(CreateOptions{Title: "molecule", Type: "molecule", Priority: 2})
			if err != nil {
				t.Fatal(err)
			}
			addMoleculeCleanupDependency(t, bd, root.ID, pinned.ID, dependencyType)
			if _, err := bd.CaptureMoleculeCleanupReceipts(pinned.ID, []string{root.ID}); err == nil {
				t.Fatalf("captured destructive cleanup authority from %s relation", dependencyType)
			}
		})
	}
}

func TestCaptureMoleculeCleanupReceiptsRejectsOutgoingForeignOwnership(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	pinned, root, child := createMoleculeCleanupFixture(t, bd)
	foreign, err := bd.Create(CreateOptions{Title: "foreign molecule", Type: "molecule", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	addMoleculeCleanupDependency(t, bd, child.ID, foreign.ID, "blocks")
	if _, err := bd.CaptureMoleculeCleanupReceipts(pinned.ID, []string{root.ID}); err == nil {
		t.Fatal("captured a child also owned by a foreign molecule")
	}
}

func TestCaptureMoleculeCleanupReceiptsRejectsOrdinaryAttachedIssue(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	ordinary, err := bd.Create(CreateOptions{Title: "ordinary task", Type: "task", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	child, err := bd.Create(CreateOptions{Title: "ordinary child", Type: "task", Priority: 2, Parent: ordinary.ID})
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := bd.Create(CreateOptions{
		Title: "work", Type: "task", Priority: 2,
		Description: "attached_molecule: " + ordinary.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bd.CaptureMoleculeCleanupReceipts(pinned.ID, []string{ordinary.ID}); err == nil {
		t.Fatal("captured an ordinary attached issue as destructive molecule authority")
	}
	for _, id := range []string{ordinary.ID, child.ID} {
		got, err := bd.Show(id)
		if err != nil || got.Status == string(StatusClosed) {
			t.Fatalf("ordinary issue changed = (%+v, %v)", got, err)
		}
	}
}

func TestCaptureMoleculeCleanupReceiptsRejectsClosedAttachedMolecule(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	root, err := bd.Create(CreateOptions{Title: "closed molecule", Type: "molecule", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := bd.Close(root.ID); err != nil {
		t.Fatal(err)
	}
	pinned, err := bd.Create(CreateOptions{
		Title: "work", Type: "task", Priority: 2,
		Description: "attached_molecule: " + root.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bd.CaptureMoleculeCleanupReceipts(pinned.ID, []string{root.ID}); err == nil {
		t.Fatal("captured a closed molecule as destructive cleanup authority")
	}
}

func TestCaptureMoleculeCleanupReceiptsRequiresCompleteBondedRootSet(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	pinned, root, _ := createMoleculeCleanupFixture(t, bd)
	extra, err := bd.Create(CreateOptions{Title: "second molecule", Type: "molecule", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := bd.AddDependency(extra.ID, pinned.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := bd.CaptureMoleculeCleanupReceipts(pinned.ID, []string{root.ID}); err == nil {
		t.Fatal("captured an incomplete destructive cleanup root set")
	}
}

func TestCaptureMoleculeCleanupReceiptsRejectsEmptyAuthorizedRootSet(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	pinned, _, _ := createMoleculeCleanupFixture(t, bd)
	if _, err := bd.CaptureMoleculeCleanupReceipts(pinned.ID, nil); err == nil {
		t.Fatal("captured an empty cleanup set while a molecule root remained authorized")
	}
}

func TestApplyMoleculeCleanupReceiptsRejectsPostValidationGraphRaces(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, *Beads, *Issue, *Issue, *Issue) func(context.Context, *sql.Conn) error
	}{
		{
			name: "root generation",
			prepare: func(_ *testing.T, _ *Beads, _ *Issue, root, _ *Issue) func(context.Context, *sql.Conn) error {
				return func(ctx context.Context, conn *sql.Conn) error {
					_, err := conn.ExecContext(ctx, "UPDATE issues SET description = 'replacement' WHERE id = ?", root.ID)
					return err
				}
			},
		},
		{
			name: "root loses molecule shape",
			prepare: func(_ *testing.T, _ *Beads, _ *Issue, root, _ *Issue) func(context.Context, *sql.Conn) error {
				return func(ctx context.Context, conn *sql.Conn) error {
					_, err := conn.ExecContext(ctx, "DELETE FROM labels WHERE issue_id = ? AND label = 'gt:molecule'", root.ID)
					return err
				}
			},
		},
		{
			name: "bond replacement",
			prepare: func(_ *testing.T, _ *Beads, _ *Issue, root, _ *Issue) func(context.Context, *sql.Conn) error {
				return func(ctx context.Context, conn *sql.Conn) error {
					_, err := conn.ExecContext(ctx, "UPDATE dependencies SET created_by = 'replacement' WHERE issue_id = ?", root.ID)
					return err
				}
			},
		},
		{
			name: "new descendant",
			prepare: func(t *testing.T, bd *Beads, _ *Issue, root, _ *Issue) func(context.Context, *sql.Conn) error {
				extra, err := bd.Create(CreateOptions{Title: "late step", Type: "task", Priority: 2})
				if err != nil {
					t.Fatal(err)
				}
				return func(ctx context.Context, conn *sql.Conn) error {
					_, err := conn.ExecContext(ctx, `INSERT INTO dependencies
						(id, issue_id, depends_on_issue_id, type, created_by)
						VALUES (?, ?, ?, 'parent-child', 'race')`, uuid.NewString(), extra.ID, root.ID)
					return err
				}
			},
		},
		{
			name: "pinned reattachment",
			prepare: func(_ *testing.T, _ *Beads, pinned, _, _ *Issue) func(context.Context, *sql.Conn) error {
				return func(ctx context.Context, conn *sql.Conn) error {
					_, err := conn.ExecContext(ctx, "UPDATE issues SET description = 'attached_molecule: replacement' WHERE id = ?", pinned.ID)
					return err
				}
			},
		},
		{
			name: "new authorized root sharing recorded member",
			prepare: func(t *testing.T, bd *Beads, pinned, _, child *Issue) func(context.Context, *sql.Conn) error {
				extra, err := bd.Create(CreateOptions{Title: "late molecule", Type: "molecule", Priority: 2})
				if err != nil {
					t.Fatal(err)
				}
				return func(ctx context.Context, conn *sql.Conn) error {
					for _, target := range []string{pinned.ID, child.ID} {
						if _, err := conn.ExecContext(ctx, `INSERT INTO dependencies
							(id, issue_id, depends_on_issue_id, type, created_by)
							VALUES (?, ?, ?, 'blocks', 'race')`, uuid.NewString(), extra.ID, target); err != nil {
							return err
						}
					}
					return nil
				}
			},
		},
		{
			name: "foreign owner of recorded member",
			prepare: func(t *testing.T, bd *Beads, _, _, child *Issue) func(context.Context, *sql.Conn) error {
				foreign, err := bd.Create(CreateOptions{Title: "foreign owner", Type: "task", Priority: 2})
				if err != nil {
					t.Fatal(err)
				}
				return func(ctx context.Context, conn *sql.Conn) error {
					_, err := conn.ExecContext(ctx, `INSERT INTO dependencies
						(id, issue_id, depends_on_issue_id, type, created_by)
						VALUES (?, ?, ?, 'blocks', 'race')`, uuid.NewString(), foreign.ID, child.ID)
					return err
				}
			},
		},
		{
			name: "recorded member gains foreign parent",
			prepare: func(t *testing.T, bd *Beads, _, _, child *Issue) func(context.Context, *sql.Conn) error {
				foreign, err := bd.Create(CreateOptions{Title: "foreign molecule", Type: "molecule", Priority: 2})
				if err != nil {
					t.Fatal(err)
				}
				return func(ctx context.Context, conn *sql.Conn) error {
					_, err := conn.ExecContext(ctx, `INSERT INTO dependencies
						(id, issue_id, depends_on_issue_id, type, created_by)
						VALUES (?, ?, ?, 'blocks', 'race')`, uuid.NewString(), child.ID, foreign.ID)
					return err
				}
			},
		},
		{
			name: "pinned away and back generation",
			prepare: func(_ *testing.T, _ *Beads, pinned, _, _ *Issue) func(context.Context, *sql.Conn) error {
				return func(ctx context.Context, conn *sql.Conn) error {
					if _, err := conn.ExecContext(ctx, `UPDATE issues
						SET description = 'replacement', updated_at = DATE_ADD(updated_at, INTERVAL 1 SECOND)
						WHERE id = ?`, pinned.ID); err != nil {
						return err
					}
					_, err := conn.ExecContext(ctx, `UPDATE issues
						SET description = ?, updated_at = DATE_ADD(updated_at, INTERVAL 1 SECOND)
						WHERE id = ?`, pinned.Description, pinned.ID)
					return err
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bd := isolatedMoleculeCleanupBeads(t)
			pinned, root, child := createMoleculeCleanupFixture(t, bd)
			receipts, err := bd.CaptureMoleculeCleanupReceipts(pinned.ID, []string{root.ID})
			if err != nil {
				t.Fatal(err)
			}
			attemptID := recordMoleculeCleanupAttempt(t, bd, pinned, receipts)
			beforeMoleculeCleanupMutation = test.prepare(t, bd, pinned, root, child)
			t.Cleanup(func() { beforeMoleculeCleanupMutation = nil })
			if _, err := bd.ApplyMoleculeCleanupReceipts(attemptID, "burned"); err == nil {
				t.Fatal("cleanup accepted a post-validation graph race")
			}
			beforeMoleculeCleanupMutation = nil
			for _, id := range []string{root.ID, child.ID} {
				issue, err := bd.Show(id)
				if err != nil {
					t.Fatal(err)
				}
				if issue.Status == string(StatusClosed) {
					t.Fatalf("%s was closed despite rejected graph race", id)
				}
			}
		})
	}
}
