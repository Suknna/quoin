package pluginevents

// The Dispatcher is the process worker Quoin wires at startup with the
// deployment-enabled plugin set only (ADR-0014). It delivers committed facts
// strictly after their authority commit: handler invocation happens outside
// every database write transaction — the handler holds no Tx, no reader and
// no credentials. Delivery is at-least-once: a crash between a successful
// handler call and the delivered ledger write replays the fact, and
// subscribers deduplicate by (their plugin ID, fact ID). Failures retry a
// bounded number of times with exponential backoff, then deadletter; only
// deadlettered deliveries can be explicitly replayed. Delivered is terminal.
//
// Without enabled subscribers the worker exits immediately and state writes
// never run: the mechanism is a no-op, never a hidden queue consumer.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// Delivery states mirror the frozen plugin_event_deliveries CHECK vocabulary.
const (
	statePending    = "pending"
	stateDelivered  = "delivered"
	stateDeadletter = "deadletter"
)

// Stable machine codes for the bounded last_error classification. Handler
// error text is truncated before persistence: ledger columns never carry an
// unbounded diagnostic.
const (
	errSubscriberMissing = "subscriber_not_enabled"
	errHandlerFailed     = "handler_failed"
)

// Dispatcher defaults; tests shorten them for deterministic bounds.
const (
	defaultPollInterval   = 2 * time.Second
	defaultHandlerTimeout = 30 * time.Second
	defaultMaxAttempts    = 5
	defaultBackoffBase    = time.Second
	defaultBackoffMax     = time.Minute
	defaultBatchSize      = 100
)

// maxErrorText bounds the persisted last_error (schema CHECK: 1024).
const maxErrorText = 512

// dueDelivery is one claimed pending delivery with its fact's frozen row.
type dueDelivery struct {
	deliveryID   int64
	subscriberID string
	attempts     int64
	fact         plugins.PostCommitFact
}

// Dispatcher delivers committed plugin facts to their enabled subscribers.
type Dispatcher struct {
	runner   *execution.Runner
	registry *plugins.Registry
	handlers map[string]plugins.PostCommitHandler
	ops      dispatcherOperations
	now      func() time.Time

	pollInterval   time.Duration
	handlerTimeout time.Duration
	maxAttempts    int
	backoffBase    time.Duration
	backoffMax     time.Duration
	batchSize      int

	kick chan struct{}
}

type dispatcherOperations struct {
	deliver     *execution.Operation
	replay      *execution.Operation
	replayAdmin *execution.Operation
}

// NewDispatcher assembles the delivery worker over the authority database,
// its read-only reader, the frozen plugin registry and the resolved
// deployment-enabled plugin IDs. The reader is installed through the same
// trusted factory as every domain service; the dispatcher owns a private
// operation registry because its ledger writes are its own domain.
func NewDispatcher(db *sql.DB, reader audit.Reader, registry *plugins.Registry, enabled []string) (*Dispatcher, error) {
	if db == nil {
		return nil, errors.New("pluginevents: database is required")
	}
	if reader == nil {
		return nil, errors.New("pluginevents: read capability is required")
	}
	if registry == nil {
		return nil, errors.New("pluginevents: plugin registry is required")
	}
	runner := execution.NewRunner(db, execution.NewRegistry(), nil)
	if err := runner.SetReader(reader); err != nil {
		return nil, fmt.Errorf("pluginevents: install read-only reader: %w", err)
	}
	ops, err := registerDispatcherOperations(runner)
	if err != nil {
		return nil, err
	}
	handlers := map[string]plugins.PostCommitHandler{}
	for _, factType := range []string{
		plugins.FactAlertObservationCommitted,
		plugins.FactInspectionDailyWindowDue,
		plugins.FactInspectionCheckEvidenceCommitted,
		plugins.FactInspectionReportSealed,
	} {
		for _, subscriber := range registry.PostCommitSubscribers(factType, enabled) {
			if _, exists := handlers[subscriber.PluginID]; exists {
				continue
			}
			handlers[subscriber.PluginID] = subscriber.Handler
		}
	}
	return &Dispatcher{
		runner: runner, registry: registry, handlers: handlers, ops: ops, now: time.Now,
		pollInterval: defaultPollInterval, handlerTimeout: defaultHandlerTimeout,
		maxAttempts: defaultMaxAttempts, backoffBase: defaultBackoffBase, backoffMax: defaultBackoffMax,
		batchSize: defaultBatchSize,
		kick:      make(chan struct{}, 1),
	}, nil
}

// registerDispatcherOperations declares the delivery-ledger writes on the
// dispatcher's own registry: two modules can never silently claim one
// operation identity, and every mutation stays an audited runner execution.
func registerDispatcherOperations(runner *execution.Runner) (dispatcherOperations, error) {
	ops := dispatcherOperations{}
	declare := func(name string, authorize func(context.Context, *execution.Tx) error, target **execution.Operation) error {
		op, err := runner.Register(execution.Operation{
			Name: name, Class: execution.ClassWrite, ObjectType: "plugin_event_delivery",
			Authorize: authorize,
		})
		if err != nil {
			return fmt.Errorf("pluginevents: register %s: %w", name, err)
		}
		*target = op
		return nil
	}
	if err := declare("plugin.event.deliver", authorizeSystemDelivery, &ops.deliver); err != nil {
		return ops, err
	}
	if err := declare("plugin.event.replay", authorizeSystemDelivery, &ops.replay); err != nil {
		return ops, err
	}
	if err := declare("plugin.event.replay_admin", func(ctx context.Context, tx *execution.Tx) error {
		return auth.VerifyExecutionSession(ctx, tx, "admin")
	}, &ops.replayAdmin); err != nil {
		return ops, err
	}
	return ops, nil
}

// authorizeSystemDelivery confines the delivery ledger to the system
// principal arriving through a deployment-internal entry — the same shape the
// alerts and inspection machine entries use. A user or HTTP scope can never
// drive delivery state, and there is no session fallback to fake it.
func authorizeSystemDelivery(ctx context.Context, _ *execution.Tx) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
		return fmt.Errorf("pluginevents: delivery work requires the system principal, got %s/%d", meta.Actor.Kind, meta.Actor.ID)
	}
	if meta.Source.Kind == execution.SourceHTTP {
		return fmt.Errorf("pluginevents: delivery work cannot arrive from the %s channel", meta.Source.Kind)
	}
	return nil
}

// systemContext establishes the dispatcher's receiver-local execution scope
// for its ledger writes (the machine-entry pattern of the alerts service).
func (dispatcher *Dispatcher) systemContext() (context.Context, error) {
	correlationID, err := execution.NewCorrelationID()
	if err != nil {
		return nil, err
	}
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: correlationID,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Initiator:     execution.Principal{Kind: execution.PrincipalSystem},
		Source:        execution.Source{Kind: execution.SourceInternal},
	})
	if err != nil {
		return nil, err
	}
	return ctx, nil
}

// Run owns the worker lifecycle until ctx ends. Without enabled subscribers
// it is a deliberate no-op.
func (dispatcher *Dispatcher) Run(ctx context.Context) {
	if len(dispatcher.handlers) == 0 {
		return
	}
	// Catch up on deliveries that committed while this process was down.
	dispatcher.drain(ctx)
	ticker := time.NewTicker(dispatcher.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-dispatcher.kick:
		case <-ticker.C:
		}
		dispatcher.drain(ctx)
	}
}

// Kick requests one dispatch pass. Non-blocking: a pass is already pending or
// the poll tick will cover it.
func (dispatcher *Dispatcher) Kick() {
	select {
	case dispatcher.kick <- struct{}{}:
	default:
	}
}

// Drain runs exactly one dispatch pass over the due deliveries. The worker
// loop uses it internally; it is exported as the deterministic seam for
// tests and future administrative reconcile paths — it never bypasses the
// bounded retry, deadletter or idempotency rules of the worker.
func (dispatcher *Dispatcher) Drain(ctx context.Context) {
	dispatcher.drain(ctx)
}

// drain claims and delivers every due batch until none remains. One pass
// serializes deliveries in commit order (plugin_events.id); a handler failure
// never withholds the remaining facts — each delivery carries its own state.
func (dispatcher *Dispatcher) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		deliveries, err := dispatcher.due(ctx)
		if err != nil {
			sharedops.LogEvent("quoin", "error", "plugin_events.due_scan_failed", err.Error())
			return
		}
		if len(deliveries) == 0 {
			return
		}
		for _, delivery := range deliveries {
			if ctx.Err() != nil {
				return
			}
			dispatcher.deliver(ctx, delivery)
		}
	}
}

// due reads the due pending deliveries with their frozen facts through the
// trusted read-only surface. The dispatcher is the single worker of its
// process, so a read-then-act claim needs no second lease table.
func (dispatcher *Dispatcher) due(ctx context.Context) ([]dueDelivery, error) {
	rows, err := dispatcher.runner.Reader().QueryContext(ctx, `
		SELECT d.id, d.subscriber_id, d.attempts, e.id, e.event_type, e.payload_version, e.committed_at, e.refs_json
		FROM plugin_event_deliveries d JOIN plugin_events e ON e.id = d.event_id
		WHERE d.state = 'pending' AND (d.next_attempt_at IS NULL OR d.next_attempt_at <= ?)
		ORDER BY d.id LIMIT ?`, timestamp(dispatcher.now), dispatcher.batchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deliveries := []dueDelivery{}
	for rows.Next() {
		var delivery dueDelivery
		var document string
		if err := rows.Scan(&delivery.deliveryID, &delivery.subscriberID, &delivery.attempts,
			&delivery.fact.ID, &delivery.fact.Type, &delivery.fact.PayloadVersion, &delivery.fact.CommittedAt, &document); err != nil {
			return nil, err
		}
		var persisted persistedRefs
		if err := json.Unmarshal([]byte(document), &persisted); err != nil {
			return nil, fmt.Errorf("pluginevents: decode fact %d refs: %w", delivery.fact.ID, err)
		}
		delivery.fact.Refs, delivery.fact.Labels = persisted.Refs, persisted.Labels
		deliveries = append(deliveries, delivery)
	}
	return deliveries, rows.Err()
}

// deliver performs one at-least-once delivery attempt outside any write
// transaction. The handler timeout is detached from the worker context so an
// in-flight delivery is never torn by a shutdown race; the ledger state it
// transitions is its own short audited execution.
func (dispatcher *Dispatcher) deliver(ctx context.Context, delivery dueDelivery) {
	handler, enabled := dispatcher.handlers[delivery.subscriberID]
	if !enabled || handler == nil {
		// The subscriber's enablement changed between the enqueueing boot and
		// this one: the fact stays durable and explicitly deadlettered, never
		// silently dropped (deployment-enabled plugins only).
		dispatcher.transition(ctx, delivery, stateDeadletter, errSubscriberMissing)
		return
	}
	handlerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dispatcher.handlerTimeout)
	defer cancel()
	err := handler.HandlePostCommitFact(handlerCtx, delivery.fact)
	if err == nil {
		dispatcher.transition(ctx, delivery, stateDelivered, "")
		return
	}
	if delivery.attempts+1 >= int64(dispatcher.maxAttempts) {
		dispatcher.transition(ctx, delivery, stateDeadletter, errHandlerFailed)
		return
	}
	dispatcher.transition(ctx, delivery, statePending, err.Error())
}

// transition persists one delivery-state change as a short audited runner
// execution on the system scope — never inside a handler call, never holding
// a business write lock across subscriber code.
func (dispatcher *Dispatcher) transition(ctx context.Context, delivery dueDelivery, state, handlerError string) {
	workCtx, err := dispatcher.systemContext()
	if err != nil {
		sharedops.LogEvent("quoin", "error", "plugin_events.scope_failed", err.Error())
		return
	}
	_, err = execution.Execute(workCtx, dispatcher.runner, dispatcher.ops.deliver, func(tx *execution.Tx) (bool, error) {
		result, execErr := tx.ExecContext(workCtx, `
			UPDATE plugin_event_deliveries
			SET state=?, attempts=?, next_attempt_at=?, last_error=?, delivered_at=?
			WHERE id=? AND state='pending'`,
			state, delivery.attempts+1, dispatcher.nextAttemptAt(state, delivery.attempts+1),
			boundErrorText(handlerError), dispatcher.deliveredAt(state), delivery.deliveryID)
		if execErr != nil {
			return false, execErr
		}
		if affected, affectedErr := result.RowsAffected(); affectedErr != nil {
			return false, affectedErr
		} else if affected == 0 {
			// Another path (explicit replay) owns the row now: a missed
			// transition is not a failure fact.
			return false, execution.ErrNoTransition
		}
		return true, nil
	}, func(bool) int64 { return delivery.deliveryID })
	if err != nil && !errors.Is(err, execution.ErrNoTransition) {
		sharedops.LogEvent("quoin", "error", "plugin_events.transition_failed", err.Error())
	}
}

// nextAttemptAt resolves the due time of the new state: delivered and
// deadlettered rows are due-never; a retry backs off exponentially and stays
// inside the bounded cap.
func (dispatcher *Dispatcher) nextAttemptAt(state string, attempts int64) any {
	if state != statePending {
		return nil
	}
	delay := dispatcher.backoffBase << (attempts - 1)
	if delay > dispatcher.backoffMax || delay <= 0 {
		delay = dispatcher.backoffMax
	}
	return timestamp(func() time.Time { return dispatcher.now().Add(delay) })
}

func (dispatcher *Dispatcher) deliveredAt(state string) any {
	if state == stateDelivered {
		return timestamp(dispatcher.now)
	}
	return nil
}

// boundErrorText keeps the persisted diagnostic non-secret-by-contract and
// within the schema bound: handlers return machine errors, and the stable
// classification code replaces any over-long text.
func boundErrorText(text string) any {
	if text == "" {
		return nil
	}
	text = strings.ToValidUTF8(text, "\uFFFD")
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if len(text) > maxErrorText {
		return errHandlerFailed
	}
	return text
}

// ReplayDeadletter explicitly requeues one deadlettered delivery: attempts
// reset, backoff cleared, state back to pending. Delivered deliveries are
// terminal and pending ones are still being retried — replaying them is a
// deterministic rejection, never a silent duplicate state rewrite.
func (dispatcher *Dispatcher) ReplayDeadletter(ctx context.Context, deliveryID int64) error {
	workCtx, err := dispatcher.systemContext()
	if err != nil {
		return err
	}
	return dispatcher.replayDeadletterWith(workCtx, dispatcher.ops.replay, deliveryID)
}

// ReplayDeadletterAsAdmin rechecks a live administrator session in the same
// transaction as the replay. The audit actor remains the human who chose the
// replay; it is never silently rewritten as the system principal.
func (dispatcher *Dispatcher) ReplayDeadletterAsAdmin(ctx context.Context, deliveryID int64) error {
	return dispatcher.replayDeadletterWith(ctx, dispatcher.ops.replayAdmin, deliveryID)
}

func (dispatcher *Dispatcher) replayDeadletterWith(ctx context.Context, op *execution.Operation, deliveryID int64) error {
	_, err := execution.Execute(ctx, dispatcher.runner, op, func(tx *execution.Tx) (bool, error) {
		result, execErr := tx.ExecContext(ctx, `
			UPDATE plugin_event_deliveries
			SET state='pending', attempts=0, next_attempt_at=NULL
			WHERE id=? AND state='deadletter'`, deliveryID)
		if execErr != nil {
			return false, execErr
		}
		affected, affectedErr := result.RowsAffected()
		if affectedErr != nil {
			return false, affectedErr
		}
		if affected == 0 {
			var state string
			stateErr := tx.QueryRowContext(ctx, `SELECT state FROM plugin_event_deliveries WHERE id=?`, deliveryID).Scan(&state)
			if errors.Is(stateErr, sql.ErrNoRows) {
				return false, &execution.Rejection{Code: "delivery_not_found", Detail: "no plugin event delivery carries this id", ObjectID: deliveryID}
			}
			if stateErr != nil {
				return false, stateErr
			}
			return false, &execution.Rejection{Code: "delivery_not_deadletter", Detail: "only deadlettered deliveries can be replayed, state is " + state, ObjectID: deliveryID}
		}
		return true, nil
	}, func(bool) int64 { return deliveryID })
	return err
}

// DeadCount reports the current deadletter backlog (diagnostic projection).
func (dispatcher *Dispatcher) DeadCount(ctx context.Context) (int64, error) {
	var count int64
	err := dispatcher.runner.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_event_deliveries WHERE state='deadletter'`).Scan(&count)
	return count, err
}

// DeadDeliveries lists the deadletter backlog in stable order for the
// diagnostic surface.
func (dispatcher *Dispatcher) DeadDeliveries(ctx context.Context, limit int) ([]DeadDelivery, error) {
	rows, err := dispatcher.runner.Reader().QueryContext(ctx, `
		SELECT id, event_id, subscriber_id, attempts, COALESCE(last_error,'')
		FROM plugin_event_deliveries WHERE state='deadletter' ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deliveries := []DeadDelivery{}
	for rows.Next() {
		var delivery DeadDelivery
		if err := rows.Scan(&delivery.DeliveryID, &delivery.EventID, &delivery.SubscriberID, &delivery.Attempts, &delivery.LastError); err != nil {
			return nil, err
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, rows.Err()
}

// DeadDelivery is one deadlettered delivery projection.
type DeadDelivery struct {
	DeliveryID   int64  `json:"deliveryId"`
	EventID      int64  `json:"eventId"`
	SubscriberID string `json:"subscriberId"`
	Attempts     int64  `json:"attempts"`
	LastError    string `json:"lastError"`
}
