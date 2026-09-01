package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCopySnapshotFileRejectsBodyDifferentFromSnapshotDigest(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source")
	if err := os.WriteFile(source, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("expected"))
	target := filepath.Join(directory, "target")
	err := copySnapshotFile(source, target, SnapshotFile{SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len("corrupt"))})
	if err == nil {
		t.Fatal("copySnapshotFile accepted a corrupt snapshot body")
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("failed verified copy remained at target: %v", statErr)
	}
}

func TestSnapshotLifecycleUpdateNeverWaitsForSQLiteWhileHoldingBlobLock(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	db.SetMaxOpenConns(1)
	store, err := NewStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.blobMu.Lock()
	entered := make(chan struct{})
	done := make(chan error, 1)
	root := t.TempDir()
	go func() {
		_, err := store.SnapshotAndCopy(context.Background(), filepath.Join(root, "snapshot.db"), filepath.Join(root, "artifacts"), func() error {
			close(entered)
			return nil
		}, nil)
		done <- err
	}()
	select {
	case <-entered:
		// The lifecycle update ran before blobMu acquisition. If it ran under the
		// lock, a concurrent upload holding SQLite's writer could deadlock here.
	case <-time.After(time.Second):
		store.blobMu.Unlock()
		t.Fatal("snapshot waited for blobMu before lifecycle database work")
	}
	store.blobMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
