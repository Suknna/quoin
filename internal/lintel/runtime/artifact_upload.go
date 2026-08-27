package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
)

const (
	artifactUploadChunkBytes = 64 * 1024
	// An upload ID is an immutable idempotency capability. Retrying the exact
	// request is safe, but an unavailable Runtime must not pin a browser slot
	// indefinitely.
	maxBrowserArtifactUploadAttempts = 3
	browserArtifactUploadRetryDelay  = 50 * time.Millisecond
)

// browserArtifactUploadDeadline is a package variable only so the bounded
// timeout can be exercised without waiting ten seconds in a unit test.
var browserArtifactUploadDeadline = 10 * time.Second

type artifactUploadBinding struct {
	client  runtimev1.ArtifactServiceClient
	context context.Context
	epoch   uint64
}

// BrowserArtifactUpload is the only Artifact upload request accepted from a
// Lintel browser operation. It intentionally has no generic owner or Artifact
// kind field: the caller can choose only the two browser-owned kinds.
type BrowserArtifactUpload struct {
	OperationID    int64
	ChildAttemptID int64
	Kind           runtimev1.ArtifactKind
	Body           []byte
	SHA256         []byte
	MediaType      string
	Sensitive      bool
	// TraceIntegrity is mandatory for trace uploads and is checked by Quoin at
	// the Artifact commit transaction; screenshots leave it unspecified.
	TraceIntegrity runtimev1.BrowserTraceIntegrity
}

func (channel *Channel) installArtifactUploadBinding(binding artifactUploadBinding) {
	channel.artifactMu.Lock()
	defer channel.artifactMu.Unlock()
	channel.artifactBinding = binding
}

func (channel *Channel) removeArtifactUploadBinding(epoch uint64) {
	channel.artifactMu.Lock()
	defer channel.artifactMu.Unlock()
	if channel.artifactBinding.epoch == epoch {
		channel.artifactBinding = artifactUploadBinding{}
	}
}

func (channel *Channel) currentArtifactUploadBinding() (artifactUploadBinding, bool) {
	channel.artifactMu.RLock()
	defer channel.artifactMu.RUnlock()
	binding := channel.artifactBinding
	return binding, binding.client != nil && binding.context != nil && binding.epoch != 0
}

// UploadBrowserArtifact uploads a completed browser trace or screenshot over
// the already-authenticated Runtime gRPC connection. It never returns a made-up
// Artifact ID: rejections and transport failures stay errors for the caller to
// map to the operation's terminal failure path.
func (channel *Channel) UploadBrowserArtifact(ctx context.Context, upload BrowserArtifactUpload) (int64, error) {
	var lastErr error
retry:
	for attempt := 0; attempt < maxBrowserArtifactUploadAttempts; attempt++ {
		attemptContext, cancel := context.WithTimeout(ctx, browserArtifactUploadDeadline)
		artifactID, err := channel.uploadBrowserArtifactOnce(attemptContext, upload)
		cancel()
		if err == nil {
			return artifactID, nil
		}
		lastErr = err
		if ctx.Err() != nil || attempt+1 == maxBrowserArtifactUploadAttempts {
			break
		}
		select {
		case <-ctx.Done():
			// A cancellation must not start another same-capability upload after
			// the caller withdrew the terminal action.
			break retry
		case <-time.After(browserArtifactUploadRetryDelay):
		}
	}
	return 0, lastErr
}

func (channel *Channel) uploadBrowserArtifactOnce(ctx context.Context, upload BrowserArtifactUpload) (int64, error) {
	if upload.OperationID < 1 || upload.MediaType == "" || (upload.ChildAttemptID < 1 && upload.Kind != runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE) {
		return 0, errors.New("browser Artifact upload requires operation, media type, and a child attempt unless it is an operation trace")
	}
	if upload.Kind != runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE && upload.Kind != runtimev1.ArtifactKind_ARTIFACT_KIND_SCREENSHOT {
		return 0, errors.New("Lintel may upload only browser trace or screenshot artifacts")
	}
	if upload.Kind == runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE && !upload.Sensitive {
		return 0, errors.New("browser trace artifacts must be sensitive")
	}
	if upload.Kind == runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE && upload.TraceIntegrity != runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE && upload.TraceIntegrity != runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE {
		return 0, errors.New("browser trace uploads require an explicit integrity")
	}
	if upload.Kind != runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE && upload.TraceIntegrity != runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_UNSPECIFIED {
		return 0, errors.New("only browser trace uploads may declare integrity")
	}
	digest := sha256.Sum256(upload.Body)
	if len(upload.SHA256) != sha256.Size || !bytes.Equal(upload.SHA256, digest[:]) {
		return 0, errors.New("browser Artifact body digest mismatch")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	binding, ok := channel.currentArtifactUploadBinding()
	if !ok {
		return 0, errors.New("Lintel Artifact upload connection is unavailable")
	}
	uploadContext, cancel := context.WithCancel(binding.context)
	stopCallerCancellation := context.AfterFunc(ctx, cancel)
	defer func() {
		stopCallerCancellation()
		cancel()
	}()
	stream, err := binding.client.Upload(uploadContext)
	if err != nil {
		return 0, fmt.Errorf("open browser Artifact upload: %w", err)
	}
	// Integrity is part of a trace upload's immutable identity. A cancellation
	// after a committed complete close must create a separate INCOMPLETE Artifact
	// even if its bounded trace projection happens to have identical bytes.
	uploadID := fmt.Sprintf("lintel-browser-%d-%d-%s-%s-%x", upload.OperationID, upload.ChildAttemptID, upload.Kind.String(), upload.TraceIntegrity.String(), digest)
	header := &runtimev1.ArtifactUploadHeader{
		UploadId:        uploadID,
		AttemptId:       upload.ChildAttemptID,
		BootId:          channel.bootID,
		ConnectionEpoch: binding.epoch,
		OwnerType:       "browser_operation",
		OwnerId:         upload.OperationID,
		Kind:            upload.Kind,
		RetentionKind:   runtimev1.RetentionKind_RETENTION_KIND_GENERATED,
		Sensitive:       upload.Sensitive,
		SizeBytes:       uint64(len(upload.Body)),
		Sha256:          append([]byte(nil), digest[:]...),
		MediaType:       upload.MediaType,
		TraceIntegrity:  upload.TraceIntegrity,
	}
	if err := stream.Send(&runtimev1.ArtifactUploadFrame{Frame: &runtimev1.ArtifactUploadFrame_Header{Header: header}}); err != nil {
		return 0, fmt.Errorf("send browser Artifact header: %w", err)
	}
	finishEarly := func(sendErr error) (int64, error) {
		result, receiveErr := stream.CloseAndRecv()
		if receiveErr == nil && result.GetCommitted() && result.GetArtifactId() > 0 {
			return result.GetArtifactId(), nil
		}
		return 0, sendErr
	}
	for offset := 0; offset < len(upload.Body); {
		end := offset + artifactUploadChunkBytes
		if end > len(upload.Body) {
			end = len(upload.Body)
		}
		if err := stream.Send(&runtimev1.ArtifactUploadFrame{Frame: &runtimev1.ArtifactUploadFrame_Chunk{Chunk: &runtimev1.ArtifactUploadChunk{Offset: uint64(offset), Payload: upload.Body[offset:end]}}}); err != nil {
			return finishEarly(fmt.Errorf("send browser Artifact chunk: %w", err))
		}
		offset = end
	}
	if err := stream.Send(&runtimev1.ArtifactUploadFrame{Frame: &runtimev1.ArtifactUploadFrame_End{End: &runtimev1.ArtifactUploadEnd{UploadedSizeBytes: uint64(len(upload.Body))}}}); err != nil {
		return finishEarly(fmt.Errorf("send browser Artifact end: %w", err))
	}
	result, err := stream.CloseAndRecv()
	if err != nil {
		return 0, fmt.Errorf("finish browser Artifact upload: %w", err)
	}
	if !result.GetCommitted() || result.GetArtifactId() < 1 {
		return 0, fmt.Errorf("browser Artifact upload rejected: %s", result.GetRejectReason().String())
	}
	return result.GetArtifactId(), nil
}
