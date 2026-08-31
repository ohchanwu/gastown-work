package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

func TestPolecatNukeBranchDeletionDoesNotPublishPreservedWork(t *testing.T) {
	repo := setupGitStateRemoteRepo(t)
	remote := filepath.Join(filepath.Dir(repo), "remote.git")
	marker := filepath.Join(t.TempDir(), "received-push")
	installReceiveMarker(t, remote, marker)

	runGitCmd(t, repo, "switch", "-c", "polecat/nitro")
	writeTestFile(t, filepath.Join(repo, "preserved.txt"), "preserved\n")
	runGitCmd(t, repo, "add", "preserved.txt")
	runGitCmd(t, repo, "commit", "-m", "preserved patch")
	patchCommit := gitOutput(t, repo, "rev-parse", "HEAD")
	runGitCmd(t, repo, "switch", "integration/test")
	runGitCmd(t, repo, "cherry-pick", patchCommit)
	runGitCmd(t, repo, "push", "origin", "integration/test")
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clear receive marker: %v", err)
	}
	runGitCmd(t, repo, "switch", "main")

	before := remoteRefSnapshot(t, remote)
	expectedOID := gitOutput(t, repo, "rev-parse", "polecat/nitro")
	if err := deletePreservedLocalPolecatBranch(git.NewGit(repo), "polecat/nitro", expectedOID, []string{"integration/test"}); err != nil {
		t.Fatalf("deletePreservedLocalPolecatBranch: %v", err)
	}
	after := remoteRefSnapshot(t, remote)
	if before != after {
		t.Fatalf("remote refs changed during local retirement\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("remote receive hook was invoked; stat error = %v", err)
	}
	if localBranchExists(repo, "polecat/nitro") {
		t.Fatal("preserved local branch still exists")
	}
}

func TestPolecatNukeBranchDeletionRefusesUnpreservedWork(t *testing.T) {
	repo := setupGitStateRemoteRepo(t)
	remote := filepath.Join(filepath.Dir(repo), "remote.git")
	runGitCmd(t, repo, "switch", "-c", "polecat/nitro")
	writeTestFile(t, filepath.Join(repo, "unique.txt"), "unique\n")
	runGitCmd(t, repo, "add", "unique.txt")
	runGitCmd(t, repo, "commit", "-m", "unique patch")
	runGitCmd(t, repo, "switch", "main")

	before := remoteRefSnapshot(t, remote)
	expectedOID := gitOutput(t, repo, "rev-parse", "polecat/nitro")
	err := deletePreservedLocalPolecatBranch(git.NewGit(repo), "polecat/nitro", expectedOID, []string{"integration/test"})
	if err == nil || !strings.Contains(err.Error(), "reconciliation") {
		t.Fatalf("error = %v, want reconciliation guidance", err)
	}
	if !localBranchExists(repo, "polecat/nitro") {
		t.Fatal("unpreserved local branch was deleted")
	}
	if after := remoteRefSnapshot(t, remote); before != after {
		t.Fatalf("remote refs changed on refusal\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestPolecatNukeBranchDeletionRefusesAmbiguousRemoteEvidence(t *testing.T) {
	repo := setupGitStateRemoteRepo(t)
	remote := filepath.Join(filepath.Dir(repo), "remote.git")
	runGitCmd(t, repo, "switch", "-c", "polecat/nitro")
	writeTestFile(t, filepath.Join(repo, "local-only.txt"), "local\n")
	runGitCmd(t, repo, "add", "local-only.txt")
	runGitCmd(t, repo, "commit", "-m", "local candidate")
	runGitCmd(t, repo, "switch", "main")

	publisher := filepath.Join(t.TempDir(), "publisher")
	runGitCmd(t, "", "clone", remote, publisher)
	runGitCmd(t, publisher, "config", "user.email", "test@example.com")
	runGitCmd(t, publisher, "config", "user.name", "Test User")
	runGitCmd(t, publisher, "switch", "-c", "polecat/nitro")
	writeTestFile(t, filepath.Join(publisher, "remote-only.txt"), "remote\n")
	runGitCmd(t, publisher, "add", "remote-only.txt")
	runGitCmd(t, publisher, "commit", "-m", "unfetched remote candidate")
	runGitCmd(t, publisher, "push", "origin", "polecat/nitro")

	before := remoteRefSnapshot(t, remote)
	expectedOID := gitOutput(t, repo, "rev-parse", "polecat/nitro")
	err := deletePreservedLocalPolecatBranch(git.NewGit(repo), "polecat/nitro", expectedOID, nil)
	if err == nil || !strings.Contains(err.Error(), "could not be verified") {
		t.Fatalf("error = %v, want ambiguous preservation refusal", err)
	}
	if !localBranchExists(repo, "polecat/nitro") {
		t.Fatal("branch with ambiguous remote evidence was deleted")
	}
	if after := remoteRefSnapshot(t, remote); before != after {
		t.Fatalf("remote refs changed on ambiguous refusal\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func installReceiveMarker(t *testing.T, remote, marker string) {
	t.Helper()
	hook := filepath.Join(remote, "hooks", "pre-receive")
	contents := "#!/bin/sh\n: > " + shellQuote(marker) + "\n"
	if err := os.WriteFile(hook, []byte(contents), 0755); err != nil {
		t.Fatalf("write receive hook: %v", err)
	}
}

func remoteRefSnapshot(t *testing.T, remote string) string {
	t.Helper()
	return gitOutput(t, "", "--git-dir", remote, "for-each-ref", "--format=%(refname) %(objectname)")
}

func localBranchExists(repo, branch string) bool {
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = repo
	return cmd.Run() == nil
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
