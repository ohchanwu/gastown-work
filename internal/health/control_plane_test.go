package health

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/session"
)

func validCanaryEvidence(now time.Time) CanaryEvidence {
	receipts := make([]canaryReceipt, 20)
	for index := range receipts {
		startedAt := now.Add(time.Duration(index) * time.Second)
		receipts[index] = canaryReceipt{
			Turn: index + 1, DeliveryID: fmt.Sprintf("ndg-%02d", index+1), NonceDigest: fmt.Sprintf("%064x", index+1),
			SubmittedAt: startedAt.Add(time.Millisecond), WindowStartedAt: startedAt, WindowCompletedAt: startedAt.Add(time.Second),
		}
	}
	return CanaryEvidence{
		SchemaVersion: 3, BinaryCommit: "candidate", Result: "passed",
		MayorPreset: "codex", MayorProvider: "codex", PolecatPreset: "codex", PolecatProvider: "codex",
		Session: session.MayorSessionName(), Runtime: "codex", SessionAuthority: strings.Repeat("b", 64),
		Turns: 20, Submitted: 20, Receipts: receipts, ReceiptCount: 20, ReceiptDigest: canaryReceiptDigest(receipts),
		AttemptedAt: now, CompletedAt: now.Add(20 * time.Second),
	}
}

func TestEvaluateControlPlaneNamesConfirmedFailures(t *testing.T) {
	now := time.Date(2026, 8, 1, 4, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		mutate     func(*ControlPlaneEvidence)
		subsystem  string
		diagnostic string
	}{
		{
			name: "stale actionable Mayor mail",
			mutate: func(e *ControlPlaneEvidence) {
				e.MayorMail = []MailEvidence{{Priority: "normal", Type: "task", WrittenAt: now.Add(-31 * time.Second)}}
			},
			subsystem: "mayor-mail", diagnostic: "gt mail inbox mayor/",
		},
		{
			name: "queued urgent wake",
			mutate: func(e *ControlPlaneEvidence) {
				e.WakeDeliveries = []WakeEvidence{{Priority: "urgent", QueuedAt: now.Add(-31 * time.Second)}}
			},
			subsystem: "wake-delivery", diagnostic: "gt nudge --help",
		},
		{
			name: "retrying urgent wake past deadline",
			mutate: func(e *ControlPlaneEvidence) {
				e.WakeDeliveries = []WakeEvidence{{
					Priority: "urgent", QueuedAt: now.Add(-31 * time.Second), Attempts: 1,
					FailureCode: "submission-unconfirmed",
				}}
			},
			subsystem: "wake-delivery", diagnostic: "gt nudge --help",
		},
		{
			name:      "proven Dolt leak",
			mutate:    func(e *ControlPlaneEvidence) { e.ActionableDoltLeaks = 1 },
			subsystem: "dolt", diagnostic: "gt dolt cleanup-test-leaks",
		},
		{
			name:      "canonical Dolt unreachable",
			mutate:    func(e *ControlPlaneEvidence) { e.CanonicalDoltReachable = false },
			subsystem: "dolt", diagnostic: "gt dolt status",
		},
		{
			name: "convoy timeout",
			mutate: func(e *ControlPlaneEvidence) {
				e.ConvoyCheck = &ConvoyEvidence{TimedOut: true, Duration: 30 * time.Second}
			},
			subsystem: "convoy", diagnostic: "gt convoy check --dry-run --json",
		},
		{
			name: "current binary canary failure",
			mutate: func(e *ControlPlaneEvidence) {
				e.Canary = &CanaryEvidence{BinaryCommit: "candidate", Result: "failed"}
			},
			subsystem: "wake-canary", diagnostic: "gt nudge-canary --help",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			canary := validCanaryEvidence(now)
			evidence := ControlPlaneEvidence{
				Now: now, CanonicalDoltReachable: true, InstalledBinaryCommit: "candidate",
				MayorPreset: "codex", MayorProvider: "codex", PolecatPreset: "codex", PolecatProvider: "codex",
				Canary: &canary,
			}
			tt.mutate(&evidence)

			got := EvaluateControlPlane(evidence)
			if got.Healthy || len(got.Failures) != 1 {
				t.Fatalf("verdict = %#v, want one failure", got)
			}
			if got.Failures[0].Subsystem != tt.subsystem || got.Failures[0].Diagnostic != tt.diagnostic {
				t.Fatalf("failure = %#v, want subsystem %q diagnostic %q", got.Failures[0], tt.subsystem, tt.diagnostic)
			}
		})
	}
}

func TestEvaluateControlPlaneRejectsPassedCanaryWithoutReceiptProof(t *testing.T) {
	evidence := ControlPlaneEvidence{
		CanonicalDoltReachable: true,
		InstalledBinaryCommit:  "candidate",
		MayorPreset:            "codex-mayor",
		MayorProvider:          "codex",
		PolecatPreset:          "codex-polecat",
		PolecatProvider:        "codex",
		Canary: &CanaryEvidence{
			SchemaVersion: 2, BinaryCommit: "candidate", MayorPreset: "codex-mayor", MayorProvider: "codex",
			PolecatPreset: "codex-polecat", PolecatProvider: "codex", Result: "passed",
		},
	}

	got := EvaluateControlPlane(evidence)
	if got.Healthy || len(got.Failures) != 1 || got.Failures[0].Subsystem != "wake-canary" {
		t.Fatalf("verdict = %#v, want proofless passed canary rejected", got)
	}
}

func TestEvaluateControlPlaneRejectsMissingCanaryProof(t *testing.T) {
	got := EvaluateControlPlane(ControlPlaneEvidence{CanonicalDoltReachable: true})
	if got.Healthy || len(got.Failures) != 1 || got.Failures[0].Subsystem != "wake-canary" {
		t.Fatalf("verdict = %#v, want missing canary proof rejected", got)
	}
}

func TestEvaluateControlPlaneRejectsInvalidReceiptAuthority(t *testing.T) {
	now := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*CanaryEvidence)
	}{
		{name: "missing receipt", mutate: func(c *CanaryEvidence) { c.Receipts = c.Receipts[:19] }},
		{name: "duplicate receipt", mutate: func(c *CanaryEvidence) {
			c.Receipts[19].DeliveryID = c.Receipts[0].DeliveryID
			c.ReceiptDigest = canaryReceiptDigest(c.Receipts)
		}},
		{name: "duplicate nonce", mutate: func(c *CanaryEvidence) {
			c.Receipts[19].NonceDigest = c.Receipts[0].NonceDigest
			c.ReceiptDigest = canaryReceiptDigest(c.Receipts)
		}},
		{name: "wrong session", mutate: func(c *CanaryEvidence) { c.Session = "live" }},
		{name: "wrong runtime", mutate: func(c *CanaryEvidence) { c.Runtime = "claude" }},
		{name: "late receipt", mutate: func(c *CanaryEvidence) {
			c.Receipts[19].SubmittedAt = c.Receipts[19].WindowCompletedAt.Add(time.Nanosecond)
			c.ReceiptDigest = canaryReceiptDigest(c.Receipts)
		}},
		{name: "changed digest", mutate: func(c *CanaryEvidence) { c.ReceiptDigest = strings.Repeat("c", 64) }},
		{name: "missing session authority", mutate: func(c *CanaryEvidence) { c.SessionAuthority = "" }},
		{name: "invalid session authority", mutate: func(c *CanaryEvidence) { c.SessionAuthority = strings.Repeat("z", 64) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			canary := validCanaryEvidence(now)
			canary.MayorPreset = "codex-mayor"
			canary.PolecatPreset = "codex-polecat"
			tt.mutate(&canary)
			got := EvaluateControlPlane(ControlPlaneEvidence{
				CanonicalDoltReachable: true, InstalledBinaryCommit: "candidate",
				MayorPreset: "codex-mayor", MayorProvider: "codex", PolecatPreset: "codex-polecat", PolecatProvider: "codex",
				Canary: &canary,
			})
			if got.Healthy || len(got.Failures) != 1 || got.Failures[0].Subsystem != "wake-canary" {
				t.Fatalf("verdict = %#v, want invalid receipt authority rejected", got)
			}
		})
	}
}

func TestCollectControlPlaneMapsExistingEvidenceWithoutPrivateData(t *testing.T) {
	now := time.Date(2026, 8, 1, 4, 0, 0, 0, time.UTC)
	sources := controlPlaneSources{
		now: func() time.Time { return now },
		unreadMayorMail: func(string) ([]*mail.Message, error) {
			return []*mail.Message{{
				Subject: "private subject", Body: "private body", Priority: mail.PriorityHigh,
				Type: mail.TypeNotification, Timestamp: now.Add(-time.Minute),
			}}, nil
		},
		queuedMayorWake: func(string) ([]nudge.QueuedNudge, error) {
			return []nudge.QueuedNudge{{
				Message: "private wake", Priority: nudge.PriorityUrgent,
				Timestamp: now.Add(-time.Minute), LastErrorCode: "submission-unconfirmed",
			}}, nil
		},
		inventory: func(string) []doltserver.LocalDoltServer {
			return []doltserver.LocalDoltServer{{Class: doltserver.DoltServerOwnedTestLeak}}
		},
		canonicalDoltReachable: func(string) (bool, error) { return true, nil },
		readFile: func(path string) ([]byte, error) {
			switch filepath.Base(path) {
			case "convoy-check.json":
				return []byte(`{"schema_version":1,"checked_at":"2026-08-01T03:59:00Z","duration_ms":31000,"timed_out":true}`), nil
			case "control-plane.json":
				return []byte(`{"schema_version":1,"installed_binary_commit":"candidate","result":"failed"}`), nil
			default:
				return nil, os.ErrNotExist
			}
		},
	}

	got, err := collectControlPlane("/private/town", "candidate", sources)
	if err != nil {
		t.Fatalf("collectControlPlane: %v", err)
	}
	if len(got.Failures) != 5 {
		t.Fatalf("failures = %#v, want five subsystems", got.Failures)
	}
	rendered := fmt.Sprintf("%#v", got)
	for _, private := range []string{"private subject", "private body", "private wake", "/private/town"} {
		if strings.Contains(rendered, private) {
			t.Fatalf("verdict leaked %q: %s", private, rendered)
		}
	}
}

func TestCollectControlPlaneReturnsSanitizedEvidenceError(t *testing.T) {
	sources := controlPlaneSources{
		now:             time.Now,
		unreadMayorMail: func(string) ([]*mail.Message, error) { return nil, errors.New("/private/mail failed") },
	}
	_, err := collectControlPlane("/private/town", "candidate", sources)
	if err == nil || err.Error() != "reading Mayor mail evidence failed" {
		t.Fatalf("error = %v", err)
	}
}

func TestCollectControlPlaneNonDoltDoesNotProbeDolt(t *testing.T) {
	sources := controlPlaneSources{
		now:                    time.Now,
		unreadMayorMail:        func(string) ([]*mail.Message, error) { return nil, nil },
		queuedMayorWake:        func(string) ([]nudge.QueuedNudge, error) { return nil, nil },
		inventory:              func(string) []doltserver.LocalDoltServer { t.Fatal("Dolt inventory called"); return nil },
		canonicalDoltReachable: func(string) (bool, error) { t.Fatal("Dolt reachability called"); return false, nil },
		readFile:               func(string) ([]byte, error) { return nil, os.ErrNotExist },
	}

	got, err := collectControlPlaneNonDolt(t.TempDir(), "candidate", sources)
	if err != nil || got.Healthy || len(got.Failures) != 1 || got.Failures[0].Subsystem != "wake-canary" {
		t.Fatalf("non-Dolt verdict = %#v, %v; want missing canary proof only", got, err)
	}
}

func TestWriteConvoyCheckEvidenceIsPrivateAndSanitized(t *testing.T) {
	townRoot := t.TempDir()
	path, err := WriteConvoyCheckEvidence(townRoot, ConvoyCheckState{
		SchemaVersion: 1, CheckedAt: time.Now(), DurationMS: 31_000, TimedOut: true, SkippedUncertain: 2,
	})
	if err != nil {
		t.Fatalf("WriteConvoyCheckEvidence: %v", err)
	}
	if path != filepath.Join(townRoot, ".runtime", "health", "convoy-check.json") {
		t.Fatalf("path = %q", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"convoy_id", "path", "error"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("state contains %q: %s", forbidden, data)
		}
	}
}

func TestEvaluateControlPlaneIgnoresNonActionableEvidence(t *testing.T) {
	now := time.Date(2026, 8, 1, 4, 0, 0, 0, time.UTC)
	evidence := ControlPlaneEvidence{
		Now:                    now,
		CanonicalDoltReachable: true,
		InstalledBinaryCommit:  "candidate",
		MayorMail:              []MailEvidence{{Priority: "low", Type: "notification", WrittenAt: now.Add(-time.Hour)}},
		WakeDeliveries:         []WakeEvidence{{Priority: "normal", QueuedAt: now.Add(-time.Hour)}},
		Canary:                 func() *CanaryEvidence { canary := validCanaryEvidence(now); return &canary }(),
		MayorPreset:            "codex", MayorProvider: "codex",
		PolecatPreset: "codex", PolecatProvider: "codex",
	}

	got := EvaluateControlPlane(evidence)
	if !got.Healthy || len(got.Failures) != 0 {
		t.Fatalf("verdict = %#v, want healthy", got)
	}
}

func TestEvaluateControlPlaneRejectsCanaryForDifferentConfiguredRoles(t *testing.T) {
	canary := validCanaryEvidence(time.Now())
	got := EvaluateControlPlane(ControlPlaneEvidence{
		Now:                    time.Now(),
		CanonicalDoltReachable: true,
		InstalledBinaryCommit:  "candidate",
		MayorPreset:            "codex-mayor",
		MayorProvider:          "codex",
		PolecatPreset:          "codex-polecat",
		PolecatProvider:        "codex",
		Canary:                 &canary,
	})

	if got.Healthy || len(got.Failures) != 1 || got.Failures[0].Subsystem != "wake-canary" {
		t.Fatalf("verdict = %#v, want configured-role canary failure", got)
	}
}

func TestEvaluateControlPlaneRejectsStaleOrWrongProviderCanary(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CanaryEvidence)
	}{
		{name: "stale binary", mutate: func(c *CanaryEvidence) { c.BinaryCommit = "previous" }},
		{name: "wrong Mayor provider", mutate: func(c *CanaryEvidence) { c.MayorProvider = "claude" }},
		{name: "wrong polecat provider", mutate: func(c *CanaryEvidence) { c.PolecatProvider = "claude" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			canary := validCanaryEvidence(time.Now())
			canary.MayorPreset = "codex-mayor"
			canary.PolecatPreset = "codex-polecat"
			tt.mutate(&canary)
			got := EvaluateControlPlane(ControlPlaneEvidence{
				Now: time.Now(), CanonicalDoltReachable: true, InstalledBinaryCommit: "candidate",
				MayorPreset: "codex-mayor", MayorProvider: "codex",
				PolecatPreset: "codex-polecat", PolecatProvider: "codex", Canary: &canary,
			})
			if got.Healthy || len(got.Failures) != 1 || got.Failures[0].Subsystem != "wake-canary" {
				t.Fatalf("verdict = %#v, want wake-canary failure", got)
			}
		})
	}
}

func TestEvaluateControlPlaneKeepsRetryingUrgentWakeHealthyBeforeDeadline(t *testing.T) {
	now := time.Now()
	canary := validCanaryEvidence(now)
	got := EvaluateControlPlane(ControlPlaneEvidence{
		Now:                    now,
		CanonicalDoltReachable: true,
		InstalledBinaryCommit:  "candidate",
		MayorPreset:            "codex",
		MayorProvider:          "codex",
		PolecatPreset:          "codex",
		PolecatProvider:        "codex",
		Canary:                 &canary,
		WakeDeliveries: []WakeEvidence{{
			Priority: "urgent", QueuedAt: now.Add(-time.Second), Attempts: 1,
			FailureCode: "submission-unconfirmed",
		}},
	})
	if !got.Healthy || len(got.Failures) != 0 {
		t.Fatalf("verdict = %#v, want retrying delivery healthy before deadline", got)
	}
}

func TestEvaluateControlPlaneCanonicalDoltFailureOutranksCleanup(t *testing.T) {
	now := time.Now()
	canary := validCanaryEvidence(now)
	got := EvaluateControlPlane(ControlPlaneEvidence{
		Now: now, CanonicalDoltReachable: false, ActionableDoltLeaks: 3,
		InstalledBinaryCommit: "candidate", MayorPreset: "codex", MayorProvider: "codex",
		PolecatPreset: "codex", PolecatProvider: "codex", Canary: &canary,
	})
	if len(got.Failures) != 1 || got.Failures[0].Subsystem != "dolt" || got.Failures[0].Diagnostic != "gt dolt status" {
		t.Fatalf("verdict = %#v, want canonical status diagnostic", got)
	}
}
