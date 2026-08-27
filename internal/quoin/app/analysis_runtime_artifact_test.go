package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"net"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/quoin/artifact"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	_ "modernc.org/sqlite"
)

func TestArtifactServiceLintelUploadScope(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/artifact.db?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	lintelToken := installArtifactTestCredential(t, db, qruntime.SlotLintel, 0x11)
	plinthToken := installArtifactTestCredential(t, db, qruntime.SlotPlinth, 0x22)
	store, err := artifact.NewStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	slots := qruntime.NewService(db)
	// Upload is a data-plane operation of the current control stream, not merely
	// a bearer-authenticated RPC. Both principals in this scope test are attached
	// to the header's declared boot/epoch.
	slots.AttachStream(qruntime.SlotLintel, "lintel-boot", 2)
	slots.AttachStream(qruntime.SlotPlinth, "lintel-boot", 2)
	RegisterArtifactService(server, NewArtifactService(slots, store))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	connection, err := grpc.DialContext(context.Background(), "bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	client := runtimev1.NewArtifactServiceClient(connection)
	withBearer := func(token string) context.Context {
		return metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token))
	}
	empty := sha256.Sum256(nil)
	trace := func(kind runtimev1.ArtifactKind, owner string, sensitive bool) *runtimev1.ArtifactUploadHeader {
		header := &runtimev1.ArtifactUploadHeader{UploadId: "up-" + kind.String() + "-" + owner, AttemptId: 8, BootId: "lintel-boot", ConnectionEpoch: 2, OwnerType: owner, OwnerId: 9, Kind: kind, RetentionKind: runtimev1.RetentionKind_RETENTION_KIND_GENERATED, Sensitive: sensitive, Sha256: empty[:], MediaType: "application/json"}
		if kind == runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE {
			header.TraceIntegrity = runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE
		}
		return header
	}
	for _, header := range []*runtimev1.ArtifactUploadHeader{
		trace(runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE, "browser_operation", true),
		trace(runtimev1.ArtifactKind_ARTIFACT_KIND_SCREENSHOT, "browser_operation", false),
	} {
		result, err := sendArtifactHeader(withBearer(lintelToken), client, header)
		if err != nil || result.GetRejectReason() != runtimev1.UploadRejectReason_UPLOAD_REJECT_REASON_ATTEMPT_NOT_RUNNING {
			t.Fatalf("valid Lintel scope did not reach ledger: result=%#v err=%v", result, err)
		}
	}
	for _, header := range []*runtimev1.ArtifactUploadHeader{
		trace(runtimev1.ArtifactKind_ARTIFACT_KIND_TOOL_RESULT, "browser_operation", false),
		trace(runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE, "tool_call", true),
		trace(runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE, "browser_operation", false),
	} {
		if _, err := sendArtifactHeader(withBearer(lintelToken), client, header); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("out-of-scope Lintel header err=%v want PermissionDenied", err)
		}
	}
	if result, err := sendArtifactHeader(withBearer(plinthToken), client, trace(runtimev1.ArtifactKind_ARTIFACT_KIND_TOOL_RESULT, "tool_call", false)); err != nil || result.GetRejectReason() != runtimev1.UploadRejectReason_UPLOAD_REJECT_REASON_ATTEMPT_NOT_RUNNING {
		t.Fatalf("Plinth upload compatibility result=%#v err=%v", result, err)
	}
	if _, err := sendArtifactHeader(context.Background(), client, trace(runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE, "browser_operation", true)); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unauthenticated upload err=%v", err)
	}
	if _, err := client.ReadText(withBearer(lintelToken), &runtimev1.ArtifactReadTextRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Lintel text Artifact read err=%v", err)
	}
}

func installArtifactTestCredential(t *testing.T, db *sql.DB, slot string, fill byte) string {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var exists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM runtime_slots WHERE slot=?`, slot).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists == 0 {
		if _, err := db.Exec(`INSERT INTO runtime_slots(slot,state,created_at) VALUES(?,'unregistered',?)`, slot, now); err != nil {
			t.Fatal(err)
		}
	}
	raw := make([]byte, 32)
	for index := range raw {
		raw[index] = fill
	}
	digest := sha256.Sum256(raw)
	result, err := db.Exec(`INSERT INTO runtime_credentials(slot,generation,token_digest,created_at,confirmed_at) VALUES(?,1,?,?,?)`, slot, digest[:], now, now)
	if err != nil {
		t.Fatal(err)
	}
	credentialID, _ := result.LastInsertId()
	if _, err := db.Exec(`UPDATE runtime_slots SET state='registered',current_credential_id=?,row_version=row_version+1 WHERE slot=?`, credentialID, slot); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func sendArtifactHeader(ctx context.Context, client runtimev1.ArtifactServiceClient, header *runtimev1.ArtifactUploadHeader) (*runtimev1.ArtifactUploadResult, error) {
	stream, err := client.Upload(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&runtimev1.ArtifactUploadFrame{Frame: &runtimev1.ArtifactUploadFrame_Header{Header: header}}); err != nil {
		return nil, err
	}
	if err := stream.Send(&runtimev1.ArtifactUploadFrame{Frame: &runtimev1.ArtifactUploadFrame_End{End: &runtimev1.ArtifactUploadEnd{}}}); err != nil {
		return nil, err
	}
	return stream.CloseAndRecv()
}

func TestArtifactServiceRejectsOversizedHeaderBeforeStaging(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/artifact.db?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	token := installArtifactTestCredential(t, db, qruntime.SlotLintel, 0x33)
	store, err := artifact.NewStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	slots := qruntime.NewService(db)
	slots.AttachStream(qruntime.SlotLintel, "lintel-boot", 2)
	RegisterArtifactService(server, NewArtifactService(slots, store))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	connection, err := grpc.DialContext(context.Background(), "bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	digest := sha256.Sum256(nil)
	header := &runtimev1.ArtifactUploadHeader{UploadId: "oversized", AttemptId: 8, BootId: "lintel-boot", ConnectionEpoch: 2, OwnerType: "browser_operation", OwnerId: 9, Kind: runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE, RetentionKind: runtimev1.RetentionKind_RETENTION_KIND_GENERATED, Sensitive: true, TraceIntegrity: runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE, SizeBytes: maxRuntimeArtifactUploadBytes + 1, Sha256: digest[:], MediaType: "application/json"}
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token))
	if _, err := sendArtifactHeader(ctx, runtimev1.NewArtifactServiceClient(connection), header); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("oversized header error=%v want InvalidArgument", err)
	}
}
