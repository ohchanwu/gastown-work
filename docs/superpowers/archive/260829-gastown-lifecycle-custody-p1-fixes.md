# Gastown lifecycle custody P1 fixes

**Goal:** Close the six binding `hq-ldj9` lifecycle races on top of reviewed commit
`ec383f48380ee6fbf4d2d164fdadc39e4536996a` without changing live state.

**Scope:** Source and tests only. Commit locally; do not push, install, invoke
`gt done`, or perform live cleanup. Preserve the reviewed object at
`refs/recovery/gastown/rictus-gastown-2oo-ec383f48`.

**Approach:** Put each guarantee at the shared mutation boundary: command-level
lifecycle fences for assignment, an exact completion attempt plus process lease,
Git expected-OID CAS, versioned strict journal validation, startup compensation
and verifier cancellation, and bead-lock attachment CAS.
Each cluster follows RED, minimum GREEN, focused normal test, focused race test.

## Task 1: Fence the whole assignment transaction

**Entry points and files:**

- Modify `internal/cmd/sling_dispatch.go`
- Modify `internal/cmd/sling_helpers.go`
- Modify `internal/cmd/hook.go`
- Modify `internal/cmd/handoff.go`
- Test `internal/cmd/sling_test.go`
- Test `internal/cmd/hook_test.go`
- Test `internal/cmd/handoff_test.go`

1. Add RED tests that record the lifecycle-fence interval and fail when any
   assignment mutation occurs outside it:
   - `executeSling`: formula attachment, raw/attachment metadata, hook, mode;
   - `runHook`: existing-hook discovery, completed/forced replacement, new hook;
   - `hookBeadForHandoff`: verification and explicit pin;
   - `sendHandoffMail`: stale-mail cleanup, mail creation, and auto-hook.
2. Run the focused test and capture RED.
3. Move each complete command-level assignment transaction under one
   `withPolecatAssignmentFence` callback. Use unlocked mutation primitives inside
   callbacks; keep cleanup that reacquires the lifecycle lock outside.
4. Prove each path rejects `completing`, `retiring`, and `nuked` generations before
   its first external mutation and cannot cross a concurrent retirement barrier.
5. Run focused normal and race tests for all four assignment entry points.

## Task 2: Make completion ownership exclusive and bar done versus nuke

**Files:**

- Modify `internal/beads/beads_agent.go`
- Modify `internal/beads/beads_agent_test.go`
- Modify `internal/polecat/manager.go`
- Modify `internal/polecat/manager_test.go`

1. Add RED tests proving concurrent completion attempts cannot share ownership,
   nuke blocks while the winner holds its lease, and a replacement attempt can
   safely recover the durable `completing` state after the owning process exits.
2. Persist an opaque completion-attempt receipt. Acquire a per-polecat process
   lease before claiming; return its release callback to `runDone` and hold it for
   the entire command. Same-attempt resume is allowed; a different live attempt
   is excluded by the lease; a post-crash claimant replaces the stale receipt.
3. Acquire the same lease for journaled nuke and hold it through cleanup, so done
   and nuke are mutually exclusive even with `--force`.
4. Reject ordinary agent-description and issue writers in `completing`,
   `retiring`, and `nuked`, except explicit owner/recovery lifecycle methods.
5. Run focused normal and race tests for beads, done, and polecat lifecycle claims.

## Task 3: Delete branches with an expected OID

**Files:**

- Modify `internal/git/git.go`
- Modify `internal/git/git_test.go`
- Modify `internal/cmd/polecat.go`
- Test `internal/cmd/polecat_nuke_scope_test.go`

1. Add RED tests proving deletion refuses a branch whose ref changed after
   preservation verification.
2. Add the minimal `git update-ref -d refs/heads/<branch> <expected-oid>` helper,
   with branch-name and expected-OID validation.
3. Route both retirement deletion paths through the expected-OID helper using the
   journaled `GitHead` receipt.
4. Run focused normal and race tests for Git and nuke branch custody.

## Task 4: Validate the retirement journal and every receipt

**Files:**

- Modify `internal/beads/beads_agent.go`
- Modify `internal/beads/beads_agent_test.go`
- Modify `internal/polecat/manager.go`
- Modify `internal/polecat/manager_test.go`
- Modify `internal/cmd/polecat.go`

1. Add RED table tests for missing/unknown version, missing/unknown phase, skipped
   or regressed phase, non-canonical clone path, invalid branch identity, missing
   Git snapshot, molecule without work, branch without head/targets, and mismatched
   authoritative hook/work receipts.
2. Version the journal and persist the complete fixed receipt: canonical clone,
   validated branch, Git snapshot/head/targets, authoritative `HookBead`,
   `LastSourceIssue`, work bead, and molecule.
3. Validate fresh and resumed receipts before cleanup. Require every phase advance
   to be the exact known successor; represent branchless verify/delete as explicit
   no-op receipt phases rather than skipping them.
4. Require the work bead to match the authoritative hook/last-source relationship
   captured under the agent lock. Decode and validate `GitState` for branchless
   retirement too; never treat an absent receipt as success.
5. Run focused normal and race tests for journal persistence and resume.

## Task 5: Compensate durable startup state on failure

**Files:**

- Modify `internal/polecat/session_manager.go`
- Modify `internal/polecat/session_manager_test.go`
- Modify `internal/cmd/polecat_spawn.go`
- Test `internal/cmd/sling_test.go`

1. Add RED tests showing both durable agent/work-bead state writes are restored
   after a partial `OnStarted` failure, and any asynchronous startup verifier is
   canceled and joined before failed-generation cleanup returns.
2. Add one generation-bound failure callback to `SessionStartOptions`, invoked by
   `StartContext` on failure after the post-start commit begins.
3. Restore both durable states to their exact pre-start values; compensate the
   first write immediately if the second post-start write fails.
4. Give the asynchronous verifier a cancelable context and completion receipt;
   cancel and join it on failed startup so it cannot mutate a replacement session.
5. Run focused normal and race tests for session startup and spawn state.

## Task 6: CAS molecule detach and reject reassignment

**Files:**

- Modify `internal/beads/audit.go`
- Test `internal/beads/audit_test.go`
- Modify `internal/cmd/polecat.go`
- Test `internal/cmd/polecat_nuke_scope_test.go`

1. Add RED tests proving detach rejects a replacement molecule and a newly
   assigned work bead while holding the per-bead lock.
2. Extend detach options with optional expected molecule and assignee receipts;
   validate them after the locked re-read and before audit or mutation.
3. Make nuke detach require the journaled molecule and the expected post-unassign
   empty assignee; fail before deleting bonds or closing the molecule on mismatch.
4. Run focused normal and race tests for detach and nuke cleanup.

## Full source gates and handoff

1. Run `gofmt` on touched Go files and inspect the cumulative diff from
   `ec383f48380ee6fbf4d2d164fdadc39e4536996a`.
2. Run:

   ```sh
   CGO_ENABLED=0 GT_TEST_DOLT_PORT="${GT_TEST_DOLT_PORT:?test-owned port required}" go test ./internal/beads ./internal/git ./internal/polecat ./internal/cmd -count=1
   if [ "$(uname -m)" = arm64 ] && [ -x /opt/homebrew/bin/brew ]; then
     ICU_BREW=/opt/homebrew/bin/brew
   else
     ICU_BREW="$(command -v brew)"
   fi
   ICU_PREFIX="$("${ICU_BREW}" --prefix icu4c@77 2>/dev/null || "${ICU_BREW}" --prefix icu4c)"
   CGO_CPPFLAGS="-I${ICU_PREFIX}/include" CGO_LDFLAGS="-L${ICU_PREFIX}/lib" GT_TEST_DOLT_PORT="${GT_TEST_DOLT_PORT:?test-owned port required}" go test -race ./internal/beads ./internal/git ./internal/polecat ./internal/cmd -count=1
   CGO_ENABLED=0 GT_TEST_DOLT_PORT="${GT_TEST_DOLT_PORT:?test-owned port required}" go test ./... -count=1
   CGO_ENABLED=0 go vet ./...
   CGO_ENABLED=0 go build ./...
   ```

3. Record qualified environmental or unchanged-base failures; do not weaken gates.
4. Update `docs/architecture.md` if the lifecycle contract description needs it.
5. Move this completed plan to `docs/superpowers/archive/`, update the index, run
   publication/secret review, commit locally, append exact-SHA evidence to
   `gastown-2oo`, and nudge Mayor. Do not push or call `gt done`.
