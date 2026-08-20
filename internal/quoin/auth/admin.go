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
	AuthRevision           int64   `json:"authRevision"`
	RowVersion             int64   `json:"rowVersion"`
	PasswordChangeRequired bool    `json:"passwordChangeRequired"`
	LastLoginAt            *string `json:"lastLoginAt"`
}

func userToPayload(user *User) userPayload {
	return userPayload{
		ID: user.ID, Username: user.Username, DisplayName: user.DisplayName, Role: user.Role,
		Enabled: user.Enabled, AuthRevision: user.AuthRevision, RowVersion: user.RowVersion,
		PasswordChangeRequired: user.PasswordChangeRequired, LastLoginAt: user.LastLoginAt,
	}
}

func (payload userPayload) toUser() User {
	return User{
		ID: payload.ID, Locator: strconv.FormatInt(payload.ID, 10), Username: payload.Username,
		DisplayName: payload.DisplayName, Role: payload.Role, Enabled: payload.Enabled,
		AuthRevision: payload.AuthRevision, RowVersion: payload.RowVersion,
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

// commandContext bundles the command key identity every write shares.
type commandContext struct {
	session   Session
	commandID string
	digest    string
	typeName  string
	action    string
}

// replayOrExecute centralizes the command-key lookup: a stored record with
// the same digest replays (committed results re-materialize, deterministic
// rejections rebuild the same 4xx), a different digest conflicts, and a new
// command falls through to execute (HTTP-COMMAND-003/007).
func (service *Service) replayOrExecute(ctx context.Context, command commandContext) (UserCommandResult, bool, error) {
	stored, found, err := LookupCommand(ctx, service.db, command.session.User.ID, command.commandID)
	if err != nil || !found {
		return UserCommandResult{}, false, err
	}
	if stored.RequestDigest != command.digest {
		return UserCommandResult{}, false, ErrCommandReused
	}
	if stored.Outcome == OutcomeRejectedKnown {
		var detail ConflictDetail
		_ = json.Unmarshal([]byte(stored.ResultPayload), &detail)
		return UserCommandResult{}, true, detail.asError()
	}
	var result UserCommandResult
	if err := json.Unmarshal([]byte(stored.ResultPayload), &result); err != nil {
		return UserCommandResult{}, false, fmt.Errorf("replay %s: %w", command.typeName, err)
	}
	if result.User != nil {
		result.User.Locator = strconv.FormatInt(result.User.ID, 10)
	}
	return result, true, nil
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
		return ErrLastAdmin
	case "command_id_reused":
		return ErrCommandReused
	case "not_found":
		return ErrNotFound
	default:
		if strings.Contains(detail.Detail, passwordMarker) {
			return fmt.Errorf("%w: %s", ErrPasswordPolicy, detail.Detail)
		}
		return fmt.Errorf("%w: %s", ErrValidation, detail.Detail)
	}
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

// recordOutcome writes the ledger row plus audit event on an OPEN
// transaction (either the domain-write transaction for a commit, or a short
// rejection transaction — DATA-AUDIT-004/005).
func (service *Service) recordOutcome(ctx context.Context, conn *sql.Conn, command commandContext, outcome, objectType string, objectID int64, payload string, auditOutcome string) error {
	if err := RecordCommand(ctx, conn, command.session.User.ID, command.commandID, command.typeName, command.digest, outcome, objectType, objectID, payload); err != nil {
		return err
	}
	return insertAudit(ctx, conn, "user", command.session.User.ID, command.action, auditOutcome, objectType, objectID, service.timestamp())
}

// rejectOutsideTransaction persists a deterministic rejection that was
// detected before any transaction opened (field validation): ledger + audit
// in one short transaction, zero domain change.
func (service *Service) rejectOutsideTransaction(ctx context.Context, command commandContext, detail ConflictDetail, wrapped error, targetID int64) error {
	payload, err := json.Marshal(detail)
	if err != nil {
		payload = []byte(`{}`)
	}
	conn, err := service.beginImmediate(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer finishImmediate(conn, &committed)
	if err := service.recordOutcome(ctx, conn, command, OutcomeRejectedKnown, "user", targetID, string(payload), "rejected"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return wrapped
}

// rejectInTransaction persists a deterministic rejection discovered inside an
// open (empty) domain transaction: ledger + audit, commit, return the
// business error (DATA-AUDIT-004). committed is flipped so the deferred
// finishImmediate does not try to roll back the committed rejection.
func (service *Service) rejectInTransaction(ctx context.Context, conn *sql.Conn, command commandContext, detail ConflictDetail, wrapped error, targetID int64, committed *bool) error {
	payload, err := json.Marshal(detail)
	if err != nil {
		payload = []byte(`{}`)
	}
	if err := service.recordOutcome(ctx, conn, command, OutcomeRejectedKnown, detail.ObjectType, targetID, string(payload), "rejected"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	*committed = true
	return wrapped
}

func (service *Service) rejectMissing(ctx context.Context, conn *sql.Conn, command commandContext, committed *bool) error {
	detail := ConflictDetail{Code: "not_found", Detail: "目标对象不存在"}
	return service.rejectInTransaction(ctx, conn, command, detail, ErrNotFound, 0, committed)
}

func (service *Service) rejectRowVersion(ctx context.Context, conn *sql.Conn, command commandContext, target User, committed *bool) error {
	detail := ConflictDetail{Code: "row_version_conflict", ObjectType: "user", ObjectID: target.Locator, CurrentRowVersion: target.RowVersion, Detail: "目标用户已被其他操作修改，请刷新后重试"}
	return service.rejectInTransaction(ctx, conn, command, detail, &RowVersionError{Current: target.RowVersion, ObjectID: target.ID}, target.ID, committed)
}

func (service *Service) rejectLastAdmin(ctx context.Context, conn *sql.Conn, command commandContext, target User, committed *bool) error {
	detail := ConflictDetail{Code: "active_conflict", ObjectType: "user", ObjectID: target.Locator, CurrentRowVersion: target.RowVersion, Detail: lastAdminDetail}
	return service.rejectInTransaction(ctx, conn, command, detail, ErrLastAdmin, target.ID, committed)
}

// ErrActorChanged marks a write whose acting principal lost authorization
// mid-command (disabled/demoted concurrently); the HTTP layer maps it to 401
// because the caller's session is already revoked.
var ErrActorChanged = errors.New("acting session authorization changed")

// verifyActorAdmin re-reads the acting user inside the open transaction and
// confirms the session identity is still an enabled administrator at the
// same auth revision (DATA-TX-002).
func verifyActorAdmin(ctx context.Context, conn *sql.Conn, session Session) error {
	actor, err := findUserByID(ctx, conn, session.User.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrActorChanged
		}
		return err
	}
	if !actor.Enabled || actor.Role != "admin" || actor.AuthRevision != session.User.AuthRevision {
		return ErrActorChanged
	}
	return nil
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
	rows, err := service.db.QueryContext(ctx, `SELECT id,username,display_name,role,enabled,auth_revision,row_version,password_change_required,password_phc,(SELECT MAX(created_at) FROM sessions WHERE user_id=users.id) FROM users WHERE id>? ORDER BY id LIMIT ?`, afterID, limit+1)
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
	var enabled, required int
	var lastLogin sql.NullString
	if err := rows.Scan(&user.ID, &user.Username, &user.DisplayName, &user.Role, &enabled, &user.AuthRevision, &user.RowVersion, &required, &user.passwordPHC, &lastLogin); err != nil {
		return User{}, err
	}
	user.Locator = strconv.FormatInt(user.ID, 10)
	user.Enabled = enabled == 1
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
	rows, err := service.db.QueryContext(ctx, `SELECT id,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at FROM sessions WHERE user_id=? AND revoked_at IS NULL AND absolute_expires_at>? AND id>? ORDER BY id LIMIT ?`, session.User.ID, now, afterID, limit+1)
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
	rows, err := service.db.QueryContext(ctx, query, args...)
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
