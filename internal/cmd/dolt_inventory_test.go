package cmd

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

func TestInspectDoltCleanupListenersFailsWithoutCleanClaim(t *testing.T) {
	want := errors.New("listener discovery failed")
	var out bytes.Buffer
	inventory, err := inspectDoltCleanupListeners(&out, func() ([]doltserver.LocalDoltServer, error) {
		return nil, want
	})
	if !errors.Is(err, want) || inventory != nil {
		t.Fatalf("inventory = %v, error = %v, want nil and %v", inventory, err, want)
	}
	got := out.String()
	if !strings.Contains(got, "local listener inventory failed") || strings.Contains(got, "No orphaned") || strings.Contains(got, "clean") {
		t.Fatalf("inventory failure output made a false clean claim: %q", got)
	}
}

func TestRenderNoOrphanedTestDatabasesStatesDatabaseOnlyScope(t *testing.T) {
	var out bytes.Buffer
	renderNoOrphanedTestDatabases(&out, nil)
	got := out.String()
	for _, want := range []string{"No orphaned test databases found.", "Process cleanup was not performed."} {
		if !strings.Contains(got, want) {
			t.Fatalf("cleanup output missing %q: %q", want, got)
		}
	}
}

func TestRenderDoltCleanupProcessScopePointsToExactTestLeakPreview(t *testing.T) {
	inventory := []doltserver.LocalDoltServer{
		{Class: doltserver.DoltServerConfiguredPortImposter, OwnerPath: "/private/imposter", ProcessToken: "imposter-token"},
		{Class: doltserver.DoltServerOwnedTownLeak, OwnerPath: "/private/town", ProcessToken: "town-token"},
		{Class: doltserver.DoltServerOwnedTestLeak},
		{Class: doltserver.DoltServerOwnedTestLeak},
		{Class: doltserver.DoltServerUnknown, OwnerPath: "/private/unknown", ProcessToken: "unknown-token"},
	}
	var out bytes.Buffer
	renderDoltCleanupProcessScope(&out, inventory)
	got := out.String()
	for _, want := range []string{
		"Process cleanup was not performed.",
		"Report-only listener counts: configured-port-imposter=1 owned-town-leak=1 unknown=1.",
		"2 positively test-owned Dolt listener leak(s) found.",
		"Preview exact test-leak cleanup: gt dolt cleanup-test-leaks",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cleanup output missing %q: %q", want, got)
		}
	}
	for _, secret := range []string{"/private/imposter", "imposter-token", "/private/town", "town-token", "/private/unknown", "unknown-token"} {
		if strings.Contains(got, secret) {
			t.Fatalf("cleanup output exposed listener custody %q: %q", secret, got)
		}
	}
}

func TestRenderDoltCleanupListenerReportInventoriesAtReportTime(t *testing.T) {
	var out bytes.Buffer
	calls := 0
	err := renderDoltCleanupListenerReport(&out, true, func() ([]doltserver.LocalDoltServer, error) {
		calls++
		return []doltserver.LocalDoltServer{{Class: doltserver.DoltServerUnknown}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("listener inventory calls = %d, want 1 at report time", calls)
	}
	for _, want := range []string{
		"No orphaned test databases found.",
		"Report-only listener counts: configured-port-imposter=0 owned-town-leak=0 unknown=1.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("cleanup report missing %q: %q", want, out.String())
		}
	}
}

func TestRunDoltCleanupReturnsNonzeroOnRefusedOrphan(t *testing.T) {
	townRoot := t.TempDir()
	dbPath := filepath.Join(townRoot, ".dolt-data", "testdb_refused", ".dolt", "noms")
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dbPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dbPath, "manifest"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dbPath, "data"), make([]byte, 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GT_DOLT_PORT", "1")
	t.Chdir(townRoot)
	oldDry, oldForce := doltCleanupDry, doltCleanupForce
	doltCleanupDry, doltCleanupForce = false, false
	t.Cleanup(func() { doltCleanupDry, doltCleanupForce = oldDry, oldForce })

	var runErr error
	output := captureStdout(t, func() { runErr = runDoltCleanup(nil, nil) })
	if runErr == nil {
		t.Fatal("runDoltCleanup() succeeded after refusing an orphan")
	}
	if _, err := os.Stat(filepath.Join(townRoot, ".dolt-data", "testdb_refused")); err != nil {
		t.Fatalf("refused orphan was not preserved: %v", err)
	}
	if !strings.Contains(output, "Process cleanup was not performed.") {
		t.Fatalf("final listener report did not run: %q", output)
	}
	if strings.Contains(output, "✓ Removed 0/1") {
		t.Fatalf("partial cleanup rendered a success marker: %q", output)
	}
}

func TestRunDoltCleanupStopsAfterWriteProbeFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX dolt stub")
	}

	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"testdb_a", "testdb_b"} {
		noms := filepath.Join(townRoot, ".dolt-data", name, ".dolt", "noms")
		if err := os.MkdirAll(noms, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(noms, "manifest"), []byte("test"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	t.Setenv("GT_DOLT_PORT", strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))

	binDir := t.TempDir()
	stub := `#!/bin/sh
case "$*" in
  *"SHOW TABLES"*) exit 0 ;;
  *"DROP DATABASE"*) exit 0 ;;
  *"DELETE FROM dolt_branch_control"*) exit 0 ;;
  *"__gt_health_probe"*) printf 'probe unavailable\n' >&2; exit 1 ;;
esac
printf 'unexpected dolt args: %s\n' "$*" >&2
exit 2
`
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	t.Chdir(townRoot)
	oldDry, oldForce := doltCleanupDry, doltCleanupForce
	doltCleanupDry, doltCleanupForce = false, false
	t.Cleanup(func() { doltCleanupDry, doltCleanupForce = oldDry, oldForce })

	var runErr error
	output := captureStdout(t, func() { runErr = runDoltCleanup(nil, nil) })
	if runErr == nil || !strings.Contains(runErr.Error(), "write probe") {
		t.Fatalf("runDoltCleanup() error = %v, want write-probe failure", runErr)
	}
	if _, err := os.Stat(filepath.Join(townRoot, ".dolt-data", "testdb_a")); !os.IsNotExist(err) {
		t.Fatalf("first orphan was not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(townRoot, ".dolt-data", "testdb_b")); err != nil {
		t.Fatalf("later orphan was not preserved: %v", err)
	}
	if strings.Contains(output, "Removed 2/2") {
		t.Fatalf("cleanup continued after an unverified write probe: %q", output)
	}
}

func TestSummarizeDoltInventoryUsesActionableClassification(t *testing.T) {
	inventory := []doltserver.LocalDoltServer{
		{Class: doltserver.DoltServerCanonical},
		{Class: doltserver.DoltServerConfiguredPortImposter},
		{Class: doltserver.DoltServerOwnedTownLeak},
		{Class: doltserver.DoltServerOwnedTestLeak},
		{Class: doltserver.DoltServerUnknown},
	}

	actionable, unknown := summarizeDoltInventory(inventory)
	if actionable != 3 || unknown != 1 {
		t.Fatalf("summarizeDoltInventory() = (%d, %d), want (3, 1)", actionable, unknown)
	}
}

func TestWriteTestLeakPreviewUsesMode0600(t *testing.T) {
	runtimeDir := filepath.Join(t.TempDir(), ".runtime")
	if err := os.Mkdir(runtimeDir, 0755); err != nil {
		t.Fatalf("mkdir permissive runtime dir: %v", err)
	}
	path := filepath.Join(runtimeDir, "preview.json")
	selections := []doltserver.TestLeakSelection{{
		PID: 701, Port: 4701, Class: doltserver.DoltServerOwnedTestLeak,
		OwnershipToken: strings.Repeat("ab", 32),
	}}
	if err := writeTestLeakPreview(path, selections); err != nil {
		t.Fatalf("writeTestLeakPreview: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat preview: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("preview mode = %04o, want 0600", got)
	}
	dirInfo, err := os.Stat(runtimeDir)
	if err != nil {
		t.Fatalf("stat preview directory: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0700 {
		t.Fatalf("preview directory mode = %04o, want 0700", got)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read preview directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("atomic preview left temporary files: %v", entries)
	}
	got, err := readTestLeakPreview(path)
	if err != nil {
		t.Fatalf("readTestLeakPreview: %v", err)
	}
	if len(got) != 1 || got[0] != selections[0] {
		t.Fatalf("preview selections = %#v, want %#v", got, selections)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatalf("chmod preview: %v", err)
	}
	if _, err := readTestLeakPreview(path); err == nil {
		t.Fatal("readTestLeakPreview accepted a non-private receipt")
	}
}

func TestCleanupTestLeaksCommandIsPreviewDefaultApplyExplicit(t *testing.T) {
	if doltCleanupTestLeaksCmd.Use != "cleanup-test-leaks" {
		t.Fatalf("Use = %q", doltCleanupTestLeaksCmd.Use)
	}
	flag := doltCleanupTestLeaksCmd.Flags().Lookup("apply")
	if flag == nil || flag.DefValue != "false" {
		t.Fatalf("apply flag = %#v, want explicit false default", flag)
	}
}

func TestFormatDoltInventoryLineDoesNotExposeOwnerPath(t *testing.T) {
	server := doltserver.LocalDoltServer{
		DoltListener: doltserver.DoltListener{PID: 77, Port: 3307},
		Class:        doltserver.DoltServerConfiguredPortImposter,
		OwnerPath:    "/private/production/secret/data",
	}

	got := formatDoltInventoryLine(server)
	if strings.Contains(got, server.OwnerPath) || strings.Contains(got, "production") {
		t.Fatalf("inventory output exposed owner path: %q", got)
	}
	for _, want := range []string{"PID 77", "port 3307", "configured-port-imposter"} {
		if !strings.Contains(got, want) {
			t.Fatalf("inventory output %q missing %q", got, want)
		}
	}
}
