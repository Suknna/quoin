package auth

// Deployment bootstrap (docs/authentication-design.md §2): an empty database
// gets exactly one pending built-in administrator whose initial password is
// the public default. The deployment access boundary is owned by the intranet
// deployment environment, so no one-time install credential exists: first
// login with the default goes straight into the unified initialization flow.

import (
	"context"
	"errors"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// ErrAlreadySetup is returned by bootstrap when any user already exists.
// (Also kept for the legacy CreateFirstAdmin path.)
// EnsureBootstrapAdmin reports the same situation as (false, nil).

// EnsureBootstrapAdmin seeds the pending built-in administrator on an empty
// database. The call is idempotent: with any user present it returns
// (false, nil) without touching state, so a restart with an already-seeded
// database never re-opens the default password.
func (service *Service) EnsureBootstrapAdmin(ctx context.Context, retentionMonths ...int) (bool, error) {
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
		phc, err := HashPassword(bootstrapDefaultPassword)
		if err != nil {
			return seedResult{}, err
		}
		now := service.timestamp()
		if _, err := tx.ExecContext(ctx, `INSERT INTO users(username,display_name,role,enabled,auth_revision,initialized,password_phc,password_change_required,row_version,created_at,updated_at) VALUES('admin','Administrator','admin',1,1,0,?,0,1,?,?)`, phc, now, now); err != nil {
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
// waits for initialization. The login page uses this to route to the
// initialization wizard instead of exposing the default-password state.
func (service *Service) HasPendingBootstrapAdmin(ctx context.Context) (bool, error) {
	var count int
	if err := service.read().QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role='admin' AND initialized=0 AND enabled=1`).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}
