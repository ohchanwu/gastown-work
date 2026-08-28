package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
)

// polecatTarget represents a polecat to operate on.
type polecatTarget struct {
	rigName     string
	polecatName string
	mgr         *polecat.Manager
	r           *rig.Rig
}

// resolvePolecatTargets builds a list of polecats from command args.
// If useAll is true, the first arg is treated as a rig name and all polecats in it are returned.
// Otherwise, args are parsed as rig/polecat addresses.
func resolvePolecatTargets(args []string, useAll bool) ([]polecatTarget, error) {
	var targets []polecatTarget

	if useAll {
		// --all flag: first arg is just the rig name
		rigName := args[0]
		// Check if it looks like rig/polecat format
		if _, _, err := parseAddress(rigName); err == nil {
			return nil, fmt.Errorf("with --all, provide just the rig name (e.g., 'gt polecat <cmd> %s --all')", strings.Split(rigName, "/")[0])
		}

		mgr, r, err := getPolecatManager(rigName)
		if err != nil {
			return nil, err
		}

		polecats, err := mgr.List()
		if err != nil {
			return nil, fmt.Errorf("listing polecats: %w", err)
		}

		for _, p := range polecats {
			targets = append(targets, polecatTarget{
				rigName:     rigName,
				polecatName: p.Name,
				mgr:         mgr,
				r:           r,
			})
		}
	} else {
		// Multiple rig/polecat arguments - require explicit rig/polecat format
		for _, arg := range args {
			// Validate format: must contain "/" to avoid misinterpreting rig names as polecat names
			if !strings.Contains(arg, "/") {
				return nil, fmt.Errorf("invalid address '%s': must be in 'rig/polecat' format (e.g., 'gastown/Toast')", arg)
			}

			rigName, polecatName, err := parseAddress(arg)
			if err != nil {
				return nil, fmt.Errorf("invalid address '%s': %w", arg, err)
			}

			mgr, r, err := getPolecatManager(rigName)
			if err != nil {
				return nil, err
			}

			targets = append(targets, polecatTarget{
				rigName:     rigName,
				polecatName: polecatName,
				mgr:         mgr,
				r:           r,
			})
		}
	}

	return targets, nil
}

// SafetyCheckResult holds the result of safety checks for a polecat.
type SafetyCheckResult struct {
	Polecat       string
	Blocked       bool
	Reasons       []string
	CleanupStatus polecat.CleanupStatus
	HookBead      string
	HookStale     bool // true if hooked bead is closed
	ActiveMR      string
	OpenMR        string
	GitState      *GitState
}

// checkPolecatSafety performs safety checks before destructive operations.
// Returns nil if the polecat is safe to operate on, or a SafetyCheckResult with reasons if blocked.
func checkPolecatSafety(target polecatTarget) *SafetyCheckResult {
	result := &SafetyCheckResult{
		Polecat: fmt.Sprintf("%s/%s", target.rigName, target.polecatName),
	}

	// Get polecat info for branch name
	polecatInfo, infoErr := target.mgr.Get(target.polecatName)
	if blocker := polecatMetadataSafetyBlocker(polecatInfo, infoErr); blocker != "" {
		result.Reasons = append(result.Reasons, blocker)
		result.Blocked = true
		return result
	}

	// Check 1: Unpushed commits via cleanup_status or git state
	bd := beads.New(target.r.Path)
	agentBeadID := polecatBeadIDForRig(target.r, target.rigName, target.polecatName)
	agentIssue, fields, err := bd.GetAgentBead(agentBeadID)

	if err != nil || fields == nil {
		// No agent bead - fall back to git check
		if infoErr == nil && polecatInfo != nil {
			gitState, gitErr := getGitState(polecatInfo.ClonePath)
			result.GitState = gitState
			if gitErr != nil {
				result.Reasons = append(result.Reasons, "cannot check git state")
			} else if !gitState.Clean {
				if gitState.UnpushedCommits > 0 {
					result.Reasons = append(result.Reasons, fmt.Sprintf("has %d unpushed commit(s)", gitState.UnpushedCommits))
				} else if len(gitState.UncommittedFiles) > 0 {
					result.Reasons = append(result.Reasons, fmt.Sprintf("has %d uncommitted file(s)", len(gitState.UncommittedFiles)))
				} else if gitState.StashCount > 0 {
					result.Reasons = append(result.Reasons, fmt.Sprintf("has %d stash(es)", gitState.StashCount))
				}
			}
		}
	} else {
		currentIssue := ""
		if infoErr == nil && polecatInfo != nil {
			currentIssue = polecatInfo.Issue
		}
		sourceHint := agentSourceIssueHint(currentIssue, fields)
		hookBead := agentHookBead(agentIssue, fields)
		var gitState *GitState
		gitStateLoaded := false
		loadGitState := func() {
			if gitStateLoaded || infoErr != nil || polecatInfo == nil {
				return
			}
			gitState, _ = getGitState(polecatInfo.ClonePath)
			result.GitState = gitState
			gitStateLoaded = true
		}
		activeMRAssessment := polecat.ActiveMRAssessment{}
		if fields.ActiveMR != "" {
			loadGitState()
			gitSafe := false
			if polecatInfo != nil {
				gitSafe = activeMRGitSafeForWorktree(polecatInfo.ClonePath)
			}
			activeMRAssessment = polecat.AssessActiveMR(bd, polecat.ActiveMRInput{ActiveMR: fields.ActiveMR, SourceIssueHint: sourceHint, RequireGitSafe: true, GitSafe: gitSafe})
		}
		workRefs := agentWorkReferences(currentIssue, agentIssue, fields)
		if activeMRAssessment.SourceIssue != "" {
			workRefs = uniqueStrings(append(workRefs, activeMRAssessment.SourceIssue))
		}
		workTerminal, workBlocker := allWorkReferencesTerminal(bd, workRefs)
		if workBlocker != "" {
			result.Reasons = append(result.Reasons, workBlocker)
		}

		// Check cleanup_status from agent bead
		result.CleanupStatus = polecat.CleanupStatus(fields.CleanupStatus)
		switch result.CleanupStatus {
		case polecat.CleanupClean:
			// OK
		default:
			if result.CleanupStatus == polecat.CleanupUnpushed {
				loadGitState()
			}
			gitSafe := false
			if polecatInfo != nil {
				gitSafe = activeMRGitSafeForWorktree(polecatInfo.ClonePath)
			}
			hookSafe, _, _ := hookBeadSafeForCleanup(bd, hookBead)
			activeMRSafe := !activeMRAssessment.Pending
			if polecat.CanIgnoreStaleCleanupStatus(result.CleanupStatus, workTerminal, hookSafe, activeMRSafe, gitSafe) {
				// OK: stale self-report after terminal source and direct clean git.
			} else {
				result.Reasons = append(result.Reasons, cleanupStatusBlocker(result.CleanupStatus))
			}
		}

		// Check 3: Work on hook
		if hookBead != "" {
			result.HookBead = hookBead
			// Check if hooked bead is still active (not closed)
			hookedIssue, err := bd.Show(hookBead)
			if err == nil && hookedIssue != nil {
				if hookedIssue.Status != "closed" {
					result.Reasons = append(result.Reasons, fmt.Sprintf("has work on hook (%s)", hookBead))
				} else {
					result.HookStale = true
				}
			} else {
				result.Reasons = append(result.Reasons, fmt.Sprintf("has work on hook (%s, unverified)", hookBead))
			}
		}

		if fields.ActiveMR != "" {
			result.ActiveMR = fields.ActiveMR
			if blocker := activeMRAssessment.Reason; activeMRAssessment.Pending && blocker != "" {
				result.Reasons = append(result.Reasons, blocker)
			}
		}
	}

	// Check 2: Open MR beads for this branch
	if infoErr == nil && polecatInfo != nil && polecatInfo.Branch != "" {
		mr, mrErr := bd.FindMRForBranch(polecatInfo.Branch)
		if mrErr != nil {
			result.Reasons = append(result.Reasons, fmt.Sprintf("open_mr_lookup_error: %v", mrErr))
		} else if mr != nil {
			result.OpenMR = mr.ID
			result.Reasons = append(result.Reasons, fmt.Sprintf("has open MR (%s)", mr.ID))
		}
	}

	result.Blocked = len(result.Reasons) > 0
	return result
}

func polecatMetadataSafetyBlocker(info *polecat.Polecat, err error) string {
	if err != nil {
		return fmt.Sprintf("polecat metadata unavailable: %v", err)
	}
	if info == nil {
		return "polecat metadata unavailable: empty result"
	}
	return ""
}

func rigPrefix(r *rig.Rig) string {
	townRoot := filepath.Dir(r.Path)
	return beads.GetPrefixForRig(townRoot, r.Name)
}

func polecatBeadIDForRig(r *rig.Rig, rigName, polecatName string) string {
	return beads.PolecatBeadIDWithPrefix(rigPrefix(r), rigName, polecatName)
}

// displaySafetyCheckBlocked prints blocked polecats and guidance.
func displaySafetyCheckBlocked(blocked []*SafetyCheckResult) {
	displaySafetyCheckBlockedTo(os.Stderr, blocked)
}

func displaySafetyCheckBlockedTo(w io.Writer, blocked []*SafetyCheckResult) {
	fmt.Fprintf(w, "%s Cannot nuke the following polecats:\n\n", style.Error.Render("Error:"))
	var polecatList []string
	for _, b := range blocked {
		fmt.Fprintf(w, "  %s:\n", style.Bold.Render(b.Polecat))
		for _, r := range b.Reasons {
			fmt.Fprintf(w, "    - %s\n", r)
		}
		polecatList = append(polecatList, b.Polecat)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Safety checks failed. Resolve issues before nuking, or use --force.")
	fmt.Fprintln(w, "Options:")
	fmt.Fprintln(w, "  1. Complete work: gt done (from polecat session)")
	fmt.Fprintln(w, "  2. Push changes: git push (from polecat worktree)")
	fmt.Fprintln(w, "  3. Escalate: gt mail send mayor/ -s \"RECOVERY_NEEDED\" -m \"...\"")
	fmt.Fprintf(w, "  4. Force nuke (LOSES WORK): gt polecat nuke --force %s\n", strings.Join(polecatList, " "))
	fmt.Fprintln(w)
}

func formatSafetyCheckBlockers(blocked []*SafetyCheckResult) string {
	parts := make([]string, 0, len(blocked))
	for _, b := range blocked {
		parts = append(parts, fmt.Sprintf("%s: %s", b.Polecat, strings.Join(b.Reasons, "; ")))
	}
	return strings.Join(parts, " | ")
}

func writeDryRunNukePlan(w io.Writer, target polecatTarget, result *SafetyCheckResult, custodyErr error, force bool) bool {
	safetyBlocked := result == nil || (result.Blocked && !force)
	blocked := safetyBlocked || custodyErr != nil
	switch {
	case safetyBlocked:
		fmt.Fprintf(w, "Would refuse to nuke %s/%s without --force:\n", target.rigName, target.polecatName)
	case custodyErr != nil:
		fmt.Fprintf(w, "Would refuse to nuke %s/%s:\n", target.rigName, target.polecatName)
	case result.Blocked:
		fmt.Fprintf(w, "Would nuke %s/%s (--force overrides safety blockers):\n", target.rigName, target.polecatName)
	default:
		fmt.Fprintf(w, "Would nuke %s/%s:\n", target.rigName, target.polecatName)
	}
	if result != nil && result.Blocked {
		for _, reason := range result.Reasons {
			fmt.Fprintf(w, "  - Safety blocker: %s\n", reason)
		}
	}
	if custodyErr != nil {
		fmt.Fprintf(w, "  - Custody proof: %v\n", custodyErr)
	}
	if blocked {
		return true
	}
	fmt.Fprintf(w, "  - Kill session: gt-%s-%s\n", target.rigName, target.polecatName)
	fmt.Fprintf(w, "  - Delete worktree: %s/polecats/%s\n", target.r.Path, target.polecatName)
	fmt.Fprintln(w, "  - Delete branch (if exists)")
	fmt.Fprintf(w, "  - Reset agent bead: %s\n", polecatBeadIDForRig(target.r, target.rigName, target.polecatName))
	fmt.Fprintln(w, "  - Remote refs: unchanged (no push or delete)")
	return false
}
