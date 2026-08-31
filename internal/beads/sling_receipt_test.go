package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLifecycleReassignmentReceiptRejectsFreshForgedIntent(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	oldAssignee := "gastown/polecats/old"
	work, err := bd.Create(CreateOptions{Title: "work", Type: "task", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := bd.Update(work.ID, UpdateOptions{Assignee: &oldAssignee}); err != nil {
		t.Fatal(err)
	}
	binding := LifecycleReassignmentBinding{
		Version:   2,
		AttemptID: uuid.NewString(), BeadID: work.ID,
		OldAssignee: oldAssignee, OldIncarnation: "old-generation",
		NewAssignee: "gastown/polecats/new", NewIncarnation: "new-generation",
		Requester: "mayor/", ThreadID: "sling-retirement-test", CapabilityHash: "trusted-capability-hash", AuthorityMAC: "trusted-authority-mac",
	}
	if err := bd.PrepareLifecycleReassignment(binding); err != nil {
		t.Fatal(err)
	}
	if err := bd.BindLifecycleReassignmentDelivery(binding, uuid.NewString(), "premature"); err == nil {
		t.Fatal("delivery bound before replacement owned the work")
	}
	if err := bd.Update(work.ID, UpdateOptions{Assignee: &binding.NewAssignee}); err != nil {
		t.Fatal(err)
	}
	deliveryID := uuid.NewString()
	intentHash := "exact-intent-hash"
	if err := bd.BindLifecycleReassignmentDelivery(binding, deliveryID, intentHash); err != nil {
		t.Fatal(err)
	}
	forged := binding
	forged.CapabilityHash = "attacker-chosen-capability-hash"
	if err := bd.ValidateLifecycleReassignmentDelivery(forged, deliveryID, intentHash); err == nil {
		t.Fatal("fresh forged intent matched immutable reassignment custody")
	}
	if err := bd.ValidateLifecycleReassignmentDelivery(binding, deliveryID, intentHash); err != nil {
		t.Fatal(err)
	}
	if err := bd.MarkLifecycleReassignmentApplied(binding, deliveryID, intentHash); err != nil {
		t.Fatal(err)
	}
}

func formulaPublicationFixture(t *testing.T, bd *Beads) (root, child, work *Issue, generation string) {
	t.Helper()
	var err error
	root, err = bd.Create(CreateOptions{Title: "formula", Type: "molecule", Priority: 2, Description: "root", Actor: "test-operation", Ephemeral: true})
	if err != nil {
		t.Fatal(err)
	}
	child, err = bd.Create(CreateOptions{Title: "step", Type: "task", Priority: 2, Description: "before", Parent: root.ID, Actor: "test-operation", Ephemeral: true})
	if err != nil {
		t.Fatal(err)
	}
	work, err = bd.Create(CreateOptions{Title: "work", Type: "task", Priority: 2, Description: "before publication"})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := bd.CaptureFormulaMoleculeGraph(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	generation, err = FormulaMoleculeGraphGeneration(graph)
	if err != nil {
		t.Fatal(err)
	}
	return root, child, work, generation
}

func formulaPublication(t *testing.T, bd *Beads, root, work *Issue, generation string) FormulaMoleculePublication {
	t.Helper()
	authorization := FormulaMoleculeAuthorization{
		PublicationID: uuid.NewString(), RootID: root.ID, RootGeneration: generation,
		Formula: "test-formula", Owner: "test-owner", RequestFingerprint: "test-request", Actor: "test-actor",
	}
	commit, err := bd.PrepareFormulaMoleculeAuthorization(authorization)
	if err != nil {
		t.Fatal(err)
	}
	return FormulaMoleculePublication{
		PublicationID: authorization.PublicationID, RootID: root.ID, RootGeneration: generation, WorkID: work.ID,
		ExpectedStatus: work.Status, ExpectedAssignee: work.Assignee, ExpectedDescription: work.Description,
		NewStatus: "hooked", NewAssignee: "gastown/polecats/new", NewDescription: "published metadata",
		Authorization: authorization, AuthorizationCommit: commit,
	}
}

func TestPublishFormulaMoleculeAssignmentRejectsGraphDriftAfterProof(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	root, child, work, generation := formulaPublicationFixture(t, bd)
	publication := formulaPublication(t, bd, root, work, generation)
	after := "changed after proof"
	if err := bd.Update(child.ID, UpdateOptions{Description: &after}); err != nil {
		t.Fatal(err)
	}
	if err := bd.PublishFormulaMoleculeAssignment(publication); err == nil {
		t.Fatal("formula assignment published after its proved graph generation changed")
	}
	current, err := bd.Show(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != work.Status || current.Assignee != work.Assignee || current.Description != work.Description {
		t.Fatalf("rejected publication changed work: %+v", current)
	}
}

func TestPublishFormulaMoleculeAssignmentSerializesConcurrentGraphWriter(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	root, child, work, generation := formulaPublicationFixture(t, bd)
	publication := formulaPublication(t, bd, root, work, generation)
	oldHook := beforeFormulaMoleculePublicationCAS
	t.Cleanup(func() { beforeFormulaMoleculePublicationCAS = oldHook })
	db, err := openDoltSQL(bd.getResolvedBeadsDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	writerObserved := make(chan struct{})
	writerDone := make(chan error, 1)
	beforeFormulaMoleculePublicationCAS = func() error {
		go func() {
			_, updateErr := db.ExecContext(context.Background(), `UPDATE /* formula-concurrent-writer */ issues SET description = 'changed concurrently' WHERE id = ?`, child.ID)
			writerDone <- updateErr
		}()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var count int
			if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM information_schema.processlist WHERE info LIKE '%formula-concurrent-writer%'`).Scan(&count); err != nil {
				return err
			}
			if count > 0 {
				close(writerObserved)
				return nil
			}
			time.Sleep(10 * time.Millisecond)
		}
		return fmt.Errorf("concurrent graph writer never reached the locked update")
	}
	if err := bd.PublishFormulaMoleculeAssignment(publication); err != nil {
		t.Fatal(err)
	}
	select {
	case <-writerObserved:
	default:
		t.Fatal("publication completed without observing the blocked graph update")
	}
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatalf("serialized graph writer: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("graph writer remained blocked after publication committed")
	}
	applied, err := bd.FormulaMoleculePublicationApplied(publication)
	if err != nil || !applied {
		t.Fatalf("formula publication receipt = (%v, %v), want applied", applied, err)
	}
	current, err := bd.Show(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != publication.NewStatus || current.Assignee != publication.NewAssignee || current.Description != publication.NewDescription {
		t.Fatalf("published work = %+v", current)
	}
}

func TestPublishFormulaMoleculeAssignmentFinishesUnknownDoltCommitBeforeSuccess(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	root, _, work, generation := formulaPublicationFixture(t, bd)
	publication := formulaPublication(t, bd, root, work, generation)
	originalCommit := execPinnedDoltCommit
	calls := 0
	execPinnedDoltCommit = func(ctx context.Context, conn *sql.Conn, message string) error {
		calls++
		if calls == 1 {
			return errors.New("injected pre-DOLT_COMMIT failure")
		}
		return originalCommit(ctx, conn, message)
	}
	t.Cleanup(func() { execPinnedDoltCommit = originalCommit })
	if err := bd.PublishFormulaMoleculeAssignment(publication); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("DOLT_COMMIT calls = %d, want failed attempt plus idempotent finish", calls)
	}
	oid, err := doltCommitOIDForMessageInStore(bd.getResolvedBeadsDir(), "gt: publish formula molecule "+publication.PublicationID)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := bd.formulaMoleculePublicationAtCommit(publication, oid)
	if err != nil || !applied {
		t.Fatalf("recovered historical publication = (%v, %v)", applied, err)
	}
}

func TestFormulaMoleculePublicationLookupRejectsUncommittedWorkingSet(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	root, _, work, generation := formulaPublicationFixture(t, bd)
	publication := formulaPublication(t, bd, root, work, generation)
	originalCommit := execPinnedDoltCommit
	execPinnedDoltCommit = func(context.Context, *sql.Conn, string) error {
		return errors.New("injected DOLT_COMMIT failure")
	}
	t.Cleanup(func() { execPinnedDoltCommit = originalCommit })

	if err := bd.PublishFormulaMoleculeAssignment(publication); err == nil {
		t.Fatal("publication unexpectedly succeeded without a durable Dolt commit")
	}
	got, err := bd.FormulaMoleculePublicationByID(publication.PublicationID)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatal("uncommitted working-set publication was accepted by ID")
	}
	applied, err := bd.FormulaMoleculePublicationApplied(publication)
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("uncommitted working-set publication was accepted as applied")
	}
}

func TestFormulaMoleculePublicationLookupAcceptsCommittedStandaloneRoot(t *testing.T) {
	bd := isolatedMoleculeCleanupBeads(t)
	root, _, _, generation := formulaPublicationFixture(t, bd)
	publication := formulaPublication(t, bd, root, root, generation)
	if err := bd.PublishFormulaMoleculeAssignment(publication); err != nil {
		t.Fatal(err)
	}
	committed, err := bd.formulaMoleculePublicationCommitted(publication)
	if err != nil || !committed {
		t.Fatalf("committed standalone publication = (%v, %v)", committed, err)
	}
	got, err := bd.FormulaMoleculePublicationByID(publication.PublicationID)
	if err != nil || got == nil {
		t.Fatalf("standalone publication lookup = (%v, %v)", got, err)
	}
}
