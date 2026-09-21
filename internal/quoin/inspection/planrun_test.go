package inspection

// ADR-0004 纵向切片测试：不创建任何业务系统，接入启用即用（默认基础计划），
// 人工 Run → 插件采证 → 检查结果/Evidence → 模型分析 → 不可变报告 全链路。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// TestNoBusinessRunReport walks the full plan-run slice without any business
// system: default plan on enablement, manual run, plugin collection closure,
// and the immutable report produced from reused Attempt/grants/Evidence.
func TestNoBusinessRunReport(t *testing.T) {
	h := newTestHarness(t)
	h.seedModelProvider(t)
	store, err := artifact.NewStore(h.db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.service.SetArtifactWriter(store.MaterializeEvidenceTransaction)
	ctx := commandContext(t)

	// 接入启用事务内幂等创建默认基础计划；重复启用返回同一行。
	// The default-plan creation now runs through the shared execution
	// runner: the guarded transaction replaces the fixture's manual
	// BEGIN/COMMIT and each call records its automatic audit row (the
	// idempotent second call records nothing new).
	if err = h.service.EnsureDefaultPlan(ctx, 1, "fixture-metrics"); err != nil {
		t.Fatal(err)
	}
	if err = h.service.EnsureDefaultPlan(ctx, 1, "fixture-metrics"); err != nil {
		t.Fatal(err)
	}
	var defaultCount int
	if err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inspection_plans WHERE plan_key=?`, "basic-fixture-metrics").Scan(&defaultCount); err != nil {
		t.Fatal(err)
	}
	if defaultCount != 1 {
		t.Fatalf("default plan rows = %d, want exactly 1", defaultCount)
	}

	// 人工 Run：默认计划即点即用，无需任何业务声明。
	detail, err := h.service.CreatePlanRun(ctx, h.principal, "slice-run-0001", "basic-fixture-metrics")
	if err != nil {
		t.Fatal(err)
	}
	if detail.ConnectionName == nil || *detail.ConnectionName != "fixture-metrics" {
		t.Fatalf("plan run connection = %v, want fixture-metrics", detail.ConnectionName)
	}
	attemptID := h.promqlAttemptID(t, detail.RunID)
	var grantPurpose string
	var revisionID int64
	if err = h.db.QueryRow(`SELECT purpose, connection_revision_id FROM attempt_connection_grants WHERE attempt_id=?`, attemptID).Scan(&grantPurpose, &revisionID); err != nil {
		t.Fatal(err)
	}
	if grantPurpose != "config_thanos_query" {
		t.Fatalf("plugin child grant = %s", grantPurpose)
	}
	var revisionCount int
	if err = h.db.QueryRow(`SELECT COUNT(*) FROM attempt_input_items i JOIN attempt_input_snapshots s ON s.id=i.snapshot_id WHERE s.attempt_id=? AND i.connection_revision_id=?`, attemptID, revisionID).Scan(&revisionCount); err != nil {
		t.Fatal(err)
	}
	if revisionCount != 1 {
		t.Fatalf("frozen source provenance items = %d, want 1", revisionCount)
	}
	h.dispatchPromQL(t, attemptID)
	if err = h.service.CommitPluginProposal(context.Background(), attemptID, "plinth-boot", 1, pluginSuccessProposal(t, h, attemptID, detail.RunID, "success")); err != nil {
		t.Fatal(err)
	}
	final, err := h.service.GetRun(ctx, detail.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != "Completed" || final.ReportCount != 0 {
		t.Fatalf("collected run = %s reports=%d", final.State, final.ReportCount)
	}
	// 采证收敛即创建分析 Attempt；提交模型 proposal 后生成不可变报告。
	analysisID := h.analysisAttemptID(t, detail.RunID)
	if err = h.attempts.BindToSlot(ctx, analysisID, "plinth", "plinth-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err = h.attempts.Accept(ctx, analysisID, "plinth-boot", 1); err != nil {
		t.Fatal(err)
	}
	promptDigest := strings.Repeat("d", 64)
	callID := h.seedSucceededModelCall(t, analysisID, promptDigest)
	evidenceIDs := evidenceIDsForRun(t, h, detail.RunID)
	artifactIDs := artifactIDsForAttempt(t, h, analysisID)
	if len(evidenceIDs) != 1 || len(artifactIDs) != 1 {
		t.Fatalf("slice evidence=%v artifacts=%v", evidenceIDs, artifactIDs)
	}
	if err = h.service.CommitReportProposal(context.Background(), analysisID, "plinth-boot", 1, reportProposalBody(analysisID, detail.RunID, callID, "无业务报告", evidenceIDs, artifactIDs, promptDigest)); err != nil {
		t.Fatal(err)
	}
	report, err := h.service.GetReport(ctx, detail.RunID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if report.Content != "无业务报告" || len(report.EvidenceIDs) != 1 {
		t.Fatalf("slice report = %+v", report)
	}
}

// TestRangePlanRunSettlesOnFailureAndPartial proves range-template runs can
// never strand Running: a failed range collection commits its gap (the frozen
// window metadata may travel without an executed window), and a partial pass
// commits as a typed gap without fabricating evidence.
func TestRangePlanRunSettlesOnFailureAndPartial(t *testing.T) {
	h := newTestHarness(t)
	ctx := commandContext(t)
	now := "2026-09-13T00:00:00Z"
	params, _ := json.Marshal(map[string]any{"expression": "up", "rangeSeconds": 300, "stepSeconds": 60})
	scope, _ := json.Marshal(map[string]any{"kind": "integration"})
	if _, err := h.db.Exec(`INSERT INTO inspection_plans(plan_key,display_name,enabled,connection_id,plugin_id,template_id,template_version,params_json,scope_json,scope_kind,cron,timezone,row_version,created_by,created_at,updated_at)
		VALUES('range-plan','范围计划',1,1,'thanos','promql_range','1',?,?,'integration',NULL,'UTC',1,1,?,?)`,
		string(params), string(scope), now, now); err != nil {
		t.Fatal(err)
	}
	detail, err := h.service.CreatePlanRun(ctx, h.principal, "range-run-0001", "range-plan")
	if err != nil {
		t.Fatal(err)
	}
	attemptID := h.promqlAttemptID(t, detail.RunID)
	h.dispatchPromQL(t, attemptID)

	// 失败的范围采集：错误正文 + 冻结窗口元数据，无证据，Run 以 gap 收敛。
	failed := map[string]any{
		"schemaKind": "inspection_plugin_result_v1", "attemptId": attemptID, "inspectionRunId": detail.RunID,
		"checkKey": "promql_range", "outcome": "error", "observedAt": "2026-09-13T00:00:01Z",
		"executionWindow": map[string]any{"startAt": "2026-09-12T23:55:00Z", "endAt": "2026-09-13T00:00:00Z", "stepSeconds": 60},
		"result":          nil, "warnings": []string{}, "errors": []string{"upstream timeout"}, "gapReason": "query_failed",
	}
	body, _ := json.Marshal(failed)
	if err := h.service.CommitPluginProposal(context.Background(), attemptID, "plinth-boot", 1, body); err != nil {
		t.Fatalf("failed range proposal must settle: %v", err)
	}
	var status, gap string
	if err := h.db.QueryRow(`SELECT status, gap_reason FROM inspection_check_results WHERE run_id=? AND attempt_id=?`, detail.RunID, attemptID).Scan(&status, &gap); err != nil {
		t.Fatal(err)
	}
	if status != "gap" || gap != "query_failed" {
		t.Fatalf("failed range check = %s/%s", status, gap)
	}
	var evidenceCount int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM evidence WHERE attempt_id=?`, attemptID).Scan(&evidenceCount); err != nil {
		t.Fatal(err)
	}
	if evidenceCount != 0 {
		t.Fatalf("failed range collection must not fabricate evidence: %d", evidenceCount)
	}
	final, err := h.service.GetRun(ctx, detail.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != "CompletedWithGaps" {
		t.Fatalf("run must settle CompletedWithGaps, got %s", final.State)
	}

	// partial_response（截断）同样收口且不制造证据。
	detail2, err := h.service.CreatePlanRun(ctx, h.principal, "range-run-0002", "range-plan")
	if err != nil {
		t.Fatal(err)
	}
	attemptID2 := h.promqlAttemptID(t, detail2.RunID)
	h.dispatchPromQL(t, attemptID2)
	partial := map[string]any{
		"schemaKind": "inspection_plugin_result_v1", "attemptId": attemptID2, "inspectionRunId": detail2.RunID,
		"checkKey": "promql_range", "outcome": "gap", "observedAt": "2026-09-13T00:00:01Z",
		"executionWindow": nil,
		"result":          nil, "warnings": []string{"collection truncated"}, "errors": []string{}, "gapReason": "partial_response",
	}
	body2, _ := json.Marshal(partial)
	if err := h.service.CommitPluginProposal(context.Background(), attemptID2, "plinth-boot", 1, body2); err != nil {
		t.Fatalf("partial range proposal must settle: %v", err)
	}
	final2, err := h.service.GetRun(ctx, detail2.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if final2.State != "CompletedWithGaps" {
		t.Fatalf("partial run must settle CompletedWithGaps, got %s", final2.State)
	}
}

// TestPlanRunAnalysisDispatchRealChain walks the REAL domain chain the
// runtime uses: collection closes → Quoin creates the analysis attempt →
// the dispatch path rebuilds the frozen input via the snapshot rebuilder
// (this is where a NULL declaration lineage previously broke real plan
// runs) → the model proposal commits the immutable report.
func TestPlanRunAnalysisDispatchRealChain(t *testing.T) {
	h := newTestHarness(t)
	h.seedModelProvider(t)
	store, err := artifact.NewStore(h.db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.service.SetArtifactWriter(store.MaterializeEvidenceTransaction)
	ctx := commandContext(t)

	// 真实链路从接入启用事务开始：默认基础计划由此产生。独立直连入口
	// EnsureDefaultPlan 走同一执行器，自动审计随命令原子落账。
	if err := h.service.EnsureDefaultPlan(ctx, 1, "fixture-metrics"); err != nil {
		t.Fatal(err)
	}

	detail, err := h.service.CreatePlanRun(ctx, h.principal, "chain-run-0001", "basic-fixture-metrics")
	if err != nil {
		t.Fatal(err)
	}
	// 采证子 Attempt 经真实派发输入重建（digest 复核路径）。
	attempts := h.service.Attempts()
	collectAttemptID := h.promqlAttemptID(t, detail.RunID)
	if _, err = attempts.DispatchInputFor(ctx, collectAttemptID); err != nil {
		t.Fatalf("collection dispatch rebuild: %v", err)
	}
	h.dispatchPromQL(t, collectAttemptID)
	if err = h.service.CommitPluginProposal(context.Background(), collectAttemptID, "plinth-boot", 1, pluginSuccessProposal(t, h, collectAttemptID, detail.RunID, "success")); err != nil {
		t.Fatal(err)
	}
	analysisID := h.analysisAttemptID(t, detail.RunID)
	// 分析 Attempt 的派发输入必须能经快照重建器重建——生产
	// analysis_queue_dispatch 就走这条路。
	input, err := attempts.DispatchInputFor(ctx, analysisID)
	if err != nil {
		t.Fatalf("analysis dispatch rebuild: %v", err)
	}
	if input.SchemaKind != "inspection_analysis_v1" {
		t.Fatalf("analysis schema kind = %s", input.SchemaKind)
	}
	var rebuilt map[string]any
	if err := json.Unmarshal(input.CanonicalJSON, &rebuilt); err != nil {
		t.Fatal(err)
	}
	if rebuilt["planKey"] != "basic-fixture-metrics" || rebuilt["connectionName"] != "fixture-metrics" {
		t.Fatalf("rebuilt analysis lineage = %v", rebuilt)
	}
	plan, ok := rebuilt["plan"].(map[string]any)
	if !ok || plan["key"] != "basic-fixture-metrics" {
		t.Fatalf("rebuilt plan context missing: %v", rebuilt["plan"])
	}
	if _, hasConfig := rebuilt["configVersionId"]; hasConfig && rebuilt["configVersionId"] != float64(0) {
		t.Fatalf("plan run must not carry declaration lineage: %v", rebuilt["configVersionId"])
	}
	// 未过滤的 Run 列表必须包含独立计划 Run（无业务系统）。
	summaries, _, err := h.service.ListRuns(ctx, "", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, summary := range summaries {
		if summary.ID == detail.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("unfiltered run list must include the plan run")
	}
	// 绑定并提交模型 proposal，报告落库。
	if err = h.attempts.BindToSlot(ctx, analysisID, "plinth", "plinth-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err = h.attempts.Accept(ctx, analysisID, "plinth-boot", 1); err != nil {
		t.Fatal(err)
	}
	promptDigest := strings.Repeat("e", 64)
	callID := h.seedSucceededModelCall(t, analysisID, promptDigest)
	evidenceIDs := evidenceIDsForRun(t, h, detail.RunID)
	artifactIDs := artifactIDsForAttempt(t, h, analysisID)
	if err = h.service.CommitReportProposal(context.Background(), analysisID, "plinth-boot", 1, reportProposalBody(analysisID, detail.RunID, callID, "链路报告", evidenceIDs, artifactIDs, promptDigest)); err != nil {
		t.Fatal(err)
	}
	if _, err = h.service.GetReport(ctx, detail.RunID, 1); err != nil {
		t.Fatalf("immutable report missing: %v", err)
	}
}

// TestPlanRunScopeFreezeRoundtrip proves the control plane freezes the exact
// scope constraints into the dispatch input (the only thing a collector may
// consume): business_view label conditions, objects observed identity labels,
// and the wire scope kind. No collector result is simulated here — these
// assertions are on the frozen inputs alone.
func TestPlanRunScopeFreezeRoundtrip(t *testing.T) {
	h := newTestHarness(t)
	ctx := commandContext(t)
	now := "2026-09-13T00:00:00Z"
	for _, statement := range []string{
		"INSERT INTO business_views(view_key,display_name,description,connection_id,label_conditions_json,row_version,created_by,created_at,updated_at) VALUES('mall-mysql-view','商城 MySQL','desc',1,'{\"env\":\"prod\",\"service\":\"mysql\"}',1,1,'" + now + "','" + now + "')",
		"INSERT INTO observed_source_objects(connection_id,object_type,identity_key,labels_json,current,created_at) VALUES(1,'target','job=mysqld,instance=:9104','{\"job\":\"mysqld\",\"instance\":\":9104\"}',1,'" + now + "')",
	} {
		if _, err := h.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	seedPlan := func(planKey, scopeKind, scopeJSON string) {
		t.Helper()
		if _, err := h.db.Exec(`INSERT INTO inspection_plans(plan_key,display_name,enabled,connection_id,plugin_id,template_id,template_version,params_json,scope_json,scope_kind,cron,timezone,row_version,created_by,created_at,updated_at)
			VALUES(?, '范围计划', 1, 1, 'thanos', 'promql_instant', '1', '{"expression":"up"}', ?, ?, NULL, 'UTC', 1, 1, ?, ?)`,
			planKey, scopeJSON, scopeKind, now, now); err != nil {
			t.Fatal(err)
		}
	}
	rebuildInput := func(t *testing.T, planKey string) (map[string]any, map[string]any) {
		t.Helper()
		detail, err := h.service.CreatePlanRun(ctx, h.principal, "cmd-freeze-"+planKey, planKey)
		if err != nil {
			t.Fatal(err)
		}
		attempts := h.service.Attempts()
		input, err := attempts.DispatchInputFor(ctx, h.promqlAttemptID(t, detail.RunID))
		if err != nil {
			t.Fatalf("dispatch rebuild: %v", err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(input.CanonicalJSON, &parsed); err != nil {
			t.Fatal(err)
		}
		return parsed, map[string]any{"id": detail.ID}
	}

	// business_view：视图条件冻结进派发输入，wire kind=businessView。
	seedPlan("view-plan", "business_view", `{"kind":"business_view","businessViewKey":"mall-mysql-view"}`)
	businessInput, _ := rebuildInput(t, "view-plan")
	if businessInput["scopeKind"] != "businessView" {
		t.Fatalf("businessView wire kind = %v", businessInput["scopeKind"])
	}
	target, _ := businessInput["target"].(map[string]any)
	if target == nil {
		t.Fatal("businessView input must carry a target")
	}
	conditions, _ := target["labelConditions"].(map[string]any)
	if conditions["env"] != "prod" || conditions["service"] != "mysql" {
		t.Fatalf("frozen view conditions = %v", target["labelConditions"])
	}
	if _, hasIdentity := target["identityLabels"]; hasIdentity {
		t.Fatalf("businessView target must not carry identityLabels: %v", target)
	}

	// objects：冻结已观测身份标签，wire kind=objects。
	seedPlan("objects-plan", "objects", `{"kind":"objects","objects":[{"objectType":"target","identityKey":"job=mysqld,instance=:9104"}]}`)
	objectsInput, _ := rebuildInput(t, "objects-plan")
	if objectsInput["scopeKind"] != "objects" {
		t.Fatalf("objects wire kind = %v", objectsInput["scopeKind"])
	}
	objectTarget, _ := objectsInput["target"].(map[string]any)
	identityLabels, _ := objectTarget["labelConditions"].(map[string]any)
	if identityLabels["job"] != "mysqld" || identityLabels["instance"] != ":9104" {
		t.Fatalf("frozen identity labels = %v", objectTarget["labelConditions"])
	}

	// integration：显式全接入，无 target、无约束。
	seedPlan("integration-plan", "integration", `{"kind":"integration"}`)
	integrationInput, _ := rebuildInput(t, "integration-plan")
	if integrationInput["scopeKind"] != "integration" {
		t.Fatalf("integration wire kind = %v", integrationInput["scopeKind"])
	}
	if _, hasTarget := integrationInput["target"]; hasTarget {
		t.Fatalf("integration input must not carry a target: %v", integrationInput["target"])
	}
}

// TestPlanRunScopeFailsClosed proves amplification is rejected, never silent:
// a condition-free business view and an unknown scope kind both produce typed
// rejections with no Run created.
func TestPlanRunScopeFailsClosed(t *testing.T) {
	h := newTestHarness(t)
	ctx := commandContext(t)
	now := "2026-09-13T00:00:00Z"
	for _, statement := range []string{
		"INSERT INTO business_views(view_key,display_name,description,connection_id,label_conditions_json,row_version,created_by,created_at,updated_at) VALUES('empty-view','空条件','desc',1,'{}',1,1,'" + now + "','" + now + "')",
	} {
		if _, err := h.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	seed := func(planKey, scopeKind, scopeJSON string) {
		t.Helper()
		if _, err := h.db.Exec(`INSERT INTO inspection_plans(plan_key,display_name,enabled,connection_id,plugin_id,template_id,template_version,params_json,scope_json,scope_kind,cron,timezone,row_version,created_by,created_at,updated_at)
			VALUES(?, '计划', 1, 1, 'thanos', 'promql_instant', '1', '{"expression":"up"}', ?, ?, NULL, 'UTC', 1, 1, ?, ?)`,
			planKey, scopeJSON, scopeKind, now, now); err != nil {
			t.Fatal(err)
		}
	}
	seed("empty-view-plan", "business_view", `{"kind":"business_view","businessViewKey":"empty-view"}`)
	if _, err := h.service.CreatePlanRun(ctx, h.principal, "cmd-empty-view", "empty-view-plan"); err == nil {
		t.Fatal("condition-free business view must fail closed")
	} else {
		var rejection *RejectionError
		if !errors.As(err, &rejection) || rejection.Code != "scope_empty" {
			t.Fatalf("empty view error = %v, want scope_empty", err)
		}
	}
	// 未知范围类型是数据库 CHECK 之下的纵深防御：直接驱动 expander 验证
	// fail closed，绝不当 integration 处理。探针在执行器事务内运行——与
	// 生产调用方完全同一受守卫通道，不使用裸连接。
	probe, regErr := h.service.runner.Register(execution.Operation{
		Name: "inspection_plan.scope_probe_test", Class: execution.ClassWrite,
		ObjectType: ObjectInspectionRun,
		Authorize:  authorizeInspectionAdmin,
	})
	if regErr != nil {
		t.Fatal(regErr)
	}
	if _, probeErr := execution.Execute(ctx, h.service.runner, probe,
		func(tx *execution.Tx) (struct{}, error) {
			return struct{}{}, sProbe(h.service, ctx, tx, 1, `{"kind":"mystery"}`)
		},
		func(struct{}) int64 { return 0 }); probeErr == nil {
		t.Fatal("unknown scope kind must fail closed")
	} else {
		var rejection *execution.Rejection
		if !errors.As(probeErr, &rejection) || rejection.Code != "malformed_scope" {
			t.Fatalf("unknown kind error = %v, want malformed_scope", probeErr)
		}
	}
	var runs int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Fatalf("rejected expansions must not create runs, got %d", runs)
	}
}

// TestPlanRunRevalidatesViewConnectionAtCreation proves the run creation
// re-checks the view's current source intersection: a view re-pointed to a
// different connection after plan save must reject, never query the old
// connection with stale labels.
func TestPlanRunRevalidatesViewConnectionAtCreation(t *testing.T) {
	h := newTestHarness(t)
	ctx := commandContext(t)
	now := "2026-09-13T00:00:00Z"
	seedAlternateMetricsConnection(t, h.db)
	for _, statement := range []string{
		"INSERT INTO business_views(view_key,display_name,description,connection_id,label_conditions_json,row_version,created_by,created_at,updated_at) VALUES('mall-mysql-view','商城 MySQL','desc',1,'{\"env\":\"prod\"}',1,1,'" + now + "','" + now + "')",
		"INSERT INTO inspection_plans(plan_key,display_name,enabled,connection_id,plugin_id,template_id,template_version,params_json,scope_json,scope_kind,cron,timezone,row_version,created_by,created_at,updated_at) VALUES('mall-mysql-inspection','商城巡检',1,1,'thanos','promql_instant','1','{\"expression\":\"up\"}','{\"kind\":\"business_view\",\"businessViewKey\":\"mall-mysql-view\"}','business_view',NULL,'UTC',1,1,'" + now + "','" + now + "')",
	} {
		if _, err := h.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	// 用户在计划保存后把视图改绑到另一接入。
	if _, err := h.db.Exec(`UPDATE business_views SET connection_id=(SELECT id FROM connections WHERE name='alternate-metrics'), row_version=row_version+1 WHERE view_key='mall-mysql-view'`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CreatePlanRun(ctx, h.principal, "cmd-view-conflict", "mall-mysql-inspection"); err == nil {
		t.Fatal("re-pointed view must reject at run creation")
	} else {
		var rejection *RejectionError
		if !errors.As(err, &rejection) || rejection.Code != "scope_conflict" {
			t.Fatalf("view conflict error = %v, want scope_conflict", err)
		}
	}
	var runs int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Fatalf("conflicting view must not create runs, got %d", runs)
	}
}

// TestRerunPreservesOriginalViewConditionsThroughRealDispatch walks the real
// recovery chain: a business_view run completes collection, the view's
// conditions are then modified, and the re-collection (Rerun) must rebuild
// dispatch inputs that are digest-consistent with the source run's frozen
// conditions — never re-reading the modified view.
func TestRerunPreservesOriginalViewConditionsThroughRealDispatch(t *testing.T) {
	h := newTestHarness(t)
	ctx := commandContext(t)
	now := "2026-09-13T00:00:00Z"
	if _, err := h.db.Exec(`INSERT INTO business_views(view_key,display_name,description,connection_id,label_conditions_json,row_version,created_by,created_at,updated_at) VALUES('mall-mysql-view','商城 MySQL','desc',1,'{"service":"mysql"}',1,1,'` + now + "','" + now + "')"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`INSERT INTO inspection_plans(plan_key,display_name,enabled,connection_id,plugin_id,template_id,template_version,params_json,scope_json,scope_kind,cron,timezone,row_version,created_by,created_at,updated_at) VALUES('mall-mysql-inspection','商城巡检',1,1,'thanos','promql_instant','1','{"expression":"up"}','{"kind":"business_view","businessViewKey":"mall-mysql-view"}','business_view',NULL,'UTC',1,1,'` + now + "','" + now + "')"); err != nil {
		t.Fatal(err)
	}
	original, err := h.service.CreatePlanRun(ctx, h.principal, "rerun-chain-create", "mall-mysql-inspection")
	if err != nil {
		t.Fatal(err)
	}
	attempts := h.service.Attempts()
	sourceAttempt := h.promqlAttemptID(t, original.RunID)
	sourceInput, err := attempts.DispatchInputFor(ctx, sourceAttempt)
	if err != nil {
		t.Fatal(err)
	}
	var sourceParsed map[string]any
	if err := json.Unmarshal(sourceInput.CanonicalJSON, &sourceParsed); err != nil {
		t.Fatal(err)
	}
	// 视图条件随后被修改：重采证必须沿源 Run 冻结条件，不读新视图。
	if _, err := h.db.Exec(`UPDATE business_views SET label_conditions_json='{"service":"redis"}', row_version=row_version+1 WHERE view_key='mall-mysql-view'`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CancelRun(ctx, h.principal, "rerun-chain-cancel", original.RunID, original.RowVersion); err != nil {
		t.Fatal(err)
	}
	rerun, err := h.service.RerunInspection(ctx, h.principal, "rerun-chain-rerun", original.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if rerun.RunID == original.RunID || rerun.State != "Running" {
		t.Fatalf("rerun = %+v", rerun)
	}
	rerunAttempt := h.promqlAttemptID(t, rerun.RunID)
	// 真实派发路径：digest 复核必须一致（此前生产即在此挂起）。
	rebuilt, err := attempts.DispatchInputFor(ctx, rerunAttempt)
	if err != nil {
		t.Fatalf("rerun dispatch rebuild must be digest-consistent: %v", err)
	}
	var rerunParsed map[string]any
	if err := json.Unmarshal(rebuilt.CanonicalJSON, &rerunParsed); err != nil {
		t.Fatal(err)
	}
	if rerunParsed["scopeKind"] != "businessView" {
		t.Fatalf("rerun scopeKind = %v", rerunParsed["scopeKind"])
	}
	target, _ := rerunParsed["target"].(map[string]any)
	conditions, _ := target["labelConditions"].(map[string]any)
	if conditions["service"] != "mysql" {
		t.Fatalf("rerun must freeze the SOURCE run conditions (service=mysql), got %v", conditions)
	}
	if conditions["service"] == "redis" {
		t.Fatal("rerun must never read the modified view")
	}
	_ = sourceParsed
}

// sProbe adapts expandPlanScope to the runner's Execute callback shape.
func sProbe(service *Service, ctx context.Context, tx *execution.Tx, connectionID int64, scopeJSON string) error {
	_, _, _, err := service.expandPlanScope(ctx, tx, connectionID, "mystery", scopeJSON)
	return err
}
