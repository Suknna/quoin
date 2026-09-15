package auth_test

// Recovery lifecycle coverage (docs/authentication-design.md §6). The
// administrator state is seeded directly through SQL so the tests exercise
// the recovery entry points themselves and never depend on the bootstrap
// default credential. Live user data stays in per-test temporary directories
// (testConfig), never in any real deployment store.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

const (
	recoveryLostPassword = "Lost administrator passphrase 2026!"
	recoveryNewPassword  = "Recovered administrator passphrase 2027!"
	recoveryContactEmail = "ops@quoin.test"
)

// seedRecoveryAdmin inserts the built-in administrator directly with the
// requested initialization state and a known password hash.
func seedRecoveryAdmin(t *testing.T, db *sql.DB, initialized bool, password string) int64 {
	t.Helper()
	phc, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	initializedValue := 0
	if initialized {
		initializedValue = 1
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := db.ExecContext(context.Background(), `INSERT INTO users(username,display_name,role,enabled,initialized,password_phc,password_change_required,created_at,updated_at) VALUES('admin','Administrator','admin',1,?,?,0,?,?)`, initializedValue, phc, now, now)
	if err != nil {
		t.Fatal(err)
	}
	userID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return userID
}

func seedRecoveryContact(t *testing.T, db *sql.DB, userID int64) int64 {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := db.ExecContext(context.Background(), `INSERT INTO user_contacts(user_id,channel,target,version,verified_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, userID, "email", recoveryContactEmail, 1, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// seedRecoverySession inserts one live session bound to the current auth
// revision and returns its bearer.
func seedRecoverySession(t *testing.T, db *sql.DB, userID int64) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	now := time.Now().UTC()
	_, err := db.ExecContext(context.Background(), `INSERT INTO sessions(user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(?,?,?,?,?,?,?,?)`,
		userID, digest[:], 1, "recovery test", now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
		now.Add(12*time.Hour).Format(time.RFC3339Nano), now.Add(7*24*time.Hour).Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// seedRecoveryPendingFlow inserts one pending login flow and returns its
// bearer, so tests can assert the recovery revocation sweep.
func seedRecoveryPendingFlow(t *testing.T, db *sql.DB, userID int64) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	now := time.Now().UTC()
	_, err := db.ExecContext(context.Background(), `INSERT INTO auth_flows(flow_type,user_id,flow_token_digest,correlation_id,auth_revision_at_issue,password_set,client_label,status,created_at,expires_at) VALUES('login',?,?,'recovery-test',?,0,'recovery test','pending',?,?)`,
		userID, digest[:], 1, now.Format(time.RFC3339Nano), now.Add(15*time.Minute).Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// TestBeginRecoveryRejectsCallerCorrelation enforces the CLI-only entry: a
// context that already carries execution metadata (HTTP, scheduler) can never
// drive the offline recovery — the operation demands a fresh system/CLI root.
func TestBeginRecoveryRejectsCallerCorrelation(t *testing.T) {
	ctx := context.Background()
	service, db := newAuthService(t)
	seedRecoveryAdmin(t, db, true, recoveryLostPassword)

	correlation, err := execution.NewCorrelationID()
	if err != nil {
		t.Fatal(err)
	}
	httpCtx, err := execution.WithMetadata(ctx, execution.Metadata{
		CorrelationID: correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Source:        execution.Source{Kind: execution.SourceHTTP},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.BeginRecovery(httpCtx, auth.RecoveryModePassword, recoveryNewPassword); err == nil {
		t.Fatal("recovery with a caller-owned context must be rejected")
	}
	var phc string
	if err := db.QueryRowContext(ctx, `SELECT password_phc FROM users WHERE username='admin'`).Scan(&phc); err != nil {
		t.Fatal(err)
	}
	if !auth.VerifyPassword(recoveryLostPassword, phc) {
		t.Fatal("the rejected run must not have touched the credential")
	}
}

func TestBeginRecoveryPasswordModePreservesVerifiedFactors(t *testing.T) {
	ctx := context.Background()
	service, db := newAuthService(t)
	userID := seedRecoveryAdmin(t, db, true, recoveryLostPassword)
	contactID := seedRecoveryContact(t, db, userID)
	sessionBearer := seedRecoverySession(t, db, userID)
	pendingBearer := seedRecoveryPendingFlow(t, db, userID)
	if _, err := service.Authenticate(ctx, sessionBearer); err != nil {
		t.Fatalf("seeded session must start valid: %v", err)
	}

	credential, err := service.BeginRecovery(ctx, auth.RecoveryModePassword, recoveryNewPassword)
	if err != nil {
		t.Fatalf("password recovery: %v", err)
	}
	if credential.TemporaryPassword != "" {
		t.Fatal("password mode must not issue a generated credential")
	}

	if _, err := service.Authenticate(ctx, sessionBearer); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("recovery must revoke the old session, got %v", err)
	}
	if _, err := service.ReadFlow(ctx, pendingBearer); !errors.Is(err, auth.ErrFlowInvalid) {
		t.Fatalf("recovery must revoke pending flows, got %v", err)
	}
	var pending int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_flows WHERE user_id=? AND status='pending'`, userID).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("no pending flow may survive recovery: count=%d err=%v", pending, err)
	}

	// The temporary password starts the unified initialization flow: the
	// verified contact is preserved, but the administrator must set a formal
	// password before a workbench session exists.
	flow, _, err := service.StartAuthentication(ctx, "admin", recoveryNewPassword, "Mozilla/5.0 test")
	if err != nil {
		t.Fatalf("temporary password must start an initialization flow: %v", err)
	}
	if flow.Type != auth.FlowAdminInitialize || flow.User.Initialized || !flow.User.PasswordChangeRequired {
		t.Fatalf("unexpected flow after password recovery: %+v", flow)
	}
	if len(flow.Contacts) != 1 || !flow.Contacts[0].Verified {
		t.Fatalf("verified contact must survive password recovery: %+v", flow.Contacts)
	}
	// A retained verified target never skips the challenge: the fresh flow
	// starts with factorVerified=false and cannot complete before its own
	// OTP was verified.
	if flow.FactorVerified {
		t.Fatal("a fresh flow must not report the factor as verified")
	}
	if err := service.CompleteAdminInitialization(ctx, flow.Bearer); err == nil {
		t.Fatal("completion before the fresh OTP must be rejected")
	}
	sender := configureAuth(t, service)
	if _, _, err := service.SendFlowChallenge(ctx, flow.Bearer, flow.Contacts[0].Locator); err != nil {
		t.Fatalf("send challenge to the retained target: %v", err)
	}
	if err := service.VerifyFlowChallenge(ctx, flow.Bearer, sender.code()); err != nil {
		t.Fatalf("the fresh OTP must verify: %v", err)
	}
	refreshed, err := service.ReadFlow(ctx, flow.Bearer)
	if err != nil {
		t.Fatal(err)
	}
	if !refreshed.FactorVerified {
		t.Fatal("after this flow's OTP the marker must be server-authoritatively true")
	}
	if _, _, err := service.StartAuthentication(ctx, "admin", recoveryLostPassword, "test"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("old password must be rejected, got %v", err)
	}
	var verified sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT verified_at FROM user_contacts WHERE id=?`, contactID).Scan(&verified); err != nil || !verified.Valid {
		t.Fatalf("verified contact must survive password recovery: err=%v verified=%v", err, verified)
	}
}

func TestBeginRecoveryPasswordModeRejections(t *testing.T) {
	ctx := context.Background()
	service, db := newAuthService(t)
	seedRecoveryAdmin(t, db, true, recoveryLostPassword)
	seedRecoveryContact(t, db, mustFirstAdminID(t, db))

	if _, err := service.BeginRecovery(ctx, auth.RecoveryModePassword, "short"); !errors.Is(err, auth.ErrPasswordPolicy) {
		t.Fatalf("weak replacement must fail the policy, got %v", err)
	}
	if _, _, err := service.StartAuthentication(ctx, "admin", recoveryLostPassword, "test"); err != nil {
		t.Fatalf("rejected recovery must leave the old credential intact: %v", err)
	}
	if _, err := service.BeginRecovery(ctx, auth.RecoveryMode("other"), recoveryNewPassword); !errors.Is(err, auth.ErrValidation) {
		t.Fatalf("unknown mode must be a validation error, got %v", err)
	}

	empty, _ := newAuthService(t)
	if _, err := empty.BeginRecovery(ctx, auth.RecoveryModePassword, recoveryNewPassword); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("without an administrator recovery must be not-found, got %v", err)
	}
}

// mustFirstAdminID returns the built-in administrator row id.
func mustFirstAdminID(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRowContext(context.Background(), `SELECT id FROM users WHERE role='admin'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// seedRecoveryHistory records a COMPLETED login flow and one consumed second
// factor challenge bound to the given contact, so recovery tests prove the
// reset keeps historical foreign keys intact (the deleted-rows approach would
// violate the RESTRICT constraints).
func seedRecoveryHistory(t *testing.T, db *sql.DB, userID, contactID int64) {
	t.Helper()
	_, digest := mustRawToken(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := db.ExecContext(context.Background(), `INSERT INTO auth_flows(flow_type,user_id,flow_token_digest,correlation_id,auth_revision_at_issue,password_set,verified_contact_id,client_label,status,created_at,expires_at,completed_at) VALUES('login',?,?,'recovery-history',1,1,?,'history test','completed',?,?,?)`,
		userID, digest, contactID, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	flowID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	var codeDigest [32]byte
	if _, err := rand.Read(codeDigest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `INSERT INTO auth_challenges(flow_id,user_id,purpose,contact_id,contact_version,auth_revision_at_issue,code_digest,delivery_id,delivery_status,created_at,expires_at,consumed_at) VALUES(?,?, 'second_factor',?,1,1,?,'recovery-history-delivery','accepted',?,?,?)`,
		flowID, userID, contactID, codeDigest[:], now, now, now); err != nil {
		t.Fatal(err)
	}
}

// mustRawToken returns a fresh random bearer and its SHA-256 digest.
func mustRawToken(t *testing.T) ([]byte, []byte) {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	return raw, digest[:]
}

func TestBeginRecoveryFactorsResetsAndSupersedesTokens(t *testing.T) {
	ctx := context.Background()
	service, db := newAuthService(t)
	userID := seedRecoveryAdmin(t, db, true, recoveryLostPassword)
	contactID := seedRecoveryContact(t, db, userID)
	sessionBearer := seedRecoverySession(t, db, userID)
	seedRecoveryHistory(t, db, userID, contactID)

	first, err := service.BeginRecovery(ctx, auth.RecoveryModeFactors, "")
	if err != nil {
		t.Fatalf("factors recovery: %v", err)
	}
	if first.TemporaryPassword == "" {
		t.Fatal("factors mode must issue a generated temporary password")
	}

	var initialized int
	if err := db.QueryRowContext(ctx, `SELECT initialized FROM users WHERE id=?`, userID).Scan(&initialized); err != nil || initialized != 0 {
		t.Fatalf("factors recovery must reset the initialized state: value=%d err=%v", initialized, err)
	}
	if _, _, err := service.StartAuthentication(ctx, "admin", recoveryLostPassword, "test"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("the previous password must be unverifiable, got %v", err)
	}
	if _, err := service.Authenticate(ctx, sessionBearer); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("factors recovery must revoke sessions, got %v", err)
	}
	// A genuine factors reset leaves NO usable target, but the contact ROW
	// survives disabled and unverified: the completed flow and the consumed
	// challenge keep their foreign-key history, and re-enrollment reuses the
	// row with a fresh verification requirement.
	var enabledTargets, verifiedTargets, totalRows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN verified_at IS NOT NULL THEN 1 ELSE 0 END),0),(SELECT COUNT(*) FROM user_contacts WHERE user_id=?) FROM user_contacts WHERE user_id=? AND enabled=1`, userID, userID).Scan(&enabledTargets, &verifiedTargets, &totalRows); err != nil {
		t.Fatal(err)
	}
	if enabledTargets != 0 || verifiedTargets != 0 || totalRows != 1 {
		t.Fatalf("factors reset must retire every target while keeping its row: enabled=%d verified=%d rows=%d", enabledTargets, verifiedTargets, totalRows)
	}
	var historyFlows, historyChallenges int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_flows WHERE verified_contact_id=? AND status='completed'`, contactID).Scan(&historyFlows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_challenges WHERE contact_id=? AND delivery_status='accepted'`, contactID).Scan(&historyChallenges); err != nil {
		t.Fatal(err)
	}
	if historyFlows != 1 || historyChallenges != 1 {
		t.Fatalf("historical flow/challenge references must survive the reset: flows=%d challenges=%d", historyFlows, historyChallenges)
	}

	// A newer recovery supersedes older temporary passwords: only the newest
	// credential can sign in.
	second, err := service.BeginRecovery(ctx, auth.RecoveryModeFactors, "")
	if err != nil {
		t.Fatalf("second factors recovery: %v", err)
	}
	if _, _, err := service.StartAuthentication(ctx, "admin", first.TemporaryPassword, "test"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("superseded temporary password must be invalid, got %v", err)
	}
	flow, _, err := service.StartAuthentication(ctx, "admin", second.TemporaryPassword, "test")
	if err != nil {
		t.Fatalf("sign in with the newest temporary password: %v", err)
	}
	if flow.Type != auth.FlowAdminInitialize || flow.PasswordSet {
		t.Fatalf("unexpected initialization flow: %+v", flow)
	}
	if len(flow.Contacts) != 0 || flow.FactorVerified {
		t.Fatalf("the factors-reset flow must expose no usable targets: %+v", flow)
	}
	// Re-enrolling the retired channel reuses its row and demands a fresh
	// verification before the flow can complete.
	contact, err := service.RegisterFlowContact(ctx, flow.Bearer, "email", recoveryContactEmail)
	if err != nil {
		t.Fatalf("re-enroll the retired channel: %v", err)
	}
	if contact.Verified {
		t.Fatalf("the re-enrolled target must start unverified: %+v", contact)
	}
	if contact.Locator == "" {
		t.Fatal("the re-enrolled target must expose its locator")
	}
	var reEnrolledID int64
	if parsed, parseErr := strconv.ParseInt(contact.Locator, 10, 64); parseErr != nil {
		t.Fatalf("unexpected contact locator %q: %v", contact.Locator, parseErr)
	} else {
		reEnrolledID = parsed
	}
	if reEnrolledID != contactID {
		t.Fatalf("re-enrollment must reuse the historical row: got id %d want %d", reEnrolledID, contactID)
	}
	// Restarting with the same temporary password supersedes the previous
	// pending flow.
	restarted, _, err := service.StartAuthentication(ctx, "admin", second.TemporaryPassword, "test")
	if err != nil {
		t.Fatalf("restart initialization: %v", err)
	}
	if _, err := service.ReadFlow(ctx, flow.Bearer); !errors.Is(err, auth.ErrFlowInvalid) {
		t.Fatalf("superseded flow must be invalid, got %v", err)
	}
	if _, err := service.ReadFlow(ctx, restarted.Bearer); err != nil {
		t.Fatalf("restarted flow must stay valid: %v", err)
	}
}

func TestRecoveryFullPathWithoutExistingVerifiedFactor(t *testing.T) {
	ctx := context.Background()
	service, db := newAuthService(t)
	// A seeded account with no usable factor: only CLI factors recovery and
	// the printed temporary credential can re-enter.
	userID := seedRecoveryAdmin(t, db, false, recoveryLostPassword)

	credential, err := service.BeginRecovery(ctx, auth.RecoveryModeFactors, "")
	if err != nil {
		t.Fatalf("factors recovery: %v", err)
	}
	sender := configureAuth(t, service)
	// The printed temporary credential signs in directly and starts the same
	// unified initialization flow as a first install.
	flow, _, err := service.StartAuthentication(ctx, "admin", credential.TemporaryPassword, "Mozilla/5.0 test")
	if err != nil {
		t.Fatalf("start unified initialization: %v", err)
	}
	if flow.Type != auth.FlowAdminInitialize {
		t.Fatalf("recovery must enter the unified initialization flow, got %q", flow.Type)
	}

	if err := service.SetFlowPassword(ctx, flow.Bearer, recoveryNewPassword); err != nil {
		t.Fatalf("set recovery password: %v", err)
	}
	contact, err := service.RegisterFlowContact(ctx, flow.Bearer, "email", recoveryContactEmail)
	if err != nil {
		t.Fatalf("register recovery contact: %v", err)
	}
	if _, _, err := service.SendFlowChallenge(ctx, flow.Bearer, contact.Locator); err != nil {
		t.Fatalf("send recovery challenge: %v", err)
	}
	if code := sender.code(); len(code) != 6 {
		t.Fatalf("expected a 6-digit code, got %q", code)
	}
	if err := service.VerifyFlowChallenge(ctx, flow.Bearer, sender.code()); err != nil {
		t.Fatalf("verify recovery challenge: %v", err)
	}
	if err := service.CompleteAdminInitialization(ctx, flow.Bearer); err != nil {
		t.Fatalf("complete re-initialization: %v", err)
	}

	var initialized int
	if err := db.QueryRowContext(ctx, `SELECT initialized FROM users WHERE id=?`, userID).Scan(&initialized); err != nil || initialized != 1 {
		t.Fatalf("recovery completion must re-initialize the administrator: value=%d err=%v", initialized, err)
	}
	login, _, err := service.StartAuthentication(ctx, "admin", recoveryNewPassword, "Mozilla/5.0 test")
	if err != nil {
		t.Fatalf("new password must start a normal login: %v", err)
	}
	if login.Type != auth.FlowLogin {
		t.Fatalf("recovered administrator must land on the login flow, got %q", login.Type)
	}
	if _, _, err := service.StartAuthentication(ctx, "admin", credential.TemporaryPassword, "test"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("replaced temporary password must be invalid, got %v", err)
	}
	if err := service.CompleteAdminInitialization(ctx, flow.Bearer); !errors.Is(err, auth.ErrFlowInvalid) {
		t.Fatalf("completed flow must be dead, got %v", err)
	}
	var audits int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE action LIKE 'auth.recovery%' OR action LIKE 'auth.admin_initialize%' OR action LIKE 'auth.contact%' OR action LIKE 'auth.otp%'`).Scan(&audits); err != nil || audits < 3 {
		t.Fatalf("every recovery phase must be audited: count=%d err=%v", audits, err)
	}
}
