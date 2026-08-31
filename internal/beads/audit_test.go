package beads

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	lockpkg "github.com/steveyegge/gastown/internal/lock"
)

func installMockDetachIssueCAS(t *testing.T) {
	t.Helper()
	previous := detachIssueWithAuditCAS
	previousVerify := detachSnapshotAtCommitFn
	detachIssueWithAuditCAS = func(b *Beads, issue *Issue, newDesc string, entry DetachAuditEntry) error {
		entry.AttemptID = uuid.NewString()
		entry.State = "committed"
		entry.CommitMessage = "test detach " + entry.AttemptID
		entry.CommitOID = "test-oid"
		entry.PreviousStatus = issue.Status
		entry.PreviousAssignee = issue.Assignee
		entry.PreviousDescription = issue.Description
		entry.ResultingStatus = issue.Status
		entry.ResultingAssignee = issue.Assignee
		entry.ResultingDescription = newDesc
		entry.ResultingGeneration = issue.UpdatedAt
		if err := b.LogDetachAudit(entry); err != nil {
			return err
		}
		return b.Update(issue.ID, UpdateOptions{Description: &newDesc})
	}
	detachSnapshotAtCommitFn = func(*Beads, DetachAuditEntry, string) (bool, error) { return true, nil }
	t.Cleanup(func() {
		detachIssueWithAuditCAS = previous
		detachSnapshotAtCommitFn = previousVerify
	})
}

func TestDetachMoleculeWithAuditRejectsChangedReceiptBeforeMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for bd")
	}
	for _, tt := range []struct {
		name             string
		showOutput       string
		expectedMolecule string
		expectedAssignee string
	}{
		{
			name: "replacement molecule", expectedMolecule: "gt-old-wisp", expectedAssignee: "gastown/polecats/nux",
			showOutput: `[{"id":"gt-work","title":"work","status":"open","assignee":"gastown/polecats/nux","description":"attached_molecule: gt-new-wisp"}]`,
		},
		{
			name: "reassigned work", expectedMolecule: "gt-new-wisp", expectedAssignee: "",
			showOutput: `[{"id":"gt-work","title":"work","status":"open","assignee":"gastown/polecats/nux","description":"attached_molecule: gt-new-wisp"}]`,
		},
		{
			name: "missing attachment", expectedMolecule: "gt-old-wisp", expectedAssignee: "",
			showOutput: `[{"id":"gt-work","title":"work","status":"open","assignee":"","description":""}]`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
				t.Fatal(err)
			}
			logPath := installMockBDShowRecorder(t, tt.showOutput)
			bd := NewIsolated(dir)
			_, err := bd.DetachMoleculeWithAudit("gt-work", DetachOptions{
				ExpectedMolecule: &tt.expectedMolecule,
				ExpectedAssignee: &tt.expectedAssignee,
			})
			if err == nil {
				t.Fatal("DetachMoleculeWithAudit succeeded after receipt changed")
			}
			if log := readMockBDLog(t, logPath); strings.Contains(log, "update ") {
				t.Fatalf("detach mutated changed bead: %q", log)
			}
			if _, statErr := os.Stat(filepath.Join(dir, ".beads", "audit.log")); !os.IsNotExist(statErr) {
				t.Fatalf("detach wrote audit before receipt validation: %v", statErr)
			}
		})
	}
}

func TestDetachMoleculeWithAuditRejectsConcurrentReassignmentAfterLockWait(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("real bd fixture is not supported on Windows")
	}
	portText := strings.TrimSpace(os.Getenv("GT_TEST_DOLT_PORT"))
	if portText == "" && os.Getenv("GT_TEST_ISOLATED") == "1" {
		portText = strings.TrimSpace(os.Getenv("GT_DOLT_PORT"))
	}
	port, err := strconv.Atoi(portText)
	if portText == "" || err != nil || port < 1 || port > 65535 {
		t.Skipf("GT_TEST_DOLT_PORT is absent or invalid: %q", portText)
	}
	dir := t.TempDir()
	bd := NewIsolatedWithPort(dir, port)
	if err := bd.Init("gt"); err != nil {
		t.Fatalf("bd init: %v", err)
	}
	work, err := bd.Create(CreateOptions{
		Title:       "work",
		Type:        "task",
		Priority:    2,
		Description: "attached_molecule: gt-old-wisp",
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	pinned := StatusPinned
	if err := bd.Update(work.ID, UpdateOptions{Status: &pinned}); err != nil {
		t.Fatalf("pin work: %v", err)
	}
	workID := work.ID
	initial, err := bd.Show(workID)
	if err != nil {
		t.Fatalf("read initial work: %v", err)
	}
	initialAttachment := ParseAttachmentFields(initial)
	if initial.Status != StatusPinned || initial.Assignee != "" || initialAttachment == nil || initialAttachment.AttachedMolecule != "gt-old-wisp" {
		t.Fatalf("initial work = status:%q assignee:%q attachment:%+v", initial.Status, initial.Assignee, initialAttachment)
	}

	unlock, err := bd.lockBead(workID)
	if err != nil {
		t.Fatalf("hold work lock: %v", err)
	}
	contended := make(chan struct{})
	done := make(chan error, 1)
	expectedMolecule, expectedAssignee := "gt-old-wisp", ""
	previousAcquire := acquireBeadLock
	acquireBeadLock = func(path string) (func(), error) {
		if release, acquired, tryErr := lockpkg.FlockTryAcquire(path); tryErr != nil {
			return nil, tryErr
		} else if acquired {
			release()
		} else {
			close(contended)
		}
		return previousAcquire(path)
	}
	t.Cleanup(func() { acquireBeadLock = previousAcquire })
	go func() {
		_, detachErr := bd.DetachMoleculeWithAudit(workID, DetachOptions{
			ExpectedMolecule: &expectedMolecule,
			ExpectedAssignee: &expectedAssignee,
		})
		done <- detachErr
	}()
	select {
	case <-contended:
	case <-time.After(5 * time.Second):
		unlock()
		t.Fatal("detach did not reach the held bead lock")
	}

	replacement, replacementAssignee := "attached_molecule: gt-replacement", "gastown/polecats/reused"
	if err := bd.Update(workID, UpdateOptions{Description: &replacement, Assignee: &replacementAssignee}); err != nil {
		unlock()
		t.Fatalf("persist replacement assignment: %v", err)
	}
	unlock()
	if err := <-done; err == nil || (!strings.Contains(err.Error(), "assignee changed") && !strings.Contains(err.Error(), "molecule changed")) {
		t.Fatalf("detach receipt error = %v", err)
	}

	got, err := bd.Show(workID)
	if err != nil {
		t.Fatalf("reread work: %v", err)
	}
	attachment := ParseAttachmentFields(got)
	if got.Assignee != replacementAssignee || attachment == nil || attachment.AttachedMolecule != "gt-replacement" {
		t.Fatalf("replacement changed: assignee=%q attachment=%+v", got.Assignee, attachment)
	}
}

func TestDetachMoleculeWithAuditRejectsConcurrentUpdateAfterValidation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("real bd fixture is not supported on Windows")
	}
	portText := strings.TrimSpace(os.Getenv("GT_TEST_DOLT_PORT"))
	if portText == "" && os.Getenv("GT_TEST_ISOLATED") == "1" {
		portText = strings.TrimSpace(os.Getenv("GT_DOLT_PORT"))
	}
	port, err := strconv.Atoi(portText)
	if portText == "" || err != nil || port < 1 || port > 65535 {
		t.Skipf("GT_TEST_DOLT_PORT is absent or invalid: %q", portText)
	}
	dir := t.TempDir()
	bd := NewIsolatedWithPort(dir, port)
	if err := bd.Init("gt"); err != nil {
		t.Fatalf("bd init: %v", err)
	}
	work, err := bd.Create(CreateOptions{Title: "work", Type: "task", Priority: 2, Description: "attached_molecule: gt-old-wisp"})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	pinned := StatusPinned
	if err := bd.Update(work.ID, UpdateOptions{Status: &pinned}); err != nil {
		t.Fatalf("pin work: %v", err)
	}

	validated := make(chan struct{})
	resume := make(chan struct{})
	beforeDetachIssueCAS = func() {
		close(validated)
		<-resume
	}
	t.Cleanup(func() { beforeDetachIssueCAS = nil })
	expectedMolecule, expectedAssignee := "gt-old-wisp", ""
	done := make(chan error, 1)
	go func() {
		_, detachErr := bd.DetachMoleculeWithAudit(work.ID, DetachOptions{
			Operation: "burn", ExpectedMolecule: &expectedMolecule, ExpectedAssignee: &expectedAssignee,
		})
		done <- detachErr
	}()
	select {
	case <-validated:
	case <-time.After(5 * time.Second):
		t.Fatal("detach did not reach the pre-CAS barrier")
	}
	replacement, replacementAssignee := "attached_molecule: gt-replacement", "gastown/polecats/reused"
	if err := bd.Update(work.ID, UpdateOptions{Description: &replacement, Assignee: &replacementAssignee}); err != nil {
		t.Fatalf("persist replacement assignment: %v", err)
	}
	close(resume)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "changed before detach") {
		t.Fatalf("detach CAS error = %v", err)
	}
	got, err := bd.Show(work.ID)
	if err != nil {
		t.Fatalf("reread work: %v", err)
	}
	attachment := ParseAttachmentFields(got)
	if got.Assignee != replacementAssignee || attachment == nil || attachment.AttachedMolecule != "gt-replacement" {
		t.Fatalf("replacement changed: assignee=%q attachment=%+v", got.Assignee, attachment)
	}
}

func TestDetachMoleculeWithAuditRetriesCommittedDetach(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a Unix shell script mock for bd")
	}
	dir := t.TempDir()
	installMockDetachIssueCAS(t)
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	statePath := filepath.Join(binDir, "detached")
	logPath := filepath.Join(binDir, "bd.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$MOCK_BD_LOG"
while [ "$1" = "--allow-stale" ]; do shift; done
cmd="$1"; shift
case "$cmd" in
  version) exit 0 ;;
  show)
    if [ "$(cat "$MOCK_DETACHED" 2>/dev/null)" = "replacement" ]; then
      printf '%s\n' '[{"id":"gt-work","title":"work","status":"pinned","assignee":"gastown/polecats/reused","description":"attached_molecule: gt-replacement","updated_at":"2026-08-30T00:00:00Z"}]'
    elif [ -f "$MOCK_DETACHED" ]; then
      printf '%s\n' '[{"id":"gt-work","title":"work","status":"pinned","assignee":"","description":"","updated_at":"2026-08-30T00:00:00Z"}]'
    else
      printf '%s\n' '[{"id":"gt-work","title":"work","status":"pinned","assignee":"","description":"attached_molecule: gt-old-wisp","updated_at":"2026-08-30T00:00:00Z"}]'
    fi
    ;;
  update) : > "$MOCK_DETACHED" ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOCK_BD_LOG", logPath)
	t.Setenv("MOCK_DETACHED", statePath)

	bd := NewIsolated(dir)
	expectedMolecule, expectedAssignee := "gt-old-wisp", ""
	opts := DetachOptions{ExpectedMolecule: &expectedMolecule, ExpectedAssignee: &expectedAssignee}
	if _, err := bd.DetachMoleculeWithAudit("gt-work", opts); err != nil {
		t.Fatalf("first detach: %v", err)
	}
	if _, err := bd.DetachMoleculeWithAudit("gt-work", opts); err != nil {
		t.Fatalf("durable detach retry: %v", err)
	}
	if err := os.WriteFile(statePath, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bd.DetachMoleculeWithAudit("gt-work", opts); err == nil || !strings.Contains(err.Error(), "molecule changed") {
		t.Fatalf("replacement after durable receipt error = %v", err)
	}
	if updates := strings.Count(readMockBDLog(t, logPath), "update "); updates != 1 {
		t.Fatalf("detach updates = %d, want 1", updates)
	}
}

func TestDetachMoleculeWithAuditRejectsDifferentOperationReceipt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a Unix shell script mock for bd")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := installMockBDShowRecorder(t, `[{"id":"gt-work","title":"work","status":"pinned","assignee":"","description":""}]`)
	bd := NewIsolated(dir)
	if err := bd.LogDetachAudit(DetachAuditEntry{
		Operation: "burn", PinnedBeadID: "gt-work", DetachedMolecule: "gt-old-wisp",
	}); err != nil {
		t.Fatal(err)
	}
	expectedMolecule, expectedAssignee := "gt-old-wisp", ""
	_, err := bd.DetachMoleculeWithAudit("gt-work", DetachOptions{
		Operation: "detach", ExpectedMolecule: &expectedMolecule, ExpectedAssignee: &expectedAssignee,
	})
	if err == nil || !strings.Contains(err.Error(), "molecule changed") {
		t.Fatalf("different-operation receipt error = %v", err)
	}
	if log := readMockBDLog(t, logPath); strings.Contains(log, "update ") {
		t.Fatalf("different-operation receipt mutated bead: %q", log)
	}
}

func TestDetachMoleculeWithAuditFailsClosedWhenReceiptCannotPersist(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a Unix shell script mock for bd")
	}
	dir := t.TempDir()
	installMockDetachIssueCAS(t)
	if err := os.MkdirAll(filepath.Join(dir, ".beads", "audit.log"), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := installMockBDShowRecorder(t, `[{"id":"gt-work","title":"work","status":"pinned","assignee":"","description":"attached_molecule: gt-old-wisp"}]`)
	bd := NewIsolated(dir)
	expectedMolecule, expectedAssignee := "gt-old-wisp", ""
	opts := DetachOptions{Operation: "burn", ExpectedMolecule: &expectedMolecule, ExpectedAssignee: &expectedAssignee}
	if _, err := bd.DetachMoleculeWithAudit("gt-work", opts); err == nil {
		t.Fatal("detach succeeded without durable audit receipt")
	}
	if log := readMockBDLog(t, logPath); strings.Contains(log, "update ") {
		t.Fatalf("detach mutated after audit failure: %q", log)
	}
	if err := os.Remove(filepath.Join(dir, ".beads", "audit.log")); err != nil {
		t.Fatal(err)
	}
	if _, err := bd.DetachMoleculeWithAudit("gt-work", opts); err != nil {
		t.Fatalf("retry after audit storage recovery: %v", err)
	}
}

func TestDetachMoleculeWithAuditReconcilesOnlyDurablePendingReceipt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("real bd fixture is not supported on Windows")
	}
	portText := strings.TrimSpace(os.Getenv("GT_TEST_DOLT_PORT"))
	if portText == "" && os.Getenv("GT_TEST_ISOLATED") == "1" {
		portText = strings.TrimSpace(os.Getenv("GT_DOLT_PORT"))
	}
	port, err := strconv.Atoi(portText)
	if portText == "" || err != nil || port < 1 || port > 65535 {
		t.Skipf("GT_TEST_DOLT_PORT is absent or invalid: %q", portText)
	}
	dir := t.TempDir()
	bd := NewIsolatedWithPort(dir, port)
	if err := bd.Init("gt"); err != nil {
		t.Fatalf("bd init: %v", err)
	}
	work, err := bd.Create(CreateOptions{Title: "work", Type: "task", Priority: 2, Description: "attached_molecule: gt-old-wisp"})
	if err != nil {
		t.Fatal(err)
	}
	pinned := StatusPinned
	if err := bd.Update(work.ID, UpdateOptions{Status: &pinned}); err != nil {
		t.Fatal(err)
	}
	oldCommit := execPinnedDoltCommit
	execPinnedDoltCommit = func(ctx context.Context, conn *sql.Conn, message string) error {
		if err := oldCommit(ctx, conn, message); err != nil {
			return err
		}
		return errors.New("lost commit acknowledgement")
	}
	t.Cleanup(func() { execPinnedDoltCommit = oldCommit })
	expectedMolecule, expectedAssignee, expectedStatus := "gt-old-wisp", "", string(StatusPinned)
	expectedDescription := "attached_molecule: gt-old-wisp"
	opts := DetachOptions{
		Operation: "burn", ExpectedMolecule: &expectedMolecule, ExpectedAssignee: &expectedAssignee,
		ExpectedStatus: &expectedStatus, ExpectedDescription: &expectedDescription,
	}
	_, err = bd.DetachMoleculeWithAudit(work.ID, opts)
	var unknown *DoltCommitOutcomeUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("detach error=%v, want unknown durable outcome", err)
	}
	execPinnedDoltCommit = oldCommit
	if _, err := bd.DetachMoleculeWithAudit(work.ID, opts); err != nil {
		t.Fatalf("durable pending receipt did not reconcile: %v", err)
	}
}

func TestDetachMoleculeWithAuditReconcilesSQLCommittedDoltUncommitted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("real bd fixture is not supported on Windows")
	}
	portText := strings.TrimSpace(os.Getenv("GT_TEST_DOLT_PORT"))
	if portText == "" && os.Getenv("GT_TEST_ISOLATED") == "1" {
		portText = strings.TrimSpace(os.Getenv("GT_DOLT_PORT"))
	}
	port, err := strconv.Atoi(portText)
	if portText == "" || err != nil || port < 1 || port > 65535 {
		t.Skipf("GT_TEST_DOLT_PORT is absent or invalid: %q", portText)
	}
	dir := t.TempDir()
	bd := NewIsolatedWithPort(dir, port)
	if err := bd.Init("gt"); err != nil {
		t.Fatalf("bd init: %v", err)
	}
	work, err := bd.Create(CreateOptions{Title: "work", Type: "task", Priority: 2, Description: "attached_molecule: gt-old-wisp"})
	if err != nil {
		t.Fatal(err)
	}
	pinned := StatusPinned
	if err := bd.Update(work.ID, UpdateOptions{Status: &pinned}); err != nil {
		t.Fatal(err)
	}
	oldCommit := execPinnedDoltCommit
	execPinnedDoltCommit = func(context.Context, *sql.Conn, string) error {
		return errors.New("DOLT_COMMIT definitely did not run")
	}
	t.Cleanup(func() { execPinnedDoltCommit = oldCommit })
	expectedMolecule, expectedAssignee, expectedStatus := "gt-old-wisp", "", string(StatusPinned)
	expectedDescription := "attached_molecule: gt-old-wisp"
	opts := DetachOptions{
		Operation: "burn", ExpectedMolecule: &expectedMolecule, ExpectedAssignee: &expectedAssignee,
		ExpectedStatus: &expectedStatus, ExpectedDescription: &expectedDescription,
	}
	_, err = bd.DetachMoleculeWithAudit(work.ID, opts)
	var unknown *DoltCommitOutcomeUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("detach error=%v, want unknown durable outcome", err)
	}
	execPinnedDoltCommit = oldCommit
	if _, err := bd.DetachMoleculeWithAudit(work.ID, opts); err != nil {
		t.Fatalf("SQL-committed detach did not create its missing Dolt commit: %v", err)
	}
}

func TestValidDoltCommitHash(t *testing.T) {
	for _, hash := range []string{
		"fafrumqo9hgjhfhes21lbh819geebdlu",
		"00000000000000000000000000000000",
	} {
		if !validDoltCommitHash(hash) {
			t.Fatalf("validDoltCommitHash(%q) = false", hash)
		}
	}
	for _, hash := range []string{"HEAD", "fafrumqo9hgjhfhes21lbh819geebdl'", "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF"} {
		if validDoltCommitHash(hash) {
			t.Fatalf("validDoltCommitHash(%q) = true", hash)
		}
	}
}

func TestDetachMoleculeWithAuditRejectsLegacyStateEmptyReceipt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a Unix shell script mock")
	}
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	installMockBDShowRecorder(t, `[{"id":"gt-work","title":"work","status":"pinned","assignee":"","description":""}]`)
	legacy := DetachAuditEntry{Operation: "burn", PinnedBeadID: "gt-work", DetachedMolecule: "gt-old-wisp"}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "audit.log"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	bd := NewIsolated(dir)
	expectedMolecule, expectedAssignee := "gt-old-wisp", ""
	if _, err := bd.DetachMoleculeWithAudit("gt-work", DetachOptions{
		Operation: "burn", ExpectedMolecule: &expectedMolecule, ExpectedAssignee: &expectedAssignee,
	}); err == nil || !strings.Contains(err.Error(), "molecule changed") {
		t.Fatalf("legacy receipt error = %v", err)
	}
}

func TestCompareAndRestoreIssueSnapshotIfMatchesIsAtomicAndDurable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("real bd fixture is not supported on Windows")
	}
	portText := strings.TrimSpace(os.Getenv("GT_TEST_DOLT_PORT"))
	if portText == "" && os.Getenv("GT_TEST_ISOLATED") == "1" {
		portText = strings.TrimSpace(os.Getenv("GT_DOLT_PORT"))
	}
	port, err := strconv.Atoi(portText)
	if portText == "" || err != nil || port < 1 || port > 65535 {
		t.Skipf("GT_TEST_DOLT_PORT is absent or invalid: %q", portText)
	}
	dir := t.TempDir()
	bd := NewIsolatedWithPort(dir, port)
	if err := bd.Init("gt"); err != nil {
		t.Fatalf("bd init: %v", err)
	}
	work, err := bd.Create(CreateOptions{Title: "work", Type: "task", Priority: 2, Description: "new workflow"})
	if err != nil {
		t.Fatal(err)
	}
	hooked, owner := StatusHooked, "gastown/polecats/toast"
	if err := bd.Update(work.ID, UpdateOptions{Status: &hooked, Assignee: &owner}); err != nil {
		t.Fatal(err)
	}
	if err := bd.CompareAndRestoreIssueSnapshotIfMatches(work.ID, hooked, owner, "new workflow", string(StatusOpen), "", "old workflow"); err != nil {
		t.Fatal(err)
	}
	db, err := openDoltSQL(bd.getResolvedBeadsDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var status, assignee, description string
	if err := db.QueryRowContext(context.Background(), `SELECT status, COALESCE(assignee, ''), COALESCE(description, '') FROM issues AS OF 'HEAD' WHERE id = ?`, work.ID).Scan(&status, &assignee, &description); err != nil {
		t.Fatal(err)
	}
	if status != string(StatusOpen) || assignee != "" || description != "old workflow" {
		t.Fatalf("durable snapshot=(%q,%q,%q)", status, assignee, description)
	}
}

func TestDetachMoleculeWithAuditRejectsMalformedDoltMetadata(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"backend":"dolt"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bd := NewIsolated(dir)
	issue := &Issue{ID: "gt-work", Status: StatusPinned, Description: "attached_molecule: gt-old-wisp"}
	err := detachIssueWithAuditCAS(bd, issue, "", DetachAuditEntry{Operation: "burn", PinnedBeadID: issue.ID, DetachedMolecule: "gt-old-wisp"})
	if err == nil || !strings.Contains(err.Error(), "invalid Dolt metadata") {
		t.Fatalf("error=%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(beadsDir, "audit.log")); !os.IsNotExist(statErr) {
		t.Fatalf("malformed metadata wrote audit receipt: %v", statErr)
	}
}

func TestDetachMoleculeWithAuditRejectsMissingDoltMetadata(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bd := NewIsolated(dir)
	issue := &Issue{ID: "gt-work", Status: StatusPinned, Description: "attached_molecule: gt-old-wisp"}
	err := detachIssueWithAuditCAS(bd, issue, "", DetachAuditEntry{Operation: "burn", PinnedBeadID: issue.ID, DetachedMolecule: "gt-old-wisp"})
	if err == nil || !strings.Contains(err.Error(), "missing Dolt metadata") {
		t.Fatalf("error=%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(beadsDir, "audit.log")); !os.IsNotExist(statErr) {
		t.Fatalf("missing metadata wrote audit receipt: %v", statErr)
	}
}

func TestExactIssueMutationsRejectMissingDoltMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	bd := NewIsolated(dir)
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "description", run: func() error {
			return bd.CompareAndUpdateIssueDescription("gt-work", "open", "", "old", "new")
		}},
		{name: "assignment", run: func() error {
			_, err := bd.compareAndUpdateIssueAssignment("gt-work", "open", "", "hooked", "gastown/polecats/toast")
			return err
		}},
		{name: "restore", run: func() error {
			return bd.CompareAndRestoreIssueSnapshotIfMatches("gt-work", "hooked", "toast", "new", "open", "", "old")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.run(); err == nil || !strings.Contains(err.Error(), "missing Dolt metadata") {
				t.Fatalf("error=%v, want missing metadata", err)
			}
		})
	}
}

func TestMoleculeCleanupPhaseIsMonotonicAndAuditIsStreaming(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	bd := NewIsolated(dir)
	attemptID := uuid.NewString()
	largeDescription := strings.Repeat("x", 128*1024)
	if err := bd.LogDetachAudit(DetachAuditEntry{
		Operation: "burn", PinnedBeadID: "gt-work", DetachedMolecule: "gt-molecule",
		AttemptID: attemptID, State: "committed", CommitMessage: "test", CommitOID: "abc",
		PreviousDescription: largeDescription,
		CleanupMolecules:    []MoleculeCleanupReceipt{{ID: "gt-molecule", Description: largeDescription}},
	}); err != nil {
		t.Fatal(err)
	}
	if pending, err := bd.PendingMoleculeCleanup("gt-work", "burn"); err != nil || pending == nil {
		t.Fatalf("large pending cleanup = (%+v, %v), want decoded attempt", pending, err)
	}
	if err := bd.LogMoleculeCleanupPhase(attemptID, "complete"); err != nil {
		t.Fatal(err)
	}
	if err := bd.LogMoleculeCleanupPhase(attemptID, "descendants_closed"); err == nil {
		t.Fatal("terminal cleanup phase regressed")
	}
	if pending, err := bd.PendingMoleculeCleanup("gt-work", "burn"); err != nil || pending != nil {
		t.Fatalf("completed cleanup = (%+v, %v), want no pending attempt", pending, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".beads", "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if got := len(lines[len(lines)-1]); got > 1024 {
		t.Fatalf("terminal phase receipt is %d bytes, want compact record", got)
	}
}
