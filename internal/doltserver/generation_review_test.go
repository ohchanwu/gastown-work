package doltserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCancelDatabaseCreationIntentAcceptsV1AbsentIntent(t *testing.T) {
	t.Setenv("GT_DOLT_PORT", "1")
	townRoot := t.TempDir()
	dbName := "parent_v1_absent"
	token := "parent-v1-intent-token"
	intent := databaseCreationIntent{
		Version:      1,
		Database:     dbName,
		Token:        token,
		PreparedNano: time.Now().UnixNano(),
	}
	data, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	path := databaseCreationIntentPath(townRoot, dbName)
	if err := ensurePrivateDatabaseCleanupDirectory(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if err := writeDatabaseCleanupFileDurable(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := CancelDatabaseCreationIntentIfAbsent(townRoot, dbName, token); err != nil {
		t.Fatalf("exact parent v1 absent intent was not recoverable: %v", err)
	}
}

func TestReleaseDatabaseCreationTokenAcceptsV1Owner(t *testing.T) {
	townRoot, _ := startOwnedDatabaseTestServer(t)
	dbName := "parent_v1_owner"
	token := "parent-v1-owner-token"
	if err := serverExecSQL(townRoot, "CREATE DATABASE `parent_v1_owner`"); err != nil {
		t.Fatal(err)
	}
	if err := waitForCatalog(townRoot, dbName); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(DefaultConfig(townRoot).DataDir, dbName)
	if err := os.WriteFile(filepath.Join(dbPath, databaseCreationOwnerFile), []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ReleaseDatabaseCreationToken(townRoot, dbName, token); err != nil {
		t.Fatalf("exact parent v1 owner was not recoverable: %v", err)
	}
}

func TestRecoverDatabaseCreationTokenRejectsReplayedGenerationTag(t *testing.T) {
	townRoot, pid := startOwnedDatabaseTestServer(t)
	dbName := "replayed_generation"
	token := "replayed-generation-token"
	if err := prepareDatabaseCreationIntent(townRoot, dbName, token, pid); err != nil {
		t.Fatal(err)
	}
	intent, err := readDatabaseCreationIntent(databaseCreationIntentPath(townRoot, dbName))
	if err != nil {
		t.Fatal(err)
	}
	if err := serverExecSQL(townRoot, ownedDatabaseCreateQuery(dbName, intent.Generation)); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(DefaultConfig(townRoot).DataDir, dbName)
	if err := installDatabaseGeneration(townRoot, dbName, dbPath, intent.Generation); err != nil {
		t.Fatal(err)
	}
	tag := databaseGenerationTag(intent.Generation)
	query := "DROP DATABASE `replayed_generation`; CREATE DATABASE `replayed_generation`; USE `replayed_generation`; CALL DOLT_TAG('" + tag + "')"
	if err := serverExecSQL(townRoot, query); err != nil {
		t.Fatal(err)
	}
	if err := waitForCatalog(townRoot, dbName); err != nil {
		t.Fatal(err)
	}

	if err := RecoverDatabaseCreationToken(townRoot, dbName, token); err == nil {
		t.Fatal("replacement that replayed the public generation tag was accepted")
	}
}

func TestRecoverDatabaseCreationTokenAcrossServerRestart(t *testing.T) {
	townRoot, pid := startOwnedDatabaseTestServer(t)
	dbName := "restart_recovery"
	token := "restart-recovery-token"
	if err := prepareDatabaseCreationIntent(townRoot, dbName, token, pid); err != nil {
		t.Fatal(err)
	}
	intent, err := readDatabaseCreationIntent(databaseCreationIntentPath(townRoot, dbName))
	if err != nil {
		t.Fatal(err)
	}
	if err := createOwnedDatabaseOnServer(townRoot, dbName, intent.Generation); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(DefaultConfig(townRoot).DataDir, dbName)
	if err := installDatabaseGeneration(townRoot, dbName, dbPath, intent.Generation); err != nil {
		t.Fatal(err)
	}
	if err := Stop(townRoot); err != nil {
		t.Fatal(err)
	}
	if err := Start(townRoot); err != nil {
		t.Fatal(err)
	}
	if err := RecoverDatabaseCreationToken(townRoot, dbName, token); err != nil {
		t.Fatalf("recovering generation-bound creation after restart: %v", err)
	}
	if err := verifyDatabaseCreationToken(townRoot, dbName, dbPath, token); err != nil {
		t.Fatal(err)
	}
}

func TestCancelDatabaseCreationIntentWaitsForDelayedCreate(t *testing.T) {
	townRoot, pid := startOwnedDatabaseTestServer(t)
	dbName := "delayed_create"
	token := "delayed-create-token"
	if err := prepareDatabaseCreationIntent(townRoot, dbName, token, pid); err != nil {
		t.Fatal(err)
	}
	intent, err := readDatabaseCreationIntent(databaseCreationIntentPath(townRoot, dbName))
	if err != nil {
		t.Fatal(err)
	}
	previous := createOwnedDatabaseStatement
	started := make(chan struct{})
	proceed := make(chan struct{})
	createOwnedDatabaseStatement = func(ctx context.Context, conn *sql.Conn, query string) error {
		close(started)
		<-proceed
		_, err := conn.ExecContext(ctx, query)
		return err
	}
	t.Cleanup(func() { createOwnedDatabaseStatement = previous })
	createDone := make(chan error, 1)
	go func() {
		createDone <- createOwnedDatabaseOnServer(townRoot, dbName, intent.Generation)
	}()
	<-started
	cancelDone := make(chan error, 1)
	go func() {
		cancelDone <- CancelDatabaseCreationIntentIfAbsent(townRoot, dbName, token)
	}()
	select {
	case err := <-cancelDone:
		t.Fatalf("cancellation returned before create completed: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(proceed)
	if err := <-createDone; err != nil {
		t.Fatal(err)
	}
	if err := <-cancelDone; err == nil {
		t.Fatal("cancellation erased an intent after the delayed database materialized")
	}
	if _, err := os.Stat(databaseCreationIntentPath(townRoot, dbName)); err != nil {
		t.Fatalf("delayed creation intent was not preserved: %v", err)
	}
}

func TestInitRigOwnedDoesNotGrantLiveCreateRollbackAuthority(t *testing.T) {
	townRoot, _ := startOwnedDatabaseTestServer(t)
	dbName := "unowned_live_create"
	token := "unowned-live-create-token"

	_, created, owned, err := InitRigOwned(townRoot, dbName, token)
	if err != nil {
		t.Fatal(err)
	}
	if !created || owned != "" {
		t.Fatalf("InitRigOwned() = created %v, owner %q", created, owned)
	}
	dbPath := filepath.Join(DefaultConfig(townRoot).DataDir, dbName)
	if _, err := os.Stat(filepath.Join(dbPath, databaseCreationOwnerFile)); !os.IsNotExist(err) {
		t.Fatalf("live create received a database owner: %v", err)
	}
	if _, err := os.Stat(databaseGenerationAnchorPath(townRoot, dbName, databaseGenerationID(token))); !os.IsNotExist(err) {
		t.Fatalf("live create received a generation anchor: %v", err)
	}
	if err := RetireUnprovenDatabaseCreationIntent(townRoot, dbName, token); err != nil {
		t.Fatalf("retiring non-destructive creation intent: %v", err)
	}
}

func TestInitRigOwnedRejectsInterposedReplacementRollbackAuthority(t *testing.T) {
	townRoot, _ := startOwnedDatabaseTestServer(t)
	dbName := "interposed_replacement"
	token := "interposed-replacement-token"
	previous := createOwnedDatabaseStatement
	createOwnedDatabaseStatement = func(ctx context.Context, conn *sql.Conn, query string) error {
		if _, err := conn.ExecContext(ctx, query); err != nil {
			return err
		}
		return serverExecSQL(townRoot, "DROP DATABASE `interposed_replacement`; CREATE DATABASE `interposed_replacement`")
	}
	t.Cleanup(func() { createOwnedDatabaseStatement = previous })

	_, created, owned, err := InitRigOwned(townRoot, dbName, token)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("interposed replacement was not observed as a new live create")
	}
	if owned != "" {
		t.Fatalf("replacement created between CREATE and anchor publication received deletion authority %q", owned)
	}
	dbPath := filepath.Join(DefaultConfig(townRoot).DataDir, dbName)
	if _, statErr := os.Stat(filepath.Join(dbPath, databaseCreationOwnerFile)); !os.IsNotExist(statErr) {
		t.Fatalf("interposed replacement received a database owner: %v", statErr)
	}
	if _, statErr := os.Stat(databaseGenerationAnchorPath(townRoot, dbName, databaseGenerationID(token))); !os.IsNotExist(statErr) {
		t.Fatalf("interposed replacement received a generation anchor: %v", statErr)
	}
}

func TestRemoveDatabaseRefusesUnconditionalLiveDrop(t *testing.T) {
	townRoot, _ := startOwnedDatabaseTestServer(t)
	dbName := "live_drop_refused"
	if err := serverExecSQL(townRoot, "CREATE DATABASE `live_drop_control`; CREATE DATABASE `live_drop_refused`"); err != nil {
		t.Fatal(err)
	}
	if err := waitForCatalog(townRoot, dbName); err != nil {
		t.Fatal(err)
	}

	err := RemoveDatabase(townRoot, dbName, true)
	if err == nil || !strings.Contains(err.Error(), "stopped Dolt server") {
		t.Fatalf("live cleanup error = %v, want offline-only refusal", err)
	}
	if !DatabaseExists(townRoot, dbName) {
		t.Fatal("live cleanup removed a database without atomic identity-bound DROP")
	}
	if _, err := os.Stat(databaseCleanupReceiptPath(townRoot, dbName)); !os.IsNotExist(err) {
		t.Fatalf("live refusal left a destructive cleanup receipt: %v", err)
	}
}
