// Source observation scheduling coordinator. All durability lives in the
// observation domain authority: due-tick admission, at-most-one-active-run
// fences, the schedule command-key dedupe and connection-disable
// cancellation are that package's transactional commands. This file also owns
// the Quoin-side construction of that authority from the compile-time
// descriptor catalog plus the deployment enablement set — enablement is
// always resolved BEFORE the service exists, so a deployment can never
// silently observe more than its YAML selected.
package app

import (
	"context"
	"database/sql"
	"time"

	"github.com/Suknna/quoin/internal/plugins/builtin"
	"github.com/Suknna/quoin/internal/quoin/observation"
)

// newSourceObservationService builds the observation authority for one
// deployment. A nil enabledPlugins means the deployment YAML is silent and
// the registry's DefaultEnabled plugins are observed; an explicit list is a
// whitelist and unknown or retired ids fail closed. The shared builtin
// source panics on a rejected built-in descriptor (a compile-time fact),
// so a violation aborts the process instead of serving with a lying catalog.
func newSourceObservationService(db *sql.DB, enabledPlugins []string) (*observation.Service, error) {
	registry := builtin.Registry()
	enabled, err := registry.ResolveEnabled(enabledPlugins)
	if err != nil {
		return nil, err
	}
	return observation.NewService(db, registry, enabled), nil
}

// ConfigureSourceObservation re-resolves the deployment plugin enablement and
// swaps the observation authority before any serving or scheduling starts.
func (application *apiServer) ConfigureSourceObservation(enabledPlugins []string) error {
	service, err := newSourceObservationService(application.db, enabledPlugins)
	if err != nil {
		return err
	}
	if application.readerWired {
		service.SetReader(application.reader)
	}
	application.observations = service
	return nil
}

// SourceObservationScheduler periodically asks the observation authority to
// reconcile and admit due work. It polls immediately at normal-process
// startup so an enabled connection with no successful observation is admitted
// without waiting for the next interval. Maintenance processes never
// construct it.
type SourceObservationScheduler struct {
	clock    sourceObservationClock
	poll     func(context.Context, time.Time) error
	dispatch func(context.Context)
}

// sourceObservationClock is deliberately narrow so scheduler timing remains
// deterministic in tests.
type sourceObservationClock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

type sourceObservationSystemClock struct{}

func (sourceObservationSystemClock) Now() time.Time { return time.Now().UTC() }
func (sourceObservationSystemClock) After(wait time.Duration) <-chan time.Time {
	return time.After(wait)
}

// NewSourceObservationScheduler constructs the production coordinator.
func NewSourceObservationScheduler(observations *observation.Service, dispatch func(context.Context)) *SourceObservationScheduler {
	scheduler := &SourceObservationScheduler{clock: sourceObservationSystemClock{}}
	if observations != nil {
		scheduler.poll = observations.AdmitDue
	}
	scheduler.dispatch = dispatch
	return scheduler
}

// Run stops promptly with ctx. A failed pass is reported but keeps no
// coordinator state, so the next pass retries the durable admission; nothing
// in the failure path can remove observed resources.
func (scheduler *SourceObservationScheduler) Run(ctx context.Context, onError func(error)) {
	if scheduler.poll == nil {
		return
	}
	for {
		if err := scheduler.poll(ctx, scheduler.clock.Now()); err != nil && onError != nil {
			onError(err)
		}
		// Newly admitted children use the normal production Plinth dispatch
		// path. A disconnected runtime leaves them Queued for this pass or
		// the reconnect kick.
		if scheduler.dispatch != nil {
			scheduler.dispatch(ctx)
		}
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
