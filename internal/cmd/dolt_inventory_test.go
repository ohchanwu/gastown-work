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

func TestRunDoltCleanupRefusesDestructiveWorkWhileServerRuns(t *testing.T) {
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

	t.Chdir(townRoot)
	oldDry, oldForce := doltCleanupDry, doltCleanupForce
	doltCleanupDry, doltCleanupForce = false, false
	t.Cleanup(func() { doltCleanupDry, doltCleanupForce = oldDry, oldForce })

	var runErr error
	output := captureStdout(t, func() { runErr = runDoltCleanup(nil, nil) })
	if runErr == nil || !strings.Contains(runErr.Error(), "requires a stopped Dolt server") {
		t.Fatalf("runDoltCleanup() error = %v, want offline-only refusal", runErr)
	}
	for _, name := range []string{"testdb_a", "testdb_b"} {
		if _, err := os.Stat(filepath.Join(townRoot, ".dolt-data", name)); err != nil {
			t.Fatalf("live cleanup did not preserve %s: %v", name, err)
		}
	}
	if output != "" {
		t.Fatalf("live cleanup produced mutation progress before refusal: %q", output)
	}
}

func TestRunDoltCleanupStopsWhenDatabaseInventoryFails(t *testing.T) {
	townRoot := t.TempDir()
	t.Setenv("GT_DOLT_PORT", "1")
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	noms := filepath.Join(townRoot, ".dolt-data", "testdb_unverified", ".dolt", "noms")
	if err := os.MkdirAll(noms, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(noms, "manifest"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(townRoot)
	oldDry, oldForce := doltCleanupDry, doltCleanupForce
	doltCleanupDry, doltCleanupForce = false, false
	oldList := listDatabasesForCleanup
	listDatabasesForCleanup = func(string) ([]string, error) {
		return nil, errors.New("catalog unavailable")
	}
	t.Cleanup(func() {
		doltCleanupDry, doltCleanupForce = oldDry, oldForce
		listDatabasesForCleanup = oldList
	})

	err := runDoltCleanup(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "listing databases for cleanup safety") {
		t.Fatalf("runDoltCleanup() error = %v, want inventory failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(townRoot, ".dolt-data", "testdb_unverified")); statErr != nil {
		t.Fatalf("orphan was not preserved: %v", statErr)
	}
}

func TestRunDoltCleanupRemovesAllOrphansWhenServerOffline(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"testdb_offline_a", "testdb_offline_b"} {
		noms := filepath.Join(townRoot, ".dolt-data", name, ".dolt", "noms")
		if err := os.MkdirAll(noms, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(noms, "manifest"), []byte("test"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Chdir(townRoot)
	t.Setenv("GT_DOLT_PORT", "1")
	oldDry, oldForce := doltCleanupDry, doltCleanupForce
	doltCleanupDry, doltCleanupForce = false, false
	t.Cleanup(func() { doltCleanupDry, doltCleanupForce = oldDry, oldForce })

	var runErr error
	output := captureStdout(t, func() { runErr = runDoltCleanup(nil, nil) })
	if runErr != nil {
		t.Fatalf("runDoltCleanup() error = %v; output = %q", runErr, output)
	}
	for _, name := range []string{"testdb_offline_a", "testdb_offline_b"} {
		if _, err := os.Stat(filepath.Join(townRoot, ".dolt-data", name)); !os.IsNotExist(err) {
			t.Fatalf("offline orphan %s still exists: %v", name, err)
		}
	}
}

func TestRunDoltCleanupDryRunPreservesPendingReceipt(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(townRoot, ".runtime", "dolt-database-cleanup", "testdb_pending.json")
	if err := os.MkdirAll(filepath.Dir(receiptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receiptPath, []byte("{\"version\":3,\"database\":\"testdb_pending\",\"force\":false,\"phase\":\"prepared\",\"incarnation\":\"dolt-root:0123456789abcdefghijklmnopqrstuv/manifest-sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Chdir(townRoot)
	t.Setenv("GT_DOLT_PORT", "1")
	oldDry, oldForce := doltCleanupDry, doltCleanupForce
	doltCleanupDry, doltCleanupForce = true, false
	t.Cleanup(func() { doltCleanupDry, doltCleanupForce = oldDry, oldForce })

	var runErr error
	output := captureStdout(t, func() { runErr = runDoltCleanup(nil, nil) })
	if runErr != nil {
		t.Fatalf("runDoltCleanup() error = %v; output = %q", runErr, output)
	}
	if _, err := os.Stat(receiptPath); err != nil {
		t.Fatalf("dry-run removed pending receipt: %v", err)
	}
	if !strings.Contains(output, "pending database cleanup") {
		t.Fatalf("dry-run output omitted pending cleanup: %q", output)
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
