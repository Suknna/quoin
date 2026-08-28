// Package runtime implements Lintel's outbound Runtime channel: the
// attached-stdin one-time registration subcommand, atomic token persistence,
// and control-stream state. The versioned Journey Catalog is embedded by both
// Lintel and Quoin, which reject a digest mismatch before browser work starts.
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
	"sync/atomic"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/lintel/browser"
	"github.com/Suknna/quoin/internal/lintel/browser/exploration"
	"github.com/Suknna/quoin/internal/lintel/profile"
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
	Config      ChannelConfig
	bootID      string
	epoch       uint64 // per-boot connection counter (RUNTIME-CTRL-004)
	browser     *browser.Manager
	profiles    *profile.Store
	operationMu sync.Mutex
	started     map[int64]*runtimev1.StartBrowserOperation
	startAcks   map[int64]*runtimev1.StartBrowserOperationAck
	completed   map[int64]*runtimev1.CompleteBrowserOperation
	completing  map[int64]bool
	// startupFailures are accepted Start operations whose physical startup failed.
	// The receive loop consumes an entry only after it has written StartAck on the
	// current stream; completion must never overtake the accepted Start boundary.
	startupFailures map[int64]*runtimev1.StartBrowserOperation
	// startAckFences prevent a synchronous/asynchronous startup crash from
	// sending a terminal completion ahead of the accepted StartAck that made
	// Quoin's operation row Running. They are released only after that write, or
	// when a broken stream is torn down for same-boot replay.
	startAckFences     map[int64]chan struct{}
	stopAcks           map[int64]*runtimev1.StopBrowserOperationAck
	published          map[int64]*runtimev1.PublishBrowserProfileResult
	tunnelMu           sync.Mutex
	tunnelCancels      map[int64]context.CancelFunc
	tunnelDones        map[int64]chan struct{}
	tunnelGenerations  map[int64]uint64
	tunnelBinding      *browserTunnelBinding // guarded by tunnelMu; immutable per control epoch
	outbound           uint64
	controlMu          sync.Mutex
	controlSend        func(*runtimev1.ControlEnvelope) error
	explorationMu      sync.Mutex
	explorationRunning map[int64]bool
	explorationCancels map[int64]context.CancelFunc
	// explorationDone closes only after an action has yielded its terminal CAS.
	// A cancellation that arrives while that CAS is held waits for this boundary
	// and then publishes the cancellation result; it never loses to a normal
	// result merely because the action happened to finish first.
	explorationDone map[int64]chan struct{}
	// explorationCancelling is the per-child terminal CAS. Once set, only the
	// cancellation handler may publish the child's terminal result.
	explorationCancelling map[int64]bool
	// explorationClaimAcks serializes the pre-upload normal-close arbitration
	// with Quoin. It is keyed by the immutable child Attempt ID.
	// explorationClaims retains the immutable pre-upload claim until its typed
	// acknowledgement arrives. Unlike an action result, a claim has no terminal
	// artifact yet, so losing its Ack must cause a same-boot reconnect replay.
	explorationClaims    map[int64]*runtimev1.BrowserExplorationTerminalClaim
	explorationClaimAcks map[int64]chan *runtimev1.BrowserExplorationTerminalClaimAck
	explorationResults   map[int64]*runtimev1.BrowserExplorationActionResult
	explorationTraces    map[int64][]explorationTraceEntry
	// explorationTraceSeals makes a terminal trace an immutable per-operation
	// value. Retries must resend the exact trace bytes/capability, not rebuild a
	// semantically similar trace with a new timestamp.
	explorationTraceSeals map[int64]explorationTraceSeal
	explorationChildren   map[int64]int64
	// traceStaging contains only files that this Lintel process actually wrote.
	// It has its own lock: trace appends and Hello/Heartbeat snapshots run
	// independently from operation lifecycle and must never share a map under
	// different mutexes.
	traceStagingMu sync.RWMutex
	// StopAck may claim trace staging cleanup only after removing one of these.
	traceStaging map[int64]string
	// explorationTerminalChildren records the child that owns the operation
	// terminal CAS while it seals a trace. It is guarded by operationMu.
	explorationTerminalChildren map[int64]int64
	// explorationActionCapabilities binds an executing child to its operation
	// under operationMu. Crash handling captures this capability atomically with
	// its terminal claim, so it cannot lose an in-flight trace to a racing action.
	explorationActionCapabilities map[int64]int64
	// browserCrashes retains a crash observation even while another terminal path
	// owns completing. That path must classify its eventual result as a crash,
	// never turn a concurrent process loss into a successful close/cancellation.
	browserCrashes map[int64]bool
	// Journey attempt state (T23): one executing child per operation and the
	// still-unacknowledged result proposals retained for same-boot replay.
	journeyRuns      map[int64]*journeyRun
	journeyProposals map[int64]*runtimev1.ResultProposal
	// Test seams retain the production Manager as the only normal execution
	// authority while allowing deterministic Runtime route tests without Chromium.
	executeBrowserAction func(context.Context, int64, exploration.Action) browser.ExplorationResult
	stopBrowser          func(int64) error
	artifactMu           sync.RWMutex
	artifactBinding      artifactUploadBinding
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
	if err := cleanupExplorationTraceStaging(config.StateDirectory); err != nil {
		return nil, err
	}
	channel := &Channel{Config: config, bootID: base64.RawURLEncoding.EncodeToString(bootRaw), browser: config.Browser, profiles: profile.NewStore(config.StateDirectory), journeyRuns: make(map[int64]*journeyRun), journeyProposals: make(map[int64]*runtimev1.ResultProposal), started: make(map[int64]*runtimev1.StartBrowserOperation), startAcks: make(map[int64]*runtimev1.StartBrowserOperationAck), completed: make(map[int64]*runtimev1.CompleteBrowserOperation), completing: make(map[int64]bool), startupFailures: make(map[int64]*runtimev1.StartBrowserOperation), startAckFences: make(map[int64]chan struct{}), stopAcks: make(map[int64]*runtimev1.StopBrowserOperationAck), published: make(map[int64]*runtimev1.PublishBrowserProfileResult), tunnelCancels: make(map[int64]context.CancelFunc), tunnelDones: make(map[int64]chan struct{}), tunnelGenerations: make(map[int64]uint64), explorationRunning: make(map[int64]bool), explorationCancels: make(map[int64]context.CancelFunc), explorationDone: make(map[int64]chan struct{}), explorationCancelling: make(map[int64]bool), explorationClaims: make(map[int64]*runtimev1.BrowserExplorationTerminalClaim), explorationClaimAcks: make(map[int64]chan *runtimev1.BrowserExplorationTerminalClaimAck), explorationResults: make(map[int64]*runtimev1.BrowserExplorationActionResult), explorationTraces: make(map[int64][]explorationTraceEntry), explorationTraceSeals: make(map[int64]explorationTraceSeal), explorationChildren: make(map[int64]int64), traceStaging: make(map[int64]string), explorationTerminalChildren: make(map[int64]int64), explorationActionCapabilities: make(map[int64]int64), browserCrashes: make(map[int64]bool)}
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

// isCurrentControlEnvelope fences buffered frames from a superseded stream
// before any operation handler can produce a browser or profile side effect.
// activeBrowserOperationIDs is the runtime's physical reconciliation report.
// Manager owns live Chromium/startup state; staged traces remain included until
// Stop cleanup deletes them, because they still require a same-boot fence.
func (channel *Channel) activeBrowserOperationIDs() []int64 {
	ids := make(map[int64]struct{})
	if channel.browser != nil {
		for _, id := range channel.browser.ActiveOperationIDs() {
			ids[id] = struct{}{}
		}
	}
	// The Runtime ownership ledger begins before physical startup and ends only
	// after terminal acknowledgement plus Stop cleanup. It therefore covers the
	// startup/probe/publish windows where Manager may already have stopped its
	// process but Quoin still needs an operation fence.
	channel.operationMu.Lock()
	for id := range channel.started {
		ids[id] = struct{}{}
	}
	channel.operationMu.Unlock()
	channel.traceStagingMu.RLock()
	for id := range channel.traceStaging {
		ids[id] = struct{}{}
	}
	channel.traceStagingMu.RUnlock()
	out := make([]int64, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// releaseStartAckFence opens the ordering gate after the current stream wrote
// the accepted StartAck. Closing a channel is idempotent only by ownership, so
// deletion and close happen together under operationMu.
func (channel *Channel) releaseStartAckFence(operationID int64) {
	channel.operationMu.Lock()
	fence := channel.startAckFences[operationID]
	if fence != nil {
		delete(channel.startAckFences, operationID)
		close(fence)
	}
	channel.operationMu.Unlock()
}

func (channel *Channel) releaseAllStartAckFences() {
	channel.operationMu.Lock()
	for operationID, fence := range channel.startAckFences {
		delete(channel.startAckFences, operationID)
		close(fence)
	}
	channel.operationMu.Unlock()
}

// awaitStartAckFence returns only after a terminal completion is allowed to
// follow its accepted StartAck. An absent fence means this direct/unit seam did
// not create a post-admission Start boundary.
func (channel *Channel) awaitStartAckFence(operationID int64) {
	channel.operationMu.Lock()
	fence := channel.startAckFences[operationID]
	channel.operationMu.Unlock()
	if fence != nil {
		<-fence
	}
}

func (channel *Channel) acceptExplorationAction(request *runtimev1.ExecuteBrowserExplorationAction) bool {
	if request == nil || request.GetOperationId() < 1 || request.GetChildAttemptId() < 1 || request.GetToolCallId() < 1 || request.GetInput() == nil {
		return false
	}
	channel.operationMu.Lock()
	defer channel.operationMu.Unlock()
	started := channel.started[request.GetOperationId()]
	// The accepted Start may already be terminalizing a physical startup failure.
	// Never admit a child merely because its StartAck was delivered first.
	return started != nil && !channel.completing[request.GetOperationId()] && channel.completed[request.GetOperationId()] == nil && started.GetKind() == runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_EXPLORATION
}

func isCurrentControlEnvelope(envelope *runtimev1.ControlEnvelope, bootID string, epoch uint64, seen map[uint64]struct{}) bool {
	if envelope.GetBootId() != bootID || envelope.GetConnectionEpoch() != epoch || envelope.GetMessageId() == 0 {
		return false
	}
	if _, duplicate := seen[envelope.GetMessageId()]; duplicate {
		return false
	}
	seen[envelope.GetMessageId()] = struct{}{}
	return true
}

// forgetTerminalOperation releases replay state after Quoin has durably
// acknowledged a terminal result and Stop has completed. stopAcks deliberately
// remains as the same-boot tombstone: deleting it would let a delayed Start
// recreate Chromium for an operation that Quoin already closed. A new boot
// naturally bounds tombstones because Channel is process-local.
func (channel *Channel) forgetTerminalOperation(operationID int64) {
	channel.operationMu.Lock()
	delete(channel.started, operationID)
	delete(channel.startAcks, operationID)
	delete(channel.completed, operationID)
	delete(channel.completing, operationID)
	delete(channel.startupFailures, operationID)
	delete(channel.startAckFences, operationID)
	delete(channel.explorationActionCapabilities, operationID)
	delete(channel.explorationTerminalChildren, operationID)
	delete(channel.published, operationID)
	channel.operationMu.Unlock()
	channel.explorationMu.Lock()
	delete(channel.explorationChildren, operationID)
	delete(channel.explorationTraces, operationID)
	delete(channel.explorationTraceSeals, operationID)
	channel.explorationMu.Unlock()
}

// hasPendingExplorationResult reports whether an unacknowledged terminal result
// still needs the operation's replay/trace state. It is deliberately separate
// from stopAcks: a Stop tombstone is retained for the whole boot.
func (channel *Channel) hasPendingExplorationResult(operationID int64) bool {
	channel.explorationMu.Lock()
	defer channel.explorationMu.Unlock()
	for _, result := range channel.explorationResults {
		if result.GetOperationId() == operationID {
			return true
		}
	}
	return false
}

// nextMessageID remains only for legacy unit seams. Production outbound
// envelopes are stamped by the serialized stream sender above.
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
	connection, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "quoin", MinVersion: tls.VersionTLS13})),
	)
	if err != nil {
		return nil, err
	}
	// grpc.NewClient is intentionally lazy. Explicitly start resolution and the
	// HTTP/2 transport so a short-lived BrowserTunnel is not left idle behind
	// the long-lived control stream.
	connection.Connect()
	return connection, nil
}
