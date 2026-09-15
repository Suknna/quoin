package auth_test

// Flow lifecycle coverage on the canonical schema (bootstrap.OpenDatabase
// loads gen.SchemaSQL): deployment bootstrap with the public default
// credential, the admin/operator initialization flows, the two-step login,
// challenge binding invalidation, contact rotation and throttled session
// renewal.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/auth"
)

const (
	fixtureAdminEmail       = "admin@quoin.test"
	fixtureOperatorTempPass = "Operator one passphrase 2026!"
	fixtureOperatorEmail    = "op1@quoin.test"
)

// recordingSender captures deliveries so tests read the issued codes; a set
// fail flag simulates delivery infrastructure failures.
type recordingSender struct {
	mu       sync.Mutex
	messages []auth.Message
	fail     bool
}

func (sender *recordingSender) Send(_ context.Context, message auth.Message) error {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	sender.messages = append(sender.messages, message)
	if sender.fail {
		return errors.New("delivery infrastructure unavailable")
	}
	return nil
}

func (sender *recordingSender) count() int {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	return len(sender.messages)
}

func (sender *recordingSender) last() auth.Message {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.messages) == 0 {
		return auth.Message{}
	}
	return sender.messages[len(sender.messages)-1]
}

func (sender *recordingSender) code() string {
	return sender.last().Variables["code"]
}

func configureAuth(t *testing.T, service *auth.Service) *recordingSender {
	t.Helper()
	sender := &recordingSender{}
	if err := service.ConfigureAuth(auth.AuthConfig{OTPKey: bytes.Repeat([]byte{0x5A}, 32), Sender: sender}); err != nil {
		t.Fatalf("configure auth: %v", err)
	}
	return sender
}

// newFlowService boots the canonical schema with ConfigureAuth installed.
func newFlowService(t *testing.T) (*auth.Service, *recordingSender, *sql.DB) {
	t.Helper()
	service, db := newAuthService(t)
	return service, configureAuth(t, service), db
}

// dbExec runs one test-side SQL statement against the canonical schema.
func dbExec(t *testing.T, db *sql.DB, query string, args ...any) error {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		return err
	}
	return nil
}

// dbQueryScan reads one row into the given destinations.
func dbQueryScan(t *testing.T, db *sql.DB, query string, args []any, dest ...any) error {
	t.Helper()
	return db.QueryRowContext(context.Background(), query, args...).Scan(dest...)
}

// bootstrapPendingAdmin seeds the pending built-in administrator exactly once.
func bootstrapPendingAdmin(t *testing.T, service *auth.Service) {
	t.Helper()
	created, err := service.EnsureBootstrapAdmin(context.Background())
	if err != nil || !created {
		t.Fatalf("bootstrap seed: created=%v err=%v", created, err)
	}
	if created, err := service.EnsureBootstrapAdmin(context.Background()); err != nil || created {
		t.Fatalf("bootstrap seed must be idempotent: created=%v err=%v", created, err)
	}
}

// initializeAdminDrive drives the whole admin initialization flow for the
// pending bootstrap administrator.
func initializeAdminDrive(t *testing.T, service *auth.Service, sender *recordingSender) {
	t.Helper()
	ctx := context.Background()
	flow, _, err := service.StartAdminInitialization(ctx, "admin", "admin")
	if err != nil {
		t.Fatalf("start admin initialization: %v", err)
	}
	if flow.Type != auth.FlowAdminInitialize || flow.PasswordSet {
		t.Fatalf("unexpected initialization flow: %+v", flow)
	}
	if err := service.SetFlowPassword(ctx, flow.Bearer, fixtureAdminPassword); err != nil {
		t.Fatalf("set flow password: %v", err)
	}
	masked, err := service.RegisterFlowContact(ctx, flow.Bearer, "email", fixtureAdminEmail)
	if err != nil {
		t.Fatalf("register flow contact: %v", err)
	}
	if !strings.HasPrefix(masked.MaskedTarget, "a***@") {
		t.Fatalf("unexpected masked target: %+v", masked)
	}
	if _, _, err := service.SendFlowChallenge(ctx, flow.Bearer, masked.Locator); err != nil {
		t.Fatalf("send flow challenge: %v", err)
	}
	if code := sender.code(); len(code) != 6 {
		t.Fatalf("expected a 6-digit code, got %q", code)
	}
	if err := service.VerifyFlowChallenge(ctx, flow.Bearer, sender.code()); err != nil {
		t.Fatalf("verify flow challenge: %v", err)
	}
	if err := service.CompleteAdminInitialization(ctx, flow.Bearer); err != nil {
		t.Fatalf("complete admin initialization: %v", err)
	}
}

// flowLogin performs a full two-step login and returns the session.
func flowLogin(t *testing.T, service *auth.Service, sender *recordingSender, username, password string) (auth.User, auth.Session, string) {
	t.Helper()
	ctx := context.Background()
	flow, _, err := service.StartAuthentication(ctx, username, password, "Mozilla/5.0 Chrome Linux")
	if err != nil {
		t.Fatalf("start authentication %s: %v", username, err)
	}
	if flow.Type != auth.FlowLogin {
		t.Fatalf("expected a login flow, got %q", flow.Type)
	}
	if len(flow.Contacts) == 0 {
		t.Fatal("login flow must expose the assigned contacts")
	}
	masked, _, err := service.SendFlowChallenge(ctx, flow.Bearer, flow.Contacts[0].Locator)
	if err != nil {
		t.Fatalf("send login challenge: %v", err)
	}
	if masked.Locator != flow.Contacts[0].Locator {
		t.Fatalf("masked contact mismatch: %+v", masked)
	}
	result, err := service.CompleteLogin(ctx, flow.Bearer, sender.code())
	if err != nil {
		t.Fatalf("complete login: %v", err)
	}
	session, err := service.Authenticate(ctx, result.Bearer)
	if err != nil {
		t.Fatalf("authenticate completed login: %v", err)
	}
	return result.User, session, result.Bearer
}

func TestBootstrapPendingAdminLifecycle(t *testing.T) {
	service, sender, _ := newFlowService(t)
	ctx := context.Background()
	bootstrapPendingAdmin(t, service)

	pending, err := service.HasPendingBootstrapAdmin(ctx)
	if err != nil || !pending {
		t.Fatalf("pending bootstrap admin expected: %v %v", pending, err)
	}
	initialized, err := service.IsDeploymentInitialized(ctx)
	if err != nil || initialized {
		t.Fatalf("deployment must start uninitialized: %v %v", initialized, err)
	}

	// The public default credentials start the unified initialization flow
	// directly; the direct login path stays dead for everyone.
	started, _, err := service.StartAuthentication(ctx, "admin", "admin", "UA")
	if err != nil {
		t.Fatalf("default credentials must start the initialization flow, got %v", err)
	}
	if started.Type != auth.FlowAdminInitialize {
		t.Fatalf("expected the admin initialization flow, got %q", started.Type)
	}
	if _, _, err := service.StartLogin(ctx, "admin", "admin", "UA"); !errors.Is(err, auth.ErrInitializationRequired) {
		t.Fatalf("default credentials must never start a login, got %v", err)
	}
	if sender.count() != 0 {
		t.Fatal("no delivery may happen before it is requested")
	}

	initializeAdminDrive(t, service, sender)

	initialized, err = service.IsDeploymentInitialized(ctx)
	if err != nil || !initialized {
		t.Fatalf("admin initialization must close the bootstrap window: %v %v", initialized, err)
	}
	pending, err = service.HasPendingBootstrapAdmin(ctx)
	if err != nil || pending {
		t.Fatalf("pending admin must be gone: %v %v", pending, err)
	}
	// The administrator is initialized: initialization can never reopen,
	// whatever the caller presents.
	if _, _, err := service.StartAdminInitialization(ctx, "admin", fixtureAdminPassword); !errors.Is(err, auth.ErrInitializeRejected) {
		t.Fatalf("initialization must be closed after completion, got %v", err)
	}
	if _, _, err := service.StartLogin(ctx, "admin", "admin", "UA"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("the default password must stop verifying, got %v", err)
	}
}

func TestAdminInitializationFlowBindingAndTwoStepLogin(t *testing.T) {
	service, sender, db := newFlowService(t)
	ctx := context.Background()
	bootstrapPendingAdmin(t, service)

	// Weak formal passwords fail policy; nothing else moves.
	flow, _, err := service.StartAdminInitialization(ctx, "admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetFlowPassword(ctx, flow.Bearer, "short"); !errors.Is(err, auth.ErrPasswordPolicy) {
		t.Fatalf("weak password must fail policy, got %v", err)
	}
	read, err := service.ReadFlow(ctx, flow.Bearer)
	if err != nil || read.PasswordSet {
		t.Fatalf("policy failure must not set the password: %+v %v", read, err)
	}
	if read.CorrelationID == "" || read.ExpiresAt == "" {
		t.Fatalf("flow projection must carry correlation and expiry: %+v", read)
	}
	if err := service.SetFlowPassword(ctx, flow.Bearer, fixtureAdminPassword); err != nil {
		t.Fatalf("set flow password: %v", err)
	}

	// A second concurrent initialization flow becomes the single active one:
	// the superseded bearer dies immediately and only the new flow can
	// finish. Its password step starts from zero again.
	secondFormal := "Second formal passphrase 2027!"
	second, _, err := service.StartAdminInitialization(ctx, "admin", fixtureAdminPassword)
	if err != nil {
		t.Fatalf("resume with the new password must work: %v", err)
	}
	if _, err := service.ReadFlow(ctx, flow.Bearer); !errors.Is(err, auth.ErrFlowInvalid) {
		t.Fatalf("the superseded flow must be invalid, got %v", err)
	}
	if err := service.SetFlowPassword(ctx, flow.Bearer, "Another fine passphrase 2027!"); !errors.Is(err, auth.ErrFlowInvalid) {
		t.Fatalf("mutating a revoked flow must fail, got %v", err)
	}
	if err := service.SetFlowPassword(ctx, second.Bearer, secondFormal); err != nil {
		t.Fatalf("the fresh flow must set its own password: %v", err)
	}
	flow = second
	secondCorrelation := flow.CorrelationID

	masked, err := service.RegisterFlowContact(ctx, flow.Bearer, "email", fixtureAdminEmail)
	if err != nil {
		t.Fatal(err)
	}
	// Resend cooldown: an immediate second send is deferred with retryAfter.
	if _, _, err := service.SendFlowChallenge(ctx, flow.Bearer, masked.Locator); err != nil {
		t.Fatal(err)
	}
	firstCode := sender.code()
	if _, retryAfter, err := service.SendFlowChallenge(ctx, flow.Bearer, masked.Locator); err == nil || retryAfter <= 0 {
		t.Fatalf("immediate resend must be rate limited with a retry window, got %v %v", retryAfter, err)
	}
	// After the cooldown window the resend supersedes the previous code.
	if err := dbExec(t, db, `UPDATE auth_challenges SET created_at=? WHERE id=(SELECT MAX(id) FROM auth_challenges)`, time.Now().UTC().Add(-31*time.Second).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.SendFlowChallenge(ctx, flow.Bearer, masked.Locator); err != nil {
		t.Fatal(err)
	}
	supersededCode := firstCode
	activeCode := sender.code()
	if activeCode == supersededCode {
		t.Fatal("the resend must issue a fresh code")
	}
	if err := service.VerifyFlowChallenge(ctx, flow.Bearer, supersededCode); !errors.Is(err, auth.ErrOtpInvalid) {
		t.Fatalf("the superseded code must not verify, got %v", err)
	}
	if err := service.VerifyFlowChallenge(ctx, flow.Bearer, activeCode); err != nil {
		t.Fatalf("the active code must verify: %v", err)
	}
	if err := service.CompleteAdminInitialization(ctx, flow.Bearer); err != nil {
		t.Fatal(err)
	}
	// Completed flows are dead.
	if _, err := service.ReadFlow(ctx, flow.Bearer); !errors.Is(err, auth.ErrFlowInvalid) {
		t.Fatalf("completed flow must be invalid, got %v", err)
	}
	// Correlation continuity: the start operation's audit row carries the
	// same correlation the flow exposes for its child steps.
	var startCorrelation string
	if err := db.QueryRowContext(ctx, `SELECT correlation_id FROM audit_events WHERE action='auth.admin_initialize.start' ORDER BY id DESC LIMIT 1`).Scan(&startCorrelation); err != nil {
		t.Fatal(err)
	}
	if startCorrelation == "" || startCorrelation != secondCorrelation {
		t.Fatalf("flow correlation must inherit the start operation correlation: start=%s flow=%s", startCorrelation, secondCorrelation)
	}

	// Two-step login: password success issues no session. The login flow is
	// created with password_set=1 (the password step belongs exclusively to
	// initialization-style flows) and factorVerified=false until the OTP.
	flowLogin1, _, err := service.StartAuthentication(ctx, "admin", secondFormal, "Mozilla/5.0 Chrome Linux")
	if err != nil {
		t.Fatal(err)
	}
	if flowLogin1.Type != auth.FlowLogin {
		t.Fatalf("expected login flow, got %q", flowLogin1.Type)
	}
	if !flowLogin1.PasswordSet || flowLogin1.FactorVerified {
		t.Fatalf("login flow must carry password_set=1 and no factor marker: %+v", flowLogin1)
	}
	var persistedPasswordSet int
	if err := db.QueryRowContext(ctx, `SELECT password_set FROM auth_flows ORDER BY id DESC LIMIT 1`).Scan(&persistedPasswordSet); err != nil {
		t.Fatal(err)
	}
	if persistedPasswordSet != 1 {
		t.Fatalf("login flow creation must persist password_set=1, got %d", persistedPasswordSet)
	}
	sessionBefore, err := service.Authenticate(ctx, "not-a-real-bearer-value-0000000000000000")
	if err == nil {
		t.Fatal("garbage bearer must not authenticate")
	}
	_ = sessionBefore
	maskedLogin, _, err := service.SendFlowChallenge(ctx, flowLogin1.Bearer, flowLogin1.Contacts[0].Locator)
	if err != nil {
		t.Fatal(err)
	}
	if sender.last().Template != "login_verification" {
		t.Fatalf("second-factor template mismatch: %+v", sender.last())
	}
	if sender.last().Variables["expires_in_seconds"] != "300" {
		t.Fatalf("delivery variables must match the design contract: %+v", sender.last())
	}
	_ = maskedLogin
	if _, err := service.CompleteLogin(ctx, flowLogin1.Bearer, "000000"); !errors.Is(err, auth.ErrOtpInvalid) {
		t.Fatalf("a wrong code must not create a session, got %v", err)
	}
	result, err := service.CompleteLogin(ctx, flowLogin1.Bearer, sender.code())
	if err != nil {
		t.Fatalf("complete login: %v", err)
	}
	if _, err := service.Authenticate(ctx, result.Bearer); err != nil {
		t.Fatalf("the issued session must authenticate: %v", err)
	}
	// The flow bearer is not a session credential.
	if _, err := service.Authenticate(ctx, flowLogin1.Bearer); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("flow bearers must never authenticate as sessions, got %v", err)
	}
	// Single consumption: the login flow cannot issue a second session.
	if _, err := service.CompleteLogin(ctx, flowLogin1.Bearer, sender.code()); !errors.Is(err, auth.ErrFlowInvalid) {
		t.Fatalf("a consumed login flow must be dead, got %v", err)
	}
}

func TestChallengeAttemptCapAndOperatorLifecycle(t *testing.T) {
	service, sender, db := newFlowService(t)
	ctx := context.Background()
	bootstrapPendingAdmin(t, service)
	initializeAdminDrive(t, service, sender)
	adminUser, adminSession, _ := flowLogin(t, service, sender, "admin", fixtureAdminPassword)

	// Only operators can be created; contacts are mandatory and
	// password_change_required marks the temporary credential.
	if _, _, err := service.CreateUser(ctx, adminSession, auth.CreateUserInput{
		ClientCommandID: "cmd-create-admin", Digest: auth.DigestCommand("user.create", map[string]any{"username": "root2", "role": "admin"}),
		Username: "root2", DisplayName: "Second Admin", Role: "admin", Password: "Second admin passphrase 2026!",
		Contacts: []auth.ContactInput{{Channel: "email", Target: "root2@quoin.test"}},
	}); !errors.Is(err, auth.ErrValidation) {
		t.Fatalf("creating a second admin must be rejected, got %v", err)
	}
	created, _, err := service.CreateUser(ctx, adminSession, auth.CreateUserInput{
		ClientCommandID: "cmd-create-op1", Digest: auth.DigestCommand("user.create", map[string]any{"username": "op1", "role": "operator"}),
		Username: "op1", DisplayName: "Operator One", Role: "operator", Password: fixtureOperatorTempPass,
		Contacts: []auth.ContactInput{{Channel: "email", Target: fixtureOperatorEmail}},
	})
	if err != nil {
		t.Fatalf("create operator: %v", err)
	}
	if created.User.Initialized || !created.User.PasswordChangeRequired {
		t.Fatalf("created operators start uninitialized with a temporary credential: %+v", created.User)
	}

	// Failed verifications accumulate across resends and kill the flow.
	opFlow, _, err := service.StartOperatorInitialization(ctx, "op1", fixtureOperatorTempPass)
	if err != nil {
		t.Fatalf("start operator initialization: %v", err)
	}
	if _, err := service.RegisterFlowContact(ctx, opFlow.Bearer, "email", "attacker@quoin.test"); !errors.Is(err, auth.ErrFlowInvalid) {
		t.Fatalf("operators must never reassign their targets, got %v", err)
	}
	contacts, err := service.ReadFlow(ctx, opFlow.Bearer)
	if err != nil || len(contacts.Contacts) != 1 {
		t.Fatalf("the assigned contact must be visible: %+v %v", contacts, err)
	}
	if _, _, err := service.SendFlowChallenge(ctx, opFlow.Bearer, contacts.Contacts[0].Locator); err != nil {
		t.Fatal(err)
	}
	if err := service.SetFlowPassword(ctx, opFlow.Bearer, fixtureOperatorFormalPassword(t)); err != nil {
		t.Fatalf("set operator password: %v", err)
	}
	for attempt := 0; attempt < 5; attempt++ {
		if err := service.VerifyFlowChallenge(ctx, opFlow.Bearer, fmt.Sprintf("%06d", attempt)); !errors.Is(err, auth.ErrOtpInvalid) {
			t.Fatalf("wrong code %d must be rejected, got %v", attempt, err)
		}
	}
	if _, err := service.ReadFlow(ctx, opFlow.Bearer); !errors.Is(err, auth.ErrFlowInvalid) {
		t.Fatalf("the flow must be failed after the attempt cap, got %v", err)
	}

	// A fresh flow completes the initialization. The first flow already
	// replaced the temporary credential, so the formal password starts it.
	opFlow, _, err = service.StartOperatorInitialization(ctx, "op1", fixtureOperatorFormalPassword(t))
	if err != nil {
		t.Fatal(err)
	}
	read, err := service.ReadFlow(ctx, opFlow.Bearer)
	if err != nil || read.PasswordSet {
		// password_set does not carry over between flows: the fresh flow
		// starts with the password step unset.
		t.Fatalf("fresh flow must start with the password step unset: %+v %v", read, err)
	}
	if err := service.SetFlowPassword(ctx, opFlow.Bearer, fixtureOperatorFormalPassword(t)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.SendFlowChallenge(ctx, opFlow.Bearer, read.Contacts[0].Locator); err != nil {
		t.Fatal(err)
	}
	if err := service.VerifyFlowChallenge(ctx, opFlow.Bearer, sender.code()); err != nil {
		t.Fatal(err)
	}
	if err := service.CompleteOperatorInitialization(ctx, opFlow.Bearer); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.StartOperatorInitialization(ctx, "op1", fixtureOperatorFormalPassword(t)); !errors.Is(err, auth.ErrInitializeRejected) {
		t.Fatalf("an initialized operator cannot restart initialization, got %v", err)
	}
	// The formal password works through the two-step login; the temporary
	// credential is gone.
	if _, _, err := service.StartLogin(ctx, "op1", fixtureOperatorTempPass, "UA"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("the temporary credential must stop verifying, got %v", err)
	}
	opUser, _, opBearer := flowLogin(t, service, sender, "op1", fixtureOperatorFormalPassword(t))
	if opUser.Role != "operator" || !opUser.Initialized {
		t.Fatalf("unexpected operator projection: %+v", opUser)
	}
	_ = adminUser

	// Contact rotation revokes sessions and pending flows, un-verifies the
	// target and reverts the user to uninitialized (last verified gone).
	var liveRowVersion int64
	if err := db.QueryRowContext(ctx, `SELECT row_version FROM users WHERE id=?`, created.User.ID).Scan(&liveRowVersion); err != nil {
		t.Fatal(err)
	}
	rotated, _, err := service.SetUserContacts(ctx, adminSession, auth.SetUserContactsInput{
		ClientCommandID: "cmd-rotate-op1", Digest: auth.DigestCommand("user.set_contacts", map[string]any{"userId": created.User.ID, "expectedRowVersion": liveRowVersion}),
		UserID: created.User.ID, ExpectedRow: liveRowVersion,
		Contacts: []auth.ContactInput{{Channel: "email", Target: "op1-rotated@quoin.test"}},
	})
	if err != nil {
		t.Fatalf("rotate contacts: %v", err)
	}
	if rotated.User == nil || rotated.User.RowVersion != liveRowVersion+1 {
		t.Fatalf("rotation must advance the row version: %+v", rotated.User)
	}
	if _, err := service.Authenticate(ctx, opBearer); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("contact rotation must revoke sessions, got %v", err)
	}
	var verified, initializedFlag int
	if err := dbQueryScan(t, db, `SELECT COUNT(*) FROM user_contacts WHERE user_id=? AND verified_at IS NOT NULL`, []any{created.User.ID}, &verified); err != nil {
		t.Fatal(err)
	}
	if err := dbQueryScan(t, db, `SELECT initialized FROM users WHERE id=?`, []any{created.User.ID}, &initializedFlag); err != nil {
		t.Fatal(err)
	}
	if verified != 0 || initializedFlag != 0 {
		t.Fatalf("rotation must unverify the target and revert initialization: verified=%d initialized=%d", verified, initializedFlag)
	}
	// Re-initialization on the rotated target completes the cycle.
	opFlow, _, err = service.StartOperatorInitialization(ctx, "op1", fixtureOperatorFormalPassword(t))
	if err != nil {
		t.Fatalf("re-initialization must start with the formal password: %v", err)
	}
	read, err = service.ReadFlow(ctx, opFlow.Bearer)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetFlowPassword(ctx, opFlow.Bearer, fixtureOperatorFormalPassword(t)+"x"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.SendFlowChallenge(ctx, opFlow.Bearer, read.Contacts[0].Locator); err != nil {
		t.Fatal(err)
	}
	if err := service.VerifyFlowChallenge(ctx, opFlow.Bearer, sender.code()); err != nil {
		t.Fatal(err)
	}
	if err := service.CompleteOperatorInitialization(ctx, opFlow.Bearer); err != nil {
		t.Fatal(err)
	}
}

func TestSetUserContactsGuardsAndFences(t *testing.T) {
	service, sender, _ := newFlowService(t)
	ctx := context.Background()
	bootstrapPendingAdmin(t, service)
	initializeAdminDrive(t, service, sender)
	_, adminSession, _ := flowLogin(t, service, sender, "admin", fixtureAdminPassword)
	admin := adminSession.User

	if _, _, err := service.SetUserContacts(ctx, adminSession, auth.SetUserContactsInput{
		ClientCommandID: "cmd-admin-contacts", Digest: auth.DigestCommand("user.set_contacts", map[string]any{"userId": admin.ID}),
		UserID: admin.ID, ExpectedRow: admin.RowVersion,
		Contacts: []auth.ContactInput{{Channel: "email", Target: "new-admin@quoin.test"}},
	}); !errors.Is(err, auth.ErrValidation) {
		t.Fatalf("admin targets must go through the safe flow, got %v", err)
	}
	// Row-version fencing applies to operator targets: a stale expected row
	// conflicts with the authoritative value.
	operator, _, err := service.CreateUser(ctx, adminSession, auth.CreateUserInput{
		ClientCommandID: "cmd-create-op9", Digest: auth.DigestCommand("user.create", map[string]any{"username": "op9", "role": "operator"}),
		Username: "op9", DisplayName: "Operator Nine", Role: "operator", Password: "Operator nine passphrase 2026!",
		Contacts: []auth.ContactInput{{Channel: "email", Target: "op9@quoin.test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.SetUserContacts(ctx, adminSession, auth.SetUserContactsInput{
		ClientCommandID: "cmd-stale-contacts", Digest: auth.DigestCommand("user.set_contacts", map[string]any{"userId": operator.User.ID, "expectedRowVersion": operator.User.RowVersion + 99}),
		UserID: operator.User.ID, ExpectedRow: operator.User.RowVersion + 99,
		Contacts: []auth.ContactInput{{Channel: "email", Target: "op9-rotated@quoin.test"}},
	}); !errors.As(err, new(*auth.RowVersionError)) {
		t.Fatalf("stale expected row must conflict, got %v", err)
	}
	// Replay returns the stored outcome; a different digest under the same
	// command key conflicts.
	input := auth.SetUserContactsInput{
		ClientCommandID: "cmd-replay-contacts", Digest: auth.DigestCommand("user.set_contacts", map[string]any{"userId": 99999}),
		UserID: 99999, ExpectedRow: 1,
		Contacts: []auth.ContactInput{{Channel: "email", Target: "missing@quoin.test"}},
	}
	if _, _, err := service.SetUserContacts(ctx, adminSession, input); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("missing target must be not-found, got %v", err)
	}
	if _, _, err := service.SetUserContacts(ctx, adminSession, input); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("the replayed rejection must rebuild the same outcome, got %v", err)
	}
	_ = sender
}

func TestDeliveryFailureNeverDegrades(t *testing.T) {
	service, sender, db := newFlowService(t)
	ctx := context.Background()
	bootstrapPendingAdmin(t, service)
	initializeAdminDrive(t, service, sender)
	_, _, _ = flowLogin(t, service, sender, "admin", fixtureAdminPassword)

	flow, _, err := service.StartLogin(ctx, "admin", fixtureAdminPassword, "UA")
	if err != nil {
		t.Fatal(err)
	}
	sender.fail = true
	if _, _, err := service.SendFlowChallenge(ctx, flow.Bearer, flow.Contacts[0].Locator); err == nil {
		t.Fatal("a failing sender must surface an error")
	}
	sender.fail = false
	// The code exists server-side but was never accepted for delivery; it
	// must not verify.
	if _, err := service.CompleteLogin(ctx, flow.Bearer, sender.code()); !errors.Is(err, auth.ErrOtpInvalid) {
		t.Fatalf("a code with unaccepted delivery must not verify, got %v", err)
	}
	// After the cooldown a retry delivers and completes normally.
	if err := dbExec(t, db, `UPDATE auth_challenges SET created_at=? WHERE id=(SELECT MAX(id) FROM auth_challenges)`, time.Now().UTC().Add(-31*time.Second).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.SendFlowChallenge(ctx, flow.Bearer, flow.Contacts[0].Locator); err != nil {
		t.Fatalf("retry after the cooldown must deliver: %v", err)
	}
	if _, err := service.CompleteLogin(ctx, flow.Bearer, sender.code()); err != nil {
		t.Fatalf("delivery recovery must complete the login: %v", err)
	}
	// Both delivery outcomes were audited under the flow's correlation with
	// the task source, and the verification code never reached the audit.
	var flowCorrelation string
	if err := db.QueryRowContext(ctx, `SELECT correlation_id FROM auth_flows ORDER BY id DESC LIMIT 1`).Scan(&flowCorrelation); err != nil {
		t.Fatal(err)
	}
	var acceptedAudit, unknownAudit, leaked int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE action='auth.challenge.delivery_result' AND outcome='success' AND correlation_id=?`, flowCorrelation).Scan(&acceptedAudit); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE action='auth.challenge.delivery_result' AND outcome='unknown' AND correlation_id=?`, flowCorrelation).Scan(&unknownAudit); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE action LIKE '%`+sender.code()+`%'`).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	// Two sends on this flow: the failing one recorded unknown (failure
	// audit), the retry accepted (success audit) — each under the flow's
	// correlation, none leaking the code.
	var accepted, unknown int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_challenges WHERE flow_id=(SELECT MAX(id) FROM auth_flows WHERE flow_type='login') AND delivery_status='accepted'`).Scan(&accepted); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_challenges WHERE flow_id=(SELECT MAX(id) FROM auth_flows WHERE flow_type='login') AND delivery_status='unknown'`).Scan(&unknown); err != nil {
		t.Fatal(err)
	}
	if acceptedAudit != 1 || unknownAudit != 1 || leaked != 0 {
		t.Fatalf("delivery outcomes must be audited distinctly under the flow correlation: success=%d unknown=%d leaked=%d", acceptedAudit, unknownAudit, leaked)
	}
	if accepted != 1 || unknown != 1 {
		t.Fatalf("delivery outcomes must be persisted per challenge: accepted=%d unknown=%d", accepted, unknown)
	}
}

func TestTouchSessionActivityRenewal(t *testing.T) {
	service, sender, db := newFlowService(t)
	ctx := context.Background()
	bootstrapPendingAdmin(t, service)
	initializeAdminDrive(t, service, sender)
	_, session, _ := flowLogin(t, service, sender, "admin", fixtureAdminPassword)

	insertSession := func(bearerRaw []byte, lastActive time.Time, idle time.Time, absolute time.Time) string {
		t.Helper()
		digest := sha256.Sum256(bearerRaw)
		if _, err := db.ExecContext(ctx, `INSERT INTO sessions(user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(?,?,?,?,?,?,?,?)`,
			session.User.ID, digest[:], session.User.AuthRevision, "Test on device",
			lastActive.Format(time.RFC3339Nano), lastActive.Format(time.RFC3339Nano),
			idle.Format(time.RFC3339Nano), absolute.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(bearerRaw)
	}
	lastActiveOf := func(bearer string) time.Time {
		t.Helper()
		raw, err := base64.RawURLEncoding.DecodeString(bearer)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(raw)
		var stamp string
		if err := db.QueryRowContext(ctx, `SELECT last_active_at FROM sessions WHERE session_token_digest=?`, digest[:]).Scan(&stamp); err != nil {
			t.Fatal(err)
		}
		value, _ := time.Parse(time.RFC3339Nano, stamp)
		return value
	}
	now := time.Now().UTC()

	// An activity window last touched two hours ago is due for renewal.
	staleBearer := insertSession(bytes.Repeat([]byte{0x21}, 32), now.Add(-2*time.Hour), now.Add(10*time.Hour), now.Add(7*24*time.Hour))
	if err := service.TouchSessionActivity(ctx, staleBearer); err != nil {
		t.Fatalf("touch must succeed: %v", err)
	}
	if renewed := lastActiveOf(staleBearer); renewed.Before(now.Add(-time.Minute)) {
		t.Fatalf("last activity must advance: %s", renewed)
	}
	// Within the throttle window nothing moves.
	unchanged := lastActiveOf(staleBearer)
	if err := service.TouchSessionActivity(ctx, staleBearer); err != nil {
		t.Fatalf("second touch must succeed: %v", err)
	}
	if lastActiveOf(staleBearer) != unchanged {
		t.Fatal("the throttle must not rewrite activity on every call")
	}

	// Renewal never passes the absolute cap: a still-valid idle window that
	// would renew past the absolute expiry gets capped at it.
	cappedBearer := insertSession(bytes.Repeat([]byte{0x22}, 32), now.Add(-time.Hour), now.Add(time.Hour), now.Add(2*time.Hour))
	if err := service.TouchSessionActivity(ctx, cappedBearer); err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(cappedBearer)
	digest := sha256.Sum256(raw)
	var idle, absolute string
	if err := db.QueryRowContext(ctx, `SELECT idle_expires_at,absolute_expires_at FROM sessions WHERE session_token_digest=?`, digest[:]).Scan(&idle, &absolute); err != nil {
		t.Fatal(err)
	}
	if idle != absolute {
		t.Fatalf("renewal must cap at the absolute expiry: idle=%s absolute=%s", idle, absolute)
	}

	// Expired and revoked sessions never renew.
	expiredBearer := insertSession(bytes.Repeat([]byte{0x23}, 32), now.Add(-2*time.Hour), now.Add(-time.Hour), now.Add(7*24*time.Hour))
	if err := service.TouchSessionActivity(ctx, expiredBearer); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("an expired session must not renew, got %v", err)
	}
	revokedBearer := insertSession(bytes.Repeat([]byte{0x24}, 32), now.Add(-2*time.Hour), now.Add(10*time.Hour), now.Add(7*24*time.Hour))
	if _, err := db.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE session_token_digest=?`, now.Format(time.RFC3339Nano), mustDigest(t, revokedBearer)); err != nil {
		t.Fatal(err)
	}
	if err := service.TouchSessionActivity(ctx, revokedBearer); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("a revoked session must not renew, got %v", err)
	}
}

func mustDigest(t *testing.T, bearer string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(bearer)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	return digest[:]
}

func TestAdminSelfContactChangeFlow(t *testing.T) {
	service, sender, db := newFlowService(t)
	ctx := context.Background()
	bootstrapPendingAdmin(t, service)
	initializeAdminDrive(t, service, sender)
	_, adminSession, adminBearer := flowLogin(t, service, sender, "admin", fixtureAdminPassword)

	// Password-only start is impossible: the fully authenticated session is
	// part of the signature, and a stale or missing session is rejected.
	if _, _, err := service.StartContactChange(ctx, auth.Session{}, fixtureAdminPassword); err == nil {
		t.Fatal("a session-less start must be impossible")
	}
	flow, _, err := service.StartContactChange(ctx, adminSession, fixtureAdminPassword)
	if err != nil {
		t.Fatalf("start contact change: %v", err)
	}
	if flow.Type != auth.FlowContactChange {
		t.Fatalf("expected contact_change flow, got %q", flow.Type)
	}
	// The staged candidate is flow state; the live contact row is untouched.
	masked, err := service.RegisterFlowContact(ctx, flow.Bearer, "email", "admin-rotated@quoin.test")
	if err != nil {
		t.Fatalf("register replacement: %v", err)
	}
	if masked.Verified || !strings.HasPrefix(masked.MaskedTarget, "a***@") {
		t.Fatalf("candidate must start unverified: %+v", masked)
	}
	var liveTarget string
	if err := db.QueryRowContext(ctx, `SELECT target FROM user_contacts WHERE user_id=? AND channel='email'`, adminSession.User.ID).Scan(&liveTarget); err != nil {
		t.Fatal(err)
	}
	if liveTarget != fixtureAdminEmail {
		t.Fatalf("the live target must survive staging: %s", liveTarget)
	}
	// Completing before the candidate is verified fails without touching
	// anything.
	if err := service.CompleteContactChange(ctx, adminSession, flow.Bearer, fixtureAdminPassword); err == nil {
		t.Fatal("premature completion must be rejected")
	}
	if _, _, err := service.SendFlowChallenge(ctx, flow.Bearer, "email"); err != nil {
		t.Fatal(err)
	}
	if sender.last().Template != "contact_verification" || sender.last().Recipient != "admin-rotated@quoin.test" {
		t.Fatalf("factor change delivery must go to the staged candidate: %+v", sender.last())
	}
	// A wrong code leaves the live target intact and the flow alive.
	if err := service.VerifyFlowChallenge(ctx, flow.Bearer, "000000"); !errors.Is(err, auth.ErrOtpInvalid) {
		t.Fatalf("wrong code must be rejected, got %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT target FROM user_contacts WHERE user_id=? AND channel='email'`, adminSession.User.ID).Scan(&liveTarget); err != nil {
		t.Fatal(err)
	}
	if liveTarget != fixtureAdminEmail {
		t.Fatalf("a failed verification must never move the live target: %s", liveTarget)
	}
	if err := service.VerifyFlowChallenge(ctx, flow.Bearer, sender.code()); err != nil {
		t.Fatal(err)
	}
	// Completion re-proves the password and atomically swaps the target,
	// revoking every session of the administrator.
	if err := service.CompleteContactChange(ctx, adminSession, flow.Bearer, "wrong password attempt!"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("completion without the current password must fail, got %v", err)
	}
	if err := service.CompleteContactChange(ctx, adminSession, flow.Bearer, fixtureAdminPassword); err != nil {
		t.Fatalf("complete contact change: %v", err)
	}
	// The swap happened, verified, enabled.
	var verified int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_contacts WHERE target='admin-rotated@quoin.test' AND verified_at IS NOT NULL AND enabled=1`).Scan(&verified); err != nil {
		t.Fatal(err)
	}
	if verified != 1 {
		t.Fatalf("the replacement must be verified and enabled: %d", verified)
	}
	// Every old session is gone: the caller must log in again.
	if _, err := service.Authenticate(ctx, adminBearer); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("factor change must revoke the old sessions, got %v", err)
	}
	// The rotated target receives the second factor of the fresh login.
	_, _, newBearer := flowLogin(t, service, sender, "admin", fixtureAdminPassword)
	if _, err := service.Authenticate(ctx, newBearer); err != nil {
		t.Fatalf("login on the rotated target must work: %v", err)
	}
}

func TestReadPoolSeamServesPureReads(t *testing.T) {
	service, sender, db := newFlowService(t)
	ctx := context.Background()
	bootstrapPendingAdmin(t, service)
	initializeAdminDrive(t, service, sender)
	// The fixture already installed the read-only pool (the real bootstrap
	// Reader, never the writer): reads and the full two-step login serve
	// through it while mutations stay on the writer database.
	_ = db
	if err := service.SetReader(nil); err == nil {
		t.Fatal("a nil reader must be rejected")
	}
	_, session, bearer := flowLogin(t, service, sender, "admin", fixtureAdminPassword)
	if session.User.Username != "admin" {
		t.Fatalf("unexpected session through the read pool: %+v", session.User)
	}
	users, _, err := service.ListUsers(ctx, 0, 10)
	if err != nil || len(users) == 0 {
		t.Fatalf("list through the read pool: %v", err)
	}
	initialized, err := service.IsDeploymentInitialized(ctx)
	if err != nil || !initialized {
		t.Fatalf("deployment state through the read pool: %v %v", initialized, err)
	}
	if _, err := service.ReadFlow(ctx, bearer); !errors.Is(err, auth.ErrFlowInvalid) {
		t.Fatalf("session bearer must stay invalid for flows, got %v", err)
	}
}

func fixtureOperatorFormalPassword(t *testing.T) string {
	t.Helper()
	return "Operator formal passphrase 2027!"
}
