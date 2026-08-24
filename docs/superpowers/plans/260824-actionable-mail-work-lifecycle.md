# Actionable mail work lifecycle implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> `superpowers:subagent-driven-development` (recommended) or
> `superpowers:executing-plans` to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make newly enrolled permanent task mail a generation-owned Beads work
record with atomic claim, recovery, and exactly-once completion, without
changing primary hook semantics.

**Architecture:** Keep the original `gt:message` issue as the sole work record.
Represent the lifecycle with its Beads status plus one versioned `gt_mail`
metadata object. All state changes go through one shared transaction layer
backed by the existing `beads.Storage.RunInTransaction` API; direct and queue
CLI paths only select candidates and supply eligibility. Capture exact tmux
generation custody before entering a transaction, and keep notification outside
the durable transaction.

**Tech stack:** Go, Cobra, Beads v1.0.5 storage transactions, Dolt, tmux exact
session-generation custody, existing Gas Town isolated test launcher.

**Spec:**
[Actionable mail work lifecycle](../specs/260824-actionable-mail-work-lifecycle.md)

**Global constraints:** Never push. Preserve `AgentRuntime.HasWork` and
`HookBead` semantics. Do not enroll historical mail. Do not fall back to
sequential claim/metadata or reply/close writes. Run Dolt-backed tests only via
`scripts/test-isolated.sh` with a non-live port. Commit locally at each passing
checkpoint.

## Implementation note: no Beads patch is required

Beads v1.0.5 already exports `Storage.RunInTransaction` and a `Transaction`
interface containing `CreateIssue`, `GetIssue`, `UpdateIssue`, `CloseIssue`,
`AddLabel`, `RemoveLabel`, and `GetLabels`. That is sufficient for both atomic
generation-bound claim and atomic reply-plus-close. Open Beads PR #4682 adds a
broader compare-and-swap API, but this feature neither needs nor replaces that
contributor work.

## Task 1: Define and validate the mail-work record

**Files:**

- Create: `internal/mail/work.go`
- Create: `internal/mail/work_test.go`
- Modify: `internal/mail/types.go`
- Test: `internal/mail/types_test.go`

- [x] Add table-driven failing tests for enrollment classification: permanent
  direct and queue task mail with `gt:mail-work` qualify; ephemeral tasks,
  channels, notifications, replies, escalations, self-handoffs, and unmarked
  historical tasks do not.
- [x] Run
  `go test ./internal/mail -run 'TestMailWork(Classification|Metadata|Validation)'`
  and confirm the new tests fail because the model does not exist.
- [x] Add the minimal model: label and schema constants, route and state types,
  generation receipt, claim/block/completion records, versioned JSON codec, and
  validation for each allowed state.
- [x] Extend `Message`/`BeadsMessage` conversion with issue status, labels,
  metadata, and mail-work helpers. Permit claim summaries on enrolled direct
  work while retaining queue-only validation for legacy records.
- [x] Re-run the focused tests and `go test ./internal/mail`; both must pass.
- [x] Commit: `feat(mail): model actionable mail work`

## Task 2: Add the atomic transition store

**Files:**

- Create: `internal/mail/work_store.go`
- Create: `internal/mail/work_store_test.go`
- Modify: `internal/mail/store.go`

- [x] Write failing storage tests for direct and queue claim, same-generation
  retry, competing agents, replacement generations of one agent, release route
  restoration, block/resume, and rejection of malformed or contradictory
  records.
- [x] Add transaction fault tests proving a callback error leaves status,
  assignee, labels, and metadata unchanged.
- [x] Run the focused tests through
  `GT_TEST_DOLT_PORT=33428 scripts/test-isolated.sh -run TestMailWorkStore ./internal/mail`
  and confirm the intended failures.
- [x] Implement one `MailWorkStore` over `beads.Storage.RunInTransaction`.
  Claim must verify eligibility and atomically write status, assignee,
  compatibility labels, full generation receipt, and a transition comment.
  Release, block, and resume must compare the exact stored generation before
  changing anything.
- [x] Re-run the focused isolated tests and `go test ./internal/mail`.
- [x] Commit: `feat(mail): add atomic work transitions`

## Task 3: Make completion exactly once

**Files:**

- Modify: `internal/mail/work_store.go`
- Modify: `internal/mail/work_store_test.go`

- [x] Write failing tests that complete `in_progress` and `blocked` work, create
  one persistent reply on the original thread, close the source, and return the
  original reply ID on retry.
- [x] Add rollback probes for failure after reply creation and before source
  closure; neither a reply nor a closure may survive.
- [x] Add fail-closed tests for closed work with absent or malformed completion
  metadata and for a non-owner generation.
- [x] Implement reply creation, completion metadata, source close, compatibility
  ownership summaries, and transition comment in one transaction. Generate retryable
  values outside the callback where needed; notify only after commit.
- [x] Run focused isolated tests and `go test ./internal/mail`.
- [x] Commit: `feat(mail): complete mail work atomically`

## Task 4: Enroll only newly sent permanent task mail

**Files:**

- Modify: `internal/mail/router.go`
- Modify: `internal/mail/router_test.go`
- Modify: `internal/cmd/mail_send_test.go`

- [x] Write failing router tests proving `gt:mail-work` is added only for new
  persistent direct or queue `TypeTask` messages, including the
  `--type task --permanent` CLI path.
- [x] Add exclusions for wisps, groups/lists, channels, announcements,
  self-handoffs, replies, notifications, and escalations.
- [x] Add the label in the shared message-label builder so direct and queue
  sends cannot drift; leave historical records untouched.
- [x] Run `go test ./internal/mail ./internal/cmd -run 'Test.*Mail.*(Work|Task|Permanent)'`.
- [x] Commit: `feat(mail): enroll persistent task messages`

## Task 5: Route claim, release, block, resume, and complete through one CLI path

**Files:**

- Modify: `internal/cmd/mail.go`
- Modify: `internal/cmd/mail_queue.go`
- Modify: `internal/cmd/mail_thread.go`
- Create: `internal/cmd/mail_work_test.go`

- [x] Write failing command tests for `gt mail claim --id`, existing queue
  selection, direct and queue release, required block reasons, same-generation
  resume, and `gt mail reply --complete` with a non-empty body.
- [x] Add exact current-generation capture from `GT_SESSION` and tmux. Refuse to
  mutate when generation capture is unavailable or incomplete.
- [x] Replace the queue label/reread race with candidate selection followed by
  the shared transaction claim. Add `block` and `resume` subcommands and a
  `--complete` reply flag. Keep ordinary reply behavior unchanged.
- [x] Ensure completion notification failures report durable success plus a
  retryable notification warning, never repeat the transaction.
- [x] Run `go test ./internal/cmd -run TestMailWork` and the full
  `go test ./internal/cmd` package. The focused suite passes; the full package
  reaches the unrelated baseline failure
  `TestDogDoneInsideOwnedTmuxSessionFinalizesOutsidePane`, reproduced alone.
- [x] Commit: `feat(mail): expose generation-safe work commands`

## Task 6: Separate reading from work state and protect generic archive

**Files:**

- Modify: `internal/mail/mailbox.go`
- Modify: `internal/mail/store.go`
- Modify: `internal/mail/mailbox_test.go`
- Modify: `internal/mail/store_test.go`
- Modify: `internal/cmd/mail_inbox.go`
- Modify: `internal/cmd/mail_inbox_test.go`
- Modify: `internal/cmd/mail_archive_test.go`

- [x] Write failing tests showing enrolled `open`, `in_progress`, and `blocked`
  mail remains in the normal inbox according to its `read` label, while
  `--all` also includes closed work.
- [x] Write failing tests that generic delete/archive/close rejects nonterminal
  enrolled work and points to release or complete. Keep legacy mail behavior.
- [x] Update both SDK and subprocess query/conversion paths so issue status is
  work state and `read` is presentation state for enrolled mail.
- [x] Run focused mail and command tests.
- [x] Commit: `fix(mail): separate task state from inbox reading`

## Task 7: Add additive status and startup discovery

**Files:**

- Modify: `internal/cmd/status.go`
- Modify: `internal/cmd/status_test.go`
- Modify: `internal/cmd/prime_output.go`
- Modify: `internal/cmd/prime_output_test.go`

- [x] Write failing JSON and human-output tests for active, blocked, and pending
  mail work, including `has_work=false`, `has_mail_work=true`, and
  `has_any_work=true`.
- [x] Preserve every existing `has_work` assertion and add the minimal fields:
  `has_mail_work`, `has_any_work`, `mail_work`, and pending-mail count.
- [x] Add startup output that lists pending enrolled tasks separately and tells
  the agent to claim before acting. Do not auto-hook or auto-claim.
- [x] Run focused status/prime tests, then `go test ./internal/cmd`. Focused
  tests pass; the full package reaches the unchanged baseline failure
  `TestDogDoneInsideOwnedTmuxSessionFinalizesOutsidePane`.
- [x] Commit: `feat(status): report secondary mail work`

## Task 8: Add generation-safe recovery and Reaper protection

**Files:**

- Modify: `internal/mail/work_store.go`
- Modify: `internal/mail/work_store_test.go`
- Modify: `internal/cmd/patrol_scan.go`
- Modify: `internal/cmd/patrol_scan_test.go`
- Modify: `internal/reaper/reaper.go`
- Modify: `internal/reaper/reaper_test.go`
- Modify: `internal/cmd/reaper.go`
- Modify: `internal/cmd/reaper_test.go`

- [x] Write failing tests for live, dead, replaced, missing, and malformed
  generation receipts. Unknown or contradictory evidence must return
  `NEEDS_RECOVERY` without mutation.
- [x] Add a compare-and-swap recovery test where a concurrent transition wins;
  recovery must preserve the newer state.
- [x] Reuse existing tmux exact-generation liveness checks. Reopen only
  `in_progress` work whose scanned status and generation still match and whose
  exact owner is proven dead. Preserve and escalate dead-owner blocked work.
- [x] Add nonterminal `gt:mail-work` exclusions to stale auto-close and purge
  candidate selection, including unknown-state preservation.
- [x] Run focused patrol/Reaper tests and affected package suites. The focused
  tests, isolated Dolt SQL tests, and full mail/Reaper packages pass. The full
  command package has only the unchanged baseline failure
  `TestDogDoneInsideOwnedTmuxSessionFinalizesOutsidePane`.
- [x] Keep the Reaper CLI unchanged because it already routes through the
  protected shared package; no duplicate command-layer guard is needed.
- [x] Commit: `fix(reaper): preserve and recover mail work safely`

## Task 9: Prove the end-to-end contract

**Files:**

- Create: `internal/cmd/mail_work_integration_test.go`

- [x] Add an isolated Dolt/tmux end-to-end test with unrelated primary hook
  work: send, read without claim, claim, status both work records, complete,
  retry completion, and prove `HookBead` never changes.
- [x] Add a no-primary-hook case proving `has_work=false` while active mail
  makes `has_mail_work` and `has_any_work` true.
- [x] Add replacement-generation cases proving live work is not stolen and
  proven-dead work is recoverable.
- [x] Run the end-to-end test through `scripts/test-isolated.sh` on dedicated
  port 33435, then run the focused mail, command, patrol, and Reaper suites.
  The most recent full command-package run remains green except for the
  unchanged baseline dog/tmux finalization failure recorded in Task 8.
- [x] Commit: `test(mail): cover actionable work lifecycle end to end`

## Task 10: Update architecture and perform the release gate

**Files:**

- Modify: `docs/architecture.md`
- Modify: `docs/superpowers/README.md`
- Move after completion:
  `docs/superpowers/plans/260824-actionable-mail-work-lifecycle.md` to
  `docs/superpowers/archive/260824-actionable-mail-work-lifecycle.md`
- Move after completion:
  `docs/superpowers/specs/260824-actionable-mail-work-lifecycle.md` to
  `docs/superpowers/archive/260824-actionable-mail-work-lifecycle-spec.md`

- [ ] Document the mail-work record, transaction boundary, secondary-work
  status fields, and conservative recovery rules in `docs/architecture.md`.
- [ ] Run `gofmt` on touched Go files and inspect the cumulative diff against
  `b9aa0538`.
- [ ] Run `CGO_ENABLED=0 GT_TEST_DOLT_PORT=33429 make test`, `make build`,
  `go vet ./...`, and Gitleaks. Classify environmental or unrelated failures
  with exact evidence; do not silently waive them.
- [ ] Verify there are no `TODO`, `TBD`, placeholders, credentials, private
  topology, or production-specific evidence in tracked changes.
- [ ] Move the completed plan/spec to the tracked archive, update the index,
  inspect the staged documentation diff, rerun Gitleaks, and commit:
  `docs: record actionable mail work architecture`.
- [ ] Confirm the worktree is clean and report local commits and verification.
  Do not push.
