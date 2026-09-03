# Inter-agent notification convergence and alert deduplication repair

**Date:** 2026-09-02

**Status:** Active; mandatory Phase 0 investigation required before implementation

**Scope:** Gas Town mail, nudge, escalation, and autonomous-agent wake-up behavior

## Summary

Gas Town must reduce repeated stale Witness verdicts, repeated Deacon and dog
incident escalations, and reply reminders that no longer require action without
weakening the communication paths that keep autonomous work moving.

The apparent redundancy may be intentional defense in depth. Durable mail,
queued nudges, tmux and ACP delivery, prompt-boundary injection, session
generation custody, receipts, and retry behavior were added over time to cover
different failure modes. Removing one path because it looks duplicative could
leave an idle agent unwoken, strand actionable work after a restart, or make a
busy agent miss work at its next safe boundary.

Accordingly, this repair is ordered around one rule:

> Prove no-loss communication and autonomous progress before optimizing
> duplicate delivery.

Before any source change, a non-mutating Phase 0 investigation must recover the
historical rationale, trace every current producer and delivery path, and
establish a failure-injection baseline. An independent reviewer must approve
that evidence. Only then may implementation begin, and the investigation may
revise or reject the proposed mechanism while preserving the behavioral goals
in this specification.

No preliminary investigation has been performed as part of writing this
specification.

## Problem statement

Operators currently receive repeated notifications that appear obsolete:

- an exact-SHA Witness verdict can be delivered more than once after its
  durable source has already been read or acknowledged;
- a sequence of newer Witness verdicts can leave older verdict wake-ups visible
  even though only the newest binding verdict can unblock work;
- Deacon and dogs can create multiple urgent escalations for recurring samples
  of one unresolved incident;
- a reply reminder can arrive after the underlying action was already taken or
  when the original message did not require a reply; and
- a durable mail plus a separate manual nudge can produce two independent
  wake-ups for the same state.

The resulting volume competes with genuinely new work. However, duplicate
suppression is not allowed to create the opposite failure: a durable message
that exists but never wakes the intended agent, an idle agent that remains
idle, or a busy agent that never sees work at a safe command boundary.

## Safety and precedence

The following precedence is binding:

1. No actionable work may be silently lost or left permanently unwoken.
2. Durable records must remain truthful and auditable.
3. Autonomous agents must resume when newly actionable work becomes available.
4. Duplicate or obsolete wake-ups should converge away only after the first
   three guarantees are proven.

If deduplication and liveness conflict, fail toward preserving the durable
source and one retryable wake-up. Report the partial convergence explicitly;
do not silently drop the source or pretend delivery was accepted.

## Goals

1. Preserve at-least-once wake-up behavior for new actionable work across every
   supported transport and session state.
2. Make durable mail or escalation state authoritative for whether a queued
   wake-up remains actionable.
3. Converge exact queued wake-ups after read, acknowledgement, close, reply, or
   explicit supersession.
4. Preserve every formal exact-SHA review verdict durably while waking only for
   the newest binding verdict that can affect current work.
5. Reuse stable escalation fingerprints so repeated observations of one open
   incident update its canonical record instead of creating new urgent mail.
6. Generate a new wake-up for a material state transition or a genuine
   recurrence after resolution.
7. Make reply reminders explicit, bounded, and self-cancelling.
8. Keep the implementation small by reusing verified existing primitives.

## Non-goals

- Reducing or deleting durable review, mail, escalation, or incident history.
- Weakening urgent delivery or changing all notification delivery to global
  at-most-once semantics.
- Treating mail delivery, message read, escalation acknowledgement, and work
  completion as equivalent lifecycle states.
- Adding a daemon, event bus, second queue, or new communications subsystem.
- Broadly clearing the live notification queue or rewriting current mail and
  escalation state.
- Changing unrelated agent lifecycle, assignment, or role ownership rules.
- Implementing or investigating the repair while this specification is being
  written.

## Existing mechanisms are hypotheses until Phase 0 verifies them

Previous observations suggest that Gas Town already has thread-scoped queue
removal, reply-reminder clearing, delivery acknowledgement, transport receipts,
session-generation custody, and escalation fingerprints. They are candidate
reuse points, not accepted facts or implementation instructions.

Phase 0 must verify their current behavior, callers, failure semantics, and
historical purpose. It must also identify any deliberate redundant path that
protects a failure mode not covered elsewhere. No mechanism may be removed or
collapsed merely because two happy-path executions look similar.

## Mandatory Phase 0 — historical and behavioral investigation

### Boundary

Phase 0 is evidence gathering only. It must not:

- edit Gas Town source or tracked documentation other than its investigation
  report;
- mutate live mail, escalation, queue, session, or agent state;
- read, acknowledge, close, reply to, supersede, or clear a live message for
  testing;
- run broad queue cleanup, session restart, doctor fix, or process cleanup; or
- use current production agents as disposable delivery fixtures.

Behavioral experiments must use an isolated fixture town, disposable agents,
private mailboxes and queues, and non-production sessions. The investigation
must capture commands, versions, fixture identities, and results sufficiently
for an independent reviewer to reproduce them.

### Historical reconstruction

Build a dated map of the major changes that introduced or modified:

- durable mail delivery and acknowledgement;
- direct and queued nudges;
- mail-check and prompt-boundary injection;
- tmux and ACP delivery and receipt handling;
- absent-session retry and session-generation custody;
- busy-agent versus idle-agent delivery;
- reply reminders;
- Witness exact-SHA verdict delivery and supersession;
- Deacon, dog, and Reaper escalation production and fingerprinting; and
- any poller, hook, or fallback intended to prevent autonomous stalls.

Use Git history, archived specifications and plans, decisions, issue and mail
records, incident notes, tests, and maintained protocol documentation. For each
change, record the failure it addressed, the guarantee it intended to add, and
whether that guarantee is still independently covered.

Historical intent is evidence, not proof of current behavior. Every claimed
guarantee must also have a current executable test or a fresh isolated
reproduction.

### Producer and consumer inventory

Trace the end-to-end path for every notification producer and consumer,
including at minimum:

- mail send, read, check, claim, mark-read, reply, and delivery acknowledgement;
- escalation creation, fingerprint matching, acknowledgement, close,
  supersession, and stale handling;
- queued-nudge enqueue, claim, acknowledgement, negative acknowledgement,
  retry, expiry, and removal;
- session-generation creation and replacement;
- prompt, hook, poller, tmux, ACP, and no-session delivery;
- Witness formulas, templates, exact-SHA review protocols, and manual wake-ups;
- Deacon, dog, and Reaper formulas, templates, periodic observations, and
  escalation paths; and
- system-generated reply reminders.

For each producer, identify the durable source, wake-up key, intended recipient,
response requirement, retry owner, terminal states, supersession rule, and the
code or protocol that performs convergence.

### Guarantee matrix

Produce a matrix showing current behavior and required behavior for:

- idle recipient;
- busy recipient;
- recipient session absent;
- recipient restart and generation change;
- hook or prompt injection failure;
- successful and failed tmux receipt;
- ACP delivery;
- no tmux or ACP transport;
- Dolt or mailbox read failure;
- queue filesystem read, write, or deletion failure; and
- duplicate producer execution or retry.

For every cell, answer:

1. Where is the actionable state durably stored?
2. What causes the recipient to wake or observe it?
3. What confirms acceptance rather than mere output?
4. Who retries, and until what terminal condition?
5. Which apparent duplicate path protects this failure mode?
6. What prevents an acknowledged or superseded state from waking again?

### Failure-injection baseline

Before proposing a source change, add or run isolated tests that establish the
current baseline for both liveness and duplication. The baseline must include:

1. new actionable work wakes an idle agent;
2. a busy agent observes it at the next supported safe boundary;
3. absent-session work remains durable and wakes the next valid generation;
4. a failed transport or injector attempt remains retryable;
5. successful transport output without an acceptance receipt is not treated as
   proof of consumption;
6. restart or generation replacement neither loses work nor reactivates a
   terminal source indefinitely;
7. read or acknowledged sources do not remain visibly queued forever;
8. superseded Witness verdicts preserve their durable records while only the
   binding verdict remains wake-eligible;
9. repeated identical incident observations converge on one canonical open
   escalation; and
10. reply, acknowledgement, close, or supersession cancels the exact reminder
    without clearing unrelated notifications.

Record both passing and failing behavior. A test that only demonstrates the
duplicate symptom is insufficient; the same fixture must prove that the
proposed removal would not break a liveness path.

### Risk classification

Classify each redundant-looking path as one of:

- **required defense in depth** — protects a distinct demonstrated failure;
- **temporary compatibility** — required for a named supported version or
  transport;
- **accidental duplication** — repeats an already accepted unchanged state and
  protects no distinct failure; or
- **unknown** — evidence is insufficient, so implementation must preserve it.

The investigation must prefer the smallest convergence point shared by all
accidental duplicate producers. It must not patch only the most visible caller
when sibling callers can create the same stale wake-up.

### Required Phase 0 deliverables

The investigation packet must contain:

1. the historical timeline and commit map;
2. the producer and consumer inventory;
3. the guarantee matrix;
4. the current test-protection map and fresh baseline results;
5. the redundancy risk classification;
6. a minimal recommended change set, including rejected alternatives;
7. an exact compatibility and rollback plan;
8. a list of behavioral assumptions that remain unproven; and
9. a proposed implementation-plan revision tied to the evidence.

The recommendation must first attempt to reuse verified existing thread-scoped
queue removal, reply-reminder clearing, delivery acknowledgement, receipts,
session custody, and escalation fingerprints. A new persistent field, schema,
daemon, queue, or transport requires evidence that no existing primitive can
express a required invariant.

## AUTO GATE — Phase 0 investigation approval

- **Actor:** A non-authoring reviewer with no authorship of the investigation
  packet or proposed source change.
- **Preconditions:** All nine Phase 0 deliverables exist; the disposable fixture
  is identified; live state was not mutated; commands and results are
  reproducible; and every supported transport and session-state row is
  addressed.
- **Action:** Review the historical reconstruction, trace representative paths
  end to end, rerun the liveness and duplication baseline, and verify that each
  proposed removal or convergence change retains coverage for the failure mode
  the current redundancy was meant to protect.
- **On failure:** Mark the packet `CHANGES REQUIRED`, preserve the first
  failure, fill the evidence or test gap, and rerun the gate. Do not begin
  implementation and do not weaken a liveness criterion to make the gate pass.
- **Retry identity:** A materially revised, content-addressed investigation
  packet with fresh evidence receives a new review automatically.
- **Automatic resume:** An `APPROVED` verdict authorizes finalization of the
  evidence-backed implementation plan and immediate start of its first source
  phase when autonomous execution has been directed. No additional human
  prompt is required.

No Gas Town source implementation may begin before this gate is `APPROVED`.

## Required post-investigation behavior

The exact implementation is deliberately conditional on Phase 0. The following
outcomes are binding unless the investigation proves they conflict with a
stronger liveness guarantee, in which case the plan must document the conflict
and preserve liveness.

### Durable source and wake-up convergence

Durable mail and escalation records remain the audit history. A queued nudge is
an ephemeral wake-up derived from a durable source, not a second source of
truth.

Reading mail, acknowledging or closing an escalation, replying, or explicitly
superseding a source must make the exact unchanged wake-up ineligible. Queue
convergence must be exact by durable source and thread; it must not remove
unrelated work for the recipient.

Queue-removal failure must not roll back or falsify a completed durable action.
The command must report partial convergence, and a later injector must consult
the durable source and self-heal instead of displaying terminal work again.

### Witness verdicts

Every formal exact-SHA verdict remains durable. For one review lineage, only
the newest verdict that is binding on current work remains wake-eligible.
Superseded verdicts stay queryable but do not repeatedly interrupt the Mayor or
executor.

Verdict identity and supersession must come from explicit durable metadata or a
verified existing protocol. Subject-line or free-text heuristics are not an
acceptable authority.

### Deacon, dog, and Reaper escalations

Repeated observations of one unresolved condition must reuse a stable incident
fingerprint and update the canonical record. They must not create a new urgent
mail and wake-up for every sample.

A new notification is justified by a material state transition, such as
severity increase, affected-scope change, a newly required operator action, or
recurrence after the prior incident was resolved. Sampling time, another
latency measurement, or a new diagnostics path alone is not a material
transition.

### Reply reminders

A reply reminder may exist only when the sender or protocol explicitly marks a
reply as required. Informational mail and state handled through an
acknowledgement or close must not implicitly require a reply.

At most one reminder may remain eligible for an unchanged response-required
state. Reply, acknowledgement, close, or supersession clears it exactly.

### Manual nudges

Protocols must not pair durable mail with an unconditional manual nudge for the
same unchanged state. A manual nudge is allowed only as a documented fallback
for a verified delivery failure, and it must use the same source identity so
that acceptance or supersession converges both paths.

### Finalized source contract

The implemented source contract uses typed durable identity rather than message
text:

- each router-produced mail wake stores one validated `wake-source:<id>` label
  and queues the same source ID, source kind, and thread ID; JSON send receipts
  expose the stored message and source IDs, and protocol fallbacks bind through
  `gt nudge --source-mail`;
- after claiming a wake and before prompt injection, every consumer re-reads the
  durable source. Positive terminal state discards only that exact claim,
  unreadable or contradictory state is NACKed for retry, and source-free legacy
  or standalone nudges retain their prior delivery behavior;
- ACP UI observation is advisory. Busy or unaccepted prompt delivery remains
  queued until a matching runtime acceptance receipt exists;
- only task mail is response-required. Reminder enqueue is unique by kind,
  thread, and source, while reply, acknowledgement, close, or supersession
  clears only the matching reminder;
- recurring automated incidents require a stable fingerprint and explicit
  scope. Severity plus normalized scope define material state; compare-and-set
  generations persist their pending recipients before delivery, and
  deterministic per-recipient records make crash and concurrency retries
  idempotent. A closed occurrence is not reused; and
- formal review mail is permanent and carries `review-lineage`, positive
  `review-generation`, lowercase 40-hex `review-exact`, and an optional
  `review-verdict` of `approved` or `changes-required`. A request is a non-reply
  task. A verdict is an exact reply and inherits its authority only from that
  stored request. Cross-thread lineage lookup retains every generation; only
  the newest unambiguous generation's verdict remains wake-eligible. Malformed,
  conflicting, changed-between-read, or incorrectly bound lineage state fails
  open for retry.

## Acceptance criteria

### Liveness floor

1. Every supported transport and session-state row in the Phase 0 matrix has a
   deterministic test.
2. Synthetic new actionable work is observed by the intended idle agent in
   every supported transport scenario.
3. A busy agent observes synthetic work at its next supported safe boundary
   without unsafe interruption.
4. Work created while no recipient session exists remains durable and is
   observed by the next valid session generation.
5. Injected transport, hook, receipt, queue, and durable-store failures cannot
   silently discard unaccepted urgent work.
6. The executor demonstrates no liveness regression against the Phase 0
   baseline before evaluating duplicate suppression.

### Convergence and deduplication

7. A read mail, acknowledged or closed escalation, reply, or superseded source
   produces zero later visible delivery for that unchanged source.
8. Twenty identical fingerprinted observations of one unresolved incident
   produce one open canonical escalation and no more than its initial wake-up.
9. A material transition produces exactly one new wake-up.
10. A rapid five-SHA Witness review lineage preserves all five durable verdicts
    while only the newest binding verdict remains wake-eligible.
11. A response-required message produces at most one reminder, and reply,
    acknowledgement, close, or supersession makes it ineligible.
12. Clearing one source never clears unrelated queued work for the same
    recipient or thread.
13. Queue-convergence failure returns a truthful nonzero or partial result; the
    next injector suppresses terminal state from the durable source without
    losing still-actionable work.

### Engineering gates

14. Focused normal and race tests pass for every touched mail, nudge,
    escalation, session, transport, and agent-protocol package.
15. The repository's documented formatter, vet, build, and full test gates pass
    from a clean exact checkout.
16. Gitleaks and publication review confirm that fixtures and reports contain no
    private mail content, live identifiers, queue paths, session tokens, or raw
    incident evidence.
17. Maintained architecture and operator documentation describe the final
    durable-source, wake-up, retry, and convergence contract.

The Phase 0 reviewer may refine the exact definition of one delivery window
when transport semantics require it. The refinement must be explicit,
testable, and no weaker than at-least-once delivery of new actionable work plus
no delivery of terminal unchanged work.

## AUTO GATE — exact source review

- **Actor:** A spec-eligible non-authoring source reviewer.
- **Preconditions:** Phase 0 is `APPROVED`; the candidate exact SHA and parent
  are available in a clean checkout; the implementation plan maps every change
  to Phase 0 evidence; and focused normal, race, full-suite, build, vet, format,
  Gitleaks, and publication checks have completed.
- **Action:** Review the exact range, reproduce liveness and convergence tests,
  and verify that each changed delivery path retains its documented fallback.
- **On failure:** Store one binding `CHANGES REQUIRED` verdict, preserve the
  first failure, make the smallest root-cause repair, rerun affected and full
  gates, and request fresh exact-SHA review automatically.
- **Retry identity:** Each repaired exact SHA receives one new binding review;
  older SHA verdicts remain durable but are superseded for wake-up purposes.
- **Automatic resume:** An `APPROVED` exact-SHA verdict authorizes disposable
  installed acceptance automatically. No human prompt is required.

## AUTO GATE — disposable installed communication acceptance

- **Actor:** A spec-eligible non-authoring verifier who did not author the
  candidate or Phase 0 packet.
- **Preconditions:** Exact-source approval, matching installed-artifact digest,
  isolated fixture town, disposable agent sessions for each supported
  transport, clean fixture mail and queue state, and captured before-state.
- **Action:** Exercise the user-path liveness floor first, then convergence,
  Witness supersession, escalation recurrence, reply-reminder, and injected
  failure scenarios against the installed artifact.
- **On failure:** Preserve the first result and fixture identities, remove only
  disposable artifacts whose custody remains exact, return the candidate to
  diagnosis and fresh source review, and continue automatically while a safe
  in-scope path remains. Do not bypass a failed check or weaken delivery.
- **Retry identity:** A freshly approved SHA, matching installed artifact, and
  fresh disposable fixture receive one new acceptance attempt automatically.
- **Automatic resume:** All liveness and convergence criteria pass, fixture
  agents are idle with no pending actionable work, disposable records and
  queues are clean, and the exact before/after evidence is durable.

Installed acceptance must not use live Mayor, Witness, Deacon, dog, polecat, or
campaign sessions as fixtures.

## Implementation constraints

After Phase 0 approval, the implementation plan must:

- patch the smallest shared convergence point that covers all verified
  accidental-duplicate callers;
- reuse verified existing primitives before adding state or processes;
- preserve unknown or unproven redundancy;
- implement root-cause tests before changing behavior;
- keep durable records append-only except through existing explicit lifecycle
  actions;
- avoid broad queue cleanup and cross-thread deletion; and
- update `docs/architecture.md` and maintained mail or escalation protocol
  documentation when their described contract changes.

Ordinary test, build, lint, race, integration, and acceptance failures are
debugging work. Preserve the first failure, diagnose the root cause, make the
smallest safe in-scope fix, and rerun until green. Never bypass a failed check
or commit a known regression.

## Rollback

Prefer a source-only repair with no schema migration. Rollback is a source
revert plus restoration of the previous installed binary. Durable mail,
verdict, and escalation history must remain intact.

If Phase 0 proves a persistent-format change unavoidable, the plan must require
backward-compatible reads, optional fields ignored by older binaries, and a
tested rollback that preserves queued actionable work. No destructive data
migration is allowed.

## Execution boundary

This specification defines requirements and authorizes no investigation or
implementation by itself. The current approval authorizes writing and
committing this specification only.

When the user later directs autonomous execution of an approved implementation
plan, that direction is standing authority for the non-mutating Phase 0
investigation, automatic review retries, evidence-backed source repair,
disposable installed acceptance, and local meaningful commits. After every
approved gate or completed phase, refresh authoritative Beads state and begin
the next unblocked dependency without another prompt.

It never authorizes live queue cleanup, mutation of live mail or escalations,
broad session lifecycle commands, deployment, or remote push.
