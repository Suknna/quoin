package app

import (
	"database/sql"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

func fixtureReadOnlyPool(t *testing.T, db *sql.DB) execution.Reader {
	t.Helper()
	reader, err := execution.OpenReadOnly(dbPathOf(t, db))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	return reader
}

func dbPathOf(t *testing.T, db *sql.DB) string {
	t.Helper()
	var file string
	if err := db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&file); err != nil {
		t.Fatal(err)
	}
	return file
}
