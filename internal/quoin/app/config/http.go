// Package appconfig owns the configuration HTTP surface (T16): the frozen
// business-system routes (list/detail/upload/publish), the Label Contract
// draft/activation routes required to reach a current contract, the
// read-only Journey Catalog view and the business-system YAML template. The
// Handler is a seam wired by the app package exactly like the investigation
// surface; it owns no SQL.
package appconfig

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/businesssystem"
	"github.com/Suknna/quoin/internal/quoin/config"
	"github.com/Suknna/quoin/internal/quoin/labelcontract"
	"github.com/Suknna/quoin/internal/quoin/tools/thanos"
	"github.com/danielgtaylor/huma/v2"
)

// Handler wires the configuration domain services into the HTTP surface.
type Handler struct {
	Systems   *businesssystem.Service
	Contracts *labelcontract.Service
	// Authenticate resolves any full (non-restricted) session to its
	// principal id; every logged-in user may read the config surface (Q209).
	Authenticate func(ctx context.Context, cookie string) (int64, error)
	// AuthenticateAdmin additionally enforces the Admin role for the frozen
	// write commands (HTTP-CONFIG-001/002/004).
	AuthenticateAdmin func(ctx context.Context, cookie string) (int64, error)
	// CancelDispatch sends a committed, bound cancellation fence to Plinth.
	// The domain service commits the fence before this best-effort delivery.
	CancelDispatch func(ctx context.Context, attemptID int64) error
	// DispatchResourceRefresh scans and dispatches committed queued resource attempts.
	DispatchResourceRefresh func(ctx context.Context)
	// DispatchConfigVerification scans and dispatches committed queued
	// PromQL verification attempts (created while Plinth is already connected).
	DispatchConfigVerification func(ctx context.Context)
}

// Register mounts the huma-owned routes; the two multipart uploads and the
// YAML template own their response heads and are mounted by the app package
// on the raw mux.
func (handler *Handler) Register(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/business-systems", OperationID: "listBusinessSystems"}, handler.listBusinessSystems)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/business-systems/{systemKey}", OperationID: "getBusinessSystem"}, handler.getBusinessSystem)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/business-systems/{systemKey}/config", OperationID: "listBusinessSystemConfigs"}, handler.listBusinessSystemConfigs)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/business-systems/{systemKey}/config/{versionId}", OperationID: "getBusinessSystemConfig"}, handler.getBusinessSystemConfig)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/business-systems/{systemKey}/config/{versionId}/publish", OperationID: "publishBusinessSystemConfig"}, handler.publishBusinessSystemConfig)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/business-systems/{systemKey}/config/{versionId}/verifications", OperationID: "listConfigVerificationRuns"}, handler.listConfigVerificationRuns)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/business-systems/{systemKey}/config/{versionId}/verifications", OperationID: "runConfigVerificationRun"}, handler.runConfigVerificationRun)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/business-systems/{systemKey}/config/{versionId}/verifications/{verificationRunId}", OperationID: "getConfigVerificationRun"}, handler.getConfigVerificationRun)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/business-systems/{systemKey}/config/{versionId}/verifications/{verificationRunId}/cancel", OperationID: "cancelConfigVerificationRun"}, handler.cancelConfigVerificationRun)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/business-systems/{systemKey}/resources:refresh", OperationID: "startResourceRefresh"}, handler.startResourceRefresh)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/business-systems/{systemKey}/resources", OperationID: "listObservedResources"}, handler.listObservedResources)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/business-systems/{systemKey}/resources/{resourceId}", OperationID: "getObservedResource"}, handler.getObservedResource)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/business-systems/{systemKey}/resource-refresh-runs/{resourceRefreshRunId}", OperationID: "getResourceRefreshRun"}, handler.getResourceRefreshRun)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/label-contracts", OperationID: "listLabelContracts"}, handler.listLabelContracts)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/label-contracts/{contractVersion}", OperationID: "getLabelContract"}, handler.getLabelContract)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/label-contracts/{contractVersion}/activate", OperationID: "activateLabelContract"}, handler.activateLabelContract)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/label-contracts/{contractVersion}/readiness", OperationID: "getLabelContractActivationReadiness"}, handler.getLabelContractActivationReadiness)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/journey-catalog", OperationID: "getJourneyCatalog"}, handler.getJourneyCatalog)
}

// problemError is the frozen ErrorModel envelope (HTTP-ERROR-002/004).
type problemError struct {
	status      int
	Code        string              `json:"code"`
	Message     string              `json:"message"`
	Retryable   bool                `json:"retryable"`
	FieldErrors []config.FieldError `json:"fieldErrors,omitempty"`
	Conflict    map[string]any      `json:"conflict,omitempty"`
}

func (p *problemError) Error() string             { return p.Message }
func (p *problemError) GetStatus() int            { return p.status }
func (p *problemError) ContentType(string) string { return "application/problem+json" }

func problem(status int, code, message string) *problemError {
	return &problemError{status: status, Code: code, Message: message, Retryable: status >= 500 || status == 429}
}

// validationProblem renders the complete fieldErrors list (HTTP-ERROR-001).
func validationProblem(validation *config.ValidationError) *problemError {
	result := problem(http.StatusUnprocessableEntity, "validation_failed", "配置内容未通过静态校验，请按逐项错误修正后重试。")
	result.FieldErrors = validation.Errors
	return result
}

// mapDomainError maps the service error lattice onto the frozen envelope.
func mapDomainError(err error) error {
	var validation *config.ValidationError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &validation):
		return validationProblem(validation)
	case errors.Is(err, businesssystem.ErrCommandReused), errors.Is(err, labelcontract.ErrCommandReused):
		conflict := problem(http.StatusConflict, "command_id_reused", "命令 ID 已被其他请求使用，请重新发起操作。")
		conflict.Conflict = map[string]any{"code": "command_id_reused"}
		return conflict
	case errors.Is(err, businesssystem.ErrNotFound), errors.Is(err, labelcontract.ErrNotFound):
		return problem(http.StatusNotFound, "not_found", "目标对象不存在，可能刚被删除或路径不正确。")
	case errors.Is(err, businesssystem.ErrBrowserVerificationUnavailable):
		return problem(http.StatusServiceUnavailable, "external_dependency_unavailable", "该配置包含浏览器检查，当前版本暂不支持执行浏览器验证。")
	case errors.Is(err, thanos.ErrThanosUnavailable):
		return problem(http.StatusServiceUnavailable, "external_dependency_unavailable", "尚未配置可用的 Thanos 连接，暂时无法执行配置验证或资源刷新。")
	}
	var systemsConflict *businesssystem.ConflictError
	if errors.As(err, &systemsConflict) {
		return conflictProblem("business_system", systemsConflict.Code, systemsConflict.Detail, systemsConflict.ObjectID, systemsConflict.CurrentVersion)
	}
	var contractConflict *labelcontract.ConflictError
	if errors.As(err, &contractConflict) {
		return conflictProblem("label_contract", contractConflict.Code, contractConflict.Detail, contractConflict.ObjectID, contractConflict.CurrentVersion)
	}
	var badRequest *labelcontract.BadRequestError
	if errors.As(err, &badRequest) {
		return problem(http.StatusBadRequest, "malformed_request", badRequest.Reason)
	}
	return problem(http.StatusInternalServerError, "unavailable", "暂时无法完成操作，请稍后重试。")
}

// conflictProblem renders the frozen ConflictErrorModel block; codes stay
// within the frozen discriminator set (row_version_conflict /
// current_pointer_conflict).
func conflictProblem(objectType, code, detail string, objectID int64, currentVersion *int64) *problemError {
	conflict := problem(http.StatusConflict, code, detail)
	block := map[string]any{"code": code, "objectType": objectType}
	if objectID != 0 {
		block["objectId"] = strconv.FormatInt(objectID, 10)
	}
	if code == "current_pointer_conflict" {
		if currentVersion != nil {
			block["currentVersionId"] = strconv.FormatInt(*currentVersion, 10)
		} else {
			block["currentVersionId"] = nil
		}
	}
	conflict.Conflict = block
	return conflict
}

func (handler *Handler) reader(ctx context.Context, cookie string) (int64, error) {
	if handler.Authenticate == nil {
		return 0, errors.New("config handler not wired")
	}
	return handler.Authenticate(ctx, cookie)
}

func (handler *Handler) admin(ctx context.Context, cookie string) (int64, error) {
	if handler.AuthenticateAdmin == nil {
		return 0, errors.New("config handler not wired")
	}
	return handler.AuthenticateAdmin(ctx, cookie)
}

func parseLocator(raw string) (int64, *problemError) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, problem(http.StatusNotFound, "not_found", "目标对象不存在，可能刚被删除或路径不正确。")
	}
	return value, nil
}

func encodeCursor(raw string) string {
	return base64Encode([]byte(raw))
}

func decodeCursor(raw string) string {
	decoded, err := base64Decode(raw)
	if err != nil {
		return ""
	}
	return string(decoded)
}
