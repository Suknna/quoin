package stele

import (
	"context"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/plugins"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type manifestClient struct {
	runtimev1.SteleRelayClient
	t        *testing.T
	manifest string
}

func (client manifestClient) assertManifest(ctx context.Context) {
	client.t.Helper()
	metadata, ok := metadata.FromOutgoingContext(ctx)
	if !ok || len(metadata.Get(plugins.InboundManifestMetadataKey)) != 1 || metadata.Get(plugins.InboundManifestMetadataKey)[0] != client.manifest {
		client.t.Fatalf("outgoing inbound manifest=%v", metadata.Get(plugins.InboundManifestMetadataKey))
	}
}

func (client manifestClient) GetCredentialSnapshot(ctx context.Context, _ *runtimev1.GetCredentialSnapshotRequest, _ ...grpc.CallOption) (*runtimev1.GetCredentialSnapshotResponse, error) {
	client.assertManifest(ctx)
	return &runtimev1.GetCredentialSnapshotResponse{ContractFingerprint: contract.ProtoAuthorityFingerprint, SnapshotVersion: 1}, nil
}

func (client manifestClient) DeliverEvents(ctx context.Context, _ *runtimev1.DeliverEventsRequest, _ ...grpc.CallOption) (*runtimev1.DeliverEventsResponse, error) {
	client.assertManifest(ctx)
	return &runtimev1.DeliverEventsResponse{}, nil
}

func TestRelayAttachesCompiledInboundManifest(t *testing.T) {
	fingerprint := "test-manifest"
	relay := &Relay{client: manifestClient{t: t, manifest: fingerprint}, manifest: fingerprint}
	relay.refresh(context.Background())
	if !relay.Ready() {
		t.Fatal("verified credential snapshot was not accepted")
	}
	if _, err := relay.DeliverEvents(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}
