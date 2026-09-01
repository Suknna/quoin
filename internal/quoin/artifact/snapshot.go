package artifact

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// SnapshotFile is one immutable body copied into a backup manifest.
type SnapshotFile struct {
	SHA256    string
	SizeBytes int64
}

// SnapshotAndCopy makes the SQLite snapshot and copies precisely the
// non-expired bodies referenced by that snapshot while the single artifact
// storage coordinator excludes GC. A later upload cannot appear only in the
// archive, and GC cannot remove a body after the snapshot selected it.
func (store *Store) SnapshotAndCopy(ctx context.Context, databaseDestination, artifactDestination string, afterSnapshot func() error, beforeCopy func(databaseSize int64, files []SnapshotFile) error) ([]SnapshotFile, error) {
	// The SQLite snapshot must be established before taking blobMu. Uploads can
	// hold SQLite's writer while waiting for blobMu; taking the mutex first
	// would invert that order and deadlock a one-connection store. A GC that
	// expires a selected body in the small gap can only make this backup fail
	// during the verified copy below—never publish an incomplete backup set.
	quoted := strings.ReplaceAll(databaseDestination, "'", "''")
	if _, err := store.db.ExecContext(ctx, "VACUUM INTO '"+quoted+"'"); err != nil {
		return nil, fmt.Errorf("snapshot database: %w", err)
	}
	// Never acquire SQLite while holding blobMu. Upload commits take SQLite's
	// writer then blobMu; moving this lifecycle update before blobMu preserves
	// the one global lock order and prevents a single-connection deadlock.
	if afterSnapshot != nil {
		if err := afterSnapshot(); err != nil {
			return nil, err
		}
	}
	store.blobMu.Lock()
	defer store.blobMu.Unlock()
	snapshot, err := sql.Open("sqlite", "file:"+databaseDestination+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer snapshot.Close()
	rows, err := snapshot.QueryContext(ctx, `SELECT b.sha256,b.size_bytes FROM artifact_blobs b WHERE EXISTS (SELECT 1 FROM artifacts a WHERE a.blob_id=b.id AND a.body_expired=0) ORDER BY b.sha256`)
	if err != nil {
		return nil, err
	}
	var files []SnapshotFile
	for rows.Next() {
		var file SnapshotFile
		if err = rows.Scan(&file.SHA256, &file.SizeBytes); err != nil {
			rows.Close()
			return nil, err
		}
		files = append(files, file)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if beforeCopy != nil {
		info, err := os.Stat(databaseDestination)
		if err != nil {
			return nil, err
		}
		if err = beforeCopy(info.Size(), files); err != nil {
			return nil, err
		}
	}
	if err = os.MkdirAll(artifactDestination, 0o700); err != nil {
		return nil, err
	}
	for _, file := range files {
		if err = copySnapshotFile(filepath.Join(store.dir, "blobs", file.SHA256+".blob"), filepath.Join(artifactDestination, file.SHA256+".blob"), file); err != nil {
			return nil, fmt.Errorf("copy artifact %s: %w", file.SHA256, err)
		}
	}
	return files, nil
}

func copySnapshotFile(source, target string, expected SnapshotFile) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(output, hash), input)
	if err == nil && written != expected.SizeBytes {
		err = fmt.Errorf("copied artifact size %d differs from snapshot %d", written, expected.SizeBytes)
	}
	if err == nil && hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 {
		err = fmt.Errorf("copied artifact hash differs from snapshot")
	}
	if syncErr := output.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := output.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(target)
	}
	return err
}
