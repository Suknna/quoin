package inspection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/testfixture"
	"github.com/Suknna/quoin/test/plugins/synthetic"
)

func setupSyntheticPlan(t *testing.T) (*testHarness, *plugins.Registry, int64) {
	t.Helper()
	h := newTestHarness(t)
	registry := plugins.NewRegistry()
	if err := registry.Register(synthetic.Plugin()); err != nil {
		t.Fatal(err)
	}
	if err := h.service.UsePluginRegistry(registry, []string{"synthetic-plugin"}); err != nil {
		t.Fatal(err)
	}
	catalogs, err := attempt.BuildCatalogs(registry, []string{"synthetic-plugin"})
	if err != nil {
		t.Fatal(err)
	}
	h.service.Attempts().Catalogs = catalogs
	connectionID := testfixture.SeedHTTPConnectionPair(t, h.db, "synthetic-platform", synthetic.ConnectionKindValue, time.Now())
	plan, err := h.service.CreatePlan(commandContext(t), h.principal, "synthetic-plan-create", PlanInput{
		PlanKey: "synthetic-plan", DisplayName: "Synthetic check", Enabled: true,
		ConnectionName: "synthetic-platform", PluginID: "synthetic-plugin",
		TemplateID: synthetic.TemplateID, Params: map[string]any{"expression": "echo"},
		ScopeKind: "integration", Timezone: "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.PluginID != "synthetic-plugin" || plan.TemplateID != synthetic.TemplateID {
		t.Fatalf("synthetic plan=%+v", plan)
	}
	return h, registry, connectionID
}

func TestSyntheticTemplateCreatesPlanThroughRegisteredConnectionKind(t *testing.T) {
	h, registry, connectionID := setupSyntheticPlan(t)
	invalid := []map[string]any{
		{},
		{"expression": ""},
		{"expression": "echo", "unexpected": true},
	}
	for index, params := range invalid {
		_, err := h.service.CreatePlan(commandContext(t), h.principal, fmt.Sprintf("synthetic-invalid-%d", index), PlanInput{
			PlanKey: fmt.Sprintf("synthetic-invalid-%d", index), DisplayName: "Invalid synthetic params", Enabled: true,
			ConnectionName: "synthetic-platform", PluginID: "synthetic-plugin",
			TemplateID: synthetic.TemplateID, Params: params,
			ScopeKind: "integration", Timezone: "UTC",
		})
		var conflict *PlanConflictError
		if !errors.As(err, &conflict) || conflict.Code != "invalid_params" {
			t.Fatalf("params=%v accepted or incorrectly rejected: %v", params, err)
		}
	}
	if err := h.service.EnsureDefaultPlan(commandContext(t), connectionID, "synthetic-platform"); err != nil {
		t.Fatal(err)
	}
	var defaults int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_plans WHERE plan_key=?`, DefaultPlanKey("synthetic-platform")).Scan(&defaults); err != nil || defaults != 0 {
		t.Fatalf("synthetic plugin declared no starter plan but got %d (err=%v)", defaults, err)
	}
	if err := h.service.UsePluginRegistry(registry, []string{}); err != nil {
		t.Fatal(err)
	}
	_, err := h.service.CreatePlan(commandContext(t), h.principal, "synthetic-disabled-plan", PlanInput{
		PlanKey: "synthetic-disabled-plan", DisplayName: "Disabled plugin", Enabled: true,
		ConnectionName: "synthetic-platform", PluginID: "synthetic-plugin",
		TemplateID: synthetic.TemplateID, Params: map[string]any{"expression": "echo"},
		ScopeKind: "integration", Timezone: "UTC",
	})
	var conflict *PlanConflictError
	if !errors.As(err, &conflict) || conflict.Code != "unsupported_connection" {
		t.Fatalf("disabled plugin plan err=%v, want unsupported_connection", err)
	}
}

func TestSyntheticCheckProducesFrozenDailyFactsAndAgentAttempt(t *testing.T) {
	h, _, _ := setupSyntheticPlan(t)
	h.seedModelProvider(t)
	day := time.Now().UTC().Truncate(time.Second)
	h.pinDailyNow(t, day)
	run, err := h.service.CreatePlanRun(commandContext(t), h.principal, "synthetic-real-run", "synthetic-plan")
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "Running" {
		t.Fatalf("synthetic run state=%s", run.State)
	}
	childID := h.promqlAttemptID(t, run.RunID)
	var purpose string
	if err := h.db.QueryRow(`SELECT purpose FROM attempt_connection_grants WHERE attempt_id=?`, childID).Scan(&purpose); err != nil || purpose != synthetic.CollectionGrantPurpose {
		t.Fatalf("collection grant purpose=%s err=%v", purpose, err)
	}
	h.dispatchPromQL(t, childID)
	proposal, err := json.Marshal(map[string]any{
		"schemaKind": "inspection_plugin_result_v1", "attemptId": childID,
		"inspectionRunId": run.RunID, "checkKey": synthetic.TemplateID,
		"outcome": "success", "observedAt": day.Format(time.RFC3339Nano),
		"executionWindow": nil, "result": map[string]any{"expression": "echo", "value": "ok"},
		"warnings": []string{}, "errors": []string{}, "gapReason": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.service.CommitPluginProposal(context.Background(), childID, "plinth-boot", 1, proposal); err != nil {
		t.Fatal(err)
	}
	var resultJSON string
	if err := h.db.QueryRow(`SELECT e.result_json FROM evidence e JOIN inspection_check_results x ON x.evidence_id=e.id WHERE x.run_id=? AND x.check_key=?`, run.RunID, synthetic.TemplateID).Scan(&resultJSON); err != nil || resultJSON != `{"expression":"echo","value":"ok"}` {
		t.Fatalf("synthetic Evidence result=%q err=%v", resultJSON, err)
	}
	_, err = h.service.CreateDailyReportConfig(commandContext(t), h.principal, "synthetic-daily-config", dailyTestConfigInput("synthetic-daily", "synthetic-plan"))
	if err != nil {
		t.Fatal(err)
	}
	nextDay := time.Date(day.Year(), day.Month(), day.Day()+1, 9, 0, 0, 0, time.UTC)
	h.pinDailyNow(t, nextDay)
	localDate := day.Format("2006-01-02")
	report, err := h.service.CreateManualDailyReport(commandContext(t), h.principal, "synthetic-daily-manual", "synthetic-daily", localDate)
	if err != nil {
		t.Fatal(err)
	}
	cutoff := nextDay.Add(2*time.Hour + time.Second)
	h.pinDailyNow(t, cutoff)
	if err := h.service.SealDueDailyReports(context.Background(), cutoff); err != nil {
		t.Fatal(err)
	}
	sealed, err := h.service.GetDailyReport(context.Background(), "synthetic-daily", localDate)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.State != "Sealed" || sealed.Latest == nil || len(sealed.Latest.Sources) != 1 || len(sealed.Latest.Sources[0].Checks) != 1 {
		t.Fatalf("synthetic daily report=%+v", sealed)
	}
	check := sealed.Latest.Sources[0].Checks[0]
	if check.EvidenceID == nil || string(check.Result) != resultJSON || check.Measurement != nil || check.Status != "ok" {
		t.Fatalf("synthetic frozen check=%+v, want exact generic JSON Evidence", check)
	}
	if err := h.service.EnsureDueDailyReportAnalyses(context.Background()); err != nil {
		t.Fatal(err)
	}
	reportID, err := strconv.ParseInt(report.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	var agentAttempt int64
	if err := h.db.QueryRow(`SELECT id FROM execution_attempts WHERE attempt_type='inspection_daily_analysis' AND scope_type='daily_report' AND scope_id=?`, reportID).Scan(&agentAttempt); err != nil {
		t.Fatalf("daily Agent attempt missing for synthetic facts: %v", err)
	}
	if _, err := h.service.Attempts().DispatchInputFor(commandContext(t), agentAttempt); err != nil {
		t.Fatalf("synthetic daily Agent frozen input cannot be rebuilt: %v", err)
	}
	h.bindAcceptDailyAnalysis(t, agentAttempt)
	promptDigest := strings.Repeat("d", 64)
	callID := h.seedSucceededModelCall(t, agentAttempt, promptDigest)
	analysis := dailyAnalysisProposalBody(agentAttempt, reportID, "synthetic-daily", localDate, 1, callID,
		"合成来源已采证：value=ok；引用该日唯一的运行与证据编号。", promptDigest)
	if err := h.service.CommitDailyAnalysisProposal(context.Background(), agentAttempt, "plinth-boot", 1, analysis); err != nil {
		t.Fatalf("synthetic daily Agent result: %v", err)
	}
	analyses, err := h.service.ListDailyReportAnalyses(context.Background(), "synthetic-daily", localDate)
	if err != nil || len(analyses) != 1 || analyses[0].ReportVersion != 1 || analyses[0].AttemptState != "Succeeded" {
		t.Fatalf("synthetic daily Agent analyses=%+v err=%v", analyses, err)
	}
}
