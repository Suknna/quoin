package knowledge

// HTTP-layer tests over a real humago mux and the frozen schema: the
// frozen status pair for create-or-return, the conflict envelopes for
// stale revisions, the human confirmation boundary, and the two search
// envelopes.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/feedback"
	domainknowledge "github.com/Suknna/quoin/internal/quoin/knowledge"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	_ "modernc.org/sqlite"
)

type httpFixture struct {
	db      *sql.DB
	handler http.Handler
	userID  int64
	cookie  string
}

func newHTTPFixture(t *testing.T) *httpFixture {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/test.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	f := &httpFixture{db: db, cookie: "session-cookie"}
	domainHandler := &Handler{
		Feedback:  feedback.NewService(db),
		Knowledge: domainknowledge.NewService(db),
		Authenticate: func(ctx context.Context, cookie string) (int64, error) {
			if cookie != f.cookie {
				return 0, errors.New("unauthenticated")
			}
			return f.userID, nil
		},
	}
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("test", "test"))
	domainHandler.Register(api)
	f.handler = mux
	return f
}

func (f *httpFixture) do(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request.AddCookie(&http.Cookie{Name: "__Host-quoin-session", Value: f.cookie})
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	var decoded map[string]any
	if recorder.Body.Len() > 0 {
		if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("response not json: %s", recorder.Body.String())
		}
	}
	return recorder.Code, decoded
}

func (f *httpFixture) seedUser(t *testing.T) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	user, err := f.db.Exec(`INSERT INTO users(username,display_name,role,enabled,password_phc,auth_revision,created_at,updated_at) VALUES('op','Op','operator',1,'x',1,?,?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	f.userID, _ = user.LastInsertId()
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

func digestOf(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestCandidateLifecycleThroughHTTP(t *testing.T) {
	f := newHTTPFixture(t)
	f.seedUser(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_ = now
	// One sealed analysis output as the immutable source.
	source, err := f.db.Exec(`INSERT INTO alert_sources(source_key,protocol,enabled,created_at) VALUES('http-src','alertmanager',1,?)`, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	sourceID, _ := source.LastInsertId()
	labels := `{"alertname":"HttpProbe","severity":"warning"}`
	occurrence, err := f.db.Exec(`INSERT INTO alert_occurrences(source_id,fingerprint,starts_at,state,labels_canonical,labels_digest,first_seen_at,last_state_change_at) VALUES(?,x'0000000000000003',?,'Firing',?,?,?,?)`,
		sourceID, time.Now().UTC().Format(time.RFC3339Nano), labels, digestOf(labels), time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	occurrenceID, _ := occurrence.LastInsertId()
	analysis, err := f.db.Exec(`INSERT INTO initial_analyses(occurrence_id,state,input_snapshot_digest,created_at) VALUES(?,'Queued',?,?)`, occurrenceID, strings.Repeat("7", 64), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	analysisID, _ := analysis.LastInsertId()
	connectionID, revisionID, generationID, probeResultID := seedProviderChain(t, f.db, f.userID)
	attemptID := seedChatAttempt(t, f.db, "initial_analysis", "analysis", "initial_analysis_v1", analysisID, connectionID, revisionID, generationID, probeResultID)
	output, err := f.db.Exec(`INSERT INTO initial_analysis_outputs(analysis_id,attempt_id,model_id,content,created_at) VALUES(?,?,'fixture-chat','数据库连接池耗尽导致超时。\n建议检查配置。',?)`, analysisID, attemptID, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	outputID, _ := output.LastInsertId()
	_ = outputID
	for _, state := range []string{"Running", "Succeeded"} {
		if _, err := f.db.Exec(`UPDATE initial_analyses SET state=?, row_version=row_version+1 WHERE id=?`, state, analysisID); err != nil {
			t.Fatal(err)
		}
	}
	createPath := "/api/v1/alerts/" + strconv.FormatInt(occurrenceID, 10) + "/analyses/" + strconv.FormatInt(analysisID, 10) + "/knowledge-candidates"

	// Create returns 201 with the suggestion prefilled.
	status, created := f.do(t, http.MethodPost, createPath, `{"clientCommandId":"http-create-1"}`)
	if status != http.StatusCreated || created["state"] != "AwaitingConfirmation" {
		t.Fatalf("create = %d %v", status, created)
	}
	// Create-or-return: a different command returns the same record with
	// the frozen 200.
	status, returned := f.do(t, http.MethodPost, createPath, `{"clientCommandId":"http-create-2"}`)
	if status != http.StatusOK || returned["id"] != created["id"] {
		t.Fatalf("create-or-return = %d %v vs %v", status, returned, created)
	}
	path := "/api/v1/knowledge/candidates/" + created["id"].(string)

	// Detail keeps the immutable original suggestion.
	status, detail := f.do(t, http.MethodGet, path, "")
	if status != http.StatusOK {
		t.Fatalf("detail status = %d", status)
	}
	if detail["originalSuggestion"] == nil || detail["draftRevision"].(float64) != 0 {
		t.Fatalf("detail shape broken: %v", detail)
	}

	// Draft edit bumps the revision; a stale edit returns the frozen
	// conflict envelope with the authoritative current revision.
	status, edited := f.do(t, http.MethodPatch, path, `{"clientCommandId":"http-edit-1","expectedRevision":0,"title":"新标题"}`)
	if status != http.StatusOK || edited["draftRevision"].(float64) != 1 {
		t.Fatalf("edit = %d %v", status, edited)
	}
	status, conflict := f.do(t, http.MethodPatch, path, `{"clientCommandId":"http-edit-2","expectedRevision":0,"title":"旧"}`)
	if status != http.StatusConflict {
		t.Fatalf("stale edit status = %d", status)
	}
	conflictMap, ok := conflict["conflict"].(map[string]any)
	if !ok || conflictMap["code"] != "row_version_conflict" || conflictMap["currentRevision"].(float64) != 1 {
		t.Fatalf("conflict envelope broken: %v", conflict)
	}

	// Confirm creates the first immutable version and the browse entry.
	status, confirmed := f.do(t, http.MethodPost, path+"/confirm", `{"clientCommandId":"http-confirm-1","expectedRevision":1}`)
	if status != http.StatusOK || confirmed["state"] != "Confirmed" || confirmed["confirmedKnowledgeId"] == nil {
		t.Fatalf("confirm = %d %v", status, confirmed)
	}
	knowledgeID := confirmed["confirmedKnowledgeId"].(string)
	status, browse := f.do(t, http.MethodGet, "/api/v1/knowledge", "")
	if status != http.StatusOK || browse["mode"] != "browse" {
		t.Fatalf("browse = %d %v", status, browse)
	}
	items := browse["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != knowledgeID {
		t.Fatalf("browse items broken: %v", browse)
	}
	status, knowledge := f.do(t, http.MethodGet, "/api/v1/knowledge/items/"+knowledgeID, "")
	if status != http.StatusOK || knowledge["versionCount"].(float64) != 1 {
		t.Fatalf("knowledge detail = %d %v", status, knowledge)
	}
	status, versions := f.do(t, http.MethodGet, "/api/v1/knowledge/items/"+knowledgeID+"/versions", "")
	if status != http.StatusOK || len(versions["items"].([]any)) != 1 {
		t.Fatalf("versions = %d %v", status, versions)
	}

	// The FTS query channel returns the raw ranked projection with an
	// honest empty semantic channel.
	status, query := f.do(t, http.MethodGet, "/api/v1/knowledge?q="+url.QueryEscape("新标题"), "")
	if status != http.StatusOK || query["mode"] != "query" {
		t.Fatalf("query = %d %v", status, query)
	}
	if len(query["exactTextMatches"].([]any)) != 1 || len(query["semanticMatches"].([]any)) != 0 {
		t.Fatalf("query channels broken: %v", query)
	}

	// A second confirm under a different command is a 409; the model
	// never publishes on its own.
	status, _ = f.do(t, http.MethodPost, path+"/confirm", `{"clientCommandId":"http-confirm-2","expectedRevision":1}`)
	if status != http.StatusConflict {
		t.Fatalf("second confirm status = %d", status)
	}
}

func TestFeedbackValidationThroughHTTP(t *testing.T) {
	f := newHTTPFixture(t)
	f.seedUser(t)
	status, _ := f.do(t, http.MethodPost, "/api/v1/knowledge/feedback", `{"clientCommandId":"http-fb-1","targetType":"investigation_message","targetId":"9999","value":"adopted"}`)
	if status != http.StatusNotFound {
		t.Fatalf("missing target status = %d", status)
	}
	// Unknown session is rejected server-side, not by hiding buttons.
	request := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/candidates", strings.NewReader(""))
	request.AddCookie(&http.Cookie{Name: "__Host-quoin-session", Value: "other"})
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", recorder.Code)
	}
}
