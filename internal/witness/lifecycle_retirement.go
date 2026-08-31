package witness

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/uuid"
	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

const (
	lifecycleRetirementIntentVersion         = 2
	lifecycleRetirementBrokerAuthority       = "trusted-lifecycle-dispatch-v1"
	lifecycleRetirementLegacyBrokerAuthority = "linux-session-broker-v1"
)

// ErrLifecycleRequestRejected marks permanent authentication or schema
// failures. Dispatchers may quarantine only errors in this class; operational
// and persistence failures must remain retryable.
var ErrLifecycleRequestRejected = errors.New("lifecycle request rejected")

var (
	lifecycleRetirementBrokerSupportedFn           = func() bool { return runtime.GOOS == "linux" }
	lifecycleRetirementBrokerWorkerFn              = func() bool { return os.Getenv(tmux.EnvSessionBrokerWorker) == "1" }
	lifecycleRetirementPortableAuthorityRequiredFn = func() bool { return runtime.GOOS != "linux" }
)

// LifecycleWitnessAuthority binds a portable retirement request to the exact
// Witness pane process authorized by the trusted sender before mail delivery.
type LifecycleWitnessAuthority struct {
	Session tmux.SessionGeneration     `json:"session"`
	Pane    tmux.PaneProcessGeneration `json:"pane"`
}

// LifecycleRetirementIntent is the canonical local authority for retiring one
// exact polecat generation after its replacement becomes viable.
type LifecycleRetirementIntent struct {
	Version    int    `json:"version,omitempty"`
	AttemptID  string `json:"attempt_id"`
	DeliveryID string `json:"delivery_id,omitempty"`
	// Capability stays in the host-side intent store and keys the applied
	// receipt MAC. It must never be copied into contained-caller-visible mail.
	Capability       string                     `json:"capability"`
	BeadID           string                     `json:"bead_id"`
	OldAssignee      string                     `json:"old_assignee"`
	OldIncarnation   string                     `json:"old_incarnation"`
	NewAssignee      string                     `json:"new_assignee"`
	NewIncarnation   string                     `json:"new_incarnation"`
	Requester        string                     `json:"requester"`
	ThreadID         string                     `json:"thread_id"`
	State            string                     `json:"state"`
	WitnessAuthority *LifecycleWitnessAuthority `json:"witness_authority,omitempty"`
}

// CaptureLifecycleWitnessAuthority records the current canonical Witness pane
// outside the Witness session before a portable retirement request is sent.
func CaptureLifecycleWitnessAuthority(townRoot, rigName string) (*LifecycleWitnessAuthority, error) {
	expectedSession := session.WitnessSessionName(session.PrefixFor(rigName))
	canonical := tmux.NewTmuxWithSocketAndEnv(session.TownSocketName(townRoot), []string{"PATH=" + os.Getenv("PATH")})
	generation, err := canonical.CaptureSessionGeneration(expectedSession)
	if err != nil {
		return nil, fmt.Errorf("capturing canonical Witness session generation: %w", err)
	}
	pane, err := canonical.CapturePaneProcessGeneration(generation)
	if err != nil {
		return nil, fmt.Errorf("capturing canonical Witness pane process: %w", err)
	}
	return &LifecycleWitnessAuthority{Session: generation, Pane: pane}, nil
}

func validateLifecycleWitnessAuthorityRecord(authority *LifecycleWitnessAuthority, expectedSession string) error {
	if authority == nil || authority.Session.Name != expectedSession ||
		authority.Session.SessionID == "" || authority.Session.PaneID == "" ||
		authority.Session.Nonce == "" || authority.Session.ServerPID <= 0 ||
		authority.Session.ServerIdentity == "" || !authority.Session.Equal(authority.Session) ||
		authority.Pane.PID <= 0 || authority.Pane.Identity == "" {
		return fmt.Errorf("%w: lifecycle retirement lacks exact original Witness pane authority", ErrLifecycleRequestRejected)
	}
	return nil
}

// ValidateLifecycleWitnessAuthority rejects a caller whose canonical pane was
// respawned after the trusted sender persisted the retirement request.
func ValidateLifecycleWitnessAuthority(ctx context.Context, rigName string, authority *LifecycleWitnessAuthority) error {
	expectedSession := session.WitnessSessionName(session.PrefixFor(rigName))
	if err := validateLifecycleWitnessAuthorityRecord(authority, expectedSession); err != nil {
		return err
	}
	bound, err := tmux.NewTmuxForSessionGeneration(authority.Session)
	if err != nil {
		return fmt.Errorf("%w: lifecycle Witness transport is not exact", ErrLifecycleRequestRejected)
	}
	paneID, panePID, currentSession, err := bound.ResolveCurrentPaneGeneration()
	if err != nil {
		return fmt.Errorf("resolving current Witness pane process: %w", err)
	}
	if currentSession != expectedSession || paneID != authority.Session.PaneID || panePID != authority.Pane.PID {
		return fmt.Errorf("%w: lifecycle caller is not the original Witness pane", ErrLifecycleRequestRejected)
	}
	current, err := bound.CaptureSessionGenerationContext(ctx, expectedSession)
	if err != nil {
		return fmt.Errorf("recapturing canonical Witness session: %w", err)
	}
	if !authority.Session.Equal(current) {
		return fmt.Errorf("%w: canonical Witness session generation changed", ErrLifecycleRequestRejected)
	}
	pane, err := bound.CapturePaneProcessGeneration(current)
	if err != nil {
		return fmt.Errorf("recapturing canonical Witness pane process: %w", err)
	}
	if !authority.Pane.Equal(pane) {
		return fmt.Errorf("%w: canonical Witness pane process changed", ErrLifecycleRequestRejected)
	}
	return nil
}

// LifecycleRetirementAppliedReceipt proves that Witness applied the exact
// canonical intent before acknowledging it through mail.
type LifecycleRetirementAppliedReceipt struct {
	AttemptID  string `json:"attempt_id"`
	DeliveryID string `json:"delivery_id,omitempty"`
	RequestID  string `json:"request_id"`
	ReceiptID  string `json:"receipt_id"`
	IntentHash string `json:"intent_hash"`
	Authority  string `json:"authority"`
	State      string `json:"state"`
	MAC        string `json:"mac"`
	legacy     bool
}

func requireLifecycleRetirementBrokerAuthority() error {
	if !lifecycleRetirementBrokerSupportedFn() || !lifecycleRetirementBrokerWorkerFn() {
		return fmt.Errorf("lifecycle retirement requires a trusted session-broker worker")
	}
	return nil
}

func lifecycleRetirementPath(townRoot, directory, attemptID string) (string, error) {
	if !validLifecycleRetirementAttemptID(attemptID) {
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

func validLifecycleRetirementAttemptID(attemptID string) bool {
	parsed, err := uuid.Parse(attemptID)
	return err == nil && parsed.String() == attemptID
}

func validateLifecycleRetirementIntent(intent *LifecycleRetirementIntent) error {
	if intent == nil {
		return fmt.Errorf("nil lifecycle retirement intent")
	}
	parsedAttempt, attemptErr := uuid.Parse(intent.AttemptID)
	parsedCapability, capabilityErr := uuid.Parse(intent.Capability)
	if attemptErr != nil || parsedAttempt.String() != intent.AttemptID ||
		capabilityErr != nil || parsedCapability.String() != intent.Capability {
		return fmt.Errorf("lifecycle retirement intent has invalid authority identifiers")
	}
	if intent.State != "pending" || intent.ThreadID != "sling-retirement-"+intent.AttemptID ||
		intent.BeadID == "" || intent.OldAssignee == "" || intent.OldIncarnation == "" ||
		intent.NewAssignee == "" || (strings.Contains(intent.NewAssignee, "/polecats/") && intent.NewIncarnation == "") {
		return fmt.Errorf("lifecycle retirement intent lacks exact generation custody")
	}
	if intent.WitnessAuthority != nil {
		rigName := strings.SplitN(intent.OldAssignee, "/", 2)[0]
		if err := validateLifecycleWitnessAuthorityRecord(intent.WitnessAuthority, session.WitnessSessionName(session.PrefixFor(rigName))); err != nil {
			return err
		}
	}
	if intent.DeliveryID != "" {
		parsedDelivery, err := uuid.Parse(intent.DeliveryID)
		if err != nil || parsedDelivery.String() != intent.DeliveryID ||
			(lifecycleRetirementPortableAuthorityRequiredFn() && intent.WitnessAuthority == nil) {
			return fmt.Errorf("lifecycle retirement intent has invalid delivery authority")
		}
	}
	for _, field := range []string{intent.BeadID, intent.OldAssignee, intent.OldIncarnation, intent.NewAssignee, intent.NewIncarnation, intent.Requester} {
		if strings.ContainsAny(field, "\r\n") {
			return fmt.Errorf("lifecycle retirement intent contains multiline authority fields")
		}
	}
	return nil
}

// WriteLifecycleRetirementIntent persists an active intent with private permissions.
func WriteLifecycleRetirementIntent(townRoot string, intent *LifecycleRetirementIntent) error {
	if err := validateLifecycleRetirementIntent(intent); err != nil {
		return err
	}
	path, err := lifecycleRetirementPath(townRoot, "sling-retirements", intent.AttemptID)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("retirement intent %s is not a regular file", filepath.Base(path))
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return atomicfile.EnsureDirAndWriteJSONWithPerm(path, intent, 0o600)
}

// LoadLifecycleRetirementIntent loads the one exact active authority record.
func LoadLifecycleRetirementIntent(townRoot, attemptID string) (*LifecycleRetirementIntent, error) {
	path, err := lifecycleRetirementPath(townRoot, "sling-retirements", attemptID)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("loading lifecycle retirement intent: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: retirement intent %s is not a regular file", ErrLifecycleRequestRejected, filepath.Base(path))
	}
	data, err := os.ReadFile(path) //nolint:gosec // exact internal path validated above
	if err != nil {
		return nil, err
	}
	var intent LifecycleRetirementIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return nil, fmt.Errorf("%w: decoding lifecycle retirement intent: %v", ErrLifecycleRequestRejected, err)
	}
	if intent.AttemptID != attemptID {
		return nil, fmt.Errorf("%w: lifecycle retirement intent filename does not match its attempt", ErrLifecycleRequestRejected)
	}
	if err := validateLifecycleRetirementIntent(&intent); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLifecycleRequestRejected, err)
	}
	return &intent, nil
}

// LifecycleRetirementRequestBody returns the exact authenticated protocol body.
func LifecycleRetirementRequestBody(intent *LifecycleRetirementIntent) string {
	body := fmt.Sprintf("Reason: work_reassigned\nAttemptID: %s\nRequestedBy: %s\nBead: %s\nOldAssignee: %s\nOldIncarnation: %s\nNewAssignee: %s\nNewIncarnation: %s",
		intent.AttemptID, intent.Requester, intent.BeadID, intent.OldAssignee, intent.OldIncarnation, intent.NewAssignee, intent.NewIncarnation)
	if intent.Version != 0 {
		body = fmt.Sprintf("%s\nVersion: %d", body, intent.Version)
	}
	if intent.DeliveryID == "" {
		return body
	}
	authority, _ := json.Marshal(intent.WitnessAuthority)
	return fmt.Sprintf("%s\nDeliveryID: %s\nWitnessAuthority: %x", body, intent.DeliveryID, sha256.Sum256(authority))
}

// ValidateLifecycleRetirementRequest binds mail delivery to local authority.
func ValidateLifecycleRetirementRequest(intent *LifecycleRetirementIntent, rigName, polecatName string, msg *mail.Message) error {
	if err := validateLifecycleRetirementIntent(intent); err != nil {
		return fmt.Errorf("%w: %v", ErrLifecycleRequestRejected, err)
	}
	if msg == nil || msg.ID == "" || msg.Type != mail.TypeTask ||
		mail.AddressToIdentity(msg.From) != mail.AddressToIdentity("mayor/") ||
		mail.AddressToIdentity(msg.To) != mail.AddressToIdentity(rigName+"/witness") ||
		msg.Subject != "LIFECYCLE:Shutdown "+polecatName || msg.ThreadID != intent.ThreadID ||
		intent.OldAssignee != fmt.Sprintf("%s/polecats/%s", rigName, polecatName) ||
		msg.Body != LifecycleRetirementRequestBody(intent) {
		return fmt.Errorf("%w: lifecycle retirement mail does not match canonical authority", ErrLifecycleRequestRejected)
	}
	return nil
}

func validLifecycleRetirementReceiptAuthority(authority string) bool {
	return authority == lifecycleRetirementBrokerAuthority || authority == lifecycleRetirementLegacyBrokerAuthority
}

func validateLifecycleRetirementAppliedReceipt(intent *LifecycleRetirementIntent, receipt *LifecycleRetirementAppliedReceipt) error {
	if receipt == nil || !validLifecycleRetirementReceiptAuthority(receipt.Authority) || receipt.AttemptID != intent.AttemptID || receipt.DeliveryID != intent.DeliveryID || receipt.IntentHash != lifecycleRetirementIntentHash(intent) || receipt.RequestID == "" || (receipt.State != "pending" && receipt.State != "applied") {
		return fmt.Errorf("lifecycle retirement receipt does not match canonical authority")
	}
	parsed, err := uuid.Parse(receipt.ReceiptID)
	if err != nil || parsed.String() != receipt.ReceiptID {
		return fmt.Errorf("lifecycle retirement receipt has invalid ID")
	}
	expectedMAC := lifecycleRetirementReceiptMAC(intent, receipt)
	if expectedMAC == "" || !hmac.Equal([]byte(receipt.MAC), []byte(expectedMAC)) {
		return fmt.Errorf("lifecycle retirement receipt lacks broker capability authentication")
	}
	return nil
}

func validateLegacyLifecycleRetirementAppliedReceipt(intent *LifecycleRetirementIntent, receipt *LifecycleRetirementAppliedReceipt) error {
	if receipt == nil || intent.DeliveryID != "" || receipt.DeliveryID != "" || receipt.Authority != lifecycleRetirementLegacyBrokerAuthority || receipt.AttemptID != intent.AttemptID || receipt.IntentHash != lifecycleRetirementIntentHash(intent) || receipt.RequestID == "" || receipt.State != "" || receipt.MAC != "" {
		return fmt.Errorf("legacy lifecycle retirement receipt does not match predecessor authority")
	}
	parsed, err := uuid.Parse(receipt.ReceiptID)
	if err != nil || parsed.String() != receipt.ReceiptID {
		return fmt.Errorf("legacy lifecycle retirement receipt has invalid ID")
	}
	return nil
}

func lifecycleRetirementReceiptMAC(intent *LifecycleRetirementIntent, receipt *LifecycleRetirementAppliedReceipt) string {
	capability, err := uuid.Parse(intent.Capability)
	if err != nil || receipt == nil {
		return ""
	}
	payload, _ := json.Marshal(struct {
		AttemptID  string `json:"attempt_id"`
		DeliveryID string `json:"delivery_id,omitempty"`
		RequestID  string `json:"request_id"`
		ReceiptID  string `json:"receipt_id"`
		IntentHash string `json:"intent_hash"`
		Authority  string `json:"authority"`
		State      string `json:"state"`
	}{
		AttemptID: receipt.AttemptID, DeliveryID: receipt.DeliveryID, RequestID: receipt.RequestID, ReceiptID: receipt.ReceiptID,
		IntentHash: receipt.IntentHash, Authority: receipt.Authority, State: receipt.State,
	})
	mac := hmac.New(sha256.New, capability[:])
	_, _ = mac.Write(payload)
	return fmt.Sprintf("%x", mac.Sum(nil))
}

func lifecycleRetirementIntentHash(intent *LifecycleRetirementIntent) string {
	data, _ := json.Marshal(intent)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func lifecycleReassignmentBinding(intent *LifecycleRetirementIntent) beads.LifecycleReassignmentBinding {
	capabilityHash := sha256.Sum256([]byte(intent.Capability))
	binding := beads.LifecycleReassignmentBinding{
		Version:   intent.Version,
		AttemptID: intent.AttemptID, BeadID: intent.BeadID,
		OldAssignee: intent.OldAssignee, OldIncarnation: intent.OldIncarnation,
		NewAssignee: intent.NewAssignee, NewIncarnation: intent.NewIncarnation,
		Requester: intent.Requester, ThreadID: intent.ThreadID,
		CapabilityHash: fmt.Sprintf("%x", capabilityHash),
	}
	payload, _ := json.Marshal(binding)
	capability, err := uuid.Parse(intent.Capability)
	if err == nil {
		mac := hmac.New(sha256.New, capability[:])
		_, _ = mac.Write(payload)
		binding.AuthorityMAC = fmt.Sprintf("%x", mac.Sum(nil))
	}
	return binding
}

func lifecycleReassignmentBeads(townRoot string, intent *LifecycleRetirementIntent) *beads.Beads {
	beadsDir := beads.ResolveBeadsDirForID(filepath.Join(townRoot, ".beads"), intent.BeadID)
	return beads.NewWithBeadsDir(townRoot, beadsDir)
}

func lifecycleReassignmentDatabaseConfigured(townRoot string, intent *LifecycleRetirementIntent) bool {
	if intent == nil {
		return false
	}
	beadsDir := beads.ResolveBeadsDirForID(filepath.Join(townRoot, ".beads"), intent.BeadID)
	info, err := os.Stat(filepath.Join(beadsDir, "metadata.json"))
	return err == nil && info.Mode().IsRegular()
}

// PrepareLifecycleReassignmentDatabaseReceipt records prior ownership before
// sling changes the work assignment.
func PrepareLifecycleReassignmentDatabaseReceipt(townRoot string, intent *LifecycleRetirementIntent) error {
	if intent != nil && intent.Version == 0 {
		intent.Version = lifecycleRetirementIntentVersion
	}
	if err := validateLifecycleRetirementIntent(intent); err != nil {
		return err
	}
	return lifecycleReassignmentBeads(townRoot, intent).PrepareLifecycleReassignment(lifecycleReassignmentBinding(intent))
}

// BindLifecycleReassignmentDatabaseDelivery binds the exact portable intent
// and delivery only after the replacement owns the work.
func BindLifecycleReassignmentDatabaseDelivery(townRoot string, intent *LifecycleRetirementIntent) error {
	if err := validateLifecycleRetirementIntent(intent); err != nil {
		return err
	}
	return lifecycleReassignmentBeads(townRoot, intent).BindLifecycleReassignmentDelivery(
		lifecycleReassignmentBinding(intent), intent.DeliveryID, lifecycleRetirementIntentHash(intent),
	)
}

func validateLifecycleReassignmentDatabaseDelivery(townRoot string, intent *LifecycleRetirementIntent) error {
	return lifecycleReassignmentBeads(townRoot, intent).ValidateLifecycleReassignmentDelivery(
		lifecycleReassignmentBinding(intent), intent.DeliveryID, lifecycleRetirementIntentHash(intent),
	)
}

func markLifecycleReassignmentDatabaseApplied(townRoot string, intent *LifecycleRetirementIntent) error {
	return lifecycleReassignmentBeads(townRoot, intent).MarkLifecycleReassignmentApplied(
		lifecycleReassignmentBinding(intent), intent.DeliveryID, lifecycleRetirementIntentHash(intent),
	)
}

func lifecycleRetirementReceiptKey(intent *LifecycleRetirementIntent) string {
	if intent.DeliveryID != "" {
		return intent.DeliveryID
	}
	return intent.AttemptID
}

// LoadLifecycleRetirementReceipt returns nil when no action journal exists.
func LoadLifecycleRetirementReceipt(townRoot string, intent *LifecycleRetirementIntent) (*LifecycleRetirementAppliedReceipt, error) {
	path, err := lifecycleRetirementPath(townRoot, "sling-retirements-applied", lifecycleRetirementReceiptKey(intent))
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: retirement applied receipt %s is not a regular file", ErrLifecycleRequestRejected, filepath.Base(path))
	}
	data, err := os.ReadFile(path) //nolint:gosec // exact internal path validated above
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("%w: decoding lifecycle retirement applied receipt: %v", ErrLifecycleRequestRejected, err)
	}
	allowed := map[string]bool{
		"attempt_id": true, "delivery_id": true, "request_id": true, "receipt_id": true, "intent_hash": true,
		"authority": true, "state": true, "mac": true,
	}
	for field := range fields {
		if !allowed[field] {
			return nil, fmt.Errorf("%w: lifecycle retirement applied receipt has unsupported field %q", ErrLifecycleRequestRejected, field)
		}
	}
	var receipt LifecycleRetirementAppliedReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return nil, fmt.Errorf("%w: decoding lifecycle retirement applied receipt: %v", ErrLifecycleRequestRejected, err)
	}
	_, hasState := fields["state"]
	_, hasMAC := fields["mac"]
	if !hasState && !hasMAC {
		if err := validateLegacyLifecycleRetirementAppliedReceipt(intent, &receipt); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrLifecycleRequestRejected, err)
		}
		receipt.State = "pending"
		receipt.legacy = true
		return &receipt, nil
	}
	if !hasState || !hasMAC {
		return nil, fmt.Errorf("%w: lifecycle retirement applied receipt has an incomplete current schema", ErrLifecycleRequestRejected)
	}
	if err := validateLifecycleRetirementAppliedReceipt(intent, &receipt); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLifecycleRequestRejected, err)
	}
	return &receipt, nil
}

// LoadLifecycleRetirementAppliedReceipt returns nil until the journal records
// that the exact retirement effect was applied or independently reconciled.
func LoadLifecycleRetirementAppliedReceipt(townRoot string, intent *LifecycleRetirementIntent) (*LifecycleRetirementAppliedReceipt, error) {
	receipt, err := LoadLifecycleRetirementReceipt(townRoot, intent)
	if err != nil || receipt == nil || receipt.State != "applied" {
		return nil, err
	}
	return receipt, nil
}

func writeLifecycleRetirementReceipt(townRoot string, receipt *LifecycleRetirementAppliedReceipt) error {
	key := receipt.AttemptID
	if receipt.DeliveryID != "" {
		key = receipt.DeliveryID
	}
	path, err := lifecycleRetirementPath(townRoot, "sling-retirements-applied", key)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("retirement applied receipt %s is not a regular file", filepath.Base(path))
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return atomicfile.EnsureDirAndWriteJSONWithPerm(path, receipt, 0o600)
}

// PrepareLifecycleRetirementReceipt durably records the exact action intent
// before any session or agent-state mutation begins.
func PrepareLifecycleRetirementReceipt(townRoot string, intent *LifecycleRetirementIntent, requestID string) (*LifecycleRetirementAppliedReceipt, error) {
	return prepareLifecycleRetirementReceipt(townRoot, intent, requestID, false)
}

func prepareLifecycleRetirementReceipt(townRoot string, intent *LifecycleRetirementIntent, requestID string, brokered bool) (*LifecycleRetirementAppliedReceipt, error) {
	if !brokered {
		if err := requireLifecycleRetirementBrokerAuthority(); err != nil {
			return nil, err
		}
	}
	if requestID == "" {
		return nil, fmt.Errorf("lifecycle retirement request ID is empty")
	}
	if err := validateLifecycleRetirementIntent(intent); err != nil {
		return nil, err
	}
	if existing, err := LoadLifecycleRetirementReceipt(townRoot, intent); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.RequestID != requestID {
			return nil, fmt.Errorf("lifecycle retirement attempt was journaled for a different request")
		}
		return existing, nil
	}
	receipt := &LifecycleRetirementAppliedReceipt{
		AttemptID: intent.AttemptID, DeliveryID: intent.DeliveryID, RequestID: requestID, ReceiptID: uuid.NewString(),
		IntentHash: lifecycleRetirementIntentHash(intent), Authority: lifecycleRetirementBrokerAuthority, State: "pending",
	}
	receipt.MAC = lifecycleRetirementReceiptMAC(intent, receipt)
	if err := validateLifecycleRetirementAppliedReceipt(intent, receipt); err != nil {
		return nil, err
	}
	if err := writeLifecycleRetirementReceipt(townRoot, receipt); err != nil {
		return nil, err
	}
	return receipt, nil
}

// MarkLifecycleRetirementAppliedReceipt commits the terminal journal state only
// after the exact effect succeeds or its postcondition is independently seen.
func MarkLifecycleRetirementAppliedReceipt(townRoot string, intent *LifecycleRetirementIntent, requestID string) (*LifecycleRetirementAppliedReceipt, error) {
	return markLifecycleRetirementAppliedReceipt(townRoot, intent, requestID, false)
}

func markLifecycleRetirementAppliedReceipt(townRoot string, intent *LifecycleRetirementIntent, requestID string, brokered bool) (*LifecycleRetirementAppliedReceipt, error) {
	if !brokered {
		if err := requireLifecycleRetirementBrokerAuthority(); err != nil {
			return nil, err
		}
	}
	receipt, err := LoadLifecycleRetirementReceipt(townRoot, intent)
	if err != nil {
		return nil, err
	}
	if receipt == nil || receipt.RequestID != requestID {
		return nil, fmt.Errorf("lifecycle retirement has no matching pending receipt")
	}
	if receipt.State == "applied" {
		return receipt, nil
	}
	if receipt.legacy {
		receipt.Authority = lifecycleRetirementBrokerAuthority
		receipt.legacy = false
	}
	receipt.State = "applied"
	receipt.MAC = lifecycleRetirementReceiptMAC(intent, receipt)
	if err := validateLifecycleRetirementAppliedReceipt(intent, receipt); err != nil {
		return nil, err
	}
	if err := writeLifecycleRetirementReceipt(townRoot, receipt); err != nil {
		return nil, err
	}
	return receipt, nil
}

// StoreLifecycleRetirementAppliedReceipt durably journals one applied action.
func StoreLifecycleRetirementAppliedReceipt(townRoot string, intent *LifecycleRetirementIntent, requestID string) (*LifecycleRetirementAppliedReceipt, error) {
	if _, err := PrepareLifecycleRetirementReceipt(townRoot, intent, requestID); err != nil {
		return nil, err
	}
	return MarkLifecycleRetirementAppliedReceipt(townRoot, intent, requestID)
}

// LifecycleRetirementAcceptanceBody binds the ACK to the durable action receipt.
func LifecycleRetirementAcceptanceBody(intent *LifecycleRetirementIntent, receipt *LifecycleRetirementAppliedReceipt) string {
	return fmt.Sprintf("Result: accepted\nAttemptID: %s\nAppliedReceipt: %s\nBead: %s\nOldAssignee: %s\nOldIncarnation: %s\nNewAssignee: %s\nNewIncarnation: %s",
		intent.AttemptID, receipt.ReceiptID, intent.BeadID, intent.OldAssignee, intent.OldIncarnation, intent.NewAssignee, intent.NewIncarnation)
}
