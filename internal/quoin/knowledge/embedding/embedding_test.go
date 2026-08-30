package embedding

// Deterministic T29 embedding projection tests over the real frozen schema:
// generation lifecycle (drift, atomic switch), attempt creation fencing,
// closed result adjudication (digest/dimension/late results), and the
// pending/failed projection semantics (DATA-EMBED-001/002, DATA-TX-012).

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

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	_ "modernc.org/sqlite"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/test.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	return db
}

func testNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }

type harness struct {
	db              *sql.DB
	service         *Service
	userID          int64
	connectionID    int64
	revisionID      int64
	credentialGenID int64
	probeResultID   int64
}

// newHarness seeds one enabled qualified embedding provider (fixture-embed-1,
// validated dimension 16) exactly as the acceptance stack provisions it.
func newHarness(t *testing.T) *harness {
	t.Helper()
	db := newTestDB(t)
	now := testNow()
	user, err := db.Exec(`INSERT INTO users(username,display_name,role,enabled,password_phc,auth_revision,created_at,updated_at) VALUES('op','Op','operator',1,'x',1,?,?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	userID, _ := user.LastInsertId()
	if _, err := db.Exec(`INSERT OR IGNORE INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`, []byte(strings.Repeat("e", 12)), []byte(strings.Repeat("f", 16)), now); err != nil {
		t.Fatal(err)
	}
	connection, err := db.Exec(`INSERT INTO connections(name,type,enabled,created_at) VALUES('t29-provider','model_provider',0,?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	connectionID, _ := connection.LastInsertId()
	revision, err := db.Exec(`INSERT INTO connection_revisions(connection_id,revision_seq,config_json,created_at) VALUES(?,1,?,?)`, connectionID, `{"baseUrl":"https://provider.test","chatModelId":"fixture-chat-1","embeddingModelId":"fixture-embed-1","contextBudgetTokens":4096,"maxOutputTokens":1024}`, now)
	if err != nil {
		t.Fatal(err)
	}
	revisionID, _ := revision.LastInsertId()
	nonce := make([]byte, 12)
	for index := range nonce {
		nonce[index] = byte(index * 11)
	}
	generation, err := db.Exec(`INSERT INTO credential_generations(connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_at) VALUES(?,1,1,1,?,?,?)`, connectionID, nonce, []byte(strings.Repeat("f", 32)), now)
	if err != nil {
		t.Fatal(err)
	}
	credentialGenID, _ := generation.LastInsertId()
	if _, err := db.Exec(`UPDATE connections SET current_revision_id=?, current_credential_generation_id=?, row_version=row_version+1 WHERE id=?`, revisionID, credentialGenID, connectionID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO runtime_slots(slot,state,created_at) VALUES('plinth','unregistered',?)`, now); err != nil {
		t.Fatal(err)
	}
	runtimeCredential, err := db.Exec(`INSERT INTO runtime_credentials(slot,generation,token_digest,created_at,confirmed_at) VALUES('plinth',1,?,?,?)`, []byte(strings.Repeat("0", 32)), now, now)
	if err != nil {
		t.Fatal(err)
	}
	runtimeCredentialID, _ := runtimeCredential.LastInsertId()
	if _, err := db.Exec(`UPDATE runtime_slots SET state='registered',current_credential_id=?,row_version=row_version+1 WHERE slot='plinth'`, runtimeCredentialID); err != nil {
		t.Fatal(err)
	}
	// Probe attempt + closed passed result with embedding capability.
	probeAttempt, err := db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,created_at) VALUES('connection_probe','connection',?,'Queued','test',?)`, connectionID, now)
	if err != nil {
		t.Fatal(err)
	}
	probeAttemptID, _ := probeAttempt.LastInsertId()
	snapshot, err := db.Exec(`INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(?,'connection_probe_v1','connection-probe-v1',?,?)`, probeAttemptID, strings.Repeat("0", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, _ := snapshot.LastInsertId()
	if _, err := db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id) VALUES(?,1,'user',?,?)`, snapshotID, strings.Repeat("0", 64), revisionID); err != nil {
		t.Fatal(err)
	}
	var probeChatGrantID, probeEmbedGrantID int64
	for _, purpose := range []string{"model_probe_chat", "model_probe_embedding"} {
		grant, err := db.Exec(`INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at) VALUES(?,?,?,?,?,?)`, probeAttemptID, purpose, connectionID, revisionID, credentialGenID, now)
		if err != nil {
			t.Fatal(err)
		}
		if purpose == "model_probe_chat" {
			probeChatGrantID, _ = grant.LastInsertId()
		} else {
			probeEmbedGrantID, _ = grant.LastInsertId()
		}
	}
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='boot',connection_epoch=1,lease_until=?,runtime_release_version='test',row_version=row_version+1 WHERE id=?`, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=?`, now, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
	var probeResultID int64
	probe, err := db.Exec(`INSERT INTO connection_probe_results(attempt_id,connection_id,connection_type,connection_revision_id,credential_generation_id,root_binding_revision,action_set_id,action_set_version,probe_contract_digest,outcome,result_digest,started_at,finished_at,created_at) VALUES(?,?,?,?,?,1,'model-provider-capabilities',1,?,?,?,?,?,?)`,
		probeAttemptID, connectionID, "model_provider", revisionID, credentialGenID, strings.Repeat("0", 64), "passed", strings.Repeat("1", 64), now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	probeResultID, _ = probe.LastInsertId()
	seedProbeModelCalls(t, db, probeAttemptID, probeChatGrantID, probeEmbedGrantID, snapshotID, now)
	if _, err := db.Exec(`INSERT INTO model_provider_connection_probe_results(probe_result_id,chat_model_id,embedding_model_id,context_budget_tokens,max_output_tokens,streaming_supported,native_tool_calling_supported,multi_tool_call_supported,cancellation_observed,usage_observed,request_id_observed,embedding_supported,embedding_vector_dim,detail_json) VALUES(?,'fixture-chat-1','fixture-embed-1',4096,1024,1,1,1,1,1,1,1,16,'{}')`, probeResultID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Succeeded',ended_at=?,row_version=row_version+1 WHERE id=?`, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO connection_enable_qualifications(connection_id,enabled_row_version,probe_result_id,created_by,created_at) VALUES(?,3,?,?,?)`, connectionID, probeResultID, userID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE connections SET enabled=1,revalidation_required=0,row_version=row_version+1 WHERE id=? AND row_version=2`, connectionID); err != nil {
		t.Fatal(err)
	}
	return &harness{db: db, service: NewService(db), userID: userID, connectionID: connectionID, revisionID: revisionID, credentialGenID: credentialGenID, probeResultID: probeResultID}
}

func seedProbeModelCalls(t *testing.T, db *sql.DB, attemptID, chatGrantID, embedGrantID, snapshotID int64, now string) {
	t.Helper()
	digest := strings.Repeat("1", 64)
	call, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,status,started_at) VALUES(?,1,'0','chat','fixture-chat-1',?,'connection-probe-v1','probe-supervisor-v1',?,?,?,?,?,4096,1024,0,'running',?)`,
		attemptID, chatGrantID, digest, digest, digest, digest, digest, now)
	if err != nil {
		t.Fatal(err)
	}
	callID, _ := call.LastInsertId()
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,1,'system',?,'system_contract'),(?,2,'system',?,'tool_schema')`, callID, digest, callID, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,1,'{"assistantText":"ok","finishReason":"stop","tool_calls":[]}',?,'stop',?)`, callID, digest, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":1,"total_tokens":2}',status='succeeded',ended_at=? WHERE id=?`, now, callID); err != nil {
		t.Fatal(err)
	}
	// The frozen action set also observes cancellation: one cancelled chat
	// call must exist before the typed child row can close.
	cancelled, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,status,started_at) VALUES(?,2,'0','chat','fixture-chat-1',?,'connection-probe-v1','probe-supervisor-v1',?,?,?,?,?,4096,1024,0,'running',?)`,
		attemptID, chatGrantID, digest, digest, digest, digest, digest, now)
	if err != nil {
		t.Fatal(err)
	}
	cancelledID, _ := cancelled.LastInsertId()
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,1,'system',?,'system_contract'),(?,2,'system',?,'tool_schema')`, cancelledID, digest, cancelledID, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE model_calls SET status='cancelled',termination_reason='cancelled',ended_at=? WHERE id=?`, now, cancelledID); err != nil {
		t.Fatal(err)
	}
	embed, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,input_snapshot_digest,rendered_request_digest,estimated_input_tokens,status,started_at) VALUES(?,3,'0','embedding','fixture-embed-1',?,?,?,0,'running',?)`,
		attemptID, embedGrantID, digest, digest, now)
	if err != nil {
		t.Fatal(err)
	}
	embedID, _ := embed.LastInsertId()
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,attempt_input_snapshot_id) VALUES(?,1,'user',?,?)`, embedID, digest, snapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,created_at) VALUES(?,1,'{}',?,?)`, embedID, digest, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":2,"total_tokens":2}',status='succeeded',ended_at=? WHERE id=?`, now, embedID); err != nil {
		t.Fatal(err)
	}
}

// confirmKnowledge inserts one eligible confirmed knowledge version with its
// retrieval state and search-doc projection row (the confirm path's frozen
// end state). Each call binds the candidate to a fresh immutable analysis
// output so the single-source uniqueness holds.
func (harness *harness) confirmKnowledge(t *testing.T, title, body string) (knowledgeID, versionID int64) {
	t.Helper()
	now := testNow()
	sourceID := harness.seedAnalysisOutput(t)
	knowledge, err := harness.db.Exec(`INSERT INTO reusable_knowledge(created_by,created_at) VALUES(?,?)`, harness.userID, now)
	if err != nil {
		t.Fatal(err)
	}
	knowledgeID, _ = knowledge.LastInsertId()
	candidate, err := harness.db.Exec(`INSERT INTO knowledge_candidates(source_type,source_id,state,original_suggestion_json,draft_title,draft_body,created_by,created_at) VALUES('initial_analysis_output',?,'AwaitingConfirmation','{}',?,?,?,?)`, sourceID, title, body, harness.userID, now)
	if err != nil {
		t.Fatal(err)
	}
	candidateID, _ := candidate.LastInsertId()
	// The confirm order is frozen: the candidate first becomes Confirmed and
	// bound to its knowledge, then the immutable version is appended.
	if _, err := harness.db.Exec(`UPDATE knowledge_candidates SET state='Confirmed',confirmed_knowledge_id=?,row_version=row_version+1 WHERE id=?`, knowledgeID, candidateID); err != nil {
		t.Fatal(err)
	}
	version, err := harness.db.Exec(`INSERT INTO knowledge_versions(knowledge_id,version_seq,title,body,source_candidate_id,created_by,created_at) VALUES(?,1,?,?,?,?,?)`, knowledgeID, title, body, candidateID, harness.userID, now)
	if err != nil {
		t.Fatal(err)
	}
	versionID, _ = version.LastInsertId()
	if _, err := harness.db.Exec(`UPDATE reusable_knowledge SET current_version_id=?,row_version=row_version+1 WHERE id=?`, versionID, knowledgeID); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.db.Exec(`INSERT INTO knowledge_version_retrieval_state(knowledge_version_id,updated_at) VALUES(?,?)`, versionID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.db.Exec(`INSERT INTO knowledge_search_docs(knowledge_version_id,title,body) VALUES(?,?,?)`, versionID, title, body); err != nil {
		t.Fatal(err)
	}
	return knowledgeID, versionID
}

// seedAnalysisOutput inserts one immutable initial analysis output.
func (harness *harness) seedAnalysisOutput(t *testing.T) int64 {
	t.Helper()
	now := testNow()
	source, err := harness.db.Exec(`INSERT INTO alert_sources(source_key,protocol,enabled,created_at) VALUES(?,'alertmanager',1,?)`, fmt.Sprintf("emb-src-%d", time.Now().UnixNano()), now)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, _ := source.LastInsertId()
	labels := `{"alertname":"EmbProbe","severity":"warning"}`
	occurrence, err := harness.db.Exec(`INSERT INTO alert_occurrences(source_id,fingerprint,starts_at,state,labels_canonical,labels_digest,first_seen_at,last_state_change_at) VALUES(?,x'0000000000000003',?,'Firing',?,?,?,?)`,
		sourceID, now, labels, digestHex(labels), now, now)
	if err != nil {
		t.Fatal(err)
	}
	occurrenceID, _ := occurrence.LastInsertId()
	analysis, err := harness.db.Exec(`INSERT INTO initial_analyses(occurrence_id,state,input_snapshot_digest,created_at) VALUES(?,'Queued',?,?)`, occurrenceID, strings.Repeat("5", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	analysisID, _ := analysis.LastInsertId()
	attemptID := harness.seedAnalysisAttempt(t, analysisID)
	output, err := harness.db.Exec(`INSERT INTO initial_analysis_outputs(analysis_id,attempt_id,model_id,content,created_at) VALUES(?,?,'fixture-chat',?,?)`, analysisID, attemptID, "content", now)
	if err != nil {
		t.Fatal(err)
	}
	outputID, _ := output.LastInsertId()
	// Close the source attempt without the Succeeded closure ladder: the
	// knowledge source trigger only requires the immutable output.
	if _, err := harness.db.Exec(`UPDATE execution_attempts SET state='Interrupted',ended_at=?,termination_reason='lease_expired',row_version=row_version+1 WHERE id=?`, now, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.db.Exec(`UPDATE initial_analyses SET state='Failed',row_version=row_version+1 WHERE id=?`, analysisID); err != nil {
		t.Fatal(err)
	}
	return outputID
}

// seedAnalysisAttempt walks one initial-analysis attempt to Succeeded so its
// output binds a real closed attempt.
func (harness *harness) seedAnalysisAttempt(t *testing.T, analysisID int64) int64 {
	t.Helper()
	now := testNow()
	insert, err := harness.db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at) VALUES('initial_analysis','analysis',?,'Queued','test','agent-v1',?)`, analysisID, now)
	if err != nil {
		t.Fatal(err)
	}
	attemptID, _ := insert.LastInsertId()
	snapshot, err := harness.db.Exec(`INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(?,'initial_analysis_v1','fixture-renderer',?,?)`, attemptID, strings.Repeat("6", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, _ := snapshot.LastInsertId()
	if _, err := harness.db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,initial_analysis_id) VALUES(?,1,'user',?,?)`, snapshotID, strings.Repeat("6", 64), analysisID); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.db.Exec(`INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at) VALUES(?,'chat_model',?,?,?,?,?)`,
		attemptID, harness.connectionID, harness.revisionID, harness.credentialGenID, harness.probeResultID, now); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339Nano)
	if _, err := harness.db.Exec(`UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='boot',connection_epoch=1,lease_until=?,runtime_release_version='test',row_version=row_version+1 WHERE id=?`, future, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.db.Exec(`UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=?`, now, now, attemptID); err != nil {
		t.Fatal(err)
	}
	return attemptID
}

func digestHex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (harness *harness) sweepOK(t *testing.T) {
	t.Helper()
	if err := harness.service.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
}

func (harness *harness) queryInt(t *testing.T, sql string, args ...any) int64 {
	t.Helper()
	var value int64
	if err := harness.db.QueryRow(sql, args...).Scan(&value); err != nil {
		t.Fatalf("query %s: %v", sql, err)
	}
	return value
}

// walkEmbeddingAttemptToRunning binds and accepts the queued rebuild attempt.
func (harness *harness) walkEmbeddingAttemptToRunning(t *testing.T, attemptID int64) {
	t.Helper()
	future := time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339Nano)
	if _, err := harness.db.Exec(`UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='boot',connection_epoch=1,lease_until=?,runtime_release_version='test',row_version=row_version+1 WHERE id=? AND state='Queued'`, future, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.db.Exec(`UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=? AND state='Assigned'`, testNow(), testNow(), attemptID); err != nil {
		t.Fatal(err)
	}
}

// activeOrLastEmbeddingAttempt returns the active rebuild attempt, falling
// back to the most recent terminal one for replay adjudication tests.
func (harness *harness) activeOrLastEmbeddingAttempt(t *testing.T) int64 {
	t.Helper()
	var attemptID int64
	err := harness.db.QueryRow(`SELECT id FROM execution_attempts WHERE attempt_type='embedding' AND state IN ('Queued','Assigned','Running') ORDER BY id LIMIT 1`).Scan(&attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		if err := harness.db.QueryRow(`SELECT id FROM execution_attempts WHERE attempt_type='embedding' ORDER BY id DESC LIMIT 1`).Scan(&attemptID); err != nil {
			t.Fatal(err)
		}
		return attemptID
	}
	if err != nil {
		t.Fatal(err)
	}
	return attemptID
}

// commitFirstRebuild commits a matching (optionally mutated) result for the
// given attempt through the real adjudication path.
func (harness *harness) commitFirstRebuild(t *testing.T, dimension int, mutate func(outcome *GenerationResult)) error {
	t.Helper()
	ctx := context.Background()
	attemptID := harness.activeOrLastEmbeddingAttempt(t)
	var attemptState string
	if err := harness.db.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&attemptState); err != nil {
		t.Fatal(err)
	}
	if attemptState == "Queued" {
		harness.walkEmbeddingAttemptToRunning(t, attemptID)
	}
	input, err := harness.service.RebuildInput(ctx, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	var frozen Input
	if err := json.Unmarshal(input, &frozen); err != nil {
		t.Fatal(err)
	}
	outcome := GenerationResult{
		SchemaKind: ResultSchemaKind, AttemptID: attemptID, GenerationID: frozen.GenerationID,
		ModelID: frozen.ModelContract.EmbeddingModelID,
	}
	for _, item := range frozen.Inputs {
		outcome.Vectors = append(outcome.Vectors, ResultVector{VersionID: item.VersionID, Digest: item.Digest, Values: unitVector(dimension, int(item.VersionID))})
	}
	if mutate != nil {
		mutate(&outcome)
	}
	raw, err := json.Marshal(outcome)
	if err != nil {
		t.Fatal(err)
	}
	return harness.service.CommitResult(ctx, attemptID, "boot", 1, raw)
}

func unitVector(dimension, seed int) []float32 {
	vector := make([]float32, dimension)
	vector[seed%dimension] = 1
	return vector
}

func TestSweepCreatesGenerationPendingRowsAndRebuildAttempt(t *testing.T) {
	harness := newHarness(t)
	_, versionA := harness.confirmKnowledge(t, "连接池治理", "数据库连接池耗尽导致超时。")
	_, versionB := harness.confirmKnowledge(t, "磁盘压力", "节点磁盘压力触发驱逐。")
	harness.sweepOK(t)

	generationID := harness.queryInt(t, `SELECT id FROM embedding_generations WHERE state='building'`)
	if generationID == 0 {
		t.Fatal("sweep did not create a building generation")
	}
	var modelName string
	var dim int64
	if err := harness.db.QueryRow(`SELECT model_name, COALESCE(vector_dim,0) FROM embedding_generations WHERE id=?`, generationID).Scan(&modelName, &dim); err != nil {
		t.Fatal(err)
	}
	if modelName != "fixture-embed-1" || dim != 16 {
		t.Fatalf("generation identity = %s/%d, want fixture-embed-1/16", modelName, dim)
	}
	if pending := harness.queryInt(t, `SELECT COUNT(*) FROM embeddings WHERE embedding_generation_id=? AND state='pending'`, generationID); pending != 2 {
		t.Fatalf("pending rows = %d, want 2", pending)
	}
	attemptID := harness.queryInt(t, `SELECT id FROM execution_attempts WHERE attempt_type='embedding' AND state='Queued'`)
	if attemptID == 0 {
		t.Fatal("sweep did not create a rebuild attempt")
	}
	if grants := harness.queryInt(t, `SELECT COUNT(*) FROM attempt_connection_grants WHERE attempt_id=? AND purpose='embedding' AND qualified_probe_result_id=?`, attemptID, harness.probeResultID); grants != 1 {
		t.Fatalf("embedding grants = %d, want 1 frozen over the qualified probe result", grants)
	}
	var schemaKind string
	if err := harness.db.QueryRow(`SELECT schema_kind FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&schemaKind); err != nil || schemaKind != InputSchemaKind {
		t.Fatalf("input schema kind = %s err=%v, want embedding_v1", schemaKind, err)
	}
	if items := harness.queryInt(t, `SELECT COUNT(*) FROM attempt_input_items i JOIN attempt_input_snapshots s ON s.id=i.snapshot_id WHERE s.attempt_id=? AND i.knowledge_version_id IN (?,?)`, attemptID, versionA, versionB); items != 2 {
		t.Fatalf("lineage items = %d, want both eligible versions", items)
	}
	// Idempotency: a second sweep creates nothing new.
	harness.sweepOK(t)
	if generations := harness.queryInt(t, `SELECT COUNT(*) FROM embedding_generations`); generations != 1 {
		t.Fatalf("generations = %d after re-sweep, want 1", generations)
	}
	if attempts := harness.queryInt(t, `SELECT COUNT(*) FROM execution_attempts WHERE attempt_type='embedding'`); attempts != 1 {
		t.Fatalf("embedding attempts = %d after re-sweep, want 1", attempts)
	}
	// Exited versions never enter the projection.
	if _, err := harness.db.Exec(`UPDATE knowledge_version_retrieval_state SET exited=1,exited_at=?,exit_reason='stopped',updated_at=?,row_version=row_version+1 WHERE knowledge_version_id=? AND exited=0`, testNow(), testNow(), versionB); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.db.Exec(`DELETE FROM knowledge_search_docs WHERE knowledge_version_id=?`, versionB); err != nil {
		t.Fatal(err)
	}
	if err := harness.service.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCommitResultValidatesAndSwitchesGeneration(t *testing.T) {
	harness := newHarness(t)
	_, versionA := harness.confirmKnowledge(t, "连接池治理", "数据库连接池耗尽导致超时。")
	harness.sweepOK(t)
	if err := harness.commitFirstRebuild(t, 16, nil); err != nil {
		t.Fatalf("commit: %v", err)
	}
	generationID := harness.queryInt(t, `SELECT id FROM embedding_generations WHERE state='current'`)
	if generationID == 0 {
		t.Fatal("settled generation did not switch to current")
	}
	if length := harness.queryInt(t, `SELECT length(vector) FROM embeddings WHERE knowledge_version_id=? AND embedding_generation_id=?`, versionA, generationID); length != 16*4 {
		t.Fatalf("stored vector byte length = %d, want %d", length, 16*4)
	}
	if state := harness.queryInt(t, `SELECT 1 FROM embeddings WHERE knowledge_version_id=? AND state='ready'`, versionA); state != 1 {
		t.Fatal("embedding row not ready after commit")
	}
	if built, validated := harness.queryInt(t, `SELECT 1 FROM embedding_generations WHERE built_at IS NOT NULL`), harness.queryInt(t, `SELECT 1 FROM embedding_generations WHERE validated_at IS NOT NULL`); built != 1 || validated != 1 {
		t.Fatal("switched generation lacks built_at/validated_at")
	}
	// The projection stays a projection: deleting it changes no authority row.
	if docs := harness.queryInt(t, `SELECT COUNT(*) FROM knowledge_search_docs WHERE knowledge_version_id=?`, versionA); docs != 1 {
		t.Fatalf("authority doc count = %d, want 1", docs)
	}
}

func TestCommitResultRejectsWrongDimensionDigestAndLateResults(t *testing.T) {
	harness := newHarness(t)
	// Two lineage versions so same-length duplicate binding is observable.
	harness.confirmKnowledge(t, "连接池治理", "数据库连接池耗尽导致超时。")
	harness.confirmKnowledge(t, "磁盘压力", "节点磁盘压力触发驱逐。")
	harness.sweepOK(t)

	if err := harness.commitFirstRebuild(t, 8, nil); err == nil || !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("dimension drift accepted: %v", err)
	}
	if err := harness.commitFirstRebuild(t, 16, func(outcome *GenerationResult) {
		outcome.Vectors[0].Digest = strings.Repeat("a", 64)
	}); err == nil || !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("digest mismatch accepted: %v", err)
	}
	if err := harness.commitFirstRebuild(t, 16, func(outcome *GenerationResult) {
		outcome.Vectors[0].VersionID = 99999
	}); err == nil || !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("unknown lineage version accepted: %v", err)
	}
	// After a successful commit, replaying a divergent payload is late.
	if err := harness.commitFirstRebuild(t, 16, nil); err != nil {
		t.Fatalf("valid commit rejected: %v", err)
	}
	if err := harness.commitFirstRebuild(t, 16, func(outcome *GenerationResult) {
		outcome.Vectors[0].Values[0] = 0.5
	}); err == nil {
		t.Fatal("post-seal divergent replay accepted")
	}
	// A sealed replay carrying only a subset of the frozen lineage must not
	// slip past the seal either.
	if err := harness.commitFirstRebuild(t, 16, func(outcome *GenerationResult) {
		outcome.Vectors = outcome.Vectors[:len(outcome.Vectors)-1]
	}); err == nil || !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("post-seal subset replay accepted: %v", err)
	}
	// A same-length envelope that duplicates one version while dropping the
	// other must be rejected before any state branch.
	if err := harness.commitFirstRebuild(t, 16, func(outcome *GenerationResult) {
		outcome.Vectors[1] = outcome.Vectors[0]
	}); err == nil || !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("same-length duplicate-version envelope accepted: %v", err)
	}
	// The payload's model identity must match the frozen generation.
	if err := harness.commitFirstRebuild(t, 16, func(outcome *GenerationResult) {
		outcome.ModelID = "other-model"
	}); err == nil || !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("wrong-model envelope accepted: %v", err)
	}
}

func TestFailedAttemptMarksRowsFailedWithoutRevokingKnowledge(t *testing.T) {
	harness := newHarness(t)
	_, versionA := harness.confirmKnowledge(t, "连接池治理", "数据库连接池耗尽导致超时。")
	harness.sweepOK(t)
	attemptID := harness.queryInt(t, `SELECT id FROM execution_attempts WHERE attempt_type='embedding' AND state='Queued'`)
	harness.walkEmbeddingAttemptToRunning(t, attemptID)
	if err := harness.service.Fail(context.Background(), attemptID, "boot", 1, "provider_unavailable"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if state := harness.queryInt(t, `SELECT 1 FROM embeddings WHERE knowledge_version_id=? AND state='failed'`, versionA); state != 1 {
		t.Fatal("failed attempt did not mark its batch rows failed")
	}
	if docs := harness.queryInt(t, `SELECT COUNT(*) FROM knowledge_search_docs WHERE knowledge_version_id=?`, versionA); docs != 1 {
		t.Fatal("failed embedding revoked the knowledge FTS projection")
	}
	// An interrupt (runtime loss) keeps rows re-attemptable instead.
	_, versionB := harness.confirmKnowledge(t, "磁盘压力", "节点磁盘压力触发驱逐。")
	if err := harness.service.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	next := harness.queryInt(t, `SELECT id FROM execution_attempts WHERE attempt_type='embedding' AND state='Queued'`)
	harness.walkEmbeddingAttemptToRunning(t, next)
	if err := harness.service.Interrupt(context.Background(), next, "boot", 1, "lease_expired"); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if state := harness.queryInt(t, `SELECT 1 FROM embeddings WHERE knowledge_version_id=? AND state='pending'`, versionB); state != 1 {
		t.Fatal("interrupt converted pending rows to failed")
	}
}

func TestModelSwitchBuildsNewGenerationAndRetiresOld(t *testing.T) {
	harness := newHarness(t)
	_, versionA := harness.confirmKnowledge(t, "连接池治理", "数据库连接池耗尽导致超时。")
	harness.sweepOK(t)
	if err := harness.commitFirstRebuild(t, 16, nil); err != nil {
		t.Fatalf("first generation commit: %v", err)
	}
	firstGeneration := harness.queryInt(t, `SELECT id FROM embedding_generations WHERE state='current'`)

	// The admin switches the embedding model through a new revision and a
	// fresh qualified probe with a different validated dimension.
	now := testNow()
	revision, err := harness.db.Exec(`INSERT INTO connection_revisions(connection_id,revision_seq,config_json,created_at) VALUES(?,2,?,?)`, harness.connectionID, `{"baseUrl":"https://provider.test","chatModelId":"fixture-chat-1","embeddingModelId":"fixture-embed-wide","contextBudgetTokens":4096,"maxOutputTokens":1024}`, now)
	if err != nil {
		t.Fatal(err)
	}
	wideRevisionID, _ := revision.LastInsertId()
	probeAttempt, err := harness.db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,created_at) VALUES('connection_probe','connection',?,'Queued','test',?)`, harness.connectionID, now)
	if err != nil {
		t.Fatal(err)
	}
	wideProbeAttemptID, _ := probeAttempt.LastInsertId()
	wideSnapshot, err := harness.db.Exec(`INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(?,'connection_probe_v1','connection-probe-v1',?,?)`, wideProbeAttemptID, strings.Repeat("0", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	wideSnapshotID, _ := wideSnapshot.LastInsertId()
	if _, err := harness.db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id) VALUES(?,1,'user',?,?)`, wideSnapshotID, strings.Repeat("0", 64), wideRevisionID); err != nil {
		t.Fatal(err)
	}
	// The real admin sequence for a model switch on an enabled provider:
	// disable, point at the new revision, qualify, re-enable.
	if _, err := harness.db.Exec(`UPDATE connections SET enabled=0,row_version=row_version+1 WHERE id=?`, harness.connectionID); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.db.Exec(`UPDATE connections SET current_revision_id=?,row_version=row_version+1 WHERE id=?`, wideRevisionID, harness.connectionID); err != nil {
		t.Fatal(err)
	}
	var wideChatGrantID, wideEmbedGrantID int64
	for _, purpose := range []string{"model_probe_chat", "model_probe_embedding"} {
		grant, grantErr := harness.db.Exec(`INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at) VALUES(?,?,?,?,?,?)`, wideProbeAttemptID, purpose, harness.connectionID, wideRevisionID, harness.credentialGenID, now)
		if grantErr != nil {
			t.Fatal(grantErr)
		}
		if purpose == "model_probe_chat" {
			wideChatGrantID, _ = grant.LastInsertId()
		} else {
			wideEmbedGrantID, _ = grant.LastInsertId()
		}
	}
	future := time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339Nano)
	if _, err := harness.db.Exec(`UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='boot',connection_epoch=1,lease_until=?,runtime_release_version='test',row_version=row_version+1 WHERE id=?`, future, wideProbeAttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.db.Exec(`UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=?`, now, now, wideProbeAttemptID); err != nil {
		t.Fatal(err)
	}
	seedProbeModelCalls(t, harness.db, wideProbeAttemptID, wideChatGrantID, wideEmbedGrantID, wideSnapshotID, now)
	wideProbe, err := harness.db.Exec(`INSERT INTO connection_probe_results(attempt_id,connection_id,connection_type,connection_revision_id,credential_generation_id,root_binding_revision,action_set_id,action_set_version,probe_contract_digest,outcome,result_digest,started_at,finished_at,created_at) VALUES(?,?,?,?,?,1,'model-provider-capabilities',1,?,?,?,?,?,?)`,
		wideProbeAttemptID, harness.connectionID, "model_provider", wideRevisionID, harness.credentialGenID, strings.Repeat("0", 64), "passed", strings.Repeat("1", 64), now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	wideProbeResultID, _ := wideProbe.LastInsertId()
	if _, err := harness.db.Exec(`INSERT INTO model_provider_connection_probe_results(probe_result_id,chat_model_id,embedding_model_id,context_budget_tokens,max_output_tokens,streaming_supported,native_tool_calling_supported,multi_tool_call_supported,cancellation_observed,usage_observed,request_id_observed,embedding_supported,embedding_vector_dim,detail_json) VALUES(?,'fixture-chat-1','fixture-embed-wide',4096,1024,1,1,1,1,1,1,1,32,'{}')`, wideProbeResultID); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.db.Exec(`UPDATE execution_attempts SET state='Succeeded',ended_at=?,row_version=row_version+1 WHERE id=?`, now, wideProbeAttemptID); err != nil {
		t.Fatal(err)
	}
	var connectionRowVersion int64
	if err := harness.db.QueryRow(`SELECT row_version FROM connections WHERE id=?`, harness.connectionID).Scan(&connectionRowVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.db.Exec(`INSERT INTO connection_enable_qualifications(connection_id,enabled_row_version,probe_result_id,created_by,created_at) VALUES(?,?,?,?,?)`, harness.connectionID, connectionRowVersion+1, wideProbeResultID, harness.userID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.db.Exec(`UPDATE connections SET enabled=1,revalidation_required=0,row_version=row_version+1 WHERE id=? AND row_version=?`, harness.connectionID, connectionRowVersion); err != nil {
		t.Fatal(err)
	}

	// Drift sweep: a new building generation for the wide model appears and
	// the old current keeps serving until the new one settles.
	harness.sweepOK(t)
	building := harness.queryInt(t, `SELECT id FROM embedding_generations WHERE state='building' AND model_name='fixture-embed-wide' AND vector_dim=32`)
	if building == 0 {
		t.Fatal("drift sweep did not open a 32-dimension building generation")
	}
	if state := harness.queryInt(t, `SELECT 1 FROM embedding_generations WHERE id=? AND state='current'`, firstGeneration); state != 1 {
		t.Fatal("old generation stopped serving before the replacement settled")
	}
	// The wide rebuild must embed at 32 dimensions; a 16-dimension payload is
	// a closed-contract violation.
	if err := harness.commitFirstRebuild(t, 16, nil); err == nil || !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("old-dimension payload accepted for the wide generation: %v", err)
	}
	// The failed attempt released the scope; retry at the right dimension.
	if err := harness.commitFirstRebuild(t, 32, nil); err != nil {
		t.Fatalf("wide generation commit: %v", err)
	}
	if state := harness.queryInt(t, `SELECT 1 FROM embedding_generations WHERE id=? AND state='current'`, building); state != 1 {
		t.Fatal("wide generation did not atomically become current")
	}
	if state := harness.queryInt(t, `SELECT 1 FROM embedding_generations WHERE id=? AND state='retired'`, firstGeneration); state != 1 {
		t.Fatal("previous generation not retired by the atomic switch")
	}
	if length := harness.queryInt(t, `SELECT length(vector) FROM embeddings WHERE embedding_generation_id=? AND knowledge_version_id=?`, building, versionA); length != 32*4 {
		t.Fatalf("wide vector byte length = %d, want %d", length, 32*4)
	}
}

func TestLateResultWhenVersionLosesEligibilityMidFlight(t *testing.T) {
	harness := newHarness(t)
	_, versionA := harness.confirmKnowledge(t, "连接池治理", "数据库连接池耗尽导致超时。")
	harness.sweepOK(t)
	attemptID := harness.queryInt(t, `SELECT id FROM execution_attempts WHERE attempt_type='embedding' AND state='Queued'`)
	harness.walkEmbeddingAttemptToRunning(t, attemptID)
	// The source is withdrawn while the batch is in flight (commit-order
	// race): the late embedding result must be dropped, not written.
	now := testNow()
	if _, err := harness.db.Exec(`UPDATE knowledge_version_retrieval_state SET exited=1,exited_at=?,exit_reason='source_rejected',updated_at=?,row_version=row_version+1 WHERE knowledge_version_id=? AND exited=0`, now, now, versionA); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.db.Exec(`DELETE FROM knowledge_search_docs WHERE knowledge_version_id=?`, versionA); err != nil {
		t.Fatal(err)
	}
	if err := harness.commitFirstRebuild(t, 16, nil); !errors.Is(err, attempt.ErrLateResult) {
		t.Fatalf("late result after eligibility loss accepted: %v", err)
	}
	var state string
	if err := harness.db.QueryRow(`SELECT state FROM embeddings WHERE knowledge_version_id=?`, versionA).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("late result wrote the projection: state=%s err=%v", state, err)
	}
	var attemptState string
	if err := harness.db.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&attemptState); err != nil || attemptState != "Running" {
		t.Fatalf("attempt sealed by a late result: %s err=%v", attemptState, err)
	}
}

func TestFailedBatchBlocksGenerationSwitch(t *testing.T) {
	harness := newHarness(t)
	harness.confirmKnowledge(t, "连接池治理", "数据库连接池耗尽导致超时。")
	harness.sweepOK(t)
	attemptID := harness.queryInt(t, `SELECT id FROM execution_attempts WHERE attempt_type='embedding' AND state='Queued'`)
	harness.walkEmbeddingAttemptToRunning(t, attemptID)
	if err := harness.service.Fail(context.Background(), attemptID, "boot", 1, "provider_unavailable"); err != nil {
		t.Fatal(err)
	}
	if err := harness.service.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	// DATA-EMBED-002: an incomplete generation never switches; nothing is
	// current, the projection honestly reports rebuilding.
	if current := harness.queryInt(t, `SELECT COUNT(*) FROM embedding_generations WHERE state='current'`); current != 0 {
		t.Fatal("incomplete generation switched to current")
	}
	if building := harness.queryInt(t, `SELECT COUNT(*) FROM embedding_generations WHERE state='building'`); building != 1 {
		t.Fatal("failed batch lost its building generation")
	}
}

func TestSweepKeepsLiveQueryAttempts(t *testing.T) {
	harness := newHarness(t)
	harness.confirmKnowledge(t, "连接池治理", "数据库连接池耗尽导致超时。")
	harness.sweepOK(t)
	if err := harness.commitFirstRebuild(t, 16, nil); err != nil {
		t.Fatal(err)
	}
	generation := harness.queryInt(t, `SELECT id FROM embedding_generations WHERE state='current'`)
	// The live request registers its originator before the attempt commits,
	// so a sweep racing the request window converges nothing.
	if _, _, err := harness.service.CreateQueryAttempt(context.Background(), "活跃请求"); err != nil {
		t.Fatal(err)
	}
	harness.sweepOK(t)
	if live := harness.queryInt(t, `SELECT COUNT(*) FROM execution_attempts WHERE attempt_type='embedding' AND state='Queued' AND scope_id=?`, generation); live != 1 {
		t.Fatal("sweep converged a live query attempt")
	}
	harness.service.forgetQuery(harness.queryInt(t, `SELECT id FROM execution_attempts WHERE attempt_type='embedding' AND state='Queued' AND scope_id=?`, generation))
}

func TestSweepConvergesOrphanedQueryAttempts(t *testing.T) {
	harness := newHarness(t)
	harness.confirmKnowledge(t, "连接池治理", "数据库连接池耗尽导致超时。")
	harness.sweepOK(t)
	if err := harness.commitFirstRebuild(t, 16, nil); err != nil {
		t.Fatal(err)
	}
	generation := harness.queryInt(t, `SELECT id FROM embedding_generations WHERE state='current'`)
	// Simulate a restart-leaved Queued query attempt with no live
	// originator: the sweep must converge it so the scope stays free. The
	// orphan is created through a detached service whose registry never
	// holds the query text (as after a process restart).
	orphanService := NewService(harness.db)
	if _, _, createErr := orphanService.CreateQueryAttempt(context.Background(), "重启前的问题"); createErr != nil {
		t.Fatalf("create query attempt: %v", createErr)
	}
	harness.sweepOK(t)
	var states []string
	rows, err := harness.db.Query(`SELECT state FROM execution_attempts WHERE attempt_type='embedding' AND scope_id=?`, generation)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var state string
		_ = rows.Scan(&state)
		states = append(states, state)
	}
	rows.Close()
	if len(states) != 2 || states[0] != "Succeeded" || states[1] != "Cancelled" {
		t.Fatalf("orphaned query attempt states = %v, want [Cancelled]", states)
	}
}
