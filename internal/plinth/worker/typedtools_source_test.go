package worker

// QUOIN_ROUTED 工具的 supervisor 转发路径测试(ADR-0011):BeginToolCall
// 围栏后 Plinth 不再本地执行,而是等待 Quoin 推送的 ExternalToolResult 并
// 组装 worker 的 ToolResult;CompleteToolCall 不由 Plinth 发送(Quoin 已
// 自行封存),失败形态按 error_code/error_detail 组装。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	workerv1 "github.com/Suknna/quoin/internal/gen/proto/plinth/worker/v1"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
)

// fakeToolCallChannel exercises the BeginToolCall -> CompleteToolCall
// control request sequence used by a live Runner, retaining every request
// for the assertions below.
type fakeToolCallChannel struct {
	begins    []*runtimev1.BeginToolCall
	completes []*runtimev1.CompleteToolCall
}

func (channel *fakeToolCallChannel) Request(_ context.Context, envelope *runtimev1.ControlEnvelope) (*runtimev1.ControlEnvelope, error) {
	if begin := envelope.GetBeginToolCall(); begin != nil {
		channel.begins = append(channel.begins, begin)
		return &runtimev1.ControlEnvelope{Msg: &runtimev1.ControlEnvelope_BeginToolCallAck{
			BeginToolCallAck: &runtimev1.BeginToolCallAck{Accepted: true},
		}}, nil
	}
	if complete := envelope.GetCompleteToolCall(); complete != nil {
		channel.completes = append(channel.completes, complete)
		return &runtimev1.ControlEnvelope{Msg: &runtimev1.ControlEnvelope_CompleteToolCallAck{
			CompleteToolCallAck: &runtimev1.CompleteToolCallAck{Accepted: true, CommittedPayload: complete.GetPayload()},
		}}, nil
	}
	return nil, fmt.Errorf("unexpected control request %T", envelope.Msg)
}

// fakeExternalResultChannel 立即投放一个预设的 ExternalToolResult,
// 模拟 Quoin 在 BeginToolCallAck 之后推送封存结果。
type fakeExternalResultChannel struct {
	mu      sync.Mutex
	waiting []int64
	result  *runtimev1.ExternalToolResult
}

func (fake *fakeExternalResultChannel) AwaitExternalToolResult(ctx context.Context, toolCallID int64) (*runtimev1.ExternalToolResult, error) {
	fake.mu.Lock()
	fake.waiting = append(fake.waiting, toolCallID)
	result := fake.result
	fake.mu.Unlock()
	if result == nil {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return result, nil
}

func (fake *fakeExternalResultChannel) awaited() []int64 {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]int64(nil), fake.waiting...)
}

// readWorkerToolResult 解码 writer 写出的全部 worker 帧,返回其中的
// ToolResult(ToolCallStarted 之后到达)。
func readWorkerToolResult(t *testing.T, buffer *bytes.Buffer) *workerv1.ToolResult {
	t.Helper()
	reader := NewFrameReader(bytes.NewReader(buffer.Bytes()))
	var result *workerv1.ToolResult
	for {
		envelope, err := reader.Read()
		if err != nil {
			break
		}
		if toolResult := envelope.GetToolResult(); toolResult != nil {
			result = toolResult
		}
	}
	if result == nil {
		t.Fatalf("no ToolResult frame in %d bytes of worker output", buffer.Len())
	}
	return result
}

func TestQuoinRoutedToolForwardsSealedResult(t *testing.T) {
	control := &fakeToolCallChannel{}
	canonical := []byte(`{"success":true,"output":"ok"}`)
	external := &fakeExternalResultChannel{result: &runtimev1.ExternalToolResult{
		AttemptId: 41, ToolCallId: 73,
		Outcome:     runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_SUCCEEDED,
		Payload:     &runtimev1.ResultPayload{SchemaKind: "artifact_read_result_v1", CanonicalJson: canonical},
		ArtifactRef: &runtimev1.ArtifactRef{ArtifactId: 901, MediaType: "text/plain", SizeBytes: 12},
		EvidenceIds: []int64{501, 502},
	}}
	meta := toolMeta{name: "artifact_read", mode: "TOOL_EXECUTION_MODE_QUOIN_ROUTED"}
	runner := &Runner{Client: nil, toolCalls: control, externalResults: external, tools: map[int64]toolMeta{73: meta}}
	var buffer bytes.Buffer
	if err := runner.executeTool(context.Background(), NewFrameWriter(&buffer), 41, 73, meta); err != nil {
		t.Fatal(err)
	}
	if len(control.begins) != 1 || control.begins[0].GetToolCallId() != 73 {
		t.Fatalf("begins=%+v, want exactly one BeginToolCall for 73", control.begins)
	}
	// Quoin 已自行封存:Plinth 不得发送 CompleteToolCall。
	if len(control.completes) != 0 {
		t.Fatalf("completions=%d, want 0 (Quoin seals quoin_routed tools)", len(control.completes))
	}
	if waited := external.awaited(); len(waited) != 1 || waited[0] != 73 {
		t.Fatalf("external wait=%v, want [73]", waited)
	}
	result := readWorkerToolResult(t, &buffer)
	if result.GetToolCallId() != 73 {
		t.Fatalf("tool call id drifted: %d", result.GetToolCallId())
	}
	if !result.GetSuccess() || !bytes.Equal(result.GetResultJson(), canonical) {
		t.Fatalf("result must forward the sealed payload verbatim: success=%t json=%s", result.GetSuccess(), result.GetResultJson())
	}
	if ref := result.GetArtifactRef(); ref == nil || ref.GetArtifactId() != 901 {
		t.Fatalf("artifact ref must map through: %+v", result.GetArtifactRef())
	}
	if ids := result.GetEvidenceIds(); len(ids) != 2 || ids[0] != 501 || ids[1] != 502 {
		t.Fatalf("evidence ids must forward: %v", ids)
	}
}

func TestQuoinRoutedToolFailureShape(t *testing.T) {
	control := &fakeToolCallChannel{}
	external := &fakeExternalResultChannel{result: &runtimev1.ExternalToolResult{
		AttemptId: 41, ToolCallId: 74,
		Outcome:     runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_FAILED,
		ErrorCode:   "connection_unreachable",
		ErrorDetail: "Stele 网关无法建立连接",
	}}
	meta := toolMeta{name: "thanos_query", mode: "TOOL_EXECUTION_MODE_QUOIN_ROUTED"}
	runner := &Runner{toolCalls: control, externalResults: external, tools: map[int64]toolMeta{74: meta}}
	var buffer bytes.Buffer
	if err := runner.executeTool(context.Background(), NewFrameWriter(&buffer), 41, 74, meta); err != nil {
		t.Fatal(err)
	}
	result := readWorkerToolResult(t, &buffer)
	if result.GetSuccess() {
		t.Fatal("failed outcome must compose a failure ToolResult")
	}
	if result.GetErrorCode() != "connection_unreachable" || result.GetErrorDetail() != "Stele 网关无法建立连接" {
		t.Fatalf("error fields drifted: %s / %s", result.GetErrorCode(), result.GetErrorDetail())
	}
	var payload map[string]any
	if err := json.Unmarshal(result.GetResultJson(), &payload); err != nil {
		t.Fatalf("synthesized failure payload must be valid JSON: %v", err)
	}
	if payload["success"] != false || payload["errorCode"] != "connection_unreachable" {
		t.Fatalf("synthesized failure payload drifted: %s", result.GetResultJson())
	}
	if len(control.completes) != 0 {
		t.Fatalf("completions=%d, want 0 (Quoin seals quoin_routed tools)", len(control.completes))
	}
}

// attempt 取消时等待方必须让出:ctx 结束即返回错误,worker 不会被一个
// 永不到来的 ToolResult 卡死。
func TestQuoinRoutedToolAbandonsOnContextCancel(t *testing.T) {
	control := &fakeToolCallChannel{}
	external := &fakeExternalResultChannel{} // never delivers
	meta := toolMeta{name: "artifact_read", mode: "TOOL_EXECUTION_MODE_QUOIN_ROUTED"}
	runner := &Runner{toolCalls: control, externalResults: external, tools: map[int64]toolMeta{75: meta}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buffer bytes.Buffer
	if err := runner.executeTool(ctx, NewFrameWriter(&buffer), 41, 75, meta); err == nil {
		t.Fatal("cancelled context must abort the quoin_routed wait")
	}
	if waited := external.awaited(); len(waited) != 1 || waited[0] != 75 {
		t.Fatalf("external wait=%v, want [75]", waited)
	}
}

// worker_local 工具在 ToolCallStarted 后即返回:执行权在 worker 沙箱,
// supervisor 不等待任何外部结果。
func TestWorkerLocalToolReturnsAfterStart(t *testing.T) {
	control := &fakeToolCallChannel{}
	external := &fakeExternalResultChannel{}
	meta := toolMeta{name: "bash", mode: "TOOL_EXECUTION_MODE_WORKER_LOCAL"}
	runner := &Runner{toolCalls: control, externalResults: external, tools: map[int64]toolMeta{76: meta}}
	var buffer bytes.Buffer
	if err := runner.executeTool(context.Background(), NewFrameWriter(&buffer), 41, 76, meta); err != nil {
		t.Fatal(err)
	}
	if len(external.awaited()) != 0 {
		t.Fatal("worker_local tool must not wait for an external result")
	}
	// ToolCallStarted 帧写出了;无 ToolResult(worker 随后自执行)。
	if buffer.Len() == 0 {
		t.Fatal("ToolCallStarted must be written")
	}
}

// 词表外的执行模式必须 fail fast。
func TestUnsupportedExecutionModeFailsFast(t *testing.T) {
	control := &fakeToolCallChannel{}
	meta := toolMeta{name: "odd", mode: "TOOL_EXECUTION_MODE_SUPERVISOR_TYPED"}
	runner := &Runner{toolCalls: control, tools: map[int64]toolMeta{77: meta}}
	if err := runner.executeTool(context.Background(), NewFrameWriter(&bytes.Buffer{}), 41, 77, meta); err == nil {
		t.Fatal("retired execution mode must fail fast")
	}
}
