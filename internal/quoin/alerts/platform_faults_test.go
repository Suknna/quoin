package alerts

import (
	"context"
	"testing"
)

func TestExecutionFaultLifecycleUsesAttemptOrderAndTerminalRecovery(t *testing.T) {
	service, _, cleanup := newTestService(t)
	defer cleanup()
	reporter := NewPlatformFaultReporter(service)
	ctx := context.Background()

	// The storage contract itself rejects unknown reasons. The reporter has a
	// second guard so business/input failures cannot reach that boundary.
	if _, err := service.db.ExecContext(ctx, `INSERT INTO platform_faults(component,reason,state,first_seen_at,last_seen_at) VALUES('plinth','invalid_response','Firing','2026-09-10T00:00:00Z','2026-09-10T00:00:00Z')`); err == nil {
		t.Fatal("platform fault reason contract accepted business failure")
	}
	// Only frozen Plinth worker failures are platform facts. Business/input
	// failures must not enter the platform source at all.
	if err := reporter.ObserveExecutionOutcome(ctx, 10, false, "invalid_response"); err != nil {
		t.Fatal(err)
	}
	if firing, err := service.AlertSnapshot(ctx, "Firing", ""); err != nil || len(firing.Items) != 0 {
		t.Fatalf("business failure fabricated platform fault: %+v %v", firing.Items, err)
	}

	if err := reporter.ObserveExecutionOutcome(ctx, 11, false, "worker_protocol_error"); err != nil {
		t.Fatal(err)
	}
	// Creation order is not terminal commit order. A later-assigned terminal
	// sequence still wins and may reopen the lifecycle after a prior success.
	if err := reporter.ObserveExecutionOutcome(ctx, 12, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := reporter.ObserveExecutionOutcome(ctx, 13, false, "worker_protocol_error"); err != nil {
		t.Fatal(err)
	}
	firing, err := service.AlertSnapshot(ctx, "Firing", "")
	if err != nil || len(firing.Items) != 1 || firing.Items[0].Reason != "worker_protocol_error" {
		t.Fatalf("execution failure not projected: %+v %v", firing.Items, err)
	}
	faultID := firing.Items[0].ID

	// A later successful terminal execution is the recovery authority. Stream
	// heartbeats never call this lifecycle, and an older failure replay cannot
	// revive the already-resolved lifecycle.
	if err := reporter.ObserveExecutionOutcome(ctx, 14, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := reporter.ObserveExecutionOutcome(ctx, 11, false, "worker_protocol_error"); err != nil {
		t.Fatal(err)
	}
	firing, err = service.AlertSnapshot(ctx, "Firing", "")
	if err != nil || len(firing.Items) != 0 {
		t.Fatalf("late failure replay revived fault: %+v %v", firing.Items, err)
	}
	resolved, err := service.AlertSnapshot(ctx, "Resolved", "")
	closedCurrent := false
	for _, item := range resolved.Items {
		closedCurrent = closedCurrent || (item.ID == faultID && item.ResolvedAt != nil)
	}
	if err != nil || !closedCurrent {
		t.Fatalf("successful execution did not close current lifecycle: %+v %v", resolved.Items, err)
	}

	// A new failure has a newer authoritative identity and starts exactly one
	// new lifecycle; its duplicate delivery remains diagnostic-only.
	if err := reporter.ObserveExecutionOutcome(ctx, 15, false, "worker_protocol_error"); err != nil {
		t.Fatal(err)
	}
	if err := reporter.ObserveExecutionOutcome(ctx, 15, false, "worker_protocol_error"); err != nil {
		t.Fatal(err)
	}
	firing, err = service.AlertSnapshot(ctx, "Firing", "")
	if err != nil || len(firing.Items) != 1 || firing.Items[0].ID == faultID {
		t.Fatalf("new failure did not create one distinct lifecycle: %+v %v", firing.Items, err)
	}
}

func TestPlatformFaultLifecycleDeduplicatesAndResolves(t *testing.T) {
	service, _, cleanup := newTestService(t)
	defer cleanup()
	reporter := NewPlatformFaultReporter(service)
	ctx := context.Background()
	if err := reporter.ObserveRuntimeConnection(ctx, "plinth", false); err != nil {
		t.Fatal(err)
	}
	if err := reporter.ObserveRuntimeConnection(ctx, "plinth", false); err != nil {
		t.Fatal(err)
	}
	current, err := service.AlertSnapshot(ctx, "Firing", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Items) != 1 || current.Items[0].Source != "platform" || current.Items[0].Component != "plinth" {
		t.Fatalf("unexpected platform projection: %+v", current.Items)
	}
	// The repeated observation updates only diagnostics. It must not increment
	// the alert lifecycle version or produce an SSE change that could reorder a
	// client without a state transition.
	if current.Items[0].RowVersion != 1 || current.Items[0].LastStateChange != current.Items[0].FirstSeenAt {
		t.Fatalf("repeat changed lifecycle projection: %+v", current.Items[0])
	}
	events, err := service.ChangesAfter(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].ChangeType != "created" || events[0].PlatformFaultID == 0 {
		t.Fatalf("repeat emitted lifecycle event: %+v", events)
	}
	if err := reporter.ObserveRuntimeConnection(ctx, "plinth", true); err != nil {
		t.Fatal(err)
	}
	current, err = service.AlertSnapshot(ctx, "Firing", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Items) != 0 {
		t.Fatalf("recovered fault remained firing: %+v", current.Items)
	}
	history, err := service.AlertSnapshot(ctx, "Resolved", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Items) != 1 || history.Items[0].ResolvedAt == nil {
		t.Fatalf("missing resolved platform lifecycle: %+v", history.Items)
	}
	if _, err := service.GetAlert(ctx, history.Items[0].ID); err != nil {
		t.Fatal(err)
	}
	observations, err := service.ListObservations(ctx, history.Items[0].ID)
	if err != nil || len(observations) != 0 {
		t.Fatalf("platform fault must not fabricate observations: %+v %v", observations, err)
	}
}
