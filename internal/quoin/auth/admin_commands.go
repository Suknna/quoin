package auth

// The four admin write commands: createUser, updateUser, resetUserPassword,
// revokeUserSessions, plus revokeOwnSession. Each is a durable command-ledger
// command: replay by key, one IMMEDIATE transaction for the domain write plus
// ledger plus audit, deterministic rejections persisted in a short
// zero-change transaction (HTTP-COMMAND-003/004/011).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// CreateUserInput carries the request fields plus the command key.
type CreateUserInput struct {
	ClientCommandID string
	Digest          string
	Username        string
	DisplayName     string
	Role            string
	Password        string
}

// UpdateUserInput is updateUser; nil pointers mean "field not provided".
type UpdateUserInput struct {
	ClientCommandID string
	Digest          string
	UserID          int64
	ExpectedRow     int64
	DisplayName     *string
	Enabled         *bool
	Role            *string
}

// ResetPasswordInput is resetUserPassword (Admin). The new temporary password
// is a request-memory secret: it never enters the digest, the ledger or the
// audit trail (SEC-PASSWORD-007/SEC-KEY-008).
type ResetPasswordInput struct {
	ClientCommandID string
	Digest          string
	UserID          int64
	ExpectedRow     int64
	NewPassword     string
}

// RevokeSessionsInput covers revokeUserSessions (Admin, SpecificSession=0)
// and revokeOwnSession (own other session).
type RevokeSessionsInput struct {
	ClientCommandID string
	Digest          string
	UserID          int64
	SpecificSession int64
	OwnScope        bool
}

// CreateUser creates a local account. The frozen schema sets
// password_change_required only for offline bootstrap, admin reset and
// restore flows, so a web-created account starts without the forced-change
// flag (schema comment on users.password_change_required).
func (service *Service) CreateUser(ctx context.Context, session Session, input CreateUserInput) (UserCommandResult, bool, error) {
	command := commandContext{session: session, commandID: input.ClientCommandID, digest: input.Digest, typeName: "user.create", action: "user.create"}
	if result, replay, err := service.replayOrExecute(ctx, command); err != nil || replay {
		return result, replay, err
	}
	username := NormalizeUsername(input.Username)
	displayName := normalizeName(input.DisplayName)
	if username == "" || len(username) > 200 || displayName == "" || len(displayName) > 200 {
		return UserCommandResult{}, false, service.rejectOutsideTransaction(ctx, command,
			ConflictDetail{Code: "validation_failed", Detail: "用户名和显示名长度必须在 1 到 200 个字符之间"}, ErrValidation, 0)
	}
	if input.Role != "admin" && input.Role != "operator" {
		return UserCommandResult{}, false, service.rejectOutsideTransaction(ctx, command,
			ConflictDetail{Code: "validation_failed", Detail: "角色只能是 admin 或 operator"}, ErrValidation, 0)
	}
	normalized, policyErr := ValidateNewPassword(input.Password, username, displayName)
	if policyErr != nil {
		return UserCommandResult{}, false, service.rejectOutsideTransaction(ctx, command,
			ConflictDetail{Code: "validation_failed", Detail: policyErr.Error()}, fmt.Errorf("%w: %v", ErrPasswordPolicy, policyErr), 0)
	}
	phc, hashErr := HashPassword(normalized)
	if hashErr != nil {
		return UserCommandResult{}, false, hashErr
	}
	conn, err := service.beginImmediate(ctx)
	if err != nil {
		return UserCommandResult{}, false, err
	}
	committed := false
	defer finishImmediate(conn, &committed)
	if err := verifyActorAdmin(ctx, conn, session); err != nil {
		return UserCommandResult{}, false, fmt.Errorf("verify acting admin: %w", err)
	}
	now := service.timestamp()
	insertResult, insertErr := conn.ExecContext(ctx,
		`INSERT INTO users(username,display_name,role,enabled,password_phc,password_change_required,password_change_required_at,row_version,created_at,updated_at) VALUES(?,?,?,1,?,0,NULL,1,?,?)`,
		username, displayName, input.Role, phc, now, now)
	if insertErr != nil {
		if isUniqueViolation(insertErr) {
			// The domain INSERT failed; this open transaction is still empty,
			// so it becomes the short rejection transaction.
			if err := service.recordOutcome(ctx, conn, command, OutcomeRejectedKnown, "user", 0,
				`{"code":"active_conflict","detail":"`+usernameTakenDetail+`"}`, "rejected"); err != nil {
				return UserCommandResult{}, false, err
			}
			if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
				return UserCommandResult{}, false, err
			}
			committed = true
			return UserCommandResult{}, false, ErrUsernameTaken
		}
		return UserCommandResult{}, false, insertErr
	}
	userID, err := insertResult.LastInsertId()
	if err != nil {
		return UserCommandResult{}, false, err
	}
	created, err := findUserByID(ctx, conn, userID)
	if err != nil {
		return UserCommandResult{}, false, err
	}
	payload, err := marshalUserResult(created)
	if err != nil {
		return UserCommandResult{}, false, err
	}
	if err := service.recordOutcome(ctx, conn, command, OutcomeCommitted, "user", userID, payload, "success"); err != nil {
		return UserCommandResult{}, false, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return UserCommandResult{}, false, err
	}
	committed = true
	return UserCommandResult{User: &created}, false, nil
}

func normalizeName(value string) string {
	start, end := 0, len(value)
	for start < end && isSpaceByte(value[start]) {
		start++
	}
	for end > start && isSpaceByte(value[end-1]) {
		end--
	}
	return value[start:end]
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// UpdateUser applies displayName/enabled/role with the row-version fence.
// Security changes (enabled/role) advance auth_revision exactly once and
// revoke the user's sessions in the same transaction; a semantic no-op is an
// idempotent success with no UPDATE and no second success audit
// (HTTP-COMMAND-011, DATA-AUTH-005).
func (service *Service) UpdateUser(ctx context.Context, session Session, input UpdateUserInput) (UserCommandResult, bool, error) {
	command := commandContext{session: session, commandID: input.ClientCommandID, digest: input.Digest, typeName: "user.update", action: "user.update"}
	if result, replay, err := service.replayOrExecute(ctx, command); err != nil || replay {
		return result, replay, err
	}
	if input.DisplayName == nil && input.Enabled == nil && input.Role == nil {
		return UserCommandResult{}, false, service.rejectOutsideTransaction(ctx, command,
			ConflictDetail{Code: "validation_failed", Detail: "至少提供 displayName、enabled 或 role 中的一个字段"}, ErrValidation, input.UserID)
	}
	if input.Role != nil && *input.Role != "admin" && *input.Role != "operator" {
		return UserCommandResult{}, false, service.rejectOutsideTransaction(ctx, command,
			ConflictDetail{Code: "validation_failed", Detail: "角色只能是 admin 或 operator"}, ErrValidation, input.UserID)
	}
	if input.DisplayName != nil {
		trimmed := normalizeName(*input.DisplayName)
		if trimmed == "" || len(trimmed) > 200 {
			return UserCommandResult{}, false, service.rejectOutsideTransaction(ctx, command,
				ConflictDetail{Code: "validation_failed", Detail: "显示名长度必须在 1 到 200 个字符之间"}, ErrValidation, input.UserID)
		}
		input.DisplayName = &trimmed
	}
	conn, err := service.beginImmediate(ctx)
	if err != nil {
		return UserCommandResult{}, false, err
	}
	committed := false
	defer finishImmediate(conn, &committed)
	if err := verifyActorAdmin(ctx, conn, session); err != nil {
		return UserCommandResult{}, false, fmt.Errorf("verify acting admin: %w", err)
	}
	target, err := findUserByID(ctx, conn, input.UserID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UserCommandResult{}, false, service.rejectMissing(ctx, conn, command, &committed)
		}
		return UserCommandResult{}, false, err
	}
	if target.RowVersion != input.ExpectedRow {
		return UserCommandResult{}, false, service.rejectRowVersion(ctx, conn, command, target, &committed)
	}
	displayChanges := input.DisplayName != nil && *input.DisplayName != target.DisplayName
	enabledChanges := input.Enabled != nil && *input.Enabled != target.Enabled
	roleChanges := input.Role != nil && *input.Role != target.Role
	securityChange := enabledChanges || roleChanges
	// DATA-TX-014: inside the serialized transaction the last-effective-admin
	// guard reads the would-be post-change state.
	if securityChange && target.Role == "admin" && target.Enabled {
		willStillBeAdmin := (input.Enabled == nil || *input.Enabled) && (input.Role == nil || *input.Role == "admin")
		if !willStillBeAdmin {
			var otherAdmins int
			if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role='admin' AND enabled=1 AND id<>?`, target.ID).Scan(&otherAdmins); err != nil {
				return UserCommandResult{}, false, err
			}
			if otherAdmins == 0 {
				return UserCommandResult{}, false, service.rejectLastAdmin(ctx, conn, command, target, &committed)
			}
		}
	}
	now := service.timestamp()
	if !displayChanges && !enabledChanges && !roleChanges {
		// Semantic no-op (HTTP-COMMAND-011): freeze as idempotent success
		// with no UPDATE, no row-version bump and no second success audit;
		// the ledger still records the outcome.
		payload, payloadErr := marshalUserResult(target)
		if payloadErr != nil {
			return UserCommandResult{}, false, payloadErr
		}
		if err := RecordCommand(ctx, conn, session.User.ID, command.commandID, command.typeName, command.digest, OutcomeCommitted, "user", target.ID, payload); err != nil {
			return UserCommandResult{}, false, err
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return UserCommandResult{}, false, err
		}
		committed = true
		return UserCommandResult{User: &target}, false, nil
	}
	newDisplayName, newEnabled, newRole := target.DisplayName, target.Enabled, target.Role
	if displayChanges {
		newDisplayName = *input.DisplayName
	}
	if enabledChanges {
		newEnabled = *input.Enabled
	}
	if roleChanges {
		newRole = *input.Role
	}
	enabledValue, roleValue := 0, newRole
	if newEnabled {
		enabledValue = 1
	}
	var updateResult sql.Result
	if securityChange {
		// Security change: advance auth_revision once, revoke every session.
		if _, err := conn.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, now, target.ID); err != nil {
			return UserCommandResult{}, false, err
		}
		updateResult, err = conn.ExecContext(ctx, `UPDATE users SET display_name=?,enabled=?,role=?,auth_revision=auth_revision+1,row_version=row_version+1,updated_at=? WHERE id=? AND row_version=?`,
			newDisplayName, enabledValue, roleValue, now, target.ID, target.RowVersion)
	} else {
		updateResult, err = conn.ExecContext(ctx, `UPDATE users SET display_name=?,row_version=row_version+1,updated_at=? WHERE id=? AND row_version=?`,
			newDisplayName, now, target.ID, target.RowVersion)
	}
	if err != nil {
		return UserCommandResult{}, false, err
	}
	if rows, _ := updateResult.RowsAffected(); rows != 1 {
		return UserCommandResult{}, false, service.rejectRowVersion(ctx, conn, command, target, &committed)
	}
	updated, err := findUserByID(ctx, conn, target.ID)
	if err != nil {
		return UserCommandResult{}, false, err
	}
	payload, err := marshalUserResult(updated)
	if err != nil {
		return UserCommandResult{}, false, err
	}
	if err := service.recordOutcome(ctx, conn, command, OutcomeCommitted, "user", updated.ID, payload, "success"); err != nil {
		return UserCommandResult{}, false, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return UserCommandResult{}, false, err
	}
	committed = true
	return UserCommandResult{User: &updated}, false, nil
}

// ResetUserPassword replaces the password with a temporary one, forces the
// change flag and revokes every session of the target user in one
// transaction; the old bearer fails its next authentication because
// auth_revision moved (HTTP-AUTH-003).
func (service *Service) ResetUserPassword(ctx context.Context, session Session, input ResetPasswordInput) (UserCommandResult, bool, error) {
	command := commandContext{session: session, commandID: input.ClientCommandID, digest: input.Digest, typeName: "user.reset_password", action: "user.reset_password"}
	if result, replay, err := service.replayOrExecute(ctx, command); err != nil || replay {
		return result, replay, err
	}
	conn, err := service.beginImmediate(ctx)
	if err != nil {
		return UserCommandResult{}, false, err
	}
	committed := false
	defer finishImmediate(conn, &committed)
	if err := verifyActorAdmin(ctx, conn, session); err != nil {
		return UserCommandResult{}, false, fmt.Errorf("verify acting admin: %w", err)
	}
	target, err := findUserByID(ctx, conn, input.UserID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UserCommandResult{}, false, service.rejectMissing(ctx, conn, command, &committed)
		}
		return UserCommandResult{}, false, err
	}
	if target.RowVersion != input.ExpectedRow {
		return UserCommandResult{}, false, service.rejectRowVersion(ctx, conn, command, target, &committed)
	}
	normalized, policyErr := ValidateNewPassword(input.NewPassword, target.Username, target.DisplayName)
	if policyErr != nil {
		return UserCommandResult{}, false, service.rejectInTransaction(ctx, conn, command,
			ConflictDetail{Code: "validation_failed", Detail: policyErr.Error()}, fmt.Errorf("%w: %v", ErrPasswordPolicy, policyErr), target.ID, &committed)
	}
	phc, hashErr := HashPassword(normalized)
	if hashErr != nil {
		return UserCommandResult{}, false, hashErr
	}
	now := service.timestamp()
	revocation, err := conn.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, now, target.ID)
	if err != nil {
		return UserCommandResult{}, false, err
	}
	revokedCount, _ := revocation.RowsAffected()
	updateResult, err := conn.ExecContext(ctx, `UPDATE users SET password_phc=?,password_change_required=1,password_change_required_at=?,auth_revision=auth_revision+1,row_version=row_version+1,updated_at=? WHERE id=? AND row_version=?`,
		phc, now, now, target.ID, target.RowVersion)
	if err != nil {
		return UserCommandResult{}, false, err
	}
	if rows, _ := updateResult.RowsAffected(); rows != 1 {
		return UserCommandResult{}, false, service.rejectRowVersion(ctx, conn, command, target, &committed)
	}
	updated, err := findUserByID(ctx, conn, target.ID)
	if err != nil {
		return UserCommandResult{}, false, err
	}
	count := revokedCount
	payload, err := json.Marshal(UserCommandResult{User: &updated, RevokedSessionCount: &count})
	if err != nil {
		return UserCommandResult{}, false, err
	}
	if err := service.recordOutcome(ctx, conn, command, OutcomeCommitted, "user", updated.ID, string(payload), "success"); err != nil {
		return UserCommandResult{}, false, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return UserCommandResult{}, false, err
	}
	committed = true
	return UserCommandResult{User: &updated, RevokedSessionCount: &count}, false, nil
}

// RevokeUserSessions revokes every active session of the target user
// (Admin), or one specific other session of the caller (revokeOwnSession).
func (service *Service) RevokeUserSessions(ctx context.Context, session Session, input RevokeSessionsInput) (UserCommandResult, bool, error) {
	command := commandContext{session: session, commandID: input.ClientCommandID, digest: input.Digest, typeName: "user.revoke_sessions", action: "user.revoke_sessions"}
	if input.OwnScope {
		command.typeName, command.action = "session.revoke_own", "session.revoke_own"
	}
	if result, replay, err := service.replayOrExecute(ctx, command); err != nil || replay {
		return result, replay, err
	}
	conn, err := service.beginImmediate(ctx)
	if err != nil {
		return UserCommandResult{}, false, err
	}
	committed := false
	defer finishImmediate(conn, &committed)
	if input.OwnScope {
		if input.SpecificSession == session.ID {
			detail := ConflictDetail{Code: "active_conflict", ObjectType: "session", ObjectID: strconv.FormatInt(input.SpecificSession, 10), Detail: "不能撤销当前请求使用的 Session，请使用退出登录"}
			return UserCommandResult{}, false, service.rejectInTransaction(ctx, conn, command, detail, fmt.Errorf("%w: 不能撤销当前请求使用的 Session", ErrValidation), 0, &committed)
		}
		var owner int64
		err := conn.QueryRowContext(ctx, `SELECT user_id FROM sessions WHERE id=?`, input.SpecificSession).Scan(&owner)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return UserCommandResult{}, false, service.rejectMissing(ctx, conn, command, &committed)
			}
			return UserCommandResult{}, false, err
		}
		if owner != session.User.ID {
			return UserCommandResult{}, false, service.rejectMissing(ctx, conn, command, &committed)
		}
	} else {
		if err := verifyActorAdmin(ctx, conn, session); err != nil {
			return UserCommandResult{}, false, fmt.Errorf("verify acting admin: %w", err)
		}
		var exists int
		if err := conn.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id=?`, input.UserID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return UserCommandResult{}, false, service.rejectMissing(ctx, conn, command, &committed)
			}
			return UserCommandResult{}, false, err
		}
	}
	now := service.timestamp()
	var revocation sql.Result
	if input.OwnScope {
		revocation, err = conn.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, now, input.SpecificSession)
	} else {
		revocation, err = conn.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, now, input.UserID)
	}
	if err != nil {
		return UserCommandResult{}, false, err
	}
	revoked, _ := revocation.RowsAffected()
	count := revoked
	payload, err := json.Marshal(UserCommandResult{RevokedSessionCount: &count})
	if err != nil {
		return UserCommandResult{}, false, err
	}
	targetID := input.UserID
	if input.OwnScope {
		targetID = input.SpecificSession
	}
	if err := service.recordOutcome(ctx, conn, command, OutcomeCommitted, "session", targetID, string(payload), "success"); err != nil {
		return UserCommandResult{}, false, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return UserCommandResult{}, false, err
	}
	committed = true
	return UserCommandResult{RevokedSessionCount: &count}, false, nil
}
