package backup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/robfig/cron/v3"
)

func (s *Service) refreshMetrics(ctx context.Context) {
	if s.metrics == nil {
		return
	}
	var active int
	var running, oldest, success, manual, failure sql.NullString
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(CASE WHEN status IN ('queued','running') THEN 1 END),MAX(CASE WHEN status='running' THEN started_at END),MIN(CASE WHEN status IN ('queued','running') THEN created_at END),MAX(CASE WHEN status='succeeded' THEN completed_at END),MAX(CASE WHEN status='succeeded' AND trigger_kind='manual' AND execution_mode='online' THEN completed_at END),MAX(CASE WHEN status='failed' THEN completed_at END) FROM backups`).Scan(&active, &running, &oldest, &success, &manual, &failure)
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
	// A directory becomes visible before the terminal SQL commit. Keep all
	// non-succeeded Runs unreachable by deleting only their numeric run root;
	// a later startup retries a failed removal instead of accepting the set.
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM backups WHERE status IN ('queued','running') OR (status='failed' AND error_code='interrupted')`)
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
	now := timestamp(s.now())
	result, err := s.db.ExecContext(ctx, `UPDATE backups SET status='failed',completed_at=?,updated_at=?,error_code='interrupted',retryable=1,error_detail='backup interrupted by process restart',row_version=row_version+1 WHERE status IN ('queued','running')`, now, now)
	if err != nil {
		return err
	}
	for _, id := range ids {
		root := filepath.Join(s.config.BackupDirectory, fmt.Sprintf("%d", id))
		if filepath.Dir(root) != filepath.Clean(s.config.BackupDirectory) {
			return fmt.Errorf("unsafe backup reconciliation path")
		}
		if err := os.RemoveAll(root); err != nil {
			return fmt.Errorf("remove interrupted backup %d: %w", id, err)
		}
		if err := syncDirectory(s.config.BackupDirectory); err != nil {
			return fmt.Errorf("sync interrupted backup cleanup: %w", err)
		}
	}
	if s.metrics != nil {
		if count, _ := result.RowsAffected(); count > 0 {
			for i := int64(0); i < count; i++ {
				s.metrics.Failures.Inc()
			}
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
	err = s.db.QueryRowContext(ctx, `SELECT status FROM backups WHERE trigger_kind='scheduled' AND scheduled_for=?`, timestamp(due.UTC())).Scan(&status)
	return boolInt(err != nil || status != "succeeded")
}
