package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/businesssystem"
)

// resourceDiscoveryPoll is the application-to-domain scheduling boundary. The
// domain command derives due work from the current published declarations and
// persists its config-version/tick command key atomically; this coordinator
// never keeps a last-run cursor in process memory.
type (
	resourceDiscoveryPoll     func(context.Context, time.Time) error
	resourceDiscoveryDispatch func(context.Context)
)

// resourceDiscoveryClock is deliberately narrow so scheduler timing remains
// deterministic in tests. Production uses resourceDiscoverySystemClock.
type resourceDiscoveryClock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

type resourceDiscoverySystemClock struct{}

func (resourceDiscoverySystemClock) Now() time.Time { return time.Now().UTC() }
func (resourceDiscoverySystemClock) After(wait time.Duration) <-chan time.Time {
	return time.After(wait)
}

// ResourceDiscoveryScheduler periodically asks the domain authority to admit
// due resource discovery. It polls immediately at normal-process startup so a
// newly published current declaration with no successful refresh is admitted
// without waiting for the next pass. Maintenance processes never construct it.
//
// The one-second poll cadence is intentionally smaller than the schema's
// minimum discovery_refresh_seconds (60). The domain command owns the durable
// interval boundary and deduplication, while this coordinator provides a
// bounded, deterministic admission delay without another scheduler store.
type ResourceDiscoveryScheduler struct {
	clock    resourceDiscoveryClock
	poll     resourceDiscoveryPoll
	dispatch resourceDiscoveryDispatch
}

// NewResourceDiscoveryScheduler constructs the production coordinator. It
// reads only current, enabled declarations and asks the domain command to make
// the final serialized admission decision.
func NewResourceDiscoveryScheduler(systems *businesssystem.Service, dispatch resourceDiscoveryDispatch) *ResourceDiscoveryScheduler {
	scheduler := newResourceDiscoveryScheduler(resourceDiscoverySystemClock{}, resourceDiscoveryAdmission(systems))
	scheduler.dispatch = dispatch
	return scheduler
}

func newResourceDiscoveryScheduler(clock resourceDiscoveryClock, poll resourceDiscoveryPoll) *ResourceDiscoveryScheduler {
	return &ResourceDiscoveryScheduler{clock: clock, poll: poll}
}

type resourceDiscoveryStart func(context.Context, string, int64, string) error

func resourceDiscoveryAdmission(systems *businesssystem.Service) resourceDiscoveryPoll {
	if systems == nil {
		return func(context.Context, time.Time) error { return nil }
	}
	return resourceDiscoveryAdmissionWithStart(systems.DB(), func(ctx context.Context, key string, versionID int64, tick string) error {
		_, err := systems.StartResourceRefresh(ctx, 0, fmt.Sprintf("resource-discovery:%d:%s", versionID, tick), key, "schedule", &tick)
		return err
	})
}

// resourceDiscoveryAdmissionWithStart separates the query/command connection
// lifecycle from the production domain authority. Its narrow test seam proves
// a MaxOpenConns(1) SQLite pool can execute the post-read command.
func resourceDiscoveryAdmissionWithStart(db *sql.DB, start resourceDiscoveryStart) resourceDiscoveryPoll {
	return func(ctx context.Context, now time.Time) error {
		if db == nil || start == nil {
			return nil
		}
		// Maintenance can begin after normal startup. Check on every pass and
		// fail closed if the authoritative state cannot be read.
		var maintenanceActive int
		if err := db.QueryRowContext(ctx, `SELECT active FROM maintenance_state WHERE id=1`).Scan(&maintenanceActive); err != nil {
			return fmt.Errorf("read resource discovery maintenance fence: %w", err)
		}
		if maintenanceActive != 0 {
			return nil
		}
		// Materialize the read-only candidate snapshot before invoking the command.
		// Production SQLite uses MaxOpenConns(1); keeping Rows open here would make
		// StartResourceRefresh wait forever for the only connection it needs.
		type candidate struct {
			key      string
			version  int64
			interval int64
			last     sql.NullString
		}
		rows, err := db.QueryContext(ctx, `
			SELECT b.key,v.id,v.discovery_refresh_seconds,
			       (SELECT MAX(l.completed_at)
			        FROM resource_refresh_runs r
			        JOIN observed_refresh_log l ON l.resource_refresh_run_id=r.id
			        WHERE r.business_system_id=b.id AND r.config_version_id=v.id
			          AND r.state IN ('Completed','CompletedWithWarnings') AND l.complete=1)
			FROM business_systems b
			JOIN business_system_config_versions v ON v.id=b.current_config_version_id
			WHERE b.enabled=1 AND v.state='published'
			  AND NOT EXISTS (SELECT 1 FROM resource_refresh_runs active
			                  WHERE active.business_system_id=b.id
			                    AND active.state IN ('Queued','Running'))
			ORDER BY b.id`)
		if err != nil {
			return fmt.Errorf("list resource discovery candidates: %w", err)
		}
		var candidates []candidate
		for rows.Next() {
			var item candidate
			if err := rows.Scan(&item.key, &item.version, &item.interval, &item.last); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, item := range candidates {
			interval := item.interval
			if interval <= 0 {
				interval = 300
			}
			if item.last.Valid {
				completed, parseErr := time.Parse(time.RFC3339Nano, item.last.String)
				if parseErr != nil {
					return fmt.Errorf("parse last resource discovery for %s: %w", item.key, parseErr)
				}
				if now.Before(completed.Add(time.Duration(interval) * time.Second)) {
					continue
				}
			}
			// The config version and its UTC interval boundary form the durable
			// command key. A failed refresh therefore retries at the next declared
			// cadence without creating multiple roots for the same tick.
			tick := now.UTC().Truncate(time.Duration(interval) * time.Second).Format(time.RFC3339)
			if err := start(ctx, item.key, item.version, tick); err != nil && !errors.Is(err, businesssystem.ErrNotFound) {
				return fmt.Errorf("start resource discovery for %s: %w", item.key, err)
			}
		}
		return nil
	}
}

// Run stops promptly with ctx. A failed pass is reported but does not advance
// any coordinator state, so the next poll retries durable admission and cannot
// remove observed resources.
func (scheduler *ResourceDiscoveryScheduler) Run(ctx context.Context, onError func(error)) {
	if scheduler.poll == nil {
		return
	}
	for {
		if err := scheduler.poll(ctx, scheduler.clock.Now()); err != nil && onError != nil {
			onError(err)
		}
		// Newly admitted children use the normal production Plinth dispatch path.
		// A disconnected runtime leaves them Queued for this pass or reconnect.
		if scheduler.dispatch != nil {
			scheduler.dispatch(ctx)
		}
		// Poll implementations may trigger process shutdown after committing a
		// final admission. Do not wait for another timer in that case.
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-scheduler.clock.After(time.Second):
		}
	}
}
