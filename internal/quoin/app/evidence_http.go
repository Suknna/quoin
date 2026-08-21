package app

// Evidence and Artifact HTTP surface (T11): the frozen getEvidence /
// getArtifactMetadata / downloadArtifactContent routes. Evidence reads are
// available to every logged-in user (HTTP-PERM-001); artifact downloads
// authorize through the logical artifacts row (DATA-ARTIFACT-003), refuse
// expired bodies with 410 and gate sensitive bodies behind Admin plus an
// audit event (HTTP-FILE-003/004/005).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/evidence"
	"github.com/danielgtaylor/huma/v2"
)

type evidenceDetailBody struct {
	Body evidence.View `json:"body"`
}

type artifactMetadataBody struct {
	Body artifactMetadata `json:"body"`
}

// artifactMetadata is the frozen ArtifactSummary response projection.
type artifactMetadata struct {
	ID            string  `json:"id"`
	Kind          string  `json:"kind"`
	Sensitive     bool    `json:"sensitive"`
	RetentionKind string  `json:"retentionKind"`
	OwnerType     string  `json:"ownerType"`
	OwnerID       string  `json:"ownerId"`
	SizeBytes     int64   `json:"sizeBytes"`
	SHA256        string  `json:"sha256"`
	BodyExpired   bool    `json:"bodyExpired"`
	ExpiresAt     *string `json:"expiresAt,omitempty"`
	CreatedAt     string  `json:"createdAt"`
}

func (application *apiServer) registerEvidenceRoutes(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/evidence/{evidenceId}", OperationID: "getEvidence"}, application.getEvidence)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/artifacts/{artifactId}", OperationID: "getArtifactMetadata"}, application.getArtifactMetadata)
}

// getEvidence returns the immutable detail of one Evidence row.
func (application *apiServer) getEvidence(ctx context.Context, input *struct {
	Session    string `cookie:"__Host-quoin-session"`
	EvidenceID string `path:"evidenceId"`
}) (*evidenceDetailBody, error) {
	if _, err := application.authenticateFull(ctx, input.Session, "读取证据"); err != nil {
		return nil, err
	}
	evidenceID, err := strconv.ParseInt(input.EvidenceID, 10, 64)
	if err != nil || evidenceID <= 0 {
		return nil, huma.Error404NotFound("证据不存在", nil)
	}
	detail, err := application.analyses.Evidence().Get(ctx, evidenceID)
	if err != nil {
		if errors.Is(err, evidence.ErrNotFound) {
			return nil, huma.Error404NotFound("证据不存在", nil)
		}
		return nil, huma.Error500InternalServerError("无法读取证据", err)
	}
	return &evidenceDetailBody{Body: detail}, nil
}

// getArtifactMetadata returns the logical metadata of one artifact.
func (application *apiServer) getArtifactMetadata(ctx context.Context, input *struct {
	Session    string `cookie:"__Host-quoin-session"`
	ArtifactID string `path:"artifactId"`
}) (*artifactMetadataBody, error) {
	if _, err := application.authenticateFull(ctx, input.Session, "读取产物信息"); err != nil {
		return nil, err
	}
	meta, err := application.artifactMetadata(ctx, input.ArtifactID)
	if err != nil {
		return nil, err
	}
	return &artifactMetadataBody{Body: meta}, nil
}

// artifactMetadata loads and projects one artifact row (shared by the
// metadata and evidence read paths).
func (application *apiServer) artifactMetadata(ctx context.Context, artifactIDText string) (artifactMetadata, error) {
	artifactID, err := strconv.ParseInt(artifactIDText, 10, 64)
	if err != nil || artifactID <= 0 {
		return artifactMetadata{}, huma.Error404NotFound("产物不存在", nil)
	}
	if application.artifacts == nil {
		return artifactMetadata{}, huma.Error500InternalServerError("产物存储不可用", nil)
	}
	meta, err := application.artifacts.Metadata(ctx, artifactID)
	if err != nil {
		if errors.Is(err, artifact.ErrNotFound) {
			return artifactMetadata{}, huma.Error404NotFound("产物不存在", nil)
		}
		return artifactMetadata{}, huma.Error500InternalServerError("无法读取产物", err)
	}
	return artifactMetadata{
		ID:            strconv.FormatInt(meta.ID, 10),
		Kind:          meta.Kind,
		Sensitive:     meta.Sensitive,
		RetentionKind: meta.RetentionKind,
		OwnerType:     meta.OwnerType,
		OwnerID:       strconv.FormatInt(meta.OwnerID, 10),
		SizeBytes:     meta.SizeBytes,
		SHA256:        meta.SHA256,
		BodyExpired:   meta.BodyExpired,
		ExpiresAt:     meta.ExpiresAt,
		CreatedAt:     meta.CreatedAt,
	}, nil
}

// downloadArtifactContent streams one live artifact body with the frozen
// download headers (HTTP-FILE-003). Sensitive bodies require Admin and an
// audit event before any byte leaves (HTTP-FILE-004); expired bodies
// answer 410 with their metadata intact (HTTP-FILE-005).
func (application *apiServer) downloadArtifactContent(writer http.ResponseWriter, request *http.Request) {
	artifactID, err := strconv.ParseInt(request.PathValue("artifactId"), 10, 64)
	if err != nil || artifactID <= 0 {
		writeStreamProblem(writer, http.StatusNotFound, "产物不存在")
		return
	}
	cookie, ok := findSessionCookie(request)
	if !ok {
		writeStreamProblem(writer, http.StatusUnauthorized, "请重新登录")
		return
	}
	session, err := application.auth.Authenticate(request.Context(), cookie)
	if err != nil {
		writeStreamProblem(writer, http.StatusUnauthorized, "请重新登录")
		return
	}
	if application.artifacts == nil {
		writeStreamProblem(writer, http.StatusInternalServerError, "产物存储不可用")
		return
	}
	meta, err := application.artifacts.Metadata(request.Context(), artifactID)
	if err != nil {
		if errors.Is(err, artifact.ErrNotFound) {
			writeStreamProblem(writer, http.StatusNotFound, "产物不存在")
			return
		}
		writeStreamProblem(writer, http.StatusInternalServerError, "无法读取产物")
		return
	}
	if meta.Sensitive && session.User.Role != "admin" {
		writeStreamProblem(writer, http.StatusForbidden, "该产物仅管理员可下载")
		return
	}
	if err := application.artifacts.RecordDownloadAudit(request.Context(), "user", session.User.ID, artifactID); err != nil {
		writeStreamProblem(writer, http.StatusInternalServerError, "下载审计失败，请重试")
		return
	}
	file, meta, err := application.artifacts.OpenBody(request.Context(), artifactID)
	if err != nil {
		if errors.Is(err, artifact.ErrBodyExpired) {
			writeStreamProblem(writer, http.StatusGone, "产物正文已过期；元数据仍保留，可查看采集时间与来源")
			return
		}
		writeStreamProblem(writer, http.StatusInternalServerError, "无法读取产物正文")
		return
	}
	defer file.Close()
	// The filename is server-generated from the stable locator and never
	// derived from client input (HTTP-FILE-003).
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="artifact-%d"`, meta.ID))
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Pragma", "no-cache")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	writer.Header().Set("ETag", `"`+meta.SHA256+`"`)
	writer.WriteHeader(http.StatusOK)
	if _, err := io.Copy(writer, file); err != nil {
		// The head is already out; a mid-body transport error cannot be
		// re-expressed as a status code (HTTP-FILE-007 forbids silent
		// truncation, so the log carries the locator for diagnosis).
		sharedops.LogEvent("quoin", "error", "artifact.download_stream_failed", fmt.Sprintf("artifact=%d %v", meta.ID, err))
	}
}
