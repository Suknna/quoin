package investigation

// Test harness over the real frozen schema (WAL SQLite, single writer):
// users, an enabled qualified model provider chain and alert occurrences
// — the same closure ladder the acceptance stack drives.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/Suknna/quoin/internal/plugins/builtin"
	"github.com/Suknna/quoin/internal/quoin/execution"
	_ "modernc.org/sqlite"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
)

func sha256Sum(body []byte) []byte {
	sum := sha256.Sum256(body)
	return sum[:]
}

var testSeedCounter int

// testAdminID/testOperatorID are the fixed fixture principals: the single
// admin (id 1) and the initialized operator (id 2) the shared setup creates
// with one real session each. The acting principal of command tests is the
// operator — investigations belong to any authenticated user, not only the
// admin; VerifyExecutionSession re-checks the session proof inside the
// runner transaction.
const (
	testAdminID    = int64(1)
	testOperatorID = int64(2)
)

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

// resolveToolGrantOnRunner drives one tool grant resolver exactly as the
// attempt machine does: composed on the service runner's guarded Tx through
// a dedicated registered test operation, never on a raw pool connection.
func resolveToolGrantOnRunner[T any](t *testing.T, service *Service, opName string, resolve func(ctx context.Context, tx *execution.Tx) (T, error)) (T, error) {
	t.Helper()
	op, err := service.runner.Register(execution.Operation{
		Name:       opName,
		Class:      execution.ClassWrite,
		ObjectType: ObjectInvestigation,
		Authorize:  func(context.Context, *execution.Tx) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "test-tool-grant-" + opName,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem, ID: 0},
		Source:        execution.Source{Kind: execution.SourceTask},
	})
	if err != nil {
		t.Fatal(err)
	}
	return execution.Execute(ctx, service.runner, op, func(tx *execution.Tx) (T, error) {
		return resolve(ctx, tx)
	}, func(T) int64 { return 0 })
}

// seedPrincipals creates the initialized admin/operator pair with one real,
// unexpired session each (auth revision 1), so execution metadata can carry
// a session proof reference that survives the runner's in-transaction
// session re-check (auth.VerifyExecutionSession).
func seedPrincipals(t *testing.T, db *sql.DB) {
	t.Helper()
	now := testNow()
	// Far-future session expiry: the fixture sessions stay valid for the
	// whole test window; revocation/expiry cases adjust the rows themselves.
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

// userContext injects execution metadata with the acting user's session
// proof reference, exactly like the HTTP admission middleware builds it.
// Commands fail closed without it — the services never synthesize identity.
func userContext(t *testing.T, principalID int64) context.Context {
	t.Helper()
	return sessionContext(t, principalID, fmt.Sprintf("corr-%s-%d", t.Name(), time.Now().UnixNano()))
}

func sessionContext(t *testing.T, principalID int64, correlation string) context.Context {
	t.Helper()
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: principalID},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-" + correlation},
		Session:       execution.SessionRef{ID: principalID, AuthRevision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// seedUser returns the shared operator principal (user 2).
func seedUser(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	return testOperatorID
}

// seedOtherUser returns the other fixture principal (the admin, user 1) for
// ownership boundaries that need a distinct second principal.
func seedOtherUser(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	return testAdminID
}

func testNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// seedProviderChain inserts one enabled qualified model provider through
// the frozen probe ladder (connection -> revision/generation -> probe
// attempt -> passed result -> enable qualification).
func seedProviderChain(t *testing.T, db *sql.DB) (connectionID, revisionID, generationID, probeResultID int64) {
	t.Helper()
	testSeedCounter++
	now := testNow()
	if _, err := db.Exec(`INSERT OR IGNORE INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`, []byte(strings.Repeat("e", 12)), []byte(strings.Repeat("f", 16)), now); err != nil {
		t.Fatal(err)
	}
	connection, err := db.Exec(`INSERT INTO connections(name,type,enabled,created_at) VALUES(?,?,'0',?)`,
		"test-provider-"+strings.Repeat("p", 8)+strings.TrimLeft(string(rune('a'+testSeedCounter%26)), " "), "model_provider", now)
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
		nonce[i] = byte(testSeedCounter*31 + i)
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
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Running',accepted_at=?,row_version=row_version+1 WHERE id=? AND state='Assigned'`, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
	probe, err := db.Exec(`INSERT INTO connection_probe_results(attempt_id,connection_id,connection_type,connection_revision_id,credential_generation_id,root_binding_revision,action_set_id,action_set_version,probe_contract_digest,outcome,result_digest,started_at,finished_at,created_at) VALUES(?,?,?,?,?,1,'model-provider-capabilities',1,?,?,?,?,?,?)`,
		probeAttemptID, connectionID, "model_provider", revisionID, generationID, strings.Repeat("0", 64), "passed", strings.Repeat("1", 64), now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	probeResultID, _ = probe.LastInsertId()
	// The frozen probe child closure requires the real call evidence:
	// one succeeded chat call, one cancelled chat call and one succeeded
	// embedding call (the probe action set drives all three).
	succeededCall, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,evicted_turn_count,status,started_at) VALUES(?,'1','0','chat','fixture-chat-1',?,'connection-probe-v1','probe-supervisor-v1',?,?,?,?,?,4096,1024,0,0,'running',?)`,
		probeAttemptID, probeChatGrantID, strings.Repeat("1", 64), strings.Repeat("1", 64), strings.Repeat("1", 64), strings.Repeat("1", 64), strings.Repeat("1", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	succeededCallID, _ := succeededCall.LastInsertId()
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,1,'system',?,'system_contract'),(?,2,'system',?,'tool_schema')`, succeededCallID, strings.Repeat("1", 64), succeededCallID, strings.Repeat("1", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,1,'{"assistantText":"ok","finishReason":"stop","tool_calls":[]}',?,'stop',?)`, succeededCallID, strings.Repeat("1", 64), now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":1,"total_tokens":2}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, succeededCallID); err != nil {
		t.Fatal(err)
	}
	cancelledCall, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,evicted_turn_count,status,started_at) VALUES(?,'4','0','chat','fixture-chat-1',?,'connection-probe-v1','probe-supervisor-v1',?,?,?,?,?,4096,1024,0,0,'running',?)`,
		probeAttemptID, probeChatGrantID, strings.Repeat("1", 64), strings.Repeat("1", 64), strings.Repeat("1", 64), strings.Repeat("1", 64), strings.Repeat("1", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	cancelledCallID, _ := cancelledCall.LastInsertId()
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,1,'system',?,'system_contract'),(?,2,'system',?,'tool_schema')`, cancelledCallID, strings.Repeat("1", 64), cancelledCallID, strings.Repeat("1", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE model_calls SET status='cancelled',termination_reason='cancelled',ended_at=? WHERE id=? AND status='running'`, now, cancelledCallID); err != nil {
		t.Fatal(err)
	}
	embeddingCall, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,input_snapshot_digest,rendered_request_digest,estimated_input_tokens,status,started_at) VALUES(?,'6','0','embedding','fixture-embed-1',?,?,?,0,'running',?)`,
		probeAttemptID, probeEmbeddingGrantID, strings.Repeat("1", 64), strings.Repeat("1", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	embeddingCallID, _ := embeddingCall.LastInsertId()
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,attempt_input_snapshot_id) VALUES(?,1,'user',?,?)`, embeddingCallID, strings.Repeat("1", 64), probeSnapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,created_at) VALUES(?,1,'{}',?,?)`, embeddingCallID, strings.Repeat("1", 64), now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":0,"total_tokens":1}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, embeddingCallID); err != nil {
		t.Fatal(err)
	}
	// The typed child (the frozen probe closure writes it from the
	// canonical detail; the seed inserts the same row directly).
	if _, err := db.Exec(`INSERT INTO model_provider_connection_probe_results(probe_result_id,chat_model_id,embedding_model_id,context_budget_tokens,max_output_tokens,streaming_supported,native_tool_calling_supported,multi_tool_call_supported,cancellation_observed,usage_observed,request_id_observed,embedding_supported,embedding_vector_dim,detail_json) VALUES(?,'fixture-chat-1','fixture-embed-1',4096,1024,1,1,1,1,1,1,1,16,'{}')`, probeResultID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Succeeded',ended_at=?,row_version=row_version+1 WHERE id=? AND state='Running'`, now, probeAttemptID); err != nil {
		t.Fatal(err)
	}
	// Enable with the explicit qualification event (row_version+1 fence).
	if _, err := db.Exec(`INSERT INTO connection_enable_qualifications(connection_id,enabled_row_version,probe_result_id,created_by,created_at) VALUES(?,3,?,?,?)`, connectionID, probeResultID, 1, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE connections SET enabled=1,revalidation_required=0,row_version=row_version+1 WHERE id=? AND row_version=2`, connectionID); err != nil {
		t.Fatal(err)
	}
	return connectionID, revisionID, generationID, probeResultID
}

func seedOccurrence(t *testing.T, db *sql.DB, alertname string) int64 {
	t.Helper()
	testSeedCounter++
	now := testNow()
	source, err := db.Exec(`INSERT INTO alert_sources(source_key,protocol,enabled,created_at) VALUES(?,'alertmanager',1,?)`,
		"test-source-"+strings.TrimLeft(string(rune('a'+testSeedCounter%26)), " ")+strings.Repeat("s", 8), now)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, _ := source.LastInsertId()
	occurrence, err := db.Exec(`INSERT INTO alert_occurrences(source_id,fingerprint,starts_at,state,labels_canonical,labels_digest,first_seen_at,last_state_change_at) VALUES(?,?,?,'Firing',?,?,?,?)`,
		sourceID, []byte{byte(testSeedCounter), 0, 0, 0, 0, 0, 0, 1}, now, `{"alertname":"`+alertname+`"}`, strings.Repeat("c", 64), now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := occurrence.LastInsertId()
	return id
}
