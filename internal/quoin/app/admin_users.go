package app

// Admin user/session/audit HTTP surface (T05): listUsers, createUser,
// updateUser, resetUserPassword, revokeUserSessions, listOwnSessions,
// revokeOwnSession, listAuditEvents. Writes go through the durable
// command ledger inside auth.Service; every error is emitted in the frozen
// problem+json envelope (code/message/retryable/conflict).

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/danielgtaylor/huma/v2"
)

// problemError serializes to the frozen ErrorModel shape (HTTP-ERROR-001).
type problemError struct {
	status      int            `json:"-"`
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

// commandUserError maps auth service errors to the frozen problem envelope.
func commandUserError(err error) error {
	var version *auth.RowVersionError
	switch {
	case errors.Is(err, auth.ErrActorChanged):
		return problem(http.StatusUnauthorized, "unauthenticated", "请重新登录。")
	case errors.Is(err, auth.ErrCommandReused):
		problemErr := problem(http.StatusConflict, "command_id_reused", "命令 ID 已被其他请求使用，请重新发起操作。")
		problemErr.Conflict = map[string]any{"code": "command_id_reused"}
		return problemErr
	case errors.As(err, &version):
		problemErr := problem(http.StatusConflict, "row_version_conflict", "该用户刚被其他操作修改，请刷新后重试。")
		problemErr.Conflict = map[string]any{"code": "row_version_conflict", "objectType": "user", "objectId": strconv.FormatInt(version.ObjectID, 10), "rowVersion": version.Current}
		return problemErr
	case errors.Is(err, auth.ErrLastAdmin):
		problemErr := problem(http.StatusConflict, "active_conflict", "系统必须保留至少一个有效的管理员；请先创建或启用另一个管理员。")
		problemErr.Conflict = map[string]any{"code": "active_conflict", "objectType": "user"}
		return problemErr
	case errors.Is(err, auth.ErrUsernameTaken):
		problemErr := problem(http.StatusConflict, "active_conflict", "用户名已存在，请换一个用户名。")
		problemErr.Conflict = map[string]any{"code": "active_conflict", "objectType": "user"}
		return problemErr
	case errors.Is(err, auth.ErrNotFound):
		return problem(http.StatusNotFound, "not_found", "目标对象不存在，可能刚被删除或路径不正确。")
	case errors.Is(err, auth.ErrPasswordPolicy):
		return problemUnprocessable("新密码不满足密码策略，请更换后重试。")
	case errors.Is(err, auth.ErrValidation):
		return problemUnprocessable("请求字段不满足要求，请检查后重试。")
	default:
		return problem(http.StatusInternalServerError, "unavailable", "暂时无法完成操作，请稍后重试。")
	}
}

func problemUnprocessable(message string) *problemError {
	return problem(http.StatusUnprocessableEntity, "validation_failed", message)
}

// authenticateWrite authenticates the caller and enforces the restricted
// session rule (HTTP-AUTH-006): every operation outside me/password/logout
// answers 403 with code=password_change_required until the password changes.
func (application *apiServer) authenticateFull(ctx context.Context, cookie string, action string) (auth.Session, error) {
	session, err := application.auth.Authenticate(ctx, cookie)
	if err != nil {
		return session, authFailure(err, action)
	}
	if session.User.PasswordChangeRequired {
		restricted := problem(http.StatusForbidden, "password_change_required", "请先完成临时密码修改后再使用该功能。")
		restricted.FieldErrors = []problemField{{Path: "", Reason: "password_change_required", Remediation: "请前往修改密码页面设置正式密码"}}
		return session, restricted
	}
	return session, nil
}

func (application *apiServer) authenticateAdmin(ctx context.Context, cookie string, action string) (auth.Session, error) {
	session, err := application.authenticateFull(ctx, cookie, action)
	if err != nil {
		return session, err
	}
	if session.User.Role != "admin" {
		return session, problem(http.StatusForbidden, "forbidden", "该操作需要管理员权限。")
	}
	return session, nil
}

func encodeCursor(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeCursor(raw string) string {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return ""
	}
	return string(decoded)
}

type usersListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items      []auth.User `json:"items"`
		NextCursor string      `json:"nextCursor,omitempty"`
	} `json:"body"`
}

func (application *apiServer) registerAdminUserRoutes(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/admin/users", OperationID: "listUsers"}, application.listUsers)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/admin/users", OperationID: "createUser"}, application.createUser)
	huma.Register(api, huma.Operation{Method: http.MethodPatch, Path: "/api/v1/admin/users/{userId}", OperationID: "updateUser"}, application.updateUser)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/admin/users/{userId}/reset-password", OperationID: "resetUserPassword"}, application.resetUserPassword)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/admin/users/{userId}/revoke-sessions", OperationID: "revokeUserSessions"}, application.revokeUserSessions)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/auth/sessions", OperationID: "listOwnSessions"}, application.listOwnSessions)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/auth/sessions/{sessionId}/revoke", OperationID: "revokeOwnSession", DefaultStatus: http.StatusNoContent}, application.revokeOwnSession)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/audit-events", OperationID: "listAuditEvents"}, application.listAuditEvents)
}

func (application *apiServer) listUsers(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Cursor  string `query:"cursor"`
	Limit   int    `query:"limit"`
}) (*usersListOutput, error) {
	if _, err := application.authenticateAdmin(ctx, input.Session, "读取用户列表"); err != nil {
		return nil, err
	}
	afterID, _ := strconv.ParseInt(decodeCursor(input.Cursor), 10, 64)
	users, more, err := application.auth.ListUsers(ctx, afterID, input.Limit)
	if err != nil {
		return nil, problem(http.StatusInternalServerError, "unavailable", "暂时无法读取用户列表，请稍后重试。")
	}
	output := &usersListOutput{CacheControl: "no-store"}
	output.Body.Items = users
	if more && len(users) > 0 {
		output.Body.NextCursor = encodeCursor(users[len(users)-1].Locator)
	}
	return output, nil
}

func (application *apiServer) createUser(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Body    struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128"`
		Username        string `json:"username" maxLength:"200"`
		DisplayName     string `json:"displayName" maxLength:"200"`
		Role            string `json:"role"`
		Password        string `json:"password" minLength:"15" maxLength:"128"`
	}
}) (*struct {
	Status int `header:"-"`
	Body   auth.User
}, error) {
	session, err := application.authenticateAdmin(ctx, input.Session, "创建用户")
	if err != nil {
		return nil, err
	}
	// Digest covers the non-secret semantic fields only (DATA-COMMAND-002):
	// the password value never enters any persisted digest (SEC-KEY-008).
	digest := auth.DigestCommand("user.create", map[string]any{
		"username": input.Body.Username, "displayName": input.Body.DisplayName, "role": input.Body.Role,
	})
	result, _, err := application.auth.CreateUser(ctx, session, auth.CreateUserInput{
		ClientCommandID: input.Body.ClientCommandID, Digest: digest,
		Username: input.Body.Username, DisplayName: input.Body.DisplayName, Role: input.Body.Role, Password: input.Body.Password,
	})
	if err != nil {
		return nil, commandUserError(err)
	}
	return &struct {
		Status int `header:"-"`
		Body   auth.User
	}{Status: http.StatusCreated, Body: *result.User}, nil
}

func parseLocatorParam(raw string) (int64, error) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, problem(http.StatusBadRequest, "malformed_request", "路径中的对象编号格式不正确。")
	}
	return value, nil
}

func (application *apiServer) updateUser(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	UserID  string `path:"userId"`
	Body    struct {
		ClientCommandID string  `json:"clientCommandId" minLength:"8" maxLength:"128"`
		ExpectedRow     int64   `json:"expectedRowVersion" minimum:"1"`
		DisplayName     *string `json:"displayName,omitempty" maxLength:"200"`
		Enabled         *bool   `json:"enabled,omitempty"`
		Role            *string `json:"role,omitempty"`
	}
}) (*struct {
	Body auth.User
}, error) {
	session, err := application.authenticateAdmin(ctx, input.Session, "更新用户")
	if err != nil {
		return nil, err
	}
	userID, err := parseLocatorParam(input.UserID)
	if err != nil {
		return nil, err
	}
	fields := map[string]any{"userId": userID, "expectedRowVersion": input.Body.ExpectedRow}
	if input.Body.DisplayName != nil {
		fields["displayName"] = *input.Body.DisplayName
	}
	if input.Body.Enabled != nil {
		fields["enabled"] = *input.Body.Enabled
	}
	if input.Body.Role != nil {
		fields["role"] = *input.Body.Role
	}
	result, _, err := application.auth.UpdateUser(ctx, session, auth.UpdateUserInput{
		ClientCommandID: input.Body.ClientCommandID, Digest: auth.DigestCommand("user.update", fields),
		UserID: userID, ExpectedRow: input.Body.ExpectedRow,
		DisplayName: input.Body.DisplayName, Enabled: input.Body.Enabled, Role: input.Body.Role,
	})
	if err != nil {
		return nil, commandUserError(err)
	}
	return &struct {
		Body auth.User
	}{Body: *result.User}, nil
}

func (application *apiServer) resetUserPassword(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	UserID  string `path:"userId"`
	Body    struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128"`
		ExpectedRow     int64  `json:"expectedRowVersion" minimum:"1"`
		NewPassword     string `json:"newPassword" minLength:"15" maxLength:"128"`
	}
}) (*struct {
	Body struct {
		User                auth.User `json:"user"`
		RevokedSessionCount int64     `json:"revokedSessionCount"`
	}
}, error) {
	session, err := application.authenticateAdmin(ctx, input.Session, "重置用户密码")
	if err != nil {
		return nil, err
	}
	userID, err := parseLocatorParam(input.UserID)
	if err != nil {
		return nil, err
	}
	// The temporary password is a request secret: the digest records only its
	// presence (SEC-KEY-008), so a same-key retry with the same other fields
	// replays the original result and discards the new value.
	digest := auth.DigestCommand("user.reset_password", map[string]any{
		"userId": userID, "expectedRowVersion": input.Body.ExpectedRow, "newPasswordPresent": true,
	})
	result, _, err := application.auth.ResetUserPassword(ctx, session, auth.ResetPasswordInput{
		ClientCommandID: input.Body.ClientCommandID, Digest: digest,
		UserID: userID, ExpectedRow: input.Body.ExpectedRow, NewPassword: input.Body.NewPassword,
	})
	if err != nil {
		return nil, commandUserError(err)
	}
	output := &struct {
		Body struct {
			User                auth.User `json:"user"`
			RevokedSessionCount int64     `json:"revokedSessionCount"`
		}
	}{}
	output.Body.User = *result.User
	output.Body.RevokedSessionCount = *result.RevokedSessionCount
	return output, nil
}

func (application *apiServer) revokeUserSessions(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	UserID  string `path:"userId"`
	Body    struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128"`
	}
}) (*struct {
	Body struct {
		RevokedSessionCount int64 `json:"revokedSessionCount"`
	}
}, error) {
	session, err := application.authenticateAdmin(ctx, input.Session, "撤销用户 Session")
	if err != nil {
		return nil, err
	}
	userID, err := parseLocatorParam(input.UserID)
	if err != nil {
		return nil, err
	}
	digest := auth.DigestCommand("user.revoke_sessions", map[string]any{"userId": userID})
	result, _, err := application.auth.RevokeUserSessions(ctx, session, auth.RevokeSessionsInput{
		ClientCommandID: input.Body.ClientCommandID, Digest: digest, UserID: userID,
	})
	if err != nil {
		return nil, commandUserError(err)
	}
	return &struct {
		Body struct {
			RevokedSessionCount int64 `json:"revokedSessionCount"`
		}
	}{Body: struct {
		RevokedSessionCount int64 `json:"revokedSessionCount"`
	}{RevokedSessionCount: *result.RevokedSessionCount}}, nil
}

func (application *apiServer) listOwnSessions(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Cursor  string `query:"cursor"`
	Limit   int    `query:"limit"`
}) (*struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items      []auth.SessionView `json:"items"`
		NextCursor string             `json:"nextCursor,omitempty"`
	}
}, error) {
	session, err := application.authenticateFull(ctx, input.Session, "读取登录设备列表")
	if err != nil {
		return nil, err
	}
	afterID, _ := strconv.ParseInt(decodeCursor(input.Cursor), 10, 64)
	sessions, more, err := application.auth.ListUserSessions(ctx, session, afterID, input.Limit)
	if err != nil {
		return nil, problem(http.StatusInternalServerError, "unavailable", "暂时无法读取登录设备列表，请稍后重试。")
	}
	output := &struct {
		CacheControl string `header:"Cache-Control"`
		Body         struct {
			Items      []auth.SessionView `json:"items"`
			NextCursor string             `json:"nextCursor,omitempty"`
		}
	}{CacheControl: "no-store"}
	output.Body.Items = sessions
	if more && len(sessions) > 0 {
		output.Body.NextCursor = encodeCursor(sessions[len(sessions)-1].ID)
	}
	return output, nil
}

func (application *apiServer) revokeOwnSession(ctx context.Context, input *struct {
	Session   string `cookie:"__Host-quoin-session"`
	SessionID string `path:"sessionId"`
	Body      struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128"`
	}
}) (*noContentOutput, error) {
	session, err := application.authenticateFull(ctx, input.Session, "撤销登录设备")
	if err != nil {
		return nil, err
	}
	sessionID, err := parseLocatorParam(input.SessionID)
	if err != nil {
		return nil, err
	}
	digest := auth.DigestCommand("session.revoke_own", map[string]any{"sessionId": sessionID})
	if _, _, err := application.auth.RevokeUserSessions(ctx, session, auth.RevokeSessionsInput{
		ClientCommandID: input.Body.ClientCommandID, Digest: digest, SpecificSession: sessionID, OwnScope: true,
	}); err != nil {
		return nil, commandUserError(err)
	}
	return &noContentOutput{CacheControl: "no-store", Pragma: "no-cache"}, nil
}

func (application *apiServer) listAuditEvents(ctx context.Context, input *struct {
	Session   string `cookie:"__Host-quoin-session"`
	Cursor    string `query:"cursor"`
	Limit     int    `query:"limit"`
	ActorType string `query:"actorType"`
	Action    string `query:"action"`
	From      string `query:"from"`
	To        string `query:"to"`
}) (*struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items      []auth.AuditEventView `json:"items"`
		NextCursor string                `json:"nextCursor,omitempty"`
	}
}, error) {
	if _, err := application.authenticateFull(ctx, input.Session, "读取审计事件"); err != nil {
		return nil, err
	}
	cursor := auth.AuditCursor{}
	if raw := decodeCursor(input.Cursor); raw != "" {
		parts := strings.SplitN(raw, "|", 2)
		if len(parts) == 2 {
			cursor.ID, _ = strconv.ParseInt(parts[0], 10, 64)
			cursor.CreatedAt = parts[1]
		}
	}
	events, more, err := application.auth.ListAuditEvents(ctx, auth.AuditEventFilters{
		ActorType: input.ActorType, Action: input.Action, From: input.From, To: input.To,
	}, cursor, input.Limit)
	if err != nil {
		return nil, problem(http.StatusInternalServerError, "unavailable", "暂时无法读取审计事件，请稍后重试。")
	}
	output := &struct {
		CacheControl string `header:"Cache-Control"`
		Body         struct {
			Items      []auth.AuditEventView `json:"items"`
			NextCursor string                `json:"nextCursor,omitempty"`
		}
	}{CacheControl: "no-store"}
	output.Body.Items = events
	if more && len(events) > 0 {
		last := events[len(events)-1]
		output.Body.NextCursor = encodeCursor(fmt.Sprintf("%s|%s", last.ID, last.CreatedAt))
	}
	return output, nil
}
