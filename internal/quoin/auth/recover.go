package auth

// Administrator recovery (docs/authentication-design.md §6): the offline
// `quoin admin recover` entry for a deployment whose administrator is locked
// out. It never restores the default password and never creates a second
// administrator.
//
// Recovery runs offline as the system principal with the CLI source
// (exclusive database, attached TTY). Both modes set a temporary password
// (printed by the CLI for mode "factors") and mark the account for the
// unified initialization flow: after the service starts, the administrator
// signs in with the temporary password and completes the same steps as a
// first install (formal password plus verified contact). The temporary
// password carries NO enforced expiry — its lifetime is the operator's
// stopped-service window, and any later recovery supersedes it. Sessions and
// pending flows are revoked; no recovery flow type exists. There is no
// frontend recovery route and no recovery credential on the wire.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// RecoveryMode selects the offline recovery strategy (docs/authentication-design.md §6).
type RecoveryMode string

const (
	// RecoveryModePassword replaces only the password with the operator-chosen
	// temporary credential; verified contacts are preserved, and the
	// administrator completes the unified initialization flow on next sign-in.
	// Retained targets still require a FRESH per-flow OTP: the flow's
	// factorVerified marker only turns true after this flow's own challenge.
	RecoveryModePassword RecoveryMode = "password"
	// RecoveryModeFactors resets every factor (contacts included) and the
	// password with a generated temporary credential; the administrator
	// completes the unified initialization flow (formal password plus a new
	// verified contact) on next sign-in.
	RecoveryModeFactors RecoveryMode = "factors"
)

// RecoveryCredential is the BeginRecoveryFactorsOn result. TemporaryPassword
// is a SECRET returned to the caller exactly once — it never enters logs,
// ledgers or audit bodies. It has no enforced expiry by design.
type RecoveryCredential struct {
	TemporaryPassword string `json:"temporaryPassword,omitempty"`
}

// BeginRecoveryFactorsOn applies the factors recovery mutation to one enabled
// built-in administrator inside the CALLER'S open write transaction: it
// replaces the password with the returned temporary credential (hash
// persisted, raw value returned exactly once), retires every contact target
// (disabled and unverified — a genuine factors reset whose history stays
// intact), resets the account to the pending state, revokes the user's
// sessions and pending flows, and supersedes older temporary passwords. After
// the service starts, the administrator signs in with the temporary
// credential and completes the unified initialization flow (formal password
// plus new verified contact). now supplies the persisted timestamps.
func BeginRecoveryFactorsOn(ctx context.Context, executor execution.Executor, adminID int64, now time.Time) (RecoveryCredential, error) {
	var revision int64
	err := executor.QueryRowContext(ctx, `SELECT auth_revision FROM users WHERE id=? AND role='admin' AND enabled=1`, adminID).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return RecoveryCredential{}, ErrNotFound
	}
	if err != nil {
		return RecoveryCredential{}, err
	}
	temporarySecret, err := randomHex(32)
	if err != nil {
		return RecoveryCredential{}, err
	}
	hashed, err := HashPassword(temporarySecret)
	if err != nil {
		return RecoveryCredential{}, err
	}
	timestamp := now.UTC().Format(time.RFC3339Nano)
	update, err := executor.ExecContext(ctx, `UPDATE users SET password_phc=?,initialized=0,password_change_required=1,password_change_required_at=?,auth_revision=auth_revision+1,row_version=row_version+1,updated_at=? WHERE id=? AND auth_revision=?`, hashed, timestamp, timestamp, adminID, revision)
	if err != nil {
		return RecoveryCredential{}, err
	}
	if rows, _ := update.RowsAffected(); rows != 1 {
		return RecoveryCredential{}, ErrNotFound
	}
	// The factors reset RETIRES every target (enabled=0, unverified, version
	// bumped) instead of deleting rows: completed flows and consumed
	// challenges keep their foreign-key history, the unified initialization
	// flow exposes no usable target, and re-enrolling a channel reuses its
	// row with a fresh verification requirement.
	if _, err := executor.ExecContext(ctx, `UPDATE user_contacts SET enabled=0,version=version+1,verified_at=NULL,updated_at=? WHERE user_id=? AND enabled=1`, timestamp, adminID); err != nil {
		return RecoveryCredential{}, err
	}
	if err := revokeRecoverySideEffects(ctx, executor, adminID, timestamp); err != nil {
		return RecoveryCredential{}, err
	}
	return RecoveryCredential{TemporaryPassword: temporarySecret}, nil
}

// BeginRecovery runs the offline half of the administrator recovery. It
// targets the deployment's single built-in administrator. mode "password"
// requires a temporary newPassword (policy validated); verified contacts are
// preserved, and the administrator completes the unified initialization flow
// on next sign-in. mode "factors" resets every factor (contacts retired) and
// the password with the printed temporary credential. Both modes revoke the
// user's sessions and pending flows and supersede older temporary passwords.
func (service *Service) BeginRecovery(ctx context.Context, mode RecoveryMode, newPassword string) (RecoveryCredential, error) {
	switch mode {
	case RecoveryModePassword, RecoveryModeFactors:
	default:
		return RecoveryCredential{}, fmt.Errorf("%w: recovery mode must be %q or %q", ErrValidation, RecoveryModePassword, RecoveryModeFactors)
	}
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
	call := &stepCall{}
	result, err := execution.Execute(withStepCall(runCtx, call), service.runner, service.ops.recoveryBegin, func(tx *execution.Tx) (recoveryBeginResult, error) {
		admin, err := findBuiltInAdmin(ctx, tx)
		if err != nil {
			return recoveryBeginResult{}, err
		}
		if mode == RecoveryModePassword {
			if err := service.beginPasswordRecovery(ctx, tx, admin, newPassword, service.timestamp()); err != nil {
				return recoveryBeginResult{}, err
			}
			return recoveryBeginResult{AdminID: admin.ID}, nil
		}
		credential, err := BeginRecoveryFactorsOn(ctx, tx, admin.ID, service.now())
		return recoveryBeginResult{AdminID: admin.ID, Credential: credential}, err
	}, func(result recoveryBeginResult) int64 { return result.AdminID })
	if err != nil {
		return RecoveryCredential{}, mapRejection(err)
	}
	return result.Credential, nil
}

type recoveryBeginResult struct {
	AdminID    int64
	Credential RecoveryCredential
}

// beginPasswordRecovery replaces the password hash with the operator-chosen
// temporary credential and marks the account for the unified initialization
// flow (formal password plus verified contact on next sign-in).
func (service *Service) beginPasswordRecovery(ctx context.Context, tx *execution.Tx, admin User, newPassword, now string) error {
	normalized, policyErr := ValidateNewPassword(newPassword, admin.Username, admin.DisplayName)
	if policyErr != nil {
		return rejection("password_policy", policyErr.Error(), admin.ID)
	}
	phc, err := HashPassword(normalized)
	if err != nil {
		return err
	}
	update, err := tx.ExecContext(ctx, `UPDATE users SET password_phc=?,initialized=0,password_change_required=1,password_change_required_at=?,auth_revision=auth_revision+1,row_version=row_version+1,updated_at=? WHERE id=? AND auth_revision=?`, phc, now, now, admin.ID, admin.AuthRevision)
	if err != nil {
		return err
	}
	if rows, _ := update.RowsAffected(); rows != 1 {
		return ErrNotFound
	}
	return revokeRecoverySideEffects(ctx, tx, admin.ID, now)
}

// revokeRecoverySideEffects applies the shared revocation sweep of both
// recovery modes: sessions and pending flows of the user (a newer recovery
// supersedes older temporary passwords and kills in-flight flows).
func revokeRecoverySideEffects(ctx context.Context, writer execution.Executor, userID int64, now string) error {
	if _, err := writer.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, now, userID); err != nil {
		return err
	}
	if _, err := writer.ExecContext(ctx, `UPDATE auth_flows SET status='revoked' WHERE user_id=? AND status='pending'`, userID); err != nil {
		return err
	}
	return nil
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
