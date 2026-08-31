package beads

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

const (
	moleculeCleanupGraphVersion        = 3
	moleculeCleanupV2GraphVersion      = 2
	moleculeCleanupV1GraphVersion      = 1
	MoleculeOwnershipDependencySQLList = "'blocks', 'conditional-blocks', 'parent-child'"
)

type FormulaMoleculeIssueSnapshot struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	IssueType   string    `json:"issue_type"`
	CreatedAt   time.Time `json:"created_at"`
	Ephemeral   bool      `json:"ephemeral"`
}

type FormulaMoleculeDependencySnapshot struct {
	IssueID     string `json:"issue_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
	CreatedBy   string `json:"created_by"`
}

type FormulaMoleculeGraphSnapshot struct {
	Issues       []FormulaMoleculeIssueSnapshot
	Dependencies []FormulaMoleculeDependencySnapshot
}

var beforeFormulaMoleculeSnapshotContinue func() error

// CaptureFormulaMoleculeGraph reads one complete formula graph from a single
// serializable Dolt snapshot so identity verification and generation hashing
// cannot observe different writer generations.
func (b *Beads) CaptureFormulaMoleculeGraph(rootID string) (*FormulaMoleculeGraphSnapshot, error) {
	return b.CaptureFormulaMoleculeGraphContext(context.Background(), rootID)
}

func (b *Beads) CaptureFormulaMoleculeGraphContext(parent context.Context, rootID string) (*FormulaMoleculeGraphSnapshot, error) {
	if strings.TrimSpace(rootID) == "" {
		return nil, fmt.Errorf("formula molecule snapshot requires a root ID")
	}
	db, err := openDoltSQL(b.getResolvedBeadsDir())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(parent, constants.BdCommandTimeout)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE"); err != nil {
		return nil, fmt.Errorf("setting formula molecule snapshot isolation: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "SET @@autocommit = 0"); err != nil {
		return nil, fmt.Errorf("starting formula molecule snapshot: %w", err)
	}
	defer rollbackConnBounded(conn)
	return captureFormulaMoleculeGraphConn(ctx, conn, rootID, false)
}

func captureFormulaMoleculeGraphConn(ctx context.Context, conn *sql.Conn, rootID string, lock bool) (*FormulaMoleculeGraphSnapshot, error) {
	if strings.TrimSpace(rootID) == "" {
		return nil, fmt.Errorf("formula molecule snapshot requires a root ID")
	}

	readIssue := func(id string) (FormulaMoleculeIssueSnapshot, error) {
		var found *FormulaMoleculeIssueSnapshot
		for _, table := range []string{"issues", "wisps"} {
			var issue FormulaMoleculeIssueSnapshot
			query := "SELECT id, title, COALESCE(description, ''), issue_type, created_at FROM " + table + " WHERE id = ?"
			if lock {
				query += " FOR UPDATE"
			}
			err := conn.QueryRowContext(ctx, query, id).
				Scan(&issue.ID, &issue.Title, &issue.Description, &issue.IssueType, &issue.CreatedAt)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return FormulaMoleculeIssueSnapshot{}, err
			}
			if found != nil {
				return FormulaMoleculeIssueSnapshot{}, fmt.Errorf("formula molecule issue %s exists in multiple stores", id)
			}
			issue.Ephemeral = table == "wisps"
			copy := issue
			found = &copy
		}
		if found == nil {
			return FormulaMoleculeIssueSnapshot{}, fmt.Errorf("formula molecule issue %s not found", id)
		}
		return *found, nil
	}

	root, err := readIssue(rootID)
	if err != nil {
		return nil, err
	}
	if beforeFormulaMoleculeSnapshotContinue != nil {
		if err := beforeFormulaMoleculeSnapshotContinue(); err != nil {
			return nil, err
		}
	}
	issues := map[string]FormulaMoleculeIssueSnapshot{rootID: root}
	queue := []string{rootID}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, table := range []string{"dependencies", "wisp_dependencies"} {
			query := "SELECT issue_id FROM " + table + " WHERE type = 'parent-child' AND COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external) = ?"
			if lock {
				query += " FOR UPDATE"
			}
			rows, err := conn.QueryContext(ctx, query, parent)
			if err != nil {
				return nil, err
			}
			var children []string
			for rows.Next() {
				var child string
				if err := rows.Scan(&child); err != nil {
					_ = rows.Close()
					return nil, err
				}
				children = append(children, child)
			}
			if err := rows.Close(); err != nil {
				return nil, err
			}
			for _, child := range children {
				if _, ok := issues[child]; ok {
					continue
				}
				issue, err := readIssue(child)
				if err != nil {
					return nil, err
				}
				issues[child] = issue
				queue = append(queue, child)
			}
		}
	}

	ids := make([]string, 0, len(issues))
	for id := range issues {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	graph := &FormulaMoleculeGraphSnapshot{Issues: make([]FormulaMoleculeIssueSnapshot, 0, len(ids))}
	for _, id := range ids {
		graph.Issues = append(graph.Issues, issues[id])
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)*2)
	for _, id := range ids {
		args = append(args, id)
	}
	for _, id := range ids {
		args = append(args, id)
	}
	seen := make(map[string]FormulaMoleculeDependencySnapshot)
	for _, table := range []string{"dependencies", "wisp_dependencies"} {
		query := "SELECT issue_id, COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external, ''), type, COALESCE(created_by, '') FROM " + table + " WHERE issue_id IN (" + placeholders + ") OR COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external, '') IN (" + placeholders + ")"
		if lock {
			query += " FOR UPDATE"
		}
		rows, err := conn.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var dependency FormulaMoleculeDependencySnapshot
			if err := rows.Scan(&dependency.IssueID, &dependency.DependsOnID, &dependency.Type, &dependency.CreatedBy); err != nil {
				_ = rows.Close()
				return nil, err
			}
			key := dependency.IssueID + "\x00" + dependency.DependsOnID + "\x00" + dependency.Type
			if prior, ok := seen[key]; ok && prior.CreatedBy != dependency.CreatedBy {
				_ = rows.Close()
				return nil, fmt.Errorf("formula molecule dependency %s has conflicting provenance", key)
			}
			seen[key] = dependency
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	for _, dependency := range seen {
		graph.Dependencies = append(graph.Dependencies, dependency)
	}
	sort.Slice(graph.Dependencies, func(i, j int) bool {
		left, right := graph.Dependencies[i], graph.Dependencies[j]
		if left.IssueID != right.IssueID {
			return left.IssueID < right.IssueID
		}
		if left.DependsOnID != right.DependsOnID {
			return left.DependsOnID < right.DependsOnID
		}
		return left.Type < right.Type
	})
	return graph, nil
}

// FormulaMoleculeGraphGeneration returns the canonical generation used by
// formula mutation receipts and transactional publication.
func FormulaMoleculeGraphGeneration(graph *FormulaMoleculeGraphSnapshot) (string, error) {
	if graph == nil {
		return "", fmt.Errorf("formula molecule generation has no graph")
	}
	issues := append([]FormulaMoleculeIssueSnapshot(nil), graph.Issues...)
	dependencies := append([]FormulaMoleculeDependencySnapshot(nil), graph.Dependencies...)
	sort.Slice(issues, func(i, j int) bool { return issues[i].ID < issues[j].ID })
	sort.Slice(dependencies, func(i, j int) bool {
		left, right := dependencies[i], dependencies[j]
		if left.IssueID != right.IssueID {
			return left.IssueID < right.IssueID
		}
		if left.DependsOnID != right.DependsOnID {
			return left.DependsOnID < right.DependsOnID
		}
		return left.Type < right.Type
	})
	encoded, err := json.Marshal(struct {
		Issues       []FormulaMoleculeIssueSnapshot      `json:"issues"`
		Dependencies []FormulaMoleculeDependencySnapshot `json:"dependencies"`
	}{issues, dependencies})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

type cleanupIssueSnapshot struct {
	receipt       MoleculeCleanupIssueReceipt
	assignee      string
	description   string
	updatedAt     time.Time
	closeReason   string
	rootShapeHash string
	isMolecule    bool
}

var beforeMoleculeCleanupMutation func(context.Context, *sql.Conn) error

// CaptureMoleculeCleanupReceipts binds every root, transitive child, and
// root-to-work bond that a later cleanup is allowed to mutate.
func (b *Beads) CaptureMoleculeCleanupReceipts(pinnedBeadID string, moleculeIDs []string) ([]MoleculeCleanupReceipt, error) {
	cleanup, _, err := b.CaptureMoleculeCleanupReceiptsPreserving(pinnedBeadID, moleculeIDs, nil)
	return cleanup, err
}

// CaptureMoleculeCleanupReceiptsPreserving binds the complete authorized root
// set while separating roots to close from roots that must remain untouched.
func (b *Beads) CaptureMoleculeCleanupReceiptsPreserving(pinnedBeadID string, moleculeIDs, preservedIDs []string) ([]MoleculeCleanupReceipt, []MoleculeCleanupReceipt, error) {
	roots := uniqueSortedStrings(moleculeIDs)
	preservedRoots := uniqueSortedStrings(preservedIDs)
	for _, root := range roots {
		if sortedContainsString(preservedRoots, root) {
			return nil, nil, fmt.Errorf("molecule root %s cannot be both cleaned and preserved", root)
		}
	}
	db, err := openDoltSQL(b.getResolvedBeadsDir())
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), constants.BdCommandTimeout)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE"); err != nil {
		return nil, nil, fmt.Errorf("setting molecule cleanup snapshot isolation: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "SET @@autocommit = 0"); err != nil {
		return nil, nil, fmt.Errorf("starting molecule cleanup snapshot: %w", err)
	}
	defer rollbackConnBounded(conn)
	authorizedRoots, err := readAuthorizedMoleculeRoots(ctx, conn, pinnedBeadID)
	if err != nil {
		return nil, nil, err
	}
	completeRoots := uniqueSortedStrings(append(append([]string(nil), roots...), preservedRoots...))
	if !equalStringSlices(completeRoots, authorizedRoots) {
		return nil, nil, fmt.Errorf("molecule cleanup root set is not the complete pinned authorization: got %v, want %v", completeRoots, authorizedRoots)
	}

	capture := func(ids []string) ([]MoleculeCleanupReceipt, error) {
		receipts := make([]MoleculeCleanupReceipt, 0, len(ids))
		for _, root := range ids {
			receipt, members, err := captureMoleculeCleanupReceipt(ctx, conn, pinnedBeadID, root)
			if err != nil {
				return nil, err
			}
			if status := members[root].receipt.Status; status == string(StatusClosed) || status == string(StatusTombstone) {
				return nil, fmt.Errorf("cleanup root %s is not open (status %q)", root, status)
			}
			receipts = append(receipts, receipt)
		}
		return receipts, nil
	}
	cleanup, err := capture(roots)
	if err != nil {
		return nil, nil, err
	}
	preserved, err := capture(preservedRoots)
	if err != nil {
		return nil, nil, err
	}
	if err := validateMoleculeCleanupOwnership(append(append([]MoleculeCleanupReceipt(nil), cleanup...), preserved...)); err != nil {
		return nil, nil, err
	}
	return cleanup, preserved, nil
}

func sortedContainsString(values []string, target string) bool {
	index := sort.SearchStrings(values, target)
	return index < len(values) && values[index] == target
}

func readAuthorizedMoleculeRoots(ctx context.Context, conn *sql.Conn, pinnedBeadID string) ([]string, error) {
	_, err := readCleanupIssueSnapshot(ctx, conn, pinnedBeadID)
	if err != nil {
		return nil, fmt.Errorf("capturing pinned cleanup authorization: %w", err)
	}
	rows, err := conn.QueryContext(ctx, `
		SELECT dependencies.issue_id
		  FROM dependencies JOIN issues ON issues.id = dependencies.issue_id
		 WHERE (issues.issue_type = 'molecule' OR EXISTS (
		           SELECT 1 FROM labels
		            WHERE labels.issue_id = issues.id AND labels.label = 'gt:molecule'
		       ))
		   AND issues.status NOT IN ('closed', 'tombstone')
		   AND dependencies.type IN (`+MoleculeOwnershipDependencySQLList+`)
		   AND COALESCE(dependencies.depends_on_issue_id, dependencies.depends_on_wisp_id, dependencies.depends_on_external) = ?
		UNION
		SELECT wisp_dependencies.issue_id
		  FROM wisp_dependencies JOIN wisps ON wisps.id = wisp_dependencies.issue_id
		 WHERE (wisps.issue_type = 'molecule' OR EXISTS (
		           SELECT 1 FROM wisp_labels
		            WHERE wisp_labels.issue_id = wisps.id AND wisp_labels.label = 'gt:molecule'
		       ))
		   AND wisps.status NOT IN ('closed', 'tombstone')
		   AND wisp_dependencies.type IN (`+MoleculeOwnershipDependencySQLList+`)
		   AND COALESCE(wisp_dependencies.depends_on_issue_id, wisp_dependencies.depends_on_wisp_id, wisp_dependencies.depends_on_external) = ?`,
		pinnedBeadID, pinnedBeadID)
	if err != nil {
		return nil, fmt.Errorf("listing pinned molecule roots: %w", err)
	}
	defer rows.Close()
	var roots []string
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return nil, err
		}
		roots = append(roots, root)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return uniqueSortedStrings(roots), nil
}

// VerifyMoleculeRootBond confirms that rootID is a molecule with an exact
// ownership edge to pinnedBeadID. It grants no cleanup authority.
func (b *Beads) VerifyMoleculeRootBond(pinnedBeadID, rootID string) error {
	if pinnedBeadID == "" || rootID == "" || pinnedBeadID == rootID {
		return fmt.Errorf("molecule ownership bond requires distinct work and root IDs")
	}
	db, err := openDoltSQL(b.getResolvedBeadsDir())
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), constants.BdCommandTimeout)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	root, err := readCleanupIssueSnapshot(ctx, conn, rootID)
	if err != nil {
		return fmt.Errorf("reading molecule root %s: %w", rootID, err)
	}
	if !root.isMolecule {
		return fmt.Errorf("bonded root %s is not a molecule", rootID)
	}
	edges, err := readCleanupOwnershipEdges(ctx, conn, []string{rootID})
	if err != nil {
		return err
	}
	var outgoing []MoleculeCleanupBondReceipt
	for _, edge := range edges {
		if edge.From == rootID {
			outgoing = append(outgoing, edge)
		}
	}
	if len(outgoing) != 1 || outgoing[0].To != pinnedBeadID {
		return fmt.Errorf("molecule root %s requires one exact ownership bond to %s", rootID, pinnedBeadID)
	}
	return nil
}

// MoleculeRootGeneration returns a stable identity for one exact molecule row.
// Mutable status, assignee, and description fields are intentionally excluded.
func (b *Beads) MoleculeRootGeneration(rootID string) (string, error) {
	if strings.TrimSpace(rootID) == "" {
		return "", fmt.Errorf("molecule root generation requires an ID")
	}
	db, err := openDoltSQL(b.getResolvedBeadsDir())
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
	var generation string
	for _, tables := range []struct{ issues, labels string }{{"issues", "labels"}, {"wisps", "wisp_labels"}} {
		var createdAt time.Time
		var issueType string
		var moleculeLabel bool
		err := conn.QueryRowContext(ctx, "SELECT created_at, issue_type, EXISTS (SELECT 1 FROM "+tables.labels+" WHERE "+tables.labels+".issue_id = "+tables.issues+".id AND label = 'gt:molecule') FROM "+tables.issues+" WHERE id = ?", rootID).
			Scan(&createdAt, &issueType, &moleculeLabel)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return "", err
		}
		if generation != "" {
			return "", fmt.Errorf("molecule root %s exists in both issues and wisps", rootID)
		}
		if issueType != "molecule" && !moleculeLabel {
			return "", fmt.Errorf("root %s is not a molecule", rootID)
		}
		generation = cleanupHash(tables.issues, rootID, createdAt.UTC().Format(time.RFC3339Nano), issueType, fmt.Sprintf("%t", moleculeLabel))
	}
	if generation == "" {
		return "", sql.ErrNoRows
	}
	return generation, nil
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ApplyMoleculeCleanupReceipts closes only the graph authorized by the exact
// committed detach attempt and removes only its recorded bonds.
func (b *Beads) ApplyMoleculeCleanupReceipts(attemptID, closeReason string) (int, error) {
	if strings.TrimSpace(attemptID) == "" {
		return 0, fmt.Errorf("molecule cleanup requires an attempt ID")
	}
	entry, err := b.moleculeCleanupAttempt(attemptID)
	if err != nil {
		return 0, err
	}
	pinnedBeadID := entry.PinnedBeadID
	receipts := entry.CleanupMolecules
	preserved := entry.PreservedMolecules
	for _, receipt := range receipts {
		if !supportedMoleculeCleanupGraphVersion(receipt.GraphVersion) || receipt.ID == "" || len(receipt.Members) == 0 {
			return 0, fmt.Errorf("molecule %s cleanup receipt does not bind a supported graph", receipt.ID)
		}
	}
	for _, receipt := range preserved {
		if !supportedMoleculeCleanupGraphVersion(receipt.GraphVersion) || receipt.ID == "" || len(receipt.Members) == 0 {
			return 0, fmt.Errorf("preserved molecule %s receipt does not bind a supported graph", receipt.ID)
		}
	}
	unlock, err := b.lockBead(pinnedBeadID)
	if err != nil {
		return 0, fmt.Errorf("locking molecule cleanup custody: %w", err)
	}
	defer unlock()

	childrenClosed := 0
	_, err = mutateAndDoltCommitWithOID(b.getResolvedBeadsDir(), "gt: cleanup detached molecule attempt "+attemptID, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE"); err != nil {
			return fmt.Errorf("setting molecule cleanup isolation: %w", err)
		}
		current, snapshots, err := captureMoleculeCleanupReceiptsFrom(ctx, conn, pinnedBeadID, receipts)
		if err != nil {
			return err
		}
		receipts, err = reconcileLegacyMoleculeCleanupReceipts(receipts, current, snapshots, closeReason, true)
		if err != nil {
			return err
		}
		if hasLegacyMoleculeCleanupReceipts(preserved) {
			currentPreserved, preservedSnapshots, err := captureMoleculeCleanupReceiptsFrom(ctx, conn, pinnedBeadID, preserved)
			if err != nil {
				return err
			}
			preserved, err = reconcileLegacyMoleculeCleanupReceipts(preserved, currentPreserved, preservedSnapshots, closeReason, false)
			if err != nil {
				return err
			}
		}
		if moleculeCleanupPostState(receipts, current, snapshots, closeReason) {
			return verifyCompletedMoleculeCleanupCustody(ctx, conn, entry, preserved)
		}
		if err := verifyAuthorizedCleanupRoots(ctx, conn, entry); err != nil {
			return err
		}
		if err := compareMoleculeCleanupReceipts(receipts, current); err != nil {
			return err
		}
		if err := verifyPreservedMoleculeReceipts(ctx, conn, pinnedBeadID, preserved); err != nil {
			return err
		}
		if err := verifyPinnedCleanupSnapshot(ctx, conn, entry); err != nil {
			return err
		}
		if beforeMoleculeCleanupMutation != nil {
			if err := beforeMoleculeCleanupMutation(ctx, conn); err != nil {
				return err
			}
			current, snapshots, err = captureMoleculeCleanupReceiptsFrom(ctx, conn, pinnedBeadID, receipts)
			if err != nil {
				return err
			}
			if err := verifyAuthorizedCleanupRoots(ctx, conn, entry); err != nil {
				return err
			}
			if err := compareMoleculeCleanupReceipts(receipts, current); err != nil {
				return err
			}
			if err := verifyPreservedMoleculeReceipts(ctx, conn, pinnedBeadID, preserved); err != nil {
				return err
			}
			if err := verifyPinnedCleanupSnapshot(ctx, conn, entry); err != nil {
				return err
			}
		}

		rootIDs := make(map[string]bool, len(receipts))
		for _, receipt := range receipts {
			rootIDs[receipt.ID] = true
		}
		memberIDs := make([]string, 0, len(snapshots))
		for id := range snapshots {
			memberIDs = append(memberIDs, id)
		}
		sort.Slice(memberIDs, func(i, j int) bool {
			if rootIDs[memberIDs[i]] != rootIDs[memberIDs[j]] {
				return !rootIDs[memberIDs[i]]
			}
			return memberIDs[i] < memberIDs[j]
		})
		for _, id := range memberIDs {
			snapshot := snapshots[id]
			if snapshot.receipt.Status == string(StatusClosed) || snapshot.receipt.Status == string(StatusTombstone) {
				continue
			}
			reason := "burned: force-close descendants"
			if rootIDs[id] {
				reason = closeReason
			} else {
				childrenClosed++
			}
			if err := closeCleanupIssueCAS(ctx, conn, snapshot, reason); err != nil {
				return err
			}
		}
		for _, receipt := range receipts {
			for _, bond := range receipt.Bonds {
				result, err := conn.ExecContext(ctx, "DELETE FROM "+bond.Storage+" WHERE id = ?", bond.ID)
				if err != nil {
					return fmt.Errorf("removing recorded molecule bond %s: %w", bond.ID, err)
				}
				rows, err := result.RowsAffected()
				if err != nil || rows != 1 {
					return fmt.Errorf("molecule bond %s changed before cleanup", bond.ID)
				}
			}
		}
		post, postSnapshots, err := captureMoleculeCleanupReceiptsFrom(ctx, conn, pinnedBeadID, receipts)
		if err != nil {
			return err
		}
		if !moleculeCleanupPostState(receipts, post, postSnapshots, closeReason) {
			return fmt.Errorf("molecule cleanup post-state did not match the recorded graph")
		}
		if err := verifyCompletedMoleculeCleanupCustody(ctx, conn, entry, preserved); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return childrenClosed, nil
}

func supportedMoleculeCleanupGraphVersion(version int) bool {
	return version == moleculeCleanupGraphVersion || version == moleculeCleanupV2GraphVersion || version == moleculeCleanupV1GraphVersion
}

func hasLegacyMoleculeCleanupReceipts(receipts []MoleculeCleanupReceipt) bool {
	for _, receipt := range receipts {
		if receipt.GraphVersion != moleculeCleanupGraphVersion {
			return true
		}
	}
	return false
}

func reconcileLegacyMoleculeCleanupReceipts(expected, current []MoleculeCleanupReceipt, snapshots map[string]cleanupIssueSnapshot, closeReason string, allowPostState bool) ([]MoleculeCleanupReceipt, error) {
	if len(expected) != len(current) {
		return nil, fmt.Errorf("molecule cleanup root set changed")
	}
	normalized := append([]MoleculeCleanupReceipt(nil), expected...)
	for i := range expected {
		if expected[i].GraphVersion == moleculeCleanupGraphVersion {
			continue
		}
		upgraded, err := reconcileLegacyMoleculeCleanupReceipt(expected[i], current[i], snapshots, closeReason, allowPostState)
		if err != nil {
			return nil, fmt.Errorf("reconciling version %d molecule cleanup receipt %s: %w", expected[i].GraphVersion, expected[i].ID, err)
		}
		normalized[i] = upgraded
	}
	if err := validateMoleculeCleanupOwnership(normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}

func reconcileLegacyMoleculeCleanupReceipt(expected, current MoleculeCleanupReceipt, snapshots map[string]cleanupIssueSnapshot, closeReason string, allowPostState bool) (MoleculeCleanupReceipt, error) {
	if expected.ID != current.ID || current.GraphVersion != moleculeCleanupGraphVersion || current.RootShapeHash == "" || len(expected.Members) != len(current.Members) {
		return MoleculeCleanupReceipt{}, fmt.Errorf("legacy cleanup receipt does not match a supported current graph")
	}
	var ownershipMatches bool
	switch expected.GraphVersion {
	case moleculeCleanupV2GraphVersion:
		ownershipMatches = equalCleanupBonds(expected.Ownership, legacyMoleculeCleanupOwnership(current.Ownership, expected.Members))
	case moleculeCleanupV1GraphVersion:
		if expected.RootShapeHash != "" || len(expected.Ownership) != 0 {
			return MoleculeCleanupReceipt{}, fmt.Errorf("version 1 cleanup receipt contains unsupported graph fields")
		}
		ownershipMatches = true
	default:
		return MoleculeCleanupReceipt{}, fmt.Errorf("unsupported legacy cleanup graph version %d", expected.GraphVersion)
	}
	if legacyMoleculeCleanupMembersMatch(expected.Members, current.Members, snapshots, false) && equalCleanupBonds(expected.Bonds, current.Bonds) && ownershipMatches {
		return current, nil
	}
	if !allowPostState || len(current.Bonds) != 0 || !legacyMoleculeCleanupMembersMatch(expected.Members, current.Members, snapshots, true) {
		return MoleculeCleanupReceipt{}, fmt.Errorf("legacy cleanup graph changed before replay")
	}

	upgraded := expected
	upgraded.GraphVersion = moleculeCleanupGraphVersion
	upgraded.RootShapeHash = current.RootShapeHash
	upgraded.Members = append([]MoleculeCleanupIssueReceipt(nil), expected.Members...)
	for i := range upgraded.Members {
		if upgraded.Members[i].Status != string(StatusClosed) && upgraded.Members[i].Status != string(StatusTombstone) {
			continue
		}
		upgraded.Members[i].SnapshotHash = snapshots[upgraded.Members[i].ID].receipt.SnapshotHash
	}
	upgraded.Ownership = append(append([]MoleculeCleanupBondReceipt(nil), current.Ownership...), expected.Bonds...)
	sort.Slice(upgraded.Ownership, func(i, j int) bool {
		return cleanupBondKey(upgraded.Ownership[i]) < cleanupBondKey(upgraded.Ownership[j])
	})
	if (expected.GraphVersion == moleculeCleanupV2GraphVersion && !equalCleanupBonds(expected.Ownership, legacyMoleculeCleanupOwnership(upgraded.Ownership, expected.Members))) ||
		!moleculeCleanupPostState([]MoleculeCleanupReceipt{upgraded}, []MoleculeCleanupReceipt{current}, snapshots, closeReason) {
		return MoleculeCleanupReceipt{}, fmt.Errorf("legacy cleanup post-state does not match the recorded graph")
	}
	return upgraded, nil
}

func legacyMoleculeCleanupMembersMatch(expected, current []MoleculeCleanupIssueReceipt, snapshots map[string]cleanupIssueSnapshot, allowClosedPostState bool) bool {
	if len(expected) != len(current) {
		return false
	}
	for i := range expected {
		want, got := expected[i], current[i]
		snapshot, ok := snapshots[want.ID]
		if !ok || want.ID != got.ID || want.Storage != got.Storage || want.ContentHash != got.ContentHash {
			return false
		}
		if allowClosedPostState && want.Status != string(StatusClosed) && want.Status != string(StatusTombstone) {
			continue
		}
		legacyHash := cleanupHash(got.Status, got.ContentHash, snapshot.updatedAt.UTC().Format(time.RFC3339Nano), snapshot.closeReason)
		if want.Status != got.Status || want.SnapshotHash != legacyHash {
			return false
		}
	}
	return true
}

func legacyMoleculeCleanupOwnership(ownership []MoleculeCleanupBondReceipt, members []MoleculeCleanupIssueReceipt) []MoleculeCleanupBondReceipt {
	memberIDs := make(map[string]bool, len(members))
	for _, member := range members {
		memberIDs[member.ID] = true
	}
	var legacy []MoleculeCleanupBondReceipt
	for _, edge := range ownership {
		if memberIDs[edge.To] {
			legacy = append(legacy, edge)
		}
	}
	return legacy
}

func equalCleanupBonds(left, right []MoleculeCleanupBondReceipt) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func verifyCompletedMoleculeCleanupCustody(ctx context.Context, conn *sql.Conn, entry *DetachAuditEntry, preserved []MoleculeCleanupReceipt) error {
	if err := verifyAuthorizedMoleculeRoots(ctx, conn, entry.PinnedBeadID, preserved); err != nil {
		return err
	}
	if err := verifyPreservedMoleculeReceipts(ctx, conn, entry.PinnedBeadID, preserved); err != nil {
		return err
	}
	return verifyPinnedCleanupSnapshot(ctx, conn, entry)
}

func verifyPreservedMoleculeReceipts(ctx context.Context, conn *sql.Conn, pinnedBeadID string, expected []MoleculeCleanupReceipt) error {
	if len(expected) == 0 {
		return nil
	}
	current, _, err := captureMoleculeCleanupReceiptsFrom(ctx, conn, pinnedBeadID, expected)
	if err != nil {
		return err
	}
	return compareMoleculeCleanupReceipts(expected, current)
}

func verifyPinnedCleanupSnapshot(ctx context.Context, conn *sql.Conn, entry *DetachAuditEntry) error {
	current, err := readCleanupIssueSnapshot(ctx, conn, entry.PinnedBeadID)
	if err != nil {
		return fmt.Errorf("revalidating pinned cleanup custody: %w", err)
	}
	if current.receipt.Status != entry.ResultingStatus || current.assignee != entry.ResultingAssignee || current.description != entry.ResultingDescription || !cleanupGenerationMatches(entry.ResultingGeneration, current.updatedAt) {
		return fmt.Errorf("pinned bead %s changed after its recorded detach", entry.PinnedBeadID)
	}
	return nil
}

func captureMoleculeCleanupReceiptsFrom(ctx context.Context, conn *sql.Conn, pinnedBeadID string, expected []MoleculeCleanupReceipt) ([]MoleculeCleanupReceipt, map[string]cleanupIssueSnapshot, error) {
	current := make([]MoleculeCleanupReceipt, 0, len(expected))
	snapshots := make(map[string]cleanupIssueSnapshot)
	for _, receipt := range expected {
		got, members, err := captureMoleculeCleanupReceipt(ctx, conn, pinnedBeadID, receipt.ID)
		if err != nil {
			return nil, nil, err
		}
		current = append(current, got)
		for id, member := range members {
			if prior, ok := snapshots[id]; ok && prior.receipt.SnapshotHash != member.receipt.SnapshotHash {
				return nil, nil, fmt.Errorf("molecule member %s has conflicting generations", id)
			}
			snapshots[id] = member
		}
	}
	if err := validateMoleculeCleanupOwnership(current); err != nil {
		return nil, nil, err
	}
	return current, snapshots, nil
}

func verifyAuthorizedCleanupRoots(ctx context.Context, conn *sql.Conn, entry *DetachAuditEntry) error {
	receipts := append(append([]MoleculeCleanupReceipt(nil), entry.CleanupMolecules...), entry.PreservedMolecules...)
	return verifyAuthorizedMoleculeRoots(ctx, conn, entry.PinnedBeadID, receipts)
}

func verifyAuthorizedMoleculeRoots(ctx context.Context, conn *sql.Conn, pinnedBeadID string, receipts []MoleculeCleanupReceipt) error {
	var want []string
	for _, receipt := range receipts {
		for _, bond := range receipt.Bonds {
			if bond.From == receipt.ID && bond.To == pinnedBeadID {
				want = append(want, receipt.ID)
				break
			}
		}
	}
	want = uniqueSortedStrings(want)
	got, err := readAuthorizedMoleculeRoots(ctx, conn, pinnedBeadID)
	if err != nil {
		return err
	}
	if !equalStringSlices(want, got) {
		return fmt.Errorf("molecule cleanup root set changed: got %v, want %v", got, want)
	}
	return nil
}

func validateMoleculeCleanupOwnership(receipts []MoleculeCleanupReceipt) error {
	members := make(map[string]bool)
	authorizedBonds := make(map[string]bool)
	for _, receipt := range receipts {
		for _, member := range receipt.Members {
			members[member.ID] = true
		}
		for _, bond := range receipt.Bonds {
			authorizedBonds[bond.ID] = true
		}
	}
	for _, receipt := range receipts {
		for _, edge := range receipt.Ownership {
			if authorizedBonds[edge.ID] {
				continue
			}
			if !members[edge.From] || !members[edge.To] {
				return fmt.Errorf("molecule ownership edge %s crosses the cleanup attempt: %s -> %s", edge.ID, edge.From, edge.To)
			}
		}
	}
	return nil
}

func captureMoleculeCleanupReceipt(ctx context.Context, conn *sql.Conn, pinnedBeadID, rootID string) (MoleculeCleanupReceipt, map[string]cleanupIssueSnapshot, error) {
	receipt := MoleculeCleanupReceipt{ID: rootID, GraphVersion: moleculeCleanupGraphVersion}
	members := make(map[string]cleanupIssueSnapshot)
	queue := []string{rootID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if _, seen := members[id]; seen {
			continue
		}
		snapshot, err := readCleanupIssueSnapshot(ctx, conn, id)
		if err != nil {
			return receipt, nil, fmt.Errorf("capturing molecule member %s: %w", id, err)
		}
		members[id] = snapshot
		children, err := readCleanupChildren(ctx, conn, id)
		if err != nil {
			return receipt, nil, err
		}
		queue = append(queue, children...)
	}
	for _, member := range members {
		receipt.Members = append(receipt.Members, member.receipt)
	}
	sort.Slice(receipt.Members, func(i, j int) bool { return receipt.Members[i].ID < receipt.Members[j].ID })
	root := members[rootID]
	if !root.isMolecule {
		return receipt, nil, fmt.Errorf("cleanup root %s is not molecule-shaped", rootID)
	}
	receipt.RootShapeHash = root.rootShapeHash
	bonds, err := readCleanupBonds(ctx, conn, pinnedBeadID, rootID)
	if err != nil {
		return receipt, nil, err
	}
	receipt.Bonds = bonds
	memberIDs := make([]string, 0, len(members))
	for id := range members {
		memberIDs = append(memberIDs, id)
	}
	receipt.Ownership, err = readCleanupOwnershipEdges(ctx, conn, memberIDs)
	if err != nil {
		return receipt, nil, err
	}
	return receipt, members, nil
}

func readCleanupIssueSnapshot(ctx context.Context, conn *sql.Conn, id string) (cleanupIssueSnapshot, error) {
	var found *cleanupIssueSnapshot
	for _, tables := range []struct{ issues, labels string }{{"issues", "labels"}, {"wisps", "wisp_labels"}} {
		var status, assignee, description, closeReason, issueType string
		var moleculeLabel bool
		var updatedAt time.Time
		query := "SELECT status, COALESCE(assignee, ''), description, updated_at, COALESCE(close_reason, ''), issue_type, " +
			"EXISTS (SELECT 1 FROM " + tables.labels + " WHERE " + tables.labels + ".issue_id = " + tables.issues + ".id AND label = 'gt:molecule') " +
			"FROM " + tables.issues + " WHERE id = ?"
		err := conn.QueryRowContext(ctx, query, id).
			Scan(&status, &assignee, &description, &updatedAt, &closeReason, &issueType, &moleculeLabel)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return cleanupIssueSnapshot{}, err
		}
		if found != nil {
			return cleanupIssueSnapshot{}, fmt.Errorf("issue exists in both issues and wisps")
		}
		contentHash := cleanupHash(assignee, description)
		rootShapeHash := cleanupHash(issueType, fmt.Sprintf("%t", moleculeLabel))
		snapshot := cleanupIssueSnapshot{
			receipt: MoleculeCleanupIssueReceipt{
				ID: id, Storage: tables.issues, Status: status, ContentHash: contentHash,
				SnapshotHash: cleanupHash(status, contentHash, rootShapeHash, updatedAt.UTC().Format(time.RFC3339Nano), closeReason),
			},
			assignee: assignee, description: description, updatedAt: updatedAt, closeReason: closeReason,
			rootShapeHash: rootShapeHash, isMolecule: issueType == "molecule" || moleculeLabel,
		}
		found = &snapshot
	}
	if found == nil {
		return cleanupIssueSnapshot{}, sql.ErrNoRows
	}
	return *found, nil
}

func readCleanupChildren(ctx context.Context, conn *sql.Conn, parentID string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT issue_id FROM dependencies
		 WHERE type = 'parent-child' AND COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external) = ?
		UNION SELECT issue_id FROM wisp_dependencies
		 WHERE type = 'parent-child' AND COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external) = ?
		UNION SELECT id FROM issues WHERE LEFT(id, CHAR_LENGTH(?) + 1) = CONCAT(?, '.')
		UNION SELECT id FROM wisps WHERE LEFT(id, CHAR_LENGTH(?) + 1) = CONCAT(?, '.')`,
		parentID, parentID, parentID, parentID, parentID, parentID)
	if err != nil {
		return nil, fmt.Errorf("listing recorded children of %s: %w", parentID, err)
	}
	defer rows.Close()
	var children []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id != parentID {
			children = append(children, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return uniqueSortedStrings(children), nil
}

func readCleanupBonds(ctx context.Context, conn *sql.Conn, pinnedBeadID, rootID string) ([]MoleculeCleanupBondReceipt, error) {
	query := `
		SELECT 'dependencies', id, issue_id,
		       COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external), type,
		       created_at, COALESCE(created_by, ''), CAST(COALESCE(metadata, JSON_OBJECT()) AS CHAR), COALESCE(thread_id, '')
		  FROM dependencies
		 WHERE issue_id = ? AND COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external) = ?
		   AND type IN (` + MoleculeOwnershipDependencySQLList + `)
		UNION ALL
		SELECT 'wisp_dependencies', id, issue_id,
		       COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external), type,
		       created_at, COALESCE(created_by, ''), CAST(COALESCE(metadata, JSON_OBJECT()) AS CHAR), COALESCE(thread_id, '')
		  FROM wisp_dependencies
		 WHERE issue_id = ? AND COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external) = ?
		   AND type IN (` + MoleculeOwnershipDependencySQLList + `)`
	return queryCleanupBonds(ctx, conn, query,
		rootID, pinnedBeadID,
		rootID, pinnedBeadID)

}

func readCleanupOwnershipEdges(ctx context.Context, conn *sql.Conn, memberIDs []string) ([]MoleculeCleanupBondReceipt, error) {
	memberIDs = uniqueSortedStrings(memberIDs)
	if len(memberIDs) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(memberIDs)), ",")
	query := fmt.Sprintf(`
		SELECT 'dependencies', id, issue_id,
		       COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external), type,
		       created_at, COALESCE(created_by, ''), CAST(COALESCE(metadata, JSON_OBJECT()) AS CHAR), COALESCE(thread_id, '')
		  FROM dependencies
		 WHERE type IN (`+MoleculeOwnershipDependencySQLList+`)
		   AND (issue_id IN (%s)
		    OR COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external) IN (%s))
		UNION ALL
		SELECT 'wisp_dependencies', id, issue_id,
		       COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external), type,
		       created_at, COALESCE(created_by, ''), CAST(COALESCE(metadata, JSON_OBJECT()) AS CHAR), COALESCE(thread_id, '')
		  FROM wisp_dependencies
		 WHERE type IN (`+MoleculeOwnershipDependencySQLList+`)
		   AND (issue_id IN (%s)
		    OR COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external) IN (%s))`, placeholders, placeholders, placeholders, placeholders)
	args := make([]any, 0, len(memberIDs)*4)
	for range 4 {
		for _, id := range memberIDs {
			args = append(args, id)
		}
	}
	return queryCleanupBonds(ctx, conn, query, args...)
}

func queryCleanupBonds(ctx context.Context, conn *sql.Conn, query string, args ...any) ([]MoleculeCleanupBondReceipt, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("capturing molecule ownership bonds: %w", err)
	}
	defer rows.Close()
	var bonds []MoleculeCleanupBondReceipt
	for rows.Next() {
		var storage, id, from, to, dependencyType, createdBy, metadata, threadID string
		var createdAt time.Time
		if err := rows.Scan(&storage, &id, &from, &to, &dependencyType, &createdAt, &createdBy, &metadata, &threadID); err != nil {
			return nil, err
		}
		bonds = append(bonds, MoleculeCleanupBondReceipt{
			ID: id, Storage: storage, From: from, To: to, Type: dependencyType,
			SnapshotHash: cleanupHash(id, storage, from, to, dependencyType, createdAt.UTC().Format(time.RFC3339Nano), createdBy, metadata, threadID),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(bonds, func(i, j int) bool {
		return cleanupBondKey(bonds[i]) < cleanupBondKey(bonds[j])
	})
	return bonds, nil
}

func compareMoleculeCleanupReceipts(expected, current []MoleculeCleanupReceipt) error {
	if len(expected) != len(current) {
		return fmt.Errorf("molecule cleanup root set changed")
	}
	for i := range expected {
		want, got := expected[i], current[i]
		if want.ID != got.ID || want.GraphVersion != got.GraphVersion || want.RootShapeHash != got.RootShapeHash || len(want.Members) != len(got.Members) || len(want.Bonds) != len(got.Bonds) {
			return fmt.Errorf("molecule %s cleanup graph changed", want.ID)
		}
		for j := range want.Members {
			if want.Members[j] != got.Members[j] {
				return fmt.Errorf("molecule member %s generation changed before cleanup", want.Members[j].ID)
			}
		}
		for j := range want.Bonds {
			if want.Bonds[j] != got.Bonds[j] {
				return fmt.Errorf("molecule %s bond generation changed before cleanup", want.ID)
			}
		}
		if len(want.Ownership) != len(got.Ownership) {
			return fmt.Errorf("molecule %s ownership graph changed before cleanup", want.ID)
		}
		for j := range want.Ownership {
			if want.Ownership[j] != got.Ownership[j] {
				return fmt.Errorf("molecule %s ownership graph changed before cleanup", want.ID)
			}
		}
	}
	return nil
}

func moleculeCleanupPostState(expected, current []MoleculeCleanupReceipt, snapshots map[string]cleanupIssueSnapshot, closeReason string) bool {
	if len(expected) != len(current) {
		return false
	}
	roots := make(map[string]bool, len(expected))
	for i := range expected {
		roots[expected[i].ID] = true
		if expected[i].ID != current[i].ID || expected[i].RootShapeHash != current[i].RootShapeHash || len(expected[i].Members) != len(current[i].Members) || len(current[i].Bonds) != 0 {
			return false
		}
		removed := make(map[string]bool, len(expected[i].Bonds))
		for _, bond := range expected[i].Bonds {
			removed[bond.ID] = true
		}
		var surviving []MoleculeCleanupBondReceipt
		for _, edge := range expected[i].Ownership {
			if !removed[edge.ID] {
				surviving = append(surviving, edge)
			}
		}
		if len(surviving) != len(current[i].Ownership) {
			return false
		}
		for j := range surviving {
			if surviving[j] != current[i].Ownership[j] {
				return false
			}
		}
	}
	for _, receipt := range expected {
		for _, member := range receipt.Members {
			got, ok := snapshots[member.ID]
			if !ok {
				return false
			}
			if member.Status == string(StatusClosed) || member.Status == string(StatusTombstone) {
				if got.receipt.SnapshotHash != member.SnapshotHash {
					return false
				}
				continue
			}
			reason := "burned: force-close descendants"
			if roots[member.ID] {
				reason = closeReason
			}
			if got.receipt.Status != string(StatusClosed) || got.receipt.ContentHash != member.ContentHash || got.closeReason != reason {
				return false
			}
		}
	}
	return true
}

func closeCleanupIssueCAS(ctx context.Context, conn *sql.Conn, snapshot cleanupIssueSnapshot, reason string) error {
	storage := snapshot.receipt.Storage
	if storage != "issues" && storage != "wisps" {
		return fmt.Errorf("unsupported molecule member storage %q", storage)
	}
	result, err := conn.ExecContext(ctx, "UPDATE "+storage+` SET status = 'closed', close_reason = ?, closed_at = CURRENT_TIMESTAMP(6), updated_at = CURRENT_TIMESTAMP(6)
		WHERE id = ? AND status = ? AND COALESCE(assignee, '') = ? AND description = ? AND updated_at = ? AND COALESCE(close_reason, '') = ?`,
		reason, snapshot.receipt.ID, snapshot.receipt.Status, snapshot.assignee, snapshot.description, snapshot.updatedAt, snapshot.closeReason)
	if err != nil {
		return fmt.Errorf("closing recorded molecule member %s: %w", snapshot.receipt.ID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return fmt.Errorf("molecule member %s changed before close", snapshot.receipt.ID)
	}
	return nil
}

func cleanupHash(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func cleanupGeneration(updatedAt time.Time) string {
	return updatedAt.UTC().Format(time.RFC3339Nano)
}

func cleanupGenerationMatches(want string, got time.Time) bool {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(want))
	return err == nil && parsed.Equal(got)
}

func cleanupGenerationStringMatches(want, got string) bool {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(got))
	return err == nil && cleanupGenerationMatches(want, parsed)
}

func cleanupBondKey(bond MoleculeCleanupBondReceipt) string {
	return strings.Join([]string{bond.Storage, bond.ID, bond.From, bond.To, bond.Type, bond.SnapshotHash}, "\x00")
}

func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
