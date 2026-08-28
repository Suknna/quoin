package artifact

// Store tests (DATA-ARTIFACT-001..006, RUNTIME-ARTIFACT-002/003): the
// content-addressed upload ledger (hash/size/owner fences), the
// attempt-scoped read grants and the bounded text read/grep paths.

import (
	"bytes"
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
// grant (the frozen closure demands succeeded before the grant).
func sealToolResult(t *testing.T, db *sql.DB, store *Store, attemptID, toolCallID, artifactID int64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`UPDATE tool_calls SET status='running',started_at=?,row_version=row_version+1 WHERE id=? AND status='pending'`, now, toolCallID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tool_calls SET status='succeeded',result_json='{"preview":"ok"}',result_artifact_id=?,ended_at=?,row_version=row_version+1 WHERE id=? AND status='running'`, artifactID, now, toolCallID); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertToolResultGrant(context.Background(), conn, attemptID, artifactID, toolCallID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `COMMIT`); err != nil {
		t.Fatal(err)
	}
}

func TestUploadReadGrepFences(t *testing.T) {
	db := newTestDB(t)
	store, err := NewStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
func TestLintelBrowserArtifactOwnerClosure(t *testing.T) {
	db := newTestDB(t)
	store, err := NewStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF; DROP TRIGGER trg_browser_operations_insert_closure; DROP TRIGGER trg_browser_exploration_actions_closure; DROP TRIGGER trg_browser_exploration_parent_tool; DROP TRIGGER trg_execution_attempts_insert_queued; DROP TRIGGER trg_execution_attempts_slot_registered`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO investigations(id,created_by,created_at) VALUES(1,1,?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,state,runtime_slot,boot_id,connection_epoch,lease_until,accepted_at,quoin_release_version,runtime_release_version,created_at) VALUES(11,'browser_exploration','investigation',1,'Running','lintel','lintel-boot',2,?,?, 'q','lintel',?)`, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO browser_operations(id,identity_id,identity_revision_id,profile_generation_id,owner_attempt_id,kind,state,journey_catalog_digest,journey_catalog_version,requested_at,start_dispatched_at,lintel_boot_id,lintel_connection_epoch,started_at) VALUES(22,1,1,1,1,'exploration','Running',?,'v1',?,'lintel-boot','lintel-boot',2,?)`, strings.Repeat("a", 64), now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO browser_exploration_actions(operation_id,action_seq,child_attempt_id,tool_call_id,action_kind,target_description,started_at) VALUES(22,1,11,1,'open','Payments',?)`, now); err != nil {
		t.Fatal(err)
	}
	// This row is deliberately action-bound but terminal, proving the Store does
	// not treat relation existence alone as authority to attach browser bytes.
	if _, err := db.Exec(`INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,state,runtime_slot,boot_id,connection_epoch,lease_until,accepted_at,quoin_release_version,runtime_release_version,created_at) VALUES(12,'browser_exploration','investigation',1,'Running','lintel','lintel-boot',2,?,?, 'q','lintel',?)`, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO browser_operations(id,identity_id,identity_revision_id,profile_generation_id,owner_attempt_id,kind,state,journey_catalog_digest,journey_catalog_version,requested_at,start_dispatched_at,lintel_boot_id,lintel_connection_epoch,ended_at,terminal_reason,stop_confirmed_at,stop_confirmation_basis) VALUES(23,1,1,1,1,'exploration','Failed',?,'v1',?,'lintel-boot','lintel-boot',2,?,'protocol_error',?,'stop_ack')`, strings.Repeat("a", 64), now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO browser_exploration_actions(operation_id,action_seq,child_attempt_id,tool_call_id,action_kind,target_description,started_at) VALUES(23,1,12,2,'open','Payments',?)`, now); err != nil {
		t.Fatal(err)
	}
	// A close claim is the only capability that allows this trace to become
	// complete. CommitUpload must atomically bind its installed Artifact rather
	// than leave crash recovery to infer from an unowned upload ledger row.
	if _, err := db.Exec(`INSERT INTO browser_exploration_terminal_claims(child_attempt_id,operation_id,tool_call_id,state,created_at) VALUES(11,22,1,'claimed_complete',?)`, now); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"action":"close_session","status":"ok"}`)
	digest := sha256.Sum256(body)
	header := UploadHeader{RuntimeSlot: "lintel", UploadID: "lintel-trace", AttemptID: 11, BootID: "lintel-boot", ConnectionEpoch: 2, OwnerType: "browser_operation", OwnerID: 22, Kind: "trace", RetentionKind: "generated", Sensitive: true, SizeBytes: int64(len(body)), SHA256: digest[:], MediaType: "application/json", TraceIntegrity: "complete"}
	file, replayID, err := store.BeginUpload(context.Background(), header)
	if err != nil || replayID != 0 {
		t.Fatalf("valid Lintel trace BeginUpload replay=%d err=%v", replayID, err)
	}
	if _, err := file.Write(body); err != nil {
		t.Fatal(err)
	}
	artifactID, err := store.CommitUpload(context.Background(), header, file)
	if err != nil {
		t.Fatal(err)
	}
	var claimState string
	var claimArtifact int64
	var claimDigest []byte
	if err := db.QueryRow(`SELECT state,trace_artifact_id,trace_digest FROM browser_exploration_terminal_claims WHERE child_attempt_id=11`).Scan(&claimState, &claimArtifact, &claimDigest); err != nil || claimState != "artifact_committed_complete" || claimArtifact != artifactID || !bytes.Equal(claimDigest, digest[:]) {
		t.Fatalf("committed trace claim state=%q artifact=%d digest=%x err=%v", claimState, claimArtifact, claimDigest, err)
	}
	// Reusing an upload ID is idempotent only with identical immutable metadata.
	// A different browser-operation owner must be rejected rather than returning
	// the first Artifact ID across ownership boundaries.
	alteredOwner := header
	alteredOwner.OwnerID = 23
	if _, _, err := store.BeginUpload(context.Background(), alteredOwner); err == nil {
		t.Fatal("same upload ID with changed owner was accepted")
	}
	// Integrity describes the terminal capability itself. An at-least-once
	// retransmission may not reuse a complete upload ID to claim incomplete
	// evidence (or the reverse), even when every other header field matches.
	alteredIntegrity := header
	alteredIntegrity.TraceIntegrity = "incomplete"
	if _, _, err := store.BeginUpload(context.Background(), alteredIntegrity); err == nil {
		t.Fatal("same upload ID with opposite trace integrity was accepted")
	}
	// A committed continuous trace may be replayed on a later authenticated
	// same-boot stream without reopening an upload or rewriting its start fence.
	traceReplay := header
	traceReplay.ConnectionEpoch = 3
	if _, replayID, err := store.BeginUpload(context.Background(), traceReplay); err != nil || replayID != artifactID {
		t.Fatalf("same-boot later-epoch trace replay id=%d err=%v", replayID, err)
	}
	traceReplay.ConnectionEpoch = 1
	if _, _, err := store.BeginUpload(context.Background(), traceReplay); err == nil {
		t.Fatal("older epoch trace replay was accepted")
	}
	var kind, owner string
	var ownerID int64
	var sensitive int
	if err := db.QueryRow(`SELECT kind,owner_type,owner_id,sensitive FROM artifacts WHERE id=?`, artifactID).Scan(&kind, &owner, &ownerID, &sensitive); err != nil || kind != "trace" || owner != "browser_operation" || ownerID != 22 || sensitive != 1 {
		t.Fatalf("committed trace owner=%s/%d kind=%s sensitive=%d err=%v", owner, ownerID, kind, sensitive, err)
	}
	// A crash trace deliberately has no child Attempt fence: its authority is
	// the running operation plus the current Lintel boot/epoch action fence.
	operationTrace := header
	operationTrace.UploadID, operationTrace.AttemptID = "lintel-operation-trace", 0
	file, replayID, err = store.BeginUpload(context.Background(), operationTrace)
	if err != nil || replayID != 0 {
		t.Fatalf("operation-level crash trace BeginUpload replay=%d err=%v", replayID, err)
	}
	if _, err := file.Write(body); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitUpload(context.Background(), operationTrace, file); err != nil {
		t.Fatalf("operation-level crash trace commit: %v", err)
	}
	// A same-boot reconnect transports pending browser bytes over a successor
	// epoch without rewriting the immutable Start epoch on either authority row.
	successor := header
	successor.UploadID, successor.ConnectionEpoch, successor.Kind, successor.Sensitive, successor.MediaType, successor.TraceIntegrity = "lintel-successor-epoch", 3, "screenshot", false, "image/png", ""
	successor.SizeBytes = 0
	emptySuccessorDigest := sha256.Sum256(nil)
	successor.SHA256 = emptySuccessorDigest[:]
	file, replayID, err = store.BeginUpload(context.Background(), successor)
	if err != nil || replayID != 0 {
		t.Fatalf("same-boot successor upload rejected replay=%d err=%v", replayID, err)
	}
	if _, err := store.CommitUpload(context.Background(), successor, file); err != nil {
		t.Fatalf("same-boot successor upload commit: %v", err)
	}
	var childEpoch, operationEpoch int64
	if err := db.QueryRow(`SELECT connection_epoch FROM execution_attempts WHERE id=11`).Scan(&childEpoch); err != nil || childEpoch != 2 {
		t.Fatalf("child start epoch was rewritten epoch=%d err=%v", childEpoch, err)
	}
	if err := db.QueryRow(`SELECT lintel_connection_epoch FROM browser_operations WHERE id=22`).Scan(&operationEpoch); err != nil || operationEpoch != 2 {
		t.Fatalf("operation start epoch was rewritten epoch=%d err=%v", operationEpoch, err)
	}
	emptyDigest := sha256.Sum256(nil)
	for _, rejected := range []UploadHeader{
		{RuntimeSlot: "lintel", UploadID: "wrong-op", AttemptID: 11, BootID: "lintel-boot", ConnectionEpoch: 2, OwnerType: "browser_operation", OwnerID: 23, Kind: "screenshot", RetentionKind: "generated", SizeBytes: 0, SHA256: emptyDigest[:], MediaType: "image/png"},
		{RuntimeSlot: "lintel", UploadID: "terminal-op", AttemptID: 12, BootID: "lintel-boot", ConnectionEpoch: 2, OwnerType: "browser_operation", OwnerID: 23, Kind: "screenshot", RetentionKind: "generated", SizeBytes: 0, SHA256: emptyDigest[:], MediaType: "image/png"},
		{RuntimeSlot: "lintel", UploadID: "wrong-boot", AttemptID: 11, BootID: "other-boot", ConnectionEpoch: 2, OwnerType: "browser_operation", OwnerID: 22, Kind: "screenshot", RetentionKind: "generated", SizeBytes: 0, SHA256: emptyDigest[:], MediaType: "image/png"},
		{RuntimeSlot: "lintel", UploadID: "stale-epoch", AttemptID: 11, BootID: "lintel-boot", ConnectionEpoch: 1, OwnerType: "browser_operation", OwnerID: 22, Kind: "screenshot", RetentionKind: "generated", SizeBytes: 0, SHA256: emptyDigest[:], MediaType: "image/png"},
		// attempt_id=0 is only the operation-level crash trace exception. It
		// still carries the exact operation's frozen Lintel fence.
		{RuntimeSlot: "lintel", UploadID: "operation-wrong-boot", AttemptID: 0, BootID: "other-boot", ConnectionEpoch: 2, OwnerType: "browser_operation", OwnerID: 22, Kind: "trace", RetentionKind: "generated", Sensitive: true, SizeBytes: 0, SHA256: emptyDigest[:], MediaType: "application/json"},
		{RuntimeSlot: "lintel", UploadID: "operation-stale-epoch", AttemptID: 0, BootID: "lintel-boot", ConnectionEpoch: 1, OwnerType: "browser_operation", OwnerID: 22, Kind: "trace", RetentionKind: "generated", Sensitive: true, SizeBytes: 0, SHA256: emptyDigest[:], MediaType: "application/json"},
		{RuntimeSlot: "lintel", UploadID: "operation-wrong-kind", AttemptID: 0, BootID: "lintel-boot", ConnectionEpoch: 2, OwnerType: "browser_operation", OwnerID: 22, Kind: "screenshot", RetentionKind: "generated", SizeBytes: 0, SHA256: emptyDigest[:], MediaType: "image/png"},
		{RuntimeSlot: "lintel", UploadID: "operation-wrong-owner", AttemptID: 0, BootID: "lintel-boot", ConnectionEpoch: 2, OwnerType: "tool_call", OwnerID: 22, Kind: "trace", RetentionKind: "generated", Sensitive: true, SizeBytes: 0, SHA256: emptyDigest[:], MediaType: "application/json"},
		{RuntimeSlot: "lintel", UploadID: "operation-trace-not-sensitive", AttemptID: 0, BootID: "lintel-boot", ConnectionEpoch: 2, OwnerType: "browser_operation", OwnerID: 22, Kind: "trace", RetentionKind: "generated", Sensitive: false, SizeBytes: 0, SHA256: emptyDigest[:], MediaType: "application/json"},
	} {
		if _, _, err := store.BeginUpload(context.Background(), rejected); err == nil {
			t.Fatalf("invalid Lintel header %+v was accepted", rejected)
		}
	}
}

func TestCommittedLintelScreenshotReplayAcceptsSuccessorEpochOnly(t *testing.T) {
	digest := sha256.Sum256([]byte("screenshot"))
	header := UploadHeader{
		RuntimeSlot: "lintel", UploadID: "screenshot-replay", AttemptID: 7,
		BootID: "boot-a", ConnectionEpoch: 8, OwnerType: "browser_operation", OwnerID: 9,
		Kind: "screenshot", MediaType: "image/png", RetentionKind: "generated", Sensitive: true,
		SizeBytes: int64(len("screenshot")), SHA256: digest[:],
	}
	attempt := sql.NullInt64{Int64: header.AttemptID, Valid: true}
	if !sameCommittedUploadMetadata(attempt, header.BootID, 7, header.OwnerType, header.OwnerID, header.Kind, header.MediaType, header.RetentionKind, 1, header.SizeBytes, hex.EncodeToString(header.SHA256), sql.NullString{}, header) {
		t.Fatal("same-boot successor epoch must replay a committed screenshot")
	}
	header.ConnectionEpoch = 7
	if sameCommittedUploadMetadata(attempt, header.BootID, 8, header.OwnerType, header.OwnerID, header.Kind, header.MediaType, header.RetentionKind, 1, header.SizeBytes, hex.EncodeToString(header.SHA256), sql.NullString{}, header) {
		t.Fatal("earlier epoch must not replay a committed browser artifact")
	}
}

func TestInterruptedLintelUploadAcceptsOnlySuccessorTransportEpoch(t *testing.T) {
	digest := sha256.Sum256([]byte("trace"))
	header := UploadHeader{
		RuntimeSlot: "lintel", UploadID: "interrupted-trace", AttemptID: 7,
		BootID: "boot-a", ConnectionEpoch: 9, OwnerType: "browser_operation", OwnerID: 9,
		Kind: "trace", MediaType: "application/json", RetentionKind: "generated", Sensitive: true,
		SizeBytes: int64(len("trace")), SHA256: digest[:], TraceIntegrity: "incomplete",
	}
	attempt := sql.NullInt64{Int64: header.AttemptID, Valid: true}
	if !sameRetryUploadMetadata(attempt, header.BootID, 8, header.OwnerType, header.OwnerID, header.Kind, header.MediaType, header.RetentionKind, 1, header.SizeBytes, hex.EncodeToString(header.SHA256), sql.NullString{String: "incomplete", Valid: true}, header) {
		t.Fatal("same-boot successor epoch must retry an interrupted Lintel trace")
	}
	header.ConnectionEpoch = 8
	if sameRetryUploadMetadata(attempt, header.BootID, 9, header.OwnerType, header.OwnerID, header.Kind, header.MediaType, header.RetentionKind, 1, header.SizeBytes, hex.EncodeToString(header.SHA256), sql.NullString{String: "incomplete", Valid: true}, header) {
		t.Fatal("older epoch must not retry an interrupted Lintel trace")
	}
	header.BootID = "boot-b"
	header.ConnectionEpoch = 10
	if sameRetryUploadMetadata(attempt, "boot-a", 8, header.OwnerType, header.OwnerID, header.Kind, header.MediaType, header.RetentionKind, 1, header.SizeBytes, hex.EncodeToString(header.SHA256), sql.NullString{String: "incomplete", Valid: true}, header) {
		t.Fatal("different boot must not retry an interrupted Lintel trace")
	}
}

func TestMaterializeEvidenceTransactionIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	store, err := NewStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	evidence, err := db.Exec(`INSERT INTO evidence(target_type,target_id,params_json,observed_at,result_json,integrity,created_at) VALUES('inspection_run',1,'{}',?,?,'complete',?)`, now, `{"value":1}`, now)
	if err != nil {
		t.Fatal(err)
	}
	evidenceID, _ := evidence.LastInsertId()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	first, err := store.MaterializeEvidenceTransaction(context.Background(), conn, evidenceID, []byte(`{"value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.MaterializeEvidenceTransaction(context.Background(), conn, evidenceID, []byte(`{"value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("materialization ids differ: %d %d", first, second)
	}
	if _, err = conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(`{"value":1}`))
	if err = verifyBlob(store.blobPath(hex.EncodeToString(sum[:])), int64(len(`{"value":1}`)), sum[:]); err != nil {
		t.Fatalf("materialized blob: %v", err)
	}
}
