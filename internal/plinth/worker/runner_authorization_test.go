package worker

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/plinth/model"
)

// 冻结目录解析出的执行模式表(ADR-0011:目录 executionMode 是唯一裁决源)。
func testExecutionModes() map[string]string {
	return map[string]string{
		"bash":          "TOOL_EXECUTION_MODE_WORKER_LOCAL",
		"thanos_query":  "TOOL_EXECUTION_MODE_QUOIN_ROUTED",
		"artifact_read": "TOOL_EXECUTION_MODE_QUOIN_ROUTED",
	}
}

func TestPrepareAuthorizedToolCallsKeepsProposalAndUsesScopedExecution(t *testing.T) {
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

	prepared, authorized, err := prepareAuthorizedToolCalls(proposed, authorizations, testExecutionModes())
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 1 || !bytes.Equal(prepared[0].GetArgumentsJson(), proposalJSON) {
		t.Fatalf("prepared call must preserve the original model proposal: %+v", prepared)
	}
	// ADR-0011:quoin_routed 工具 Plinth 只转发不执行;授权执行参数只做
	// 摘要一致性校验,元数据只剩路由必需的 name+mode。
	meta, ok := authorized[42]
	if !ok {
		t.Fatal("authorized metadata missing")
	}
	if meta.mode != "TOOL_EXECUTION_MODE_QUOIN_ROUTED" {
		t.Fatalf("execution mode=%q, want quoin_routed", meta.mode)
	}
	if prepared[0].GetExecutionMode().String() != "TOOL_EXECUTION_MODE_QUOIN_ROUTED" {
		t.Fatalf("prepared execution mode=%s, want quoin_routed", prepared[0].GetExecutionMode())
	}
}

func TestPrepareAuthorizedToolCallsRejectsMismatchedExecutionDigest(t *testing.T) {
	proposed := []model.ProposedTool{{ProviderIndex: 0, ToolName: "thanos_query", ArgumentsJSON: []byte(`{"resourceRef":"checkout","query":"up"}`)}}
	_, _, err := prepareAuthorizedToolCalls(proposed, []model.Authorization{{
		ToolCallID: 42, ProviderIndex: 0,
		ExecutionArgumentsJSON:   []byte(`{"resourceRef":"checkout","query":"up{namespace=\"payments\"}"}`),
		ExecutionArgumentsDigest: make([]byte, sha256.Size),
	}}, testExecutionModes())
	if err == nil || !strings.Contains(err.Error(), "execution arguments digest mismatch") {
		t.Fatalf("err=%v, want execution digest rejection", err)
	}
}

// 冻结目录没有的工具不得获得执行模式:模型只能调用目录内工具,授权数据
// 与目录漂移要在最早处暴露。
func TestPrepareAuthorizedToolCallsRejectsToolOutsideFrozenCatalog(t *testing.T) {
	proposed := []model.ProposedTool{{ProviderIndex: 0, ToolName: "mystery", ArgumentsJSON: []byte(`{}`)}}
	_, _, err := prepareAuthorizedToolCalls(proposed, []model.Authorization{{ToolCallID: 42, ProviderIndex: 0}}, testExecutionModes())
	if err == nil || !strings.Contains(err.Error(), "frozen catalog") {
		t.Fatalf("err=%v, want frozen-catalog rejection", err)
	}
}
