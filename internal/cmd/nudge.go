package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/mayor"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/telemetry"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

func hasACPSessionByName(townRoot, sessionName string) bool {
	if townRoot == "" {
		return false
	}

	// Currently only the Mayor supports ACP.
	if sessionName == session.MayorSessionName() {
		return mayor.IsACPActive(townRoot)
	}

	return false
}

var (
	nudgeMessageFlag    string
	nudgeForceFlag      bool
	nudgeStdinFlag      bool
	nudgeIfFreshFlag    bool
	nudgeModeFlag       string
	nudgePriorityFlag   string
	nudgeSourceMailFlag string
)

type nudgeSource struct {
	ID        string
	Kind      string
	ThreadID  string
	QueueKind string
}

func newNudgeDeliveryRecord(sender, message, priority string, source nudgeSource) nudge.QueuedNudge {
	return nudge.QueuedNudge{
		Sender:     sender,
		Message:    message,
		Priority:   priority,
		Kind:       source.QueueKind,
		ThreadID:   source.ThreadID,
		SourceID:   source.ID,
		SourceKind: source.Kind,
	}
}

func validateNudgeSourceMessage(msg *mail.Message, target string) (nudgeSource, error) {
	if msg == nil || mail.AddressToIdentity(msg.To) != mail.AddressToIdentity(target) {
		return nudgeSource{}, fmt.Errorf("source mail recipient does not match nudge target %q", target)
	}
	if msg.ThreadID == "" {
		return nudgeSource{}, fmt.Errorf("source mail has no thread ID")
	}
	sourceID, err := mail.WakeSourceForMessage(msg)
	if err != nil {
		return nudgeSource{}, err
	}
	queueKind := "mail"
	if msg.Type == mail.TypeEscalation {
		queueKind = "escalation"
	}
	return nudgeSource{
		ID:        sourceID,
		Kind:      nudge.SourceKindMail,
		ThreadID:  msg.ThreadID,
		QueueKind: queueKind,
	}, nil
}

func resolveNudgeSourceMail(townRoot, target, messageID string) (nudgeSource, error) {
	router := mail.NewRouter(townRoot)
	mailbox, err := router.GetMailbox(target)
	if err != nil {
		return nudgeSource{}, fmt.Errorf("opening source mailbox: %w", err)
	}
	msg, err := mailbox.Get(messageID)
	if err != nil {
		return nudgeSource{}, fmt.Errorf("source mail %q is not readable by recipient %q: %w", messageID, target, err)
	}
	return validateNudgeSourceMessage(msg, target)
}

// Nudge delivery modes.
const (
	// NudgeModeImmediate sends directly via tmux send-keys (current behavior).
	// This interrupts in-flight work but guarantees immediate delivery.
	NudgeModeImmediate = "immediate"
	// NudgeModeQueue writes to a file queue; agent picks up via hook at next
	// turn boundary. Zero interruption but delivery depends on agent turn frequency.
	NudgeModeQueue = "queue"
	// NudgeModeWaitIdle waits for the agent to become idle (prompt visible),
	// then delivers directly. Falls back to queue on timeout. Best of both worlds.
	NudgeModeWaitIdle = "wait-idle"
)

type nudgeDeliveryResult string

const (
	nudgeDeliveryQueued     nudgeDeliveryResult = "queued"
	nudgeDeliverySubmitted  nudgeDeliveryResult = "submitted"
	nudgeDeliverySuppressed nudgeDeliveryResult = "suppressed"
)

func submissionReceiptMatches(receipt tmux.SubmissionReceipt, sessionName, deliveryID string, baseline time.Time) bool {
	return deliveryID != "" && receipt.Typed && receipt.Submitted && receipt.Session == sessionName && receipt.DeliveryID == deliveryID && !receipt.SubmittedAt.Before(baseline)
}

func fallbackUrgentDelivery(townRoot, sessionName string, queued nudge.QueuedNudge, baseline time.Time, receipt tmux.SubmissionReceipt, deliveryErr error) (nudgeDeliveryResult, error) {
	matched := submissionReceiptMatches(receipt, sessionName, queued.DeliveryID, baseline)
	if deliveryErr == nil && matched {
		return nudgeDeliverySubmitted, nil
	}
	if deliveryErr == nil {
		deliveryErr = tmux.ErrSubmitNotVerified
	}
	if queued.Priority != nudge.PriorityUrgent || townRoot == "" || matched {
		return "", deliveryErr
	}
	queued.DurableUntilAck = true
	if err := nudge.Enqueue(townRoot, sessionName, queued); err != nil {
		return "", fmt.Errorf("urgent queue fallback failed: %v (original: %w)", err, deliveryErr)
	}
	return nudgeDeliveryQueued, nil
}

// deliverDirectNudge revalidates a durable source immediately before prompt
// injection. Terminal sources are suppressed; unreadable sources stay queued
// for retry instead of being delivered on stale information.
func deliverDirectNudge(t *tmux.Tmux, townRoot, sessionName, message string, queued nudge.QueuedNudge, opts tmux.NudgeOpts) (tmux.SubmissionReceipt, nudgeDeliveryResult, error) {
	eligibility, eligibilityErr := mail.CheckWakeEligibility(townRoot, queued)
	switch eligibility {
	case mail.WakeTerminal:
		return tmux.SubmissionReceipt{}, nudgeDeliverySuppressed, nil
	case mail.WakeUnknown:
		queued.DurableUntilAck = true
		if _, err := nudge.EnqueueUniqueBySource(townRoot, sessionName, queued); err != nil {
			return tmux.SubmissionReceipt{}, "", errors.Join(eligibilityErr, fmt.Errorf("retaining source-bound nudge: %w", err))
		}
		fmt.Fprintf(os.Stderr, "nudge source eligibility unknown; queued durably for %s: %v\n", sessionName, eligibilityErr)
		return tmux.SubmissionReceipt{}, nudgeDeliveryQueued, nil
	}

	if logPath := os.Getenv("GT_TEST_NUDGE_LOG"); logPath != "" {
		entry := fmt.Sprintf("nudge:%s::%s\n", sessionName, message)
		if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
			_, _ = f.WriteString(entry)
			_ = f.Close()
		}
		now := time.Now()
		return tmux.SubmissionReceipt{
			Session: sessionName, DeliveryID: opts.DeliveryID, Runtime: "test",
			Typed: true, Submitted: true, TypedAt: now, SubmittedAt: now,
		}, nudgeDeliverySubmitted, nil
	}

	receipt, err := t.NudgeSessionWithReceipt(sessionName, message, opts)
	return receipt, "", err
}

func init() {
	rootCmd.AddCommand(nudgeCmd)
	nudgeCmd.Flags().StringVarP(&nudgeMessageFlag, "message", "m", "", "Message to send")
	nudgeCmd.Flags().BoolVarP(&nudgeForceFlag, "force", "f", false, "Send even if target has DND enabled")
	nudgeCmd.Flags().BoolVar(&nudgeStdinFlag, "stdin", false, "Read message from stdin (avoids shell quoting issues)")
	nudgeCmd.Flags().BoolVar(&nudgeIfFreshFlag, "if-fresh", false, "Only send if caller's tmux session is <60s old (suppresses compaction nudges)")
	nudgeCmd.Flags().StringVar(&nudgeModeFlag, "mode", NudgeModeWaitIdle, "Delivery mode: wait-idle (default), queue, or immediate")
	nudgeCmd.Flags().StringVar(&nudgePriorityFlag, "priority", nudge.PriorityNormal, "Queue priority: normal (default) or urgent")
	nudgeCmd.Flags().StringVar(&nudgeSourceMailFlag, "source-mail", "", "Bind this fallback to one durable mail message")
}

var nudgeCmd = &cobra.Command{
	Use:     "nudge <target> [message]",
	GroupID: GroupComm,
	Annotations: map[string]string{
		AnnotationPolecatSafe:     "true",
		BrokerSafeAnnotation:      "true",
		brokerSafeArgsAnnotation:  brokerSafeArgsCobra,
		brokerSafeFlagsAnnotation: "message,stdin,if-fresh,mode,priority,source-mail",
	},
	Short: "Send a synchronous message to any Gas Town worker",
	Long: `Universal messaging API for Gas Town worker-to-worker communication.

Delivers a message to any worker's Claude Code session: polecats, crew,
witness, refinery, mayor, or deacon.

Delivery modes (--mode):
  wait-idle  Wait for agent to become idle (prompt visible), then deliver
             directly. Falls back to queue on timeout. If both idle-wait and
             queue fail, falls back to immediate delivery as a last resort.
             This is the default — it avoids interrupting active tool calls.
  queue      Write to a file queue; agent picks up via hook at next turn
             boundary. Zero interruption. Use for non-urgent coordination.
  immediate  Send directly via tmux send-keys. Interrupts in-flight work
             but guarantees immediate delivery. Use only when you need to
             break through (e.g., stuck agent, emergency).

Queue and wait-idle modes require a drain mechanism. Claude agents drain
via UserPromptSubmit hook; other agents use a background nudge-poller
that periodically drains and injects via tmux. If neither is available,
use --mode=immediate.

This is the ONLY way to send messages to Claude sessions.
Do not use raw tmux send-keys elsewhere.

Role shortcuts (expand to session names):
  mayor     Maps to gt-mayor
  deacon    Maps to gt-deacon
  witness   Maps to gt-<rig>-witness (uses current rig)
  refinery  Maps to gt-<rig>-refinery (uses current rig)

Channel syntax:
  channel:<name>  Nudges all members of a named channel defined in
                  <town-root>/config/messaging.json under "nudge_channels".
                  Patterns like "gastown/polecats/*" are expanded.

DND (Do Not Disturb):
  If the target has DND enabled (gt dnd on), the nudge is skipped.
  Use --force to override DND and send anyway.

Examples:
  gt nudge greenplace/furiosa "Check your mail and start working"
  gt nudge greenplace/alpha -m "What's your status?"
  gt nudge mayor "Status update requested"
  gt nudge witness "Check polecat health"
  gt nudge deacon session-started
  gt nudge channel:workers "New priority work available"

  # Use --stdin for messages with special characters or formatting:
  gt nudge gastown/alpha --stdin <<'EOF'
  Status update:
  - Task 1: complete
  - Task 2: in progress
  EOF`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runNudge,
}

// ifFreshMaxAge is the maximum session age for --if-fresh to allow a nudge.
// Sessions older than this are considered compaction/clear restarts, not new sessions.
const ifFreshMaxAge = 60 * time.Second

// waitIdleTimeout is how long --mode=wait-idle will poll before falling back to queue.
// This is a var (not const) so tests can override it to avoid 15s waits.
var waitIdleTimeout = 15 * time.Second

// idleWatcherTimeout is how long the background idle watcher polls after
// queuing a nudge. If the agent becomes idle within this window, the watcher
// drains the queue and delivers directly. This covers the gap where an agent
// finishes work after WaitForIdle's timeout but before anyone sends new input
// (so UserPromptSubmit never fires and the queue never drains).
// Var so tests can override.
var idleWatcherTimeout = 60 * time.Second

// idleWatcherPollInterval is how often the background watcher checks for idle.
// Var so tests can override.
var idleWatcherPollInterval = 1 * time.Second

// idleWatcherProbeTimeout leaves enough time for WaitForIdle's one-second
// stability proof plus tmux command overhead without slowing the outer cadence.
const idleWatcherProbeTimeout = 2 * time.Second

// deliverNudge routes a nudge based on the --mode flag.
// For "immediate" mode: sends directly via tmux (current behavior).
// For "queue" mode: writes to the nudge queue for cooperative delivery.
// For "wait-idle" mode: waits for idle, then delivers or falls back to queue.
func deliverNudge(t *tmux.Tmux, sessionName, message, sender string, source nudgeSource) (nudgeDeliveryResult, error) {
	// Test hook: when GT_TEST_NUDGE_LOG is set, log the nudge instead of
	// delivering through real tmux/queue transport. Prevents test-suite
	// runs from delivering "test" messages to live agents (mayor reported
	// recurring synthetic nudges traced to nudge_test.go invocations).
	// Mirrors the pattern in sling_helpers.go's nudgeWitness/nudgeRefinery.
	if logPath := os.Getenv("GT_TEST_NUDGE_LOG"); logPath != "" && source.ID == "" {
		entry := fmt.Sprintf("nudge:%s:%s:%s\n", sessionName, sender, message)
		if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
			_, _ = f.WriteString(entry)
			_ = f.Close()
		}
		return nudgeDeliverySubmitted, nil
	}

	townRoot, _ := workspace.FindFromCwd()

	// Use the requested mode, but force queue mode for ACP sessions.
	// ACP agents don't have tmux panes to send-keys to.
	mode := nudgeModeFlag
	if hasACPSessionByName(townRoot, sessionName) {
		mode = NudgeModeQueue
	}

	// For direct tmux delivery, prefix with sender attribution.
	// Queue-based delivery stores Sender as a separate field and
	// FormatForInjection adds the prefix, so we must NOT double-prefix.
	prefixedMessage := fmt.Sprintf("[from %s] %s", sender, message)

	switch mode {
	case NudgeModeQueue:
		if townRoot == "" {
			return "", fmt.Errorf("--mode=queue requires a Gas Town workspace")
		}
		err := nudge.Enqueue(townRoot, sessionName, newNudgeDeliveryRecord(sender, message, nudgePriorityFlag, source))
		return nudgeDeliveryQueued, err

	case NudgeModeWaitIdle:
		if townRoot == "" {
			// wait-idle needs workspace for queue fallback — fail explicitly
			// rather than silently degrading to immediate (destructive) delivery.
			return "", fmt.Errorf("--mode=wait-idle requires a Gas Town workspace")
		}
		// Check if the target agent supports prompt-based idle detection.
		// WaitForIdle uses Claude Code's prompt pattern (❯) and status bar (⏵⏵).
		// Non-Claude agents (Gemini, Codex, etc.) have no ReadyPromptPrefix,
		// so WaitForIdle produces false positives — it sees no busy indicator
		// and matches stale prompt characters in the pane buffer. (GH#gt-5ey3)
		// Degrade to queue mode for agents without prompt-based detection.
		if agentName, err := t.GetEnvironment(sessionName, "GT_AGENT"); err == nil && agentName != "" {
			preset := config.GetAgentPresetByName(agentName)
			if preset != nil && preset.ReadyPromptPrefix == "" {
				fmt.Fprintf(os.Stderr, "wait-idle: %s agent %q has no prompt detection, using queue mode\n", sessionName, agentName)
				queued := newNudgeDeliveryRecord(sender, message, nudgePriorityFlag, source)
				queued.DurableUntilAck = nudgePriorityFlag == nudge.PriorityUrgent
				if qErr := nudge.Enqueue(townRoot, sessionName, queued); qErr != nil {
					formatted := nudge.FormatForInjection([]nudge.QueuedNudge{queued})
					queued.DeliveryID = nudge.NewDeliveryID()
					baseline := time.Now()
					receipt, result, err := deliverDirectNudge(t, townRoot, sessionName, formatted, queued, tmux.NudgeOpts{TownRoot: townRoot, DeliveryID: queued.DeliveryID})
					if result != "" {
						return result, err
					}
					if err == nil && submissionReceiptMatches(receipt, sessionName, queued.DeliveryID, baseline) {
						return nudgeDeliverySubmitted, nil
					}
					if err == nil {
						err = tmux.ErrSubmitNotVerified
					}
					return "", err
				}
				// Ensure a nudge-poller is running so the queue actually drains.
				// The poller is normally started by gt crew start, but if the
				// session was started manually (or the poller crashed), queued
				// nudges sit undelivered forever. StartPoller is idempotent —
				// it no-ops if a poller is already alive for this session.
				if _, pollerErr := nudge.StartPoller(townRoot, sessionName); pollerErr != nil {
					fmt.Fprintf(os.Stderr, "wait-idle: could not start nudge poller for %s: %v\n", sessionName, pollerErr)
				}
				return nudgeDeliveryQueued, nil
			}
		}
		// Try to wait for idle
		err := t.WaitForIdle(sessionName, waitIdleTimeout)
		if err == nil {
			// Agent is idle — deliver directly. Format as system-reminder
			// so the agent processes it as a background notification rather
			// than a user interruption/correction.
			queued := newNudgeDeliveryRecord(sender, message, nudgePriorityFlag, source)
			queued.DeliveryID = nudge.NewDeliveryID()
			formatted := nudge.FormatForInjection([]nudge.QueuedNudge{queued})
			baseline := time.Now()
			receipt, result, deliverErr := deliverDirectNudge(t, townRoot, sessionName, formatted, queued, tmux.NudgeOpts{TownRoot: townRoot, DeliveryID: queued.DeliveryID})
			if result != "" {
				return result, deliverErr
			}
			if deliverErr == nil && submissionReceiptMatches(receipt, sessionName, queued.DeliveryID, baseline) {
				return nudgeDeliverySubmitted, nil
			}
			if queued.Priority == nudge.PriorityUrgent {
				return fallbackUrgentDelivery(townRoot, sessionName, queued, baseline, receipt, deliverErr)
			}
			if !errors.Is(deliverErr, tmux.ErrSubmitNotVerified) {
				return "", deliverErr
			}
			fmt.Fprintf(os.Stderr, "wait-idle: %v; queueing for %s\n", deliverErr, sessionName)
			if qErr := nudge.Enqueue(townRoot, sessionName, queued); qErr != nil {
				return "", fmt.Errorf("queue fallback after unverified submit failed: %v (original: %w)", qErr, deliverErr)
			}
			return nudgeDeliveryQueued, nil
		}
		// Terminal errors (session gone, no server) — propagate, don't queue.
		// Queueing a nudge for a dead session means it will never be delivered.
		if errors.Is(err, tmux.ErrSessionNotFound) || errors.Is(err, tmux.ErrNoServer) {
			if nudgePriorityFlag == nudge.PriorityUrgent {
				queued := newNudgeDeliveryRecord(sender, message, nudge.PriorityUrgent, source)
				queued.DeliveryID = nudge.NewDeliveryID()
				return fallbackUrgentDelivery(townRoot, sessionName, queued, time.Now(), tmux.SubmissionReceipt{}, err)
			}
			return "", fmt.Errorf("wait-idle: %w", err)
		}
		// Timeout (agent busy) — queue instead
		queued := newNudgeDeliveryRecord(sender, message, nudgePriorityFlag, source)
		queued.DurableUntilAck = nudgePriorityFlag == nudge.PriorityUrgent
		if qErr := nudge.Enqueue(townRoot, sessionName, queued); qErr != nil {
			// Queue failed — fall back to immediate as last resort.
			// Better to interrupt than lose the message entirely.
			fmt.Fprintf(os.Stderr, "Warning: queue fallback failed (%v), delivering immediately\n", qErr)
			// Still use FormatForInjection so the agent sees a consistent
			// <system-reminder> format regardless of delivery path.
			formatted := nudge.FormatForInjection([]nudge.QueuedNudge{queued})
			queued.DeliveryID = nudge.NewDeliveryID()
			baseline := time.Now()
			receipt, result, err := deliverDirectNudge(t, townRoot, sessionName, formatted, queued, tmux.NudgeOpts{TownRoot: townRoot, DeliveryID: queued.DeliveryID})
			if result != "" {
				return result, err
			}
			if err == nil && submissionReceiptMatches(receipt, sessionName, queued.DeliveryID, baseline) {
				return nudgeDeliverySubmitted, nil
			}
			if err == nil {
				err = tmux.ErrSubmitNotVerified
			}
			return "", err
		}
		// Run watcher synchronously: polls for idle over a longer window.
		// The UserPromptSubmit hook drains the queue on agent input, but an
		// idle agent receives no input — so queued nudges are lost without
		// this watcher. It exits on: delivery, session death, or timeout.
		// Must be synchronous (not a goroutine) because gt nudge is a CLI
		// command — the process exits after return, killing any goroutines.
		return watchAndDeliver(t, townRoot, sessionName), nil

	default: // NudgeModeImmediate
		opts := tmux.NudgeOpts{TownRoot: townRoot}
		// Check if the target agent uses Escape as cancel (e.g., Gemini CLI).
		// For these agents, skip the Escape keystroke to avoid canceling
		// in-flight generation. (GH#gt-wasn)
		if agentName, err := t.GetEnvironment(sessionName, "GT_AGENT"); err == nil && agentName != "" {
			if preset := config.GetAgentPresetByName(agentName); preset != nil && preset.EscapeCancelsRequest {
				opts.SkipEscape = true
			}
		}
		queued := newNudgeDeliveryRecord(sender, message, nudgePriorityFlag, source)
		queued.DeliveryID = nudge.NewDeliveryID()
		opts.DeliveryID = queued.DeliveryID
		baseline := time.Now()
		receipt, result, deliveryErr := deliverDirectNudge(t, townRoot, sessionName, prefixedMessage, queued, opts)
		if result != "" {
			return result, deliveryErr
		}
		fallbackResult, err := fallbackUrgentDelivery(townRoot, sessionName, queued, baseline, receipt, deliveryErr)
		if fallbackResult == nudgeDeliveryQueued {
			fmt.Fprintf(os.Stderr, "urgent nudge submission unverified; queued durably for %s\n", sessionName)
		}
		return fallbackResult, err
	}
}

// watchAndDeliver polls a session for idle state over idleWatcherTimeout.
// When the agent becomes idle, it drains the nudge queue and sends the
// formatted content directly via NudgeSession. This bypasses the
// UserPromptSubmit hook entirely — that hook does not fire for tmux
// send-keys input, so we cannot rely on it.
//
// This runs synchronously — gt nudge blocks until the watcher exits.
// Errors are logged to stderr rather than returned since delivery failure
// after successful queue write is non-fatal (queue persists for next drain).
//
// Exit conditions:
//   - Agent becomes idle: drain queue and deliver formatted content, exit.
//   - Queue is empty (someone else drained it): exit.
//   - Session disappears: exit (nothing to deliver to).
//   - Timeout: exit (queue stays for next input or watcher cycle).
func watchAndDeliver(t *tmux.Tmux, townRoot, sessionName string) nudgeDeliveryResult {
	fmt.Fprintf(os.Stderr, "Watching %s for idle (up to %s)...\n", sessionName, idleWatcherTimeout)
	deadline := time.Now().Add(idleWatcherTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(idleWatcherPollInterval)

		// If queue is already empty, someone else drained it.
		if nudge.QueueLen(townRoot, sessionName) == 0 {
			return nudgeDeliveryQueued
		}

		// Check if session still exists — no point watching a dead session.
		if exists, _ := t.HasSession(sessionName); !exists {
			return nudgeDeliveryQueued
		}

		// Use WaitForIdle instead of single-snapshot IsIdle to get the
		// consecutive-poll guard.
		// This avoids false positives during inter-tool-call gaps where
		// the prompt briefly appears while Claude Code is still working.
		if err := t.WaitForIdle(sessionName, idleWatcherProbeTimeout); err == nil {
			// The delivery lease is the ownership boundary for the entire
			// claim -> receipt -> settlement transaction. In particular, do not
			// let zombie cleanup acquire the lease after text was typed but before
			// its durable claim was acknowledged.
			leaseCtx, cancelLease := context.WithDeadline(context.Background(), deadline)
			claim, releaseLease, err := claimPollerNudgeWhenIdleContext(
				leaseCtx,
				false,
				func(context.Context) error { return nil },
				func(ctx context.Context) (func(), error) {
					return t.AcquireNudgeLeaseContext(ctx, townRoot, sessionName)
				},
				func() (*nudge.ClaimedNudge, error) { return nudge.ClaimDue(townRoot, sessionName) },
			)
			cancelLease()
			if err != nil || claim == nil {
				return nudgeDeliveryQueued
			}
			deliver, eligibilityErr := mail.PrepareWakeClaim(townRoot, claim)
			if !deliver {
				releaseLease()
				if eligibilityErr != nil {
					fmt.Fprintf(os.Stderr, "idle-watcher: source eligibility for %s failed: %v\n", sessionName, eligibilityErr)
				}
				return nudgeDeliveryQueued
			}
			formatted := nudge.FormatForInjection([]nudge.QueuedNudge{claim.Nudge})
			receipt, deliveryErr := t.NudgeSessionWithReceipt(sessionName, formatted, tmux.NudgeOpts{TownRoot: townRoot, DeliveryID: claim.Nudge.DeliveryID})
			nextAttempt := nudge.NextRetry(claim.Nudge.Attempts)
			settleErr := settlePollerClaimUnderLease(
				releaseLease,
				receipt,
				deliveryErr,
				nextAttempt,
				claim.AckSubmitted,
				claim.Nack,
				claim.HasRecoverableState,
				func() error { return claim.Nack("settlement-recovery", nextAttempt) },
				func() { time.Sleep(nudgePollerStopInterval) },
			)
			if settleErr != nil {
				fmt.Fprintf(os.Stderr, "idle-watcher: settlement for %s failed: %v\n", sessionName, settleErr)
			}
			if deliveryErr == nil && settleErr == nil && receipt.Submitted {
				return nudgeDeliverySubmitted
			}
			return nudgeDeliveryQueued
		}
	}
	// Timeout — nudge stays in queue for next watcher or manual drain.
	return nudgeDeliveryQueued
}

// requeueDrainedNudges remains for compatibility with callers that explicitly
// use the consume-on-read Drain API. Runtime delivery uses ClaimDue/Nack.
func requeueDrainedNudges(townRoot, sessionName, source string, drained []nudge.QueuedNudge) {
	if err := nudge.Requeue(townRoot, sessionName, drained); err != nil {
		fmt.Fprintf(os.Stderr, "%s: requeue for %s failed: %v\n", source, sessionName, err)
	}
}

// validNudgeModes is the set of allowed --mode values.
var validNudgeModes = map[string]bool{
	NudgeModeImmediate: true,
	NudgeModeQueue:     true,
	NudgeModeWaitIdle:  true,
}

// validNudgePriorities is the set of allowed --priority values.
var validNudgePriorities = map[string]bool{
	nudge.PriorityNormal: true,
	nudge.PriorityUrgent: true,
}

func runNudge(cmd *cobra.Command, args []string) (retErr error) {
	defer func() {
		target := ""
		if len(args) > 0 {
			target = args[0]
		}
		telemetry.RecordNudge(context.Background(), target, retErr)
	}()
	// Validate --mode and --priority before doing anything else.
	if !validNudgeModes[nudgeModeFlag] {
		return fmt.Errorf("invalid --mode %q: must be one of immediate, queue, wait-idle", nudgeModeFlag)
	}
	if !validNudgePriorities[nudgePriorityFlag] {
		return fmt.Errorf("invalid --priority %q: must be one of normal, urgent", nudgePriorityFlag)
	}

	// --if-fresh: skip nudge if the caller's tmux session is older than 60s.
	// This prevents compaction/clear SessionStart hooks from spamming the deacon.
	if nudgeIfFreshFlag {
		sessionName := tmux.CurrentSessionName()
		if sessionName != "" {
			t := tmux.NewTmux()
			created, err := t.GetSessionCreatedUnix(sessionName)
			if err == nil && created > 0 {
				age := time.Since(time.Unix(created, 0))
				if age > ifFreshMaxAge {
					// Session is old — this is a compaction/clear, not a new session
					return nil
				}
			}
		}
	}

	target := args[0]

	// Normalize trailing slash: the mail system uses "mayor/" and "deacon/"
	// as canonical addresses, but nudge role shortcuts expect bare names.
	// Without this, "mayor/" falls through to parseAddress which rejects
	// the empty second component, silently dropping the nudge.
	target = strings.TrimSuffix(target, "/")

	// Handle --stdin: read message from stdin (avoids shell quoting issues)
	if nudgeStdinFlag {
		if nudgeMessageFlag != "" {
			return fmt.Errorf("cannot use --stdin with --message/-m")
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("reading stdin: %w", err)
		}
		nudgeMessageFlag = strings.TrimRight(string(data), "\n")
	}

	// Get message from -m flag or positional arg
	var message string
	if nudgeMessageFlag != "" {
		message = nudgeMessageFlag
	} else if len(args) >= 2 {
		message = args[1]
	} else {
		return fmt.Errorf("message required: use -m flag or provide as second argument")
	}

	// Identify sender for message prefix (needed before channel check)
	sender := "unknown"
	if roleInfo, err := GetRole(); err == nil {
		switch roleInfo.Role {
		case RoleMayor:
			sender = constants.RoleMayor
		case RoleCrew:
			sender = fmt.Sprintf("%s/crew/%s", roleInfo.Rig, roleInfo.Polecat)
		case RolePolecat:
			sender = fmt.Sprintf("%s/%s", roleInfo.Rig, roleInfo.Polecat)
		case RoleWitness:
			sender = fmt.Sprintf("%s/witness", roleInfo.Rig)
		case RoleRefinery:
			sender = fmt.Sprintf("%s/refinery", roleInfo.Rig)
		case RoleDeacon:
			sender = constants.RoleDeacon
		default:
			sender = string(roleInfo.Role)
		}
	}

	// Handle channel syntax: channel:<name>
	if strings.HasPrefix(target, "channel:") {
		if nudgeSourceMailFlag != "" {
			return fmt.Errorf("--source-mail requires one direct recipient")
		}
		channelName := strings.TrimPrefix(target, "channel:")
		return runNudgeChannel(channelName, message, sender)
	}

	// Check DND status for target (unless force flag or channel target)
	townRoot, _ := workspace.FindFromCwd()
	source := nudgeSource{}
	if nudgeSourceMailFlag != "" {
		if townRoot == "" {
			return fmt.Errorf("--source-mail requires a Gas Town workspace")
		}
		var err error
		source, err = resolveNudgeSourceMail(townRoot, target, nudgeSourceMailFlag)
		if err != nil {
			return err
		}
	}
	if townRoot != "" {
		// Initialize tmux socket and prefix registry so NewTmux() connects
		// to the correct town socket. Without this, nudge from non-agent
		// contexts (e.g., crew workspaces without GT_TOWN_SOCKET) falls
		// through to the sentinel socket and fails to find sessions.
		_ = session.InitRegistry(townRoot)
	}
	if townRoot != "" && !nudgeForceFlag {
		shouldSend, level, _ := shouldNudgeTarget(townRoot, target, nudgeForceFlag)
		if !shouldSend {
			fmt.Printf("%s Target has DND enabled (%s) - nudge skipped\n", style.Dim.Render("○"), level)
			fmt.Printf("  Use %s to override\n", style.Bold.Render("--force"))
			return nil
		}
	}

	t := tmux.NewTmux()

	// Expand role shortcuts to session names
	// These shortcuts let users type "mayor" instead of "gt-mayor"
	switch target {
	case constants.RoleMayor:
		target = session.MayorSessionName()
	case constants.RoleWitness, constants.RoleRefinery:
		// These need the current rig
		roleInfo, err := GetRole()
		if err != nil {
			return fmt.Errorf("cannot determine rig for %s shortcut: %w", target, err)
		}
		if roleInfo.Rig == "" {
			return fmt.Errorf("cannot determine rig for %s shortcut (not in a rig context)", target)
		}
		rigPrefix := session.PrefixFor(roleInfo.Rig)
		if target == constants.RoleWitness {
			target = session.WitnessSessionName(rigPrefix)
		} else {
			target = session.RefinerySessionName(rigPrefix)
		}
	}

	// Special case: "deacon" target maps to the Deacon session
	if target == constants.RoleDeacon {
		deaconSession := session.DeaconSessionName()
		// Check if Deacon session exists (tmux or ACP)
		hasACP := hasACPSessionByName(townRoot, deaconSession)
		exists := false
		if !hasACP {
			exists, _ = t.HasSession(deaconSession)
		}

		if !hasACP && !exists {
			// Deacon not running - this is not an error, just log and return
			fmt.Printf("%s Deacon not running, nudge skipped\n", style.Dim.Render("○"))
			return nil
		}

		result, err := deliverNudge(t, deaconSession, message, sender, source)
		if err != nil {
			return fmt.Errorf("nudging deacon: %w", err)
		}

		fmt.Printf("%s Nudge deacon: %s\n", style.Bold.Render("✓"), result)

		// Log nudge event
		if townRoot, err := workspace.FindFromCwd(); err == nil && townRoot != "" {
			_ = LogNudge(townRoot, constants.RoleDeacon, message)
		}
		_ = events.LogFeed(events.TypeNudge, sender, events.NudgePayload("", constants.RoleDeacon, message))
		return nil
	}
	if dogName, ok := mail.DogAddressName(target); ok {
		sessionName := session.DogSessionName(dogName)
		if nudgeModeFlag != NudgeModeImmediate && !hasACPSessionByName(townRoot, sessionName) {
			exists, err := t.HasSession(sessionName)
			if err != nil {
				return fmt.Errorf("checking dog session: %w", err)
			}
			if !exists {
				return fmt.Errorf("session %q not found (cannot queue nudge for nonexistent session)", sessionName)
			}
		}

		result, err := deliverNudge(t, sessionName, message, sender, source)
		if err != nil {
			return fmt.Errorf("nudging dog: %w", err)
		}

		fmt.Printf("%s Nudge %s: %s\n", style.Bold.Render("✓"), target, result)
		if townRoot, err := workspace.FindFromCwd(); err == nil && townRoot != "" {
			_ = LogNudge(townRoot, target, message)
		}
		_ = events.LogFeed(events.TypeNudge, sender, events.NudgePayload("", target, message))
		return nil
	}
	if strings.HasPrefix(target, constants.RoleMayor+"/") || strings.HasPrefix(target, constants.RoleDeacon+"/") {
		return fmt.Errorf("invalid town target %q", target)
	}

	// Check if target is rig/polecat format or raw session name
	if strings.Contains(target, "/") {
		// Parse rig/polecat format
		rigName, polecatName, err := parseAddress(target)
		if err != nil {
			return err
		}

		var sessionName string

		// Check if this is a crew address (polecatName starts with "crew/")
		if strings.HasPrefix(polecatName, "crew/") {
			// Extract crew name and use crew session naming
			crewName := strings.TrimPrefix(polecatName, "crew/")
			sessionName = crewSessionName(rigName, crewName)
		} else if strings.HasPrefix(polecatName, "polecats/") {
			// Explicit polecat address (e.g., "vastal/polecats/furiosa").
			// Bypasses crew-first resolution for short addresses.
			pcName := strings.TrimPrefix(polecatName, "polecats/")
			mgr, _, err := getSessionManager(rigName)
			if err != nil {
				return err
			}
			sessionName = mgr.SessionName(pcName)
		} else {
			// Short address (e.g., "gastown/holden") - could be crew or polecat.
			// Try crew first (matches mail system's addressToSessionIDs pattern),
			// then fall back to polecat.
			crewSession := crewSessionName(rigName, polecatName)
			if exists, _ := t.HasSession(crewSession); exists {
				sessionName = crewSession
			} else {
				mgr, _, err := getSessionManager(rigName)
				if err != nil {
					return err
				}
				sessionName = mgr.SessionName(polecatName)
			}
		}

		// For queue/wait-idle modes, verify session exists before enqueuing.
		// Without this, queue mode silently succeeds for nonexistent sessions —
		// the file is written but never drained.
		// ACP sessions are always allowed as they use queue mode.
		if nudgeModeFlag != NudgeModeImmediate && !hasACPSessionByName(townRoot, sessionName) {
			exists, err := t.HasSession(sessionName)
			if err != nil {
				return fmt.Errorf("checking session: %w", err)
			}
			if !exists {
				return fmt.Errorf("session %q not found (cannot queue nudge for nonexistent session)", sessionName)
			}
		}

		// Send nudge using the configured delivery mode
		result, err := deliverNudge(t, sessionName, message, sender, source)
		if err != nil {
			return fmt.Errorf("nudging session: %w", err)
		}

		fmt.Printf("%s Nudge %s/%s: %s\n", style.Bold.Render("✓"), rigName, polecatName, result)

		// Log nudge event
		if townRoot, err := workspace.FindFromCwd(); err == nil && townRoot != "" {
			_ = LogNudge(townRoot, target, message)
		}
		_ = events.LogFeed(events.TypeNudge, sender, events.NudgePayload(rigName, target, message))
	} else {
		// Raw session name (legacy)
		// Check for ACP session - ACP agents don't have tmux sessions but can receive nudges via queue
		hasACP := hasACPSessionByName(townRoot, target)

		if !hasACP {
			exists, err := t.HasSession(target)
			if err != nil {
				return fmt.Errorf("checking session: %w", err)
			}
			if !exists {
				return fmt.Errorf("session %q not found", target)
			}
		}

		result, err := deliverNudge(t, target, message, sender, source)
		if err != nil {
			return fmt.Errorf("nudging session: %w", err)
		}

		fmt.Printf("✓ Nudge %s: %s\n", target, result)

		// Log nudge event
		if townRoot, err := workspace.FindFromCwd(); err == nil && townRoot != "" {
			_ = LogNudge(townRoot, target, message)
		}
		_ = events.LogFeed(events.TypeNudge, sender, events.NudgePayload("", target, message))
	}

	return nil
}

// runNudgeChannel nudges all members of a named channel.
// Routes each target through deliverNudge so --mode is respected.
func runNudgeChannel(channelName, message, sender string) error {
	// Find town root
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("cannot find town root: %w", err)
	}

	// Load messaging config
	msgConfigPath := config.MessagingConfigPath(townRoot)
	msgConfig, err := config.LoadMessagingConfig(msgConfigPath)
	if err != nil {
		return fmt.Errorf("loading messaging config: %w", err)
	}

	// Look up channel
	patterns, ok := msgConfig.NudgeChannels[channelName]
	if !ok {
		return fmt.Errorf("nudge channel %q not found in messaging config", channelName)
	}

	if len(patterns) == 0 {
		return fmt.Errorf("nudge channel %q has no members", channelName)
	}

	// Get all running sessions for pattern matching
	agents, err := getAgentSessions(true)
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}

	// Resolve patterns to session names
	var targets []string
	seenTargets := make(map[string]bool)

	for _, pattern := range patterns {
		resolved := resolveNudgePattern(pattern, agents)
		for _, sessionName := range resolved {
			if !seenTargets[sessionName] {
				seenTargets[sessionName] = true
				targets = append(targets, sessionName)
			}
		}
	}

	if len(targets) == 0 {
		fmt.Printf("%s No sessions match channel %q patterns\n", style.WarningPrefix, channelName)
		return nil
	}

	// Send nudges via deliverNudge (respects --mode flag)
	t := tmux.NewTmux()
	var succeeded, failed, skipped int
	var failures []string

	fmt.Printf("Nudging channel %q (%d target(s), mode=%s)...\n\n", channelName, len(targets), nudgeModeFlag)

	for i, sessionName := range targets {
		// Check DND status before nudging each target
		// Convert session name back to address format for DND lookup
		targetAddr := sessionNameToAddress(sessionName)
		if targetAddr != "" {
			if shouldSend, level, _ := shouldNudgeTarget(townRoot, targetAddr, false); !shouldSend {
				skipped++
				fmt.Printf("  %s %s (DND: %s)\n", style.Dim.Render("○"), sessionName, level)
				continue
			}
		}

		result, err := deliverNudge(t, sessionName, message, sender, nudgeSource{})
		if err != nil {
			failed++
			failures = append(failures, fmt.Sprintf("%s: %v", sessionName, err))
			fmt.Printf("  %s %s\n", style.ErrorPrefix, sessionName)
		} else {
			succeeded++
			fmt.Printf("  %s %s (%s)\n", style.SuccessPrefix, sessionName, result)
		}

		// Small delay between nudges
		if i < len(targets)-1 {
			time.Sleep(100 * time.Millisecond)
		}
	}

	fmt.Println()

	// Log nudge event
	_ = events.LogFeed(events.TypeNudge, sender, events.NudgePayload("", "channel:"+channelName, message))

	if failed > 0 {
		summary := fmt.Sprintf("Channel nudge complete: %d succeeded, %d failed", succeeded, failed)
		if skipped > 0 {
			summary += fmt.Sprintf(", %d skipped (DND)", skipped)
		}
		fmt.Printf("%s %s\n", style.WarningPrefix, summary)
		for _, f := range failures {
			fmt.Printf("  %s\n", style.Dim.Render(f))
		}
		return fmt.Errorf("%d nudge(s) failed", failed)
	}

	summary := fmt.Sprintf("Channel nudge complete: %d target(s) nudged", succeeded)
	if skipped > 0 {
		summary += fmt.Sprintf(", %d skipped (DND)", skipped)
	}
	fmt.Printf("%s %s\n", style.SuccessPrefix, summary)
	return nil
}

// resolveNudgePattern resolves a nudge channel pattern to session names.
// Patterns can be:
//   - Literal: "gastown/witness" → gt-gastown-witness
//   - Wildcard: "gastown/polecats/*" → all polecat sessions in gastown
//   - Role: "*/witness" → all witness sessions
//   - Special: "mayor", "deacon" → gt-{town}-mayor, gt-{town}-deacon
//
// townName is used to generate the correct session names for mayor/deacon.
func resolveNudgePattern(pattern string, agents []*AgentSession) []string {
	var results []string

	// Handle special cases
	switch pattern {
	case constants.RoleMayor:
		return []string{session.MayorSessionName()}
	case constants.RoleDeacon:
		return []string{session.DeaconSessionName()}
	}

	// Parse pattern
	if !strings.Contains(pattern, "/") {
		// Unknown pattern format
		return nil
	}

	parts := strings.SplitN(pattern, "/", 2)
	rigPattern := parts[0]
	targetPattern := parts[1]

	for _, agent := range agents {
		// Match rig pattern
		if rigPattern != "*" && rigPattern != agent.Rig {
			continue
		}

		// Match target pattern
		if strings.HasPrefix(targetPattern, "polecats/") {
			// polecats/* or polecats/<name>
			if agent.Type != AgentPolecat {
				continue
			}
			suffix := strings.TrimPrefix(targetPattern, "polecats/")
			if suffix != "*" && suffix != agent.AgentName {
				continue
			}
		} else if strings.HasPrefix(targetPattern, "crew/") {
			// crew/* or crew/<name>
			if agent.Type != AgentCrew {
				continue
			}
			suffix := strings.TrimPrefix(targetPattern, "crew/")
			if suffix != "*" && suffix != agent.AgentName {
				continue
			}
		} else if targetPattern == constants.RoleWitness {
			if agent.Type != AgentWitness {
				continue
			}
		} else if targetPattern == constants.RoleRefinery {
			if agent.Type != AgentRefinery {
				continue
			}
		} else {
			// Assume it's a polecat name (legacy short format)
			if agent.Type != AgentPolecat || agent.AgentName != targetPattern {
				continue
			}
		}

		results = append(results, agent.Name)
	}

	return results
}

// shouldNudgeTarget checks if a nudge should be sent based on the target's notification level.
// Returns (shouldSend bool, level string, err error).
// If force is true, always returns true.
// If the agent bead cannot be found, returns true (fail-open for backward compatibility).
func shouldNudgeTarget(townRoot, targetAddress string, force bool) (bool, string, error) { //nolint:unparam // error return kept for future use
	if force {
		return true, "", nil
	}

	// Try to determine agent bead ID from address
	agentBeadID := addressToAgentBeadID(targetAddress)
	if agentBeadID == "" {
		// Can't determine agent bead, allow the nudge
		return true, "", nil
	}

	bd := beads.New(townRoot)
	level, err := bd.GetAgentNotificationLevel(agentBeadID)
	if err != nil {
		// Agent bead might not exist, allow the nudge
		return true, "", nil
	}

	// Allow nudge if level is not muted
	return level != beads.NotifyMuted, level, nil
}

// sessionNameToAddress converts a tmux session name back to a mail address
// for DND lookup. Returns empty string if the format is unrecognized.
// Examples:
//   - "gt-gastown-crew-max" -> "gastown/crew/max"
//   - "gt-gastown-alpha" -> "gastown/alpha"
//   - "gt-gastown-witness" -> "gastown/witness"
//   - "hq-mayor" -> "mayor"
//   - "hq-deacon" -> "deacon"
func sessionNameToAddress(sessionName string) string {
	identity, err := session.ParseSessionName(sessionName)
	if err != nil {
		return ""
	}

	// Use short address format: rig/name (not rig/polecats/name)
	switch identity.Role {
	case session.RoleMayor:
		return constants.RoleMayor
	case session.RoleDeacon:
		return constants.RoleDeacon
	case session.RoleDog:
		return mail.DogAddress(identity.Name)
	case session.RoleWitness:
		return fmt.Sprintf("%s/witness", identity.Rig)
	case session.RoleRefinery:
		return fmt.Sprintf("%s/refinery", identity.Rig)
	case session.RoleCrew:
		return fmt.Sprintf("%s/crew/%s", identity.Rig, identity.Name)
	case session.RolePolecat:
		return fmt.Sprintf("%s/%s", identity.Rig, identity.Name)
	default:
		return ""
	}
}

// addressToAgentBeadID converts a target address to an agent bead ID.
// Examples:
//   - "mayor" -> "gt-{town}-mayor"
//   - "deacon" -> "gt-{town}-deacon"
//   - "gastown/witness" -> "gt-gastown-witness"
//   - "gastown/alpha" -> "gt-gastown-polecat-alpha"
//
// Returns empty string if the address cannot be converted.
func addressToAgentBeadID(address string) string {
	if dogName, ok := mail.DogAddressName(address); ok {
		return session.DogSessionName(dogName)
	}
	// Handle special cases
	switch address {
	case constants.RoleMayor, constants.RoleMayor + "/":
		return session.MayorSessionName()
	case constants.RoleDeacon, constants.RoleDeacon + "/":
		return session.DeaconSessionName()
	}
	if strings.HasPrefix(address, constants.RoleMayor+"/") || strings.HasPrefix(address, constants.RoleDeacon+"/") {
		return ""
	}

	// Parse rig/role format
	if !strings.Contains(address, "/") {
		return ""
	}

	parts := strings.SplitN(address, "/", 2)
	if len(parts) != 2 {
		return ""
	}

	rig := parts[0]
	role := parts[1]

	switch role {
	case constants.RoleWitness:
		return session.WitnessSessionName(session.PrefixFor(rig))
	case constants.RoleRefinery:
		return session.RefinerySessionName(session.PrefixFor(rig))
	default:
		// Assume polecat
		if strings.HasPrefix(role, "crew/") {
			crewName := strings.TrimPrefix(role, "crew/")
			return session.CrewSessionName(session.PrefixFor(rig), crewName)
		}
		if strings.HasPrefix(role, "polecats/") {
			pcName := strings.TrimPrefix(role, "polecats/")
			return session.PolecatSessionName(session.PrefixFor(rig), pcName)
		}
		return session.PolecatSessionName(session.PrefixFor(rig), role)
	}
}
