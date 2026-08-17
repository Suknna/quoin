package auth_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

func TestFirstAdminLoginPasswordChangeAndLogout(t *testing.T) {
	ctx := context.Background()
	config := testConfig(t)
	if created, err := bootstrap.BootstrapSecrets(config); err != nil || !created {
		t.Fatalf("bootstrap secrets: created=%v err=%v", created, err)
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	service, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	const temporary = "Correct horse battery staple 2026!"
	created, err := service.CreateFirstAdmin(ctx, "Admin", "Quoin Admin", temporary)
	if err != nil || !created {
		t.Fatalf("create admin: created=%v err=%v", created, err)
	}
	if created, err := service.CreateFirstAdmin(ctx, "other", "Other", "Another sufficiently long password 2026!"); err != nil || created {
		t.Fatalf("second bootstrap must be idempotent: created=%v err=%v", created, err)
	}
	first, _, err := service.Login(ctx, "ADMIN", temporary, "Mozilla/5.0 Chrome Linux")
	if err != nil {
		t.Fatal(err)
	}
	if !first.User.PasswordChangeRequired || first.User.AuthRevision != 1 || first.User.Role != "admin" {
		t.Fatalf("unexpected first login user: %+v", first.User)
	}
	second, _, err := service.Login(ctx, "admin", temporary, "Mozilla/5.0 Firefox Linux")
	if err != nil {
		t.Fatal(err)
	}
	current, err := service.Authenticate(ctx, first.Bearer)
	if err != nil {
		t.Fatal(err)
	}
	const replacement = "A new private passphrase for Quoin 2027!"
	if err := service.ChangePassword(ctx, current, temporary, replacement); err != nil {
		t.Fatal(err)
	}
	current, err = service.Authenticate(ctx, first.Bearer)
	if err != nil {
		t.Fatalf("current bearer must survive self password change: %v", err)
	}
	if current.User.PasswordChangeRequired || current.User.AuthRevision != 2 {
		t.Fatalf("password change projection did not advance: %+v", current.User)
	}
	if _, err := service.Authenticate(ctx, second.Bearer); err == nil {
		t.Fatal("other session survived password change")
	}
	if _, _, err := service.Login(ctx, "admin", temporary, "test"); err == nil {
		t.Fatal("old password still authenticates")
	}
	newLogin, _, err := service.Login(ctx, "admin", replacement, "test")
	if err != nil {
		t.Fatal(err)
	}
	newSession, err := service.Authenticate(ctx, newLogin.Bearer)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Logout(ctx, newSession); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(ctx, newLogin.Bearer); err == nil {
		t.Fatal("logout did not revoke current session")
	}
	var auditCount int
	if err := database.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount < 6 {
		t.Fatalf("expected bootstrap/login/password/logout audit events, got %d", auditCount)
	}
}

func TestSessionRevisionGuardRejectsInvalidTransitions(t *testing.T) {
	ctx := context.Background()
	config := testConfig(t)
	_, _ = bootstrap.BootstrapSecrets(config)
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	service, _ := auth.NewService(database.SQL)
	const password = "Correct horse battery staple 2026!"
	if _, err := service.CreateFirstAdmin(ctx, "admin", "Admin", password); err != nil {
		t.Fatal(err)
	}
	login, _, err := service.Login(ctx, "admin", password, "test")
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.Authenticate(ctx, login.Bearer)
	if err != nil {
		t.Fatal(err)
	}
	for name, query := range map[string]string{
		"skip":                 `UPDATE sessions SET auth_revision_at_issue=auth_revision_at_issue+2 WHERE id=?`,
		"rewind":               `UPDATE sessions SET auth_revision_at_issue=auth_revision_at_issue-1 WHERE id=?`,
		"without user advance": `UPDATE sessions SET auth_revision_at_issue=auth_revision_at_issue+1 WHERE id=?`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := database.SQL.ExecContext(ctx, query, session.ID); err == nil {
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
		RuntimeTLSPrivateKeyFile: filepath.Join(secrets, "runtime-tls.key"), SteleServiceTokenFile: filepath.Join(secrets, "stele-service-token"),
	}
}
