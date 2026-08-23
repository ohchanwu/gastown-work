package mail

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/tmux"
)

const (
	MailWorkLabel  = "gt:mail-work"
	MailWorkSchema = 1
)

type WorkState string

const (
	WorkStateOpen       WorkState = "open"
	WorkStateInProgress WorkState = "in_progress"
	WorkStateBlocked    WorkState = "blocked"
	WorkStateClosed     WorkState = "closed"
)

type WorkRoute string

const (
	WorkRouteDirect WorkRoute = "direct"
	WorkRouteQueue  WorkRoute = "queue"
)

type WorkGeneration struct {
	Name           string                `json:"name"`
	SessionID      string                `json:"session_id"`
	PaneID         string                `json:"pane_id,omitempty"`
	Nonce          string                `json:"nonce"`
	Custody        string                `json:"custody"`
	ServerPID      int                   `json:"server_pid"`
	ServerIdentity string                `json:"server_identity"`
	Transport      tmux.SessionTransport `json:"transport"`
}

func WorkGenerationFromTmux(g tmux.SessionGeneration) WorkGeneration {
	return WorkGeneration{
		Name:           g.Name,
		SessionID:      g.SessionID,
		PaneID:         g.PaneID,
		Nonce:          g.Nonce,
		Custody:        g.Custody,
		ServerPID:      g.ServerPID,
		ServerIdentity: g.ServerIdentity,
		Transport:      g.Transport,
	}
}

func (g WorkGeneration) Tmux() tmux.SessionGeneration {
	return tmux.SessionGeneration{
		Name:           g.Name,
		SessionID:      g.SessionID,
		PaneID:         g.PaneID,
		Nonce:          g.Nonce,
		Custody:        g.Custody,
		ServerPID:      g.ServerPID,
		ServerIdentity: g.ServerIdentity,
		Transport:      g.Transport,
	}
}

func (g WorkGeneration) Equal(other WorkGeneration) bool {
	return g.Tmux().Equal(other.Tmux())
}

func (g WorkGeneration) validate() error {
	if strings.TrimSpace(g.Name) == "" || strings.TrimSpace(g.SessionID) == "" ||
		strings.TrimSpace(g.Nonce) == "" || strings.TrimSpace(g.Custody) == "" ||
		g.ServerPID <= 0 || strings.TrimSpace(g.ServerIdentity) == "" {
		return fmt.Errorf("incomplete session generation custody")
	}
	if _, err := tmux.NewTmuxForSessionGeneration(g.Tmux()); err != nil {
		return fmt.Errorf("invalid session generation: %w", err)
	}
	if !g.Equal(g) {
		return fmt.Errorf("invalid session generation custody")
	}
	return nil
}

type WorkClaim struct {
	Actor      string         `json:"actor"`
	ClaimedAt  time.Time      `json:"claimed_at"`
	Generation WorkGeneration `json:"generation"`
}

type WorkBlock struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

type WorkCompletion struct {
	ReplyID     string    `json:"reply_id"`
	CompletedAt time.Time `json:"completed_at"`
}

type WorkMetadata struct {
	Schema     int             `json:"schema"`
	Route      WorkRoute       `json:"route"`
	Claim      *WorkClaim      `json:"claim,omitempty"`
	Blocked    *WorkBlock      `json:"blocked,omitempty"`
	Completion *WorkCompletion `json:"completion,omitempty"`
}

func (w *WorkMetadata) Validate(state WorkState) error {
	if w == nil {
		return fmt.Errorf("missing gt_mail metadata")
	}
	if w.Schema != MailWorkSchema {
		return fmt.Errorf("unsupported gt_mail schema %d", w.Schema)
	}
	if w.Route != WorkRouteDirect && w.Route != WorkRouteQueue {
		return fmt.Errorf("invalid mail work route %q", w.Route)
	}

	validateClaim := func() error {
		if w.Claim == nil {
			return fmt.Errorf("missing claim")
		}
		if strings.TrimSpace(w.Claim.Actor) == "" || w.Claim.ClaimedAt.IsZero() {
			return fmt.Errorf("incomplete claim")
		}
		return w.Claim.Generation.validate()
	}

	switch state {
	case WorkStateOpen:
		if w.Claim != nil || w.Blocked != nil || w.Completion != nil {
			return fmt.Errorf("open work contains active state")
		}
	case WorkStateInProgress:
		if err := validateClaim(); err != nil {
			return err
		}
		if w.Blocked != nil || w.Completion != nil {
			return fmt.Errorf("in-progress work contains terminal or blocked state")
		}
	case WorkStateBlocked:
		if err := validateClaim(); err != nil {
			return err
		}
		if w.Blocked == nil || strings.TrimSpace(w.Blocked.Reason) == "" || w.Blocked.At.IsZero() {
			return fmt.Errorf("incomplete blocker")
		}
		if w.Completion != nil {
			return fmt.Errorf("blocked work contains completion")
		}
	case WorkStateClosed:
		if err := validateClaim(); err != nil {
			return err
		}
		if w.Blocked != nil {
			return fmt.Errorf("closed work contains active blocker")
		}
		if w.Completion == nil || strings.TrimSpace(w.Completion.ReplyID) == "" || w.Completion.CompletedAt.IsZero() {
			return fmt.Errorf("incomplete completion")
		}
	default:
		return fmt.Errorf("unsupported mail work state %q", state)
	}
	return nil
}

func ParseMailWorkMetadata(raw json.RawMessage) (*WorkMetadata, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("parse issue metadata: %w", err)
	}
	encoded, ok := envelope["gt_mail"]
	if !ok {
		return nil, nil
	}
	var work WorkMetadata
	if err := json.Unmarshal(encoded, &work); err != nil {
		return nil, fmt.Errorf("parse gt_mail metadata: %w", err)
	}
	return &work, nil
}

func EncodeMailWorkMetadata(raw json.RawMessage, work *WorkMetadata) (json.RawMessage, error) {
	envelope := make(map[string]json.RawMessage)
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return nil, fmt.Errorf("parse issue metadata: %w", err)
		}
	}
	encoded, err := json.Marshal(work)
	if err != nil {
		return nil, fmt.Errorf("encode gt_mail metadata: %w", err)
	}
	envelope["gt_mail"] = encoded
	return json.Marshal(envelope)
}

func (m *Message) IsActionableWork() bool {
	if m == nil || m.Wisp || m.Type != TypeTask || !m.HasLabel("gt:message") || !m.HasLabel(MailWorkLabel) {
		return false
	}
	if AddressToIdentity(m.From) == AddressToIdentity(m.To) {
		return false
	}
	return m.IsDirectMessage() || m.IsQueueMessage()
}

func (m *Message) HasLabel(label string) bool {
	if m == nil {
		return false
	}
	for _, existing := range m.Labels {
		if existing == label {
			return true
		}
	}
	return false
}
