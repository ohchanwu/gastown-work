package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/witness"
)

func TestWitnessRestartAgentFlag(t *testing.T) {
	flag := witnessRestartCmd.Flags().Lookup("agent")
	if flag == nil {
		t.Fatal("expected witness restart to define --agent flag")
	}
	if flag.DefValue != "" {
		t.Errorf("expected default agent override to be empty, got %q", flag.DefValue)
	}
	if !strings.Contains(flag.Usage, "overrides town default") {
		t.Errorf("expected --agent usage to mention overrides town default, got %q", flag.Usage)
	}
}

func TestWitnessStartAgentFlag(t *testing.T) {
	flag := witnessStartCmd.Flags().Lookup("agent")
	if flag == nil {
		t.Fatal("expected witness start to define --agent flag")
	}
	if flag.DefValue != "" {
		t.Errorf("expected default agent override to be empty, got %q", flag.DefValue)
	}
	if !strings.Contains(flag.Usage, "overrides town default") {
		t.Errorf("expected --agent usage to mention overrides town default, got %q", flag.Usage)
	}
}

func TestWitnessStartForegroundFlagHidden(t *testing.T) {
	flag := witnessStartCmd.Flags().Lookup("foreground")
	if flag == nil {
		t.Fatal("expected hidden compatibility --foreground flag")
	}
	if !flag.Hidden {
		t.Fatal("expected --foreground to be hidden")
	}
	if strings.Contains(witnessStartCmd.Long, "--foreground") {
		t.Fatalf("witness start help should not advertise --foreground:\n%s", witnessStartCmd.Long)
	}
}

func TestWitnessHandleLifecycleCommandIsRoutable(t *testing.T) {
	command, _, err := rootCmd.Find([]string{"witness", "handle-lifecycle"})
	if err != nil || command == nil || command.Name() != "handle-lifecycle" {
		t.Fatalf("witness lifecycle consumer route = (%v, %v), want exact command", command, err)
	}
}

func TestWitnessHandleLifecycleRejectsDirectCobraEvenWithWorkerEnvironment(t *testing.T) {
	t.Setenv(tmux.EnvSessionBrokerWorker, "1")
	err := runWitnessHandleLifecycle(nil, []string{"gastown", "hq-forged"})
	if err == nil || !strings.Contains(err.Error(), "live session broker") {
		t.Fatalf("direct lifecycle command error = %v, want live-broker rejection", err)
	}
}

func TestDispatchWitnessLifecycleMessagesQuarantinesRejectedAndContinues(t *testing.T) {
	messages := []*mail.Message{
		{ID: "bad", Subject: "LIFECYCLE:Shutdown old"},
		{ID: "valid", Subject: "LIFECYCLE:Shutdown old"},
		{ID: "old-bad", Subject: "LIFECYCLE:Shutdown old", Labels: []string{mail.LifecycleRejectedLabel}},
		{ID: "ordinary", Subject: "hello"},
	}
	var executed, quarantined []string
	err := dispatchWitnessLifecycleMessagesContext(
		context.Background(), messages,
		func(_ context.Context, id string) error {
			quarantined = append(quarantined, id)
			return nil
		},
		func(_ context.Context, msg *mail.Message) error {
			executed = append(executed, msg.ID)
			if msg.ID == "bad" {
				return fmt.Errorf("%w: forged request", witness.ErrLifecycleRequestRejected)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(executed, ",") != "bad,valid" {
		t.Fatalf("executed = %v, want rejected then valid", executed)
	}
	if strings.Join(quarantined, ",") != "bad" {
		t.Fatalf("quarantined = %v, want bad", quarantined)
	}
	if !messages[0].Read || !messages[1].Read || messages[2].Read || messages[3].Read {
		t.Fatalf("read state after dispatch = %v %v %v %v", messages[0].Read, messages[1].Read, messages[2].Read, messages[3].Read)
	}
}

func TestDispatchWitnessLifecycleMessagesQuarantinesMalformedIntentAndContinues(t *testing.T) {
	townRoot := t.TempDir()
	attemptID := "44444444-4444-4444-8444-444444444444"
	intentDir := filepath.Join(townRoot, ".runtime", "sling-retirements")
	if err := os.MkdirAll(intentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(intentDir, attemptID+".json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	messages := []*mail.Message{
		{ID: "poison", Subject: "LIFECYCLE:Shutdown old"},
		{ID: "valid", Subject: "LIFECYCLE:Shutdown old"},
	}
	var executed, quarantined []string
	err := dispatchWitnessLifecycleMessagesContext(
		context.Background(), messages,
		func(_ context.Context, id string) error {
			quarantined = append(quarantined, id)
			return nil
		},
		func(_ context.Context, msg *mail.Message) error {
			executed = append(executed, msg.ID)
			if msg.ID == "poison" {
				_, err := witness.LoadLifecycleRetirementIntent(townRoot, attemptID)
				return err
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(executed, ",") != "poison,valid" || strings.Join(quarantined, ",") != "poison" {
		t.Fatalf("malformed dispatch executed=%v quarantined=%v", executed, quarantined)
	}
}

func TestDispatchWitnessLifecycleMessagesLeavesTransientFailureRetryable(t *testing.T) {
	message := &mail.Message{ID: "retry", Subject: "LIFECYCLE:Shutdown old"}
	quarantined := 0
	executions := 0
	execute := func(context.Context, *mail.Message) error {
		executions++
		if executions == 1 {
			return errors.New("storing lifecycle acceptance: one-shot failure")
		}
		return nil
	}
	quarantine := func(context.Context, string) error { quarantined++; return nil }

	if err := dispatchWitnessLifecycleMessagesContext(context.Background(), []*mail.Message{message}, quarantine, execute); err == nil {
		t.Fatal("transient lifecycle failure was swallowed")
	}
	if message.Read || quarantined != 0 {
		t.Fatalf("transient lifecycle failure read=%v quarantined=%d, want retryable", message.Read, quarantined)
	}
	if err := dispatchWitnessLifecycleMessagesContext(context.Background(), []*mail.Message{message}, quarantine, execute); err != nil {
		t.Fatal(err)
	}
	if !message.Read || executions != 2 || quarantined != 0 {
		t.Fatalf("lifecycle retry read=%v executions=%d quarantined=%d", message.Read, executions, quarantined)
	}
}

func TestDispatchWitnessLifecycleMessagesReplaysAppliedReceiptAfterACKFailure(t *testing.T) {
	message := &mail.Message{ID: "replay", Subject: "LIFECYCLE:Shutdown old"}
	receiptPath := filepath.Join(t.TempDir(), "applied-receipt")
	applyCalls, sendCalls, quarantined := 0, 0, 0
	execute := func(context.Context, *mail.Message) error {
		if _, err := os.Stat(receiptPath); os.IsNotExist(err) {
			applyCalls++
			if err := os.WriteFile(receiptPath, []byte("applied"), 0o600); err != nil {
				return err
			}
		}
		sendCalls++
		if sendCalls == 1 {
			return errors.New("one-shot ACK storage failure")
		}
		return nil
	}
	quarantine := func(context.Context, string) error { quarantined++; return nil }

	if err := dispatchWitnessLifecycleMessagesContext(context.Background(), []*mail.Message{message}, quarantine, execute); err == nil {
		t.Fatal("ACK failure was swallowed")
	}
	if message.Read || quarantined != 0 {
		t.Fatalf("ACK failure read=%v quarantined=%d, want retryable", message.Read, quarantined)
	}
	if _, err := os.Stat(receiptPath); err != nil {
		t.Fatalf("applied receipt missing after ACK failure: %v", err)
	}
	if err := dispatchWitnessLifecycleMessagesContext(context.Background(), []*mail.Message{message}, quarantine, execute); err != nil {
		t.Fatal(err)
	}
	if !message.Read || applyCalls != 1 || sendCalls != 2 || quarantined != 0 {
		t.Fatalf("replay read=%v apply=%d send=%d quarantined=%d", message.Read, applyCalls, sendCalls, quarantined)
	}
}
