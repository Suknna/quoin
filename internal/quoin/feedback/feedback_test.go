package feedback

// Feedback ledger tests over the real frozen schema: append-only timeline,
// closed target shape, durable command replay, and the DATA-TX-011
// rejected-source invalidation (SourceInvalid candidates, permanently
// exited retrieval, FTS projection removal).

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
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
	_ "modernc.org/sqlite"
)

// newTestDB 打开 fixture 写库并在同一文件上建立真实只读池（与生产组合一致：
// execution.OpenReadOnly 的 mode=ro + query_only，可写池绝不充当读源）。
// 只读池由测试持有并在结束时关闭。
func newTestDB(t *testing.T) (*sql.DB, execution.Reader) {
	t.Helper()
	dbPath := t.TempDir() + "/test.db"
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	reader, err := execution.OpenReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	return db, reader
}

type fixture struct {
	db       *sql.DB
	service  *Service
	userID   int64
	outputID int64
	msgID    int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, reader := newTestDB(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	user, err := db.Exec(`INSERT INTO users(username,display_name,role,enabled,initialized,password_phc,auth_revision,created_at,updated_at) VALUES('op','Op','operator',1,1,'x',1,?,?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	userID, _ := user.LastInsertId()
	// 会话证明：fixture 用户在签发 revision 1 的有效会话（远期过期）。
	// 会话复核用例再单独改变撤销或漂移事实。
	if _, err := db.Exec(`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,?,randomblob(32),1,'fixture',?,?,?,?)`,
		userID, now, now, "2036-09-15T00:00:00Z", "2036-09-22T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	connectionID, revisionID, generationID, probeResultID := seedProviderChain(t, db, userID)
	// One firing occurrence plus its sealed Initial Analysis output.
	source, err := db.Exec(`INSERT INTO alert_sources(source_key,protocol,enabled,created_at) VALUES('fb-src','alertmanager',1,?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, _ := source.LastInsertId()
	labels := `{"alertname":"FbProbe","severity":"warning"}`
	occurrence, err := db.Exec(`INSERT INTO alert_occurrences(source_id,fingerprint,starts_at,state,labels_canonical,labels_digest,first_seen_at,last_state_change_at) VALUES(?,x'0000000000000001',?,'Firing',?,?,?,?)`,
		sourceID, now, labels, sha256HexFor(labels), now, now)
	if err != nil {
		t.Fatal(err)
	}
	occurrenceID, _ := occurrence.LastInsertId()
	analysis, err := db.Exec(`INSERT INTO initial_analyses(occurrence_id,state,input_snapshot_digest,created_at) VALUES(?,'Queued',?,?)`, occurrenceID, strings.Repeat("3", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	analysisID, _ := analysis.LastInsertId()
	attemptID := seedChatAttempt(t, db, "initial_analysis", "analysis", "initial_analysis_v1", analysisID, connectionID, revisionID, generationID, probeResultID)
	output, err := db.Exec(`INSERT INTO initial_analysis_outputs(analysis_id,attempt_id,model_id,content,created_at) VALUES(?,?,'fixture-chat','数据库连接池耗尽导致超时。',?)`, analysisID, attemptID, now)
	if err != nil {
		t.Fatal(err)
	}
	outputID, _ := output.LastInsertId()
	for _, state := range []string{"Running", "Succeeded"} {
		if _, err := db.Exec(`UPDATE initial_analyses SET state=?, row_version=row_version+1 WHERE id=?`, state, analysisID); err != nil {
			t.Fatal(err)
		}
	}
	// One investigation: its single attempt carries the user message while
	// Queued and the assistant message while Running (DATA-INVEST-001).
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
	if _, err := db.Exec(`INSERT INTO investigation_messages(investigation_id,attempt_id,seq,role,status,content,created_at) VALUES(?,?,2,'user','active','好的。',?)`, investigationID, turnAttemptID, now); err != nil {
		t.Fatal(err)
	}
	seedChatAttemptFrom(t, db, "investigation", "investigation", "investigation_v1", investigationID, turnAttemptID, connectionID, revisionID, generationID, probeResultID)
	assistant, err := db.Exec(`INSERT INTO investigation_messages(investigation_id,attempt_id,seq,role,status,content,created_at) VALUES(?,?,1,'assistant','active','建议检查连接池配置。',?)`, investigationID, turnAttemptID, now)
	if err != nil {
		t.Fatal(err)
	}
	msgID, _ := assistant.LastInsertId()
	// 与生产组合一致（app.configureReadOnly）：真实只读池经共享执行器验证后
	// 装配服务；可写池绝不充当读源。
	runner := execution.NewRunner(db, execution.NewRegistry(), nil)
	service, err := NewServiceWithReader(reader, runner)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{db: db, service: service, userID: userID, outputID: outputID, msgID: msgID}
}

// ctx 注入带会话证明引用的合法执行元数据：真实入口（HTTP 准入）统一接线
// 前，由测试充当可信入口，主体与会话和 fixture 数据一致。
func (f *fixture) ctx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "corr-fb-" + strconv.FormatInt(f.userID, 10),
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: f.userID},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-fb"},
		Session:       execution.SessionRef{ID: 1, AuthRevision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// seedChatAttempt walks one model-driven attempt from Queued to Running
// with the frozen input snapshot, chat_model grant and agent binding the
// dispatch-ready trigger demands. userMessageID adds the investigation
// user-message input item (0 for the analysis variant).
func seedChatAttempt(t *testing.T, db *sql.DB, attemptType, scopeType, schemaKind string, scopeID, connectionID, revisionID, generationID, probeResultID int64) int64 {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	attempt, err := db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,created_at) VALUES(?,?,?,'Queued','test',?)`, attemptType, scopeType, scopeID, now)
	if err != nil {
		t.Fatal(err)
	}
	attemptID, _ := attempt.LastInsertId()
	seedChatAttemptFrom(t, db, attemptType, scopeType, schemaKind, scopeID, attemptID, connectionID, revisionID, generationID, probeResultID)
	return attemptID
}

// seedChatAttemptFrom freezes the input snapshot, items and chat_model
// grant on an existing Queued attempt, then walks it to Running.
func seedChatAttemptFrom(t *testing.T, db *sql.DB, attemptType, scopeType, schemaKind string, scopeID, attemptID, connectionID, revisionID, generationID, probeResultID int64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	snapshot, err := db.Exec(`INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(?,?,'fixture-renderer',?,?)`, attemptID, schemaKind, strings.Repeat("4", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, _ := snapshot.LastInsertId()
	var messageCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM investigation_messages WHERE attempt_id=?`, attemptID).Scan(&messageCount); err != nil {
		t.Fatal(err)
	}
	if messageCount > 0 {
		if _, err := db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,investigation_message_id) VALUES(?,1,'user',?,(SELECT id FROM investigation_messages WHERE attempt_id=? AND role='user' LIMIT 1))`, snapshotID, strings.Repeat("4", 64), attemptID); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,initial_analysis_id) VALUES(?,1,'user',?,?)`, snapshotID, strings.Repeat("4", 64), scopeID); err != nil {
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

// seedProviderChain inserts one enabled qualified model provider with a
// passed probe result (the analysis-test precedent).
func seedProviderChain(t *testing.T, db *sql.DB, actorID int64) (connectionID, revisionID, generationID, probeResultID int64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT OR IGNORE INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`, []byte(strings.Repeat("e", 12)), []byte(strings.Repeat("f", 16)), now); err != nil {
		t.Fatal(err)
	}
	connection, err := db.Exec(`INSERT INTO connections(name,type,enabled,created_at) VALUES('fb-provider','model_provider',0,?)`, now)
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
		nonce[i] = byte(i * 7)
	}
	generation, err := db.Exec(`INSERT INTO credential_generations(connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_at) VALUES(?,1,1,1,?,?,?)`, connectionID, nonce, []byte(strings.Repeat("f", 32)), now)
	if err != nil {
		t.Fatal(err)
	}
	generationID, _ = generation.LastInsertId()
	if _, err := db.Exec(`UPDATE connections SET current_revision_id=?, current_credential_generation_id=?, row_version=row_version+1 WHERE id=?`, revisionID, generationID, connectionID); err != nil {
		t.Fatal(err)
	}
	// Register the plinth slot so chat attempts may dispatch (the analysis
	// test precedent).
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

// seedKnowledge inserts one candidate (state given) for the given target
// and, for Confirmed, its knowledge/version/retrieval/search-doc chain.
func (f *fixture) seedKnowledge(t *testing.T, targetType string, targetID int64, candidateState string) (candidateID, knowledgeID, versionID int64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	candidate, err := f.db.Exec(`INSERT INTO knowledge_candidates(source_type,source_id,generation,state,original_suggestion_json,draft_title,draft_body,draft_revision,created_by,created_at) VALUES(?,?,1,?,?,'数据库连接池耗尽','数据库连接池耗尽导致超时。',0,?,?)`,
		targetType, targetID, candidateState, `{"v":1}`, f.userID, now)
	if err != nil {
		t.Fatal(err)
	}
	candidateID, _ = candidate.LastInsertId()
	if candidateState != "Confirmed" {
		return candidateID, 0, 0
	}
	knowledge, err := f.db.Exec(`INSERT INTO reusable_knowledge(created_by,created_at) VALUES(?,?)`, f.userID, now)
	if err != nil {
		t.Fatal(err)
	}
	knowledgeID, _ = knowledge.LastInsertId()
	if _, err := f.db.Exec(`UPDATE knowledge_candidates SET confirmed_knowledge_id=?, row_version=row_version+1 WHERE id=?`, knowledgeID, candidateID); err != nil {
		t.Fatal(err)
	}
	version, err := f.db.Exec(`INSERT INTO knowledge_versions(knowledge_id,version_seq,title,body,source_candidate_id,created_by,created_at) VALUES(?,1,'数据库连接池耗尽','数据库连接池耗尽导致超时。',?,?,?)`, knowledgeID, candidateID, f.userID, now)
	if err != nil {
		t.Fatal(err)
	}
	versionID, _ = version.LastInsertId()
	if _, err := f.db.Exec(`UPDATE reusable_knowledge SET current_version_id=?, row_version=row_version+1 WHERE id=?`, versionID, knowledgeID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO knowledge_version_retrieval_state(knowledge_version_id,updated_at) VALUES(?,?)`, versionID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO knowledge_search_docs(knowledge_version_id,title,body) VALUES(?,'数据库连接池耗尽','数据库连接池耗尽导致超时。')`, versionID); err != nil {
		t.Fatal(err)
	}
	return candidateID, knowledgeID, versionID
}

func sha256HexFor(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
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

func parseTestID(value string) int64 {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return -1
	}
	return id
}

func TestAppendRecordsTimelineAndLatestValue(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx(t)
	first, err := f.service.Append(ctx, f.userID, "cmd-feedback-1", Target{Type: TargetAnalysisOutput, ID: f.outputID}, ValueAdopted, "看起来对症")
	if err != nil {
		t.Fatal(err)
	}
	if first.Value != ValueAdopted || first.Note != "看起来对症" || first.ID == "" {
		t.Fatalf("unexpected first event: %+v", first)
	}
	second, err := f.service.Append(ctx, f.userID, "cmd-feedback-2", Target{Type: TargetAnalysisOutput, ID: f.outputID}, ValueVerifiedEffective, "")
	if err != nil {
		t.Fatal(err)
	}
	if parseTestID(second.ID) <= parseTestID(first.ID) {
		t.Fatalf("event ids must advance: %s then %s", first.ID, second.ID)
	}
	// The timeline keeps the full history; latestValue is the projection of
	// the last committed event and never rewrites history.
	page, err := f.service.List(ctx, Target{Type: TargetAnalysisOutput, ID: f.outputID}, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if page.LatestValue != ValueVerifiedEffective {
		t.Fatalf("latestValue = %q", page.LatestValue)
	}
	if len(page.Items) != 2 || page.Items[0].Value != ValueVerifiedEffective || page.Items[1].Value != ValueAdopted {
		t.Fatalf("timeline order broken: %+v", page.Items)
	}
}

func TestAppendRejectsInvalidTargetsAndValues(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx(t)
	// A user message is not one of the three immutable diagnosis outputs.
	var userMessageID int64
	if err := f.db.QueryRow(`SELECT id FROM investigation_messages WHERE role='user'`).Scan(&userMessageID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Append(ctx, f.userID, "cmd-bad-1", Target{Type: TargetMessage, ID: userMessageID}, ValueAdopted, ""); err != ErrInvalidTarget {
		t.Fatalf("user message must be invalid: %v", err)
	}
	if _, err := f.service.Append(ctx, f.userID, "cmd-bad-2", Target{Type: TargetAnalysisOutput, ID: 9999}, ValueAdopted, ""); err != ErrNotFound {
		t.Fatalf("missing target must be 404: %v", err)
	}
	if _, err := f.service.Append(ctx, f.userID, "cmd-bad-3", Target{Type: TargetReport, ID: 9999}, "maybe", ""); err != ErrInvalidValue {
		t.Fatalf("closed value set violated: %v", err)
	}
	if _, err := f.service.Append(ctx, f.userID, "cmd-bad-4", Target{Type: "investigation", ID: 1}, ValueAdopted, ""); err != ErrInvalidTarget {
		t.Fatalf("unknown target type accepted: %v", err)
	}
	long := make([]byte, 4097)
	for i := range long {
		long[i] = 'x'
	}
	if _, err := f.service.Append(ctx, f.userID, "cmd-bad-5", Target{Type: TargetAnalysisOutput, ID: f.outputID}, ValueAdopted, string(long)); err != ErrNoteTooLong {
		t.Fatalf("note limit not enforced: %v", err)
	}
}

func TestAppendCommandReplayIsDeterministic(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx(t)
	target := Target{Type: TargetMessage, ID: f.msgID}
	first, err := f.service.Append(ctx, f.userID, "cmd-replay-1", target, ValueExecuted, "已执行")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := f.service.Append(ctx, f.userID, "cmd-replay-1", target, ValueExecuted, "已执行")
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != first.ID {
		t.Fatalf("replay returned a different event: %s vs %s", replayed.ID, first.ID)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM diagnosis_feedback WHERE target_type=? AND target_id=?`, target.Type, target.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("replay duplicated the event: %d", count)
	}
	// Same id, different semantics → deterministic conflict.
	if _, err := f.service.Append(ctx, f.userID, "cmd-replay-1", target, ValueAdopted, ""); err != ErrCommandReused {
		t.Fatalf("reused command id accepted: %v", err)
	}
}

func TestRejectedInvalidatesCandidatesAndRetrieval(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx(t)
	// Source A (the analysis output) carries a Confirmed candidate with a
	// live retrieval doc; source B (the assistant message) carries an
	// AwaitingConfirmation candidate.
	_, confirmedK, confirmedV := f.seedKnowledge(t, TargetAnalysisOutput, f.outputID, "Confirmed")
	awaitingID, _, _ := f.seedKnowledge(t, TargetMessage, f.msgID, "AwaitingConfirmation")
	outputTarget := Target{Type: TargetAnalysisOutput, ID: f.outputID}
	messageTarget := Target{Type: TargetMessage, ID: f.msgID}
	if _, err := f.service.Append(ctx, f.userID, "cmd-reject-a", outputTarget, ValueRejected, "结论与现场不符"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Append(ctx, f.userID, "cmd-reject-b", messageTarget, ValueRejected, ""); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.db.QueryRow(`SELECT state FROM knowledge_candidates WHERE id=?`, awaitingID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "SourceInvalid" {
		t.Fatalf("awaiting candidate state = %q", state)
	}
	var exited int
	var reason string
	if err := f.db.QueryRow(`SELECT exited, exit_reason FROM knowledge_version_retrieval_state WHERE knowledge_version_id=?`, confirmedV).Scan(&exited, &reason); err != nil {
		t.Fatal(err)
	}
	if exited != 1 || reason != "source_rejected" {
		t.Fatalf("retrieval exit = %d/%q", exited, reason)
	}
	var docs int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM knowledge_search_docs WHERE knowledge_version_id=?`, confirmedV).Scan(&docs); err != nil {
		t.Fatal(err)
	}
	if docs != 0 {
		t.Fatalf("search doc survived rejection: %d", docs)
	}
	var ftsCount int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM knowledge_fts`).Scan(&ftsCount); err != nil {
		t.Fatal(err)
	}
	if ftsCount != 0 {
		t.Fatalf("FTS row survived rejection: %d", ftsCount)
	}
	// A second rejected event stays append-only and idempotent on effects.
	if _, err := f.service.Append(ctx, f.userID, "cmd-reject-a2", outputTarget, ValueRejected, ""); err != nil {
		t.Fatal(err)
	}
	var events int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM diagnosis_feedback WHERE target_type=? AND target_id=?`, outputTarget.Type, outputTarget.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 2 {
		t.Fatalf("append-only timeline broken: %d", events)
	}
	var versionRowVersion int
	if err := f.db.QueryRow(`SELECT row_version FROM knowledge_version_retrieval_state WHERE knowledge_version_id=?`, confirmedV).Scan(&versionRowVersion); err != nil {
		t.Fatal(err)
	}
	if versionRowVersion != 2 {
		t.Fatalf("second rejection must not bump exit row again: %d", versionRowVersion)
	}
	// Confirmed candidate stays Confirmed (only retrieval retired).
	var confirmedState string
	if err := f.db.QueryRow(`SELECT state FROM knowledge_candidates WHERE confirmed_knowledge_id=?`, confirmedK).Scan(&confirmedState); err != nil {
		t.Fatal(err)
	}
	if confirmedState != "Confirmed" {
		t.Fatalf("confirmed candidate state = %q", confirmedState)
	}
}

func TestTimelinePaginationKeyset(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx(t)
	target := Target{Type: TargetAnalysisOutput, ID: f.outputID}
	for index := 0; index < 5; index++ {
		if _, err := f.service.Append(ctx, f.userID, fmt.Sprintf("cmd-page-%d", index), target, ValueAdopted, fmt.Sprintf("note-%d", index)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := f.service.List(ctx, target, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.LatestValue != ValueAdopted || first.Next == nil {
		t.Fatalf("first page broken: %+v", first)
	}
	if first.Items[0].Note != "note-4" || first.Items[1].Note != "note-3" {
		t.Fatalf("first page order: %+v", first.Items)
	}
	second, err := f.service.List(ctx, target, first.Next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 2 || second.Items[0].Note != "note-2" || second.Items[1].Note != "note-1" {
		t.Fatalf("second page order: %+v", second.Items)
	}
	if second.LatestValue != "" {
		t.Fatalf("latestValue is a first-page projection only: %q", second.LatestValue)
	}
}

func TestEventJSONShape(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx(t)
	event, err := f.service.Append(ctx, f.userID, "cmd-json-1", Target{Type: TargetMessage, ID: f.msgID}, ValueAdopted, "备注")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "targetType", "targetId", "value", "note", "createdBy", "createdAt"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("event json missing %q: %s", key, body)
		}
	}
}

// 自动审计：追加命令同事务产生一条 execute 审计与目标引用；幂等重放返回
// 原事件且不新增审计；换载荷重用命令键确定性冲突且不留新痕迹。
func TestAppendAutomaticAuditAndReplay(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx(t)
	target := Target{Type: TargetAnalysisOutput, ID: f.outputID}
	event, err := f.service.Append(ctx, f.userID, "cmd-audit-1", target, ValueAdopted, "看起来对症")
	if err != nil {
		t.Fatal(err)
	}
	var (
		action, outcome, phase, commandID, correlation string
		refType                                        string
		actorID, refID                                 int64
	)
	if err := f.db.QueryRow(`SELECT action,outcome,phase,client_command_id,correlation_id,domain_ref_type,domain_ref_id,actor_id FROM audit_events`).Scan(
		&action, &outcome, &phase, &commandID, &correlation, &refType, &refID, &actorID); err != nil {
		t.Fatal(err)
	}
	if action != CommandAppend || outcome != "success" || phase != "execute" || commandID != "cmd-audit-1" ||
		correlation != "corr-fb-"+strconv.FormatInt(f.userID, 10) || refType != ObjectFeedbackEvent ||
		refID != parseTestID(event.ID) || actorID != f.userID {
		t.Fatalf("audit row = action=%s outcome=%s phase=%s cmd=%s corr=%s ref=%s/%d actor=%d",
			action, outcome, phase, commandID, correlation, refType, refID, actorID)
	}
	if got := countTestRows(t, f.db, `SELECT COUNT(*) FROM audit_event_targets WHERE target_type=? AND target_id=?`, ObjectFeedbackEvent, parseTestID(event.ID)); got != 1 {
		t.Fatalf("audit targets = %d, want 1", got)
	}

	// 幂等重放：同一命令键返回原事件，不新增审计。
	replayed, err := f.service.Append(ctx, f.userID, "cmd-audit-1", target, ValueAdopted, "看起来对症")
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != event.ID {
		t.Fatalf("replay returned a different event: %s vs %s", replayed.ID, event.ID)
	}
	if got := countTestRows(t, f.db, `SELECT COUNT(*) FROM audit_events`); got != 1 {
		t.Fatalf("audit events after replay = %d, want 1", got)
	}

	// 命令键重用（同 ID 不同请求）：确定性冲突，不新增审计。
	if _, err := f.service.Append(ctx, f.userID, "cmd-audit-1", target, ValueExecuted, ""); err != ErrCommandReused {
		t.Fatalf("reused command id must surface ErrCommandReused, got %v", err)
	}
	if got := countTestRows(t, f.db, `SELECT COUNT(*) FROM audit_events`); got != 1 {
		t.Fatalf("audit events after reuse conflict = %d, want 1", got)
	}
}

// 确定性拒绝持久化：缺失目标的拒绝事实进入台账与审计，业务行不落库；
// 重放同一拒绝命令返回同一哨兵错误且不重复审计。
func TestAppendMissingTargetRejectionIsDurable(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx(t)
	missing := Target{Type: TargetAnalysisOutput, ID: 9999}
	if _, err := f.service.Append(ctx, f.userID, "cmd-missing-1", missing, ValueAdopted, ""); err != ErrNotFound {
		t.Fatalf("missing target must be 404: %v", err)
	}
	var ledgerOutcome string
	if err := f.db.QueryRow(`SELECT outcome FROM client_commands WHERE client_command_id='cmd-missing-1'`).Scan(&ledgerOutcome); err != nil {
		t.Fatal(err)
	}
	if ledgerOutcome != "rejected_known" {
		t.Fatalf("rejection ledger outcome = %q", ledgerOutcome)
	}
	if got := countTestRows(t, f.db, `SELECT COUNT(*) FROM audit_events WHERE outcome='rejected'`); got != 1 {
		t.Fatalf("rejected audit events = %d, want 1", got)
	}
	if got := countTestRows(t, f.db, `SELECT COUNT(*) FROM diagnosis_feedback`); got != 0 {
		t.Fatalf("rejected append persisted an event: %d", got)
	}
	// 重放拒绝：同一哨兵错误，不再新增审计行。
	if _, err := f.service.Append(ctx, f.userID, "cmd-missing-1", missing, ValueAdopted, ""); err != ErrNotFound {
		t.Fatalf("replayed rejection must stay 404: %v", err)
	}
	if got := countTestRows(t, f.db, `SELECT COUNT(*) FROM audit_events`); got != 1 {
		t.Fatalf("audit events after rejection replay = %d, want 1", got)
	}
}

// 会话复核：会话证明引用缺失（伪造或入口未携带）不得退化为仅查 users 行；
// 会话撤销后即使准入通过也在事务内复核失败。两类失败都干净无痕。
func TestAppendSessionProofIsVerifiedInTransaction(t *testing.T) {
	f := newFixture(t)
	noSession, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "corr-fb-nosession",
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: f.userID},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-fb-nosession"},
	})
	if err != nil {
		t.Fatal(err)
	}
	target := Target{Type: TargetAnalysisOutput, ID: f.outputID}
	if _, err := f.service.Append(noSession, f.userID, "cmd-nosession-1", target, ValueAdopted, ""); !errors.Is(err, auth.ErrActorChanged) {
		t.Fatalf("missing session proof must surface ErrActorChanged, got %v", err)
	}
	if _, err := f.db.Exec(`UPDATE sessions SET revoked_at=datetime('now') WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Append(f.ctx(t), f.userID, "cmd-revoked-1", target, ValueAdopted, ""); !errors.Is(err, auth.ErrActorChanged) {
		t.Fatalf("revoked session must surface ErrActorChanged, got %v", err)
	}
	for name, query := range map[string]string{
		"diagnosis_feedback": `SELECT COUNT(*) FROM diagnosis_feedback`,
		"client_commands":    `SELECT COUNT(*) FROM client_commands`,
		"audit_events":       `SELECT COUNT(*) FROM audit_events`,
	} {
		if got := countTestRows(t, f.db, query); got != 0 {
			t.Fatalf("%s rows after session rejection = %d, want 0", name, got)
		}
	}
}

func countTestRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var count int
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// writableAdapter 是任意实现 audit.Reader 的适配器：包着可写连接的读形状，
// 正是注入边界必须结构性拒绝的第二条写通道。
type writableAdapter struct{ db *sql.DB }

func (a writableAdapter) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return a.db.QueryContext(ctx, query, args...)
}

func (a writableAdapter) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return a.db.QueryRowContext(ctx, query, args...)
}

// 默认构造（应用初始装配，configureReadOnly 之前）绝不把可写 db 当作读源：
// 读路径保持零值可信只读面，所有读取失败关闭；构造本身不 panic。
func TestDefaultServiceReadsFailClosed(t *testing.T) {
	db, _ := newTestDB(t)
	service := NewService(db)
	if _, err := service.List(context.Background(), Target{Type: TargetAnalysisOutput, ID: 1}, nil, 10); err == nil {
		t.Fatal("unwired default service must fail closed on reads")
	} else if !strings.Contains(err.Error(), "no read-only reader") {
		t.Fatalf("read failure must report the missing read-only reader, got %v", err)
	}
}

// 读能力注入只接受 execution.OpenReadOnly 产生的可信类型：裸可写 db、任意
// audit.Reader 适配器与 nil 一并被拒绝，不存在可写兼容通道；可信读源装配
// 后读路径真实可用。
func TestReaderInjectionAcceptsOnlyTrustedReadOnly(t *testing.T) {
	db, reader := newTestDB(t)
	runner := execution.NewRunner(db, execution.NewRegistry(), nil)
	if _, err := NewServiceWithReader(db, runner); err == nil {
		t.Fatal("raw writable database must not be accepted as a reader")
	}
	if _, err := NewServiceWithReader(nil, runner); err == nil {
		t.Fatal("nil reader must be rejected")
	}
	if _, err := NewServiceWithReader(writableAdapter{db: db}, runner); err == nil {
		t.Fatal("arbitrary audit.Reader adapters must not be accepted as a reader")
	}
	if _, err := NewServiceWithReader(reader, runner); err != nil {
		t.Fatalf("trusted OpenReadOnly reader must be accepted: %v", err)
	}
}

// SetReader 把真实只读能力转发给共享执行器验证并采纳其读面：默认构造的
// 服务注入前读失败关闭，注入后读路径真实可用；可写 db 仍被拒绝。
func TestSetReaderWiresReadOnlyCapability(t *testing.T) {
	db, reader := newTestDB(t)
	service := NewService(db)
	if err := service.SetReader(db); err == nil {
		t.Fatal("writable db must not be accepted as reader")
	}
	target := Target{Type: TargetAnalysisOutput, ID: 1}
	if _, err := service.List(context.Background(), target, nil, 10); err == nil {
		t.Fatal("service must stay fail closed before the reader is wired")
	}
	if err := service.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	page, err := service.List(context.Background(), target, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("empty timeline expected, got %+v", page.Items)
	}
}
