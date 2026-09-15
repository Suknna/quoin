package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

func VerifyExecutionSession(ctx context.Context, reader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, requiredRole string) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalUser || meta.Session.ID <= 0 || meta.Session.AuthRevision <= 0 {
		return ErrActorChanged
	}
	var enabled, initialized, passwordRequired int
	var role string
	var revision int64
	var revoked sql.NullString
	var idle, absolute string
	err = reader.QueryRowContext(ctx, `SELECT u.enabled,u.initialized,u.password_change_required,u.role,u.auth_revision,s.revoked_at,s.idle_expires_at,s.absolute_expires_at FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.id=? AND s.user_id=? AND s.auth_revision_at_issue=u.auth_revision`, meta.Session.ID, meta.Actor.ID).Scan(&enabled, &initialized, &passwordRequired, &role, &revision, &revoked, &idle, &absolute)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrActorChanged
	}
	if err != nil {
		return err
	}
	if enabled != 1 || initialized != 1 || passwordRequired != 0 || revision != meta.Session.AuthRevision || revoked.Valid {
		return ErrActorChanged
	}
	if requiredRole != "" && role != requiredRole {
		return ErrActorChanged
	}
	idleTime, err := time.Parse(time.RFC3339Nano, idle)
	if err != nil {
		return ErrActorChanged
	}
	absoluteTime, err := time.Parse(time.RFC3339Nano, absolute)
	if err != nil {
		return ErrActorChanged
	}
	now := time.Now().UTC()
	if !now.Before(idleTime) || !now.Before(absoluteTime) {
		return ErrActorChanged
	}
	return nil
}
