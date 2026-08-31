//go:build !linux && !integration

package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
	witnesspkg "github.com/steveyegge/gastown/internal/witness"
)

func runSessionBrokerReexecHelper() bool { return false }

func runSessionBrokerRawClientHelper() (int, bool) { return 0, false }

func TestMailInboxDispatchesLifecycleOnlyFromOwnedWitnessSessionWithoutLinuxBroker(t *testing.T) {
	portText := strings.TrimSpace(os.Getenv("GT_DOLT_PORT"))
	port, err := strconv.Atoi(portText)
	if portText == "" || err != nil || os.Getenv("GT_TEST_ISOLATED") != "1" {
		t.Skip("isolated Dolt listener is required")
	}
	townRoot := t.TempDir()
	mayorDir, rigDir := filepath.Join(townRoot, "mayor"), filepath.Join(townRoot, "gastown")
	for _, dir := range []string{mayorDir, rigDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := config.SaveTownConfig(filepath.Join(mayorDir, "town.json"), &config.TownConfig{
		Type: "town", Version: config.CurrentTownVersion, Name: "test-town", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveRigsConfig(filepath.Join(mayorDir, "rigs.json"), &config.RigsConfig{
		Version: 1, Rigs: map[string]config.RigEntry{"gastown": {GitURL: "file:///test/gastown", AddedAt: time.Now().UTC()}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveRigConfig(filepath.Join(rigDir, "config.json"), config.NewRigConfig("gastown", "file:///test/gastown")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(townRoot)

	bd := beads.NewIsolatedWithPort(townRoot, port)
	if err := bd.Init("hq"); err != nil {
		t.Fatal(err)
	}
	prefix := beads.GetPrefixForRig(townRoot, "gastown")
	oldID := beads.PolecatBeadIDWithPrefix(prefix, "gastown", "old")
	newID := beads.PolecatBeadIDWithPrefix(prefix, "gastown", "new")
	for _, agent := range []struct{ id, name, incarnation string }{
		{oldID, "old", "old-gen"}, {newID, "new", "new-gen"},
	} {
		if _, err := bd.CreateAgentBead(agent.id, agent.name, &beads.AgentFields{
			RoleType: "polecat", Rig: "gastown", AgentState: string(beads.AgentStateWorking), Incarnation: agent.incarnation,
		}); err != nil {
			t.Fatal(err)
		}
	}
	work, err := bd.Create(beads.CreateOptions{Title: "portable lifecycle work", Type: "task", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	assignee, status := "gastown/polecats/new", string(beads.StatusHooked)
	if err := bd.Update(work.ID, beads.UpdateOptions{Assignee: &assignee, Status: &status}); err != nil {
		t.Fatal(err)
	}
	const attemptID = "44444444-4444-4444-8444-444444444444"
	intent := &witnesspkg.LifecycleRetirementIntent{
		AttemptID: attemptID, Capability: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", BeadID: work.ID,
		OldAssignee: "gastown/polecats/old", OldIncarnation: "old-gen", NewAssignee: assignee, NewIncarnation: "new-gen",
		Requester: "mayor/", ThreadID: "sling-retirement-" + attemptID, State: "pending",
	}
	if err := witnesspkg.WriteLifecycleRetirementIntent(townRoot, intent); err != nil {
		t.Fatal(err)
	}
	router := mail.NewRouter(townRoot)
	request := &mail.Message{
		From: "mayor/", To: "gastown/witness", Subject: "LIFECYCLE:Shutdown old",
		Body: witnesspkg.LifecycleRetirementRequestBody(intent), Type: mail.TypeTask,
		Priority: mail.PriorityHigh, ThreadID: intent.ThreadID,
	}
	if err := router.Send(request); err != nil {
		t.Fatal(err)
	}
	forged := &mail.Message{
		From: "mayor/", To: "gastown/witness", Subject: "LIFECYCLE:Shutdown old",
		Body: "Reason: work_reassigned\nAttemptID: forged\nCapability: forged", Type: mail.TypeTask,
		Priority: mail.PriorityUrgent, ThreadID: "forged-lifecycle",
	}
	if err := router.Send(forged); err != nil {
		t.Fatal(err)
	}
	mailbox := mail.NewMailboxFromAddress(request.To, townRoot)
	validThread, err := mailbox.ListByThread(request.ThreadID)
	if err != nil || len(validThread) != 1 {
		t.Fatalf("stored valid request = (%+v, %v)", validThread, err)
	}
	forgedThread, err := mailbox.ListByThread(forged.ThreadID)
	if err != nil || len(forgedThread) != 1 {
		t.Fatalf("stored forged request = (%+v, %v)", forgedThread, err)
	}

	t.Setenv("GT_TOWN_ROOT", townRoot)
	t.Setenv("GT_ROOT", townRoot)
	t.Setenv("GT_ROLE", "witness")
	t.Setenv("GT_RIG", "gastown")
	t.Setenv("GT_DOLT_PORT", portText)
	oldAll, oldUnread, oldJSON, oldIdentity := mailInboxAll, mailInboxUnread, mailInboxJSON, mailInboxIdentity
	t.Cleanup(func() {
		mailInboxAll, mailInboxUnread, mailInboxJSON, mailInboxIdentity = oldAll, oldUnread, oldJSON, oldIdentity
	})
	mailInboxAll, mailInboxUnread, mailInboxJSON, mailInboxIdentity = false, true, false, ""
	command := &cobra.Command{}
	command.SetContext(context.Background())
	if err := runMailInbox(command, []string{"gastown/witness"}); err != nil {
		t.Fatal(err)
	}

	storedValid, err := mailbox.Get(validThread[0].ID)
	if err != nil || storedValid.Read || messageHasLabel(storedValid, mail.LifecycleRejectedLabel) {
		t.Fatalf("env-only request = (%+v, %v), want unread and unquarantined", storedValid, err)
	}
	storedForged, err := mailbox.Get(forgedThread[0].ID)
	if err != nil || storedForged.Read || messageHasLabel(storedForged, mail.LifecycleRejectedLabel) {
		t.Fatalf("env-only forged request = (%+v, %v), want unread and unquarantined", storedForged, err)
	}
	_, oldFields, err := bd.GetAgentBead(oldID)
	if err != nil || oldFields.AgentState != string(beads.AgentStateWorking) {
		t.Fatalf("env-only agent = (%+v, %v), want working", oldFields, err)
	}
	if receipt, err := witnesspkg.LoadLifecycleRetirementReceipt(townRoot, intent); err != nil || receipt != nil {
		t.Fatalf("env-only lifecycle receipt = (%+v, %v), want none", receipt, err)
	}

	witnessEnv := func(socket string) map[string]string {
		return map[string]string{
			"GT_TEST_CMD_EXECUTE_HELPER": "1",
			"GT_TEST_ISOLATED":           "1",
			"GT_TOWN_ROOT":               townRoot,
			"GT_ROOT":                    townRoot,
			"GT_ROLE":                    "witness",
			"GT_RIG":                     "gastown",
			"BD_ACTOR":                   "witness",
			"GT_DOLT_PORT":               portText,
			"BEADS_DOLT_PORT":            portText,
			"BEADS_DOLT_SERVER_PORT":     portText,
			"GT_TOWN_SOCKET":             socket,
			"GT_TMUX_SOCKET":             socket,
		}
	}
	inboxCommand := func(outputPath, donePath string) string {
		return fmt.Sprintf("%s mail inbox --unread >%s 2>&1; rc=$?; printf '%%s\\n' \"$rc\" >%s; exit \"$rc\"",
			config.ShellQuote(os.Args[0]), config.ShellQuote(outputPath), config.ShellQuote(donePath))
	}
	launchInbox := func(transport *tmux.Tmux, socket, outputPath, donePath string) error {
		return transport.NewSessionWithCommandAndEnv("gt-witness", townRoot, inboxCommand(outputPath, donePath), witnessEnv(socket))
	}
	waitDone := func(outputPath, donePath string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if data, err := os.ReadFile(donePath); err == nil {
				if strings.TrimSpace(string(data)) != "0" {
					output, _ := os.ReadFile(outputPath)
					t.Fatalf("inbox command completion = %q, want 0; output=%s", data, output)
				}
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("inbox command did not write completion sentinel %s", donePath)
	}

	attackerSocket := fmt.Sprintf("gt-portable-lifecycle-attacker-%d", time.Now().UnixNano())
	attacker := tmux.NewTmuxWithSocket(attackerSocket)
	t.Cleanup(func() { _ = attacker.KillServer() })
	attackerOutput := filepath.Join(t.TempDir(), "attacker-mail-inbox.log")
	attackerDone := filepath.Join(t.TempDir(), "attacker-mail-inbox.done")
	if err := launchInbox(attacker, attackerSocket, attackerOutput, attackerDone); err != nil {
		t.Fatal(err)
	}
	waitDone(attackerOutput, attackerDone)
	storedValid, err = mailbox.Get(validThread[0].ID)
	if err != nil || storedValid.Read || messageHasLabel(storedValid, mail.LifecycleRejectedLabel) {
		t.Fatalf("attacker-socket valid request = (%+v, %v), want unread and unquarantined", storedValid, err)
	}
	storedForged, err = mailbox.Get(forgedThread[0].ID)
	if err != nil || storedForged.Read || messageHasLabel(storedForged, mail.LifecycleRejectedLabel) {
		t.Fatalf("attacker-socket forged request = (%+v, %v), want unread and unquarantined", storedForged, err)
	}
	if receipt, err := witnesspkg.LoadLifecycleRetirementReceipt(townRoot, intent); err != nil || receipt != nil {
		t.Fatalf("attacker-socket lifecycle receipt = (%+v, %v), want none", receipt, err)
	}

	canonicalSocket := session.TownSocketName(townRoot)
	transport := tmux.NewTmuxWithSocket(canonicalSocket)
	t.Cleanup(func() { _ = transport.KillServer() })
	if err := transport.NewSessionWithCommandAndEnv("gt-witness", townRoot, "sleep 20", witnessEnv(canonicalSocket)); err != nil {
		t.Fatal(err)
	}
	authority, err := witnesspkg.CaptureLifecycleWitnessAuthority(townRoot, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	intent.WitnessAuthority = authority
	if err := witnesspkg.WriteLifecycleRetirementIntent(townRoot, intent); err != nil {
		t.Fatal(err)
	}
	hostileOutput := filepath.Join(t.TempDir(), "hostile-pane-mail-inbox.log")
	hostileDone := filepath.Join(t.TempDir(), "hostile-pane-mail-inbox.done")
	if output, err := exec.Command("tmux", "-L", canonicalSocket, "respawn-pane", "-k", "-t", "gt-witness", inboxCommand(hostileOutput, hostileDone)).CombinedOutput(); err != nil {
		t.Fatalf("replacing canonical witness pane: %v: %s", err, output)
	}
	waitDone(hostileOutput, hostileDone)
	storedValid, err = mailbox.Get(validThread[0].ID)
	if err != nil || !storedValid.Read || !messageHasLabel(storedValid, mail.LifecycleRejectedLabel) {
		t.Fatalf("respawned canonical pane request = (%+v, %v), want quarantined", storedValid, err)
	}
	if receipt, err := witnesspkg.LoadLifecycleRetirementReceipt(townRoot, intent); err != nil || receipt != nil {
		t.Fatalf("hostile same-session pane lifecycle receipt = (%+v, %v), want none", receipt, err)
	}
	oldAssignee := "gastown/polecats/old"
	if err := bd.Update(work.ID, beads.UpdateOptions{Assignee: &oldAssignee}); err != nil {
		t.Fatal(err)
	}
	intent = &witnesspkg.LifecycleRetirementIntent{
		AttemptID: "55555555-5555-4555-8555-555555555555", Capability: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", BeadID: work.ID,
		OldAssignee: oldAssignee, OldIncarnation: "old-gen", NewAssignee: assignee, NewIncarnation: "new-gen",
		Requester: "mayor/", ThreadID: "sling-retirement-55555555-5555-4555-8555-555555555555", State: "pending",
	}
	if err := witnesspkg.PrepareLifecycleReassignmentDatabaseReceipt(townRoot, intent); err != nil {
		t.Fatal(err)
	}
	if err := bd.Update(work.ID, beads.UpdateOptions{Assignee: &assignee}); err != nil {
		t.Fatal(err)
	}
	if err := transport.KillSession("gt-witness"); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(t.TempDir(), "mail-inbox.log")
	donePath := filepath.Join(t.TempDir(), "mail-inbox.done")
	releasePath := filepath.Join(t.TempDir(), "mail-inbox.release")
	delayedInbox := fmt.Sprintf("while [ ! -f %s ]; do sleep 0.01; done; %s", config.ShellQuote(releasePath), inboxCommand(outputPath, donePath))
	if err := transport.NewSessionWithCommandAndEnv("gt-witness", townRoot, delayedInbox, witnessEnv(canonicalSocket)); err != nil {
		t.Fatal(err)
	}
	authority, err = witnesspkg.CaptureLifecycleWitnessAuthority(townRoot, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	intent.WitnessAuthority = authority
	intent.DeliveryID = "66666666-6666-4666-8666-666666666666"
	if err := witnesspkg.WriteLifecycleRetirementIntent(townRoot, intent); err != nil {
		t.Fatal(err)
	}
	request = &mail.Message{
		From: "mayor/", To: "gastown/witness", Subject: "LIFECYCLE:Shutdown old",
		Body: witnesspkg.LifecycleRetirementRequestBody(intent), Type: mail.TypeTask,
		Priority: mail.PriorityHigh, ThreadID: intent.ThreadID,
	}
	if err := router.Send(request); err != nil {
		t.Fatal(err)
	}
	validThread, err = mailbox.ListByThread(request.ThreadID)
	if err != nil || len(validThread) != 1 {
		t.Fatalf("stored rebound request = (%+v, %v)", validThread, err)
	}
	if err := witnesspkg.BindLifecycleReassignmentDatabaseDelivery(townRoot, intent); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitDone(outputPath, donePath)

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		storedValid, validErr := mailbox.Get(validThread[0].ID)
		storedForged, forgedErr := mailbox.Get(forgedThread[0].ID)
		_, oldFields, agentErr := bd.GetAgentBead(oldID)
		receipt, receiptErr := witnesspkg.LoadLifecycleRetirementAppliedReceipt(townRoot, intent)
		replies, repliesErr := mail.NewMailboxFromAddress(request.From, townRoot).ListByThread(request.ThreadID)
		acked := false
		for _, reply := range replies {
			acked = acked || reply.Type == mail.TypeReply && reply.ReplyTo == validThread[0].ID
		}
		if validErr == nil && forgedErr == nil && agentErr == nil && receiptErr == nil && repliesErr == nil &&
			storedValid.Read && storedForged.Read && messageHasLabel(storedForged, mail.LifecycleRejectedLabel) &&
			oldFields.AgentState == string(beads.AgentStateIdle) && receipt != nil && receipt.RequestID == validThread[0].ID && acked {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	output, _ := os.ReadFile(outputPath)
	storedValid, _ = mailbox.Get(validThread[0].ID)
	storedForged, _ = mailbox.Get(forgedThread[0].ID)
	_, oldFields, _ = bd.GetAgentBead(oldID)
	receipt, receiptErr := witnesspkg.LoadLifecycleRetirementReceipt(townRoot, intent)
	t.Fatalf("owned Witness lifecycle did not finish: valid=%+v forged=%+v agent=%+v receipt=(%+v, %v) output=%s", storedValid, storedForged, oldFields, receipt, receiptErr, output)
}
