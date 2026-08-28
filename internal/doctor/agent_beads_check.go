package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

// AgentBeadsCheck verifies that agent beads exist for all agents.
// This includes:
// - Global agents (deacon, mayor) - stored in town beads with hq- prefix
// - Per-rig agents (witness, refinery) - stored in each rig's beads
//
// Worker identities are created by their lifecycle commands; Doctor does not
// infer them from directory names because crash worktrees are not identities.
// Each rig uses its configured prefix (e.g., "gt-" for gastown, "bd-" for beads).
type AgentBeadsCheck struct {
	FixableCheck
}

// NewAgentBeadsCheck creates a new agent beads check.
func NewAgentBeadsCheck() *AgentBeadsCheck {
	return &AgentBeadsCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "agent-beads-exist",
				CheckDescription: "Verify agent beads exist for all agents",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// rigInfo holds the rig name and its beads path from routes.
type rigInfo struct {
	name      string // rig name (first component of path)
	prefix    string // routed database prefix without trailing hyphen
	beadsPath string // full path to beads directory relative to town root
}

func loadRegisteredRigInfos(townRoot, rigScope string) ([]rigInfo, error) {
	registry, err := config.LoadRigsConfig(filepath.Join(townRoot, "mayor", "rigs.json"))
	if err != nil {
		return nil, fmt.Errorf("loading rigs registry: %w", err)
	}
	routes, err := beads.LoadRoutes(filepath.Join(townRoot, ".beads"))
	if err != nil {
		return nil, fmt.Errorf("loading routes.jsonl: %w", err)
	}

	routeByRig := make(map[string]rigInfo)
	for _, route := range routes {
		name := strings.Split(route.Path, "/")[0]
		_, registered := registry.Rigs[name]
		canonical := route.Path == name || route.Path == name+"/mayor/rig"
		if !registered || !canonical {
			continue
		}
		info := rigInfo{name: name, prefix: strings.TrimSuffix(route.Prefix, "-"), beadsPath: route.Path}
		if previous, exists := routeByRig[name]; exists && (previous.prefix != info.prefix || previous.beadsPath != info.beadsPath) {
			return nil, fmt.Errorf("registered rig %s has multiple routes", name)
		}
		routeByRig[name] = info
	}

	names := make([]string, 0, len(registry.Rigs))
	for name := range registry.Rigs {
		if rigScope == "" || name == rigScope {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	infos := make([]rigInfo, 0, len(names))
	for _, name := range names {
		info, ok := routeByRig[name]
		if !ok {
			return nil, fmt.Errorf("registered rig %s has no canonical route", name)
		}
		infos = append(infos, info)
	}
	return infos, nil
}

type expectedAgentIdentity struct {
	id        string
	role      string
	rig       string
	beadsPath string
}

type agentIdentityCandidate struct {
	issue     *beads.Issue
	beadsPath string
	ephemeral bool
}

func (c agentIdentityCandidate) identityKey() string {
	return fmt.Sprintf("%s\x00%s\x00%t", filepath.Clean(c.beadsPath), c.issue.ID, c.ephemeral)
}

func loadIdentityCandidates(workDirs []string) ([]agentIdentityCandidate, error) {
	var candidates []agentIdentityCandidate
	for _, workDir := range workDirs {
		bd := beads.New(workDir)
		resolved := beads.ResolveBeadsDir(workDir)
		for _, query := range []struct {
			args      []string
			ephemeral bool
		}{
			{args: []string{"list", "--include-infra", "--status=all", "--json", "--flat", "--no-pager", "--limit=0"}},
			{args: []string{"query", "--json", "ephemeral=true", "--all", "--limit=0"}, ephemeral: true},
		} {
			out, err := bd.Run(query.args...)
			if err != nil {
				return nil, fmt.Errorf("inventorying identities in %s: %w", resolved, err)
			}
			if len(out) == 0 {
				continue
			}
			if !json.Valid(out) {
				return nil, fmt.Errorf("inventorying identities in %s: invalid JSON", resolved)
			}
			var issues []*beads.Issue
			if err := json.Unmarshal(out, &issues); err != nil {
				return nil, fmt.Errorf("inventorying identities in %s: %w", resolved, err)
			}
			for _, issue := range issues {
				candidates = append(candidates, agentIdentityCandidate{issue: issue, beadsPath: resolved, ephemeral: query.ephemeral})
			}
		}
	}
	return candidates, nil
}

// createOrCompareAgentIdentity is the fail-closed creation gate used by Doctor.
// It reuses one canonical object, rejects legacy/misrouted or ambiguous objects,
// and creates only when the inventory contains no matching identity.
func createOrCompareAgentIdentity(
	expected expectedAgentIdentity,
	candidates []agentIdentityCandidate,
	create func() (*beads.Issue, error),
) (*agentIdentityCandidate, error) {
	candidate, err := compareAgentIdentity(expected, candidates)
	if err != nil || candidate != nil {
		return candidate, err
	}

	issue, err := create()
	if err != nil {
		return nil, err
	}
	if issue == nil {
		return nil, fmt.Errorf("creating %s returned unexpected identity", expected.id)
	}
	created := agentIdentityCandidate{issue: issue, beadsPath: expected.beadsPath}
	validated, err := compareAgentIdentity(expected, []agentIdentityCandidate{created})
	if err != nil {
		return nil, err
	}
	if validated == nil {
		return nil, fmt.Errorf("creating %s returned unexpected identity", expected.id)
	}
	return validated, nil
}

func compareAgentIdentity(expected expectedAgentIdentity, candidates []agentIdentityCandidate) (*agentIdentityCandidate, error) {
	var matches []agentIdentityCandidate
	seen := make(map[string]bool)
	for _, candidate := range candidates {
		if candidate.issue == nil {
			continue
		}
		fields := beads.ParseAgentFields(candidate.issue.Description)
		metadataMatches := fields.RoleType == expected.role && fields.Rig == expected.rig
		parsedRig, parsedRole, _, parsed := beads.ParseAgentBeadID(candidate.issue.ID)
		idMatches := parsed && parsedRole == expected.role && parsedRig == expected.rig
		if candidate.issue.ID == expected.id && !metadataMatches {
			return nil, fmt.Errorf("agent identity %s has contradictory metadata: role=%q rig=%q", candidate.issue.ID, fields.RoleType, fields.Rig)
		}
		if !metadataMatches && !idMatches {
			continue
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
			return nil, fmt.Errorf("legacy or misplaced agent identity %s conflicts with %s", candidate.issue.ID, expected.id)
		}
		return &candidate, nil
	}
	return nil, nil
}

// Run checks if agent beads exist for all expected agents.
func (c *AgentBeadsCheck) Run(ctx *CheckContext) *CheckResult {
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

	townBeadsPath := beads.GetTownBeadsPath(ctx.TownRoot)
	workDirs := []string{townBeadsPath}
	for _, info := range allRigInfos {
		workDirs = append(workDirs, filepath.Join(ctx.TownRoot, info.beadsPath))
	}
	candidates, err := loadIdentityCandidates(workDirs)
	if err != nil {
		return &CheckResult{Name: c.Name(), Status: StatusError, Message: "Could not inventory agent identities", Details: []string{err.Error()}}
	}

	var missing []string
	var missingLabel []string
	var conflicts []string
	var checked int
	checkAgentBead := func(expected expectedAgentIdentity) {
		checked++
		candidate, err := compareAgentIdentity(expected, candidates)
		if err != nil {
			conflicts = append(conflicts, err.Error())
			return
		}
		if candidate == nil || candidate.issue.Status == "closed" {
			missing = append(missing, expected.id)
			return
		}
		if !beads.HasLabel(candidate.issue, "gt:agent") {
			missingLabel = append(missingLabel, expected.id)
		}
	}

	if ctx.RigName == "" {
		townPath := beads.ResolveBeadsDir(townBeadsPath)
		checkAgentBead(expectedAgentIdentity{id: beads.DeaconBeadIDTown(), role: "deacon", beadsPath: townPath})
		checkAgentBead(expectedAgentIdentity{id: beads.MayorBeadIDTown(), role: "mayor", beadsPath: townPath})
	}
	for _, info := range rigInfos {
		rigPath := beads.ResolveBeadsDir(filepath.Join(ctx.TownRoot, info.beadsPath))
		checkAgentBead(expectedAgentIdentity{id: beads.WitnessBeadIDWithPrefix(info.prefix, info.name), role: "witness", rig: info.name, beadsPath: rigPath})
		checkAgentBead(expectedAgentIdentity{id: beads.RefineryBeadIDWithPrefix(info.prefix, info.name), role: "refinery", rig: info.name, beadsPath: rigPath})
	}

	if len(conflicts) > 0 {
		return &CheckResult{Name: c.Name(), Status: StatusError, Message: fmt.Sprintf("%d agent identity conflict(s)", len(conflicts)), Details: conflicts}
	}
	if len(missing) > 0 {
		return &CheckResult{Name: c.Name(), Status: StatusError, Message: fmt.Sprintf("%d agent bead(s) missing", len(missing)), Details: missing, FixHint: "Run 'gt doctor --fix' to create missing agent beads"}
	}
	if len(missingLabel) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%d agent bead(s) missing gt:agent label", len(missingLabel)),
			Details: missingLabel,
			FixHint: "Run 'gt doctor --fix' to add missing labels",
		}
	}
	return &CheckResult{Name: c.Name(), Status: StatusOK, Message: fmt.Sprintf("All %d agent beads exist with gt:agent label", checked)}
}

// Fix creates missing agent beads and adds gt:agent labels to beads missing them.
func (c *AgentBeadsCheck) Fix(ctx *CheckContext) error {
	townBeadsPath := beads.GetTownBeadsPath(ctx.TownRoot)
	townBd := beads.New(townBeadsPath)
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
	workDirs := []string{townBeadsPath}
	for _, info := range allRigInfos {
		workDirs = append(workDirs, filepath.Join(ctx.TownRoot, info.beadsPath))
	}
	identityCandidates, err := loadIdentityCandidates(workDirs)
	if err != nil {
		return err
	}
	var errs []error

	// fixAgentBead ensures an agent bead exists and is open.
	// Logic:
	//   1. If in issues table → ensure gt:agent label
	//   2. If in wisps table (open) → ensure gt:agent label
	//   3. If exists but closed → REOPEN it (don't recreate)
	//   4. If truly missing → CREATE it
	// Uses CreateAgentBead which creates durable agent beads (not wisps)
	// so they survive wisp GC (GH#2768).
	// workDir is the rig directory for direct SQL fallback when bd update
	// fails silently (e.g., legacy prefixes that can't be routed — GH#2127).
	fixAgentBead := func(bd *beads.Beads, workDir, id, desc string, fields *beads.AgentFields) error {
		expected := expectedAgentIdentity{
			id:        id,
			role:      fields.RoleType,
			rig:       fields.Rig,
			beadsPath: beads.ResolveBeadsDir(workDir),
		}
		candidate, err := compareAgentIdentity(expected, identityCandidates)
		if err != nil {
			return err
		}
		if candidate != nil {
			if candidate.issue.Status == "closed" {
				openStatus := "open"
				if err := bd.Update(id, beads.UpdateOptions{Status: &openStatus}); err != nil {
					return fmt.Errorf("reopening closed agent bead %s: %w", id, err)
				}
			}
			if !beads.HasLabel(candidate.issue, "gt:agent") {
				if candidate.ephemeral {
					if err := bd.Update(id, beads.UpdateOptions{AddLabels: []string{"gt:agent"}}); err != nil {
						return fmt.Errorf("adding gt:agent label to wisp %s: %w", id, err)
					}
					if err := addWispLabelSQL(workDir, id, "gt:agent"); err != nil {
						return fmt.Errorf("persisting gt:agent wisp label for %s: %w", id, err)
					}
				} else {
					err := bd.Update(id, beads.UpdateOptions{AddLabels: []string{"gt:agent"}})
					if err != nil || !verifyLabelAdded(workDir, id, "gt:agent") {
						if sqlErr := addLabelSQL(workDir, id, "gt:agent"); sqlErr != nil {
							return fmt.Errorf("adding gt:agent label to %s: bd update: %v; SQL fallback: %w", id, err, sqlErr)
						}
					}
				}
			}
			return nil
		}

		// Bead truly missing — create it (CreateAgentBead handles ephemeral fallback)
		created, err := createOrCompareAgentIdentity(expected, identityCandidates, func() (*beads.Issue, error) {
			return bd.CreateAgentBead(id, desc, fields)
		})
		if err != nil {
			return fmt.Errorf("creating %s: %w", id, err)
		}
		identityCandidates = append(identityCandidates, *created)
		// Also insert into wisp_labels — CreateAgentBead may create a wisp-backed
		// bead where bd create --labels only writes to the labels table, not
		// wisp_labels. Doctor checks query wisps via JOIN wisp_labels, so the label
		// must exist there or the check still reports the bead as missing. See gt-3vx.
		_ = addWispLabelSQL(workDir, id, "gt:agent")
		return nil
	}

	if ctx.RigName == "" {
		deaconID := beads.DeaconBeadIDTown()
		if err := fixAgentBead(townBd, townBeadsPath, deaconID,
			"Deacon (daemon beacon) - receives mechanical heartbeats, runs town plugins and monitoring.",
			&beads.AgentFields{RoleType: "deacon", AgentState: "idle"},
		); err != nil {
			errs = append(errs, err)
		}

		mayorID := beads.MayorBeadIDTown()
		if err := fixAgentBead(townBd, townBeadsPath, mayorID,
			"Mayor - global coordinator, handles cross-rig communication and escalations.",
			&beads.AgentFields{RoleType: "mayor", AgentState: "idle"},
		); err != nil {
			errs = append(errs, err)
		}
	}

	if len(rigInfos) == 0 {
		return errors.Join(errs...)
	}

	// Fix agents for each rig
	for _, info := range rigInfos {
		prefix := info.prefix
		rigBeadsPath := filepath.Join(ctx.TownRoot, info.beadsPath)
		bd := beads.New(rigBeadsPath)
		rigName := info.name

		witnessID := beads.WitnessBeadIDWithPrefix(prefix, rigName)
		if err := fixAgentBead(bd, rigBeadsPath, witnessID,
			fmt.Sprintf("Witness for %s - monitors polecat health and progress.", rigName),
			&beads.AgentFields{RoleType: "witness", Rig: rigName, AgentState: "idle"},
		); err != nil {
			errs = append(errs, err)
		}

		refineryID := beads.RefineryBeadIDWithPrefix(prefix, rigName)
		if err := fixAgentBead(bd, rigBeadsPath, refineryID,
			fmt.Sprintf("Refinery for %s - processes merge queue.", rigName),
			&beads.AgentFields{RoleType: "refinery", Rig: rigName, AgentState: "idle"},
		); err != nil {
			errs = append(errs, err)
		}

	}

	return errors.Join(errs...)
}

// listCrewWorkers returns the names of canonical crew workers in a rig.
// Filters out git worktrees and other non-identity directories that may
// exist under <rig>/crew/ (e.g., fix branches, cross-rig worktrees).
// See GH#2767.
func listCrewWorkers(townRoot, rigName string) []string {
	crewDir := filepath.Join(townRoot, rigName, "crew")
	entries, err := os.ReadDir(crewDir)
	if err != nil {
		return nil // No crew directory or can't read it
	}

	var workers []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		// Git worktrees have a .git FILE (not directory) that contains
		// "gitdir: /path/to/main/.git/worktrees/<name>". Canonical crew
		// workers have a .git DIRECTORY (they are the main checkout).
		// Skip directories where .git is a file — they're worktrees.
		dotGit := filepath.Join(crewDir, entry.Name(), ".git")
		if info, err := os.Lstat(dotGit); err == nil && !info.IsDir() {
			continue // .git is a file → this is a worktree, not a crew identity
		}
		workers = append(workers, entry.Name())
	}
	return workers
}

// addLabelSQL adds a label to a bead via direct SQL INSERT.
// This bypasses bd's prefix routing, which silently fails for beads with
// legacy/unroutable prefixes (GH#2127).
func addLabelSQL(workDir, beadID, label string) error {
	escapedID := strings.ReplaceAll(beadID, "'", "''")
	escapedLabel := strings.ReplaceAll(label, "'", "''")
	query := fmt.Sprintf("INSERT IGNORE INTO labels (issue_id, label) VALUES ('%s', '%s')", escapedID, escapedLabel)
	return execBdSQLWrite(workDir, query)
}

// addWispLabelSQL adds a label to a wisp bead via direct SQL INSERT into wisp_labels.
// This is needed because bd create --labels=X only inserts into the labels table,
// not wisp_labels. Doctor checks and bd list for wisps join on wisp_labels to resolve
// labels, so the label must be present there for wisp-backed beads to be visible.
// See gt-3vx.
func addWispLabelSQL(workDir, beadID, label string) error {
	escapedID := strings.ReplaceAll(beadID, "'", "''")
	escapedLabel := strings.ReplaceAll(label, "'", "''")
	query := fmt.Sprintf("INSERT IGNORE INTO wisp_labels (issue_id, label) VALUES ('%s', '%s')", escapedID, escapedLabel)
	return execBdSQLWrite(workDir, query)
}

// verifyLabelAdded checks whether a label exists on a bead by querying labels table.
// Returns false if the label is not found or the query fails.
func verifyLabelAdded(workDir, beadID, label string) bool {
	escapedID := strings.ReplaceAll(beadID, "'", "''")
	escapedLabel := strings.ReplaceAll(label, "'", "''")
	query := fmt.Sprintf("SELECT 1 FROM labels WHERE issue_id = '%s' AND label = '%s' LIMIT 1", escapedID, escapedLabel)
	cmd := exec.Command("bd", "sql", query) //nolint:gosec // G204: query uses escaped internal values
	cmd.Dir = workDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	// bd sql returns header + data rows; if we got more than just a header, the label exists
	return strings.Contains(string(output), "1")
}

// listPolecats returns the names of canonical polecat directories in a rig.
// Filters out git worktrees (same logic as listCrewWorkers). See GH#2767.
func listPolecats(townRoot, rigName string) []string {
	polecatDir := filepath.Join(townRoot, rigName, "polecats")
	entries, err := os.ReadDir(polecatDir)
	if err != nil {
		return nil // No polecats directory or can't read it
	}

	var polecats []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dotGit := filepath.Join(polecatDir, entry.Name(), ".git")
		if info, err := os.Lstat(dotGit); err == nil && !info.IsDir() {
			continue // worktree — skip
		}
		polecats = append(polecats, entry.Name())
	}
	return polecats
}
