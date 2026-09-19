package artifact

// Store tests (DATA-ARTIFACT-001..006, RUNTIME-ARTIFACT-002/003): the
// content-addressed upload ledger (hash/size/owner fences), the
// attempt-scoped read grants and the bounded text read/grep paths.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/testfixture"
	_ "modernc.org/sqlite"
)

func newTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := t.TempDir() + "/test.db"
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO artifact_retention_settings(id,generated_retention_days,row_version,updated_at) VALUES(1,90,1,?)`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	return db, path
}

// newTestStore builds the fixture store and wires its pure reads to a real
// independent read-only pool — execution.OpenReadOnly on the same database
// file, closed with the test. Production reads fail closed until the
// composition layer installs the reader, exactly as the app must.
func newTestStore(t *testing.T) (*sql.DB, *Store) {
	t.Helper()
	db, path := newTestDB(t)
	store, err := NewStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := execution.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if err := store.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	return db, store
}

// seedToolOwner builds a Running connection_probe attempt carrying one
// pending bash tool_call owned by the attempt (the upload owner closure
// and read grants both target it). The probe ladder avoids the agent
// grant-qualification chain the artifact mechanics do not exercise.
func seedToolOwner(t *testing.T, db *sql.DB) (attemptID, toolCallID int64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT OR IGNORE INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`, []byte(strings.Repeat("e", 12)), []byte(strings.Repeat("f", 16)), now); err != nil {
		t.Fatal(err)
	}
	connection, err := db.Exec(`INSERT INTO connections(name,type,enabled,created_at) VALUES(?,'model_provider',0,?)`, fmt.Sprintf("conn-%d", time.Now().UnixNano()), now)
	if err != nil {
		t.Fatal(err)
	}
	connectionID, _ := connection.LastInsertId()
	revision, err := db.Exec(`INSERT INTO connection_revisions(connection_id,revision_seq,config_json,created_at) VALUES(?,1,'{}',?)`, connectionID, now)
	if err != nil {
		t.Fatal(err)
	}
	revisionID, _ := revision.LastInsertId()
	generation, err := db.Exec(`INSERT INTO credential_generations(connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_at) VALUES(?,1,1,1,?,?,?)`, connectionID, []byte(strings.Repeat("e", 12)), []byte(strings.Repeat("f", 32)), now)
	if err != nil {
		t.Fatal(err)
	}
	generationID, _ := generation.LastInsertId()
	if _, err := db.Exec(`UPDATE connections SET current_revision_id=?, current_credential_generation_id=?, row_version=row_version+1 WHERE id=?`, revisionID, generationID, connectionID); err != nil {
		t.Fatal(err)
	}
	attempt, err := db.Exec(`INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,created_at) VALUES('connection_probe','connection',?,'Queued','test',?)`, connectionID, now)
	if err != nil {
		t.Fatal(err)
	}
	attemptID, _ = attempt.LastInsertId()
	snapshot, err := db.Exec(`INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(?,'connection_probe_v1','connection-probe-v1',?,?)`, attemptID, strings.Repeat("c", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, _ := snapshot.LastInsertId()
	if _, err := db.Exec(`INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id) VALUES(?,1,'user',?,?)`, snapshotID, strings.Repeat("d", 64), revisionID); err != nil {
		t.Fatal(err)
	}
	var chatGrantID int64
	for _, purpose := range []string{"model_probe_chat", "model_probe_embedding"} {
		grant, err := db.Exec(`INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at) VALUES(?,?,?,?,?,?)`, attemptID, purpose, connectionID, revisionID, generationID, now)
		if err != nil {
			t.Fatal(err)
		}
		if purpose == "model_probe_chat" {
			chatGrantID, _ = grant.LastInsertId()
		}
	}
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='boot-1',connection_epoch=7,lease_until=?,runtime_release_version='test',row_version=row_version+1 WHERE id=? AND state='Queued'`, now, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Running',started_at=?,accepted_at=?,row_version=row_version+1 WHERE id=? AND state='Assigned'`, now, now, attemptID); err != nil {
		t.Fatal(err)
	}
	call, err := db.Exec(`INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,evicted_turn_count,status,started_at) VALUES(?,'1','0','chat','fixture-chat-1',?,'connection-probe-v1','probe-supervisor-v1',?,?,?,?,?,4096,1024,0,0,'running',?)`,
		attemptID, chatGrantID, strings.Repeat("e", 64), strings.Repeat("f", 64), strings.Repeat("f", 64), strings.Repeat("1", 64), strings.Repeat("2", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	callID, _ := call.LastInsertId()
	if _, err := db.Exec(`INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,1,'system',?,'system_contract'),(?,2,'system',?,'tool_schema')`, callID, strings.Repeat("e", 64), callID, strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,1,'{"assistantText":"","finishReason":"tool_calls","tool_calls":[{"id":"call-1","name":"bash","arguments":{"command":"echo hi"}}]}',?, 'tool_calls',?)`, callID, strings.Repeat("3", 64), now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":1,"total_tokens":2}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
		t.Fatal(err)
	}
	tool, err := db.Exec(`INSERT INTO tool_calls(attempt_id,model_call_id,call_seq,tool_index,provider_tool_call_id,tool_name,tool_version,arguments_json,arguments_digest,execution_mode,failure_mode,status,created_at) VALUES(?,?,'1',0,'call-1','bash','1','{"command":"echo hi"}',?,'worker_local','return_to_model','pending',?)`,
		attemptID, callID, strings.Repeat("4", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	toolCallID, _ = tool.LastInsertId()
	return attemptID, toolCallID
}

func uploadText(t *testing.T, store *Store, ctx context.Context, attemptID, toolCallID int64, body string) (int64, string) {
	t.Helper()
	hash := sha256.Sum256([]byte(body))
	header := UploadHeader{
		UploadID: fmt.Sprintf("up-%d", time.Now().UnixNano()), AttemptID: attemptID,
		BootID: "boot-1", ConnectionEpoch: 7,
		OwnerType: "tool_call", OwnerID: toolCallID,
		Kind: "tool_result", RetentionKind: "generated",
		Sensitive: false, SizeBytes: int64(len(body)), SHA256: hash[:], MediaType: "text/plain",
	}
	file, replayID, err := store.BeginUpload(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	if replayID != 0 {
		return replayID, hex.EncodeToString(hash[:])
	}
	if _, err := file.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	artifactID, err := store.CommitUpload(ctx, header, file)
	if err != nil {
		t.Fatal(err)
	}
	return artifactID, hex.EncodeToString(hash[:])
}

// sealToolResult completes the schema-honest tool ladder after an upload:
// pending -> running -> succeeded with the result artifact, then the read
// grant (the frozen closure demands succeeded before the grant). The grant
// write is composed the way production composes it: one fresh runner
// transaction whose guarded *execution.Tx is the only write surface —
// a raw *sql.Conn can no longer satisfy the store's Executor parameter.
func sealToolResult(t *testing.T, db *sql.DB, store *Store, attemptID, toolCallID, artifactID int64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`UPDATE tool_calls SET status='running',started_at=?,row_version=row_version+1 WHERE id=? AND status='pending'`, now, toolCallID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tool_calls SET status='succeeded',result_json='{"preview":"ok"}',result_artifact_id=?,ended_at=?,row_version=row_version+1 WHERE id=? AND status='running'`, artifactID, now, toolCallID); err != nil {
		t.Fatal(err)
	}
	runner := execution.NewRunner(db, execution.NewRegistry(), nil)
	op, err := runner.Register(execution.Operation{
		Name: "test.artifact.read-grant", Class: execution.ClassWrite, ObjectType: objectTypeArtifact,
		Authorize: authorizeSystemDataPlane,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := testfixture.SystemContext(t, "artifact-test-grant")
	if _, err := execution.Execute(ctx, runner, op, func(tx *execution.Tx) (struct{}, error) {
		return struct{}{}, store.InsertToolResultGrant(ctx, tx, attemptID, artifactID, toolCallID)
	}, func(struct{}) int64 { return artifactID }); err != nil {
		t.Fatal(err)
	}
}

func TestUploadReadGrepFences(t *testing.T) {
	db, store := newTestStore(t)
	ctx := context.Background()
	attemptID, toolCallID := seedToolOwner(t, db)
	body := "line one: alpha\nline two: beta\nline three: alpha again\n"
	artifactID, shaHex := uploadText(t, store, ctx, attemptID, toolCallID, body)

	// The upload ledger rejects a hash mismatch and records the rejection.
	// The bad upload runs BEFORE sealToolResult: the owner fence only
	// accepts pending/running tool calls.
	badHash := make([]byte, 32)
	header := UploadHeader{
		UploadID: "up-bad", AttemptID: attemptID, BootID: "boot-1", ConnectionEpoch: 7,
		OwnerType: "tool_call", OwnerID: toolCallID,
		Kind: "tool_result", RetentionKind: "generated",
		SizeBytes: 3, SHA256: badHash, MediaType: "text/plain",
	}
	file, _, err := store.BeginUpload(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("abc"))
	if _, err := store.CommitUpload(ctx, header, file); err == nil {
		t.Fatal("hash mismatch must reject the upload")
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM runtime_artifact_uploads WHERE upload_id='up-bad'`).Scan(&state); err != nil || state != "rejected" {
		t.Fatalf("state=%q err=%v", state, err)
	}
	_ = os.Remove(store.StagingPath("up-bad"))

	sealToolResult(t, db, store, attemptID, toolCallID, artifactID)

	// ReadText: bounded slice with correct offsets and EOF.
	slice, err := store.ReadText(ctx, attemptID, artifactID, "boot-1", 7, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if string(slice.Content) != "line one: alpha\nline two: beta\n" || slice.EOF || slice.NextLine != 3 {
		t.Fatalf("slice=%+v content=%q", slice, slice.Content)
	}
	if slice.SHA256 == nil || hex.EncodeToString(slice.SHA256) != shaHex {
		t.Fatalf("sha=%x want=%s", slice.SHA256, shaHex)
	}
	second, err := store.ReadText(ctx, attemptID, artifactID, "boot-1", 7, 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !second.EOF || string(second.Content) != "line three: alpha again\n" {
		t.Fatalf("second=%q eof=%v", second.Content, second.EOF)
	}

	// GrepText: RE2 matches with line numbers.
	grep, err := store.GrepText(ctx, attemptID, artifactID, "boot-1", 7, "alpha", 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(grep.Matches) != 2 || grep.Matches[0].LineNumber != 1 || grep.Matches[1].LineNumber != 3 {
		t.Fatalf("grep=%+v", grep.Matches)
	}

	// Fences: wrong boot, wrong epoch, ungranted artifact.
	if _, err := store.ReadText(ctx, attemptID, artifactID, "boot-other", 7, 1, 10); err == nil {
		t.Fatal("boot fence must reject")
	}
	if _, err := store.ReadText(ctx, attemptID, artifactID, "boot-1", 9, 1, 10); err == nil {
		t.Fatal("epoch fence must reject")
	}
	if _, err := store.ReadText(ctx, attemptID, artifactID+100, "boot-1", 7, 1, 10); err == nil {
		t.Fatal("ungranted artifact must reject")
	}
}

// TestLintelBrowserArtifactOwnerClosure exercises the frozen SQLite tables and
// the Store's Lintel-specific relation check. The small fixture intentionally
// uses the real table CHECK constraints; unrelated parent/model provenance is
// outside Artifact ownership and therefore omitted with foreign keys disabled.
func TestMaterializeEvidenceTransactionIsIdempotent(t *testing.T) {
	db, store := newTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	evidence, err := db.Exec(`INSERT INTO evidence(target_type,target_id,params_json,observed_at,result_json,integrity,created_at) VALUES('inspection_run',1,'{}',?,?,'complete',?)`, now, `{"value":1}`, now)
	if err != nil {
		t.Fatal(err)
	}
	evidenceID, _ := evidence.LastInsertId()
	// Both materializations join ONE runner transaction, preserving the
	// caller-transaction idempotency contract: the second call observes the
	// first call's rows inside the same transaction and answers the same id.
	runner := execution.NewRunner(db, execution.NewRegistry(), nil)
	op, err := runner.Register(execution.Operation{
		Name: "test.artifact.materialize", Class: execution.ClassWrite, ObjectType: objectTypeArtifact,
		Authorize: authorizeSystemDataPlane,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := testfixture.SystemContext(t, "artifact-test-materialize")
	var first, second int64
	if _, err := execution.Execute(ctx, runner, op, func(tx *execution.Tx) (struct{}, error) {
		var err error
		first, err = store.MaterializeEvidenceTransaction(ctx, tx, evidenceID, []byte(`{"value":1}`))
		if err != nil {
			return struct{}{}, err
		}
		second, err = store.MaterializeEvidenceTransaction(ctx, tx, evidenceID, []byte(`{"value":1}`))
		return struct{}{}, err
	}, func(struct{}) int64 { return first }); err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("materialization ids differ: %d %d", first, second)
	}
	sum := sha256.Sum256([]byte(`{"value":1}`))
	if err = verifyBlob(store.blobPath(hex.EncodeToString(sum[:])), int64(len(`{"value":1}`)), sum[:]); err != nil {
		t.Fatalf("materialized blob: %v", err)
	}
}
