# Actionable mail work lifecycle

Status: implemented locally; awaiting integration

Created: 2026-08-24

Related incident:
[Direct actionable mail can bypass the Beads work lifecycle](../../operations/agents/260822-witness-review-work-invisible.md)

## Summary

Gas Town already stores mail as Beads records, but durable direct task mail does
not enter a durable work lifecycle. A recipient can read and execute a request
while its requester sees no owner, progress state, blocker, or completion.

This design makes the original permanent task message the work record. It
reuses Beads issue states, mail threads, delivery receipts, and exact tmux
session-generation custody. It does not create a linked task bead, replace the
primary hook, or add another scheduler.

The lifecycle is:

```text
open --claim--> in_progress --block--> blocked
  ^                   |                   |
  |                   +--complete--------+
  |                   |
  +------release------+--complete--> closed
```

Reading and delivery remain independent facts. Only a generation-bound claim
starts work, and only an exact-once completion transaction closes it.

## Goals

- Make a direct request observably unclaimed, active, blocked, or complete.
- Bind active ownership to one exact agent session generation.
- Let coordinator roles perform mail work without replacing primary hook work.
- Give direct and queue task mail the same ownership semantics.
- Recover abandoned work without stealing it from a live or uncertain owner.
- Put the final durable result on the originating mail thread exactly once.
- Preserve existing behavior for notifications, replies, protocol messages,
  handoff mail, and ordinary ephemeral mail.

## Non-goals

- Replacing hooks, molecules, convoys, or role-specific patrol loops.
- Treating every message as work.
- Inferring tasks from subject or body text.
- Deduplicating independently created messages by semantic content such as a
  commit SHA. Idempotency is scoped to one message ID.
- Authorizing destructive, deployment, provider, credential, or lifecycle
  operations that the recipient could not already perform.
- Automatically migrating historical task-looking mail into active work.

## Existing constraints

### Mail and work currently overlap without composing

Direct mail is already a `gt:message` Beads issue assigned to its recipient.
The mail model separately records delivery, reading, message type, thread, and
reply identity. None of those fields claims work.

The primary agent hook is scalar. `AgentRuntime.HasWork` reports that hook, so
it must retain its existing meaning for compatibility. Auto-hooking direct task
mail would displace patrol or other primary work and is therefore not allowed.

### The current queue claim is not a sufficient atomic primitive

`internal/cmd/mail_queue.go` currently adds `claimed-by` and `claimed-at`
labels, rereads the record to determine the apparent winner, and removes losing
labels. This is optimistic contention cleanup, not one compare-and-swap state
transition.

Beads provides a real compare-and-swap claim that moves an issue from `open` to
`in_progress` while assigning the actor. However, the current update command
performs claim and metadata updates as separate storage operations. Combining
`--claim` with metadata flags therefore does not atomically bind the exact
session generation.

The implementation must add or adopt a Beads storage operation that claims the
issue and writes the generation receipt in the same transaction. Gas Town must
not emulate this guarantee with a claim followed by a second write.

## Actionable-message contract

A newly sent message participates in this lifecycle only when all of these are
true:

- it has the `gt:message` label;
- it is direct-agent or queue-addressed mail;
- it has `msg-type:task`;
- it is persistent rather than ephemeral; and
- the sending path adds `gt:mail-work`, the version-one enrollment marker.

The marker prevents an upgrade from activating old mail unexpectedly. In the
new sender, `--type task --permanent` adds it automatically. Historical mail
without the marker remains ordinary mail and requires a new explicit request if
the work is still wanted.

Replies, notifications, escalations, delivery receipts, protocol messages,
channel broadcasts, and self-handoff mail are not enrolled. Self-handoff keeps
its specialized auto-hook behavior.

Explicit task classification is a routing instruction, not extra authority.
The recipient must still enforce its role, task scope, custody rules, and all
human-only or destructive-operation gates.

## Record model

The enrolled message remains the only work record. Existing mail labels retain
their current meanings:

- `from:<identity>` identifies the sender;
- `thread:<id>` identifies the conversation;
- `reply-to:<id>` links replies;
- `msg-type:task` records task intent;
- `queue:<name>` retains queue routing when applicable; and
- `read` records presentation state only.

`claimed-by` and `claimed-at` remain compatibility summaries in mail JSON and
labels. The shared transition writes or clears them in the same transaction as
status and `gt_mail` metadata. They are observable ownership hints; the exact
generation receipt remains the authority for mutation and recovery. The mail
model's current queue-only validation for these fields must be generalized to
enrolled direct work.

The Beads issue status becomes the work status for enrolled messages:

- `open`: available and unclaimed;
- `in_progress`: owned by one exact generation;
- `blocked`: owned work cannot currently proceed; and
- `closed`: a durable completion reply exists.

`hooked` is excluded. It remains reserved for self-handoff and primary hook
semantics.

Work metadata is stored under one versioned `gt_mail` object:

```json
{
  "gt_mail": {
    "schema": 1,
    "route": "direct",
    "claim": {
      "actor": "<canonical-agent>",
      "claimed_at": "<rfc3339>",
      "generation": {
        "name": "<session-name>",
        "session_id": "<session-id>",
        "pane_id": "<pane-id>",
        "nonce": "<generation-nonce>",
        "custody": "<custody-marker>",
        "server_pid": 0,
        "server_identity": "<server-identity>",
        "transport": "<transport>"
      }
    },
    "blocked": {
      "reason": "<non-empty-reason>",
      "at": "<rfc3339>"
    },
    "completion": {
      "reply_id": "<message-id>",
      "completed_at": "<rfc3339>"
    }
  }
}
```

Only fields relevant to the current state are present. State transitions also
write ordinary Beads events so cleared metadata does not erase history.

The full generation receipt is used internally for custody checks. Human and
JSON status output expose only a stable generation summary and liveness result,
not private process or transport details.

## Lifecycle operations

All mutations below are compare-and-swap operations. A failed precondition
leaves the message unchanged and returns its current owner and state.

### Create and deliver

The router creates one enrolled persistent message in `open`. Direct mail keeps
the canonical recipient as assignee. Queue mail keeps its current queue-routing
identity and eligibility rules.

Delivery acknowledgement and `read` may change before or after claim. Neither
changes work status.

For enrolled mail, inbox presentation is driven by the `read` label rather than
issue status. Normal inbox queries include `open`, `in_progress`, and `blocked`
enrolled messages; `--all` also includes `closed`. Viewing a task adds `read`
without changing status. A generic mail-close or archive command must reject
nonterminal enrolled work and direct the caller to release or complete it.
Non-enrolled mail keeps its existing presentation and close behavior.

Notification uses the existing recipient wake path. On a new turn, `gt prime`
or equivalent startup context lists enrolled open mail separately and instructs
the agent to claim it before starting the requested work. A failed notification
does not lose the task because the open Beads record remains discoverable.

### Claim

`gt mail claim --id <message-id>` claims an exact direct or queue task. The
existing queue-selection form remains available and calls the same internal
primitive after choosing a candidate.

One storage transaction must:

1. verify the enrollment marker, route, current `open` state, recipient or
   queue eligibility, and caller identity;
2. set status to `in_progress`;
3. assign queue mail to the claimant while leaving direct mail assigned to its
   canonical recipient;
4. record canonical actor, claim time, and the complete current
   `SessionGeneration`; and
5. append the claim event.

The idempotency key is message ID plus exact generation, not canonical actor
alone. A retry from the same generation succeeds without changing the original
claim time. The same canonical agent running in a replacement generation gets
an ownership conflict until recovery proves the stored generation dead.

No requested work begins unless this transaction succeeds.

### Release

`gt mail release <message-id>` conditionally moves `in_progress` back to
`open`. Only the stored exact generation may release its claim.

Direct mail remains assigned to its canonical recipient. Queue mail restores
its queue-routing identity. Active claim metadata is cleared and a release
event preserves the former owner and reason. Release never changes `read`.

### Block and resume

`gt mail block <message-id> -m <reason>` conditionally moves `in_progress` to
`blocked`. The reason is required, and the exact generation remains owner.

`gt mail resume <message-id>` conditionally returns `blocked` to `in_progress`
only for that same generation. The active blocker is cleared from current
metadata but remains in event history.

A terminal negative result such as a review verdict of `CHANGES REQUIRED` is
completion, not a blocked state. `blocked` means the requested work itself
cannot yet reach a verdict.

### Complete

`gt mail reply <message-id> --complete` requires a non-empty reply body and
performs one transaction that:

1. verifies the caller owns the task by exact generation;
2. accepts `in_progress` or `blocked` as the source state;
3. creates one persistent reply on the original thread;
4. records that reply ID in completion metadata;
5. changes the original message to `closed`; and
6. appends the completion event.

The operation is idempotent. Repeating it after success returns the recorded
reply ID and creates no second reply. A `closed` task lacking completion
metadata is inconsistent and must fail closed for recovery; it must not create
a guessed reply.

Reply notification happens after the transaction. If notification fails, the
thread and closed work record remain durable and notification may retry without
repeating completion.

This requires a Beads transaction boundary capable of creating the reply and
updating the source issue together. A two-write reply-then-close or
close-then-reply sequence does not meet the contract.

## Primary and secondary work

The agent bead's `HookBead` remains the sole primary assignment. Mail work is a
secondary collection derived from enrolled messages owned by the agent.
Claiming, blocking, releasing, or completing mail never changes `HookBead`.

`gt status --json` preserves `has_work` exactly and adds:

```json
{
  "has_work": false,
  "has_mail_work": true,
  "has_any_work": true,
  "mail_work": [
    {
      "id": "<message-id>",
      "thread_id": "<thread-id>",
      "subject": "<subject>",
      "route": "direct",
      "status": "in_progress",
      "claimed_by": "<canonical-agent>",
      "claimed_at": "<rfc3339>",
      "generation_id": "<opaque-summary>",
      "generation_live": true,
      "last_transition_at": "<rfc3339>"
    }
  ]
}
```

`has_mail_work` is true for `in_progress` or `blocked` enrolled mail owned by
the target agent. `has_any_work` is `has_work || has_mail_work`. Open,
unclaimed tasks appear in a separate pending-mail count so they are visible but
not falsely reported as active ownership.

Human status output renders primary hook work, active mail work, blocked mail,
and pending task mail as distinct sections.

## Recovery and supervision

Patrol scans enrolled `in_progress` and `blocked` messages. It evaluates the
stored `SessionGeneration` with the same exact-generation liveness machinery
used for safe session cleanup.

Recovery rules are conservative:

- A live matching generation retains ownership regardless of task age.
- An unknown liveness result, missing receipt, malformed receipt, or mismatched
  storage state becomes `NEEDS_RECOVERY`; no automatic mutation occurs.
- Age may trigger the existing task reminder and an escalation, but never
  reassignment.
- An `in_progress` task whose exact generation is proven dead may be
  conditionally reopened only if its status and generation still equal the
  scanned values.
- A `blocked` task whose generation is proven dead remains blocked and is
  escalated to its responsible owner. Recovery does not assume its blocker has
  disappeared.

After a safe reopen, a new generation uses the ordinary claim operation. There
is no direct claim transfer and no force path.

## Retention and cleanup

Reaper and mail cleanup must exclude enrolled messages in `open`,
`in_progress`, or `blocked` from stale-message auto-close and purge selection.

Closed enrolled messages are persistent Beads work history. They follow normal
persistent issue retention and are not treated as ephemeral mail cleanup
candidates. Thread replies produced by completion are persistent as well.

Unknown state, absent enrollment metadata, or contradictory cleanup authority
is preservation authority. Cleanup reports the record instead of repairing or
deleting it.

## Compatibility and rollout

The change is additive for status consumers:

- `has_work` retains its primary-hook definition;
- new JSON fields are additive;
- existing notification, reply, channel, escalation, and handoff mail is
  unchanged; and
- existing queue CLI forms remain, backed by the new shared claim operation.

There is no automatic historical migration. Only messages created with
`gt:mail-work` enter the lifecycle. Operators should resend any still-needed
legacy request using `--type task --permanent` after rollout.

Rollout order:

1. Add Beads transaction support for generation-bound claim and atomic
   reply-plus-close, with concurrency tests.
2. Add the shared Gas Town mail-work model and transition functions.
3. Enroll new permanent task mail and route direct and queue claims through the
   shared primitive.
4. Add CLI transitions, startup discovery, and status fields.
5. Add generation-safe patrol recovery and Reaper protections.
6. Enable enrollment by default only after the end-to-end and crash-boundary
   suites pass.

If the Beads transaction support is unavailable, rollout stops after tests and
does not fall back to sequential writes.

## Failure behavior

- Storage unavailable during create or claim: fail without beginning work.
- Notification unavailable after create: leave the task open and discoverable.
- Claim contention: one generation wins; all others observe its receipt.
- Crash during claim: transaction commits the state and receipt together or
  commits neither.
- Crash during completion: transaction commits the reply and closure together
  or commits neither.
- Completion notification failure: retain the durable reply and retry only the
  notification.
- Active state without a valid generation receipt: preserve and escalate.
- Closed state without a completion reply ID: preserve and escalate.
- Unsupported or contradictory state: preserve and report; never infer safe
  cleanup.

## Test requirements

Unit tests must cover:

- actionable classification and every excluded message class;
- read and delivery changes without claim;
- inbox visibility after claim and block;
- rejection of generic close or archive for nonterminal work;
- direct and queue claim parity;
- claim contention between different agents;
- contention between two generations of the same agent;
- same-generation claim retry;
- claim failure leaving both state and receipt unchanged;
- release route restoration;
- block, same-generation resume, and terminal negative completion;
- completion retry returning the original reply ID;
- notification failure after durable completion;
- missing, malformed, live, dead, and replaced generation receipts;
- recovery compare-and-swap losing a concurrent race;
- Reaper protection for every nonterminal work state; and
- status compatibility for `has_work` and the new mail fields.

Mutation tests or equivalent valid-state probes must prove that moving claim
metadata to a second write and moving reply creation outside the completion
transaction both fail the suite.

The end-to-end test must:

1. start a recipient with unrelated primary hook work;
2. send persistent direct task mail;
3. read it and prove it remains open and unclaimed;
4. claim it and prove the exact generation owns `in_progress` mail while the
   primary `HookBead` is unchanged;
5. expose `has_work=true`, `has_mail_work=true`, and both work records
   separately;
6. complete with a thread reply;
7. retry completion and prove there is still one reply; and
8. replace the recipient generation in a separate case and prove live work is
   never stolen while dead work is recoverable.

A second end-to-end case with no primary hook must prove that active mail work
can coexist with `has_work=false` while `has_mail_work` and `has_any_work` are
true.

## Acceptance criteria

- The original enrolled message is the only task record.
- Reading or acknowledging delivery never claims or completes work.
- One transaction binds `in_progress` to one exact generation.
- Direct and queue task mail use the same transition implementation.
- Mail work never changes the primary `HookBead`.
- Requesters can see task ID, thread, state, owner, claim time, and last
  transition without terminal inspection.
- Completion produces one durable thread reply and closes exactly once.
- A replacement generation cannot inherit a live or uncertain claim.
- A proven-dead `in_progress` owner can be recovered through compare-and-swap.
- Blocked work is preserved until its blocker is explicitly resolved.
- Reaper cannot auto-close or purge nonterminal enrolled work.
- Existing non-actionable mail and existing status consumers remain compatible.
- The required unit, concurrency, mutation, crash-boundary, and end-to-end tests
  pass before enrollment is enabled.

## Rejected alternatives

### Create a linked task bead

This preserves the message as an immutable envelope, but introduces two records
whose ownership, status, thread, completion, and cleanup must remain consistent.
The original message is already a Beads issue, so duplication adds failure
modes without adding required capability.

### Auto-hook direct task mail

The primary hook is scalar. Auto-hooking would displace patrol or other primary
work and serialize coordinator roles around an unrelated transport detail.

### Put the generation in the assignee string

Generation-qualified assignees would make the existing claim atomic, but would
break canonical identity, inbox, status, and queue queries. A narrow atomic
metadata claim is less invasive and preserves the current identity model.

### Use claim followed by metadata

Sequential writes leave a crash window with active work but no exact recovery
authority. Treating that window as recoverable risks stealing work; treating it
as permanent preservation defeats autonomy. The transaction prerequisite is
therefore part of the design, not an optional hardening step.
