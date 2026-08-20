package auth_test

// Deterministic service-level coverage for the T05 admin commands: ledger
// replay, row-version fences, the last-effective-admin rule under concurrent
// disable/demote, session revocation semantics and audit projections.

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

const fixtureAdminPassword = "Root admin passphrase 2026!"

type adminFixture struct {
	service *auth.Service
	admin   auth.Session
	t       *testing.T
}

func newAuthService(t *testing.T) *auth.Service {
	t.Helper()
	config := testConfig(t)
	if created, err := bootstrap.BootstrapSecrets(config); err != nil || !created {
		t.Fatalf("bootstrap secrets: created=%v err=%v", created, err)
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
	return service
}

func loginOrFail(t *testing.T, service *auth.Service, username, password string) (auth.User, auth.Session, string) {
	t.Helper()
	login, _, err := service.Login(context.Background(), username, password, "Mozilla/5.0 Chrome Linux")
	if err != nil {
		t.Fatalf("login %s: %v", username, err)
	}
	session, err := service.Authenticate(context.Background(), login.Bearer)
	if err != nil {
		t.Fatal(err)
	}
	return login.User, session, login.Bearer
}

// newAdminFixture boots a database with one enabled administrator and
// optionally a second administrator so last-admin changes stay legal, then
// returns the primary admin's authenticated session.
func newAdminFixture(t *testing.T, extraAdmin bool) *adminFixture {
	t.Helper()
	service := newAuthService(t)
	if _, err := service.CreateFirstAdmin(context.Background(), "admin", "Primary Admin", fixtureAdminPassword); err != nil {
		t.Fatalf("first admin: %v", err)
	}
	_, session, _ := loginOrFail(t, service, "admin", fixtureAdminPassword)
	fixture := &adminFixture{service: service, admin: session, t: t}
	if extraAdmin {
		fixture.createUser("backup", "Backup Admin", "admin", "Backup admin passphrase 2026!")
	}
	return fixture
}

func (fixture *adminFixture) createUser(username, displayName, role, password string) auth.User {
	fixture.t.Helper()
	result, _, err := fixture.service.CreateUser(context.Background(), fixture.admin, auth.CreateUserInput{
		ClientCommandID: "cmd-create-" + username, Digest: auth.DigestCommand("user.create", map[string]any{
			"username": username, "displayName": displayName, "role": role,
		}),
		Username: username, DisplayName: displayName, Role: role, Password: password,
	})
	if err != nil {
		fixture.t.Fatalf("create user %s: %v", username, err)
	}
	return *result.User
}

func (fixture *adminFixture) displayName(name string) *string { return &name }
func (fixture *adminFixture) enabled(value bool) *bool        { return &value }
func (fixture *adminFixture) role(value string) *string       { return &value }

func TestAdminCreateUserReplayAndConflicts(t *testing.T) {
	fixture := newAdminFixture(t, false)
	ctx := context.Background()
	input := auth.CreateUserInput{
		ClientCommandID: "cmd-0001", Digest: auth.DigestCommand("user.create", map[string]any{
			"username": "op1", "displayName": "Operator One", "role": "operator",
		}),
		Username: "op1", DisplayName: "Operator One", Role: "operator", Password: "Operator one passphrase 2026!",
	}
	first, replayed, err := fixture.service.CreateUser(ctx, fixture.admin, input)
	if err != nil || replayed {
		t.Fatalf("create: err=%v replayed=%v", err, replayed)
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
	}
	if _, _, err := fixture.service.CreateUser(ctx, fixture.admin, duplicate); !errors.Is(err, auth.ErrUsernameTaken) {
		t.Fatalf("duplicate username must be rejected, got %v", err)
	}
	// And replaying the rejected command returns the same rejection.
	if _, _, err := fixture.service.CreateUser(ctx, fixture.admin, duplicate); !errors.Is(err, auth.ErrUsernameTaken) {
		t.Fatalf("rejected replay must rebuild the same rejection, got %v", err)
	}
	// Blocklisted / short passwords never create accounts.
	weak := auth.CreateUserInput{
		ClientCommandID: "cmd-0003", Digest: auth.DigestCommand("user.create", map[string]any{
			"username": "op2", "displayName": "Two", "role": "operator",
		}),
		Username: "op2", DisplayName: "Two", Role: "operator", Password: "short",
	}
	if _, _, err := fixture.service.CreateUser(ctx, fixture.admin, weak); !errors.Is(err, auth.ErrPasswordPolicy) {
		t.Fatalf("weak password must fail policy, got %v", err)
	}
}

func TestAdminUpdateUserSecurityChangeRevokesSessions(t *testing.T) {
	fixture := newAdminFixture(t, true)
	ctx := context.Background()
	created := fixture.createUser("op1", "Operator One", "operator", "Operator one passphrase 2026!")
	_, _, opBearer := loginOrFail(t, fixture.service, "op1", "Operator one passphrase 2026!")
	if _, err := fixture.service.Authenticate(ctx, opBearer); err != nil {
		t.Fatalf("operator session must start valid: %v", err)
	}
	// DisplayName-only change: row_version advances, auth_revision does not,
	// sessions survive (DATA-AUTH-004/005).
	renamed, _, err := fixture.service.UpdateUser(ctx, fixture.admin, auth.UpdateUserInput{
		ClientCommandID: "cmd-rename", Digest: auth.DigestCommand("user.update", map[string]any{
			"userId": created.ID, "expectedRowVersion": created.RowVersion, "displayName": "Renamed Operator",
		}),
		UserID: created.ID, ExpectedRow: created.RowVersion, DisplayName: fixture.displayName("Renamed Operator"),
	})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if renamed.User.RowVersion != created.RowVersion+1 || renamed.User.AuthRevision != created.AuthRevision {
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
			"userId": created.ID, "expectedRowVersion": created.RowVersion, "displayName": "Stale",
		}),
		UserID: created.ID, ExpectedRow: created.RowVersion, DisplayName: fixture.displayName("Stale"),
	})
	if !errors.As(err, new(*auth.RowVersionError)) || stale.User != nil {
		t.Fatalf("stale row version must conflict with current value, got %v", err)
	}
	if auth.CurrentRowVersion(err) != renamed.User.RowVersion {
		t.Fatalf("conflict must carry the authoritative row version, got %d", auth.CurrentRowVersion(err))
	}
	// Role change is a security change: auth_revision advances once and the
	// operator's session dies on its next authentication.
	promoted, _, err := fixture.service.UpdateUser(ctx, fixture.admin, auth.UpdateUserInput{
		ClientCommandID: "cmd-promote", Digest: auth.DigestCommand("user.update", map[string]any{
			"userId": created.ID, "expectedRowVersion": renamed.User.RowVersion, "role": "admin",
		}),
		UserID: created.ID, ExpectedRow: renamed.User.RowVersion, Role: fixture.role("admin"),
	})
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if promoted.User.AuthRevision != renamed.User.AuthRevision+1 {
		t.Fatalf("security change must advance auth_revision once: %+v", promoted.User)
	}
	if _, err := fixture.service.Authenticate(ctx, opBearer); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("security change must revoke the target's sessions: %v", err)
	}
}

func TestAdminLastEffectiveAdminGuard(t *testing.T) {
	fixture := newAdminFixture(t, false)
	ctx := context.Background()
	admin := fixture.admin.User
	// Single admin: any disable or demote conflicts (DATA-TX-014).
	if _, _, err := fixture.service.UpdateUser(ctx, fixture.admin, auth.UpdateUserInput{
		ClientCommandID: "cmd-disable-self", Digest: auth.DigestCommand("user.update", map[string]any{
			"userId": admin.ID, "expectedRowVersion": admin.RowVersion, "enabled": false,
		}),
		UserID: admin.ID, ExpectedRow: admin.RowVersion, Enabled: fixture.enabled(false),
	}); !errors.Is(err, auth.ErrLastAdmin) {
		t.Fatalf("disabling the last admin must conflict, got %v", err)
	}
	if _, _, err := fixture.service.UpdateUser(ctx, fixture.admin, auth.UpdateUserInput{
		ClientCommandID: "cmd-demote-self", Digest: auth.DigestCommand("user.update", map[string]any{
			"userId": admin.ID, "expectedRowVersion": admin.RowVersion, "role": "operator",
		}),
		UserID: admin.ID, ExpectedRow: admin.RowVersion, Role: fixture.role("operator"),
	}); !errors.Is(err, auth.ErrLastAdmin) {
		t.Fatalf("demoting the last admin must conflict, got %v", err)
	}
	// Reset-password is not a last-admin operation and stays legal.
	if _, _, err := fixture.service.ResetUserPassword(ctx, fixture.admin, auth.ResetPasswordInput{
		ClientCommandID: "cmd-reset-self", Digest: auth.DigestCommand("user.reset_password", map[string]any{
			"userId": admin.ID, "expectedRowVersion": admin.RowVersion, "newPasswordPresent": true,
		}),
		UserID: admin.ID, ExpectedRow: admin.RowVersion, NewPassword: "Fresh admin passphrase 2027!",
	}); err != nil {
		t.Fatalf("self reset: %v", err)
	}
}

// TestAdminConcurrentLastAdminRace proves the in-transaction guard: with two
// enabled administrators, concurrent commands that each disable one of them
// cannot both commit — exactly one disable wins; every other attempt is
// rejected either by the last-admin guard (target is the final remaining
// admin) or by the row-version fence (target already disabled) (DATA-TX-014).
func TestAdminConcurrentLastAdminRace(t *testing.T) {
	fixture := newAdminFixture(t, false)
	ctx := context.Background()
	primary := fixture.admin.User
	backup := fixture.createUser("backup2", "Backup Two", "admin", "Backup two passphrase 2026!")
	_, backupSession, _ := loginOrFail(t, fixture.service, "backup2", "Backup two passphrase 2026!")
	users, _, err := fixture.service.ListUsers(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	rowVersions := map[int64]int64{}
	for _, user := range users {
		rowVersions[user.ID] = user.RowVersion
	}
	const attempts = 8
	var commandCounter int64
	var commandMutex sync.Mutex
	results := make(chan error, attempts*2)
	var start sync.WaitGroup
	start.Add(1)
	attempt := func(session auth.Session, userID int64) {
		start.Wait()
		commandMutex.Lock()
		commandCounter++
		commandID := "cmd-race-" + strconv.FormatInt(commandCounter, 10)
		commandMutex.Unlock()
		_, _, err := fixture.service.UpdateUser(context.Background(), session, auth.UpdateUserInput{
			ClientCommandID: commandID,
			Digest: auth.DigestCommand("user.update", map[string]any{
				"userId": userID, "expectedRowVersion": rowVersions[userID], "enabled": false,
			}),
			UserID: userID, ExpectedRow: rowVersions[userID], Enabled: fixture.enabled(false),
		})
		results <- err
	}
	for i := 0; i < attempts; i++ {
		go attempt(fixture.admin, backup.ID)
		go attempt(backupSession, primary.ID)
	}
	start.Done()
	successes, lastAdminRejects, versionRejects, actorChanged, other := 0, 0, 0, 0, 0
	for i := 0; i < attempts*2; i++ {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, auth.ErrLastAdmin):
			lastAdminRejects++
		case errors.As(err, new(*auth.RowVersionError)):
			versionRejects++
		case errors.Is(err, auth.ErrActorChanged):
			actorChanged++
		default:
			other++
			t.Errorf("unexpected race error: %v", err)
		}
	}
	if other > 0 {
		t.Fatalf("race produced non-deterministic errors: %d", other)
	}
	if successes != 1 {
		t.Fatalf("exactly one disable must win the race, got %d", successes)
	}
	if lastAdminRejects+versionRejects+actorChanged != attempts*2-1 {
		t.Fatalf("every other attempt must be deterministically rejected: lastAdmin=%d version=%d actor=%d", lastAdminRejects, versionRejects, actorChanged)
	}
	users, _, err = fixture.service.ListUsers(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	enabledAdmins := 0
	for _, user := range users {
		if user.Role == "admin" && user.Enabled {
			enabledAdmins++
		}
	}
	if enabledAdmins != 1 {
		t.Fatalf("exactly one effective admin must remain, got %d", enabledAdmins)
	}
	// After the race exactly one effective admin remains: whoever survived
	// still cannot disable or demote themselves (DATA-TX-014 end state).
	var remaining auth.User = fixture.admin.User
	remainingSession := fixture.admin
	for _, user := range users {
		if user.Role == "admin" && user.Enabled && user.ID == backup.ID {
			remaining = user
			remainingSession = backupSession
		}
	}
	if _, _, err := fixture.service.UpdateUser(ctx, remainingSession, auth.UpdateUserInput{
		ClientCommandID: "cmd-final-self-disable", Digest: auth.DigestCommand("user.update", map[string]any{
			"userId": remaining.ID, "expectedRowVersion": remaining.RowVersion, "enabled": false,
		}),
		UserID: remaining.ID, ExpectedRow: remaining.RowVersion, Enabled: fixture.enabled(false),
	}); !errors.Is(err, auth.ErrLastAdmin) {
		t.Fatalf("the surviving admin must hit the last-admin guard, got %v", err)
	}
}

func TestAdminResetPasswordAndSessionRejection(t *testing.T) {
	fixture := newAdminFixture(t, true)
	ctx := context.Background()
	created := fixture.createUser("op1", "Operator One", "operator", "Operator one passphrase 2026!")
	_, _, firstBearer := loginOrFail(t, fixture.service, "op1", "Operator one passphrase 2026!")
	_, _, secondBearer := loginOrFail(t, fixture.service, "op1", "Operator one passphrase 2026!")
	result, _, err := fixture.service.ResetUserPassword(ctx, fixture.admin, auth.ResetPasswordInput{
		ClientCommandID: "cmd-reset-op", Digest: auth.DigestCommand("user.reset_password", map[string]any{
			"userId": created.ID, "expectedRowVersion": created.RowVersion, "newPasswordPresent": true,
		}),
		UserID: created.ID, ExpectedRow: created.RowVersion, NewPassword: "Replacement passphrase 2027!",
	})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if *result.RevokedSessionCount != 2 {
		t.Fatalf("both sessions must be revoked by the reset, got %d", *result.RevokedSessionCount)
	}
	if !result.User.PasswordChangeRequired {
		t.Fatal("reset must set password_change_required")
	}
	for name, bearer := range map[string]string{"first": firstBearer, "second": secondBearer} {
		if _, err := fixture.service.Authenticate(ctx, bearer); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("%s bearer must be rejected after reset: %v", name, err)
		}
	}
	// The temporary password logs into a restricted session (flag visible).
	login, _, err := fixture.service.Login(ctx, "op1", "Replacement passphrase 2027!", "UA Chrome")
	if err != nil {
		t.Fatalf("temporary login: %v", err)
	}
	if !login.User.PasswordChangeRequired {
		t.Fatal("temporary login must report the forced change")
	}
}

func TestRevokeSessionsAndOwnSessionRules(t *testing.T) {
	fixture := newAdminFixture(t, true)
	ctx := context.Background()
	created := fixture.createUser("op1", "Operator One", "operator", "Operator one passphrase 2026!")
	_, _, firstBearer := loginOrFail(t, fixture.service, "op1", "Operator one passphrase 2026!")
	_, _, secondBearer := loginOrFail(t, fixture.service, "op1", "Operator one passphrase 2026!")
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
	// A revoked target user no longer exists for future commands.
	if _, _, err := fixture.service.RevokeUserSessions(ctx, fixture.admin, auth.RevokeSessionsInput{
		ClientCommandID: "cmd-missing-user", Digest: auth.DigestCommand("user.revoke_sessions", map[string]any{"userId": 99999}),
		UserID: 99999,
	}); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("missing user must be not-found, got %v", err)
	}
}

func TestAuditEventsProjection(t *testing.T) {
	fixture := newAdminFixture(t, true)
	ctx := context.Background()
	fixture.createUser("op1", "Operator One", "operator", "Operator one passphrase 2026!")
	events, more, err := fixture.service.ListAuditEvents(ctx, auth.AuditEventFilters{Action: "user.create"}, auth.AuditCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if more || len(events) != 2 { // bootstrap admin + op1
		t.Fatalf("expected two user.create events, got %d (more=%v)", len(events), more)
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
