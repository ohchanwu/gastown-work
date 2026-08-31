# Codex Startup Repair Tracker Reconciliation

**Status:** Completed and archived on 2026-09-01

**Trackers:** `gastown-r6o.2.1` and parent epic `gastown-r6o`

**Priority:** P0 tracker truthfulness

## Completion

Replacement-disabled ancestry proved the startup repair commit
`4171108ada48f2ee651abe83bc8a3853a100bf0d` is present in exact landed source
`880b3520fac364b24e8d749093945413fd13b17a`. The maintained deleted-working-
directory regression passed normally and under the race detector on the landed
source. Child `gastown-r6o.2.1` and parent `gastown-r6o` closed with durable
exact-source evidence; no source edit, push, install, or live session mutation
was needed for this reconciliation.

## Problem

The Codex startup fix for a deleted tmux server working directory is present in
the authoritative source history, but its child tracker and parent epic remain
open. This is tracker drift, not evidence that the source fix is absent.

The implementation commit is
`4171108ada48f2ee651abe83bc8a3853a100bf0d` (`fix: survive deleted tmux server
cwd`). The maintained regression is
`internal/tmux/session_creation_test.go::TestNewSessionWithCommandAndEnvContext_EntersWorkDirWhenServerCWDIsUnlinked`.

## Required reconciliation

Resolve the current authoritative integration SHA before acting. Verify that:

- `4171108ada48f2ee651abe83bc8a3853a100bf0d` is its ancestor without replace
  refs or substitute objects;
- the current `internal/tmux/tmux.go` still enters the requested working
  directory when the tmux server inherited an unlinked directory;
- the exact maintained regression is still present; and
- the checkout used for verification is clean.

Run the focused regression normally and under the race detector, then run build,
vet, format, and diff checks for the affected tmux package. Use isolated tmux
state and prove that no private server or session remains.

If those gates pass, attach a durable closeout note to `gastown-r6o.2.1` with
the authoritative SHA, ancestry proof, commands, results, and residue check,
then close only that child.

If source, ancestry, behavior, or the regression is missing or failing, do not
close the child. Record the failure and convert it into current source-repair
work from the authoritative base.

After the child is truthful, inspect every direct child of `gastown-r6o` and
the archived startup-repair acceptance record. Close the parent epic only when
all required children and durable acceptance gates are terminal. Otherwise,
leave it open and record the exact remaining child or evidence gap. Do not use
the parent title or age as proof of completion.

## Acceptance criteria

- The focused normal and race regressions pass from the authoritative checkout.
- Build, vet, format, and diff checks pass for the affected package.
- No tmux fixture residue remains.
- The child closeout cites exact captured evidence rather than intended work.
- The parent state matches its complete child inventory and archived acceptance
  evidence.
- No unrelated tracker or source state changes.

## Boundaries

This is a source-verification and tracker-reconciliation task. It does not
authorize source edits, rebase, install, deploy, remote push, broad Beads
cleanup, session restart, or `gt done`. Preview and reread each exact tracker
before its narrow mutation.

Ordinary verification failures are debugging work only when the root cause is
inside this scope. A real source regression must remain open as repair work.
Stop only when no safe in-scope path remains, authority or access is
unavailable, an explicit boundary blocks progress, or a genuine product
decision is required.
