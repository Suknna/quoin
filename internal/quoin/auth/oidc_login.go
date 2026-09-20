package auth

// OIDC identity resolution and JIT provisioning (ADR-0010 / docs/
// authentication-design.md §1/§3). The external identity key is strictly
// identities(issuer, subject) — the email claim never matches accounts. An
// unknown identity is provisioned as an enabled operator with no local
// password in one audited transaction (auth.oidc.jit); the session issue is
// the separate audited auth.login.oidc operation.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// OIDCIdentity is the verified claim projection of one completed round-trip.
type OIDCIdentity struct {
	Issuer        string
	Subject       string
	Username      string // preferred_username, may be empty
	DisplayName   string // name claim, may be empty
	Email         string
	EmailVerified bool
}

// ResolveOIDCIdentity maps the (issuer, subject) pair to its user, JIT
// provisioning an enabled operator with a deterministic unique username when
// the identity is unknown. Email is stored as a display-only contact and
// never participates in matching. The operation is idempotent: a repeat
// resolves to the same user without re-provisioning.
func (service *Service) ResolveOIDCIdentity(ctx context.Context, identity OIDCIdentity) (int64, error) {
	if identity.Issuer == "" || identity.Subject == "" {
		return 0, fmt.Errorf("%w: issuer and subject are required", ErrOIDCRejected)
	}
	if _, exists := execution.FromContext(ctx); !exists {
		correlation, err := execution.NewCorrelationID()
		if err != nil {
			return 0, err
		}
		attached, attachErr := execution.WithMetadata(ctx, execution.Metadata{
			CorrelationID: correlation,
			Actor:         execution.Principal{Kind: execution.PrincipalSystem},
			Source:        execution.Source{Kind: execution.SourceInternal},
		})
		if attachErr != nil {
			return 0, attachErr
		}
		ctx = attached
	}
	result, err := execution.Execute(ctx, service.runner, service.ops.oidcJit, func(tx *execution.Tx) (int64, error) {
		var userID int64
		err := tx.QueryRowContext(ctx, `SELECT user_id FROM identities WHERE issuer=? AND subject=?`, identity.Issuer, identity.Subject).Scan(&userID)
		if err == nil {
			return userID, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		username, displayName := service.oidcNames(identity)
		now := service.now().UTC().Format(time.RFC3339Nano)
		insert, err := tx.ExecContext(ctx, `INSERT INTO users(username,display_name,role,enabled,auth_revision,initialized,password_phc,password_change_required,row_version,created_at,updated_at) VALUES(?,?,'operator',1,1,1,NULL,0,1,?,?)`,
			username, displayName, now, now)
		if err != nil {
			return 0, fmt.Errorf("provision oidc user: %w", err)
		}
		userID, err = insert.LastInsertId()
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO identities(user_id,issuer,subject,created_at) VALUES(?,?,?,?)`, userID, identity.Issuer, identity.Subject, now); err != nil {
			return 0, fmt.Errorf("provision oidc identity: %w", err)
		}
		if identity.Email != "" {
			verified := any(nil)
			if identity.EmailVerified {
				verified = now
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO user_contacts(user_id,channel,target,enabled,version,verified_at,created_at,updated_at) VALUES(?, 'email', ?, 1, 1, ?, ?, ?)`,
				userID, identity.Email, verified, now, now); err != nil {
				return 0, fmt.Errorf("provision oidc contact: %w", err)
			}
		}
		return userID, nil
	}, func(userID int64) int64 { return userID })
	if err != nil {
		return 0, err
	}
	return result, nil
}

// oidcNames derives the deterministic unique username and the display name.
// The email claim never participates (ADR-0010 red line).
func (service *Service) oidcNames(identity OIDCIdentity) (string, string) {
	base := NormalizeUsername(identity.Username)
	if base == "" {
		digest := sha256.Sum256([]byte(identity.Subject))
		base = "oidc-" + hex.EncodeToString(digest[:])[:16]
	}
	if len(base) > 60 {
		base = base[:60]
	}
	username := base
	for suffix := 1; ; suffix++ {
		var exists int
		if err := service.read().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM users WHERE username=?`, username).Scan(&exists); err != nil || exists == 0 {
			break
		}
		username = fmt.Sprintf("%s%d", base, suffix)
	}
	displayName := identity.DisplayName
	if displayName == "" {
		displayName = username
	}
	return username, displayName
}

// CompleteOIDCLogin finishes the channel: identity resolution (with JIT)
// followed by the audited session issue. A disabled account is a recorded
// failure even though the IdP authenticated the subject.
func (service *Service) CompleteOIDCLogin(ctx context.Context, issuer, subject, username, displayName, email string, emailVerified bool, userAgent string) (LoginResult, error) {
	userID, err := service.ResolveOIDCIdentity(ctx, OIDCIdentity{
		Issuer: issuer, Subject: subject, Username: username, DisplayName: displayName,
		Email: email, EmailVerified: emailVerified,
	})
	if err != nil {
		return LoginResult{}, err
	}
	if _, exists := execution.FromContext(ctx); !exists {
		correlation, err := execution.NewCorrelationID()
		if err != nil {
			return LoginResult{}, err
		}
		attached, attachErr := execution.WithMetadata(ctx, execution.Metadata{
			CorrelationID: correlation,
			Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: userID},
			Source:        execution.Source{Kind: execution.SourceInternal},
		})
		if attachErr != nil {
			return LoginResult{}, attachErr
		}
		ctx = attached
	}
	type loginOutcome struct {
		Bearer string
	}
	result, err := execution.Execute(ctx, service.runner, service.ops.loginOIDC, func(tx *execution.Tx) (loginOutcome, error) {
		var enabled int
		var revision int64
		if err := tx.QueryRowContext(ctx, `SELECT enabled,auth_revision FROM users WHERE id=?`, userID).Scan(&enabled, &revision); err != nil {
			return loginOutcome{}, err
		}
		if enabled != 1 {
			return loginOutcome{}, &execution.RecordedFailure{Code: "account_disabled", Detail: "该账号已被禁用，无法登录。", ObjectID: userID}
		}
		bearer, err := issueSession(ctx, tx, userID, revision, clientLabel(userAgent), service.now().UTC())
		if err != nil {
			return loginOutcome{}, err
		}
		return loginOutcome{Bearer: bearer}, nil
	}, func(loginOutcome) int64 { return userID })
	if err != nil {
		var failure *execution.RecordedFailure
		if errors.As(err, &failure) && failure.Code == "account_disabled" {
			return LoginResult{}, ErrAccountDisabled
		}
		return LoginResult{}, err
	}
	user, err := findUserByID(ctx, service.read(), userID)
	if err != nil {
		return LoginResult{}, err
	}
	user.AuthSource = "oidc"
	now := service.timestamp()
	user.LastLoginAt = &now
	return LoginResult{Bearer: result.Bearer, User: user}, nil
}
