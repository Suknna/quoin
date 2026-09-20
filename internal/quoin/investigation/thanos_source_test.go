package investigation

// Source-level thanos_query authorization for direct chat (ADR-0004): a
// blank business key no longer means "no metrics authority". The enabled
// integrations are frozen into the attempt (input items + rendered input),
// and the thanos_query grant resolves against that frozen list with
// explicit-source semantics — ambiguity stays a recoverable Tool Result.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/tools/thanos"
)

// investigationAdminContext is the trusted-entry execution metadata the
// connection commands re-verify in-transaction (admin user 1, session 1).
func investigationAdminContext(t *testing.T) context.Context {
	t.Helper()
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "corr-investigation-source-seed",
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: 1},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-investigation-source-seed"},
		Session:       execution.SessionRef{ID: 1, AuthRevision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// seedThanosIntegration drives the production metrics lifecycle through the
// connections service (create -> probe -> passed -> qualified enable), the
// same server-side admission fence the analysis harness uses. Admin
// enablement IS the source authorization this slice relies on.
func seedThanosIntegration(t *testing.T, db *sql.DB, name string) (connectionID, revisionID, generationID int64) {
	t.Helper()
	now := testNow()
	if _, err := db.Exec(`INSERT OR IGNORE INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`, make([]byte, 12), make([]byte, 16), now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO users(id,username,display_name,role,enabled,initialized,password_phc,auth_revision,created_at,updated_at) VALUES(1,'test-admin','Test Admin','admin',1,1,'x',1,?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,1,randomblob(32),1,'investigation-source-test',?,?,?,?)`, now, now, "2036-09-15T00:00:00Z", "2036-09-22T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO maintenance_state(id,active,row_version) VALUES(1,0,1)`); err != nil {
		t.Fatal(err)
	}
	connections.ProbeContractSource = func() string { return "investigation-source-test-probe-v1" }
	service := newTestConnectionsService(t, db)
	summary, err := service.Create(investigationAdminContext(t), connections.CreateInput{Name: name, Type: connections.TypeThanos, NonSecretJSON: []byte(`{"type":"thanos","baseUrl":"http://thanos.test","authType":"none"}`)}, 1, "investigation-metrics-create-"+name)
	if err != nil {
		t.Fatal(err)
	}
	probeEpoch := uint64(time.Now().UnixNano())
	probeAttemptID, err := service.StartProbe(investigationAdminContext(t), summary.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, err := service.BindQueuedToStream(context.Background(), probeAttemptID, "investigation-probe", probeEpoch, time.Minute); err != nil || !ok {
		t.Fatalf("bind metrics probe: ok=%v err=%v", ok, err)
	}
	if err := service.AcceptProbe(context.Background(), probeAttemptID, "investigation-probe", probeEpoch); err != nil {
		t.Fatal(err)
	}
	if err := service.CommitProbeResult(context.Background(), probeAttemptID, "investigation-probe", probeEpoch, connections.TypedProbeResult{Outcome: "passed", ResultDigest: strings.Repeat("a", 64), StartedAt: now, FinishedAt: now}, &connections.TypedChild{Thanos: &connections.ThanosProbeChild{Query: "vector(1)", ResponseType: "vector", SampleCount: 1, SampleValue: "1", DetailJSON: `{"kind":"thanos"}`}}); err != nil {
		t.Fatal(err)
	}
	var probeID int64
	if err := db.QueryRow(`SELECT id FROM connection_probe_results WHERE attempt_id=?`, probeAttemptID).Scan(&probeID); err != nil {
		t.Fatal(err)
	}
	enabled, err := service.Enable(investigationAdminContext(t), summary.Name, summary.RowVersion, probeID, 1)
	if err != nil {
		t.Fatal(err)
	}
	return enabled.ID, enabled.CurrentRevisionID, enabled.CurrentGenerationID
}

func enabledSourceNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM connections WHERE type IN ('thanos','prometheus') AND enabled=1 AND revalidation_required=0 ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return names
}

// driveFirstCall opens the attempt's first chat call and returns the call id.
func driveFirstCall(t *testing.T, db *sql.DB, service *Service, attemptID int64) int64 {
	t.Helper()
	if err := bindRunning(t, db, attemptID); err != nil {
		t.Fatal(err)
	}
	frozen, err := service.Attempts().FrozenToolCatalog(context.Background(), attemptID)
	if err != nil {
		t.Fatal(err)
	}
	toolsDigest, err := frozen.Digest()
	if err != nil {
		t.Fatal(err)
	}
	var snapshotDigest string
	if err := db.QueryRow(`SELECT content_digest FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&snapshotDigest); err != nil {
		t.Fatal(err)
	}
	callID, err := service.Attempts().BeginModelCall(context.Background(), attempt.BeginCall{
		AttemptID: attemptID, CallSeq: 1, ModelID: "fixture-chat-1",
		PromptDigest: strings.Repeat("a", 64), ToolSchemaDigest: toolsDigest,
		InputDigest: strings.Repeat("b", 64), RenderedDigest: strings.Repeat("c", 64),
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
	return callID
}

// pendingSourceThanosCall seals the anchor model call and inserts one raw
// pending thanos_query Tool Call with the proposal verbatim.
func pendingSourceThanosCall(t *testing.T, db *sql.DB, attemptID, callID int64, arguments string) int64 {
	t.Helper()
	now := testNow()
	response, err := json.Marshal(map[string]any{
		"assistantText": "", "finishReason": "tool_calls",
		"tool_calls": []any{map[string]any{
			"id": "raw-source-thanos", "name": thanos.QueryToolName,
			"arguments": json.RawMessage(arguments),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,1,?,?,?,?)`, callID, string(response), fmt.Sprintf("%x", sha256.Sum256(response)), "tool_calls", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":1,"total_tokens":2}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, callID); err != nil {
		t.Fatal(err)
	}
	insert, err := db.Exec(`INSERT INTO tool_calls(attempt_id,model_call_id,call_seq,tool_index,provider_tool_call_id,tool_name,tool_version,arguments_json,arguments_digest,execution_mode,failure_mode,status,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,'pending',?)`,
		attemptID, callID, 1, 0, "raw-source-thanos", thanos.QueryToolName, thanos.QueryToolVersion, arguments, fmt.Sprintf("%x", sha256.Sum256([]byte(arguments))), "supervisor_typed", "return_to_model", now)
	if err != nil {
		t.Fatal(err)
	}
	toolCallID, _ := insert.LastInsertId()
	return toolCallID
}

func resolveSourceThanosCall(t *testing.T, db *sql.DB, service *Service, attemptID, toolCallID int64) (attempt.ToolResolution, error) {
	t.Helper()
	tool, ok := attempt.DefaultCatalogs().Implementation(thanos.QueryToolName)
	if !ok {
		t.Fatal("thanos_query missing from the assembled implementation table")
	}
	return resolveToolGrantOnRunner(t, service, "test.tool_grant.thanos."+strconv.FormatInt(toolCallID, 10),
		func(ctx context.Context, tx *execution.Tx) (attempt.ToolResolution, error) {
			return service.Attempts().ToolGrantResolver(ctx, tx, attemptID, toolCallID, tool)
		})
}

// newTestConnectionsService builds the connections fixture service with its
// read-only reader opened from the same fixture file (t.TempDir() is stable
// per test), so the probe lifecycle's pure reads share the fail-closed
// composition instead of the removed writer-pool fallback.
func newTestConnectionsService(t *testing.T, db *sql.DB) *connections.Service {
	t.Helper()
	service := connections.NewService(db, func() ([]byte, error) { return []byte(strings.Repeat("k", 32)), nil })
	reader, err := execution.OpenReadOnly(fixtureDBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	if err := service.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	return service
}

// TestSourceInvestigationFreezesIntegrations proves the blank-key mainline:
// the enabled integrations are frozen as input items and rendered into the
// canonical input, later enablement churn cannot re-interpret the snapshot,
// and the rebuild reproduces the frozen digest exactly.
func TestSourceInvestigationFreezesIntegrations(t *testing.T) {
	db, dbPath := newTestDB(t)
	service := newTestService(t, db, dbPath)
	principalID := seedUser(t, db)
	ctx := userContext(t, principalID)
	seedProviderChain(t, db)
	seedThanosIntegration(t, db, "thanos-source-a")
	created, err := service.Create(ctx, principalID, "cmd-source-investigation", "查一下流量错误率", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var frozenDigest string
	if err := db.QueryRow(`SELECT content_digest FROM attempt_input_snapshots WHERE attempt_id=?`, created.AttemptID).Scan(&frozenDigest); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := service.RebuildInput(ctx, created.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(rebuilt)
	if hex.EncodeToString(sum[:]) != frozenDigest {
		t.Fatalf("source investigation rebuild drifted: %s != %s", hex.EncodeToString(sum[:]), frozenDigest)
	}
	var input struct {
		BusinessContext *map[string]any     `json:"businessContext"`
		Integrations    []map[string]string `json:"integrations"`
	}
	if err := json.Unmarshal(rebuilt, &input); err != nil {
		t.Fatal(err)
	}
	if input.BusinessContext != nil {
		t.Fatalf("blank-key investigation leaked a business context: %v", input.BusinessContext)
	}
	if len(input.Integrations) != 1 || input.Integrations[0]["kind"] != "metrics" || input.Integrations[0]["name"] != "thanos-source-a" {
		t.Fatalf("integrations=%+v, want the single enabled metrics source", input.Integrations)
	}
	var sourceItems int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempt_input_items WHERE snapshot_id=(SELECT id FROM attempt_input_snapshots WHERE attempt_id=?) AND item_role='metrics_source'`, created.AttemptID).Scan(&sourceItems); err != nil || sourceItems != 1 {
		t.Fatalf("metrics_source items=%d err=%v", sourceItems, err)
	}
	// Later enablement must not re-interpret the frozen snapshot.
	seedThanosIntegration(t, db, "thanos-source-b")
	again, err := service.RebuildInput(ctx, created.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(rebuilt) {
		t.Fatalf("frozen integrations changed after a new enablement")
	}
}

// TestSourceInvestigationThanosGrantResolvesBySourceRef proves the explicit
// source path end to end through the investigation resolver wiring.
func TestSourceInvestigationThanosGrantResolvesBySourceRef(t *testing.T) {
	db, dbPath := newTestDB(t)
	service := newTestService(t, db, dbPath)
	principalID := seedUser(t, db)
	ctx := userContext(t, principalID)
	seedProviderChain(t, db)
	connectionID, _, _ := seedThanosIntegration(t, db, "thanos-source-a")
	created, err := service.Create(ctx, principalID, "cmd-source-grant", "查一下流量错误率", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	callID := driveFirstCall(t, db, service, created.AttemptID)
	toolCallID := pendingSourceThanosCall(t, db, created.AttemptID, callID, `{"sourceRef":"thanos-source-a","query":"up"}`)

	resolution, err := resolveSourceThanosCall(t, db, service, created.AttemptID, toolCallID)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.PreflightCode != "" || len(resolution.Grants) != 1 {
		t.Fatalf("resolution=%+v, want one grant", resolution)
	}
	var purpose string
	var grantedConnection, grantedBusiness sql.NullInt64
	if err := db.QueryRow(`SELECT purpose,connection_id,business_system_id FROM attempt_connection_grants WHERE id=?`, resolution.Grants[0].GrantID).Scan(&purpose, &grantedConnection, &grantedBusiness); err != nil {
		t.Fatal(err)
	}
	if purpose != thanos.QueryToolPurpose || grantedConnection.Int64 != connectionID || grantedBusiness.Valid {
		t.Fatalf("grant purpose=%s connection=%v business=%v", purpose, grantedConnection, grantedBusiness)
	}
	var executionJSON string
	if err := db.QueryRow(`SELECT arguments_json FROM tool_call_execution_inputs WHERE tool_call_id=?`, toolCallID).Scan(&executionJSON); err != nil {
		t.Fatal(err)
	}
	var execution map[string]string
	if err := json.Unmarshal([]byte(executionJSON), &execution); err != nil {
		t.Fatal(err)
	}
	if execution["query"] != "up" || execution["sourceRef"] != "thanos-source-a" {
		t.Fatalf("execution args=%v, want query+sourceRef", execution)
	}
}

// TestSourceInvestigationAmbiguousSourcesPreflight proves the frozen two-
// source list without an explicit name stays a recoverable Tool Result.
func TestSourceInvestigationAmbiguousSourcesPreflight(t *testing.T) {
	db, dbPath := newTestDB(t)
	service := newTestService(t, db, dbPath)
	principalID := seedUser(t, db)
	ctx := userContext(t, principalID)
	seedProviderChain(t, db)
	seedThanosIntegration(t, db, "thanos-source-a")
	seedThanosIntegration(t, db, "thanos-source-b")
	names := enabledSourceNames(t, db)
	if len(names) != 2 {
		t.Fatalf("fixture needs two enabled sources, got %v", names)
	}
	created, err := service.Create(ctx, principalID, "cmd-source-ambiguous", "查一下流量错误率", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	callID := driveFirstCall(t, db, service, created.AttemptID)
	toolCallID := pendingSourceThanosCall(t, db, created.AttemptID, callID, `{"query":"up"}`)

	resolution, err := resolveSourceThanosCall(t, db, service, created.AttemptID, toolCallID)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.PreflightCode != thanos.PreflightTargetAmbiguous {
		t.Fatalf("preflight=%+v, want target_ambiguous", resolution)
	}
	for _, name := range names {
		if !strings.Contains(resolution.PreflightDetail, name) {
			t.Fatalf("preflight detail %q must list %q", resolution.PreflightDetail, name)
		}
	}
	var grants int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempt_connection_grants WHERE attempt_id=? AND purpose='thanos_query'`, created.AttemptID).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("ambiguous routing created %d grants (err=%v)", grants, err)
	}
}
