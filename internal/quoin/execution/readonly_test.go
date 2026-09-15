package execution

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenReadOnlyEnforcesSQLiteReadOnlyMode(t *testing.T) {
	// Build a real database file, then reopen it read-only.
	path := filepath.Join(t.TempDir(), "quoin.db")
	writeDB, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeDB.Exec(businessTableSchema); err != nil {
		t.Fatal(err)
	}
	if err := writeDB.Close(); err != nil {
		t.Fatal(err)
	}

	readOnly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()

	var count int
	if err := readOnly.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM quoin_items`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("count=%d, want empty table", count)
	}
	// SQLite itself must reject writes on the mode=ro connection.
	if _, err := readOnly.QueryContext(context.Background(), `INSERT INTO quoin_items(name) VALUES('nope') RETURNING id`); err == nil || !strings.Contains(err.Error(), "readonly") {
		t.Fatalf("write err=%v, want SQLite readonly rejection", err)
	}
}

func TestOpenReadOnlyRejectsMissingDatabase(t *testing.T) {
	if _, err := OpenReadOnly(filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Fatal("missing database must fail")
	}
}
