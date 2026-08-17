package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/quoin/alerts"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// steleRelayServer implements the frozen SteleRelay unary service
// (RUNTIME-STELE-001..006): service-token metadata auth, strict release
// equality, credential digest snapshot, and idempotent Delivery relay.
type steleRelayServer struct {
	runtimev1.UnimplementedSteleRelayServer
	alerts    *alerts.Service
	tokenHash [32]byte
}

// NewSteleRelayServer builds the server with the deployment service token.
// The token file holds 32 random bytes; the wire text is the base64url form
// (RUNTIME-AUTH-006), so the server hashes that text for comparison.
func NewSteleRelayServer(alertsService *alerts.Service, serviceToken []byte) *steleRelayServer {
	text := base64.RawURLEncoding.EncodeToString(serviceToken)
	return &steleRelayServer{alerts: alertsService, tokenHash: sha256.Sum256([]byte(text))}
}

// RegisterSteleRelay attaches the unary service to the Runtime gRPC server.
func RegisterSteleRelay(server *grpc.Server, relay *steleRelayServer) {
	runtimev1.RegisterSteleRelayServer(server, relay)
}

func (server *steleRelayServer) authorize(ctx context.Context) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}
	values := md.Get("authorization")
	if len(values) != 1 || len(values[0]) < 8 || values[0][:7] != "Bearer " {
		return status.Error(codes.Unauthenticated, "invalid authorization metadata")
	}
	token := []byte(values[0][7:])
	if subtle.ConstantTimeCompare(hashOf(token), server.tokenHash[:]) != 1 {
		return status.Error(codes.Unauthenticated, "invalid service token")
	}
	return nil
}

func hashOf(value []byte) []byte {
	sum := sha256.Sum256(value)
	return sum[:]
}

func (server *steleRelayServer) checkRelease(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("x-quoin-release")
	if len(values) != 1 || values[0] != buildinfo.Release {
		return status.Error(codes.FailedPrecondition, "release version mismatch")
	}
	return nil
}

func (server *steleRelayServer) GetCredentialSnapshot(ctx context.Context, request *runtimev1.GetCredentialSnapshotRequest) (*runtimev1.GetCredentialSnapshotResponse, error) {
	if err := server.authorize(ctx); err != nil {
		return nil, err
	}
	if request.GetReleaseVersion() != buildinfo.Release {
		return nil, status.Error(codes.FailedPrecondition, "release version mismatch")
	}
	version, sources, err := server.alerts.CredentialSnapshot(ctx)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "credential snapshot unavailable")
	}
	response := &runtimev1.GetCredentialSnapshotResponse{
		SnapshotVersion:     version,
		QuoinReleaseVersion: buildinfo.Release,
	}
	for _, source := range sources {
		snapshot := &runtimev1.AlertSourceSnapshot{
			SourceId: source.SourceID, SourceKey: source.SourceKey,
			Protocol: source.Protocol, Enabled: source.Enabled,
		}
		for _, credential := range source.Credentials {
			snapshot.Credentials = append(snapshot.Credentials, &runtimev1.CredentialDigestEntry{
				CredentialId: credential.ID, Digest: credential.Digest,
			})
		}
		response.Sources = append(response.Sources, snapshot)
	}
	return response, nil
}

func (server *steleRelayServer) Deliver(ctx context.Context, request *runtimev1.DeliveryRelayRequest) (*runtimev1.DeliveryRelayResponse, error) {
	if err := server.authorize(ctx); err != nil {
		return nil, err
	}
	if err := server.checkRelease(ctx); err != nil {
		return nil, err
	}
	if request.GetReleaseVersion() != buildinfo.Release {
		return nil, status.Error(codes.FailedPrecondition, "release version mismatch")
	}
	if request.GetRelayId() == "" || request.GetSourceId() <= 0 || request.GetCredentialId() <= 0 || request.GetCredentialSnapshotVersion() < 1 {
		return nil, status.Error(codes.InvalidArgument, "relay_id, source_id, credential_id and snapshot_version are required")
	}
	receivedAt := time.Now().UTC()
	if request.GetReceivedAt() != nil && request.GetReceivedAt().IsValid() {
		receivedAt = request.GetReceivedAt().AsTime().UTC()
	}
	result, err := server.alerts.Deliver(ctx, request.GetRelayId(), request.GetSourceId(), request.GetCredentialId(), request.GetCredentialSnapshotVersion(), request.GetBody(), receivedAt)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "delivery could not be committed")
	}
	if result.Accepted {
		return &runtimev1.DeliveryRelayResponse{Status: runtimev1.DeliveryStatus_DELIVERY_STATUS_ACCEPTED, Detail: result.Detail}, nil
	}
	if result.Rejected {
		return &runtimev1.DeliveryRelayResponse{Status: runtimev1.DeliveryStatus_DELIVERY_STATUS_REJECTED, Detail: result.Detail}, nil
	}
	return &runtimev1.DeliveryRelayResponse{Status: runtimev1.DeliveryStatus_DELIVERY_STATUS_UNAVAILABLE, Detail: result.Detail}, nil
}
