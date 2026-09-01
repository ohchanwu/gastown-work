package beads

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestShowMultipleContextRunsRoutesConcurrentlyAndPreservesPartialError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}

	townRoot := t.TempDir()
	for _, path := range []string{
		filepath.Join(townRoot, "mayor"),
		filepath.Join(townRoot, ".beads"),
		filepath.Join(townRoot, "rig-a", ".beads"),
		filepath.Join(townRoot, "rig-b", ".beads"),
		filepath.Join(townRoot, "rig-c", ".beads"),
	} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", RoutesFileName), []byte(
		"{\"prefix\":\"aa-\",\"path\":\"rig-a\"}\n"+
			"{\"prefix\":\"bb-\",\"path\":\"rig-b\"}\n"+
			"{\"prefix\":\"cc-\",\"path\":\"rig-c\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = "--allow-stale" ]; then
  shift
fi
if [ "$1" = "version" ]; then
  exit 0
fi
sleep 2
case "$BEADS_DIR" in
  */rig-b/.beads) printf 'route b failed\n' >&2; exit 1 ;;
esac
id="$3"
printf '[{"id":"%s","title":"%s","status":"open"}]\n' "$id" "$id"
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ResetBdAllowStaleCacheForTest()

	started := time.Now()
	got, err := New(townRoot).ShowMultipleContext(context.Background(), []string{"cc-one", "bb-one", "aa-one"})
	if err == nil {
		t.Fatal("partial routed failure returned nil error")
	}
	if got["aa-one"] == nil || got["cc-one"] == nil || got["bb-one"] != nil {
		t.Fatalf("partial result = %#v", got)
	}
	if elapsed := time.Since(started); elapsed >= 5*time.Second {
		t.Fatalf("three routed lookups took %s, want under 5s", elapsed.Round(time.Millisecond))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started = time.Now()
	_, err = New(townRoot).ShowMultipleContext(ctx, []string{"aa-two", "cc-two"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled routed lookup error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("canceled routed lookup returned after %s", elapsed.Round(time.Millisecond))
	}
}
