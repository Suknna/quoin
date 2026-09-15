package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

type manifest struct {
	Version   int         `json:"version"`
	Release   string      `json:"release"`
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
	// The Run task executor reattaches the queued run's persisted correlation
	// when the caller arrives without one (a restart), so every lifecycle fact
	// lands on the original operation. A correlation-less row is a legacy
	// no-correlation fact: the run executes as a deliberate recovery operation
	// under its own fresh task scope — no fake link is fabricated.
	commandCtx, err := s.restoreOrEstablishTaskContext(ctx, backupID)
	if err != nil {
		return Summary{}, err
	}
	ctx = commandCtx
	value, err := s.Get(ctx, backupID)
	if err != nil {
		return Summary{}, err
	}
	if value.Status != "queued" {
		return value, nil
	}
	startedAt, started, err := s.startRun(ctx, backupID)
	if err != nil {
		return Summary{}, err
	}
	if !started {
		return s.Get(ctx, backupID)
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
		// A terminal failed row must never coexist with a usable set. Clean both
		// possible publish roots durably before making the Run non-active; if the
		// filesystem cannot prove removal, retain the active row for Reconcile.
		if cleanupErr := s.cleanupRunFiles(backupID); cleanupErr != nil {
			return Summary{}, fmt.Errorf("cleanup failed backup before terminal state: %w", cleanupErr)
		}
		errorCode := "backup_failed"
		var storageFailure *StorageFailure
		if errors.As(runErr, &storageFailure) {
			errorCode = "storage_unavailable"
		}
		// markRunFailed commits on a detached scope: a caller cancellation must
		// never leave the exclusive active row behind.
		failed, err := s.markRunFailed(ctx, backupID, errorCode, truncate(runErr.Error(), 4096), completed)
		if err != nil {
			return Summary{}, err
		}
		if s.metrics != nil {
			s.metrics.Failures.Inc()
			s.metrics.Duration.Observe(s.now().Sub(parseTimestamp(startedAt)).Seconds())
		}
		s.refreshMetrics(ctx)
		return failed, runErr
	}
	if err = s.markRunSucceeded(ctx, backupID, finalDir, dbHash, manifestHash, count, sizeBytes, completed); err != nil {
		// The filesystem set was published but has no committed authority. Remove
		// it durably before returning; otherwise Reconcile retries it next boot.
		cleanupErr := s.cleanupRunFiles(backupID)
		if cleanupErr != nil {
			return Summary{}, fmt.Errorf("commit backup run: %w (published set cleanup: %v)", err, cleanupErr)
		}
		failedAt, failedAtErr := s.nextRunTimestamp(context.Background(), backupID)
		if failedAtErr != nil {
			return Summary{}, fmt.Errorf("commit backup run: %w (prepare failed state: %v)", err, failedAtErr)
		}
		if _, failedErr := s.markRunFailed(ctx, backupID, "backup_failed", truncate(err.Error(), 4096), failedAt); failedErr != nil {
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
		s.metrics.Duration.Observe(s.now().Sub(parseTimestamp(startedAt)).Seconds())
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

// restoreOrEstablishTaskContext resolves the Run task scope: inherited when
// the caller already carries metadata, restored from the queued row's
// persisted correlation when restarting without one, or established fresh for
// a legacy correlation-less row (a deliberate recovery operation).
func (s *Service) restoreOrEstablishTaskContext(ctx context.Context, id int64) (context.Context, error) {
	if _, ok := execution.FromContext(ctx); ok {
		return ctx, nil
	}
	if identity, err := s.runIdentityOn(ctx, s.reader, id); err == nil && identity.restorable() {
		return s.restoreTaskContext(ctx, identity)
	}
	return s.executionContext(ctx, execution.SourceTask)
}

// startRun records the queued→running lifecycle fact through the runner: the
// transition and its automatic audit row commit atomically, and a run that
// lost the queue race stays unaudited silence (the winner's fact stands).
func (s *Service) startRun(ctx context.Context, id int64) (startedAt string, started bool, err error) {
	if _, err := execution.Execute(ctx, s.commands.runner, s.commands.runStart, func(tx *execution.Tx) (Summary, error) {
		at, err := s.nextRunTimestampOn(tx, id)
		if err != nil {
			return Summary{}, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE backups SET status='running',stage='preflight',started_at=?,updated_at=?,row_version=row_version+1 WHERE id=? AND status='queued'`, at, at, id)
		if err != nil {
			return Summary{}, err
		}
		if rows, err := result.RowsAffected(); err != nil {
			return Summary{}, err
		} else if rows != 1 {
			return Summary{}, errRunNotQueued
		}
		startedAt = at
		return Summary{}, nil
	}, func(Summary) int64 { return id }); err != nil {
		if errors.Is(err, errRunNotQueued) {
			return "", false, nil
		}
		return "", false, err
	}
	return startedAt, true, nil
}

// markRunSucceeded records the running→succeeded terminal fact with the
// published set's verifiable identity in one runner transaction.
func (s *Service) markRunSucceeded(ctx context.Context, id int64, finalDir, dbHash, manifestHash string, count int, sizeBytes int64, completed string) error {
	_, err := execution.Execute(ctx, s.commands.runner, s.commands.runSucceed, func(tx *execution.Tx) (Summary, error) {
		result, err := tx.ExecContext(ctx, `UPDATE backups SET status='succeeded',stage='completed',completed_at=?,updated_at=?,db_sha256=?,manifest_sha256=?,artifact_count=?,size_bytes=?,manifest_path=?,row_version=row_version+1 WHERE id=? AND status='running'`, completed, completed, dbHash, manifestHash, count, sizeBytes, filepath.Join(finalDir, "manifest.json"), id)
		if err != nil {
			return Summary{}, err
		}
		if rows, err := result.RowsAffected(); err != nil {
			return Summary{}, err
		} else if rows != 1 {
			return Summary{}, errRunNotRunning
		}
		return Summary{}, nil
	}, func(Summary) int64 { return id })
	return err
}

// markRunFailed records the running→failed terminal fact on a detached scope
// so a cancelled caller can never leave the exclusive active row behind. A
// losing race against an already-terminal row returns the durable state
// without a duplicate fact.
func (s *Service) markRunFailed(ctx context.Context, id int64, errorCode, detail, completed string) (Summary, error) {
	markCtx, err := s.detachedTaskContext(ctx)
	if err != nil {
		return Summary{}, err
	}
	if _, err := execution.Execute(markCtx, s.commands.runner, s.commands.runFail, func(tx *execution.Tx) (Summary, error) {
		result, err := tx.ExecContext(markCtx, `UPDATE backups SET status='failed',completed_at=?,updated_at=?,error_code=?,retryable=1,error_detail=?,row_version=row_version+1 WHERE id=? AND status='running'`, completed, completed, errorCode, detail, id)
		if err != nil {
			return Summary{}, err
		}
		if rows, err := result.RowsAffected(); err != nil {
			return Summary{}, err
		} else if rows != 1 {
			return Summary{}, errRunNotRunning
		}
		return Summary{}, nil
	}, func(Summary) int64 { return id }); err != nil {
		if errors.Is(err, errRunNotRunning) {
			return s.Get(markCtx, id)
		}
		return Summary{}, err
	}
	return s.Get(markCtx, id)
}

// markRunInterrupted records one restart-recovery fact for a run the previous
// process left active. It reports whether this call performed the transition.
// The fact lands on the interrupted run's own persisted correlation when the
// row carries one; a legacy NULL-correlation row keeps the fact on the
// reconcile pass scope — no fabricated link is written.
func (s *Service) markRunInterrupted(ctx context.Context, id int64, at string) (bool, error) {
	markCtx := ctx
	if identity, err := s.runIdentityOn(ctx, s.reader, id); err == nil && identity.restorable() {
		if restored, restoreErr := s.restoreTaskContext(ctx, identity); restoreErr == nil {
			markCtx = restored
		}
	}
	marked := true
	if _, err := execution.Execute(markCtx, s.commands.runner, s.commands.runInterrupted, func(tx *execution.Tx) (Summary, error) {
		result, err := tx.ExecContext(markCtx, `UPDATE backups SET status='failed',completed_at=?,updated_at=?,error_code='interrupted',retryable=1,error_detail='backup interrupted by process restart',row_version=row_version+1 WHERE id=? AND status IN ('queued','running')`, at, at, id)
		if err != nil {
			return Summary{}, err
		}
		if rows, err := result.RowsAffected(); err != nil {
			return Summary{}, err
		} else if rows != 1 {
			return Summary{}, errRunNotRunning
		}
		return Summary{}, nil
	}, func(Summary) int64 { return id }); err != nil {
		if errors.Is(err, errRunNotRunning) {
			return false, nil
		}
		return false, err
	}
	return marked, nil
}

// detachedTaskContext re-roots an operation's correlation onto a fresh
// background scope with the system task executor, so lifecycle facts can
// commit after the originating context was cancelled. The original initiator
// and request identity are preserved; without metadata a fresh task scope is
// established. This is a deliberate re-rooting for terminal and health facts,
// never a child-side correlation swap of a live caller scope.
func (s *Service) detachedTaskContext(ctx context.Context) (context.Context, error) {
	base := context.Background()
	meta, ok := execution.FromContext(ctx)
	if !ok {
		return s.executionContext(base, execution.SourceTask)
	}
	return execution.ReplaceMetadata(base, execution.Metadata{
		CorrelationID: meta.CorrelationID,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Initiator:     meta.Initiator,
		Source:        execution.Source{Kind: execution.SourceTask, RequestID: meta.Source.RequestID},
	})
}

// nextRunTimestamp derives a timestamp strictly newer than the persisted Run
// timestamp. SQLite rejects equal updated_at values, and wall clocks need not
// advance between adjacent durable transitions.
func (s *Service) nextRunTimestamp(ctx context.Context, id int64) (string, error) {
	return s.nextRunTimestampOn(s.reader, id)
}

// nextRunTimestampOn is the transaction-scoped form: lifecycle operations
// read the persisted timestamp through the runner's guarded transaction so
// the derived value can never trail a committed change.
func (s *Service) nextRunTimestampOn(reader summaryReader, id int64) (string, error) {
	var previous string
	if err := reader.QueryRowContext(context.Background(), `SELECT updated_at FROM backups WHERE id=?`, id).Scan(&previous); err != nil {
		return "", err
	}
	next := s.now()
	if prior, err := time.Parse(time.RFC3339Nano, previous); err == nil && !next.After(prior) {
		next = prior.Add(time.Nanosecond)
	}
	return timestamp(next), nil
}

// advanceRunStage records one bounded stage transition (preflight,
// database_snapshot, artifact_copy, manifest_publish) as an audited system
// fact while the Run stays running.
func (s *Service) advanceRunStage(ctx context.Context, id int64, stage string) error {
	_, err := execution.Execute(ctx, s.commands.runner, s.commands.runStage, func(tx *execution.Tx) (Summary, error) {
		next, err := s.nextRunTimestampOn(tx, id)
		if err != nil {
			return Summary{}, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE backups SET stage=?,updated_at=?,row_version=row_version+1 WHERE id=? AND status='running'`, stage, next, id)
		return Summary{}, err
	}, func(Summary) int64 { return id })
	return err
}

// cleanupRunFiles removes every path a non-terminal Run can have published and
// fsyncs the parent. Callers must not write status=failed until it succeeds.
func (s *Service) cleanupRunFiles(id int64) error {
	root := filepath.Join(s.config.BackupDirectory, fmt.Sprintf("%d", id))
	if filepath.Dir(root) != filepath.Clean(s.config.BackupDirectory) {
		return errors.New("unsafe backup cleanup path")
	}
	if err := s.removeAll(root); err != nil {
		return err
	}
	if err := s.removeAll(root + ".partial"); err != nil {
		return err
	}
	return syncDirectory(s.config.BackupDirectory)
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
	body, err := json.Marshal(manifest{Version: 1, Release: buildinfo.Release, Database: fileEntry{Path: "quoin.db", SHA256: dbHash, SizeBytes: dbSize}, Artifacts: entries, CreatedAt: timestamp(s.now())})
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
