package appinvestigation

// T15 turn-command HTTP surface: latest-turn Undo, the explicit Stop
// fence and Retry (DATA-INVEST-002, HTTP-COMMAND-005). The handlers live
// beside the T13 surface but in their own file: the route table stays in
// Register (http.go) while these own the turn semantics and their frozen
// error envelopes.

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/investigation"
)

type attemptBody struct {
	Body investigation.InvestigationAttemptItem `json:"body"`
}

// dispatchCancelFence delivers the committed cancellation fence to the
// runtime (RUNTIME-CANCEL-001). The fence is already durable: a dispatch
// failure leaves the attempt Cancelling for the reconciler and is logged,
// never surfaced as a failed command.
func (handler *Handler) dispatchCancelFence(ctx context.Context, attemptID int64) {
	if handler.CancelDispatch == nil {
		return
	}
	if err := handler.CancelDispatch(ctx, attemptID); err != nil {
		sharedops.LogEvent("quoin", "error", "investigation.cancel_dispatch", fmt.Sprintf("attempt=%d %v", attemptID, err))
	}
}

// undoInvestigationMessage withdraws the latest user turn and all its
// successors in one transaction (DATA-INVEST-002); the current head
// fences the withdrawal and a stale head conflicts deterministically.
func (handler *Handler) undoInvestigationMessage(ctx context.Context, input *struct {
	Session         string `cookie:"__Host-quoin-session"`
	InvestigationID string `path:"investigationId"`
	Body            struct {
		ClientCommandID       string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
		ExpectedHeadMessageID string `json:"expectedHeadMessageId" pattern:"^[0-9]+$"`
	}
}) (*investigationDetailBody, error) {
	principalID, err := handler.principal(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	investigationID, err := strconv.ParseInt(input.InvestigationID, 10, 64)
	if err != nil || investigationID <= 0 {
		return nil, problem(404, "not_found", "调查不存在。")
	}
	expectedHead, err := strconv.ParseInt(input.Body.ExpectedHeadMessageID, 10, 64)
	if err != nil || expectedHead <= 0 {
		return nil, problemUnprocessable("expectedHeadMessageId 无效。")
	}
	outcome, err := handler.Service.Undo(ctx, principalID, input.Body.ClientCommandID, investigationID, expectedHead)
	if err != nil {
		return nil, undoError(err, investigationID)
	}
	if outcome.DispatchRequired {
		handler.dispatchCancelFence(ctx, outcome.AttemptID)
	}
	detail, err := handler.Service.Get(ctx, investigationID)
	if err != nil {
		if errors.Is(err, investigation.ErrNotFound) {
			return nil, problem(404, "not_found", "调查不存在。")
		}
		return nil, problem(500, "unavailable", "撤回已提交，但暂时无法读取详情，请刷新页面。")
	}
	return &investigationDetailBody{Body: detail}, nil
}

// cancelInvestigationAttempt commits the explicit Stop fence
// (HTTP-COMMAND-005): success already committed answers the completed
// object; a still-active attempt requires the current row version.
func (handler *Handler) cancelInvestigationAttempt(ctx context.Context, input *struct {
	Session         string `cookie:"__Host-quoin-session"`
	InvestigationID string `path:"investigationId"`
	AttemptID       string `path:"attemptId"`
	Body            struct {
		ClientCommandID    string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
		ExpectedRowVersion int64  `json:"expectedRowVersion" minimum:"1"`
	}
}) (*attemptBody, error) {
	principalID, err := handler.principal(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	investigationID, attemptID, err := parseAttemptPath(input.InvestigationID, input.AttemptID)
	if err != nil {
		return nil, problem(404, "not_found", "执行 Attempt 不存在。")
	}
	if input.Body.ExpectedRowVersion < 1 {
		return nil, problemUnprocessable("expectedRowVersion 无效。")
	}
	outcome, err := handler.Service.Cancel(ctx, principalID, input.Body.ClientCommandID, investigationID, attemptID, input.Body.ExpectedRowVersion)
	if err != nil {
		return nil, stopError(err, attemptID)
	}
	if outcome.DispatchRequired {
		handler.dispatchCancelFence(ctx, outcome.AttemptID)
	}
	view, err := handler.Service.AttemptView(ctx, investigationID, attemptID)
	if err != nil {
		return nil, problem(500, "unavailable", "停止已受理，但暂时无法读取执行状态，请刷新页面。")
	}
	return &attemptBody{Body: view}, nil
}

// retryInvestigationAttempt creates a new attempt re-answering a failed
// turn's user message (DATA-INVEST-002); withdrawn or branch-left
// messages conflict and never re-enter the model context.
func (handler *Handler) retryInvestigationAttempt(ctx context.Context, input *struct {
	Session         string `cookie:"__Host-quoin-session"`
	InvestigationID string `path:"investigationId"`
	AttemptID       string `path:"attemptId"`
	Body            struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
	}
}) (*attemptBody, error) {
	principalID, err := handler.principal(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	investigationID, attemptID, err := parseAttemptPath(input.InvestigationID, input.AttemptID)
	if err != nil {
		return nil, problem(404, "not_found", "执行 Attempt 不存在。")
	}
	newAttemptID, err := handler.Service.Retry(ctx, principalID, input.Body.ClientCommandID, investigationID, attemptID)
	if err != nil {
		return nil, retryError(err, investigationID)
	}
	if handler.Dispatch != nil {
		if err := handler.Dispatch(ctx, newAttemptID); err != nil {
			sharedops.LogEvent("quoin", "error", "investigation.dispatch_failed", fmt.Sprintf("attempt=%d %v", newAttemptID, err))
		}
	}
	view, err := handler.Service.AttemptView(ctx, investigationID, newAttemptID)
	if err != nil {
		return nil, problem(500, "unavailable", "重试已受理，但暂时无法读取执行状态，请刷新页面。")
	}
	return &attemptBody{Body: view}, nil
}

func parseAttemptPath(investigationParam, attemptParam string) (int64, int64, error) {
	investigationID, err := strconv.ParseInt(investigationParam, 10, 64)
	if err != nil || investigationID <= 0 {
		return 0, 0, err
	}
	attemptID, err := strconv.ParseInt(attemptParam, 10, 64)
	if err != nil || attemptID <= 0 {
		return 0, 0, err
	}
	return investigationID, attemptID, nil
}

func undoError(err error, investigationID int64) error {
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
	case errors.Is(err, investigation.ErrCommandReused):
		conflict := problem(409, "command_id_reused", "命令 ID 已被其他请求使用，请重新发起。")
		conflict.Conflict = map[string]any{"code": "command_id_reused"}
		return conflict
	case errors.Is(err, investigation.ErrNoUserTurn):
		conflict := problem(409, "head_conflict", "当前没有可撤回的回合，请刷新后重试。")
		conflict.Conflict = map[string]any{"code": "head_conflict", "objectType": "investigation", "objectId": strconv.FormatInt(investigationID, 10), "headMessageId": nil}
		return conflict
	default:
		return problem(500, "unavailable", "暂时无法撤回消息，请重试。")
	}
}

func stopError(err error, attemptID int64) error {
	var rowVersion *attempt.RowVersionError
	switch {
	case errors.Is(err, investigation.ErrNotFound):
		return problem(404, "not_found", "执行 Attempt 不存在。")
	case errors.As(err, &rowVersion):
		conflict := problem(409, "row_version_conflict", "执行状态已变化，请刷新后重试。")
		conflict.Conflict = map[string]any{
			"code":       "row_version_conflict",
			"objectType": "execution_attempt",
			"objectId":   strconv.FormatInt(attemptID, 10),
			"rowVersion": rowVersion.Current,
		}
		return conflict
	case errors.Is(err, investigation.ErrCommandReused):
		conflict := problem(409, "command_id_reused", "命令 ID 已被其他请求使用，请重新发起。")
		conflict.Conflict = map[string]any{"code": "command_id_reused"}
		return conflict
	default:
		return problem(500, "unavailable", "暂时无法停止回复，请重试。")
	}
}

func retryError(err error, investigationID int64) error {
	switch {
	case errors.Is(err, investigation.ErrNotFound):
		return problem(404, "not_found", "执行 Attempt 不存在。")
	case errors.Is(err, investigation.ErrAttemptNotFailed):
		conflict := problem(409, "active_conflict", "只有失败的回合可以重试。")
		conflict.Conflict = map[string]any{"code": "active_conflict", "objectType": "investigation", "objectId": strconv.FormatInt(investigationID, 10)}
		return conflict
	case errors.Is(err, investigation.ErrRetryMessageWithdrawn):
		conflict := problem(409, "active_conflict", "该回合已撤回，不能重新进入模型上下文。")
		conflict.Conflict = map[string]any{"code": "active_conflict", "objectType": "investigation", "objectId": strconv.FormatInt(investigationID, 10)}
		return conflict
	case errors.Is(err, investigation.ErrActiveAttempt):
		conflict := problem(409, "active_conflict", "当前对话正在生成回复，请稍后再重试。")
		conflict.Conflict = map[string]any{"code": "active_conflict", "objectType": "investigation", "objectId": strconv.FormatInt(investigationID, 10)}
		return conflict
	case errors.Is(err, investigation.ErrCommandReused):
		conflict := problem(409, "command_id_reused", "命令 ID 已被其他请求使用，请重新发起。")
		conflict.Conflict = map[string]any{"code": "command_id_reused"}
		return conflict
	case errors.Is(err, investigation.ErrModelProviderMissing):
		return problem(503, "unavailable", "模型供应商尚未启用并通过能力探测，请管理员完成设置后再试。")
	default:
		return problem(500, "unavailable", "暂时无法重试，请稍后重试。")
	}
}
