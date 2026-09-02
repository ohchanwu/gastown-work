package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFindStrandedConvoysLiveSize(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}

	townRoot, _ := makeRoutingTownWorkspace(t)
	chdirConvoyTest(t, townRoot)
	for _, path := range []string{
		filepath.Join(townRoot, "rig-a", "polecats"),
		filepath.Join(townRoot, "rig-a", "mayor", "rig", ".beads"),
		filepath.Join(townRoot, "rig-b", "polecats"),
		filepath.Join(townRoot, "rig-b", "mayor", "rig", ".beads"),
	} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(
		"{\"prefix\":\"gt-\",\"path\":\"rig-a/mayor/rig\"}\n"+
			"{\"prefix\":\"bd-\",\"path\":\"rig-b/mayor/rig\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isSlingableBead(townRoot, "gt-task-00") {
		t.Fatal("synthetic routed task is not slingable")
	}
	convoys := make([]convoyListIssue, 50)
	rows := make([]map[string]string, len(convoys))
	for i := range convoys {
		convoys[i] = convoyListIssue{ID: fmt.Sprintf("hq-cv-%02d", i), Title: fmt.Sprintf("Convoy %02d", i), Status: "open", IssueType: "convoy", Labels: []string{"gt:convoy"}}
		prefix := "gt"
		if i%2 == 1 {
			prefix = "bd"
		}
		rows[i] = map[string]string{"issue_id": convoys[i].ID, "depends_on_id": fmt.Sprintf("%s-task-%02d", prefix, i)}
	}
	convoyJSON, err := json.Marshal(convoys)
	if err != nil {
		t.Fatal(err)
	}
	edgeJSON, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	dependencyLog := filepath.Join(t.TempDir(), "dependency-scans")
	detailLog := filepath.Join(t.TempDir(), "detail-scans")
	workerLog := filepath.Join(t.TempDir(), "worker-scans")
	commandLog := filepath.Join(t.TempDir(), "commands")
	script := fmt.Sprintf(`printf '%%s\n' "$*" >> "$GT_COMMAND_LOG"
case "$*" in
  "--allow-stale version") exit 0 ;;
  "list --label=gt:convoy --json --limit=0 --status=open --flat") printf '%%s\n' '%s' ;;
  "list --json --limit=0 --status=open --flat") printf '[]\n' ;;
  sql*) printf 'scan\n' >> "$GT_DEPENDENCY_SCAN_LOG"; printf '%%s\n' '%s' ;;
  "list --label=gt:agent --status=open --json --limit=0 --flat") printf 'scan\n' >> "$GT_WORKER_SCAN_LOG"; printf '[]\n' ;;
  show*)
	printf 'scan\n' >> "$GT_DETAIL_SCAN_LOG"
    shift 2
    sep=
    printf '['
    for id do
	  printf '%%s{"id":"%%s","title":"Open task","status":"open","issue_type":"task","assignee":"gastown/polecats/capable"}' "$sep" "$id"
      sep=,
    done
    printf ']\n'
    ;;
  query*) printf '[]\n' ;;
  *) printf 'unexpected bd args: %%s\n' "$*" >&2; exit 1 ;;
esac
`, convoyJSON, edgeJSON)
	writeRoutingBdStub(t, script)
	t.Setenv("GT_DEPENDENCY_SCAN_LOG", dependencyLog)
	t.Setenv("GT_DETAIL_SCAN_LOG", detailLog)
	t.Setenv("GT_WORKER_SCAN_LOG", workerLog)
	t.Setenv("GT_COMMAND_LOG", commandLog)
	tmuxLog := filepath.Join(t.TempDir(), "tmux-scans")
	tmuxBin := filepath.Join(t.TempDir(), "tmux")
	if err := os.WriteFile(tmuxBin, []byte("#!/bin/sh\nprintf 'scan\\n' >> \"$GT_TMUX_SCAN_LOG\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GT_INTERNAL_PINNED_TMUX_BINARY", tmuxBin)
	t.Setenv("GT_TMUX_SCAN_LOG", tmuxLog)
	started := time.Now()
	got, err := findStrandedConvoysContext(context.Background(), townRoot)
	if err != nil {
		commands, _ := os.ReadFile(commandLog)
		t.Fatalf("%v; commands:\n%s", err, commands)
	}
	if len(got) != len(convoys) {
		t.Fatalf("stranded convoys = %d, want %d", len(got), len(convoys))
	}
	for i, convoy := range got {
		if convoy.ID != convoys[i].ID || convoy.ReadyCount != 1 || convoy.TrackedCount != 1 {
			commands, _ := os.ReadFile(commandLog)
			t.Fatalf("stranded[%d] = %+v, want ordered one-ready convoy %s; commands:\n%s", i, convoy, convoys[i].ID, commands)
		}
	}
	data, err := os.ReadFile(dependencyLog)
	if err != nil {
		t.Fatal(err)
	}
	if scans := strings.Count(string(data), "scan\n"); scans != 1 {
		t.Fatalf("dependency snapshot process launches = %d, want 1", scans)
	}
	data, err = os.ReadFile(detailLog)
	if err != nil {
		t.Fatal(err)
	}
	if scans := strings.Count(string(data), "scan\n"); scans != 2 {
		t.Fatalf("issue detail route-group process launches = %d, want 2", scans)
	}
	data, err = os.ReadFile(workerLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if scans := strings.Count(string(data), "scan\n"); scans != 0 {
		t.Fatalf("worker inventory route scans = %d, want 0", scans)
	}
	data, err = os.ReadFile(tmuxLog)
	if err != nil {
		t.Fatal(err)
	}
	if scans := strings.Count(string(data), "scan\n"); scans != 1 {
		t.Fatalf("tmux session inventory process launches = %d, want 1", scans)
	}
	if elapsed := time.Since(started); elapsed >= 5*time.Second {
		t.Fatalf("stranded scan elapsed = %s, want under 5s", elapsed)
	}

	if err := os.Remove(filepath.Join(townRoot, ".beads", "routes.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(townRoot, ".beads", "routes.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := findStrandedConvoysContext(context.Background(), townRoot); err == nil {
		t.Fatal("unreadable route snapshot published a complete stranded inventory")
	}
}

func TestGetWorkersForIssuesContextCancelsAndCapsRigQueries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}

	townRoot := t.TempDir()
	for i := 0; i < 6; i++ {
		for _, suffix := range []string{"polecats", filepath.Join("mayor", "rig", ".beads")} {
			if err := os.MkdirAll(filepath.Join(townRoot, "rig-"+string(rune('a'+i)), suffix), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	binDir := t.TempDir()
	startLog := filepath.Join(t.TempDir(), "starts")
	script := "#!/bin/sh\nprintf 'start\\n' >> \"$GT_WORKER_START_LOG\"\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GT_WORKER_START_LOG", startLog)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		_, err := getWorkersForIssuesContext(ctx, townRoot, []string{"gt-work"})
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(startLog); err == nil && len(data) > 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
			t.Fatal("worker scan did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled worker scan did not return before the 30s child could exit normally")
	}
	data, err := os.ReadFile(startLog)
	if err != nil {
		t.Fatal(err)
	}
	if starts := strings.Count(string(data), "start\n"); starts < 1 || starts > convoyLookupConcurrency {
		t.Fatalf("started rig queries = %d, want 1..%d", starts, convoyLookupConcurrency)
	}
}
