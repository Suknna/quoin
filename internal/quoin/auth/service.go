package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrUnauthenticated = errors.New("username or password is incorrect")
	ErrRateLimited     = errors.New("login is temporarily unavailable after repeated failures")
	ErrAlreadySetup    = errors.New("an administrator already exists")
	// ErrPasswordPolicy marks deterministic password-policy rejections
	// (length, blocklist, context match); the HTTP layer maps only these to
	// 422 while infrastructure failures become 500.
	ErrPasswordPolicy = errors.New("password does not satisfy the policy")
)

type User struct {
	ID                     int64   `json:"-"`
	Locator                string  `json:"id"`
	Username               string  `json:"username"`
	DisplayName            string  `json:"displayName"`
	Role                   string  `json:"role"`
	Enabled                bool    `json:"enabled"`
	AuthRevision           int64   `json:"authRevision"`
	RowVersion             int64   `json:"rowVersion"`
	PasswordChangeRequired bool    `json:"passwordChangeRequired"`
	LastLoginAt            *string `json:"lastLoginAt"`
	passwordPHC            string
}

type Session struct {
	ID     int64
	Digest [32]byte
	User   User
}

type LoginResult struct {
	Bearer string
	User   User
}

type Service struct {
	db        *sql.DB
	passwords *Passwords
	limiter   *loginLimiter
	now       func() time.Time
}

func NewService(db *sql.DB) (*Service, error) {
	passwords, err := NewPasswords()
	if err != nil {
		return nil, err
	}
	return &Service{db: db, passwords: passwords, limiter: newLoginLimiter(), now: time.Now}, nil
}

func (service *Service) CreateFirstAdmin(ctx context.Context, username, displayName, password string) (bool, error) {
	username = NormalizeUsername(username)
	displayName = strings.TrimSpace(displayName)
	if username == "" || len(username) > 200 || displayName == "" || len(displayName) > 200 {
		return false, fmt.Errorf("username and display name must contain 1 to 200 characters")
	}
	normalized, err := ValidateNewPassword(password, username, displayName)
	if err != nil {
		return false, err
	}
	phc, err := HashPassword(normalized)
	if err != nil {
		return false, err
	}
	conn, err := service.beginImmediate(ctx)
	if err != nil {
		return false, err
	}
	committed := false
	defer finishImmediate(conn, &committed)
	var count int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return false, err
	}
	if count != 0 {
		if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
			return false, err
		}
		committed = true
		return false, nil
	}
	now := service.timestamp()
	result, err := conn.ExecContext(ctx, `INSERT INTO users(username,display_name,role,enabled,password_phc,password_change_required,password_change_required_at,created_at,updated_at) VALUES(?,?,'admin',1,?,1,?,?,?)`, username, displayName, phc, now, now, now)
	if err != nil {
		return false, fmt.Errorf("create first administrator: %w", err)
	}
	userID, err := result.LastInsertId()
	if err != nil {
		return false, err
	}
	if err := insertAudit(ctx, conn, "system", 0, "admin.bootstrap", "success", "user", userID, now); err != nil {
		return false, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return false, err
	}
	committed = true
	return true, nil
}

func (service *Service) HasUsers(ctx context.Context) (bool, error) {
	var count int
	if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (service *Service) Login(ctx context.Context, username, password, userAgent string) (LoginResult, time.Duration, error) {
	username = NormalizeUsername(username)
	allowed, retryAfter := service.limiter.allow(username)
	if !allowed {
		service.passwords.VerifyDummy(password)
		return LoginResult{}, retryAfter, ErrRateLimited
	}
	normalizedPassword, passwordErr := NormalizePassword(password)
	user, userErr := service.findUserByUsername(ctx, username)
	if userErr != nil && !errors.Is(userErr, sql.ErrNoRows) {
		return LoginResult{}, 0, userErr
	}
	passwordMatches := false
	if passwordErr == nil && userErr == nil {
		passwordMatches = VerifyPassword(normalizedPassword, user.passwordPHC)
	} else {
		service.passwords.VerifyDummy(password)
	}
	valid := passwordMatches && user.Enabled
	if !valid {
		service.limiter.failed(username)
		return LoginResult{}, 0, ErrUnauthenticated
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return LoginResult{}, 0, fmt.Errorf("create session: %w", err)
	}
	digest := sha256.Sum256(raw)
	conn, err := service.beginImmediate(ctx)
	if err != nil {
		return LoginResult{}, 0, err
	}
	committed := false
	defer finishImmediate(conn, &committed)
	current, err := findUserByID(ctx, conn, user.ID)
	if err != nil {
		return LoginResult{}, 0, err
	}
	if !current.Enabled || current.AuthRevision != user.AuthRevision || current.passwordPHC != user.passwordPHC {
		return LoginResult{}, 0, ErrUnauthenticated
	}
	nowTime := service.now().UTC()
	now := nowTime.Format(time.RFC3339Nano)
	idle := nowTime.Add(12 * time.Hour).Format(time.RFC3339Nano)
	absolute := nowTime.Add(7 * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, err := conn.ExecContext(ctx, `INSERT INTO sessions(user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(?,?,?,?,?,?,?,?)`, user.ID, digest[:], user.AuthRevision, clientLabel(userAgent), now, now, idle, absolute); err != nil {
		return LoginResult{}, 0, fmt.Errorf("persist session: %w", err)
	}
	if err := insertAudit(ctx, conn, "user", user.ID, "session.login", "success", "user", user.ID, now); err != nil {
		return LoginResult{}, 0, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return LoginResult{}, 0, err
	}
	committed = true
	service.limiter.succeeded(username)
	current.LastLoginAt = &now
	return LoginResult{Bearer: base64.RawURLEncoding.EncodeToString(raw), User: current}, 0, nil
}

func (service *Service) Authenticate(ctx context.Context, bearer string) (Session, error) {
	raw, err := base64.RawURLEncoding.DecodeString(bearer)
	if err != nil || len(raw) != 32 {
		return Session{}, ErrUnauthenticated
	}
	digest := sha256.Sum256(raw)
	var session Session
	var storedDigest []byte
	var enabled, passwordRequired int
	var revoked sql.NullString
	var idleExpires, absoluteExpires string
	var lastLogin sql.NullString
	err = service.db.QueryRowContext(ctx, `SELECT s.id,s.session_token_digest,s.revoked_at,s.idle_expires_at,s.absolute_expires_at,u.id,u.username,u.display_name,u.role,u.enabled,u.auth_revision,u.row_version,u.password_change_required,u.password_phc,(SELECT MAX(created_at) FROM sessions WHERE user_id=u.id) FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.session_token_digest=? AND s.auth_revision_at_issue=u.auth_revision`, digest[:]).Scan(
		&session.ID, &storedDigest, &revoked, &idleExpires, &absoluteExpires, &session.User.ID, &session.User.Username, &session.User.DisplayName, &session.User.Role, &enabled, &session.User.AuthRevision, &session.User.RowVersion, &passwordRequired, &session.User.passwordPHC, &lastLogin)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Session{}, fmt.Errorf("read session state: %w", err)
	}
	if err != nil || subtle.ConstantTimeCompare(storedDigest, digest[:]) != 1 || revoked.Valid || enabled != 1 {
		return Session{}, ErrUnauthenticated
	}
	now := service.now().UTC()
	idleTime, idleErr := time.Parse(time.RFC3339Nano, idleExpires)
	absoluteTime, absoluteErr := time.Parse(time.RFC3339Nano, absoluteExpires)
	if idleErr != nil || absoluteErr != nil || !now.Before(idleTime) || !now.Before(absoluteTime) {
		return Session{}, ErrUnauthenticated
	}
	copy(session.Digest[:], storedDigest)
	session.User.Locator = fmt.Sprint(session.User.ID)
	session.User.Enabled = true
	session.User.PasswordChangeRequired = passwordRequired == 1
	if lastLogin.Valid {
		session.User.LastLoginAt = &lastLogin.String
	}
	return session, nil
}

func (service *Service) ChangePassword(ctx context.Context, session Session, currentPassword, newPassword string) error {
	currentNormalized, err := NormalizePassword(currentPassword)
	if err != nil {
		return ErrUnauthenticated
	}
	if !VerifyPassword(currentNormalized, session.User.passwordPHC) {
		return ErrUnauthenticated
	}
	newNormalized, err := ValidateNewPassword(newPassword, session.User.Username, session.User.DisplayName)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPasswordPolicy, err)
	}
	newPHC, err := HashPassword(newNormalized)
	if err != nil {
		return err
	}
	conn, err := service.beginImmediate(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer finishImmediate(conn, &committed)
	current, err := findUserByID(ctx, conn, session.User.ID)
	if err != nil {
		return err
	}
	if !current.Enabled || current.AuthRevision != session.User.AuthRevision || current.passwordPHC != session.User.passwordPHC {
		return ErrUnauthenticated
	}
	now := service.timestamp()
	result, err := conn.ExecContext(ctx, `UPDATE users SET password_phc=?,password_change_required=0,password_change_required_at=NULL,auth_revision=auth_revision+1,row_version=row_version+1,updated_at=? WHERE id=? AND auth_revision=?`, newPHC, now, current.ID, current.AuthRevision)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return ErrUnauthenticated
	}
	if _, err := conn.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE user_id=? AND id<>? AND revoked_at IS NULL`, now, current.ID, session.ID); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE sessions SET auth_revision_at_issue=? WHERE id=? AND revoked_at IS NULL`, current.AuthRevision+1, session.ID); err != nil {
		return err
	}
	if err := insertAudit(ctx, conn, "user", current.ID, "user.change_own_password", "success", "user", current.ID, now); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

func (service *Service) Logout(ctx context.Context, session Session) error {
	conn, err := service.beginImmediate(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer finishImmediate(conn, &committed)
	now := service.timestamp()
	if _, err := conn.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, now, session.ID); err != nil {
		return err
	}
	if err := insertAudit(ctx, conn, "user", session.User.ID, "session.logout", "success", "session", session.ID, now); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

func (service *Service) findUserByUsername(ctx context.Context, username string) (User, error) {
	return scanUser(service.db.QueryRowContext(ctx, `SELECT id,username,display_name,role,enabled,auth_revision,row_version,password_change_required,password_phc,(SELECT MAX(created_at) FROM sessions WHERE user_id=users.id) FROM users WHERE username=?`, username))
}

func findUserByID(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64) (User, error) {
	return scanUser(queryer.QueryRowContext(ctx, `SELECT id,username,display_name,role,enabled,auth_revision,row_version,password_change_required,password_phc,(SELECT MAX(created_at) FROM sessions WHERE user_id=users.id) FROM users WHERE id=?`, id))
}

func scanUser(row *sql.Row) (User, error) {
	var user User
	var enabled, required int
	var lastLogin sql.NullString
	err := row.Scan(&user.ID, &user.Username, &user.DisplayName, &user.Role, &enabled, &user.AuthRevision, &user.RowVersion, &required, &user.passwordPHC, &lastLogin)
	if err != nil {
		return User{}, err
	}
	user.Locator = fmt.Sprint(user.ID)
	user.Enabled = enabled == 1
	user.PasswordChangeRequired = required == 1
	if lastLogin.Valid {
		user.LastLoginAt = &lastLogin.String
	}
	return user, nil
}

func (service *Service) beginImmediate(ctx context.Context) (*sql.Conn, error) {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func finishImmediate(conn *sql.Conn, committed *bool) {
	if !*committed {
		_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
	}
	_ = conn.Close()
}

func insertAudit(ctx context.Context, conn *sql.Conn, actorType string, actorID int64, action, outcome, targetType string, targetID int64, timestamp string) error {
	result, err := conn.ExecContext(ctx, `INSERT INTO audit_events(actor_type,actor_id,action,outcome,domain_ref_type,domain_ref_id,created_at) VALUES(?,?,?,?,?,?,?)`, actorType, actorID, action, outcome, targetType, targetID, timestamp)
	if err != nil {
		return fmt.Errorf("persist audit event: %w", err)
	}
	auditID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO audit_event_targets(audit_event_id,target_type,target_id) VALUES(?,?,?)`, auditID, targetType, targetID)
	return err
}

func clientLabel(userAgent string) string {
	browser := "Browser"
	for _, candidate := range []string{"Firefox", "Chrome", "Safari", "Edge"} {
		if strings.Contains(userAgent, candidate) {
			browser = candidate
			break
		}
	}
	platform := "device"
	for _, candidate := range []string{"Windows", "Android", "iPhone", "iPad", "Mac", "Linux"} {
		if strings.Contains(userAgent, candidate) {
			platform = candidate
			break
		}
	}
	return browser + " on " + platform
}

func (service *Service) timestamp() string {
	return service.now().UTC().Format(time.RFC3339Nano)
}
