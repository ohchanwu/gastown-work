package mail

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
)

type threadSearchStore struct {
	beadsdk.Storage
	issues []*beadsdk.Issue
	err    error
	calls  int
	query  string
	filter beadsdk.IssueFilter
}

func (s *threadSearchStore) SearchIssues(_ context.Context, query string, filter beadsdk.IssueFilter) ([]*beadsdk.Issue, error) {
	s.calls++
	s.query = query
	s.filter = filter
	return s.issues, s.err
}

func TestMailboxStoreListByThread(t *testing.T) {
	base := time.Date(2026, time.August, 20, 0, 0, 0, 0, time.UTC)
	store := &threadSearchStore{issues: []*beadsdk.Issue{
		{ID: "msg-z", Title: "message z", Assignee: "gastown/Toast", Status: beadsdk.StatusClosed, CreatedAt: base, Labels: []string{"gt:message", "thread:thread-target", "from:mayor/"}},
		{ID: "msg-hooked", Title: "hooked message", Assignee: "gastown/Toast", Status: beadsdk.Status("hooked"), CreatedAt: base.Add(time.Minute), Labels: []string{"gt:message", "thread:thread-target", "from:mayor/", "read"}},
		{ID: "msg-wisp", Title: "ephemeral message", Assignee: "gastown/Toast", Status: beadsdk.StatusOpen, CreatedAt: base.Add(2 * time.Minute), Ephemeral: true, Labels: []string{"gt:message", "thread:thread-target", "from:mayor/"}},
		{ID: "msg-a", Title: "message a", Assignee: "gastown/Toast", Status: beadsdk.StatusOpen, CreatedAt: base, Labels: []string{"gt:message", "thread:thread-target", "from:mayor/"}},
	}}
	t.Setenv("PATH", t.TempDir())
	m := NewMailboxBeadsWithStore("gastown/Toast", t.TempDir(), store)

	messages, err := m.ListByThread("thread-target")
	if err != nil {
		t.Fatalf("ListByThread: %v", err)
	}
	if store.calls != 1 || store.query != "" {
		t.Fatalf("SearchIssues calls/query = %d/%q, want 1/empty", store.calls, store.query)
	}
	wantFilter := beadsdk.IssueFilter{
		Labels: []string{"gt:message", "thread:thread-target"},
		Limit:  0,
	}
	if !reflect.DeepEqual(store.filter, wantFilter) {
		t.Fatalf("SearchIssues filter = %#v, want %#v", store.filter, wantFilter)
	}
	if len(messages) != 4 {
		t.Fatalf("ListByThread returned %d messages, want 4", len(messages))
	}
	wantIDs := []string{"msg-a", "msg-z", "msg-hooked", "msg-wisp"}
	for i, want := range wantIDs {
		if messages[i].ID != want {
			t.Fatalf("message[%d].ID = %q, want %q", i, messages[i].ID, want)
		}
	}
	if !messages[1].Read || !messages[2].Read {
		t.Fatalf("closed/read-labelled messages not preserved: %#v", messages)
	}
	if !messages[3].Wisp {
		t.Fatal("ephemeral message was not preserved")
	}
}

func TestMailboxStoreListByThreadRejectsInvalidMessage(t *testing.T) {
	tests := []struct {
		name  string
		issue *beadsdk.Issue
	}{
		{name: "nil"},
		{name: "missing route", issue: &beadsdk.Issue{
			ID: "msg-1", Title: "message", Labels: []string{"gt:message", "thread:thread-target", "from:mayor/"},
		}},
		{name: "wrong thread", issue: &beadsdk.Issue{
			ID: "msg-1", Title: "message", Assignee: "gastown/Toast",
			Labels: []string{"gt:message", "thread:thread-other", "from:mayor/"},
		}},
		{name: "queue assignee mismatch", issue: &beadsdk.Issue{
			ID: "msg-1", Title: "message", Assignee: "queue:other",
			Labels: []string{"gt:message", "thread:thread-target", "from:mayor/", "queue:triage"},
		}},
		{name: "duplicate sender labels", issue: &beadsdk.Issue{
			ID: "msg-1", Title: "message", Assignee: "gastown/Toast",
			Labels: []string{"gt:message", "thread:thread-target", "from:mayor/", "from:reaper"},
		}},
		{name: "duplicate thread labels", issue: &beadsdk.Issue{
			ID: "msg-1", Title: "message", Assignee: "gastown/Toast",
			Labels: []string{"gt:message", "thread:thread-other", "thread:thread-target", "from:mayor/"},
		}},
		{name: "duplicate message type labels", issue: &beadsdk.Issue{
			ID: "msg-1", Title: "message", Assignee: "gastown/Toast",
			Labels: []string{"gt:message", "thread:thread-target", "from:mayor/", "msg-type:notification", "msg-type:escalation"},
		}},
		{name: "escalation marker missing", issue: &beadsdk.Issue{
			ID: "msg-1", Title: "message", Assignee: "gastown/Toast",
			Labels: []string{"gt:message", "thread:thread-target", "from:reaper", "msg-type:escalation"},
		}},
		{name: "duplicate escalation markers", issue: &beadsdk.Issue{
			ID: "msg-1", Title: "message", Assignee: "gastown/Toast",
			Labels: []string{"gt:message", "gt:escalation", "gt:escalation", "thread:thread-target", "from:reaper", "msg-type:escalation"},
		}},
		{name: "escalation marker on notification", issue: &beadsdk.Issue{
			ID: "msg-1", Title: "message", Assignee: "gastown/Toast",
			Labels: []string{"gt:message", "gt:escalation", "thread:thread-target", "from:reaper", "msg-type:notification"},
		}},
		{name: "announce route label missing", issue: &beadsdk.Issue{
			ID: "msg-1", Title: "message", Assignee: "announce:alerts",
			Labels: []string{"gt:message", "thread:thread-target", "from:reaper"},
		}},
		{name: "announce assignee mismatch", issue: &beadsdk.Issue{
			ID: "msg-1", Title: "message", Assignee: "announce:other",
			Labels: []string{"gt:message", "thread:thread-target", "from:reaper", "announce:alerts"},
		}},
		{name: "duplicate announce labels", issue: &beadsdk.Issue{
			ID: "msg-1", Title: "message", Assignee: "announce:alerts",
			Labels: []string{"gt:message", "thread:thread-target", "from:reaper", "announce:other", "announce:alerts"},
		}},
		{name: "announce label on direct message", issue: &beadsdk.Issue{
			ID: "msg-1", Title: "message", Assignee: "gastown/Toast",
			Labels: []string{"gt:message", "thread:thread-target", "from:reaper", "announce:alerts"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &threadSearchStore{issues: []*beadsdk.Issue{tt.issue}}
			t.Setenv("PATH", t.TempDir())
			m := NewMailboxBeadsWithStore("gastown/Toast", t.TempDir(), store)
			if _, err := m.ListByThread("thread-target"); err == nil {
				t.Fatal("ListByThread succeeded, want invalid stored message error")
			}
		})
	}
}

func TestMailboxStoreListByThreadAcceptsStoredQueueChannelAndAnnounceRoutes(t *testing.T) {
	store := &threadSearchStore{issues: []*beadsdk.Issue{
		{
			ID: "msg-announce", Title: "announce message", Assignee: "announce:alerts",
			Labels: []string{"gt:message", "thread:thread-target", "from:reaper", "announce:alerts"},
		},
		{
			ID: "msg-queue", Title: "queue message", Assignee: "queue:triage",
			Labels: []string{"gt:message", "thread:thread-target", "from:reaper", "queue:triage"},
		},
		{
			ID: "msg-channel", Title: "channel message", Assignee: "channel:alerts",
			Labels: []string{"gt:message", "thread:thread-target", "from:reaper", "channel:alerts"},
		},
	}}
	t.Setenv("PATH", t.TempDir())
	messages, err := NewMailboxBeadsWithStore("gastown/Toast", t.TempDir(), store).ListByThread("thread-target")
	if err != nil {
		t.Fatalf("ListByThread: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("messages = %#v, want announce, channel, and queue routes", messages)
	}
	byID := make(map[string]*Message, len(messages))
	for _, message := range messages {
		byID[message.ID] = message
	}
	if byID["msg-announce"].To != "announce:alerts" || byID["msg-channel"].Channel != "alerts" || byID["msg-queue"].Queue != "triage" {
		t.Fatalf("messages = %#v, want announce, channel, and queue routes", messages)
	}
}

func TestMailboxStoreListByThreadEmpty(t *testing.T) {
	store := &threadSearchStore{issues: []*beadsdk.Issue{}}
	t.Setenv("PATH", t.TempDir())
	messages, err := NewMailboxBeadsWithStore("gastown/Toast", t.TempDir(), store).ListByThread("thread-missing")
	if err != nil {
		t.Fatalf("ListByThread: %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("ListByThread returned %d messages, want 0", len(messages))
	}
}

func TestMailboxStoreListByThreadReturnsStoreFailureWithoutCLIFallback(t *testing.T) {
	wantErr := errors.New("search failed")
	store := &threadSearchStore{err: wantErr}
	t.Setenv("PATH", t.TempDir())
	m := NewMailboxBeadsWithStore("gastown/Toast", t.TempDir(), store)

	_, err := m.ListByThread("thread-target")
	if err == nil {
		t.Fatal("ListByThread succeeded, want store error")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("ListByThread error = %v, want wrapped %v", err, wantErr)
	}
	if got := err.Error(); got != "store list thread: search failed" {
		t.Fatalf("ListByThread error = %q, want store context", got)
	}
	if store.calls != 1 {
		t.Fatalf("SearchIssues called %d times, want 1", store.calls)
	}
}

func TestMailboxStoreListIncludesActiveWorkAndListAllAddsClosedWork(t *testing.T) {
	store := &threadSearchStore{issues: []*beadsdk.Issue{
		{ID: "open", Status: beadsdk.StatusOpen, Assignee: "gastown/Toast", Labels: []string{"gt:message", MailWorkLabel, "msg-type:task", "from:mayor/"}},
		{ID: "active", Status: beadsdk.StatusInProgress, Assignee: "gastown/Toast", Labels: []string{"gt:message", MailWorkLabel, "msg-type:task", "from:mayor/"}},
		{ID: "blocked", Status: beadsdk.StatusBlocked, Assignee: "gastown/Toast", Labels: []string{"gt:message", MailWorkLabel, "msg-type:task", "from:mayor/"}},
		{ID: "closed-work", Status: beadsdk.StatusClosed, Assignee: "gastown/Toast", Labels: []string{"gt:message", MailWorkLabel, "msg-type:task", "from:mayor/"}},
		{ID: "closed-mail", Status: beadsdk.StatusClosed, Assignee: "gastown/Toast", Labels: []string{"gt:message", "from:mayor/"}},
	}}
	m := NewMailboxBeadsWithStore("gastown/Toast", t.TempDir(), store)

	normal, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if got := messageIDs(normal); !reflect.DeepEqual(got, []string{"open", "active", "blocked"}) {
		t.Fatalf("normal inbox = %v", got)
	}
	all, err := m.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	if got := messageIDs(all); !reflect.DeepEqual(got, []string{"open", "active", "blocked", "closed-work"}) {
		t.Fatalf("all inbox = %v", got)
	}
}

type closeGuardStore struct {
	beadsdk.Storage
	issue  *beadsdk.Issue
	closed int
}

func (s *closeGuardStore) GetIssue(context.Context, string) (*beadsdk.Issue, error) {
	return s.issue, nil
}

func (s *closeGuardStore) CloseIssue(context.Context, string, string, string, string) error {
	s.closed++
	return nil
}

func TestMailboxRejectsGenericCloseForActiveMailWork(t *testing.T) {
	for _, operation := range []string{"delete", "archive"} {
		t.Run(operation, func(t *testing.T) {
			store := &closeGuardStore{issue: &beadsdk.Issue{
				ID: "hq-work", Title: "work", Status: beadsdk.StatusInProgress,
				Assignee: "gastown/Toast",
				Labels:   []string{"gt:message", MailWorkLabel, "msg-type:task", "from:mayor/"},
			}}
			m := NewMailboxWithBeadsDirAndStore("gastown/Toast", t.TempDir(), t.TempDir(), store)
			var err error
			if operation == "delete" {
				err = m.Delete("hq-work")
			} else {
				err = m.Archive("hq-work")
			}
			if !errors.Is(err, ErrMailWorkRequiresLifecycle) {
				t.Fatalf("%s error = %v", operation, err)
			}
			if store.closed != 0 {
				t.Fatalf("%s closed active work", operation)
			}
			if _, statErr := os.Stat(m.ArchivePath()); !os.IsNotExist(statErr) {
				t.Fatalf("%s wrote archive before rejection: %v", operation, statErr)
			}
		})
	}
}
