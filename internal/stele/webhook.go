package stele

// 入向 webhook（ADR-0011）：POST /webhook/{source} → 按 source 解析
// EventSource 插件 → 网关侧 Bearer 认证（快照 digest 常量时间比对）→
// VerifyAndParse 归一化 → 单事务写入本地 outbox → 202。解析失败在入队前
// 拒绝（400）。可靠性由队列重试+死信承担，这里不再同步等 Quoin 落库。

import (
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/plugins"
)

// CredentialLookup 是 webhook 依赖的快照认证面（Relay 实现；测试注入 stub）。
type CredentialLookup interface {
	Ready() bool
	Credential(bearer, sourceKind string) (sourceID, credentialID int64, snapshotVersion uint64, ok bool)
}

// SourceRegistry 是 webhook 依赖的插件解析面（*plugins.Registry 实现）。
type SourceRegistry interface {
	EventSource(kind string) (plugins.EventSource, string, bool)
}

// Webhook is the inbound HTTP surface: one handler per source kind under
// /webhook/, wired with the local queue, the snapshot lookup, the plugin
// registry, and the process metrics.
type Webhook struct {
	queue   *Queue
	lookup  CredentialLookup
	sources SourceRegistry
	metrics *Metrics
}

// NewWebhook builds the handler. It never blocks on Quoin: enqueue is the
// acknowledgement point.
func NewWebhook(queue *Queue, lookup CredentialLookup, sources SourceRegistry, metrics *Metrics) *Webhook {
	return &Webhook{queue: queue, lookup: lookup, sources: sources, metrics: metrics}
}

// Handler returns the routed HTTP handler: POST /webhook/{source} plus a 404
// fallback. The deployment gateway strips the /stele prefix before dialing.
func (webhook *Webhook) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/", webhook.serveSource)
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "not found", http.StatusNotFound)
	})
	return mux
}

// serveSource dispatches one /webhook/{source} request.
func (webhook *Webhook) serveSource(writer http.ResponseWriter, request *http.Request) {
	source := strings.TrimPrefix(request.URL.Path, "/webhook/")
	if source == "" || strings.Contains(source, "/") {
		http.Error(writer, "not found", http.StatusNotFound)
		return
	}
	if request.Method != http.MethodPost {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	eventSource, _, ok := webhook.sources.EventSource(source)
	if !ok {
		// 未知 source 在认证前就 404：不向探测者泄露已装配的协议面。
		http.Error(writer, "unknown source", http.StatusNotFound)
		return
	}
	if !webhook.lookup.Ready() {
		http.Error(writer, "credential snapshot not loaded", http.StatusServiceUnavailable)
		webhook.metrics.RecordIntake("unavailable")
		return
	}
	sourceID, credentialID, snapshotVersion, authorized := webhook.authenticate(request, source)
	if !authorized {
		http.Error(writer, "invalid bearer credential", http.StatusUnauthorized)
		webhook.metrics.RecordIntake("rejected")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxWebhookBody+1))
	if err != nil {
		http.Error(writer, "read body failed", http.StatusBadRequest)
		webhook.metrics.RecordIntake("rejected")
		return
	}
	if len(body) > maxWebhookBody {
		http.Error(writer, "body too large", http.StatusRequestEntityTooLarge)
		webhook.metrics.RecordIntake("rejected")
		return
	}
	receivedAt := time.Now().UTC()
	events, err := eventSource.VerifyAndParse(request.Context(), plugins.InboundRequest{
		Header: request.Header, Body: body, ReceivedAt: receivedAt,
	})
	if err != nil {
		// 入队前拒绝：坏负载不进入可靠性管道。
		http.Error(writer, "payload rejected: "+err.Error(), http.StatusBadRequest)
		webhook.metrics.RecordIntake("rejected")
		return
	}
	queued := make([]QueuedEvent, 0, len(events))
	for _, event := range events {
		eventID, err := randomEventID()
		if err != nil {
			http.Error(writer, "event id unavailable", http.StatusServiceUnavailable)
			webhook.metrics.RecordIntake("unavailable")
			return
		}
		queued = append(queued, QueuedEvent{
			ID:                        eventID,
			SourceKind:                source,
			SourceID:                  sourceID,
			CredentialID:              credentialID,
			CredentialSnapshotVersion: snapshotVersion,
			EventType:                 event.Type,
			ReceivedAt:                receivedAt,
			Payload:                   event.Payload,
		})
	}
	if err := webhook.queue.EnqueueEvents(request.Context(), queued); err != nil {
		http.Error(writer, "enqueue failed", http.StatusInternalServerError)
		webhook.metrics.RecordIntake("unavailable")
		return
	}
	// 202 即入站裁决通过。计数单位是“每次请求一次”，与 4xx 拒绝/5xx 不可用
	// 路径一致：上游重复投递（同批重试）每次 202 各计一次 accepted，多事件
	// 载荷也只计一次（逐事件计数属于 stele_events_forwarded_total）。
	webhook.metrics.RecordIntake("accepted")
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(http.StatusAccepted)
	_, _ = writer.Write([]byte("queued\n"))
}

// authenticate parses the bearer and resolves it on this source's protocol;
// both failure modes (missing header, no digest hit) answer 401 without
// distinguishing them.
func (webhook *Webhook) authenticate(request *http.Request, source string) (int64, int64, uint64, bool) {
	authHeader := request.Header.Get("Authorization")
	if len(authHeader) < 8 || authHeader[:7] != "Bearer " {
		return 0, 0, 0, false
	}
	sourceID, credentialID, snapshotVersion, ok := webhook.lookup.Credential(authHeader[7:], source)
	if !ok {
		return 0, 0, 0, false
	}
	return sourceID, credentialID, snapshotVersion, true
}

// randomEventID 生成 16 字节随机数的 base64url：DeliverEvents 的幂等键。
func randomEventID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
