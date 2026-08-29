// Package scheduler turns already-published inspection plan projections into
// deterministic scheduled Runs. SQLite remains the sole duplicate and overlap
// authority; this package deliberately keeps no last-run cursor.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/inspection"
	"github.com/Suknna/quoin/internal/quoin/inspection/clock"
	"github.com/robfig/cron/v3"
)

type service interface {
	ScheduledPlans(context.Context) ([]inspection.ScheduledPlan, error)
	CreateScheduledInspectionRun(context.Context, inspection.ScheduledPlan, time.Time, inspection.RuntimeAvailability) (inspection.RunDetail, error)
}

// source is intentionally package-private: tests control time through this
// narrow seam, while production always constructs clock.System.
type source interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

// Availability reports the Runtime slots that can accept collection work at
// the scheduling boundary. The Run still commits if either is unavailable so
// the missed collection is durable rather than silently backfilled later.
type Availability func(context.Context) inspection.RuntimeAvailability

// Scheduler evaluates only boundaries reached while its process is running.
// A start after a boundary waits for the next one and cannot recreate an older
// occurrence.
type Scheduler struct {
	service      service
	clock        source
	availability Availability
	afterTick    func(context.Context)
}

func New(service service, availability Availability) *Scheduler {
	return newScheduler(service, clock.System{}, availability)
}

func newScheduler(service service, source source, availability Availability) *Scheduler {
	return &Scheduler{service: service, clock: source, availability: availability}
}

// AfterTick attaches process-owned, idempotent dispatch reconciliation after
// each completed scheduling pass, including one with per-plan failures. The
// scheduler itself remains domain-only and owns neither Runtime transport nor a
// second queue.
func (s *Scheduler) AfterTick(after func(context.Context)) *Scheduler {
	s.afterTick = after
	return s
}

// Run owns the process-lifetime minute loop. A fresh process first waits for
// the next boundary: scheduling its startup minute would backfill a slot that
// elapsed while it was down. onError may be nil.
func (s *Scheduler) Run(ctx context.Context, onError func(error)) {
	for {
		now := s.clock.Now()
		boundary := now.Truncate(time.Minute).Add(time.Minute)
		select {
		case <-ctx.Done():
			return
		case <-s.clock.After(boundary.Sub(now)):
		}
		// A suspended or overloaded process can wake after a later boundary.
		// Never turn that stale timer event into a historical Run.
		if !s.clock.Now().Truncate(time.Minute).Equal(boundary) {
			continue
		}
		if err := s.tickAt(ctx, boundary); err != nil && onError != nil {
			onError(err)
		}
		// Reconciliation is safe and idempotent. It must not be withheld from
		// already-committed plans merely because another plan failed to create.
		if s.afterTick != nil {
			s.afterTick(ctx)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) error {
	return s.tickAt(ctx, s.clock.Now())
}

func (s *Scheduler) tickAt(ctx context.Context, boundary time.Time) error {
	// This is the boundary snapshot. Querying SQLite can block, so sample slots
	// before it rather than relabelling a boundary-time state with a later one.
	availability := inspection.RuntimeAvailability{}
	if s.availability != nil {
		availability = s.availability(ctx)
	}
	plans, err := s.service.ScheduledPlans(ctx)
	if err != nil {
		return err
	}
	var scheduleErrors []error
	for _, plan := range plans {
		scheduledFor, due, err := dueAt(plan, boundary)
		if err != nil {
			return fmt.Errorf("plan %s/%s: %w", plan.SystemKey, plan.PlanKey, err)
		}
		if !due {
			continue
		}
		if _, err := s.service.CreateScheduledInspectionRun(ctx, plan, scheduledFor, availability); err != nil {
			scheduleErrors = append(scheduleErrors, fmt.Errorf("schedule %s/%s at %s: %w", plan.SystemKey, plan.PlanKey, scheduledFor.Format(time.RFC3339Nano), err))
		}
	}
	return errors.Join(scheduleErrors...)
}

func dueAt(plan inspection.ScheduledPlan, now time.Time) (time.Time, bool, error) {
	location, err := time.LoadLocation(plan.Timezone)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("load timezone %q: %w", plan.Timezone, err)
	}
	schedule, err := cron.ParseStandard(plan.Cron)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse cron %q: %w", plan.Cron, err)
	}
	boundary := now.In(location).Truncate(time.Minute)
	candidate := schedule.Next(boundary.Add(-time.Nanosecond))
	if !candidate.Equal(boundary) {
		return time.Time{}, false, nil
	}
	return candidate.UTC(), true, nil
}
