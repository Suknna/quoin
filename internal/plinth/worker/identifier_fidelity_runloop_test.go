package worker

// 盲测补修 4 的 final 化链路回归：用脚本化的真实模型响应帧（ChatModelStarted/
// ChatModelCompleted/ToolResult，即 supervisor 实际转发给 worker 的载荷形状）驱动
// 真实 runLoop，断言三件事：
//  1. 第一轮 ChatModelRequest 的 messages_json（模型实际可见消息）携带标识符
//     逐字约束的系统提示词（补修前为红）；
//  2. 第二轮 ChatModelRequest 的 messages_json 携带 alerts_recent 工具结果的
//     原字节——模型写最终诊断时正确拼写确实在其上下文中（失效形态的证据回归）；
//  3. 最终诊断段含改写标识符的模型响应被逐字封存进 WorkerResultProposal
//     （不篡改正文——本仓不可变输出边界的守护断言），Evidence 引用按封存结果
//     透传。
// fixture 与 agent 包 identifier_fidelity_prompt_test.go 共用一套通用值；不含
// 真实业务名/地址。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	workerv1 "github.com/Suknna/quoin/internal/gen/proto/plinth/worker/v1"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
)

const runLoopFixtureCanonical = `{
	"occurrence": {
		"id": "3",
		"state": "Resolved",
		"firstSeenAt": "2026-09-21T16:07:14.883Z",
		"lastStateChangeAt": "2026-09-21T16:12:14.884Z",
		"resolvedAt": "2026-09-21T16:12:14.884Z",
		"labels": {
			"alertname": "FixtureShopCacheUnavailable",
			"instance": "192.0.2.10:9121",
			"job": "fixture-cache-exporter",
			"severity": "critical",
			"system_id": "fixture-shop"
		},
		"annotations": {
			"description": "cache_up=0 observed by exporter; the service itself may still be healthy",
			"summary": "fixture-shop cache not serving commands"
		}
	},
	"integrations": [{"kind": "metrics", "name": "fixture-shop-prometheus"}],
	"modelContract": {"modelId": "fixture-chat-1", "contextBudgetTokens": 32000, "maxOutputTokens": 4096},
	"toolCatalog": {
		"tools": [
			{
				"name": "alerts_recent",
				"version": "1",
				"executionMode": "quoin_routed",
				"failureMode": "return_to_model",
				"resultSchemaKind": "alerts_recent_result_v1",
				"description": "查询 Quoin 告警库中的近期告警（归一化语义）：可按业务视图、最低 severity、时间窗过滤，返回 id/severity/title/state/startsAt/labels 摘要。用于分析时获取相关告警上下文。",
				"parameters": {
					"type": "object",
					"properties": {
						"hours": {"type": "number", "minimum": 1, "maximum": 168, "description": "时间窗（小时），按告警 startsAt 回看；缺省 24，上限 168。"},
						"limit": {"type": "number", "minimum": 1, "maximum": 50, "description": "返回条数上限；缺省 10，上限 50。"}
					}
				}
			}
		]
	}
}`

const runLoopAlertsRecentResult = `{"alerts":[{"occurrenceId":"4","resource":"192.0.2.10:9121","severity":"critical","startedAt":"2026-09-21T16:33:48.339Z","state":"Resolved","title":"FixtureShopCacheUnavailable","viewKeys":["fixture-shop"]},{"occurrenceId":"2","resource":"192.0.2.10:9121","severity":"critical","startedAt":"2026-09-21T15:58:33.339Z","state":"Resolved","title":"FixtureShopMiddlewareTargetDown","viewKeys":["fixture-shop"]}],"total":2,"window":"2026-09-20T17:44:00Z/2026-09-21T17:44:00Z"}`

// runLoopDriftedFinalAnswer 的证据段正确引用工具返回的关联告警标题，最终诊断段
// 把同一标识符改写成未出现在任何输入中的近似拼写——与 occ3 analysis2 的失效
// 形状同构（前缀段替换，其余逐字保留）。
const runLoopDriftedFinalAnswer = "## 结论\n\n该告警已恢复。up==0 共 5 个采样点（15:58:00Z–16:02:00Z），与告警列表中的 #2 FixtureShopMiddlewareTargetDown（15:58:33Z，同一实例，已 Resolved）时间吻合。\n\n**最终诊断**：这是一条已恢复的 critical 告警，并伴随一段更早的抓取目标不可达（对应 FixienMiddlewareTargetDown 告警）。建议按只读方式核对端点配置。"

// driveRunLoopWithDriftedAnswer 以脚本化 supervisor 帧驱动真实 runLoop 走完
// 「首轮模型调用→alerts_recent 工具→最终响应→WorkerResultProposal」，返回
// 两轮 ChatModelRequest 的 messages_json 与最终 WorkerResultProposal。
func driveRunLoopWithDriftedAnswer(t *testing.T) (firstRequest, secondRequest string, proposal *workerv1.WorkerResultProposal) {
	t.Helper()
	supervisorReader, supervisorWriter := io.Pipe() // 测试 -> runLoop 的 reader
	workerReader, workerWriter := io.Pipe()         // runLoop 的 writer -> 测试
	defer func() {
		_ = supervisorWriter.Close()
		_ = workerWriter.Close()
	}()
	digest := sha256.Sum256([]byte(runLoopFixtureCanonical))
	start := &workerv1.StartAttempt{
		Mode:          workerv1.WorkMode_WORK_MODE_INITIAL_ANALYSIS,
		SchemaKind:    "initial_analysis_v1",
		CanonicalJson: []byte(runLoopFixtureCanonical),
		ContentDigest: digest[:],
		AgentVersion:  WorkerAgentVersion,
	}
	mode, err := verifyStart(start)
	if err != nil {
		t.Fatal(err)
	}
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- runLoop(context.Background(), Config{Stderr: io.Discard},
			NewFrameReader(supervisorReader), NewFrameWriter(workerWriter), 7, start, mode)
	}()
	supervisor := NewFrameWriter(supervisorWriter)
	worker := NewFrameReader(workerReader)

	// 第 1 轮：模型请求到达后回放 Started + 带 alerts_recent tool call 的 Completed。
	first := readModelRequest(t, worker)
	send := func(envelope *workerv1.WorkerEnvelope) {
		t.Helper()
		if err := supervisor.Send(envelope); err != nil {
			t.Fatal(err)
		}
	}
	send(&workerv1.WorkerEnvelope{AttemptId: 7, Msg: &workerv1.WorkerEnvelope_ChatModelStarted{
		ChatModelStarted: &workerv1.ChatModelStarted{ModelCallId: 101, CallSeq: 1},
	}})
	send(&workerv1.WorkerEnvelope{AttemptId: 7, Msg: &workerv1.WorkerEnvelope_ChatModelCompleted{
		ChatModelCompleted: &workerv1.ChatModelCompleted{
			ModelCallId: 101, AssistantText: "先查询近期告警。", ResponseDigest: []byte("digest-1"),
			ToolCalls: []*workerv1.PreparedToolCall{{
				ToolCallId: 9001, ProviderIndex: 0, ProviderToolCallId: "call_1", ToolName: "alerts_recent",
				ArgumentsJson: []byte(`{"hours":24,"limit":10}`), ArgumentsDigest: []byte("args-digest"),
				ExecutionMode: runtimev1.ToolExecutionMode_TOOL_EXECUTION_MODE_QUOIN_ROUTED,
				FailureMode:   runtimev1.ToolFailureMode_TOOL_FAILURE_MODE_RETURN_TO_MODEL,
			}},
		},
	}})
	// worker 请求执行工具：回放 ToolCallStarted + 封存 ToolResult（携带 Evidence id）。
	envelope, err := worker.Read()
	if err != nil {
		t.Fatal(err)
	}
	if execute := envelope.GetExecuteToolCall(); execute == nil || execute.GetToolCallId() != 9001 {
		t.Fatalf("expected ExecuteToolCall for tool call 9001, got %v", envelope.Msg)
	}
	send(&workerv1.WorkerEnvelope{AttemptId: 7, Msg: &workerv1.WorkerEnvelope_ToolCallStarted{
		ToolCallStarted: &workerv1.ToolCallStarted{
			ToolCallId: 9001, ExecutionMode: runtimev1.ToolExecutionMode_TOOL_EXECUTION_MODE_QUOIN_ROUTED,
		},
	}})
	send(&workerv1.WorkerEnvelope{AttemptId: 7, Msg: &workerv1.WorkerEnvelope_ToolResult{
		ToolResult: &workerv1.ToolResult{
			ToolCallId: 9001, ResultJson: []byte(runLoopAlertsRecentResult), EvidenceIds: []int64{238},
		},
	}})
	// 第 2 轮：最终响应（无 tool call）。
	second := readModelRequest(t, worker)
	send(&workerv1.WorkerEnvelope{AttemptId: 7, Msg: &workerv1.WorkerEnvelope_ChatModelStarted{
		ChatModelStarted: &workerv1.ChatModelStarted{ModelCallId: 102, CallSeq: 2},
	}})
	send(&workerv1.WorkerEnvelope{AttemptId: 7, Msg: &workerv1.WorkerEnvelope_ChatModelCompleted{
		ChatModelCompleted: &workerv1.ChatModelCompleted{
			ModelCallId: 102, AssistantText: runLoopDriftedFinalAnswer, ResponseDigest: []byte("digest-2"),
		},
	}})
	// 最终提案 + 确认 ack。
	envelope, err = worker.Read()
	if err != nil {
		t.Fatal(err)
	}
	proposed := envelope.GetWorkerResultProposal()
	if proposed == nil {
		t.Fatalf("expected WorkerResultProposal, got %v", envelope.Msg)
	}
	send(&workerv1.WorkerEnvelope{AttemptId: 7, Msg: &workerv1.WorkerEnvelope_WorkerResultAck{
		WorkerResultAck: &workerv1.WorkerResultAck{Accepted: true},
	}})
	select {
	case err := <-loopDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runLoop did not terminate after the result ack")
	}
	return first, second, proposed
}

// readModelRequest 读取并解包下一个 ChatModelRequest 的 messages_json。
func readModelRequest(t *testing.T, reader *FrameReader) string {
	t.Helper()
	envelope, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	request := envelope.GetChatModelRequest()
	if request == nil {
		t.Fatalf("expected ChatModelRequest, got %v", envelope.Msg)
	}
	return string(request.GetMessagesJson())
}

// TestRunLoopModelVisibleMessagesCarryIdentifierFidelityRule 断言 1+2：约束与
// 工具结果原字节都真实进入模型可见消息（messages_json 是 marshalMessages 的
// 原始输出）。
func TestRunLoopModelVisibleMessagesCarryIdentifierFidelityRule(t *testing.T) {
	first, second, _ := driveRunLoopWithDriftedAnswer(t)
	for _, rule := range []string{
		"告警名称、其他告警的标题、指标名、标签值、来源名",
		"先回查原文再落笔",
		"不得凭印象改写、缩略、拼接或另造近似的名称",
	} {
		if !strings.Contains(first, rule) {
			t.Fatalf("model-visible first request lost identifier-fidelity rule %q", rule)
		}
	}
	if !strings.Contains(first, "FixtureShopCacheUnavailable") {
		t.Fatal("model-visible first request lost the frozen occurrence context")
	}
	// 失效形态证据回归：写最终诊断时，工具返回的正确标识符原字节在模型可见
	// 上下文中，而改写拼写不存在——漂移只能来自模型自身复述，不是输入污染。
	if !strings.Contains(second, "FixtureShopMiddlewareTargetDown") {
		t.Fatalf("model-visible second request lost the exact tool-result identifier bytes:\n%s", second)
	}
	if strings.Contains(second, "FixienMiddlewareTargetDown") {
		t.Fatalf("mutated identifier must not exist anywhere in the model-visible context:\n%s", second)
	}
}

// TestRunLoopSealsFinalAnswerVerbatim 断言 3：含漂移标识符的最终响应被逐字
// 封存（无任何改写/纠正），digest 与 Evidence 引用按封存载荷闭合。
func TestRunLoopSealsFinalAnswerVerbatim(t *testing.T) {
	_, _, proposal := driveRunLoopWithDriftedAnswer(t)
	if proposal.GetSchemaKind() != OutputSchemaKind {
		t.Fatalf("expected schema kind %q, got %q", OutputSchemaKind, proposal.GetSchemaKind())
	}
	want, err := json.Marshal(runLoopDriftedFinalAnswer)
	if err != nil {
		t.Fatal(err)
	}
	if string(proposal.GetCanonicalJson()) != string(want) {
		t.Fatalf("final answer must be sealed verbatim (no tampering):\nwant %s\ngot  %s", want, proposal.GetCanonicalJson())
	}
	sum := sha256.Sum256(proposal.GetCanonicalJson())
	if hex.EncodeToString(sum[:]) != hex.EncodeToString(proposal.GetContentDigest()) {
		t.Fatal("proposal content digest must match its canonical bytes")
	}
	if len(proposal.GetEvidenceIds()) != 1 || proposal.GetEvidenceIds()[0] != 238 {
		t.Fatalf("proposal must echo the sealed evidence ids, got %v", proposal.GetEvidenceIds())
	}
}
