package stele

// 转发循环行为验证：ACCEPTED 删除、REJECTED 死信、UNAVAILABLE 退避、
// 整批 gRPC 错误退避、指纹随请求携带。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
)

// scriptDeliverer 按脚本逐次响应；记录每次请求用于断言。
type scriptDeliverer struct {
	responses []*runtimev1.DeliverEventsResponse
	errs      []error
	requests  []*runtimev1.DeliverEventsRequest
}

func (deliverer *scriptDeliverer) DeliverEvents(_ context.Context, events []*runtimev1.RelayEvent) (*runtimev1.DeliverEventsResponse, error) {
	index := len(deliverer.requests)
	request := &runtimev1.DeliverEventsRequest{ContractFingerprint: contract.ProtoAuthorityFingerprint, Events: events}
	deliverer.requests = append(deliverer.requests, request)
	if index >= len(deliverer.errs) || deliverer.errs[index] != nil {
		var err error
		if index < len(deliverer.errs) {
			err = deliverer.errs[index]
		}
		if err != nil {
			return nil, err
		}
	}
	if index < len(deliverer.responses) && deliverer.responses[index] != nil {
		return deliverer.responses[index], nil
	}
	return &runtimev1.DeliverEventsResponse{}, nil
}

func drainForwarder(t *testing.T, deliverer *scriptDeliverer, queue *Queue) *Forwarder {
	t.Helper()
	forwarder := NewForwarder(queue, deliverer, NewMetrics())
	forwarder.forwardPass(context.Background())
	return forwarder
}

func TestForwarderAcceptedDeletesAndRejectedDeadLetters(t *testing.T) {
	ctx := context.Background()
	queue := openTestQueue(t)
	if err := queue.EnqueueEvents(ctx, []QueuedEvent{
		sampleEvent("evt-accepted"), sampleEvent("evt-rejected"),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	deliverer := &scriptDeliverer{responses: []*runtimev1.DeliverEventsResponse{{
		Results: []runtimev1.EventDeliveryStatus{
			runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_ACCEPTED,
			runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_REJECTED,
		},
	}}}
	drainForwarder(t, deliverer, queue)

	if len(deliverer.requests) != 1 || len(deliverer.requests[0].GetEvents()) != 2 {
		t.Fatalf("deliverer saw %d requests, want 1 with 2 events", len(deliverer.requests))
	}
	if got := deliverer.requests[0].GetContractFingerprint(); got != contract.ProtoAuthorityFingerprint {
		t.Fatalf("request fingerprint = %q, want the authority fingerprint", got)
	}
	if depth, _ := queue.QueueDepth(ctx); depth != 0 {
		t.Fatalf("depth after pass = %d, want 0", depth)
	}
	reasons := deadLetterReasons(t, queue)
	if len(reasons) != 1 || reasons["evt-rejected"] != "rejected" {
		t.Fatalf("dead letters = %v, want only evt-rejected/rejected", reasons)
	}
}

func TestForwarderUnavailableSchedulesBackoff(t *testing.T) {
	ctx := context.Background()
	queue := openTestQueue(t)
	if err := queue.EnqueueEvents(ctx, []QueuedEvent{sampleEvent("evt-retry")}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	deliverer := &scriptDeliverer{responses: []*runtimev1.DeliverEventsResponse{{
		Results: []runtimev1.EventDeliveryStatus{
			runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_UNAVAILABLE,
		},
	}}}
	drainForwarder(t, deliverer, queue)

	// 退避期内不可见；推进时钟后可见且 attempts 已累计。
	if pending, err := queue.FetchDueBatch(ctx, 10, time.Now().UTC()); err != nil || len(pending) != 0 {
		t.Fatalf("pending during backoff = %d err=%v, want 0", len(pending), err)
	}
	pending, err := queue.FetchDueBatch(ctx, 10, time.Now().UTC().Add(2*time.Second))
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending after backoff = %d err=%v, want 1", len(pending), err)
	}
	if pending[0].Attempts != 1 || pending[0].ID != "evt-retry" {
		t.Fatalf("retried event = %+v, want evt-retry with attempts=1", pending[0])
	}
}

func TestForwarderGRPCErrorTreatsWholeBatchUnavailable(t *testing.T) {
	ctx := context.Background()
	queue := openTestQueue(t)
	if err := queue.EnqueueEvents(ctx, []QueuedEvent{
		sampleEvent("evt-a"), sampleEvent("evt-b"),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	deliverer := &scriptDeliverer{errs: []error{errors.New("rpc error: connection refused")}}
	drainForwarder(t, deliverer, queue)

	if len(deliverer.requests) != 1 {
		t.Fatalf("deliverer saw %d requests, want 1", len(deliverer.requests))
	}
	pending, err := queue.FetchDueBatch(ctx, 10, time.Now().UTC().Add(2*time.Second))
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending after whole-batch failure = %d err=%v, want 2", len(pending), err)
	}
	for _, event := range pending {
		if event.Attempts != 1 {
			t.Fatalf("event %s attempts = %d, want 1", event.ID, event.Attempts)
		}
	}
}

func TestForwarderResultCountMismatchTreatedUnavailable(t *testing.T) {
	ctx := context.Background()
	queue := openTestQueue(t)
	if err := queue.EnqueueEvents(ctx, []QueuedEvent{sampleEvent("evt-a"), sampleEvent("evt-b")}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	deliverer := &scriptDeliverer{responses: []*runtimev1.DeliverEventsResponse{{
		Results: []runtimev1.EventDeliveryStatus{
			runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_ACCEPTED,
		},
	}}}
	drainForwarder(t, deliverer, queue)

	// 一个结果对两个事件：不能只删除第一个——整批退避。
	pending, err := queue.FetchDueBatch(ctx, 10, time.Now().UTC().Add(2*time.Second))
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending after mismatch = %d err=%v, want 2", len(pending), err)
	}
}
