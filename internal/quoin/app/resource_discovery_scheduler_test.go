package app

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

type resourceDiscoveryTestClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []chan time.Time
}

func (clock *resourceDiscoveryTestClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *resourceDiscoveryTestClock) After(time.Duration) <-chan time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	waiter := make(chan time.Time, 1)
	clock.waiters = append(clock.waiters, waiter)
	return waiter
}

func (clock *resourceDiscoveryTestClock) advance(t *testing.T, now time.Time) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		clock.mu.Lock()
		if len(clock.waiters) != 0 {
			waiter := clock.waiters[0]
			clock.waiters = clock.waiters[1:]
			clock.now = now
			clock.mu.Unlock()
			waiter <- now
			return
		}
		clock.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("scheduler did not begin waiting for its next poll")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestResourceDiscoverySchedulerPollsImmediatelyAndRepeats(t *testing.T) {
	clock := &resourceDiscoveryTestClock{now: time.Date(2026, time.September, 11, 10, 0, 0, 0, time.UTC)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := make(chan time.Time, 2)
	scheduler := newResourceDiscoveryScheduler(clock, func(_ context.Context, at time.Time) error {
		called <- at
		return nil
	})
	done := make(chan struct{})
	go func() { scheduler.Run(ctx, nil); close(done) }()

	select {
	case got := <-called:
		if !got.Equal(clock.now) {
			t.Fatalf("immediate poll at %s, want %s", got, clock.now)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not poll immediately")
	}
	next := clock.now.Add(time.Second)
	clock.advance(t, next)
	select {
	case got := <-called:
		if !got.Equal(next) {
			t.Fatalf("repeated poll at %s, want %s", got, next)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not repeat poll")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop after cancellation")
	}
}

func TestResourceDiscoverySchedulerRetriesAfterFailure(t *testing.T) {
	clock := &resourceDiscoveryTestClock{now: time.Date(2026, time.September, 11, 10, 0, 0, 0, time.UTC)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int
	errs := make(chan error, 1)
	scheduler := newResourceDiscoveryScheduler(clock, func(context.Context, time.Time) error {
		calls++
		if calls == 1 {
			return errors.New("temporary admission failure")
		}
		cancel()
		return nil
	})
	done := make(chan struct{})
	go func() { scheduler.Run(ctx, func(err error) { errs <- err }); close(done) }()

	select {
	case err := <-errs:
		if err.Error() != "temporary admission failure" {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not report failed poll")
	}
	clock.advance(t, clock.now.Add(time.Second))
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not retry after failed poll")
	}
	if calls != 2 {
		t.Fatalf("poll calls = %d, want retry", calls)
	}
}

func TestResourceDiscoveryAdmissionMaterializesCandidatesBeforeStartingRefresh(t *testing.T) {
	// A single pooled connection mirrors production SQLite. The candidate query
	// must close Rows before StartResourceRefresh begins its writer transaction.
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/scheduler.db?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, statement := range []string{
		`CREATE TABLE maintenance_state(id INTEGER PRIMARY KEY,active INTEGER NOT NULL)`,
		`CREATE TABLE business_systems(id INTEGER PRIMARY KEY,key TEXT NOT NULL,enabled INTEGER NOT NULL,current_config_version_id INTEGER)`,
		`CREATE TABLE business_system_config_versions(id INTEGER PRIMARY KEY,state TEXT NOT NULL,discovery_refresh_seconds INTEGER NOT NULL)`,
		`CREATE TABLE resource_refresh_runs(id INTEGER PRIMARY KEY,business_system_id INTEGER NOT NULL,config_version_id INTEGER NOT NULL,state TEXT NOT NULL,scheduled_for TEXT)`,
		`CREATE TABLE observed_refresh_log(resource_refresh_run_id INTEGER NOT NULL,completed_at TEXT NOT NULL,complete INTEGER NOT NULL)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO maintenance_state(id,active) VALUES(1,0); INSERT INTO business_system_config_versions(id,state,discovery_refresh_seconds) VALUES(7,'published',300); INSERT INTO business_systems(id,key,enabled,current_config_version_id) VALUES(3,'payments',1,7)`); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := make(chan struct{}, 1)
	admit := resourceDiscoveryAdmissionWithStart(db, func(ctx context.Context, key string, versionID int64, tick string) error {
		if _, err := db.ExecContext(ctx, `INSERT INTO resource_refresh_runs(business_system_id,config_version_id,state,scheduled_for) VALUES(?,?, 'Running',?)`, 3, versionID, tick); err != nil {
			return err
		}
		started <- struct{}{}
		return nil
	})
	if err := admit(ctx, time.Date(2026, time.September, 11, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("startup admission: %v", err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("scheduler admission deadlocked on the single SQLite connection")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM resource_refresh_runs`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("refresh runs = %d, err=%v; want immediate admission", count, err)
	}
}

func TestResourceDiscoverySchedulerStopsWithoutPollingAfterCancellation(t *testing.T) {
	clock := &resourceDiscoveryTestClock{now: time.Now().UTC()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	newResourceDiscoveryScheduler(clock, func(context.Context, time.Time) error {
		called = true
		return nil
	}).Run(ctx, nil)
	if !called {
		t.Fatal("startup poll must evaluate current declarations even when shutdown races startup")
	}
}
