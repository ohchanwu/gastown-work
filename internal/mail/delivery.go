package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

const (
	// DeliveryStatePending indicates a message has been durably written but not
	// yet acknowledged by a worker/recipient.
	DeliveryStatePending = "pending"
	// DeliveryStateAcked indicates receipt has been acknowledged.
	DeliveryStateAcked = "acked"

	// Label keys used for two-phase delivery tracking.
	DeliveryLabelPending       = "delivery:pending"
	DeliveryLabelAcked         = "delivery:acked"
	DeliveryLabelAckedByPrefix = "delivery-acked-by:"
	DeliveryLabelAckedAtPrefix = "delivery-acked-at:"
	WakeSourceLabelPrefix      = "wake-source:"
)

func validWakeSourceID(id string) bool {
	if !strings.HasPrefix(id, "msg-") || len(id) <= len("msg-") || len(id) > len("msg-")+32 {
		return false
	}
	for _, r := range strings.TrimPrefix(id, "msg-") {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func wakeSourceIDFromLabels(labels []string) (string, error) {
	var sourceID string
	for _, label := range labels {
		if !strings.HasPrefix(label, WakeSourceLabelPrefix) {
			continue
		}
		id := strings.TrimPrefix(label, WakeSourceLabelPrefix)
		if !validWakeSourceID(id) {
			return "", fmt.Errorf("invalid wake source label %q", label)
		}
		if sourceID != "" && sourceID != id {
			return "", fmt.Errorf("conflicting wake source labels")
		}
		sourceID = id
	}
	if sourceID == "" {
		return "", fmt.Errorf("message has no wake source label")
	}
	return sourceID, nil
}

func wakeSourceIDOrEmpty(labels []string) string {
	id, err := wakeSourceIDFromLabels(labels)
	if err != nil {
		return ""
	}
	return id
}

// WakeSourceForMessage returns the validated source identity stored on a
// durable mail message.
func WakeSourceForMessage(msg *Message) (string, error) {
	if msg == nil {
		return "", fmt.Errorf("source mail is nil")
	}
	if msg.WakeSourceID != "" {
		if !validWakeSourceID(msg.WakeSourceID) {
			return "", fmt.Errorf("invalid wake source ID %q", msg.WakeSourceID)
		}
		return msg.WakeSourceID, nil
	}
	return wakeSourceIDFromLabels(msg.Labels)
}

func ensureWakeSource(msg *Message) error {
	if msg.WakeSourceID == "" {
		msg.WakeSourceID = GenerateID()
	}
	if !validWakeSourceID(msg.WakeSourceID) {
		return fmt.Errorf("invalid wake source ID %q", msg.WakeSourceID)
	}
	return nil
}

// DeliverySendLabels returns labels written during phase-1 (send).
func DeliverySendLabels() []string {
	return []string{DeliveryLabelPending}
}

// DeliveryAckLabelSequence returns the full intended ack label sequence for
// phase-2 (ack), reusing an existing timestamp from existingLabels if one is
// present AND the recipient identity matches. This ensures retries produce
// the exact same label set instead of appending duplicate timestamps. If the
// recipient differs (e.g., after a claim-release-reclaim cycle), a fresh
// timestamp is generated.
//
// The label ordering is intentional for crash safety: state remains pending
// until the final delivery:acked label write succeeds.
//
// The scan is order-independent because bd show --json returns labels in
// lexicographic order, not insertion order. We collect all acked-by and
// acked-at values and only reuse a timestamp when this recipient is the
// sole acker (no mixed state from crash recovery).
//
// This returns the intended sequence; idempotent filtering against the
// labels already present is performed by deliveryAckLabelsToWrite.
func DeliveryAckLabelSequence(recipientIdentity string, at time.Time, existingLabels []string) []string {
	ts := at.UTC().Format(time.RFC3339)
	var recipients []string
	var timestamps []string
	for _, label := range existingLabels {
		if strings.HasPrefix(label, DeliveryLabelAckedByPrefix) {
			recipients = append(recipients, strings.TrimPrefix(label, DeliveryLabelAckedByPrefix))
		}
		if strings.HasPrefix(label, DeliveryLabelAckedAtPrefix) {
			timestamps = append(timestamps, strings.TrimPrefix(label, DeliveryLabelAckedAtPrefix))
		}
	}
	// Reuse existing timestamp only when this recipient is the sole acker
	// and exactly one timestamp exists. Multiple acked-by labels indicate
	// mixed state from crash recovery — use fresh timestamp to avoid
	// cross-recipient leakage.
	if len(recipients) == 1 && recipients[0] == recipientIdentity && len(timestamps) == 1 {
		ts = timestamps[0]
	}
	return []string{
		DeliveryLabelAckedByPrefix + recipientIdentity,
		DeliveryLabelAckedAtPrefix + ts,
		DeliveryLabelAcked,
	}
}

// AcknowledgeDeliveryBead writes phase-2 delivery ack labels for a bead and
// converges terminal state by removing delivery:pending after delivery:acked is
// durable. It is safe to call for already-acked messages and no-ops for beads
// without delivery labels.
//
// If beadID routes to a different rig than beadsDir, BEADS_DIR is stripped
// ("") so bd performs its own prefix-based routing via routes.jsonl. Pinning
// BEADS_DIR at a rig's .beads bypasses routing and fails the lookup for
// beads whose prefix maps to a different database on the shared Dolt
// server (au-ofe, au-b9d).
func AcknowledgeDeliveryBead(workDir, beadsDir, beadID, recipientIdentity string) error {
	ctx, cancel := bdWriteCtx()
	defer cancel()
	return AcknowledgeDeliveryBeadContext(ctx, workDir, beadsDir, beadID, recipientIdentity)
}

// AcknowledgeDeliveryBeadContext writes delivery acknowledgement labels with
// caller cancellation.
func AcknowledgeDeliveryBeadContext(ctx context.Context, workDir, beadsDir, beadID, recipientIdentity string) error {
	beadsDir = routedBeadsDirForID(beadsDir, beadID)
	existingLabels, readErr := readBeadLabelsSharedContext(ctx, workDir, beadsDir, beadID)
	if readErr != nil {
		return readErr
	}

	state, _, _ := ParseDeliveryLabels(existingLabels)
	if state == "" {
		return nil
	}

	toWrite := deliveryAckLabelsToWrite(recipientIdentity, timeNow().UTC(), existingLabels)
	for _, label := range toWrite {
		args := []string{"label", "add", beadID, label}
		_, err := runBdCommand(ctx, args, workDir, beadsDir)
		if err == nil {
			continue // bd label add silently succeeds on duplicate labels.
		}
		if bdErr, ok := err.(*bdError); ok && (bdErr.ContainsError("not found") || bdErr.ContainsError("no issue found")) {
			return ErrMessageNotFound
		}
		return err
	}

	labelsAfterAck := append(append([]string{}, existingLabels...), toWrite...)
	if deliveryPendingRemovalNeeded(labelsAfterAck) {
		return removeDeliveryPendingLabelContext(ctx, workDir, beadsDir, beadID)
	}
	return nil
}

func deliveryPendingRemovalNeeded(labels []string) bool {
	hasPending := false
	hasAcked := false
	for _, label := range labels {
		switch label {
		case DeliveryLabelPending:
			hasPending = true
		case DeliveryLabelAcked:
			hasAcked = true
		}
	}
	return hasPending && hasAcked
}

func removeDeliveryPendingLabel(workDir, beadsDir, beadID string) error {
	ctx, cancel := bdWriteCtx()
	defer cancel()
	return removeDeliveryPendingLabelContext(ctx, workDir, beadsDir, beadID)
}

func removeDeliveryPendingLabelContext(ctx context.Context, workDir, beadsDir, beadID string) error {
	args := []string{"label", "remove", beadID, DeliveryLabelPending}
	_, err := runBdCommand(ctx, args, workDir, beadsDir)
	if err == nil {
		return nil
	}
	if bdErr, ok := err.(*bdError); ok {
		switch {
		case bdErr.ContainsError("does not have label"):
			return nil
		case bdErr.ContainsError("not found") || bdErr.ContainsError("no issue found"):
			return ErrMessageNotFound
		}
	}
	return err
}

// deliveryAckLabelsToWrite returns the idempotent ack labels that are not
// already present. This avoids duplicate bd label-add calls, which otherwise
// attempt a Dolt commit and produce "nothing to commit" warnings.
func deliveryAckLabelsToWrite(recipientIdentity string, at time.Time, existingLabels []string) []string {
	sequence := DeliveryAckLabelSequence(recipientIdentity, at, existingLabels)
	if len(existingLabels) == 0 {
		return sequence
	}

	existing := make(map[string]struct{}, len(existingLabels))
	for _, label := range existingLabels {
		existing[label] = struct{}{}
	}

	missing := make([]string, 0, len(sequence))
	for _, label := range sequence {
		if _, ok := existing[label]; ok {
			continue
		}
		missing = append(missing, label)
	}
	return missing
}

// routedBeadsDirForID returns the BEADS_DIR value to pass into runBdCommand
// so that beadID resolves correctly. Same-rig ids keep currentBeadsDir;
// cross-rig ids return "" so bd's own prefix routing (via routes.jsonl in
// the town's .beads) dispatches to the correct database. See au-ofe, au-b9d.
func routedBeadsDirForID(currentBeadsDir, beadID string) string {
	target := beads.ResolveBeadsDirForID(currentBeadsDir, beadID)
	if target == "" || target == currentBeadsDir {
		return currentBeadsDir
	}
	return ""
}

// readBeadLabelsShared reads the labels for a bead, returning an error on failure
// instead of silently swallowing it.
func readBeadLabelsShared(workDir, beadsDir, id string) ([]string, error) {
	ctx, cancel := bdReadCtx()
	defer cancel()
	return readBeadLabelsSharedContext(ctx, workDir, beadsDir, id)
}

func readBeadLabelsSharedContext(ctx context.Context, workDir, beadsDir, id string) ([]string, error) {
	args := []string{"show", id, "--json"}
	stdout, err := runBdCommand(ctx, args, workDir, beadsDir)
	if err != nil {
		if bdErr, ok := err.(*bdError); ok && (bdErr.ContainsError("not found") || bdErr.ContainsError("no issue found")) {
			return nil, ErrMessageNotFound
		}
		return nil, fmt.Errorf("bd show %s: %w", id, err)
	}
	var bms []BeadsMessage
	if err := json.Unmarshal(stdout, &bms); err != nil {
		return nil, fmt.Errorf("parsing bd show %s: %w", id, err)
	}
	if len(bms) == 0 {
		return nil, nil
	}
	return bms[0].Labels, nil
}

// ParseDeliveryLabels derives delivery state and ack metadata from labels.
// The state is append-only:
// - `delivery:pending` means pending
// - once `delivery:acked` appears, state is acked (even if pending remains)
//
// Note: bd show --json returns labels in lexicographic order, so this parser
// must be order-independent. It uses last-wins for both acked-by and acked-at.
// For RFC3339 timestamps, lexicographic last-wins is chronologically correct.
func ParseDeliveryLabels(labels []string) (state, ackedBy string, ackedAt *time.Time) {
	hasPending := false
	hasAcked := false

	for _, label := range labels {
		switch {
		case label == DeliveryLabelPending:
			hasPending = true
		case label == DeliveryLabelAcked:
			hasAcked = true
		case strings.HasPrefix(label, DeliveryLabelAckedByPrefix):
			ackedBy = strings.TrimPrefix(label, DeliveryLabelAckedByPrefix)
		case strings.HasPrefix(label, DeliveryLabelAckedAtPrefix):
			ts := strings.TrimPrefix(label, DeliveryLabelAckedAtPrefix)
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				ackedAt = &t
			}
		}
	}

	if hasAcked {
		return DeliveryStateAcked, ackedBy, ackedAt
	}
	if hasPending {
		return DeliveryStatePending, "", nil
	}
	return "", "", nil
}
