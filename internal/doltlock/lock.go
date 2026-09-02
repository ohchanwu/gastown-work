// Package doltlock coordinates shared-server ownership publishers and cleanup.
package doltlock

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gofrs/flock"
)

var ownershipMu sync.Mutex

// WithDatabaseOwnership excludes destructive cleanup while ownership is
// created, renamed, or published.
func WithDatabaseOwnership(townRoot string, operation func() error) error {
	if operation == nil {
		return fmt.Errorf("database ownership operation is required")
	}
	ownershipMu.Lock()
	defer ownershipMu.Unlock()

	lockDir := filepath.Join(townRoot, ".runtime", "dolt-database-cleanup")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return fmt.Errorf("creating database ownership lock directory: %w", err)
	}
	if err := os.Chmod(lockDir, 0o700); err != nil {
		return fmt.Errorf("tightening database ownership lock directory: %w", err)
	}
	lockPath := filepath.Join(lockDir, "ownership.lock")
	lock := flock.New(lockPath)
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("locking database ownership: %w", err)
	}
	defer func() { _ = lock.Unlock() }()
	if err := os.Chmod(lockPath, 0o600); err != nil {
		return fmt.Errorf("tightening database ownership lock: %w", err)
	}
	return operation()
}
