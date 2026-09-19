package app

import (
	"context"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/quoin/alerts"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// steleRelayServer implements the frozen SteleRelay unary service
// (RUNTIME-STELE-001..006): mTLS client-identity auth (CN=stele, ADR-0009),
// complete Proto authority fingerprint admission, credential digest snapshot,
// and idempotent Delivery relay.
type steleRelayServer struct {
	runtimev1.UnimplementedSteleRelayServer
	alerts *alerts.Service
}

// NewSteleRelayServer builds the relay adapter. Authentication is the
// listener's mTLS client verification; there is no service token.
func NewSteleRelayServer(alertsService *alerts.Service) *steleRelayServer {
	return &steleRelayServer{alerts: alertsService}
}

// RegisterSteleRelay attaches the unary service to the Runtime gRPC server.
func RegisterSteleRelay(server *grpc.Server, relay *steleRelayServer) {
	runtimev1.RegisterSteleRelayServer(server, relay)
}

func (server *steleRelayServer) authorize(ctx context.Context) error {
	if !requireComponentIdentity(ctx, "stele") {
		return status.Error(codes.Unauthenticated, "stele client identity required")
	}
	return nil
}

func (server *steleRelayServer) checkContractFingerprint(value string) error {
	if !contract.ValidProtoAuthorityFingerprint(value) || value != contract.ProtoAuthorityFingerprint {
		return status.Error(codes.FailedPrecondition, "Proto contract fingerprint mismatch")
	}
	return nil
}

func (server *steleRelayServer) GetCredentialSnapshot(ctx context.Context, request *runtimev1.GetCredentialSnapshotRequest) (*runtimev1.GetCredentialSnapshotResponse, error) {
	if err := server.authorize(ctx); err != nil {
		return nil, err
	}
	if err := server.checkContractFingerprint(request.GetContractFingerprint()); err != nil {
		return nil, err
	}
	version, sources, err := server.alerts.CredentialSnapshot(ctx)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "credential snapshot unavailable")
	}
	response := &runtimev1.GetCredentialSnapshotResponse{
		SnapshotVersion:     version,
		ContractFingerprint: contract.ProtoAuthorityFingerprint,
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
	if err := server.checkContractFingerprint(request.GetContractFingerprint()); err != nil {
		return nil, err
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
