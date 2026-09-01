package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestLoadConvoyTrackedIDsUsesBoundedFallbackOnlyWhenSQLUnsupported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	depLog := filepath.Join(t.TempDir(), "dep-calls")
	writeRoutingBdStub(t, `case "$*" in
  "--allow-stale version") exit 0 ;;
  sql*) printf 'unknown command sql\n' >&2; exit 1 ;;
  "dep list hq-a --direction=down --type=tracks --json") printf 'call\n' >> "$GT_DEP_LOG"; printf '[{"id":"gt-a","dependency_type":"tracks"}]\n' ;;
  "dep list hq-b --direction=down --type=tracks --json") printf 'call\n' >> "$GT_DEP_LOG"; printf '[{"id":"gt-b","dependency_type":"tracks"}]\n' ;;
  *) printf 'unexpected bd args: %s\n' "$*" >&2; exit 1 ;;
esac
`)
	t.Setenv("GT_DEP_LOG", depLog)

	got, err := loadConvoyTrackedIDsContext(context.Background(), townRoot, []string{"hq-b", "hq-a"})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got["hq-a"]) != "[gt-a]" || fmt.Sprint(got["hq-b"]) != "[gt-b]" {
		t.Fatalf("fallback relationships = %#v", got)
	}
	data, err := os.ReadFile(depLog)
	if err != nil {
		t.Fatal(err)
	}
	if calls := strings.Count(string(data), "call\n"); calls != 2 {
		t.Fatalf("bounded fallback calls = %d, want one per convoy", calls)
	}
}

func TestParseConvoyTrackedRowsRejectsPartialAndSorts(t *testing.T) {
	wanted := map[string]struct{}{"hq-a": {}}
	got, err := parseConvoyTrackedRows([]byte(`[{"issue_id":"hq-a","depends_on_id":"gt-z"},{"issue_id":"hq-a","depends_on_id":"gt-a"}]`), wanted)
	if err != nil || fmt.Sprint(got["hq-a"]) != "[gt-a gt-z]" {
		t.Fatalf("sorted relationships = %#v, err=%v", got, err)
	}
	if _, err := parseConvoyTrackedRows([]byte(`[{"issue_id":"hq-a"}]`), wanted); err == nil {
		t.Fatal("partial relationship row accepted")
	}
}

func TestConvoyRelationshipSnapshotFiftyRecords(t *testing.T) {
	convoys := make([]convoyListIssue, 50)
	edges := make(map[string][]string, len(convoys))
	details := map[string]*issueDetails{
		"gt-shared": {Title: "Shared", Status: "closed"},
	}
	for i := range convoys {
		convoyID := fmt.Sprintf("hq-convoy-%02d", i)
		issueID := fmt.Sprintf("rig-work-%02d", i)
		if i%2 == 1 {
			issueID = fmt.Sprintf("other-work-%02d", i)
		}
		convoys[i] = convoyListIssue{ID: convoyID, Status: "open"}
		edges[convoyID] = []string{"gt-shared", issueID}
		if i != 17 { // A routed lookup that cannot resolve remains explicitly unknown.
			details[issueID] = &issueDetails{Title: issueID, Status: "open"}
		}
	}

	var edgeLoads, detailLoads int
	detailIDs := make(map[string]int)
	started := time.Now()
	snapshot, err := loadConvoyRelationshipSnapshotContext(
		context.Background(),
		t.TempDir(),
		convoys,
		func(_ context.Context, _ string, gotIDs []string) (map[string][]string, error) {
			edgeLoads++
			if len(gotIDs) != len(convoys) {
				t.Fatalf("edge loader received %d convoy IDs, want %d", len(gotIDs), len(convoys))
			}
			return edges, nil
		},
		func(_ context.Context, gotIDs []string) (map[string]*issueDetails, error) {
			detailLoads++
			for _, id := range gotIDs {
				detailIDs[id]++
			}
			return details, nil
		},
	)
	if err != nil {
		t.Fatalf("loadConvoyRelationshipSnapshotContext() error: %v", err)
	}
	if edgeLoads != 1 {
		t.Fatalf("dependency edge loads = %d, want 1", edgeLoads)
	}
	if detailLoads != 1 {
		t.Fatalf("issue detail batch loads = %d, want 1", detailLoads)
	}
	if len(detailIDs) != 51 {
		t.Fatalf("unique issue details requested = %d, want 51", len(detailIDs))
	}
	for id, count := range detailIDs {
		if count != 1 {
			t.Fatalf("issue detail %s requested %d times, want 1", id, count)
		}
	}
	unknown := snapshot["hq-convoy-17"]
	if len(unknown) != 2 || unknown[1].ID != "other-work-17" || unknown[1].Status != trackedStatusUnknown {
		t.Fatalf("partial routed lookup = %#v, want stable shared+unknown result", unknown)
	}
	for _, convoy := range convoys {
		tracked := snapshot[convoy.ID]
		if len(tracked) != 2 || !sort.SliceIsSorted(tracked, func(i, j int) bool { return tracked[i].ID < tracked[j].ID }) {
			t.Fatalf("snapshot[%s] = %#v, want two deterministically ordered issues", convoy.ID, tracked)
		}
	}
	t.Logf("stages: list=1 edges=%d details=%d workers=0 classify=1 elapsed=%s", edgeLoads, detailLoads, time.Since(started))
}

func TestConvoyRelationshipSnapshotPreservesFailureAndCancellation(t *testing.T) {
	convoys := []convoyListIssue{{ID: "hq-convoy-00", Status: "open"}}
	wantErr := errors.New("partial relationship query")
	_, err := loadConvoyRelationshipSnapshotContext(
		context.Background(), "unused", convoys,
		func(context.Context, string, []string) (map[string][]string, error) { return nil, wantErr },
		func(context.Context, []string) (map[string]*issueDetails, error) {
			t.Fatal("details loaded after edge failure")
			return nil, nil
		},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("relationship error = %v, want %v", err, wantErr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = loadConvoyRelationshipSnapshotContext(
		ctx, "unused", convoys,
		func(ctx context.Context, _ string, _ []string) (map[string][]string, error) { return nil, ctx.Err() },
		func(context.Context, []string) (map[string]*issueDetails, error) {
			t.Fatal("details loaded after cancellation")
			return nil, nil
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v, want context.Canceled", err)
	}

	wantErr = errors.New("partial routed detail query")
	_, err = loadConvoyRelationshipSnapshotContext(
		context.Background(), "unused", convoys,
		func(context.Context, string, []string) (map[string][]string, error) {
			return map[string][]string{"hq-convoy-00": {"aa-one", "bb-one"}}, nil
		},
		func(context.Context, []string) (map[string]*issueDetails, error) {
			return map[string]*issueDetails{"aa-one": {Status: "open"}}, wantErr
		},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("detail error = %v, want %v", err, wantErr)
	}
}
