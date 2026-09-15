package execution

import (
	"context"
	"testing"
)

func TestReadCapabilityCannotChangeConnectionAuthority(t *testing.T) {
	dir := t.TempDir()
	newReaderTestWriter(t, dir)
	reader := newReaderTestPool(t, dir)
	for _, query := range []string{
		`PRAGMA query_only=0`,
		`PRAGMA writable_schema=1`,
		`PRAGMA journal_mode=WAL`,
		`ATTACH DATABASE ':memory:' AS writable`,
		`SELECT 1; PRAGMA query_only=0`,
		`BEGIN IMMEDIATE`,
	} {
		t.Run(query, func(t *testing.T) {
			if _, err := reader.QueryContext(context.Background(), query); err == nil {
				t.Fatal("reader must not expose connection control")
			}
			if err := reader.QueryRowContext(context.Background(), query).Scan(new(any)); err == nil {
				t.Fatal("single-row reader must not expose connection control")
			}
		})
	}
	snapshot, err := reader.BeginSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Rollback()
	if err := snapshot.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM quoin_items`).Scan(new(int)); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.QueryContext(context.Background(), `INSERT INTO quoin_items(name) VALUES('no') RETURNING id`); err == nil {
		t.Fatal("snapshot must retain SQLite readonly enforcement")
	}
	if _, err := snapshot.QueryContext(context.Background(), `COMMIT`); err == nil {
		t.Fatal("snapshot queries must not control transaction lifetime")
	}
	if err := snapshot.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.QueryContext(context.Background(), `SELECT 1`); err == nil {
		t.Fatal("closed snapshot must not remain usable")
	}
}
