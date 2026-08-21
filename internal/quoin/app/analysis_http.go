package app

// Initial Analysis HTTP surface (T10): create/list/get/retry/cancel under
// the frozen /api/v1/alerts/{occurrenceId}/analyses routes. Every domain
// write carries the user-invisible client_command_id; cancellation carries
// the row-version fence and answers the completed object when success won
// the commit-order race (HTTP-COMMAND-005).

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/analysis"
	"github.com/danielgtaylor/huma/v2"
)

type analysisDetailBody struct {
	Body analysis.Detail `json:"body"`
}

type analysisListBody struct {
	Body struct {
		Items      []analysis.Summary `json:"items"`
		NextCursor string             `json:"nextCursor,omitempty"`
	} `json:"body"`
}

type analysisAttemptsBody struct {
	Body struct {
		Items      []analysis.AttemptItem `json:"items"`
		NextCursor string                 `json:"nextCursor,omitempty"`
	} `json:"body"`
}

func (application *apiServer) registerAnalysisRoutes(api huma.API) {
	huma.Register(api, huma.Operation{Method: "POST", Path: "/api/v1/alerts/{occurrenceId}/analyses", OperationID: "createInitialAnalysis", DefaultStatus: 202}, application.createInitialAnalysis)
	huma.Register(api, huma.Operation{Method: "GET", Path: "/api/v1/alerts/{occurrenceId}/analyses", OperationID: "listInitialAnalyses"}, application.listInitialAnalyses)
	huma.Register(api, huma.Operation{Method: "GET", Path: "/api/v1/alerts/{occurrenceId}/analyses/{analysisId}", OperationID: "getInitialAnalysis"}, application.getInitialAnalysis)
	huma.Register(api, huma.Operation{Method: "GET", Path: "/api/v1/alerts/{occurrenceId}/analyses/{analysisId}/attempts", OperationID: "listInitialAnalysisAttempts"}, application.listInitialAnalysisAttempts)
	huma.Register(api, huma.Operation{Method: "POST", Path: "/api/v1/alerts/{occurrenceId}/analyses/{analysisId}/retry", OperationID: "retryInitialAnalysis", DefaultStatus: 202}, application.retryInitialAnalysis)
	huma.Register(api, huma.Operation{Method: "POST", Path: "/api/v1/alerts/{occurrenceId}/analyses/{analysisId}/cancel", OperationID: "cancelInitialAnalysis"}, application.cancelInitialAnalysis)
}

// createInitialAnalysis accepts one command; a repeated create for the same
// occurrence returns the active analysis (DATA-ANALYSIS-001). No enabled
// qualified model provider is a deterministic 503 (HTTP-ERROR-007).
func (application *apiServer) createInitialAnalysis(ctx context.Context, input *struct {
	Session      string `cookie:"__Host-quoin-session"`
	OccurrenceID string `path:"occurrenceId"`
	Body         struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
	}
}) (*analysisDetailBody, error) {
	session, err := application.authenticateFull(ctx, input.Session, "发起初步分析")
	if err != nil {
		return nil, err
	}
	occurrenceID, err := strconv.ParseInt(input.OccurrenceID, 10, 64)
	if err != nil || occurrenceID <= 0 {
		return nil, huma.Error404NotFound("告警不存在", nil)
	}
	result, err := application.analyses.Create(ctx, occurrenceID, session.User.ID, input.Body.ClientCommandID)
	if err != nil {
		if errors.Is(err, analysis.ErrNotFound) {
			return nil, huma.Error404NotFound("告警不存在", nil)
		}
		if errors.Is(err, analysis.ErrModelProviderMissing) {
			return nil, huma.Error503ServiceUnavailable("模型供应商尚未启用并通过能力探测，请管理员完成设置后再发起初步分析。", err)
		}
		return nil, huma.Error500InternalServerError("暂时无法发起初步分析，请重试。", err)
	}
	// Dispatch immediately when Plinth is attached; otherwise the attempt
	// stays Queued and the connect-path dispatcher picks it up.
	if application.analysisDispatchFunc != nil {
		_ = application.analysisDispatchFunc(ctx, result.AttemptID)
	}
	detail, err := application.analyses.Get(ctx, result.AnalysisID)
	if err != nil {
		return nil, huma.Error500InternalServerError("无法读取初步分析", err)
	}
	return &analysisDetailBody{Body: detail}, nil
}

func (application *apiServer) listInitialAnalyses(ctx context.Context, input *struct {
	Session      string `cookie:"__Host-quoin-session"`
	OccurrenceID string `path:"occurrenceId"`
	Cursor       string `query:"cursor"`
	Limit        int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
}) (*analysisListBody, error) {
	if _, err := application.authenticateFull(ctx, input.Session, "读取初步分析"); err != nil {
		return nil, err
	}
	occurrenceID, err := strconv.ParseInt(input.OccurrenceID, 10, 64)
	if err != nil || occurrenceID <= 0 {
		return nil, huma.Error404NotFound("告警不存在", nil)
	}
	after, err := cursorAfter(input.Cursor)
	if err != nil {
		return nil, huma.Error400BadRequest("分页游标无效", err)
	}
	items, next, err := application.analyses.ListByOccurrence(ctx, occurrenceID, after, input.Limit)
	if err != nil {
		return nil, huma.Error500InternalServerError("无法读取初步分析", err)
	}
	output := &analysisListBody{}
	output.Body.Items = asItems(items)
	if next > 0 {
		output.Body.NextCursor = cursorToken(next)
	}
	return output, nil
}

func (application *apiServer) getInitialAnalysis(ctx context.Context, input *struct {
	Session      string `cookie:"__Host-quoin-session"`
	OccurrenceID string `path:"occurrenceId"`
	AnalysisID   string `path:"analysisId"`
}) (*analysisDetailBody, error) {
	if _, err := application.authenticateFull(ctx, input.Session, "读取初步分析"); err != nil {
		return nil, err
	}
	analysisID, err := strconv.ParseInt(input.AnalysisID, 10, 64)
	if err != nil || analysisID <= 0 {
		return nil, huma.Error404NotFound("初步分析不存在", nil)
	}
	occurrenceID, err := strconv.ParseInt(input.OccurrenceID, 10, 64)
	if err != nil || occurrenceID <= 0 {
		return nil, huma.Error404NotFound("告警不存在", nil)
	}
	if err := application.analyses.VerifyOccurrence(ctx, analysisID, occurrenceID); err != nil {
		if errors.Is(err, analysis.ErrNotFound) {
			return nil, huma.Error404NotFound("初步分析不存在", nil)
		}
		return nil, huma.Error500InternalServerError("无法读取初步分析", err)
	}
	detail, err := application.analyses.Get(ctx, analysisID)
	if err != nil {
		if errors.Is(err, analysis.ErrNotFound) {
			return nil, huma.Error404NotFound("初步分析不存在", nil)
		}
		return nil, huma.Error500InternalServerError("无法读取初步分析", err)
	}
	return &analysisDetailBody{Body: detail}, nil
}

func (application *apiServer) listInitialAnalysisAttempts(ctx context.Context, input *struct {
	Session      string `cookie:"__Host-quoin-session"`
	OccurrenceID string `path:"occurrenceId"`
	AnalysisID   string `path:"analysisId"`
	Cursor       string `query:"cursor"`
	Limit        int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
}) (*analysisAttemptsBody, error) {
	if _, err := application.authenticateFull(ctx, input.Session, "读取执行 Attempt"); err != nil {
		return nil, err
	}
	analysisID, err := strconv.ParseInt(input.AnalysisID, 10, 64)
	if err != nil || analysisID <= 0 {
		return nil, huma.Error404NotFound("初步分析不存在", nil)
	}
	occurrenceID, err := strconv.ParseInt(input.OccurrenceID, 10, 64)
	if err != nil || occurrenceID <= 0 {
		return nil, huma.Error404NotFound("告警不存在", nil)
	}
	if err := application.analyses.VerifyOccurrence(ctx, analysisID, occurrenceID); err != nil {
		if errors.Is(err, analysis.ErrNotFound) {
			return nil, huma.Error404NotFound("初步分析不存在", nil)
		}
		return nil, huma.Error500InternalServerError("无法读取执行 Attempt", err)
	}
	after, err := cursorAfter(input.Cursor)
	if err != nil {
		return nil, huma.Error400BadRequest("分页游标无效", err)
	}
	items, next, err := application.analyses.ListAttempts(ctx, analysisID, after, input.Limit)
	if err != nil {
		return nil, huma.Error500InternalServerError("无法读取执行 Attempt", err)
	}
	output := &analysisAttemptsBody{}
	output.Body.Items = asItems(items)
	if next > 0 {
		output.Body.NextCursor = cursorToken(next)
	}
	return output, nil
}

// retryInitialAnalysis creates a new Attempt for a technical failure and
// reuses the frozen input snapshot (DATA-ANALYSIS-001).
func (application *apiServer) retryInitialAnalysis(ctx context.Context, input *struct {
	Session      string `cookie:"__Host-quoin-session"`
	OccurrenceID string `path:"occurrenceId"`
	AnalysisID   string `path:"analysisId"`
	Body         struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
	}
}) (*analysisDetailBody, error) {
	session, err := application.authenticateFull(ctx, input.Session, "重试初步分析")
	if err != nil {
		return nil, err
	}
	analysisID, err := strconv.ParseInt(input.AnalysisID, 10, 64)
	if err != nil || analysisID <= 0 {
		return nil, huma.Error404NotFound("初步分析不存在", nil)
	}
	occurrenceID, err := strconv.ParseInt(input.OccurrenceID, 10, 64)
	if err != nil || occurrenceID <= 0 {
		return nil, huma.Error404NotFound("告警不存在", nil)
	}
	if err := application.analyses.VerifyOccurrence(ctx, analysisID, occurrenceID); err != nil {
		if errors.Is(err, analysis.ErrNotFound) {
			return nil, huma.Error404NotFound("初步分析不存在", nil)
		}
		return nil, huma.Error500InternalServerError("暂时无法重试初步分析，请重试。", err)
	}
	attemptID, err := application.analyses.Retry(ctx, analysisID, session.User.ID, input.Body.ClientCommandID)
	if err != nil {
		if errors.Is(err, analysis.ErrNotFound) {
			return nil, huma.Error404NotFound("初步分析不存在", nil)
		}
		if errors.Is(err, analysis.ErrActiveConflict) {
			return nil, huma.Error409Conflict("只有技术失败的初步分析可以重试。")
		}
		if errors.Is(err, analysis.ErrModelProviderMissing) {
			return nil, huma.Error503ServiceUnavailable("模型供应商尚未启用并通过能力探测，请管理员完成设置后再重试。", err)
		}
		return nil, huma.Error500InternalServerError("暂时无法重试初步分析，请重试。", err)
	}
	if application.analysisDispatchFunc != nil {
		_ = application.analysisDispatchFunc(ctx, attemptID)
	}
	detail, err := application.analyses.Get(ctx, analysisID)
	if err != nil {
		return nil, huma.Error500InternalServerError("无法读取初步分析", err)
	}
	return &analysisDetailBody{Body: detail}, nil
}

// cancelInitialAnalysis commits the idempotent cancellation fence
// (HTTP-COMMAND-005): success already committed answers the completed
// object; a stale row version conflicts; the fence never leaves a fenced
// middle state.
func (application *apiServer) cancelInitialAnalysis(ctx context.Context, input *struct {
	Session      string `cookie:"__Host-quoin-session"`
	OccurrenceID string `path:"occurrenceId"`
	AnalysisID   string `path:"analysisId"`
	Body         struct {
		ClientCommandID    string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
		ExpectedRowVersion int64  `json:"expectedRowVersion" minimum:"1"`
	}
}) (*analysisDetailBody, error) {
	session, err := application.authenticateFull(ctx, input.Session, "取消初步分析")
	if err != nil {
		return nil, err
	}
	analysisID, err := strconv.ParseInt(input.AnalysisID, 10, 64)
	if err != nil || analysisID <= 0 {
		return nil, huma.Error404NotFound("初步分析不存在", nil)
	}
	occurrenceID, err := strconv.ParseInt(input.OccurrenceID, 10, 64)
	if err != nil || occurrenceID <= 0 {
		return nil, huma.Error404NotFound("告警不存在", nil)
	}
	if err := application.analyses.VerifyOccurrence(ctx, analysisID, occurrenceID); err != nil {
		if errors.Is(err, analysis.ErrNotFound) {
			return nil, huma.Error404NotFound("初步分析不存在", nil)
		}
		return nil, huma.Error500InternalServerError("暂时无法取消初步分析，请重试。", err)
	}
	outcome, err := application.analyses.Cancel(ctx, analysisID, session.User.ID, input.Body.ExpectedRowVersion, input.Body.ClientCommandID)
	if err != nil {
		var rowVersion *analysis.RowVersionError
		switch {
		case errors.Is(err, analysis.ErrNotFound):
			return nil, huma.Error404NotFound("初步分析不存在", nil)
		case errors.As(err, &rowVersion):
			return nil, huma.Error409Conflict("初步分析状态已变化，请刷新后重试。")
		case errors.Is(err, analysis.ErrActiveConflict):
			return nil, huma.Error409Conflict("该初步分析已结束，无法取消。")
		default:
			return nil, huma.Error500InternalServerError("暂时无法取消初步分析，请重试。", err)
		}
	}
	// The fence is committed: deliver the runtime cancellation now
	// (RUNTIME-CANCEL-001). The CancelAttempt response is acknowledged
	// asynchronously; a dispatch failure leaves the attempt Cancelling for
	// the T12 reconciler and must never roll back the committed fence.
	if outcome.DispatchRequired && application.cancelDispatchFunc != nil {
		if err := application.cancelDispatchFunc(context.Background(), outcome.AttemptID); err != nil {
			sharedops.LogEvent("quoin", "error", "analysis.cancel_dispatch", fmt.Sprintf("attempt=%d error=%v", outcome.AttemptID, err))
		}
	}
	detail, err := application.analyses.Get(ctx, analysisID)
	if err != nil {
		return nil, huma.Error500InternalServerError("无法读取初步分析", err)
	}
	// Success already won the race reports the completed object; every
	// other fence outcome reports the current detail (HTTP-COMMAND-005).
	return &analysisDetailBody{Body: detail}, nil
}

// cursorAfter decodes the opaque keyset cursor (plain base64url id token).
func cursorAfter(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}
	return decodeCursorToken(cursor)
}
