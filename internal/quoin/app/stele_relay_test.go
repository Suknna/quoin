package app_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/quoin/alerts"
	"github.com/Suknna/quoin/internal/quoin/app"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// relayTLSFixture boots the deployment-shaped mTLS listener material through
// the real secrets bootstrap (ADR-0009): one Runtime CA, the quoin server
// identity, and the stele/plinth client identities.
type relayTLSFixture struct {
	caPEM         []byte
	steleClient   tls.Certificate
	plinthClient  tls.Certificate
	serverAddress string
}

func startRelayServer(t *testing.T, alertsService *alerts.Service) relayTLSFixture {
	t.Helper()
	root := t.TempDir()
	secrets := root + "/secrets"
	config := contract.QuoinConfig{
		Component: "quoin", PublicOrigin: "https://quoin.test",
		DataDirectory:             root + "/data",
		BackupDirectory:           root + "/backup",
		RootKeyFile:               secrets + "/root-key",
		RuntimeTLSCertificateFile: secrets + "/runtime-tls.crt",
		RuntimeTLSPrivateKeyFile:  secrets + "/runtime-tls.key",
		RuntimeClientCAFile:       secrets + "/runtime-ca.pem",
	}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	fixture := relayTLSFixture{}
	var err error
	if fixture.caPEM, err = os.ReadFile(secrets + "/runtime-ca.pem"); err != nil {
		t.Fatal(err)
	}
	if fixture.steleClient, err = tls.LoadX509KeyPair(secrets+"/stele-client.crt", secrets+"/stele-client.key"); err != nil {
		t.Fatal(err)
	}
	if fixture.plinthClient, err = tls.LoadX509KeyPair(secrets+"/plinth-client.crt", secrets+"/plinth-client.key"); err != nil {
		t.Fatal(err)
	}
	serverCert, err := tls.LoadX509KeyPair(secrets+"/runtime-tls.crt", secrets+"/runtime-tls.key")
	if err != nil {
		t.Fatal(err)
	}
	clientPool := x509.NewCertPool()
	if !clientPool.AppendCertsFromPEM(fixture.caPEM) {
		t.Fatal("bootstrap CA cannot be parsed")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// TLS terminates inside gRPC exactly like production (grpc.Creds on a raw
	// listener): every handler context then carries the verified client
	// identity (ADR-0009).
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientPool,
	})))
	app.RegisterSteleRelay(server, app.NewSteleRelayServer(alertsService))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	fixture.serverAddress = listener.Addr().String()
	return fixture
}

func relayClient(t *testing.T, fixture relayTLSFixture, clientCert *tls.Certificate) runtimev1.SteleRelayClient {
	t.Helper()
	rootPool := x509.NewCertPool()
	if !rootPool.AppendCertsFromPEM(fixture.caPEM) {
		t.Fatal("bootstrap CA cannot be parsed")
	}
	tlsConfig := &tls.Config{RootCAs: rootPool, ServerName: "localhost", MinVersion: tls.VersionTLS13}
	if clientCert != nil {
		tlsConfig.Certificates = []tls.Certificate{*clientCert}
	}
	conn, err := grpc.NewClient(fixture.serverAddress, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return runtimev1.NewSteleRelayClient(conn)
}

// TestSteleRelayMTLSIdentityAndDelivery drives the frozen SteleRelay service
// over a real mTLS listener: the CN=stele client identity authenticates
// GetCredentialSnapshot and idempotent Deliver, the CN=plinth identity is
// rejected, a certificate-less dial cannot even complete the TLS handshake,
// and a mismatched contract fingerprint never admits a request (ADR-0009).
func TestSteleRelayMTLSIdentityAndDelivery(t *testing.T) {
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
		RuntimeClientCAFile:       secrets + "/runtime-ca.pem",
	}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	// Credential snapshot and delivery lookups are pure reads on the alerts
	// runner; they fail closed until the bootstrap's real read-only pool is
	// installed at assembly (alerts has no post-construction setter).
	alertsService, err := alerts.NewServiceWithReader(database.SQL, database.Reader, execution.NewRunner(database.SQL, execution.NewRegistry(), nil))
	if err != nil {
		t.Fatal(err)
	}

	// Seed a source with a bearer whose digest matches a known value. The
	// create command verifies a real administrator session proof, so seed one
	// and attach the execution metadata admission would provide.
	now := "2026-09-14T00:00:00Z"
	idle, absolute := "2036-09-14T00:00:00Z", "2036-09-21T00:00:00Z"
	for _, statement := range []string{
		`INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,1,'fixture',1,'` + now + `','` + now + `')`,
		`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,1,randomblob(32),1,'relay-fixture','` + now + `','` + now + `','` + idle + `','` + absolute + `')`,
	} {
		if _, err := database.SQL.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	adminCtx, err := execution.WithMetadata(ctx, execution.Metadata{
		CorrelationID: "stele-relay-seed",
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: 1},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-stele-relay-seed"},
		Session:       execution.SessionRef{ID: 1, AuthRevision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	bearer := "known-test-bearer-0123456789abcdef"
	digest := sha256Sum(bearer)
	result, _, err := alertsService.CreateSource(adminCtx, "relay-seed-0001", "test-source", "alertmanager", digest[:])
	if err != nil {
		t.Fatal(err)
	}

	fixture := startRelayServer(t, alertsService)

	// The CN=plinth identity must never reach the SteleRelay surface.
	plinthClient := relayClient(t, fixture, &fixture.plinthClient)
	if _, err := plinthClient.GetCredentialSnapshot(ctx, &runtimev1.GetCredentialSnapshotRequest{ContractFingerprint: contract.ProtoAuthorityFingerprint}); err == nil {
		t.Fatal("the plinth client identity must not authorize SteleRelay")
	}

	// A certificate-less dial cannot complete the mandatory-client-cert
	// handshake at all: the RPC fails on transport, before any handler.
	noCertClient := relayClient(t, fixture, nil)
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := noCertClient.GetCredentialSnapshot(callCtx, &runtimev1.GetCredentialSnapshotRequest{ContractFingerprint: contract.ProtoAuthorityFingerprint}); err == nil {
		t.Fatal("a dial without a client certificate must fail the mTLS handshake")
	}

	steleClient := relayClient(t, fixture, &fixture.steleClient)
	snapshot, err := steleClient.GetCredentialSnapshot(ctx, &runtimev1.GetCredentialSnapshotRequest{ContractFingerprint: contract.ProtoAuthorityFingerprint})
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
		response, err := steleClient.Deliver(ctx, &runtimev1.DeliveryRelayRequest{
			RelayId: relayID, SourceId: result.SourceID, CredentialId: result.CredentialID,
			CredentialSnapshotVersion: snapshot.GetSnapshotVersion(), Protocol: "alertmanager",
			Body: body, ContractFingerprint: contract.ProtoAuthorityFingerprint,
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

	// A missing or malformed contract fingerprint must never provide a legacy
	// release-version admission path — even with a valid stele identity.
	if _, err := steleClient.GetCredentialSnapshot(ctx, &runtimev1.GetCredentialSnapshotRequest{ContractFingerprint: "not-a-valid-fingerprint"}); err == nil {
		t.Fatal("invalid contract fingerprint must fail")
	}
	if _, err := steleClient.Deliver(ctx, &runtimev1.DeliveryRelayRequest{RelayId: "relay-bad-contract", SourceId: result.SourceID, CredentialId: result.CredentialID, CredentialSnapshotVersion: snapshot.GetSnapshotVersion(), Protocol: "alertmanager", Body: body}); err == nil {
		t.Fatal("missing contract fingerprint must fail")
	}
}

func sha256Sum(value string) [32]byte {
	return sha256.Sum256([]byte(value))
}

func relayFingerprintHex(labels map[string]string) string {
	return fmt.Sprintf("%x", binary.BigEndian.Uint64(alerts.FingerprintOf(labels)))
}
