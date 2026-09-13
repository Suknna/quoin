package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/plinth/model"
)

func TestPrepareAuthorizedToolCallsKeepsProposalAndUsesScopedThanosExecution(t *testing.T) {
	proposalJSON := []byte(`{"resourceRef":"checkout","query":"up"}`)
	scopedJSON := []byte(`{"resourceRef":"checkout","query":"up{namespace=\"payments\"}"}`)
	digest := sha256.Sum256(scopedJSON)
	proposed := []model.ProposedTool{{
		ProviderIndex: 3, ProviderToolCallID: "provider-call", ToolName: "thanos_query",
		ArgumentsJSON: proposalJSON, ArgumentsDigest: "proposal-digest",
	}}
	authorizations := []model.Authorization{{
		ToolCallID: 42, ProviderIndex: 3, FailureMode: "TOOL_FAILURE_MODE_RETURN_TO_MODEL",
		ExecutionArgumentsJSON: scopedJSON, ExecutionArgumentsDigest: digest[:],
	}}

	prepared, authorized, err := prepareAuthorizedToolCalls(proposed, authorizations)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 1 || !bytes.Equal(prepared[0].GetArgumentsJson(), proposalJSON) {
		t.Fatalf("prepared call must preserve the original model proposal: %+v", prepared)
	}
	meta, ok := authorized[42]
	if !ok {
		t.Fatal("authorized metadata missing")
	}
	// ExecuteTool reads this metadata through the same Runner seam used by the
	// typed Thanos dispatcher; it must receive Quoin's scoped query, not up.
	runner := &Runner{tools: authorized}
	execution, err := runner.toolArguments(context.Background(), 1, 42)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := execution["query"], `up{namespace="payments"}`; got != want {
		t.Fatalf("execution query=%q, want Quoin-scoped %q", got, want)
	}
	if !bytes.Equal(meta.argumentsJSON, scopedJSON) {
		t.Fatalf("authorized execution JSON=%s, want %s", meta.argumentsJSON, scopedJSON)
	}
}

func TestPrepareAuthorizedToolCallsRejectsMismatchedExecutionDigest(t *testing.T) {
	proposed := []model.ProposedTool{{ProviderIndex: 0, ToolName: "thanos_query", ArgumentsJSON: []byte(`{"resourceRef":"checkout","query":"up"}`)}}
	_, _, err := prepareAuthorizedToolCalls(proposed, []model.Authorization{{
		ToolCallID: 42, ProviderIndex: 0,
		ExecutionArgumentsJSON:   []byte(`{"resourceRef":"checkout","query":"up{namespace=\"payments\"}"}`),
		ExecutionArgumentsDigest: make([]byte, sha256.Size),
	}})
	if err == nil || !strings.Contains(err.Error(), "execution arguments digest mismatch") {
		t.Fatalf("err=%v, want execution digest rejection", err)
	}
}

func TestPrepareAuthorizedToolCallsRejectsThanosWithoutAuthorizedOverride(t *testing.T) {
	proposed := []model.ProposedTool{{ProviderIndex: 0, ToolName: "thanos_query", ArgumentsJSON: []byte(`{"resourceRef":"checkout","query":"up"}`)}}
	_, _, err := prepareAuthorizedToolCalls(proposed, []model.Authorization{{ToolCallID: 42, ProviderIndex: 0}})
	if err == nil || !strings.Contains(err.Error(), "lacks Quoin-authorized execution arguments") {
		t.Fatalf("err=%v, want missing Thanos override rejection", err)
	}
}
