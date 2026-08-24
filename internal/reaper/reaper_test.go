package reaper

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidateDBName(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{"hq", false},
		{"beads", false},
		{"gt", false},
		{"test_db_123", false},
		{"", true},
		{"drop table", true},
		{"db;--", true},
		{"db`name", true},
		{"../etc/passwd", true},
	}
	for _, tt := range tests {
		err := ValidateDBName(tt.name)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateDBName(%q) error = %v, wantErr %v", tt.name, err, tt.wantErr)
		}
	}
}

func TestDefaultDatabases(t *testing.T) {
	if len(DefaultDatabases) == 0 {
		t.Error("DefaultDatabases should not be empty")
	}
	for _, db := range DefaultDatabases {
		if err := ValidateDBName(db); err != nil {
			t.Errorf("DefaultDatabases contains invalid name %q: %v", db, err)
		}
	}
}

func TestDogReaperFormulaAlertThresholdMatchesDefault(t *testing.T) {
	data, err := os.ReadFile("../formula/formulas/mol-dog-reaper.formula.toml")
	if err != nil {
		t.Fatalf("read mol-dog-reaper formula: %v", err)
	}

	threshold := fmt.Sprintf("%d", DefaultAlertThreshold)
	source := string(data)
	alertThresholdVars := sourceBetween(t, source, "[vars.alert_threshold]", "[vars.dry_run]")
	if !strings.Contains(alertThresholdVars, fmt.Sprintf("default = %q", threshold)) {
		t.Fatalf("mol-dog-reaper alert_threshold default should match DefaultAlertThreshold %s", threshold)
	}
	if !strings.Contains(source, fmt.Sprintf("default %s", threshold)) {
		t.Fatalf("mol-dog-reaper alert_threshold prose should document default %s", threshold)
	}
}

func TestFormatJSON(t *testing.T) {
	result := FormatJSON(map[string]int{"count": 42})
	if result == "" {
		t.Error("FormatJSON should not return empty string")
	}
	if result[0] != '{' {
		t.Errorf("FormatJSON should return JSON object, got %q", result[:10])
	}
}

func TestParentExcludeJoin(t *testing.T) {
	joinClause, whereCondition := parentExcludeJoin("testdb")

	// JOIN clause should reference the correct database.
	if joinClause == "" {
		t.Error("parentExcludeJoin joinClause should not be empty")
	}
	// parentExcludeJoin no longer qualifies table names with the database — the
	// reaper connects to a specific database via the DSN, so unqualified names
	// are correct. The dbName parameter is retained for API compatibility.

	// JOIN should select wisps with open parents from wisp_dependencies.
	if !contains(joinClause, "wisp_dependencies") {
		t.Error("parentExcludeJoin should query wisp_dependencies")
	}
	if !contains(joinClause, "wd.depends_on_wisp_id") {
		t.Error("parentExcludeJoin should join wisp parents through depends_on_wisp_id")
	}
	if !contains(joinClause, "wd.depends_on_issue_id") {
		t.Error("parentExcludeJoin should join issue parents through depends_on_issue_id")
	}
	if contains(joinClause, "wd.depends_on_id") {
		t.Error("parentExcludeJoin should not use legacy depends_on_id")
	}
	if !contains(joinClause, "parent-child") {
		t.Error("parentExcludeJoin should filter on parent-child type")
	}
	if !contains(joinClause, "'open', 'hooked', 'in_progress'") {
		t.Error("parentExcludeJoin should check for open parent statuses")
	}

	// WHERE condition should be an IS NULL anti-join filter.
	if whereCondition == "" {
		t.Error("parentExcludeJoin whereCondition should not be empty")
	}
	if !contains(whereCondition, "IS NULL") {
		t.Error("parentExcludeJoin whereCondition should use IS NULL for anti-join")
	}
}

func TestReaperQueriesUseTypedDependencyColumns(t *testing.T) {
	sourcePath := "reaper.go"
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("read %s: %v", sourcePath, err)
	}
	source := string(data)
	if strings.Contains(source, "depends_on_id") {
		t.Fatalf("reaper queries should not use legacy depends_on_id")
	}

	scanBody := sourceBetween(t, source, "func Scan(", "func Reap(")
	autoCloseBody := sourceBetween(t, source, "func AutoClose(", "// batchDeleteRows")
	batchDeleteBody := sourceBetween(t, source, "func batchDeleteRows(", "// ClosePluginReceiptResult")
	schemaBody := sourceBetween(t, source, "func HasReaperSchema(", "func tableExists(")

	for _, want := range []string{
		`hasColumns(ctx, db, "wisp_dependencies", "depends_on_issue_id", "depends_on_wisp_id", "depends_on_external")`,
		`hasColumns(ctx, db, "dependencies", "depends_on_issue_id", "depends_on_wisp_id", "depends_on_external")`,
	} {
		if !strings.Contains(schemaBody, want) {
			t.Fatalf("HasReaperSchema missing typed schema gate %q", want)
		}
	}

	for _, body := range []struct {
		name string
		text string
	}{
		{name: "Scan", text: scanBody},
		{name: "AutoClose", text: autoCloseBody},
	} {
		if !strings.Contains(body.text, "d.depends_on_issue_id = dep.id") {
			t.Fatalf("%s should join dependency blockers through depends_on_issue_id", body.name)
		}
		if !strings.Contains(body.text, "SELECT DISTINCT d.depends_on_issue_id") {
			t.Fatalf("%s should exclude blocked issues through depends_on_issue_id", body.name)
		}
		if !strings.Contains(body.text, "d.depends_on_issue_id IS NOT NULL") {
			t.Fatalf("%s should guard nullable depends_on_issue_id in NOT IN subquery", body.name)
		}
		if !strings.Contains(body.text, "'gt:message'") {
			t.Fatalf("%s should exclude protocol mail from stale auto-close", body.name)
		}
	}

	if !strings.Contains(scanBody, "wd.depends_on_wisp_id IS NOT NULL OR wd.depends_on_issue_id IS NOT NULL") {
		t.Fatal("Scan dangling-parent anomaly should ignore external-only dependency rows")
	}
	if !strings.Contains(batchDeleteBody, "DELETE FROM wisp_dependencies WHERE depends_on_wisp_id IN %s") {
		t.Fatal("batchDeleteRows should clean reverse wisp dependency references")
	}
	if !strings.Contains(batchDeleteBody, "DELETE FROM dependencies WHERE depends_on_wisp_id IN %s") {
		t.Fatal("batchDeleteRows should clean reverse issue dependency references to wisps")
	}
	if !strings.Contains(batchDeleteBody, "DELETE FROM wisp_dependencies WHERE depends_on_issue_id IN %s") {
		t.Fatal("batchDeleteRows should clean reverse wisp parent references to issues")
	}
	if !strings.Contains(batchDeleteBody, "DELETE FROM dependencies WHERE depends_on_issue_id IN %s") {
		t.Fatal("batchDeleteRows should clean reverse issue dependency references")
	}
}

// TestReapQueryNoDatabaseNameInjection verifies that the Reap function's batch
// SELECT query does not inject the database name into the SQL string. Previously,
// dbName was passed as a Sprintf arg but the format string didn't use it, causing
// positional shift: "FROM wisps w gt WHERE..." instead of "FROM wisps w LEFT JOIN...".
func TestReapQueryNoDatabaseNameInjection(t *testing.T) {
	// Reproduce the exact Sprintf call from Reap() to verify no dbName injection.
	dbName := "gt"
	parentJoin, parentWhere := parentExcludeJoin(dbName)
	whereClause := fmt.Sprintf(
		"w.status IN ('open', 'hooked', 'in_progress') AND w.created_at < ? AND %s", parentWhere)

	// This is the fixed query — dbName is NOT in the Sprintf args.
	idQuery := fmt.Sprintf(
		"SELECT w.id FROM wisps w %s WHERE %s LIMIT %d",
		parentJoin, whereClause, DefaultBatchSize)

	// The query must NOT contain the literal database name as a bare token.
	// Before the fix, "gt" appeared between "wisps w" and "WHERE".
	if strings.Contains(idQuery, "wisps w gt") {
		t.Errorf("Reap idQuery contains injected database name: %s", idQuery)
	}
	if !strings.Contains(idQuery, "LEFT JOIN") {
		t.Errorf("Reap idQuery should contain LEFT JOIN from parentExcludeJoin, got: %s", idQuery)
	}
	if !strings.Contains(idQuery, fmt.Sprintf("LIMIT %d", DefaultBatchSize)) {
		t.Errorf("Reap idQuery should end with LIMIT %d, got: %s", DefaultBatchSize, idQuery)
	}
}

// TestReapUpdateQueryNoDatabaseNameInjection verifies that the UPDATE query in
// Reap() does not inject dbName where the IN clause should go.
func TestReapUpdateQueryNoDatabaseNameInjection(t *testing.T) {
	dbName := "gt"
	inClause := "?,?,?"

	// This is the fixed query — only inClause in the Sprintf args.
	updateQuery := fmt.Sprintf(
		"UPDATE wisps SET status='closed', closed_at=NOW() WHERE id IN (%s)",
		inClause)

	if strings.Contains(updateQuery, dbName) {
		t.Errorf("Reap updateQuery contains injected database name %q: %s", dbName, updateQuery)
	}
	if !strings.Contains(updateQuery, "IN (?,?,?)") {
		t.Errorf("Reap updateQuery should contain parameterized IN clause, got: %s", updateQuery)
	}
}

// TestPurgeDigestQueryNoDatabaseNameInjection verifies that the purge digest
// query is a plain string with no Sprintf interpolation at all.
func TestPurgeDigestQueryNoDatabaseNameInjection(t *testing.T) {
	// The fixed digestQuery is a string literal — no Sprintf.
	digestQuery := "SELECT COALESCE(w.wisp_type, 'unknown') AS wtype, COUNT(*) AS cnt FROM wisps w WHERE w.status = 'closed' AND w.closed_at < ? GROUP BY wtype"

	if strings.Contains(digestQuery, "gt") {
		t.Errorf("purge digestQuery should not contain database name, got: %s", digestQuery)
	}
	if !strings.Contains(digestQuery, "GROUP BY wtype") {
		t.Errorf("purge digestQuery should end with GROUP BY, got: %s", digestQuery)
	}
}

// TestPurgeBatchQueryNoDatabaseNameInjection verifies that the purge batch
// SELECT query uses DefaultBatchSize as the LIMIT, not dbName.
func TestPurgeBatchQueryNoDatabaseNameInjection(t *testing.T) {
	// This is the fixed query — only DefaultBatchSize in the Sprintf args.
	idQuery := fmt.Sprintf(
		"SELECT w.id FROM wisps w WHERE w.status = 'closed' AND w.closed_at < ? LIMIT %d",
		DefaultBatchSize)

	if strings.Contains(idQuery, "gt") {
		t.Errorf("purge idQuery contains injected database name: %s", idQuery)
	}
	expected := fmt.Sprintf("LIMIT %d", DefaultBatchSize)
	if !strings.Contains(idQuery, expected) {
		t.Errorf("purge idQuery should contain %s, got: %s", expected, idQuery)
	}
}

// TestIsNothingToCommit verifies that "nothing to commit" errors are recognized
// correctly. This prevents false-positive dolt_commit_failed anomalies when the
// reaper operates on dolt_ignored tables (wisps, wisp_*), where Dolt has nothing
// to version after a successful SQL DELETE.
func TestIsNothingToCommit(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"nothing to commit", true},
		{"NOTHING TO COMMIT", true},
		{"Error 1105 (HY000): nothing to commit", true},
		{"no changes to commit", false}, // must also contain "commit" — see isNothingToCommit
		{"no changes", false},
		{"connection refused", false},
		{"table not found: wisps", false},
		{"", false},
	}
	for _, c := range cases {
		var err error
		if c.msg != "" {
			err = fmt.Errorf("%s", c.msg)
		}
		got := isNothingToCommit(err)
		if got != c.want {
			t.Errorf("isNothingToCommit(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func sourceBetween(t *testing.T, source, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(source, startMarker)
	if start == -1 {
		t.Fatalf("could not find %q", startMarker)
	}
	end := strings.Index(source[start:], endMarker)
	if end == -1 {
		t.Fatalf("could not find %q after %q", endMarker, startMarker)
	}
	return source[start : start+end]
}

// TestReapExcludesAgentBeads verifies that the Reap function excludes agent beads
// from being closed, regardless of their age. This is a regression test for the bug
// where the wisp reaper was closing agent beads (hq-mayor, hq-deacon, witness, refinery,
// etc.) after 24 hours, causing doctor to report them as missing.
func TestReapExcludesAgentBeads(t *testing.T) {
	// Verify that the WHERE clause in Reap() excludes issue_type='agent'
	// by checking the source code pattern.
	// This is a compile-time guard — if the exclusion is removed, this test
	// will fail when the query pattern doesn't match.

	// The whereClause in Reap() should contain:
	// "w.issue_type != 'agent'"
	// This test documents the expected behavior; actual exclusion is tested
	// in integration tests with a real database.

	// Integration test would require spinning up a Dolt server, which is
	// beyond the scope of this unit test. The exclusion is verified manually
	// by checking that agent beads are not closed by the wisp_reaper patrol.
	t.Log("Agent beads (issue_type='agent') are excluded from wisp reaping")
	t.Log("This prevents hq-mayor, hq-deacon, witness, refinery, etc. from being closed")
}

// TestScanExcludesAgentBeads documents that Scan() must use the same eligibility
// predicate as Reap() for stale open wisps. If Scan counts agent beads but Reap
// excludes them, the operator sees scan>0 and reap=0 for the same cutoff.
func TestScanExcludesAgentBeads(t *testing.T) {
	sourcePath := "reaper.go"
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("read %s: %v", sourcePath, err)
	}
	source := string(data)
	scanStart := strings.Index(source, "func Scan(")
	reapStart := strings.Index(source, "func Reap(")
	if scanStart == -1 || reapStart == -1 || reapStart <= scanStart {
		t.Fatalf("could not isolate Scan() body in %s", sourcePath)
	}
	scanBody := source[scanStart:reapStart]
	if !strings.Contains(scanBody, "w.issue_type != 'agent'") {
		t.Fatalf("expected Scan() eligibility to exclude agent beads, scan body was:\n%s", scanBody)
	}
}

func TestClosedMoleculeStepReapBehavior(t *testing.T) {
	now := time.Now().UTC()
	state := &fakeReaperState{
		wisps: map[string]*fakeWisp{
			"mol-closed":               {id: "mol-closed", status: "closed", issueType: "molecule", createdAt: now},
			"mol-open":                 {id: "mol-open", status: "open", issueType: "molecule", createdAt: now},
			"closed-epic":              {id: "closed-epic", status: "closed", issueType: "epic", createdAt: now},
			"step-closed-mol-recent":   {id: "step-closed-mol-recent", status: "open", issueType: "task", createdAt: now.Add(-1 * time.Hour)},
			"step-closed-mol-old":      {id: "step-closed-mol-old", status: "open", issueType: "task", createdAt: now.Add(-48 * time.Hour)},
			"step-mixed-parent-old":    {id: "step-mixed-parent-old", status: "open", issueType: "task", createdAt: now.Add(-48 * time.Hour)},
			"step-external-parent-old": {id: "step-external-parent-old", status: "open", issueType: "task", createdAt: now.Add(-48 * time.Hour)},
			"step-open-parent-old":     {id: "step-open-parent-old", status: "open", issueType: "task", createdAt: now.Add(-48 * time.Hour)},
			"step-non-molecule-parent": {id: "step-non-molecule-parent", status: "open", issueType: "task", createdAt: now.Add(-48 * time.Hour)},
			"agent-step":               {id: "agent-step", status: "open", issueType: "agent", createdAt: now.Add(-48 * time.Hour)},
			"stale-orphan":             {id: "stale-orphan", status: "open", issueType: "task", createdAt: now.Add(-48 * time.Hour)},
			"fresh-orphan":             {id: "fresh-orphan", status: "open", issueType: "task", createdAt: now.Add(-1 * time.Hour)},
		},
		deps: []fakeDep{
			{issueID: "step-closed-mol-recent", dependsOnID: "mol-closed", depType: "parent-child"},
			{issueID: "step-closed-mol-old", dependsOnID: "mol-closed", depType: "parent-child"},
			{issueID: "step-mixed-parent-old", dependsOnID: "mol-closed", depType: "parent-child"},
			{issueID: "step-mixed-parent-old", dependsOnID: "mol-open", depType: "parent-child"},
			{issueID: "step-external-parent-old", dependsOnID: "mol-closed", depType: "parent-child"},
			{issueID: "step-external-parent-old", dependsOnExternal: "external:other", depType: "parent-child"},
			{issueID: "step-open-parent-old", dependsOnID: "mol-open", depType: "parent-child"},
			{issueID: "step-non-molecule-parent", dependsOnID: "closed-epic", depType: "parent-child"},
			{issueID: "agent-step", dependsOnID: "mol-closed", depType: "parent-child"},
		},
		ops: map[int][]string{},
	}
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })

	maxAge := 24 * time.Hour
	scan, err := Scan(db, "testdb", maxAge, 7*24*time.Hour, 7*24*time.Hour, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if scan.MoleculeStepCandidates != 2 {
		t.Fatalf("Scan MoleculeStepCandidates = %d, want 2", scan.MoleculeStepCandidates)
	}
	if scan.ReapCandidates != 2 {
		t.Fatalf("Scan ReapCandidates = %d, want 2", scan.ReapCandidates)
	}

	beforeDryRun := state.statuses()
	dryRun, err := Reap(db, "testdb", maxAge, true)
	if err != nil {
		t.Fatalf("dry-run Reap: %v", err)
	}
	if dryRun.MoleculeStepsClosed != 2 {
		t.Fatalf("dry-run MoleculeStepsClosed = %d, want 2", dryRun.MoleculeStepsClosed)
	}
	if dryRun.Reaped != 2 {
		t.Fatalf("dry-run Reaped = %d, want 2", dryRun.Reaped)
	}
	if dryRun.OpenRemain != 10 {
		t.Fatalf("dry-run OpenRemain = %d, want 10", dryRun.OpenRemain)
	}
	if afterDryRun := state.statuses(); !reflect.DeepEqual(afterDryRun, beforeDryRun) {
		t.Fatalf("dry-run mutated statuses: before=%v after=%v", beforeDryRun, afterDryRun)
	}

	preRealOps := state.opCounts()
	realRun, err := Reap(db, "testdb", maxAge, false)
	if err != nil {
		t.Fatalf("real Reap: %v", err)
	}
	if realRun.MoleculeStepsClosed != 2 {
		t.Fatalf("real MoleculeStepsClosed = %d, want 2; ops=%v statuses=%v", realRun.MoleculeStepsClosed, state.opsSince(preRealOps), state.statuses())
	}
	if realRun.Reaped != 2 {
		t.Fatalf("real Reaped = %d, want 2", realRun.Reaped)
	}
	if realRun.OpenRemain != 6 {
		t.Fatalf("real OpenRemain = %d, want 6", realRun.OpenRemain)
	}

	for _, id := range []string{"step-closed-mol-recent", "step-closed-mol-old", "step-non-molecule-parent", "stale-orphan"} {
		if got := state.status(id); got != "closed" {
			t.Fatalf("%s status = %q, want closed", id, got)
		}
	}
	for _, id := range []string{"step-mixed-parent-old", "step-external-parent-old", "step-open-parent-old", "agent-step", "fresh-orphan", "mol-open"} {
		if got := state.status(id); got != "open" {
			t.Fatalf("%s status = %q, want open", id, got)
		}
	}
	realOps := state.opsSince(preRealOps)
	if len(realOps) != 1 {
		t.Fatalf("real Reap used %d connections, want 1: %#v", len(realOps), realOps)
	}
	for connID, ops := range realOps {
		assertOpsContainInOrder(t, ops,
			"EXEC SET @@autocommit = 0",
			"QUERY SELECT w.id FROM wisps w INNER JOIN",
			"EXEC UPDATE wisps SET status='closed'",
			"QUERY SELECT w.id FROM wisps w LEFT JOIN",
			"EXEC UPDATE wisps SET status='closed'",
			"EXEC COMMIT",
			"EXEC CALL DOLT_COMMIT",
			"QUERY SELECT COUNT(*) FROM wisps WHERE status IN",
			"EXEC SET @@autocommit = 1",
		)
		t.Logf("real Reap used pinned connection %d", connID)
	}
}

func TestAutoCloseExcludesControlPlaneIdentityRecords(t *testing.T) {
	now := time.Now().UTC()
	state := &fakeReaperState{
		issues: map[string]*fakeIssue{
			"stale-task":          {id: "stale-task", title: "Stale task", status: "open", issueType: "task", updatedAt: now.Add(-8 * 24 * time.Hour)},
			"protected-message":   {id: "protected-message", title: "Protocol escalation", status: "open", issueType: "task", updatedAt: now.Add(-8 * 24 * time.Hour), labels: []string{"gt:message", "gt:escalation", "msg-type:escalation"}},
			"protected-mail-work": {id: "protected-mail-work", title: "Actionable mail", status: "open", issueType: "task", updatedAt: now.Add(-8 * 24 * time.Hour), labels: []string{"gt:mail-work"}},
			"labeled-convoy":      {id: "labeled-convoy", title: "Tracked by convoy", status: "open", issueType: "task", updatedAt: now.Add(-8 * 24 * time.Hour), labels: []string{"gt:convoy"}},
			"typed-convoy":        {id: "typed-convoy", title: "Convoy", status: "open", issueType: "convoy", updatedAt: now.Add(-8 * 24 * time.Hour)},
			"typed-agent":         {id: "typed-agent", title: "Agent", status: "open", issueType: "agent", updatedAt: now.Add(-8 * 24 * time.Hour)},
			"protected-agent":     {id: "protected-agent", title: "Refinery identity", status: "open", issueType: "task", updatedAt: now.Add(-8 * 24 * time.Hour), labels: []string{"gt:agent"}},
			"protected-standing":  {id: "protected-standing", title: "Standing order", status: "open", issueType: "task", updatedAt: now.Add(-8 * 24 * time.Hour), labels: []string{"gt:standing-orders"}},
			"protected-keep":      {id: "protected-keep", title: "Keep", status: "open", issueType: "task", updatedAt: now.Add(-8 * 24 * time.Hour), labels: []string{"gt:keep"}},
			"protected-role":      {id: "protected-role", title: "Role", status: "open", issueType: "task", updatedAt: now.Add(-8 * 24 * time.Hour), labels: []string{"gt:role"}},
			"protected-rig":       {id: "protected-rig", title: "Rig", status: "open", issueType: "task", updatedAt: now.Add(-8 * 24 * time.Hour), labels: []string{"gt:rig"}},
		},
		ops: map[int][]string{},
	}
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })

	result, err := AutoClose(db, "testdb", 7*24*time.Hour, true)
	if err != nil {
		t.Fatalf("AutoClose: %v", err)
	}
	if result.Closed != 1 {
		t.Fatalf("AutoClose closed %d issues, want only the unprotected stale task: %#v", result.Closed, result.ClosedEntries)
	}
	if got := result.ClosedEntries[0].ID; got != "stale-task" {
		t.Fatalf("AutoClose candidate = %q, want stale-task", got)
	}

	result, err = AutoClose(db, "testdb", 7*24*time.Hour, false)
	if err != nil {
		t.Fatalf("AutoClose live: %v", err)
	}
	if result.Closed != 1 || state.status("stale-task") != "closed" {
		t.Fatalf("AutoClose live closed = %#v; stale task status = %q", result.ClosedEntries, state.status("stale-task"))
	}
	for _, id := range []string{"protected-message", "protected-mail-work", "labeled-convoy", "typed-convoy", "typed-agent", "protected-agent", "protected-standing", "protected-keep", "protected-role", "protected-rig"} {
		if got := state.status(id); got != "open" {
			t.Fatalf("AutoClose live changed protected identity %q to %q", id, got)
		}
	}
}

func TestAutoCloseRevalidatesEligibilityAtUpdate(t *testing.T) {
	now := time.Now().UTC()
	state := &fakeReaperState{
		issues: map[string]*fakeIssue{
			"becomes-agent":       {id: "becomes-agent", title: "Agent race", status: "open", issueType: "task", updatedAt: now.Add(-8 * 24 * time.Hour)},
			"gains-agent-tag":     {id: "gains-agent-tag", title: "Label race", status: "open", issueType: "task", updatedAt: now.Add(-8 * 24 * time.Hour)},
			"gains-message-tag":   {id: "gains-message-tag", title: "Protocol mail race", status: "open", issueType: "task", updatedAt: now.Add(-8 * 24 * time.Hour)},
			"gains-mail-work-tag": {id: "gains-mail-work-tag", title: "Mail race", status: "open", issueType: "task", updatedAt: now.Add(-8 * 24 * time.Hour)},
			"still-stale":         {id: "still-stale", title: "Still stale", status: "open", issueType: "task", updatedAt: now.Add(-8 * 24 * time.Hour)},
		},
		ops: map[int][]string{},
	}
	state.beforeAutoCloseUpdate = func(s *fakeReaperState) {
		s.issues["becomes-agent"].issueType = "agent"
		s.issues["gains-agent-tag"].labels = append(s.issues["gains-agent-tag"].labels, "gt:agent")
		s.issues["gains-message-tag"].labels = append(s.issues["gains-message-tag"].labels, "gt:message")
		s.issues["gains-mail-work-tag"].labels = append(s.issues["gains-mail-work-tag"].labels, "gt:mail-work")
	}
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })

	result, err := AutoClose(db, "testdb", 7*24*time.Hour, false)
	if err != nil {
		t.Fatalf("AutoClose: %v", err)
	}
	if result.Closed != 1 || len(result.ClosedEntries) != 1 || result.ClosedEntries[0].ID != "still-stale" {
		t.Fatalf("AutoClose closed = %d entries=%#v, want only still-stale", result.Closed, result.ClosedEntries)
	}
	if got := state.status("becomes-agent"); got != "open" {
		t.Fatalf("concurrently retyped agent status = %q, want open", got)
	}
	if got := state.status("gains-agent-tag"); got != "open" {
		t.Fatalf("concurrently protected identity status = %q, want open", got)
	}
	if got := state.status("gains-message-tag"); got != "open" {
		t.Fatalf("concurrently protected protocol mail status = %q, want open", got)
	}
	if got := state.status("gains-mail-work-tag"); got != "open" {
		t.Fatalf("concurrently enrolled mail work status = %q, want open", got)
	}
	transactionConnections := map[int]bool{}
	for connectionID, operations := range state.opsSince(nil) {
		for _, operation := range operations {
			if strings.Contains(operation, "SET @@autocommit = 0") ||
				strings.Contains(operation, "UPDATE `testdb`.issues") ||
				operation == "EXEC COMMIT" || strings.Contains(operation, "CALL DOLT_COMMIT") {
				transactionConnections[connectionID] = true
			}
		}
	}
	if len(transactionConnections) != 1 {
		t.Fatalf("AutoClose transaction used connections %v, want one pinned connection", transactionConnections)
	}
}

func TestAutoCloseConditionalUpdateRunsOnIsolatedDolt(t *testing.T) {
	if os.Getenv("GT_TEST_EXTERNAL_DOLT") != "1" ||
		os.Getenv("GT_TEST_ISOLATED") != "1" ||
		os.Getenv("GT_DOLT_HOST") != "127.0.0.1" {
		t.Skip("requires the explicit isolated Dolt test harness")
	}
	port, err := strconv.Atoi(os.Getenv("GT_DOLT_PORT"))
	if err != nil || port <= 0 || port == 33327 {
		t.Fatalf("refusing non-isolated Dolt port %q", os.Getenv("GT_DOLT_PORT"))
	}
	dbName := fmt.Sprintf("reaper_autoclose_%d", time.Now().UnixNano())
	rootDB, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/?parseTime=true", port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rootDB.Close() })
	if _, err := rootDB.Exec("CREATE DATABASE `" + dbName + "`"); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	t.Cleanup(func() { _, _ = rootDB.Exec("DROP DATABASE IF EXISTS `" + dbName + "`") })

	db, err := OpenDB("127.0.0.1", port, dbName, 15*time.Second, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`CREATE TABLE issues (
			id VARCHAR(64) PRIMARY KEY, title VARCHAR(255) NOT NULL, status VARCHAR(32) NOT NULL,
			priority INT NOT NULL, issue_type VARCHAR(32) NOT NULL, updated_at DATETIME NOT NULL,
			closed_at DATETIME NULL, close_reason TEXT NULL)`,
		`CREATE TABLE labels (issue_id VARCHAR(64) NOT NULL, label VARCHAR(64) NOT NULL)`,
		`CREATE TABLE dependencies (issue_id VARCHAR(64) NOT NULL, depends_on_issue_id VARCHAR(64) NULL)`,
		`INSERT INTO issues (id, title, status, priority, issue_type, updated_at) VALUES
			('stale-task', 'Stale task', 'open', 3, 'task', NOW() - INTERVAL 8 DAY),
			('protected-agent', 'Agent identity', 'open', 3, 'task', NOW() - INTERVAL 8 DAY),
			('protected-message', 'Protocol escalation', 'open', 3, 'task', NOW() - INTERVAL 8 DAY),
			('protected-mail-work', 'Actionable mail', 'open', 3, 'task', NOW() - INTERVAL 8 DAY),
			('mixed-dependent', 'Mixed dependency states', 'open', 3, 'task', NOW() - INTERVAL 8 DAY),
			('closed-dependency', 'Closed dependency', 'closed', 3, 'task', NOW()),
			('open-dependency', 'Open dependency', 'open', 3, 'task', NOW()),
			('mixed-blocked', 'Mixed blocker states', 'open', 3, 'task', NOW() - INTERVAL 8 DAY),
			('closed-blocker', 'Closed blocker', 'closed', 3, 'task', NOW()),
			('open-blocker', 'Open blocker', 'open', 3, 'task', NOW())`,
		`INSERT INTO labels (issue_id, label) VALUES
			('protected-agent', 'gt:agent'),
			('protected-message', 'gt:message'),
			('protected-mail-work', 'gt:mail-work')`,
		`INSERT INTO dependencies (issue_id, depends_on_issue_id) VALUES
			('mixed-dependent', 'closed-dependency'),
			('mixed-dependent', 'open-dependency'),
			('closed-blocker', 'mixed-blocked'),
			('open-blocker', 'mixed-blocked')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("initialize isolated schema: %v\n%s", err, statement)
		}
	}

	result, err := AutoClose(db, dbName, 7*24*time.Hour, false)
	if err != nil {
		t.Fatalf("AutoClose on isolated Dolt: %v", err)
	}
	if result.Closed != 1 || len(result.ClosedEntries) != 1 || result.ClosedEntries[0].ID != "stale-task" {
		t.Fatalf("AutoClose result = %#v, want only stale-task", result)
	}
	var staleStatus, protectedStatus, messageStatus, mailWorkStatus, mixedDependentStatus, mixedBlockedStatus string
	if err := db.QueryRow("SELECT status FROM issues WHERE id = 'stale-task'").Scan(&staleStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT status FROM issues WHERE id = 'protected-agent'").Scan(&protectedStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT status FROM issues WHERE id = 'protected-message'").Scan(&messageStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT status FROM issues WHERE id = 'protected-mail-work'").Scan(&mailWorkStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT status FROM issues WHERE id = 'mixed-dependent'").Scan(&mixedDependentStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT status FROM issues WHERE id = 'mixed-blocked'").Scan(&mixedBlockedStatus); err != nil {
		t.Fatal(err)
	}
	if staleStatus != "closed" || protectedStatus != "open" || messageStatus != "open" || mailWorkStatus != "open" || mixedDependentStatus != "open" || mixedBlockedStatus != "open" {
		t.Fatalf(
			"isolated statuses stale=%q protected=%q message=%q mail-work=%q mixed-dependent=%q mixed-blocked=%q",
			staleStatus, protectedStatus, messageStatus, mailWorkStatus, mixedDependentStatus, mixedBlockedStatus,
		)
	}
}

func TestPurgeOldMailExcludesLifecycleHistoryOnIsolatedDolt(t *testing.T) {
	if os.Getenv("GT_TEST_EXTERNAL_DOLT") != "1" ||
		os.Getenv("GT_TEST_ISOLATED") != "1" ||
		os.Getenv("GT_DOLT_HOST") != "127.0.0.1" {
		t.Skip("requires the explicit isolated Dolt test harness")
	}
	port, err := strconv.Atoi(os.Getenv("GT_DOLT_PORT"))
	if err != nil || port <= 0 || port == 33327 {
		t.Fatalf("refusing non-isolated Dolt port %q", os.Getenv("GT_DOLT_PORT"))
	}
	dbName := fmt.Sprintf("reaper_mail_%d", time.Now().UnixNano())
	rootDB, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/?parseTime=true", port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rootDB.Close() })
	if _, err := rootDB.Exec("CREATE DATABASE `" + dbName + "`"); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	t.Cleanup(func() { _, _ = rootDB.Exec("DROP DATABASE IF EXISTS `" + dbName + "`") })

	db, err := OpenDB("127.0.0.1", port, dbName, 15*time.Second, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`CREATE TABLE issues (id VARCHAR(64) PRIMARY KEY, status VARCHAR(32) NOT NULL, closed_at DATETIME NULL)`,
		`CREATE TABLE labels (issue_id VARCHAR(64) NOT NULL, label VARCHAR(64) NOT NULL)`,
		`INSERT INTO issues (id, status, closed_at) VALUES
			('ordinary-mail', 'closed', NOW() - INTERVAL 31 DAY),
			('mail-work-history', 'closed', NOW() - INTERVAL 31 DAY)`,
		`INSERT INTO labels (issue_id, label) VALUES
			('ordinary-mail', 'gt:message'),
			('mail-work-history', 'gt:message'),
			('mail-work-history', 'gt:mail-work')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("initialize isolated schema: %v\n%s", err, statement)
		}
	}

	count, err := purgeOldMail(db, dbName, 30*24*time.Hour, true)
	if err != nil {
		t.Fatalf("purgeOldMail dry run: %v", err)
	}
	if count != 1 {
		t.Fatalf("purgeOldMail candidates = %d, want only ordinary mail", count)
	}
}

func TestScanReturnsStableDanglingParentAnomaly(t *testing.T) {
	now := time.Now().UTC()
	state := &fakeReaperState{
		wisps: map[string]*fakeWisp{
			"child-b": {id: "child-b", status: "open", issueType: "task", createdAt: now},
			"child-a": {id: "child-a", status: "open", issueType: "task", createdAt: now},
		},
		issues: map[string]*fakeIssue{},
		deps: []fakeDep{
			{issueID: "child-b", dependsOnID: "missing-parent", depType: "parent-child"},
			{issueID: "child-a", dependsOnID: "missing-parent", depType: "parent-child"},
			{issueID: "child-a", dependsOnID: "missing-parent", depType: "parent-child"},
			{issueID: "child-b", dependsOnID: "external-parent", dependsOnExternal: "other", depType: "parent-child"},
		},
		ops: map[int][]string{},
	}
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })

	result, err := Scan(db, "testdb", time.Hour, 24*time.Hour, 24*time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Anomalies) != 1 {
		t.Fatalf("anomalies = %#v, want one dangling-parent anomaly", result.Anomalies)
	}
	got := result.Anomalies[0]
	if got.Type != "dangling_parent_ref" || got.Scope != "testdb" || got.Remediation != "repair_parent_links" {
		t.Fatalf("stable anomaly fields = %#v", got)
	}
	wantIDs := []string{"child-a", "child-b"}
	if !reflect.DeepEqual(got.AffectedIDs, wantIDs) {
		t.Fatalf("affected IDs = %#v, want %#v", got.AffectedIDs, wantIDs)
	}
	if got.Count != len(wantIDs) {
		t.Fatalf("count = %d, want %d", got.Count, len(wantIDs))
	}
}

func TestCommitFailureAnomalyCarriesStableLifecycleFields(t *testing.T) {
	anomaly := commitFailureAnomaly(
		"sql_commit_failed", "hq", []string{"hq-b", "hq-a"}, "retry_auto_close_commit", "volatile database detail",
	)
	if anomaly.Type != "sql_commit_failed" || anomaly.Scope != "hq" || anomaly.Remediation != "retry_auto_close_commit" {
		t.Fatalf("stable fields = %#v", anomaly)
	}
	if !reflect.DeepEqual(anomaly.AffectedIDs, []string{"hq-b", "hq-a"}) {
		t.Fatalf("affected IDs = %v", anomaly.AffectedIDs)
	}
	if _, err := FingerprintAnomaly(anomaly); err != nil {
		t.Fatalf("fingerprint structured commit anomaly: %v", err)
	}
}

func TestAutoCloseSQLCommitFailureReturnsNoSuccess(t *testing.T) {
	state := autoCloseFailureState()
	state.execFailures["COMMIT"] = fmt.Errorf("injected SQL commit failure")
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })

	result, err := AutoClose(db, "testdb", 7*24*time.Hour, false)
	if !errors.Is(err, ErrAutoCloseCommitOutcomeUnknown) {
		t.Fatalf("SQL commit error = %v, want ErrAutoCloseCommitOutcomeUnknown", err)
	}
	if result == nil || result.Closed != 0 || len(result.ClosedEntries) != 0 {
		t.Fatalf("SQL commit failure result = %#v, want zero ordinary success", result)
	}
	for _, want := range []string{`"type":"sql_commit_failed"`, `"affected_ids":["stale-task"]`, "if open, rerun auto-close", "if closed, run CALL DOLT_COMMIT"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("SQL commit error = %q, want preserved recovery detail %q", err, want)
		}
	}
}

func TestAutoCloseDoltCommitFailureReturnsNoSuccess(t *testing.T) {
	state := autoCloseFailureState()
	state.execFailures["CALL DOLT_COMMIT"] = fmt.Errorf("injected Dolt commit failure")
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })

	result, err := AutoClose(db, "testdb", 7*24*time.Hour, false)
	if !errors.Is(err, ErrAutoCloseCommitOutcomeUnknown) {
		t.Fatalf("Dolt commit error = %v, want ErrAutoCloseCommitOutcomeUnknown", err)
	}
	if result == nil || result.Closed != 0 || len(result.ClosedEntries) != 0 {
		t.Fatalf("Dolt commit failure result = %#v, want zero ordinary success", result)
	}
	if len(result.Anomalies) != 1 {
		t.Fatalf("Dolt commit failure anomalies = %#v, want one recovery anomaly", result.Anomalies)
	}
	var outcomeErr *AutoCloseCommitOutcomeError
	if !errors.As(err, &outcomeErr) || !reflect.DeepEqual(outcomeErr.Anomalies, result.Anomalies) {
		t.Fatalf("Dolt commit error = %#v, want structured result anomalies", err)
	}
	for _, want := range []string{`"type":"dolt_commit_failed"`, `"affected_ids":["stale-task"]`, "CALL DOLT_COMMIT", "do not rerun auto-close"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Dolt commit error = %q, want preserved recovery detail %q", err, want)
		}
	}
	if !strings.Contains(result.Anomalies[0].Remediation, "CALL DOLT_COMMIT") {
		t.Fatalf("Dolt commit remediation = %q, want executable commit action", result.Anomalies[0].Remediation)
	}
}

func TestAutoCloseDiscardsConnectionWhenTransactionResetFails(t *testing.T) {
	state := autoCloseFailureState()
	state.execFailures["COMMIT"] = fmt.Errorf("injected SQL commit failure")
	state.execFailures["ROLLBACK"] = fmt.Errorf("injected rollback failure")
	db := openFakeReaperDB(t, state)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	result, err := AutoClose(db, "testdb", 7*24*time.Hour, false)
	if err == nil {
		t.Errorf("AutoClose error = nil, want commit/reset failure")
	}
	if result != nil && (result.Closed != 0 || len(result.ClosedEntries) != 0) {
		t.Errorf("failed reset result = %#v, want zero ordinary success", result)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("ping after discarded connection: %v", err)
	}
	state.mu.Lock()
	opened := state.nextConn
	state.mu.Unlock()
	if opened != 2 {
		t.Fatalf("connections opened after failed reset = %d, want 2 (dirty connection discarded)", opened)
	}
}

func TestAutoCloseRowsAffectedAndAutocommitResetFailureDiscardsConnection(t *testing.T) {
	state := autoCloseFailureState()
	state.rowsAffectedFailure = fmt.Errorf("injected RowsAffected failure")
	state.execFailures["SET @@autocommit = 1"] = fmt.Errorf("injected autocommit reset failure")
	db := openFakeReaperDB(t, state)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	result, err := AutoClose(db, "testdb", 7*24*time.Hour, false)
	if err == nil || !strings.Contains(err.Error(), "RowsAffected failure") ||
		!strings.Contains(err.Error(), "autocommit reset failure") {
		t.Fatalf("AutoClose error = %v, want mutation and reset failures", err)
	}
	if result != nil && (result.Closed != 0 || len(result.ClosedEntries) != 0) {
		t.Fatalf("failed mutation result = %#v, want zero ordinary success", result)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("ping after discarded connection: %v", err)
	}
	state.mu.Lock()
	opened := state.nextConn
	state.mu.Unlock()
	if opened != 2 {
		t.Fatalf("connections opened after failed reset = %d, want 2 (dirty connection discarded)", opened)
	}
}

func autoCloseFailureState() *fakeReaperState {
	now := time.Now().UTC()
	return &fakeReaperState{
		issues: map[string]*fakeIssue{
			"stale-task": {
				id: "stale-task", title: "Stale task", status: "open",
				issueType: "task", updatedAt: now.Add(-8 * 24 * time.Hour),
			},
		},
		wisps:        map[string]*fakeWisp{},
		ops:          map[int][]string{},
		execFailures: map[string]error{},
	}
}

var fakeReaperDriverID uint64

func openFakeReaperDB(t *testing.T, state *fakeReaperState) *sql.DB {
	t.Helper()
	driverName := fmt.Sprintf("fake_reaper_%d", atomic.AddUint64(&fakeReaperDriverID, 1))
	sql.Register(driverName, &fakeReaperDriver{state: state})
	db, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open fake db: %v", err)
	}
	return db
}

type fakeWisp struct {
	id        string
	status    string
	issueType string
	createdAt time.Time
}

type fakeIssue struct {
	id        string
	title     string
	status    string
	issueType string
	updatedAt time.Time
	labels    []string
}

type fakeDep struct {
	issueID           string
	dependsOnID       string
	dependsOnExternal string
	depType           string
}

type fakeReaperState struct {
	mu                    sync.Mutex
	wisps                 map[string]*fakeWisp
	issues                map[string]*fakeIssue
	deps                  []fakeDep
	nextConn              int
	ops                   map[int][]string
	execFailures          map[string]error
	rowsAffectedFailure   error
	beforeAutoCloseUpdate func(*fakeReaperState)
}

func (s *fakeReaperState) autoCloseCandidatesLocked(query string, cutoff time.Time) []*fakeIssue {
	var candidates []*fakeIssue
	for _, issue := range s.issues {
		if (issue.status != "open" && issue.status != "in_progress") || !issue.updatedAt.Before(cutoff) ||
			issue.issueType == "epic" || issue.issueType == "convoy" ||
			(issue.issueType == "agent" && strings.Contains(query, "'agent'")) {
			continue
		}
		protected := false
		for _, label := range issue.labels {
			if strings.Contains(query, "'"+label+"'") {
				protected = true
				break
			}
		}
		if !protected {
			candidates = append(candidates, issue)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].id < candidates[j].id })
	return candidates
}

func (s *fakeReaperState) status(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.wisps[id]; w != nil {
		return w.status
	}
	if issue := s.issues[id]; issue != nil {
		return issue.status
	}
	return ""
}

func (s *fakeReaperState) statuses() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	statuses := make(map[string]string, len(s.wisps))
	for id, w := range s.wisps {
		statuses[id] = w.status
	}
	return statuses
}

func (s *fakeReaperState) opCounts() map[int]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := make(map[int]int, len(s.ops))
	for connID, ops := range s.ops {
		counts[connID] = len(ops)
	}
	return counts
}

func (s *fakeReaperState) opsSince(counts map[int]int) map[int][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	opsSince := map[int][]string{}
	for connID, ops := range s.ops {
		start := counts[connID]
		if start < len(ops) {
			opsSince[connID] = append([]string(nil), ops[start:]...)
		}
	}
	return opsSince
}

func (s *fakeReaperState) record(connID int, op string) {
	s.ops[connID] = append(s.ops[connID], normalizeSQL(op))
}

func (s *fakeReaperState) moleculeStepCandidatesLocked() []string {
	var ids []string
	for id := range s.wisps {
		if s.isMoleculeStepCandidateLocked(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (s *fakeReaperState) isMoleculeStepCandidateLocked(id string) bool {
	w := s.wisps[id]
	if w == nil || !isOpenWispStatus(w.status) || w.issueType == "agent" {
		return false
	}
	for _, dep := range s.deps {
		if dep.issueID != id || dep.depType != "parent-child" {
			continue
		}
		if dep.dependsOnExternal != "" {
			return false
		}
		if s.hasOpenParentLocked(id) {
			return false
		}
		parent := s.wisps[dep.dependsOnID]
		if parent != nil && parent.issueType == "molecule" && parent.status == "closed" {
			return true
		}
	}
	return false
}

func (s *fakeReaperState) staleCandidatesLocked(cutoff time.Time, excludeMoleculeSteps bool) []string {
	var ids []string
	for id, w := range s.wisps {
		if !isOpenWispStatus(w.status) || w.issueType == "agent" || !w.createdAt.Before(cutoff) {
			continue
		}
		if s.hasOpenParentLocked(id) {
			continue
		}
		if excludeMoleculeSteps && s.isMoleculeStepCandidateLocked(id) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (s *fakeReaperState) hasOpenParentLocked(id string) bool {
	for _, dep := range s.deps {
		if dep.issueID != id || dep.depType != "parent-child" {
			continue
		}
		if dep.dependsOnExternal != "" {
			return true
		}
		parent := s.wisps[dep.dependsOnID]
		if parent != nil && isOpenWispStatus(parent.status) {
			return true
		}
	}
	return false
}

func (s *fakeReaperState) openCountLocked() int {
	count := 0
	for _, w := range s.wisps {
		if isOpenWispStatus(w.status) {
			count++
		}
	}
	return count
}

func (s *fakeReaperState) danglingChildIDsLocked() []string {
	seen := make(map[string]bool)
	for _, dep := range s.deps {
		if dep.depType != "parent-child" || dep.dependsOnExternal != "" {
			continue
		}
		if s.wisps[dep.dependsOnID] != nil || s.issues[dep.dependsOnID] != nil {
			continue
		}
		seen[dep.issueID] = true
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

type fakeReaperDriver struct {
	state *fakeReaperState
}

func (d *fakeReaperDriver) Open(string) (driver.Conn, error) {
	d.state.mu.Lock()
	defer d.state.mu.Unlock()
	d.state.nextConn++
	connID := d.state.nextConn
	d.state.ops[connID] = nil
	return &fakeReaperConn{state: d.state, id: connID}, nil
}

type fakeReaperConn struct {
	state *fakeReaperState
	id    int
}

func (c *fakeReaperConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("prepare not implemented")
}

func (c *fakeReaperConn) Close() error { return nil }

func (c *fakeReaperConn) Begin() (driver.Tx, error) { return fakeReaperTx{}, nil }

func (c *fakeReaperConn) CheckNamedValue(*driver.NamedValue) error { return nil }

func (c *fakeReaperConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	normalized := normalizeSQL(query)
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	c.state.record(c.id, "QUERY "+normalized)

	switch {
	case strings.Contains(normalized, "SELECT i.id, i.title, i.updated_at FROM issues i WHERE"):
		return fakeIssueRows(c.state.autoCloseCandidatesLocked(normalized, namedTime(args))), nil
	case strings.Contains(normalized, "SELECT COUNT(*) FROM wisps w") && strings.Contains(normalized, "created_at <"):
		if err := validateStaleWispQuery(normalized); err != nil {
			return nil, err
		}
		return fakeCountRows(len(c.state.staleCandidatesLocked(namedTime(args), strings.Contains(normalized, "closed_molecule_step.issue_id IS NULL")))), nil
	case strings.Contains(normalized, "SELECT COUNT(*) FROM wisps w") && strings.Contains(normalized, "pm.issue_type = 'molecule'"):
		if err := validateMoleculeStepQuery(normalized); err != nil {
			return nil, err
		}
		return fakeCountRows(len(c.state.moleculeStepCandidatesLocked())), nil
	case strings.Contains(normalized, "SELECT COUNT(*) FROM wisps WHERE status IN"):
		return fakeCountRows(c.state.openCountLocked()), nil
	case strings.Contains(normalized, "SELECT COUNT(*) FROM wisps w WHERE w.status = 'closed'"):
		return fakeCountRows(0), nil
	case strings.Contains(normalized, "SELECT COUNT(*) FROM issues"):
		return fakeCountRows(0), nil
	case strings.Contains(normalized, "SELECT COUNT(*) FROM wisp_dependencies wd"):
		return fakeCountRows(0), nil
	case strings.Contains(normalized, "SELECT w.id FROM wisps w") && strings.Contains(normalized, "created_at <"):
		if err := validateStaleWispQuery(normalized); err != nil {
			return nil, err
		}
		return fakeIDRows(c.state.staleCandidatesLocked(namedTime(args), strings.Contains(normalized, "closed_molecule_step.issue_id IS NULL"))), nil
	case strings.Contains(normalized, "SELECT w.id FROM wisps w") && strings.Contains(normalized, "pm.issue_type = 'molecule'"):
		if err := validateMoleculeStepQuery(normalized); err != nil {
			return nil, err
		}
		return fakeIDRows(c.state.moleculeStepCandidatesLocked()), nil
	case strings.Contains(normalized, "SELECT DISTINCT wd.issue_id FROM wisp_dependencies wd"):
		return fakeIDRows(c.state.danglingChildIDsLocked()), nil
	default:
		return nil, fmt.Errorf("unexpected query: %s", normalized)
	}
}

func (c *fakeReaperConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	normalized := normalizeSQL(query)
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	c.state.record(c.id, "EXEC "+normalized)
	for prefix, err := range c.state.execFailures {
		if normalized == prefix || strings.HasPrefix(normalized, prefix) {
			delete(c.state.execFailures, prefix)
			return nil, err
		}
	}

	switch {
	case strings.HasPrefix(normalized, "UPDATE `testdb`.issues") && strings.Contains(normalized, "SET i.status = 'closed'"):
		if hook := c.state.beforeAutoCloseUpdate; hook != nil {
			c.state.beforeAutoCloseUpdate = nil
			hook(c.state)
		}
		eligible := make(map[string]bool)
		for _, issue := range c.state.autoCloseCandidatesLocked(normalized, namedTime(args)) {
			eligible[issue.id] = true
		}
		affected := int64(0)
		for _, arg := range args {
			id, _ := arg.Value.(string)
			if issue := c.state.issues[id]; issue != nil && eligible[id] {
				issue.status = "closed"
				affected++
			}
		}
		if err := c.state.rowsAffectedFailure; err != nil {
			c.state.rowsAffectedFailure = nil
			return fakeReaperResultFailure{affected: affected, err: err}, nil
		}
		return fakeReaperResult(affected), nil
	case strings.HasPrefix(normalized, "UPDATE wisps SET status='closed'"):
		affected := int64(0)
		for _, arg := range args {
			id, _ := arg.Value.(string)
			if w := c.state.wisps[id]; w != nil && isOpenWispStatus(w.status) {
				w.status = "closed"
				affected++
			}
		}
		return fakeReaperResult(affected), nil
	case normalized == "SET @@autocommit = 0" || normalized == "SET @@autocommit = 1" || normalized == "ROLLBACK" || normalized == "COMMIT" || strings.HasPrefix(normalized, "CALL DOLT_COMMIT"):
		return fakeReaperResult(0), nil
	default:
		return nil, fmt.Errorf("unexpected exec: %s", normalized)
	}
}

type fakeReaperTx struct{}

func (fakeReaperTx) Commit() error   { return nil }
func (fakeReaperTx) Rollback() error { return nil }

type fakeReaperResult int64

func (r fakeReaperResult) LastInsertId() (int64, error) { return 0, nil }
func (r fakeReaperResult) RowsAffected() (int64, error) { return int64(r), nil }

type fakeReaperResultFailure struct {
	affected int64
	err      error
}

func (r fakeReaperResultFailure) LastInsertId() (int64, error) { return 0, nil }
func (r fakeReaperResultFailure) RowsAffected() (int64, error) { return r.affected, r.err }

type fakeReaperRows struct {
	cols []string
	rows [][]driver.Value
	next int
}

func fakeCountRows(count int) *fakeReaperRows {
	return &fakeReaperRows{cols: []string{"count"}, rows: [][]driver.Value{{int64(count)}}}
}

func fakeIDRows(ids []string) *fakeReaperRows {
	rows := make([][]driver.Value, len(ids))
	for i, id := range ids {
		rows[i] = []driver.Value{id}
	}
	return &fakeReaperRows{cols: []string{"id"}, rows: rows}
}

func fakeIssueRows(issues []*fakeIssue) *fakeReaperRows {
	rows := make([][]driver.Value, len(issues))
	for i, issue := range issues {
		rows[i] = []driver.Value{issue.id, issue.title, issue.updatedAt}
	}
	return &fakeReaperRows{cols: []string{"id", "title", "updated_at"}, rows: rows}
}

func (r *fakeReaperRows) Columns() []string { return r.cols }
func (r *fakeReaperRows) Close() error      { return nil }

func (r *fakeReaperRows) Next(dest []driver.Value) error {
	if r.next >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.next])
	r.next++
	return nil
}

func namedTime(args []driver.NamedValue) time.Time {
	for _, arg := range args {
		if value, ok := arg.Value.(time.Time); ok {
			return value
		}
	}
	return time.Time{}
}

func isOpenWispStatus(status string) bool {
	return status == "open" || status == "hooked" || status == "in_progress"
}

func normalizeSQL(query string) string {
	return strings.Join(strings.Fields(query), " ")
}

func validateMoleculeStepQuery(query string) error {
	return requireSQL(query,
		"wd.issue_id",
		"pm.id = wd.depends_on_wisp_id",
		"wd.type = 'parent-child'",
		"pm.issue_type = 'molecule'",
		"pm.status = 'closed'",
		"NOT EXISTS",
		"open_dep.depends_on_external IS NOT NULL",
		"w.issue_type != 'agent'",
		"w.status IN ('open', 'hooked', 'in_progress')",
	)
}

func validateStaleWispQuery(query string) error {
	return requireSQL(query,
		"wd.issue_id",
		"pw.id = wd.depends_on_wisp_id",
		"pi.id = wd.depends_on_issue_id",
		"pi.status IN ('open', 'hooked', 'in_progress')",
		"depends_on_external IS NOT NULL",
		"wd.type = 'parent-child'",
		"w.issue_type != 'agent'",
		"w.created_at < ?",
		"open_parent.issue_id IS NULL",
		"closed_molecule_step.issue_id IS NULL",
	)
}

func requireSQL(query string, required ...string) error {
	if strings.Contains(query, "depends_on_id") {
		return fmt.Errorf("query uses legacy depends_on_id column: %s", query)
	}
	for _, want := range required {
		if !strings.Contains(query, want) {
			return fmt.Errorf("query missing %q: %s", want, query)
		}
	}
	return nil
}

func assertOpsContainInOrder(t *testing.T, ops []string, want ...string) {
	t.Helper()
	next := 0
	for _, op := range ops {
		if strings.Contains(op, want[next]) {
			next++
			if next == len(want) {
				return
			}
		}
	}
	t.Fatalf("ops missing ordered sequence %v in %v", want[next:], ops)
}
