package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/quoin/artifact"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"github.com/Suknna/quoin/test/support"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	_ "modernc.org/sqlite"
)

// artifactMTLSFixture mirrors the production mTLS plane (ADR-0009): the
// deployment CA from the real secrets bootstrap, one gRPC server terminating
// TLS itself, and the plinth client identity.
type artifactMTLSFixture struct {
	address      string
	caPEM        []byte
	plinthClient tls.Certificate
}

func newArtifactFixture(t *testing.T, slots *qruntime.Service, store *artifact.Store) artifactMTLSFixture {
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
	if err := support.GenerateDeploymentSecrets(config); err != nil {
		t.Fatal(err)
	}
	fixture := artifactMTLSFixture{}
	var err error
	if fixture.caPEM, err = os.ReadFile(secrets + "/runtime-ca.pem"); err != nil {
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
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientPool,
	})))
	RegisterArtifactService(server, NewArtifactService(slots, store))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	fixture.address = listener.Addr().String()
	return fixture
}

func (fixture artifactMTLSFixture) client(t *testing.T, withIdentity bool) runtimev1.ArtifactServiceClient {
	t.Helper()
	rootPool := x509.NewCertPool()
	if !rootPool.AppendCertsFromPEM(fixture.caPEM) {
		t.Fatal("bootstrap CA cannot be parsed")
	}
	tlsConfig := &tls.Config{RootCAs: rootPool, ServerName: "localhost", MinVersion: tls.VersionTLS13}
	if withIdentity {
		tlsConfig.Certificates = []tls.Certificate{fixture.plinthClient}
	}
	conn, err := grpc.NewClient(fixture.address, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return runtimev1.NewArtifactServiceClient(conn)
}

func artifactStoreFixture(t *testing.T) (*sql.DB, *artifact.Store) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "artifact.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	// Pure reads (artifact read fences) fail closed until the fixture's one
	// real OpenReadOnly pool is wired; it lives exactly as long as the fixture.
	reader := fixtureReadOnlyPool(t, db)
	store, err := artifact.NewStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	return db, store
}

// TestArtifactServiceMTLSUploadIdentity drives the ArtifactService over the
// real mTLS plane: the CN=plinth identity authorizes uploads bound to the
// current control stream, a certificate-less dial cannot complete the
// handshake, and text reads without identity stay unauthenticated (ADR-0009).
func TestArtifactServiceMTLSUploadIdentity(t *testing.T) {
	_, store := artifactStoreFixture(t)
	slots := qruntime.NewService()
	// Upload is a data-plane operation of the current control stream, not
	// merely an identity-authenticated RPC: the header must name the slot's
	// attached boot/epoch.
	slots.AttachStream(qruntime.SlotPlinth, "plinth-boot", 2)
	fixture := newArtifactFixture(t, slots, store)
	identityClient := fixture.client(t, true)

	empty := sha256Empty()
	toolResult := func(owner string) *runtimev1.ArtifactUploadHeader {
		return &runtimev1.ArtifactUploadHeader{UploadId: "up-tool-result-" + owner, AttemptId: 8, BootId: "plinth-boot", ConnectionEpoch: 2, OwnerType: owner, OwnerId: 9, Kind: runtimev1.ArtifactKind_ARTIFACT_KIND_TOOL_RESULT, RetentionKind: runtimev1.RetentionKind_RETENTION_KIND_GENERATED, Sha256: empty[:], MediaType: "application/json"}
	}
	result, err := sendArtifactHeader(context.Background(), identityClient, toolResult("tool_call"))
	if err != nil || result.GetRejectReason() != runtimev1.UploadRejectReason_UPLOAD_REJECT_REASON_ATTEMPT_NOT_RUNNING {
		t.Fatalf("plinth upload did not reach ledger: result=%#v err=%v", result, err)
	}
	// A header naming a boot/epoch this slot does not own is refused even
	// with a valid identity.
	stale := toolResult("tool_call")
	stale.BootId = "another-boot"
	if _, err := sendArtifactHeader(context.Background(), identityClient, stale); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("stale stream upload err=%v want Unauthenticated", err)
	}

	// A dial without a client certificate fails the mandatory-client-cert
	// handshake before any handler runs.
	noIdentity := fixture.client(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := noIdentity.ReadText(ctx, &runtimev1.ArtifactReadTextRequest{}); err == nil {
		t.Fatal("a dial without a client certificate must fail the mTLS handshake")
	}

	// Text reads with the plinth identity pass the identity fence (the
	// attempt-bound read fence then rejects the empty request).
	if _, err := identityClient.ReadText(ctx, &runtimev1.ArtifactReadTextRequest{}); status.Code(err) == codes.Unauthenticated {
		t.Fatalf("plinth text read must pass the identity fence, got %v", err)
	}
}

// TestArtifactServiceRejectsOversizedHeaderBeforeStaging pins the transport
// bound: an oversized header is rejected with InvalidArgument before any
// staging happens, identity notwithstanding.
func TestArtifactServiceRejectsOversizedHeaderBeforeStaging(t *testing.T) {
	_, store := artifactStoreFixture(t)
	slots := qruntime.NewService()
	slots.AttachStream(qruntime.SlotPlinth, "plinth-boot", 2)
	fixture := newArtifactFixture(t, slots, store)
	client := fixture.client(t, true)
	empty := sha256Empty()
	header := &runtimev1.ArtifactUploadHeader{UploadId: "oversized", AttemptId: 8, BootId: "plinth-boot", ConnectionEpoch: 2, OwnerType: "tool_call", OwnerId: 9, Kind: runtimev1.ArtifactKind_ARTIFACT_KIND_TOOL_RESULT, RetentionKind: runtimev1.RetentionKind_RETENTION_KIND_GENERATED, SizeBytes: maxRuntimeArtifactUploadBytes + 1, Sha256: empty[:], MediaType: "application/json"}
	if _, err := sendArtifactHeader(context.Background(), client, header); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("oversized header error=%v want InvalidArgument", err)
	}
}

func sha256Empty() [32]byte {
	var empty [32]byte
	return empty
}

func sendArtifactHeader(ctx context.Context, client runtimev1.ArtifactServiceClient, header *runtimev1.ArtifactUploadHeader) (*runtimev1.ArtifactUploadResult, error) {
	stream, err := client.Upload(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&runtimev1.ArtifactUploadFrame{Frame: &runtimev1.ArtifactUploadFrame_Header{Header: header}}); err != nil {
		// A stream the server rejected after its first frame surfaces the
		// typed status only at CloseAndRecv; the Send error is the EOF
		// race of the rejected stream, never the verdict itself.
		result, closeErr := stream.CloseAndRecv()
		if closeErr != nil {
			return nil, closeErr
		}
		return result, nil
	}
	if err := stream.Send(&runtimev1.ArtifactUploadFrame{Frame: &runtimev1.ArtifactUploadFrame_End{End: &runtimev1.ArtifactUploadEnd{}}}); err != nil {
		result, closeErr := stream.CloseAndRecv()
		if closeErr != nil {
			return nil, closeErr
		}
		return result, nil
	}
	return stream.CloseAndRecv()
}
