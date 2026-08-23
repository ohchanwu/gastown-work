package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/delivery"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

func recordCodexSubmissionReceipt(townRoot, session string, input *hookInput) error {
	if input == nil || input.HookEventName != "UserPromptSubmit" {
		return nil
	}
	_, err := delivery.RecordPromptSubmitted(townRoot, session, "codex", input.Prompt, time.Now())
	return err
}

func recordCodexSubmissionReceiptFromHook(input *hookInput) error {
	if input == nil || input.HookEventName != "UserPromptSubmit" {
		return nil
	}
	townRoot, err := findMailWorkDir()
	if err != nil {
		return err
	}
	sessionName := os.Getenv("GT_SESSION")
	if sessionName == "" {
		sessionName = tmux.CurrentSessionName()
	}
	return recordCodexSubmissionReceipt(townRoot, sessionName, input)
}

func injectQueuedNudgeForMailCheck(ctx context.Context, workDir, sessionName string, output, errorOutput io.Writer) {
	transport := tmux.NewTmux()
	// A receipt-producing delivery may already own this lease while waiting for
	// the current hook to return. Probe briefly so hook output never deadlocks
	// the transport whose receipt it enables.
	leaseCtx, cancelLease := context.WithTimeout(ctx, 50*time.Millisecond)
	releaseLease, leaseErr := transport.AcquireNudgeLeaseContext(leaseCtx, workDir, sessionName)
	cancelLease()
	if leaseErr != nil {
		fmt.Fprintf(errorOutput, "gt mail check: nudge delivery lease error: %v\n", leaseErr)
		return
	}
	defer releaseLease()
	claim, claimErr := nudge.ClaimDue(workDir, sessionName)
	if claimErr != nil {
		fmt.Fprintf(errorOutput, "gt mail check: nudge queue claim error: %v\n", claimErr)
		return
	}
	if claim == nil {
		return
	}
	fmt.Fprint(output, nudge.FormatForInjection([]nudge.QueuedNudge{claim.Nudge}))
	// Hook stdout is context, not runtime acceptance. Preserve the queue item
	// until a later transport obtains a real receipt.
	if nackErr := claim.Nack("hook-output-unverified", nudge.NextRetry(claim.Nudge.Attempts)); nackErr != nil {
		fmt.Fprintf(errorOutput, "gt mail check: nudge queue nack error: %v\n", nackErr)
	}
}

func runMailCheck(cmd *cobra.Command, args []string) error {
	if mailCheckInject {
		input := readStdinJSON()
		if err := recordCodexSubmissionReceiptFromHook(input); err != nil {
			fmt.Fprintf(os.Stderr, "gt mail check: delivery receipt error: %v\n", err)
		}
	}

	// Determine which inbox (priority: --identity flag, auto-detect)
	address := ""
	if mailCheckIdentity != "" {
		address = mailCheckIdentity
	} else {
		address = detectSender()
	}

	// All mail uses town beads (two-level architecture)
	workDir, err := findMailWorkDir()
	if err != nil {
		if mailCheckInject {
			fmt.Fprintf(os.Stderr, "gt mail check: workspace lookup failed: %v\n", err)
			return nil
		}
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Get mailbox
	router := mail.NewRouter(workDir)
	mailbox, err := router.GetMailbox(address)
	if err != nil {
		if mailCheckInject {
			fmt.Fprintf(os.Stderr, "gt mail check: mailbox error for %s: %v\n", address, err)
			return nil
		}
		return fmt.Errorf("getting mailbox: %w", err)
	}

	// Load the inbox once. The inject path needs unread messages later, and
	// calling Count() followed by ListUnread() doubles bd/Dolt reads.
	messages, _, unread, err := loadInboxSnapshot(mailbox, false, false)
	if err != nil {
		if mailCheckInject {
			fmt.Fprintf(os.Stderr, "gt mail check: inbox load error for %s: %v\n", address, err)
			return nil
		}
		return fmt.Errorf("loading inbox: %w", err)
	}

	// JSON output
	if mailCheckJSON {
		result := map[string]interface{}{
			"address": address,
			"unread":  unread,
			"has_new": unread > 0,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	// Inject mode: notify agent of mail with priority-appropriate framing.
	// Three tiers: urgent interrupts immediately, high-priority is processed
	// at the next task boundary, normal/low is informational but still
	// checked before going idle (prevents mail from sitting unread).
	if mailCheckInject {
		sessionName := tmux.CurrentSessionName()
		// Agent-side E-stop check (defense-in-depth).
		// If an E-stop is active (town-wide or per-rig), inject a system reminder
		// telling the agent to checkpoint and wait. This catches agents that
		// survived the SIGTSTP freeze.
		if townRoot, twErr := workspace.FindFromCwd(); twErr == nil {
			rigName := os.Getenv("GT_RIG")
			if estop.IsActive(townRoot) || (rigName != "" && estop.IsRigActive(townRoot, rigName)) {
				fmt.Print("<system-reminder>\n")
				fmt.Print("EMERGENCY STOP ACTIVE. All work is paused.\n")
				fmt.Print("Do NOT start new tasks or tool calls. Checkpoint your current state\n")
				fmt.Print("(save progress notes) and wait for the overseer to run 'gt thaw'.\n")
				fmt.Print("This is a system-level pause — it may be due to infrastructure failure,\n")
				fmt.Print("maintenance, or the operator traveling.\n")
				fmt.Print("</system-reminder>\n")
			}
		}

		if unread > 0 {
			messages = filterUnreadMessages(messages)
			fmt.Print(formatInjectOutput(messages))
			// Ack after output so message is delivered before being marked acked.
			if ackErr := mailbox.AcknowledgeDeliveries(address, messages); ackErr != nil {
				fmt.Fprintf(os.Stderr, "gt mail check: delivery ack update failed for %s: %v\n", address, ackErr)
			} else if sessionName != "" {
				for _, msg := range messages {
					_, _ = nudge.RemoveKindByThread(workDir, sessionName, "mail", msg.ThreadID)
					_, _ = nudge.RemoveKindByThread(workDir, sessionName, "escalation", msg.ThreadID)
				}
			}
		}

		// Also drain queued nudges (from --mode=queue or --mode=wait-idle fallback).
		// The nudge queue is per-session; detect our session name.
		if sessionName != "" {
			injectQueuedNudgeForMailCheck(cmd.Context(), workDir, sessionName, os.Stdout, os.Stderr)
		}

		return nil
	}

	// Normal mode
	if unread > 0 {
		fmt.Printf("%s %d unread message(s)\n", style.Bold.Render("📬"), unread)
		return NewSilentExit(0)
	}
	fmt.Println("No new mail")
	return NewSilentExit(1)
}

// formatInjectOutput builds the system-reminder text for inject mode.
// It separates messages into three tiers (urgent, high, normal/low) and
// formats them with priority-appropriate framing for the agent.
func formatInjectOutput(messages []*mail.Message) string {
	var urgent, high, normal []*mail.Message
	for _, msg := range messages {
		switch msg.Priority {
		case mail.PriorityUrgent:
			urgent = append(urgent, msg)
		case mail.PriorityHigh:
			high = append(high, msg)
		default:
			normal = append(normal, msg)
		}
	}

	var b strings.Builder

	if len(urgent) > 0 {
		// Urgent mail: interrupt — agent should stop and read.
		b.WriteString("<system-reminder>\n")
		fmt.Fprintf(&b, "URGENT: %d urgent message(s) require immediate attention.\n\n", len(urgent))
		for _, msg := range urgent {
			fmt.Fprintf(&b, "- %s from %s: %s\n", msg.ID, msg.From, msg.Subject)
		}
		// Show high-priority messages separately so their "process before idle"
		// framing is preserved even when urgent messages are present.
		if len(high) > 0 {
			fmt.Fprintf(&b, "\nAlso %d high-priority message(s) — process before going idle:\n", len(high))
			for _, msg := range high {
				fmt.Fprintf(&b, "- %s from %s: %s\n", msg.ID, msg.From, msg.Subject)
			}
		}
		if len(normal) > 0 {
			fmt.Fprintf(&b, "\n(Plus %d additional message(s) — check after current task.)\n", len(normal))
		}
		b.WriteString("\nRun 'gt mail read <id>' to read urgent messages.\n")
		b.WriteString("</system-reminder>\n")
	} else if len(high) > 0 {
		// High-priority mail: don't interrupt, but process promptly at task boundary.
		b.WriteString("<system-reminder>\n")
		fmt.Fprintf(&b, "You have %d high-priority message(s) in your inbox.\n\n", len(high))
		for _, msg := range high {
			fmt.Fprintf(&b, "- %s from %s: %s\n", msg.ID, msg.From, msg.Subject)
		}
		if len(normal) > 0 {
			fmt.Fprintf(&b, "\n(Plus %d additional message(s).)\n", len(normal))
		}
		b.WriteString("\nContinue your current task. When it completes, process these messages\n")
		b.WriteString("before going idle: 'gt mail inbox'\n")
		b.WriteString("</system-reminder>\n")
	} else {
		// Normal/low mail: informational, process at next task boundary.
		b.WriteString("<system-reminder>\n")
		fmt.Fprintf(&b, "You have %d unread message(s) in your inbox.\n\n", len(normal))
		for _, msg := range normal {
			fmt.Fprintf(&b, "- %s from %s: %s\n", msg.ID, msg.From, msg.Subject)
		}
		b.WriteString("\nContinue your current task. When it completes, check these messages\n")
		b.WriteString("before going idle: 'gt mail inbox'\n")
		b.WriteString("</system-reminder>\n")
	}

	return b.String()
}
