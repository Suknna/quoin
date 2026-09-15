package backup

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// TestReadOnlyReaderRejectsWrites pins the structural guarantee of the read
// split: the reader is a real SQLite mode=ro pool, so any write a query path
// could attempt fails in the database itself, while public queries on the
// reader still project state committed through the write side.
func TestReadOnlyReaderRejectsWrites(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	if _, err := service.reader.QueryContext(context.Background(), `UPDATE backup_settings SET enabled=0 WHERE id=1 RETURNING id`); err == nil {
		t.Fatal("read-only reader accepted a write")
	}
	enabled := false
	if _, err := service.UpdateSettingsCommand(newCommandContext(t, 1), 1, 1, "reader-split-command", &enabled, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	value, err := service.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if value.Enabled {
		t.Fatalf("settings enabled=%t on the reader, want the committed false from the write side", value.Enabled)
	}
	if _, err := service.Get(context.Background(), 1); err == nil {
		t.Fatal("reader Get unexpectedly returned a nonexistent backup")
	}
}

// TestCloseIsOwnershipAwareAndIdempotent pins the shutdown contract: Close
// releases only the service-owned mode=ro pool, never the caller-owned write
// pool or a reader attached through SetReader, and a second Close is a no-op.
func TestCloseIsOwnershipAwareAndIdempotent(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	owned := service.reader
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := owned.QueryContext(context.Background(), `SELECT 1`); err == nil {
		t.Fatal("Close left the owned read-only pool open")
	}
	if err := service.Close(); err != nil {
		t.Fatalf("repeated Close must be a no-op, got %v", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("Close must never close the caller-owned write pool: %v", err)
	}

	// A shared reader attached after Close stays with its owner across Close.
	shared, err := execution.OpenReadOnly(filepath.Join(service.config.DataDirectory, "quoin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	if err := service.SetReader(shared); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if err := shared.PingContext(context.Background()); err != nil {
		t.Fatalf("Close closed the attached shared reader: %v", err)
	}
}

// TestSetReaderAttachesSharedPool pins the wiring contract: SetReader swaps
// the self-opened mode=ro pool for the process's shared read-only pool,
// closing the replaced one, and the public queries continue on the attached
// pool while writes keep their private runner pool.
func TestSetReaderAttachesSharedPool(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	if err := service.SetReader(execution.Reader{}); err == nil {
		t.Fatal("nil reader accepted")
	}
	selfOpened := service.reader
	shared, err := execution.OpenReadOnly(filepath.Join(service.config.DataDirectory, "quoin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	if err := service.SetReader(shared); err != nil {
		t.Fatal(err)
	}
	if service.reader != shared || service.selfReader {
		t.Fatalf("shared reader not attached: selfReader=%t", service.selfReader)
	}
	if _, err := selfOpened.QueryContext(context.Background(), `SELECT 1`); err == nil {
		t.Fatal("replaced self-opened reader pool was not closed")
	}
	queued, err := service.QueueManual(newCommandContext(t, 1), 1, "reader-shared-command")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(context.Background(), mustID(t, queued.ID)); err != nil {
		t.Fatalf("shared-pool read: %v", err)
	}
	value, err := service.Settings(context.Background())
	if err != nil || !value.Enabled {
		t.Fatalf("settings=%+v err=%v, want true via the shared pool", value, err)
	}
}
