package cmd

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/steveyegge/gastown/internal/beads"
)

var logMoleculeCleanupPhaseFn = func(bd *beads.Beads, attemptID, phase string) error {
	return bd.LogMoleculeCleanupPhase(attemptID, phase)
}

var detachAndCleanupMoleculesFn = detachAndCleanupMolecules

var applyPreservingMoleculeCleanupFn = detachAndCleanupMoleculesPreserving

var resumeMoleculeCleanupIfPresentFn = resumeMoleculeCleanupIfPresent

func captureMoleculeCleanupReceipts(bd *beads.Beads, pinnedBeadID string, moleculeIDs []string) ([]beads.MoleculeCleanupReceipt, error) {
	return bd.CaptureMoleculeCleanupReceipts(pinnedBeadID, moleculeIDs)
}

func resumeMoleculeCleanupIfPresent(bd *beads.Beads, pinnedBeadID, operation, reason string) (bool, int, error) {
	entry, err := bd.PendingMoleculeCleanup(pinnedBeadID, operation)
	if err != nil || entry == nil {
		return false, 0, err
	}
	if entry.State == "pending" {
		expectedMolecule := entry.DetachedMolecule
		opts := beads.DetachOptions{
			Operation: operation, Agent: entry.DetachedBy, Reason: entry.Reason,
			ExpectedMolecule: &expectedMolecule, ExpectedAssignee: &entry.PreviousAssignee,
			ExpectedStatus: &entry.PreviousStatus, ExpectedDescription: &entry.PreviousDescription,
			AttemptID: entry.AttemptID, CleanupMolecules: entry.CleanupMolecules, CleanupCloseReason: entry.CleanupCloseReason,
			PreservedMolecules: entry.PreservedMolecules,
		}
		if _, err := bd.DetachMoleculeWithAudit(pinnedBeadID, opts); err != nil {
			return true, 0, fmt.Errorf("reconciling detach before molecule cleanup: %w", err)
		}
		entry, err = bd.PendingMoleculeCleanup(pinnedBeadID, operation)
		if err != nil || entry == nil {
			return true, 0, fmt.Errorf("reloading reconciled molecule cleanup: %w", err)
		}
	}
	if entry.CleanupCloseReason != "" {
		reason = entry.CleanupCloseReason
	}
	closed, err := runRecordedMoleculeCleanup(bd, pinnedBeadID, entry, reason)
	return true, closed, err
}

func detachAndCleanupMolecules(bd *beads.Beads, pinnedBeadID string, pinned *beadInfo, operation, agent, reason, closeReason string, moleculeIDs []string) (int, error) {
	return detachAndCleanupMoleculesPreserving(bd, pinnedBeadID, pinned, operation, agent, reason, closeReason, moleculeIDs, nil)
}

func detachAndCleanupMoleculesPreserving(bd *beads.Beads, pinnedBeadID string, pinned *beadInfo, operation, agent, reason, closeReason string, moleculeIDs, preservedIDs []string) (int, error) {
	if resumed, closed, err := resumeMoleculeCleanupIfPresent(bd, pinnedBeadID, operation, closeReason); resumed || err != nil {
		return closed, err
	}
	current, err := bd.Show(pinnedBeadID)
	if err != nil {
		return 0, fmt.Errorf("revalidating pinned bead before molecule cleanup: %w", err)
	}
	expectedAttachment := beads.ParseAttachmentFields(&beads.Issue{Description: pinned.Description})
	currentAttachment := beads.ParseAttachmentFields(current)
	expectedMolecule, currentMolecule := "", ""
	if expectedAttachment != nil {
		expectedMolecule = expectedAttachment.AttachedMolecule
	}
	if currentAttachment != nil {
		currentMolecule = currentAttachment.AttachedMolecule
	}
	if currentMolecule != expectedMolecule {
		return 0, fmt.Errorf("pinned bead %s molecule changed from %q to %q", pinnedBeadID, expectedMolecule, currentMolecule)
	}
	if current.Assignee != pinned.Assignee {
		return 0, fmt.Errorf("pinned bead %s assignee changed from %q to %q", pinnedBeadID, pinned.Assignee, current.Assignee)
	}
	if current.Status != pinned.Status || current.Description != pinned.Description {
		return 0, fmt.Errorf("pinned bead %s generation changed before molecule cleanup", pinnedBeadID)
	}
	receipts, preserved, err := bd.CaptureMoleculeCleanupReceiptsPreserving(pinnedBeadID, moleculeIDs, preservedIDs)
	if err != nil {
		return 0, err
	}
	attemptID := uuid.NewString()
	opts := beads.DetachOptions{
		Operation: operation, Agent: agent, Reason: reason,
		ExpectedMolecule: &expectedMolecule, ExpectedAssignee: &pinned.Assignee,
		ExpectedStatus: &pinned.Status, ExpectedDescription: &pinned.Description,
		AttemptID: attemptID, CleanupMolecules: receipts, PreservedMolecules: preserved, CleanupCloseReason: closeReason,
	}
	if _, err := bd.DetachMoleculeWithAudit(pinnedBeadID, opts); err != nil {
		return 0, err
	}
	entry, err := bd.PendingMoleculeCleanup(pinnedBeadID, operation)
	if err != nil {
		return 0, fmt.Errorf("reloading recorded molecule cleanup attempt %s: %w", attemptID, err)
	}
	if entry == nil || entry.AttemptID != attemptID {
		return 0, fmt.Errorf("recorded molecule cleanup attempt %s is not authoritative", attemptID)
	}
	return runRecordedMoleculeCleanup(bd, pinnedBeadID, entry, closeReason)
}

func runRecordedMoleculeCleanup(bd *beads.Beads, pinnedBeadID string, entry *beads.DetachAuditEntry, closeReason string) (int, error) {
	if entry.CleanupPhase == "complete" {
		return 0, nil
	}
	childrenClosed, err := bd.ApplyMoleculeCleanupReceipts(entry.AttemptID, closeReason)
	if err != nil {
		return 0, err
	}
	if err := logMoleculeCleanupPhaseFn(bd, entry.AttemptID, "complete"); err != nil {
		return childrenClosed, err
	}
	return childrenClosed, nil
}
