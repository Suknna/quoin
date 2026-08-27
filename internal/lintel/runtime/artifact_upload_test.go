package runtime

import (
	"context"
	"crypto/sha256"
	"net"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type capturedArtifactUpload struct {
	runtimev1.UnimplementedArtifactServiceServer
	mu               sync.Mutex
	frames           []*runtimev1.ArtifactUploadFrame
	closeAfterHeader bool
}

func (service *capturedArtifactUpload) Upload(stream runtimev1.ArtifactService_UploadServer) error {
	var uploadID string
	for {
		frame, err := stream.Recv()
		if err != nil {
			break
		}
		if header := frame.GetHeader(); header != nil {
			uploadID = header.GetUploadId()
		}
		service.mu.Lock()
		service.frames = append(service.frames, frame)
		closeNow := service.closeAfterHeader && len(service.frames) == 1
		service.mu.Unlock()
		if closeNow {
			return stream.SendAndClose(&runtimev1.ArtifactUploadResult{UploadId: uploadID, Committed: true, ArtifactId: 73})
		}
	}
	return stream.SendAndClose(&runtimev1.ArtifactUploadResult{UploadId: uploadID, Committed: true, ArtifactId: 73})
}

func TestUploadBrowserArtifactUsesBoundLintelFenceAndFullStream(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	captured := &capturedArtifactUpload{}
	runtimev1.RegisterArtifactServiceServer(server, captured)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	connection, err := grpc.DialContext(context.Background(), "bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	channel := &Channel{bootID: "lintel-boot"}
	channel.installArtifactUploadBinding(artifactUploadBinding{client: runtimev1.NewArtifactServiceClient(connection), context: context.Background(), epoch: 19})
	body := make([]byte, artifactUploadChunkBytes+5)
	for index := range body {
		body[index] = byte(index)
	}
	digest := sha256.Sum256(body)
	artifactID, err := channel.UploadBrowserArtifact(context.Background(), BrowserArtifactUpload{
		OperationID: 44, ChildAttemptID: 55, Kind: runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE, TraceIntegrity: runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE,
		Body: body, SHA256: digest[:], MediaType: "application/json", Sensitive: true,
	})
	if err != nil || artifactID != 73 {
		t.Fatalf("upload result id=%d err=%v", artifactID, err)
	}
	captured.mu.Lock()
	defer captured.mu.Unlock()
	if len(captured.frames) != 4 {
		t.Fatalf("frames=%d want header, 2 chunks, end", len(captured.frames))
	}
	header := captured.frames[0].GetHeader()
	if header == nil || header.GetAttemptId() != 55 || header.GetOwnerId() != 44 || header.GetOwnerType() != "browser_operation" ||
		header.GetBootId() != "lintel-boot" || header.GetConnectionEpoch() != 19 || header.GetKind() != runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE ||
		header.GetRetentionKind() != runtimev1.RetentionKind_RETENTION_KIND_GENERATED || !header.GetSensitive() || string(header.GetSha256()) != string(digest[:]) {
		t.Fatalf("unexpected upload header: %#v", header)
	}
	if header.GetUploadId() != "lintel-browser-44-55-ARTIFACT_KIND_TRACE-BROWSER_TRACE_INTEGRITY_COMPLETE-"+fmtHex(digest[:]) {
		t.Fatalf("upload id=%q", header.GetUploadId())
	}
	if first, second := captured.frames[1].GetChunk(), captured.frames[2].GetChunk(); first == nil || second == nil || first.GetOffset() != 0 || second.GetOffset() != artifactUploadChunkBytes || string(append(first.GetPayload(), second.GetPayload()...)) != string(body) {
		t.Fatalf("chunk fence/payload mismatch: first=%#v second=%#v", first, second)
	}
	if end := captured.frames[3].GetEnd(); end == nil || end.GetUploadedSizeBytes() != uint64(len(body)) {
		t.Fatalf("end=%#v", end)
	}
}

func TestTraceUploadIntegrityIsPartOfTheLintelArtifactIdentity(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	captured := &capturedArtifactUpload{}
	runtimev1.RegisterArtifactServiceServer(server, captured)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	connection, err := grpc.DialContext(context.Background(), "bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	channel := &Channel{bootID: "lintel-boot", explorationTraces: map[int64][]explorationTraceEntry{44: {{Action: "read", At: "2026-01-01T00:00:00Z"}}}}
	channel.installArtifactUploadBinding(artifactUploadBinding{client: runtimev1.NewArtifactServiceClient(connection), context: context.Background(), epoch: 19})
	completeBody := channel.traceForClose(44, "close_session", map[string]any{"action": "close_session"})
	completeDigest := sha256.Sum256(completeBody)
	// Model the first server commit before the late parent cancellation. The
	// local complete seal remains historical evidence; cancellation builds and
	// uploads a separate INCOMPLETE trace body.
	channel.markExplorationTraceCommitted(44, 73, completeDigest[:])
	incompleteBody := channel.traceForCancellation(44)
	incompleteDigest := sha256.Sum256(incompleteBody)
	for _, upload := range []struct {
		body      []byte
		digest    []byte
		integrity runtimev1.BrowserTraceIntegrity
	}{
		{completeBody, completeDigest[:], runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE},
		{incompleteBody, incompleteDigest[:], runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE},
	} {
		if _, err := channel.UploadBrowserArtifact(context.Background(), BrowserArtifactUpload{OperationID: 44, ChildAttemptID: 55, Kind: runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE, TraceIntegrity: upload.integrity, Body: upload.body, SHA256: upload.digest, MediaType: "application/json", Sensitive: true}); err != nil {
			t.Fatalf("%s upload: %v", upload.integrity, err)
		}
	}
	captured.mu.Lock()
	defer captured.mu.Unlock()
	if len(captured.frames) != 6 {
		t.Fatalf("frames=%d, want two complete streams", len(captured.frames))
	}
	complete, incomplete := captured.frames[0].GetHeader(), captured.frames[3].GetHeader()
	if complete == nil || incomplete == nil || complete.GetUploadId() == incomplete.GetUploadId() || string(complete.GetSha256()) == string(incomplete.GetSha256()) || complete.GetTraceIntegrity() != runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE || incomplete.GetTraceIntegrity() != runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE {
		t.Fatalf("complete/incomplete upload identities or digests collapsed: complete=%#v incomplete=%#v", complete, incomplete)
	}
}

func TestUploadBrowserArtifactAcceptsCommittedReplayThatClosesAfterHeader(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	captured := &capturedArtifactUpload{closeAfterHeader: true}
	runtimev1.RegisterArtifactServiceServer(server, captured)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	connection, err := grpc.DialContext(context.Background(), "bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	body := make([]byte, artifactUploadChunkBytes+1)
	digest := sha256.Sum256(body)
	channel := &Channel{bootID: "lintel-boot"}
	channel.installArtifactUploadBinding(artifactUploadBinding{client: runtimev1.NewArtifactServiceClient(connection), context: context.Background(), epoch: 19})
	id, err := channel.UploadBrowserArtifact(context.Background(), BrowserArtifactUpload{OperationID: 44, ChildAttemptID: 55, Kind: runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE, TraceIntegrity: runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE, Body: body, SHA256: digest[:], MediaType: "application/json", Sensitive: true})
	if err != nil || id != 73 {
		t.Fatalf("replayed upload id=%d err=%v", id, err)
	}
}

func TestUploadBrowserArtifactRejectsNonBrowserKindsAndBadTrace(t *testing.T) {
	channel := &Channel{}
	body := []byte("trace")
	digest := sha256.Sum256(body)
	for _, upload := range []BrowserArtifactUpload{
		{OperationID: 1, ChildAttemptID: 2, Kind: runtimev1.ArtifactKind_ARTIFACT_KIND_TOOL_RESULT, Body: body, SHA256: digest[:], MediaType: "text/plain"},
		{OperationID: 1, ChildAttemptID: 2, Kind: runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE, TraceIntegrity: runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE, Body: body, SHA256: digest[:], MediaType: "application/json", Sensitive: false},
	} {
		if _, err := channel.UploadBrowserArtifact(context.Background(), upload); err == nil {
			t.Fatalf("upload %#v unexpectedly accepted", upload)
		}
	}
}

func fmtHex(bytes []byte) string {
	const alphabet = "0123456789abcdef"
	text := make([]byte, len(bytes)*2)
	for index, value := range bytes {
		text[index*2], text[index*2+1] = alphabet[value>>4], alphabet[value&0x0f]
	}
	return string(text)
}

// stalledArtifactUpload accepts each header and then never closes the stream;
// it reproduces a Runtime upload RPC that is live at the transport layer but
// makes no application progress.
type stalledArtifactUpload struct {
	runtimev1.UnimplementedArtifactServiceServer
	mu      sync.Mutex
	attempt int
}

func (service *stalledArtifactUpload) Upload(stream runtimev1.ArtifactService_UploadServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	service.mu.Lock()
	service.attempt++
	service.mu.Unlock()
	<-stream.Context().Done()
	return stream.Context().Err()
}

func TestUploadBrowserArtifactBoundsStalledRPCAndRetries(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	stalled := &stalledArtifactUpload{}
	runtimev1.RegisterArtifactServiceServer(server, stalled)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	connection, err := grpc.DialContext(context.Background(), "bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	previousDeadline := browserArtifactUploadDeadline
	browserArtifactUploadDeadline = 20 * time.Millisecond
	t.Cleanup(func() { browserArtifactUploadDeadline = previousDeadline })
	body := []byte("stalled trace")
	digest := sha256.Sum256(body)
	channel := &Channel{bootID: "lintel-boot"}
	channel.installArtifactUploadBinding(artifactUploadBinding{client: runtimev1.NewArtifactServiceClient(connection), context: context.Background(), epoch: 19})
	started := time.Now()
	if _, err := channel.UploadBrowserArtifact(context.Background(), BrowserArtifactUpload{OperationID: 44, Kind: runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE, TraceIntegrity: runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE, Body: body, SHA256: digest[:], MediaType: "application/json", Sensitive: true}); err == nil {
		t.Fatal("stalled upload unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("upload was not bounded, elapsed=%s", elapsed)
	}
	stalled.mu.Lock()
	attempts := stalled.attempt
	stalled.mu.Unlock()
	if attempts != maxBrowserArtifactUploadAttempts {
		t.Fatalf("attempts=%d want=%d", attempts, maxBrowserArtifactUploadAttempts)
	}
}

func TestTraceStagingConcurrentSnapshotAndCleanup(t *testing.T) {
	channel := &Channel{Config: ChannelConfig{StateDirectory: t.TempDir()}, traceStaging: make(map[int64]string), started: make(map[int64]*runtimev1.StartBrowserOperation)}
	for worker := 0; worker < 8; worker++ {
		operationID := int64(901 + worker)
		channel.started[operationID] = &runtimev1.StartBrowserOperation{OperationId: operationID}
	}
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		operationID := int64(901 + worker)
		workers.Add(1)
		go func(operationID int64) {
			defer workers.Done()
			for iteration := 0; iteration < 100; iteration++ {
				if err := channel.appendTraceStaging(operationID, []byte(`{"entry":true}`)); err != nil {
					t.Errorf("append staging: %v", err)
					return
				}
				_ = channel.activeBrowserOperationIDs()
				if err := channel.deleteTraceStaging(operationID); err != nil {
					t.Errorf("delete staging: %v", err)
					return
				}
			}
		}(operationID)
	}
	workers.Wait()
}
