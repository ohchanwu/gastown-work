package nudge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEnqueueAndDrain(t *testing.T) {
	townRoot := t.TempDir()

	session := "gt-gastown-crew-sean"
	n1 := QueuedNudge{
		Sender:   "mayor",
		Message:  "Check your hook",
		Priority: PriorityNormal,
	}
	n2 := QueuedNudge{
		Sender:   "gastown/witness",
		Message:  "Polecat alpha is stuck",
		Priority: PriorityUrgent,
	}

	// Enqueue two nudges
	if err := Enqueue(townRoot, session, n1); err != nil {
		t.Fatalf("Enqueue n1: %v", err)
	}
	// Small delay to ensure different timestamps
	time.Sleep(time.Millisecond)
	if err := Enqueue(townRoot, session, n2); err != nil {
		t.Fatalf("Enqueue n2: %v", err)
	}

	// Check pending count
	count, err := Pending(townRoot, session)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if count != 2 {
		t.Errorf("Pending = %d, want 2", count)
	}

	// Drain
	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 2 {
		t.Fatalf("Drain returned %d nudges, want 2", len(nudges))
	}

	// Verify FIFO order
	if nudges[0].Sender != "mayor" {
		t.Errorf("nudges[0].Sender = %q, want %q", nudges[0].Sender, "mayor")
	}
	if nudges[1].Sender != "gastown/witness" {
		t.Errorf("nudges[1].Sender = %q, want %q", nudges[1].Sender, "gastown/witness")
	}

	// After drain, pending should be 0
	count, err = Pending(townRoot, session)
	if err != nil {
		t.Fatalf("Pending after drain: %v", err)
	}
	if count != 0 {
		t.Errorf("Pending after drain = %d, want 0", count)
	}
}

func TestEnqueueUniqueBySourceConcurrent(t *testing.T) {
	townRoot := t.TempDir()
	sessionID := "gt-gastown-crew-sean"
	reminder := QueuedNudge{
		Sender:     "system",
		Message:    "reply required",
		Kind:       "reply-reminder",
		ThreadID:   "thread-actionable",
		SourceID:   "msg-0123456789abcdef",
		SourceKind: SourceKindMail,
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := EnqueueUniqueBySource(townRoot, sessionID, reminder); err != nil {
				t.Errorf("EnqueueUniqueBySource: %v", err)
			}
		}()
	}
	wg.Wait()

	queued, err := ListQueued(townRoot, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 {
		t.Fatalf("queued %d reminders, want 1", len(queued))
	}
}

func TestEnqueueUniqueBySourceUpgradesExpiringMatch(t *testing.T) {
	const sessionID = "gt-test-durable-source"
	for _, claimed := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued", true: "claimed"}[claimed], func(t *testing.T) {
			townRoot := t.TempDir()
			expiring := QueuedNudge{
				DeliveryID: "ndg-expiring", Sender: "mayor", Message: "read the mail",
				Priority: PriorityNormal, Kind: "mail", ThreadID: "thread-actionable",
				SourceID: "msg-0123456789abcdef", SourceKind: SourceKindMail,
			}
			if err := Enqueue(townRoot, sessionID, expiring); err != nil {
				t.Fatal(err)
			}
			var claim *ClaimedNudge
			if claimed {
				var err error
				claim, err = ClaimDue(townRoot, sessionID)
				if err != nil || claim == nil {
					t.Fatalf("ClaimDue = %#v, %v", claim, err)
				}
			}

			durable := expiring
			durable.DurableUntilAck = true
			added, err := EnqueueUniqueBySource(townRoot, sessionID, durable)
			if err != nil || added {
				t.Fatalf("EnqueueUniqueBySource = %t, %v; want upgraded existing record", added, err)
			}
			if claim != nil {
				if err := claim.Nack("retry", time.Now()); err != nil {
					t.Fatalf("Nack upgraded claim: %v", err)
				}
			}
			queued, err := ListQueued(townRoot, sessionID)
			if err != nil || len(queued) != 1 {
				t.Fatalf("ListQueued = %#v, %v", queued, err)
			}
			if !queued[0].DurableUntilAck || !queued[0].ExpiresAt.IsZero() {
				t.Fatalf("matching record remained expiring: %#v", queued[0])
			}
		})
	}
}

func TestQueuedNudgeSourceJSONCompatibility(t *testing.T) {
	var legacy QueuedNudge
	if err := json.Unmarshal([]byte(`{"sender":"mayor","message":"legacy","priority":"normal"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.SourceID != "" || legacy.SourceKind != "" {
		t.Fatalf("legacy source = %q/%q, want empty", legacy.SourceKind, legacy.SourceID)
	}

	want := QueuedNudge{SourceID: "msg-0123456789abcdef", SourceKind: SourceKindMail}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got QueuedNudge
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.SourceID != want.SourceID || got.SourceKind != want.SourceKind {
		t.Fatalf("round trip source = %q/%q, want %q/%q", got.SourceKind, got.SourceID, want.SourceKind, want.SourceID)
	}
}

func TestEnqueueTightensLegacyQueuePermissions(t *testing.T) {
	townRoot := t.TempDir()
	const session = "gt-test-private-queue"
	dir := queueDir(townRoot, session)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := Enqueue(townRoot, session, QueuedNudge{Sender: "mayor", Message: "private"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("queue dir mode = %o, want 0700", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("queue entries = %v, %v", entries, err)
	}
	info, err = entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("queue record mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestHasQueuedOrClaimedKeepsOrphanRecoveryVisible(t *testing.T) {
	townRoot := t.TempDir()
	const session = "gt-test-claimed-recovery"
	if err := Enqueue(townRoot, session, QueuedNudge{DeliveryID: "ndg-claimed", Sender: "mayor", Message: "recover me"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claim, err := ClaimDue(townRoot, session)
	if err != nil || claim == nil {
		t.Fatalf("ClaimDue = %#v, %v", claim, err)
	}
	if !claim.HasRecoverableState() {
		t.Fatal("private valid claim was not recoverable")
	}
	if err := os.Chmod(claim.claimPath, 0644); err != nil {
		t.Fatal(err)
	}
	if claim.HasRecoverableState() {
		t.Fatal("public claim mode was accepted as recoverable custody")
	}
	if err := os.Chmod(claim.claimPath, 0600); err != nil {
		t.Fatal(err)
	}
	if pending, err := Pending(townRoot, session); err != nil || pending != 0 {
		t.Fatalf("Pending claimed record = %d, %v", pending, err)
	}
	visible, err := HasQueuedOrClaimed(townRoot, session)
	if err != nil || !visible {
		t.Fatalf("HasQueuedOrClaimed = %v, %v; want claimed recovery visible", visible, err)
	}
}

func TestListQueuedReadsWithoutConsuming(t *testing.T) {
	townRoot := t.TempDir()
	session := "hq-mayor"
	queuedAt := time.Now().Add(-time.Minute)
	want := QueuedNudge{
		DeliveryID: "ndg-health", Priority: PriorityUrgent, Timestamp: queuedAt,
		LastErrorCode: "submission-unconfirmed", Message: "private message",
	}
	if err := Enqueue(townRoot, session, want); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	got, err := ListQueued(townRoot, session)
	if err != nil {
		t.Fatalf("ListQueued: %v", err)
	}
	if len(got) != 1 || got[0].DeliveryID != want.DeliveryID || got[0].LastErrorCode != want.LastErrorCode {
		t.Fatalf("ListQueued = %#v", got)
	}
	if pending, err := Pending(townRoot, session); err != nil || pending != 1 {
		t.Fatalf("Pending after ListQueued = %d, %v; want 1", pending, err)
	}
}

func TestListQueuedIncludesClaimWithoutConsumingIt(t *testing.T) {
	townRoot := t.TempDir()
	session := "hq-mayor"
	if err := Enqueue(townRoot, session, QueuedNudge{Priority: PriorityUrgent, Message: "private"}); err != nil {
		t.Fatal(err)
	}
	claim, err := ClaimDue(townRoot, session)
	if err != nil || claim == nil {
		t.Fatalf("ClaimDue = %#v, %v", claim, err)
	}

	got, err := ListQueued(townRoot, session)
	if err != nil || len(got) != 1 || got[0].DeliveryID != claim.Nudge.DeliveryID {
		t.Fatalf("ListQueued = %#v, %v", got, err)
	}
	if err := claim.Nack("test", time.Now()); err != nil {
		t.Fatalf("claim was consumed by ListQueued: %v", err)
	}
}

func TestListQueuedSkipsRecordAckedAfterReadDir(t *testing.T) {
	townRoot := t.TempDir()
	session := "hq-mayor"
	if err := Enqueue(townRoot, session, QueuedNudge{Priority: PriorityUrgent}); err != nil {
		t.Fatal(err)
	}

	oldRead := readQueueRecord
	readQueueRecord = func(path string) ([]byte, error) {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() { readQueueRecord = oldRead })

	got, err := ListQueued(townRoot, session)
	if err != nil || len(got) != 0 {
		t.Fatalf("ListQueued = %#v, %v; want empty snapshot", got, err)
	}
}

func TestClaimDueRequiresMatchingPostClaimReceipt(t *testing.T) {
	townRoot := t.TempDir()
	const session = "gt-test-claim"

	if err := Enqueue(townRoot, session, QueuedNudge{Sender: "mayor", Message: "wake"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	claim, err := ClaimDue(townRoot, session)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if claim == nil || claim.Nudge.Message != "wake" {
		t.Fatalf("ClaimDue = %#v, want wake nudge", claim)
	}
	if claim.Nudge.Session != session || claim.Nudge.DeliveryID == "" || claim.Nudge.Attempts != 1 || claim.Nudge.ClaimedAt.IsZero() {
		t.Fatalf("claim metadata = %#v", claim.Nudge)
	}
	info, err := os.Stat(claim.claimPath)
	if err != nil {
		t.Fatalf("Stat claim: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("claim mode = %o, want 0600", info.Mode().Perm())
	}
	if err := claim.AckSubmitted(SubmissionReceipt{
		Session: session, DeliveryID: claim.Nudge.DeliveryID, SubmittedAt: time.Now(),
	}); err == nil {
		t.Fatal("AckSubmitted accepted submitted=false receipt")
	}

	bad := SubmissionReceipt{
		Session:     "other-session",
		DeliveryID:  claim.Nudge.DeliveryID,
		Submitted:   true,
		SubmittedAt: time.Now(),
	}
	if err := claim.AckSubmitted(bad); err == nil {
		t.Fatal("AckSubmitted accepted a receipt for another session")
	}
	withoutRuntime := SubmissionReceipt{
		Session: session, DeliveryID: claim.Nudge.DeliveryID,
		Submitted: true, SubmittedAt: time.Now(),
	}
	if err := claim.AckSubmitted(withoutRuntime); err == nil {
		t.Fatal("AckSubmitted accepted a receipt without runtime identity")
	}

	receipt := SubmissionReceipt{
		Session:     session,
		DeliveryID:  claim.Nudge.DeliveryID,
		Runtime:     "codex",
		Typed:       true,
		Submitted:   true,
		SubmittedAt: time.Now(),
	}
	if err := claim.AckSubmitted(receipt); err != nil {
		t.Fatalf("AckSubmitted: %v", err)
	}
	if entries, err := os.ReadDir(queueDir(townRoot, session)); err != nil || len(entries) != 0 {
		t.Fatalf("queue after AckSubmitted = %v, %v; want empty", entries, err)
	}
}

func TestAckSubmittedRejectsEqualClaimTimestamp(t *testing.T) {
	townRoot := t.TempDir()
	const session = "gt-test-equal-claim"
	if err := Enqueue(townRoot, session, QueuedNudge{Sender: "mayor", Message: "wake"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claim, err := ClaimDue(townRoot, session)
	if err != nil || claim == nil {
		t.Fatalf("ClaimDue() = %#v, %v", claim, err)
	}
	receipt := SubmissionReceipt{
		Session: claim.Nudge.Session, DeliveryID: claim.Nudge.DeliveryID, Runtime: "codex",
		Typed: true, Submitted: true, SubmittedAt: claim.Nudge.ClaimedAt,
	}
	if err := claim.AckSubmitted(receipt); err == nil {
		t.Fatal("AckSubmitted accepted a receipt at the exact claim timestamp")
	}
	if _, err := os.Stat(claim.claimPath); err != nil {
		t.Fatalf("equal-timestamp rejection removed claim: %v", err)
	}
}

func TestNackSanitizesErrorAndDefersRetry(t *testing.T) {
	townRoot := t.TempDir()
	const session = "gt-test-nack"
	if err := Enqueue(townRoot, session, QueuedNudge{Sender: "mayor", Message: "wake"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claim, err := ClaimDue(townRoot, session)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	next := time.Now().Add(time.Minute)
	if err := claim.Nack("Transport Failed: private/path", next); err != nil {
		t.Fatalf("Nack: %v", err)
	}
	if got, err := ClaimDue(townRoot, session); err != nil || got != nil {
		t.Fatalf("ClaimDue before retry = %#v, %v; want nil", got, err)
	}

	entries, err := os.ReadDir(queueDir(townRoot, session))
	if err != nil || len(entries) != 1 {
		t.Fatalf("queue entries = %v, %v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(queueDir(townRoot, session), entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var queued QueuedNudge
	if err := json.Unmarshal(data, &queued); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if queued.LastErrorCode != "transport-failed-private-path" || !queued.NextAttempt.Equal(next) || !queued.ClaimedAt.IsZero() {
		t.Fatalf("nacked metadata = %#v", queued)
	}
}

func TestDrainEmptyQueue(t *testing.T) {
	townRoot := t.TempDir()

	nudges, err := Drain(townRoot, "nonexistent-session")
	if err != nil {
		t.Fatalf("Drain empty: %v", err)
	}
	if len(nudges) != 0 {
		t.Errorf("Drain empty returned %d nudges, want 0", len(nudges))
	}
}

func TestClaimDuePreservesMalformedUrgentRecord(t *testing.T) {
	townRoot := t.TempDir()
	const session = "gt-test-malformed-urgent"

	dir := queueDir(townRoot, session)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "100.json")
	const malformed = `{"priority":"urgent","message":"private-marker"`
	if err := os.WriteFile(path, []byte(malformed), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(townRoot, session, QueuedNudge{Sender: "mayor", Message: "valid-after-malformed", Priority: PriorityUrgent}); err != nil {
		t.Fatalf("Enqueue valid record: %v", err)
	}

	claim, err := ClaimDue(townRoot, session)
	if claim != nil || err == nil {
		t.Fatalf("ClaimDue() = %#v, %v; want nil sanitized error", claim, err)
	}
	if strings.Contains(err.Error(), "private-marker") || strings.Contains(err.Error(), session) || strings.Contains(err.Error(), path) {
		t.Fatalf("ClaimDue() leaked private queue data: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("malformed FIFO path still blocks queue: %v", statErr)
	}
	entries, readDirErr := os.ReadDir(dir)
	if readDirErr != nil {
		t.Fatal(readDirErr)
	}
	var quarantinePath string
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".malformed.") && !strings.HasSuffix(entry.Name(), ".json") {
			quarantinePath = filepath.Join(dir, entry.Name())
			break
		}
	}
	if quarantinePath == "" {
		t.Fatalf("queue entries = %v, want unique non-json quarantine", entries)
	}
	data, readErr := os.ReadFile(quarantinePath)
	info, statErr := os.Stat(quarantinePath)
	if readErr != nil || string(data) != malformed || statErr != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("quarantine = %q, mode=%v, read=%v, stat=%v; want exact mode-0600 bytes", data, info, readErr, statErr)
	}
	next, nextErr := ClaimDue(townRoot, session)
	if nextErr != nil || next == nil || next.Nudge.Message != "valid-after-malformed" {
		t.Fatalf("ClaimDue after quarantine = %#v, %v; want following valid record", next, nextErr)
	}
}

func TestFormatForInjection_Normal(t *testing.T) {
	nudges := []QueuedNudge{
		{Sender: "mayor", Message: "Check status", Priority: PriorityNormal},
	}
	output := FormatForInjection(nudges)

	if output == "" {
		t.Fatal("FormatForInjection returned empty string")
	}
	if !strings.Contains(output, "<system-reminder>") {
		t.Error("missing <system-reminder> tag")
	}
	if !strings.Contains(output, "background notification") {
		t.Error("normal nudges should mention background notification")
	}
	if strings.Contains(output, "URGENT") {
		t.Error("normal nudges should not contain URGENT")
	}
}

func TestFormatForInjection_Urgent(t *testing.T) {
	nudges := []QueuedNudge{
		{Sender: "witness", Message: "Polecat stuck", Priority: PriorityUrgent},
		{Sender: "mayor", Message: "FYI", Priority: PriorityNormal},
	}
	output := FormatForInjection(nudges)

	if !strings.Contains(output, "URGENT") {
		t.Error("should mention URGENT for urgent nudges")
	}
	if !strings.Contains(output, "Handle urgent") {
		t.Error("should instruct agent to handle urgent nudges")
	}
	if !strings.Contains(output, "non-urgent") {
		t.Error("should mention non-urgent nudges")
	}
}

func TestFormatForInjection_Empty(t *testing.T) {
	output := FormatForInjection(nil)
	if output != "" {
		t.Errorf("FormatForInjection(nil) = %q, want empty", output)
	}
}

func TestPendingNonexistentDir(t *testing.T) {
	count, err := Pending("/nonexistent/path", "session")
	if err != nil {
		t.Fatalf("Pending on nonexistent dir should not error: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
}

func TestEnqueueDefaults(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-defaults"

	// Enqueue with zero timestamp and empty priority — should get defaults
	n := QueuedNudge{
		Sender:  "test",
		Message: "hello",
	}
	if err := Enqueue(townRoot, session, n); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 1 {
		t.Fatalf("got %d nudges, want 1", len(nudges))
	}
	if nudges[0].Priority != PriorityNormal {
		t.Errorf("Priority = %q, want %q", nudges[0].Priority, PriorityNormal)
	}
	if nudges[0].Timestamp.IsZero() {
		t.Error("Timestamp should have been set to non-zero default")
	}
	if nudges[0].ExpiresAt.IsZero() {
		t.Error("ExpiresAt should have been set to non-zero default")
	}
	// Normal priority should get DefaultNormalTTL
	expectedExpiry := nudges[0].Timestamp.Add(DefaultNormalTTL)
	if !nudges[0].ExpiresAt.Equal(expectedExpiry) {
		t.Errorf("ExpiresAt = %v, want %v (Timestamp + DefaultNormalTTL)", nudges[0].ExpiresAt, expectedExpiry)
	}
}

func TestEnqueueUrgentTTL(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-urgent-ttl"

	n := QueuedNudge{
		Sender:   "test",
		Message:  "urgent message",
		Priority: PriorityUrgent,
	}
	if err := Enqueue(townRoot, session, n); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 1 {
		t.Fatalf("got %d nudges, want 1", len(nudges))
	}
	// Urgent priority should get DefaultUrgentTTL
	expectedExpiry := nudges[0].Timestamp.Add(DefaultUrgentTTL)
	if !nudges[0].ExpiresAt.Equal(expectedExpiry) {
		t.Errorf("ExpiresAt = %v, want %v (Timestamp + DefaultUrgentTTL)", nudges[0].ExpiresAt, expectedExpiry)
	}
}

func TestEnqueueCustomExpiry(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-custom-expiry"

	customExpiry := time.Now().Add(5 * time.Minute)
	n := QueuedNudge{
		Sender:    "test",
		Message:   "custom expiry",
		ExpiresAt: customExpiry,
	}
	if err := Enqueue(townRoot, session, n); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 1 {
		t.Fatalf("got %d nudges, want 1", len(nudges))
	}
	// Custom expiry should be preserved, not overwritten by default TTL
	if !nudges[0].ExpiresAt.Equal(customExpiry) {
		t.Errorf("ExpiresAt = %v, want %v (custom)", nudges[0].ExpiresAt, customExpiry)
	}
}

func TestDrainSkipsExpired(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-expired"

	// Enqueue an already-expired nudge
	expired := QueuedNudge{
		Sender:    "old-sender",
		Message:   "stale message",
		Timestamp: time.Now().Add(-time.Hour),
		ExpiresAt: time.Now().Add(-30 * time.Minute), // expired 30 min ago
	}
	if err := Enqueue(townRoot, session, expired); err != nil {
		t.Fatalf("Enqueue expired: %v", err)
	}

	// Enqueue a fresh nudge
	time.Sleep(time.Millisecond)
	fresh := QueuedNudge{
		Sender:  "new-sender",
		Message: "fresh message",
	}
	if err := Enqueue(townRoot, session, fresh); err != nil {
		t.Fatalf("Enqueue fresh: %v", err)
	}

	// Pending counts both (doesn't check expiry)
	pending, err := Pending(townRoot, session)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending != 2 {
		t.Errorf("Pending = %d, want 2 (counts all files)", pending)
	}

	// Drain should skip the expired nudge
	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 1 {
		t.Fatalf("Drain returned %d nudges, want 1 (expired should be skipped)", len(nudges))
	}
	if nudges[0].Sender != "new-sender" {
		t.Errorf("got sender %q, want %q", nudges[0].Sender, "new-sender")
	}

	// After drain, queue dir should be empty (both files removed)
	dir := filepath.Join(townRoot, ".runtime", "nudge_queue", session)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("queue dir should be empty after drain, got %d entries", len(entries))
	}
}

func TestEnqueueQueueDepthLimit(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-depth"

	// Fill the queue to MaxQueueDepth
	for i := 0; i < MaxQueueDepth; i++ {
		n := QueuedNudge{
			Sender:  "sender",
			Message: "msg",
		}
		if err := Enqueue(townRoot, session, n); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	// Next enqueue should fail
	overflow := QueuedNudge{
		Sender:  "sender",
		Message: "overflow",
	}
	err := Enqueue(townRoot, session, overflow)
	if err == nil {
		t.Fatal("expected error when queue is full")
	}
	if !strings.Contains(err.Error(), "is full") {
		t.Errorf("got error %q, want to contain 'is full'", err.Error())
	}

	// Verify pending count is at max
	pending, _ := Pending(townRoot, session)
	if pending != MaxQueueDepth {
		t.Errorf("Pending = %d, want %d", pending, MaxQueueDepth)
	}

	// After draining, enqueue should work again
	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != MaxQueueDepth {
		t.Errorf("Drain returned %d, want %d", len(nudges), MaxQueueDepth)
	}

	err = Enqueue(townRoot, session, overflow)
	if err != nil {
		t.Errorf("Enqueue after drain should succeed: %v", err)
	}
}

func TestDrainSweepsOrphanedClaims(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-orphans"

	dir := filepath.Join(townRoot, ".runtime", "nudge_queue", session)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create an orphaned .claimed file with old mod time
	// Claim files now use the format: <original>.json.claimed.<suffix>
	orphanPath := filepath.Join(dir, "100.json.claimed.deadbeef")
	if err := os.WriteFile(orphanPath, []byte(`{"sender":"ghost"}`), 0644); err != nil {
		t.Fatal(err)
	}
	// Set mod time to well past the stale threshold
	oldTime := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(orphanPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	// Create a fresh .claimed file (should NOT be swept)
	freshClaimPath := filepath.Join(dir, "200.json.claimed.cafebabe")
	if err := os.WriteFile(freshClaimPath, []byte(`{"sender":"active"}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Enqueue a valid nudge
	n := QueuedNudge{Sender: "test", Message: "valid"}
	if err := Enqueue(townRoot, session, n); err != nil {
		t.Fatal(err)
	}

	// First Drain: requeues the orphaned claim (rename .claimed → .json),
	// keeps the fresh claim, and returns the valid nudge.
	// The requeued file isn't in the current ReadDir snapshot, so it's
	// picked up on the next Drain call.
	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 1 {
		t.Fatalf("first Drain got %d nudges, want 1", len(nudges))
	}
	if nudges[0].Message != "valid" {
		t.Errorf("got message %q, want %q", nudges[0].Message, "valid")
	}

	// The orphaned .claimed file should have been requeued as .json
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Error("orphaned .claimed file should no longer exist (requeued to .json)")
	}
	// Restored path strips everything from ".claimed" onward
	restoredPath := filepath.Join(dir, "100.json")
	if _, err := os.Stat(restoredPath); os.IsNotExist(err) {
		t.Error("restored .json file should exist after requeue")
	}

	// Second Drain: picks up the requeued orphan
	nudges2, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("second Drain: %v", err)
	}
	if len(nudges2) != 1 {
		t.Fatalf("second Drain got %d nudges, want 1 (the requeued orphan)", len(nudges2))
	}
	if nudges2[0].Sender != "ghost" {
		t.Errorf("got sender %q, want %q", nudges2[0].Sender, "ghost")
	}

	// The fresh claim should still exist (not old enough to sweep)
	if _, err := os.Stat(freshClaimPath); os.IsNotExist(err) {
		t.Error("fresh .claimed file should NOT have been swept")
	}
}

func TestConcurrentEnqueueNoDuplicateLoss(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-concurrent"

	// Fire 20 concurrent enqueues — all should succeed without collision.
	const count = 20
	var wg sync.WaitGroup
	errs := make(chan error, count)

	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n := QueuedNudge{
				Sender:  "sender",
				Message: strings.Repeat("x", i+1), // unique per goroutine
			}
			if err := Enqueue(townRoot, session, n); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent Enqueue failed: %v", err)
	}

	// All 20 should be pending
	pending, err := Pending(townRoot, session)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending != count {
		t.Errorf("Pending = %d, want %d (some nudges lost to collision?)", pending, count)
	}

	// Drain should return all 20
	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != count {
		t.Errorf("Drain returned %d, want %d", len(nudges), count)
	}
}

// --- DeliverAfter tests ---

// TestDrainSkipsDeferredNudge verifies that a nudge with a future DeliverAfter
// is not returned by Drain and remains in the queue.
func TestDrainSkipsDeferredNudge(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-deferred"

	deferred := QueuedNudge{
		Sender:       "system",
		Message:      "reply reminder",
		DeliverAfter: time.Now().Add(10 * time.Second), // far future
	}
	if err := Enqueue(townRoot, session, deferred); err != nil {
		t.Fatalf("Enqueue deferred: %v", err)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 0 {
		t.Fatalf("Drain returned %d nudges, want 0 (deferred not ready)", len(nudges))
	}

	// File should still be in queue (not discarded)
	pending, err := Pending(townRoot, session)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending != 1 {
		t.Errorf("Pending = %d, want 1 (deferred nudge still in queue)", pending)
	}
}

// TestDrainDeliversDeferredNudgeWhenReady verifies that a nudge with a past
// DeliverAfter is delivered normally.
func TestDrainDeliversDeferredNudgeWhenReady(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-deferred-ready"

	ready := QueuedNudge{
		Sender:       "system",
		Message:      "reply reminder",
		DeliverAfter: time.Now().Add(-1 * time.Second), // already past
	}
	if err := Enqueue(townRoot, session, ready); err != nil {
		t.Fatalf("Enqueue ready: %v", err)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 1 {
		t.Fatalf("Drain returned %d nudges, want 1 (deferred is ready)", len(nudges))
	}
	if nudges[0].Message != "reply reminder" {
		t.Errorf("got message %q, want %q", nudges[0].Message, "reply reminder")
	}
}

// TestDrainMixedDeferredAndReady verifies that only ready nudges are returned
// when a mix of deferred and immediately-deliverable nudges are queued.
func TestDrainMixedDeferredAndReady(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-mixed-deferred"

	// Enqueue: immediate, then deferred, then immediate (interleaved order).
	n1 := QueuedNudge{Sender: "mayor", Message: "immediate-1"}
	if err := Enqueue(townRoot, session, n1); err != nil {
		t.Fatalf("Enqueue n1: %v", err)
	}
	time.Sleep(time.Millisecond)

	deferred := QueuedNudge{
		Sender:       "system",
		Message:      "deferred",
		DeliverAfter: time.Now().Add(60 * time.Second),
	}
	if err := Enqueue(townRoot, session, deferred); err != nil {
		t.Fatalf("Enqueue deferred: %v", err)
	}
	time.Sleep(time.Millisecond)

	n2 := QueuedNudge{Sender: "witness", Message: "immediate-2"}
	if err := Enqueue(townRoot, session, n2); err != nil {
		t.Fatalf("Enqueue n2: %v", err)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 2 {
		t.Fatalf("Drain returned %d nudges, want 2 (deferred stays in queue)", len(nudges))
	}
	if nudges[0].Message != "immediate-1" {
		t.Errorf("nudges[0].Message = %q, want %q", nudges[0].Message, "immediate-1")
	}
	if nudges[1].Message != "immediate-2" {
		t.Errorf("nudges[1].Message = %q, want %q", nudges[1].Message, "immediate-2")
	}

	// Deferred nudge remains in queue
	pending, err := Pending(townRoot, session)
	if err != nil {
		t.Fatalf("Pending after drain: %v", err)
	}
	if pending != 1 {
		t.Errorf("Pending = %d, want 1 (deferred nudge still in queue)", pending)
	}
}

func TestRemoveKindByThread(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-remove"

	keep := QueuedNudge{Sender: "system", Message: "keep", Kind: "mail", ThreadID: "thread-1"}
	removeA := QueuedNudge{Sender: "system", Message: "remove-a", Kind: "reply-reminder", ThreadID: "thread-1"}
	removeB := QueuedNudge{Sender: "system", Message: "remove-b", Kind: "reply-reminder", ThreadID: "thread-1"}
	otherThread := QueuedNudge{Sender: "system", Message: "other-thread", Kind: "reply-reminder", ThreadID: "thread-2"}

	for _, n := range []QueuedNudge{keep, removeA, removeB, otherThread} {
		if err := Enqueue(townRoot, session, n); err != nil {
			t.Fatalf("Enqueue(%q): %v", n.Message, err)
		}
		time.Sleep(time.Millisecond)
	}

	removed, err := RemoveKindByThread(townRoot, session, "reply-reminder", "thread-1")
	if err != nil {
		t.Fatalf("RemoveKindByThread: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 2 {
		t.Fatalf("Drain returned %d nudges, want 2", len(nudges))
	}
	if nudges[0].Message != "keep" {
		t.Fatalf("nudges[0].Message = %q, want %q", nudges[0].Message, "keep")
	}
	if nudges[1].Message != "other-thread" {
		t.Fatalf("nudges[1].Message = %q, want %q", nudges[1].Message, "other-thread")
	}
}

func TestClaimedWakeSelfHealsAfterThreadRemovalRace(t *testing.T) {
	townRoot := t.TempDir()
	const session = "gt-test-claimed-thread-race"
	if err := Enqueue(townRoot, session, QueuedNudge{
		Kind:     "mail",
		ThreadID: "thread-terminal",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claim, err := ClaimDue(townRoot, session)
	if err != nil || claim == nil {
		t.Fatalf("ClaimDue = %#v, %v", claim, err)
	}
	if removed, err := RemoveKindByThread(townRoot, session, "mail", "thread-terminal"); err != nil || removed != 0 {
		t.Fatalf("RemoveKindByThread = %d, %v; want claimed record untouched", removed, err)
	}
	if err := claim.DiscardTerminal(); err != nil {
		t.Fatalf("DiscardTerminal: %v", err)
	}
	visible, err := HasQueuedOrClaimed(townRoot, session)
	if err != nil || visible {
		t.Fatalf("HasQueuedOrClaimed = %t, %v; want converged queue", visible, err)
	}
}

// TestDeferredNudgeDeliveredAfterDelay uses a very short DeliverAfter to confirm
// that the same nudge is skipped on first Drain and delivered on a second Drain
// after the deadline elapses.
func TestDeferredNudgeDeliveredAfterDelay(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-deferred-sequence"

	shortDelay := QueuedNudge{
		Sender:       "system",
		Message:      "reply via mail",
		DeliverAfter: time.Now().Add(50 * time.Millisecond),
	}
	if err := Enqueue(townRoot, session, shortDelay); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// First Drain: not ready yet.
	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("first Drain: %v", err)
	}
	if len(nudges) != 0 {
		t.Fatalf("first Drain: got %d nudges, want 0 (deferred not ready)", len(nudges))
	}

	// Wait for deadline.
	time.Sleep(60 * time.Millisecond)

	// Second Drain: ready now.
	nudges, err = Drain(townRoot, session)
	if err != nil {
		t.Fatalf("second Drain: %v", err)
	}
	if len(nudges) != 1 {
		t.Fatalf("second Drain: got %d nudges, want 1 (deferred now ready)", len(nudges))
	}
	if nudges[0].Message != "reply via mail" {
		t.Errorf("got message %q, want %q", nudges[0].Message, "reply via mail")
	}

	// Queue should now be empty.
	pending, err := Pending(townRoot, session)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending != 0 {
		t.Errorf("Pending = %d, want 0 (deferred nudge delivered)", pending)
	}
}

// TestZeroDeliverAfterIsImmediate verifies that a zero DeliverAfter (unset)
// is treated as immediately deliverable (not deferred).
func TestZeroDeliverAfterIsImmediate(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-zero-deliver-after"

	n := QueuedNudge{
		Sender:  "mayor",
		Message: "no delay",
		// DeliverAfter intentionally left zero
	}
	if err := Enqueue(townRoot, session, n); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	nudges, err := Drain(townRoot, session)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(nudges) != 1 {
		t.Fatalf("got %d nudges, want 1 (zero DeliverAfter = immediate)", len(nudges))
	}
}

func TestConcurrentDrainNoDoubleDeli(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-drain-race"

	// Enqueue 10 nudges
	const count = 10
	for i := 0; i < count; i++ {
		n := QueuedNudge{
			Sender:  "sender",
			Message: strings.Repeat("m", i+1),
		}
		if err := Enqueue(townRoot, session, n); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
		time.Sleep(time.Millisecond) // ensure ordering
	}

	// Race 5 concurrent Drains — total nudges collected should equal count.
	const drainers = 5
	var wg sync.WaitGroup
	results := make(chan []QueuedNudge, drainers)

	for i := 0; i < drainers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			nudges, err := Drain(townRoot, session)
			if err != nil {
				t.Errorf("concurrent Drain: %v", err)
				return
			}
			results <- nudges
		}()
	}
	wg.Wait()
	close(results)

	total := 0
	for nudges := range results {
		total += len(nudges)
	}

	// On Windows, transient sharing violations (antivirus, search indexer)
	// can prevent all concurrent drainers from claiming a file.  The nudge
	// stays as .json and is picked up on the next Drain — mirror that here
	// with a straggler sweep so the test validates no-loss, not one-shot
	// completeness.
	for retries := 0; retries < 3 && total < count; retries++ {
		time.Sleep(50 * time.Millisecond)
		stragglers, err := Drain(townRoot, session)
		if err != nil {
			t.Fatalf("straggler Drain: %v", err)
		}
		total += len(stragglers)
	}

	if total != count {
		t.Errorf("concurrent Drains delivered %d total nudges, want exactly %d (double-delivery or loss)", total, count)
	}

	// Verify no double-delivery: total must be exactly count, not more.
	if total > count {
		t.Errorf("double delivery detected: got %d total nudges, want exactly %d", total, count)
	}
}
