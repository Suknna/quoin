package stele

// 转发循环（ADR-0011）：把本地 outbox 中到期的事件批量交给 Quoin 的
// DeliverEvents。逐事件按裁决落账：ACCEPTED/REJECTED 删除（REJECTED 另写
// 死信），UNAVAILABLE 指数退避、耗尽入死信。任何 gRPC 错误都视为整批
// UNAVAILABLE——批量调用的部分状态不可知，Quoin 侧按 event_id 幂等去重，
// 重复投递是安全的。

import (
	"context"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
)

const (
	forwardBatchSize   = 32
	forwardInterval    = 2 * time.Second
	forwardCallTimeout = 10 * time.Second
)

// EventDeliverer 是转发循环依赖的投递面（*Relay 实现；测试注入 stub）。
type EventDeliverer interface {
	DeliverEvents(ctx context.Context, events []*runtimev1.RelayEvent) (*runtimev1.DeliverEventsResponse, error)
}

// Forwarder drains the local outbox toward Quoin.
type Forwarder struct {
	queue     *Queue
	deliverer EventDeliverer
	metrics   *Metrics
	wake      chan struct{}
}

// NewForwarder wires the loop. Wake() 让入队方在 202 之后立即触发一次扫描，
// 平摊的 2s 周期只兜底。
func NewForwarder(queue *Queue, deliverer EventDeliverer, metrics *Metrics) *Forwarder {
	return &Forwarder{queue: queue, deliverer: deliverer, metrics: metrics, wake: make(chan struct{}, 1)}
}

// Wake requests an immediate forward pass (non-blocking; a pending wake
// collapses).
func (forwarder *Forwarder) Wake() {
	select {
	case forwarder.wake <- struct{}{}:
	default:
	}
}

// Run drives the loop until the context ends.
func (forwarder *Forwarder) Run(ctx context.Context) {
	forwarder.forwardPass(ctx)
	ticker := time.NewTicker(forwardInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			forwarder.forwardPass(ctx)
		case <-forwarder.wake:
			forwarder.forwardPass(ctx)
		}
	}
}

// forwardPass executes one drain cycle. Errors are logged and retried by the
// next tick; nothing here panics the process. 满批成功则继续排空，任何错误或
// 短批都结束本轮，由下一 tick/唤醒接续——落账失败的批次不会原地空转。
func (forwarder *Forwarder) forwardPass(ctx context.Context) {
	defer forwarder.projectDepth(ctx)
	for {
		if ctx.Err() != nil {
			return
		}
		events, err := forwarder.queue.FetchDueBatch(ctx, forwardBatchSize, time.Now().UTC())
		if err != nil {
			sharedops.LogEvent("stele", "error", "forward.fetch_failed", err.Error())
			return
		}
		if len(events) == 0 {
			return
		}
		relayEvents := make([]*runtimev1.RelayEvent, 0, len(events))
		for _, event := range events {
			relayEvents = append(relayEvents, event.RelayEvent())
		}
		callCtx, cancel := context.WithTimeout(ctx, forwardCallTimeout)
		response, err := forwarder.deliverer.DeliverEvents(callCtx, relayEvents)
		cancel()
		now := time.Now().UTC()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// 整批 UNAVAILABLE：请求可能未到达，逐事件退避。
			sharedops.LogEvent("stele", "error", "forward.batch_unavailable", err.Error())
			forwarder.markBatch(ctx, events, nil, now)
			return
		}
		if len(response.GetResults()) != len(events) {
			// 裁决数与请求数不一致：视为协议级不可用，逐事件退避。
			sharedops.LogEvent("stele", "error", "forward.results_mismatch",
				"expected results do not match the batch size")
			forwarder.markBatch(ctx, events, nil, now)
			return
		}
		allMarked := forwarder.markBatch(ctx, events, response.GetResults(), now)
		if !allMarked || len(events) < forwardBatchSize {
			return
		}
	}
}

// markBatch 落账一批结果；results 为 nil 表示整批 UNAVAILABLE。返回是否
// 全部落账成功（失败的事件仍处于到期态，调用方应结束本轮避免空转）。
func (forwarder *Forwarder) markBatch(ctx context.Context, events []QueuedEvent, results []runtimev1.EventDeliveryStatus, now time.Time) bool {
	allMarked := true
	for index, event := range events {
		result := runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_UNAVAILABLE
		if results != nil {
			result = results[index]
		}
		if result == runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_UNSPECIFIED {
			result = runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_UNAVAILABLE
		}
		dead, err := forwarder.queue.MarkResult(ctx, event.ID, result, now)
		if err != nil {
			allMarked = false
			sharedops.LogEvent("stele", "error", "forward.mark_failed", event.ID+": "+err.Error())
		}
		if dead {
			forwarder.metrics.RecordDeadLetter()
		}
		forwarder.metrics.RecordForwarded(int32(result))
	}
	return allMarked
}

func (forwarder *Forwarder) projectDepth(ctx context.Context) {
	depth, err := forwarder.queue.QueueDepth(ctx)
	if err != nil {
		return
	}
	forwarder.metrics.SetQueueDepth(depth)
}
