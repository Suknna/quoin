package auth_test

// Offline recovery coverage (ADR-0010 §6): the password-only recovery resets
// the built-in administrator into the forced-change state, re-arms the
// bootstrap deadline when asked (the CLI path for an expired initial
// password), and stays reserved for the stopped-service CLI scope. The
// restore-isolation credential reset shares the same stage.

import (
	"context"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

func TestBeginRecoveryRejectsCallerCorrelation(t *testing.T) {
	service, _ := newAuthService(t)
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "caller", Actor: execution.Principal{Kind: execution.PrincipalUser, ID: 1},
		Source: execution.Source{Kind: execution.SourceHTTP},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.BeginRecovery(ctx, "temporary", nil); err == nil {
		t.Fatal("a caller context must never drive an offline recovery")
	}
}

func TestBeginRecoveryResetsAdminAndPreservesContacts(t *testing.T) {
	ctx := context.Background()
	service, db := newAuthService(t)
	_, _ = initializeAdminDrive(t, service)

	// Display-only contacts survive the recovery (ADR-0010: contacts carry
	// no verification binding anymore).
	session := mustSession(t, service)
	if _, _, err := service.SetUserContacts(ctx, session, auth.SetUserContactsInput{
		ClientCommandID: "cmd-contacts", Digest: auth.DigestCommand("user.set_contacts", map[string]any{"userId": session.User.ID}),
		UserID: session.User.ID, ExpectedRow: session.User.RowVersion,
		Contacts: []auth.ContactInput{{Channel: "email", Target: "admin@quoin.test"}},
	}); err != nil {
		t.Fatalf("seed admin contact: %v", err)
	}

	deadline := time.Now().UTC().Add(auth.InitialPasswordLifetime)
	if _, err := service.BeginRecovery(ctx, "Recovery passphrase 2027!", &deadline); err != nil {
		t.Fatal(err)
	}
	var initialized, required int
	var expiry string
	if err := db.QueryRow(`SELECT initialized,password_change_required,COALESCE(initial_password_expires_at,'') FROM users WHERE username='admin'`).Scan(&initialized, &required, &expiry); err != nil {
		t.Fatal(err)
	}
	if initialized != 0 || required != 1 || expiry == "" {
		t.Fatalf("recovery must force the change and arm the deadline: initialized=%d required=%d expiry=%q", initialized, required, expiry)
	}
	var contacts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM user_contacts WHERE enabled=1`).Scan(&contacts); err != nil || contacts != 1 {
		t.Fatalf("display contacts must survive: %d %v", contacts, err)
	}
	var sessions int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE revoked_at IS NULL`).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("recovery must revoke every session: %d %v", sessions, err)
	}
	// The re-armed credential logs into the restricted session and the forced
	// change clears the deadline again.
	result, err := service.LoginWithPassword(ctx, "admin", "Recovery passphrase 2027!", "UA")
	if err != nil {
		t.Fatal(err)
	}
	if !result.User.PasswordChangeRequired {
		t.Fatalf("recovery login must be restricted: %+v", result.User)
	}
	recovered, err := service.Authenticate(ctx, result.Bearer)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ChangePassword(ctx, recovered, "Recovery passphrase 2027!", "Post recovery passphrase 2028!"); err != nil {
		t.Fatal(err)
	}
	var cleared int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE username='admin' AND initial_password_expires_at IS NULL AND initialized=1`).Scan(&cleared); err != nil || cleared != 1 {
		t.Fatalf("forced change must clear the recovery state: %d %v", cleared, err)
	}
}

func TestResetAdminCredentialOnForRestoreIsolation(t *testing.T) {
	ctx := context.Background()
	service, db := newAuthService(t)
	_, _ = initializeAdminDrive(t, service)
	admin := mustSession(t, service)
	// The restore isolation runs the credential reset inside its own runner
	// transaction (maintenance.restore.enter in production); the test borrows
	// a throwaway registered operation for the same stage.
	runner := execution.NewRunner(db, nil, nil)
	op, err := runner.Register(execution.Operation{
		Name: "test.restore.isolation", Class: execution.ClassWrite, ObjectType: "maintenance",
		Authorize: func(context.Context, *execution.Tx) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, err := execution.WithMetadata(ctx, execution.Metadata{
		CorrelationID: "test-restore", Actor: execution.Principal{Kind: execution.PrincipalSystem},
		Source: execution.Source{Kind: execution.SourceCLI},
	})
	if err != nil {
		t.Fatal(err)
	}
	var credential auth.RecoveryCredential
	if _, err := execution.Execute(runCtx, runner, op, func(tx *execution.Tx) (int64, error) {
		var resetErr error
		credential, resetErr = auth.ResetAdminCredentialOn(ctx, tx, admin.User.ID, time.Now().UTC())
		return admin.User.ID, resetErr
	}, func(id int64) int64 { return id }); err != nil {
		t.Fatal(err)
	}
	if len(credential.TemporaryPassword) < 20 {
		t.Fatalf("restore isolation must mint a strong temporary credential: %d chars", len(credential.TemporaryPassword))
	}
	var initialized, required int
	if err := db.QueryRow(`SELECT initialized,password_change_required FROM users WHERE id=?`, admin.User.ID).Scan(&initialized, &required); err != nil {
		t.Fatal(err)
	}
	if initialized != 0 || required != 1 {
		t.Fatalf("restore isolation must force the change: initialized=%d required=%d", initialized, required)
	}
	// The temporary credential verifies and lands in the restricted state.
	if _, err := service.LoginWithPassword(ctx, "admin", credential.TemporaryPassword, "UA"); err != nil {
		t.Fatalf("restore temporary credential must log in: %v", err)
	}
}

// mustSession logs the initialized administrator in and returns the session.
func mustSession(t *testing.T, service *auth.Service) auth.Session {
	t.Helper()
	_, session, _ := loginPassword(t, service, "admin", fixtureAdminPassword)
	return session
}
