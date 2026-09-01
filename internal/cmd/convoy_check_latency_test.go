package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCheckCompletedConvoysLiveSizeSkipsWorkerInventory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}

	townRoot, _ := makeRoutingTownWorkspace(t)
	chdirConvoyTest(t, townRoot)
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte("{\"prefix\":\"hq-\",\"path\":\".\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "rig-a", "polecats"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "rig-a", "mayor", "rig", ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}

	convoys := make([]convoyListIssue, 51)
	tracked := make(map[string][]string, len(convoys))
	uniqueTargets := make(map[string]struct{})
	edge := 0
	for i := range convoys {
		convoys[i] = convoyListIssue{ID: fmt.Sprintf("hq-cv-%02d", i), Title: fmt.Sprintf("Convoy %02d", i), Status: "open", IssueType: "convoy", Labels: []string{"gt:convoy"}}
		count := 1
		if i == 0 {
			count = 13
		} else if i <= 8 {
			count = 2
		}
		for range count {
			target := fmt.Sprintf("hq-task-%02d", edge)
			if edge == 70 {
				target = "hq-task-00"
			}
			tracked[convoys[i].ID] = append(tracked[convoys[i].ID], target)
			uniqueTargets[target] = struct{}{}
			edge++
		}
	}
	if edge != 71 || len(uniqueTargets) != 70 || len(tracked[convoys[0].ID]) != 13 {
		t.Fatalf("fixture shape: convoys=%d edges=%d targets=%d max=%d", len(convoys), edge, len(uniqueTargets), len(tracked[convoys[0].ID]))
	}

	convoyJSON, err := json.Marshal(convoys)
	if err != nil {
		t.Fatal(err)
	}
	workerLog := filepath.Join(t.TempDir(), "worker-scans")
	dependencyLog := filepath.Join(t.TempDir(), "dependency-scans")
	detailLog := filepath.Join(t.TempDir(), "detail-scans")
	mutationLog := filepath.Join(t.TempDir(), "mutations")
	var dependencyRows []map[string]string
	for _, convoy := range convoys {
		for _, target := range tracked[convoy.ID] {
			dependencyRows = append(dependencyRows, map[string]string{"issue_id": convoy.ID, "depends_on_id": target})
		}
	}
	dependencyJSON, err := json.Marshal(dependencyRows)
	if err != nil {
		t.Fatal(err)
	}
	var script strings.Builder
	fmt.Fprintf(&script, `case "$*" in
  "--allow-stale version") exit 0 ;;
  "list --label=gt:convoy --json --limit=0 --status=open --flat") printf '%%s\n' '%s' ;;
  "list --json --limit=0 --status=open --flat") printf '%%s\n' '[]' ;;
  "list --label=gt:agent --status=open --json --limit=0 --flat") printf 'scan\n' >> "$GT_WORKER_SCAN_LOG"; printf '%%s\n' '[]' ;;
  sql*) printf 'scan\n' >> "$GT_DEPENDENCY_SCAN_LOG"; printf '%%s\n' '%s' ;;
  show*)
	printf 'scan\n' >> "$GT_DETAIL_SCAN_LOG"
    shift 2
    sep=
    printf '['
    for id do
	  status=open
	  [ "$id" = "hq-task-00" ] && status=closed
	  printf '%%s{"id":"%%s","title":"Task","status":"%%s","issue_type":"task"}' "$sep" "$id" "$status"
      sep=,
    done
    printf ']\n'
    ;;
  close*|update*|export*) printf '%%s\n' "$*" >> "$GT_MUTATION_LOG" ;;
  *) printf 'unexpected bd args: %%s\n' "$*" >&2; exit 1 ;;
esac
`, convoyJSON, dependencyJSON)
	writeRoutingBdStub(t, script.String())
	t.Setenv("GT_WORKER_SCAN_LOG", workerLog)
	t.Setenv("GT_DEPENDENCY_SCAN_LOG", dependencyLog)
	t.Setenv("GT_DETAIL_SCAN_LOG", detailLog)
	t.Setenv("GT_MUTATION_LOG", mutationLog)

	start := time.Now()
	summary, err := checkCompletedConvoys(context.Background(), townRoot, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Checked != 51 || summary.EligibleClosed != 1 || summary.SkippedUncertain != 0 || summary.TimedOut || len(summary.Errors) != 0 {
		t.Fatalf("dry-run summary = %+v, want one eligible closure", summary)
	}
	if data, readErr := os.ReadFile(mutationLog); readErr == nil || !os.IsNotExist(readErr) {
		t.Fatalf("dry-run mutation log = %q, err=%v", data, readErr)
	}
	data, err := os.ReadFile(workerLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	scans := strings.Count(string(data), "scan\n")
	t.Logf("live-size completion: convoys=%d edges=%d targets=%d worker_scans=%d elapsed=%s", len(convoys), edge, len(uniqueTargets), scans, time.Since(start).Round(time.Millisecond))
	if scans != 0 {
		t.Fatalf("worker inventory process launches = %d, want 0", scans)
	}
	data, err = os.ReadFile(dependencyLog)
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
	if scans := strings.Count(string(data), "scan\n"); scans != 1 {
		t.Fatalf("issue detail batch process launches = %d, want 1", scans)
	}

	summary, err = checkCompletedConvoys(context.Background(), townRoot, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if summary.EligibleClosed != 1 || len(summary.Errors) != 0 {
		t.Fatalf("non-dry-run summary = %+v, want one closure", summary)
	}
	data, err = os.ReadFile(mutationLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "close hq-cv-50") {
		t.Fatalf("non-dry-run mutation log = %q, want convoy close", data)
	}
}
