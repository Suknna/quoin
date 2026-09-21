package analysis

// Analysis aggregate tests over the real frozen schema: create (one-active
// invariant, command replay), dispatch/accept, result seal with the model
// call closure, retry and the cancel-vs-success commit-order races
// (DATA-ANALYSIS-001/002, DATA-TX-005).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	_ "github.com/Suknna/quoin/internal/plugins/builtin"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/execution"
	_ "modernc.org/sqlite"
)

// commandContext returns a background context carrying execution metadata,
// mirroring the admission layer: attempt creators centrally persist this
// metadata onto the new row (ADR-0006) and fail closed without it.
func commandContext(t *testing.T) context.Context {
	t.Helper()
	correlationID, err := execution.NewCorrelationID()
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: correlationID,
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: testOperatorID},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "test-req"},
		Session:       execution.SessionRef{ID: testOperatorID, AuthRevision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// fixtureDBPath records the newest fixture database file so seed helpers deep
// in a call chain can open the read-only factory over the same file. Tests in
// this package run sequentially (no t.Parallel), so one variable is safe.
var fixtureDBPath string

func newTestDB(t *testing.T) (*sql.DB, string) {
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
	seedPrincipals(t, db)
	fixtureDBPath = dbPath
	return db, dbPath
}

// newTestService composes the production read seam onto NewService: a real
// query_only reader over the same fixture file, opened through the
// execution.OpenReadOnly factory and validated by runner.SetReader's probe.
// A second writable handle is never an acceptable reader.
func newTestService(t *testing.T, db *sql.DB, dbPath string) *Service {
	t.Helper()
	service := NewService(db)
	reader, err := execution.OpenReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	if err := service.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	return service
}

// The fixed fixture principals: the single initialized admin (id 1) and the
// initialized operator (id 2) with one real, unexpired session each — the
// same closure ladder the admission middleware drives before any command.
const (
	testAdminID    = int64(1)
	testOperatorID = int64(2)
)

func seedPrincipals(t *testing.T, db *sql.DB) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	idle, absolute := "2036-09-13T00:00:00Z", "2036-09-20T00:00:00Z"
	for _, statement := range []string{
		`INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,auth_revision,row_version,created_at,updated_at) VALUES(1,'fixture-admin','Fixture Admin','admin',1,1,'fixture',1,1,'` + now + `','` + now + `')`,
		`INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,auth_revision,row_version,created_at,updated_at) VALUES(2,'fixture-operator','Fixture Operator','operator',1,1,'fixture',1,1,'` + now + `','` + now + `')`,
		`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,1,randomblob(32),1,'fixture','` + now + `','` + now + `','` + idle + `','` + absolute + `')`,
		`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(2,2,randomblob(32),1,'fixture','` + now + `','` + now + `','` + idle + `','` + absolute + `')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

var seedCounter int

// seedOccurrence inserts one firing alert occurrence with the ADR-0012
// normalized semantics frozen on the row (severity/title/annotations/
// resource). Analysis creation must not infer a global metrics connection;
// authority is always the frozen integration list.
func seedOccurrence(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	seedCounter++
	now := time.Now().UTC().Format(time.RFC3339Nano)
	source, err := db.Exec(`INSERT INTO alert_sources(source_key,protocol,enabled,created_at) VALUES(?,'alertmanager',1,?)`, fmt.Sprintf("source-%d", seedCounter), now)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, _ := source.LastInsertId()
	labels := `{"alertname":"HighErrorRate","severity":"critical"}`
	occurrence, err := db.Exec(`INSERT INTO alert_occurrences(source_id,fingerprint,starts_at,state,labels_canonical,labels_digest,severity,title,annotations_canonical,resource,first_seen_at,last_state_change_at) VALUES(?,?,?,'Firing',?,?,?,'HighErrorRate','{}','',?,?)`,
		sourceID, []byte{0, 0, 0, 0, 0, 0, byte(seedCounter % 8), 1}, now, labels, sha256Hex(labels), "critical", now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := occurrence.LastInsertId()
	return id
}

// seedProviderChain inserts one enabled qualified model provider.
func seedProviderChain(t *testing.T, db *sql.DB) (connectionID, revisionID, generationID, probeResultID int64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var existingConnectionID, existingRevisionID, existingGenerationID, existingProbeID int64
	if err := db.QueryRow(`SELECT c.id, c.current_revision_id, c.current_credential_generation_id, (SELECT probe_result_id FROM connection_enable_qualifications q WHERE q.connection_id=c.id ORDER BY q.id DESC LIMIT 1) FROM connections c WHERE c.type='model_provider' AND c.enabled=1`).Scan(&existingConnectionID, &existingRevisionID, &existingGenerationID, &existingProbeID); err == nil {
		return existingConnectionID, existingRevisionID, existingGenerationID, existingProbeID
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`, []byte(strings.Repeat("e", 12)), []byte(strings.Repeat("f", 16)), now); err != nil {
		t.Fatal(err)
	}
	var actorID int64
	err := db.QueryRow(`SELECT id FROM users LIMIT 1`).Scan(&actorID)
	if err != nil {
		user, err := db.Exec(`INSERT INTO users(username,display_name,role,enabled,password_phc,auth_revision,created_at,updated_at) VALUES('test-admin','Test Admin','admin',1,'x',1,?,?)`, now, now)
		if err != nil {
			t.Fatal(err)
		}
		actorID, _ = user.LastInsertId()
	}
	connection, err := db.Exec(`INSERT INTO connections(name,type,enabled,created_at) VALUES(?,'model_provider',0,?)`, fmt.Sprintf("provider-%d", seedCounter), now)
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
		nonce[i] = byte(seedCounter*31 + i)
	}
	generation, err := db.Exec(`INSERT INTO credential_generations(connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_at) VALUES(?,1,1,1,?,?,?)`, connectionID, nonce, []byte(strings.Repeat("f", 32)), now)
	if err != nil {
		t.Fatal(err)
	}
	generationID, _ = generation.LastInsertId()
	if _, err := db.Exec(`UPDATE connections SET current_revision_id=?, current_credential_generation_id=?, row_version=row_version+1 WHERE id=?`, revisionID, generationID, connectionID); err != nil {
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
	if _, err := db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id) VALUES(?,1,'user',?,?)`, probeSnapshotID, strings.Repeat("0", 64), revisionID); err != nil {
		t.Fatal(err)
	}
	var probeChatGrantID, probeEmbeddingGrantID int64
	for _, purpose := range []string{"model_probe_chat", "model_probe_embedding"} {
		grant, err := db.Exec(`INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at) VALUES(?,?,?,?,?,?)`, probeAttemptID, purpose, connectionID, revisionID, generationID, now)
		if err != nil {
			t.Fatal(err)
		}
		if purpose == "model_probe_chat" {
			probeChatGrantID, _ = grant.LastInsertId()
		} else {
			probeEmbeddingGrantID, _ = grant.LastInsertId()
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
	seedModelCall(t, db, probeAttemptID, probeChatGrantID, 1, "fixture-chat-1", now, true)
	seedModelCall(t, db, probeAttemptID, probeChatGrantID, 4, "fixture-chat-1", now, false)
	seedEmbeddingCall(t, db, probeAttemptID, probeEmbeddingGrantID, probeSnapshotID, now)
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

func seedModelCall(t *testing.T, db *sql.DB, attemptID, grantID int64, callSeq int, modelID, now string, succeeded bool) int64 {
	t.Helper()
	digest := strings.Repeat("1", 64)
	call, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,evicted_turn_count,status,started_at) VALUES(?,?,'0','chat',?,?,'connection-probe-v1','probe-supervisor-v1',?,?,?,?,?,4096,1024,0,0,'running',?)`,
		attemptID, callSeq, modelID, grantID, digest, digest, digest, digest, digest, now)
	if err != nil {
		t.Fatal(err)
	}
	callID, _ := call.LastInsertId()
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,1,'system',?,'system_contract'),(?,2,'system',?,'tool_schema')`, callID, digest, callID, digest); err != nil {
		t.Fatal(err)
	}
	if succeeded {
		if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,1,'{"assistantText":"ok","finishReason":"stop","tool_calls":[]}',?, 'stop',?)`, callID, digest, now); err != nil {
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
	return callID
}

func seedEmbeddingCall(t *testing.T, db *sql.DB, attemptID, grantID, snapshotID int64, now string) {
	t.Helper()
	digest := strings.Repeat("1", 64)
	call, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,input_snapshot_digest,rendered_request_digest,estimated_input_tokens,status,started_at) VALUES(?,'6','0','embedding','fixture-embed-1',?,?,?,0,'running',?)`,
		attemptID, grantID, digest, digest, now)
	if err != nil {
		t.Fatal(err)
	}
	callID, _ := call.LastInsertId()
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,attempt_input_snapshot_id) VALUES(?,1,'user',?,?)`, callID, digest, snapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,created_at) VALUES(?,1,'{}',?,?)`, callID, digest, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":0,"total_tokens":1}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
		t.Fatal(err)
	}
}

func sha256Hex(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// sealAgentCall inserts one succeeded chat call with tool-call closure for
// an agent attempt (the succeeded-attempt trigger requires it).
func sealAgentCall(t *testing.T, db *sql.DB, attemptID int64, modelID string) int64 {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var grantID int64
	if err := db.QueryRow(`SELECT id FROM attempt_connection_grants WHERE attempt_id=? AND purpose='chat_model'`, attemptID).Scan(&grantID); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("2", 64)
	call, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,evicted_turn_count,status,started_at) VALUES(?,'1','0','chat',?,?,'initial-analysis-renderer-v1','initial-analysis-v1',?,?,?,?,?,4096,1024,0,0,'running',?)`,
		attemptID, modelID, grantID, digest, digest, digest, digest, digest, now)
	if err != nil {
		t.Fatal(err)
	}
	callID, _ := call.LastInsertId()
	var snapshotID int64
	if err := db.QueryRow(`SELECT id FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&snapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,1,'system',?,'system_contract'),(?,2,'system',?,'tool_schema')`, callID, digest, callID, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,attempt_input_snapshot_id) VALUES(?,3,'system',?,?)`, callID, digest, snapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,1,'{"assistantText":"分析结论","finishReason":"stop","tool_calls":[]}',?, 'stop',?)`, callID, digest, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":10,"output_tokens":4,"total_tokens":14}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
		t.Fatal(err)
	}
	return callID
}

func TestTerminalFaultProjectionSharesAttemptCommitTransaction(t *testing.T) {
	db, dbPath := newTestDB(t)
	service := newTestService(t, db, dbPath)
	ctx := commandContext(t)
	occurrenceID := seedOccurrence(t, db)
	seedProviderChain(t, db)
	created, err := service.Create(ctx, occurrenceID, testOperatorID, "cmd-platform-fault")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Attempts().BindToStream(ctx, created.AttemptID, "boot-1", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.AcceptAttempt(context.Background(), created.AttemptID, "boot-1", 1); err != nil {
		t.Fatal(err)
	}
	projected := false
	service.ProjectTerminalOutcome = func(ctx context.Context, tx TxWriter, sequence int64, succeeded bool, termination string) error {
		projected = true
		if sequence <= 0 || succeeded || termination != "worker_protocol_error" {
			t.Fatalf("terminal projection=%d/%v/%q", sequence, succeeded, termination)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO platform_faults(component,reason,state,first_seen_at,last_seen_at,last_execution_commit_sequence) VALUES('plinth','worker_protocol_error','Firing','2026-09-10T00:00:00Z','2026-09-10T00:00:00Z',?)`, sequence)
		return err
	}
	if err := service.CommitResult(context.Background(), Result{AttemptID: created.AttemptID, BootID: "boot-1", Epoch: 1, Succeeded: false, Termination: "worker_protocol_error"}); err != nil {
		t.Fatal(err)
	}
	if !projected {
		t.Fatal("terminal transaction did not project worker failure")
	}
	var attemptState, faultState string
	if err := db.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, created.AttemptID).Scan(&attemptState); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT state FROM platform_faults WHERE reason='worker_protocol_error'`).Scan(&faultState); err != nil {
		t.Fatal(err)
	}
	if attemptState != "Failed" || faultState != "Firing" {
		t.Fatalf("attempt=%q fault=%q", attemptState, faultState)
	}
	// The terminal transition is audited automatically by the runner on the
	// restored task correlation (the attempt row's persisted association),
	// in the same commit as the fault projection.
	var auditCorrelation string
	if err := db.QueryRow(`SELECT correlation_id FROM audit_events WHERE action='initial_analysis.failed' AND outcome='success'`).Scan(&auditCorrelation); err != nil {
		t.Fatal(err)
	}
	var persistedCorrelation string
	if err := db.QueryRow(`SELECT operation_correlation_id FROM execution_attempts WHERE id=?`, created.AttemptID).Scan(&persistedCorrelation); err != nil {
		t.Fatal(err)
	}
	if auditCorrelation == "" || auditCorrelation != persistedCorrelation {
		t.Fatalf("terminal audit correlation=%q, want the persisted attempt correlation %q", auditCorrelation, persistedCorrelation)
	}
}

// seedAnnotatedOccurrence inserts one firing occurrence whose frozen
// annotation column carries the supplied map（ADR-0012：annotations 来自首观测
// 冻结列，不再从交付 body 现算）。
func seedAnnotatedOccurrence(t *testing.T, db *sql.DB, annotations map[string]string) int64 {
	t.Helper()
	seedCounter++
	now := time.Now().UTC().Format(time.RFC3339Nano)
	encoded, err := json.Marshal(annotations)
	if err != nil {
		t.Fatal(err)
	}
	source, err := db.Exec(`INSERT INTO alert_sources(source_key,protocol,enabled,created_at) VALUES(?,'alertmanager',1,?)`, fmt.Sprintf("source-annotated-%d", seedCounter), now)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, _ := source.LastInsertId()
	labels := `{"alertname":"HighErrorRate","severity":"critical"}`
	occurrence, err := db.Exec(`INSERT INTO alert_occurrences(source_id,fingerprint,starts_at,state,labels_canonical,labels_digest,severity,title,annotations_canonical,resource,first_seen_at,last_state_change_at) VALUES(?,?,?,'Firing',?,?,?,'HighErrorRate',?,'',?,?)`,
		sourceID, []byte{0, 0, 0, 0, 0, 0, byte(seedCounter % 8), 3}, now, labels, sha256Hex(labels), "critical", string(encoded), now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := occurrence.LastInsertId()
	return id
}

func TestRebuildInputPreservesFrozenAnnotations(t *testing.T) {
	for _, test := range []struct {
		name        string
		annotations map[string]string
		// want 是冻结列序列化后回读的期望形状：'{}' 按 omitempty 渲染为 nil。
		want map[string]string
	}{
		{name: "summary", annotations: map[string]string{"summary": "controlled GUI acceptance probe", "description": "No true fault; this is a controlled test annotation."}, want: map[string]string{"summary": "controlled GUI acceptance probe", "description": "No true fault; this is a controlled test annotation."}},
		{name: "empty", annotations: map[string]string{}, want: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, dbPath := newTestDB(t)
			service := newTestService(t, db, dbPath)
			occurrenceID := seedAnnotatedOccurrence(t, db, test.annotations)
			seedProviderChain(t, db)

			created, err := service.Create(commandContext(t), occurrenceID, testOperatorID, "cmd-annotations-"+test.name)
			if err != nil {
				t.Fatal(err)
			}
			canonical, err := service.RebuildInput(context.Background(), created.AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			// Dispatch rebuild is the production fence. It must reproduce the digest
			// frozen at admission including the frozen annotation column.
			if _, err := service.Attempts().DispatchInputFor(context.Background(), created.AttemptID); err != nil {
				t.Fatalf("annotation-bearing input must dispatch: %v", err)
			}
			var input Input
			if err := json.Unmarshal(canonical, &input); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(input.Occurrence.Annotations, test.want) {
				t.Fatalf("annotations = %#v, want exact frozen %#v", input.Occurrence.Annotations, test.want)
			}
			if input.Occurrence.Severity != "critical" || input.Occurrence.Title != "HighErrorRate" {
				t.Fatalf("normalized semantics = %q/%q", input.Occurrence.Severity, input.Occurrence.Title)
			}
		})
	}
}

// TestRebuildInputFreezesRelatedAlertWindow proves the ADR-0012 related-alert
// window: creation freezes the correlated/same-source 24h window as lineage
// items, and later arrivals outside the frozen window cannot drift the digest.
func TestRebuildInputFreezesRelatedAlertWindow(t *testing.T) {
	db, dbPath := newTestDB(t)
	service := newTestService(t, db, dbPath)
	occurrenceID := seedOccurrence(t, db)
	seedProviderChain(t, db)

	created, err := service.Create(commandContext(t), occurrenceID, testOperatorID, "cmd-related-window")
	if err != nil {
		t.Fatal(err)
	}
	before, err := service.RebuildInput(context.Background(), created.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	var input Input
	if err := json.Unmarshal(before, &input); err != nil {
		t.Fatal(err)
	}
	if len(input.Occurrence.RelatedAlerts) != 0 {
		t.Fatalf("isolated occurrence must freeze an empty window: %+v", input.Occurrence.RelatedAlerts)
	}
	// A later same-source occurrence is outside the frozen window (its
	// first_seen_at is now after the anchor's); the digest must not move.
	var sourceID int64
	if err := db.QueryRow(`SELECT source_id FROM alert_occurrences WHERE id=?`, occurrenceID).Scan(&sourceID); err != nil {
		t.Fatal(err)
	}
	late := time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano)
	labels := `{"alertname":"LaterAlert"}`
	if _, err := db.Exec(`INSERT INTO alert_occurrences(source_id,fingerprint,starts_at,state,labels_canonical,labels_digest,severity,title,annotations_canonical,resource,first_seen_at,last_state_change_at) VALUES(?,?,?,'Firing',?,?,?,'LaterAlert','{}','',?,?)`,
		sourceID, []byte{1, 2, 3, 4, 5, 6, 7, 8}, late, labels, sha256Hex(labels), "warning", late, late); err != nil {
		t.Fatal(err)
	}
	after, err := service.RebuildInput(context.Background(), created.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("frozen related-alert window drifted after a later arrival: %s vs %s", after, before)
	}
}

func TestCreateDispatchAcceptSeal(t *testing.T) {
	db, dbPath := newTestDB(t)
	service := newTestService(t, db, dbPath)
	ctx := commandContext(t)
	occurrenceID := seedOccurrence(t, db)
	seedProviderChain(t, db)

	created, err := service.Create(ctx, occurrenceID, testOperatorID, "cmd-create-1")
	if err != nil {
		t.Fatal(err)
	}
	// The one-active invariant: a second create returns the same record
	// (DATA-ANALYSIS-001).
	replayed, err := service.Create(ctx, occurrenceID, testOperatorID, "cmd-create-2")
	if err != nil || replayed.AnalysisID != created.AnalysisID {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
	detail, err := service.Get(ctx, created.AnalysisID)
	if err != nil || detail.State != "Queued" || detail.AttemptCount != 1 {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
	// Rebuild reproduces the frozen digest and dispatch binds.
	if _, err := service.RebuildInput(ctx, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	input, err := service.Attempts().DispatchInputFor(ctx, created.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if input.SchemaKind != "initial_analysis_v1" {
		t.Fatalf("schema=%q", input.SchemaKind)
	}
	if err := service.Attempts().BindToStream(ctx, created.AttemptID, "boot-1", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.AcceptAttempt(context.Background(), created.AttemptID, "boot-1", 1); err != nil {
		t.Fatal(err)
	}
	detail, _ = service.Get(ctx, created.AnalysisID)
	if detail.State != "Running" {
		t.Fatalf("analysis state=%q", detail.State)
	}
	// The succeeded closure requires a succeeded model call.
	sealAgentCall(t, db, created.AttemptID, "fixture-chat-1")
	content, _ := json.Marshal("这是初步分析结论。")
	digest := sha256.Sum256(content)
	err = service.CommitResult(context.Background(), Result{
		AttemptID: created.AttemptID, BootID: "boot-1", Epoch: 1, Succeeded: true,
		SchemaKind: OutputSchemaKind, Canonical: content, Digest: digest[:],
	})
	if err != nil {
		t.Fatal(err)
	}
	detail, _ = service.Get(ctx, created.AnalysisID)
	if detail.State != "Succeeded" || detail.Output == nil || detail.Output.Content != "这是初步分析结论。" {
		t.Fatalf("detail=%+v", detail)
	}
	// An identical replay of the sealed result is idempotent (T12,
	// RUNTIME-TASK-008: the runtime retries its terminal proposal until
	// an ack survives; the retry observes the original verdict).
	if err := service.CommitResult(context.Background(), Result{AttemptID: created.AttemptID, BootID: "boot-1", Epoch: 1, Succeeded: true, SchemaKind: OutputSchemaKind, Canonical: content, Digest: digest[:]}); err != nil {
		t.Fatalf("identical replay rejected: %v", err)
	}
	// A divergent late result after success still loses (DATA-TX-005).
	otherContent, _ := json.Marshal("另一个迟到的结论")
	otherDigest := sha256.Sum256(otherContent)
	if err := service.CommitResult(context.Background(), Result{AttemptID: created.AttemptID, BootID: "boot-1", Epoch: 1, Succeeded: true, SchemaKind: OutputSchemaKind, Canonical: otherContent, Digest: otherDigest[:]}); !errors.Is(err, ErrLateResult) {
		t.Fatalf("late result=%v", err)
	}
	// Re-analysis creates a NEW analysis after success.
	again, err := service.Create(ctx, occurrenceID, testOperatorID, "cmd-create-3")
	if err != nil || again.AnalysisID == created.AnalysisID {
		t.Fatalf("re-analysis=%+v err=%v", again, err)
	}
}

func TestRetryAfterFailure(t *testing.T) {
	db, dbPath := newTestDB(t)
	service := newTestService(t, db, dbPath)
	ctx := commandContext(t)
	occurrenceID := seedOccurrence(t, db)
	seedProviderChain(t, db)
	created, err := service.Create(ctx, occurrenceID, testOperatorID, "cmd-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Attempts().BindToStream(ctx, created.AttemptID, "boot-1", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.AcceptAttempt(context.Background(), created.AttemptID, "boot-1", 1); err != nil {
		t.Fatal(err)
	}
	// A rejected AgentComplete is reported by the runtime as invalid_response.
	// Once the underlying grant/runtime defect is repaired, this terminal
	// technical failure must admit a new queued analysis through the real
	// result-adjudication and retry service seam.
	if err := service.CommitResult(context.Background(), Result{AttemptID: created.AttemptID, BootID: "boot-1", Epoch: 1, Succeeded: false, Termination: "invalid_response"}); err != nil {
		t.Fatal(err)
	}
	detail, _ := service.Get(ctx, created.AnalysisID)
	if detail.State != "Failed" {
		t.Fatalf("state=%q", detail.State)
	}
	// The original failed record remains terminal and inspectable. Retry creates
	// a fresh analysis/attempt from the current eligible provider, which is the
	// recovery path after a repaired model configuration or runtime defect.
	retried, err := service.Retry(ctx, created.AnalysisID, testOperatorID, "cmd-retry-1")
	if err != nil {
		t.Fatalf("retry after technical failure: %v", err)
	}
	if retried.AnalysisID == created.AnalysisID || retried.AttemptID == created.AttemptID {
		t.Fatalf("retry must create fresh records: original=%+v retry=%+v", created, retried)
	}
	// A command id belongs to this retry and this failed source analysis only.
	// Reusing it as a create must not return the recovery result for an unrelated
	// operation, even when the occurrence is the same.
	if _, err := service.Create(ctx, occurrenceID, testOperatorID, "cmd-retry-1"); !errors.Is(err, ErrCommandReplayMismatch) {
		t.Fatalf("cross-operation replay=%v", err)
	}
	// Even the same operation cannot replay against a different requested
	// failed analysis; it must not return the first recovery's unrelated
	// records. Drive the recovery attempt to its own technical terminal state
	// so this exercises retry eligibility rather than an active-state conflict.
	if err := service.Attempts().BindToStream(ctx, retried.AttemptID, "boot-2", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.AcceptAttempt(context.Background(), retried.AttemptID, "boot-2", 1); err != nil {
		t.Fatal(err)
	}
	if err := service.CommitResult(context.Background(), Result{AttemptID: retried.AttemptID, BootID: "boot-2", Epoch: 1, Succeeded: false, Termination: "invalid_response"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Retry(ctx, retried.AnalysisID, testOperatorID, "cmd-retry-1"); !errors.Is(err, ErrCommandReplayMismatch) {
		t.Fatalf("cross-target replay=%v", err)
	}
	if detail, err := service.Get(ctx, created.AnalysisID); err != nil || detail.State != "Failed" {
		t.Fatalf("original failure must remain immutable: detail=%+v err=%v", detail, err)
	}
	if detail, err := service.Get(ctx, retried.AnalysisID); err != nil || detail.State != "Failed" {
		t.Fatalf("second technical failure=%+v err=%v", detail, err)
	}
	// A network retry of the same command must return the same new attempt,
	// never fabricate another recovery record.
	again, err := service.Retry(ctx, created.AnalysisID, testOperatorID, "cmd-retry-1")
	if err != nil || again != retried {
		t.Fatalf("retry replay=%+v want=%+v err=%v", again, retried, err)
	}
}

func TestCancelVsSuccessCommitOrder(t *testing.T) {
	db, dbPath := newTestDB(t)
	service := newTestService(t, db, dbPath)
	ctx := commandContext(t)
	occurrenceID := seedOccurrence(t, db)
	seedProviderChain(t, db)
	created, err := service.Create(ctx, occurrenceID, testOperatorID, "cmd-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Attempts().BindToStream(ctx, created.AttemptID, "boot-1", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.AcceptAttempt(context.Background(), created.AttemptID, "boot-1", 1); err != nil {
		t.Fatal(err)
	}
	// Cancel-before-result: the fence commits, the late result loses.
	detail, _ := service.Get(ctx, created.AnalysisID)
	outcome, err := service.Cancel(ctx, created.AnalysisID, testOperatorID, detail.RowVersion, "cmd-cancel-1")
	if err != nil || outcome.State != "Running" {
		t.Fatalf("cancel outcome=%+v err=%v", outcome, err)
	}
	if !outcome.DispatchRequired || outcome.AttemptID != created.AttemptID {
		t.Fatalf("running cancel must require a runtime dispatch: %+v", outcome)
	}
	content, _ := json.Marshal("迟到结果")
	digest := sha256.Sum256(content)
	if err := service.CommitResult(context.Background(), Result{AttemptID: created.AttemptID, BootID: "boot-1", Epoch: 1, Succeeded: true, SchemaKind: OutputSchemaKind, Canonical: content, Digest: digest[:]}); !errors.Is(err, ErrLateResult) {
		t.Fatalf("late success=%v", err)
	}
	if err := service.CancelAck(context.Background(), created.AttemptID); err != nil {
		t.Fatal(err)
	}
	detail, _ = service.Get(ctx, created.AnalysisID)
	if detail.State != "Cancelled" {
		t.Fatalf("state=%q", detail.State)
	}

	// Success-before-cancel: the seal wins and the fence answers the
	// completed object (HTTP-COMMAND-005).
	again, err := service.Create(ctx, occurrenceID, testOperatorID, "cmd-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Attempts().BindToStream(ctx, again.AttemptID, "boot-1", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.AcceptAttempt(context.Background(), again.AttemptID, "boot-1", 1); err != nil {
		t.Fatal(err)
	}
	sealAgentCall(t, db, again.AttemptID, "fixture-chat-1")
	content, _ = json.Marshal("成功结果")
	digest = sha256.Sum256(content)
	if err := service.CommitResult(context.Background(), Result{AttemptID: again.AttemptID, BootID: "boot-1", Epoch: 1, Succeeded: true, SchemaKind: OutputSchemaKind, Canonical: content, Digest: digest[:]}); err != nil {
		t.Fatal(err)
	}
	detail, _ = service.Get(ctx, again.AnalysisID)
	outcome, err = service.Cancel(ctx, again.AnalysisID, testOperatorID, detail.RowVersion, "cmd-cancel-2")
	if err != nil || outcome.State != "Succeeded" {
		t.Fatalf("cancel after success=%+v err=%v", outcome, err)
	}
	// A stale row version conflicts (a fresh command id with the outdated
	// version); a current version answers the completed object instead.
	if _, err := service.Cancel(ctx, again.AnalysisID, testOperatorID, detail.RowVersion-1, "cmd-cancel-3"); err == nil {
		t.Fatal("stale row version must conflict")
	}

	// A Queued attempt closes as Cancelled directly: no runtime dispatch
	// (DATA-ATTEMPT-003).
	queued, err := service.Create(ctx, occurrenceID, testOperatorID, "cmd-queued")
	if err != nil {
		t.Fatal(err)
	}
	queuedDetail, _ := service.Get(ctx, queued.AnalysisID)
	outcome, err = service.Cancel(ctx, queued.AnalysisID, testOperatorID, queuedDetail.RowVersion, "cmd-cancel-queued")
	if err != nil || outcome.State != "Cancelled" || outcome.DispatchRequired {
		t.Fatalf("queued cancel=%+v err=%v", outcome, err)
	}
	if _, err := service.Get(ctx, queued.AnalysisID); err != nil || outcome.AttemptID != queued.AttemptID {
		t.Fatalf("queued cancel attempt=%d err=%v", outcome.AttemptID, err)
	}
}

var _ = attempt.ErrLateResult
