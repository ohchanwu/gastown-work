package nudge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/util"
)

func testPollerIdentity(session string) pollerIdentity {
	return pollerIdentity{StartTime: "test-start", Command: "gt nudge-poller " + session, Generation: "test-generation", Transport: "fixture\x00fixture"}
}

func TestStopPollerBeforeReplacementOrdering(t *testing.T) {
	stopErr := errors.New("stop failed")
	replacements := 0
	if err := StopPollerBeforeReplacement(func() error { return stopErr }, func() error { replacements++; return nil }); !errors.Is(err, stopErr) || replacements != 0 {
		t.Fatalf("failed stop err=%v replacements=%d", err, replacements)
	}
	if err := StopPollerBeforeReplacement(func() error { return nil }, func() error { replacements++; return nil }); err != nil || replacements != 1 {
		t.Fatalf("successful stop err=%v replacements=%d", err, replacements)
	}
}

func TestReplaceBeforeStoppingPollerGenerationContextReleasesLockOnDeadline(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	generation, err := CapturePollerGeneration(townRoot, session)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = ReplaceBeforeStoppingPollerGenerationContext(
		ctx,
		townRoot,
		session,
		generation,
		pollerReconcileTimeout,
		func(ctx context.Context) (PollerReplacementCommit, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("replacement error = %v, want deadline", err)
	}
	lock, err := lockPoller(townRoot, session)
	if err != nil {
		t.Fatalf("ownership lock remained wedged after deadline: %v", err)
	}
	_ = lock.Unlock()
}

func TestReplaceBeforeStoppingPollerGenerationContextChecksDeadlineAfterValidation(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	ctx, cancel := context.WithCancel(context.Background())
	replacementCalled := false
	err := replaceBeforeStoppingPollerGenerationContext(
		ctx,
		townRoot,
		session,
		PollerGeneration{},
		pollerReconcileTimeout,
		pollerTransitionOps{
			read: func(string) ([]byte, error) {
				cancel()
				return nil, os.ErrNotExist
			},
		},
		func(context.Context) (PollerReplacementCommit, error) {
			replacementCalled = true
			return nil, nil
		},
	)
	if !errors.Is(err, context.Canceled) || replacementCalled {
		t.Fatalf("replacement error=%v called=%v, want cancellation before replacement", err, replacementCalled)
	}
}

func TestStopPollerGenerationContextCancelsWhileOwnershipLockIsHeld(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-held-lock"
	lock, err := lockPoller(townRoot, session)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Unlock() }()

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	err = StopPollerGenerationContext(ctx, townRoot, session, PollerGeneration{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("held-lock stop error = %v, want deadline", err)
	}
}

func TestReplaceBeforeStoppingPollerGenerationContextHoldsLifecycleLockDuringReplacement(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	generation, err := CapturePollerGeneration(townRoot, session)
	if err != nil {
		t.Fatal(err)
	}
	competing := make(chan error, 1)
	replacementErr := errors.New("replacement preserved")
	err = ReplaceBeforeStoppingPollerGenerationContext(
		context.Background(),
		townRoot,
		session,
		generation,
		pollerReconcileTimeout,
		func(context.Context) (PollerReplacementCommit, error) {
			go func() {
				lock, lockErr := lockPoller(townRoot, session)
				if lockErr == nil {
					_ = lock.Unlock()
				}
				competing <- lockErr
			}()
			select {
			case lockErr := <-competing:
				t.Fatalf("competing lifecycle lock acquired during replacement: %v", lockErr)
			case <-time.After(100 * time.Millisecond):
			}
			return nil, replacementErr
		},
	)
	if !errors.Is(err, replacementErr) {
		t.Fatalf("replacement error = %v, want %v", err, replacementErr)
	}
	select {
	case lockErr := <-competing:
		if lockErr != nil {
			t.Fatalf("competing lifecycle lock failed after replacement: %v", lockErr)
		}
	case <-time.After(time.Second):
		t.Fatal("competing lifecycle lock did not acquire after replacement")
	}
}

func TestReplaceBeforeStoppingPollerGenerationPreservesLivePollerOnReplacementFailure(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	pidPath := pollerPidFile(townRoot, session)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	record := []byte(formatPollerRecord(123, testPollerIdentity(session), session))
	if err := os.WriteFile(pidPath, record, 0o600); err != nil {
		t.Fatal(err)
	}
	generation, err := CapturePollerGeneration(townRoot, session)
	if err != nil {
		t.Fatal(err)
	}
	stopCalled := false
	replacementErr := errors.New("replacement preserved")
	err = replaceBeforeStoppingPollerGenerationContext(
		context.Background(),
		townRoot,
		session,
		generation,
		pollerReconcileTimeout,
		pollerTransitionOps{
			read:       os.ReadFile,
			alive:      func(int) bool { return true },
			identity:   func(int) (pollerIdentity, error) { return testPollerIdentity(session), nil },
			stop:       func([]byte) error { stopCalled = true; return nil },
			wait:       func(context.Context, int, pollerRecord) error { return nil },
			quarantine: func(string, []byte) error { return nil },
			remove:     os.Remove,
		},
		func(context.Context) (PollerReplacementCommit, error) { return nil, replacementErr },
	)
	if !errors.Is(err, replacementErr) || stopCalled {
		t.Fatalf("replacement error=%v stopCalled=%v, want preserved live poller", err, stopCalled)
	}
	got, err := os.ReadFile(pidPath)
	if err != nil || !bytes.Equal(got, record) {
		t.Fatalf("poller record after replacement failure = %q, err %v", got, err)
	}
	lock, err := lockPoller(townRoot, session)
	if err != nil {
		t.Fatal(err)
	}
	_ = lock.Unlock()
}

func TestReplaceBeforeStoppingPollerGenerationPreservesPollerWhenCommitSignalFails(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	pidPath := pollerPidFile(townRoot, session)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	record := []byte(formatPollerRecord(123, testPollerIdentity(session), session))
	if err := os.WriteFile(pidPath, record, 0o600); err != nil {
		t.Fatal(err)
	}
	generation, err := CapturePollerGeneration(townRoot, session)
	if err != nil {
		t.Fatal(err)
	}
	stopCalled := false
	commitErr := errors.New("first destructive signal was not issued")
	err = replaceBeforeStoppingPollerGenerationContext(
		context.Background(), townRoot, session, generation, pollerReconcileTimeout,
		pollerTransitionOps{
			read:       os.ReadFile,
			alive:      func(int) bool { return true },
			identity:   func(int) (pollerIdentity, error) { return testPollerIdentity(session), nil },
			stop:       func([]byte) error { stopCalled = true; return nil },
			wait:       func(context.Context, int, pollerRecord) error { return nil },
			quarantine: func(string, []byte) error { return nil },
			remove:     os.Remove,
		},
		func(context.Context) (PollerReplacementCommit, error) {
			return func(context.Context) (bool, error) { return false, commitErr }, nil
		},
	)
	if !errors.Is(err, commitErr) || stopCalled {
		t.Fatalf("replacement error=%v stopCalled=%v, want pre-commit preservation", err, stopCalled)
	}
	if got, readErr := os.ReadFile(pidPath); readErr != nil || !bytes.Equal(got, record) {
		t.Fatalf("poller record after failed commit = %q, err %v", got, readErr)
	}
}

func TestReplaceBeforeStoppingPollerGenerationReconcilesAfterCommittedError(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	pidPath := pollerPidFile(townRoot, session)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	record := []byte(formatPollerRecord(123, testPollerIdentity(session), session))
	if err := os.WriteFile(pidPath, record, 0o600); err != nil {
		t.Fatal(err)
	}
	generation, err := CapturePollerGeneration(townRoot, session)
	if err != nil {
		t.Fatal(err)
	}
	commitErr := errors.New("post-commit session verification failed")
	stopCalled := false
	err = replaceBeforeStoppingPollerGenerationContext(
		context.Background(), townRoot, session, generation, pollerReconcileTimeout,
		pollerTransitionOps{
			read:       os.ReadFile,
			alive:      func(int) bool { return true },
			identity:   func(int) (pollerIdentity, error) { return testPollerIdentity(session), nil },
			stop:       func([]byte) error { stopCalled = true; return nil },
			wait:       func(context.Context, int, pollerRecord) error { return nil },
			quarantine: func(string, []byte) error { return nil },
			remove:     os.Remove,
		},
		func(context.Context) (PollerReplacementCommit, error) {
			return func(context.Context) (bool, error) { return true, commitErr }, nil
		},
	)
	if !errors.Is(err, commitErr) || !stopCalled {
		t.Fatalf("replacement error=%v stopCalled=%v, want mandatory post-commit reconciliation", err, stopCalled)
	}
	if _, statErr := os.Stat(pidPath); !os.IsNotExist(statErr) {
		t.Fatalf("poller record survived committed replacement error: %v", statErr)
	}
}

func TestReplaceBeforeStoppingPollerGenerationPreservesPollerAfterUnreconciledCommit(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	pidPath := pollerPidFile(townRoot, session)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	record := []byte(formatPollerRecord(123, testPollerIdentity(session), session))
	if err := os.WriteFile(pidPath, record, 0o600); err != nil {
		t.Fatal(err)
	}
	generation, err := CapturePollerGeneration(townRoot, session)
	if err != nil {
		t.Fatal(err)
	}
	commitErr := errors.New("committed tmux reconciliation remained ambiguous")
	stopCalled := false
	err = replaceBeforeStoppingPollerGenerationOutcomeContext(
		context.Background(), townRoot, session, generation,
		20*time.Millisecond, 20*time.Millisecond,
		pollerTransitionOps{
			read:       os.ReadFile,
			alive:      func(int) bool { return true },
			identity:   func(int) (pollerIdentity, error) { return testPollerIdentity(session), nil },
			stop:       func([]byte) error { stopCalled = true; return nil },
			wait:       func(context.Context, int, pollerRecord) error { return nil },
			quarantine: func(string, []byte) error { return nil },
			remove:     os.Remove,
		},
		func(context.Context) (PollerReplacementOutcomeCommit, error) {
			return func(context.Context) (PollerReplacementOutcome, error) {
				return PollerReplacementOutcome{Committed: true, Terminal: false}, commitErr
			}, nil
		},
	)
	if !errors.Is(err, commitErr) || !errors.Is(err, ErrPollerPreservedAfterCommittedCleanup) || stopCalled {
		t.Fatalf("replacement error=%v stopCalled=%v, want committed evidence with preserved poller", err, stopCalled)
	}
	if got, readErr := os.ReadFile(pidPath); readErr != nil || !bytes.Equal(got, record) {
		t.Fatalf("poller record after ambiguous committed cleanup = %q, err %v", got, readErr)
	}
}

func TestReplaceBeforeStoppingPollerGenerationRefreshesBudgetAfterCommit(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	pidPath := pollerPidFile(townRoot, session)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	record := []byte(formatPollerRecord(123, testPollerIdentity(session), session))
	if err := os.WriteFile(pidPath, record, 0o600); err != nil {
		t.Fatal(err)
	}
	generation, err := CapturePollerGeneration(townRoot, session)
	if err != nil {
		t.Fatal(err)
	}
	const budget = 20 * time.Millisecond
	stopCalled := false
	err = replaceBeforeStoppingPollerGenerationContext(
		context.Background(), townRoot, session, generation, budget,
		pollerTransitionOps{
			read:       os.ReadFile,
			alive:      func(int) bool { return true },
			identity:   func(int) (pollerIdentity, error) { return testPollerIdentity(session), nil },
			stop:       func([]byte) error { stopCalled = true; return nil },
			wait:       func(ctx context.Context, _ int, _ pollerRecord) error { return ctx.Err() },
			quarantine: func(string, []byte) error { return nil },
			remove:     os.Remove,
		},
		func(context.Context) (PollerReplacementCommit, error) {
			return func(ctx context.Context) (bool, error) {
				<-ctx.Done()
				return true, ctx.Err()
			}, nil
		},
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("replacement error = %v, want committed deadline evidence", err)
	}
	if !stopCalled {
		t.Fatal("poller reconciliation did not receive a fresh post-commit budget")
	}
	if _, statErr := os.Stat(pidPath); !os.IsNotExist(statErr) {
		t.Fatalf("poller record survived exhausted commit budget: %v", statErr)
	}
}

func TestPollerLifecycleLockHelperProcess(t *testing.T) {
	if os.Getenv("GT_TEST_POLLER_LOCK_HELPER") == "" {
		return
	}
	lockPath := pollerPidFile(os.Getenv("GT_TEST_POLLER_TOWN"), os.Getenv("GT_TEST_POLLER_SESSION")) + ".lock"
	probe := flock.New(lockPath)
	locked, err := probe.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	if locked {
		_ = probe.Unlock()
	}
	fmt.Printf("locked=%v", locked)
}

func TestReplaceBeforeStoppingPollerGenerationHoldsLifecycleLockCrossProcess(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	generation, err := CapturePollerGeneration(townRoot, session)
	if err != nil {
		t.Fatal(err)
	}
	probe := func() string {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPollerLifecycleLockHelperProcess$")
		cmd.Env = append(os.Environ(),
			"GT_TEST_POLLER_LOCK_HELPER=1",
			"GT_TEST_POLLER_TOWN="+townRoot,
			"GT_TEST_POLLER_SESSION="+session,
		)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("cross-process lifecycle lock probe: %v\n%s", err, output)
		}
		return string(output)
	}
	err = ReplaceBeforeStoppingPollerGenerationContext(
		context.Background(), townRoot, session, generation, pollerReconcileTimeout,
		func(context.Context) (PollerReplacementCommit, error) {
			if got := probe(); !strings.Contains(got, "locked=false") {
				t.Fatalf("competing process acquired lifecycle lock during preparation: %q", got)
			}
			return nil, errors.New("stop before commit")
		},
	)
	if err == nil {
		t.Fatal("replacement unexpectedly committed")
	}
	if got := probe(); !strings.Contains(got, "locked=true") {
		t.Fatalf("lifecycle lock remained held after transaction: %q", got)
	}
}

func TestReplaceBeforeStoppingPollerGenerationStopsOnlyAfterSuccess(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	pidPath := pollerPidFile(townRoot, session)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	record := []byte(formatPollerRecord(123, testPollerIdentity(session), session))
	if err := os.WriteFile(pidPath, record, 0o600); err != nil {
		t.Fatal(err)
	}
	generation, err := CapturePollerGeneration(townRoot, session)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	err = replaceBeforeStoppingPollerGenerationContext(
		context.Background(),
		townRoot,
		session,
		generation,
		pollerReconcileTimeout,
		pollerTransitionOps{
			read:       os.ReadFile,
			alive:      func(int) bool { return true },
			identity:   func(int) (pollerIdentity, error) { return testPollerIdentity(session), nil },
			stop:       func([]byte) error { events = append(events, "stop"); return nil },
			wait:       func(context.Context, int, pollerRecord) error { events = append(events, "wait"); return nil },
			quarantine: func(string, []byte) error { return nil },
			remove: func(path string) error {
				events = append(events, "remove")
				return os.Remove(path)
			},
		},
		func(context.Context) (PollerReplacementCommit, error) {
			return func(context.Context) (bool, error) {
				events = append(events, "replace")
				return true, nil
			}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"replace", "stop", "wait", "remove"}) {
		t.Fatalf("transition events = %v", events)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("poller record survived successful replacement: %v", err)
	}
}

func TestReplaceBeforeStoppingPollerGenerationReconcilesAfterReplacementCancelsContext(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	pidPath := pollerPidFile(townRoot, session)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	record := []byte(formatPollerRecord(123, testPollerIdentity(session), session))
	if err := os.WriteFile(pidPath, record, 0o600); err != nil {
		t.Fatal(err)
	}
	generation, err := CapturePollerGeneration(townRoot, session)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var events []string
	err = replaceBeforeStoppingPollerGenerationContext(
		ctx,
		townRoot,
		session,
		generation,
		pollerReconcileTimeout,
		pollerTransitionOps{
			read:     os.ReadFile,
			alive:    func(int) bool { return true },
			identity: func(int) (pollerIdentity, error) { return testPollerIdentity(session), nil },
			stop:     func([]byte) error { events = append(events, "stop"); return nil },
			wait: func(waitCtx context.Context, _ int, _ pollerRecord) error {
				if err := waitCtx.Err(); err != nil {
					return fmt.Errorf("post-commit context already expired: %w", err)
				}
				events = append(events, "wait")
				return nil
			},
			quarantine: func(string, []byte) error { return nil },
			remove: func(path string) error {
				events = append(events, "remove")
				return os.Remove(path)
			},
		},
		func(context.Context) (PollerReplacementCommit, error) {
			return func(context.Context) (bool, error) {
				events = append(events, "replace")
				cancel()
				return true, nil
			}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"replace", "stop", "wait", "remove"}) {
		t.Fatalf("transition events = %v", events)
	}
}

func TestStartPollerSerializesConcurrentLaunches(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	release := make(chan struct{})
	var launches atomic.Int32

	launcher := func(string, string, []string) (pollerLaunch, error) {
		switch launches.Add(1) {
		case 1:
			close(firstStarted)
		case 2:
			close(secondStarted)
		}
		<-release
		return pollerLaunch{pid: os.Getpid(), identity: testPollerIdentity(session)}, nil
	}

	results := make(chan int, 2)
	errs := make(chan error, 2)
	start := func() {
		pid, err := startPollerWithLauncherStatus(townRoot, session, nil, launcher, os.WriteFile, func(root, name string) (int, bool, error) {
			data, err := os.ReadFile(pollerPidFile(root, name))
			if os.IsNotExist(err) {
				return 0, false, nil
			}
			record, parseErr := parsePollerRecord(string(data))
			return record.PID, parseErr == nil, parseErr
		})
		results <- pid
		errs <- err
	}
	go start()
	<-firstStarted
	secondCalling := make(chan struct{})
	go func() {
		close(secondCalling)
		start()
	}()
	<-secondCalling

	duplicate := false
	select {
	case <-secondStarted:
		duplicate = true
	case <-time.After(3 * time.Second):
	}
	close(release)

	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if pid := <-results; pid != os.Getpid() {
			t.Fatalf("pid = %d, want %d", pid, os.Getpid())
		}
	}
	if duplicate || launches.Load() != 1 {
		t.Fatalf("launcher called %d times, want 1 serialized start", launches.Load())
	}
}

func TestNormalizePollerTransportAliases(t *testing.T) {
	if normalizePollerTransport([]string{"GT_TOWN_SOCKET=a", "GT_TMUX_SOCKET=a"}) != "a\x00a" {
		t.Fatal("alias normalization mismatch")
	}
	if normalizePollerTransport([]string{"GT_TOWN_SOCKET=a"}) != "a\x00a" {
		t.Fatal("single town alias mismatch")
	}
	if normalizePollerTransport([]string{"GT_TMUX_SOCKET=a"}) != "a\x00a" {
		t.Fatal("single tmux alias mismatch")
	}
}

func TestStartPollerUsesDefaultTmuxTransport(t *testing.T) {
	originalSocket := tmux.GetDefaultSocket()
	tmux.SetDefaultSocket("test-poller-socket")
	t.Cleanup(func() { tmux.SetDefaultSocket(originalSocket) })
	t.Setenv("GT_TOWN_SOCKET", "")
	t.Setenv("GT_TMUX_SOCKET", "")

	root, session := t.TempDir(), "s"
	var launchedEnv []string
	pid, err := startPollerWithLauncherStatus(root, session, nil, func(_ string, _ string, env []string) (pollerLaunch, error) {
		launchedEnv = append([]string(nil), env...)
		return pollerLaunch{pid: 7, identity: testPollerIdentity(session)}, nil
	}, os.WriteFile, func(string, string) (int, bool, error) {
		return 0, false, nil
	})
	if err != nil || pid != 7 {
		t.Fatalf("StartPoller = pid %d, err %v", pid, err)
	}
	want := "test-poller-socket\x00test-poller-socket"
	if got := normalizePollerTransport(launchedEnv); got != want {
		t.Fatalf("launched transport = %q, want %q", got, want)
	}
	data, err := os.ReadFile(pollerPidFile(root, session))
	if err != nil {
		t.Fatal(err)
	}
	record, err := parsePollerRecord(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if record.Identity.Transport != want {
		t.Fatalf("record transport = %q, want %q", record.Identity.Transport, want)
	}
}

func TestStartPollerDifferentTransportFailsClosedWithoutLaunch(t *testing.T) {
	root, session := t.TempDir(), "s"
	identity := testPollerIdentity(session)
	identity.Transport = "old\x00old"
	_ = os.MkdirAll(pollerPidDir(root), 0755)
	_ = os.WriteFile(pollerPidFile(root, session), []byte(formatPollerRecord(1, identity, session)), 0644)
	launched := false
	_, err := startPollerWithLauncherStatus(root, session, []string{"GT_TOWN_SOCKET=new"}, func(string, string, []string) (pollerLaunch, error) { launched = true; return pollerLaunch{}, nil }, os.WriteFile, func(string, string) (int, bool, error) { return 1, true, nil })
	if err == nil || launched {
		t.Fatalf("transport mismatch err=%v launched=%v", err, launched)
	}
}

func TestStartPollerLiveLegacyFailsClosedForTransportCustody(t *testing.T) {
	root, session := t.TempDir(), "s"
	_ = os.MkdirAll(pollerPidDir(root), 0755)
	_ = os.WriteFile(pollerPidFile(root, session), []byte("7"), 0644)
	launched := false
	_, err := startPollerWithLauncherStatus(root, session, []string{"GT_TOWN_SOCKET=a"}, func(string, string, []string) (pollerLaunch, error) { launched = true; return pollerLaunch{}, nil }, os.WriteFile, func(string, string) (int, bool, error) { return 7, true, nil })
	if err == nil || launched {
		t.Fatalf("legacy transport err=%v launched=%v", err, launched)
	}
}

func TestStartPollerDeadPreTransportRecordRecoversAndLaunches(t *testing.T) {
	root, session := t.TempDir(), "s"
	identity := testPollerIdentity(session)
	identity.Transport = ""
	if err := os.MkdirAll(pollerPidDir(root), 0755); err != nil {
		t.Fatal(err)
	}
	pidPath := pollerPidFile(root, session)
	if err := os.WriteFile(pidPath, []byte(formatPollerRecord(7, identity, session)), 0644); err != nil {
		t.Fatal(err)
	}
	launched := 0
	pid, err := startPollerWithLauncherStatus(root, session, []string{"GT_TOWN_SOCKET=new"}, func(string, string, []string) (pollerLaunch, error) {
		launched++
		return pollerLaunch{pid: 9, identity: testPollerIdentity(session)}, nil
	}, os.WriteFile, func(root, name string) (int, bool, error) {
		return pollerStatusWithOps(root, name, os.ReadFile, func(int) bool { return false }, func(int) (pollerIdentity, error) {
			t.Fatal("dead record requested process identity")
			return pollerIdentity{}, nil
		}, func(string, []byte) error {
			t.Fatal("dead record was quarantined")
			return nil
		}, os.Remove)
	})
	if err != nil || pid != 9 || launched != 1 {
		t.Fatalf("dead pre-transport recovery = pid %d err %v launches %d", pid, err, launched)
	}
}

func TestStartPollerDeadMismatchedTransportRecoversAndLaunches(t *testing.T) {
	root, session := t.TempDir(), "s"
	identity := testPollerIdentity(session)
	identity.Transport = "old\x00old"
	if err := os.MkdirAll(pollerPidDir(root), 0755); err != nil {
		t.Fatal(err)
	}
	pidPath := pollerPidFile(root, session)
	if err := os.WriteFile(pidPath, []byte(formatPollerRecord(7, identity, session)), 0644); err != nil {
		t.Fatal(err)
	}
	launched := 0
	pid, err := startPollerWithLauncherStatus(root, session, []string{"GT_TOWN_SOCKET=new"}, func(string, string, []string) (pollerLaunch, error) {
		launched++
		return pollerLaunch{pid: 9, identity: testPollerIdentity(session)}, nil
	}, os.WriteFile, func(root, name string) (int, bool, error) {
		return pollerStatusWithOps(root, name, os.ReadFile, func(int) bool { return false }, func(int) (pollerIdentity, error) {
			t.Fatal("dead record requested process identity")
			return pollerIdentity{}, nil
		}, func(string, []byte) error {
			t.Fatal("dead record was quarantined")
			return nil
		}, os.Remove)
	})
	if err != nil || pid != 9 || launched != 1 {
		t.Fatalf("dead mismatched recovery = pid %d err %v launches %d", pid, err, launched)
	}
}

func TestStartPollerSameTransportReusesLivePID(t *testing.T) {
	root, session := t.TempDir(), "s"
	identity := testPollerIdentity(session)
	identity.Transport = "a\x00a"
	_ = os.MkdirAll(pollerPidDir(root), 0755)
	_ = os.WriteFile(pollerPidFile(root, session), []byte(formatPollerRecord(7, identity, session)), 0644)
	launched := false
	pid, err := startPollerWithLauncherStatus(root, session, []string{"GT_TOWN_SOCKET=a"}, func(string, string, []string) (pollerLaunch, error) { launched = true; return pollerLaunch{}, nil }, os.WriteFile, func(string, string) (int, bool, error) { return 7, true, nil })
	if err != nil || pid != 7 || launched {
		t.Fatalf("reuse err=%v pid=%d launched=%v", err, pid, launched)
	}
}

func TestStartPollerMissingTransportFailsClosed(t *testing.T) {
	root, session := t.TempDir(), "s"
	identity := testPollerIdentity(session)
	identity.Transport = ""
	_ = os.MkdirAll(pollerPidDir(root), 0755)
	_ = os.WriteFile(pollerPidFile(root, session), []byte(`{"PID":7,"Identity":{"StartTime":"test-start","Command":"gt nudge-poller s","Generation":"g"},"Session":"s"}`), 0644)
	launched := false
	_, err := startPollerWithLauncherStatus(root, session, []string{"GT_TOWN_SOCKET=a"}, func(string, string, []string) (pollerLaunch, error) { launched = true; return pollerLaunch{}, nil }, os.WriteFile, func(string, string) (int, bool, error) { return 7, true, nil })
	if err == nil || launched {
		t.Fatalf("missing transport err=%v launched=%v", err, launched)
	}
}

func TestStartPollerNilEnvUsesInheritedTransport(t *testing.T) {
	if normalizePollerTransport(effectivePollerEnv(nil)) != normalizePollerTransport(os.Environ()) {
		t.Fatal("nil env did not inherit")
	}
}

func TestStartPollerPIDWriteFailureTerminatesLaunchedProcess(t *testing.T) {
	townRoot := t.TempDir()
	writeErr := errors.New("injected PID write failure")
	var terminated, released atomic.Int32

	launcher := func(string, string, []string) (pollerLaunch, error) {
		return pollerLaunch{
			pid:       123,
			identity:  testPollerIdentity("gt-gastown-polecat-test"),
			release:   func() error { released.Add(1); return nil },
			terminate: func() error { terminated.Add(1); return nil },
		}, nil
	}
	writer := func(string, []byte, os.FileMode) error { return writeErr }

	pid, err := startPollerWithLauncher(townRoot, "gt-gastown-polecat-test", nil, launcher, writer)
	if !errors.Is(err, writeErr) {
		t.Fatalf("error = %v, want injected PID write failure", err)
	}
	if pid != 0 {
		t.Fatalf("pid = %d, want 0 on failed persistence", pid)
	}
	if got := terminated.Load(); got != 1 {
		t.Fatalf("terminate calls = %d, want 1", got)
	}
	if got := released.Load(); got != 0 {
		t.Fatalf("release calls = %d, want 0", got)
	}
}

func TestStartPollerEmptyGenerationTerminatesWithoutWriting(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	var terminated, wrote atomic.Bool

	pid, err := startPollerWithLauncher(townRoot, session, nil,
		func(string, string, []string) (pollerLaunch, error) {
			return pollerLaunch{
				pid: 123,
				identity: pollerIdentity{
					StartTime: "test-start",
					Command:   "gt nudge-poller " + session,
				},
				terminate: func() error { terminated.Store(true); return nil },
			}, nil
		},
		func(string, []byte, os.FileMode) error { wrote.Store(true); return nil },
	)
	if err == nil || pid != 0 || !terminated.Load() || wrote.Load() {
		t.Fatalf("empty generation start = pid %d err %v terminated %v wrote %v", pid, err, terminated.Load(), wrote.Load())
	}
}

func TestStopPollerRequestFailurePreservesCustody(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	pidPath := pollerPidFile(townRoot, session)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0755); err != nil {
		t.Fatal(err)
	}
	record := []byte(formatPollerRecord(123, testPollerIdentity(session), session))
	if err := os.WriteFile(pidPath, record, 0644); err != nil {
		t.Fatal(err)
	}
	requestErr := errors.New("injected stop request failure")
	removed := false

	err := stopPollerWithGenerationOps(townRoot, session,
		os.ReadFile,
		func(int) bool { return true },
		func(int) (pollerIdentity, error) { return testPollerIdentity(session), nil },
		func([]byte) error { return requestErr },
		func(int, pollerRecord) error { t.Fatal("failed stop request waited for exit"); return nil },
		func(string, []byte) error { t.Fatal("matching ownership quarantined"); return nil },
		func(string) error { removed = true; return nil },
	)
	if !errors.Is(err, requestErr) {
		t.Fatalf("error = %v, want stop request failure", err)
	}
	if removed {
		t.Fatal("StopPoller removed custody after stop request failure")
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("ownership stat = %v, want custody preserved", err)
	}
}

func TestStopPollerSerializesStartCustodyTransition(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	stopStarted := make(chan struct{})
	releaseStop := make(chan struct{})
	launchStarted := make(chan struct{})
	if err := os.MkdirAll(pollerPidDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	record := []byte(formatPollerRecord(123, testPollerIdentity(session), session))
	if err := os.WriteFile(pollerPidFile(townRoot, session), record, 0644); err != nil {
		t.Fatal(err)
	}

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- stopPollerWithGenerationOps(townRoot, session,
			os.ReadFile,
			func(int) bool { return true },
			func(int) (pollerIdentity, error) { return testPollerIdentity(session), nil },
			func(data []byte) error {
				close(stopStarted)
				<-releaseStop
				return os.WriteFile(pollerStopFile(townRoot, session), data, 0600)
			},
			func(int, pollerRecord) error { return nil },
			func(string, []byte) error { return nil },
			os.Remove,
		)
	}()
	<-stopStarted

	launcher := func(string, string, []string) (pollerLaunch, error) {
		close(launchStarted)
		return pollerLaunch{pid: os.Getpid(), identity: testPollerIdentity(session)}, nil
	}
	startDone := make(chan error, 1)
	go func() {
		_, err := startPollerWithLauncherStatus(townRoot, session, nil, launcher, os.WriteFile, func(root, name string) (int, bool, error) {
			data, err := os.ReadFile(pollerPidFile(root, name))
			if os.IsNotExist(err) {
				return 0, false, nil
			}
			record, parseErr := parsePollerRecord(string(data))
			return record.PID, parseErr == nil, parseErr
		})
		startDone <- err
	}()

	select {
	case <-launchStarted:
		t.Fatal("StartPoller entered launch while StopPoller held custody lock")
	case <-time.After(500 * time.Millisecond):
	}
	close(releaseStop)
	if err := <-stopDone; err != nil {
		t.Fatal(err)
	}
	if err := <-startDone; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pollerPidFile(townRoot, session)); err != nil {
		t.Fatalf("final PID custody missing after serialized transition: %v", err)
	}
}

func TestStopPollerIdentityMismatchQuarantinesWithoutSignal(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	record := formatPollerRecord(123, pollerIdentity{StartTime: "old", Command: "gt nudge-poller " + session, Generation: "fixture-generation"}, session)
	if err := os.MkdirAll(pollerPidDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pollerPidFile(townRoot, session), []byte(record), 0644); err != nil {
		t.Fatal(err)
	}
	stopRequested := false
	quarantined := ""
	err := stopPollerWithGenerationOps(townRoot, session,
		os.ReadFile,
		func(int) bool { return true },
		func(int) (pollerIdentity, error) {
			return pollerIdentity{StartTime: "new", Command: "gt nudge-poller " + session, Generation: "fixture-generation"}, nil
		},
		func([]byte) error { stopRequested = true; return nil },
		func(int, pollerRecord) error { return nil },
		func(path string, _ []byte) error { quarantined = path + ".stale-test"; return nil },
		os.Remove,
	)
	if err == nil {
		t.Fatal("identity mismatch returned nil error")
	}
	if stopRequested {
		t.Fatal("identity mismatch published a cooperative stop request")
	}
	if quarantined == pollerPidFile(townRoot, session) || quarantined == "" {
		t.Fatalf("quarantine path = %q, want deterministic non-colliding stale path", quarantined)
	}
}

func TestStopPollerRemovesRecordOnlyAfterConfirmedExit(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	record := pollerRecord{PID: 123, Identity: pollerIdentity{StartTime: "same", Command: "gt nudge-poller " + session, Generation: "fixture-generation"}, Session: session}
	data := []byte(formatPollerRecordValue(record))
	if err := os.MkdirAll(pollerPidDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pollerPidFile(townRoot, session), data, 0644); err != nil {
		t.Fatal(err)
	}
	removed := make([]string, 0, 2)
	waited := false
	err := stopPollerWithGenerationOps(townRoot, session,
		os.ReadFile,
		func(int) bool { return true },
		func(int) (pollerIdentity, error) { return record.Identity, nil },
		func(stopData []byte) error { return os.WriteFile(pollerStopFile(townRoot, session), stopData, 0600) },
		func(int, pollerRecord) error { waited = true; return nil },
		func(string, []byte) error { return nil },
		func(path string) error { removed = append(removed, path); return os.Remove(path) },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !waited || len(removed) != 2 || removed[0] != pollerStopFile(townRoot, session) || removed[1] != pollerPidFile(townRoot, session) {
		t.Fatalf("confirmed exit custody = waited %v, removed %q", waited, removed)
	}
}

func TestStopPollerPreservesMismatchedStopRequest(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	oldRecord := pollerRecord{PID: 123, Identity: testPollerIdentity(session), Session: session}
	oldData := []byte(formatPollerRecordValue(oldRecord))
	replacement := oldRecord
	replacement.Identity.Generation = "replacement-generation"
	replacementData := []byte(formatPollerRecordValue(replacement))
	if err := os.MkdirAll(pollerPidDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pollerPidFile(townRoot, session), oldData, 0644); err != nil {
		t.Fatal(err)
	}

	err := stopPollerWithGenerationOps(townRoot, session,
		os.ReadFile,
		func(int) bool { return true },
		func(int) (pollerIdentity, error) { return oldRecord.Identity, nil },
		func([]byte) error { return os.WriteFile(pollerStopFile(townRoot, session), replacementData, 0600) },
		func(int, pollerRecord) error { return nil },
		func(string, []byte) error { t.Fatal("matching ownership quarantined"); return nil },
		os.Remove,
	)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(pollerStopFile(townRoot, session))
	if err != nil || !bytes.Equal(got, replacementData) {
		t.Fatalf("replacement stop request = %q, err %v", got, err)
	}
	if _, err := os.Stat(pollerPidFile(townRoot, session)); !os.IsNotExist(err) {
		t.Fatalf("old ownership still present after confirmed exit: %v", err)
	}
}

func TestStopPollerTimeoutPreservesRecord(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	record := pollerRecord{PID: 123, Identity: pollerIdentity{StartTime: "same", Command: "gt nudge-poller " + session, Generation: "fixture-generation"}, Session: session}
	data := []byte(formatPollerRecordValue(record))
	if err := os.MkdirAll(pollerPidDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pollerPidFile(townRoot, session), data, 0644); err != nil {
		t.Fatal(err)
	}
	removed := false
	err := stopPollerWithGenerationOps(townRoot, session,
		os.ReadFile,
		func(int) bool { return true },
		func(int) (pollerIdentity, error) { return record.Identity, nil },
		func(stopData []byte) error { return os.WriteFile(pollerStopFile(townRoot, session), stopData, 0600) },
		func(int, pollerRecord) error { return errors.New("exit confirmation timeout") },
		func(string, []byte) error { t.Fatal("matching ownership quarantined"); return nil },
		func(string) error { removed = true; return nil },
	)
	if err == nil {
		t.Fatal("timeout returned nil error")
	}
	if removed {
		t.Fatal("timeout removed live process ownership record")
	}
	for _, path := range []string{pollerPidFile(townRoot, session), pollerStopFile(townRoot, session)} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("timeout custody %s: %v", path, statErr)
		}
	}
}

func TestWaitPollerExitTreatsReusedPIDAsOriginalExit(t *testing.T) {
	session := "gt-gastown-polecat-test"
	record := pollerRecord{PID: 123, Identity: testPollerIdentity(session), Session: session}
	identityChecks := 0
	err := waitPollerExitWithOps(record.PID, record,
		func(int) bool { return true },
		func(int) (pollerIdentity, error) {
			identityChecks++
			return pollerIdentity{
				StartTime:  "reused-start",
				Command:    "unrelated-process",
				Generation: "unrelated-generation",
			}, nil
		},
	)
	if err != nil || identityChecks != 1 {
		t.Fatalf("PID reuse exit = err %v identityChecks %d", err, identityChecks)
	}
}

func TestStopPollerReplacementRecordPreserved(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	oldRecord := []byte(formatPollerRecord(123, pollerIdentity{StartTime: "same", Command: "gt nudge-poller " + session, Generation: "fixture-generation"}, session))
	newRecord := []byte(formatPollerRecord(456, pollerIdentity{StartTime: "new", Command: "gt nudge-poller " + session, Generation: "fixture-generation"}, session))
	reads := 0
	removed := false
	err := stopPollerWithGenerationOps(townRoot, session,
		func(string) ([]byte, error) {
			reads++
			if reads == 1 {
				return oldRecord, nil
			}
			return newRecord, nil
		},
		func(int) bool { return true },
		func(int) (pollerIdentity, error) {
			return pollerIdentity{StartTime: "same", Command: "gt nudge-poller " + session, Generation: "fixture-generation"}, nil
		},
		func(stopData []byte) error {
			if !bytes.Equal(stopData, oldRecord) {
				t.Fatalf("stop request = %q, want exact old ownership", stopData)
			}
			return nil
		},
		func(int, pollerRecord) error { return nil },
		func(string, []byte) error { return nil },
		func(string) error { removed = true; return nil },
	)
	if err == nil || removed || reads != 2 {
		t.Fatalf("replacement transition = err %v removed %v reads %d", err, removed, reads)
	}
}

func TestStopPollerGenerationRejectsAdvancedRecord(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	pidPath := pollerPidFile(townRoot, session)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	oldRecord := []byte("old-generation")
	newRecord := []byte("replacement-generation")
	if err := os.WriteFile(pidPath, oldRecord, 0o600); err != nil {
		t.Fatal(err)
	}
	generation, err := CapturePollerGeneration(townRoot, session)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidPath, newRecord, 0o600); err != nil {
		t.Fatal(err)
	}

	err = StopPollerGeneration(townRoot, session, generation)
	if err == nil || !strings.Contains(err.Error(), "poller ownership advanced") {
		t.Fatalf("StopPollerGeneration() error = %v, want advanced-generation refusal", err)
	}
	got, readErr := os.ReadFile(pidPath)
	if readErr != nil || !bytes.Equal(got, newRecord) {
		t.Fatalf("replacement ownership = %q, err %v", got, readErr)
	}
	if _, statErr := os.Stat(pollerStopFile(townRoot, session)); !os.IsNotExist(statErr) {
		t.Fatalf("replacement generation received stop request: %v", statErr)
	}
}

func TestStopPollerRejectsSameStartDifferentCommand(t *testing.T) {
	session := "gt-gastown-polecat-test"
	record := pollerRecord{PID: 123, Identity: pollerIdentity{StartTime: "same", Command: "gt nudge-poller " + session, Generation: "fixture-generation"}, Session: session}
	stopRequested := false
	err := stopPollerWithGenerationOps(t.TempDir(), session,
		func(string) ([]byte, error) { return []byte(formatPollerRecordValue(record)), nil },
		func(int) bool { return true },
		func(int) (pollerIdentity, error) {
			return pollerIdentity{StartTime: "same", Command: "gt other-command " + session, Generation: "fixture-generation"}, nil
		},
		func([]byte) error { stopRequested = true; return nil },
		func(int, pollerRecord) error { return nil },
		func(string, []byte) error { return nil },
		func(string) error { return nil },
	)
	if err == nil || stopRequested {
		t.Fatalf("same-start command mismatch = err %v stopRequested %v", err, stopRequested)
	}
}

func TestStartPollerReadErrorBlocksLaunch(t *testing.T) {
	launched := false
	readErr := errors.New("permission denied")
	_, err := startPollerWithLauncherStatus(t.TempDir(), "gt-gastown-polecat-test", nil,
		func(string, string, []string) (pollerLaunch, error) { launched = true; return pollerLaunch{}, nil },
		os.WriteFile,
		func(string, string) (int, bool, error) { return 0, false, readErr },
	)
	if !errors.Is(err, readErr) || launched {
		t.Fatalf("read error start = err %v launched %v", err, launched)
	}
}

func TestStartPollerIdentityCleanupFailureIsReturned(t *testing.T) {
	cleanupErr := errors.New("wait failed")
	_, err := startPollerWithLauncher(t.TempDir(), "gt-gastown-polecat-test", nil,
		func(string, string, []string) (pollerLaunch, error) {
			return pollerLaunch{pid: 123, terminate: func() error { return cleanupErr }}, nil
		}, os.WriteFile)
	if !errors.Is(err, cleanupErr) || err == nil {
		t.Fatalf("identity cleanup error = %v, want joined cleanup failure", err)
	}
}

func TestPollerStatusLiveLegacyRequiresVerifiedMigration(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	pid, alive, err := pollerStatusWithOps(townRoot, session,
		func(string) ([]byte, error) { return []byte("123"), nil },
		func(int) bool { return true },
		func(int) (pollerIdentity, error) {
			return pollerIdentity{StartTime: "current", Command: "gt nudge-poller " + session, Generation: "fixture-generation"}, nil
		},
		func(string, []byte) error { t.Fatal("live legacy record quarantined"); return nil },
		func(string) error { t.Fatal("live legacy record removed"); return nil },
	)
	if err == nil || pid != 123 || !alive {
		t.Fatalf("live legacy status = pid %d alive %v err %v; want preserved migration error", pid, alive, err)
	}
}

func TestStopRequestedIsSessionBound(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	if err := os.MkdirAll(pollerPidDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	data := []byte(formatPollerRecord(123, testPollerIdentity(session), session))
	if err := os.WriteFile(pollerStopFile(townRoot, session), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pollerPidFile(townRoot, session), data, 0600); err != nil {
		t.Fatal(err)
	}
	if !StopRequested(townRoot, session) || StopRequested(townRoot, session+"-other") {
		t.Fatal("cooperative stop generation was not session-bound")
	}
}

func TestStopRequestedRejectsStaleGeneration(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	if err := os.MkdirAll(pollerPidDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	old := []byte(formatPollerRecord(123, testPollerIdentity(session), session))
	current := []byte(formatPollerRecord(123, pollerIdentity{StartTime: "test-start", Command: "gt nudge-poller " + session, Generation: "new-generation"}, session))
	if err := os.WriteFile(pollerStopFile(townRoot, session), old, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pollerPidFile(townRoot, session), current, 0600); err != nil {
		t.Fatal(err)
	}
	if StopRequested(townRoot, session) {
		t.Fatal("stale stop request matched replacement ownership")
	}
}

func TestPollerRecordRoundTripsDelimitersAndGeneration(t *testing.T) {
	session := "session|with|pipes"
	identity := pollerIdentity{StartTime: "start", Command: "gt|nudge-poller|" + session, Generation: "generation-b"}
	record, err := parsePollerRecord(formatPollerRecord(123, identity, session))
	if err != nil || record.Identity != identity || record.Session != session {
		t.Fatalf("record round trip = %#v, err %v", record, err)
	}
}

func TestStopPollerRemovesDeadStructuredRecord(t *testing.T) {
	removed := false
	session := "gt-gastown-polecat-test"
	data := []byte(formatPollerRecord(123, testPollerIdentity(session), session))
	err := stopPollerWithGenerationOps(t.TempDir(), session,
		func(string) ([]byte, error) { return data, nil },
		func(int) bool { return false },
		func(int) (pollerIdentity, error) {
			t.Fatal("dead structured identity was queried")
			return pollerIdentity{}, nil
		},
		func([]byte) error { t.Fatal("dead structured poller received a stop request"); return nil },
		func(int, pollerRecord) error { t.Fatal("dead structured poller was waited on"); return nil },
		func(string, []byte) error { t.Fatal("dead structured record quarantined"); return nil },
		func(string) error { removed = true; return nil },
	)
	if err != nil || !removed {
		t.Fatalf("dead structured stop = err %v removed %v; want removal", err, removed)
	}
}

func TestStopPollerRemovesRecordWhenIdentityLookupSeesExit(t *testing.T) {
	removed := false
	livenessChecks := 0
	session := "gt-gastown-polecat-test"
	data := []byte(formatPollerRecord(123, testPollerIdentity(session), session))
	err := stopPollerWithGenerationOps(t.TempDir(), session,
		func(string) ([]byte, error) { return data, nil },
		func(int) bool { livenessChecks++; return livenessChecks == 1 },
		func(int) (pollerIdentity, error) { return pollerIdentity{}, errors.New("identity unavailable") },
		func([]byte) error { t.Fatal("exited poller received a stop request"); return nil },
		func(int, pollerRecord) error { t.Fatal("exited poller was waited on"); return nil },
		func(string, []byte) error { t.Fatal("exited record quarantined"); return nil },
		func(string) error { removed = true; return nil },
	)
	if err != nil || !removed || livenessChecks != 2 {
		t.Fatalf("identity-race stop = err %v removed %v liveness checks %d; want removal after recheck", err, removed, livenessChecks)
	}
}

func TestStartPollerLiveLegacyDoesNotLaunchDuplicate(t *testing.T) {
	launched := false
	_, err := startPollerWithLauncherStatus(t.TempDir(), "gt-gastown-polecat-test", nil,
		func(string, string, []string) (pollerLaunch, error) {
			launched = true
			return pollerLaunch{}, nil
		},
		os.WriteFile,
		func(string, string) (int, bool, error) {
			return 123, true, errors.New("legacy poller ownership requires separately verified migration")
		},
	)
	if err == nil || launched {
		t.Fatalf("live legacy start = err %v launched %v; want migration error without duplicate", err, launched)
	}
}

func TestPollerStatusDeadLegacyRemovesRecord(t *testing.T) {
	removed := false
	_, alive, err := pollerStatusWithOps(t.TempDir(), "gt-gastown-polecat-test",
		func(string) ([]byte, error) { return []byte("123"), nil },
		func(int) bool { return false },
		func(int) (pollerIdentity, error) {
			t.Fatal("dead legacy identity was queried")
			return pollerIdentity{}, nil
		},
		func(string, []byte) error { return nil },
		func(string) error { removed = true; return nil },
	)
	if err != nil || alive || !removed {
		t.Fatalf("dead legacy status = alive %v err %v removed %v", alive, err, removed)
	}
}

func TestPollerStatusMalformedLegacyPreservesRecord(t *testing.T) {
	removed := false
	_, alive, err := pollerStatusWithOps(t.TempDir(), "gt-gastown-polecat-test",
		func(string) ([]byte, error) { return []byte("not-a-pid"), nil },
		func(int) bool { t.Fatal("malformed legacy liveness was queried"); return false },
		func(int) (pollerIdentity, error) { return pollerIdentity{}, nil },
		func(string, []byte) error { return nil },
		func(string) error { removed = true; return nil },
	)
	if err == nil || alive || removed {
		t.Fatalf("malformed legacy status = alive %v err %v removed %v; want visible migration error and custody", alive, err, removed)
	}
}

func TestPollerPidFile(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-crew-bear"

	pidFile := pollerPidFile(townRoot, session)
	expected := filepath.Join(townRoot, ".runtime", "nudge_poller", session+".pid")
	if pidFile != expected {
		t.Errorf("pollerPidFile() = %q, want %q", pidFile, expected)
	}
}

func TestPollerPidFile_SlashSanitized(t *testing.T) {
	townRoot := t.TempDir()
	session := "some/session"

	pidFile := pollerPidFile(townRoot, session)
	// Slashes should be replaced with underscores
	expected := filepath.Join(townRoot, ".runtime", "nudge_poller", "some_session.pid")
	if pidFile != expected {
		t.Errorf("pollerPidFile() = %q, want %q", pidFile, expected)
	}
}

func TestPollerAlive_NoPidFile(t *testing.T) {
	townRoot := t.TempDir()
	_, alive := pollerAlive(townRoot, "nonexistent-session")
	if alive {
		t.Error("pollerAlive() returned true for nonexistent PID file")
	}
}

func TestPollerAlive_StalePid(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-crew-test"

	// Write a PID file with an invalid PID (process doesn't exist).
	pidDir := pollerPidDir(townRoot)
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		t.Fatal(err)
	}
	pidPath := pollerPidFile(townRoot, session)
	// Use a very high PID that's almost certainly not running.
	if err := os.WriteFile(pidPath, []byte("999999999"), 0644); err != nil {
		t.Fatal(err)
	}

	_, alive := pollerAlive(townRoot, session)
	if alive {
		t.Error("pollerAlive() returned true for dead PID")
	}

	// Stale PID file should be cleaned up.
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("stale PID file was not cleaned up")
	}
}

func TestPollerAlive_CorruptPidFile(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-crew-test"

	pidDir := pollerPidDir(townRoot)
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		t.Fatal(err)
	}
	pidPath := pollerPidFile(townRoot, session)
	if err := os.WriteFile(pidPath, []byte("not-a-number"), 0644); err != nil {
		t.Fatal(err)
	}

	_, alive := pollerAlive(townRoot, session)
	if alive {
		t.Error("pollerAlive() returned true for corrupt PID file")
	}
}

func TestStopPoller_NoPidFile(t *testing.T) {
	townRoot := t.TempDir()
	// Should be a no-op, no error.
	if err := StopPoller(townRoot, "nonexistent"); err != nil {
		t.Errorf("StopPoller() unexpected error: %v", err)
	}
}

func TestStopPoller_StalePid(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-crew-test"

	// Write a stale PID file.
	pidDir := pollerPidDir(townRoot)
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		t.Fatal(err)
	}
	pidPath := pollerPidFile(townRoot, session)
	if err := os.WriteFile(pidPath, []byte("999999999"), 0644); err != nil {
		t.Fatal(err)
	}

	// Should succeed and clean up the stale PID file.
	if err := StopPoller(townRoot, session); err != nil {
		t.Errorf("StopPoller() unexpected error: %v", err)
	}

	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("StopPoller did not clean up stale PID file")
	}
}

func TestPollerAlive_LiveProcess(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-crew-test"

	// Write our own PID — we're definitely alive.
	pidDir := pollerPidDir(townRoot)
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		t.Fatal(err)
	}
	pidPath := pollerPidFile(townRoot, session)
	myPid := os.Getpid()
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(myPid)), 0644); err != nil {
		t.Fatal(err)
	}

	pid, alive := pollerAlive(townRoot, session)
	if !alive {
		t.Error("pollerAlive() returned false for live process")
	}
	if pid != myPid {
		t.Errorf("pollerAlive() pid = %d, want %d", pid, myPid)
	}
}

func TestBuildPollerCommand_UsesDetachedProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process group management is not supported on Windows")
	}
	townRoot := t.TempDir()
	env := []string{"PATH=/usr/bin", "GT_TOWN_SOCKET=private-town"}
	cmd := buildPollerCommand("/tmp/fake-gt", townRoot, "gt-gastown-crew-bear", env)

	if got, want := cmd.Dir, townRoot; got != want {
		t.Fatalf("cmd.Dir = %q, want %q", got, want)
	}
	if got, want := cmd.Path, "/tmp/fake-gt"; got != want {
		t.Fatalf("cmd.Path = %q, want %q", got, want)
	}
	if len(cmd.Args) != 3 || cmd.Args[1] != "nudge-poller" || cmd.Args[2] != "gt-gastown-crew-bear" {
		t.Fatalf("cmd.Args = %#v, want poller invocation", cmd.Args)
	}
	if cmd.Cancel != nil {
		t.Fatal("buildPollerCommand() installed cmd.Cancel; detached pollers must leave it nil")
	}
	if cmd.Stdout != nil || cmd.Stderr != nil {
		t.Fatal("buildPollerCommand() should discard stdout/stderr")
	}
	if got := strings.Join(cmd.Env, "|"); got != "PATH=/usr/bin|GT_TOWN_SOCKET=private-town" {
		t.Fatalf("buildPollerCommand() env = %q, want isolated child environment", got)
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("buildPollerCommand() did not configure SysProcAttr")
	}
}

func TestSetProcessGroup_InstallsCancelHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SetProcessGroup is a no-op on Windows")
	}
	cmd := exec.Command("true")
	util.SetProcessGroup(cmd)

	if cmd.Cancel == nil {
		t.Fatal("SetProcessGroup() should install a cancel hook")
	}
}
