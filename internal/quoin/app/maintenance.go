package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/maintenance"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

type maintenanceOutput struct {
	CacheControl string                   `header:"Cache-Control"`
	Body         maintenanceStateResponse `json:"body"`
}

type maintenanceStateResponse struct {
	Active     bool              `json:"active"`
	Reason     string            `json:"reason,omitempty"`
	RowVersion int64             `json:"rowVersion"`
	Items      []maintenanceItem `json:"items"`
}

type maintenanceItem struct {
	Kind       string `json:"kind"`
	ObjectKey  string `json:"objectKey"`
	SafeState  string `json:"safeState"`
	DetailCode string `json:"detailCode"`
}

type exitMaintenanceInput struct {
	Session string `cookie:"__Host-quoin-session"`
	Body    struct {
		ClientCommandID    string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
		ExpectedRowVersion int64  `json:"expectedRowVersion" minimum:"1"`
		ExpectedReason     string `json:"expectedReason" enum:"Restore,Upgrade,RootKeyRebind"`
	}
}

// NewMaintenanceHandler retains the testable public constructor.
func NewMaintenanceHandler(service *auth.Service, database *sql.DB, publicOrigin string) (http.Handler, error) {
	return newMaintenanceHandler(NewMaintenanceAPIServer(service, database, ""), publicOrigin)
}

// newMaintenanceHandler exposes exactly the OpenAPI Restore allowlist. Its
// domain authorities are durable-only; the caller wires only the restricted
// Runtime control plane, never schedulers, GC, backup or task dispatch.
func newMaintenanceHandler(application *apiServer, publicOrigin string) (http.Handler, error) {
	configureHumaErrorModel()
	apiMux := http.NewServeMux()
	staticMux := http.NewServeMux()
	apiConfig := huma.DefaultConfig("Quoin maintenance API", "1.0.0-draft")
	apiConfig.OpenAPIPath, apiConfig.DocsPath, apiConfig.SchemasPath = "", "", ""
	apiConfig.Transformers, apiConfig.CreateHooks = []huma.Transformer{}, nil
	api := humago.New(apiMux, apiConfig)
	// Maintenance defaults every non-allowlisted product API — including HEAD
	// and extension methods — to a retriable unavailable response rather than
	// pretending it does not exist. Explicit Huma operation routes below win
	// over this path-prefix catch-all; the SPA's GET / fallback never sees /api/.
	maintenanceUnavailable := func(writer http.ResponseWriter, request *http.Request) {
		cookie, err := request.Cookie("__Host-quoin-session")
		if err != nil {
			writeBackupProblem(writer, http.StatusUnauthorized, "unauthenticated", "请重新登录后再使用该功能。", false)
			return
		}
		if _, err := application.authenticateFull(request.Context(), cookie.Value, "使用维护期间不可用的功能"); err != nil {
			writeMaintenanceAuthenticationProblem(writer, err)
			return
		}
		writeBackupProblem(writer, http.StatusServiceUnavailable, "unavailable", "系统正在维护中，请完成维护后重试。", true)
	}
	apiMux.HandleFunc("/api/", maintenanceUnavailable)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/auth/login", OperationID: "login"}, application.login)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/auth/me", OperationID: "getCurrentUser"}, application.me)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/runtime", OperationID: "getRuntimeStatus"}, application.runtimeStatus)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/audit-events", OperationID: "listAuditEvents"}, application.listAuditEvents)
	huma.Register(api, huma.Operation{Method: http.MethodPut, Path: "/api/v1/auth/password", OperationID: "changeOwnPassword"}, application.maintenanceChangePassword)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/auth/logout", OperationID: "logout"}, application.maintenanceLogout)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/maintenance", OperationID: "getMaintenanceState"}, application.getMaintenanceState)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/maintenance/exit", OperationID: "exitMaintenance"}, application.exitMaintenance)
	application.registerMaintenanceTrustRebuildRoutes(api)
	application.registerStatic(staticMux)
	root := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/api/") {
			apiMux.ServeHTTP(writer, request)
			return
		}
		staticMux.ServeHTTP(writer, request)
	})
	csrf := http.NewCrossOriginProtection()
	if err := csrf.AddTrustedOrigin(publicOrigin); err != nil {
		return nil, err
	}
	return securityHeaders(requireBrowserOrigin(csrf.Handler(root))), nil
}

// registerMaintenanceTrustRebuildRoutes is intentionally an explicit projection
// of the OpenAPI trust-rebuild allowlist. Do not reuse normal route registrars:
// they contain ordinary work-creation and unmarked operations which are closed
// throughout maintenance.
func (application *apiServer) registerMaintenanceTrustRebuildRoutes(api huma.API) {
	// User reconstruction.
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/admin/users", OperationID: "listUsers"}, application.listUsers)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/admin/users", OperationID: "createUser"}, application.createUser)
	huma.Register(api, huma.Operation{Method: http.MethodPatch, Path: "/api/v1/admin/users/{userId}", OperationID: "updateUser"}, application.updateUser)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/admin/users/{userId}/reset-password", OperationID: "resetUserPassword"}, application.resetUserPassword)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/admin/users/{userId}/revoke-sessions", OperationID: "revokeUserSessions"}, application.revokeUserSessions)

	// Runtime credential reconstruction. These operations only mutate the
	// durable slot/credential state; they do not start a scheduler or dispatch.
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/runtime-slots/{slot}/registration/prepare", OperationID: "prepareRuntimeRegistration"}, application.prepareRuntimeRegistration)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/runtime-slots/registration-token/reveal", OperationID: "revealRuntimeRegistrationToken"}, application.revealRuntimeRegistrationToken)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/runtime-slots/{slot}/retiring-credential/retire", OperationID: "retireRuntimeCredential"}, application.retireRuntimeCredential)

	// Connection reconstruction. Probe/enable are deliberately absent: both can
	// create external work and are not OpenAPI-marked for maintenance.
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/connections", OperationID: "listConnections"}, application.listConnections)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/connections/{connectionName}", OperationID: "getConnection"}, application.getConnection)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/connections/{connectionName}/rotate", OperationID: "rotateConnectionCredential"}, application.rotateConnectionCredential)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/connections/{connectionName}/disable", OperationID: "disableConnection"}, application.disableConnection)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/connections/{connectionName}/probe-attempts/{attemptId}", OperationID: "getConnectionProbeAttempt"}, application.getConnectionProbeAttempt)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/connections/{connectionName}/probe-results", OperationID: "listConnectionProbeResults"}, application.listConnectionProbeResults)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/connections/{connectionName}/revisions", OperationID: "listConnectionRevisions"}, application.listConnectionRevisions)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/connections/{connectionName}/generations", OperationID: "listCredentialGenerations"}, application.listCredentialGenerations)

	// Alert-source reconstruction operations implemented by the current app.
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/alert-sources", OperationID: "listAlertSources"}, application.listAlertSources)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/alert-sources/{sourceKey}", OperationID: "getAlertSource"}, application.getAlertSource)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/alert-sources/{sourceKey}/credentials", OperationID: "listAlertSourceCredentials"}, application.listAlertSourceCredentials)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/alert-sources/{sourceKey}/rotate", OperationID: "rotateAlertSourceCredential"}, application.rotateAlertSourceCredential)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/alert-sources/{sourceKey}/credentials/{credentialId}/retire", OperationID: "retireAlertSourceCredential"}, application.retireAlertSourceCredential)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/alert-sources/{sourceKey}/disable", OperationID: "disableAlertSource"}, application.disableAlertSource)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/alert-sources/credentials/reveal", OperationID: "revealAlertSourceCredential"}, application.revealAlertSourceCredential)
}

func writeMaintenanceAuthenticationProblem(writer http.ResponseWriter, err error) {
	var structured *problemError
	if errors.As(err, &structured) {
		writeBackupProblem(writer, structured.status, structured.Code, structured.Message, structured.Retryable)
		return
	}
	var status huma.StatusError
	if errors.As(err, &status) {
		if status.GetStatus() == http.StatusUnauthorized {
			writeBackupProblem(writer, http.StatusUnauthorized, "unauthenticated", "请重新登录后再使用该功能。", false)
			return
		}
		if status.GetStatus() >= http.StatusInternalServerError {
			writeBackupProblem(writer, status.GetStatus(), "unavailable", "暂时无法验证登录状态，请稍后重试。", true)
			return
		}
	}
	writeBackupProblem(writer, http.StatusInternalServerError, "unavailable", "暂时无法验证登录状态，请稍后重试。", true)
}

func (application *apiServer) maintenanceChangePassword(ctx context.Context, input *passwordInput) (*noContentOutput, error) {
	session, err := application.auth.Authenticate(ctx, input.Session)
	if err != nil {
		return nil, authFailure(err, "保存新密码")
	}
	if err := application.auth.ChangePassword(ctx, session, input.Body.CurrentPassword, input.Body.NewPassword); err != nil {
		if errors.Is(err, auth.ErrUnauthenticated) {
			return nil, huma.Error422UnprocessableEntity("当前密码不正确")
		}
		if errors.Is(err, auth.ErrPasswordPolicy) {
			return nil, huma.Error422UnprocessableEntity("新密码不满足密码策略，请更换后重试。")
		}
		return nil, huma.Error500InternalServerError("暂时无法保存新密码，请重试。", err)
	}
	if err := application.maintenance.MarkAdminPasswordSafe(ctx, session.User.ID); err != nil {
		return nil, huma.Error500InternalServerError("暂时无法更新恢复清单，请重试。", err)
	}
	return &noContentOutput{CacheControl: "no-store", Pragma: "no-cache"}, nil
}

func (application *apiServer) maintenanceLogout(ctx context.Context, input *authInput) (*noContentOutput, error) {
	session, err := application.auth.Authenticate(ctx, input.Session)
	if err != nil {
		return nil, authFailure(err, "完成登出")
	}
	if err := application.auth.Logout(ctx, session); err != nil {
		return nil, huma.Error500InternalServerError("无法完成登出", err)
	}
	return &noContentOutput{SetCookie: sessionCookie("", -time.Hour), ClearSiteData: `"cache", "cookies", "storage"`, CacheControl: "no-store", Pragma: "no-cache"}, nil
}

func (application *apiServer) getMaintenanceState(ctx context.Context, input *authInput) (*maintenanceOutput, error) {
	if _, err := application.auth.Authenticate(ctx, input.Session); err != nil {
		return nil, authFailure(err, "读取维护状态")
	}
	state, err := application.maintenance.State(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("暂时无法读取维护状态", err)
	}
	return &maintenanceOutput{CacheControl: "no-store", Body: maintenanceResponse(state)}, nil
}

func (application *apiServer) exitMaintenance(ctx context.Context, input *exitMaintenanceInput) (*maintenanceOutput, error) {
	session, err := application.authenticateAdmin(ctx, input.Session, "退出维护")
	if err != nil {
		return nil, err
	}
	state, err := application.maintenance.Exit(ctx, maintenance.ExitRequest{ActorID: session.User.ID, ClientCommandID: input.Body.ClientCommandID, ExpectedReason: input.Body.ExpectedReason, ExpectedRowVersion: input.Body.ExpectedRowVersion})
	if err != nil {
		if errors.Is(err, maintenance.ErrConflict) || errors.Is(err, maintenance.ErrCommandReused) {
			return nil, problem(http.StatusConflict, "maintenance_conflict", "维护状态或安全清单已变化，请刷新后重试。")
		}
		return nil, huma.Error500InternalServerError("暂时无法退出维护", err)
	}
	return &maintenanceOutput{CacheControl: "no-store", Body: maintenanceResponse(state)}, nil
}

func maintenanceResponse(state maintenance.State) maintenanceStateResponse {
	response := maintenanceStateResponse{Active: state.Active, Reason: state.Reason, RowVersion: state.RowVersion, Items: make([]maintenanceItem, 0, len(state.Items))}
	for _, item := range state.Items {
		response.Items = append(response.Items, maintenanceItem{Kind: item.Kind, ObjectKey: item.ObjectKey, SafeState: item.SafeState, DetailCode: item.DetailCode})
	}
	return response
}
