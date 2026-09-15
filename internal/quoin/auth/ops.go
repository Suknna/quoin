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
	seedBootstrap        *execution.Operation
	adminInitStart       *execution.Operation
	operatorInitStart    *execution.Operation
	loginStart           *execution.Operation
	setFlowPassword      *execution.Operation
	registerFlowContact  *execution.Operation
	stageFlowContact     *execution.Operation
	issueChallenge       *execution.Operation
	verifyChallenge      *execution.Operation
	completeAdminInit    *execution.Operation
	completeOperatorInit *execution.Operation
	completeLogin        *execution.Operation

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
	contactChangeStart   *execution.Operation
	contactChangeDone    *execution.Operation
	challengeDelivery    *execution.Operation
	recoveryBegin        *execution.Operation
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
	return authOperations{
		seedBootstrap: register(execution.Operation{
			Name: "auth.bootstrap.seed", Class: execution.ClassWrite, ObjectType: "deployment",
			Authorize: authorizeBootstrapSeed,
		}),
		adminInitStart: register(execution.Operation{
			Name: "auth.admin_initialize.start", Class: execution.ClassWrite, ObjectType: "user",
			Authorize: authorizeCredentialStart(startClassAdmin),
		}),
		operatorInitStart: register(execution.Operation{
			Name: "auth.operator_initialize.start", Class: execution.ClassWrite, ObjectType: "user",
			Authorize: authorizeCredentialStart(startClassOperator),
		}),
		loginStart: register(execution.Operation{
			Name: "auth.login.start", Class: execution.ClassWrite, ObjectType: "user",
			Authorize: authorizeCredentialStart(startClassLogin),
		}),
		setFlowPassword: register(execution.Operation{
			Name: "auth.flow.set_password", Class: execution.ClassWrite, ObjectType: "user",
			Authorize: authorizeFlowStep(FlowAdminInitialize, FlowOperatorInitialize),
		}),
		registerFlowContact: register(execution.Operation{
			Name: "auth.flow.register_contact", Class: execution.ClassWrite, ObjectType: "user_contact",
			Authorize: authorizeFlowStep(FlowAdminInitialize, FlowContactChange),
		}),
		// The factor-change start stages the replacement on the auth_flows row;
		// no user_contacts row exists yet, so its audit target is the flow.
		stageFlowContact: register(execution.Operation{
			Name: "auth.flow.stage_contact", Class: execution.ClassWrite, ObjectType: "auth_flow",
			Authorize: authorizeFlowStep(FlowContactChange),
		}),
		issueChallenge: register(execution.Operation{
			Name: "auth.challenge.issue", Class: execution.ClassWrite, ObjectType: "auth_flow",
			Authorize: authorizeFlowStep(FlowAdminInitialize, FlowOperatorInitialize, FlowLogin, FlowContactChange),
		}),
		// Verification consumes the challenge but mutates the flow's verified
		// state; the addressed object is the flow (its id is also the recorded
		// failure/rejection object id), never the contact row.
		verifyChallenge: register(execution.Operation{
			Name: "auth.challenge.verify", Class: execution.ClassWrite, ObjectType: "auth_flow",
			Authorize: authorizeFlowStep(FlowAdminInitialize, FlowOperatorInitialize, FlowContactChange),
		}),
		completeAdminInit: register(execution.Operation{
			Name: "auth.admin_initialize.complete", Class: execution.ClassWrite, ObjectType: "user",
			Authorize: authorizeFlowStep(FlowAdminInitialize),
		}),
		completeOperatorInit: register(execution.Operation{
			Name: "auth.operator_initialize.complete", Class: execution.ClassWrite, ObjectType: "user",
			Authorize: authorizeFlowStep(FlowOperatorInitialize),
		}),
		recoveryBegin: register(execution.Operation{
			Name: "auth.recovery.begin", Class: execution.ClassWrite, ObjectType: "user",
			// Recovery is a stopped-service CLI operation: the system principal
			// with the CLI source is the only admissible context. A caller
			// context that already carries HTTP or scheduler metadata can never
			// drive an offline recovery through the service.
			Authorize: authorizeRecoveryBegin,
		}),
		completeLogin: register(execution.Operation{
			Name: "auth.login.complete", Class: execution.ClassWrite, ObjectType: "auth_flow",
			Authorize: authorizeFlowStep(FlowLogin),
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
		contactChangeStart: register(execution.Operation{
			Name: "auth.contact_change.start", Class: execution.ClassWrite, ObjectType: "user",
			Authorize: authorizeCredentialStart(startClassContactChange),
		}),
		// The atomic completion acts on the administrator's account (session
		// revocation, revision binding) and its rejection object id is the
		// user; the swapped user_contacts row has no stable id before the
		// upsert, so the user is the one honest target authority.
		contactChangeDone: register(execution.Operation{
			Name: "auth.contact_change.complete", Class: execution.ClassWrite, ObjectType: "user",
			Authorize: authorizeFlowStep(FlowContactChange),
		}),
		challengeDelivery: register(execution.Operation{
			Name: "auth.challenge.delivery_result", Class: execution.ClassWrite, ObjectType: "auth_challenge",
			Authorize: authorizeDeliveryResult,
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
