package auth

// Admin user-management commands (T05): create/update/reset-password/revoke-
// sessions plus the user-facing session list and own-session revocation.
// Every command runs in one BEGIN IMMEDIATE transaction that re-verifies the
// acting admin (enabled, role, unchanged auth_revision) before committing
// (SEC-SESSION-003/DATA-TX-002), records the durable command-ledger outcome
// and its audit event in that same transaction, and resolves the
// last-effective-admin rule inside the serialized transaction so concurrent
// disable/demote races cannot strip the deployment of its final admin.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	// ErrNotFound maps to 404.
	ErrNotFound = errors.New("target object does not exist")
	// ErrRowVersion maps to 409 row_version_conflict; RowVersionError carries
	// the authoritative value for the conflict envelope.
	ErrRowVersion = errors.New("expected row version does not match")
	// ErrCommandReused maps to 409 command_id_reused.
	ErrCommandReused = errors.New("client command id reused with a different request")
	// ErrLastAdmin maps to 409 active_conflict: the change would disable or
	// demote the last effective administrator (DATA-TX-014).
	ErrLastAdmin = errors.New("operation would remove the last effective administrator")
	// ErrUsernameTaken maps to 409 identity conflict on createUser.
	ErrUsernameTaken = errors.New("username already exists")
	// ErrValidation maps deterministic field rejections to 422.
	ErrValidation = errors.New("request fields do not satisfy the domain rules")
	// ErrActorChanged marks a write whose acting principal lost authorization
	// mid-command (disabled concurrently or session revoked); the HTTP layer
	// maps it to 401 because the caller's session is already gone.
	ErrActorChanged = errors.New("acting session authorization changed")
)

// RowVersionError decorates ErrRowVersion with the authoritative row version
// so the handler can fill the 409 conflict envelope.
type RowVersionError struct {
	Current  int64
	ObjectID int64
}

func (e *RowVersionError) Error() string { return "expected row version does not match" }
func (e *RowVersionError) Unwrap() error { return ErrRowVersion }

// CurrentRowVersion extracts the authoritative version from a conflict.
func CurrentRowVersion(err error) int64 {
	var clash *RowVersionError
	if errors.As(err, &clash) {
		return clash.Current
	}
	return 0
}

// userPayload is the wire/ledger projection of UserSummary; auth.User.ID is
// deliberately untagged for HTTP output, so the ledger stores this explicit
// shape and replays reconstruct it.
type userPayload struct {
	ID                     int64   `json:"id"`
	Username               string  `json:"username"`
	DisplayName            string  `json:"displayName"`
	Role                   string  `json:"role"`
	Enabled                bool    `json:"enabled"`
	Initialized            bool    `json:"initialized"`
	AuthRevision           int64   `json:"authRevision"`
	RowVersion             int64   `json:"rowVersion"`
	PasswordChangeRequired bool    `json:"passwordChangeRequired"`
	LastLoginAt            *string `json:"lastLoginAt"`
}

func userToPayload(user *User) userPayload {
	return userPayload{
		ID: user.ID, Username: user.Username, DisplayName: user.DisplayName, Role: user.Role,
		Enabled: user.Enabled, Initialized: user.Initialized, AuthRevision: user.AuthRevision, RowVersion: user.RowVersion,
		PasswordChangeRequired: user.PasswordChangeRequired, LastLoginAt: user.LastLoginAt,
	}
}

func (payload userPayload) toUser() User {
	return User{
		ID: payload.ID, Locator: strconv.FormatInt(payload.ID, 10), Username: payload.Username,
		DisplayName: payload.DisplayName, Role: payload.Role, Enabled: payload.Enabled,
		Initialized: payload.Initialized, AuthRevision: payload.AuthRevision, RowVersion: payload.RowVersion,
		PasswordChangeRequired: payload.PasswordChangeRequired, LastLoginAt: payload.LastLoginAt,
	}
}

// UserCommandResult is the deterministic non-secret payload shared by
// create/update (user summary) and reset/revoke (user + count) responses and
// persisted for replay. Custom JSON projection: auth.User.ID carries no HTTP
// tag, so persistence stores the explicit userPayload shape and reconstructs
// it on replay.
type UserCommandResult struct {
	User                *User  `json:"-"`
	RevokedSessionCount *int64 `json:"revokedSessionCount,omitempty"`
}

type userCommandResultJSON struct {
	User                *userPayload `json:"user,omitempty"`
	RevokedSessionCount *int64       `json:"revokedSessionCount,omitempty"`
}

func (result UserCommandResult) MarshalJSON() ([]byte, error) {
	var payload *userPayload
	if result.User != nil {
		projection := userToPayload(result.User)
		payload = &projection
	}
	return json.Marshal(userCommandResultJSON{User: payload, RevokedSessionCount: result.RevokedSessionCount})
}

func (result *UserCommandResult) UnmarshalJSON(body []byte) error {
	var projection userCommandResultJSON
	if err := json.Unmarshal(body, &projection); err != nil {
		return err
	}
	if projection.User != nil {
		user := projection.User.toUser()
		result.User = &user
	}
	result.RevokedSessionCount = projection.RevokedSessionCount
	return nil
}

// SessionView is the SessionInfo projection for listOwnSessions.
type SessionView struct {
	ID                string `json:"id"`
	ClientLabel       string `json:"clientLabel"`
	CreatedAt         string `json:"createdAt"`
	LastActiveAt      string `json:"lastActiveAt"`
	IdleExpiresAt     string `json:"idleExpiresAt"`
	AbsoluteExpiresAt string `json:"absoluteExpiresAt"`
	Current           bool   `json:"current"`
}

// ConflictDetail is the non-secret rejection payload persisted with
// rejected_known commands so a replay rebuilds the identical 4xx response.
type ConflictDetail struct {
	Code              string `json:"code"`
	ObjectType        string `json:"objectType,omitempty"`
	ObjectID          string `json:"objectId,omitempty"`
	CurrentRowVersion int64  `json:"currentRowVersion,omitempty"`
	Detail            string `json:"detail,omitempty"`
}

const (
	lastAdminDetail     = "不能禁用或降级最后一个有效的管理员"
	usernameTakenDetail = "用户名已存在"
	passwordMarker      = "密码"
)

func parseLocator(text string) int64 {
	value, _ := strconv.ParseInt(text, 10, 64)
	return value
}

// asError rebuilds the typed package errors from a persisted rejection,
// including the replay-parity mappings for password-policy details and the
// current-session revocation conflict.
func (detail ConflictDetail) asError() error {
	switch detail.Code {
	case "row_version_conflict":
		return &RowVersionError{Current: detail.CurrentRowVersion, ObjectID: parseLocator(detail.ObjectID)}
	case "active_conflict":
		if detail.Detail == lastAdminDetail {
			return ErrLastAdmin
		}
		if detail.Detail == usernameTakenDetail {
			return ErrUsernameTaken
		}
		if strings.Contains(detail.Detail, "当前请求使用的 Session") {
			return fmt.Errorf("%w: %s", ErrValidation, detail.Detail)
		}
		return ErrLastAdmin
	case "command_id_reused":
		return ErrCommandReused
	case "not_found":
		return ErrNotFound
	default:
		if strings.Contains(detail.Detail, passwordMarker) || strings.Contains(strings.ToLower(detail.Detail), "password") {
			return fmt.Errorf("%w: %s", ErrPasswordPolicy, detail.Detail)
		}
		return fmt.Errorf("%w: %s", ErrValidation, detail.Detail)
	}
}

func marshalUserResult(user User) (string, error) {
	body, err := json.Marshal(UserCommandResult{User: &user})
	return string(body), err
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ListUsers returns one keyset page of user summaries ordered by locator.
func (service *Service) ListUsers(ctx context.Context, afterID int64, limit int) ([]User, bool, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := service.read().QueryContext(ctx, `SELECT id,username,display_name,role,enabled,auth_revision,initialized,row_version,password_change_required,password_phc,(SELECT CASE WHEN EXISTS(SELECT 1 FROM identities i WHERE i.user_id=users.id) THEN 'oidc' ELSE 'local' END),(SELECT MAX(created_at) FROM sessions WHERE user_id=users.id) FROM users WHERE id>? ORDER BY id LIMIT ?`, afterID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	users := []User{}
	for rows.Next() {
		user, scanErr := scanUserRow(rows)
		if scanErr != nil {
			return nil, false, scanErr
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := false
	if len(users) > limit {
		users = users[:limit]
		more = true
	}
	return users, more, nil
}

// scanUserRow shares the users projection between list and detail queries.
func scanUserRow(rows *sql.Rows) (User, error) {
	var user User
	var enabled, initialized, required int
	var passwordPHC sql.NullString
	var lastLogin sql.NullString
	if err := rows.Scan(&user.ID, &user.Username, &user.DisplayName, &user.Role, &enabled, &user.AuthRevision, &initialized, &user.RowVersion, &required, &passwordPHC, &user.AuthSource, &lastLogin); err != nil {
		return User{}, err
	}
	user.passwordPHC = passwordPHC.String
	user.Locator = strconv.FormatInt(user.ID, 10)
	user.Enabled = enabled == 1
	user.Initialized = initialized == 1
	user.PasswordChangeRequired = required == 1
	if lastLogin.Valid {
		user.LastLoginAt = &lastLogin.String
	}
	return user, nil
}

// ListUserSessions lists the caller's still-valid sessions, marking the
// session used for the current request.
func (service *Service) ListUserSessions(ctx context.Context, session Session, afterID int64, limit int) ([]SessionView, bool, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	now := service.timestamp()
	rows, err := service.read().QueryContext(ctx, `SELECT id,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at FROM sessions WHERE user_id=? AND revoked_at IS NULL AND absolute_expires_at>? AND id>? ORDER BY id LIMIT ?`, session.User.ID, now, afterID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	views := []SessionView{}
	for rows.Next() {
		var view SessionView
		var id int64
		if err := rows.Scan(&id, &view.ClientLabel, &view.CreatedAt, &view.LastActiveAt, &view.IdleExpiresAt, &view.AbsoluteExpiresAt); err != nil {
			return nil, false, err
		}
		view.ID = strconv.FormatInt(id, 10)
		view.Current = id == session.ID
		views = append(views, view)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := false
	if len(views) > limit {
		views = views[:limit]
		more = true
	}
	return views, more, nil
}

// AuditEventView is the AuditEventSummary projection.
type AuditEventView struct {
	ID              string `json:"id"`
	ActorType       string `json:"actorType"`
	ActorID         string `json:"actorId"`
	Action          string `json:"action"`
	Outcome         string `json:"outcome"`
	ClientCommandID string `json:"clientCommandId,omitempty"`
	DomainRefType   string `json:"domainRefType,omitempty"`
	DomainRefID     string `json:"domainRefId,omitempty"`
	CreatedAt       string `json:"createdAt"`
}

// ListAuditEvents returns one newest-first page with the frozen filters.
func (service *Service) ListAuditEvents(ctx context.Context, filters AuditEventFilters, cursor AuditCursor, limit int) ([]AuditEventView, bool, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT id,actor_type,actor_id,action,outcome,COALESCE(client_command_id,''),COALESCE(domain_ref_type,''),COALESCE(domain_ref_id,0),created_at FROM audit_events`
	conditions := []string{}
	args := []any{}
	if filters.ActorType != "" {
		conditions = append(conditions, "actor_type=?")
		args = append(args, filters.ActorType)
	}
	if filters.Action != "" {
		conditions = append(conditions, "action=?")
		args = append(args, filters.Action)
	}
	if filters.From != "" {
		conditions = append(conditions, "created_at>=?")
		args = append(args, filters.From)
	}
	if filters.To != "" {
		conditions = append(conditions, "created_at<=?")
		args = append(args, filters.To)
	}
	if cursor.ID != 0 || cursor.CreatedAt != "" {
		// Keyset on (created_at, id) descending; the cursor is the last row
		// already delivered.
		conditions = append(conditions, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit+1)
	rows, err := service.read().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	events := []AuditEventView{}
	for rows.Next() {
		var event AuditEventView
		var id, actorID, domainRefID int64
		if err := rows.Scan(&id, &event.ActorType, &actorID, &event.Action, &event.Outcome, &event.ClientCommandID, &event.DomainRefType, &domainRefID, &event.CreatedAt); err != nil {
			return nil, false, err
		}
		event.ID = strconv.FormatInt(id, 10)
		event.ActorID = strconv.FormatInt(actorID, 10)
		if domainRefID != 0 {
			event.DomainRefID = strconv.FormatInt(domainRefID, 10)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := false
	if len(events) > limit {
		events = events[:limit]
		more = true
	}
	return events, more, nil
}

// AuditEventFilters mirrors the frozen query parameters.
type AuditEventFilters struct {
	ActorType string
	Action    string
	From      string
	To        string
}

// AuditCursor is the opaque keyset cursor for audit pagination.
type AuditCursor struct {
	ID        int64
	CreatedAt string
}
