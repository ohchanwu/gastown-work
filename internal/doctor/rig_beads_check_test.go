package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestCompareRigIdentityFailsClosedOnLegacyAndDuplicateCandidates(t *testing.T) {
	expected := expectedRigIdentity{
		id:        "gs-rig-gastown",
		name:      "gastown",
		beadsPath: filepath.Join("town", "gastown", ".beads"),
	}
	candidate := func(id, beadsPath string) rigIdentityCandidate {
		return rigIdentityCandidate{
			issue: &beads.Issue{
				ID:          id,
				Title:       "gastown",
				Description: beads.FormatRigDescription("gastown", &beads.RigFields{Prefix: "gs"}),
			},
			beadsPath: beadsPath,
		}
	}

	t.Run("canonical candidate is reused", func(t *testing.T) {
		got, err := compareRigIdentity(expected, []rigIdentityCandidate{candidate(expected.id, expected.beadsPath)})
		if err != nil || got == nil || got.issue.ID != expected.id {
			t.Fatalf("got=%#v err=%v", got, err)
		}
	})

	t.Run("legacy candidate is preserved", func(t *testing.T) {
		_, err := compareRigIdentity(expected, []rigIdentityCandidate{candidate("gt-rig-gastown", expected.beadsPath)})
		if err == nil || !strings.Contains(err.Error(), "legacy or misplaced") {
			t.Fatalf("error = %v, want legacy collision", err)
		}
	})

	t.Run("same identity in two databases is ambiguous", func(t *testing.T) {
		_, err := compareRigIdentity(expected, []rigIdentityCandidate{
			candidate(expected.id, expected.beadsPath),
			candidate(expected.id, filepath.Join("town", ".beads")),
		})
		if err == nil || !strings.Contains(err.Error(), "multiple candidates") {
			t.Fatalf("error = %v, want multiple candidates", err)
		}
	})

	t.Run("canonical ID with contradictory title fails closed", func(t *testing.T) {
		contradictory := candidate(expected.id, expected.beadsPath)
		contradictory.issue.Title = "other-rig"
		_, err := compareRigIdentity(expected, []rigIdentityCandidate{contradictory})
		if err == nil || !strings.Contains(err.Error(), "contradictory metadata") {
			t.Fatalf("error = %v, want contradictory metadata", err)
		}
	})
}

func TestRigBeadsFixReopensClosedCanonicalIdentity(t *testing.T) {
	townRoot := t.TempDir()
	rigBeadsPath := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	for _, path := range []string{filepath.Join(townRoot, ".beads"), rigBeadsPath} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestRigsRegistry(t, townRoot, map[string]string{"gastown": "gs"})
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(`{"prefix":"gs-","path":"gastown/mayor/rig"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	closed := &beads.Issue{
		ID:          beads.RigBeadIDWithPrefix("gs", "gastown"),
		Title:       "gastown",
		Status:      "closed",
		Labels:      []string{"gt:rig"},
		Description: beads.FormatRigDescription("gastown", &beads.RigFields{Prefix: "gs"}),
	}
	open := *closed
	open.Status = "open"
	closedJSON, err := json.Marshal([]*beads.Issue{closed})
	if err != nil {
		t.Fatal(err)
	}
	openJSON, err := json.Marshal([]*beads.Issue{&open})
	if err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	updateLog := filepath.Join(t.TempDir(), "updates")
	if err := os.WriteFile(updateLog, nil, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_RIG_BEADS_DIR", rigBeadsPath)
	t.Setenv("TEST_RIG_CLOSED_JSON", string(closedJSON))
	t.Setenv("TEST_RIG_OPEN_JSON", string(openJSON))
	t.Setenv("TEST_RIG_REOPENED", filepath.Join(t.TempDir(), "reopened"))
	t.Setenv("TEST_RIG_UPDATE_LOG", updateLog)
	script := `#!/usr/bin/env bash
set -euo pipefail
case " $* " in
  *" version "*) printf 'bd test\n' ;;
  *" list "*)
    if [[ "${BEADS_DIR:-}" != "$TEST_RIG_BEADS_DIR" ]]; then
      printf '[]\n'
    elif [[ -f "$TEST_RIG_REOPENED" ]]; then
      printf '%s\n' "$TEST_RIG_OPEN_JSON"
    else
      printf '%s\n' "$TEST_RIG_CLOSED_JSON"
    fi
    ;;
  *" query "*) printf '[]\n' ;;
  *" update "*)
    printf '%s\n' "$*" >> "$TEST_RIG_UPDATE_LOG"
    [[ " $* " == *" gs-rig-gastown "* && " $* " == *" --status=open "* ]]
    : > "$TEST_RIG_REOPENED"
    printf '{}\n'
    ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	beads.ResetBdAllowStaleCacheForTest()
	t.Cleanup(beads.ResetBdAllowStaleCacheForTest)

	ctx := &CheckContext{TownRoot: townRoot, RigName: "gastown"}
	if err := NewRigBeadsCheck().Fix(ctx); err != nil {
		t.Fatal(err)
	}
	logOutput, err := os.ReadFile(updateLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logOutput), "update gs-rig-gastown --status=open") {
		t.Fatalf("update log = %q, want rig identity reopen", logOutput)
	}
	if result := NewRigBeadsCheck().Run(ctx); result.Status != StatusOK {
		t.Fatalf("result = %#v, want healthy reopened rig identity", result)
	}
}

func TestRigIdentityInventoryFindsUnlabelledLegacyCollision(t *testing.T) {
	workDir := t.TempDir()
	beadsPath := filepath.Join(workDir, ".beads")
	if err := os.MkdirAll(beadsPath, 0755); err != nil {
		t.Fatal(err)
	}
	legacy := &beads.Issue{
		ID:          "gt-rig-gastown",
		Title:       "gastown",
		Status:      "open",
		Description: beads.FormatRigDescription("gastown", &beads.RigFields{Prefix: "gt"}),
	}
	installIdentityInventoryBD(t, []*beads.Issue{legacy}, nil, nil, nil)

	candidates, err := loadRigIdentityCandidates([]string{workDir})
	if err != nil {
		t.Fatal(err)
	}
	_, err = compareRigIdentity(expectedRigIdentity{id: "gs-rig-gastown", name: "gastown", beadsPath: beadsPath}, candidates)
	if err == nil || !strings.Contains(err.Error(), "legacy or misplaced") {
		t.Fatalf("error = %v, want unlabelled legacy collision", err)
	}
}

func TestRigBeadsRunReportsCrossDatabaseAmbiguity(t *testing.T) {
	townRoot := t.TempDir()
	for _, path := range []string{
		filepath.Join(townRoot, ".beads"),
		filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads"),
	} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestRigsRegistry(t, townRoot, map[string]string{"gastown": "gs"})
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(`{"prefix":"gs-","path":"gastown/mayor/rig"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	canonical := &beads.Issue{
		ID:          beads.RigBeadIDWithPrefix("gs", "gastown"),
		Title:       "gastown",
		Status:      "open",
		Labels:      []string{"gt:rig"},
		Description: beads.FormatRigDescription("gastown", &beads.RigFields{Prefix: "gs"}),
	}
	installIdentityInventoryBD(t, []*beads.Issue{canonical}, []*beads.Issue{canonical}, nil, nil)

	result := NewRigBeadsCheck().Run(&CheckContext{TownRoot: townRoot})
	if result.Status != StatusError || !strings.Contains(strings.Join(result.Details, "\n"), "multiple candidates") {
		t.Fatalf("result = %#v, want cross-database ambiguity error", result)
	}
}
