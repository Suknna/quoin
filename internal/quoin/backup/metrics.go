package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

func (s *Service) refreshMetrics(ctx context.Context) {
	if s.metrics == nil {
		return
	}
	var active int
	var running, oldest, success, manual, failure sql.NullString
	_ = s.reader.QueryRowContext(ctx, `SELECT COUNT(CASE WHEN status IN ('queued','running') THEN 1 END),MAX(CASE WHEN status='running' THEN started_at END),MIN(CASE WHEN status IN ('queued','running') THEN created_at END),MAX(CASE WHEN status='succeeded' THEN completed_at END),MAX(CASE WHEN status='succeeded' AND trigger_kind='manual' AND execution_mode='online' THEN completed_at END),MAX(CASE WHEN status='failed' THEN completed_at END) FROM backups`).Scan(&active, &running, &oldest, &success, &manual, &failure)
	if active > 0 {
		s.metrics.Active.Set(1)
	} else {
		s.metrics.Active.Set(0)
	}
	s.metrics.RunningSince.Set(unixTime(running))
	s.metrics.LastSuccess.Set(unixTime(success))
	s.metrics.LastOnlineManualSuccess.Set(unixTime(manual))
	s.metrics.LastFailure.Set(unixTime(failure))
	if oldest.Valid {
		age := s.now().Sub(parseTimestamp(oldest.String)).Seconds()
		if age < 0 {
			age = 0
		}
		s.metrics.OldestActiveAge.Set(age)
	} else {
		s.metrics.OldestActiveAge.Set(0)
	}
	s.metrics.ScheduleOverdue.Set(float64(s.scheduleOverdue(ctx)))
	health, err := s.RetentionHealth(ctx)
	if err != nil || health.LastFailureAt != nil {
		s.metrics.RetentionCleanupHealthy.Set(0)
		if err == nil && health.LastFailureAt != nil {
			s.metrics.RetentionCleanupLastFailure.Set(unixTime(sql.NullString{String: *health.LastFailureAt, Valid: true}))
		}
	} else {
		s.metrics.RetentionCleanupHealthy.Set(1)
		s.metrics.RetentionCleanupLastFailure.Set(0)
	}
}
func parseTimestamp(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
func unixTime(value sql.NullString) float64 {
	if !value.Valid {
		return 0
	}
	parsed := parseTimestamp(value.String)
	if parsed.IsZero() {
		return 0
	}
	return float64(parsed.Unix())
}

func (s *Service) Reconcile(ctx context.Context) error {
	// Restart recovery is its own system operation: the interrupted-run facts
	// below are audited through the runner under this internal scope.
	reconcileCtx, err := s.executionContext(ctx, execution.SourceInternal)
	if err != nil {
		return err
	}
	ctx = reconcileCtx
	// A directory becomes visible before the terminal SQL commit. Keep all
	// non-succeeded Runs unreachable by deleting only their numeric run root;
	// a later startup retries a failed removal instead of accepting the set.
	// A published directory is valid only once its Run committed succeeded.
	// Reconciliation must therefore retry cleanup for every non-succeeded row,
	// including failures caused after the final rename.
	rows, err := s.reader.QueryContext(ctx, `SELECT id FROM backups WHERE status <> 'succeeded'`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	// Delete and sync every non-succeeded root before changing active rows to
	// failed. If this cannot complete, they intentionally remain active so the
	// next Reconcile retries rather than exposing a failed recoverable set.
	for _, id := range ids {
		if err := s.cleanupRunFiles(id); err != nil {
			return fmt.Errorf("cleanup interrupted backup %d: %w", id, err)
		}
	}
	now := timestamp(s.now())
	activeRows, err := s.reader.QueryContext(ctx, `SELECT id FROM backups WHERE status IN ('queued','running')`)
	if err != nil {
		return err
	}
	var active []int64
	for activeRows.Next() {
		var id int64
		if err := activeRows.Scan(&id); err != nil {
			activeRows.Close()
			return err
		}
		active = append(active, id)
	}
	if err := activeRows.Close(); err != nil {
		return err
	}
	// One audited interrupted fact per run the previous process left active.
	interrupted := 0
	for _, id := range active {
		marked, err := s.markRunInterrupted(ctx, id, now)
		if err != nil {
			return err
		}
		if marked {
			interrupted++
		}
	}
	archives, err := filepath.Glob(filepath.Join(s.config.BackupDirectory, ".archive-*.tar"))
	if err != nil {
		return fmt.Errorf("list interrupted backup archives: %w", err)
	}
	for _, archive := range archives {
		if err := os.Remove(archive); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove interrupted backup archive: %w", err)
		}
	}
	if len(ids) > 0 || len(archives) > 0 {
		if err := syncDirectory(s.config.BackupDirectory); err != nil {
			return fmt.Errorf("sync interrupted backup cleanup: %w", err)
		}
	}
	if s.metrics != nil {
		for i := 0; i < interrupted; i++ {
			s.metrics.Failures.Inc()
		}
		s.refreshMetrics(ctx)
	}
	return nil
}
func (s *Service) scheduleOverdue(ctx context.Context) int {
	settings, err := s.Settings(ctx)
	if err != nil || !settings.Enabled || settings.ScheduleCron == nil || *settings.ScheduleCron == "" {
		return 0
	}
	location, err := time.LoadLocation(settings.Timezone)
	if err != nil {
		return 1
	}
	schedule, err := cron.ParseStandard(*settings.ScheduleCron)
	if err != nil {
		return 1
	}
	start, err := s.scheduleStart(ctx, sql.NullString{}, location)
	if err != nil {
		return 1
	}
	due := latestScheduleBoundary(schedule, start, s.now().In(location))
	if due.IsZero() {
		return 0
	}
	var status string
	err = s.reader.QueryRowContext(ctx, `SELECT status FROM backups WHERE trigger_kind='scheduled' AND scheduled_for=?`, timestamp(due.UTC())).Scan(&status)
	return boolInt(err != nil || status != "succeeded")
}
