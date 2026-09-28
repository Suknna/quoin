package alerts

// Post-commit fact emission coverage for the intake authority transaction
// (ADR-0014, issue #110): a committed normalized observation persists its
// bounded fact in the SAME transaction, delivery to the fake subscriber
// happens only after the commit (through the dispatcher), relay replays emit
// no duplicate fact, and rejected deliveries emit nothing.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/pluginevents"
)

// factRecorder is the fake subscriber of this package's emission tests.
type factRecorder struct {
	calls int
	facts []plugins.PostCommitFact
}

func (recorder *factRecorder) HandlePostCommitFact(_ context.Context, fact plugins.PostCommitFact) error {
	recorder.calls++
	recorder.facts = append(recorder.facts, fact)
	return nil
}

func TestDeliveryEmitsObservationFactAfterCommit(t *testing.T) {
	service, database, teardown := newTestService(t)
	defer teardown()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "facts-am")

	subscriber := &factRecorder{}
	registry := plugins.NewRegistry()
	if err := registry.Register(plugins.Plugin{
		ID: "hooky", Version: "v1-test",
		PostCommitSubscriptions: []plugins.PostCommitSubscription{{EventType: plugins.FactAlertObservationCommitted}},
		PostCommitHandler:       subscriber,
	}); err != nil {
		t.Fatal(err)
	}
	publisher := pluginevents.NewPublisher(registry, []string{"hooky"})
	service.SetPostCommitPublisher(publisher)
	dispatcher, err := pluginevents.NewDispatcher(database.SQL, database.Reader, registry, []string{"hooky"})
	if err != nil {
		t.Fatal(err)
	}

	body := webhookBody("firing", map[string]string{"alertname": "CPU"}, "2026-08-17T10:00:00Z", "")
	result, err := service.Deliver(ctx, "relay-fact", sourceID, credentialID, 1, body, time.Now().UTC())
	if err != nil || !result.Accepted {
		t.Fatalf("deliver err=%v result=%+v", err, result)
	}
	// Nothing delivers before an explicit dispatch pass: the fact is durable
	// with the delivery, never handled mid-transaction.
	if subscriber.calls != 0 {
		t.Fatalf("subscriber ran before dispatch: %d", subscriber.calls)
	}
	var events int
	if err := database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_events WHERE event_type=?`, plugins.FactAlertObservationCommitted).Scan(&events); err != nil || events != 1 {
		t.Fatalf("fact rows=%d err=%v", events, err)
	}
	var refsJSON string
	if err := database.SQL.QueryRowContext(ctx, `SELECT refs_json FROM plugin_events`).Scan(&refsJSON); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sourceId", "deliveryId", "occurrenceId", "observationId", "initial_firing"} {
		if !strings.Contains(refsJSON, name) {
			t.Fatalf("refs document misses %q: %s", name, refsJSON)
		}
	}
	if strings.Contains(refsJSON, "annotations") || strings.Contains(refsJSON, "alertname") {
		t.Fatalf("fact must stay reference-bounded: %s", refsJSON)
	}
	var deliveries int
	if err := database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_event_deliveries WHERE state='pending'`).Scan(&deliveries); err != nil || deliveries != 1 {
		t.Fatalf("delivery rows=%d err=%v", deliveries, err)
	}

	dispatcher.Drain(ctx)
	if subscriber.calls != 1 {
		t.Fatalf("subscriber calls=%d, want 1", subscriber.calls)
	}
	occurrence, ok := subscriber.facts[0].Ref("deliveryId")
	if !ok || occurrence.ID != result.DeliveryID {
		t.Fatalf("delivered fact must reference the committed delivery: %+v", subscriber.facts[0].Refs)
	}
}

func TestRelayReplayEmitsNoDuplicateFact(t *testing.T) {
	service, database, teardown := newTestService(t)
	defer teardown()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "replay-facts-am")

	registry := plugins.NewRegistry()
	if err := registry.Register(plugins.Plugin{
		ID: "hooky", Version: "v1-test",
		PostCommitSubscriptions: []plugins.PostCommitSubscription{{EventType: plugins.FactAlertObservationCommitted}},
		PostCommitHandler:       &factRecorder{},
	}); err != nil {
		t.Fatal(err)
	}
	service.SetPostCommitPublisher(pluginevents.NewPublisher(registry, []string{"hooky"}))

	body := webhookBody("firing", map[string]string{"alertname": "CPU"}, "2026-08-17T10:00:00Z", "")
	received := time.Now().UTC()
	if _, err := service.Deliver(ctx, "relay-replay", sourceID, credentialID, 1, body, received); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Deliver(ctx, "relay-replay", sourceID, credentialID, 1, body, received); err != nil {
		t.Fatal(err)
	}
	var events int
	if err := database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_events`).Scan(&events); err != nil || events != 1 {
		t.Fatalf("relay replay must not duplicate facts, rows=%d err=%v", events, err)
	}
}

func TestRejectedDeliveryEmitsNoFact(t *testing.T) {
	service, database, teardown := newTestService(t)
	defer teardown()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "rejected-facts-am")

	registry := plugins.NewRegistry()
	if err := registry.Register(plugins.Plugin{
		ID: "hooky", Version: "v1-test",
		PostCommitSubscriptions: []plugins.PostCommitSubscription{{EventType: plugins.FactAlertObservationCommitted}},
		PostCommitHandler:       &factRecorder{},
	}); err != nil {
		t.Fatal(err)
	}
	service.SetPostCommitPublisher(pluginevents.NewPublisher(registry, []string{"hooky"}))

	body := []byte(`{not-json`)
	if result, err := service.Deliver(ctx, "relay-bad", sourceID, credentialID, 1, body, time.Now().UTC()); err != nil || !result.Rejected {
		t.Fatalf("deliver err=%v result=%+v", err, result)
	}
	var events int
	if err := database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_events`).Scan(&events); err != nil || events != 0 {
		t.Fatalf("rejected delivery must emit no fact, rows=%d err=%v", events, err)
	}
}
