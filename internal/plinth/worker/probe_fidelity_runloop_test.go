package worker

// 点查配对约束的 final 化链路回归（investigation 模式——失效形状观测到的路径，
// 会话 9）：用脚本化的真实模型响应帧（ChatModelStarted/ChatModelCompleted/
// ToolResult，即 supervisor 实际转发给 worker 的载荷形状）驱动真实 runLoop，断言
// 三件事：
//  1. 第一轮 ChatModelRequest 的 messages_json（模型实际可见消息）携带「点查
//     配对」约束的系统提示词（补修前为红）；
//  2. 第二轮 ChatModelRequest 的 messages_json 携带 thanos_query 点查结果的
//     [时刻, 值] 配对原字节——落笔回读时正确的原始配对确实在其上下文中
//     （数据原值保留；失效形态的证据回归：错配只能来自模型复述，不是输入污染）；
//  3. 引用值与原始返回不符（0 被写成 1/已恢复）+「互不相关」无据断言的模型响应
//     被逐字封存进 WorkerResultProposal（不篡改正文——本仓不可变输出边界的守护
//     断言），Evidence 引用按封存结果透传。
// fixture 与 temporal_fidelity_runloop_test.go 共用一套通用值；不含真实业务名/
// 地址/事故时刻/事故点查值。

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

// probeQueryArguments / probeQueryResult：一次 up 点查（fixture epoch 1789123450
// = 2026-06-15T10:57:30.5Z，非事故时刻），原始返回值为 "0"。
const probeQueryArguments = `{"sourceRef":"fixture-shop-prometheus","query":"up{instance=\"192.0.2.10:9121\"} @ 1789123450"}`

const probeQueryResult = `{"success":true,"status":"success","resultType":"vector","sampleCount":1,"truncated":false,"output":"{\"status\":\"success\",\"data\":{\"resultType\":\"vector\",\"result\":[{\"metric\":{\"__name__\":\"up\",\"instance\":\"192.0.2.10:9121\",\"job\":\"fixture-cache-exporter\"},\"value\":[1789123450.5,\"0\"]}]}}"}`

// probeDriftedFinalAnswer 镜像会话 9 的失效形状（通用 fixture 值）：正文把原始
// 返回 0 的点查写成 1 并据此宣称「已恢复」，另有「互不相关」类无据断言无关。
const probeDriftedFinalAnswer = "## 证据\n\nup 探测：@10:57:30=1，目标在 10:57:30 已恢复；其余 4 条 fixture-check 记录互不相关。\n\n## 总结\n\n第一段中断在 10:57:30 前后已恢复，无遗留异常；其余记录互不相关。"

// driveInvestigationRunLoopWithProbeDrift 以脚本化 supervisor 帧驱动真实
// investigation runLoop 走完「首轮模型调用→thanos_query 点查工具→最终响应→
// WorkerResultProposal」，返回两轮 ChatModelRequest 的 messages_json 与最终提案。
func driveInvestigationRunLoopWithProbeDrift(t *testing.T) (firstRequest, secondRequest string, proposal *workerv1.WorkerResultProposal) {
	t.Helper()
	supervisorReader, supervisorWriter := io.Pipe() // 测试 -> runLoop 的 reader
	workerReader, workerWriter := io.Pipe()         // runLoop 的 writer -> 测试
	defer func() {
		_ = supervisorWriter.Close()
		_ = workerWriter.Close()
	}()
	digest := sha256.Sum256([]byte(temporalInvestigationCanonical))
	start := &workerv1.StartAttempt{
		Mode:          workerv1.WorkMode_WORK_MODE_INVESTIGATION,
		SchemaKind:    "investigation_v1",
		CanonicalJson: []byte(temporalInvestigationCanonical),
		ContentDigest: digest[:],
		AgentVersion:  WorkerInvestigationAgentVersion,
	}
	mode, err := verifyStart(start)
	if err != nil {
		t.Fatal(err)
	}
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- runLoop(context.Background(), Config{Stderr: io.Discard},
			NewFrameReader(supervisorReader), NewFrameWriter(workerWriter), 13, start, mode)
	}()
	supervisor := NewFrameWriter(supervisorWriter)
	worker := NewFrameReader(workerReader)

	// 第 1 轮：模型请求到达后回放 Started + 带 thanos_query 点查 tool call 的 Completed。
	first := readModelRequest(t, worker)
	send := func(envelope *workerv1.WorkerEnvelope) {
		t.Helper()
		if err := supervisor.Send(envelope); err != nil {
			t.Fatal(err)
		}
	}
	send(&workerv1.WorkerEnvelope{AttemptId: 13, Msg: &workerv1.WorkerEnvelope_ChatModelStarted{
		ChatModelStarted: &workerv1.ChatModelStarted{ModelCallId: 401, CallSeq: 1},
	}})
	send(&workerv1.WorkerEnvelope{AttemptId: 13, Msg: &workerv1.WorkerEnvelope_ChatModelCompleted{
		ChatModelCompleted: &workerv1.ChatModelCompleted{
			ModelCallId: 401, AssistantText: "先做一次点查。", ResponseDigest: []byte("digest-1"),
			ToolCalls: []*workerv1.PreparedToolCall{{
				ToolCallId: 9301, ProviderIndex: 0, ProviderToolCallId: "call_p", ToolName: "thanos_query",
				ArgumentsJson: []byte(probeQueryArguments), ArgumentsDigest: []byte("args-digest"),
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
	if execute := envelope.GetExecuteToolCall(); execute == nil || execute.GetToolCallId() != 9301 {
		t.Fatalf("expected ExecuteToolCall for tool call 9301, got %v", envelope.Msg)
	}
	send(&workerv1.WorkerEnvelope{AttemptId: 13, Msg: &workerv1.WorkerEnvelope_ToolCallStarted{
		ToolCallStarted: &workerv1.ToolCallStarted{
			ToolCallId: 9301, ExecutionMode: runtimev1.ToolExecutionMode_TOOL_EXECUTION_MODE_QUOIN_ROUTED,
		},
	}})
	send(&workerv1.WorkerEnvelope{AttemptId: 13, Msg: &workerv1.WorkerEnvelope_ToolResult{
		ToolResult: &workerv1.ToolResult{
			ToolCallId: 9301, ResultJson: []byte(probeQueryResult), EvidenceIds: []int64{281},
		},
	}})
	// 第 2 轮：最终响应（无 tool call）。
	second := readModelRequest(t, worker)
	send(&workerv1.WorkerEnvelope{AttemptId: 13, Msg: &workerv1.WorkerEnvelope_ChatModelStarted{
		ChatModelStarted: &workerv1.ChatModelStarted{ModelCallId: 402, CallSeq: 2},
	}})
	send(&workerv1.WorkerEnvelope{AttemptId: 13, Msg: &workerv1.WorkerEnvelope_ChatModelCompleted{
		ChatModelCompleted: &workerv1.ChatModelCompleted{
			ModelCallId: 402, AssistantText: probeDriftedFinalAnswer, ResponseDigest: []byte("digest-2"),
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
	send(&workerv1.WorkerEnvelope{AttemptId: 13, Msg: &workerv1.WorkerEnvelope_WorkerResultAck{
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

// TestRunLoopModelVisibleMessagesCarryProbeFidelityRule 断言 1+2：约束与点查
// [时刻, 值] 配对原字节都真实进入模型可见消息（messages_json 内 JSON 转义为
// \"value\":[…]）。
func TestRunLoopModelVisibleMessagesCarryProbeFidelityRule(t *testing.T) {
	first, second, _ := driveInvestigationRunLoopWithProbeDrift(t)
	for _, rule := range []string{
		"点查事实是不可拆分的配对",
		"逐点核对",
		"优先保留经回读核准的必要点",
		"不凭记忆铺列时刻或值",
		"未实际查询过的时刻不得补造",
		"不得改写原始值",
		"按该指标已确认的取值语义判断",
		"不能仅凭数值 0 或 1 断定",
		"边界未确认就如实写未知",
		"没有关联证据不等于确定无关",
		"不得据此说成同一种故障或同一问题",
	} {
		if !strings.Contains(first, rule) {
			t.Fatalf("model-visible first request lost probe-fidelity rule %q", rule)
		}
	}
	// 失效形态证据回归：写点查结论时，原始 [时刻, 值] 配对（0 值）原字节在模型
	// 可见上下文中——错配只能来自模型复述，不是输入污染。点查结果内嵌一层 JSON
	// 字符串，进入 messages_json 后为三重转义（\\\"value\\\":…）。
	if !strings.Contains(second, `\\\"value\\\":[1789123450.5,\\\"0\\\"]`) {
		t.Fatalf("model-visible second request lost the raw probe pair bytes:\n%s", second)
	}
}

// TestRunLoopSealsProbeDriftedAnswerVerbatim 断言 3：点查值错配 +「互不相关」
// 无据断言的最终响应被逐字封存（无任何改写/纠正——提示缓解是概率手段，输出侧
// 不拦截），digest 与 Evidence 引用按封存载荷闭合。
func TestRunLoopSealsProbeDriftedAnswerVerbatim(t *testing.T) {
	_, _, proposal := driveInvestigationRunLoopWithProbeDrift(t)
	if proposal.GetSchemaKind() != InvestigationOutputSchemaKind {
		t.Fatalf("expected schema kind %q, got %q", InvestigationOutputSchemaKind, proposal.GetSchemaKind())
	}
	want, err := json.Marshal(probeDriftedFinalAnswer)
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
	if len(proposal.GetEvidenceIds()) != 1 || proposal.GetEvidenceIds()[0] != 281 {
		t.Fatalf("proposal must echo the sealed evidence ids, got %v", proposal.GetEvidenceIds())
	}
}
