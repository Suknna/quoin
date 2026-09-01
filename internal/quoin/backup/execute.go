package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type manifest struct {
	Version   int         `json:"version"`
	Database  fileEntry   `json:"database"`
	Artifacts []fileEntry `json:"artifacts"`
	CreatedAt string      `json:"createdAt"`
}
type fileEntry struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
}

func (s *Service) Run(ctx context.Context, id string) (Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	backupID := int64(0)
	if _, err := fmt.Sscan(id, &backupID); err != nil || backupID < 1 {
		return Summary{}, ErrNotFound
	}
	value, err := s.Get(ctx, backupID)
	if err != nil {
		return Summary{}, err
	}
	if value.Status != "queued" {
		return value, nil
	}
	started, err := s.nextRunTimestamp(ctx, backupID)
	if err != nil {
		return Summary{}, err
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE backups SET status='running',stage='preflight',started_at=?,updated_at=?,row_version=row_version+1 WHERE id=? AND status='queued'`, started, started, backupID); err != nil {
		return Summary{}, err
	}
	s.refreshMetrics(ctx)
	runErr := s.preflight(ctx)
	finalDir, dbHash, count, manifestHash, sizeBytes := "", "", 0, "", int64(0)
	if runErr == nil {
		finalDir, dbHash, count, manifestHash, sizeBytes, runErr = s.publish(ctx, backupID)
		if runErr == nil && s.afterPublish != nil {
			s.afterPublish()
		}
	}
	completed, timestampErr := s.nextRunTimestamp(context.Background(), backupID)
	if timestampErr != nil {
		return Summary{}, timestampErr
	}
	if runErr != nil {
		errorCode := "backup_failed"
		var storageFailure *StorageFailure
		if errors.As(runErr, &storageFailure) {
			errorCode = "storage_unavailable"
		}
		// Persist failure on an uncancelled context. A caller cancellation must
		// never leave the exclusive active row behind.
		_, err = s.db.ExecContext(context.Background(), `UPDATE backups SET status='failed',completed_at=?,updated_at=?,error_code=?,retryable=1,error_detail=?,row_version=row_version+1 WHERE id=? AND status='running'`, completed, completed, errorCode, truncate(runErr.Error(), 4096), backupID)
		if err != nil {
			return Summary{}, err
		}
		if s.metrics != nil {
			s.metrics.Failures.Inc()
			s.metrics.Duration.Observe(s.now().Sub(parseTimestamp(started)).Seconds())
		}
		s.refreshMetrics(ctx)
		failed, getErr := s.Get(ctx, backupID)
		if getErr != nil {
			return Summary{}, getErr
		}
		return failed, runErr
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE backups SET status='succeeded',stage='completed',completed_at=?,updated_at=?,db_sha256=?,manifest_sha256=?,artifact_count=?,size_bytes=?,manifest_path=?,row_version=row_version+1 WHERE id=? AND status='running'`, completed, completed, dbHash, manifestHash, count, sizeBytes, filepath.Join(finalDir, "manifest.json"), backupID); err != nil {
		// The filesystem set was published but has no committed authority. Remove
		// it durably before returning; otherwise Reconcile retries it next boot.
		cleanupErr := s.removeAll(finalDir)
		if cleanupErr == nil {
			cleanupErr = syncDirectory(s.config.BackupDirectory)
		}
		if cleanupErr != nil {
			return Summary{}, fmt.Errorf("commit backup run: %w (published set cleanup: %v)", err, cleanupErr)
		}
		failedAt, failedAtErr := s.nextRunTimestamp(context.Background(), backupID)
		if failedAtErr != nil {
			return Summary{}, fmt.Errorf("commit backup run: %w (prepare failed state: %v)", err, failedAtErr)
		}
		if _, failedErr := s.db.ExecContext(context.Background(), `UPDATE backups SET status='failed',completed_at=?,updated_at=?,error_code='backup_failed',retryable=1,error_detail=?,row_version=row_version+1 WHERE id=? AND status='running'`, failedAt, failedAt, truncate(err.Error(), 4096), backupID); failedErr != nil {
			return Summary{}, fmt.Errorf("commit backup run: %w (persist failed state: %v)", err, failedErr)
		}
		failed, getErr := s.Get(context.Background(), backupID)
		if getErr != nil {
			return Summary{}, fmt.Errorf("commit backup run: %w (read failed state: %v)", err, getErr)
		}
		return failed, err
	}
	value, err = s.Get(ctx, backupID)
	if s.metrics != nil {
		s.metrics.Duration.Observe(s.now().Sub(parseTimestamp(started)).Seconds())
	}
	s.refreshMetrics(ctx)
	if err == nil {
		// Retention cleanup has an independent durable health projection. A
		// deletion failure must not turn a fully published newest snapshot into a
		// failed Backup Run.
		_ = s.gcLocked(ctx)
	}
	return value, err
}

// nextRunTimestamp derives a timestamp strictly newer than the persisted Run
// timestamp. SQLite rejects equal updated_at values, and wall clocks need not
// advance between adjacent durable transitions.
func (s *Service) nextRunTimestamp(ctx context.Context, id int64) (string, error) {
	var previous string
	if err := s.db.QueryRowContext(ctx, `SELECT updated_at FROM backups WHERE id=?`, id).Scan(&previous); err != nil {
		return "", err
	}
	next := s.now()
	if prior, err := time.Parse(time.RFC3339Nano, previous); err == nil && !next.After(prior) {
		next = prior.Add(time.Nanosecond)
	}
	return timestamp(next), nil
}

func (s *Service) advanceRunStage(ctx context.Context, id int64, stage string) error {
	next, err := s.nextRunTimestamp(ctx, id)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE backups SET stage=?,updated_at=?,row_version=row_version+1 WHERE id=? AND status='running'`, stage, next, id)
	return err
}

func (s *Service) publish(ctx context.Context, id int64) (string, string, int, string, int64, error) {
	root := filepath.Join(s.config.BackupDirectory, fmt.Sprintf("%d", id))
	staging := root + ".partial"
	_ = s.removeAll(staging)
	if err := os.MkdirAll(filepath.Join(staging, "artifacts"), 0o700); err != nil {
		return "", "", 0, "", 0, s.backupWriteFailure(err)
	}
	fail := func(err error) (string, string, int, string, int64, error) {
		_ = s.removeAll(staging)
		return "", "", 0, "", 0, err
	}
	if err := s.advanceRunStage(ctx, id, "database_snapshot"); err != nil {
		return fail(err)
	}
	snapshot := filepath.Join(staging, "quoin.db")
	files, err := s.artifactStore.SnapshotAndCopy(ctx, snapshot, filepath.Join(staging, "artifacts"), func() error {
		return s.advanceRunStage(ctx, id, "artifact_copy")
	}, s.preflightCopiedSet)
	if err != nil {
		return fail(s.classifyBackupFailure(err))
	}
	dbHash, dbSize, err := hashFileSize(snapshot)
	if err != nil {
		return fail(s.classifyBackupFailure(err))
	}
	entries := make([]fileEntry, 0, len(files))
	sizeBytes := dbSize
	for _, file := range files {
		// SnapshotAndCopy selected this file from the fixed SQLite snapshot and
		// fsync'ed it under the same coordinator that gates GC.
		sum, size, err := hashFileSize(filepath.Join(staging, "artifacts", file.SHA256+".blob"))
		if err != nil {
			return fail(s.classifyBackupFailure(err))
		}
		entries = append(entries, fileEntry{Path: "artifacts/" + file.SHA256 + ".blob", SHA256: sum, SizeBytes: size})
		sizeBytes += size
	}
	if err = s.advanceRunStage(ctx, id, "manifest_publish"); err != nil {
		return fail(err)
	}
	body, err := json.Marshal(manifest{Version: 1, Database: fileEntry{Path: "quoin.db", SHA256: dbHash, SizeBytes: dbSize}, Artifacts: entries, CreatedAt: timestamp(s.now())})
	if err != nil {
		return fail(err)
	}
	manifestPath := filepath.Join(staging, "manifest.json")
	if err = writeDurableFile(manifestPath, append(body, '\n')); err != nil {
		return fail(s.backupWriteFailure(err))
	}
	manifestInfo, err := os.Stat(manifestPath)
	if err != nil {
		return fail(s.classifyBackupFailure(err))
	}
	sizeBytes += manifestInfo.Size()
	manifestHash, err := hashFile(manifestPath)
	if err != nil {
		return fail(s.classifyBackupFailure(err))
	}
	if err = syncDirectory(staging); err != nil {
		return fail(s.backupWriteFailure(err))
	}
	if err = os.Rename(staging, root); err != nil {
		return fail(s.backupWriteFailure(err))
	}
	if err = syncDirectory(s.config.BackupDirectory); err != nil {
		// The DB row remains running until Run records failure; remove the renamed
		// directory so a failed publish never leaves a complete-looking set.
		_ = s.removeAll(root)
		_ = syncDirectory(s.config.BackupDirectory)
		return "", "", 0, "", 0, s.backupWriteFailure(err)
	}
	return root, dbHash, len(entries), manifestHash, sizeBytes, nil
}

// writeDurableFile creates the final manifest name only after its contents are
// flushed. The staging directory is then fsync'ed before its atomic rename.
func writeDurableFile(path string, body []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(body); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
