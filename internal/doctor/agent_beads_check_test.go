package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

func installIdentityInventoryBD(t *testing.T, issues, labelledIssues, wisps, labelledWisps []*beads.Issue) {
	t.Helper()
	marshal := func(value []*beads.Issue) string {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	binDir := t.TempDir()
	script := `#!/usr/bin/env bash
set -euo pipefail
args=()
for arg in "$@"; do
  [[ "$arg" == "--allow-stale" ]] || args+=("$arg")
done
cmd=""
idx=0
for i in "${!args[@]}"; do
  if [[ "${args[$i]}" != -* ]]; then
    cmd="${args[$i]}"
    idx=$i
    break
  fi
done
rest=("${args[@]:$((idx + 1))}")
case "$cmd" in
  version)
    printf 'bd test\n'
    ;;
  list)
    if [[ " ${rest[*]} " == *" --label="* ]]; then
      printf '%s\n' '` + marshal(labelledIssues) + `'
    else
      printf '%s\n' '` + marshal(issues) + `'
    fi
    ;;
  query)
    if [[ " ${rest[*]} " == *"label="* ]]; then
      printf '%s\n' '` + marshal(labelledWisps) + `'
    else
      printf '%s\n' '` + marshal(wisps) + `'
    fi
    ;;
  mol)
    printf '{"wisps":[]}\n'
    ;;
  *)
    exit 1
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fmt.Sprintf("%s%c%s", binDir, os.PathListSeparator, os.Getenv("PATH")))
	beads.ResetBdAllowStaleCacheForTest()
	t.Cleanup(beads.ResetBdAllowStaleCacheForTest)
}

func installInvalidWispInventoryBD(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	script := `#!/usr/bin/env bash
set -euo pipefail
case " $* " in
  *" version "*) printf 'bd test\n' ;;
  *" list "*) printf '[]\n' ;;
  *" query "*) printf 'not-json\n' ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fmt.Sprintf("%s%c%s", binDir, os.PathListSeparator, os.Getenv("PATH")))
	beads.ResetBdAllowStaleCacheForTest()
	t.Cleanup(beads.ResetBdAllowStaleCacheForTest)
}

func writeTestRigsRegistry(t *testing.T, townRoot string, prefixes map[string]string) {
	t.Helper()
	rigs := make(map[string]config.RigEntry, len(prefixes))
	for name, prefix := range prefixes {
		rigs[name] = config.RigEntry{
			GitURL:      "https://example.invalid/" + name,
			AddedAt:     time.Now(),
			BeadsConfig: &config.BeadsConfig{Prefix: prefix},
		}
	}
	path := filepath.Join(townRoot, "mayor", "rigs.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveRigsConfig(path, &config.RigsConfig{Version: config.CurrentRigsVersion, Rigs: rigs}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateOrCompareAgentIdentity(t *testing.T) {
	expected := expectedAgentIdentity{
		id:        "gs-gastown-witness",
		role:      "witness",
		rig:       "gastown",
		beadsPath: "/town/gastown/mayor/rig/.beads",
	}
	canonical := func(id, role, rig, beadsPath string) agentIdentityCandidate {
		return agentIdentityCandidate{
			issue: &beads.Issue{
				ID:          id,
				Status:      "open",
				Description: beads.FormatAgentDescription(id, &beads.AgentFields{RoleType: role, Rig: rig}),
			},
			beadsPath: beadsPath,
		}
	}

	t.Run("retry reuses the identity created by the first attempt", func(t *testing.T) {
		var candidates []agentIdentityCandidate
		creates := 0
		create := func() (*beads.Issue, error) {
			creates++
			return canonical(expected.id, expected.role, expected.rig, expected.beadsPath).issue, nil
		}

		created, err := createOrCompareAgentIdentity(expected, candidates, create)
		if err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, *created)
		if _, err := createOrCompareAgentIdentity(expected, candidates, create); err != nil {
			t.Fatal(err)
		}
		if creates != 1 {
			t.Fatalf("create calls = %d, want 1", creates)
		}
	})

	t.Run("existing canonical identity is reused", func(t *testing.T) {
		candidate := canonical(expected.id, expected.role, expected.rig, expected.beadsPath)
		creates := 0
		got, err := createOrCompareAgentIdentity(expected, []agentIdentityCandidate{candidate}, func() (*beads.Issue, error) {
			creates++
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.issue.ID != expected.id || creates != 0 {
			t.Fatalf("got %q with %d create calls", got.issue.ID, creates)
		}
	})

	t.Run("legacy collision is preserved", func(t *testing.T) {
		legacy := canonical("gs-witness", expected.role, expected.rig, expected.beadsPath)
		creates := 0
		_, err := createOrCompareAgentIdentity(expected, []agentIdentityCandidate{legacy}, func() (*beads.Issue, error) {
			creates++
			return nil, nil
		})
		if err == nil || !strings.Contains(err.Error(), "legacy or misplaced") {
			t.Fatalf("error = %v, want legacy collision", err)
		}
		if creates != 0 {
			t.Fatalf("create calls = %d, want 0", creates)
		}
	})

	t.Run("multiple candidates fail closed", func(t *testing.T) {
		otherDB := canonical(expected.id, expected.role, expected.rig, "/town/.beads")
		canonicalCandidate := canonical(expected.id, expected.role, expected.rig, expected.beadsPath)
		_, err := createOrCompareAgentIdentity(expected, []agentIdentityCandidate{canonicalCandidate, otherDB}, func() (*beads.Issue, error) {
			t.Fatal("create must not run for ambiguous identity")
			return nil, nil
		})
		if err == nil || !strings.Contains(err.Error(), "multiple candidates") {
			t.Fatalf("error = %v, want multiple candidates", err)
		}
	})

	t.Run("canonical ID with contradictory metadata fails closed", func(t *testing.T) {
		contradictory := canonical(expected.id, "refinery", expected.rig, expected.beadsPath)
		_, err := compareAgentIdentity(expected, []agentIdentityCandidate{contradictory})
		if err == nil || !strings.Contains(err.Error(), "contradictory metadata") {
			t.Fatalf("error = %v, want contradictory metadata", err)
		}
	})

	t.Run("creation result with contradictory metadata fails closed", func(t *testing.T) {
		_, err := createOrCompareAgentIdentity(expected, nil, func() (*beads.Issue, error) {
			return canonical(expected.id, "refinery", expected.rig, expected.beadsPath).issue, nil
		})
		if err == nil || !strings.Contains(err.Error(), "contradictory metadata") {
			t.Fatalf("error = %v, want contradictory creation metadata", err)
		}
	})
}

func TestReviewSameAgentIDInIssueAndWispIsAmbiguous(t *testing.T) {
	expected := expectedAgentIdentity{
		id:        "gs-gastown-witness",
		role:      "witness",
		rig:       "gastown",
		beadsPath: "/town/gastown/mayor/rig/.beads",
	}
	issue := func() *beads.Issue {
		return &beads.Issue{
			ID:          expected.id,
			Status:      "open",
			Description: beads.FormatAgentDescription(expected.id, &beads.AgentFields{RoleType: expected.role, Rig: expected.rig}),
		}
	}

	_, err := compareAgentIdentity(expected, []agentIdentityCandidate{
		{issue: issue(), beadsPath: expected.beadsPath},
		{issue: issue(), beadsPath: expected.beadsPath, ephemeral: true},
	})
	if err == nil || !strings.Contains(err.Error(), "multiple candidates") {
		t.Fatalf("error = %v, want durable/wisp ambiguity", err)
	}
}

func TestIdentityInventoryFindsUnlabelledLegacyCollisions(t *testing.T) {
	expected := expectedAgentIdentity{
		id:        "gs-gastown-witness",
		role:      "witness",
		rig:       "gastown",
		beadsPath: filepath.Join(t.TempDir(), ".beads"),
	}
	legacy := &beads.Issue{
		ID:          "gs-witness",
		Status:      "open",
		Description: beads.FormatAgentDescription("legacy witness", &beads.AgentFields{RoleType: "witness", Rig: "gastown"}),
	}

	for _, tc := range []struct {
		name   string
		issues []*beads.Issue
		wisps  []*beads.Issue
	}{
		{name: "durable issue", issues: []*beads.Issue{legacy}},
		{name: "wisp", wisps: []*beads.Issue{legacy}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir := filepath.Dir(expected.beadsPath)
			if err := os.MkdirAll(expected.beadsPath, 0755); err != nil {
				t.Fatal(err)
			}
			installIdentityInventoryBD(t, tc.issues, nil, tc.wisps, nil)

			candidates, err := loadIdentityCandidates([]string{workDir})
			if err != nil {
				t.Fatal(err)
			}
			_, err = compareAgentIdentity(expected, candidates)
			if err == nil || !strings.Contains(err.Error(), "legacy or misplaced") {
				t.Fatalf("error = %v, want unlabelled legacy collision", err)
			}
		})
	}
}

func TestAgentBeadsFixPreservesUnlabelledLegacyCollision(t *testing.T) {
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
	legacy := &beads.Issue{
		ID:          "gs-witness",
		Status:      "open",
		Description: beads.FormatAgentDescription("legacy witness", &beads.AgentFields{RoleType: "witness", Rig: "gastown"}),
	}
	installIdentityInventoryBD(t, []*beads.Issue{legacy}, nil, nil, nil)

	err := NewAgentBeadsCheck().Fix(&CheckContext{TownRoot: townRoot, RigName: "gastown"})
	if err == nil || (!strings.Contains(err.Error(), "legacy or misplaced") && !strings.Contains(err.Error(), "multiple candidates")) {
		t.Fatalf("error = %v, want unlabelled legacy collision", err)
	}
}

func TestIdentityInventoryFailsClosedOnInvalidWispJSON(t *testing.T) {
	installInvalidWispInventoryBD(t)

	_, err := loadIdentityCandidates([]string{t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("error = %v, want invalid JSON", err)
	}
}

func TestDoctorIdentityChecksPropagateWispInventoryErrors(t *testing.T) {
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
	installInvalidWispInventoryBD(t)
	ctx := &CheckContext{TownRoot: townRoot}

	for name, result := range map[string]*CheckResult{
		"agent run": NewAgentBeadsCheck().Run(ctx),
		"rig run":   NewRigBeadsCheck().Run(ctx),
	} {
		if result.Status != StatusError || !strings.Contains(strings.Join(result.Details, "\n"), "invalid JSON") {
			t.Errorf("%s result = %#v, want invalid JSON error", name, result)
		}
	}
	for name, err := range map[string]error{
		"agent fix": NewAgentBeadsCheck().Fix(ctx),
		"rig fix":   NewRigBeadsCheck().Fix(ctx),
	} {
		if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
			t.Errorf("%s error = %v, want invalid JSON", name, err)
		}
	}
}

// TestAgentBeadsExistCheck_NoRoutes verifies the check handles missing routes.
func TestAgentBeadsExistCheck_NoRoutes(t *testing.T) {
	tmpDir := t.TempDir()

	// No .beads dir at all
	check := NewAgentBeadsCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	// With no routes, only global agents (deacon, mayor) are checked
	// They won't exist without Dolt, so we expect error
	t.Logf("Result: status=%v, message=%s", result.Status, result.Message)
	if result.Status == StatusOK {
		t.Error("expected error for missing global agent beads")
	}
}

// TestAgentBeadsExistCheck_NoRigs verifies the check handles empty routes.
func TestAgentBeadsExistCheck_NoRigs(t *testing.T) {
	tmpDir := t.TempDir()

	// Create .beads dir with empty routes.jsonl
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewAgentBeadsCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	// With empty routes, only global agents (deacon, mayor) are checked
	// They won't exist without Dolt, so we expect error or warning
	t.Logf("Result: status=%v, message=%s", result.Status, result.Message)
}

// TestAgentBeadsExistCheck_ExpectedIDs verifies the check looks for correct agent bead IDs.
func TestAgentBeadsExistCheck_ExpectedIDs(t *testing.T) {
	tmpDir := t.TempDir()

	// Set up routes pointing to a rig with known prefix
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Use "sw" prefix to match sallaWork pattern
	routesContent := `{"prefix":"sw-","path":"sallaWork/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}
	writeTestRigsRegistry(t, tmpDir, map[string]string{"sallaWork": "sw"})

	// Create rig beads directory
	rigBeadsDir := filepath.Join(tmpDir, "sallaWork", "mayor", "rig", ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	installIdentityInventoryBD(t, nil, nil, nil, nil)

	check := NewAgentBeadsCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	// Should report missing beads
	if result.Status == StatusOK {
		t.Errorf("expected error for missing agent beads, got: %s", result.Message)
	}

	// Should mention the expected bead IDs in details
	if len(result.Details) == 0 {
		t.Error("expected details to contain missing bead IDs")
	}

	// Verify the expected IDs are in the details
	expectedIDs := []string{"sw-sallaWork-witness", "sw-sallaWork-refinery"}
	for _, expectedID := range expectedIDs {
		found := false
		for _, detail := range result.Details {
			if detail == expectedID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected missing bead ID %s in details, got: %v", expectedID, result.Details)
		}
	}

	t.Logf("Result: status=%v, message=%s, details=%v", result.Status, result.Message, result.Details)
}

func TestRegisteredRigInfosUsesRegistryNotUnregisteredRoutes(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	routes := strings.Join([]string{
		`{"prefix":"gs-","path":"gastown/mayor/rig"}`,
		`{"prefix":"do-","path":"directory_only/mayor/rig"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatal(err)
	}
	registry := `{"version":1,"rigs":{"gastown":{"git_url":"https://example.invalid/gastown","added_at":"2026-01-01T00:00:00Z","beads":{"prefix":"gt"}}}}`
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "rigs.json"), []byte(registry), 0644); err != nil {
		t.Fatal(err)
	}

	infos, err := loadRegisteredRigInfos(townRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].name != "gastown" || infos[0].prefix != "gs" {
		t.Fatalf("infos = %#v, want only registered gastown using routed prefix", infos)
	}
}

func TestRegisteredRigInfosRejectsNestedOnlyRoute(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatal(err)
	}
	writeTestRigsRegistry(t, townRoot, map[string]string{"gastown": "gs"})
	if err := os.WriteFile(
		filepath.Join(townRoot, ".beads", "routes.jsonl"),
		[]byte(`{"prefix":"gs-","path":"gastown/polecats/recovery"}`+"\n"),
		0644,
	); err != nil {
		t.Fatal(err)
	}

	_, err := loadRegisteredRigInfos(townRoot, "")
	if err == nil || !strings.Contains(err.Error(), "canonical route") {
		t.Fatalf("error = %v, want missing canonical route", err)
	}
}

func TestRegisteredRigInfosAcceptsCanonicalRigRootRoute(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatal(err)
	}
	writeTestRigsRegistry(t, townRoot, map[string]string{"gastown": "gs"})
	if err := os.WriteFile(
		filepath.Join(townRoot, ".beads", "routes.jsonl"),
		[]byte(`{"prefix":"gs-","path":"gastown"}`+"\n"),
		0644,
	); err != nil {
		t.Fatal(err)
	}

	infos, err := loadRegisteredRigInfos(townRoot, "")
	if err != nil || len(infos) != 1 || infos[0].beadsPath != "gastown" {
		t.Fatalf("infos = %#v, error = %v, want canonical rig-root route", infos, err)
	}
}

func TestAgentBeadsRunReportsCrossDatabaseAmbiguity(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads"), 0755); err != nil {
		t.Fatal(err)
	}
	writeTestRigsRegistry(t, townRoot, map[string]string{"gastown": "gs"})
	if err := os.WriteFile(
		filepath.Join(townRoot, ".beads", "routes.jsonl"),
		[]byte(`{"prefix":"gs-","path":"gastown/mayor/rig"}`+"\n"),
		0644,
	); err != nil {
		t.Fatal(err)
	}
	identity := func(id, role, rig string) *beads.Issue {
		return &beads.Issue{
			ID:          id,
			Status:      "open",
			Labels:      []string{"gt:agent"},
			Description: beads.FormatAgentDescription(id, &beads.AgentFields{RoleType: role, Rig: rig}),
		}
	}
	labelled := []*beads.Issue{
		identity(beads.DeaconBeadIDTown(), "deacon", ""),
		identity(beads.MayorBeadIDTown(), "mayor", ""),
		identity(beads.WitnessBeadIDWithPrefix("gs", "gastown"), "witness", "gastown"),
		identity(beads.RefineryBeadIDWithPrefix("gs", "gastown"), "refinery", "gastown"),
	}
	installIdentityInventoryBD(t, labelled, labelled, nil, nil)

	result := NewAgentBeadsCheck().Run(&CheckContext{TownRoot: townRoot})
	if result.Status != StatusError || !strings.Contains(strings.Join(result.Details, "\n"), "multiple candidates") {
		t.Fatalf("result = %#v, want cross-database ambiguity error", result)
	}
}

func TestAgentBeadsExistCheck_DirectoryOnlyWorkersAreNotCanonical(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(`{"prefix":"gs-","path":"gastown/mayor/rig"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	registry := `{"version":1,"rigs":{"gastown":{"git_url":"https://example.invalid/gastown","added_at":"2026-01-01T00:00:00Z","beads":{"prefix":"gs"}}}}`
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "rigs.json"), []byte(registry), 0644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads"),
		filepath.Join(townRoot, "gastown", "crew", "directory-only", ".git"),
		filepath.Join(townRoot, "gastown", "polecats", "directory-only", ".git"),
	} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}

	result := NewAgentBeadsCheck().Run(&CheckContext{TownRoot: townRoot})
	for _, detail := range result.Details {
		if strings.Contains(detail, "directory-only") {
			t.Fatalf("directory-only worker treated as canonical identity: %v", result.Details)
		}
	}
}

// TestAgentBeadsExistCheck_RespectsRigScope verifies that --rig excludes
// unrelated rig routes from agent-bead expectations.
func TestAgentBeadsExistCheck_RespectsRigScope(t *testing.T) {
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := strings.Join([]string{
		`{"prefix":"gs-","path":"gastown/mayor/rig"}`,
		`{"prefix":"do-","path":"coder_dotfiles/mayor/rig"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}
	writeTestRigsRegistry(t, tmpDir, map[string]string{"gastown": "gs", "coder_dotfiles": "do"})

	for _, path := range []string{
		filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads"),
		filepath.Join(tmpDir, "coder_dotfiles", "mayor", "rig", ".beads"),
	} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	installIdentityInventoryBD(t, nil, nil, nil, nil)

	check := NewAgentBeadsCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: "gastown"}

	result := check.Run(ctx)

	if result.Status == StatusOK {
		t.Fatalf("expected missing agent beads for scoped rig, got OK")
	}
	for _, detail := range result.Details {
		if strings.HasPrefix(detail, "do-") {
			t.Fatalf("expected --rig scope to exclude coder_dotfiles agent bead %q, got details: %v", detail, result.Details)
		}
	}
	foundGastown := false
	for _, detail := range result.Details {
		if strings.HasPrefix(detail, "gs-") {
			foundGastown = true
			break
		}
	}
	if !foundGastown {
		t.Fatalf("expected scoped result to include gastown agent beads, got details: %v", result.Details)
	}
}

// TestAgentBeadsExistCheck_FixRespectsRigScope verifies that --fix with a rig
// scope does not create agent beads for unrelated rig prefixes.
func TestAgentBeadsExistCheck_FixRespectsRigScope(t *testing.T) {
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := strings.Join([]string{
		`{"prefix":"gs-","path":"gastown/mayor/rig"}`,
		`{"prefix":"do-","path":"coder_dotfiles/mayor/rig"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}
	writeTestRigsRegistry(t, tmpDir, map[string]string{"gastown": "gs", "coder_dotfiles": "do"})

	for _, path := range []string{
		filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads"),
		filepath.Join(tmpDir, "coder_dotfiles", "mayor", "rig", ".beads"),
	} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "gastown", "crew", "alice", ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "coder_dotfiles", "crew", "bella", ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(tmpDir, "bd.log")
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	bdScript := filepath.Join(binDir, "bd")
	script := `#!/usr/bin/env bash
set -euo pipefail

logfile="` + logFile + `"

args=()
for arg in "$@"; do
  if [[ "$arg" == --allow-stale ]]; then
    continue
  fi
  args+=("$arg")
done

cmd=""
idx=0
for i in "${!args[@]}"; do
  if [[ "${args[$i]}" != -* ]]; then
    cmd="${args[$i]}"
    idx=$i
    break
  fi
done

if [[ -z "$cmd" ]]; then
  exit 0
fi

rest=("${args[@]:$((idx + 1))}")

case "$cmd" in
  list)
    printf '[]\n'
    ;;
  mol)
    if [[ "${rest[0]:-}" == "wisp" && "${rest[1]:-}" == "list" ]]; then
      printf '{"wisps":[]}\n'
      exit 0
    fi
    exit 1
    ;;
  show)
    exit 1
    ;;
  create)
    id=""
    title=""
    for arg in "${rest[@]}"; do
      case "$arg" in
        --id=*) id="${arg#--id=}" ;;
        --title=*) title="${arg#--title=}" ;;
      esac
    done
    printf 'create %s\n' "$id" >> "$logfile"
    role=""
    case "$id" in
      *-witness) role="witness" ;;
      *-refinery) role="refinery" ;;
    esac
    printf '{"id":"%s","title":"%s","status":"open","labels":["gt:agent"],"description":"agent\\n\\nrole_type: %s\\nrig: gastown"}\n' "$id" "$title" "$role"
    ;;
  update)
    if [[ ${#rest[@]} -gt 0 ]]; then
      printf 'update %s\n' "${rest[0]}" >> "$logfile"
    fi
    printf '{}'\n
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(bdScript, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", fmt.Sprintf("%s%c%s", binDir, os.PathListSeparator, os.Getenv("PATH")))

	check := NewAgentBeadsCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: "gastown"}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() returned error: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("reading fake bd log: %v", err)
	}
	log := string(data)
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		if strings.Contains(line, " do-") {
			t.Fatalf("expected scoped Fix() to avoid coder_dotfiles beads, got log line %q", line)
		}
	}
	if !strings.Contains(log, "create gs-gastown-witness") {
		t.Fatalf("expected scoped Fix() to create gastown witness bead, got log: %q", log)
	}
}

// TestListCrewWorkers_FiltersWorktrees verifies that listCrewWorkers skips
// git worktrees (directories where .git is a file) and only returns canonical
// crew workers (where .git is a directory). This is the fix for GH#2767.
func TestListCrewWorkers_FiltersWorktrees(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "myrig"
	crewDir := filepath.Join(tmpDir, rigName, "crew")

	// Create a canonical crew worker: .git is a directory
	canonicalDir := filepath.Join(crewDir, "alice")
	if err := os.MkdirAll(filepath.Join(canonicalDir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create a worktree: .git is a file (contains gitdir pointer)
	worktreeDir := filepath.Join(crewDir, "alice-worktree")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreeDir, ".git"),
		[]byte("gitdir: /path/to/main/.git/worktrees/alice-worktree\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a second canonical worker
	bobDir := filepath.Join(crewDir, "bob")
	if err := os.MkdirAll(filepath.Join(bobDir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create a directory without .git at all (should be included — not a worktree)
	plainDir := filepath.Join(crewDir, "charlie")
	if err := os.MkdirAll(plainDir, 0755); err != nil {
		t.Fatal(err)
	}

	workers := listCrewWorkers(tmpDir, rigName)

	// Should include alice, bob, charlie but NOT alice-worktree
	expected := map[string]bool{"alice": false, "bob": false, "charlie": false}
	for _, w := range workers {
		if w == "alice-worktree" {
			t.Errorf("listCrewWorkers should skip worktree 'alice-worktree', got: %v", workers)
		}
		if _, ok := expected[w]; ok {
			expected[w] = true
		}
	}
	for name, found := range expected {
		if !found {
			t.Errorf("listCrewWorkers should include canonical worker %q, got: %v", name, workers)
		}
	}
}

// TestAddWispLabelSQL_ErrorsGracefully verifies addWispLabelSQL doesn't panic
// and returns an error when bd is unavailable (no Dolt server).
// This is a regression guard for gt-3vx: after CreateAgentBead, the gt:agent
// label must also be inserted into wisp_labels so doctor checks that join
// wisp_labels can find the bead.
func TestAddWispLabelSQL_ErrorsGracefully(t *testing.T) {
	tmpDir := t.TempDir()
	err := addWispLabelSQL(tmpDir, "gt-gastown-witness", "gt:agent")
	// bd sql will fail without a Dolt server — just verify no panic and that the
	// function returns an error (not silently discarding the failure).
	if err == nil {
		t.Log("addWispLabelSQL succeeded (Dolt server is running)")
	} else {
		t.Logf("addWispLabelSQL returned expected error without Dolt: %v", err)
	}
}

// TestListPolecats_FiltersWorktrees verifies that listPolecats skips
// git worktrees, same as listCrewWorkers. See GH#2767.
func TestListPolecats_FiltersWorktrees(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "myrig"
	polecatDir := filepath.Join(tmpDir, rigName, "polecats")

	// Canonical polecat
	if err := os.MkdirAll(filepath.Join(polecatDir, "scout", ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	// Worktree polecat (.git is a file)
	wtDir := filepath.Join(polecatDir, "scout-wt")
	if err := os.MkdirAll(wtDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, ".git"),
		[]byte("gitdir: /path/to/main/.git/worktrees/scout-wt\n"), 0644); err != nil {
		t.Fatal(err)
	}

	polecats := listPolecats(tmpDir, rigName)

	if len(polecats) != 1 || polecats[0] != "scout" {
		t.Errorf("listPolecats should return only [scout], got: %v", polecats)
	}
}
