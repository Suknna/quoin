package app_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
	"github.com/Suknna/quoin/internal/contract"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/quoin/alerts"
	"github.com/Suknna/quoin/internal/quoin/app"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

// TestSteleRelayEndToEnd drives the frozen SteleRelay service in-process over
// a bufconn: GetCredentialSnapshot, then Deliver (firing) with metadata auth,
// asserts the occurrence persisted, then replays the same relay_id to prove
// idempotency.
func TestSteleRelayEndToEnd(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	secrets := root + "/secrets"
	config := contract.QuoinConfig{
		Component: "quoin", PublicOrigin: "https://quoin.test",
		DataDirectory:             root + "/data",
		BackupDirectory:           root + "/backup",
		RootKeyFile:               secrets + "/root-key",
		RuntimeTLSCertificateFile: secrets + "/runtime-tls.crt",
		RuntimeTLSPrivateKeyFile:  secrets + "/runtime-tls.key",
		SteleServiceTokenFile:     secrets + "/stele-service-token",
	}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	alertsService := alerts.NewService(database.SQL)

	serviceToken, err := os.ReadFile(config.SteleServiceTokenFile)
	if err != nil {
		t.Fatal(err)
	}
	// Seed a source with a bearer whose digest matches a known value.
	bearer := "known-test-bearer-0123456789abcdef"
	digest := sha256Sum(bearer)
	result, err := alertsService.CreateSource(ctx, "test-source", "alertmanager", digest[:], 1, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}

	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	app.RegisterSteleRelay(server, app.NewSteleRelayServer(alertsService, serviceToken))
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return listener.DialContext(ctx)
	}), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := runtimev1.NewSteleRelayClient(conn)

	// The deployment writes 32 random bytes; the wire text form is base64url
	// (RUNTIME-AUTH-006). The server hashes the received text; hash the same
	// text here.
	tokenText := base64.RawURLEncoding.EncodeToString(serviceToken)
	authCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+tokenText, "x-quoin-release", buildinfo.Release))

	snapshot, err := client.GetCredentialSnapshot(authCtx, &runtimev1.GetCredentialSnapshotRequest{ReleaseVersion: buildinfo.Release})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.GetSnapshotVersion() == 0 || len(snapshot.GetSources()) != 1 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	found := false
	for _, source := range snapshot.GetSources() {
		if source.GetSourceId() == result.SourceID && len(source.GetCredentials()) == 1 && source.GetCredentials()[0].GetCredentialId() == result.CredentialID {
			found = true
		}
	}
	if !found {
		t.Fatalf("seeded credential missing from snapshot: %+v", snapshot)
	}

	body := []byte(`{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"CPU","instance":"db-1"},"startsAt":"2026-08-17T10:00:00Z","fingerprint":"` + relayFingerprintHex(map[string]string{"alertname": "CPU", "instance": "db-1"}) + `"}],"truncatedAlerts":0}`)
	deliver := func(relayID string) *runtimev1.DeliveryRelayResponse {
		response, err := client.Deliver(authCtx, &runtimev1.DeliveryRelayRequest{
			RelayId: relayID, SourceId: result.SourceID, CredentialId: result.CredentialID,
			CredentialSnapshotVersion: snapshot.GetSnapshotVersion(), Protocol: "alertmanager",
			Body: body, ReleaseVersion: buildinfo.Release,
		})
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	first := deliver("relay-e2e-1")
	if first.GetStatus() != runtimev1.DeliveryStatus_DELIVERY_STATUS_ACCEPTED {
		t.Fatalf("first delivery=%s detail=%s", first.GetStatus(), first.GetDetail())
	}
	second := deliver("relay-e2e-1")
	if second.GetStatus() != runtimev1.DeliveryStatus_DELIVERY_STATUS_ACCEPTED {
		t.Fatalf("replay delivery=%s detail=%s", second.GetStatus(), second.GetDetail())
	}
	var occurrenceCount int
	if err := database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_occurrences`).Scan(&occurrenceCount); err != nil || occurrenceCount != 1 {
		t.Fatalf("occurrence count=%d err=%v", occurrenceCount, err)
	}
	var deliveryCount int
	if err := database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_deliveries`).Scan(&deliveryCount); err != nil || deliveryCount != 1 {
		t.Fatalf("delivery count=%d err=%v", deliveryCount, err)
	}

	// Wrong release must be rejected.
	badCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+tokenText, "x-quoin-release", "v9.9.9"))
	if _, err := client.GetCredentialSnapshot(badCtx, &runtimev1.GetCredentialSnapshotRequest{ReleaseVersion: "v9.9.9"}); err == nil {
		t.Fatal("release mismatch must fail")
	}
	// Wrong token must be rejected.
	badAuth := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer wrong-token", "x-quoin-release", buildinfo.Release))
	if _, err := client.GetCredentialSnapshot(badAuth, &runtimev1.GetCredentialSnapshotRequest{ReleaseVersion: buildinfo.Release}); err == nil {
		t.Fatal("bad token must fail")
	}
}

func sha256Sum(value string) [32]byte {
	return sha256.Sum256([]byte(value))
}

func relayFingerprintHex(labels map[string]string) string {
	return fmt.Sprintf("%x", binary.BigEndian.Uint64(alerts.FingerprintOf(labels)))
}
