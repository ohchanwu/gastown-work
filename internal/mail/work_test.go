package mail

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/tmux"
)

func TestMailWorkClassification(t *testing.T) {
	base := Message{
		ID:      "hq-task",
		From:    "mayor/",
		To:      "gastown/Toast",
		Subject: "Do the work",
		Type:    TypeTask,
		Status:  WorkStateOpen,
		Labels:  []string{"gt:message", "msg-type:task", MailWorkLabel},
	}

	tests := []struct {
		name string
		edit func(*Message)
		want bool
	}{
		{name: "persistent direct task", want: true},
		{name: "persistent queue task", edit: func(m *Message) {
			m.To = "queue:repairs"
			m.Queue = "repairs"
			m.Labels = append(m.Labels, "queue:repairs")
		}, want: true},
		{name: "historical unmarked task", edit: func(m *Message) {
			m.Labels = []string{"gt:message", "msg-type:task"}
		}},
		{name: "ephemeral task", edit: func(m *Message) { m.Wisp = true }},
		{name: "channel task", edit: func(m *Message) {
			m.To = "channel:ops"
			m.Channel = "ops"
		}},
		{name: "notification", edit: func(m *Message) { m.Type = TypeNotification }},
		{name: "reply", edit: func(m *Message) { m.Type = TypeReply }},
		{name: "escalation", edit: func(m *Message) { m.Type = TypeEscalation }},
		{name: "self task", edit: func(m *Message) { m.To = m.From }},
		{name: "missing message label", edit: func(m *Message) {
			m.Labels = []string{"msg-type:task", MailWorkLabel}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := base
			msg.Labels = append([]string(nil), base.Labels...)
			if tt.edit != nil {
				tt.edit(&msg)
			}
			if got := msg.IsActionableWork(); got != tt.want {
				t.Fatalf("IsActionableWork() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMailWorkMetadataRoundTripPreservesUnrelatedMetadata(t *testing.T) {
	claimedAt := time.Date(2026, 8, 24, 7, 45, 0, 0, time.UTC)
	work := &WorkMetadata{
		Schema: MailWorkSchema,
		Route:  WorkRouteDirect,
		Claim: &WorkClaim{
			Actor:      "gastown/Toast",
			ClaimedAt:  claimedAt,
			Generation: testWorkGeneration(),
		},
	}

	raw, err := EncodeMailWorkMetadata(json.RawMessage(`{"other":{"keep":true}}`), work)
	if err != nil {
		t.Fatalf("EncodeMailWorkMetadata: %v", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if string(envelope["other"]) != `{"keep":true}` {
		t.Fatalf("unrelated metadata = %s, want preserved object", envelope["other"])
	}

	got, err := ParseMailWorkMetadata(raw)
	if err != nil {
		t.Fatalf("ParseMailWorkMetadata: %v", err)
	}
	if err := got.Validate(WorkStateInProgress); err != nil {
		t.Fatalf("Validate(in_progress): %v", err)
	}
	if got.Claim.Actor != work.Claim.Actor || !got.Claim.Generation.Equal(work.Claim.Generation) {
		t.Fatalf("round trip claim = %+v, want %+v", got.Claim, work.Claim)
	}
}

func TestMailWorkValidation(t *testing.T) {
	now := time.Date(2026, 8, 24, 7, 45, 0, 0, time.UTC)
	claim := &WorkClaim{Actor: "gastown/Toast", ClaimedAt: now, Generation: testWorkGeneration()}
	incompleteGeneration := testWorkGeneration()
	incompleteGeneration.Custody = ""

	tests := []struct {
		name    string
		state   WorkState
		work    WorkMetadata
		wantErr bool
	}{
		{name: "open", state: WorkStateOpen, work: WorkMetadata{Schema: 1, Route: WorkRouteDirect}},
		{name: "in progress", state: WorkStateInProgress, work: WorkMetadata{Schema: 1, Route: WorkRouteDirect, Claim: claim}},
		{name: "blocked", state: WorkStateBlocked, work: WorkMetadata{Schema: 1, Route: WorkRouteQueue, Claim: claim, Blocked: &WorkBlock{Reason: "waiting for review", At: now}}},
		{name: "closed", state: WorkStateClosed, work: WorkMetadata{Schema: 1, Route: WorkRouteDirect, Claim: claim, Completion: &WorkCompletion{ReplyID: "hq-reply", CompletedAt: now}}},
		{name: "unsupported schema", state: WorkStateOpen, work: WorkMetadata{Schema: 2, Route: WorkRouteDirect}, wantErr: true},
		{name: "missing route", state: WorkStateOpen, work: WorkMetadata{Schema: 1}, wantErr: true},
		{name: "open with claim", state: WorkStateOpen, work: WorkMetadata{Schema: 1, Route: WorkRouteDirect, Claim: claim}, wantErr: true},
		{name: "active without claim", state: WorkStateInProgress, work: WorkMetadata{Schema: 1, Route: WorkRouteDirect}, wantErr: true},
		{name: "claim without actor", state: WorkStateInProgress, work: WorkMetadata{Schema: 1, Route: WorkRouteDirect, Claim: &WorkClaim{ClaimedAt: now, Generation: testWorkGeneration()}}, wantErr: true},
		{name: "claim without exact generation", state: WorkStateInProgress, work: WorkMetadata{Schema: 1, Route: WorkRouteDirect, Claim: &WorkClaim{Actor: "gastown/Toast", ClaimedAt: now}}, wantErr: true},
		{name: "claim with incomplete generation", state: WorkStateInProgress, work: WorkMetadata{Schema: 1, Route: WorkRouteDirect, Claim: &WorkClaim{Actor: "gastown/Toast", ClaimedAt: now, Generation: incompleteGeneration}}, wantErr: true},
		{name: "blocked without reason", state: WorkStateBlocked, work: WorkMetadata{Schema: 1, Route: WorkRouteDirect, Claim: claim, Blocked: &WorkBlock{At: now}}, wantErr: true},
		{name: "closed without completion", state: WorkStateClosed, work: WorkMetadata{Schema: 1, Route: WorkRouteDirect, Claim: claim}, wantErr: true},
		{name: "closed without reply", state: WorkStateClosed, work: WorkMetadata{Schema: 1, Route: WorkRouteDirect, Claim: claim, Completion: &WorkCompletion{CompletedAt: now}}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.work.Validate(tt.state)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate(%s) error = %v, wantErr %v", tt.state, err, tt.wantErr)
			}
		})
	}
}

func testWorkGeneration() WorkGeneration {
	return WorkGenerationFromTmux(tmux.SessionGeneration{
		Name:           "gt-gastown-Toast",
		SessionID:      "$8",
		PaneID:         "%12",
		Nonce:          "generation-nonce",
		Custody:        "custody-marker",
		ServerPID:      4242,
		ServerIdentity: "server-start-identity",
		Transport: tmux.SessionTransport{
			Bound:      true,
			SocketName: "gastown",
			SocketPath: "/tmp/tmux-test/gastown",
		},
	})
}
