# Inter-Agent Notification Convergence Phase 0 Investigation

Status: Phase 0 correction complete; awaiting independent AUTO GATE rereview

The governing specification is the town-operations file
`260902-inter-agent-notification-convergence-and-alert-deduplication-repair-spec.md`.
It is maintained outside this source checkout under the town-level
`docs/superpowers/specs/` tree. This report is self-contained for review; the
post-gate documentation task mirrors the specification into this checkout.

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
  omit a fingerprint.
- The durable-mail/manual-nudge duplicate crosses two shared CLI entry points:
  `runMailSend` stores through `mail.Router.Send` and `NotifyPersisted`, while a
  later recovery command enters `runNudge` and creates a fresh delivery ID.
  `waitForMailNotifications` exposes only aggregate notification status, and
  `gt mail send` does not expose the stored direct-message ID, so a caller
  currently cannot bind that fallback to the durable source.
- The repository contains no source-owned function or formula step that
  unconditionally pairs those commands for the same state. The observed pairs
  are agent/operator recovery sequences. Existing Boot, Deacon, lease, and
  patrol nudges are standalone health or work signals and must not be migrated
  merely because their files also contain mail commands.
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
refresh the same canonical record for repeated observations. The fingerprint
identifies the incident family and stable monitored subject; a separate
material-state key identifies severity and current affected scope. Emit one new
wake only for a material transition or recurrence after closure.

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
reuse the durable source identity and consult its eligibility. The shared
producer interface is `gt mail send --json` returning every stored recipient
message ID plus `gt nudge --source-mail <message-id>` validating the same
recipient and copying that source into every direct, queued, and transport-
fallback record. No `--source-mail` means the existing standalone behavior.

## Current test-protection map and fresh baseline

### Environment and inventory

Phase 0 used Go 1.24.4 on Darwin arm64, tmux 3.7, Dolt 2.1.8, and
Homebrew ICU 77 for race builds. `go test -list .` compiled all ten planned
packages and enumerated 2,778 tests. All fixtures were package-owned temporary
directories, private tmux sockets, or in-memory stores; tests requiring an
explicit isolated Dolt port skipped rather than using the canonical server.

The exact plan-focused normal and race commands passed in `internal/mail`,
`internal/nudge`, `internal/delivery`, `internal/tmux`, `internal/acp`,
`internal/witness`, `internal/reaper`, `internal/deacon`, and `internal/dog`.
Both commands failed in `internal/cmd` on the same two tests; the race run
reported no data race before that functional failure.

### Preserved first failure and diagnosis

The first failure was
`TestCompleteRetirementRecordKeepsPendingAfterStoredMailSendError`; the paired
`TestCompleteRetirementRecordRenewsPortableDeliveryAfterWitnessRestart` failed
for the same reason. Each test passes alone, and the pair passes for twenty
shuffled repetitions. The failure reduces to this order:

1. `TestNudgeValidModesAccepted` invokes `runNudge` from the source checkout.
2. `runNudge` calls `session.InitRegistry` for the surrounding town.
3. The test restores its CLI flags and environment but not the process-global
   prefix registry.
4. The following retirement test's fixed `gt-witness` authority no longer
   matches the reloaded registry and is rejected before its intended injected
   mail failure.

The two-test reproduction is deterministic under shuffle seed
`1788441383814540000`; the reverse order passes. This is an isolated-test
pollution defect and a required pre-green repair, not evidence that retirement
delivery itself regressed. Phase 0 did not edit the test.

### Executable liveness protections

- Idle tmux delivery and router handoff:
  `TestNotifyRecipient_StartupIdleProofSurvivesRouterHandoff`.
- Busy tmux queueing and safe-boundary preservation:
  `TestNotifyRecipient_BusyAgent` and
  `TestPollerCustomPromptBusyDoesNotClaimQueue`.
- Absent/headless retention:
  `TestNotifyRecipient_CanonicalAliasQueuesAllHeadlessCandidates` and
  `TestPropeller_DeliverNudges_RequeuesWhenSessionUnavailable`.
- Transport failure and retry ownership:
  `TestNotifyRecipient_QueuedRetryStarterFailureIsVisible`,
  `TestNackSanitizesErrorAndDefersRetry`, and
  `TestSettlePollerClaimRequiresRecoverableStateAfterAckAndNackFailures`.
- Output versus acceptance:
  `TestNudgeQueueTypedOnlySurvivesUntilMatchingRuntimeReceipt`,
  `TestClaimDueRequiresMatchingPostClaimReceipt`, and the three focused
  control-plane receipt tests.
- Generation replacement:
  `TestReplaceBeforeStoppingPollerGenerationPreservesLivePollerOnReplacementFailure`,
  `TestStopRequestedRejectsStaleGeneration`, and the portable mail-work tests.
- Exact ordinary-record convergence:
  `TestAcknowledgeDeliveryBeadConvergesPendingLabel`,
  `TestRemoveKindByThread`, and `TestClearReplyReminders`.
- Reaper incident identity:
  `TestReconcileAnomaliesFiveIdenticalSnapshotsCreateOneOccurrence`,
  `TestReconcileAnomaliesChangedAffectedSetReplacesOccurrence`,
  `TestReconcileAnomaliesRecurrenceLinksLatestClosedOccurrence`, and the
  persistent command-level five-patrol test.

The listed exact selections passed normally and under the race detector in
all eight exercised packages: mail, nudge, delivery, tmux, ACP, Reaper, cmd,
and control-plane health.

### Duplication and stress baseline

- Five identical Reaper scans converge on one occurrence, escalation, and
  mail. A changed affected set replaces the occurrence; a resolved incident can
  recur as one new linked occurrence.
- Ordinary thread removal deletes only the requested kind and thread while
  preserving a different kind on that thread and the same kind on another
  thread.
- An informational `TypeNotification` from a replyable sender currently creates
  a reminder. This is a deterministic RED against the new explicit
  response-required contract.
- Twenty observations in one unchanged non-Reaper incident fixture are
  unproven. Current general escalation convergence depends on callers supplying
  the optional fingerprint.
- A rapid five-SHA Witness lineage is unproven because current durable records
  have no explicit lineage or binding-verdict metadata.
- Normal-priority ACP while busy is unproven by tests and source-traced RED:
  `notify` emits a UI update, skips `InjectPrompt`, returns nil, and the caller
  acknowledges and deletes the claim as submitted.
- In-flight claim convergence is unproven and source-traced RED:
  `RemoveKindByThread` ignores `.claimed` records.
- Queue-removal failure followed by next-injector self-healing is unproven.
  `mail check` discards the removal error, and injectors do not recheck durable
  terminal state.

### Isolation result

The post-test read-only inventory matched the pre-test baseline: one canonical
Dolt server; zero configured-port imposters, owned-town leaks, or owned-test
leaks; nineteen unknown listeners; and one unrelated orphan database. No live
message, escalation, queue, session, agent, or database was mutated. Sanitized
command results are retained in the ignored local Phase 0 archive.

## Redundancy risk classification

### Required defense in depth

- Durable Beads state plus an ephemeral wake are distinct layers. Removing
  either would recreate autonomous stalls or erase audit history.
- Direct idle tmux delivery plus queued poller fallback protects distinct
  receipt, busy-session, and unavailable-session failures.
- `mail check` and the nudge poller are independent consumers. The hook exposes
  durable unread mail at a real prompt boundary; the poller wakes an otherwise
  idle agent that would never reach another boundary.
- Typed-versus-submitted receipt validation, delivery leases, Nack retry,
  stale-claim recovery, and generation custody each protect a demonstrated
  loss or wrong-recipient race.
- Candidate-alias fan-out for an absent ambiguous session protects routing
  uncertainty. It must converge by durable source, not be reduced to an
  arbitrary single alias.
- Reaper fingerprint, family transition, occurrence, and send-once state are
  required. They already implement the desired recurrence model.

### Temporary compatibility

- Existing queue JSON without a durable source key must remain readable and
  wake-eligible. Suppressing an unidentifiable legacy record would risk loss.
- Existing mail without notification-source or review-lineage metadata must
  keep its current read, delivery, and thread behavior. No heuristic backfill
  is safe.
- Standalone manual nudges without a durable source remain intentionally
  ephemeral. They are not automatically deduplicated.
- Manual escalations without a fingerprint retain current create-new behavior.
  The repair targets recurring machine producers that can supply a real key.

### Accidental duplication

- Router reminders for every non-reply message duplicate informational mail
  and can enqueue more than one reminder for one unchanged thread.
- Repeated automated `gt escalate` calls without `--fingerprint` create a new
  incident, durable mail, wake, and reminder for each sample even when only a
  latency value or diagnostics path changed.
- A protocol that sends durable mail and then an unrelated manual nudge creates
  two wake identities which cannot converge after source completion.
- Best-effort ordinary queue removal leaves claimed records and hides removal
  errors. The later claim can display an already terminal source.
- Generic Witness verdict mail preserves history but lacks a durable lineage
  key, so every superseded exact can remain independently wake-eligible.

The executable receipt, queue, hook, and Reaper tests prove that eliminating
these duplicate *wake identities* does not require eliminating their distinct
delivery transports or durable records.

### Unknown and therefore preserved

- Witness's process-local message-ID deduplicator may still prevent a separate
  within-session handler replay. It is not a substitute for durable lineage and
  remains unchanged until a dedicated handler test proves otherwise.
- Queued records without a source key, unsupported runtime-specific fallbacks,
  and legacy messages without explicit protocol metadata remain eligible.
- An active `.claimed` file remains owned by its consumer lease. External
  thread cleanup must not delete it; the owner must self-heal after a durable
  eligibility check.
- UI notification value for ACP users is not removed. Only the false claim that
  a UI update proves agent prompt acceptance is rejected.

## Minimal recommended change set

The smallest safe design keeps every current transport and introduces one
source-eligibility boundary immediately after a consumer claims a queued wake
and before it injects a prompt.

1. Add optional `SourceID` and `SourceKind` fields to `nudge.QueuedNudge`.
   Router-generated mail, escalation, and reminder wakes use one generated
   source ID that is also stored as a validated `wake-source:<id>` label on the
   durable message. Legacy or manual records leave the fields empty.
2. Make `gt mail send --json` return the exact stored message ID for each
   recipient, including fan-out copies. Add an optional
   `gt nudge --source-mail <message-id>` producer seam which resolves that
   message, rejects a recipient mismatch, and reuses its wake-source identity.
   Source-owned protocol fallbacks must use this seam; standalone health and
   coordination nudges continue without a source.
3. Add a mail-package eligibility query which uses the source ID, thread, typed
   labels, delivery/read/work state, and replies. A lookup failure returns
   `unknown`, which retains and Nacks the claim; only positive terminal or
   superseded proof may suppress it.
4. Add an exact claim terminalization operation. A consumer holding the
   delivery lease may remove its own claim after positive durable proof without
   fabricating a submission receipt. `RemoveKindByThread` continues to ignore
   claims so another process cannot steal active custody.
5. Call the same eligibility query from the poller, idle watcher,
   prompt-boundary queue injector, and ACP Propeller. Report ordinary queue
   removal failures as partial convergence; the later claim then self-heals.
6. Make ACP `notify` distinguish UI-only observation from accepted prompt
   injection. A busy normal wake remains queued; only a successful
   `InjectPrompt` can produce the local ACP submission receipt.
7. Reuse `TypeTask` as the existing explicit response-required marker. Enqueue
   at most one reminder per source and thread. Informational notifications do
   not create reminders; exact reply or mail-work completion clears them.
8. Keep generic manual escalation behavior, but separate the stable incident
   fingerprint from the material-state key for recurring Deacon, dog, and
   cross-rig producers. Reuse one open canonical record, notify once for each
   state transition, and create a new occurrence only after closure.
9. Add minimal typed Witness review labels for lineage, generation, exact SHA,
   and verdict. A verdict must be an exact reply to its request. Only the
   highest unambiguous generation in one lineage is binding; conflicting
   metadata fails open and reports the ambiguity. Historical verdicts remain
   durable and queryable.
10. Restore the process-global prefix registry in the nudge CLI test so the
   unchanged focused suite becomes order-independent.

The generic source key and eligibility query are the shared convergence point.
Reminder, Witness, and escalation code supply typed identity; transport code
does not interpret subjects or bodies.

### Rejected alternatives

- **Subject or body matching:** text is neither stable identity nor lifecycle
  authority and would suppress unrelated work with similar wording.
- **A global time-window deduplicator:** it can drop a real material transition
  and loses state across restart.
- **Caller-by-caller stale-wake filters:** sibling producers and the next retry
  path would remain broken.
- **Broad queue cleanup:** it violates source and thread custody and risks
  deleting unrelated work.
- **Removing mail-check, poller, alias fan-out, or direct delivery:** each is a
  separately tested liveness defense.
- **Treating ACP UI output as acceptance:** it repeats the current silent-loss
  bug.
- **A new daemon, database table, or transport:** existing Beads labels,
  optional queue JSON, thread queries, receipts, and escalation fingerprints
  can express every invariant except explicit review lineage; typed labels are
  sufficient for that gap.

## Compatibility and rollback

This is a source-only, backward-compatible format extension with no database
schema migration:

- Go JSON readers ignore the new optional queue fields; new readers treat their
  absence as `unknown` and preserve delivery.
- Older binaries ignore the new Beads labels. New binaries do not infer typed
  identity for older messages.
- Unknown source kinds and malformed or conflicting review lineage fail open:
  preserve the wake, return a diagnostic, and do not mark it accepted.
- Existing standalone nudges and manual unfingerprinted escalations retain
  their current semantics.
- The new mail-send JSON result, source-bound nudge flag, escalation scope,
  material-state key, and material generation are additive. Older senders omit
  them; new readers fail open rather than infer them.
- Exact terminal claim removal requires a held lease plus a source-ID match;
  it cannot delete an unrelated record.
- No existing mail, verdict, escalation, receipt, or queue record is rewritten
  or migrated during installation.

Rollback is a source revert plus restoration of the prior installed binary.
Durable messages, verdicts, escalation history, and existing queue records stay
intact. The prior binary ignores the optional queue fields and added labels. A
rollback fixture must create mixed old/new queue records, restore the prior
binary, and prove both remain readable and actionable. No live cleanup or data
migration is part of rollback.

## Unproven assumptions

- Whether any supported ACP client treats a UI-only `session/update` as a safe
  agent work boundary. Current code and receipt semantics say it must not.
- Whether a current protocol carries machine-readable Witness review lineage
  outside the searched Beads message fields. No implementation or test was
  found.
- Whether any recurring non-Reaper incident producer already supplies a stable
  fingerprint indirectly. The traced formulas and helpers do not.
- Whether a claimed wake can be cancelled safely without violating the active
  consumer's delivery lease. A new exact claim-state seam is required.
- Whether queue filesystem failure can be reproduced without adding a test seam
  or relying on platform-specific permissions. Current tests cover Ack/Nack
  recovery but not terminal-source self-healing after removal failure.
- Whether twenty identical samples expose a limit absent from the existing
  five-snapshot Reaper test. Implementation acceptance must exercise all twenty
  in one fixture.

## Evidence-backed implementation-plan revision

The post-gate implementation plan should replace its placeholder source phases
with the following exact dependency order. Every task begins RED, makes the
smallest GREEN change, runs normal and race tests, checks listener/queue
residue, and commits locally without pushing.

### Source Task A — repair the baseline harness

- **Files/functions:** `internal/cmd/nudge_test.go`,
  `TestNudgeValidModesAccepted`, `setupNudgeTestRegistry`.
- **RED:** run the minimized two-test shuffle reproduction from this report.
- **GREEN:** save and restore `session.DefaultRegistry` around the nudge test;
  do not change production registry initialization.
- **Verify:** the minimized command, the full focused cmd regex normally and
  under race, and no new Dolt listener.
- **Commit:** `test: isolate nudge prefix registry`.

### Source Task B — bind queued wakes to durable source eligibility

- **Files/functions:** `internal/nudge/queue.go`, queue tests;
  `internal/mail/types.go`, `router.go`, `delivery.go`, and tests;
  `internal/cmd/mail_send.go`, `nudge.go`, `nudge_poller.go`, `mail_check.go`,
  and tests; `internal/acp/propulsion.go` and tests. Review and update only
  source-owned protocol templates proved to issue a delivery-failure fallback;
  the inventory found no current unconditional in-repo pair.
- **Interface:** optional queue source ID/kind, durable `wake-source` label,
  tri-state eligibility (`eligible`, `terminal`, `unknown`), and exact
  owner-held claim terminalization. `gt mail send --json` returns per-recipient
  stored IDs; `gt nudge --source-mail <id>` validates source/recipient binding
  and copies the source through immediate, queued, and urgent-fallback paths.
- **RED:** add
  `TestQueuedWakeTerminalSourceIsNotInjected`,
  `TestQueuedWakeEligibilityFailureRemainsRetryable`,
  `TestClaimedWakeSelfHealsAfterThreadRemovalRace`, and
  `TestMailCheckReportsPartialQueueConvergence`; add a CLI integration fixture
  proving mail plus source-bound fallback yields one visible wake after source
  acceptance, a recipient mismatch is rejected, and a standalone nudge stays
  source-free and deliverable.
- **GREEN:** stamp router wakes, surface exact stored IDs, bind an explicit
  protocol fallback to the same source, query Beads before injection, retain on
  unknown, and terminalize only the exact owned claim on positive proof.
- **Verify:** focused mail/nudge/delivery/cmd/ACP normal and race suites; mixed
  legacy/new queue records; mail-plus-fallback convergence; exact unrelated-
  source and standalone-nudge preservation.
- **Residue:** fixture queues empty only for terminal sources; no listener or
  live-state delta.
- **Commit:** `fix: converge queued wakes with durable sources`.

### Source Task C — preserve busy ACP wake liveness

- **Files/functions:** `internal/acp/propulsion.go`, `notify`,
  `deliverNudges`, and `propulsion_test.go`.
- **RED:** `TestPropellerBusyNormalNudgeRemainsQueued` must show that the current
  UI-only path deletes the claim.
- **GREEN:** return explicit prompt-acceptance state; Nack a busy normal claim
  until a safe boundary; keep urgent behavior and UI notification intact.
- **Verify:** ACP normal/race package plus idle, busy, absent, lease-contention,
  and failed-injector cases.
- **Residue:** private queue and proxy streams closed; no live ACP target.
- **Commit:** `fix: retain busy ACP notifications until acceptance`.

### Source Task D — make reply reminders explicit and exact

- **Files/functions:** `internal/mail/router.go`,
  `enqueueReplyReminder`, `ClearReplyReminders`, mail-work completion, and
  router/work-store tests.
- **RED:** add tests proving informational mail creates zero reminders, twenty
  retries of one task create one reminder, terminal action clears it, and
  unrelated thread/kind records survive.
- **GREEN:** treat only `TypeTask` as response-required, enqueue uniquely by
  source and thread, and reuse exact clearing plus Task B eligibility.
- **Verify:** mail/nudge/cmd normal and race suites and the reminder lifecycle
  fixture.
- **Residue:** one due reminder before action, zero after action, unrelated
  record unchanged.
- **Commit:** `fix: bound reply reminders to actionable mail`.

### Source Task E — converge recurring automated escalations

- **Files/functions:** `internal/cmd/escalate.go`, `escalate_impl.go`,
  `escalate_test.go`, `dog.go`, `capacity_dispatch.go`;
  `internal/beads/beads_escalation.go` and tests; recurring Deacon/dog formula
  call sites under `internal/formula/formulas/`; escalation transition labels
  and exact-message lookup in `internal/mail`.
- **Interface:** `--fingerprint` is the stable incident identity: condition
  family plus stable monitored-subject identity. It excludes severity,
  affected-set value, observation time, measurement, reason text, and
  diagnostics path. Add explicit normalized
  `--scope`/`EscalationFields.Scope`; the material-state key is the normalized
  severity plus scope. Routing derived from severity is therefore part of
  material state without hashing volatile prose. A positive material generation
  increments only after a state-key change; transition mail is keyed by
  escalation ID, generation, and recipient.
- **RED:** add one fixture with twenty identical observations, one severity
  transition, one affected-scope transition, closure, and recurrence. Add a
  concurrent identical-transition case. Assert one canonical open bead, one
  initial wake, exactly one wake per material transition, and one new
  occurrence/wake after closure.
- **GREEN:** on a matching open fingerprint, compare the stored material-state
  key. For an identical state, compare-and-update only the same bead's latest
  observation fields and suppress mail/wake. For a changed state, atomically
  update the same bead's severity, scope, title/reason/source, state key, and
  incremented generation. Then ensure one same-thread transition mail/wake per
  generation and recipient; a retry finds the durable transition mail instead
  of creating another, and a CAS loser re-reads the winning generation. Exclude
  closed occurrences from open matching so a later recurrence creates a new
  bead. Multiple open matches or lookup/update failure return an error and send
  nothing. Require recurring machine producers to supply the stable fingerprint
  and explicit scope.
- **Verify:** escalation/Reaper/formula/cmd normal and race tests plus embedded
  formula validation.
- **Residue:** one open recurrence fixture incident and no duplicate or orphaned
  transition mail; fixture removed.
- **Commit:** `fix: fingerprint recurring agent escalations`.

### Source Task F — model Witness binding verdict lineage

- **Files/functions:** `internal/mail/types.go`, label validation and parsing in
  `router.go` and `types.go`, `internal/cmd/mail_send.go`, Witness review
  protocol handlers/templates, and focused tests.
- **Interface:** validated review lineage, positive generation, exact 40-hex
  SHA, verdict enum, and exact `ReplyTo` request binding. No text heuristics.
- **RED:** add a rapid five-generation lineage fixture with five durable
  verdicts and only generation five wake-eligible; add malformed, conflicting,
  and out-of-order cases that fail open.
- **GREEN:** persist typed labels, inherit them only through an exact reply, and
  let Task B's eligibility query select the highest unambiguous generation.
- **Verify:** mail/Witness/cmd normal and race suites and the five-SHA fixture.
- **Residue:** five queryable verdicts, one eligible wake, no live mail changes.
- **Commit:** `fix: converge Witness verdict wake lineage`.

### Integration and installed acceptance

- Update `docs/design/architecture.md`, the maintained mail protocol, formula
  embeds, and the source/town mirrored specification only after behavior is
  green.
- Run format, vet, build, focused normal/race, full suite, Gitleaks, mixed-version
  rollback, and listener/queue residue checks.
- Obtain an independent exact-SHA source verdict, install only the approved
  binary into a disposable fixture, and exercise every transport/session row,
  twenty identical incidents, one material transition, five-SHA lineage,
  reminder lifecycle, and queue-removal self-heal.
- Restore the prior installed artifact after the disposable gate. Never mutate
  live Mayor, Witness, Deacon, dog, polecat, mail, escalation, or queue state.

## Reproduction commands

Test inventory and the plan-focused baseline:

```bash
CGO_ENABLED=0 go test -list . \
  ./internal/mail ./internal/nudge ./internal/delivery ./internal/tmux \
  ./internal/acp ./internal/witness ./internal/reaper ./internal/deacon \
  ./internal/dog ./internal/cmd

CGO_ENABLED=0 go test \
  ./internal/mail ./internal/nudge ./internal/delivery ./internal/tmux \
  ./internal/acp ./internal/witness ./internal/reaper ./internal/deacon \
  ./internal/dog ./internal/cmd \
  -run 'Mail|Nudge|Queue|Delivery|Receipt|Reminder|Escalat|Supersed|Poller|Prompt|ACP|Generation|Absent|Busy|Idle' \
  -count=1

ICU_PREFIX="$(brew --prefix icu4c@77)"
CGO_CPPFLAGS="-I${ICU_PREFIX}/include" \
CGO_LDFLAGS="-L${ICU_PREFIX}/lib" \
go test -race \
  ./internal/mail ./internal/nudge ./internal/delivery ./internal/tmux \
  ./internal/acp ./internal/witness ./internal/reaper ./internal/deacon \
  ./internal/dog ./internal/cmd \
  -run 'Mail|Nudge|Queue|Delivery|Receipt|Reminder|Escalat|Supersed|Poller|Prompt|ACP|Generation|Absent|Busy|Idle' \
  -count=1
```

Minimized registry-pollution failure:

```bash
CGO_ENABLED=0 go test ./internal/cmd \
  -run '^(TestNudgeValidModesAccepted|TestCompleteRetirementRecordKeepsPendingAfterStoredMailSendError)$' \
  -count=1 -shuffle=1788441383814540000 -v
```

Representative exact user-path selections:

```bash
CGO_ENABLED=0 go test ./internal/mail \
  -run 'TestNotifyRecipient_|TestClearReplyReminders|TestAcknowledgeDeliveryBeadConvergesPendingLabel' \
  -count=1
CGO_ENABLED=0 go test ./internal/nudge ./internal/delivery \
  -run 'TestClaimDueRequiresMatchingPostClaimReceipt|TestNackSanitizesErrorAndDefersRetry|TestRemoveKindByThread|TestPromptSubmittedReceipt' \
  -count=1
CGO_ENABLED=0 go test ./internal/acp ./internal/reaper ./internal/health \
  -run 'TestPropeller_|TestReconcileAnomalies|TestEvaluateControlPlane' \
  -count=1
CGO_ENABLED=0 go test ./internal/cmd \
  -run 'TestNudgeQueueTypedOnly|TestPollerCustomPromptBusy|TestSettlePollerClaim|TestReconcileAnomalyScansFive' \
  -count=1
```
