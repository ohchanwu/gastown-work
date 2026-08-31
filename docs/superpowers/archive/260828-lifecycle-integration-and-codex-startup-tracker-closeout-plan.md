# Lifecycle Integration and Codex Startup Tracker Closeout Plan

**Status:** Completed and archived on 2026-09-01

**Terminal result:** Lifecycle repairs landed locally at exact
`880b3520fac364b24e8d749093945413fd13b17a` after independent candidate and
integration approval. Trackers `gastown-2oo`, `gastown-r6o.2.1`, and
`gastown-r6o` are closed with durable exact-source evidence. No push, install,
or live lifecycle mutation was performed.

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> `superpowers:executing-plans` to execute this plan task-by-task and
> `superpowers:using-git-worktrees` for source isolation. Use
> `superpowers:systematic-debugging` for every unexpected failure and
> `superpowers:verification-before-completion` before every commit or closeout.
> Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Integrate the reviewed `gastown-2oo` lifecycle repair into the current
authoritative source and reconcile the already-integrated Codex deleted-cwd fix
with its child and parent trackers.

**Architecture:** Treat the two lanes as independently gated work over one exact
source baseline. The lifecycle lane reviews the immutable candidate, applies it
to an isolated integration branch, reviews the resulting exact SHA, and then
fast-forwards the authoritative local branch. The startup lane verifies existing
source and test ancestry before narrowly correcting Beads state; it does not
change source unless verification exposes a regression.

**Tech Stack:** Git and worktrees, Go, tmux test fixtures, Dolt-backed Beads,
`gt mail`, Gitleaks, build/vet/format/diff gates.

**Specs:**

- `docs/superpowers/specs/260817-lifecycle-custody-and-truthful-closeout-repair-spec.md`
- `docs/superpowers/specs/260828-codex-startup-repair-tracker-reconciliation-spec.md`

## Global Constraints

- Re-resolve current Git, worktree, Beads, hook, and mail state before every
  mutation; startup observations and short SHAs are never authority.
- Preserve unrelated worktrees, branches, refs, stashes, sessions, hooks,
  trackers, and uncommitted files.
- Commit meaningful local checkpoints. Never push, install, deploy, start or
  restart live agents, run Doctor fixes, restart Dolt, or perform broad cleanup.
- A non-authoring Witness must issue a durable verdict for the exact lifecycle
  candidate and for any different exact integration SHA.
- A failed formal gate blocks only its dependent lane. Refresh authoritative
  state and continue any other unblocked lane automatically.
- Ordinary test, build, lint, race, integration, and conflict failures are
  debugging work: preserve the first failure, diagnose the root cause, make the
  smallest safe in-scope fix, and rerun until green. Never bypass a check or
  commit a known regression.
- Stop only when no safe in-scope path remains, required authority or access is
  unavailable, an explicit specification boundary blocks progress, or a genuine
  product decision is required.

---

### Task 1: Establish exact custody and an isolated integration branch

**Files:**

- Inspect: authoritative Gas Town source checkout and Git common directory
- Inspect: candidate `491e660564ebeaa5b36f3e3163d00b937daa604a`
- Inspect: Beads trackers `gastown-2oo`, `gastown-r6o.2.1`, and `gastown-r6o`
- Create: one ignored Git worktree and local integration branch

- [ ] **Step 1: Capture town and source custody**

  Record the source toplevel, current branch, full HEAD, status, stash list,
  worktree list, replace refs, remotes, and upstream without modifying them.
  Record town-doc status separately so unrelated documentation work cannot enter
  a source commit.

- [ ] **Step 2: Prove the lifecycle candidate**

  Require the exact object to exist as one commit with sole parent
  `e0c5b83e7266873a0824204a385f74dde86f9309`. Record its tree, subject,
  changed paths, and patch ID. Reject a substitute object, unexpected parent, or
  replace-ref-dependent proof.

- [ ] **Step 3: Capture exact tracker state**

  Read all three exact trackers in JSON, including status, assignee, parent,
  children, dependencies, comments, and current attachment fields. Do not infer
  completion from a title, age, or historical mail.

- [ ] **Step 4: Create the isolated source worktree**

  Detect whether the source checkout is already a linked worktree. From the
  current authoritative full SHA, create one ignored worktree on a uniquely
  named local integration branch. Do not reuse or remove a foreign worktree.

- [ ] **Step 5: Establish a qualified baseline**

  Run the focused packages affected by the candidate and the deleted-cwd tmux
  regression before integration. Preserve and diagnose any failure; do not ask
  for approval merely because a baseline check failed.

### Task 2: Verify and reconcile the Codex startup trackers

**Files:**

- Inspect: `internal/tmux/tmux.go`
- Test: `internal/tmux/session_creation_test.go`
- Mutate only after gates pass: `gastown-r6o.2.1` and possibly `gastown-r6o`

- [ ] **Step 1: Prove integrated source ancestry**

  With replacement objects disabled, prove
  `4171108ada48f2ee651abe83bc8a3853a100bf0d` is an ancestor of the exact
  authoritative baseline. Inspect `commandInWorkDir` and the maintained
  `TestNewSessionWithCommandAndEnvContext_EntersWorkDirWhenServerCWDIsUnlinked`
  regression from that checkout.

- [ ] **Step 2: Run focused verification**

  Run the exact regression normally and under the race detector, then run build,
  vet, format, and diff checks for `internal/tmux`. Capture private tmux state
  before and after and require zero fixture residue.

- [ ] **Step 3: Close only the child when truthful**

  Reread `gastown-r6o.2.1`. If ancestry, current behavior, tests, and residue
  gates pass, add a durable comment containing the full authoritative SHA and
  captured commands, then close only the child. If any gate fails, keep it open
  and continue as source-repair work from the exact failing baseline.

- [ ] **Step 4: Reconcile the parent independently**

  Inventory every direct child and archived acceptance record for
  `gastown-r6o`. Close the parent only if all required children and gates are
  terminal. Otherwise leave it open and record the exact remaining child or
  evidence gap. Never close siblings or dependencies as collateral work.

### Task 3: Review and integrate the lifecycle candidate

**Files:**

- Expected candidate paths: `internal/beads`, `internal/polecat`, `internal/cmd`
- Modify only if review or integration exposes an in-scope defect
- Preserve: candidate commit and all foreign Git state

- [ ] **Step 1: Request exact candidate review**

  Send Witness the full candidate and parent SHAs, patch identity, clean custody
  evidence, spec path, and requested review dimensions. Require a permanent
  `APPROVED` or `CHANGES REQUIRED` reply. While the verdict is pending, continue
  Task 2 rather than waiting idly.

- [ ] **Step 2: Repair review findings when necessary**

  For `CHANGES REQUIRED`, reproduce each finding, apply the smallest root-cause
  repair with deterministic tests, run the affected normal/race gates, commit a
  new local candidate, and request a fresh exact-SHA verdict. Do not reinterpret
  a failed verdict as approval.

- [ ] **Step 3: Apply the approved patch to the integration branch**

  Apply the approved candidate without rewriting its evidence object. If it
  applies cleanly, record patch equivalence. If conflicts arise, preserve the
  first conflict, resolve only semantic overlap with newer authoritative source,
  commit the traceable result, and treat that exact SHA as unreviewed.

- [ ] **Step 4: Verify the integrated result**

  From the clean integration checkout, run focused normal and race tests for the
  changed lifecycle behavior, full affected-package tests, build, vet, format,
  diff, repository-wide tests, residue checks, and Gitleaks. Qualify an external
  or baseline failure only after reproducing it unchanged outside the candidate
  diff; otherwise debug it as in-scope work.

- [ ] **Step 5: Obtain exact integration review**

  Send Witness the exact integration SHA, parent, tree, candidate relationship,
  clean status, and captured gates. Repair and rereview every blocking finding
  until a durable exact-SHA approval exists.

### Task 4: Land locally and close the lifecycle tracker truthfully

**Files:**

- Modify: authoritative local source branch by fast-forward only
- Mutate after approval: `gastown-2oo`

- [ ] **Step 1: Revalidate both branch tips**

  Immediately before landing, require the authoritative branch still equals the
  recorded base and the integration branch still equals the approved SHA. If the
  authoritative tip moved, rebuild the integration result on the new exact base
  and repeat all dependent verification and review.

- [ ] **Step 2: Fast-forward the authoritative local branch**

  Land only with a fast-forward from its clean checkout. Do not rebase, force,
  push, install, or mutate live agent state.

- [ ] **Step 3: Reverify the landed SHA**

  Confirm branch ancestry, clean status, patch presence, focused normal/race
  lifecycle tests, startup ancestry, and the deleted-cwd regression on the exact
  landed SHA.

- [ ] **Step 4: Close `gastown-2oo`**

  Reread the tracker, attach a durable closeout with the exact landed SHA,
  candidate and integration verdicts, captured verification, qualified broad
  result, and preserved foreign state, then close only `gastown-2oo`.

### Task 5: Final documentation and custody closeout

**Files:**

- Move after completion: this plan from `docs/superpowers/plans/` to
  `docs/superpowers/archive/`
- Modify: `docs/superpowers/README.md`
- Modify when tracker state becomes terminal: the two active specifications'
  status or archive placement

- [ ] **Step 1: Audit final state**

  Capture exact source HEAD, branch status, tracker states, independent verdict
  records, worktree inventory, stashes, replace refs, hooks, and unread urgent
  mail. Preserve unrelated state.

- [ ] **Step 2: Archive only terminal records**

  Archive this plan and any specification whose requirements are fully complete.
  Keep a specification active when a tracker, formal gate, or source integration
  remains open. Update the index without adopting unrelated documentation edits.

- [ ] **Step 3: Run the documentation commit gate**

  Inspect the complete staged diff, validate index links against the staged
  tree, run whitespace, ambiguity, publication-safety, table-width, and Gitleaks
  checks, then commit only the deliberate documentation paths locally.

- [ ] **Step 4: Report the terminal result**

  Report exact commits, tracker states, approval records, verification commands,
  qualified failures, preserved foreign state, and any remaining explicit
  boundary. Never describe a sent command or queued notification as confirmed
  execution.
