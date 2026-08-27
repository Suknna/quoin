package app

import (
	"context"
	"errors"

	"io"
	"os"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/quoin/artifact"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ArtifactService gRPC surface: the artifact store adapters and wire mappers.

// artifactService adapts the artifact store to the frozen ArtifactService
// (RUNTIME-UPLOAD-001..006, RUNTIME-ARTIFACT-001..004).
// maxRuntimeArtifactUploadBytes is an explicit server-side safety bound for one
// streamed Runtime Artifact. The header is validated before a staging file is
// allocated, so a peer cannot reserve unbounded disk merely by declaring size.
const maxRuntimeArtifactUploadBytes = 64 << 20

type artifactService struct {
	runtimev1.UnimplementedArtifactServiceServer
	Slots     *qruntime.Service
	Artifacts *artifact.Store
}

// RegisterArtifactService mounts the service on the runtime gRPC server.
func RegisterArtifactService(server *grpc.Server, service *artifactService) {
	runtimev1.RegisterArtifactServiceServer(server, service)
}

// NewArtifactService builds the artifact gRPC adapter.
func NewArtifactService(slots *qruntime.Service, store *artifact.Store) *artifactService {
	return &artifactService{Slots: slots, Artifacts: store}
}

// plinthArtifactFence retains Plinth's supervisor-only text Artifact access.
func (service *artifactService) plinthArtifactFence(ctx context.Context) error {
	if !service.Slots.ValidateBearer(ctx, bearerFromContext(ctx), qruntime.SlotPlinth) {
		return status.Error(codes.Unauthenticated, "plinth runtime bearer required")
	}
	return nil
}

// uploadRuntimeSlot authenticates the upload and returns the only principal
// whose identity may be passed to the Artifact ledger. Lintel is deliberately
// constrained to browser-owned trace and screenshot uploads; it never gains
// Artifact text read/search access or generic Artifact write authority.
func (service *artifactService) uploadRuntimeSlot(ctx context.Context, header *runtimev1.ArtifactUploadHeader) (string, error) {
	if header == nil || header.GetBootId() == "" || header.GetConnectionEpoch() == 0 {
		return "", status.Error(codes.Unauthenticated, "live runtime stream fence required")
	}
	bearer := bearerFromContext(ctx)
	for _, slot := range []string{qruntime.SlotPlinth, qruntime.SlotLintel} {
		if !service.Slots.ValidateBearer(ctx, bearer, slot) {
			continue
		}
		// A long-lived bearer alone does not authorize data-plane writes. The
		// header must name the slot's currently attached control stream, so a
		// revoked/replaced stream cannot commit an upload using an old token.
		if service.Slots.WithCurrent(slot, header.GetBootId(), header.GetConnectionEpoch(), func() error { return nil }) != nil {
			return "", status.Error(codes.Unauthenticated, "runtime stream is no longer current")
		}
		if slot == qruntime.SlotLintel && (header.GetOwnerType() != "browser_operation" || header.GetRetentionKind() != runtimev1.RetentionKind_RETENTION_KIND_GENERATED ||
			(header.GetKind() != runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE && header.GetKind() != runtimev1.ArtifactKind_ARTIFACT_KIND_SCREENSHOT) ||
			(header.GetKind() == runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE && (!header.GetSensitive() || (header.GetTraceIntegrity() != runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE && header.GetTraceIntegrity() != runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE))) ||
			(header.GetKind() != runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE && header.GetTraceIntegrity() != runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_UNSPECIFIED)) {
			return "", status.Error(codes.PermissionDenied, "lintel may upload only generated browser trace or screenshot artifacts")
		}
		return slot, nil
	}
	return "", status.Error(codes.Unauthenticated, "runtime bearer required")
}

// Upload consumes one client-stream upload (RUNTIME-UPLOAD-001..006).
func (service *artifactService) Upload(stream runtimev1.ArtifactService_UploadServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return status.Error(codes.InvalidArgument, "header frame required")
	}
	headerFrame := first.GetHeader()
	if headerFrame == nil {
		return status.Error(codes.InvalidArgument, "first frame must be a header")
	}
	runtimeSlot, err := service.uploadRuntimeSlot(ctx, headerFrame)
	if err != nil {
		return err
	}
	if headerFrame.GetSizeBytes() > maxRuntimeArtifactUploadBytes {
		return status.Error(codes.InvalidArgument, "Artifact upload exceeds fixed size limit")
	}
	header := artifact.UploadHeader{
		RuntimeSlot: runtimeSlot,
		UploadID:    headerFrame.GetUploadId(), AttemptID: headerFrame.GetAttemptId(),
		BootID: headerFrame.GetBootId(), ConnectionEpoch: headerFrame.GetConnectionEpoch(),
		OwnerType: headerFrame.GetOwnerType(), OwnerID: headerFrame.GetOwnerId(),
		Kind: artifactKindOf(headerFrame.GetKind()), RetentionKind: retentionKindOf(headerFrame.GetRetentionKind()),
		Sensitive: headerFrame.GetSensitive(), SizeBytes: int64(headerFrame.GetSizeBytes()),
		SHA256: headerFrame.GetSha256(), MediaType: headerFrame.GetMediaType(),
		TraceIntegrity: traceIntegrityOf(headerFrame.GetTraceIntegrity()),
	}
	file, replayID, err := service.Artifacts.BeginUpload(ctx, header)
	if err != nil {
		var rejection *artifact.Rejection
		if errors.As(err, &rejection) {
			err = stream.SendAndClose(&runtimev1.ArtifactUploadResult{
				UploadId: header.UploadID, Committed: false,
				RejectReason: runtimev1.UploadRejectReason(uploadRejectValue(rejection.Reason)),
			})
			return nil
		}
		return status.Error(codes.Internal, "upload begin failed")
	}
	committed := false
	defer func() {
		if !committed {
			if file != nil {
				_ = file.Close()
			}
			service.Artifacts.AbortUpload(header.UploadID)
		}
	}()
	if replayID != 0 {
		// The caller cannot know whether the prior CloseAndRecv response crossed
		// the network. Drain this full retry before replying so the normal client
		// stream state machine may resend header/chunks/end unchanged.
		for {
			_, receiveErr := stream.Recv()
			if errors.Is(receiveErr, io.EOF) {
				break
			}
			if receiveErr != nil {
				return receiveErr
			}
		}
		err = stream.SendAndClose(&runtimev1.ArtifactUploadResult{
			UploadId: header.UploadID, Committed: true, ArtifactId: replayID,
		})
		committed = err == nil
		return err
	}
	var offset uint64
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			// End is preferred, but EOF remains an accepted stream terminator.
			break
		}
		if err != nil {
			file.Close()
			_ = os.RemoveAll(service.Artifacts.StagingPath(header.UploadID))
			return err
		}
		switch payload := frame.GetFrame().(type) {
		case *runtimev1.ArtifactUploadFrame_Chunk:
			chunk := payload.Chunk
			if chunk.GetOffset() != offset {
				file.Close()
				err = stream.SendAndClose(&runtimev1.ArtifactUploadResult{
					UploadId: header.UploadID, RejectReason: runtimev1.UploadRejectReason_UPLOAD_REJECT_REASON_METADATA_MISMATCH,
				})
				return nil
			}
			if _, err := file.Write(chunk.GetPayload()); err != nil {
				file.Close()
				return status.Error(codes.Internal, "staging write failed")
			}
			offset += uint64(len(chunk.GetPayload()))
			if offset > headerFrame.GetSizeBytes() {
				file.Close()
				return status.Error(codes.InvalidArgument, "Artifact upload exceeds declared size")
			}
		case *runtimev1.ArtifactUploadFrame_End:
			goto finish
		default:
			file.Close()
			return status.Error(codes.InvalidArgument, "unexpected upload frame")
		}
	}
finish:
	artifactID, err := service.Artifacts.CommitUpload(ctx, header, file)
	if err != nil {
		var rejection *artifact.Rejection
		if errors.As(err, &rejection) {
			err = stream.SendAndClose(&runtimev1.ArtifactUploadResult{
				UploadId: header.UploadID, RejectReason: runtimev1.UploadRejectReason(uploadRejectValue(rejection.Reason)),
			})
			return nil
		}
		return status.Error(codes.Internal, "upload commit failed")
	}
	err = stream.SendAndClose(&runtimev1.ArtifactUploadResult{
		UploadId: header.UploadID, Committed: true, ArtifactId: artifactID,
	})
	committed = err == nil
	return err
}

// ReadText serves one attempt-scoped bounded read (RUNTIME-ARTIFACT-002/003).
func (service *artifactService) ReadText(ctx context.Context, request *runtimev1.ArtifactReadTextRequest) (*runtimev1.ArtifactReadTextResponse, error) {
	if err := service.plinthArtifactFence(ctx); err != nil {
		return nil, err
	}
	slice, err := service.Artifacts.ReadText(ctx, request.GetAttemptId(), request.GetArtifactId(), request.GetBootId(), request.GetConnectionEpoch(), int64(request.GetStartLine()), int(request.GetMaxLines()))
	if err != nil {
		if errors.Is(err, artifact.ErrBodyExpired) {
			return nil, status.Error(codes.FailedPrecondition, "ARTIFACT_BODY_EXPIRED")
		}
		return nil, status.Error(codes.PermissionDenied, "artifact read denied")
	}
	return &runtimev1.ArtifactReadTextResponse{
		ArtifactId: slice.ArtifactID, Content: slice.Content, StartLine: uint64(slice.StartLine),
		NextLine: uint64(slice.NextLine), Eof: slice.EOF, TotalSizeBytes: uint64(slice.TotalSizeBytes),
		TotalLines: uint64(slice.TotalLines), Sha256: slice.SHA256, MediaType: slice.MediaType,
	}, nil
}

// GrepText serves one attempt-scoped bounded RE2 search (RUNTIME-ARTIFACT-003).
func (service *artifactService) GrepText(ctx context.Context, request *runtimev1.ArtifactGrepTextRequest) (*runtimev1.ArtifactGrepTextResponse, error) {
	if err := service.plinthArtifactFence(ctx); err != nil {
		return nil, err
	}
	result, err := service.Artifacts.GrepText(ctx, request.GetAttemptId(), request.GetArtifactId(), request.GetBootId(), request.GetConnectionEpoch(), request.GetRe2Pattern(), int(request.GetMaxMatches()), int(request.GetContextLines()))
	if err != nil {
		if errors.Is(err, artifact.ErrBodyExpired) {
			return nil, status.Error(codes.FailedPrecondition, "ARTIFACT_BODY_EXPIRED")
		}
		return nil, status.Error(codes.PermissionDenied, "artifact grep denied")
	}
	var matches []*runtimev1.ArtifactTextMatch
	for _, match := range result.Matches {
		matches = append(matches, &runtimev1.ArtifactTextMatch{
			Line: uint64(match.LineNumber), Content: []byte(match.Line),
		})
	}
	return &runtimev1.ArtifactGrepTextResponse{
		ArtifactId: result.ArtifactID, Matches: matches, Truncated: result.Truncated,
		TotalSizeBytes: uint64(result.TotalSizeBytes), TotalLines: uint64(result.TotalLines),
		Sha256: result.SHA256, MediaType: result.MediaType,
	}, nil
}

func artifactKindOf(kind runtimev1.ArtifactKind) string {
	switch kind {
	case runtimev1.ArtifactKind_ARTIFACT_KIND_ATTACHMENT:
		return "attachment"
	case runtimev1.ArtifactKind_ARTIFACT_KIND_SCREENSHOT:
		return "screenshot"
	case runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE:
		return "trace"
	case runtimev1.ArtifactKind_ARTIFACT_KIND_TOOL_RESULT:
		return "tool_result"
	case runtimev1.ArtifactKind_ARTIFACT_KIND_REPORT_FILE:
		return "report_file"
	default:
		return ""
	}
}

func retentionKindOf(kind runtimev1.RetentionKind) string {
	if kind == runtimev1.RetentionKind_RETENTION_KIND_LONG_TERM {
		return "long_term"
	}
	return "generated"
}

func uploadRejectValue(reason artifact.RejectReason) int32 {
	switch reason {
	case artifact.RejectSHA256Mismatch:
		return int32(runtimev1.UploadRejectReason_UPLOAD_REJECT_REASON_SHA256_MISMATCH)
	case artifact.RejectSizeMismatch:
		return int32(runtimev1.UploadRejectReason_UPLOAD_REJECT_REASON_SIZE_MISMATCH)
	case artifact.RejectAttemptNotRunning:
		return int32(runtimev1.UploadRejectReason_UPLOAD_REJECT_REASON_ATTEMPT_NOT_RUNNING)
	case artifact.RejectBootMismatch:
		return int32(runtimev1.UploadRejectReason_UPLOAD_REJECT_REASON_BOOT_MISMATCH)
	case artifact.RejectEpochMismatch:
		return int32(runtimev1.UploadRejectReason_UPLOAD_REJECT_REASON_EPOCH_MISMATCH)
	case artifact.RejectMetadataMismatch:
		return int32(runtimev1.UploadRejectReason_UPLOAD_REJECT_REASON_METADATA_MISMATCH)
	default:
		return int32(runtimev1.UploadRejectReason_UPLOAD_REJECT_REASON_INTERNAL)
	}
}

func traceIntegrityOf(integrity runtimev1.BrowserTraceIntegrity) string {
	switch integrity {
	case runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE:
		return "complete"
	case runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE:
		return "incomplete"
	default:
		return ""
	}
}
