package auth

// Administrator recovery (ADR-0010 / docs/authentication-design.md §6): the
// offline `quoin admin recover` entry for a deployment whose administrator is
// locked out, and the credential reset the restore isolation shares. It
// never restores any default password and never creates a second
// administrator.
//
// Recovery runs offline as the system principal with the CLI source
// (exclusive database, attached TTY). The operator-chosen temporary password
// carries NO enforced expiry — its lifetime is the operator's stopped-service
// window, and any later recovery supersedes it. When the target is the still
// pending bootstrap administrator (initial password dead after 24 hours),
// recover re-arms the same generated-file path: the CLI mints a fresh random
// credential, rewrites the initial-admin-password file and restores the
// 24-hour deadline. Sessions are revoked; no recovery credential type exists
// on the wire.

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// RecoveryCredential is the reset result. TemporaryPassword is a SECRET
// returned to the caller exactly once — it never enters logs, ledgers or
// audit bodies. It has no enforced expiry by design.
type RecoveryCredential struct {
	TemporaryPassword string `json:"temporaryPassword,omitempty"`
}

// BeginRecovery replaces the built-in administrator's password with the
// operator-supplied temporary credential (policy validated) and marks the
// account for the forced password change on next sign-in. initialExpiry, when
// set, also (re)arms the 24-hour bootstrap deadline on the users row — the
// CLI passes it for the pending bootstrap administrator whose generated
// credential file it just rewrote. Sessions are revoked atomically.
func (service *Service) BeginRecovery(ctx context.Context, newPassword string, initialExpiry *time.Time) (RecoveryCredential, error) {
	// Recovery is a root CLI operation: an existing caller correlation must
	// never leak into it (the operation metadata is built fresh below, and
	// the operation's authorization admits only the system/CLI pairing).
	if _, exists := execution.FromContext(ctx); exists {
		return RecoveryCredential{}, errors.New("offline recovery requires a context without execution metadata")
	}
	correlation, err := execution.NewCorrelationID()
	if err != nil {
		return RecoveryCredential{}, err
	}
	runCtx, err := execution.WithMetadata(ctx, execution.Metadata{
		CorrelationID: correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Source:        execution.Source{Kind: execution.SourceCLI},
	})
	if err != nil {
		return RecoveryCredential{}, err
	}
	_, err = execution.Execute(runCtx, service.runner, service.ops.recoveryBegin, func(tx *execution.Tx) (int64, error) {
		admin, err := findBuiltInAdmin(ctx, tx)
		if err != nil {
			return 0, err
		}
		if err := service.resetAdminCredential(ctx, tx, admin, newPassword, initialExpiry, service.timestamp()); err != nil {
			return 0, err
		}
		return admin.ID, nil
	}, func(adminID int64) int64 { return adminID })
	if err != nil {
		return RecoveryCredential{}, mapRejection(err)
	}
	return RecoveryCredential{}, nil
}

// resetAdminCredential replaces the password hash, forces the change marker
// and optionally (re)arms the initial-password deadline in one statement.
func (service *Service) resetAdminCredential(ctx context.Context, tx *execution.Tx, admin User, newPassword string, initialExpiry *time.Time, now string) error {
	normalized, policyErr := ValidateNewPassword(newPassword, admin.Username, admin.DisplayName)
	if policyErr != nil {
		return rejection("password_policy", policyErr.Error(), admin.ID)
	}
	phc, err := HashPassword(normalized)
	if err != nil {
		return err
	}
	expiry := any(nil)
	if initialExpiry != nil {
		expiry = initialExpiry.UTC().Format(time.RFC3339Nano)
	}
	update, err := tx.ExecContext(ctx, `UPDATE users SET password_phc=?,initialized=0,password_change_required=1,password_change_required_at=?,initial_password_expires_at=?,auth_revision=auth_revision+1,row_version=row_version+1,updated_at=? WHERE id=? AND auth_revision=?`, phc, now, expiry, now, admin.ID, admin.AuthRevision)
	if err != nil {
		return err
	}
	if rows, _ := update.RowsAffected(); rows != 1 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, now, admin.ID); err != nil {
		return err
	}
	return nil
}

// ResetAdminCredentialOn drives one enabled built-in administrator into the
// forced-change state inside the CALLER'S open write transaction (the restore
// isolation shares this stage): a fresh generated temporary credential
// (hashed in place, raw value returned exactly once), initialized=0 with the
// forced change marker, no bootstrap deadline (the restore flow re-runs the
// ordinary password change). Session revocation stays the caller's call.
func ResetAdminCredentialOn(ctx context.Context, executor execution.Executor, adminID int64, now time.Time) (RecoveryCredential, error) {
	var revision int64
	err := executor.QueryRowContext(ctx, `SELECT auth_revision FROM users WHERE id=? AND role='admin' AND enabled=1`, adminID).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return RecoveryCredential{}, ErrNotFound
	}
	if err != nil {
		return RecoveryCredential{}, err
	}
	temporarySecret, err := GenerateInitialPassword()
	if err != nil {
		return RecoveryCredential{}, err
	}
	hashed, err := HashPassword(temporarySecret)
	if err != nil {
		return RecoveryCredential{}, err
	}
	timestamp := now.UTC().Format(time.RFC3339Nano)
	update, err := executor.ExecContext(ctx, `UPDATE users SET password_phc=?,initialized=0,password_change_required=1,password_change_required_at=?,initial_password_expires_at=NULL,auth_revision=auth_revision+1,row_version=row_version+1,updated_at=? WHERE id=? AND auth_revision=?`, hashed, timestamp, timestamp, adminID, revision)
	if err != nil {
		return RecoveryCredential{}, err
	}
	if rows, _ := update.RowsAffected(); rows != 1 {
		return RecoveryCredential{}, ErrNotFound
	}
	return RecoveryCredential{TemporaryPassword: temporarySecret}, nil
}

// findBuiltInAdmin reads the deployment's single built-in administrator.
func findBuiltInAdmin(ctx context.Context, reader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
},
) (User, error) {
	user, err := scanUser(reader.QueryRowContext(ctx, `SELECT id,username,display_name,role,enabled,auth_revision,initialized,row_version,password_change_required,password_phc,(SELECT MAX(created_at) FROM sessions WHERE user_id=users.id) FROM users WHERE role='admin'`))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return user, err
}

// withExecutionMetadataIfAbsent attaches trusted entry-point metadata when
// the caller context carries none; a caller-supplied metadata wins (the
// operation then runs as a step of the caller's correlation).
func withExecutionMetadataIfAbsent(ctx context.Context, meta execution.Metadata) (context.Context, error) {
	correlation, err := execution.NewCorrelationID()
	if err != nil {
		return nil, err
	}
	meta.CorrelationID = correlation
	attached, attachErr := execution.WithMetadata(ctx, meta)
	if attachErr != nil {
		return ctx, nil
	}
	return attached, nil
}
