package inspection

// 知识接入代的巡检报告引用回归：分析 Attempt 冻结工具目录（巡检首次），
// 重建字节与冻结一致；报告的知识引用由 Quoin 从本 Attempt 成功的
// knowledge_get 实际消费记录权威推导——只 search 未读正文不算消费、失败读取
// 不算消费、提案声明的列表不进入 ledger。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/attempt"
)

// completedAnalysisRun drives one plan run to a closed collection with the
// auto-created analysis attempt, returning (runID, analysisAttemptID).
func completedAnalysisRun(t *testing.T, h *testHarness, planKey, commandID string) (int64, int64) {
	t.Helper()
	h.seedPlan(t, planKey)
	h.seedModelProvider(t)
	store, err := artifact.NewStore(h.db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.service.SetArtifactWriter(store.MaterializeEvidenceTransaction)
	ctx := commandContext(t)
	detail, err := h.service.CreatePlanRun(ctx, h.principal, commandID, planKey)
	if err != nil {
		t.Fatal(err)
	}
	collectionID := h.promqlAttemptID(t, detail.RunID)
	h.dispatchPromQL(t, collectionID)
	if err := h.service.CommitPluginProposal(context.Background(), collectionID, "plinth-boot", 1, pluginSuccessProposal(t, h, collectionID, detail.RunID, "success")); err != nil {
		t.Fatal(err)
	}
	analysisID := h.analysisAttemptID(t, detail.RunID)
	return detail.RunID, analysisID
}

// TestReportAnalysisFreezesToolCatalog：巡检分析（知识接入代）在创建事务内
// 冻结工具目录——snapshot 携带 tool_catalog_json，canonical 输入内嵌同一文档
// （worker 侧 ProviderToolsDigestForInput 可解析），重建字节与冻结摘要一致。
func TestReportAnalysisFreezesToolCatalog(t *testing.T) {
	h := newTestHarness(t)
	_, analysisID := completedAnalysisRun(t, h, "knowledge-plan", "cmd-knowledge")

	var agentVersion, catalogJSON string
	if err := h.db.QueryRow(`
		SELECT a.agent_version, s.tool_catalog_json
		FROM execution_attempts a JOIN attempt_input_snapshots s ON s.attempt_id=a.id
		WHERE a.id=?`, analysisID).Scan(&agentVersion, &catalogJSON); err != nil {
		t.Fatal(err)
	}
	if agentVersion != attempt.InspectionAgentVersion {
		t.Fatalf("analysis agent version = %q", agentVersion)
	}
	if catalogJSON == "" {
		t.Fatal("inspection analysis snapshot must freeze a tool catalog")
	}
	// 重建字节内嵌同一目录且与冻结摘要一致（rebuildAttemptInput 内部复核
	// content_digest，任何漂移都会在此失败）。
	rebuilt, err := h.service.rebuildAttemptInput(context.Background(), analysisID)
	if err != nil {
		t.Fatal(err)
	}
	embedded, ok := attempt.CatalogFromInputDocument(rebuilt)
	if !ok {
		t.Fatal("rebuilt inspection input carries no embedded tool catalog")
	}
	var frozen attempt.FrozenCatalog
	if err := json.Unmarshal([]byte(catalogJSON), &frozen); err != nil {
		t.Fatal(err)
	}
	frozenBytes, _ := json.Marshal(&frozen)
	embeddedBytes, _ := json.Marshal(embedded)
	if string(frozenBytes) != string(embeddedBytes) {
		t.Fatalf("embedded catalog drifted from the frozen document:\n%s\n%s", embeddedBytes, frozenBytes)
	}
	names := map[string]bool{}
	for _, tool := range embedded.Tools {
		names[tool.Name] = true
	}
	for _, required := range []string{"artifact_read", "artifact_grep", "knowledge_search", "knowledge_get"} {
		if !names[required] {
			t.Fatalf("inspection frozen catalog lost %s: %v", required, names)
		}
	}
	if embedded.AgentVersion != attempt.InspectionAgentVersion {
		t.Fatalf("frozen catalog agent = %q", embedded.AgentVersion)
	}
}

// TestLegacyReportInputOmitsToolCatalog：旧形状（无目录）重建的字节保证——
// reportInput.ToolCatalog 为 nil 时 canonical JSON 不携带 toolCatalog 键，旧
// Attempt 的输入摘要保持可重建。
func TestLegacyReportInputOmitsToolCatalog(t *testing.T) {
	input := reportInput{
		SchemaKind: reportInputKind, AttemptID: 7, InspectionRunID: 3, ReportVersion: 1,
		PlanKey: "p", EvidenceIDs: []int64{}, ArtifactIDs: []int64{}, KnowledgeVersionID: []int64{},
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "toolCatalog") {
		t.Fatalf("nil ToolCatalog must stay omitted for legacy rebuild bytes: %s", body)
	}
	withCatalog := input
	catalog := &attempt.FrozenCatalog{SchemaVersion: "inspection-analysis-tools-v1", AgentVersion: attempt.InspectionAgentVersion}
	withCatalog.ToolCatalog = catalog
	body, err = json.Marshal(withCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "toolCatalog") {
		t.Fatalf("non-nil ToolCatalog must embed into the canonical input: %s", body)
	}
}

// TestReportKnowledgeCitationsRequireModelConsumption：ledger 的知识引用只来自
// 「执行成功且其封存结果进入报告所依据模型调用输入谱系」的 knowledge_get 调用
// （首次读取顺序去重）。执行成功但未被模型消费（如被淘汰出最终上下文）、
// knowledge_search 命中、失败读取与提案声明值都不构成引用。
func TestReportKnowledgeCitationsRequireModelConsumption(t *testing.T) {
	h := newTestHarness(t)
	runID, analysisID := completedAnalysisRun(t, h, "knowledge-plan", "cmd-knowledge")
	ctx := commandContext(t)

	// 播种两份已确认知识（版本 101/102）。
	seedConfirmedKnowledge(t, h, 101, "数据库连接池打满处理")
	seedConfirmedKnowledge(t, h, 102, "P95 延迟升高排查")

	// 分析 Attempt 进入 Running。第一轮模型调用声明 4 个工具调用：
	// knowledge_get(101)、knowledge_get(102)、knowledge_get(101)（重复）、
	// knowledge_get(999)（随后失败）。
	if err := h.attempts.BindToSlot(ctx, analysisID, "plinth", "plinth-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.Accept(ctx, analysisID, "plinth-boot", 1); err != nil {
		t.Fatal(err)
	}
	promptDigest := strings.Repeat("c", 64)
	toolCalls := []map[string]any{
		{"id": "call-k1", "name": "knowledge_get", "arguments": map[string]any{"versionId": 101}},
		{"id": "call-k2", "name": "knowledge_get", "arguments": map[string]any{"versionId": 102}},
		{"id": "call-k3", "name": "knowledge_get", "arguments": map[string]any{"versionId": 101}},
		{"id": "call-k4", "name": "knowledge_get", "arguments": map[string]any{"versionId": 999}},
	}
	proposingCallID := seedModelCallWithToolProposals(t, h, analysisID, promptDigest, toolCalls)
	seedKnowledgeToolCall(t, h, analysisID, proposingCallID, 0, "call-k1", "knowledge_get", `{"versionId":101}`, "succeeded")
	seedKnowledgeToolCall(t, h, analysisID, proposingCallID, 1, "call-k2", "knowledge_get", `{"versionId":102}`, "succeeded")
	seedKnowledgeToolCall(t, h, analysisID, proposingCallID, 2, "call-k3", "knowledge_get", `{"versionId":101}`, "succeeded")
	seedKnowledgeToolCall(t, h, analysisID, proposingCallID, 3, "call-k4", "knowledge_get", `{"versionId":999}`, "failed")

	// 工具完成后，worker 以累积输入谱系发起最终（报告）模型调用：系统契约、
	// 工具目录、前一轮调用与各工具结果。102 的读取虽执行成功，但结果未进入
	// 最终调用输入（模拟被上下文淘汰）——不构成引用依据。
	finalCallID := seedFinalModelCallConsumingToolResults(t, h, analysisID, promptDigest, proposingCallID, []string{"call-k1", "call-k3"})

	evidenceIDs := runEvidenceIDs(t, h, runID)
	if err := h.service.CommitReportProposal(context.Background(), analysisID, "plinth-boot", 1, reportProposalBody(analysisID, runID, finalCallID, "报告正文", evidenceIDs, []int64{}, promptDigest)); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := h.db.QueryRow(`SELECT knowledge_version_ids_json FROM inspection_report_result_ledgers WHERE attempt_id=?`, analysisID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "[101]" {
		t.Fatalf("ledger knowledge citations = %s, want [101] (only the model-consumed first read)", stored)
	}
	var referenced int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_report_knowledge_versions`).Scan(&referenced); err != nil {
		t.Fatal(err)
	}
	if referenced != 1 {
		t.Fatalf("report knowledge references = %d", referenced)
	}
}

// TestReportProposalClaimedKnowledgeIsIgnoredWithoutConsumption：没有任何
// knowledge_get 消费时，即使提案声明引用（worker 契约恒发空列表，此处模拟
// 篡改声明），ledger 仍封存空引用。
func TestReportProposalClaimedKnowledgeIsIgnoredWithoutConsumption(t *testing.T) {
	h := newTestHarness(t)
	runID, analysisID := completedAnalysisRun(t, h, "knowledge-plan-2", "cmd-knowledge-2")
	ctx := commandContext(t)
	seedConfirmedKnowledge(t, h, 201, "未读取的知识")

	if err := h.attempts.BindToSlot(ctx, analysisID, "plinth", "plinth-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.Accept(ctx, analysisID, "plinth-boot", 1); err != nil {
		t.Fatal(err)
	}
	promptDigest := strings.Repeat("d", 64)
	callID := h.seedSucceededModelCall(t, analysisID, promptDigest)
	evidenceIDs := runEvidenceIDs(t, h, runID)

	// 声明引用 201（传输摘要按声明值计算，保持 wire 一致），但没有任何
	// knowledge_get 消费记录。
	evidenceJSON, _ := json.Marshal(evidenceIDs)
	evidenceSum := sha256.Sum256(evidenceJSON)
	evidenceDigest := hex.EncodeToString(evidenceSum[:])
	artifactJSON, _ := json.Marshal([]int64{})
	claimed := []int64{201}
	claimedJSON, _ := json.Marshal(claimed)
	canonical := fmt.Sprintf("inspection_report_result_v1|%d|%d|%d|success|%s|%s|%s|%s|%s|%s",
		analysisID, runID, callID, "报告正文", string(evidenceJSON), string(artifactJSON), string(claimedJSON), evidenceDigest, promptDigest)
	resultSum := sha256.Sum256([]byte(canonical))
	body, _ := json.Marshal(map[string]any{
		"schemaKind": "inspection_report_result_v1", "attemptId": analysisID, "inspectionRunId": runID,
		"modelCallId": callID, "outcome": "success", "content": "报告正文",
		"evidenceIds": evidenceIDs, "artifactIds": []int64{}, "knowledgeVersionIds": claimed,
		"resultDigest": hex.EncodeToString(resultSum[:]), "evidenceDigest": evidenceDigest, "promptDigest": promptDigest,
	})
	if err := h.service.CommitReportProposal(context.Background(), analysisID, "plinth-boot", 1, body); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := h.db.QueryRow(`SELECT knowledge_version_ids_json FROM inspection_report_result_ledgers WHERE attempt_id=?`, analysisID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "[]" {
		t.Fatalf("claimed-but-unconsumed knowledge must not be cited, ledger = %s", stored)
	}
}

// TestReportAgainstProposingCallCitesNothing：报告引用提出工具调用的那次模型
// 调用本身（其输入只有 system_contract/tool_schema，工具结果从未进入其输入
// 谱系）时，即使 knowledge_get 已执行成功，也不构成引用——这正是“执行成功
// ≠模型消费”的边界。
func TestReportAgainstProposingCallCitesNothing(t *testing.T) {
	h := newTestHarness(t)
	runID, analysisID := completedAnalysisRun(t, h, "knowledge-plan-3", "cmd-knowledge-3")
	ctx := commandContext(t)
	seedConfirmedKnowledge(t, h, 301, "已读取但未被最终调用消费")

	if err := h.attempts.BindToSlot(ctx, analysisID, "plinth", "plinth-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.Accept(ctx, analysisID, "plinth-boot", 1); err != nil {
		t.Fatal(err)
	}
	promptDigest := strings.Repeat("e", 64)
	toolCalls := []map[string]any{
		{"id": "call-s1", "name": "knowledge_get", "arguments": map[string]any{"versionId": 301}},
	}
	proposingCallID := seedModelCallWithToolProposals(t, h, analysisID, promptDigest, toolCalls)
	seedKnowledgeToolCall(t, h, analysisID, proposingCallID, 0, "call-s1", "knowledge_get", `{"versionId":301}`, "succeeded")

	// 报告直接引用提出工具调用的模型调用：其输入谱系没有该工具结果。
	evidenceIDs := runEvidenceIDs(t, h, runID)

	// schema 闭包负例：直接 SQL 写入一条引用 301 的 ledger——读取虽执行成功，
	// 但 301 的工具结果没有进入报告模型调用的输入谱系，触发器必须拒绝。
	evidenceJSON, _ := json.Marshal(evidenceIDs)
	evidenceSum := sha256.Sum256(evidenceJSON)
	evidenceDigest := hex.EncodeToString(evidenceSum[:])
	artifactJSON, _ := json.Marshal([]int64{})
	cited := []int64{301}
	citedJSON, _ := json.Marshal(cited)
	forgedCanonical := fmt.Sprintf("inspection_report_result_v1|%d|%d|%d|success|%s|%s|%s|%s|%s|%s",
		analysisID, runID, proposingCallID, "报告正文", string(evidenceJSON), string(artifactJSON), string(citedJSON), evidenceDigest, promptDigest)
	forgedSum := sha256.Sum256([]byte(forgedCanonical))
	_, insertErr := h.db.Exec(`INSERT INTO inspection_report_result_ledgers(
			attempt_id, inspection_run_id, report_version, model_call_id, result_digest, evidence_digest,
			content, prompt_digest, evidence_ids_json, artifact_ids_json, knowledge_version_ids_json, created_at)
		VALUES(?,?,1,?,?,?,?,?,?,?,?,?)`,
		analysisID, runID, proposingCallID, forgedSum[:], evidenceDigest,
		"报告正文", promptDigest, string(evidenceJSON), string(artifactJSON), string(citedJSON), time.Now().UTC().Format(time.RFC3339Nano))
	if insertErr == nil || !strings.Contains(insertErr.Error(), "inspection Report ResultProposal") {
		t.Fatalf("schema closure must reject citing knowledge the report call never consumed, err=%v", insertErr)
	}

	if err := h.service.CommitReportProposal(context.Background(), analysisID, "plinth-boot", 1, reportProposalBody(analysisID, runID, proposingCallID, "报告正文", evidenceIDs, []int64{}, promptDigest)); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := h.db.QueryRow(`SELECT knowledge_version_ids_json FROM inspection_report_result_ledgers WHERE attempt_id=?`, analysisID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "[]" {
		t.Fatalf("executed-but-unconsumed read must not be cited, ledger = %s", stored)
	}
}

// seedFinalModelCallConsumingToolResults 完成报告产出的最终模型调用（call_seq=2，
// 无工具调用提案），输入谱系镜像 worker 的累积形状：系统契约、工具目录、前
// 一轮调用（prior_call）与 consumedProviderIDs 指定（按 provider_tool_call_id
// 解析）的工具结果项（item_role='tool'）。返回新调用 ID。
func seedFinalModelCallConsumingToolResults(t *testing.T, h *testHarness, attemptID int64, promptDigest string, priorCallID int64, consumedProviderIDs []string) int64 {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	call, err := h.db.Exec(`
		INSERT INTO model_calls(attempt_id, call_seq, retry_seq, operation, model_id, connection_grant_id, prompt_renderer_version, agent_version, prompt_digest, tool_schema_version, tool_schema_digest, input_snapshot_digest, rendered_request_digest, context_budget_tokens, max_output_tokens, estimated_input_tokens, evicted_turn_count, status, started_at)
		VALUES(?, 2, 0, 'chat', 'fixture-chat-1', (SELECT id FROM attempt_connection_grants WHERE attempt_id=? AND purpose='chat_model'), 'v1', 'agent-v1', ?, 'v1', ?, ?, ?, 2, 1, 0, 0, 'running', ?)`,
		attemptID, attemptID, promptDigest, promptDigest, promptDigest, promptDigest, now)
	if err != nil {
		t.Fatal(err)
	}
	callID, _ := call.LastInsertId()
	type lineageItem struct {
		role      string
		kind      string
		toolCall  int64
		priorCall int64
	}
	items := []lineageItem{
		{role: "system", kind: "system_contract"},
		{role: "system", kind: "tool_schema"},
		{role: "assistant", kind: "prior_call", priorCall: priorCallID},
	}
	for _, providerID := range consumedProviderIDs {
		var toolCallID int64
		if err := h.db.QueryRow(`SELECT id FROM tool_calls WHERE attempt_id=? AND provider_tool_call_id=?`, attemptID, providerID).Scan(&toolCallID); err != nil {
			t.Fatal(err)
		}
		items = append(items, lineageItem{role: "tool", kind: "tool_call", toolCall: toolCallID})
	}
	for index, item := range items {
		var toolCallID, priorCallIDValue any
		var synthetic any
		switch item.kind {
		case "tool_call":
			toolCallID = item.toolCall
		case "prior_call":
			priorCallIDValue = item.priorCall
		case "system_contract":
			synthetic = "system_contract"
		case "tool_schema":
			synthetic = "tool_schema"
		}
		if _, err := h.db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,prior_model_call_id,tool_call_id,synthetic_kind) VALUES(?,?,?,?,?,?,?)`,
			callID, index+1, item.role, promptDigest, priorCallIDValue, toolCallID, synthetic); err != nil {
			t.Fatal(err)
		}
	}
	output := `{"assistantText":"报告正文","finishReason":"stop","tool_calls":[]}`
	if _, err := h.db.Exec(`INSERT INTO model_call_outputs(model_call_id, complete, response_json, response_digest, finish_reason, created_at) VALUES(?, 1, ?, ?, 'stop', ?)`, callID, output, promptDigest, now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE model_calls SET status='succeeded', usage_json='{}', latency_ms=1, ended_at=? WHERE id=?`, now, callID); err != nil {
		t.Fatal(err)
	}
	return callID
}

// runEvidenceIDs 收集一个 Run 全部成功检查的 Evidence 定位符。
func runEvidenceIDs(t *testing.T, h *testHarness, runID int64) []int64 {
	t.Helper()
	rows, err := h.db.Query(`SELECT evidence_id FROM inspection_check_results WHERE run_id=? AND evidence_id IS NOT NULL ORDER BY check_key`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// seedConfirmedKnowledge 播种一份已确认知识（版本号由调用者指定），插入顺序
// 遵循 confirm.go 的生产闭包（候选确认绑定 → 版本插入 → 指针前移 → 检索投影）。
func seedConfirmedKnowledge(t *testing.T, h *testHarness, versionID int64, title string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// 候选来源走 source_material 导入路径（真实不可变来源闭包）。
	material, err := h.db.Exec(`INSERT INTO source_materials(kind,digest,size_bytes,content,created_by,created_at) VALUES('knowledge_import',?,?,'导入原文',1,?)`, fmt.Sprintf("%064x", versionID), len("导入原文"), now)
	if err != nil {
		t.Fatal(err)
	}
	materialID, _ := material.LastInsertId()
	batch, err := h.db.Exec(`INSERT INTO knowledge_import_batches(source_material_id,state,created_by,created_at) VALUES(?,'Processing',1,?)`, materialID, now)
	if err != nil {
		t.Fatal(err)
	}
	batchID, _ := batch.LastInsertId()
	candidate, err := h.db.Exec(`INSERT INTO knowledge_candidates(import_batch_id,source_type,source_id,state,original_suggestion_json,draft_title,draft_body,created_by,created_at) VALUES(?,'source_material',?,'AwaitingConfirmation','{}',?,?,1,?)`, batchID, materialID, title, "正文：处理步骤。", now)
	if err != nil {
		t.Fatal(err)
	}
	candidateID, _ := candidate.LastInsertId()
	knowledge, err := h.db.Exec(`INSERT INTO reusable_knowledge(created_by,created_at) VALUES(1,?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	knowledgeID, _ := knowledge.LastInsertId()
	if _, err = h.db.Exec(`UPDATE knowledge_candidates SET state='Confirmed',confirmed_knowledge_id=?,row_version=row_version+1 WHERE id=?`, knowledgeID, candidateID); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec(`INSERT INTO knowledge_versions(id,knowledge_id,version_seq,title,body,source_candidate_id,created_by,created_at) VALUES(?,?,1,?,'正文：处理步骤。',?,1,?)`, versionID, knowledgeID, title, candidateID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec(`UPDATE reusable_knowledge SET current_version_id=?,row_version=row_version+1 WHERE id=?`, versionID, knowledgeID); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec(`INSERT INTO knowledge_version_retrieval_state(knowledge_version_id,updated_at) VALUES(?,?)`, versionID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec(`INSERT INTO knowledge_search_docs(knowledge_version_id,title,body) VALUES(?,?,?)`, versionID, title, "正文：处理步骤。"); err != nil {
		t.Fatal(err)
	}
	// 批次状态闭包允许 Processing → AwaitingConfirmation → Completed 前向迁移。
	if _, err = h.db.Exec(`UPDATE knowledge_import_batches SET state='AwaitingConfirmation',row_version=row_version+1 WHERE id=?`, batchID); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec(`UPDATE knowledge_import_batches SET state='Completed',row_version=row_version+1 WHERE id=?`, batchID); err != nil {
		t.Fatal(err)
	}
}

// seedModelCallWithToolProposals 完成一次声明若干工具调用提案的成功模型调用。
func seedModelCallWithToolProposals(t *testing.T, h *testHarness, attemptID int64, promptDigest string, proposals []map[string]any) int64 {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	call, err := h.db.Exec(`
		INSERT INTO model_calls(attempt_id, call_seq, retry_seq, operation, model_id, connection_grant_id, prompt_renderer_version, agent_version, prompt_digest, tool_schema_version, tool_schema_digest, input_snapshot_digest, rendered_request_digest, context_budget_tokens, max_output_tokens, estimated_input_tokens, evicted_turn_count, status, started_at)
		VALUES(?, 1, 0, 'chat', 'fixture-chat-1', (SELECT id FROM attempt_connection_grants WHERE attempt_id=? AND purpose='chat_model'), 'v1', 'agent-v1', ?, 'v1', ?, ?, ?, 2, 1, 0, 0, 'running', ?)`,
		attemptID, attemptID, promptDigest, promptDigest, promptDigest, promptDigest, now)
	if err != nil {
		t.Fatal(err)
	}
	callID, _ := call.LastInsertId()
	for seq, kind := range []string{"system_contract", "tool_schema"} {
		if _, err = h.db.Exec(`INSERT INTO model_call_input_items(model_call_id, item_seq, item_role, source_digest, synthetic_kind) VALUES(?,?, 'system', ?, ?)`, callID, seq+1, promptDigest, kind); err != nil {
			t.Fatal(err)
		}
	}
	output := map[string]any{"assistantText": "", "finishReason": "tool_calls", "tool_calls": proposals}
	outputJSON, _ := json.Marshal(output)
	if _, err = h.db.Exec(`INSERT INTO model_call_outputs(model_call_id, complete, response_json, response_digest, finish_reason, created_at) VALUES(?, 1, ?, ?, 'tool_calls', ?)`, callID, string(outputJSON), promptDigest, now); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec(`UPDATE model_calls SET status='succeeded', usage_json='{}', latency_ms=1, ended_at=? WHERE id=?`, now, callID); err != nil {
		t.Fatal(err)
	}
	return callID
}

// seedKnowledgeToolCall 播种一条知识工具调用并推进到指定终态（succeeded 携带
// 结果预览，failed 携带 return_to_model 错误载荷）。
func seedKnowledgeToolCall(t *testing.T, h *testHarness, attemptID, callID int64, toolIndex int, providerID, toolName, arguments, status string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	argumentsDigest := sha256.Sum256([]byte(arguments))
	insert, err := h.db.Exec(`
		INSERT INTO tool_calls(attempt_id, model_call_id, call_seq, tool_index, provider_tool_call_id, tool_name, tool_version, arguments_json, arguments_digest, execution_mode, failure_mode, status, created_at)
		VALUES(?,?,1,?,?,?,?,?,?,'quoin_routed','return_to_model','pending',?)`,
		attemptID, callID, toolIndex, providerID, toolName, "1", arguments, hex.EncodeToString(argumentsDigest[:]), now)
	if err != nil {
		t.Fatal(err)
	}
	toolCallID, _ := insert.LastInsertId()
	started := time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano)
	if _, err = h.db.Exec(`UPDATE tool_calls SET status='running', started_at=?, row_version=row_version+1 WHERE id=?`, started, toolCallID); err != nil {
		t.Fatal(err)
	}
	if status == "succeeded" {
		result, _ := json.Marshal(map[string]any{"success": true})
		if _, err = h.db.Exec(`UPDATE tool_calls SET status='succeeded', result_json=?, ended_at=?, row_version=row_version+1 WHERE id=?`, string(result), started, toolCallID); err != nil {
			t.Fatal(err)
		}
		return
	}
	failure, _ := json.Marshal(map[string]any{"success": false, "errorCode": "knowledge_version_ineligible", "errorDetail": "不存在"})
	if _, err = h.db.Exec(`UPDATE tool_calls SET status='failed', result_json=?, error_detail='不存在', ended_at=?, row_version=row_version+1 WHERE id=?`, string(failure), started, toolCallID); err != nil {
		t.Fatal(err)
	}
}
