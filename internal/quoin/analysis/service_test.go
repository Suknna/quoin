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

var seedCounter int

// seedOccurrence inserts one firing alert occurrence with a published business
// declaration. Analysis creation must not infer a global metrics connection.
func seedOccurrence(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	seedCounter++
	now := time.Now().UTC().Format(time.RFC3339Nano)
	metricsConnectionID, _, _ := seedThanosChain(t, db)
	contractID := seedActiveAnalysisContract(t, db, now)
	key := fmt.Sprintf("business-%d", seedCounter)
	business, err := db.Exec(`INSERT INTO business_systems(key,display_name,enabled,created_at) VALUES(?,?,0,?)`, key, key, now)
	if err != nil {
		t.Fatal(err)
	}
	businessID, _ := business.LastInsertId()
	declaration, err := json.Marshal(map[string]any{"systemKey": key, "displayName": key, "MetricsConnectionID": metricsConnectionID, "resources": []any{map[string]any{"name": "default", "displayName": "Default", "matchLabels": map[string]string{"business_system": key}, "discoveryMetric": "up", "identityLabels": []string{"instance"}, "allowedMetrics": []string{"up"}}}})
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.Exec(`INSERT INTO business_system_config_versions(business_system_id,version_seq,state,yaml_body,parser_version,schema_version,label_contract_version_id,declaration_json,journey_catalog_digest,journey_catalog_version,digest,created_at,system_key,display_name,metrics_connection_id,enabled,timezone) VALUES(?,1,'draft','fixture','fixture','v1',?,?,?,'fixture',?,?,?,?,?,1,'UTC')`, businessID, contractID, string(declaration), strings.Repeat("c", 64), strings.Repeat("b", 64), now, key, key, metricsConnectionID)
	if err != nil {
		t.Fatal(err)
	}
	versionID, _ := version.LastInsertId()
	if _, err := db.Exec(`UPDATE business_systems SET current_config_version_id=?,display_name=?,enabled=1,timezone='UTC',row_version=row_version+1 WHERE id=?`, versionID, key, businessID); err != nil {
		t.Fatal(err)
	}
	source, err := db.Exec(`INSERT INTO alert_sources(source_key,protocol,enabled,created_at) VALUES(?,'alertmanager',1,?)`, fmt.Sprintf("source-%d", seedCounter), now)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, _ := source.LastInsertId()
	labels := `{"alertname":"HighErrorRate","severity":"critical","business_system":"` + key + `"}`
	occurrence, err := db.Exec(`INSERT INTO alert_occurrences(source_id,fingerprint,starts_at,state,labels_canonical,labels_digest,business_system_id,first_seen_at,last_state_change_at) VALUES(?,?,?,'Firing',?,?,?,?,?)`, sourceID, []byte{0, 0, 0, 0, 0, 0, byte(seedCounter % 8), 1}, now, labels, sha256Hex(labels), businessID, now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := occurrence.LastInsertId()
	return id
}

// seedActiveAnalysisContract establishes the Label Contract required by the
// analysis snapshot. It is intentionally independent of the current business
// pointer so later publications cannot alter a previously created snapshot.
func seedActiveAnalysisContract(t *testing.T, db *sql.DB, now string) int64 {
	t.Helper()
	var existing int64
	if err := db.QueryRow(`SELECT id FROM label_contracts WHERE state='active' LIMIT 1`).Scan(&existing); err == nil {
		return existing
	}
	if _, err := db.Exec(`INSERT INTO label_contract_state(id,row_version,updated_at) SELECT 1,1,? WHERE NOT EXISTS (SELECT 1 FROM label_contract_state WHERE id=1)`, now); err != nil {
		t.Fatal(err)
	}
	contract, err := db.Exec(`INSERT INTO label_contracts(version,yaml_body,contract_json,digest,parser_version,schema_version,state,row_version,created_at) VALUES(1,'fixture','{"label_contract":{"business_system_label":"business_system"}}',?,'fixture','v1','draft',1,?)`, strings.Repeat("a", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	contractID, _ := contract.LastInsertId()
	if _, err := db.Exec(`INSERT INTO label_contract_activations(contract_id,expected_target_row_version,expected_state_row_version,items_json,created_at) VALUES(?,1,1,'[]',?)`, contractID, now); err != nil {
		t.Fatal(err)
	}
	return contractID
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
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	occurrenceID := seedOccurrence(t, db)
	seedProviderChain(t, db)
	created, err := service.Create(ctx, occurrenceID, 1, "cmd-platform-fault")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Attempts().BindToStream(ctx, created.AttemptID, "boot-1", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.AcceptAttempt(ctx, created.AttemptID, "boot-1", 1); err != nil {
		t.Fatal(err)
	}
	projected := false
	service.ProjectTerminalOutcome = func(ctx context.Context, conn *sql.Conn, sequence int64, succeeded bool, termination string) error {
		projected = true
		if sequence <= 0 || succeeded || termination != "worker_protocol_error" {
			t.Fatalf("terminal projection=%d/%v/%q", sequence, succeeded, termination)
		}
		_, err := conn.ExecContext(ctx, `INSERT INTO platform_faults(component,reason,state,first_seen_at,last_seen_at,last_execution_commit_sequence) VALUES('plinth','worker_protocol_error','Firing','2026-09-10T00:00:00Z','2026-09-10T00:00:00Z',?)`, sequence)
		return err
	}
	if err := service.CommitResult(ctx, Result{AttemptID: created.AttemptID, BootID: "boot-1", Epoch: 1, Succeeded: false, Termination: "worker_protocol_error"}); err != nil {
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
}

// seedObservationAnnotations attaches one immutable accepted Alertmanager item to
// an occurrence. The analysis snapshot must receive exactly these supplied
// annotations instead of inferring meaning from the alert name.
func seedObservationAnnotations(t *testing.T, db *sql.DB, occurrenceID int64, state string, annotations map[string]string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var sourceID int64
	if err := db.QueryRow(`SELECT source_id FROM alert_occurrences WHERE id=?`, occurrenceID).Scan(&sourceID); err != nil {
		t.Fatal(err)
	}
	credential, err := db.Exec(`INSERT INTO alert_source_credentials(source_id,digest,state,created_at) VALUES(?,?, 'Active', ?)`, sourceID, make([]byte, 32), now)
	if err != nil {
		t.Fatal(err)
	}
	credentialID, _ := credential.LastInsertId()
	body, err := json.Marshal(map[string]any{"alerts": []map[string]any{{"annotations": annotations}}})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := db.Exec(`INSERT INTO alert_deliveries(relay_id,source_id,credential_id,credential_snapshot_version,protocol,body,body_size_bytes,integrity,status,received_at,committed_at) VALUES(?,?,?,?, 'alertmanager',?,?, 'complete','processed',?,?)`, fmt.Sprintf("annotation-relay-%d-%s", occurrenceID, state), sourceID, credentialID, 1, body, len(body), now, now)
	if err != nil {
		t.Fatal(err)
	}
	deliveryID, _ := delivery.LastInsertId()
	item, err := db.Exec(`INSERT INTO alert_delivery_items(delivery_id,item_index,status,fingerprint,starts_at,labels_canonical) VALUES(?,0,'ok',?,?,?)`, deliveryID, make([]byte, 8), now, `{"alertname":"MallGUIAcceptanceProbe"}`)
	if err != nil {
		t.Fatal(err)
	}
	itemID, _ := item.LastInsertId()
	if _, err := db.Exec(`INSERT INTO alert_observations(delivery_id,delivery_item_id,occurrence_id,observed_state,starts_at_source,received_at,committed_at,effect) VALUES(?,?,?,?,?,?,?,?)`, deliveryID, itemID, occurrenceID, state, now, now, now, map[string]string{"firing": "initial_firing", "resolved": "resolved_first"}[state]); err != nil {
		t.Fatal(err)
	}
}

func TestRebuildInputPreservesSuppliedObservationAnnotations(t *testing.T) {
	for _, test := range []struct {
		name        string
		state       string
		annotations map[string]string
	}{
		{name: "firing", state: "firing", annotations: map[string]string{"summary": "controlled GUI acceptance probe", "description": "No true fault; this is a controlled test annotation."}},
		{name: "resolved", state: "resolved", annotations: map[string]string{"summary": "controlled GUI acceptance probe resolved", "description": "No true fault; this is a controlled test annotation."}},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := newTestDB(t)
			service := NewService(db)
			occurrenceID := seedOccurrence(t, db)
			seedObservationAnnotations(t, db, occurrenceID, test.state, test.annotations)
			seedProviderChain(t, db)

			created, err := service.Create(context.Background(), occurrenceID, 1, "cmd-annotations-"+test.state)
			if err != nil {
				t.Fatal(err)
			}
			canonical, err := service.RebuildInput(context.Background(), created.AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			// Dispatch rebuild is the production fence. It must reproduce the digest
			// frozen at admission even when Alertmanager supplied annotations.
			if _, err := service.Attempts().DispatchInputFor(context.Background(), created.AttemptID); err != nil {
				t.Fatalf("annotation-bearing input must dispatch: %v", err)
			}
			var input Input
			if err := json.Unmarshal(canonical, &input); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(input.Occurrence.Annotations, test.annotations) {
				t.Fatalf("annotations = %#v, want exact supplied %#v", input.Occurrence.Annotations, test.annotations)
			}
		})
	}
}

// TestRebuildInputRetainsLegacyAnnotationOmission proves the v1 renderer
// contract remains byte-stable. It lets already-Assigned attempts created
// before annotations entered the snapshot resume through reconnect replay.
func TestRebuildInputRetainsLegacyAnnotationOmission(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	occurrenceID := seedOccurrence(t, db)
	seedObservationAnnotations(t, db, occurrenceID, "firing", map[string]string{"summary": "new field"})
	seedProviderChain(t, db)
	created, err := service.Create(context.Background(), occurrenceID, 1, "cmd-legacy-annotations")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := service.RebuildInput(context.Background(), created.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	var legacy Input
	if err := json.Unmarshal(canonical, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy.Occurrence.Annotations = nil
	legacy.BusinessContext.Resources = nil
	legacyCanonical, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	// The production schema correctly freezes snapshots. This fixture models an
	// already-admitted v1 record from before annotations were part of the input.
	if _, err := db.Exec(`DROP TRIGGER trg_attempt_input_snapshots_no_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE attempt_input_snapshots SET renderer_version='initial-analysis-renderer-v1' WHERE attempt_id=?`, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	// Model a pre-cutover snapshot completely: legacy rebuilds retain their
	// independently frozen Label Contract item, unlike all new attempts.
	if _, err := db.Exec(`
		INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,label_contract_version_id)
		SELECT s.id,3,'label_contract',?,v.label_contract_version_id
		FROM attempt_input_snapshots s
		JOIN attempt_input_items i ON i.snapshot_id=s.id AND i.business_system_config_version_id IS NOT NULL
		JOIN business_system_config_versions v ON v.id=i.business_system_config_version_id
		WHERE s.attempt_id=?`, strings.Repeat("f", 64), created.AttemptID); err != nil {
		t.Fatal(err)
	}
	legacyCanonical, err = service.RebuildInput(context.Background(), created.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	legacyDigest := sha256.Sum256(legacyCanonical)
	if _, err := db.Exec(`UPDATE attempt_input_snapshots SET content_digest=? WHERE attempt_id=?`, hex.EncodeToString(legacyDigest[:]), created.AttemptID); err != nil {
		t.Fatal(err)
	}
	dispatch, err := service.Attempts().DispatchInputFor(context.Background(), created.AttemptID)
	if err != nil {
		t.Fatalf("legacy annotation-free snapshot must resume: %v", err)
	}
	if string(dispatch.CanonicalJSON) != string(legacyCanonical) {
		t.Fatalf("legacy canonical=%s, want=%s", dispatch.CanonicalJSON, legacyCanonical)
	}
}

func TestCreateDispatchAcceptSeal(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	occurrenceID := seedOccurrence(t, db)
	seedProviderChain(t, db)

	created, err := service.Create(ctx, occurrenceID, 1, "cmd-create-1")
	if err != nil {
		t.Fatal(err)
	}
	// The one-active invariant: a second create returns the same record
	// (DATA-ANALYSIS-001).
	replayed, err := service.Create(ctx, occurrenceID, 1, "cmd-create-2")
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
	if err := service.AcceptAttempt(ctx, created.AttemptID, "boot-1", 1); err != nil {
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
	err = service.CommitResult(ctx, Result{
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
	if err := service.CommitResult(ctx, Result{AttemptID: created.AttemptID, BootID: "boot-1", Epoch: 1, Succeeded: true, SchemaKind: OutputSchemaKind, Canonical: content, Digest: digest[:]}); err != nil {
		t.Fatalf("identical replay rejected: %v", err)
	}
	// A divergent late result after success still loses (DATA-TX-005).
	otherContent, _ := json.Marshal("另一个迟到的结论")
	otherDigest := sha256.Sum256(otherContent)
	if err := service.CommitResult(ctx, Result{AttemptID: created.AttemptID, BootID: "boot-1", Epoch: 1, Succeeded: true, SchemaKind: OutputSchemaKind, Canonical: otherContent, Digest: otherDigest[:]}); !errors.Is(err, ErrLateResult) {
		t.Fatalf("late result=%v", err)
	}
	// Re-analysis creates a NEW analysis after success.
	again, err := service.Create(ctx, occurrenceID, 1, "cmd-create-3")
	if err != nil || again.AnalysisID == created.AnalysisID {
		t.Fatalf("re-analysis=%+v err=%v", again, err)
	}
}

func TestRetryAfterFailure(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	occurrenceID := seedOccurrence(t, db)
	seedProviderChain(t, db)
	created, err := service.Create(ctx, occurrenceID, 1, "cmd-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Attempts().BindToStream(ctx, created.AttemptID, "boot-1", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.AcceptAttempt(ctx, created.AttemptID, "boot-1", 1); err != nil {
		t.Fatal(err)
	}
	// A rejected AgentComplete is reported by the runtime as invalid_response.
	// Once the underlying grant/runtime defect is repaired, this terminal
	// technical failure must admit a new queued analysis through the real
	// result-adjudication and retry service seam.
	if err := service.CommitResult(ctx, Result{AttemptID: created.AttemptID, BootID: "boot-1", Epoch: 1, Succeeded: false, Termination: "invalid_response"}); err != nil {
		t.Fatal(err)
	}
	detail, _ := service.Get(ctx, created.AnalysisID)
	if detail.State != "Failed" {
		t.Fatalf("state=%q", detail.State)
	}
	// The original failed record remains terminal and inspectable. Retry creates
	// a fresh analysis/attempt from the current eligible provider, which is the
	// recovery path after a repaired model configuration or runtime defect.
	retried, err := service.Retry(ctx, created.AnalysisID, 1, "cmd-retry-1")
	if err != nil {
		t.Fatalf("retry after technical failure: %v", err)
	}
	if retried.AnalysisID == created.AnalysisID || retried.AttemptID == created.AttemptID {
		t.Fatalf("retry must create fresh records: original=%+v retry=%+v", created, retried)
	}
	// A command id belongs to this retry and this failed source analysis only.
	// Reusing it as a create must not return the recovery result for an unrelated
	// operation, even when the occurrence is the same.
	if _, err := service.Create(ctx, occurrenceID, 1, "cmd-retry-1"); !errors.Is(err, ErrCommandReplayMismatch) {
		t.Fatalf("cross-operation replay=%v", err)
	}
	// Even the same operation cannot replay against a different requested
	// failed analysis; it must not return the first recovery's unrelated
	// records. Drive the recovery attempt to its own technical terminal state
	// so this exercises retry eligibility rather than an active-state conflict.
	if err := service.Attempts().BindToStream(ctx, retried.AttemptID, "boot-2", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.AcceptAttempt(ctx, retried.AttemptID, "boot-2", 1); err != nil {
		t.Fatal(err)
	}
	if err := service.CommitResult(ctx, Result{AttemptID: retried.AttemptID, BootID: "boot-2", Epoch: 1, Succeeded: false, Termination: "invalid_response"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Retry(ctx, retried.AnalysisID, 1, "cmd-retry-1"); !errors.Is(err, ErrCommandReplayMismatch) {
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
	again, err := service.Retry(ctx, created.AnalysisID, 1, "cmd-retry-1")
	if err != nil || again != retried {
		t.Fatalf("retry replay=%+v want=%+v err=%v", again, retried, err)
	}
}

func TestCancelVsSuccessCommitOrder(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	occurrenceID := seedOccurrence(t, db)
	seedProviderChain(t, db)
	created, err := service.Create(ctx, occurrenceID, 1, "cmd-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Attempts().BindToStream(ctx, created.AttemptID, "boot-1", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.AcceptAttempt(ctx, created.AttemptID, "boot-1", 1); err != nil {
		t.Fatal(err)
	}
	// Cancel-before-result: the fence commits, the late result loses.
	detail, _ := service.Get(ctx, created.AnalysisID)
	outcome, err := service.Cancel(ctx, created.AnalysisID, 1, detail.RowVersion, "cmd-cancel-1")
	if err != nil || outcome.State != "Running" {
		t.Fatalf("cancel outcome=%+v err=%v", outcome, err)
	}
	if !outcome.DispatchRequired || outcome.AttemptID != created.AttemptID {
		t.Fatalf("running cancel must require a runtime dispatch: %+v", outcome)
	}
	content, _ := json.Marshal("迟到结果")
	digest := sha256.Sum256(content)
	if err := service.CommitResult(ctx, Result{AttemptID: created.AttemptID, BootID: "boot-1", Epoch: 1, Succeeded: true, SchemaKind: OutputSchemaKind, Canonical: content, Digest: digest[:]}); !errors.Is(err, ErrLateResult) {
		t.Fatalf("late success=%v", err)
	}
	if err := service.CancelAck(ctx, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	detail, _ = service.Get(ctx, created.AnalysisID)
	if detail.State != "Cancelled" {
		t.Fatalf("state=%q", detail.State)
	}

	// Success-before-cancel: the seal wins and the fence answers the
	// completed object (HTTP-COMMAND-005).
	again, err := service.Create(ctx, occurrenceID, 1, "cmd-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Attempts().BindToStream(ctx, again.AttemptID, "boot-1", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.AcceptAttempt(ctx, again.AttemptID, "boot-1", 1); err != nil {
		t.Fatal(err)
	}
	sealAgentCall(t, db, again.AttemptID, "fixture-chat-1")
	content, _ = json.Marshal("成功结果")
	digest = sha256.Sum256(content)
	if err := service.CommitResult(ctx, Result{AttemptID: again.AttemptID, BootID: "boot-1", Epoch: 1, Succeeded: true, SchemaKind: OutputSchemaKind, Canonical: content, Digest: digest[:]}); err != nil {
		t.Fatal(err)
	}
	detail, _ = service.Get(ctx, again.AnalysisID)
	outcome, err = service.Cancel(ctx, again.AnalysisID, 1, detail.RowVersion, "cmd-cancel-2")
	if err != nil || outcome.State != "Succeeded" {
		t.Fatalf("cancel after success=%+v err=%v", outcome, err)
	}
	// A stale row version conflicts (a fresh command id with the outdated
	// version); a current version answers the completed object instead.
	if _, err := service.Cancel(ctx, again.AnalysisID, 1, detail.RowVersion-1, "cmd-cancel-3"); err == nil {
		t.Fatal("stale row version must conflict")
	}

	// A Queued attempt closes as Cancelled directly: no runtime dispatch
	// (DATA-ATTEMPT-003).
	queued, err := service.Create(ctx, occurrenceID, 1, "cmd-queued")
	if err != nil {
		t.Fatal(err)
	}
	queuedDetail, _ := service.Get(ctx, queued.AnalysisID)
	outcome, err = service.Cancel(ctx, queued.AnalysisID, 1, queuedDetail.RowVersion, "cmd-cancel-queued")
	if err != nil || outcome.State != "Cancelled" || outcome.DispatchRequired {
		t.Fatalf("queued cancel=%+v err=%v", outcome, err)
	}
	if _, err := service.Get(ctx, queued.AnalysisID); err != nil || outcome.AttemptID != queued.AttemptID {
		t.Fatalf("queued cancel attempt=%d err=%v", outcome.AttemptID, err)
	}
}

var _ = attempt.ErrLateResult
