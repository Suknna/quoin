package inspection

// 巡检分析冻结行为测试：计划的可选分析语义字段（检查说明/单位/初始报告要求）
// 随 Run 冻结、重采证复制、自动分析用冻结值、重分析仅本次覆盖、逐检查项结构
// 化清单与重建摘要稳定性、缺口显式可见且不伪造执行事实。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// createSemanticsPlan 通过命令面创建携带分析语义的计划（校验走真实路径）。
func (h *testHarness) createSemanticsPlan(t *testing.T, clientCommandID, planKey string, input PlanInput) Plan {
	t.Helper()
	ctx := commandContext(t)
	plan, err := h.service.CreatePlan(ctx, h.principal, clientCommandID, input)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestPlanAnalysisSemanticsRoundTripAndValidation(t *testing.T) {
	h := newTestHarness(t)
	ctx := commandContext(t)
	base := func() PlanInput {
		return PlanInput{
			PlanKey: "semantics-plan", DisplayName: "语义计划", Enabled: true,
			ConnectionName: "fixture-metrics", PluginID: "thanos", TemplateID: "promql_instant",
			Params: map[string]any{"expression": "up"}, ScopeKind: "integration", Timezone: "UTC",
		}
	}
	withSemantics := base()
	withSemantics.CheckDescription = "检查 Prometheus 连通性"
	withSemantics.MetricUnit = "比率"
	withSemantics.ReportInstructions = "报告必须给出每个检查项的结论。"
	plan := h.createSemanticsPlan(t, "semantics-create-0001", "semantics-plan", withSemantics)
	if plan.CheckDescription == nil || plan.MetricUnit == nil || plan.ReportInstructions == nil {
		t.Fatalf("created plan lost analysis semantics: %+v", plan)
	}
	if *plan.CheckDescription != "检查 Prometheus 连通性" || *plan.MetricUnit != "比率" {
		t.Fatalf("plan semantics wrong: %+v", plan)
	}
	// 更新语义合法；清空单位存储为 NULL。
	cleared := base()
	cleared.CheckDescription = "新说明"
	cleared.ReportInstructions = "新要求"
	updated, err := h.service.UpdatePlan(ctx, h.principal, "semantics-update-0001", cleared, plan.RowVersion)
	if err != nil {
		t.Fatal(err)
	}
	if updated.MetricUnit != nil {
		t.Fatalf("cleared unit must store NULL, got %q", *updated.MetricUnit)
	}
	// 长度上界逐一拒绝。
	cases := []struct {
		name        string
		description string
		unit        string
		instruction string
	}{
		{"description", strings.Repeat("说", 2001), "", ""},
		{"unit", "", strings.Repeat("单", 101), ""},
		{"instructions", "", "", strings.Repeat("报", 4001)},
	}
	for _, testCase := range cases {
		rejected := base()
		rejected.CheckDescription = testCase.description
		rejected.MetricUnit = testCase.unit
		rejected.ReportInstructions = testCase.instruction
		if _, err := h.service.UpdatePlan(ctx, h.principal, "semantics-reject-"+testCase.name, rejected, updated.RowVersion); err == nil {
			t.Fatalf("%s over length must reject", testCase.name)
		}
	}
}

// TestRunFreezesAnalysisSemanticsAndPlanChangesDoNotRewrite proves 名称/检查说
// 明/单位/初始报告要求随 Run 冻结，计划后续修改不改写已存在 Run，重采证逐字
// 段复制源 Run 的冻结语义。
func TestRunFreezesAnalysisSemanticsAndPlanChangesDoNotRewrite(t *testing.T) {
	h := newTestHarness(t)
	ctx := commandContext(t)
	input := PlanInput{
		PlanKey: "freeze-plan", DisplayName: "语义计划 freeze-plan", Enabled: true,
		ConnectionName: "fixture-metrics", PluginID: "thanos", TemplateID: "promql_instant",
		Params: map[string]any{"expression": "up"}, ScopeKind: "integration", Timezone: "UTC",
		CheckDescription: "连通性检查", MetricUnit: "布尔", ReportInstructions: "初始要求 v1",
	}
	plan := h.createSemanticsPlan(t, "freeze-create-0001", "freeze-plan", input)
	run, err := h.service.CreatePlanRun(ctx, h.principal, "freeze-run-0001", "freeze-plan")
	if err != nil {
		t.Fatal(err)
	}
	if run.FrozenConfig == nil || run.FrozenConfig.DisplayName == nil ||
		run.FrozenConfig.CheckDescription == nil || run.FrozenConfig.MetricUnit == nil || run.FrozenConfig.ReportInstructions == nil {
		t.Fatalf("run must freeze all analysis semantics: %+v", run.FrozenConfig)
	}
	if *run.FrozenConfig.ReportInstructions != "初始要求 v1" || *run.FrozenConfig.CheckDescription != "连通性检查" || *run.FrozenConfig.MetricUnit != "布尔" {
		t.Fatalf("frozen semantics wrong: %+v", run.FrozenConfig)
	}
	if *run.FrozenConfig.DisplayName != "语义计划 freeze-plan" {
		t.Fatalf("frozen display name = %q", *run.FrozenConfig.DisplayName)
	}
	// 计划后续修改不改写已存在 Run。
	input.DisplayName = "改名后的计划"
	input.CheckDescription = "被修改的说明"
	input.MetricUnit = "被修改单位"
	input.ReportInstructions = "被修改要求"
	if _, err := h.service.UpdatePlan(ctx, h.principal, "freeze-update-0001", input, plan.RowVersion); err != nil {
		t.Fatal(err)
	}
	after, err := h.service.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if *after.FrozenConfig.ReportInstructions != "初始要求 v1" || *after.FrozenConfig.DisplayName != "语义计划 freeze-plan" {
		t.Fatalf("plan edit rewrote frozen run semantics: %+v", after.FrozenConfig)
	}
	// 重采证逐字段复制源 Run 冻结语义，不读计划当前定义。
	if _, err := h.service.CancelRun(ctx, h.principal, "freeze-cancel-0001", run.RunID, after.RowVersion); err != nil {
		t.Fatal(err)
	}
	rerun, err := h.service.RerunInspection(ctx, h.principal, "freeze-rerun-0001", run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if rerun.FrozenConfig == nil || *rerun.FrozenConfig.ReportInstructions != "初始要求 v1" ||
		*rerun.FrozenConfig.CheckDescription != "连通性检查" || *rerun.FrozenConfig.MetricUnit != "布尔" {
		t.Fatalf("rerun must copy the source run frozen semantics: %+v", rerun.FrozenConfig)
	}
}

// completeChecklistRun creates a plan with frozen semantics, runs one instant
// collection to success, and returns the run id.
func completeChecklistRun(t *testing.T, h *testHarness) int64 {
	t.Helper()
	h.seedModelProvider(t)
	store, err := artifact.NewStore(h.db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.service.SetArtifactWriter(store.MaterializeEvidenceTransaction)
	if _, err := h.service.CreatePlan(commandContext(t), h.principal, "checklist-create-0001", PlanInput{
		PlanKey: "checklist-plan", DisplayName: "清单计划", Enabled: true,
		ConnectionName: "fixture-metrics", PluginID: "thanos", TemplateID: "promql_instant",
		Params: map[string]any{"expression": "up"}, ScopeKind: "integration", Timezone: "UTC",
		CheckDescription: "连通性检查", MetricUnit: "1=在线", ReportInstructions: "初始报告要求 v1",
	}); err != nil {
		t.Fatal(err)
	}
	run, err := h.service.CreatePlanRun(commandContext(t), h.principal, "checklist-run-0001", "checklist-plan")
	if err != nil {
		t.Fatal(err)
	}
	instantAttempt := h.promqlAttemptID(t, run.RunID)
	h.dispatchPromQL(t, instantAttempt)
	if err := h.service.CommitPluginProposal(context.Background(), instantAttempt, "plinth-boot", 1, pluginSuccessProposal(t, h, instantAttempt, run.RunID, "success")); err != nil {
		t.Fatal(err)
	}
	return run.RunID
}

// TestAnalysisSnapshotCarriesStructuredChecklistAndFrozenInstructions walks the
// real dispatch chain: the frozen analysis input must carry the per-check
// structured list (identity/semantics/expression/observedAt/evidence mapping),
// the run-frozen instructions, and rebuild to the exact same bytes.
func TestAnalysisSnapshotCarriesStructuredChecklistAndFrozenInstructions(t *testing.T) {
	h := newTestHarness(t)
	runID := completeChecklistRun(t, h)
	ctx := commandContext(t)
	analysisID := h.analysisAttemptID(t, runID)
	attempts := h.service.Attempts()
	input, err := attempts.DispatchInputFor(ctx, analysisID)
	if err != nil {
		t.Fatalf("analysis dispatch rebuild: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(input.CanonicalJSON, &parsed); err != nil {
		t.Fatal(err)
	}
	plan, ok := parsed["plan"].(map[string]any)
	if !ok {
		t.Fatalf("plan context missing: %v", parsed)
	}
	if plan["reportInstructions"] != "初始报告要求 v1" || plan["checkDescription"] != "连通性检查" || plan["metricUnit"] != "1=在线" {
		t.Fatalf("frozen analysis semantics missing from input: %v", plan)
	}
	checks, ok := parsed["checks"].([]any)
	if !ok || len(checks) != 1 {
		t.Fatalf("checks list = %v", parsed["checks"])
	}
	check := checks[0].(map[string]any)
	if check["checkKey"] != "promql_instant" || check["displayName"] != "PromQL 即时查询" {
		t.Fatalf("check identity wrong: %v", check)
	}
	if check["expression"] != "up" || check["status"] != "ok" {
		t.Fatalf("check item shape wrong: %v", check)
	}
	if check["evidenceId"] == nil || check["artifactId"] == nil {
		t.Fatalf("check item must map evidence and artifact: %v", check)
	}
	if check["observedAt"] == nil {
		t.Fatalf("check item must carry observedAt: %v", check)
	}
	// 重建确定性：重复重建必须产出逐字节相同的结果。
	again, err := attempts.DispatchInputFor(ctx, analysisID)
	if err != nil {
		t.Fatal(err)
	}
	if string(again.CanonicalJSON) != string(input.CanonicalJSON) {
		t.Fatal("rebuild must be byte-stable")
	}
}

// TestReanalysisOverrideFrozenPerAttemptAndPlanIndependence proves the
// re-analysis override: effective instructions freeze per attempt, the run's
// frozen requirement stays untouched, and the plan's current definition is
// never read back into any snapshot.
func TestReanalysisOverrideFrozenPerAttemptAndPlanIndependence(t *testing.T) {
	h := newTestHarness(t)
	runID := completeChecklistRun(t, h)
	ctx := commandContext(t)
	// 先把首个分析驱动到 Succeeded（提交首版报告），否则 Run 已有活动分析。
	firstAnalysis := h.analysisAttemptID(t, runID)
	if err := h.attempts.BindToSlot(ctx, firstAnalysis, "plinth", "plinth-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.Accept(ctx, firstAnalysis, "plinth-boot", 1); err != nil {
		t.Fatal(err)
	}
	promptDigest := strings.Repeat("b", 64)
	callID := h.seedSucceededModelCall(t, firstAnalysis, promptDigest)
	evidenceIDs := evidenceIDsForRun(t, h, runID)
	artifactIDs := artifactIDsForAttempt(t, h, firstAnalysis)
	if err := h.service.CommitReportProposal(context.Background(), firstAnalysis, "plinth-boot", 1, reportProposalBody(firstAnalysis, runID, callID, "首版报告", evidenceIDs, artifactIDs, promptDigest)); err != nil {
		t.Fatal(err)
	}
	override := "只看异常项，报告保持简短"
	next, err := h.service.ReanalyzeRun(ctx, h.principal, "override-reanalyze-0001", runID, &override)
	if err != nil {
		t.Fatal(err)
	}
	var storedOverride sql.NullString
	if err := h.db.QueryRow(`SELECT report_instructions_override FROM inspection_analysis_requirements WHERE attempt_id=?`, next.AttemptID).Scan(&storedOverride); err != nil {
		t.Fatal(err)
	}
	if !storedOverride.Valid || storedOverride.String != override {
		t.Fatalf("override not frozen per attempt: %+v", storedOverride)
	}
	// Run 冻结的初始要求不被覆盖改写。
	detail, err := h.service.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if *detail.FrozenConfig.ReportInstructions != "初始报告要求 v1" {
		t.Fatalf("override rewrote frozen run requirement: %q", *detail.FrozenConfig.ReportInstructions)
	}
	// 重建携带仅本次覆盖，且计划语义仍来自 Run 冻结值。
	attempts := h.service.Attempts()
	input, err := attempts.DispatchInputFor(ctx, next.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(input.CanonicalJSON, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["reportInstructionsOverride"] != override {
		t.Fatalf("rebuilt override = %v", parsed["reportInstructionsOverride"])
	}
	plan := parsed["plan"].(map[string]any)
	if plan["reportInstructions"] != "初始报告要求 v1" {
		t.Fatalf("rebuilt plan instructions = %v", plan["reportInstructions"])
	}
	// 计划定义随后被修改：重建仍逐字节稳定（绝不回读当前计划）。
	if _, err := h.db.Exec(`UPDATE inspection_plans SET report_instructions='被修改的要求', check_description='被修改的说明', metric_unit='被修改单位', row_version=row_version+1 WHERE plan_key='checklist-plan'`); err != nil {
		t.Fatal(err)
	}
	again, err := attempts.DispatchInputFor(ctx, next.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if string(again.CanonicalJSON) != string(input.CanonicalJSON) {
		t.Fatal("rebuild must not read the current plan definition")
	}
	// 超长覆盖拒绝。
	tooLong := strings.Repeat("长", 4001)
	if _, err := h.service.ReanalyzeRun(ctx, h.principal, "override-reanalyze-long", runID, &tooLong); err == nil {
		t.Fatal("over-length override must reject")
	}
}

// TestGapChecksAreVisibleWithoutFabricatingWindows proves gap check items stay
// visible in the checklist with their gap reason and without execution facts.
func TestGapChecksAreVisibleWithoutFabricatingWindows(t *testing.T) {
	h := newTestHarness(t)
	h.seedModelProvider(t)
	store, err := artifact.NewStore(h.db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.service.SetArtifactWriter(store.MaterializeEvidenceTransaction)
	if _, err := h.service.CreatePlan(commandContext(t), h.principal, "gap-create-0001", PlanInput{
		PlanKey: "gap-plan", DisplayName: "缺口计划", Enabled: true,
		ConnectionName: "fixture-metrics", PluginID: "thanos", TemplateID: "promql_range",
		Params:    map[string]any{"expression": "up", "rangeSeconds": float64(300), "stepSeconds": float64(60)},
		ScopeKind: "integration", Timezone: "UTC",
		MetricUnit: "秒", ReportInstructions: "缺口必须可见",
	}); err != nil {
		t.Fatal(err)
	}
	run, err := h.service.CreatePlanRun(commandContext(t), h.principal, "gap-run-0001", "gap-plan")
	if err != nil {
		t.Fatal(err)
	}
	attemptID := h.promqlAttemptID(t, run.RunID)
	h.dispatchPromQL(t, attemptID)
	failed := map[string]any{
		"schemaKind": "inspection_plugin_result_v1", "attemptId": attemptID, "inspectionRunId": run.RunID,
		"checkKey": "promql_range", "outcome": "gap", "observedAt": "2026-09-16T00:00:01Z",
		"executionWindow": nil, "result": nil, "warnings": []string{"collection truncated"},
		"errors": []string{}, "gapReason": "partial_response",
	}
	body, _ := json.Marshal(failed)
	if err := h.service.CommitPluginProposal(context.Background(), attemptID, "plinth-boot", 1, body); err != nil {
		t.Fatal(err)
	}
	analysisID := h.analysisAttemptID(t, run.RunID)
	input, err := h.service.Attempts().DispatchInputFor(commandContext(t), analysisID)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(input.CanonicalJSON, &parsed); err != nil {
		t.Fatal(err)
	}
	check := parsed["checks"].([]any)[0].(map[string]any)
	if check["status"] != "gap" || check["gapReason"] != "partial_response" {
		t.Fatalf("gap check item = %v", check)
	}
	// 缺口保持可见且携带真实观察事实（observedAt/warnings 来自冻结元数据）。
	if check["observedAt"] != "2026-09-16T00:00:01Z" {
		t.Fatalf("gap check must carry its real observedAt: %v", check)
	}
	warnings, ok := check["warnings"].([]any)
	if !ok || len(warnings) != 1 || warnings[0] != "collection truncated" {
		t.Fatalf("gap check must carry real warnings: %v", check)
	}
	// 但绝不伪造成功事实：无 Evidence/Artifact，未携带窗口（提案未带）。
	if check["evidenceId"] != nil || check["artifactId"] != nil || check["windowStartAt"] != nil {
		t.Fatalf("gap check must not fabricate success facts: %v", check)
	}
	if check["expression"] != "up" || check["rangeSeconds"] != float64(300) {
		t.Fatalf("gap check must still carry the frozen query shape: %v", check)
	}
}

// TestRangeWindowFrozenIntoEvidenceAndChecklist proves a successful range
// collection freezes its real execution window into evidence metadata and the
// checklist reports the actual window/step beside the frozen query shape.
func TestRangeWindowFrozenIntoEvidenceAndChecklist(t *testing.T) {
	h := newTestHarness(t)
	h.seedModelProvider(t)
	store, err := artifact.NewStore(h.db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.service.SetArtifactWriter(store.MaterializeEvidenceTransaction)
	if _, err := h.service.CreatePlan(commandContext(t), h.principal, "window-create-0001", PlanInput{
		PlanKey: "window-plan", DisplayName: "窗口计划", Enabled: true,
		ConnectionName: "fixture-metrics", PluginID: "thanos", TemplateID: "promql_range",
		Params:    map[string]any{"expression": "up", "rangeSeconds": float64(300), "stepSeconds": float64(60)},
		ScopeKind: "integration", Timezone: "UTC",
		CheckDescription: "五分钟窗口", MetricUnit: "秒",
	}); err != nil {
		t.Fatal(err)
	}
	run, err := h.service.CreatePlanRun(commandContext(t), h.principal, "window-run-0001", "window-plan")
	if err != nil {
		t.Fatal(err)
	}
	attemptID := h.promqlAttemptID(t, run.RunID)
	h.dispatchPromQL(t, attemptID)
	result := map[string]any{"resultType": "matrix", "result": []any{
		map[string]any{"metric": map[string]string{"job": "quoin"}, "values": []any{[]any{0, "1"}, []any{60, "1"}}},
	}}
	success := map[string]any{
		"schemaKind": "inspection_plugin_result_v1", "attemptId": attemptID, "inspectionRunId": run.RunID,
		"checkKey": "promql_range", "outcome": "success", "observedAt": "2026-09-16T00:05:00Z",
		"executionWindow": map[string]any{"startAt": "2026-09-16T00:00:00Z", "endAt": "2026-09-16T00:05:00Z", "stepSeconds": 60},
		"result":          result, "warnings": []string{}, "errors": []string{}, "gapReason": nil,
	}
	body, _ := json.Marshal(success)
	if err := h.service.CommitPluginProposal(context.Background(), attemptID, "plinth-boot", 1, body); err != nil {
		t.Fatal(err)
	}
	// 真实执行窗口冻结进检查结果元数据（唯一元数据来源；evidence params 保持
	// 冻结闭包校验的 check_key 形状）。
	var params, meta string
	if err := h.db.QueryRow(`SELECT params_json FROM evidence WHERE attempt_id=?`, attemptID).Scan(&params); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(params, `"check_key":"promql_range"`) || strings.Contains(params, "executionWindow") {
		t.Fatalf("evidence params must keep the closure shape without duplicating window: %s", params)
	}
	if err := h.db.QueryRow(`SELECT meta_json FROM inspection_check_results WHERE attempt_id=?`, attemptID).Scan(&meta); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(meta, `"startAt":"2026-09-16T00:00:00Z"`) || !strings.Contains(meta, `"observedAt":"2026-09-16T00:05:00Z"`) {
		t.Fatalf("real window/observedAt not frozen into result meta: %s", meta)
	}
	analysisID := h.analysisAttemptID(t, run.RunID)
	input, err := h.service.Attempts().DispatchInputFor(commandContext(t), analysisID)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(input.CanonicalJSON, &parsed); err != nil {
		t.Fatal(err)
	}
	check := parsed["checks"].([]any)[0].(map[string]any)
	if check["windowStartAt"] != "2026-09-16T00:00:00Z" || check["windowEndAt"] != "2026-09-16T00:05:00Z" || check["executedStepSeconds"] != float64(60) {
		t.Fatalf("checklist real window wrong: %v", check)
	}
	// 冻结的字面量窗口（300/60）与实际窗口并存，语义不混淆。
	if check["rangeSeconds"] != float64(300) || check["stepSeconds"] != float64(60) {
		t.Fatalf("frozen query shape wrong: %v", check)
	}
}

// oldPlanContext/oldReportInput 逐字段复刻语义冻结之前的 analysis 输入形状
// （字段名与顺序一致），用于以真实字节钉住旧形状 Attempt 的重建边界。
type oldPlanContext struct {
	Key    string         `json:"key"`
	Params map[string]any `json:"params"`
	Scope  map[string]any `json:"scope"`
}

type oldReportInput struct {
	SchemaKind         string  `json:"schemaKind"`
	AttemptID          int64   `json:"attemptId"`
	InspectionRunID    int64   `json:"inspectionRunId"`
	ReportVersion      int64   `json:"reportVersion"`
	PlanKey            string  `json:"planKey"`
	EvidenceIDs        []int64 `json:"evidenceIds"`
	ArtifactIDs        []int64 `json:"artifactIds"`
	KnowledgeVersionID []int64 `json:"knowledgeVersionIds"`
	ModelContract      struct {
		ModelID             string `json:"modelId"`
		ContextBudgetTokens int64  `json:"contextBudgetTokens"`
		MaxOutputTokens     int64  `json:"maxOutputTokens"`
	} `json:"modelContract"`
	Plan            oldPlanContext `json:"plan"`
	ConnectionName  string         `json:"connectionName"`
	TemplateID      string         `json:"templateId"`
	TemplateVersion string         `json:"templateVersion"`
}

// TestOldPlanRunAttemptsRebuildWithoutSemantics pins the upgrade boundary: an
// analysis attempt whose snapshot predates the requirements table (built here
// byte-for-byte in the old input shape, exactly what an upgraded database
// holds) must rebuild to those exact bytes — never growing semantic fields
// from the run's frozen columns or the current plan definition.
func TestOldPlanRunAttemptsRebuildWithoutSemantics(t *testing.T) {
	h := newTestHarness(t)
	h.seedModelProvider(t)
	store, err := artifact.NewStore(h.db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.service.SetArtifactWriter(store.MaterializeEvidenceTransaction)
	ctx := commandContext(t)
	if _, err := h.service.CreatePlan(ctx, h.principal, "old-create-0001", PlanInput{
		PlanKey: "old-plan", DisplayName: "旧计划", Enabled: true,
		ConnectionName: "fixture-metrics", PluginID: "thanos", TemplateID: "promql_instant",
		Params: map[string]any{"expression": "up"}, ScopeKind: "integration", Timezone: "UTC",
		CheckDescription: "新语义不应进入旧 Attempt 重建",
	}); err != nil {
		t.Fatal(err)
	}
	run, err := h.service.CreatePlanRun(ctx, h.principal, "old-run-0001", "old-plan")
	if err != nil {
		t.Fatal(err)
	}
	attemptID := h.promqlAttemptID(t, run.RunID)
	h.dispatchPromQL(t, attemptID)
	if err := h.service.CommitPluginProposal(context.Background(), attemptID, "plinth-boot", 1, pluginSuccessProposal(t, h, attemptID, run.RunID, "success")); err != nil {
		t.Fatal(err)
	}
	evidenceIDs := evidenceIDsForRun(t, h, run.RunID)
	if len(evidenceIDs) != 1 {
		t.Fatalf("fixture evidence = %v", evidenceIDs)
	}
	// 采证物化的 report_file artifact 归属该 Evidence。
	var artifactID int64
	if err := h.db.QueryRow(`SELECT id FROM artifacts WHERE owner_type='evidence' AND owner_id=? AND kind='report_file'`, evidenceIDs[0]).Scan(&artifactID); err != nil {
		t.Fatal(err)
	}
	const now = "2026-09-16T00:00:00Z"
	// 首个分析先驱动到 Succeeded：同一 Run 的活动 analysis Attempt 有唯一索引。
	firstAnalysis := h.analysisAttemptID(t, run.RunID)
	if err := h.attempts.BindToSlot(ctx, firstAnalysis, "plinth", "plinth-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.Accept(ctx, firstAnalysis, "plinth-boot", 1); err != nil {
		t.Fatal(err)
	}
	promptDigest := strings.Repeat("a", 64)
	callID := h.seedSucceededModelCall(t, firstAnalysis, promptDigest)
	if err := h.service.CommitReportProposal(context.Background(), firstAnalysis, "plinth-boot", 1, reportProposalBody(firstAnalysis, run.RunID, callID, "首版", evidenceIDs, artifactIDsForAttempt(t, h, firstAnalysis), promptDigest)); err != nil {
		t.Fatal(err)
	}
	legacyAttempt, err := h.db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
		VALUES('inspection_analysis','run',?,'Queued','test','agent',?)`, run.RunID, now)
	if err != nil {
		t.Fatal(err)
	}
	legacyID, _ := legacyAttempt.LastInsertId()
	// 复制新 Attempt 的 chat_model grant：LookupChatContract 据此还原模型契约。
	if _, err := h.db.Exec(`INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at)
		SELECT ?,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,?
		FROM attempt_connection_grants WHERE attempt_id=? AND purpose='chat_model'`, legacyID, now, firstAnalysis); err != nil {
		t.Fatal(err)
	}
	var paramsRaw, scopeRaw string
	if err := h.db.QueryRow(`SELECT frozen_params_json, frozen_scope_json FROM inspection_runs WHERE id=?`, run.RunID).Scan(&paramsRaw, &scopeRaw); err != nil {
		t.Fatal(err)
	}
	old := oldReportInput{
		SchemaKind: reportInputKind, AttemptID: legacyID, InspectionRunID: run.RunID,
		ReportVersion: 1, PlanKey: "old-plan",
		EvidenceIDs: evidenceIDs, ArtifactIDs: []int64{artifactID}, KnowledgeVersionID: []int64{},
		Plan: oldPlanContext{
			Key:    "old-plan",
			Params: map[string]any{}, Scope: map[string]any{},
		},
		ConnectionName: "fixture-metrics", TemplateID: "promql_instant", TemplateVersion: "1",
	}
	if err := json.Unmarshal([]byte(paramsRaw), &old.Plan.Params); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(scopeRaw), &old.Plan.Scope); err != nil {
		t.Fatal(err)
	}
	old.ModelContract.ModelID = "fixture-chat-1"
	old.ModelContract.ContextBudgetTokens = 4096
	old.ModelContract.MaxOutputTokens = 1024
	canonical, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	if _, err := h.db.Exec(`INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,inspection_report_version,created_at)
		VALUES(?, 'inspection_analysis_v1','v1',?,1,?)`, legacyID, hex.EncodeToString(digest[:]), now); err != nil {
		t.Fatal(err)
	}
	var snapshotID int64
	if err := h.db.QueryRow(`SELECT id FROM attempt_input_snapshots WHERE attempt_id=?`, legacyID).Scan(&snapshotID); err != nil {
		t.Fatal(err)
	}
	insertItem := func(seq int, role string, source string, refColumn string, refID int64) {
		t.Helper()
		itemDigest := sha256.Sum256([]byte(source))
		if refColumn == "" {
			if _, err := h.db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest) VALUES(?,?,?,?)`,
				snapshotID, seq, role, hex.EncodeToString(itemDigest[:])); err != nil {
				t.Fatal(err)
			}
			return
		}
		if _, err := h.db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,`+refColumn+`) VALUES(?,?,?, ?,?)`,
			snapshotID, seq, role, hex.EncodeToString(itemDigest[:]), refID); err != nil {
			t.Fatal(err)
		}
	}
	insertItem(1, "inspection_run", fmt.Sprintf("inspection-run:%d", run.RunID), "inspection_run_id", run.RunID)
	var resultID int64
	if err := h.db.QueryRow(`SELECT id FROM inspection_check_results WHERE run_id=?`, run.RunID).Scan(&resultID); err != nil {
		t.Fatal(err)
	}
	insertItem(2, "inspection_check_result", fmt.Sprintf("inspection-check-result:%d", resultID), "inspection_check_result_id", resultID)
	insertItem(3, "inspection_evidence", fmt.Sprintf("evidence:%d", evidenceIDs[0]), "evidence_id", evidenceIDs[0])
	insertItem(4, "inspection_artifact", fmt.Sprintf("artifact:%d", artifactID), "artifact_id", artifactID)
	if _, err := h.db.Exec(`INSERT INTO attempt_artifact_grants(attempt_id,artifact_id,source_kind,source_id,granted_at) VALUES(?,?,'input_snapshot',?,?)`,
		legacyID, artifactID, snapshotID, now); err != nil {
		t.Fatal(err)
	}
	// 重建必须逐字节命中旧形状：没有语义字段，也没有检查项清单。
	attempts := h.service.Attempts()
	input, err := attempts.DispatchInputFor(ctx, legacyID)
	if err != nil {
		t.Fatalf("old-style rebuild must succeed: %v", err)
	}
	if string(input.CanonicalJSON) != string(canonical) {
		t.Fatalf("old attempt rebuild drifted.\nwant: %s\ngot:  %s", canonical, input.CanonicalJSON)
	}
}

// TestRequirementsScopeClosureTrigger 复用周边 trigger 模式：requirements 行
// 只能绑定 inspection_analysis × run 的 Attempt，其它身份一律 ABORT。
func TestRequirementsScopeClosureTrigger(t *testing.T) {
	h := newTestHarness(t)
	runID := completeChecklistRun(t, h)
	analysisID := h.analysisAttemptID(t, runID)
	const now = "2026-09-16T00:00:00Z"
	// 正例由生产路径自证：收敛创建的分析 Attempt 已带有 requirements 行。
	var autoRows int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_analysis_requirements WHERE attempt_id=?`, analysisID).Scan(&autoRows); err != nil {
		t.Fatal(err)
	}
	if autoRows != 1 {
		t.Fatalf("analysis attempt requirements rows = %d, want 1", autoRows)
	}
	// 采证 Attempt 绝不能挂分析要求。
	collectionID := h.promqlAttemptID(t, runID)
	if _, err := h.db.Exec(`INSERT INTO inspection_analysis_requirements(attempt_id,report_instructions_override,created_at) VALUES(?,NULL,?)`, collectionID, now); err == nil {
		t.Fatal("non-analysis attempt must not accept a requirements row")
	} else if !strings.Contains(err.Error(), "inspection_analysis run Attempt") {
		t.Fatalf("closure trigger message = %v", err)
	}
}

// TestReanalysisOverrideTriStateAndRuneLengths 钉住三态语义：nil 继承（列
// NULL）、空串显式清除（列 ”，重建字节含空覆盖字段）、非空仅本次覆盖；长度
// 按 rune 计数（2000 个汉字 = 6000 字节必须接受，4001 个汉字必须拒绝）。
func TestReanalysisOverrideTriStateAndRuneLengths(t *testing.T) {
	h := newTestHarness(t)
	runID := completeChecklistRun(t, h)
	ctx := commandContext(t)
	firstAnalysis := h.analysisAttemptID(t, runID)
	if err := h.attempts.BindToSlot(ctx, firstAnalysis, "plinth", "plinth-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.Accept(ctx, firstAnalysis, "plinth-boot", 1); err != nil {
		t.Fatal(err)
	}
	promptDigest := strings.Repeat("3", 64)
	callID := h.seedSucceededModelCall(t, firstAnalysis, promptDigest)
	if err := h.service.CommitReportProposal(context.Background(), firstAnalysis, "plinth-boot", 1, reportProposalBody(firstAnalysis, runID, callID, "首版", evidenceIDsForRun(t, h, runID), artifactIDsForAttempt(t, h, firstAnalysis), promptDigest)); err != nil {
		t.Fatal(err)
	}
	commit := func(attemptID int64, content, seed string) {
		t.Helper()
		if err := h.attempts.BindToSlot(ctx, attemptID, "plinth", "plinth-boot", 1, time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := h.attempts.Accept(ctx, attemptID, "plinth-boot", 1); err != nil {
			t.Fatal(err)
		}
		promptDigest := strings.Repeat(seed, 64)
		callID := h.seedSucceededModelCall(t, attemptID, promptDigest)
		if err := h.service.CommitReportProposal(context.Background(), attemptID, "plinth-boot", 1, reportProposalBody(attemptID, runID, callID, content, evidenceIDsForRun(t, h, runID), artifactIDsForAttempt(t, h, attemptID), promptDigest)); err != nil {
			t.Fatal(err)
		}
	}
	// nil：继承，列保持 NULL。
	inherited, err := h.service.ReanalyzeRun(ctx, h.principal, "tri-state-nil", runID, nil)
	if err != nil {
		t.Fatal(err)
	}
	commit(inherited.AttemptID, "继承版", "1")
	var nilOverride sql.NullString
	if err := h.db.QueryRow(`SELECT report_instructions_override FROM inspection_analysis_requirements WHERE attempt_id=?`, inherited.AttemptID).Scan(&nilOverride); err != nil {
		t.Fatal(err)
	}
	if nilOverride.Valid {
		t.Fatalf("nil override must store NULL, got %q", nilOverride.String)
	}
	// 空串：显式清除，列存 ''，重建 canonical 携带空覆盖字段。
	cleared, err := h.service.ReanalyzeRun(ctx, h.principal, "tri-state-clear", runID, new(string))
	if err != nil {
		t.Fatal(err)
	}
	commit(cleared.AttemptID, "清除版", "2")
	var clearedOverride sql.NullString
	if err := h.db.QueryRow(`SELECT report_instructions_override FROM inspection_analysis_requirements WHERE attempt_id=?`, cleared.AttemptID).Scan(&clearedOverride); err != nil {
		t.Fatal(err)
	}
	if !clearedOverride.Valid || clearedOverride.String != "" {
		t.Fatalf("explicit clear must store empty string, got %+v", clearedOverride)
	}
	clearedInput, err := h.service.Attempts().DispatchInputFor(ctx, cleared.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(clearedInput.CanonicalJSON), `"reportInstructionsOverride":""`) {
		t.Fatalf("rebuilt input must carry the explicit empty override: %s", clearedInput.CanonicalJSON)
	}
	// rune 计数：2000 个汉字（6000 字节）必须接受——字节计数会错误拒绝。
	longChinese := strings.Repeat("巡", 2000)
	overrideRun, err := h.service.ReanalyzeRun(ctx, h.principal, "tri-state-rune-pass", runID, &longChinese)
	if err != nil {
		t.Fatalf("rune counting must accept 2000 han characters: %v", err)
	}
	commit(overrideRun.AttemptID, "长中文版", "3")
	var stored sql.NullString
	if err := h.db.QueryRow(`SELECT report_instructions_override FROM inspection_analysis_requirements WHERE attempt_id=?`, overrideRun.AttemptID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if utf8.RuneCountInString(stored.String) != 2000 {
		t.Fatalf("stored override rune count = %d", utf8.RuneCountInString(stored.String))
	}
	// 4001 个汉字拒绝。
	tooLong := strings.Repeat("巡", 4001)
	if _, err := h.service.ReanalyzeRun(ctx, h.principal, "tri-state-rune-reject", runID, &tooLong); err == nil {
		t.Fatal("4001 han characters must reject")
	}
}

// TestPlanSemanticsRuneCountsAlignWithSchema 钉住应用侧 rune 计数与 SQLite
// length() 字符计数一致：长中文说明在命令面被接受，超限在两侧同样拒绝。
func TestPlanSemanticsRuneCountsAlignWithSchema(t *testing.T) {
	h := newTestHarness(t)
	ctx := commandContext(t)
	longChinese := strings.Repeat("检", 1500) // 4500 字节、1500 字符：字节计数会错误拒绝
	plan, err := h.service.CreatePlan(ctx, h.principal, "rune-create-0001", PlanInput{
		PlanKey: "rune-plan", DisplayName: "长度计划", Enabled: true,
		ConnectionName: "fixture-metrics", PluginID: "thanos", TemplateID: "promql_instant",
		Params: map[string]any{"expression": "up"}, ScopeKind: "integration", Timezone: "UTC",
		CheckDescription: longChinese, MetricUnit: strings.Repeat("比", 50), ReportInstructions: strings.Repeat("求", 1500),
	})
	if err != nil {
		t.Fatalf("rune-counted semantics must accept Chinese text within limits: %v", err)
	}
	if utf8.RuneCountInString(*plan.CheckDescription) != 1500 {
		t.Fatalf("stored description rune count = %d", utf8.RuneCountInString(*plan.CheckDescription))
	}
	// 2001 个汉字超限拒绝（无论字节还是字符计数都超）。
	tooLong := strings.Repeat("检", 2001)
	if _, err := h.service.UpdatePlan(ctx, h.principal, "rune-update-0001", PlanInput{
		PlanKey: "rune-plan", DisplayName: "长度计划", Enabled: true,
		ConnectionName: "fixture-metrics", PluginID: "thanos", TemplateID: "promql_instant",
		Params: map[string]any{"expression": "up"}, ScopeKind: "integration", Timezone: "UTC",
		CheckDescription: tooLong,
	}, plan.RowVersion); err == nil {
		t.Fatal("over-length description must reject")
	}
	// schema CHECK 与应用计数同字面：直写 2501 个汉字（字符超限）必须被
	// SQLite length() CHECK 拒绝。
	if _, err := h.db.Exec(`UPDATE inspection_plans SET check_description=? WHERE plan_key='rune-plan'`, strings.Repeat("检", 2501)); err == nil {
		t.Fatal("schema CHECK must reject character counts over the limit")
	}
}

// TestPlanCommandDigestDistinguishesSemantics 钉住命令 digest：同一
// clientCommandID 携带不同分析语义字段必须按命令复用拒绝，绝不错误 replay
// 第一个载荷。
func TestPlanCommandDigestDistinguishesSemantics(t *testing.T) {
	h := newTestHarness(t)
	ctx := commandContext(t)
	first, err := h.service.CreatePlan(ctx, h.principal, "digest-cmd-0001", PlanInput{
		PlanKey: "digest-plan", DisplayName: "语义计划", Enabled: true,
		ConnectionName: "fixture-metrics", PluginID: "thanos", TemplateID: "promql_instant",
		Params: map[string]any{"expression": "up"}, ScopeKind: "integration", Timezone: "UTC",
		CheckDescription: "说明 A",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 同 ID、不同语义载荷：拒绝为命令复用，而不是 replay 返回第一个计划。
	if _, err := h.service.CreatePlan(ctx, h.principal, "digest-cmd-0001", PlanInput{
		PlanKey: "digest-plan", DisplayName: "语义计划", Enabled: true,
		ConnectionName: "fixture-metrics", PluginID: "thanos", TemplateID: "promql_instant",
		Params: map[string]any{"expression": "up"}, ScopeKind: "integration", Timezone: "UTC",
		CheckDescription: "说明 B",
	}); err == nil {
		t.Fatal("same command id with different semantics must reject")
	} else {
		var conflict *PlanConflictError
		if !errors.As(err, &conflict) || conflict.Code != "command_reused" {
			t.Fatalf("same command id with different semantics must be command_reused, got %v", err)
		}
	}
	// 同 ID 同载荷：幂等 replay 返回同一计划。
	replayed, err := h.service.CreatePlan(ctx, h.principal, "digest-cmd-0001", PlanInput{
		PlanKey: "digest-plan", DisplayName: "语义计划", Enabled: true,
		ConnectionName: "fixture-metrics", PluginID: "thanos", TemplateID: "promql_instant",
		Params: map[string]any{"expression": "up"}, ScopeKind: "integration", Timezone: "UTC",
		CheckDescription: "说明 A",
	})
	if err != nil || replayed.PlanKey != first.PlanKey {
		t.Fatalf("identical replay must return the committed plan: %v %+v", err, replayed)
	}
}

// TestReportProjectsPerVersionInstructions 钉住每个报告版本的实际生效要求：
// 首版来自 Run 冻结初始要求；仅本次覆盖版来自该 Attempt 的不可变
// requirements 行；显式清除版投影空串。
func TestReportProjectsPerVersionInstructions(t *testing.T) {
	h := newTestHarness(t)
	runID := completeChecklistRun(t, h)
	ctx := commandContext(t)
	commitReport := func(attemptID int64, content, promptSeed string) {
		t.Helper()
		if err := h.attempts.BindToSlot(ctx, attemptID, "plinth", "plinth-boot", 1, time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := h.attempts.Accept(ctx, attemptID, "plinth-boot", 1); err != nil {
			t.Fatal(err)
		}
		promptDigest := strings.Repeat(promptSeed, 64)
		callID := h.seedSucceededModelCall(t, attemptID, promptDigest)
		if err := h.service.CommitReportProposal(context.Background(), attemptID, "plinth-boot", 1, reportProposalBody(attemptID, runID, callID, content, evidenceIDsForRun(t, h, runID), artifactIDsForAttempt(t, h, attemptID), promptDigest)); err != nil {
			t.Fatal(err)
		}
	}
	commitReport(h.analysisAttemptID(t, runID), "首版", "4")
	first, err := h.service.GetReport(ctx, runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if first.ReportInstructions == nil || *first.ReportInstructions != "初始报告要求 v1" {
		t.Fatalf("v1 must inherit the frozen requirement, got %+v", first.ReportInstructions)
	}
	override := "仅本次：只看异常"
	next, err := h.service.ReanalyzeRun(ctx, h.principal, "per-version-override", runID, &override)
	if err != nil {
		t.Fatal(err)
	}
	commitReport(next.AttemptID, "覆盖版", "5")
	second, err := h.service.GetReport(ctx, runID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if second.ReportInstructions == nil || *second.ReportInstructions != override {
		t.Fatalf("v2 must project its own override, got %+v", second.ReportInstructions)
	}
	// v1 投影不因 v2 的覆盖而改变。
	first, err = h.service.GetReport(ctx, runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if *first.ReportInstructions != "初始报告要求 v1" {
		t.Fatalf("v1 projection drifted: %q", *first.ReportInstructions)
	}
	// 显式清除版投影空串。
	cleared, err := h.service.ReanalyzeRun(ctx, h.principal, "per-version-clear", runID, new(string))
	if err != nil {
		t.Fatal(err)
	}
	commitReport(cleared.AttemptID, "清除版", "6")
	third, err := h.service.GetReport(ctx, runID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if third.ReportInstructions == nil || *third.ReportInstructions != "" {
		t.Fatalf("v3 must project the explicit clear, got %+v", third.ReportInstructions)
	}
}

// TestChecklistRejectsMalformedFrozenJSONWithIdentity 钉住数据完整性：冻结
// JSON 形状损坏时清单推导带 run/check 身份失败，绝不静默吞掉。
func TestChecklistRejectsMalformedFrozenJSONWithIdentity(t *testing.T) {
	h := newTestHarness(t)
	// 用一个保持 Running 的新 Run：run_check 闭合触发器要求检查结果落在活动
	// Run 上，畸形 meta 行通过 runtime_unavailable 边界分支植入。
	if _, err := h.service.CreatePlan(commandContext(t), h.principal, "broken-create-0001", PlanInput{
		PlanKey: "broken-plan", DisplayName: "畸形计划", Enabled: true,
		ConnectionName: "fixture-metrics", PluginID: "thanos", TemplateID: "promql_instant",
		Params: map[string]any{"expression": "up"}, ScopeKind: "integration", Timezone: "UTC",
	}); err != nil {
		t.Fatal(err)
	}
	run, err := h.service.CreatePlanRun(commandContext(t), h.principal, "broken-run-0001", "broken-plan")
	if err != nil {
		t.Fatal(err)
	}
	runID := run.RunID
	const now = "2026-09-16T00:00:00Z"
	// scope 闭合触发器要求 run_check Attempt 的 check_key 已在活动 Run 的检查
	// 目录中，因此先插 run_checks 行再插 Attempt。
	if _, err := h.db.Exec(`INSERT INTO inspection_run_checks(run_id,check_key,display_name,plugin_id,template_id,template_version,params_json,created_at)
		VALUES(?, 'broken_check','畸形检查','thanos','promql_instant','1','{"expression":"up"}',?)`, runID, now); err != nil {
		t.Fatal(err)
	}
	probeAttempt, err := h.db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,check_key,state,quoin_release_version,created_at)
		VALUES('inspection_collection','run_check',?,'broken_check','Queued','test',?)`, runID, now)
	if err != nil {
		t.Fatal(err)
	}
	probeID, _ := probeAttempt.LastInsertId()
	if _, err := h.db.Exec(`INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(?,'inspection_plugin_execution_v1','v1',?,?)`,
		probeID, strings.Repeat("0", 64), now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE execution_attempts SET state='Failed',ended_at=?,row_version=row_version+1 WHERE id=? AND state='Queued'`, now, probeID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`INSERT INTO inspection_check_results(run_id,check_key,status,evidence_id,attempt_id,result_digest,gap_reason,meta_json,created_at)
		VALUES(?, 'broken_check','gap',NULL,?,NULL,'runtime_unavailable','{"observedAt":123}',?)`, runID, probeID, now); err != nil {
		t.Fatal(err)
	}
	// 在执行器事务内以生产同一查询面驱动推导（与 sProbe 相同的白盒模式）。
	probe, regErr := h.service.runner.Register(execution.Operation{
		Name: "inspection_checklist_probe_test", Class: execution.ClassWrite,
		ObjectType: ObjectInspectionRun, Authorize: authorizeInspectionAdmin,
	})
	if regErr != nil {
		t.Fatal(regErr)
	}
	if _, probeErr := execution.Execute(commandContext(t), h.service.runner, probe,
		func(tx *execution.Tx) (struct{}, error) {
			_, err := reportCheckItemsOn(context.Background(), tx, runID)
			return struct{}{}, err
		}, func(struct{}) int64 { return 0 }); probeErr == nil {
		t.Fatal("malformed frozen meta must fail the checklist derivation")
	} else if !strings.Contains(probeErr.Error(), "check=broken_check") || !strings.Contains(probeErr.Error(), fmt.Sprintf("run=%d", runID)) {
		t.Fatalf("malformed meta error must carry run/check identity: %v", probeErr)
	}
}

// TestAnalysisAttemptsCarryInspectionAgentVersion 钉住版本身份：巡检分析
// Attempt 的 agent_version 是独立的 inspection 生成，绝不再借用
// initial-analysis 的共享身份。
func TestAnalysisAttemptsCarryInspectionAgentVersion(t *testing.T) {
	h := newTestHarness(t)
	runID := completeChecklistRun(t, h)
	var agentVersion string
	if err := h.db.QueryRow(`SELECT agent_version FROM execution_attempts WHERE id=?`, h.analysisAttemptID(t, runID)).Scan(&agentVersion); err != nil {
		t.Fatal(err)
	}
	if agentVersion != attempt.InspectionAgentVersion {
		t.Fatalf("analysis attempt agent_version = %q, want %q", agentVersion, attempt.InspectionAgentVersion)
	}
}

// TestPlanDigestKeepsLegacyLayoutForEmptySemantics 钉住旧命令重放兼容：三个
// 语义字段全部缺省时 digest 与升级前的 map 布局逐字节相同；任一字段实际携带
// 语义才追加键（不同语义互不可混淆 replay）。
func TestPlanDigestKeepsLegacyLayoutForEmptySemantics(t *testing.T) {
	base := PlanInput{
		PlanKey: "digest-plan", DisplayName: "语义计划", Enabled: true,
		ConnectionName: "fixture-metrics", PluginID: "thanos", TemplateID: "promql_instant",
		Params: map[string]any{"expression": "up"}, ScopeKind: "integration", Timezone: "UTC",
	}
	legacyCreate := auth.DigestCommand(CommandCreatePlan, map[string]any{
		"planKey": base.PlanKey, "connectionName": base.ConnectionName,
		"displayName": base.DisplayName, "enabled": base.Enabled,
	})
	if planDigest(CommandCreatePlan, base, 0) != legacyCreate {
		t.Fatal("create digest with empty semantics must equal the pre-upgrade layout")
	}
	legacyUpdate := auth.DigestCommand(CommandUpdatePlan, map[string]any{
		"planKey": base.PlanKey, "expectedRowVersion": int64(3),
		"displayName": base.DisplayName, "enabled": base.Enabled,
	})
	if planDigest(CommandUpdatePlan, base, 3) != legacyUpdate {
		t.Fatal("update digest with empty semantics must equal the pre-upgrade layout")
	}
	// 实际携带语义：digest 必须变化且互异。
	withSemantics := base
	withSemantics.CheckDescription = "说明"
	if planDigest(CommandCreatePlan, withSemantics, 0) == legacyCreate {
		t.Fatal("carrying semantics must change the digest")
	}
	withUnit := base
	withUnit.MetricUnit = "比率"
	withInstructions := base
	withInstructions.ReportInstructions = "要求"
	if planDigest(CommandCreatePlan, withSemantics, 0) == planDigest(CommandCreatePlan, withUnit, 0) ||
		planDigest(CommandCreatePlan, withSemantics, 0) == planDigest(CommandCreatePlan, withInstructions, 0) ||
		planDigest(CommandCreatePlan, withUnit, 0) == planDigest(CommandCreatePlan, withInstructions, 0) {
		t.Fatal("different semantics must produce different digests")
	}
}

// TestReanalyzeDigestKeepsLegacyLayoutForInherit 钉住重分析命令 digest：nil
// （继承）与升级前布局逐字节相同；显式空串（清除）与非空覆盖各自加键，三态
// digest 互异。
func TestReanalyzeDigestKeepsLegacyLayoutForInherit(t *testing.T) {
	legacy := auth.DigestCommand(CommandReanalyzeRun, map[string]any{"runId": int64(7)})
	if reanalyzeDigest(7, nil) != legacy {
		t.Fatal("inherit digest must equal the pre-upgrade layout")
	}
	empty := ""
	text := "仅本次"
	if reanalyzeDigest(7, &empty) == legacy {
		t.Fatal("explicit clear must not collide with inherit")
	}
	if reanalyzeDigest(7, &text) == reanalyzeDigest(7, &empty) || reanalyzeDigest(7, &text) == legacy {
		t.Fatal("override, clear and inherit must have distinct digests")
	}
}

// TestLegacyQueuedAnalysisDispatchCarriesLegacyIdentity 钉住在途兼容端到端：
// 升级前创建的 inspection Attempt 携带旧共享身份，其派发输入把该身份传给
// worker（worker 据此选择上一代冻结 prompt 渲染，见 worker 包准入测试）。
func TestLegacyQueuedAnalysisDispatchCarriesLegacyIdentity(t *testing.T) {
	h := newTestHarness(t)
	h.seedModelProvider(t)
	store, err := artifact.NewStore(h.db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.service.SetArtifactWriter(store.MaterializeEvidenceTransaction)
	runID := completeChecklistRun(t, h)
	analysisID := h.analysisAttemptID(t, runID)
	ctx := commandContext(t)
	// 先把当前 Attempt 驱动到 Succeeded（活动 analysis 有唯一索引）。
	if err := h.attempts.BindToSlot(ctx, analysisID, "plinth", "plinth-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.Accept(ctx, analysisID, "plinth-boot", 1); err != nil {
		t.Fatal(err)
	}
	promptDigest := strings.Repeat("7", 64)
	callID := h.seedSucceededModelCall(t, analysisID, promptDigest)
	if err := h.service.CommitReportProposal(context.Background(), analysisID, "plinth-boot", 1, reportProposalBody(analysisID, runID, callID, "当前代次报告", evidenceIDsForRun(t, h, runID), artifactIDsForAttempt(t, h, analysisID), promptDigest)); err != nil {
		t.Fatal(err)
	}
	// 当前生成的 Attempt 是新身份。
	input, err := h.service.Attempts().DispatchInputFor(ctx, analysisID)
	if err != nil {
		t.Fatal(err)
	}
	if input.AgentVersion != attempt.InspectionAgentVersion {
		t.Fatalf("current dispatch identity = %q", input.AgentVersion)
	}
	// 模拟升级前在途 Attempt：旧共享身份 + 旧形状快照（无 requirements 行）。
	const now = "2026-09-16T00:00:00Z"
	legacyAttempt, err := h.db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
		VALUES('inspection_analysis','run',?,'Queued','test','initial-analysis-v1',?)`, runID, now)
	if err != nil {
		t.Fatal(err)
	}
	legacyID, _ := legacyAttempt.LastInsertId()
	legacyInput := oldReportInput{
		SchemaKind: reportInputKind, AttemptID: legacyID, InspectionRunID: runID,
		ReportVersion: 2, PlanKey: "checklist-plan",
		EvidenceIDs: evidenceIDsForRun(t, h, runID), ArtifactIDs: artifactIDsForAttempt(t, h, analysisID), KnowledgeVersionID: []int64{},
		ModelContract: struct {
			ModelID             string `json:"modelId"`
			ContextBudgetTokens int64  `json:"contextBudgetTokens"`
			MaxOutputTokens     int64  `json:"maxOutputTokens"`
		}{ModelID: "fixture-chat-1", ContextBudgetTokens: 4096, MaxOutputTokens: 1024},
		Plan:           oldPlanContext{Key: "checklist-plan", Params: map[string]any{}, Scope: map[string]any{}},
		ConnectionName: "fixture-metrics", TemplateID: "promql_instant", TemplateVersion: "1",
	}
	var paramsRaw, scopeRaw string
	if err := h.db.QueryRow(`SELECT frozen_params_json, frozen_scope_json FROM inspection_runs WHERE id=?`, runID).Scan(&paramsRaw, &scopeRaw); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(paramsRaw), &legacyInput.Plan.Params)
	_ = json.Unmarshal([]byte(scopeRaw), &legacyInput.Plan.Scope)
	canonical, err := json.Marshal(legacyInput)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	if _, err := h.db.Exec(`INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,inspection_report_version,created_at)
		VALUES(?, 'inspection_analysis_v1','v1',?,2,?)`, legacyID, hex.EncodeToString(digest[:]), now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at)
		SELECT ?,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,?
		FROM attempt_connection_grants WHERE attempt_id=? AND purpose='chat_model'`, legacyID, now, analysisID); err != nil {
		t.Fatal(err)
	}
	insertLegacyItem := func(seq int, role, source string, column string, refID int64) {
		t.Helper()
		itemDigest := sha256.Sum256([]byte(source))
		if column == "" {
			if _, err := h.db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest) VALUES(?,?,?,?)`,
				snapshotIDOf(t, h, legacyID), seq, role, hex.EncodeToString(itemDigest[:])); err != nil {
				t.Fatal(err)
			}
			return
		}
		if _, err := h.db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,`+column+`) VALUES(?,?,?, ?,?)`,
			snapshotIDOf(t, h, legacyID), seq, role, hex.EncodeToString(itemDigest[:]), refID); err != nil {
			t.Fatal(err)
		}
	}
	insertLegacyItem(1, "inspection_run", fmt.Sprintf("inspection-run:%d", runID), "inspection_run_id", runID)
	var resultID int64
	if err := h.db.QueryRow(`SELECT id FROM inspection_check_results WHERE run_id=?`, runID).Scan(&resultID); err != nil {
		t.Fatal(err)
	}
	insertLegacyItem(2, "inspection_check_result", fmt.Sprintf("inspection-check-result:%d", resultID), "inspection_check_result_id", resultID)
	for index, evidenceID := range evidenceIDsForRun(t, h, runID) {
		insertLegacyItem(3+index, "inspection_evidence", fmt.Sprintf("evidence:%d", evidenceID), "evidence_id", evidenceID)
	}
	for index, artifactID := range artifactIDsForAttempt(t, h, analysisID) {
		insertLegacyItem(3+len(evidenceIDsForRun(t, h, runID))+index, "inspection_artifact", fmt.Sprintf("artifact:%d", artifactID), "artifact_id", artifactID)
	}
	if _, err := h.db.Exec(`INSERT INTO attempt_artifact_grants(attempt_id,artifact_id,source_kind,source_id,granted_at)
		SELECT ?,artifact_id,'input_snapshot',(SELECT id FROM attempt_input_snapshots WHERE attempt_id=?),? FROM attempt_artifact_grants WHERE attempt_id=?`,
		legacyID, legacyID, now, analysisID); err != nil {
		t.Fatal(err)
	}
	legacyDispatch, err := h.service.Attempts().DispatchInputFor(ctx, legacyID)
	if err != nil {
		t.Fatalf("legacy in-flight attempt must stay dispatchable: %v", err)
	}
	if legacyDispatch.AgentVersion != "initial-analysis-v1" {
		t.Fatalf("legacy dispatch identity = %q", legacyDispatch.AgentVersion)
	}
	if string(legacyDispatch.CanonicalJSON) != string(canonical) {
		t.Fatal("legacy attempt rebuild must stay byte-stable")
	}
}

func snapshotIDOf(t *testing.T, h *testHarness, attemptID int64) int64 {
	t.Helper()
	var id int64
	if err := h.db.QueryRow(`SELECT id FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
