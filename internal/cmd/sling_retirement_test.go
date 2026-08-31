package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/tmux"
	witnesspkg "github.com/steveyegge/gastown/internal/witness"
)

func stubPortableWitnessAuthorityCapture(t *testing.T) {
	t.Helper()
	old := captureLifecycleWitnessAuthorityFn
	captureLifecycleWitnessAuthorityFn = func(string, string) (*witnesspkg.LifecycleWitnessAuthority, error) {
		return portableWitnessAuthorityFixture("fixture"), nil
	}
	t.Cleanup(func() { captureLifecycleWitnessAuthorityFn = old })
}

func portableWitnessAuthorityFixture(identity string) *witnesspkg.LifecycleWitnessAuthority {
	return &witnesspkg.LifecycleWitnessAuthority{
		Session: tmux.SessionGeneration{
			Name: "gt-witness", SessionID: "$1", PaneID: "%1", Nonce: identity + "-generation",
			ServerPID: 1, ServerIdentity: identity + "-server", Transport: tmux.SessionTransport{Bound: true, SocketPath: "/tmp/" + identity + "-tmux.sock"},
		},
		Pane: tmux.PaneProcessGeneration{PID: 2, Identity: identity + "-pane"},
	}
}

func TestCompleteRetirementRecordTerminalizesLegacyDoltIntentWithoutShutdown(t *testing.T) {
	townRoot := t.TempDir()
	record := &slingRetirementRecord{
		AttemptID: "12121212-1212-4212-8212-121212121212", Capability: "34343434-3434-4434-8434-343434343434",
		BeadID: "gt-work", OldAssignee: "gastown/polecats/old", OldIncarnation: "old-gen",
		NewAssignee: "gastown/polecats/new", NewIncarnation: "new-gen", Requester: "mayor/",
		ThreadID: "sling-retirement-12121212-1212-4212-8212-121212121212", State: "pending",
	}
	oldConfigured, oldAccepted, oldValidate, oldSend := slingReceiptDatabaseConfiguredFn, retirementAcceptanceStoredFn, validateRetirementReplacementFn, sendRetirementMessageFn
	slingReceiptDatabaseConfiguredFn = func(string, string) bool { return true }
	retirementAcceptanceStoredFn = func(string, string, *mail.Message, *slingRetirementRecord) (bool, error) { return false, nil }
	validateRetirementReplacementFn = func(string, *slingRetirementRecord) error { return nil }
	shutdownCalls := 0
	sendRetirementMessageFn = func(string, *mail.Message) error {
		shutdownCalls++
		return nil
	}
	t.Cleanup(func() {
		slingReceiptDatabaseConfiguredFn, retirementAcceptanceStoredFn, validateRetirementReplacementFn, sendRetirementMessageFn = oldConfigured, oldAccepted, oldValidate, oldSend
	})
	for range 2 {
		if err := completeRetirementRecord(townRoot, record); err != nil {
			t.Fatal(err)
		}
	}
	if record.State != "aborted" || shutdownCalls != 0 {
		t.Fatalf("legacy retirement = state %q shutdown calls %d, want safe terminalization", record.State, shutdownCalls)
	}
}

func TestWriteRetirementRecordRejectsTraversalAttemptID(t *testing.T) {
	townRoot := t.TempDir()
	record := &slingRetirementRecord{AttemptID: "../../escape", State: "pending"}
	if err := writeRetirementRecord(townRoot, record); err == nil {
		t.Fatal("traversal attempt ID was accepted")
	}
	if _, err := os.Lstat(filepath.Join(townRoot, ".runtime", "escape.json")); !os.IsNotExist(err) {
		t.Fatalf("traversal record escaped retirement directory: %v", err)
	}
}

func TestWriteRetirementRecordMovesTerminalStateOutOfActiveQueue(t *testing.T) {
	townRoot := t.TempDir()
	record, err := prepareRetirementRecord(townRoot, "gt-work", "gastown/polecats/old", "old-gen", "gastown/polecats/new", "new-gen", "test")
	if err != nil {
		t.Fatal(err)
	}
	record.State = "accepted"
	if err := writeRetirementRecord(townRoot, record); err != nil {
		t.Fatal(err)
	}
	active, err := retirementRecordPath(townRoot, record.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(active); !os.IsNotExist(err) {
		t.Fatalf("terminal record remained in active queue: %v", err)
	}
	archived, err := retirementRecordPathInDir(townRoot, "sling-retirements-terminal", record.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(archived)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"state": "accepted"`) && !strings.Contains(string(data), `"state":"accepted"`) {
		t.Fatalf("archived terminal record = %s", data)
	}
}

func TestResumePendingRetirementRejectsSymlinkRecord(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior differs on Windows")
	}
	townRoot := t.TempDir()
	dir := filepath.Join(townRoot, ".runtime", "sling-retirements")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const attemptID = "11111111-1111-4111-8111-111111111111"
	record := slingRetirementRecord{
		AttemptID: attemptID, BeadID: "gt-work", OldAssignee: "gastown/polecats/old",
		OldIncarnation: "old-gen", NewAssignee: "gastown/polecats/new",
		NewIncarnation: "new-gen", State: "pending",
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(townRoot, "outside.json")
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, attemptID+".json")); err != nil {
		t.Fatal(err)
	}
	oldCapture, oldComplete := capturePolecatIncarnationFn, completeRetirementRecordFn
	t.Cleanup(func() { capturePolecatIncarnationFn, completeRetirementRecordFn = oldCapture, oldComplete })
	capturePolecatIncarnationFn = func(string, string) (string, error) { return "new-gen", nil }
	completeRetirementRecordFn = func(string, *slingRetirementRecord) error { return nil }
	if resumed, err := resumePendingRetirement(townRoot, record.BeadID, record.NewAssignee); err == nil || resumed {
		t.Fatalf("symlink retirement record resume = (%v, %v), want fail closed", resumed, err)
	}
}

func TestResumePendingRetirementRejectsMismatchedAttemptID(t *testing.T) {
	townRoot := t.TempDir()
	dir := filepath.Join(townRoot, ".runtime", "sling-retirements")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const filenameID = "11111111-1111-4111-8111-111111111111"
	record := slingRetirementRecord{
		AttemptID: "../../escape", BeadID: "gt-work", OldAssignee: "gastown/polecats/old",
		OldIncarnation: "old-gen", NewAssignee: "gastown/polecats/new",
		NewIncarnation: "new-gen", State: "pending",
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filenameID+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	oldCapture, oldComplete := capturePolecatIncarnationFn, completeRetirementRecordFn
	t.Cleanup(func() { capturePolecatIncarnationFn, completeRetirementRecordFn = oldCapture, oldComplete })
	capturePolecatIncarnationFn = func(string, string) (string, error) { return "new-gen", nil }
	completeRetirementRecordFn = func(string, *slingRetirementRecord) error {
		t.Fatal("mismatched record reached write path")
		return nil
	}
	if resumed, err := resumePendingRetirement(townRoot, record.BeadID, record.NewAssignee); err == nil || resumed {
		t.Fatalf("mismatched retirement record resume = (%v, %v), want fail closed", resumed, err)
	}
}

func TestPrepareRetirementRecordSkipsIdenticalOwnerGeneration(t *testing.T) {
	record, err := prepareRetirementRecord(t.TempDir(), "gt-work", "gastown/polecats/toast", "gen-1", "gastown/polecats/toast", "gen-1", "test")
	if err != nil || record != nil {
		t.Fatalf("same-owner retirement = (%+v, %v), want nil", record, err)
	}
}

func TestPrepareRetirementRecordKeepsSameNameReplacementGeneration(t *testing.T) {
	record, err := prepareRetirementRecord(t.TempDir(), "gt-work", "gastown/polecats/toast", "gen-1", "gastown/polecats/toast", "gen-2", "test")
	if err != nil {
		t.Fatal(err)
	}
	if record == nil || record.OldIncarnation != "gen-1" || record.NewIncarnation != "gen-2" {
		t.Fatalf("same-name replacement retirement = %+v, want gen-1 -> gen-2", record)
	}
}

func TestCompleteRetirementRecordKeepsPendingAfterStoredMailSendError(t *testing.T) {
	stubPortableWitnessAuthorityCapture(t)
	townRoot := t.TempDir()
	record, err := prepareRetirementRecord(townRoot, "gt-work", "gastown/polecats/old", "old-gen", "gastown/polecats/new", "new-gen", "test")
	if err != nil {
		t.Fatal(err)
	}
	oldStored, oldSend, oldAccepted, oldValidate := retirementMessageStoredFn, sendRetirementMessageFn, retirementAcceptanceStoredFn, validateRetirementReplacementFn
	t.Cleanup(func() {
		retirementMessageStoredFn, sendRetirementMessageFn, retirementAcceptanceStoredFn, validateRetirementReplacementFn = oldStored, oldSend, oldAccepted, oldValidate
	})
	retirementAcceptanceStoredFn = func(string, string, *mail.Message, *slingRetirementRecord) (bool, error) { return false, nil }
	validateRetirementReplacementFn = func(string, *slingRetirementRecord) error { return nil }
	stored := false
	retirementMessageStoredFn = func(_ string, _ string, msg *mail.Message) (bool, error) {
		if msg.From != "mayor/" {
			t.Fatalf("retirement reply address = %q, want mayor/", msg.From)
		}
		if !strings.Contains(msg.Body, "AttemptID: "+record.AttemptID) ||
			!strings.Contains(msg.Body, "OldIncarnation: old-gen") ||
			!strings.Contains(msg.Body, "NewIncarnation: new-gen") {
			t.Fatalf("retirement body lacks exact generations: %q", msg.Body)
		}
		if strings.Contains(msg.Body, record.Capability) || strings.Contains(msg.Body, "Capability:") {
			t.Fatalf("retirement body disclosed broker MAC capability: %q", msg.Body)
		}
		return stored, nil
	}
	sendRetirementMessageFn = func(string, *mail.Message) error {
		stored = true
		return errors.New("notification acknowledgement lost")
	}
	if err := completeRetirementRecord(townRoot, record); !errors.Is(err, errSlingRetirementPending) {
		t.Fatalf("stored mail outcome = %v, want explicit pending", err)
	}
	path, err := retirementRecordPath(townRoot, record.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got slingRetirementRecord
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "pending" {
		t.Fatalf("retirement state = %q, want pending", got.State)
	}
}

func TestCompleteRetirementRecordAcceptsTerminalProofBeforeReplacementRevalidation(t *testing.T) {
	stubPortableWitnessAuthorityCapture(t)
	townRoot := t.TempDir()
	record, err := prepareRetirementRecord(townRoot, "gt-work", "gastown/polecats/old", "old-gen", "gastown/polecats/new", "new-gen", "test")
	if err != nil {
		t.Fatal(err)
	}
	oldAccepted, oldValidate := retirementAcceptanceStoredFn, validateRetirementReplacementFn
	t.Cleanup(func() {
		retirementAcceptanceStoredFn, validateRetirementReplacementFn = oldAccepted, oldValidate
	})
	retirementAcceptanceStoredFn = func(string, string, *mail.Message, *slingRetirementRecord) (bool, error) {
		return true, nil
	}
	validateRetirementReplacementFn = func(string, *slingRetirementRecord) error {
		t.Fatal("authenticated terminal proof revalidated mutable replacement custody")
		return nil
	}

	if err := completeRetirementRecord(townRoot, record); err != nil {
		t.Fatal(err)
	}
	if record.State != "accepted" {
		t.Fatalf("retirement state = %q, want accepted", record.State)
	}
}

func TestCompleteRetirementRecordReconcilesPredecessorBeforePortableAuthorityMigration(t *testing.T) {
	townRoot := t.TempDir()
	record, err := prepareRetirementRecord(townRoot, "gt-work", "gastown/polecats/old", "old-gen", "gastown/polecats/new", "new-gen", "test")
	if err != nil {
		t.Fatal(err)
	}
	oldAccepted, oldCapture, oldValidate := retirementAcceptanceStoredFn, captureLifecycleWitnessAuthorityFn, validateRetirementReplacementFn
	t.Cleanup(func() {
		retirementAcceptanceStoredFn, captureLifecycleWitnessAuthorityFn, validateRetirementReplacementFn = oldAccepted, oldCapture, oldValidate
	})
	retirementAcceptanceStoredFn = func(_ string, _ string, _ *mail.Message, got *slingRetirementRecord) (bool, error) {
		if got.WitnessAuthority != nil || got.DeliveryID != "" {
			t.Fatalf("predecessor receipt checked after delivery mutation: %+v", got)
		}
		return true, nil
	}
	captureLifecycleWitnessAuthorityFn = func(string, string) (*witnesspkg.LifecycleWitnessAuthority, error) {
		t.Fatal("terminal predecessor receipt captured a new Witness authority")
		return nil, nil
	}
	validateRetirementReplacementFn = func(string, *slingRetirementRecord) error {
		t.Fatal("terminal predecessor receipt revalidated replacement custody")
		return nil
	}
	if err := completeRetirementRecord(townRoot, record); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteRetirementRecordRenewsPortableDeliveryAfterWitnessRestart(t *testing.T) {
	townRoot := t.TempDir()
	record, err := prepareRetirementRecord(townRoot, "gt-work", "gastown/polecats/old", "old-gen", "gastown/polecats/new", "new-gen", "test")
	if err != nil {
		t.Fatal(err)
	}
	oldAccepted, oldCapture, oldValidate, oldStored, oldSend, oldPortable := retirementAcceptanceStoredFn, captureLifecycleWitnessAuthorityFn, validateRetirementReplacementFn, retirementMessageStoredFn, sendRetirementMessageFn, lifecycleRetirementPortableFn
	t.Cleanup(func() {
		retirementAcceptanceStoredFn, captureLifecycleWitnessAuthorityFn, validateRetirementReplacementFn, retirementMessageStoredFn, sendRetirementMessageFn = oldAccepted, oldCapture, oldValidate, oldStored, oldSend
		lifecycleRetirementPortableFn = oldPortable
	})
	lifecycleRetirementPortableFn = func() bool { return true }
	retirementAcceptanceStoredFn = func(string, string, *mail.Message, *slingRetirementRecord) (bool, error) { return false, nil }
	validateRetirementReplacementFn = func(string, *slingRetirementRecord) error { return nil }
	retirementMessageStoredFn = func(string, string, *mail.Message) (bool, error) { return false, nil }
	authority := portableWitnessAuthorityFixture("first")
	captureLifecycleWitnessAuthorityFn = func(string, string) (*witnesspkg.LifecycleWitnessAuthority, error) { return authority, nil }
	var bodies []string
	sendRetirementMessageFn = func(_ string, msg *mail.Message) error {
		bodies = append(bodies, msg.Body)
		return nil
	}
	if err := completeRetirementRecord(townRoot, record); !errors.Is(err, errSlingRetirementPending) {
		t.Fatalf("first delivery = %v, want pending", err)
	}
	firstDeliveryID := record.DeliveryID
	authority = portableWitnessAuthorityFixture("replacement")
	if err := completeRetirementRecord(townRoot, record); !errors.Is(err, errSlingRetirementPending) {
		t.Fatalf("renewed delivery = %v, want pending", err)
	}
	if firstDeliveryID == "" || record.DeliveryID == "" || record.DeliveryID == firstDeliveryID {
		t.Fatalf("delivery ID did not advance across Witness restart: %q -> %q", firstDeliveryID, record.DeliveryID)
	}
	if len(bodies) != 2 || bodies[0] == bodies[1] {
		t.Fatalf("portable delivery bodies = %q, want two distinct request-bound epochs", bodies)
	}
}

func TestCompleteRetirementRecordRequiresExactSameThreadAcceptance(t *testing.T) {
	stubPortableWitnessAuthorityCapture(t)
	if runtime.GOOS == "windows" {
		t.Skip("real bd fixture is not supported on Windows")
	}
	portText := strings.TrimSpace(os.Getenv("GT_TEST_DOLT_PORT"))
	if portText == "" && os.Getenv("GT_TEST_ISOLATED") == "1" {
		portText = strings.TrimSpace(os.Getenv("GT_DOLT_PORT"))
	}
	port, err := strconv.Atoi(portText)
	if portText == "" || err != nil || port < 1 || port > 65535 {
		t.Skipf("GT_TEST_DOLT_PORT is absent or invalid: %q", portText)
	}
	townRoot := t.TempDir()
	bd := beads.NewIsolatedWithPort(townRoot, port)
	if err := bd.Init("hq"); err != nil {
		t.Fatalf("bd init: %v", err)
	}
	t.Setenv("GT_DOLT_PORT", portText)
	work, err := bd.Create(beads.CreateOptions{Title: "work", Type: "task", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	oldAssignee := "gastown/polecats/old"
	if err := bd.Update(work.ID, beads.UpdateOptions{Assignee: &oldAssignee}); err != nil {
		t.Fatal(err)
	}
	newAssignee := "gastown/polecats/new"
	record, err := prepareRetirementRecord(townRoot, work.ID, oldAssignee, "old-gen", newAssignee, "new-gen", "mayor/")
	if err != nil {
		t.Fatal(err)
	}
	if err := bd.Update(work.ID, beads.UpdateOptions{Assignee: &newAssignee}); err != nil {
		t.Fatal(err)
	}
	oldStored, oldSend, oldCapture, oldApplied := retirementMessageStoredFn, sendRetirementMessageFn, capturePolecatIncarnationFn, retirementAppliedPostconditionFn
	t.Cleanup(func() {
		retirementMessageStoredFn, sendRetirementMessageFn, capturePolecatIncarnationFn, retirementAppliedPostconditionFn = oldStored, oldSend, oldCapture, oldApplied
	})
	capturePolecatIncarnationFn = func(string, string) (string, error) { return "new-gen", nil }
	retirementMessageStoredFn = retirementMessageStored
	var request *mail.Message
	sendRetirementMessageFn = func(root string, msg *mail.Message) error {
		copy := *msg
		if err := mail.NewRouter(root).Send(&copy); err != nil {
			return err
		}
		request = &copy
		return nil
	}
	if err := completeRetirementRecord(townRoot, record); !errors.Is(err, errSlingRetirementPending) {
		t.Fatalf("initial retirement outcome = %v, want pending", err)
	}
	if record.State != "pending" || request == nil {
		t.Fatalf("retirement after request = state %q request %+v, want pending stored request", record.State, request)
	}
	stored, err := mail.NewMailboxFromAddress(request.To, townRoot).ListByThread(request.ThreadID)
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored retirement request = (%+v, %v), want one", stored, err)
	}
	request.ID = stored[0].ID

	ackBody := func(newIncarnation, appliedReceipt string) string {
		return "Result: accepted\nAttemptID: " + record.AttemptID +
			"\nAppliedReceipt: " + appliedReceipt +
			"\nBead: " + record.BeadID +
			"\nOldAssignee: " + record.OldAssignee +
			"\nOldIncarnation: " + record.OldIncarnation +
			"\nNewAssignee: " + record.NewAssignee +
			"\nNewIncarnation: " + newIncarnation
	}
	wrong := &mail.Message{
		From: request.To, To: request.From,
		Subject: "ACK " + request.Subject, Body: ackBody("wrong-gen", "forged-receipt"),
		Type: mail.TypeReply, ThreadID: request.ThreadID, ReplyTo: request.ID,
	}
	if err := mail.NewRouter(townRoot).Send(wrong); err != nil {
		t.Fatal(err)
	}
	if err := completeRetirementRecord(townRoot, record); !errors.Is(err, errSlingRetirementPending) {
		t.Fatalf("mismatched reply outcome = %v, want pending", err)
	}
	if record.State != "pending" {
		t.Fatalf("retirement accepted mismatched reply: state %q", record.State)
	}

	forged := *wrong
	forged.ID = ""
	forged.Body = ackBody(record.NewIncarnation, "forged-receipt")
	if err := mail.NewRouter(townRoot).Send(&forged); err != nil {
		t.Fatal(err)
	}
	if err := completeRetirementRecord(townRoot, record); !errors.Is(err, errSlingRetirementPending) {
		t.Fatalf("forged reply outcome = %v, want pending", err)
	}
	if record.State != "pending" {
		t.Fatalf("retirement accepted forged ACK without applied receipt: state %q", record.State)
	}

	if runtime.GOOS != "linux" {
		return
	}
	t.Setenv(tmux.EnvSessionBrokerWorker, "1")
	receipt, err := witnesspkg.StoreLifecycleRetirementAppliedReceipt(townRoot, record, request.ID)
	if err != nil {
		t.Fatal(err)
	}
	accepted := *wrong
	accepted.ID = ""
	accepted.Body = witnesspkg.LifecycleRetirementAcceptanceBody(record, receipt)
	if err := mail.NewRouter(townRoot).Send(&accepted); err != nil {
		t.Fatal(err)
	}
	retirementAppliedPostconditionFn = func(string, *witnesspkg.LifecycleRetirementIntent) (bool, error) { return false, nil }
	if err := completeRetirementRecord(townRoot, record); !errors.Is(err, errSlingRetirementPending) {
		t.Fatalf("valid forged terminal records outcome = %v, want pending without applied postcondition", err)
	}
	if record.State != "pending" {
		t.Fatalf("valid forged terminal records changed state to %q", record.State)
	}
	retirementAppliedPostconditionFn = func(string, *witnesspkg.LifecycleRetirementIntent) (bool, error) { return true, nil }
	if err := completeRetirementRecord(townRoot, record); err != nil {
		t.Fatal(err)
	}
	if record.State != "accepted" {
		replies, listErr := mail.NewMailboxFromAddress(request.From, townRoot).ListByThread(request.ThreadID)
		for _, reply := range replies {
			t.Logf("reply id=%q from=%q to=%q type=%q subject=%q thread=%q replyTo=%q body=%q", reply.ID, reply.From, reply.To, reply.Type, reply.Subject, reply.ThreadID, reply.ReplyTo, reply.Body)
		}
		t.Fatalf("retirement state = %q, want accepted after exact reply; listErr=%v", record.State, listErr)
	}
}

func TestCompleteRetirementRecordRevalidatesReplacementAssigneeBeforeSend(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("real bd fixture is not supported on Windows")
	}
	portText := strings.TrimSpace(os.Getenv("GT_TEST_DOLT_PORT"))
	if portText == "" && os.Getenv("GT_TEST_ISOLATED") == "1" {
		portText = strings.TrimSpace(os.Getenv("GT_DOLT_PORT"))
	}
	port, err := strconv.Atoi(portText)
	if portText == "" || err != nil || port < 1 || port > 65535 {
		t.Skipf("GT_TEST_DOLT_PORT is absent or invalid: %q", portText)
	}
	townRoot := t.TempDir()
	bd := beads.NewIsolatedWithPort(townRoot, port)
	if err := bd.Init("hq"); err != nil {
		t.Fatalf("bd init: %v", err)
	}
	t.Setenv("GT_DOLT_PORT", portText)
	work, err := bd.Create(beads.CreateOptions{Title: "work", Type: "task", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	oldAssignee := "gastown/polecats/old"
	if err := bd.Update(work.ID, beads.UpdateOptions{Assignee: &oldAssignee}); err != nil {
		t.Fatal(err)
	}
	record, err := prepareRetirementRecord(townRoot, work.ID, oldAssignee, "old-gen", "gastown/polecats/new", "new-gen", "mayor/")
	if err != nil {
		t.Fatal(err)
	}
	driftedAssignee := "gastown/polecats/drifted"
	if err := bd.Update(work.ID, beads.UpdateOptions{Assignee: &driftedAssignee}); err != nil {
		t.Fatal(err)
	}
	oldStored, oldSend, oldAccepted := retirementMessageStoredFn, sendRetirementMessageFn, retirementAcceptanceStoredFn
	t.Cleanup(func() {
		retirementMessageStoredFn, sendRetirementMessageFn, retirementAcceptanceStoredFn = oldStored, oldSend, oldAccepted
	})
	retirementAcceptanceStoredFn = func(string, string, *mail.Message, *slingRetirementRecord) (bool, error) { return false, nil }
	retirementMessageStoredFn = func(string, string, *mail.Message) (bool, error) { return false, nil }
	sendCalls := 0
	sendRetirementMessageFn = func(string, *mail.Message) error { sendCalls++; return nil }
	if err := completeRetirementRecord(townRoot, record); err == nil {
		t.Fatal("replacement assignee drift was accepted")
	}
	if sendCalls != 0 {
		t.Fatalf("retirement request sent after replacement drift: %d", sendCalls)
	}
}

func TestResumePendingRetirementRequiresReplacementGeneration(t *testing.T) {
	stubPortableWitnessAuthorityCapture(t)
	townRoot := t.TempDir()
	record, err := prepareRetirementRecord(townRoot, "gt-work", "gastown/polecats/old", "old-gen", "gastown/polecats/new", "new-gen", "test")
	if err != nil {
		t.Fatal(err)
	}
	oldComplete, oldAccepted, oldValidate, oldStored := completeRetirementRecordFn, retirementAcceptanceStoredFn, validateRetirementReplacementFn, retirementMessageStoredFn
	t.Cleanup(func() {
		completeRetirementRecordFn, retirementAcceptanceStoredFn, validateRetirementReplacementFn, retirementMessageStoredFn = oldComplete, oldAccepted, oldValidate, oldStored
	})
	completeRetirementRecordFn = completeRetirementRecord
	retirementAcceptanceStoredFn = func(string, string, *mail.Message, *slingRetirementRecord) (bool, error) { return false, nil }
	wantErr := errors.New("replacement incarnation changed")
	validateRetirementReplacementFn = func(string, *slingRetirementRecord) error { return wantErr }
	retirementMessageStoredFn = func(string, string, *mail.Message) (bool, error) {
		t.Fatal("unapplied retirement continued after replacement validation failed")
		return false, nil
	}
	if resumed, err := resumePendingRetirement(townRoot, record.BeadID, record.NewAssignee); !errors.Is(err, wantErr) || resumed {
		t.Fatalf("changed replacement resume = (%v, %v), want fail closed", resumed, err)
	}
}

func TestResumePendingRetirementAcceptsTerminalProofAfterReplacementChanges(t *testing.T) {
	townRoot := t.TempDir()
	record, err := prepareRetirementRecord(townRoot, "gt-work", "gastown/polecats/old", "old-gen", "gastown/polecats/new", "new-gen", "test")
	if err != nil {
		t.Fatal(err)
	}
	oldCapture, oldComplete := capturePolecatIncarnationFn, completeRetirementRecordFn
	t.Cleanup(func() { capturePolecatIncarnationFn, completeRetirementRecordFn = oldCapture, oldComplete })
	capturePolecatIncarnationFn = func(string, string) (string, error) {
		t.Fatal("terminal reconciliation inspected mutable replacement incarnation")
		return "", nil
	}
	completeRetirementRecordFn = func(_ string, got *slingRetirementRecord) error {
		got.State = "accepted"
		return nil
	}

	resumed, err := resumePendingRetirement(townRoot, record.BeadID, "gastown/polecats/later-owner")
	if err != nil || !resumed {
		t.Fatalf("terminal retirement resume = (%v, %v), want accepted", resumed, err)
	}
}

func TestResumePendingRetirementDistinguishesPendingFromAccepted(t *testing.T) {
	for _, tc := range []struct {
		name      string
		complete  func(*slingRetirementRecord)
		wantError bool
	}{
		{name: "request still pending", wantError: true},
		{name: "accepted", complete: func(record *slingRetirementRecord) { record.State = "accepted" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			townRoot := t.TempDir()
			record, err := prepareRetirementRecord(townRoot, "gt-work", "gastown/polecats/old", "old-gen", "gastown/polecats/new", "new-gen", "test")
			if err != nil {
				t.Fatal(err)
			}
			oldCapture, oldComplete := capturePolecatIncarnationFn, completeRetirementRecordFn
			t.Cleanup(func() { capturePolecatIncarnationFn, completeRetirementRecordFn = oldCapture, oldComplete })
			capturePolecatIncarnationFn = func(string, string) (string, error) { return "new-gen", nil }
			completeRetirementRecordFn = func(_ string, got *slingRetirementRecord) error {
				if tc.complete != nil {
					tc.complete(got)
				}
				return nil
			}

			resumed, err := resumePendingRetirement(townRoot, record.BeadID, record.NewAssignee)
			if !resumed {
				t.Fatalf("pending record was not found: %v", err)
			}
			if errors.Is(err, errSlingRetirementPending) != tc.wantError {
				t.Fatalf("resume error = %v, want pending=%v", err, tc.wantError)
			}
		})
	}
}
