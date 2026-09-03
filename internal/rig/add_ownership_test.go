package rig

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

func TestRecoverInterruptedAddRemovesExactCreatedDatabase(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".dolt-data", "recovering", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".dolt-data", "recovering", ".gastown-creation-owner"), []byte("database-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rigPath := filepath.Join(townRoot, "recovering")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAddOwnershipStamp(rigPath, "owner-token"); err != nil {
		t.Fatal(err)
	}
	if err := writeAddDatabaseOwnership(rigPath, addDatabaseOwnership{Owner: "owner-token", DatabaseToken: "database-token", RoutePrefix: "rc-", RoutePath: "recovering", RouteToken: "route-token"}); err != nil {
		t.Fatal(err)
	}
	previous := removeAddDatabase
	t.Cleanup(func() { removeAddDatabase = previous })
	called := false
	removeAddDatabase = func(gotTown, gotName, gotToken string, force bool) error {
		called = true
		if gotTown != townRoot || gotName != "recovering" || gotToken != "database-token" || !force {
			t.Fatalf("cleanup args = %q, %q, %q, %v", gotTown, gotName, gotToken, force)
		}
		return nil
	}

	recovered, err := recoverInterruptedAdd(townRoot, "recovering")
	if err != nil {
		t.Fatal(err)
	}
	if !recovered || !called {
		t.Fatalf("recovered = %v, cleanup called = %v; want true, true", recovered, called)
	}
	if _, err := os.Stat(rigPath); !os.IsNotExist(err) {
		t.Fatalf("interrupted rig path remains: %v", err)
	}
}

func TestRecoverInterruptedAddPreservesPathWhenDatabaseIdentityChanged(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".dolt-data", "recovering", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".dolt-data", "recovering", ".gastown-creation-owner"), []byte("database-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rigPath := filepath.Join(townRoot, "recovering")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAddOwnershipStamp(rigPath, "owner-token"); err != nil {
		t.Fatal(err)
	}
	if err := writeAddDatabaseOwnership(rigPath, addDatabaseOwnership{Owner: "owner-token", DatabaseToken: "database-token", RoutePrefix: "rc-", RoutePath: "recovering", RouteToken: "route-token"}); err != nil {
		t.Fatal(err)
	}
	previous := removeAddDatabase
	t.Cleanup(func() { removeAddDatabase = previous })
	removeAddDatabase = func(string, string, string, bool) error {
		return errors.New("database no longer matches owning creation token")
	}

	if _, err := recoverInterruptedAdd(townRoot, "recovering"); err == nil {
		t.Fatal("recovery succeeded after database identity changed")
	}
	if _, err := os.Stat(filepath.Join(rigPath, addOwnershipStampFile)); err != nil {
		t.Fatalf("recovery evidence was not preserved: %v", err)
	}
}

func TestRecoverInterruptedAddPreservesUnprovenDatabaseAndRetriesUnowned(t *testing.T) {
	t.Setenv("GT_DOLT_PORT", "1")
	townRoot := t.TempDir()
	rigName := "ambiguous"
	token := "ambiguous-database-token"
	dbPath := filepath.Join(townRoot, ".dolt-data", rigName)
	if err := os.MkdirAll(filepath.Join(dbPath, ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	rigPath := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAddOwnershipStamp(rigPath, "owner-token"); err != nil {
		t.Fatal(err)
	}
	if err := writeAddDatabaseOwnership(rigPath, addDatabaseOwnership{
		Owner: "owner-token", DatabaseToken: token,
		RoutePrefix: "am-", RoutePath: rigName, RouteToken: "route-token",
	}); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(token))
	intentDir := filepath.Join(townRoot, ".runtime", "dolt-database-creations")
	if err := os.MkdirAll(intentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	intent := `{"version":3,"database":"` + rigName + `","token":"` + token + `","prepared_unix_nano":1,"generation":"` + hex.EncodeToString(sum[:16]) + `"}`
	intentPath := filepath.Join(intentDir, rigName+".json")
	if err := os.WriteFile(intentPath, []byte(intent+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := removeAddDatabase
	t.Cleanup(func() { removeAddDatabase = previous })
	removeAddDatabase = func(string, string, string, bool) error {
		t.Fatal("unproven database was selected for destructive cleanup")
		return nil
	}
	recovered, err := recoverInterruptedAdd(townRoot, rigName)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered {
		t.Fatal("ambiguous add was not made retryable")
	}
	if _, err := os.Stat(filepath.Join(dbPath, ".dolt")); err != nil {
		t.Fatalf("unproven database was not preserved: %v", err)
	}
	if _, err := os.Stat(intentPath); !os.IsNotExist(err) {
		t.Fatalf("unproven creation intent remains: %v", err)
	}
}

func TestRecoverInterruptedAddPreservesUnownedDatabaseAfterIntentRetirement(t *testing.T) {
	t.Setenv("GT_DOLT_PORT", "1")
	townRoot := t.TempDir()
	rigName := "retired_intent"
	token := "retired-intent-token"
	dbPath := filepath.Join(townRoot, ".dolt-data", rigName)
	if err := os.MkdirAll(filepath.Join(dbPath, ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	rigPath := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAddOwnershipStamp(rigPath, "owner-token"); err != nil {
		t.Fatal(err)
	}
	if err := writeAddDatabaseOwnership(rigPath, addDatabaseOwnership{
		Owner: "owner-token", DatabaseToken: token,
		RoutePrefix: "ri-", RoutePath: rigName, RouteToken: "route-token",
	}); err != nil {
		t.Fatal(err)
	}
	previous := removeAddDatabase
	t.Cleanup(func() { removeAddDatabase = previous })
	removeAddDatabase = func(string, string, string, bool) error {
		t.Fatal("unowned database was selected for destructive cleanup")
		return nil
	}

	recovered, err := recoverInterruptedAdd(townRoot, rigName)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered {
		t.Fatal("retired database intent was not made retryable")
	}
	if _, err := os.Stat(filepath.Join(dbPath, ".dolt")); err != nil {
		t.Fatalf("unowned database was not preserved: %v", err)
	}
	if _, err := os.Stat(rigPath); !os.IsNotExist(err) {
		t.Fatalf("interrupted rig path remains: %v", err)
	}
}

func TestRecoverInterruptedAddRetiresExactAbsentDatabaseIntent(t *testing.T) {
	t.Setenv("GT_DOLT_PORT", "1")
	townRoot := t.TempDir()
	rigName := "never_created"
	token := "database-token"
	rigPath := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAddOwnershipStamp(rigPath, "owner-token"); err != nil {
		t.Fatal(err)
	}
	if err := writeAddDatabaseOwnership(rigPath, addDatabaseOwnership{
		Owner: "owner-token", DatabaseToken: token,
		RoutePrefix: "nc-", RoutePath: rigName, RouteToken: "route-token",
	}); err != nil {
		t.Fatal(err)
	}
	intentDir := filepath.Join(townRoot, ".runtime", "dolt-database-creations")
	if err := os.MkdirAll(intentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Use a stable positive timestamp rather than coupling this recovery test to
	// the creation-intent encoder.
	intent := `{"version":2,"database":"` + rigName + `","token":"` + token + `","prepared_unix_nano":1,"generation":"cc5dfabe424c2f72e6bf7e7c6f13fbd3"}`
	intentPath := filepath.Join(intentDir, rigName+".json")
	if err := os.WriteFile(intentPath, []byte(intent+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	recovered, err := recoverInterruptedAdd(townRoot, rigName)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered {
		t.Fatal("absent database add was not recovered")
	}
	if _, err := os.Stat(intentPath); !os.IsNotExist(err) {
		t.Fatalf("stale database creation intent remains: %v", err)
	}
}

func TestReadAddDatabaseOwnershipAcceptsEcfRootShape(t *testing.T) {
	rigPath := t.TempDir()
	legacy := `{"owner":"owner-token","root":"dolt-root:legacy","route_prefix":"lg-","route_path":"legacy","route_token":"route-token"}`
	if err := os.WriteFile(filepath.Join(rigPath, addDatabaseOwnershipFile), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	ownership, err := readAddDatabaseOwnership(rigPath)
	if err != nil {
		t.Fatal(err)
	}
	if ownership.LegacyRoot != "dolt-root:legacy" || ownership.DatabaseToken != "" {
		t.Fatalf("legacy ownership decoded as %#v", ownership)
	}
}

func TestRecoverInterruptedAddUsesLegacyRootProof(t *testing.T) {
	townRoot := t.TempDir()
	rigName := "legacy"
	rigPath := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAddOwnershipStamp(rigPath, "owner-token"); err != nil {
		t.Fatal(err)
	}
	legacy := `{"owner":"owner-token","root":"dolt-root:legacy","route_prefix":"lg-","route_path":"legacy","route_token":"route-token"}`
	if err := os.WriteFile(filepath.Join(rigPath, addDatabaseOwnershipFile), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, ".dolt-data", rigName, ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := removeLegacyAddDatabase
	t.Cleanup(func() { removeLegacyAddDatabase = previous })
	called := false
	removeLegacyAddDatabase = func(gotTown, gotName, gotRoot string, force bool) error {
		called = true
		if gotTown != townRoot || gotName != rigName || gotRoot != "dolt-root:legacy" || !force {
			t.Fatalf("legacy cleanup args = %q, %q, %q, %v", gotTown, gotName, gotRoot, force)
		}
		return nil
	}
	recovered, err := recoverInterruptedAdd(townRoot, rigName)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered || !called {
		t.Fatalf("recovered=%v legacy cleanup called=%v", recovered, called)
	}
}

func TestMigrateAndValidateAddRegistrationVersionsLegacyMarker(t *testing.T) {
	townRoot := t.TempDir()
	rigName := "legacy_pending"
	rigPath := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAddOwnershipStamp(rigPath, "owner-token"); err != nil {
		t.Fatal(err)
	}
	legacy := `{"owner":"owner-token","root":"dolt-root:legacy","route_prefix":"lp-","route_path":"legacy_pending","route_token":"route-token"}`
	if err := os.WriteFile(filepath.Join(rigPath, addDatabaseOwnershipFile), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := migrateLegacyAddDatabase
	t.Cleanup(func() { migrateLegacyAddDatabase = previous })
	migrateLegacyAddDatabase = func(gotTown, gotName, gotToken, gotRoot string) error {
		if gotTown != townRoot || gotName != rigName || gotToken != "owner-token" || gotRoot != "dolt-root:legacy" {
			t.Fatalf("legacy migration args = %q, %q, %q, %q", gotTown, gotName, gotToken, gotRoot)
		}
		return nil
	}
	entry := config.RigEntry{
		RegistrationToken: "route-token", RegistrationPending: true,
		RegistrationPathToken: "owner-token",
	}
	if err := migrateAndValidateAddRegistration(townRoot, rigName, beads.Route{Prefix: "lp-", Path: rigName}, false, &entry); err != nil {
		t.Fatal(err)
	}
	if entry.RegistrationDatabaseToken != "owner-token" || entry.RegistrationDatabase != rigName {
		t.Fatalf("migrated entry = %#v", entry)
	}
	ownership, err := readAddDatabaseOwnership(rigPath)
	if err != nil {
		t.Fatal(err)
	}
	if ownership.Version != addDatabaseOwnershipVersion || ownership.DatabaseToken != "owner-token" || ownership.LegacyRoot != "" {
		t.Fatalf("migrated marker = %#v", ownership)
	}
}

func TestRemoveRigPathIfOwned_MatchingStampRemovesPath(t *testing.T) {
	rigPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rigPath, "some-file"), []byte("x"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	stamp, err := newAddOwnershipStamp()
	if err != nil {
		t.Fatalf("new ownership stamp: %v", err)
	}
	if err := writeAddOwnershipStamp(rigPath, stamp); err != nil {
		t.Fatalf("write ownership stamp: %v", err)
	}

	removeRigPathIfOwned(rigPath, stamp)

	if _, err := os.Stat(rigPath); !os.IsNotExist(err) {
		t.Fatalf("expected rig path to be removed, stat err=%v", err)
	}
}

func TestRemoveRigPathIfOwned_MismatchedStampKeepsPath(t *testing.T) {
	rigPath := t.TempDir()
	preserved := filepath.Join(rigPath, "preserve-me")
	if err := os.WriteFile(preserved, []byte("important"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := writeAddOwnershipStamp(rigPath, "newer-stamp"); err != nil {
		t.Fatalf("write ownership stamp: %v", err)
	}

	removeRigPathIfOwned(rigPath, "older-stamp")

	if _, err := os.Stat(preserved); err != nil {
		t.Fatalf("preserved file was deleted: %v", err)
	}
}

func TestRemoveRigPathIfOwned_MissingStampOnNonEmptyPathKeepsPath(t *testing.T) {
	rigPath := t.TempDir()
	preserved := filepath.Join(rigPath, "rig-content")
	if err := os.WriteFile(preserved, []byte("important"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	removeRigPathIfOwned(rigPath, "stale-stamp")

	if _, err := os.Stat(preserved); err != nil {
		t.Fatalf("preserved file was deleted: %v", err)
	}
}

func TestRemoveRigPathIfOwned_MissingStampOnEmptyPathRemovesPath(t *testing.T) {
	rigPath := t.TempDir()

	removeRigPathIfOwned(rigPath, "stale-stamp")

	if _, err := os.Stat(rigPath); !os.IsNotExist(err) {
		t.Fatalf("expected empty rig path to be removed, stat err=%v", err)
	}
}

func TestRemoveRigPathIfOwned_NoExpectedStampRemovesPath(t *testing.T) {
	rigPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rigPath, "x"), []byte("x"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	removeRigPathIfOwned(rigPath, "")

	if _, err := os.Stat(rigPath); !os.IsNotExist(err) {
		t.Fatalf("expected rig path to be removed, stat err=%v", err)
	}
}
