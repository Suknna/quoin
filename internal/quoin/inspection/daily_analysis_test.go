package inspection

// 日报 Agent 分析行为测试（ADR-0014）：以 SQLite 域服务与不可变事实为最高
// 行为接缝，验证
//   - 封存后排队分析、缺模型保留事实并可在模型恢复后重试；
//   - 冻结目录绝不携带实时平台工具（重分析绝不混入执行时刻的实时数据，
//     唯一事实路径是 Quoin 只读工具 daily_report_get 读封存行）；
//   - 提案重裁决落库为版本化分析：事实文档逐字节不变、重放幂等、篡改拒绝；
//   - 人工重跑生成新报告版本后，新分析版本追加且旧版本/旧分析原样可读。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// seedSealedDailyReport 把一份日报从触发走到封存（无任何检查事实 → 全部
// 显式缺口），返回报告 id。
func (h *testHarness) seedSealedDailyReport(t *testing.T, configKey, planKey string, boundary time.Time) int64 {
	t.Helper()
	h.seedPlan(t, planKey)
	h.seedDailyConfig(t, dailyTestConfigInput(configKey, planKey))
	h.pinDailyNow(t, boundary.Add(2*time.Hour))
	if err := h.service.CreateScheduledDailyReport(context.Background(), DailyReportConfig{ConfigKey: configKey, Timezone: "UTC"}, boundary); err != nil {
		t.Fatalf("trigger daily report %s: %v", configKey, err)
	}
	if err := h.service.SealDueDailyReports(context.Background(), boundary.Add(2*time.Hour)); err != nil {
		t.Fatalf("seal daily report %s: %v", configKey, err)
	}
	var reportID int64
	if err := h.db.QueryRow(`SELECT id FROM inspection_daily_reports WHERE config_key=? AND local_date=?`, configKey, boundary.In(time.UTC).AddDate(0, 0, -1).Format("2006-01-02")).Scan(&reportID); err != nil {
		t.Fatal(err)
	}
	return reportID
}

func (h *testHarness) dailyAnalysisAttemptID(t *testing.T, reportID int64) int64 {
	t.Helper()
	var id int64
	if err := h.db.QueryRow(`SELECT id FROM execution_attempts WHERE attempt_type='inspection_daily_analysis' AND scope_type='daily_report' AND scope_id=?`, reportID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (h *testHarness) bindAcceptDailyAnalysis(t *testing.T, attemptID int64) {
	t.Helper()
	ctx := commandContext(t)
	// 使用装配了 SnapshotRebuilder 的服务实例（与派发路径一致）。
	attempts := h.service.Attempts()
	if err := attempts.BindToSlot(ctx, attemptID, "plinth", "plinth-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := attempts.Accept(ctx, attemptID, "plinth-boot", 1); err != nil {
		t.Fatal(err)
	}
}

func dailyAnalysisProposalBody(attemptID, reportID int64, configKey, localDate string, reportVersion, callID int64, content, promptDigest string) []byte {
	canonical := fmt.Sprintf("inspection_daily_analysis_result_v1|%d|%d|%d|success|%s|%s", attemptID, reportID, callID, content, promptDigest)
	resultSum := sha256.Sum256([]byte(canonical))
	body, _ := json.Marshal(map[string]any{
		"schemaKind": "inspection_daily_analysis_result_v1", "attemptId": attemptID, "dailyReportId": reportID,
		"configKey": configKey, "localDate": localDate, "reportVersion": reportVersion,
		"modelCallId": callID, "outcome": "success", "content": content,
		"resultDigest": hex.EncodeToString(resultSum[:]), "promptDigest": promptDigest,
	})
	return body
}

func TestDailyAnalysisQueuesAfterSealAndRetriesWithoutModel(t *testing.T) {
	h := newTestHarness(t)
	boundary := time.Date(2026, time.September, 28, 6, 0, 0, 0, time.UTC)
	reportID := h.seedSealedDailyReport(t, "core-daily", "plan-a", boundary)
	var sealedContent string
	if err := h.db.QueryRow(`SELECT content FROM inspection_daily_report_versions WHERE report_id=? AND version=1`, reportID).Scan(&sealedContent); err != nil {
		t.Fatal(err)
	}

	// 缺模型：确定性事实保留，不创建 Attempt，不虚构任何分析。
	if err := h.service.EnsureDueDailyReportAnalyses(context.Background()); err != nil {
		t.Fatal(err)
	}
	var attempts int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM execution_attempts WHERE attempt_type='inspection_daily_analysis'`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Fatalf("missing model must not create an analysis attempt, got %d", attempts)
	}

	// 模型恢复后同一重试路径创建 Queued 分析，冻结输入/目录/授权齐全。
	h.seedModelProvider(t)
	if err := h.service.EnsureDueDailyReportAnalyses(context.Background()); err != nil {
		t.Fatal(err)
	}
	attemptID := h.dailyAnalysisAttemptID(t, reportID)
	var state, agentVersion string
	if err := h.db.QueryRow(`SELECT state,agent_version FROM execution_attempts WHERE id=?`, attemptID).Scan(&state, &agentVersion); err != nil {
		t.Fatal(err)
	}
	if state != "Queued" || agentVersion != "inspection-daily-analysis-v1" {
		t.Fatalf("analysis attempt = %s/%s", state, agentVersion)
	}
	var schemaKind string
	var reportVersion int64
	if err := h.db.QueryRow(`SELECT schema_kind,inspection_report_version FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&schemaKind, &reportVersion); err != nil {
		t.Fatal(err)
	}
	if schemaKind != "inspection_daily_analysis_v1" || reportVersion != 1 {
		t.Fatalf("analysis snapshot = %s/v%d", schemaKind, reportVersion)
	}
	var itemCount, wrongItems int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM attempt_input_items WHERE snapshot_id=(SELECT id FROM attempt_input_snapshots WHERE attempt_id=?)`, attemptID).Scan(&itemCount); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM attempt_input_items WHERE inspection_daily_report_id IS NOT NULL AND inspection_daily_report_id=?`, reportID).Scan(&wrongItems); err != nil {
		t.Fatal(err)
	}
	if itemCount != 1 || wrongItems != 1 {
		t.Fatalf("frozen input items = %d (daily locator rows %d), want exactly the sealed report locator", itemCount, wrongItems)
	}
	var grant int64
	if err := h.db.QueryRow(`SELECT id FROM attempt_connection_grants WHERE attempt_id=? AND purpose='chat_model'`, attemptID).Scan(&grant); err != nil {
		t.Fatalf("chat grant missing: %v", err)
	}
	// 重建逐字节一致（派发围栏的 digest 校验即此断言）。
	if _, err := h.service.Attempts().DispatchInputFor(commandContext(t), attemptID); err != nil {
		t.Fatalf("dispatch rebuild: %v", err)
	}
	// 已有分析版本的 Attempt 不重复创建。
	if err := h.service.EnsureDueDailyReportAnalyses(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM execution_attempts WHERE attempt_type='inspection_daily_analysis' AND scope_type='daily_report' AND scope_id=?`, reportID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("repeated ensure created %d attempts, want 1", attempts)
	}
	_ = sealedContent
}

// TestDailyAnalysisFrozenCatalogCarriesNoLivePlatformTool 是「重分析绝不做
// 实时平台查询」的目录级断言：日报总结目录只有平台工具与 Quoin 只读
// daily_report_get，任何插件贡献的实时指标工具（thanos_query）都不进入——
// 哪怕 metrics 插件在本进程装配并处于默认启用集。
// TestDailyExpectedOutputFreezesPerVersion 钉住「人类期望输出」的冻结链：
// 管理员撰写的期望随配置下发，触发时冻结进报告行并随封存进入 v1；人工重
// 分析采用届时配置值冻结进新版本（改配置不改写旧版本）；分析输入与重建
// 逐字节携带同一冻结文本。
func TestDailyExpectedOutputFreezesPerVersion(t *testing.T) {
	h := newTestHarness(t)
	h.seedPlan(t, "plan-a")
	ctx := commandContext(t)
	configInput := dailyTestConfigInput("core-daily", "plan-a")
	expectation := "逐来源给出结论，引用 runId 与 evidenceId。"
	configInput.ReportInstructions = &expectation
	h.seedDailyConfig(t, configInput)
	boundary := time.Date(2026, time.September, 28, 6, 0, 0, 0, time.UTC)
	h.pinDailyNow(t, boundary.Add(2*time.Hour))
	if err := h.service.CreateScheduledDailyReport(context.Background(), DailyReportConfig{ConfigKey: "core-daily", Timezone: "UTC"}, boundary); err != nil {
		t.Fatal(err)
	}
	if err := h.service.SealDueDailyReports(context.Background(), boundary.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var reportID int64
	if err := h.db.QueryRow(`SELECT id FROM inspection_daily_reports WHERE config_key='core-daily'`).Scan(&reportID); err != nil {
		t.Fatal(err)
	}
	var reportExpectation *string
	if err := h.db.QueryRow(`SELECT expected_output FROM inspection_daily_reports WHERE id=?`, reportID).Scan(&reportExpectation); err != nil {
		t.Fatal(err)
	}
	if reportExpectation == nil || *reportExpectation != expectation {
		t.Fatalf("trigger-time expectation = %+v", reportExpectation)
	}
	versions, err := h.service.ListDailyReportVersions(context.Background(), "core-daily", "2026-09-27")
	if err != nil || len(versions) != 1 || versions[0].ExpectedOutput == nil || *versions[0].ExpectedOutput != expectation {
		t.Fatalf("v1 expectation = %+v err=%v", versions, err)
	}
	h.seedModelProvider(t)
	if err := h.service.EnsureDueDailyReportAnalyses(context.Background()); err != nil {
		t.Fatal(err)
	}
	firstAttempt := h.dailyAnalysisAttemptID(t, reportID)
	input, err := h.service.Attempts().DispatchInputFor(ctx, firstAttempt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(input.CanonicalJSON), "evidenceId。") {
		t.Fatal("frozen analysis input must carry the admin expectation verbatim")
	}
	// 配置改写只影响之后的版本；v1 冻结文本不变。
	updated := dailyTestConfigInput("core-daily", "plan-a")
	changed := "新版期望：按台帐核对。"
	updated.ReportInstructions = &changed
	if _, err := h.service.UpdateDailyReportConfig(ctx, h.principal, "expected-update-1", updated, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RerunDailyReport(ctx, h.principal, "expected-rerun-1", "core-daily", "2026-09-27"); err != nil {
		t.Fatal(err)
	}
	versions, err = h.service.ListDailyReportVersions(context.Background(), "core-daily", "2026-09-27")
	if err != nil || len(versions) != 2 {
		t.Fatalf("versions = %+v err=%v", versions, err)
	}
	if versions[0].ExpectedOutput == nil || *versions[0].ExpectedOutput != changed {
		t.Fatalf("v2 expectation = %+v, want the changed text", versions[0].ExpectedOutput)
	}
	if *versions[1].ExpectedOutput != expectation {
		t.Fatalf("v1 expectation changed by rerun: %+v", versions[1].ExpectedOutput)
	}
}

func TestDailyAnalysisFrozenCatalogCarriesNoLivePlatformTool(t *testing.T) {
	h := newTestHarness(t)
	boundary := time.Date(2026, time.September, 28, 6, 0, 0, 0, time.UTC)
	reportID := h.seedSealedDailyReport(t, "core-daily", "plan-a", boundary)
	h.seedModelProvider(t)
	if err := h.service.EnsureDueDailyReportAnalyses(context.Background()); err != nil {
		t.Fatal(err)
	}
	attemptID := h.dailyAnalysisAttemptID(t, reportID)
	var catalog string
	if err := h.db.QueryRow(`SELECT tool_catalog_json FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&catalog); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(catalog, "thanos_query") || strings.Contains(catalog, "metrics_collect") {
		t.Fatal("daily analysis frozen catalog exposes a live platform/metric tool")
	}
	if !strings.Contains(catalog, "daily_report_get") {
		t.Fatal("daily analysis frozen catalog must carry the frozen-facts retrieval tool")
	}
}

func TestDailyAnalysisCommitAppendsImmutableAnalysisWithoutTouchingFacts(t *testing.T) {
	h := newTestHarness(t)
	boundary := time.Date(2026, time.September, 28, 6, 0, 0, 0, time.UTC)
	reportID := h.seedSealedDailyReport(t, "core-daily", "plan-a", boundary)
	h.seedModelProvider(t)
	if err := h.service.EnsureDueDailyReportAnalyses(context.Background()); err != nil {
		t.Fatal(err)
	}
	attemptID := h.dailyAnalysisAttemptID(t, reportID)
	var contentBefore string
	if err := h.db.QueryRow(`SELECT content FROM inspection_daily_report_versions WHERE report_id=? AND version=1`, reportID).Scan(&contentBefore); err != nil {
		t.Fatal(err)
	}
	h.bindAcceptDailyAnalysis(t, attemptID)
	promptDigest := strings.Repeat("d", 64)
	callID := h.seedSucceededModelCall(t, attemptID, promptDigest)
	body := dailyAnalysisProposalBody(attemptID, reportID, "core-daily", "2026-09-27", 1, callID, "当日全源存在显式缺口：采证截止前未收敛。", promptDigest)
	if err := h.service.CommitDailyAnalysisProposal(context.Background(), attemptID, "plinth-boot", 1, body); err != nil {
		t.Fatalf("commit daily analysis: %v", err)
	}
	var attemptState string
	if err := h.db.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&attemptState); err != nil {
		t.Fatal(err)
	}
	if attemptState != "Succeeded" {
		t.Fatalf("analysis attempt state = %s, want Succeeded", attemptState)
	}
	var analysisVersion, analysisReportVersion int64
	var analysisModel, analysisContent string
	if err := h.db.QueryRow(`SELECT analysis_version,report_version,model_id,content FROM inspection_daily_report_analyses WHERE report_id=?`, reportID).
		Scan(&analysisVersion, &analysisReportVersion, &analysisModel, &analysisContent); err != nil {
		t.Fatal(err)
	}
	if analysisVersion != 1 || analysisReportVersion != 1 || analysisModel != "fixture-chat-1" || !strings.Contains(analysisContent, "显式缺口") {
		t.Fatalf("analysis row = v%d/report v%d/%s/%q", analysisVersion, analysisReportVersion, analysisModel, analysisContent)
	}
	// 事实文档逐字节不变：分析绝不改写封存事实。
	var contentAfter string
	if err := h.db.QueryRow(`SELECT content FROM inspection_daily_report_versions WHERE report_id=? AND version=1`, reportID).Scan(&contentAfter); err != nil {
		t.Fatal(err)
	}
	if contentAfter != contentBefore {
		t.Fatal("sealed daily report content changed by the analysis commit")
	}
	// 同字节重放幂等；篡改内容与越权定位符确定性拒绝且不落行。
	if err := h.service.CommitDailyAnalysisProposal(context.Background(), attemptID, "plinth-boot", 1, body); err != nil {
		t.Fatalf("identical replay must be idempotent: %v", err)
	}
	tampered := dailyAnalysisProposalBody(attemptID, reportID, "core-daily", "2026-09-27", 1, callID, "被改写的结论", promptDigest)
	if err := h.service.CommitDailyAnalysisProposal(context.Background(), attemptID, "plinth-boot", 1, tampered); err == nil {
		t.Fatal("tampered replay must be rejected")
	}
	foreign := dailyAnalysisProposalBody(attemptID, reportID, "other-config", "2026-09-27", 1, callID, "当日全源存在显式缺口：采证截止前未收敛。", promptDigest)
	if err := h.service.CommitDailyAnalysisProposal(context.Background(), attemptID, "plinth-boot", 1, foreign); err == nil {
		t.Fatal("mismatched report locator must be rejected")
	}
	var analyses int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_daily_report_analyses WHERE report_id=?`, reportID).Scan(&analyses); err != nil {
		t.Fatal(err)
	}
	if analyses != 1 {
		t.Fatalf("analysis rows = %d, want exactly 1", analyses)
	}
	// 读模型：列表与新旧版本读取。
	items, err := h.service.ListDailyReportAnalyses(context.Background(), "core-daily", "2026-09-27")
	if err != nil || len(items) != 1 || items[0].AnalysisVersion != 1 {
		t.Fatalf("analysis list = %+v err=%v", items, err)
	}
	detail, err := h.service.GetDailyReportAnalysis(context.Background(), "core-daily", "2026-09-27", 1)
	if err != nil || detail.Content != analysisContent || detail.ModelID != "fixture-chat-1" {
		t.Fatalf("analysis detail = %+v err=%v", detail, err)
	}
}

func TestDailyRerunCreatesSecondAnalysisVersionOnFrozenV2(t *testing.T) {
	h := newTestHarness(t)
	day := time.Date(2026, time.September, 27, 10, 0, 0, 0, time.UTC)
	h.seedPlan(t, "plan-late")
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
	var reportID int64
	if err := h.db.QueryRow(`SELECT id FROM inspection_daily_reports WHERE config_key='core-daily' AND local_date='2026-09-27'`).Scan(&reportID); err != nil {
		t.Fatal(err)
	}
	h.seedModelProvider(t)
	if err := h.service.EnsureDueDailyReportAnalyses(context.Background()); err != nil {
		t.Fatal(err)
	}
	firstAttempt := h.dailyAnalysisAttemptID(t, reportID)
	h.bindAcceptDailyAnalysis(t, firstAttempt)
	promptDigest := strings.Repeat("e", 64)
	firstCall := h.seedSucceededModelCall(t, firstAttempt, promptDigest)
	if err := h.service.CommitDailyAnalysisProposal(context.Background(), firstAttempt, "plinth-boot", 1,
		dailyAnalysisProposalBody(firstAttempt, reportID, "core-daily", "2026-09-27", 1, firstCall, "v1 总结：来源缺口。", promptDigest)); err != nil {
		t.Fatal(err)
	}

	// 迟到采集收敛后人工重跑：日报 v2 追加（v1 原样可读），新分析冻结在 v2。
	lateAttempt := h.promqlAttemptID(t, runs["plan-late"])
	h.dispatchPromQL(t, lateAttempt)
	if err := h.service.CommitPluginProposal(context.Background(), lateAttempt, "plinth-boot", 1, pluginSuccessProposal(t, h, lateAttempt, runs["plan-late"], "success")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.RerunDailyReport(commandContext(t), h.principal, "daily-rerun-1", "core-daily", "2026-09-27"); err != nil {
		t.Fatal(err)
	}
	if err := h.service.EnsureDueDailyReportAnalyses(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := h.db.Query(`SELECT id FROM execution_attempts WHERE attempt_type='inspection_daily_analysis' AND scope_type='daily_report' AND scope_id=? ORDER BY id`, reportID)
	if err != nil {
		t.Fatal(err)
	}
	var attemptIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		attemptIDs = append(attemptIDs, id)
	}
	rows.Close()
	if len(attemptIDs) != 2 {
		t.Fatalf("analysis attempts = %v, want one per sealed report version", attemptIDs)
	}
	secondAttempt := attemptIDs[1]
	var frozenVersion int64
	if err := h.db.QueryRow(`SELECT inspection_report_version FROM attempt_input_snapshots WHERE attempt_id=?`, secondAttempt).Scan(&frozenVersion); err != nil {
		t.Fatal(err)
	}
	if frozenVersion != 2 {
		t.Fatalf("second analysis frozen to report v%d, want v2", frozenVersion)
	}
	h.bindAcceptDailyAnalysis(t, secondAttempt)
	secondCall := h.seedSucceededModelCall(t, secondAttempt, promptDigest)
	if err := h.service.CommitDailyAnalysisProposal(context.Background(), secondAttempt, "plinth-boot", 1,
		dailyAnalysisProposalBody(secondAttempt, reportID, "core-daily", "2026-09-27", 2, secondCall, "v2 总结：迟到事实已进入新版日报。", promptDigest)); err != nil {
		t.Fatal(err)
	}
	items, err := h.service.ListDailyReportAnalyses(context.Background(), "core-daily", "2026-09-27")
	if err != nil || len(items) != 2 || items[0].AnalysisVersion != 2 || items[1].AnalysisVersion != 1 {
		t.Fatalf("analysis versions = %+v err=%v, want [2 1]", items, err)
	}
	first, err := h.service.GetDailyReportAnalysis(context.Background(), "core-daily", "2026-09-27", 1)
	if err != nil || first.Content != "v1 总结：来源缺口。" || first.ReportVersion != 1 {
		t.Fatalf("analysis v1 must stay readable: %+v err=%v", first, err)
	}
	// 报告事实两版原样：v1 内容在分析提交后不变。
	var v1Content, v2Content string
	if err := h.db.QueryRow(`SELECT content FROM inspection_daily_report_versions WHERE report_id=? AND version=1`, reportID).Scan(&v1Content); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT content FROM inspection_daily_report_versions WHERE report_id=? AND version=2`, reportID).Scan(&v2Content); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v1Content, `"status":"gap"`) || !strings.Contains(v2Content, `"status":"ok"`) {
		t.Fatalf("report fact versions diverged unexpectedly: v1 gap=%v v2 ok=%v", strings.Contains(v1Content, `"status":"gap"`), strings.Contains(v2Content, `"status":"ok"`))
	}
}
