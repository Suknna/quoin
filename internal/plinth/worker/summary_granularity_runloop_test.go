package worker

// 总结粒度正向约束的 final 化链路回归（investigation 模式——失效形状观测到的
// 路径，会话 8）：用脚本化的真实模型响应帧（ChatModelStarted/ChatModelCompleted/
// ToolResult，即 supervisor 实际转发给 worker 的载荷形状）驱动真实 runLoop，断言
// 三件事：
//  1. 第一轮 ChatModelRequest 的 messages_json（模型实际可见消息）携带「总结
//     粒度」约束的系统提示词（补修前为红）；
//  2. 第二轮 ChatModelRequest 的 messages_json 携带 alerts_recent 工具结果的
//     per-occurrence startedAt 原字节——写总结时逐异常事实确实在其上下文中；
//  3. 正文独立、总结归并的模型响应（会话 8 失效形状：正文明确三条独立
//     occurrence，总结却用「一次…共 3 条」「唯一遗留」总括覆盖）被逐字封存进
//     WorkerResultProposal（不篡改正文——本仓不可变输出边界的守护断言），
//     Evidence 引用按封存结果透传。
// fixture 与 temporal_fidelity_runloop_test.go 共用一套通用值；不含真实业务名/
// 地址/事故时间。

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

// summaryMergedFinalAnswer 镜像会话 8 的失效形状（通用 fixture 值）：正文表格与
// 叙述正确且明确独立，总结段却把三条 occurrence 收回成「一次…共 3 条」并宣称
// 「唯一遗留的不确定」。
const summaryMergedFinalAnswer = "## 发生了什么（按 startsAt）\n\n| 时间 | 标题 |\n|---|---|\n| 09:11:07 | FixtureShopMiddlewareTargetDown |\n| 09:47:52 | FixtureShopCacheUnavailable |\n| 10:23:19 | FixtureShopCacheUnavailable |\n\n这是同一资源上的三条独立 occurrence，我没有证据把它们合并成一个事件。\n\n## 证据\n\nup 该目标当前为 1；min_over_time(up[24h])=0、changes(up[24h])=2。\n\n## 总结\n\n这 24 小时的核心异常是一次集中在该采集目标的抓取中断：09:11Z 起先报 FixtureShopMiddlewareTargetDown，09:47Z、10:23Z 又各报一次 FixtureShopCacheUnavailable，共 3 条 critical，现已全部 Resolved。唯一遗留的不确定是中断根因未确认。"

// driveInvestigationRunLoopWithMergedSummary 以脚本化 supervisor 帧驱动真实
// investigation runLoop 走完「首轮模型调用→alerts_recent 工具→最终响应→
// WorkerResultProposal」，返回两轮 ChatModelRequest 的 messages_json 与最终提案。
func driveInvestigationRunLoopWithMergedSummary(t *testing.T) (firstRequest, secondRequest string, proposal *workerv1.WorkerResultProposal) {
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
			NewFrameReader(supervisorReader), NewFrameWriter(workerWriter), 12, start, mode)
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
	send(&workerv1.WorkerEnvelope{AttemptId: 12, Msg: &workerv1.WorkerEnvelope_ChatModelStarted{
		ChatModelStarted: &workerv1.ChatModelStarted{ModelCallId: 301, CallSeq: 1},
	}})
	send(&workerv1.WorkerEnvelope{AttemptId: 12, Msg: &workerv1.WorkerEnvelope_ChatModelCompleted{
		ChatModelCompleted: &workerv1.ChatModelCompleted{
			ModelCallId: 301, AssistantText: "先查询近期告警。", ResponseDigest: []byte("digest-1"),
			ToolCalls: []*workerv1.PreparedToolCall{{
				ToolCallId: 9201, ProviderIndex: 0, ProviderToolCallId: "call_m", ToolName: "alerts_recent",
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
	if execute := envelope.GetExecuteToolCall(); execute == nil || execute.GetToolCallId() != 9201 {
		t.Fatalf("expected ExecuteToolCall for tool call 9201, got %v", envelope.Msg)
	}
	send(&workerv1.WorkerEnvelope{AttemptId: 12, Msg: &workerv1.WorkerEnvelope_ToolCallStarted{
		ToolCallStarted: &workerv1.ToolCallStarted{
			ToolCallId: 9201, ExecutionMode: runtimev1.ToolExecutionMode_TOOL_EXECUTION_MODE_QUOIN_ROUTED,
		},
	}})
	send(&workerv1.WorkerEnvelope{AttemptId: 12, Msg: &workerv1.WorkerEnvelope_ToolResult{
		ToolResult: &workerv1.ToolResult{
			ToolCallId: 9201, ResultJson: []byte(temporalAlertsRecentResult), EvidenceIds: []int64{260},
		},
	}})
	// 第 2 轮：最终响应（无 tool call）。
	second := readModelRequest(t, worker)
	send(&workerv1.WorkerEnvelope{AttemptId: 12, Msg: &workerv1.WorkerEnvelope_ChatModelStarted{
		ChatModelStarted: &workerv1.ChatModelStarted{ModelCallId: 302, CallSeq: 2},
	}})
	send(&workerv1.WorkerEnvelope{AttemptId: 12, Msg: &workerv1.WorkerEnvelope_ChatModelCompleted{
		ChatModelCompleted: &workerv1.ChatModelCompleted{
			ModelCallId: 302, AssistantText: summaryMergedFinalAnswer, ResponseDigest: []byte("digest-2"),
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
	send(&workerv1.WorkerEnvelope{AttemptId: 12, Msg: &workerv1.WorkerEnvelope_WorkerResultAck{
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

// TestRunLoopModelVisibleMessagesCarrySummaryGranularityRule 断言 1+2：约束与
// 工具结果 startedAt 原字节都真实进入模型可见消息（messages_json 是
// marshalMessages 的原始输出；工具结果内 JSON 转义为 \"startedAt\":\"…\"）。
func TestRunLoopModelVisibleMessagesCarrySummaryGranularityRule(t *testing.T) {
	first, second, _ := driveInvestigationRunLoopWithMergedSummary(t)
	for _, rule := range []string{
		"必须保留正文已区分的逐异常粒度",
		"只解释该指标自身",
		"不得用来代表其他告警",
		"明确写明未验证",
		"禁止用一次事件、同一问题、唯一遗留等总括表述覆盖正文已分别列出的独立项",
		"总结粒度不得粗于正文证据已支持的粒度",
	} {
		if !strings.Contains(first, rule) {
			t.Fatalf("model-visible first request lost summary-granularity rule %q", rule)
		}
	}
	// 写总结时，工具返回的 per-occurrence startedAt 原字节在模型可见上下文中
	// ——逐异常事实本来就在，归并只能来自模型收束段复述。
	for _, timestamp := range []string{
		"2026-09-21T09:11:07.123Z",
		"2026-09-21T09:47:52.123Z",
		"2026-09-21T10:23:19.123Z",
	} {
		if !strings.Contains(second, `\"startedAt\":\"`+timestamp+`\"`) {
			t.Fatalf("model-visible second request lost the exact occurrence time bytes %q:\n%s", timestamp, second)
		}
	}
}

// TestRunLoopSealsSummaryMergedAnswerVerbatim 断言 3：正文独立、总结归并的最终
// 响应被逐字封存（无任何改写/纠正——提示缓解是概率手段，输出侧不拦截），
// digest 与 Evidence 引用按封存载荷闭合。
func TestRunLoopSealsSummaryMergedAnswerVerbatim(t *testing.T) {
	_, _, proposal := driveInvestigationRunLoopWithMergedSummary(t)
	if proposal.GetSchemaKind() != InvestigationOutputSchemaKind {
		t.Fatalf("expected schema kind %q, got %q", InvestigationOutputSchemaKind, proposal.GetSchemaKind())
	}
	want, err := json.Marshal(summaryMergedFinalAnswer)
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
	if len(proposal.GetEvidenceIds()) != 1 || proposal.GetEvidenceIds()[0] != 260 {
		t.Fatalf("proposal must echo the sealed evidence ids, got %v", proposal.GetEvidenceIds())
	}
}
