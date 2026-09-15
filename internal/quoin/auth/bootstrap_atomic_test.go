package auth_test

import (
	"context"
	"testing"
)

func TestBootstrapRetentionSharesAdminAndAuditTransaction(t *testing.T) {
	service, db := newAuthService(t)
	if _, err := db.Exec(`CREATE TRIGGER fail_bootstrap_audit BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if created, err := service.EnsureBootstrapAdmin(context.Background(), 12); err == nil || created {
		t.Fatalf("audit failure must reject bootstrap: created=%v err=%v", created, err)
	}
	var users, months int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT retention_months FROM audit_retention WHERE id=1`).Scan(&months); err != nil {
		t.Fatal(err)
	}
	if users != 0 || months != 6 {
		t.Fatalf("failed bootstrap leaked changes: users=%d months=%d", users, months)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_bootstrap_audit`); err != nil {
		t.Fatal(err)
	}
	if created, err := service.EnsureBootstrapAdmin(context.Background(), 12); err != nil || !created {
		t.Fatalf("bootstrap retry: created=%v err=%v", created, err)
	}
	if created, err := service.EnsureBootstrapAdmin(context.Background(), 18); err != nil || created {
		t.Fatalf("bootstrap repeat: created=%v err=%v", created, err)
	}
	if err := db.QueryRow(`SELECT retention_months FROM audit_retention WHERE id=1`).Scan(&months); err != nil {
		t.Fatal(err)
	}
	var events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if months != 12 || events != 1 {
		t.Fatalf("bootstrap must seed once and preserve settings on restart: months=%d events=%d", months, events)
	}
}
