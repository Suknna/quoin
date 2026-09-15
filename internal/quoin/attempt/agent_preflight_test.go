package attempt

// Real CompleteModelCall preflight-path tests (ADR-0004): a recoverable
// source-resolution preflight must reach the model as a pending tool call
// with its preflight code — never fail the whole response for missing
// normalized execution inputs, which a preflight resolution deliberately
// does not write.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

func seedRunningAttemptWithFrozenCatalog(t *testing.T, db *sql.DB, service *Service) (int64, *FrozenCatalog) {
	t.Helper()
	attemptID, _ := seedAttempt(t, db)
	// The state machine is Queued -> Assigned -> Running; the Assigned hop
	// freezes the dispatch binding and every UPDATE carries the exact next
	// row_version (trg_execution_attempts_*).
	now := time.Now().UTC().Format(time.RFC3339Nano)
	leaseUntil := time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339Nano)
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id='boot-preflight',connection_epoch=1,lease_until=?,runtime_release_version='test',row_version=row_version+1 WHERE id=?`, leaseUntil, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE execution_attempts SET state='Running',accepted_at=?,row_version=row_version+1 WHERE id=?`, now, attemptID); err != nil {
		t.Fatal(err)
	}
	catalog, err := service.FrozenToolCatalog(context.Background(), attemptID)
	if err != nil {
		t.Fatal(err)
	}
	return attemptID, catalog
}

func beginThanosModelCall(t *testing.T, service *Service, db *sql.DB, attemptID int64, catalog *FrozenCatalog) int64 {
	t.Helper()
	digest, err := catalog.Digest()
	if err != nil {
		t.Fatal(err)
	}
	var snapshotDigest string
	if err := db.QueryRow(`SELECT content_digest FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&snapshotDigest); err != nil {
		t.Fatal(err)
	}
	promptSum := sha256.Sum256([]byte("fixture-prompt"))
	callID, err := service.BeginModelCall(context.Background(), BeginCall{
		AttemptID: attemptID, CallSeq: 1, ModelID: "fixture-chat-1",
		PromptDigest: fmt.Sprintf("%x", promptSum[:]), ToolSchemaDigest: digest,
		InputDigest: strings.Repeat("a", 64), RenderedDigest: strings.Repeat("b", 64),
		InputItems: []ModelInputItem{
			{Sequence: 1, ItemKind: "system_contract", ContentDigest: strings.Repeat("c", 64), Role: "system"},
			{Sequence: 2, ItemKind: "tool_schema", ContentDigest: strings.Repeat("d", 64), Role: "system"},
			{Sequence: 3, ItemKind: "snapshot", ContentDigest: snapshotDigest, Role: "system"},
		},
		ContextBudget: 4096, MaxOutput: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	return callID
}

func proposeThanosQuery(t *testing.T, query string) ProposedTool {
	t.Helper()
	arguments := []byte(fmt.Sprintf(`{"query":%q}`, query))
	sum := sha256.Sum256(arguments)
	return ProposedTool{
		ProviderIndex: 0, ProviderToolCallID: "fixture-call-1", ToolName: "thanos_query",
		ArgumentsJSON: arguments, ArgumentsDigest: hex.EncodeToString(sum[:]),
	}
}

func sealModelCall(t *testing.T, service *Service, attemptID, callID int64, proposed []ProposedTool) []ToolAuthorization {
	t.Helper()
	_, responseDigest, err := CanonicalChatResponseJSON("", proposed)
	if err != nil {
		t.Fatal(err)
	}
	authorizations, err := service.CompleteModelCall(context.Background(), CompleteCall{
		AttemptID: attemptID, CallID: callID, Outcome: "succeeded", FinishReason: "tool_calls",
		ProposedTools: proposed, ResponseDigest: responseDigest, ResponseComplete: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return authorizations
}

// A recoverable source-resolution preflight reaches the model: the whole
// response must NOT fail for missing normalized execution inputs, the tool
// call stays pending with its preflight code, and no execution inputs exist.
func TestCompleteModelCallReturnsSourceAmbiguityPreflightToModel(t *testing.T) {
	db := newTestDB(t)
	service := newTestService(t, db)
	service.ToolGrantResolver = func(ctx context.Context, conn execution.Executor, attemptID, toolCallID int64, tool ToolDef) (ToolResolution, error) {
		if tool.Name != "thanos_query" {
			return ToolResolution{}, fmt.Errorf("unexpected resolver call for %s", tool.Name)
		}
		return ToolResolution{PreflightCode: "target_ambiguous", PreflightDetail: "多个指标来源可用，请用 sourceRef 显式命名。"}, nil
	}
	attemptID, catalog := seedRunningAttemptWithFrozenCatalog(t, db, service)
	callID := beginThanosModelCall(t, service, db, attemptID, catalog)
	proposed := []ProposedTool{proposeThanosQuery(t, "up")}
	authorizations := sealModelCall(t, service, attemptID, callID, proposed)

	if len(authorizations) != 1 {
		t.Fatalf("authorizations=%d, want 1", len(authorizations))
	}
	authorization := authorizations[0]
	if authorization.PreflightCode != "target_ambiguous" || len(authorization.Grants) != 0 {
		t.Fatalf("authorization=%+v, want the recoverable preflight without grants", authorization)
	}
	if authorization.ExecutionArgumentsJSON != nil {
		t.Fatalf("preflight authorization carried execution inputs: %s", authorization.ExecutionArgumentsJSON)
	}
	var preflightCode, toolStatus string
	if err := db.QueryRow(`SELECT preflight_error_code,status FROM tool_calls WHERE id=?`, authorization.ToolCallID).
		Scan(&preflightCode, &toolStatus); err != nil {
		t.Fatal(err)
	}
	if preflightCode != "target_ambiguous" || toolStatus != "pending" {
		t.Fatalf("tool call = (%q,%q), want (ambiguous_source,pending)", preflightCode, toolStatus)
	}
	var modelStatus string
	if err := db.QueryRow(`SELECT status FROM model_calls WHERE id=?`, callID).Scan(&modelStatus); err != nil {
		t.Fatal(err)
	}
	if modelStatus != "succeeded" {
		t.Fatalf("model call status = %q, want succeeded", modelStatus)
	}
	var inputs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tool_call_execution_inputs WHERE tool_call_id=?`, authorization.ToolCallID).Scan(&inputs); err != nil {
		t.Fatal(err)
	}
	if inputs != 0 {
		t.Fatal("a preflight resolution wrote normalized execution inputs")
	}
}

// A real grant resolution freezes the normalized execution arguments and the
// authorization carries exactly those Quoin-scoped bytes.
func TestCompleteModelCallFreezesNormalizedExecutionInputs(t *testing.T) {
	db := newTestDB(t)
	service := newTestService(t, db)
	service.ToolGrantResolver = func(ctx context.Context, conn execution.Executor, attemptID, toolCallID int64, tool ToolDef) (ToolResolution, error) {
		scoped, _ := json.Marshal(map[string]string{"resourceRef": "prom-main", "query": "up"})
		sum := sha256.Sum256(scoped)
		if _, err := conn.ExecContext(ctx, `INSERT INTO tool_call_execution_inputs(tool_call_id,arguments_json,arguments_digest,created_at) VALUES(?,?,?,datetime('now'))`,
			toolCallID, string(scoped), hex.EncodeToString(sum[:])); err != nil {
			return ToolResolution{}, err
		}
		return ToolResolution{Grants: []ToolGrant{{GrantID: 71, ConnectionRevisionID: 7, CredentialGenerationID: 7, Purpose: "thanos_query"}}}, nil
	}
	attemptID, catalog := seedRunningAttemptWithFrozenCatalog(t, db, service)
	callID := beginThanosModelCall(t, service, db, attemptID, catalog)

	authorizations := sealModelCall(t, service, attemptID, callID, []ProposedTool{proposeThanosQuery(t, "up")})
	if len(authorizations) != 1 {
		t.Fatalf("authorizations=%d, want 1", len(authorizations))
	}
	authorization := authorizations[0]
	if authorization.PreflightCode != "" {
		t.Fatalf("grant resolution produced a preflight: %+v", authorization.PreflightCode)
	}
	if len(authorization.Grants) != 1 || !strings.Contains(string(authorization.ExecutionArgumentsJSON), "resourceRef") {
		t.Fatalf("authorization=%+v, want one grant with normalized execution arguments", authorization)
	}
}

// A grant resolution WITHOUT normalization stays fail-closed: the response
// fails instead of executing unscoped arguments (RUNTIME-AGENT-005).
func TestCompleteModelCallFailsClosedWithoutNormalizedInputs(t *testing.T) {
	db := newTestDB(t)
	service := newTestService(t, db)
	service.ToolGrantResolver = func(ctx context.Context, conn execution.Executor, attemptID, toolCallID int64, tool ToolDef) (ToolResolution, error) {
		return ToolResolution{Grants: []ToolGrant{{GrantID: 71, ConnectionRevisionID: 7, CredentialGenerationID: 7, Purpose: "thanos_query"}}}, nil
	}
	attemptID, catalog := seedRunningAttemptWithFrozenCatalog(t, db, service)
	callID := beginThanosModelCall(t, service, db, attemptID, catalog)

	_, responseDigest, err := CanonicalChatResponseJSON("", []ProposedTool{proposeThanosQuery(t, "up")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteModelCall(context.Background(), CompleteCall{
		AttemptID: attemptID, CallID: callID, Outcome: "succeeded", FinishReason: "tool_calls",
		ProposedTools: []ProposedTool{proposeThanosQuery(t, "up")}, ResponseDigest: responseDigest, ResponseComplete: true,
	}); err == nil || !strings.Contains(err.Error(), "normalized execution arguments missing") {
		t.Fatalf("err=%v, want fail-closed on missing normalization", err)
	}
}
