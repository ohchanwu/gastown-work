package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecat"
)

func TestRoutedIssueBeadsUsesTownRoutesForCustomPrefix(t *testing.T) {
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)

	_, gotCurrent, gotRouted := routedIssueBeads(workDir, "bd-source")
	if gotCurrent != currentBeadsDir {
		t.Fatalf("current beads dir = %q, want %q", gotCurrent, currentBeadsDir)
	}
	if gotRouted != ownerBeadsDir {
		t.Fatalf("routed beads dir = %q, want %q", gotRouted, ownerBeadsDir)
	}
}

func TestSourceRouteContextNamesCurrentAndRoutedDB(t *testing.T) {
	context := sourceRouteContext("/town/gastown/.beads", "/town/beads/.beads")
	for _, want := range []string{"current_db=/town/gastown/.beads", "routed_db=/town/beads/.beads"} {
		if !strings.Contains(context, want) {
			t.Fatalf("source route context %q missing %q", context, want)
		}
	}
}

func TestResolveSubmitSourceIssueIgnoresCurrentRigMirror(t *testing.T) {
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
	installSubmitSourceBDStub(t, currentBeadsDir, ownerBeadsDir, false)

	source, err := resolveSubmitSourceIssue(workDir, "bd-source")
	if err != nil {
		t.Fatalf("resolveSubmitSourceIssue: %v", err)
	}
	if source.Issue.Title != "owner source" {
		t.Fatalf("source title = %q, want routed owner source (current-rig mirror must be ignored)", source.Issue.Title)
	}
	if source.CurrentBeadsDir != currentBeadsDir || source.RoutedBeadsDir != ownerBeadsDir {
		t.Fatalf("route = current %q routed %q, want current %q routed %q", source.CurrentBeadsDir, source.RoutedBeadsDir, currentBeadsDir, ownerBeadsDir)
	}
}

func TestResolveSubmitSourceIssueFailureNamesRoutingContext(t *testing.T) {
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
	installSubmitSourceBDStub(t, currentBeadsDir, ownerBeadsDir, true)

	_, err := resolveSubmitSourceIssue(workDir, "bd-source")
	if err == nil {
		t.Fatal("resolveSubmitSourceIssue succeeded, want routed owner lookup failure")
	}
	errText := err.Error()
	for _, want := range []string{"source_issue bd-source could not be resolved", "current_db=" + currentBeadsDir, "routed_db=" + ownerBeadsDir} {
		if !strings.Contains(errText, want) {
			t.Fatalf("error %q missing %q", errText, want)
		}
	}
}

func TestValidateMergeRequestSourceUsesPreResolvedSource(t *testing.T) {
	mr := &beads.Issue{ID: "gt-mr", Description: "source_issue: bd-source\n"}
	if err := validateMergeRequestSource(mr, "bd-source", nil); err == nil || !strings.Contains(err.Error(), "pre-resolved") {
		t.Fatalf("validateMergeRequestSource without source = %v, want pre-resolved error", err)
	}
	if err := validateMergeRequestSource(mr, "bd-source", &beads.Issue{ID: "bd-source", Type: "task"}); err != nil {
		t.Fatalf("validateMergeRequestSource with routed source: %v", err)
	}
}

func TestMqSubmitPathUsesRoutedSourceAndCurrentRigQueueBeads(t *testing.T) {
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
	logPath := installSubmitSourceBDRecorder(t, currentBeadsDir, ownerBeadsDir)

	currentBD := beads.New(workDir)
	source, err := resolveSubmitSourceIssue(workDir, "bd-source")
	if err != nil {
		t.Fatalf("resolveSubmitSourceIssue: %v", err)
	}

	if _, err := currentBD.Create(beads.CreateOptions{
		Title:       "Merge: bd-source",
		Labels:      []string{"gt:merge-request"},
		Priority:    source.Issue.Priority,
		Description: "branch: polecat/refuge/bd-source\ntarget: main\nsource_issue: bd-source\nrig: gastown",
		Ephemeral:   true,
		Rig:         "gastown",
	}); err != nil {
		t.Fatalf("current-rig MR create: %v", err)
	}
	if err := source.BD.AddComment("bd-source", "MR created: gt-mr"); err != nil {
		t.Fatalf("source back-link comment: %v", err)
	}

	log := readSubmitSourceBDLog(t, logPath)
	assertBDLogContains(t, log, ownerBeadsDir, "show bd-source --json")
	assertBDLogContains(t, log, currentBeadsDir, "create --json")
	assertBDLogContains(t, log, ownerBeadsDir, "comments add bd-source")
	assertBDLogNotContains(t, log, currentBeadsDir, "show bd-source --json")
}

func TestDoneNoMRClosePathUsesRoutedSourceBeads(t *testing.T) {
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
	logPath := installSubmitSourceBDRecorder(t, currentBeadsDir, ownerBeadsDir)

	source, err := resolveSubmitSourceIssue(workDir, "bd-source")
	if err != nil {
		t.Fatalf("resolveSubmitSourceIssue: %v", err)
	}
	if skipReason, fatal := doneSourceCloseSkipReason(source.BD, "bd-source", source.Issue); skipReason != "" || fatal {
		t.Fatalf("doneSourceCloseSkipReason = %q, %v; want close allowed", skipReason, fatal)
	}
	if err := source.BD.ForceCloseWithReason("done", "bd-source"); err != nil {
		t.Fatalf("routed source close: %v", err)
	}

	log := readSubmitSourceBDLog(t, logPath)
	assertBDLogContains(t, log, ownerBeadsDir, "show bd-source --json")
	assertBDLogContains(t, log, ownerBeadsDir, "close bd-source")
	assertBDLogNotContains(t, log, currentBeadsDir, "close bd-source")
}

func TestRunMqSubmitWithRoutedIssueIgnoresCurrentRigMirror(t *testing.T) {
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
	setupRoutedSubmitCommandTown(t, workDir)
	branch := setupRoutedSubmitGitRepo(t, workDir, true)
	logPath := installSubmitSourceBDRecorder(t, currentBeadsDir, ownerBeadsDir)
	resetMqSubmitFlagsForTest(t)
	t.Setenv("GT_TEST_NUDGE_LOG", filepath.Join(t.TempDir(), "nudge.log"))
	t.Setenv("GT_RIG", "")
	t.Chdir(workDir)

	mqSubmitBranch = branch
	mqSubmitIssue = "bd-source"
	mqSubmitNoCleanup = true
	if err := runMqSubmit(nil, nil); err != nil {
		t.Fatalf("runMqSubmit: %v", err)
	}

	log := readSubmitSourceBDLog(t, logPath)
	assertBDLogContains(t, log, ownerBeadsDir, "show bd-source --json")
	assertBDLogContains(t, log, currentBeadsDir, "create --json")
	assertBDLogContains(t, log, ownerBeadsDir, "comments add bd-source")
	assertBDLogNotContains(t, log, currentBeadsDir, "show bd-source --json")
}

func TestRunDoneAbortsBeforeSubmissionWhenCompletionOwnerWriteFails(t *testing.T) {
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
	setupRoutedSubmitCommandTown(t, workDir)
	setupRoutedSubmitGitRepo(t, workDir, false)
	logPath := installSubmitSourceBDRecorder(t, currentBeadsDir, ownerBeadsDir)
	resetDoneFlagsForTest(t)
	townRoot := routedSourceTestTownRoot(workDir)
	t.Setenv("GT_TEST_NUDGE_LOG", filepath.Join(t.TempDir(), "nudge.log"))
	t.Setenv("GT_TOWN_ROOT", townRoot)
	t.Setenv("GT_ROOT", townRoot)
	t.Setenv("GT_ROLE", "gastown/polecats/refuge")
	t.Setenv("GT_RIG", "gastown")
	t.Setenv("GT_POLECAT", "refuge")
	t.Setenv("BD_ACTOR", "gastown/polecats/refuge")
	t.Setenv(polecat.EnvAgentIncarnation, "fixture-generation")
	t.Chdir(workDir)

	doneIssue = "bd-source"
	doneCleanupStatus = "unpushed"
	doneSkipVerify = true
	updateAgentStateOnDoneFn = func(cwd, townRoot, exitType, issueID, expectedIncarnation, completionAttempt string) error {
		return nil
	}
	if err := runDone(nil, nil); err == nil || !strings.Contains(err.Error(), "done-intent") {
		t.Fatalf("runDone completion-owner error = %v", err)
	}

	log := readSubmitSourceBDLog(t, logPath)
	assertBDLogNotContains(t, log, ownerBeadsDir, "show bd-source --json")
	assertBDLogNotContains(t, log, currentBeadsDir, "show bd-source --json")
	assertBDLogNotContains(t, log, currentBeadsDir, "create --json")
}

func TestRunDoneLateOwnerFailurePreservesRecoveryMarkersAndSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
	setupRoutedSubmitCommandTown(t, workDir)
	setupRoutedSubmitGitRepo(t, workDir, false)
	_, labelsPath := installLateCompletionFailureBDRecorder(t, currentBeadsDir, ownerBeadsDir)
	resetDoneFlagsForTest(t)
	townRoot := routedSourceTestTownRoot(workDir)
	nudgeLog := filepath.Join(t.TempDir(), "nudge.log")
	t.Setenv("GT_TEST_NUDGE_LOG", nudgeLog)
	t.Setenv("GT_TOWN_ROOT", townRoot)
	t.Setenv("GT_ROOT", townRoot)
	t.Setenv("GT_ROLE", "gastown/polecats/refuge")
	t.Setenv("GT_RIG", "gastown")
	t.Setenv("GT_POLECAT", "refuge")
	t.Setenv("BD_ACTOR", "gastown/polecats/refuge")
	t.Setenv(polecat.EnvAgentIncarnation, "fixture-generation")
	t.Chdir(workDir)
	fakeKiller := &fakeDoneSessionKiller{}
	oldKiller := newDoneSessionKiller
	newDoneSessionKiller = func() doneSessionKiller { return fakeKiller }
	t.Cleanup(func() { newDoneSessionKiller = oldKiller })

	doneIssue = "bd-source"
	doneCleanupStatus = "unpushed"
	doneSkipVerify = true
	err := runDone(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "injected final completion failure") {
		t.Fatalf("runDone late completion error = %v", err)
	}
	if fakeKiller.calls != 0 {
		t.Fatalf("retirement calls after failed final owner write = %d", fakeKiller.calls)
	}
	if data, readErr := os.ReadFile(nudgeLog); readErr == nil {
		for _, successSignal := range []string{"MERGE_READY", "POLECAT_DONE"} {
			if strings.Contains(string(data), successSignal) {
				t.Fatalf("success nudge %q emitted after failed final owner write: %s", successSignal, data)
			}
		}
	}
	labels, readErr := os.ReadFile(labelsPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, marker := range []string{"done-intent:", "done-cp:"} {
		if !strings.Contains(string(labels), marker) {
			t.Fatalf("recovery marker %q lost after failed final owner write: %s", marker, labels)
		}
	}
}

func TestRunDoneLateRoleResolverFailurePreservesRecoveryMarkersAndSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
	setupRoutedSubmitCommandTown(t, workDir)
	setupRoutedSubmitGitRepo(t, workDir, false)
	_, labelsPath := installLateCompletionFailureBDRecorder(t, currentBeadsDir, ownerBeadsDir)
	resetDoneFlagsForTest(t)
	townRoot := routedSourceTestTownRoot(workDir)
	nudgeLog := filepath.Join(t.TempDir(), "nudge.log")
	t.Setenv("GT_TEST_NUDGE_LOG", nudgeLog)
	t.Setenv("GT_TOWN_ROOT", townRoot)
	t.Setenv("GT_ROOT", townRoot)
	t.Setenv("GT_ROLE", "gastown/polecats/refuge")
	t.Setenv("GT_RIG", "gastown")
	t.Setenv("GT_POLECAT", "refuge")
	t.Setenv("BD_ACTOR", "gastown/polecats/refuge")
	t.Setenv(polecat.EnvAgentIncarnation, "fixture-generation")
	t.Chdir(workDir)
	fakeKiller := &fakeDoneSessionKiller{}
	oldKiller := newDoneSessionKiller
	newDoneSessionKiller = func() doneSessionKiller { return fakeKiller }
	oldResolver := resolveDoneAgentRoleFn
	resolveDoneAgentRoleFn = func(string, string) (RoleInfo, error) {
		_ = os.Unsetenv("GT_ROLE")
		_ = os.Unsetenv("GT_RIG")
		return RoleInfo{}, errors.New("injected late role resolution failure")
	}
	t.Cleanup(func() {
		newDoneSessionKiller = oldKiller
		resolveDoneAgentRoleFn = oldResolver
	})

	doneIssue = "bd-source"
	doneCleanupStatus = "unpushed"
	doneSkipVerify = true
	err := runDone(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "injected late role resolution failure") {
		t.Fatalf("runDone late resolver error = %v", err)
	}
	if fakeKiller.calls != 0 {
		t.Fatalf("retirement calls after late resolver failure = %d", fakeKiller.calls)
	}
	if data, readErr := os.ReadFile(nudgeLog); readErr == nil {
		for _, successSignal := range []string{"MERGE_READY", "POLECAT_DONE"} {
			if strings.Contains(string(data), successSignal) {
				t.Fatalf("success nudge %q emitted after late resolver failure: %s", successSignal, data)
			}
		}
	}
	labels, readErr := os.ReadFile(labelsPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, marker := range []string{"done-intent:", "done-cp:"} {
		if !strings.Contains(string(labels), marker) {
			t.Fatalf("recovery marker %q lost after late resolver failure: %s", marker, labels)
		}
	}
}

func TestRunDoneFailedSubmissionPreservesOwnedRecoveryWithoutSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	for _, tt := range []struct {
		name      string
		failSetup func(t *testing.T, workDir string)
	}{
		{
			name: "push failure",
			failSetup: func(t *testing.T, workDir string) {
				runGitForMQSubmitTest(t, workDir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
			},
		},
		{
			name: "MR failure",
			failSetup: func(t *testing.T, _ string) {
				t.Setenv("GT_TEST_FAIL_MR_CREATE", "1")
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
			setupRoutedSubmitCommandTown(t, workDir)
			setupRoutedSubmitGitRepo(t, workDir, false)
			descriptionPath, labelsPath := installLateCompletionFailureBDRecorder(t, currentBeadsDir, ownerBeadsDir)
			resetDoneFlagsForTest(t)
			tt.failSetup(t, workDir)
			townRoot := routedSourceTestTownRoot(workDir)
			nudgeLog := filepath.Join(t.TempDir(), "nudge.log")
			t.Setenv("GT_TEST_NUDGE_LOG", nudgeLog)
			t.Setenv("GT_TOWN_ROOT", townRoot)
			t.Setenv("GT_ROOT", townRoot)
			t.Setenv("GT_ROLE", "gastown/polecats/refuge")
			t.Setenv("GT_RIG", "gastown")
			t.Setenv("GT_POLECAT", "refuge")
			t.Setenv("BD_ACTOR", "gastown/polecats/refuge")
			t.Setenv(polecat.EnvAgentIncarnation, "fixture-generation")
			t.Chdir(workDir)
			fakeKiller := &fakeDoneSessionKiller{}
			oldKiller := newDoneSessionKiller
			newDoneSessionKiller = func() doneSessionKiller { return fakeKiller }
			t.Cleanup(func() { newDoneSessionKiller = oldKiller })

			doneIssue = "bd-source"
			doneCleanupStatus = "unpushed"
			doneSkipVerify = true
			err := runDone(nil, nil)
			if err == nil || !strings.Contains(err.Error(), "completion attempt") {
				t.Fatalf("runDone owned failed submission error = %v", err)
			}
			if fakeKiller.calls != 0 {
				t.Fatalf("retirement calls after failed submission = %d", fakeKiller.calls)
			}
			if data, readErr := os.ReadFile(nudgeLog); readErr == nil {
				for _, successSignal := range []string{"MERGE_READY", "POLECAT_DONE"} {
					if strings.Contains(string(data), successSignal) {
						t.Fatalf("success nudge %q emitted after failed submission: %s", successSignal, data)
					}
				}
			}
			description, readErr := os.ReadFile(descriptionPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, retained := range []string{"agent_state: completing", "completion_attempt:", "hook_bead: bd-source"} {
				if !strings.Contains(string(description), retained) {
					t.Fatalf("owned recovery field %q lost after failed submission: %s", retained, description)
				}
			}
			labels, readErr := os.ReadFile(labelsPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, marker := range []string{"done-intent:", "done-cp:"} {
				if !strings.Contains(string(labels), marker) {
					t.Fatalf("recovery marker %q lost after failed submission: %s", marker, labels)
				}
			}
		})
	}
}

func TestRunDoneRetriesActiveMROwnerWriteOnCheckpointFlow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
	setupRoutedSubmitCommandTown(t, workDir)
	branch := setupRoutedSubmitGitRepo(t, workDir, true)
	currentOID := gitOutput(t, workDir, "rev-parse", "HEAD")
	descriptionPath, labelsPath := installLateCompletionFailureBDRecorder(t, currentBeadsDir, ownerBeadsDir)
	labels := fmt.Sprintf("gt:agent\ndone-cp:pushed:%s@%s:1\ndone-cp:mr-created:gt-mr:2\n", branch, currentOID)
	if err := os.WriteFile(labelsPath, []byte(labels), 0o644); err != nil {
		t.Fatal(err)
	}
	createMarker := filepath.Join(t.TempDir(), "mr-created")
	resetDoneFlagsForTest(t)
	t.Setenv("GT_TEST_FAIL_ACTIVE_MR_ONCE", "1")
	t.Setenv("GT_TEST_ACTIVE_MR_FAIL_FILE", filepath.Join(t.TempDir(), "active-mr-failed"))
	t.Setenv("GT_TEST_MR_COMMIT_SHA", currentOID)
	t.Setenv("GT_TEST_MR_CREATE_FILE", createMarker)
	t.Setenv("GT_TEST_ALLOW_FINAL_COMPLETION", "1")
	t.Setenv("GT_TOWN_ROOT", routedSourceTestTownRoot(workDir))
	t.Setenv("GT_ROOT", routedSourceTestTownRoot(workDir))
	t.Setenv("GT_ROLE", "gastown/polecats/refuge")
	t.Setenv("GT_RIG", "gastown")
	t.Setenv("GT_POLECAT", "refuge")
	t.Setenv("BD_ACTOR", "gastown/polecats/refuge")
	t.Setenv(polecat.EnvAgentIncarnation, "fixture-generation")
	t.Chdir(workDir)
	doneIssue = "bd-source"
	doneCleanupStatus = "unpushed"
	doneSkipVerify = true
	if err := runDone(nil, nil); err == nil || !strings.Contains(err.Error(), "active_mr") {
		t.Fatalf("first run active_mr error = %v", err)
	}
	if err := runDone(nil, nil); err != nil {
		t.Fatalf("checkpoint retry after active_mr recovery: %v", err)
	}
	description, err := os.ReadFile(descriptionPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(description), "active_mr: gt-mr") {
		t.Fatalf("active_mr owner write did not persist after retry: %s", description)
	}
	if _, err := os.Stat(createMarker); !os.IsNotExist(err) {
		t.Fatalf("checkpoint resume unexpectedly created an MR: %v", err)
	}
}

func TestRunDoneDiscardsStaleSameBranchMRCheckpointAtNewHead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
	setupRoutedSubmitCommandTown(t, workDir)
	branch := setupRoutedSubmitGitRepo(t, workDir, true)
	oldOID := gitOutput(t, workDir, "rev-parse", "HEAD")
	writeMQSubmitTestFile(t, workDir, "file.txt", "feature-new-head\n")
	runGitForMQSubmitTest(t, workDir, "commit", "-am", "new head")
	descriptionPath, labelsPath := installLateCompletionFailureBDRecorder(t, currentBeadsDir, ownerBeadsDir)
	labels := fmt.Sprintf("gt:agent\ndone-cp:pushed:%s@%s:1\ndone-cp:mr-created:gt-mr:2\n", branch, oldOID)
	if err := os.WriteFile(labelsPath, []byte(labels), 0o644); err != nil {
		t.Fatal(err)
	}
	createMarker := filepath.Join(t.TempDir(), "mr-created")
	t.Setenv("GT_TEST_MR_COMMIT_SHA", oldOID)
	t.Setenv("GT_TEST_MR_CREATE_FILE", createMarker)
	t.Setenv("GT_TEST_LEGACY_MR", "1")
	t.Setenv("GT_TEST_ALLOW_FINAL_COMPLETION", "1")
	t.Setenv("GT_TOWN_ROOT", routedSourceTestTownRoot(workDir))
	t.Setenv("GT_ROOT", routedSourceTestTownRoot(workDir))
	t.Setenv("GT_ROLE", "gastown/polecats/refuge")
	t.Setenv("GT_RIG", "gastown")
	t.Setenv("GT_POLECAT", "refuge")
	t.Setenv("BD_ACTOR", "gastown/polecats/refuge")
	t.Setenv(polecat.EnvAgentIncarnation, "fixture-generation")
	t.Chdir(workDir)
	resetDoneFlagsForTest(t)
	doneIssue, doneCleanupStatus, doneSkipVerify = "bd-source", "unpushed", true
	if err := runDone(nil, nil); err != nil {
		t.Fatalf("runDone stale checkpoint recovery: %v", err)
	}
	if _, err := os.Stat(createMarker); err != nil {
		t.Fatalf("stale same-branch MR checkpoint was reused instead of creating for new HEAD: %v", err)
	}
	if data, err := os.ReadFile(descriptionPath); err != nil || !strings.Contains(string(data), "active_mr: gt-mr") || strings.Contains(string(data), "active_mr: gt-legacy") {
		t.Fatalf("active MR missing after stale checkpoint recovery: %s (%v)", data, err)
	}
}

func TestUpdateAgentStateCompletionOwnerRejectsEmptyResolvedAgentBead(t *testing.T) {
	oldResolver := resolveDoneAgentRoleFn
	resolveDoneAgentRoleFn = func(string, string) (RoleInfo, error) {
		return RoleInfo{Role: RolePolecat}, nil
	}
	t.Cleanup(func() { resolveDoneAgentRoleFn = oldResolver })

	err := updateAgentStateOnDoneIfCompletionOwner("/missing/worktree", "/missing/town", ExitCompleted, "gt-work", "generation-1", "attempt-1")
	if err == nil || !strings.Contains(err.Error(), "agent bead ID") {
		t.Fatalf("empty completion-owner agent bead error = %v", err)
	}
}

func TestCompletionOwnerLifecycleWriteFailuresPreserveRecovery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	for _, tt := range []struct {
		name    string
		failEnv string
		wantErr string
	}{
		{name: "hook clear", failEnv: "GT_TEST_FAIL_HOOK_CLEAR", wantErr: "clearing hook_bead"},
		{name: "cleanup status", failEnv: "GT_TEST_FAIL_CLEANUP_STATUS", wantErr: "updating cleanup_status"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			workDir, currentBeadsDir, ownerBeadsDir := setupRoutedSourceTestTown(t)
			descriptionPath, labelsPath := installLateCompletionFailureBDRecorder(t, currentBeadsDir, ownerBeadsDir)
			description := "role_type: polecat\nrig: gastown\nagent_state: completing\nincarnation: fixture-generation\ncompletion_attempt: attempt-1\nhook_bead: bd-source\ncleanup_status: clean\n"
			if err := os.WriteFile(descriptionPath, []byte(description), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(labelsPath, []byte("gt:agent\ndone-intent:COMPLETED:1\ndone-cp:pushed:branch@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:2\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			resetDoneFlagsForTest(t)
			t.Setenv(tt.failEnv, "1")
			t.Setenv("GT_ROLE", "gastown/polecats/refuge")
			t.Setenv("GT_RIG", "gastown")
			t.Setenv("GT_POLECAT", "refuge")
			doneCleanupStatus = "has_stash"
			err := updateAgentStateOnDoneIfCompletionOwner(workDir, routedSourceTestTownRoot(workDir), ExitCompleted, "bd-source", "fixture-generation", "attempt-1")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("completion lifecycle write error = %v, want %q", err, tt.wantErr)
			}
			gotDescription, err := os.ReadFile(descriptionPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, retained := range []string{"agent_state: completing", "completion_attempt: attempt-1"} {
				if !strings.Contains(string(gotDescription), retained) {
					t.Fatalf("recovery field %q lost: %s", retained, gotDescription)
				}
			}
			labels, err := os.ReadFile(labelsPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, marker := range []string{"done-intent:", "done-cp:"} {
				if !strings.Contains(string(labels), marker) {
					t.Fatalf("recovery marker %q lost: %s", marker, labels)
				}
			}
		})
	}
}

func setupRoutedSourceTestTown(t *testing.T) (workDir, currentBeadsDir, ownerBeadsDir string) {
	t.Helper()
	townRoot := canonicalTestTempDir(t)
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write town sentinel: %v", err)
	}

	workDir = filepath.Join(townRoot, "gastown", "polecats", "refuge", "gastown")
	currentBeadsDir = filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	ownerBeadsDir = filepath.Join(townRoot, "beads", "mayor", "rig", ".beads")
	townBeadsDir := filepath.Join(townRoot, ".beads")
	for _, dir := range []string{filepath.Join(workDir, ".beads"), currentBeadsDir, ownerBeadsDir, townBeadsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(workDir, ".beads", "redirect"), []byte("../../../mayor/rig/.beads\n"), 0o644); err != nil {
		t.Fatalf("write redirect: %v", err)
	}
	if err := beads.WriteRoutes(townBeadsDir, []beads.Route{
		{Prefix: "gt-", Path: "gastown/mayor/rig"},
		{Prefix: "bd-", Path: "beads/mayor/rig"},
	}); err != nil {
		t.Fatalf("write routes: %v", err)
	}
	return workDir, currentBeadsDir, ownerBeadsDir
}

func routedSourceTestTownRoot(workDir string) string {
	return filepath.Clean(filepath.Join(workDir, "..", "..", "..", ".."))
}

func setupRoutedSubmitCommandTown(t *testing.T, workDir string) {
	t.Helper()
	townRoot := routedSourceTestTownRoot(workDir)
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	if err := config.SaveRigsConfig(rigsPath, &config.RigsConfig{
		Version: config.CurrentRigsVersion,
		Rigs: map[string]config.RigEntry{
			"gastown": {GitURL: "file://test-gastown"},
		},
	}); err != nil {
		t.Fatalf("save rigs config: %v", err)
	}
}

func setupRoutedSubmitGitRepo(t *testing.T, workDir string, pushBranch bool) string {
	t.Helper()
	remote := t.TempDir()
	runGitForMQSubmitTest(t, remote, "init", "--bare")
	runGitForMQSubmitTest(t, workDir, "init")
	runGitForMQSubmitTest(t, workDir, "config", "user.email", "test@example.com")
	runGitForMQSubmitTest(t, workDir, "config", "user.name", "Test User")
	runGitForMQSubmitTest(t, workDir, "remote", "add", "origin", remote)
	writeMQSubmitTestFile(t, workDir, ".gitignore", ".beads/\n.runtime/\n")
	writeMQSubmitTestFile(t, workDir, "file.txt", "main\n")
	runGitForMQSubmitTest(t, workDir, "add", ".gitignore", "file.txt")
	runGitForMQSubmitTest(t, workDir, "commit", "-m", "main")
	runGitForMQSubmitTest(t, workDir, "branch", "-M", "main")
	runGitForMQSubmitTest(t, workDir, "push", "-u", "origin", "main")
	branch := "feature/routed-submit"
	runGitForMQSubmitTest(t, workDir, "checkout", "-b", branch)
	writeMQSubmitTestFile(t, workDir, "file.txt", "feature\n")
	runGitForMQSubmitTest(t, workDir, "commit", "-am", "feature")
	if pushBranch {
		runGitForMQSubmitTest(t, workDir, "push", "origin", branch)
	}
	return branch
}

func installSubmitSourceBDStub(t *testing.T, currentBeadsDir, ownerBeadsDir string, ownerMissing bool) {
	t.Helper()
	binDir := t.TempDir()
	ownerCase := fmt.Sprintf(`
if [ "$BEADS_DIR" = %q ]; then
  echo '[{"id":"bd-source","title":"owner source","status":"open","priority":1,"issue_type":"task"}]'
  exit 0
fi`, ownerBeadsDir)
	if ownerMissing {
		ownerCase = fmt.Sprintf(`
if [ "$BEADS_DIR" = %q ]; then
  echo "Issue not found in owner" >&2
  exit 1
fi`, ownerBeadsDir)
	}
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--allow-stale" ]; then
  shift
fi
if [ "$1" = "version" ]; then
  echo "bd stub"
  exit 0
fi
if [ "$1" = "show" ] && [ "$2" = "bd-source" ]; then
  if [ "$BEADS_DIR" = %q ]; then
    echo '[{"id":"bd-source","title":"current mirror","status":"open","priority":1,"issue_type":"task"}]'
    exit 0
  fi
%s
  echo "Issue not found in $BEADS_DIR" >&2
  exit 1
fi
echo "unexpected bd command: $*" >&2
exit 1
`, currentBeadsDir, ownerCase)
	path := filepath.Join(binDir, "bd")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	beads.ResetBdAllowStaleCacheForTest()
}

func installSubmitSourceBDRecorder(t *testing.T, currentBeadsDir, ownerBeadsDir string) string {
	t.Helper()
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--allow-stale" ]; then
  shift
fi
if [ "$1" = "version" ]; then
  echo "bd stub"
  exit 0
fi
printf '%%s\t%%s\n' "$BEADS_DIR" "$*" >> %q
if [ "$1" = "show" ] && [ "$2" = "bd-source" ]; then
  if [ "$BEADS_DIR" = %q ]; then
    echo '[{"id":"bd-source","title":"current mirror","status":"open","priority":1,"issue_type":"task","description":"convoy_id: hq-cv-test\\nmerge_strategy: mr"}]'
    exit 0
  fi
  if [ "$BEADS_DIR" = %q ]; then
    echo '[{"id":"bd-source","title":"owner source","status":"open","priority":1,"issue_type":"task","description":"convoy_id: hq-cv-test\\nmerge_strategy: mr"}]'
    exit 0
  fi
  echo "Issue not found in $BEADS_DIR" >&2
  exit 1
fi
if [ "$1" = "show" ] && [ "$2" = "gt-mr" ]; then
  echo '[{"id":"gt-mr","title":"Merge: bd-source","status":"open","priority":1,"issue_type":"task","labels":["gt:merge-request"],"description":"branch: feature/routed-submit\\ntarget: main\\nsource_issue: bd-source\\nrig: gastown"}]'
  exit 0
fi
if [ "$1" = "show" ] && [ "$2" = "gt-gastown-polecat-refuge" ]; then
  echo '[{"id":"gt-gastown-polecat-refuge","title":"Polecat refuge","status":"open","issue_type":"agent","labels":["gt:agent"],"description":"role_type: polecat\\nrig: gastown\\nagent_state: working\\nincarnation: fixture-generation\\nhook_bead: bd-source\\ncleanup_status: clean"}]'
	exit 0
fi
if [ "$1" = "update" ] && [ "$2" = "gt-gastown-polecat-refuge" ]; then
  cat >/dev/null
  exit 0
fi
if [ "$1" = "list" ]; then
  echo '[]'
  exit 0
fi
if [ "$1" = "sql" ]; then
  echo '[]'
  exit 0
fi
if [ "$1" = "create" ]; then
  echo '{"id":"gt-mr","title":"Merge: bd-source","status":"open","priority":1,"issue_type":"task","labels":["gt:merge-request"]}'
  exit 0
fi
if [ "$1" = "comments" ] && [ "$2" = "add" ]; then
  exit 0
fi
if [ "$1" = "close" ]; then
  exit 0
fi
echo "unexpected bd command: $*" >&2
exit 1
`, logPath, currentBeadsDir, ownerBeadsDir)
	path := filepath.Join(binDir, "bd")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write bd recorder: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	beads.ResetBdAllowStaleCacheForTest()
	t.Cleanup(beads.ResetBdAllowStaleCacheForTest)
	return logPath
}

func installLateCompletionFailureBDRecorder(t *testing.T, currentBeadsDir, ownerBeadsDir string) (string, string) {
	t.Helper()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	descriptionPath := filepath.Join(stateDir, "agent.description")
	labelsPath := filepath.Join(stateDir, "agent.labels")
	description := "role_type: polecat\nrig: gastown\nagent_state: working\nincarnation: fixture-generation\nhook_bead: bd-source\ncleanup_status: clean\n"
	if err := os.WriteFile(descriptionPath, []byte(description), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(labelsPath, []byte("gt:agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
while [ "$1" = "--allow-stale" ]; do shift; done
if [ "$1" = "version" ]; then echo "bd stub"; exit 0; fi

json_string_file() {
  awk 'BEGIN { printf "\"" } { gsub(/\\/, "\\\\"); gsub(/\"/, "\\\""); if (NR > 1) printf "\\n"; printf "%%s", $0 } END { print "\"" }' "$1"
}
json_labels_file() {
  awk 'BEGIN { printf "[" } { gsub(/\\/, "\\\\"); gsub(/\"/, "\\\""); if (NR > 1) printf ","; printf "\"%%s\"", $0 } END { print "]" }' "$1"
}

if [ "$1" = "show" ] && [ "$2" = "gt-gastown-polecat-refuge" ]; then
  printf '[{"id":"gt-gastown-polecat-refuge","title":"Polecat refuge","status":"open","issue_type":"agent","labels":'
  json_labels_file %q
  printf ',"description":'
  json_string_file %q
  printf '}]\n'
  exit 0
fi
if [ "$1" = "show" ] && [ "$2" = "bd-source" ]; then
  if [ "$BEADS_DIR" = %q ]; then
    printf '%%s\n' '[{"id":"bd-source","title":"current mirror","status":"open","priority":1,"issue_type":"task","description":"convoy_id: hq-cv-test\nmerge_strategy: mr"}]'
    exit 0
  fi
  if [ "$BEADS_DIR" = %q ]; then
    printf '%%s\n' '[{"id":"bd-source","title":"owner source","status":"open","priority":1,"issue_type":"task","description":"convoy_id: hq-cv-test\nmerge_strategy: mr"}]'
    exit 0
  fi
fi
if [ "$1" = "show" ] && [ "$2" = "gt-mr" ]; then
  printf '%%s\n' '[{"id":"gt-mr","title":"Merge: bd-source","status":"open","priority":1,"issue_type":"task","labels":["gt:merge-request"],"description":"branch: feature/routed-submit\ntarget: main\nsource_issue: bd-source\nrig: gastown\ncommit_sha: '"${GT_TEST_MR_COMMIT_SHA:-}"'"}]'
  exit 0
fi
if [ "$1" = "show" ] && [ "$2" = "--json" ] && [ "$3" = "gt-legacy" ]; then
	printf '%%s\n' '[{"id":"gt-legacy","title":"Legacy merge: bd-source","status":"open","priority":1,"issue_type":"task","labels":["gt:merge-request"],"description":"branch: feature/routed-submit\ntarget: main\nsource_issue: bd-source\nrig: gastown"}]'
	exit 0
fi
if [ "$1" = "update" ] && [ "$2" = "gt-gastown-polecat-refuge" ]; then
  body=""
  for arg in "$@"; do
    if [ "$arg" = "--body-file=-" ]; then
      body=%q.tmp
      cat > "$body"
	  if [ "$GT_TEST_FAIL_HOOK_CLEAR" = "1" ] && grep -q '^hook_bead: null$' "$body"; then
		rm -f "$body"; echo 'injected hook clear failure' >&2; exit 1
	  fi
	      if [ "$GT_TEST_FAIL_CLEANUP_STATUS" = "1" ] && grep -q '^cleanup_status: has_stash$' "$body"; then
		rm -f "$body"; echo 'injected cleanup status failure' >&2; exit 1
	      fi
	      if [ "$GT_TEST_FAIL_ACTIVE_MR_ONCE" = "1" ] && grep -q '^active_mr:' "$body" && [ ! -f "$GT_TEST_ACTIVE_MR_FAIL_FILE" ]; then
	        touch "$GT_TEST_ACTIVE_MR_FAIL_FILE"
	        rm -f "$body"; echo 'injected active_mr failure' >&2; exit 1
	      fi
	      if [ "${GT_TEST_ALLOW_FINAL_COMPLETION:-0}" != "1" ] && grep -Eq '^agent_state: (done|stuck)$' "$body"; then
        rm -f "$body"
        echo 'injected final completion failure' >&2
        exit 1
      fi
      mv "$body" %q
    fi
  done
  for arg in "$@"; do
    case "$arg" in
      --add-label=*) printf '%%s\n' "${arg#--add-label=}" >> %q ;;
      --remove-label=*) grep -Fvx "${arg#--remove-label=}" %q > %q.tmp || true; mv %q.tmp %q ;;
    esac
  done
  exit 0
fi
if [ "$1" = "list" ]; then
	if [ "$GT_TEST_LEGACY_MR" = "1" ]; then
		printf '%%s\n' '[{"id":"gt-legacy","title":"Legacy merge: bd-source","status":"open","priority":1,"issue_type":"task","labels":["gt:merge-request"],"description":"branch: feature/routed-submit\ntarget: main\nsource_issue: bd-source\nrig: gastown"}]'
	else
		echo '[]'
	fi
	exit 0
fi
if [ "$1" = "sql" ]; then echo '[]'; exit 0; fi
if [ "$1" = "create" ]; then
	if [ "$GT_TEST_FAIL_MR_CREATE" = "1" ]; then echo 'injected MR creation failure' >&2; exit 1; fi
	if [ -n "${GT_TEST_MR_CREATE_FILE:-}" ]; then touch "$GT_TEST_MR_CREATE_FILE"; fi
  echo '{"id":"gt-mr","title":"Merge: bd-source","status":"open","priority":1,"issue_type":"task","labels":["gt:merge-request"]}'
  exit 0
fi
if [ "$1" = "comments" ] || [ "$1" = "close" ]; then cat >/dev/null; exit 0; fi
echo "unexpected bd command: $*" >&2
exit 1
`, labelsPath, descriptionPath, currentBeadsDir, ownerBeadsDir,
		descriptionPath, descriptionPath, labelsPath, labelsPath, labelsPath, labelsPath, labelsPath)
	path := filepath.Join(binDir, "bd")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	beads.ResetBdAllowStaleCacheForTest()
	t.Cleanup(beads.ResetBdAllowStaleCacheForTest)
	return descriptionPath, labelsPath
}

func readSubmitSourceBDLog(t *testing.T, logPath string) string {
	t.Helper()
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd recorder log: %v", err)
	}
	return string(log)
}

func assertBDLogContains(t *testing.T, log, beadsDir, args string) {
	t.Helper()
	needle := beadsDir + "\t" + args
	if !strings.Contains(log, needle) {
		t.Fatalf("bd log missing %q:\n%s", needle, log)
	}
}

func assertBDLogNotContains(t *testing.T, log, beadsDir, args string) {
	t.Helper()
	needle := beadsDir + "\t" + args
	if strings.Contains(log, needle) {
		t.Fatalf("bd log unexpectedly contains %q:\n%s", needle, log)
	}
}

func resetMqSubmitFlagsForTest(t *testing.T) {
	t.Helper()
	oldBranch, oldIssue, oldEpic := mqSubmitBranch, mqSubmitIssue, mqSubmitEpic
	oldPriority := mqSubmitPriority
	oldNoCleanup, oldSkipDeps, oldResubmit := mqSubmitNoCleanup, mqSubmitSkipDeps, mqSubmitResubmit
	mqSubmitBranch, mqSubmitIssue, mqSubmitEpic = "", "", ""
	mqSubmitPriority = -1
	mqSubmitNoCleanup, mqSubmitSkipDeps, mqSubmitResubmit = false, false, false
	t.Cleanup(func() {
		mqSubmitBranch, mqSubmitIssue, mqSubmitEpic = oldBranch, oldIssue, oldEpic
		mqSubmitPriority = oldPriority
		mqSubmitNoCleanup, mqSubmitSkipDeps, mqSubmitResubmit = oldNoCleanup, oldSkipDeps, oldResubmit
	})
}

func resetDoneFlagsForTest(t *testing.T) {
	t.Helper()
	oldIssue, oldStatus, oldCleanupStatus, oldTarget := doneIssue, doneStatus, doneCleanupStatus, doneTarget
	oldPriority := donePriority
	oldResume, oldPreVerified, oldSkipVerify := doneResume, donePreVerified, doneSkipVerify
	oldUpdateAgentStateOnDoneFn := updateAgentStateOnDoneFn
	doneIssue = ""
	donePriority = -1
	doneStatus = ExitCompleted
	doneCleanupStatus = ""
	doneResume = false
	donePreVerified = false
	doneTarget = ""
	doneSkipVerify = false
	t.Cleanup(func() {
		doneIssue, doneStatus, doneCleanupStatus, doneTarget = oldIssue, oldStatus, oldCleanupStatus, oldTarget
		donePriority = oldPriority
		doneResume, donePreVerified, doneSkipVerify = oldResume, oldPreVerified, oldSkipVerify
		updateAgentStateOnDoneFn = oldUpdateAgentStateOnDoneFn
	})
}
