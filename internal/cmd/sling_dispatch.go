package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
)

// SlingParams captures everything needed to sling one bead to a rig.
// This is the serialization boundary for queue dispatch: at enqueue time,
// these fields are stored as queue metadata; at dispatch time, they are
// reconstructed into a SlingParams and passed to executeSling().
type SlingParams struct {
	// What to sling
	BeadID      string // Base bead
	FormulaName string // Formula to apply ("mol-polecat-work", user formula, or "")
	RigName     string // Target rig (always a rig for queue)

	// CLI flag passthrough
	Args         string   // --args
	Vars         []string // --var (key=value pairs)
	Merge        string   // --merge (convoy strategy)
	BaseBranch   string   // --base-branch
	ResumeBranch string   // --branch / --pr (resume existing PR branch, gh#3602)
	Account      string   // --account
	Agent        string   // --agent
	NoConvoy     bool     // --no-convoy
	Owned        bool     // --owned
	NoMerge      bool     // --no-merge
	Force        bool     // --force
	HookRawBead  bool     // --hook-raw-bead
	NoBoot       bool     // --no-boot
	Mode         string   // --ralph: "" (normal) or "ralph"
	ReviewOnly   bool     // --review-only: review and report back only, no merge/commit/push

	// Execution behavior (set by caller, not serialized to queue)
	Context          context.Context
	SkipCook         bool   // Batch optimization: formula already cooked
	FormulaFailFatal bool   // true=rollback+error (single/queue), false=hook raw bead (batch)
	CallerContext    string // Identifies the caller for shutdown messages (e.g., "queue-dispatch", "batch-sling")
	TownRoot         string
	BeadsDir         string
}

// SlingResult captures the outcome of executeSling for caller-level tracking.
type SlingResult struct {
	BeadID           string
	PolecatName      string
	SpawnInfo        *SpawnedPolecatInfo
	Success          bool
	ErrMsg           string
	AttachedMolecule string
}

var cleanupSpawnedPolecatFn = cleanupSpawnedPolecat
var cleanupSpawnedPolecatWhileAssignmentFencedFn = cleanupSpawnedPolecatWhileAssignmentFenced

// executeSling performs the unified per-bead polecat/rig dispatch.
// Batch sling and queue dispatch call this function. The single-sling path
// (runSling) retains its own implementation for now (handles dogs, mayor,
// nudge, and other non-rig targets). See TODO in sling.go.
//
// Caller responsibilities (NOT handled by executeSling):
//   - Cross-rig guard: callers must call checkCrossRigGuard() before executeSling
//     to verify the bead's prefix matches the target rig. Batch sling does this
//     pre-loop; queue dispatch skips the guard because the bead prefix was
//     validated at enqueue time and is immutable.
//   - wakeRigAgents: callers must call wakeRigAgents() after the dispatch loop
//     when NoBoot is false. Batch sling calls it post-loop; queue dispatch sets
//     NoBoot=true to avoid lock contention in the daemon.
//
// Steps:
//  1. Get bead info + status check
//  2. Burn stale molecules (if formula and force)
//  3. Spawn polecat (via spawnPolecatForSling)
//  4. Auto-convoy (if !NoConvoy)
//  5. Cook formula (unless SkipCook)
//  6. Instantiate formula on bead (wisp + bond)
//  7. Hook bead with retry
//  8. Log sling event
//  9. Update agent hook_bead state
//  10. Store fields in bead (dispatcher, args, attached_molecule, no_merge)
//  11. Create Dolt branch
//  12. Start polecat session
func executeSling(params SlingParams) (*SlingResult, error) {
	townRoot := params.TownRoot
	if townRoot == "" {
		var err error
		townRoot, err = findTownRoot()
		if err != nil {
			return nil, err
		}
	}

	// Acquire per-bead flock to prevent concurrent dispatch races (TOCTOU).
	// The CLI path (runSling) has its own flock; this closes the gap where
	// batch sling and queue dispatch could race against each other or against
	// a concurrent CLI invocation.
	releaseLock, err := tryAcquireSlingBeadLock(townRoot, params.BeadID)
	if err != nil {
		return &SlingResult{BeadID: params.BeadID, ErrMsg: err.Error()}, err
	}
	defer releaseLock()

	beadsDir := params.BeadsDir
	if beadsDir == "" {
		beadsDir = filepath.Join(townRoot, ".beads")
	}

	result := &SlingResult{
		BeadID: params.BeadID,
	}
	ctx := params.Context
	if params.FormulaName != "" && ctx == nil {
		result.ErrMsg = "formula sling requires caller context"
		return result, errors.New(result.ErrMsg)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// 0. Check if rig is parked or docked before dispatching (gt-4owfd.1, gt-11y)
	if params.RigName != "" {
		if blocked, reason := IsRigParkedOrDocked(townRoot, params.RigName); blocked {
			result.ErrMsg = "rig " + reason
			undoCmd := "gt rig unpark"
			if reason == "docked" {
				undoCmd = "gt rig undock"
			}
			return result, fmt.Errorf("cannot sling to %s rig %q\n%s %s", reason, params.RigName, undoCmd, params.RigName)
		}
	}

	// 1. Get bead info + status check
	info, err := getBeadInfoFromTownRoot(townRoot, params.BeadID)
	if err != nil {
		result.ErrMsg = err.Error()
		return result, fmt.Errorf("could not get bead info: %w", err)
	}
	if resumed, err := resumePendingRetirement(townRoot, params.BeadID, info.Assignee); err != nil {
		result.ErrMsg = err.Error()
		return result, err
	} else if resumed {
		result.Success = true
		result.PolecatName = info.Assignee
		return result, nil
	}

	// Guard against dispatching closed/tombstone beads (defense-in-depth).
	// Not bypassed by --force — if you need to re-dispatch, reopen the bead first.
	if info.Status == "closed" || info.Status == "tombstone" {
		result.ErrMsg = "already " + info.Status
		return result, fmt.Errorf("bead %s is %s (work already completed)", params.BeadID, info.Status)
	}

	// Save explicit force state before dead-agent auto-force, so the deferred
	// gate below still requires an explicit --force for deferred beads.
	explicitForce := params.Force

	if (info.Status == "pinned" || info.Status == "hooked" || info.Status == "in_progress") && !params.Force {
		// Auto-force when hooked/in_progress agent's session is confirmed dead (gt-npzy, GH#1380).
		// Mirrors the dead-agent detection in runSling (sling.go) so that
		// programmatic dispatch also handles stale hooks from nuked polecats.
		if (info.Status == "hooked" || info.Status == "in_progress") && info.Assignee != "" && isHookedAgentDeadFn(info.Assignee) {
			fmt.Printf("  %s Hooked agent %s has no active session, auto-forcing dispatch...\n",
				style.Warning.Render("⚠"), info.Assignee)
			params.Force = true
		} else {
			result.ErrMsg = "already " + info.Status
			return result, fmt.Errorf("already %s (use --force to re-sling)", info.Status)
		}
	}

	// Guard against slinging deferred beads (gt-1326mw).
	// Uses explicitForce (not params.Force) so dead-agent auto-force doesn't
	// accidentally bypass the deferred gate.
	if isDeferredBead(info) && !explicitForce {
		result.ErrMsg = "deferred"
		return result, fmt.Errorf("bead %s is deferred (use --force to override)", params.BeadID)
	}

	if params.RigName != "" {
		if err := verifyBeadExistsInTargetRigDatabase(params.BeadID, params.RigName, townRoot); err != nil {
			result.ErrMsg = err.Error()
			return result, err
		}
	}

	// Retire the old owner only after the replacement is fully viable.
	oldAssignee := ""
	oldIncarnation := ""
	forcedReassignment := (info.Status == "hooked" || info.Status == "in_progress") && params.Force && info.Assignee != ""
	if forcedReassignment {
		if !lifecycleRetirementAvailableFn() {
			result.ErrMsg = tmux.ErrSessionCustodyUnsupported.Error()
			return result, tmux.ErrSessionCustodyUnsupported
		}
		oldAssignee = info.Assignee
		oldIncarnation, err = capturePolecatIncarnationFn(townRoot, oldAssignee)
		if err != nil {
			result.ErrMsg = err.Error()
			return result, err
		}
	}

	// 3. Spawn polecat (via spawnPolecatForSling)
	spawnOpts := SlingSpawnOptions{
		TownRoot:     townRoot,
		Force:        params.Force,
		Account:      params.Account,
		HookBead:     params.BeadID,
		Agent:        params.Agent,
		BaseBranch:   params.BaseBranch,
		ResumeBranch: params.ResumeBranch,
		// Create is always true for rig targets: executeSling only handles
		// rig-targeted dispatch (batch sling + queue dispatch), where a fresh
		// polecat must be spawned. The single-sling path (runSling) handles
		// the --create flag for non-rig targets via resolveTarget.
		Create: true,
	}
	spawnInfo, err := spawnPolecatForSling(params.RigName, spawnOpts)
	if err != nil {
		if spawnInfo != nil {
			cleanupSpawnedPolecatFn(spawnInfo, params.RigName, "")
		}
		result.ErrMsg = err.Error()
		return result, fmt.Errorf("failed to spawn polecat: %w", err)
	}
	result.SpawnInfo = spawnInfo
	result.PolecatName = spawnInfo.PolecatName

	targetAgent := spawnInfo.AgentID()
	callerCtx := params.CallerContext
	if callerCtx == "" {
		callerCtx = "sling"
	}
	retirement, err := prepareRetirementRecord(townRoot, params.BeadID, oldAssignee, oldIncarnation, targetAgent, spawnInfo.Incarnation, callerCtx)
	if err != nil {
		cleanupSpawnedPolecatFn(spawnInfo, params.RigName, "")
		result.ErrMsg = err.Error()
		return result, err
	}
	hookWorkDir := spawnInfo.ClonePath
	beadToHook := params.BeadID
	attachedMoleculeID := ""
	rollbackReason := "Assignment failed"

	// Serialize the whole generation-bound assignment transaction. Keep the
	// existing assignee -> lifecycle lock order used by the other sling paths.
	assigneeUnlock, assigneeLockErr := tryAcquireSlingAssigneeLock(townRoot, targetAgent)
	if assigneeLockErr != nil {
		cleanupSpawnedPolecat(spawnInfo, params.RigName, "")
		if abortErr := abortRetirementRecordFn(townRoot, retirement); abortErr != nil {
			assigneeLockErr = errors.Join(assigneeLockErr, abortErr)
		}
		result.ErrMsg = "assignee lock failed"
		return result, fmt.Errorf("serializing assignment for %s: %w", targetAgent, assigneeLockErr)
	}
	defer assigneeUnlock()
	var allVars []string
	varsForAttachment := append([]string(nil), params.Vars...)
	formulaVarsForAttachment := strings.Join(varsForAttachment, "\n")
	if params.FormulaName != "" {
		allVars = append(loadRigCommandVars(townRoot, params.RigName), params.Vars...)
		if spawnInfo.BaseBranch != "" && spawnInfo.BaseBranch != "main" {
			allVars = append(allVars, fmt.Sprintf("base_branch=%s", spawnInfo.BaseBranch))
		}
		if priorVars := lookupPriorAttempt(beadsDir, params.BeadID); len(priorVars) > 0 {
			allVars = append(allVars, priorVars...)
			fmt.Printf("  %s Prior attempt found — context injected for polecat\n", style.Dim.Render("↻"))
		}
		varsForAttachment = append([]string(nil), allVars...)
		formulaVarsForAttachment = strings.Join(allVars, "\n")
	}

	// 4. Auto-convoy (if !NoConvoy)
	convoyID := ""
	rollbackSpawnedPolecat := func(rollbackBeadID, reason string) {
		fmt.Printf("  %s %s, rolling back spawned polecat %s...\n", style.Warning.Render("⚠"), reason, spawnInfo.PolecatName)
		rollbackSlingArtifactsWhileAssignmentFencedFn(spawnInfo, rollbackBeadID, hookWorkDir, convoyID)
	}
	assignmentEntered := false
	assignmentErr := withPolecatAssignmentFence(targetAgent, townRoot, func() (retErr error) {
		assignmentEntered = true
		defer func() {
			if retErr != nil {
				rollbackSpawnedPolecat(beadToHook, rollbackReason)
			}
		}()
		var pendingFormulaResult *FormulaOnBeadResult
		if params.FormulaName != "" {
			pendingFormulaResult, err = reconcilePendingFormulaAndCleanup(
				ctx, params.FormulaName, params.BeadID, info.Title, hookWorkDir, townRoot, true, allVars,
				func(lockedInfo *beadInfo) bool {
					return params.Force ||
						(lockedInfo.Assignee == "" && (lockedInfo.Status == "open" || lockedInfo.Status == "in_progress")) ||
						(lockedInfo.Assignee != "" && isHookedAgentDeadFn(lockedInfo.Assignee))
				},
			)
			if err != nil {
				rollbackReason = "Stale molecule reconciliation failed"
				return err
			}
		}
		if !params.NoConvoy {
			existingConvoy := isTrackedByConvoy(params.BeadID)
			if existingConvoy == "" {
				var err error
				convoyID, err = createAutoConvoy(params.BeadID, info.Title, params.Owned, params.Merge, params.BaseBranch)
				if err != nil {
					fmt.Printf("  %s Could not create auto-convoy: %v\n", style.Dim.Render("Warning:"), err)
				} else {
					fmt.Printf("  %s Created convoy %s\n", style.Bold.Render("→"), convoyID)
				}
			} else {
				fmt.Printf("  %s Already tracked by convoy %s\n", style.Dim.Render("○"), existingConvoy)
			}
		}

		// 5. Cook formula (unless SkipCook)
		formulaCooked := params.SkipCook || pendingFormulaResult != nil
		if params.FormulaName != "" && !formulaCooked {
			workDir := beads.ResolveHookDir(townRoot, params.BeadID, hookWorkDir)
			if err := CookFormula(params.FormulaName, workDir, townRoot); err != nil {
				if params.FormulaFailFatal {
					rollbackReason = "Formula cook failed"
					result.ErrMsg = fmt.Sprintf("cook failed: %v", err)
					return fmt.Errorf("cooking formula %s: %w", params.FormulaName, err)
				}
				fmt.Printf("  %s Could not cook formula %s: %v\n", style.Dim.Render("Warning:"), params.FormulaName, err)
			} else {
				formulaCooked = true
			}
		}

		// 6. Instantiate formula on bead (wisp + bond)
		if params.FormulaName != "" && formulaCooked {
			formulaResult := pendingFormulaResult
			if formulaResult == nil {
				formulaResult, err = InstantiateFormulaOnBead(ctx, params.FormulaName, params.BeadID, info.Title, hookWorkDir, townRoot, true, allVars)
			}
			if err != nil {
				if params.FormulaFailFatal {
					rollbackReason = "Formula instantiation failed"
					result.ErrMsg = fmt.Sprintf("formula failed: %v", err)
					return fmt.Errorf("instantiating formula %s: %w", params.FormulaName, err)
				}
				// Best-effort: in batch mode, a formula instantiation failure should not abort or rollback the
				// spawned polecat. We still hook the raw bead so work can proceed (e.g., missing required vars).
				fmt.Printf("  %s Could not apply formula: %v (hooking raw bead)\n", style.Dim.Render("Warning:"), err)
			} else {
				fmt.Printf("  %s Formula %s applied\n", style.Bold.Render("✓"), params.FormulaName)
				beadToHook = formulaResult.BeadToHook
				attachedMoleculeID = formulaResult.WispRootID
				if len(formulaResult.FormulaVars) > 0 {
					allVars = formulaResult.FormulaVars
					varsForAttachment = append([]string(nil), allVars...)
					formulaVarsForAttachment = strings.Join(allVars, "\n")
				}
			}
		}
		result.AttachedMolecule = attachedMoleculeID

		actor := detectActor()
		fieldUpdates := beadFieldUpdates{
			Dispatcher:       actor,
			Args:             params.Args,
			Vars:             varsForAttachment,
			AttachedMolecule: attachedMoleculeID,
			NoMerge:          params.NoMerge,
			ReviewOnly:       params.ReviewOnly,
			Mode:             &params.Mode,
			FormulaVars:      formulaVarsForAttachment,
		}
		if params.FormulaName != "" {
			if attachedMoleculeID != "" {
				fieldUpdates.AttachedFormula = params.FormulaName
			} else {
				fieldUpdates.ClearAttachment = true
				fieldUpdates.Vars = nil
				fieldUpdates.FormulaVars = ""
			}
		}
		if fieldUpdates.AttachedMolecule != "" || fieldUpdates.AttachedFormula != "" || params.NoMerge || params.ReviewOnly {
			fieldUpdates.AttachedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}

		// 7. Hook bead with retry
		workflowReceipt, receiptErr := captureSlingWorkflowReceiptFn(beadToHook)
		if receiptErr != nil {
			return fmt.Errorf("capturing workflow receipt: %w", receiptErr)
		}
		if attachedMoleculeID == "" && (params.NoMerge || params.ReviewOnly) {
			if err := storeSlingFieldsWithReceipt(townRoot, beadToHook, fieldUpdates, workflowReceipt); err != nil {
				recordSlingAssignment(spawnInfo, beadToHook, targetAgent, workflowReceipt)
				rollbackReason = "Raw sling metadata failed"
				result.ErrMsg = "raw sling metadata failed"
				return fmt.Errorf("storing raw sling metadata before hook: %w", err)
			}
		}
		hookDir := beads.ResolveHookDir(townRoot, beadToHook, hookWorkDir)
		recordSlingAssignment(spawnInfo, beadToHook, targetAgent, workflowReceipt)
		if err := hookBeadWithRetryWithTownRootFn(beadToHook, targetAgent, hookDir, townRoot); err != nil {
			rollbackReason = "Hook failed"
			result.ErrMsg = "hook failed"
			return fmt.Errorf("failed to hook bead: %w", err)
		}
		fmt.Printf("  %s Work attached to %s\n", style.Bold.Render("✓"), spawnInfo.PolecatName)

		// 8. Log sling event
		_ = events.LogFeed(events.TypeSling, actor, events.SlingPayload(beadToHook, targetAgent))

		// 9. Update agent hook_bead state
		updateAgentHookBead(targetAgent, beadToHook, hookWorkDir, beadsDir)

		// 10. Store fields in bead (dispatcher, args, attached_molecule, no_merge, mode)
		// Use beadToHook for the update target (may differ from beadID when formula-on-bead)
		if err := storeSlingFieldsWithReceipt(townRoot, beadToHook, fieldUpdates, workflowReceipt); err != nil {
			rollbackReason = "Metadata persistence failed"
			result.ErrMsg = "metadata persistence failed"
			return fmt.Errorf("storing sling fields after hook: %w", err)
		}
		if params.FormulaName != "" && attachedMoleculeID != "" {
			if err := clearFormulaMutationAttempt(townRoot, formulaBondMutationKey(params.FormulaName, params.BeadID)); err != nil {
				return fmt.Errorf("clearing committed formula bond receipt: %w", err)
			}
		}

		// Update agent bead mode for stuck-detector Ralph thresholds. Reuse/reset clears stale mode.
		if params.Mode != "" {
			updateAgentMode(targetAgent, params.Mode, hookWorkDir, beadsDir)
		}
		pane, startErr := startSpawnedPolecatSessionFn(spawnInfo, true)
		if startErr != nil {
			rollbackReason = "Session failed"
			return fmt.Errorf("starting polecat session: %w", startErr)
		}
		spawnInfo.Pane = pane
		return nil
	})
	if assignmentErr != nil {
		if !assignmentEntered {
			cleanupSpawnedPolecatFn(spawnInfo, params.RigName, convoyID)
		}
		if result.ErrMsg == "" {
			result.ErrMsg = "assignment failed"
		}
		if abortErr := abortRetirementRecordFn(townRoot, retirement); abortErr != nil {
			assignmentErr = errors.Join(assignmentErr, abortErr)
		}
		return result, assignmentErr
	}
	if err := completeRetirementRecordFn(townRoot, retirement); err != nil {
		result.ErrMsg = "old-owner retirement pending"
		return result, fmt.Errorf("replacement is viable but old-owner retirement is pending: %w", err)
	}

	fmt.Printf("  %s Session started for %s\n", style.Bold.Render("▶"), spawnInfo.PolecatName)

	result.Success = true
	return result, nil
}

// findTownRoot is defined in hook.go
