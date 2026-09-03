# Inter-Agent Notification Convergence Phase 0 Investigation

Status: In progress

Governing specification:
`docs/superpowers/specs/260902-inter-agent-notification-convergence-and-alert-deduplication-repair-spec.md`

Repair tracker: `gastown-7rp`

## Exact custody and non-mutation statement

The investigation checkout is branch
`investigate/notification-convergence-260903` at exact base
`ba122e52d0562b26cb8f603a81275908fe4f02bb`, tree
`22750f71b054da058b30ac65169e931c09ee2e5f`. It started clean, has no replace
refs, and was created from the clean authoritative
`repair/remaining-gastown-repairs` tip after verifying that freshly fetched
`ohchanwu/gastown:main` at `e18b7eacb80a1b43bc1b37f1d139890d33166711`
is its ancestor. The primary rig checkout, foreign worktrees, and one foreign
stash were not modified.

Phase 0 changes only this report. It does not read message bodies or mutate
live mail, escalations, queues, sessions, agents, hooks, delivery state,
listeners, or databases. Behavioral experiments use only isolated fixtures.

The read-only live baseline reported one healthy canonical Dolt server, zero
configured-port imposters, zero owned town leaks, zero owned test leaks,
nineteen unknown listeners, and one unrelated orphan database. These are
observations, not cleanup targets. Direct query latency was zero seconds and
eight of one thousand connections were in use.

## Historical timeline and commit map

The history shows repeated repairs to proof and custody, not gratuitous retry
loops. A wake that looks redundant may be the only surviving defense against a
different delivery failure. The following commits changed a named guarantee
and remain represented in current source or tests:

- **2026-08-05 — `2b116422`: startup nudge submission proof.** A fast turn
  could finish before idle-prompt verification and be mistaken for a lost
  startup nudge. `SessionManager.verifyStartupNudgeDelivery` now accepts an
  exact submitted receipt before using the bounded legacy fallback.
  `TestVerifyStartupNudgeDelivery_SubmittedReceiptSkipsIdleRetry` retains the
  regression.
- **2026-08-19/20 — `cb80ca0`, `7cbb663`: deterministic stored-pane proof.**
  Pane output and ambient acknowledgements could falsely pass a nudge test.
  The tmux harness now uses nonce-bound receiver and decoy transcripts and
  exact payload acknowledgements. `TestNudgeStoredPaneReceiverProtocol`,
  `TestStoredPaneNudgeReceiverInvocation`, and
  `TestValidateStoredPaneNudgeOutput` retain the proof.
- **2026-08-20 — `ed5d3c6`, `12018ab`: supported thread retrieval and route
  custody.** Thread lookup formerly depended on an unsupported Beads command
  and then accepted incompletely bound announce records. `Mailbox.ListByThread`
  now uses supported label queries and validates direct, queue, channel, and
  announce route labels. `TestMailboxBeadsListByThread` and
  `TestMailboxBeadsListByThreadAcceptsStoredQueueChannelAndAnnounceRoutes`
  cover the behavior.
- **2026-08-24 — `407cb8c`, `15bc95c`, `1253c36`: actionable-mail state.**
  Reading, transport acknowledgement, ownership, and completion were
  previously conflated. `MailWorkStore` now binds claims to exact generations
  and completes the reply plus source closure atomically, while `Mailbox`
  records presentation separately. `TestMailWorkStoreClaimDirectAndQueue`,
  `TestMailWorkStoreCompleteExactlyOnce`, and
  `TestAppendBeadsMessagesKeepsActiveWorkAndAllAddsClosedWork` retain the
  central invariants.
- **2026-08-24 — `b47bac9`, `c89120c`: preservation during recovery and
  cleanup.** Reaper could recover or close mail without sufficient lifecycle
  authority. Patrol recovery now uses exact-generation evidence, and Reaper
  excludes protocol mail and nonterminal enrolled work. Current coverage
  includes `TestMailWorkStoreRecoveryIsConservativeAndCASBound`,
  `TestPurgeOldMailExcludesLifecycleHistoryOnIsolatedDolt`, and the stale-close
  protocol-mail cases in `internal/reaper/reaper_test.go`.
- **2026-08-27 — `af5af3e`: receipt-driven wake health.** Health reporting
  could treat unproven output as accepted delivery. The canary and control-plane
  evaluator now require matching submitted receipts while treating retrying
  urgent delivery as healthy before its deadline.
  `TestEvaluateControlPlaneRejectsPassedCanaryWithoutReceiptProof` and
  `TestEvaluateControlPlaneKeepsRetryingUrgentWakeHealthyBeforeDeadline`
  retain that distinction.
- **2026-08-27 — `46b3b60`: portable generation validation.** Mail work from a
  valid portable generation was rejected solely because one legacy custody
  field was absent. `WorkGeneration.validate` now requires the portable
  identity fields and compares captured runtime authority separately.
  `TestPortableMailWorkGenerationWithoutCustodyValidatesClaimsAndRemainsLive`
  and `TestWorkGenerationValidationRequiresPortableIdentityFields` cover it.
- **2026-09-01 — `880b352`: integrated lifecycle custody.** This integration
  carried the mail delivery, router, poller, tmux, Witness, and lifecycle
  repairs together. It is an integration boundary rather than authority to
  remove any fallback independently.

The maintained mail protocol confirms the architectural split: mail is the
durable source; a nudge is an ephemeral wake; storage success is not delivery
acceptance; and unverified notification remains queued for receipt-confirmed
retry. The archived actionable-mail specification explicitly made semantic
content deduplication a non-goal and scoped idempotency to one message ID. This
investigation therefore treats the newer request for alert convergence as a
separate, evidence-gated change rather than silently weakening that contract.

The historical evidence command in the plan used `git log --follow` with
multiple paths, which Git rejects. The investigation used the equivalent
multi-path history query without `--follow`; no history range was omitted.

## Producer and consumer inventory

Investigation pending.

## Guarantee matrix

Investigation pending.

## Current test-protection map and fresh baseline

Investigation pending.

## Redundancy risk classification

Investigation pending.

## Minimal recommended change set

Investigation pending.

## Compatibility and rollback

Investigation pending.

## Unproven assumptions

Investigation pending.

## Evidence-backed implementation-plan revision

Investigation pending.

## Reproduction commands

Investigation pending.
