// Package runtime implements Lintel's outbound Runtime channel (T06): the
// attached-stdin one-time registration subcommand, atomic token persistence
// and the outbound Connect loop carrying the Journey Catalog digest,
// browser capacity and Chromium revision in the Hello (RUNTIME-CTRL-010).
// The catalog is embedded as an empty journeys object for this stage; a
// later release builds the full catalog and both sides must agree on its
// JCS digest.
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
	"sync/atomic"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/lintel/browser"
	"github.com/Suknna/quoin/internal/lintel/catalog"
	"github.com/Suknna/quoin/internal/lintel/profile"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

type stateFile struct {
	Slot          string `json:"slot"`
	Generation    int64  `json:"generation"`
	LongTermToken string `json:"longTermToken"`
}

type Channel struct {
	Config            ChannelConfig
	bootID            string
	epoch             uint64 // per-boot connection counter (RUNTIME-CTRL-004)
	browser           *browser.Manager
	profiles          *profile.Store
	operationMu       sync.Mutex
	started           map[int64]*runtimev1.StartBrowserOperation
	startAcks         map[int64]*runtimev1.StartBrowserOperationAck
	completed         map[int64]*runtimev1.CompleteBrowserOperation
	completing        map[int64]bool
	stopAcks          map[int64]*runtimev1.StopBrowserOperationAck
	published         map[int64]*runtimev1.PublishBrowserProfileResult
	tunnelMu          sync.Mutex
	tunnelCancels     map[int64]context.CancelFunc
	tunnelGenerations map[int64]uint64
	outbound          uint64
	tunnelClient      runtimev1.BrowserTunnelClient
	tunnelContext     context.Context
	controlMu         sync.Mutex
	controlSend       func(*runtimev1.ControlEnvelope) error
}

type ChannelConfig struct {
	Slot               string // "lintel"
	QuoinEndpoint      string
	QuoinRuntimeCAFile string
	StateDirectory     string
	BrowserSlots       uint32
	ChromiumRevision   string
	Browser            *browser.Manager
}

func NewChannel(config ChannelConfig) (*Channel, error) {
	bootRaw := make([]byte, 16)
	if _, err := rand.Read(bootRaw); err != nil {
		return nil, err
	}
	if config.Browser == nil {
		return nil, errors.New("browser manager is required")
	}
	channel := &Channel{Config: config, bootID: base64.RawURLEncoding.EncodeToString(bootRaw), browser: config.Browser, profiles: profile.NewStore(config.StateDirectory), started: make(map[int64]*runtimev1.StartBrowserOperation), startAcks: make(map[int64]*runtimev1.StartBrowserOperationAck), completed: make(map[int64]*runtimev1.CompleteBrowserOperation), completing: make(map[int64]bool), stopAcks: make(map[int64]*runtimev1.StopBrowserOperationAck), published: make(map[int64]*runtimev1.PublishBrowserProfileResult), tunnelCancels: make(map[int64]context.CancelFunc), tunnelGenerations: make(map[int64]uint64)}
	config.Browser.OnCrash = channel.browserCrashed
	return channel, nil
}

func (channel *Channel) tokenPath() string {
	return filepath.Join(channel.Config.StateDirectory, "runtime-token.json")
}

// RunRegister consumes the one-time token from attached stdin and persists
// the long-term token atomically (mirror of the Plinth flow).
func (channel *Channel) RunRegister(ctx context.Context) error {
	buffer := make([]byte, 256)
	total := 0
	_ = os.Stdin.SetReadDeadline(time.Now().Add(2 * time.Minute))
	for total < len(buffer) {
		n, err := os.Stdin.Read(buffer[total:])
		if err != nil {
			return fmt.Errorf("读取注册令牌（attached stdin）失败: %w", err)
		}
		total += n
		if buffer[total-1] == '\n' || buffer[total-1] == '\r' {
			break
		}
	}
	text := trimWhitespace(string(buffer[:total]))
	var parsed struct {
		Slot       string `json:"slot"`
		Generation int64  `json:"generation"`
		Token      string `json:"token"`
	}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return fmt.Errorf("注册令牌格式必须是 {slot,generation,token} JSON: %w", err)
	}
	connection, err := channel.dial(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	client := runtimev1.NewRuntimeControlClient(connection)
	response, err := client.Register(metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+parsed.Token)), &runtimev1.RegisterRuntimeRequest{
		Slot:           runtimev1.RuntimeSlot_RUNTIME_SLOT_LINTEL,
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
	fmt.Printf("注册成功：generation=%d。长期 token 已写入状态卷。\n", response.GetGeneration())
	return nil
}

func trimWhitespace(value string) string {
	start, end := 0, len(value)
	for start < end && (value[start] == ' ' || value[start] == '\n' || value[start] == '\r' || value[start] == '\t') {
		start++
	}
	for end > start && (value[end-1] == ' ' || value[end-1] == '\n' || value[end-1] == '\r' || value[end-1] == '\t') {
		end--
	}
	return value[start:end]
}

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

// RunConnect keeps the lintel control stream alive; the Hello carries the
// catalog digest/version, browser capacity and Chromium revision
// (RUNTIME-CTRL-010), and each new boot answers the ProfileInventoryRequest
// with a complete empty report (RUNTIME-BROWSER-002).
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
	channel.tunnelClient = runtimev1.NewBrowserTunnelClient(connection)
	channel.tunnelContext = streamCtx
	stream, err := client.Connect(streamCtx)
	if err != nil {
		return err
	}
	channel.epoch++
	atomic.StoreUint64(&channel.outbound, 1) // Hello consumes the first outbound ID.
	hello := &runtimev1.Hello{
		Slot:                  runtimev1.RuntimeSlot_RUNTIME_SLOT_LINTEL,
		BootId:                channel.bootID,
		ConnectionEpoch:       channel.epoch,
		ReleaseVersion:        buildinfo.Release,
		JourneyCatalogDigest:  catalog.Digest(),
		JourneyCatalogVersion: catalog.Version,
		BrowserCapacitySlots:  channel.Config.BrowserSlots,
		ChromiumRevision:      channel.Config.ChromiumRevision,
	}
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
		// A new Lintel boot remains fenced until Quoin accepts its complete
		// profile inventory. No browser work is considered ready before then.
		readiness.SetReadiness(sharedops.Readiness{Component: channel.Config.Slot, Release: buildinfo.Release, Mode: "normal", AcceptingWork: !helloAck.GetProfileReconcileRequired(), Reason: sharedops.Ready})
	}
	sharedops.LogEvent("lintel", "info", "runtime.connected", "quoin="+channel.Config.QuoinEndpoint)
	var outbound sync.Mutex
	send := func(envelope *runtimev1.ControlEnvelope) error {
		outbound.Lock()
		defer outbound.Unlock()
		return stream.Send(envelope)
	}
	channel.controlMu.Lock()
	channel.controlSend = send
	channel.controlMu.Unlock()
	defer func() {
		channel.controlMu.Lock()
		channel.controlSend = nil
		channel.controlMu.Unlock()
	}()
	// Completion is an unknown-outcome message until Quoin returns a matching
	// acknowledgement. Replay it after every same-boot reconnect.
	channel.resendPendingCompletions()
	channel.reopenRunningBrowserTunnels()
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeat.C:
				messageID := channel.nextMessageID()
				if err := send(&runtimev1.ControlEnvelope{
					MessageId: messageID, ConnectionEpoch: channel.epoch, BootId: channel.bootID,
					Msg: &runtimev1.ControlEnvelope_Heartbeat{Heartbeat: &runtimev1.Heartbeat{Seq: messageID}},
				}); err != nil {
					return
				}
			}
		}
	}()
	for {
		envelope, err := stream.Recv()
		if err != nil {
			if readiness != nil {
				readiness.SetReadiness(sharedops.Readiness{Component: channel.Config.Slot, Release: buildinfo.Release, Mode: "normal", AcceptingWork: false, Reason: sharedops.DependencyUnavailable})
			}
			return fmt.Errorf("控制流结束: %w", err)
		}
		if ack := envelope.GetCompleteBrowserOperationAck(); ack != nil {
			channel.acknowledgeCompletion(ack)
			continue
		}
		if request := envelope.GetProfileInventoryRequest(); request != nil {
			if err := send(channel.inventoryResponse(envelope, request)); err != nil {
				return err
			}
			if readiness != nil {
				readiness.SetReadiness(sharedops.Readiness{Component: channel.Config.Slot, Release: buildinfo.Release, Mode: "normal", AcceptingWork: true, Reason: sharedops.Ready})
			}
			continue
		}
		if request := envelope.GetStartBrowserOperation(); request != nil {
			response := channel.startResponse(envelope, request)
			if err := send(response); err != nil {
				return err
			}
			if response.GetStartBrowserOperationAck().GetAccepted() {
				// Quoin commits Running from the durable Ack before Lintel executes
				// the follow-up operation work.
				if request.GetKind() == runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_MANUAL_LOGIN {
					go channel.openBrowserTunnel(request)
				} else if request.GetKind() == runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_AUTHENTICATION_PROBE {
					go channel.completeRevisionProbe(request)
				}
			}
			continue
		}
		if request := envelope.GetPublishBrowserProfile(); request != nil {
			if err := send(channel.publishResponse(envelope, request)); err != nil {
				return err
			}
			continue
		}
		if request := envelope.GetStopBrowserOperation(); request != nil {
			if err := send(channel.stopResponse(envelope, request)); err != nil {
				return err
			}
			continue
		}
	}
}

func (channel *Channel) nextMessageID() uint64 {
	return atomic.AddUint64(&channel.outbound, 1)
}

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
