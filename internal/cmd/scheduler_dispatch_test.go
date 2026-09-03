package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/scheduler/capacity"
)

func installFakeBD(t *testing.T, script string) {
	t.Helper()
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir fake bd bin: %v", err)
	}
	fakeBD := filepath.Join(binDir, "bd")
	if err := os.WriteFile(fakeBD, []byte(script), 0755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func setupSchedulerScanFailureTown(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	for _, dir := range []string{
		filepath.Join(townRoot, "mayor"),
		filepath.Join(townRoot, ".beads"),
		filepath.Join(townRoot, "rig", ".beads"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	installFakeBD(t, `#!/bin/sh
case "$BEADS_DIR" in
  */rig/.beads) echo "scan failed" >&2; exit 7 ;;
  *) printf '[]\n'; exit 0 ;;
esac
`)
	return townRoot
}

func TestDispatchScheduledWorkReportsHeldLock(t *testing.T) {
	townRoot := t.TempDir()
	runtimeDir := filepath.Join(townRoot, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}
	lockFile := filepath.Join(runtimeDir, "scheduler-dispatch.lock")
	lock := flock.New(lockFile)
	locked, err := lock.TryLock()
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	if !locked {
		t.Fatal("test could not acquire scheduler dispatch lock")
	}
	t.Cleanup(func() { _ = lock.Unlock() })

	_, err = dispatchScheduledWork(context.Background(), townRoot, "test", 1, false)
	if err == nil {
		t.Fatal("dispatchScheduledWork succeeded with held scheduler lock")
	}
	if !strings.Contains(err.Error(), "scheduler dispatch already in progress") || !strings.Contains(err.Error(), lockFile) {
		t.Fatalf("error = %q, want explicit held lock reason with path", err.Error())
	}
}

func TestValidateDryRunDispatchPlanMarksAllInvalidAsValidation(t *testing.T) {
	townRoot := t.TempDir()
	writeJSONFile(t, filepath.Join(townRoot, "mayor", "rigs.json"), &config.RigsConfig{
		Version: config.CurrentRigsVersion,
		Rigs: map[string]config.RigEntry{
			"testrig": {BeadsConfig: &config.BeadsConfig{Prefix: "gt"}},
		},
	})

	plan := validateDryRunDispatchPlan(townRoot, capacity.DispatchPlan{
		ToDispatch: []capacity.PendingBead{{ID: "ctx-1", WorkBeadID: "hq-one", TargetRig: "testrig"}},
		Reason:     "ready",
	})

	if len(plan.ToDispatch) != 0 || plan.Skipped != 1 || plan.Reason != "validation" {
		t.Fatalf("validated plan = %+v, want no dispatch, skipped=1, reason=validation", plan)
	}
}

func TestListAllSlingContextRecordsFailsOnPartialScanFailure(t *testing.T) {
	townRoot := setupSchedulerScanFailureTown(t)

	_, err := listAllSlingContextRecords(townRoot)
	if err == nil {
		t.Fatal("partial sling-context scan failure should fail closed")
	}
	if !strings.Contains(err.Error(), "listing sling contexts") || !strings.Contains(err.Error(), filepath.Join("rig", ".beads")) {
		t.Fatalf("error = %q, want explicit context scan failure", err.Error())
	}
}

func TestListAllSlingContextRecordsScansRigsConcurrently(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"} {
		if err := os.MkdirAll(filepath.Join(townRoot, "rig-"+name, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	scanLog := filepath.Join(t.TempDir(), "scans")
	installFakeBD(t, `#!/bin/sh
if [ "$1" = "--allow-stale" ] && [ "$2" = "version" ]; then
  exit 0
fi
sleep 0.3
printf '%s\n' "$BEADS_DIR" >> "$GT_SLING_SCAN_LOG"
printf '[]\n'
`)
	t.Setenv("GT_SLING_SCAN_LOG", scanLog)

	started := time.Now()
	records, err := listAllSlingContextRecordsContext(context.Background(), townRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("records = %d, want 0", len(records))
	}
	if elapsed := time.Since(started); elapsed >= 2500*time.Millisecond {
		t.Fatalf("13 sling-context scans took %s, want bounded concurrency under 2.5s", elapsed.Round(time.Millisecond))
	}
	data, err := os.ReadFile(scanLog)
	if err != nil {
		t.Fatal(err)
	}
	if scans := len(strings.Fields(string(data))); scans != 13 {
		t.Fatalf("sling-context scans = %d, want 13 unique directories", scans)
	}
}

func TestListAllSlingContextRecordsScansResolvedDatabaseOnce(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(townRoot, "rig", "mayor", "rig", ".beads")
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canonical, filepath.Join(townRoot, "rig", ".beads")); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	scanLog := filepath.Join(t.TempDir(), "scans")
	installFakeBD(t, `#!/bin/sh
if [ "$1" = "--allow-stale" ] && [ "$2" = "version" ]; then
  exit 0
fi
printf '%s\n' "$BEADS_DIR" >> "$GT_SLING_SCAN_LOG"
printf '[]\n'
`)
	t.Setenv("GT_SLING_SCAN_LOG", scanLog)

	if _, err := listAllSlingContextRecordsContext(context.Background(), townRoot); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(scanLog)
	if err != nil {
		t.Fatal(err)
	}
	if scans := len(strings.Fields(string(data))); scans != 2 {
		t.Fatalf("sling-context scans = %d, want town plus one resolved rig database; dirs:\n%s", scans, data)
	}
}

func TestListAllSlingContextRecordsKeepsResolvedDatabaseAfterAliasRetarget(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(townRoot, "rig", "mayor", "rig", ".beads")
	replacement := filepath.Join(townRoot, "replacement", ".beads")
	for _, dir := range []string{canonical, replacement} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(townRoot, "rig", ".beads")
	if err := os.Symlink(canonical, alias); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}

	ready := filepath.Join(t.TempDir(), "ready")
	release := filepath.Join(t.TempDir(), "release")
	scanLog := filepath.Join(t.TempDir(), "scans")
	installFakeBD(t, `#!/bin/sh
if [ "$1" = "--allow-stale" ] && [ "$2" = "version" ]; then
  exit 0
fi
if [ "$BEADS_DIR" = "$GT_SLING_ALIAS" ] || [ "$BEADS_DIR" = "$GT_SLING_CANONICAL" ]; then
  : > "$GT_SLING_READY"
  while [ ! -f "$GT_SLING_RELEASE" ]; do sleep 0.01; done
  physical=$(cd "$BEADS_DIR" && pwd -P) || exit 8
  printf '%s\n' "$physical" >> "$GT_SLING_SCAN_LOG"
  printf '[{"id":"ctx-canonical","title":"context","description":"test","status":"open","issue_type":"task","labels":["gt:sling-context"]}]\n'
  exit 0
fi
printf '[]\n'
`)
	t.Setenv("GT_SLING_ALIAS", alias)
	t.Setenv("GT_SLING_CANONICAL", canonical)
	t.Setenv("GT_SLING_READY", ready)
	t.Setenv("GT_SLING_RELEASE", release)
	t.Setenv("GT_SLING_SCAN_LOG", scanLog)

	type scanOutcome struct {
		records []slingContextRecord
		err     error
	}
	done := make(chan scanOutcome, 1)
	go func() {
		records, err := listAllSlingContextRecordsContext(context.Background(), townRoot)
		done <- scanOutcome{records: records, err: err}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for alias scan")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	outcome := <-done
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if len(outcome.records) != 1 {
		t.Fatalf("records = %d, want 1", len(outcome.records))
	}
	if outcome.records[0].beadsDir != canonical {
		t.Fatalf("record beads dir = %q, want resolved canonical %q", outcome.records[0].beadsDir, canonical)
	}
	data, err := os.ReadFile(scanLog)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != canonical {
		t.Fatalf("scanned physical database = %q, want %q", got, canonical)
	}
}

func TestListAllSlingContextRecordsCancelsSiblingScansOnFailure(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		if err := os.MkdirAll(filepath.Join(townRoot, "rig-"+name, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	startedLog := filepath.Join(t.TempDir(), "started")
	installFakeBD(t, `#!/bin/sh
if [ "$1" = "--allow-stale" ] && [ "$2" = "version" ]; then
  exit 0
fi
printf '%s\n' "$BEADS_DIR" >> "$GT_SLING_STARTED_LOG"
case "$BEADS_DIR" in
  */rig-a/.beads) echo "scan failed" >&2; exit 7 ;;
  *) sleep 4; printf '[]\n' ;;
esac
`)
	t.Setenv("GT_SLING_STARTED_LOG", startedLog)

	started := time.Now()
	_, err := listAllSlingContextRecordsContext(context.Background(), townRoot)
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), filepath.Join("rig-a", ".beads")) {
		t.Fatalf("error = %v, want original rig-a scan failure", err)
	}
	if elapsed >= 2500*time.Millisecond {
		t.Fatalf("fatal scan returned after %s, want sibling cancellation under 2.5s", elapsed.Round(time.Millisecond))
	}
	data, err := os.ReadFile(startedLog)
	if err != nil {
		t.Fatal(err)
	}
	if scans := len(strings.Fields(string(data))); scans > slingContextScanConcurrency {
		t.Fatalf("started scans = %d, want at most %d before cancellation; dirs:\n%s", scans, slingContextScanConcurrency, data)
	}
}

func TestAreScheduledFailsClosedOnContextScanFailure(t *testing.T) {
	townRoot := setupSchedulerScanFailureTown(t)
	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(townRoot); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCWD) })

	got := areScheduled([]string{"gt-one", "gt-two"})
	if !got["gt-one"] || !got["gt-two"] {
		t.Fatalf("areScheduled on scan failure = %+v, want all requested IDs marked scheduled", got)
	}
}

func TestRunSchedulerClearFailsOnContextScanFailure(t *testing.T) {
	townRoot := setupSchedulerScanFailureTown(t)
	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(townRoot); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCWD) })
	oldClearBead := schedulerClearBead
	schedulerClearBead = ""
	t.Cleanup(func() { schedulerClearBead = oldClearBead })

	err = runSchedulerClear(nil, nil)
	if err == nil {
		t.Fatal("scheduler clear succeeded with incomplete context scan")
	}
	if !strings.Contains(err.Error(), "listing sling contexts") {
		t.Fatalf("error = %q, want sling context scan failure", err.Error())
	}
}
