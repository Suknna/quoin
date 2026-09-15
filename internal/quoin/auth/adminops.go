package auth

// Admin/own-session command plumbing over the execution runner: every legacy
// mutation (create/update/reset contacts/revoke/change-password/logout) runs
// through execution.Run or execution.Execute — one runner-owned transaction,
// authorization re-checked inside it, ledger row and audit committed
// atomically. No hand-written audit calls remain. Deterministic rejections
// travel as execution.Rejection whose Detail carries the legacy
// ConflictDetail JSON so replays rebuild byte-identical typed errors.

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// adminCall carries the acting session (ids only, no secrets) into the fixed
// Authorize callbacks of session-originated commands.
type adminCall struct {
	session Session
}

type adminCallKey struct{}

func withAdminCall(ctx context.Context, session Session) context.Context {
	return context.WithValue(ctx, adminCallKey{}, session)
}

func adminCallFromContext(ctx context.Context) (Session, bool) {
	session, ok := ctx.Value(adminCallKey{}).(Session)
	return session, ok
}

// authorizeSessionCall re-verifies the acting session (and, for admin
// commands, the administrator role) inside the runner transaction.
func authorizeSessionCall(requireAdmin bool) func(context.Context, *execution.Tx) error {
	return func(ctx context.Context, tx *execution.Tx) error {
		session, ok := adminCallFromContext(ctx)
		if !ok {
			return errors.New("auth: session inputs are missing from the request context")
		}
		return verifyActorTx(ctx, tx, session, requireAdmin)
	}
}

// authorizeBootstrapSeedLike guards the legacy offline CreateFirstAdmin seed:
// system principal with an internal or CLI source only.
func authorizeBootstrapSeedLike(ctx context.Context, _ *execution.Tx) error {
	meta, ok := execution.FromContext(ctx)
	if !ok {
		return errors.New("auth: legacy bootstrap requires execution metadata")
	}
	if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
		return errors.New("auth: legacy bootstrap is reserved for the system principal")
	}
	if meta.Source.Kind != execution.SourceInternal && meta.Source.Kind != execution.SourceCLI {
		return errors.New("auth: legacy bootstrap source must be internal or cli")
	}
	return nil
}

// verifyActorTx re-reads the acting session AND user inside the open
// transaction: the session must belong to the caller at the same auth
// revision without revocation or expiry, and the user must be enabled at that
// revision. Admin commands additionally require the administrator role
// (DATA-TX-002): a session revoked without a revision change must not keep
// write access until its next read.
func verifyActorTx(ctx context.Context, reader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, session Session, requireAdmin bool) error {
	var revoked sql.NullString
	var idleExpires, absoluteExpires string
	err := reader.QueryRowContext(ctx, `SELECT s.revoked_at,s.idle_expires_at,s.absolute_expires_at FROM sessions s WHERE s.id=? AND s.user_id=? AND s.auth_revision_at_issue=?`, session.ID, session.User.ID, session.User.AuthRevision).Scan(&revoked, &idleExpires, &absoluteExpires)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrActorChanged
		}
		return err
	}
	if revoked.Valid {
		return ErrActorChanged
	}
	now := time.Now().UTC()
	idleTime, idleErr := time.Parse(time.RFC3339Nano, idleExpires)
	absoluteTime, absoluteErr := time.Parse(time.RFC3339Nano, absoluteExpires)
	if idleErr != nil || absoluteErr != nil || !now.Before(idleTime) || !now.Before(absoluteTime) {
		return ErrActorChanged
	}
	actor, err := findUserByID(ctx, reader, session.User.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrActorChanged
		}
		return err
	}
	if !actor.Enabled || actor.AuthRevision != session.User.AuthRevision {
		return ErrActorChanged
	}
	if requireAdmin && (actor.Role != "admin" || actor.passwordPHC != session.User.passwordPHC || subtle.ConstantTimeCompare([]byte(actor.passwordPHC), []byte(session.User.passwordPHC)) != 1) {
		return ErrActorChanged
	}
	return nil
}

// sessionContext attaches the caller's session-derived execution metadata
// when the request context carries none; an app-provided metadata block wins
// (the admission layer fills actor and session from the authenticated
// request). The session reference is ids only — never a credential.
func (service *Service) sessionContext(ctx context.Context, session Session) (context.Context, error) {
	if _, exists := execution.FromContext(ctx); exists {
		return ctx, nil
	}
	correlation, err := execution.NewCorrelationID()
	if err != nil {
		return nil, err
	}
	meta := execution.Metadata{
		CorrelationID: correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: session.User.ID},
		Source:        execution.Source{Kind: execution.SourceInternal},
		Session:       execution.SessionRef{ID: session.ID, AuthRevision: session.User.AuthRevision},
	}
	return execution.WithMetadata(ctx, meta)
}

// sessionCommand builds the durable ledger key of one session-originated
// command. Digest comes from the caller's DigestCommand projection of the
// non-secret semantic fields.
func sessionCommand(session Session, clientCommandID, digest string) execution.Command {
	return execution.Command{
		PrincipalType:   "user",
		PrincipalID:     session.User.ID,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}
}

// conflictRejection wraps the legacy ConflictDetail into a runner rejection.
// The full detail JSON rides in Detail so the ledger replay rebuilds the same
// typed error the fresh path produced.
func conflictRejection(detail ConflictDetail) *execution.Rejection {
	body, err := json.Marshal(detail)
	if err != nil {
		body = []byte(`{}`)
	}
	return &execution.Rejection{Code: detail.Code, Detail: string(body), ObjectID: parseLocator(detail.ObjectID)}
}

// mapAdminRejection translates runner outcomes back into the package's stable
// error surface: command reuse, deterministic rejections (including the
// authoritative row version inside RowVersionError) and pass-through
// infrastructure failures.
func mapAdminRejection(err error) error {
	if errors.Is(err, execution.ErrCommandReused) {
		return ErrCommandReused
	}
	var rejection *execution.Rejection
	if errors.As(err, &rejection) {
		var detail ConflictDetail
		if jsonErr := json.Unmarshal([]byte(rejection.Detail), &detail); jsonErr == nil && detail.Code != "" {
			return detail.asError()
		}
	}
	return err
}

// verifySessionTx is the own-session authorization used by logout and the
// self-service password change.
func verifySessionTx(ctx context.Context, tx *execution.Tx, session Session) error {
	return verifyActorTx(ctx, tx, session, false)
}
