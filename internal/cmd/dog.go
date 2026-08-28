package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dog"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/plugin"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Dog command flags
var (
	dogListJSON      bool
	dogStatusJSON    bool
	dogForce         bool
	dogRemoveAll     bool
	dogCallAll       bool
	dogDoneFinalizer string

	// Dispatch flags
	dogDispatchPlugin string
	dogDispatchRig    string
	dogDispatchCreate bool
	dogDispatchDog    string
	dogDispatchJSON   bool
	dogDispatchDryRun bool

	// Health-check flags
	dogHealthJSON          bool
	dogHealthAutoClear     bool
	dogHealthMaxInactivity time.Duration
)

var dogCmd = &cobra.Command{
	Use:     "dog",
	Aliases: []string{"dogs"},
	GroupID: GroupAgents,
	Short:   "Manage dogs (cross-rig infrastructure workers)",
	Long: `Manage dogs - reusable workers for infrastructure and cleanup.

CATS VS DOGS:
  Polecats (cats) build features. One rig. Ephemeral sessions (one task, then nuked).
  Dogs clean up messes. Cross-rig. Reusable (multiple tasks, eventually recycled).

Dogs are managed by the Deacon for town-level work:
  - Infrastructure tasks (rebuilding, syncing, migrations)
  - Cleanup operations (orphan branches, stale files)
  - Cross-rig work that spans multiple projects

Each dog has worktrees into every configured rig, enabling cross-project
operations. Dogs return to idle state after completing work (unlike cats).

The kennel is at ~/gt/deacon/dogs/. The Deacon dispatches work to dogs.`,
}

var dogAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Create a new dog in the kennel",
	Long: `Create a new dog in the kennel with multi-rig worktrees.

Each dog gets a worktree per configured rig (e.g., gastown, beads).
The dog starts in idle state, ready to receive work from the Deacon.

Example:
  gt dog add alpha
  gt dog add bravo`,
	Args: cobra.ExactArgs(1),
	RunE: runDogAdd,
}

var dogRemoveCmd = &cobra.Command{
	Use:   "remove <name>... | --all",
	Short: "Remove dogs from the kennel",
	Long: `Remove one or more dogs from the kennel.

Removes all worktrees and the dog directory.
Use --force to remove even if dog is in working state.

Examples:
  gt dog remove alpha
  gt dog remove alpha bravo
  gt dog remove --all
  gt dog remove alpha --force`,
	Args: func(cmd *cobra.Command, args []string) error {
		if dogRemoveAll {
			return nil
		}
		if len(args) < 1 {
			return fmt.Errorf("requires at least 1 dog name (or use --all)")
		}
		return nil
	},
	RunE: runDogRemove,
}

var dogListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List all dogs in the kennel",
	Long: `List all dogs in the kennel with their status.

Shows each dog's state (idle/working), current work assignment,
and last active timestamp.

Examples:
  gt dog list
  gt dog list --json`,
	RunE: runDogList,
}

var dogCallCmd = &cobra.Command{
	Use:   "call [name]",
	Short: "Wake idle dog(s) for work",
	Long: `Wake an idle dog to prepare for work.

With a name, wakes the specific dog.
With --all, wakes all idle dogs.
Without arguments, wakes one idle dog (if available).

This updates the dog's last-active timestamp and can trigger
session creation for the dog's worktrees.

Examples:
  gt dog call alpha
  gt dog call --all
  gt dog call`,
	RunE: runDogCall,
}

var dogDoneCmd = &cobra.Command{
	Use:   "done [name]",
	Short: "Mark dog as done and return to idle",
	Long: `Mark a dog as done with its current work and return to idle state.

Dogs should call this when they complete their work assignment.
This clears the work field and sets state to idle, making the dog
available for new work.

Without a name argument, auto-detects the current dog from the working
directory (must be run from within a dog's worktree).

Examples:
  gt dog done         # Auto-detect from cwd
  gt dog done alpha   # Explicit name`,
	Args: cobra.MaximumNArgs(1),
	RunE: runDogDone,
	Annotations: map[string]string{
		BrokerSafeAnnotation:      "true",
		brokerSafeArgsAnnotation:  brokerSafeArgsExactOne,
		brokerSafeFlagsAnnotation: "finalizer",
	},
}

var dogClearCmd = &cobra.Command{
	Use:   "clear <name>",
	Short: "Reset a stuck dog to idle state",
	Long: `Reset a stuck dog to idle state.

Use this when a dog is stuck in "working" state but its session has died.
The Deacon uses this during patrol to clear dogs that have timed out.

By default, refuses to clear a dog if its tmux session still exists.
Use --force to clear even if the session is alive.

Examples:
  gt dog clear alpha           # Clear if session is dead
  gt dog clear alpha --force   # Force clear even if session exists`,
	Args: cobra.ExactArgs(1),
	RunE: runDogClear,
}

var dogStatusCmd = &cobra.Command{
	Use:   "status [name]",
	Short: "Show detailed dog status",
	Long: `Show detailed status for a specific dog or summary for all dogs.

With a name, shows detailed info including:
  - State (idle/working)
  - Current work assignment
  - Worktree paths per rig
  - Last active timestamp

Without a name, shows pack summary:
  - Total dogs
  - Idle/working counts
  - Pack health

Examples:
  gt dog status alpha
  gt dog status
  gt dog status --json`,
	RunE: runDogStatus,
}

var dogDispatchCmd = &cobra.Command{
	Use:   "dispatch --plugin <name>",
	Short: "Dispatch plugin execution to a dog",
	Long: `Dispatch a plugin for execution by a dog worker.

This is the formalized command for sending plugin work to dogs. The Deacon
uses this during patrol cycles to dispatch plugins with open gates.

The command:
1. Finds the plugin definition (plugin.md)
2. Assigns work to an idle dog (marks as working)
3. Sends mail with plugin instructions to the dog
4. Returns immediately (non-blocking)

The dog discovers the work via its mail inbox and executes the plugin
instructions. On completion, the dog sends DOG_DONE mail to deacon/.

Examples:
  gt dog dispatch --plugin rebuild-gt
  gt dog dispatch --plugin rebuild-gt --rig gastown
  gt dog dispatch --plugin rebuild-gt --dog alpha
  gt dog dispatch --plugin rebuild-gt --create
  gt dog dispatch --plugin rebuild-gt --dry-run
  gt dog dispatch --plugin rebuild-gt --json`,
	RunE: runDogDispatch,
}

var dogHealthCheckCmd = &cobra.Command{
	Use:   "health-check [name]",
	Short: "Check dog health (zombies, hung, orphans)",
	Long: `Check dog health and detect problems.

Detects:
  - Zombies: state=working but tmux session or agent process is dead
  - Hung: agent alive but no tmux activity for too long
  - Orphans: dog idle but tmux session still exists

With --auto-clear, zombies are automatically returned to idle state.
Hung dogs are reported only (Deacon decides per ZFC principle).

Exit codes:
  0 = all healthy
  1 = error
  2 = needs attention

Examples:
  gt dog health-check
  gt dog health-check alpha
  gt dog health-check --json
  gt dog health-check --auto-clear
  gt dog health-check --max-inactivity 1h`,
	Args: cobra.MaximumNArgs(1),
	RunE: runDogHealthCheck,
}

func init() {
	// List flags
	dogListCmd.Flags().BoolVar(&dogListJSON, "json", false, "Output as JSON")

	// Remove flags
	dogRemoveCmd.Flags().BoolVarP(&dogForce, "force", "f", false, "Force removal even if working")
	dogRemoveCmd.Flags().BoolVar(&dogRemoveAll, "all", false, "Remove all dogs")

	// Call flags
	dogCallCmd.Flags().BoolVar(&dogCallAll, "all", false, "Wake all idle dogs")

	// Clear flags (reuses dogForce from remove)
	dogClearCmd.Flags().BoolVarP(&dogForce, "force", "f", false, "Force clear even if session exists")

	// Status flags
	dogStatusCmd.Flags().BoolVar(&dogStatusJSON, "json", false, "Output as JSON")

	dogDoneCmd.Flags().StringVar(&dogDoneFinalizer, "finalizer", "", "exact external closeout snapshot")
	_ = dogDoneCmd.Flags().MarkHidden("finalizer")

	// Dispatch flags
	dogDispatchCmd.Flags().StringVar(&dogDispatchPlugin, "plugin", "", "Plugin name to dispatch (required)")
	dogDispatchCmd.Flags().StringVar(&dogDispatchRig, "rig", "", "Limit plugin search to specific rig")
	dogDispatchCmd.Flags().StringVar(&dogDispatchDog, "dog", "", "Dispatch to specific dog (default: any idle)")
	dogDispatchCmd.Flags().BoolVar(&dogDispatchCreate, "create", false, "Create a dog if none idle")
	dogDispatchCmd.Flags().BoolVar(&dogDispatchJSON, "json", false, "Output as JSON")
	dogDispatchCmd.Flags().BoolVarP(&dogDispatchDryRun, "dry-run", "n", false, "Show what would be done without doing it")
	_ = dogDispatchCmd.MarkFlagRequired("plugin")

	// Health-check flags
	dogHealthCheckCmd.Flags().BoolVar(&dogHealthJSON, "json", false, "Output as JSON")
	dogHealthCheckCmd.Flags().BoolVar(&dogHealthAutoClear, "auto-clear", false, "Auto-clear zombie dogs")
	dogHealthCheckCmd.Flags().DurationVar(&dogHealthMaxInactivity, "max-inactivity", 10*time.Minute, "Max inactivity before considering hung")

	// Add subcommands
	dogCmd.AddCommand(dogAddCmd)
	dogCmd.AddCommand(dogRemoveCmd)
	dogCmd.AddCommand(dogListCmd)
	dogCmd.AddCommand(dogCallCmd)
	dogCmd.AddCommand(dogClearCmd)
	dogCmd.AddCommand(dogDoneCmd)
	dogCmd.AddCommand(dogStatusCmd)
	dogCmd.AddCommand(dogDispatchCmd)
	dogCmd.AddCommand(dogHealthCheckCmd)

	rootCmd.AddCommand(dogCmd)
}

// getDogManager creates a dog.Manager with the current town root.
//
// Use FindFromCwdOrError so we honor GT_TOWN_ROOT/GT_ROOT env vars when
// invoked from a dog worktree (e.g. ~/gt/deacon/dogs/alpha/<rig>/), where
// FindFromCwd alone might walk up to a non-town ancestor or stop at a path
// without mayor/rigs.json — which previously broke `gt dog done` and
// blocked DOG_DONE delivery (hq-zyvo).
func getDogManager() (*dog.Manager, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, fmt.Errorf("finding town root: %w", err)
	}

	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading rigs config: %w", err)
	}

	return dog.NewManager(townRoot, rigsConfig), nil
}

func runDogAdd(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Validate name
	if strings.ContainsAny(name, "/\\. ") {
		return fmt.Errorf("dog name cannot contain /, \\, ., or spaces")
	}

	mgr, err := getDogManager()
	if err != nil {
		return err
	}
	d, err := mgr.Add(name)
	if err != nil {
		return fmt.Errorf("adding dog %s: %w", name, err)
	}

	fmt.Printf("✓ Created dog %s in kennel\n", style.Bold.Render(name))
	fmt.Printf("  Path: %s\n", d.Path)
	fmt.Printf("  Worktrees:\n")
	for rigName, path := range d.Worktrees {
		fmt.Printf("    %s: %s\n", rigName, path)
	}

	// Create agent bead for the dog
	townRoot, _ := workspace.FindFromCwd()
	if townRoot != "" {
		b := beads.New(townRoot)
		location := filepath.Join("deacon", "dogs", name)

		issue, err := b.CreateDogAgentBead(name, location)
		if err != nil {
			// Non-fatal: warn but don't fail dog creation
			fmt.Printf("  Warning: could not create agent bead: %v\n", err)
		} else {
			fmt.Printf("  Agent bead: %s\n", issue.ID)
		}
	}

	return nil
}

func runDogRemove(cmd *cobra.Command, args []string) error {
	mgr, err := getDogManager()
	if err != nil {
		return err
	}
	var names []string
	if dogRemoveAll {
		dogs, err := mgr.List()
		if err != nil {
			return fmt.Errorf("listing dogs: %w", err)
		}
		for _, d := range dogs {
			names = append(names, d.Name)
		}
		if len(names) == 0 {
			fmt.Println("No dogs in kennel")
			return nil
		}
	} else {
		names = args
	}

	// Get beads client for cleanup
	townRoot, _ := workspace.FindFromCwd()
	var b *beads.Beads
	if townRoot != "" {
		b = beads.New(townRoot)
	}

	var removeErrors []string
	removed := 0

	for _, name := range names {
		d, err := mgr.Get(name)
		if err != nil {
			style.PrintWarning("dog %s not found, skipping", name)
			continue
		}
		controller := dogSessionControllerFromEnvironment()
		if d.SessionGeneration != nil {
			controller, err = dogSessionControllerFromSnapshot(d)
			if err != nil {
				removeErrors = append(removeErrors, fmt.Sprintf("%s: selecting exact dog runtime transport: %v", name, err))
				continue
			}
		}

		removedExact, err := removeDogExact(mgr, controller, d, dogForce)
		if err != nil {
			removeErrors = append(removeErrors, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if !removedExact {
			removeErrors = append(removeErrors, fmt.Sprintf("%s: lifecycle changed during removal; retry with a fresh snapshot", name))
			continue
		}

		fmt.Printf("✓ Removed dog %s\n", name)
		removed++

		// Reset agent bead for the dog (preserves persistent identity)
		if b != nil {
			if err := b.ResetDogAgentBead(name); err != nil {
				// Non-fatal: warn but don't fail dog removal
				fmt.Printf("  Warning: could not reset agent bead: %v\n", err)
			}
		}
	}

	if len(removeErrors) > 0 {
		fmt.Printf("\nSome removals failed:\n")
		for _, e := range removeErrors {
			fmt.Printf("  - %s\n", e)
		}
	}

	if removed > 0 {
		fmt.Printf("\n✓ Removed %d dog(s).\n", removed)
	}

	if len(removeErrors) > 0 {
		return fmt.Errorf("%d removal(s) failed", len(removeErrors))
	}

	return nil
}

func removeDogExact(mgr *dog.Manager, sessions dogSessionController, snapshot *dog.Dog, force bool) (bool, error) {
	if mgr == nil || sessions == nil || snapshot == nil {
		return false, errors.New("dog removal lifecycle evidence is unavailable")
	}
	return mgr.RemoveWithTeardownIfMatches(snapshot, force, func(stored *dog.SessionGeneration) error {
		sessionName := fmt.Sprintf("hq-dog-%s", snapshot.Name)
		if stored == nil {
			if snapshot.SessionAbsenceProven {
				return nil
			}
			return dog.ErrSessionGenerationUnavailable
		}

		expected := stored.Tmux()
		current, err := sessions.CaptureSessionGeneration(sessionName)
		if err == nil && !current.Equal(expected) {
			return tmux.ErrSessionGenerationChanged
		}
		if err != nil && !errors.Is(err, tmux.ErrSessionNotFound) && !errors.Is(err, tmux.ErrNoServer) {
			return fmt.Errorf("capturing exact dog session before removal: %w", err)
		}
		return teardownDogSessionGeneration(sessions, expected)
	})
}

func runDogList(cmd *cobra.Command, args []string) error {
	mgr, err := getDogManager()
	if err != nil {
		return err
	}

	dogs, err := mgr.List()
	if err != nil {
		return fmt.Errorf("listing dogs: %w", err)
	}

	if len(dogs) == 0 {
		if dogListJSON {
			fmt.Println("[]")
		} else {
			fmt.Println("No dogs in kennel")
		}
		return nil
	}

	if dogListJSON {
		type DogListItem struct {
			Name          string            `json:"name"`
			State         dog.State         `json:"state"`
			Work          string            `json:"work,omitempty"`
			WorkStartedAt *time.Time        `json:"work_started_at,omitempty"`
			LastActive    time.Time         `json:"last_active"`
			Worktrees     map[string]string `json:"worktrees,omitempty"`
		}

		var items []DogListItem
		for _, d := range dogs {
			item := DogListItem{
				Name:       d.Name,
				State:      d.State,
				Work:       d.Work,
				LastActive: d.LastActive,
				Worktrees:  d.Worktrees,
			}
			if !d.WorkStartedAt.IsZero() {
				t := d.WorkStartedAt
				item.WorkStartedAt = &t
			}
			items = append(items, item)
		}

		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(items)
	}

	// Pretty print
	fmt.Println(style.Bold.Render("The Pack"))
	fmt.Println()

	idleCount := 0
	workingCount := 0

	for _, d := range dogs {
		stateIcon := "○"
		stateStyle := style.Dim
		if d.State == dog.StateWorking {
			stateIcon = "●"
			stateStyle = style.Bold
			workingCount++
		} else {
			idleCount++
		}

		line := fmt.Sprintf("  %s %s", stateIcon, stateStyle.Render(d.Name))
		if d.Work != "" {
			line += fmt.Sprintf(" → %s", style.Dim.Render(d.Work))
		}
		fmt.Println(line)
	}

	fmt.Println()
	fmt.Printf("  %d idle, %d working\n", idleCount, workingCount)

	return nil
}

func runDogCall(cmd *cobra.Command, args []string) error {
	mgr, err := getDogManager()
	if err != nil {
		return err
	}

	if dogCallAll {
		// Wake all idle dogs
		dogs, err := mgr.List()
		if err != nil {
			return fmt.Errorf("listing dogs: %w", err)
		}

		woken := 0
		for _, d := range dogs {
			if d.State == dog.StateIdle && d.SessionGeneration == nil && d.SessionAbsenceProven {
				if err := mgr.SetState(d.Name, dog.StateIdle); err != nil {
					style.PrintWarning("failed to wake %s: %v", d.Name, err)
					continue
				}
				woken++
				fmt.Printf("✓ Called %s\n", d.Name)
			}
		}

		if woken == 0 {
			fmt.Println("No idle dogs to call")
		} else {
			fmt.Printf("\n%d dog(s) ready\n", woken)
		}
		return nil
	}

	if len(args) > 0 {
		// Wake specific dog
		name := args[0]
		d, err := mgr.Get(name)
		if err != nil {
			return fmt.Errorf("getting dog %s: %w", name, err)
		}

		if d.State == dog.StateWorking {
			fmt.Printf("Dog %s is already working (use 'gt dog done %s' when complete)\n", name, name)
			return nil
		}
		if d.SessionGeneration != nil || !d.SessionAbsenceProven {
			return fmt.Errorf("waking dog %s: %w", name, dog.ErrSessionGenerationUnavailable)
		}

		if err := mgr.SetState(name, dog.StateIdle); err != nil {
			return fmt.Errorf("waking dog %s: %w", name, err)
		}

		fmt.Printf("✓ Called %s - ready for work\n", name)
		return nil
	}

	// Wake one idle dog
	d, err := mgr.GetIdleDog()
	if err != nil {
		return fmt.Errorf("getting idle dog: %w", err)
	}

	if d == nil {
		fmt.Println("No idle dogs available")
		return nil
	}

	if err := mgr.SetState(d.Name, dog.StateIdle); err != nil {
		return fmt.Errorf("waking dog %s: %w", d.Name, err)
	}

	fmt.Printf("✓ Called %s - ready for work\n", d.Name)
	return nil
}

func runDogClear(cmd *cobra.Command, args []string) error {
	name := args[0]

	mgr, err := getDogManager()
	if err != nil {
		return err
	}

	d, err := mgr.Get(name)
	if err != nil {
		return fmt.Errorf("getting dog %s: %w", name, err)
	}

	alreadyIdle := d.State == dog.StateIdle && d.Work == "" &&
		d.SessionGeneration == nil && d.SessionAbsenceProven

	// Clear only after exact absence or generation-bound teardown. --force
	// bypasses the liveness refusal; it never bypasses identity custody. Select
	// the controller first so liveness and teardown address the same durable
	// endpoint instead of reusing a socket alias under an ambient socket root.
	controller := dogSessionControllerFromEnvironment()
	if d.SessionGeneration != nil {
		controller, err = dogSessionControllerFromSnapshot(d)
		if err != nil {
			return fmt.Errorf("selecting exact dog runtime transport: %w", err)
		}
	}
	if err := clearDogSnapshot(mgr, controller, d, dogForce); err != nil {
		return fmt.Errorf("clearing dog %s: %w", name, err)
	}
	if alreadyIdle {
		fmt.Printf("Dog %s is already idle\n", name)
		return nil
	}

	fmt.Printf("✓ Cleared dog %s (now idle)\n", name)
	if d.Work != "" {
		fmt.Printf("  Previous work: %s\n", d.Work)
	}
	return nil
}

func runDogDone(cmd *cobra.Command, args []string) error {
	mgr, err := getDogManager()
	if err != nil {
		return err
	}

	var finalizerSnapshot *dog.Dog
	isFinalizer := strings.TrimSpace(dogDoneFinalizer) != ""
	if isFinalizer {
		if !dogCloseoutFinalizerAuthorized(dogDoneFinalizer) {
			return errors.New("dog closeout finalizer did not arrive through the trusted host boundary")
		}
		finalizerSnapshot, err = dogCloseoutSnapshotFromEncoded(dogDoneFinalizer)
	} else {
		finalizerSnapshot, isFinalizer, err = dogCloseoutSnapshotFromEnvironment()
	}
	if err != nil {
		return err
	}
	var name string
	if isFinalizer {
		name = finalizerSnapshot.Name
		if len(args) > 0 && args[0] != name {
			return errors.New("dog closeout finalizer name does not match snapshot")
		}
	} else if len(args) > 0 {
		name = args[0]
	} else {
		// Auto-detect dog from cwd
		// Dog worktrees are at ~/gt/deacon/dogs/<name>/<rig>/
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("getting cwd: %w", err)
		}

		// Look for /deacon/dogs/<name>/ in path
		parts := splitPathComponents(cwd)
		for i := 0; i < len(parts)-1; i++ {
			if parts[i] == "dogs" && i > 0 && parts[i-1] == "deacon" {
				name = parts[i+1]
				break
			}
		}

		if name == "" {
			return fmt.Errorf("could not detect dog name from cwd: %s\nRun from a dog worktree or specify name: gt dog done <name>", cwd)
		}
	}

	d := finalizerSnapshot
	if !isFinalizer {
		d, err = mgr.Get(name)
		if err != nil {
			return fmt.Errorf("getting dog %s: %w", name, err)
		}
	}

	// Close accumulated plugin mails only after durable work and exact runtime
	// teardown agree. Incomplete closeout leaves all recovery evidence intact.
	controller := dogSessionControllerFromEnvironment()
	if d.SessionGeneration != nil {
		controller, err = dogSessionControllerFromSnapshot(d)
		if err != nil {
			return fmt.Errorf("selecting exact dog runtime transport: %w", err)
		}
	}
	if !isFinalizer && d.SessionGeneration != nil {
		if os.Getenv("GT_ROLE") == "dog" && os.Getenv("GT_DOG_NAME") == d.Name {
			handled, brokerErr := requestDogCloseoutBroker(d)
			if handled {
				if brokerErr != nil {
					return fmt.Errorf("scheduling brokered closeout for dog %s: %w", name, brokerErr)
				}
				fmt.Printf("✓ Dog %s closeout handed to the host broker\n", name)
				return nil
			}
		}
		currentSession, resolveErr := controller.ResolveCurrentSession()
		if resolveErr == nil && currentSession == fmt.Sprintf("hq-dog-%s", d.Name) {
			if err := scheduleDogCloseoutFinalizer(controller, d); err != nil {
				return fmt.Errorf("scheduling external closeout for dog %s: %w", name, err)
			}
			fmt.Printf("✓ Dog %s closeout handed to the tmux server\n", name)
			return nil
		}
	}
	if err := completeDogCloseout(mgr, controller, d); err != nil {
		return fmt.Errorf("closing out dog %s: %w", name, err)
	}

	fmt.Printf("✓ Dog %s returned to kennel (idle)\n", name)
	return nil
}

type dogCloseoutManager interface {
	ClearWorkWithFinalizeIfMatches(name, expectedWork string, expectedStartedAt time.Time, finalize func() error) (bool, error)
	CompleteWorkWithTeardownAndFinalizeIfMatches(name, expectedWork string, expectedStartedAt time.Time, expectedGeneration tmux.SessionGeneration, teardown func(tmux.SessionGeneration) error, finalize func() error) (bool, error)
	RetireSessionWithTeardownIfMatches(name string, expectedGeneration tmux.SessionGeneration, teardown func(tmux.SessionGeneration) error) (bool, error)
}

type dogSessionController interface {
	HasSession(name string) (bool, error)
	CaptureSessionGeneration(name string) (tmux.SessionGeneration, error)
	KillSessionGenerationWithProcessesContext(context.Context, tmux.SessionGeneration) error
	KillSessionGenerationWithProcessesPortableContext(context.Context, tmux.SessionGeneration) error
}

func clearDogSnapshot(mgr dogCloseoutManager, controller dogSessionController, snapshot *dog.Dog, force bool) error {
	if snapshot == nil {
		return errors.New("dog closeout snapshot is unavailable")
	}
	if snapshot.SessionGeneration == nil && !snapshot.SessionAbsenceProven {
		return fmt.Errorf("%w: dog session absence is unproven", errDogCloseoutIncomplete)
	}
	if !force && snapshot.SessionGeneration != nil {
		sessionName := fmt.Sprintf("hq-dog-%s", snapshot.Name)
		has, err := controller.HasSession(sessionName)
		if err != nil {
			return fmt.Errorf("checking exact dog session %s: %w", sessionName, err)
		}
		if has {
			return fmt.Errorf("dog %s has an active session (%s)\nUse --force to clear anyway", snapshot.Name, sessionName)
		}
	}
	return completeDogCloseout(mgr, controller, snapshot)
}

var errDogCloseoutIncomplete = errors.New("dog closeout incomplete")

const dogCloseoutFinalizerEnv = "GT_DOG_CLOSEOUT_FINALIZER"

const dogCloseoutHostSessionEnv = "GT_INTERNAL_DOG_CLOSEOUT_SESSION"

const dogCloseoutTeardownTimeout = 15 * time.Second

const dogCloseoutHostHandoffTimeout = 2 * time.Minute

type durableDogCloseoutSnapshot struct {
	Name              string                 `json:"name"`
	State             dog.State              `json:"state"`
	Work              string                 `json:"work"`
	WorkStartedAt     time.Time              `json:"work_started_at"`
	SessionGeneration *dog.SessionGeneration `json:"session_generation"`
}

func dogCloseoutSnapshotFromEnvironment() (*dog.Dog, bool, error) {
	encoded := strings.TrimSpace(os.Getenv(dogCloseoutFinalizerEnv))
	if encoded == "" {
		return nil, false, nil
	}
	if !dogCloseoutFinalizerAuthorized(encoded) {
		return nil, true, errors.New("dog closeout finalizer did not arrive through the trusted host boundary")
	}
	snapshot, err := dogCloseoutSnapshotFromEncoded(encoded)
	return snapshot, true, err
}

func dogCloseoutSnapshotFromEncoded(encoded string) (*dog.Dog, error) {
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decoding dog closeout snapshot: %w", err)
	}
	var snapshot durableDogCloseoutSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, fmt.Errorf("reading dog closeout snapshot: %w", err)
	}
	if snapshot.Name == "" || snapshot.SessionGeneration == nil {
		return nil, errors.New("dog closeout snapshot is incomplete")
	}
	return &dog.Dog{
		Name:              snapshot.Name,
		State:             snapshot.State,
		Work:              snapshot.Work,
		WorkStartedAt:     snapshot.WorkStartedAt,
		SessionGeneration: snapshot.SessionGeneration,
	}, nil
}

func dogSessionControllerFromEnvironment() *tmux.Tmux {
	// NewTmux prefers the registry-initialized town transport and consults the
	// environment only when no town has been initialized. Exact generations use
	// dogSessionControllerFromSnapshot instead and never depend on ambient state.
	return tmux.NewTmux()
}

func dogSessionControllerFromSnapshot(snapshot *dog.Dog) (*tmux.Tmux, error) {
	if snapshot == nil || snapshot.SessionGeneration == nil {
		return nil, fmt.Errorf("%w: durable tmux generation is unavailable", errDogCloseoutIncomplete)
	}
	controller, err := tmux.NewTmuxForSessionGeneration(snapshot.SessionGeneration.Tmux())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errDogCloseoutIncomplete, err)
	}
	return controller, nil
}

func dogCloseoutHostSessionAuthorized(encoded string) bool {
	if encoded == "" || os.Getenv(dogCloseoutFinalizerEnv) != encoded {
		return false
	}
	snapshot, err := dogCloseoutSnapshotFromEncoded(encoded)
	if err != nil {
		return false
	}
	sessionName := strings.TrimSpace(os.Getenv(dogCloseoutHostSessionEnv))
	if !strings.HasPrefix(sessionName, "hq-dog-finalizer-"+snapshot.Name+"-") {
		return false
	}
	controller, err := dogSessionControllerFromSnapshot(snapshot)
	if err != nil {
		return false
	}
	current, err := controller.ResolveCurrentSession()
	return err == nil && current == sessionName
}

var runDogCloseoutBroker = tmux.RunSessionBrokerClient

func dogCloseoutFinalizerRequest(snapshot *dog.Dog) (string, []string, error) {
	if snapshot == nil || snapshot.SessionGeneration == nil {
		return "", nil, errors.New("dog closeout scheduling evidence unavailable")
	}
	payload, err := json.Marshal(durableDogCloseoutSnapshot{
		Name:              snapshot.Name,
		State:             snapshot.State,
		Work:              snapshot.Work,
		WorkStartedAt:     snapshot.WorkStartedAt,
		SessionGeneration: snapshot.SessionGeneration,
	})
	if err != nil {
		return "", nil, fmt.Errorf("encoding dog closeout snapshot: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded, []string{"dog", "done", snapshot.Name, "--finalizer", encoded}, nil
}

func requestDogCloseoutBroker(snapshot *dog.Dog) (bool, error) {
	_, finalizerArgs, err := dogCloseoutFinalizerRequest(snapshot)
	if err != nil {
		return true, err
	}
	handled, exitCode, brokerErr := runDogCloseoutBroker(finalizerArgs)
	if !handled {
		return false, nil
	}
	if brokerErr != nil {
		return true, fmt.Errorf("requesting host-brokered dog closeout: %w", brokerErr)
	}
	if exitCode != 0 {
		return true, fmt.Errorf("host-brokered dog closeout exited with status %d", exitCode)
	}
	return true, nil
}

func scheduleDogCloseoutFinalizer(controller *tmux.Tmux, snapshot *dog.Dog) error {
	if controller == nil {
		return errors.New("dog closeout scheduling evidence unavailable")
	}
	encoded, finalizerArgs, err := dogCloseoutFinalizerRequest(snapshot)
	if err != nil {
		return err
	}
	if handled, brokerErr := requestDogCloseoutBroker(snapshot); handled {
		return brokerErr
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving current executable: %w", err)
	}
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("resolving town root for dog closeout: %w", err)
	}
	environment := map[string]string{
		dogCloseoutFinalizerEnv:      encoded,
		"GT_TOWN_ROOT":               townRoot,
		"GT_ROOT":                    townRoot,
		"GT_ROLE":                    "dog",
		"GT_DOG_NAME":                snapshot.Name,
		"BD_ACTOR":                   "dog",
		"GT_TOWN_SOCKET":             os.Getenv("GT_TOWN_SOCKET"),
		"GT_TEST_CMD_EXECUTE_HELPER": os.Getenv("GT_TEST_CMD_EXECUTE_HELPER"),
	}
	finalizerSession := "hq-dog-finalizer-" + snapshot.Name + "-" + uuid.NewString()
	environment[dogCloseoutHostSessionEnv] = finalizerSession
	hostGeneration, waitForHandoff, err := scheduleDogCloseoutHostFallback(
		controller,
		finalizerSession,
		executable,
		townRoot,
		finalizerArgs,
		environment,
	)
	if err != nil || !waitForHandoff {
		return err
	}
	return waitForDogCloseoutHostHandoff(controller, snapshot, hostGeneration)
}

func waitForDogCloseoutHostHandoff(
	controller *tmux.Tmux,
	snapshot *dog.Dog,
	hostGeneration tmux.SessionGeneration,
) error {
	if controller == nil || snapshot == nil || snapshot.SessionGeneration == nil || hostGeneration.Name == "" {
		return errors.New("dog closeout host handoff evidence unavailable")
	}
	target := snapshot.SessionGeneration.Tmux()
	deadline := time.Now().Add(dogCloseoutHostHandoffTimeout)
	for time.Now().Before(deadline) {
		currentTarget, targetErr := controller.CaptureSessionGeneration(target.Name)
		switch {
		case errors.Is(targetErr, tmux.ErrSessionNotFound), errors.Is(targetErr, tmux.ErrNoServer):
			return nil
		case targetErr != nil:
			return fmt.Errorf("checking target during dog closeout host handoff: %w", targetErr)
		case !currentTarget.Equal(target):
			cleanupErr := teardownDogSessionGeneration(controller, hostGeneration)
			return errors.Join(
				tmux.ErrSessionGenerationChanged,
				cleanupErr,
			)
		}

		currentHost, hostErr := controller.CaptureSessionGeneration(hostGeneration.Name)
		switch {
		case errors.Is(hostErr, tmux.ErrSessionNotFound), errors.Is(hostErr, tmux.ErrNoServer):
			return errors.New("dog closeout host finalizer exited before exact target teardown")
		case hostErr != nil:
			hostExists, existsErr := controller.HasSession(hostGeneration.Name)
			if existsErr != nil {
				return errors.Join(
					fmt.Errorf("checking dog closeout host finalizer: %w", hostErr),
					fmt.Errorf("confirming dog closeout host finalizer presence: %w", existsErr),
				)
			}
			if !hostExists {
				return errors.New("dog closeout host finalizer exited before exact target teardown")
			}
			return fmt.Errorf("checking dog closeout host finalizer: %w", hostErr)
		case !currentHost.Equal(hostGeneration):
			return errors.New("dog closeout host finalizer generation changed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cleanupErr := teardownDogSessionGeneration(controller, hostGeneration)
	return errors.Join(context.DeadlineExceeded, cleanupErr)
}

func completeDogCloseout(mgr dogCloseoutManager, sessions dogSessionController, snapshot *dog.Dog) error {
	return completeDogCloseoutWithFinalize(mgr, sessions, snapshot, nil)
}

func completeDogCloseoutWithFinalize(
	mgr dogCloseoutManager,
	sessions dogSessionController,
	snapshot *dog.Dog,
	finalize func() error,
) error {
	if mgr == nil || sessions == nil || snapshot == nil {
		return fmt.Errorf("%w: lifecycle evidence unavailable", errDogCloseoutIncomplete)
	}
	if snapshot.SessionGeneration == nil {
		if !snapshot.SessionAbsenceProven {
			return fmt.Errorf("%w: dog session absence is unproven", errDogCloseoutIncomplete)
		}
		if snapshot.State == dog.StateIdle && snapshot.Work == "" {
			return nil
		}
		completed, err := mgr.ClearWorkWithFinalizeIfMatches(
			snapshot.Name, snapshot.Work, snapshot.WorkStartedAt, finalize,
		)
		if err != nil {
			return fmt.Errorf("completing generation-free dog work: %w", err)
		}
		if !completed {
			return fmt.Errorf("%w: work assignment or absence proof changed", errDogCloseoutIncomplete)
		}
		return nil
	}

	sessionName := fmt.Sprintf("hq-dog-%s", snapshot.Name)
	current, captureErr := sessions.CaptureSessionGeneration(sessionName)

	expected := snapshot.SessionGeneration.Tmux()
	if !expected.Transport.Bound {
		return fmt.Errorf("%w: %w", errDogCloseoutIncomplete, tmux.ErrSessionTransportUnbound)
	}
	if captureErr != nil {
		if !errors.Is(captureErr, tmux.ErrSessionNotFound) && !errors.Is(captureErr, tmux.ErrNoServer) {
			return fmt.Errorf("%w: session identity could not be verified", errDogCloseoutIncomplete)
		}
		completed, err := completeDogCloseoutState(mgr, snapshot, expected, func(generation tmux.SessionGeneration) error {
			return teardownDogSessionGeneration(sessions, generation)
		}, finalize)
		if err != nil {
			return fmt.Errorf("completing work after proven session absence: %w", err)
		}
		if !completed {
			return fmt.Errorf("%w: work assignment or generation changed", errDogCloseoutIncomplete)
		}
		return nil
	}
	if !current.Equal(expected) {
		return fmt.Errorf("%w: live session does not match the persisted generation", errDogCloseoutIncomplete)
	}

	completed, err := completeDogCloseoutState(mgr, snapshot, expected, func(generation tmux.SessionGeneration) error {
		return teardownDogSessionGeneration(sessions, generation)
	}, finalize)
	if err != nil {
		return fmt.Errorf("%w: exact dog session teardown failed: %w", errDogCloseoutIncomplete, err)
	}
	if !completed {
		return fmt.Errorf("%w: work assignment or generation changed", errDogCloseoutIncomplete)
	}
	return nil
}

func teardownDogSessionGeneration(sessions dogSessionController, generation tmux.SessionGeneration) error {
	ctx, cancel := context.WithTimeout(context.Background(), dogCloseoutTeardownTimeout)
	defer cancel()
	if os.Getenv(tmux.EnvSessionBrokerWorker) == "1" {
		return sessions.KillSessionGenerationWithProcessesContext(ctx, generation)
	}
	return sessions.KillSessionGenerationWithProcessesPortableContext(ctx, generation)
}

func completeDogCloseoutState(
	mgr dogCloseoutManager,
	snapshot *dog.Dog,
	expected tmux.SessionGeneration,
	teardown func(tmux.SessionGeneration) error,
	finalize func() error,
) (bool, error) {
	if snapshot.State == dog.StateIdle && snapshot.Work == "" {
		return mgr.RetireSessionWithTeardownIfMatches(snapshot.Name, expected, teardown)
	}
	return mgr.CompleteWorkWithTeardownAndFinalizeIfMatches(
		snapshot.Name,
		snapshot.Work,
		snapshot.WorkStartedAt,
		expected,
		teardown,
		finalize,
	)
}

func splitPathComponents(path string) []string {
	if path == "" {
		return nil
	}

	return strings.FieldsFunc(path, func(r rune) bool {
		return r == '/' || r == '\\'
	})
}

func runDogStatus(cmd *cobra.Command, args []string) error {
	mgr, err := getDogManager()
	if err != nil {
		return err
	}

	if len(args) > 0 {
		// Show specific dog status
		name := args[0]
		return showDogStatus(mgr, name)
	}

	// Show pack summary
	return showPackStatus(mgr)
}

type dogSessionState string

const (
	dogSessionRunning dogSessionState = "running"
	dogSessionAbsent  dogSessionState = "absent"
	dogSessionStale   dogSessionState = "stale"
	dogSessionUnknown dogSessionState = "unknown"
)

type dogSessionStatus struct {
	Name            string
	State           dogSessionState
	GenerationMatch bool
	Diagnostic      string
	Err             error
}

func inspectDogSession(d *dog.Dog, sessions dogSessionController) dogSessionStatus {
	status := dogSessionStatus{Name: fmt.Sprintf("hq-dog-%s", d.Name)}
	current, err := sessions.CaptureSessionGeneration(status.Name)
	if errors.Is(err, tmux.ErrSessionNotFound) || errors.Is(err, tmux.ErrNoServer) {
		status.State = dogSessionAbsent
		return status
	}
	if err != nil {
		status.State = dogSessionUnknown
		status.Diagnostic = "session identity could not be verified"
		status.Err = err
		return status
	}
	if d.SessionGeneration == nil {
		status.State = dogSessionUnknown
		status.Diagnostic = "live session has no persisted generation"
		return status
	}
	if d.SessionGeneration.EqualTmux(current) {
		status.State = dogSessionRunning
		status.GenerationMatch = true
		return status
	}
	status.State = dogSessionStale
	status.Diagnostic = "live session does not match persisted generation"
	return status
}

func writeDogStatus(w io.Writer, d *dog.Dog, sessionStatus dogSessionStatus, jsonOutput bool) error {
	if jsonOutput {
		payload := struct {
			*dog.Dog
			SessionName     string          `json:"session_name"`
			SessionState    dogSessionState `json:"session_state"`
			GenerationMatch bool            `json:"generation_match"`
			Diagnostic      string          `json:"diagnostic,omitempty"`
		}{
			Dog:             d,
			SessionName:     sessionStatus.Name,
			SessionState:    sessionStatus.State,
			GenerationMatch: sessionStatus.GenerationMatch,
			Diagnostic:      sessionStatus.Diagnostic,
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(payload)
	}

	fmt.Fprintf(w, "Dog: %s\n\n", style.Bold.Render(d.Name))
	fmt.Fprintf(w, "  Work State:  %s\n", d.State)
	if d.Work != "" {
		fmt.Fprintf(w, "  Work:        %s\n", d.Work)
	} else {
		fmt.Fprintf(w, "  Work:        %s\n", style.Dim.Render("(none)"))
	}
	fmt.Fprintf(w, "  Session:     %s (%s)\n", sessionStatus.Name, sessionStatus.State)
	if sessionStatus.Diagnostic != "" {
		fmt.Fprintf(w, "  Diagnostic:  %s\n", sessionStatus.Diagnostic)
	}
	fmt.Fprintf(w, "  Path:        %s\n", d.Path)
	fmt.Fprintf(w, "  Last Active: %s\n", dogFormatTimeAgo(d.LastActive))
	fmt.Fprintf(w, "  Created:     %s\n", d.CreatedAt.Format("2006-01-02 15:04"))

	if len(d.Worktrees) > 0 {
		fmt.Fprintln(w, "\nWorktrees:")
		for rigName, path := range d.Worktrees {
			exists := "✓"
			if _, err := os.Stat(path); os.IsNotExist(err) {
				exists = "✗"
			}
			fmt.Fprintf(w, "  %s %s: %s\n", exists, rigName, path)
		}
	}
	return nil
}

func showDogStatus(mgr *dog.Manager, name string) error {
	d, err := mgr.Get(name)
	if err != nil {
		return fmt.Errorf("getting dog %s: %w", name, err)
	}

	sessionStatus := inspectDogSession(d, tmux.NewTmux())
	if sessionStatus.Err != nil {
		return fmt.Errorf("checking dog session %s: %w", sessionStatus.Name, sessionStatus.Err)
	}
	return writeDogStatus(os.Stdout, d, sessionStatus, dogStatusJSON)
}

func showPackStatus(mgr *dog.Manager) error {
	dogs, err := mgr.List()
	if err != nil {
		return fmt.Errorf("listing dogs: %w", err)
	}

	if dogStatusJSON {
		type PackStatus struct {
			Total     int    `json:"total"`
			Idle      int    `json:"idle"`
			Working   int    `json:"working"`
			KennelDir string `json:"kennel_dir"`
		}

		townRoot, _ := workspace.FindFromCwd()
		status := PackStatus{
			Total:     len(dogs),
			KennelDir: filepath.Join(townRoot, "deacon", "dogs"),
		}
		for _, d := range dogs {
			if d.State == dog.StateIdle {
				status.Idle++
			} else {
				status.Working++
			}
		}

		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}

	fmt.Println(style.Bold.Render("Pack Status"))
	fmt.Println()

	if len(dogs) == 0 {
		fmt.Println("  No dogs in kennel")
		fmt.Println()
		fmt.Println("  Use 'gt dog add <name>' to add a dog")
		return nil
	}

	idleCount := 0
	workingCount := 0
	for _, d := range dogs {
		if d.State == dog.StateIdle {
			idleCount++
		} else {
			workingCount++
		}
	}

	fmt.Printf("  Total:   %d\n", len(dogs))
	fmt.Printf("  Idle:    %d\n", idleCount)
	fmt.Printf("  Working: %d\n", workingCount)

	if idleCount > 0 {
		fmt.Println()
		fmt.Println(style.Dim.Render("  Ready for work. Use 'gt dog call' to wake."))
	}

	return nil
}

// dogFormatTimeAgo formats a time as a relative string like "2 hours ago".
func dogFormatTimeAgo(t time.Time) string {
	if t.IsZero() {
		return "(unknown)"
	}

	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		mins := int(d.Minutes())
		if mins == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", mins)
	case d < 24*time.Hour:
		hours := int(d.Hours())
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	}
}

func runDogHealthCheck(cmd *cobra.Command, args []string) error {
	mgr, err := getDogManager()
	if err != nil {
		return err
	}

	tm := tmux.NewTmux()
	hc := dog.NewHealthChecker(mgr, tm)

	var results []dog.DogHealthResult

	if len(args) > 0 {
		// Single dog
		d, err := mgr.Get(args[0])
		if err != nil {
			return fmt.Errorf("getting dog %s: %w", args[0], err)
		}
		r := hc.Check(d, dogHealthMaxInactivity, dogHealthAutoClear)
		results = []dog.DogHealthResult{r}
	} else {
		// All dogs
		results, err = hc.CheckAll(dogHealthMaxInactivity, dogHealthAutoClear)
		if err != nil {
			return err
		}
	}

	attention := dog.NeedsAttentionCount(results)

	if dogHealthJSON {
		type HealthReport struct {
			Dogs           []dog.DogHealthResult `json:"dogs"`
			NeedsAttention int                   `json:"needs_attention"`
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(HealthReport{Dogs: results, NeedsAttention: attention}); err != nil {
			return err
		}
	} else {
		if len(results) == 0 {
			fmt.Println("No dogs in kennel")
			return nil
		}

		fmt.Println(style.Bold.Render("Dog Health Check"))
		fmt.Println()

		for _, r := range results {
			icon := "✓"
			if r.NeedsAttention {
				icon = "✗"
			}
			line := fmt.Sprintf("  %s %s [%s] session=%s", icon, r.Name, r.State, r.SessionStatus)
			if r.WorkDuration > 0 {
				line += fmt.Sprintf(" duration=%s", r.WorkDuration.Truncate(time.Second))
			}
			if r.AutoCleared {
				line += " (auto-cleared)"
			}
			fmt.Println(line)
			if r.Recommendation != "" && r.NeedsAttention {
				fmt.Printf("    → %s\n", r.Recommendation)
			}
		}

		fmt.Println()
		if attention > 0 {
			fmt.Printf("  %d dog(s) need attention\n", attention)
		} else {
			fmt.Println("  All dogs healthy")
		}
	}

	// Exit code 2 for needs-attention
	if attention > 0 {
		os.Exit(2)
	}

	return nil
}

// runDogDispatch dispatches plugin execution to a dog worker.
func runDogDispatch(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}

	// Get rig names for plugin scanner
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		return fmt.Errorf("loading rigs config: %w", err)
	}

	var rigNames []string
	for rigName := range rigsConfig.Rigs {
		rigNames = append(rigNames, rigName)
	}

	// If --rig specified, search only that rig
	if dogDispatchRig != "" {
		rigNames = []string{dogDispatchRig}
	}

	// Find the plugin using scanner
	scanner := plugin.NewScanner(townRoot, rigNames)
	p, err := scanner.GetPlugin(dogDispatchPlugin)
	if err != nil {
		return fmt.Errorf("finding plugin: %w", err)
	}

	// Get dog manager (reuse rigsConfig from above)
	mgr := dog.NewManager(townRoot, rigsConfig)

	// Find target dog
	var targetDog *dog.Dog
	var dogCreated bool
	if dogDispatchDog != "" {
		// Specific dog requested
		targetDog, err = mgr.Get(dogDispatchDog)
		if err != nil {
			return fmt.Errorf("getting dog %s: %w", dogDispatchDog, err)
		}
		if targetDog.State == dog.StateWorking {
			return fmt.Errorf("dog %s is already working", dogDispatchDog)
		}
	} else {
		// Find idle dog from pool
		targetDog, err = mgr.GetIdleDog()
		if err != nil {
			return fmt.Errorf("finding idle dog: %w", err)
		}

		if targetDog == nil {
			if dogDispatchCreate {
				// Create a new dog (reuse generateDogName from sling_dog.go)
				newName := generateDogName(mgr)
				if dogDispatchDryRun {
					targetDog = &dog.Dog{Name: newName, State: dog.StateIdle}
					dogCreated = true
				} else {
					targetDog, err = mgr.Add(newName)
					if err != nil {
						return fmt.Errorf("creating dog %s: %w", newName, err)
					}
					dogCreated = true

					// Create agent bead for the dog
					b := beads.New(townRoot)
					location := filepath.Join("deacon", "dogs", newName)
					if _, beadErr := b.CreateDogAgentBead(newName, location); beadErr != nil {
						// Non-fatal warning
						if !dogDispatchJSON {
							fmt.Printf("  Warning: could not create agent bead: %v\n", beadErr)
						}
					}
				}
			} else {
				return fmt.Errorf("no idle dogs available (use --create to add one)")
			}
		}
	}

	// Prepare dispatch result for JSON output
	workDesc := fmt.Sprintf("plugin:%s", p.Name)
	result := dogDispatchResult{
		Plugin:     p.Name,
		PluginPath: p.Path,
		Dog:        targetDog.Name,
		DogCreated: dogCreated,
		Work:       workDesc,
		DryRun:     dogDispatchDryRun,
	}
	if p.RigName != "" {
		result.PluginRig = p.RigName
	}

	// Dry-run mode: show what would happen and exit
	if dogDispatchDryRun {
		if dogDispatchJSON {
			return json.NewEncoder(os.Stdout).Encode(result)
		}
		fmt.Printf("Dry run - would dispatch:\n")
		fmt.Printf("  Plugin: %s\n", p.Name)
		if p.RigName != "" {
			fmt.Printf("  Location: %s/plugins/%s\n", p.RigName, p.Name)
		} else {
			fmt.Printf("  Location: plugins/%s (town-level)\n", p.Name)
		}
		fmt.Printf("  Dog: %s%s\n", targetDog.Name, ifStr(dogCreated, " (would create)", ""))
		fmt.Printf("  Work: %s\n", workDesc)
		return nil
	}

	// Ensure dog has an agent bead before sending mail.
	// Dogs created before agent beads were added, or whose bead creation
	// failed silently, won't have one. The mail router requires agent beads
	// to validate recipients.
	b := beads.New(townRoot)
	if existing, _ := b.FindDogAgentBead(targetDog.Name); existing == nil {
		location := filepath.Join("deacon", "dogs", targetDog.Name)
		if _, beadErr := b.CreateDogAgentBead(targetDog.Name, location); beadErr != nil {
			if !dogDispatchJSON {
				fmt.Printf("  Warning: could not create agent bead: %v\n", beadErr)
			}
		}
	}

	// Assign work FIRST (before sending mail) to prevent race condition
	// If this fails, we haven't sent any mail yet
	assignedState, err := mgr.AssignWorkIfIdle(targetDog.Name, workDesc)
	if err != nil {
		return fmt.Errorf("assigning work to dog: %w", err)
	}

	// Create and send mail message with plugin instructions
	dogAddress := fmt.Sprintf("deacon/dogs/%s", targetDog.Name)
	subject := fmt.Sprintf("Plugin: %s", p.Name)
	body := p.FormatMailBody()

	router := mail.NewRouterWithTownRoot(townRoot, townRoot)
	defer waitForMailNotifications(router)
	msg := &mail.Message{
		From:      "deacon/",
		To:        dogAddress,
		Subject:   subject,
		Body:      body,
		Timestamp: time.Now(),
		ThreadID:  dog.AssignmentThreadID(targetDog.Name, assignedState.Work, assignedState.WorkStartedAt),
	}

	if err := router.Send(msg); err != nil {
		// Rollback: clear work assignment since mail failed
		if _, clearErr := mgr.ClearWorkIfMatches(targetDog.Name, assignedState.Work, assignedState.WorkStartedAt); clearErr != nil {
			// Log rollback failure but return original error
			if !dogDispatchJSON {
				fmt.Printf("  Warning: rollback failed: %v\n", clearErr)
			}
		}
		return fmt.Errorf("sending plugin mail to dog: %w", err)
	}

	// Ensure dog session is running so it can read the mail.
	// Without this, dispatched work sits in mail with no session to read it.
	t := tmux.NewTmux()
	sessMgr := dog.NewSessionManager(t, townRoot, mgr)
	sessOpts := dog.SessionStartOptions{
		WorkDesc:          workDesc,
		AssignmentReceipt: assignedState.StartReceipt,
	}
	result.SessionStarted = true
	if _, sessErr := sessMgr.EnsureRunning(targetDog.Name, sessOpts); sessErr != nil {
		result.SessionStarted = false
		// Roll back the work assignment: without a running session the dog
		// cannot read its mail, leaving it stuck in StateWorking (zombie).
		// Clearing work returns it to idle so it can be re-dispatched.
		// See: github.com/steveyegge/gastown/issues/2748
		var (
			clearErr error
			cleared  bool
		)
		if errors.Is(sessErr, dog.ErrSessionStartCleanupIncomplete) {
			clearErr = errors.New("exact session cleanup is incomplete; preserving assignment for recovery")
		} else {
			cleared, clearErr = mgr.ClearWorkIfMatches(targetDog.Name, assignedState.Work, assignedState.WorkStartedAt)
			if clearErr == nil && !cleared {
				clearErr = errors.New("assignment changed before rollback; preserving current state")
			}
		}
		if clearErr != nil {
			warn := fmt.Sprintf("session start failed AND rollback failed for dog %s — dog stuck in StateWorking, run: gt dog health-check --auto-clear: %v", targetDog.Name, clearErr)
			result.Warnings = append(result.Warnings, warn)
			if !dogDispatchJSON {
				style.PrintWarning("%s", warn)
			}
		}
		disposition := "work preserved for exact recovery"
		if cleared {
			disposition = fmt.Sprintf("work rolled back; re-dispatch with: gt dog dispatch --plugin %s", p.Name)
		}
		warn := fmt.Sprintf("dog dispatch: session start failed for %s (%s): %v", targetDog.Name, disposition, sessErr)
		result.Warnings = append(result.Warnings, warn)
		if !dogDispatchJSON {
			style.PrintWarning("%s", warn)
		}
		if escErr := dogEscalateBestEffort(warn); escErr != nil {
			if !dogDispatchJSON {
				style.PrintWarning("escalation also failed (%v) — escalate manually: gt escalate --severity medium %q", escErr, warn)
			}
		}
	}

	// Verify the work state write is readable. A read-back failure here
	// indicates state corruption, not a timing race.
	// See: github.com/steveyegge/gastown/issues/2748
	result.WorkConfirmed = false
	if d, getErr := mgr.Get(targetDog.Name); getErr != nil {
		warn := fmt.Sprintf("dog dispatch: could not verify work assignment for %s: %v", targetDog.Name, getErr)
		result.Warnings = append(result.Warnings, warn)
		if !dogDispatchJSON {
			style.PrintWarning("%s", warn)
		}
		_ = dogEscalateBestEffort(warn)
	} else if d.Work != "" {
		result.WorkConfirmed = true
	} else {
		warn := fmt.Sprintf("dog dispatch: work assignment cleared for %s between dispatch and verify — re-dispatch required", targetDog.Name)
		result.Warnings = append(result.Warnings, warn)
		if !dogDispatchJSON {
			style.PrintWarning("%s", warn)
		}
		_ = dogEscalateBestEffort(warn)
	}

	// Success - output result
	if dogDispatchJSON {
		return json.NewEncoder(os.Stdout).Encode(result)
	}

	fmt.Printf("%s Found plugin: %s\n", style.Bold.Render("✓"), p.Name)
	if p.RigName != "" {
		fmt.Printf("  Location: %s/plugins/%s\n", p.RigName, p.Name)
	} else {
		fmt.Printf("  Location: plugins/%s (town-level)\n", p.Name)
	}
	if dogCreated {
		fmt.Printf("%s Created dog %s (pool was empty)\n", style.Bold.Render("✓"), targetDog.Name)
	}
	fmt.Printf("%s Dispatching to dog: %s\n", style.Bold.Render("🐕"), targetDog.Name)
	fmt.Printf("%s Plugin dispatched (non-blocking)\n", style.Bold.Render("✓"))
	fmt.Printf("  Dog: %s\n", targetDog.Name)
	fmt.Printf("  Work: %s\n", workDesc)

	return nil
}

// dogDispatchResult is the JSON output for gt dog dispatch.
type dogDispatchResult struct {
	Plugin         string   `json:"plugin"`
	PluginRig      string   `json:"plugin_rig,omitempty"`
	PluginPath     string   `json:"plugin_path"`
	Dog            string   `json:"dog"`
	DogCreated     bool     `json:"dog_created,omitempty"`
	Work           string   `json:"work"`
	DryRun         bool     `json:"dry_run,omitempty"`
	SessionStarted bool     `json:"session_started"`
	WorkConfirmed  bool     `json:"work_confirmed"`
	Warnings       []string `json:"warnings,omitempty"`
}

// dogEscalateBestEffort fires a MEDIUM escalation via gt escalate.
func dogEscalateBestEffort(msg string) error {
	cmd := exec.Command("gt", "escalate", "--severity", "medium", msg)
	return cmd.Run()
}

// ifStr returns ifTrue if cond is true, otherwise ifFalse.
func ifStr(cond bool, ifTrue, ifFalse string) string {
	if cond {
		return ifTrue
	}
	return ifFalse
}
