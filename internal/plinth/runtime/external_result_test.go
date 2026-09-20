package runtime

// QUOIN_ROUTED 工具结果分发测试(ADR-0011):ExternalToolResult 帧按
// tool_call_id 路由到等待中的 waiter;ctx 结束清理 waiter;没有 waiter 的
// 结果只审计丢弃(Quoin 已自行封存,Plinth 不是裁决方)。

import (
	"context"
	"testing"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
)

func TestExternalToolResultRoutesToWaiter(t *testing.T) {
	channel := &Channel{}
	delivered := make(chan *runtimev1.ExternalToolResult, 1)
	go func() {
		result, err := channel.AwaitExternalToolResult(context.Background(), 73)
		if err != nil {
			t.Errorf("await: %v", err)
			return
		}
		delivered <- result
	}()
	// 等待 waiter 注册完成再投递,避免与注册竞争。
	deadline := time.Now().Add(time.Second)
	for {
		channel.externalMu.Lock()
		registered := len(channel.externalWaiters) > 0
		channel.externalMu.Unlock()
		if registered || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	channel.deliverExternalToolResult(&runtimev1.ExternalToolResult{
		AttemptId: 41, ToolCallId: 73,
		Outcome: runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_SUCCEEDED,
		Payload: &runtimev1.ResultPayload{SchemaKind: "artifact_read_result_v1", CanonicalJson: []byte(`{"success":true}`)},
	})
	select {
	case result := <-delivered:
		if result.GetToolCallId() != 73 || result.GetOutcome() != runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_SUCCEEDED {
			t.Fatalf("delivered result drifted: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("ExternalToolResult was not routed to the waiter")
	}
}

func TestExternalToolResultAbandonedOnContextCancel(t *testing.T) {
	channel := &Channel{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := channel.AwaitExternalToolResult(ctx, 74)
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		channel.externalMu.Lock()
		registered := len(channel.externalWaiters) > 0
		channel.externalMu.Unlock()
		if registered || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled wait must return an error")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled wait did not return")
	}
	channel.externalMu.Lock()
	remaining := len(channel.externalWaiters)
	channel.externalMu.Unlock()
	if remaining != 0 {
		t.Fatalf("waiter must be cleaned on cancel, %d remain", remaining)
	}
	// 取消后到达的结果只审计丢弃,不 panic、不阻塞。
	channel.deliverExternalToolResult(&runtimev1.ExternalToolResult{ToolCallId: 74, Outcome: runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_CANCELLED})
}

// 迟到/重复的结果没有 waiter:丢弃即可(Quoin 幂等封存,Plinth 侧无消费者)。
func TestExternalToolResultWithoutWaiterDropped(t *testing.T) {
	channel := &Channel{}
	channel.deliverExternalToolResult(&runtimev1.ExternalToolResult{ToolCallId: 404, Outcome: runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_SUCCEEDED})
}

// dispatchServerFrame 的 ExternalToolResult 分支走同一路由。
func TestDispatchServerFrameExternalToolResult(t *testing.T) {
	channel := &Channel{}
	result := &runtimev1.ExternalToolResult{AttemptId: 9, ToolCallId: 75, Outcome: runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_FAILED, ErrorCode: "x"}
	delivered := make(chan *runtimev1.ExternalToolResult, 1)
	go func() {
		got, err := channel.AwaitExternalToolResult(context.Background(), 75)
		if err != nil {
			t.Errorf("await: %v", err)
			return
		}
		delivered <- got
	}()
	deadline := time.Now().Add(time.Second)
	for {
		channel.externalMu.Lock()
		registered := len(channel.externalWaiters) > 0
		channel.externalMu.Unlock()
		if registered || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	channel.dispatchServerFrame(context.Background(), &FrameSink{channel: channel}, nil, &runtimev1.ControlEnvelope{
		Msg: &runtimev1.ControlEnvelope_ExternalToolResult{ExternalToolResult: result},
	})
	select {
	case got := <-delivered:
		if got.GetErrorCode() != "x" {
			t.Fatalf("dispatched result drifted: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("dispatchServerFrame did not route ExternalToolResult")
	}
}
