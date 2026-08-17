package contracts_test

import (
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestTicket01SessionRevisionContract(t *testing.T) {
	db := openSchemaDB(t)
	insertAuthFixture(t, db)

	mustReject(t, db, `UPDATE sessions SET auth_revision_at_issue = 2 WHERE id = 1`, "other active session")
	mustReject(t, db, `UPDATE sessions SET auth_revision_at_issue = 0 WHERE id = 1`, "revision rollback")
	mustReject(t, db, `UPDATE sessions SET auth_revision_at_issue = 3 WHERE id = 1`, "revision jump")

	mustExec(t, db, `BEGIN IMMEDIATE`)
	mustExec(t, db, `UPDATE users SET password_phc = '$argon2id$new', auth_revision = 2, row_version = 2, updated_at = '2026-08-17T00:01:00Z' WHERE id = 1`)
	mustExec(t, db, `UPDATE sessions SET revoked_at = '2026-08-17T00:01:00Z' WHERE id = 2`)
	mustExec(t, db, `UPDATE sessions SET auth_revision_at_issue = 2 WHERE id = 1`)
	mustExec(t, db, `COMMIT`)

	var revision int
	if err := db.QueryRow(`SELECT auth_revision_at_issue FROM sessions WHERE id = 1`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if revision != 2 {
		t.Fatalf("current session revision = %d, want 2", revision)
	}

	mustExec(t, db, `UPDATE sessions SET revoked_at = '2026-08-17T00:02:00Z' WHERE id = 1`)
	mustReject(t, db, `UPDATE sessions SET auth_revision_at_issue = 3 WHERE id = 1`, "revoked session")
}

func openSchemaDB(t *testing.T) *sql.DB {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	schema, err := os.ReadFile(filepath.Join(root, "docs/specs/quoin-v1/contracts/sql/schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("execute full schema: %v", err)
	}
	return db
}

func insertAuthFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db, `INSERT INTO users(id,username,display_name,role,enabled,password_phc,password_change_required,password_change_required_at,created_at,updated_at) VALUES (1,'admin','Admin','admin',1,'$argon2id$old',1,'2026-08-17T00:00:00Z','2026-08-17T00:00:00Z','2026-08-17T00:00:00Z')`)
	for id, digest := range []string{strings.Repeat("01", 32), strings.Repeat("02", 32)} {
		mustExec(t, db, `INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES (?,1,?,1,'Test browser','2026-08-17T00:00:00Z','2026-08-17T00:00:00Z','2026-08-17T12:00:00Z','2026-08-24T00:00:00Z')`, id+1, mustHex(t, digest))
	}
}

func mustHex(t *testing.T, value string) []byte {
	t.Helper()
	out, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func mustReject(t *testing.T, db *sql.DB, query, name string) {
	t.Helper()
	if _, err := db.Exec(query); err == nil {
		t.Fatalf("%s was accepted", name)
	}
}
