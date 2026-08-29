// Package appinspection owns the manual Inspection HTTP surface (T24): the
// mixed-plan run command over one published config version, the run
// list/detail reads with their immutable report projection, and the cancel
// fence. The Handler is a seam wired by the app package exactly like the
// configuration surface; it owns no SQL.
package appinspection

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/inspection"
	"github.com/Suknna/quoin/internal/quoin/tools/thanos"
	"github.com/danielgtaylor/huma/v2"
)

type runListing struct {
	Items      []inspection.RunSummary `json:"items"`
	NextCursor string                  `json:"nextCursor,omitempty"`
}

// Handler wires the inspection domain service into the HTTP surface.
type Handler struct {
	Inspections *inspection.Service
	// Authenticate resolves any full (non-restricted) session to its
	// principal id; Operators may run and read manual inspections (Q209).
	Authenticate func(ctx context.Context, cookie string) (int64, error)
	// DispatchInspections scans and dispatches committed queued inspection
	// work (created while the runtime is already connected).
	DispatchInspections func(ctx context.Context)
	// CancelDispatch delivers a committed cancellation fence to its owning
	// Runtime. Failed sends stay durable for reconciliation and never undo the
	// accepted user command.
	CancelDispatch func(ctx context.Context, attemptID int64) error
}

type problemError struct {
	status  int
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (p *problemError) Error() string  { return p.Message }
func (p *problemError) GetStatus() int { return p.status }
func noStore() string                  { return "no-store" }

func problem(status int, code, message string) *problemError {
	return &problemError{status: status, Code: code, Message: message}
}

func mapDomainError(err error) *problemError {
	var rejection *inspection.RejectionError
	if errors.As(err, &rejection) {
		status := http.StatusUnprocessableEntity
		switch rejection.Code {
		case "active_conflict", "row_version_conflict", "state_conflict":
			status = http.StatusConflict
		case "not_found", "empty_plan":
			status = http.StatusNotFound
		case "thanos_unavailable", "model_unavailable":
			status = http.StatusServiceUnavailable
		}
		return problem(status, rejection.Code, rejection.Detail)
	}
	if errors.Is(err, inspection.ErrNotFound) {
		return problem(http.StatusNotFound, "not_found", "巡检 Run 不存在")
	}
	if errors.Is(err, inspection.ErrCommandReused) {
		return problem(http.StatusConflict, "command_reused", "命令标识已用于其它请求，请更换后重试")
	}
	if errors.Is(err, thanos.ErrThanosUnavailable) || errors.Is(err, thanos.ErrGrantNotCurrent) {
		return problem(http.StatusServiceUnavailable, "thanos_unavailable", "尚无可用的 Thanos 指标连接，请先创建并启用连接后重试。")
	}
	return problem(http.StatusInternalServerError, "internal", "请求无法完成，请稍后重试")
}

func (handler *Handler) listRuns(ctx context.Context, input *struct {
	Session           string `cookie:"__Host-quoin-session"`
	BusinessSystemKey string `query:"businessSystemKey"`
	Cursor            string `query:"cursor"`
	Limit             int    `query:"limit"`
}) (*struct {
	CacheControl string     `header:"Cache-Control"`
	Body         runListing `json:"body"`
}, error) {
	if _, err := handler.reader(ctx, input.Session); err != nil {
		return nil, err
	}
	if input.BusinessSystemKey == "" {
		return nil, problem(http.StatusBadRequest, "malformed_request", "缺少 businessSystemKey 查询参数。")
	}
	cursor, cursorProblem := decodeCursor(input.Cursor)
	if cursorProblem != nil {
		return nil, cursorProblem
	}
	items, more, err := handler.Inspections.ListRuns(ctx, input.BusinessSystemKey, cursor, input.Limit)
	if err != nil {
		return nil, mapDomainError(err)
	}
	response := &struct {
		CacheControl string     `header:"Cache-Control"`
		Body         runListing `json:"body"`
	}{CacheControl: noStore()}
	response.Body.Items = items
	if more && len(items) > 0 {
		last := items[len(items)-1]
		response.Body.NextCursor = base64.StdEncoding.EncodeToString([]byte(last.CreatedAt + "\x00" + last.ID))
	}
	return response, nil
}

func (handler *Handler) reader(ctx context.Context, cookie string) (int64, error) {
	if handler.Authenticate == nil {
		return 0, problem(http.StatusServiceUnavailable, "unavailable", "服务尚未就绪")
	}
	principalID, err := handler.Authenticate(ctx, cookie)
	if err != nil {
		return 0, problem(http.StatusUnauthorized, "unauthorized", "登录会话无效，请重新登录。")
	}
	return principalID, nil
}

func decodeCursor(raw string) (string, *problemError) {
	if raw == "" {
		return "", nil
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", problem(http.StatusBadRequest, "malformed_request", "分页游标无效，请从第一页重新读取。")
	}
	cursor := string(decoded)
	createdAt, id, found := strings.Cut(cursor, "\x00")
	if !found || createdAt == "" {
		return "", problem(http.StatusBadRequest, "malformed_request", "分页游标无效，请从第一页重新读取。")
	}
	if value, err := strconv.ParseInt(id, 10, 64); err != nil || value <= 0 {
		return "", problem(http.StatusBadRequest, "malformed_request", "分页游标无效，请从第一页重新读取。")
	}
	return cursor, nil
}

func (handler *Handler) createRun(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Body    struct {
		BusinessSystemKey string `json:"businessSystemKey" minLength:"1" maxLength:"200"`
		PlanKey           string `json:"planKey" minLength:"1" maxLength:"200"`
		ClientCommandID   string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
	}
}) (*struct {
	Status       int                  `header:"-"`
	CacheControl string               `header:"Cache-Control"`
	Body         inspection.RunDetail `json:"body"`
}, error) {
	principalID, err := handler.reader(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	detail, err := handler.Inspections.CreateInspectionRun(ctx, principalID, input.Body.ClientCommandID, input.Body.BusinessSystemKey, input.Body.PlanKey)
	if err != nil {
		return nil, mapDomainError(err)
	}
	if handler.DispatchInspections != nil {
		go handler.DispatchInspections(context.Background())
	}
	return &struct {
		Status       int                  `header:"-"`
		CacheControl string               `header:"Cache-Control"`
		Body         inspection.RunDetail `json:"body"`
	}{Status: http.StatusAccepted, CacheControl: noStore(), Body: detail}, nil
}

func (handler *Handler) getRun(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	RunID   string `path:"runId"`
}) (*struct {
	CacheControl string               `header:"Cache-Control"`
	Body         inspection.RunDetail `json:"body"`
}, error) {
	if _, err := handler.reader(ctx, input.Session); err != nil {
		return nil, err
	}
	runID, parseErr := strconv.ParseInt(input.RunID, 10, 64)
	if parseErr != nil || runID < 1 {
		return nil, problem(http.StatusBadRequest, "malformed_request", "巡检 Run 标识无效。")
	}
	var systemKey string
	if err := handler.Inspections.DB().QueryRowContext(ctx, `
		SELECT s.key FROM business_systems s JOIN inspection_runs r ON r.business_system_id=s.id WHERE r.id=?`, runID).Scan(&systemKey); err != nil {
		return nil, problem(http.StatusNotFound, "not_found", "巡检 Run 不存在")
	}
	detail, err := handler.Inspections.GetRun(ctx, systemKey, runID)
	if err != nil {
		return nil, mapDomainError(err)
	}
	return &struct {
		CacheControl string               `header:"Cache-Control"`
		Body         inspection.RunDetail `json:"body"`
	}{CacheControl: noStore(), Body: detail}, nil
}

func (handler *Handler) listReports(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	RunID   string `path:"runId"`
	Limit   int    `query:"limit"`
}) (*struct {
	CacheControl string        `header:"Cache-Control"`
	Body         reportListing `json:"body"`
}, error) {
	if _, err := handler.reader(ctx, input.Session); err != nil {
		return nil, err
	}
	runID, parseErr := strconv.ParseInt(input.RunID, 10, 64)
	if parseErr != nil || runID < 1 {
		return nil, problem(http.StatusBadRequest, "malformed_request", "巡检 Run 标识无效。")
	}
	items, err := handler.Inspections.ListReports(ctx, runID, input.Limit)
	if err != nil {
		return nil, mapDomainError(err)
	}
	return &struct {
		CacheControl string        `header:"Cache-Control"`
		Body         reportListing `json:"body"`
	}{CacheControl: noStore(), Body: reportListing{Items: items}}, nil
}

func (handler *Handler) getReport(ctx context.Context, input *struct {
	Session       string `cookie:"__Host-quoin-session"`
	RunID         string `path:"runId"`
	ReportVersion string `path:"reportVersion"`
}) (*struct {
	CacheControl string                  `header:"Cache-Control"`
	Body         inspection.ReportDetail `json:"body"`
}, error) {
	if _, err := handler.reader(ctx, input.Session); err != nil {
		return nil, err
	}
	runID, runErr := strconv.ParseInt(input.RunID, 10, 64)
	version, versionErr := strconv.ParseInt(input.ReportVersion, 10, 64)
	if runErr != nil || runID < 1 || versionErr != nil || version < 1 {
		return nil, problem(http.StatusBadRequest, "malformed_request", "巡检 Run 或报告版本标识无效。")
	}
	detail, err := handler.Inspections.GetReport(ctx, runID, version)
	if err != nil {
		return nil, mapDomainError(err)
	}
	return &struct {
		CacheControl string                  `header:"Cache-Control"`
		Body         inspection.ReportDetail `json:"body"`
	}{CacheControl: noStore(), Body: detail}, nil
}

type reportListing struct {
	Items []inspection.ReportSummaryItem `json:"items"`
}

func (handler *Handler) dispatchCancelFence(ctx context.Context, attemptID int64) {
	if handler.CancelDispatch == nil {
		return
	}
	if err := handler.CancelDispatch(ctx, attemptID); err != nil {
		sharedops.LogEvent("quoin", "error", "inspection.cancel_dispatch", fmt.Sprintf("attempt=%d %v", attemptID, err))
	}
}

func (handler *Handler) cancelRun(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	RunID   string `path:"runId"`
	Body    struct {
		ClientCommandID    string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
		ExpectedRowVersion int64  `json:"expectedRowVersion" minimum:"1"`
	}
}) (*struct {
	CacheControl string               `header:"Cache-Control"`
	Body         inspection.RunDetail `json:"body"`
}, error) {
	principalID, err := handler.reader(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	runID, parseErr := strconv.ParseInt(input.RunID, 10, 64)
	if parseErr != nil || runID < 1 {
		return nil, problem(http.StatusBadRequest, "malformed_request", "巡检 Run 标识无效。")
	}
	var systemKey string
	if err := handler.Inspections.DB().QueryRowContext(ctx, `
		SELECT s.key FROM business_systems s JOIN inspection_runs r ON r.business_system_id=s.id WHERE r.id=?`, runID).Scan(&systemKey); err != nil {
		return nil, problem(http.StatusNotFound, "not_found", "巡检 Run 不存在")
	}
	outcome, err := handler.Inspections.CancelRunWithDispatch(ctx, principalID, input.Body.ClientCommandID, systemKey, runID, input.Body.ExpectedRowVersion)
	if err != nil {
		return nil, mapDomainError(err)
	}
	for _, attemptID := range outcome.DispatchAttemptIDs {
		go handler.dispatchCancelFence(context.Background(), attemptID)
	}
	return &struct {
		CacheControl string               `header:"Cache-Control"`
		Body         inspection.RunDetail `json:"body"`
	}{CacheControl: noStore(), Body: outcome.Detail}, nil
}

func (handler *Handler) reanalyzeRun(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	RunID   string `path:"runId"`
	Body    struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
	}
}) (*struct {
	Status       int                       `header:"-"`
	CacheControl string                    `header:"Cache-Control"`
	Body         inspection.AttemptSummary `json:"body"`
}, error) {
	principalID, err := handler.reader(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	runID, systemKey, err := handler.runScope(ctx, input.RunID)
	if err != nil {
		return nil, err
	}
	result, err := handler.Inspections.ReanalyzeRun(ctx, principalID, input.Body.ClientCommandID, systemKey, runID)
	if errors.Is(err, inspection.ErrModelProviderMissing) {
		return nil, problem(http.StatusServiceUnavailable, "model_unavailable", "尚无可用的模型连接，请先完成连接探测并启用后重试。")
	}
	if err != nil {
		return nil, mapDomainError(err)
	}
	if handler.DispatchInspections != nil {
		go handler.DispatchInspections(context.Background())
	}
	return &struct {
		Status       int                       `header:"-"`
		CacheControl string                    `header:"Cache-Control"`
		Body         inspection.AttemptSummary `json:"body"`
	}{Status: http.StatusAccepted, CacheControl: noStore(), Body: result}, nil
}

func (handler *Handler) rerunInspection(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	RunID   string `path:"runId"`
	Body    struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
	}
}) (*struct {
	Status       int                  `header:"-"`
	CacheControl string               `header:"Cache-Control"`
	Body         inspection.RunDetail `json:"body"`
}, error) {
	principalID, err := handler.reader(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	runID, systemKey, err := handler.runScope(ctx, input.RunID)
	if err != nil {
		return nil, err
	}
	detail, err := handler.Inspections.RerunInspection(ctx, principalID, input.Body.ClientCommandID, systemKey, runID)
	if err != nil {
		return nil, mapDomainError(err)
	}
	if handler.DispatchInspections != nil {
		go handler.DispatchInspections(context.Background())
	}
	return &struct {
		Status       int                  `header:"-"`
		CacheControl string               `header:"Cache-Control"`
		Body         inspection.RunDetail `json:"body"`
	}{Status: http.StatusAccepted, CacheControl: noStore(), Body: detail}, nil
}

func (handler *Handler) runScope(ctx context.Context, rawRunID string) (int64, string, error) {
	runID, parseErr := strconv.ParseInt(rawRunID, 10, 64)
	if parseErr != nil || runID < 1 {
		return 0, "", problem(http.StatusBadRequest, "malformed_request", "巡检 Run 标识无效。")
	}
	var systemKey string
	if err := handler.Inspections.DB().QueryRowContext(ctx, `
		SELECT s.key FROM business_systems s JOIN inspection_runs r ON r.business_system_id=s.id WHERE r.id=?`, runID).Scan(&systemKey); err != nil {
		return 0, "", problem(http.StatusNotFound, "not_found", "巡检 Run 不存在")
	}
	return runID, systemKey, nil
}

// Register mounts the manual inspection routes (HTTP-INSPECT surface, T24).
func (handler *Handler) Register(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/inspections/runs", OperationID: "listInspectionRuns"}, handler.listRuns)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/inspections/runs", OperationID: "createInspectionRun"}, handler.createRun)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/inspections/runs/{runId}", OperationID: "getInspectionRun"}, handler.getRun)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/inspections/runs/{runId}/reports", OperationID: "listInspectionReports"}, handler.listReports)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/inspections/runs/{runId}/reports/{reportVersion}", OperationID: "getInspectionReport"}, handler.getReport)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/inspections/runs/{runId}/cancel", OperationID: "cancelInspectionRun"}, handler.cancelRun)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/inspections/runs/{runId}/analyze", OperationID: "retryInspectionAnalysis"}, handler.reanalyzeRun)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/inspections/runs/{runId}/rerun", OperationID: "rerunInspection"}, handler.rerunInspection)
}
