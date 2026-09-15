package artifact

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

const (
	artifactGCBatchSize = 100
	artifactGCInterval  = 15 * time.Minute
)

// RunGC drains an overdue backlog through bounded passes, releasing storage
// coordination between batches so uploads and backups can make progress.
func (store *Store) RunGC(ctx context.Context) {
	for {
		more, err := store.runGarbageCollection(ctx)
		delay := artifactGCInterval
		if err == nil && more {
			delay = time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (store *Store) RunGarbageCollection(ctx context.Context) error {
	_, err := store.runGarbageCollection(ctx)
	return err
}

func (store *Store) runGarbageCollection(ctx context.Context) (bool, error) {
	store.gcMu.Lock()
	defer store.gcMu.Unlock()
	ctx, err := store.systemTaskContext(ctx)
	if err != nil {
		return false, err
	}
	expiredMore, err := execution.Execute(ctx, store.runner, store.opGCCollect, func(tx *execution.Tx) (bool, error) {
		return store.expireOverdueBodiesOn(ctx, tx)
	}, func(bool) int64 { return 0 })
	if err != nil && !errors.Is(err, execution.ErrNoTransition) {
		return false, err
	}
	orphanMore, err := store.collectOrphanBlobs(ctx)
	if err != nil {
		return false, err
	}
	if store.gcSuccess != nil {
		store.gcSuccess(float64(store.now().UTC().Unix()))
	}
	return expiredMore || orphanMore, nil
}

func (store *Store) expireOverdueBodiesOn(ctx context.Context, tx execution.Executor) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM artifacts WHERE body_expired=0 AND expires_at IS NOT NULL AND expires_at<=? ORDER BY id LIMIT ?`, store.now().Format(time.RFC3339Nano), artifactGCBatchSize+1)
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
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	if err = rows.Close(); err != nil {
		return false, err
	}
	if len(ids) == 0 {
		return false, execution.ErrNoTransition
	}
	more := len(ids) > artifactGCBatchSize
	if more {
		ids = ids[:artifactGCBatchSize]
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `UPDATE artifacts SET body_expired=1 WHERE id=? AND body_expired=0`, id); err != nil {
			return false, err
		}
	}
	return more, nil
}

type orphanBlob struct {
	ID     int64
	SHA256 string
}

// Blob rows outlive their files. Remaining files are retried after a failed
// pass or restart; an unmatched begin event means removal was not confirmed,
// never that a missing file was proven to have been removed by this process.
func (store *Store) collectOrphanBlobs(ctx context.Context) (bool, error) {
	rows, err := store.reads().QueryContext(ctx, `SELECT b.id,b.sha256 FROM artifact_blobs b WHERE b.sha256 > ? AND NOT EXISTS (SELECT 1 FROM artifacts a WHERE a.blob_id=b.id AND a.body_expired=0) ORDER BY b.sha256 LIMIT ?`, store.gcOrphanCursor, artifactGCBatchSize+1)
	if err != nil {
		return false, err
	}
	var blobs []orphanBlob
	for rows.Next() {
		var blob orphanBlob
		if err := rows.Scan(&blob.ID, &blob.SHA256); err != nil {
			rows.Close()
			return false, err
		}
		blobs = append(blobs, blob)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	more := len(blobs) > artifactGCBatchSize
	if more {
		blobs = blobs[:artifactGCBatchSize]
	}
	for _, blob := range blobs {
		if _, err := os.Stat(store.blobPath(blob.SHA256)); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return false, err
		}
		if err := store.collectBlob(ctx, blob); err != nil {
			return false, err
		}
	}
	store.gcOrphanCursor = ""
	if more {
		store.gcOrphanCursor = blobs[len(blobs)-1].SHA256
	}
	return more, nil
}

func (store *Store) collectBlob(ctx context.Context, blob orphanBlob) error {
	// A fresh pair identifies this physical attempt, including a later
	// re-upload of the same digest; old successful intent is never replayed.
	correlation, err := execution.NewCorrelationID()
	if err != nil {
		return err
	}
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	meta.CorrelationID = correlation
	ctx, err = execution.ReplaceMetadata(ctx, meta)
	if err != nil {
		return err
	}
	_, err = execution.Execute(ctx, store.runner, store.opGCIntent, func(tx *execution.Tx) (int64, error) {
		store.blobMu.Lock()
		defer store.blobMu.Unlock()
		eligible, err := orphanEligible(ctx, tx, blob)
		if err != nil {
			return 0, err
		}
		if !eligible {
			return 0, execution.ErrNoTransition
		}
		if _, err := os.Stat(store.blobPath(blob.SHA256)); os.IsNotExist(err) {
			return 0, execution.ErrNoTransition
		} else if err != nil {
			return 0, err
		}
		return blob.ID, nil
	}, func(id int64) int64 { return id })
	if errors.Is(err, execution.ErrNoTransition) {
		return nil
	}
	if err != nil {
		return err
	}

	// The committed begin audit precedes irreversible unlink. Acquire the
	// writer before blobMu, as upload materialization releases blobMu before
	// committing its references. A read-only recheck alone would race that gap.
	_, err = execution.Execute(ctx, store.runner, store.opGCComplete, func(tx *execution.Tx) (int64, error) {
		store.blobMu.Lock()
		defer store.blobMu.Unlock()
		eligible, err := orphanEligible(ctx, tx, blob)
		if err != nil {
			return 0, err
		}
		if !eligible {
			return 0, &execution.RecordedFailure{Code: "collection_superseded", ObjectID: blob.ID}
		}
		directory, err := os.Open(filepath.Join(store.dir, "blobs"))
		if err != nil {
			return 0, &execution.RecordedFailure{Code: "collection_directory_unavailable", ObjectID: blob.ID}
		}
		defer directory.Close()
		if err := os.Remove(store.blobPath(blob.SHA256)); os.IsNotExist(err) {
			return 0, &execution.RecordedFailure{Code: "collection_already_absent", ObjectID: blob.ID, Outcome: audit.OutcomeUnknown}
		} else if err != nil {
			return 0, &execution.RecordedFailure{Code: "collection_unlink_failed", ObjectID: blob.ID}
		}
		if err := directory.Sync(); err != nil {
			return 0, &execution.RecordedFailure{Code: "collection_sync_unknown", ObjectID: blob.ID, Outcome: audit.OutcomeUnknown}
		}
		return blob.ID, nil
	}, func(id int64) int64 { return id })
	return err
}

func orphanEligible(ctx context.Context, reader audit.Reader, blob orphanBlob) (bool, error) {
	var count int
	err := reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifact_blobs b WHERE b.id=? AND b.sha256=? AND NOT EXISTS (SELECT 1 FROM artifacts a WHERE a.blob_id=b.id AND a.body_expired=0)`, blob.ID, blob.SHA256).Scan(&count)
	return count == 1, err
}
