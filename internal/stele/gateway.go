package stele

// 出向网关执行（ADR-0011）：经 SteleRelay.Connect 长期流接收 Quoin 下发的
// ExecutePlatformCall，解析连接材料（AcquireConnectionCredential 按需拉取并
// 缓存于内存），按连接限流后执行 HTTP 并回传 ExecutePlatformCallResult。
// Stele 的判断只有：签名对不对、配额超没超、平台通没通——任何 HTTP 状态码
// 都是 SUCCEEDED，语义解释归 Quoin 侧 Handler。

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
	"github.com/Suknna/quoin/internal/contract"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
)

const (
	gatewayHeartbeatInterval  = 10 * time.Second
	gatewayReconnectBackoff   = 2 * time.Second
	gatewayHelloRejectBackoff = 5 * time.Second
	gatewayRateFlushInterval  = 30 * time.Second
	// 每连接默认 60 请求/分钟，桶容量 = 速率（允许一次小突发后回到稳态）。
	gatewayDefaultRatePerMinute = 60
	// 出向请求超时夹在 [1s, 60s]（timeout_ms 是部署侧上限的期望值）。
	gatewayMinTimeout      = 1 * time.Second
	gatewayMaxTimeout      = 60 * time.Second
	gatewayMaxResponseBody = 16 << 20 // 16 MiB
)

// envelopeStream 是网关流的最小面（grpc.BidiStreamingClient 满足；测试用
// 内存管道实现）。
type envelopeStream interface {
	Send(*runtimev1.SteleEnvelope) error
	Recv() (*runtimev1.SteleEnvelope, error)
	CloseSend() error
}

// streamDialer / materialAcquirer 把"建流"与"取材料"抽象成可注入函数。
type (
	streamDialer     func(ctx context.Context) (envelopeStream, error)
	materialAcquirer func(ctx context.Context, connectionID int64) (*runtimev1.AcquireConnectionCredentialResponse, error)
)

// material 是一个连接的已解析执行材料；revision 变化即失效重取。client 按
// 连接 TLS 配置构建一次复用。
type material struct {
	revisionID     int64
	connectionType string
	baseURL        string
	authType       string
	username       string
	password       string
	bearerToken    string
	client         *http.Client
}

// revisionConfig 是 Quoin 下发的非秘密类型化投影（MetricsConfig 形状）。
type revisionConfig struct {
	Type          string `json:"type"`
	BaseURL       string `json:"baseUrl"`
	TLSCaPem      string `json:"tlsCaPem,omitempty"`
	TLSServerName string `json:"tlsServerName,omitempty"`
	TLSSkipVerify bool   `json:"tlsSkipVerify,omitempty"`
	AuthType      string `json:"authType,omitempty"`
	Username      string `json:"username,omitempty"`
}

// rateCounter 是一个连接的限流判定累计（内存增量，周期刷入 rate_counters）。
// 字段用原子操作：执行 goroutine 并发累计，flush 周期原子换出增量。
type rateCounter struct {
	allowed atomic.Int64
	denied  atomic.Int64
}

// tokenBucket 是经典的按速率补充令牌桶；速率单位为"每分钟"。
type tokenBucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
	perMin float64
	burst  float64
}

func newTokenBucket(perMin float64, now time.Time) *tokenBucket {
	return &tokenBucket{tokens: perMin, last: now, perMin: perMin, burst: perMin}
}

func (bucket *tokenBucket) allow(now time.Time) bool {
	bucket.mu.Lock()
	defer bucket.mu.Unlock()
	elapsed := now.Sub(bucket.last).Seconds()
	if elapsed > 0 {
		bucket.tokens += elapsed * bucket.perMin / 60
		if bucket.tokens > bucket.burst {
			bucket.tokens = bucket.burst
		}
		bucket.last = now
	}
	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens--
	return true
}

// Gateway 是 Stele 的出向执行半边：长期流 + 连接材料缓存 + 每连接限流。
type Gateway struct {
	dial    streamDialer
	acquire materialAcquirer
	queue   *Queue
	metrics *Metrics

	bootID string

	mu        sync.Mutex
	epoch     uint64
	materials map[int64]*material
	buckets   map[int64]*tokenBucket
	counters  map[int64]*rateCounter

	ratePerMinute float64 // 测试可调低；默认 gatewayDefaultRatePerMinute

	readyFlag atomic.Bool
}

// NewGateway wires the production gateway onto the shared Relay conn.
func NewGateway(relay *Relay, queue *Queue, metrics *Metrics) *Gateway {
	return newGateway(relay.ConnectStreamWrapped, relay.AcquireConnectionCredential, queue, metrics)
}

func newGateway(dial streamDialer, acquire materialAcquirer, queue *Queue, metrics *Metrics) *Gateway {
	bootRaw := make([]byte, 16)
	if _, err := rand.Read(bootRaw); err != nil {
		// crypto/rand 失败意味着进程环境的熵源已坏，快速失败比静默降级好。
		panic(fmt.Sprintf("stele: generate gateway boot id: %v", err))
	}
	return &Gateway{
		dial: dial, acquire: acquire, queue: queue, metrics: metrics,
		bootID:    hex.EncodeToString(bootRaw),
		materials: map[int64]*material{}, buckets: map[int64]*tokenBucket{},
		counters: map[int64]*rateCounter{}, ratePerMinute: gatewayDefaultRatePerMinute,
	}
}

// Ready reports whether an accepted gateway stream is currently established.
// 它只作观测（ops readiness 不因网关未就绪而阻塞入向）。
func (gateway *Gateway) Ready() bool {
	return gateway.readyFlag.Load()
}

// Run maintains the stream until the context ends: hello/hello_ack 握手、
// 心跳、执行循环、断流退避重连；退出前把限流计数刷入本地库。
func (gateway *Gateway) Run(ctx context.Context) {
	flushStop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(gatewayRateFlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				gateway.flushRateCounters()
				close(flushStop)
				return
			case <-ticker.C:
				gateway.flushRateCounters()
			}
		}
	}()
	defer func() { <-flushStop }()
	for ctx.Err() == nil {
		backoff, err := gateway.runConnection(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			sharedops.LogEvent("stele", "error", "gateway.stream_failed", err.Error())
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
	}
}

// runConnection 走一条连接的完整生命周期，返回重连前的退避时长。
func (gateway *Gateway) runConnection(ctx context.Context) (time.Duration, error) {
	stream, err := gateway.dial(ctx)
	if err != nil {
		return gatewayReconnectBackoff, fmt.Errorf("dial gateway stream: %w", err)
	}
	gateway.mu.Lock()
	gateway.epoch++
	epoch := gateway.epoch
	gateway.mu.Unlock()
	var messageSeq uint64
	nextMessageID := func() uint64 { return atomic.AddUint64(&messageSeq, 1) }

	if err := stream.Send(&runtimev1.SteleEnvelope{
		MessageId: nextMessageID(), BootId: gateway.bootID,
		Msg: &runtimev1.SteleEnvelope_Hello{Hello: &runtimev1.SteleHello{
			BootId: gateway.bootID, ConnectionEpoch: epoch,
			ContractFingerprint: contract.ProtoAuthorityFingerprint,
			ReleaseVersion:      buildinfo.Release,
		}},
	}); err != nil {
		_ = stream.CloseSend()
		return gatewayReconnectBackoff, fmt.Errorf("send hello: %w", err)
	}
	first, err := stream.Recv()
	if err != nil {
		_ = stream.CloseSend()
		return gatewayReconnectBackoff, fmt.Errorf("await hello ack: %w", err)
	}
	ack := first.GetHelloAck()
	if ack == nil {
		_ = stream.CloseSend()
		return gatewayReconnectBackoff, errors.New("first gateway frame is not a hello ack")
	}
	if !ack.GetAccepted() {
		_ = stream.CloseSend()
		sharedops.LogEvent("stele", "error", "gateway.hello_rejected", ack.GetDetail())
		return gatewayHelloRejectBackoff, fmt.Errorf("hello rejected: %s", ack.GetDetail())
	}

	gateway.readyFlag.Store(true)
	defer gateway.readyFlag.Store(false)
	sharedops.LogEvent("stele", "info", "gateway.stream_accepted",
		fmt.Sprintf("boot_id=%s epoch=%d", gateway.bootID, epoch))

	// 心跳与执行结果共享发送端：Send 非并发安全，统一串行化。
	var sendMu sync.Mutex
	send := func(envelope *runtimev1.SteleEnvelope) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(envelope)
	}
	heartbeatStop := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(gatewayHeartbeatInterval)
		defer ticker.Stop()
		var seq uint64
		for {
			select {
			case <-heartbeatStop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				seq++
				if err := send(&runtimev1.SteleEnvelope{
					MessageId: nextMessageID(), BootId: gateway.bootID,
					Msg: &runtimev1.SteleEnvelope_Heartbeat{Heartbeat: &runtimev1.SteleHeartbeat{Seq: seq}},
				}); err != nil {
					return
				}
			}
		}
	}()
	defer func() {
		close(heartbeatStop)
		<-heartbeatDone
		_ = stream.CloseSend()
	}()

	for {
		envelope, err := stream.Recv()
		if err != nil {
			return gatewayReconnectBackoff, fmt.Errorf("gateway receive: %w", err)
		}
		if envelope.GetBootId() != "" && envelope.GetBootId() != gateway.bootID {
			// 除 SteleHello 外 boot_id 必须匹配本进程上下文；错配视为流错乱。
			return gatewayReconnectBackoff, fmt.Errorf("gateway frame boot id %q does not match", envelope.GetBootId())
		}
		switch message := envelope.GetMsg().(type) {
		case *runtimev1.SteleEnvelope_Execute:
			call := message.Execute
			correlation := envelope.GetCorrelationId()
			if correlation == 0 {
				correlation = envelope.GetMessageId()
			}
			go func(call *runtimev1.ExecutePlatformCall, correlation uint64) {
				result := gateway.executeCall(ctx, call)
				if err := send(&runtimev1.SteleEnvelope{
					MessageId: nextMessageID(), CorrelationId: correlation, BootId: gateway.bootID,
					Msg: &runtimev1.SteleEnvelope_ExecuteResult{ExecuteResult: result},
				}); err != nil {
					sharedops.LogEvent("stele", "error", "gateway.result_send_failed", call.GetCallId()+": "+err.Error())
				}
			}(call, correlation)
		case *runtimev1.SteleEnvelope_GoAway:
			// 尽力而为的停机通知；关闭是权威，统一走退避重连。
			sharedops.LogEvent("stele", "info", "gateway.go_away",
				fmt.Sprintf("reason=%d", message.GoAway.GetReason()))
			return gatewayReconnectBackoff, nil
		case *runtimev1.SteleEnvelope_HelloAck, *runtimev1.SteleEnvelope_Heartbeat:
			// 心跳只更新瞬时投影；重复握手裁决无需处理。
		default:
			return gatewayReconnectBackoff, fmt.Errorf("unexpected gateway frame %T", message)
		}
	}
}

// executeCall 执行一次平台调用并产出结果信封。这里是网关判定的全集：
// 材料不可用 → CREDENTIAL_UNAVAILABLE；配额超限 → RATE_LIMITED；网络
// 不通/超时 → UNREACHABLE；内部错误 → INTERNAL；拿到 HTTP 响应（任何状态）
// → SUCCEEDED。
func (gateway *Gateway) executeCall(ctx context.Context, call *runtimev1.ExecutePlatformCall) *runtimev1.ExecutePlatformCallResult {
	started := time.Now()
	result := &runtimev1.ExecutePlatformCallResult{CallId: call.GetCallId()}
	defer func() {
		result.LatencyMs = uint64(time.Since(started).Milliseconds())
		gateway.metrics.RecordGatewayCall(int32(result.GetStatus()), float64(result.GetLatencyMs()))
	}()

	// 配额先行：限流是纯内存判定，挡在可能触发 Acquire 的材料解析之前。
	if !gateway.allowCall(call.GetConnectionId(), time.Now()) {
		result.Status = runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_RATE_LIMITED
		result.ErrorCode = "rate_limited"
		result.ErrorDetail = "per-connection rate limit exceeded"
		return result
	}
	material, err := gateway.materialFor(ctx, call.GetConnectionId(), call.GetConnectionRevisionId())
	if err != nil {
		result.Status = runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_CREDENTIAL_UNAVAILABLE
		result.ErrorCode = "credential_unavailable"
		result.ErrorDetail = err.Error()
		return result
	}

	request, cancelRequest, err := buildPlatformRequest(ctx, material, call)
	if err != nil {
		result.Status = runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_INTERNAL
		result.ErrorCode = "invalid_request"
		result.ErrorDetail = err.Error()
		return result
	}
	defer cancelRequest()
	response, err := material.client.Do(request)
	if err != nil {
		// 超时与网络错误同属"平台不通"：Quoin 侧按 error_detail 细分。
		result.Status = runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_UNREACHABLE
		result.ErrorCode = "network_error"
		if errors.Is(err, context.DeadlineExceeded) {
			result.ErrorCode = "timeout"
		}
		result.ErrorDetail = err.Error()
		return result
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, gatewayMaxResponseBody+1))
	if err != nil {
		result.Status = runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_UNREACHABLE
		result.ErrorCode = "read_body_failed"
		result.ErrorDetail = err.Error()
		return result
	}
	if len(body) > gatewayMaxResponseBody {
		// 截断不诚实：直接丢弃并报内部错误，让 Quoin 侧决策重试或放弃。
		result.Status = runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_INTERNAL
		result.ErrorCode = "response_too_large"
		result.ErrorDetail = "platform response exceeded 16 MiB"
		return result
	}
	result.Status = runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_SUCCEEDED
	result.HttpStatus = int32(response.StatusCode)
	result.Body = body
	return result
}

// materialFor 解析连接材料：缓存命中且 revision 一致直接复用；miss 或
// revision 漂移时重新 Acquire 并覆盖缓存。Acquire 失败不缓存（下一次调用
// 重试），避免瞬时故障固化。
func (gateway *Gateway) materialFor(ctx context.Context, connectionID, revisionID int64) (*material, error) {
	gateway.mu.Lock()
	cached, hit := gateway.materials[connectionID]
	gateway.mu.Unlock()
	if hit && cached.revisionID == revisionID {
		return cached, nil
	}
	response, err := gateway.acquire(ctx, connectionID)
	if err != nil {
		return nil, fmt.Errorf("acquire connection credential: %w", err)
	}
	material, err := parseMaterial(response)
	if err != nil {
		return nil, fmt.Errorf("parse connection %d material: %w", connectionID, err)
	}
	gateway.mu.Lock()
	gateway.materials[connectionID] = material
	gateway.mu.Unlock()
	return material, nil
}

// parseMaterial 把 Acquire 响应解析为可执行材料：非秘密投影决定 endpoint/
// TLS/认证方式，秘密只在 response.GetThanos()。
func parseMaterial(response *runtimev1.AcquireConnectionCredentialResponse) (*material, error) {
	var config revisionConfig
	if len(response.GetRevisionConfigJson()) == 0 {
		return nil, errors.New("empty revision config")
	}
	decoder := json.NewDecoder(bytes.NewReader(response.GetRevisionConfigJson()))
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("revision config is not valid JSON: %w", err)
	}
	if config.BaseURL == "" {
		return nil, errors.New("revision config carries no baseUrl")
	}
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("baseUrl %q is not an absolute http(s) URL", config.BaseURL)
	}
	switch config.AuthType {
	case "", "none", "basic", "bearer":
	default:
		return nil, fmt.Errorf("unsupported auth type %q", config.AuthType)
	}
	secret := response.GetThanos()
	material := &material{
		revisionID:     response.GetConnectionRevisionId(),
		connectionType: response.GetConnectionType(),
		baseURL:        config.BaseURL,
		authType:       config.AuthType,
	}
	if secret != nil {
		material.username, material.password, material.bearerToken = secret.GetUsername(), secret.GetPassword(), secret.GetBearerToken()
	}
	// basic 的用户名回退到非秘密投影里的 username（secret 只在动态凭证时携带）。
	if material.username == "" {
		material.username = config.Username
	}
	client, err := platformHTTPClient(config)
	if err != nil {
		return nil, err
	}
	material.client = client
	return material, nil
}

// platformHTTPClient 按连接 TLS 配置构建客户端（内联自 Plinth 的
// thanosHTTPClient 语义；Stele 不依赖 Plinth 包）。整体超时取上限 60s，
// 每个请求另有自身的 ctx 截止。
func platformHTTPClient(config revisionConfig) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if config.TLSSkipVerify {
		tlsConfig.InsecureSkipVerify = true
	}
	if config.TLSCaPem != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(config.TLSCaPem)) {
			return nil, errors.New("tlsCaPem cannot be parsed")
		}
		tlsConfig.RootCAs = pool
	}
	if config.TLSServerName != "" {
		tlsConfig.ServerName = config.TLSServerName
	}
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
		Timeout:   gatewayMaxTimeout,
	}, nil
}

// buildPlatformRequest 组装平台请求：endpoint 拼接、封闭方法集、GET 无体、
// 超时夹取，以及认证注入（authType 空时兼容旧数据：password 非空即 basic）。
// 返回的 cancel 归调用方所有（请求完成后释放）。
func buildPlatformRequest(ctx context.Context, material *material, call *runtimev1.ExecutePlatformCall) (*http.Request, context.CancelFunc, error) {
	method := strings.ToUpper(call.GetMethod())
	if method != http.MethodGet && method != http.MethodPost {
		return nil, nil, fmt.Errorf("method %q is outside the closed GET|POST set", call.GetMethod())
	}
	if !strings.HasPrefix(call.GetPath(), "/") {
		return nil, nil, fmt.Errorf("path %q must start with /", call.GetPath())
	}
	if strings.Contains(call.GetPath(), "?") {
		return nil, nil, errors.New("path must not carry a query string; use the query field")
	}
	if method == http.MethodGet && len(call.GetBody()) > 0 {
		return nil, nil, errors.New("GET calls must not carry a body")
	}
	timeout := time.Duration(call.GetTimeoutMs()) * time.Millisecond
	if timeout < gatewayMinTimeout {
		timeout = gatewayMinTimeout
	}
	if timeout > gatewayMaxTimeout {
		timeout = gatewayMaxTimeout
	}
	target := strings.TrimSuffix(material.baseURL, "/") + call.GetPath()
	if call.GetQuery() != "" {
		target += "?" + call.GetQuery()
	}
	if _, err := url.Parse(target); err != nil {
		return nil, nil, fmt.Errorf("resolve endpoint: %w", err)
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	var reader io.Reader
	if len(call.GetBody()) > 0 {
		reader = bytes.NewReader(call.GetBody())
	}
	request, err := http.NewRequestWithContext(execCtx, method, target, reader)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("build platform request: %w", err)
	}
	for name, value := range call.GetHeaders() {
		request.Header.Set(name, value)
	}
	if err := applyMaterialAuth(request, material); err != nil {
		cancel()
		return nil, nil, err
	}
	return request, cancel, nil
}

// applyMaterialAuth 注入认证头。与 Plinth ApplyMetricsAuth 语义一致：authType
// 空视为旧数据——password 非空即 basic，否则 none；差异是网关对多余的
// 秘密字段选择忽略而不是让调用失败（容错优先：一次调用不应因历史残留字段
// 不可用），配置层的一致性校验已在保存时完成。
func applyMaterialAuth(request *http.Request, material *material) error {
	authType := material.authType
	if authType == "" {
		authType = "none"
		if material.password != "" {
			authType = "basic"
		}
	}
	switch authType {
	case "none":
		return nil
	case "basic":
		if material.username == "" || material.password == "" {
			return fmt.Errorf("basic credentials are incomplete for connection")
		}
		request.SetBasicAuth(material.username, material.password)
	case "bearer":
		if material.bearerToken == "" {
			return errors.New("bearer token is empty for connection")
		}
		request.Header.Set("Authorization", "Bearer "+material.bearerToken)
	default:
		return fmt.Errorf("unsupported metrics auth type %q", authType)
	}
	return nil
}

// allowCall 判定配额并累计 allowed/denied。
func (gateway *Gateway) allowCall(connectionID int64, now time.Time) bool {
	gateway.mu.Lock()
	bucket, ok := gateway.buckets[connectionID]
	if !ok {
		bucket = newTokenBucket(gateway.ratePerMinute, now)
		gateway.buckets[connectionID] = bucket
	}
	counter, ok := gateway.counters[connectionID]
	if !ok {
		counter = &rateCounter{}
		gateway.counters[connectionID] = counter
	}
	gateway.mu.Unlock()
	allowed := bucket.allow(now)
	if allowed {
		counter.allowed.Add(1)
	} else {
		counter.denied.Add(1)
	}
	return allowed
}

// flushRateCounters 把内存累计增量刷入本地 rate_counters 表。
func (gateway *Gateway) flushRateCounters() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type rateDelta struct{ allowed, denied int64 }
	gateway.mu.Lock()
	pending := make(map[int64]rateDelta, len(gateway.counters))
	for connectionID, counter := range gateway.counters {
		allowed, denied := counter.allowed.Swap(0), counter.denied.Swap(0)
		if allowed == 0 && denied == 0 {
			continue
		}
		pending[connectionID] = rateDelta{allowed: allowed, denied: denied}
	}
	gateway.mu.Unlock()
	var firstErr error
	for connectionID, delta := range pending {
		if err := gateway.queue.AddRateCounters(ctx, connectionID, delta.allowed, delta.denied); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		sharedops.LogEvent("stele", "error", "gateway.rate_flush_failed", firstErr.Error())
		// 刷失败的增量记账回内存，下个周期连同新增量一起重试。
		gateway.mu.Lock()
		for connectionID, delta := range pending {
			if counter, ok := gateway.counters[connectionID]; ok {
				counter.allowed.Add(delta.allowed)
				counter.denied.Add(delta.denied)
			}
		}
		gateway.mu.Unlock()
	}
}

// sleepCtx 在 ctx 结束前等待；返回 false 表示被取消。
func sleepCtx(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// ConnectStreamWrapped adapts Relay.ConnectStream to the dialer signature.
func (relay *Relay) ConnectStreamWrapped(ctx context.Context) (envelopeStream, error) {
	return relay.ConnectStream(ctx)
}
