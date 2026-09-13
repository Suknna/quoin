package upgrade

import (
	"context"
	"errors"
	"testing"
)

func TestDeclarationPredecessorSchemaPreflightIsReadOnlyAndChecksLedger(t *testing.T) {
	db := declarationCutoverFixture(t)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}
	var result PreflightResult
	if err := verifySchemaGate(context.Background(), conn, &result); err != nil {
		t.Fatal(err)
	}
	if result.MigrationHistory != 1 {
		t.Fatalf("history=%d", result.MigrationHistory)
	}
	if _, err := conn.ExecContext(context.Background(), `PRAGMA query_only=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES('unknown',?, '2026-09-12T00:00:00Z')`, digest64()); err != nil {
		t.Fatal(err)
	}
	if err := verifySchemaGate(context.Background(), conn, &result); !errors.Is(err, ErrSchemaHistoryPresent) {
		t.Fatalf("unexpected ledger accepted: %v", err)
	}
}
