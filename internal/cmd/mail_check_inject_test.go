package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/delivery"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestRunMailCheckReceiptUsesManagedHookIdentityOutsideWorkspace(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(townRoot, "mayor"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	t.Setenv("GT_TOWN_ROOT", "")
	t.Setenv("GT_ROOT", townRoot)
	t.Setenv("GT_SESSION", "gt-test-codex-managed-hook")
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	if resolved, err := findMailWorkDir(); err != nil || resolved != townRoot {
		t.Fatalf("managed mail root: match=%t err=%v", resolved == townRoot, err)
	}

	deliveryID := "ndg-managed-hook-receipt"
	input := &hookInput{
		HookEventName: "UserPromptSubmit",
		Prompt:        delivery.ControlMessage(deliveryID, "private hook prompt"),
	}
	if err := recordCodexSubmissionReceiptFromHook(input); err != nil {
		t.Fatalf("recordCodexSubmissionReceiptFromHook: %v", err)
	}
	if _, err := os.Stat(delivery.ReceiptPath(townRoot, os.Getenv("GT_SESSION"))); err != nil {
		t.Fatalf("managed hook receipt path exists: %t", err == nil)
	}
	if _, ok, err := delivery.FindSubmittedAfter(townRoot, os.Getenv("GT_SESSION"), deliveryID, time.Time{}); err != nil || !ok {
		t.Fatalf("managed hook receipt: ok=%t err=%v", ok, err)
	}
}

func TestRecordCodexSubmissionReceiptFromHookInput(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-test-codex-hook"
	deliveryID := "ndg-hook-receipt"
	// This unit invokes the hook writer directly, without a transport attempt.
	// Strict post-attempt ordering is covered by delivery receipt tests.
	baseline := time.Time{}
	input := &hookInput{
		HookEventName: "UserPromptSubmit",
		Prompt:        delivery.ControlMessage(deliveryID, "private hook prompt"),
	}
	if err := recordCodexSubmissionReceipt(townRoot, session, input); err != nil {
		t.Fatalf("recordCodexSubmissionReceipt: %v", err)
	}
	receipt, ok, err := delivery.FindSubmittedAfter(townRoot, session, deliveryID, baseline)
	if err != nil || !ok || !receipt.Submitted || receipt.Runtime != "codex" {
		t.Fatalf("FindSubmittedAfter = %#v, %v, %v", receipt, ok, err)
	}
}

func TestInjectQueuedNudgePreservesDurableQueueWhileDeliveryLeaseContended(t *testing.T) {
	townRoot := t.TempDir()
	const session = "gt-test-mail-check-lease"
	const deliveryID = "ndg-mail-check-lease-contender"
	if err := nudge.Enqueue(townRoot, session, nudge.QueuedNudge{
		DeliveryID:      deliveryID,
		Sender:          "witness",
		Message:         "mail-check contender must not claim me",
		Priority:        nudge.PriorityUrgent,
		DurableUntilAck: true,
	}); err != nil {
		t.Fatal(err)
	}
	owner := tmux.NewTmuxWithSocket("gt-mail-check-lease-owner")
	releaseOwner, err := owner.AcquireNudgeLease(townRoot, session)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseOwner()

	var output, errorOutput bytes.Buffer
	injectQueuedNudgeForMailCheck(context.Background(), townRoot, session, &output, &errorOutput)
	if output.Len() != 0 {
		t.Fatalf("contended mail-check injected unowned output: %q", output.String())
	}
	queued, err := nudge.ListQueued(townRoot, session)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].DeliveryID != deliveryID || queued[0].Attempts != 0 {
		t.Fatalf("queue after contention = %#v, want untouched %s", queued, deliveryID)
	}
	if !strings.Contains(errorOutput.String(), "nudge delivery lease error") {
		t.Fatalf("contention diagnostic = %q", errorOutput.String())
	}

	releaseOwner()
	output.Reset()
	errorOutput.Reset()
	injectQueuedNudgeForMailCheck(context.Background(), townRoot, session, &output, &errorOutput)
	if !strings.Contains(output.String(), "mail-check contender must not claim me") {
		t.Fatalf("delivery after lease release missing queued message: %q", output.String())
	}
}

func TestMailCheckReportsPartialQueueConvergence(t *testing.T) {
	townRoot := t.TempDir()
	const session = "gt-test-partial-convergence"
	queueParent := filepath.Join(townRoot, ".runtime", "nudge_queue")
	if err := os.MkdirAll(queueParent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(queueParent, session), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}

	err := removeAcknowledgedWakeKinds(townRoot, session, "thread-partial")
	if err == nil || !strings.Contains(err.Error(), "reading nudge queue") {
		t.Fatalf("removeAcknowledgedWakeKinds error = %v, want queue read failure", err)
	}
}

func TestFormatInjectOutput(t *testing.T) {
	// Helper to build test messages with a given priority.
	msg := func(id, from, subject string, priority mail.Priority) *mail.Message {
		return &mail.Message{
			ID:       id,
			From:     from,
			Subject:  subject,
			Priority: priority,
		}
	}

	tests := []struct {
		name     string
		messages []*mail.Message
		// Strings that MUST appear in the output.
		wantContains []string
		// Strings that must NOT appear in the output.
		wantAbsent []string
	}{
		{
			name: "urgent only",
			messages: []*mail.Message{
				msg("m1", "mayor/", "Deploy now", mail.PriorityUrgent),
			},
			wantContains: []string{
				"<system-reminder>",
				"</system-reminder>",
				"URGENT: 1 urgent message(s)",
				"m1 from mayor/: Deploy now",
				"gt mail read <id>",
			},
			wantAbsent: []string{
				"high-priority",
				"additional",
			},
		},
		{
			name: "high only",
			messages: []*mail.Message{
				msg("m2", "gastown/wolf", "Review PR", mail.PriorityHigh),
			},
			wantContains: []string{
				"<system-reminder>",
				"1 high-priority message(s)",
				"m2 from gastown/wolf: Review PR",
				"process these messages",
				"before going idle",
			},
			wantAbsent: []string{
				"URGENT",
				"additional",
			},
		},
		{
			name: "normal only",
			messages: []*mail.Message{
				msg("m3", "gastown/toast", "FYI update", mail.PriorityNormal),
			},
			wantContains: []string{
				"<system-reminder>",
				"1 unread message(s)",
				"m3 from gastown/toast: FYI update",
				"check these messages",
				"before going idle",
			},
			wantAbsent: []string{
				"URGENT",
				"high-priority",
			},
		},
		{
			name: "low priority treated as normal tier",
			messages: []*mail.Message{
				msg("m4", "gastown/nux", "Backlog item", mail.PriorityLow),
			},
			wantContains: []string{
				"1 unread message(s)",
				"m4 from gastown/nux: Backlog item",
				"check these messages",
			},
			wantAbsent: []string{
				"URGENT",
				"high-priority",
			},
		},
		{
			name: "urgent + high: high listed separately",
			messages: []*mail.Message{
				msg("m5", "mayor/", "Emergency", mail.PriorityUrgent),
				msg("m6", "gastown/wolf", "Important review", mail.PriorityHigh),
			},
			wantContains: []string{
				"URGENT: 1 urgent message(s)",
				"m5 from mayor/: Emergency",
				"1 high-priority message(s)",
				"m6 from gastown/wolf: Important review",
				"process before going idle",
				"gt mail read <id>",
			},
			wantAbsent: []string{
				// High-priority should NOT be folded into a generic "non-urgent" count.
				"non-urgent",
			},
		},
		{
			name: "urgent + high + normal: all tiers shown",
			messages: []*mail.Message{
				msg("m7", "mayor/", "Fire", mail.PriorityUrgent),
				msg("m8", "gastown/wolf", "Review ASAP", mail.PriorityHigh),
				msg("m9", "gastown/toast", "Newsletter", mail.PriorityNormal),
			},
			wantContains: []string{
				"URGENT: 1 urgent message(s)",
				"m7 from mayor/: Fire",
				"1 high-priority message(s)",
				"m8 from gastown/wolf: Review ASAP",
				"1 additional message(s)",
			},
			wantAbsent: []string{
				"normal-priority",
				"non-urgent",
			},
		},
		{
			name: "urgent + normal (no high): normal shown as additional",
			messages: []*mail.Message{
				msg("m10", "mayor/", "Alert", mail.PriorityUrgent),
				msg("m11", "gastown/nux", "Low item", mail.PriorityLow),
				msg("m12", "gastown/toast", "Info", mail.PriorityNormal),
			},
			wantContains: []string{
				"URGENT: 1 urgent message(s)",
				"2 additional message(s)",
			},
			wantAbsent: []string{
				"high-priority",
				"normal-priority",
			},
		},
		{
			name: "high + normal: normal shown as additional",
			messages: []*mail.Message{
				msg("m13", "gastown/wolf", "Review", mail.PriorityHigh),
				msg("m14", "gastown/toast", "FYI", mail.PriorityNormal),
				msg("m15", "gastown/nux", "Backlog", mail.PriorityLow),
			},
			wantContains: []string{
				"1 high-priority message(s)",
				"2 additional message(s)",
			},
			wantAbsent: []string{
				"URGENT",
				"normal-priority",
			},
		},
		{
			name: "multiple urgent messages",
			messages: []*mail.Message{
				msg("m16", "mayor/", "Fire 1", mail.PriorityUrgent),
				msg("m17", "deacon/", "Fire 2", mail.PriorityUrgent),
			},
			wantContains: []string{
				"URGENT: 2 urgent message(s)",
				"m16 from mayor/: Fire 1",
				"m17 from deacon/: Fire 2",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := formatInjectOutput(tt.messages)

			for _, want := range tt.wantContains {
				if !strings.Contains(output, want) {
					t.Errorf("output should contain %q\n\nGot:\n%s", want, output)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(output, absent) {
					t.Errorf("output should NOT contain %q\n\nGot:\n%s", absent, output)
				}
			}
		})
	}
}
