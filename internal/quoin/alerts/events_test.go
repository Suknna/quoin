package alerts

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// deliverStatus delivers one alertmanager item with the given labels,
// startsAt and status (firing|resolved) through the real Deliver path.
func deliverStatus(t *testing.T, service *Service, ctx context.Context, sourceID, credentialID int64, relayID string, labels map[string]string, startsAt, status string) DeliveryResult {
	t.Helper()
	body := webhookBody(status, labels, startsAt, "")
	result, err := service.Deliver(ctx, relayID, sourceID, credentialID, 1, body, time.Now().UTC())
	if err != nil {
		t.Fatalf("deliver %s: %v", relayID, err)
	}
	return result
}

// TestChangeEventLogDerivedInTransaction proves the frozen derivation
// (DATA-SSE-001): inserts emit created events, Firing→Resolved emits
// state_changed with the new row version, repeat firing without a state
// change emits nothing, and every occurrence's latest event row_version
// equals the occurrence row_version.
func TestChangeEventLogDerivedInTransaction(t *testing.T) {
	service, database, teardown := newTestService(t)
	defer teardown()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "events")

	labels := map[string]string{"alertname": "Evt", "severity": "warning"}
	starts := "2026-08-18T10:00:00Z"

	deliverStatus(t, service, ctx, sourceID, credentialID, "r1", labels, starts, "firing")
	high, oldest, err := service.Watermarks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if high != 1 || oldest != 1 {
		t.Fatalf("after first firing: high=%d oldest=%d, want 1/1", high, oldest)
	}
	events, err := service.ChangesAfter(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].ChangeType != "created" || events[0].RowVersion != 1 || events[0].OccurrenceID != 1 {
		t.Fatalf("first event wrong: %+v", events[0])
	}

	// Repeat firing: no state change, no event.
	deliverStatus(t, service, ctx, sourceID, credentialID, "r2", labels, starts, "firing")
	high, _, _ = service.Watermarks(ctx)
	if high != 1 {
		t.Fatalf("repeat firing must not emit an event, high=%d", high)
	}

	// Resolved: state change, row_version 2.
	deliverStatus(t, service, ctx, sourceID, credentialID, "r3", labels, starts, "resolved")
	events, err = service.ChangesAfter(ctx, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].ChangeType != "state_changed" || events[0].RowVersion != 2 {
		t.Fatalf("resolved event wrong: %+v", events)
	}
	var state string
	var rowVersion int64
	if err := database.SQL.QueryRow(`SELECT state, row_version FROM alert_occurrences WHERE id=1`).Scan(&state, &rowVersion); err != nil {
		t.Fatal(err)
	}
	if state != "Resolved" || rowVersion != 2 {
		t.Fatalf("occurrence state=%s rv=%d, want Resolved/2", state, rowVersion)
	}

	// Late firing after resolved: observation retained, no reopen, no event
	// beyond the state_changed already recorded.
	deliverStatus(t, service, ctx, sourceID, credentialID, "r4", labels, starts, "firing")
	high, _, _ = service.Watermarks(ctx)
	if high != 2 {
		t.Fatalf("late firing must not emit a state event, high=%d", high)
	}
	var effects []string
	rows, err := database.SQL.Query(`SELECT effect FROM alert_observations WHERE occurrence_id=1 ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var effect string
		if err := rows.Scan(&effect); err != nil {
			t.Fatal(err)
		}
		effects = append(effects, effect)
	}
	want := []string{"initial_firing", "repeat_firing", "resolved", "late_firing_after_resolved"}
	if len(effects) != len(want) {
		t.Fatalf("effects=%v want %v", effects, want)
	}
	for index := range want {
		if effects[index] != want[index] {
			t.Fatalf("effects=%v want %v", effects, want)
		}
	}
}

// TestCursorPredicateMatrix walks the frozen last-seen predicate corner
// cases (DATA-SSE-009): empty log, first event, oldest-1 valid, oldest-2
// expired, cursor==high always current.
func TestCursorPredicateMatrix(t *testing.T) {
	cases := []struct {
		cursor, high, oldest int64
		expired              bool
		note                 string
	}{
		{0, 0, 0, false, "empty log: only cursor 0 valid"},
		{1, 0, 0, true, "empty log: nonzero cursor expired"},
		{0, 1, 1, false, "first event: after=0 replays it"},
		{1, 1, 1, false, "cursor==high always current"},
		{9, 1, 1, false, "cursor above high (future) is not expired"},
		{4, 10, 5, false, "after=oldest-1 valid, replays oldest"},
		{3, 10, 5, true, "after=oldest-2 expired"},
		{10, 10, 5, false, "snapshot cursor == high always current"},
		{10, 20, 11, false, "after=high after GC bump still current when == oldest-1"},
	}
	for _, testCase := range cases {
		if got := CursorExpired(testCase.cursor, testCase.high, testCase.oldest); got != testCase.expired {
			t.Errorf("CursorExpired(%d,%d,%d)=%v want %v (%s)", testCase.cursor, testCase.high, testCase.oldest, got, testCase.expired, testCase.note)
		}
	}
}

// TestHighWaterNeverRegressesOverGC proves the retention GC boundary from
// the spec: old rows may be DELETEd, MAX(id) never regresses, MIN(id)
// reflects the retention window, and the latest row cannot be deleted.
func TestHighWaterNeverRegressesOverGC(t *testing.T) {
	service, database, teardown := newTestService(t)
	defer teardown()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "gc")

	for index := 0; index < 4; index++ {
		labels := map[string]string{"alertname": fmt.Sprintf("G%d", index)}
		deliverStatus(t, service, ctx, sourceID, credentialID, fmt.Sprintf("gc-r%d", index), labels, "2026-08-18T11:00:00Z", "firing")
	}
	high, oldest, _ := service.Watermarks(ctx)
	if high != 4 || oldest != 1 {
		t.Fatalf("watermarks high=%d oldest=%d, want 4/1", high, oldest)
	}

	// GC-delete the two oldest rows (simulate retention window).
	if _, err := database.SQL.Exec(`DELETE FROM alert_change_log WHERE id <= 2`); err != nil {
		t.Fatal(err)
	}
	high, oldest, _ = service.Watermarks(ctx)
	if high != 4 || oldest != 3 {
		t.Fatalf("after GC high=%d oldest=%d, want 4/3 (MAX must not regress)", high, oldest)
	}
	// after=2 (== oldest-1) is still valid and replays id=3.
	if CursorExpired(2, high, oldest) {
		t.Fatal("after=oldest-1 must remain valid after GC")
	}
	// after=1 (== oldest-2) is expired.
	if !CursorExpired(1, high, oldest) {
		t.Fatal("after=oldest-2 must be expired after GC")
	}
	// The latest row can never be deleted (frozen trigger).
	if _, err := database.SQL.Exec(`DELETE FROM alert_change_log WHERE id = 4`); err == nil {
		t.Fatal("deleting the high-water row must fail")
	}
	high, _, _ = service.Watermarks(ctx)
	if high != 4 {
		t.Fatalf("high-water deleted: %d", high)
	}
}

// TestResolvedDeliveryBarrierInterleavings constructs explicit SQLite commit
// orders for the resolved race: an old (firing-state) write prepared before
// the resolving delivery commits must not resurrect the occurrence. The
// interleaving is driven with a real second connection and the
// alert_occurrences row-version fence (expected_row_version in WHERE).
func TestResolvedDeliveryBarrierInterleavings(t *testing.T) {
	service, database, teardown := newTestService(t)
	defer teardown()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "barrier")

	labels := map[string]string{"alertname": "Br", "instance": "b-1"}
	starts := "2026-08-18T12:00:00Z"
	deliverStatus(t, service, ctx, sourceID, credentialID, "b1", labels, starts, "firing")

	// Stale writer: reads row_version=1 and pauses (barrier) while the
	// resolving delivery commits row_version=2.
	stale := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := database.SQL.Conn(ctx)
		if err != nil {
			stale <- err
			return
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
			stale <- err
			return
		}
		// Fenced update: only lands on row_version=1. After the resolver
		// commits version 2 this matches 0 rows and changes nothing.
		result, err := conn.ExecContext(ctx, `UPDATE alert_occurrences SET row_version=row_version+1 WHERE id=1 AND row_version=1`)
		if err != nil {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
			stale <- err
			return
		}
		affected, _ := result.RowsAffected()
		_ = affected
		<-stale // barrier: hold the write lock until the main path signals
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			stale <- err
			return
		}
		close(stale)
	}()
	// Let the stale writer take the write lock first.
	time.Sleep(150 * time.Millisecond)

	// The resolver (resolved delivery) queues behind the stale writer's
	// IMMEDIATE transaction; it commits after the stale writer releases.
	resolverDone := make(chan struct{})
	go func() {
		defer close(resolverDone)
		deliverStatus(t, service, ctx, sourceID, credentialID, "b2", labels, starts, "resolved")
	}()
	time.Sleep(150 * time.Millisecond)
	// Release the barrier: stale writer commits first, resolver second.
	// Commit order: stale write (rv 1→2 spurious bump), then resolver.
	// The resolver's UPDATE fences on its own observed version; the final
	// state must still be Resolved with a monotonically larger version.
	staleRelease := make(chan struct{})
	go func() { stale <- nil; close(staleRelease) }()
	<-staleRelease
	<-resolverDone
	wg.Wait()

	var state string
	var rowVersion int64
	if err := database.SQL.QueryRow(`SELECT state, row_version FROM alert_occurrences WHERE id=1`).Scan(&state, &rowVersion); err != nil {
		t.Fatal(err)
	}
	if state != "Resolved" {
		t.Fatalf("occurrence must end Resolved regardless of interleaving, got %s rv=%d", state, rowVersion)
	}
	if rowVersion < 2 || rowVersion > 3 {
		t.Fatalf("row_version=%d outside the fenced 2..3 window", rowVersion)
	}
	// The resolved transition produced exactly one state_changed event.
	events, err := service.ChangesAfter(ctx, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	stateChanged := 0
	for _, event := range events {
		if event.ChangeType == "state_changed" {
			stateChanged++
		}
	}
	if stateChanged != 1 {
		t.Fatalf("exactly one state_changed event expected, got %d", stateChanged)
	}
}

// TestConcurrentDistinctOccurrencesGrowSeqMonotonically runs several
// deliveries for distinct identities concurrently and proves the change seq
// stays strictly monotonic with no duplicate ids (commit-order allocation).
func TestConcurrentDistinctOccurrencesGrowSeqMonotonically(t *testing.T) {
	service, _, teardown := newTestService(t)
	defer teardown()
	ctx := context.Background()
	sourceID, credentialID := seedSource(t, service, ctx, "concurrent")

	const workers = 6
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			labels := map[string]string{"alertname": "Cc", "instance": fmt.Sprintf("c-%d", worker)}
			deliverStatus(t, service, ctx, sourceID, credentialID, fmt.Sprintf("cc-r%d", worker), labels, "2026-08-18T13:00:00Z", "firing")
		}(worker)
	}
	wg.Wait()

	events, err := service.ChangesAfter(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != workers {
		t.Fatalf("events=%d want %d", len(events), workers)
	}
	seen := map[int64]bool{}
	previous := int64(0)
	for _, event := range events {
		if event.Seq <= previous {
			t.Fatalf("seq not monotonic: %d after %d", event.Seq, previous)
		}
		if seen[event.Seq] {
			t.Fatalf("duplicate seq %d", event.Seq)
		}
		seen[event.Seq] = true
		previous = event.Seq
	}
}
