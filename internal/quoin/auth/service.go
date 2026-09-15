package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

var (
	ErrUnauthenticated = errors.New("username or password is incorrect")
	ErrRateLimited     = errors.New("login is temporarily unavailable after repeated failures")
	ErrAlreadySetup    = errors.New("an administrator already exists")
	// ErrPasswordPolicy marks deterministic password-policy rejections
	// (length, blocklist, context match); the HTTP layer maps only these to
	// 422 while infrastructure failures become 500.
	ErrPasswordPolicy = errors.New("password does not satisfy the policy")
	// ErrInitializeRejected marks rejected initialization starts (unknown
	// user, wrong credential class or wrong role for the requested flow).
	ErrInitializeRejected = errors.New("initialization credentials are invalid")
	// ErrFlowDeliveryNotConfigured marks a challenge request before
	// ConfigureAuth installed the OTP key and sender (fail closed, 503).
	ErrFlowDeliveryNotConfigured = errors.New("message delivery is not configured")
)

type User struct {
	ID                     int64   `json:"-"`
	Locator                string  `json:"id"`
	Username               string  `json:"username"`
	DisplayName            string  `json:"displayName"`
	Role                   string  `json:"role"`
	Enabled                bool    `json:"enabled"`
	Initialized            bool    `json:"initialized"`
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
	// runner executes every flow step with automatic, same-transaction audit
	// (ADR-0006); ops holds this package's declared write operations.
	runner *execution.Runner
	ops    authOperations
	// authMu guards the configured OTP key, the delivery sender and the
	// read-only pool installed at startup.
	authMu sync.RWMutex
	otpKey []byte
	sender Sender
	// reader is the optional read-only source for pure reads; nil falls back
	// to the writer database.
	reader audit.Reader
	// otpFailures is the per-user cross-flow verification failure limiter
	// (in-memory; the durable failed-flow gate bounds restart resets).
	otpFailures *loginLimiter
	// factors holds the registered per-channel second-factor sources;
	// nil entries fall back to the built-in numeric generator.
	factors map[string]FactorSource
}

func NewService(db *sql.DB) (*Service, error) {
	passwords, err := NewPasswords()
	if err != nil {
		return nil, err
	}
	ops, registry := newAuthOperations()
	return &Service{
		db: db, passwords: passwords, limiter: newLoginLimiter(), otpFailures: newLoginLimiter(), now: time.Now,
		runner: execution.NewRunner(db, registry, nil), ops: ops,
	}, nil
}

// CreateFirstAdmin is the legacy offline bootstrap seed kept for the not-yet-
// retired CLI path and existing external tests. It seeds an initialized=0
// administrator who can never reach a session without the full initialization
// flow and second factor; new deployments use EnsureBootstrapAdmin.
// The seed runs through the execution runner (audited, no manual audit).
func (service *Service) CreateFirstAdmin(ctx context.Context, username, displayName, password string) (bool, error) {
	if _, exists := execution.FromContext(ctx); exists {
		return false, errors.New("legacy bootstrap requires a context without execution metadata")
	}
	correlation, err := execution.NewCorrelationID()
	if err != nil {
		return false, err
	}
	meta := execution.Metadata{
		CorrelationID: correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Source:        execution.Source{Kind: execution.SourceInternal},
	}
	runCtx, err := execution.WithMetadata(ctx, meta)
	if err != nil {
		return false, err
	}
	normalized := NormalizeUsername(username)
	displayTrimmed := strings.TrimSpace(displayName)
	if normalized == "" || len(normalized) > 200 || displayTrimmed == "" || len(displayTrimmed) > 200 {
		return false, fmt.Errorf("username and display name must contain 1 to 200 characters")
	}
	validated, policyErr := ValidateNewPassword(password, normalized, displayTrimmed)
	if policyErr != nil {
		return false, policyErr
	}
	phc, err := HashPassword(validated)
	if err != nil {
		return false, err
	}
	type seed struct {
		Created bool
	}
	result, err := execution.Execute(runCtx, service.runner, service.ops.legacyBootstrapSeed, func(tx *execution.Tx) (seed, error) {
		var count int
		if err := tx.QueryRowContext(runCtx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
			return seed{}, err
		}
		if count != 0 {
			return seed{Created: false}, nil
		}
		now := service.timestamp()
		insert, err := tx.ExecContext(runCtx, `INSERT INTO users(username,display_name,role,enabled,initialized,password_phc,password_change_required,password_change_required_at,row_version,created_at,updated_at) VALUES(?,?,'admin',1,0,?,1,?,1,?,?)`, normalized, displayTrimmed, phc, now, now, now)
		if err != nil {
			return seed{}, fmt.Errorf("create first administrator: %w", err)
		}
		if _, err := insert.LastInsertId(); err != nil {
			return seed{}, err
		}
		return seed{Created: true}, nil
	}, func(result seed) int64 { return 0 })
	if err != nil {
		return false, err
	}
	return result.Created, nil
}

func (service *Service) HasUsers(ctx context.Context) (bool, error) {
	var count int
	if err := service.read().QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (service *Service) Authenticate(ctx context.Context, bearer string) (Session, error) {
	raw, err := base64.RawURLEncoding.DecodeString(bearer)
	if err != nil || len(raw) != 32 {
		return Session{}, ErrUnauthenticated
	}
	digest := sha256.Sum256(raw)
	var session Session
	var storedDigest []byte
	var enabled, initialized, passwordRequired int
	var revoked sql.NullString
	var idleExpires, absoluteExpires string
	var lastLogin sql.NullString
	err = service.read().QueryRowContext(ctx, `SELECT s.id,s.session_token_digest,s.revoked_at,s.idle_expires_at,s.absolute_expires_at,u.id,u.username,u.display_name,u.role,u.enabled,u.auth_revision,u.initialized,u.row_version,u.password_change_required,u.password_phc,(SELECT MAX(created_at) FROM sessions WHERE user_id=u.id) FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.session_token_digest=? AND s.auth_revision_at_issue=u.auth_revision`, digest[:]).Scan(
		&session.ID, &storedDigest, &revoked, &idleExpires, &absoluteExpires, &session.User.ID, &session.User.Username, &session.User.DisplayName, &session.User.Role, &enabled, &session.User.AuthRevision, &initialized, &session.User.RowVersion, &passwordRequired, &session.User.passwordPHC, &lastLogin)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		if !service.readPoolWired() {
			return Session{}, ErrReaderNotWired
		}
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
	session.User.Initialized = initialized == 1
	session.User.PasswordChangeRequired = passwordRequired == 1
	if lastLogin.Valid {
		session.User.LastLoginAt = &lastLogin.String
	}
	return session, nil
}

// ChangePassword performs the self-service password change through the
// execution runner: the current password is verified inside the authorized
// transaction, every other session is revoked, and the acting session is
// re-bound to the advanced revision atomically (DATA-AUTH-001/005).
func (service *Service) ChangePassword(ctx context.Context, session Session, currentPassword, newPassword string) error {
	runCtx, err := service.sessionContext(ctx, session)
	if err != nil {
		return err
	}
	_, err = execution.Execute(withAdminCall(runCtx, session), service.runner, service.ops.changeOwnPassword, func(tx *execution.Tx) (bool, error) {
		current, err := findUserByID(runCtx, tx, session.User.ID)
		if err != nil {
			return false, err
		}
		currentNormalized, normalizeErr := NormalizePassword(currentPassword)
		if normalizeErr != nil || !VerifyPassword(currentNormalized, current.passwordPHC) {
			return false, ErrUnauthenticated
		}
		newNormalized, policyErr := ValidateNewPassword(newPassword, current.Username, current.DisplayName)
		if policyErr != nil {
			return false, conflictRejection(ConflictDetail{Code: "validation_failed", ObjectType: "user", ObjectID: current.Locator, Detail: policyErr.Error()})
		}
		newPHC, err := HashPassword(newNormalized)
		if err != nil {
			return false, err
		}
		now := service.timestamp()
		update, err := tx.ExecContext(runCtx, `UPDATE users SET password_phc=?,password_change_required=0,password_change_required_at=NULL,auth_revision=auth_revision+1,row_version=row_version+1,updated_at=? WHERE id=? AND auth_revision=?`, newPHC, now, current.ID, current.AuthRevision)
		if err != nil {
			return false, err
		}
		if rows, _ := update.RowsAffected(); rows != 1 {
			return false, ErrUnauthenticated
		}
		if _, err := tx.ExecContext(runCtx, `UPDATE sessions SET revoked_at=? WHERE user_id=? AND id<>? AND revoked_at IS NULL`, now, current.ID, session.ID); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(runCtx, `UPDATE sessions SET auth_revision_at_issue=? WHERE id=? AND revoked_at IS NULL`, current.AuthRevision+1, session.ID); err != nil {
			return false, err
		}
		return true, nil
	}, func(bool) int64 { return session.User.ID })
	if err != nil {
		return mapAdminRejection(err)
	}
	return nil
}

// Logout revokes the acting session through the execution runner
// (revocation is terminal; the audit row commits atomically).
func (service *Service) Logout(ctx context.Context, session Session) error {
	runCtx, err := service.sessionContext(ctx, session)
	if err != nil {
		return err
	}
	_, err = execution.Execute(withAdminCall(runCtx, session), service.runner, service.ops.logout, func(tx *execution.Tx) (bool, error) {
		now := service.timestamp()
		if _, err := tx.ExecContext(runCtx, `UPDATE sessions SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, now, session.ID); err != nil {
			return false, err
		}
		return true, nil
	}, func(bool) int64 { return session.ID })
	return err
}

func (service *Service) findUserByUsername(ctx context.Context, username string) (User, error) {
	return scanUser(service.read().QueryRowContext(ctx, `SELECT id,username,display_name,role,enabled,auth_revision,initialized,row_version,password_change_required,password_phc,(SELECT MAX(created_at) FROM sessions WHERE user_id=users.id) FROM users WHERE username=?`, username))
}

func findUserByID(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64) (User, error) {
	return scanUser(queryer.QueryRowContext(ctx, `SELECT id,username,display_name,role,enabled,auth_revision,initialized,row_version,password_change_required,password_phc,(SELECT MAX(created_at) FROM sessions WHERE user_id=users.id) FROM users WHERE id=?`, id))
}

func scanUser(row *sql.Row) (User, error) {
	var user User
	var enabled, initialized, required int
	var lastLogin sql.NullString
	err := row.Scan(&user.ID, &user.Username, &user.DisplayName, &user.Role, &enabled, &user.AuthRevision, &initialized, &user.RowVersion, &required, &user.passwordPHC, &lastLogin)
	if err != nil {
		return User{}, err
	}
	user.Locator = fmt.Sprint(user.ID)
	user.Enabled = enabled == 1
	user.Initialized = initialized == 1
	user.PasswordChangeRequired = required == 1
	if lastLogin.Valid {
		user.LastLoginAt = &lastLogin.String
	}
	return user, nil
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
