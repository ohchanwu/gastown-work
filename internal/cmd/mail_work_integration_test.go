package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestMailWorkLifecycleEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix tmux")
	}
	if os.Getenv("GT_TEST_ISOLATED") != "1" || os.Getenv("GT_DOLT_HOST") != "127.0.0.1" {
		t.Skip("requires the repository isolated Dolt test launcher")
	}
	port, err := strconv.Atoi(os.Getenv("GT_DOLT_PORT"))
	if err != nil || port <= 0 || port == 33327 {
		t.Fatalf("refusing non-isolated Dolt port %q", os.Getenv("GT_DOLT_PORT"))
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not installed")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	townRoot := setupMailWorkLifecycleTown(t, port)
	townBeads := beads.NewWithBeadsDir(townRoot, filepath.Join(townRoot, ".beads"))
	primary, err := townBeads.Create(beads.CreateOptions{Title: "Primary hook work", Priority: 2, Actor: "mayor/"})
	if err != nil {
		t.Fatalf("create primary work: %v", err)
	}
	if _, err := townBeads.CreateAgentBead(beads.DeaconBeadIDTown(), "Deacon", &beads.AgentFields{
		RoleType: "deacon", AgentState: "working", HookBead: primary.ID,
	}); err != nil {
		t.Fatalf("create deacon agent bead: %v", err)
	}

	socket := fmt.Sprintf("gt-mail-work-e2e-%d", time.Now().UnixNano())
	t.Setenv("GT_TOWN_SOCKET", socket)
	t.Setenv("GT_SESSION", "hq-deacon")
	t.Setenv("GT_ROLE", "deacon")
	socketRoot, err := os.MkdirTemp("/tmp", "gtmw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketRoot) })
	t.Setenv("TMUX_TMPDIR", socketRoot)
	tm := tmux.NewTmuxWithSocket(socket)
	generation := createMailWorkOwnerSession(t, tm, socket, townRoot)
	t.Cleanup(func() { _ = tm.KillSessionGeneration(generation) })

	router := mail.NewRouter(townRoot)
	deaconMailbox, err := router.GetMailbox("deacon/")
	if err != nil {
		t.Fatal(err)
	}
	first := sendLifecycleTask(t, router, "Lifecycle task")
	storedFirst := findLifecycleMessage(t, deaconMailbox, first.Subject)
	if _, err := deaconMailbox.Get(storedFirst.ID); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if err := deaconMailbox.MarkReadOnly(storedFirst.ID); err != nil {
		t.Fatalf("mark task read: %v", err)
	}

	openCtx, cancelOpen := context.WithTimeout(context.Background(), 30*time.Second)
	store, cleanup, err := townBeads.OpenStore(openCtx)
	cancelOpen()
	if err != nil {
		t.Fatalf("open lifecycle store: %v", err)
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	workStore := mail.NewMailWorkStore(store)
	readIssue, err := store.GetIssue(ctx, storedFirst.ID)
	if err != nil {
		t.Fatalf("get read task: %v", err)
	}
	readWork, err := mail.ParseMailWorkMetadata(readIssue.Metadata)
	if err != nil || readIssue.Status != beadsdk.StatusOpen || readWork.Claim != nil {
		t.Fatalf("reading claimed work: issue=%+v work=%+v err=%v", readIssue, readWork, err)
	}
	if _, err := workStore.Claim(ctx, storedFirst.ID, "deacon/", generation, nil); err != nil {
		t.Fatalf("claim direct task: %v", err)
	}

	agentBeads := map[string]*beads.Issue{
		beads.DeaconBeadIDTown(): {
			ID: beads.DeaconBeadIDTown(), HookBead: primary.ID,
			Description: beads.FormatAgentDescription("Deacon", &beads.AgentFields{RoleType: "deacon", AgentState: "working", HookBead: primary.ID}),
		},
	}
	hookBeads := map[string]*beads.Issue{primary.ID: primary}
	deacon := findGlobalAgent(t, discoverGlobalAgents(townRoot, map[string]bool{"hq-deacon": true}, agentBeads, hookBeads, router, false), "deacon/")
	if !deacon.HasWork || deacon.HookBead != primary.ID || !deacon.HasMailWork || !deacon.HasAnyWork || len(deacon.MailWork) != 1 {
		t.Fatalf("combined primary/mail status = %+v", deacon)
	}

	completed, err := workStore.Complete(ctx, storedFirst.ID, "deacon/", generation, "", "Lifecycle completed")
	if err != nil {
		t.Fatalf("complete task: %v", err)
	}
	retry, err := workStore.Complete(ctx, storedFirst.ID, "deacon/", generation, "", "Lifecycle completed")
	if err != nil || !completed.Created || retry.Created || retry.ReplyID != completed.ReplyID {
		t.Fatalf("completion retry = %+v / %+v, err=%v", completed, retry, err)
	}
	deacon = findGlobalAgent(t, discoverGlobalAgents(townRoot, map[string]bool{"hq-deacon": true}, agentBeads, hookBeads, router, false), "deacon/")
	if !deacon.HasWork || deacon.HookBead != primary.ID || deacon.HasMailWork || !deacon.HasAnyWork {
		t.Fatalf("post-completion status = %+v", deacon)
	}

	second := sendLifecycleTask(t, router, "Recovery task")
	storedSecond := findLifecycleMessage(t, deaconMailbox, second.Subject)
	if _, err := workStore.Claim(ctx, storedSecond.ID, "deacon/", generation, nil); err != nil {
		t.Fatalf("claim recovery task: %v", err)
	}
	noPrimary := map[string]*beads.Issue{
		beads.DeaconBeadIDTown(): {ID: beads.DeaconBeadIDTown(), Description: agentBeads[beads.DeaconBeadIDTown()].Description},
	}
	deacon = findGlobalAgent(t, discoverGlobalAgents(townRoot, map[string]bool{"hq-deacon": true}, noPrimary, nil, router, false), "deacon/")
	if deacon.HasWork || !deacon.HasMailWork || !deacon.HasAnyWork {
		t.Fatalf("mail-only status = %+v", deacon)
	}

	replacementClaim := generation
	replacementClaim.Nonce = "replacement-generation"
	if _, err := workStore.Claim(ctx, storedSecond.ID, "deacon/", replacementClaim, nil); !errors.Is(err, mail.ErrMailWorkConflict) {
		t.Fatalf("replacement generation claim error = %v, want conflict", err)
	}
	storedSecondIssue, err := store.GetIssue(ctx, storedSecond.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondWork, err := mail.ParseMailWorkMetadata(storedSecondIssue.Metadata)
	if err != nil || secondWork.Claim == nil {
		t.Fatalf("parse recovery claim: work=%+v err=%v", secondWork, err)
	}
	live := mail.ObserveMailWorkGeneration(&secondWork.Claim.Generation)
	liveResult, err := workStore.Recover(ctx, storedSecond.ID, "deacon/witness", mail.MailWorkRecoveryScan{Status: mail.WorkStateInProgress, Generation: secondWork.Claim.Generation}, live)
	if err != nil || liveResult.Action != mail.MailWorkRecoveryPreserved {
		t.Fatalf("live recovery = %+v err=%v", liveResult, err)
	}

	if err := tm.KillSessionGeneration(generation); err != nil {
		t.Fatalf("stop exact owner generation: %v", err)
	}
	if got := mail.ObserveMailWorkGeneration(&secondWork.Claim.Generation); got != mail.MailWorkGenerationDead {
		t.Fatalf("stopped owner generation = %q, want dead", got)
	}
	replacement := createMailWorkOwnerSession(t, tm, socket, townRoot)
	t.Cleanup(func() { _ = tm.KillSessionGeneration(replacement) })
	if got := mail.ObserveMailWorkGeneration(&secondWork.Claim.Generation); got != mail.MailWorkGenerationReplaced {
		t.Fatalf("old owner with replacement session = %q, want replaced", got)
	}
	recovered, err := workStore.Recover(ctx, storedSecond.ID, "deacon/witness", mail.MailWorkRecoveryScan{Status: mail.WorkStateInProgress, Generation: secondWork.Claim.Generation}, mail.MailWorkGenerationReplaced)
	if err != nil || recovered.Action != mail.MailWorkRecoveryReopened {
		t.Fatalf("replacement recovery = %+v err=%v", recovered, err)
	}
	if _, err := workStore.Claim(ctx, storedSecond.ID, "deacon/", replacement, nil); err != nil {
		t.Fatalf("ordinary claim after safe reopen: %v", err)
	}

	_, finalAgentFields, err := townBeads.GetAgentBead(beads.DeaconBeadIDTown())
	if err != nil || finalAgentFields == nil || finalAgentFields.HookBead != primary.ID {
		t.Fatalf("primary hook changed: fields=%+v err=%v", finalAgentFields, err)
	}
}

func setupMailWorkLifecycleTown(t *testing.T, port int) string {
	t.Helper()
	townRoot := t.TempDir()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveTownConfig(filepath.Join(mayorDir, "town.json"), &config.TownConfig{
		Type: "town", Version: config.CurrentTownVersion, Name: "mail-work-test", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GT_TOWN_ROOT", townRoot)
	t.Setenv("GT_ROOT", townRoot)
	t.Setenv("GT_DOLT_HOST", "127.0.0.1")
	t.Setenv("GT_DOLT_PORT", strconv.Itoa(port))
	t.Setenv("BEADS_DOLT_SERVER_HOST", "127.0.0.1")
	t.Setenv("BEADS_DOLT_SERVER_PORT", strconv.Itoa(port))
	t.Setenv("BEADS_DOLT_PORT", strconv.Itoa(port))
	t.Setenv("BEADS_DOLT_AUTO_START", "0")
	t.Setenv("BEADS_TEST_MODE", "1")
	t.Setenv("BD_ACTOR", "mayor/")

	command := exec.Command("bd", "init", "--prefix", "hq", "--database", fmt.Sprintf("mail_work_%d", time.Now().UnixNano()), "--server", "--external", "--server-host", "127.0.0.1", "--server-port", strconv.Itoa(port), "--skip-hooks", "--skip-agents", "--non-interactive")
	command.Dir = townRoot
	command.Env = os.Environ()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("initialize town beads: %v\n%s", err, output)
	}
	return townRoot
}

func sendLifecycleTask(t *testing.T, router *mail.Router, subject string) *mail.Message {
	t.Helper()
	message := mail.NewMessage("mayor/", "deacon/", subject, "Please handle this task")
	message.Type = mail.TypeTask
	message.Wisp = false
	message.SuppressNotify = true
	if err := router.Send(message); err != nil {
		t.Fatalf("send %s: %v", subject, err)
	}
	return message
}

func createMailWorkOwnerSession(t *testing.T, transport *tmux.Tmux, socket, workDir string) tmux.SessionGeneration {
	t.Helper()
	nonce := fmt.Sprintf("mail-work-%d", time.Now().UnixNano())
	command := exec.Command("tmux", "-L", socket, "new-session", "-d", "-s", "hq-deacon", "-c", workDir,
		"-e", tmux.EnvSessionGeneration+"="+nonce, "-e", tmux.EnvSessionCustody+"="+nonce, "sleep", "300")
	command.Env = os.Environ()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create exact owner session: %v\n%s", err, output)
	}
	generation, err := transport.CaptureSessionGeneration("hq-deacon")
	if err != nil {
		t.Fatalf("capture exact owner generation: %v", err)
	}
	return generation
}

func findLifecycleMessage(t *testing.T, mailbox *mail.Mailbox, subject string) *mail.Message {
	t.Helper()
	messages, err := mailbox.ListAll()
	if err != nil {
		t.Fatalf("list lifecycle messages: %v", err)
	}
	for _, message := range messages {
		if message.Subject == subject {
			return message
		}
	}
	t.Fatalf("mail %q not found", subject)
	return nil
}

func findGlobalAgent(t *testing.T, agents []AgentRuntime, address string) AgentRuntime {
	t.Helper()
	for _, agent := range agents {
		if agent.Address == address {
			return agent
		}
	}
	t.Fatalf("agent %q not found in %+v", address, agents)
	return AgentRuntime{}
}
