package artifact

// Write-capability seal tests (ADR-0006): the Executor parameter of the
// store's caller-transaction helpers is the concrete runner-owned
// *execution.Tx alias. A raw pool connection can neither satisfy nor wrap it,
// so attachment registration, evidence materialization and inline commits can
// never run outside an audited runner transaction, and the store's pure-read
// pool rejects writes at the SQLite level.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// TestRawConnCannotSatisfyExecutor pins the seal in both directions: the
// runner's guarded transaction satisfies the Executor alias, while a raw
// *sql.Conn does not — the alias is concrete, so no wrapper can be embedded
// around it either.
func TestRawConnCannotSatisfyExecutor(t *testing.T) {
	var _ execution.Executor = (*execution.Tx)(nil)
	var raw any = &sql.Conn{}
	if _, ok := raw.(execution.Executor); ok {
		t.Fatal("a raw *sql.Conn must not satisfy execution.Executor")
	}
}

// TestStoreReaderRejectsInsertReturning proves the store's wired pure-read
// pool is a real mode=ro/query_only connection: SQLite itself rejects a write
// even through QueryContext with RETURNING (the store never reads through the
// writer, and nothing can mutate through the reader).
func TestStoreReaderRejectsInsertReturning(t *testing.T) {
	_, store := newTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.reads().QueryContext(context.Background(), `
		INSERT INTO artifact_blobs(sha256,size_bytes,storage_key,created_at)
		VALUES('injected',1,'blobs/injected.blob',?) RETURNING id`, now); err == nil {
		t.Fatal("the store's read-only pool accepted INSERT..RETURNING")
	}
}
