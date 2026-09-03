package appinvestigation

// Investigation HTTP surface (T13): the frozen /api/v1/investigations
// routes. Every domain write carries the user-invisible client_command_id;
// sends carry the expected head fence (DATA-INVEST-001). The unbounded
// histories stay cursor-paginated subresources (HTTP-PAGE-005). The
// Handler is a seam wired by the app package: it owns the domain service,
// the session check and the dispatch hook without importing its parent.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/investigation"
	"github.com/danielgtaylor/huma/v2"
)

// Handler wires the investigation domain service into the Huma surface.
type Handler struct {
	Service *investigation.Service
	// Authenticate resolves the session cookie to the principal id; any
	// non-nil error is mapped by the caller's session middleware
	// (authentication failures, restricted-session rules).
	Authenticate func(ctx context.Context, cookie string) (int64, error)
	// SessionValid re-checks an established stream's session each tick
	// (Q214/SEC-SESSION-002): revoked sessions must close live streams.
	SessionValid func(ctx context.Context, cookie string) bool
	// Dispatch delivers one Queued attempt to the live Plinth stream.
	Dispatch func(ctx context.Context, attemptID int64) error
	// CancelDispatch delivers one committed cancellation fence to the
	// runtime that owns the attempt (RUNTIME-CANCEL-001; stop and undo
	// share it).
	CancelDispatch func(ctx context.Context, attemptID int64) error
}

// RegisterUpgradeDrain mounts only this surface's frozen upgrade-drain
// cancel operation (openapi x-quoin-maintenance-access: upgrade-drain).
func (handler *Handler) RegisterUpgradeDrain(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/investigations/{investigationId}/attempts/{attemptId}/cancel", OperationID: "cancelInvestigationAttempt"}, handler.cancelInvestigationAttempt)
}

// Register mounts the investigation routes (T13 core + T14 attachments +
// T15 head concurrency: undo, stop, retry).
func (handler *Handler) Register(api huma.API) {
	huma.Register(api, huma.Operation{Method: "GET", Path: "/api/v1/investigations", OperationID: "listInvestigations"}, handler.listInvestigations)
	huma.Register(api, huma.Operation{Method: "POST", Path: "/api/v1/investigations", OperationID: "createInvestigation", DefaultStatus: 201}, handler.createInvestigation)
	huma.Register(api, huma.Operation{Method: "GET", Path: "/api/v1/investigations/{investigationId}", OperationID: "getInvestigation"}, handler.getInvestigation)
	huma.Register(api, huma.Operation{Method: "GET", Path: "/api/v1/investigations/{investigationId}/messages", OperationID: "listInvestigationMessages"}, handler.listInvestigationMessages)
	huma.Register(api, huma.Operation{Method: "POST", Path: "/api/v1/investigations/{investigationId}/messages", OperationID: "sendInvestigationMessage", DefaultStatus: 201}, handler.sendInvestigationMessage)
	huma.Register(api, huma.Operation{Method: "POST", Path: "/api/v1/investigations/{investigationId}/messages/{messageId}/stream", OperationID: "streamInvestigationMessage"}, handler.streamInvestigationMessage)
	huma.Register(api, huma.Operation{Method: "GET", Path: "/api/v1/investigations/{investigationId}/attempts", OperationID: "listInvestigationAttempts"}, handler.listInvestigationAttempts)
	huma.Register(api, huma.Operation{Method: "GET", Path: "/api/v1/investigations/{investigationId}/attempts/{attemptId}/tool-calls", OperationID: "listAttemptToolCalls"}, handler.listAttemptToolCalls)
	huma.Register(api, huma.Operation{Method: "POST", Path: "/api/v1/investigations/{investigationId}/undo", OperationID: "undoInvestigationMessage"}, handler.undoInvestigationMessage)
	handler.RegisterUpgradeDrain(api)
	huma.Register(api, huma.Operation{Method: "POST", Path: "/api/v1/investigations/{investigationId}/attempts/{attemptId}/retry", OperationID: "retryInvestigationAttempt", DefaultStatus: 202}, handler.retryInvestigationAttempt)
	huma.Register(api, huma.Operation{Method: "GET", Path: "/api/v1/investigation-attachments/{attachmentId}", OperationID: "getInvestigationAttachment"}, handler.getInvestigationAttachment)
}

func (handler *Handler) principal(ctx context.Context, cookie string) (int64, error) {
	if handler.Authenticate == nil {
		return 0, errors.New("investigation handler not wired")
	}
	return handler.Authenticate(ctx, cookie)
}

// problemError is the frozen ErrorModel envelope (HTTP-ERROR-002/004):
// stable code, ordinary-language message, retryability and the optional
// conflict discriminator.
type problemError struct {
	status      int
	Code        string         `json:"code"`
	Message     string         `json:"message"`
	Retryable   bool           `json:"retryable"`
	FieldErrors []problemField `json:"fieldErrors,omitempty"`
	Conflict    map[string]any `json:"conflict,omitempty"`
}

type problemField struct {
	Path        string `json:"path"`
	Reason      string `json:"reason"`
	Remediation string `json:"remediation,omitempty"`
}

func (p *problemError) Error() string  { return p.Message }
func (p *problemError) GetStatus() int { return p.status }

func problem(status int, code, message string) *problemError {
	return &problemError{status: status, Code: code, Message: message, Retryable: status >= 500 || status == 429}
}

func problemUnprocessable(message string) *problemError {
	return problem(http.StatusUnprocessableEntity, "validation_failed", message)
}

type investigationDetailBody struct {
	Body investigation.InvestigationDetail `json:"body"`
}

type investigationListBody struct {
	Body struct {
		Items      []investigation.InvestigationSummary `json:"items"`
		NextCursor string                               `json:"nextCursor,omitempty"`
	} `json:"body"`
}

type messageBody struct {
	Body messageOutput `json:"body"`
}

// messageOutput is the MessageSummary wire shape; attachments lists the
// message's ordered immutable attachment summaries (empty array on the
// wire when the turn carried none).
type messageOutput struct {
	ID              string                            `json:"id"`
	Seq             int64                             `json:"seq"`
	Role            string                            `json:"role"`
	Status          string                            `json:"status"`
	Content         string                            `json:"content"`
	ParentMessageID string                            `json:"parentMessageId,omitempty"`
	Attachments     []investigation.MessageAttachment `json:"attachments"`
	AttemptID       string                            `json:"attemptId,omitempty"`
	EvidenceIDs     []string                          `json:"evidenceIds"`
	CreatedAt       string                            `json:"createdAt"`
}

type messageListBody struct {
	Body struct {
		Items      []messageOutput `json:"items"`
		NextCursor string          `json:"nextCursor,omitempty"`
	} `json:"body"`
}

type attemptListBody struct {
	Body struct {
		Items      []investigation.InvestigationAttemptItem `json:"items"`
		NextCursor string                                   `json:"nextCursor,omitempty"`
	} `json:"body"`
}

type toolCallListBody struct {
	Body struct {
		Items      []investigation.InvestigationToolCallItem `json:"items"`
		NextCursor string                                    `json:"nextCursor,omitempty"`
	} `json:"body"`
}

type sourceInputWire struct {
	Type     string `json:"type" enum:"occurrence,initial_analysis,evidence,inspection_report"`
	SourceID string `json:"sourceId" pattern:"^[0-9]+$"`
}

// listInvestigations pages the derived (lastActivityAt, id) keyset
// (DATA-INVEST-005).
func (handler *Handler) listInvestigations(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Cursor  string `query:"cursor"`
	Limit   int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
}) (*investigationListBody, error) {
	if _, err := handler.principal(ctx, input.Session); err != nil {
		return nil, err
	}
	after, err := decodeListCursor(input.Cursor)
	if err != nil {
		return nil, problem(400, "malformed_request", "分页游标无效，请刷新列表后重试。")
	}
	items, next, err := handler.Service.List(ctx, after, input.Limit)
	if err != nil {
		return nil, problem(500, "unavailable", "暂时无法读取调查列表，请重试。")
	}
	output := &investigationListBody{}
	output.Body.Items = items
	if next != nil {
		output.Body.NextCursor = encodeListCursor(*next)
	}
	return output, nil
}

// createInvestigation atomically creates the Investigation, its sources,
// the first user message (text and/or ordered attachments) and the
// Execution Attempt (DATA-INVEST-001); no empty Investigation is ever
// persisted.
func (handler *Handler) createInvestigation(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Body    struct {
		ClientCommandID string            `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
		Content         string            `json:"content,omitempty"`
		AttachmentIDs   []string          `json:"attachmentIds,omitempty"`
		Sources         []sourceInputWire `json:"sources,omitempty"`
	}
}) (*investigationDetailBody, error) {
	principalID, err := handler.principal(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	attachmentIDs, err := investigation.ParseAttachmentIDs(input.Body.AttachmentIDs)
	if err != nil {
		return nil, problemUnprocessable("附件引用无效，请重新选择附件后重试。")
	}
	sources, err := parseSources(input.Body.Sources)
	if err != nil {
		return nil, problemUnprocessable("来源引用无效，请返回告警或分析页面重新发起。")
	}
	result, err := handler.Service.Create(ctx, principalID, input.Body.ClientCommandID, input.Body.Content, attachmentIDs, sources)
	if err != nil {
		return nil, createSendError(err)
	}
	if handler.Dispatch != nil {
		if err := handler.Dispatch(ctx, result.AttemptID); err != nil {
			// A failed dispatch must be loud: the attempt stays Queued and
			// the runtime reconcile (onPlinthAttached DispatchQueued) delivers
			// it when a live stream appears, but the operator needs the
			// reason in the log (RUNTIME-TASK-005).
			sharedops.LogEvent("quoin", "error", "investigation.dispatch_failed", fmt.Sprintf("attempt=%d %v", result.AttemptID, err))
		}
	}
	detail, err := handler.Service.Get(ctx, result.InvestigationID)
	if err != nil {
		return nil, problem(500, "unavailable", "调查已创建，但暂时无法读取详情，请稍后重试。")
	}
	return &investigationDetailBody{Body: detail}, nil
}

// getInvestigation reads the detail projection (head, active attempt,
// counts, bounded sources).
func (handler *Handler) getInvestigation(ctx context.Context, input *struct {
	Session         string `cookie:"__Host-quoin-session"`
	InvestigationID string `path:"investigationId"`
}) (*investigationDetailBody, error) {
	if _, err := handler.principal(ctx, input.Session); err != nil {
		return nil, err
	}
	investigationID, err := strconv.ParseInt(input.InvestigationID, 10, 64)
	if err != nil || investigationID <= 0 {
		return nil, problem(404, "not_found", "调查不存在。")
	}
	detail, err := handler.Service.Get(ctx, investigationID)
	if err != nil {
		if errors.Is(err, investigation.ErrNotFound) {
			return nil, problem(404, "not_found", "调查不存在。")
		}
		return nil, problem(500, "unavailable", "暂时无法读取调查，请重试。")
	}
	return &investigationDetailBody{Body: detail}, nil
}

// listInvestigationMessages pages the message history in seq order.
func (handler *Handler) listInvestigationMessages(ctx context.Context, input *struct {
	Session         string `cookie:"__Host-quoin-session"`
	InvestigationID string `path:"investigationId"`
	Cursor          string `query:"cursor"`
	Limit           int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
}) (*messageListBody, error) {
	if _, err := handler.principal(ctx, input.Session); err != nil {
		return nil, err
	}
	investigationID, err := strconv.ParseInt(input.InvestigationID, 10, 64)
	if err != nil || investigationID <= 0 {
		return nil, problem(404, "not_found", "调查不存在。")
	}
	afterSeq, err := decodeSeqCursor(input.Cursor)
	if err != nil {
		return nil, problem(400, "malformed_request", "分页游标无效，请刷新后重试。")
	}
	items, hasMore, err := handler.Service.ListMessages(ctx, investigationID, afterSeq, input.Limit)
	if err != nil {
		return nil, problem(500, "unavailable", "暂时无法读取调查消息，请重试。")
	}
	output := &messageListBody{}
	for _, item := range items {
		output.Body.Items = append(output.Body.Items, messageOutput{
			ID: item.ID, Seq: item.Seq, Role: item.Role, Status: item.Status,
			Content: item.Content, ParentMessageID: item.ParentMessageID,
			Attachments: item.Attachments, AttemptID: item.AttemptID,
			EvidenceIDs: item.EvidenceIDs, CreatedAt: item.CreatedAt,
		})
	}
	if hasMore && len(items) > 0 {
		output.Body.NextCursor = encodeSeqCursor(items[len(items)-1].Seq)
	}
	return output, nil
}

// sendInvestigationMessage appends one user turn in a single transaction
// (DATA-INVEST-001); a stale head or an active attempt conflicts.
func (handler *Handler) sendInvestigationMessage(ctx context.Context, input *struct {
	Session         string `cookie:"__Host-quoin-session"`
	InvestigationID string `path:"investigationId"`
	Body            struct {
		ClientCommandID       string   `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
		Content               string   `json:"content,omitempty"`
		AttachmentIDs         []string `json:"attachmentIds,omitempty"`
		ExpectedHeadMessageID *string  `json:"expectedHeadMessageId"`
	}
}) (*messageBody, error) {
	principalID, err := handler.principal(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	investigationID, err := strconv.ParseInt(input.InvestigationID, 10, 64)
	if err != nil || investigationID <= 0 {
		return nil, problem(404, "not_found", "调查不存在。")
	}
	attachmentIDs, err := investigation.ParseAttachmentIDs(input.Body.AttachmentIDs)
	if err != nil {
		return nil, problemUnprocessable("附件引用无效，请重新选择附件后重试。")
	}
	var expectedHead *int64
	if input.Body.ExpectedHeadMessageID != nil {
		value, err := strconv.ParseInt(*input.Body.ExpectedHeadMessageID, 10, 64)
		if err != nil || value <= 0 {
			return nil, problemUnprocessable("expectedHeadMessageId 无效。")
		}
		expectedHead = &value
	}
	result, err := handler.Service.Send(ctx, principalID, input.Body.ClientCommandID, investigationID, expectedHead, input.Body.Content, attachmentIDs)
	if err != nil {
		return nil, sendError(err, investigationID)
	}
	if handler.Dispatch != nil {
		if err := handler.Dispatch(ctx, result.AttemptID); err != nil {
			sharedops.LogEvent("quoin", "error", "investigation.dispatch_failed", fmt.Sprintf("attempt=%d %v", result.AttemptID, err))
		}
	}
	item, err := handler.Service.MessageFor(ctx, investigationID, result.MessageID)
	if err != nil {
		return nil, problem(500, "unavailable", "消息已受理，但暂时无法读取，请刷新页面。")
	}
	return &messageBody{Body: messageOutput{
		ID: item.ID, Seq: item.Seq, Role: item.Role, Status: item.Status,
		Content: item.Content, ParentMessageID: item.ParentMessageID,
		Attachments: item.Attachments, AttemptID: item.AttemptID,
		EvidenceIDs: item.EvidenceIDs, CreatedAt: item.CreatedAt,
	}}, nil
}

// listInvestigationAttempts pages the attempt history (createdAt order).
func (handler *Handler) listInvestigationAttempts(ctx context.Context, input *struct {
	Session         string `cookie:"__Host-quoin-session"`
	InvestigationID string `path:"investigationId"`
	Cursor          string `query:"cursor"`
	Limit           int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
}) (*attemptListBody, error) {
	if _, err := handler.principal(ctx, input.Session); err != nil {
		return nil, err
	}
	investigationID, err := strconv.ParseInt(input.InvestigationID, 10, 64)
	if err != nil || investigationID <= 0 {
		return nil, problem(404, "not_found", "调查不存在。")
	}
	afterID, err := decodeIDCursor(input.Cursor)
	if err != nil {
		return nil, problem(400, "malformed_request", "分页游标无效，请刷新后重试。")
	}
	items, hasMore, err := handler.Service.ListAttempts(ctx, investigationID, afterID, input.Limit)
	if err != nil {
		return nil, problem(500, "unavailable", "暂时无法读取执行记录，请重试。")
	}
	output := &attemptListBody{}
	output.Body.Items = items
	if hasMore && len(items) > 0 {
		if id, err := strconv.ParseInt(items[len(items)-1].ID, 10, 64); err == nil {
			output.Body.NextCursor = encodeIDCursor(id)
		}
	}
	return output, nil
}

// listAttemptToolCalls pages one attempt's persisted tool-call timeline
// (bounded result previews only; artifact bodies stay behind their own
// endpoints).
func (handler *Handler) listAttemptToolCalls(ctx context.Context, input *struct {
	Session         string `cookie:"__Host-quoin-session"`
	InvestigationID string `path:"investigationId"`
	AttemptID       string `path:"attemptId"`
	Cursor          string `query:"cursor"`
	Limit           int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
}) (*toolCallListBody, error) {
	if _, err := handler.principal(ctx, input.Session); err != nil {
		return nil, err
	}
	attemptID, err := strconv.ParseInt(input.AttemptID, 10, 64)
	if err != nil || attemptID <= 0 {
		return nil, problem(404, "not_found", "执行 Attempt 不存在。")
	}
	afterID, err := decodeIDCursor(input.Cursor)
	if err != nil {
		return nil, problem(400, "malformed_request", "分页游标无效，请刷新后重试。")
	}
	items, hasMore, err := handler.Service.ListToolCalls(ctx, attemptID, afterID, input.Limit)
	if err != nil {
		return nil, problem(500, "unavailable", "暂时无法读取工具调用，请重试。")
	}
	output := &toolCallListBody{}
	output.Body.Items = items
	if hasMore && len(items) > 0 {
		if id, err := strconv.ParseInt(items[len(items)-1].ID, 10, 64); err == nil {
			output.Body.NextCursor = encodeIDCursor(id)
		}
	}
	return output, nil
}

func createSendError(err error) error {
	switch {
	case errors.Is(err, investigation.ErrCommandReused):
		conflict := problem(409, "command_id_reused", "命令 ID 已被其他请求使用，请重新发起。")
		conflict.Conflict = map[string]any{"code": "command_id_reused"}
		return conflict
	case errors.Is(err, investigation.ErrMessageInvalid):
		return problemUnprocessable("消息必须包含正文或至少一个附件。")
	case errors.Is(err, investigation.ErrAttachmentInvalidRef):
		return problemUnprocessable("附件引用无效，请重新选择附件后重试。")
	case errors.Is(err, investigation.ErrAttachmentTooLarge):
		return problem(413, "payload_too_large", "附件超过大小上限（默认 10 MiB，可与管理员确认部署边界）。")
	case errors.Is(err, investigation.ErrInvalidSource), errors.Is(err, investigation.ErrSourceNotFound):
		return problemUnprocessable("来源引用无效，请返回告警或分析页面重新发起。")
	case errors.Is(err, investigation.ErrModelProviderMissing):
		return problem(503, "unavailable", "模型供应商尚未启用并通过能力探测，请管理员完成设置后再试。")
	default:
		return problem(500, "unavailable", "暂时无法发起调查，请重试。")
	}
}

func sendError(err error, investigationID int64) error {
	var headConflict *investigation.HeadConflictError
	switch {
	case errors.Is(err, investigation.ErrNotFound):
		return problem(404, "not_found", "调查不存在。")
	case errors.As(err, &headConflict):
		conflict := problem(409, "head_conflict", "对话已发生变化，请刷新后重试。")
		conflict.Conflict = map[string]any{
			"code":       "head_conflict",
			"objectType": "investigation",
			"objectId":   strconv.FormatInt(investigationID, 10),
		}
		if headConflict.CurrentHead != nil {
			conflict.Conflict["headMessageId"] = strconv.FormatInt(*headConflict.CurrentHead, 10)
		} else {
			conflict.Conflict["headMessageId"] = nil
		}
		return conflict
	case errors.Is(err, investigation.ErrActiveAttempt):
		conflict := problem(409, "active_conflict", "当前对话正在生成回复，请稍后再发送。")
		conflict.Conflict = map[string]any{"code": "active_conflict", "objectType": "investigation", "objectId": strconv.FormatInt(investigationID, 10)}
		return conflict
	case errors.Is(err, investigation.ErrCommandReused):
		conflict := problem(409, "command_id_reused", "命令 ID 已被其他请求使用，请重新发起。")
		conflict.Conflict = map[string]any{"code": "command_id_reused"}
		return conflict
	case errors.Is(err, investigation.ErrMessageInvalid):
		return problemUnprocessable("消息必须包含正文或至少一个附件。")
	case errors.Is(err, investigation.ErrAttachmentInvalidRef):
		return problemUnprocessable("附件引用无效，请重新选择附件后重试。")
	case errors.Is(err, investigation.ErrAttachmentTooLarge):
		return problem(413, "payload_too_large", "附件超过大小上限（默认 10 MiB，可与管理员确认部署边界）。")
	case errors.Is(err, investigation.ErrModelProviderMissing):
		return problem(503, "unavailable", "模型供应商尚未启用并通过能力探测，请管理员完成设置后再试。")
	default:
		return problem(500, "unavailable", "暂时无法发送消息，请重试。")
	}
}

func parseSources(wire []sourceInputWire) ([]investigation.SourceInput, error) {
	var sources []investigation.SourceInput
	for _, item := range wire {
		id, err := strconv.ParseInt(item.SourceID, 10, 64)
		if err != nil || id <= 0 {
			return nil, investigation.ErrInvalidSource
		}
		sources = append(sources, investigation.SourceInput{Type: item.Type, SourceID: id})
	}
	return sources, nil
}
