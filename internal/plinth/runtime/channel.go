// Package runtime implements Plinth's outbound Runtime channel (T06): the
// mTLS-authenticated outbound Connect control loop with Hello handshake and
// heartbeats. Component identity is the deployment CA-signed client
// certificate (ADR-0009); there is no registration or long-term token state.
// Readiness stays strict: the ops endpoint flips to ready only after a
// Quoin-accepted handshake.
package runtime

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
	"github.com/Suknna/quoin/internal/contract"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

// bootFile identifies this process boot on the state volume.
type bootFile struct {
	BootID string `json:"bootId"`
}

type Channel struct {
	Config ChannelConfig
	bootID string
	epoch  uint64 // per-boot connection counter (RUNTIME-CTRL-004)
	// Tasks executes dispatched inference attempts when wired (T10;
	// ADR-0011 之后 Plinth 只承载推理 attempt)。
	Tasks TaskSupervisor
	// Artifacts is the ArtifactService client on the live connection
	// (T10: tool_result uploads and attempt-scoped reads).
	Artifacts runtimev1.ArtifactServiceClient
	// live-stream send state; guarded by outboundMu.
	outboundMu  sync.Mutex
	outboundSeq uint64
	sendStream  interface {
		Send(*runtimev1.ControlEnvelope) error
	}
	cancelMu sync.Mutex
	// active survives control-stream reconnects within this boot: the
	// task goroutines keep running while the stream re-establishes, so the
	// registry must not be wiped per connection (T12, RUNTIME-TASK-005).
	active map[int64]*activeTask
	// cancelled holds per-boot cancellation tombstones. A duplicate dispatch
	// arriving after CancelAttempt must never revive a worker that Quoin already
	// observed as cancelled (RUNTIME-CTRL-008 / RUNTIME-CANCEL-003).
	cancelled map[int64]struct{}
	// pendingMu guards the reliable terminal-result registry (T12,
	// RUNTIME-TASK-008): every terminal ResultProposal is retried until a
	// ResultAck survives the stream it travelled on.
	pendingMu sync.Mutex
	pending   map[int64]*pendingResult
	replyMu   sync.Mutex
	nextCorr  uint64
	waiters   map[uint64]chan *runtimev1.ControlEnvelope
	// externalMu guards the QUOIN_ROUTED tool-result waiters (ADR-0011):
	// supervisor 按 tool_call_id 等待 Quoin 推送的 ExternalToolResult 帧。
	// waiter 挂在 Channel(而非单条流)上,所以同 boot 重连后 Quoin 的
	// 重发仍能送达;没有 waiter 的迟到结果只审计丢弃(Quoin 已自行封存,
	// Plinth 不是裁决方)。
	externalMu      sync.Mutex
	externalWaiters map[int64]chan *runtimev1.ExternalToolResult
}

// resultDeliveryInterval is the fixed retry cadence for outstanding terminal
// results (RUNTIME-SCOPE-004: frozen release-internal constant).
const resultDeliveryInterval = 3 * time.Second

// pendingResult is one terminal result awaiting a durable ResultAck.
type activeTask struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type pendingResult struct {
	proposal *runtimev1.ResultProposal
	ack      chan *runtimev1.ResultAck
}

// DispatchBinding is the frozen (boot, epoch) identity of one dispatched
// attempt: terminal proposals carry this binding, never the live stream's
// current epoch (Quoin adjudicates against the frozen row binding,
// RUNTIME-TASK-008).
type DispatchBinding struct {
	BootID string
	Epoch  uint64
}

type ChannelConfig struct {
	Slot               string // "plinth"
	QuoinEndpoint      string
	QuoinRuntimeCAFile string
	// QuoinRuntimeClientCertificateFile / QuoinRuntimeClientPrivateKeyFile are
	// the deployment CA-signed client identity (CN=plinth) presented during
	// the mTLS handshake (ADR-0009).
	QuoinRuntimeClientCertificateFile string
	QuoinRuntimeClientPrivateKeyFile  string
	StateDirectory                    string
	// ExternalResultTimeout bounds a worker's wait for one QUOIN_ROUTED tool
	// result (ADR-0011). Quoin's own routed execution caps at 60s and the
	// reconnect reconcile replays sealed results on attach, so a result that
	// still has not arrived after this window is treated as lost; zero selects
	// defaultExternalResultTimeout.
	ExternalResultTimeout time.Duration
	// CatalogDigest/catalogVersion stay empty for plinth (RUNTIME-CTRL-010).
}

// defaultExternalResultTimeout 是 QUOIN_ROUTED 结果等待的默认上限：Quoin 侧
// 执行上限 60s，留出一次重连对账补发的余量。
const defaultExternalResultTimeout = 90 * time.Second

// externalResultTimeout 返回生效的等待上限。
func (channel *Channel) externalResultTimeout() time.Duration {
	if channel.Config.ExternalResultTimeout > 0 {
		return channel.Config.ExternalResultTimeout
	}
	return defaultExternalResultTimeout
}

func NewChannel(config ChannelConfig) (*Channel, error) {
	bootRaw := make([]byte, 16)
	if _, err := rand.Read(bootRaw); err != nil {
		return nil, err
	}
	return &Channel{
		Config: config, bootID: base64.RawURLEncoding.EncodeToString(bootRaw),
		active: map[int64]*activeTask{}, pending: map[int64]*pendingResult{},
	}, nil
}

func (channel *Channel) bootPath() string {
	return filepath.Join(channel.Config.StateDirectory, "runtime-boot.json")
}

// RunConnect keeps the outbound control stream alive: mTLS-authenticated dial,
// Hello handshake, then heartbeats; rejected handshakes flip readiness to
// dependency-unavailable and the loop retries with backoff. Task frames
// arrive with later tickets.
func (channel *Channel) RunConnect(ctx context.Context, readiness *sharedops.Server) error {
	connection, err := channel.dial(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	client := runtimev1.NewRuntimeControlClient(connection)
	stream, err := client.Connect(ctx)
	if err != nil {
		return err
	}
	channel.Artifacts = runtimev1.NewArtifactServiceClient(connection)
	channel.epoch++
	// Frames from the prior stream carry its epoch and are rejected before task
	// dispatch. The next attach replays durable Cancelling fences, so these
	// transport-local pre-dispatch tombstones need not grow across a long boot.
	channel.cancelMu.Lock()
	channel.cancelled = make(map[int64]struct{})
	channel.cancelMu.Unlock()
	hello := &runtimev1.Hello{
		Slot:                runtimev1.RuntimeSlot_RUNTIME_SLOT_PLINTH,
		BootId:              channel.bootID,
		ConnectionEpoch:     channel.epoch,
		ContractFingerprint: contract.ProtoAuthorityFingerprint,
		ReleaseVersion:      buildinfo.Release,
	}
	channel.outboundMu.Lock()
	channel.sendStream = stream
	channel.outboundMu.Unlock()
	if err := stream.Send(&runtimev1.ControlEnvelope{MessageId: 1, ConnectionEpoch: channel.epoch, BootId: channel.bootID, Msg: &runtimev1.ControlEnvelope_Hello{Hello: hello}}); err != nil {
		return err
	}
	ack, err := stream.Recv()
	if err != nil {
		return err
	}
	helloAck := ack.GetHelloAck()
	if helloAck == nil || !helloAck.GetAccepted() {
		if readiness != nil {
			readiness.SetReadiness(sharedops.Readiness{Component: channel.Config.Slot, Release: buildinfo.Release, Mode: "normal", AcceptingWork: false, Reason: sharedops.DependencyUnavailable})
		}
		return fmt.Errorf("握手被拒绝: %s", helloAck.GetRejectReason())
	}
	if readiness != nil {
		readiness.SetReadiness(sharedops.Readiness{Component: channel.Config.Slot, Release: buildinfo.Release, Mode: "normal", AcceptingWork: true, Reason: sharedops.Ready})
	}
	sharedops.LogEvent("plinth", "info", "runtime.connected", "quoin="+channel.Config.QuoinEndpoint)
	// All outbound frames (heartbeats, task replies) share one serialized
	// sender and one per-direction message-id sequence (RUNTIME-CTRL-009).
	channel.outboundMu.Lock()
	channel.outboundSeq = 1 // Hello consumed id 1
	channel.outboundMu.Unlock()
	// The heartbeat goroutine belongs to THIS connection: sendEnvelope always
	// targets the current sendStream, so without a per-connection stop the
	// goroutine of a replaced stream keeps ticking onto its successor and
	// every successful reconnect leaks one more sender.
	heartbeatStop := make(chan struct{})
	defer close(heartbeatStop)
	go channel.runHeartbeats(ctx, heartbeatStop)
	sink := &FrameSink{channel: channel}
	for {
		envelope, err := stream.Recv()
		if err != nil {
			if readiness != nil {
				readiness.SetReadiness(sharedops.Readiness{Component: channel.Config.Slot, Release: buildinfo.Release, Mode: "normal", AcceptingWork: false, Reason: sharedops.DependencyUnavailable})
			}
			return fmt.Errorf("控制流结束: %w", err)
		}
		channel.dispatchServerFrame(ctx, sink, client, envelope)
	}
}

// runHeartbeats 发送周期性心跳直到所属连接结束（stop 关闭）、进程上下文
// 结束或发送失败。连接级 stop 是该 goroutine 的主要退出信号：sendEnvelope
// 始终指向当前 sendStream，没有它旧连接的 goroutine 会把心跳打到继任流上，
// 每次成功重连净泄漏一个发送者。
func (channel *Channel) runHeartbeats(ctx context.Context, stop <-chan struct{}) {
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	seq := uint64(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-heartbeat.C:
			seq++
			if err := channel.sendEnvelope(&runtimev1.ControlEnvelope{
				ConnectionEpoch: channel.epoch, BootId: channel.bootID,
				Msg: &runtimev1.ControlEnvelope_Heartbeat{Heartbeat: &runtimev1.Heartbeat{Seq: seq}},
			}); err != nil {
				return
			}
		}
	}
}

// dispatchServerFrame adjudicates one inbound control-stream frame
// (RUNTIME-CTRL-008 dedup, RUNTIME-TASK-005 reconcile, RUNTIME-TASK-008
// ack completion). Extracted from the receive loop so the interleavings
// are unit-testable without a live gRPC stream.
func (channel *Channel) dispatchServerFrame(ctx context.Context, sink *FrameSink, client runtimev1.RuntimeControlClient, envelope *runtimev1.ControlEnvelope) {
	switch payload := envelope.Msg.(type) {
	case *runtimev1.ControlEnvelope_DispatchAttempt:
		if channel.Tasks != nil {
			task := payload.DispatchAttempt
			// Physical dispatch dedup (RUNTIME-CTRL-008): an attempt this
			// boot already executes re-acks its accept; an attempt whose
			// terminal result still awaits its ack re-delivers that result;
			// neither ever spawns a second worker.
			// A terminal proposal can be pending while its worker goroutine has
			// not unwound yet. Result replay has priority over AttemptAccept: after
			// an Ack-loss retry Quoin needs the exact terminal proposal, not a
			// misleading confirmation that the attempt remains active.
			if channel.HasPendingResult(task.GetAttemptId()) {
				channel.DeliverPendingResults()
				return
			}
			if channel.TaskCancelled(task.GetAttemptId()) {
				channel.waitForTask(task.GetAttemptId())
				_ = sink.Send(&runtimev1.ControlEnvelope{
					CorrelationId: uint64(task.GetAttemptId()),
					Msg:           &runtimev1.ControlEnvelope_CancelAck{CancelAck: &runtimev1.CancelAck{AttemptId: task.GetAttemptId()}},
				})
				return
			}
			if channel.TaskActive(task.GetAttemptId()) {
				_ = sink.Send(&runtimev1.ControlEnvelope{
					CorrelationId: uint64(task.GetAttemptId()),
					Msg:           &runtimev1.ControlEnvelope_AttemptAccept{AttemptAccept: &runtimev1.AttemptAccept{AttemptId: task.GetAttemptId()}},
				})
				return
			}
			binding := DispatchBinding{BootID: envelope.GetBootId(), Epoch: envelope.GetConnectionEpoch()}
			// Register a cancellation fence before scheduling supervisor work. A
			// following CancelAttempt must be able to cancel this parent even when
			// the dispatch goroutine has not yet registered its child task.
			taskCtx, taskCancel := context.WithCancel(ctx)
			channel.RegisterTask(task.GetAttemptId(), taskCancel)
			go func() {
				channel.Tasks.HandleDispatchAttempt(taskCtx, sink, client, task, binding, channel.stopTask)
				channel.FinishTask(task.GetAttemptId())
			}()
		}
	case *runtimev1.ControlEnvelope_CancelAttempt:
		channel.cancelAndWait(payload.CancelAttempt.GetAttemptId())
		_ = sink.Send(&runtimev1.ControlEnvelope{
			CorrelationId: uint64(payload.CancelAttempt.GetAttemptId()),
			Msg:           &runtimev1.ControlEnvelope_CancelAck{CancelAck: &runtimev1.CancelAck{AttemptId: payload.CancelAttempt.GetAttemptId()}},
		})
	case *runtimev1.ControlEnvelope_ReconcileRequest:
		// Same-boot reconnect reconciliation (RUNTIME-TASK-005): pending
		// terminal results are flushed BEFORE the report so Quoin never
		// observes an attempt as lost while its un-acked result is still
		// in flight (deterministic wire ordering).
		channel.DeliverPendingResults()
		_ = sink.Send(&runtimev1.ControlEnvelope{
			CorrelationId: envelope.GetCorrelationId(),
			Msg:           &runtimev1.ControlEnvelope_ReconcileReport{ReconcileReport: &runtimev1.ReconcileReport{RunningAttemptIds: channel.ActiveAttempts()}},
		})
	case *runtimev1.ControlEnvelope_ResultAck:
		ack := payload.ResultAck
		sharedops.LogEvent("plinth", "info", "runtime.result_ack", fmt.Sprintf("attempt=%d accepted=%v detail=%s", ack.GetAttemptId(), ack.GetAccepted(), ack.GetDetail()))
		if !channel.completePendingResult(ack.GetAttemptId(), ack) {
			channel.deliverReply(envelope)
		}
	case *runtimev1.ControlEnvelope_BeginModelCallAck:
		channel.deliverReply(envelope)
	case *runtimev1.ControlEnvelope_CompleteModelCallAck:
		channel.deliverReply(envelope)
	case *runtimev1.ControlEnvelope_BeginToolCallAck:
		channel.deliverReply(envelope)
	case *runtimev1.ControlEnvelope_CompleteToolCallAck:
		channel.deliverReply(envelope)
	case *runtimev1.ControlEnvelope_ModelTokenDelta:
		// Transient observer deltas never reply; the ledger is the
		// authority (RUNTIME-AGENT).
	case *runtimev1.ControlEnvelope_ExternalToolResult:
		// QUOIN_ROUTED 工具的封存结果下发(ADR-0011):Quoin 已完成执行、
		// 封存(tool_calls 终态+evidence+artifact),这里只按 tool_call_id
		// 路由给等待中的 supervisor waiter,由其组装 worker 的 ToolResult。
		channel.deliverExternalToolResult(payload.ExternalToolResult)
	default:
		// Handshake-adjacent frames do not concern the
		// plinth task slice.
	}
}

// sendEnvelope serializes an outbound frame with the next message id.
func (channel *Channel) sendEnvelope(envelope *runtimev1.ControlEnvelope) error {
	channel.outboundMu.Lock()
	defer channel.outboundMu.Unlock()
	if channel.sendStream == nil {
		return errors.New("控制流发送端未就绪")
	}
	channel.outboundSeq++
	envelope.MessageId = channel.outboundSeq
	envelope.ConnectionEpoch = channel.epoch
	envelope.BootId = channel.bootID
	return channel.sendStream.Send(envelope)
}

// TaskSupervisor executes dispatched attempts (agent analysis and
// embedding). Channel owns cancellation acknowledgement so it can wait for the
// registered task's physical shutdown before replying (RUNTIME-CANCEL-003).
type TaskSupervisor interface {
	HandleDispatchAttempt(ctx context.Context, sink *FrameSink, client runtimev1.RuntimeControlClient, dispatch *runtimev1.DispatchAttempt, binding DispatchBinding, stopTask func(int64) bool)
}

// FrameSink replies on the live control stream with correct fencing.
type FrameSink struct{ channel *Channel }

// Send replies with one envelope (ids and fencing applied centrally).
func (sink *FrameSink) Send(envelope *runtimev1.ControlEnvelope) error {
	return sink.channel.sendEnvelope(envelope)
}

// Epoch is the live connection epoch for outgoing frames.
func (sink *FrameSink) Epoch() uint64 { return sink.channel.epoch }

// BootID is the live boot identity.
func (sink *FrameSink) BootID() string { return sink.channel.bootID }

func (channel *Channel) dial(ctx context.Context) (*grpc.ClientConn, error) {
	caBody, err := os.ReadFile(channel.Config.QuoinRuntimeCAFile)
	if err != nil {
		return nil, fmt.Errorf("read Quoin Runtime CA: %w", err)
	}
	clientCert, err := tls.LoadX509KeyPair(channel.Config.QuoinRuntimeClientCertificateFile, channel.Config.QuoinRuntimeClientPrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load Plinth client identity: %w", err)
	}
	// The generated config carries a full https:// URL; gRPC dial targets
	// are bare host:port with the TLS identity supplied by the pool below.
	endpoint := strings.TrimPrefix(strings.TrimPrefix(channel.Config.QuoinEndpoint, "https://"), "http://")
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBody) {
		return nil, errors.New("Quoin Runtime CA 证书无法解析")
	}
	return grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			RootCAs: pool, ServerName: "quoin", MinVersion: tls.VersionTLS13,
			Certificates: []tls.Certificate{clientCert},
		})),
		// Dead-stream detection: a network partition that only drops packets
		// (docker bridge detach) leaves TCP sends buffered forever without
		// keepalive; the heartbeat failure then breaks the loop quickly and
		// the reconnect path takes over (T12, RUNTIME-CTRL-006).
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time: 20 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true,
		}),
	)
}

// RegisterTask records the cancel func of one running attempt. A cancellation
// that reached this boot before the worker registered wins: the late child is
// immediately cancelled and must not overwrite the tombstone.
func (channel *Channel) RegisterTask(attemptID int64, cancel context.CancelFunc) {
	channel.cancelMu.Lock()
	if _, cancelled := channel.cancelled[attemptID]; cancelled {
		channel.cancelMu.Unlock()
		cancel()
		return
	}
	if channel.active == nil {
		channel.active = map[int64]*activeTask{}
	}
	task := channel.active[attemptID]
	if task == nil {
		task = &activeTask{done: make(chan struct{})}
		channel.active[attemptID] = task
	}
	task.cancel = cancel
	channel.cancelMu.Unlock()
}

// stopTask cancels one running attempt and reports whether it was live.
// FinishTask removes a naturally terminated worker without invoking its
// cancellation function. Pending terminal result replay remains independent in
// channel.pending until Quoin's ResultAck is received.
func (channel *Channel) FinishTask(attemptID int64) {
	channel.cancelMu.Lock()
	task := channel.active[attemptID]
	delete(channel.active, attemptID)
	channel.cancelMu.Unlock()
	if task != nil {
		close(task.done)
	}
}

// stopTask only signals the task context. It is also deferred by natural
// supervisor completion paths, so the durable cancellation tombstone belongs
// exclusively to cancelAndWait (the CancelAttempt command path).
func (channel *Channel) stopTask(attemptID int64) bool {
	channel.cancelMu.Lock()
	task := channel.active[attemptID]
	var cancel context.CancelFunc
	if task != nil {
		cancel = task.cancel
	}
	channel.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return task != nil
}

// cancelAndWait makes a cancellation durable, signals the active worker, and
// waits for its goroutine to exit before Channel emits CancelAck.
func (channel *Channel) cancelAndWait(attemptID int64) {
	channel.cancelMu.Lock()
	if channel.cancelled == nil {
		channel.cancelled = map[int64]struct{}{}
	}
	channel.cancelled[attemptID] = struct{}{}
	task := channel.active[attemptID]
	var cancel context.CancelFunc
	if task != nil {
		cancel = task.cancel
	}
	channel.cancelMu.Unlock()
	if task == nil {
		return
	}
	if cancel != nil {
		cancel()
	}
	<-task.done
}

// TaskCancelled reports whether this boot has durably observed a cancellation
// for the attempt, including the dispatch-before-registration interleaving.
func (channel *Channel) TaskCancelled(attemptID int64) bool {
	channel.cancelMu.Lock()
	defer channel.cancelMu.Unlock()
	_, cancelled := channel.cancelled[attemptID]
	return cancelled
}

func (channel *Channel) waitForTask(attemptID int64) {
	channel.cancelMu.Lock()
	task := channel.active[attemptID]
	channel.cancelMu.Unlock()
	if task != nil {
		<-task.done
	}
}

// allocateCorrelation reserves a unique correlation id for one
// request/reply pair.
func (channel *Channel) allocateCorrelation() (uint64, chan *runtimev1.ControlEnvelope) {
	channel.replyMu.Lock()
	defer channel.replyMu.Unlock()
	if channel.waiters == nil {
		channel.waiters = map[uint64]chan *runtimev1.ControlEnvelope{}
	}
	channel.nextCorr++
	waiter := make(chan *runtimev1.ControlEnvelope, 1)
	channel.waiters[channel.nextCorr] = waiter
	return channel.nextCorr, waiter
}

// deliverReply routes one reply envelope to its waiter (no waiter: audit
// only — stale or duplicate replies are dropped).
func (channel *Channel) deliverReply(envelope *runtimev1.ControlEnvelope) {
	channel.replyMu.Lock()
	waiter, live := channel.waiters[envelope.GetCorrelationId()]
	if live {
		delete(channel.waiters, envelope.GetCorrelationId())
	}
	channel.replyMu.Unlock()
	if live {
		waiter <- envelope
	}
}

// Request sends one envelope and waits for the correlated reply.
func (channel *Channel) Request(ctx context.Context, envelope *runtimev1.ControlEnvelope) (*runtimev1.ControlEnvelope, error) {
	correlation, waiter := channel.allocateCorrelation()
	envelope.CorrelationId = correlation
	if err := channel.sendEnvelope(envelope); err != nil {
		channel.replyMu.Lock()
		delete(channel.waiters, correlation)
		channel.replyMu.Unlock()
		return nil, err
	}
	select {
	case reply := <-waiter:
		return reply, nil
	case <-ctx.Done():
		channel.replyMu.Lock()
		delete(channel.waiters, correlation)
		channel.replyMu.Unlock()
		return nil, ctx.Err()
	case <-time.After(30 * time.Second):
		channel.replyMu.Lock()
		delete(channel.waiters, correlation)
		channel.replyMu.Unlock()
		return nil, errors.New("控制流请求超时未收到回复")
	}
}

// TaskActive reports whether this boot is still executing the attempt.
func (channel *Channel) TaskActive(attemptID int64) bool {
	channel.cancelMu.Lock()
	defer channel.cancelMu.Unlock()
	_, live := channel.active[attemptID]
	return live
}

// ActiveAttempts returns the sorted ids this boot is actually executing
// (the ReconcileReport payload, RUNTIME-TASK-005).
func (channel *Channel) ActiveAttempts() []int64 {
	channel.cancelMu.Lock()
	defer channel.cancelMu.Unlock()
	ids := make([]int64, 0, len(channel.active))
	for id := range channel.active {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// HasPendingResult reports whether a terminal result of the attempt still
// awaits a surviving ResultAck.
func (channel *Channel) HasPendingResult(attemptID int64) bool {
	channel.pendingMu.Lock()
	defer channel.pendingMu.Unlock()
	_, live := channel.pending[attemptID]
	return live
}

// completePendingResult hands one ResultAck to the registered waiter and
// reports whether a pending entry existed (duplicate acks are dropped).
func (channel *Channel) completePendingResult(attemptID int64, ack *runtimev1.ResultAck) bool {
	channel.pendingMu.Lock()
	entry, live := channel.pending[attemptID]
	if live {
		delete(channel.pending, attemptID)
	}
	channel.pendingMu.Unlock()
	if !live {
		return false
	}
	select {
	case entry.ack <- ack:
	default:
	}
	return true
}

// RegisterResult records one terminal result for reliable delivery without
// waiting (the failure paths of the supervisor use this; delivery and the
// ack are the channel's responsibility).
func (channel *Channel) RegisterResult(proposal *runtimev1.ResultProposal) {
	channel.pendingMu.Lock()
	if channel.pending == nil {
		channel.pending = map[int64]*pendingResult{}
	}
	channel.pending[proposal.GetAttemptId()] = &pendingResult{proposal: proposal, ack: make(chan *runtimev1.ResultAck, 1)}
	channel.pendingMu.Unlock()
	channel.DeliverPendingResults()
}

// ProposeResult registers one terminal result and blocks until Quoin
// adjudicates it (a ResultAck arrives), the caller's context ends or the
// delivery window closes with the process. Reconnects re-deliver
// automatically; Quoin's idempotent adjudication makes every replay safe.
func (channel *Channel) ProposeResult(ctx context.Context, proposal *runtimev1.ResultProposal) (*runtimev1.ResultAck, error) {
	entry := &pendingResult{proposal: proposal, ack: make(chan *runtimev1.ResultAck, 1)}
	channel.pendingMu.Lock()
	if channel.pending == nil {
		channel.pending = map[int64]*pendingResult{}
	}
	channel.pending[proposal.GetAttemptId()] = entry
	channel.pendingMu.Unlock()
	channel.DeliverPendingResults()
	select {
	case ack := <-entry.ack:
		return ack, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// DeliverPendingResults sends every outstanding terminal result on the
// live stream (fire-and-forget: the recv loop completes entries from the
// ResultAck; a lost ack leaves the entry registered for the next round).
// Attempts are delivered in ascending id order for deterministic wiring.
func (channel *Channel) DeliverPendingResults() {
	channel.pendingMu.Lock()
	ids := make([]int64, 0, len(channel.pending))
	entries := make(map[int64]*pendingResult, len(channel.pending))
	for id, entry := range channel.pending {
		ids = append(ids, id)
		entries[id] = entry
	}
	channel.pendingMu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		proposal := entries[id].proposal
		if err := channel.sendEnvelope(&runtimev1.ControlEnvelope{
			CorrelationId: uint64(id),
			Msg:           &runtimev1.ControlEnvelope_ResultProposal{ResultProposal: proposal},
		}); err != nil {
			sharedops.LogEvent("plinth", "info", "runtime.result_pending", fmt.Sprintf("attempt=%d delivery deferred: %v", id, err))
			return
		}
	}
}

// RunResultDeliveryLoop retries outstanding terminal results on a fixed
// cadence while the process lives: it covers acks lost on a healthy stream
// and re-delivers everything after a reconnect (T12, RUNTIME-TASK-008).
func (channel *Channel) RunResultDeliveryLoop(ctx context.Context) {
	ticker := time.NewTicker(resultDeliveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if channel.HasAnyPendingResult() {
				channel.DeliverPendingResults()
			}
		}
	}
}

// HasAnyPendingResult reports whether any terminal result is outstanding.
func (channel *Channel) HasAnyPendingResult() bool {
	channel.pendingMu.Lock()
	defer channel.pendingMu.Unlock()
	return len(channel.pending) > 0
}

// ErrExternalToolResultTimeout 表示 QUOIN_ROUTED 工具结果在等待上限内
// 未送达（首发丢失且重连对账补发也未覆盖）。worker 把它收敛为模型可见的
// 失败 ToolResult，而不是无限阻塞整个 attempt。
var ErrExternalToolResultTimeout = errors.New("external tool result delivery timeout")

// AwaitExternalToolResult 注册一个按 tool_call_id 等待 QUOIN_ROUTED 工具
// 结果的 waiter 并阻塞到 Quoin 的 ExternalToolResult 帧到达、等待超过
// ExternalResultTimeout、调用方上下文结束(attempt 取消/worker 退出)或进程
// 关闭为止。超时与上下文结束时 waiter 被就地清理：Quoin 已封存，迟到帧
// 只审计丢弃，账实以 Quoin ledger 为准。
// waiter 生命周期跨流:同 boot 重连后 Quoin 经重连对账补发的结果仍能送达,
// 重放语义由 Quoin 的幂等封存保证。
func (channel *Channel) AwaitExternalToolResult(ctx context.Context, toolCallID int64) (*runtimev1.ExternalToolResult, error) {
	waiter := make(chan *runtimev1.ExternalToolResult, 1)
	channel.externalMu.Lock()
	if channel.externalWaiters == nil {
		channel.externalWaiters = map[int64]chan *runtimev1.ExternalToolResult{}
	}
	channel.externalWaiters[toolCallID] = waiter
	channel.externalMu.Unlock()
	timer := time.NewTimer(channel.externalResultTimeout())
	defer timer.Stop()
	select {
	case result := <-waiter:
		return result, nil
	case <-timer.C:
		channel.abandonExternalToolResult(toolCallID, waiter)
		return nil, ErrExternalToolResultTimeout
	case <-ctx.Done():
		channel.abandonExternalToolResult(toolCallID, waiter)
		return nil, ctx.Err()
	}
}

// abandonExternalToolResult 摘除一个 waiter;只有仍是注册的那个 chan 才
// 删除,避免迟到的 deliver 与新 waiter 交叉时误删。
func (channel *Channel) abandonExternalToolResult(toolCallID int64, waiter chan *runtimev1.ExternalToolResult) {
	channel.externalMu.Lock()
	if channel.externalWaiters[toolCallID] == waiter {
		delete(channel.externalWaiters, toolCallID)
	}
	channel.externalMu.Unlock()
}

// deliverExternalToolResult 把一帧 ExternalToolResult 路由给按 tool_call_id
// 注册的 waiter。没有 waiter(结果迟到于 attempt 取消/worker 退出,或重复
// 下发)时只审计丢弃:Quoin 是这类工具的权威封存方,丢帧不影响封存事实。
func (channel *Channel) deliverExternalToolResult(result *runtimev1.ExternalToolResult) {
	if result == nil {
		return
	}
	channel.externalMu.Lock()
	waiter, live := channel.externalWaiters[result.GetToolCallId()]
	if live {
		delete(channel.externalWaiters, result.GetToolCallId())
	}
	channel.externalMu.Unlock()
	if !live {
		sharedops.LogEvent("plinth", "info", "runtime.external_result_unwaited", fmt.Sprintf("tool_call=%d attempt=%d outcome=%s", result.GetToolCallId(), result.GetAttemptId(), result.GetOutcome()))
		return
	}
	select {
	case waiter <- result:
	default:
	}
}
