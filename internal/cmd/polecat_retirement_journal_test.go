package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
)

func TestVerifyRetirementGitRecordRejectsMalformedAndMismatchedReceipts(t *testing.T) {
	clonePath, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	valid := beads.AgentRetirementRecord{
		Version:   beads.AgentRetirementJournalVersion,
		Phase:     beads.AgentRetirementPhaseFenced,
		ClonePath: clonePath,
		GitState:  `{"WorktreePresent":false}`,
	}
	tests := []struct {
		name string
		edit func(*beads.AgentRetirementRecord)
		want string
	}{
		{name: "missing branchless GitState", edit: func(r *beads.AgentRetirementRecord) { r.GitState = "" }, want: "Git receipt"},
		{name: "malformed branchless GitState", edit: func(r *beads.AgentRetirementRecord) { r.GitState = "{" }, want: "Git receipt is malformed"},
		{name: "branchless snapshot names branch", edit: func(r *beads.AgentRetirementRecord) { r.GitState = `{"WorktreePresent":false,"Branch":"polecat/nux"}` }, want: "branch identity"},
		{name: "branchless snapshot names head", edit: func(r *beads.AgentRetirementRecord) { r.GitState = `{"WorktreePresent":false,"Head":"abc123"}` }, want: "branch identity"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := valid
			tt.edit(&record)
			err := verifyRetirementGitRecord(nil, record)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestVerifyRetirementGitRecordRejectsInvalidBranchAndHeadMismatch(t *testing.T) {
	repo := setupGitStateRemoteRepo(t)
	clonePath, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	head := gitOutput(t, repo, "rev-parse", "integration/test")
	base := beads.AgentRetirementRecord{
		Version:   beads.AgentRetirementJournalVersion,
		Phase:     beads.AgentRetirementPhaseFenced,
		ClonePath: clonePath,
		Branch:    "integration/test",
		GitHead:   head,
		GitState:  `{"WorktreePresent":true,"Branch":"integration/test","Head":"` + head + `"}`,
		Targets:   []string{"refs/remotes/origin/integration/test"},
	}
	r := &rig.Rig{Path: repo}

	invalidBranch := base
	invalidBranch.Branch = "refs/heads/integration/test"
	invalidBranch.GitState = `{"WorktreePresent":true,"Branch":"refs/heads/integration/test","Head":"` + head + `"}`
	if err := verifyRetirementGitRecord(r, invalidBranch); err == nil || !strings.Contains(err.Error(), "invalid branch") {
		t.Fatalf("invalid branch error = %v", err)
	}

	mismatchedHead := base
	mismatchedHead.GitState = `{"WorktreePresent":true,"Branch":"integration/test","Head":"different"}`
	if err := verifyRetirementGitRecord(r, mismatchedHead); err == nil || !strings.Contains(err.Error(), "head") {
		t.Fatalf("head mismatch error = %v", err)
	}
}

func TestVerifyRetirementGitRecordRejectsBranchlessWorktreeCustodyMismatch(t *testing.T) {
	repo := setupGitStateRemoteRepo(t)
	clonePath, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	record := beads.AgentRetirementRecord{
		Version: beads.AgentRetirementJournalVersion, Phase: beads.AgentRetirementPhaseSessionStopped,
		ClonePath: clonePath, GitState: `{"WorktreePresent":false}`,
	}
	if err := verifyRetirementGitRecord(&rig.Rig{Path: repo}, record); err == nil {
		t.Fatal("branchless destructive resume accepted mismatched worktree custody")
	}
}

func TestVerifyRetirementGitRecordAcceptsBranchlessLocalRemoved(t *testing.T) {
	repo := setupGitStateRemoteRepo(t)
	clonePath := filepath.Join(t.TempDir(), "removed-branchless-worktree")
	record := beads.AgentRetirementRecord{
		Version: beads.AgentRetirementJournalVersion, Phase: beads.AgentRetirementPhaseLocalRemoved,
		ClonePath: clonePath, GitState: `{"WorktreePresent":false}`,
	}
	if err := verifyRetirementGitRecord(&rig.Rig{Path: repo}, record); err != nil {
		t.Fatalf("branchless local-removed receipt rejected: %v", err)
	}
}

func TestVerifyRetirementGitRecordRequiresBranchAbsenceAfterDelete(t *testing.T) {
	repo := setupGitStateRemoteRepo(t)
	rigPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigPath, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repo, filepath.Join(rigPath, "mayor", "rig")); err != nil {
		t.Fatal(err)
	}
	head := gitOutput(t, repo, "rev-parse", "integration/test")
	record := beads.AgentRetirementRecord{
		Version: beads.AgentRetirementJournalVersion, Phase: beads.AgentRetirementPhaseBranchDeleted,
		ClonePath: filepath.Join(t.TempDir(), "removed-worktree"), Branch: "integration/test", GitHead: head,
		GitState: `{"WorktreePresent":true,"Branch":"integration/test","Head":"` + head + `"}`,
		Targets:  []string{"refs/remotes/origin/integration/test"},
	}
	if err := verifyRetirementGitRecord(&rig.Rig{Path: rigPath}, record); err == nil || !strings.Contains(err.Error(), "still exists") {
		t.Fatalf("recreated branch after delete error = %v", err)
	}

	runGitCmd(t, repo, "switch", "main")
	runGitCmd(t, repo, "branch", "-D", "integration/test")
	if err := verifyRetirementGitRecord(&rig.Rig{Path: rigPath}, record); err != nil {
		t.Fatalf("deleted branch checkpoint rejected: %v", err)
	}
	record.Phase = beads.AgentRetirementPhaseBranchVerified
	if err := verifyRetirementGitRecord(&rig.Rig{Path: rigPath}, record); err != nil {
		t.Fatalf("post-delete pre-checkpoint receipt rejected: %v", err)
	}
}

func TestNukePolecatResumeValidatesGitReceiptBeforeFilesystemRemoval(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a Unix shell script mock for bd")
	}
	rigPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	clonePath, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(clonePath, "must-survive")
	if err := os.WriteFile(sentinel, []byte("custody"), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	script := `#!/bin/sh
while [ "$1" = "--allow-stale" ]; do shift; done
case "$1" in
  version) printf 'bd mock\n' ;;
  show) printf '%s\n' "$MOCK_AGENT_JSON" ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	oldRemove := removePolecatJournaled
	t.Cleanup(func() { removePolecatJournaled = oldRemove })
	removePolecatJournaled = func(_ *polecat.Manager, _, _ string, _, _, _ bool, _ beads.AgentRetirementRecord,
		_ func(*polecat.Polecat) (*beads.AgentFields, error), _ func(beads.AgentRetirementRecord) error,
		_ func(*beads.AgentRetirementRecord, func(string) error) error,
	) error {
		return os.Remove(sentinel)
	}
	r := &rig.Rig{Name: "rig", Path: rigPath}
	mgr := polecat.NewManager(r, nil, nil)
	base := beads.AgentRetirementRecord{
		Version:   beads.AgentRetirementJournalVersion,
		Phase:     beads.AgentRetirementPhaseSessionStopped,
		ClonePath: clonePath,
		Branch:    "polecat/rictus/work",
		GitHead:   "expected-head",
		GitState:  `{"Branch":"polecat/rictus/work","Head":"expected-head"}`,
		Targets:   []string{"refs/remotes/origin/polecat/rictus/work"},
	}
	for _, tt := range []struct {
		name   string
		mutate func(*beads.AgentRetirementRecord)
	}{
		{name: "empty Git snapshot", mutate: func(record *beads.AgentRetirementRecord) { record.GitState = `{}` }},
		{name: "branch head mismatch", mutate: func(record *beads.AgentRetirementRecord) {
			record.GitState = `{"Branch":"polecat/rictus/work","Head":"different-head"}`
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			record := base
			tt.mutate(&record)
			description := strings.Join([]string{
				"role_type: polecat",
				"rig: rig",
				"agent_state: retiring",
				"incarnation: fixture-generation",
				"retirement_version: " + record.Version,
				"retirement_phase: " + record.Phase,
				"retirement_clone_path: " + record.ClonePath,
				"retirement_branch: " + record.Branch,
				"retirement_git_head: " + record.GitHead,
				"retirement_git_state: " + record.GitState,
				`retirement_targets: ["refs/remotes/origin/polecat/rictus/work"]`,
			}, "\n")
			payload, marshalErr := json.Marshal([]*beads.Issue{{
				ID: "gt-rig-polecat-toast", Title: "agent", Type: "agent", Labels: []string{"gt:agent"}, Description: description,
			}})
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			t.Setenv("MOCK_AGENT_JSON", string(payload))
			err := nukePolecatFullWithOptions("toast", "rig", mgr, r, nukePolecatOptions{Force: true})
			if err == nil || !strings.Contains(err.Error(), "validating resumed retirement Git receipt") {
				t.Fatalf("resumed retirement validation error = %v", err)
			}
			if _, statErr := os.Stat(sentinel); statErr != nil {
				t.Fatalf("filesystem changed before resumed record validation: %v", statErr)
			}
		})
	}
}

func TestCleanupRetirementWorkReceiptsVisitsEveryAuthoritativePairOnRetry(t *testing.T) {
	receipts := []beads.AgentRetirementWorkReceipt{
		{WorkBead: "gt-previous", Molecule: "gt-old-wisp"},
		{WorkBead: "gt-work", Molecule: "gt-current-wisp"},
	}
	var calls []string
	cleanup := func(work, molecule string, _ *rig.Rig) error {
		calls = append(calls, work+"/"+molecule)
		return nil
	}
	for range 2 {
		if err := cleanupRetirementWorkReceipts(receipts, nil, cleanup); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"gt-previous/gt-old-wisp", "gt-work/gt-current-wisp",
		"gt-previous/gt-old-wisp", "gt-work/gt-current-wisp",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("cleanup calls = %v, want %v", calls, want)
	}
}

func TestNukeCleanupRejectsReplacementBeforeClosingDescendants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a Unix shell script mock for bd")
	}
	rigPath := t.TempDir()
	workDir := filepath.Join(rigPath, "mayor", "rig")
	if err := os.MkdirAll(filepath.Join(workDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "bd.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$MOCK_BD_LOG"
while [ "$1" = "--allow-stale" ]; do shift; done
cmd="$1"; shift
case "$cmd" in
  version) exit 0 ;;
  show) printf '%s\n' '[{"id":"gt-work","title":"work","status":"open","assignee":"","description":"attached_molecule: gt-replacement"}]' ;;
  list) printf '%s\n' '[{"id":"gt-child","title":"child","status":"open"}]' ;;
  *) exit 0 ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOCK_BD_LOG", logPath)

	err := nukeCleanupMoleculeExact("gt-work", "gt-original", &rig.Rig{Path: rigPath})
	if err == nil || !strings.Contains(err.Error(), "molecule changed") {
		t.Fatalf("replacement detach error = %v", err)
	}
	log, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(log), "close ") || strings.Contains(string(log), "dep remove") || strings.Contains(string(log), "update ") {
		t.Fatalf("replacement molecule mutated before detach CAS: %s", log)
	}
}

func TestNukeCleanupRetriesAfterDetachCommittedBeforeLaterFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a Unix shell script mock for bd")
	}
	rigPath := t.TempDir()
	workDir := filepath.Join(rigPath, "mayor", "rig")
	if err := os.MkdirAll(filepath.Join(workDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "bd.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$MOCK_BD_LOG"
while [ "$1" = "--allow-stale" ]; do shift; done
cmd="$1"; shift
case "$cmd" in
  version) exit 0 ;;
  show)
    case "$1" in
      gt-work) printf '%s\n' '[{"id":"gt-work","title":"work","status":"pinned","assignee":"","description":"attached_molecule: gt-old-wisp"}]' ;;
      *) printf '%s\n' '[{"id":"gt-old-wisp","title":"molecule","status":"closed"}]' ;;
    esac
    ;;
  dep) exit 0 ;;
  close) exit 0 ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOCK_BD_LOG", logPath)

	oldDetach := detachAndCleanupMoleculesFn
	oldResume := resumeMoleculeCleanupIfPresentFn
	t.Cleanup(func() {
		detachAndCleanupMoleculesFn = oldDetach
		resumeMoleculeCleanupIfPresentFn = oldResume
	})
	detachCalls := 0
	detached := false
	detachAndCleanupMoleculesFn = func(*beads.Beads, string, *beadInfo, string, string, string, string, []string) (int, error) {
		detachCalls++
		detached = true
		return 0, errors.New("injected post-detach failure")
	}
	resumeMoleculeCleanupIfPresentFn = func(*beads.Beads, string, string, string) (bool, int, error) {
		return detached, 0, nil
	}
	r := &rig.Rig{Path: rigPath}
	if err := nukeCleanupMoleculeExact("gt-work", "gt-old-wisp", r); err == nil || !strings.Contains(err.Error(), "injected post-detach failure") {
		t.Fatalf("first cleanup error = %v", err)
	}
	if err := nukeCleanupMoleculeExact("gt-work", "gt-old-wisp", r); err != nil {
		t.Fatalf("durable cleanup retry: %v", err)
	}
	if detachCalls != 1 {
		t.Fatalf("detach calls = %d, want 1", detachCalls)
	}
}
