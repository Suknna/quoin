package knowledge

// Knowledge candidate state machine tests over the real frozen schema:
// create-or-return idempotency, source closure, immutable original
// suggestion, revisioned drafts with conflict reporting, the human
// confirmation boundary producing the first immutable KnowledgeVersion,
// exclusion and the read projections (DATA-KNOWLEDGE-001..008).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
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

const analysisContent = "数据库连接池耗尽导致超时。\n建议检查最大连接数配置并重启实例。"

type fixture struct {
	db              *sql.DB
	service         *Service
	userID          int64
	outputID        int64
	analysisID      int64
	occurrenceID    int64
	investigationID int64
	assistantID     int64
	connectionID    int64
	revisionID      int64
	generationID    int64
	probeResultID   int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := newTestDB(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	user, err := db.Exec(`INSERT INTO users(username,display_name,role,enabled,password_phc,auth_revision,created_at,updated_at) VALUES('op','Op','operator',1,'x',1,?,?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	userID, _ := user.LastInsertId()
	connectionID, revisionID, generationID, probeResultID := seedProviderChain(t, db, userID)
	source, err := db.Exec(`INSERT INTO alert_sources(source_key,protocol,enabled,created_at) VALUES('kn-src','alertmanager',1,?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, _ := source.LastInsertId()
	labels := `{"alertname":"KnProbe","severity":"warning"}`
	occurrence, err := db.Exec(`INSERT INTO alert_occurrences(source_id,fingerprint,starts_at,state,labels_canonical,labels_digest,first_seen_at,last_state_change_at) VALUES(?,x'0000000000000002',?,'Firing',?,?,?,?)`,
		sourceID, now, labels, digestOf(labels), now, now)
	if err != nil {
		t.Fatal(err)
	}
	occurrenceID, _ := occurrence.LastInsertId()
	analysis, err := db.Exec(`INSERT INTO initial_analyses(occurrence_id,state,input_snapshot_digest,created_at) VALUES(?,'Queued',?,?)`, occurrenceID, strings.Repeat("5", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	analysisID, _ := analysis.LastInsertId()
	attemptID := seedChatAttempt(t, db, "initial_analysis", "analysis", "initial_analysis_v1", analysisID, connectionID, revisionID, generationID, probeResultID)
	output, err := db.Exec(`INSERT INTO initial_analysis_outputs(analysis_id,attempt_id,model_id,content,created_at) VALUES(?,?,'fixture-chat',?,?)`, analysisID, attemptID, analysisContent, now)
	if err != nil {
		t.Fatal(err)
	}
	outputID, _ := output.LastInsertId()
	for _, state := range []string{"Running", "Succeeded"} {
		if _, err := db.Exec(`UPDATE initial_analyses SET state=?, row_version=row_version+1 WHERE id=?`, state, analysisID); err != nil {
			t.Fatal(err)
		}
	}
	investigation, err := db.Exec(`INSERT INTO investigations(created_at) VALUES(?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	investigationID, _ := investigation.LastInsertId()
	turnAttempt, err := db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,created_at) VALUES('investigation','investigation',?,'Queued','test',?)`, investigationID, now)
	if err != nil {
		t.Fatal(err)
	}
	turnAttemptID, _ := turnAttempt.LastInsertId()
	if _, err := db.Exec(`INSERT INTO investigation_messages(investigation_id,attempt_id,seq,role,status,content,created_at) VALUES(?,?,1,'user','active','连接池的问题怎么处理？',?)`, investigationID, turnAttemptID, now); err != nil {
		t.Fatal(err)
	}
	walkAttemptToRunning(t, db, turnAttemptID, "investigation_v1", investigationID, connectionID, revisionID, generationID, probeResultID)
	assistant, err := db.Exec(`INSERT INTO investigation_messages(investigation_id,attempt_id,seq,role,status,content,created_at) VALUES(?,?,2,'assistant','active','先检查连接池上限，再观察超时指标。',?)`, investigationID, turnAttemptID, now)
	if err != nil {
		t.Fatal(err)
	}
	assistantID, _ := assistant.LastInsertId()
	return &fixture{db: db, service: NewService(db), userID: userID, outputID: outputID, analysisID: analysisID, occurrenceID: occurrenceID, investigationID: investigationID, assistantID: assistantID, connectionID: connectionID, revisionID: revisionID, generationID: generationID, probeResultID: probeResultID}
}

// seedChatAttempt creates and walks one chat attempt to Running.
func seedChatAttempt(t *testing.T, db *sql.DB, attemptType, scopeType, schemaKind string, scopeID, connectionID, revisionID, generationID, probeResultID int64) int64 {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	attempt, err := db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,created_at) VALUES(?,?,?,'Queued','test',?)`, attemptType, scopeType, scopeID, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := attempt.LastInsertId()
	walkAttemptToRunning(t, db, id, schemaKind, scopeID, connectionID, revisionID, generationID, probeResultID)
	return id
}

// walkAttemptToRunning freezes a minimal input snapshot, chat_model grant
// and dispatch binding, then accepts the attempt.
func walkAttemptToRunning(t *testing.T, db *sql.DB, attemptID int64, schemaKind string, scopeID, connectionID, revisionID, generationID, probeResultID int64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	snapshot, err := db.Exec(`INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(?,?,'fixture-renderer',?,?)`, attemptID, schemaKind, strings.Repeat("6", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, _ := snapshot.LastInsertId()
	if _, err := db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,initial_analysis_id) VALUES(?,1,'user',?,?)`, snapshotID, strings.Repeat("6", 64), scopeID); err != nil {
		if _, messageErr := db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,investigation_message_id) VALUES(?,1,'user',?,(SELECT id FROM investigation_messages WHERE attempt_id=? AND role='user' LIMIT 1))`, snapshotID, strings.Repeat("6", 64), attemptID); messageErr != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at) VALUES(?,'chat_model',?,?,?,?,?)`,
		attemptID, connectionID, revisionID, generationID, probeResultID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Assigned', runtime_slot='plinth', boot_id='boot', connection_epoch=1, lease_until=?, runtime_release_version='test', agent_version='agent-v1', row_version=row_version+1 WHERE id=?`, now, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Running', accepted_at=?, started_at=?, row_version=row_version+1 WHERE id=?`, now, now, attemptID); err != nil {
		t.Fatal(err)
	}
}

func digestOf(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func strconvParseInt(value string) (int64, error) {
	return strconv.ParseInt(value, 10, 64)
}

// seedProviderChain inserts one enabled qualified model provider (the
// analysis-test precedent) so chat attempts may dispatch.
func seedProviderChain(t *testing.T, db *sql.DB, actorID int64) (connectionID, revisionID, generationID, probeResultID int64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT OR IGNORE INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`, []byte(strings.Repeat("e", 12)), []byte(strings.Repeat("f", 16)), now); err != nil {
		t.Fatal(err)
	}
	connection, err := db.Exec(`INSERT INTO connections(name,type,enabled,created_at) VALUES('kn-provider','model_provider',0,?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	connectionID, _ = connection.LastInsertId()
	revision, err := db.Exec(`INSERT INTO connection_revisions(connection_id,revision_seq,config_json,created_at) VALUES(?,1,?,?)`, connectionID, `{"baseUrl":"https://provider.test","chatModelId":"fixture-chat-1","contextBudgetTokens":4096,"maxOutputTokens":1024}`, now)
	if err != nil {
		t.Fatal(err)
	}
	revisionID, _ = revision.LastInsertId()
	nonce := make([]byte, 12)
	for i := range nonce {
		nonce[i] = byte(i * 11)
	}
	generation, err := db.Exec(`INSERT INTO credential_generations(connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_at) VALUES(?,1,1,1,?,?,?)`, connectionID, nonce, []byte(strings.Repeat("f", 32)), now)
	if err != nil {
		t.Fatal(err)
	}
	generationID, _ = generation.LastInsertId()
	if _, err := db.Exec(`UPDATE connections SET current_revision_id=?, current_credential_generation_id=?, row_version=row_version+1 WHERE id=?`, revisionID, generationID, connectionID); err != nil {
		t.Fatal(err)
	}
	var slotState string
	if err := db.QueryRow(`SELECT state FROM runtime_slots WHERE slot='plinth'`).Scan(&slotState); err != nil {
		if _, err := db.Exec(`INSERT INTO runtime_slots(slot,state,created_at) VALUES('plinth','unregistered',?)`, now); err != nil {
			t.Fatal(err)
		}
		slotState = "unregistered"
	}
	if slotState == "unregistered" {
		runtimeCredential, err := db.Exec(`INSERT INTO runtime_credentials(slot,generation,token_digest,created_at,confirmed_at) VALUES('plinth',1,?,?,?)`, []byte(strings.Repeat("0", 32)), now, now)
		if err != nil {
			t.Fatal(err)
		}
		runtimeCredentialID, _ := runtimeCredential.LastInsertId()
		if _, err := db.Exec(`UPDATE runtime_slots SET state='registered',current_credential_id=?,row_version=row_version+1 WHERE slot='plinth' AND state='unregistered'`, runtimeCredentialID); err != nil {
			t.Fatal(err)
		}
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
	if _, err := db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id) VALUES(?,1,'user',?,?)`, probeSnapshotID, strings.Repeat("0", 64), revisionID); err != nil {
		t.Fatal(err)
	}
	var probeChatGrantID, probeEmbedGrantID int64
	for _, purpose := range []string{"model_probe_chat", "model_probe_embedding"} {
		grant, err := db.Exec(`INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at) VALUES(?,?,?,?,?,?)`, probeAttemptID, purpose, connectionID, revisionID, generationID, now)
		if err != nil {
			t.Fatal(err)
		}
		if purpose == "model_probe_chat" {
			probeChatGrantID, _ = grant.LastInsertId()
		} else {
			probeEmbedGrantID, _ = grant.LastInsertId()
		}
	}
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='boot',connection_epoch=1,lease_until=?,runtime_release_version='test',row_version=row_version+1 WHERE id=? AND state='Queued'`, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Running',accepted_at=?,started_at=?,row_version=row_version+1 WHERE id=? AND state='Assigned'`, now, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
	probe, err := db.Exec(`INSERT INTO connection_probe_results(attempt_id,connection_id,connection_type,connection_revision_id,credential_generation_id,root_binding_revision,action_set_id,action_set_version,probe_contract_digest,outcome,result_digest,started_at,finished_at,created_at) VALUES(?,?,?,?,?,1,'model-provider-capabilities',1,?,?,?,?,?,?)`,
		probeAttemptID, connectionID, "model_provider", revisionID, generationID, strings.Repeat("0", 64), "passed", strings.Repeat("1", 64), now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	probeResultID, _ = probe.LastInsertId()
	seedProbeModelCalls(t, db, probeAttemptID, probeChatGrantID, probeEmbedGrantID, now)
	if _, err := db.Exec(`INSERT INTO model_provider_connection_probe_results(probe_result_id,chat_model_id,embedding_model_id,context_budget_tokens,max_output_tokens,streaming_supported,native_tool_calling_supported,multi_tool_call_supported,cancellation_observed,usage_observed,request_id_observed,embedding_supported,embedding_vector_dim,detail_json) VALUES(?,'fixture-chat-1','fixture-embed-1',4096,1024,1,1,1,1,1,1,1,16,'{}')`, probeResultID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Succeeded',ended_at=?,row_version=row_version+1 WHERE id=? AND state='Running'`, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO connection_enable_qualifications(connection_id,enabled_row_version,probe_result_id,created_by,created_at) VALUES(?,3,?,?,?)`, connectionID, probeResultID, actorID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE connections SET enabled=1,revalidation_required=0,row_version=row_version+1 WHERE id=? AND row_version=2`, connectionID); err != nil {
		t.Fatal(err)
	}
	return connectionID, revisionID, generationID, probeResultID
}

// seedProbeModelCalls writes the succeeded chat calls and one embedding
// call the probe attempt's Succeeded transition must close over.
func seedProbeModelCalls(t *testing.T, db *sql.DB, attemptID, chatGrantID, embedGrantID int64, now string) {
	t.Helper()
	digest := strings.Repeat("1", 64)
	for _, callSeq := range []int{1, 4} {
		call, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,evicted_turn_count,status,started_at) VALUES(?,?,'0','chat','fixture-chat-1',?,'connection-probe-v1','probe-supervisor-v1',?,?,?,?,?,4096,1024,0,0,'running',?)`,
			attemptID, callSeq, chatGrantID, digest, digest, digest, digest, digest, now)
		if err != nil {
			t.Fatal(err)
		}
		callID, _ := call.LastInsertId()
		if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,1,'system',?,'system_contract'),(?,2,'system',?,'tool_schema')`, callID, digest, callID, digest); err != nil {
			t.Fatal(err)
		}
		if callSeq == 1 {
			if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,1,'{"assistantText":"ok","finishReason":"stop","tool_calls":[]}',?,'stop',?)`, callID, digest, now); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":1,"total_tokens":2}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := db.Exec(`UPDATE model_calls SET status='cancelled',termination_reason='cancelled',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
				t.Fatal(err)
			}
		}
	}
	embed, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,input_snapshot_digest,rendered_request_digest,estimated_input_tokens,status,started_at) VALUES(?,6,'0','embedding','fixture-embed-1',?,?,?,0,'running',?)`,
		attemptID, embedGrantID, digest, digest, now)
	if err != nil {
		t.Fatal(err)
	}
	embedID, _ := embed.LastInsertId()
	var probeSnapshotID int64
	if err := db.QueryRow(`SELECT id FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&probeSnapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,attempt_input_snapshot_id) VALUES(?,1,'user',?,?)`, embedID, digest, probeSnapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,created_at) VALUES(?,1,'{}',?,?)`, embedID, digest, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":0,"total_tokens":1}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, embedID); err != nil {
		t.Fatal(err)
	}
}

func TestCreateFromAnalysisOutputSeedsSuggestion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	result, err := f.service.CreateFromAnalysisOutput(ctx, f.userID, "cmd-kn-create-1", f.occurrenceID, f.analysisID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.Candidate.State != StateAwaiting || result.Candidate.DraftRevision != 0 {
		t.Fatalf("unexpected create result: %+v", result.Candidate)
	}
	if result.Candidate.DraftTitle != "数据库连接池耗尽导致超时。" {
		t.Fatalf("draft title = %q", result.Candidate.DraftTitle)
	}
	if result.Candidate.DraftBody != analysisContent {
		t.Fatalf("draft body = %q", result.Candidate.DraftBody)
	}
	detail, err := f.service.GetCandidate(ctx, parseID(t, result.Candidate.ID))
	if err != nil {
		t.Fatal(err)
	}
	var suggestion Suggestion
	if err := json.Unmarshal(detail.OriginalSuggestion, &suggestion); err != nil {
		t.Fatal(err)
	}
	if suggestion.V != 1 || suggestion.Body != analysisContent || suggestion.Source.Type != SourceAnalysisOutput {
		t.Fatalf("suggestion projection broken: %+v", suggestion)
	}
	if suggestion.Source.ModelID != "fixture-chat" || suggestion.Source.ID != result.Candidate.SourceID {
		t.Fatalf("suggestion source link broken: %+v", suggestion.Source)
	}
	if suggestion.Source.Locator["analysisId"] != float64(f.analysisID) {
		t.Fatalf("suggestion locator missing analysis link: %+v", suggestion.Source.Locator)
	}
	// A wrong occurrence path is not this output's home.
	if _, err := f.service.CreateFromAnalysisOutput(ctx, f.userID, "cmd-kn-create-2", f.occurrenceID+999, f.analysisID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-occurrence create = %v", err)
	}
}

func TestCreateOrReturnAndCommandReplay(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first, err := f.service.CreateFromAnalysisOutput(ctx, f.userID, "cmd-kn-a", f.occurrenceID, f.analysisID)
	if err != nil {
		t.Fatal(err)
	}
	// A different command id still returns the same candidate.
	second, err := f.service.CreateFromAnalysisOutput(ctx, f.userID, "cmd-kn-b", f.occurrenceID, f.analysisID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.Candidate.ID != first.Candidate.ID {
		t.Fatalf("create-or-return broken: %+v vs %+v", first.Candidate, second.Candidate)
	}
	// Same command replay returns the original outcome verbatim, including
	// the created flag (HTTP-COMMAND-003).
	replay, err := f.service.CreateFromAnalysisOutput(ctx, f.userID, "cmd-kn-a", f.occurrenceID, f.analysisID)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Candidate.ID != first.Candidate.ID || replay.Created != first.Created {
		t.Fatalf("replay broken: %+v", replay.Candidate)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM knowledge_candidates`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("create-or-return duplicated rows: %d", count)
	}
}

func TestCreateFromInvestigationMessageValidation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	result, err := f.service.CreateFromInvestigationMessage(ctx, f.userID, "cmd-kn-msg-1", f.investigationID, f.assistantID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.Candidate.SourceType != SourceMessage {
		t.Fatalf("message create broken: %+v", result.Candidate)
	}
	if result.Candidate.DraftBody != "先检查连接池上限，再观察超时指标。" {
		t.Fatalf("message draft body = %q", result.Candidate.DraftBody)
	}
	// A user message is not a valid source.
	var userMessageID int64
	if err := f.db.QueryRow(`SELECT id FROM investigation_messages WHERE role='user'`).Scan(&userMessageID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.CreateFromInvestigationMessage(ctx, f.userID, "cmd-kn-msg-2", f.investigationID, userMessageID); !errors.Is(err, ErrSourceShape) {
		t.Fatalf("user message accepted: %v", err)
	}
	// A message of another investigation is 404.
	if _, err := f.service.CreateFromInvestigationMessage(ctx, f.userID, "cmd-kn-msg-3", f.investigationID+999, f.assistantID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-investigation accepted: %v", err)
	}
	// A withdrawn assistant message of another investigation can no
	// longer seed a candidate (its own turn attempt stays Running).
	withdrawnInvestigation, err := f.db.Exec(`INSERT INTO investigations(created_at) VALUES(?)`, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	withdrawnInvestigationID, _ := withdrawnInvestigation.LastInsertId()
	withdrawnTurn, err := f.db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,created_at) VALUES('investigation','investigation',?,'Queued','test',?)`, withdrawnInvestigationID, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	withdrawnTurnID, _ := withdrawnTurn.LastInsertId()
	if _, err := f.db.Exec(`INSERT INTO investigation_messages(investigation_id,attempt_id,seq,role,status,content,created_at) VALUES(?,?,1,'user','active','问题？',?)`, withdrawnInvestigationID, withdrawnTurnID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	walkAttemptToRunning(t, f.db, withdrawnTurnID, "investigation_v1", withdrawnInvestigationID, f.connectionID, f.revisionID, f.generationID, f.probeResultID)
	withdrawn, err := f.db.Exec(`INSERT INTO investigation_messages(investigation_id,attempt_id,seq,role,status,content,created_at) VALUES(?,?,2,'assistant','withdrawn','已撤回的建议',?)`,
		withdrawnInvestigationID, withdrawnTurnID, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	withdrawnID, _ := withdrawn.LastInsertId()
	if _, err := f.service.CreateFromInvestigationMessage(ctx, f.userID, "cmd-kn-msg-4", withdrawnInvestigationID, withdrawnID); !errors.Is(err, ErrSourceShape) {
		t.Fatalf("withdrawn message accepted: %v", err)
	}
}

func TestCreateRejectedSourceOnlyWithoutExistingCandidate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := f.db.Exec(`INSERT INTO diagnosis_feedback(target_type,target_id,value,created_by,created_at) VALUES('initial_analysis_output',?,'rejected',?,?)`, f.outputID, f.userID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.CreateFromAnalysisOutput(ctx, f.userID, "cmd-kn-rej-1", f.occurrenceID, f.analysisID); !errors.Is(err, ErrSourceRejected) {
		t.Fatalf("rejected source accepted: %v", err)
	}
	// With an existing candidate the create still returns the record
	// (dedupe precedes the source-state check, DATA-KNOWLEDGE-006).
	now2 := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := f.db.Exec(`INSERT INTO knowledge_candidates(source_type,source_id,generation,state,original_suggestion_json,draft_title,draft_body,draft_revision,created_by,created_at) VALUES('investigation_message',?,1,'AwaitingConfirmation','{}','标题','正文',0,?,?)`, f.assistantID, f.userID, now2); err != nil {
		t.Fatal(err)
	}
	result, err := f.service.CreateFromInvestigationMessage(ctx, f.userID, "cmd-kn-rej-2", f.investigationID, f.assistantID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created {
		t.Fatalf("existing candidate must be returned, not recreated")
	}
}

func TestDraftEditRevisionAndConflicts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created, err := f.service.CreateFromAnalysisOutput(ctx, f.userID, "cmd-kn-edit-0", f.occurrenceID, f.analysisID)
	if err != nil {
		t.Fatal(err)
	}
	candidateID := parseID(t, created.Candidate.ID)
	newTitle := "连接池耗尽处置"
	edited, err := f.service.EditDraft(ctx, f.userID, "cmd-kn-edit-1", candidateID, 0, &newTitle, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if edited.DraftRevision != 1 || edited.DraftTitle != newTitle || edited.DraftBody != analysisContent {
		t.Fatalf("edit result broken: %+v", edited)
	}
	// Stale expected revision reports the authoritative value.
	_, err = f.service.EditDraft(ctx, f.userID, "cmd-kn-edit-2", candidateID, 0, &newTitle, nil, nil)
	var conflict *RevisionConflict
	if !errors.As(err, &conflict) || conflict.Current != 1 {
		t.Fatalf("stale revision conflict = %v", err)
	}
	// Replay of the committed edit returns the same summary.
	replayed, err := f.service.EditDraft(ctx, f.userID, "cmd-kn-edit-1", candidateID, 0, &newTitle, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.DraftRevision != 1 {
		t.Fatalf("edit replay broken: %+v", replayed)
	}
	// The original suggestion never changes.
	detail, err := f.service.GetCandidate(ctx, candidateID)
	if err != nil {
		t.Fatal(err)
	}
	var suggestion Suggestion
	if err := json.Unmarshal(detail.OriginalSuggestion, &suggestion); err != nil {
		t.Fatal(err)
	}
	if suggestion.Title != "数据库连接池耗尽导致超时。" {
		t.Fatalf("original suggestion mutated: %q", suggestion.Title)
	}
	// An empty edit is rejected deterministically.
	if _, err := f.service.EditDraft(ctx, f.userID, "cmd-kn-edit-3", candidateID, 1, nil, nil, nil); !errors.Is(err, ErrEmptyEdit) {
		t.Fatalf("empty edit accepted: %v", err)
	}
}

func TestDraftScopePersistsIntoVersion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created, err := f.service.CreateFromAnalysisOutput(ctx, f.userID, "cmd-kn-scope-0", f.occurrenceID, f.analysisID)
	if err != nil {
		t.Fatal(err)
	}
	candidateID := parseID(t, created.Candidate.ID)
	scope := json.RawMessage(`{"businessSystem":"payments","alertname":"HighErrorRate"}`)
	edited, err := f.service.EditDraft(ctx, f.userID, "cmd-kn-scope-1", candidateID, 0, nil, nil, &scope)
	if err != nil {
		t.Fatal(err)
	}
	if string(edited.DraftScope) != `{"alertname":"HighErrorRate","businessSystem":"payments"}` {
		t.Fatalf("draft scope projection = %s", edited.DraftScope)
	}
	confirmed, err := f.service.Confirm(ctx, f.userID, "cmd-kn-scope-2", candidateID, 1)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := f.service.GetKnowledge(ctx, parseID(t, confirmed.ConfirmedKnowledgeID))
	if err != nil {
		t.Fatal(err)
	}
	version, err := f.service.GetVersion(ctx, parseID(t, confirmed.ConfirmedKnowledgeID), parseID(t, detail.CurrentVersionID))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(version.Scope, &decoded); err != nil {
		t.Fatalf("version scope missing: %v", err)
	}
	if decoded["businessSystem"] != "payments" || decoded["alertname"] != "HighErrorRate" {
		t.Fatalf("version scope = %v", decoded)
	}
	// Clearing the scope stores no restriction again.
	empty := json.RawMessage(`{}`)
	if _, err := f.service.EditDraft(ctx, f.userID, "cmd-kn-scope-3", parseID(t, "0"), 0, nil, nil, &empty); err == nil {
		t.Fatal("candidate 0 accepted")
	}
	// A non-object scope is a deterministic 422.
	array := json.RawMessage(`[1]`)
	if _, err := f.service.EditDraft(ctx, f.userID, "cmd-kn-scope-4", candidateID, 1, nil, nil, &array); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("array scope accepted: %v", err)
	}
}

func TestConfirmCreatesImmutableFirstVersion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created, err := f.service.CreateFromAnalysisOutput(ctx, f.userID, "cmd-kn-cf-0", f.occurrenceID, f.analysisID)
	if err != nil {
		t.Fatal(err)
	}
	candidateID := parseID(t, created.Candidate.ID)
	newBody := "连接池上限不足时先扩容再重启实例。"
	if _, err := f.service.EditDraft(ctx, f.userID, "cmd-kn-cf-1", candidateID, 0, nil, &newBody, nil); err != nil {
		t.Fatal(err)
	}
	confirmed, err := f.service.Confirm(ctx, f.userID, "cmd-kn-cf-2", candidateID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.State != StateConfirmed || confirmed.ConfirmedKnowledgeID == "" {
		t.Fatalf("confirm result broken: %+v", confirmed)
	}
	knowledgeID := parseID(t, confirmed.ConfirmedKnowledgeID)
	detail, err := f.service.GetKnowledge(ctx, knowledgeID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.VersionCount != 1 || detail.CurrentVersionSeq != 1 || !detail.Eligible {
		t.Fatalf("knowledge detail broken: %+v", detail)
	}
	version, err := f.service.GetVersion(ctx, knowledgeID, parseID(t, detail.CurrentVersionID))
	if err != nil {
		t.Fatal(err)
	}
	if version.Body != newBody || version.SourceCandidateID != confirmed.ID {
		t.Fatalf("version content broken: %+v", version)
	}
	var ftsTitle string
	if err := f.db.QueryRow(`SELECT title FROM knowledge_fts WHERE rowid=?`, parseID(t, detail.CurrentVersionID)).Scan(&ftsTitle); err != nil {
		t.Fatalf("FTS projection missing: %v", err)
	}
	if ftsTitle != confirmed.DraftTitle {
		t.Fatalf("FTS title = %q", ftsTitle)
	}
	// Replay returns the original confirmation.
	replayed, err := f.service.Confirm(ctx, f.userID, "cmd-kn-cf-2", candidateID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ConfirmedKnowledgeID != confirmed.ConfirmedKnowledgeID {
		t.Fatalf("confirm replay created a second knowledge: %+v", replayed)
	}
	// A second confirm under a different command is a state conflict.
	if _, err := f.service.Confirm(ctx, f.userID, "cmd-kn-cf-3", candidateID, 1); err == nil {
		t.Fatal("second confirm accepted")
	}
	// Exactly one knowledge/version exists for this candidate.
	var versions int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM knowledge_versions WHERE source_candidate_id=?`, candidateID).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Fatalf("candidate produced %d versions", versions)
	}
	// Draft edits after confirmation are state conflicts.
	if _, err := f.service.EditDraft(ctx, f.userID, "cmd-kn-cf-4", candidateID, 1, &newBody, nil, nil); err == nil {
		t.Fatal("edit after confirm accepted")
	}
	// Stale revision confirm conflicts with the current value reported.
	other, err := f.service.CreateFromInvestigationMessage(ctx, f.userID, "cmd-kn-cf-5", f.investigationID, f.assistantID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.Confirm(ctx, f.userID, "cmd-kn-cf-6", parseID(t, other.Candidate.ID), 7)
	var stale *RevisionConflict
	if !errors.As(err, &stale) {
		t.Fatalf("stale confirm = %v", err)
	}
}

func TestExcludeFenceAndState(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created, err := f.service.CreateFromAnalysisOutput(ctx, f.userID, "cmd-kn-ex-0", f.occurrenceID, f.analysisID)
	if err != nil {
		t.Fatal(err)
	}
	candidateID := parseID(t, created.Candidate.ID)
	if _, err := f.service.Exclude(ctx, f.userID, "cmd-kn-ex-1", candidateID, 99); err == nil {
		t.Fatal("wrong row version accepted")
	}
	excluded, err := f.service.Exclude(ctx, f.userID, "cmd-kn-ex-2", candidateID, created.Candidate.RowVersion)
	if err != nil {
		t.Fatal(err)
	}
	if excluded.State != StateExcluded {
		t.Fatalf("exclude result: %+v", excluded)
	}
	if _, err := f.service.Confirm(ctx, f.userID, "cmd-kn-ex-3", candidateID, excluded.DraftRevision); err == nil {
		t.Fatal("confirm after exclude accepted")
	}
	// Create-or-return still returns the excluded record.
	again, err := f.service.CreateFromAnalysisOutput(ctx, f.userID, "cmd-kn-ex-4", f.occurrenceID, f.analysisID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Created || again.Candidate.ID != created.Candidate.ID {
		t.Fatalf("excluded candidate must still be returned: %+v", again.Candidate)
	}
}

func TestListAndBrowseProjections(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first, err := f.service.CreateFromAnalysisOutput(ctx, f.userID, "cmd-kn-ls-1", f.occurrenceID, f.analysisID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.service.CreateFromInvestigationMessage(ctx, f.userID, "cmd-kn-ls-2", f.investigationID, f.assistantID)
	if err != nil {
		t.Fatal(err)
	}
	items, _, err := f.service.ListCandidates(ctx, ListFilter{}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].State != StateAwaiting || items[1].State != StateAwaiting {
		t.Fatalf("list broken: %+v", items)
	}
	if _, err := f.service.Exclude(ctx, f.userID, "cmd-kn-ls-3", parseID(t, second.Candidate.ID), second.Candidate.RowVersion); err != nil {
		t.Fatal(err)
	}
	filtered, _, err := f.service.ListCandidates(ctx, ListFilter{State: StateExcluded}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].ID != second.Candidate.ID {
		t.Fatalf("state filter broken: %+v", filtered)
	}
	// Only confirmed knowledge appears in browse.
	browse, _, err := f.service.Browse(ctx, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(browse) != 0 {
		t.Fatalf("browse before confirmation: %+v", browse)
	}
	confirmed, err := f.service.Confirm(ctx, f.userID, "cmd-kn-ls-4", parseID(t, first.Candidate.ID), 0)
	if err != nil {
		t.Fatal(err)
	}
	browse, _, err = f.service.Browse(ctx, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(browse) != 1 || browse[0].ID != confirmed.ConfirmedKnowledgeID || !browse[0].Eligible {
		t.Fatalf("browse after confirmation: %+v", browse)
	}
	versions, _, err := f.service.ListVersions(ctx, parseID(t, confirmed.ConfirmedKnowledgeID), nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || versions[0].VersionSeq != 1 || !versions[0].Eligible {
		t.Fatalf("versions projection: %+v", versions)
	}
	if _, err := f.service.GetKnowledge(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing knowledge = %v", err)
	}
	if _, err := f.service.GetVersion(ctx, parseID(t, confirmed.ConfirmedKnowledgeID), 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing version = %v", err)
	}
}

func parseID(t *testing.T, value string) int64 {
	t.Helper()
	id, err := strconvParseInt(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestConcurrentConfirmCreatesExactlyOneKnowledge(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created, err := f.service.CreateFromAnalysisOutput(ctx, f.userID, "cmd-kn-race-create", f.occurrenceID, f.analysisID)
	if err != nil {
		t.Fatal(err)
	}
	candidateID := parseID(t, created.Candidate.ID)
	// Two concurrent confirms with the correct expected revision: SQLite
	// BEGIN IMMEDIATE serializes the writers, so exactly one commits the
	// knowledge aggregate; the loser deterministically conflicts (the
	// commit-order adjudication DATA-KNOWLEDGE-004 relies on).
	const racers = 2
	outcomes := make(chan error, racers)
	var started sync.WaitGroup
	started.Add(racers)
	for index := 0; index < racers; index++ {
		go func(index int) {

			started.Done()
			_, err := f.service.Confirm(ctx, f.userID, fmt.Sprintf("cmd-kn-race-%d", index), candidateID, 0)
			if err != nil {
				outcomes <- err
				return
			}
			outcomes <- nil
		}(index)
	}
	started.Wait()
	var successes, conflicts int
	for index := 0; index < racers; index++ {
		if err := <-outcomes; err == nil {
			successes++
		} else {
			var state *StateConflict
			var revision *RevisionConflict
			if errors.As(err, &state) || errors.As(err, &revision) {
				conflicts++
			} else {
				t.Fatalf("unexpected race outcome: %v", err)
			}
		}
	}
	if successes != 1 || conflicts != racers-1 {
		t.Fatalf("confirm race: successes=%d conflicts=%d", successes, conflicts)
	}
	var knowledgeCount, versionCount int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM reusable_knowledge`).Scan(&knowledgeCount); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM knowledge_versions`).Scan(&versionCount); err != nil {
		t.Fatal(err)
	}
	if knowledgeCount != 1 || versionCount != 1 {
		t.Fatalf("race produced %d knowledge / %d versions", knowledgeCount, versionCount)
	}
}
