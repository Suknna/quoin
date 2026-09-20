package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

// The unified bootstrap seeds exactly one pending built-in administrator
// whose initial password is randomly generated and written to the 0600
// initial-admin-password file inside the data directory (ADR-0010). The
// credential logs into the restricted session; admin/admin never exists.
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
	if err := service.SetReader(db.Reader); err != nil {
		t.Fatal(err)
	}
	if err := prepareAuthenticationBootstrap(context.Background(), service, dir); err != nil {
		t.Fatal(err)
	}
	if err := prepareAuthenticationBootstrap(context.Background(), service, dir); err != nil {
		t.Fatal(err)
	}
	var users, admins int
	if err := db.SQL.QueryRow(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN role='admin' THEN 1 ELSE 0 END),0) FROM users`).Scan(&users, &admins); err != nil {
		t.Fatal(err)
	}
	if users != 1 || admins != 1 {
		t.Fatalf("bootstrap duplicated state users=%d admins=%d", users, admins)
	}
	// The bootstrap credential file exists, is mode 0600, and carries the
	// password that logs into the restricted session.
	path := initialPasswordPath(dir)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("initial password file missing: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("initial password file mode = %v", info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	password := strings.TrimSpace(string(raw))
	result, err := service.LoginWithPassword(context.Background(), "admin", password, "test")
	if err != nil {
		t.Fatalf("the file credential must log in: %v", err)
	}
	if !result.User.PasswordChangeRequired {
		t.Fatalf("the bootstrap login must stay restricted: %+v", result.User)
	}
	// admin/admin never verifies.
	if _, err := service.LoginWithPassword(context.Background(), "admin", "admin", "test"); err == nil {
		t.Fatal("the public default credential must not exist")
	}
	// The deadline is stamped on the users row.
	var expiry string
	if err := db.SQL.QueryRow(`SELECT initial_password_expires_at FROM users WHERE username='admin'`).Scan(&expiry); err != nil || expiry == "" {
		t.Fatalf("bootstrap must stamp the deadline: %q %v", expiry, err)
	}
	deadline, err := time.Parse(time.RFC3339Nano, expiry)
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(deadline) < 23*time.Hour {
		t.Fatalf("deadline must be 24h out, got %v", time.Until(deadline))
	}
}

// A database that already has users must never be reseeded; a completed
// initialization also clears the lingering credential file on the next boot.
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
	if err := service.SetReader(db.Reader); err != nil {
		t.Fatal(err)
	}
	if err := prepareAuthenticationBootstrap(context.Background(), service, dir); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// Complete the initialization, then boot again: the seed stays single and
	// the credential file is cleared.
	raw, err := os.ReadFile(initialPasswordPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	initial := strings.TrimSpace(string(raw))
	result, err := service.LoginWithPassword(context.Background(), "admin", initial, "test")
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.Authenticate(context.Background(), result.Bearer)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ChangePassword(context.Background(), session, initial, "Post bootstrap passphrase 2027!"); err != nil {
		t.Fatal(err)
	}
	if err := prepareAuthenticationBootstrap(context.Background(), service, dir); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != 1 || after != 1 {
		t.Fatalf("restart must not reseed users: before=%d after=%d", before, after)
	}
	if _, err := os.Stat(initialPasswordPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("completed initialization must clear the credential file: %v", err)
	}
}
