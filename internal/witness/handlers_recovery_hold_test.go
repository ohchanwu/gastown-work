package witness

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/tmux"
)

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
		wantFound            bool
		wantAction           string
		wantError            string
		wantMutations        int
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
			stillZombie: true, wantFound: true, wantMutations: 1,
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
						return true, nil
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
			if got.MutationPerformed != (tt.wantMutations > 0) {
				t.Errorf("mutation_performed = %v, want %v", got.MutationPerformed, tt.wantMutations > 0)
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
