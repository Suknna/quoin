package artifact

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"time"
)

const (
	artifactGCBatchSize = 100
	artifactGCInterval  = 15 * time.Minute
)

// RunGC drains an overdue backlog through bounded passes. Each pass releases
// blobMu before yielding, so a newly arriving backup can acquire the shared
// artifact-storage coordinator between batches. Once no work remains it sleeps
// until the normal internal wake-up interval.
func (store *Store) RunGC(ctx context.Context) {
	for {
		more, err := store.runGarbageCollection(ctx)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(artifactGCInterval):
			}
			continue
		}
		if more {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Millisecond):
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(artifactGCInterval):
		}
	}
}

// RunGarbageCollection expires one bounded metadata batch then retries every
// orphaned blob deletion. The success gauge advances only when both phases
// completed, so a permissions failure cannot look like GC success.
func (store *Store) RunGarbageCollection(ctx context.Context) error {
	_, err := store.runGarbageCollection(ctx)
	return err
}

// runGarbageCollection returns whether another expiry batch is immediately
// available. Its caller deliberately yields before acquiring blobMu again.
func (store *Store) runGarbageCollection(ctx context.Context) (bool, error) {
	// All blob lifecycle paths reserve a database connection before blobMu.
	// CommitUpload follows the same order, avoiding the conn→blob / blob→conn
	// inversion while making install+first-reference indivisible from GC.
	conn, err := store.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	store.blobMu.Lock()
	defer store.blobMu.Unlock()
	rows, err := conn.QueryContext(ctx, `SELECT id FROM artifacts WHERE body_expired=0 AND expires_at IS NOT NULL AND expires_at<=? ORDER BY id LIMIT ?`, store.now().Format(time.RFC3339Nano), artifactGCBatchSize+1)
	if err != nil {
		return false, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		ids = append(ids, id)
	}
	if err = rows.Close(); err != nil {
		return false, err
	}
	more := len(ids) > artifactGCBatchSize
	if more {
		ids = ids[:artifactGCBatchSize]
	}
	for _, id := range ids {
		if _, err = conn.ExecContext(ctx, `UPDATE artifacts SET body_expired=1 WHERE id=? AND body_expired=0`, id); err != nil {
			return false, err
		}
	}
	orphanMore, err := store.removeOrphanBlobsOn(ctx, conn)
	if err != nil {
		return false, err
	}
	if store.gcSuccess != nil {
		store.gcSuccess(float64(store.now().UTC().Unix()))
	}
	return more || orphanMore, nil
}

// removeOrphanBlobs intentionally searches body_expired rows too. A previous
// successful state update followed by an unlink error is therefore retried,
// instead of becoming a durable-but-never-collected orphan.
func (store *Store) removeOrphanBlobsOn(ctx context.Context, conn *sql.Conn) (bool, error) {
	rows, err := conn.QueryContext(ctx, `SELECT b.sha256 FROM artifact_blobs b WHERE b.sha256 > ? AND NOT EXISTS (SELECT 1 FROM artifacts a WHERE a.blob_id=b.id AND a.body_expired=0) ORDER BY b.sha256 LIMIT ?`, store.gcOrphanCursor, artifactGCBatchSize+1)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var sha string
		if err = rows.Scan(&sha); err != nil {
			return false, err
		}
		hashes = append(hashes, sha)
	}
	if err = rows.Err(); err != nil {
		return false, err
	}
	more := len(hashes) > artifactGCBatchSize
	if more {
		hashes = hashes[:artifactGCBatchSize]
	}
	for _, sha := range hashes {
		if err = os.Remove(filepath.Join(store.dir, "blobs", sha+".blob")); err != nil && !os.IsNotExist(err) {
			return false, err
		}
	}
	if len(hashes) == 0 || !more {
		store.gcOrphanCursor = ""
	} else {
		store.gcOrphanCursor = hashes[len(hashes)-1]
	}
	return more, nil
}
