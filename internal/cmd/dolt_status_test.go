package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gtconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/doltserver"
)

func TestRenderLocalDoltInventoryReportsAllClassesWithoutCustodySecrets(t *testing.T) {
	secretPath := "/private/test-owner/path"
	secretToken := strings.Repeat("ab", 32)
	inventory := []doltserver.LocalDoltServer{
		{DoltListener: doltserver.DoltListener{PID: 50, Port: 4505}, Class: doltserver.DoltServerUnknown, OwnerPath: secretPath, ProcessToken: secretToken},
		{DoltListener: doltserver.DoltListener{PID: 20, Port: 4502}, Class: doltserver.DoltServerOwnedTestLeak, OwnerPath: secretPath, ProcessToken: secretToken},
		{DoltListener: doltserver.DoltListener{PID: 10, Port: 4501}, Class: doltserver.DoltServerCanonical, OwnerPath: secretPath, ProcessToken: secretToken},
		{DoltListener: doltserver.DoltListener{PID: 40, Port: 4504}, Class: doltserver.DoltServerOwnedTownLeak, OwnerPath: secretPath, ProcessToken: secretToken},
		{DoltListener: doltserver.DoltListener{PID: 30, Port: 4503}, Class: doltserver.DoltServerConfiguredPortImposter, OwnerPath: secretPath, ProcessToken: secretToken},
	}
	var out bytes.Buffer

	err := renderLocalDoltInventory(&out, true, func() ([]doltserver.LocalDoltServer, error) {
		return inventory, nil
	})
	if err != nil {
		t.Fatalf("renderLocalDoltInventory: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Local Dolt listeners:",
		"PID 10 port 4501 (canonical)",
		"PID 30 port 4503 (configured-port-imposter)",
		"PID 40 port 4504 (owned-town-leak)",
		"PID 20 port 4502 (owned-test-leak)",
		"PID 50 port 4505 (unknown)",
		"Totals: canonical=1 configured-port-imposter=1 owned-town-leak=1 owned-test-leak=1 unknown=1",
		"Preview exact test-leak cleanup: gt dolt cleanup-test-leaks",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("inventory output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, secretPath) || strings.Contains(got, secretToken) {
		t.Fatalf("inventory output exposed private custody:\n%s", got)
	}
	if canonical, configured := strings.Index(got, "(canonical)"), strings.Index(got, "(configured-port-imposter)"); canonical < 0 || configured < canonical {
		t.Fatalf("inventory output was not sorted by class:\n%s", got)
	}
}

func TestRenderLocalDoltInventoryFailsClosedOnDiscoveryError(t *testing.T) {
	want := errors.New("lsof unavailable")
	var out bytes.Buffer
	err := renderLocalDoltInventory(&out, true, func() ([]doltserver.LocalDoltServer, error) {
		return nil, want
	})
	if !errors.Is(err, want) || !strings.Contains(out.String(), "local listener inventory failed") {
		t.Fatalf("output = %q, error = %v, want explicit failure preserving %v", out.String(), err, want)
	}
}

func TestRenderLocalDoltInventoryReportsUnsupportedWithoutClaimingClean(t *testing.T) {
	var out bytes.Buffer
	err := renderLocalDoltInventory(&out, false, func() ([]doltserver.LocalDoltServer, error) {
		t.Fatal("unsupported platform called inventory")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("renderLocalDoltInventory: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "unsupported") || strings.Contains(got, "none") || strings.Contains(got, "clean") {
		t.Fatalf("unsupported output made a false clean claim: %q", got)
	}
}

func TestReadBeadsRuntimeConfigServerMetadata(t *testing.T) {
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir beads dir: %v", err)
	}
	metadata := `{
  "backend": "dolt",
  "dolt_mode": "server",
  "dolt_server_host": "192.0.2.10",
  "dolt_server_port": 4311,
  "dolt_database": "gastown"
}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	cfg, ok := readBeadsRuntimeConfig(beadsDir)
	if !ok {
		t.Fatal("readBeadsRuntimeConfig did not detect server metadata")
	}
	if cfg.Database != "gastown" {
		t.Fatalf("Database = %q, want gastown", cfg.Database)
	}
	if cfg.Host != "192.0.2.10" {
		t.Fatalf("Host = %q, want 192.0.2.10", cfg.Host)
	}
	if cfg.Port != 4311 {
		t.Fatalf("Port = %d, want 4311", cfg.Port)
	}
}

func TestReadBeadsRuntimeConfigDefaultServerAddr(t *testing.T) {
	t.Setenv("GT_DOLT_PORT", "32769")

	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir beads dir: %v", err)
	}
	metadata := `{
  "backend": "dolt",
  "dolt_mode": "server",
  "database": "dolt"
}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	cfg, ok := readBeadsRuntimeConfig(beadsDir)
	if !ok {
		t.Fatal("readBeadsRuntimeConfig did not detect server metadata")
	}
	if cfg.Host != "127.0.0.1" {
		t.Fatalf("Host = %q, want 127.0.0.1", cfg.Host)
	}
	if cfg.Port != doltserver.DefaultPort {
		t.Fatalf("Port = %d, want default %d", cfg.Port, doltserver.DefaultPort)
	}
}

func TestReadBeadsRuntimeConfigPortFileFallback(t *testing.T) {
	t.Setenv("GT_DOLT_PORT", "32769")

	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir beads dir: %v", err)
	}
	metadata := `{
  "backend": "dolt",
  "dolt_mode": "server",
  "database": "dolt"
}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.port"), []byte("43113\n"), 0600); err != nil {
		t.Fatalf("write port file: %v", err)
	}

	cfg, ok := readBeadsRuntimeConfig(beadsDir)
	if !ok {
		t.Fatal("readBeadsRuntimeConfig did not detect server metadata")
	}
	if cfg.Port != 43113 {
		t.Fatalf("Port = %d, want port file 43113", cfg.Port)
	}
}

func TestReadBeadsRuntimeConfigIgnoresEmbeddedMetadata(t *testing.T) {
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir beads dir: %v", err)
	}
	metadata := `{
  "backend": "dolt",
  "dolt_mode": "embedded",
  "dolt_database": "gastown"
}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	if _, ok := readBeadsRuntimeConfig(beadsDir); ok {
		t.Fatal("embedded metadata should not be reported as shared-server config")
	}
}

func TestBeadsScopeHint_HQWarnsAgainstGlobal(t *testing.T) {
	townRoot := filepath.Join(string(filepath.Separator), "custom", "town root")
	hint := beadsScopeHint("hq", townRoot)

	for _, want := range []string{"database hq", "bd -C " + gtconfig.ShellQuote(townRoot), "bd --global", "beads_global"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("beadsScopeHint() missing %q in:\n%s", want, hint)
		}
	}
	if strings.Contains(hint, "~/gt") {
		t.Fatalf("beadsScopeHint() should not hardcode ~/gt:\n%s", hint)
	}
}

func TestBeadsScopeHint_NonHQEmpty(t *testing.T) {
	if hint := beadsScopeHint("gastown", "/custom/town"); hint != "" {
		t.Fatalf("beadsScopeHint() = %q, want empty", hint)
	}
}
