package pluginevents

// Fake-subscriber coverage for the post-commit fact runtime (ADR-0014, issue
// #110): commit-before-delivery, same-transaction rollback, bounded retry
// then delivery, restart without redelivery, deadletter with explicit safe
// replay, disabled-subscriber deadlettering, and the no-subscriber no-op.

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/execution"
	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	_ "modernc.org/sqlite"
)

// recordingSubscriber is the fake subscriber: it records delivered facts and
// can fail a bounded number of leading invocations. Invocations counts every
// handler call (successful or not — at-least-once means retries re-invoke);
// facts records the successful deliveries.
type recordingSubscriber struct {
	mu          sync.Mutex
	invocations int
	facts       []plugins.PostCommitFact
	failFirst   int
	failWith    error
}

func (subscriber *recordingSubscriber) HandlePostCommitFact(_ context.Context, fact plugins.PostCommitFact) error {
	subscriber.mu.Lock()
	defer subscriber.mu.Unlock()
	subscriber.invocations++
	if subscriber.failFirst > 0 {
		subscriber.failFirst--
		if subscriber.failWith != nil {
			return subscriber.failWith
		}
		return errors.New("synthetic subscriber failure")
	}
	subscriber.facts = append(subscriber.facts, fact)
	return nil
}

func (subscriber *recordingSubscriber) calls() int {
	subscriber.mu.Lock()
	defer subscriber.mu.Unlock()
	return subscriber.invocations
}

func (subscriber *recordingSubscriber) last() plugins.PostCommitFact {
	subscriber.mu.Lock()
	defer subscriber.mu.Unlock()
	if len(subscriber.facts) == 0 {
		return plugins.PostCommitFact{}
	}
	return subscriber.facts[len(subscriber.facts)-1]
}

type fixture struct {
	db         *sql.DB
	publisher  *Publisher
	dispatcher *Dispatcher
	emitRunner *execution.Runner
	emitOp     *execution.Operation
	subscriber *recordingSubscriber
}

// newFixture assembles one plugin ("watcher") subscribed to the observation
// fact over a fresh schema, with fast deterministic retry bounds. The
// publisher and dispatcher enablement sets are passed separately so tests can
// model an enablement change between the enqueueing boot and the delivering
// boot.
func newFixture(t *testing.T, publisherEnabled, dispatcherEnabled []string) *fixture {
	t.Helper()
	databasePath := filepath.Join(t.TempDir(), "quoin.db")
	db, err := sql.Open("sqlite", "file:"+databasePath+"?_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	reader, err := execution.OpenReadOnly(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	registry := plugins.NewRegistry()
	subscriber := &recordingSubscriber{}
	if err := registry.Register(plugins.Plugin{
		ID: "watcher", Version: "v1-test",
		PostCommitSubscriptions: []plugins.PostCommitSubscription{{EventType: plugins.FactAlertObservationCommitted}},
		PostCommitHandler:       subscriber,
	}); err != nil {
		t.Fatal(err)
	}
	// A second plugin subscribed to the same fact but outside every fixture
	// enablement: its delivery rows exercise the disabled-subscriber path.
	if err := registry.Register(plugins.Plugin{
		ID: "dozing", Version: "v1-test",
		PostCommitSubscriptions: []plugins.PostCommitSubscription{{EventType: plugins.FactAlertObservationCommitted}},
		PostCommitHandler:       &recordingSubscriber{},
	}); err != nil {
		t.Fatal(err)
	}

	publisher := NewPublisher(registry, publisherEnabled)
	emitRunner := execution.NewRunner(db, execution.NewRegistry(), nil)
	emitOp, err := emitRunner.Register(execution.Operation{
		Name: "test.emit", Class: execution.ClassWrite, ObjectType: "plugin_event",
		Authorize: func(context.Context, *execution.Tx) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(db, reader, registry, dispatcherEnabled)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.pollInterval = time.Millisecond
	dispatcher.handlerTimeout = time.Second
	dispatcher.maxAttempts = 3
	dispatcher.backoffBase = time.Millisecond
	dispatcher.backoffMax = 2 * time.Millisecond
	dispatcher.batchSize = 10
	return &fixture{db: db, publisher: publisher, dispatcher: dispatcher, emitRunner: emitRunner, emitOp: emitOp, subscriber: subscriber}
}

// emit persists one fact through a runner-owned authority transaction, the
// same path the domain services use.
func (fixture *fixture) emit(t *testing.T, fact Fact) int64 {
	t.Helper()
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "0123456789abcdef0123456789abcdef",
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Initiator:     execution.Principal{Kind: execution.PrincipalSystem},
		Source:        execution.Source{Kind: execution.SourceInternal},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := execution.Execute(ctx, fixture.emitRunner, fixture.emitOp, func(tx *execution.Tx) (int64, error) {
		return fixture.publisher.Emit(ctx, tx, fact)
	}, func(id int64) int64 { return id })
	if err != nil {
		t.Fatalf("emit fact: %v", err)
	}
	return result
}

func sampleFact() Fact {
	return Fact{
		Type: plugins.FactAlertObservationCommitted,
		Refs: []plugins.PostCommitFactRef{
			{Name: "sourceId", ID: 3},
			{Name: "deliveryId", ID: 9},
			{Name: "occurrenceId", ID: 12, Version: 2},
			{Name: "observationId", ID: 15},
		},
		Labels: map[string]string{"state": "Firing", "effect": "initial_firing"},
	}
}

// drainUntil runs dispatch passes until the predicate holds or the deadline
// passes; every retry stays inside the worker's real drain path.
func drainUntil(t *testing.T, dispatcher *Dispatcher, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		dispatcher.drain(context.Background())
		if predicate() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("dispatch deadline passed before the predicate held")
}

func deliveryState(t *testing.T, db *sql.DB, eventID int64) (state string, attempts int64, lastError string) {
	t.Helper()
	err := db.QueryRowContext(context.Background(),
		`SELECT state, attempts, COALESCE(last_error,'') FROM plugin_event_deliveries WHERE event_id=? AND subscriber_id='watcher'`, eventID).
		Scan(&state, &attempts, &lastError)
	if err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	return state, attempts, lastError
}

func TestCommitBeforeDelivery(t *testing.T) {
	fixture := newFixture(t, []string{"watcher"}, []string{"watcher"})
	eventID := int64(0)
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "0123456789abcdef0123456789abcdef",
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Initiator:     execution.Principal{Kind: execution.PrincipalSystem},
		Source:        execution.Source{Kind: execution.SourceInternal},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = execution.Execute(ctx, fixture.emitRunner, fixture.emitOp, func(tx *execution.Tx) (int64, error) {
		id, emitErr := fixture.publisher.Emit(ctx, tx, sampleFact())
		// Inside the open authority transaction no subscriber has run: the
		// fact is durable-with-the-domain-write, never delivered early.
		if calls := fixture.subscriber.calls(); calls != 0 {
			return 0, errors.New("subscriber ran before commit")
		}
		return id, emitErr
	}, func(id int64) int64 { eventID = id; return id })
	if err != nil {
		t.Fatal(err)
	}
	if eventID == 0 {
		t.Fatal("fact row must persist for an enabled subscriber")
	}
	fixture.dispatcher.drain(context.Background())
	if fixture.subscriber.calls() != 1 {
		t.Fatalf("expected exactly one delivery, got %d", fixture.subscriber.calls())
	}
	fact := fixture.subscriber.last()
	if fact.ID != int64(eventID) || fact.Type != plugins.FactAlertObservationCommitted || fact.PayloadVersion != plugins.PostCommitPayloadVersion {
		t.Fatalf("fact identity mismatch: %+v", fact)
	}
	occurrence, ok := fact.Ref("occurrenceId")
	if !ok || occurrence.ID != 12 || occurrence.Version != 2 {
		t.Fatalf("versioned ref mismatch: %+v", fact.Refs)
	}
	if fact.Labels["effect"] != "initial_firing" || fact.Labels["state"] != "Firing" {
		t.Fatalf("labels mismatch: %+v", fact.Labels)
	}
	state, attempts, _ := deliveryState(t, fixture.db, eventID)
	if state != stateDelivered || attempts != 1 {
		t.Fatalf("delivery must settle delivered on first attempt, got %s/%d", state, attempts)
	}
}

func TestFactRollsBackWithItsTransaction(t *testing.T) {
	fixture := newFixture(t, []string{"watcher"}, []string{"watcher"})
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "0123456789abcdef0123456789abcdef",
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Source:        execution.Source{Kind: execution.SourceInternal},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = execution.Execute(ctx, fixture.emitRunner, fixture.emitOp, func(tx *execution.Tx) (int64, error) {
		if _, emitErr := fixture.publisher.Emit(ctx, tx, sampleFact()); emitErr != nil {
			return 0, emitErr
		}
		// The domain stage fails after emitting: the fact must share the
		// rollback — no orphaned event can outlive its domain write.
		return 0, errors.New("domain stage failed")
	}, func(id int64) int64 { return id })
	if err == nil {
		t.Fatal("expected the domain failure")
	}
	var events, deliveries int
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM plugin_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM plugin_event_deliveries`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if events != 0 || deliveries != 0 {
		t.Fatalf("fact must roll back with its transaction, got %d events / %d deliveries", events, deliveries)
	}
	fixture.dispatcher.drain(context.Background())
	if fixture.subscriber.calls() != 0 {
		t.Fatal("rolled-back fact must never be delivered")
	}
}

func TestBoundedRetryThenDelivered(t *testing.T) {
	fixture := newFixture(t, []string{"watcher"}, []string{"watcher"})
	fixture.subscriber.failFirst = 2
	eventID := fixture.emit(t, sampleFact())
	drainUntil(t, fixture.dispatcher, func() bool { return fixture.subscriber.calls() == 3 })
	state, attempts, lastError := deliveryState(t, fixture.db, eventID)
	if state != stateDelivered || attempts != 3 {
		t.Fatalf("expected delivered after 3 attempts, got %s/%d", state, attempts)
	}
	if lastError != "" {
		t.Fatalf("delivered row must clear its diagnostic, got %q", lastError)
	}
}

func TestRestartDoesNotRedeliver(t *testing.T) {
	fixture := newFixture(t, []string{"watcher"}, []string{"watcher"})
	eventID := fixture.emit(t, sampleFact())
	drainUntil(t, fixture.dispatcher, func() bool { return fixture.subscriber.calls() == 1 })

	// A new process boot rebuilds the dispatcher from the same durable state.
	restarted, err := NewDispatcher(fixture.db, readerOf(t, fixture), registryOf(t, fixture), []string{"watcher"})
	if err != nil {
		t.Fatal(err)
	}
	restarted.batchSize = 10
	restarted.drain(context.Background())
	if fixture.subscriber.calls() != 1 {
		t.Fatalf("delivered is terminal across restarts, calls=%d", fixture.subscriber.calls())
	}
	if _, attempts, _ := deliveryState(t, fixture.db, eventID); attempts != 1 {
		t.Fatalf("restart must not touch the delivered row, attempts=%d", attempts)
	}
}

func TestDeadletterThenExplicitReplay(t *testing.T) {
	fixture := newFixture(t, []string{"watcher"}, []string{"watcher"})
	fixture.dispatcher.maxAttempts = 2
	fixture.subscriber.failFirst = 100
	eventID := fixture.emit(t, sampleFact())
	drainUntil(t, fixture.dispatcher, func() bool {
		state, _, lastError := deliveryState(t, fixture.db, eventID)
		return state == stateDeadletter && lastError != ""
	})
	if fixture.subscriber.calls() != 2 {
		t.Fatalf("bounded retry must stop at the cap, calls=%d", fixture.subscriber.calls())
	}
	dead, err := fixture.dispatcher.DeadCount(context.Background())
	if err != nil || dead != 1 {
		t.Fatalf("deadletter backlog: %d (%v)", dead, err)
	}

	// Replay requeues the deadletter only; the handler now succeeds.
	fixture.subscriber.failFirst = 0
	if err := fixture.dispatcher.ReplayDeadletter(context.Background(), eventRowDeliveryID(t, fixture.db, eventID)); err != nil {
		t.Fatalf("replay deadletter: %v", err)
	}
	drainUntil(t, fixture.dispatcher, func() bool { return fixture.subscriber.calls() == 3 })
	state, attempts, _ := deliveryState(t, fixture.db, eventID)
	if state != stateDelivered || attempts != 1 {
		t.Fatalf("replayed delivery must settle delivered with reset attempts, got %s/%d", state, attempts)
	}

	// Delivered is terminal: replaying it is a deterministic rejection.
	if err := fixture.dispatcher.ReplayDeadletter(context.Background(), eventRowDeliveryID(t, fixture.db, eventID)); err == nil {
		t.Fatal("replaying a delivered row must fail")
	}
}

func TestDisabledSubscriberDeadletters(t *testing.T) {
	// Both subscribers were enabled when the fact was enqueued; the
	// delivering boot deploys only "watcher", so "dozing"'s durable delivery
	// row deadletters explicitly instead of lingering or vanishing.
	fixture := newFixture(t, []string{"watcher", "dozing"}, []string{"watcher"})
	eventID := fixture.emit(t, sampleFact())
	var watcherCalls, dozingDeliveries int
	drainUntil(t, fixture.dispatcher, func() bool {
		_ = fixture.db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM plugin_event_deliveries WHERE state='deadletter' AND subscriber_id='dozing'`).Scan(&dozingDeliveries)
		watcherCalls = fixture.subscriber.calls()
		return watcherCalls == 1 && dozingDeliveries == 1
	})
	if _, attempts, lastError := deliveryState(t, fixture.db, eventID); attempts != 1 || lastError != "" {
		t.Fatalf("enabled subscriber must deliver cleanly: %d/%q", attempts, lastError)
	}
	var dozingError string
	if err := fixture.db.QueryRowContext(context.Background(),
		`SELECT last_error FROM plugin_event_deliveries WHERE event_id=? AND subscriber_id='dozing'`, eventID).Scan(&dozingError); err != nil {
		t.Fatal(err)
	}
	if dozingError != errSubscriberMissing {
		t.Fatalf("disabled subscriber must deadletter as %q, got %q", errSubscriberMissing, dozingError)
	}
}

func TestNoSubscriberIsARowFreeNoOp(t *testing.T) {
	// No plugin in the assembly subscribes at all.
	databasePath := filepath.Join(t.TempDir(), "quoin.db")
	db, err := sql.Open("sqlite", "file:"+databasePath+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	reader, err := execution.OpenReadOnly(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	registry := plugins.NewRegistry()
	publisher := NewPublisher(registry, nil)
	runner := execution.NewRunner(db, execution.NewRegistry(), nil)
	op, err := runner.Register(execution.Operation{
		Name: "test.emit", Class: execution.ClassWrite, ObjectType: "plugin_event",
		Authorize: func(context.Context, *execution.Tx) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "0123456789abcdef0123456789abcdef",
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Source:        execution.Source{Kind: execution.SourceInternal},
	})
	if err != nil {
		t.Fatal(err)
	}
	var eventID int64
	if _, err := execution.Execute(ctx, runner, op, func(tx *execution.Tx) (int64, error) {
		return publisher.Emit(ctx, tx, sampleFact())
	}, func(id int64) int64 { eventID = id; return id }); err != nil {
		t.Fatal(err)
	}
	if eventID != 0 {
		t.Fatal("no subscriber must mean no event row")
	}
	var events int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM plugin_events`).Scan(&events); err != nil || events != 0 {
		t.Fatalf("no-subscriber emit must stay row-free (%d, %v)", events, err)
	}
	dispatcher, err := NewDispatcher(db, reader, registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { dispatcher.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dispatcher without subscribers must be an immediate no-op")
	}
}

func TestEmitRejectsUnboundedOrUnknownFacts(t *testing.T) {
	fixture := newFixture(t, []string{"watcher"}, []string{"watcher"})
	cases := map[string]Fact{
		"unknown type": {Type: "quoin.made.up"},
		"oversized label": {Type: plugins.FactAlertObservationCommitted, Labels: map[string]string{
			"state": string(make([]byte, maxLabelValue+1)),
		}},
		"invalid ref name": {Type: plugins.FactAlertObservationCommitted, Refs: []plugins.PostCommitFactRef{
			{Name: "9bad", ID: 1},
		}},
	}
	for name, fact := range cases {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("%s: emit must fail loudly, not panic: %v", name, recovered)
				}
			}()
			ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
				CorrelationID: "0123456789abcdef0123456789abcdef",
				Actor:         execution.Principal{Kind: execution.PrincipalSystem},
				Source:        execution.Source{Kind: execution.SourceInternal},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = execution.Execute(ctx, fixture.emitRunner, fixture.emitOp, func(tx *execution.Tx) (int64, error) {
				return fixture.publisher.Emit(ctx, tx, fact)
			}, func(id int64) int64 { return id })
			if err == nil {
				t.Fatalf("%s: emit must reject", name)
			}
		}()
	}
}

// TestDeliveredIsTerminal exercises the schema trigger: the delivered state
// can never regress, even from raw SQL — only deadletter rows replay.
func TestDeliveredIsTerminal(t *testing.T) {
	fixture := newFixture(t, []string{"watcher"}, []string{"watcher"})
	eventID := fixture.emit(t, sampleFact())
	drainUntil(t, fixture.dispatcher, func() bool { return fixture.subscriber.calls() == 1 })
	_, err := fixture.db.ExecContext(context.Background(),
		`UPDATE plugin_event_deliveries SET state='pending' WHERE event_id=? AND subscriber_id='watcher'`, eventID)
	if err == nil {
		t.Fatal("delivered must be terminal by schema trigger")
	}
}

func eventRowDeliveryID(t *testing.T, db *sql.DB, eventID int64) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRowContext(context.Background(),
		`SELECT id FROM plugin_event_deliveries WHERE event_id=? AND subscriber_id='watcher'`, eventID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// readerOf reopens the trusted read-only surface for a rebuilt dispatcher.
func readerOf(t *testing.T, fixture *fixture) audit.Reader {
	t.Helper()
	var path string
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT file FROM pragma_database_list WHERE seq=0`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	reader, err := execution.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	return reader
}

// registryOf rebuilds the fixture's registry: the plugin assembly is a
// compile-time fact, so a restarted dispatcher resolves the same handlers.
func registryOf(t *testing.T, fixture *fixture) *plugins.Registry {
	t.Helper()
	registry := plugins.NewRegistry()
	if err := registry.Register(plugins.Plugin{
		ID: "watcher", Version: "v1-test",
		PostCommitSubscriptions: []plugins.PostCommitSubscription{{EventType: plugins.FactAlertObservationCommitted}},
		PostCommitHandler:       fixture.subscriber,
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(plugins.Plugin{
		ID: "dozing", Version: "v1-test",
		PostCommitSubscriptions: []plugins.PostCommitSubscription{{EventType: plugins.FactAlertObservationCommitted}},
		PostCommitHandler:       &recordingSubscriber{},
	}); err != nil {
		t.Fatal(err)
	}
	return registry
}
