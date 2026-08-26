package cmd

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/crew"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// agentBeadUpserter captures the subset of bead operations needed by crew add.
// Using a narrow interface allows deterministic unit tests of crew bead creation
// behavior without requiring a live bd backend.
type agentBeadUpserter interface {
	CreateOrReopenAgentBead(id, title string, fields *beads.AgentFields) (*beads.Issue, error)
}

type crewWorkspaceManager interface {
	Add(name string, createBranch bool) (*crew.CrewWorker, error)
	Get(name string) (*crew.CrewWorker, error)
}

// upsertCrewAgentBead ensures the crew agent bead exists with expected metadata.
// It uses CreateOrReopenAgentBead instead of a Show()+Create sequence so existing
// beads in alternate stores (issues/wisps) do not trigger false "issue not found"
// warnings during crew creation.
func upsertCrewAgentBead(bd agentBeadUpserter, townRoot, rigName, crewName string) (string, error) {
	prefix := beads.GetPrefixForRig(townRoot, rigName)
	crewID := beads.CrewBeadIDWithPrefix(prefix, rigName, crewName)
	fields := &beads.AgentFields{
		RoleType:   "crew",
		Rig:        rigName,
		AgentState: "idle",
	}
	desc := fmt.Sprintf("Crew worker %s in %s - human-managed persistent workspace.", crewName, rigName)
	if _, err := bd.CreateOrReopenAgentBead(crewID, desc, fields); err != nil {
		return "", err
	}
	return crewID, nil
}

func ensureCrewAgentBead(manager crewWorkspaceManager, bd agentBeadUpserter, townRoot, rigName, crewName string, worker *crew.CrewWorker, addErr error) (*crew.CrewWorker, string, bool, error) {
	recovered := false
	if errors.Is(addErr, crew.ErrCrewExists) {
		var err error
		worker, err = manager.Get(crewName)
		if err != nil {
			return nil, "", false, addErr
		}
		// Add persists these fields only after the workspace is fully created.
		// Bare or crash-left directories therefore remain excluded identities.
		if worker == nil || worker.CreatedAt.IsZero() || worker.Branch == "" {
			return nil, "", false, fmt.Errorf("%w: workspace state is incomplete", crew.ErrCrewExists)
		}
		recovered = true
	} else if addErr != nil {
		return nil, "", false, addErr
	}

	crewID, err := upsertCrewAgentBead(bd, townRoot, rigName, crewName)
	return worker, crewID, recovered, err
}

func runCrewAdd(cmd *cobra.Command, args []string) error {
	// Deduplicate args to handle cases like "gt crew add foo --branch foo"
	// where "foo" appears twice because --branch is a boolean flag.
	// This prevents confusing "already exists" errors after a successful create.
	seen := make(map[string]bool)
	var dedupedArgs []string
	for _, arg := range args {
		if !seen[arg] {
			seen[arg] = true
			dedupedArgs = append(dedupedArgs, arg)
		}
	}
	args = dedupedArgs

	// Find workspace first (needed for all names)
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Load rigs config
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}

	// Determine base rig from --rig flag or first name's rig/name format
	baseRig := crewRig
	if baseRig == "" {
		// Check if first arg has rig/name format
		if parsedRig, _, ok := parseRigSlashName(args[0]); ok {
			baseRig = parsedRig
		}
	}
	if baseRig == "" {
		// Try to infer from cwd
		baseRig, err = inferRigFromCwd(townRoot)
		if err != nil {
			return fmt.Errorf("could not determine rig (use --rig flag): %w", err)
		}
	}

	// Get rig
	g := git.NewGit(townRoot)
	rigMgr := rig.NewManager(townRoot, rigsConfig, g)
	r, err := rigMgr.GetRig(baseRig)
	if err != nil {
		return fmt.Errorf("rig '%s' not found", baseRig)
	}

	// Create crew manager
	crewGit := git.NewGit(r.Path)
	crewMgr := crew.NewManager(r, crewGit)

	bd := beads.New(beads.ResolveBeadsDir(r.Path))
	return runCrewAddWith(args, baseRig, townRoot, crewBranch, crewMgr, bd)
}

func runCrewAddWith(args []string, baseRig, townRoot string, createBranch bool, crewMgr crewWorkspaceManager, bd agentBeadUpserter) error {
	// Track results
	var created []string
	var failed []string
	var lastWorker *crew.CrewWorker

	// Process each name
	for _, arg := range args {
		name := arg
		rigName := baseRig

		// Parse rig/name format (e.g., "beads/emma" -> rig=beads, name=emma)
		if parsedRig, crewName, ok := parseRigSlashName(arg); ok {
			// For rig/name format, use that rig (but warn if different from base)
			if parsedRig != baseRig {
				style.PrintWarning("%s: different rig '%s' ignored (use --rig to change)", arg, parsedRig)
			}
			name = crewName
		}

		// Create crew workspace
		fmt.Printf("Creating crew workspace %s in %s...\n", name, rigName)

		worker, addErr := crewMgr.Add(name, createBranch)
		worker, crewID, recovered, err := ensureCrewAgentBead(crewMgr, bd, townRoot, rigName, name, worker, addErr)
		if worker != nil && !recovered {
			fmt.Printf("%s Created crew workspace: %s/%s\n",
				style.Bold.Render("✓"), rigName, name)
			fmt.Printf("  Path: %s\n", worker.ClonePath)
			fmt.Printf("  Branch: %s\n", worker.Branch)
			created = append(created, name)
			lastWorker = worker
		}
		if err != nil {
			if worker != nil && !recovered {
				style.PrintWarning("could not create agent bead for %s: %v", name, err)
				failed = append(failed, name+" (agent bead)")
				fmt.Println()
				continue
			}
			if errors.Is(err, crew.ErrCrewExists) {
				style.PrintWarning("crew workspace '%s' already exists but is incomplete, skipping", name)
				failed = append(failed, name+" (exists)")
			} else {
				style.PrintWarning("creating crew workspace or agent bead '%s': %v", name, err)
				failed = append(failed, name)
			}
			continue
		}
		if recovered {
			fmt.Printf("%s Recovered crew agent bead: %s\n", style.Bold.Render("✓"), crewID)
		} else {
			fmt.Printf("  Agent bead: %s\n", crewID)
		}
		fmt.Println()
	}

	// Summary
	if len(created) > 0 {
		fmt.Printf("%s Created %d crew workspace(s): %v\n",
			style.Bold.Render("✓"), len(created), created)
		if lastWorker != nil && len(created) == 1 {
			fmt.Printf("\n%s\n", style.Dim.Render("Start working with: cd "+lastWorker.ClonePath))
		}
	}
	if len(failed) > 0 {
		fmt.Printf("%s Failed to complete %d crew workspace(s): %v\n",
			style.Warning.Render("!"), len(failed), failed)
	}

	if len(failed) > 0 {
		return fmt.Errorf("failed to complete %d crew workspace(s)", len(failed))
	}

	return nil
}
