package cmd

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/testutil"
)

func TestBuildWitnessPatrolVars_NilContext(t *testing.T) {
	ctx := RoleContext{}
	vars := buildWitnessPatrolVars(ctx)
	if len(vars) != 0 {
		t.Errorf("expected empty vars for nil context, got %v", vars)
	}
}

func TestBuildWitnessPatrolVars_InjectsRigAndPrefix(t *testing.T) {
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildWitnessPatrolVars(ctx)
	if len(vars) != 2 {
		t.Fatalf("expected 2 vars (rig, prefix), got %v", vars)
	}
	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}
	if got := varMap["rig"]; got != "testrig" {
		t.Errorf("rig = %q, want %q", got, "testrig")
	}
	if got := varMap["prefix"]; got != "gt" {
		t.Errorf("prefix = %q, want %q (default fallback)", got, "gt")
	}
}

func TestBuildRefineryPatrolVars_NilContext(t *testing.T) {
	ctx := RoleContext{}
	vars := buildRefineryPatrolVars(ctx)
	if len(vars) != 0 {
		t.Errorf("expected empty vars for nil context, got %v", vars)
	}
}

func TestBuildRefineryPatrolVars_MissingSettings(t *testing.T) {
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	if err := os.MkdirAll(filepath.Join(rigDir, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)
	// rig and target_branch should always be present.
	if len(vars) != 2 {
		t.Errorf("expected 2 vars (rig, target_branch) when settings file missing, got %v", vars)
	}
	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}
	if got := varMap["rig"]; got != "testrig" {
		t.Errorf("rig = %q, want %q", got, "testrig")
	}
	if got := varMap["target_branch"]; got != "main" {
		t.Errorf("target_branch = %q, want %q", got, "main")
	}
}

func TestAutoSpawnPatrol_RefinerySafetyStoppedSkipsWispCreate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock bd script uses POSIX shell")
	}
	townRoot := setupRefinerySafetyStopTown(t)
	logPath := installRefinerySafetyStopMockBD(t)

	_, err := autoSpawnPatrol(PatrolConfig{
		RoleName:      "refinery",
		PatrolMolName: constants.MolRefineryPatrol,
		BeadsDir:      townRoot,
		Assignee:      "testrig/refinery",
	})
	if !errors.Is(err, refinery.ErrSafetyStopped) {
		t.Fatalf("autoSpawnPatrol error = %v, want ErrSafetyStopped", err)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}
	if strings.Contains(string(logData), "mol wisp create") || strings.Contains(string(logData), "update ") {
		t.Fatalf("autoSpawnPatrol mutated patrol state despite safety stop; log:\n%s", logData)
	}
}

func setupRefinerySafetyStopTown(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	for _, dir := range []string{filepath.Join(townRoot, "mayor"), filepath.Join(townRoot, ".beads"), filepath.Join(townRoot, "testrig")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"test"}`), 0o644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
	return townRoot
}

func installRefinerySafetyStopMockBD(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "bd.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
cmd=""
for arg in "$@"; do
  case "$arg" in
    --*) ;;
    *) cmd="$arg"; break ;;
  esac
done
case "$cmd" in
  version)
    echo "bd test"
    ;;
  show)
    printf '%s\n' '[{"id":"gt-testrig-refinery","title":"Refinery","issue_type":"task","labels":["gt:agent","safety_stop:hq-vmrwr"],"status":"open","description":"role_type: refinery\nrig: testrig\nagent_state: idle"}]'
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func TestBuildRefineryPatrolVars_NilMergeQueue(t *testing.T) {
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	settingsDir := filepath.Join(rigDir, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write settings with no merge_queue
	settings := config.RigSettings{
		Type:    "rig-settings",
		Version: 1,
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)
	// rig and target_branch should always be present.
	if len(vars) != 2 {
		t.Errorf("expected 2 vars (rig, target_branch) when merge_queue is nil, got %v", vars)
	}
	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}
	if got := varMap["rig"]; got != "testrig" {
		t.Errorf("rig = %q, want %q", got, "testrig")
	}
	if got := varMap["target_branch"]; got != "main" {
		t.Errorf("target_branch = %q, want %q", got, "main")
	}
}

func TestBuildRefineryPatrolVars_FullConfig(t *testing.T) {
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	settingsDir := filepath.Join(rigDir, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write rig config.json with default_branch (source of truth for default branch)
	rigConfig := map[string]interface{}{"type": "rig", "version": 1, "name": "testrig"}
	rigData, _ := json.Marshal(rigConfig)
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), rigData, 0o644); err != nil {
		t.Fatal(err)
	}

	mq := config.DefaultMergeQueueConfig()
	settings := config.RigSettings{
		Type:       "rig-settings",
		Version:    1,
		MergeQueue: mq,
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)

	// DefaultMergeQueueConfig: refinery_enabled=true, auto_land=false, run_tests=true,
	// test_command="" (language-agnostic), target_branch="main" (from rig config),
	// delete_merged_branches=true, judgment_enabled=false, review_depth="standard"
	// merge_strategy is omitted when not explicitly set (formula default "direct" applies)
	// New commands (setup, typecheck, lint, build) default to empty = omitted
	// judgment_enabled defaults to false, review_depth defaults to "standard"
	expected := map[string]string{
		"rig":                                 "testrig",
		"integration_branch_refinery_enabled": "true",
		"integration_branch_auto_land":        "false",
		"run_tests":                           "true",
		"target_branch":                       "main",
		"delete_merged_branches":              "true",
		"judgment_enabled":                    "false",
		"review_depth":                        "standard",
		"require_review":                      "false",
	}

	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}

	for key, want := range expected {
		got, ok := varMap[key]
		if !ok {
			t.Errorf("missing var %q", key)
			continue
		}
		if got != want {
			t.Errorf("var %q = %q, want %q", key, got, want)
		}
	}

	// Verify empty commands and unset strategy are NOT included
	for _, shouldBeAbsent := range []string{"setup_command", "typecheck_command", "lint_command", "build_command", "merge_strategy"} {
		if _, ok := varMap[shouldBeAbsent]; ok {
			t.Errorf("%q should be omitted when empty/unset", shouldBeAbsent)
		}
	}

	if len(vars) != len(expected) {
		t.Errorf("expected %d vars, got %d: %v", len(expected), len(vars), vars)
	}
}

func TestBuildRefineryPatrolVars_AllCommandsSet(t *testing.T) {
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	settingsDir := filepath.Join(rigDir, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	mq := config.DefaultMergeQueueConfig()
	mq.SetupCommand = "pnpm install"
	mq.TypecheckCommand = "tsc --noEmit"
	mq.LintCommand = "eslint ."
	mq.BuildCommand = "pnpm build"
	settings := config.RigSettings{
		Type:       "rig-settings",
		Version:    1,
		MergeQueue: mq,
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)

	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}

	// All configured commands should be present (test_command is empty by default)
	commandExpected := map[string]string{
		"setup_command":     "pnpm install",
		"typecheck_command": "tsc --noEmit",
		"lint_command":      "eslint .",
		"build_command":     "pnpm build",
	}
	for key, want := range commandExpected {
		got, ok := varMap[key]
		if !ok {
			t.Errorf("missing var %q", key)
			continue
		}
		if got != want {
			t.Errorf("var %q = %q, want %q", key, got, want)
		}
	}
}

func TestBuildRefineryPatrolVars_EmptyTestCommand(t *testing.T) {
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	settingsDir := filepath.Join(rigDir, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	falseVal := false
	trueVal2 := true
	mq := &config.MergeQueueConfig{
		Enabled:              true,
		RunTests:             &falseVal,
		TestCommand:          "", // empty - should be omitted
		DeleteMergedBranches: &trueVal2,
	}
	settings := config.RigSettings{
		Type:       "rig-settings",
		Version:    1,
		MergeQueue: mq,
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)

	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}

	// test_command should not be present when empty
	if _, ok := varMap["test_command"]; ok {
		t.Error("test_command should be omitted when empty")
	}

	// All command vars should be omitted when empty
	for _, cmd := range []string{"setup_command", "typecheck_command", "lint_command", "build_command"} {
		if _, ok := varMap[cmd]; ok {
			t.Errorf("%q should be omitted when empty", cmd)
		}
	}

	// run_tests should be "false"
	if got := varMap["run_tests"]; got != "false" {
		t.Errorf("run_tests = %q, want %q", got, "false")
	}
}

func TestBuildRefineryPatrolVars_BoolFormat(t *testing.T) {
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	settingsDir := filepath.Join(rigDir, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write rig config.json with default_branch = "develop"
	rigConfig := map[string]interface{}{"type": "rig", "version": 1, "name": "testrig", "default_branch": "develop"}
	rigData, _ := json.Marshal(rigConfig)
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), rigData, 0o644); err != nil {
		t.Fatal(err)
	}

	trueVal := true
	falseVal2 := false
	mq := &config.MergeQueueConfig{
		Enabled:                          true,
		IntegrationBranchAutoLand:        &trueVal,
		IntegrationBranchRefineryEnabled: &trueVal,
		RunTests:                         &trueVal,
		SetupCommand:                     "npm ci",
		TypecheckCommand:                 "tsc --noEmit",
		LintCommand:                      "eslint .",
		TestCommand:                      "make test",
		BuildCommand:                     "make build",
		DeleteMergedBranches:             &falseVal2,
	}
	settings := config.RigSettings{
		Type:       "rig-settings",
		Version:    1,
		MergeQueue: mq,
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)

	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}

	// Check bool format is "true"/"false" strings
	if got := varMap["integration_branch_auto_land"]; got != "true" {
		t.Errorf("integration_branch_auto_land = %q, want %q", got, "true")
	}
	if got := varMap["delete_merged_branches"]; got != "false" {
		t.Errorf("delete_merged_branches = %q, want %q", got, "false")
	}
	if got := varMap["target_branch"]; got != "develop" {
		t.Errorf("target_branch = %q, want %q", got, "develop")
	}
	if got := varMap["test_command"]; got != "make test" {
		t.Errorf("test_command = %q, want %q", got, "make test")
	}
	if got := varMap["setup_command"]; got != "npm ci" {
		t.Errorf("setup_command = %q, want %q", got, "npm ci")
	}
	if got := varMap["typecheck_command"]; got != "tsc --noEmit" {
		t.Errorf("typecheck_command = %q, want %q", got, "tsc --noEmit")
	}
	if got := varMap["lint_command"]; got != "eslint ." {
		t.Errorf("lint_command = %q, want %q", got, "eslint .")
	}
	if got := varMap["build_command"]; got != "make build" {
		t.Errorf("build_command = %q, want %q", got, "make build")
	}
}

func TestBuildRefineryPatrolVars_DefaultBranchWithoutMQ(t *testing.T) {
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write rig config with custom default_branch but NO settings/config.json
	rigConfig := map[string]interface{}{
		"type": "rig", "version": 1, "name": "testrig",
		"default_branch": "gastown",
	}
	rigData, _ := json.Marshal(rigConfig)
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), rigData, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)

	// rig and target_branch must be present even without merge_queue settings.
	if len(vars) != 2 {
		t.Errorf("expected 2 vars (rig, target_branch), got %d: %v", len(vars), vars)
	}
	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}
	if got := varMap["rig"]; got != "testrig" {
		t.Errorf("rig = %q, want %q", got, "testrig")
	}
	if got := varMap["target_branch"]; got != "gastown" {
		t.Errorf("target_branch = %q, want %q (should read rig config even without MQ settings)", got, "gastown")
	}
}

func TestBuildRefineryPatrolVars_MergeStrategy(t *testing.T) {
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	settingsDir := filepath.Join(rigDir, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	mq := config.DefaultMergeQueueConfig()
	mq.MergeStrategy = "pr"
	settings := config.RigSettings{
		Type:       "rig-settings",
		Version:    1,
		MergeQueue: mq,
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)

	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}

	if got := varMap["merge_strategy"]; got != "pr" {
		t.Errorf("merge_strategy = %q, want %q (rig-level config must override formula default)", got, "pr")
	}
}

func TestBuildRefineryPatrolVars_MergeStrategyDefaultOmitted(t *testing.T) {
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	settingsDir := filepath.Join(rigDir, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// MergeStrategy not set — should not be injected (formula default "direct" applies)
	mq := config.DefaultMergeQueueConfig()
	settings := config.RigSettings{
		Type:       "rig-settings",
		Version:    1,
		MergeQueue: mq,
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)

	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}

	// merge_strategy should be absent when not explicitly configured
	if _, ok := varMap["merge_strategy"]; ok {
		t.Error("merge_strategy should be omitted when not configured (let formula default apply)")
	}
}

func TestBuildRefineryPatrolVars_RequireReview(t *testing.T) {
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	settingsDir := filepath.Join(rigDir, "settings")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	mq := config.DefaultMergeQueueConfig()
	mq.MergeStrategy = "pr"
	requireReview := true
	mq.RequireReview = &requireReview
	settings := config.RigSettings{
		Type:       "rig-settings",
		Version:    1,
		MergeQueue: mq,
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildRefineryPatrolVars(ctx)

	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}

	if got := varMap["require_review"]; got != "true" {
		t.Errorf("require_review = %q, want %q", got, "true")
	}
	if got := varMap["merge_strategy"]; got != "pr" {
		t.Errorf("merge_strategy = %q, want %q", got, "pr")
	}
}

// splitFirstEquals splits a string on the first '=' only.
func splitFirstEquals(s string) []string {
	idx := -1
	for i, c := range s {
		if c == '=' {
			idx = i
			break
		}
	}
	if idx < 0 {
		return []string{s}
	}
	return []string{s[:idx], s[idx+1:]}
}

func TestPatrolRigName(t *testing.T) {
	if got := patrolRigName(PatrolConfig{Assignee: "gastown/refinery"}); got != "gastown" {
		t.Fatalf("patrolRigName = %q, want gastown", got)
	}
	if got := patrolRigName(PatrolConfig{Assignee: "deacon"}); got != "" {
		t.Fatalf("patrolRigName without rig = %q, want empty", got)
	}
}

func TestRenderPatrolWispDescription_DeaconInlinesStepsAndVars(t *testing.T) {
	desc, err := renderPatrolWispDescription(PatrolConfig{
		PatrolMolName: "mol-deacon-patrol",
		BeadsDir:      t.TempDir(),
		Assignee:      "deacon",
		ExtraVars:     []string{"idle_effort_threshold=7"},
	})
	if err != nil {
		t.Fatalf("renderPatrolWispDescription: %v", err)
	}
	for _, want := range []string{
		"Mayor's daemon patrol loop.",
		"**Formula Checklist**",
		"gt deacon heartbeat \"starting patrol cycle\"",
		"idle_cycles >= 7",
	} {
		if !strings.Contains(desc, want) {
			t.Fatalf("description missing %q:\n%s", want, desc)
		}
	}
}

func TestRenderPatrolWispDescription_AppliesOverlay(t *testing.T) {
	townRoot := t.TempDir()
	overlayDir := filepath.Join(townRoot, "formula-overlays")
	if err := os.MkdirAll(overlayDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overlayDir, "mol-deacon-patrol.toml"), []byte(`[[step-overrides]]
step_id = "heartbeat"
mode = "append"
description = "overlay heartbeat note"
`), 0644); err != nil {
		t.Fatal(err)
	}

	desc, err := renderPatrolWispDescription(PatrolConfig{
		PatrolMolName: "mol-deacon-patrol",
		BeadsDir:      townRoot,
		Assignee:      "deacon",
	})
	if err != nil {
		t.Fatalf("renderPatrolWispDescription: %v", err)
	}
	if !strings.Contains(desc, "overlay heartbeat note") {
		t.Fatalf("description did not include overlay text:\n%s", desc)
	}
}

func TestRenderPatrolWispDescription_RefinerySubstitutesRigAndEmptyDefaults(t *testing.T) {
	desc, err := renderPatrolWispDescription(PatrolConfig{
		PatrolMolName: "mol-refinery-patrol",
		BeadsDir:      t.TempDir(),
		Assignee:      "gastown/refinery",
	})
	if err != nil {
		t.Fatalf("renderPatrolWispDescription: %v", err)
	}
	if strings.Contains(desc, "{{") {
		t.Fatalf("description contains unresolved placeholder:\n%s", desc)
	}
	if !strings.Contains(desc, "gt agents resolve --role refinery --rig gastown") {
		t.Fatalf("description did not substitute refinery rig:\n%s", desc)
	}
}

func TestRenderPatrolWispDescription_ExtraVarsOverrideRoleVars(t *testing.T) {
	desc, err := renderPatrolWispDescription(PatrolConfig{
		PatrolMolName: constants.MolRefineryPatrol,
		BeadsDir:      t.TempDir(),
		Assignee:      "gastown/refinery",
		ExtraVars:     []string{"rig=override"},
	})
	if err != nil {
		t.Fatalf("renderPatrolWispDescription: %v", err)
	}
	if !strings.Contains(desc, "gt agents resolve --role refinery --rig override") {
		t.Fatalf("description did not use ExtraVars override:\n%s", desc)
	}
	if strings.Contains(desc, "gt agents resolve --role refinery --rig gastown") {
		t.Fatalf("description used role var instead of ExtraVars override:\n%s", desc)
	}
}

func TestUpdatePatrolWispDescriptionUsesBodyFileStdin(t *testing.T) {
	binDir := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "bd.log")
	writeBDStub(t, binDir, `#!/usr/bin/env sh
{
  printf 'args:%s\n' "$*"
  printf 'stdin:'
  cat
  printf '\n'
} >> "$BD_STUB_LOG"
`, `@echo off
echo args:%* >> %BD_STUB_LOG%
set /p stdin=
echo stdin:%stdin% >> %BD_STUB_LOG%
`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_STUB_LOG", logFile)

	err := updatePatrolWispDescription(PatrolConfig{BeadsDir: t.TempDir()}, filepath.Join(t.TempDir(), ".beads"), "gt-wisp-test", "line one\nline two")
	if err != nil {
		t.Fatalf("updatePatrolWispDescription: %v", err)
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, "args:update gt-wisp-test --body-file=-") {
		t.Fatalf("expected body-file update args, got:\n%s", log)
	}
	if strings.Contains(log, "--description=") {
		t.Fatalf("description must not be passed through argv:\n%s", log)
	}
	if !strings.Contains(log, "stdin:line one\nline two") {
		t.Fatalf("expected multiline description on stdin, got:\n%s", log)
	}
}

// --- Patrol discovery tests (findActivePatrol) ---

func TestCanonicalPatrolAssignee(t *testing.T) {
	tests := []struct {
		assignee string
		want     string
	}{
		{assignee: "deacon", want: "deacon/"},
		{assignee: "deacon/", want: "deacon/"},
		{assignee: "testrig/witness", want: "testrig/witness"},
		{assignee: "testrig/refinery", want: "testrig/refinery"},
	}
	for _, tt := range tests {
		t.Run(tt.assignee, func(t *testing.T) {
			got, err := canonicalPatrolAssignee(tt.assignee)
			if err != nil {
				t.Fatalf("canonicalPatrolAssignee(%q) error: %v", tt.assignee, err)
			}
			if got != tt.want {
				t.Fatalf("canonicalPatrolAssignee(%q) = %q, want %q", tt.assignee, got, tt.want)
			}
		})
	}
}

func stubPatrolDiscovery(t *testing.T, list func(*beads.Beads, string) ([]*beads.Issue, error), hasOpen func(*beads.Beads, string) (bool, error)) {
	t.Helper()
	oldList, oldHasOpen, oldCleanup := patrolAssignedWork, patrolHasOpenChildren, patrolCleanupStale
	patrolAssignedWork, patrolHasOpenChildren = list, hasOpen
	patrolCleanupStale = func(*beads.Beads, string) error { return nil }
	t.Cleanup(func() {
		patrolAssignedWork, patrolHasOpenChildren, patrolCleanupStale = oldList, oldHasOpen, oldCleanup
	})
}

func TestFindActivePatrolFailsClosedWhenChildDiscoveryIsIncomplete(t *testing.T) {
	stubPatrolDiscovery(t,
		func(*beads.Beads, string) ([]*beads.Issue, error) {
			return []*beads.Issue{{ID: "active", Title: "mol-test-patrol"}, {ID: "unknown", Title: "mol-test-patrol"}}, nil
		},
		func(_ *beads.Beads, id string) (bool, error) {
			if id == "unknown" {
				return false, errors.New("transient child list failure")
			}
			return true, nil
		},
	)

	patrolID, _, found, err := findActivePatrol(PatrolConfig{PatrolMolName: "mol-test-patrol", Assignee: "deacon"})
	if err == nil || !strings.Contains(err.Error(), "skipped due to child-listing errors") {
		t.Fatalf("findActivePatrol error = %v, want child-listing failure", err)
	}
	if found || patrolID != "" {
		t.Fatalf("findActivePatrol = (%q, %t), want fail-closed empty result", patrolID, found)
	}
}

func TestFindActivePatrolFailsClosedAtScanLimitWithActiveCandidate(t *testing.T) {
	candidates := make([]*beads.Issue, maxPatrolDiscoveryScans+1)
	for i := range candidates {
		candidates[i] = &beads.Issue{ID: fmt.Sprintf("patrol-%d", i), Title: "mol-test-patrol"}
	}
	stubPatrolDiscovery(t,
		func(*beads.Beads, string) ([]*beads.Issue, error) { return candidates, nil },
		func(_ *beads.Beads, id string) (bool, error) { return id == "patrol-0", nil },
	)

	patrolID, _, found, err := findActivePatrol(PatrolConfig{PatrolMolName: "mol-test-patrol", Assignee: "deacon"})
	wantErr := fmt.Sprintf("discovery incomplete: patrol scan limit reached after %d candidates", maxPatrolDiscoveryScans)
	if err == nil || err.Error() != wantErr {
		t.Fatalf("findActivePatrol error = %v, want %q", err, wantErr)
	}
	if found || patrolID != "" {
		t.Fatalf("findActivePatrol = (%q, %t), want fail-closed empty result", patrolID, found)
	}
}

func TestFindActivePatrolDoesNotMutateStaleRoots(t *testing.T) {
	stubPatrolDiscovery(t,
		func(*beads.Beads, string) ([]*beads.Issue, error) {
			return []*beads.Issue{{ID: "stale-root", Title: "mol-test-patrol"}}, nil
		},
		func(*beads.Beads, string) (bool, error) { return false, nil },
	)
	cleanupCalls := 0
	patrolCleanupStale = func(*beads.Beads, string) error { cleanupCalls++; return nil }

	patrolID, _, found, err := findActivePatrol(PatrolConfig{PatrolMolName: "mol-test-patrol", Assignee: "deacon"})
	if err != nil || found || patrolID != "" {
		t.Fatalf("findActivePatrol = (%q, %t, %v), want read-only no-active result", patrolID, found, err)
	}
	if cleanupCalls != 0 {
		t.Fatalf("findActivePatrol performed %d stale cleanup mutation(s)", cleanupCalls)
	}
}

func TestPersistPatrolReportWritesExactAuditAndPreservesRootOnFailure(t *testing.T) {
	oldUpdate := patrolReportUpdate
	t.Cleanup(func() { patrolReportUpdate = oldUpdate })
	var gotID, gotDescription string
	patrolReportUpdate = func(_ *beads.Beads, id string, opts beads.UpdateOptions) error {
		gotID = id
		gotDescription = *opts.Description
		return nil
	}
	if err := persistPatrolReport(nil, "patrol-root", "all clear", "Steps: inbox OK (1/1)"); err != nil {
		t.Fatalf("persistPatrolReport success error: %v", err)
	}
	if gotID != "patrol-root" || gotDescription != "Patrol report: all clear\n\nSteps: inbox OK (1/1)" {
		t.Fatalf("persisted (%q, %q), want exact root and audit", gotID, gotDescription)
	}
	patrolReportUpdate = func(*beads.Beads, string, beads.UpdateOptions) error { return errors.New("write failed") }
	if err := persistPatrolReport(nil, "patrol-root", "all clear", "Steps: inbox OK (1/1)"); err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("persistPatrolReport error = %v, want write failure", err)
	}
}

func TestValidatePatrolForCloseoutRejectsChangedFields(t *testing.T) {
	oldShow, oldHasOpen := patrolShow, patrolHasOpenChildren
	t.Cleanup(func() { patrolShow, patrolHasOpenChildren = oldShow, oldHasOpen })
	patrolHasOpenChildren = func(*beads.Beads, string) (bool, error) { return true, nil }
	valid := &beads.Issue{ID: "patrol-root", Title: "mol-test-patrol", Status: beads.StatusHooked, Assignee: "deacon/"}
	patrolShow = func(*beads.Beads, string) (*beads.Issue, error) { return valid, nil }
	if err := validatePatrolForCloseout(nil, PatrolConfig{PatrolMolName: "mol-test-patrol", Assignee: "deacon"}, "patrol-root"); err != nil {
		t.Fatalf("validatePatrolForCloseout valid root: %v", err)
	}
	for _, changed := range []*beads.Issue{
		{ID: "patrol-root", Title: "mol-test-patrol", Status: "closed", Assignee: "deacon/"},
		{ID: "patrol-root", Title: "mol-test-patrol", Status: beads.StatusHooked, Assignee: "deacon"},
		{ID: "patrol-root", Title: "mol-other-patrol", Status: beads.StatusHooked, Assignee: "deacon/"},
	} {
		patrolShow = func(*beads.Beads, string) (*beads.Issue, error) { return changed, nil }
		if err := validatePatrolForCloseout(nil, PatrolConfig{PatrolMolName: "mol-test-patrol", Assignee: "deacon"}, "patrol-root"); err == nil {
			t.Fatalf("validatePatrolForCloseout accepted changed root %#v", changed)
		}
	}
}

func TestVerifyPatrolSuccessorRequiresExactHookedCanonicalRoot(t *testing.T) {
	candidates := []*beads.Issue{{ID: "successor", Title: "mol-test-patrol"}}
	stubPatrolDiscovery(t,
		func(_ *beads.Beads, assignee string) ([]*beads.Issue, error) {
			if assignee != "deacon/" {
				t.Fatalf("discovery assignee = %q, want deacon/", assignee)
			}
			return candidates, nil
		},
		func(*beads.Beads, string) (bool, error) { return true, nil },
	)
	oldShow := patrolShow
	t.Cleanup(func() { patrolShow = oldShow })
	patrolShow = func(*beads.Beads, string) (*beads.Issue, error) {
		return &beads.Issue{ID: "successor", Title: "mol-test-patrol", Status: beads.StatusHooked, Assignee: "deacon/"}, nil
	}
	cfg := PatrolConfig{PatrolMolName: "mol-test-patrol", Assignee: "deacon"}
	if err := verifyPatrolSuccessor(cfg, "successor"); err != nil {
		t.Fatalf("verifyPatrolSuccessor success error: %v", err)
	}
	patrolShow = func(*beads.Beads, string) (*beads.Issue, error) {
		return &beads.Issue{ID: "successor", Title: "mol-test-patrol", Status: "in_progress", Assignee: "deacon/"}, nil
	}
	if err := verifyPatrolSuccessor(cfg, "successor"); err == nil {
		t.Fatal("verifyPatrolSuccessor accepted an unhooked successor")
	}
	patrolShow = func(*beads.Beads, string) (*beads.Issue, error) {
		return &beads.Issue{ID: "successor", Title: "mol-test-patrol", Status: beads.StatusHooked, Assignee: "deacon/"}, nil
	}
	candidates = append(candidates, &beads.Issue{ID: "duplicate", Title: "mol-test-patrol"})
	if err := verifyPatrolSuccessor(cfg, "successor"); err == nil {
		t.Fatal("verifyPatrolSuccessor accepted multiple canonical successors")
	}
	candidates = candidates[:1]
	patrolShow = func(*beads.Beads, string) (*beads.Issue, error) {
		return &beads.Issue{ID: "successor", Title: "mol-other-patrol", Status: beads.StatusHooked, Assignee: "deacon/"}, nil
	}
	if err := verifyPatrolSuccessor(cfg, "successor"); err == nil {
		t.Fatal("verifyPatrolSuccessor accepted a changed formula/title on reread")
	}
	patrolShow = func(*beads.Beads, string) (*beads.Issue, error) {
		return &beads.Issue{ID: "other-root", Title: "mol-test-patrol", Status: beads.StatusHooked, Assignee: "deacon/"}, nil
	}
	if err := verifyPatrolSuccessor(cfg, "successor"); err == nil {
		t.Fatal("verifyPatrolSuccessor accepted a different ID on authoritative reread")
	}
}

func TestFindActivePatrolUsesCanonicalAssigneeWithoutDolt(t *testing.T) {
	lookups := map[string]int{}
	byAssignee := map[string][]*beads.Issue{
		"deacon/": {{ID: "canonical", Title: "mol-test-patrol", Status: beads.StatusHooked, Assignee: "deacon/"}},
		"deacon":  {{ID: "bare-wrapper", Title: "mol-test-patrol", Status: "closed", Assignee: "deacon"}},
	}
	stubPatrolDiscovery(t,
		func(_ *beads.Beads, assignee string) ([]*beads.Issue, error) {
			lookups[assignee]++
			if assignee != "deacon/" {
				t.Fatalf("discovery assignee = %q, want canonical deacon/", assignee)
			}
			return byAssignee[assignee], nil
		},
		func(*beads.Beads, string) (bool, error) { return true, nil },
	)
	patrolID, _, found, err := findActivePatrol(PatrolConfig{PatrolMolName: "mol-test-patrol", Assignee: "deacon"})
	if err != nil || !found || patrolID != "canonical" {
		t.Fatalf("findActivePatrol = (%q, %t, %v), want canonical root", patrolID, found, err)
	}
	if lookups["deacon"] != 0 {
		t.Fatalf("bare wrapper lookup count = %d, want 0", lookups["deacon"])
	}
}

func TestAutoSpawnPatrolReturnsHookFailure(t *testing.T) {
	stubPatrolDiscovery(t,
		func(*beads.Beads, string) ([]*beads.Issue, error) { return nil, nil },
		func(*beads.Beads, string) (bool, error) { return false, nil },
	)
	binDir := t.TempDir()
	for name, script := range map[string]string{
		"gt": "#!/bin/sh\n[ \"$1 $2\" = \"formula list\" ] && echo 'proto mol-test-patrol'\n",
		"bd": "#!/bin/sh\ncase \"$*\" in *'--status=hooked'*) exit 1;; *'mol wisp create'*) echo 'Root issue: successor';; esac\n",
	} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	townRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	patrolID, err := autoSpawnPatrol(PatrolConfig{RoleName: "deacon", PatrolMolName: "mol-test-patrol", BeadsDir: townRoot, Assignee: "deacon"})
	if patrolID != "successor" || err == nil {
		t.Fatalf("autoSpawnPatrol = (%q, %v), want successor and hook failure", patrolID, err)
	}
}

func fakePatrolSpawnHarness(t *testing.T, failClose bool) (PatrolConfig, string) {
	t.Helper()
	binDir := t.TempDir()
	commandLog := filepath.Join(t.TempDir(), "bd.log")
	for name, script := range map[string]string{
		"gt": "#!/bin/sh\n[ \"$1 $2\" = \"formula list\" ] && echo 'proto mol-test-patrol'\n",
		"bd": "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$PATROL_BD_LOG\"\ncase \"$1\" in\nlist) echo '[]' ;;\nclose) [ \"$PATROL_FAIL_CLOSE\" = 1 ] && exit 1; exit 0 ;;\nmol) [ \"$2 $3\" = \"wisp create\" ] && echo 'Root issue: successor' ;;\nesac\n",
	} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PATROL_BD_LOG", commandLog)
	if failClose {
		t.Setenv("PATROL_FAIL_CLOSE", "1")
	}
	townRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	return PatrolConfig{RoleName: "deacon", PatrolMolName: "mol-test-patrol", BeadsDir: townRoot, Assignee: "deacon"}, commandLog
}

func fakePatrolReportHarness(t *testing.T) (string, string) {
	t.Helper()
	binDir := t.TempDir()
	commandLog := filepath.Join(t.TempDir(), "bd.log")
	for name, script := range map[string]string{
		"gt": "#!/bin/sh\n[ \"$1 $2\" = \"formula list\" ] && echo 'proto mol-deacon-patrol'\n",
		"bd": "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$PATROL_BD_LOG\"\ncase \"$*\" in\n*'mol wisp create'*) echo 'Root issue: successor' ;;\n*' list '*) echo '[]' ;;\n*' query '*) echo '[]' ;;\nesac\n",
	} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PATROL_BD_LOG", commandLog)
	townRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	return townRoot, commandLog
}

func patrolWispCreateCount(t *testing.T, commandLog string) int {
	t.Helper()
	contents, err := os.ReadFile(commandLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read fake bd log: %v", err)
	}
	count := 0
	for _, line := range strings.Split(string(contents), "\n") {
		if strings.HasPrefix(line, "mol wisp create ") {
			count++
		}
	}
	return count
}

func TestAutoSpawnPatrolStopsBeforeCreateWhenCanonicalListFails(t *testing.T) {
	stubPatrolDiscovery(t,
		func(*beads.Beads, string) ([]*beads.Issue, error) { return nil, errors.New("canonical list failed") },
		func(*beads.Beads, string) (bool, error) { return false, nil },
	)
	cfg, commandLog := fakePatrolSpawnHarness(t, false)
	if _, err := autoSpawnPatrol(cfg); err == nil || !strings.Contains(err.Error(), "canonical list failed") {
		t.Fatalf("autoSpawnPatrol error = %v, want canonical list failure", err)
	}
	if got := patrolWispCreateCount(t, commandLog); got != 0 {
		t.Fatalf("bd mol wisp create calls = %d, want 0 after canonical list failure", got)
	}
}

func TestAutoSpawnPatrolStopsBeforeCreateWhenPriorRootCloseFails(t *testing.T) {
	stubPatrolDiscovery(t,
		func(*beads.Beads, string) ([]*beads.Issue, error) {
			return []*beads.Issue{{ID: "prior-root", Title: "mol-test-patrol", Status: beads.StatusHooked}}, nil
		},
		func(*beads.Beads, string) (bool, error) { return false, nil },
	)
	cfg, commandLog := fakePatrolSpawnHarness(t, true)
	if _, err := autoSpawnPatrol(cfg); err == nil {
		t.Fatal("autoSpawnPatrol accepted a failed prior-root close")
	}
	if got := patrolWispCreateCount(t, commandLog); got != 0 {
		t.Fatalf("bd mol wisp create calls = %d, want 0 after prior-root close failure", got)
	}
}

func TestAutoSpawnPatrolRetryCreatesAndVerifiesOneCanonicalSuccessor(t *testing.T) {
	firstAttempt := true
	var commandLog string
	stubPatrolDiscovery(t,
		func(*beads.Beads, string) ([]*beads.Issue, error) {
			if firstAttempt {
				return nil, errors.New("canonical list failed")
			}
			if patrolWispCreateCount(t, commandLog) == 0 {
				return nil, nil
			}
			return []*beads.Issue{{ID: "successor", Title: "mol-test-patrol", Status: beads.StatusHooked, Assignee: "deacon/"}}, nil
		},
		func(*beads.Beads, string) (bool, error) { return true, nil },
	)
	oldShow := patrolShow
	t.Cleanup(func() { patrolShow = oldShow })
	patrolShow = func(*beads.Beads, string) (*beads.Issue, error) {
		return &beads.Issue{ID: "successor", Title: "mol-test-patrol", Status: beads.StatusHooked, Assignee: "deacon/"}, nil
	}
	cfg, commandLog := fakePatrolSpawnHarness(t, false)
	if _, err := autoSpawnPatrol(cfg); err == nil || !strings.Contains(err.Error(), "canonical list failed") {
		t.Fatalf("first autoSpawnPatrol error = %v, want canonical list failure", err)
	}
	if got := patrolWispCreateCount(t, commandLog); got != 0 {
		t.Fatalf("first attempt bd mol wisp create calls = %d, want 0", got)
	}
	firstAttempt = false
	patrolID, err := autoSpawnPatrol(cfg)
	if err != nil || patrolID != "successor" {
		t.Fatalf("retry autoSpawnPatrol = (%q, %v), want successor", patrolID, err)
	}
	if got := patrolWispCreateCount(t, commandLog); got != 1 {
		t.Fatalf("retry bd mol wisp create calls = %d, want exactly 1", got)
	}
	if err := verifyPatrolSuccessor(cfg, patrolID); err != nil {
		t.Fatalf("verifyPatrolSuccessor retry = %v", err)
	}
}

func TestAutoSpawnPatrolConcurrentAttemptsCreateOneHookedCanonicalSuccessor(t *testing.T) {
	oldList, oldOpen, oldShow := patrolAssignedWork, patrolHasOpenChildren, patrolShow
	t.Cleanup(func() { patrolAssignedWork, patrolHasOpenChildren, patrolShow = oldList, oldOpen, oldShow })
	cfg, commandLog := fakePatrolSpawnHarness(t, false)
	patrolAssignedWork = func(*beads.Beads, string) ([]*beads.Issue, error) {
		if patrolWispCreateCount(t, commandLog) == 0 {
			return nil, nil
		}
		return []*beads.Issue{{ID: "successor", Title: cfg.PatrolMolName, Status: beads.StatusHooked, Assignee: "deacon/"}}, nil
	}
	patrolHasOpenChildren = func(*beads.Beads, string) (bool, error) { return true, nil }
	patrolShow = func(*beads.Beads, string) (*beads.Issue, error) {
		return &beads.Issue{ID: "successor", Title: cfg.PatrolMolName, Status: beads.StatusHooked, Assignee: "deacon/"}, nil
	}
	start := make(chan struct{})
	type result struct {
		id  string
		err error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() { <-start; id, err := autoSpawnPatrol(cfg); results <- result{id, err} }()
	}
	close(start)
	for range 2 {
		var result result
		select {
		case result = <-results:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for concurrent patrol spawn result")
		}
		if result.err != nil {
			t.Fatalf("autoSpawnPatrol: %v", result.err)
		}
		if result.id != "successor" {
			t.Fatalf("id = %q", result.id)
		}
	}
	if got := patrolWispCreateCount(t, commandLog); got != 1 {
		t.Fatalf("bd mol wisp create calls = %d, want 1", got)
	}
	if err := verifyPatrolSuccessor(cfg, "successor"); err != nil {
		t.Fatal(err)
	}
}

func TestPatrolCustodyProcessHelper(t *testing.T) {
	mode := os.Getenv("GT_PATROL_CUSTODY_HELPER")
	if mode == "" {
		t.Skip("helper process")
	}
	root := os.Getenv("GT_PATROL_CUSTODY_ROOT")
	statePath := filepath.Join(root, "state")
	cfg := PatrolConfig{PatrolMolName: "mol-test-patrol", BeadsDir: root, Assignee: "deacon"}
	if mode == "report" {
		if err := withPatrolCustodyLock(cfg, func(PatrolConfig) error {
			if err := os.WriteFile(filepath.Join(root, "report-locked"), nil, 0o600); err != nil {
				return err
			}
			if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
				return err
			}
			return os.WriteFile(statePath, []byte("successor"), 0o600)
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	if mode == "lock-probe" {
		if err := withPatrolCustodyLock(cfg, func(PatrolConfig) error {
			return os.WriteFile(filepath.Join(root, "probe-locked"), nil, 0o600)
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	if mode == "report-transaction" {
		worker := os.Getenv("GT_PATROL_CUSTODY_WORKER")
		firstRead := true
		readState := func() (string, error) {
			state, err := os.ReadFile(statePath)
			return strings.TrimSpace(string(state)), err
		}
		patrolReportGetRole = func() (RoleInfo, error) {
			return RoleInfo{Role: RoleDeacon, TownRoot: root}, nil
		}
		patrolReportSummary, patrolReportSteps = "concurrent closeout", "heartbeat:OK"
		patrolAssignedWork = func(*beads.Beads, string) ([]*beads.Issue, error) {
			if firstRead {
				firstRead = false
				if err := os.WriteFile(filepath.Join(root, "snapshot-"+worker), nil, 0o600); err != nil {
					return nil, err
				}
				for {
					if _, err := os.Stat(filepath.Join(root, "release")); err == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				return []*beads.Issue{{ID: "original-root", Title: constants.MolDeaconPatrol, Status: beads.StatusHooked, Assignee: "deacon/"}}, nil
			}
			state, err := readState()
			if err != nil {
				return nil, err
			}
			switch state {
			case "original-root", "successor":
				return []*beads.Issue{{ID: state, Title: constants.MolDeaconPatrol, Status: beads.StatusHooked, Assignee: "deacon/"}}, nil
			default:
				return nil, nil
			}
		}
		patrolHasOpenChildren = func(*beads.Beads, string) (bool, error) { return true, nil }
		patrolShow = func(_ *beads.Beads, id string) (*beads.Issue, error) {
			state, err := readState()
			if err != nil {
				return nil, err
			}
			if id == "original-root" && state == "closed" {
				return &beads.Issue{ID: id, Title: constants.MolDeaconPatrol, Status: "closed", Assignee: "deacon/"}, nil
			}
			if id == state {
				return &beads.Issue{ID: id, Title: constants.MolDeaconPatrol, Status: beads.StatusHooked, Assignee: "deacon/"}, nil
			}
			return nil, errors.New("patrol not found")
		}
		appendEvent := func(event string) error {
			f, err := os.OpenFile(filepath.Join(root, "events"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = fmt.Fprintln(f, event)
			return err
		}
		patrolReportValidate = validatePatrolForCloseout
		patrolReportUpdate = func(*beads.Beads, string, beads.UpdateOptions) error { return appendEvent("report-write") }
		patrolReportCloseRoot = func(_ *beads.Beads, _ string, ids ...string) error {
			if err := appendEvent("root-close " + strings.Join(ids, ",")); err != nil {
				return err
			}
			return os.WriteFile(statePath, []byte("closed"), 0o600)
		}
		moleculeChildren = func(_ *beads.Beads, id string) ([]*beads.Issue, error) {
			switch id {
			case "original-root":
				return []*beads.Issue{{ID: "child", Status: "open"}}, nil
			case "child":
				return []*beads.Issue{{ID: "grandchild", Status: "open", Ephemeral: true}}, nil
			default:
				return nil, nil
			}
		}
		moleculeForceClose = func(_ *beads.Beads, ids ...string) error {
			return appendEvent("descendant-close " + strings.Join(ids, ","))
		}
		err := runPatrolReport(nil, nil)
		result := "ok"
		if err != nil {
			result = err.Error()
		}
		if err := os.WriteFile(filepath.Join(root, "result-"+worker), []byte(result), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if mode != "prime" {
		t.Fatalf("unknown helper mode %q", mode)
	}
	cfg.Assignee = "deacon/"
	patrolAssignedWork = func(*beads.Beads, string) ([]*beads.Issue, error) {
		state, err := os.ReadFile(statePath)
		if err != nil {
			return nil, err
		}
		id := strings.TrimSpace(string(state))
		return []*beads.Issue{{ID: id, Title: cfg.PatrolMolName, Status: beads.StatusHooked, Assignee: "deacon/"}}, nil
	}
	patrolHasOpenChildren = func(_ *beads.Beads, id string) (bool, error) { return id == "successor", nil }
	patrolCleanupStale = func(*beads.Beads, string) error {
		return os.WriteFile(filepath.Join(root, "unexpected-cleanup"), nil, 0o600)
	}
	if _, _, found, err := findActivePatrol(cfg); err != nil || found {
		t.Fatalf("read-only discovery = (found %t, %v), want no active root", found, err)
	}
	if err := os.WriteFile(filepath.Join(root, "prime-read-done"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := autoSpawnPatrol(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "prime-result"), []byte(id), 0o600); err != nil {
		t.Fatal(err)
	}
}

type patrolTestProcess struct {
	cmd      *exec.Cmd
	done     chan struct{}
	waitErr  error
	killOnce sync.Once
	output   bytes.Buffer
}

func startPatrolTestProcess(t *testing.T, cmd *exec.Cmd) (*patrolTestProcess, error) {
	t.Helper()
	process := &patrolTestProcess{cmd: cmd, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = &process.output, &process.output
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		if err := process.killAndReap(5 * time.Second); err != nil {
			t.Errorf("cleanup helper process: %v", err)
		}
	})
	go func() {
		process.waitErr = cmd.Wait()
		close(process.done)
	}()
	return process, nil
}

func (p *patrolTestProcess) killAndReap(timeout time.Duration) error {
	select {
	case <-p.done:
		return nil
	default:
	}
	p.killOnce.Do(func() { _ = p.cmd.Process.Kill() })
	select {
	case <-p.done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("pid %d was not reaped after kill", p.cmd.Process.Pid)
	}
}

func (p *patrolTestProcess) wait(timeout time.Duration) error {
	select {
	case <-p.done:
		if p.waitErr != nil {
			return fmt.Errorf("%w: %s", p.waitErr, strings.TrimSpace(p.output.String()))
		}
		return nil
	case <-time.After(timeout):
		if err := p.killAndReap(timeout); err != nil {
			return fmt.Errorf("helper timed out: %w", err)
		}
		return fmt.Errorf("helper timed out and was killed")
	}
}

func TestPatrolTestProcessTimeoutKillsAndReaps(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestPatrolCustodyProcessHelper$")
	cmd.Env = append(os.Environ(), "GT_PATROL_CUSTODY_HELPER=report", "GT_PATROL_CUSTODY_ROOT="+root)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	process, err := startPatrolTestProcess(t, cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "report-locked")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not acquire report lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := process.wait(100 * time.Millisecond); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("wait error = %v, want timeout", err)
	}
	if process.cmd.ProcessState == nil {
		t.Fatal("timed-out helper was not reaped")
	}
	probeCmd := exec.Command(os.Args[0], "-test.run=^TestPatrolCustodyProcessHelper$")
	probeCmd.Env = append(os.Environ(), "GT_PATROL_CUSTODY_HELPER=lock-probe", "GT_PATROL_CUSTODY_ROOT="+root)
	probe, err := startPatrolTestProcess(t, probeCmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.wait(5 * time.Second); err != nil {
		t.Fatalf("custody lock remained held after timeout cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "probe-locked")); err != nil {
		t.Fatalf("probe did not reacquire custody lock: %v", err)
	}
}

func TestPatrolPrimeReportInterleavingUsesCrossProcessCustody(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	helper := func(mode string) (*patrolTestProcess, io.WriteCloser, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPatrolCustodyProcessHelper$")
		cmd.Env = append(os.Environ(), "GT_PATROL_CUSTODY_HELPER="+mode, "GT_PATROL_CUSTODY_ROOT="+root)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, nil, err
		}
		process, err := startPatrolTestProcess(t, cmd)
		if err != nil {
			return nil, nil, err
		}
		return process, stdin, nil
	}
	waitFor := func(name string) error {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(root, name)); err == nil {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("timed out waiting for %s", name)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	report, reportStdin, err := helper("report")
	if err != nil {
		t.Fatal(err)
	}
	if err := waitFor("report-locked"); err != nil {
		_ = reportStdin.Close()
		_ = report.killAndReap(5 * time.Second)
		t.Fatal(err)
	}
	prime, primeStdin, err := helper("prime")
	if err != nil {
		_ = reportStdin.Close()
		_ = report.killAndReap(5 * time.Second)
		t.Fatal(err)
	}
	var failures []string
	if err := primeStdin.Close(); err != nil {
		failures = append(failures, err.Error())
	}
	if err := waitFor("prime-read-done"); err != nil {
		failures = append(failures, err.Error())
	}
	if _, err := os.Stat(filepath.Join(root, "unexpected-cleanup")); !os.IsNotExist(err) {
		failures = append(failures, "prime discovery mutated stale custody outside the report flock")
	}
	if _, err := os.Stat(filepath.Join(root, "prime-result")); !os.IsNotExist(err) {
		failures = append(failures, "canonical alias bypassed the report flock")
	}
	if err := reportStdin.Close(); err != nil {
		failures = append(failures, err.Error())
	}
	if err := report.wait(5 * time.Second); err != nil {
		failures = append(failures, "report helper: "+err.Error())
	}
	if err := prime.wait(5 * time.Second); err != nil {
		failures = append(failures, "prime helper: "+err.Error())
	}
	result, err := os.ReadFile(filepath.Join(root, "prime-result"))
	if err != nil || string(result) != "successor" {
		failures = append(failures, fmt.Sprintf("prime loser result = %q, %v; want existing successor", result, err))
	}
	if _, err := os.Stat(filepath.Join(root, "unexpected-cleanup")); !os.IsNotExist(err) {
		failures = append(failures, "helper performed stale cleanup outside the custody transaction")
	}
	if len(failures) > 0 {
		t.Fatal(strings.Join(failures, "; "))
	}
}

func TestRunPatrolReportConcurrentTransactionsCreateOneHookedCanonicalSuccessor(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state"), []byte("original-root"), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	commandLog := filepath.Join(root, "bd.log")
	for name, script := range map[string]string{
		"gt": "#!/bin/sh\n[ \"$1 $2\" = \"formula list\" ] && echo 'proto mol-deacon-patrol'\n",
		"bd": "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$PATROL_BD_LOG\"\ncase \"$*\" in\n*'mol wisp create'*) echo 'Root issue: successor' ;;\n'update successor --status=hooked --assignee=deacon/') printf successor > \"$PATROL_STATE\" ;;\n*' list '*) echo '[]' ;;\n*' query '*) echo '[]' ;;\nesac\n",
	} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	start := func(worker string) (*patrolTestProcess, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPatrolCustodyProcessHelper$")
		cmd.Env = append(os.Environ(),
			"GT_PATROL_CUSTODY_HELPER=report-transaction",
			"GT_PATROL_CUSTODY_ROOT="+root,
			"GT_PATROL_CUSTODY_WORKER="+worker,
			"PATROL_BD_LOG="+commandLog,
			"PATROL_STATE="+filepath.Join(root, "state"),
			"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		)
		return startPatrolTestProcess(t, cmd)
	}
	processes := make([]*patrolTestProcess, 0, 2)
	var failures []string
	for _, worker := range []string{"one", "two"} {
		process, err := start(worker)
		if err != nil {
			failures = append(failures, worker+" start: "+err.Error())
			break
		}
		processes = append(processes, process)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(failures) == 0 {
		ready := true
		for _, worker := range []string{"one", "two"} {
			if _, err := os.Stat(filepath.Join(root, "snapshot-"+worker)); err != nil {
				ready = false
			}
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			failures = append(failures, "two report commands did not reach the shared-root snapshot barrier")
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(root, "release"), nil, 0o600); err != nil {
		failures = append(failures, "release snapshots: "+err.Error())
	}
	for i, process := range processes {
		if err := process.wait(5 * time.Second); err != nil {
			failures = append(failures, fmt.Sprintf("worker %d: %v", i+1, err))
		}
	}

	var succeeded int
	var rejected string
	for _, worker := range []string{"one", "two"} {
		result, err := os.ReadFile(filepath.Join(root, "result-"+worker))
		if err != nil {
			failures = append(failures, worker+" result: "+err.Error())
			continue
		}
		if string(result) == "ok" {
			succeeded++
		} else {
			rejected = string(result)
		}
	}
	if succeeded != 1 || !strings.Contains(rejected, "patrol custody changed from expected root original-root") {
		failures = append(failures, fmt.Sprintf("report results = %d success, rejected %q; want one success and changed-root rejection", succeeded, rejected))
	}
	events, err := os.ReadFile(filepath.Join(root, "events"))
	if err != nil {
		failures = append(failures, "read events: "+err.Error())
	}
	if strings.Count(string(events), "report-write\n") != 1 ||
		strings.Count(string(events), "descendant-close grandchild\n") != 1 ||
		strings.Count(string(events), "descendant-close child\n") != 1 ||
		strings.Count(string(events), "root-close original-root\n") != 1 {
		failures = append(failures, fmt.Sprintf("transaction events = %q; want one report, descendant close, and root close", events))
	}
	commands, err := os.ReadFile(commandLog)
	if err != nil {
		failures = append(failures, "read command log: "+err.Error())
	}
	if strings.Count(string(commands), "mol wisp create ") != 1 ||
		strings.Count(string(commands), "update successor --status=hooked --assignee=deacon/") != 1 {
		failures = append(failures, fmt.Sprintf("command log = %q; want one create and one hook", commands))
	}
	state, err := os.ReadFile(filepath.Join(root, "state"))
	if err != nil || string(state) != "successor" {
		failures = append(failures, fmt.Sprintf("canonical state = %q, %v; want successor", state, err))
	}
	if len(failures) > 0 {
		t.Fatal(strings.Join(failures, "; "))
	}
}

func TestRunPatrolReportInvalidCustodyThroughLockDoesNotWriteOrMutate(t *testing.T) {
	cfg, commandLog := fakePatrolSpawnHarness(t, false)
	oldValidate, oldUpdate, oldClose := patrolReportValidate, patrolReportUpdate, patrolReportCloseRoot
	oldList, oldOpen, oldShow, oldCleanup := patrolAssignedWork, patrolHasOpenChildren, patrolShow, patrolCleanupStale
	oldChildren, oldForceClose := moleculeChildren, moleculeForceClose
	t.Cleanup(func() {
		patrolReportValidate, patrolReportUpdate, patrolReportCloseRoot = oldValidate, oldUpdate, oldClose
		patrolAssignedWork, patrolHasOpenChildren, patrolShow, patrolCleanupStale = oldList, oldOpen, oldShow, oldCleanup
		moleculeChildren, moleculeForceClose = oldChildren, oldForceClose
	})
	var cleanupCalls, writes, descendantLists, descendantCloses, rootCloses, successorProofs int
	patrolAssignedWork = func(*beads.Beads, string) ([]*beads.Issue, error) {
		return []*beads.Issue{
			{ID: "changed-root", Title: cfg.PatrolMolName, Status: beads.StatusHooked, Assignee: "deacon/"},
			{ID: "stale-root", Title: cfg.PatrolMolName, Status: beads.StatusHooked, Assignee: "deacon/"},
		}, nil
	}
	patrolHasOpenChildren = func(_ *beads.Beads, id string) (bool, error) { return id == "changed-root", nil }
	patrolCleanupStale = func(*beads.Beads, string) error { cleanupCalls++; return nil }
	patrolReportValidate = func(*beads.Beads, PatrolConfig, string) error {
		t.Fatal("validation ran after invalid expected-root custody")
		return nil
	}
	patrolReportUpdate = func(*beads.Beads, string, beads.UpdateOptions) error { writes++; return nil }
	patrolReportCloseRoot = func(*beads.Beads, string, ...string) error { rootCloses++; return nil }
	patrolShow = func(*beads.Beads, string) (*beads.Issue, error) { successorProofs++; return nil, nil }
	moleculeChildren = func(*beads.Beads, string) ([]*beads.Issue, error) { descendantLists++; return nil, nil }
	moleculeForceClose = func(*beads.Beads, ...string) error { descendantCloses++; return nil }
	err := withPatrolCustodyLock(cfg, func(lockedCfg PatrolConfig) error {
		return runPatrolReportLocked(lockedCfg, "expected-root")
	})
	if err == nil || !strings.Contains(err.Error(), "expected-root") {
		t.Fatalf("locked invalid-custody error = %v", err)
	}
	contents, readErr := os.ReadFile(commandLog)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read command log: %v", readErr)
	}
	if cleanupCalls != 0 || writes != 0 || descendantLists != 0 || descendantCloses != 0 || rootCloses != 0 || successorProofs != 0 || patrolWispCreateCount(t, commandLog) != 0 || strings.Contains(string(contents), "--status=hooked") {
		t.Fatalf("invalid custody mutated: cleanups=%d writes=%d lists=%d descendant closes=%d root closes=%d proofs=%d log=%q", cleanupCalls, writes, descendantLists, descendantCloses, rootCloses, successorProofs, contents)
	}
}

func TestRunPatrolReportRejectsOriginalRootAsSuccessor(t *testing.T) {
	oldList, oldOpen, oldShow := patrolAssignedWork, patrolHasOpenChildren, patrolShow
	oldValidate, oldUpdate, oldClose := patrolReportValidate, patrolReportUpdate, patrolReportCloseRoot
	oldChildren, oldSummary, oldSteps := moleculeChildren, patrolReportSummary, patrolReportSteps
	t.Cleanup(func() {
		patrolAssignedWork, patrolHasOpenChildren, patrolShow = oldList, oldOpen, oldShow
		patrolReportValidate, patrolReportUpdate, patrolReportCloseRoot = oldValidate, oldUpdate, oldClose
		moleculeChildren, patrolReportSummary, patrolReportSteps = oldChildren, oldSummary, oldSteps
	})
	cfg := PatrolConfig{RoleName: "witness", PatrolMolName: "mol-test-patrol", BeadsDir: t.TempDir(), Assignee: "deacon"}
	root := &beads.Issue{ID: "original-root", Title: cfg.PatrolMolName, Status: beads.StatusHooked, Assignee: "deacon/"}
	patrolAssignedWork = func(*beads.Beads, string) ([]*beads.Issue, error) { return []*beads.Issue{root}, nil }
	patrolHasOpenChildren = func(*beads.Beads, string) (bool, error) { return true, nil }
	patrolShow = func(*beads.Beads, string) (*beads.Issue, error) { return root, nil }
	patrolReportValidate = validatePatrolForCloseout
	patrolReportUpdate = func(*beads.Beads, string, beads.UpdateOptions) error { return nil }
	patrolReportCloseRoot = func(*beads.Beads, string, ...string) error { return nil }
	moleculeChildren = func(*beads.Beads, string) ([]*beads.Issue, error) { return nil, nil }
	patrolReportSummary, patrolReportSteps = "all clear", "inbox:OK"

	err := runPatrolReportLocked(cfg, root.ID)
	if err == nil {
		t.Fatal("runPatrolReportLocked accepted the original root as its own successor")
	}
}

func TestRunPatrolReportRejectsReopenedOriginalRootAsSuccessor(t *testing.T) {
	oldList, oldOpen, oldShow := patrolAssignedWork, patrolHasOpenChildren, patrolShow
	oldValidate, oldUpdate, oldClose := patrolReportValidate, patrolReportUpdate, patrolReportCloseRoot
	oldChildren, oldSummary, oldSteps := moleculeChildren, patrolReportSummary, patrolReportSteps
	t.Cleanup(func() {
		patrolAssignedWork, patrolHasOpenChildren, patrolShow = oldList, oldOpen, oldShow
		patrolReportValidate, patrolReportUpdate, patrolReportCloseRoot = oldValidate, oldUpdate, oldClose
		moleculeChildren, patrolReportSummary, patrolReportSteps = oldChildren, oldSummary, oldSteps
	})
	cfg := PatrolConfig{RoleName: "witness", PatrolMolName: "mol-test-patrol", BeadsDir: t.TempDir(), Assignee: "deacon"}
	root := &beads.Issue{ID: "original-root", Title: cfg.PatrolMolName, Status: beads.StatusHooked, Assignee: "deacon/"}
	patrolAssignedWork = func(*beads.Beads, string) ([]*beads.Issue, error) { return []*beads.Issue{root}, nil }
	patrolHasOpenChildren = func(*beads.Beads, string) (bool, error) { return true, nil }
	showCalls := 0
	patrolShow = func(*beads.Beads, string) (*beads.Issue, error) {
		showCalls++
		copy := *root
		if showCalls == 3 {
			copy.Status = "closed"
		}
		return &copy, nil
	}
	patrolReportValidate = validatePatrolForCloseout
	patrolReportUpdate = func(*beads.Beads, string, beads.UpdateOptions) error { return nil }
	patrolReportCloseRoot = func(*beads.Beads, string, ...string) error { return nil }
	moleculeChildren = func(*beads.Beads, string) ([]*beads.Issue, error) { return nil, nil }
	patrolReportSummary, patrolReportSteps = "all clear", "inbox:OK"

	err := runPatrolReportLocked(cfg, root.ID)
	if err == nil || !strings.Contains(err.Error(), "cannot be its own successor") {
		t.Fatalf("runPatrolReportLocked error = %v, want self-successor rejection", err)
	}
}

func TestAutoSpawnPatrolStaleCleanupFailuresLeaveRootAndPreventCreate(t *testing.T) {
	for _, tt := range []struct {
		name string
		set  func(error)
	}{
		{
			name: "grandchild discovery",
			set: func(wantErr error) {
				moleculeChildren = func(_ *beads.Beads, id string) ([]*beads.Issue, error) {
					if id == "stale-child" {
						return nil, wantErr
					}
					return []*beads.Issue{{ID: "stale-child", Status: "open"}}, nil
				}
				moleculeForceClose = func(*beads.Beads, ...string) error { return nil }
			},
		},
		{
			name: "descendant close",
			set: func(wantErr error) {
				moleculeChildren = func(_ *beads.Beads, id string) ([]*beads.Issue, error) {
					if id == "stale-root" {
						return []*beads.Issue{{ID: "stale-child", Status: "open"}}, nil
					}
					return nil, nil
				}
				moleculeForceClose = func(*beads.Beads, ...string) error { return wantErr }
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			oldList, oldOpen, oldChildren, oldForceClose := patrolAssignedWork, patrolHasOpenChildren, moleculeChildren, moleculeForceClose
			t.Cleanup(func() {
				patrolAssignedWork, patrolHasOpenChildren = oldList, oldOpen
				moleculeChildren, moleculeForceClose = oldChildren, oldForceClose
			})
			wantErr := errors.New(tt.name + " failed")
			patrolAssignedWork = func(*beads.Beads, string) ([]*beads.Issue, error) {
				return []*beads.Issue{{ID: "stale-root", Title: "mol-test-patrol", Status: beads.StatusHooked, Assignee: "deacon/"}}, nil
			}
			patrolHasOpenChildren = func(*beads.Beads, string) (bool, error) { return false, nil }
			tt.set(wantErr)
			cfg, commandLog := fakePatrolSpawnHarness(t, false)
			if _, err := autoSpawnPatrol(cfg); !errors.Is(err, wantErr) {
				t.Fatalf("autoSpawnPatrol error = %v, want exact %v", err, wantErr)
			}
			contents, readErr := os.ReadFile(commandLog)
			if readErr != nil && !os.IsNotExist(readErr) {
				t.Fatalf("read command log: %v", readErr)
			}
			if strings.Contains(string(contents), "close ") || patrolWispCreateCount(t, commandLog) != 0 {
				t.Fatalf("stale cleanup failure closed root or created successor; log:\n%s", contents)
			}
		})
	}
}

func TestAutoSpawnPatrolStaleAndBurnRereadProofFailuresPreventCreate(t *testing.T) {
	for _, path := range []string{"stale cleanup", "burn"} {
		for _, proof := range []string{"reread error", "non-closed status"} {
			t.Run(path+"/"+proof, func(t *testing.T) {
				oldList, oldOpen, oldShow := patrolAssignedWork, patrolHasOpenChildren, patrolShow
				oldChildren, oldForceClose := moleculeChildren, moleculeForceClose
				t.Cleanup(func() {
					patrolAssignedWork, patrolHasOpenChildren, patrolShow = oldList, oldOpen, oldShow
					moleculeChildren, moleculeForceClose = oldChildren, oldForceClose
				})

				listCalls := 0
				patrolAssignedWork = func(*beads.Beads, string) ([]*beads.Issue, error) {
					listCalls++
					if path == "burn" && listCalls == 1 {
						return nil, nil
					}
					return []*beads.Issue{{ID: "prior-root", Title: "mol-test-patrol", Status: beads.StatusHooked, Assignee: "deacon/"}}, nil
				}
				patrolHasOpenChildren = func(*beads.Beads, string) (bool, error) { return false, nil }
				moleculeChildren = func(*beads.Beads, string) ([]*beads.Issue, error) { return nil, nil }
				moleculeForceClose = func(*beads.Beads, ...string) error { return nil }

				wantErr := errors.New(path + " reread failed")
				showCalls := 0
				patrolShow = func(*beads.Beads, string) (*beads.Issue, error) {
					showCalls++
					if proof == "reread error" {
						return nil, wantErr
					}
					return &beads.Issue{ID: "prior-root", Status: beads.StatusHooked}, nil
				}

				cfg, commandLog := fakePatrolSpawnHarness(t, false)
				_, err := autoSpawnPatrol(cfg)
				if proof == "reread error" {
					if !errors.Is(err, wantErr) {
						t.Fatalf("autoSpawnPatrol error = %v, want exact %v", err, wantErr)
					}
				} else if err == nil || !strings.Contains(err.Error(), "remains \"hooked\"") {
					t.Fatalf("autoSpawnPatrol error = %v, want non-closed proof failure", err)
				}
				contents, readErr := os.ReadFile(commandLog)
				if readErr != nil {
					t.Fatalf("read command log: %v", readErr)
				}
				var rootCloses int
				for _, line := range strings.Split(string(contents), "\n") {
					if strings.HasPrefix(line, "close prior-root ") {
						rootCloses++
					}
				}
				if rootCloses != 1 || showCalls != 1 || patrolWispCreateCount(t, commandLog) != 0 {
					t.Fatalf("root closes=%d rereads=%d creates=%d, want 1,1,0; log:\n%s", rootCloses, showCalls, patrolWispCreateCount(t, commandLog), contents)
				}
			})
		}
	}
}

func TestListChildrenAcrossTablesPropagatesEachTableFailure(t *testing.T) {
	for _, tt := range []struct {
		name, fail, want string
	}{
		{name: "durable", fail: "list", want: "bd list --json --status=all --parent=root --limit=0 --flat: durable-query-sentinel"},
		{name: "ephemeral", fail: "query", want: "bd query --json ephemeral=true AND parent=\"root\" --all --limit=0: ephemeral-query-sentinel"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			binDir := t.TempDir()
			script := "#!/bin/sh\ncase \" $* \" in\n*' --help '*) exit 0 ;;\n*' list '*) [ \"$CROSS_TABLE_FAIL\" = list ] && { echo durable-query-sentinel >&2; exit 1; }; echo '[]' ;;\n*' query '*) [ \"$CROSS_TABLE_FAIL\" = query ] && { echo ephemeral-query-sentinel >&2; exit 1; }; echo '[]' ;;\nesac\n"
			if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
				t.Fatalf("write fake bd: %v", err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CROSS_TABLE_FAIL", tt.fail)
			workDir := t.TempDir()
			if err := os.Mkdir(filepath.Join(workDir, ".beads"), 0o755); err != nil {
				t.Fatalf("mkdir .beads: %v", err)
			}
			_, err := listChildrenAcrossTables(beads.NewIsolated(workDir), "root")
			if err == nil || err.Error() != tt.want {
				t.Fatalf("listChildrenAcrossTables error = %v, want exact %q", err, tt.want)
			}
		})
	}
}

func TestForceCloseDescendantsIncludesEphemeralGrandchildren(t *testing.T) {
	oldChildren, oldClose := moleculeChildren, moleculeForceClose
	t.Cleanup(func() { moleculeChildren, moleculeForceClose = oldChildren, oldClose })
	children := map[string][]*beads.Issue{"root": {{ID: "ephemeral-child", Status: "open", Ephemeral: true}}, "ephemeral-child": {{ID: "ephemeral-grandchild", Status: "open", Ephemeral: true}}}
	moleculeChildren = func(_ *beads.Beads, id string) ([]*beads.Issue, error) { return children[id], nil }
	var closed []string
	moleculeForceClose = func(_ *beads.Beads, ids ...string) error { closed = append(closed, ids...); return nil }
	count, err := forceCloseDescendants(nil, "root")
	if err != nil || count != 2 {
		t.Fatalf("forceCloseDescendants = (%d, %v), want (2,nil)", count, err)
	}
	if strings.Join(closed, ",") != "ephemeral-grandchild,ephemeral-child" {
		t.Fatalf("closed = %v", closed)
	}
	moleculeChildren = func(*beads.Beads, string) ([]*beads.Issue, error) { return nil, errors.New("ephemeral query failed") }
	if _, err := forceCloseDescendants(nil, "root"); err == nil {
		t.Fatal("expected child query failure")
	}
}

func requireBd(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd CLI not installed, skipping patrol test")
	}
}

func setupPatrolTestDB(t *testing.T) (string, *beads.Beads) {
	t.Helper()
	testutil.RequireDoltContainer(t)
	port, _ := strconv.Atoi(testutil.DoltContainerPort())
	tmpDir := t.TempDir()
	b := beads.NewIsolatedWithPort(tmpDir, port)
	// Use a unique prefix per test run to avoid cross-run contamination
	// in the shared Dolt database.
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	prefix := "pt" + hex.EncodeToString(buf[:])
	if err := b.Init(prefix); err != nil {
		t.Fatalf("bd init: %v", err)
	}

	// Clean up the test database after the test to avoid leaking
	// beads_pt* databases on the shared Dolt server.
	dbName := "beads_" + prefix
	t.Cleanup(func() {
		dsn := fmt.Sprintf("root:@tcp(127.0.0.1:%s)/", testutil.DoltContainerPort())
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Logf("cleanup: failed to connect to dolt server to drop %s: %v", dbName, err)
			return
		}
		defer db.Close()
		if _, err := db.Exec("DROP DATABASE IF EXISTS `" + dbName + "`"); err != nil {
			t.Logf("cleanup: failed to drop %s: %v", dbName, err)
		}
		// Purge dropped databases to prevent accumulation on disk
		db.Exec("CALL dolt_purge_dropped_databases()") //nolint:errcheck
	})

	return tmpDir, b
}

func openPatrolTestSQL(t *testing.T, workDir string) *sql.DB {
	t.Helper()
	dbName := beads.DatabaseNameFromMetadata(filepath.Join(workDir, ".beads"))
	if dbName == "" {
		t.Fatalf("no Dolt database metadata in %s", workDir)
	}
	dsn := fmt.Sprintf("root:@tcp(127.0.0.1:%s)/%s", testutil.DoltContainerPort(), dbName)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open patrol test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// createHookedPatrol creates a bead with a patrol title and hooks it.
// If withOpenChild is true, creates an open child bead to simulate an active patrol.
func createHookedPatrol(t *testing.T, b *beads.Beads, molName, assignee string, withOpenChild bool) string {
	t.Helper()
	root, err := b.Create(beads.CreateOptions{
		Title:    molName + " (wisp)",
		Priority: -1,
	})
	if err != nil {
		t.Fatalf("create patrol root: %v", err)
	}

	hooked := beads.StatusHooked
	if err := b.Update(root.ID, beads.UpdateOptions{
		Status:   &hooked,
		Assignee: &assignee,
	}); err != nil {
		t.Fatalf("hook patrol: %v", err)
	}

	if withOpenChild {
		_, err := b.Create(beads.CreateOptions{
			Title:    "inbox-check",
			Parent:   root.ID,
			Priority: -1,
		})
		if err != nil {
			t.Fatalf("create child: %v", err)
		}
	}
	return root.ID
}

func TestFindActivePatrolHooked(t *testing.T) {
	requireBd(t)
	tmpDir, b := setupPatrolTestDB(t)

	molName := "mol-test-patrol"
	assignee := "testrig/witness"

	rootID := createHookedPatrol(t, b, molName, assignee, true /* withOpenChild */)

	cfg := PatrolConfig{
		PatrolMolName: molName,
		BeadsDir:      tmpDir,
		Assignee:      assignee,
		Beads:         b,
	}

	patrolID, _, found, findErr := findActivePatrol(cfg)
	if findErr != nil {
		t.Fatalf("findActivePatrol error: %v", findErr)
	}
	if !found {
		t.Fatal("expected to find active patrol, got not found")
	}
	if patrolID != rootID {
		t.Errorf("patrolID = %q, want %q", patrolID, rootID)
	}

	// Verify the patrol is still hooked (not closed)
	issue, err := b.Show(rootID)
	if err != nil {
		t.Fatalf("show patrol: %v", err)
	}
	if issue.Status != beads.StatusHooked {
		t.Errorf("patrol status = %q, want %q", issue.Status, beads.StatusHooked)
	}
}

func TestFindActivePatrolUsesCanonicalAssignee(t *testing.T) {
	requireBd(t)
	tmpDir, b := setupPatrolTestDB(t)

	const (
		molName  = "mol-test-patrol"
		assignee = "deacon"
	)
	canonicalID := createHookedPatrol(t, b, molName, "deacon/", false)
	decoyID := createHookedPatrol(t, b, molName, assignee, false)

	patrolID, _, found, err := findActivePatrol(PatrolConfig{
		PatrolMolName: molName,
		BeadsDir:      tmpDir,
		Assignee:      assignee,
		Beads:         b,
	})
	if err != nil {
		t.Fatalf("findActivePatrol error: %v", err)
	}
	if !found || patrolID != canonicalID {
		t.Fatalf("findActivePatrol = (%q, %t), want canonical root %q", patrolID, found, canonicalID)
	}
	decoy, err := b.Show(decoyID)
	if err != nil {
		t.Fatalf("show bare-address decoy: %v", err)
	}
	if decoy.Status != beads.StatusHooked {
		t.Fatalf("bare-address decoy status = %q, want %q", decoy.Status, beads.StatusHooked)
	}
}

func TestFindActivePatrolRejectsMultipleCanonicalRoots(t *testing.T) {
	requireBd(t)
	tmpDir, b := setupPatrolTestDB(t)

	const molName = "mol-test-patrol"
	createHookedPatrol(t, b, molName, "deacon/", false)
	createHookedPatrol(t, b, molName, "deacon/", false)

	patrolID, _, found, err := findActivePatrol(PatrolConfig{
		PatrolMolName: molName,
		BeadsDir:      tmpDir,
		Assignee:      "deacon",
		Beads:         b,
	})
	if err == nil || !strings.Contains(err.Error(), "multiple active patrols") {
		t.Fatalf("findActivePatrol error = %v, want multiple active patrols", err)
	}
	if found || patrolID != "" {
		t.Fatalf("findActivePatrol = (%q, %t), want fail-closed empty result", patrolID, found)
	}
}

func TestFindActivePatrolStale(t *testing.T) {
	requireBd(t)
	tmpDir, b := setupPatrolTestDB(t)

	molName := "mol-test-patrol"
	assignee := "testrig/witness"

	// Create a patrol with a closed child (simulates post-squash state)
	rootID := createHookedPatrol(t, b, molName, assignee, true /* with child */)

	// Close the child to make the patrol stale
	children, err := b.List(beads.ListOptions{Parent: rootID, Status: "all", Priority: -1})
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	for _, child := range children {
		if closeErr := b.ForceCloseWithReason("test cleanup", child.ID); closeErr != nil {
			t.Fatalf("close child: %v", closeErr)
		}
	}

	cfg := PatrolConfig{
		PatrolMolName: molName,
		BeadsDir:      tmpDir,
		Assignee:      assignee,
		Beads:         b,
	}

	_, _, found, findErr := findActivePatrol(cfg)
	if findErr != nil {
		t.Fatalf("findActivePatrol error: %v", findErr)
	}
	if found {
		t.Fatal("expected stale patrol (all children closed) to NOT be found as active")
	}

	// Verify the stale patrol was closed
	issue, err := b.Show(rootID)
	if err != nil {
		t.Fatalf("show patrol: %v", err)
	}
	if issue.Status != "closed" {
		t.Errorf("stale patrol status = %q, want %q", issue.Status, "closed")
	}
}

func TestFindActivePatrolZeroChildren(t *testing.T) {
	requireBd(t)
	tmpDir, b := setupPatrolTestDB(t)

	molName := "mol-test-patrol"
	assignee := "testrig/witness"

	// Create a patrol with NO children — simulates a freshly created wisp
	// whose steps haven't materialized yet. Should be treated as active,
	// not stale, to prevent race condition.
	rootID := createHookedPatrol(t, b, molName, assignee, false /* no children */)

	cfg := PatrolConfig{
		PatrolMolName: molName,
		BeadsDir:      tmpDir,
		Assignee:      assignee,
		Beads:         b,
	}

	patrolID, _, found, findErr := findActivePatrol(cfg)
	if findErr != nil {
		t.Fatalf("findActivePatrol error: %v", findErr)
	}
	if !found {
		t.Fatal("expected zero-children patrol to be treated as active (not stale)")
	}
	if patrolID != rootID {
		t.Errorf("patrolID = %q, want %q", patrolID, rootID)
	}

	// Verify it was NOT closed
	issue, err := b.Show(rootID)
	if err != nil {
		t.Fatalf("show patrol: %v", err)
	}
	if issue.Status != beads.StatusHooked {
		t.Errorf("zero-children patrol status = %q, want %q (should remain hooked)", issue.Status, beads.StatusHooked)
	}
}

func TestFindActivePatrolPrefersHookedRootAcrossTablesAndStatuses(t *testing.T) {
	requireBd(t)
	tmpDir, b := setupPatrolTestDB(t)

	molName := "mol-test-patrol"
	assignee := "deacon/"
	hookedID := createHookedPatrol(t, b, molName, assignee, false)

	duplicate, err := b.Create(beads.CreateOptions{
		Title:     molName + " (stale duplicate)",
		Priority:  -1,
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("create duplicate patrol wisp: %v", err)
	}
	inProgress := "in_progress"
	if err := b.Update(duplicate.ID, beads.UpdateOptions{
		Status:   &inProgress,
		Assignee: &assignee,
	}); err != nil {
		t.Fatalf("assign duplicate patrol wisp: %v", err)
	}
	child, err := b.Create(beads.CreateOptions{Title: "closed duplicate child", Parent: duplicate.ID, Priority: -1})
	if err != nil {
		t.Fatalf("create duplicate child: %v", err)
	}
	if err := b.ForceCloseWithReason("test stale duplicate", child.ID); err != nil {
		t.Fatalf("close duplicate child: %v", err)
	}

	patrolID, _, found, findErr := findActivePatrolLocked(PatrolConfig{
		PatrolMolName: molName,
		BeadsDir:      tmpDir,
		Assignee:      assignee,
		Beads:         b,
	})
	if findErr != nil {
		t.Fatalf("findActivePatrol error: %v", findErr)
	}
	if !found {
		t.Fatal("expected to find current hooked patrol")
	}
	if patrolID != hookedID {
		t.Fatalf("patrolID = %q, want current hooked root %q", patrolID, hookedID)
	}
}

func TestFindActivePatrolReconcilesDuplicateSnapshotTitle(t *testing.T) {
	requireBd(t)
	tmpDir, b := setupPatrolTestDB(t)

	molName := "mol-test-patrol"
	assignee := "deacon/"
	hookedID := createHookedPatrol(t, b, molName, assignee, false)
	db := openPatrolTestSQL(t, tmpDir)
	if _, err := db.Exec("UPDATE issues SET title = '' WHERE id = ?", hookedID); err != nil {
		t.Fatalf("blank hooked snapshot title: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO wisps
			(id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, assignee, ephemeral, no_history)
		VALUES (?, ?, '', '', '', '', 'in_progress', 2, 'task', ?, 1, 1)`,
		hookedID, molName+" (wisp)", assignee); err != nil {
		t.Fatalf("insert duplicate wisp snapshot: %v", err)
	}

	patrolID, patrolLine, found, findErr := findActivePatrol(PatrolConfig{
		PatrolMolName: molName,
		BeadsDir:      tmpDir,
		Assignee:      assignee,
		Beads:         b,
	})
	if findErr != nil {
		t.Fatalf("findActivePatrol error: %v", findErr)
	}
	if !found || patrolID != hookedID {
		t.Fatalf("findActivePatrol = (%q, %t), want hooked root %q", patrolID, found, hookedID)
	}
	if !strings.Contains(patrolLine, "[hooked]") {
		t.Fatalf("patrolLine = %q, want hooked snapshot status", patrolLine)
	}
}

func TestFindActivePatrolFailsClosedAtScanBudget(t *testing.T) {
	requireBd(t)
	tmpDir, b := setupPatrolTestDB(t)

	molName := "mol-test-patrol"
	assignee := "deacon/"
	staleIDs := make([]string, 0, maxStalePurgePerRun+2)
	for i := 0; i < maxStalePurgePerRun+2; i++ {
		rootID := createHookedPatrol(t, b, molName, assignee, true)
		staleIDs = append(staleIDs, rootID)
		children, err := b.List(beads.ListOptions{Parent: rootID, Status: "all", Priority: -1})
		if err != nil {
			t.Fatalf("list children of stale patrol %d: %v", i, err)
		}
		for _, child := range children {
			if err := b.ForceCloseWithReason("test stale patrol", child.ID); err != nil {
				t.Fatalf("close child of stale patrol %d: %v", i, err)
			}
		}
	}

	active, err := b.Create(beads.CreateOptions{
		Title:     molName + " (active in-progress)",
		Priority:  -1,
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("create active in-progress patrol: %v", err)
	}
	inProgress := "in_progress"
	if err := b.Update(active.ID, beads.UpdateOptions{Status: &inProgress, Assignee: &assignee}); err != nil {
		t.Fatalf("assign active in-progress patrol: %v", err)
	}

	patrolID, _, found, findErr := findActivePatrol(PatrolConfig{
		PatrolMolName: molName,
		BeadsDir:      tmpDir,
		Assignee:      assignee,
		Beads:         b,
	})
	wantErr := fmt.Sprintf("discovery incomplete: patrol scan limit reached after %d candidates", maxStalePurgePerRun+1)
	if findErr == nil || findErr.Error() != wantErr {
		t.Fatalf("findActivePatrol error = %v, want %q", findErr, wantErr)
	}
	if found || patrolID != "" {
		t.Fatalf("findActivePatrol = (%q, %t), want fail-closed empty result", patrolID, found)
	}

	var closed, hooked int
	for _, staleID := range staleIDs {
		issue, err := b.Show(staleID)
		if err != nil {
			t.Fatalf("show stale patrol %s: %v", staleID, err)
		}
		switch issue.Status {
		case "closed":
			closed++
		case beads.StatusHooked:
			hooked++
		default:
			t.Fatalf("stale patrol %s status = %q, want closed or hooked", staleID, issue.Status)
		}
	}
	if closed != maxStalePurgePerRun || hooked != 2 {
		t.Fatalf("first scan cleanup = %d closed, %d hooked; want %d closed, 2 hooked", closed, hooked, maxStalePurgePerRun)
	}

	patrolID, _, found, findErr = findActivePatrol(PatrolConfig{
		PatrolMolName: molName,
		BeadsDir:      tmpDir,
		Assignee:      assignee,
		Beads:         b,
	})
	if findErr != nil {
		t.Fatalf("second findActivePatrol error: %v", findErr)
	}
	if !found || patrolID != active.ID {
		t.Fatalf("second findActivePatrol = (%q, %t), want existing active root %q", patrolID, found, active.ID)
	}
}

func TestFindActivePatrolMultiple(t *testing.T) {
	requireBd(t)
	tmpDir, b := setupPatrolTestDB(t)

	molName := "mol-test-patrol"
	assignee := "testrig/witness"

	// Create 2 stale patrols (with closed children) and 1 active patrol (with open child)
	stale1 := createHookedPatrol(t, b, molName, assignee, true)
	stale2 := createHookedPatrol(t, b, molName, assignee, true)
	activeID := createHookedPatrol(t, b, molName, assignee, true)

	// Close children of stale patrols to make them stale
	for _, staleID := range []string{stale1, stale2} {
		children, err := b.List(beads.ListOptions{Parent: staleID, Status: "all", Priority: -1})
		if err != nil {
			t.Fatalf("list children of %s: %v", staleID, err)
		}
		for _, child := range children {
			if closeErr := b.ForceCloseWithReason("test cleanup", child.ID); closeErr != nil {
				t.Fatalf("close child: %v", closeErr)
			}
		}
	}

	cfg := PatrolConfig{
		PatrolMolName: molName,
		BeadsDir:      tmpDir,
		Assignee:      assignee,
		Beads:         b,
	}

	patrolID, _, found, findErr := findActivePatrol(cfg)
	if findErr != nil {
		t.Fatalf("findActivePatrol error: %v", findErr)
	}
	if !found {
		t.Fatal("expected to find active patrol")
	}
	if patrolID != activeID {
		t.Errorf("patrolID = %q, want %q (should return the active one)", patrolID, activeID)
	}

	// Verify active patrol is still hooked
	issue, err := b.Show(activeID)
	if err != nil {
		t.Fatalf("show active: %v", err)
	}
	if issue.Status != beads.StatusHooked {
		t.Errorf("active patrol status = %q, want %q", issue.Status, beads.StatusHooked)
	}

	// Stale patrol cleanup is not guaranteed when an active patrol is found —
	// findActivePatrol breaks early on active discovery to prevent N+1 Dolt queries
	// (gt-18dzn6p). Remaining stale beads are cleaned by burnPreviousPatrolWisps
	// when the patrol cycle ends. Verify stale beads are either closed or still hooked
	// (not left in an intermediate broken state).
	for _, id := range []string{stale1, stale2} {
		staleIssue, showErr := b.Show(id)
		if showErr != nil {
			t.Fatalf("show stale %s: %v", id, showErr)
		}
		if staleIssue.Status != "closed" && staleIssue.Status != beads.StatusHooked {
			t.Errorf("stale patrol %s status = %q, want closed or hooked", id, staleIssue.Status)
		}
	}
}

// TestFindActivePatrol_StaleCleanupCapped verifies that when many stale patrols
// accumulate with no active patrol, cleanup is capped at maxStalePurgePerRun per call
// to prevent overwhelming Dolt with sequential write queries (gt-18dzn6p).
func TestFindActivePatrol_StaleCleanupCapped(t *testing.T) {
	requireBd(t)
	tmpDir, b := setupPatrolTestDB(t)

	molName := "mol-test-patrol"
	assignee := "testrig/witness"

	// Create more stale patrols than maxStalePurgePerRun (currently 5)
	numStale := maxStalePurgePerRun + 3 // e.g., 8 total
	staleIDs := make([]string, numStale)
	for i := 0; i < numStale; i++ {
		id := createHookedPatrol(t, b, molName, assignee, true /* with child */)
		staleIDs[i] = id

		// Close the child to make the patrol stale
		children, err := b.List(beads.ListOptions{Parent: id, Status: "all", Priority: -1})
		if err != nil {
			t.Fatalf("list children of %s: %v", id, err)
		}
		for _, child := range children {
			if closeErr := b.ForceCloseWithReason("test cleanup", child.ID); closeErr != nil {
				t.Fatalf("close child of %s: %v", id, closeErr)
			}
		}
	}

	cfg := PatrolConfig{
		PatrolMolName: molName,
		BeadsDir:      tmpDir,
		Assignee:      assignee,
		Beads:         b,
	}

	_, _, found, findErr := findActivePatrolLocked(cfg)
	wantErr := fmt.Sprintf("discovery incomplete: patrol scan limit reached after %d candidates", maxPatrolDiscoveryScans)
	if findErr == nil || findErr.Error() != wantErr {
		t.Fatalf("findActivePatrol error = %v, want %q", findErr, wantErr)
	}
	if found {
		t.Fatal("expected no active patrol (all stale)")
	}

	// Count how many stale patrols were actually closed
	closedCount := 0
	hookedCount := 0
	for _, id := range staleIDs {
		issue, err := b.Show(id)
		if err != nil {
			t.Fatalf("show stale %s: %v", id, err)
		}
		switch issue.Status {
		case "closed":
			closedCount++
		case beads.StatusHooked:
			hookedCount++
		default:
			t.Errorf("stale patrol %s unexpected status %q", id, issue.Status)
		}
	}

	// Cleanup reaches, but never exceeds, the bounded stale batch.
	if closedCount != maxStalePurgePerRun {
		t.Errorf("closed %d stale patrols, want exactly %d", closedCount, maxStalePurgePerRun)
	}
	// Total accounted for
	if closedCount+hookedCount != numStale {
		t.Errorf("closed=%d + hooked=%d != total=%d", closedCount, hookedCount, numStale)
	}
}

func TestBurnPreviousPatrolWisps(t *testing.T) {
	requireBd(t)
	tmpDir, b := setupPatrolTestDB(t)

	molName := "mol-test-patrol"
	assignee := "testrig/witness"

	// Create 3 hooked patrol wisps (simulating accumulated orphans)
	id1 := createHookedPatrol(t, b, molName, assignee, true)
	id2 := createHookedPatrol(t, b, molName, assignee, false)
	id3 := createHookedPatrol(t, b, molName, assignee, true)

	cfg := PatrolConfig{
		PatrolMolName: molName,
		BeadsDir:      tmpDir,
		Assignee:      assignee,
		Beads:         b,
	}

	if err := burnPreviousPatrolWisps(cfg); err != nil {
		t.Fatalf("burnPreviousPatrolWisps: %v", err)
	}

	// All 3 patrols should now be closed
	for _, id := range []string{id1, id2, id3} {
		issue, err := b.Show(id)
		if err != nil {
			t.Fatalf("show %s: %v", id, err)
		}
		if issue.Status != "closed" {
			t.Errorf("patrol %s status = %q, want %q after burn", id, issue.Status, "closed")
		}
	}
}

func TestBurnPreviousPatrolWisps_IgnoresOtherBeads(t *testing.T) {
	requireBd(t)
	tmpDir, b := setupPatrolTestDB(t)

	molName := "mol-test-patrol"
	assignee := "testrig/witness"

	// Create a patrol wisp (should be burned)
	patrolID := createHookedPatrol(t, b, molName, assignee, true)

	// Create a non-patrol hooked bead (should NOT be burned)
	other, err := b.Create(beads.CreateOptions{
		Title:    "some-other-work",
		Priority: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	hooked := beads.StatusHooked
	if err := b.Update(other.ID, beads.UpdateOptions{
		Status:   &hooked,
		Assignee: &assignee,
	}); err != nil {
		t.Fatal(err)
	}

	cfg := PatrolConfig{
		PatrolMolName: molName,
		BeadsDir:      tmpDir,
		Assignee:      assignee,
		Beads:         b,
	}

	if err := burnPreviousPatrolWisps(cfg); err != nil {
		t.Fatalf("burnPreviousPatrolWisps: %v", err)
	}

	// Patrol should be closed
	issue, err := b.Show(patrolID)
	if err != nil {
		t.Fatalf("show patrol: %v", err)
	}
	if issue.Status != "closed" {
		t.Errorf("patrol status = %q, want %q", issue.Status, "closed")
	}

	// Non-patrol bead should still be hooked
	otherIssue, err := b.Show(other.ID)
	if err != nil {
		t.Fatalf("show other: %v", err)
	}
	if otherIssue.Status != beads.StatusHooked {
		t.Errorf("non-patrol bead status = %q, want %q (should not be burned)", otherIssue.Status, beads.StatusHooked)
	}
}
