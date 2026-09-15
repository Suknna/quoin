package auth_test

// Regression for the automatic audit target authority (ADR-0006): the audit
// row's domain_ref/target pair must name exactly the object the operation
// declaration promises — the flow's real user row for credential starts,
// initializations and the factor change, the pending auth_flow row for the
// flow steps, the real user_contacts row for contact registration — for
// success, rejected and recorded-failure outcomes alike. The fixture first
// creates several users, contacts, flows and challenges so user, contact and
// flow ids diverge; a swapped id (e.g. the flow id under the "user" type, or
// the user id under "auth_flow") can no longer satisfy the join back to the
// real row. No success body secret (bearer, one-time code) may reach any
// audit text column.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/auth"
)

// deriveWrongCode returns a deterministic 6-digit code that differs from the
// issued one.
func deriveWrongCode(issued string) string {
	if issued == "000000" {
		return "000001"
	}
	return "000000"
}

func TestAuditTargetsNameTheDeclaredAuthority(t *testing.T) {
	service, sender, db := newFlowService(t)
	ctx := context.Background()
	bootstrapPendingAdmin(t, service)
	initializeAdminDrive(t, service, sender)
	adminUser, session, adminSessionBearer := flowLogin(t, service, sender, "admin", fixtureAdminPassword)
	adminID := adminUser.ID

	// expectTarget asserts the newest audit row of one action/outcome pair
	// (narrowed by the flow correlation and/or the client command id, each
	// optional) declares the wanted target type and id, and that the domain
	// ref and the target row agree.
	expectTarget := func(action, outcome, correlation, clientCommandID, wantType string, wantID int64) {
		t.Helper()
		var refType, targetType string
		var targetID int64
		if err := db.QueryRowContext(ctx, `
			SELECT e.domain_ref_type, t.target_type, t.target_id
			FROM audit_events e JOIN audit_event_targets t ON t.audit_event_id=e.id
			WHERE e.action=? AND e.outcome=? AND (?='' OR e.correlation_id=?) AND (?='' OR e.client_command_id=?)
			ORDER BY e.id DESC LIMIT 1`,
			action, outcome, correlation, correlation, clientCommandID, clientCommandID).Scan(&refType, &targetType, &targetID); err != nil {
			t.Fatalf("audit row %s/%s: %v", action, outcome, err)
		}
		if refType != targetType {
			t.Fatalf("%s: domain_ref_type %q must equal target_type %q", action, refType, targetType)
		}
		if targetType != wantType || targetID != wantID {
			t.Fatalf("%s/%s audit target = (%s,%d), want (%s,%d)", action, outcome, targetType, targetID, wantType, wantID)
		}
	}
	flowByCorrelation := func(correlation string) (int64, int64) {
		t.Helper()
		var id, userID int64
		if err := db.QueryRowContext(ctx, `SELECT id,user_id FROM auth_flows WHERE correlation_id=?`, correlation).Scan(&id, &userID); err != nil {
			t.Fatalf("flow for correlation %s: %v", correlation, err)
		}
		return id, userID
	}

	// Volume first: a second operator with BOTH channels pushes the
	// user_contacts sequence ahead of the users sequence, so user, contact,
	// flow and challenge ids all diverge.
	fixture := &adminFixture{service: service, db: db, sender: sender, admin: session, t: t}
	if _, _, err := service.CreateUser(ctx, session, auth.CreateUserInput{
		ClientCommandID: "cmd-create-op2", Digest: auth.DigestCommand("user.create", map[string]any{"username": "op2"}),
		Username: "op2", DisplayName: "Operator Two", Role: "operator", Password: fixtureOperatorTempPass,
		Contacts: []auth.ContactInput{{Channel: "email", Target: "op2@quoin.test"}, {Channel: "sms", Target: "+15550000002"}},
	}); err != nil {
		t.Fatal(err)
	}
	fixture.completeOperator("op2", fixtureOperatorTempPass, "Operator two passphrase 2027!")

	// Every transient secret this run minted, for the end-of-run leak check.
	var issuedCodes, bearers []string

	// Operator creation: the ledger-path command targets the created user row.
	if _, _, err := service.CreateUser(ctx, session, auth.CreateUserInput{
		ClientCommandID: "cmd-create-op1", Digest: auth.DigestCommand("user.create", map[string]any{"username": "op1"}),
		Username: "op1", DisplayName: "Operator One", Role: "operator", Password: fixtureOperatorTempPass,
		Contacts: []auth.ContactInput{{Channel: "email", Target: fixtureOperatorEmail}},
	}); err != nil {
		t.Fatal(err)
	}
	var op1ID int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM users WHERE username='op1'`).Scan(&op1ID); err != nil {
		t.Fatal(err)
	}
	expectTarget("user.create", "success", "", "cmd-create-op1", "user", op1ID)

	// Shift the flow sequence past the contact sequence: a throwaway login
	// start guarantees every later flow row id lies beyond the contact id
	// sequence, so a swapped id can never pass the checks silently.
	if _, _, err := service.StartAuthentication(ctx, "admin", fixtureAdminPassword, "UA"); err != nil {
		t.Fatal(err)
	}
	// All contact rows exist from here on; every flow created afterwards must
	// carry a strictly higher id than the whole contact and user sequences.
	var maxContactID, maxUserID int64
	if err := db.QueryRowContext(ctx, `SELECT (SELECT MAX(id) FROM user_contacts), (SELECT MAX(id) FROM users)`).Scan(&maxContactID, &maxUserID); err != nil {
		t.Fatal(err)
	}

	// Operator initialization chain, step by step, including a wrong-code
	// verification (recorded failure) before the successful one.
	flow, _, err := service.StartOperatorInitialization(ctx, "op1", fixtureOperatorTempPass)
	if err != nil {
		t.Fatal(err)
	}
	// Divergence guards: the flow id lies beyond both id sequences and
	// differs from the flow's own user id, so a swapped id (the old bug)
	// can never silently satisfy the checks below.
	flowID, flowUserID := flowByCorrelation(flow.CorrelationID)
	if flowUserID != op1ID {
		t.Fatalf("operator flow must belong to op1: flow user %d", flowUserID)
	}
	if flowID == op1ID || flowID <= maxUserID || flowID <= maxContactID {
		t.Fatalf("flow id %d must lie beyond user ids (max %d) and contact ids (max %d) and differ from its user %d", flowID, maxUserID, maxContactID, op1ID)
	}
	expectTarget("auth.operator_initialize.start", "success", flow.CorrelationID, "", "user", op1ID)
	if err := service.SetFlowPassword(ctx, flow.Bearer, "Operator one passphrase 2027!"); err != nil {
		t.Fatal(err)
	}
	expectTarget("auth.flow.set_password", "success", flow.CorrelationID, "", "user", op1ID)
	read, err := service.ReadFlow(ctx, flow.Bearer)
	if err != nil {
		t.Fatal(err)
	}
	op1ContactID, err := strconv.ParseInt(read.Contacts[0].Locator, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if op1ContactID == op1ID || op1ContactID >= flowID {
		t.Fatalf("contact id %d must differ from user id %d and stay below flow id %d", op1ContactID, op1ID, flowID)
	}
	if _, _, err := service.SendFlowChallenge(ctx, flow.Bearer, read.Contacts[0].Locator); err != nil {
		t.Fatal(err)
	}
	issuedCodes = append(issuedCodes, sender.code())
	expectTarget("auth.challenge.issue", "success", flow.CorrelationID, "", "auth_flow", flowID)
	if err := service.VerifyFlowChallenge(ctx, flow.Bearer, deriveWrongCode(sender.code())); !errors.Is(err, auth.ErrOtpInvalid) {
		t.Fatalf("wrong code must fail verification, got %v", err)
	}
	expectTarget("auth.challenge.verify", "failure", flow.CorrelationID, "", "auth_flow", flowID)
	if err := service.VerifyFlowChallenge(ctx, flow.Bearer, sender.code()); err != nil {
		t.Fatal(err)
	}
	expectTarget("auth.challenge.verify", "success", flow.CorrelationID, "", "auth_flow", flowID)
	if err := service.CompleteOperatorInitialization(ctx, flow.Bearer); err != nil {
		t.Fatal(err)
	}
	expectTarget("auth.operator_initialize.complete", "success", flow.CorrelationID, "", "user", op1ID)

	// Contact registration happens in the admin initialization chain: the
	// newest register row must name the admin's actual user_contacts row.
	var adminContactID int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM user_contacts WHERE user_id=? AND channel='email'`, adminID).Scan(&adminContactID); err != nil {
		t.Fatal(err)
	}
	expectTarget("auth.flow.register_contact", "success", "", "", "user_contact", adminContactID)

	// Two-step login chain on a fresh login flow: a wrong code before any
	// challenge (recorded failure), a rejected issue for a missing contact,
	// then the successful issue and completion — all on the same flow id.
	loginFlow, _, err := service.StartAuthentication(ctx, "admin", fixtureAdminPassword, "Mozilla/5.0 Chrome Linux")
	if err != nil {
		t.Fatal(err)
	}
	loginFlowID, loginFlowUser := flowByCorrelation(loginFlow.CorrelationID)
	if loginFlowUser != adminID || loginFlowID <= maxUserID || loginFlowID <= maxContactID {
		t.Fatalf("login flow ids must diverge from user/contact ids: flow=%d maxUser=%d maxContact=%d", loginFlowID, maxUserID, maxContactID)
	}
	expectTarget("auth.login.start", "success", loginFlow.CorrelationID, "", "user", adminID)
	if _, err := service.CompleteLogin(ctx, loginFlow.Bearer, "000000"); !errors.Is(err, auth.ErrOtpInvalid) {
		t.Fatalf("code without a challenge must fail, got %v", err)
	}
	expectTarget("auth.login.complete", "failure", loginFlow.CorrelationID, "", "auth_flow", loginFlowID)
	if _, _, err := service.SendFlowChallenge(ctx, loginFlow.Bearer, "999999"); !errors.Is(err, auth.ErrValidation) {
		t.Fatalf("missing contact must be rejected, got %v", err)
	}
	expectTarget("auth.challenge.issue", "rejected", loginFlow.CorrelationID, "", "auth_flow", loginFlowID)
	if _, _, err := service.SendFlowChallenge(ctx, loginFlow.Bearer, loginFlow.Contacts[0].Locator); err != nil {
		t.Fatal(err)
	}
	issuedCodes = append(issuedCodes, sender.code())
	bearers = append(bearers, loginFlow.Bearer)
	expectTarget("auth.challenge.issue", "success", loginFlow.CorrelationID, "", "auth_flow", loginFlowID)
	if _, err := service.CompleteLogin(ctx, loginFlow.Bearer, sender.code()); err != nil {
		t.Fatal(err)
	}
	expectTarget("auth.login.complete", "success", loginFlow.CorrelationID, "", "auth_flow", loginFlowID)

	// Session-scope corrections: the touched sessions row, the aggregate
	// revocation's target user and the specifically revoked own session row
	// each name their own object — never the acting user id.
	op1User, _, _ := flowLogin(t, service, sender, "op1", "Operator one passphrase 2027!")
	if err != nil {
		t.Fatal(err)
	}
	issuedCodes = append(issuedCodes, sender.code())
	if op1User.ID == adminID {
		t.Fatal("operator and administrator ids must diverge for this regression to bite")
	}
	// The aggregate sweep targets op1's whole session set: the audited object
	// is op1's user row, distinguishable from the acting administrator.
	if _, _, err := service.RevokeUserSessions(ctx, session, auth.RevokeSessionsInput{
		ClientCommandID: "cmd-revoke-op1", Digest: auth.DigestCommand("user.revoke_sessions", map[string]any{"userId": op1User.ID}),
		UserID: op1User.ID,
	}); err != nil {
		t.Fatal(err)
	}
	expectTarget("user.revoke_sessions", "success", "", "cmd-revoke-op1", "user", op1User.ID)

	// A second administrator session: its row id strictly differs from the
	// user id, so a swapped target cannot pass silently.
	_, secondAdminSession, secondAdminBearer := flowLogin(t, service, sender, "admin", fixtureAdminPassword)
	issuedCodes = append(issuedCodes, sender.code())
	bearers = append(bearers, secondAdminBearer)
	if secondAdminSession.ID == adminID || secondAdminSession.ID == session.ID {
		t.Fatalf("session id %d must differ from user ids %d/%d", secondAdminSession.ID, adminID, session.ID)
	}
	// Activity renewal runs only past the throttle window and the schema
	// forbids moving last_active_at backwards, so the fixture inserts an
	// already-stale session row for the administrator (same pattern as the
	// renewal test). Its row id strictly differs from the user id.
	stale := time.Now().UTC().Add(-2 * time.Hour)
	touchDigest := sha256.Sum256(bytes.Repeat([]byte{0x31}, 32))
	touchResult, err := db.ExecContext(ctx, `INSERT INTO sessions(user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(?,?,?,?,?,?,?,?)`,
		adminID, touchDigest[:], session.User.AuthRevision, "Test on device",
		stale.Format(time.RFC3339Nano), stale.Format(time.RFC3339Nano),
		stale.Add(12*time.Hour).Format(time.RFC3339Nano), stale.Add(7*24*time.Hour).Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	touchSessionID, err := touchResult.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if touchSessionID == adminID {
		t.Fatalf("session id %d must differ from the user id", touchSessionID)
	}
	touchBearer := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x31}, 32))
	bearers = append(bearers, touchBearer)
	if err := service.TouchSessionActivity(ctx, touchBearer); err != nil {
		t.Fatal(err)
	}
	expectTarget("auth.session.activity", "success", "", "", "session", touchSessionID)

	// The specific own-session revocation names the revoked sessions row.
	if _, _, err := service.RevokeUserSessions(ctx, session, auth.RevokeSessionsInput{
		ClientCommandID: "cmd-own-revoke", Digest: auth.DigestCommand("session.revoke_own", map[string]any{"sessionId": secondAdminSession.ID}),
		OwnScope: true, SpecificSession: secondAdminSession.ID,
	}); err != nil {
		t.Fatal(err)
	}
	expectTarget("session.revoke_own", "success", "", "cmd-own-revoke", "session", secondAdminSession.ID)

	// A specific revocation whose target row does not exist records the
	// rejection under the session type with NO object id (0) — never a
	// foreign row id.
	if _, _, err := service.RevokeUserSessions(ctx, session, auth.RevokeSessionsInput{
		ClientCommandID: "cmd-own-revoke-missing", Digest: auth.DigestCommand("session.revoke_own", map[string]any{"sessionId": int64(999999)}),
		OwnScope: true, SpecificSession: 999999,
	}); err == nil {
		t.Fatal("revoking a missing session must fail")
	}
	expectTarget("session.revoke_own", "rejected", "", "cmd-own-revoke-missing", "session", 0)

	// Factor change: the completion without a staged candidate is a
	// deterministic rejection naming the administrator; the staged candidate
	// lives on the flow, so the staging audit target is the flow row.
	staged, _, err := service.StartContactChange(ctx, session, fixtureAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.CompleteContactChange(ctx, session, staged.Bearer, fixtureAdminPassword); err == nil {
		t.Fatal("completion without a staged candidate must be rejected")
	}
	expectTarget("auth.contact_change.complete", "rejected", "", "", "user", adminID)
	changeFlow, _, err := service.StartContactChange(ctx, session, fixtureAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	changeFlowID, _ := flowByCorrelation(changeFlow.CorrelationID)
	if changeFlowID <= maxUserID || changeFlowID <= maxContactID || changeFlowID == adminID {
		t.Fatalf("change flow id %d must diverge from user ids (max %d) and contact ids (max %d)", changeFlowID, maxUserID, maxContactID)
	}
	if _, err := service.RegisterFlowContact(ctx, changeFlow.Bearer, "email", "admin2@quoin.test"); err != nil {
		t.Fatal(err)
	}
	expectTarget("auth.flow.stage_contact", "success", changeFlow.CorrelationID, "", "auth_flow", changeFlowID)
	if _, _, err := service.SendFlowChallenge(ctx, changeFlow.Bearer, "email"); err != nil {
		t.Fatal(err)
	}
	issuedCodes = append(issuedCodes, sender.code())
	bearers = append(bearers, changeFlow.Bearer)
	expectTarget("auth.challenge.issue", "success", changeFlow.CorrelationID, "", "auth_flow", changeFlowID)
	if err := service.VerifyFlowChallenge(ctx, changeFlow.Bearer, deriveWrongCode(sender.code())); !errors.Is(err, auth.ErrOtpInvalid) {
		t.Fatalf("wrong code must fail verification, got %v", err)
	}
	expectTarget("auth.challenge.verify", "failure", changeFlow.CorrelationID, "", "auth_flow", changeFlowID)
	if err := service.VerifyFlowChallenge(ctx, changeFlow.Bearer, sender.code()); err != nil {
		t.Fatal(err)
	}
	if err := service.CompleteContactChange(ctx, session, changeFlow.Bearer, fixtureAdminPassword); err != nil {
		t.Fatal(err)
	}
	expectTarget("auth.contact_change.complete", "success", "", "", "user", adminID)

	// Set-based authority invariants over EVERY audit row written by this
	// run: each class must join back to the real row it declares.
	invariant := func(name, query string, wantMinimum int64) {
		t.Helper()
		var mismatches, total int64
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (`+query+`) WHERE ok=0`).Scan(&mismatches); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (`+query+`)`).Scan(&total); err != nil {
			t.Fatal(err)
		}
		if mismatches != 0 {
			t.Fatalf("%s: %d of %d audit targets name a wrong object", name, mismatches, total)
		}
		if total < wantMinimum {
			t.Fatalf("%s: only %d audit rows checked, want at least %d", name, total, wantMinimum)
		}
	}
	// Flow steps: the target is THE flow row of the row's own correlation —
	// success, failure and rejected alike.
	invariant("flow steps", `
		SELECT t.target_type, t.target_id, e.correlation_id,
		       CASE WHEN t.target_type='auth_flow' AND EXISTS (
		            SELECT 1 FROM auth_flows f
		            WHERE f.id=t.target_id AND f.correlation_id=e.correlation_id)
		       THEN 1 ELSE 0 END AS ok
		FROM audit_events e JOIN audit_event_targets t ON t.audit_event_id=e.id
		WHERE e.phase='execute' AND e.action IN
		      ('auth.challenge.issue','auth.challenge.verify','auth.login.complete','auth.flow.stage_contact')`, 8)
	// Credential starts, initializations and the password step: the target is
	// the flow's real user row under the same correlation.
	invariant("user steps", `
		SELECT t.target_type, t.target_id, e.correlation_id,
		       CASE WHEN t.target_type='user' AND EXISTS (
		            SELECT 1 FROM auth_flows f JOIN users u ON u.id=f.user_id
		            WHERE f.correlation_id=e.correlation_id AND u.id=t.target_id)
		       THEN 1 ELSE 0 END AS ok
		FROM audit_events e JOIN audit_event_targets t ON t.audit_event_id=e.id
		WHERE e.phase='execute' AND e.action IN
		      ('auth.login.start','auth.admin_initialize.start','auth.operator_initialize.start','auth.contact_change.start',
		       'auth.admin_initialize.complete','auth.operator_initialize.complete','auth.flow.set_password')`, 6)
	// Contact registration: the target is a real user_contacts row owned by
	// the flow's user.
	invariant("contact registration", `
		SELECT t.target_type, t.target_id, e.correlation_id,
		       CASE WHEN t.target_type='user_contact' AND EXISTS (
		            SELECT 1 FROM user_contacts c JOIN auth_flows f ON f.user_id=c.user_id
		            WHERE c.id=t.target_id AND f.correlation_id=e.correlation_id)
		       THEN 1 ELSE 0 END AS ok
		FROM audit_events e JOIN audit_event_targets t ON t.audit_event_id=e.id
		WHERE e.phase='execute' AND e.action='auth.flow.register_contact'`, 1)
	// The factor-change completion is session-correlated: the target must be
	// the real administrator row.
	invariant("factor change completion", `
		SELECT t.target_type, t.target_id,
		       CASE WHEN t.target_type='user' AND EXISTS (
		            SELECT 1 FROM users u WHERE u.id=t.target_id)
		       THEN 1 ELSE 0 END AS ok
		FROM audit_events e JOIN audit_event_targets t ON t.audit_event_id=e.id
		WHERE e.phase='execute' AND e.action='auth.contact_change.complete'`, 2)

	// No success body secret reaches any audit text column: every issued
	// one-time code and every flow/session bearer minted by this run.
	secrets := append(append([]string{}, issuedCodes...), bearers...)
	secrets = append(secrets, adminSessionBearer)
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		var leaked int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM audit_events
			WHERE instr(action,?)>0 OR instr(COALESCE(correlation_id,''),?)>0
			   OR instr(COALESCE(request_id,''),?)>0 OR instr(COALESCE(client_command_id,''),?)>0`,
			secret, secret, secret, secret).Scan(&leaked); err != nil {
			t.Fatal(err)
		}
		if leaked != 0 {
			t.Fatalf("a secret leaked into %d audit rows", leaked)
		}
	}
}
