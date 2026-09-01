package artifact

// T11 interruption tests (ARCH-VALIDATION-002): an interrupted upload keeps
// its ledger row uploading and its staging removed, and retries with the
// same upload_id/sha recover to the same artifact; a retry with different
// metadata conflicts. Expired bodies answer structured reads while their
// metadata and blob stay durable (the GC fence facts).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// interruptedUploadHeader builds the header of one tool_result upload.
func interruptedUploadHeader(attemptID, toolCallID int64, body string) UploadHeader {
	hash := sha256.Sum256([]byte(body))
	return UploadHeader{
		UploadID: "up-interrupted-1", AttemptID: attemptID,
		BootID: "boot-1", ConnectionEpoch: 7,
		OwnerType: "tool_call", OwnerID: toolCallID,
		Kind: "tool_result", RetentionKind: "generated",
		Sensitive: false, SizeBytes: int64(len(body)), SHA256: hash[:], MediaType: "text/plain",
	}
}

// TestTicket11UploadInterruptionRetry proves the bounded-retry contract
// (RUNTIME-UPLOAD-001/002): the transport aborts mid-stream, the ledger
// keeps uploading, and the retry with the same upload_id and digest
// commits the same body.
func TestTicket11UploadInterruptionRetry(t *testing.T) {
	db := newTestDB(t)
	store, err := NewStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	attemptID, toolCallID := seedToolOwner(t, db)
	body := strings.Repeat("thanos-matrix-line\n", 900)
	header := interruptedUploadHeader(attemptID, toolCallID, body)

	// First attempt: begin, write a partial chunk, then abandon WITHOUT
	// CommitUpload (the adapter closes both handles and removes the
	// staging file when the stream errors).
	file, replayID, err := store.BeginUpload(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	if replayID != 0 {
		t.Fatal("fresh upload must not replay")
	}
	if _, err := file.Write([]byte(body[:1024])); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if err := os.RemoveAll(store.StagingPath(header.UploadID)); err != nil {
		t.Fatal(err)
	}
	store.AbortUpload(header.UploadID)
	// The ledger must still be uploading (a crash never fabricates a
	// commit), and no blob may exist.
	var state string
	if err := db.QueryRow(`SELECT state FROM runtime_artifact_uploads WHERE upload_id=?`, header.UploadID).Scan(&state); err != nil || state != "uploading" {
		t.Fatalf("state=%q err=%v", state, err)
	}
	if entries, _ := filepath.Glob(filepath.Join(store.dir, "blobs", "*.blob")); len(entries) != 0 {
		t.Fatalf("blobs created before commit: %v", entries)
	}

	// Retry: the same upload_id and digest reset the staging file and
	// commit normally (whole-body retransmission, RUNTIME-UPLOAD-002).
	retryFile, retryReplay, err := store.BeginUpload(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	if retryReplay != 0 {
		t.Fatal("interrupted upload must not replay before a commit")
	}
	if _, err := retryFile.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	artifactID, err := store.CommitUpload(ctx, header, retryFile)
	if err != nil {
		t.Fatal(err)
	}
	if artifactID <= 0 {
		t.Fatal("commit returned no artifact id")
	}
	var committedState string
	if err := db.QueryRow(`SELECT state FROM runtime_artifact_uploads WHERE upload_id=?`, header.UploadID).Scan(&committedState); err != nil || committedState != "committed" {
		t.Fatalf("state=%q err=%v", committedState, err)
	}
	// A third begin replays the committed id (Ack-loss recovery).
	replayFile, replayID, err := store.BeginUpload(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	if replayID != artifactID {
		t.Fatalf("replay id=%d want %d", replayID, artifactID)
	}
	if replayFile != nil {
		t.Fatal("replay must not reopen staging handles")
	}
}

// TestTicket11UploadInterruptionMetadataConflict proves the same upload_id
// with different content is a deterministic conflict, never a second
// authority (RUNTIME-UPLOAD-002, DATA-ARTIFACT-006).
func TestTicket11UploadInterruptionMetadataConflict(t *testing.T) {
	db := newTestDB(t)
	store, err := NewStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	attemptID, toolCallID := seedToolOwner(t, db)
	header := interruptedUploadHeader(attemptID, toolCallID, "first-body")
	file, _, err := store.BeginUpload(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	_ = os.RemoveAll(store.StagingPath(header.UploadID))
	store.AbortUpload(header.UploadID)
	different := interruptedUploadHeader(attemptID, toolCallID, "second-body")
	different.UploadID = header.UploadID
	_, _, err = store.BeginUpload(ctx, different)
	var rejection *Rejection
	if !errors.As(err, &rejection) || rejection.Reason != RejectMetadataMismatch {
		t.Fatalf("err=%v", err)
	}
}

// TestTicket11ExpiredBodyReadFence proves the GC fence facts
// (DATA-ARTIFACT-003/004): after expiry the metadata stays durable with
// body_expired=1, the blob file remains (a single live reference forbids
// physical collection), and every read/grep path answers the structured
// ErrBodyExpired instead of serving or deleting the bytes.
func TestGeneratedArtifactRetentionExpiresBodyAndCollectsUnreferencedBlob(t *testing.T) {
	db := newTestDB(t)
	store, err := NewStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return time.Now().UTC().Add(-91 * 24 * time.Hour) }
	attemptID, toolCallID := seedToolOwner(t, db)
	artifactID, shaHex := uploadText(t, store, context.Background(), attemptID, toolCallID, "expired generated body")
	sealToolResult(t, db, store, attemptID, toolCallID, artifactID)
	store.now = func() time.Time { return time.Now().UTC() }
	store.RunGarbageCollection(context.Background())
	meta, err := store.Metadata(context.Background(), artifactID)
	if err != nil || !meta.BodyExpired {
		t.Fatalf("meta=%+v err=%v", meta, err)
	}
	if _, err := os.Stat(filepath.Join(store.dir, "blobs", shaHex+".blob")); !os.IsNotExist(err) {
		t.Fatalf("expired unreferenced body stat err=%v, want not exist", err)
	}
}

func TestTicket11ExpiredBodyReadFence(t *testing.T) {
	db := newTestDB(t)
	store, err := NewStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	attemptID, toolCallID := seedToolOwner(t, db)
	body := "series-line-1\nseries-line-2\n"
	artifactID, shaHex := uploadText(t, store, ctx, attemptID, toolCallID, body)
	sealToolResult(t, db, store, attemptID, toolCallID, artifactID)
	blobPath := filepath.Join(store.dir, "blobs", shaHex+".blob")
	if _, err := os.Stat(blobPath); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE artifacts SET body_expired=1 WHERE id=?`, artifactID); err != nil {
		t.Fatal(err)
	}
	// Metadata persists with the expiry fact.
	meta, err := store.Metadata(ctx, artifactID)
	if err != nil || !meta.BodyExpired {
		t.Fatalf("meta=%+v err=%v", meta, err)
	}
	// The blob stays (GC may only collect when EVERY reference expired;
	// the tool_call reference still needs the metadata lineage).
	if _, err := os.Stat(blobPath); err != nil {
		t.Fatalf("blob vanished before GC: %v", err)
	}
	if _, err := store.ReadText(ctx, attemptID, artifactID, "boot-1", 7, 1, 200); !errors.Is(err, ErrBodyExpired) {
		t.Fatalf("read err=%v", err)
	}
	if _, err := store.GrepText(ctx, attemptID, artifactID, "boot-1", 7, "series", 200, 5); !errors.Is(err, ErrBodyExpired) {
		t.Fatalf("grep err=%v", err)
	}
	_, _, err = store.OpenBody(ctx, artifactID)
	if !errors.Is(err, ErrBodyExpired) {
		t.Fatalf("open err=%v", err)
	}
}

// TestTicket11ReadGrepDeniedOutsideGrant proves the attempt fence: another
// attempt (or a wrong boot/epoch) can never read the tool result
// (RUNTIME-ARTIFACT-002).
func TestTicket11ReadGrepDeniedOutsideGrant(t *testing.T) {
	db := newTestDB(t)
	store, err := NewStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	attemptID, toolCallID := seedToolOwner(t, db)
	body := "granted-line\n"
	artifactID, _ := uploadText(t, store, ctx, attemptID, toolCallID, body)
	sealToolResult(t, db, store, attemptID, toolCallID, artifactID)
	if _, err := store.ReadText(ctx, attemptID, artifactID, "boot-1", 8, 1, 200); err == nil {
		t.Fatal("read with a wrong epoch must be denied")
	}
	if _, err := store.GrepText(ctx, attemptID+1, artifactID, "boot-1", 7, "granted", 200, 5); err == nil {
		t.Fatal("grep with a wrong attempt must be denied")
	}
	// The granted read still works (the fence denies, it does not destroy).
	slice, err := store.ReadText(ctx, attemptID, artifactID, "boot-1", 7, 1, 200)
	if err != nil || !strings.Contains(string(slice.Content), "granted-line") {
		t.Fatalf("read=%v err=%v", string(slice.Content), err)
	}
}

// TestTicket11MetadataAfterExpiryStable proves the frozen retention facts
// a generated artifact carries from creation: expires_at is set once and
// the logical row never rewrites it (DATA-ARTIFACT-003).
func TestTicket11MetadataAfterExpiryStable(t *testing.T) {
	db := newTestDB(t)
	store, err := NewStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	attemptID, toolCallID := seedToolOwner(t, db)
	artifactID, _ := uploadText(t, store, ctx, attemptID, toolCallID, "stable-line\n")
	sealToolResult(t, db, store, attemptID, toolCallID, artifactID)
	var expiresAt string
	var createdAt string
	if err := db.QueryRow(`SELECT expires_at, created_at FROM artifacts WHERE id=?`, artifactID).Scan(&expiresAt, &createdAt); err != nil {
		t.Fatal(err)
	}
	if expiresAt == "" || expiresAt < createdAt {
		t.Fatalf("expiresAt=%q createdAt=%q", expiresAt, createdAt)
	}
	if _, err := time.Parse(time.RFC3339Nano, expiresAt); err != nil {
		t.Fatalf("expiresAt not RFC3339: %v", err)
	}
}

// TestBeginUploadDoesNotHoldPoolConnectionWhileStreaming reproduces the
// single-connection deployment: receiving a slow body must not starve every
// unrelated read until the client finishes uploading.
func TestBeginUploadDoesNotHoldPoolConnectionWhileStreaming(t *testing.T) {
	db := newTestDB(t)
	db.SetMaxOpenConns(1)
	store, err := NewStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	attemptID, toolCallID := seedToolOwner(t, db)
	header := interruptedUploadHeader(attemptID, toolCallID, "slow body")
	file, replayID, err := store.BeginUpload(context.Background(), header)
	if err != nil || replayID != 0 {
		t.Fatalf("begin replay=%d err=%v", replayID, err)
	}
	defer func() {
		_ = file.Close()
		store.AbortUpload(header.UploadID)
	}()
	// This query would block forever with the former BeginUpload connection
	// retained across stream body writes.
	queryContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var count int
	if err := db.QueryRowContext(queryContext, `SELECT COUNT(*) FROM runtime_artifact_uploads WHERE upload_id=?`, header.UploadID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("unrelated query blocked or lost ledger: count=%d err=%v", count, err)
	}
}

// TestCommitFailureNeverDeletesSharedBlob reproduces two uploads of the same
// content where the second reference transaction fails after the first has
// committed. The failed upload may remove only its staging name, never the
// already-shared content-addressed blob.
func TestCommitFailureNeverDeletesSharedBlob(t *testing.T) {
	db := newTestDB(t)
	store, err := NewStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	firstAttempt, firstTool := seedToolOwner(t, db)
	body := "shared blob body"
	_, digest := uploadText(t, store, context.Background(), firstAttempt, firstTool, body)
	hash := sha256.Sum256([]byte(body))
	// A separate upload capability may attach another generated artifact to the
	// same pending owner; the digest is the shared physical blob identity.
	header := UploadHeader{UploadID: "shared-blob-failing-reference", AttemptID: firstAttempt, BootID: "boot-1", ConnectionEpoch: 7, OwnerType: "tool_call", OwnerID: firstTool, Kind: "tool_result", RetentionKind: "generated", SizeBytes: int64(len(body)), SHA256: hash[:], MediaType: "text/plain"}
	file, replayID, err := store.BeginUpload(context.Background(), header)
	if err != nil || replayID != 0 {
		t.Fatalf("begin replay=%d err=%v", replayID, err)
	}
	if _, err := file.WriteString(body); err != nil {
		t.Fatal(err)
	}
	// Force the reference transaction to fail only after filesystem installation.
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF; DROP TABLE artifacts`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitUpload(context.Background(), header, file); err == nil {
		t.Fatal("reference failure unexpectedly committed")
	}
	blob := filepath.Join(store.dir, "blobs", digest+".blob")
	got, err := os.ReadFile(blob)
	if err != nil || string(got) != body {
		t.Fatalf("shared blob was deleted or altered: body=%q err=%v", got, err)
	}
}

func TestConcurrentSameUploadIDLinearizesToOneArtifact(t *testing.T) {
	db := newTestDB(t)
	store, err := NewStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	attemptID, toolCallID := seedToolOwner(t, db)
	header := interruptedUploadHeader(attemptID, toolCallID, "linearized body")
	first, replayID, err := store.BeginUpload(context.Background(), header)
	if err != nil || replayID != 0 {
		t.Fatalf("first begin replay=%d err=%v", replayID, err)
	}
	if _, err := first.WriteString("linearized body"); err != nil {
		t.Fatal(err)
	}
	type beginResult struct {
		file   *os.File
		replay int64
		err    error
	}
	secondStarted := make(chan struct{})
	secondResult := make(chan beginResult, 1)
	go func() {
		close(secondStarted)
		file, replay, beginErr := store.BeginUpload(context.Background(), header)
		secondResult <- beginResult{file: file, replay: replay, err: beginErr}
	}()
	<-secondStarted
	select {
	case result := <-secondResult:
		t.Fatalf("concurrent BeginUpload bypassed same-id lease: %+v", result)
	case <-time.After(25 * time.Millisecond):
	}
	artifactID, err := store.CommitUpload(context.Background(), header, first)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-secondResult:
		if result.err != nil || result.file != nil || result.replay != artifactID {
			t.Fatalf("second begin=%+v want committed replay=%d", result, artifactID)
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent BeginUpload did not resume after commit")
	}
	var artifacts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM artifacts`).Scan(&artifacts); err != nil || artifacts != 1 {
		t.Fatalf("artifacts=%d err=%v", artifacts, err)
	}
}

func TestExistingDigestBlobMustVerifyBeforeReferenceCommit(t *testing.T) {
	db := newTestDB(t)
	store, err := NewStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	attemptID, toolCallID := seedToolOwner(t, db)
	header := interruptedUploadHeader(attemptID, toolCallID, "expected body")
	if err := os.WriteFile(store.blobPath(hex.EncodeToString(header.SHA256)), []byte("corrupt body"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, replayID, err := store.BeginUpload(context.Background(), header)
	if err != nil || replayID != 0 {
		t.Fatalf("begin replay=%d err=%v", replayID, err)
	}
	if _, err := file.WriteString("expected body"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitUpload(context.Background(), header, file); err == nil {
		t.Fatal("corrupt existing blob was accepted")
	}
}
