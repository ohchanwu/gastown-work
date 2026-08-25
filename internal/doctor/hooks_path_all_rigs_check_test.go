package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestHooksPathChecksUseSameValidation(t *testing.T) {
	tests := []struct {
		name      string
		hooksPath string
		configure bool
		setup     func(t *testing.T, clonePath, hooksPath string)
		want      bool
	}{
		{name: "absolute path", hooksPath: "absolute", configure: true, setup: createHooksDirectory, want: true},
		{name: "relative path", hooksPath: filepath.Join("config", "hooks"), configure: true, setup: createHooksDirectory, want: true},
		{name: "trailing space in path", hooksPath: ".githooks ", configure: true, setup: createHooksDirectory, want: true},
		{name: "empty path", configure: true, want: false},
		{name: "missing config", want: false},
		{
			name:      "non-directory",
			hooksPath: "hooks-file",
			configure: true,
			setup: func(t *testing.T, clonePath, hooksPath string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(clonePath, hooksPath), []byte("not a directory"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: false,
		},
		{name: "invalid path without legacy githooks", hooksPath: "missing-hooks", configure: true, want: false},
		{
			name:      "missing pre-push",
			hooksPath: "hooks",
			configure: true,
			setup: func(t *testing.T, clonePath, hooksPath string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(clonePath, hooksPath), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: false,
		},
		{
			name:      "pre-push is not a regular file",
			hooksPath: "hooks",
			configure: true,
			setup: func(t *testing.T, clonePath, hooksPath string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(clonePath, hooksPath, "pre-push"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			townRoot := t.TempDir()
			clonePath := filepath.Join(townRoot, "rig", "refinery", "rig")
			initHooksTestRepo(t, clonePath)

			hooksPath := tt.hooksPath
			if hooksPath == "absolute" {
				hooksPath = filepath.Join(clonePath, "absolute-hooks")
			}
			if tt.setup != nil {
				tt.setup(t, clonePath, hooksPath)
			}
			if tt.configure {
				setHooksPath(t, clonePath, hooksPath)
			}

			if got := hooksPathConfigured(clonePath); got != tt.want {
				t.Errorf("hooksPathConfigured() = %v, want %v", got, tt.want)
			}

			wantStatus := StatusWarning
			if tt.want {
				wantStatus = StatusOK
			}
			ctx := &CheckContext{TownRoot: townRoot, RigName: "rig"}
			for name, result := range map[string]*CheckResult{
				"rig":    NewHooksPathConfiguredCheck().Run(ctx),
				"global": NewHooksPathAllRigsCheck().Run(ctx),
			} {
				if result.Status != wantStatus {
					t.Errorf("%s check status = %v, want %v: %s", name, result.Status, wantStatus, result.Message)
				}
			}
		})
	}
}

func initHooksTestRepo(t *testing.T, clonePath string) {
	t.Helper()
	if output, err := exec.Command("git", "init", clonePath).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
}

func setHooksPath(t *testing.T, clonePath, hooksPath string) {
	t.Helper()
	if output, err := exec.Command("git", "-C", clonePath, "config", "core.hooksPath", hooksPath).CombinedOutput(); err != nil {
		t.Fatalf("git config core.hooksPath: %v\n%s", err, output)
	}
}

func createHooksDirectory(t *testing.T, clonePath, hooksPath string) {
	t.Helper()
	if !filepath.IsAbs(hooksPath) {
		hooksPath = filepath.Join(clonePath, hooksPath)
	}
	if err := os.MkdirAll(hooksPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooksPath, "pre-push"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
