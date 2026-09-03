package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/verification/deployment"
	"github.com/danielgtaylor/huma/v2"
)

// verificationArtifacts adapts the durable artifact store to the Deployment
// Acceptance service. The store owns every blob file; the rows join one
// dedicated writer connection per artifact.
type verificationArtifacts struct {
	store *artifact.Store
	db    *sql.DB
}

func (adapter verificationArtifacts) CommitVerificationArtifact(ctx context.Context, kind, mediaType, retentionKind string, ownerID int64, body []byte, createdAt time.Time) (int64, string, error) {
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	conn, err := adapter.db.Conn(ctx)
	if err != nil {
		return 0, "", err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return 0, "", err
	}
	artifactID, digest, err := adapter.store.CommitInlineArtifact(ctx, conn, kind, mediaType, retentionKind, "verification_invocation", ownerID, body, createdAt)
	if err != nil {
		_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		return 0, "", err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return 0, "", err
	}
	return artifactID, digest, nil
}

// mapVerificationError maps the deployment service error families onto the
// frozen problem envelope.
func mapVerificationError(err error) error {
	if err == nil {
		return nil
	}
	var serviceErr *deployment.Error
	if errors.As(err, &serviceErr) {
		switch serviceErr.Family {
		case "not_found":
			return problem(http.StatusNotFound, "not_found", "目标验收记录不存在。")
		case "invalid":
			return problem(http.StatusBadRequest, "malformed_request", "请求与冻结的验收条目不匹配，请刷新后重试。")
		case "conflict":
			return problem(http.StatusConflict, "verification_conflict", serviceErr.Message)
		case "unavailable":
			return problem(http.StatusServiceUnavailable, "deployment_acceptance_unavailable", serviceErr.Message)
		default:
			return problem(http.StatusConflict, "verification_in_progress", serviceErr.Message)
		}
	}
	return problem(http.StatusInternalServerError, "unavailable", "暂时无法完成操作，请稍后重试。")
}

// registerVerificationRoutes mounts the frozen Deployment Acceptance family
// (HTTP-VERIFY-001..004). The YAML helper-request download owns its response
// head and is mounted on the raw mux by NewHandler.
func (application *apiServer) registerVerificationRoutes(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/deployment-verifications", OperationID: "listDeploymentVerifications"}, application.listDeploymentVerifications)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/deployment-verifications", OperationID: "startDeploymentVerification"}, application.startDeploymentVerification)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/deployment-verifications/{invocationId}", OperationID: "getDeploymentVerification"}, application.getDeploymentVerification)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/deployment-verifications/{invocationId}/cancel", OperationID: "cancelDeploymentVerification"}, application.cancelDeploymentVerification)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/deployment-verifications/{invocationId}/helper-reports", OperationID: "importDeploymentVerificationHelperReport"}, application.importDeploymentVerificationHelperReport)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/deployment-verifications/{invocationId}/observations", OperationID: "submitDeploymentVerificationObservation"}, application.submitDeploymentVerificationObservation)
}

func (application *apiServer) verificationReader(ctx context.Context, cookie string) (auth.Session, error) {
	return application.authenticateFull(ctx, cookie, "读取部署验收")
}

func (application *apiServer) verificationAdmin(ctx context.Context, cookie string) (auth.Session, error) {
	return application.authenticateAdmin(ctx, cookie, "管理部署验收")
}

func (application *apiServer) listDeploymentVerifications(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Cursor  string `query:"cursor"`
	Limit   int    `query:"limit"`
}) (*struct {
	CacheControl string                     `header:"Cache-Control"`
	Body         deploymentListResponseBody `json:"body"`
}, error) {
	if _, err := application.verificationReader(ctx, input.Session); err != nil {
		return nil, err
	}
	after := int64(0)
	if input.Cursor != "" {
		decoded, err := strconv.ParseInt(input.Cursor, 10, 64)
		if err != nil || decoded < 0 {
			return nil, problem(http.StatusBadRequest, "malformed_request", "分页游标无效。")
		}
		after = decoded
	}
	page, next, err := application.verifications.List(ctx, after, input.Limit)
	if err != nil {
		return nil, mapVerificationError(err)
	}
	body := deploymentListResponseBody{Items: page}
	if next > 0 {
		body.NextCursor = strconv.FormatInt(next, 10)
	}
	return &struct {
		CacheControl string                     `header:"Cache-Control"`
		Body         deploymentListResponseBody `json:"body"`
	}{CacheControl: "no-store", Body: body}, nil
}

type deploymentListResponseBody struct {
	Items      []*deployment.VerificationDetail `json:"items"`
	NextCursor string                           `json:"nextCursor,omitempty"`
}

func (application *apiServer) startDeploymentVerification(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Body    struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
	}
}) (*struct {
	CacheControl string `header:"Cache-Control"`
	Status       int
	Body         *deployment.VerificationDetail `json:"body"`
}, error) {
	session, err := application.verificationAdmin(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	invocationID, err := application.verifications.Start(ctx, session.ID, session.User.ID, input.Body.ClientCommandID)
	if err != nil {
		return nil, mapVerificationError(err)
	}
	if application.verifications.Binding() == nil {
		return nil, mapVerificationError(application.verifications.ErrBindingUnavailable())
	}
	// Browser identity items own a real deployment verification operation in
	// the global FIFO; the physical admission stays with the runtime.
	if err := application.verifications.EnqueueBrowserOperations(ctx, invocationID); err != nil {
		return nil, mapVerificationError(err)
	}
	if application.browserOperationsDispatchFunc != nil {
		go application.browserOperationsDispatchFunc(context.Background())
	}
	detail, err := application.verifications.Load(ctx, invocationID)
	if err != nil {
		return nil, mapVerificationError(err)
	}
	// The frozen contract answers 202: the manifest is frozen and execution
	// has begun, but nothing is decided yet.
	return &struct {
		CacheControl string `header:"Cache-Control"`
		Status       int
		Body         *deployment.VerificationDetail `json:"body"`
	}{CacheControl: "no-store", Status: http.StatusAccepted, Body: detail}, nil
}

func (application *apiServer) getDeploymentVerification(ctx context.Context, input *struct {
	Session      string `cookie:"__Host-quoin-session"`
	InvocationID string `path:"invocationId"`
}) (*struct {
	CacheControl string                         `header:"Cache-Control"`
	Body         *deployment.VerificationDetail `json:"body"`
}, error) {
	if _, err := application.verificationReader(ctx, input.Session); err != nil {
		return nil, err
	}
	invocationID, locatorProblem := parseVerificationLocator(input.InvocationID)
	if locatorProblem != nil {
		return nil, locatorProblem
	}
	detail, err := application.verifications.Load(ctx, invocationID)
	if err != nil {
		return nil, mapVerificationError(err)
	}
	return &struct {
		CacheControl string                         `header:"Cache-Control"`
		Body         *deployment.VerificationDetail `json:"body"`
	}{CacheControl: "no-store", Body: detail}, nil
}

func (application *apiServer) cancelDeploymentVerification(ctx context.Context, input *struct {
	Session      string `cookie:"__Host-quoin-session"`
	InvocationID string `path:"invocationId"`
	Body         struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
	}
}) (*struct {
	CacheControl string                         `header:"Cache-Control"`
	Body         *deployment.VerificationDetail `json:"body"`
}, error) {
	session, err := application.verificationAdmin(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	invocationID, locatorProblem := parseVerificationLocator(input.InvocationID)
	if locatorProblem != nil {
		return nil, locatorProblem
	}
	detail, err := application.verifications.Cancel(ctx, session.ID, session.User.ID, invocationID, input.Body.ClientCommandID)
	if err != nil {
		return nil, mapVerificationError(err)
	}
	return &struct {
		CacheControl string                         `header:"Cache-Control"`
		Body         *deployment.VerificationDetail `json:"body"`
	}{CacheControl: "no-store", Body: detail}, nil
}

func (application *apiServer) importDeploymentVerificationHelperReport(ctx context.Context, input *struct {
	Session      string `cookie:"__Host-quoin-session"`
	InvocationID string `path:"invocationId"`
	RawBody      []byte `contentType:"application/yaml"`
}) (*struct {
	CacheControl string                         `header:"Cache-Control"`
	Body         *deployment.VerificationDetail `json:"body"`
}, error) {
	session, err := application.verificationAdmin(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	if session.ID == 0 {
		return nil, problem(http.StatusUnauthorized, "unauthenticated", "请重新登录。")
	}
	invocationID, locatorProblem := parseVerificationLocator(input.InvocationID)
	if locatorProblem != nil {
		return nil, locatorProblem
	}
	if len(input.RawBody) == 0 || len(input.RawBody) > 4<<20 {
		return nil, problem(http.StatusBadRequest, "malformed_request", "helper 报告为空或超过大小限制。")
	}
	detail, _, err := application.verifications.ImportHelperReport(ctx, invocationID, input.RawBody)
	if err != nil {
		return nil, mapVerificationError(err)
	}
	return &struct {
		CacheControl string                         `header:"Cache-Control"`
		Body         *deployment.VerificationDetail `json:"body"`
	}{CacheControl: "no-store", Body: detail}, nil
}

func (application *apiServer) submitDeploymentVerificationObservation(ctx context.Context, input *struct {
	Session      string `cookie:"__Host-quoin-session"`
	InvocationID string `path:"invocationId"`
	Body         struct {
		ClientCommandID      string  `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
		ItemID               string  `json:"itemId" pattern:"^[1-9][0-9]*$"`
		InputDigest          string  `json:"inputDigest" pattern:"^[a-f0-9]{64}$"`
		VisualResult         string  `json:"visualResult" enum:"passed,failed"`
		MotionResult         string  `json:"motionResult" enum:"passed,failed"`
		FocusOcclusionResult string  `json:"focusOcclusionResult" enum:"passed,failed"`
		Note                 *string `json:"note,omitempty" maxLength:"2000"`
	}
}) (*struct {
	CacheControl string `header:"Cache-Control"`
	Status       int
	Body         deploymentObservationBody `json:"body"`
}, error) {
	session, err := application.verificationAdmin(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	invocationID, locatorProblem := parseVerificationLocator(input.InvocationID)
	if locatorProblem != nil {
		return nil, locatorProblem
	}
	itemID, err := strconv.ParseInt(input.Body.ItemID, 10, 64)
	if err != nil || itemID <= 0 {
		return nil, problem(http.StatusBadRequest, "malformed_request", "验收条目无效。")
	}
	// Command replay for observations rides on the observation result
	// idempotency: identical typed fields return the original result.
	resultID, err := application.verifications.SubmitObservation(ctx, deployment.ObservationInput{
		InvocationID: invocationID, ItemID: itemID, InputDigest: input.Body.InputDigest,
		AdminSessionID: session.ID, VisualResult: input.Body.VisualResult,
		MotionResult: input.Body.MotionResult, FocusOcclusionResult: input.Body.FocusOcclusionResult,
		Note: input.Body.Note,
	})
	if err != nil {
		return nil, mapVerificationError(err)
	}
	return &struct {
		CacheControl string `header:"Cache-Control"`
		Status       int
		Body         deploymentObservationBody `json:"body"`
	}{CacheControl: "no-store", Status: http.StatusCreated, Body: deploymentObservationBody{ResultID: strconv.FormatInt(resultID, 10)}}, nil
}

type deploymentObservationBody struct {
	ResultID string `json:"id"`
}

// serveDeploymentVerificationHelperRequest is the raw-mux YAML download of
// the immutable offline helper request (exportDeploymentVerificationHelperRequest).
func (application *apiServer) serveDeploymentVerificationHelperRequest(writer http.ResponseWriter, request *http.Request) {
	cookie, ok := findSessionCookie(request)
	if !ok {
		writeVerificationProblemJSON(writer, http.StatusUnauthorized, "unauthenticated", "请重新登录后再导出 helper request。")
		return
	}
	if _, err := application.verificationAdmin(request.Context(), cookie); err != nil {
		writeVerificationProblemJSON(writer, http.StatusUnauthorized, "unauthenticated", "请重新登录后再导出 helper request。")
		return
	}
	invocationID, err := strconv.ParseInt(request.PathValue("invocationId"), 10, 64)
	if err != nil || invocationID <= 0 {
		writeVerificationProblemJSON(writer, http.StatusNotFound, "not_found", "目标验收记录不存在。")
		return
	}
	body, _, err := application.verifications.HelperRequest(request.Context(), invocationID)
	if err != nil {
		writeProblemStatus(writer, err)
		return
	}
	sum := sha256.Sum256(body)
	writer.Header().Set("Content-Type", "application/yaml")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Quoin-Request-Digest", hex.EncodeToString(sum[:]))
	writer.Header().Set("Content-Disposition", `attachment; filename="deployment-verification-request-`+strconv.FormatInt(invocationID, 10)+`.yaml"`)
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(body)
}

func writeProblemStatus(writer http.ResponseWriter, err error) {
	var serviceErr *deployment.Error
	if errors.As(err, &serviceErr) && serviceErr.Family == "not_found" {
		writeVerificationProblemJSON(writer, http.StatusNotFound, "not_found", "目标验收记录不存在。")
		return
	}
	writeVerificationProblemJSON(writer, http.StatusInternalServerError, "unavailable", "暂时无法完成操作，请稍后重试。")
}

// writeVerificationProblemJSON renders the frozen ErrorModel for the raw-mux
// verification downloads.
func writeVerificationProblemJSON(writer http.ResponseWriter, status int, code, message string) {
	writer.Header().Set("Content-Type", "application/problem+json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"code": code, "message": message, "retryable": status >= 500 || status == 429,
	})
}

func parseVerificationLocator(raw string) (int64, error) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, problem(http.StatusNotFound, "not_found", "目标验收记录不存在。")
	}
	return value, nil
}

// runDeploymentDeadlineSweeper periodically closes invocations whose
// eight-hour window is ending: not_run is synthesized for undecidable items
// at the deadline and the system receipt is written. Missed deadlines stay
// receiptless by contract (VERIFY-DA-TIME-001).
func (application *apiServer) runDeploymentDeadlineSweeper(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			application.deploymentDeadlineSweeper(ctx)
		}
	}
}

func (application *apiServer) deploymentDeadlineSweeper(ctx context.Context) {
	invocations := []int64{}
	rows, err := application.db.QueryContext(ctx, `SELECT m.id FROM verification_invocation_manifests m
		WHERE NOT EXISTS (SELECT 1 FROM verification_finalization_receipts r WHERE r.invocation_id=m.id)`)
	if err != nil {
		return
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return
		}
		invocations = append(invocations, id)
	}
	rows.Close()
	for _, invocationID := range invocations {
		_ = application.verifications.SweepDeadline(ctx, invocationID)
	}
}

// SetDeploymentVerificationBinding (re)binds the Deployment Acceptance
// authority to the deployment identity. Run calls it with the component
// configuration before the servers are built; tests use it to inject a
// frozen binding.
func (application *apiServer) SetDeploymentVerificationBinding(binding *contract.DeploymentBinding, publicOrigin string) {
	application.verifications = deployment.NewService(application.db, time.Now, binding, publicOrigin)
	// A rebinding (tests freeze fixture digests after construction) must not
	// drop the already-wired durable artifact adapter.
	if application.verificationAdapter.store != nil {
		application.verifications.SetArtifactStore(application.verificationAdapter)
	}
}

// WireVerificationArtifacts attaches the durable artifact store to the
// Deployment Acceptance authority. Run performs this once the store exists;
// the exported seam exists for handler-level wiring tests.
func (application *apiServer) WireVerificationArtifacts(store *artifact.Store) {
	adapter := verificationArtifacts{store: store, db: application.db}
	application.verificationAdapter = adapter
	application.verifications.SetArtifactStore(adapter)
}
