package investigation

// Transient model-delta feed (RUNTIME-AGENT-004, HTTP-STREAM-001..006):
// deltas are display-only projections — never message authority. The feed
// fans out one attempt's ordered deltas to its attached stream handlers,
// drops non-monotonic sequences and post-terminal deltas, and delivers the
// committed terminal view exactly once per subscriber. Transport detach
// only removes the observer; the attempt keeps running (HTTP-STREAM-006).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
)

// feedBuffer bounds one subscriber's in-flight events; a stalled reader
// drops further deltas (transient display only — the durable message and
// the terminal event remain authoritative).
const feedBuffer = 256

// Delta is one ordered visible model delta.
type Delta struct {
	ModelCallID int64
	DeltaSeq    uint64
	Text        string
}

// TerminalEvent is the committed terminal view delivered to stream
// subscribers after the durable transaction closes.
type TerminalEvent struct {
	State             string
	Content           string // assistant message content (Succeeded only)
	TerminationReason string
	InputTokens       int64
	OutputTokens      int64
}

// StreamEvent is one feed item: exactly one of Delta/Terminal is set.
type StreamEvent struct {
	Delta    *Delta
	Terminal *TerminalEvent
}

type feed struct {
	mu           sync.Mutex
	subscribers  map[chan StreamEvent]struct{}
	terminal     bool
	terminalView StreamEvent
	lastCall     int64
	lastSeq      uint64
}

// Subscribe attaches one stream handler to an attempt's feed. When the
// attempt is already terminal the buffered terminal view is delivered so
// the handler never misses the close. The returned release function
// removes the subscriber and drops an empty feed.
func (service *Service) Subscribe(ctx context.Context, attemptID int64) (<-chan StreamEvent, func(), error) {
	service.streamMu.Lock()
	defer service.streamMu.Unlock()
	current := service.streams[attemptID]
	if current == nil {
		current = &feed{subscribers: map[chan StreamEvent]struct{}{}}
		service.streams[attemptID] = current
	}
	channel := make(chan StreamEvent, feedBuffer)
	current.mu.Lock()
	current.subscribers[channel] = struct{}{}
	if current.terminal {
		channel <- current.terminalView
	}
	current.mu.Unlock()
	release := func() {
		service.streamMu.Lock()
		defer service.streamMu.Unlock()
		if feed, ok := service.streams[attemptID]; ok {
			feed.mu.Lock()
			delete(feed.subscribers, channel)
			empty := len(feed.subscribers) == 0
			feed.mu.Unlock()
			if empty {
				delete(service.streams, attemptID)
			}
		}
	}
	return channel, release, nil
}

// DeliverDelta fans one visible delta out to the attempt's observers.
// Unknown attempts (no observer), post-terminal deltas and non-monotonic
// delta sequences per model call are dropped (RUNTIME-AGENT-004).
func (service *Service) DeliverDelta(attemptID, modelCallID int64, deltaSeq uint64, text string) {
	service.streamMu.Lock()
	current := service.streams[attemptID]
	service.streamMu.Unlock()
	if current == nil || text == "" {
		return
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.terminal {
		return
	}
	if modelCallID != current.lastCall {
		current.lastCall = modelCallID
		current.lastSeq = 0
	}
	if deltaSeq <= current.lastSeq {
		return
	}
	current.lastSeq = deltaSeq
	event := StreamEvent{Delta: &Delta{ModelCallID: modelCallID, DeltaSeq: deltaSeq, Text: text}}
	for subscriber := range current.subscribers {
		select {
		case subscriber <- event:
		default:
			// Slow reader: deltas are transient; the terminal event and the
			// durable message remain the recovery path.
		}
	}
}

// NotifyTerminal loads the committed terminal view and broadcasts it to
// every observer of the attempt (no-op when the attempt is still active).
func (service *Service) NotifyTerminal(ctx context.Context, attemptID int64) {
	view, err := service.terminalView(ctx, attemptID)
	if err != nil {
		return
	}
	if view.State == "Running" || view.State == "Queued" || view.State == "Assigned" || view.State == "Cancelling" {
		return
	}
	event := StreamEvent{Terminal: &view}
	service.streamMu.Lock()
	current := service.streams[attemptID]
	if current == nil {
		current = &feed{subscribers: map[chan StreamEvent]struct{}{}}
		service.streams[attemptID] = current
	}
	service.streamMu.Unlock()
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.terminal {
		return
	}
	current.terminal = true
	current.terminalView = event
	for subscriber := range current.subscribers {
		select {
		case subscriber <- event:
		default:
		}
	}
}

// TerminalViewFor returns the committed terminal view of one attempt
// (replay path; ErrNotFound for unknown attempts, nil for still-active).
func (service *Service) TerminalViewFor(ctx context.Context, attemptID int64) (*TerminalEvent, error) {
	view, err := service.terminalView(ctx, attemptID)
	if err != nil {
		return nil, err
	}
	switch view.State {
	case "Succeeded", "Failed", "Cancelled", "Interrupted":
		return &view, nil
	default:
		return nil, nil
	}
}

// terminalView loads the authoritative terminal facts for one attempt.
func (service *Service) terminalView(ctx context.Context, attemptID int64) (TerminalEvent, error) {
	var view TerminalEvent
	var reason sql.NullString
	if err := service.db.QueryRowContext(ctx, `
		SELECT state, termination_reason FROM execution_attempts WHERE id=?`,
		attemptID).Scan(&view.State, &reason); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TerminalEvent{}, ErrNotFound
		}
		return TerminalEvent{}, err
	}
	view.TerminationReason = reason.String
	if view.State == "Succeeded" {
		if err := service.db.QueryRowContext(ctx, `
			SELECT content FROM investigation_messages WHERE attempt_id=? AND role='assistant'`,
			attemptID).Scan(&view.Content); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return TerminalEvent{}, err
		}
		var usageJSON sql.NullString
		if err := service.db.QueryRowContext(ctx, `
			SELECT usage_json FROM model_calls WHERE attempt_id=? AND status='succeeded'
			ORDER BY id DESC LIMIT 1`, attemptID).Scan(&usageJSON); err == nil && usageJSON.Valid {
			var usage struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			}
			if json.Unmarshal([]byte(usageJSON.String), &usage) == nil {
				view.InputTokens = usage.InputTokens
				view.OutputTokens = usage.OutputTokens
			}
		}
	}
	return view, nil
}
