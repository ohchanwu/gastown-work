package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dog"
	"github.com/steveyegge/gastown/internal/tmux"
)

// =============================================================================
// Test Fixtures
// =============================================================================

// testDogManager creates a dog.Manager with a temporary town root for testing.
func testDogManager(t *testing.T) (*dog.Manager, string) {
	t.Helper()
	tmpDir := t.TempDir()

	rigsConfig := &config.RigsConfig{
		Version: 1,
		Rigs: map[string]config.RigEntry{
			"gastown": {GitURL: "git@github.com:test/gastown.git"},
			"beads":   {GitURL: "git@github.com:test/beads.git"},
		},
	}

	m := dog.NewManager(tmpDir, rigsConfig)
	return m, tmpDir
}

// setupTestDog creates a dog directory with a state file for testing.
func setupTestDog(t *testing.T, m *dog.Manager, townRoot, name string, state *dog.DogState) {
	t.Helper()

	dogPath := filepath.Join(townRoot, "deacon", "dogs", name)
	if err := os.MkdirAll(dogPath, 0755); err != nil {
		t.Fatalf("Failed to create dog dir: %v", err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal state: %v", err)
	}

	statePath := filepath.Join(dogPath, ".dog.json")
	if err := os.WriteFile(statePath, data, 0644); err != nil {
		t.Fatalf("Failed to write state file: %v", err)
	}
}

func configureDogCommandTown(t *testing.T, townRoot string, rigsConfig *config.RigsConfig) {
	t.Helper()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rigsConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GT_TOWN_ROOT", townRoot)
	t.Setenv("GT_ROOT", townRoot)
}

// =============================================================================
// Dog Name Detection from Path Tests
// =============================================================================

// TestDetectDogNameFromPath tests the path parsing logic used by runDogDone
// to auto-detect the dog name from the current working directory.
func TestDetectDogNameFromPath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		wantName string
		wantOK   bool
	}{
		{
			name:     "dog worktree root",
			path:     "/Users/user/gt/deacon/dogs/alpha",
			wantName: "alpha",
			wantOK:   true,
		},
		{
			name:     "dog rig worktree",
			path:     "/Users/user/gt/deacon/dogs/alpha/gastown",
			wantName: "alpha",
			wantOK:   true,
		},
		{
			name:     "deep path in dog worktree",
			path:     "/Users/user/gt/deacon/dogs/bravo/beads/internal/cmd",
			wantName: "bravo",
			wantOK:   true,
		},
		{
			name:     "hyphenated dog name",
			path:     "/Users/user/gt/deacon/dogs/my-dog/gastown",
			wantName: "my-dog",
			wantOK:   true,
		},
		{
			name:     "numeric dog name",
			path:     "/Users/user/gt/deacon/dogs/dog123/beads",
			wantName: "dog123",
			wantOK:   true,
		},
		{
			name:     "not a dog path - polecat",
			path:     "/Users/user/gt/gastown/polecats/fixer/internal",
			wantName: "",
			wantOK:   false,
		},
		{
			name:     "not a dog path - crew",
			path:     "/Users/user/gt/gastown/crew/george/internal",
			wantName: "",
			wantOK:   false,
		},
		{
			name:     "deacon but not dogs directory",
			path:     "/Users/user/gt/deacon/boot",
			wantName: "",
			wantOK:   false,
		},
		{
			name:     "dogs without deacon parent",
			path:     "/Users/user/gt/some/dogs/alpha",
			wantName: "",
			wantOK:   false,
		},
		{
			name:     "empty path",
			path:     "",
			wantName: "",
			wantOK:   false,
		},
		{
			name:     "root path",
			path:     "/",
			wantName: "",
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotOK := detectDogNameFromPath(tt.path)
			if gotName != tt.wantName {
				t.Errorf("detectDogNameFromPath(%q) name = %q, want %q", tt.path, gotName, tt.wantName)
			}
			if gotOK != tt.wantOK {
				t.Errorf("detectDogNameFromPath(%q) ok = %v, want %v", tt.path, gotOK, tt.wantOK)
			}
		})
	}
}

// detectDogNameFromPath extracts the dog name from a filesystem path.
// This mirrors the logic in runDogDone for testability.
// Returns the dog name and true if found, empty string and false otherwise.
func detectDogNameFromPath(path string) (string, bool) {
	if path == "" {
		return "", false
	}

	// Use the same split logic as runDogDone
	parts := splitPathComponents(path)

	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == "dogs" && i > 0 && parts[i-1] == "deacon" {
			return parts[i+1], true
		}
	}

	return "", false
}

// splitPath splits a path into its components.
func splitPath(path string) []string {
	// Clean and split the path
	path = filepath.Clean(path)
	var parts []string
	for {
		dir, file := filepath.Split(path)
		if file != "" {
			parts = append([]string{file}, parts...)
		}
		if dir == "" || dir == "/" || dir == path {
			break
		}
		path = filepath.Clean(dir)
	}
	return parts
}

// =============================================================================
// Dog Done Command Tests
// =============================================================================

// TestDogDone_AlreadyIdle verifies that dogDone handles the case where
// a dog is already idle gracefully.
func TestDogDone_AlreadyIdle(t *testing.T) {
	m, tmpDir := testDogManager(t)

	now := time.Now()
	state := &dog.DogState{
		Name:                 "alpha",
		State:                dog.StateIdle,
		Work:                 "",
		SessionAbsenceProven: true,
		LastActive:           now,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	setupTestDog(t, m, tmpDir, "alpha", state)

	// Get the dog and verify it's idle
	d, err := m.Get("alpha")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if d.State != dog.StateIdle {
		t.Errorf("State = %q, want %q", d.State, dog.StateIdle)
	}
	if d.Work != "" {
		t.Errorf("Work = %q, want empty", d.Work)
	}

	// ClearWork on already-idle dog should succeed without error
	if err := m.ClearWork("alpha"); err != nil {
		t.Fatalf("ClearWork() error = %v", err)
	}

	// Verify still idle
	d, _ = m.Get("alpha")
	if d.State != dog.StateIdle {
		t.Errorf("After ClearWork: State = %q, want %q", d.State, dog.StateIdle)
	}
}

// TestDogDone_WorkingToIdle verifies that dogDone transitions a working
// dog back to idle state.
func TestDogDone_WorkingToIdle(t *testing.T) {
	m, tmpDir := testDogManager(t)

	now := time.Now()
	state := &dog.DogState{
		Name:                 "alpha",
		State:                dog.StateWorking,
		Work:                 "hq-convoy-xyz",
		SessionAbsenceProven: true,
		LastActive:           now,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	setupTestDog(t, m, tmpDir, "alpha", state)

	// Verify dog is working
	d, err := m.Get("alpha")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if d.State != dog.StateWorking {
		t.Errorf("Initial State = %q, want %q", d.State, dog.StateWorking)
	}
	if d.Work != "hq-convoy-xyz" {
		t.Errorf("Initial Work = %q, want 'hq-convoy-xyz'", d.Work)
	}

	// Clear work
	if err := m.ClearWork("alpha"); err != nil {
		t.Fatalf("ClearWork() error = %v", err)
	}

	// Verify now idle with no work
	d, _ = m.Get("alpha")
	if d.State != dog.StateIdle {
		t.Errorf("After ClearWork: State = %q, want %q", d.State, dog.StateIdle)
	}
	if d.Work != "" {
		t.Errorf("After ClearWork: Work = %q, want empty", d.Work)
	}
}

// TestDogDone_NotFound verifies error handling for non-existent dog.
func TestDogDone_NotFound(t *testing.T) {
	m, _ := testDogManager(t)

	err := m.ClearWork("nonexistent")
	if err != dog.ErrDogNotFound {
		t.Errorf("ClearWork() error = %v, want ErrDogNotFound", err)
	}
}

type fakeDogSessionController struct {
	hasSession    bool
	hasSessionErr error
	captured      tmux.SessionGeneration
	captureErr    error
	captureFn     func(string) (tmux.SessionGeneration, error)
	killed        []tmux.SessionGeneration
	killFn        func(tmux.SessionGeneration) error
	processKilled []tmux.SessionGeneration
	processKillFn func(context.Context, tmux.SessionGeneration) error
	strongKilled  []tmux.SessionGeneration
	strongKillFn  func(context.Context, tmux.SessionGeneration) error
}

func (f *fakeDogSessionController) HasSession(string) (bool, error) {
	return f.hasSession, f.hasSessionErr
}

func (f *fakeDogSessionController) CaptureSessionGeneration(name string) (tmux.SessionGeneration, error) {
	if f.captureFn != nil {
		return f.captureFn(name)
	}
	return f.captured, f.captureErr
}

func (f *fakeDogSessionController) KillSessionGeneration(generation tmux.SessionGeneration) error {
	f.killed = append(f.killed, generation)
	if f.killFn != nil {
		return f.killFn(generation)
	}
	return nil
}

func (f *fakeDogSessionController) KillSessionGenerationWithProcessesPortableContext(ctx context.Context, generation tmux.SessionGeneration) error {
	f.processKilled = append(f.processKilled, generation)
	if f.processKillFn != nil {
		return f.processKillFn(ctx, generation)
	}
	return nil
}

func (f *fakeDogSessionController) KillSessionGenerationWithProcessesContext(ctx context.Context, generation tmux.SessionGeneration) error {
	f.strongKilled = append(f.strongKilled, generation)
	if f.strongKillFn != nil {
		return f.strongKillFn(ctx, generation)
	}
	return nil
}

func TestDogCloseoutBrokerWorkerUsesStrongCustodyWithoutDescendantFallback(t *testing.T) {
	t.Setenv(tmux.EnvSessionBrokerWorker, "1")
	generation := cmdTestDogGeneration("$strong", "nonce-strong")
	controller := &fakeDogSessionController{}
	if err := teardownDogSessionGeneration(controller, generation); err != nil {
		t.Fatal(err)
	}
	if len(controller.strongKilled) != 1 || !controller.strongKilled[0].Equal(generation) {
		t.Fatalf("strong cleanup = %+v", controller.strongKilled)
	}
	if len(controller.processKilled) != 0 {
		t.Fatalf("portable descendant fallback ran for broker worker: %+v", controller.processKilled)
	}
}

func TestDogDoneGenerationMatchingCloseout(t *testing.T) {
	mgr, townRoot := testDogManager(t)
	generation := cmdTestDogGeneration("$1", "nonce-old")
	started := time.Now().UTC().Round(0)
	setupTestDog(t, mgr, townRoot, "alpha", &dog.DogState{
		Name:              "alpha",
		State:             dog.StateWorking,
		Work:              "hq-work-old",
		WorkStartedAt:     started,
		LastActive:        started,
		CreatedAt:         started,
		UpdatedAt:         started,
		SessionGeneration: dog.SessionGenerationFromTmux(generation),
	})
	d, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	controller := &fakeDogSessionController{captured: generation}

	if err := completeDogCloseout(mgr, controller, d); err != nil {
		t.Fatalf("completeDogCloseout: %v", err)
	}
	if len(controller.processKilled) != 1 || !controller.processKilled[0].Equal(generation) {
		t.Fatalf("process-aware kills = %+v, want exact persisted generation", controller.processKilled)
	}
	got, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != dog.StateIdle || got.Work != "" || got.SessionGeneration != nil {
		t.Fatalf("completed dog = %+v, want idle with no work or generation", got)
	}
}

func TestDogDoneUnreconciledProcessCleanupPreservesAssignment(t *testing.T) {
	mgr, townRoot := testDogManager(t)
	generation := cmdTestDogGeneration("$process", "nonce-process")
	started := time.Now().UTC().Round(0)
	setupTestDog(t, mgr, townRoot, "alpha", &dog.DogState{
		Name: "alpha", State: dog.StateWorking, Work: "hq-work-process",
		WorkStartedAt: started, LastActive: started, CreatedAt: started, UpdatedAt: started,
		SessionGeneration: dog.SessionGenerationFromTmux(generation),
	})
	snapshot, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	controller := &fakeDogSessionController{
		captureErr: tmux.ErrSessionNotFound,
		processKillFn: func(context.Context, tmux.SessionGeneration) error {
			return errors.Join(tmux.ErrSessionCleanupUnreconciled, errors.New("detached descendant survived"))
		},
	}

	err = completeDogCloseout(mgr, controller, snapshot)
	if !errors.Is(err, tmux.ErrSessionCleanupUnreconciled) {
		t.Fatalf("completeDogCloseout() error = %v, want unreconciled cleanup", err)
	}
	stored, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != dog.StateWorking || stored.Work != snapshot.Work || stored.SessionGeneration == nil ||
		!stored.SessionGeneration.EqualTmux(generation) {
		t.Fatalf("unreconciled cleanup released assignment custody: %+v", stored)
	}
}

func TestRemoveDogExactRejectsLiveLegacySessionEvenWithForce(t *testing.T) {
	mgr, townRoot := testDogManager(t)
	now := time.Now().UTC().Round(0)
	setupTestDog(t, mgr, townRoot, "alpha", &dog.DogState{
		Name: "alpha", State: dog.StateWorking, Work: "task", WorkStartedAt: now,
		LastActive: now, CreatedAt: now, UpdatedAt: now,
	})
	snapshot, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	controller := &fakeDogSessionController{captured: cmdTestDogGeneration("$legacy", "nonce-legacy")}

	removed, err := removeDogExact(mgr, controller, snapshot, true)
	if removed || !errors.Is(err, dog.ErrSessionGenerationUnavailable) {
		t.Fatalf("forced legacy removal = %v, %v; want preserved recovery blocker", removed, err)
	}
	if _, err := mgr.Get("alpha"); err != nil {
		t.Fatalf("live legacy dog was removed: %v", err)
	}
}

func TestRemoveDogExactRejectsAmbientAbsenceForLegacySession(t *testing.T) {
	mgr, townRoot := testDogManager(t)
	now := time.Now().UTC().Round(0)
	setupTestDog(t, mgr, townRoot, "alpha", &dog.DogState{
		Name: "alpha", State: dog.StateIdle, LastActive: now, CreatedAt: now, UpdatedAt: now,
	})
	snapshot, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	controller := &fakeDogSessionController{captureErr: tmux.ErrSessionNotFound}

	removed, err := removeDogExact(mgr, controller, snapshot, true)
	if removed || !errors.Is(err, dog.ErrSessionGenerationUnavailable) {
		t.Fatalf("legacy removal under ambient absence = %v, %v; want preserved recovery blocker", removed, err)
	}
	if _, err := mgr.Get("alpha"); err != nil {
		t.Fatalf("ambient absence removed legacy dog: %v", err)
	}
}

func TestRemoveDogExactUsesDurableAbsenceProofWithoutAmbientLookup(t *testing.T) {
	mgr, townRoot := testDogManager(t)
	now := time.Now().UTC().Round(0)
	setupTestDog(t, mgr, townRoot, "alpha", &dog.DogState{
		Name: "alpha", State: dog.StateIdle, SessionAbsenceProven: true,
		LastActive: now, CreatedAt: now, UpdatedAt: now,
	})
	snapshot, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	controller := &fakeDogSessionController{captureErr: errors.New("ambient transport must not be consulted")}

	removed, err := removeDogExact(mgr, controller, snapshot, true)
	if err != nil || !removed {
		t.Fatalf("proven-absent removal = %v, %v; want true, nil", removed, err)
	}
	if _, err := mgr.Get("alpha"); !errors.Is(err, dog.ErrDogNotFound) {
		t.Fatalf("removed dog lookup = %v, want ErrDogNotFound", err)
	}
}

func TestRemoveDogExactPreservesGenerationOnUnreconciledProcessCleanup(t *testing.T) {
	mgr, townRoot := testDogManager(t)
	now := time.Now().UTC().Round(0)
	generation := cmdTestDogGeneration("$remove-process", "nonce-remove-process")
	setupTestDog(t, mgr, townRoot, "alpha", &dog.DogState{
		Name: "alpha", State: dog.StateIdle, LastActive: now, CreatedAt: now, UpdatedAt: now,
		SessionGeneration: dog.SessionGenerationFromTmux(generation),
	})
	snapshot, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	controller := &fakeDogSessionController{
		captured: generation,
		processKillFn: func(context.Context, tmux.SessionGeneration) error {
			return tmux.ErrSessionCleanupUnreconciled
		},
	}

	removed, err := removeDogExact(mgr, controller, snapshot, true)
	if removed || !errors.Is(err, tmux.ErrSessionCleanupUnreconciled) {
		t.Fatalf("unreconciled removal = %v, %v; want preserved blocker", removed, err)
	}
	stored, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if stored.SessionGeneration == nil || !stored.SessionGeneration.EqualTmux(generation) {
		t.Fatalf("unreconciled removal lost generation: %+v", stored)
	}
}

func TestDogCloseoutPluginMailFailurePreservesAssignment(t *testing.T) {
	mgr, townRoot := testDogManager(t)
	started := time.Now().UTC().Round(0)
	generation := cmdTestDogGeneration("$plugin-mail", "nonce-plugin-mail")
	setupTestDog(t, mgr, townRoot, "alpha", &dog.DogState{
		Name:              "alpha",
		State:             dog.StateWorking,
		Work:              "plugin:reaper",
		WorkStartedAt:     started,
		LastActive:        started,
		CreatedAt:         started,
		UpdatedAt:         started,
		SessionGeneration: dog.SessionGenerationFromTmux(generation),
	})
	snapshot, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	controller := &fakeDogSessionController{captureErr: tmux.ErrSessionNotFound}

	if err := completeDogCloseout(mgr, controller, snapshot); err == nil || !strings.Contains(err.Error(), "finalizing dog assignment") {
		t.Fatalf("completeDogCloseout() error = %v, want mandatory assignment finalizer failure", err)
	}
	got, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != dog.StateWorking || got.Work != snapshot.Work || !got.WorkStartedAt.Equal(snapshot.WorkStartedAt) {
		t.Fatalf("command closeout released plugin assignment: %+v", got)
	}
	if replacement, err := mgr.AssignWorkIfIdle("alpha", "plugin:replacement"); replacement != nil || !errors.Is(err, dog.ErrDogWorking) {
		t.Fatalf("replacement assignment = %+v, %v; want blocked", replacement, err)
	}
}

func TestDogDoneCallerFinalizerFailurePreservesCustodyAndRetries(t *testing.T) {
	mgr, townRoot := testDogManager(t)
	generation := cmdTestDogGeneration("$archive", "nonce-archive")
	started := time.Now().UTC().Round(0)
	setupTestDog(t, mgr, townRoot, "alpha", &dog.DogState{
		Name:              "alpha",
		State:             dog.StateWorking,
		Work:              "task:reaper",
		WorkStartedAt:     started,
		LastActive:        started,
		CreatedAt:         started,
		UpdatedAt:         started,
		SessionGeneration: dog.SessionGenerationFromTmux(generation),
	})
	snapshot, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	controller := &fakeDogSessionController{captureErr: tmux.ErrSessionNotFound}
	archiveErr := errors.New("archive failed")
	if err := completeDogCloseoutWithFinalize(mgr, controller, snapshot, func() error { return archiveErr }); !errors.Is(err, archiveErr) {
		t.Fatalf("closeout error = %v, want archive failure", err)
	}
	got, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != dog.StateWorking || got.Work != snapshot.Work || got.SessionGeneration == nil ||
		!got.SessionGeneration.EqualTmux(generation) {
		t.Fatalf("archive failure released custody: %+v", got)
	}
	if assigned, err := mgr.AssignWorkIfIdle("alpha", "replacement"); !errors.Is(err, dog.ErrDogWorking) || assigned != nil {
		t.Fatalf("replacement assignment = %+v, %v; want blocked", assigned, err)
	}
	if err := completeDogCloseoutWithFinalize(mgr, controller, snapshot, func() error { return nil }); err != nil {
		t.Fatalf("retry closeout: %v", err)
	}
	got, err = mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != dog.StateIdle || got.Work != "" || got.SessionGeneration != nil {
		t.Fatalf("successful retry did not release custody: %+v", got)
	}
}

func TestDogDoneAbsentSessionCannotClearNewlyPersistedGeneration(t *testing.T) {
	mgr, townRoot := testDogManager(t)
	started := time.Now().UTC().Round(0)
	setupTestDog(t, mgr, townRoot, "alpha", &dog.DogState{
		Name:                 "alpha",
		State:                dog.StateWorking,
		Work:                 "plugin:reaper",
		WorkStartedAt:        started,
		SessionAbsenceProven: true,
		LastActive:           started,
		CreatedAt:            started,
		UpdatedAt:            started,
	})
	snapshot, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	generation := cmdTestDogGeneration("$replacement", "nonce-replacement")
	persisted, persistErr := mgr.SetSessionGenerationIfAssignmentMatches(
		"alpha", snapshot.Work, snapshot.WorkStartedAt, nil, generation,
	)
	if persistErr != nil || !persisted {
		t.Fatalf("persist replacement generation = %v, %v", persisted, persistErr)
	}
	controller := &fakeDogSessionController{captureErr: errors.New("ambient transport must not be consulted")}

	err = completeDogCloseout(mgr, controller, snapshot)
	if !errors.Is(err, errDogCloseoutIncomplete) {
		t.Fatalf("closeout error = %v, want incomplete", err)
	}
	got, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != dog.StateWorking || got.Work != snapshot.Work || got.SessionGeneration == nil ||
		!got.SessionGeneration.EqualTmux(generation) {
		t.Fatalf("stale absent-session closeout mutated replacement generation: %+v", got)
	}
}

func TestDogDoneGenerationChangeDuringTeardownPreservesDurableCustody(t *testing.T) {
	mgr, townRoot := testDogManager(t)
	oldGeneration := cmdTestDogGeneration("$1", "nonce-old")
	started := time.Now().UTC().Round(0)
	setupTestDog(t, mgr, townRoot, "alpha", &dog.DogState{
		Name:              "alpha",
		State:             dog.StateWorking,
		Work:              "hq-work-old",
		WorkStartedAt:     started,
		LastActive:        started,
		CreatedAt:         started,
		UpdatedAt:         started,
		SessionGeneration: dog.SessionGenerationFromTmux(oldGeneration),
	})
	d, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	controller := &fakeDogSessionController{captured: oldGeneration}
	controller.processKillFn = func(_ context.Context, generation tmux.SessionGeneration) error {
		if !generation.Equal(oldGeneration) {
			t.Fatalf("kill targeted %+v, want old generation", generation)
		}
		return tmux.ErrSessionGenerationChanged
	}

	err = completeDogCloseout(mgr, controller, d)
	if !errors.Is(err, tmux.ErrSessionGenerationChanged) {
		t.Fatalf("completeDogCloseout error = %v, want generation-changed", err)
	}
	got, getErr := mgr.Get("alpha")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Work != "hq-work-old" || got.State != dog.StateWorking ||
		got.SessionGeneration == nil || !got.SessionGeneration.EqualTmux(oldGeneration) {
		t.Fatalf("failed teardown mutated durable custody: %+v", got)
	}
}

func TestDogDoneGenerationAbsentSessionCompletesAndRepeatsIdempotently(t *testing.T) {
	mgr, townRoot := testDogManager(t)
	generation := cmdTestDogGeneration("$1", "nonce-absent")
	started := time.Now().UTC().Round(0)
	setupTestDog(t, mgr, townRoot, "alpha", &dog.DogState{
		Name:              "alpha",
		State:             dog.StateWorking,
		Work:              "hq-work",
		WorkStartedAt:     started,
		LastActive:        started,
		CreatedAt:         started,
		UpdatedAt:         started,
		SessionGeneration: dog.SessionGenerationFromTmux(generation),
	})
	controller := &fakeDogSessionController{captureErr: tmux.ErrSessionNotFound}
	d, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := completeDogCloseout(mgr, controller, d); err != nil {
		t.Fatalf("absent closeout: %v", err)
	}
	d, err = mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := completeDogCloseout(mgr, controller, d); err != nil {
		t.Fatalf("repeated absent closeout: %v", err)
	}
	if len(controller.killed) != 0 {
		t.Fatalf("absent session received kills: %+v", controller.killed)
	}
}

func TestDogDoneGenerationLegacyLiveAndTmuxUnknownRefuseWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		controller *fakeDogSessionController
	}{
		{name: "legacy live", controller: &fakeDogSessionController{captured: cmdTestDogGeneration("$8", "nonce-legacy")}},
		{name: "tmux unknown", controller: &fakeDogSessionController{captureErr: errors.New("tmux unavailable")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr, townRoot := testDogManager(t)
			started := time.Now().UTC().Round(0)
			setupTestDog(t, mgr, townRoot, "alpha", &dog.DogState{
				Name:          "alpha",
				State:         dog.StateWorking,
				Work:          "hq-work",
				WorkStartedAt: started,
				LastActive:    started,
				CreatedAt:     started,
				UpdatedAt:     started,
			})
			before, err := mgr.Get("alpha")
			if err != nil {
				t.Fatal(err)
			}
			if err := completeDogCloseout(mgr, tc.controller, before); !errors.Is(err, errDogCloseoutIncomplete) {
				t.Fatalf("closeout error = %v, want incomplete", err)
			}
			after, err := mgr.Get("alpha")
			if err != nil {
				t.Fatal(err)
			}
			if after.State != before.State || after.Work != before.Work ||
				!after.WorkStartedAt.Equal(before.WorkStartedAt) || after.SessionGeneration != nil {
				t.Fatalf("refusal mutated dog state: before=%+v after=%+v", before, after)
			}
			if len(tc.controller.killed) != 0 {
				t.Fatalf("refusal killed session: %+v", tc.controller.killed)
			}
		})
	}
}

func TestClearDogSnapshotUsesFreshAbsenceProofWithoutAmbientLookup(t *testing.T) {
	mgr, townRoot := testDogManager(t)
	started := time.Now().UTC().Round(0)
	setupTestDog(t, mgr, townRoot, "alpha", &dog.DogState{
		Name: "alpha", State: dog.StateWorking, Work: "hq-work", WorkStartedAt: started,
		SessionAbsenceProven: true,
		LastActive:           started, CreatedAt: started, UpdatedAt: started,
	})
	snapshot, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	ambientErr := errors.New("ambient transport must not be consulted")
	controller := &fakeDogSessionController{
		hasSessionErr: ambientErr,
		captureErr:    ambientErr,
	}

	if err := clearDogSnapshot(mgr, controller, snapshot, false); err != nil {
		t.Fatalf("clear generation-free fresh assignment: %v", err)
	}
	current, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if current.State != dog.StateIdle || current.Work != "" || !current.SessionAbsenceProven {
		t.Fatalf("fresh absence closeout = %+v, want proven idle", current)
	}
}

func TestDogStatusSessionStateClassification(t *testing.T) {
	oldGeneration := cmdTestDogGeneration("$1", "nonce-old")
	newGeneration := cmdTestDogGeneration("$2", "nonce-new")
	tests := []struct {
		name       string
		persisted  *dog.SessionGeneration
		controller *fakeDogSessionController
		wantState  dogSessionState
		wantMatch  bool
		wantDiag   bool
		wantErr    bool
	}{
		{name: "absent legacy", controller: &fakeDogSessionController{captureErr: tmux.ErrSessionNotFound}, wantState: dogSessionAbsent},
		{name: "legacy live unknown", controller: &fakeDogSessionController{captured: oldGeneration}, wantState: dogSessionUnknown, wantDiag: true},
		{name: "exact running", persisted: dog.SessionGenerationFromTmux(oldGeneration), controller: &fakeDogSessionController{captured: oldGeneration}, wantState: dogSessionRunning, wantMatch: true},
		{name: "replacement stale", persisted: dog.SessionGenerationFromTmux(oldGeneration), controller: &fakeDogSessionController{captured: newGeneration}, wantState: dogSessionStale, wantDiag: true},
		{name: "tmux failure unknown", persisted: dog.SessionGenerationFromTmux(oldGeneration), controller: &fakeDogSessionController{captureErr: errors.New("private process detail")}, wantState: dogSessionUnknown, wantDiag: true, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := &dog.Dog{Name: "alpha", SessionGeneration: tc.persisted}
			got := inspectDogSession(d, tc.controller)
			if got.State != tc.wantState || got.GenerationMatch != tc.wantMatch {
				t.Fatalf("status = %+v, want state=%s match=%v", got, tc.wantState, tc.wantMatch)
			}
			if (got.Diagnostic != "") != tc.wantDiag {
				t.Fatalf("diagnostic = %q, want present=%v", got.Diagnostic, tc.wantDiag)
			}
			if (got.Err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want present=%v", got.Err, tc.wantErr)
			}
			if strings.Contains(got.Diagnostic, "private process detail") {
				t.Fatalf("diagnostic leaked tmux detail: %q", got.Diagnostic)
			}
		})
	}
}

func TestShowDogStatusFailsClosedOnSessionTransportError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX tmux stub")
	}
	mgr, townRoot := testDogManager(t)
	now := time.Now()
	setupTestDog(t, mgr, townRoot, "alpha", &dog.DogState{Name: "alpha", State: dog.StateWorking, LastActive: now, CreatedAt: now, UpdatedAt: now})
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte("#!/bin/sh\necho transport-failed >&2\nexit 2\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	oldJSON := dogStatusJSON
	dogStatusJSON = true
	t.Cleanup(func() { dogStatusJSON = oldJSON })

	var gotErr error
	output := captureStdout(t, func() { gotErr = showDogStatus(mgr, "alpha") })
	if gotErr == nil || !strings.Contains(gotErr.Error(), "checking dog session hq-dog-alpha") {
		t.Fatalf("showDogStatus error = %v, want session transport failure", gotErr)
	}
	if output != "" {
		t.Fatalf("showDogStatus output = %q before failed session check, want empty", output)
	}
}

func TestDogStatusOutputSeparatesWorkAndSessionState(t *testing.T) {
	d := &dog.Dog{Name: "alpha", State: dog.StateWorking, Work: "hq-work", Path: "/tmp/alpha"}
	status := dogSessionStatus{Name: "hq-dog-alpha", State: dogSessionStale, Diagnostic: "live session does not match persisted generation"}
	var human bytes.Buffer
	if err := writeDogStatus(&human, d, status, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Work State:", "working", "Session:", "stale"} {
		if !strings.Contains(human.String(), want) {
			t.Fatalf("human status missing %q in %q", want, human.String())
		}
	}

	var encoded bytes.Buffer
	if err := writeDogStatus(&encoded, d, status, true); err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded.Bytes(), &object); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	for _, key := range []string{"Name", "State", "Path", "Worktrees", "LastActive", "Work", "WorkStartedAt", "CreatedAt", "session_name", "session_state", "generation_match"} {
		if _, ok := object[key]; !ok {
			t.Errorf("JSON missing existing or additive field %q: %s", key, encoded.String())
		}
	}
	if _, ok := object["SessionGeneration"]; ok {
		t.Fatalf("JSON exposed raw process generation: %s", encoded.String())
	}
}

func TestDogDoneCLIFormsReachDogSpecificCloseout(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "implicit"
		if explicit {
			name = "explicit"
		}
		t.Run(name, func(t *testing.T) {
			townRoot := canonicalTestTempDir(t)
			if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
				t.Fatal(err)
			}
			rigsData, err := json.Marshal(&config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(townRoot, "mayor", "rigs.json"), rigsData, 0644); err != nil {
				t.Fatal(err)
			}
			dogName := "alpha"
			kennel := filepath.Join(townRoot, "deacon", "dogs", dogName)
			if err := os.MkdirAll(kennel, 0755); err != nil {
				t.Fatal(err)
			}
			socket := fmt.Sprintf("gt-dog-cli-%s-%d", name, os.Getpid())
			tm := tmux.NewTmuxWithSocket(socket)
			t.Cleanup(func() { _ = tm.KillServer() })
			if err := tm.NewSessionWithCommand("hq-dog-alpha", kennel, "sleep 300"); err != nil {
				t.Fatalf("create dog session: %v", err)
			}
			generation, err := tm.CaptureSessionGeneration("hq-dog-alpha")
			if err != nil {
				t.Fatalf("capture dog session: %v", err)
			}
			now := time.Now().UTC().Round(0)
			setupTestDog(t, dog.NewManager(townRoot, &config.RigsConfig{}), townRoot, dogName, &dog.DogState{
				Name:              dogName,
				State:             dog.StateWorking,
				Work:              "hq-cli-work",
				WorkStartedAt:     now,
				LastActive:        now,
				CreatedAt:         now,
				UpdatedAt:         now,
				SessionGeneration: dog.SessionGenerationFromTmux(generation),
			})

			args := []string{"dog", "done"}
			if explicit {
				args = append(args, dogName)
			}
			command := exec.Command(os.Args[0], args...)
			command.Dir = kennel
			command.Env = append(os.Environ(),
				"GT_TEST_CMD_EXECUTE_HELPER=1",
				"GT_TOWN_ROOT="+townRoot,
				"GT_ROOT="+townRoot,
				"GT_ROLE=dog",
				"BD_ACTOR=dog",
				"GT_TOWN_SOCKET="+socket,
				"TMUX=",
			)
			output, runErr := command.CombinedOutput()
			if runErr != nil {
				t.Fatalf("gt dog done failed: %v\n%s", runErr, output)
			}
			if strings.Contains(string(output), "gt done is for polecats only") {
				t.Fatalf("dog-specific command reached generic done route: %s", output)
			}
			stored, err := dog.NewManager(townRoot, &config.RigsConfig{}).Get(dogName)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Work != "" || stored.State != dog.StateIdle || stored.SessionGeneration != nil {
				t.Fatalf("CLI closeout state = %+v", stored)
			}
		})
	}
}

func TestDogDoneInsideOwnedTmuxSessionFinalizesOutsidePane(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux run-shell closeout test uses a POSIX command")
	}
	townRoot := canonicalTestTempDir(t)
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	rigsData, err := json.Marshal(&config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "rigs.json"), rigsData, 0644); err != nil {
		t.Fatal(err)
	}
	dogName := "alpha"
	kennel := filepath.Join(townRoot, "deacon", "dogs", dogName)
	if err := os.MkdirAll(kennel, 0755); err != nil {
		t.Fatal(err)
	}

	socket := fmt.Sprintf("gt-dog-cli-in-session-%d", os.Getpid())
	tm := tmux.NewTmuxWithSocket(socket)
	t.Cleanup(func() { _ = tm.KillServer() })
	barrier := filepath.Join(t.TempDir(), "start")
	command := fmt.Sprintf(
		"while [ ! -f %s ]; do sleep 0.02; done; exec %s dog done",
		shellQuote(barrier),
		shellQuote(os.Args[0]),
	)
	generation, err := tm.NewSessionWithCommandAndEnvGeneration(
		"hq-dog-alpha",
		kennel,
		command,
		map[string]string{
			"GT_TEST_CMD_EXECUTE_HELPER": "1",
			"GT_TOWN_ROOT":               townRoot,
			"GT_ROOT":                    townRoot,
			"GT_ROLE":                    "dog",
			"BD_ACTOR":                   "dog",
			"GT_TOWN_SOCKET":             socket,
		},
	)
	if err != nil {
		t.Fatalf("create in-session dog command: %v", err)
	}
	now := time.Now().UTC().Round(0)
	setupTestDog(t, dog.NewManager(townRoot, &config.RigsConfig{}), townRoot, dogName, &dog.DogState{
		Name:              dogName,
		State:             dog.StateWorking,
		Work:              "hq-cli-work",
		WorkStartedAt:     now,
		LastActive:        now,
		CreatedAt:         now,
		UpdatedAt:         now,
		SessionGeneration: dog.SessionGenerationFromTmux(generation),
	})
	if err := os.WriteFile(barrier, []byte("go\n"), 0600); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		stored, getErr := dog.NewManager(townRoot, &config.RigsConfig{}).Get(dogName)
		running, sessionErr := tm.HasSession("hq-dog-alpha")
		if getErr == nil && sessionErr == nil && !running &&
			stored.State == dog.StateIdle && stored.Work == "" && stored.SessionGeneration == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	stored, _ := dog.NewManager(townRoot, &config.RigsConfig{}).Get(dogName)
	running, _ := tm.HasSession("hq-dog-alpha")
	currentGeneration, generationErr := tm.CaptureSessionGeneration("hq-dog-alpha")
	currentPane, paneErr := tm.GetPaneID("hq-dog-alpha")
	t.Fatalf(
		"in-session closeout did not finish: running=%v state=%+v original=%+v current=%+v generation_err=%v pane=%q pane_err=%v",
		running, stored, generation, currentGeneration, generationErr, currentPane, paneErr,
	)
}

func TestScheduleDogCloseoutFinalizerUsesExactBrokerRequest(t *testing.T) {
	townRoot := canonicalTestTempDir(t)
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(townRoot)
	started := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	generation := cmdTestDogGeneration("$81", "broker-schedule")
	snapshot := &dog.Dog{
		Name: "alpha", State: dog.StateWorking, Work: "plugin:reaper",
		WorkStartedAt: started, SessionGeneration: dog.SessionGenerationFromTmux(generation),
	}

	original := runDogCloseoutBroker
	var request []string
	runDogCloseoutBroker = func(args []string) (bool, int, error) {
		request = append([]string(nil), args...)
		return true, 0, nil
	}
	t.Cleanup(func() { runDogCloseoutBroker = original })
	if err := scheduleDogCloseoutFinalizer(tmux.NewTmuxWithSocket("unused"), snapshot); err != nil {
		t.Fatal(err)
	}
	if len(request) != 5 || request[0] != "dog" || request[1] != "done" || request[2] != "alpha" || request[3] != "--finalizer" {
		t.Fatalf("broker request = %q", request)
	}
	decoded, err := dogCloseoutSnapshotFromEncoded(request[4])
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Name != snapshot.Name || decoded.Work != snapshot.Work || decoded.WorkStartedAt != snapshot.WorkStartedAt ||
		decoded.SessionGeneration == nil || !decoded.SessionGeneration.EqualTmux(generation) {
		t.Fatalf("broker snapshot = %+v, want %+v", decoded, snapshot)
	}
}

func TestDogCloseoutSnapshotFromEnvironmentRequiresHostBoundary(t *testing.T) {
	generation := cmdTestDogGeneration("$forged-finalizer", "nonce-forged-finalizer")
	snapshot := &dog.Dog{
		Name:              "alpha",
		SessionGeneration: dog.SessionGenerationFromTmux(generation),
	}
	encoded, _, err := dogCloseoutFinalizerRequest(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(dogCloseoutFinalizerEnv, encoded)
	t.Setenv(dogCloseoutHostSessionEnv, "hq-dog-finalizer-alpha-forged")
	t.Setenv(tmux.EnvSessionBrokerWorker, "")

	decoded, present, err := dogCloseoutSnapshotFromEnvironment()
	if !present {
		t.Fatal("environment finalizer was not detected")
	}
	if err == nil || !strings.Contains(err.Error(), "trusted host boundary") {
		t.Fatalf("dogCloseoutSnapshotFromEnvironment() error = %v", err)
	}
	if decoded != nil {
		t.Fatalf("unauthorized environment snapshot decoded as %+v", decoded)
	}
}

func TestRequestDogCloseoutBrokerDoesNotFallBackAfterHandledFailure(t *testing.T) {
	generation := cmdTestDogGeneration("$broker-failure", "nonce-broker-failure")
	snapshot := &dog.Dog{Name: "alpha", SessionGeneration: dog.SessionGenerationFromTmux(generation)}
	original := runDogCloseoutBroker
	t.Cleanup(func() { runDogCloseoutBroker = original })

	runDogCloseoutBroker = func([]string) (bool, int, error) { return false, 0, nil }
	if handled, err := requestDogCloseoutBroker(snapshot); handled || err != nil {
		t.Fatalf("absent broker = handled %v, err %v", handled, err)
	}
	runDogCloseoutBroker = func([]string) (bool, int, error) { return true, 19, nil }
	if handled, err := requestDogCloseoutBroker(snapshot); !handled || err == nil {
		t.Fatalf("failed broker = handled %v, err %v", handled, err)
	}
}

func TestDogSessionControllerFromSnapshotUsesPersistedTransport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native Windows tmux workflows are unsupported; WSL runs the Linux path")
	}
	targetSocket := fmt.Sprintf("gt-dog-bound-target-%d", time.Now().UnixNano())
	wrongSocket := fmt.Sprintf("gt-dog-bound-wrong-%d", time.Now().UnixNano())
	targetController := tmux.NewTmuxWithSocket(targetSocket)
	t.Cleanup(func() { _ = targetController.KillServer() })

	target, err := targetController.NewSessionWithCommandAndEnvGeneration(
		"hq-dog-alpha",
		t.TempDir(),
		"sleep 30",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GT_TOWN_SOCKET", wrongSocket)
	snapshot := &dog.Dog{Name: "alpha", SessionGeneration: dog.SessionGenerationFromTmux(target)}

	controller, err := dogSessionControllerFromSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	current, err := controller.CaptureSessionGeneration(target.Name)
	if err != nil {
		t.Fatalf("persisted transport did not reach target server: %v", err)
	}
	if !current.Equal(target) {
		t.Fatalf("persisted transport captured %+v, want %+v", current, target)
	}
}

func TestDogSessionControllerFromSnapshotRejectsUnboundTransport(t *testing.T) {
	generation := cmdTestDogGeneration("$legacy", "nonce-legacy")
	generation.Transport = tmux.SessionTransport{}
	snapshot := &dog.Dog{Name: "alpha", SessionGeneration: dog.SessionGenerationFromTmux(generation)}
	if _, err := dogSessionControllerFromSnapshot(snapshot); !errors.Is(err, tmux.ErrSessionTransportUnbound) {
		t.Fatalf("dogSessionControllerFromSnapshot() error = %v, want unbound transport", err)
	}
}

func TestClearDogSnapshotNonForceUsesPersistedEndpointAfterAmbientRootDrift(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native Windows tmux workflows are unsupported; WSL runs the Linux path")
	}
	firstRoot, err := os.MkdirTemp("/tmp", "gt-dog-clear-a-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(firstRoot) })
	secondRoot, err := os.MkdirTemp("/tmp", "gt-dog-clear-b-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(secondRoot) })
	socket := fmt.Sprintf("gt-dog-clear-bound-%d", time.Now().UnixNano())
	target := tmux.NewTmuxWithSocketAndEnv(socket, []string{
		"PATH=" + os.Getenv("PATH"), "TMUX_TMPDIR=" + firstRoot,
	})
	t.Cleanup(func() { _ = target.KillServer() })
	generation, err := target.NewSessionWithCommandAndEnvGeneration(
		"hq-dog-alpha", t.TempDir(), "sleep 30", nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	m, tmpDir := testDogManager(t)
	now := time.Now().UTC().Round(0)
	setupTestDog(t, m, tmpDir, "alpha", &dog.DogState{
		Name: "alpha", State: dog.StateWorking, Work: "task-bound", WorkStartedAt: now,
		LastActive: now, CreatedAt: now, UpdatedAt: now,
		SessionGeneration: dog.SessionGenerationFromTmux(generation),
	})
	snapshot, err := m.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TMPDIR", secondRoot)
	controller, err := dogSessionControllerFromSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}

	err = clearDogSnapshot(m, controller, snapshot, false)
	if err == nil || !strings.Contains(err.Error(), "has an active session") {
		t.Fatalf("clearDogSnapshot() error = %v, want exact live-session refusal", err)
	}
	current, err := m.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if current.State != dog.StateWorking || current.Work != "task-bound" || current.SessionGeneration == nil {
		t.Fatalf("non-force refusal changed custody: %+v", current)
	}
}

func TestClearDogSnapshotFailsClosedOnBoundControllerLookupError(t *testing.T) {
	m, tmpDir := testDogManager(t)
	now := time.Now().UTC().Round(0)
	generation := cmdTestDogGeneration("$clear", "nonce-clear")
	setupTestDog(t, m, tmpDir, "alpha", &dog.DogState{
		Name: "alpha", State: dog.StateWorking, Work: "task-clear", WorkStartedAt: now,
		LastActive: now, CreatedAt: now, UpdatedAt: now,
		SessionGeneration: dog.SessionGenerationFromTmux(generation),
	})
	d, err := m.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("exact transport lookup failed")
	controller := &fakeDogSessionController{hasSessionErr: wantErr}

	err = clearDogSnapshot(m, controller, d, false)
	if !errors.Is(err, wantErr) {
		t.Fatalf("clearDogSnapshot() error = %v, want exact lookup error", err)
	}
	if len(controller.processKilled)+len(controller.strongKilled)+len(controller.killed) != 0 {
		t.Fatalf("lookup failure performed teardown: %+v", controller)
	}
	current, err := m.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if current.State != dog.StateWorking || current.Work != "task-clear" || current.SessionGeneration == nil {
		t.Fatalf("lookup failure changed custody: %+v", current)
	}
}

func TestRunDogClearIdleLegacyDogPreservesLiveSessionWithoutAmbientLookup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native Windows tmux workflows are unsupported; WSL runs the Linux path")
	}
	mgr, townRoot := testDogManager(t)
	configureDogCommandTown(t, townRoot, &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}})
	now := time.Now().UTC().Round(0)
	setupTestDog(t, mgr, townRoot, "alpha", &dog.DogState{
		Name: "alpha", State: dog.StateIdle, LastActive: now,
		CreatedAt: now, UpdatedAt: now,
	})
	socketRoot, err := os.MkdirTemp("/tmp", "gt-dog-clear-idle-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketRoot) })
	socket := fmt.Sprintf("gt-dog-clear-idle-%d", time.Now().UnixNano())
	target := tmux.NewTmuxWithSocketAndEnv(socket, []string{
		"PATH=" + os.Getenv("PATH"), "TMUX_TMPDIR=" + socketRoot, "TMUX=",
	})
	t.Cleanup(func() { _ = target.KillServer() })
	if err := target.NewSessionWithCommand("hq-dog-alpha", t.TempDir(), "sleep 30"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GT_TOWN_SOCKET", socket)
	t.Setenv("TMUX_TMPDIR", socketRoot)
	t.Setenv("TMUX", "")
	oldForce := dogForce
	dogForce = false
	t.Cleanup(func() { dogForce = oldForce })

	err = runDogClear(nil, []string{"alpha"})
	if err == nil || !strings.Contains(err.Error(), "session absence is unproven") {
		t.Fatalf("runDogClear() error = %v, want durable absence-proof refusal", err)
	}
	if running, checkErr := target.HasSession("hq-dog-alpha"); checkErr != nil || !running {
		t.Fatalf("legacy session changed: running=%v err=%v", running, checkErr)
	}
}

func TestRunDogClearIdleLegacyDogFailsClosedOnLookupError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pinned tmux failure helper uses a POSIX script")
	}
	mgr, townRoot := testDogManager(t)
	configureDogCommandTown(t, townRoot, &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}})
	now := time.Now().UTC().Round(0)
	setupTestDog(t, mgr, townRoot, "alpha", &dog.DogState{
		Name: "alpha", State: dog.StateIdle, LastActive: now,
		CreatedAt: now, UpdatedAt: now,
	})
	helper := filepath.Join(t.TempDir(), "tmux-fail")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho exact transport lookup failed >&2\nexit 2\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GT_INTERNAL_PINNED_TMUX_BINARY", helper)
	t.Setenv("GT_TOWN_SOCKET", fmt.Sprintf("gt-dog-clear-error-%d", time.Now().UnixNano()))
	t.Setenv("TMUX", "")
	oldForce := dogForce
	dogForce = false
	t.Cleanup(func() { dogForce = oldForce })

	err := runDogClear(nil, []string{"alpha"})
	if err == nil || !strings.Contains(err.Error(), "session absence is unproven") {
		t.Fatalf("runDogClear() error = %v, want refusal before ambient lookup", err)
	}
}

func TestWaitForDogCloseoutHostHandoffRejectsEarlyFinalizerExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("transient host-session proof uses POSIX commands")
	}
	socket := fmt.Sprintf("gt-dog-host-handoff-%d", time.Now().UnixNano())
	tm := tmux.NewTmuxWithSocket(socket)
	t.Cleanup(func() { _ = tm.KillServer() })
	target, err := tm.NewSessionWithCommandAndEnvGeneration("hq-dog-alpha", t.TempDir(), "sleep 30", nil)
	if err != nil {
		t.Fatal(err)
	}
	host, err := tm.StartTransientSessionWithCommandAndEnv(
		"hq-dog-finalizer-alpha-early",
		t.TempDir(),
		"true",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &dog.Dog{
		Name: "alpha", State: dog.StateWorking, Work: "work",
		WorkStartedAt: time.Now().UTC(), SessionGeneration: dog.SessionGenerationFromTmux(target),
	}
	err = waitForDogCloseoutHostHandoff(tm, snapshot, host)
	if err == nil || !strings.Contains(err.Error(), "finalizer exited before exact target teardown") {
		t.Fatalf("waitForDogCloseoutHostHandoff() error = %v", err)
	}
	running, err := tm.HasSession(target.Name)
	if err != nil || !running {
		t.Fatalf("target session after failed handoff: running=%v err=%v", running, err)
	}
}

func cmdTestDogGeneration(sessionID, nonce string) tmux.SessionGeneration {
	return tmux.SessionGeneration{
		Name:           "hq-dog-alpha",
		SessionID:      sessionID,
		PaneID:         "%0",
		Nonce:          nonce,
		Custody:        "custody-alpha",
		ServerPID:      4242,
		ServerIdentity: "server-start-alpha",
		Transport: tmux.SessionTransport{
			Bound: true, SocketName: "fixture-socket", SocketPath: "/tmp/tmux-fixture/fixture-socket",
		},
	}
}

// =============================================================================
// Dog Clear Tests
// =============================================================================

// TestDogClear_WorkingToIdle verifies that dogClear transitions a working
// dog back to idle state.
func TestDogClear_WorkingToIdle(t *testing.T) {
	m, tmpDir := testDogManager(t)

	now := time.Now()
	state := &dog.DogState{
		Name:                 "alpha",
		State:                dog.StateWorking,
		Work:                 constants.MolConvoyFeed,
		SessionAbsenceProven: true,
		LastActive:           now,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	setupTestDog(t, m, tmpDir, "alpha", state)

	// Verify dog is working
	d, err := m.Get("alpha")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if d.State != dog.StateWorking {
		t.Errorf("Initial State = %q, want %q", d.State, dog.StateWorking)
	}

	// Clear the dog (simulates gt dog clear alpha)
	err = m.ClearWork("alpha")
	if err != nil {
		t.Fatalf("ClearWork() error = %v", err)
	}

	// Verify dog is now idle
	d, err = m.Get("alpha")
	if err != nil {
		t.Fatalf("Get() after clear error = %v", err)
	}
	if d.State != dog.StateIdle {
		t.Errorf("After ClearWork: State = %q, want %q", d.State, dog.StateIdle)
	}
	if d.Work != "" {
		t.Errorf("After ClearWork: Work = %q, want empty", d.Work)
	}
}

// TestDogClear_AlreadyIdle verifies that dogClear handles the case where
// a dog is already idle gracefully.
func TestDogClear_AlreadyIdle(t *testing.T) {
	m, tmpDir := testDogManager(t)

	now := time.Now()
	state := &dog.DogState{
		Name:                 "alpha",
		State:                dog.StateIdle,
		Work:                 "",
		SessionAbsenceProven: true,
		LastActive:           now,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	setupTestDog(t, m, tmpDir, "alpha", state)

	// Get the dog and verify it's idle
	d, err := m.Get("alpha")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if d.State != dog.StateIdle {
		t.Errorf("Initial State = %q, want %q", d.State, dog.StateIdle)
	}

	// ClearWork on an already idle dog should succeed (idempotent)
	err = m.ClearWork("alpha")
	if err != nil {
		t.Errorf("ClearWork() on idle dog error = %v, want nil", err)
	}

	// Verify dog is still idle
	d, err = m.Get("alpha")
	if err != nil {
		t.Fatalf("Get() after clear error = %v", err)
	}
	if d.State != dog.StateIdle {
		t.Errorf("After ClearWork: State = %q, want %q", d.State, dog.StateIdle)
	}
}

// TestDogClear_NotFound verifies error handling for non-existent dog.
func TestDogClear_NotFound(t *testing.T) {
	m, _ := testDogManager(t)

	err := m.ClearWork("nonexistent")
	if err != dog.ErrDogNotFound {
		t.Errorf("ClearWork() error = %v, want ErrDogNotFound", err)
	}
}

// =============================================================================
// Path Splitting Tests
// =============================================================================

func TestSplitPath(t *testing.T) {
	tests := []struct {
		path string
		want []string
	}{
		{
			path: "/Users/user/gt/deacon/dogs/alpha",
			want: []string{"Users", "user", "gt", "deacon", "dogs", "alpha"},
		},
		{
			path: "/a/b/c",
			want: []string{"a", "b", "c"},
		},
		{
			path: "relative/path",
			want: []string{"relative", "path"},
		},
		{
			path: "/",
			want: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := splitPath(tt.path)
			if len(got) != len(tt.want) {
				t.Errorf("splitPath(%q) = %v (len %d), want %v (len %d)",
					tt.path, got, len(got), tt.want, len(tt.want))
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("splitPath(%q)[%d] = %q, want %q",
						tt.path, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// =============================================================================
// Dog Format Time Ago Tests
// =============================================================================

func TestDogFormatTimeAgo(t *testing.T) {
	tests := []struct {
		name   string
		offset time.Duration
		want   string
	}{
		{"just now", 30 * time.Second, "just now"},
		{"1 minute ago", 1 * time.Minute, "1 minute ago"},
		{"5 minutes ago", 5 * time.Minute, "5 minutes ago"},
		{"1 hour ago", 1 * time.Hour, "1 hour ago"},
		{"3 hours ago", 3 * time.Hour, "3 hours ago"},
		{"1 day ago", 24 * time.Hour, "1 day ago"},
		{"5 days ago", 5 * 24 * time.Hour, "5 days ago"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testTime := time.Now().Add(-tt.offset)
			got := dogFormatTimeAgo(testTime)
			if got != tt.want {
				t.Errorf("dogFormatTimeAgo(%v ago) = %q, want %q", tt.offset, got, tt.want)
			}
		})
	}
}

func TestDogFormatTimeAgo_ZeroTime(t *testing.T) {
	got := dogFormatTimeAgo(time.Time{})
	if got != "(unknown)" {
		t.Errorf("dogFormatTimeAgo(zero) = %q, want '(unknown)'", got)
	}
}
