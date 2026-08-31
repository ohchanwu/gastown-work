package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/steveyegge/gastown/internal/constants"

	_ "github.com/go-sql-driver/mysql"
)

// DoltCommitOutcomeUnknownError means SQL may have reached the Dolt working
// set, but the durable commit acknowledgement was lost or failed.
type DoltCommitOutcomeUnknownError struct {
	Phase string
	Err   error
}

func (e *DoltCommitOutcomeUnknownError) Error() string {
	return fmt.Sprintf("%s outcome unknown: %v", e.Phase, e.Err)
}

func (e *DoltCommitOutcomeUnknownError) Unwrap() error { return e.Err }

var execPinnedDoltCommit = func(ctx context.Context, conn *sql.Conn, message string) error {
	_, err := conn.ExecContext(ctx, "CALL DOLT_COMMIT('--allow-empty', '-Am', ?)", message)
	return err
}

var sqlCleanupTimeout = constants.BdCommandTimeout

func mutateAndDoltCommit(beadsDir, message string, mutate func(context.Context, *sql.Conn) error) error {
	_, err := mutateAndDoltCommitWithOID(beadsDir, message, mutate)
	return err
}

func mutateAndDoltCommitWithOID(beadsDir, message string, mutate func(context.Context, *sql.Conn) error) (string, error) {
	db, err := openDoltSQL(beadsDir)
	if err != nil {
		return "", err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), constants.BdCommandTimeout)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SET @@autocommit = 0"); err != nil {
		return "", fmt.Errorf("disabling SQL autocommit: %w", err)
	}
	commitAttempted := false
	defer func() {
		if !commitAttempted {
			rollbackConnBounded(conn)
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), sqlCleanupTimeout)
		defer cleanupCancel()
		_, _ = conn.ExecContext(cleanupCtx, "SET @@autocommit = 1")
	}()
	if err := mutate(ctx, conn); err != nil {
		return "", err
	}
	commitAttempted = true
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return "", &DoltCommitOutcomeUnknownError{Phase: "SQL commit", Err: err}
	}
	if err := execPinnedDoltCommit(ctx, conn, message); err != nil {
		return "", &DoltCommitOutcomeUnknownError{Phase: "Dolt commit", Err: err}
	}
	oid, err := doltCommitOIDForMessage(ctx, conn, message)
	if err != nil {
		return "", &DoltCommitOutcomeUnknownError{Phase: "Dolt commit receipt", Err: err}
	}
	return oid, nil
}

func rollbackConnBounded(conn *sql.Conn) {
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), sqlCleanupTimeout)
	defer cleanupCancel()
	_, _ = conn.ExecContext(cleanupCtx, "ROLLBACK")
}

func doltCommitOIDForMessage(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, message string) (string, error) {
	var oid string
	if err := q.QueryRowContext(ctx, `SELECT commit_hash FROM dolt_log WHERE message = ? ORDER BY date DESC LIMIT 1`, message).Scan(&oid); err != nil {
		return "", err
	}
	if oid == "" {
		return "", fmt.Errorf("empty commit hash for message %q", message)
	}
	return oid, nil
}

func doltCommitOIDForMessageInStore(beadsDir, message string) (string, error) {
	db, err := openDoltSQL(beadsDir)
	if err != nil {
		return "", err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), sqlCleanupTimeout)
	defer cancel()
	return doltCommitOIDForMessage(ctx, db, message)
}

func finishDoltCommitWithOID(beadsDir, message string) (string, error) {
	db, err := openDoltSQL(beadsDir)
	if err != nil {
		return "", err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), sqlCleanupTimeout)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := execPinnedDoltCommit(ctx, conn, message); err != nil {
		return "", err
	}
	return doltCommitOIDForMessage(ctx, conn, message)
}

func legacyStoreWithoutMetadata(beadsDir string) (bool, error) {
	_, err := os.Stat(filepath.Join(beadsDir, "metadata.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("missing Dolt metadata in %s", beadsDir)
	}
	if err != nil {
		return false, fmt.Errorf("reading Dolt metadata: %w", err)
	}
	if DatabaseNameFromMetadata(beadsDir) == "" {
		return false, fmt.Errorf("invalid Dolt metadata in %s: missing dolt_database", beadsDir)
	}
	return false, nil
}

// EnsureDoltConfigValue writes a config key directly to the configured Dolt
// database. Fresh setup uses this to avoid older bd config/schema bootstrap paths.
func EnsureDoltConfigValue(beadsDir, key, value string) error {
	return mutateAndDoltCommit(beadsDir, "gt: persist config "+key, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, "REPLACE INTO config (`key`, `value`) VALUES (?, ?)", key, value)
		return err
	})
}

func openDoltSQL(beadsDir string) (*sql.DB, error) {
	if beadsDir == "" {
		return nil, fmt.Errorf("empty beads directory")
	}
	database := DatabaseNameFromMetadata(beadsDir)
	if database == "" {
		return nil, fmt.Errorf("missing dolt_database in %s", beadsDir)
	}
	meta := readDoltMetadata(beadsDir)
	host := meta.Host
	if host == "" {
		host = os.Getenv("GT_DOLT_HOST")
	}
	if host == "" {
		host = "127.0.0.1"
	}
	port := meta.Port
	if port == "" {
		port = os.Getenv("BEADS_DOLT_SERVER_PORT")
	}
	if port == "" {
		port = os.Getenv("BEADS_DOLT_PORT")
	}
	if port == "" {
		port = os.Getenv("GT_DOLT_PORT")
	}
	if port == "" {
		port = "3307"
	}
	if _, err := strconv.Atoi(port); err != nil {
		return nil, fmt.Errorf("invalid Dolt port %q: %w", port, err)
	}

	timeout := url.QueryEscape(constants.BdCommandTimeout.String())
	dsn := fmt.Sprintf("root@tcp(%s)/%s?parseTime=true&clientFoundRows=true&timeout=%s&readTimeout=%s&writeTimeout=%s",
		net.JoinHostPort(host, port), url.PathEscape(database), timeout, timeout, timeout)
	return sql.Open("mysql", dsn)
}

// CompareAndUpdateIssueDescription updates one exact issue snapshot. Dolt-backed
// workspaces use an affected-row CAS; the fallback only supports legacy/local
// stores that have no Dolt metadata.
func (b *Beads) CompareAndUpdateIssueDescription(id, expectedStatus, expectedAssignee, expectedDescription, newDescription string) error {
	beadsDir := b.getResolvedBeadsDir()
	legacy, err := legacyStoreWithoutMetadata(beadsDir)
	if err != nil {
		return err
	}
	if legacy {
		issue, err := b.Show(id)
		if err != nil {
			return err
		}
		if issue.Status != expectedStatus || issue.Assignee != expectedAssignee || issue.Description != expectedDescription {
			return fmt.Errorf("%w: issue snapshot", ErrAgentFieldsChanged)
		}
		return b.Update(id, UpdateOptions{Description: &newDescription})
	}
	return mutateAndDoltCommit(beadsDir, "gt: compare-and-update issue description "+id, func(ctx context.Context, conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx, `UPDATE issues
		SET description = ?, updated_at = CURRENT_TIMESTAMP(6)
		WHERE id = ?
		  AND COALESCE(description, '') = ?
		  AND COALESCE(assignee, '') = ?
		  AND status = ?`, newDescription, id, expectedDescription, expectedAssignee, expectedStatus)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return fmt.Errorf("%w: issue snapshot", ErrAgentFieldsChanged)
		}
		return nil
	})
}

var beforeUnbondedMoleculeAttachmentCAS func() error

// CompareAndClearUnbondedMoleculeAttachment clears an exact metadata snapshot
// only while the issue has no live molecule ownership bond.
func (b *Beads) CompareAndClearUnbondedMoleculeAttachment(id, expectedStatus, expectedAssignee, expectedDescription, newDescription string) error {
	beadsDir := b.getResolvedBeadsDir()
	if _, err := legacyStoreWithoutMetadata(beadsDir); err != nil {
		return err
	}
	unlock, err := b.lockBead(id)
	if err != nil {
		return fmt.Errorf("locking metadata-only molecule cleanup: %w", err)
	}
	if beforeUnbondedMoleculeAttachmentCAS != nil {
		if err := beforeUnbondedMoleculeAttachmentCAS(); err != nil {
			unlock()
			return err
		}
	}
	mutationErr := mutateAndDoltCommit(beadsDir, "gt: clear unbonded molecule attachment "+id, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE"); err != nil {
			return fmt.Errorf("setting metadata-only cleanup isolation: %w", err)
		}
		for _, table := range []string{"dependencies", "wisp_dependencies"} {
			rows, err := conn.QueryContext(ctx, `SELECT id FROM `+table+`
				WHERE type IN (`+MoleculeOwnershipDependencySQLList+`)
				  AND COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external) = ?
				FOR UPDATE`, id)
			if err != nil {
				return fmt.Errorf("locking molecule ownership range in %s: %w", table, err)
			}
			for rows.Next() {
				var dependencyID string
				if err := rows.Scan(&dependencyID); err != nil {
					_ = rows.Close()
					return err
				}
			}
			if err := rows.Close(); err != nil {
				return err
			}
		}
		result, err := conn.ExecContext(ctx, `UPDATE issues
			SET description = ?, updated_at = CURRENT_TIMESTAMP(6)
			WHERE id = ? AND status = ? AND COALESCE(assignee, '') = ?
			  AND COALESCE(description, '') = ?
			  AND NOT EXISTS (
			      SELECT 1 FROM dependencies d JOIN issues root ON root.id = d.issue_id
			       WHERE (root.issue_type = 'molecule' OR EXISTS (
			                 SELECT 1 FROM labels WHERE labels.issue_id = root.id AND labels.label = 'gt:molecule'
			             ))
			         AND root.status NOT IN ('closed', 'tombstone')
			         AND d.type IN (`+MoleculeOwnershipDependencySQLList+`)
			         AND COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external) = issues.id
			  )
			  AND NOT EXISTS (
			      SELECT 1 FROM wisp_dependencies d JOIN wisps root ON root.id = d.issue_id
			       WHERE (root.issue_type = 'molecule' OR EXISTS (
			                 SELECT 1 FROM wisp_labels WHERE wisp_labels.issue_id = root.id AND wisp_labels.label = 'gt:molecule'
			             ))
			         AND root.status NOT IN ('closed', 'tombstone')
			         AND d.type IN (`+MoleculeOwnershipDependencySQLList+`)
			         AND COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external) = issues.id
			  )`, newDescription, id, expectedStatus, expectedAssignee, expectedDescription)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return fmt.Errorf("%w: issue or dependency generation", ErrAgentFieldsChanged)
		}
		return nil
	})
	unlock()
	if mutationErr != nil {
		return mutationErr
	}
	bonded, verifyErr := b.hasLiveMoleculeOwnershipBond(id)
	if verifyErr == nil && !bonded {
		return nil
	}
	restoreErr := b.CompareAndUpdateIssueDescription(id, expectedStatus, expectedAssignee, newDescription, expectedDescription)
	return errors.Join(fmt.Errorf("%w: molecule ownership appeared during metadata cleanup", ErrAgentFieldsChanged), verifyErr, restoreErr)
}

func (b *Beads) hasLiveMoleculeOwnershipBond(id string) (bool, error) {
	db, err := openDoltSQL(b.getResolvedBeadsDir())
	if err != nil {
		return false, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), sqlCleanupTimeout)
	defer cancel()
	var bonded bool
	err = db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM dependencies d JOIN issues root ON root.id = d.issue_id
		 WHERE (root.issue_type = 'molecule' OR EXISTS (
		           SELECT 1 FROM labels WHERE labels.issue_id = root.id AND labels.label = 'gt:molecule'
		       ))
		   AND root.status NOT IN ('closed', 'tombstone')
		   AND d.type IN (`+MoleculeOwnershipDependencySQLList+`)
		   AND COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external) = ?
		UNION ALL
		SELECT 1 FROM wisp_dependencies d JOIN wisps root ON root.id = d.issue_id
		 WHERE (root.issue_type = 'molecule' OR EXISTS (
		           SELECT 1 FROM wisp_labels WHERE wisp_labels.issue_id = root.id AND wisp_labels.label = 'gt:molecule'
		       ))
		   AND root.status NOT IN ('closed', 'tombstone')
		   AND d.type IN (`+MoleculeOwnershipDependencySQLList+`)
		   AND COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external) = ?
	)`, id, id).Scan(&bonded)
	return bonded, err
}

func (b *Beads) compareAndUpdateIssueAssignment(id, expectedStatus, expectedAssignee, newStatus, newAssignee string) (bool, error) {
	beadsDir := b.getResolvedBeadsDir()
	legacy, err := legacyStoreWithoutMetadata(beadsDir)
	if err != nil {
		return true, err
	}
	if legacy {
		return false, nil
	}
	err = mutateAndDoltCommit(beadsDir, "gt: compare-and-update issue assignment "+id, func(ctx context.Context, conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx, `UPDATE issues
		SET status = ?, assignee = ?, updated_at = CURRENT_TIMESTAMP(6)
		WHERE id = ? AND status = ? AND COALESCE(assignee, '') = ?`,
			newStatus, newAssignee, id, expectedStatus, expectedAssignee)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return fmt.Errorf("%w: work status or assignee", ErrAgentFieldsChanged)
		}
		return nil
	})
	return true, err
}

// CompareAndRestoreIssueSnapshotIfMatches restores description, status, and
// assignee from one exact snapshot in one durable transaction.
func (b *Beads) CompareAndRestoreIssueSnapshotIfMatches(id, expectedStatus, expectedAssignee, expectedDescription, restoreStatus, restoreAssignee, restoreDescription string) error {
	beadsDir := b.getResolvedBeadsDir()
	legacy, err := legacyStoreWithoutMetadata(beadsDir)
	if err != nil {
		return err
	}
	if legacy {
		issue, err := b.Show(id)
		if err != nil {
			return err
		}
		if issue.Status != expectedStatus || issue.Assignee != expectedAssignee || issue.Description != expectedDescription {
			return fmt.Errorf("%w: issue snapshot", ErrAgentFieldsChanged)
		}
		return b.Update(id, UpdateOptions{Status: &restoreStatus, Assignee: &restoreAssignee, Description: &restoreDescription})
	}
	return mutateAndDoltCommit(beadsDir, "gt: restore issue snapshot "+id, func(ctx context.Context, conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx, `UPDATE issues
			SET status = ?, assignee = ?, description = ?, updated_at = CURRENT_TIMESTAMP(6)
			WHERE id = ? AND status = ? AND COALESCE(assignee, '') = ? AND COALESCE(description, '') = ?`,
			restoreStatus, restoreAssignee, restoreDescription, id, expectedStatus, expectedAssignee, expectedDescription)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return fmt.Errorf("%w: issue snapshot", ErrAgentFieldsChanged)
		}
		return nil
	})
}
