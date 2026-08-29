package knowledge

// HTTP surface for T27 (internal/quoin/app/knowledge): the frozen
// OpenAPI operations for diagnosis feedback and knowledge candidates,
// mounted by the shared app server. Handlers authenticate every request
// server-side and map domain errors onto the frozen problem envelope.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/feedback"
	"github.com/Suknna/quoin/internal/quoin/knowledge"
	"github.com/danielgtaylor/huma/v2"
)

// Authenticate resolves the session cookie to the acting user id.
type Authenticate func(ctx context.Context, cookie string) (int64, error)

// Handler owns the knowledge and feedback routes.
type Handler struct {
	Feedback     *feedback.Service
	Knowledge    *knowledge.Service
	Authenticate Authenticate
}

// problemError serializes to the frozen ErrorModel shape (HTTP-ERROR-001),
// mirroring the shared app helper (not importable from this package).
type problemError struct {
	status      int            `json:"-"`
	Code        string         `json:"code"`
	Message     string         `json:"message"`
	Retryable   bool           `json:"retryable"`
	FieldErrors []problemField `json:"fieldErrors,omitempty"`
	Conflict    map[string]any `json:"conflict,omitempty"`
}

type problemField struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

func (p *problemError) Error() string             { return p.Message }
func (p *problemError) GetStatus() int            { return p.status }
func (p *problemError) ContentType(string) string { return "application/problem+json" }

func problem(status int, code, message string) *problemError {
	return &problemError{status: status, Code: code, Message: message, Retryable: status >= 500 || status == 429}
}

func problemUnprocessable(message string) *problemError {
	return problem(422, "validation_failed", message)
}

func stateConflictProblem(state string) *problemError {
	conflict := problem(409, "active_conflict", "该候选当前状态不允许此操作。")
	conflict.Conflict = map[string]any{"code": "active_conflict", "state": state}
	return conflict
}

func commandReusedProblem() *problemError {
	conflict := problem(409, "command_id_reused", "命令 ID 已被其他请求使用，请重新发起。")
	conflict.Conflict = map[string]any{"code": "command_id_reused"}
	return conflict
}

// parseID converts a path locator; invalid values are 404s (the object
// cannot exist under that name).
func parseID(value string) (int64, *problemError) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, problem(404, "not_found", "对象不存在。")
	}
	return id, nil
}

// encodeCursor renders one keyset cursor opaquely.
func encodeCursor(value any) string {
	body, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(body)
}

func decodeCursor(token string, dest any) *problemError {
	if token == "" {
		return nil
	}
	body, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return problem(400, "malformed_request", "分页游标无效。")
	}
	if err := json.Unmarshal(body, dest); err != nil {
		return problem(400, "malformed_request", "分页游标无效。")
	}
	return nil
}

// Register mounts every T27 route on the Huma API.
func (handler *Handler) Register(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/knowledge/feedback", OperationID: "appendDiagnosisFeedback", DefaultStatus: http.StatusCreated}, handler.appendDiagnosisFeedback)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/knowledge/feedback", OperationID: "listDiagnosisFeedback"}, handler.listDiagnosisFeedback)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/alerts/{occurrenceId}/analyses/{analysisId}/knowledge-candidates", OperationID: "createAnalysisKnowledgeCandidate"}, handler.createAnalysisKnowledgeCandidate)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/investigations/{investigationId}/knowledge-candidates", OperationID: "createInvestigationKnowledgeCandidate"}, handler.createInvestigationKnowledgeCandidate)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/inspections/runs/{runId}/reports/{reportVersion}/knowledge-candidates", OperationID: "createReportKnowledgeCandidate"}, handler.createReportKnowledgeCandidate)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/knowledge/candidates", OperationID: "listKnowledgeCandidates"}, handler.listKnowledgeCandidates)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/knowledge/candidates/{candidateId}", OperationID: "getKnowledgeCandidate"}, handler.getKnowledgeCandidate)
	huma.Register(api, huma.Operation{Method: http.MethodPatch, Path: "/api/v1/knowledge/candidates/{candidateId}", OperationID: "editKnowledgeCandidateDraft"}, handler.editKnowledgeCandidateDraft)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/knowledge/candidates/{candidateId}/confirm", OperationID: "confirmKnowledgeCandidate"}, handler.confirmKnowledgeCandidate)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/knowledge/candidates/{candidateId}/exclude", OperationID: "excludeKnowledgeCandidate"}, handler.excludeKnowledgeCandidate)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/knowledge", OperationID: "searchKnowledge"}, handler.searchKnowledge)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/knowledge/items/{knowledgeId}", OperationID: "getKnowledge"}, handler.getKnowledge)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/knowledge/items/{knowledgeId}/versions", OperationID: "listKnowledgeVersions"}, handler.listKnowledgeVersions)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/knowledge/items/{knowledgeId}/versions/{versionId}", OperationID: "getKnowledgeVersion"}, handler.getKnowledgeVersion)
}

func (handler *Handler) user(ctx context.Context, cookie string) (int64, *problemError) {
	principalID, err := handler.Authenticate(ctx, cookie)
	if err != nil {
		return 0, problem(401, "unauthenticated", "请重新登录。")
	}
	return principalID, nil
}

func feedbackProblem(err error) *problemError {
	switch {
	case errors.Is(err, feedback.ErrInvalidTarget):
		return problemUnprocessable("反馈目标必须是初步分析结论、巡检报告或助手消息。")
	case errors.Is(err, feedback.ErrInvalidValue):
		return problemUnprocessable("反馈值不在允许范围内。")
	case errors.Is(err, feedback.ErrNoteTooLong):
		return problemUnprocessable("说明最长 4096 个字符。")
	case errors.Is(err, feedback.ErrNotFound):
		return problem(404, "not_found", "反馈目标不存在。")
	case errors.Is(err, feedback.ErrCommandReused):
		return commandReusedProblem()
	}
	return problem(500, "unavailable", "暂时无法记录反馈，请重试。")
}

func (handler *Handler) appendDiagnosisFeedback(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Body    struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
		TargetType      string `json:"targetType" enum:"initial_analysis_output,inspection_report,investigation_message"`
		TargetID        string `json:"targetId"`
		Value           string `json:"value" enum:"adopted,executed,verified_effective,rejected"`
		Note            string `json:"note,omitempty" maxLength:"4096"`
	}
}) (*struct {
	Body feedback.Event `json:"body"`
}, error) {
	principalID, authProblem := handler.user(ctx, input.Session)
	if authProblem != nil {
		return nil, authProblem
	}
	targetID, idProblem := parseID(input.Body.TargetID)
	if idProblem != nil {
		return nil, idProblem
	}
	event, err := handler.Feedback.Append(ctx, principalID, input.Body.ClientCommandID, feedback.Target{Type: input.Body.TargetType, ID: targetID}, input.Body.Value, feedback.TrimNote(input.Body.Note))
	if err != nil {
		return nil, feedbackProblem(err)
	}
	return &struct {
		Body feedback.Event `json:"body"`
	}{Body: event}, nil
}

type feedbackTimeline struct {
	LatestValue string           `json:"latestValue,omitempty"`
	Items       []feedback.Event `json:"items"`
	NextCursor  string           `json:"nextCursor,omitempty"`
}

type feedbackTimelineBody struct {
	Body feedbackTimeline `json:"body"`
}

func nextFeedbackCursor(next *feedback.Cursor) string {
	if next == nil {
		return ""
	}
	return encodeCursor(*next)
}

func (handler *Handler) listDiagnosisFeedback(ctx context.Context, input *struct {
	Session    string `cookie:"__Host-quoin-session"`
	TargetType string `query:"targetType" enum:"initial_analysis_output,inspection_report,investigation_message"`
	TargetID   string `query:"targetId"`
	Cursor     string `query:"cursor"`
	Limit      int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
}) (*feedbackTimelineBody, error) {
	if _, authProblem := handler.user(ctx, input.Session); authProblem != nil {
		return nil, authProblem
	}
	targetID, idProblem := parseID(input.TargetID)
	if idProblem != nil {
		return nil, idProblem
	}
	var after *feedback.Cursor
	if input.Cursor != "" {
		var cursor feedback.Cursor
		if cursorProblem := decodeCursor(input.Cursor, &cursor); cursorProblem != nil {
			return nil, cursorProblem
		}
		after = &cursor
	}
	timeline, err := handler.Feedback.List(ctx, feedback.Target{Type: input.TargetType, ID: targetID}, after, input.Limit)
	if err != nil {
		return nil, feedbackProblem(err)
	}
	return &feedbackTimelineBody{Body: feedbackTimeline{
		LatestValue: timeline.LatestValue,
		Items:       timeline.Items,
		NextCursor:  nextFeedbackCursor(timeline.Next),
	}}, nil
}
