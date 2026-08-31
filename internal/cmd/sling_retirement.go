package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"github.com/google/uuid"
	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/mail"
	witnesspkg "github.com/steveyegge/gastown/internal/witness"
)

type slingRetirementRecord = witnesspkg.LifecycleRetirementIntent

var capturePolecatIncarnationFn = capturePolecatIncarnation
var completeRetirementRecordFn = completeRetirementRecord
var abortRetirementRecordFn = abortRetirementRecord
var retirementMessageStoredFn = retirementMessageStored
var sendRetirementMessageFn = sendRetirementMessage
var retirementAcceptanceStoredFn = retirementAcceptanceStored
var validateRetirementReplacementFn = validateRetirementReplacement
var retirementAppliedPostconditionFn = witnesspkg.LifecycleRetirementApplied
var captureLifecycleWitnessAuthorityFn = witnesspkg.CaptureLifecycleWitnessAuthority
var lifecycleRetirementPortableFn = func() bool { return runtime.GOOS != "linux" }

var errSlingRetirementPending = errors.New("old-owner retirement is pending")

func slingReceiptDatabaseConfigured(townRoot, beadID string) bool {
	beadsDir := beads.ResolveBeadsDirForID(filepath.Join(townRoot, ".beads"), beadID)
	info, err := os.Stat(filepath.Join(beadsDir, "metadata.json"))
	return err == nil && info.Mode().IsRegular()
}

var slingReceiptDatabaseConfiguredFn = slingReceiptDatabaseConfigured

func capturePolecatIncarnation(townRoot, assignee string) (string, error) {
	parts := strings.Split(assignee, "/")
	if len(parts) < 3 || parts[1] != "polecats" {
		return "", nil
	}
	agentID := agentIDToBeadID(assignee, townRoot)
	if agentID == "" {
		return "", fmt.Errorf("resolving agent bead for %s", assignee)
	}
	_, fields, err := beads.New(townRoot).GetAgentBead(agentID)
	if err != nil {
		return "", fmt.Errorf("reading incarnation for %s: %w", assignee, err)
	}
	if fields == nil || strings.TrimSpace(fields.Incarnation) == "" {
		return "", fmt.Errorf("missing incarnation for %s", assignee)
	}
	return strings.TrimSpace(fields.Incarnation), nil
}

func prepareRetirementRecord(townRoot, beadID, oldAssignee, oldIncarnation, newAssignee, newIncarnation, requester string) (*slingRetirementRecord, error) {
	if oldAssignee == "" || oldIncarnation == "" || (oldAssignee == newAssignee && oldIncarnation == newIncarnation) {
		return nil, nil
	}
	parts := strings.Split(oldAssignee, "/")
	if len(parts) < 3 || parts[1] != "polecats" {
		return nil, nil
	}
	if strings.Contains(newAssignee, "/polecats/") && newIncarnation == "" {
		return nil, fmt.Errorf("missing replacement incarnation for %s", newAssignee)
	}
	attemptID := uuid.NewString()
	record := &slingRetirementRecord{
		AttemptID: attemptID, Capability: uuid.NewString(), BeadID: beadID,
		OldAssignee: oldAssignee, OldIncarnation: oldIncarnation,
		NewAssignee: newAssignee, NewIncarnation: newIncarnation,
		Requester: requester, ThreadID: "sling-retirement-" + attemptID, State: "pending",
	}
	if slingReceiptDatabaseConfigured(townRoot, beadID) {
		if err := witnesspkg.PrepareLifecycleReassignmentDatabaseReceipt(townRoot, record); err != nil {
			return nil, fmt.Errorf("recording prior reassignment custody: %w", err)
		}
	}
	if err := writeRetirementRecord(townRoot, record); err != nil {
		return nil, fmt.Errorf("persisting reassignment retirement intent: %w", err)
	}
	return record, nil
}

func retirementRecordPath(townRoot, attemptID string) (string, error) {
	return retirementRecordPathInDir(townRoot, "sling-retirements", attemptID)
}

func retirementRecordPathInDir(townRoot, directory, attemptID string) (string, error) {
	parsed, err := uuid.Parse(attemptID)
	if err != nil || parsed.String() != attemptID {
		return "", fmt.Errorf("invalid retirement attempt ID %q", attemptID)
	}
	dir := filepath.Clean(filepath.Join(townRoot, ".runtime", directory))
	path := filepath.Clean(filepath.Join(dir, attemptID+".json"))
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("retirement record path escapes %s", dir)
	}
	return path, nil
}

func writeRetirementRecord(townRoot string, record *slingRetirementRecord) error {
	if record == nil {
		return fmt.Errorf("nil retirement record")
	}
	terminal := record.State == "accepted" || record.State == "aborted"
	if !terminal {
		return witnesspkg.WriteLifecycleRetirementIntent(townRoot, record)
	}
	directory := "sling-retirements-terminal"
	path, err := retirementRecordPathInDir(townRoot, directory, record.AttemptID)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("retirement record %s is not a regular file", filepath.Base(path))
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := atomicfile.EnsureDirAndWriteJSONWithPerm(path, record, 0o600); err != nil {
		return err
	}
	activePath, err := retirementRecordPath(townRoot, record.AttemptID)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(activePath); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("retirement record %s is not a regular file", filepath.Base(activePath))
		}
		if err := os.Remove(activePath); err != nil {
			return fmt.Errorf("removing terminal retirement record from active queue: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func abortRetirementRecord(townRoot string, record *slingRetirementRecord) error {
	if record == nil {
		return nil
	}
	record.State = "aborted"
	return writeRetirementRecord(townRoot, record)
}

func completeRetirementRecord(townRoot string, record *slingRetirementRecord) error {
	if record == nil {
		return nil
	}
	parts := strings.Split(record.OldAssignee, "/")
	if len(parts) < 3 || parts[1] != "polecats" {
		return abortRetirementRecord(townRoot, record)
	}
	witness := fmt.Sprintf("%s/witness", parts[0])
	request := func(candidate *slingRetirementRecord) *mail.Message {
		return &mail.Message{
			From: "mayor/", To: witness,
			Subject: fmt.Sprintf("LIFECYCLE:Shutdown %s", parts[2]),
			Body:    witnesspkg.LifecycleRetirementRequestBody(candidate),
			Type:    mail.TypeTask, Priority: mail.PriorityHigh, ThreadID: candidate.ThreadID,
		}
	}
	candidates := []*slingRetirementRecord{record}
	if record.DeliveryID == "" && record.WitnessAuthority != nil {
		predecessor := *record
		predecessor.WitnessAuthority = nil
		candidates = []*slingRetirementRecord{&predecessor, record}
	}
	var acceptanceErr error
	acceptanceChecked := false
	for _, candidate := range candidates {
		accepted, err := retirementAcceptanceStoredFn(townRoot, witness, request(candidate), candidate)
		if err != nil {
			acceptanceErr = errors.Join(acceptanceErr, err)
			continue
		}
		acceptanceChecked = true
		if !accepted {
			continue
		}
		record.State = "accepted"
		if err := writeRetirementRecord(townRoot, record); err != nil {
			return fmt.Errorf("persisting reassignment retirement acceptance: %w", err)
		}
		return nil
	}
	if !acceptanceChecked && acceptanceErr != nil {
		return acceptanceErr
	}
	if err := validateRetirementReplacementFn(townRoot, record); err != nil {
		return err
	}
	if slingReceiptDatabaseConfiguredFn(townRoot, record.BeadID) && record.Version != 2 {
		if err := abortRetirementRecord(townRoot, record); err != nil {
			return fmt.Errorf("terminalizing legacy retirement intent: %w", err)
		}
		return nil
	}
	if lifecycleRetirementPortableFn() {
		authority, err := captureLifecycleWitnessAuthorityFn(townRoot, parts[0])
		if err != nil {
			return fmt.Errorf("capturing portable Witness lifecycle authority: %w", err)
		}
		if record.DeliveryID == "" || !reflect.DeepEqual(record.WitnessAuthority, authority) {
			record.DeliveryID = uuid.NewString()
			record.WitnessAuthority = authority
			if err := writeRetirementRecord(townRoot, record); err != nil {
				return fmt.Errorf("persisting portable Witness lifecycle delivery: %w", err)
			}
		}
	} else if record.DeliveryID == "" {
		record.DeliveryID = uuid.NewString()
		if err := writeRetirementRecord(townRoot, record); err != nil {
			return fmt.Errorf("persisting brokered Witness lifecycle delivery: %w", err)
		}
	}
	if slingReceiptDatabaseConfiguredFn(townRoot, record.BeadID) {
		if err := witnesspkg.BindLifecycleReassignmentDatabaseDelivery(townRoot, record); err != nil {
			return fmt.Errorf("binding retirement delivery to reassignment custody: %w", err)
		}
	}
	msg := request(record)
	stored, err := retirementMessageStoredFn(townRoot, witness, msg)
	if err != nil {
		return err
	}
	if !stored {
		sendErr := sendRetirementMessageFn(townRoot, msg)
		if sendErr != nil {
			stored, err = retirementMessageStoredFn(townRoot, witness, msg)
			if err != nil || !stored {
				return errors.Join(sendErr, err)
			}
		}
	}
	if err := validateRetirementReplacementFn(townRoot, record); err != nil {
		return err
	}
	record.State = "pending"
	if err := writeRetirementRecord(townRoot, record); err != nil {
		return fmt.Errorf("persisting pending reassignment retirement request: %w", err)
	}
	return fmt.Errorf("%w for %s", errSlingRetirementPending, record.OldAssignee)
}

func validateRetirementReplacement(townRoot string, record *slingRetirementRecord) error {
	info, err := getBeadInfoFromTownRoot(townRoot, record.BeadID)
	if err != nil {
		return fmt.Errorf("revalidating replacement work custody: %w", err)
	}
	if info.Assignee != record.NewAssignee {
		return fmt.Errorf("replacement custody for %s changed from %s to %s", record.BeadID, record.NewAssignee, info.Assignee)
	}
	if !strings.Contains(record.NewAssignee, "/polecats/") {
		if record.NewIncarnation != "" {
			return fmt.Errorf("non-polecat replacement %s has unexpected incarnation", record.NewAssignee)
		}
		return nil
	}
	incarnation, err := capturePolecatIncarnationFn(townRoot, record.NewAssignee)
	if err != nil {
		return err
	}
	if incarnation != record.NewIncarnation {
		return fmt.Errorf("replacement incarnation for %s changed from %s to %s", record.NewAssignee, record.NewIncarnation, incarnation)
	}
	return nil
}

func sendRetirementMessage(townRoot string, msg *mail.Message) error {
	router := mail.NewRouter(townRoot)
	err := router.Send(msg)
	waitForMailNotifications(router)
	return err
}

func retirementMessageStored(townRoot, witness string, expected *mail.Message) (bool, error) {
	message, err := storedRetirementRequest(townRoot, witness, expected)
	return message != nil, err
}

func storedRetirementRequest(townRoot, witness string, expected *mail.Message) (*mail.Message, error) {
	messages, err := mail.NewMailboxFromAddress(witness, townRoot).ListByThread(expected.ThreadID)
	if err != nil {
		return nil, fmt.Errorf("reconciling reassignment retirement mail: %w", err)
	}
	for _, message := range messages {
		if mail.AddressToIdentity(message.From) == mail.AddressToIdentity(expected.From) &&
			mail.AddressToIdentity(message.To) == mail.AddressToIdentity(expected.To) &&
			message.Subject == expected.Subject && message.Body == expected.Body {
			return message, nil
		}
	}
	return nil, nil
}

func retirementAcceptanceStored(townRoot, witness string, expected *mail.Message, record *slingRetirementRecord) (bool, error) {
	receipt, err := witnesspkg.LoadLifecycleRetirementReceipt(townRoot, record)
	if err != nil || receipt == nil {
		return false, err
	}
	requests, err := mail.NewMailboxFromAddress(witness, townRoot).ListByThread(expected.ThreadID)
	if err != nil {
		return false, fmt.Errorf("reconciling reassignment retirement mail: %w", err)
	}
	var request *mail.Message
	for _, candidate := range requests {
		if candidate.ID == receipt.RequestID &&
			mail.AddressToIdentity(candidate.From) == mail.AddressToIdentity(expected.From) &&
			mail.AddressToIdentity(candidate.To) == mail.AddressToIdentity(expected.To) &&
			candidate.Subject == expected.Subject && candidate.Body == expected.Body {
			request = candidate
			break
		}
	}
	if request == nil {
		return false, nil
	}
	applied, err := retirementAppliedPostconditionFn(townRoot, record)
	if err != nil {
		return false, fmt.Errorf("verifying lifecycle retirement postcondition: %w", err)
	}
	return applied, nil
}

func resumePendingRetirement(townRoot, beadID, _ string) (bool, error) {
	paths, err := filepath.Glob(filepath.Join(townRoot, ".runtime", "sling-retirements", "*.json"))
	if err != nil {
		return false, err
	}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			return false, err
		}
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("retirement record %s is not a regular file", filepath.Base(path))
		}
		data, err := os.ReadFile(path) //nolint:gosec // paths come from the internal retirement directory
		if err != nil {
			return false, err
		}
		var record slingRetirementRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return false, fmt.Errorf("reading reassignment retirement record %s: %w", filepath.Base(path), err)
		}
		expectedPath, err := retirementRecordPath(townRoot, record.AttemptID)
		if err != nil {
			return false, fmt.Errorf("reading reassignment retirement record %s: %w", filepath.Base(path), err)
		}
		if filepath.Clean(path) != expectedPath {
			return false, fmt.Errorf("retirement record %s does not match attempt %s", filepath.Base(path), record.AttemptID)
		}
		if record.State != "pending" || record.BeadID != beadID {
			continue
		}
		if err := completeRetirementRecordFn(townRoot, &record); err != nil {
			if errors.Is(err, errSlingRetirementPending) {
				return true, err
			}
			return false, err
		}
		if record.State != "accepted" {
			return true, fmt.Errorf("%w for %s", errSlingRetirementPending, record.OldAssignee)
		}
		return true, nil
	}
	return false, nil
}
