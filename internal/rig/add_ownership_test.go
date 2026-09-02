package rig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoverInterruptedAddRemovesExactCreatedDatabase(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".dolt-data", "recovering", ".dolt"), 0o755); err != nil {
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
