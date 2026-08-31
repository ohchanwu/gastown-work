package beads

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const slingReceiptTable = "gt_internal_sling_receipts"

var beforeFormulaMoleculePublicationCAS func() error

// LifecycleReassignmentBinding is the immutable custody proof created while
// the old assignee still owns the work. The capability is stored as a hash so
// the database receipt cannot disclose the host-side action capability.
type LifecycleReassignmentBinding struct {
	Version        int    `json:"version"`
	AttemptID      string `json:"attempt_id"`
	BeadID         string `json:"bead_id"`
	OldAssignee    string `json:"old_assignee"`
	OldIncarnation string `json:"old_incarnation"`
	NewAssignee    string `json:"new_assignee"`
	NewIncarnation string `json:"new_incarnation"`
	Requester      string `json:"requester"`
	ThreadID       string `json:"thread_id"`
	CapabilityHash string `json:"capability_hash"`
	AuthorityMAC   string `json:"authority_mac"`
}

// FormulaMoleculePublication binds one verified graph generation to the exact
// hook and metadata snapshot published for its work bead.
type FormulaMoleculePublication struct {
	PublicationID       string                       `json:"publication_id"`
	RootID              string                       `json:"root_id"`
	RootGeneration      string                       `json:"root_generation"`
	WorkID              string                       `json:"work_id"`
	ExpectedStatus      string                       `json:"expected_status"`
	ExpectedAssignee    string                       `json:"expected_assignee"`
	ExpectedDescription string                       `json:"expected_description"`
	NewStatus           string                       `json:"new_status"`
	NewAssignee         string                       `json:"new_assignee"`
	NewDescription      string                       `json:"new_description"`
	Authorization       FormulaMoleculeAuthorization `json:"authorization"`
	AuthorizationCommit string                       `json:"authorization_commit"`
}

// FormulaMoleculeAuthorization is the verified request and graph identity
// committed before its hook publication can consume it.
type FormulaMoleculeAuthorization struct {
	PublicationID      string `json:"publication_id"`
	RootID             string `json:"root_id"`
	RootGeneration     string `json:"root_generation"`
	Formula            string `json:"formula"`
	Owner              string `json:"owner"`
	RequestFingerprint string `json:"request_fingerprint"`
	Actor              string `json:"actor"`
}

func (b *Beads) ensureSlingReceiptTable() error {
	return mutateAndDoltCommit(b.getResolvedBeadsDir(), "gt: ensure internal sling receipts", func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+slingReceiptTable+` (
			receipt_key VARCHAR(191) PRIMARY KEY,
			kind VARCHAR(32) NOT NULL,
			target_id VARCHAR(191) NOT NULL,
			payload LONGTEXT NOT NULL,
			state VARCHAR(32) NOT NULL,
			delivery_id VARCHAR(64) NOT NULL DEFAULT '',
			generation VARCHAR(128) NOT NULL DEFAULT '',
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
		)`)
		return err
	})
}

func validateLifecycleReassignmentBinding(binding LifecycleReassignmentBinding) error {
	if binding.Version != 2 {
		return fmt.Errorf("lifecycle reassignment binding has unsupported version %d", binding.Version)
	}
	for _, value := range []string{
		binding.AttemptID, binding.BeadID, binding.OldAssignee, binding.OldIncarnation,
		binding.NewAssignee, binding.Requester, binding.ThreadID, binding.CapabilityHash, binding.AuthorityMAC,
	} {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("lifecycle reassignment binding is incomplete")
		}
	}
	if strings.Contains(binding.NewAssignee, "/polecats/") && strings.TrimSpace(binding.NewIncarnation) == "" {
		return fmt.Errorf("lifecycle reassignment binding lacks replacement incarnation")
	}
	return nil
}

func slingReceiptKey(kind, id string) string { return kind + ":" + id }

type slingReceiptRow struct {
	payload, state, deliveryID, generation string
}

type slingReceiptQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readSlingReceipt(ctx context.Context, conn slingReceiptQueryer, key string, lock bool) (*slingReceiptRow, error) {
	query := `SELECT payload, state, delivery_id, generation FROM ` + slingReceiptTable + ` WHERE receipt_key = ?`
	if lock {
		query += " FOR UPDATE"
	}
	var row slingReceiptRow
	err := conn.QueryRowContext(ctx, query, key).Scan(&row.payload, &row.state, &row.deliveryID, &row.generation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func readIssueSlingSnapshot(ctx context.Context, conn slingReceiptQueryer, id string) (table, status, assignee, description string, err error) {
	found := false
	for _, candidate := range []string{"issues", "wisps"} {
		var gotStatus, gotAssignee, gotDescription string
		scanErr := conn.QueryRowContext(ctx, `SELECT status, COALESCE(assignee, ''), COALESCE(description, '') FROM `+candidate+` WHERE id = ?`, id).
			Scan(&gotStatus, &gotAssignee, &gotDescription)
		if errors.Is(scanErr, sql.ErrNoRows) {
			continue
		}
		if scanErr != nil {
			return "", "", "", "", scanErr
		}
		if found {
			return "", "", "", "", fmt.Errorf("work %s exists in multiple stores", id)
		}
		found = true
		table, status, assignee, description = candidate, gotStatus, gotAssignee, gotDescription
	}
	if !found {
		return "", "", "", "", fmt.Errorf("work %s not found", id)
	}
	return table, status, assignee, description, nil
}

// PrepareLifecycleReassignment records the immutable old-owner proof in the
// same serializable transaction that observes the old assignee.
func (b *Beads) PrepareLifecycleReassignment(binding LifecycleReassignmentBinding) error {
	if err := validateLifecycleReassignmentBinding(binding); err != nil {
		return err
	}
	payload, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	if err := b.ensureSlingReceiptTable(); err != nil {
		return err
	}
	key := slingReceiptKey("retirement", binding.AttemptID)
	return mutateAndDoltCommit(b.getResolvedBeadsDir(), "gt: prepare lifecycle reassignment "+binding.AttemptID, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE"); err != nil {
			return err
		}
		row, err := readSlingReceipt(ctx, conn, key, true)
		if err != nil {
			return err
		}
		if row != nil {
			if row.payload != string(payload) {
				return fmt.Errorf("lifecycle reassignment attempt already binds different custody")
			}
			return nil
		}
		_, _, assignee, _, err := readIssueSlingSnapshot(ctx, conn, binding.BeadID)
		if err != nil {
			return err
		}
		if assignee != binding.OldAssignee {
			return fmt.Errorf("work %s is assigned to %s, not prior owner %s", binding.BeadID, assignee, binding.OldAssignee)
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO `+slingReceiptTable+`
			(receipt_key, kind, target_id, payload, state) VALUES (?, 'retirement', ?, ?, 'prepared')`,
			key, binding.BeadID, string(payload))
		return err
	})
}

// BindLifecycleReassignmentDelivery advances only an exact prepared receipt
// after the replacement owns the work, binding the immutable proof to mail.
func (b *Beads) BindLifecycleReassignmentDelivery(binding LifecycleReassignmentBinding, deliveryID, intentHash string) error {
	if err := validateLifecycleReassignmentBinding(binding); err != nil {
		return err
	}
	if deliveryID == "" || intentHash == "" {
		return fmt.Errorf("lifecycle reassignment delivery is incomplete")
	}
	payload, _ := json.Marshal(binding)
	key := slingReceiptKey("retirement", binding.AttemptID)
	return mutateAndDoltCommit(b.getResolvedBeadsDir(), "gt: bind lifecycle reassignment delivery "+binding.AttemptID, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE"); err != nil {
			return err
		}
		row, err := readSlingReceipt(ctx, conn, key, true)
		if err != nil {
			return err
		}
		if row == nil || row.payload != string(payload) {
			return fmt.Errorf("lifecycle reassignment has no exact immutable custody receipt")
		}
		if row.state == "applied" {
			if row.deliveryID == deliveryID && row.generation == intentHash {
				return nil
			}
			return fmt.Errorf("applied lifecycle reassignment cannot be rebound")
		}
		_, _, assignee, _, err := readIssueSlingSnapshot(ctx, conn, binding.BeadID)
		if err != nil {
			return err
		}
		if assignee != binding.NewAssignee {
			return fmt.Errorf("work %s is assigned to %s, not replacement %s", binding.BeadID, assignee, binding.NewAssignee)
		}
		result, err := conn.ExecContext(ctx, `UPDATE `+slingReceiptTable+`
			SET state = 'delivered', delivery_id = ?, generation = ?, updated_at = CURRENT_TIMESTAMP(6)
			WHERE receipt_key = ? AND payload = ? AND state IN ('prepared', 'delivered')`,
			deliveryID, intentHash, key, string(payload))
		if err != nil {
			return err
		}
		if rows, err := result.RowsAffected(); err != nil || rows != 1 {
			return fmt.Errorf("lifecycle reassignment delivery CAS failed")
		}
		return nil
	})
}

func (b *Beads) ValidateLifecycleReassignmentDelivery(binding LifecycleReassignmentBinding, deliveryID, intentHash string) error {
	if err := validateLifecycleReassignmentBinding(binding); err != nil {
		return err
	}
	payload, _ := json.Marshal(binding)
	db, err := openDoltSQL(b.getResolvedBeadsDir())
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), sqlCleanupTimeout)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	row, err := readSlingReceipt(ctx, conn, slingReceiptKey("retirement", binding.AttemptID), false)
	if err != nil {
		return err
	}
	if row == nil || row.payload != string(payload) || (row.state != "delivered" && row.state != "applied") || row.deliveryID != deliveryID || row.generation != intentHash {
		return fmt.Errorf("lifecycle reassignment delivery lacks exact database custody")
	}
	return nil
}

func (b *Beads) MarkLifecycleReassignmentApplied(binding LifecycleReassignmentBinding, deliveryID, intentHash string) error {
	if err := b.ValidateLifecycleReassignmentDelivery(binding, deliveryID, intentHash); err != nil {
		return err
	}
	payload, _ := json.Marshal(binding)
	key := slingReceiptKey("retirement", binding.AttemptID)
	return mutateAndDoltCommit(b.getResolvedBeadsDir(), "gt: apply lifecycle reassignment "+binding.AttemptID, func(ctx context.Context, conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx, `UPDATE `+slingReceiptTable+`
			SET state = 'applied', updated_at = CURRENT_TIMESTAMP(6)
			WHERE receipt_key = ? AND payload = ? AND delivery_id = ? AND generation = ? AND state IN ('delivered', 'applied')`,
			key, string(payload), deliveryID, intentHash)
		if err != nil {
			return err
		}
		if rows, err := result.RowsAffected(); err != nil || rows != 1 {
			return fmt.Errorf("lifecycle reassignment applied CAS failed")
		}
		return nil
	})
}

func validateFormulaMoleculePublication(publication FormulaMoleculePublication) error {
	for _, value := range []string{publication.PublicationID, publication.RootID, publication.RootGeneration, publication.WorkID, publication.NewStatus, publication.NewAssignee} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("formula publication is incomplete")
		}
	}
	if err := validateFormulaMoleculeAuthorization(publication.Authorization); err != nil {
		return err
	}
	if publication.Authorization.PublicationID != publication.PublicationID ||
		publication.Authorization.RootID != publication.RootID ||
		publication.Authorization.RootGeneration != publication.RootGeneration ||
		!validDoltCommitHash(publication.AuthorizationCommit) {
		return fmt.Errorf("formula publication lacks exact committed authorization")
	}
	return nil
}

func validateFormulaMoleculeAuthorization(authorization FormulaMoleculeAuthorization) error {
	for _, value := range []string{
		authorization.PublicationID, authorization.RootID, authorization.RootGeneration,
		authorization.Formula, authorization.Owner, authorization.RequestFingerprint, authorization.Actor,
	} {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("formula publication authorization is incomplete")
		}
	}
	return nil
}

func (b *Beads) formulaAuthorizationAtCommit(authorization FormulaMoleculeAuthorization, oid string) (bool, error) {
	if !validDoltCommitHash(oid) {
		return false, fmt.Errorf("invalid formula authorization commit %q", oid)
	}
	payload, err := json.Marshal(authorization)
	if err != nil {
		return false, err
	}
	db, err := openDoltSQL(b.getResolvedBeadsDir())
	if err != nil {
		return false, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), sqlCleanupTimeout)
	defer cancel()
	query := fmt.Sprintf(`SELECT payload, state FROM %s AS OF '%s' WHERE receipt_key = ?`, slingReceiptTable, oid)
	var gotPayload, state string
	err = db.QueryRowContext(ctx, query, slingReceiptKey("formula", authorization.PublicationID)).Scan(&gotPayload, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && gotPayload == string(payload) && state == "prepared", err
}

// PrepareFormulaMoleculeAuthorization commits the independently verified
// request and exact graph generation before publication can consume it.
func (b *Beads) PrepareFormulaMoleculeAuthorization(authorization FormulaMoleculeAuthorization) (string, error) {
	if err := validateFormulaMoleculeAuthorization(authorization); err != nil {
		return "", err
	}
	payload, _ := json.Marshal(authorization)
	if err := b.ensureSlingReceiptTable(); err != nil {
		return "", err
	}
	message := "gt: authorize formula molecule " + authorization.PublicationID
	if oid, err := doltCommitOIDForMessageInStore(b.getResolvedBeadsDir(), message); err == nil {
		if matched, matchErr := b.formulaAuthorizationAtCommit(authorization, oid); matchErr == nil && matched {
			return oid, nil
		}
	}
	key := slingReceiptKey("formula", authorization.PublicationID)
	oid, err := mutateAndDoltCommitWithOID(b.getResolvedBeadsDir(), message, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE"); err != nil {
			return err
		}
		row, err := readSlingReceipt(ctx, conn, key, true)
		if err != nil {
			return err
		}
		if row != nil {
			if row.payload == string(payload) && row.state == "prepared" {
				return nil
			}
			return fmt.Errorf("formula authorization attempt already binds different custody")
		}
		graph, err := captureFormulaMoleculeGraphConn(ctx, conn, authorization.RootID, true)
		if err != nil {
			return err
		}
		generation, err := FormulaMoleculeGraphGeneration(graph)
		if err != nil {
			return err
		}
		if generation != authorization.RootGeneration {
			return fmt.Errorf("formula molecule %s generation changed before authorization", authorization.RootID)
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO `+slingReceiptTable+`
			(receipt_key, kind, target_id, payload, state, generation) VALUES (?, 'formula', ?, ?, 'prepared', ?)`,
			key, authorization.RootID, string(payload), authorization.RootGeneration)
		return err
	})
	if err == nil {
		return oid, nil
	}
	if recovered, lookupErr := doltCommitOIDForMessageInStore(b.getResolvedBeadsDir(), message); lookupErr == nil {
		if matched, matchErr := b.formulaAuthorizationAtCommit(authorization, recovered); matchErr == nil && matched {
			return recovered, nil
		}
	}
	var unknown *DoltCommitOutcomeUnknownError
	if !errors.As(err, &unknown) {
		return "", err
	}
	recovered, finishErr := finishDoltCommitWithOID(b.getResolvedBeadsDir(), message)
	if finishErr != nil {
		return "", errors.Join(err, finishErr)
	}
	matched, matchErr := b.formulaAuthorizationAtCommit(authorization, recovered)
	if matchErr != nil || !matched {
		return "", errors.Join(err, matchErr, fmt.Errorf("formula authorization is absent from recovered commit %s", recovered))
	}
	return recovered, nil
}

// PublishFormulaMoleculeAssignment verifies the exact formula graph and
// publishes the work hook plus metadata in the same serializable transaction.
func (b *Beads) PublishFormulaMoleculeAssignment(publication FormulaMoleculePublication) error {
	if err := validateFormulaMoleculePublication(publication); err != nil {
		return err
	}
	payload, err := json.Marshal(publication)
	if err != nil {
		return err
	}
	if authorized, err := b.formulaAuthorizationAtCommit(publication.Authorization, publication.AuthorizationCommit); err != nil || !authorized {
		return errors.Join(err, fmt.Errorf("formula publication lacks immutable authorization at %s", publication.AuthorizationCommit))
	}
	key := slingReceiptKey("formula", publication.PublicationID)
	message := "gt: publish formula molecule " + publication.PublicationID
	err = func() error {
		_, mutationErr := mutateAndDoltCommitWithOID(b.getResolvedBeadsDir(), message, func(ctx context.Context, conn *sql.Conn) error {
			if _, err := conn.ExecContext(ctx, "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE"); err != nil {
				return err
			}
			row, err := readSlingReceipt(ctx, conn, key, true)
			if err != nil {
				return err
			}
			table, status, assignee, description, err := readIssueSlingSnapshot(ctx, conn, publication.WorkID)
			if err != nil {
				return err
			}
			authorizationPayload, _ := json.Marshal(publication.Authorization)
			if row != nil {
				if row.payload == string(payload) && row.state == "published" &&
					status == publication.NewStatus && assignee == publication.NewAssignee && description == publication.NewDescription {
					return nil
				}
				if row.payload != string(authorizationPayload) || row.state != "prepared" || row.generation != publication.RootGeneration {
					return fmt.Errorf("formula publication attempt already binds different custody")
				}
			} else {
				return fmt.Errorf("formula publication has no prepared authorization")
			}
			graph, err := captureFormulaMoleculeGraphConn(ctx, conn, publication.RootID, true)
			if err != nil {
				return err
			}
			generation, err := FormulaMoleculeGraphGeneration(graph)
			if err != nil {
				return err
			}
			if generation != publication.RootGeneration {
				return fmt.Errorf("formula molecule %s generation changed before publication", publication.RootID)
			}
			if beforeFormulaMoleculePublicationCAS != nil {
				if err := beforeFormulaMoleculePublicationCAS(); err != nil {
					return err
				}
			}
			if status != publication.ExpectedStatus || assignee != publication.ExpectedAssignee || description != publication.ExpectedDescription {
				return fmt.Errorf("formula publication work snapshot changed")
			}
			result, err := conn.ExecContext(ctx, `UPDATE `+table+`
			SET status = ?, assignee = ?, description = ?, updated_at = CURRENT_TIMESTAMP(6)
			WHERE id = ? AND status = ? AND COALESCE(assignee, '') = ? AND COALESCE(description, '') = ?`,
				publication.NewStatus, publication.NewAssignee, publication.NewDescription,
				publication.WorkID, publication.ExpectedStatus, publication.ExpectedAssignee, publication.ExpectedDescription)
			if err != nil {
				return err
			}
			if rows, err := result.RowsAffected(); err != nil || rows != 1 {
				return fmt.Errorf("formula publication assignment CAS failed")
			}
			result, err = conn.ExecContext(ctx, `UPDATE `+slingReceiptTable+`
			SET target_id = ?, payload = ?, state = 'published', updated_at = CURRENT_TIMESTAMP(6)
			WHERE receipt_key = ? AND payload = ? AND state = 'prepared' AND generation = ?`,
				publication.WorkID, string(payload), key, string(authorizationPayload), publication.RootGeneration)
			if err != nil {
				return err
			}
			if rows, err := result.RowsAffected(); err != nil || rows != 1 {
				return fmt.Errorf("formula publication receipt CAS failed")
			}
			return nil
		})
		return mutationErr
	}()
	if err == nil {
		return nil
	}
	if oid, lookupErr := doltCommitOIDForMessageInStore(b.getResolvedBeadsDir(), message); lookupErr == nil {
		if applied, applyErr := b.formulaMoleculePublicationAtCommit(publication, oid); applyErr == nil && applied {
			return nil
		}
	}
	var unknown *DoltCommitOutcomeUnknownError
	if !errors.As(err, &unknown) {
		return err
	}
	oid, finishErr := finishDoltCommitWithOID(b.getResolvedBeadsDir(), message)
	if finishErr != nil {
		return errors.Join(err, finishErr)
	}
	applied, reconcileErr := b.formulaMoleculePublicationAtCommit(publication, oid)
	if reconcileErr == nil && applied {
		return nil
	}
	return errors.Join(err, reconcileErr, fmt.Errorf("formula publication is absent from recovered commit %s", oid))
}

func (b *Beads) formulaMoleculePublicationAtCommit(publication FormulaMoleculePublication, oid string) (bool, error) {
	if !validDoltCommitHash(oid) {
		return false, fmt.Errorf("invalid formula publication commit %q", oid)
	}
	payload, err := json.Marshal(publication)
	if err != nil {
		return false, err
	}
	db, err := openDoltSQL(b.getResolvedBeadsDir())
	if err != nil {
		return false, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), sqlCleanupTimeout)
	defer cancel()
	receiptQuery := fmt.Sprintf(`SELECT payload, state FROM %s AS OF '%s' WHERE receipt_key = ?`, slingReceiptTable, oid)
	var gotPayload, state string
	if err := db.QueryRowContext(ctx, receiptQuery, slingReceiptKey("formula", publication.PublicationID)).Scan(&gotPayload, &state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if gotPayload != string(payload) || state != "published" {
		return false, nil
	}
	matches := 0
	for _, table := range []string{"issues", "wisps"} {
		query := fmt.Sprintf(`SELECT status, COALESCE(assignee, ''), COALESCE(description, '') FROM %s AS OF '%s' WHERE id = ?`, table, oid)
		var status, assignee, description string
		err := db.QueryRowContext(ctx, query, publication.WorkID).Scan(&status, &assignee, &description)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil && strings.Contains(err.Error(), "Error 1146") {
			continue
		}
		if err != nil {
			return false, err
		}
		if status != publication.NewStatus || assignee != publication.NewAssignee || description != publication.NewDescription {
			return false, nil
		}
		matches++
	}
	if matches == 1 {
		return true, nil
	}
	if matches != 0 {
		return false, nil
	}
	table, status, assignee, description, err := readIssueSlingSnapshot(ctx, db, publication.WorkID)
	if err != nil {
		return false, err
	}
	return table == "wisps" && status == publication.NewStatus && assignee == publication.NewAssignee && description == publication.NewDescription, nil
}

func (b *Beads) formulaMoleculePublicationCommitted(publication FormulaMoleculePublication) (bool, error) {
	oid, err := doltCommitOIDForMessageInStore(b.getResolvedBeadsDir(), "gt: publish formula molecule "+publication.PublicationID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return b.formulaMoleculePublicationAtCommit(publication, oid)
}

func (b *Beads) FormulaMoleculePublicationApplied(publication FormulaMoleculePublication) (bool, error) {
	committed, err := b.formulaMoleculePublicationCommitted(publication)
	if err != nil || !committed {
		return false, err
	}
	payload, err := json.Marshal(publication)
	if err != nil {
		return false, err
	}
	db, err := openDoltSQL(b.getResolvedBeadsDir())
	if err != nil {
		return false, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), sqlCleanupTimeout)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	row, err := readSlingReceipt(ctx, conn, slingReceiptKey("formula", publication.PublicationID), false)
	if err != nil || row == nil || row.payload != string(payload) || row.state != "published" {
		return false, err
	}
	_, status, assignee, description, err := readIssueSlingSnapshot(ctx, conn, publication.WorkID)
	if err != nil {
		return false, err
	}
	return status == publication.NewStatus && assignee == publication.NewAssignee && description == publication.NewDescription, nil
}

// FormulaMoleculePublicationByID returns a published receipt only while its
// exact work snapshot remains current.
func (b *Beads) FormulaMoleculePublicationByID(publicationID string) (*FormulaMoleculePublication, error) {
	db, err := openDoltSQL(b.getResolvedBeadsDir())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), sqlCleanupTimeout)
	defer cancel()
	row, err := readSlingReceipt(ctx, db, slingReceiptKey("formula", publicationID), false)
	if err != nil || row == nil || row.state != "published" {
		return nil, err
	}
	var publication FormulaMoleculePublication
	if err := json.Unmarshal([]byte(row.payload), &publication); err != nil {
		return nil, err
	}
	if err := validateFormulaMoleculePublication(publication); err != nil {
		return nil, err
	}
	committed, err := b.formulaMoleculePublicationCommitted(publication)
	if err != nil || !committed {
		return nil, err
	}
	_, status, assignee, description, err := readIssueSlingSnapshot(ctx, db, publication.WorkID)
	if err != nil {
		return nil, err
	}
	if status != publication.NewStatus || assignee != publication.NewAssignee || description != publication.NewDescription {
		return nil, nil
	}
	return &publication, nil
}
