package stele

// Stele 进程自有的指标集合（契约：contracts/metrics.yaml 的 stele_* 族）。
// 族形状与冻结目录一致；collectors 注册进本包持有的 registry，供测试断言
// 与后续接入 ops 侧导出面（:9090 的目录族由 internal/ops 按同一契约预置）。

import (
	"github.com/prometheus/client_golang/prometheus"
)

// deliveryStatusLabel 是 EventDeliveryStatus 的封闭小写取值（与目录
// label_set delivery_status 一致）。
func deliveryStatusLabel(status int32) string {
	switch status {
	case 1:
		return "accepted"
	case 2:
		return "rejected"
	default:
		return "unavailable"
	}
}

var platformCallStatusLabels = []string{
	"succeeded", "rate_limited", "credential_unavailable", "unreachable", "internal",
}

// Metrics 汇集 Stele 四条链路的观测：webhook 入站、本地队列、转发循环、
// 出向网关执行。nil 安全：未接线时调用方直接跳过观测。
type Metrics struct {
	registry *prometheus.Registry

	// deliveries 是 webhook 入站裁决（202 入队/4xx 拒绝/5xx 不可用），
	// 沿用 stele_deliveries_total 族名，语义为队列入站计数（ADR-0011）。
	deliveries *prometheus.CounterVec
	// queueDepth / deadLetters 投影本地队列与死信表。
	queueDepth  prometheus.Gauge
	deadLetters prometheus.Counter
	// forwarded 按逐事件 DeliveryStatus 统计 DeliverEvents 结果。
	forwarded *prometheus.CounterVec
	// gatewayCalls / gatewayLatency 按网关判定统计出向平台执行。
	gatewayCalls   *prometheus.CounterVec
	gatewayLatency prometheus.Histogram
}

// NewMetrics 构造并预置全部封闭标签组合（closed labels 契约：未发生的
// 序列也从启动起可见）。
func NewMetrics() *Metrics {
	registry := prometheus.NewRegistry()
	metrics := &Metrics{registry: registry}
	metrics.deliveries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stele_deliveries_total",
		Help: "Stele webhook intake outcomes grouped by the authoritative relay DeliveryStatus.",
	}, []string{"delivery_status"})
	for _, status := range []string{"accepted", "rejected", "unavailable"} {
		metrics.deliveries.WithLabelValues(status).Add(0)
	}
	metrics.queueDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "stele_queue_depth",
		Help: "Current number of pending events in Stele's local outbound queue.",
	})
	metrics.deadLetters = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "stele_queue_dead_letters_total",
		Help: "Events moved into Stele's local dead-letter table.",
	})
	metrics.forwarded = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stele_events_forwarded_total",
		Help: "Stele queue events handed to Quoin DeliverEvents grouped by the authoritative relay DeliveryStatus.",
	}, []string{"delivery_status"})
	for _, status := range []string{"accepted", "rejected", "unavailable"} {
		metrics.forwarded.WithLabelValues(status).Add(0)
	}
	metrics.gatewayCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "stele_gateway_calls_total",
		Help: "Stele outbound gateway platform calls grouped by gateway-judged PlatformCallStatus.",
	}, []string{"platform_call_status"})
	for _, status := range platformCallStatusLabels {
		metrics.gatewayCalls.WithLabelValues(status).Add(0)
	}
	metrics.gatewayLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "stele_gateway_latency_ms",
		Help:    "Stele outbound gateway platform call latency in milliseconds.",
		Buckets: []float64{1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000},
	})
	registry.MustRegister(metrics.deliveries, metrics.queueDepth, metrics.deadLetters,
		metrics.forwarded, metrics.gatewayCalls, metrics.gatewayLatency)
	return metrics
}

// Registry 暴露本包指标面（测试断言与后续导出接线用）。
func (metrics *Metrics) Registry() *prometheus.Registry {
	return metrics.registry
}

// RecordIntake 记录一次 webhook 入站裁决。
func (metrics *Metrics) RecordIntake(status string) {
	if metrics == nil {
		return
	}
	metrics.deliveries.WithLabelValues(status).Inc()
}

// SetQueueDepth 投影当前队列深度。
func (metrics *Metrics) SetQueueDepth(depth int) {
	if metrics == nil {
		return
	}
	metrics.queueDepth.Set(float64(depth))
}

// RecordDeadLetter 记录一次死信迁移。
func (metrics *Metrics) RecordDeadLetter() {
	if metrics == nil {
		return
	}
	metrics.deadLetters.Inc()
}

// RecordForwarded 记录一次逐事件转发结果。
func (metrics *Metrics) RecordForwarded(status int32) {
	if metrics == nil {
		return
	}
	metrics.forwarded.WithLabelValues(deliveryStatusLabel(status)).Inc()
}

// platformCallStatusLabel 是 PlatformCallStatus 的封闭小写取值（与目录
// label_set platform_call_status 一致）。
func platformCallStatusLabel(status int32) string {
	switch status {
	case 1:
		return "succeeded"
	case 2:
		return "rate_limited"
	case 3:
		return "credential_unavailable"
	case 4:
		return "unreachable"
	default:
		return "internal"
	}
}

// RecordGatewayCall 记录一次出向平台执行的网关判定与耗时。
func (metrics *Metrics) RecordGatewayCall(status int32, latencyMs float64) {
	if metrics == nil {
		return
	}
	metrics.gatewayCalls.WithLabelValues(platformCallStatusLabel(status)).Inc()
	metrics.gatewayLatency.Observe(latencyMs)
}
