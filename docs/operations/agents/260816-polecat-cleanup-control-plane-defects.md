# Lifecycle cleanup control-plane defects

Status: repair candidate implemented; independent exact-SHA review pending

This note records three control-plane defects observed during narrow agent
recovery on 2026-08-16 and 2026-08-17. They affect whether routine cleanup can
preserve exact custody, publication policy, and truthful work/runtime state.
The source candidate implements the repairs described below, but an installed
binary that predates its acceptance still requires the operator workarounds.

## 1. Stale identity state can deadlock safe recovery

### Symptom

A stopped polecat can have all live recovery predicates in a safe state while
its durable agent bead still reports stale metadata:

- the session is stopped;
- the hook is empty or its work is terminal;
- the worktree is clean and has no stash;
- the branch has no unpreserved patch or unpushed commit; and
- the branch tip is already contained in the authoritative branch.

Despite those facts, `gt polecat check-recovery --reconcile-cleanup` can leave
the polecat at `NEEDS_RECOVERY` when the agent bead contains both
`agent_state=stuck` and a dirty `cleanup_status`, such as `has_unpushed`.
`gt polecat nuke --dry-run` then refuses the cleanup, as it should when the
precondition has not become `SAFE_TO_NUKE`.

### Cause

The recovery evaluator can ignore a stale dirty `cleanup_status` after it has
independently proved terminal work, a safe hook, no pending merge request, and
safe Git state. The later persistence step is stricter:
`cleanupStatusReconcileCandidate` in `internal/cmd/polecat.go` requires both the
derived polecat state and the stored agent-bead state to be `idle` before it
will rewrite the stale cleanup status.

That makes the repair path depend on the stale field it needs to recover from.
The codebase has an internal `TransitionPolecatToIdle` helper, but the observed
ordinary recovery workflow exposed no generation-safe command that could
correct this durable state without bypassing custody checks.

### Consequence

Safe cleanup becomes a deadlock: force cleanup would discard the safety gate,
while ordinary cleanup cannot make the metadata transition required to pass
the gate. The correct result is leaked but preserved worktrees, branches, and
identity records until a narrow repair exists.

### Required operator behavior

- Preserve the polecat when `check-recovery` remains `NEEDS_RECOVERY`, even if
  independent Git inspection appears clean.
- Do not use `--force`, edit Dolt directly, or restart broad Gas Town services
  to make the record disappear.
- Record exact live custody evidence and leave any unique rejected object under
  an explicit recovery ref.
- Escalate the stale identity record for a generation-bound metadata repair.

### Repair acceptance criteria

- Recovery can atomically reconcile stale `agent_state` and `cleanup_status`
  only after rechecking session generation, hook ownership, work terminality,
  active merge-request state, worktree/stash state, and branch preservation.
- A superseding polecat generation cannot be modified through a stale name.
- Failure between validation and persistence leaves the record conservative
  and retryable.
- Tests reproduce `agent_state=stuck` plus stale `cleanup_status` with safe live
  predicates, as well as generation replacement during reconciliation.

### Candidate repair

`check-recovery --reconcile-cleanup` now rechecks polecat identity, tmux
absence, every distinct work reference, active merge request, worktree/stash
state, branch identity, and patch preservation while the per-agent lock still
protects the update. The guarded snapshot includes the structured hook/state
columns as well as description fields, and changes `agent_state` plus
`cleanup_status` together. Any lookup, preservation, revalidation, or
compare-and-set failure returns stable `NEEDS_RECOVERY` and writes nothing.

## 2. `gt polecat nuke` can push implicitly

### Symptom

After a polecat passed `SAFE_TO_NUKE`, the dry run previewed local retirement.
The actual `gt polecat nuke` invocation then created or updated the polecat's
remote branch before deleting its local worktree, branch, and identity.

### Cause

`nukePolecatFullWithOptions` in `internal/cmd/polecat.go` contains a
"best-effort push before nuke" step. It pushes the branch to `origin` and
continues even if the push fails. The current dry-run output and cleanup command
reference do not expose that remote mutation as part of the operation.

### Consequence

`nuke` crosses a publication boundary. In the observed case, the pushed object
was already contained in the authoritative branch, so main did not move and no
unique content was published. In another case, the same behavior could publish
local-only or unreviewed commits despite a no-push campaign policy. It can also
leave an unwanted remote branch that requires separate authorization to delete.

### Required operator behavior

- Treat `gt polecat nuke` as remote-mutating until this defect is repaired.
- Do not run it in a local-only or no-push campaign unless an external guard
  provably rejects the push.
- Compare remote refs before and after any authorized cleanup.
- Do not delete an unexpectedly created remote ref without separate authority.

### Repair acceptance criteria

- Routine cleanup never pushes implicitly.
- Any future publish option is explicit, separately authorized, and blocked by
  repository or campaign no-push policy before network activity begins.
- Dry-run output includes every prospective local and remote mutation.
- Tests prove that ordinary `nuke` cannot create or update a remote ref and that
  push rejection does not weaken local custody checks.

### Candidate repair

Ordinary `gt polecat nuke` no longer calls either the direct push helper or the
manager's historical push-before-remove path. Preservation is proved against
the polecat's named branch rather than whichever branch happens to be checked
out. Dynamic bare-origin tests reject any attempted receive and prove both that
the remote ref set is unchanged and that local custody gates still apply.
The same lifecycle lock now spans a second full safety proof, exact-session
teardown, post-stop Git/worktree/branch revalidation, full lifecycle-snapshot
compare-and-set retirement, worktree removal, and local branch deletion. The
snapshot covers every reset-for-reuse field, and stale `gt done` writers cannot
cross an incarnation boundary. Registered-worktree removal executes while the
agent snapshot remains locked; Git refusal preserves bead, worktree, and branch,
while recursive deletion is limited to positively identified standalone clones.
Refusal-only active-MR and shell preflights finish before exact-session teardown
or molecule cleanup. Missing polecat metadata, a changed work reference,
same-incarnation agent-field drift, or post-proof Git work refuses ordinary
nuke without deleting the bead, worktree, or branch. Blocked dry-run output
reports blockers without advertising actions that the real command would skip.

## 3. Dog closeout can target a reusable session name

### Symptom

`gt dog done` and `gt dog done <name>` could fail before reaching their dog
handler with `gt done is for polecats only (BD_ACTOR=dog)`. When closeout did
run, it cleared work first and scheduled a delayed kill of `hq-dog-<name>`.
That name could identify a replacement generation by the time the delayed kill
executed. Status separately probed the reusable name and did not report whether
the running session was the generation that owned the durable dog work.

### Cause

The root persistent pre-run guard classified a command by the leaf name
`done`, so nested `gt dog done` inherited the top-level polecat publication
guard. Dog state also persisted work and start time but not an exact tmux
generation. Closeout therefore had no durable tuple with which to compare the
tmux session ID, random nonce, tmux-server process identity, and optional
containment custody before mutation or teardown.

### Consequence

The routing collision makes ordinary dog completion unusable. The missing
generation record creates a more serious custody ambiguity: a stale completion
can mark newer work idle or kill a replacement session, while a failed delayed
kill can leave durable work state and runtime state disagreeing.

### Required operator behavior

- On an installed binary that still rejects dog actors, do not reinterpret the
  error as successful closeout. Preserve the live session and escalate for a
  narrow dog-specific recovery.
- Do not kill `hq-dog-<name>` from its name alone when a replacement may have
  started.
- Treat a live legacy dog with no persisted generation as unknown custody. A
  proven-absent legacy session may be reconciled idempotently.

### Candidate repair

- The root guard recognizes only top-level `gt done`; nested `gt dog done`
  reaches the dog lifecycle handler.
- Dog state stores an optional, JSON-compatible `session_generation` record.
  Session start captures and persists the exact tmux generation; a persistence
  failure tears down only that captured generation.
- Closeout compare-and-sets the expected work, start time, and session
  generation before calling exact-generation teardown, then compare-clears the
  generation record. Substitution and unknown tmux state fail closed.
- Human and JSON status report work state separately from `running`, `absent`,
  `stale`, or `unknown` session state. Existing JSON consumers retain the old
  dog fields while the new diagnostics are additive. Tmux transport failures
  return an error before either renderer writes a successful-looking status.
- Real Cobra subprocess tests cover both dog completion entry points and use
  dedicated tmux sockets to prove the installed routing shape.

## Relationship between the defects

These defects pull in different unsafe directions. The stale identity deadlock
prevents a provably clean polecat from retiring, implicit push makes a
successful retirement broader than the operator authorized, and name-based dog
closeout can mutate the wrong generation. A complete repair must keep recovery
conservative, keep local cleanup separate from publication, and bind runtime
teardown to the exact durable owner.
