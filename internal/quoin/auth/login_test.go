package auth_test

// Single-step local login coverage on the canonical schema (ADR-0010): the
// random initial password bootstrap, the forced password change unlocking the
// full admission level, the 24-hour initial password deadline and its
// recovery re-arm, wrong-credential auditing, the rate limiter, session
// revision guards and the throttled activity renewal. Shared fixtures for
// the package live here.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/test/support"
)

const fixtureAdminPassword = "Root admin passphrase 2026!"

func testConfig(t *testing.T) contract.QuoinConfig {
	t.Helper()
	root := t.TempDir()
	secrets := filepath.Join(root, "secrets")
	return contract.QuoinConfig{
		Component: "quoin", PublicOrigin: "https://quoin.test",
		DataDirectory: filepath.Join(root, "data"), BackupDirectory: filepath.Join(root, "backup"),
		RootKeyFile: filepath.Join(secrets, "root-key"), RuntimeTLSCertificateFile: filepath.Join(secrets, "runtime-tls.crt"),
		RuntimeTLSPrivateKeyFile: filepath.Join(secrets, "runtime-tls.key"), RuntimeClientCAFile: filepath.Join(secrets, "stele-service-token"),
	}
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

// bootstrapPendingAdmin seeds the pending built-in administrator exactly once
// with a generated initial password and returns that credential.
func bootstrapPendingAdmin(t *testing.T, service *auth.Service) string {
	t.Helper()
	ctx := context.Background()
	initial, err := auth.GenerateInitialPassword()
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.EnsureBootstrapAdmin(ctx, initial, time.Now().UTC().Add(auth.InitialPasswordLifetime))
	if err != nil || !created {
		t.Fatalf("bootstrap seed: created=%v err=%v", created, err)
	}
	if created, err := service.EnsureBootstrapAdmin(ctx, initial, time.Now().UTC().Add(auth.InitialPasswordLifetime)); err != nil || created {
		t.Fatalf("bootstrap seed must be idempotent: created=%v err=%v", created, err)
	}
	return initial
}

// loginPassword performs one single-step local login and returns the
// authenticated session.
func loginPassword(t *testing.T, service *auth.Service, username, password string) (auth.User, auth.Session, string) {
	t.Helper()
	result, err := service.LoginWithPassword(context.Background(), username, password, "Mozilla/5.0 Chrome Linux")
	if err != nil {
		t.Fatalf("login %s: %v", username, err)
	}
	session, err := service.Authenticate(context.Background(), result.Bearer)
	if err != nil {
		t.Fatalf("authenticate completed login: %v", err)
	}
	return result.User, session, result.Bearer
}

// initializeAdminDrive seeds and unlocks the built-in administrator: the
// initial restricted session completes the forced password change, then the
// formal password logs in. Returns the formal session.
func initializeAdminDrive(t *testing.T, service *auth.Service) (auth.Session, string) {
	t.Helper()
	ctx := context.Background()
	initial := bootstrapPendingAdmin(t, service)
	_, session, _ := loginPassword(t, service, "admin", initial)
	if !session.User.PasswordChangeRequired || session.User.Initialized {
		t.Fatalf("bootstrap login must stay restricted: %+v", session.User)
	}
	if err := service.ChangePassword(ctx, session, initial, fixtureAdminPassword); err != nil {
		t.Fatalf("forced password change: %v", err)
	}
	formalUser, formalSession, bearer := loginPassword(t, service, "admin", fixtureAdminPassword)
	if formalUser.PasswordChangeRequired || !formalUser.Initialized || formalUser.AuthSource != "local" {
		t.Fatalf("formal login projection: %+v", formalUser)
	}
	return formalSession, bearer
}

func TestBootstrapRandomInitialPasswordLifecycle(t *testing.T) {
	ctx := context.Background()
	service, db := newAuthService(t)
	initial := bootstrapPendingAdmin(t, service)

	var expiry string
	if err := db.QueryRow(`SELECT initial_password_expires_at FROM users WHERE username='admin'`).Scan(&expiry); err != nil || expiry == "" {
		t.Fatalf("bootstrap must stamp the initial password deadline: %q %v", expiry, err)
	}

	// Wrong password: deterministic rejection plus an audited failure row.
	if _, err := service.LoginWithPassword(ctx, "admin", "wrong password value!", "UA"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("wrong password must be rejected, got %v", err)
	}
	var failures int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='auth.login.local' AND outcome='failure'`).Scan(&failures); err != nil || failures != 1 {
		t.Fatalf("wrong password must be audited: %d %v", failures, err)
	}

	// The initial password logs into the restricted session; the forced
	// change clears the marker, deadline and flips initialized in one step.
	result, err := service.LoginWithPassword(ctx, "admin", initial, "UA")
	if err != nil {
		t.Fatal(err)
	}
	if !result.User.PasswordChangeRequired {
		t.Fatalf("bootstrap login must be restricted: %+v", result.User)
	}
	session, err := service.Authenticate(ctx, result.Bearer)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ChangePassword(ctx, session, initial, fixtureAdminPassword); err != nil {
		t.Fatal(err)
	}
	var cleared int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE username='admin' AND initial_password_expires_at IS NULL AND initialized=1 AND password_change_required=0`).Scan(&cleared); err != nil || cleared != 1 {
		t.Fatalf("forced change must clear the bootstrap state: %d %v", cleared, err)
	}
	// The dead initial password no longer verifies.
	if _, err := service.LoginWithPassword(ctx, "admin", initial, "UA"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("the initial password must stop verifying after the change, got %v", err)
	}
	var successes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='auth.login.local' AND outcome='success'`).Scan(&successes); err != nil || successes < 1 {
		t.Fatalf("successful logins must be audited: %d %v", successes, err)
	}
}

func TestInitialPasswordExpiryAndRecoveryRearm(t *testing.T) {
	ctx := context.Background()
	service, db := newAuthService(t)
	// Seed with a deadline already in the past: the credential is dead.
	initial, err := auth.GenerateInitialPassword()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnsureBootstrapAdmin(ctx, initial, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.LoginWithPassword(ctx, "admin", initial, "UA"); !errors.Is(err, auth.ErrInitialPasswordExpired) {
		t.Fatalf("expired initial password must be rejected with the deadline error, got %v", err)
	}
	// The runner records the expiry rejection as a failed local login.
	var expired int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='auth.login.local' AND outcome='failure'`).Scan(&expired); err != nil || expired != 1 {
		t.Fatalf("expiry rejection must be audited: %d %v", expired, err)
	}
	// Recovery re-arms the same generated-file path with a fresh deadline.
	replacement := "Fresh recovery passphrase 2027!"
	deadline := time.Now().UTC().Add(auth.InitialPasswordLifetime)
	if _, err := service.BeginRecovery(ctx, replacement, &deadline); err != nil {
		t.Fatal(err)
	}
	if _, _, bearer := loginPassword(t, service, "admin", replacement); bearer == "" {
		t.Fatal("re-armed credential must log in")
	}
}

func TestLocalLoginRejectsExternalAccounts(t *testing.T) {
	ctx := context.Background()
	service, db := newAuthService(t)
	_, _ = initializeAdminDrive(t, service)
	// A manually provisioned external account: identity row, no password.
	seed := `INSERT INTO users(username,display_name,role,enabled,auth_revision,initialized,password_phc,row_version,created_at,updated_at) VALUES('jane','Jane','operator',1,1,1,NULL,1,'2026-09-20T00:00:00Z','2026-09-20T00:00:00Z')`
	if _, err := db.Exec(seed); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO identities(user_id,issuer,subject,created_at) SELECT id,'https://sso.example.com','sub-jane','2026-09-20T00:00:00Z' FROM users WHERE username='jane'`); err != nil {
		t.Fatal(err)
	}
	_, err := service.LoginWithPassword(ctx, "jane", "any password attempt 123!", "UA")
	if err == nil || !errors.Is(err, auth.ErrLocalLoginUnavailable) {
		t.Fatalf("external account must be rejected from local login with the dedicated error, got %v", err)
	}
}

func TestSelfPasswordChangeSessionSemantics(t *testing.T) {
	ctx := context.Background()
	service, _ := newAuthService(t)
	_, _ = initializeAdminDrive(t, service)
	_, firstSession, firstBearer := loginPassword(t, service, "admin", fixtureAdminPassword)
	_, _, secondBearer := loginPassword(t, service, "admin", fixtureAdminPassword)

	const replacement = "A new private passphrase for Quoin 2027!"
	if err := service.ChangePassword(ctx, firstSession, fixtureAdminPassword, replacement); err != nil {
		t.Fatalf("self password change: %v", err)
	}
	// The acting session survives its own password change and advances.
	current, err := service.Authenticate(ctx, firstBearer)
	if err != nil {
		t.Fatalf("current bearer must survive self password change: %v", err)
	}
	if current.User.PasswordChangeRequired || current.User.AuthRevision < 2 {
		t.Fatalf("password change projection did not advance: %+v", current.User)
	}
	// Every other session is revoked by the revision change.
	if _, err := service.Authenticate(ctx, secondBearer); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("other sessions must die on a password change: %v", err)
	}
	// The old password stops verifying.
	if _, err := service.LoginWithPassword(ctx, "admin", fixtureAdminPassword, "UA"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("the old password must stop verifying, got %v", err)
	}
	_, session, bearer := loginPassword(t, service, "admin", replacement)
	if err := service.Logout(ctx, session); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(ctx, bearer); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("logout did not revoke the current session: %v", err)
	}
}

func TestLoginRateLimiter(t *testing.T) {
	ctx := context.Background()
	service, _ := newAuthService(t)
	_, _ = initializeAdminDrive(t, service)
	for i := 0; i < 5; i++ {
		if _, err := service.LoginWithPassword(ctx, "admin", "wrong password value!", "UA"); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("attempt %d must be a plain rejection, got %v", i, err)
		}
	}
	if _, err := service.LoginWithPassword(ctx, "admin", fixtureAdminPassword, "UA"); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("the sixth attempt must hit the limiter even with the right password, got %v", err)
	}
}

func TestSessionRevisionGuardRejectsInvalidTransitions(t *testing.T) {
	ctx := context.Background()
	service, db := newAuthService(t)
	_, _ = initializeAdminDrive(t, service)
	_, session, _ := loginPassword(t, service, "admin", fixtureAdminPassword)
	for name, query := range map[string]string{
		"skip":                 `UPDATE sessions SET auth_revision_at_issue=auth_revision_at_issue+2 WHERE id=?`,
		"rewind":               `UPDATE sessions SET auth_revision_at_issue=auth_revision_at_issue-1 WHERE id=?`,
		"without user advance": `UPDATE sessions SET auth_revision_at_issue=auth_revision_at_issue+1 WHERE id=?`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, query, session.ID); err == nil {
				t.Fatal("invalid revision transition was accepted")
			}
		})
	}
}

func TestTouchSessionActivityRenewal(t *testing.T) {
	ctx := context.Background()
	service, db := newAuthService(t)
	_, _ = initializeAdminDrive(t, service)
	_, session, _ := loginPassword(t, service, "admin", fixtureAdminPassword)
	stale := time.Now().UTC().Add(-2 * time.Hour)
	digest := sha256.Sum256(bytes.Repeat([]byte{0x31}, 32))
	if _, err := db.ExecContext(ctx, `INSERT INTO sessions(user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(?,?,?,?,?,?,?,?)`,
		session.User.ID, digest[:], session.User.AuthRevision, "Test on device",
		stale.Format(time.RFC3339Nano), stale.Format(time.RFC3339Nano),
		stale.Add(12*time.Hour).Format(time.RFC3339Nano), stale.Add(7*24*time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	bearer := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x31}, 32))
	if err := service.TouchSessionActivity(ctx, bearer); err != nil {
		t.Fatal(err)
	}
	var renewed int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE session_token_digest=? AND last_active_at>?`, digest[:], stale.Format(time.RFC3339Nano)).Scan(&renewed); err != nil || renewed != 1 {
		t.Fatalf("stale session must renew: %d %v", renewed, err)
	}
	// An immediate second touch is throttled into a no-op (no audit row).
	var activity int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE action='auth.session.activity'`).Scan(&activity); err != nil || activity != 1 {
		t.Fatalf("activity audit volume must be throttled to one renewal row: %d %v", activity, err)
	}
	if err := service.TouchSessionActivity(ctx, bearer); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE action='auth.session.activity'`).Scan(&activity); err != nil || activity != 1 {
		t.Fatalf("throttled touch must not write again: %d %v", activity, err)
	}
}
