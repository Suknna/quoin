package app

// Runtime slot HTTP surface (T06): prepareRuntimeRegistration,
// revealRuntimeRegistrationToken, retireRuntimeCredential and the enriched
// getRuntimeStatus projection.

import (
	"context"
	"errors"
	"net/http"
	"time"

	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"github.com/Suknna/quoin/internal/quoin/secrets"
	"github.com/danielgtaylor/huma/v2"
)

func runtimeConflict(err error) error {
	var version *qruntime.RowVersionError
	switch {
	case errors.As(err, &version):
		problemErr := problem(http.StatusConflict, "row_version_conflict", "该组件状态刚被其他操作修改，请刷新后重试。")
		problemErr.Conflict = map[string]any{"code": "row_version_conflict", "objectType": "runtime_slot", "rowVersion": version.Current}
		return problemErr
	case errors.Is(err, qruntime.ErrActiveConflict):
		problemErr := problem(http.StatusConflict, "active_conflict", "该组件仍有进行中的工作，暂时不能执行此操作。")
		problemErr.Conflict = map[string]any{"code": "active_conflict", "objectType": "runtime_slot"}
		return problemErr
	case errors.Is(err, qruntime.ErrNotFound):
		return problem(http.StatusNotFound, "not_found", "目标组件不存在。")
	case errors.Is(err, qruntime.ErrTokenGone):
		return problem(http.StatusGone, "resource_expired", "注册令牌已过期或已消费；请重新发起注册准备。")
	default:
		return problem(http.StatusInternalServerError, "unavailable", "暂时无法完成操作，请重试。")
	}
}

func (application *apiServer) registerRuntimeRoutes(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/runtime-slots/{slot}/registration/prepare", OperationID: "prepareRuntimeRegistration"}, application.prepareRuntimeRegistration)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/runtime-slots/registration-token/reveal", OperationID: "revealRuntimeRegistrationToken"}, application.revealRuntimeRegistrationToken)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/runtime-slots/{slot}/retiring-credential/retire", OperationID: "retireRuntimeCredential"}, application.retireRuntimeCredential)
}

func (application *apiServer) prepareRuntimeRegistration(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Slot    string `path:"slot"`
	Body    struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128"`
		ExpectedRow     int64  `json:"expectedRowVersion" minimum:"1"`
	}
}) (*struct {
	Body struct {
		Slot                       string `json:"slot"`
		State                      string `json:"state"`
		CurrentGeneration          int64  `json:"currentGeneration"`
		RowVersion                 int64  `json:"rowVersion"`
		RegistrationTokenAvailable bool   `json:"registrationTokenAvailable"`
		RegistrationTokenHandle    string `json:"registrationTokenHandle,omitempty"`
	}
}, error) {
	if _, err := application.authenticateAdmin(ctx, input.Session, "准备 Runtime 注册"); err != nil {
		return nil, err
	}
	if !qruntime.ValidSlot(input.Slot) {
		return nil, problem(http.StatusBadRequest, "malformed_request", "slot 必须是 plinth 或 lintel。")
	}
	view, handle, available, err := application.runtime.PrepareRegistration(ctx, input.Slot, input.Body.ExpectedRow, secrets.SessionDigest(input.Session))
	if err != nil {
		return nil, runtimeConflict(err)
	}
	output := &struct {
		Body struct {
			Slot                       string `json:"slot"`
			State                      string `json:"state"`
			CurrentGeneration          int64  `json:"currentGeneration"`
			RowVersion                 int64  `json:"rowVersion"`
			RegistrationTokenAvailable bool   `json:"registrationTokenAvailable"`
			RegistrationTokenHandle    string `json:"registrationTokenHandle,omitempty"`
		}
	}{}
	output.Body.Slot = view.Slot
	output.Body.State = string(view.State)
	output.Body.CurrentGeneration = view.CurrentGeneration
	output.Body.RowVersion = view.RowVersion
	output.Body.RegistrationTokenAvailable = available
	if available {
		output.Body.RegistrationTokenHandle = handle
	}
	return output, nil
}

func (application *apiServer) revealRuntimeRegistrationToken(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Body    struct {
		RegistrationTokenHandle string `json:"registrationTokenHandle" minLength:"32" maxLength:"256"`
	}
}) (*struct {
	Body struct {
		Slot              string `json:"slot"`
		Generation        int64  `json:"generation"`
		RegistrationToken string `json:"registrationToken"`
	}
}, error) {
	session, err := application.authenticateAdmin(ctx, input.Session, "显示 Runtime 注册令牌")
	if err != nil {
		return nil, err
	}
	raw, slot, generation, revealErr := application.runtime.RevealToken(input.Body.RegistrationTokenHandle, secrets.SessionDigest(input.Session))
	if revealErr != nil {
		return nil, runtimeConflict(revealErr)
	}
	// Audit the reveal without the handle or raw token (SEC-REVEAL-004).
	if auditErr := application.alerts.RecordRevealAudit(ctx, session.User.ID, generation, "success", nowTimestamp()); auditErr != nil {
		return nil, problem(http.StatusInternalServerError, "unavailable", "暂时无法记录审计，请重试。")
	}
	output := &struct {
		Body struct {
			Slot              string `json:"slot"`
			Generation        int64  `json:"generation"`
			RegistrationToken string `json:"registrationToken"`
		}
	}{}
	output.Body.Slot = slot
	output.Body.Generation = generation
	output.Body.RegistrationToken = raw
	return output, nil
}

func (application *apiServer) retireRuntimeCredential(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Slot    string `path:"slot"`
	Body    struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128"`
		ExpectedRow     int64  `json:"expectedRowVersion" minimum:"1"`
	}
}) (*struct {
	Body qruntime.SlotView
}, error) {
	if _, err := application.authenticateAdmin(ctx, input.Session, "退休 Runtime 旧凭据"); err != nil {
		return nil, err
	}
	view, err := application.runtime.Retire(ctx, input.Slot, input.Body.ExpectedRow)
	if err != nil {
		return nil, runtimeConflict(err)
	}
	return &struct {
		Body qruntime.SlotView
	}{Body: view}, nil
}

func nowTimestamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }
