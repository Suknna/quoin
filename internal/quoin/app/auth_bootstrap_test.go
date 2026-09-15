package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

// The unified bootstrap seeds exactly one pending built-in administrator and
// writes no credential artifacts: admin/admin goes straight into the unified
// initialization flow and the deployment network owns the first-install
// access boundary (docs/deployment.md).
func TestAuthenticationBootstrapSeedsPendingAdminOnly(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "root")
	if err := os.WriteFile(keyFile, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := bootstrap.OpenDatabase(context.Background(), dir, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service, err := auth.NewService(db.SQL)
	if err != nil {
		t.Fatal(err)
	}
	// Production installs the read-only pool before serving; tests wire
	// their only handle so pure reads run through the same seam.
	if err := service.SetReader(db.Reader); err != nil {
		t.Fatal(err)
	}
	if err := prepareAuthenticationBootstrap(context.Background(), service, db.SQL, dir); err != nil {
		t.Fatal(err)
	}
	if err := prepareAuthenticationBootstrap(context.Background(), service, db.SQL, dir); err != nil {
		t.Fatal(err)
	}
	var users, admins int
	if err := db.SQL.QueryRow(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN role='admin' THEN 1 ELSE 0 END),0) FROM users`).Scan(&users, &admins); err != nil {
		t.Fatal(err)
	}
	if users != 1 || admins != 1 {
		t.Fatalf("bootstrap duplicated state users=%d admins=%d", users, admins)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "quoin.db" && entry.Name() != "quoin.db-wal" && entry.Name() != "quoin.db-shm" && entry.Name() != "lock" && entry.Name() != ".quoin.lock" {
			t.Fatalf("bootstrap must not write credential artifacts, found %s", entry.Name())
		}
	}
	flow, _, err := service.StartAuthentication(context.Background(), "admin", "admin", "test")
	if err != nil || flow.Type != auth.FlowAdminInitialize {
		t.Fatalf("default credentials must start the unified initialization flow: %v", err)
	}
}

// A database that already has users must never be reseeded, and a
// multi-admin legacy database is rejected instead of silently repaired.
func TestAuthenticationBootstrapRefusesExistingUsers(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "root")
	if err := os.WriteFile(keyFile, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := bootstrap.OpenDatabase(context.Background(), dir, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service, err := auth.NewService(db.SQL)
	if err != nil {
		t.Fatal(err)
	}
	// Production installs the read-only pool before serving; tests wire
	// their only handle so pure reads run through the same seam.
	if err := service.SetReader(db.Reader); err != nil {
		t.Fatal(err)
	}
	if err := prepareAuthenticationBootstrap(context.Background(), service, db.SQL, dir); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := prepareAuthenticationBootstrap(context.Background(), service, db.SQL, dir); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != 1 || after != 1 {
		t.Fatalf("restart must not reseed users: before=%d after=%d", before, after)
	}
}
