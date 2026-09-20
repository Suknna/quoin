// Package stele implements the external gateway (ADR-0011): the single front
// door for every external platform interaction. Inbound webhooks are
// authenticated against Quoin's credential digest snapshot, parsed by
// EventSource plugins, and queued in a local SQLite outbox (ack on enqueue;
// reliability belongs to the queue). Outbound platform calls arrive over the
// long-lived SteleRelay gateway stream, resolve connection material on demand,
// and execute under per-connection rate limits. Stele judges only signatures,
// quotas, and reachability — never business semantics.
package stele

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxWebhookBody   = 16 << 20 // 16 MiB（入站 webhook 与出站响应体共用的上限）
	relayCallTimeout = 10 * time.Second
)

// Relay is the Stele→Quoin client half: the mTLS connection, the cached
// credential digest snapshot for inbound bearer auth, and the unary RPCs the
// forwarder and gateway need.
type Relay struct {
	conn   *grpc.ClientConn
	client runtimev1.SteleRelayClient

	mu        sync.RWMutex
	snapshot  *runtimev1.GetCredentialSnapshotResponse
	ready     bool
	lastError error
}

// NewRelay dials Quoin Runtime over mTLS: the deployment CA verifies the
// server and the CA-signed client certificate (CN=stele) authenticates Stele
// (ADR-0009). The gateway stream runs on the same conn.
func NewRelay(endpoint, caFile, clientCertFile, clientKeyFile string) (*Relay, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read Quoin CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("Quoin CA is not valid PEM")
	}
	clientCert, err := tls.LoadX509KeyPair(clientCertFile, clientKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load Stele client identity: %w", err)
	}
	transport := credentials.NewTLS(&tls.Config{
		RootCAs: pool, ServerName: "quoin", MinVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{clientCert},
	})
	// The deployment config writes https:// endpoints (schema pattern);
	// grpc.NewClient accepts bare host:port or dns:/// targets only.
	target := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(transport), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxWebhookBody+4096)))
	if err != nil {
		return nil, err
	}
	return &Relay{conn: conn, client: runtimev1.NewSteleRelayClient(conn)}, nil
}

func (relay *Relay) Close() error {
	return relay.conn.Close()
}

// Run refreshes the credential snapshot immediately and then every 5s until
// the context ends (RUNTIME-STELE-002: not ready until a snapshot loads
// successfully). The snapshot gates inbound bearer auth only; a transient
// loss keeps the last good snapshot so already-queued events keep flowing.
func (relay *Relay) Run(ctx context.Context) {
	relay.refresh(ctx)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			relay.refresh(ctx)
		}
	}
}

func (relay *Relay) refresh(ctx context.Context) {
	callCtx, cancel := context.WithTimeout(ctx, relayCallTimeout)
	defer cancel()
	response, err := relay.client.GetCredentialSnapshot(callCtx, &runtimev1.GetCredentialSnapshotRequest{ContractFingerprint: contract.ProtoAuthorityFingerprint})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		// Keep the previous snapshot; only the ready flag falls until the
		// next successful pull.
		relay.mu.Lock()
		relay.lastError = err
		relay.ready = false
		relay.mu.Unlock()
		sharedops.LogEvent("stele", "error", "relay.snapshot_failed", err.Error())
		return
	}
	if response.GetContractFingerprint() != contract.ProtoAuthorityFingerprint {
		relay.mu.Lock()
		relay.lastError = fmt.Errorf("proto contract fingerprint mismatch")
		relay.ready = false
		relay.mu.Unlock()
		return
	}
	relay.mu.Lock()
	relay.snapshot = response
	relay.ready = true
	relay.lastError = nil
	relay.mu.Unlock()
}

// Ready reports whether the credential snapshot is loaded (OPS-HEALTH-005).
func (relay *Relay) Ready() bool {
	relay.mu.RLock()
	defer relay.mu.RUnlock()
	return relay.ready
}

// Credential resolves the digest entry for a bearer on one source kind.
// protocol == EventSource.Kind() is part of the match: a credential minted for
// one protocol never authorizes another source's path (SEC-REVEAL-001: the
// bearer is base64url text of 32 raw bytes, the digest is SHA-256 of the RAW
// bytes, so decode before hashing).
func (relay *Relay) Credential(bearer, sourceKind string) (sourceID, credentialID int64, snapshotVersion uint64, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(bearer)
	if err != nil || len(raw) != 32 {
		return 0, 0, 0, false
	}
	digest := sha256.Sum256(raw)
	relay.mu.RLock()
	snapshot := relay.snapshot
	relay.mu.RUnlock()
	if snapshot == nil {
		return 0, 0, 0, false
	}
	for _, source := range snapshot.GetSources() {
		if !source.GetEnabled() || source.GetProtocol() != sourceKind {
			continue
		}
		for _, credential := range source.GetCredentials() {
			if subtleCompare(credential.GetDigest(), digest[:]) {
				return source.GetSourceId(), credential.GetCredentialId(), snapshot.GetSnapshotVersion(), true
			}
		}
	}
	return 0, 0, 0, false
}

func subtleCompare(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for index := range a {
		diff |= a[index] ^ b[index]
	}
	return diff == 0
}

// DeliverEvents hands one batch to Quoin (single attempt; retry/backoff is
// the forwarder's job on top of the local queue).
func (relay *Relay) DeliverEvents(ctx context.Context, events []*runtimev1.RelayEvent) (*runtimev1.DeliverEventsResponse, error) {
	callCtx, cancel := context.WithTimeout(ctx, relayCallTimeout)
	defer cancel()
	return relay.client.DeliverEvents(callCtx, &runtimev1.DeliverEventsRequest{
		ContractFingerprint: contract.ProtoAuthorityFingerprint,
		Events:              events,
	})
}

// AcquireConnectionCredential pulls one connection's material (non-secret
// revision projection + decrypted secret). miss/revision-mismatch is the only
// trigger; Stele caches it in memory.
func (relay *Relay) AcquireConnectionCredential(ctx context.Context, connectionID int64) (*runtimev1.AcquireConnectionCredentialResponse, error) {
	callCtx, cancel := context.WithTimeout(ctx, relayCallTimeout)
	defer cancel()
	response, err := relay.client.AcquireConnectionCredential(callCtx, &runtimev1.AcquireConnectionCredentialRequest{
		ConnectionId:        connectionID,
		ContractFingerprint: contract.ProtoAuthorityFingerprint,
	})
	if err != nil {
		return nil, err
	}
	if response.GetConnectionId() != connectionID {
		return nil, fmt.Errorf("acquire returned connection %d for request %d", response.GetConnectionId(), connectionID)
	}
	return response, nil
}

// ConnectStream opens the long-lived gateway bidi stream on the shared conn.
func (relay *Relay) ConnectStream(ctx context.Context) (grpc.BidiStreamingClient[runtimev1.SteleEnvelope, runtimev1.SteleEnvelope], error) {
	return relay.client.Connect(ctx)
}

func timestampProto(value time.Time) *timestamppb.Timestamp {
	return timestamppb.New(value)
}
