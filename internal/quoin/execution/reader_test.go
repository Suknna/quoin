package execution

// The composition read seam: Runner.Reader serves only an explicitly injected
// real read-only pool (bootstrap opens it mode=ro with query_only), fails
// closed until wired, and never owns the injected pool's lifetime.

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
)

const readerItemsSchema = `CREATE TABLE quoin_items (
  id   INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  name TEXT NOT NULL
) STRICT;`

// newReaderTestWriter opens the writer pool over one temp-dir file with a
// single connection (Quoin's writer shape) and creates the business table.
func newReaderTestWriter(t *testing.T, dir string) *sql.DB {
	t.Helper()
	writer, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "quoin.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	writer.SetMaxOpenConns(1)
	if _, err := writer.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(readerItemsSchema); err != nil {
		t.Fatal(err)
	}
	return writer
}

// newReaderTestPool opens a second pool over the same database file in real
// SQLite read-only mode, exactly like the composition layer's bootstrap.
func newReaderTestPool(t *testing.T, dir string) Reader {
	t.Helper()
	reader, err := OpenReadOnly(filepath.Join(dir, "quoin.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	return reader
}

func TestReaderFailsClosedWithoutInjectedPool(t *testing.T) {
	runner := NewRunner(newReaderTestWriter(t, t.TempDir()), nil, nil)
	if _, err := runner.Reader().QueryContext(context.Background(), `SELECT 1`); err == nil {
		t.Fatal("an unwired reader must fail closed instead of wrapping the write pool")
	}
	if err := runner.Reader().QueryRowContext(context.Background(), `SELECT 1`).Scan(new(any)); err == nil {
		t.Fatal("an unwired reader row must fail closed")
	}
	if err := runner.SetReader(nil); err == nil {
		t.Fatal("a nil reader must be rejected")
	}
	// Wiring the runner's own write pool as the reader would silently
	// re-open the bypass the seam exists to close.
	if err := runner.SetReader(runner.db); err == nil {
		t.Fatal("wiring the write pool as reader must be rejected")
	}
	// An arbitrary SECOND writable handle is equally rejected: the injection
	// builder enforces provable read-only (PRAGMA query_only=1), not just
	// "some other pool".
	second, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "second.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if err := runner.SetReader(second); err == nil {
		t.Fatal("a writable second handle must be rejected as reader")
	}
}

func TestReaderRejectsPoolWithOnlyOneReadonlyConnection(t *testing.T) {
	dir := t.TempDir()
	writer := newReaderTestWriter(t, dir)
	pool, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "quoin.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	pool.SetMaxOpenConns(2)
	pool.SetMaxIdleConns(2)
	writable, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer writable.Close()
	probe, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probe.ExecContext(context.Background(), `PRAGMA query_only=1`); err != nil {
		t.Fatal(err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	var queryOnly int
	if err := pool.QueryRow(`PRAGMA query_only`).Scan(&queryOnly); err != nil || queryOnly != 1 {
		t.Fatalf("fixture must fool a connection-local probe: value=%d err=%v", queryOnly, err)
	}
	if err := NewRunner(writer, nil, nil).SetReader(pool); err == nil {
		t.Fatal("a partially readonly arbitrary pool must not become a trusted reader")
	}
	if _, err := writable.ExecContext(context.Background(), `INSERT INTO quoin_items(name) VALUES('still writable')`); err != nil {
		t.Fatalf("fixture must retain its second writable connection: %v", err)
	}
}

func TestReaderCanComposeWithoutExposingItsPool(t *testing.T) {
	dir := t.TempDir()
	writer := newReaderTestWriter(t, dir)
	parent := NewRunner(writer, nil, nil)
	child := NewRunner(writer, nil, nil)
	if err := child.SetReader(parent.Reader()); err == nil {
		t.Fatal("an unwired parent reader must not authorize child reads")
	}
	if err := parent.SetReader(newReaderTestPool(t, dir)); err != nil {
		t.Fatal(err)
	}
	if err := child.SetReader(parent.Reader()); err != nil {
		t.Fatal(err)
	}
	if err := child.Reader().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM quoin_items`).Scan(new(int)); err != nil {
		t.Fatal(err)
	}
	if _, err := child.Reader().QueryContext(context.Background(), `INSERT INTO quoin_items(name) VALUES('smuggled') RETURNING id`); err == nil {
		t.Fatal("child reader must retain SQLite write protection")
	}
}

func TestInjectedReadOnlyPoolRejectsWrites(t *testing.T) {
	dir := t.TempDir()
	writer := newReaderTestWriter(t, dir)
	if _, err := writer.Exec(`INSERT INTO quoin_items(name) VALUES('readable')`); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(writer, nil, nil)
	if err := runner.SetReader(newReaderTestPool(t, dir)); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := runner.Reader().QueryRowContext(context.Background(), `SELECT name FROM quoin_items LIMIT 1`).Scan(&name); err != nil {
		t.Fatalf("read through the injected pool: %v", err)
	}
	if name != "readable" {
		t.Fatalf("unexpected row: %q", name)
	}
	// The negative contract: the exposed reader cannot write, not even
	// through QueryContext with RETURNING.
	if _, err := runner.Reader().QueryContext(context.Background(),
		`INSERT INTO quoin_items(name) VALUES('smuggled') RETURNING id`); err == nil {
		t.Fatal("QueryContext INSERT RETURNING through the read-only pool must fail")
	} else if !strings.Contains(err.Error(), "readonly") && !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("the failure must be the SQLite read-only rejection, got %v", err)
	}
	var count int
	if err := runner.Reader().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM quoin_items`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("the smuggled insert must not exist: count=%d", count)
	}
}

// Retained-reader reuse: the injected pool is a different handle than the
// writer, so a read issued while the runner holds its single writer
// connection never deadlocks, and finishing the execution never closes the
// injected pool.
func TestReaderUsableDuringAndAfterWriterTransaction(t *testing.T) {
	dir := t.TempDir()
	writer := newReaderTestWriter(t, dir)
	if _, err := writer.Exec(`INSERT INTO quoin_items(name) VALUES('seed')`); err != nil {
		t.Fatal(err)
	}
	reader := newReaderTestPool(t, dir)

	registry := NewRegistry()
	op, err := registry.Register(Operation{
		Name: "item.create", Class: ClassWrite, ObjectType: "quoin_item",
		Authorize: func(context.Context, *Tx) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(writer, registry, nil)
	if err := runner.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	meta, err := WithMetadata(context.Background(), Metadata{
		CorrelationID: "reader-test",
		Actor:         Principal{Kind: PrincipalUser, ID: 1},
		Source:        Source{Kind: SourceInternal},
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := Execute(meta, runner, op, func(tx *Tx) (int64, error) {
			// The business stage reads through the injected read-only pool
			// while the writer connection is held by this transaction: the
			// split pools make this a non-blocking read, and the read only
			// observes committed state.
			var count int
			if err := runner.Reader().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM quoin_items`).Scan(&count); err != nil {
				return 0, err
			}
			if count != 1 {
				return 0, errors.New("reader must observe the committed seed")
			}
			if _, err := tx.ExecContext(context.Background(), `INSERT INTO quoin_items(name) VALUES('tx')`); err != nil {
				return 0, err
			}
			return 2, nil
		}, func(id int64) int64 { return id })
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reader read during held writer transaction deadlocked or failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reader read during held writer transaction deadlocked")
	}

	// After the execution the reader stays usable and observes the commit —
	// the runner never closed the injected pool (its ownership stays with
	// the composition layer).
	var count int
	if err := runner.Reader().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM quoin_items`).Scan(&count); err != nil {
		t.Fatalf("reader must stay usable after executions: %v", err)
	}
	if count != 2 {
		t.Fatalf("reader must observe the committed transaction: count=%d", count)
	}
}
