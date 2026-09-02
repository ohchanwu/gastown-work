package beads

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/config"
)

func TestWriteRoutesWaitsForDatabaseOwnership(t *testing.T) {
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	lockPath := filepath.Join(townRoot, ".runtime", "dolt-database-cleanup", "ownership.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	ownershipLock := flock.New(lockPath)
	if err := ownershipLock.Lock(); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- WriteRoutes(beadsDir, []Route{{Prefix: "hq-", Path: "."}}) }()
	select {
	case err := <-done:
		_ = ownershipLock.Unlock()
		t.Fatalf("WriteRoutes bypassed ownership fence: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := ownershipLock.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("WriteRoutes after ownership release: %v", err)
	}
}

func TestAppendRouteToDirSerializesReadModifyWrite(t *testing.T) {
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := WriteRoutes(beadsDir, []Route{{Prefix: "hq-", Path: "."}}); err != nil {
		t.Fatal(err)
	}

	entered := make(chan string, 2)
	release := map[string]chan struct{}{"aa-": make(chan struct{}, 1), "bb-": make(chan struct{}, 1)}
	done := make(chan error, 2)
	update := func(route Route) {
		done <- UpdateRoutes(beadsDir, func(routes []Route) ([]Route, error) {
			entered <- route.Prefix
			<-release[route.Prefix]
			return append(routes, route), nil
		})
	}
	go update(Route{Prefix: "aa-", Path: "a"})
	first := <-entered
	attempted := make(chan struct{})
	go func() {
		close(attempted)
		update(Route{Prefix: "bb-", Path: "b"})
	}()
	<-attempted
	select {
	case second := <-entered:
		release[first] <- struct{}{}
		release[second] <- struct{}{}
		t.Fatal("route update callbacks ran concurrently")
	case <-time.After(50 * time.Millisecond):
	}
	release[first] <- struct{}{}
	second := <-entered
	release[second] <- struct{}{}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}

	routes, err := LoadRoutes(beadsDir)
	if err != nil {
		t.Fatal(err)
	}
	found := make(map[string]bool, len(routes))
	for _, route := range routes {
		found[route.Prefix] = true
	}
	if !found["aa-"] || !found["bb-"] {
		t.Fatalf("concurrent ownership update was lost: routes=%v", routes)
	}
}

func TestAppendRouteRejectsConcurrentDifferentRigAfterBothPreflight(t *testing.T) {
	townRoot := t.TempDir()
	for _, path := range []string{"alpha", "bravo"} {
		if err := CheckPrefixAvailable(townRoot, "zz-", path); err != nil {
			t.Fatalf("preflight for %s: %v", path, err)
		}
	}

	start := make(chan struct{})
	done := make(chan error, 2)
	for _, path := range []string{"alpha", "bravo"} {
		go func() {
			<-start
			done <- AppendRoute(townRoot, Route{Prefix: "zz-", Path: path})
		}()
	}
	close(start)

	var successes, collisions int
	for range 2 {
		err := <-done
		switch {
		case err == nil:
			successes++
		case strings.Contains(err.Error(), "already used"):
			collisions++
		default:
			t.Fatalf("route publication error = %v, want prefix collision", err)
		}
	}
	if successes != 1 || collisions != 1 {
		t.Fatalf("route publication results: successes=%d collisions=%d, want 1 and 1", successes, collisions)
	}

	routes, err := LoadRoutes(filepath.Join(townRoot, ".beads"))
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Prefix != "zz-" {
		t.Fatalf("routes = %v, want one zz- owner", routes)
	}
}

func TestReleaseRouteReservationRemovesOnlyCreatedExactRoute(t *testing.T) {
	townRoot := t.TempDir()
	reserved := Route{Prefix: "zz-", Path: "alpha"}
	created, err := ReserveRoute(townRoot, reserved)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("new route was not reported as created")
	}

	created, err = ReserveRoute(townRoot, reserved)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("existing exact route was reported as created")
	}

	if err := AppendRoute(townRoot, Route{Prefix: "yy-", Path: "bravo"}); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseRouteReservation(townRoot, reserved); err != nil {
		t.Fatal(err)
	}
	routes, err := LoadRoutes(filepath.Join(townRoot, ".beads"))
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Prefix != "yy-" {
		t.Fatalf("routes = %v, want only unrelated yy- route", routes)
	}
}

func TestGetPrefixForRig(t *testing.T) {
	// Create a temporary directory with routes.jsonl
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix": "gt-", "path": "gastown/mayor/rig"}
{"prefix": "bd-", "path": "beads/mayor/rig"}
{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		rig      string
		expected string
	}{
		{"gastown", "gt"},
		{"beads", "bd"},
		{"unknown", "gt"}, // default
		{"", "gt"},        // empty rig -> default
	}

	for _, tc := range tests {
		t.Run(tc.rig, func(t *testing.T) {
			result := GetPrefixForRig(tmpDir, tc.rig)
			if result != tc.expected {
				t.Errorf("GetPrefixForRig(%q, %q) = %q, want %q", tmpDir, tc.rig, result, tc.expected)
			}
		})
	}
}

func TestGetPrefixForRig_NoRoutesFile(t *testing.T) {
	tmpDir := t.TempDir()
	// No routes.jsonl file

	result := GetPrefixForRig(tmpDir, "anything")
	if result != "gt" {
		t.Errorf("Expected default 'gt' when no routes file, got %q", result)
	}
}

func TestGetPrefixForRig_RigsConfigFallback(t *testing.T) {
	tmpDir := t.TempDir()

	// Write rigs.json with a non-gt prefix
	rigsPath := filepath.Join(tmpDir, "mayor", "rigs.json")
	if err := os.MkdirAll(filepath.Dir(rigsPath), 0755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.RigsConfig{
		Version: config.CurrentRigsVersion,
		Rigs: map[string]config.RigEntry{
			"project_ideas": {
				BeadsConfig: &config.BeadsConfig{Prefix: "pi"},
			},
		},
	}
	if err := config.SaveRigsConfig(rigsPath, cfg); err != nil {
		t.Fatalf("SaveRigsConfig: %v", err)
	}

	result := GetPrefixForRig(tmpDir, "project_ideas")
	if result != "pi" {
		t.Errorf("Expected prefix from rigs config, got %q", result)
	}
}

func TestExtractPrefix(t *testing.T) {
	tests := []struct {
		beadID   string
		expected string
	}{
		{"ap-qtsup.16", "ap-"},
		{"hq-cv-abc", "hq-"},
		{"gt-mol-xyz", "gt-"},
		{"bd-123", "bd-"},
		{"", ""},
		{"nohyphen", ""},
		{"-startswithhyphen", ""}, // Leading hyphen = invalid prefix
		{"-", ""},                 // Just hyphen = invalid
		{"a-", "a-"},              // Trailing hyphen is valid
	}

	for _, tc := range tests {
		t.Run(tc.beadID, func(t *testing.T) {
			result := ExtractPrefix(tc.beadID)
			if result != tc.expected {
				t.Errorf("ExtractPrefix(%q) = %q, want %q", tc.beadID, result, tc.expected)
			}
		})
	}
}

func TestGetRigPathForPrefix(t *testing.T) {
	// Create a temporary directory with routes.jsonl
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix": "ap-", "path": "ai_platform/mayor/rig"}
{"prefix": "gt-", "path": "gastown/mayor/rig"}
{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		prefix   string
		expected string
	}{
		{"ap-", filepath.Join(tmpDir, "ai_platform/mayor/rig")},
		{"gt-", filepath.Join(tmpDir, "gastown/mayor/rig")},
		{"hq-", tmpDir},  // Town-level beads return townRoot
		{"unknown-", ""}, // Unknown prefix returns empty
		{"", ""},         // Empty prefix returns empty
	}

	for _, tc := range tests {
		t.Run(tc.prefix, func(t *testing.T) {
			result := GetRigPathForPrefix(tmpDir, tc.prefix)
			if result != tc.expected {
				t.Errorf("GetRigPathForPrefix(%q, %q) = %q, want %q", tmpDir, tc.prefix, result, tc.expected)
			}
		})
	}
}

func TestGetRigPathForPrefix_NoRoutesFile(t *testing.T) {
	tmpDir := t.TempDir()
	// No routes.jsonl file

	result := GetRigPathForPrefix(tmpDir, "ap-")
	if result != "" {
		t.Errorf("Expected empty string when no routes file, got %q", result)
	}
}

func TestResolveHookDir(t *testing.T) {
	// Create a temporary directory with routes.jsonl
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix": "ap-", "path": "ai_platform/mayor/rig"}
{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		beadID      string
		hookWorkDir string
		expected    string
	}{
		{
			name:        "prefix resolution takes precedence over hookWorkDir",
			beadID:      "ap-test",
			hookWorkDir: "/custom/path",
			expected:    filepath.Join(tmpDir, "ai_platform/mayor/rig"),
		},
		{
			name:        "resolves rig path from prefix",
			beadID:      "ap-test",
			hookWorkDir: "",
			expected:    filepath.Join(tmpDir, "ai_platform/mayor/rig"),
		},
		{
			name:        "town-level bead returns townRoot",
			beadID:      "hq-test",
			hookWorkDir: "",
			expected:    tmpDir,
		},
		{
			name:        "unknown prefix uses hookWorkDir as fallback",
			beadID:      "xx-unknown",
			hookWorkDir: "/fallback/path",
			expected:    "/fallback/path",
		},
		{
			name:        "unknown prefix without hookWorkDir falls back to townRoot",
			beadID:      "xx-unknown",
			hookWorkDir: "",
			expected:    tmpDir,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := ResolveHookDir(tmpDir, tc.beadID, tc.hookWorkDir)
			if result != tc.expected {
				t.Errorf("ResolveHookDir(%q, %q, %q) = %q, want %q",
					tmpDir, tc.beadID, tc.hookWorkDir, result, tc.expected)
			}
		})
	}
}

func TestResolveBeadsDirForID(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create rig beads directory for gt- prefix
	rigBeadsDir := filepath.Join(tmpDir, "gastown/mayor/rig/.beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix": "gt-", "path": "gastown/mayor/rig"}
{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		beadID   string
		expected string
	}{
		{
			name:     "town-level bead returns currentBeadsDir",
			beadID:   "hq-test123",
			expected: beadsDir,
		},
		{
			name:     "rig-prefixed bead resolves to rig beadsDir",
			beadID:   "gt-abc",
			expected: rigBeadsDir,
		},
		{
			name:     "unknown prefix returns currentBeadsDir",
			beadID:   "xx-unknown",
			expected: beadsDir,
		},
		{
			name:     "empty bead ID returns currentBeadsDir",
			beadID:   "",
			expected: beadsDir,
		},
		{
			name:     "no hyphen returns currentBeadsDir",
			beadID:   "nohyphen",
			expected: beadsDir,
		},
		{
			name:     "wisp bead (hq-wisp-xxx) resolves to town beads",
			beadID:   "hq-wisp-abc123",
			expected: beadsDir,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := ResolveBeadsDirForID(beadsDir, tc.beadID)
			if result != tc.expected {
				t.Errorf("ResolveBeadsDirForID(%q, %q) = %q, want %q",
					beadsDir, tc.beadID, result, tc.expected)
			}
		})
	}
}

func TestResolveBeadsDirForID_NoRoutes(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	// No routes.jsonl — should always return currentBeadsDir
	result := ResolveBeadsDirForID(beadsDir, "gt-abc")
	if result != beadsDir {
		t.Errorf("expected %q, got %q", beadsDir, result)
	}
}

func TestResolveBeadsDirForID_UsesTownRoutesFromWorktreeBeadsDir(t *testing.T) {
	tmpDir := t.TempDir()
	townBeadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(townBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	worktreeBeadsDir := filepath.Join(tmpDir, "gastown", "polecats", "chrome", "gastown", ".beads")
	if err := os.MkdirAll(worktreeBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	rigBeadsDir := filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "mayor", "town.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix": "gt-", "path": "gastown/mayor/rig"}
{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(townBeadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	result := ResolveBeadsDirForID(worktreeBeadsDir, "gt-abc")
	if result != rigBeadsDir {
		t.Fatalf("ResolveBeadsDirForID(%q, %q) = %q, want %q", worktreeBeadsDir, "gt-abc", result, rigBeadsDir)
	}
	result = ResolveBeadsDirForID(worktreeBeadsDir, "hq-wisp-abc")
	if result != townBeadsDir {
		t.Fatalf("ResolveBeadsDirForID(%q, %q) = %q, want %q", worktreeBeadsDir, "hq-wisp-abc", result, townBeadsDir)
	}
}

func TestGetRigNameForPrefix(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix": "gt-", "path": "gastown/mayor/rig"}
{"prefix": "bd-", "path": "beads/mayor/rig"}
{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		prefix   string
		expected string
	}{
		{"gt-", "gastown"},
		{"bd-", "beads"},
		{"hq-", ""},      // Town-level, no specific rig
		{"unknown-", ""}, // Not in routes
		{"", ""},         // Empty prefix
	}

	for _, tc := range tests {
		t.Run(tc.prefix, func(t *testing.T) {
			result := GetRigNameForPrefix(tmpDir, tc.prefix)
			if result != tc.expected {
				t.Errorf("GetRigNameForPrefix(%q, %q) = %q, want %q", tmpDir, tc.prefix, result, tc.expected)
			}
		})
	}
}

func TestGetRigDirForName(t *testing.T) {
	tmpDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(tmpDir); err == nil {
		tmpDir = resolved
	}
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix": "ga-", "path": "gantry"}
{"prefix": "al-", "path": "algoanki/mayor/rig"}
{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		rigName  string
		expected string
	}{
		{"gantry", filepath.Join(tmpDir, "gantry")},
		{"algoanki", filepath.Join(tmpDir, "algoanki/mayor/rig")},
		{"unknown", ""}, // Not in routes
		{"", ""},        // Empty rig name
	}

	for _, tc := range tests {
		t.Run(tc.rigName, func(t *testing.T) {
			result := GetRigDirForName(tmpDir, tc.rigName)
			if result != tc.expected {
				t.Errorf("GetRigDirForName(%q, %q) = %q, want %q", tmpDir, tc.rigName, result, tc.expected)
			}
		})
	}
}

func TestGetRigDirForName_TownLevelNotReturned(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}
	// Town-level rig (path=".") should not be returned — it has no rig dir.
	result := GetRigDirForName(tmpDir, "hq")
	if result != "" {
		t.Errorf("GetRigDirForName for town-level path = %q, want empty string", result)
	}
}

func TestResolveRepoAliasBeadsDir(t *testing.T) {
	townRoot := t.TempDir()
	townBeads := filepath.Join(townRoot, ".beads")
	rigRoot := filepath.Join(townRoot, "gastown")
	canonicalRig := filepath.Join(rigRoot, "mayor", "rig")
	canonicalBeads := filepath.Join(canonicalRig, ".beads")
	decoyBeads := filepath.Join(rigRoot, ".beads")
	escapeRig := filepath.Join(townRoot, "escape", "mayor", "rig")
	escapeBeads := filepath.Join(escapeRig, ".beads")
	escapeTarget := filepath.Join(townRoot, "..", "outside", ".beads")

	for _, dir := range []string{townBeads, canonicalBeads, decoyBeads, escapeBeads, escapeTarget} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(
		`{"prefix":"hq-","path":"."}`+"\n"+
			`{"prefix":"gt-","path":"gastown/mayor/rig"}`+"\n"+
			`{"prefix":"es-","path":"escape/mayor/rig"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonicalBeads, "metadata.json"), []byte(`{"dolt_database":"gastown"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoyBeads, "metadata.json"), []byte(`{"dolt_database":"hq"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(escapeBeads, "redirect"), []byte(filepath.Join("..", "..", "..", "..", "outside", ".beads")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		repo     string
		expected string
		ok       bool
	}{
		{"rig alias uses canonical route", "gastown", canonicalBeads, true},
		{"hq alias uses town beads", "hq", townBeads, true},
		{"town alias uses town beads", "town", townBeads, true},
		{"path-like remains unresolved", "gastown/mayor/rig", "", false},
		{"unknown bare repo remains unresolved", "unknown", "", false},
		{"redirect outside town rejected", "escape", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ResolveRepoAliasBeadsDir(townRoot, tc.repo)
			if ok != tc.ok {
				t.Fatalf("ResolveRepoAliasBeadsDir ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.expected {
				t.Fatalf("ResolveRepoAliasBeadsDir dir = %q, want %q", got, tc.expected)
			}
		})
	}
}

func TestRewriteBDCreateRepoAlias(t *testing.T) {
	townRoot := t.TempDir()
	townBeads := filepath.Join(townRoot, ".beads")
	canonicalBeads := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	decoyBeads := filepath.Join(townRoot, "gastown", ".beads")
	for _, dir := range []string{townBeads, canonicalBeads, decoyBeads} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(
		`{"prefix":"hq-","path":"."}`+"\n"+
			`{"prefix":"gt-","path":"gastown/mayor/rig"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		argv    []string
		want    []string
		wantDir string
	}{
		{
			name:    "global flags before create",
			argv:    []string{"bd", "--allow-stale", "create", "--repo", "gastown", "--title", "x"},
			want:    []string{"bd", "--allow-stale", "create", "--title", "x"},
			wantDir: canonicalBeads,
		},
		{
			name:    "equals form",
			argv:    []string{"bd", "--json", "create", "--repo=gastown"},
			want:    []string{"bd", "--json", "create"},
			wantDir: canonicalBeads,
		},
		{
			name:    "unknown bare repo unchanged",
			argv:    []string{"bd", "create", "--repo", "unknown", "--title", "x"},
			want:    []string{"bd", "create", "--repo", "unknown", "--title", "x"},
			wantDir: "",
		},
		{
			name:    "path-like repo unchanged",
			argv:    []string{"bd", "create", "--repo", "gastown/mayor/rig", "--title", "x"},
			want:    []string{"bd", "create", "--repo", "gastown/mayor/rig", "--title", "x"},
			wantDir: "",
		},
		{
			name:    "duplicate repo unchanged",
			argv:    []string{"bd", "create", "--repo", "gastown", "--repo", "/tmp/other"},
			want:    []string{"bd", "create", "--repo", "gastown", "--repo", "/tmp/other"},
			wantDir: "",
		},
		{
			name:    "repo after sentinel unchanged",
			argv:    []string{"bd", "create", "--", "--repo", "gastown"},
			want:    []string{"bd", "create", "--", "--repo", "gastown"},
			wantDir: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, gotDir := RewriteBDCreateRepoAlias(townRoot, tc.argv)
			if gotDir != tc.wantDir {
				t.Fatalf("beads dir = %q, want %q", gotDir, tc.wantDir)
			}
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("argv = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestCheckPrefixAvailable(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix": "gt-", "path": "gastown/mayor/rig"}
{"prefix": "bd-", "path": "beads/mayor/rig"}
{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		prefix  string
		newPath string
		wantErr bool
	}{
		{
			name:    "new prefix is available",
			prefix:  "cr-",
			newPath: "crucible",
			wantErr: false,
		},
		{
			name:    "same rig re-registering same prefix",
			prefix:  "gt-",
			newPath: "gastown",
			wantErr: false,
		},
		{
			name:    "same rig different path variant",
			prefix:  "gt-",
			newPath: "gastown/mayor/rig",
			wantErr: false,
		},
		{
			name:    "collision with different rig",
			prefix:  "gt-",
			newPath: "getresearch",
			wantErr: true,
		},
		{
			name:    "collision with beads prefix",
			prefix:  "bd-",
			newPath: "boardgame",
			wantErr: true,
		},
		{
			name:    "town-level prefix not blocked",
			prefix:  "hq-",
			newPath: "headquarters",
			wantErr: true, // "." rig name conflicts with "headquarters"
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckPrefixAvailable(tmpDir, tc.prefix, tc.newPath)
			if (err != nil) != tc.wantErr {
				t.Errorf("CheckPrefixAvailable(%q, %q) error = %v, wantErr %v",
					tc.prefix, tc.newPath, err, tc.wantErr)
			}
		})
	}
}

func TestCheckPrefixAvailable_NoRoutes(t *testing.T) {
	tmpDir := t.TempDir()
	// No .beads directory — all prefixes should be available
	err := CheckPrefixAvailable(tmpDir, "gt-", "gastown")
	if err != nil {
		t.Errorf("expected no error with no routes file, got: %v", err)
	}
}

func TestAgentBeadIDsWithPrefix(t *testing.T) {
	tests := []struct {
		name     string
		fn       func() string
		expected string
	}{
		{"PolecatBeadIDWithPrefix bd beads obsidian",
			func() string { return PolecatBeadIDWithPrefix("bd", "beads", "obsidian") },
			"bd-beads-polecat-obsidian"},
		{"PolecatBeadIDWithPrefix gt gastown Toast",
			func() string { return PolecatBeadIDWithPrefix("gt", "gastown", "Toast") },
			"gt-gastown-polecat-Toast"},
		{"WitnessBeadIDWithPrefix bd beads",
			func() string { return WitnessBeadIDWithPrefix("bd", "beads") },
			"bd-beads-witness"},
		{"RefineryBeadIDWithPrefix bd beads",
			func() string { return RefineryBeadIDWithPrefix("bd", "beads") },
			"bd-beads-refinery"},
		{"CrewBeadIDWithPrefix bd beads max",
			func() string { return CrewBeadIDWithPrefix("bd", "beads", "max") },
			"bd-beads-crew-max"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.fn()
			if result != tc.expected {
				t.Errorf("got %q, want %q", result, tc.expected)
			}
		})
	}
}

// TestValidateRigPrefix verifies the post-creation prefix guard (gt-gpy).
func TestValidateRigPrefix(t *testing.T) {
	// Set up a town root with routes.jsonl.
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix": "gt-", "path": "gastown/mayor/rig"}
{"prefix": "bd-", "path": "beads/mayor/rig"}
{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		rigName string
		beadID  string
		wantErr bool
	}{
		{
			name:    "same-rig bead: no error",
			rigName: "gastown",
			beadID:  "gt-wisp-abc",
			wantErr: false,
		},
		{
			name:    "cross-rig: hq- bead on gastown rig returns error",
			rigName: "gastown",
			beadID:  "hq-wisp-xyz",
			wantErr: true,
		},
		{
			name:    "bd- bead on beads rig: no error",
			rigName: "beads",
			beadID:  "bd-wisp-123",
			wantErr: false,
		},
		{
			name:    "empty bead ID: no error (can't determine prefix)",
			rigName: "gastown",
			beadID:  "",
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRigPrefix(tmpDir, tc.rigName, tc.beadID)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateRigPrefix(%q, %q) error = %v, wantErr %v", tc.rigName, tc.beadID, err, tc.wantErr)
			}
		})
	}
}
