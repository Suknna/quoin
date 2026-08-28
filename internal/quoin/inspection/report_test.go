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
	h.publishMixedPlan(t)
	h.seedBrowserIdentity(t, false)
	h.seedModelProvider(t)
	store, err := artifact.NewStore(h.db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.service.SetArtifactWriter(store.MaterializeEvidenceTransaction)
	ctx := context.Background()
	detail, err := h.service.CreateInspectionRun(ctx, h.principal, "cmd-1", "payments", "mixed-plan")
	if err != nil {
		t.Fatal(err)
	}
	attemptID := h.promqlAttemptID(t, detail.RunID)
	h.dispatchPromQL(t, attemptID)
	if err := h.service.CommitPromQLProposal(ctx, attemptID, "plinth-boot", 1, promqlSuccessProposal(attemptID, detail.RunID, "success", "instant")); err != nil {
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
	// run + 2 check results + 1 evidence + its readable artifact + config + contract.
	if itemCount != 7 {
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
	if err := h.service.CommitReportProposal(ctx, analysisID, "plinth-boot", 1, reportProposalBody(analysisID, detail.RunID, callID, "巡检报告正文", evidenceIDs, artifactIDs, promptDigest)); err != nil {
		t.Fatal(err)
	}
	final, err := h.service.GetRun(ctx, "payments", detail.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if final.ReportCount != 1 {
		t.Fatalf("run should expose one report version, got %d", final.ReportCount)
	}
	report, err := h.service.GetReport(ctx, detail.RunID, 1)
	if err != nil || report.Version != 1 || report.Content != "巡检报告正文" || report.ModelID != "fixture-chat-1" || len(report.EvidenceIDs) != 1 || report.EvidenceDigest == "" {
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
	if err := h.service.CommitReportProposal(ctx, analysisID, "plinth-boot", 1, reportProposalBody(analysisID, detail.RunID, callID, "巡检报告正文", evidenceIDs, artifactIDs, promptDigest)); err != nil {
		t.Fatalf("identical replay must be idempotent: %v", err)
	}
	if err := h.service.CommitReportProposal(ctx, analysisID, "plinth-boot", 1, reportProposalBody(analysisID, detail.RunID, callID, "篡改内容", evidenceIDs, artifactIDs, promptDigest)); err == nil {
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
