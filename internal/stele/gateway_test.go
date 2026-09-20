package stele

// 出向网关执行验证：握手/心跳流、材料获取与 revision 失效重取、认证注入、
// HTTP 状态透传、限流、超时、凭证不可用与请求校验。平台用 httptest 假
// 服务，流与材料获取用内存 stub（envelopeStream / materialAcquirer 注入）。

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
)

// fakeStream 是 SteleRelay.Connect 流的内存实现：sent 收集 Stele→Quoin 帧，
// incoming 注入 Quoin→Stele 帧，关闭 incoming 即模拟流断开。
type fakeStream struct {
	mu       sync.Mutex
	sent     chan *runtimev1.SteleEnvelope
	incoming chan *runtimev1.SteleEnvelope
}

func newFakeStream() *fakeStream {
	return &fakeStream{
		sent:     make(chan *runtimev1.SteleEnvelope, 128),
		incoming: make(chan *runtimev1.SteleEnvelope, 128),
	}
}

func (stream *fakeStream) Send(envelope *runtimev1.SteleEnvelope) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	stream.sent <- envelope
	return nil
}

func (stream *fakeStream) Recv() (*runtimev1.SteleEnvelope, error) {
	envelope, ok := <-stream.incoming
	if !ok {
		return nil, io.EOF
	}
	return envelope, nil
}

func (stream *fakeStream) CloseSend() error { return nil }

// platformRecord 捕获假平台看到的一次请求。
type platformRecord struct {
	Method string
	Target string
	Auth   string
}

// newPlatform 起一个可编程的假平台。
func newPlatform(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

// acquireResponse 组装一份 Acquire 响应（MetricsConfig 形状 + secret）。
func acquireResponse(connectionID, revision int64, baseURL, authType, username, password, bearer string) *runtimev1.AcquireConnectionCredentialResponse {
	return &runtimev1.AcquireConnectionCredentialResponse{
		ConnectionId:         connectionID,
		ConnectionRevisionId: revision,
		ConnectionType:       "prometheus",
		RevisionConfigJson: []byte(fmt.Sprintf(
			`{"type":"prometheus","baseUrl":%q,"authType":%q,"username":%q}`, baseURL, authType, username)),
		Thanos: &runtimev1.ThanosCredentialSecret{Username: username, Password: password, BearerToken: bearer},
	}
}

// gatewayHarness 聚合一个运行中的网关与其注入件。
type gatewayHarness struct {
	gateway   *Gateway
	stream    *fakeStream
	queue     *Queue
	cancel    context.CancelFunc
	done      chan struct{}
	hello     *runtimev1.SteleHello
	bootKnown func() string
	stopOnce  sync.Once
}

// stop 幂等地结束网关：取消 ctx 并断开流，等待 Run（含末次限流刷库）退出。
func (harness *gatewayHarness) stop(t *testing.T) {
	harness.stopOnce.Do(func() {
		harness.cancel()
		close(harness.stream.incoming)
		select {
		case <-harness.done:
		case <-time.After(5 * time.Second):
			t.Error("gateway Run did not stop within 5s of cancel")
		}
	})
}

// startGateway 启动一个用 stub 流与材料的网关；dial 即自动回 accepted
// hello_ack，模拟 Quoin 握手裁决。
func startGateway(t *testing.T, acquire materialAcquirer, ratePerMinute float64) *gatewayHarness {
	t.Helper()
	stream := newFakeStream()
	queue := openTestQueue(t)
	gateway := newGateway(
		func(ctx context.Context) (envelopeStream, error) {
			// 模拟服务端：立即回 accepted hello_ack。
			stream.incoming <- &runtimev1.SteleEnvelope{
				Msg: &runtimev1.SteleEnvelope_HelloAck{HelloAck: &runtimev1.SteleHelloAck{Accepted: true}},
			}
			return stream, nil
		},
		acquire,
		queue,
		NewMetrics(),
	)
	gateway.ratePerMinute = ratePerMinute
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { gateway.Run(ctx); close(done) }()
	harness := &gatewayHarness{
		gateway: gateway, stream: stream, queue: queue, cancel: cancel, done: done,
		bootKnown: func() string { return gateway.bootID },
	}
	t.Cleanup(func() { harness.stop(t) })
	// 消费首帧 hello 并留档，确认流已建立。
	harness.hello = waitEnvelope(t, stream.sent, 5*time.Second, func(envelope *runtimev1.SteleEnvelope) bool {
		return envelope.GetHello() != nil
	}).GetHello()
	return harness
}

// pushExecute 注入一帧服务端执行请求，返回供轮询的 call_id。
func (harness *gatewayHarness) pushExecute(callID string, connectionID, revision int64, method, path, query string, timeoutMs uint32, body []byte) {
	harness.stream.incoming <- &runtimev1.SteleEnvelope{
		MessageId: 500, CorrelationId: 777, BootId: harness.bootKnown(),
		Msg: &runtimev1.SteleEnvelope_Execute{Execute: &runtimev1.ExecutePlatformCall{
			CallId: callID, ConnectionId: connectionID, ConnectionRevisionId: revision,
			Method: method, Path: path, Query: query, TimeoutMs: timeoutMs, Body: body,
		}},
	}
}

// awaitResult 等待指定 call_id 的执行结果帧。
func (harness *gatewayHarness) awaitResult(t *testing.T, callID string) *runtimev1.ExecutePlatformCallResult {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case envelope := <-harness.stream.sent:
			if result := envelope.GetExecuteResult(); result != nil && result.GetCallId() == callID {
				if envelope.GetCorrelationId() != 777 {
					t.Fatalf("result correlation = %d, want 777 (request correlation)", envelope.GetCorrelationId())
				}
				if envelope.GetBootId() != harness.bootKnown() {
					t.Fatalf("result boot id = %q, want the process boot id", envelope.GetBootId())
				}
				return result
			}
		case <-deadline:
			t.Fatalf("result for call %s did not arrive within 5s", callID)
		case <-harness.done:
			t.Fatalf("gateway stopped before answering call %s", callID)
		}
	}
}

func waitEnvelope(t *testing.T, source <-chan *runtimev1.SteleEnvelope, timeout time.Duration, match func(*runtimev1.SteleEnvelope) bool) *runtimev1.SteleEnvelope {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case envelope := <-source:
			if match(envelope) {
				return envelope
			}
		case <-deadline:
			t.Fatal("expected envelope did not arrive in time")
			return nil
		}
	}
}

func TestGatewayReadyAfterAcceptedHandshake(t *testing.T) {
	harness := startGateway(t, func(context.Context, int64) (*runtimev1.AcquireConnectionCredentialResponse, error) {
		return acquireResponse(1, 1, "http://unused.invalid", "none", "", "", ""), nil
	}, gatewayDefaultRatePerMinute)
	if !harness.gateway.Ready() {
		t.Fatal("gateway must be ready after an accepted hello ack")
	}
}

func TestGatewayHelloCarriesIdentityAndFingerprint(t *testing.T) {
	harness := startGateway(t, func(context.Context, int64) (*runtimev1.AcquireConnectionCredentialResponse, error) {
		return acquireResponse(1, 1, "http://unused.invalid", "none", "", "", ""), nil
	}, gatewayDefaultRatePerMinute)
	hello := harness.hello
	if hello.GetBootId() != harness.bootKnown() || hello.GetConnectionEpoch() != 1 {
		t.Fatalf("hello identity = %s/%d, want process boot id and epoch 1", hello.GetBootId(), hello.GetConnectionEpoch())
	}
	if hello.GetContractFingerprint() == "" || hello.GetReleaseVersion() == "" {
		t.Fatal("hello must carry the contract fingerprint and release version")
	}
}

func TestGatewayExecutesWithBasicAuthAndSucceeds(t *testing.T) {
	var records sync.Mutex
	var seen []platformRecord
	platform := newPlatform(t, func(writer http.ResponseWriter, request *http.Request) {
		records.Lock()
		seen = append(seen, platformRecord{Method: request.Method, Target: request.URL.RequestURI(), Auth: request.Header.Get("Authorization")})
		records.Unlock()
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"status":"success"}`))
	})
	harness := startGateway(t, func(_ context.Context, connectionID int64) (*runtimev1.AcquireConnectionCredentialResponse, error) {
		return acquireResponse(connectionID, 11, platform.URL, "basic", "projection-user", "secret-pass", ""), nil
	}, gatewayDefaultRatePerMinute)

	harness.pushExecute("call-1", 42, 11, "GET", "/api/v1/query", "query=up", 5000, nil)
	result := harness.awaitResult(t, "call-1")
	if result.GetStatus() != runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_SUCCEEDED {
		t.Fatalf("status = %v (%s %s), want SUCCEEDED", result.GetStatus(), result.GetErrorCode(), result.GetErrorDetail())
	}
	if result.GetHttpStatus() != http.StatusOK || string(result.GetBody()) != `{"status":"success"}` {
		t.Fatalf("http result = %d %q", result.GetHttpStatus(), result.GetBody())
	}
	if result.GetLatencyMs() > 5000 {
		t.Fatalf("latency %dms exceeded the request budget", result.GetLatencyMs())
	}
	records.Lock()
	defer records.Unlock()
	if len(seen) != 1 {
		t.Fatalf("platform saw %d requests, want 1", len(seen))
	}
	// secret 里的用户名优先于非秘密投影的 username。
	expected := "Basic " + basicAuthHeader("projection-user", "secret-pass")
	if seen[0].Auth != expected {
		t.Fatalf("authorization = %q, want %q", seen[0].Auth, expected)
	}
	if seen[0].Method != http.MethodGet || seen[0].Target != "/api/v1/query?query=up" {
		t.Fatalf("platform request = %s %s", seen[0].Method, seen[0].Target)
	}
}

func TestGatewayBearerAuthAndHTTPErrorPassThrough(t *testing.T) {
	var auth atomic.Value
	platform := newPlatform(t, func(writer http.ResponseWriter, request *http.Request) {
		auth.Store(request.Header.Get("Authorization"))
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte("not found"))
	})
	harness := startGateway(t, func(_ context.Context, connectionID int64) (*runtimev1.AcquireConnectionCredentialResponse, error) {
		return acquireResponse(connectionID, 5, platform.URL, "bearer", "", "", "tok-123"), nil
	}, gatewayDefaultRatePerMinute)

	harness.pushExecute("call-404", 7, 5, "POST", "/api/v1/notthere", "", 5000, []byte(`{}`))
	result := harness.awaitResult(t, "call-404")
	// 平台返回了 HTTP 响应：即使 4xx 也是 SUCCEEDED，语义解释归 Quoin。
	if result.GetStatus() != runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_SUCCEEDED {
		t.Fatalf("status = %v, want SUCCEEDED for an HTTP response", result.GetStatus())
	}
	if result.GetHttpStatus() != http.StatusNotFound || string(result.GetBody()) != "not found" {
		t.Fatalf("http result = %d %q", result.GetHttpStatus(), result.GetBody())
	}
	if got, _ := auth.Load().(string); got != "Bearer tok-123" {
		t.Fatalf("authorization = %q, want Bearer tok-123", got)
	}
}

func TestGatewayLegacyAuthTypeFallsBackToBasic(t *testing.T) {
	// authType 为空 + password 非空：兼容旧数据的 basic 语义。
	var auth atomic.Value
	platform := newPlatform(t, func(writer http.ResponseWriter, request *http.Request) {
		auth.Store(request.Header.Get("Authorization"))
		writer.WriteHeader(http.StatusOK)
	})
	harness := startGateway(t, func(_ context.Context, connectionID int64) (*runtimev1.AcquireConnectionCredentialResponse, error) {
		return acquireResponse(connectionID, 1, platform.URL, "", "legacy-user", "legacy-pass", ""), nil
	}, gatewayDefaultRatePerMinute)

	harness.pushExecute("call-legacy", 8, 1, "GET", "/metrics", "", 5000, nil)
	result := harness.awaitResult(t, "call-legacy")
	if result.GetStatus() != runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_SUCCEEDED {
		t.Fatalf("status = %v (%s), want SUCCEEDED", result.GetStatus(), result.GetErrorDetail())
	}
	if got, _ := auth.Load().(string); got != "Basic "+basicAuthHeader("legacy-user", "legacy-pass") {
		t.Fatalf("authorization = %q, want legacy basic auth", got)
	}
}

func TestGatewayRateLimitDeniesAndCountersPersist(t *testing.T) {
	platform := newPlatform(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})
	var acquires atomic.Int64
	harness := startGateway(t, func(_ context.Context, connectionID int64) (*runtimev1.AcquireConnectionCredentialResponse, error) {
		acquires.Add(1)
		return acquireResponse(connectionID, 1, platform.URL, "none", "", "", ""), nil
	}, 1) // 1/min：桶容量 1，第二次立即调用必被拒。

	harness.pushExecute("call-first", 9, 1, "GET", "/ok", "", 5000, nil)
	first := harness.awaitResult(t, "call-first")
	if first.GetStatus() != runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_SUCCEEDED {
		t.Fatalf("first status = %v (%s), want SUCCEEDED", first.GetStatus(), first.GetErrorDetail())
	}
	harness.pushExecute("call-second", 9, 1, "GET", "/ok", "", 5000, nil)
	second := harness.awaitResult(t, "call-second")
	if second.GetStatus() != runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_RATE_LIMITED || second.GetErrorCode() != "rate_limited" {
		t.Fatalf("second status = %v code=%s, want RATE_LIMITED", second.GetStatus(), second.GetErrorCode())
	}
	// 被限流的调用不应触发材料获取（配额先于 Acquire）。
	if acquires.Load() != 1 {
		t.Fatalf("acquires = %d, want 1 (rate limit gates acquire)", acquires.Load())
	}
	// 取消退出后限流计数落入本地库。
	harness.stop(t)
	allowed, denied, err := harness.queue.RateCounter(context.Background(), 9)
	if err != nil {
		t.Fatalf("read rate counters: %v", err)
	}
	if allowed != 1 || denied != 1 {
		t.Fatalf("rate counters = %d/%d, want 1/1", allowed, denied)
	}
}

func TestGatewayTimeoutIsUnreachable(t *testing.T) {
	platform := newPlatform(t, func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(1500 * time.Millisecond) // 超过夹取后的 1s 下限即可
		writer.WriteHeader(http.StatusOK)
	})
	harness := startGateway(t, func(_ context.Context, connectionID int64) (*runtimev1.AcquireConnectionCredentialResponse, error) {
		return acquireResponse(connectionID, 1, platform.URL, "none", "", "", ""), nil
	}, gatewayDefaultRatePerMinute)

	harness.pushExecute("call-slow", 10, 1, "GET", "/slow", "", 1000, nil)
	result := harness.awaitResult(t, "call-slow")
	if result.GetStatus() != runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_UNREACHABLE || result.GetErrorCode() != "timeout" {
		t.Fatalf("status = %v code=%s, want UNREACHABLE/timeout", result.GetStatus(), result.GetErrorCode())
	}
}

func TestGatewayCredentialUnavailable(t *testing.T) {
	platform := newPlatform(t, func(http.ResponseWriter, *http.Request) {})
	harness := startGateway(t, func(context.Context, int64) (*runtimev1.AcquireConnectionCredentialResponse, error) {
		return nil, errors.New("connection revoked")
	}, gatewayDefaultRatePerMinute)

	harness.pushExecute("call-nocred", 11, 3, "GET", "/anything", "", 5000, nil)
	result := harness.awaitResult(t, "call-nocred")
	if result.GetStatus() != runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_CREDENTIAL_UNAVAILABLE ||
		result.GetErrorCode() != "credential_unavailable" || result.GetErrorDetail() == "" {
		t.Fatalf("status = %v code=%s detail=%q, want CREDENTIAL_UNAVAILABLE with detail", result.GetStatus(), result.GetErrorCode(), result.GetErrorDetail())
	}
	_ = platform
}

func TestGatewayRejectsInvalidRequests(t *testing.T) {
	platform := newPlatform(t, func(http.ResponseWriter, *http.Request) {})
	harness := startGateway(t, func(_ context.Context, connectionID int64) (*runtimev1.AcquireConnectionCredentialResponse, error) {
		return acquireResponse(connectionID, 1, platform.URL, "none", "", "", ""), nil
	}, gatewayDefaultRatePerMinute)

	cases := []struct {
		callID string
		method string
		path   string
		body   []byte
	}{
		{"call-method", "DELETE", "/x", nil},
		{"call-path", "GET", "no-slash", nil},
		{"call-get-body", "GET", "/x", []byte(`{}`)},
	}
	for _, testCase := range cases {
		harness.pushExecute(testCase.callID, 12, 1, testCase.method, testCase.path, "", 5000, testCase.body)
		result := harness.awaitResult(t, testCase.callID)
		if result.GetStatus() != runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_INTERNAL || result.GetErrorCode() != "invalid_request" {
			t.Fatalf("%s: status = %v code=%s, want INTERNAL/invalid_request", testCase.callID, result.GetStatus(), result.GetErrorCode())
		}
	}
}

func TestGatewayRevisionDriftReacquiresMaterial(t *testing.T) {
	first := newPlatform(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("v1"))
	})
	second := newPlatform(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("v2"))
	})
	current := &atomic.Value{}
	current.Store(first.URL)
	var acquires atomic.Int64
	harness := startGateway(t, func(_ context.Context, connectionID int64) (*runtimev1.AcquireConnectionCredentialResponse, error) {
		count := acquires.Add(1)
		return acquireResponse(connectionID, int64(count), current.Load().(string), "none", "", "", ""), nil
	}, gatewayDefaultRatePerMinute)

	harness.pushExecute("call-r1", 13, 1, "GET", "/", "", 5000, nil)
	firstResult := harness.awaitResult(t, "call-r1")
	if string(firstResult.GetBody()) != "v1" {
		t.Fatalf("first body = %q, want v1", firstResult.GetBody())
	}
	// revision 漂移：缓存失效，重新 Acquire 指向新 endpoint。
	current.Store(second.URL)
	harness.pushExecute("call-r2", 13, 2, "GET", "/", "", 5000, nil)
	secondResult := harness.awaitResult(t, "call-r2")
	if string(secondResult.GetBody()) != "v2" {
		t.Fatalf("second body = %q, want v2", secondResult.GetBody())
	}
	if acquires.Load() != 2 {
		t.Fatalf("acquires = %d, want 2", acquires.Load())
	}
}

func TestGatewayResponseTooLargeIsInternal(t *testing.T) {
	platform := newPlatform(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(make([]byte, gatewayMaxResponseBody+1))
	})
	harness := startGateway(t, func(_ context.Context, connectionID int64) (*runtimev1.AcquireConnectionCredentialResponse, error) {
		return acquireResponse(connectionID, 1, platform.URL, "none", "", "", ""), nil
	}, gatewayDefaultRatePerMinute)

	harness.pushExecute("call-huge", 14, 1, "GET", "/big", "", 30000, nil)
	result := harness.awaitResult(t, "call-huge")
	if result.GetStatus() != runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_INTERNAL || result.GetErrorCode() != "response_too_large" {
		t.Fatalf("status = %v code=%s, want INTERNAL/response_too_large", result.GetStatus(), result.GetErrorCode())
	}
}

// basicAuthHeader 生成 Basic 凭据头的值（测试断言用）。
func basicAuthHeader(username, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
}
