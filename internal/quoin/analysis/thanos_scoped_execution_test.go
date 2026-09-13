package analysis

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/config"
)

func TestThanosAuthorizationFreezesScopedExecutionWithoutRewritingProposal(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	seedProviderChain(t, db)
	seedThanosChain(t, db)
	attemptID, callID := runThanosAttempt(t, db, service, seedOccurrence(t, db), "cmd-thanos-scope-execution")
	authorizations, err := completeThanosProposalWithQuery(t, service, attemptID, callID, "up")
	if err != nil {
		t.Fatal(err)
	}
	if len(authorizations) != 1 {
		t.Fatalf("authorizations = %d, want one", len(authorizations))
	}
	authorization := authorizations[0]
	var proposalQuery, executionJSON, executionDigest, declarationJSON string
	if err := db.QueryRow(`SELECT json_extract(t.arguments_json,'$.query'),e.arguments_json,e.arguments_digest FROM tool_calls t JOIN tool_call_execution_inputs e ON e.tool_call_id=t.id WHERE t.id=?`, authorization.ToolCallID).Scan(&proposalQuery, &executionJSON, &executionDigest); err != nil {
		t.Fatal(err)
	}
	if proposalQuery != "up" {
		t.Fatalf("model proposal was rewritten: %q", proposalQuery)
	}
	if err := db.QueryRow(`SELECT c.declaration_json FROM attempt_input_snapshots s JOIN attempt_input_items i ON i.snapshot_id=s.id AND i.business_system_config_version_id IS NOT NULL JOIN business_system_config_versions c ON c.id=i.business_system_config_version_id WHERE s.attempt_id=?`, attemptID).Scan(&declarationJSON); err != nil {
		t.Fatal(err)
	}
	var declaration config.BusinessSystemDocument
	if err := json.Unmarshal([]byte(declarationJSON), &declaration); err != nil {
		t.Fatal(err)
	}
	scope, err := declaration.CompileResourceScope("default")
	if err != nil {
		t.Fatal(err)
	}
	expectedQuery, err := scope.ScopeExpression("up")
	if err != nil {
		t.Fatal(err)
	}
	var execution struct {
		ResourceRef string `json:"resourceRef"`
		Query       string `json:"query"`
	}
	if err := json.Unmarshal([]byte(executionJSON), &execution); err != nil {
		t.Fatal(err)
	}
	if execution.ResourceRef != "default" || execution.Query != expectedQuery || execution.Query == proposalQuery {
		t.Fatalf("execution did not acquire the frozen scope: %#v, want %q", execution, expectedQuery)
	}
	digest := sha256.Sum256([]byte(executionJSON))
	if executionDigest != hex.EncodeToString(digest[:]) || string(authorization.ExecutionArgumentsJSON) != executionJSON || hex.EncodeToString(authorization.ExecutionArgumentsDigest) != executionDigest {
		t.Fatal("authorization payload differs from the immutable execution request")
	}
}
