package app

// steleGateway 单元测试：握手裁决、心跳投影、流替换、Execute 的结果映射
// 与流断开唤醒（内存流 stub，不经真实 gRPC）。

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/plugins"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// fakeGatewayStream 是内存双端流的服务端 stub：Send 经回调注入（默认只记
// 帧），Recv 按 inbound 通道出帧、ctx 结束后收尾。
type fakeGatewayStream struct {
	ctx      context.Context
	cancel   context.CancelFunc
	sendHook func(*runtimev1.SteleEnvelope) error
	sendLock sync.Mutex
	sent     []*runtimev1.SteleEnvelope
	inbound  chan *runtimev1.SteleEnvelope
}

func newFakeGatewayStream(ctx context.Context) *fakeGatewayStream {
	stream := &fakeGatewayStream{inbound: make(chan *runtimev1.SteleEnvelope, 8)}
	stream.ctx, stream.cancel = context.WithCancel(ctx)
	return stream
}

func (s *fakeGatewayStream) Send(envelope *runtimev1.SteleEnvelope) error {
	s.sendLock.Lock()
	hook := s.sendHook
	s.sent = append(s.sent, envelope)
	s.sendLock.Unlock()
	if hook != nil {
		return hook(envelope)
	}
	return nil
}

func (s *fakeGatewayStream) Recv() (*runtimev1.SteleEnvelope, error) {
	select {
	case envelope := <-s.inbound:
		return envelope, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

func (s *fakeGatewayStream) Context() context.Context { return s.ctx }

func (s *fakeGatewayStream) SetHeader(metadata.MD) error  { return nil }
func (s *fakeGatewayStream) SendHeader(metadata.MD) error { return nil }
func (s *fakeGatewayStream) SetTrailer(metadata.MD)       {}
func (s *fakeGatewayStream) SendMsg(any) error            { return nil }
func (s *fakeGatewayStream) RecvMsg(any) error            { return nil }

func (s *fakeGatewayStream) sentFrames() []*runtimev1.SteleEnvelope {
	s.sendLock.Lock()
	defer s.sendLock.Unlock()
	frames := make([]*runtimev1.SteleEnvelope, 0, len(s.sent))
	frames = append(frames, s.sent...)
	return frames
}

// steleIdentityContext 构造带 CN=stele 客户端身份的 gRPC peer 上下文
// （等价于真实 mTLS 握手后的组件身份投影）。
func steleIdentityContext(t *testing.T) context.Context {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "stele"},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}},
	})
}

func gatewayHelloEnvelope(epoch uint64, fingerprint string) *runtimev1.SteleEnvelope {
	return &runtimev1.SteleEnvelope{
		MessageId: 1, BootId: "stele-boot-1",
		Msg: &runtimev1.SteleEnvelope_Hello{Hello: &runtimev1.SteleHello{
			BootId: "stele-boot-1", ConnectionEpoch: epoch,
			ContractFingerprint: fingerprint, ReleaseVersion: "test",
		}},
	}
}

// runGatewayConnect 在带 stele 身份的流上启动 Connect 循环。
func runGatewayConnect(t *testing.T, gateway *steleGateway) *fakeGatewayStream {
	t.Helper()
	stream := newFakeGatewayStream(steleIdentityContext(t))
	done := make(chan error, 1)
	go func() { done <- gateway.Connect(stream) }()
	t.Cleanup(func() {
		stream.cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("gateway Connect did not return after stream cancellation")
		}
	})
	return stream
}

// awaitHelloAck 轮询等待首帧 hello_ack。
func awaitHelloAck(t *testing.T, stream *fakeGatewayStream) *runtimev1.SteleHelloAck {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if frames := stream.sentFrames(); len(frames) > 0 {
			ack := frames[0].GetHelloAck()
			if ack == nil {
				t.Fatalf("first outbound frame is not hello_ack: %+v", frames[0].GetHello())
			}
			return ack
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("hello_ack not sent")
	return nil
}

func TestSteleGatewayConnectHandshakeAcceptsAndTracksHeartbeat(t *testing.T) {
	gateway := NewSteleGateway()
	stream := runGatewayConnect(t, gateway)
	stream.inbound <- gatewayHelloEnvelope(1, contract.ProtoAuthorityFingerprint)
	if ack := awaitHelloAck(t, stream); !ack.GetAccepted() {
		t.Fatalf("hello_ack=%+v, want accepted", ack)
	}
	stream.inbound <- &runtimev1.SteleEnvelope{
		MessageId: 2, BootId: "stele-boot-1",
		Msg: &runtimev1.SteleEnvelope_Heartbeat{Heartbeat: &runtimev1.SteleHeartbeat{Seq: 1}},
	}
	connected, heartbeat := gateway.Connected()
	if !connected {
		t.Fatal("gateway must project connected after accepted hello")
	}
	if time.Since(heartbeat) > time.Minute {
		t.Fatalf("heartbeat projection stale: %v", heartbeat)
	}
}

func TestSteleGatewayConnectRejectsFingerprintMismatch(t *testing.T) {
	gateway := NewSteleGateway()
	stream := runGatewayConnect(t, gateway)
	stream.inbound <- gatewayHelloEnvelope(1, "not-a-valid-fingerprint")
	if ack := awaitHelloAck(t, stream); ack.GetAccepted() {
		t.Fatalf("hello_ack=%+v, want rejected", ack)
	}
	if connected, _ := gateway.Connected(); connected {
		t.Fatal("rejected handshake must not register the stream")
	}
}

func TestSteleGatewayConnectRejectsZeroEpoch(t *testing.T) {
	gateway := NewSteleGateway()
	stream := runGatewayConnect(t, gateway)
	stream.inbound <- gatewayHelloEnvelope(0, contract.ProtoAuthorityFingerprint)
	if ack := awaitHelloAck(t, stream); ack.GetAccepted() {
		t.Fatalf("hello_ack=%+v, want rejected for connection_epoch=0", ack)
	}
}

func TestSteleGatewayNewStreamReplacesOld(t *testing.T) {
	gateway := NewSteleGateway()
	first := runGatewayConnect(t, gateway)
	first.inbound <- gatewayHelloEnvelope(1, contract.ProtoAuthorityFingerprint)
	awaitHelloAck(t, first)
	gateway.mu.Lock()
	firstDone := gateway.streamDone
	gateway.mu.Unlock()
	// 第二条流到来：第一条被替换（streamDone 关闭）。
	second := runGatewayConnect(t, gateway)
	second.inbound <- gatewayHelloEnvelope(2, contract.ProtoAuthorityFingerprint)
	awaitHelloAck(t, second)
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("old stream was not replaced by the new connection")
	}
	if connected, _ := gateway.Connected(); !connected {
		t.Fatal("new stream must keep the gateway connected")
	}
}

// attachFakeStream 以测试方式直接登记一条流（绕过握手），供 Execute 映射
// 用例使用；返回流结束通道。
func attachFakeStream(gateway *steleGateway, stream *fakeGatewayStream) <-chan struct{} {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	gateway.stream = stream
	done := make(chan struct{})
	gateway.streamDone = done
	gateway.bootID = "stele-boot-1"
	gateway.heartbeat = time.Now().UTC()
	gateway.messageID = 1
	return done
}

func TestSteleGatewayExecuteNotConnected(t *testing.T) {
	gateway := NewSteleGateway()
	_, err := gateway.Execute(context.Background(), 7, 8, plugins.PlatformRequest{Method: "GET", Path: "/api/v1/query"})
	if !errors.Is(err, plugins.ErrPlatformUnreachable) {
		t.Fatalf("err=%v, want ErrPlatformUnreachable", err)
	}
}

// TestSteleGatewayExecuteMapsResultStatuses 逐状态验证结果映射：Send 同步
// 投递对应结果帧。
func TestSteleGatewayExecuteMapsResultStatuses(t *testing.T) {
	cases := []struct {
		name    string
		status  runtimev1.PlatformCallStatus
		want    error
		wantErr bool
	}{
		{name: "succeeded", status: runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_SUCCEEDED},
		{name: "rate_limited", status: runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_RATE_LIMITED, want: plugins.ErrPlatformRateLimited, wantErr: true},
		{name: "credential_unavailable", status: runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_CREDENTIAL_UNAVAILABLE, want: plugins.ErrCredentialUnavailable, wantErr: true},
		{name: "unreachable", status: runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_UNREACHABLE, want: plugins.ErrPlatformUnreachable, wantErr: true},
		{name: "internal", status: runtimev1.PlatformCallStatus_PLATFORM_CALL_STATUS_INTERNAL, want: plugins.ErrPlatformUnreachable, wantErr: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			gateway := NewSteleGateway()
			stream := newFakeGatewayStream(context.Background())
			stream.sendHook = func(envelope *runtimev1.SteleEnvelope) error {
				go gateway.deliverResult(envelope.GetCorrelationId(), &runtimev1.ExecutePlatformCallResult{
					CallId: envelope.GetExecute().GetCallId(), Status: testCase.status, HttpStatus: 200, Body: []byte(`{}`),
				})
				return nil
			}
			attachFakeStream(gateway, stream)
			response, err := gateway.Execute(context.Background(), 7, 8, plugins.PlatformRequest{
				Method: "GET", Path: "/api/v1/query", Query: url.Values{"query": {"up()"}}, Timeout: 5 * time.Second,
			})
			if testCase.wantErr {
				if !errors.Is(err, testCase.want) {
					t.Fatalf("err=%v, want %v", err, testCase.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != 200 || string(response.Body) != "{}" {
				t.Fatalf("response=%+v", response)
			}
		})
	}
}

func TestSteleGatewayExecuteTimeout(t *testing.T) {
	original := executeResultGrace
	executeResultGrace = 50 * time.Millisecond
	defer func() { executeResultGrace = original }()
	gateway := NewSteleGateway()
	stream := newFakeGatewayStream(context.Background()) // 无回程
	attachFakeStream(gateway, stream)
	_, err := gateway.Execute(context.Background(), 7, 8, plugins.PlatformRequest{Method: "GET", Path: "/x", Timeout: 10 * time.Millisecond})
	if !errors.Is(err, plugins.ErrPlatformUnreachable) {
		t.Fatalf("err=%v, want ErrPlatformUnreachable", err)
	}
}

func TestSteleGatewayStreamDetachWakesWaiters(t *testing.T) {
	gateway := NewSteleGateway()
	stream := newFakeGatewayStream(context.Background())
	attachFakeStream(gateway, stream)
	result := make(chan error, 1)
	go func() {
		_, err := gateway.Execute(context.Background(), 7, 8, plugins.PlatformRequest{Method: "GET", Path: "/x", Timeout: 30 * time.Second})
		result <- err
	}()
	// 等待下发帧出现后断开流（detach 关闭 streamDone 并唤醒等待者）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if frames := stream.sentFrames(); len(frames) > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	gateway.detach()
	select {
	case err := <-result:
		if !errors.Is(err, plugins.ErrPlatformUnreachable) {
			t.Fatalf("err=%v, want ErrPlatformUnreachable after stream detach", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter was not woken by stream detach")
	}
}
