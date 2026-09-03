// Package nudge provides non-destructive nudge delivery for Gas Town agents.
//
// The nudge queue allows messages to be delivered cooperatively: instead of
// sending text directly to a tmux session (which cancels in-flight tool calls),
// nudges are written to a queue directory and picked up by the agent's
// UserPromptSubmit hook at the next natural turn boundary.
//
// Queue location: <townRoot>/.runtime/nudge_queue/<session>/
// Each nudge is a JSON file named by timestamp for FIFO ordering.
package nudge

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/delivery"
)

// Priority levels for nudge delivery.
const (
	// PriorityNormal is the default — delivered at next turn boundary.
	PriorityNormal = "normal"
	// PriorityUrgent means the agent should handle this promptly.
	PriorityUrgent = "urgent"

	// SourceKindMail identifies a wake derived from one durable mail message.
	SourceKindMail = "mail"
)

// Operational limits and defaults.
// These are compiled-in fallbacks. Configurable via operational.nudge
// in settings/config.json (ZFC pattern).
const (
	// DefaultNormalTTL is the time-to-live for normal-priority nudges.
	DefaultNormalTTL = 30 * time.Minute

	// DefaultUrgentTTL is the time-to-live for urgent-priority nudges.
	DefaultUrgentTTL = 2 * time.Hour

	// MaxQueueDepth is the maximum number of pending nudges per session.
	MaxQueueDepth = 50

	// staleClaimThreshold is how long a .claimed file must be untouched
	// before Drain considers it orphaned (from a crashed drainer) and removes it.
	staleClaimThreshold = 5 * time.Minute
)

var readQueueRecord = os.ReadFile

// nudgeConfig loads nudge-specific thresholds from town settings.
func nudgeConfig(townRoot string) *config.NudgeThresholds {
	return config.LoadOperationalConfig(townRoot).GetNudgeConfig()
}

// QueuedNudge represents a nudge message stored in the queue.
type QueuedNudge struct {
	DeliveryID      string    `json:"delivery_id"`
	Session         string    `json:"session"`
	Sender          string    `json:"sender"`
	Message         string    `json:"message"`
	Priority        string    `json:"priority"`
	Kind            string    `json:"kind,omitempty"`
	ThreadID        string    `json:"thread_id,omitempty"`
	Severity        string    `json:"severity,omitempty"`
	SourceID        string    `json:"source_id,omitempty"`
	SourceKind      string    `json:"source_kind,omitempty"`
	Timestamp       time.Time `json:"timestamp"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
	Attempts        int       `json:"attempts"`
	ClaimedAt       time.Time `json:"claimed_at,omitempty"`
	NextAttempt     time.Time `json:"next_attempt_at,omitempty"`
	LastErrorCode   string    `json:"last_error_code,omitempty"`
	DurableUntilAck bool      `json:"durable_until_ack,omitempty"`
	// DeliverAfter, if non-zero, defers delivery until this time has passed.
	// Drain skips (but does not discard) the nudge until the deadline is met.
	DeliverAfter time.Time `json:"deliver_after,omitempty"`
}

type SubmissionReceipt = delivery.SubmissionReceipt

// ClaimedNudge owns one FIFO queue record until AckSubmitted or Nack.
type ClaimedNudge struct {
	Nudge     QueuedNudge
	queuePath string
	claimPath string
}

func lockQueueDir(dir string) (func(), error) {
	lockDir := filepath.Join(filepath.Dir(dir), ".locks")
	if err := os.MkdirAll(lockDir, 0700); err != nil {
		return nil, fmt.Errorf("creating nudge queue lock dir: %w", err)
	}
	if err := os.Chmod(lockDir, 0700); err != nil {
		return nil, fmt.Errorf("securing nudge queue lock dir: %w", err)
	}
	queueLock := flock.New(filepath.Join(lockDir, filepath.Base(dir)+".lock"))
	if err := queueLock.Lock(); err != nil {
		return nil, fmt.Errorf("locking nudge queue: %w", err)
	}
	return func() { _ = queueLock.Unlock() }, nil
}

// AckSubmitted removes a claim only when the receipt matches its owner and was
// produced after the claim baseline.
func (c *ClaimedNudge) AckSubmitted(receipt SubmissionReceipt) error {
	if !receipt.Submitted {
		return fmt.Errorf("submission receipt is not submitted")
	}
	if receipt.Runtime == "" {
		return fmt.Errorf("submission receipt has no runtime identity")
	}
	if receipt.Session != c.Nudge.Session || receipt.DeliveryID != c.Nudge.DeliveryID {
		return fmt.Errorf("submission receipt does not own delivery")
	}
	if !receipt.SubmittedAt.After(c.Nudge.ClaimedAt) {
		return fmt.Errorf("submission receipt is not newer than claim")
	}
	unlock, err := lockQueueDir(filepath.Dir(c.claimPath))
	if err != nil {
		return err
	}
	defer unlock()
	return os.Remove(c.claimPath)
}

// Nack records a sanitized failure and returns the delivery to its FIFO slot.
func (c *ClaimedNudge) Nack(errorCode string, nextAttempt time.Time) error {
	unlock, err := lockQueueDir(filepath.Dir(c.claimPath))
	if err != nil {
		return err
	}
	defer unlock()
	data, err := readQueueRecord(c.claimPath)
	if err != nil {
		return errors.New("reading claimed nudge before retry failed")
	}
	var stored QueuedNudge
	if err := json.Unmarshal(data, &stored); err != nil || stored.DeliveryID != c.Nudge.DeliveryID {
		return errors.New("claimed nudge changed before retry")
	}
	if stored.DurableUntilAck {
		c.Nudge.DurableUntilAck = true
		c.Nudge.ExpiresAt = time.Time{}
	}
	c.Nudge.ClaimedAt = time.Time{}
	c.Nudge.NextAttempt = nextAttempt
	c.Nudge.LastErrorCode = sanitizeErrorCode(errorCode)
	if err := writeQueueRecord(c.claimPath, c.Nudge); err != nil {
		return err
	}
	return os.Rename(c.claimPath, c.queuePath)
}

// DiscardTerminal removes only the queue claim owned by this consumer. Broad
// thread cleanup deliberately ignores claims so a concurrent owner cannot be
// raced into data loss.
func (c *ClaimedNudge) DiscardTerminal() error {
	unlock, err := lockQueueDir(filepath.Dir(c.claimPath))
	if err != nil {
		return err
	}
	defer unlock()
	return os.Remove(c.claimPath)
}

// HasRecoverableState proves the delivery still exists either in its in-flight
// claim or restored FIFO slot. Callers use this before releasing external
// delivery custody after a filesystem error.
func (c *ClaimedNudge) HasRecoverableState() bool {
	for _, path := range []string{c.claimPath, c.queuePath} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			continue
		}
		data, err := readQueueRecord(path)
		if err != nil {
			continue
		}
		var queued QueuedNudge
		if err := json.Unmarshal(data, &queued); err == nil && queued.DeliveryID == c.Nudge.DeliveryID && (queued.Session == "" || queued.Session == c.Nudge.Session) {
			return true
		}
	}
	return false
}

func sanitizeErrorCode(value string) string {
	value = strings.ToLower(value)
	var b strings.Builder
	dash := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' {
			b.WriteRune(r)
			dash = false
		} else if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 64 {
			break
		}
	}
	result := strings.Trim(b.String(), "-")
	if result == "" {
		return "unknown"
	}
	return result
}

// queueDir returns the nudge queue directory for a given session.
// Path: <townRoot>/.runtime/nudge_queue/<session>/
func queueDir(townRoot, session string) string {
	// Sanitize session name for filesystem safety
	safe := strings.ReplaceAll(session, "/", "_")
	return filepath.Join(townRoot, constants.DirRuntime, "nudge_queue", safe)
}

// randomSuffix returns a short random hex string to disambiguate filenames
// when multiple processes enqueue within the same nanosecond.
func randomSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// NewDeliveryID returns an opaque identifier suitable for queue ownership.
func NewDeliveryID() string { return "ndg-" + randomSuffix() }

// NextRetry returns a bounded linear retry time for a failed attempt.
func NextRetry(attempt int) time.Time {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 60 {
		attempt = 60
	}
	return time.Now().Add(time.Duration(attempt) * time.Second)
}

func writeQueueRecord(path string, n QueuedNudge) error {
	data, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling nudge: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".nudge-*.tmp")
	if err != nil {
		return fmt.Errorf("creating nudge queue record: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("securing nudge queue record: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing nudge queue record: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing nudge queue record: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing nudge queue record: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publishing nudge queue record: %w", err)
	}
	return nil
}

// Enqueue writes a nudge to the queue for the given session.
// The nudge will be picked up by the agent's hook at the next turn boundary.
// Returns an error if the queue is full (MaxQueueDepth reached).
func Enqueue(townRoot, session string, nudge QueuedNudge) error {
	dir := queueDir(townRoot, session)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating nudge queue dir: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return fmt.Errorf("securing nudge queue dir: %w", err)
	}

	// Check queue depth before writing to prevent runaway senders.
	maxDepth := nudgeConfig(townRoot).MaxQueueDepthV()
	pending, _ := Pending(townRoot, session)
	if pending >= maxDepth {
		return fmt.Errorf("nudge queue for %s is full (%d/%d pending)", session, pending, maxDepth)
	}

	if nudge.Timestamp.IsZero() {
		nudge.Timestamp = time.Now()
	}
	if nudge.DeliveryID == "" {
		nudge.DeliveryID = NewDeliveryID()
	}
	nudge.Session = session
	if nudge.Priority == "" {
		nudge.Priority = PriorityNormal
	}

	// Set expiry if not already specified by the caller.
	if nudge.ExpiresAt.IsZero() && !nudge.DurableUntilAck {
		switch nudge.Priority {
		case PriorityUrgent:
			nudge.ExpiresAt = nudge.Timestamp.Add(DefaultUrgentTTL)
		default:
			nudge.ExpiresAt = nudge.Timestamp.Add(DefaultNormalTTL)
		}
	}

	// Use nanosecond timestamp + random suffix for unique, ordered filenames.
	// The random suffix prevents collisions when multiple agents enqueue
	// nudges for the same session within the same nanosecond.
	filename := fmt.Sprintf("%d-%s.json", nudge.Timestamp.UnixNano(), randomSuffix())
	path := filepath.Join(dir, filename)

	return writeQueueRecord(path, nudge)
}

// EnqueueUniqueBySource writes a nudge unless the same kind, thread, and
// durable source is already queued or claimed for this session.
func EnqueueUniqueBySource(townRoot, session string, n QueuedNudge) (bool, error) {
	if n.Kind == "" || n.ThreadID == "" || n.SourceID == "" {
		return false, errors.New("unique nudge requires kind, thread, and source")
	}
	dir := queueDir(townRoot, session)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return false, fmt.Errorf("creating nudge queue dir: %w", err)
	}
	unlock, err := lockQueueDir(dir)
	if err != nil {
		return false, err
	}
	defer unlock()

	records, err := listQueuedRecords(dir)
	if err != nil {
		return false, err
	}
	for _, record := range records {
		existing := record.nudge
		if existing.Kind == n.Kind && existing.ThreadID == n.ThreadID && existing.SourceID == n.SourceID {
			if n.DurableUntilAck && (!existing.DurableUntilAck || !existing.ExpiresAt.IsZero()) {
				existing.DurableUntilAck = true
				existing.ExpiresAt = time.Time{}
				if err := writeQueueRecord(record.path, existing); err != nil {
					return false, fmt.Errorf("upgrading matching nudge durability: %w", err)
				}
			}
			return false, nil
		}
	}
	if err := Enqueue(townRoot, session, n); err != nil {
		return false, err
	}
	return true, nil
}

// Requeue writes previously drained nudges back to the queue for later delivery.
// Existing timestamps are preserved so FIFO ordering remains stable relative to
// one another; only expired nudges are skipped.
func Requeue(townRoot, session string, nudges []QueuedNudge) error {
	for _, n := range nudges {
		if !n.ExpiresAt.IsZero() && time.Now().After(n.ExpiresAt) {
			continue
		}
		if err := Enqueue(townRoot, session, n); err != nil {
			return err
		}
	}
	return nil
}

// ClaimDue atomically reserves the oldest due nudge for a session.
// Callers must AckSubmitted only after runtime proof, or Nack on failure.
//
// Uses rename-then-process to prevent concurrent Drain calls from delivering
// the same nudge twice: each file is atomically renamed to a .claimed suffix
// before reading, so only one caller can claim each nudge.
//
// Expired nudges (past ExpiresAt) are silently discarded during drain.
// Orphaned .claimed files from crashed drainers are swept if older than 5 minutes.
func ClaimDue(townRoot, session string) (*ClaimedNudge, error) {
	dir := queueDir(townRoot, session)

	_, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading nudge queue: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, fmt.Errorf("securing nudge queue: %w", err)
	}
	unlock, err := lockQueueDir(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading nudge queue: %w", err)
	}

	// Requeue orphaned .claimed files from crashed drainers.
	// A .claimed file older than staleClaimThreshold is certainly orphaned —
	// normal processing completes in milliseconds. We rename it back to .json
	// so it gets picked up on this or a future Drain call, rather than deleting
	// it (which would permanently drop the nudge).
	staleThreshold := nudgeConfig(townRoot).StaleClaimThresholdD()
	now := time.Now()
	for _, entry := range entries {
		if !strings.Contains(entry.Name(), ".claimed") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > staleThreshold {
			orphanPath := filepath.Join(dir, entry.Name())
			// Strip everything from ".claimed" onward to restore original .json filename
			name := entry.Name()
			claimedIdx := strings.Index(name, ".claimed")
			restoredPath := filepath.Join(dir, name[:claimedIdx])
			if err := os.Rename(orphanPath, restoredPath); err != nil {
				// Leave the claim in place on failure. A later recovery pass can
				// retry; deleting here would turn a filesystem error into data loss.
				fmt.Fprintf(os.Stderr, "Warning: failed to requeue orphaned claim %s: %v\n", entry.Name(), err)
			}
		}
	}

	// Sort by name (timestamp-based) for FIFO ordering
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		path := filepath.Join(dir, entry.Name())

		// Atomically claim the file by renaming it. If another Drain call
		// is racing us, only one rename will succeed — the loser gets
		// ENOENT and moves on. This prevents double-delivery.
		//
		// Each drainer uses a unique claim suffix to avoid destination
		// collisions. On Windows, os.Rename to a shared destination is
		// not atomic — two goroutines can both "succeed" via
		// MOVEFILE_REPLACE_EXISTING, causing data loss. Unique suffixes
		// ensure each rename has a distinct target.
		claimPath := path + ".claimed." + randomSuffix()
		if err := os.Rename(path, claimPath); err != nil {
			// Another Drain got it first, or file was already removed
			continue
		}

		data, err := os.ReadFile(claimPath)
		if err != nil {
			if os.IsNotExist(err) {
				// File vanished between rename and read — treat as lost race
				continue
			}
			// Transient read error (e.g., Windows AV/indexer holding a share
			// lock) — unclaim so the nudge can be retried on a future Drain
			// call rather than permanently lost.
			_ = os.Rename(claimPath, path) // best-effort unclaim; orphan sweep catches failures
			continue
		}

		var n QueuedNudge
		if err := json.Unmarshal(data, &n); err != nil {
			_ = os.Chmod(claimPath, 0600)
			quarantinePath := path + ".malformed." + randomSuffix()
			_ = os.Rename(claimPath, quarantinePath)
			return nil, errors.New("malformed nudge queue record preserved")
		}

		// Skip expired nudges — stale messages create noise, not value.
		if !n.ExpiresAt.IsZero() && now.After(n.ExpiresAt) {
			if rmErr := os.Remove(claimPath); rmErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to remove expired nudge %s: %v\n", entry.Name(), rmErr)
			}
			continue
		}
		if n.Session == "" {
			n.Session = session
		}
		if n.Session != session {
			_ = os.Rename(claimPath, path)
			return nil, fmt.Errorf("delivery %q belongs to session %q, not %q", n.DeliveryID, n.Session, session)
		}
		if n.DeliveryID == "" {
			n.DeliveryID = NewDeliveryID()
		}

		// Deferred nudge: not ready yet — unclaim and leave in queue.
		dueAt := n.DeliverAfter
		if n.NextAttempt.After(dueAt) {
			dueAt = n.NextAttempt
		}
		if !dueAt.IsZero() && now.Before(dueAt) {
			if renameErr := os.Rename(claimPath, path); renameErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to unclaim deferred nudge %s: %v\n", entry.Name(), renameErr)
			}
			continue
		}

		n.Attempts++
		n.ClaimedAt = now
		if err := writeQueueRecord(claimPath, n); err != nil {
			_ = os.Rename(claimPath, path)
			return nil, err
		}
		return &ClaimedNudge{Nudge: n, queuePath: path, claimPath: claimPath}, nil
	}

	return nil, nil
}

// Drain preserves the original consume-on-read API for hook and inspection
// callers. Runtime delivery paths should use Claim and acknowledge explicitly.
func Drain(townRoot, session string) ([]QueuedNudge, error) {
	limit, err := Pending(townRoot, session)
	if err != nil {
		return nil, err
	}
	var nudges []QueuedNudge
	for range limit {
		claim, err := ClaimDue(townRoot, session)
		if err != nil {
			return nil, err
		}
		if claim == nil {
			return nudges, nil
		}
		receipt := SubmissionReceipt{Session: session, DeliveryID: claim.Nudge.DeliveryID, Runtime: "legacy", Submitted: true, SubmittedAt: time.Now()}
		if err := claim.AckSubmitted(receipt); err != nil {
			return nil, err
		}
		nudges = append(nudges, claim.Nudge)
	}
	return nudges, nil
}

// Pending returns the count of queued nudges for a session without draining.
// This is an approximate count — it does not check expiry or read file contents.
func Pending(townRoot, session string) (int, error) {
	dir := queueDir(townRoot, session)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading nudge queue: %w", err)
	}

	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}

	return count, nil
}

// HasQueuedOrClaimed reports whether a poll cycle has FIFO work or in-flight
// state that may need orphan recovery. Unlike Pending, it deliberately includes
// unique .json.claimed.* records.
func HasQueuedOrClaimed(townRoot, session string) (bool, error) {
	dir := queueDir(townRoot, session)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading nudge queue: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && (strings.HasSuffix(name, ".json") || strings.Contains(name, ".json.claimed.")) {
			return true, nil
		}
	}
	return false, nil
}

type queuedRecord struct {
	path  string
	nudge QueuedNudge
}

func listQueuedRecords(dir string) ([]queuedRecord, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading nudge queue: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	records := make([]queuedRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || (!strings.HasSuffix(entry.Name(), ".json") && !strings.Contains(entry.Name(), ".json.claimed.")) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := readQueueRecord(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, errors.New("reading nudge queue record failed")
		}
		var n QueuedNudge
		if err := json.Unmarshal(data, &n); err != nil {
			return nil, errors.New("malformed nudge queue record preserved")
		}
		records = append(records, queuedRecord{path: path, nudge: n})
	}
	return records, nil
}

// ListQueued returns a read-only snapshot of queued and in-flight deliveries.
func ListQueued(townRoot, session string) ([]QueuedNudge, error) {
	records, err := listQueuedRecords(queueDir(townRoot, session))
	if err != nil {
		return nil, err
	}
	queued := make([]QueuedNudge, 0, len(records))
	for _, record := range records {
		queued = append(queued, record.nudge)
	}
	return queued, nil
}

// QueueLen returns the number of pending nudges for a session without draining.
// Returns 0 on error — callers use this for quick checks. Missing queue
// directories are expected (no nudges yet) and silenced; other filesystem
// errors are logged to stderr so they don't go unnoticed.
func QueueLen(townRoot, session string) int {
	n, err := Pending(townRoot, session)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: nudge queue check failed for %s: %v\n", session, err)
	}
	return n
}

// RemoveKindByThread deletes queued nudges for a session that match both the
// provided kind and thread ID. It only removes queued .json files, leaving any
// in-flight claimed files alone so concurrent drainers can finish safely.
func RemoveKindByThread(townRoot, session, kind, threadID string) (int, error) {
	if kind == "" || threadID == "" {
		return 0, nil
	}

	dir := queueDir(townRoot, session)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading nudge queue: %w", err)
	}

	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, fmt.Errorf("reading queued nudge %s: %w", entry.Name(), err)
		}

		var n QueuedNudge
		if err := json.Unmarshal(data, &n); err != nil {
			continue
		}
		if n.Kind != kind || n.ThreadID != threadID {
			continue
		}

		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, fmt.Errorf("removing queued nudge %s: %w", entry.Name(), err)
		}
		removed++
	}

	return removed, nil
}

// FormatForInjection formats queued nudges as a system-reminder block
// suitable for Claude Code hook output.
func FormatForInjection(nudges []QueuedNudge) string {
	if len(nudges) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("<system-reminder>\n")

	// Separate urgent from normal
	var urgent, normal []QueuedNudge
	for _, n := range nudges {
		if n.Priority == PriorityUrgent {
			urgent = append(urgent, n)
		} else {
			normal = append(normal, n)
		}
	}

	if len(urgent) > 0 {
		b.WriteString(fmt.Sprintf("QUEUED NUDGE (%d urgent):\n\n", len(urgent)))
		for _, n := range urgent {
			b.WriteString(fmt.Sprintf("  [URGENT from %s] %s\n", n.Sender, n.Message))
		}
		if len(normal) > 0 {
			b.WriteString(fmt.Sprintf("\nPlus %d non-urgent nudge(s):\n", len(normal)))
			for _, n := range normal {
				b.WriteString(fmt.Sprintf("  [from %s] %s\n", n.Sender, n.Message))
			}
		}
		b.WriteString("\nHandle urgent nudges before continuing current work.\n")
	} else {
		b.WriteString(fmt.Sprintf("QUEUED NUDGE (%d message(s)):\n\n", len(normal)))
		for _, n := range normal {
			b.WriteString(fmt.Sprintf("  [from %s] %s\n", n.Sender, n.Message))
		}
		b.WriteString("\nThis is a background notification. Continue current work unless the nudge is higher priority.\n")
	}

	b.WriteString("</system-reminder>\n")
	return b.String()
}
