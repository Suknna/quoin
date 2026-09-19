package auth_test

// Core two-step login semantics on the canonical schema: password success
// issues no session, the consumed challenge does, self password changes keep
// the acting session while revoking the others, and the session revision
// guard still rejects invalid transitions.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
)

func TestFirstAdminTwoStepLoginAndSelfPasswordChange(t *testing.T) {
	ctx := context.Background()
	service, sender, db := newFlowService(t)
	bootstrapPendingAdmin(t, service)
	initializeAdminDrive(t, service, sender)

	// Username normalization still applies at the flow entry.
	firstUser, _, firstBearer := flowLogin(t, service, sender, "ADMIN", fixtureAdminPassword)
	if !firstUser.Initialized || firstUser.Role != "admin" {
		t.Fatalf("unexpected first login user: %+v", firstUser)
	}
	_, _, secondBearer := flowLogin(t, service, sender, "admin", fixtureAdminPassword)

	const replacement = "A new private passphrase for Quoin 2027!"
	secondSession, err := service.Authenticate(ctx, secondBearer)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ChangePassword(ctx, secondSession, fixtureAdminPassword, replacement); err != nil {
		t.Fatalf("self password change: %v", err)
	}
	// The acting session survives its own password change and advances.
	current, err := service.Authenticate(ctx, secondBearer)
	if err != nil {
		t.Fatalf("current bearer must survive self password change: %v", err)
	}
	// Initialization set the formal password (revision 2); the self change
	// advances exactly once more.
	if current.User.PasswordChangeRequired || current.User.AuthRevision != 3 {
		t.Fatalf("password change projection did not advance: %+v", current.User)
	}
	// Every other session is revoked by the revision change.
	if _, err := service.Authenticate(ctx, firstBearer); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("other sessions must die on a password change: %v", err)
	}
	// The old password stops verifying; in-flight challenges with it die.
	if _, _, err := service.StartLogin(ctx, "admin", fixtureAdminPassword, "UA"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("the old password must stop verifying, got %v", err)
	}
	_, _, newBearer := flowLogin(t, service, sender, "admin", replacement)
	newSession, err := service.Authenticate(ctx, newBearer)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Logout(ctx, newSession); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(ctx, newBearer); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("logout did not revoke the current session: %v", err)
	}
	var auditCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount < 6 {
		t.Fatalf("expected bootstrap/init/verify/login/password/logout audit events, got %d", auditCount)
	}
}

func TestSessionRevisionGuardRejectsInvalidTransitions(t *testing.T) {
	ctx := context.Background()
	service, sender, db := newFlowService(t)
	bootstrapPendingAdmin(t, service)
	initializeAdminDrive(t, service, sender)
	_, session, _ := flowLogin(t, service, sender, "admin", fixtureAdminPassword)
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
