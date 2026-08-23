package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestMailWorkStoreClaimDirectAndQueue(t *testing.T) {
	tests := []struct {
		name         string
		route        WorkRoute
		queueAllowed bool
		wantAssignee string
	}{
		{name: "direct", route: WorkRouteDirect, wantAssignee: "gastown/Toast"},
		{name: "queue", route: WorkRouteQueue, queueAllowed: true, wantAssignee: "gastown/Toast"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeMailWorkStore(t, testMailWorkIssue(t, tt.route))
			workStore := NewMailWorkStore(store)
			workStore.now = func() time.Time { return time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC) }
			eligible := func(queue, actor string) bool {
				return tt.queueAllowed && queue == "repairs" && actor == "gastown/Toast"
			}

			work, err := workStore.Claim(context.Background(), "hq-task", "gastown/Toast", testWorkGeneration().Tmux(), eligible)
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			if err := work.Validate(WorkStateInProgress); err != nil {
				t.Fatalf("claimed metadata: %v", err)
			}

			issue := store.snapshot()
			if issue.Status != beadsdk.StatusInProgress || issue.Assignee != tt.wantAssignee {
				t.Fatalf("claimed issue status/assignee = %s/%q", issue.Status, issue.Assignee)
			}
			assertLabelCount(t, issue.Labels, "claimed-by:gastown/Toast", 1)
			assertLabelCount(t, issue.Labels, "claimed-at:2026-08-24T08:00:00Z", 1)
		})
	}
}

func TestMailWorkStoreClaimIsIdempotentForExactGeneration(t *testing.T) {
	store := newFakeMailWorkStore(t, testMailWorkIssue(t, WorkRouteDirect))
	workStore := NewMailWorkStore(store)
	firstTime := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	workStore.now = func() time.Time { return firstTime }
	generation := testWorkGeneration().Tmux()

	first, err := workStore.Claim(context.Background(), "hq-task", "gastown/Toast", generation, nil)
	if err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	workStore.now = func() time.Time { return firstTime.Add(time.Hour) }
	second, err := workStore.Claim(context.Background(), "hq-task", "gastown/Toast", generation, nil)
	if err != nil {
		t.Fatalf("retry Claim: %v", err)
	}
	if !second.Claim.ClaimedAt.Equal(first.Claim.ClaimedAt) || len(store.comments) != 1 {
		t.Fatalf("retry changed claim: first=%+v second=%+v comments=%v", first.Claim, second.Claim, store.comments)
	}
}

func TestMailWorkStoreClaimRejectsContenders(t *testing.T) {
	store := newFakeMailWorkStore(t, testMailWorkIssue(t, WorkRouteDirect))
	workStore := NewMailWorkStore(store)
	firstGeneration := testWorkGeneration().Tmux()
	secondGeneration := firstGeneration
	secondGeneration.Nonce = "replacement-generation"

	if _, err := workStore.Claim(context.Background(), "hq-task", "gastown/Toast", firstGeneration, nil); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	if _, err := workStore.Claim(context.Background(), "hq-task", "gastown/Toast", secondGeneration, nil); !errors.Is(err, ErrMailWorkConflict) {
		t.Fatalf("replacement-generation Claim error = %v, want conflict", err)
	}
	if _, err := workStore.Claim(context.Background(), "hq-task", "gastown/Other", secondGeneration, nil); !errors.Is(err, ErrMailWorkConflict) {
		t.Fatalf("other-agent Claim error = %v, want conflict", err)
	}
}

func TestMailWorkStoreConcurrentClaimHasOneWinner(t *testing.T) {
	store := newFakeMailWorkStore(t, testMailWorkIssue(t, WorkRouteQueue))
	workStore := NewMailWorkStore(store)
	base := testWorkGeneration().Tmux()

	type result struct {
		actor string
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for i, actor := range []string{"gastown/Toast", "gastown/Synth"} {
		generation := base
		generation.Name = "gt-" + actor
		generation.SessionID = fmt.Sprintf("$%d", i+20)
		generation.Nonce = fmt.Sprintf("generation-%d", i)
		go func(actor string, generation tmux.SessionGeneration) {
			<-start
			_, err := workStore.Claim(context.Background(), "hq-task", actor, generation, func(queue, candidate string) bool {
				return queue == "repairs" && candidate == actor
			})
			results <- result{actor: actor, err: err}
		}(actor, generation)
	}
	close(start)

	wins, conflicts := 0, 0
	for range 2 {
		result := <-results
		switch {
		case result.err == nil:
			wins++
		case errors.Is(result.err, ErrMailWorkConflict):
			conflicts++
		default:
			t.Fatalf("claim error = %v", result.err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins/conflicts = %d/%d, want 1/1", wins, conflicts)
	}
}

func TestMailWorkStoreReleaseBlockAndResume(t *testing.T) {
	store := newFakeMailWorkStore(t, testMailWorkIssue(t, WorkRouteQueue))
	workStore := NewMailWorkStore(store)
	generation := testWorkGeneration().Tmux()
	eligible := func(queue, actor string) bool { return queue == "repairs" && actor == "gastown/Toast" }

	if _, err := workStore.Claim(context.Background(), "hq-task", "gastown/Toast", generation, eligible); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := workStore.Block(context.Background(), "hq-task", "gastown/Toast", generation, "waiting for review"); err != nil {
		t.Fatalf("Block: %v", err)
	}
	if issue := store.snapshot(); issue.Status != beadsdk.StatusBlocked {
		t.Fatalf("blocked status = %s", issue.Status)
	}
	if _, err := workStore.Resume(context.Background(), "hq-task", "gastown/Toast", generation); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if _, err := workStore.Release(context.Background(), "hq-task", "gastown/Toast", generation); err != nil {
		t.Fatalf("Release: %v", err)
	}

	issue := store.snapshot()
	if issue.Status != beadsdk.StatusOpen || issue.Assignee != "queue:repairs" {
		t.Fatalf("released status/assignee = %s/%q", issue.Status, issue.Assignee)
	}
	for _, label := range issue.Labels {
		if len(label) >= len("claimed-") && label[:len("claimed-")] == "claimed-" {
			t.Fatalf("release retained compatibility label %q", label)
		}
	}
	work, err := ParseMailWorkMetadata(issue.Metadata)
	if err != nil || work.Validate(WorkStateOpen) != nil {
		t.Fatalf("released metadata = %+v, error = %v", work, err)
	}
}

func TestMailWorkStoreTransitionFailureRollsBack(t *testing.T) {
	original := testMailWorkIssue(t, WorkRouteDirect)
	store := newFakeMailWorkStore(t, original)
	store.failComment = errors.New("event write failed")
	workStore := NewMailWorkStore(store)

	if _, err := workStore.Claim(context.Background(), "hq-task", "gastown/Toast", testWorkGeneration().Tmux(), nil); err == nil {
		t.Fatal("Claim succeeded, want transaction failure")
	}
	issue := store.snapshot()
	if issue.Status != beadsdk.StatusOpen || issue.Assignee != original.Assignee || string(issue.Metadata) != string(original.Metadata) {
		t.Fatalf("failed transaction mutated issue: %+v", issue)
	}
	assertLabelCount(t, issue.Labels, "claimed-by:gastown/Toast", 0)
}

func TestMailWorkStoreDoltConcurrentClaim(t *testing.T) {
	if os.Getenv("GT_TEST_ISOLATED") != "1" {
		t.Skip("requires the repository isolated Dolt test launcher")
	}
	store, cleanup := openMailWorkTestStore(t)
	defer cleanup()

	issue := testMailWorkIssue(t, WorkRouteQueue)
	issue.ID = "test-mail-work"
	issue.UpdatedAt = issue.CreatedAt
	labels := append([]string(nil), issue.Labels...)
	if err := store.CreateIssue(context.Background(), issue, "mayor/"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	for _, label := range labels {
		if err := store.AddLabel(context.Background(), issue.ID, label, "mayor/"); err != nil {
			t.Fatalf("AddLabel(%s): %v", label, err)
		}
	}
	created, err := store.GetIssue(context.Background(), issue.ID)
	if err != nil {
		t.Fatalf("GetIssue after create: %v", err)
	}
	if message := sdkIssueToMessage(created); !message.IsActionableWork() {
		t.Fatalf("created issue is not actionable: status=%s type=%s labels=%v ephemeral=%v assignee=%s", created.Status, message.Type, created.Labels, created.Ephemeral, created.Assignee)
	}
	workStore := NewMailWorkStore(store)
	base := testWorkGeneration().Tmux()

	errs := make(chan error, 2)
	for i, actor := range []string{"gastown/Toast", "gastown/Synth"} {
		generation := base
		generation.Name = "gt-" + actor
		generation.SessionID = fmt.Sprintf("$%d", i+30)
		generation.Nonce = fmt.Sprintf("dolt-generation-%d", i)
		go func(actor string, generation tmux.SessionGeneration) {
			_, err := workStore.Claim(context.Background(), issue.ID, actor, generation, func(queue, candidate string) bool {
				return queue == "repairs" && candidate == actor
			})
			errs <- err
		}(actor, generation)
	}

	wins, conflicts := 0, 0
	for range 2 {
		err := <-errs
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrMailWorkConflict):
			conflicts++
		default:
			t.Fatalf("claim error = %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins/conflicts = %d/%d, want 1/1", wins, conflicts)
	}

	stored, err := store.GetIssue(context.Background(), issue.ID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	work, err := ParseMailWorkMetadata(stored.Metadata)
	if err != nil || work.Validate(WorkStateInProgress) != nil {
		t.Fatalf("stored work = %+v, parse error = %v", work, err)
	}
}

func openMailWorkTestStore(t *testing.T) (beadsdk.Storage, func()) {
	t.Helper()
	t.Setenv("BEADS_TEST_MODE", "1")
	doltPath := filepath.Join(t.TempDir(), ".beads", "dolt")
	if err := os.MkdirAll(doltPath, 0o755); err != nil {
		t.Fatalf("create test store: %v", err)
	}
	store, err := beadsdk.Open(context.Background(), doltPath)
	if err != nil {
		t.Fatalf("open beads store: %v", err)
	}
	if err := store.SetConfig(context.Background(), "issue_prefix", "test"); err != nil {
		_ = store.Close()
		t.Fatalf("set issue prefix: %v", err)
	}
	return store, func() { _ = store.Close() }
}

type fakeMailWorkStore struct {
	beadsdk.Storage
	mu          sync.Mutex
	issue       *beadsdk.Issue
	comments    []string
	failComment error
}

func newFakeMailWorkStore(t *testing.T, issue *beadsdk.Issue) *fakeMailWorkStore {
	t.Helper()
	return &fakeMailWorkStore{issue: cloneWorkIssue(t, issue)}
}

func (s *fakeMailWorkStore) RunInTransaction(ctx context.Context, _ string, fn func(beadsdk.Transaction) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := &fakeMailWorkTransaction{
		issue:       cloneWorkIssueNoTest(s.issue),
		comments:    append([]string(nil), s.comments...),
		failComment: s.failComment,
	}
	if err := fn(tx); err != nil {
		return err
	}
	s.issue = tx.issue
	s.comments = tx.comments
	return nil
}

func (s *fakeMailWorkStore) snapshot() *beadsdk.Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneWorkIssueNoTest(s.issue)
}

type fakeMailWorkTransaction struct {
	beadsdk.Transaction
	issue       *beadsdk.Issue
	comments    []string
	failComment error
}

func (tx *fakeMailWorkTransaction) GetIssue(_ context.Context, id string) (*beadsdk.Issue, error) {
	if tx.issue == nil || tx.issue.ID != id {
		return nil, ErrMessageNotFound
	}
	return cloneWorkIssueNoTest(tx.issue), nil
}

func (tx *fakeMailWorkTransaction) UpdateIssue(_ context.Context, id string, updates map[string]interface{}, _ string) error {
	if tx.issue == nil || tx.issue.ID != id {
		return ErrMessageNotFound
	}
	for key, value := range updates {
		switch key {
		case "status":
			tx.issue.Status = beadsdk.Status(value.(string))
		case "assignee":
			tx.issue.Assignee = value.(string)
		case "metadata":
			tx.issue.Metadata = append(json.RawMessage(nil), value.(json.RawMessage)...)
		default:
			return fmt.Errorf("unexpected update %q", key)
		}
	}
	return nil
}

func (tx *fakeMailWorkTransaction) GetLabels(_ context.Context, id string) ([]string, error) {
	if tx.issue == nil || tx.issue.ID != id {
		return nil, ErrMessageNotFound
	}
	return append([]string(nil), tx.issue.Labels...), nil
}

func (tx *fakeMailWorkTransaction) AddLabel(_ context.Context, id, label, _ string) error {
	if tx.issue == nil || tx.issue.ID != id {
		return ErrMessageNotFound
	}
	for _, existing := range tx.issue.Labels {
		if existing == label {
			return nil
		}
	}
	tx.issue.Labels = append(tx.issue.Labels, label)
	return nil
}

func (tx *fakeMailWorkTransaction) RemoveLabel(_ context.Context, id, label, _ string) error {
	if tx.issue == nil || tx.issue.ID != id {
		return ErrMessageNotFound
	}
	filtered := tx.issue.Labels[:0]
	for _, existing := range tx.issue.Labels {
		if existing != label {
			filtered = append(filtered, existing)
		}
	}
	tx.issue.Labels = filtered
	return nil
}

func (tx *fakeMailWorkTransaction) AddComment(_ context.Context, id, actor, comment string) error {
	if tx.issue == nil || tx.issue.ID != id {
		return ErrMessageNotFound
	}
	if tx.failComment != nil {
		return tx.failComment
	}
	tx.comments = append(tx.comments, actor+": "+comment)
	return nil
}

func testMailWorkIssue(t *testing.T, route WorkRoute) *beadsdk.Issue {
	t.Helper()
	work := &WorkMetadata{Schema: MailWorkSchema, Route: route}
	metadata, err := EncodeMailWorkMetadata(nil, work)
	if err != nil {
		t.Fatalf("EncodeMailWorkMetadata: %v", err)
	}
	issue := &beadsdk.Issue{
		ID:          "hq-task",
		Title:       "Repair the system",
		Description: "Use the approved plan",
		Status:      beadsdk.StatusOpen,
		Priority:    2,
		IssueType:   beadsdk.TypeTask,
		Assignee:    "gastown/Toast",
		CreatedAt:   time.Date(2026, 8, 24, 7, 0, 0, 0, time.UTC),
		Metadata:    metadata,
		Labels:      []string{"gt:message", MailWorkLabel, "msg-type:task", "from:mayor/", "thread:thread-task"},
	}
	if route == WorkRouteQueue {
		issue.Assignee = "queue:repairs"
		issue.Labels = append(issue.Labels, "queue:repairs")
	}
	return issue
}

func cloneWorkIssue(t *testing.T, issue *beadsdk.Issue) *beadsdk.Issue {
	t.Helper()
	cloned := cloneWorkIssueNoTest(issue)
	if cloned == nil && issue != nil {
		t.Fatal("clone returned nil")
	}
	return cloned
}

func cloneWorkIssueNoTest(issue *beadsdk.Issue) *beadsdk.Issue {
	if issue == nil {
		return nil
	}
	cloned := *issue
	cloned.Labels = append([]string(nil), issue.Labels...)
	cloned.Metadata = append(json.RawMessage(nil), issue.Metadata...)
	return &cloned
}

func assertLabelCount(t *testing.T, labels []string, want string, count int) {
	t.Helper()
	got := 0
	for _, label := range labels {
		if label == want {
			got++
		}
	}
	if got != count {
		t.Fatalf("label %q count = %d, want %d in %v", want, got, count, labels)
	}
}
