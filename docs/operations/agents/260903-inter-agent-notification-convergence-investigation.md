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

The implementation plan's initial static path list omitted
`internal/mail/router.go`. The end-to-end trace found that file is the shared
mail-to-wake convergence point. The Phase 0 plan must therefore add it before
source work begins.

### Producers

- `gt mail send` stores one Beads message through `mail.Router`, labels delivery
  pending, then asynchronously notifies every resolved recipient session.
- `gt escalate` creates a durable escalation and sends durable mail to each
  target. It converges only when its caller supplies the same explicit
  `--fingerprint`; without that flag each observation creates a new incident.
- Reaper anomaly reconciliation computes a stable fingerprint, reuses the
  current exact occurrence, resolves superseded occurrences, and stores mail
  once per occurrence.
- Witness, Deacon, dog, callback, patrol, lifecycle, and formula code invokes
  the generic mail or escalation paths. Most recurring `gt escalate` templates
  omit a fingerprint; several protocols also add manual nudges independently.
- `mail.Router.enqueueReplyReminder` creates a delayed queued nudge after every
  non-reply message from a replyable sender. No explicit response-required bit
  is checked.
- `gt nudge` creates an ephemeral direct or queued wake. A caller retry creates
  a new delivery identity and has no durable-source convergence key.

### Durable stores and keys

- Beads mail, escalation, thread, reply, delivery, read, and lifecycle labels
  are the durable audit source.
- `MailWorkStore` adds exact work-generation claim and completion custody for
  actionable mail. It is not the wake queue.
- `.runtime/nudge_queue/<session>/` holds one JSON record per queued wake. Its
  delivery ID owns the claim; kind and thread ID support limited convergence.
- Submitted receipts bind a session and delivery ID to runtime acceptance.
- Witness `MessageDeduplicator` is process-local and keyed only by message ID.
  It neither survives restart nor models exact-SHA review lineage.
- Tmux pane text and ACP UI updates are observations, not durable acceptance.

### Transports and consumers

- Idle tmux delivery uses `NudgeSessionWithReceipt`; only a matching submitted
  receipt proves acceptance.
- Busy or unverified tmux delivery queues the wake and starts the nudge poller.
  The poller waits for a safe boundary, claims under a lease, and Ack/Nacks from
  the receipt result.
- `gt mail check --inject` is the prompt-boundary consumer. It prints unread
  mail before acknowledging delivery, then attempts thread-scoped queue
  removal and drains one queued wake.
- ACP `Propeller` watches the same queue, claims under the same lease, emits a
  UI update, and conditionally injects an agent prompt.
- `gt mail inbox`, `read`, `check`, `claim`, and reply operations update durable
  presentation, delivery, ownership, or completion state. A reply also asks the
  router to clear reminders for the thread.
- No-session routing preserves durable mail and queues candidate session aliases
  for a later supported consumer.

### Current convergence ownership

- A tmux poller owns retry until a matching receipt, expiry, terminal session,
  or a recoverable queued/claimed record remains.
- ACP owns a claim only for one delivery attempt, but currently fabricates its
  submitted receipt after `notify` returns rather than receiving independent
  runtime proof.
- `mail check` removes queued `mail` and `escalation` kinds after delivery
  acknowledgement. It discards removal errors.
- `RemoveKindByThread` removes only ordinary queue records. It deliberately
  leaves `.claimed` records untouched, so an in-flight stale wake can survive a
  terminal durable transition.
- Only a reply clears `reply-reminder` records today. Read, acknowledgement,
  close, and supersession do not share that cleanup path.
- Reaper owns its anomaly lifecycle and is the strongest existing example of
  stable fingerprint, material-transition, and send-once convergence.
- During this investigation, two metadata-only wakes for already superseded
  review exacts arrived. Their bodies and live records were not read or
  mutated. The observation confirms that terminal source state is not checked
  by every injector.

## Guarantee matrix

Each row below describes current behavior. "Needed" is the minimum behavior
that implementation must prove without weakening the current liveness floor.

### Idle tmux recipient

| Field | Value |
| --- | --- |
| Durable source | Beads mail or escalation |
| Observation | Direct tmux prompt injection |
| Acceptance | Matching submitted receipt |
| Retry owner | Router queues; poller retries |
| Defensive path | Queue after failed or unverified direct send |
| Terminal suppression | Thread removal after mail-check acknowledgement |

Needed: retain receipt proof and queue fallback; validate the durable source
again before any queued injection.

### Busy tmux recipient

| Field | Value |
| --- | --- |
| Durable source | Beads mail or escalation |
| Observation | Poller at a verified idle boundary |
| Acceptance | Matching submitted receipt |
| Retry owner | Queue poller under delivery lease |
| Defensive path | Prompt-boundary hook can expose durable mail |
| Terminal suppression | Partial; claimed records survive removal |

Needed: preserve safe-boundary delivery and suppress a claim whose exact source
became terminal before injection.

### Recipient session absent

| Field | Value |
| --- | --- |
| Durable source | Beads mail or escalation |
| Observation | Candidate aliases receive queued wakes |
| Acceptance | Later consumer-specific receipt or boundary |
| Retry owner | Next supported session consumer |
| Defensive path | Durable mailbox remains independently readable |
| Terminal suppression | No common pre-injection source check |

Needed: keep mail durable across absence and generation replacement; discard
only a wake proven terminal for its exact source.

### Recipient restart or generation change

| Field | Value |
| --- | --- |
| Durable source | Beads plus work-generation custody |
| Observation | New generation reads mail or alias queue |
| Acceptance | Generation-bound claim for mail work |
| Retry owner | Mail work recovery and queue consumer |
| Defensive path | Portable generation validation |
| Terminal suppression | Queue identity is not generation-bound |

Needed: preserve portable work custody and prevent an old wake from reviving a
terminal source in a replacement session.

### Tmux injection or receipt failure

| Field | Value |
| --- | --- |
| Durable source | Beads source plus queued wake |
| Observation | Later poller retry |
| Acceptance | Typed output alone is rejected |
| Retry owner | Router or poller Ack/Nack loop |
| Defensive path | Durable queue and recoverable claim |
| Terminal suppression | Source is not rechecked before retry |

Needed: retain the exact receipt contract; add terminal-state convergence at
the last common point before injection.

### Prompt-boundary mail hook

| Field | Value |
| --- | --- |
| Durable source | Unread Beads mail |
| Observation | `gt mail check --inject` output |
| Acceptance | Hook runs at a real user prompt boundary |
| Retry owner | Future hook if mailbox read or ack fails |
| Defensive path | Direct wake and poller avoid idle stalls |
| Terminal suppression | Removal errors are ignored |

Needed: keep the independent mailbox observation path, report partial cleanup,
and let later injectors self-heal from durable state.

### ACP idle recipient

| Field | Value |
| --- | --- |
| Durable source | Beads source plus queued wake |
| Observation | UI update and ACP prompt injection |
| Acceptance | Locally synthesized receipt after success |
| Retry owner | ACP Nack on returned injection error |
| Defensive path | Queue remains on proxy/session failure |
| Terminal suppression | No durable-source preflight |

Needed: retain queue durability and require an acceptance event that cannot be
synthesized after UI-only output.

### ACP busy recipient

| Field | Value |
| --- | --- |
| Durable source | Beads source plus queued wake |
| Observation | UI update only for normal priority |
| Acceptance | Incorrectly synthesized as submitted |
| Retry owner | None after the false Ack |
| Defensive path | Mail remains readable, but wake is lost |
| Terminal suppression | Claim is deleted despite no prompt |

Needed: RED. A busy normal-priority wake must remain queued until the next safe
boundary; UI decoration is not agent acceptance.

### No tmux or ACP transport

| Field | Value |
| --- | --- |
| Durable source | Beads mail or escalation |
| Observation | Later inbox, hook, or replacement session |
| Acceptance | Consumer-specific delivery or claim proof |
| Retry owner | Durable mailbox and queued alias |
| Defensive path | Mail remains independently queryable |
| Terminal suppression | No shared queue eligibility check |

Needed: preserve offline durability without treating queue presence as a second
source of truth.

### Dolt or mailbox read failure

| Field | Value |
| --- | --- |
| Durable source | Beads; read result is unknown |
| Observation | Later mail-check or inbox retry |
| Acceptance | None on read failure |
| Retry owner | Subsequent consumer invocation |
| Defensive path | Existing wake remains queued |
| Terminal suppression | Must fail open for actionable eligibility |

Needed: an inability to prove terminal state must retain the wake and return a
truthful partial or retryable result.

### Queue filesystem write or read failure

| Field | Value |
| --- | --- |
| Durable source | Beads remains authoritative |
| Observation | Direct path or later mailbox check |
| Acceptance | No queue acceptance on filesystem failure |
| Retry owner | Router reports queued/failed result; later sender |
| Defensive path | Durable mail remains readable |
| Terminal suppression | Not applicable until a record exists |

Needed: surface the error without rolling back durable mail and retain another
supported observation path.

### Queue acknowledgement or deletion failure

| Field | Value |
| --- | --- |
| Durable source | Beads plus queued or claimed record |
| Observation | Same or later injector retries |
| Acceptance | Receipt may exist while cleanup failed |
| Retry owner | Poller protects recoverable claims |
| Defensive path | Stale-claim recovery prevents silent loss |
| Terminal suppression | Mail-check discards removal errors |

Needed: return partial convergence and make the next injector consult the
durable source before showing the stale wake.

### Duplicate producer or retry

| Field | Value |
| --- | --- |
| Durable source | Varies by producer |
| Observation | One wake per fresh delivery ID |
| Acceptance | Per-delivery receipt only |
| Retry owner | Each producer or queue item independently |
| Defensive path | Some duplicates protect distinct transports |
| Terminal suppression | No general source-identity convergence |

Needed: deduplicate only by verified durable source identity and material
state, never by similar free text.

### Read or delivery acknowledgement

| Field | Value |
| --- | --- |
| Durable source | Beads read and delivery labels |
| Observation | Mail command or prompt-boundary hook |
| Acceptance | Durable label update after presentation |
| Retry owner | Mail command on failed label update |
| Defensive path | Queue remains if acknowledgement fails |
| Terminal suppression | Best-effort thread removal, not claims |

Needed: keep presentation separate from actionable completion while making an
exact acknowledged source ineligible at every later injector.

### Reply reminder lifecycle

| Field | Value |
| --- | --- |
| Durable source | Original mail thread |
| Observation | Delayed queued reminder |
| Acceptance | None; timer expiry makes it due |
| Retry owner | Queue consumers until TTL |
| Defensive path | Reminder protects forgotten required replies |
| Terminal suppression | Reply only; other terminal states do not clear |

Needed: require explicit response-required intent, allow at most one unchanged
reminder, and clear it on every exact terminal transition.

### Fingerprinted escalation

| Field | Value |
| --- | --- |
| Durable source | Open escalation bead |
| Observation | Router mail and wake to each target |
| Acceptance | Target-specific mail delivery state |
| Retry owner | Mail router and queue consumer |
| Defensive path | Explicit fingerprint reuses open incident |
| Terminal suppression | Only when every caller supplies the key |

Needed: recurring producers must compute a stable fingerprint by default and
emit one new wake only for a material transition or recurrence after closure.

### Reaper anomaly reconciliation

| Field | Value |
| --- | --- |
| Durable source | Fingerprinted anomaly occurrence |
| Observation | Mail once per current occurrence |
| Acceptance | Persisted mail-stored state after send |
| Retry owner | Reconciler retries unrecorded send failure |
| Defensive path | Family and exact fingerprints preserve transitions |
| Terminal suppression | Current duplicate and superseded rows resolve |

Needed: preserve this existing model and reuse it for periodic incident
producers rather than adding a second deduplication subsystem.

### Witness exact-SHA verdict lineage

| Field | Value |
| --- | --- |
| Durable source | One durable mail record per verdict |
| Observation | Generic mail wake and reminder paths |
| Acceptance | Delivery state, not binding-lineage state |
| Retry owner | Router and queue consumers |
| Defensive path | Durable history preserves every verdict |
| Terminal suppression | No structured SHA-lineage convergence exists |

Needed: retain all verdict records, require explicit lineage and binding
metadata, and make only the newest binding verdict wake-eligible.

### Manual nudge

| Field | Value |
| --- | --- |
| Durable source | None unless a protocol separately sent mail |
| Observation | Direct, wait-idle, or urgent queued injection |
| Acceptance | Receipt for supported direct delivery |
| Retry owner | Wait-idle watcher or urgent queue |
| Defensive path | Useful for verified delivery failure |
| Terminal suppression | New identity on every caller retry |

Needed: leave standalone ephemeral nudges alone; protocol fallback nudges must
reuse the durable source identity and consult its eligibility.

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
