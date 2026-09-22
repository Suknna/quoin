package inspection

// Immutable report closure domain tests: analysis creation after collection,
// frozen version/locator inputs, ledger commit, replay and tamper rejection.

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
	_ "github.com/Suknna/quoin/internal/quoin/bootstrap"
)

// seedModelProvider registers one enabled qualified model provider through
// the same durable facts the capability-probe path writes (ported from the
// analysis harness seedProviderChain).
func (h *testHarness) seedModelProvider(t *testing.T) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var existing int64
	if err := h.db.QueryRow(`SELECT id FROM connections WHERE type='model_provider' AND enabled=1`).Scan(&existing); err == nil {
		return
	}
	connection, err := h.db.Exec(`INSERT INTO connections(name,type,enabled,created_at) VALUES('model','model_provider',0,?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	connectionID, _ := connection.LastInsertId()
	revision, err := h.db.Exec(`INSERT INTO connection_revisions(connection_id,revision_seq,config_json,created_at) VALUES(?,1,?,?)`, connectionID, `{"baseUrl":"https://provider.test","chatModelId":"fixture-chat-1","contextBudgetTokens":4096,"maxOutputTokens":1024}`, now)
	if err != nil {
		t.Fatal(err)
	}
	revisionID, _ := revision.LastInsertId()
	nonce := make([]byte, 12)
	for i := range nonce {
		nonce[i] = byte(7*31 + i)
	}
	generation, err := h.db.Exec(`INSERT INTO credential_generations(connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_at) VALUES(?,1,1,1,?,?,?)`, connectionID, nonce, []byte(strings.Repeat("f", 32)), now)
	if err != nil {
		t.Fatal(err)
	}
	generationID, _ := generation.LastInsertId()
	if _, err = h.db.Exec(`UPDATE connections SET current_revision_id=?, current_credential_generation_id=?, row_version=row_version+1 WHERE id=?`, revisionID, generationID, connectionID); err != nil {
		t.Fatal(err)
	}
	probeAttempt, err := h.db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,created_at) VALUES('connection_probe','connection',?,'Queued','test',?)`, connectionID, now)
	if err != nil {
		t.Fatal(err)
	}
	probeAttemptID, _ := probeAttempt.LastInsertId()
	probeSnapshot, err := h.db.Exec(`INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(?,'connection_probe_v1','connection-probe-v1',?,?)`, probeAttemptID, strings.Repeat("0", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	probeSnapshotID, _ := probeSnapshot.LastInsertId()
	if _, err = h.db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id) VALUES(?,1,'user',?,?)`, probeSnapshotID, strings.Repeat("0", 64), revisionID); err != nil {
		t.Fatal(err)
	}
	var probeChatGrantID, probeEmbeddingGrantID int64
	for _, purpose := range []string{"model_probe_chat", "model_probe_embedding"} {
		grant, grantErr := h.db.Exec(`INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at) VALUES(?,?,?,?,?,?)`, probeAttemptID, purpose, connectionID, revisionID, generationID, now)
		if grantErr != nil {
			t.Fatal(grantErr)
		}
		if purpose == "model_probe_chat" {
			probeChatGrantID, _ = grant.LastInsertId()
		} else {
			probeEmbeddingGrantID, _ = grant.LastInsertId()
		}
	}
	if _, err = h.db.Exec(`UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='boot',connection_epoch=1,lease_until=?,runtime_release_version='test',row_version=row_version+1 WHERE id=? AND state='Queued'`, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec(`UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=? AND state='Assigned'`, now, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
	probe, err := h.db.Exec(`INSERT INTO connection_probe_results(attempt_id,connection_id,connection_type,connection_revision_id,credential_generation_id,root_binding_revision,action_set_id,action_set_version,probe_contract_digest,outcome,result_digest,started_at,finished_at,created_at) VALUES(?,?,?,?,?,1,'model-provider-capabilities',1,?,?,?,?,?,?)`,
		probeAttemptID, connectionID, "model_provider", revisionID, generationID, strings.Repeat("0", 64), "passed", strings.Repeat("1", 64), now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	probeResultID, _ := probe.LastInsertId()
	h.seedProbeModelCall(t, probeAttemptID, probeChatGrantID, 1, now, true)
	h.seedProbeModelCall(t, probeAttemptID, probeChatGrantID, 4, now, false)
	h.seedProbeEmbeddingCall(t, probeAttemptID, probeEmbeddingGrantID, probeSnapshotID, now)
	if _, err = h.db.Exec(`INSERT INTO model_provider_connection_probe_results(probe_result_id,chat_model_id,embedding_model_id,context_budget_tokens,max_output_tokens,streaming_supported,native_tool_calling_supported,multi_tool_call_supported,cancellation_observed,usage_observed,request_id_observed,embedding_supported,embedding_vector_dim,detail_json) VALUES(?,'fixture-chat-1','fixture-embed-1',4096,1024,1,1,1,1,1,1,1,16,'{}')`, probeResultID); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec(`UPDATE execution_attempts SET state='Succeeded',ended_at=?,row_version=row_version+1 WHERE id=? AND state='Running'`, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec(`INSERT INTO connection_enable_qualifications(connection_id,enabled_row_version,probe_result_id,created_by,created_at) VALUES(?,3,?,?,?)`, connectionID, probeResultID, h.principal, now); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec(`UPDATE connections SET enabled=1,revalidation_required=0,row_version=row_version+1 WHERE id=? AND row_version=2`, connectionID); err != nil {
		t.Fatal(err)
	}
}

func (h *testHarness) seedProbeModelCall(t *testing.T, attemptID, grantID int64, callSeq int64, now string, succeeded bool) {
	t.Helper()
	digest := strings.Repeat("1", 64)
	call, err := h.db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,evicted_turn_count,status,started_at) VALUES(?,?,'0','chat','fixture-chat-1',?,'connection-probe-v1','probe-supervisor-v1',?,?,?,?,?,4096,1024,0,0,'running',?)`,
		attemptID, callSeq, grantID, digest, digest, digest, digest, digest, now)
	if err != nil {
		t.Fatal(err)
	}
	callID, _ := call.LastInsertId()
	if _, err = h.db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,1,'system',?,'system_contract'),(?,2,'system',?,'tool_schema')`, callID, digest, callID, digest); err != nil {
		t.Fatal(err)
	}
	if succeeded {
		if _, err = h.db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,1,'{"assistantText":"ok","finishReason":"stop","tool_calls":[]}',?, 'stop',?)`, callID, digest, now); err != nil {
			t.Fatal(err)
		}
		if _, err = h.db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":1,"total_tokens":2}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err = h.db.Exec(`UPDATE model_calls SET status='cancelled',termination_reason='cancelled',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
		t.Fatal(err)
	}
}

func (h *testHarness) seedProbeEmbeddingCall(t *testing.T, attemptID, grantID, snapshotID int64, now string) {
	t.Helper()
	digest := strings.Repeat("1", 64)
	call, err := h.db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,input_snapshot_digest,rendered_request_digest,estimated_input_tokens,status,started_at) VALUES(?,'6','0','embedding','fixture-embed-1',?,?,?,0,'running',?)`,
		attemptID, grantID, digest, digest, now)
	if err != nil {
		t.Fatal(err)
	}
	callID, _ := call.LastInsertId()
	if _, err = h.db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,attempt_input_snapshot_id) VALUES(?,1,'user',?,?)`, callID, digest, snapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,created_at) VALUES(?,1,'{}',?,?)`, callID, digest, now); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":0,"total_tokens":1}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
		t.Fatal(err)
	}
}

func (h *testHarness) analysisAttemptID(t *testing.T, runID int64) int64 {
	t.Helper()
	var id int64
	if err := h.db.QueryRow(`SELECT id FROM execution_attempts WHERE attempt_type='inspection_analysis' AND scope_type='run' AND scope_id=?`, runID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// seedSucceededModelCall drives one complete chat call for the Running
// analysis attempt and returns its id (production drives this through the
// agent runtime; the domain test seals the same durable facts).
func (h *testHarness) seedSucceededModelCall(t *testing.T, attemptID int64, promptDigest string) int64 {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	call, err := h.db.Exec(`
		INSERT INTO model_calls(attempt_id, call_seq, retry_seq, operation, model_id, connection_grant_id, provider_request_id, prompt_renderer_version, agent_version, prompt_digest, tool_schema_version, tool_schema_digest, input_snapshot_digest, rendered_request_digest, context_budget_tokens, max_output_tokens, estimated_input_tokens, evicted_turn_count, usage_json, latency_ms, status, termination_reason, started_at, ended_at)
		VALUES(?, 1, 0, 'chat', 'fixture-chat-1', (SELECT id FROM attempt_connection_grants WHERE attempt_id=? AND purpose='chat_model'), NULL, 'v1', 'agent-v1', ?, 'v1', ?, ?, ?, 2, 1, 0, 0, NULL, NULL, 'running', NULL, ?, NULL)`,
		attemptID, attemptID, promptDigest, promptDigest, promptDigest, promptDigest, now)
	if err != nil {
		t.Fatal(err)
	}
	callID, err := call.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	for seq, kind := range []string{"system_contract", "tool_schema"} {
		if _, err = h.db.Exec(`
			INSERT INTO model_call_input_items(model_call_id, item_seq, item_role, source_digest, synthetic_kind)
			VALUES(?,?, 'system', ?, ?)`, callID, seq+1, promptDigest, kind); err != nil {
			t.Fatal(err)
		}
	}
	response := `{"tool_calls":[]}`
	if _, err = h.db.Exec(`
		INSERT INTO model_call_outputs(model_call_id, complete, response_json, response_digest, finish_reason, created_at)
		VALUES(?, 1, ?, ?, 'stop', ?)`, callID, response, promptDigest, now); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Exec(`UPDATE model_calls SET status='succeeded', usage_json='{}', latency_ms=1, ended_at=? WHERE id=?`, now, callID); err != nil {
		t.Fatal(err)
	}
	return callID
}

func reportProposalBody(attemptID, runID, callID int64, content string, evidenceIDs, artifactIDs []int64, promptDigest string) []byte {
	evidenceJSON, _ := json.Marshal(evidenceIDs)
	evidenceSum := sha256.Sum256(evidenceJSON)
	evidenceDigest := hex.EncodeToString(evidenceSum[:])
	artifactJSON, _ := json.Marshal(artifactIDs)
	canonical := fmt.Sprintf("inspection_report_result_v1|%d|%d|%d|success|%s|%s|%s|[]|%s|%s",
		attemptID, runID, callID, content, string(evidenceJSON), string(artifactJSON), evidenceDigest, promptDigest)
	resultSum := sha256.Sum256([]byte(canonical))
	body, _ := json.Marshal(map[string]any{
		"schemaKind": "inspection_report_result_v1", "attemptId": attemptID, "inspectionRunId": runID,
		"modelCallId": callID, "outcome": "success", "content": content,
		"evidenceIds": evidenceIDs, "artifactIds": artifactIDs, "knowledgeVersionIds": []int64{},
		"resultDigest": hex.EncodeToString(resultSum[:]), "evidenceDigest": evidenceDigest, "promptDigest": promptDigest,
	})
	return body
}

func TestImmutableReportClosure(t *testing.T) {
	h := newTestHarness(t)
	h.seedPlan(t, "mixed-plan")
	h.seedModelProvider(t)
	store, err := artifact.NewStore(h.db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.service.SetArtifactWriter(store.MaterializeEvidenceTransaction)
	ctx := commandContext(t)
	detail, err := h.service.CreatePlanRun(ctx, h.principal, "cmd-1", "mixed-plan")
	if err != nil {
		t.Fatal(err)
	}
	attemptID := h.promqlAttemptID(t, detail.RunID)
	h.dispatchPromQL(t, attemptID)
	if err := h.service.CommitPluginProposal(context.Background(), attemptID, "plinth-boot", 1, pluginSuccessProposal(t, h, attemptID, detail.RunID, "success")); err != nil {
		t.Fatal(err)
	}
	// Convergence created exactly one analysis attempt with the structured
	// preallocated version and the full frozen locator set.
	analysisID := h.analysisAttemptID(t, detail.RunID)
	var version int64
	var kind string
	if err := h.db.QueryRow(`SELECT inspection_report_version, schema_kind FROM attempt_input_snapshots WHERE attempt_id=?`, analysisID).Scan(&version, &kind); err != nil {
		t.Fatal(err)
	}
	if version != 1 || kind != "inspection_analysis_v1" {
		t.Fatalf("analysis snapshot = v%d/%s", version, kind)
	}
	var itemCount int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM attempt_input_items WHERE snapshot_id=(SELECT id FROM attempt_input_snapshots WHERE attempt_id=?)`, analysisID).Scan(&itemCount); err != nil {
		t.Fatal(err)
	}
	// run + plugin check result + evidence + readable artifact；计划 Run 不引用
	// 任何业务声明版本谱系项。
	if itemCount != 4 {
		t.Fatalf("analysis frozen items = %d", itemCount)
	}
	if err := h.attempts.BindToSlot(ctx, analysisID, "plinth", "plinth-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := h.attempts.Accept(ctx, analysisID, "plinth-boot", 1); err != nil {
		t.Fatal(err)
	}
	promptDigest := strings.Repeat("c", 64)
	callID := h.seedSucceededModelCall(t, analysisID, promptDigest)
	var evidenceIDs []int64
	rows, err := h.db.Query(`SELECT evidence_id FROM inspection_check_results WHERE run_id=? AND evidence_id IS NOT NULL ORDER BY check_key`, detail.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		evidenceIDs = append(evidenceIDs, id)
	}
	rows.Close()
	if len(evidenceIDs) != 1 {
		t.Fatalf("expected one collected evidence, got %d", len(evidenceIDs))
	}
	var artifactIDs []int64
	artifactRows, err := h.db.Query(`SELECT g.artifact_id FROM attempt_artifact_grants g JOIN attempt_input_snapshots s ON s.id=g.source_id JOIN attempt_input_items i ON i.snapshot_id=s.id AND i.artifact_id=g.artifact_id WHERE g.attempt_id=? AND g.source_kind='input_snapshot' ORDER BY i.item_seq`, analysisID)
	if err != nil {
		t.Fatal(err)
	}
	for artifactRows.Next() {
		var id int64
		if err = artifactRows.Scan(&id); err != nil {
			artifactRows.Close()
			t.Fatal(err)
		}
		artifactIDs = append(artifactIDs, id)
	}
	artifactRows.Close()
	if len(artifactIDs) != len(evidenceIDs) {
		t.Fatalf("frozen artifact grants = %v", artifactIDs)
	}
	if err := h.service.CommitReportProposal(context.Background(), analysisID, "plinth-boot", 1, reportProposalBody(analysisID, detail.RunID, callID, "巡检报告正文", evidenceIDs, artifactIDs, promptDigest)); err != nil {
		t.Fatal(err)
	}
	final, err := h.service.GetRun(ctx, detail.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if final.ReportCount != 1 {
		t.Fatalf("run should expose one report version, got %d", final.ReportCount)
	}
	report, err := h.service.GetReport(ctx, detail.RunID, 1)
	if err != nil || report.ID == "" || report.Version != 1 || report.Content != "巡检报告正文" || report.ModelID != "fixture-chat-1" || len(report.EvidenceIDs) != 1 || report.EvidenceDigest == "" {
		t.Fatalf("immutable report missing or wrong: %+v err=%v", report, err)
	}
	var analysisState string
	if err := h.db.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, analysisID).Scan(&analysisState); err != nil {
		t.Fatal(err)
	}
	if analysisState != "Succeeded" {
		t.Fatalf("analysis attempt state = %s", analysisState)
	}
	// Same-bytes replay is idempotent; tampered content never lands.
	if err := h.service.CommitReportProposal(context.Background(), analysisID, "plinth-boot", 1, reportProposalBody(analysisID, detail.RunID, callID, "巡检报告正文", evidenceIDs, artifactIDs, promptDigest)); err != nil {
		t.Fatalf("identical replay must be idempotent: %v", err)
	}
	if err := h.service.CommitReportProposal(context.Background(), analysisID, "plinth-boot", 1, reportProposalBody(analysisID, detail.RunID, callID, "篡改内容", evidenceIDs, artifactIDs, promptDigest)); err == nil {
		t.Fatal("tampered report replay must be rejected")
	}
	var reports int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_reports WHERE run_id=?`, detail.RunID).Scan(&reports); err != nil {
		t.Fatal(err)
	}
	if reports != 1 {
		t.Fatalf("exactly one immutable report must exist, got %d", reports)
	}
}

// TestInspectionAnalysisModelCallsStayOnFrozenEvidence 复现实机 fix4 Run2 的
// 失败形态并钉死纠正后的行为：巡检分析目录只含平台工具，模型按目录可用的
// 提案（alerts_recent/artifact_read）必须正常授权，不再出现 "needs a grant
// resolver" 导致的 invalid_response；目录外的实时指标工具（thanos_query，
// 模拟目录漂移/历史冻结目录）必须整体确定性拒绝且不留任何 tool call 行——
// 巡检重新分析绝不获得实时指标授权，也不混入执行时刻的实时数据。
func TestInspectionAnalysisModelCallsStayOnFrozenEvidence(t *testing.T) {
	h := newTestHarness(t)
	h.seedModelProvider(t)
	store, err := artifact.NewStore(h.db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.service.SetArtifactWriter(store.MaterializeEvidenceTransaction)
	ctx := commandContext(t)

	if err := h.service.EnsureDefaultPlan(ctx, 1, "fixture-metrics"); err != nil {
		t.Fatal(err)
	}
	detail, err := h.service.CreatePlanRun(ctx, h.principal, "catalog-run-0001", "basic-fixture-metrics")
	if err != nil {
		t.Fatal(err)
	}
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
	if _, err = attempts.DispatchInputFor(ctx, analysisID); err != nil {
		t.Fatalf("analysis dispatch rebuild: %v", err)
	}
	if err = h.attempts.BindToSlot(ctx, analysisID, "plinth", "plinth-boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err = h.attempts.Accept(ctx, analysisID, "plinth-boot", 1); err != nil {
		t.Fatal(err)
	}

	// 冻结目录必须只含平台工具：thanos_query 不在（根因纠正的直接断言）。
	var catalogTools string
	if err := h.db.QueryRow(`SELECT tool_catalog_json FROM attempt_input_snapshots WHERE attempt_id=?`, analysisID).Scan(&catalogTools); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(catalogTools, "thanos_query") {
		t.Fatal("inspection analysis frozen catalog exposes thanos_query (real-time metric tool)")
	}

	seedRunningCall := func(seq int) int64 {
		t.Helper()
		now := time.Now().UTC().Format(time.RFC3339Nano)
		digest := strings.Repeat("7", 64)
		call, callErr := h.db.Exec(`
			INSERT INTO model_calls(attempt_id, call_seq, retry_seq, operation, model_id, connection_grant_id, prompt_renderer_version, agent_version, prompt_digest, tool_schema_version, tool_schema_digest, input_snapshot_digest, rendered_request_digest, context_budget_tokens, max_output_tokens, estimated_input_tokens, status, started_at)
			VALUES(?,?, 0, 'chat', 'fixture-chat-1', (SELECT id FROM attempt_connection_grants WHERE attempt_id=? AND purpose='chat_model'), 'v1', 'inspection-analysis-v4', ?, 'inspection-analysis-tools-v1', ?, ?, ?, 4096, 1024, 0, 'running', ?)`,
			analysisID, seq, analysisID, digest, digest, digest, digest, now)
		if callErr != nil {
			t.Fatal(callErr)
		}
		callID, _ := call.LastInsertId()
		for itemSeq, kind := range []string{"system_contract", "tool_schema"} {
			if _, err = h.db.Exec(`INSERT INTO model_call_input_items(model_call_id, item_seq, item_role, source_digest, synthetic_kind) VALUES(?,?, 'system', ?, ?)`, callID, itemSeq+1, digest, kind); err != nil {
				t.Fatal(err)
			}
		}
		return callID
	}
	complete := func(callID int64, proposals []attempt.ProposedTool) ([]attempt.ToolAuthorization, error) {
		t.Helper()
		_, responseDigest, responseErr := attempt.CanonicalChatResponseJSON("", proposals)
		if responseErr != nil {
			t.Fatal(responseErr)
		}
		return attempts.CompleteModelCall(ctx, attempt.CompleteCall{
			AttemptID: analysisID, CallID: callID, Outcome: "succeeded",
			FinishReason: "tool_calls", ResponseDigest: responseDigest, ResponseComplete: true,
			ProposedTools: proposals,
		})
	}
	proposal := func(index int, providerID, name, arguments string) attempt.ProposedTool {
		t.Helper()
		sum := sha256.Sum256([]byte(arguments))
		return attempt.ProposedTool{
			ProviderIndex: uint32(index), ProviderToolCallID: providerID, ToolName: name,
			ArgumentsJSON: []byte(arguments), ArgumentsDigest: hex.EncodeToString(sum[:]),
		}
	}

	// 目录内提案（告警上下文 + 读取冻结证据）正常授权——不再 invalid_response。
	authorized, authorizedErr := complete(seedRunningCall(1), []attempt.ProposedTool{
		proposal(0, "call-alerts-0", "alerts_recent", `{"hours":24,"limit":10}`),
		proposal(1, "call-artifact-0", "artifact_read", `{"artifactId":"1","offset":1,"limit":10}`),
	})
	if authorizedErr != nil {
		t.Fatalf("in-catalog proposals must authorize (实机 bug: whole-call rejection): %v", authorizedErr)
	}
	if len(authorized) != 2 {
		t.Fatalf("authorizations=%+v", authorized)
	}

	// 前序 tool call 到终态（生产由 quoin_routed 编排封存；域测试按既有
	// seedKnowledgeToolCall 惯例直接落终态事实），序列约束才放行下一调用。
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, providerID := range []string{"call-alerts-0", "call-artifact-0"} {
		if _, err = h.db.Exec(`UPDATE tool_calls SET status='running', started_at=?, row_version=row_version+1 WHERE attempt_id=? AND provider_tool_call_id=? AND status='pending'`, now, analysisID, providerID); err != nil {
			t.Fatal(err)
		}
		if _, err = h.db.Exec(`UPDATE tool_calls SET status='succeeded', result_json='{"success":true,"output":"fixture"}', ended_at=?, row_version=row_version+1 WHERE attempt_id=? AND provider_tool_call_id=? AND status='running'`, now, analysisID, providerID); err != nil {
			t.Fatal(err)
		}
	}

	// 目录外实时指标工具：整体确定性拒绝且零 tool call 行落库。
	_, rogueErr := complete(seedRunningCall(2), []attempt.ProposedTool{
		proposal(0, "call-thanos-0", "thanos_query", `{"query":"redis_up or mysql_up"}`),
	})
	if rogueErr == nil {
		t.Fatal("out-of-catalog real-time metric tool must fail closed")
	}
	if !strings.Contains(rogueErr.Error(), "frozen catalog") {
		t.Fatalf("rejection must be the frozen-catalog denial, got: %v", rogueErr)
	}
	var rogueCalls int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM tool_calls WHERE attempt_id=? AND provider_tool_call_id='call-thanos-0'`, analysisID).Scan(&rogueCalls); err != nil || rogueCalls != 0 {
		t.Fatalf("rogue proposal must leave no tool call rows, got %d (err=%v)", rogueCalls, err)
	}
}
