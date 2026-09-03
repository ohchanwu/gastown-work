# Gas Town Mail Protocol

> Reference for inter-agent mail communication in Gas Town

## Overview

Gas Town agents coordinate via mail messages routed through the beads system.
Mail uses `type=message` beads with routing handled by `gt mail`.

## Message Types

### POLECAT_DONE

**Route**: Polecat → Witness

**Purpose**: Signal work completion, trigger cleanup flow.

**Subject format**: `POLECAT_DONE <polecat-name>`

**Body format**:
```
Exit: MERGED|ESCALATED|DEFERRED
Issue: <issue-id>
MR: <mr-id>          # if exit=MERGED
Branch: <branch>
```

**Trigger**: `gt done` command generates this automatically.

**Handler**: Witness creates a cleanup wisp for the polecat.

### MERGE_READY

**Route**: Witness → Refinery

**Purpose**: Signal a branch is ready for merge queue processing.

**Subject format**: `MERGE_READY <polecat-name>`

**Body format**:
```
Branch: <branch>
Issue: <issue-id>
Polecat: <polecat-name>
Verified: clean git state, issue closed
```

**Trigger**: Witness sends after verifying polecat work is complete.

**Handler**: Refinery adds to merge queue, processes when ready.

### MERGED

**Route**: Refinery → Witness

**Purpose**: Confirm branch was merged successfully, safe to nuke polecat.

**Subject format**: `MERGED <polecat-name>`

**Body format**:
```
Branch: <branch>
Issue: <issue-id>
Polecat: <polecat-name>
Rig: <rig>
Target: <target-branch>
Merged-At: <timestamp>
Merge-Commit: <sha>
```

**Trigger**: Refinery sends after successful merge to main.

**Handler**: Witness completes cleanup wisp, nukes polecat worktree.

### MERGE_FAILED

**Route**: Refinery → Witness

**Purpose**: Notify that merge attempt failed (tests, build, or other non-conflict error).

**Subject format**: `MERGE_FAILED <polecat-name>`

**Body format**:
```
Branch: <branch>
Issue: <issue-id>
Polecat: <polecat-name>
Rig: <rig>
Target: <target-branch>
Failed-At: <timestamp>
Failure-Type: <tests|build|push|other>
Error: <error-message>
```

**Trigger**: Refinery sends when merge fails for non-conflict reasons.

**Handler**: Witness notifies polecat, assigns work back for rework.

### REWORK_REQUEST

**Route**: Refinery → Witness

**Purpose**: Request polecat to rebase branch due to merge conflicts.

**Subject format**: `REWORK_REQUEST <polecat-name>`

**Body format**:
```
Branch: <branch>
Issue: <issue-id>
Polecat: <polecat-name>
Rig: <rig>
Target: <target-branch>
Requested-At: <timestamp>
Conflict-Files: <file1>, <file2>, ...

Please rebase your changes onto <target-branch>:

  git fetch origin
  git rebase origin/<target-branch>
  # Resolve any conflicts
  git push -f

The Refinery will retry the merge after rebase is complete.
```

**Trigger**: Refinery sends when merge has conflicts with target branch.

**Handler**: Witness notifies polecat with rebase instructions.

### RECOVERED_BEAD

**Route**: Witness → Deacon

**Purpose**: Notify Deacon that a dead polecat's abandoned work has been recovered
and needs re-dispatch.

**Subject format**: `RECOVERED_BEAD <bead-id>`

**Body format**:
```
Recovered abandoned bead from dead polecat.

Bead: <bead-id>
Polecat: <rig>/<polecat-name>
Previous Status: <hooked|in_progress>

The bead has been reset to open with no assignee.
Please re-dispatch to an available polecat.
```

**Trigger**: Witness detects a zombie polecat with work still hooked/in_progress.
The bead is reset to open status and this mail is sent for re-dispatch.

**Handler**: Deacon runs `gt deacon redispatch <bead-id>` which:
- Rate-limits re-dispatches (5-minute cooldown per bead)
- Tracks failure count (after 3 failures, escalates to Mayor)
- Auto-detects target rig from bead prefix
- Slings the bead to an available polecat via `gt sling`

### RECOVERY_NEEDED

**Route**: Witness → Deacon

**Purpose**: Escalate a dirty polecat that has unpushed/uncommitted work needing
manual recovery before cleanup.

**Subject format**: `RECOVERY_NEEDED <rig>/<polecat-name>`

**Body format**:
```
Polecat: <rig>/<polecat-name>
Cleanup Status: <has_uncommitted|has_stash|has_unpushed>
Branch: <branch>
Issue: <issue-id>
Detected: <timestamp>
```

**Trigger**: Witness detects zombie polecat with dirty git state.

**Handler**: Deacon coordinates recovery (push branch, save work) before
authorizing cleanup. Only escalates to Mayor if Deacon cannot resolve.

### HELP

**Route**: Any → escalation target (usually Mayor)

**Purpose**: Request intervention for stuck/blocked work.

**Subject format**: `HELP: <brief-description>`

**Body format**:
```
Agent: <agent-id>
Issue: <issue-id>       # if applicable
Problem: <description>
Tried: <what was attempted>
```

**Trigger**: Agent unable to proceed, needs external help.

**Handler**: Escalation target assesses and intervenes.

### HANDOFF

**Route**: Agent → self (or successor)

**Purpose**: Session continuity across context limits/restarts.

**Subject format**: `🤝 HANDOFF: <brief-context>`

**Body format**:
```
attached_molecule: <molecule-id>   # if work in progress
attached_at: <timestamp>

## Context
<freeform notes for successor>

## Status
<where things stand>

## Next
<what successor should do>
```

**Trigger**: `gt handoff` command, or manual send before session end.

**Handler**: Next session reads handoff, continues from context.

## Format Conventions

### Subject Line

- **Type prefix**: Uppercase, identifies message type
- **Colon separator**: After type for structured info
- **Brief context**: Human-readable summary

Examples:
```
POLECAT_DONE nux
MERGE_READY greenplace/nux
HELP: Polecat stuck on test failures
🤝 HANDOFF: Schema work in progress
```

### Body Structure

- **Key-value pairs**: For structured data (one per line)
- **Blank line**: Separates structured data from freeform content
- **Markdown sections**: For freeform content (##, lists, code blocks)

### Addresses

Format: `<rig>/<role>` or `<rig>/<type>/<name>`

Examples:
```
greenplace/witness       # Witness for greenplace rig
beads/refinery           # Refinery for beads rig
greenplace/polecats/nux  # Specific polecat
mayor/                # Town-level Mayor
deacon/               # Town-level Deacon
```

## Protocol Flows

### Polecat Completion Flow

```
Polecat                    Witness                    Refinery
   │                          │                          │
   │ POLECAT_DONE             │                          │
   │─────────────────────────>│                          │
   │                          │                          │
   │                    (verify clean)                   │
   │                          │                          │
   │                          │ MERGE_READY              │
   │                          │─────────────────────────>│
   │                          │                          │
   │                          │                    (merge attempt)
   │                          │                          │
   │                          │ MERGED (success)         │
   │                          │<─────────────────────────│
   │                          │                          │
   │                    (nuke polecat)                   │
   │                          │                          │
```

### Merge Failure Flow

```
                           Witness                    Refinery
                              │                          │
                              │                    (merge fails)
                              │                          │
                              │ MERGE_FAILED             │
   ┌──────────────────────────│<─────────────────────────│
   │                          │                          │
   │ (failure notification)   │                          │
   │<─────────────────────────│                          │
   │                          │                          │
Polecat (rework needed)
```

### Rebase Required Flow

```
                           Witness                    Refinery
                              │                          │
                              │                    (conflict detected)
                              │                          │
                              │ REWORK_REQUEST           │
   ┌──────────────────────────│<─────────────────────────│
   │                          │                          │
   │ (rebase instructions)    │                          │
   │<─────────────────────────│                          │
   │                          │                          │
Polecat                       │                          │
   │                          │                          │
   │ (rebases, gt done)       │                          │
   │─────────────────────────>│ MERGE_READY              │
   │                          │─────────────────────────>│
   │                          │                    (retry merge)
```

### Abandoned Work Recovery Flow

```
Dead Polecat               Witness                    Deacon
     │                        │                          │
     │ (session dies)         │                          │
     │                        │                          │
     │                  (detects zombie)                 │
     │                  (bead status=hooked)             │
     │                        │                          │
     │                  resetAbandonedBead()             │
     │                  bd update --status=open          │
     │                        │                          │
     │                        │ RECOVERED_BEAD           │
     │                        │─────────────────────────>│
     │                        │                          │
     │                        │                    gt deacon redispatch
     │                        │                    gt sling <bead> <rig>
     │                        │                          │
     │                        │                          ├──> New Polecat
     │                        │                          │    (re-dispatched)
```

### Second-Order Monitoring

```
Witness-1 ──┐
            │ (check agent bead last_activity)
Witness-2 ──┼────────────────> Deacon agent bead
            │
Witness-N ──┘
                                 │
                          (if stale >5min)
                                 │
            ─────────────────────┘
            ALERT to Mayor (mail only on failure)
```

## Communication Hygiene: Mail vs Nudge

Agents overuse mail for routine communication, generating permanent beads and
Dolt commits for messages that should be ephemeral. Every `gt mail send` creates
a wisp bead in Dolt -- a permanent record with its own commit in the git-like
history. This is a critical pollution source.

### The Two Channels

**`gt nudge` (ephemeral, preferred for routine comms)**
- Sends a message directly to an agent's tmux session
- No beads created. No Dolt commits. Zero storage cost.
- Message appears as a `<system-reminder>` in the agent's context
- Suitable for: health checks, status requests, simple instructions, "wake up" signals
- Limitation: if the target session is dead, the nudge is lost

**`gt mail send` (persistent, for structured protocol messages only)**
- Creates a bead (wisp) in the Dolt database
- Generates at least one Dolt commit (the write)
- Persists across session restarts -- survives agent death
- Stores the message before attempting a runtime wake; storage success does not
  by itself prove that the recipient was notified
- Leaves an unverified notification queued for receipt-confirmed retry, while
  the stored message remains available in the recipient's inbox
- Suitable for: HANDOFF context, MERGE_READY/MERGED protocol, escalations, HELP
  requests, anything that MUST survive session death

### The Rule

**Default to `gt nudge`. Only use `gt mail send` when the message MUST survive
the recipient's session death.**

The litmus test: "If the recipient's session dies and restarts, do they need this
message?" If yes -> mail. If no -> nudge.

### Role-Specific Guidance

- **Polecat:** Budget 0-1 mails per session. Mail only HELP or ESCALATE
  (`gt escalate` preferred); nudge everything else.
- **Witness:** Mail only protocol messages such as MERGE_READY, RECOVERED_BEAD,
  RECOVERY_NEEDED, and Mayor escalations. Nudge health checks, status requests,
  and nudge-and-observe work.
- **Refinery:** Mail MERGED, MERGE_FAILED, and REWORK_REQUEST. Nudge routine
  Witness status updates.
- **Deacon:** Mail escalations to Mayor and exceptional self-handoffs. Nudge
  TIMER callbacks, HEALTH_CHECK, and lifecycle pokes.
- **Dogs:** Send no routine mail. Record results in event beads or logs and
  nudge completion to Deacon.
- **Mayor:** Reserve mail for strategic cross-rig coordination and exceptional
  self-handoffs. Nudge routine instructions to Deacon or Witness.

### Why This Matters (The Commit Graph)

Dolt is git under the hood. Every mail creates a Dolt commit. Over a day of
normal operations:
- 4 agents x 15 patrol cycles x 2 mails per cycle = 120 commits just for routine chatter
- These commits live in the git history forever, even after mail rows are deleted
- Rebase can remove them, but prevention is always cheaper than cleanup

### Anti-Patterns

**DOG_DONE as mail** -- Dogs should not mail their completion status. Use
`gt nudge deacon/ "DOG_DONE: plugin-name success"` instead.

**Duplicate escalations** -- Witnesses sending 2+ mails about the same issue
minutes apart. Check inbox before sending: if you already sent about this topic,
don't send again.

**HANDOFF for routine cycles** -- Patrol agents (Witness, Deacon) doing routine
handoffs should use minimal mail. If there's nothing extraordinary, just cycle --
the next session discovers state from beads, not from mail.

**Health check responses via mail** -- When Deacon sends a health check nudge, do
NOT respond with mail. The Deacon tracks health via session status, not mail
responses.

## Durable Wake Convergence

Mail storage and runtime wake delivery are separate operations. Every
router-produced wake is bound to the durable message by a validated
`wake-source:<id>` label plus the same source ID, `mail` source kind, and thread
ID in the queued nudge. `gt mail send --json` exposes the exact stored message
and wake-source IDs; a protocol fallback uses `gt nudge --source-mail` so it
shares that identity instead of creating independent authority.

Consumers claim one queued wake, then re-read its source before injection.
Closed, replied-to, read, acknowledged, or superseded sources terminalize only
that exact claim. Missing, malformed, contradictory, or unreadable source state
is NACKed and remains retryable. Source-free legacy and standalone nudges remain
eligible for compatibility. Queue uniqueness is exact by recipient, kind,
thread, and source.

For ACP agents, rendering a notification in the UI is not acceptance. A busy
agent leaves the claim retryable for the next safe prompt boundary; only a
matching runtime receipt acknowledges and removes it.

## Response-Required Mail

Only `msg-type:task` mail produces a reply reminder. Repeated and concurrent
enqueue attempts converge on one reminder for the same thread and source.
Reply, acknowledgement, close, completion, or supersession clears that exact
reminder without touching unrelated queued work.

## Recurring Escalations

Machine-produced recurring incidents supply a stable fingerprint and explicit
scope. The canonical open escalation stores a material-state hash of severity
and normalized scope, a positive material generation, and the recipients still
pending durable delivery. The initial create or a compare-and-set transition
persists the generation and recipient set before any mail attempt.

Each generation and recipient maps to a deterministic message ID. Retries after
a crash or concurrent observation therefore reuse the existing durable record,
then clear that recipient only after persistence succeeds. Identical samples
update observation evidence without a new wake. Severity or scope transitions
create one new generation and wake; recurrence after close creates a new
incident.

## Typed Exact-SHA Review Lineage

Formal review requests and verdicts are permanent mail with four typed labels:

- `review-lineage:<stable-id>`
- `review-generation:<positive-integer>`
- `review-exact:<lowercase-40-hex-sha>`
- `review-verdict:approved|changes-required` on verdict replies only

A request is a non-reply task. A verdict must be a reply to that exact stored
request and inherits lineage, generation, and SHA only through `--reply-to`.
Subject and body text never grant authority. Generations may use different mail
threads; lineage lookup retains every request and verdict while only the newest
unambiguous verdict is wake-eligible. An older verdict becomes terminal as soon
as a newer request exists. Duplicate, malformed, mismatched, out-of-order, or
changed-between-read authority fails open and remains retryable.

## Implementation

### Sending Mail

```bash
# Basic send
gt mail send <addr> -s "Subject" -m "Body"

# With structured body
gt mail send greenplace/witness -s "MERGE_READY nux" -m "Branch: feature-xyz
Issue: gp-abc
Polecat: nux
Verified: clean"
```

### Receiving Mail

```bash
# Check inbox
gt mail inbox

# Read specific message
gt mail read <msg-id>

# Mark as read
gt mail ack <msg-id>
```

### In Patrol Formulas

Formulas should:
1. Check inbox at start of each cycle
2. Parse subject prefix to route handling
3. Extract structured data from body
4. Take appropriate action
5. Mark mail as read after processing

## Extensibility

New message types follow the pattern:
1. Define subject prefix (TYPE: or TYPE_SUBTYPE)
2. Document body format (key-value pairs + freeform)
3. Specify route (sender → receiver)
4. Implement handlers in relevant patrol formulas

The protocol is intentionally simple - structured enough for parsing,
flexible enough for human debugging.

## Beads-Native Messaging

Beyond direct agent-to-agent mail, the messaging system supports three bead-backed
primitives for group and broadcast communication. All use the `hq-` prefix
(town-level entities that span rigs).

### Groups (`gt:group`)

Named collections of addresses for mail distribution. Sending to a group
delivers to all members.

**Bead ID format:** `hq-group-<name>`

**Member types:** direct addresses (`gastown/crew/max`), wildcard patterns
(`*/witness`, `gastown/crew/*`), special patterns (`@town`, `@crew`,
`@witnesses`), or nested group names.

### Queues (`gt:queue`)

Work queues where each message goes to exactly one claimant (unlike groups).

**Bead ID format:** `hq-q-<name>` (town-level) or `gt-q-<name>` (rig-level)

Fields: `status` (active/paused/closed), `max_concurrency`, `processing_order`
(fifo/priority), plus count fields (available, processing, completed, failed).

### Actionable task lifecycle (`gt:mail-work`)

New permanent direct or queue task mail is durable secondary work:

```bash
gt mail send deacon/ --type task --permanent -s "Repair request" -m "Details"
gt mail claim --id <message-id>
# Then block/resume, release, or complete through one of these paths:
gt mail block <message-id> -m "Waiting for dependency"
gt mail resume <message-id>
gt mail release <message-id>
gt mail reply <message-id> --complete -m "Result"
```

The original message is the sole work record. Work moves between `open`,
`in_progress`, and `blocked`; `closed` is terminal. Reading changes only its
`read` label.
Claims bind to the caller's exact tmux session generation and never replace the
primary hook. Completion creates one persistent thread reply and closes the
source atomically.

Patrol preserves live and uncertain owners. It may reopen only `in_progress`
work whose exact stored generation is proven dead or replaced; blocked work is
escalated without mutation. Reaper never stale-closes any `gt:message`; it
revalidates that protection immediately before mutation. Records labeled
`gt:mail-work` are also excluded from old-mail purge, including completed
history.

### Channels (`gt:channel`)

Pub/sub broadcast streams with configurable message retention.

**Bead ID format:** `hq-channel-<name>`

Fields: `subscribers`, `status` (active/closed), `retention_count`,
`retention_hours`.

### Group and Channel CLI Commands

```bash
# Groups
gt mail group list
gt mail group show <name>
gt mail group create <name> [members...]
gt mail group add <name> <member>
gt mail group remove <name> <member>
gt mail group delete <name>

# Channels
gt mail channel list
gt mail channel show <name>
gt mail channel create <name> [--retain-count=N] [--retain-hours=N]
gt mail channel delete <name>
```

### Sending to Groups, Queues, and Channels

```bash
gt mail send my-group -s "Subject" -m "Body"           # group (expands to members)
gt mail send queue:my-queue -s "Work item" -m "Details" # queue (single claimant)
gt mail send channel:alerts -s "Alert" -m "Content"     # channel (broadcast)
```

### Address Resolution Order

When sending mail, addresses are resolved in this order:

1. **Explicit prefix** -- `group:`, `queue:`, or `channel:` uses that type directly
2. **Contains `/`** -- Treat as agent address or pattern (direct delivery)
3. **Starts with `@`** -- Special pattern (`@town`, `@crew`, etc.) or group
4. **Name lookup** -- Search group -> queue -> channel by name

If a name matches multiple types, the resolver returns an error requiring an
explicit prefix.

### Retention Policy

Channels support count-based (`--retain-count=N`) and time-based
(`--retain-hours=N`) retention. Retention is enforced on-write (after posting)
and on-patrol (Deacon runs `PruneAllChannels()` with a 10% buffer to avoid
thrashing).

## Related Documents

- `docs/agent-as-bead.md` - Agent identity and slots
- `.beads/formulas/mol-witness-patrol.formula.toml` - Witness handling
- `internal/mail/` - Mail routing implementation
- `internal/protocol/` - Protocol handlers for Witness-Refinery communication
