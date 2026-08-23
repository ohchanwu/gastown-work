package mail

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/tmux"
)

var (
	ErrMailWorkConflict     = errors.New("mail work conflict")
	ErrMailWorkUnauthorized = errors.New("mail work unauthorized")
	ErrMailWorkInvalid      = errors.New("invalid mail work record")
)

type MailWorkStore struct {
	store beadsdk.Storage
	now   func() time.Time
}

func NewMailWorkStore(store beadsdk.Storage) *MailWorkStore {
	return &MailWorkStore{store: store, now: time.Now}
}

type QueueClaimEligibility func(queue, actor string) bool

func (s *MailWorkStore) Claim(ctx context.Context, id, actor string, generation tmux.SessionGeneration, eligible QueueClaimEligibility) (*WorkMetadata, error) {
	actor = AddressToIdentity(actor)
	currentGeneration := WorkGenerationFromTmux(generation)
	if err := currentGeneration.validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMailWorkInvalid, err)
	}
	claimedAt := s.now().UTC().Truncate(time.Second)
	var result *WorkMetadata
	err := s.runTransaction(ctx, "gt: claim mail work "+id, func(tx beadsdk.Transaction) error {
		issue, message, work, err := loadMailWork(ctx, tx, id)
		if err != nil {
			return err
		}

		if issue.Status == beadsdk.StatusInProgress && work.Claim != nil &&
			work.Claim.Actor == actor && work.Claim.Generation.Equal(currentGeneration) {
			result = work
			return nil
		}
		if issue.Status != beadsdk.StatusOpen {
			return mailWorkConflict(id, issue.Status, work)
		}
		if message.ClaimedBy != "" || message.ClaimedAt != nil {
			return fmt.Errorf("%w: open work %s has claim labels", ErrMailWorkInvalid, id)
		}

		switch work.Route {
		case WorkRouteDirect:
			if !message.IsDirectMessage() || AddressToIdentity(message.To) != actor {
				return fmt.Errorf("%w: %s is assigned to %s", ErrMailWorkUnauthorized, id, message.To)
			}
		case WorkRouteQueue:
			if !message.IsQueueMessage() || eligible == nil || !eligible(message.Queue, actor) {
				return fmt.Errorf("%w: %s cannot claim queue %s", ErrMailWorkUnauthorized, actor, message.Queue)
			}
		default:
			return fmt.Errorf("%w: unsupported route %q", ErrMailWorkInvalid, work.Route)
		}

		work.Claim = &WorkClaim{Actor: actor, ClaimedAt: claimedAt, Generation: currentGeneration}
		if err := work.Validate(WorkStateInProgress); err != nil {
			return fmt.Errorf("%w: %v", ErrMailWorkInvalid, err)
		}
		metadata, err := EncodeMailWorkMetadata(issue.Metadata, work)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrMailWorkInvalid, err)
		}
		updates := map[string]interface{}{
			"status":   string(WorkStateInProgress),
			"metadata": metadata,
		}
		if work.Route == WorkRouteQueue {
			updates["assignee"] = actor
		}
		if err := tx.UpdateIssue(ctx, id, updates, actor); err != nil {
			return err
		}
		if err := tx.AddLabel(ctx, id, "claimed-by:"+actor, actor); err != nil {
			return err
		}
		if err := tx.AddLabel(ctx, id, "claimed-at:"+claimedAt.Format(time.RFC3339), actor); err != nil {
			return err
		}
		if err := tx.AddComment(ctx, id, actor, "mail work claimed"); err != nil {
			return err
		}
		result = work
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *MailWorkStore) Release(ctx context.Context, id, actor string, generation tmux.SessionGeneration) (*WorkMetadata, error) {
	return s.transitionOwned(ctx, id, actor, generation, WorkStateInProgress, WorkStateOpen, "released", func(tx beadsdk.Transaction, issue *beadsdk.Issue, message *Message, work *WorkMetadata) (map[string]interface{}, error) {
		work.Claim = nil
		updates := make(map[string]interface{})
		if work.Route == WorkRouteQueue {
			updates["assignee"] = "queue:" + message.Queue
		}
		for _, label := range issue.Labels {
			if strings.HasPrefix(label, "claimed-by:") || strings.HasPrefix(label, "claimed-at:") {
				if err := tx.RemoveLabel(ctx, id, label, actor); err != nil {
					return nil, err
				}
			}
		}
		return updates, nil
	})
}

func (s *MailWorkStore) Block(ctx context.Context, id, actor string, generation tmux.SessionGeneration, reason string) (*WorkMetadata, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, fmt.Errorf("%w: block reason is required", ErrMailWorkInvalid)
	}
	blockedAt := s.now().UTC().Truncate(time.Second)
	return s.transitionOwned(ctx, id, actor, generation, WorkStateInProgress, WorkStateBlocked, "blocked", func(_ beadsdk.Transaction, _ *beadsdk.Issue, _ *Message, work *WorkMetadata) (map[string]interface{}, error) {
		work.Blocked = &WorkBlock{Reason: reason, At: blockedAt}
		return nil, nil
	})
}

func (s *MailWorkStore) Resume(ctx context.Context, id, actor string, generation tmux.SessionGeneration) (*WorkMetadata, error) {
	return s.transitionOwned(ctx, id, actor, generation, WorkStateBlocked, WorkStateInProgress, "resumed", func(_ beadsdk.Transaction, _ *beadsdk.Issue, _ *Message, work *WorkMetadata) (map[string]interface{}, error) {
		work.Blocked = nil
		return nil, nil
	})
}

type ownedWorkMutation func(beadsdk.Transaction, *beadsdk.Issue, *Message, *WorkMetadata) (map[string]interface{}, error)

func (s *MailWorkStore) transitionOwned(ctx context.Context, id, actor string, generation tmux.SessionGeneration, from, to WorkState, event string, mutate ownedWorkMutation) (*WorkMetadata, error) {
	actor = AddressToIdentity(actor)
	currentGeneration := WorkGenerationFromTmux(generation)
	if err := currentGeneration.validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMailWorkInvalid, err)
	}
	var result *WorkMetadata
	err := s.runTransaction(ctx, "gt: "+event+" mail work "+id, func(tx beadsdk.Transaction) error {
		issue, message, work, err := loadMailWork(ctx, tx, id)
		if err != nil {
			return err
		}
		if WorkState(issue.Status) != from {
			return mailWorkConflict(id, issue.Status, work)
		}
		if work.Claim == nil || work.Claim.Actor != actor || !work.Claim.Generation.Equal(currentGeneration) {
			return mailWorkConflict(id, issue.Status, work)
		}
		updates, err := mutate(tx, issue, message, work)
		if err != nil {
			return err
		}
		if err := work.Validate(to); err != nil {
			return fmt.Errorf("%w: %v", ErrMailWorkInvalid, err)
		}
		metadata, err := EncodeMailWorkMetadata(issue.Metadata, work)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrMailWorkInvalid, err)
		}
		if updates == nil {
			updates = make(map[string]interface{})
		}
		updates["status"] = string(to)
		updates["metadata"] = metadata
		if err := tx.UpdateIssue(ctx, id, updates, actor); err != nil {
			return err
		}
		if err := tx.AddComment(ctx, id, actor, "mail work "+event); err != nil {
			return err
		}
		result = work
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *MailWorkStore) runTransaction(ctx context.Context, commitMsg string, fn func(beadsdk.Transaction) error) error {
	for attempt := 0; attempt < 3; attempt++ {
		err := s.store.RunInTransaction(ctx, commitMsg, fn)
		if err == nil || !isMailWorkSerializationConflict(err) || attempt == 2 {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

func isMailWorkSerializationConflict(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "serialization failure") ||
		strings.Contains(message, "try restarting transaction") ||
		strings.Contains(message, "optimistic lock") ||
		strings.Contains(message, "lock wait timeout")
}

func loadMailWork(ctx context.Context, tx beadsdk.Transaction, id string) (*beadsdk.Issue, *Message, *WorkMetadata, error) {
	issue, err := tx.GetIssue(ctx, id)
	if err != nil {
		return nil, nil, nil, err
	}
	if issue == nil {
		return nil, nil, nil, ErrMessageNotFound
	}
	labels, err := tx.GetLabels(ctx, id)
	if err != nil {
		return nil, nil, nil, err
	}
	issue.Labels = labels
	message := sdkIssueToMessage(issue)
	if message == nil || !message.IsActionableWork() {
		return nil, nil, nil, fmt.Errorf("%w: %s is not enrolled", ErrMailWorkInvalid, id)
	}
	if err := message.ValidateStored(); err != nil {
		return nil, nil, nil, fmt.Errorf("%w: %v", ErrMailWorkInvalid, err)
	}
	work, err := ParseMailWorkMetadata(issue.Metadata)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: %v", ErrMailWorkInvalid, err)
	}
	if err := work.Validate(WorkState(issue.Status)); err != nil {
		return nil, nil, nil, fmt.Errorf("%w: %v", ErrMailWorkInvalid, err)
	}
	wantRoute := WorkRouteDirect
	if message.IsQueueMessage() {
		wantRoute = WorkRouteQueue
	}
	if work.Route != wantRoute {
		return nil, nil, nil, fmt.Errorf("%w: metadata route %q does not match message route %q", ErrMailWorkInvalid, work.Route, wantRoute)
	}
	return issue, message, work, nil
}

func mailWorkConflict(id string, status beadsdk.Status, work *WorkMetadata) error {
	owner := "unclaimed"
	if work != nil && work.Claim != nil && work.Claim.Actor != "" {
		owner = work.Claim.Actor
	}
	return fmt.Errorf("%w: %s is %s owned by %s", ErrMailWorkConflict, id, status, owner)
}
