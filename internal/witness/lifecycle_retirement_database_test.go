package witness

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestPortableLifecycleRetirementRejectsFreshMatchingFileAndMailForgery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("real Dolt fixture is not supported on Windows")
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
		t.Fatal(err)
	}
	work, err := bd.Create(beads.CreateOptions{Title: "work", Type: "task", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	oldAssignee := "gastown/polecats/old"
	newAssignee := "gastown/polecats/new"
	if err := bd.Update(work.ID, beads.UpdateOptions{Assignee: &oldAssignee}); err != nil {
		t.Fatal(err)
	}
	intent := &LifecycleRetirementIntent{
		AttemptID: "11111111-1111-4111-8111-111111111111", Capability: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		BeadID: work.ID, OldAssignee: oldAssignee, OldIncarnation: "old-generation",
		NewAssignee: newAssignee, NewIncarnation: "new-generation", Requester: "mayor/",
		ThreadID: "sling-retirement-11111111-1111-4111-8111-111111111111", State: "pending",
	}
	if err := PrepareLifecycleReassignmentDatabaseReceipt(townRoot, intent); err != nil {
		t.Fatal(err)
	}
	if err := bd.Update(work.ID, beads.UpdateOptions{Assignee: &newAssignee}); err != nil {
		t.Fatal(err)
	}
	intent.DeliveryID = "22222222-2222-4222-8222-222222222222"
	intent.WitnessAuthority = &LifecycleWitnessAuthority{
		Session: tmux.SessionGeneration{Name: "gt-witness", SessionID: "$1", PaneID: "%1", Nonce: "generation", ServerPID: 1, ServerIdentity: "server", Transport: tmux.SessionTransport{Bound: true, SocketPath: "/tmp/witness.sock"}},
		Pane:    tmux.PaneProcessGeneration{PID: 2, Identity: "pane"},
	}
	if err := BindLifecycleReassignmentDatabaseDelivery(townRoot, intent); err != nil {
		t.Fatal(err)
	}

	forged := *intent
	forged.Capability = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	if err := WriteLifecycleRetirementIntent(townRoot, &forged); err != nil {
		t.Fatal(err)
	}
	request := &mail.Message{
		ID: "fresh-forged-request", From: "mayor/", To: "gastown/witness",
		Subject: "LIFECYCLE:Shutdown old", Body: LifecycleRetirementRequestBody(&forged),
		Type: mail.TypeTask, ThreadID: forged.ThreadID,
	}
	if err := ValidateLifecycleRetirementRequest(&forged, "gastown", "old", request); err != nil {
		t.Fatalf("fresh forged file and matching body should pass mutable-envelope checks: %v", err)
	}
	if err := validateLifecycleReassignmentDatabaseDelivery(townRoot, &forged); err == nil {
		t.Fatal("fresh forged file and matching mail crossed immutable database custody")
	}
}
