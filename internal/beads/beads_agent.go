// Package beads provides agent bead management.
package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/uuid"
	beadsdk "github.com/steveyegge/beads"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/telemetry"
)

// lockAgentBead acquires an exclusive file lock for a specific agent bead ID.
// This prevents concurrent read-modify-write races in methods like
// CreateOrReopenAgentBead, ResetAgentBeadForReuse, and UpdateAgentDescriptionFields.
// Caller must defer fl.Unlock().
func (b *Beads) lockAgentBead(id string) (*flock.Flock, error) {
	return b.lockAgentBeadContext(context.Background(), id)
}

func (b *Beads) lockAgentBeadContext(ctx context.Context, id string) (*flock.Flock, error) {
	lockDir := filepath.Join(b.getResolvedBeadsDir(), ".locks")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		return nil, fmt.Errorf("creating bead lock dir: %w", err)
	}
	lockPath := filepath.Join(lockDir, fmt.Sprintf("agent-%s.lock", id))
	fl := flock.New(lockPath)
	locked, err := fl.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("acquiring agent bead lock for %s: %w", id, err)
	}
	if !locked {
		return nil, ctx.Err()
	}
	return fl, nil
}

// AgentFields holds structured fields for agent beads.
// These are stored as "key: value" lines in the description.
type AgentFields struct {
	RoleType          string // polecat, witness, refinery, deacon, mayor
	Rig               string // Rig name (empty for global agents like mayor/deacon)
	AgentState        string // spawning, working, done, stuck, escalated, idle, running, completing, retiring, nuked
	Incarnation       string // Opaque polecat lifetime ID; immutable until retirement/reuse
	HookBead          string // Currently pinned work bead ID
	CleanupStatus     string // ZFC: polecat self-reports git state (clean, has_uncommitted, has_stash, has_unpushed)
	ActiveMR          string // Currently active merge request bead ID (for traceability)
	NotificationLevel string // DND mode: verbose, normal, muted (default: normal)
	Mode              string // Execution mode: "" (normal) or "ralph" (Ralph Wiggum loop)
	// Note: RoleBead field removed - role definitions are now config-based.
	// See internal/config/roles/*.toml and config-based-roles.md.

	// Completion metadata fields (gt-x7t9).
	// Written by gt done, read by witness survey-workers to discover
	// completion state from beads instead of POLECAT_DONE mail.
	ExitType          string // COMPLETED, ESCALATED, DEFERRED, PHASE_COMPLETE (see witness.ExitType*)
	MRID              string // MR bead ID (if MR was created)
	Branch            string // Polecat working branch name
	LastSourceIssue   string // Last source/work bead ID, preserved after hook_bead is cleared
	MRFailed          bool   // True when MR creation was attempted but failed
	PushFailed        bool   // True when branch push to origin failed (gas-556)
	CompletionTime    string // RFC3339 timestamp of when gt done was called
	CompletionAttempt string // Opaque owner receipt for the current gt done process lease

	// Durable retirement journal. These fields remain generation-bound until
	// every destructive and post-cleanup phase has committed.
	RetirementVersion   string
	RetirementPhase     string
	RetirementHookBead  string
	RetirementLastIssue string
	RetirementWork      []AgentRetirementWorkReceipt
	RetirementClonePath string
	RetirementBranch    string
	RetirementGitHead   string
	RetirementGitState  string
	RetirementTargets   []string

	structuredAgentState string
	structuredHookBead   string
}

// AgentRetirementRecord is the durable, generation-bound cleanup receipt kept
// on an agent bead until every retirement phase has completed.
type AgentRetirementRecord struct {
	Version         string
	Phase           string
	HookBead        string
	LastSourceIssue string
	WorkReceipts    []AgentRetirementWorkReceipt
	ClonePath       string
	Branch          string
	GitHead         string
	GitState        string
	Targets         []string
}

// AgentRetirementWorkReceipt binds every authoritative work bead to the exact
// molecule attached when the generation was fenced.
type AgentRetirementWorkReceipt struct {
	WorkBead string `json:"work_bead"`
	Molecule string `json:"molecule,omitempty"`
}

const (
	AgentRetirementJournalVersion       = "1"
	AgentRetirementPhaseFenced          = "fenced"
	AgentRetirementPhaseSessionStopped  = "session-stopped"
	AgentRetirementPhaseLocalRemoved    = "local-removed"
	AgentRetirementPhaseWorkUnassigned  = "work-unassigned"
	AgentRetirementPhaseNameReleased    = "name-released"
	AgentRetirementPhaseMoleculeCleaned = "molecule-cleaned"
	AgentRetirementPhaseBranchVerified  = "branch-verified"
	AgentRetirementPhaseBranchDeleted   = "branch-deleted"
	AgentRetirementPhaseCleanupComplete = "cleanup-complete"
)

func (f *AgentFields) RetirementRecord() AgentRetirementRecord {
	if f == nil {
		return AgentRetirementRecord{}
	}
	return AgentRetirementRecord{
		Version:         f.RetirementVersion,
		Phase:           f.RetirementPhase,
		HookBead:        f.RetirementHookBead,
		LastSourceIssue: f.RetirementLastIssue,
		WorkReceipts:    append([]AgentRetirementWorkReceipt(nil), f.RetirementWork...),
		ClonePath:       f.RetirementClonePath,
		Branch:          f.RetirementBranch,
		GitHead:         f.RetirementGitHead,
		GitState:        f.RetirementGitState,
		Targets:         append([]string(nil), f.RetirementTargets...),
	}
}

func applyAgentRetirementRecord(fields *AgentFields, record AgentRetirementRecord) {
	fields.RetirementVersion = record.Version
	fields.RetirementPhase = record.Phase
	fields.RetirementHookBead = record.HookBead
	fields.RetirementLastIssue = record.LastSourceIssue
	fields.RetirementWork = append([]AgentRetirementWorkReceipt(nil), record.WorkReceipts...)
	fields.RetirementClonePath = record.ClonePath
	fields.RetirementBranch = record.Branch
	fields.RetirementGitHead = record.GitHead
	fields.RetirementGitState = record.GitState
	fields.RetirementTargets = append([]string(nil), record.Targets...)
}

var agentRetirementPhases = []string{
	AgentRetirementPhaseFenced,
	AgentRetirementPhaseSessionStopped,
	AgentRetirementPhaseLocalRemoved,
	AgentRetirementPhaseWorkUnassigned,
	AgentRetirementPhaseMoleculeCleaned,
	AgentRetirementPhaseBranchVerified,
	AgentRetirementPhaseBranchDeleted,
	AgentRetirementPhaseNameReleased,
	AgentRetirementPhaseCleanupComplete,
}

func agentRetirementPhaseIndex(phase string) int {
	for i, candidate := range agentRetirementPhases {
		if phase == candidate {
			return i
		}
	}
	return -1
}

// ValidateAgentRetirementTransition permits only the next durable cleanup phase.
func ValidateAgentRetirementTransition(current, next string) error {
	currentIndex, nextIndex := agentRetirementPhaseIndex(current), agentRetirementPhaseIndex(next)
	if currentIndex < 0 || nextIndex != currentIndex+1 {
		return fmt.Errorf("invalid retirement transition %q -> %q", current, next)
	}
	return nil
}

// ValidateAgentRetirementRecord rejects incomplete or internally inconsistent
// durable cleanup receipts before any destructive retirement work resumes.
func ValidateAgentRetirementRecord(record AgentRetirementRecord) error {
	if record.Version != AgentRetirementJournalVersion {
		return fmt.Errorf("unsupported retirement journal version %q", record.Version)
	}
	if agentRetirementPhaseIndex(record.Phase) < 0 {
		return fmt.Errorf("unknown retirement phase %q", record.Phase)
	}
	if record.ClonePath == "" || !filepath.IsAbs(record.ClonePath) || filepath.Clean(record.ClonePath) != record.ClonePath {
		return fmt.Errorf("retirement clone path is not canonical: %q", record.ClonePath)
	}
	if strings.TrimSpace(record.GitState) == "" {
		return errors.New("retirement Git receipt is missing")
	}
	if !json.Valid([]byte(record.GitState)) {
		return errors.New("retirement Git receipt is malformed")
	}
	var gitState struct {
		WorktreePresent *bool
	}
	if err := json.Unmarshal([]byte(record.GitState), &gitState); err != nil || gitState.WorktreePresent == nil {
		return errors.New("retirement Git receipt lacks positive custody")
	}
	authoritative := make(map[string]bool, 2)
	for _, work := range []string{record.HookBead, record.LastSourceIssue} {
		if work = strings.TrimSpace(work); work != "" {
			authoritative[work] = true
		}
	}
	seen := make(map[string]bool, len(record.WorkReceipts))
	previous := ""
	for _, receipt := range record.WorkReceipts {
		work := strings.TrimSpace(receipt.WorkBead)
		if work == "" || seen[work] || !authoritative[work] || (previous != "" && work < previous) {
			return errors.New("retirement work receipts are not the canonical authoritative set")
		}
		seen[work], previous = true, work
	}
	if len(seen) != len(authoritative) {
		return errors.New("retirement work receipts are incomplete")
	}
	for work := range authoritative {
		if !seen[work] {
			return errors.New("retirement work receipts are incomplete")
		}
	}
	if strings.TrimSpace(record.Branch) == "" {
		if strings.TrimSpace(record.GitHead) != "" || len(record.Targets) != 0 {
			return errors.New("branchless retirement has branch identity receipts")
		}
	} else if strings.TrimSpace(record.GitHead) == "" || len(record.Targets) == 0 {
		return errors.New("retirement branch identity receipt is incomplete")
	}
	previous = ""
	for _, target := range record.Targets {
		if target = strings.TrimSpace(target); target == "" || (previous != "" && target <= previous) {
			return errors.New("retirement branch targets are not canonical")
		}
		if record.Branch != "" && (target == record.Branch || target == "refs/heads/"+record.Branch) {
			return errors.New("retirement branch cannot preserve itself")
		}
		previous = target
	}
	return nil
}

// LifecycleExpectations captures both compatibility description fields and
// authoritative structured columns for a generation-bound update.
func (fields *AgentFields) LifecycleExpectations() AgentFieldExpectations {
	if fields == nil {
		return AgentFieldExpectations{}
	}
	return AgentFieldExpectations{
		AgentState:           agentStringPointer(fields.AgentState),
		Incarnation:          agentStringPointer(fields.Incarnation),
		CleanupStatus:        agentStringPointer(fields.CleanupStatus),
		ActiveMR:             agentStringPointer(fields.ActiveMR),
		Mode:                 agentStringPointer(fields.Mode),
		HookBead:             agentStringPointer(fields.HookBead),
		ExitType:             agentStringPointer(fields.ExitType),
		MRID:                 agentStringPointer(fields.MRID),
		Branch:               agentStringPointer(fields.Branch),
		LastSourceIssue:      agentStringPointer(fields.LastSourceIssue),
		MRFailed:             agentBoolPointer(fields.MRFailed),
		PushFailed:           agentBoolPointer(fields.PushFailed),
		CompletionTime:       agentStringPointer(fields.CompletionTime),
		CompletionAttempt:    agentStringPointer(fields.CompletionAttempt),
		structuredAgentState: agentStringPointer(fields.structuredAgentState),
		structuredHookBead:   agentStringPointer(fields.structuredHookBead),
	}
}

func agentStringPointer(value string) *string { return &value }
func agentBoolPointer(value bool) *bool       { return &value }

// Notification level constants
const (
	NotifyVerbose = "verbose" // All notifications (mail, convoy events, etc.)
	NotifyNormal  = "normal"  // Important events only (default)
	NotifyMuted   = "muted"   // Silent/DND mode - batch for later
)

// FormatAgentDescription creates a description string from agent fields.
func FormatAgentDescription(title string, fields *AgentFields) string {
	if fields == nil {
		return title
	}

	var lines []string
	lines = append(lines, title)
	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("role_type: %s", fields.RoleType))

	if fields.Rig != "" {
		lines = append(lines, fmt.Sprintf("rig: %s", fields.Rig))
	} else {
		lines = append(lines, "rig: null")
	}

	lines = append(lines, fmt.Sprintf("agent_state: %s", fields.AgentState))
	if fields.Incarnation != "" {
		lines = append(lines, fmt.Sprintf("incarnation: %s", fields.Incarnation))
	}

	if fields.HookBead != "" {
		lines = append(lines, fmt.Sprintf("hook_bead: %s", fields.HookBead))
	} else {
		lines = append(lines, "hook_bead: null")
	}

	// Note: role_bead field no longer written - role definitions are config-based

	if fields.CleanupStatus != "" {
		lines = append(lines, fmt.Sprintf("cleanup_status: %s", fields.CleanupStatus))
	} else {
		lines = append(lines, "cleanup_status: null")
	}

	if fields.ActiveMR != "" {
		lines = append(lines, fmt.Sprintf("active_mr: %s", fields.ActiveMR))
	} else {
		lines = append(lines, "active_mr: null")
	}

	if fields.NotificationLevel != "" {
		lines = append(lines, fmt.Sprintf("notification_level: %s", fields.NotificationLevel))
	} else {
		lines = append(lines, "notification_level: null")
	}

	if fields.Mode != "" {
		lines = append(lines, fmt.Sprintf("mode: %s", fields.Mode))
	}

	// Completion metadata fields (gt-x7t9)
	if fields.ExitType != "" {
		lines = append(lines, fmt.Sprintf("exit_type: %s", fields.ExitType))
	}
	if fields.MRID != "" {
		lines = append(lines, fmt.Sprintf("mr_id: %s", fields.MRID))
	}
	if fields.Branch != "" {
		lines = append(lines, fmt.Sprintf("branch: %s", fields.Branch))
	}
	if fields.LastSourceIssue != "" {
		lines = append(lines, fmt.Sprintf("last_source_issue: %s", fields.LastSourceIssue))
	}
	if fields.MRFailed {
		lines = append(lines, "mr_failed: true")
	}
	if fields.PushFailed {
		lines = append(lines, "push_failed: true")
	}
	if fields.CompletionTime != "" {
		lines = append(lines, fmt.Sprintf("completion_time: %s", fields.CompletionTime))
	}
	if fields.CompletionAttempt != "" {
		lines = append(lines, fmt.Sprintf("completion_attempt: %s", fields.CompletionAttempt))
	}
	if fields.RetirementVersion != "" {
		lines = append(lines, fmt.Sprintf("retirement_version: %s", fields.RetirementVersion))
	}
	if fields.RetirementPhase != "" {
		lines = append(lines, fmt.Sprintf("retirement_phase: %s", fields.RetirementPhase))
	}
	if fields.RetirementHookBead != "" {
		lines = append(lines, fmt.Sprintf("retirement_hook_bead: %s", fields.RetirementHookBead))
	}
	if fields.RetirementLastIssue != "" {
		lines = append(lines, fmt.Sprintf("retirement_last_source_issue: %s", fields.RetirementLastIssue))
	}
	if len(fields.RetirementWork) > 0 {
		if encoded, err := json.Marshal(fields.RetirementWork); err == nil {
			lines = append(lines, fmt.Sprintf("retirement_work_receipts: %s", encoded))
		}
	}
	if fields.RetirementClonePath != "" {
		lines = append(lines, fmt.Sprintf("retirement_clone_path: %s", fields.RetirementClonePath))
	}
	if fields.RetirementBranch != "" {
		lines = append(lines, fmt.Sprintf("retirement_branch: %s", fields.RetirementBranch))
	}
	if fields.RetirementGitHead != "" {
		lines = append(lines, fmt.Sprintf("retirement_git_head: %s", fields.RetirementGitHead))
	}
	if fields.RetirementGitState != "" {
		lines = append(lines, fmt.Sprintf("retirement_git_state: %s", fields.RetirementGitState))
	}
	if len(fields.RetirementTargets) > 0 {
		if encoded, err := json.Marshal(fields.RetirementTargets); err == nil {
			lines = append(lines, fmt.Sprintf("retirement_targets: %s", encoded))
		}
	}

	return strings.Join(lines, "\n")
}

// ParseAgentFields extracts agent fields from an issue's description.
func ParseAgentFields(description string) *AgentFields {
	fields := &AgentFields{}

	for _, line := range strings.Split(description, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		colonIdx := strings.Index(line, ":")
		if colonIdx == -1 {
			continue
		}

		key := strings.TrimSpace(line[:colonIdx])
		value := strings.TrimSpace(line[colonIdx+1:])
		if value == "null" || value == "" {
			value = ""
		}

		switch strings.ToLower(key) {
		case "role_type":
			fields.RoleType = value
		case "rig":
			fields.Rig = value
		case "agent_state":
			fields.AgentState = value
		case "incarnation":
			fields.Incarnation = value
		case "hook_bead":
			fields.HookBead = value
		case "cleanup_status":
			fields.CleanupStatus = value
		case "active_mr":
			fields.ActiveMR = value
		case "notification_level":
			fields.NotificationLevel = value
		case "mode":
			fields.Mode = value
		// Completion metadata fields (gt-x7t9)
		case "exit_type":
			fields.ExitType = value
		case "mr_id":
			fields.MRID = value
		case "branch":
			fields.Branch = value
		case "last_source_issue":
			fields.LastSourceIssue = value
		case "mr_failed":
			fields.MRFailed = value == "true"
		case "push_failed":
			fields.PushFailed = value == "true"
		case "completion_time":
			fields.CompletionTime = value
		case "completion_attempt":
			fields.CompletionAttempt = value
		case "retirement_version":
			fields.RetirementVersion = value
		case "retirement_phase":
			fields.RetirementPhase = value
		case "retirement_hook_bead":
			fields.RetirementHookBead = value
		case "retirement_last_source_issue":
			fields.RetirementLastIssue = value
		case "retirement_work_receipts":
			if err := json.Unmarshal([]byte(value), &fields.RetirementWork); err != nil {
				fields.RetirementWork = []AgentRetirementWorkReceipt{{}}
			}
		case "retirement_clone_path":
			fields.RetirementClonePath = value
		case "retirement_branch":
			fields.RetirementBranch = value
		case "retirement_git_head":
			fields.RetirementGitHead = value
		case "retirement_git_state":
			fields.RetirementGitState = value
		case "retirement_targets":
			if err := json.Unmarshal([]byte(value), &fields.RetirementTargets); err != nil {
				fields.RetirementTargets = []string{""}
			}
		}
	}

	return fields
}

// CreateAgentBead creates an agent bead for tracking agent lifecycle.
// The ID format is: <prefix>-<rig>-<role>-<name> (e.g., gt-gastown-polecat-Toast)
// Use AgentBeadID() helper to generate correct IDs.
// The created_by field is populated from BD_ACTOR env var for provenance tracking.
//
// This function automatically ensures custom types are configured in the target
// database before creating the bead. This handles multi-repo routing scenarios
// where the bead may be routed to a different database than the one this wrapper
// is connected to.
func (b *Beads) CreateAgentBead(id, title string, fields *AgentFields) (*Issue, error) {
	// Guard against flag-like titles (gt-e0kx5: --help garbage beads)
	if IsFlagLikeTitle(title) {
		return nil, fmt.Errorf("refusing to create agent bead: %w (got %q)", ErrFlagTitle, title)
	}

	target := b.agentBeadTarget()
	targetDir := target.getResolvedBeadsDir()

	description := FormatAgentDescription(title, fields)
	if issue, err := target.createAgentBeadViaStore(context.Background(), id, title, description); err == nil {
		return issue, nil
	}

	// Ensure target database has custom types configured before falling back to
	// the bd CLI. The store path above avoids stale external bd schema during
	// fresh install; this remains for older stores or non-server configurations.
	_ = EnsureCustomTypes(targetDir)

	buildArgs := func() []string {
		a := []string{"create", "--json",
			"--id=" + id,
			"--title=" + title,
			"--description=" + description,
			"--type=task",
			"--labels=gt:agent",
		}
		if NeedsForceForID(id) {
			a = append(a, "--force")
		}
		// Default actor from BD_ACTOR env var for provenance tracking
		// Uses getActor() to respect isolated mode (tests)
		if actor := target.getActor(); actor != "" {
			a = append(a, "--actor="+actor)
		}
		return a
	}

	out, err := target.run(buildArgs()...)
	if err != nil {
		out, err = target.run(buildArgs()...)
		if err != nil {
			return nil, fmt.Errorf("creating %s: bd create failed: %w", id, err)
		}
	}

	var issue Issue
	if err := json.Unmarshal(out, &issue); err != nil {
		return nil, fmt.Errorf("parsing bd create output: %w", err)
	}

	// Note: role slot no longer set - role definitions are config-based
	// Note: hook_bead slot no longer set - bd slot removed in v0.62 (hq-l6mm5)

	return &issue, nil
}

func (b *Beads) createAgentBeadViaStore(ctx context.Context, id, title, description string) (*Issue, error) {
	store, cleanup, err := b.OpenStore(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	now := time.Now().UTC()
	actor := b.getActor()
	issue := &beadsdk.Issue{
		ID:          id,
		Title:       title,
		Description: description,
		Status:      beadsdk.StatusOpen,
		Priority:    2,
		IssueType:   beadsdk.TypeTask,
		CreatedAt:   now,
		UpdatedAt:   now,
		CreatedBy:   actor,
		Labels:      []string{"gt:agent"},
	}
	if err := store.CreateIssue(ctx, issue, actor); err != nil {
		return nil, err
	}
	return sdkIssueToIssue(issue), nil
}

// CreateOrReopenAgentBead creates an agent bead or reopens an existing one.
// This handles the case where a polecat is nuked and re-spawned with the same name:
// the old agent bead exists (open or closed), so we update it instead of
// failing with a UNIQUE constraint error.
//
// The function:
// 1. Tries to create the agent bead
// 2. If create fails, checks if bead exists (via bd show)
// 3. If bead exists and is closed, reopens it
// 4. Updates the bead with new fields regardless of prior state
//
// This is robust against Dolt backend issues where bd close/reopen may fail:
// - If nuke used ResetAgentBeadForReuse, bead is open → update directly
// - If bead is closed (legacy state), reopen then update
// - If bead is in unknown state, falls back to show+update
func (b *Beads) CreateOrReopenAgentBead(id, title string, fields *AgentFields) (*Issue, error) {
	// First try to create the bead (no lock needed - create is atomic)
	issue, err := b.CreateAgentBead(id, title, fields)
	if err == nil {
		return issue, nil
	}

	// Create failed - need to do Show→Reopen→Update which requires locking
	// to prevent concurrent modifications (e.g., nuke clearing fields while
	// spawn is updating them). See gt-joazs.
	fl, lockErr := b.lockAgentBead(id)
	if lockErr != nil {
		return nil, fmt.Errorf("locking agent bead %s: %w", id, lockErr)
	}
	defer func() { _ = fl.Unlock() }()

	// Create failed - check if bead already exists (handles both open and closed states)
	createErr := err

	target := b.agentBeadTarget()

	existing, showErr := target.Show(id)
	if showErr != nil {
		// Bead doesn't exist (or can't be read) - return original create error
		return nil, createErr
	}
	existingFields := agentFieldsFromIssue(existing)
	if existingFields != nil && (existingFields.AgentState == string(AgentStateRetiring) || existingFields.AgentState == string(AgentStateCompleting)) {
		return nil, fmt.Errorf("%w: agent_state", ErrAgentFieldsChanged)
	}

	// If bead is closed, reopen it first
	if existing.Status == "closed" {
		if _, reopenErr := target.run("reopen", id, "--reason=re-spawning agent"); reopenErr != nil {
			// Reopen failed - try setting status to open via update as fallback
			// This handles Dolt backends where bd reopen may not work
			openStatus := "open"
			if updateErr := target.Update(id, UpdateOptions{Status: &openStatus}); updateErr != nil {
				return nil, fmt.Errorf("could not reopen agent bead %s (reopen: %v, update: %v, original: %v)",
					id, reopenErr, updateErr, createErr)
			}
		}
	}

	// Update the bead with new fields and ensure gt:agent label is set.
	// Agent beads use type=task (a valid built-in type) and are identified
	// by the gt:agent label, not by type (see IsAgentBead).
	description := FormatAgentDescription(title, fields)
	updateOpts := UpdateOptions{
		Title:       &title,
		Description: &description,
		SetLabels:   labelsForAgentBeadReuse(existing.Labels),
	}
	if err := target.Update(id, updateOpts); err != nil {
		return nil, fmt.Errorf("updating agent bead: %w", err)
	}

	// Note: role slot no longer set - role definitions are config-based
	// Note: hook_bead slot no longer set - bd slot removed in v0.62 (hq-l6mm5)

	// Return the updated bead
	return target.Show(id)
}

func labelsForAgentBeadReuse(existing []string) []string {
	labels := []string{"gt:agent"}
	seen := map[string]bool{"gt:agent": true}
	for _, label := range existing {
		if !strings.HasPrefix(label, "safety_stop:") || seen[label] {
			continue
		}
		labels = append(labels, label)
		seen[label] = true
	}
	return labels
}

// ResetAgentBeadForReuse clears all mutable fields on an agent bead without closing it.
// This is the preferred cleanup method during polecat nuke because it avoids the
// close/reopen cycle that fails on Dolt backends (tombstone operations not supported,
// bd reopen failures). By keeping the bead open with agent_state="nuked",
// CreateOrReopenAgentBead can simply update it on re-spawn without needing reopen.
//
// This is the standard nuke path (gt-14b8o).
func (b *Beads) ResetAgentBeadForReuse(id, reason string) error {
	return b.resetAgentBeadForReuse(id, reason, nil, nil)
}

// ResetAgentBeadForReuseIfIncarnation retires only the exact polecat lifetime
// observed by the caller. A same-name replacement is preserved even when all
// other mutable agent fields happen to have the same values (the ABA case).
func (b *Beads) ResetAgentBeadForReuseIfIncarnation(id, reason, expectedIncarnation string) error {
	expectedIncarnation = strings.TrimSpace(expectedIncarnation)
	if expectedIncarnation == "" {
		return fmt.Errorf("%w: incarnation", ErrAgentFieldsChanged)
	}
	expected := AgentFieldExpectations{Incarnation: &expectedIncarnation}
	return b.resetAgentBeadForReuse(id, reason, &expected, nil)
}

// ResetAgentBeadForReuseIfUnchanged retires only the exact lifecycle snapshot
// observed by the caller. Mutable same-incarnation work cannot be erased.
func (b *Beads) ResetAgentBeadForReuseIfUnchanged(id, reason string, expected AgentFieldExpectations) error {
	return b.ResetAgentBeadForReuseIfUnchangedAfter(id, reason, expected, nil)
}

// ResetAgentBeadForReuseIfUnchangedAfter runs the final destructive step while
// the exact lifecycle snapshot is locked, then retires that same generation.
func (b *Beads) ResetAgentBeadForReuseIfUnchangedAfter(
	id, reason string,
	expected AgentFieldExpectations,
	beforeReset func() error,
) error {
	return b.ResetAgentBeadForReuseIfUnchangedRevalidatedAfter(id, reason, expected, nil, beforeReset)
}

// ResetAgentBeadForReuseIfUnchangedRevalidatedAfter performs the final live
// proof under the agent lock, durably fences the generation as retiring, runs
// the destructive callback, and only then clears the retired generation.
func (b *Beads) ResetAgentBeadForReuseIfUnchangedRevalidatedAfter(
	id, reason string,
	expected AgentFieldExpectations,
	revalidate func(*Issue, *AgentFields) error,
	beforeReset func() error,
) error {
	if expected.AgentState == nil || expected.Incarnation == nil || expected.CleanupStatus == nil ||
		expected.ActiveMR == nil || expected.Mode == nil || expected.HookBead == nil ||
		expected.ExitType == nil || expected.MRID == nil || expected.Branch == nil ||
		expected.LastSourceIssue == nil || expected.MRFailed == nil || expected.PushFailed == nil ||
		expected.CompletionTime == nil || expected.CompletionAttempt == nil || expected.structuredAgentState == nil || expected.structuredHookBead == nil ||
		strings.TrimSpace(*expected.Incarnation) == "" {
		return fmt.Errorf("%w: incomplete lifecycle snapshot", ErrAgentFieldsChanged)
	}
	return b.resetAgentBeadForReuseRevalidated(id, reason, &expected, revalidate, beforeReset)
}

func (b *Beads) resetAgentBeadForReuse(
	id, reason string,
	expected *AgentFieldExpectations,
	beforeReset func() error,
) error {
	return b.resetAgentBeadForReuseRevalidated(id, reason, expected, nil, beforeReset)
}

func (b *Beads) resetAgentBeadForReuseRevalidated(
	id, reason string,
	expected *AgentFieldExpectations,
	revalidate func(*Issue, *AgentFields) error,
	beforeReset func() error,
) (retErr error) {
	// Lock the agent bead to prevent concurrent read-modify-write races.
	// Without this, a concurrent CreateOrReopenAgentBead could overwrite
	// the nuked state we're about to set. See gt-joazs.
	fl, lockErr := b.lockAgentBead(id)
	if lockErr != nil {
		return fmt.Errorf("locking agent bead %s: %w", id, lockErr)
	}
	defer func() { _ = fl.Unlock() }()

	target := b.agentBeadTarget()

	// Get current issue to preserve immutable fields (title, role_type, rig)
	issue, err := target.Show(id)
	if err != nil {
		return err
	}

	// Parse existing fields and clear mutable ones
	fields := agentFieldsFromIssue(issue)
	if expected != nil {
		if err := checkAgentFieldExpectationsAllowRetiring(fields, *expected); err != nil {
			return err
		}
	}
	if revalidate != nil {
		if err := revalidate(issue, fields); err != nil {
			return err
		}
	}
	fenced := fields.AgentState == string(AgentStateRetiring)
	if !fenced {
		fields.AgentState = string(AgentStateRetiring)
		description := FormatAgentDescription(issue.Title, fields)
		if err := target.Update(id, UpdateOptions{Description: &description}); err != nil {
			return fmt.Errorf("fencing retiring agent generation: %w", err)
		}
		fenced = true
	}
	defer func() {
		if retErr != nil && fenced {
			retErr = errors.Join(ErrAgentRetirementFenced, retErr)
		}
	}()
	if beforeReset != nil {
		if err := beforeReset(); err != nil {
			return err
		}
	}
	fields.HookBead = ""      // Clear hook_bead
	fields.ActiveMR = ""      // Clear active_mr
	fields.CleanupStatus = "" // Clear cleanup_status
	fields.Mode = ""          // Clear Ralph-mode threshold marker
	fields.Incarnation = ""   // Retired generation must never match a stale lifecycle proof
	fields.AgentState = string(AgentStateNuked)
	// Clear completion metadata (gt-x7t9)
	fields.ExitType = ""
	fields.MRID = ""
	fields.Branch = ""
	fields.LastSourceIssue = ""
	fields.MRFailed = false
	fields.PushFailed = false
	fields.CompletionTime = ""
	fields.CompletionAttempt = ""

	// Update description with cleared fields
	description := FormatAgentDescription(issue.Title, fields)
	if err := target.Update(id, UpdateOptions{Description: &description}); err != nil {
		return fmt.Errorf("resetting agent bead fields: %w", err)
	}

	// Hook slot no longer maintained (hq-l6mm5) — no need to clear.

	return nil
}

// RetireAgentGeneration durably records an exact generation's cleanup inputs,
// runs resumable cleanup while holding the agent-bead lock, and clears identity
// only after the caller commits cleanup-complete.
func (b *Beads) RetireAgentGeneration(
	id string,
	expected AgentFieldExpectations,
	record AgentRetirementRecord,
	revalidate func(*Issue, *AgentFields) error,
	cleanup func(AgentRetirementRecord, func(string) error) error,
) (retErr error) {
	if target := b.agentBeadTarget(); target != b {
		return target.RetireAgentGeneration(id, expected, record, revalidate, cleanup)
	}
	if expected.Incarnation == nil || strings.TrimSpace(*expected.Incarnation) == "" {
		return fmt.Errorf("%w: incarnation", ErrAgentFieldsChanged)
	}
	fl, err := b.lockAgentBead(id)
	if err != nil {
		return fmt.Errorf("locking agent bead %s: %w", id, err)
	}
	defer func() { _ = fl.Unlock() }()

	issue, err := b.Show(id)
	if err != nil {
		return err
	}
	fields := agentFieldsFromIssue(issue)
	if fields == nil || fields.Incarnation != *expected.Incarnation {
		return fmt.Errorf("%w: incarnation", ErrAgentFieldsChanged)
	}
	fenced := fields.AgentState == string(AgentStateRetiring)
	if !fenced {
		if err := checkAgentFieldExpectationsAllowRetiring(fields, expected); err != nil {
			return err
		}
		if revalidate != nil {
			if err := revalidate(issue, fields); err != nil {
				return err
			}
		}
		record.Phase = AgentRetirementPhaseFenced
		if err := ValidateAgentRetirementRecord(record); err != nil {
			return err
		}
		fields.AgentState = string(AgentStateRetiring)
		applyAgentRetirementRecord(fields, record)
		description := FormatAgentDescription(issue.Title, fields)
		if err := b.Update(id, UpdateOptions{Description: &description}); err != nil {
			return fmt.Errorf("fencing retiring agent generation: %w", err)
		}
	} else {
		record = fields.RetirementRecord()
		if err := ValidateAgentRetirementRecord(record); err != nil {
			return fmt.Errorf("invalid durable retirement journal: %w", err)
		}
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(ErrAgentRetirementFenced, retErr)
		}
	}()

	advance := func(phase string) error {
		if err := ValidateAgentRetirementTransition(record.Phase, phase); err != nil {
			return err
		}
		record.Phase = phase
		applyAgentRetirementRecord(fields, record)
		description := FormatAgentDescription(issue.Title, fields)
		if err := b.Update(id, UpdateOptions{Description: &description}); err != nil {
			return fmt.Errorf("advancing retirement to %s: %w", phase, err)
		}
		return nil
	}
	if cleanup != nil {
		if err := cleanup(record, advance); err != nil {
			return err
		}
	}
	if record.Phase != AgentRetirementPhaseCleanupComplete {
		return errors.New("retirement cleanup did not commit cleanup-complete")
	}

	fields.HookBead = ""
	fields.ActiveMR = ""
	fields.CleanupStatus = ""
	fields.Mode = ""
	fields.Incarnation = ""
	fields.AgentState = string(AgentStateNuked)
	fields.ExitType = ""
	fields.MRID = ""
	fields.Branch = ""
	fields.LastSourceIssue = ""
	fields.MRFailed = false
	fields.PushFailed = false
	fields.CompletionTime = ""
	fields.CompletionAttempt = ""
	applyAgentRetirementRecord(fields, AgentRetirementRecord{})
	description := FormatAgentDescription(issue.Title, fields)
	if err := b.Update(id, UpdateOptions{Description: &description}); err != nil {
		return fmt.Errorf("resetting retired agent generation: %w", err)
	}
	return nil
}

// ClaimAgentCompletion atomically changes a matching live generation to the
// completing state before gt done performs any external mutation.
func (b *Beads) ClaimAgentCompletion(id, expectedIncarnation, attempt string) error {
	if target := b.agentBeadTarget(); target != b {
		return target.ClaimAgentCompletion(id, expectedIncarnation, attempt)
	}
	expectedIncarnation = strings.TrimSpace(expectedIncarnation)
	attempt = strings.TrimSpace(attempt)
	if expectedIncarnation == "" || attempt == "" {
		return fmt.Errorf("%w: completion receipt", ErrAgentFieldsChanged)
	}
	fl, err := b.lockAgentBead(id)
	if err != nil {
		return fmt.Errorf("locking agent bead %s: %w", id, err)
	}
	defer func() { _ = fl.Unlock() }()
	issue, err := b.Show(id)
	if err != nil {
		return err
	}
	fields := agentFieldsFromIssue(issue)
	if fields == nil || fields.Incarnation != expectedIncarnation {
		return fmt.Errorf("%w: incarnation", ErrAgentFieldsChanged)
	}
	switch fields.AgentState {
	case string(AgentStateCompleting):
		// The manager's process lease proves the prior owner exited. Replace its
		// stale receipt so this exact attempt can resume checkpoints safely.
	case string(AgentStateWorking), string(AgentStateRunning), string(AgentStateSpawning):
	default:
		return fmt.Errorf("%w: agent_state", ErrAgentFieldsChanged)
	}
	fields.AgentState = string(AgentStateCompleting)
	fields.CompletionAttempt = attempt
	description := FormatAgentDescription(issue.Title, fields)
	return b.Update(id, UpdateOptions{Description: &description})
}

// UpdateAgentState updates the agent_state field in an agent bead.
// bd >= 0.62.0 no longer provides a supported `bd agent state` writer, so
// Gastown writes agent_state through the description field and readers mirror
// that contract with fallback to the legacy structured column via ResolveAgentState.
//
// Resolves the concrete target DB first so the update hits the correct database
// when the agent bead routes to a different beads dir via routes.jsonl.
func (b *Beads) UpdateAgentState(id string, state string) (retErr error) {
	defer func() { telemetry.RecordAgentStateChange(context.Background(), id, state, nil, retErr) }()
	target := b.agentBeadTarget()
	return target.UpdateAgentDescriptionFields(id, AgentFieldUpdates{AgentState: &state})
}

// SetHookBead and ClearHookBead removed (hq-l6mm5).
// Hook slot on agent beads is no longer maintained. Work bead status=hooked
// and assignee=<agent> is the authoritative source for hook tracking.

// AgentFieldUpdates specifies which agent description fields to update.
// Only non-nil fields are modified; nil fields are left unchanged.
// This allows multiple fields to be updated in a single read-modify-write
// cycle, avoiding races where concurrent callers overwrite each other's changes.
type AgentFieldUpdates struct {
	AgentState        *string // Sync description agent_state with column (gt-ulom)
	CleanupStatus     *string
	ActiveMR          *string
	NotificationLevel *string
	Mode              *string
	HookBead          *string // Clear hook_bead on completion (gt-qbh)
	// Completion metadata fields (gt-x7t9)
	ExitType          *string
	MRID              *string
	Branch            *string
	LastSourceIssue   *string
	MRFailed          *bool
	PushFailed        *bool // True when branch push to origin failed (gas-556)
	CompletionTime    *string
	CompletionAttempt *string
}

// AgentFieldExpectations specifies description fields that must still have
// their observed values before an update may be written. Nil fields are not
// compared.
type AgentFieldExpectations struct {
	AgentState           *string
	Incarnation          *string
	CleanupStatus        *string
	ActiveMR             *string
	Mode                 *string
	HookBead             *string
	ExitType             *string
	MRID                 *string
	Branch               *string
	LastSourceIssue      *string
	MRFailed             *bool
	PushFailed           *bool
	CompletionTime       *string
	CompletionAttempt    *string
	structuredAgentState *string
	structuredHookBead   *string
}

// ErrAgentFieldsChanged reports that an agent bead changed after the caller
// observed it, so the guarded mutation was not written.
var ErrAgentFieldsChanged = errors.New("agent description fields changed")

// ErrAgentRetirementFenced reports that the old generation is durably barred
// from lifecycle writes and the interrupted retirement must be resumed.
var ErrAgentRetirementFenced = errors.New("agent retirement durably fenced")

func validateAgentFieldUpdates(updates AgentFieldUpdates) error {
	if updates.NotificationLevel == nil {
		return nil
	}
	level := *updates.NotificationLevel
	if level != "" && level != NotifyVerbose && level != NotifyNormal && level != NotifyMuted {
		return fmt.Errorf("invalid notification level %q: must be verbose, normal, or muted", level)
	}
	return nil
}

// UpdateAgentDescriptionFields atomically updates one or more agent description
// fields in a single Show-Parse-Modify-Update cycle. This prevents the race
// condition where concurrent callers updating different fields overwrite each
// other because the entire description is replaced.
func (b *Beads) UpdateAgentDescriptionFields(id string, updates AgentFieldUpdates) error {
	if target := b.agentBeadTarget(); target != b {
		return target.UpdateAgentDescriptionFields(id, updates)
	}
	if err := validateAgentFieldUpdates(updates); err != nil {
		return err
	}

	// Lock the agent bead to prevent concurrent read-modify-write races.
	// Without this, concurrent callers updating different fields could overwrite
	// each other's changes. See gt-joazs.
	fl, lockErr := b.lockAgentBead(id)
	if lockErr != nil {
		return fmt.Errorf("locking agent bead %s: %w", id, lockErr)
	}
	defer func() { _ = fl.Unlock() }()

	return b.updateAgentDescriptionFieldsLocked(id, nil, updates, nil, false)
}

// UpdateAgentDescriptionFieldsIfIncarnation applies lifecycle writes only to
// the generation that the caller originally observed.
func (b *Beads) UpdateAgentDescriptionFieldsIfIncarnation(
	id, expectedIncarnation string,
	updates AgentFieldUpdates,
) error {
	if strings.TrimSpace(expectedIncarnation) == "" {
		return fmt.Errorf("%w: incarnation", ErrAgentFieldsChanged)
	}
	return b.CompareAndUpdateAgentDescriptionFields(
		id,
		AgentFieldExpectations{Incarnation: &expectedIncarnation},
		updates,
	)
}

// UpdateAgentDescriptionFieldsIfIncarnationContext is the cancellation-aware
// form used by lifecycle control-plane operations.
func (b *Beads) UpdateAgentDescriptionFieldsIfIncarnationContext(
	ctx context.Context,
	id, expectedIncarnation string,
	updates AgentFieldUpdates,
) error {
	if strings.TrimSpace(expectedIncarnation) == "" {
		return fmt.Errorf("%w: incarnation", ErrAgentFieldsChanged)
	}
	if target := b.agentBeadTarget(); target != b {
		return target.UpdateAgentDescriptionFieldsIfIncarnationContext(ctx, id, expectedIncarnation, updates)
	}
	if err := validateAgentFieldUpdates(updates); err != nil {
		return err
	}
	fl, err := b.lockAgentBeadContext(ctx, id)
	if err != nil {
		return fmt.Errorf("locking agent bead %s: %w", id, err)
	}
	defer func() { _ = fl.Unlock() }()
	return b.updateAgentDescriptionFieldsLockedContext(
		ctx,
		id,
		&AgentFieldExpectations{Incarnation: &expectedIncarnation},
		updates,
		nil,
		false,
	)
}

// CompareAndUpdateAgentDescriptionFields updates an agent description only if
// every supplied expectation still matches inside the agent bead lock.
func (b *Beads) CompareAndUpdateAgentDescriptionFields(
	id string,
	expected AgentFieldExpectations,
	updates AgentFieldUpdates,
) (retErr error) {
	return b.compareRevalidateAndUpdateAgentDescriptionFields(id, expected, updates, nil)
}

// CompareRevalidateAndUpdateAgentDescriptionFields performs a final live
// revalidation while the same agent lock protects the guarded update.
func (b *Beads) CompareRevalidateAndUpdateAgentDescriptionFields(
	id string,
	expected AgentFieldExpectations,
	updates AgentFieldUpdates,
	revalidate func(*Issue, *AgentFields) error,
) error {
	return b.compareRevalidateAndUpdateAgentDescriptionFields(id, expected, updates, revalidate)
}

func (b *Beads) compareRevalidateAndUpdateAgentDescriptionFields(
	id string,
	expected AgentFieldExpectations,
	updates AgentFieldUpdates,
	revalidate func(*Issue, *AgentFields) error,
) (retErr error) {
	if target := b.agentBeadTarget(); target != b {
		return target.compareRevalidateAndUpdateAgentDescriptionFields(id, expected, updates, revalidate)
	}
	if err := validateAgentFieldUpdates(updates); err != nil {
		return err
	}
	if updates.AgentState != nil {
		defer func() {
			telemetry.RecordAgentStateChange(context.Background(), id, *updates.AgentState, nil, retErr)
		}()
	}

	fl, lockErr := b.lockAgentBead(id)
	if lockErr != nil {
		return fmt.Errorf("locking agent bead %s: %w", id, lockErr)
	}
	defer func() { _ = fl.Unlock() }()

	return b.updateAgentDescriptionFieldsLocked(id, &expected, updates, revalidate, false)
}

// UpdateAgentDescriptionFieldsIfCompletionOwner permits lifecycle writes only
// for the exact process attempt that owns the durable completing generation.
func (b *Beads) UpdateAgentDescriptionFieldsIfCompletionOwner(
	id, expectedIncarnation, expectedAttempt string,
	updates AgentFieldUpdates,
) error {
	if target := b.agentBeadTarget(); target != b {
		return target.UpdateAgentDescriptionFieldsIfCompletionOwner(id, expectedIncarnation, expectedAttempt, updates)
	}
	if strings.TrimSpace(expectedIncarnation) == "" || strings.TrimSpace(expectedAttempt) == "" {
		return fmt.Errorf("%w: completion owner", ErrAgentFieldsChanged)
	}
	if err := validateAgentFieldUpdates(updates); err != nil {
		return err
	}
	fl, err := b.lockAgentBead(id)
	if err != nil {
		return fmt.Errorf("locking agent bead %s: %w", id, err)
	}
	defer func() { _ = fl.Unlock() }()
	state := string(AgentStateCompleting)
	expected := AgentFieldExpectations{
		AgentState:        &state,
		Incarnation:       &expectedIncarnation,
		CompletionAttempt: &expectedAttempt,
	}
	return b.updateAgentDescriptionFieldsLocked(id, &expected, updates, nil, true)
}

func (b *Beads) updateAgentDescriptionFieldsLocked(
	id string,
	expected *AgentFieldExpectations,
	updates AgentFieldUpdates,
	revalidate func(*Issue, *AgentFields) error,
	allowCompletionOwner bool,
) error {
	return b.updateAgentDescriptionFieldsLockedContext(context.Background(), id, expected, updates, revalidate, allowCompletionOwner)
}

func (b *Beads) updateAgentDescriptionFieldsLockedContext(
	ctx context.Context,
	id string,
	expected *AgentFieldExpectations,
	updates AgentFieldUpdates,
	revalidate func(*Issue, *AgentFields) error,
	allowCompletionOwner bool,
) error {
	issue, err := b.ShowContext(ctx, id)
	if err != nil {
		return err
	}

	fields := agentFieldsFromIssue(issue)
	if fields != nil && agentLifecycleFrozen(fields.AgentState) && !allowCompletionOwner {
		return fmt.Errorf("%w: agent_state", ErrAgentFieldsChanged)
	}
	if expected != nil {
		if err := checkAgentFieldExpectations(fields, *expected); err != nil {
			return err
		}
	}
	if revalidate != nil {
		if err := revalidate(issue, fields); err != nil {
			return err
		}
	}

	if updates.AgentState != nil {
		fields.AgentState = *updates.AgentState
		if fields.AgentState != string(AgentStateCompleting) {
			fields.CompletionAttempt = ""
		}
	}
	if updates.CleanupStatus != nil {
		fields.CleanupStatus = *updates.CleanupStatus
	}
	if updates.ActiveMR != nil {
		fields.ActiveMR = *updates.ActiveMR
	}
	if updates.NotificationLevel != nil {
		fields.NotificationLevel = *updates.NotificationLevel
	}
	if updates.Mode != nil {
		fields.Mode = *updates.Mode
	}
	if updates.HookBead != nil {
		fields.HookBead = *updates.HookBead
	}
	// Completion metadata fields (gt-x7t9)
	if updates.ExitType != nil {
		fields.ExitType = *updates.ExitType
	}
	if updates.MRID != nil {
		fields.MRID = *updates.MRID
	}
	if updates.Branch != nil {
		fields.Branch = *updates.Branch
	}
	if updates.LastSourceIssue != nil {
		fields.LastSourceIssue = *updates.LastSourceIssue
	}
	if updates.MRFailed != nil {
		fields.MRFailed = *updates.MRFailed
	}
	if updates.PushFailed != nil {
		fields.PushFailed = *updates.PushFailed
	}
	if updates.CompletionTime != nil {
		fields.CompletionTime = *updates.CompletionTime
	}
	if updates.CompletionAttempt != nil {
		fields.CompletionAttempt = *updates.CompletionAttempt
	}

	description := FormatAgentDescription(issue.Title, fields)
	return b.UpdateContext(ctx, id, UpdateOptions{Description: &description})
}

func checkAgentFieldExpectations(fields *AgentFields, expected AgentFieldExpectations) error {
	if fields != nil && expected.Incarnation != nil && agentLifecycleFrozen(fields.AgentState) &&
		!(fields.AgentState == string(AgentStateCompleting) && expected.CompletionAttempt != nil) {
		return fmt.Errorf("%w: agent_state", ErrAgentFieldsChanged)
	}
	return checkAgentFieldExpectationsAllowRetiring(fields, expected)
}

func agentLifecycleFrozen(state string) bool {
	switch state {
	case string(AgentStateCompleting), string(AgentStateRetiring), string(AgentStateNuked):
		return true
	default:
		return false
	}
}

func checkAgentFieldExpectationsAllowRetiring(fields *AgentFields, expected AgentFieldExpectations) error {
	if fields == nil {
		return fmt.Errorf("%w: missing agent fields", ErrAgentFieldsChanged)
	}
	checks := []struct {
		name    string
		want    *string
		current string
	}{
		{name: "agent_state", want: expected.AgentState, current: fields.AgentState},
		{name: "incarnation", want: expected.Incarnation, current: fields.Incarnation},
		{name: "cleanup_status", want: expected.CleanupStatus, current: fields.CleanupStatus},
		{name: "active_mr", want: expected.ActiveMR, current: fields.ActiveMR},
		{name: "mode", want: expected.Mode, current: fields.Mode},
		{name: "hook_bead", want: expected.HookBead, current: fields.HookBead},
		{name: "exit_type", want: expected.ExitType, current: fields.ExitType},
		{name: "mr_id", want: expected.MRID, current: fields.MRID},
		{name: "branch", want: expected.Branch, current: fields.Branch},
		{name: "last_source_issue", want: expected.LastSourceIssue, current: fields.LastSourceIssue},
		{name: "completion_time", want: expected.CompletionTime, current: fields.CompletionTime},
		{name: "completion_attempt", want: expected.CompletionAttempt, current: fields.CompletionAttempt},
		{name: "structured_agent_state", want: expected.structuredAgentState, current: fields.structuredAgentState},
		{name: "structured_hook_bead", want: expected.structuredHookBead, current: fields.structuredHookBead},
	}
	for _, check := range checks {
		if check.want != nil && check.current != *check.want {
			return fmt.Errorf("%w: %s", ErrAgentFieldsChanged, check.name)
		}
	}
	boolChecks := []struct {
		name    string
		want    *bool
		current bool
	}{
		{name: "mr_failed", want: expected.MRFailed, current: fields.MRFailed},
		{name: "push_failed", want: expected.PushFailed, current: fields.PushFailed},
	}
	for _, check := range boolChecks {
		if check.want != nil && check.current != *check.want {
			return fmt.Errorf("%w: %s", ErrAgentFieldsChanged, check.name)
		}
	}
	return nil
}

// UpdateAgentCleanupStatus updates the cleanup_status field in an agent bead.
// This is called by the polecat to self-report its git state (ZFC compliance).
// Valid statuses: clean, has_uncommitted, has_stash, has_unpushed
func (b *Beads) UpdateAgentCleanupStatus(id string, cleanupStatus string) error {
	return b.UpdateAgentDescriptionFields(id, AgentFieldUpdates{CleanupStatus: &cleanupStatus})
}

// UpdateAgentActiveMR updates the active_mr field in an agent bead.
// This links the agent to their current merge request for traceability.
// Pass empty string to clear the field (e.g., after merge completes).
func (b *Beads) UpdateAgentActiveMR(id string, activeMR string) error {
	return b.UpdateAgentDescriptionFields(id, AgentFieldUpdates{ActiveMR: &activeMR})
}

// UpdateAgentActiveMRIfIncarnation prevents a stale polecat process from
// attaching an MR to a same-name replacement generation.
func (b *Beads) UpdateAgentActiveMRIfIncarnation(id, expectedIncarnation, activeMR string) error {
	return b.UpdateAgentDescriptionFieldsIfIncarnation(id, expectedIncarnation, AgentFieldUpdates{ActiveMR: &activeMR})
}

// UpdateAgentActiveMRIfCompletionOwner allows the exact completion attempt to
// persist its MR while ordinary writers remain frozen out of completing state.
func (b *Beads) UpdateAgentActiveMRIfCompletionOwner(id, expectedIncarnation, expectedAttempt, activeMR string) error {
	return b.UpdateAgentDescriptionFieldsIfCompletionOwner(
		id, expectedIncarnation, expectedAttempt, AgentFieldUpdates{ActiveMR: &activeMR},
	)
}

// UpdateAgentIfIncarnation applies non-description issue updates only when the
// agent bead still belongs to the caller's generation.
func (b *Beads) UpdateAgentIfIncarnation(id, expectedIncarnation string, updates UpdateOptions) error {
	if target := b.agentBeadTarget(); target != b {
		return target.UpdateAgentIfIncarnation(id, expectedIncarnation, updates)
	}
	expectedIncarnation = strings.TrimSpace(expectedIncarnation)
	if expectedIncarnation == "" {
		return fmt.Errorf("%w: incarnation", ErrAgentFieldsChanged)
	}
	fl, lockErr := b.lockAgentBead(id)
	if lockErr != nil {
		return fmt.Errorf("locking agent bead %s: %w", id, lockErr)
	}
	defer func() { _ = fl.Unlock() }()
	issue, err := b.Show(id)
	if err != nil {
		return err
	}
	if fields := agentFieldsFromIssue(issue); fields == nil || fields.Incarnation != expectedIncarnation || agentLifecycleFrozen(fields.AgentState) {
		return fmt.Errorf("%w: incarnation", ErrAgentFieldsChanged)
	}
	return b.Update(id, updates)
}

// CompareAndRestoreIssueStatusIfAssignee restores startup-mutated work state
// only while both the exact assignee and the state written by startup remain.
func (b *Beads) CompareAndRestoreIssueStatusIfAssignee(id, expectedStatus, expectedAssignee, restoreStatus string) error {
	return b.CompareAndRestoreIssueAssignmentIfMatches(id, expectedStatus, expectedAssignee, restoreStatus, expectedAssignee)
}

// CompareAndRestoreIssueAssignmentIfMatches restores status and assignee only
// while both values written by the failed transaction still match.
func (b *Beads) CompareAndRestoreIssueAssignmentIfMatches(id, expectedStatus, expectedAssignee, restoreStatus, restoreAssignee string) error {
	if !b.noRoute {
		if target := b.forIssueID(id); target != b {
			return target.CompareAndRestoreIssueAssignmentIfMatches(id, expectedStatus, expectedAssignee, restoreStatus, restoreAssignee)
		}
	}
	if handled, err := b.compareAndUpdateIssueAssignment(id, expectedStatus, expectedAssignee, restoreStatus, restoreAssignee); handled {
		return err
	}
	fl, err := b.lockAgentBead(id)
	if err != nil {
		return fmt.Errorf("locking work bead %s: %w", id, err)
	}
	defer func() { _ = fl.Unlock() }()
	issue, err := b.Show(id)
	if err != nil {
		return err
	}
	if issue.Status != expectedStatus || issue.Assignee != expectedAssignee {
		return fmt.Errorf("%w: work status or assignee", ErrAgentFieldsChanged)
	}
	return b.Update(id, UpdateOptions{Status: &restoreStatus, Assignee: &restoreAssignee})
}

// UpdateAgentIfCompletionOwner applies label/checkpoint writes for the exact
// completion attempt while ordinary issue writers remain frozen out.
func (b *Beads) UpdateAgentIfCompletionOwner(id, expectedIncarnation, expectedAttempt string, updates UpdateOptions) error {
	if target := b.agentBeadTarget(); target != b {
		return target.UpdateAgentIfCompletionOwner(id, expectedIncarnation, expectedAttempt, updates)
	}
	if strings.TrimSpace(expectedIncarnation) == "" || strings.TrimSpace(expectedAttempt) == "" {
		return fmt.Errorf("%w: completion owner", ErrAgentFieldsChanged)
	}
	fl, err := b.lockAgentBead(id)
	if err != nil {
		return fmt.Errorf("locking agent bead %s: %w", id, err)
	}
	defer func() { _ = fl.Unlock() }()
	issue, err := b.Show(id)
	if err != nil {
		return err
	}
	fields := agentFieldsFromIssue(issue)
	if fields == nil || fields.Incarnation != expectedIncarnation ||
		fields.AgentState != string(AgentStateCompleting) || fields.CompletionAttempt != expectedAttempt {
		return fmt.Errorf("%w: completion owner", ErrAgentFieldsChanged)
	}
	return b.Update(id, updates)
}

// FinalizeAgentCompletionIfOwner atomically releases the completing state and
// removes its recovery markers for the exact completion process owner.
func (b *Beads) FinalizeAgentCompletionIfOwner(id, expectedIncarnation, expectedAttempt string, finalState AgentState) error {
	if target := b.agentBeadTarget(); target != b {
		return target.FinalizeAgentCompletionIfOwner(id, expectedIncarnation, expectedAttempt, finalState)
	}
	if strings.TrimSpace(expectedIncarnation) == "" || strings.TrimSpace(expectedAttempt) == "" {
		return fmt.Errorf("%w: completion owner", ErrAgentFieldsChanged)
	}
	if finalState != AgentStateDone && finalState != AgentStateStuck {
		return fmt.Errorf("invalid final completion state %q", finalState)
	}
	fl, err := b.lockAgentBead(id)
	if err != nil {
		return fmt.Errorf("locking agent bead %s: %w", id, err)
	}
	defer func() { _ = fl.Unlock() }()
	issue, err := b.Show(id)
	if err != nil {
		return err
	}
	fields := agentFieldsFromIssue(issue)
	if fields == nil || fields.Incarnation != expectedIncarnation ||
		fields.AgentState != string(AgentStateCompleting) || fields.CompletionAttempt != expectedAttempt {
		return fmt.Errorf("%w: completion owner", ErrAgentFieldsChanged)
	}
	fields.AgentState = string(finalState)
	fields.CompletionAttempt = ""
	description := FormatAgentDescription(issue.Title, fields)
	removeLabels := make([]string, 0)
	for _, label := range issue.Labels {
		if strings.HasPrefix(label, "done-intent:") || strings.HasPrefix(label, "done-cp:") {
			removeLabels = append(removeLabels, label)
		}
	}
	return b.Update(id, UpdateOptions{Description: &description, RemoveLabels: removeLabels})
}

// ClearAgentActiveMRIfMatches clears active_mr only when it still references
// expectedMR. It returns true when a clear was written.
func (b *Beads) ClearAgentActiveMRIfMatches(id string, expectedMR string) (bool, error) {
	if target := b.agentBeadTarget(); target != b {
		return target.ClearAgentActiveMRIfMatches(id, expectedMR)
	}

	id = strings.TrimSpace(id)
	expectedMR = strings.TrimSpace(expectedMR)
	if id == "" || expectedMR == "" {
		return false, nil
	}

	fl, lockErr := b.lockAgentBead(id)
	if lockErr != nil {
		return false, fmt.Errorf("locking agent bead %s: %w", id, lockErr)
	}
	defer func() { _ = fl.Unlock() }()

	issue, err := b.Show(id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if !IsAgentBead(issue) {
		return false, fmt.Errorf("%s is not an agent bead", id)
	}

	fields := ParseAgentFields(issue.Description)
	if agentLifecycleFrozen(fields.AgentState) {
		return false, fmt.Errorf("%w: agent lifecycle is %s", ErrAgentFieldsChanged, fields.AgentState)
	}
	if strings.TrimSpace(fields.ActiveMR) != expectedMR {
		return false, nil
	}

	fields.ActiveMR = ""
	description := FormatAgentDescription(issue.Title, fields)
	if err := b.Update(id, UpdateOptions{Description: &description}); err != nil {
		return false, err
	}
	return true, nil
}

// UpdateAgentNotificationLevel updates the notification_level field in an agent bead.
// Valid levels: verbose, normal, muted (DND mode).
// Pass empty string to reset to default (normal).
func (b *Beads) UpdateAgentNotificationLevel(id string, level string) error {
	return b.UpdateAgentDescriptionFields(id, AgentFieldUpdates{NotificationLevel: &level})
}

// CompletionMetadata holds the fields written by gt done to record
// polecat work completion on the agent bead. The witness survey-workers
// step reads these fields to discover completion state from beads
// instead of POLECAT_DONE mail (nudge-over-mail redesign, gt-x7t9).
type CompletionMetadata struct {
	ExitType       string // COMPLETED, ESCALATED, DEFERRED, PHASE_COMPLETE
	MRID           string // MR bead ID (empty if no MR)
	Branch         string // Polecat working branch
	HookBead       string // The work bead ID
	MRFailed       bool   // True when MR creation was attempted but failed
	PushFailed     bool   // True when branch push to origin failed (gas-556)
	CompletionTime string // RFC3339 timestamp
}

// UpdateAgentCompletion atomically writes all completion metadata fields
// to an agent bead. Called by gt done to record completion state.
func (b *Beads) UpdateAgentCompletion(id string, meta *CompletionMetadata) error {
	return b.updateAgentCompletion(id, "", meta)
}

// UpdateAgentCompletionIfIncarnation binds completion metadata to the polecat
// generation that started gt done.
func (b *Beads) UpdateAgentCompletionIfIncarnation(id, expectedIncarnation string, meta *CompletionMetadata) error {
	if strings.TrimSpace(expectedIncarnation) == "" {
		return fmt.Errorf("%w: incarnation", ErrAgentFieldsChanged)
	}
	return b.updateAgentCompletion(id, expectedIncarnation, meta)
}

// UpdateAgentCompletionIfOwner writes completion metadata for the exact
// completion process that owns the durable attempt receipt.
func (b *Beads) UpdateAgentCompletionIfOwner(id, expectedIncarnation, expectedAttempt string, meta *CompletionMetadata) error {
	mrFailed := meta.MRFailed
	pushFailed := meta.PushFailed
	return b.UpdateAgentDescriptionFieldsIfCompletionOwner(id, expectedIncarnation, expectedAttempt, AgentFieldUpdates{
		ExitType:        &meta.ExitType,
		MRID:            &meta.MRID,
		Branch:          &meta.Branch,
		LastSourceIssue: &meta.HookBead,
		MRFailed:        &mrFailed,
		PushFailed:      &pushFailed,
		CompletionTime:  &meta.CompletionTime,
	})
}

func (b *Beads) updateAgentCompletion(id, expectedIncarnation string, meta *CompletionMetadata) error {
	mrFailed := meta.MRFailed
	pushFailed := meta.PushFailed
	updates := AgentFieldUpdates{
		ExitType:        &meta.ExitType,
		MRID:            &meta.MRID,
		Branch:          &meta.Branch,
		LastSourceIssue: &meta.HookBead,
		MRFailed:        &mrFailed,
		PushFailed:      &pushFailed,
		CompletionTime:  &meta.CompletionTime,
	}
	if expectedIncarnation == "" {
		return b.UpdateAgentDescriptionFields(id, updates)
	}
	return b.CompareAndUpdateAgentDescriptionFields(
		id,
		AgentFieldExpectations{Incarnation: &expectedIncarnation},
		updates,
	)
}

// ClearAgentCompletion removes all completion metadata fields from an agent bead.
// Called when a polecat is re-slung with new work (resets stale completion state).
func (b *Beads) ClearAgentCompletion(id string) error {
	empty := ""
	notFailed := false
	return b.UpdateAgentDescriptionFields(id, AgentFieldUpdates{
		ExitType:          &empty,
		MRID:              &empty,
		Branch:            &empty,
		LastSourceIssue:   &empty,
		MRFailed:          &notFailed,
		PushFailed:        &notFailed,
		CompletionTime:    &empty,
		CompletionAttempt: &empty,
	})
}

// GetAgentNotificationLevel returns the notification level for an agent.
// Returns "normal" if not set (the default).
func (b *Beads) GetAgentNotificationLevel(id string) (string, error) {
	_, fields, err := b.GetAgentBead(id)
	if err != nil {
		return "", err
	}
	if fields == nil {
		return NotifyNormal, nil
	}
	if fields.NotificationLevel == "" {
		return NotifyNormal, nil
	}
	return fields.NotificationLevel, nil
}

// GetAgentBead retrieves an agent bead by ID.
// Returns nil if not found.
func (b *Beads) GetAgentBead(id string) (*Issue, *AgentFields, error) {
	return b.GetAgentBeadContext(context.Background(), id)
}

// GetAgentBeadContext retrieves an agent bead and honors caller cancellation.
func (b *Beads) GetAgentBeadContext(ctx context.Context, id string) (*Issue, *AgentFields, error) {
	if target := b.agentBeadTarget(); target != b {
		return target.GetAgentBeadContext(ctx, id)
	}

	issue, err := b.ShowContext(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil, nil
		}
		return nil, nil, err
	}

	if !IsAgentBead(issue) {
		return nil, nil, fmt.Errorf("issue %s is not an agent bead (type=%s)", id, issue.Type)
	}

	fields := agentFieldsFromIssue(issue)
	return issue, fields, nil
}

func agentFieldsFromIssue(issue *Issue) *AgentFields {
	if issue == nil {
		return nil
	}
	fields := ParseAgentFields(issue.Description)
	fields.AgentState = ResolveAgentState(issue.Description, issue.AgentState)
	fields.structuredAgentState = strings.TrimSpace(issue.AgentState)
	fields.structuredHookBead = strings.TrimSpace(issue.HookBead)
	return fields
}

// InitializeAgentIncarnationIfMissing performs the sole permitted in-place
// incarnation transition: a legacy empty value becomes a new opaque lifetime
// ID. Existing values are immutable and returned unchanged.
func (b *Beads) InitializeAgentIncarnationIfMissing(id string) (string, error) {
	if target := b.agentBeadTarget(); target != b {
		return target.InitializeAgentIncarnationIfMissing(id)
	}
	fl, lockErr := b.lockAgentBead(id)
	if lockErr != nil {
		return "", fmt.Errorf("locking agent bead %s: %w", id, lockErr)
	}
	defer func() { _ = fl.Unlock() }()

	issue, err := b.Show(id)
	if err != nil {
		return "", err
	}
	if !IsAgentBead(issue) {
		return "", fmt.Errorf("issue %s is not an agent bead (type=%s)", id, issue.Type)
	}
	fields := ParseAgentFields(issue.Description)
	if fields.Incarnation != "" {
		return fields.Incarnation, nil
	}
	incarnation := uuid.NewString()
	fields.Incarnation = incarnation
	description := FormatAgentDescription(issue.Title, fields)
	if err := b.Update(id, UpdateOptions{Description: &description}); err != nil {
		return "", fmt.Errorf("initializing agent incarnation: %w", err)
	}
	return incarnation, nil
}

// ListAgentBeads returns all agent beads in a single query.
// Returns a map of agent bead ID to Issue.
//
// Queries both the issues table (authoritative metadata source) and the
// wisps table (fallback existence source). Issues take precedence for duplicate
// IDs so labels/type are preserved for doctor validation.
func (b *Beads) ListAgentBeads() (map[string]*Issue, error) {
	// Query issues table first. Issues include labels and type metadata used by
	// doctor checks (for example, validating gt:agent labels).
	// Agent beads are type=agent (infrastructure), hidden by bd list default filter.
	// Use --include-infra so they appear in results.
	out, err := b.run("list", "--label=gt:agent", "--include-infra", "--json", "--flat", "--no-pager")
	if err != nil {
		return nil, err
	}
	issuesByID := make(map[string]*Issue)
	var issues []*Issue
	if jsonErr := json.Unmarshal(out, &issues); jsonErr != nil {
		return nil, fmt.Errorf("parsing bd list --json output: %w (raw output %d bytes)", jsonErr, len(out))
	}
	for _, issue := range issues {
		issuesByID[issue.ID] = issue
	}

	// Query wisps table as a fallback source.
	// Keep issues-table entries when both exist for the same ID so richer
	// metadata (labels/type) is preserved.
	wispBeads, _ := b.ListAgentBeadsFromWisps()

	return mergeAgentBeadSources(issuesByID, wispBeads), nil
}

// mergeAgentBeadSources merges issue-backed and wisp-backed agent bead maps.
// Issues are authoritative because they carry full metadata (labels/type),
// while wisps are treated as a fallback existence source.
func mergeAgentBeadSources(issuesByID, wispsByID map[string]*Issue) map[string]*Issue {
	merged := make(map[string]*Issue, len(issuesByID)+len(wispsByID))
	for id, issue := range issuesByID {
		merged[id] = issue
	}
	for id, issue := range wispsByID {
		if _, exists := merged[id]; !exists {
			merged[id] = issue
		}
	}
	return merged
}

// ListAgentBeadsFromWisps queries the wisps table for agent beads.
// Returns nil, nil if the wisps table doesn't exist yet or has no agent beads.
func (b *Beads) ListAgentBeadsFromWisps() (map[string]*Issue, error) {
	out, err := b.run("mol", "wisp", "list", "--json")
	if err != nil {
		return nil, nil // Wisps table may not exist yet
	}

	// bd mol wisp list --json returns {"wisps": [...], "count": N, ...}
	var wrapper struct {
		Wisps []*Issue `json:"wisps"`
	}
	if err := json.Unmarshal(out, &wrapper); err != nil {
		return nil, nil
	}

	result := make(map[string]*Issue)
	for _, w := range wrapper.Wisps {
		// Check by type/label first (works when fields are present)
		if IsAgentBead(w) {
			result[w.ID] = w
			continue
		}
		// Fallback: wisps JSON may omit issue_type/labels fields.
		// Detect agent beads by ID pattern (prefix-rig-role format).
		if isAgentBeadByID(w.ID) {
			result[w.ID] = w
		}
	}

	return result, nil
}

// isAgentBeadByID detects agent beads by their ID naming convention.
// Agent bead IDs follow two patterns:
//   - Full form (prefix != rig): prefix-rig-role[-name] (e.g., gt-gastown-witness)
//   - Collapsed form (prefix == rig): prefix-role[-name] (e.g., bcc-witness)
//
// where role is one of: witness, refinery, crew, polecat, deacon, mayor.
// The collapsed form has only 2 parts for role-only IDs, so we must check
// from parts[1:] not parts[2:].
func isAgentBeadByID(id string) bool {
	parts := strings.Split(id, "-")
	if len(parts) < 2 {
		return false
	}
	// Check parts[1:] to handle both full-form (role at parts[2]) and
	// collapsed-form (role at parts[1]) agent bead IDs.
	for _, part := range parts[1:] {
		switch part {
		case constants.RoleWitness, constants.RoleRefinery, constants.RoleCrew, constants.RolePolecat, constants.RoleDeacon, constants.RoleMayor:
			return true
		}
	}
	return false
}

// ListWispIDs returns a set of all wisp IDs in the wisps table.
// This is useful for existence checks where wisp metadata (type, labels)
// may not be available in the list output.
func (b *Beads) ListWispIDs() (map[string]bool, error) {
	out, err := b.run("mol", "wisp", "list", "--json")
	if err != nil {
		return nil, nil
	}

	var wrapper struct {
		Wisps []struct {
			ID string `json:"id"`
		} `json:"wisps"`
	}
	if err := json.Unmarshal(out, &wrapper); err != nil {
		return nil, nil
	}

	result := make(map[string]bool, len(wrapper.Wisps))
	for _, w := range wrapper.Wisps {
		result[w.ID] = true
	}
	return result, nil
}
