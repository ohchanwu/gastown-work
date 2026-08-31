package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssignmentEntryPointsRejectRetirementBeforeMutation(t *testing.T) {
	townRoot := t.TempDir()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mayorDir, "town.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	writeBDStub(t, binDir, `#!/bin/sh
printf '%s\n' "$*" >> "$BD_LOG"
case " $* " in
  *" show "*) printf '%s\n' '[{"id":"gt-work","title":"work","status":"open","priority":2,"issue_type":"task"}]' ;;
  *" list "*) printf '%s\n' '[]' ;;
esac
`, "@echo off\r\nexit /b 1\r\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_LOG", logPath)
	t.Setenv("GT_TOWN_ROOT", townRoot)
	t.Setenv("GT_ROOT", townRoot)
	t.Setenv("GT_ROLE", "mayor")
	t.Chdir(mayorDir)

	barrierErr := errors.New("retirement barrier")
	previousAssign := assignPolecatWorkIfCurrent
	previousTarget := resolveTargetAgentFn
	previousSelf := resolveSelfTargetFn
	previousDryRun := handoffDryRun
	previousHookForce := hookForce
	t.Cleanup(func() {
		assignPolecatWorkIfCurrent = previousAssign
		resolveTargetAgentFn = previousTarget
		resolveSelfTargetFn = previousSelf
		handoffDryRun = previousDryRun
		hookForce = previousHookForce
	})
	assignPolecatWorkIfCurrent = func(_, _, _ string, _ func() error) error { return barrierErr }
	resolveTargetAgentFn = func(string) (string, string, string, error) {
		return "gastown/polecats/toast", "", mayorDir, nil
	}
	resolveSelfTargetFn = func() (string, string, string, error) {
		return "gastown/polecats/toast", "", mayorDir, nil
	}
	handoffDryRun = false
	hookForce = true

	tests := []struct {
		name string
		run  func() error
	}{
		{"runHook replacement", func() error { return runHook(nil, []string{"gt-work", "gastown/polecats/toast"}) }},
		{"hookBeadForHandoff", func() error { return hookBeadForHandoff("gt-work") }},
		{"sendHandoffMail", func() error { _, err := sendHandoffMail("test", "body"); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(logPath, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			err := tt.run()
			if !errors.Is(err, barrierErr) {
				t.Fatalf("error = %v, want retirement barrier", err)
			}
			log, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, mutation := range []string{" update ", " create ", " close "} {
				if strings.Contains(" "+string(log), mutation) {
					t.Fatalf("recorded mutation %q before retirement rejection: %s", mutation, log)
				}
			}
		})
	}
}
