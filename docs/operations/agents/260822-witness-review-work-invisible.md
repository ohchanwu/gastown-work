# Witness review work can become operationally invisible

Status: open

Severity: P1 when the review gates a production or security-sensitive change

Observed: 2026-08-22

## Summary

A durable exact-commit review request reached a running Witness, received a
delivery acknowledgement, and was later marked read. For more than an hour,
however, Gas Town exposed no durable state showing whether the review was
queued, claimed, active, abandoned, or complete. The request thread had no
reply, and `gt status --json` reported `has_work=false` because that field only
describes pinned hook work.

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
one of these states observable:

- `queued`: delivered but not claimed;
- `claimed`: a specific Witness generation owns the request;
- `in_progress`: review work has begun;
- `blocked`: review cannot continue, with a durable reason;
- `complete`: a durable exact-object verdict exists.

The requester should not need terminal capture or an out-of-band nudge to
distinguish these states. Reading a message should remain separate from claiming
its work.

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
     -m "Review exact <sha> and return a durable verdict."
   ```

3. Have the Witness view the message and begin work without replying or
   attaching the message to its hook.
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

### `read` is not a work acknowledgement

`internal/mail/mailbox.go` implements `MarkReadOnly` as a read label that leaves
the message in the inbox. `internal/cmd/mail_inbox.go` marks a message read when
it is viewed. Delivery acknowledgement is a separate operation. These are
transport and presentation states, not evidence that the requested work was
claimed or completed.

### `has_work` means pinned work

`AgentRuntime.HasWork` in `internal/cmd/status.go` is documented as "Has pinned
work?" and is populated from an agent's hook bead or handoff attachment. A
mail-triggered review performed without a hook can therefore be active while
`has_work=false` remains correct according to the current schema.

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

The confirmed root cause is a state-model gap: mail delivery, message reading,
pinned work, and review execution are represented independently, with no durable
transition connecting them.

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

- Require the Witness to send a prompt, durable thread reply such as
  `CLAIMED`, `BLOCKED`, or `DECLINED` for exact-object review requests.
- If no acknowledgement arrives within an operator-defined interval, use one
  `gt nudge` referencing the original message ID. Do not send duplicate durable
  mail unless the original record is missing or corrupt.
- Interpret `has_work=false` only as "no pinned hook work," not as proof of an
  idle agent.
- Keep the gated operation fail-closed until the exact durable verdict exists.

## Proposed repair

Add a small, durable mail-work lifecycle rather than inferring execution from
read state:

1. Allow a recipient to atomically claim a message with its exact agent/session
   generation.
2. Store `queued`, `claimed`, `in_progress`, `blocked`, and `complete` states on
   the message or a linked work record.
3. Require exact-object review verdicts to reply on the original thread and
   close the active claim.
4. Expose active mail work separately from pinned hook work in `gt status`.
5. Re-notify or escalate an acknowledged-but-unclaimed request after a bounded
   interval. Do not duplicate a claim that already has a live generation.
6. Recover claims after session replacement by comparing the stored generation
   with the live session before reassignment.

This preserves the useful distinction between mail and hook work while making
review ownership observable.

## Acceptance criteria

- A delivered review request becomes visibly `queued` without being considered
  claimed merely because it was read.
- Starting the review atomically records the owning Witness generation and
  changes the request to `in_progress`.
- `gt status --json` reports pinned work and active mail work as separate fields.
- The requester can identify the active message ID, thread ID, owner, state, and
  last transition without reading terminal output.
- A final verdict replies on the originating thread and transitions the claim to
  `complete` exactly once.
- Duplicate delivery or repeated nudges cannot create concurrent reviews for the
  same exact object and request.
- A dead or replaced Witness generation leaves a recoverable claim that another
  generation can safely adopt.
- Tests cover read-without-claim, claim-without-hook, duplicate delivery,
  generation replacement, blocked review, and exact-once completion.
- An end-to-end test proves that a requester sees `in_progress` while a Witness
  performs a review with `has_work=false`.

## Relevant components

- `internal/mail/mailbox.go`: read labels and delivery acknowledgements
- `internal/mail/router.go`: durable mail notification and nudge delivery
- `internal/cmd/mail_inbox.go`: inbox viewing and read transitions
- `internal/cmd/status.go`: agent runtime and pinned-work reporting
- Witness startup/patrol instructions: review claim and reply protocol

## Evidence boundary

The report intentionally records only durable message metadata, public source
paths, status fields, and lifecycle events. It excludes credentials, production
resource identifiers, private endpoints, and terminal contents unrelated to the
coordination defect.
