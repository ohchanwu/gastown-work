//go:build linux && !integration

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dog"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/tmux"
	witnesspkg "github.com/steveyegge/gastown/internal/witness"
	"golang.org/x/sys/unix"
)

const (
	envSessionBrokerReexecHelper = "GT_TEST_CMD_REEXEC_HELPER"
	envSessionBrokerReexecStage  = "GT_TEST_CMD_REEXEC_STAGE"
	envSessionBrokerReexecClose  = "GT_TEST_CMD_REEXEC_CLOSE_BROKER"
	envSessionBrokerRawClient    = "GT_TEST_CMD_BROKER_RAW_CLIENT"
)

func runSessionBrokerReexecHelper() bool {
	if os.Getenv(envSessionBrokerReexecHelper) != "1" {
		return false
	}
	if os.Getenv(envSessionBrokerReexecStage) == "" {
		if err := os.Setenv(envSessionBrokerReexecStage, "1"); err != nil {
			panic(err)
		}
		if err := unix.Exec("/proc/self/exe", os.Args, os.Environ()); err != nil {
			panic(err)
		}
	}
	if os.Getenv(envSessionBrokerReexecClose) == "1" {
		if err := unix.Close(6); err != nil {
			fmt.Fprintf(os.Stderr, "broker-close=%v\n", err)
		} else {
			fmt.Fprintln(os.Stderr, "broker-close=allowed")
		}
	}
	_ = os.Unsetenv(envSessionBrokerReexecHelper)
	_ = os.Unsetenv(envSessionBrokerReexecStage)
	_ = os.Unsetenv(envSessionBrokerReexecClose)
	return true
}

func runSessionBrokerRawClientHelper() (int, bool) {
	if os.Getenv(envSessionBrokerRawClient) != "1" {
		return 0, false
	}
	handled, code, err := tmux.RunSessionBrokerClient(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "raw broker client: %v\n", err)
		return 1, true
	}
	if !handled {
		fmt.Fprintln(os.Stderr, "raw broker client: inherited endpoint was not handled")
		return 1, true
	}
	return code, true
}

func TestSessionBrokerRawUnsafeFramesNeverStartWorker(t *testing.T) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	clientEndpoint := os.NewFile(uintptr(pair[1]), "raw-broker-client")
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	proofPath := filepath.Join(t.TempDir(), "worker-started")
	workerPath := filepath.Join(t.TempDir(), "worker")
	worker := "#!/bin/sh\ntouch " + config.ShellQuote(proofPath) + "\nexit 0\n"
	if err := os.WriteFile(workerPath, []byte(worker), 0o700); err != nil {
		t.Fatal(err)
	}
	go func() {
		serverDone <- tmux.ServeSessionBroker(ctx, workerPath, pair[0], func(args []string) error {
			return IsBrokerSafeCommand(rootCmd, args)
		})
	}()
	t.Cleanup(func() {
		cancel()
		_ = clientEndpoint.Close()
		select {
		case err := <-serverDone:
			if err != nil {
				t.Errorf("ServeSessionBroker() cleanup error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("ServeSessionBroker() did not stop")
		}
	})

	nullFiles := make([]*os.File, 3)
	for index := range nullFiles {
		nullFiles[index], err = os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer nullFiles[index].Close()
	}
	unsafeRequests := [][]string{
		{"session-custody-init"},
		{"session-custody", "--id", "00000000-0000-0000-0000-000000000000", "--", "true"},
		{"shell", "-c", "touch " + proofPath},
		{"doctor", "--fix"},
		{"up", "--restore"},
		{"tmux", "send-keys", "hq-mayor", "payload"},
		{"sling", "gt-example", "gastown"},
		{"done"},
		{"mail", "send", "deacon", "--from", "mayor/", "--subject", "LIFECYCLE shutdown"},
	}
	for _, args := range unsafeRequests {
		command := exec.Command(os.Args[0], args...)
		command.Env = append(os.Environ(), envSessionBrokerRawClient+"=1")
		command.ExtraFiles = []*os.File{nullFiles[0], nullFiles[1], nullFiles[2], clientEndpoint}
		output, err := command.CombinedOutput()
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 126 {
			t.Fatalf("raw unsafe request %q error = %v, output = %q; want exit 126", args, err, output)
		}
	}
	if _, err := os.Stat(proofPath); !os.IsNotExist(err) {
		t.Fatalf("unsafe raw frame started worker; stat error = %v", err)
	}
}

func TestSessionBrokerMailReadDeniesForeignMailboxBead(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(townRoot, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	readProofPrefix := filepath.Join(t.TempDir(), "read-label-")
	fakeBD := filepath.Join(binDir, "bd")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$BD_LOG"
if [ "$1" = "show" ] && [ "$2" = "foreign-mail" ]; then
  printf '%s\n' '[{"id":"foreign-mail","title":"Foreign","description":"foreign private body","status":"open","priority":2,"assignee":"gastown/other","created_at":"2026-08-09T00:00:00Z","labels":["gt:message","from:mayor/","msg-type:notification"]}]'
  exit 0
fi
if [ "$1" = "show" ] && [ "$2" = "foreign-hook" ]; then
  printf '%s\n' '[{"id":"foreign-hook","title":"Hook","description":"foreign hook body","status":"hooked","priority":2,"assignee":"gastown/other","created_at":"2026-08-09T00:00:01Z","labels":["gt:hook","from:mayor/"]}]'
  exit 0
fi
if [ "$1" = "label" ] && [ "$2" = "add" ] && [ "$4" = "read" ]; then
  : > "$READ_PROOF_PREFIX$3"
  exit 0
fi
printf 'unexpected bd args: %s\n' "$*" >&2
exit 1
`
	if err := os.WriteFile(fakeBD, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_LOG", bdLog)
	t.Setenv("READ_PROOF_PREFIX", readProofPrefix)
	t.Setenv("GT_TOWN_ROOT", townRoot)
	t.Setenv("GT_ROOT", townRoot)
	t.Setenv("GT_ROLE", "witness")
	t.Setenv("GT_RIG", "gastown")
	t.Setenv("GT_TEST_CMD_EXECUTE_HELPER", "1")

	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	clientEndpoint := os.NewFile(uintptr(pair[1]), "mail-read-broker-client")
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- tmux.ServeSessionBroker(ctx, os.Args[0], pair[0], func(args []string) error {
			return IsBrokerSafeCommand(rootCmd, args)
		})
	}()
	t.Cleanup(func() {
		cancel()
		_ = clientEndpoint.Close()
		select {
		case err := <-serverDone:
			if err != nil {
				t.Errorf("ServeSessionBroker() cleanup error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("ServeSessionBroker() did not stop")
		}
	})

	nullFiles := make([]*os.File, 3)
	for index := range nullFiles {
		nullFiles[index], err = os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer nullFiles[index].Close()
	}
	for _, test := range []struct {
		id   string
		body string
	}{
		{id: "foreign-mail", body: "foreign private body"},
		{id: "foreign-hook", body: "foreign hook body"},
	} {
		command := exec.Command(os.Args[0], "mail", "read", test.id)
		command.Env = os.Environ()
		command.ExtraFiles = []*os.File{nullFiles[0], nullFiles[1], nullFiles[2], clientEndpoint}
		output, err := command.CombinedOutput()
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() == 0 {
			t.Fatalf("brokered mail read %q error = %v, output = %q; want nonzero exit", test.id, err, output)
		}
		if strings.Contains(string(output), test.body) {
			t.Fatalf("brokered mail read %q disclosed body: %q", test.id, output)
		}
		if _, err := os.Stat(readProofPrefix + test.id); !os.IsNotExist(err) {
			t.Fatalf("brokered mail read %q added read label; stat error = %v", test.id, err)
		}
	}
	logData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"foreign-mail", "foreign-hook"} {
		if !strings.Contains(string(logData), "show "+id+" --json") {
			t.Fatalf("brokered mail read %q did not reach recipient-scoped lookup: %q", id, logData)
		}
	}
}

func TestSessionBrokerInboxDispatchesExactOwnedWitnessLifecycleCommand(t *testing.T) {
	portText := strings.TrimSpace(os.Getenv("GT_TEST_DOLT_PORT"))
	if portText == "" && os.Getenv("GT_TEST_ISOLATED") == "1" {
		portText = strings.TrimSpace(os.Getenv("GT_DOLT_PORT"))
	}
	port, err := strconv.Atoi(portText)
	if portText == "" || err != nil || port < 1 || port > 65535 {
		t.Skipf("GT_TEST_DOLT_PORT is absent or invalid: %q", portText)
	}

	townRoot := t.TempDir()
	mayorDir := filepath.Join(townRoot, "mayor")
	rigDir := filepath.Join(townRoot, "gastown")
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
		t.Fatalf("bd init: %v", err)
	}
	prefix := beads.GetPrefixForRig(townRoot, "gastown")
	oldID := beads.PolecatBeadIDWithPrefix(prefix, "gastown", "old")
	newID := beads.PolecatBeadIDWithPrefix(prefix, "gastown", "new")
	for _, agent := range []struct{ id, name, incarnation string }{
		{oldID, "old", "old-gen"},
		{newID, "new", "new-gen"},
	} {
		if _, err := bd.CreateAgentBead(agent.id, agent.name, &beads.AgentFields{
			RoleType: "polecat", Rig: "gastown", AgentState: string(beads.AgentStateWorking), Incarnation: agent.incarnation,
		}); err != nil {
			t.Fatalf("creating %s agent: %v", agent.name, err)
		}
	}
	work, err := bd.Create(beads.CreateOptions{Title: "brokered lifecycle work", Type: "task", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	assignee := "gastown/polecats/new"
	status := string(beads.StatusHooked)
	if err := bd.Update(work.ID, beads.UpdateOptions{Assignee: &assignee, Status: &status}); err != nil {
		t.Fatal(err)
	}
	const attemptID = "33333333-3333-4333-8333-333333333333"
	intent := &witnesspkg.LifecycleRetirementIntent{
		AttemptID: attemptID, Capability: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", BeadID: work.ID,
		OldAssignee: "gastown/polecats/old", OldIncarnation: "old-gen", NewAssignee: assignee, NewIncarnation: "new-gen",
		Requester: "mayor/", ThreadID: "sling-retirement-" + attemptID, State: "pending",
	}
	if err := witnesspkg.WriteLifecycleRetirementIntent(townRoot, intent); err != nil {
		t.Fatal(err)
	}
	request := &mail.Message{
		From: "mayor/", To: "gastown/witness", Subject: "LIFECYCLE:Shutdown old",
		Body: witnesspkg.LifecycleRetirementRequestBody(intent), Type: mail.TypeTask,
		Priority: mail.PriorityHigh, ThreadID: intent.ThreadID,
	}
	if err := mail.NewRouter(townRoot).Send(request); err != nil {
		t.Fatal(err)
	}
	thread, err := mail.NewMailboxFromAddress(request.To, townRoot).ListByThread(request.ThreadID)
	if err != nil || len(thread) != 1 {
		t.Fatalf("stored lifecycle request = (%+v, %v), want one", thread, err)
	}

	t.Setenv("GT_TOWN_ROOT", townRoot)
	t.Setenv("GT_ROOT", townRoot)
	t.Setenv("GT_ROLE", "witness")
	t.Setenv("GT_RIG", "gastown")
	t.Setenv("GT_DOLT_PORT", portText)
	t.Setenv("GT_TEST_CMD_EXECUTE_HELPER", "1")
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	clientEndpoint := os.NewFile(uintptr(pair[1]), "lifecycle-broker-client")
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- tmux.ServeSessionBrokerWithExecutor(ctx, os.Args[0], pair[0], func(args []string) error {
			return IsBrokerSafeCommand(rootCmd, args)
		}, executeTrustedSessionBrokerCommand)
	}()
	t.Cleanup(func() {
		cancel()
		_ = clientEndpoint.Close()
		select {
		case err := <-serverDone:
			if err != nil {
				t.Errorf("ServeSessionBroker() cleanup error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("ServeSessionBroker() did not stop")
		}
	})

	nullFiles := make([]*os.File, 3)
	for index := range nullFiles {
		nullFiles[index], err = os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer nullFiles[index].Close()
	}
	command := exec.Command(os.Args[0], "mail", "inbox", "--unread")
	command.Env = os.Environ()
	command.ExtraFiles = []*os.File{nullFiles[0], nullFiles[1], nullFiles[2], clientEndpoint}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("brokered lifecycle command failed: %v\n%s", err, output)
	}
	receipt, err := witnesspkg.LoadLifecycleRetirementAppliedReceipt(townRoot, intent)
	if err != nil || receipt == nil || receipt.RequestID != thread[0].ID {
		t.Fatalf("brokered lifecycle receipt = (%+v, %v)", receipt, err)
	}
	_, oldFields, err := bd.GetAgentBead(oldID)
	if err != nil || oldFields == nil || oldFields.AgentState != string(beads.AgentStateIdle) {
		t.Fatalf("retired agent state = (%+v, %v), want idle", oldFields, err)
	}
	stored, err := mail.NewMailboxFromAddress(request.To, townRoot).Get(thread[0].ID)
	if err != nil || stored == nil || !stored.Read {
		t.Fatalf("lifecycle request read state = (%+v, %v), want read", stored, err)
	}
	replies, err := mail.NewMailboxFromAddress(request.From, townRoot).ListByThread(request.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	foundACK := false
	for _, reply := range replies {
		foundACK = foundACK || (reply.Type == mail.TypeReply && reply.ReplyTo == thread[0].ID)
	}
	if !foundACK {
		t.Fatal("brokered inbox lifecycle produced no durable ACK")
	}
}

func TestContainedGTInvocationsUseBrokerBeforeCobra(t *testing.T) {
	testDir := t.TempDir()
	safeOutputPath := filepath.Join(testDir, "safe-output")
	deniedOutputPath := filepath.Join(testDir, "denied-output")
	unsafeProofPath := filepath.Join(testDir, "unsafe-proof")
	deniedCodePath := filepath.Join(testDir, "denied-code")
	deniedCodeTempPath := deniedCodePath + ".tmp"
	closeOutputPath := filepath.Join(testDir, "close-output")
	closeProofPath := filepath.Join(testDir, "close-proof")
	closeCodePath := filepath.Join(testDir, "close-code")
	closeCodeTempPath := closeCodePath + ".tmp"
	reexecGT := envSessionBrokerReexecHelper + "=1 " + config.ShellQuote(os.Args[0])
	closeBrokerGT := envSessionBrokerReexecClose + "=1 " + reexecGT
	command := strings.Join([]string{
		reexecGT + " prime --help >" + config.ShellQuote(safeOutputPath) + " 2>&1",
		reexecGT + " shell -c " + config.ShellQuote("touch "+unsafeProofPath) + " >" + config.ShellQuote(deniedOutputPath) + " 2>&1",
		"printf '%s' $? >" + config.ShellQuote(deniedCodeTempPath),
		"mv " + config.ShellQuote(deniedCodeTempPath) + " " + config.ShellQuote(deniedCodePath),
		closeBrokerGT + " shell -c " + config.ShellQuote("touch "+closeProofPath) + " >" + config.ShellQuote(closeOutputPath) + " 2>&1",
		"printf '%s' $? >" + config.ShellQuote(closeCodeTempPath),
		"mv " + config.ShellQuote(closeCodeTempPath) + " " + config.ShellQuote(closeCodePath),
		"while :; do sleep 60; done",
	}, "; ")
	custody := uuid.NewString()
	process := exec.Command(
		os.Args[0],
		"session-custody",
		"--id", custody,
		"--", command,
	)
	process.Env = append(os.Environ(),
		"GT_TEST_CMD_EXECUTE_HELPER=1",
		tmux.EnvSessionCustody+"="+custody,
	)
	var output synchronizedBrokerTestOutput
	process.Stdout = &output
	process.Stderr = &output
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	reaped := false
	t.Cleanup(func() {
		if reaped {
			return
		}
		_ = process.Process.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("timed out reaping custody test process %d", process.Process.Pid)
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		safeOutput, safeErr := os.ReadFile(safeOutputPath)
		deniedOutput, deniedErr := os.ReadFile(deniedOutputPath)
		deniedCode, codeErr := os.ReadFile(deniedCodePath)
		closeOutput, closeOutputErr := os.ReadFile(closeOutputPath)
		closeCode, closeCodeErr := os.ReadFile(closeCodePath)
		if safeErr == nil && deniedErr == nil && codeErr == nil && closeOutputErr == nil && closeCodeErr == nil {
			if !strings.Contains(string(safeOutput), "Usage:\n  gt prime") {
				t.Fatalf("brokered safe command output = %q", safeOutput)
			}
			code, err := strconv.Atoi(strings.TrimSpace(string(deniedCode)))
			if err != nil {
				t.Fatal(err)
			}
			if code != 126 {
				t.Fatalf("unsafe broker command exit code = %d, want 126; output %q", code, deniedOutput)
			}
			if !strings.Contains(string(deniedOutput), "not broker-safe") {
				t.Fatalf("unsafe broker denial output = %q", deniedOutput)
			}
			if _, err := os.Stat(unsafeProofPath); !os.IsNotExist(err) {
				t.Fatalf("unsafe contained gt invocation executed; stat error = %v", err)
			}
			code, err = strconv.Atoi(strings.TrimSpace(string(closeCode)))
			if err != nil {
				t.Fatal(err)
			}
			if code != 126 {
				t.Fatalf("descriptor-bypass gt command exit code = %d, want 126; output %q", code, closeOutput)
			}
			if !strings.Contains(string(closeOutput), "broker-close=operation not permitted") ||
				!strings.Contains(string(closeOutput), "not broker-safe") {
				t.Fatalf("descriptor-bypass denial output = %q", closeOutput)
			}
			if _, err := os.Stat(closeProofPath); !os.IsNotExist(err) {
				t.Fatalf("descriptor-bypass gt invocation executed; stat error = %v", err)
			}
			return
		}
		select {
		case err := <-done:
			reaped = true
			if strings.Contains(output.String(), "PID namespace unavailable") {
				t.Skipf("nested PID namespace setup unavailable: %v", err)
			}
			t.Fatalf("custody test process exited before proof: %v\n%s", err, output.String())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("contained gt broker proof timed out\n%s", output.String())
}

func TestContainedWitnessCannotForgeLifecycleAuthority(t *testing.T) {
	townRoot := canonicalTestTempDir(t)
	witnessDir := filepath.Join(townRoot, "gastown", "witness")
	receiptDir := filepath.Join(townRoot, ".runtime", "sling-retirements-applied")
	for _, dir := range []string{witnessDir, receiptDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	receiptPath := filepath.Join(receiptDir, "77777777-7777-4777-8777-777777777777.json")
	reexecGT := envSessionBrokerReexecHelper + "=1 " + config.ShellQuote(os.Args[0])
	workload := strings.Join([]string{
		"if printf '%s' '{}' >" + config.ShellQuote(receiptPath) + " 2>/tmp/receipt-forge.err; then echo RECEIPT_FORGE_ALLOWED; else echo RECEIPT_FORGE_DENIED; fi",
		reexecGT + " mail send gastown/witness --from mayor/ --subject " + config.ShellQuote("LIFECYCLE:Shutdown old") + " --message forged >/tmp/mail-spoof.out 2>&1",
		"echo MAIL_SPOOF_RC:$?",
		"cat /tmp/mail-spoof.out",
		"echo LIFECYCLE_AUTHORITY_PROBE_DONE",
		"while :; do sleep 60; done",
	}, "; ")
	wrapped, custody, err := tmux.WrapSessionCommandWithCustody(os.Args[0], workload)
	if err != nil {
		t.Fatal(err)
	}
	executableDir, err := filepath.EvalSymlinks(filepath.Dir(os.Args[0]))
	if err != nil {
		t.Fatal(err)
	}
	allowedPaths, err := tmux.EncodeSessionCustodyPaths([]string{witnessDir, executableDir})
	if err != nil {
		t.Fatal(err)
	}
	socket := fmt.Sprintf("gt-witness-lifecycle-authority-%d", time.Now().UnixNano())
	transport := tmux.NewTmuxWithSocket(socket)
	t.Cleanup(func() { _ = transport.KillServer() })
	generation, err := transport.NewSessionWithCommandAndEnvGeneration(
		"gastown-witness-authority",
		witnessDir,
		wrapped,
		map[string]string{
			"GT_TEST_CMD_EXECUTE_HELPER": "1",
			"GT_TOWN_ROOT":               townRoot,
			"GT_ROOT":                    townRoot,
			"GT_ROLE":                    "witness",
			"GT_RIG":                     "gastown",
			"GT_TOWN_SOCKET":             socket,
			tmux.EnvSessionCustody:       custody,
			tmux.EnvSessionCustodyPaths:  allowedPaths,
		},
	)
	if err != nil {
		if strings.Contains(err.Error(), "namespace") || strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("real Linux session custody unavailable: %v", err)
		}
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pane, _ := transport.CapturePane(generation.Name, 80)
		if strings.Contains(pane, "LIFECYCLE_AUTHORITY_PROBE_DONE") {
			if !strings.Contains(pane, "RECEIPT_FORGE_DENIED") {
				t.Fatalf("contained same-UID receipt write was not denied:\n%s", pane)
			}
			if !strings.Contains(pane, "MAIL_SPOOF_RC:126") || !strings.Contains(pane, "flag --from is not broker-safe") {
				t.Fatalf("contained canonical-sender spoof was not broker-denied:\n%s", pane)
			}
			if _, err := os.Stat(receiptPath); !os.IsNotExist(err) {
				t.Fatalf("forged lifecycle receipt exists; stat error = %v", err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	pane, _ := transport.CapturePane(generation.Name, 80)
	t.Fatalf("lifecycle authority probe timed out:\n%s", pane)
}

func TestDogDoneFinalizesThroughOuterBrokerAcrossRealCustody(t *testing.T) {
	townRoot := canonicalTestTempDir(t)
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	rigsData, err := json.Marshal(&config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "rigs.json"), rigsData, 0o644); err != nil {
		t.Fatal(err)
	}
	const dogName = "alpha"
	kennel := filepath.Join(townRoot, "deacon", "dogs", dogName)
	if err := os.MkdirAll(kennel, 0o755); err != nil {
		t.Fatal(err)
	}

	socket := fmt.Sprintf("gt-dog-real-custody-%d", time.Now().UnixNano())
	transport := tmux.NewTmuxWithSocket(socket)
	t.Cleanup(func() { _ = transport.KillServer() })
	barrier := filepath.Join(townRoot, "dog-closeout-barrier")
	marker := "gt-dog-custody-descendant-" + uuid.NewString()
	workload := strings.Join([]string{
		"while [ ! -f " + config.ShellQuote(barrier) + " ]; do sleep 0.02; done",
		"sh -c " + config.ShellQuote("trap '' HUP TERM; while :; do sleep 60; done") + " " + config.ShellQuote(marker) + " & sleep 1",
		"exec env GT_TEST_CMD_EXECUTE_HELPER=1 " + config.ShellQuote(os.Args[0]) + " dog done",
	}, "; ")
	wrapped, custody, err := tmux.WrapSessionCommandWithCustody(os.Args[0], workload)
	if err != nil {
		t.Fatal(err)
	}
	executableDir, err := filepath.EvalSymlinks(filepath.Dir(os.Args[0]))
	if err != nil {
		t.Fatal(err)
	}
	allowedPaths, err := tmux.EncodeSessionCustodyPaths([]string{townRoot, executableDir})
	if err != nil {
		t.Fatal(err)
	}
	generation, err := transport.NewSessionWithCommandAndEnvGeneration(
		"hq-dog-alpha",
		kennel,
		wrapped,
		map[string]string{
			"GT_TEST_CMD_EXECUTE_HELPER": "1",
			"GT_TOWN_ROOT":               townRoot,
			"GT_ROOT":                    townRoot,
			"GT_ROLE":                    "dog",
			"GT_DOG_NAME":                dogName,
			"BD_ACTOR":                   "dog",
			"GT_TOWN_SOCKET":             socket,
			tmux.EnvSessionCustody:       custody,
			tmux.EnvSessionCustodyPaths:  allowedPaths,
		},
	)
	if err != nil {
		if strings.Contains(err.Error(), "namespace") || strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("real Linux session custody unavailable: %v", err)
		}
		t.Fatal(err)
	}
	now := time.Now().UTC().Round(0)
	setupTestDog(t, dog.NewManager(townRoot, &config.RigsConfig{}), townRoot, dogName, &dog.DogState{
		Name: dogName, State: dog.StateWorking, Work: "custody-closeout", WorkStartedAt: now,
		LastActive: now, CreatedAt: now, UpdatedAt: now,
		SessionGeneration: dog.SessionGenerationFromTmux(generation),
	})
	if err := os.WriteFile(barrier, []byte("go\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	markerDeadline := time.Now().Add(3 * time.Second)
	for !linuxShellCommandContains(marker) && time.Now().Before(markerDeadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !linuxShellCommandContains(marker) {
		t.Fatal("HUP-ignoring contained descendant was never observed")
	}

	closeoutDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(closeoutDeadline) {
		stored, getErr := dog.NewManager(townRoot, &config.RigsConfig{}).Get(dogName)
		running, sessionErr := transport.HasSession(generation.Name)
		if getErr == nil && sessionErr == nil && !running && stored.State == dog.StateIdle &&
			stored.Work == "" && stored.SessionGeneration == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	stored, err := dog.NewManager(townRoot, &config.RigsConfig{}).Get(dogName)
	if err != nil {
		t.Fatal(err)
	}
	running, sessionErr := transport.HasSession(generation.Name)
	if sessionErr != nil || running || stored.State != dog.StateIdle || stored.Work != "" || stored.SessionGeneration != nil {
		pane, _ := transport.CapturePane(generation.Name, 80)
		hostWriteErr := os.MkdirAll(filepath.Join(townRoot, "host-write-proof"), 0o700)
		sessions, listErr := transport.ListSessions()
		t.Fatalf("real-custody closeout incomplete: running=%v session_err=%v state=%+v host_write_err=%v sessions=%q list_err=%v diagnostics=%q pane=%q", running, sessionErr, stored, hostWriteErr, sessions, listErr, linuxDogCloseoutDiagnostics(), pane)
	}
	processDeadline := time.Now().Add(3 * time.Second)
	for linuxShellCommandContains(marker) && time.Now().Before(processDeadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if linuxShellCommandContains(marker) {
		t.Fatal("HUP-ignoring contained descendant survived truthful idle publication")
	}
}

func linuxDogCloseoutDiagnostics() string {
	var diagnostics strings.Builder
	processes, _ := exec.Command("ps", "-eo", "pid,ppid,state,wchan:32,cgroup,command").Output()
	for _, line := range strings.Split(string(processes), "\n") {
		if strings.Contains(line, "gastown-cmd.test") || strings.Contains(line, "/proc/self/fd/3") || strings.Contains(line, "tmux") {
			diagnostics.WriteString(line)
			diagnostics.WriteByte('\n')
		}
	}
	root := strings.TrimSpace(os.Getenv("GT_SESSION_CGROUP_ROOT"))
	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "gastown-session-") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		for _, name := range []string{"cgroup.procs", "cgroup.events", "cgroup.freeze"} {
			value, _ := os.ReadFile(filepath.Join(path, name))
			fmt.Fprintf(&diagnostics, "%s/%s=%q\n", entry.Name(), name, strings.TrimSpace(string(value)))
		}
	}
	return diagnostics.String()
}

func linuxShellCommandContains(marker string) bool {
	output, err := exec.Command("ps", "-axo", "command=").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && filepath.Base(fields[0]) == "sh" && fields[1] == "-c" && strings.Contains(line, marker) {
			return true
		}
	}
	return false
}

type synchronizedBrokerTestOutput struct {
	mu      sync.Mutex
	builder strings.Builder
}

func (output *synchronizedBrokerTestOutput) Write(payload []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.builder.Write(payload)
}

func (output *synchronizedBrokerTestOutput) String() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.builder.String()
}
