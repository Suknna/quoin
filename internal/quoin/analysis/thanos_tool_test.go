package analysis

// T11 tool-authorization and evidence-closure tests over the real frozen
// schema and the production service wiring: the thanos_query grant freezes
// inside the CompleteModelCall transaction, execution authorization
// re-checks the connection state, and the deterministic Evidence commits
// in the same transaction as the Tool Call terminal state
// (ARCH-INPUT-003, DATA-CONN-002, ARCH-TOOL-003, DATA-EVIDENCE-001).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/connections"
)

// seedThanosChain follows the production metrics lifecycle: a current passed
// probe is committed first, then Enable atomically records its qualification.
// Tests must not bypass this server-side admission fence.
func seedThanosChain(t *testing.T, db *sql.DB) (connectionID, revisionID, generationID int64) {
	t.Helper()
	var existingConnectionID, existingRevisionID, existingGenerationID int64
	err := db.QueryRow(`SELECT id,current_revision_id,current_credential_generation_id FROM connections WHERE type='thanos' AND enabled=1 AND revalidation_required=0`).Scan(&existingConnectionID, &existingRevisionID, &existingGenerationID)
	if err == nil {
		return existingConnectionID, existingRevisionID, existingGenerationID
	}
	if err != sql.ErrNoRows {
		t.Fatalf("existing thanos lookup: %v", err)
	}
	summary := createQualifiedThanos(t, db, fmt.Sprintf("thanos-%d", seedCounter), uint64(seedCounter+1))
	return summary.ID, summary.CurrentRevisionID, summary.CurrentGenerationID
}

func createQualifiedThanos(t *testing.T, db *sql.DB, name string, epoch uint64) connections.Summary {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT OR IGNORE INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`, make([]byte, 12), make([]byte, 16), now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO runtime_slots(slot,state,row_version,created_at) VALUES('plinth','unregistered',1,?)`, now); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM runtime_slots WHERE slot='plinth'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state == "unregistered" {
		credential, err := db.Exec(`INSERT INTO runtime_credentials(slot,generation,token_digest,confirmed_at,created_at) VALUES('plinth',1,?,?,?)`, make([]byte, 32), now, now)
		if err != nil {
			t.Fatal(err)
		}
		credentialID, _ := credential.LastInsertId()
		if _, err := db.Exec(`UPDATE runtime_slots SET state='registered',current_credential_id=?,row_version=row_version+1 WHERE slot='plinth'`, credentialID); err != nil {
			t.Fatal(err)
		}
	}
	connections.ProbeContractSource = func() string { return "analysis-test-probe-contract-v1" }
	if _, err := db.Exec(`INSERT OR IGNORE INTO users(id,username,display_name,role,enabled,password_phc,auth_revision,created_at,updated_at) VALUES(1,'test-admin','Test Admin','admin',1,'x',1,?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO maintenance_state(id,active,row_version) VALUES(1,0,1)`); err != nil {
		t.Fatal(err)
	}
	service := connections.NewService(db, func() ([]byte, error) { return []byte(strings.Repeat("k", 32)), nil })
	summary, err := service.Create(context.Background(), connections.CreateInput{Name: name, Type: connections.TypeThanos, NonSecretJSON: []byte(`{"type":"thanos","baseUrl":"http://thanos.test","authType":"none"}`)}, 1, "analysis-metrics-create-"+name)
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := service.StartProbe(context.Background(), summary.Name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, err := service.BindQueuedToStream(context.Background(), attemptID, "analysis-probe", epoch, time.Minute); err != nil || !ok {
		t.Fatalf("bind metrics probe: ok=%v err=%v", ok, err)
	}
	if err := service.AcceptProbe(context.Background(), attemptID, "analysis-probe", epoch); err != nil {
		t.Fatal(err)
	}
	if err := service.CommitProbeResult(context.Background(), attemptID, "analysis-probe", epoch, connections.TypedProbeResult{Outcome: "passed", ResultDigest: strings.Repeat("a", 64), StartedAt: now, FinishedAt: now}, &connections.TypedChild{Thanos: &connections.ThanosProbeChild{Query: "vector(1)", ResponseType: "vector", SampleCount: 1, SampleValue: "1", DetailJSON: `{"kind":"thanos"}`}}); err != nil {
		t.Fatal(err)
	}
	var probeID int64
	if err := db.QueryRow(`SELECT id FROM connection_probe_results WHERE attempt_id=?`, attemptID).Scan(&probeID); err != nil {
		t.Fatal(err)
	}
	enabled, err := service.Enable(context.Background(), summary.Name, summary.RowVersion, probeID, 1)
	if err != nil {
		t.Fatal(err)
	}
	return enabled
}

// runThanosAttempt creates one analysis through the production path and
// drives its attempt to Running, opening the first chat call.
func runThanosAttempt(t *testing.T, db *sql.DB, service *Service, occurrenceID int64, commandID string) (attemptID, callID int64) {
	t.Helper()
	created, err := service.Create(context.Background(), occurrenceID, 1, commandID)
	if err != nil {
		t.Fatal(err)
	}
	attemptID = created.AttemptID
	if err := service.Attempts().BindToStream(context.Background(), attemptID, "boot", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.Attempts().Accept(context.Background(), attemptID, "boot", 1); err != nil {
		t.Fatal(err)
	}
	snapshotDigest, err := service.Attempts().InputSnapshotDigest(context.Background(), attemptID)
	if err != nil {
		t.Fatal(err)
	}
	toolsDigest, err := attempt.CanonicalToolsDigest()
	if err != nil {
		t.Fatal(err)
	}
	callID, err = service.Attempts().BeginModelCall(context.Background(), attempt.BeginCall{
		AttemptID: attemptID, CallSeq: 1, RetrySeq: 0,
		ModelID:          "fixture-chat-1",
		PromptDigest:     strings.Repeat("a", 64),
		ToolSchemaDigest: toolsDigest,
		InputDigest:      strings.Repeat("b", 64), RenderedDigest: strings.Repeat("c", 64),
		InputItems: []attempt.ModelInputItem{
			{Sequence: 1, ItemKind: "system_contract", ContentDigest: strings.Repeat("d", 64), Role: "system"},
			{Sequence: 2, ItemKind: "tool_schema", ContentDigest: strings.Repeat("e", 64), Role: "system"},
			{Sequence: 3, ItemKind: "snapshot", ContentDigest: snapshotDigest, Role: "system"},
		},
		ContextBudget: 4096, MaxOutput: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	return attemptID, callID
}

// completeThanosProposal seals the model call carrying one proposed
// thanos_query and returns the durable authorization.
func completeThanosProposalWithQuery(t *testing.T, service *Service, attemptID, callID int64, query string) ([]attempt.ToolAuthorization, error) {
	t.Helper()
	arguments := []byte(`{"resourceRef":"default","query":` + strconv.Quote(query) + `}`)
	proposed := []attempt.ProposedTool{{
		ProviderIndex: 0, ProviderToolCallID: "call-agent-thanos",
		ToolName: "thanos_query", ArgumentsJSON: arguments, ArgumentsDigest: sha256Hex(string(arguments)),
	}}
	_, responseDigest, err := attempt.CanonicalChatResponseJSON("", proposed)
	if err != nil {
		t.Fatal(err)
	}
	return service.Attempts().CompleteModelCall(context.Background(), attempt.CompleteCall{
		AttemptID: attemptID, CallID: callID,
		Outcome: "succeeded", FinishReason: "tool_calls",
		AssistantText: "", ProposedTools: proposed,
		ResponseDigest: responseDigest, ResponseComplete: true,
		InputTokens: 12, OutputTokens: 8, TotalTokens: 20,
	})
}

func completeThanosProposal(t *testing.T, service *Service, attemptID, callID int64) attempt.ToolAuthorization {
	t.Helper()
	var systemKey string
	if err := service.DB().QueryRow(`
		SELECT config.system_key
		FROM attempt_input_snapshots snapshot
		JOIN attempt_input_items item ON item.snapshot_id=snapshot.id AND item.business_system_config_version_id IS NOT NULL
		JOIN business_system_config_versions config ON config.id=item.business_system_config_version_id
		WHERE snapshot.attempt_id=?`, attemptID).Scan(&systemKey); err != nil {
		t.Fatal(err)
	}
	arguments := []byte(`{"resourceRef":"default","query":"up{business_system=\"` + systemKey + `\"}"}`)
	proposed := []attempt.ProposedTool{{
		ProviderIndex: 0, ProviderToolCallID: "call-agent-thanos",
		ToolName: "thanos_query", ArgumentsJSON: arguments, ArgumentsDigest: sha256Hex(string(arguments)),
	}}
	_, responseDigest, err := attempt.CanonicalChatResponseJSON("", proposed)
	if err != nil {
		t.Fatal(err)
	}
	authorizations, err := service.Attempts().CompleteModelCall(context.Background(), attempt.CompleteCall{
		AttemptID: attemptID, CallID: callID,
		Outcome: "succeeded", FinishReason: "tool_calls",
		AssistantText: "", ProposedTools: proposed,
		ResponseDigest: responseDigest, ResponseComplete: true,
		InputTokens: 12, OutputTokens: 8, TotalTokens: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(authorizations) != 1 {
		t.Fatalf("authorizations=%+v", authorizations)
	}
	return authorizations[0]
}

// TestThanosGrantFreezesInToolCallTransaction proves the production wiring:
// the grant resolves inside the pending-row transaction and travels in the
// authorization (ARCH-INPUT-003).
func TestThanosGrantFreezesInToolCallTransaction(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	seedProviderChain(t, db)
	seedThanosChain(t, db)
	attemptID, callID := runThanosAttempt(t, db, service, seedOccurrence(t, db), "cmd-thanos-grant")
	authorization := completeThanosProposal(t, service, attemptID, callID)
	if len(authorization.Grants) != 1 || authorization.Grants[0].Purpose != "thanos_query" {
		t.Fatalf("authorization=%+v", authorization)
	}
	var bindings int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tool_call_connection_grants WHERE tool_call_id=? AND connection_grant_id=?`, authorization.ToolCallID, authorization.Grants[0].GrantID).Scan(&bindings); err != nil || bindings != 1 {
		t.Fatalf("bindings=%d err=%v", bindings, err)
	}
	var grantedBusiness, expectedBusiness int64
	if err := db.QueryRow(`SELECT business_system_id FROM attempt_connection_grants WHERE id=?`, authorization.Grants[0].GrantID).Scan(&grantedBusiness); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT occurrence.business_system_id FROM alert_occurrences occurrence JOIN initial_analyses analysis ON analysis.occurrence_id=occurrence.id JOIN execution_attempts attempt ON attempt.scope_id=analysis.id WHERE attempt.id=?`, attemptID).Scan(&expectedBusiness); err != nil || grantedBusiness != expectedBusiness {
		t.Fatalf("grant business=%d expected=%d err=%v", grantedBusiness, expectedBusiness, err)
	}
}

// TestThanosGrantIsReusedForMultipleCallsInOneResponse covers the production
// AgentComplete seam behind a real model response with two metrics calls. One
// immutable attempt binding authorizes both Tool Calls: the grant's frozen
// revision/generation must be reused, while each Tool Call retains its own
// authorization link.
func TestThanosGrantIsReusedForMultipleCallsInOneResponse(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	seedProviderChain(t, db)
	seedThanosChain(t, db)
	attemptID, callID := runThanosAttempt(t, db, service, seedOccurrence(t, db), "cmd-thanos-grant-reuse")

	var systemKey string
	if err := db.QueryRow(`
		SELECT config.system_key
		FROM attempt_input_snapshots snapshot
		JOIN attempt_input_items item ON item.snapshot_id=snapshot.id AND item.business_system_config_version_id IS NOT NULL
		JOIN business_system_config_versions config ON config.id=item.business_system_config_version_id
		WHERE snapshot.attempt_id=?`, attemptID).Scan(&systemKey); err != nil {
		t.Fatal(err)
	}
	firstArguments := []byte(`{"resourceRef":"default","query":"up{business_system=\"` + systemKey + `\"}"}`)
	secondArguments := []byte(`{"resourceRef":"default","query":"up{business_system=\"` + systemKey + `\"}"}`)
	proposed := []attempt.ProposedTool{
		{ProviderIndex: 0, ProviderToolCallID: "call-agent-thanos-first", ToolName: "thanos_query", ArgumentsJSON: firstArguments, ArgumentsDigest: sha256Hex(string(firstArguments))},
		{ProviderIndex: 1, ProviderToolCallID: "call-agent-thanos-second", ToolName: "thanos_query", ArgumentsJSON: secondArguments, ArgumentsDigest: sha256Hex(string(secondArguments))},
	}
	_, responseDigest, err := attempt.CanonicalChatResponseJSON("", proposed)
	if err != nil {
		t.Fatal(err)
	}
	authorizations, err := service.Attempts().CompleteModelCall(context.Background(), attempt.CompleteCall{
		AttemptID: attemptID, CallID: callID, Outcome: "succeeded", FinishReason: "tool_calls",
		ProposedTools: proposed, ResponseDigest: responseDigest, ResponseComplete: true,
		InputTokens: 12, OutputTokens: 8, TotalTokens: 20,
	})
	if err != nil {
		t.Fatalf("complete response with two thanos calls: %v", err)
	}
	if len(authorizations) != 2 || len(authorizations[0].Grants) != 1 || len(authorizations[1].Grants) != 1 {
		t.Fatalf("authorizations=%+v", authorizations)
	}
	if authorizations[0].Grants[0] != authorizations[1].Grants[0] {
		t.Fatalf("grants differ: first=%+v second=%+v", authorizations[0].Grants[0], authorizations[1].Grants[0])
	}
	var bindingCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tool_call_connection_grants WHERE connection_grant_id=?`, authorizations[0].Grants[0].GrantID).Scan(&bindingCount); err != nil || bindingCount != 2 {
		t.Fatalf("grant bindings=%d err=%v", bindingCount, err)
	}
}

// A model-supplied conflicting selector must fail before grant creation;
// prompt text cannot widen the frozen business declaration's resource scope.
func TestThanosToolRejectsUnsafeQueryWithoutGrant(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	seedProviderChain(t, db)
	seedThanosChain(t, db)
	attemptID, callID := runThanosAttempt(t, db, service, seedOccurrence(t, db), "cmd-thanos-unsafe")
	if _, err := completeThanosProposalWithQuery(t, service, attemptID, callID, "up{business_system=\"other\"}"); err == nil {
		t.Fatal("complete must reject a conflicting business selector")
	}
	var grants int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempt_connection_grants WHERE attempt_id=? AND purpose='thanos_query'`, attemptID).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("grants=%d err=%v", grants, err)
	}
}

// TestThanosToolRejectedWithoutAuthorizationTarget proves the
// tool-before-authorization rejection: without an enabled Thanos
// connection the whole model call is refused and no tool call rows exist
// (RUNTIME-AGENT-005: an unresolvable tool route is invalid_response).
func TestThanosToolRejectedWithoutAuthorizationTarget(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	seedProviderChain(t, db)
	attemptID, callID := runThanosAttempt(t, db, service, seedOccurrence(t, db), "cmd-thanos-reject")
	var systemKey string
	if err := db.QueryRow(`SELECT config.system_key FROM attempt_input_snapshots snapshot JOIN attempt_input_items item ON item.snapshot_id=snapshot.id AND item.business_system_config_version_id IS NOT NULL JOIN business_system_config_versions config ON config.id=item.business_system_config_version_id WHERE snapshot.attempt_id=?`, attemptID).Scan(&systemKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE connections SET enabled=0,row_version=row_version+1 WHERE id=(SELECT config.metrics_connection_id FROM attempt_input_snapshots snapshot JOIN attempt_input_items item ON item.snapshot_id=snapshot.id AND item.business_system_config_version_id IS NOT NULL JOIN business_system_config_versions config ON config.id=item.business_system_config_version_id WHERE snapshot.attempt_id=?)`, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := completeThanosProposalWithQuery(t, service, attemptID, callID, `up{business_system="`+systemKey+`"}`); err == nil {
		t.Fatal("complete must reject a thanos_query without an enabled connection")
	}
	var toolRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tool_calls WHERE attempt_id=?`, attemptID).Scan(&toolRows); err != nil || toolRows != 0 {
		t.Fatalf("toolRows=%d err=%v", toolRows, err)
	}
}

// TestThanosBeginToolCallExecutionFence proves the execution authorization
// re-reads the connection state (DATA-CONN-002): a disable committed after
// the grant refuses BeginToolCall and the tool call stays pending.
// TestThanosQueryUsesAnalysisSnapshotAfterNewPublish proves a new business
// publish cannot redirect an already-created analysis to its newer connection.
func TestThanosQueryUsesAnalysisSnapshotAfterNewPublish(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	seedProviderChain(t, db)
	originalConnection, _, _ := seedThanosChain(t, db)
	occurrenceID := seedOccurrence(t, db)
	attemptID, callID := runThanosAttempt(t, db, service, occurrenceID, "cmd-thanos-fixed-snapshot")

	var businessID, oldVersionID, contractID int64
	var systemKey, displayName string
	if err := db.QueryRow(`
		SELECT occurrence.business_system_id, config.id, config.label_contract_version_id, config.system_key, config.display_name
		FROM alert_occurrences occurrence
		JOIN business_systems business ON business.id=occurrence.business_system_id
		JOIN business_system_config_versions config ON config.id=business.current_config_version_id
		WHERE occurrence.id=?`, occurrenceID).Scan(&businessID, &oldVersionID, &contractID, &systemKey, &displayName); err != nil {
		t.Fatal(err)
	}
	newConnection := seedAdditionalThanosChain(t, db)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	declaration, err := json.Marshal(map[string]any{"systemKey": systemKey, "displayName": displayName, "MetricsConnectionID": newConnection, "resources": []any{map[string]any{"name": "default", "displayName": "Default", "matchLabels": map[string]string{"business_system": systemKey}, "discoveryMetric": "up", "identityLabels": []string{"instance"}, "allowedMetrics": []string{"up"}}}})
	if err != nil {
		t.Fatal(err)
	}
	draft, err := db.Exec(`INSERT INTO business_system_config_versions(business_system_id,version_seq,state,yaml_body,parser_version,schema_version,label_contract_version_id,declaration_json,journey_catalog_digest,journey_catalog_version,digest,created_at,system_key,display_name,metrics_connection_id,enabled,timezone) VALUES(?,2,'draft','fixture','fixture','v1',?,?,?,'fixture',?,?,?,?,?,1,'UTC')`, businessID, contractID, string(declaration), strings.Repeat("c", 64), strings.Repeat("d", 64), now, systemKey, displayName, newConnection)
	if err != nil {
		t.Fatal(err)
	}
	newVersionID, _ := draft.LastInsertId()
	if _, err := db.Exec(`UPDATE business_systems SET current_config_version_id=?,display_name=?,enabled=1,timezone='UTC',row_version=row_version+1 WHERE id=?`, newVersionID, displayName, businessID); err != nil {
		t.Fatal(err)
	}
	authorization := completeThanosProposal(t, service, attemptID, callID)
	var grantedConnection int64
	if err := db.QueryRow(`SELECT connection_id FROM attempt_connection_grants WHERE id=?`, authorization.Grants[0].GrantID).Scan(&grantedConnection); err != nil || grantedConnection != originalConnection {
		t.Fatalf("grant connection=%d, want frozen original=%d (err=%v)", grantedConnection, originalConnection, err)
	}
	var snapshotVersion int64
	if err := db.QueryRow(`SELECT business_system_config_version_id FROM attempt_input_items WHERE snapshot_id=(SELECT id FROM attempt_input_snapshots WHERE attempt_id=?) AND business_system_config_version_id IS NOT NULL`, attemptID).Scan(&snapshotVersion); err != nil || snapshotVersion != oldVersionID {
		t.Fatalf("snapshot version=%d, want original=%d (err=%v)", snapshotVersion, oldVersionID, err)
	}
}

func seedAdditionalThanosChain(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	return createQualifiedThanos(t, db, fmt.Sprintf("thanos-extra-%d", seedCounter), uint64(seedCounter+100)).ID
}

// TestThanosBeginToolCallExecutionFence proves the execution authorization
// re-reads the connection state (DATA-CONN-002): a disable committed after
// the grant refuses BeginToolCall and the tool call stays pending.
func TestThanosBeginToolCallExecutionFence(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	seedProviderChain(t, db)
	seedThanosChain(t, db)
	attemptID, callID := runThanosAttempt(t, db, service, seedOccurrence(t, db), "cmd-thanos-fence")
	authorization := completeThanosProposal(t, service, attemptID, callID)
	var connectionID int64
	if err := db.QueryRow(`SELECT connection_id FROM attempt_connection_grants WHERE id=?`, authorization.Grants[0].GrantID).Scan(&connectionID); err != nil {
		t.Fatal(err)
	}
	var connectionName string
	var connectionVersion int64
	if err := db.QueryRow(`SELECT name,row_version FROM connections WHERE id=?`, connectionID).Scan(&connectionName, &connectionVersion); err != nil {
		t.Fatal(err)
	}
	connectionService := connections.NewService(db, func() ([]byte, error) { return []byte(strings.Repeat("k", 32)), nil })
	if _, err := connectionService.Disable(context.Background(), connectionName, connectionVersion); err != nil {
		t.Fatal(err)
	}
	if err := service.Attempts().BeginToolCall(context.Background(), attemptID, authorization.ToolCallID); err == nil {
		t.Fatal("begin must refuse a disabled connection")
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM tool_calls WHERE id=?`, authorization.ToolCallID).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("status=%q err=%v", status, err)
	}
	// Disabled is terminal for this credential route until a new successful
	// qualification and Enable command. The pending call stays denied rather
	// than re-enabling it with an unchecked SQL update.
}

// TestThanosEvidenceCommitsWithToolCallTerminalState proves the success
// transaction: tool call terminal state, deterministic Evidence and the
// grant binding commit atomically, and the Evidence detail projects the
// producer and connection facts (ARCH-TOOL-003, DATA-EVIDENCE-001).
func TestThanosEvidenceCommitsWithToolCallTerminalState(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	seedProviderChain(t, db)
	seedThanosChain(t, db)
	attemptID, callID := runThanosAttempt(t, db, service, seedOccurrence(t, db), "cmd-thanos-evidence")
	authorization := completeThanosProposal(t, service, attemptID, callID)
	if err := service.Attempts().BeginToolCall(context.Background(), attemptID, authorization.ToolCallID); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Format(time.RFC3339Nano)
	payload, err := json.Marshal(map[string]any{
		"success": true, "status": "success", "resultType": "vector", "sampleCount": 1,
		"startedAt": started, "finishedAt": started,
		"truncated": false, "totalBytes": 32, "totalLines": 1, "output": `{"status":"success"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	evidenceIDs, err := service.Attempts().CompleteToolCall(context.Background(), attempt.ToolResult{
		AttemptID: attemptID, ToolCallID: authorization.ToolCallID,
		Outcome: "succeeded", ResultJSON: string(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evidenceIDs) != 1 {
		t.Fatalf("evidence ids=%v", evidenceIDs)
	}
	var toolStatus string
	if err := db.QueryRow(`SELECT status FROM tool_calls WHERE id=?`, authorization.ToolCallID).Scan(&toolStatus); err != nil || toolStatus != "succeeded" {
		t.Fatalf("toolStatus=%q err=%v", toolStatus, err)
	}
	detail, err := service.Evidence().Get(context.Background(), evidenceIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	producer, ok := detail.Producer.(map[string]any)
	if !ok || producer["toolName"] != "thanos_query" {
		t.Fatalf("producer=%v", detail.Producer)
	}
	if len(detail.Connections) != 1 || detail.Connections[0].Type != "thanos" {
		t.Fatalf("connections=%v", detail.Connections)
	}
}

// TestThanosSpilledResultBindsEvidenceToArtifact proves the long-output
// closure: the evidence body is exactly the committed tool_result Artifact
// (DATA-EVIDENCE-001: 正文位置恰好一个).
func TestThanosSpilledResultBindsEvidenceToArtifact(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	seedProviderChain(t, db)
	seedThanosChain(t, db)
	attemptID, callID := runThanosAttempt(t, db, service, seedOccurrence(t, db), "cmd-thanos-spill")
	authorization := completeThanosProposal(t, service, attemptID, callID)
	if err := service.Attempts().BeginToolCall(context.Background(), attemptID, authorization.ToolCallID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	blob, err := db.Exec(`INSERT INTO artifact_blobs(sha256,size_bytes,storage_key,created_at) VALUES(?,?,?,?)`, strings.Repeat("a", 64), 70000, "blobs/test", now)
	if err != nil {
		t.Fatal(err)
	}
	blobID, _ := blob.LastInsertId()
	artifact, err := db.Exec(`INSERT INTO artifacts(blob_id,kind,media_type,sensitive,retention_kind,owner_type,owner_id,expires_at,created_at) VALUES(?,'tool_result','application/json',0,'generated','tool_call',?,?,?)`, blobID, authorization.ToolCallID, now, now)
	if err != nil {
		t.Fatal(err)
	}
	artifactID, _ := artifact.LastInsertId()
	payload, err := json.Marshal(map[string]any{
		"success": true, "status": "success", "resultType": "matrix", "sampleCount": 900,
		"startedAt": now, "finishedAt": now,
		"truncated": true, "totalBytes": 70000, "totalLines": 3600, "output": "…（完整输出已存入 Artifact）\nhead",
		"artifact": map[string]any{"id": fmt.Sprint(artifactID), "mediaType": "application/json", "sha256": strings.Repeat("a", 64), "sizeBytes": 70000, "totalLines": 3600},
	})
	if err != nil {
		t.Fatal(err)
	}
	evidenceIDs, err := service.Attempts().CompleteToolCall(context.Background(), attempt.ToolResult{
		AttemptID: attemptID, ToolCallID: authorization.ToolCallID,
		Outcome: "succeeded", ResultJSON: string(payload), ArtifactID: artifactID,
	})
	if err != nil {
		t.Fatal(err)
	}
	var bodyArtifactID int64
	var bodyJSON sql.NullString
	if err := db.QueryRow(`SELECT artifact_id,result_json FROM evidence WHERE id=?`, evidenceIDs[0]).Scan(&bodyArtifactID, &bodyJSON); err != nil {
		t.Fatal(err)
	}
	if bodyArtifactID != artifactID || bodyJSON.Valid {
		t.Fatalf("evidence body wrong: artifact=%d json=%v", bodyArtifactID, bodyJSON.Valid)
	}
	var toolArtifactID int64
	if err := db.QueryRow(`SELECT result_artifact_id FROM tool_calls WHERE id=?`, authorization.ToolCallID).Scan(&toolArtifactID); err != nil || toolArtifactID != artifactID {
		t.Fatalf("toolArtifact=%d err=%v", toolArtifactID, err)
	}
}

// TestThanosResultPayloadShapeRejected proves the frozen result schema
// validation (RUNTIME-AGENT-008): a malformed thanos_query payload refuses
// the tool completion and leaves the call running (the runtime then seals
// the technical failure, never a fake observation).
func TestThanosResultPayloadShapeRejected(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	seedProviderChain(t, db)
	seedThanosChain(t, db)
	attemptID, callID := runThanosAttempt(t, db, service, seedOccurrence(t, db), "cmd-thanos-shape")
	authorization := completeThanosProposal(t, service, attemptID, callID)
	if err := service.Attempts().BeginToolCall(context.Background(), attemptID, authorization.ToolCallID); err != nil {
		t.Fatal(err)
	}
	// Truncated=true without an artifact locator violates the frozen shape.
	if _, err := service.Attempts().CompleteToolCall(context.Background(), attempt.ToolResult{
		AttemptID: attemptID, ToolCallID: authorization.ToolCallID,
		Outcome:    "succeeded",
		ResultJSON: `{"success":true,"status":"success","resultType":"matrix","sampleCount":1,"startedAt":"2026-01-01T00:00:00Z","finishedAt":"2026-01-01T00:00:00Z","truncated":true,"totalBytes":10,"totalLines":1,"output":"x"}`,
	}); err == nil {
		t.Fatal("complete must reject a malformed thanos result")
	}
	var toolStatus string
	if err := db.QueryRow(`SELECT status FROM tool_calls WHERE id=?`, authorization.ToolCallID).Scan(&toolStatus); err != nil || toolStatus != "running" {
		t.Fatalf("toolStatus=%q err=%v", toolStatus, err)
	}
}
