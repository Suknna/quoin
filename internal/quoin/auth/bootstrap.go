package auth

// Deployment bootstrap (ADR-0010 / docs/authentication-design.md §2): an
// empty database gets exactly one pending built-in administrator whose
// initial password is randomly generated at first start, written to a 0600
// file next to the database by the application layer, and expires after 24
// hours unless the administrator completes the forced password change. There
// is no public default password and no re-arm on restart; `quoin admin
// recover` is the only re-arm path once the initial password expired.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// InitialPasswordLifetime bounds the bootstrap administrator's initial
// random password: past the deadline the credential is dead and only
// `quoin admin recover` can mint a new one.
const InitialPasswordLifetime = 24 * time.Hour

// GenerateInitialPassword mints the bootstrap credential: 24 random bytes in
// unpadded base64 (32 characters). It always satisfies the formal password
// policy trivially and never contains characters hostile to shell copy/paste.
func GenerateInitialPassword() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate initial password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// EnsureBootstrapAdmin seeds the pending built-in administrator on an empty
// database. initialPassword is the deployment-generated bootstrap credential
// (the caller owns the 0600 initial-admin-password file); expiresAt is its
// 24-hour deadline persisted on the users row. The call is idempotent: with
// any user present it returns (false, nil) without touching state, so a
// restart with an already-seeded database never re-arms a credential.
func (service *Service) EnsureBootstrapAdmin(ctx context.Context, initialPassword string, expiresAt time.Time, retentionMonths ...int) (bool, error) {
	meta := execution.Metadata{
		Actor:  execution.Principal{Kind: execution.PrincipalSystem},
		Source: execution.Source{Kind: execution.SourceInternal},
	}
	correlation, err := execution.NewCorrelationID()
	if err != nil {
		return false, err
	}
	meta.CorrelationID = correlation
	// Bootstrap is its own root operation: an existing caller context must
	// not leak its correlation into the system seed (fail loudly, never
	// silently re-root).
	if _, exists := execution.FromContext(ctx); exists {
		return false, errors.New("bootstrap seed requires a context without execution metadata")
	}
	runCtx, err := execution.WithMetadata(ctx, meta)
	if err != nil {
		return false, err
	}
	result, err := execution.Execute(runCtx, service.runner, service.ops.seedBootstrap, func(tx *execution.Tx) (seedResult, error) {
		var count, admins int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN role='admin' AND enabled=1 THEN 1 ELSE 0 END),0) FROM users`).Scan(&count, &admins); err != nil {
			return seedResult{}, err
		}
		if count > 0 {
			if admins != 1 {
				return seedResult{}, errors.New("database must contain exactly one enabled administrator")
			}
			return seedResult{}, execution.ErrNoTransition
		}
		if len(retentionMonths) > 1 {
			return seedResult{}, errors.New("bootstrap accepts only one audit retention default")
		}
		if len(retentionMonths) == 1 && retentionMonths[0] != 0 {
			if retentionMonths[0] < 6 || retentionMonths[0] > 240 {
				return seedResult{}, errors.New("audit retention must be between six and 240 months")
			}
			if _, err := tx.ExecContext(ctx, `UPDATE audit_retention SET retention_months=? WHERE id=1 AND row_version=1 AND updated_by_type IS NULL`, retentionMonths[0]); err != nil {
				return seedResult{}, err
			}
		}
		// The generated bootstrap credential always satisfies the policy; the
		// explicit validation keeps that contract true even if the generator
		// changes shape later.
		validated, policyErr := ValidateNewPassword(initialPassword, "admin", "Administrator")
		if policyErr != nil {
			return seedResult{}, fmt.Errorf("initial password violates policy: %w", policyErr)
		}
		phc, err := HashPassword(validated)
		if err != nil {
			return seedResult{}, err
		}
		now := service.now().UTC()
		// password_change_required=1 keeps the first login on the restricted
		// session until the administrator sets a formal password, which also
		// clears the initial-password deadline atomically.
		if _, err := tx.ExecContext(ctx, `INSERT INTO users(username,display_name,role,enabled,auth_revision,initialized,password_phc,password_change_required,password_change_required_at,initial_password_expires_at,row_version,created_at,updated_at) VALUES('admin','Administrator','admin',1,1,0,?,1,?,?,1,?,?)`,
			phc, now.Format(time.RFC3339Nano), expiresAt.UTC().Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
			return seedResult{}, fmt.Errorf("seed pending administrator: %w", err)
		}
		return seedResult{Created: true}, nil
	}, func(result seedResult) int64 { return 0 })
	if errors.Is(err, execution.ErrNoTransition) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return result.Created, nil
}

type seedResult struct {
	Created bool
}

// IsDeploymentInitialized reports whether the built-in administrator
// completed initialization. Only the admin closes the bootstrap window: an
// initialized operator must never end it.
func (service *Service) IsDeploymentInitialized(ctx context.Context) (bool, error) {
	var count int
	if err := service.read().QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role='admin' AND initialized=1`).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// HasPendingBootstrapAdmin reports whether the built-in administrator still
// waits for initialization. The login page uses this to route to the forced
// password change instead of exposing the bootstrap state.
func (service *Service) HasPendingBootstrapAdmin(ctx context.Context) (bool, error) {
	var count int
	if err := service.read().QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role='admin' AND initialized=0 AND enabled=1`).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}
