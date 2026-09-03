package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	nudgePollerIntervalFlag string
	nudgePollerIdleFlag     string
)

const nudgePollerStopInterval = 100 * time.Millisecond

func init() {
	rootCmd.AddCommand(nudgePollerCmd)
	nudgePollerCmd.Flags().StringVar(&nudgePollerIntervalFlag, "interval", nudge.DefaultPollInterval, "Poll interval (e.g., 10s, 30s)")
	nudgePollerCmd.Flags().StringVar(&nudgePollerIdleFlag, "idle-timeout", nudge.DefaultIdleTimeout, "How long to wait for agent idle before skipping")
}

var nudgePollerCmd = &cobra.Command{
	Use:    "nudge-poller <session>",
	Short:  "Background nudge queue poller for non-Claude agents",
	Hidden: true, // Internal command — launched by crew manager, not by users.
	Long: `Polls the nudge queue for a tmux session and drains it when the agent
is idle. This is the background equivalent of Claude's UserPromptSubmit hook
drain — it ensures queued nudges are delivered to agents that lack
turn-boundary hooks (Gemini, Codex, Cursor, etc.).

This command runs as a long-lived background process. It exits when:
  - The target tmux session dies
  - Its generation-bound cooperative stop request is published
  - It receives SIGTERM or SIGINT from external process teardown
  - The poll loop encounters an unrecoverable error

Normally launched automatically by 'gt crew start' for non-Claude agents.
Not intended for direct user invocation.`,
	Args: cobra.ExactArgs(1),
	RunE: runNudgePoller,
}

func runNudgePoller(cmd *cobra.Command, args []string) error {
	sessionName := args[0]

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("cannot find town root: %w", err)
	}

	pollInterval, err := time.ParseDuration(nudgePollerIntervalFlag)
	if err != nil {
		return fmt.Errorf("invalid --interval: %w", err)
	}

	idleTimeout, err := time.ParseDuration(nudgePollerIdleFlag)
	if err != nil {
		return fmt.Errorf("invalid --idle-timeout: %w", err)
	}

	return runNudgePollerLoop(cmd.Context(), townRoot, sessionName, tmux.NewTmux(), pollInterval, idleTimeout)
}

func runNudgePollerLoop(ctx context.Context, townRoot, sessionName string, t *tmux.Tmux, pollInterval, idleTimeout time.Duration) error {
	return runNudgePollerLoopWithWait(ctx, townRoot, sessionName, t, pollInterval, idleTimeout, t.WaitForIdleContext)
}

func runNudgePollerLoopWithWait(
	ctx context.Context,
	townRoot, sessionName string,
	t *tmux.Tmux,
	pollInterval, idleTimeout time.Duration,
	waitForIdle func(context.Context, string, time.Duration) error,
) error {

	// Verify session exists before starting the loop.
	if exists, err := pollerHasSession(t, sessionName); err != nil {
		return err
	} else if !exists {
		return fmt.Errorf("session %q not found", sessionName)
	}

	// Resolve nudge options once at startup: if the target agent uses Escape
	// as cancel (e.g., Gemini CLI), skip the Escape keystroke during delivery
	// to avoid canceling in-flight generation. (GH#gt-wasn)
	hasPromptDetection, nudgeOpts := resolvePollerSessionMetadata(t, sessionName)

	// Make signals, caller cancellation, and generation-bound cooperative stop
	// one context so idle and lease waits can terminate without abandoning a
	// claimed queue record.
	signalContext, stopSignals := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()
	pollerContext, cancelPoller := context.WithCancel(signalContext)
	defer cancelPoller()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	cooperativeStop := watchCooperativePollerStop(pollerContext, nudgePollerStopInterval, func() bool {
		return nudge.StopRequested(townRoot, sessionName)
	})
	go func() {
		select {
		case <-cooperativeStop:
			cancelPoller()
		case <-pollerContext.Done():
		}
	}()

	for {
		select {
		case <-pollerContext.Done():
			if err := ctx.Err(); err != nil {
				return err
			}
			return nil

		case <-ticker.C:
			// Check if session still exists.
			if exists, err := pollerHasSession(t, sessionName); err != nil {
				fmt.Fprintf(os.Stderr, "nudge-poller: session check error for %s: %v\n", sessionName, err)
				continue
			} else if !exists {
				return nil // session gone, exit
			}

			// Claimed-only state must keep polling so ClaimDue can perform its
			// orphan recovery even when no ordinary .json record remains.
			hasWork, workErr := nudge.HasQueuedOrClaimed(townRoot, sessionName)
			if workErr != nil {
				fmt.Fprintf(os.Stderr, "nudge-poller: queue check error for %s: %v\n", sessionName, workErr)
				continue
			}
			if !hasWork {
				continue
			}

			// For runtimes with prompt detection, defer delivery until the session
			// is actually idle. Runtimes without prompt detection preserve the old
			// best-effort behavior and drain on the poll interval.
			claim, releaseLease, err := claimPollerNudgeWhenIdleContext(
				pollerContext,
				hasPromptDetection,
				func(ctx context.Context) error { return waitForIdle(ctx, sessionName, idleTimeout) },
				func(ctx context.Context) (func(), error) {
					return t.AcquireNudgeLeaseContext(ctx, townRoot, sessionName)
				},
				func() (*nudge.ClaimedNudge, error) { return nudge.ClaimDue(townRoot, sessionName) },
			)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					if parentErr := ctx.Err(); parentErr != nil {
						return parentErr
					}
					return nil
				}
				fmt.Fprintf(os.Stderr, "nudge-poller: claim error for %s: %v\n", sessionName, err)
				continue
			}
			if claim == nil {
				continue // someone else drained it
			}
			deliver, eligibilityErr := mail.PrepareWakeClaim(townRoot, claim)
			if !deliver {
				releaseLease()
				if eligibilityErr != nil {
					fmt.Fprintf(os.Stderr, "nudge-poller: source eligibility error for %s: %v\n", sessionName, eligibilityErr)
				}
				continue
			}

			func() {
				formatted := nudge.FormatForInjection([]nudge.QueuedNudge{claim.Nudge})
				nudgeOpts.TownRoot = townRoot
				nudgeOpts.DeliveryID = claim.Nudge.DeliveryID
				receipt, err := t.NudgeSessionWithReceipt(sessionName, formatted, nudgeOpts)
				nextAttempt := nudge.NextRetry(claim.Nudge.Attempts)
				settleErr := settlePollerClaimUnderLease(
					releaseLease,
					receipt,
					err,
					nextAttempt,
					claim.AckSubmitted,
					claim.Nack,
					claim.HasRecoverableState,
					func() error { return claim.Nack("settlement-recovery", nextAttempt) },
					func() { time.Sleep(nudgePollerStopInterval) },
				)
				if settleErr != nil {
					fmt.Fprintf(os.Stderr, "nudge-poller: settlement error for %s: %v\n", sessionName, settleErr)
				}
			}()
		}
	}
}

func settlePollerClaimUnderLease(
	releaseLease func(),
	receipt tmux.SubmissionReceipt,
	deliveryErr error,
	nextAttempt time.Time,
	ack func(tmux.SubmissionReceipt) error,
	nack func(string, time.Time) error,
	recoverable func() bool,
	retry func() error,
	pause func(),
) error {
	safe, settleErr := settlePollerClaim(receipt, deliveryErr, nextAttempt, ack, nack, recoverable)
	if !safe {
		// Keep the delivery lease until the in-memory claim has once again
		// reached a provably durable path. Stop/replacement stays fail-closed
		// while filesystem recovery is impossible.
		waitForRecoverablePollerClaim(recoverable, retry, pause)
	}
	releaseLease()
	return settleErr
}

func settlePollerClaim(
	receipt tmux.SubmissionReceipt,
	deliveryErr error,
	nextAttempt time.Time,
	ack func(tmux.SubmissionReceipt) error,
	nack func(string, time.Time) error,
	recoverable func() bool,
) (bool, error) {
	if deliveryErr == nil && receipt.Submitted {
		if ackErr := ack(receipt); ackErr == nil {
			return true, nil
		} else if nackErr := nack("receipt-mismatch", nextAttempt); nackErr == nil {
			return true, ackErr
		} else {
			return recoverable(), errors.Join(ackErr, nackErr)
		}
	}
	nackErr := nack("submit-unverified", nextAttempt)
	if nackErr == nil {
		return true, deliveryErr
	}
	return recoverable(), errors.Join(deliveryErr, nackErr)
}

func waitForRecoverablePollerClaim(recoverable func() bool, retry func() error, pause func()) {
	for !recoverable() {
		_ = retry()
		if recoverable() {
			return
		}
		pause()
	}
}

func claimPollerNudgeWhenIdleContext(
	ctx context.Context,
	hasPromptDetection bool,
	waitForIdle func(context.Context) error,
	acquireLease func(context.Context) (func(), error),
	claimDue func() (*nudge.ClaimedNudge, error),
) (*nudge.ClaimedNudge, func(), error) {
	if hasPromptDetection {
		if err := waitForIdle(ctx); err != nil {
			if errors.Is(err, tmux.ErrIdleTimeout) {
				return nil, nil, nil
			}
			return nil, nil, err
		}
	}
	release, err := acquireLease(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, nil, err
	}
	claim, err := claimDue()
	if err != nil || claim == nil {
		release()
		return claim, nil, err
	}
	return claim, release, nil
}

func pollerHasSession(t *tmux.Tmux, session string) (bool, error) {
	return pollerHasSessionWith(func() (bool, error) { return t.HasSession(session) })
}

func pollerHasSessionWith(check func() (bool, error)) (bool, error) {
	for attempt := 0; attempt < 3; attempt++ {
		exists, err := check()
		if err == nil || exists {
			return exists, err
		}
		time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
	}
	return false, fmt.Errorf("session check failed")
}

func watchCooperativePollerStop(ctx context.Context, interval time.Duration, stopRequested func() bool) <-chan struct{} {
	stopped := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if stopRequested() {
					close(stopped)
					return
				}
			}
		}
	}()
	return stopped
}

func claimPollerNudgeWhenIdle(
	hasPromptDetection bool,
	waitForIdle func() error,
	claimDue func() (*nudge.ClaimedNudge, error),
) (*nudge.ClaimedNudge, error) {
	if shouldSkipDrainUntilIdle(hasPromptDetection, waitForIdle()) {
		return nil, nil
	}
	return claimDue()
}

func resolvePollerSessionMetadata(t *tmux.Tmux, sessionName string) (bool, tmux.NudgeOpts) {
	var opts tmux.NudgeOpts
	agentName, _ := t.GetEnvironment(sessionName, "GT_AGENT")
	preset := config.GetAgentPresetByName(agentName)
	if preset != nil && preset.EscapeCancelsRequest {
		opts.SkipEscape = true
	}
	if prompt, err := t.GetEnvironment(sessionName, "GT_READY_PROMPT_PREFIX"); err == nil {
		return prompt != "", opts
	}
	return preset != nil && preset.ReadyPromptPrefix != "", opts
}

func shouldSkipDrainUntilIdle(hasPromptDetection bool, waitErr error) bool {
	return hasPromptDetection && waitErr != nil
}
