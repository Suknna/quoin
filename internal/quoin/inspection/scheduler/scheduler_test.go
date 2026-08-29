package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/inspection"
)

type recordingService struct {
	plans []inspection.ScheduledPlan
	calls []scheduledCall
}

type scheduledCall struct {
	systemKey, planKey string
	scheduledFor       time.Time
	availability       inspection.RuntimeAvailability
}

func (s *recordingService) ScheduledPlans(context.Context) ([]inspection.ScheduledPlan, error) {
	return s.plans, nil
}

func (s *recordingService) CreateScheduledInspectionRun(_ context.Context, plan inspection.ScheduledPlan, scheduledFor time.Time, availability inspection.RuntimeAvailability) (inspection.RunDetail, error) {
	s.calls = append(s.calls, scheduledCall{systemKey: plan.SystemKey, planKey: plan.PlanKey, scheduledFor: scheduledFor, availability: availability})
	return inspection.RunDetail{}, nil
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }
func (c fixedClock) After(time.Duration) <-chan time.Time {
	return make(chan time.Time)
}

type oneBoundaryClock struct {
	now, wake time.Time
	arrived   bool
	signaled  chan struct{}
}

func newOneBoundaryClock(now time.Time) *oneBoundaryClock {
	return newBoundaryClock(now, now.Truncate(time.Minute).Add(time.Minute))
}

func newBoundaryClock(now, wake time.Time) *oneBoundaryClock {
	return &oneBoundaryClock{now: now, wake: wake, signaled: make(chan struct{}, 1)}
}

func (c *oneBoundaryClock) Now() time.Time {
	if c.arrived {
		// Run calls Now after consuming the timer and immediately before the
		// stale-boundary guard. Signal that exact point to the late-wakeup test.
		select {
		case c.signaled <- struct{}{}:
		default:
		}
		return c.wake
	}
	return c.now
}

func (c *oneBoundaryClock) After(time.Duration) <-chan time.Time {
	if c.arrived {
		return make(chan time.Time)
	}
	c.arrived = true
	result := make(chan time.Time, 1)
	result <- c.wake
	return result
}

func TestTickSchedulesCurrentBoundaryInPlanTimezone(t *testing.T) {
	service := &recordingService{plans: []inspection.ScheduledPlan{{
		SystemKey: "payments", PlanKey: "morning", Cron: "30 8 * * *", Timezone: "Asia/Shanghai",
	}}}
	at := time.Date(2026, time.August, 28, 0, 30, 0, 0, time.UTC)
	scheduler := newScheduler(service, fixedClock{now: at}, func(context.Context) inspection.RuntimeAvailability {
		return inspection.RuntimeAvailability{Plinth: true, Lintel: true}
	})

	if err := scheduler.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(service.calls) != 1 {
		t.Fatalf("scheduled calls = %d, want 1", len(service.calls))
	}
	call := service.calls[0]
	want := at.Truncate(time.Minute)
	if call.scheduledFor != want {
		t.Fatalf("scheduled_for = %s, want canonical UTC boundary %s", call.scheduledFor, want)
	}
	if call.systemKey != "payments" || call.planKey != "morning" {
		t.Fatalf("scheduled plan = %s/%s", call.systemKey, call.planKey)
	}
}

func TestRunDoesNotBackfillStartupMinute(t *testing.T) {
	service := &recordingService{plans: []inspection.ScheduledPlan{{
		SystemKey: "payments", PlanKey: "every-minute", Cron: "* * * * *", Timezone: "UTC",
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startedAt := time.Date(2026, time.August, 28, 0, 30, 20, 0, time.UTC)
	scheduler := newScheduler(service, newOneBoundaryClock(startedAt), nil)
	completed := make(chan struct{})
	scheduler.AfterTick(func(context.Context) { cancel() })
	go func() { scheduler.Run(ctx, nil); close(completed) }()
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not process its first post-start boundary")
	}
	if len(service.calls) != 1 {
		t.Fatalf("scheduled calls = %d, want only the post-start boundary", len(service.calls))
	}
	if want := startedAt.Truncate(time.Minute).Add(time.Minute); !service.calls[0].scheduledFor.Equal(want) {
		t.Fatalf("scheduled_for = %s, want first boundary after startup %s", service.calls[0].scheduledFor, want)
	}
}

func TestRunSkipsLateWakeupWithoutBackfill(t *testing.T) {
	service := &recordingService{plans: []inspection.ScheduledPlan{{
		SystemKey: "payments", PlanKey: "every-minute", Cron: "* * * * *", Timezone: "UTC",
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startedAt := time.Date(2026, time.August, 28, 0, 30, 20, 0, time.UTC)
	clock := newBoundaryClock(startedAt, startedAt.Add(2*time.Minute+40*time.Second))
	done := make(chan struct{})
	go func() { scheduler := newScheduler(service, clock, nil); scheduler.Run(ctx, nil); close(done) }()
	select {
	case <-clock.signaled:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not wait for its first boundary")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop after cancellation")
	}
	if len(service.calls) != 0 {
		t.Fatalf("late wakeup created %d historical runs", len(service.calls))
	}
}

func TestTickKeepsRepeatedDSTOccurrencesDistinctByUTC(t *testing.T) {
	service := &recordingService{plans: []inspection.ScheduledPlan{{
		SystemKey: "payments", PlanKey: "fall-back", Cron: "30 1 * * *", Timezone: "America/New_York",
	}}}
	availability := func(context.Context) inspection.RuntimeAvailability { return inspection.RuntimeAvailability{} }

	first := time.Date(2026, time.November, 1, 5, 30, 0, 0, time.UTC)
	second := time.Date(2026, time.November, 1, 6, 30, 0, 0, time.UTC)
	for _, at := range []time.Time{first, second} {
		scheduler := newScheduler(service, fixedClock{now: at}, availability)
		if err := scheduler.tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(service.calls) != 2 {
		t.Fatalf("fall-back schedule calls = %d, want two distinct UTC occurrences", len(service.calls))
	}
	if service.calls[0].scheduledFor.Equal(service.calls[1].scheduledFor) {
		t.Fatalf("repeated wall-clock schedule reused UTC key %s", service.calls[0].scheduledFor)
	}
}

func TestTickSkipsSpringForwardNonexistentWallTime(t *testing.T) {
	service := &recordingService{plans: []inspection.ScheduledPlan{{
		SystemKey: "payments", PlanKey: "spring-forward", Cron: "30 2 * * *", Timezone: "America/New_York",
	}}}
	// 2026-03-08T07:30Z is 03:30 EDT: 02:30 never exists on this date.
	at := time.Date(2026, time.March, 8, 7, 30, 0, 0, time.UTC)
	scheduler := newScheduler(service, fixedClock{now: at}, nil)
	if err := scheduler.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(service.calls) != 0 {
		t.Fatalf("spring-forward nonexistent local occurrence scheduled %d calls", len(service.calls))
	}
}

func TestRunDispatchesCommittedScheduledWorkAfterTick(t *testing.T) {
	service := &recordingService{plans: []inspection.ScheduledPlan{{
		SystemKey: "payments", PlanKey: "minute", Cron: "* * * * *", Timezone: "UTC",
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dispatched := make(chan struct{}, 1)
	scheduler := newScheduler(service, newOneBoundaryClock(time.Date(2026, time.August, 28, 0, 30, 0, 0, time.UTC)), nil)
	scheduler.AfterTick(func(context.Context) {
		dispatched <- struct{}{}
		cancel()
	})
	done := make(chan struct{})
	go func() { scheduler.Run(ctx, nil); close(done) }()
	select {
	case <-dispatched:
	case <-time.After(time.Second):
		t.Fatal("successful schedule tick did not trigger dispatch reconciliation")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop with context cancellation")
	}
	if len(service.calls) != 1 {
		t.Fatalf("scheduled calls = %d, want one before dispatch", len(service.calls))
	}
}
