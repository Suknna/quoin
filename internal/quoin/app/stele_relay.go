package app

// SteleRelay 服务端（ADR-0011，RUNTIME-STELE-001..006 演进）：mTLS 客户端
// 身份认证（CN=stele，ADR-0009）、完整 Proto 权威指纹准入、凭据 digest
// 快照、事件批量转交（DeliverEvents，按 source_kind 路由消费）、按需连接
// 材料投递（AcquireConnectionCredential）与出向网关流（Connect 转交
// steleGateway）。旧的同步 Deliver 已随本地队列 + DeliverEvents 模型删除。

import (
	"context"
	"errors"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/quoin/alerts"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// steleRelayServer implements the SteleRelay service: the inbound event
// surface (snapshot + batched delivery), the outbound connection-material
// seam and the gateway stream handoff.
type steleRelayServer struct {
	runtimev1.UnimplementedSteleRelayServer
	alerts      *alerts.Service
	connections *connections.Service
	gateway     *steleGateway
}

// NewSteleRelayServer builds the relay adapter. Authentication is the
// listener's mTLS client verification; there is no service token.
func NewSteleRelayServer(alertsService *alerts.Service, connectionsService *connections.Service, gateway *steleGateway) *steleRelayServer {
	return &steleRelayServer{alerts: alertsService, connections: connectionsService, gateway: gateway}
}

// RegisterSteleRelay attaches the service to the Runtime gRPC server.
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

// Connect 把网关流转交给 steleGateway（握手裁决与收发循环在那里）。
func (server *steleRelayServer) Connect(stream runtimev1.SteleRelay_ConnectServer) error {
	if server.gateway == nil {
		return status.Error(codes.Unavailable, "stele gateway is not wired")
	}
	return server.gateway.Connect(stream)
}

// GetCredentialSnapshot 保持原有语义（RUNTIME-STELE-002）：Stele 仅内存
// 缓存该 digest 快照，DeliverEvents 回传 snapshot_version。
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

// DeliverEvents 逐事件独立裁决（ADR-0011：Stele 本地队列的批量转发）。
// source_kind 路由：alertmanager -> alerts.Deliver（relay_id=event_id、
// body=归一化 payload JSON，语义与旧 Deliver 完全一致）；未知 source_kind
// 确定性 REJECTED。alerts 服务自身错误 -> UNAVAILABLE（Stele 稍后重试）。
// 返回与请求等长的 results，顺序一一对应。
func (server *steleRelayServer) DeliverEvents(ctx context.Context, request *runtimev1.DeliverEventsRequest) (*runtimev1.DeliverEventsResponse, error) {
	if err := server.authorize(ctx); err != nil {
		return nil, err
	}
	if err := server.checkContractFingerprint(request.GetContractFingerprint()); err != nil {
		return nil, err
	}
	response := &runtimev1.DeliverEventsResponse{
		Results: make([]runtimev1.EventDeliveryStatus, 0, len(request.GetEvents())),
	}
	for _, event := range request.GetEvents() {
		response.Results = append(response.Results, server.deliverOneEvent(ctx, event))
	}
	return response, nil
}

// deliverOneEvent 裁决单个事件并映射到 EventDeliveryStatus。
func (server *steleRelayServer) deliverOneEvent(ctx context.Context, event *runtimev1.RelayEvent) runtimev1.EventDeliveryStatus {
	if event.GetEventId() == "" {
		// 幂等键缺失是确定性畸形：重试也不会变好，直接 REJECTED。
		return runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_REJECTED
	}
	if event.GetSourceKind() != "alertmanager" {
		// v1 的唯一入向 source_kind；未知来源没有消费方，确定性拒绝。
		return runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_REJECTED
	}
	receivedAt := time.Now().UTC()
	if event.GetReceivedAt() != nil && event.GetReceivedAt().IsValid() {
		receivedAt = event.GetReceivedAt().AsTime().UTC()
	}
	result, err := server.alerts.Deliver(ctx, event.GetEventId(), event.GetSourceId(), event.GetCredentialId(), event.GetCredentialSnapshotVersion(), event.GetPayload(), receivedAt)
	if err != nil {
		// 服务错误（库不可用等）不是事件的确定性属性：UNAVAILABLE 让
		// Stele 稍后重试，超限入死信。
		return runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_UNAVAILABLE
	}
	switch {
	case result.Accepted:
		return runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_ACCEPTED
	case result.Rejected:
		return runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_REJECTED
	default:
		return runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_UNAVAILABLE
	}
}

// AcquireConnectionCredential 按需投递出向执行材料（ADR-0011）：仅接受
// prometheus/thanos 连接；model_provider 拒绝（PermissionDenied）——模型
// 凭据只走 Plinth 的 FetchCredentialGrant，绝不经网关信道分发。
func (server *steleRelayServer) AcquireConnectionCredential(ctx context.Context, request *runtimev1.AcquireConnectionCredentialRequest) (*runtimev1.AcquireConnectionCredentialResponse, error) {
	if err := server.authorize(ctx); err != nil {
		return nil, err
	}
	if err := server.checkContractFingerprint(request.GetContractFingerprint()); err != nil {
		return nil, err
	}
	if server.connections == nil {
		return nil, status.Error(codes.Unavailable, "connections not wired")
	}
	payload, err := server.connections.AcquireMetricsConnection(ctx, request.GetConnectionId())
	if err != nil {
		if errors.Is(err, connections.ErrAcquireDenied) {
			// 类型不符（含 model_provider）与禁用/不存在都是确定性拒绝。
			return nil, status.Error(codes.PermissionDenied, "connection material denied")
		}
		return nil, status.Error(codes.Unavailable, "connection material unavailable")
	}
	return &runtimev1.AcquireConnectionCredentialResponse{
		ConnectionId:           payload.ConnectionID,
		ConnectionRevisionId:   payload.ConnectionRevisionID,
		CredentialGenerationId: payload.CredentialGeneration,
		ConnectionType:         payload.ConnectionType,
		RevisionConfigJson:     payload.RevisionConfigJSON,
		Thanos: &runtimev1.ThanosCredentialSecret{
			Username:    payload.Metrics.Username,
			Password:    payload.Metrics.Password,
			BearerToken: payload.Metrics.BearerToken,
		},
	}, nil
}
