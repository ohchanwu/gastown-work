package doctor

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/rig"
)

// RigBeadsCheck verifies that rig identity beads exist for all rigs.
// Rig identity beads track rig metadata like git URL, prefix, and operational state.
// They are created by gt rig add (see gt-zmznh) but may be missing for legacy rigs.
type RigBeadsCheck struct {
	FixableCheck
}

type expectedRigIdentity struct {
	id        string
	name      string
	beadsPath string
}

type rigIdentityCandidate = agentIdentityCandidate

func compareRigIdentity(expected expectedRigIdentity, candidates []rigIdentityCandidate) (*rigIdentityCandidate, error) {
	var matches []rigIdentityCandidate
	seen := make(map[string]bool)
	for _, candidate := range candidates {
		if candidate.issue == nil || !strings.HasSuffix(candidate.issue.ID, "-rig-"+expected.name) {
			continue
		}
		if candidate.issue.ID == expected.id && candidate.issue.Title != expected.name {
			return nil, fmt.Errorf("rig identity %s has contradictory metadata: title=%q", candidate.issue.ID, candidate.issue.Title)
		}
		key := candidate.identityKey()
		if !seen[key] {
			seen[key] = true
			matches = append(matches, candidate)
		}
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("multiple candidates for %s", expected.id)
	}
	if len(matches) == 1 {
		candidate := matches[0]
		if candidate.issue.ID != expected.id || filepath.Clean(candidate.beadsPath) != filepath.Clean(expected.beadsPath) {
			return nil, fmt.Errorf("legacy or misplaced rig identity %s conflicts with %s", candidate.issue.ID, expected.id)
		}
		return &candidate, nil
	}
	return nil, nil
}

func loadRigIdentityCandidates(workDirs []string) ([]rigIdentityCandidate, error) {
	return loadIdentityCandidates(workDirs)
}

// NewRigBeadsCheck creates a new rig identity beads check.
func NewRigBeadsCheck() *RigBeadsCheck {
	return &RigBeadsCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "rig-beads-exist",
				CheckDescription: "Verify rig identity beads exist for all rigs",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// Run checks if rig identity beads exist for all rigs.
func (c *RigBeadsCheck) Run(ctx *CheckContext) *CheckResult {
	allRigInfos, err := loadRegisteredRigInfos(ctx.TownRoot, "")
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "Could not derive registered rig identity inventory",
			Details: []string{err.Error()},
		}
	}
	rigInfos := allRigInfos
	if ctx.RigName != "" {
		rigInfos, err = loadRegisteredRigInfos(ctx.TownRoot, ctx.RigName)
		if err != nil {
			return &CheckResult{Name: c.Name(), Status: StatusError, Message: "Could not derive scoped rig identity inventory", Details: []string{err.Error()}}
		}
	}

	if len(rigInfos) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No rigs to check",
		}
	}
	workDirs := []string{beads.GetTownBeadsPath(ctx.TownRoot)}
	for _, info := range allRigInfos {
		workDirs = append(workDirs, filepath.Join(ctx.TownRoot, info.beadsPath))
	}
	candidates, err := loadRigIdentityCandidates(workDirs)
	if err != nil {
		return &CheckResult{Name: c.Name(), Status: StatusError, Message: "Could not inventory rig identities", Details: []string{err.Error()}}
	}

	var missing []string
	var conflicts []string
	var checked int

	// Check each rig for its identity bead
	for _, info := range rigInfos {
		rigName := info.name
		rigBeadsPath := filepath.Join(ctx.TownRoot, info.beadsPath)
		rigBeadID := beads.RigBeadIDWithPrefix(info.prefix, rigName)
		expected := expectedRigIdentity{id: rigBeadID, name: rigName, beadsPath: beads.ResolveBeadsDir(rigBeadsPath)}
		candidate, err := compareRigIdentity(expected, candidates)
		if err != nil {
			conflicts = append(conflicts, err.Error())
		} else if candidate == nil || candidate.issue.Status == "closed" {
			missing = append(missing, rigBeadID)
		}
		checked++
	}
	if len(conflicts) > 0 {
		return &CheckResult{Name: c.Name(), Status: StatusError, Message: fmt.Sprintf("%d rig identity conflict(s)", len(conflicts)), Details: conflicts}
	}

	if len(missing) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("All %d rig identity beads exist", checked),
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusError,
		Message: fmt.Sprintf("%d rig identity bead(s) missing", len(missing)),
		Details: missing,
		FixHint: "Run 'gt doctor --fix' to create missing rig identity beads",
	}
}

// Fix creates missing rig identity beads.
func (c *RigBeadsCheck) Fix(ctx *CheckContext) error {
	allRigInfos, err := loadRegisteredRigInfos(ctx.TownRoot, "")
	if err != nil {
		return err
	}
	rigInfos := allRigInfos
	if ctx.RigName != "" {
		rigInfos, err = loadRegisteredRigInfos(ctx.TownRoot, ctx.RigName)
		if err != nil {
			return err
		}
	}

	if len(rigInfos) == 0 {
		return nil // No rigs to process
	}
	workDirs := []string{beads.GetTownBeadsPath(ctx.TownRoot)}
	for _, info := range allRigInfos {
		workDirs = append(workDirs, filepath.Join(ctx.TownRoot, info.beadsPath))
	}
	candidates, err := loadRigIdentityCandidates(workDirs)
	if err != nil {
		return err
	}

	// Create missing rig identity beads — collect errors instead of failing
	// on first so one broken rig doesn't block fixes for others.
	var errs []error
	for _, info := range rigInfos {
		rigName := info.name
		rigBeadsPath := filepath.Join(ctx.TownRoot, info.beadsPath)
		bd := beads.New(rigBeadsPath)

		// Try to get git URL from rig config
		rigPath := filepath.Join(ctx.TownRoot, rigName)
		gitURL := ""
		if cfg, err := rig.LoadRigConfig(rigPath); err == nil {
			gitURL = cfg.GitURL
		}

		fields := &beads.RigFields{
			Repo:   gitURL,
			Prefix: info.prefix,
			State:  beads.RigStateActive,
		}

		rigBeadID := beads.RigBeadIDWithPrefix(info.prefix, rigName)
		expected := expectedRigIdentity{id: rigBeadID, name: rigName, beadsPath: beads.ResolveBeadsDir(rigBeadsPath)}
		candidate, err := compareRigIdentity(expected, candidates)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if candidate != nil {
			if candidate.issue.Status == "closed" {
				openStatus := "open"
				if err := bd.Update(rigBeadID, beads.UpdateOptions{Status: &openStatus}); err != nil {
					errs = append(errs, fmt.Errorf("reopening closed rig identity %s: %w", rigBeadID, err))
				}
			}
			continue
		}
		created, err := bd.EnsureRigBead(rigName, fields)
		if err != nil {
			errs = append(errs, fmt.Errorf("ensuring %s: %w", rigBeadID, err))
			continue
		}
		candidates = append(candidates, rigIdentityCandidate{issue: created, beadsPath: expected.beadsPath})
	}

	return errors.Join(errs...)
}
