package polecat

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux"
)

func newSessionCustodyTestManager(t *testing.T) (*SessionManager, *tmux.Tmux, string) {
	t.Helper()
	requireTmux(t)
	setupTestRegistryForSession(t)
	tm := tmux.NewTmuxWithSocket(fmt.Sprintf("gt-polecat-custody-%d", time.Now().UnixNano()))
	t.Cleanup(func() { _ = tm.KillServer() })
	m := NewSessionManager(tm, &rig.Rig{Name: "gastown", Path: t.TempDir()})
	return m, tm, m.SessionName("nitro")
}

func TestStopSessionCustodyPreservesSameNameReplacement(t *testing.T) {
	m, tm, sessionID := newSessionCustodyTestManager(t)
	if err := tm.NewSessionWithCommandAndEnv(sessionID, t.TempDir(), "sleep 60", nil); err != nil {
		t.Fatalf("create original session: %v", err)
	}
	custody, err := m.CaptureSessionCustody("nitro")
	if err != nil {
		t.Fatalf("capture original custody: %v", err)
	}
	if err := tm.KillSessionGeneration(custody.generation); err != nil {
		t.Fatalf("kill original generation: %v", err)
	}
	if err := tm.NewSessionWithCommandAndEnv(sessionID, t.TempDir(), "sleep 60", nil); err != nil {
		t.Fatalf("create replacement session: %v", err)
	}

	err = m.StopSessionCustody(custody)
	if !errors.Is(err, tmux.ErrSessionGenerationChanged) {
		t.Fatalf("stop error = %v, want ErrSessionGenerationChanged", err)
	}
	if running, checkErr := tm.HasSession(sessionID); checkErr != nil || !running {
		t.Fatalf("replacement running = %v, error = %v; want preserved", running, checkErr)
	}
}

func TestStopSessionCustodyStopsCapturedGeneration(t *testing.T) {
	m, tm, sessionID := newSessionCustodyTestManager(t)
	if err := tm.NewSessionWithCommandAndEnv(sessionID, t.TempDir(), "sleep 60", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	custody, err := m.CaptureSessionCustody("nitro")
	if err != nil {
		t.Fatalf("capture custody: %v", err)
	}
	if err := m.StopSessionCustody(custody); err != nil {
		t.Fatalf("stop exact custody: %v", err)
	}
	if running, checkErr := tm.HasSession(sessionID); checkErr != nil || running {
		t.Fatalf("captured session running = %v, error = %v; want terminal", running, checkErr)
	}
}

func TestStopSessionCustodyAcceptsProvenAbsence(t *testing.T) {
	m, _, _ := newSessionCustodyTestManager(t)
	custody, err := m.CaptureSessionCustody("nitro")
	if err != nil {
		t.Fatalf("capture absent custody: %v", err)
	}
	if err := m.StopSessionCustody(custody); err != nil {
		t.Fatalf("stop absent custody: %v", err)
	}
}

func TestStopSessionCustodyPreservesSessionCreatedAfterAbsentProof(t *testing.T) {
	m, tm, sessionID := newSessionCustodyTestManager(t)
	custody, err := m.CaptureSessionCustody("nitro")
	if err != nil {
		t.Fatalf("capture absent custody: %v", err)
	}
	if err := tm.NewSessionWithCommandAndEnv(sessionID, t.TempDir(), "sleep 60", nil); err != nil {
		t.Fatalf("create replacement session: %v", err)
	}

	err = m.StopSessionCustody(custody)
	if !errors.Is(err, tmux.ErrSessionGenerationChanged) {
		t.Fatalf("stop error = %v, want ErrSessionGenerationChanged", err)
	}
	if running, checkErr := tm.HasSession(sessionID); checkErr != nil || !running {
		t.Fatalf("replacement running = %v, error = %v; want preserved", running, checkErr)
	}
}

func TestStopSessionCustodyPollerFailureDoesNotKillSession(t *testing.T) {
	m, tm, sessionID := newSessionCustodyTestManager(t)
	if err := tm.NewSessionWithCommandAndEnv(sessionID, t.TempDir(), "sleep 60", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	m.capturePollerGeneration = func(string, string) (nudge.PollerGeneration, error) {
		return nudge.PollerGeneration{}, nil
	}
	stopErr := errors.New("poller ownership changed")
	m.stopPollerGeneration = func(context.Context, string, string, nudge.PollerGeneration) error { return stopErr }
	custody, err := m.CaptureSessionCustody("nitro")
	if err != nil {
		t.Fatalf("capture custody: %v", err)
	}

	err = m.StopSessionCustody(custody)
	if !errors.Is(err, stopErr) {
		t.Fatalf("stop error = %v, want %v", err, stopErr)
	}
	if running, checkErr := tm.HasSession(sessionID); checkErr != nil || !running {
		t.Fatalf("session running = %v, error = %v; want preserved", running, checkErr)
	}
}
