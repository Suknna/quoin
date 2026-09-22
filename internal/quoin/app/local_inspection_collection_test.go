package app

// Local inspection collection end-to-end (ADR-0011): an enabled metrics
// connection qualified through the LOCAL probe executor feeds a plan Run whose
// plugin collection child executes through the stubbed metrics_collect tool;
// the mapped inspection_plugin_result_v1 commits with Evidence and closes the
// Run, and the report analysis attempt stays Queued for the Plinth dispatch
// path (unchanged agent behaviour).

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/analysis"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/inspection"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	_ "modernc.org/sqlite"
)

func localInspectionAdminContext(t *testing.T) context.Context {
	t.Helper()
	correlationID, err := execution.NewCorrelationID()
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: correlationID,
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: 1},
		Session:       execution.SessionRef{ID: 1, AuthRevision: 1},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "test-req"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// newLocalInspectionFixture builds the full local collection stack: schema,
// admin session, a thanos connection enabled through the local probe executor,
// and one enabled promql_instant plan bound to it. The caller must install a
// tool stub covering BOTH metrics_probe and metrics_collect (see
// stubLocalCollectTool) before building the fixture, because the
// qualification probe itself executes locally.
func newLocalInspectionFixture(t *testing.T) (*sql.DB, *RuntimeService) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/local-collection.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	now := "2026-08-28T00:00:00Z"
	seed := strings.Join([]string{
		`INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at)
		 VALUES (1,'admin','Admin','admin',1,1,'$argon2id$fixture',1,'` + now + `','` + now + `')`,
		`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at)
		 VALUES (1,1,zeroblob(32),1,'local collection','` + now + `','` + now + `','2099-01-01T00:00:00Z','2099-01-01T00:00:00Z')`,
		`INSERT INTO root_key_state(id, binding_revision, verifier_nonce, verifier_ciphertext, bound_at) VALUES (1, 1, zeroblob(12), zeroblob(16), '` + now + `')`,
	}, "; ")
	if _, err := db.Exec(seed); err != nil {
		t.Fatal(err)
	}
	reader := fixtureReadOnlyPool(t, db)
	connections.ProbeContractSource = func() string { return string(gencontracts.ConnectionProbesYAML) }
	conns := connections.NewService(db, func() ([]byte, error) { return make([]byte, 32), nil })
	if err := conns.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	analyses := analysis.NewService(db)
	if err := analyses.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	inspections := inspection.NewService(db)
	if err := inspections.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	service := NewRuntimeControl(qruntime.NewService(), "test", conns, db)
	service.Analyses = analyses
	service.Inspections = inspections

	// The connection chain: create → local probe execution → enable with the
	// locally committed passed probe result.
	admin := localInspectionAdminContext(t)
	created, err := conns.Create(admin, connections.CreateInput{
		Name: "fixture-metrics", Type: connections.TypeThanos,
		NonSecretJSON: []byte(`{"type":"thanos","baseUrl":"https://metrics.fixture","authType":"none"}`),
	}, 1, "seed-local-metrics")
	if err != nil {
		t.Fatal(err)
	}
	probeAttempt, err := conns.StartProbe(admin, created.Name)
	if err != nil {
		t.Fatal(err)
	}
	service.runLocalExecutionPass(context.Background())
	var probeState string
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &probeState, probeAttempt)
	if probeState != "Succeeded" {
		t.Fatalf("local qualification probe is %s, want Succeeded", probeState)
	}
	var probeResultID int64
	mustQuery(t, db, `SELECT id FROM connection_probe_results WHERE attempt_id=?`, &probeResultID, probeAttempt)
	if _, err := conns.Enable(admin, created.Name, created.RowVersion, probeResultID, 1); err != nil {
		t.Fatalf("enable locally qualified connection: %v", err)
	}

	// One enabled instant plan on the qualified connection.
	planNow := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO inspection_plans(plan_key,display_name,enabled,connection_id,plugin_id,template_id,template_version,params_json,scope_json,scope_kind,cron,timezone,row_version,created_by,created_at,updated_at)
		VALUES('local-collection-plan','本地采集计划',1,1,'thanos','promql_instant','1','{"expression":"up"}','{"kind":"integration"}','integration',NULL,'UTC',1,1,?,?)`, planNow, planNow); err != nil {
		t.Fatal(err)
	}
	// The model provider seeds after the metrics connection so the plan's
	// hardcoded connection_id=1 keeps pointing at the metrics source.
	seedLocalModelProvider(t, db)
	return db, service
}

// stubLocalCollectTool replaces the internal tool lookup with a metrics_collect
// stub capturing the frozen request and returning one complete pass.
// seedLocalModelProvider plants one enabled, qualified model_provider
// connection so the report analysis attempt is created when the local
// collection closes the run (startReportAnalysisOn skips silently without
// one — the reconciler would retry later).
func seedLocalModelProvider(t *testing.T, db *sql.DB) {
	t.Helper()
	const now = "2026-08-28T00:00:00Z"
	connection, err := db.Exec(`INSERT INTO connections(name,type,enabled,created_at) VALUES('model','model_provider',0,?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	connectionID, _ := connection.LastInsertId()
	revision, err := db.Exec(`INSERT INTO connection_revisions(connection_id,revision_seq,config_json,created_at) VALUES(?,1,?,?)`, connectionID, `{"baseUrl":"https://provider.test","chatModelId":"fixture-chat-1","contextBudgetTokens":4096,"maxOutputTokens":1024}`, now)
	if err != nil {
		t.Fatal(err)
	}
	revisionID, _ := revision.LastInsertId()
	nonce := make([]byte, 12)
	for i := range nonce {
		nonce[i] = byte(7*31 + i)
	}
	generation, err := db.Exec(`INSERT INTO credential_generations(connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_at) VALUES(?,1,1,1,?,?,?)`, connectionID, nonce, []byte(strings.Repeat("f", 32)), now)
	if err != nil {
		t.Fatal(err)
	}
	generationID, _ := generation.LastInsertId()
	if _, err = db.Exec(`UPDATE connections SET current_revision_id=?, current_credential_generation_id=?, row_version=row_version+1 WHERE id=?`, revisionID, generationID, connectionID); err != nil {
		t.Fatal(err)
	}
	probeAttempt, err := db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,created_at) VALUES('connection_probe','connection',?,'Queued','test',?)`, connectionID, now)
	if err != nil {
		t.Fatal(err)
	}
	probeAttemptID, _ := probeAttempt.LastInsertId()
	probeSnapshot, err := db.Exec(`INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(?,'connection_probe_v1','connection-probe-v1',?,?)`, probeAttemptID, strings.Repeat("0", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	probeSnapshotID, _ := probeSnapshot.LastInsertId()
	if _, err = db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id) VALUES(?,1,'user',?,?)`, probeSnapshotID, strings.Repeat("0", 64), revisionID); err != nil {
		t.Fatal(err)
	}
	var probeChatGrantID, probeEmbeddingGrantID int64
	for _, purpose := range []string{"model_probe_chat", "model_probe_embedding"} {
		grant, grantErr := db.Exec(`INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at) VALUES(?,?,?,?,?,?)`, probeAttemptID, purpose, connectionID, revisionID, generationID, now)
		if grantErr != nil {
			t.Fatal(grantErr)
		}
		if purpose == "model_probe_chat" {
			probeChatGrantID, _ = grant.LastInsertId()
		} else {
			probeEmbeddingGrantID, _ = grant.LastInsertId()
		}
	}
	if _, err = db.Exec(`UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='boot',connection_epoch=1,lease_until=?,runtime_release_version='test',row_version=row_version+1 WHERE id=? AND state='Queued'`, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=? AND state='Assigned'`, now, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
	seedProbeModelCall(t, db, probeAttemptID, probeChatGrantID, 1, now, true)
	seedProbeModelCall(t, db, probeAttemptID, probeChatGrantID, 4, now, false)
	seedProbeEmbeddingCall(t, db, probeAttemptID, probeEmbeddingGrantID, probeSnapshotID, now)
	probe, err := db.Exec(`INSERT INTO connection_probe_results(attempt_id,connection_id,connection_type,connection_revision_id,credential_generation_id,root_binding_revision,action_set_id,action_set_version,probe_contract_digest,outcome,result_digest,started_at,finished_at,created_at) VALUES(?,?,?,?,?,1,'model-provider-capabilities',1,?,?,?,?,?,?)`,
		probeAttemptID, connectionID, "model_provider", revisionID, generationID, strings.Repeat("0", 64), "passed", strings.Repeat("1", 64), now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	probeResultID, _ := probe.LastInsertId()
	if _, err = db.Exec(`INSERT INTO model_provider_connection_probe_results(probe_result_id,chat_model_id,embedding_model_id,context_budget_tokens,max_output_tokens,streaming_supported,native_tool_calling_supported,multi_tool_call_supported,cancellation_observed,usage_observed,request_id_observed,embedding_supported,embedding_vector_dim,detail_json) VALUES(?,'fixture-chat-1','fixture-embed-1',4096,1024,1,1,1,1,1,1,1,16,'{}')`, probeResultID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO connection_enable_qualifications(connection_id,enabled_row_version,probe_result_id,created_by,created_at) VALUES(?,3,?,?,?)`, connectionID, probeResultID, 1, now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE connections SET enabled=1,revalidation_required=0,row_version=row_version+1 WHERE id=?`, connectionID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE execution_attempts SET state='Succeeded',ended_at=?,row_version=row_version+1 WHERE id=? AND state='Running'`, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
}

func stubLocalCollectTool(t *testing.T) *[]collectRequestJSON {
	t.Helper()
	captured := &[]collectRequestJSON{}
	previous := localMetricsToolEntry
	localMetricsToolEntry = func(name string) (plugins.ToolEntry, bool) {
		if name != "metrics_collect" && name != "metrics_probe" {
			return plugins.ToolEntry{}, false
		}
		return plugins.ToolEntry{Timeout: time.Second, Invoke: func(ctx context.Context, exec plugins.ToolExecution) (json.RawMessage, error) {
			if name == "metrics_probe" {
				return json.Marshal(map[string]any{
					"reachable": true, "latencyMs": 3, "kind": exec.Conn.Type, "query": "vector(1)",
					"responseType": "vector", "sampleCount": 1, "sampleValue": "1",
				})
			}
			var request collectRequestJSON
			if err := json.Unmarshal(exec.Arguments, &request); err != nil {
				return nil, err
			}
			*captured = append(*captured, request)
			return json.Marshal(plugins.CollectResult{Checks: []plugins.CheckObservation{{
				CheckID: request.TemplateID, Succeeded: true,
				EvidenceJSON: []byte(`{"result":{"resultType":"vector","result":[{"metric":{"job":"fixture"},"value":[1760000000,"1"]}]}}`),
			}}})
		}}, true
	}
	t.Cleanup(func() { localMetricsToolEntry = previous })
	return captured
}

func TestLocalInspectionCollectionExecutesPlanCheckAndClosesRun(t *testing.T) {
	requests := stubLocalCollectTool(t)
	db, service := newLocalInspectionFixture(t)
	detail, err := service.Inspections.CreatePlanRun(localInspectionAdminContext(t), 1, "local-collection-run", "local-collection-plan")
	if err != nil {
		t.Fatal(err)
	}
	service.runLocalExecutionPass(context.Background())

	var attemptID int64
	mustQuery(t, db, `SELECT id FROM execution_attempts WHERE scope_type='run_check' AND scope_id=?`, &attemptID, detail.RunID)
	var state string
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &state, attemptID)
	if state != "Succeeded" {
		t.Fatalf("local collection left attempt %d in %s, want Succeeded", attemptID, state)
	}
	var status, gapReason, metaJSON string
	mustQuery(t, db, `SELECT status FROM inspection_check_results WHERE attempt_id=?`, &status, attemptID)
	mustQuery(t, db, `SELECT COALESCE(gap_reason,'') FROM inspection_check_results WHERE attempt_id=?`, &gapReason, attemptID)
	mustQuery(t, db, `SELECT COALESCE(meta_json,'') FROM inspection_check_results WHERE attempt_id=?`, &metaJSON, attemptID)
	var errorsJSON string
	_ = db.QueryRow(`SELECT COALESCE(meta_json,'') FROM inspection_check_results WHERE attempt_id=?`, attemptID).Scan(&errorsJSON)
	rows, _ := db.Query(`SELECT errors_json FROM inspection_check_results WHERE attempt_id=?`, attemptID)
	if rows != nil {
		rows.Close()
	}
	t.Logf("DEBUG status=%s gap=%s meta=%s", status, gapReason, errorsJSON)
	if status != "ok" {
		t.Fatalf("local collection check status %q, want ok", status)
	}
	var evidenceJSON string
	mustQuery(t, db, `SELECT result_json FROM evidence WHERE attempt_id=?`, &evidenceJSON, attemptID)
	if !strings.Contains(evidenceJSON, `"resultType":"vector"`) {
		t.Fatalf("evidence body %q, want the frozen Prometheus projection", evidenceJSON)
	}
	run, err := service.Inspections.GetRun(context.Background(), detail.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "Completed" {
		t.Fatalf("run settled in %s, want Completed", run.State)
	}
	// The collect request carries the frozen template binding verbatim.
	if len(*requests) != 1 {
		t.Fatalf("collect invocations = %d, want 1", len(*requests))
	}
	request := (*requests)[0]
	if request.TemplateID != "promql_instant" || request.TemplateVersion != "1" || request.ScopeKind != "integration" {
		t.Fatalf("collect request = %+v, want the frozen promql_instant/1 integration binding", request)
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(paramsOf(t, request)), &params); err != nil {
		t.Fatal(err)
	}
	if params["expression"] != "up" {
		t.Fatalf("collect params = %v, want the frozen expression", params)
	}
	// The report analysis attempt stays Queued: agent attempts still dispatch
	// through the Plinth stream, never the local executor.
	var analysisState string
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE attempt_type='inspection_analysis' AND scope_id=?`, &analysisState, detail.RunID)
	if analysisState != "Queued" {
		t.Fatalf("report analysis is %s, want Queued for the Plinth dispatch path", analysisState)
	}
}

func paramsOf(t *testing.T, request collectRequestJSON) []byte {
	t.Helper()
	encoded, err := json.Marshal(request.Params)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// TestLocalInspectionCollectionRefusesFrozenGrantAfterDisable reproduces the
// P1 review finding: a collection whose grant froze while the connection was
// enabled must NOT execute once an admin disables the connection — the
// pre-execution guard transaction refuses the frozen grant before any tool
// dispatch, so no platform call (and no Stele material acquire) can happen.
func TestLocalInspectionCollectionRefusesFrozenGrantAfterDisable(t *testing.T) {
	requests := stubLocalCollectTool(t)
	db, service := newLocalInspectionFixture(t)
	admin := localInspectionAdminContext(t)
	detail, err := service.Inspections.CreatePlanRun(admin, 1, "disabled-guard-run", "local-collection-plan")
	if err != nil {
		t.Fatal(err)
	}
	// The run froze its child grants against the enabled connection; disable
	// lands afterwards, exactly like an admin reacting to an incident.
	if _, err := db.Exec(`INSERT INTO maintenance_state(id,active,row_version) VALUES(1,0,1)`); err != nil {
		t.Fatal(err)
	}
	var rowVersion int64
	mustQuery(t, db, `SELECT row_version FROM connections WHERE id=1`, &rowVersion)
	if _, err := service.Connections.Disable(admin, "fixture-metrics", rowVersion); err != nil {
		t.Fatalf("disable connection: %v", err)
	}
	service.runLocalExecutionPass(context.Background())

	var attemptID int64
	mustQuery(t, db, `SELECT id FROM execution_attempts WHERE scope_type='run_check' AND scope_id=?`, &attemptID, detail.RunID)
	var state string
	mustQuery(t, db, `SELECT state FROM execution_attempts WHERE id=?`, &state, attemptID)
	if state == "Queued" || state == "Running" {
		t.Fatalf("denied collection must converge instead of wedging %s (attempt %d)", state, attemptID)
	}
	// No tool dispatch happened: without an invoked metrics_collect there is no
	// platform call and no gateway material acquire to serve it.
	if len(*requests) != 0 {
		t.Fatalf("metrics_collect was dispatched %d times against a disabled connection", len(*requests))
	}
	// The refusal is recorded honestly on the frozen check as a query gap, and
	// no evidence was fabricated for the refused collection.
	var status, gapReason string
	var evidenceCount int
	mustQuery(t, db, `SELECT status FROM inspection_check_results WHERE attempt_id=?`, &status, attemptID)
	mustQuery(t, db, `SELECT COALESCE(gap_reason,'') FROM inspection_check_results WHERE attempt_id=?`, &gapReason, attemptID)
	mustQuery(t, db, `SELECT COUNT(*) FROM evidence WHERE attempt_id=?`, &evidenceCount, attemptID)
	if status != "gap" || gapReason != "query_failed" {
		t.Fatalf("check result = %s/%s, want gap/query_failed", status, gapReason)
	}
	if evidenceCount != 0 {
		t.Fatalf("refused collection must not fabricate evidence, found %d rows", evidenceCount)
	}
}

// seedProbeModelCall / seedProbeEmbeddingCall mirror the inspection report
// harness: the model-provider probe child trigger validates the qualification
// against real model calls.
func seedProbeModelCall(t *testing.T, db *sql.DB, attemptID, grantID, callSeq int64, now string, succeeded bool) {
	t.Helper()
	digest := strings.Repeat("1", 64)
	call, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,evicted_turn_count,status,started_at) VALUES(?,?,'0','chat','fixture-chat-1',?,'connection-probe-v1','probe-supervisor-v1',?,?,?,?,?,4096,1024,0,0,'running',?)`,
		attemptID, callSeq, grantID, digest, digest, digest, digest, digest, now)
	if err != nil {
		t.Fatal(err)
	}
	callID, _ := call.LastInsertId()
	if _, err = db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,1,'system',?,'system_contract'),(?,2,'system',?,'tool_schema')`, callID, digest, callID, digest); err != nil {
		t.Fatal(err)
	}
	if succeeded {
		if _, err = db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,1,'{"assistantText":"ok","finishReason":"stop","tool_calls":[]}',?, 'stop',?)`, callID, digest, now); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":1,"total_tokens":2}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err = db.Exec(`UPDATE model_calls SET status='cancelled',termination_reason='cancelled',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
		t.Fatal(err)
	}
}

func seedProbeEmbeddingCall(t *testing.T, db *sql.DB, attemptID, grantID, snapshotID int64, now string) {
	t.Helper()
	digest := strings.Repeat("1", 64)
	call, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,input_snapshot_digest,rendered_request_digest,estimated_input_tokens,status,started_at) VALUES(?,'6','0','embedding','fixture-embed-1',?,?,?,0,'running',?)`,
		attemptID, grantID, digest, digest, now)
	if err != nil {
		t.Fatal(err)
	}
	callID, _ := call.LastInsertId()
	if _, err = db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,attempt_input_snapshot_id) VALUES(?,1,'user',?,?)`, callID, digest, snapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,created_at) VALUES(?,1,'{}',?,?)`, callID, digest, now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":0,"total_tokens":1}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
		t.Fatal(err)
	}
}
