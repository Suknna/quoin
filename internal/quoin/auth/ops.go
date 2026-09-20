package auth

// Declared write operations of the authentication domain. Registration is
// static and fails fast at service construction; every operation carries the
// authorization callback the execution runner invokes inside its transaction
// before any business statement (ADR-0006 automatic audit).

import (
	"context"
	"errors"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

type authOperations struct {
	seedBootstrap *execution.Operation
	loginLocal    *execution.Operation
	oidcJit       *execution.Operation
	loginOIDC     *execution.Operation
	recoveryBegin *execution.Operation

	// Session-originated domain commands (runner-ledger path).
	adminCreateUser      *execution.Operation
	adminUpdateUser      *execution.Operation
	adminResetPassword   *execution.Operation
	adminSetContacts     *execution.Operation
	adminRevokeSessions  *execution.Operation
	revokeOwnSession     *execution.Operation
	changeOwnPassword    *execution.Operation
	logout               *execution.Operation
	legacyBootstrapSeed  *execution.Operation
	touchSessionActivity *execution.Operation
}

func newAuthOperations() (authOperations, *execution.Registry) {
	registry := execution.NewRegistry()
	register := func(op execution.Operation) *execution.Operation {
		stored, err := registry.Register(op)
		if err != nil {
			// Static declarations: a duplicate or invalid name is a programmer
			// error that must surface at startup, not per request.
			panic(fmt.Sprintf("auth: register operation %q: %v", op.Name, err))
		}
		return stored
	}
	// authorizeBootstrapSeed restricts the seed to the system principal with
	// an internal or CLI source: an arbitrary caller context must never run it.
	authorizeBootstrapSeed := func(ctx context.Context, _ *execution.Tx) error {
		meta, ok := execution.FromContext(ctx)
		if !ok {
			return errors.New("auth: bootstrap seed requires execution metadata")
		}
		if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
			return errors.New("auth: bootstrap seed is reserved for the system principal")
		}
		if meta.Source.Kind != execution.SourceInternal && meta.Source.Kind != execution.SourceCLI {
			return fmt.Errorf("auth: bootstrap seed source must be internal or cli, got %q", meta.Source.Kind)
		}
		return nil
	}
	// authorizeLogin requires only execution metadata: the anonymous caller
	// cannot present a session yet, and the business closure itself verifies
	// the credential inside the transaction (the authority for the outcome).
	authorizeLogin := func(ctx context.Context, _ *execution.Tx) error {
		if _, ok := execution.FromContext(ctx); !ok {
			return errors.New("auth: login requires execution metadata")
		}
		return nil
	}
	return authOperations{
		seedBootstrap: register(execution.Operation{
			Name: "auth.bootstrap.seed", Class: execution.ClassWrite, ObjectType: "deployment",
			Authorize: authorizeBootstrapSeed,
		}),
		// The local emergency login: one audited operation whose outcome is
		// success (session issued) or rejected (wrong credential / expired
		// initial password / external account). The audit authority is the
		// account row — a failed attempt has no session to name.
		loginLocal: register(execution.Operation{
			Name: "auth.login.local", Class: execution.ClassWrite, ObjectType: "user",
			Authorize: authorizeLogin,
		}),
		// JIT provisioning of an unknown external identity: one audited
		// user+identity(+contact) creation, idempotent on the unique key.
		oidcJit: register(execution.Operation{
			Name: "auth.oidc.jit", Class: execution.ClassWrite, ObjectType: "user",
			Authorize: authorizeLogin,
		}),
		// The everyday SSO login: the audit authority is the account row.
		loginOIDC: register(execution.Operation{
			Name: "auth.login.oidc", Class: execution.ClassWrite, ObjectType: "user",
			Authorize: authorizeLogin,
		}),
		recoveryBegin: register(execution.Operation{
			Name: "auth.recovery.begin", Class: execution.ClassWrite, ObjectType: "user",
			// Recovery is a stopped-service CLI operation: the system principal
			// with the CLI source is the only admissible context. A caller
			// context that already carries HTTP or scheduler metadata can never
			// drive an offline recovery through the service.
			Authorize: authorizeRecoveryBegin,
		}),

		// Session-originated domain commands (execution.Run: ledger replay +
		// automatic audit replace the former hand-written recordOutcome path).
		adminCreateUser: register(execution.Operation{
			Name: "user.create", Class: execution.ClassWrite, ObjectType: "user",
			Authorize: authorizeSessionCall(true),
		}),
		adminUpdateUser: register(execution.Operation{
			Name: "user.update", Class: execution.ClassWrite, ObjectType: "user",
			Authorize: authorizeSessionCall(true),
		}),
		adminResetPassword: register(execution.Operation{
			Name: "user.reset_password", Class: execution.ClassWrite, ObjectType: "user",
			Authorize: authorizeSessionCall(true),
		}),
		adminSetContacts: register(execution.Operation{
			Name: "user.set_contacts", Class: execution.ClassWrite, ObjectType: "user",
			Authorize: authorizeSessionCall(true),
		}),
		// The aggregate revocation sweeps one user's whole session set: no
		// single session row identifies the command, so the target user is
		// the declared authority (the own-session variant revokes one named
		// row and stays session-scoped).
		adminRevokeSessions: register(execution.Operation{
			Name: "user.revoke_sessions", Class: execution.ClassWrite, ObjectType: "user",
			Authorize: authorizeSessionCall(true),
		}),
		revokeOwnSession: register(execution.Operation{
			Name: "session.revoke_own", Class: execution.ClassWrite, ObjectType: "session",
			Authorize: authorizeSessionCall(false),
		}),
		changeOwnPassword: register(execution.Operation{
			Name: "user.change_own_password", Class: execution.ClassWrite, ObjectType: "user",
			Authorize: authorizeSessionCall(false),
		}),
		logout: register(execution.Operation{
			Name: "session.logout", Class: execution.ClassWrite, ObjectType: "session",
			Authorize: authorizeSessionCall(false),
		}),
		legacyBootstrapSeed: register(execution.Operation{
			Name: "admin.bootstrap", Class: execution.ClassWrite, ObjectType: "deployment",
			Authorize: authorizeBootstrapSeedLike,
		}),
		touchSessionActivity: register(execution.Operation{
			Name: "auth.session.activity", Class: execution.ClassWrite, ObjectType: "session",
			Authorize: authorizeSessionTouch,
		}),
	}, registry
}

// authorizeRecoveryBegin guards the offline recovery start: system principal
// id 0 with the CLI source only.
func authorizeRecoveryBegin(ctx context.Context, _ *execution.Tx) error {
	meta, ok := execution.FromContext(ctx)
	if !ok || meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
		return errors.New("auth: offline recovery is reserved for the system principal")
	}
	if meta.Source.Kind != execution.SourceCLI {
		return fmt.Errorf("auth: offline recovery source must be cli, got %q", meta.Source.Kind)
	}
	return nil
}

// authorizeSessionTouch re-proves the touched session inside the runner
// transaction: the same live-session checks the throttle pre-check applied,
// now authoritative for the renewal write.
func authorizeSessionTouch(ctx context.Context, tx *execution.Tx) error {
	call := stepCallFromContext(ctx)
	if call == nil || len(call.tokenDigest) != 32 {
		return errors.New("auth: session touch inputs are missing from the request context")
	}
	state, err := readSessionActivity(ctx, tx, call.tokenDigest)
	if err != nil {
		return err
	}
	call.userID = state.userID
	return nil
}
