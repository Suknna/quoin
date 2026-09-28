package stele

// 入向 webhook 行为验证：未知 source 404、快照未加载 503、无 bearer/错
// 凭据 401、解析失败 400（入队前拒绝）、成功 202 且 outbox 落行。EventSource
// 用内存 stub——内置插件装配的冻结校验属于 internal/plugins 的职责，这里
// 只验证网关自身的路由、认证与入队语义。

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// stubLookup 复刻 Relay 的快照认证语义：bearer 是 32 字节随机数的
// base64url 文本，digest 是原始字节的 SHA-256；且 sourceKind 必须等于
// 凭据所属来源的 protocol。
type stubLookup struct {
	mu       sync.RWMutex
	ready    bool
	digest   []byte
	settings []byte // 命中后随 CredentialMatch 交给 InboundRequest 的来源设置
}

func (lookup *stubLookup) Ready() bool {
	lookup.mu.RLock()
	defer lookup.mu.RUnlock()
	return lookup.ready
}

func (lookup *stubLookup) Credential(bearer, sourceKind string) (CredentialMatch, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(bearer)
	if err != nil || len(raw) != 32 {
		return CredentialMatch{}, false
	}
	digest := sha256.Sum256(raw)
	if sourceKind != "alertmanager" || !subtleCompare(lookup.digest, digest[:]) {
		return CredentialMatch{}, false
	}
	return CredentialMatch{SourceID: 7, CredentialID: 9, SnapshotVersion: 3, Settings: lookup.settings}, true
}

// stubEventSource 是最小 EventSource：成功时把请求体原样归一化为一个
// alerts.batch 事件；fail 时模拟协议解析失败。settingsSeen 记录最近一次
// VerifyAndParse 收到的 Settings（ADR-0014 story 2 投递验证用）。
type stubEventSource struct {
	fail         bool
	eventType    string
	settingsSeen []byte
}

func (source *stubEventSource) Kind() string { return "alertmanager" }

func (source *stubEventSource) VerifyAndParse(_ context.Context, req plugins.InboundRequest) ([]plugins.Event, error) {
	if source.fail {
		return nil, errors.New("stub payload rejected")
	}
	source.settingsSeen = req.Settings
	var probe map[string]any
	if err := json.Unmarshal(req.Body, &probe); err != nil {
		return nil, err
	}
	eventType := source.eventType
	if eventType == "" {
		eventType = "alerts.batch"
	}
	return []plugins.Event{{Type: eventType, Payload: req.Body}}, nil
}

// stubKindSource 是 Kind 可配置的最小 EventSource。
type stubKindSource string

func (kind stubKindSource) Kind() string { return string(kind) }

func (kind stubKindSource) VerifyAndParse(_ context.Context, req plugins.InboundRequest) ([]plugins.Event, error) {
	return []plugins.Event{{Type: "generic", Payload: req.Body}}, nil
}

type stubSourceRegistry struct {
	sources map[string]plugins.EventSource
}

func (registry stubSourceRegistry) EventSource(kind string) (plugins.EventSource, string, bool) {
	source, ok := registry.sources[kind]
	return source, "stub-plugin", ok
}

func (registry stubSourceRegistry) SourceEvent(kind, eventType string) bool {
	_, ok := registry.sources[kind]
	return ok && (eventType == "alerts.batch" || eventType == "generic")
}

func alertmanagerRegistry(fail bool) stubSourceRegistry {
	source := &stubEventSource{fail: fail}
	return stubSourceRegistry{sources: map[string]plugins.EventSource{"alertmanager": source}}
}

const validPayload = `{"status":"firing","alerts":[{"labels":{"alertname":"Test"}}]}`

func newTestWebhook(t *testing.T, lookup *stubLookup, sources SourceRegistry) (*httptest.Server, *Queue) {
	t.Helper()
	server, queue, _ := newTestWebhookWithMetrics(t, lookup, sources)
	return server, queue
}

// newTestWebhookWithMetrics builds the same real webhook but additionally
// exposes the Metrics instance so tests can assert the intake counters of
// the real pipeline.
func newTestWebhookWithMetrics(t *testing.T, lookup *stubLookup, sources SourceRegistry) (*httptest.Server, *Queue, *Metrics) {
	t.Helper()
	queue := openTestQueue(t)
	metrics := NewMetrics()
	webhook := NewWebhook(queue, lookup, sources, metrics)
	server := httptest.NewServer(webhook.Handler())
	t.Cleanup(server.Close)
	return server, queue, metrics
}

// testBearer 是确定性的测试凭据：32 字节 1..32 的 base64url 文本 + 其
// 原始字节 SHA-256 digest（与 SEC-REVEAL-001 的派生规则一致）。
func testBearer() (string, []byte) {
	raw := make([]byte, 32)
	for index := range raw {
		raw[index] = byte(index + 1)
	}
	digest := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(raw), digest[:]
}

func bearerRequest(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	bearer, _ := testBearer()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	return request
}

func TestWebhookUnknownSourceIs404(t *testing.T) {
	lookup := &stubLookup{ready: true}
	server, _ := newTestWebhook(t, lookup, alertmanagerRegistry(false))
	response, err := http.Post(server.URL+"/webhook/nosuchsource", "application/json", strings.NewReader(validPayload))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown source status = %d, want 404", response.StatusCode)
	}
}

func TestWebhookWithoutSnapshotIs503(t *testing.T) {
	lookup := &stubLookup{ready: false}
	server, queue := newTestWebhook(t, lookup, alertmanagerRegistry(false))
	response, err := http.Post(server.URL+"/webhook/alertmanager", "application/json", strings.NewReader(validPayload))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no snapshot status = %d, want 503", response.StatusCode)
	}
	if depth, _ := queue.QueueDepth(context.Background()); depth != 0 {
		t.Fatalf("queue depth = %d, want 0", depth)
	}
}

func TestWebhookMissingOrWrongBearerIs401(t *testing.T) {
	_, digest := testBearer()
	lookup := &stubLookup{ready: true, digest: digest}
	server, _ := newTestWebhook(t, lookup, alertmanagerRegistry(false))

	response, err := http.Post(server.URL+"/webhook/alertmanager", "application/json", strings.NewReader(validPayload))
	if err != nil {
		t.Fatalf("post without bearer: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing bearer status = %d, want 401", response.StatusCode)
	}

	request, _ := http.NewRequest(http.MethodPost, server.URL+"/webhook/alertmanager", strings.NewReader(validPayload))
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("A", 43))
	httpResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post with wrong bearer: %v", err)
	}
	httpResponse.Body.Close()
	if httpResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong bearer status = %d, want 401", httpResponse.StatusCode)
	}
}

func TestWebhookCredentialBoundToSourceProtocol(t *testing.T) {
	// digest 命中但 protocol 不匹配（source 路径换成别的已装配 kind）时
	// 必须拒绝：凭据与来源协议一一绑定。
	_, digest := testBearer()
	lookup := &stubLookup{ready: true, digest: digest}
	mixed := stubSourceRegistry{sources: map[string]plugins.EventSource{
		"alertmanager": &stubEventSource{},
		"other":        stubKindSource("other"),
	}}
	server, _ := newTestWebhook(t, lookup, mixed)
	response, err := http.DefaultClient.Do(bearerRequest(t, http.MethodPost, server.URL+"/webhook/other", validPayload))
	if err != nil {
		t.Fatalf("post cross-protocol: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cross-protocol bearer status = %d, want 401", response.StatusCode)
	}
}

func TestWebhookMalformedPayloadIs400BeforeEnqueue(t *testing.T) {
	_, digest := testBearer()
	lookup := &stubLookup{ready: true, digest: digest}
	server, queue := newTestWebhook(t, lookup, alertmanagerRegistry(true))

	response, err := http.DefaultClient.Do(bearerRequest(t, http.MethodPost, server.URL+"/webhook/alertmanager", validPayload))
	if err != nil {
		t.Fatalf("post malformed: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed payload status = %d, want 400", response.StatusCode)
	}
	if depth, _ := queue.QueueDepth(context.Background()); depth != 0 {
		t.Fatalf("queue depth after rejection = %d, want 0", depth)
	}
}

func TestWebhookUndeclaredEventIsRejectedBeforeEnqueue(t *testing.T) {
	_, digest := testBearer()
	lookup := &stubLookup{ready: true, digest: digest}
	registry := stubSourceRegistry{sources: map[string]plugins.EventSource{
		"alertmanager": &stubEventSource{eventType: "undeclared.event"},
	}}
	server, queue := newTestWebhook(t, lookup, registry)
	response, err := http.DefaultClient.Do(bearerRequest(t, http.MethodPost, server.URL+"/webhook/alertmanager", validPayload))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("undeclared event status=%d", response.StatusCode)
	}
	if depth, err := queue.QueueDepth(context.Background()); err != nil || depth != 0 {
		t.Fatalf("undeclared event enqueued: depth=%d err=%v", depth, err)
	}
}

func TestWebhookAcceptsAndEnqueues(t *testing.T) {
	_, digest := testBearer()
	lookup := &stubLookup{ready: true, digest: digest}
	server, queue := newTestWebhook(t, lookup, alertmanagerRegistry(false))

	response, err := http.DefaultClient.Do(bearerRequest(t, http.MethodPost, server.URL+"/webhook/alertmanager", validPayload))
	if err != nil {
		t.Fatalf("post valid: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("valid payload status = %d, want 202", response.StatusCode)
	}
	events, err := queue.FetchDueBatch(context.Background(), 10, time.Now().UTC())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("queued events = %d, want 1", len(events))
	}
	event := events[0]
	if event.SourceKind != "alertmanager" || event.SourceID != 7 || event.CredentialID != 9 ||
		event.CredentialSnapshotVersion != 3 || event.EventType != "alerts.batch" {
		t.Fatalf("queued event lost attribution fields: %+v", event)
	}
	if string(event.Payload) != validPayload {
		t.Fatalf("payload = %q, want the normalized body verbatim", event.Payload)
	}
	if len(event.ID) != 22 { // 16 字节 base64url
		t.Fatalf("event id %q is not 16 raw bytes of base64url", event.ID)
	}
}

func TestWebhookMethodAndPathRouting(t *testing.T) {
	lookup := &stubLookup{ready: true}
	server, _ := newTestWebhook(t, lookup, alertmanagerRegistry(false))
	response, err := http.Get(server.URL + "/webhook/alertmanager")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", response.StatusCode)
	}
	response, err = http.Get(server.URL + "/other")
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("non-webhook path status = %d, want 404", response.StatusCode)
	}
	response, err = http.Post(server.URL+"/webhook/alertmanager/extra", "application/json", strings.NewReader(validPayload))
	if err != nil {
		t.Fatalf("post nested path: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("nested path status = %d, want 404", response.StatusCode)
	}
}

// TestWebhookIntakeMetricPerRequestVerdict 验证入站计数的真实行为与单位：
// 每次请求恰好一次裁决计数。202（含上游重复投递同批）各计一次
// accepted；4xx 入站拒绝计 rejected；5xx 不可用计 unavailable；多事件
// 载荷不放大 accepted（逐事件计数属于 stele_events_forwarded_total）。
// source 解析前的传输层拒绝（404/405）不进入入站裁决计数。
func TestWebhookIntakeMetricPerRequestVerdict(t *testing.T) {
	_, digest := testBearer()
	lookup := &stubLookup{ready: true, digest: digest}
	server, queue, metrics := newTestWebhookWithMetrics(t, lookup, alertmanagerRegistry(false))

	intake := func(status string) float64 {
		var metricDTO dto.Metric
		if err := metrics.deliveries.WithLabelValues(status).(prometheus.Metric).Write(&metricDTO); err != nil {
			t.Fatalf("read intake counter %s: %v", status, err)
		}
		return metricDTO.GetCounter().GetValue()
	}
	if intake("accepted") != 0 || intake("rejected") != 0 || intake("unavailable") != 0 {
		t.Fatalf("intake counters must start at the closed-label zeros")
	}

	// 成功 202：一次请求计一次 accepted。
	response, err := http.DefaultClient.Do(bearerRequest(t, http.MethodPost, server.URL+"/webhook/alertmanager", validPayload))
	if err != nil {
		t.Fatalf("post valid: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("valid payload status = %d, want 202", response.StatusCode)
	}
	if intake("accepted") != 1 {
		t.Fatalf("accepted = %v after first 202, want 1", intake("accepted"))
	}

	// 上游重复投递同一批：再次 202，再计一次 accepted（幂等去重在上游）。
	response, err = http.DefaultClient.Do(bearerRequest(t, http.MethodPost, server.URL+"/webhook/alertmanager", validPayload))
	if err != nil {
		t.Fatalf("post duplicate: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("duplicate payload status = %d, want 202", response.StatusCode)
	}
	if intake("accepted") != 2 {
		t.Fatalf("accepted = %v after duplicate 202, want 2", intake("accepted"))
	}
	if depth, _ := queue.QueueDepth(context.Background()); depth != 2 {
		t.Fatalf("queue depth = %d, want 2 (each 202 enqueues its own batch)", depth)
	}

	// 认证失败 401：一次请求计一次 rejected，accepted 不变。
	response, err = http.Post(server.URL+"/webhook/alertmanager", "application/json", strings.NewReader(validPayload))
	if err != nil {
		t.Fatalf("post without bearer: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing bearer status = %d, want 401", response.StatusCode)
	}
	if intake("rejected") != 1 || intake("accepted") != 2 {
		t.Fatalf("after 401: accepted=%v rejected=%v, want 2/1", intake("accepted"), intake("rejected"))
	}

	// 解析失败 400：计一次 rejected。
	response, err = http.DefaultClient.Do(bearerRequest(t, http.MethodPost, server.URL+"/webhook/alertmanager", "not-json"))
	if err != nil {
		t.Fatalf("post malformed: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed status = %d, want 400", response.StatusCode)
	}
	if intake("rejected") != 2 || intake("accepted") != 2 || intake("unavailable") != 0 {
		t.Fatalf("after 400: accepted=%v rejected=%v unavailable=%v, want 2/2/0",
			intake("accepted"), intake("rejected"), intake("unavailable"))
	}

	// 传输层拒绝（未知 source 404、GET 405、嵌套路径 404）：不是入站裁决，不计数。
	if response, err = http.Post(server.URL+"/webhook/nosuchsource", "application/json", strings.NewReader(validPayload)); err != nil {
		t.Fatalf("post unknown source: %v", err)
	} else {
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown source status = %d, want 404", response.StatusCode)
		}
	}
	if response, err = http.Get(server.URL + "/webhook/alertmanager"); err != nil {
		t.Fatalf("get: %v", err)
	} else {
		response.Body.Close()
		if response.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("GET status = %d, want 405", response.StatusCode)
		}
	}
	if intake("accepted") != 2 || intake("rejected") != 2 || intake("unavailable") != 0 {
		t.Fatalf("transport-level rejections must not be counted as intake verdicts: accepted=%v rejected=%v unavailable=%v",
			intake("accepted"), intake("rejected"), intake("unavailable"))
	}
}

// TestWebhookDeliversMatchedSourceSettingsToPlugin（ADR-0014 story 2）：
// Bearer/digest 命中并把来源实例锁定之后，命中实例的非秘密设置才进入
// InboundRequest.Settings；认证失败（401）绝不触达插件，设置也绝不随事件
// 载荷扩散（载荷仍是原始 body）。
func TestWebhookDeliversMatchedSourceSettingsToPlugin(t *testing.T) {
	_, digest := testBearer()
	source := &stubEventSource{}
	lookup := &stubLookup{ready: true, digest: digest, settings: []byte(`{"ignoredAlertnames":["noise"]}`)}
	queue := openTestQueue(t)
	webhook := NewWebhook(queue, lookup, stubSourceRegistry{sources: map[string]plugins.EventSource{"alertmanager": source}}, NewMetrics())
	server := httptest.NewServer(webhook.Handler())
	defer server.Close()

	response, err := http.DefaultClient.Do(bearerRequest(t, http.MethodPost, server.URL+"/webhook/alertmanager", validPayload))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.StatusCode)
	}
	if string(source.settingsSeen) != `{"ignoredAlertnames":["noise"]}` {
		t.Fatalf("plugin settings = %q, want the matched instance document", source.settingsSeen)
	}

	// 认证失败路径：错误 bearer 401，插件绝不被调用（settingsSeen 不变）。
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/webhook/alertmanager", strings.NewReader(validPayload))
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("A", 43))
	denied, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("wrong bearer post: %v", err)
	}
	denied.Body.Close()
	if denied.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong bearer status = %d, want 401", denied.StatusCode)
	}
	if string(source.settingsSeen) != `{"ignoredAlertnames":["noise"]}` {
		t.Fatalf("failed authentication reached the plugin: %q", source.settingsSeen)
	}

	// 设置不进入队列载荷：outbox payload 只是原始请求体。
	batch, err := queue.FetchDueBatch(context.Background(), 10, time.Now())
	if err != nil || len(batch) != 1 {
		t.Fatalf("due batch = %v err=%v", batch, err)
	}
	if strings.Contains(string(batch[0].Payload), "ignoredAlertnames") {
		t.Fatalf("settings leaked into the queued payload: %s", batch[0].Payload)
	}
}
