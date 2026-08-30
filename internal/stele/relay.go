// Package stele implements the alert protocol entry: webhook HTTP intake,
// bearer authentication against the cached credential snapshot, and exact
// relay to Quoin's SteleRelay gRPC service.
package stele

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const maxWebhookBody = 16 << 20 // 16 MiB (Alertmanager webhook payload cap)

// Relay is the Stele→Quoin relay client with the cached credential snapshot.
type Relay struct {
	conn         *grpc.ClientConn
	client       runtimev1.SteleRelayClient
	serviceToken string
	mu           sync.RWMutex
	snapshot     *runtimev1.GetCredentialSnapshotResponse
	ready        bool
	lastError    error
}

// NewRelay dials Quoin Runtime over TLS and verifies the deployment CA.
func NewRelay(endpoint, caFile, serviceTokenFile string) (*Relay, error) {
	token, err := os.ReadFile(serviceTokenFile)
	if err != nil {
		return nil, fmt.Errorf("read service token: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read Quoin CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("Quoin CA is not valid PEM")
	}
	transport := credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "quoin", MinVersion: tls.VersionTLS13})
	// The deployment config writes https:// endpoints (schema pattern);
	// grpc.NewClient accepts bare host:port or dns:/// targets only.
	target := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(transport), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxWebhookBody+4096)))
	if err != nil {
		return nil, err
	}
	relay := &Relay{
		conn: conn, client: runtimev1.NewSteleRelayClient(conn),
		// RUNTIME-AUTH-006: the wire text is base64url of the raw 32 bytes.
		serviceToken: base64.RawURLEncoding.EncodeToString(token),
	}
	go relay.refreshLoop()
	return relay, nil
}

func (relay *Relay) Close() error {
	return relay.conn.Close()
}

func (relay *Relay) context() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	md := metadata.Pairs("authorization", "Bearer "+relay.serviceToken, "x-quoin-release", buildinfo.Release)
	return metadata.NewOutgoingContext(ctx, md), cancel
}

// refreshLoop pulls the credential snapshot on boot and then every 5s
// (RUNTIME-STELE-002: not ready until a snapshot loads successfully).
func (relay *Relay) refreshLoop() {
	relay.refresh()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		relay.refresh()
	}
}

func (relay *Relay) refresh() {
	ctx, cancel := relay.context()
	defer cancel()
	response, err := relay.client.GetCredentialSnapshot(ctx, &runtimev1.GetCredentialSnapshotRequest{ReleaseVersion: buildinfo.Release})
	if err != nil {
		relay.mu.Lock()
		relay.lastError = err
		relay.ready = false
		relay.mu.Unlock()
		sharedops.LogEvent("stele", "error", "relay.snapshot_failed", err.Error())
		return
	}
	if response.GetQuoinReleaseVersion() != buildinfo.Release {
		relay.mu.Lock()
		relay.lastError = fmt.Errorf("release mismatch: quoin=%s stele=%s", response.GetQuoinReleaseVersion(), buildinfo.Release)
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

// Ready reports whether the snapshot is loaded (OPS-HEALTH-005).
func (relay *Relay) Ready() bool {
	relay.mu.RLock()
	defer relay.mu.RUnlock()
	return relay.ready
}

// Credential returns the cached digest entry for a bearer, or false.
// The bearer is base64url text of 32 raw bytes (SEC-REVEAL-001); the database
// digest is SHA-256 of the RAW bytes, so decode before hashing.
func (relay *Relay) Credential(bearer string) (sourceID, credentialID int64, snapshotVersion uint64, ok bool) {
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
		if !source.GetEnabled() {
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

// Deliver relays one exact body with the relay_id; internal retries reuse the
// same id so Quoin dedupes (CONTEXT「Stele」). Each attempt gets its own
// deadline short enough that all three fit inside the webhook context.
func (relay *Relay) Deliver(ctx context.Context, relayID string, sourceID, credentialID int64, snapshotVersion uint64, body []byte, receivedAt time.Time) (runtimev1.DeliveryStatus, error) {
	request := &runtimev1.DeliveryRelayRequest{
		RelayId: relayID, SourceId: sourceID, CredentialId: credentialID,
		CredentialSnapshotVersion: uint64(snapshotVersion), Protocol: "alertmanager",
		Body: body, ReceivedAt: timestampProto(receivedAt), ReleaseVersion: buildinfo.Release,
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		md := metadata.Pairs("authorization", "Bearer "+relay.serviceToken, "x-quoin-release", buildinfo.Release)
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		response, err := relay.client.Deliver(metadata.NewOutgoingContext(callCtx, md), request)
		cancel()
		if err != nil {
			lastErr = err
			// UNAVAILABLE / deadline / resource exhausted: retry with backoff;
			// others permanent.
			if !isRetryable(err) {
				return runtimev1.DeliveryStatus_DELIVERY_STATUS_UNAVAILABLE, err
			}
			if err := sleepCtx(ctx, time.Duration(attempt+1)*500*time.Millisecond); err != nil {
				return runtimev1.DeliveryStatus_DELIVERY_STATUS_UNAVAILABLE, err
			}
			continue
		}
		if response.GetStatus() == runtimev1.DeliveryStatus_DELIVERY_STATUS_UNAVAILABLE {
			lastErr = errors.New(response.GetDetail())
			if err := sleepCtx(ctx, time.Duration(attempt+1)*500*time.Millisecond); err != nil {
				return runtimev1.DeliveryStatus_DELIVERY_STATUS_UNAVAILABLE, err
			}
			continue
		}
		return response.GetStatus(), nil
	}
	return runtimev1.DeliveryStatus_DELIVERY_STATUS_UNAVAILABLE, lastErr
}

func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	// grpc status codes that Alertmanager-safe Stele retries on.
	code := status.Code(err)
	return code == codes.Unavailable || code == codes.DeadlineExceeded || code == codes.ResourceExhausted
}

// sleepCtx sleeps for the duration unless the parent context expires first.
func sleepCtx(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func timestampProto(value time.Time) *timestamppb.Timestamp {
	return timestamppb.New(value)
}
