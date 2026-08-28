package mail

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestNewMailbox(t *testing.T) {
	m := NewMailbox("/tmp/test")
	if filepath.ToSlash(m.path) != "/tmp/test/inbox.jsonl" {
		t.Errorf("NewMailbox path = %q, want %q", m.path, "/tmp/test/inbox.jsonl")
	}
	if !m.legacy {
		t.Error("NewMailbox should create legacy mailbox")
	}
}

func TestNewMailboxBeads(t *testing.T) {
	m := NewMailboxBeads("gastown/Toast", "/work/dir")
	if m.identity != "gastown/Toast" {
		t.Errorf("identity = %q, want %q", m.identity, "gastown/Toast")
	}
	if m.legacy {
		t.Error("NewMailboxBeads should not create legacy mailbox")
	}
}

func TestMailboxLegacyAppend(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	msg := &Message{
		ID:        "msg-001",
		From:      "mayor/",
		To:        "gastown/Toast",
		Subject:   "Test message",
		Body:      "Hello world",
		Timestamp: time.Now(),
	}

	if err := m.Append(msg); err != nil {
		t.Fatalf("Append error: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(m.path); os.IsNotExist(err) {
		t.Fatal("inbox.jsonl was not created")
	}

	// Verify content
	content, err := os.ReadFile(m.path)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}

	var readMsg Message
	if err := json.Unmarshal(content[:len(content)-1], &readMsg); err != nil { // -1 for newline
		t.Fatalf("Unmarshal error: %v", err)
	}

	if readMsg.ID != msg.ID {
		t.Errorf("ID = %q, want %q", readMsg.ID, msg.ID)
	}
}

func TestMailboxLegacyList(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	// Append multiple messages
	msgs := []*Message{
		{ID: "msg-001", Subject: "First", Timestamp: time.Now().Add(-2 * time.Hour)},
		{ID: "msg-002", Subject: "Second", Timestamp: time.Now().Add(-1 * time.Hour)},
		{ID: "msg-003", Subject: "Third", Timestamp: time.Now()},
	}

	for _, msg := range msgs {
		if err := m.Append(msg); err != nil {
			t.Fatalf("Append error: %v", err)
		}
	}

	// List should return newest first
	listed, err := m.List()
	if err != nil {
		t.Fatalf("List error: %v", err)
	}

	if len(listed) != 3 {
		t.Fatalf("List returned %d messages, want 3", len(listed))
	}

	// Verify order (newest first)
	if listed[0].ID != "msg-003" {
		t.Errorf("First message ID = %q, want msg-003 (newest)", listed[0].ID)
	}
	if listed[2].ID != "msg-001" {
		t.Errorf("Last message ID = %q, want msg-001 (oldest)", listed[2].ID)
	}
}

func TestMailboxLegacyGet(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	msg := &Message{
		ID:      "msg-001",
		Subject: "Test",
		Body:    "Content",
	}
	if err := m.Append(msg); err != nil {
		t.Fatalf("Append error: %v", err)
	}

	// Get existing message
	got, err := m.Get("msg-001")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if got.Subject != "Test" {
		t.Errorf("Subject = %q, want %q", got.Subject, "Test")
	}

	// Get non-existent message
	_, err = m.Get("msg-nonexistent")
	if err != ErrMessageNotFound {
		t.Errorf("Get non-existent = %v, want ErrMessageNotFound", err)
	}
}

func TestMailboxLegacyMarkRead(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	msg := &Message{
		ID:   "msg-001",
		Read: false,
	}
	if err := m.Append(msg); err != nil {
		t.Fatalf("Append error: %v", err)
	}

	// Mark as read
	if err := m.MarkRead("msg-001"); err != nil {
		t.Fatalf("MarkRead error: %v", err)
	}

	// Verify it's now read
	got, err := m.Get("msg-001")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if !got.Read {
		t.Error("Message should be marked as read")
	}

	// Mark non-existent
	err = m.MarkRead("msg-nonexistent")
	if err != ErrMessageNotFound {
		t.Errorf("MarkRead non-existent = %v, want ErrMessageNotFound", err)
	}
}

func TestMailboxLegacyDelete(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	msgs := []*Message{
		{ID: "msg-001", Subject: "First"},
		{ID: "msg-002", Subject: "Second"},
	}
	for _, msg := range msgs {
		if err := m.Append(msg); err != nil {
			t.Fatalf("Append error: %v", err)
		}
	}

	// Delete one
	if err := m.Delete("msg-001"); err != nil {
		t.Fatalf("Delete error: %v", err)
	}

	// Verify only one remains
	listed, err := m.List()
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("List returned %d messages, want 1", len(listed))
	}
	if listed[0].ID != "msg-002" {
		t.Errorf("Remaining message ID = %q, want msg-002", listed[0].ID)
	}

	// Delete non-existent
	err = m.Delete("msg-nonexistent")
	if err != ErrMessageNotFound {
		t.Errorf("Delete non-existent = %v, want ErrMessageNotFound", err)
	}
}

func TestMailboxLegacyCount(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	// Empty inbox
	total, unread, err := m.Count()
	if err != nil {
		t.Fatalf("Count error: %v", err)
	}
	if total != 0 || unread != 0 {
		t.Errorf("Empty inbox count = (%d, %d), want (0, 0)", total, unread)
	}

	// Add messages
	msgs := []*Message{
		{ID: "msg-001", Read: false},
		{ID: "msg-002", Read: true},
		{ID: "msg-003", Read: false},
	}
	for _, msg := range msgs {
		if err := m.Append(msg); err != nil {
			t.Fatalf("Append error: %v", err)
		}
	}

	total, unread, err = m.Count()
	if err != nil {
		t.Fatalf("Count error: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if unread != 2 {
		t.Errorf("unread = %d, want 2", unread)
	}
}

func TestMailboxLegacyListUnread(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	msgs := []*Message{
		{ID: "msg-001", Read: false},
		{ID: "msg-002", Read: true},
		{ID: "msg-003", Read: false},
	}
	for _, msg := range msgs {
		if err := m.Append(msg); err != nil {
			t.Fatalf("Append error: %v", err)
		}
	}

	unread, err := m.ListUnread()
	if err != nil {
		t.Fatalf("ListUnread error: %v", err)
	}
	if len(unread) != 2 {
		t.Errorf("ListUnread returned %d, want 2", len(unread))
	}
}

func TestMailboxMarkReadOnlyExcludesFromUnread(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	msgs := []*Message{
		{ID: "msg-001", Read: false, Subject: "First"},
		{ID: "msg-002", Read: false, Subject: "Second"},
	}
	for _, msg := range msgs {
		if err := m.Append(msg); err != nil {
			t.Fatalf("Append error: %v", err)
		}
	}

	// Both should be unread initially
	unread, err := m.ListUnread()
	if err != nil {
		t.Fatalf("ListUnread error: %v", err)
	}
	if len(unread) != 2 {
		t.Errorf("ListUnread returned %d, want 2", len(unread))
	}

	// Mark one as read-only (simulates gt mail read behavior)
	if err := m.MarkReadOnly("msg-001"); err != nil {
		t.Fatalf("MarkReadOnly error: %v", err)
	}

	// Should only have 1 unread now
	unread, err = m.ListUnread()
	if err != nil {
		t.Fatalf("ListUnread error: %v", err)
	}
	if len(unread) != 1 {
		t.Errorf("ListUnread returned %d after MarkReadOnly, want 1", len(unread))
	}
	if len(unread) == 1 && unread[0].ID != "msg-002" {
		t.Errorf("Expected msg-002 to be unread, got %s", unread[0].ID)
	}

	// The marked message should still be in full list
	all, err := m.List()
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("List returned %d, want 2 (MarkReadOnly should not remove)", len(all))
	}
}

func TestMailboxLegacyListByThread(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	msgs := []*Message{
		{ID: "msg-001", ThreadID: "thread-A", Timestamp: time.Now().Add(-2 * time.Hour)},
		{ID: "msg-002", ThreadID: "thread-B", Timestamp: time.Now().Add(-1 * time.Hour)},
		{ID: "msg-003", ThreadID: "thread-A", Timestamp: time.Now()},
	}
	for _, msg := range msgs {
		if err := m.Append(msg); err != nil {
			t.Fatalf("Append error: %v", err)
		}
	}

	// Get thread A
	thread, err := m.ListByThread("thread-A")
	if err != nil {
		t.Fatalf("ListByThread error: %v", err)
	}
	if len(thread) != 2 {
		t.Fatalf("thread-A has %d messages, want 2", len(thread))
	}

	// Verify oldest first
	if thread[0].ID != "msg-001" {
		t.Errorf("First thread message = %q, want msg-001 (oldest)", thread[0].ID)
	}

	// Non-existent thread
	empty, err := m.ListByThread("thread-nonexistent")
	if err != nil {
		t.Fatalf("ListByThread error: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("Non-existent thread has %d messages, want 0", len(empty))
	}
}

func TestMailboxBeadsListByThread(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake bd is POSIX-only")
	}

	base := time.Date(2026, time.August, 20, 0, 0, 0, 0, time.UTC)
	beadsMessages := make([]BeadsMessage, 0, 55)
	for i := 0; i < 53; i++ {
		beadsMessages = append(beadsMessages, BeadsMessage{
			ID:        fmt.Sprintf("msg-%03d", i),
			Title:     fmt.Sprintf("message %d", i),
			Assignee:  "gastown/Toast",
			Status:    "open",
			CreatedAt: base.Add(time.Duration(53-i) * time.Minute),
			Labels:    []string{"gt:message", "thread:thread-target", "from:mayor/"},
		})
	}
	beadsMessages = append(beadsMessages,
		BeadsMessage{ID: "msg-z", Title: "message z", Assignee: "gastown/Toast", Status: "closed", CreatedAt: base, Labels: []string{"gt:message", "thread:thread-target", "from:mayor/"}},
		BeadsMessage{ID: "msg-a", Title: "message a", Assignee: "gastown/Toast", Status: "closed", CreatedAt: base, Labels: []string{"gt:message", "thread:thread-target", "from:mayor/"}},
	)

	m, logPath := newBeadsThreadTestMailbox(t, beadsMessages)
	messages, err := m.ListByThread("thread-target")
	if err != nil {
		t.Fatalf("ListByThread: %v", err)
	}
	if len(messages) != 55 {
		t.Fatalf("ListByThread returned %d messages, want 55", len(messages))
	}
	if messages[0].ID != "msg-a" || messages[1].ID != "msg-z" {
		t.Fatalf("equal-timestamp order = [%s %s], want [msg-a msg-z]", messages[0].ID, messages[1].ID)
	}

	log := readStubLog(t, logPath)
	wantArgs := "args:[list][--include-infra][--all][--label][gt:message][--label][thread:thread-target][--limit][0][--json][--flat]"
	for _, want := range []string{
		wantArgs,
		"BD_READONLY=true",
		"BD_IDENTITY=gastown/Toast",
		"BEADS_DIR=" + m.beadsDir,
		"BEADS_DOLT_SERVER_DATABASE=maildb",
		"PWD=" + m.workDir,
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("bd log missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "args:[message][thread]") {
		t.Fatalf("bd log used unsupported message thread command:\n%s", log)
	}
}

func TestMailboxBeadsListByThreadSchemaV1Envelope(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake bd is POSIX-only")
	}

	tests := []struct {
		name    string
		data    []BeadsMessage
		wantIDs []string
	}{
		{
			name: "populated",
			data: []BeadsMessage{{
				ID: "msg-1", Title: "message", Assignee: "gastown/Toast",
				Labels: []string{"gt:message", "thread:thread-target", "from:mayor/"},
			}},
			wantIDs: []string{"msg-1"},
		},
		{name: "empty", data: []BeadsMessage{}, wantIDs: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, err := json.Marshal(struct {
				SchemaVersion int            `json:"schema_version"`
				Data          []BeadsMessage `json:"data"`
			}{SchemaVersion: 1, Data: tt.data})
			if err != nil {
				t.Fatal(err)
			}
			m, logPath := newBeadsThreadTestMailboxOutput(t, string(stdout))
			t.Setenv("BD_JSON_ENVELOPE", "1")

			messages, err := m.ListByThread("thread-target")
			if err != nil {
				t.Fatalf("ListByThread: %v", err)
			}
			if len(messages) != len(tt.wantIDs) {
				t.Fatalf("ListByThread returned %d messages, want %d", len(messages), len(tt.wantIDs))
			}
			for i, want := range tt.wantIDs {
				if messages[i].ID != want {
					t.Fatalf("message[%d].ID = %q, want %q", i, messages[i].ID, want)
				}
			}
			if !strings.Contains(readStubLog(t, logPath), "BD_JSON_ENVELOPE=1") {
				t.Fatal("bd subprocess did not inherit BD_JSON_ENVELOPE=1")
			}
		})
	}
}

func TestMailboxBeadsListByThreadAcceptsStoredQueueChannelAndAnnounceRoutes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake bd is POSIX-only")
	}

	m, _ := newBeadsThreadTestMailbox(t, []BeadsMessage{
		{
			ID: "msg-queue", Title: "queue message", Assignee: "queue:triage",
			Labels: []string{"gt:message", "thread:thread-target", "from:reaper", "queue:triage"},
		},
		{
			ID: "msg-channel", Title: "channel message", Assignee: "channel:alerts",
			Labels: []string{"gt:message", "thread:thread-target", "from:reaper", "channel:alerts"},
		},
		{
			ID: "msg-announce", Title: "announce message", Assignee: "announce:alerts",
			Labels: []string{"gt:message", "thread:thread-target", "from:reaper", "announce:alerts"},
		},
	})
	messages, err := m.ListByThread("thread-target")
	if err != nil {
		t.Fatalf("ListByThread: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("ListByThread returned %d messages, want 3", len(messages))
	}
	byID := make(map[string]*Message, len(messages))
	for _, message := range messages {
		byID[message.ID] = message
	}
	if got := byID["msg-queue"]; got == nil || got.To != "queue:triage" || got.Queue != "triage" {
		t.Fatalf("queue message = %#v", got)
	}
	if got := byID["msg-channel"]; got == nil || got.To != "channel:alerts" || got.Channel != "alerts" {
		t.Fatalf("channel message = %#v", got)
	}
	if got := byID["msg-announce"]; got == nil || got.To != "announce:alerts" {
		t.Fatalf("announce message = %#v", got)
	}
}

func TestMailboxBeadsListByThreadEmptyArray(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake bd is POSIX-only")
	}

	m, _ := newBeadsThreadTestMailbox(t, []BeadsMessage{})
	messages, err := m.ListByThread("thread-missing")
	if err != nil {
		t.Fatalf("ListByThread: %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("ListByThread returned %d messages, want 0", len(messages))
	}
	if messages == nil {
		t.Fatal("ListByThread returned a nil slice, want JSON []")
	}
}

func TestMailboxBeadsListByThreadRejectsInvalidOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake bd is POSIX-only")
	}

	tests := []struct {
		name   string
		stdout string
	}{
		{name: "empty stdout"},
		{name: "null", stdout: "null\n"},
		{name: "object", stdout: "{}\n"},
		{name: "scalar", stdout: `"value"`},
		{name: "malformed JSON", stdout: "[{\n"},
		{name: "non-JSON", stdout: "not json\n"},
		{name: "null record", stdout: "[null]"},
		{name: "empty record", stdout: "[{}]"},
		{name: "missing message label", stdout: `[{"id":"msg-1","title":"message","assignee":"mayor/","labels":["thread:thread-target","from:reaper"]}]`},
		{name: "wrong thread", stdout: `[{"id":"msg-1","title":"message","assignee":"mayor/","labels":["gt:message","thread:thread-other","from:reaper"]}]`},
		{name: "missing route", stdout: `[{"id":"msg-1","title":"message","labels":["gt:message","thread:thread-target","from:reaper"]}]`},
		{name: "unknown schema", stdout: `{"schema_version":2,"data":[]}`},
		{name: "missing schema", stdout: `{"data":[]}`},
		{name: "missing data", stdout: `{"schema_version":1}`},
		{name: "extra envelope field", stdout: `{"schema_version":1,"data":[],"extra":true}`},
		{name: "duplicate schema", stdout: `{"schema_version":1,"schema_version":1,"data":[]}`},
		{name: "duplicate data", stdout: `{"schema_version":1,"data":[],"data":[]}`},
		{name: "null envelope data", stdout: `{"schema_version":1,"data":null}`},
		{name: "object envelope data", stdout: `{"schema_version":1,"data":{}}`},
		{name: "duplicate record ID", stdout: `[{"id":"ignored","id":"msg-1","title":"message","assignee":"mayor/","labels":["gt:message","thread:thread-target","from:reaper"]}]`},
		{name: "duplicate record assignee", stdout: `[{"id":"msg-1","title":"message","assignee":"overseer","assignee":"mayor/","labels":["gt:message","thread:thread-target","from:reaper"]}]`},
		{name: "duplicate record labels", stdout: `[{"id":"msg-1","title":"message","assignee":"mayor/","labels":[],"labels":["gt:message","thread:thread-target","from:reaper"]}]`},
		{name: "case folded duplicate record labels", stdout: `[{"id":"msg-1","title":"message","assignee":"mayor/","labels":[],"Labels":["gt:message","thread:thread-target","from:reaper"]}]`},
		{name: "unicode folded duplicate record labels", stdout: `[{"id":"msg-1","title":"message","assignee":"mayor/","labels":[],"labelſ":["gt:message","thread:thread-target","from:reaper"]}]`},
		{name: "unicode folded duplicate record assignee", stdout: `[{"id":"msg-1","title":"message","assignee":"overseer","aſſignee":"mayor/","labels":["gt:message","thread:thread-target","from:reaper"]}]`},
		{name: "duplicate sender labels", stdout: `[{"id":"msg-1","title":"message","assignee":"mayor/","labels":["gt:message","thread:thread-target","from:mayor/","from:reaper"]}]`},
		{name: "duplicate thread labels", stdout: `[{"id":"msg-1","title":"message","assignee":"mayor/","labels":["gt:message","thread:thread-other","thread:thread-target","from:reaper"]}]`},
		{name: "duplicate message type labels", stdout: `[{"id":"msg-1","title":"message","assignee":"mayor/","labels":["gt:message","thread:thread-target","from:reaper","msg-type:notification","msg-type:escalation"]}]`},
		{name: "escalation marker missing", stdout: `[{"id":"msg-1","title":"message","assignee":"mayor/","labels":["gt:message","thread:thread-target","from:reaper","msg-type:escalation"]}]`},
		{name: "duplicate escalation markers", stdout: `[{"id":"msg-1","title":"message","assignee":"mayor/","labels":["gt:message","gt:escalation","gt:escalation","thread:thread-target","from:reaper","msg-type:escalation"]}]`},
		{name: "escalation marker on notification", stdout: `[{"id":"msg-1","title":"message","assignee":"mayor/","labels":["gt:message","gt:escalation","thread:thread-target","from:reaper","msg-type:notification"]}]`},
		{name: "duplicate queue labels", stdout: `[{"id":"msg-1","title":"message","assignee":"queue:triage","labels":["gt:message","thread:thread-target","from:reaper","queue:other","queue:triage"]}]`},
		{name: "duplicate channel labels", stdout: `[{"id":"msg-1","title":"message","assignee":"channel:alerts","labels":["gt:message","thread:thread-target","from:reaper","channel:other","channel:alerts"]}]`},
		{name: "queue assignee mismatch", stdout: `[{"id":"msg-1","title":"message","assignee":"queue:other","labels":["gt:message","thread:thread-target","from:reaper","queue:triage"]}]`},
		{name: "queue route label missing", stdout: `[{"id":"msg-1","title":"message","assignee":"queue:triage","labels":["gt:message","thread:thread-target","from:reaper"]}]`},
		{name: "channel assignee mismatch", stdout: `[{"id":"msg-1","title":"message","assignee":"channel:other","labels":["gt:message","thread:thread-target","from:reaper","channel:alerts"]}]`},
		{name: "announce route label missing", stdout: `[{"id":"msg-1","title":"message","assignee":"announce:alerts","labels":["gt:message","thread:thread-target","from:reaper"]}]`},
		{name: "announce assignee mismatch", stdout: `[{"id":"msg-1","title":"message","assignee":"announce:other","labels":["gt:message","thread:thread-target","from:reaper","announce:alerts"]}]`},
		{name: "duplicate announce labels", stdout: `[{"id":"msg-1","title":"message","assignee":"announce:alerts","labels":["gt:message","thread:thread-target","from:reaper","announce:other","announce:alerts"]}]`},
		{name: "announce label on direct message", stdout: `[{"id":"msg-1","title":"message","assignee":"mayor/","labels":["gt:message","thread:thread-target","from:reaper","announce:alerts"]}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := newBeadsThreadTestMailboxOutput(t, tt.stdout)
			if _, err := m.ListByThread("thread-target"); err == nil {
				t.Fatal("ListByThread succeeded, want output validation error")
			}
		})
	}
}

func TestMailboxBeadsListByThreadReturnsCommandFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake bd is POSIX-only")
	}

	m, _ := newBeadsThreadTestMailboxOutput(t, "[]\n")
	t.Setenv("BD_STUB_EXIT", "7")
	t.Setenv("BD_STUB_STDERR", "thread lookup failed")
	_, err := m.ListByThread("thread-target")
	if err == nil {
		t.Fatal("ListByThread succeeded, want command error")
	}
	var commandErr *bdError
	if !errors.As(err, &commandErr) {
		t.Fatalf("ListByThread error type = %T, want *bdError", err)
	}
}

func newBeadsThreadTestMailbox(t *testing.T, messages []BeadsMessage) (*Mailbox, string) {
	t.Helper()
	data, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	return newBeadsThreadTestMailboxOutput(t, string(data))
}

func newBeadsThreadTestMailboxOutput(t *testing.T, stdout string) (*Mailbox, string) {
	t.Helper()
	binDir := t.TempDir()
	writeMailBDStub(t, binDir)
	logPath := filepath.Join(t.TempDir(), "bd.log")
	stdoutPath := filepath.Join(t.TempDir(), "stdout")
	if err := os.WriteFile(stdoutPath, []byte(stdout), 0644); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	beadsDir := filepath.Join(workDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"dolt_database":"maildb"}`), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_STUB_LOG", logPath)
	t.Setenv("BD_STUB_STDOUT_FILE", stdoutPath)
	return NewMailboxWithBeadsDir("gastown/Toast", workDir, beadsDir), logPath
}

func TestMailboxLegacyEmptyInbox(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	// List on non-existent file should return empty, not error
	msgs, err := m.List()
	if err != nil {
		t.Fatalf("List on empty inbox error: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("Empty inbox returned %d messages, want 0", len(msgs))
	}
}

func TestMailboxBeadsAppendError(t *testing.T) {
	m := NewMailboxBeads("gastown/Toast", "/work/dir")

	err := m.Append(&Message{})
	if err == nil {
		t.Error("Append on beads mailbox should error")
	}
}

func TestMailboxIdentityAndPath(t *testing.T) {
	// Legacy mailbox
	legacy := NewMailbox("/tmp/test")
	if legacy.Identity() != "" {
		t.Errorf("Legacy mailbox identity = %q, want empty", legacy.Identity())
	}
	if filepath.ToSlash(legacy.Path()) != "/tmp/test/inbox.jsonl" {
		t.Errorf("Legacy mailbox path = %q, want /tmp/test/inbox.jsonl", legacy.Path())
	}

	// Beads mailbox
	beads := NewMailboxBeads("gastown/Toast", "/work/dir")
	if beads.Identity() != "gastown/Toast" {
		t.Errorf("Beads mailbox identity = %q, want gastown/Toast", beads.Identity())
	}
	if beads.Path() != "" {
		t.Errorf("Beads mailbox path = %q, want empty", beads.Path())
	}
}

func TestMailboxPersistence(t *testing.T) {
	tmpDir := t.TempDir()

	// Create mailbox and add message
	m1 := NewMailbox(tmpDir)
	msg := &Message{
		ID:      "persist-001",
		Subject: "Persistent message",
		Body:    "Should survive reload",
	}
	if err := m1.Append(msg); err != nil {
		t.Fatalf("Append error: %v", err)
	}

	// Create new mailbox pointing to same location
	m2 := NewMailbox(tmpDir)
	msgs, err := m2.List()
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Reloaded mailbox has %d messages, want 1", len(msgs))
	}
	if msgs[0].Subject != "Persistent message" {
		t.Errorf("Subject = %q, want 'Persistent message'", msgs[0].Subject)
	}
}

func TestNewMailboxWithBeadsDir(t *testing.T) {
	m := NewMailboxWithBeadsDir("gastown/Toast", "/work/dir", "/custom/.beads")
	if m.identity != "gastown/Toast" {
		t.Errorf("identity = %q, want 'gastown/Toast'", m.identity)
	}
	if filepath.ToSlash(m.beadsDir) != "/custom/.beads" {
		t.Errorf("beadsDir = %q, want '/custom/.beads'", m.beadsDir)
	}
}

func TestMailboxGetBeadsOnlyReturnsOwnedMail(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake bd is POSIX-only")
	}

	binDir := t.TempDir()
	fakeBD := filepath.Join(binDir, "bd")
	script := `#!/bin/sh
if [ "$1" != "show" ]; then
  printf 'unexpected bd args: %s\n' "$*" >&2
  exit 1
fi
case "$2" in
  direct-owned)
    printf '%s\n' '[{"id":"direct-owned","title":"Direct","description":"direct body","status":"open","priority":2,"assignee":"gastown/synth","created_at":"2026-08-09T00:00:00Z","labels":["gt:message","from:mayor/","msg-type:notification"]}]'
    ;;
  cc-owned)
    printf '%s\n' '[{"id":"cc-owned","title":"CC","description":"cc body","status":"open","priority":2,"assignee":"mayor/","created_at":"2026-08-09T00:00:01Z","labels":["gt:message","from:deacon/","cc:gastown/synth","msg-type:notification"]}]'
    ;;
  foreign-mail)
    printf '%s\n' '[{"id":"foreign-mail","title":"Foreign","description":"private body","status":"open","priority":2,"assignee":"gastown/other","created_at":"2026-08-09T00:00:02Z","labels":["gt:message","from:mayor/","msg-type:notification"]}]'
    ;;
  owned-hook)
    printf '%s\n' '[{"id":"owned-hook","title":"Hook","description":"work body","status":"hooked","priority":2,"assignee":"gastown/synth","created_at":"2026-08-09T00:00:03Z","labels":["gt:hook","from:mayor/"]}]'
    ;;
  *)
    printf '%s\n' '[]'
    ;;
esac
`
	if err := os.WriteFile(fakeBD, []byte(script), 0755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	mailbox := NewMailboxWithBeadsDir("gastown/synth", t.TempDir(), t.TempDir())
	for _, test := range []struct {
		id      string
		wantErr error
	}{
		{id: "direct-owned"},
		{id: "cc-owned"},
		{id: "foreign-mail", wantErr: ErrMessageNotFound},
		{id: "owned-hook", wantErr: ErrMessageNotFound},
	} {
		t.Run(test.id, func(t *testing.T) {
			message, err := mailbox.Get(test.id)
			if err != test.wantErr {
				t.Fatalf("Get(%q) error = %v, want %v", test.id, err, test.wantErr)
			}
			if test.wantErr == nil && (message == nil || message.ID != test.id) {
				t.Fatalf("Get(%q) message = %#v", test.id, message)
			}
		})
	}
}

func TestMailboxListFromDirConvergesWispQueryAndFiltersStatuses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake bd is POSIX-only")
	}

	beadsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(beadsDir, ".gt-types-configured"), []byte(beads.TypeConfigSentinelValue()+"\n"), 0644); err != nil {
		t.Fatalf("write types sentinel: %v", err)
	}

	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	fakeBD := filepath.Join(binDir, "bd")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$BD_LOG"
if [ "$1" = "list" ]; then
  case "$*" in
    *"--assignee gastown/synth"*)
      printf '%s\n' '[{"id":"issue-direct-open","title":"Direct open","description":"","status":"open","priority":2,"assignee":"gastown/synth","created_at":"2026-06-12T12:00:05Z","labels":["gt:message","from:mayor/"]},{"id":"issue-direct-hooked","title":"Direct hooked","description":"","status":"hooked","priority":2,"assignee":"gastown/synth","created_at":"2026-06-12T12:00:04Z","labels":["gt:message","from:mayor/"]},{"id":"issue-direct-closed","title":"Direct closed","description":"","status":"closed","priority":2,"assignee":"gastown/synth","created_at":"2026-06-12T12:00:03Z","labels":["gt:message","from:mayor/"]}]'
      exit 0
      ;;
    *"--label cc:gastown/synth"*)
      printf '%s\n' '[{"id":"issue-cc-open","title":"CC open","description":"","status":"open","priority":2,"assignee":"mayor/","created_at":"2026-06-12T12:00:02Z","labels":["gt:message","cc:gastown/synth","from:mayor/"]},{"id":"issue-cc-hooked","title":"CC hooked","description":"","status":"hooked","priority":2,"assignee":"mayor/","created_at":"2026-06-12T12:00:01Z","labels":["gt:message","cc:gastown/synth","from:mayor/"]}]'
      exit 0
      ;;
  esac
  printf '%s\n' 'No issues found.'
  exit 0
fi
if [ "$1" = "sql" ]; then
  printf '%s\n' '[{"id":"wisp-direct-open","title":"Wisp direct open","description":"","status":"open","priority":2,"assignee":"gastown/synth","created_at":"2026-06-12T12:00:00Z","updated_at":"2026-06-12T12:00:00Z","labels_csv":"gt:message,from:mayor/","assignee_match":1,"cc_match":0},{"id":"wisp-direct-hooked","title":"Wisp direct hooked","description":"","status":"hooked","priority":2,"assignee":"gastown/synth","created_at":"2026-06-12T11:59:59Z","updated_at":"2026-06-12T11:59:59Z","labels_csv":"gt:message,from:mayor/","assignee_match":1,"cc_match":0},{"id":"wisp-cc-open","title":"Wisp CC open","description":"","status":"open","priority":2,"assignee":"mayor/","created_at":"2026-06-12T11:59:58Z","updated_at":"2026-06-12T11:59:58Z","labels_csv":"gt:message,cc:gastown/synth,from:mayor/","assignee_match":0,"cc_match":1},{"id":"wisp-cc-hooked","title":"Wisp CC hooked","description":"","status":"hooked","priority":2,"assignee":"mayor/","created_at":"2026-06-12T11:59:57Z","updated_at":"2026-06-12T11:59:57Z","labels_csv":"gt:message,cc:gastown/synth,from:mayor/","assignee_match":0,"cc_match":1},{"id":"issue-direct-open","title":"Duplicate wisp","description":"","status":"open","priority":2,"assignee":"gastown/synth","created_at":"2026-06-12T11:59:56Z","updated_at":"2026-06-12T11:59:56Z","labels_csv":"gt:message,from:mayor/","assignee_match":1,"cc_match":0}]'
  exit 0
fi
printf 'unexpected bd args: %s\n' "$*" >&2
exit 1
`
	if err := os.WriteFile(fakeBD, []byte(script), 0755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_LOG", logPath)

	m := NewMailboxWithBeadsDir("gastown/synth", t.TempDir(), beadsDir)
	msgs, err := m.listFromDir(beadsDir, false)
	if err != nil {
		t.Fatalf("listFromDir: %v", err)
	}

	byID := make(map[string]*Message)
	for _, msg := range msgs {
		byID[msg.ID] = msg
	}
	wantPresent := []string{
		"issue-direct-open",
		"issue-direct-hooked",
		"issue-cc-open",
		"wisp-direct-open",
		"wisp-direct-hooked",
		"wisp-cc-open",
	}
	for _, id := range wantPresent {
		if byID[id] == nil {
			t.Fatalf("missing message %s in %#v", id, byID)
		}
	}
	wantAbsent := []string{
		"issue-direct-closed",
		"issue-cc-hooked",
		"wisp-cc-hooked",
	}
	for _, id := range wantAbsent {
		if byID[id] != nil {
			t.Fatalf("unexpected message %s in inbox", id)
		}
	}
	if byID["issue-direct-open"].Wisp {
		t.Fatal("issue duplicate should keep issue result, not later wisp result")
	}
	for _, id := range []string{"wisp-direct-open", "wisp-direct-hooked", "wisp-cc-open"} {
		if !byID[id].Wisp {
			t.Fatalf("%s should be marked as wisp", id)
		}
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	if got := strings.Count(string(logBytes), "sql "); got != 1 {
		t.Fatalf("bd sql calls = %d, want 1; log:\n%s", got, string(logBytes))
	}
	if got := strings.Count(string(logBytes), "--status=all"); got != 2 {
		t.Fatalf("bd all-status issue queries = %d, want 2; log:\n%s", got, string(logBytes))
	}
}

func TestQueryWispMessagesEscapesIdentitySQLLiterals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake bd is POSIX-only")
	}

	binDir := t.TempDir()
	sqlLogPath := filepath.Join(t.TempDir(), "bd-sql.log")
	fakeBD := filepath.Join(binDir, "bd")
	script := `#!/bin/sh
if [ "$1" = "sql" ]; then
  printf '%s\n' "$3" >> "$BD_SQL_LOG"
  printf '%s\n' '[]'
  exit 0
fi
printf 'unexpected bd args: %s\n' "$*" >&2
exit 1
`
	if err := os.WriteFile(fakeBD, []byte(script), 0755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_SQL_LOG", sqlLogPath)

	m := NewMailboxWithBeadsDir("mayor/", t.TempDir(), t.TempDir())
	_, err := m.queryWispMessages(t.TempDir(), []string{"mayor/", "mayor", `rig/o\'malley`})
	if err != nil {
		t.Fatalf("queryWispMessages: %v", err)
	}

	sqlLog, err := os.ReadFile(sqlLogPath)
	if err != nil {
		t.Fatalf("read SQL log: %v", err)
	}
	queries := strings.Split(strings.TrimSpace(string(sqlLog)), "\n")
	if len(queries) != 1 {
		t.Fatalf("bd sql calls = %d, want 1; log:\n%s", len(queries), string(sqlLog))
	}
	sql := queries[0]
	for _, want := range []string{
		`'mayor/'`,
		`'mayor'`,
		`'cc:mayor/'`,
		`'cc:mayor'`,
		`'rig/o\\''malley'`,
		`'cc:rig/o\\''malley'`,
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("generated SQL missing %q:\n%s", want, sql)
		}
	}
}

func TestSQLStringListEscapesSQLLiterals(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   string
	}{
		{name: "plain", values: []string{"mayor"}, want: `'mayor'`},
		{name: "quote", values: []string{"o'brien"}, want: `'o''brien'`},
		{name: "backslash", values: []string{`rig\agent`}, want: `'rig\\agent'`},
		{name: "trailing backslash", values: []string{`rig\`}, want: `'rig\\'`},
		{name: "quote after backslash", values: []string{`rig/o\'malley`}, want: `'rig/o\\''malley'`},
		{name: "list", values: []string{"mayor/", `rig/o\'malley`}, want: `'mayor/','rig/o\\''malley'`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sqlStringList(tt.values); got != tt.want {
				t.Fatalf("sqlStringList(%#v) = %q, want %q", tt.values, got, tt.want)
			}
		})
	}
}

func TestParseWispTimestamp(t *testing.T) {
	want := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Time
		ok    bool
	}{
		{name: "rfc3339", value: "2026-06-12T12:00:00Z", want: want, ok: true},
		{name: "rfc3339 offset", value: "2026-06-12T08:00:00-04:00", want: want, ok: true},
		{name: "go utc", value: "2026-06-12 12:00:00 +0000 UTC", want: want, ok: true},
		{name: "sql datetime", value: "2026-06-12 12:00:00", want: want, ok: true},
		{name: "empty"},
		{name: "malformed", value: "not a timestamp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseWispTimestamp(tt.value)
			if ok != tt.ok {
				t.Fatalf("parseWispTimestamp(%q) ok = %v, want %v", tt.value, ok, tt.ok)
			}
			if ok && !got.Equal(tt.want) {
				t.Fatalf("parseWispTimestamp(%q) = %v, want %v", tt.value, got, tt.want)
			}
			if ok && got.Location() != time.UTC {
				t.Fatalf("parseWispTimestamp(%q) location = %v, want UTC", tt.value, got.Location())
			}
		})
	}
}

func TestMailboxLegacyMultipleOperations(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	// Append multiple messages
	for i := 0; i < 5; i++ {
		msg := &Message{
			ID:        fmt.Sprintf("msg-%03d", i),
			Subject:   fmt.Sprintf("Subject %d", i),
			Body:      fmt.Sprintf("Body %d", i),
			Read:      i%2 == 0, // Alternate read/unread
			Timestamp: time.Now().Add(time.Duration(i) * time.Minute),
		}
		if err := m.Append(msg); err != nil {
			t.Fatalf("Append error: %v", err)
		}
	}

	// Delete middle message
	if err := m.Delete("msg-002"); err != nil {
		t.Fatalf("Delete error: %v", err)
	}

	// Mark one as read
	if err := m.MarkRead("msg-001"); err != nil {
		t.Fatalf("MarkRead error: %v", err)
	}

	// Verify counts
	total, unread, err := m.Count()
	if err != nil {
		t.Fatalf("Count error: %v", err)
	}
	if total != 4 {
		t.Errorf("total = %d, want 4", total)
	}
	// After marking msg-001 as read, we have: msg-000 (read), msg-001 (read), msg-003 (unread), msg-004 (read)
	// So unread = 1
	if unread != 1 {
		t.Errorf("unread = %d, want 1", unread)
	}
}

func TestMailboxLegacyAppendWithMissingDir(t *testing.T) {
	tmpDir := t.TempDir()
	deepPath := filepath.Join(tmpDir, "deep", "nested", "inbox")
	m := NewMailbox(deepPath)

	msg := &Message{
		ID:      "msg-001",
		Subject: "Test",
	}

	// Should create directories
	if err := m.Append(msg); err != nil {
		t.Fatalf("Append error: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(m.path); os.IsNotExist(err) {
		t.Fatal("inbox.jsonl was not created")
	}
}

func TestMailboxLegacyDeleteAll(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	// Add messages
	msgs := []*Message{
		{ID: "msg-001"},
		{ID: "msg-002"},
	}
	for _, msg := range msgs {
		if err := m.Append(msg); err != nil {
			t.Fatalf("Append error: %v", err)
		}
	}

	// Delete all
	for _, msg := range msgs {
		if err := m.Delete(msg.ID); err != nil {
			t.Fatalf("Delete error: %v", err)
		}
	}

	// Should be empty
	total, _, err := m.Count()
	if err != nil {
		t.Fatalf("Count error: %v", err)
	}
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
}

func TestMailboxLegacyMarkReadTwice(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	msg := &Message{ID: "msg-001", Read: false}
	if err := m.Append(msg); err != nil {
		t.Fatalf("Append error: %v", err)
	}

	// Mark as read twice
	if err := m.MarkRead("msg-001"); err != nil {
		t.Fatalf("First MarkRead error: %v", err)
	}
	if err := m.MarkRead("msg-001"); err != nil {
		t.Fatalf("Second MarkRead error: %v", err)
	}

	// Should still be read
	got, err := m.Get("msg-001")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if !got.Read {
		t.Error("Message should be marked as read")
	}
}

func TestMailboxLegacyCorruptionDetection(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	// Write a valid message followed by a corrupt line
	msg := &Message{ID: "msg-001", Subject: "Valid"}
	if err := m.Append(msg); err != nil {
		t.Fatalf("Append error: %v", err)
	}

	// Manually append a corrupt line
	f, err := os.OpenFile(m.path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatalf("OpenFile error: %v", err)
	}
	if _, err := f.WriteString("this is not valid json\n"); err != nil {
		t.Fatalf("WriteString error: %v", err)
	}
	f.Close()

	// List should return error mentioning corruption
	_, err = m.List()
	if err == nil {
		t.Fatal("List should return error for corrupt mailbox")
	}
	if !strings.Contains(err.Error(), "corrupt mailbox") {
		t.Errorf("error should mention corruption, got: %v", err)
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error should mention line number, got: %v", err)
	}
}

func TestMailboxLegacyArchiveCorruptionDetection(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	// Create a corrupt archive file
	archivePath := m.ArchivePath()
	if err := os.WriteFile(archivePath, []byte("{bad json\n"), 0644); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}

	_, err := m.ListArchived()
	if err == nil {
		t.Fatal("ListArchived should return error for corrupt archive")
	}
	if !strings.Contains(err.Error(), "corrupt archive") {
		t.Errorf("error should mention corruption, got: %v", err)
	}
}

func TestMailboxLegacyConcurrentMarkRead(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	// Add messages
	for i := 0; i < 10; i++ {
		msg := &Message{
			ID:        fmt.Sprintf("msg-%03d", i),
			Subject:   fmt.Sprintf("Subject %d", i),
			Read:      false,
			Timestamp: time.Now().Add(time.Duration(i) * time.Minute),
		}
		if err := m.Append(msg); err != nil {
			t.Fatalf("Append error: %v", err)
		}
	}

	// Concurrently mark different messages as read
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = m.MarkRead(fmt.Sprintf("msg-%03d", idx))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("MarkRead msg-%03d error: %v", i, err)
		}
	}

	// All messages should be marked as read
	messages, err := m.List()
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(messages) != 10 {
		t.Fatalf("Expected 10 messages, got %d", len(messages))
	}
	for _, msg := range messages {
		if !msg.Read {
			t.Errorf("Message %s should be marked as read", msg.ID)
		}
	}
}

func TestMailboxLegacyAtomicArchive(t *testing.T) {
	tmpDir := t.TempDir()
	m := NewMailbox(tmpDir)

	// Add messages
	msgs := []*Message{
		{ID: "msg-001", Subject: "First", Timestamp: time.Now().Add(-2 * time.Hour)},
		{ID: "msg-002", Subject: "Second", Timestamp: time.Now().Add(-1 * time.Hour)},
		{ID: "msg-003", Subject: "Third", Timestamp: time.Now()},
	}
	for _, msg := range msgs {
		if err := m.Append(msg); err != nil {
			t.Fatalf("Append error: %v", err)
		}
	}

	// Archive the middle message
	if err := m.Archive("msg-002"); err != nil {
		t.Fatalf("Archive error: %v", err)
	}

	// Inbox should have 2 messages
	inbox, err := m.List()
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(inbox) != 2 {
		t.Fatalf("Expected 2 inbox messages, got %d", len(inbox))
	}

	// Archive should have 1 message
	archived, err := m.ListArchived()
	if err != nil {
		t.Fatalf("ListArchived error: %v", err)
	}
	if len(archived) != 1 {
		t.Fatalf("Expected 1 archived message, got %d", len(archived))
	}
	if archived[0].ID != "msg-002" {
		t.Errorf("Archived message ID = %q, want msg-002", archived[0].ID)
	}
}

func TestAppendBeadsMessagesKeepsActiveWorkAndAllAddsClosedWork(t *testing.T) {
	items := []BeadsMessage{
		{ID: "open", Status: "open", Assignee: "gastown/Toast", Labels: []string{"gt:message", MailWorkLabel, "msg-type:task", "from:mayor/"}},
		{ID: "active", Status: "in_progress", Assignee: "gastown/Toast", Labels: []string{"gt:message", MailWorkLabel, "msg-type:task", "from:mayor/"}},
		{ID: "blocked", Status: "blocked", Assignee: "gastown/Toast", Labels: []string{"gt:message", MailWorkLabel, "msg-type:task", "from:mayor/"}},
		{ID: "closed-work", Status: "closed", Assignee: "gastown/Toast", Labels: []string{"gt:message", MailWorkLabel, "msg-type:task", "from:mayor/"}},
		{ID: "closed-mail", Status: "closed", Assignee: "gastown/Toast", Labels: []string{"gt:message", "from:mayor/"}},
	}

	normal := appendBeadsMessages(nil, map[string]bool{}, items, true, false)
	if got := messageIDs(normal); !reflect.DeepEqual(got, []string{"open", "active", "blocked"}) {
		t.Fatalf("normal inbox = %v", got)
	}
	all := appendBeadsMessages(nil, map[string]bool{}, items, true, true)
	if got := messageIDs(all); !reflect.DeepEqual(got, []string{"open", "active", "blocked", "closed-work"}) {
		t.Fatalf("all inbox = %v", got)
	}
}

func messageIDs(messages []*Message) []string {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	return ids
}
