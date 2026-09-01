package witness

import (
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

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestDetectZombieLiveSessionPreservesRecoveryHold(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("isolated tmux fixture is Unix-only")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is unavailable")
	}

	oldRecovery := zombieRecoveryDispositionForPolecat
	oldRestart := restartPolecatSession
	zombieRecoveryDispositionForPolecat = func(string, string, string, *tmux.Tmux) (polecat.WorkstateDisposition, error) {
		return polecat.WorkstateDisposition{
			Verdict: polecat.WorkstateVerdictNeedsRecovery, NeedsRecovery: true,
			Blockers: []string{"dirty-worktree"},
		}, nil
	}
	restarts := 0
	restartPolecatSession = func(string, string, string) error {
		restarts++
		return nil
	}
	t.Cleanup(func() {
		zombieRecoveryDispositionForPolecat = oldRecovery
		restartPolecatSession = oldRestart
	})

	for _, tt := range []struct {
		name           string
		doneIntent     bool
		classification ZombieClassification
	}{
		{name: "stuck in done", doneIntent: true, classification: ZombieStuckInDone},
		{name: "agent dead in live session", classification: ZombieAgentDeadInSession},
	} {
		t.Run(tt.name, func(t *testing.T) {
			socket := fmt.Sprintf("gt-recovery-hold-%d-%d", os.Getpid(), time.Now().UnixNano())
			env := []string{"PATH=" + os.Getenv("PATH"), "TMUX_TMPDIR=/tmp"}
			tm := tmux.NewTmuxWithSocketAndEnv(socket, env)
			sessionName := "gastown-nux"
			start := exec.Command("tmux", "-L", socket, "new-session", "-d", "-s", sessionName,
				"-e", tmux.EnvSessionGeneration+"=fixture-generation-0001",
				"-e", tmux.EnvSessionCustody+"=fixture-custody-0001",
				"-e", tmux.EnvSessionPane+"=", "sleep 60")
			start.Env = env
			if output, err := start.CombinedOutput(); err != nil {
				t.Fatalf("create isolated session: %v: %s", err, output)
			}
			t.Cleanup(func() {
				_ = tm.KillServer()
				_ = os.Remove(filepath.Join("/tmp", fmt.Sprintf("tmux-%d", os.Getuid()), socket))
			})
			generation, err := tm.CaptureSessionGeneration(sessionName)
			if err != nil {
				t.Fatalf("capture isolated session: %v", err)
			}

			stamp := time.Now().Add(-2 * config.DefaultWitnessDoneIntentStuckTimeout)
			labels := []string{}
			var intent *DoneIntent
			if tt.doneIntent {
				labels = []string{fmt.Sprintf("done-intent:COMPLETED:%d", stamp.Unix())}
				intent = &DoneIntent{ExitType: "COMPLETED", Timestamp: stamp}
			}
			description := "role_type: polecat\nrig: gastown\nagent_state: working\nhook_bead: gt-work\ncleanup_status: has_uncommitted\nincarnation: generation-1"
			payload, err := json.Marshal([]map[string]any{{
				"agent_state": "working", "hook_bead": "gt-work", "labels": labels,
				"updated_at": time.Now().UTC().Format(time.RFC3339), "description": description,
			}})
			if err != nil {
				t.Fatal(err)
			}
			bd, _ := mockBd(func(args []string) (string, error) {
				if len(args) > 0 && args[0] == "show" {
					return string(payload), nil
				}
				return "[]", nil
			}, func([]string) error { return nil })
			snapshot := &agentBeadSnapshot{
				AgentState: "working", HookBead: "gt-work", Labels: labels,
				Fields: &beads.AgentFields{Incarnation: "generation-1", CleanupStatus: "has_uncommitted"},
			}

			result, found := detectZombieLiveSession(
				bd, t.TempDir(), t.TempDir(), "gastown", "nux", sessionName,
				tm, intent, &config.WitnessThresholds{}, snapshot,
			)
			if !found || result.Classification != tt.classification {
				t.Fatalf("result = %+v, found=%v", result, found)
			}
			if result.Action != "preserved-recovery-hold" || result.MutationPerformed {
				t.Fatalf("hold result = %+v", result)
			}
			if restarts != 0 {
				t.Fatalf("restart calls = %d, want 0", restarts)
			}
			current, err := tm.CaptureSessionGeneration(sessionName)
			if err != nil || !current.Equal(generation) {
				t.Fatalf("session generation changed: current=%+v err=%v", current, err)
			}
			if err := tm.KillSessionGeneration(generation); err != nil {
				t.Fatalf("clean exact fixture session: %v", err)
			}
			if alive, err := tm.HasSession(sessionName); err != nil || alive {
				t.Fatalf("fixture session leaked: alive=%v err=%v", alive, err)
			}
		})
	}
}

func TestGuardZombieMutation(t *testing.T) {
	originalSession := testZombieSessionGeneration("$1")
	replacementSession := testZombieSessionGeneration("$2")
	lookupErr := errors.New("lookup failed")

	tests := []struct {
		name                 string
		disposition          polecat.WorkstateDisposition
		recoveryErr          error
		expectedIncarnation  string
		currentIncarnation   string
		snapshotErr          error
		expectedSession      tmux.SessionGeneration
		expectedSessionAlive bool
		currentSession       tmux.SessionGeneration
		currentSessionAlive  bool
		sessionErr           error
		stillZombie          bool
		stillZombieErr       error
		mutationErr          error
		wantFound            bool
		wantAction           string
		wantError            string
		wantMutations        int
		wantPerformed        bool
	}{
		{
			name: "stuck done session preserves recovery hold",
			disposition: polecat.WorkstateDisposition{
				Verdict: "NEEDS_RECOVERY", NeedsRecovery: true,
				Blockers: []string{"dirty-worktree"},
			},
			expectedIncarnation: "generation-1", currentIncarnation: "generation-1",
			expectedSession: originalSession, expectedSessionAlive: true,
			currentSession: originalSession, currentSessionAlive: true,
			stillZombie: true, wantFound: true,
			wantAction: "preserved-recovery-hold", wantMutations: 0,
		},
		{
			name: "agent dead session preserves unsafe disposition",
			disposition: polecat.WorkstateDisposition{
				Verdict: "PENDING_MR", SafeToNuke: false,
				Blockers: []string{"active-mr-open"},
			},
			expectedIncarnation: "generation-1", currentIncarnation: "generation-1",
			expectedSession: originalSession, expectedSessionAlive: true,
			currentSession: originalSession, currentSessionAlive: true,
			stillZombie: true, wantFound: true,
			wantAction: "preserved-recovery-hold", wantMutations: 0,
		},
		{
			name:                "recovery lookup fails closed",
			recoveryErr:         lookupErr,
			expectedIncarnation: "generation-1", currentIncarnation: "generation-1",
			expectedSession: originalSession, expectedSessionAlive: true,
			currentSession: originalSession, currentSessionAlive: true,
			stillZombie: true, wantFound: true,
			wantAction: "preserved-recovery-check-error", wantError: "lookup failed",
		},
		{
			name:                "empty recovery verdict fails closed",
			disposition:         polecat.WorkstateDisposition{SafeToNuke: true},
			expectedIncarnation: "generation-1", currentIncarnation: "generation-1",
			expectedSession: originalSession, expectedSessionAlive: true,
			currentSession: originalSession, currentSessionAlive: true,
			stillZombie: true, wantFound: true,
			wantAction: "preserved-recovery-check-error", wantError: "empty recovery verdict",
		},
		{
			name:                "agent identity lookup fails closed",
			disposition:         safeZombieDisposition(),
			expectedIncarnation: "generation-1", snapshotErr: lookupErr,
			expectedSession: originalSession, expectedSessionAlive: true,
			currentSession: originalSession, currentSessionAlive: true,
			stillZombie: true, wantFound: true,
			wantAction: "preserved-custody-error", wantError: "lookup failed",
		},
		{
			name:            "missing initial incarnation fails closed",
			disposition:     safeZombieDisposition(),
			expectedSession: originalSession, expectedSessionAlive: true,
			currentSession: originalSession, currentSessionAlive: true,
			stillZombie: true, wantFound: true,
			wantAction: "preserved-custody-error", wantError: "missing initial incarnation",
		},
		{
			name:                "incarnation drift fails closed",
			disposition:         safeZombieDisposition(),
			expectedIncarnation: "generation-1", currentIncarnation: "generation-2",
			expectedSession: originalSession, expectedSessionAlive: true,
			currentSession: originalSession, currentSessionAlive: true,
			stillZombie: true, wantFound: true,
			wantAction: "preserved-custody-drift", wantError: "incarnation changed",
		},
		{
			name:                "session generation drift fails closed",
			disposition:         safeZombieDisposition(),
			expectedIncarnation: "generation-1", currentIncarnation: "generation-1",
			expectedSession: originalSession, expectedSessionAlive: true,
			currentSession: replacementSession, currentSessionAlive: true,
			stillZombie: true, wantFound: true,
			wantAction: "preserved-custody-drift", wantError: "session generation changed",
		},
		{
			name:                "session lookup fails closed",
			disposition:         safeZombieDisposition(),
			expectedIncarnation: "generation-1", currentIncarnation: "generation-1",
			expectedSession: originalSession, expectedSessionAlive: true,
			sessionErr:  lookupErr,
			stillZombie: true, wantFound: true,
			wantAction: "preserved-custody-error", wantError: "lookup failed",
		},
		{
			name:                "dead session replacement fails closed",
			disposition:         safeZombieDisposition(),
			expectedIncarnation: "generation-1", currentIncarnation: "generation-1",
			expectedSessionAlive: false,
			currentSession:       replacementSession, currentSessionAlive: true,
			stillZombie: true, wantFound: true,
			wantAction: "preserved-custody-drift", wantError: "session liveness changed",
		},
		{
			name:                "cleared classification does not mutate",
			disposition:         safeZombieDisposition(),
			expectedIncarnation: "generation-1", currentIncarnation: "generation-1",
			expectedSession: originalSession, expectedSessionAlive: true,
			currentSession: originalSession, currentSessionAlive: true,
			stillZombie: false, wantFound: false,
		},
		{
			name:                "classification lookup fails closed",
			disposition:         safeZombieDisposition(),
			expectedIncarnation: "generation-1", currentIncarnation: "generation-1",
			expectedSession: originalSession, expectedSessionAlive: true,
			currentSession: originalSession, currentSessionAlive: true,
			stillZombieErr: lookupErr, wantFound: true,
			wantAction: "preserved-custody-error", wantError: "lookup failed",
		},
		{
			name:                "unchanged safe custody mutates once",
			disposition:         safeZombieDisposition(),
			expectedIncarnation: "generation-1", currentIncarnation: "generation-1",
			expectedSession: originalSession, expectedSessionAlive: true,
			currentSession: originalSession, currentSessionAlive: true,
			stillZombie: true, wantFound: true, wantMutations: 1, wantPerformed: true,
		},
		{
			name:                "failed mutation is not reported as performed",
			disposition:         safeZombieDisposition(),
			expectedIncarnation: "generation-1", currentIncarnation: "generation-1",
			expectedSession: originalSession, expectedSessionAlive: true,
			currentSession: originalSession, currentSessionAlive: true,
			stillZombie: true, mutationErr: lookupErr,
			wantFound: true, wantError: "lookup failed", wantMutations: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutations := 0
			got, found := guardZombieMutation(
				tt.expectedIncarnation,
				tt.expectedSession,
				tt.expectedSessionAlive,
				ZombieResult{PolecatName: "nux", Action: "restart-zombie"},
				zombieMutationGuardOps{
					recovery: func() (polecat.WorkstateDisposition, error) {
						return tt.disposition, tt.recoveryErr
					},
					currentSnapshot: func() (*agentBeadSnapshot, error) {
						return &agentBeadSnapshot{Fields: &beads.AgentFields{Incarnation: tt.currentIncarnation}}, tt.snapshotErr
					},
					currentSession: func() (tmux.SessionGeneration, bool, error) {
						return tt.currentSession, tt.currentSessionAlive, tt.sessionErr
					},
					stillZombie: func(*agentBeadSnapshot) (bool, error) {
						return tt.stillZombie, tt.stillZombieErr
					},
					mutate: func(*ZombieResult, *agentBeadSnapshot) (bool, error) {
						mutations++
						return tt.mutationErr == nil, tt.mutationErr
					},
				},
			)
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v (result=%+v)", found, tt.wantFound, got)
			}
			if mutations != tt.wantMutations {
				t.Fatalf("mutations = %d, want %d", mutations, tt.wantMutations)
			}
			if !found {
				return
			}
			if tt.wantAction != "" && got.Action != tt.wantAction {
				t.Errorf("action = %q, want %q", got.Action, tt.wantAction)
			}
			if tt.wantError == "" && got.Error != nil {
				t.Errorf("unexpected error: %v", got.Error)
			}
			if tt.wantError != "" && (got.Error == nil || !strings.Contains(got.Error.Error(), tt.wantError)) {
				t.Errorf("error = %v, want substring %q", got.Error, tt.wantError)
			}
			if got.MutationPerformed != tt.wantPerformed {
				t.Errorf("mutation_performed = %v, want %v", got.MutationPerformed, tt.wantPerformed)
			}
			if tt.expectedIncarnation != "" && got.RecoveryVerdict != tt.disposition.Verdict {
				t.Errorf("recovery_verdict = %q, want %q", got.RecoveryVerdict, tt.disposition.Verdict)
			}
			if strings.Join(got.RecoveryBlockers, ",") != strings.Join(tt.disposition.Blockers, ",") && tt.expectedIncarnation != "" {
				t.Errorf("recovery_blockers = %v, want %v", got.RecoveryBlockers, tt.disposition.Blockers)
			}
		})
	}
}

func safeZombieDisposition() polecat.WorkstateDisposition {
	return polecat.WorkstateDisposition{Verdict: polecat.WorkstateVerdictSafeToNuke, SafeToNuke: true}
}

func testZombieSessionGeneration(id string) tmux.SessionGeneration {
	return tmux.SessionGeneration{
		Name: "gastown-nux", SessionID: id, PaneID: "%1", Nonce: "nonce",
		Custody: "custody", ServerPID: 42, ServerIdentity: "server-1",
		Transport: tmux.SessionTransport{Bound: true, SocketName: "default", SocketPath: "/tmp/gastown-test.sock"},
	}
}
