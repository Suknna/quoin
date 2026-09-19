package auth_test

// Deterministic service-level coverage for the admin commands under the
// unique-admin design: ledger replay, row-version fences, the immutable
// administrator, operator-only creation with admin-assigned contacts,
// session revocation semantics and audit projections. Actors log in through
// the full two-step flow — the direct login path is gone.

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/test/support"
)

const fixtureAdminPassword = "Root admin passphrase 2026!"

type adminFixture struct {
	service *auth.Service
	db      *sql.DB
	sender  *recordingSender
	admin   auth.Session
	t       *testing.T
}

func newAuthService(t *testing.T) (*auth.Service, *sql.DB) {
	t.Helper()
	config := testConfig(t)
	if err := support.GenerateDeploymentSecrets(config); err != nil {
		t.Fatalf("bootstrap secrets: %v", err)
	}
	database, err := bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	service, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	// Production installs the read-only pool before serving; the fixture
	// wires its only handle so pure reads run through the same seam.
	if err := service.SetReader(database.Reader); err != nil {
		t.Fatal(err)
	}
	return service, database.SQL
}

// newAdminFixture boots one initialized administrator with an authenticated
// session from the full two-step flow.
func newAdminFixture(t *testing.T) *adminFixture {
	t.Helper()
	service, db := newAuthService(t)
	sender := configureAuth(t, service)
	bootstrapPendingAdmin(t, service)
	initializeAdminDrive(t, service, sender)
	_, session, _ := flowLogin(t, service, sender, "admin", fixtureAdminPassword)
	return &adminFixture{service: service, db: db, sender: sender, admin: session, t: t}
}

func (fixture *adminFixture) login(username, password string) (auth.User, auth.Session, string) {
	fixture.t.Helper()
	return flowLogin(fixture.t, fixture.service, fixture.sender, username, password)
}

func (fixture *adminFixture) createUser(username, displayName, role, password string) auth.User {
	fixture.t.Helper()
	result, _, err := fixture.service.CreateUser(context.Background(), fixture.admin, auth.CreateUserInput{
		ClientCommandID: "cmd-create-" + username, Digest: auth.DigestCommand("user.create", map[string]any{
			"username": username, "displayName": displayName, "role": role,
		}),
		Username: username, DisplayName: displayName, Role: role, Password: password,
		Contacts: []auth.ContactInput{{Channel: "email", Target: username + "@quoin.test"}},
	})
	if err != nil {
		fixture.t.Fatalf("create user %s: %v", username, err)
	}
	return *result.User
}

// completeOperator drives the operator initialization flow to completion so
// the formal password can run the two-step login.
func (fixture *adminFixture) completeOperator(username, tempPassword, formalPassword string) {
	fixture.t.Helper()
	ctx := context.Background()
	flow, _, err := fixture.service.StartOperatorInitialization(ctx, username, tempPassword)
	if err != nil {
		fixture.t.Fatalf("start operator initialization %s: %v", username, err)
	}
	if err := fixture.service.SetFlowPassword(ctx, flow.Bearer, formalPassword); err != nil {
		fixture.t.Fatalf("set operator password: %v", err)
	}
	read, err := fixture.service.ReadFlow(ctx, flow.Bearer)
	if err != nil {
		fixture.t.Fatal(err)
	}
	if _, _, err := fixture.service.SendFlowChallenge(ctx, flow.Bearer, read.Contacts[0].Locator); err != nil {
		fixture.t.Fatalf("send operator challenge: %v", err)
	}
	if err := fixture.service.VerifyFlowChallenge(ctx, flow.Bearer, fixture.sender.code()); err != nil {
		fixture.t.Fatalf("verify operator challenge: %v", err)
	}
	if err := fixture.service.CompleteOperatorInitialization(ctx, flow.Bearer); err != nil {
		fixture.t.Fatalf("complete operator initialization: %v", err)
	}
}

// rowVersionOf reads the authoritative row version of one user.
func (fixture *adminFixture) rowVersionOf(username string) int64 {
	fixture.t.Helper()
	var rowVersion int64
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT row_version FROM users WHERE username=?`, username).Scan(&rowVersion); err != nil {
		fixture.t.Fatalf("row version of %s: %v", username, err)
	}
	return rowVersion
}

func (fixture *adminFixture) displayName(name string) *string { return &name }
func (fixture *adminFixture) enabled(value bool) *bool        { return &value }
func (fixture *adminFixture) role(value string) *string       { return &value }

func TestAdminCreateUserReplayAndConflicts(t *testing.T) {
	fixture := newAdminFixture(t)
	ctx := context.Background()
	input := auth.CreateUserInput{
		ClientCommandID: "cmd-0001", Digest: auth.DigestCommand("user.create", map[string]any{
			"username": "op1", "displayName": "Operator One", "role": "operator",
		}),
		Username: "op1", DisplayName: "Operator One", Role: "operator", Password: "Operator one passphrase 2026!",
		Contacts: []auth.ContactInput{{Channel: "email", Target: "op1@quoin.test"}},
	}
	first, replayed, err := fixture.service.CreateUser(ctx, fixture.admin, input)
	if err != nil || replayed {
		t.Fatalf("create: err=%v replayed=%v", err, replayed)
	}
	if first.User.Initialized || !first.User.PasswordChangeRequired {
		t.Fatalf("created operators start uninitialized: %+v", first.User)
	}
	second, replayed, err := fixture.service.CreateUser(ctx, fixture.admin, input)
	if err != nil || !replayed {
		t.Fatalf("replay must return the original result: err=%v replayed=%v", err, replayed)
	}
	if second.User == nil || second.User.Locator != first.User.Locator {
		t.Fatalf("replay returned a different user: %+v vs %+v", second.User, first.User)
	}
	// Different digest under the same key conflicts.
	input.Digest = auth.DigestCommand("user.create", map[string]any{"username": "op1", "displayName": "Changed", "role": "operator"})
	if _, _, err := fixture.service.CreateUser(ctx, fixture.admin, input); !errors.Is(err, auth.ErrCommandReused) {
		t.Fatalf("different digest must conflict, got %v", err)
	}
	// Duplicate username under a new key is a deterministic rejection.
	duplicate := auth.CreateUserInput{
		ClientCommandID: "cmd-0002", Digest: auth.DigestCommand("user.create", map[string]any{
			"username": "op1", "displayName": "Other", "role": "operator",
		}),
		Username: "op1", DisplayName: "Other", Role: "operator", Password: "Another operator passphrase 2027!",
		Contacts: []auth.ContactInput{{Channel: "email", Target: "op1@quoin.test"}},
	}
	if _, _, err := fixture.service.CreateUser(ctx, fixture.admin, duplicate); !errors.Is(err, auth.ErrUsernameTaken) {
		t.Fatalf("duplicate username must be rejected, got %v", err)
	}
	// And replaying the rejected command returns the same rejection.
	if _, _, err := fixture.service.CreateUser(ctx, fixture.admin, duplicate); !errors.Is(err, auth.ErrUsernameTaken) {
		t.Fatalf("rejected replay must rebuild the same rejection, got %v", err)
	}
	// The unique-admin design removes second administrators entirely.
	adminAttempt := auth.CreateUserInput{
		ClientCommandID: "cmd-0003", Digest: auth.DigestCommand("user.create", map[string]any{
			"username": "root2", "displayName": "Two", "role": "admin",
		}),
		Username: "root2", DisplayName: "Two", Role: "admin", Password: "Second admin passphrase 2026!",
		Contacts: []auth.ContactInput{{Channel: "email", Target: "root2@quoin.test"}},
	}
	if _, _, err := fixture.service.CreateUser(ctx, fixture.admin, adminAttempt); !errors.Is(err, auth.ErrValidation) {
		t.Fatalf("creating a second admin must be rejected, got %v", err)
	}
	// Blocklisted / short passwords never create accounts.
	weak := auth.CreateUserInput{
		ClientCommandID: "cmd-0004", Digest: auth.DigestCommand("user.create", map[string]any{
			"username": "op2", "displayName": "Two", "role": "operator",
		}),
		Username: "op2", DisplayName: "Two", Role: "operator", Password: "short",
		Contacts: []auth.ContactInput{{Channel: "email", Target: "op2@quoin.test"}},
	}
	if _, _, err := fixture.service.CreateUser(ctx, fixture.admin, weak); !errors.Is(err, auth.ErrPasswordPolicy) {
		t.Fatalf("weak password must fail policy, got %v", err)
	}
	// Missing contacts never create accounts.
	contactless := auth.CreateUserInput{
		ClientCommandID: "cmd-0005", Digest: auth.DigestCommand("user.create", map[string]any{
			"username": "op3", "displayName": "Three", "role": "operator",
		}),
		Username: "op3", DisplayName: "Three", Role: "operator", Password: "Operator three passphrase 2026!",
	}
	if _, _, err := fixture.service.CreateUser(ctx, fixture.admin, contactless); !errors.Is(err, auth.ErrValidation) {
		t.Fatalf("operator creation without contacts must be rejected, got %v", err)
	}
}

func TestAdminUpdateUserSecurityChangeRevokesSessions(t *testing.T) {
	fixture := newAdminFixture(t)
	ctx := context.Background()
	created := fixture.createUser("op1", "Operator One", "operator", "Operator one passphrase 2026!")
	fixture.completeOperator("op1", "Operator one passphrase 2026!", "Operator formal passphrase 2027!")
	_, _, opBearer := fixture.login("op1", "Operator formal passphrase 2027!")
	if _, err := fixture.service.Authenticate(ctx, opBearer); err != nil {
		t.Fatalf("operator session must start valid: %v", err)
	}
	expectedRow := fixture.rowVersionOf("op1")
	// DisplayName-only change: row_version advances, auth_revision does not,
	// sessions survive (DATA-AUTH-004/005).
	renamed, _, err := fixture.service.UpdateUser(ctx, fixture.admin, auth.UpdateUserInput{
		ClientCommandID: "cmd-rename", Digest: auth.DigestCommand("user.update", map[string]any{
			"userId": created.ID, "expectedRowVersion": expectedRow, "displayName": "Renamed Operator",
		}),
		UserID: created.ID, ExpectedRow: expectedRow, DisplayName: fixture.displayName("Renamed Operator"),
	})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if renamed.User.RowVersion != expectedRow+1 || renamed.User.AuthRevision != created.AuthRevision+1 {
		t.Fatalf("display-only change must not touch auth_revision: %+v", renamed.User)
	}
	if _, err := fixture.service.Authenticate(ctx, opBearer); err != nil {
		t.Fatalf("display-only change must keep sessions: %v", err)
	}
	// Semantic no-op: same command with the same fields replays idempotently
	// with no UPDATE and no second audit success (HTTP-COMMAND-011).
	noop, _, err := fixture.service.UpdateUser(ctx, fixture.admin, auth.UpdateUserInput{
		ClientCommandID: "cmd-noop", Digest: auth.DigestCommand("user.update", map[string]any{
			"userId": created.ID, "expectedRowVersion": renamed.User.RowVersion, "displayName": "Renamed Operator",
		}),
		UserID: created.ID, ExpectedRow: renamed.User.RowVersion, DisplayName: fixture.displayName("Renamed Operator"),
	})
	if err != nil || noop.User.RowVersion != renamed.User.RowVersion {
		t.Fatalf("no-op must not bump row_version: err=%v %+v", err, noop.User)
	}
	// Stale expected version conflicts with the authoritative value.
	stale, _, err := fixture.service.UpdateUser(ctx, fixture.admin, auth.UpdateUserInput{
		ClientCommandID: "cmd-stale", Digest: auth.DigestCommand("user.update", map[string]any{
			"userId": created.ID, "expectedRowVersion": expectedRow, "displayName": "Stale",
		}),
		UserID: created.ID, ExpectedRow: expectedRow, DisplayName: fixture.displayName("Stale"),
	})
	if !errors.As(err, new(*auth.RowVersionError)) || stale.User != nil {
		t.Fatalf("stale row version must conflict with current value, got %v", err)
	}
	if auth.CurrentRowVersion(err) != renamed.User.RowVersion {
		t.Fatalf("conflict must carry the authoritative row version, got %d", auth.CurrentRowVersion(err))
	}
	// Identity changes are gone with the unique-admin design.
	if _, _, err := fixture.service.UpdateUser(ctx, fixture.admin, auth.UpdateUserInput{
		ClientCommandID: "cmd-promote", Digest: auth.DigestCommand("user.update", map[string]any{
			"userId": created.ID, "expectedRowVersion": renamed.User.RowVersion, "role": "admin",
		}),
		UserID: created.ID, ExpectedRow: renamed.User.RowVersion, Role: fixture.role("admin"),
	}); !errors.Is(err, auth.ErrValidation) {
		t.Fatalf("identity changes must be rejected, got %v", err)
	}
	if _, err := fixture.service.Authenticate(ctx, opBearer); err != nil {
		t.Fatalf("a rejected change must keep sessions: %v", err)
	}
	// Disabling the operator is the remaining security change: sessions die.
	disabled, _, err := fixture.service.UpdateUser(ctx, fixture.admin, auth.UpdateUserInput{
		ClientCommandID: "cmd-disable", Digest: auth.DigestCommand("user.update", map[string]any{
			"userId": created.ID, "expectedRowVersion": renamed.User.RowVersion, "enabled": false,
		}),
		UserID: created.ID, ExpectedRow: renamed.User.RowVersion, Enabled: fixture.enabled(false),
	})
	if err != nil || disabled.User.AuthRevision != renamed.User.AuthRevision+1 {
		t.Fatalf("disable must advance auth_revision once: %+v %v", disabled.User, err)
	}
	if _, err := fixture.service.Authenticate(ctx, opBearer); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("security change must revoke the target's sessions: %v", err)
	}
}

func TestAdminIdentityImmutable(t *testing.T) {
	fixture := newAdminFixture(t)
	ctx := context.Background()
	admin := fixture.admin.User
	// The built-in administrator can never be disabled or demoted; both the
	// domain checks and the schema triggers enforce it.
	if _, _, err := fixture.service.UpdateUser(ctx, fixture.admin, auth.UpdateUserInput{
		ClientCommandID: "cmd-disable-self", Digest: auth.DigestCommand("user.update", map[string]any{
			"userId": admin.ID, "expectedRowVersion": admin.RowVersion, "enabled": false,
		}),
		UserID: admin.ID, ExpectedRow: admin.RowVersion, Enabled: fixture.enabled(false),
	}); !errors.Is(err, auth.ErrLastAdmin) {
		t.Fatalf("disabling the administrator must conflict, got %v", err)
	}
	if _, _, err := fixture.service.UpdateUser(ctx, fixture.admin, auth.UpdateUserInput{
		ClientCommandID: "cmd-demote-self", Digest: auth.DigestCommand("user.update", map[string]any{
			"userId": admin.ID, "expectedRowVersion": admin.RowVersion, "role": "operator",
		}),
		UserID: admin.ID, ExpectedRow: admin.RowVersion, Role: fixture.role("operator"),
	}); !errors.Is(err, auth.ErrValidation) {
		t.Fatalf("demoting the administrator must be rejected, got %v", err)
	}
	// Reset-password is not available for the unique administrator: a reset
	// would flip the admin back to uninitialized, and no online path could
	// complete that re-initialization — returning the deployment's only admin
	// to the initialization flow is the offline `quoin admin recover` channel's
	// decision (design §6), so the online reset is a deterministic rejection —
	// fresh and replayed alike.
	adminReset := auth.ResetPasswordInput{
		ClientCommandID: "cmd-reset-self", Digest: auth.DigestCommand("user.reset_password", map[string]any{
			"userId": admin.ID, "expectedRowVersion": admin.RowVersion, "newPasswordPresent": true,
		}),
		UserID: admin.ID, ExpectedRow: admin.RowVersion, NewPassword: "Fresh admin passphrase 2027!",
	}
	if _, _, err := fixture.service.ResetUserPassword(ctx, fixture.admin, adminReset); !errors.Is(err, auth.ErrValidation) {
		t.Fatalf("resetting the administrator must be rejected as a validation error, got %v", err)
	}
	if _, _, err := fixture.service.ResetUserPassword(ctx, fixture.admin, adminReset); !errors.Is(err, auth.ErrValidation) {
		t.Fatalf("the replayed rejection must rebuild the same outcome, got %v", err)
	}
	// The rejection changed nothing.
	if fixture.rowVersionOf("admin") != admin.RowVersion {
		t.Fatal("a rejected reset must not move the administrator row")
	}
}

// TestAdminConcurrentCreateUniqueUsername proves the serialized uniqueness
// rule now that the single-admin invariant removed the last-admin race:
// concurrent creations of one username resolve to exactly one winner with
// deterministic rejections for everyone else.
func TestAdminConcurrentCreateUniqueUsername(t *testing.T) {
	fixture := newAdminFixture(t)
	ctx := context.Background()
	const attempts = 8
	results := make(chan error, attempts)
	var start sync.WaitGroup
	start.Add(1)
	for i := 0; i < attempts; i++ {
		go func(n int) {
			start.Wait()
			_, _, err := fixture.service.CreateUser(context.Background(), fixture.admin, auth.CreateUserInput{
				ClientCommandID: "cmd-race-" + strconv.Itoa(n),
				Digest: auth.DigestCommand("user.create", map[string]any{
					"username": "raced", "displayName": "Raced Operator", "role": "operator",
				}),
				Username: "raced", DisplayName: "Raced Operator", Role: "operator", Password: "Raced operator passphrase 2026!",
				Contacts: []auth.ContactInput{{Channel: "email", Target: "raced@quoin.test"}},
			})
			results <- err
		}(i)
	}
	start.Done()
	successes, taken, other := 0, 0, 0
	for i := 0; i < attempts; i++ {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, auth.ErrUsernameTaken):
			taken++
		default:
			other++
			t.Errorf("unexpected race error: %v", err)
		}
	}
	if other > 0 || successes != 1 || taken != attempts-1 {
		t.Fatalf("exactly one creator must win: success=%d taken=%d other=%d", successes, taken, other)
	}
	// The surviving administrator still cannot disable themselves.
	var rowVersion int64
	if err := fixture.db.QueryRowContext(ctx, `SELECT row_version FROM users WHERE role='admin'`).Scan(&rowVersion); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.service.UpdateUser(ctx, fixture.admin, auth.UpdateUserInput{
		ClientCommandID: "cmd-final-self-disable", Digest: auth.DigestCommand("user.update", map[string]any{
			"userId": fixture.admin.User.ID, "expectedRowVersion": rowVersion, "enabled": false,
		}),
		UserID: fixture.admin.User.ID, ExpectedRow: rowVersion, Enabled: fixture.enabled(false),
	}); !errors.Is(err, auth.ErrLastAdmin) {
		t.Fatalf("the surviving admin must hit the admin guard, got %v", err)
	}
}

func TestAdminResetPasswordAndSessionRejection(t *testing.T) {
	fixture := newAdminFixture(t)
	ctx := context.Background()
	created := fixture.createUser("op1", "Operator One", "operator", "Operator one passphrase 2026!")
	fixture.completeOperator("op1", "Operator one passphrase 2026!", "Operator formal passphrase 2027!")
	_, _, firstBearer := fixture.login("op1", "Operator formal passphrase 2027!")
	_, _, secondBearer := fixture.login("op1", "Operator formal passphrase 2027!")
	// A pending second-factor flow with an outstanding challenge must die
	// with the credential: no flow may survive into the reset state.
	pending, _, err := fixture.service.StartAuthentication(ctx, "op1", "Operator formal passphrase 2027!", "UA Chrome")
	if err != nil || pending.Type != auth.FlowLogin {
		t.Fatalf("pre-reset login flow: %v %+v", err, pending)
	}
	if _, _, err := fixture.service.SendFlowChallenge(ctx, pending.Bearer, pending.Contacts[0].Locator); err != nil {
		t.Fatalf("pre-reset challenge: %v", err)
	}
	expectedRow := fixture.rowVersionOf("op1")
	result, replayed, err := fixture.service.ResetUserPassword(ctx, fixture.admin, auth.ResetPasswordInput{
		ClientCommandID: "cmd-reset-op", Digest: auth.DigestCommand("user.reset_password", map[string]any{
			"userId": created.ID, "expectedRowVersion": expectedRow, "newPasswordPresent": true,
		}),
		UserID: created.ID, ExpectedRow: expectedRow, NewPassword: "Replacement passphrase 2027!",
	})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if replayed {
		t.Fatal("the first reset must execute, not replay")
	}
	if *result.RevokedSessionCount != 2 {
		t.Fatalf("both sessions must be revoked by the reset, got %d", *result.RevokedSessionCount)
	}
	if !result.User.PasswordChangeRequired || result.User.Initialized {
		t.Fatalf("reset must return the operator to the uninitialized temporary state: %+v", result.User)
	}
	if result.User.AuthRevision != pending.User.AuthRevision+1 || result.User.RowVersion != expectedRow+1 {
		t.Fatalf("reset must advance auth_revision and row_version exactly once: %+v", result.User)
	}
	var initialized, required, activeSessions int
	if err := fixture.db.QueryRowContext(ctx, `SELECT initialized,password_change_required,(SELECT COUNT(*) FROM sessions WHERE user_id=users.id AND revoked_at IS NULL) FROM users WHERE username='op1'`).Scan(&initialized, &required, &activeSessions); err != nil {
		t.Fatal(err)
	}
	if initialized != 0 || required != 1 || activeSessions != 0 {
		t.Fatalf("reset must leave an uninitialized temporary credential with no live session: initialized=%d required=%d active=%d", initialized, required, activeSessions)
	}
	// Replay returns the stored count verbatim — the revocation is not
	// re-executed and the count does not drift.
	replay, replayed, err := fixture.service.ResetUserPassword(ctx, fixture.admin, auth.ResetPasswordInput{
		ClientCommandID: "cmd-reset-op", Digest: auth.DigestCommand("user.reset_password", map[string]any{
			"userId": created.ID, "expectedRowVersion": expectedRow, "newPasswordPresent": true,
		}),
		UserID: created.ID, ExpectedRow: expectedRow, NewPassword: "Ignored replacement 2027!",
	})
	if err != nil || !replayed {
		t.Fatalf("reset replay: err=%v replayed=%v", err, replayed)
	}
	if *replay.RevokedSessionCount != 2 {
		t.Fatalf("replay must return the original revocation count, got %d", *replay.RevokedSessionCount)
	}
	for name, bearer := range map[string]string{"first": firstBearer, "second": secondBearer} {
		if _, err := fixture.service.Authenticate(ctx, bearer); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("%s bearer must be rejected after reset: %v", name, err)
		}
	}
	// The pending login flow is revoked with its outstanding challenge: the
	// bearer no longer resolves and the code can never issue a session.
	if _, err := fixture.service.ReadFlow(ctx, pending.Bearer); !errors.Is(err, auth.ErrFlowInvalid) {
		t.Fatalf("the pre-reset flow must be revoked, got %v", err)
	}
	if _, err := fixture.service.CompleteLogin(ctx, pending.Bearer, fixture.sender.code()); !errors.Is(err, auth.ErrFlowInvalid) {
		t.Fatalf("a revoked flow must never complete a login, got %v", err)
	}
	// The temporary credential routes into the operator initialization flow —
	// never a login (no session exists before initialization completes).
	if _, _, err := fixture.service.StartLogin(ctx, "op1", "Replacement passphrase 2027!", "UA Chrome"); !errors.Is(err, auth.ErrInitializationRequired) {
		t.Fatalf("the temporary credential must not start a login, got %v", err)
	}
	flow, _, err := fixture.service.StartAuthentication(ctx, "op1", "Replacement passphrase 2027!", "UA Chrome")
	if err != nil {
		t.Fatalf("temporary credential must start the initialization flow: %v", err)
	}
	if flow.Type != auth.FlowOperatorInitialize || flow.PasswordSet {
		t.Fatalf("expected a fresh operator initialization flow: %+v", flow)
	}
	if !flow.User.PasswordChangeRequired || flow.User.Initialized {
		t.Fatalf("the flow projection must show the uninitialized temporary state: %+v", flow.User)
	}
	if len(flow.Contacts) == 0 {
		t.Fatal("the assigned factor must survive the reset")
	}
	// Re-initialization: formal password plus the verified assigned factor,
	// then the two-step login with the new password.
	if err := fixture.service.SetFlowPassword(ctx, flow.Bearer, "Reinitialized passphrase 2028!"); err != nil {
		t.Fatalf("set flow password: %v", err)
	}
	if _, _, err := fixture.service.SendFlowChallenge(ctx, flow.Bearer, flow.Contacts[0].Locator); err != nil {
		t.Fatalf("send re-initialization challenge: %v", err)
	}
	if err := fixture.service.VerifyFlowChallenge(ctx, flow.Bearer, fixture.sender.code()); err != nil {
		t.Fatalf("verify re-initialization challenge: %v", err)
	}
	if err := fixture.service.CompleteOperatorInitialization(ctx, flow.Bearer); err != nil {
		t.Fatalf("complete re-initialization: %v", err)
	}
	if _, _, err := fixture.service.StartAuthentication(ctx, "op1", "Replacement passphrase 2027!", "UA Chrome"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("the temporary credential must be gone, got %v", err)
	}
	opUser, session, bearer := fixture.login("op1", "Reinitialized passphrase 2028!")
	if !opUser.Initialized || opUser.PasswordChangeRequired {
		t.Fatalf("the re-initialized operator must be a normal user: %+v", opUser)
	}
	if _, err := fixture.service.Authenticate(ctx, bearer); err != nil || session.User.ID != opUser.ID {
		t.Fatalf("the issued session must be a full workbench session: %v", err)
	}
}

func TestRevokeSessionsAndOwnSessionRules(t *testing.T) {
	fixture := newAdminFixture(t)
	ctx := context.Background()
	created := fixture.createUser("op1", "Operator One", "operator", "Operator one passphrase 2026!")
	fixture.completeOperator("op1", "Operator one passphrase 2026!", "Operator formal passphrase 2027!")
	_, _, firstBearer := fixture.login("op1", "Operator formal passphrase 2027!")
	_, _, secondBearer := fixture.login("op1", "Operator formal passphrase 2027!")
	secondSession, err := fixture.service.Authenticate(ctx, secondBearer)
	if err != nil {
		t.Fatal(err)
	}
	sessions, _, err := fixture.service.ListUserSessions(ctx, secondSession, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	currentCount := 0
	for _, view := range sessions {
		if view.Current {
			currentCount++
		}
	}
	if len(sessions) != 2 || currentCount != 1 {
		t.Fatalf("session list must show two sessions with exactly one current: %+v", sessions)
	}
	// revokeOwnSession refuses the current session and accepts another.
	if _, _, err := fixture.service.RevokeUserSessions(ctx, secondSession, auth.RevokeSessionsInput{
		ClientCommandID: "cmd-revoke-current", Digest: auth.DigestCommand("session.revoke_own", map[string]any{"sessionId": secondSession.ID}),
		SpecificSession: secondSession.ID, OwnScope: true,
	}); err == nil {
		t.Fatal("revoking the current session must fail")
	}
	firstSession, err := fixture.service.Authenticate(ctx, firstBearer)
	if err != nil {
		t.Fatal(err)
	}
	revoked, _, err := fixture.service.RevokeUserSessions(ctx, secondSession, auth.RevokeSessionsInput{
		ClientCommandID: "cmd-revoke-other", Digest: auth.DigestCommand("session.revoke_own", map[string]any{"sessionId": firstSession.ID}),
		SpecificSession: firstSession.ID, OwnScope: true,
	})
	if err != nil || *revoked.RevokedSessionCount != 1 {
		t.Fatalf("revoke other session: err=%v count=%v", err, revoked.RevokedSessionCount)
	}
	if _, err := fixture.service.Authenticate(ctx, firstBearer); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("revoked session must fail subsequent authentication")
	}
	// Admin revoke-sessions clears the rest and stays replay-idempotent.
	adminResult, replayed, err := fixture.service.RevokeUserSessions(ctx, fixture.admin, auth.RevokeSessionsInput{
		ClientCommandID: "cmd-admin-revoke", Digest: auth.DigestCommand("user.revoke_sessions", map[string]any{"userId": created.ID}),
		UserID: created.ID,
	})
	if err != nil || replayed {
		t.Fatalf("admin revoke: err=%v replayed=%v", err, replayed)
	}
	if *adminResult.RevokedSessionCount != 1 {
		t.Fatalf("only the still-active session counts, got %d", *adminResult.RevokedSessionCount)
	}
	if _, err := fixture.service.Authenticate(ctx, secondBearer); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("admin revoke must invalidate the remaining session")
	}
	// A missing target user stays not-found for future commands.
	if _, _, err := fixture.service.RevokeUserSessions(ctx, fixture.admin, auth.RevokeSessionsInput{
		ClientCommandID: "cmd-missing-user", Digest: auth.DigestCommand("user.revoke_sessions", map[string]any{"userId": 99999}),
		UserID: 99999,
	}); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("missing user must be not-found, got %v", err)
	}
}

func TestAuditEventsProjection(t *testing.T) {
	fixture := newAdminFixture(t)
	ctx := context.Background()
	fixture.createUser("op1", "Operator One", "operator", "Operator one passphrase 2026!")
	events, more, err := fixture.service.ListAuditEvents(ctx, auth.AuditEventFilters{Action: "user.create"}, auth.AuditCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if more || len(events) != 1 { // only op1; bootstrap seeds via auth.bootstrap.seed
		t.Fatalf("expected one user.create event, got %d (more=%v)", len(events), more)
	}
	if events[0].Action != "user.create" || events[0].Outcome != "success" {
		t.Fatalf("unexpected event: %+v", events[0])
	}
	// Newest first keyset: the second page is empty after consuming all rows.
	cursor := auth.AuditCursor{ID: parseID(t, events[len(events)-1].ID), CreatedAt: events[len(events)-1].CreatedAt}
	next, more, err := fixture.service.ListAuditEvents(ctx, auth.AuditEventFilters{Action: "user.create"}, cursor, 10)
	if err != nil || more || len(next) != 0 {
		t.Fatalf("second page must be empty: err=%v more=%v items=%d", err, more, len(next))
	}
}

func parseID(t *testing.T, locator string) int64 {
	value, err := strconv.ParseInt(locator, 10, 64)
	if err != nil {
		t.Fatalf("locator %q: %v", locator, err)
	}
	return value
}
