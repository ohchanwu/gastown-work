package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestHooksPathAllRigsCheck_AcceptsExistingAbsoluteHooksPath(t *testing.T) {
	townRoot := t.TempDir()
	clonePath := filepath.Join(townRoot, "rig", "refinery", "rig")
	hooksPath := filepath.Join(clonePath, ".beads", "hooks")
	for _, dir := range []string{filepath.Join(clonePath, ".githooks"), hooksPath} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for _, args := range [][]string{{"init", clonePath}, {"-C", clonePath, "config", "core.hooksPath", hooksPath}} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}

	result := NewHooksPathAllRigsCheck().Run(&CheckContext{TownRoot: townRoot})
	if result.Status != StatusOK {
		t.Fatalf("expected an existing absolute hooks path to pass, got %v: %s", result.Status, result.Message)
	}
}

func TestHooksPathAllRigsCheck_RejectsEmptyHooksPath(t *testing.T) {
	townRoot := t.TempDir()
	clonePath := filepath.Join(townRoot, "rig", "refinery", "rig")
	if err := os.MkdirAll(filepath.Join(clonePath, ".githooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", clonePath}, {"-C", clonePath, "config", "core.hooksPath", ""}} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}

	result := NewHooksPathAllRigsCheck().Run(&CheckContext{TownRoot: townRoot})
	if result.Status != StatusWarning {
		t.Fatalf("expected an empty hooks path to warn, got %v: %s", result.Status, result.Message)
	}
}
