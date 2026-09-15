package execution

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

// OpenReadOnly opens an existing SQLite database with mode=ro and query_only.
// The caller owns the pool; SQLite rejects writes even through QueryContext.
func OpenReadOnly(databasePath string) (Reader, error) {
	if _, err := os.Stat(databasePath); err != nil {
		return Reader{}, fmt.Errorf("execution: inspect database for read-only open: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: databasePath, RawQuery: "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return Reader{}, fmt.Errorf("execution: open read-only database: %w", err)
	}
	db.SetMaxOpenConns(4)
	return Reader{db: db}, nil
}

func guardReadStatement(query string) error {
	statements, err := sqlStatements(query)
	if err != nil {
		return err
	}
	if len(statements) == 1 && firstSQLWord(statements[0]) == "PRAGMA" {
		parts := strings.Fields(strings.ToLower(strings.TrimSpace(statements[0])))
		if len(parts) == 2 {
			switch parts[1] {
			case "query_only", "page_count", "page_size", "foreign_keys", "recursive_triggers", "journal_mode", "synchronous", "freelist_count", "database_list", "busy_timeout", "integrity_check", "foreign_key_check", "schema_version", "user_version":
				return nil
			}
		}
		return errors.New("execution: only read-only diagnostic pragmas are allowed")
	}
	return guardStatement(query)
}
