package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	started := timestamp(s.now())
	if _, err = s.db.ExecContext(ctx, `UPDATE backups SET status='running',stage='preflight',started_at=?,updated_at=?,row_version=row_version+1 WHERE id=? AND status='queued'`, started, started, backupID); err != nil {
		return Summary{}, err
	}
	s.refreshMetrics(ctx)
	runErr := s.preflight(ctx)
	finalDir, dbHash, count, manifestHash, sizeBytes := "", "", 0, "", int64(0)
	if runErr == nil {
		finalDir, dbHash, count, manifestHash, sizeBytes, runErr = s.publish(ctx, backupID)
	}
	completed := timestamp(s.now())
	if runErr != nil {
		errorCode := "backup_failed"
		var storageFailure *StorageFailure
		if errors.As(runErr, &storageFailure) {
			errorCode = "storage_unavailable"
		}
		_, err = s.db.ExecContext(ctx, `UPDATE backups SET status='failed',completed_at=?,updated_at=?,error_code=?,retryable=1,error_detail=?,row_version=row_version+1 WHERE id=? AND status='running'`, completed, completed, errorCode, truncate(runErr.Error(), 4096), backupID)
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
		return Summary{}, err
	}
	value, err = s.Get(ctx, backupID)
	if s.metrics != nil {
		s.metrics.Duration.Observe(s.now().Sub(parseTimestamp(started)).Seconds())
	}
	s.refreshMetrics(ctx)
	if err == nil {
		_ = s.gcLocked(ctx)
	}
	return value, err
}
func (s *Service) publish(ctx context.Context, id int64) (string, string, int, string, int64, error) {
	root := filepath.Join(s.config.BackupDirectory, fmt.Sprintf("%d", id))
	staging := root + ".partial"
	_ = os.RemoveAll(staging)
	if err := os.MkdirAll(filepath.Join(staging, "artifacts"), 0o700); err != nil {
		return "", "", 0, "", 0, s.backupWriteFailure(err)
	}
	fail := func(err error) (string, string, int, string, int64, error) {
		_ = os.RemoveAll(staging)
		return "", "", 0, "", 0, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE backups SET stage='database_snapshot',updated_at=?,row_version=row_version+1 WHERE id=?`, timestamp(s.now()), id); err != nil {
		return fail(err)
	}
	snapshot := filepath.Join(staging, "quoin.db")
	files, err := s.artifactStore.SnapshotAndCopy(ctx, snapshot, filepath.Join(staging, "artifacts"), func() error {
		_, stageErr := s.db.ExecContext(ctx, `UPDATE backups SET stage='artifact_copy',updated_at=?,row_version=row_version+1 WHERE id=?`, timestamp(s.now()), id)
		return stageErr
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
	if _, err = s.db.ExecContext(ctx, `UPDATE backups SET stage='manifest_publish',updated_at=?,row_version=row_version+1 WHERE id=?`, timestamp(s.now()), id); err != nil {
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
		_ = os.RemoveAll(root)
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
