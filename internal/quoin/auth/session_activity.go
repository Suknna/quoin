package auth

// Session activity renewal (docs/authentication-design.md §5). The idle
// window advances only for real user activity, throttled to one audited
// renewal per interval, and never past the absolute cap.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// activityRenewalInterval throttles session activity renewal so background
// polling cannot renew idle expiry on every request.
const activityRenewalInterval = 30 * time.Minute

// sessionActivity carries the touch-relevant facts of one live session row.
type sessionActivity struct {
	sessionID         int64
	userID            int64
	revoked           string
	lastActiveAt      string
	idleExpiresAt     string
	absoluteExpiresAt string
}

const sessionActivitySelect = `SELECT s.id,u.id,COALESCE(s.revoked_at,''),s.last_active_at,s.idle_expires_at,s.absolute_expires_at FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.session_token_digest=? AND s.auth_revision_at_issue=u.auth_revision AND u.enabled=1`

// readSessionActivity reads the touch-relevant session facts through any row
// reader. A missing, revoked, revision-stale, disabled or expired session is
// ErrUnauthenticated; a live session returns its facts.
func readSessionActivity(ctx context.Context, reader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, digest []byte,
) (sessionActivity, error) {
	var state sessionActivity
	err := reader.QueryRowContext(ctx, sessionActivitySelect, digest).Scan(&state.sessionID, &state.userID, &state.revoked, &state.lastActiveAt, &state.idleExpiresAt, &state.absoluteExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return sessionActivity{}, ErrUnauthenticated
	}
	if err != nil {
		return sessionActivity{}, err
	}
	now := serviceClock()
	absolute, err := time.Parse(time.RFC3339Nano, state.absoluteExpiresAt)
	if err != nil || !now.Before(absolute) {
		return sessionActivity{}, ErrUnauthenticated
	}
	idle, err := time.Parse(time.RFC3339Nano, state.idleExpiresAt)
	if err != nil || !now.Before(idle) {
		return sessionActivity{}, ErrUnauthenticated
	}
	if _, err := time.Parse(time.RFC3339Nano, state.lastActiveAt); err != nil {
		return sessionActivity{}, ErrUnauthenticated
	}
	if state.revoked != "" {
		return sessionActivity{}, ErrUnauthenticated
	}
	return state, nil
}

// due reports whether the renewal throttle window has elapsed.
func (state sessionActivity) due() bool {
	last, err := time.Parse(time.RFC3339Nano, state.lastActiveAt)
	return err == nil && serviceClock().Sub(last) >= activityRenewalInterval
}

// TouchSessionActivity renews a session's activity window under a throttle:
// renewal happens only when the last update is older than
// activityRenewalInterval, and idle expiry never passes the absolute cap.
// The application decides which endpoints count as real activity; SSE
// heartbeats and background polling must not call this.
//
// Audit constraints (ADR-0006): the frequent no-op touches are pure reads —
// the throttle pre-check keeps them off the write path and therefore
// unaudited; only an actual renewal executes as the audited
// auth.session.activity operation, bounding the audit volume to one row per
// session and renewal interval. The runner transaction re-checks every
// session fact so the pre-check is an optimization, never the authority.
func (service *Service) TouchSessionActivity(ctx context.Context, bearer string) error {
	raw, err := base64.RawURLEncoding.DecodeString(bearer)
	if err != nil || len(raw) != 32 {
		return ErrUnauthenticated
	}
	digest := sha256.Sum256(raw)
	state, err := readSessionActivity(ctx, service.read(), digest[:])
	if err != nil {
		return err
	}
	if !state.due() {
		return nil
	}
	runCtx, err := withExecutionMetadataIfAbsent(ctx, execution.Metadata{
		Actor:  execution.Principal{Kind: execution.PrincipalUser, ID: state.userID},
		Source: execution.Source{Kind: execution.SourceInternal},
	})
	if err != nil {
		return err
	}
	call := &stepCall{userID: state.userID, tokenDigest: digest[:]}
	_, err = execution.Execute(withStepCall(runCtx, call), service.runner, service.ops.touchSessionActivity, func(tx *execution.Tx) (bool, error) {
		// The authoritative re-read decides again: a concurrent touch that
		// renewed first simply loses the race and stays a no-op.
		current, err := readSessionActivity(ctx, tx, call.tokenDigest)
		if err != nil {
			return false, err
		}
		if !current.due() {
			return false, nil
		}
		now := serviceClock()
		renewedIdle := now.Add(12 * time.Hour)
		if absolute, parseErr := time.Parse(time.RFC3339Nano, current.absoluteExpiresAt); parseErr == nil && renewedIdle.After(absolute) {
			renewedIdle = absolute
		}
		update, err := tx.ExecContext(ctx, `UPDATE sessions SET last_active_at=?,idle_expires_at=? WHERE id=? AND revoked_at IS NULL`, now.Format(time.RFC3339Nano), renewedIdle.Format(time.RFC3339Nano), current.sessionID)
		if err != nil {
			return false, err
		}
		if rows, _ := update.RowsAffected(); rows != 1 {
			return false, ErrUnauthenticated
		}
		return true, nil
	}, func(bool) int64 { return state.sessionID }) // audit target: the touched sessions row itself
	return err
}

// serviceClock is a tiny indirection for deterministic tests.
var clockNow = func() time.Time { return time.Now() }

func serviceClock() time.Time { return clockNow().UTC().Round(0) }
