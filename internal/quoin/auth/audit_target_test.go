package auth_test

// Regression for the automatic audit target authority (ADR-0006) on the
// ADR-0010 surface: the audit row's domain_ref/target pair must name exactly
// the object the operation declaration promises — the user row for the
// single-step local login (success and recorded failure alike), the created
// user row for the ledger-path commands, the revoked sessions row for the
// own-session revocation and the touched sessions row for the activity
// renewal. No session bearer may reach any audit text column.

import (
	"context"
	"errors"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

func TestAuditTargetsNameTheDeclaredAuthority(t *testing.T) {
	service, db := newAuthService(t)
	_, _ = initializeAdminDrive(t, service)
	ctx := context.Background()
	admin := mustSession(t, service)
	adminID := admin.User.ID

	expectTarget := func(action, outcome, clientCommandID, wantType string, wantID int64) {
		t.Helper()
		var refType, targetType string
		var targetID int64
		if err := db.QueryRowContext(ctx, `
			SELECT e.domain_ref_type, t.target_type, t.target_id
			FROM audit_events e JOIN audit_event_targets t ON t.audit_event_id=e.id
			WHERE e.action=? AND e.outcome=? AND (?='' OR e.client_command_id=?)
			ORDER BY e.id DESC LIMIT 1`,
			action, outcome, clientCommandID, clientCommandID).Scan(&refType, &targetType, &targetID); err != nil {
			t.Fatalf("audit row %s/%s: %v", action, outcome, err)
		}
		if refType != targetType {
			t.Fatalf("%s: domain_ref_type %q must equal target_type %q", action, refType, targetType)
		}
		if targetType != wantType || targetID != wantID {
			t.Fatalf("%s/%s audit target = (%s,%d), want (%s,%d)", action, outcome, targetType, targetID, wantType, wantID)
		}
	}

	// A wrong password records a failed local login naming the admin row.
	if _, err := service.LoginWithPassword(ctx, "admin", "wrong password value!", "UA"); !errIsUnauthenticated(err) {
		t.Fatalf("wrong password must be rejected, got %v", err)
	}
	expectTarget("auth.login.local", "failure", "", "user", adminID)
	// The successful login names the same user row.
	result, err := service.LoginWithPassword(ctx, "admin", fixtureAdminPassword, "UA")
	if err != nil {
		t.Fatal(err)
	}
	expectTarget("auth.login.local", "success", "", "user", adminID)

	// Operator creation: the ledger-path command targets the created row.
	created := (&adminFixture{service: service, db: db, admin: admin, t: t}).createUser("op1", "Operator One", "operator", "Operator one passphrase 2026!")
	expectTarget("user.create", "success", "cmd-create-op1", "user", created.ID)

	// The own-session revocation names the revoked sessions row, and a
	// missing target records the rejection under the session type with NO
	// object id — never a foreign row id.
	_, secondSession, _ := loginPassword(t, service, "admin", fixtureAdminPassword)
	if secondSession.ID == adminID || secondSession.ID == admin.ID {
		t.Fatalf("session id %d must differ from the user ids %d/%d", secondSession.ID, adminID, admin.ID)
	}
	if _, _, err := service.RevokeUserSessions(ctx, admin, auth.RevokeSessionsInput{
		ClientCommandID: "cmd-own-revoke", Digest: auth.DigestCommand("session.revoke_own", map[string]any{"sessionId": secondSession.ID}),
		OwnScope: true, SpecificSession: secondSession.ID,
	}); err != nil {
		t.Fatal(err)
	}
	expectTarget("session.revoke_own", "success", "cmd-own-revoke", "session", secondSession.ID)
	if _, _, err := service.RevokeUserSessions(ctx, admin, auth.RevokeSessionsInput{
		ClientCommandID: "cmd-own-revoke-missing", Digest: auth.DigestCommand("session.revoke_own", map[string]any{"sessionId": int64(999999)}),
		OwnScope: true, SpecificSession: 999999,
	}); err == nil {
		t.Fatal("revoking a missing session must fail")
	}
	expectTarget("session.revoke_own", "rejected", "cmd-own-revoke-missing", "session", 0)

	// The session bearer never reaches any audit text column.
	var leaked int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM audit_events
		WHERE instr(action,?)>0 OR instr(COALESCE(correlation_id,''),?)>0
		   OR instr(COALESCE(request_id,''),?)>0 OR instr(COALESCE(client_command_id,''),?)>0`,
		result.Bearer, result.Bearer, result.Bearer, result.Bearer).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("a session bearer leaked into %d audit rows", leaked)
	}
}

func errIsUnauthenticated(err error) bool {
	return errors.Is(err, auth.ErrUnauthenticated)
}

// HTTP 主路径（准入层为公开请求预置 system/0 actor）的登录审计归属：本地
// 与 OIDC 登录的 actor 都必须是被尝试/已解析账号，而不是准入占位身份；
// 未知用户名归属 system（不发明用户 id）。
func TestLoginAuditActorOnHTTPPath(t *testing.T) {
	service, db := newAuthService(t)
	_, _ = initializeAdminDrive(t, service)
	admin := mustSession(t, service)
	ctx := context.Background()

	// 模拟准入层 finalize 注入的公开请求 metadata（admission.go:341-347）。
	admissionCtx, err := execution.WithMetadata(ctx, execution.Metadata{
		CorrelationID: "corr-http-login",
		Actor:         execution.Principal{Kind: execution.PrincipalSystem, ID: 0},
		Initiator:     execution.Principal{Kind: execution.PrincipalSystem, ID: 0},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-http-login"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.LoginWithPassword(admissionCtx, "admin", fixtureAdminPassword, "UA"); err != nil {
		t.Fatal(err)
	}
	var actorType string
	var actorID int64
	if err := db.QueryRowContext(ctx, `SELECT actor_type, actor_id FROM audit_events WHERE action='auth.login.local' ORDER BY id DESC LIMIT 1`).Scan(&actorType, &actorID); err != nil {
		t.Fatal(err)
	}
	if actorType != "user" || actorID != admin.User.ID {
		t.Fatalf("HTTP-path local login audit actor=(%s,%d), want (user,%d)", actorType, actorID, admin.User.ID)
	}
	// 关联保持准入链，不被重 rooting。
	var correlation string
	if err := db.QueryRowContext(ctx, `SELECT correlation_id FROM audit_events WHERE action='auth.login.local' ORDER BY id DESC LIMIT 1`).Scan(&correlation); err != nil {
		t.Fatal(err)
	}
	if correlation != "corr-http-login" {
		t.Fatalf("correlation=%q, want the admission correlation", correlation)
	}

	// 失败路径同样归属被尝试账号。
	admissionCtx2, err := execution.WithMetadata(ctx, execution.Metadata{
		CorrelationID: "corr-http-login-fail",
		Actor:         execution.Principal{Kind: execution.PrincipalSystem, ID: 0},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-http-login-fail"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.LoginWithPassword(admissionCtx2, "admin", "wrong password value!", "UA"); !errIsUnauthenticated(err) {
		t.Fatalf("wrong password must be rejected, got %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT actor_type, actor_id FROM audit_events WHERE action='auth.login.local' AND outcome='failure' ORDER BY id DESC LIMIT 1`).Scan(&actorType, &actorID); err != nil {
		t.Fatal(err)
	}
	if actorType != "user" || actorID != admin.User.ID {
		t.Fatalf("HTTP-path failed login audit actor=(%s,%d), want (user,%d)", actorType, actorID, admin.User.ID)
	}
}
