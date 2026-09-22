package worker

// 时间序与独立 occurrence 保真约束的 final 化链路回归（investigation 模式——
// 失效形状观测到的路径）：用
// 脚本化的真实模型响应帧（ChatModelStarted/ChatModelCompleted/ToolResult，即
// supervisor 实际转发给 worker 的载荷形状）驱动真实 runLoop，断言三件事：
//  1. 第一轮 ChatModelRequest 的 messages_json（模型实际可见消息）携带「时间序
//     与独立 occurrence 保真」约束的系统提示词（补修前为红）；
//  2. 第二轮 ChatModelRequest 的 messages_json 携带 alerts_recent 工具结果的
//     per-occurrence startedAt 原字节——模型写叙述/总结时正确的先后事实确实在其
//     上下文中（失效形态的证据回归：重排与合并只能来自模型复述，不是输入污染）；
//  3. 叙述段排列矛盾 + 总结段粗合并的模型响应被逐字封存进 WorkerResultProposal
//     （不篡改正文——本仓不可变输出边界的守护断言），Evidence 引用按封存结果
//     透传。
// fixture 与 agent 包 temporal_fidelity_prompt_test.go 共用一套通用值；不含真实
// 业务名/地址/事故时间。

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

// temporalInvestigationCanonical 镜像真实 investigation_v1 冻结输入：一条用户
// 消息 + 新在前的 recentOccurrences（最早一条与另两条标题不同）+ 内嵌冻结工具
// 目录（catalogFromInputDocument 从 canonical 提取）。
const temporalInvestigationCanonical = `{
	"messages": [{"role": "user", "content": "过去24小时有哪些告警，分别是什么时候开始的？"}],
	"sources": [],
	"recentOccurrences": [
		{"id": "9", "sourceKey": "fixture-am", "startsAt": "2026-09-21T10:23:19.123Z", "labels": {"alertname": "FixtureShopCacheUnavailable", "severity": "critical"}},
		{"id": "8", "sourceKey": "fixture-am", "startsAt": "2026-09-21T09:47:52.123Z", "labels": {"alertname": "FixtureShopCacheUnavailable", "severity": "critical"}},
		{"id": "7", "sourceKey": "fixture-am", "startsAt": "2026-09-21T09:11:07.123Z", "labels": {"alertname": "FixtureShopMiddlewareTargetDown", "severity": "critical"}}
	],
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

// temporalAlertsRecentResult 与 agent 包 fixture 相同：新在前的真实返回顺序，
// startedAt 真值表明最早的告警与另两条标题不同（不存在「后者夹着前者」的排列）。
const temporalAlertsRecentResult = `{"alerts":[{"occurrenceId":"9","resource":"192.0.2.10:9121","severity":"critical","startedAt":"2026-09-21T10:23:19.123Z","state":"Resolved","title":"FixtureShopCacheUnavailable","viewKeys":["fixture-shop"]},{"occurrenceId":"8","resource":"192.0.2.10:9121","severity":"critical","startedAt":"2026-09-21T09:47:52.123Z","state":"Resolved","title":"FixtureShopCacheUnavailable","viewKeys":["fixture-shop"]},{"occurrenceId":"7","resource":"192.0.2.10:9121","severity":"critical","startedAt":"2026-09-21T09:11:07.123Z","state":"Resolved","title":"FixtureShopMiddlewareTargetDown","viewKeys":["fixture-shop"]}],"total":3,"window":"2026-09-20T10:30:00Z/2026-09-21T10:30:00Z"}`

// temporalDriftedFinalAnswer 镜像会话 7 的两处失效形状（通用 fixture 值）：表格/
// 列表正确呈现 startedAt 的前提下，叙述段声称「2 条 X 夹着 1 条 Y」（与真值矛盾：
// Y 最先），总结段把三条 occurrence 收回成「只有一轮」。
const temporalDriftedFinalAnswer = "## 直接回答\n\n窗口内共 3 条 critical 告警 occurrence，全部 Resolved。\n\n## 发生了什么（按 startedAt）\n\n| 时间 | 标题 |\n|---|---|\n| 09:11:07 | FixtureShopMiddlewareTargetDown |\n| 09:47:52 | FixtureShopCacheUnavailable |\n| 10:23:19 | FixtureShopCacheUnavailable |\n\n## 不确定之处\n\n现象结构（2 条 FixtureShopCacheUnavailable 夹着 1 条 FixtureShopMiddlewareTargetDown）推测是先抓取不可达，再缓存不可用，这是与告警序列自洽的推断。\n\n## 总结\n\n过去 24 小时只有一轮 critical 波动（09:11–10:23），现已全部 Resolved。"

// driveInvestigationRunLoopWithTemporalDrift 以脚本化 supervisor 帧驱动真实
// investigation runLoop 走完「首轮模型调用→alerts_recent 工具→最终响应→
// WorkerResultProposal」，返回两轮 ChatModelRequest 的 messages_json 与最终提案。
func driveInvestigationRunLoopWithTemporalDrift(t *testing.T) (firstRequest, secondRequest string, proposal *workerv1.WorkerResultProposal) {
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
			NewFrameReader(supervisorReader), NewFrameWriter(workerWriter), 11, start, mode)
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
	send(&workerv1.WorkerEnvelope{AttemptId: 11, Msg: &workerv1.WorkerEnvelope_ChatModelStarted{
		ChatModelStarted: &workerv1.ChatModelStarted{ModelCallId: 201, CallSeq: 1},
	}})
	send(&workerv1.WorkerEnvelope{AttemptId: 11, Msg: &workerv1.WorkerEnvelope_ChatModelCompleted{
		ChatModelCompleted: &workerv1.ChatModelCompleted{
			ModelCallId: 201, AssistantText: "先查询近期告警。", ResponseDigest: []byte("digest-1"),
			ToolCalls: []*workerv1.PreparedToolCall{{
				ToolCallId: 9101, ProviderIndex: 0, ProviderToolCallId: "call_9", ToolName: "alerts_recent",
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
	if execute := envelope.GetExecuteToolCall(); execute == nil || execute.GetToolCallId() != 9101 {
		t.Fatalf("expected ExecuteToolCall for tool call 9101, got %v", envelope.Msg)
	}
	send(&workerv1.WorkerEnvelope{AttemptId: 11, Msg: &workerv1.WorkerEnvelope_ToolCallStarted{
		ToolCallStarted: &workerv1.ToolCallStarted{
			ToolCallId: 9101, ExecutionMode: runtimev1.ToolExecutionMode_TOOL_EXECUTION_MODE_QUOIN_ROUTED,
		},
	}})
	send(&workerv1.WorkerEnvelope{AttemptId: 11, Msg: &workerv1.WorkerEnvelope_ToolResult{
		ToolResult: &workerv1.ToolResult{
			ToolCallId: 9101, ResultJson: []byte(temporalAlertsRecentResult), EvidenceIds: []int64{241},
		},
	}})
	// 第 2 轮：最终响应（无 tool call）。
	second := readModelRequest(t, worker)
	send(&workerv1.WorkerEnvelope{AttemptId: 11, Msg: &workerv1.WorkerEnvelope_ChatModelStarted{
		ChatModelStarted: &workerv1.ChatModelStarted{ModelCallId: 202, CallSeq: 2},
	}})
	send(&workerv1.WorkerEnvelope{AttemptId: 11, Msg: &workerv1.WorkerEnvelope_ChatModelCompleted{
		ChatModelCompleted: &workerv1.ChatModelCompleted{
			ModelCallId: 202, AssistantText: temporalDriftedFinalAnswer, ResponseDigest: []byte("digest-2"),
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
	send(&workerv1.WorkerEnvelope{AttemptId: 11, Msg: &workerv1.WorkerEnvelope_WorkerResultAck{
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

// TestRunLoopModelVisibleMessagesCarryTemporalFidelityRule 断言 1+2：约束与
// 工具结果 startedAt 原字节都真实进入模型可见消息（messages_json 是
// marshalMessages 的原始输出）。
func TestRunLoopModelVisibleMessagesCarryTemporalFidelityRule(t *testing.T) {
	first, second, _ := driveInvestigationRunLoopWithTemporalDrift(t)
	for _, rule := range []string{
		"比较先后时必须分清所引字段并按其真实时间值排序",
		"不得描述数据中不存在的顺序或区间",
		"正文叙述与表格/列表的先后不得互相矛盾",
		"没有关联证据时不得合并成一个事件、一轮波动或单一根因",
		"同一资源的多条记录是多次独立 occurrence",
	} {
		if !strings.Contains(first, rule) {
			t.Fatalf("model-visible first request lost temporal-fidelity rule %q", rule)
		}
	}
	if !strings.Contains(first, "FixtureShopMiddlewareTargetDown") {
		t.Fatal("model-visible first request lost the frozen recent-occurrence history")
	}
	// 失效形态证据回归：写叙述/总结时，工具返回的 per-occurrence startedAt 原字节
	// 在模型可见上下文中（messages_json 内 JSON 转义为 \"startedAt\":\"…\"，字段名
	// 与时间值都来自工具结果原字节）——正确的先后事实本来就在，重排与合并只能
	// 来自模型复述。
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

// TestRunLoopSealsTemporalDriftedAnswerVerbatim 断言 3：排列矛盾 + 粗合并的最终
// 响应被逐字封存（无任何改写/纠正——提示缓解是概率手段，输出侧不拦截），
// digest 与 Evidence 引用按封存载荷闭合。
func TestRunLoopSealsTemporalDriftedAnswerVerbatim(t *testing.T) {
	_, _, proposal := driveInvestigationRunLoopWithTemporalDrift(t)
	if proposal.GetSchemaKind() != InvestigationOutputSchemaKind {
		t.Fatalf("expected schema kind %q, got %q", InvestigationOutputSchemaKind, proposal.GetSchemaKind())
	}
	want, err := json.Marshal(temporalDriftedFinalAnswer)
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
	if len(proposal.GetEvidenceIds()) != 1 || proposal.GetEvidenceIds()[0] != 241 {
		t.Fatalf("proposal must echo the sealed evidence ids, got %v", proposal.GetEvidenceIds())
	}
}
