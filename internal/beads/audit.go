// Package beads provides audit logging for molecule operations.
package beads

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/steveyegge/gastown/internal/constants"
)

// DetachAuditEntry represents an audit log entry for a detach operation.
type DetachAuditEntry struct {
	Timestamp            string                   `json:"timestamp"`
	Operation            string                   `json:"operation"` // "detach", "burn", "squash"
	PinnedBeadID         string                   `json:"pinned_bead_id"`
	DetachedMolecule     string                   `json:"detached_molecule"`
	DetachedBy           string                   `json:"detached_by,omitempty"` // Agent that triggered detach
	Reason               string                   `json:"reason,omitempty"`      // Optional reason for detach
	PreviousState        string                   `json:"previous_state,omitempty"`
	AttemptID            string                   `json:"attempt_id,omitempty"`
	State                string                   `json:"state,omitempty"`
	CommitMessage        string                   `json:"commit_message,omitempty"`
	CommitOID            string                   `json:"commit_oid,omitempty"`
	PreviousStatus       string                   `json:"previous_status,omitempty"`
	PreviousAssignee     string                   `json:"previous_assignee,omitempty"`
	PreviousDescription  string                   `json:"previous_description,omitempty"`
	ResultingStatus      string                   `json:"resulting_status,omitempty"`
	ResultingAssignee    string                   `json:"resulting_assignee,omitempty"`
	ResultingDescription string                   `json:"resulting_description,omitempty"`
	ResultingGeneration  string                   `json:"resulting_generation,omitempty"`
	CleanupMolecules     []MoleculeCleanupReceipt `json:"cleanup_molecules,omitempty"`
	PreservedMolecules   []MoleculeCleanupReceipt `json:"preserved_molecules,omitempty"`
	CleanupPhase         string                   `json:"cleanup_phase,omitempty"`
	CleanupCloseReason   string                   `json:"cleanup_close_reason,omitempty"`
}

type MoleculeCleanupReceipt struct {
	ID            string                        `json:"id"`
	GraphVersion  int                           `json:"graph_version,omitempty"`
	RootShapeHash string                        `json:"root_shape_hash,omitempty"`
	Members       []MoleculeCleanupIssueReceipt `json:"members,omitempty"`
	Bonds         []MoleculeCleanupBondReceipt  `json:"bonds,omitempty"`
	Ownership     []MoleculeCleanupBondReceipt  `json:"ownership,omitempty"`
	Status        string                        `json:"status,omitempty"` // legacy receipt
	Assignee      string                        `json:"assignee,omitempty"`
	Description   string                        `json:"description,omitempty"`
	UpdatedAt     string                        `json:"updated_at,omitempty"`
}

type MoleculeCleanupIssueReceipt struct {
	ID           string `json:"id"`
	Storage      string `json:"storage"`
	Status       string `json:"status"`
	ContentHash  string `json:"content_hash"`
	SnapshotHash string `json:"snapshot_hash"`
}

type MoleculeCleanupBondReceipt struct {
	ID           string `json:"id"`
	Storage      string `json:"storage"`
	From         string `json:"from"`
	To           string `json:"to"`
	Type         string `json:"type"`
	SnapshotHash string `json:"snapshot_hash"`
}

// DetachOptions specifies optional context for a detach operation.
type DetachOptions struct {
	Operation           string  // "detach", "burn", "squash" - defaults to "detach"
	Agent               string  // Who is performing the detach
	Reason              string  // Optional reason for the detach
	ExpectedMolecule    *string // Optional exact attachment receipt
	ExpectedAssignee    *string // Optional exact assignee receipt
	ExpectedStatus      *string // Optional exact status receipt
	ExpectedDescription *string // Optional exact full-description receipt
	AttemptID           string  // Optional caller-owned durable attempt ID
	CleanupMolecules    []MoleculeCleanupReceipt
	PreservedMolecules  []MoleculeCleanupReceipt
	CleanupCloseReason  string
}

var detachIssueWithAuditCAS = func(b *Beads, issue *Issue, newDesc string, entry DetachAuditEntry) error {
	beadsDir := b.getResolvedBeadsDir()
	if _, err := legacyStoreWithoutMetadata(beadsDir); err != nil {
		return err
	}
	if entry.AttemptID == "" {
		entry.AttemptID = uuid.NewString()
	}
	entry.CommitMessage = "gt: detach molecule from " + issue.ID + " attempt " + entry.AttemptID
	entry.State = "pending"
	entry.PreviousStatus = issue.Status
	entry.PreviousAssignee = issue.Assignee
	entry.PreviousDescription = issue.Description
	entry.ResultingStatus = issue.Status
	entry.ResultingAssignee = issue.Assignee
	entry.ResultingDescription = newDesc
	if err := b.LogDetachAudit(entry); err != nil {
		return fmt.Errorf("persisting pending detach audit receipt: %w", err)
	}
	oid, err := mutateAndDoltCommitWithOID(beadsDir, entry.CommitMessage, func(ctx context.Context, conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx, `UPDATE issues
		SET description = ?, updated_at = CURRENT_TIMESTAMP(6)
		WHERE id = ?
		  AND COALESCE(description, '') = ?
		  AND COALESCE(assignee, '') = ?
		  AND status = ?`, newDesc, issue.ID, issue.Description, issue.Assignee, issue.Status)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return fmt.Errorf("%w: pinned bead changed before detach", ErrAgentFieldsChanged)
		}
		var updatedAt time.Time
		if err := conn.QueryRowContext(ctx, "SELECT updated_at FROM issues WHERE id = ?", issue.ID).Scan(&updatedAt); err != nil {
			return fmt.Errorf("capturing detached bead generation: %w", err)
		}
		entry.ResultingGeneration = cleanupGeneration(updatedAt)
		entry.Timestamp = currentTimestamp()
		if err := b.LogDetachAudit(entry); err != nil {
			return fmt.Errorf("persisting pending detach generation: %w", err)
		}
		return nil
	})
	if err != nil {
		var unknown *DoltCommitOutcomeUnknownError
		if errors.As(err, &unknown) {
			return err
		}
		entry.State = "aborted"
		entry.Timestamp = currentTimestamp()
		if auditErr := b.LogDetachAudit(entry); auditErr != nil {
			return errors.Join(err, fmt.Errorf("persisting aborted detach audit receipt: %w", auditErr))
		}
		return err
	}
	entry.State = "committed"
	entry.CommitOID = oid
	entry.Timestamp = currentTimestamp()
	if err := b.LogDetachAudit(entry); err != nil {
		return fmt.Errorf("persisting committed detach audit receipt: %w", err)
	}
	return nil
}

var beforeDetachIssueCAS func()

// DetachMoleculeWithAudit removes molecule attachment from a pinned bead and logs the operation.
// Uses advisory file locking to prevent concurrent read-modify-write races.
// Returns the updated issue.
func (b *Beads) DetachMoleculeWithAudit(pinnedBeadID string, opts DetachOptions) (*Issue, error) {
	// Acquire per-bead lock to serialize concurrent attach/detach operations
	unlock, err := b.lockBead(pinnedBeadID)
	if err != nil {
		return nil, fmt.Errorf("acquiring bead lock: %w", err)
	}
	defer unlock()

	// Fetch the pinned bead first to get previous state
	issue, err := b.Show(pinnedBeadID)
	if err != nil {
		return nil, fmt.Errorf("fetching pinned bead: %w", err)
	}

	operation := normalizeDetachOperation(opts.Operation)

	// Get current attachment info for audit.
	attachment := ParseAttachmentFields(issue)
	currentMolecule := ""
	if attachment != nil {
		currentMolecule = attachment.AttachedMolecule
	}
	if opts.ExpectedMolecule != nil && currentMolecule != *opts.ExpectedMolecule {
		if currentMolecule == "" {
			committed, err := b.hasDetachAuditReceipt(pinnedBeadID, *opts.ExpectedMolecule, operation, opts, issue)
			if err != nil {
				return nil, err
			}
			if committed {
				return issue, nil
			}
		}
		return nil, fmt.Errorf("pinned bead %s molecule changed from %q to %q", pinnedBeadID, *opts.ExpectedMolecule, currentMolecule)
	}
	if opts.ExpectedAssignee != nil && issue.Assignee != *opts.ExpectedAssignee {
		return nil, fmt.Errorf("pinned bead %s assignee changed from %q to %q", pinnedBeadID, *opts.ExpectedAssignee, issue.Assignee)
	}
	if opts.ExpectedStatus != nil && issue.Status != *opts.ExpectedStatus {
		return nil, fmt.Errorf("pinned bead %s status changed from %q to %q", pinnedBeadID, *opts.ExpectedStatus, issue.Status)
	}
	if opts.ExpectedDescription != nil && issue.Description != *opts.ExpectedDescription {
		return nil, fmt.Errorf("pinned bead %s description changed", pinnedBeadID)
	}
	if attachment == nil && opts.ExpectedDescription == nil {
		return issue, nil // Nothing to detach
	}

	entry := DetachAuditEntry{
		Timestamp:          currentTimestamp(),
		Operation:          operation,
		PinnedBeadID:       pinnedBeadID,
		DetachedMolecule:   currentMolecule,
		DetachedBy:         opts.Agent,
		Reason:             opts.Reason,
		PreviousState:      issue.Status,
		AttemptID:          opts.AttemptID,
		CleanupMolecules:   append([]MoleculeCleanupReceipt(nil), opts.CleanupMolecules...),
		PreservedMolecules: append([]MoleculeCleanupReceipt(nil), opts.PreservedMolecules...),
		CleanupCloseReason: opts.CleanupCloseReason,
	}
	// Clear attachment fields by passing nil
	newDesc := SetAttachmentFields(issue, nil)
	if beforeDetachIssueCAS != nil {
		beforeDetachIssueCAS()
	}
	if err := detachIssueWithAuditCAS(b, issue, newDesc, entry); err != nil {
		return nil, fmt.Errorf("conditionally detaching pinned bead: %w", err)
	}

	// Re-fetch to return updated state
	return b.Show(pinnedBeadID)
}

func normalizeDetachOperation(operation string) string {
	operation = strings.ToLower(strings.TrimSpace(operation))
	if operation == "" {
		return "detach"
	}
	return operation
}

func (b *Beads) hasDetachAuditReceipt(pinnedBeadID, moleculeID, operation string, opts DetachOptions, current *Issue) (bool, error) {
	entries, err := b.detachAuditEntries()
	if err != nil {
		return false, fmt.Errorf("reading detach audit: %w", err)
	}
	for _, entry := range entries {
		if entry.PinnedBeadID != pinnedBeadID || entry.DetachedMolecule != moleculeID ||
			normalizeDetachOperation(entry.Operation) != operation || entry.AttemptID == "" {
			continue
		}
		if !detachReceiptMatches(entry, opts, current) || entry.State == "aborted" {
			continue
		}
		if entry.State == "committed" && entry.CommitOID != "" {
			return detachSnapshotAtCommitFn(b, entry, entry.CommitOID)
		}
		if entry.State != "pending" || entry.CommitMessage == "" {
			continue
		}
		oid, err := detachCommitOIDFn(b, entry.CommitMessage)
		if errors.Is(err, sql.ErrNoRows) {
			oid, err = reconcilePendingDetachCommitFn(b, entry)
		}
		if err != nil {
			return false, err
		}
		committed, err := detachSnapshotAtCommitFn(b, entry, oid)
		if err != nil || !committed {
			return false, err
		}
		entry.State = "committed"
		entry.CommitOID = oid
		entry.Timestamp = currentTimestamp()
		if err := b.LogDetachAudit(entry); err != nil {
			return false, fmt.Errorf("reconciling committed detach audit receipt: %w", err)
		}
		return true, nil
	}
	return false, nil
}

func detachReceiptMatches(entry DetachAuditEntry, opts DetachOptions, current *Issue) bool {
	if entry.PreviousStatus == "" || entry.CommitMessage == "" || current == nil {
		return false
	}
	if opts.ExpectedStatus != nil && entry.PreviousStatus != *opts.ExpectedStatus {
		return false
	}
	if opts.ExpectedAssignee != nil && entry.PreviousAssignee != *opts.ExpectedAssignee {
		return false
	}
	if opts.ExpectedDescription != nil && entry.PreviousDescription != *opts.ExpectedDescription {
		return false
	}
	if opts.AttemptID != "" && entry.AttemptID != opts.AttemptID {
		return false
	}
	if len(opts.CleanupMolecules) > 0 && !cleanupReceiptsEqual(entry.CleanupMolecules, opts.CleanupMolecules) {
		return false
	}
	if len(opts.PreservedMolecules) > 0 && !cleanupReceiptsEqual(entry.PreservedMolecules, opts.PreservedMolecules) {
		return false
	}
	return current.Status == entry.ResultingStatus && current.Assignee == entry.ResultingAssignee && current.Description == entry.ResultingDescription && cleanupGenerationStringMatches(entry.ResultingGeneration, current.UpdatedAt)
}

func cleanupReceiptsEqual(a, b []MoleculeCleanupReceipt) bool {
	return reflect.DeepEqual(a, b)
}

func (b *Beads) PendingMoleculeCleanup(pinnedBeadID, operation string) (*DetachAuditEntry, error) {
	entries, err := b.detachAuditEntries()
	if err != nil {
		return nil, err
	}
	latest := make(map[string]DetachAuditEntry)
	for _, entry := range entries {
		if entry.PinnedBeadID == pinnedBeadID && normalizeDetachOperation(entry.Operation) == normalizeDetachOperation(operation) && entry.AttemptID != "" {
			latest[entry.AttemptID] = entry
		}
	}
	var pending *DetachAuditEntry
	pendingCount := 0
	for _, entry := range latest {
		if len(entry.CleanupMolecules) == 0 || entry.State == "aborted" || entry.CleanupPhase == "complete" {
			continue
		}
		copy := entry
		pending = &copy
		pendingCount++
	}
	if pendingCount > 1 {
		return nil, fmt.Errorf("multiple incomplete molecule cleanup attempts for %s", pinnedBeadID)
	}
	return pending, nil
}

func (b *Beads) moleculeCleanupAttempt(attemptID string) (*DetachAuditEntry, error) {
	entries, err := b.detachAuditEntries()
	if err != nil {
		return nil, err
	}
	for i := range entries {
		entry := &entries[i]
		if entry.AttemptID != attemptID {
			continue
		}
		if entry.State != "committed" || entry.PinnedBeadID == "" || len(entry.CleanupMolecules) == 0 {
			return nil, fmt.Errorf("molecule cleanup attempt %s lacks committed cleanup authority", attemptID)
		}
		if entry.ResultingGeneration == "" {
			generation, matches, err := b.detachGenerationAtCommit(*entry, entry.CommitOID)
			if err != nil {
				return nil, fmt.Errorf("recovering historical cleanup generation: %w", err)
			}
			if !matches {
				return nil, fmt.Errorf("molecule cleanup attempt %s does not match its committed snapshot", attemptID)
			}
			entry.ResultingGeneration = generation
		}
		return entry, nil
	}
	return nil, fmt.Errorf("molecule cleanup attempt %s has no durable cleanup authority", attemptID)
}

func (b *Beads) PendingDetachAttempt(pinnedBeadID, operation string) (*DetachAuditEntry, error) {
	entries, err := b.detachAuditEntries()
	if err != nil {
		return nil, err
	}
	latest := make(map[string]DetachAuditEntry)
	for _, entry := range entries {
		if entry.PinnedBeadID == pinnedBeadID && normalizeDetachOperation(entry.Operation) == normalizeDetachOperation(operation) && entry.AttemptID != "" {
			latest[entry.AttemptID] = entry
		}
	}
	var pending *DetachAuditEntry
	for _, entry := range latest {
		if entry.State != "pending" {
			continue
		}
		if pending != nil {
			return nil, fmt.Errorf("multiple pending detach attempts for %s", pinnedBeadID)
		}
		copy := entry
		pending = &copy
	}
	return pending, nil
}

func (b *Beads) LogMoleculeCleanupPhase(attemptID, phase string) error {
	parsed, err := uuid.Parse(attemptID)
	if err != nil || parsed.String() != attemptID {
		return fmt.Errorf("invalid molecule cleanup attempt ID %q", attemptID)
	}
	rank := map[string]int{"": 0, "descendants_closed": 1, "bonds_removed": 2, "complete": 3}
	nextRank, ok := rank[phase]
	if !ok || phase == "" {
		return fmt.Errorf("unknown molecule cleanup phase %q", phase)
	}
	unlock, err := b.lockBead("molecule-cleanup-" + attemptID)
	if err != nil {
		return fmt.Errorf("locking molecule cleanup phase: %w", err)
	}
	defer unlock()

	entries, err := b.detachAuditEntries()
	if err != nil {
		return err
	}
	var latest *DetachAuditEntry
	for _, entry := range entries {
		if entry.AttemptID == attemptID {
			copy := entry
			latest = &copy
		}
	}
	if latest == nil || latest.State != "committed" {
		return fmt.Errorf("missing committed detach receipt for cleanup attempt %s", attemptID)
	}
	currentRank, ok := rank[latest.CleanupPhase]
	if !ok {
		return fmt.Errorf("unknown stored molecule cleanup phase %q", latest.CleanupPhase)
	}
	if nextRank < currentRank {
		return fmt.Errorf("molecule cleanup attempt %s cannot regress from %s to %s", attemptID, latest.CleanupPhase, phase)
	}
	if nextRank == currentRank {
		return nil
	}
	return b.LogDetachAudit(DetachAuditEntry{
		Timestamp: currentTimestamp(), AttemptID: attemptID, CleanupPhase: phase,
	})
}

func (b *Beads) detachAuditEntries() ([]DetachAuditEntry, error) {
	f, err := os.Open(filepath.Join(b.getResolvedBeadsDir(), "audit.log")) //nolint:gosec // path is constructed internally
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries := make(map[string]DetachAuditEntry)
	var withoutAttempt []DetachAuditEntry
	decoder := json.NewDecoder(f)
	for {
		var entry DetachAuditEntry
		if err := decoder.Decode(&entry); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decoding detach audit: %w", err)
		}
		if entry.AttemptID == "" {
			withoutAttempt = append(withoutAttempt, entry)
			continue
		}
		if entry.CleanupPhase != "" && entry.CommitMessage == "" {
			prior, ok := entries[entry.AttemptID]
			if !ok {
				return nil, fmt.Errorf("cleanup phase for unknown detach attempt %s", entry.AttemptID)
			}
			prior.Timestamp = entry.Timestamp
			prior.CleanupPhase = entry.CleanupPhase
			entries[entry.AttemptID] = prior
			continue
		}
		entries[entry.AttemptID] = entry
	}
	result := append([]DetachAuditEntry(nil), withoutAttempt...)
	for _, entry := range entries {
		result = append(result, entry)
	}
	return result, nil
}

func (b *Beads) detachCommitOID(message string) (string, error) {
	beadsDir := b.getResolvedBeadsDir()
	db, err := openDoltSQL(beadsDir)
	if err != nil {
		return "", err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), constants.BdCommandTimeout)
	defer cancel()
	return doltCommitOIDForMessage(ctx, db, message)
}

var detachCommitOIDFn = (*Beads).detachCommitOID
var detachSnapshotAtCommitFn = (*Beads).detachSnapshotAtCommit
var reconcilePendingDetachCommitFn = (*Beads).reconcilePendingDetachCommit

func (b *Beads) reconcilePendingDetachCommit(entry DetachAuditEntry) (string, error) {
	return mutateAndDoltCommitWithOID(b.getResolvedBeadsDir(), entry.CommitMessage, func(ctx context.Context, conn *sql.Conn) error {
		matches := 0
		for _, storage := range []string{"issues", "wisps"} {
			var status, assignee, description string
			var updatedAt time.Time
			err := conn.QueryRowContext(ctx, "SELECT status, COALESCE(assignee, ''), COALESCE(description, ''), updated_at FROM "+storage+" WHERE id = ?", entry.PinnedBeadID).
				Scan(&status, &assignee, &description, &updatedAt)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			matches++
			if status != entry.ResultingStatus || assignee != entry.ResultingAssignee || description != entry.ResultingDescription || !cleanupGenerationMatches(entry.ResultingGeneration, updatedAt) {
				return fmt.Errorf("%w: pending detach result changed before Dolt commit", ErrAgentFieldsChanged)
			}
		}
		if matches != 1 {
			return fmt.Errorf("%w: pending detach result exists in %d stores", ErrAgentFieldsChanged, matches)
		}
		return nil
	})
}

func (b *Beads) detachSnapshotAtCommit(entry DetachAuditEntry, oid string) (bool, error) {
	_, matches, err := b.detachGenerationAtCommit(entry, oid)
	return matches, err
}

func (b *Beads) detachGenerationAtCommit(entry DetachAuditEntry, oid string) (string, bool, error) {
	if oid == "" || entry.ResultingStatus == "" {
		return "", false, nil
	}
	if !validDoltCommitHash(oid) {
		return "", false, fmt.Errorf("invalid Dolt commit hash %q", oid)
	}
	db, err := openDoltSQL(b.getResolvedBeadsDir())
	if err != nil {
		return "", false, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), constants.BdCommandTimeout)
	defer cancel()
	var status, assignee, description string
	var updatedAt time.Time
	query := fmt.Sprintf(`SELECT status, COALESCE(assignee, ''), COALESCE(description, ''), updated_at
		FROM issues AS OF '%s' WHERE id = ?`, oid)
	err = db.QueryRowContext(ctx, query, entry.PinnedBeadID).Scan(&status, &assignee, &description, &updatedAt)
	if err != nil {
		return "", false, err
	}
	matches := status == entry.ResultingStatus && assignee == entry.ResultingAssignee && description == entry.ResultingDescription
	if entry.ResultingGeneration != "" {
		matches = matches && cleanupGenerationMatches(entry.ResultingGeneration, updatedAt)
	}
	return cleanupGeneration(updatedAt), matches, nil
}

func validDoltCommitHash(hash string) bool {
	if len(hash) != 32 {
		return false
	}
	for _, c := range hash {
		if (c < '0' || c > '9') && (c < 'a' || c > 'v') {
			return false
		}
	}
	return true
}

// LogDetachAudit appends an audit entry to the audit log file.
// The audit log is stored in the resolved .beads directory as audit.log in JSONL format.
// This follows any beads redirect so audit entries go to the correct location.
func (b *Beads) LogDetachAudit(entry DetachAuditEntry) (retErr error) {
	auditPath := filepath.Join(b.getResolvedBeadsDir(), "audit.log")

	// Marshal entry to JSON
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshaling audit entry: %w", err)
	}

	// Append to audit log file
	f, err := os.OpenFile(auditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600) //nolint:gosec // G304: path is constructed internally
	if err != nil {
		return fmt.Errorf("opening audit log: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("closing audit log: %w", err)
		}
	}()

	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("writing audit entry: %w", err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing audit log: %w", err)
	}

	return nil
}
