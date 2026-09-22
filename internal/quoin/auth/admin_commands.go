package auth

// Admin user-management commands and the own-session commands, all executed
// through the execution runner: one runner-owned IMMEDIATE transaction per
// command holding the authorization re-check, the domain write, the durable
// command-ledger row (execution.Run) and the automatic audit event. The
// former hand-written recordOutcome/audit path is gone. Deterministic
// rejections ride as execution.Rejection carrying the legacy ConflictDetail
// JSON so replays rebuild the identical typed errors.

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// CreateUserInput carries the request fields plus the command key.
type CreateUserInput struct {
	ClientCommandID string
	Digest          string
	Username        string
	DisplayName     string
	Role            string
	Password        string
	// Contacts are display-only since the OTP retirement (ADR-0010): the
	// administrator may attach zero to two informational targets (at most
	// one per channel). They never gate the operator lifecycle — only the
	// temporary password and its forced change do.
	Contacts []ContactInput
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

// adminResetOfflineDetail routes the unique built-in administrator to the
// offline recovery channel instead of the online reset. The wording
// deliberately avoids the password-policy marker (密码/password): the
// persisted rejection must rebuild as a validation error, never as a policy
// failure (ConflictDetail.asError).
const adminResetOfflineDetail = "管理员的凭据重置必须通过离线恢复命令（quoin admin recover）完成"

// RevokeSessionsInput covers revokeUserSessions (Admin, SpecificSession=0)
// and revokeOwnSession (own other session).
type RevokeSessionsInput struct {
	ClientCommandID string
	Digest          string
	UserID          int64
	SpecificSession int64
	OwnScope        bool
}

// runUserCommand is the shared execution.Run wrapper for session-originated
// user commands: session-derived metadata (unless the caller already attached
// execution metadata), the acting session in context for authorization, the
// durable command key, and the stable error mapping. A custom objectID
// accessor is required when result.User cannot identify the declared target;
// nil defaults to result.User.ID.
func (service *Service) runUserCommand(
	ctx context.Context,
	session Session,
	op *execution.Operation,
	clientCommandID, digest string,
	business func(tx *execution.Tx) (UserCommandResult, execution.Change, error),
	objectID func(UserCommandResult) int64,
) (UserCommandResult, bool, error) {
	runCtx, err := service.sessionContext(ctx, session)
	if err != nil {
		return UserCommandResult{}, false, err
	}
	if objectID == nil {
		objectID = func(result UserCommandResult) int64 {
			if result.User != nil {
				return result.User.ID
			}
			return 0
		}
	}
	outcome, err := execution.Run(withAdminCall(runCtx, session), service.runner, op, sessionCommand(session, clientCommandID, digest), business, objectID)
	if err != nil {
		return UserCommandResult{}, false, mapAdminRejection(err)
	}
	return outcome.Result, outcome.Replayed, nil
}

// CreateUser creates a local operator account. The deployment keeps exactly
// one built-in administrator, so only operators can be created here. The
// account starts uninitialized with the administrator-issued temporary
// credential: password_change_required stays set until the operator's
// initialization flow replaces it with a formal password.
func (service *Service) CreateUser(ctx context.Context, session Session, input CreateUserInput) (UserCommandResult, bool, error) {
	return service.runUserCommand(ctx, session, service.ops.adminCreateUser, input.ClientCommandID, input.Digest,
		func(tx *execution.Tx) (UserCommandResult, execution.Change, error) {
			username := NormalizeUsername(input.Username)
			displayName := normalizeName(input.DisplayName)
			if username == "" || len(username) > 200 || displayName == "" || len(displayName) > 200 {
				return UserCommandResult{}, execution.Unchanged, conflictRejection(ConflictDetail{Code: "validation_failed", Detail: "用户名和显示名长度必须在 1 到 200 个字符之间"})
			}
			if input.Role != "operator" {
				return UserCommandResult{}, execution.Unchanged, conflictRejection(ConflictDetail{Code: "validation_failed", Detail: "只能创建 operator 账户，管理员唯一且内置"})
			}
			contacts, rejection := normalizeContactSet(input.Contacts)
			if rejection != nil {
				return UserCommandResult{}, execution.Unchanged, rejection
			}
			normalized, policyErr := ValidateNewPassword(input.Password, username, displayName)
			if policyErr != nil {
				return UserCommandResult{}, execution.Unchanged, conflictRejection(ConflictDetail{Code: "validation_failed", Detail: policyErr.Error()})
			}
			phc, hashErr := HashPassword(normalized)
			if hashErr != nil {
				return UserCommandResult{}, execution.Changed, hashErr
			}
			now := service.timestamp()
			insertResult, insertErr := tx.ExecContext(ctx,
				`INSERT INTO users(username,display_name,role,enabled,initialized,password_phc,password_change_required,password_change_required_at,row_version,created_at,updated_at) VALUES(?,?,?,1,0,?,1,?,1,?,?)`,
				username, displayName, input.Role, phc, now, now, now)
			if insertErr != nil {
				if isUniqueViolation(insertErr) {
					return UserCommandResult{}, execution.Unchanged, conflictRejection(ConflictDetail{Code: "active_conflict", Detail: usernameTakenDetail})
				}
				return UserCommandResult{}, execution.Changed, insertErr
			}
			userID, err := insertResult.LastInsertId()
			if err != nil {
				return UserCommandResult{}, execution.Changed, err
			}
			for _, contact := range contacts {
				if _, _, err := upsertContact(ctx, tx, userID, contact, now); err != nil {
					return UserCommandResult{}, execution.Changed, err
				}
			}
			created, err := findUserByID(ctx, tx, userID)
			if err != nil {
				return UserCommandResult{}, execution.Changed, err
			}
			return UserCommandResult{User: &created}, execution.Changed, nil
		}, nil)
}

// normalizeContactSet validates and deduplicates the admin-supplied contact
// set (zero to two display-only entries, distinct channels, bounded targets).
// An empty set is the explicit "no display contacts" state; it retires every
// existing channel under SetUserContacts' full-set semantics.
func normalizeContactSet(contacts []ContactInput) ([]ContactInput, *execution.Rejection) {
	if len(contacts) > 2 {
		return nil, conflictRejection(ConflictDetail{Code: "validation_failed", Detail: "联系方式最多配置两个（每渠道一个）"})
	}
	normalized := make([]ContactInput, 0, len(contacts))
	seenChannels := map[string]bool{}
	for _, contact := range contacts {
		if seenChannels[contact.Channel] {
			return nil, conflictRejection(ConflictDetail{Code: "validation_failed", Detail: "每个渠道最多一个联系方式"})
		}
		seenChannels[contact.Channel] = true
		if err := validateContactInput(contact.Channel, contact.Target); err != nil {
			return nil, conflictRejection(ConflictDetail{Code: "validation_failed", Detail: err.Error()})
		}
		normalized = append(normalized, ContactInput{Channel: contact.Channel, Target: normalizeName(contact.Target)})
	}
	return normalized, nil
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

// UpdateUser applies displayName/enabled with the row-version fence. Identity
// changes were removed with the unique-admin design. Security changes
// (enabled) advance auth_revision exactly once and revoke the user's sessions
// in the same transaction; a semantic no-op is an idempotent success with no
// UPDATE and no second success audit (HTTP-COMMAND-011, DATA-AUTH-005).
func (service *Service) UpdateUser(ctx context.Context, session Session, input UpdateUserInput) (UserCommandResult, bool, error) {
	return service.runUserCommand(ctx, session, service.ops.adminUpdateUser, input.ClientCommandID, input.Digest,
		func(tx *execution.Tx) (UserCommandResult, execution.Change, error) {
			if input.DisplayName == nil && input.Enabled == nil && input.Role == nil {
				return UserCommandResult{}, execution.Unchanged, conflictRejection(ConflictDetail{Code: "validation_failed", ObjectType: "user", ObjectID: strconv.FormatInt(input.UserID, 10), Detail: "至少提供 displayName、enabled 或 role 中的一个字段"})
			}
			displayName := ""
			if input.DisplayName != nil {
				displayName = normalizeName(*input.DisplayName)
				if displayName == "" || len(displayName) > 200 {
					return UserCommandResult{}, execution.Unchanged, conflictRejection(ConflictDetail{Code: "validation_failed", ObjectType: "user", ObjectID: strconv.FormatInt(input.UserID, 10), Detail: "显示名长度必须在 1 到 200 个字符之间"})
				}
			}
			target, err := findUserByID(ctx, tx, input.UserID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return UserCommandResult{}, execution.Unchanged, conflictRejection(ConflictDetail{Code: "not_found", ObjectType: "user", ObjectID: strconv.FormatInt(input.UserID, 10), Detail: "目标对象不存在"})
				}
				return UserCommandResult{}, execution.Changed, err
			}
			if target.RowVersion != input.ExpectedRow {
				return UserCommandResult{}, execution.Unchanged, conflictRejection(rowVersionConflict(target))
			}
			displayChanges := input.DisplayName != nil && displayName != target.DisplayName
			enabledChanges := input.Enabled != nil && *input.Enabled != target.Enabled
			roleChanges := input.Role != nil && *input.Role != target.Role
			if roleChanges {
				// Identity changes were removed with the unique-admin design;
				// the schema trigger trg_users_admin_identity is the backstop.
				return UserCommandResult{}, execution.Unchanged, conflictRejection(ConflictDetail{Code: "validation_failed", ObjectType: "user", ObjectID: target.Locator, Detail: "身份不可变更"})
			}
			if enabledChanges && target.Role == "admin" && !*input.Enabled {
				return UserCommandResult{}, execution.Unchanged, conflictRejection(ConflictDetail{Code: "active_conflict", ObjectType: "user", ObjectID: target.Locator, CurrentRowVersion: target.RowVersion, Detail: lastAdminDetail})
			}
			if !displayChanges && !enabledChanges {
				// Semantic no-op (HTTP-COMMAND-011): freeze as idempotent
				// success with no UPDATE and no row-version bump.
				return UserCommandResult{User: &target}, execution.Unchanged, nil
			}
			newDisplayName := target.DisplayName
			if displayChanges {
				newDisplayName = displayName
			}
			now := service.timestamp()
			var updateResult sql.Result
			if enabledChanges {
				// Security change: advance auth_revision once, revoke every
				// session.
				enabledValue := 0
				if *input.Enabled {
					enabledValue = 1
				}
				if _, err := tx.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, now, target.ID); err != nil {
					return UserCommandResult{}, execution.Changed, err
				}
				updateResult, err = tx.ExecContext(ctx, `UPDATE users SET display_name=?,enabled=?,auth_revision=auth_revision+1,row_version=row_version+1,updated_at=? WHERE id=? AND row_version=?`,
					newDisplayName, enabledValue, now, target.ID, target.RowVersion)
			} else {
				updateResult, err = tx.ExecContext(ctx, `UPDATE users SET display_name=?,row_version=row_version+1,updated_at=? WHERE id=? AND row_version=?`,
					newDisplayName, now, target.ID, target.RowVersion)
			}
			if err != nil {
				return UserCommandResult{}, execution.Changed, err
			}
			if rows, _ := updateResult.RowsAffected(); rows != 1 {
				return UserCommandResult{}, execution.Unchanged, conflictRejection(rowVersionConflict(target))
			}
			updated, err := findUserByID(ctx, tx, target.ID)
			if err != nil {
				return UserCommandResult{}, execution.Changed, err
			}
			return UserCommandResult{User: &updated}, execution.Changed, nil
		}, nil)
}

// rowVersionConflict builds the deterministic stale-row rejection carrying
// the authoritative version for the 409 envelope.
func rowVersionConflict(target User) ConflictDetail {
	return ConflictDetail{Code: "row_version_conflict", ObjectType: "user", ObjectID: target.Locator, CurrentRowVersion: target.RowVersion, Detail: "目标用户已被其他操作修改，请刷新后重试"}
}

// ResetUserPassword replaces an operator's password with an administrator-
// issued temporary one and returns the account to the uninitialized state in
// the SAME transaction, so StartAuthentication re-runs the operator
// initialization flow (temporary credential -> formal password plus the
// verified assigned factor -> back to the login page). No session — full or
// restricted — is ever issued on the way (design §1/§3: initialization never
// auto-login). The command revokes every session, pending authentication
// flow and outstanding challenge of the target atomically; auth_revision
// moves exactly once so old bearers fail their next authentication
// (HTTP-AUTH-003, DATA-AUTH-005). The unique built-in administrator is
// rejected: a reset would return the deployment's only admin to the
// initialization flow, and returning there is the offline `quoin admin
// recover` channel's decision (design §6).
func (service *Service) ResetUserPassword(ctx context.Context, session Session, input ResetPasswordInput) (UserCommandResult, bool, error) {
	return service.runUserCommand(ctx, session, service.ops.adminResetPassword, input.ClientCommandID, input.Digest,
		func(tx *execution.Tx) (UserCommandResult, execution.Change, error) {
			target, err := findUserByID(ctx, tx, input.UserID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return UserCommandResult{}, execution.Unchanged, conflictRejection(ConflictDetail{Code: "not_found", ObjectType: "user", ObjectID: strconv.FormatInt(input.UserID, 10), Detail: "目标对象不存在"})
				}
				return UserCommandResult{}, execution.Changed, err
			}
			if target.Role == "admin" {
				return UserCommandResult{}, execution.Unchanged, conflictRejection(ConflictDetail{Code: "validation_failed", ObjectType: "user", ObjectID: target.Locator, Detail: adminResetOfflineDetail})
			}
			if target.RowVersion != input.ExpectedRow {
				return UserCommandResult{}, execution.Unchanged, conflictRejection(rowVersionConflict(target))
			}
			normalized, policyErr := ValidateNewPassword(input.NewPassword, target.Username, target.DisplayName)
			if policyErr != nil {
				return UserCommandResult{}, execution.Unchanged, conflictRejection(ConflictDetail{Code: "validation_failed", ObjectType: "user", ObjectID: target.Locator, Detail: policyErr.Error()})
			}
			phc, hashErr := HashPassword(normalized)
			if hashErr != nil {
				return UserCommandResult{}, execution.Changed, hashErr
			}
			now := service.timestamp()
			// The credential rotation revokes the target's sessions atomically;
			// with the OTP retirement no flow or challenge rows exist anymore.
			revocation, err := tx.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, now, target.ID)
			if err != nil {
				return UserCommandResult{}, execution.Changed, err
			}
			revokedCount, _ := revocation.RowsAffected()
			// The uninitialized flag is what re-routes the next credential
			// verification into the initialization flow instead of login; the
			// security fields move auth_revision exactly once (schema
			// triggers) while row_version advances exactly once on its own.
			updateResult, err := tx.ExecContext(ctx, `UPDATE users SET password_phc=?,initialized=0,password_change_required=1,password_change_required_at=?,auth_revision=auth_revision+1,row_version=row_version+1,updated_at=? WHERE id=? AND row_version=?`,
				phc, now, now, target.ID, target.RowVersion)
			if err != nil {
				return UserCommandResult{}, execution.Changed, err
			}
			if rows, _ := updateResult.RowsAffected(); rows != 1 {
				return UserCommandResult{}, execution.Unchanged, conflictRejection(rowVersionConflict(target))
			}
			updated, err := findUserByID(ctx, tx, target.ID)
			if err != nil {
				return UserCommandResult{}, execution.Changed, err
			}
			count := revokedCount
			return UserCommandResult{User: &updated, RevokedSessionCount: &count}, execution.Changed, nil
		}, nil)
}

// SetUserContactsInput assigns or replaces the receive targets of one
// operator through the command ledger. The full desired per-channel set is
// supplied; at most one contact per channel exists (user_contacts unique).
type SetUserContactsInput struct {
	ClientCommandID string
	Digest          string
	UserID          int64
	ExpectedRow     int64
	Contacts        []ContactInput
}

// SetUserContacts rotates a user's contact targets with full-set semantics:
// channels absent from the input are retired (enabled=0, unverified, version
// bumped) rather than deleted. Contacts are display-only since the OTP
// retirement (ADR-0010) — they carry the OIDC email or an administrator's
// informational address and no longer bind any verification, so a change no
// longer revokes sessions or reverts initialization.
func (service *Service) SetUserContacts(ctx context.Context, session Session, input SetUserContactsInput) (UserCommandResult, bool, error) {
	return service.runUserCommand(ctx, session, service.ops.adminSetContacts, input.ClientCommandID, input.Digest,
		func(tx *execution.Tx) (UserCommandResult, execution.Change, error) {
			contacts, rejection := normalizeContactSet(input.Contacts)
			if rejection != nil {
				return UserCommandResult{}, execution.Unchanged, rejection
			}
			target, err := findUserByID(ctx, tx, input.UserID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return UserCommandResult{}, execution.Unchanged, conflictRejection(ConflictDetail{Code: "not_found", ObjectType: "user", ObjectID: strconv.FormatInt(input.UserID, 10), Detail: "目标对象不存在"})
				}
				return UserCommandResult{}, execution.Changed, err
			}
			if target.RowVersion != input.ExpectedRow {
				return UserCommandResult{}, execution.Unchanged, conflictRejection(rowVersionConflict(target))
			}
			now := service.timestamp()
			activeChannels := map[string]bool{}
			for _, contact := range contacts {
				activeChannels[contact.Channel] = true
				if _, _, err := upsertContact(ctx, tx, target.ID, contact, now); err != nil {
					return UserCommandResult{}, execution.Changed, err
				}
			}
			// Full-set semantics: retire every channel the input omits.
			if _, err := retireMissingContacts(ctx, tx, target.ID, activeChannels, now); err != nil {
				return UserCommandResult{}, execution.Changed, err
			}
			updated, err := findUserByID(ctx, tx, target.ID)
			if err != nil {
				return UserCommandResult{}, execution.Changed, err
			}
			return UserCommandResult{User: &updated}, execution.Changed, nil
		}, nil)
}

// RevokeUserSessions revokes every active session of the target user
// (Admin), or one specific other session of the caller (revokeOwnSession).
func (service *Service) RevokeUserSessions(ctx context.Context, session Session, input RevokeSessionsInput) (UserCommandResult, bool, error) {
	op := service.ops.adminRevokeSessions
	if input.OwnScope {
		op = service.ops.revokeOwnSession
	}
	return service.runUserCommand(ctx, session, op, input.ClientCommandID, input.Digest,
		func(tx *execution.Tx) (UserCommandResult, execution.Change, error) {
			if input.OwnScope {
				if input.SpecificSession == session.ID {
					return UserCommandResult{}, execution.Unchanged, conflictRejection(ConflictDetail{Code: "active_conflict", ObjectType: "session", ObjectID: strconv.FormatInt(input.SpecificSession, 10), Detail: "不能撤销当前请求使用的 Session，请使用退出登录"})
				}
				var owner int64
				err := tx.QueryRowContext(ctx, `SELECT user_id FROM sessions WHERE id=?`, input.SpecificSession).Scan(&owner)
				if err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						return UserCommandResult{}, execution.Unchanged, conflictRejection(ConflictDetail{Code: "not_found", Detail: "目标对象不存在"})
					}
					return UserCommandResult{}, execution.Changed, err
				}
				if owner != session.User.ID {
					return UserCommandResult{}, execution.Unchanged, conflictRejection(ConflictDetail{Code: "not_found", Detail: "目标对象不存在"})
				}
			} else {
				var exists int
				if err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id=?`, input.UserID).Scan(&exists); err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						return UserCommandResult{}, execution.Unchanged, conflictRejection(ConflictDetail{Code: "not_found", ObjectType: "user", ObjectID: strconv.FormatInt(input.UserID, 10), Detail: "目标对象不存在"})
					}
					return UserCommandResult{}, execution.Changed, err
				}
			}
			now := service.timestamp()
			var revocation sql.Result
			var err error
			if input.OwnScope {
				revocation, err = tx.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, now, input.SpecificSession)
			} else {
				revocation, err = tx.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, now, input.UserID)
			}
			if err != nil {
				return UserCommandResult{}, execution.Changed, err
			}
			revoked, _ := revocation.RowsAffected()
			count := revoked
			return UserCommandResult{RevokedSessionCount: &count}, execution.Changed, nil
		}, func(UserCommandResult) int64 {
			// The aggregate sweep names the target user; the own-session
			// variant names the one revoked sessions row.
			if input.OwnScope {
				return input.SpecificSession
			}
			return input.UserID
		})
}
