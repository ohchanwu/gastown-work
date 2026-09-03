package beads

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestFormatEscalationDescription(t *testing.T) {
	tests := []struct {
		name   string
		title  string
		fields *EscalationFields
		want   []string
		notIn  []string
	}{
		{
			name:   "nil fields returns title only",
			title:  "Test Escalation",
			fields: nil,
			want:   []string{"Test Escalation"},
			notIn:  []string{"severity:"},
		},
		{
			name:  "basic escalation",
			title: "Build failure",
			fields: &EscalationFields{
				Severity:    "high",
				Reason:      "Build failed 3 times",
				Source:      "patrol:deacon",
				EscalatedBy: "gastown/deacon",
				EscalatedAt: "2024-01-15T10:00:00Z",
			},
			want: []string{
				"Build failure",
				"severity: high",
				"reason: Build failed 3 times",
				"source: patrol:deacon",
				"escalated_by: gastown/deacon",
				"escalated_at: 2024-01-15T10:00:00Z",
			},
		},
		{
			name:  "acknowledged escalation",
			title: "Agent stuck",
			fields: &EscalationFields{
				Severity:    "medium",
				Reason:      "Agent not responding",
				EscalatedBy: "gastown/witness",
				EscalatedAt: "2024-01-15T10:00:00Z",
				AckedBy:     "gastown/crew/joe",
				AckedAt:     "2024-01-15T10:05:00Z",
			},
			want: []string{
				"severity: medium",
				"acked_by: gastown/crew/joe",
				"acked_at: 2024-01-15T10:05:00Z",
			},
		},
		{
			name:  "closed escalation",
			title: "Disk full",
			fields: &EscalationFields{
				Severity:     "critical",
				Reason:       "Disk >95%",
				EscalatedBy:  "gastown/deacon",
				EscalatedAt:  "2024-01-15T10:00:00Z",
				ClosedBy:     "human",
				ClosedReason: "Cleaned up temp files",
			},
			want: []string{
				"closed_by: human",
				"closed_reason: Cleaned up temp files",
			},
		},
		{
			name:  "null fields formatted explicitly",
			title: "New escalation",
			fields: &EscalationFields{
				Severity:    "low",
				Reason:      "Minor issue",
				EscalatedBy: "test",
				EscalatedAt: "2024-01-01T00:00:00Z",
			},
			want: []string{
				"acked_by: null",
				"acked_at: null",
				"closed_by: null",
				"closed_reason: null",
				"related_bead: null",
				"original_severity: null",
			},
		},
		{
			name:  "reescalation fields",
			title: "Bumped escalation",
			fields: &EscalationFields{
				Severity:          "high",
				Reason:            "Stale for 2h",
				EscalatedBy:       "patrol",
				EscalatedAt:       "2024-01-15T08:00:00Z",
				OriginalSeverity:  "low",
				ReescalationCount: 2,
				LastReescalatedAt: "2024-01-15T10:00:00Z",
				LastReescalatedBy: "deacon",
			},
			want: []string{
				"original_severity: low",
				"reescalation_count: 2",
				"last_reescalated_at: 2024-01-15T10:00:00Z",
				"last_reescalated_by: deacon",
			},
		},
		{
			name:  "fingerprint field",
			title: "Repeated alert",
			fields: &EscalationFields{
				Severity:    "medium",
				Reason:      "control-plane timeout",
				EscalatedBy: "deacon",
				EscalatedAt: "2024-01-15T10:00:00Z",
				Fingerprint: "escalation-fp:abc123def456",
			},
			want: []string{
				"fingerprint: escalation-fp:abc123def456",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatEscalationDescription(tt.title, tt.fields)
			for _, line := range tt.want {
				if !strings.Contains(got, line) {
					t.Errorf("missing line %q in output:\n%s", line, got)
				}
			}
			for _, line := range tt.notIn {
				if strings.Contains(got, line) {
					t.Errorf("unexpected %q in output:\n%s", line, got)
				}
			}
		})
	}
}

func TestParseEscalationFields(t *testing.T) {
	tests := []struct {
		name string
		desc string
		want *EscalationFields
	}{
		{
			name: "empty description",
			desc: "",
			want: &EscalationFields{},
		},
		{
			name: "full escalation",
			desc: `Escalation: Build failure

severity: high
reason: Build failed 3 times
source: patrol:deacon
escalated_by: gastown/deacon
escalated_at: 2024-01-15T10:00:00Z
acked_by: gastown/crew/joe
acked_at: 2024-01-15T10:05:00Z
closed_by: null
closed_reason: null
related_bead: gt-abc123
original_severity: medium
reescalation_count: 1
last_reescalated_at: 2024-01-15T09:30:00Z
last_reescalated_by: deacon
fingerprint: escalation-fp:abc123def456`,
			want: &EscalationFields{
				Severity:          "high",
				Reason:            "Build failed 3 times",
				Source:            "patrol:deacon",
				EscalatedBy:       "gastown/deacon",
				EscalatedAt:       "2024-01-15T10:00:00Z",
				AckedBy:           "gastown/crew/joe",
				AckedAt:           "2024-01-15T10:05:00Z",
				ClosedBy:          "",
				ClosedReason:      "",
				RelatedBead:       "gt-abc123",
				OriginalSeverity:  "medium",
				ReescalationCount: 1,
				LastReescalatedAt: "2024-01-15T09:30:00Z",
				LastReescalatedBy: "deacon",
				Fingerprint:       "escalation-fp:abc123def456",
			},
		},
		{
			name: "null values become empty strings",
			desc: "severity: critical\nsource: null\nacked_by: null",
			want: &EscalationFields{
				Severity: "critical",
				Source:   "",
				AckedBy:  "",
			},
		},
		{
			name: "invalid reescalation_count ignored",
			desc: "severity: low\nreescalation_count: not-a-number",
			want: &EscalationFields{
				Severity:          "low",
				ReescalationCount: 0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseEscalationFields(tt.desc)
			if got.Severity != tt.want.Severity {
				t.Errorf("Severity = %q, want %q", got.Severity, tt.want.Severity)
			}
			if got.Reason != tt.want.Reason {
				t.Errorf("Reason = %q, want %q", got.Reason, tt.want.Reason)
			}
			if got.Source != tt.want.Source {
				t.Errorf("Source = %q, want %q", got.Source, tt.want.Source)
			}
			if got.EscalatedBy != tt.want.EscalatedBy {
				t.Errorf("EscalatedBy = %q, want %q", got.EscalatedBy, tt.want.EscalatedBy)
			}
			if got.EscalatedAt != tt.want.EscalatedAt {
				t.Errorf("EscalatedAt = %q, want %q", got.EscalatedAt, tt.want.EscalatedAt)
			}
			if got.AckedBy != tt.want.AckedBy {
				t.Errorf("AckedBy = %q, want %q", got.AckedBy, tt.want.AckedBy)
			}
			if got.AckedAt != tt.want.AckedAt {
				t.Errorf("AckedAt = %q, want %q", got.AckedAt, tt.want.AckedAt)
			}
			if got.ClosedBy != tt.want.ClosedBy {
				t.Errorf("ClosedBy = %q, want %q", got.ClosedBy, tt.want.ClosedBy)
			}
			if got.ClosedReason != tt.want.ClosedReason {
				t.Errorf("ClosedReason = %q, want %q", got.ClosedReason, tt.want.ClosedReason)
			}
			if got.RelatedBead != tt.want.RelatedBead {
				t.Errorf("RelatedBead = %q, want %q", got.RelatedBead, tt.want.RelatedBead)
			}
			if got.OriginalSeverity != tt.want.OriginalSeverity {
				t.Errorf("OriginalSeverity = %q, want %q", got.OriginalSeverity, tt.want.OriginalSeverity)
			}
			if got.ReescalationCount != tt.want.ReescalationCount {
				t.Errorf("ReescalationCount = %d, want %d", got.ReescalationCount, tt.want.ReescalationCount)
			}
			if got.LastReescalatedAt != tt.want.LastReescalatedAt {
				t.Errorf("LastReescalatedAt = %q, want %q", got.LastReescalatedAt, tt.want.LastReescalatedAt)
			}
			if got.LastReescalatedBy != tt.want.LastReescalatedBy {
				t.Errorf("LastReescalatedBy = %q, want %q", got.LastReescalatedBy, tt.want.LastReescalatedBy)
			}
			if got.Fingerprint != tt.want.Fingerprint {
				t.Errorf("Fingerprint = %q, want %q", got.Fingerprint, tt.want.Fingerprint)
			}
		})
	}
}

func TestEscalationFieldsRoundTripAnomalyLifecycle(t *testing.T) {
	fields := &EscalationFields{
		Severity:           "medium",
		Reason:             "dangling parents",
		EscalatedBy:        "reaper",
		EscalatedAt:        "2026-08-01T00:00:00Z",
		Fingerprint:        "reaper-anomaly:v1:abc",
		AnomalyFamily:      "reaper-anomaly-family:v1:def",
		AnomalyScope:       "hq",
		PreviousOccurrence: "hq-esc-previous",
		AnomalyMailStored:  true,
	}

	got := ParseEscalationFields(FormatEscalationDescription("Reaper anomaly", fields))
	if got.AnomalyFamily != fields.AnomalyFamily || got.AnomalyScope != fields.AnomalyScope ||
		got.PreviousOccurrence != fields.PreviousOccurrence || got.AnomalyMailStored != fields.AnomalyMailStored {
		t.Fatalf("lifecycle fields = %#v, want %#v", got, fields)
	}
}

func TestEscalationFieldsRoundTrip(t *testing.T) {
	original := &EscalationFields{
		Severity:          "high",
		Reason:            "Agent stuck for 1h",
		Source:            "patrol:witness",
		EscalatedBy:       "gastown/witness",
		EscalatedAt:       "2024-06-15T12:00:00Z",
		AckedBy:           "gastown/crew/joe",
		AckedAt:           "2024-06-15T12:05:00Z",
		RelatedBead:       "gt-stuck123",
		OriginalSeverity:  "medium",
		ReescalationCount: 1,
		LastReescalatedAt: "2024-06-15T11:30:00Z",
		LastReescalatedBy: "deacon",
		Fingerprint:       "escalation-fp:feedface1234",
	}

	formatted := FormatEscalationDescription("Escalation: Agent stuck", original)
	parsed := ParseEscalationFields(formatted)

	if parsed.Severity != original.Severity {
		t.Errorf("Severity: got %q, want %q", parsed.Severity, original.Severity)
	}
	if parsed.Reason != original.Reason {
		t.Errorf("Reason: got %q, want %q", parsed.Reason, original.Reason)
	}
	if parsed.Source != original.Source {
		t.Errorf("Source: got %q, want %q", parsed.Source, original.Source)
	}
	if parsed.EscalatedBy != original.EscalatedBy {
		t.Errorf("EscalatedBy: got %q, want %q", parsed.EscalatedBy, original.EscalatedBy)
	}
	if parsed.EscalatedAt != original.EscalatedAt {
		t.Errorf("EscalatedAt: got %q, want %q", parsed.EscalatedAt, original.EscalatedAt)
	}
	if parsed.AckedBy != original.AckedBy {
		t.Errorf("AckedBy: got %q, want %q", parsed.AckedBy, original.AckedBy)
	}
	if parsed.AckedAt != original.AckedAt {
		t.Errorf("AckedAt: got %q, want %q", parsed.AckedAt, original.AckedAt)
	}
	if parsed.RelatedBead != original.RelatedBead {
		t.Errorf("RelatedBead: got %q, want %q", parsed.RelatedBead, original.RelatedBead)
	}
	if parsed.OriginalSeverity != original.OriginalSeverity {
		t.Errorf("OriginalSeverity: got %q, want %q", parsed.OriginalSeverity, original.OriginalSeverity)
	}
	if parsed.ReescalationCount != original.ReescalationCount {
		t.Errorf("ReescalationCount: got %d, want %d", parsed.ReescalationCount, original.ReescalationCount)
	}
	if parsed.LastReescalatedAt != original.LastReescalatedAt {
		t.Errorf("LastReescalatedAt: got %q, want %q", parsed.LastReescalatedAt, original.LastReescalatedAt)
	}
	if parsed.LastReescalatedBy != original.LastReescalatedBy {
		t.Errorf("LastReescalatedBy: got %q, want %q", parsed.LastReescalatedBy, original.LastReescalatedBy)
	}
	if parsed.Fingerprint != original.Fingerprint {
		t.Errorf("Fingerprint: got %q, want %q", parsed.Fingerprint, original.Fingerprint)
	}
}

func TestPrepareEscalationTransitionFieldsLifecycle(t *testing.T) {
	observation := EscalationObservation{
		Severity:    "high",
		Scope:       "db-a,db-b",
		Reason:      "first sample",
		Source:      "reaper",
		EscalatedBy: "deacon/dogs/bravo",
		ObservedAt:  "2026-09-04T01:00:00Z",
		Fingerprint: "escalation-fp:abc123def456",
		Recipients:  []string{"overseer", "mayor/", "mayor/"},
		RelatedBead: "hq-incident",
	}

	fields, kind, err := prepareEscalationTransitionFields(nil, observation)
	if err != nil {
		t.Fatal(err)
	}
	if kind != EscalationTransitionCreated || fields.MaterialGeneration != 1 {
		t.Fatalf("initial transition = %q generation %d", kind, fields.MaterialGeneration)
	}
	if !reflect.DeepEqual(fields.PendingRecipients, []string{"mayor/", "overseer"}) {
		t.Fatalf("initial pending recipients = %#v", fields.PendingRecipients)
	}

	for i := range 20 {
		observation.Reason = "sample " + string(rune('a'+i))
		fields, kind, err = prepareEscalationTransitionFields(fields, observation)
		if err != nil {
			t.Fatal(err)
		}
		if kind != EscalationTransitionUnchanged || fields.MaterialGeneration != 1 {
			t.Fatalf("identical observation %d = %q generation %d", i, kind, fields.MaterialGeneration)
		}
	}

	fields.PendingRecipients = nil
	observation.Severity = "critical"
	fields, kind, err = prepareEscalationTransitionFields(fields, observation)
	if err != nil {
		t.Fatal(err)
	}
	if kind != EscalationTransitionChanged || fields.MaterialGeneration != 2 {
		t.Fatalf("severity transition = %q generation %d", kind, fields.MaterialGeneration)
	}

	fields.PendingRecipients = nil
	observation.Scope = "db-a,db-c"
	fields, kind, err = prepareEscalationTransitionFields(fields, observation)
	if err != nil {
		t.Fatal(err)
	}
	if kind != EscalationTransitionChanged || fields.MaterialGeneration != 3 {
		t.Fatalf("scope transition = %q generation %d", kind, fields.MaterialGeneration)
	}

	roundTrip := ParseEscalationFields(FormatEscalationDescription("incident", fields))
	if roundTrip.MaterialState != fields.MaterialState || roundTrip.MaterialGeneration != 3 ||
		!reflect.DeepEqual(roundTrip.PendingRecipients, fields.PendingRecipients) ||
		roundTrip.Scope != observation.Scope || roundTrip.LastObservedAt != observation.ObservedAt {
		t.Fatalf("transition round trip = %#v, want %#v", roundTrip, fields)
	}
}

func TestConvergeEscalationObservationSerializesOnePersistedOccurrence(t *testing.T) {
	b, statePath := newEscalationBDHarness(t)
	observation := EscalationObservation{
		Title:       "Dolt latency",
		Severity:    "high",
		Scope:       "town",
		Reason:      "slow query",
		Source:      "reaper",
		EscalatedBy: "deacon/dogs/bravo",
		ObservedAt:  "2026-09-04T01:00:00Z",
		Fingerprint: "escalation-fp:abc123def456",
		Recipients:  []string{"mayor/", "overseer"},
	}

	var wg sync.WaitGroup
	results := make(chan *EscalationTransition, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			transition, err := b.ConvergeEscalationObservation(observation)
			if err != nil {
				errs <- err
				return
			}
			results <- transition
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("ConvergeEscalationObservation: %v", err)
	}

	kinds := map[EscalationTransitionKind]int{}
	for result := range results {
		kinds[result.Kind]++
	}
	if kinds[EscalationTransitionCreated] != 1 || kinds[EscalationTransitionUnchanged] != 1 {
		t.Fatalf("transition kinds = %#v", kinds)
	}
	issue := readEscalationBDState(t, statePath)
	fields := ParseEscalationFields(issue.Description)
	if fields.MaterialGeneration != 1 || !reflect.DeepEqual(fields.PendingRecipients, []string{"mayor/", "overseer"}) {
		t.Fatalf("persisted fields = %#v", fields)
	}
}

func TestEscalationTransitionPersistenceFailuresRemainRetryable(t *testing.T) {
	const fingerprint = "escalation-fp:abc123def456"
	t.Run("observation update", func(t *testing.T) {
		b, statePath := newEscalationBDHarness(t)
		seedEscalationBDState(t, statePath, &EscalationFields{
			Severity: "high", Scope: "town", Fingerprint: fingerprint,
			MaterialState: "severity=high|scope=town", MaterialGeneration: 1,
		})
		t.Setenv("GT_ESCALATION_BD_FAIL", "update")
		_, err := b.ConvergeEscalationObservation(EscalationObservation{
			Title: "Dolt latency", Severity: "critical", Scope: "town",
			ObservedAt: "2026-09-04T02:00:00Z", Fingerprint: fingerprint,
			Recipients: []string{"mayor/"},
		})
		if err == nil {
			t.Fatal("ConvergeEscalationObservation accepted a failed persisted update")
		}
		if got := ParseEscalationFields(readEscalationBDState(t, statePath).Description); got.MaterialGeneration != 1 {
			t.Fatalf("failed update changed persisted generation to %d", got.MaterialGeneration)
		}
	})

	t.Run("stale and failed recipient completion", func(t *testing.T) {
		b, statePath := newEscalationBDHarness(t)
		seedEscalationBDState(t, statePath, &EscalationFields{
			Severity: "high", Scope: "town", Fingerprint: fingerprint,
			MaterialState: "severity=high|scope=town", MaterialGeneration: 2,
			PendingRecipients: []string{"mayor/", "overseer"},
		})
		if err := b.CompleteEscalationRecipient("hq-escalation-test", fingerprint, 1, "mayor/"); !errors.Is(err, ErrAgentFieldsChanged) {
			t.Fatalf("stale completion error = %v, want ErrAgentFieldsChanged", err)
		}
		t.Setenv("GT_ESCALATION_BD_FAIL", "update")
		if err := b.CompleteEscalationRecipient("hq-escalation-test", fingerprint, 2, "mayor/"); err == nil {
			t.Fatal("recipient completion accepted a failed persisted update")
		}
		got := ParseEscalationFields(readEscalationBDState(t, statePath).Description)
		if !reflect.DeepEqual(got.PendingRecipients, []string{"mayor/", "overseer"}) {
			t.Fatalf("failed completion changed pending recipients to %#v", got.PendingRecipients)
		}
	})
}

func TestEscalationBDHelperProcess(t *testing.T) {
	statePath := os.Getenv("GT_ESCALATION_BD_STATE")
	if statePath == "" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) > 0 {
		args = args[1:]
	}
	command := ""
	for _, arg := range args {
		switch arg {
		case "list", "create", "update", "show":
			command = arg
		}
	}
	if command == "" {
		_, _ = os.Stdout.WriteString("--allow-stale\n")
		os.Exit(0)
	}

	readState := func() *Issue {
		data, err := os.ReadFile(statePath)
		if err != nil {
			return nil
		}
		var issue Issue
		if json.Unmarshal(data, &issue) != nil {
			return nil
		}
		return &issue
	}
	writeState := func(issue *Issue) {
		data, _ := json.Marshal(issue)
		tmp := statePath + ".tmp"
		_ = os.WriteFile(tmp, data, 0600)
		_ = os.Rename(tmp, statePath)
	}
	writeJSON := func(value any) {
		_ = json.NewEncoder(os.Stdout).Encode(value)
	}
	flagValue := func(prefix string) string {
		for _, arg := range args {
			if strings.HasPrefix(arg, prefix) {
				return strings.TrimPrefix(arg, prefix)
			}
		}
		return ""
	}

	switch command {
	case "list":
		if issue := readState(); issue != nil {
			writeJSON([]*Issue{issue})
		} else {
			writeJSON([]*Issue{})
		}
	case "create":
		if readState() != nil {
			_, _ = os.Stderr.WriteString("duplicate escalation\n")
			os.Exit(1)
		}
		description, _ := io.ReadAll(os.Stdin)
		issue := &Issue{
			ID: "hq-escalation-test", Title: flagValue("--title="),
			Description: string(description), Status: "open", Type: "task", Ephemeral: true,
			Labels: []string{"gt:escalation"},
		}
		for _, arg := range args {
			if strings.HasPrefix(arg, "--labels=") {
				issue.Labels = append(issue.Labels, strings.TrimPrefix(arg, "--labels="))
			}
		}
		writeState(issue)
		writeJSON(issue)
	case "show":
		issue := readState()
		if issue == nil {
			_, _ = os.Stderr.WriteString("not found\n")
			os.Exit(1)
		}
		writeJSON([]*Issue{issue})
	case "update":
		if os.Getenv("GT_ESCALATION_BD_FAIL") == "update" {
			_, _ = os.Stderr.WriteString("injected update failure\n")
			os.Exit(1)
		}
		issue := readState()
		if issue == nil {
			os.Exit(1)
		}
		if title := flagValue("--title="); title != "" {
			issue.Title = title
		}
		for _, arg := range args {
			switch {
			case arg == "--body-file=-":
				description, _ := io.ReadAll(os.Stdin)
				issue.Description = string(description)
			case strings.HasPrefix(arg, "--add-label="):
				issue.Labels = append(issue.Labels, strings.TrimPrefix(arg, "--add-label="))
			case strings.HasPrefix(arg, "--remove-label="):
				remove := strings.TrimPrefix(arg, "--remove-label=")
				kept := issue.Labels[:0]
				for _, label := range issue.Labels {
					if label != remove {
						kept = append(kept, label)
					}
				}
				issue.Labels = kept
			}
		}
		writeState(issue)
		writeJSON(issue)
	}
	os.Exit(0)
}

func newEscalationBDHarness(t *testing.T) (*Beads, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fake bd is POSIX-only")
	}
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	stubPath := filepath.Join(dir, "bd")
	stub := "#!/bin/sh\nexec \"" + os.Args[0] + "\" -test.run=TestEscalationBDHelperProcess -- \"$@\"\n"
	if err := os.WriteFile(stubPath, []byte(stub), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GT_ESCALATION_BD_STATE", statePath)
	ResetBdAllowStaleCacheForTest()
	b := New(t.TempDir())
	b.noRoute = true
	return b, statePath
}

func seedEscalationBDState(t *testing.T, statePath string, fields *EscalationFields) {
	t.Helper()
	issue := &Issue{
		ID: "hq-escalation-test", Title: "Dolt latency",
		Description: FormatEscalationDescription("Dolt latency", fields),
		Status:      "open", Type: "task", Ephemeral: true,
		Labels: []string{"gt:escalation", fields.Fingerprint, "severity:" + fields.Severity},
	}
	data, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func readEscalationBDState(t *testing.T, statePath string) *Issue {
	t.Helper()
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var issue Issue
	if err := json.Unmarshal(data, &issue); err != nil {
		t.Fatal(err)
	}
	return &issue
}

func TestFilterEscalationRecordsSkipsMailMessages(t *testing.T) {
	issues := []*Issue{
		{ID: "hq-root", Labels: []string{"gt:escalation"}},
		{ID: "hq-mail", Labels: []string{"gt:escalation", "gt:message"}},
	}

	got := filterEscalationRecords(issues)
	if len(got) != 1 || got[0].ID != "hq-root" {
		t.Fatalf("filterEscalationRecords() = %#v, want only root escalation", got)
	}
}

func TestBumpSeverity(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"low", "medium"},
		{"medium", "high"},
		{"high", "critical"},
		{"critical", "critical"}, // already at max
		{"unknown", "critical"},  // default fallthrough
		{"", "critical"},         // empty defaults to critical
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := bumpSeverity(tt.input)
			if got != tt.want {
				t.Errorf("bumpSeverity(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestCreateEscalationBead_PassesDescriptionViaStdin verifies that
// CreateEscalationBead passes the multi-line description through bd's stdin
// (--body-file=-) rather than embedding newlines in --description=...
//
// Regression test for dc-1bxe: bd 1.0.3+ rejects newlines inside --description
// flag values, which broke `gt escalate` for any escalation containing the
// structured YAML metadata block (severity, reason, escalated_by, etc.).
func TestCreateEscalationBead_PassesDescriptionViaStdin(t *testing.T) {
	stubDir := t.TempDir()
	argsPath := filepath.Join(stubDir, "args.txt")
	stdinPath := filepath.Join(stubDir, "stdin.txt")

	// Stub bd: write each arg on its own line to args.txt, capture stdin to
	// stdin.txt, and emit a minimal valid issue JSON so unmarshal succeeds.
	stubScript := `#!/bin/sh
for a in "$@"; do
  printf '%s\n' "$a" >> "` + argsPath + `"
done
cat > "` + stdinPath + `"
echo '{"id":"dc-test1","title":"x","status":"open","priority":2,"type":"task","labels":["gt:escalation"]}'
exit 0
`
	stubPath := filepath.Join(stubDir, "bd")
	if err := os.WriteFile(stubPath, []byte(stubScript), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Reset --allow-stale capability cache so the stub gets probed fresh.
	ResetBdAllowStaleCacheForTest()

	b := New(t.TempDir())
	fields := &EscalationFields{
		Severity:    "high",
		Reason:      "multi-line\nreason\nwith embedded newlines",
		EscalatedBy: "test/agent",
		EscalatedAt: "2026-05-08T15:00:00Z",
		Fingerprint: "escalation-fp:abc123def456",
	}

	if _, err := b.CreateEscalationBead("Test escalation", fields); err != nil {
		t.Fatalf("CreateEscalationBead: %v", err)
	}

	argsData, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read stub args: %v", err)
	}
	args := string(argsData)

	// Must use --body-file=- to read description from stdin.
	if !strings.Contains(args, "--body-file=-") {
		t.Errorf("expected --body-file=- in bd args, got:\n%s", args)
	}
	if !strings.Contains(args, "--labels=escalation-fp:abc123def456") {
		t.Errorf("expected fingerprint label in bd args, got:\n%s", args)
	}
	// Must NOT pass --description=... at all (any --description value would
	// embed the newline-containing structured description and fail bd 1.0.3+).
	for _, line := range strings.Split(args, "\n") {
		if strings.HasPrefix(line, "--description=") {
			t.Errorf("--description=... must not be used (bd rejects newlines), got %q", line)
		}
	}

	stdinData, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatalf("read stub stdin: %v", err)
	}
	stdin := string(stdinData)
	// The structured description must reach bd via stdin.
	wantInStdin := []string{
		"Test escalation",
		"severity: high",
		"escalated_by: test/agent",
	}
	for _, want := range wantInStdin {
		if !strings.Contains(stdin, want) {
			t.Errorf("expected stdin to contain %q, got:\n%s", want, stdin)
		}
	}
	// Sanity: stdin must contain newlines (it's the multi-line description).
	if !strings.Contains(stdin, "\n") {
		t.Errorf("expected stdin to be multi-line, got %q", stdin)
	}
}
