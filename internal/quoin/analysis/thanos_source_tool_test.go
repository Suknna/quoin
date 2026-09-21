package analysis

// Source-level thanos_query authorization (ADR-0004): an admin-enabled
// metrics connection frozen at attempt creation is itself the read-only
// authority（business_system 授权已随 ADR-0012 整域退役）。Routing ambiguity
// is a recoverable model-visible preflight result, never a silent first-pick
// or an all-sources fan-out.

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
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/tools/thanos"
)

// seedSourceOccurrence inserts one firing occurrence（ADR-0012 归一语义列随
// 行冻结）：来源级主线，告警不携带任何业务视图关联。
func seedSourceOccurrence(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	seedCounter++
	now := time.Now().UTC().Format(time.RFC3339Nano)
	source, err := db.Exec(`INSERT INTO alert_sources(source_key,protocol,enabled,created_at) VALUES(?,'alertmanager',1,?)`, fmt.Sprintf("source-plain-%d", seedCounter), now)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, _ := source.LastInsertId()
	labels := `{"alertname":"HighErrorRate","severity":"critical"}`
	occurrence, err := db.Exec(`INSERT INTO alert_occurrences(source_id,fingerprint,starts_at,state,labels_canonical,labels_digest,severity,title,annotations_canonical,resource,first_seen_at,last_state_change_at) VALUES(?,?,?,'Firing',?,?,?,'HighErrorRate','{}','',?,?)`,
		sourceID, []byte{0, 0, 0, 0, 0, 0, byte(seedCounter % 8), 2}, now, labels, sha256Hex(labels), "critical", now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := occurrence.LastInsertId()
	return id
}

// pendingSourceThanosCall inserts one raw pending thanos_query Tool Call
// carrying the model proposal verbatim. It bypasses the offered-catalog
// validation on purpose: the catalog gains sourceRef in a versioned schema
// bump, while the authorization resolution below is exercisable now through
// the same production resolver CompleteModelCall invokes.
func pendingSourceThanosCall(t *testing.T, db *sql.DB, attemptID, modelCallID, callSeq int64, arguments string) int64 {
	t.Helper()
	// Seal the anchor model call first: the ledger only accepts pending Tool
	// Calls that follow a successful model call in the same Running attempt.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	response, err := json.Marshal(map[string]any{
		"assistantText": "", "finishReason": "tool_calls",
		"tool_calls": []any{map[string]any{
			"id": "raw-source-thanos-" + strconv.FormatInt(callSeq, 10), "name": thanos.QueryToolName,
			"arguments": json.RawMessage(arguments),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,1,?,?,?,?)`, modelCallID, string(response), sha256Hex(string(response)), "tool_calls", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE model_calls SET usage_json='{"input_tokens":1,"output_tokens":1,"total_tokens":2}',status='succeeded',ended_at=? WHERE id=? AND status='running'`, now, modelCallID); err != nil {
		t.Fatal(err)
	}
	insert, err := db.Exec(`INSERT INTO tool_calls(attempt_id,model_call_id,call_seq,tool_index,provider_tool_call_id,tool_name,tool_version,arguments_json,arguments_digest,execution_mode,failure_mode,status,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,'pending',?)`,
		attemptID, modelCallID, callSeq, 0, "raw-source-thanos-"+strconv.FormatInt(callSeq, 10), thanos.QueryToolName, thanos.QueryToolVersion, arguments, sha256Hex(arguments), "quoin_routed", "return_to_model", time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	toolCallID, _ := insert.LastInsertId()
	return toolCallID
}

// resolveThanosCall drives the production grant resolver exactly as the
// attempt machine does: composed on the service runner's guarded Tx through
// a dedicated registered test operation, never on a raw pool connection.
func resolveThanosCall(t *testing.T, db *sql.DB, service *Service, attemptID, toolCallID int64) (attempt.ToolResolution, error) {
	t.Helper()
	tool, ok := attempt.DefaultCatalogs().Implementation(thanos.QueryToolName)
	if !ok {
		t.Fatal("thanos_query missing from the assembled implementation table")
	}
	op, err := service.runner.Register(execution.Operation{
		Name:       "test.tool_grant." + strconv.FormatInt(toolCallID, 10),
		Class:      execution.ClassWrite,
		ObjectType: ObjectAnalysis,
		Authorize:  func(context.Context, *execution.Tx) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "test-tool-grant-" + strconv.FormatInt(toolCallID, 10),
		Actor:         execution.Principal{Kind: execution.PrincipalSystem, ID: 0},
		Source:        execution.Source{Kind: execution.SourceTask},
	})
	if err != nil {
		t.Fatal(err)
	}
	return execution.Execute(ctx, service.runner, op, func(tx *execution.Tx) (attempt.ToolResolution, error) {
		return service.Attempts().ToolGrantResolver(ctx, tx, attemptID, toolCallID, tool)
	}, func(attempt.ToolResolution) int64 { return attemptID })
}

func enabledMetricsNames(t *testing.T, db *sql.DB) []string {
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

func thanosSourceArguments(sourceRef, query string, withResourceRef bool) string {
	body := ""
	if withResourceRef {
		body += `"resourceRef":"default",`
	}
	if sourceRef != "" {
		body += `"sourceRef":` + strconv.Quote(sourceRef) + `,`
	}
	return `{` + body + `"query":` + strconv.Quote(query) + `}`
}

// runSourceThanosAttempt creates one source-mode analysis through the
// production create path and opens its first chat call.
func runSourceThanosAttempt(t *testing.T, db *sql.DB, service *Service, commandID string) (attemptID, callID int64) {
	t.Helper()
	attemptID, callID = runThanosAttempt(t, db, service, seedSourceOccurrence(t, db), commandID)
	return attemptID, callID
}

// TestSourceAnalysisCreatesAndRebuildsWithEnabledSource proves the no-
// business mainline end to end: creation succeeds, the canonical input
// freezes the enabled integration list (kind+name) instead of a business
// context, and the rebuild reproduces the frozen digest exactly.
func TestSourceAnalysisCreatesAndRebuildsWithEnabledSource(t *testing.T) {
	db, dbPath := newTestDB(t)
	service := newTestService(t, db, dbPath)
	seedProviderChain(t, db)
	seedThanosChain(t, db)
	attemptID, _ := runSourceThanosAttempt(t, db, service, "cmd-source-create")

	var frozenDigest string
	if err := db.QueryRow(`SELECT content_digest FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&frozenDigest); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := service.RebuildInput(context.Background(), attemptID)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(rebuilt)
	if hex.EncodeToString(sum[:]) != frozenDigest {
		t.Fatalf("source-mode rebuild drifted: %s != %s", hex.EncodeToString(sum[:]), frozenDigest)
	}
	var input struct {
		BusinessContext *map[string]any     `json:"businessContext"`
		Integrations    []map[string]string `json:"integrations"`
		Occurrence      map[string]any      `json:"occurrence"`
		ModelContract   map[string]any      `json:"modelContract"`
	}
	if err := json.Unmarshal(rebuilt, &input); err != nil {
		t.Fatal(err)
	}
	if input.BusinessContext != nil {
		t.Fatalf("source attempt leaked a business context: %v", input.BusinessContext)
	}
	names := enabledMetricsNames(t, db)
	if len(names) != 1 {
		t.Fatalf("fixture must hold exactly one enabled metrics connection, got %v", names)
	}
	if len(input.Integrations) != 1 || input.Integrations[0]["kind"] != "metrics" || input.Integrations[0]["name"] != names[0] {
		t.Fatalf("integrations=%+v, want the single enabled metrics connection %q", input.Integrations, names[0])
	}
	// The frozen integration list must survive later integration churn:
	// enabling another connection cannot re-interpret the frozen input.
	seedAdditionalThanosChain(t, db)
	again, err := service.RebuildInput(context.Background(), attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(rebuilt) {
		t.Fatalf("frozen integrations were re-interpreted after a new enablement: %s vs %s", again, rebuilt)
	}
}

// TestThanosSourceGrantResolvesByExplicitName proves the exact-name path:
// sourceRef names the connection, the grant freezes on it with no business
// system, and the canonical execution request carries query+sourceRef.
func TestThanosSourceGrantResolvesByExplicitName(t *testing.T) {
	db, dbPath := newTestDB(t)
	service := newTestService(t, db, dbPath)
	seedProviderChain(t, db)
	connectionID, _, _ := seedThanosChain(t, db)
	names := enabledMetricsNames(t, db)
	attemptID, callID := runSourceThanosAttempt(t, db, service, "cmd-source-explicit")
	toolCallID := pendingSourceThanosCall(t, db, attemptID, callID, 1, thanosSourceArguments(names[0], "up", false))

	resolution, err := resolveThanosCall(t, db, service, attemptID, toolCallID)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.PreflightCode != "" || len(resolution.Grants) != 1 {
		t.Fatalf("resolution=%+v, want one grant", resolution)
	}
	var purpose string
	var grantedConnection int64
	if err := db.QueryRow(`SELECT purpose,connection_id FROM attempt_connection_grants WHERE id=?`, resolution.Grants[0].GrantID).Scan(&purpose, &grantedConnection); err != nil {
		t.Fatal(err)
	}
	if purpose != "thanos_query" || grantedConnection != connectionID {
		t.Fatalf("grant purpose=%s connection=%v", purpose, grantedConnection)
	}
	var binding int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tool_call_connection_grants WHERE tool_call_id=? AND connection_grant_id=?`, toolCallID, resolution.Grants[0].GrantID).Scan(&binding); err != nil || binding != 1 {
		t.Fatalf("tool call binding=%d err=%v", binding, err)
	}
	var executionJSON string
	if err := db.QueryRow(`SELECT arguments_json FROM tool_call_execution_inputs WHERE tool_call_id=?`, toolCallID).Scan(&executionJSON); err != nil {
		t.Fatal(err)
	}
	var execution map[string]string
	if err := json.Unmarshal([]byte(executionJSON), &execution); err != nil {
		t.Fatal(err)
	}
	if execution["query"] != "up" || execution["sourceRef"] != names[0] {
		t.Fatalf("execution args=%v, want query+sourceRef", execution)
	}
}

// TestThanosSourceSingleCandidateResolvesWithoutName proves the unique-
// candidate rule: with exactly one enabled metrics connection an omitted
// sourceRef is deterministic, not a first-pick.
func TestThanosSourceSingleCandidateResolvesWithoutName(t *testing.T) {
	db, dbPath := newTestDB(t)
	service := newTestService(t, db, dbPath)
	seedProviderChain(t, db)
	seedThanosChain(t, db)
	attemptID, callID := runSourceThanosAttempt(t, db, service, "cmd-source-unique")
	toolCallID := pendingSourceThanosCall(t, db, attemptID, callID, 1, thanosSourceArguments("", "up", false))

	resolution, err := resolveThanosCall(t, db, service, attemptID, toolCallID)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.PreflightCode != "" || len(resolution.Grants) != 1 {
		t.Fatalf("resolution=%+v, want one grant", resolution)
	}
}

// TestThanosSourceAmbiguityReturnsRecoverablePreflight proves that several
// enabled sources without an explicit name stay a recoverable model-visible
// result: no grant, no silent selection.
func TestThanosSourceAmbiguityReturnsRecoverablePreflight(t *testing.T) {
	db, dbPath := newTestDB(t)
	service := newTestService(t, db, dbPath)
	seedProviderChain(t, db)
	seedThanosChain(t, db)
	seedAdditionalThanosChain(t, db)
	names := enabledMetricsNames(t, db)
	if len(names) < 2 {
		t.Fatalf("fixture needs two enabled sources, got %v", names)
	}
	attemptID, callID := runSourceThanosAttempt(t, db, service, "cmd-source-ambiguous")
	toolCallID := pendingSourceThanosCall(t, db, attemptID, callID, 1, thanosSourceArguments("", "up", false))

	resolution, err := resolveThanosCall(t, db, service, attemptID, toolCallID)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.PreflightCode != "target_ambiguous" {
		t.Fatalf("preflight=%+v, want target_ambiguous", resolution)
	}
	for _, name := range names {
		if !strings.Contains(resolution.PreflightDetail, name) {
			t.Fatalf("preflight detail %q must list candidate %q", resolution.PreflightDetail, name)
		}
	}
	var grants int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempt_connection_grants WHERE attempt_id=? AND purpose='thanos_query'`, attemptID).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("ambiguous routing created %d grants (err=%v)", grants, err)
	}
}

// TestThanosSourceUnknownNameReturnsRecoverablePreflight proves a misspelled
// sourceRef stays recoverable and names the available sources.
func TestThanosSourceUnknownNameReturnsRecoverablePreflight(t *testing.T) {
	db, dbPath := newTestDB(t)
	service := newTestService(t, db, dbPath)
	seedProviderChain(t, db)
	seedThanosChain(t, db)
	names := enabledMetricsNames(t, db)
	attemptID, callID := runSourceThanosAttempt(t, db, service, "cmd-source-miss")
	toolCallID := pendingSourceThanosCall(t, db, attemptID, callID, 1, thanosSourceArguments("does-not-exist", "up", false))

	resolution, err := resolveThanosCall(t, db, service, attemptID, toolCallID)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.PreflightCode != "target_not_found" || !strings.Contains(resolution.PreflightDetail, names[0]) {
		t.Fatalf("preflight=%+v, want target_not_found listing %q", resolution, names[0])
	}
}

// TestThanosSourceWithoutEnabledConnectionReturnsPreflight proves a disabled
// integration is a model-visible routing miss, not a crash or silent skip.
func TestThanosSourceWithoutEnabledConnectionReturnsPreflight(t *testing.T) {
	db, dbPath := newTestDB(t)
	service := newTestService(t, db, dbPath)
	seedProviderChain(t, db)
	connectionID, _, _ := seedThanosChain(t, db)
	attemptID, callID := runSourceThanosAttempt(t, db, service, "cmd-source-disabled")
	if _, err := db.Exec(`UPDATE connections SET enabled=0,row_version=row_version+1 WHERE id=?`, connectionID); err != nil {
		t.Fatal(err)
	}
	toolCallID := pendingSourceThanosCall(t, db, attemptID, callID, 1, thanosSourceArguments("", "up", false))

	resolution, err := resolveThanosCall(t, db, service, attemptID, toolCallID)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.PreflightCode != "no_mapping" || len(resolution.Grants) != 0 {
		t.Fatalf("preflight=%+v, want no_mapping without grants", resolution)
	}
}

// TestThanosSourceModeRejectsBusinessResourceRef proves the declaration
// vocabulary does not leak into source-level attempts.
func TestThanosSourceModeRejectsBusinessResourceRef(t *testing.T) {
	db, dbPath := newTestDB(t)
	service := newTestService(t, db, dbPath)
	seedProviderChain(t, db)
	seedThanosChain(t, db)
	attemptID, callID := runSourceThanosAttempt(t, db, service, "cmd-source-resourceref")
	toolCallID := pendingSourceThanosCall(t, db, attemptID, callID, 1, thanosSourceArguments("", "up", true))

	resolution, err := resolveThanosCall(t, db, service, attemptID, toolCallID)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.PreflightCode == "" || len(resolution.Grants) != 0 {
		t.Fatalf("resolution=%+v, want recoverable refusal", resolution)
	}
}

// TestThanosFrozenListStaysNarrowAfterNewEnablement proves the frozen
// source list grants nothing wider: an attempt resolves only the sources
// frozen at creation — a connection enabled afterwards stays outside the list.
func TestThanosFrozenListStaysNarrowAfterNewEnablement(t *testing.T) {
	db, dbPath := newTestDB(t)
	service := newTestService(t, db, dbPath)
	seedProviderChain(t, db)
	seedThanosChain(t, db)
	occurrenceID := seedOccurrence(t, db)
	// The attempt freezes ONLY the sources enabled at creation; the second
	// connection is enabled afterwards so it is genuinely outside the list.
	attemptID, callID := runThanosAttempt(t, db, service, occurrenceID, "cmd-frozen-foreign-source")
	additionalID := seedAdditionalThanosChain(t, db)
	var foreignName string
	if err := db.QueryRow(`SELECT name FROM connections WHERE id=?`, additionalID).Scan(&foreignName); err != nil {
		t.Fatal(err)
	}
	frozenName := enabledMetricsNames(t, db)[0]
	if foreignName == frozenName {
		t.Fatalf("fixture names must differ: %q", foreignName)
	}
	toolCallID := pendingSourceThanosCall(t, db, attemptID, callID, 1, thanosSourceArguments(foreignName, "up", false))

	resolution, err := resolveThanosCall(t, db, service, attemptID, toolCallID)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.PreflightCode != thanos.PreflightTargetNotFound || !strings.Contains(resolution.PreflightDetail, frozenName) {
		t.Fatalf("resolution=%+v, want target_not_found listing frozen candidate %q", resolution, frozenName)
	}

	// The frozen connection itself resolves normally (fresh attempt, one
	// pending Tool Call each).
	narrowAttempt, narrowCall := runThanosAttempt(t, db, service, seedOccurrence(t, db), "cmd-frozen-matching-source")
	okCall := pendingSourceThanosCall(t, db, narrowAttempt, narrowCall, 1, thanosSourceArguments(frozenName, "up", false))
	okResolution, err := resolveThanosCall(t, db, service, narrowAttempt, okCall)
	if err != nil {
		t.Fatal(err)
	}
	if okResolution.PreflightCode != "" || len(okResolution.Grants) != 1 {
		t.Fatalf("frozen-source resolution=%+v, want one grant", okResolution)
	}
}
