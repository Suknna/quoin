package plugins

// Post-commit fact contract (ADR-0014 小核心事件插件). Quoin owns a FINITE
// vocabulary of committed business facts; a plugin may declare compile-time
// subscriptions to them. Delivery is strictly after the authority commit,
// at-least-once, with per-(subscriber, event id) idempotency on the Quoin
// side; a subscriber failure can never change the already-committed inbound
// adjudication or bypass Quoin's write commands.
//
// The contract stays free of Quoin domain dependencies: a fact carries only
// its stable identity, a bounded non-secret label set and stable versioned
// references. Never credentials, never raw payload bodies, never unbounded
// text.

import (
	"context"
	"fmt"
	"sort"
)

// The finite post-commit fact vocabulary (ADR-0014). A subscription naming
// anything else fails plugin registration — unknown facts are a programming
// error, never a silent no-op.
const (
	// FactInboundEventAccepted: the normalized inbound batch was committed as
	// one authoritative delivery, even when individual items carry issues.
	FactInboundEventAccepted = "quoin.ingress.event.accepted"
	// FactAlertObservationCommitted: one normalized alert observation and its
	// occurrence effect committed inside the intake transaction.
	FactAlertObservationCommitted = "quoin.alert.observation.committed"
	// FactInspectionDailyWindowDue: a daily report's frozen UTC window became
	// due and its Collecting report row committed at the trigger boundary.
	FactInspectionDailyWindowDue = "quoin.inspection.daily_window_due"
	// FactInspectionCheckEvidenceCommitted: one check result (success evidence
	// or explicit gap) committed for an inspection run.
	FactInspectionCheckEvidenceCommitted = "quoin.inspection.check_evidence.committed"
	// FactInspectionReportSealed: a daily report's immutable sealed version
	// committed.
	FactInspectionReportSealed = "quoin.inspection.report.sealed"
)

// PostCommitPayloadVersion is the payload generation every fact of the
// initial vocabulary carries. A future incompatible shape bumps the version
// per fact type; subscribers declare the vocabulary, delivery stays honest.
const PostCommitPayloadVersion = 1

// PostCommitFactRef is one stable, versioned reference inside a fact — a
// named row identity the subscriber can resolve through Quoin's stable read
// surfaces. Names are the fact type's documented contract (e.g.
// "occurrenceId"); ID is the committed row identity; Version is the row
// version at commit time (0 when the object carries no row version).
type PostCommitFactRef struct {
	Name    string
	ID      int64
	Version int64
}

// PostCommitFact is the bounded fact handed to a subscriber after commit.
// Labels are a small closed non-secret string set (effect/state/window
// bounds); every value is bounded by the publisher before enqueue.
type PostCommitFact struct {
	// ID is the stable event identity (plugin_events.id). Subscribers
	// deduplicate at-least-once redelivery by (their identity, ID).
	ID int64
	// Type is one of the Fact* vocabulary constants.
	Type string
	// PayloadVersion is the fact shape generation.
	PayloadVersion int
	// CommittedAt is the authority commit timestamp (RFC3339Nano UTC).
	CommittedAt string
	// Refs are the fact's stable versioned references.
	Refs []PostCommitFactRef
	// Labels are the bounded non-secret classification labels.
	Labels map[string]string
}

// Ref resolves one named reference.
func (fact PostCommitFact) Ref(name string) (PostCommitFactRef, bool) {
	for _, ref := range fact.Refs {
		if ref.Name == name {
			return ref, true
		}
	}
	return PostCommitFactRef{}, false
}

// PostCommitHandler is the after-commit subscriber capability. It runs
// outside every Quoin write transaction (never under a DB write lock), may be
// invoked more than once per fact (at-least-once) and must therefore
// deduplicate by fact ID. It receives only the bounded fact — no database
// handle, no credentials — and cannot influence the inbound adjudication
// that produced the fact. A returned error schedules bounded retry and, once
// exhausted, deadletter.
type PostCommitHandler interface {
	HandlePostCommitFact(ctx context.Context, fact PostCommitFact) error
}

// PostCommitSubscription declares one fact type a plugin consumes. The
// subscription list is part of the frozen plugin assembly: registration
// validates every type against the vocabulary and rejects duplicates.
type PostCommitSubscription struct {
	// EventType is one of the Fact* vocabulary constants.
	EventType string
}

// PostCommitSubscribers resolves the enabled after-commit subscribers of one
// fact type from the frozen assembly: entries are (plugin ID, handler) in
// stable plugin-ID order. Enablement filtering is the deployment decision the
// host applies by passing the resolved enabled-ID set; the returned handlers
// are the only invocation surface — never a database handle.
func (r *Registry) PostCommitSubscribers(eventType string, enabledIDs []string) []PostCommitSubscriberEntry {
	r.mu.Lock()
	r.ensureFrozen()
	enabled := make(map[string]bool, len(enabledIDs))
	for _, id := range enabledIDs {
		enabled[id] = true
	}
	var subscribers []PostCommitSubscriberEntry
	ids := append([]string(nil), r.order...)
	sort.Strings(ids)
	for _, id := range ids {
		plugin := r.plugins[id]
		if !enabled[id] || plugin.PostCommitHandler == nil {
			continue
		}
		for _, subscription := range plugin.PostCommitSubscriptions {
			if subscription.EventType == eventType {
				subscribers = append(subscribers, PostCommitSubscriberEntry{PluginID: id, Handler: plugin.PostCommitHandler})
				break
			}
		}
	}
	r.mu.Unlock()
	return subscribers
}

// PostCommitSubscriberEntry is one resolved subscriber of a fact type.
type PostCommitSubscriberEntry struct {
	PluginID string
	Handler  PostCommitHandler
}

// ValidPostCommitFactType reports whether the type is in the frozen
// vocabulary. Publishers and subscribers both fail closed on anything else.
func ValidPostCommitFactType(eventType string) bool { return validPostCommitFactType(eventType) }

// validPostCommitFactType reports whether the type is in the frozen
// vocabulary.
func validPostCommitFactType(eventType string) bool {
	switch eventType {
	case FactInboundEventAccepted,
		FactAlertObservationCommitted,
		FactInspectionDailyWindowDue,
		FactInspectionCheckEvidenceCommitted,
		FactInspectionReportSealed:
		return true
	}
	return false
}

// validatePostCommitDeclarations checks one plugin's subscription block in
// isolation (registry freeze re-checks cross-plugin invariants).
func validatePostCommitDeclarations(plugin Plugin) error {
	if plugin.PostCommitHandler == nil && len(plugin.PostCommitSubscriptions) == 0 {
		return nil
	}
	if plugin.PostCommitHandler == nil || len(plugin.PostCommitSubscriptions) == 0 {
		return fmt.Errorf("%w: %s must declare post-commit subscriptions and their handler together", ErrInvalidPlugin, plugin.ID)
	}
	seen := map[string]bool{}
	for _, subscription := range plugin.PostCommitSubscriptions {
		if !validPostCommitFactType(subscription.EventType) {
			return fmt.Errorf("%w: %s subscribes to unknown post-commit fact %q", ErrInvalidPlugin, plugin.ID, subscription.EventType)
		}
		if seen[subscription.EventType] {
			return fmt.Errorf("%w: %s declares duplicate post-commit subscription %q", ErrInvalidPlugin, plugin.ID, subscription.EventType)
		}
		seen[subscription.EventType] = true
	}
	return nil
}
