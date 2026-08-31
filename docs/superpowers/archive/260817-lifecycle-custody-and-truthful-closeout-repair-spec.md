# Lifecycle Custody and Truthful Closeout Repair

**Status:** Completed and archived on 2026-09-01

**Tracker:** `gastown-2oo`

**Initial candidate:** `491e660564ebeaa5b36f3e3163d00b937daa604a`

**Initial candidate parent:** `e0c5b83e7266873a0824204a385f74dde86f9309`

## Completion

The repaired lifecycle source landed locally on `main` at exact
`880b3520fac364b24e8d749093945413fd13b17a` with tree
`74279c23bf948ce3843efb880ca26e8e97897898`. Independent Witness approvals are
stored as `hq-wisp-vfhrye` for candidate
`a33a376514133f4a23a5b4f1a4febd760a7731ba` and `hq-wisp-xr1dcy` for the exact
integration commit. Tracker `gastown-2oo` closed with the landed SHA and
verification evidence. No push, install, or live lifecycle mutation was part of
this closeout.

## Purpose

Complete the post-campaign lifecycle repair without losing custody, publishing
work implicitly, or reporting cleanup that did not happen. This specification
now describes the implemented candidate and its remaining review, integration,
and truthful tracker-closeout gates; it no longer presents the repair as
unstarted design work.

## Candidate state

The single candidate commit changes the lifecycle paths in `internal/beads`,
`internal/polecat`, and `internal/cmd`, with matching tests and maintained
architecture and operations documentation.

The candidate implements these behaviors:

- cleanup compares a full authoritative `AgentFields` snapshot and updates the
  description only if that snapshot is unchanged;
- generation or custody changes fail closed as `NEEDS_RECOVERY` and do not
  write;
- final cleanup revalidates live polecat, session, hook, task, merge-request,
  Git, and merge-queue state before mutation;
- ordinary polecat nuke does not publish commits implicitly;
- dry-run output reports only actions the real command would perform;
- dog completion and status propagate tmux and lifecycle errors instead of
  translating them into a false not-running or success result; and
- root command completion classification uses exact command identity rather
  than a broad name match.

Recorded candidate gates include build, focused vet, focused race tests, full
tests for the affected Beads, polecat, and command packages, diff checks, and a
staged secret scan. The repository-wide suite also reproduced three pre-existing
environment or baseline failures involving unavailable container support,
offline Dolt cleanup expectations, and tmux missing-target classification.
Those failures are not silently waived: integration must confirm that this
candidate neither touches nor worsens them.

On 2026-08-28 the observed authoritative integration checkout was
`ade39ca01cd34ca2035c14e31c831f6e7684d137`, and the candidate was not yet its
ancestor. Re-resolve the authoritative target immediately before integration;
the recorded observation is evidence, not a permanent branch assumption.

## Remaining work

### 1. Establish exact custody

Before review or integration, prove the candidate object, its sole parent, tree,
and clean checkout. Reject replace refs, an unexpected parent, a dirty candidate
tree, or a short-SHA substitute.

Resolve and record the current authoritative integration SHA separately. Do not
assume the previously observed target is still current.

### 2. Obtain independent source review

A non-authoring reviewer must inspect the exact candidate and store a durable
`APPROVED` or `CHANGES REQUIRED` verdict. The review must cover lifecycle
atomicity, generation binding, no-publication behavior, truthful dry-run and
closeout reporting, error propagation, and deterministic tests.

An earlier review attempt that timed out is not approval. Any repair commit
created after review requires a new exact-SHA verdict.

### 3. Integrate without rewriting evidence

Preserve the candidate commit as immutable review evidence. Apply its patch to
the current authoritative target using the smallest safe integration method.
If it applies without semantic change, record patch equivalence. If conflicts or
newer source require changes, create a traceable integration commit and submit
that resulting exact SHA for independent review.

Do not discard or overwrite unrelated worktrees, branches, refs, or stashes.
Do not publish the source unless a separate instruction explicitly authorizes a
push.

### 4. Reverify the integrated result

From a clean checkout of the exact integrated SHA, run:

- focused normal and race tests for the changed lifecycle behavior;
- full tests for the affected Beads, polecat, and command packages;
- build, vet, format, and diff checks;
- the configured secret scan; and
- the repository-wide suite, with any failure preserved and attributed before
  deciding whether it is in scope.

Tests must prove the negative paths, including generation drift, incomplete
cleanup, lookup failure, tmux error, dirty or unpublished Git state, and dry-run
truthfulness. Assertions must verify zero forbidden mutations, not only an
error string.

### 5. Close truthfully

Close `gastown-2oo` only after the exact reviewed behavior is present in the
authoritative source and all required gates are durably recorded. The closeout
must name the integrated SHA, reviewer verdict, verification commands, qualified
broad-suite result, and preserved foreign state.

If integration or a formal gate fails, keep the tracker open and resume repair
from the failing exact SHA. A local candidate alone is not terminal completion.

## Acceptance criteria

- Full-snapshot compare-and-set prevents stale cleanup writes.
- Every custody or generation ambiguity fails closed without mutation.
- Cleanup and nuke never publish work implicitly.
- Dry-run and human output describe only captured behavior.
- Dog and tmux failures remain errors through the user-facing command.
- Deterministic tests cover success and each fail-closed branch under the race
  detector.
- A non-authoring reviewer approves the exact integrated SHA.
- The authoritative integration history contains the approved behavior.
- The tracker closeout cites captured evidence and preserves unrelated state.

## Boundaries and autonomous execution

This specification does not authorize install, deploy, live agent cleanup,
broad Doctor repair, Dolt restart, task reassignment, remote push, or removal of
foreign worktrees, refs, branches, sessions, or stashes.

After each gate, refresh authoritative Git and Beads state and continue with the
next unblocked dependency without waiting for another prompt. Ordinary test,
build, lint, race, and integration failures are debugging work: preserve the
first failure, diagnose the root cause, make the smallest safe in-scope fix, and
rerun. Never bypass a check or commit a known regression.

A failed formal review or acceptance gate blocks its dependents. Stop only when
no safe in-scope path remains, required authorization or access is unavailable,
an explicit specification boundary blocks progress, or a genuine product
decision is required.
