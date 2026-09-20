package auth

// Single-step local login (ADR-0010 / docs/authentication-design.md §4). A
// verified password issues the full session directly: the platform no longer
// runs a second factor, the IdP owns that duty for everyday accounts and this
// channel exists for IdP-outage maintenance. The credential verification runs
// inside the runner transaction so both outcomes — success and wrong-password
// rejection — commit an audit row (auth.login.local), matching the login
// audit contract of the redesign.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// localLoginRow carries the login-path facts beyond the shared user
// projection: the bootstrap expiry gate and the external-identity presence.
type localLoginRow struct {
	User                  User
	InitialPasswordExpiry sql.NullString
	HasIdentity           bool
}

// findLocalLoginUser reads one user with the local-login gate facts.
func findLocalLoginUser(ctx context.Context, reader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, username string,
) (localLoginRow, error) {
	row := reader.QueryRowContext(ctx, `SELECT id,username,display_name,role,enabled,auth_revision,initialized,row_version,password_change_required,password_phc,initial_password_expires_at,EXISTS(SELECT 1 FROM identities i WHERE i.user_id=users.id),(SELECT MAX(created_at) FROM sessions WHERE user_id=users.id) FROM users WHERE username=?`, username)
	var entry localLoginRow
	var enabled, initialized, required int
	var passwordPHC sql.NullString
	var lastLogin sql.NullString
	if err := row.Scan(&entry.User.ID, &entry.User.Username, &entry.User.DisplayName, &entry.User.Role, &enabled, &entry.User.AuthRevision, &initialized, &entry.User.RowVersion, &required, &passwordPHC, &entry.InitialPasswordExpiry, &entry.HasIdentity, &lastLogin); err != nil {
		return localLoginRow{}, err
	}
	entry.User.passwordPHC = passwordPHC.String
	entry.User.Locator = fmt.Sprint(entry.User.ID)
	entry.User.Enabled = enabled == 1
	entry.User.Initialized = initialized == 1
	entry.User.PasswordChangeRequired = required == 1
	entry.User.AuthSource = "local"
	if entry.HasIdentity {
		entry.User.AuthSource = "oidc"
	}
	if lastLogin.Valid {
		entry.User.LastLoginAt = &lastLogin.String
	}
	return entry, nil
}

// LoginWithPassword verifies the credential once and issues the full session
// in the same runner transaction (one audit row per attempt). The in-memory
// limiter sits in front; the transaction is the authority for the user state.
// A bootstrap administrator whose initial random password expired is rejected
// with ErrInitialPasswordExpired — only `quoin admin recover` can re-arm it.
func (service *Service) LoginWithPassword(ctx context.Context, username, password, userAgent string) (LoginResult, error) {
	normalized := NormalizeUsername(username)
	if allowed, retryAfter := service.limiter.allow(normalized); !allowed {
		service.passwords.VerifyDummy(password)
		return LoginResult{}, fmt.Errorf("%w: retry after %s", ErrRateLimited, retryAfter)
	}
	// Pre-transaction read resolves the audit actor (the attempted account);
	// the business closure re-reads the row inside the transaction, which
	// stays the authority. An unwired read pool fails closed here.
	entry, readErr := findLocalLoginUser(ctx, service.read(), normalized)
	if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
		return LoginResult{}, readErr
	}
	actor := execution.Principal{Kind: execution.PrincipalUser}
	if readErr != nil {
		// Unknown username: attribute the audited failure to the system
		// principal rather than inventing a user id.
		actor = execution.Principal{Kind: execution.PrincipalSystem}
	} else {
		actor.ID = entry.User.ID
	}
	meta, exists := execution.FromContext(ctx)
	if !exists {
		correlation, err := execution.NewCorrelationID()
		if err != nil {
			return LoginResult{}, err
		}
		meta = execution.Metadata{
			CorrelationID: correlation,
			Actor:         actor,
			Source:        execution.Source{Kind: execution.SourceInternal},
		}
		attached, attachErr := execution.WithMetadata(ctx, meta)
		if attachErr != nil {
			return LoginResult{}, attachErr
		}
		ctx = attached
	}
	type loginOutcome struct {
		Bearer string
		UserID int64
	}
	result, err := execution.Execute(ctx, service.runner, service.ops.loginLocal, func(tx *execution.Tx) (loginOutcome, error) {
		entry, err := findLocalLoginUser(ctx, tx, normalized)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// Uniform failure: an unknown username costs the caller the
				// same verification work as a wrong password.
				service.passwords.VerifyDummy(password)
				return loginOutcome{}, &execution.RecordedFailure{Code: "unauthenticated", Detail: "username or password is incorrect"}
			}
			return loginOutcome{}, err
		}
		if entry.HasIdentity || entry.User.passwordPHC == "" {
			// External accounts have no local password by construction; the
			// login page routes them to the IdP. This is a deterministic
			// rejection, not a credential failure.
			return loginOutcome{}, &execution.Rejection{Code: "local_login_unavailable", Detail: "this account signs in through the unified identity platform", ObjectID: entry.User.ID}
		}
		candidate, normalizeErr := NormalizePassword(password)
		if normalizeErr != nil || !VerifyPassword(candidate, entry.User.passwordPHC) || !entry.User.Enabled {
			return loginOutcome{}, &execution.RecordedFailure{Code: "unauthenticated", Detail: "username or password is incorrect", ObjectID: entry.User.ID}
		}
		if entry.InitialPasswordExpiry.Valid {
			expiry, parseErr := time.Parse(time.RFC3339Nano, entry.InitialPasswordExpiry.String)
			if parseErr != nil {
				return loginOutcome{}, fmt.Errorf("parse initial password expiry: %w", parseErr)
			}
			if !service.now().UTC().Before(expiry) {
				return loginOutcome{}, &execution.RecordedFailure{Code: "initial_password_expired", Detail: "initial administrator password expired; run quoin admin recover", ObjectID: entry.User.ID}
			}
		}
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return loginOutcome{}, fmt.Errorf("create session: %w", err)
		}
		digest := sha256.Sum256(raw)
		nowTime := service.now().UTC()
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(?,?,?,?,?,?,?,?)`,
			entry.User.ID, digest[:], entry.User.AuthRevision, clientLabel(userAgent), nowTime.Format(time.RFC3339Nano), nowTime.Format(time.RFC3339Nano), nowTime.Add(12*time.Hour).Format(time.RFC3339Nano), nowTime.Add(7*24*time.Hour).Format(time.RFC3339Nano)); err != nil {
			return loginOutcome{}, fmt.Errorf("persist session: %w", err)
		}
		return loginOutcome{Bearer: base64.RawURLEncoding.EncodeToString(raw), UserID: entry.User.ID}, nil
	}, func(result loginOutcome) int64 { return result.UserID })
	if err != nil {
		mapped := mapLoginFailure(err)
		if errors.Is(mapped, ErrUnauthenticated) || errors.Is(mapped, ErrInitialPasswordExpired) {
			service.limiter.failed(normalized)
		}
		return LoginResult{}, mapped
	}
	service.limiter.succeeded(normalized)
	user, err := findUserByID(ctx, service.read(), result.UserID)
	if err != nil {
		return LoginResult{}, err
	}
	user.AuthSource = "local"
	now := service.timestamp()
	user.LastLoginAt = &now
	return LoginResult{Bearer: result.Bearer, User: user}, nil
}

// mapLoginFailure translates the login channel's runner outcomes back into
// the package sentinels so the app keeps one stable error surface.
func mapLoginFailure(err error) error {
	var failure *execution.RecordedFailure
	if errors.As(err, &failure) {
		switch failure.Code {
		case "unauthenticated":
			return ErrUnauthenticated
		case "initial_password_expired":
			return ErrInitialPasswordExpired
		}
	}
	var rejection *execution.Rejection
	if errors.As(err, &rejection) && rejection.Code == "local_login_unavailable" {
		return ErrLocalLoginUnavailable
	}
	return err
}
