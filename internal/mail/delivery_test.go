package mail

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/nudge"
)

func TestParseDeliveryLabels_CrashAndRetryStates(t *testing.T) {
	t.Run("pending only", func(t *testing.T) {
		state, by, at := ParseDeliveryLabels([]string{
			DeliveryLabelPending,
		})
		if state != DeliveryStatePending {
			t.Fatalf("state = %q, want %q", state, DeliveryStatePending)
		}
		if by != "" || at != nil {
			t.Fatalf("pending state should not include ack metadata, got by=%q at=%v", by, at)
		}
	})

	t.Run("partial ack write keeps pending", func(t *testing.T) {
		state, by, at := ParseDeliveryLabels([]string{
			DeliveryLabelPending,
			"delivery-acked-by:gastown/worker",
			"delivery-acked-at:2026-02-17T12:00:00Z",
		})
		if state != DeliveryStatePending {
			t.Fatalf("state = %q, want %q", state, DeliveryStatePending)
		}
		if by != "" || at != nil {
			t.Fatalf("partial ack should not flip state, got by=%q at=%v", by, at)
		}
	})

	t.Run("acked label flips state", func(t *testing.T) {
		state, by, at := ParseDeliveryLabels([]string{
			DeliveryLabelPending,
			"delivery-acked-by:gastown/worker",
			"delivery-acked-at:2026-02-17T12:00:00Z",
			DeliveryLabelAcked,
		})
		if state != DeliveryStateAcked {
			t.Fatalf("state = %q, want %q", state, DeliveryStateAcked)
		}
		if by != "gastown/worker" {
			t.Fatalf("ackedBy = %q, want %q", by, "gastown/worker")
		}
		if at == nil {
			t.Fatal("ackedAt should be populated for acked state")
		}
	})

	t.Run("lexicographic label order still parses correctly", func(t *testing.T) {
		// bd show --json returns labels in lexicographic order.
		state, by, at := ParseDeliveryLabels([]string{
			"delivery-acked-at:2026-02-17T12:00:00Z",
			"delivery-acked-by:gastown/worker",
			"delivery:acked",
			"delivery:pending",
		})
		if state != DeliveryStateAcked {
			t.Fatalf("state = %q, want %q", state, DeliveryStateAcked)
		}
		if by != "gastown/worker" {
			t.Fatalf("ackedBy = %q, want %q", by, "gastown/worker")
		}
		if at == nil {
			t.Fatal("ackedAt should be populated for acked state with lex-ordered labels")
		}
	})
}

func TestDeliveryAckLabelSequence(t *testing.T) {
	t.Run("no existing labels uses new timestamp", func(t *testing.T) {
		at := time.Date(2026, 2, 17, 14, 0, 0, 0, time.UTC)
		got := DeliveryAckLabelSequence("gastown/worker", at, nil)
		want := []string{
			"delivery-acked-by:gastown/worker",
			"delivery-acked-at:2026-02-17T14:00:00Z",
			"delivery:acked",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("existing timestamp is reused on retry", func(t *testing.T) {
		existing := []string{
			"delivery:pending",
			"delivery-acked-by:gastown/worker",
			"delivery-acked-at:2026-02-17T12:00:00Z",
		}
		// Use a different time — should be ignored in favor of existing.
		at := time.Date(2026, 2, 17, 14, 0, 0, 0, time.UTC)
		got := DeliveryAckLabelSequence("gastown/worker", at, existing)
		want := []string{
			"delivery-acked-by:gastown/worker",
			"delivery-acked-at:2026-02-17T12:00:00Z",
			"delivery:acked",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("lexicographic label order still reuses timestamp", func(t *testing.T) {
		// bd show --json returns labels in lexicographic order, so acked-at
		// appears before acked-by. The function must be order-independent.
		existing := []string{
			"delivery-acked-at:2026-02-17T12:00:00Z",
			"delivery-acked-by:gastown/worker",
			"delivery:acked",
			"delivery:pending",
		}
		at := time.Date(2026, 2, 17, 14, 0, 0, 0, time.UTC)
		got := DeliveryAckLabelSequence("gastown/worker", at, existing)
		want := []string{
			"delivery-acked-by:gastown/worker",
			"delivery-acked-at:2026-02-17T12:00:00Z",
			"delivery:acked",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("different recipient gets fresh timestamp", func(t *testing.T) {
		existing := []string{
			"delivery:pending",
			"delivery-acked-by:gastown/workerA",
			"delivery-acked-at:2026-02-17T12:00:00Z",
		}
		// Different recipient — should NOT reuse workerA's timestamp.
		at := time.Date(2026, 2, 17, 14, 0, 0, 0, time.UTC)
		got := DeliveryAckLabelSequence("gastown/workerB", at, existing)
		want := []string{
			"delivery-acked-by:gastown/workerB",
			"delivery-acked-at:2026-02-17T14:00:00Z",
			"delivery:acked",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("mixed labels after crash: B must not reuse A's timestamp", func(t *testing.T) {
		// Scenario: A acked fully, then B started acking but crashed after
		// writing acked-by:B (before acked-at). Labels accumulated:
		existing := []string{
			"delivery:pending",
			"delivery-acked-by:gastown/workerA",
			"delivery-acked-at:2026-02-17T12:00:00Z",
			"delivery:acked",
			"delivery-acked-by:gastown/workerB",
		}
		// B retries — must generate a fresh timestamp, not reuse A's t1.
		at := time.Date(2026, 2, 17, 14, 0, 0, 0, time.UTC)
		got := DeliveryAckLabelSequence("gastown/workerB", at, existing)
		want := []string{
			"delivery-acked-by:gastown/workerB",
			"delivery-acked-at:2026-02-17T14:00:00Z",
			"delivery:acked",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
}

func TestWakeSourceLabelsRequireValidIdentity(t *testing.T) {
	const want = "msg-0123456789abcdef"
	if got, err := wakeSourceIDFromLabels([]string{"gt:message", WakeSourceLabelPrefix + want}); err != nil || got != want {
		t.Fatalf("valid source = %q, %v; want %q", got, err, want)
	}
	for _, labels := range [][]string{
		{"gt:message"},
		{WakeSourceLabelPrefix + "../escape"},
		{WakeSourceLabelPrefix + want, WakeSourceLabelPrefix + "msg-fedcba9876543210"},
	} {
		if _, err := wakeSourceIDFromLabels(labels); err == nil {
			t.Fatalf("wakeSourceIDFromLabels(%q) accepted invalid labels", labels)
		}
	}
}

func TestFindWakeSourceMessageFailsClosedOnDuplicates(t *testing.T) {
	const sourceID = "msg-0123456789abcdef"
	one := &Message{ID: "hq-one", ThreadID: "hq-incident", WakeSourceID: sourceID}
	found, err := findWakeSourceMessage([]*Message{one}, sourceID)
	if err != nil || found != one {
		t.Fatalf("single source = %#v, %v", found, err)
	}
	if _, err := findWakeSourceMessage([]*Message{one, {
		ID: "hq-two", ThreadID: "hq-incident", WakeSourceID: sourceID,
	}}, sourceID); err == nil {
		t.Fatal("duplicate durable source should fail closed")
	}
}

func TestEnsureDirectMessageBySourceConcurrent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell bd stub is POSIX-only")
	}
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, ".gt-types-configured"), []byte(beads.TypeConfigSentinelValue()+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "messages.json")
	countPath := filepath.Join(t.TempDir(), "creates")
	binDir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
  list)
    if [ -s "$MAIL_SOURCE_STATE" ]; then cat "$MAIL_SOURCE_STATE"; else printf '[]\n'; fi
    ;;
  create)
    printf 'create\n' >> "$MAIL_SOURCE_COUNT"
    printf '%s\n' '[{"id":"hq-mail-one","title":"[HIGH] incident","description":"body","assignee":"mayor/","priority":1,"status":"open","labels":["gt:message","gt:escalation","from:deacon/dogs/bravo","msg-type:escalation","wake-source:msg-0123456789abcdef","thread:hq-wisp-incident","delivery:pending"]}]' > "$MAIL_SOURCE_STATE"
    printf '%s\n' '{"id":"hq-mail-one"}'
    ;;
  *) printf '[]\n' ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MAIL_SOURCE_STATE", statePath)
	t.Setenv("MAIL_SOURCE_COUNT", countPath)

	router := NewRouterWithTownRoot(townRoot, townRoot)
	router.startPoller = nil
	const workers = 20
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := router.EnsureDirectMessageBySource(&Message{
				From: "deacon/dogs/bravo", To: "mayor/", Subject: "[HIGH] incident", Body: "body",
				Type: TypeEscalation, ThreadID: "hq-wisp-incident",
				WakeSourceID: "msg-0123456789abcdef",
			})
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "create\n") != 1 {
		t.Fatalf("durable creates = %q, want exactly one", data)
	}
	if pending, err := nudge.Pending(townRoot, "hq-mayor"); err != nil || pending != 1 {
		t.Fatalf("queued source wakes = %d, %v; want 1", pending, err)
	}
}

func TestQueuedWakeTerminalSourceIsNotInjected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake bd is POSIX-only")
	}
	const (
		threadID = "thread-terminal"
		sourceID = "msg-0123456789abcdef"
		session  = "gt-test-terminal-source"
	)
	sourceRecord := BeadsMessage{
		ID:        "hq-source",
		Title:     "source",
		Assignee:  "gastown/Toast",
		Status:    "open",
		CreatedAt: time.Now(),
		Labels: []string{
			"gt:message", "thread:" + threadID, "from:mayor/", "read",
			DeliveryLabelPending, WakeSourceLabelPrefix + sourceID,
		},
	}
	mailbox, _ := newBeadsThreadTestMailbox(t, []BeadsMessage{sourceRecord})
	queued := nudge.QueuedNudge{
		Sender:     "mayor/",
		Message:    "terminal wake",
		Kind:       "mail",
		ThreadID:   threadID,
		SourceID:   sourceID,
		SourceKind: nudge.SourceKindMail,
	}
	if err := nudge.Enqueue(mailbox.workDir, session, queued); err != nil {
		t.Fatalf("Enqueue terminal wake: %v", err)
	}
	unrelated := queued
	unrelated.Message = "unrelated wake"
	unrelated.SourceID = "msg-fedcba9876543210"
	if err := nudge.Enqueue(mailbox.workDir, session, unrelated); err != nil {
		t.Fatalf("Enqueue unrelated wake: %v", err)
	}
	claim, err := nudge.ClaimDue(mailbox.workDir, session)
	if err != nil || claim == nil {
		t.Fatalf("ClaimDue = %#v, %v", claim, err)
	}

	deliver, err := PrepareWakeClaim(mailbox.workDir, claim)
	if err != nil || deliver {
		t.Fatalf("PrepareWakeClaim = %t, %v; want terminal suppression", deliver, err)
	}
	remaining, err := nudge.ListQueued(mailbox.workDir, session)
	if err != nil {
		t.Fatalf("ListQueued: %v", err)
	}
	if len(remaining) != 1 || remaining[0].SourceID != unrelated.SourceID {
		t.Fatalf("remaining queue = %#v, want only unrelated source", remaining)
	}
}

func TestQueuedWakeActiveSourceAndReadReminderStayEligible(t *testing.T) {
	const sourceID = "msg-0123456789abcdef"
	source := &Message{
		ID:            "hq-source",
		ThreadID:      "thread-active",
		WakeSourceID:  sourceID,
		Type:          TypeNotification,
		Status:        WorkStateOpen,
		DeliveryState: DeliveryStatePending,
	}
	tests := []struct {
		name string
		kind string
		read bool
	}{
		{name: "active mail", kind: "mail"},
		{name: "read reminder awaiting reply", kind: "reply-reminder", read: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := *source
			candidate.Read = tt.read
			queued := nudge.QueuedNudge{
				Kind:       tt.kind,
				ThreadID:   candidate.ThreadID,
				SourceID:   sourceID,
				SourceKind: nudge.SourceKindMail,
			}
			got, err := classifyWakeSource(queued, &candidate, []*Message{&candidate})
			if err != nil || got != WakeEligible {
				t.Fatalf("classifyWakeSource = %q, %v; want %q", got, err, WakeEligible)
			}
		})
	}
}

func TestQueuedWakeEligibilityFailureRemainsRetryable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake bd is POSIX-only")
	}
	mailbox, _ := newBeadsThreadTestMailbox(t, nil)
	const session = "gt-test-source-unknown"
	if err := nudge.Enqueue(mailbox.workDir, session, nudge.QueuedNudge{
		Kind:       "mail",
		ThreadID:   "thread-missing",
		SourceID:   "msg-0123456789abcdef",
		SourceKind: nudge.SourceKindMail,
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claim, err := nudge.ClaimDue(mailbox.workDir, session)
	if err != nil || claim == nil {
		t.Fatalf("ClaimDue = %#v, %v", claim, err)
	}

	deliver, err := PrepareWakeClaim(mailbox.workDir, claim)
	if err == nil || deliver {
		t.Fatalf("PrepareWakeClaim = %t, %v; want retryable failure", deliver, err)
	}
	queued, err := nudge.ListQueued(mailbox.workDir, session)
	if err != nil {
		t.Fatalf("ListQueued: %v", err)
	}
	if len(queued) != 1 || queued[0].Attempts != 1 || queued[0].LastErrorCode != "wake-source-unknown" {
		t.Fatalf("retry queue = %#v, want one unknown-source retry", queued)
	}
}

func TestDeliveryAckLabelsToWriteSkipsExistingLabels(t *testing.T) {
	at := time.Date(2026, 2, 17, 14, 0, 0, 0, time.UTC)

	t.Run("partial retry only writes missing ack label", func(t *testing.T) {
		existing := []string{
			"delivery:pending",
			"delivery-acked-by:gastown/worker",
			"delivery-acked-at:2026-02-17T12:00:00Z",
		}
		got := deliveryAckLabelsToWrite("gastown/worker", at, existing)
		want := []string{"delivery:acked"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("complete retry writes nothing", func(t *testing.T) {
		existing := []string{
			"delivery:pending",
			"delivery-acked-by:gastown/worker",
			"delivery-acked-at:2026-02-17T12:00:00Z",
			"delivery:acked",
		}
		got := deliveryAckLabelsToWrite("gastown/worker", at, existing)
		if len(got) != 0 {
			t.Fatalf("got %v, want no labels", got)
		}
	})
}

func TestDeliveryPendingRemovalNeeded(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"no delivery labels", []string{"gt:message"}, false},
		{"pending only", []string{DeliveryLabelPending}, false},
		{"partial ack keeps pending", []string{DeliveryLabelPending, "delivery-acked-by:gastown/worker", "delivery-acked-at:2026-02-17T12:00:00Z"}, false},
		{"pending and acked converges", []string{DeliveryLabelPending, DeliveryLabelAcked}, true},
		{"acked only", []string{DeliveryLabelAcked}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deliveryPendingRemovalNeeded(tt.labels); got != tt.want {
				t.Fatalf("deliveryPendingRemovalNeeded(%v) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}

func TestAcknowledgeDeliveryBeadConvergesPendingLabel(t *testing.T) {
	tmp := t.TempDir()
	labelsPath := filepath.Join(tmp, "labels.txt")
	logPath := filepath.Join(tmp, "bd.log")
	initialLabels := strings.Join([]string{
		DeliveryLabelPending,
		"delivery-acked-by:gastown/worker",
		"delivery-acked-at:2026-02-17T12:00:00Z",
		DeliveryLabelAcked,
	}, "\n") + "\n"
	if err := os.WriteFile(labelsPath, []byte(initialLabels), 0644); err != nil {
		t.Fatalf("write labels: %v", err)
	}

	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	bdStub := filepath.Join(binDir, "bd")
	script := `#!/usr/bin/env bash
set -euo pipefail
labels_file="$BD_STUB_LABELS"
log_file="$BD_STUB_LOG"
printf '%s\n' "$*" >> "$log_file"

if [[ "${1:-}" == "show" ]]; then
  id="${2:-}"
  printf '[{"id":"%s","labels":[' "$id"
  first=1
  while IFS= read -r label; do
    [[ -z "$label" ]] && continue
    if [[ $first -eq 0 ]]; then printf ','; fi
    first=0
    printf '"%s"' "$label"
  done < "$labels_file"
  printf ']}]'
  exit 0
fi

if [[ "${1:-}" == "label" && "${2:-}" == "add" ]]; then
  label="${4:-}"
  found=0
  while IFS= read -r existing; do
    if [[ "$existing" == "$label" ]]; then
      found=1
      break
    fi
  done < "$labels_file"
  if [[ $found -eq 0 ]]; then
    printf '%s\n' "$label" >> "$labels_file"
  fi
  exit 0
fi

if [[ "${1:-}" == "label" && "${2:-}" == "remove" ]]; then
  label="${4:-}"
  tmp_file="$labels_file.tmp"
  removed=0
  : > "$tmp_file"
  while IFS= read -r existing; do
    if [[ "$existing" == "$label" ]]; then
      removed=1
      continue
    fi
    printf '%s\n' "$existing" >> "$tmp_file"
  done < "$labels_file"
  mv "$tmp_file" "$labels_file"
  if [[ $removed -eq 0 ]]; then
    echo "does not have label" >&2
    exit 1
  fi
  exit 0
fi

echo "unsupported bd args: $*" >&2
exit 1
`
	if err := os.WriteFile(bdStub, []byte(script), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_STUB_LABELS", labelsPath)
	t.Setenv("BD_STUB_LOG", logPath)

	if err := AcknowledgeDeliveryBead(tmp, "", "msg-1", "gastown/worker"); err != nil {
		t.Fatalf("AcknowledgeDeliveryBead: %v", err)
	}

	data, err := os.ReadFile(labelsPath)
	if err != nil {
		t.Fatalf("read labels: %v", err)
	}
	labels := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, label := range labels {
		if label == DeliveryLabelPending {
			t.Fatalf("%s should have been removed after ack convergence; labels=%v", DeliveryLabelPending, labels)
		}
	}
	if !containsDeliveryTestLabel(labels, DeliveryLabelAcked) {
		t.Fatalf("%s should remain; labels=%v", DeliveryLabelAcked, labels)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(logData), "label remove msg-1 "+DeliveryLabelPending) {
		t.Fatalf("expected pending label removal command, log:\n%s", logData)
	}
}

func containsDeliveryTestLabel(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
