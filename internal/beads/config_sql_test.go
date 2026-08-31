package beads

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"testing"
	"time"
)

type blockingCleanupDriver struct{}

type blockingCleanupConn struct{}

func (blockingCleanupDriver) Open(string) (driver.Conn, error)  { return blockingCleanupConn{}, nil }
func (blockingCleanupConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (blockingCleanupConn) Close() error                        { return nil }
func (blockingCleanupConn) Begin() (driver.Tx, error)           { return nil, errors.New("unused") }
func (blockingCleanupConn) ExecContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

var registerBlockingCleanupDriver sync.Once

func TestRollbackConnBoundedReleasesBlockingCleanup(t *testing.T) {
	registerBlockingCleanupDriver.Do(func() { sql.Register("gt-blocking-cleanup", blockingCleanupDriver{}) })
	db, err := sql.Open("gt-blocking-cleanup", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	previous := sqlCleanupTimeout
	sqlCleanupTimeout = 20 * time.Millisecond
	t.Cleanup(func() { sqlCleanupTimeout = previous })
	started := time.Now()
	rollbackConnBounded(conn)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded rollback took %s", elapsed)
	}
}
