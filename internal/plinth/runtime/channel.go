// Package runtime implements Plinth's outbound Runtime channel (T06): the
// attached-stdin one-time registration subcommand, atomic long-term token
// persistence on the 0600 state volume, and the outbound Connect control
// loop with Hello handshake and heartbeats. Readiness stays strict: the ops
// endpoint flips to ready only after a Quoin-accepted handshake.
package runtime

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// State file layout on the state volume: token.json (0600) holds the
// long-term bearer and its generation; boot.json identifies this process
// boot. The file is written atomically (temp + rename) so a crash never
// leaves a partial token (RUNTIME-REG-002 supervisor duty).
type stateFile struct {
	Slot          string `json:"slot"`
	Generation    int64  `json:"generation"`
	LongTermToken string `json:"longTermToken"`
}

type bootFile struct {
	BootID string `json:"bootId"`
}

type Channel struct {
	Config ChannelConfig
	bootID string
	epoch  uint64 // per-boot connection counter (RUNTIME-CTRL-004)
	// Tasks executes connection_probe dispatches when wired (T07).
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
	pendingMu       sync.Mutex
	pending         map[int64]*pendingResult
	replyMu         sync.Mutex
	nextCorr        uint64
	waiters         map[uint64]chan *runtimev1.ControlEnvelope
	browserMu       sync.Mutex
	browserWaiters  map[int64]chan *runtimev1.ToolResultDelivery
	browserResults  map[int64]*runtimev1.ToolResultDelivery
	browserReleased map[int64]*runtimev1.ToolResultDelivery
}

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
	StateDirectory     string
	// CatalogDigest/catalogVersion stay empty for plinth (RUNTIME-CTRL-010).
}

func NewChannel(config ChannelConfig) (*Channel, error) {
	bootRaw := make([]byte, 16)
	if _, err := rand.Read(bootRaw); err != nil {
		return nil, err
	}
	return &Channel{
		Config: config, bootID: base64.RawURLEncoding.EncodeToString(bootRaw),
		active: map[int64]*activeTask{}, pending: map[int64]*pendingResult{},
		browserWaiters: map[int64]chan *runtimev1.ToolResultDelivery{}, browserResults: map[int64]*runtimev1.ToolResultDelivery{}, browserReleased: map[int64]*runtimev1.ToolResultDelivery{},
	}, nil
}

func (channel *Channel) tokenPath() string {
	return filepath.Join(channel.Config.StateDirectory, "runtime-token.json")
}
func (channel *Channel) bootPath() string {
	return filepath.Join(channel.Config.StateDirectory, "runtime-boot.json")
}

// RunRegister performs the attached-stdin one-time registration: it consumes
// the registration token from stdin (never argv), calls Register over TLS,
// and atomically persists the returned long-term token (first registration
// only; re-running with an existing token file reports the current state).
func (channel *Channel) RunRegister(ctx context.Context, stdin *os.File, stdout *os.File) error {
	// Read exactly one line from the attached TTY (bytes never in argv).
	buffer := make([]byte, 256)
	total := 0
	deadline := time.Now().Add(2 * time.Minute)
	_ = stdin.SetReadDeadline(deadline)
	for total < len(buffer) {
		n, err := stdin.Read(buffer[total:])
		if err != nil {
			return fmt.Errorf("读取注册令牌（attached stdin）失败: %w", err)
		}
		total += n
		if buffer[total-1] == '\n' || buffer[total-1] == '\r' {
			break
		}
	}
	tokenText := trimTokenWhitespace(string(buffer[:total]))
	if tokenText == "" {
		return errors.New("注册令牌为空")
	}
	var parsed struct {
		Slot       string `json:"slot"`
		Generation int64  `json:"generation"`
		Token      string `json:"token"`
	}
	if err := json.Unmarshal([]byte(tokenText), &parsed); err != nil {
		// Also accept a bare token with generation on argv-free stdin line
		// two; the admin reveal returns {slot,generation,token}.
		return fmt.Errorf("注册令牌格式必须是 {slot,generation,token} JSON: %w", err)
	}
	connection, err := channel.dial(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	client := runtimev1.NewRuntimeControlClient(connection)
	response, err := client.Register(metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+parsed.Token)), &runtimev1.RegisterRuntimeRequest{
		Slot:           runtimev1.RuntimeSlot_RUNTIME_SLOT_PLINTH,
		OneTimeToken:   parsed.Token,
		Generation:     uint64(parsed.Generation),
		BootId:         channel.bootID,
		ReleaseVersion: buildinfo.Release,
	})
	if err != nil {
		return fmt.Errorf("注册失败: %w", err)
	}
	if err := channel.persist(response.GetLongTermToken(), int64(response.GetGeneration())); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "注册成功：generation=%d。长期 token 已写入状态卷。\n", response.GetGeneration())
	return nil
}

func trimTokenWhitespace(value string) string {
	start, end := 0, len(value)
	for start < end && (value[start] == ' ' || value[start] == '\n' || value[start] == '\r' || value[start] == '\t') {
		start++
	}
	for end > start && (value[end-1] == ' ' || value[end-1] == '\n' || value[end-1] == '\r' || value[end-1] == '\t') {
		end--
	}
	return value[start:end]
}

// persist writes the long-term token atomically with 0600 permissions.
func (channel *Channel) persist(token string, generation int64) error {
	if err := os.MkdirAll(channel.Config.StateDirectory, 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(stateFile{Slot: channel.Config.Slot, Generation: generation, LongTermToken: token})
	if err != nil {
		return err
	}
	temp := channel.tokenPath() + ".tmp"
	if err := os.WriteFile(temp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, channel.tokenPath())
}

func (channel *Channel) loadToken() (stateFile, error) {
	var state stateFile
	body, err := os.ReadFile(channel.tokenPath())
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return state, err
	}
	if state.LongTermToken == "" || state.Generation == 0 {
		return state, errors.New("状态卷 token 不完整")
	}
	return state, nil
}

// RunConnect keeps the outbound control stream alive: Hello handshake, then
// heartbeats; rejected handshakes mark readiness unregistered-equivalent and
// the loop retries with backoff. Task frames arrive with later tickets.
func (channel *Channel) RunConnect(ctx context.Context, readiness *sharedops.Server) error {
	state, err := channel.loadToken()
	if err != nil {
		return fmt.Errorf("尚未注册（读取状态卷失败）: %w", err)
	}
	connection, err := channel.dial(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	client := runtimev1.NewRuntimeControlClient(connection)
	streamCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+state.LongTermToken))
	stream, err := client.Connect(streamCtx)
	if err != nil {
		return err
	}
	channel.Artifacts = runtimev1.NewArtifactServiceClient(connection)
	channel.epoch++
	hello := &runtimev1.Hello{
		Slot:            runtimev1.RuntimeSlot_RUNTIME_SLOT_PLINTH,
		BootId:          channel.bootID,
		ConnectionEpoch: channel.epoch,
		ReleaseVersion:  buildinfo.Release,
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
			readiness.SetReadiness(sharedops.Readiness{Component: channel.Config.Slot, Release: buildinfo.Release, Mode: "normal", AcceptingWork: false, Reason: sharedops.RuntimeUnregistered})
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
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	go func() {
		seq := uint64(0)
		for {
			select {
			case <-ctx.Done():
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
	}()
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
	case *runtimev1.ControlEnvelope_BrowserSubExecutionAck:
		channel.deliverReply(envelope)
	case *runtimev1.ControlEnvelope_ToolResultDelivery:
		accepted := channel.deliverBrowserToolResult(payload.ToolResultDelivery)
		_ = sink.Send(&runtimev1.ControlEnvelope{CorrelationId: envelope.GetCorrelationId(), Msg: &runtimev1.ControlEnvelope_ToolResultDeliveryAck{ToolResultDeliveryAck: &runtimev1.ToolResultDeliveryAck{ToolCallId: payload.ToolResultDelivery.GetToolCallId(), ChildAttemptId: payload.ToolResultDelivery.GetChildAttemptId(), Accepted: accepted}}})
	case *runtimev1.ControlEnvelope_ModelTokenDelta:
		// Transient observer deltas never reply; the ledger is the
		// authority (RUNTIME-AGENT).
	default:
		// Handshake-adjacent and lintel-only frames do not concern the
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

// TaskSupervisor executes dispatched attempts (T07 probes, T10 agent
// analysis). Channel owns cancellation acknowledgement so it can wait for the
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
	// The generated config carries a full https:// URL; gRPC dial targets
	// are bare host:port with the TLS identity supplied by the pool below.
	endpoint := strings.TrimPrefix(strings.TrimPrefix(channel.Config.QuoinEndpoint, "https://"), "http://")
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBody) {
		return nil, errors.New("Quoin Runtime CA 证书无法解析")
	}
	return grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "quoin", MinVersion: tls.VersionTLS13})),
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

// BearerToken returns the current long-term token for RPCs made outside
// the control stream (FetchCredentialGrant).
func (channel *Channel) BearerToken() (string, error) {
	state, err := channel.loadToken()
	if err != nil {
		return "", err
	}
	return state.LongTermToken, nil
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

// resultDeliveryInterval is the fixed retry cadence for outstanding
// terminal results (RUNTIME-SCOPE-004: frozen release-internal constant).
const resultDeliveryInterval = 3 * time.Second

// maxBrowserToolReplayTombstones bounds terminal replay protection after a
// worker consumes a delivery. The durable Quoin ledger remains the authority.
const maxBrowserToolReplayTombstones = 1024

// RegisterBrowserToolResult reserves the one durable ToolResultDelivery for a
// running quoin_browser Tool Call before its request leaves Plinth. A replay
// delivered on a preceding stream is retained by tool_call_id, so reconnects
// cannot race registration into a lost model turn.
func (channel *Channel) RegisterBrowserToolResult(toolCallID int64) (<-chan *runtimev1.ToolResultDelivery, error) {
	if toolCallID < 1 {
		return nil, errors.New("browser tool call id must be positive")
	}
	channel.browserMu.Lock()
	defer channel.browserMu.Unlock()
	if result := channel.browserResults[toolCallID]; result != nil {
		ready := make(chan *runtimev1.ToolResultDelivery, 1)
		ready <- result
		return ready, nil
	}
	if existing := channel.browserWaiters[toolCallID]; existing != nil {
		return existing, nil
	}
	waiter := make(chan *runtimev1.ToolResultDelivery, 1)
	channel.browserWaiters[toolCallID] = waiter
	return waiter, nil
}

// ReleaseBrowserToolResult drops the process-local delivery cache after the
// worker has consumed its terminal result. The durable Quoin Tool Call ledger,
// rather than this cache, remains the replay authority across reconnects.
func (channel *Channel) ReleaseBrowserToolResult(toolCallID int64) {
	if toolCallID < 1 {
		return
	}
	channel.browserMu.Lock()
	if result := channel.browserResults[toolCallID]; result != nil {
		if channel.browserReleased == nil {
			channel.browserReleased = map[int64]*runtimev1.ToolResultDelivery{}
		}
		// This bounded tombstone rejects a conflicting delayed replay without
		// retaining every worker result for the lifetime of the process.
		if len(channel.browserReleased) >= maxBrowserToolReplayTombstones {
			for id := range channel.browserReleased {
				delete(channel.browserReleased, id)
				break
			}
		}
		channel.browserReleased[toolCallID] = result
	}
	delete(channel.browserResults, toolCallID)
	delete(channel.browserWaiters, toolCallID)
	channel.browserMu.Unlock()
}

// deliverBrowserToolResult accepts exact replays only. A different terminal
// payload under the same Tool Call ID is a protocol violation, not a retry:
// accepting it would let a stale stream replace the model-visible result.
func (channel *Channel) deliverBrowserToolResult(result *runtimev1.ToolResultDelivery) bool {
	if result == nil || result.GetToolCallId() < 1 {
		return false
	}
	channel.browserMu.Lock()
	if existing := channel.browserResults[result.GetToolCallId()]; existing != nil {
		accepted := proto.Equal(existing, result)
		channel.browserMu.Unlock()
		return accepted
	}
	if released := channel.browserReleased[result.GetToolCallId()]; released != nil {
		accepted := proto.Equal(released, result)
		channel.browserMu.Unlock()
		return accepted
	}
	waiter := channel.browserWaiters[result.GetToolCallId()]
	channel.browserResults[result.GetToolCallId()] = result
	delete(channel.browserWaiters, result.GetToolCallId())
	channel.browserMu.Unlock()
	if waiter != nil {
		waiter <- result
	}
	return true
}
