package beads

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func installMockBDFixedShowOutput(t *testing.T, showOutput string) {
	t.Helper()

	binDir := t.TempDir()
	if runtime.GOOS == "windows" {
		scriptPath := filepath.Join(binDir, "bd.cmd")
		script := "@echo off\r\n" +
			"setlocal EnableDelayedExpansion\r\n" +
			"set \"cmd=\"\r\n" +
			":findcmd\r\n" +
			"if \"%~1\"==\"\" goto havecmd\r\n" +
			"set \"arg=%~1\"\r\n" +
			"if /I \"!arg:~0,2!\"==\"--\" (\r\n" +
			"  shift\r\n" +
			"  goto findcmd\r\n" +
			")\r\n" +
			"set \"cmd=%~1\"\r\n" +
			":havecmd\r\n" +
			"if /I \"%cmd%\"==\"version\" exit /b 0\r\n" +
			"if /I \"%cmd%\"==\"show\" (\r\n" +
			"  echo(%MOCK_BD_SHOW_OUTPUT%\r\n" +
			"  exit /b 0\r\n" +
			")\r\n" +
			"exit /b 0\r\n"
		if err := os.WriteFile(scriptPath, []byte(script), 0644); err != nil {
			t.Fatalf("write mock bd: %v", err)
		}
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("MOCK_BD_SHOW_OUTPUT", showOutput)
		return
	}

	script := `#!/bin/sh
cmd=""
for arg in "$@"; do
  case "$arg" in
    --*) ;;
    *) cmd="$arg"; break ;;
  esac
done

case "$cmd" in
  version)
    exit 0
    ;;
  show)
    printf '%s\n' "$MOCK_BD_SHOW_OUTPUT"
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`
	scriptPath := filepath.Join(binDir, "bd")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write mock bd: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOCK_BD_SHOW_OUTPUT", showOutput)
}

func installMockBDShowRecorder(t *testing.T, showOutput string) string {
	t.Helper()

	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "bd.log")

	script := `#!/bin/sh
LOG_FILE='` + logPath + `'
printf '%s\n' "$*" >> "$LOG_FILE"

cmd=""
for arg in "$@"; do
  case "$arg" in
    --*) ;;
    *) cmd="$arg"; break ;;
  esac
done

case "$cmd" in
  version)
    exit 0
    ;;
  show)
    printf '%s\n' "$MOCK_BD_SHOW_OUTPUT"
    exit 0
    ;;
  update)
	cat >> "$LOG_FILE"
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`
	scriptPath := filepath.Join(binDir, "bd")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write mock bd: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOCK_BD_SHOW_OUTPUT", showOutput)
	return logPath
}

func installMockBDRequireExplicitBeadsDir(t *testing.T, expectedBeadsDir string) {
	t.Helper()

	binDir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
cmd=""
for arg in "$@"; do
  case "$arg" in
    --*) ;;
    *) cmd="$arg"; break ;;
  esac
done

target="${BEADS_DIR:-$(pwd)/.beads}"
if [ "$target" != "%s" ]; then
  echo "wrong target $target" >&2
  exit 9
fi

case "$cmd" in
  version)
    exit 0
    ;;
  show)
    printf '%%s\n' '[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":"role_type: polecat\nrig: gastown\nagent_state: idle\nhook_bead: null","agent_state":"idle"}]'
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, expectedBeadsDir)
	scriptPath := filepath.Join(binDir, "bd")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write mock bd: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestGetAgentBead_PrefersDescriptionAgentState(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	installMockBDFixedShowOutput(t, `[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":"role_type: polecat\nrig: gastown\nagent_state: spawning\nhook_bead: null","agent_state":"idle"}]`)

	bd := NewIsolated(tmpDir)
	issue, fields, err := bd.GetAgentBead("gt-gastown-polecat-nux")
	if err != nil {
		t.Fatalf("GetAgentBead: %v", err)
	}
	if issue == nil {
		t.Fatal("GetAgentBead returned nil issue")
	}
	if fields == nil {
		t.Fatal("GetAgentBead returned nil fields")
	}
	if issue.AgentState != "idle" {
		t.Fatalf("issue.AgentState = %q, want %q", issue.AgentState, "idle")
	}
	// Description agent_state ("spawning") now takes priority over the legacy
	// structured column ("idle") per the bd 0.62+ contract.
	if fields.AgentState != "spawning" {
		t.Fatalf("fields.AgentState = %q, want %q (description should win)", fields.AgentState, "spawning")
	}
}

func TestGetAgentBead_FallsBackToDescriptionAgentState(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	installMockBDFixedShowOutput(t, `[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":"role_type: polecat\nrig: gastown\nagent_state: spawning\nhook_bead: null"}]`)

	bd := NewIsolated(tmpDir)
	_, fields, err := bd.GetAgentBead("gt-gastown-polecat-nux")
	if err != nil {
		t.Fatalf("GetAgentBead: %v", err)
	}
	if fields == nil {
		t.Fatal("GetAgentBead returned nil fields")
	}
	if fields.AgentState != "spawning" {
		t.Fatalf("fields.AgentState = %q, want %q", fields.AgentState, "spawning")
	}
}

func TestUpdateAgentState_UsesUpdateDescriptionPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	logPath := installMockBDShowRecorder(t, `[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":"role_type: polecat\nrig: gastown\nagent_state: spawning\nhook_bead: null"}]`)
	bd := NewIsolated(tmpDir)

	if err := bd.UpdateAgentState("gt-gastown-polecat-nux", "working"); err != nil {
		t.Fatalf("UpdateAgentState: %v", err)
	}

	logOutput := readMockBDLog(t, logPath)
	if !strings.Contains(logOutput, "show gt-gastown-polecat-nux --json") {
		t.Fatalf("mock bd log %q missing show call", logOutput)
	}
	if !strings.Contains(logOutput, "update gt-gastown-polecat-nux") {
		t.Fatalf("mock bd log %q missing update call", logOutput)
	}
	// Should NOT use the obsolete bd agent state or bd set-state path
	if strings.Contains(logOutput, "agent state") || strings.Contains(logOutput, "set-state") {
		t.Fatalf("mock bd log %q unexpectedly used obsolete bd agent state / set-state path", logOutput)
	}
}

func TestCompareAndUpdateAgentDescriptionFieldsUpdatesMatchingStateAtomically(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	logPath := installMockBDShowRecorder(t, `[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":"role_type: polecat\nrig: gastown\nagent_state: stuck\nhook_bead: null\ncleanup_status: has_unpushed"}]`)
	bd := NewIsolated(tmpDir)
	stuck, dirty := "stuck", "has_unpushed"
	idle, clean := "idle", "clean"

	err := bd.CompareAndUpdateAgentDescriptionFields(
		"gt-gastown-polecat-nux",
		AgentFieldExpectations{AgentState: &stuck, CleanupStatus: &dirty},
		AgentFieldUpdates{AgentState: &idle, CleanupStatus: &clean},
	)
	if err != nil {
		t.Fatalf("CompareAndUpdateAgentDescriptionFields: %v", err)
	}

	logOutput := readMockBDLog(t, logPath)
	if got := strings.Count(logOutput, "update gt-gastown-polecat-nux"); got != 1 {
		t.Fatalf("update calls = %d, want 1; log: %q", got, logOutput)
	}
	if !strings.Contains(logOutput, "agent_state: idle") || !strings.Contains(logOutput, "cleanup_status: clean") {
		t.Fatalf("mock bd log %q missing atomic state and cleanup update", logOutput)
	}
}

func TestCompareAndUpdateAgentDescriptionFieldsRejectsChangedExpectations(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tests := []struct {
		name        string
		description string
		expected    AgentFieldExpectations
	}{
		{
			name:        "agent state",
			description: "role_type: polecat\nrig: gastown\nagent_state: working\ncleanup_status: has_unpushed",
			expected:    AgentFieldExpectations{AgentState: stringPointer("stuck")},
		},
		{
			name:        "cleanup status",
			description: "role_type: polecat\nrig: gastown\nagent_state: stuck\ncleanup_status: clean",
			expected:    AgentFieldExpectations{CleanupStatus: stringPointer("has_unpushed")},
		},
		{
			name:        "hook bead",
			description: "role_type: polecat\nrig: gastown\nagent_state: stuck\nhook_bead: gt-new",
			expected:    AgentFieldExpectations{HookBead: stringPointer("")},
		},
		{
			name:        "active mr",
			description: "role_type: polecat\nrig: gastown\nagent_state: stuck\nactive_mr: gt-mr-new",
			expected:    AgentFieldExpectations{ActiveMR: stringPointer("")},
		},
		{
			name:        "incarnation same-value ABA",
			description: "role_type: polecat\nrig: gastown\nagent_state: stuck\ncleanup_status: has_unpushed\nincarnation: replacement-generation",
			expected:    AgentFieldExpectations{Incarnation: stringPointer("original-generation")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755); err != nil {
				t.Fatalf("mkdir .beads: %v", err)
			}
			showOutput := fmt.Sprintf(`[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":%q}]`, tt.description)
			logPath := installMockBDShowRecorder(t, showOutput)
			bd := NewIsolated(tmpDir)
			idle := "idle"

			err := bd.CompareAndUpdateAgentDescriptionFields(
				"gt-gastown-polecat-nux",
				tt.expected,
				AgentFieldUpdates{AgentState: &idle},
			)
			if !errors.Is(err, ErrAgentFieldsChanged) {
				t.Fatalf("error = %v, want ErrAgentFieldsChanged", err)
			}
			if logOutput := readMockBDLog(t, logPath); strings.Contains(logOutput, "update gt-gastown-polecat-nux") {
				t.Fatalf("mock bd log %q unexpectedly updated changed fields", logOutput)
			}
		})
	}
}

func TestCompareRevalidateAndUpdateAgentDescriptionFieldsRejectsStructuredDrift(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	description := "role_type: polecat\nrig: gastown\nagent_state: stuck\nincarnation: generation-1\nhook_bead: null\ncleanup_status: has_unpushed"
	showOutput := fmt.Sprintf(`[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"agent_state":"stuck","hook_bead":"gt-new","description":%q}]`, description)
	logPath := installMockBDShowRecorder(t, showOutput)
	bd := NewIsolated(tmpDir)
	expected := agentFieldsFromIssue(&Issue{Description: description, AgentState: "stuck"}).LifecycleExpectations()
	idle, clean := "idle", "clean"
	revalidated := false

	err := bd.CompareRevalidateAndUpdateAgentDescriptionFields(
		"gt-gastown-polecat-nux",
		expected,
		AgentFieldUpdates{AgentState: &idle, CleanupStatus: &clean},
		func(*Issue, *AgentFields) error {
			revalidated = true
			return nil
		},
	)
	if !errors.Is(err, ErrAgentFieldsChanged) {
		t.Fatalf("error = %v, want ErrAgentFieldsChanged", err)
	}
	if revalidated {
		t.Fatal("revalidated after structured hook drift")
	}
	if logOutput := readMockBDLog(t, logPath); strings.Contains(logOutput, "update gt-gastown-polecat-nux") {
		t.Fatalf("mock bd log %q unexpectedly updated after structured drift", logOutput)
	}
}

func TestCompareRevalidateAndUpdateAgentDescriptionFieldsPreservesOtherFields(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	description := "role_type: polecat\nrig: gastown\nagent_state: stuck\nincarnation: generation-1\nhook_bead: null\ncleanup_status: has_unpushed\nactive_mr: mr-1\nnotification_level: muted\nbranch: polecat/nux/work\nlast_source_issue: gt-task"
	showOutput := fmt.Sprintf(`[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"agent_state":"stuck","description":%q}]`, description)
	logPath := installMockBDShowRecorder(t, showOutput)
	bd := NewIsolated(tmpDir)
	fields := agentFieldsFromIssue(&Issue{Description: description, AgentState: "stuck"})
	idle, clean := "idle", "clean"

	err := bd.CompareRevalidateAndUpdateAgentDescriptionFields(
		"gt-gastown-polecat-nux",
		fields.LifecycleExpectations(),
		AgentFieldUpdates{AgentState: &idle, CleanupStatus: &clean},
		func(issue *Issue, current *AgentFields) error {
			if issue == nil || current == nil || current.Branch != "polecat/nux/work" {
				t.Fatalf("locked snapshot = issue=%+v fields=%+v", issue, current)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("CompareRevalidateAndUpdateAgentDescriptionFields: %v", err)
	}
	logOutput := readMockBDLog(t, logPath)
	for _, want := range []string{"agent_state: idle", "cleanup_status: clean", "active_mr: mr-1", "notification_level: muted", "branch: polecat/nux/work", "last_source_issue: gt-task"} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("mock bd log %q missing preserved field %q", logOutput, want)
		}
	}
}

func TestResetAgentBeadForReuseIfUnchangedRejectsSameIncarnationLifecycleDrift(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	before := "role_type: polecat\nrig: gastown\nagent_state: stuck\nincarnation: generation-1\nhook_bead: null\ncleanup_status: clean\nbranch: polecat/nux/old\nlast_source_issue: gt-old"
	after := "role_type: polecat\nrig: gastown\nagent_state: stuck\nincarnation: generation-1\nhook_bead: gt-new\ncleanup_status: clean\nbranch: polecat/nux/new\nlast_source_issue: gt-new"
	showOutput := fmt.Sprintf(`[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"agent_state":"stuck","hook_bead":"gt-new","description":%q}]`, after)
	logPath := installMockBDShowRecorder(t, showOutput)
	bd := NewIsolated(tmpDir)
	expected := agentFieldsFromIssue(&Issue{Description: before, AgentState: "stuck"}).LifecycleExpectations()

	err := bd.ResetAgentBeadForReuseIfUnchanged(
		"gt-gastown-polecat-nux",
		"polecat removed",
		expected,
	)
	if !errors.Is(err, ErrAgentFieldsChanged) {
		t.Fatalf("error = %v, want ErrAgentFieldsChanged", err)
	}
	if logOutput := readMockBDLog(t, logPath); strings.Contains(logOutput, "update gt-gastown-polecat-nux") {
		t.Fatalf("mock bd log %q unexpectedly reset drifted lifecycle fields", logOutput)
	}
}

func TestResetAgentBeadForReuseFencesBeforeDestructiveCallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	description := "role_type: polecat\nrig: gastown\nagent_state: stuck\nincarnation: generation-1\nhook_bead: null\ncleanup_status: clean"
	showOutput := fmt.Sprintf(`[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":%q}]`, description)
	logPath := installMockBDShowRecorder(t, showOutput)
	bd := NewIsolated(tmpDir)
	expected := agentFieldsFromIssue(&Issue{Description: description}).LifecycleExpectations()
	callbackErr := errors.New("injected destructive callback failure")

	err := bd.ResetAgentBeadForReuseIfUnchangedRevalidatedAfter(
		"gt-gastown-polecat-nux",
		"polecat removed",
		expected,
		nil,
		func() error {
			if log := readMockBDLog(t, logPath); !strings.Contains(log, "agent_state: retiring") {
				t.Fatalf("destructive callback ran before durable retiring fence: %q", log)
			}
			return callbackErr
		},
	)
	if !errors.Is(err, callbackErr) || !errors.Is(err, ErrAgentRetirementFenced) {
		t.Fatalf("error = %v, want callback error plus retirement fence", err)
	}
	logOutput := readMockBDLog(t, logPath)
	if got := strings.Count(logOutput, "update gt-gastown-polecat-nux"); got != 1 {
		t.Fatalf("updates after callback failure = %d, want only durable fence; log=%q", got, logOutput)
	}
}

func TestAgentRetirementRecordRoundTrips(t *testing.T) {
	fields := &AgentFields{RetirementPhase: AgentRetirementPhaseLocalRemoved}
	applyAgentRetirementRecord(fields, AgentRetirementRecord{
		Version: AgentRetirementJournalVersion, Phase: AgentRetirementPhaseWorkUnassigned,
		HookBead: "gt-work", LastSourceIssue: "gt-previous",
		WorkReceipts: []AgentRetirementWorkReceipt{{WorkBead: "gt-previous"}, {WorkBead: "gt-work", Molecule: "gt-wisp"}},
		ClonePath:    "/tmp/polecat", Branch: "polecat/nux", GitHead: "abc123", GitState: `{"WorktreePresent":true,"Head":"abc123"}`,
		Targets: []string{"refs/heads/main", "refs/remotes/origin/main"},
	})
	got := ParseAgentFields(FormatAgentDescription("Polecat nux", fields)).RetirementRecord()
	if got.Version != AgentRetirementJournalVersion || got.Phase != AgentRetirementPhaseWorkUnassigned ||
		got.HookBead != "gt-work" || got.LastSourceIssue != "gt-previous" ||
		!slices.Equal(got.WorkReceipts, []AgentRetirementWorkReceipt{{WorkBead: "gt-previous"}, {WorkBead: "gt-work", Molecule: "gt-wisp"}}) ||
		got.ClonePath != "/tmp/polecat" || got.Branch != "polecat/nux" || got.GitHead != "abc123" ||
		got.GitState != `{"WorktreePresent":true,"Head":"abc123"}` || !slices.Equal(got.Targets, []string{"refs/heads/main", "refs/remotes/origin/main"}) {
		t.Fatalf("retirement record = %+v", got)
	}
}

func TestParsedRetirementRecordRejectsMalformedJSONFields(t *testing.T) {
	base := "retirement_version: 1\nretirement_phase: fenced\nretirement_clone_path: /tmp/polecat\nretirement_git_state: {}\n"
	for _, tt := range []struct {
		name  string
		field string
	}{
		{name: "work receipts", field: "retirement_work_receipts: not-json\n"},
		{name: "targets", field: "retirement_targets: not-json\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			record := ParseAgentFields(base + tt.field).RetirementRecord()
			if err := ValidateAgentRetirementRecord(record); err == nil {
				t.Fatalf("malformed %s passed retirement validation: %+v", tt.name, record)
			}
		})
	}
}

func TestValidateAgentRetirementRecordRejectsIncompleteReceipts(t *testing.T) {
	valid := AgentRetirementRecord{
		Version:  AgentRetirementJournalVersion,
		Phase:    AgentRetirementPhaseFenced,
		HookBead: "gt-work", LastSourceIssue: "gt-previous",
		WorkReceipts: []AgentRetirementWorkReceipt{{WorkBead: "gt-previous"}, {WorkBead: "gt-work", Molecule: "gt-wisp"}},
		ClonePath:    "/tmp/polecat", Branch: "polecat/nux", GitHead: "abc123", GitState: `{"WorktreePresent":true,"Head":"abc123"}`,
		Targets: []string{"refs/heads/main"},
	}
	if err := ValidateAgentRetirementRecord(valid); err != nil {
		t.Fatalf("valid record: %v", err)
	}
	branchless := valid
	branchless.Branch, branchless.GitHead, branchless.Targets = "", "", nil
	branchless.GitState = `{"WorktreePresent":false}`
	if err := ValidateAgentRetirementRecord(branchless); err != nil {
		t.Fatalf("valid branchless record with absent worktree: %v", err)
	}

	tests := []struct {
		name string
		edit func(*AgentRetirementRecord)
	}{
		{name: "missing version", edit: func(r *AgentRetirementRecord) { r.Version = "" }},
		{name: "unsupported future version", edit: func(r *AgentRetirementRecord) { r.Version = "2" }},
		{name: "unknown phase", edit: func(r *AgentRetirementRecord) { r.Phase = "future-phase" }},
		{name: "relative clone", edit: func(r *AgentRetirementRecord) { r.ClonePath = "relative/polecat" }},
		{name: "unclean absolute clone", edit: func(r *AgentRetirementRecord) { r.ClonePath = "/tmp/../tmp/polecat" }},
		{name: "missing git state", edit: func(r *AgentRetirementRecord) { r.GitState = "" }},
		{name: "non-authoritative work", edit: func(r *AgentRetirementRecord) { r.WorkReceipts[0].WorkBead = "gt-other" }},
		{name: "missing last source receipt", edit: func(r *AgentRetirementRecord) { r.WorkReceipts = r.WorkReceipts[1:] }},
		{name: "duplicate work receipt", edit: func(r *AgentRetirementRecord) { r.WorkReceipts[0] = r.WorkReceipts[1] }},
		{name: "branch without head", edit: func(r *AgentRetirementRecord) { r.GitHead = "" }},
		{name: "branch without targets", edit: func(r *AgentRetirementRecord) { r.Targets = nil }},
		{name: "duplicate targets", edit: func(r *AgentRetirementRecord) { r.Targets = []string{"refs/heads/main", "refs/heads/main"} }},
		{name: "empty Git custody", edit: func(r *AgentRetirementRecord) { r.GitState = `{}` }},
		{name: "null worktree custody", edit: func(r *AgentRetirementRecord) { r.GitState = `{"WorktreePresent":null}` }},
		{name: "string worktree custody", edit: func(r *AgentRetirementRecord) { r.GitState = `{"WorktreePresent":"true"}` }},
		{name: "numeric worktree custody", edit: func(r *AgentRetirementRecord) { r.GitState = `{"WorktreePresent":1}` }},
		{name: "self preservation target", edit: func(r *AgentRetirementRecord) { r.Targets = []string{"refs/heads/polecat/nux"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := valid
			record.Targets = append([]string(nil), valid.Targets...)
			record.WorkReceipts = append([]AgentRetirementWorkReceipt(nil), valid.WorkReceipts...)
			tt.edit(&record)
			if err := ValidateAgentRetirementRecord(record); err == nil {
				t.Fatalf("ValidateAgentRetirementRecord(%+v) succeeded", record)
			}
		})
	}
}

func TestClearAgentActiveMRIfMatchesRejectsFrozenAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	for _, state := range []string{"completing", "retiring", "nuked"} {
		t.Run(state, func(t *testing.T) {
			tmpDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0o755); err != nil {
				t.Fatal(err)
			}
			show := `[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":"role_type: polecat\nrig: gastown\nagent_state: ` + state + `\nactive_mr: gt-wisp-old"}]`
			logPath := installMockBDShowRecorder(t, show)
			cleared, err := NewIsolated(tmpDir).ClearAgentActiveMRIfMatches("gt-gastown-polecat-nux", "gt-wisp-old")
			if err == nil || cleared {
				t.Fatalf("frozen clear = %v, %v", cleared, err)
			}
			if log := readMockBDLog(t, logPath); strings.Contains(log, "update ") {
				t.Fatalf("frozen agent mutated: %q", log)
			}
		})
	}
}

func TestFinalizeAgentCompletionIfOwnerPreservesRecoveryMarkersOnFailedAtomicWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	showOutput := `[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent","done-intent:COMPLETED:1","done-cp:pushed:branch:2"],"description":"role_type: polecat\nrig: gastown\nagent_state: completing\nincarnation: generation-1\ncompletion_attempt: attempt-1"}]`
	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "bd.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$MOCK_BD_LOG"
while [ "$1" = "--allow-stale" ]; do shift; done
case "$1" in
version) exit 0 ;;
show) printf '%s\n' "$MOCK_BD_SHOW_OUTPUT" ;;
update) cat >> "$MOCK_BD_LOG"; echo 'injected atomic update failure' >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOCK_BD_LOG", logPath)
	t.Setenv("MOCK_BD_SHOW_OUTPUT", showOutput)
	ResetBdAllowStaleCacheForTest()
	t.Cleanup(ResetBdAllowStaleCacheForTest)

	bd := NewIsolated(tmpDir)
	err := bd.FinalizeAgentCompletionIfOwner("gt-gastown-polecat-nux", "generation-1", "attempt-1", AgentStateDone)
	if err == nil {
		t.Fatal("final completion suppressed atomic update failure")
	}
	log := readMockBDLog(t, logPath)
	if updates := strings.Count(log, "update gt-gastown-polecat-nux"); updates != 1 {
		t.Fatalf("final completion updates = %d, want one atomic write; log=%q", updates, log)
	}
	for _, want := range []string{
		"--body-file=-",
		"--remove-label=done-intent:COMPLETED:1",
		"--remove-label=done-cp:pushed:branch:2",
		"agent_state: done",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("atomic finalization log missing %q: %q", want, log)
		}
	}
	issue, showErr := bd.Show("gt-gastown-polecat-nux")
	if showErr != nil {
		t.Fatal(showErr)
	}
	if fields := ParseAgentFields(issue.Description); fields.AgentState != string(AgentStateCompleting) {
		t.Fatalf("agent state after failed finalization = %q", fields.AgentState)
	}
	if !HasLabel(issue, "done-intent:COMPLETED:1") || !HasLabel(issue, "done-cp:pushed:branch:2") {
		t.Fatalf("recovery markers lost after failed finalization: %v", issue.Labels)
	}
}

func TestNextAgentRetirementPhaseRejectsSkippedAndUnknownTransitions(t *testing.T) {
	if err := ValidateAgentRetirementTransition(AgentRetirementPhaseFenced, AgentRetirementPhaseSessionStopped); err != nil {
		t.Fatalf("exact successor: %v", err)
	}
	for _, transition := range [][2]string{
		{AgentRetirementPhaseFenced, AgentRetirementPhaseLocalRemoved},
		{AgentRetirementPhaseLocalRemoved, AgentRetirementPhaseFenced},
		{"future-phase", AgentRetirementPhaseSessionStopped},
		{AgentRetirementPhaseFenced, "future-phase"},
	} {
		if err := ValidateAgentRetirementTransition(transition[0], transition[1]); err == nil {
			t.Fatalf("transition %q -> %q succeeded", transition[0], transition[1])
		}
	}
}

func TestCompareAndRestoreIssueStatusIfAssigneeRejectsMissingMetadata(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	showOutput := `[{"id":"gt-work","title":"work","issue_type":"task","status":"in_progress","assignee":"gastown/polecats/nux"}]`
	logPath := installMockBDShowRecorder(t, showOutput)
	bd := NewIsolated(tmpDir)
	if err := bd.CompareAndRestoreIssueStatusIfAssignee("gt-work", "in_progress", "gastown/polecats/nux", "hooked"); err == nil || !strings.Contains(err.Error(), "missing Dolt metadata") {
		t.Fatalf("CompareAndRestoreIssueStatusIfAssignee: %v", err)
	}
	if log := readMockBDLog(t, logPath); strings.Contains(log, "update gt-work") {
		t.Fatalf("missing metadata mutated issue: %q", log)
	}
}

func TestCompareAndRestoreIssueStatusIfAssigneePreservesStatusDrift(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	showOutput := `[{"id":"gt-work","title":"work","issue_type":"task","status":"blocked","assignee":"gastown/polecats/nux"}]`
	logPath := installMockBDShowRecorder(t, showOutput)
	bd := NewIsolated(tmpDir)
	err := bd.CompareAndRestoreIssueStatusIfAssignee("gt-work", "in_progress", "gastown/polecats/nux", "open")
	if err == nil || !strings.Contains(err.Error(), "missing Dolt metadata") {
		t.Fatalf("status drift error = %v, want missing metadata", err)
	}
	if log := readMockBDLog(t, logPath); strings.Contains(log, "update gt-work") {
		t.Fatalf("status drift overwritten: %q", log)
	}
}

func TestCompareAndRestoreIssueAssignmentIfMatchesRestoresExactReceipt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	showOutput := `[{"id":"gt-work","title":"work","issue_type":"task","status":"hooked","assignee":"gastown/polecats/nux"}]`
	logPath := installMockBDShowRecorder(t, showOutput)
	bd := NewIsolated(tmpDir)
	if err := bd.CompareAndRestoreIssueAssignmentIfMatches("gt-work", "hooked", "gastown/polecats/nux", "open", ""); err == nil || !strings.Contains(err.Error(), "missing Dolt metadata") {
		t.Fatalf("CompareAndRestoreIssueAssignmentIfMatches: %v", err)
	}
	log := readMockBDLog(t, logPath)
	if strings.Contains(log, "update gt-work") {
		t.Fatalf("missing metadata mutated assignment: %q", log)
	}
}

func TestRetireAgentGenerationRejectsMalformedResumeBeforeCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	description := "role_type: polecat\nrig: gastown\nagent_state: retiring\nincarnation: generation-1\nretirement_version: 2\nretirement_phase: fenced\nretirement_clone_path: /tmp/polecat\nretirement_git_state: {}"
	showOutput := fmt.Sprintf(`[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":%q}]`, description)
	logPath := installMockBDShowRecorder(t, showOutput)
	bd := NewIsolated(tmpDir)
	cleanupCalled := false
	err := bd.RetireAgentGeneration(
		"gt-gastown-polecat-nux",
		AgentFieldExpectations{Incarnation: agentStringPointer("generation-1")},
		AgentRetirementRecord{}, nil,
		func(AgentRetirementRecord, func(string) error) error {
			cleanupCalled = true
			return nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "unsupported retirement journal version") {
		t.Fatalf("error = %v", err)
	}
	if cleanupCalled {
		t.Fatal("cleanup ran for malformed resumed journal")
	}
	if log := readMockBDLog(t, logPath); strings.Contains(log, "update gt-gastown-polecat-nux") {
		t.Fatalf("malformed resume mutated agent: %q", log)
	}
}

func TestClaimAgentCompletionTakesOverStaleAttemptUnderProcessLease(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	description := "role_type: polecat\nrig: gastown\nagent_state: completing\nincarnation: generation-1\ncompletion_attempt: crashed-attempt"
	showOutput := fmt.Sprintf(`[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":%q}]`, description)
	logPath := installMockBDShowRecorder(t, showOutput)
	bd := NewIsolated(tmpDir)

	if err := bd.ClaimAgentCompletion("gt-gastown-polecat-nux", "generation-1", "recovery-attempt"); err != nil {
		t.Fatalf("ClaimAgentCompletion takeover: %v", err)
	}
	log := readMockBDLog(t, logPath)
	if !strings.Contains(log, "agent_state: completing") || !strings.Contains(log, "completion_attempt: recovery-attempt") {
		t.Fatalf("takeover receipt not persisted: %q", log)
	}
}

func TestOrdinaryAgentWritersRejectFrozenLifecycleStates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	for _, state := range []AgentState{AgentStateCompleting, AgentStateRetiring, AgentStateNuked} {
		t.Run(string(state), func(t *testing.T) {
			tmpDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0o755); err != nil {
				t.Fatal(err)
			}
			description := fmt.Sprintf("role_type: polecat\nrig: gastown\nagent_state: %s\nincarnation: generation-1\ncompletion_attempt: attempt-1", state)
			showOutput := fmt.Sprintf(`[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":%q}]`, description)
			logPath := installMockBDShowRecorder(t, showOutput)
			bd := NewIsolated(tmpDir)
			mode := "ralph"
			if err := bd.UpdateAgentDescriptionFields("gt-gastown-polecat-nux", AgentFieldUpdates{Mode: &mode}); !errors.Is(err, ErrAgentFieldsChanged) {
				t.Fatalf("description writer error = %v, want ErrAgentFieldsChanged", err)
			}
			if err := bd.UpdateAgentIfIncarnation("gt-gastown-polecat-nux", "generation-1", UpdateOptions{AddLabels: []string{"ordinary"}}); !errors.Is(err, ErrAgentFieldsChanged) {
				t.Fatalf("issue writer error = %v, want ErrAgentFieldsChanged", err)
			}
			if log := readMockBDLog(t, logPath); strings.Contains(log, "update gt-gastown-polecat-nux") {
				t.Fatalf("frozen generation was mutated: %q", log)
			}
		})
	}
}

func TestExactCompletionOwnerCanWriteCompletingGeneration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	description := "role_type: polecat\nrig: gastown\nagent_state: completing\nincarnation: generation-1\ncompletion_attempt: attempt-1"
	showOutput := fmt.Sprintf(`[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":%q}]`, description)
	logPath := installMockBDShowRecorder(t, showOutput)
	bd := NewIsolated(tmpDir)
	mode := "ralph"
	if err := bd.UpdateAgentDescriptionFieldsIfCompletionOwner(
		"gt-gastown-polecat-nux", "generation-1", "attempt-1", AgentFieldUpdates{Mode: &mode},
	); err != nil {
		t.Fatalf("completion owner update: %v", err)
	}
	if log := readMockBDLog(t, logPath); !strings.Contains(log, "mode: ralph") {
		t.Fatalf("completion owner write missing: %q", log)
	}
	if err := bd.UpdateAgentDescriptionFieldsIfCompletionOwner(
		"gt-gastown-polecat-nux", "generation-1", "wrong-attempt", AgentFieldUpdates{Mode: &mode},
	); !errors.Is(err, ErrAgentFieldsChanged) {
		t.Fatalf("wrong completion owner error = %v, want ErrAgentFieldsChanged", err)
	}
}

func TestCompletionOwnerActiveMRWriterPersistsAndRereads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	descriptionPath := filepath.Join(stateDir, "description")
	description := "role_type: polecat\nrig: gastown\nagent_state: completing\nincarnation: generation-1\ncompletion_attempt: attempt-1\nactive_mr: null\n"
	if err := os.WriteFile(descriptionPath, []byte(description), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	script := `#!/bin/sh
while [ "$1" = "--allow-stale" ]; do shift; done
case "$1" in
version) exit 0 ;;
show)
  escaped=$(awk 'BEGIN { printf "\"" } { gsub(/\\/, "\\\\"); gsub(/\"/, "\\\""); if (NR > 1) printf "\\n"; printf "%s", $0 } END { print "\"" }' "$GT_TEST_AGENT_DESCRIPTION")
  printf '[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":%s}]\n' "$escaped"
  ;;
update)
  for arg in "$@"; do
    if [ "$arg" = "--body-file=-" ]; then cat > "$GT_TEST_AGENT_DESCRIPTION"; fi
  done
  ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GT_TEST_AGENT_DESCRIPTION", descriptionPath)
	ResetBdAllowStaleCacheForTest()
	t.Cleanup(ResetBdAllowStaleCacheForTest)

	bd := NewIsolated(tmpDir)
	if err := bd.UpdateAgentActiveMRIfCompletionOwner("gt-gastown-polecat-nux", "generation-1", "attempt-1", "gt-mr-new"); err != nil {
		t.Fatalf("completion-owner ActiveMR write: %v", err)
	}
	issue, err := bd.Show("gt-gastown-polecat-nux")
	if err != nil {
		t.Fatal(err)
	}
	fields := ParseAgentFields(issue.Description)
	if fields.AgentState != string(AgentStateCompleting) || fields.CompletionAttempt != "attempt-1" || fields.ActiveMR != "gt-mr-new" {
		t.Fatalf("persisted completion owner fields = %+v", fields)
	}
}

func TestClaimAgentCompletionFencesBeforeExternalWork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755); err != nil {
		t.Fatal(err)
	}
	description := "role_type: polecat\nrig: gastown\nagent_state: working\nincarnation: generation-1"
	showOutput := fmt.Sprintf(`[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":%q}]`, description)
	logPath := installMockBDShowRecorder(t, showOutput)
	bd := NewIsolated(tmpDir)
	if err := bd.ClaimAgentCompletion("gt-gastown-polecat-nux", "generation-1", "attempt-1"); err != nil {
		t.Fatalf("ClaimAgentCompletion: %v", err)
	}
	if log := readMockBDLog(t, logPath); !strings.Contains(log, "agent_state: completing") {
		t.Fatalf("completion fence missing from update: %q", log)
	}
}

func TestOrdinaryAgentWriterRejectsRetiringGeneration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755); err != nil {
		t.Fatal(err)
	}
	description := "role_type: polecat\nrig: gastown\nagent_state: retiring\nincarnation: generation-1\nretirement_phase: local-removed"
	showOutput := fmt.Sprintf(`[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":%q}]`, description)
	logPath := installMockBDShowRecorder(t, showOutput)
	bd := NewIsolated(tmpDir)
	state := "working"
	if err := bd.UpdateAgentDescriptionFields("gt-gastown-polecat-nux", AgentFieldUpdates{AgentState: &state}); !errors.Is(err, ErrAgentFieldsChanged) {
		t.Fatalf("writer error = %v, want ErrAgentFieldsChanged", err)
	}
	if log := readMockBDLog(t, logPath); strings.Contains(log, "update gt-gastown-polecat-nux") {
		t.Fatalf("retiring generation was mutated: %q", log)
	}
}

func TestRetireAgentGenerationPersistsCursorBeforeFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755); err != nil {
		t.Fatal(err)
	}
	description := "role_type: polecat\nrig: gastown\nagent_state: working\nincarnation: generation-1\ncleanup_status: clean"
	showOutput := fmt.Sprintf(`[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":%q}]`, description)
	logPath := installMockBDShowRecorder(t, showOutput)
	bd := NewIsolated(tmpDir)
	fields := ParseAgentFields(description)
	injected := errors.New("cleanup interrupted")
	err := bd.RetireAgentGeneration(
		"gt-gastown-polecat-nux", fields.LifecycleExpectations(),
		AgentRetirementRecord{
			Version: AgentRetirementJournalVersion, HookBead: "gt-work",
			WorkReceipts: []AgentRetirementWorkReceipt{{WorkBead: "gt-work"}},
			ClonePath:    filepath.Clean(tmpDir), GitState: `{"WorktreePresent":true}`,
		}, nil,
		func(_ AgentRetirementRecord, advance func(string) error) error {
			if err := advance(AgentRetirementPhaseSessionStopped); err != nil {
				return err
			}
			if err := advance(AgentRetirementPhaseLocalRemoved); err != nil {
				return err
			}
			return injected
		},
	)
	if !errors.Is(err, injected) || !errors.Is(err, ErrAgentRetirementFenced) {
		t.Fatalf("retirement error = %v", err)
	}
	log := readMockBDLog(t, logPath)
	if !strings.Contains(log, "agent_state: retiring") || !strings.Contains(log, "retirement_phase: local-removed") || strings.Contains(log, "agent_state: nuked") {
		t.Fatalf("durable cursor log = %q", log)
	}
}

func TestResetAgentBeadForReuseRevalidationFailureHasZeroMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	description := "role_type: polecat\nrig: gastown\nagent_state: stuck\nincarnation: generation-1\nhook_bead: null\ncleanup_status: clean"
	showOutput := fmt.Sprintf(`[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":%q}]`, description)
	logPath := installMockBDShowRecorder(t, showOutput)
	bd := NewIsolated(tmpDir)
	expected := agentFieldsFromIssue(&Issue{Description: description}).LifecycleExpectations()
	driftErr := errors.New("work receipt changed")
	destructiveCalls := 0

	err := bd.ResetAgentBeadForReuseIfUnchangedRevalidatedAfter(
		"gt-gastown-polecat-nux",
		"polecat removed",
		expected,
		func(*Issue, *AgentFields) error { return driftErr },
		func() error { destructiveCalls++; return nil },
	)
	if !errors.Is(err, driftErr) {
		t.Fatalf("error = %v, want %v", err, driftErr)
	}
	if destructiveCalls != 0 {
		t.Fatalf("destructive callbacks = %d, want 0", destructiveCalls)
	}
	if log := readMockBDLog(t, logPath); strings.Contains(log, "update gt-gastown-polecat-nux") {
		t.Fatalf("revalidation failure mutated agent bead: %q", log)
	}
}

func TestResetAgentBeadForReuseResumesDurablyFencedRetirement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	description := "role_type: polecat\nrig: gastown\nagent_state: retiring\nincarnation: generation-1\nhook_bead: null\ncleanup_status: clean"
	showOutput := fmt.Sprintf(`[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":%q}]`, description)
	logPath := installMockBDShowRecorder(t, showOutput)
	bd := NewIsolated(tmpDir)
	expected := agentFieldsFromIssue(&Issue{Description: description}).LifecycleExpectations()

	if err := bd.ResetAgentBeadForReuseIfUnchangedRevalidatedAfter(
		"gt-gastown-polecat-nux", "polecat removed", expected, nil, nil,
	); err != nil {
		t.Fatalf("resume fenced retirement: %v", err)
	}
	logOutput := readMockBDLog(t, logPath)
	if got := strings.Count(logOutput, "update gt-gastown-polecat-nux"); got != 1 {
		t.Fatalf("updates while resuming fenced retirement = %d, want final reset only; log=%q", got, logOutput)
	}
	if !strings.Contains(logOutput, "agent_state: nuked") {
		t.Fatalf("resumed retirement did not reach nuked state: %q", logOutput)
	}
}

func TestIncarnationBoundWritersRejectRetiringGeneration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	description := "role_type: polecat\nrig: gastown\nagent_state: retiring\nincarnation: generation-1\nhook_bead: null\ncleanup_status: clean"
	showOutput := fmt.Sprintf(`[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":%q}]`, description)
	logPath := installMockBDShowRecorder(t, showOutput)
	bd := NewIsolated(tmpDir)

	if err := bd.UpdateAgentCompletionIfIncarnation("gt-gastown-polecat-nux", "generation-1", &CompletionMetadata{ExitType: "COMPLETED"}); !errors.Is(err, ErrAgentFieldsChanged) {
		t.Fatalf("completion error = %v, want ErrAgentFieldsChanged", err)
	}
	if err := bd.UpdateAgentIfIncarnation("gt-gastown-polecat-nux", "generation-1", UpdateOptions{AddLabels: []string{"done-cp:pushed"}}); !errors.Is(err, ErrAgentFieldsChanged) {
		t.Fatalf("label error = %v, want ErrAgentFieldsChanged", err)
	}
	if log := readMockBDLog(t, logPath); strings.Contains(log, "update gt-gastown-polecat-nux") {
		t.Fatalf("retiring generation accepted lifecycle write: %q", log)
	}
}

func TestLifecycleExpectationsRejectEveryResetFieldDrift(t *testing.T) {
	base := &AgentFields{
		AgentState: "stuck", Incarnation: "generation-1", CleanupStatus: "clean",
		ActiveMR: "mr-1", Mode: "ralph", HookBead: "gt-work", ExitType: "COMPLETED",
		MRID: "mr-1", Branch: "polecat/nux/work", LastSourceIssue: "gt-work",
		MRFailed: true, PushFailed: true, CompletionTime: "2026-08-28T12:00:00Z",
		structuredAgentState: "stuck", structuredHookBead: "gt-work",
	}
	tests := map[string]func(*AgentFields){
		"agent_state":            func(f *AgentFields) { f.AgentState = "done" },
		"incarnation":            func(f *AgentFields) { f.Incarnation = "generation-2" },
		"cleanup_status":         func(f *AgentFields) { f.CleanupStatus = "has_unpushed" },
		"active_mr":              func(f *AgentFields) { f.ActiveMR = "mr-2" },
		"mode":                   func(f *AgentFields) { f.Mode = "" },
		"hook_bead":              func(f *AgentFields) { f.HookBead = "gt-new" },
		"exit_type":              func(f *AgentFields) { f.ExitType = "DEFERRED" },
		"mr_id":                  func(f *AgentFields) { f.MRID = "mr-2" },
		"branch":                 func(f *AgentFields) { f.Branch = "polecat/nux/new" },
		"last_source_issue":      func(f *AgentFields) { f.LastSourceIssue = "gt-new" },
		"mr_failed":              func(f *AgentFields) { f.MRFailed = false },
		"push_failed":            func(f *AgentFields) { f.PushFailed = false },
		"completion_time":        func(f *AgentFields) { f.CompletionTime = "2026-08-28T12:01:00Z" },
		"structured_agent_state": func(f *AgentFields) { f.structuredAgentState = "done" },
		"structured_hook_bead":   func(f *AgentFields) { f.structuredHookBead = "gt-new" },
	}
	expected := base.LifecycleExpectations()
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			current := *base
			mutate(&current)
			if err := checkAgentFieldExpectations(&current, expected); !errors.Is(err, ErrAgentFieldsChanged) {
				t.Fatalf("error = %v, want ErrAgentFieldsChanged", err)
			}
		})
	}
}

func TestUpdateAgentCompletionIfIncarnationRejectsReusedAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	description := "role_type: polecat\nrig: gastown\nagent_state: working\nincarnation: generation-2\nhook_bead: gt-new"
	showOutput := fmt.Sprintf(`[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":%q}]`, description)
	logPath := installMockBDShowRecorder(t, showOutput)
	bd := NewIsolated(tmpDir)

	err := bd.UpdateAgentCompletionIfIncarnation("gt-gastown-polecat-nux", "generation-1", &CompletionMetadata{ExitType: "COMPLETED"})
	if !errors.Is(err, ErrAgentFieldsChanged) {
		t.Fatalf("error = %v, want ErrAgentFieldsChanged", err)
	}
	err = bd.UpdateAgentIfIncarnation("gt-gastown-polecat-nux", "generation-1", UpdateOptions{AddLabels: []string{"done-cp:pushed:main:1"}})
	if !errors.Is(err, ErrAgentFieldsChanged) {
		t.Fatalf("label writer error = %v, want ErrAgentFieldsChanged", err)
	}
	if logOutput := readMockBDLog(t, logPath); strings.Contains(logOutput, "update gt-gastown-polecat-nux") {
		t.Fatalf("stale generation updated replacement bead: %s", logOutput)
	}
}

func TestInitializeAgentIncarnationIfMissingWritesOpaqueGeneration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	logPath := installMockBDShowRecorder(t, `[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":"role_type: polecat\nrig: gastown\nagent_state: stuck\ncleanup_status: has_unpushed"}]`)
	bd := NewIsolated(tmpDir)

	incarnation, err := bd.InitializeAgentIncarnationIfMissing("gt-gastown-polecat-nux")
	if err != nil {
		t.Fatalf("InitializeAgentIncarnationIfMissing: %v", err)
	}
	if strings.TrimSpace(incarnation) == "" {
		t.Fatal("initialized incarnation is empty")
	}
	logOutput := readMockBDLog(t, logPath)
	if !strings.Contains(logOutput, "incarnation: "+incarnation) {
		t.Fatalf("mock bd log %q missing initialized incarnation", logOutput)
	}
}

func TestInitializeAgentIncarnationIfMissingPreservesExistingGeneration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	logPath := installMockBDShowRecorder(t, `[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":"role_type: polecat\nrig: gastown\nagent_state: stuck\nincarnation: existing-generation\ncleanup_status: has_unpushed"}]`)
	bd := NewIsolated(tmpDir)

	incarnation, err := bd.InitializeAgentIncarnationIfMissing("gt-gastown-polecat-nux")
	if err != nil {
		t.Fatalf("InitializeAgentIncarnationIfMissing: %v", err)
	}
	if incarnation != "existing-generation" {
		t.Fatalf("incarnation = %q, want existing-generation", incarnation)
	}
	if logOutput := readMockBDLog(t, logPath); strings.Contains(logOutput, "update gt-gastown-polecat-nux") {
		t.Fatalf("mock bd log %q unexpectedly rewrote immutable incarnation", logOutput)
	}
}

func stringPointer(value string) *string {
	return &value
}

func TestClearAgentActiveMRIfMatchesClearsExactMatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	logPath := installMockBDShowRecorder(t, `[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":"role_type: polecat\nrig: gastown\nagent_state: working\nactive_mr: gt-wisp-old\ncleanup_status: clean"}]`)
	bd := NewIsolated(tmpDir)

	cleared, err := bd.ClearAgentActiveMRIfMatches("gt-gastown-polecat-nux", "gt-wisp-old")
	if err != nil {
		t.Fatalf("ClearAgentActiveMRIfMatches: %v", err)
	}
	if !cleared {
		t.Fatal("ClearAgentActiveMRIfMatches cleared = false, want true")
	}

	logOutput := readMockBDLog(t, logPath)
	if !strings.Contains(logOutput, "show gt-gastown-polecat-nux --json") || !strings.Contains(logOutput, "update gt-gastown-polecat-nux") {
		t.Fatalf("mock bd log %q missing show/update", logOutput)
	}
}

func TestClearAgentActiveMRIfMatchesNoopsWhenDifferent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	logPath := installMockBDShowRecorder(t, `[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":"role_type: polecat\nrig: gastown\nagent_state: working\nactive_mr: gt-wisp-new"}]`)
	bd := NewIsolated(tmpDir)

	cleared, err := bd.ClearAgentActiveMRIfMatches("gt-gastown-polecat-nux", "gt-wisp-old")
	if err != nil {
		t.Fatalf("ClearAgentActiveMRIfMatches: %v", err)
	}
	if cleared {
		t.Fatal("ClearAgentActiveMRIfMatches cleared = true, want false")
	}

	logOutput := readMockBDLog(t, logPath)
	if strings.Contains(logOutput, "update gt-gastown-polecat-nux") {
		t.Fatalf("mock bd log %q unexpectedly updated mismatched active_mr", logOutput)
	}
}

func TestClearAgentActiveMRIfMatchesRejectsNonAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	logPath := installMockBDShowRecorder(t, `[{"id":"gt-task","title":"Task","issue_type":"task","labels":["gt:task"],"description":"active_mr: gt-wisp-old"}]`)
	bd := NewIsolated(tmpDir)

	cleared, err := bd.ClearAgentActiveMRIfMatches("gt-task", "gt-wisp-old")
	if err == nil {
		t.Fatal("ClearAgentActiveMRIfMatches expected non-agent error")
	}
	if cleared {
		t.Fatal("ClearAgentActiveMRIfMatches cleared = true, want false")
	}

	logOutput := readMockBDLog(t, logPath)
	if strings.Contains(logOutput, "update gt-task") {
		t.Fatalf("mock bd log %q unexpectedly updated non-agent", logOutput)
	}
}

func TestUpdateAgentState_UsesExplicitBeadsDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	workDir := t.TempDir()
	targetBeadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(targetBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir target .beads: %v", err)
	}

	installMockBDRequireExplicitBeadsDir(t, targetBeadsDir)

	bd := NewWithBeadsDir(workDir, targetBeadsDir)
	if err := bd.UpdateAgentState("gt-gastown-polecat-nux", "spawning"); err != nil {
		t.Fatalf("UpdateAgentState: %v", err)
	}
}

func TestIsAgentBeadByID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		// Full-form IDs (prefix != rig): prefix-rig-role[-name]
		{name: "full witness", id: "gt-gastown-witness", want: true},
		{name: "full refinery", id: "gt-gastown-refinery", want: true},
		{name: "full crew with name", id: "gt-gastown-crew-krystian", want: true},
		{name: "full polecat with name", id: "gt-gastown-polecat-Toast", want: true},
		{name: "full deacon", id: "sh-shippercrm-deacon", want: true},
		{name: "full mayor", id: "ax-axon-mayor", want: true},

		// Collapsed-form IDs (prefix == rig): prefix-role[-name]
		// These have only 2 parts for witness/refinery, must still be detected.
		{name: "collapsed witness", id: "bcc-witness", want: true},
		{name: "collapsed refinery", id: "bcc-refinery", want: true},
		{name: "collapsed crew with name", id: "bcc-crew-krystian", want: true},
		{name: "collapsed polecat with name", id: "bcc-polecat-obsidian", want: true},

		// Non-agent IDs
		{name: "regular issue", id: "gt-12345", want: false},
		{name: "task bead", id: "bcc-fix-button-color", want: false},
		{name: "single part", id: "witness", want: false},
		{name: "empty string", id: "", want: false},
		{name: "patrol molecule", id: "mol-patrol-abc123", want: false},
		{name: "merge request", id: "gt-mr-1234", want: false},

		// Edge cases
		{name: "role in first position", id: "witness-something", want: false},
		{name: "beads prefix collapsed", id: "bd-beads-witness", want: true},
		{name: "beads crew", id: "bd-beads-crew-krystian", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isAgentBeadByID(tt.id)
			if got != tt.want {
				t.Errorf("isAgentBeadByID(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

func TestMergeAgentBeadSources(t *testing.T) {
	t.Run("issues override duplicate wisp ids", func(t *testing.T) {
		issuesByID := map[string]*Issue{
			"hq-deacon": {ID: "hq-deacon", Type: "agent", Labels: []string{"gt:agent"}},
		}
		wispsByID := map[string]*Issue{
			"hq-deacon": {ID: "hq-deacon"},
		}

		merged := mergeAgentBeadSources(issuesByID, wispsByID)
		if len(merged) != 1 {
			t.Fatalf("len(merged) = %d, want 1", len(merged))
		}
		if merged["hq-deacon"].Type != "agent" {
			t.Fatalf("merged issue type = %q, want %q", merged["hq-deacon"].Type, "agent")
		}
		if len(merged["hq-deacon"].Labels) != 1 || merged["hq-deacon"].Labels[0] != "gt:agent" {
			t.Fatalf("merged labels = %v, want [gt:agent]", merged["hq-deacon"].Labels)
		}
	})

	t.Run("wisps are included when missing from issues", func(t *testing.T) {
		issuesByID := map[string]*Issue{
			"hq-mayor": {ID: "hq-mayor", Type: "agent", Labels: []string{"gt:agent"}},
		}
		wispsByID := map[string]*Issue{
			"bom-bti_ops_match-witness": {ID: "bom-bti_ops_match-witness"},
		}

		merged := mergeAgentBeadSources(issuesByID, wispsByID)
		if len(merged) != 2 {
			t.Fatalf("len(merged) = %d, want 2", len(merged))
		}
		if _, ok := merged["hq-mayor"]; !ok {
			t.Fatalf("expected hq-mayor in merged set")
		}
		if _, ok := merged["bom-bti_ops_match-witness"]; !ok {
			t.Fatalf("expected bom-bti_ops_match-witness in merged set")
		}
	})

	t.Run("handles nil maps", func(t *testing.T) {
		merged := mergeAgentBeadSources(nil, nil)
		if len(merged) != 0 {
			t.Fatalf("len(merged) = %d, want 0", len(merged))
		}
	})
}

func TestLabelsForAgentBeadReusePreservesOnlySafetyStop(t *testing.T) {
	got := labelsForAgentBeadReuse([]string{
		"gt:agent",
		"heartbeat:123",
		"idle:2",
		"done-intent:COMPLETED:123",
		"safety_stop:hq-vmrwr",
		"safety_stop:hq-vmrwr",
		"safety_stop:hq-other",
	})
	want := []string{"gt:agent", "safety_stop:hq-vmrwr", "safety_stop:hq-other"}
	if len(got) != len(want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("labels = %v, want %v", got, want)
		}
	}
}

func installMockBDCreateRecorder(t *testing.T, logPath string) {
	t.Helper()

	binDir := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Skip("cross-rig create recorder test not implemented on Windows")
	}

	script := `#!/bin/sh
printf 'pwd=%s\n' "$(pwd)" >> "$MOCK_BD_LOG"
printf 'beads_dir=%s\n' "$BEADS_DIR" >> "$MOCK_BD_LOG"
printf 'args=%s\n' "$*" >> "$MOCK_BD_LOG"

cmd=""
for arg in "$@"; do
  case "$arg" in
    --*) ;;
    *) cmd="$arg"; break ;;
  esac
done

case "$cmd" in
  create)
    printf '{"id":"pt-imported-polecat-shiny","title":"shiny","status":"open"}\n'
    exit 0
    ;;
  slot|config|migrate|init|show|update)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`
	scriptPath := filepath.Join(binDir, "bd")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write mock bd: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOCK_BD_LOG", logPath)
}

func TestCreateAgentBead_UsesTownRootForCrossRigRoutes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("path assertions are Unix-oriented")
	}

	// Resolve symlinks so path assertions match shell pwd output.
	// On macOS, t.TempDir() returns /var/... but pwd resolves to /private/var/...
	townRoot, _ := filepath.EvalSymlinks(t.TempDir())
	for _, dir := range []string{
		filepath.Join(townRoot, "mayor"),
		filepath.Join(townRoot, ".beads"),
		filepath.Join(townRoot, "imported", "mayor", "rig", ".beads"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"test"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte("{\"prefix\":\"pt-\",\"path\":\"imported/mayor/rig\"}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(townRoot, "bd.log")
	installMockBDCreateRecorder(t, logPath)

	workerDir := filepath.Join(townRoot, "imported", "mayor", "rig")
	bd := NewWithBeadsDir(workerDir, filepath.Join(workerDir, ".beads"))

	issue, err := bd.CreateAgentBead("pt-imported-polecat-shiny", "shiny", &AgentFields{
		RoleType:   "polecat",
		Rig:        "imported",
		AgentState: "spawning",
		HookBead:   "pt-task-1",
	})
	if err != nil {
		t.Fatalf("CreateAgentBead: %v", err)
	}
	if issue == nil {
		t.Fatal("CreateAgentBead returned nil issue")
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read mock bd log: %v", err)
	}
	logOutput := string(logData)
	if !strings.Contains(logOutput, "pwd="+townRoot) {
		t.Fatalf("mock bd log missing town root cwd:\n%s", logOutput)
	}
	if !strings.Contains(logOutput, "beads_dir="+filepath.Join(townRoot, ".beads")) {
		t.Fatalf("mock bd log missing town-root BEADS_DIR:\n%s", logOutput)
	}
	if !strings.Contains(logOutput, "create --json --id=pt-imported-polecat-shiny") {
		t.Fatalf("mock bd log missing create call:\n%s", logOutput)
	}
	// Note: hook_bead slot is no longer set — bd slot removed in v0.62 (hq-l6mm5).
	// Work bead status=hooked and assignee=<agent> is now the authoritative source.
}

func TestCreateAgentBead_ParsesMockCreateOutput(t *testing.T) {
	raw := []byte(`{"id":"pt-imported-polecat-shiny","title":"shiny","status":"open"}`)
	var issue Issue
	if err := json.Unmarshal(raw, &issue); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if issue.ID != "pt-imported-polecat-shiny" {
		t.Fatalf("issue.ID = %q", issue.ID)
	}
}

func TestCreateOrReopenAgentBeadExistingUsesTownBeadsDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}

	townRoot, _ := filepath.EvalSymlinks(t.TempDir())
	townBeadsDir := filepath.Join(townRoot, ".beads")
	rigDir := filepath.Join(townRoot, "gastown", "mayor", "rig")
	rigBeadsDir := filepath.Join(rigDir, ".beads")
	for _, dir := range []string{filepath.Join(townRoot, "mayor"), townBeadsDir, rigBeadsDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"test"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := WriteRoutes(townBeadsDir, []Route{{Prefix: "hq-", Path: "."}, {Prefix: "gt-", Path: "gastown/mayor/rig"}}); err != nil {
		t.Fatalf("write routes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townBeadsDir, ".gt-types-configured"), []byte(TypeConfigSentinelValue()+"\n"), 0644); err != nil {
		t.Fatalf("write types sentinel: %v", err)
	}

	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "bd.log")
	script := fmt.Sprintf(`#!/bin/sh
LOG=%q
EXPECTED=%q
printf 'beads_dir=%%s args=%%s\n' "${BEADS_DIR:-<unset>}" "$*" >> "$LOG"
cmd=""
for arg in "$@"; do
  case "$arg" in
    --*) ;;
    *) cmd="$arg"; break ;;
  esac
done
if [ "$cmd" != "version" ] && [ "${BEADS_DIR:-}" != "$EXPECTED" ]; then
  echo "wrong BEADS_DIR ${BEADS_DIR:-<unset>}" >&2
  exit 9
fi
case "$cmd" in
  version|update|reopen)
    exit 0
    ;;
  create)
    echo 'already exists' >&2
    exit 1
    ;;
  show)
    printf '%%s\n' '[{"id":"gt-gastown-polecat-rust","title":"old","issue_type":"task","labels":["gt:agent"],"status":"open","description":"role_type: polecat\nrig: gastown\nagent_state: idle\nhook_bead: old"}]'
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, logPath, townBeadsDir)
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatalf("write mock bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	bd := NewWithBeadsDir(rigDir, rigBeadsDir)
	if _, err := bd.CreateOrReopenAgentBead("gt-gastown-polecat-rust", "gt-gastown-polecat-rust", &AgentFields{
		RoleType:   "polecat",
		Rig:        "gastown",
		AgentState: "spawning",
	}); err != nil {
		t.Fatalf("CreateOrReopenAgentBead: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read mock log: %v", err)
	}
	logOutput := string(logBytes)
	if strings.Contains(logOutput, "beads_dir="+rigBeadsDir) {
		t.Fatalf("CreateOrReopenAgentBead used rig BEADS_DIR; log:\n%s", logOutput)
	}
	if !strings.Contains(logOutput, "beads_dir="+townBeadsDir) || !strings.Contains(logOutput, "args=show") || !strings.Contains(logOutput, "args=update") {
		t.Fatalf("CreateOrReopenAgentBead did not use town BEADS_DIR for existing bead path; log:\n%s", logOutput)
	}
}
