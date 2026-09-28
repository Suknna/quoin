package inspection

// 跨来源日报行为测试（ADR-0014）：以 SQLite 域服务为最高行为接缝，验证
// 时区/DST 窗口冻结、边界触发幂等、无健康虚构的窗口聚合、两小时采证截止、
// 人工补跑原日期窗口与不可变版本化。断言先于私有结构：读模型与封存内容
// JSON 是对外事实。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// pinDailyNow pins the service clock so created runs' evidence_at lands
// inside the frozen window under test.
func (h *testHarness) pinDailyNow(t *testing.T, at time.Time) {
	t.Helper()
	h.service.now = func() time.Time { return at }
}

func dailyTestConfigInput(configKey string, planKeys ...string) DailyReportConfigInput {
	return DailyReportConfigInput{
		ConfigKey: configKey, DisplayName: "核心日报", Enabled: true,
		Timezone: "UTC", TriggerTime: "06:00", PlanKeys: planKeys,
	}
}

func (h *testHarness) seedDailyConfig(t *testing.T, input DailyReportConfigInput) DailyReportConfig {
	t.Helper()
	config, err := h.service.CreateDailyReportConfig(commandContext(t), h.principal, "seed-daily-config-"+input.ConfigKey, input)
	if err != nil {
		t.Fatalf("seed daily config %s: %v", input.ConfigKey, err)
	}
	return config
}

func TestDailyWindowUTCFreezesDSTCorrectBoundaries(t *testing.T) {
	utc, err := time.LoadLocation("UTC")
	if err != nil {
		t.Fatal(err)
	}
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	start, end, err := dailyWindowUTC("2026-09-27", utc)
	if err != nil {
		t.Fatal(err)
	}
	if want := "2026-09-27T00:00:00Z"; start.Format(time.RFC3339) != want || end.Format(time.RFC3339) != "2026-09-28T00:00:00Z" {
		t.Fatalf("normal UTC window = [%s,%s), want [%s,2026-09-28T00:00:00Z)", start, end, want)
	}
	// 2026-03-08 springs forward: the local day is 23h, frozen as UTC facts.
	start, end, err = dailyWindowUTC("2026-03-08", newYork)
	if err != nil {
		t.Fatal(err)
	}
	if start.Format(time.RFC3339) != "2026-03-08T05:00:00Z" || end.Format(time.RFC3339) != "2026-03-09T04:00:00Z" {
		t.Fatalf("spring-forward window = [%s,%s), want 23h UTC span", start, end)
	}
	// 2026-11-01 falls back: the local day is 25h.
	start, end, err = dailyWindowUTC("2026-11-01", newYork)
	if err != nil {
		t.Fatal(err)
	}
	if start.Format(time.RFC3339) != "2026-11-01T04:00:00Z" || end.Format(time.RFC3339) != "2026-11-02T05:00:00Z" {
		t.Fatalf("fall-back window = [%s,%s), want 25h UTC span", start, end)
	}
	// Month/year rollover stays inside the pure local-date arithmetic.
	start, end, err = dailyWindowUTC("2026-12-31", utc)
	if err != nil {
		t.Fatal(err)
	}
	if end.Format(time.RFC3339) != "2027-01-01T00:00:00Z" {
		t.Fatalf("year-end window end = %s", end)
	}
	if _, _, err := dailyWindowUTC("2026-13-40", utc); err == nil {
		t.Fatal("impossible local date must be rejected")
	}
}

func TestDailyReportConfigValidationAndOptimisticUpdate(t *testing.T) {
	h := newTestHarness(t)
	h.seedPlan(t, "plan-a")
	h.seedPlan(t, "plan-b")
	ctx := commandContext(t)

	cases := []struct {
		name     string
		mutate   func(*DailyReportConfigInput)
		wantCode string
	}{
		{name: "invalid timezone", mutate: func(c *DailyReportConfigInput) { c.Timezone = "Mars/Olympus" }, wantCode: "malformed_timezone"},
		{name: "invalid trigger time", mutate: func(c *DailyReportConfigInput) { c.TriggerTime = "6:00" }, wantCode: "malformed_trigger_time"},
		{name: "empty plan set", mutate: func(c *DailyReportConfigInput) { c.PlanKeys = nil }, wantCode: "malformed_plan_keys"},
		{name: "duplicate plan key", mutate: func(c *DailyReportConfigInput) { c.PlanKeys = []string{"plan-a", "plan-a"} }, wantCode: "malformed_plan_keys"},
		{name: "unknown plan", mutate: func(c *DailyReportConfigInput) { c.PlanKeys = []string{"missing-plan"} }, wantCode: "unknown_plan"},
		{name: "bad key", mutate: func(c *DailyReportConfigInput) { c.ConfigKey = "Core_Daily" }, wantCode: "malformed_key"},
	}
	for _, testCase := range cases {
		input := dailyTestConfigInput("reject-me", "plan-a")
		testCase.mutate(&input)
		if _, err := h.service.CreateDailyReportConfig(ctx, h.principal, "reject-cmd-"+testCase.name, input); err == nil {
			t.Fatalf("%s: expected deterministic rejection", testCase.name)
		} else {
			var conflict *PlanConflictError
			if !errors.As(err, &conflict) || conflict.Code != testCase.wantCode {
				t.Fatalf("%s: rejection = %v, want code %s", testCase.name, err, testCase.wantCode)
			}
		}
		var rows int
		if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_daily_report_configs`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 0 {
			t.Fatalf("%s: rejected create persisted a row", testCase.name)
		}
	}

	// 参与集合归一化：输入顺序不进入冻结快照，存储按稳定 key 序。
	created, err := h.service.CreateDailyReportConfig(ctx, h.principal, "create-cmd-1", dailyTestConfigInput("core-daily", "plan-b", "plan-a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(created.PlanKeys) != 2 || created.PlanKeys[0] != "plan-a" || created.PlanKeys[1] != "plan-b" {
		t.Fatalf("created plan keys = %v, want sorted [plan-a plan-b]", created.PlanKeys)
	}
	if _, err := h.service.CreateDailyReportConfig(ctx, h.principal, "create-cmd-2", dailyTestConfigInput("core-daily", "plan-a")); err == nil {
		t.Fatal("duplicate config key must be rejected")
	} else {
		var conflict *PlanConflictError
		if !errors.As(err, &conflict) || conflict.Code != "config_exists" {
			t.Fatalf("duplicate config rejection = %v", err)
		}
	}
	// 乐观并发：旧 row_version 的更新被确定性拒绝。
	stale := dailyTestConfigInput("core-daily", "plan-a")
	stale.DisplayName = "旧名称"
	if _, err := h.service.UpdateDailyReportConfig(ctx, h.principal, "update-cmd-stale", stale, created.RowVersion+5); err == nil {
		t.Fatal("stale row_version update must be rejected")
	}
	updated, err := h.service.UpdateDailyReportConfig(ctx, h.principal, "update-cmd-1", stale, created.RowVersion)
	if err != nil {
		t.Fatal(err)
	}
	if updated.RowVersion != created.RowVersion+1 || updated.DisplayName != "旧名称" {
		t.Fatalf("updated config = %+v", updated)
	}
}

func TestDueDailyReportConfigsMatchesLocalTriggerMinute(t *testing.T) {
	h := newTestHarness(t)
	h.seedPlan(t, "plan-a")
	h.seedDailyConfig(t, dailyTestConfigInput("utc-morning", "plan-a"))
	newYorkInput := dailyTestConfigInput("ny-morning", "plan-a")
	newYorkInput.Timezone = "America/New_York"
	newYorkInput.TriggerTime = "01:30"
	h.seedDailyConfig(t, newYorkInput)
	// 停用配置不产生到期投影。
	disabled := dailyTestConfigInput("disabled-daily", "plan-a")
	disabled.Enabled = false
	h.seedDailyConfig(t, disabled)

	// 2026-11-01 的回退日：本地 01:30 出现两次，两次边界都到期。
	for _, boundary := range []time.Time{
		time.Date(2026, time.November, 1, 5, 30, 0, 0, time.UTC),
		time.Date(2026, time.November, 1, 6, 30, 0, 0, time.UTC),
	} {
		due, err := h.service.DueDailyReportConfigs(context.Background(), boundary)
		if err != nil {
			t.Fatal(err)
		}
		if len(due) != 1 || due[0].ConfigKey != "ny-morning" {
			t.Fatalf("fall-back due at %s = %+v, want only ny-morning", boundary, due)
		}
	}
	nonDue, err := h.service.DueDailyReportConfigs(context.Background(), time.Date(2026, time.November, 1, 5, 29, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(nonDue) != 0 {
		t.Fatalf("non-due minute projected %+v", nonDue)
	}
	// UTC 配置在自己的触发分钟到期。
	due, err := h.service.DueDailyReportConfigs(context.Background(), time.Date(2026, time.November, 1, 6, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ConfigKey != "utc-morning" {
		t.Fatalf("utc due = %+v, want utc-morning", due)
	}
}

func TestScheduledDailyReportFreezesWindowContributionsAndReplays(t *testing.T) {
	h := newTestHarness(t)
	h.seedPlan(t, "plan-a")
	h.seedPlan(t, "plan-b")
	h.seedDailyConfig(t, dailyTestConfigInput("core-daily", "plan-a", "plan-b"))

	boundary := time.Date(2026, time.September, 28, 6, 0, 0, 0, time.UTC)
	if err := h.service.CreateScheduledDailyReport(context.Background(), DailyReportConfig{ConfigKey: "core-daily", Timezone: "UTC"}, boundary); err != nil {
		t.Fatal(err)
	}
	var localDate, timezone, windowStart, windowEnd, state, scheduledFor, cutoffAt, contributionsJSON string
	if err := h.db.QueryRow(`SELECT local_date,timezone,window_start_utc,window_end_utc,state,scheduled_for,cutoff_at,contributions_json
		FROM inspection_daily_reports WHERE config_key='core-daily'`).Scan(&localDate, &timezone, &windowStart, &windowEnd, &state, &scheduledFor, &cutoffAt, &contributionsJSON); err != nil {
		t.Fatal(err)
	}
	if localDate != "2026-09-27" || state != "Collecting" {
		t.Fatalf("trigger produced local_date=%s state=%s, want previous fully ended day Collecting", localDate, state)
	}
	if windowStart != "2026-09-27T00:00:00Z" || windowEnd != "2026-09-28T00:00:00Z" {
		t.Fatalf("frozen window = [%s,%s)", windowStart, windowEnd)
	}
	if scheduledFor != boundary.Format(time.RFC3339Nano) || cutoffAt != boundary.Add(2*time.Hour).Format(time.RFC3339Nano) {
		t.Fatalf("scheduled_for=%s cutoff_at=%s", scheduledFor, cutoffAt)
	}
	contributions := []dailyContribution{}
	if err := json.Unmarshal([]byte(contributionsJSON), &contributions); err != nil {
		t.Fatal(err)
	}
	if len(contributions) != 2 {
		t.Fatalf("contributions = %+v, want both participating plans frozen", contributions)
	}
	for _, contribution := range contributions {
		if !contribution.Enabled || !contribution.SourceEnabled || contribution.PluginID != "thanos" || contribution.TemplateVersion != "1" {
			t.Fatalf("contribution = %+v, want live source identity frozen", contribution)
		}
	}

	// 同一边界重放是成功静默：第一个创建的审计保持，绝不二次成行。
	if err := h.service.CreateScheduledDailyReport(context.Background(), DailyReportConfig{ConfigKey: "core-daily", Timezone: "UTC"}, boundary); err != nil {
		t.Fatalf("replayed boundary must observe success: %v", err)
	}
	var rows int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_daily_reports WHERE config_key='core-daily'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("replayed trigger created %d rows, want 1", rows)
	}

	// 触发时冻结：触发后停用计划/配置不改写已冻结快照；停用后的重复边界静默跳过。
	if _, err := h.db.Exec(`UPDATE inspection_plans SET enabled=0 WHERE plan_key='plan-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE inspection_daily_report_configs SET enabled=0 WHERE config_key='core-daily'`); err != nil {
		t.Fatal(err)
	}
	nextBoundary := boundary.Add(24 * time.Hour)
	if err := h.service.CreateScheduledDailyReport(context.Background(), DailyReportConfig{ConfigKey: "core-daily", Timezone: "UTC"}, nextBoundary); err != nil {
		t.Fatalf("disabled config after trigger must skip silently: %v", err)
	}
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_daily_reports WHERE config_key='core-daily'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("disabled config boundary created rows = %d, want 0 new", rows-1)
	}
	stillFrozen := []dailyContribution{}
	if err := h.db.QueryRow(`SELECT contributions_json FROM inspection_daily_reports WHERE config_key='core-daily'`).Scan(&contributionsJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(contributionsJSON), &stillFrozen); err != nil {
		t.Fatal(err)
	}
	for _, contribution := range stillFrozen {
		if !contribution.Enabled {
			t.Fatalf("post-trigger plan disable leaked into frozen snapshot: %+v", contribution)
		}
	}
}

// seedWindowFacts 用域命令把一天的真实采集事实放进窗口：ok、gap 与一个
// 仍未收敛的 Running Run。
func (h *testHarness) seedWindowFacts(t *testing.T, day time.Time, planKeys ...string) map[string]int64 {
	t.Helper()
	h.pinDailyNow(t, day)
	runs := map[string]int64{}
	sequence := 0
	for _, planKey := range planKeys {
		sequence++
		detail, err := h.service.CreatePlanRun(commandContext(t), h.principal, "window-run-"+planKey, planKey)
		if err != nil {
			t.Fatalf("create window run for %s: %v", planKey, err)
		}
		runs[planKey] = detail.RunID
	}
	return runs
}

func TestDailySealAggregatesWindowFactsWithoutFabricatingHealth(t *testing.T) {
	h := newTestHarness(t)
	for _, planKey := range []string{"plan-ok", "plan-gap", "plan-pending", "plan-cancelled", "plan-disabled"} {
		h.seedPlan(t, planKey)
	}
	if _, err := h.db.Exec(`UPDATE inspection_plans SET enabled=0 WHERE plan_key='plan-disabled'`); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, time.September, 27, 10, 0, 0, 0, time.UTC)
	runs := h.seedWindowFacts(t, day, "plan-ok", "plan-gap", "plan-pending")
	// ok 源：真实采集成功。
	okAttempt := h.promqlAttemptID(t, runs["plan-ok"])
	h.dispatchPromQL(t, okAttempt)
	if err := h.service.CommitPluginProposal(context.Background(), okAttempt, "plinth-boot", 1, pluginSuccessProposal(t, h, okAttempt, runs["plan-ok"], "success")); err != nil {
		t.Fatal(err)
	}
	// gap 源：显式查询失败缺口，绝不当作健康。
	gapAttempt := h.promqlAttemptID(t, runs["plan-gap"])
	h.dispatchPromQL(t, gapAttempt)
	if err := h.service.CommitPluginProposal(context.Background(), gapAttempt, "plinth-boot", 1, pluginSuccessProposal(t, h, gapAttempt, runs["plan-gap"], "gap")); err != nil {
		t.Fatal(err)
	}
	// plan-pending 的 Run 保持 Running；plan-cancelled 的 Run 已被取消（无任何
	// 已收敛检查）；plan-disabled 计划停用且无 Run。
	cancelledRun, err := h.service.CreatePlanRun(commandContext(t), h.principal, "window-run-cancelled", "plan-cancelled")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CancelRun(commandContext(t), h.principal, "cancel-window-run", cancelledRun.RunID, cancelledRun.RowVersion); err != nil {
		t.Fatal(err)
	}
	h.seedDailyConfig(t, dailyTestConfigInput("core-daily", "plan-ok", "plan-gap", "plan-pending", "plan-cancelled", "plan-disabled"))

	boundary := time.Date(2026, time.September, 28, 6, 0, 0, 0, time.UTC)
	h.pinDailyNow(t, boundary.Add(2*time.Hour))
	if err := h.service.CreateScheduledDailyReport(context.Background(), DailyReportConfig{ConfigKey: "core-daily", Timezone: "UTC"}, boundary); err != nil {
		t.Fatal(err)
	}
	if err := h.service.SealDueDailyReports(context.Background(), boundary.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	detail, err := h.service.GetDailyReport(context.Background(), "core-daily", "2026-09-27")
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != "Sealed" || detail.Latest == nil {
		t.Fatalf("sealed detail = %+v", detail.DailyReportSummary)
	}
	content := detail.Latest
	if content.SchemaKind != dailyReportContentKind || content.LocalDate != "2026-09-27" {
		t.Fatalf("sealed content header = %+v", content)
	}
	sources := map[string]dailySourceReport{}
	for _, source := range content.Sources {
		sources[source.PlanKey] = source
	}
	if len(sources) != 5 {
		t.Fatalf("sources = %d, want every contribution listed", len(sources))
	}
	okSource := sources["plan-ok"]
	if okSource.Status != "ok" || len(okSource.GapReasons) != 0 || len(okSource.Checks) != 1 || okSource.Checks[0].Status != "ok" {
		t.Fatalf("ok source = %+v", okSource)
	}
	gapSource := sources["plan-gap"]
	if gapSource.Status != "gap" || len(gapSource.GapReasons) != 0 || gapSource.Checks[0].Status != "gap" || gapSource.Checks[0].GapReason == nil || *gapSource.Checks[0].GapReason != "query_failed" {
		t.Fatalf("gap source = %+v, want explicit query_failed check fact", gapSource)
	}
	if gapSource.Checks[0].ObservedAt == nil {
		t.Fatal("gap check must still carry its frozen observation time")
	}
	pendingSource := sources["plan-pending"]
	if pendingSource.Status != "gap" || len(pendingSource.GapReasons) != 1 || pendingSource.GapReasons[0] != dailyGapCutoffExceeded {
		t.Fatalf("pending source = %+v, want cutoff_exceeded gap", pendingSource)
	}
	cancelledSource := sources["plan-cancelled"]
	if cancelledSource.Status != "gap" || len(cancelledSource.GapReasons) != 1 || cancelledSource.GapReasons[0] != dailyGapRunCancelled {
		t.Fatalf("cancelled-run source = %+v, want run_cancelled gap", cancelledSource)
	}
	disabledSource := sources["plan-disabled"]
	if disabledSource.Status != "gap" || len(disabledSource.GapReasons) != 2 {
		t.Fatalf("disabled source = %+v, want plan_disabled + no_collection", disabledSource)
	}
	if !strings.Contains(strings.Join(disabledSource.GapReasons, ","), dailyGapPlanDisabled) || !strings.Contains(strings.Join(disabledSource.GapReasons, ","), dailyGapNoCollection) {
		t.Fatalf("disabled source reasons = %v", disabledSource.GapReasons)
	}
	if content.Totals.ChecksOK != 1 || content.Totals.ChecksGap != 1 || content.Totals.SourcesGap != 4 {
		t.Fatalf("totals = %+v, want one ok check, one gap check, four gap sources", content.Totals)
	} // 冻结身份不可改写：SQL 触发器拒绝任何窗口/贡献改写。
	var reportID int64
	if err := h.db.QueryRow(`SELECT id FROM inspection_daily_reports WHERE config_key='core-daily'`).Scan(&reportID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE inspection_daily_reports SET window_start_utc='2026-09-26T00:00:00Z' WHERE id=?`, reportID); err == nil {
		t.Fatal("frozen window must be SQL-immutable")
	}
	if _, err := h.db.Exec(`UPDATE inspection_daily_report_versions SET content='{}' WHERE report_id=? AND version=1`, reportID); err == nil {
		t.Fatal("sealed version content must be SQL-immutable")
	}
}

func TestDailySealWaitsForCutoffAndNeverSealsEarly(t *testing.T) {
	h := newTestHarness(t)
	h.seedPlan(t, "plan-pending")
	day := time.Date(2026, time.September, 27, 10, 0, 0, 0, time.UTC)
	h.seedWindowFacts(t, day, "plan-pending")
	h.seedDailyConfig(t, dailyTestConfigInput("core-daily", "plan-pending"))

	boundary := time.Date(2026, time.September, 28, 6, 0, 0, 0, time.UTC)
	if err := h.service.CreateScheduledDailyReport(context.Background(), DailyReportConfig{ConfigKey: "core-daily", Timezone: "UTC"}, boundary); err != nil {
		t.Fatal(err)
	}
	// 截止之前：绝不封存。
	if err := h.service.SealDueDailyReports(context.Background(), boundary.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := h.db.QueryRow(`SELECT state FROM inspection_daily_reports WHERE config_key='core-daily'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "Collecting" {
		t.Fatalf("state before cutoff = %s, want Collecting", state)
	}
	var versions int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_daily_report_versions`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 0 {
		t.Fatalf("pre-cutoff versions = %d, want 0", versions)
	}
	// 截止之时：Running 的 Run 成为显式超时缺口并封存。
	if err := h.service.SealDueDailyReports(context.Background(), boundary.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT state FROM inspection_daily_reports WHERE config_key='core-daily'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "Sealed" {
		t.Fatalf("state at cutoff = %s, want Sealed", state)
	}
	var runState string
	if err := h.db.QueryRow(`SELECT state FROM inspection_runs WHERE id=?`, 0).Scan(&runState); err == nil {
		t.Fatal("unexpected run probe success")
	}
	detail, err := h.service.GetDailyReport(context.Background(), "core-daily", "2026-09-27")
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Latest.Sources) != 1 || detail.Latest.Sources[0].Status != "gap" {
		t.Fatalf("sealed sources = %+v", detail.Latest.Sources)
	}
}

func TestManualDailyReportBackfillKeepsOriginalDate(t *testing.T) {
	h := newTestHarness(t)
	h.seedPlan(t, "plan-a")
	h.seedDailyConfig(t, dailyTestConfigInput("core-daily", "plan-a"))
	now := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	h.pinDailyNow(t, now)
	ctx := commandContext(t)

	if _, err := h.service.CreateManualDailyReport(ctx, h.principal, "backfill-1", "core-daily", "2026-09-27"); err != nil {
		t.Fatal(err)
	}
	var localDate, windowStart, windowEnd, triggerKind, state, cutoffAt string
	var scheduledFor sql.NullString
	if err := h.db.QueryRow(`SELECT local_date,window_start_utc,window_end_utc,trigger_kind,scheduled_for,state,cutoff_at
		FROM inspection_daily_reports WHERE config_key='core-daily'`).Scan(&localDate, &windowStart, &windowEnd, &triggerKind, &scheduledFor, &state, &cutoffAt); err != nil {
		t.Fatal(err)
	}
	if localDate != "2026-09-27" || windowStart != "2026-09-27T00:00:00Z" || windowEnd != "2026-09-28T00:00:00Z" {
		t.Fatalf("backfill window = [%s,%s) for %s, want the requested original date", windowStart, windowEnd, localDate)
	}
	if triggerKind != "manual" || scheduledFor.Valid || state != "Collecting" {
		t.Fatalf("backfill trigger facts = %s/%v/%s", triggerKind, scheduledFor, state)
	}
	if cutoffAt != now.Add(2*time.Hour).Format(time.RFC3339Nano) {
		t.Fatalf("manual cutoff = %s, want trigger+2h", cutoffAt)
	}
	// 同一 (config, date) 的第二次补跑是确定性拒绝；未结束的今天被拒绝；
	// 畸形日期被拒绝；未知配置被拒绝。
	if _, err := h.service.CreateManualDailyReport(ctx, h.principal, "backfill-2", "core-daily", "2026-09-27"); err == nil {
		t.Fatal("duplicate backfill must be rejected")
	} else {
		var conflict *PlanConflictError
		if !errors.As(err, &conflict) || conflict.Code != "daily_report_exists" {
			t.Fatalf("duplicate backfill rejection = %v", err)
		}
	}
	if _, err := h.service.CreateManualDailyReport(ctx, h.principal, "backfill-3", "core-daily", "2026-09-28"); err == nil {
		t.Fatal("backfilling the not-yet-ended local day must be rejected")
	}
	if _, err := h.service.CreateManualDailyReport(ctx, h.principal, "backfill-4", "core-daily", "2026-9-7"); err == nil {
		t.Fatal("malformed local date must be rejected")
	}
	if _, err := h.service.CreateManualDailyReport(ctx, h.principal, "backfill-5", "missing-config", "2026-09-27"); err == nil {
		t.Fatal("unknown config must be rejected")
	}
}

func TestDailyReportRerunAppendsVersionAndKeepsOldReadable(t *testing.T) {
	h := newTestHarness(t)
	h.seedPlan(t, "plan-late")
	day := time.Date(2026, time.September, 27, 10, 0, 0, 0, time.UTC)
	runs := h.seedWindowFacts(t, day, "plan-late")
	h.seedDailyConfig(t, dailyTestConfigInput("core-daily", "plan-late"))

	boundary := time.Date(2026, time.September, 28, 6, 0, 0, 0, time.UTC)
	h.pinDailyNow(t, boundary.Add(2*time.Hour))
	if err := h.service.CreateScheduledDailyReport(context.Background(), DailyReportConfig{ConfigKey: "core-daily", Timezone: "UTC"}, boundary); err != nil {
		t.Fatal(err)
	}
	if err := h.service.SealDueDailyReports(context.Background(), boundary.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	first, err := h.service.GetDailyReport(context.Background(), "core-daily", "2026-09-27")
	if err != nil {
		t.Fatal(err)
	}
	if first.LatestVersion != 1 || first.Latest == nil || first.Latest.Sources[0].Status != "gap" {
		t.Fatalf("v1 = %+v", first.DailyReportSummary)
	}
	v1Raw, err := h.service.GetDailyReportVersion(context.Background(), "core-daily", "2026-09-27", 1)
	if err != nil {
		t.Fatal(err)
	}

	// 迟到的采集在封存后收敛；人工重分析从同一冻结窗口生成 v2，迟到事实
	// 进入新版本，旧版本原样可读。
	lateAttempt := h.promqlAttemptID(t, runs["plan-late"])
	h.dispatchPromQL(t, lateAttempt)
	if err := h.service.CommitPluginProposal(context.Background(), lateAttempt, "plinth-boot", 1, pluginSuccessProposal(t, h, lateAttempt, runs["plan-late"], "success")); err != nil {
		t.Fatal(err)
	}
	ctx := commandContext(t)
	summary, err := h.service.RerunDailyReport(ctx, h.principal, "rerun-1", "core-daily", "2026-09-27")
	if err != nil {
		t.Fatal(err)
	}
	if summary.LatestVersion != 2 {
		t.Fatalf("rerun summary = %+v, want version 2", summary)
	}
	second, err := h.service.GetDailyReport(context.Background(), "core-daily", "2026-09-27")
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Versions) != 2 || second.Versions[0].Version != 2 {
		t.Fatalf("versions = %+v, want [2 1]", second.Versions)
	}
	if second.Latest.Sources[0].Status != "ok" || second.Latest.Totals.ChecksOK != 1 {
		t.Fatalf("v2 content = %+v, want the late ok fact", second.Latest)
	}
	v1After, err := h.service.GetDailyReportVersion(context.Background(), "core-daily", "2026-09-27", 1)
	if err != nil {
		t.Fatal(err)
	}
	if v1After != v1Raw {
		t.Fatal("v1 content changed after the rerun")
	}
	// 未封存的报告不能重分析。
	if _, err := h.service.RerunDailyReport(ctx, h.principal, "rerun-2", "core-daily", "2026-09-26"); err == nil {
		t.Fatal("rerun of a missing report must be rejected")
	}
	var collected int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_daily_report_versions`).Scan(&collected); err != nil {
		t.Fatal(err)
	}
	if collected != 2 {
		t.Fatalf("version rows = %d, want exactly 2", collected)
	}
}

// TestDailyReportReadModelsFailClosed 验证读路径：未配置 reader 的 Service
// 拒绝全部日报读操作，Get 对缺失对象返回 ErrNotFound。
func TestDailyReportReadModelsFailClosed(t *testing.T) {
	h := newTestHarness(t)
	h.seedPlan(t, "plan-a")
	unwired := NewService(h.db)
	unwired.now = h.service.now
	if _, err := unwired.ListDailyReportConfigs(context.Background()); err == nil {
		t.Fatal("unwired reader must fail closed")
	}
	if _, err := unwired.ListDailyReports(context.Background(), "", 10); err == nil {
		t.Fatal("unwired reader must fail closed")
	}
	if _, err := h.service.GetDailyReport(context.Background(), "core-daily", "2026-09-27"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing report error = %v, want ErrNotFound", err)
	}
	var nullRow sql.NullString
	if err := h.db.QueryRow(`SELECT config_key FROM inspection_daily_report_configs WHERE config_key='none'`).Scan(&nullRow); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("probe query should miss")
	}
}
