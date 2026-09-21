package app

// Stele 网关流服务端（ADR-0011）：出向工具执行链 Quoin -> Stele 的下发
// 半边。Plinth 的 tool_call 经 Quoin 权限/审计后，实际的平台调用通过
// 本网关以 ExecutePlatformCall 帧下发给当前 Stele 流，Stele 注入凭证、
// 限流并执行传输，ExecutePlatformCallResult 按 correlation_id 配对回程。
//
// 与 RuntimeService 的 Slots/stream 管理相比这里刻意大幅简化：协议约定
// 单 Stele 实例（runtime.proto SteleRelay 注释），因此只保留"当前一条流 +
// 等待者表"的内存投影——connected/lastHeartbeat 不落库，流断开即消失，
// 重连由 Stele 重新 Connect 建立。

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/plugins"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// executeResultGrace 是等待 ExecutePlatformCallResult 时在请求自身超时
// 之外的余量：覆盖帧在流上的往返与 Stele 收尾，避免紧贴 deadline 的合法
// 结果被误判为超时。变量形态仅为测试可注入。
var executeResultGrace = 10 * time.Second

// steleGateway 持有当前 Stele 流与其等待者表。
type steleGateway struct {
	mu sync.Mutex
	// stream 是当前登记的 Stele 流；新流到来时替换并关闭旧流（单实例
	// 假设下后到者胜——重连意味着旧流已死或即将死）。
	stream runtimev1.SteleRelay_ConnectServer
	// streamDone 在当前流摘除时关闭，供等待侧感知流已失效。
	streamDone chan struct{}
	// closing 在进程关停时关闭：Connect 的 Recv 循环据此返回，gRPC
	// GracefulStop 才能收尾（否则 SIGTERM 永久阻塞在网关上）。
	closing   chan struct{}
	closeOnce sync.Once
	// bootID 来自已接受的 SteleHello，出向帧据此保持与流上下文一致的
	// 围栏字段。
	bootID string
	// waiters 按 correlation_id 等待 ExecutePlatformCallResult。
	waiters map[uint64]chan *runtimev1.ExecutePlatformCallResult
	// calls 把已下发 call_id 折到其 correlation_id：对端回程帧丢了
	// correlation_id 时仍可按 call_id 配对（宽松回程的兜底路径）。
	calls map[string]uint64
	// messageID 是 Quoin->Stele 方向的单调帧序号（hello_ack 占 1）。
	messageID uint64
	// correlationID 是流内单调的配对键（0 保留为"无"，从 1 起）。
	correlationID uint64
	// heartbeat 记录最近一次心跳时间（仅内存投影，不落库）。
	heartbeat time.Time
}

// NewSteleGateway 构建网关（无流状态；Connect 之后才有投影）。
func NewSteleGateway() *steleGateway {
	return &steleGateway{
		closing: make(chan struct{}),
		waiters: map[uint64]chan *runtimev1.ExecutePlatformCallResult{},
		calls:   map[string]uint64{},
	}
}

// Connected 返回网关流是否处于已连接状态与最近心跳时间（仅内存投影）。
func (gateway *steleGateway) Connected() (bool, time.Time) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	return gateway.stream != nil, gateway.heartbeat
}

// Close 通知当前网关流结束（进程关停）：Connect 的 Recv 循环与 Execute
// 等待者随之返回，gRPC GracefulStop 不再无限等待网关在飞 handler。
// 幂等；与 runtime.CloseAll 同语义（connection.close 的 sync.Once 模式）。
func (gateway *steleGateway) Close() {
	gateway.closeOnce.Do(func() { close(gateway.closing) })
}

// Connect 承接 Stele 的网关流握手与收发循环。首帧必须是 SteleHello：
// contract_fingerprint 必须与 Quoin 的完整 Proto 权威契约一致，
// connection_epoch >= 1；不符回 hello_ack{accepted:false} 后关闭。接受后
// 登记当前流（替换旧流——新流到来时关闭旧流），回 hello_ack{accepted:true}。
func (gateway *steleGateway) Connect(stream runtimev1.SteleRelay_ConnectServer) error {
	ctx := stream.Context()
	select {
	case <-gateway.closing:
		return status.Error(codes.Unavailable, "quoin is shutting down")
	default:
	}
	if !requireComponentIdentity(ctx, "stele") {
		return status.Error(codes.Unauthenticated, "stele client identity required")
	}
	first, err := stream.Recv()
	if err != nil {
		return status.Error(codes.InvalidArgument, "hello frame required")
	}
	hello := first.GetHello()
	if hello == nil || first.GetMessageId() != 1 {
		return status.Error(codes.InvalidArgument, "first frame must be hello")
	}
	reject := func(detail string) error {
		_ = stream.Send(&runtimev1.SteleEnvelope{
			MessageId: 1, BootId: hello.GetBootId(), CorrelationId: first.GetCorrelationId(),
			Msg: &runtimev1.SteleEnvelope_HelloAck{HelloAck: &runtimev1.SteleHelloAck{Accepted: false, Detail: detail}},
		})
		sharedops.LogEvent("quoin", "info", "stele_gateway.hello_rejected", detail)
		return status.Error(codes.Unauthenticated, "stele gateway handshake rejected: "+detail)
	}
	if !contract.ValidProtoAuthorityFingerprint(hello.GetContractFingerprint()) || hello.GetContractFingerprint() != contract.ProtoAuthorityFingerprint {
		return reject("contract fingerprint mismatch")
	}
	if hello.GetBootId() == "" {
		return reject("boot_id is required")
	}
	if hello.GetConnectionEpoch() < 1 {
		return reject("connection_epoch must be >= 1")
	}
	// 单 Stele 实例：新流无条件替换旧流。先摘除旧流（唤醒其全部等待者），
	// 再登记新流，避免两条流同时认为自己当前。
	gateway.mu.Lock()
	if gateway.stream != nil {
		gateway.detachLocked()
	}
	done := make(chan struct{})
	gateway.stream = stream
	gateway.streamDone = done
	gateway.bootID = hello.GetBootId()
	gateway.heartbeat = time.Now().UTC()
	gateway.messageID = 1 // hello_ack 占用序号 1
	gateway.correlationID = 0
	gateway.mu.Unlock()
	defer func() {
		gateway.mu.Lock()
		defer gateway.mu.Unlock()
		if gateway.streamDone == done {
			gateway.detachLocked()
		}
	}()
	sharedops.LogEvent("quoin", "info", "stele_gateway.connected",
		fmt.Sprintf("boot=%s epoch=%d release=%s", hello.GetBootId(), hello.GetConnectionEpoch(), hello.GetReleaseVersion()))
	if err := stream.Send(&runtimev1.SteleEnvelope{
		MessageId: gateway.allocateMessageID(), BootId: hello.GetBootId(),
		Msg: &runtimev1.SteleEnvelope_HelloAck{HelloAck: &runtimev1.SteleHelloAck{Accepted: true}},
	}); err != nil {
		return err
	}
	for {
		var envelope *runtimev1.SteleEnvelope
		// 关停信号必须在 Recv 阻塞期间也能结束 RPC：与 runtime 控制流同一
		// 模式（runtime_service.go 的 closing race），gRPC 两端同时拆流，
		// GracefulStop 不再被网关在飞 handler 永久挂住。
		received := make(chan error, 1)
		go func() {
			frame, recvErr := stream.Recv()
			envelope = frame
			received <- recvErr
		}()
		var err error
		select {
		case <-done:
			return nil
		case <-gateway.closing:
			return status.Error(codes.Canceled, "quoin is shutting down")
		case err = <-received:
			if err != nil {
				// 流结束（Stele 停机/替换）：deferred detach 摘除流并唤醒
				// 全部等待者。
				return nil
			}
		}
		switch payload := envelope.GetMsg().(type) {
		case *runtimev1.SteleEnvelope_Heartbeat:
			gateway.mu.Lock()
			gateway.heartbeat = time.Now().UTC()
			gateway.mu.Unlock()
		case *runtimev1.SteleEnvelope_ExecuteResult:
			gateway.deliverResult(envelope.GetCorrelationId(), payload.ExecuteResult)
		case *runtimev1.SteleEnvelope_Hello, *runtimev1.SteleEnvelope_HelloAck:
			// 握手帧只允许出现在首帧位置；流内重复握手按协议错误处理。
			return status.Error(codes.InvalidArgument, "hello frame is only valid as the first frame")
		default:
			// 其余帧（含对端 go_away 的回程方向不合法）宽松忽略：网关
			// 协议的方向约束以 SteleEnvelope 注释为权威。
			sharedops.LogEvent("quoin", "info", "stele_gateway.envelope_ignored", "unexpected frame on gateway stream")
		}
	}
}

// allocateMessageID 以线程安全方式分配出向帧序号。
func (gateway *steleGateway) allocateMessageID() uint64 {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	return gateway.nextMessageIDLocked()
}

func (gateway *steleGateway) nextMessageIDLocked() uint64 {
	gateway.messageID++
	return gateway.messageID
}

// deliverResult 按回程帧唤醒等待者：优先用 envelope.correlation_id，缺失
// 或无人等待时按 result.call_id 兜底（Stele 侧对回程帧校验宽松）。无人
// 等待的结果（超时后迟到）被丢弃——出向调用以幂等读为主，迟到结果不再
// 有消费者。
func (gateway *steleGateway) deliverResult(correlation uint64, result *runtimev1.ExecutePlatformCallResult) {
	if result == nil {
		return
	}
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if correlation == 0 {
		correlation = gateway.calls[result.GetCallId()]
	} else {
		delete(gateway.calls, result.GetCallId())
	}
	if correlation == 0 {
		return
	}
	waiter, waiting := gateway.waiters[correlation]
	if !waiting {
		return
	}
	delete(gateway.waiters, correlation)
	// 缓冲为 1：投递永不阻塞收发循环，即使等待者已因超时离开。
	waiter <- result
}

// detach 摘除当前流并唤醒全部等待者。
func (gateway *steleGateway) detach() {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	gateway.detachLocked()
}

// detachLocked 是 detach 的持锁实现：清空流投影、关闭 streamDone、以 nil
// 结果唤醒全部等待者（等待者将 nil 视为流断开 -> ErrPlatformUnreachable）。
func (gateway *steleGateway) detachLocked() {
	if gateway.streamDone != nil {
		close(gateway.streamDone)
	}
	gateway.streamDone = nil
	gateway.stream = nil
	gateway.bootID = ""
	gateway.calls = map[string]uint64{}
	for correlation, waiter := range gateway.waiters {
		delete(gateway.waiters, correlation)
		// 缓冲为 1，投递不阻塞；等待者可能已超时离开，满仓即放弃。
		select {
		case waiter <- nil:
		default:
		}
	}
}

// Execute 是请求方入口：经当前 Stele 流下发一次平台调用并等待配对结果。
// 流未连接直接以 ErrPlatformUnreachable 失败。结果状态映射：
// SUCCEEDED -> PlatformResponse（含平台自身 4xx/5xx，语义由 Handler 解释），
// RATE_LIMITED -> wrap ErrPlatformRateLimited，
// CREDENTIAL_UNAVAILABLE -> wrap ErrCredentialUnavailable，
// UNREACHABLE/INTERNAL/超时/流断开 -> wrap ErrPlatformUnreachable。
// RateLimit 不在帧字段中（Stele 侧按 per-entry 默认执行），此处不传。
func (gateway *steleGateway) Execute(ctx context.Context, connectionID, revisionID int64, req plugins.PlatformRequest) (*plugins.PlatformResponse, error) {
	callID, err := newGatewayCallID()
	if err != nil {
		return nil, fmt.Errorf("%w: allocate call id: %v", plugins.ErrPlatformUnreachable, err)
	}
	gateway.mu.Lock()
	stream := gateway.stream
	if stream == nil {
		gateway.mu.Unlock()
		return nil, fmt.Errorf("%w: stele gateway not connected", plugins.ErrPlatformUnreachable)
	}
	done := gateway.streamDone
	bootID := gateway.bootID
	gateway.correlationID++
	correlation := gateway.correlationID
	waiter := make(chan *runtimev1.ExecutePlatformCallResult, 1)
	gateway.waiters[correlation] = waiter
	gateway.calls[callID] = correlation
	envelope := &runtimev1.SteleEnvelope{
		MessageId: gateway.nextMessageIDLocked(), CorrelationId: correlation, BootId: bootID,
		Msg: &runtimev1.SteleEnvelope_Execute{Execute: &runtimev1.ExecutePlatformCall{
			CallId: callID, ConnectionId: connectionID, ConnectionRevisionId: revisionID,
			Method: req.Method, Path: req.Path, Query: req.Query.Encode(),
			Headers: headerMapOf(req.Header), Body: req.Body, TimeoutMs: timeoutMillisOf(req.Timeout),
		}},
	}
	gateway.mu.Unlock()
	deleteWaiter := func() {
		gateway.mu.Lock()
		delete(gateway.waiters, correlation)
		delete(gateway.calls, callID)
		gateway.mu.Unlock()
	}
	if err := stream.Send(envelope); err != nil {
		deleteWaiter()
		return nil, fmt.Errorf("%w: stele gateway send failed: %v", plugins.ErrPlatformUnreachable, err)
	}
	// 等待窗口 = max(请求超时, 最小执行窗) + 余量；ctx 仍是最终上限。
	waitFor := executeResultGrace
	normalizedTimeout := time.Duration(timeoutMillisOf(req.Timeout)) * time.Millisecond
	if normalizedTimeout+executeResultGrace > waitFor {
		waitFor = normalizedTimeout + executeResultGrace
	}
	timer := time.NewTimer(waitFor)
	defer timer.Stop()
	select {
	case result := <-waiter:
		if result == nil {
			return nil, fmt.Errorf("%w: stele gateway stream ended before the result arrived", plugins.ErrPlatformUnreachable)
		}
		return gatewayResponseOf(result)
	case <-timer.C:
		deleteWaiter()
		return nil, fmt.Errorf("%w: stele gateway result timed out for call %s", plugins.ErrPlatformUnreachable, callID)
	case <-done:
		deleteWaiter()
		return nil, fmt.Errorf("%w: stele gateway stream disconnected", plugins.ErrPlatformUnreachable)
	case <-ctx.Done():
		deleteWaiter()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: platform call deadline exceeded: %v", plugins.ErrPlatformUnreachable, ctx.Err())
		}
		return nil, fmt.Errorf("%w: platform call cancelled: %v", plugins.ErrPlatformUnreachable, ctx.Err())
	}
}

// gatewayResponseOf 把 Stele 的执行结果状态映射到插件契约。
func gatewayResponseOf(result *runtimev1.ExecutePlatformCallResult) (*plugins.PlatformResponse, error) {
	switch result.GetStatus() {
	case runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_SUCCEEDED:
		return &plugins.PlatformResponse{StatusCode: int(result.GetHttpStatus()), Body: result.GetBody()}, nil
	case runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_RATE_LIMITED:
		return nil, fmt.Errorf("%w: %s %s", plugins.ErrPlatformRateLimited, result.GetErrorCode(), result.GetErrorDetail())
	case runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_CREDENTIAL_UNAVAILABLE:
		return nil, fmt.Errorf("%w: %s %s", plugins.ErrCredentialUnavailable, result.GetErrorCode(), result.GetErrorDetail())
	default:
		// UNREACHABLE / INTERNAL / UNSPECIFIED 一律按不可达类失败。
		return nil, fmt.Errorf("%w: %s %s", plugins.ErrPlatformUnreachable, result.GetErrorCode(), result.GetErrorDetail())
	}
}

// newGatewayCallID 生成 16 字节随机数的 base64url 编码（call_id 语义上仅是
// 流内配对标识，不承诺全局唯一）。
func newGatewayCallID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// headerMapOf 把附加非凭证头转为帧字段（nil 安全，多值头取首值——出向
// 工具词表内的头都是单值语义）。
func headerMapOf(header map[string][]string) map[string]string {
	if len(header) == 0 {
		return nil
	}
	out := make(map[string]string, len(header))
	for name, values := range header {
		if len(values) == 0 {
			continue
		}
		out[name] = values[0]
	}
	return out
}

// gatewayDefaultTimeout 是调用方未声明超时时下发的部署默认值。帧契约
// （runtime.proto ExecutePlatformCall.timeout_ms）要求 >0；此前非正值被
// 序列化为 0，而 Stele 侧没有"默认值"概念，0 会被通用下限夹到 1s——新
// 插件一旦忘记设 Timeout，大查询将系统性地以 1s 超时失败。
const gatewayDefaultTimeout = 30 * time.Second

// timeoutMillisOf 把请求超时序列化为帧字段（契约要求 >0）：非正值收敛到
// 部署默认，而不是把契约禁止的 0 放上 wire。
func timeoutMillisOf(timeout time.Duration) uint32 {
	if timeout <= 0 {
		timeout = gatewayDefaultTimeout
	}
	millis := timeout.Milliseconds()
	if millis < 1 {
		millis = 1
	}
	if millis > 1<<31-1 {
		millis = 1<<31 - 1
	}
	return uint32(millis)
}
