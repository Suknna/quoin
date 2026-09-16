package alerts

// Reader regression: every pure read serves the runner-installed trusted
// read-only pool and fails closed without one. The writer database is never
// a read fallback, the pre-composition constructor stays valid unwired, and
// only the opaque execution.Reader capability passes SetReader validation.
// Snapshot filter/detail and lifecycle behavior stay covered by
// TestSnapshotFilterAndDetailKey and the platform-fault lifecycle tests.

import (
	"context"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

func TestReaderFailClosedRawRejectedAndTrustedWired(t *testing.T) {
	_, database, done := newTestService(t)
	defer done()
	ctx := context.Background()

	// The pre-composition constructor returns a valid service, but all six
	// pure reads fail closed instead of silently reading the writer pool.
	unwired := NewService(database.SQL)
	if _, _, err := unwired.Watermarks(ctx); err == nil {
		t.Fatal("unwired Watermarks must fail closed")
	}
	if _, err := unwired.ChangesAfter(ctx, 0, 1); err == nil {
		t.Fatal("unwired ChangesAfter must fail closed")
	}
	if _, err := unwired.AlertSnapshot(ctx, "Firing", "", ""); err == nil {
		t.Fatal("unwired AlertSnapshot must fail closed")
	}
	if _, err := unwired.GetAlert(ctx, "1"); err == nil {
		t.Fatal("unwired GetAlert must fail closed")
	}
	if _, err := unwired.ListObservations(ctx, "1"); err == nil {
		t.Fatal("unwired ListObservations must fail closed")
	}
	if _, err := unwired.ListIntakeIssues(ctx, false); err == nil {
		t.Fatal("unwired ListIntakeIssues must fail closed")
	}

	// SetReader validation accepts only the opaque read-only capability: the
	// writable pool is rejected outright, not silently adopted as a reader.
	if _, err := NewServiceWithReader(database.SQL, database.SQL, execution.NewRunner(database.SQL, execution.NewRegistry(), nil)); err == nil {
		t.Fatal("writer pool must be rejected as a read-only reader")
	}

	// The trusted wiring serves actual data: a runner mutation on the writer
	// becomes visible through the read-only pool's change-log and snapshot
	// reads, with the snapshot watermark matching the committed state.
	wired, err := NewServiceWithReader(database.SQL, database.Reader, execution.NewRunner(database.SQL, execution.NewRegistry(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := NewPlatformFaultReporter(wired).ObserveRuntimeConnection(ctx, "lintel", false); err != nil {
		t.Fatal(err)
	}
	highWater, oldest, err := wired.Watermarks(ctx)
	if err != nil || highWater != 1 || oldest != 1 {
		t.Fatalf("trusted Watermarks = (%d,%d,%v), want (1,1,nil)", highWater, oldest, err)
	}
	events, err := wired.ChangesAfter(ctx, 0, 10)
	if err != nil || len(events) != 1 || events[0].PlatformFaultID == 0 {
		t.Fatalf("trusted ChangesAfter = %+v %v, want one platform fault event", events, err)
	}
	snapshot, err := wired.AlertSnapshot(ctx, "Firing", "", "")
	if err != nil || len(snapshot.Items) != 1 || snapshot.Items[0].Component != "lintel" || snapshot.SnapshotSeq != 1 {
		t.Fatalf("trusted AlertSnapshot = %+v %v, want the lintel fault at seq 1", snapshot, err)
	}
}
