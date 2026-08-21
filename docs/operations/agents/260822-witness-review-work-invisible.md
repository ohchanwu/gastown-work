# Direct actionable mail can bypass the Beads work lifecycle

Status: open

Severity: P1 when the review gates a production or security-sensitive change

Observed: 2026-08-22

## Summary

A durable exact-commit review request reached a running Witness, received a
delivery acknowledgement, and was later marked read. For more than an hour,
however, Gas Town exposed no durable state showing whether the review was
queued, claimed, active, abandoned, or complete. The request thread had no
reply, and `gt status --json` reported `has_work=false` because that field only
describes the agent's primary hook work.

Gas Town already stores mail in Beads and already has relevant work primitives.
Ordinary issues use `open`, `hooked`, `in_progress`, `blocked`, and `closed`;
mail queues add `claimed-by` and `claimed-at` ownership labels; and self-handoff
mail is explicitly auto-hooked. The defect is not the absence of a lifecycle
engine. It is that direct actionable mail does not enter any of those existing
work-ownership paths.

A direct `gt nudge` produced an acknowledgement 12 seconds later. The Witness
reported that the review was already active, full CI and custody checks were
complete, and adversarial probes were in progress. This means the incident does
not prove that the Witness was idle. It proves that a mail-triggered review can
become operationally invisible to its requester and to status tooling.

The ambiguity caused repeated investigation and acknowledgement traffic while
a production deployment remained deliberately stopped pending the verdict.

## Impact

- A requester cannot distinguish an active review from an ignored request.
- `read=true` can be mistaken for completed handling even though it records
  only that the message was viewed.
- `has_work=false` can be mistaken for agent idleness even though it records
  only the absence of pinned work.
- Operators may send duplicate mail, create redundant durable beads, or nudge
  an agent that is already working.
- A security or production gate can remain blocked without an observable owner
  or progress state.
- Automated supervisors cannot reliably detect a genuinely abandoned review
  without inspecting the agent's terminal output.

No code, production, credential, or provider state was lost or mutated during
this incident. The failure was coordination and observability, not review
correctness.

## Incident timeline

Times below are Asia/Seoul unless marked UTC.

1. At 2026-08-21 20:23:36 UTC (2026-08-22 05:23:36 local), Mayor sent exact
   review request `hq-wisp-fmbpf5` to `jobscraper/witness`.
2. Delivery was acknowledged by the recipient at 20:23:40 UTC, four seconds
   after storage.
3. A later inspection found the request marked read, with no reply in its
   thread. The Witness session was running. `gt status --json` reported
   `has_work=false`, eight unread messages, and an older message as the first
   unread subject.
4. At 06:37:37 local, Mayor sent a non-durable nudge asking the Witness to
   confirm whether the exact review was active.
5. At 06:37:49 local, 12 seconds later, Witness replied by nudge that the review
   was active, custody and full CI gates were complete, and adversarial
   PostgreSQL and toolchain probes were in progress.

The durable request therefore spent about 74 minutes without a thread reply or
other requester-visible review state. The available evidence cannot determine
how much of that interval contained active review work.

## Expected behavior

After a running Witness receives a durable review request, Gas Town should make
its state observable using existing Beads primitives:

- `open`: actionable work is available but unclaimed;
- an atomic claim: a specific Witness generation owns the request;
- `in_progress`: review work has begun;
- `blocked`: review cannot continue, with a durable reason;
- `closed`: a durable exact-object verdict completed the work.

The requester should not need terminal capture or an out-of-band nudge to
distinguish these states. Reading a message should remain separate from claiming
its work. A coordinator must also be able to claim mail work without displacing
an unrelated patrol bead already occupying its primary hook.

## Actual behavior

Gas Town exposed three independent facts that did not compose into a review
lifecycle:

- The mail record showed `delivery_state=acked` and `read=true`.
- The thread contained no reply.
- Agent status showed a running session with `has_work=false`.

None of those facts answered whether the exact review was active. The first
positive progress signal arrived only after a direct nudge.

## Reproduction

This reproduces the observable gap without requiring a production system:

1. Start a Witness session with no pinned hook work.
2. Send it a durable review request:

   ```bash
   gt mail send <rig>/witness \
     -s "REREVIEW REQUEST exact <sha>" \
     -m "Review exact <sha> and return a durable verdict." \
     --type task --permanent
   ```

   The incident used the command defaults instead, so its Beads record was an
   ephemeral `msg-type:notification`. The explicit flags above demonstrate that
   even correctly classifying and durably storing a task does not currently
   claim it or attach it to work state.
3. Have the Witness view the message and begin work without replying, claiming
   the queue-style labels, or attaching a linked work bead.
4. From the requester, inspect the recipient inbox, thread, and status:

   ```bash
   gt mail inbox <rig>/witness --all --json
   gt mail thread <message-id>
   gt status --json
   ```

5. Observe that the message can be delivered and read, the thread can remain
   empty, and `has_work` can remain false while the Witness is actively
   reviewing.
6. Send a nudge and observe that the Witness can immediately report progress
   that was not represented in durable status:

   ```bash
   gt nudge <rig>/witness \
     "Confirm whether review <message-id> is queued, active, or blocked."
   ```

## Technical findings

### Mail is already Beads-native

`internal/mail/router.go` stores direct mail as a `gt:message` Beads record
assigned to the recipient. The observed request was therefore already a Beads
object. Its relevant fields were `status=open`, `assignee=jobscraper/witness`,
`ephemeral=true`, `msg-type:notification`, `delivery:acked`, and `read`.

This is a storage bridge, not a work-ownership bridge. Direct send does not set
the message to `hooked` or `in_progress`, add queue claim labels, or update the
recipient agent's `HookBead`.

### Existing work bridges are specialized and disconnected

Gas Town has two narrower mail-to-work paths:

- `internal/cmd/handoff.go` creates permanent self-handoff mail and explicitly
  changes that message to `status=hooked` for the successor session.
- `internal/cmd/mail_queue.go` lets eligible workers claim queue-addressed mail
  by adding `claimed-by` and `claimed-at` labels, and release it by removing
  those labels.

Neither path is invoked for ordinary direct mail to an agent. Queue claim also
does not update the message to `in_progress`, expose blocked/completed work, or
populate the claimant's `HookBead`. Conversely, auto-hooking every direct
message would be incorrect because agents have one primary hook and coordinator
roles may receive several actionable messages while patrolling.

### Mail type does not imply work ownership

`gt mail send` defaults to `--type notification` and ephemeral storage. The
`--type task` option records intent and controls reply-reminder policy, but it
does not promote or claim the Beads record. The observed request therefore
exposed an additional semantic mismatch: Beads reported `issue_type=task` while
mail metadata reported `msg-type:notification`.

### `read` is not a work acknowledgement

`internal/mail/mailbox.go` implements `MarkReadOnly` as a read label that leaves
the message in the inbox. `internal/cmd/mail_inbox.go` marks a message read when
it is viewed. Delivery acknowledgement is a separate operation. These are
transport and presentation states, not evidence that the requested work was
claimed or completed.

### `has_work` means primary hook work

`AgentRuntime.HasWork` in `internal/cmd/status.go` is documented as "Has pinned
work?" and is populated from the agent bead's scalar `HookBead`. A
mail-triggered review performed outside that primary hook can therefore be
active while `has_work=false` remains correct according to the current schema.

### Mail threads do not carry a review lifecycle

The request had a thread ID, but no protocol required the Witness to write a
claim or progress reply before beginning the review. The thread could therefore
remain empty until the final verdict, leaving no durable owner or intermediate
state.

### Delivery receipts prove transport, not execution

The four-second delivery acknowledgement proved that the durable message reached
the recipient mailbox. It did not prove that an agent turn claimed the work, nor
did it identify the Witness generation responsible for the review.

## Root-cause assessment

The confirmed root cause is a composition gap among existing state models. Mail
storage, delivery, reading, queue claims, handoff hooks, primary agent work, and
review execution all have Beads-backed representations, but the direct-agent
send path has no durable transition into work ownership.

The incident did not exercise a generic mail-to-work transition that then
failed. No such transition is called for direct mail. The request followed the
implemented default path: an ephemeral notification bead was stored, delivered,
and read while remaining `open`. Using `--type task` would have improved intent
classification but would still not have claimed or started Beads work.

This fragmentation reflects different original purposes: hooks represent one
primary serialized assignment; direct mail represents communication and
protocol events; mail queues distribute work across eligible workers; and
handoff mail is a special successor-session case. The paths were never unified
for long-running direct requests handled alongside coordinator patrol work.

It is not confirmed that the Witness scheduler failed or that the Witness was
idle. The fast post-nudge response stated that substantial review work was
already complete. Treating the event as a proven scheduling failure would
overstate the evidence and could lead to the wrong repair.

Two behaviors still require focused tracing:

1. whether a mail notification always causes an agent turn when the target is
   running but already processing other queued notifications; and
2. whether any inbox or startup path marks a request read before the agent has
   durably claimed its work.

## Immediate workaround

Until review state is explicit:

- Send exact-object requests with `--type task --permanent` so their intent is
  explicit and their evidence is not subject to wisp cleanup.
- Require the Witness to send a prompt, durable thread reply such as
  `CLAIMED`, `BLOCKED`, or `DECLINED` for exact-object review requests.
- If no acknowledgement arrives within an operator-defined interval, use one
  `gt nudge` referencing the original message ID. Do not send duplicate durable
  mail unless the original record is missing or corrupt.
- Interpret `has_work=false` only as "no pinned hook work," not as proof of an
  idle agent.
- Keep the gated operation fail-closed until the exact durable verdict exists.

## Proposed repair

Compose the existing Beads primitives instead of adding another lifecycle:

1. Treat permanent `msg-type:task` direct mail as actionable work while leaving
   notifications, replies, and protocol events as ordinary mail.
2. Atomically claim actionable mail using the existing queue-ownership model,
   extended with the exact agent/session generation needed for recovery.
3. Represent execution with the existing issue states: `open`, `in_progress`,
   `blocked`, and `closed`. Either promote the message itself or create one
   linked work bead when the mail record must remain immutable.
4. Reserve `hooked` and `HookBead` for primary hook work. Track claimed mail as
   secondary work so a Witness review cannot displace its patrol hook.
5. Require exact-object verdicts to reply on the original thread and close the
   associated work exactly once.
6. Expose primary hook work and claimed secondary mail work separately in
   `gt status`.
7. Re-notify or escalate an acknowledged-but-unclaimed task after a bounded
   interval, and recover a stale claim only after proving its stored generation
   is no longer live.

This reuses mail storage, queue claims, Beads issue states, thread identity, and
generation custody while preserving the distinction between communication and
work.

## Acceptance criteria

- A permanent `msg-type:task` direct request remains `open` and visibly
  unclaimed until a recipient claims it; reading it does not claim it.
- Starting the review atomically records the owning Witness generation and uses
  the existing `in_progress` status on the message or its linked work bead.
- Existing queue-addressed tasks and direct-agent tasks use the same claim
  ownership semantics.
- Claiming secondary mail work does not replace the agent's primary `HookBead`.
- `gt status --json` reports primary hook work and claimed secondary mail work
  separately.
- The requester can identify the active message ID, thread ID, owner, state, and
  last transition without reading terminal output.
- A final verdict replies on the originating thread and transitions the work to
  `closed` exactly once.
- Duplicate delivery or repeated nudges cannot create concurrent reviews for the
  same exact object and request.
- A dead or replaced Witness generation leaves a recoverable claim that another
  generation can safely adopt.
- Tests cover notification-without-work, task read-without-claim,
  claim-without-hook displacement, queue/direct claim parity, duplicate
  delivery, generation replacement, blocked review, and exact-once completion.
- An end-to-end test proves that a requester sees `in_progress` while a Witness
  performs a review with `has_work=false`.

## Relevant components

- `internal/mail/mailbox.go`: read labels and delivery acknowledgements
- `internal/mail/router.go`: durable mail notification and nudge delivery
- `internal/cmd/mail_queue.go`: queue claim and release ownership labels
- `internal/cmd/handoff.go`: specialized self-mail auto-hook path
- `internal/cmd/mail_inbox.go`: inbox viewing and read transitions
- `internal/cmd/status.go`: agent runtime and pinned-work reporting
- Witness startup/patrol instructions: review claim and reply protocol

## Evidence boundary

The report intentionally records only durable message metadata, public source
paths, status fields, and lifecycle events. It excludes credentials, production
resource identifiers, private endpoints, and terminal contents unrelated to the
coordination defect.
