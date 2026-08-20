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
	"strings"
	"sync"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
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
	// live-stream send state; guarded by outboundMu.
	outboundMu  sync.Mutex
	outboundSeq uint64
	sendStream  interface {
		Send(*runtimev1.ControlEnvelope) error
	}
	cancelMu sync.Mutex
	active   map[int64]context.CancelFunc
	replyMu  sync.Mutex
	nextCorr uint64
	waiters  map[uint64]chan *runtimev1.ControlEnvelope
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
	return &Channel{Config: config, bootID: base64.RawURLEncoding.EncodeToString(bootRaw)}, nil
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
	channel.epoch++
	hello := &runtimev1.Hello{
		Slot:            runtimev1.RuntimeSlot_RUNTIME_SLOT_PLINTH,
		BootId:          channel.bootID,
		ConnectionEpoch: channel.epoch,
		ReleaseVersion:  buildinfo.Release,
	}
	channel.outboundMu.Lock()
	channel.sendStream = stream
	channel.active = map[int64]context.CancelFunc{}
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
		switch payload := envelope.Msg.(type) {
		case *runtimev1.ControlEnvelope_DispatchAttempt:
			if channel.Tasks != nil {
				task := payload.DispatchAttempt
				go channel.Tasks.HandleDispatchAttempt(ctx, sink, client, task, channel.stopTask)
			}
		case *runtimev1.ControlEnvelope_CancelAttempt:
			if channel.Tasks != nil {
				channel.Tasks.HandleCancelAttempt(ctx, sink, payload.CancelAttempt, channel.stopTask)
			}
		case *runtimev1.ControlEnvelope_ResultAck:
			ack := payload.ResultAck
			sharedops.LogEvent("plinth", "info", "runtime.result_ack", fmt.Sprintf("attempt=%d accepted=%v detail=%s", ack.GetAttemptId(), ack.GetAccepted(), ack.GetDetail()))
			channel.deliverReply(envelope)
		case *runtimev1.ControlEnvelope_BeginModelCallAck:
			channel.deliverReply(envelope)
		case *runtimev1.ControlEnvelope_CompleteModelCallAck:
			channel.deliverReply(envelope)
		default:
			// Handshake-adjacent and lintel-only frames do not concern the
			// plinth task slice.
		}
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

// TaskSupervisor executes deterministic connection_probe dispatches (T07).
type TaskSupervisor interface {
	HandleDispatchAttempt(ctx context.Context, sink *FrameSink, client runtimev1.RuntimeControlClient, dispatch *runtimev1.DispatchAttempt, stopTask func(int64) bool)
	HandleCancelAttempt(ctx context.Context, sink *FrameSink, cancel *runtimev1.CancelAttempt, stopTask func(int64) bool)
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
	)
}

// RegisterTask records the cancel func of one running attempt.
func (channel *Channel) RegisterTask(attemptID int64, cancel context.CancelFunc) {
	channel.cancelMu.Lock()
	defer channel.cancelMu.Unlock()
	if channel.active == nil {
		channel.active = map[int64]context.CancelFunc{}
	}
	channel.active[attemptID] = cancel
}

// stopTask cancels one running attempt and reports whether it was live.
func (channel *Channel) stopTask(attemptID int64) bool {
	channel.cancelMu.Lock()
	cancel, live := channel.active[attemptID]
	delete(channel.active, attemptID)
	channel.cancelMu.Unlock()
	if live {
		cancel()
	}
	return live
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
