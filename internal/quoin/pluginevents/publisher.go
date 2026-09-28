// Package pluginevents is the Quoin runtime for the bounded post-commit
// plugin fact contract (ADR-0014, issue #110): the Publisher persists a fact
// row plus one delivery ledger row per enabled subscriber inside the SAME
// authority transaction that committed the domain fact; the Dispatcher — a
// process worker wired at Quoin startup with the deployment-enabled plugin
// set only — delivers strictly after that commit, outside every database
// write lock, at-least-once with per-(subscriber, event id) idempotency,
// bounded retry/backoff and an explicit deadletter replay path.
//
// Facts carry stable IDs, versions and bounded immutable references — never
// credentials or unbounded bodies. A subscriber receives only the typed fact:
// no database handle, no writer, no credentials, and no way to change the
// already-committed inbound adjudication. Without subscribers for a fact type
// the whole mechanism is a no-op: no rows, no worker.
package pluginevents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// Fact is the emission-side shape a domain service hands the Publisher
// inside its authority transaction. Type must be in the frozen vocabulary;
// Refs and Labels are bounded non-secret references and classification
// labels.
type Fact struct {
	Type   string
	Refs   []plugins.PostCommitFactRef
	Labels map[string]string
}

// Bounded-shape limits enforced at emit: violating them is a programming
// error that must fail the carrying transaction, never silently truncate a
// fact.
const (
	maxRefs         = 16
	maxLabels       = 16
	maxLabelValue   = 256
	maxRefsJSONSize = 4096
)

// Publisher persists committed facts for the enabled subscriber set. The
// subscriber index is the frozen plugin assembly filtered by the resolved
// deployment enablement — identical inputs to the Dispatcher, so enqueue and
// delivery can never disagree about who receives a fact. A nil Publisher or
// a type without subscribers makes Emit a no-op.
type Publisher struct {
	registry *plugins.Registry
	enabled  []string
	now      func() time.Time

	mu     sync.Mutex
	notify func()
}

// NewPublisher assembles the emit-side index over the frozen registry and
// the resolved deployment-enabled plugin IDs.
func NewPublisher(registry *plugins.Registry, enabled []string) *Publisher {
	return &Publisher{registry: registry, enabled: enabled, now: time.Now}
}

// SetNotifier installs the post-commit kick (the Dispatcher's wake signal).
// The notifier must never be called while a write transaction is open —
// domain services call it only after the runner committed.
func (publisher *Publisher) SetNotifier(notify func()) {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	publisher.notify = notify
}

// Notify fires the post-commit kick. Nil-publisher and nil-notifier safe.
func (publisher *Publisher) Notify() {
	if publisher == nil {
		return
	}
	publisher.mu.Lock()
	notify := publisher.notify
	publisher.mu.Unlock()
	if notify != nil {
		notify()
	}
}

// persistedRefs is the schema-gated non-secret refs_json document.
type persistedRefs struct {
	Refs   []plugins.PostCommitFactRef `json:"refs"`
	Labels map[string]string           `json:"labels,omitempty"`
}

// Emit persists one fact of the finite vocabulary plus its per-subscriber
// delivery rows on the caller's authority transaction — they commit or roll
// back together with the domain fact that raised them. Without enabled
// subscribers for the type it inserts nothing and reports event ID 0.
// Emit never performs I/O beyond the transaction and never blocks on a
// subscriber: delivery happens strictly after the commit.
func (publisher *Publisher) Emit(ctx context.Context, tx execution.Executor, fact Fact) (int64, error) {
	if publisher == nil {
		return 0, nil
	}
	if publisher.registry == nil || tx == nil {
		return 0, errors.New("pluginevents: publisher requires a registry and authority transaction")
	}
	if !plugins.ValidPostCommitFactType(fact.Type) {
		return 0, fmt.Errorf("pluginevents: fact type %q is outside the frozen vocabulary", fact.Type)
	}
	if len(fact.Refs) > maxRefs || len(fact.Labels) > maxLabels {
		return 0, fmt.Errorf("pluginevents: fact %s exceeds the bounded shape (%d refs, %d labels)", fact.Type, len(fact.Refs), len(fact.Labels))
	}
	for _, ref := range fact.Refs {
		if !validRefName(ref.Name) || ref.ID < 1 || ref.Version < 0 {
			return 0, fmt.Errorf("pluginevents: fact %s carries invalid ref %q", fact.Type, ref.Name)
		}
	}
	names := make([]string, 0, len(fact.Labels))
	for name, value := range fact.Labels {
		if !validRefName(name) || value == "" || len(value) > maxLabelValue {
			return 0, fmt.Errorf("pluginevents: fact %s carries invalid label %q", fact.Type, name)
		}
		names = append(names, name)
	}
	sort.Strings(names)

	subscribers := publisher.registry.PostCommitSubscribers(fact.Type, publisher.enabled)
	if len(subscribers) == 0 {
		// No subscriber, no rows: the optional event cannot burden the main
		// flow (ADR-0014).
		return 0, nil
	}
	document, err := json.Marshal(persistedRefs{Refs: fact.Refs, Labels: fact.Labels})
	if err != nil {
		return 0, fmt.Errorf("pluginevents: encode fact %s: %w", fact.Type, err)
	}
	if len(document) > maxRefsJSONSize {
		return 0, fmt.Errorf("pluginevents: fact %s refs document exceeds %d bytes", fact.Type, maxRefsJSONSize)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO plugin_events(event_type, payload_version, committed_at, refs_json) VALUES(?,?,?,?)`,
		fact.Type, plugins.PostCommitPayloadVersion, timestamp(publisher.now), string(document))
	if err != nil {
		return 0, fmt.Errorf("pluginevents: persist fact %s: %w", fact.Type, err)
	}
	eventID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("pluginevents: read fact id: %w", err)
	}
	for _, subscriber := range subscribers {
		if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_event_deliveries(event_id, subscriber_id, state, attempts, created_at) VALUES(?,?,'pending',0,?)`,
			eventID, subscriber.PluginID, timestamp(publisher.now)); err != nil {
			return 0, fmt.Errorf("pluginevents: persist delivery for %s: %w", subscriber.PluginID, err)
		}
	}
	return eventID, nil
}

// validRefName bounds reference and label names to a closed machine shape.
func validRefName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for index, char := range name {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z' && index > 0:
		case char >= '0' && char <= '9' && index > 0:
		case char == '_' && index > 0:
		default:
			return false
		}
	}
	return true
}

func timestamp(now func() time.Time) string {
	return now().UTC().Format(time.RFC3339Nano)
}
