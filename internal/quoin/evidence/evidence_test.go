package evidence

// Reader-seam tests (ADR-0006, auth-audit plan stage 2): every pure read of
// the evidence authority runs on the composition layer's real read-only pool
// (execution.OpenReadOnly, mode=ro + query_only). Until SetReader wires it,
// reads fail closed — the writer database is never a read fallback. The
// negative tests prove SQLite itself rejects writes on the read-only pool,
// including INSERT..RETURNING through QueryContext, and that only an
// OpenReadOnly-produced reader is trusted at injection time.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/execution"
	_ "modernc.org/sqlite"
)

// newServiceFixture builds the evidence service over one file-backed writer
// database with one seeded inspection-run evidence row, and returns the
// service, the writer handle and the database file path for OpenReadOnly.
func newServiceFixture(t *testing.T) (*Service, *sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO evidence(target_type,target_id,params_json,observed_at,result_json,integrity,created_at) VALUES('inspection_run',1,'{}',?,?,'complete',?)`, now, `{"value":1}`, now); err != nil {
		t.Fatal(err)
	}
	return NewService(db), db, path
}

// TestGetFailsClosedUntilReaderWired pins the no-writer-fallback rule: a row
// exists on the writer database, yet an unwired reader must refuse the read.
func TestGetFailsClosedUntilReaderWired(t *testing.T) {
	service, _, _ := newServiceFixture(t)
	if _, err := service.Get(context.Background(), 1); err == nil {
		t.Fatal("an unwired reader must fail closed instead of serving the writer pool")
	}
}

// TestSetReaderRejectsArbitraryHandles pins the injection gate: only an
// OpenReadOnly-produced execution.Reader is trusted — a raw *sql.DB, a nil
// reader or an unwired execution.Reader are all denied.
func TestSetReaderRejectsArbitraryHandles(t *testing.T) {
	service, _, path := newServiceFixture(t)
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=query_only(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if err := service.SetReader(raw); err == nil {
		t.Fatal("a raw *sql.DB must be denied even with query_only: only OpenReadOnly-produced readers are trusted")
	}
	if err := service.SetReader(nil); err == nil {
		t.Fatal("a nil reader must be denied")
	}
	if err := service.SetReader(execution.Reader{}); err == nil {
		t.Fatal("an unwired execution.Reader must be denied")
	}
}

// TestSetReaderServesPureReadsFromReadOnlyPool proves the wiring contract:
// after SetReader the same evidence row is served from the independent
// read-only pool, projected to the frozen detail shape.
func TestSetReaderServesPureReadsFromReadOnlyPool(t *testing.T) {
	service, _, path := newServiceFixture(t)
	reader, err := execution.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if err := service.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	view, err := service.Get(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != "1" || view.TargetType != "inspection_run" || view.Integrity != "complete" {
		t.Fatalf("view=%+v", view)
	}
	body, ok := view.Body.(map[string]any)
	if !ok || body["kind"] != "inline_json" {
		t.Fatalf("body=%+v", view.Body)
	}
}

// TestReadOnlyPoolRejectsInsertReturning proves the guarantee is SQLite's,
// not code discipline: the mode=ro/query_only connection rejects a write
// even through QueryContext with RETURNING, and no row lands.
func TestReadOnlyPoolRejectsInsertReturning(t *testing.T) {
	service, db, path := newServiceFixture(t)
	reader, err := execution.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := reader.QueryContext(context.Background(), `
		INSERT INTO evidence(target_type,target_id,params_json,observed_at,result_json,integrity,created_at)
		VALUES('inspection_run',99,'{}',?,?,'complete',?) RETURNING id`, now, `{"injected":1}`, now); err == nil {
		t.Fatal("the read-only pool accepted INSERT..RETURNING")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM evidence WHERE target_id=99`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("injected row survived: count=%d err=%v", count, err)
	}
	// The wired service still reads (the rejection is on writes only).
	if err := service.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
}
