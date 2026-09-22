package worker

// 指标查询时间分层约束的 final 化链路回归（investigation 模式——失效形状观测到
// 的路径，会话 10 精度残余）：用脚本化的真实模型响应帧驱动真实 runLoop，断言
// 三件事：
//  1. 第一轮 ChatModelRequest 的 messages_json（模型实际可见消息）携带「指标
//     查询时间分层」约束的系统提示词（补修前为红）；
//  2. 第二轮 ChatModelRequest 的 messages_json 携带 timestamp 边界查询结果的
//     [评估时刻, 步进时刻] 配对原字节——区分步进与采样时刻的判断材料在模型
//     上下文中（数据原值保留）；
//  3. 把步进时刻写成「精确采样时刻/精确恢复」的模型响应被逐字封存进
//     WorkerResultProposal（不篡改正文——本仓不可变输出边界的守护断言），
//     Evidence 引用按封存结果透传。
// fixture 与 probe_fidelity_runloop_test.go 共用一套通用值；不含真实业务名/
// 地址/事故时刻。

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

// stepBoundaryArguments / stepBoundaryResult：一次 timestamp 边界查询（fixture
// epoch，非事故时刻），value 为 [评估时刻, "步进时刻"]。
const stepBoundaryArguments = `{"sourceRef":"fixture-shop-prometheus","query":"max_over_time(timestamp(up{instance=\"192.0.2.10:9121\"} == 0)[24h:15s])"}`

const stepBoundaryResult = `{"success":true,"status":"success","resultType":"vector","sampleCount":1,"truncated":false,"output":"{\"status\":\"success\",\"data\":{\"resultType\":\"vector\",\"result\":[{\"metric\":{\"__name__\":\"up\",\"instance\":\"192.0.2.10:9121\",\"job\":\"fixture-cache-exporter\"},\"value\":[1789126900.5,\"1789123500\"]}]}}"}`

// stepTimeAsSampleFinalAnswer 镜像会话 10 的精度残余形状（通用 fixture 值）：
// 把步进时刻上界写成「精确采样时刻」，并把近似区间端点写成「精确恢复」。
const stepTimeAsSampleFinalAnswer = "## 证据\n\n边界查询返回 1789123500，即最晚一次 down 采样的精确采样时刻。\n\n## 总结\n\n该段中断恰在步进时刻 1789123500 精确恢复，无遗留不确定。"

// driveInvestigationRunLoopWithStepAsSample 以脚本化 supervisor 帧驱动真实
// investigation runLoop 走完「首轮模型调用→thanos_query 边界查询→最终响应→
// WorkerResultProposal」，返回两轮 ChatModelRequest 的 messages_json 与最终提案。
func driveInvestigationRunLoopWithStepAsSample(t *testing.T) (firstRequest, secondRequest string, proposal *workerv1.WorkerResultProposal) {
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
			NewFrameReader(supervisorReader), NewFrameWriter(workerWriter), 14, start, mode)
	}()
	supervisor := NewFrameWriter(supervisorWriter)
	worker := NewFrameReader(workerReader)

	// 第 1 轮：模型请求到达后回放 Started + 带 thanos_query 边界查询的 Completed。
	first := readModelRequest(t, worker)
	send := func(envelope *workerv1.WorkerEnvelope) {
		t.Helper()
		if err := supervisor.Send(envelope); err != nil {
			t.Fatal(err)
		}
	}
	send(&workerv1.WorkerEnvelope{AttemptId: 14, Msg: &workerv1.WorkerEnvelope_ChatModelStarted{
		ChatModelStarted: &workerv1.ChatModelStarted{ModelCallId: 501, CallSeq: 1},
	}})
	send(&workerv1.WorkerEnvelope{AttemptId: 14, Msg: &workerv1.WorkerEnvelope_ChatModelCompleted{
		ChatModelCompleted: &workerv1.ChatModelCompleted{
			ModelCallId: 501, AssistantText: "先查边界。", ResponseDigest: []byte("digest-1"),
			ToolCalls: []*workerv1.PreparedToolCall{{
				ToolCallId: 9401, ProviderIndex: 0, ProviderToolCallId: "call_s", ToolName: "thanos_query",
				ArgumentsJson: []byte(stepBoundaryArguments), ArgumentsDigest: []byte("args-digest"),
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
	if execute := envelope.GetExecuteToolCall(); execute == nil || execute.GetToolCallId() != 9401 {
		t.Fatalf("expected ExecuteToolCall for tool call 9401, got %v", envelope.Msg)
	}
	send(&workerv1.WorkerEnvelope{AttemptId: 14, Msg: &workerv1.WorkerEnvelope_ToolCallStarted{
		ToolCallStarted: &workerv1.ToolCallStarted{
			ToolCallId: 9401, ExecutionMode: runtimev1.ToolExecutionMode_TOOL_EXECUTION_MODE_QUOIN_ROUTED,
		},
	}})
	send(&workerv1.WorkerEnvelope{AttemptId: 14, Msg: &workerv1.WorkerEnvelope_ToolResult{
		ToolResult: &workerv1.ToolResult{
			ToolCallId: 9401, ResultJson: []byte(stepBoundaryResult), EvidenceIds: []int64{301},
		},
	}})
	// 第 2 轮：最终响应（无 tool call）。
	second := readModelRequest(t, worker)
	send(&workerv1.WorkerEnvelope{AttemptId: 14, Msg: &workerv1.WorkerEnvelope_ChatModelStarted{
		ChatModelStarted: &workerv1.ChatModelStarted{ModelCallId: 502, CallSeq: 2},
	}})
	send(&workerv1.WorkerEnvelope{AttemptId: 14, Msg: &workerv1.WorkerEnvelope_ChatModelCompleted{
		ChatModelCompleted: &workerv1.ChatModelCompleted{
			ModelCallId: 502, AssistantText: stepTimeAsSampleFinalAnswer, ResponseDigest: []byte("digest-2"),
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
	send(&workerv1.WorkerEnvelope{AttemptId: 14, Msg: &workerv1.WorkerEnvelope_WorkerResultAck{
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

// TestRunLoopModelVisibleMessagesCarryQueryTimeSemanticsRule 断言 1+2：约束与
// 边界查询 [评估时刻, 步进时刻] 配对原字节都真实进入模型可见消息（内嵌 JSON
// 字符串进入 messages_json 后为三重转义）。
func TestRunLoopModelVisibleMessagesCarryQueryTimeSemanticsRule(t *testing.T) {
	first, second, _ := driveInvestigationRunLoopWithStepAsSample(t)
	for _, rule := range []string{
		"查询评估时刻、子查询步进时刻与原始采样时刻",
		"只有来源明确给出原始样本 timestamp 的才能称为实际采样时刻",
		"评估或步进时刻及其回看窗口不得写成采样时刻",
		"offset 探测的锚点按评估时刻减 offset 换算理解",
		"自造的人工时刻标签不构成时间证据",
		"近似区间只可近似表述，不得写成精确的恢复或复现时刻",
	} {
		if !strings.Contains(first, rule) {
			t.Fatalf("model-visible first request lost query-time-semantics rule %q", rule)
		}
	}
	// 失效形态证据回归：步进时刻配对原字节在模型可见上下文中——把它写成采样
	// 时刻只能来自模型复述，不是输入污染。
	if !strings.Contains(second, `\\\"value\\\":[1789126900.5,\\\"1789123500\\\"]`) {
		t.Fatalf("model-visible second request lost the raw boundary-pair bytes:\n%s", second)
	}
}

// TestRunLoopSealsStepTimeAsSampleAnswerVerbatim 断言 3：步进时刻写成精确
// 采样/精确恢复的最终响应被逐字封存（无任何改写/纠正——提示缓解是概率手段，
// 输出侧不拦截），digest 与 Evidence 引用按封存载荷闭合。
func TestRunLoopSealsStepTimeAsSampleAnswerVerbatim(t *testing.T) {
	_, _, proposal := driveInvestigationRunLoopWithStepAsSample(t)
	if proposal.GetSchemaKind() != InvestigationOutputSchemaKind {
		t.Fatalf("expected schema kind %q, got %q", InvestigationOutputSchemaKind, proposal.GetSchemaKind())
	}
	want, err := json.Marshal(stepTimeAsSampleFinalAnswer)
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
	if len(proposal.GetEvidenceIds()) != 1 || proposal.GetEvidenceIds()[0] != 301 {
		t.Fatalf("proposal must echo the sealed evidence ids, got %v", proposal.GetEvidenceIds())
	}
}
