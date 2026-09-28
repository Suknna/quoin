package inspection

import (
	"context"
	"testing"
	"time"
)

func TestMissingDailyReportsShowsOnlyDueDatesSinceCurrentSchedule(t *testing.T) {
	h := newTestHarness(t)
	h.seedPlan(t, "plan-missing")
	h.pinDailyNow(t, time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC))
	h.seedDailyConfig(t, dailyTestConfigInput("missing-daily", "plan-missing"))
	h.pinDailyNow(t, time.Date(2026, 9, 28, 7, 0, 0, 0, time.UTC))
	ctx := context.Background()
	missing, err := h.service.MissingDailyReports(ctx, "missing-daily")
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 3 || missing[0].LocalDate != "2026-09-27" || missing[1].LocalDate != "2026-09-26" || missing[2].LocalDate != "2026-09-25" {
		t.Fatalf("missing dates=%+v, want three due days since creation (newest first)", missing)
	}
	for _, date := range missing {
		if date.Reason != "not_scheduled" {
			t.Fatalf("unexpected missing reason: %+v", date)
		}
	}
	if _, err := h.service.CreateManualDailyReport(commandContext(t), h.principal, "backfill-missing-day", "missing-daily", "2026-09-26"); err != nil {
		t.Fatal(err)
	}
	missing, err = h.service.MissingDailyReports(ctx, "missing-daily")
	if err != nil || len(missing) != 2 || missing[0].LocalDate != "2026-09-27" || missing[1].LocalDate != "2026-09-25" {
		t.Fatalf("manual backfill did not clear exactly its missing identity: %+v err=%v", missing, err)
	}
	config, err := h.service.GetDailyReportConfig(ctx, "missing-daily")
	if err != nil {
		t.Fatal(err)
	}
	updated := dailyTestConfigInput("missing-daily", "plan-missing")
	updated.Enabled = false
	if _, err := h.service.UpdateDailyReportConfig(commandContext(t), h.principal, "disable-missing-config", updated, config.RowVersion); err != nil {
		t.Fatal(err)
	}
	missing, err = h.service.MissingDailyReports(ctx, "missing-daily")
	if err != nil || len(missing) != 0 {
		t.Fatalf("disabled config falsely claims scheduled missing dates: %+v err=%v", missing, err)
	}
}

func TestMissingDailyReportsIdentifiesNonexistentDSTTrigger(t *testing.T) {
	h := newTestHarness(t)
	h.seedPlan(t, "plan-dst-missing")
	h.pinDailyNow(t, time.Date(2026, 3, 7, 17, 0, 0, 0, time.UTC)) // New York noon before spring-forward.
	config := dailyTestConfigInput("missing-dst", "plan-dst-missing")
	config.Timezone = "America/New_York"
	config.TriggerTime = "02:30"
	h.seedDailyConfig(t, config)
	h.pinDailyNow(t, time.Date(2026, 3, 9, 7, 0, 0, 0, time.UTC))
	missing, err := h.service.MissingDailyReports(context.Background(), "missing-dst")
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) < 2 || missing[0].LocalDate != "2026-03-08" || missing[1].LocalDate != "2026-03-07" || missing[1].Reason != "trigger_nonexistent" {
		t.Fatalf("spring-forward missing day not identified for manual recovery: %+v", missing)
	}
}
