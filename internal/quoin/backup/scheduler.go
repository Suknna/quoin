package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// RunScheduler keeps scheduling in process; backups.scheduled_for, rather than
// timer memory, is the durable restart and overlap boundary.
func (s *Service) RunScheduler(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		s.runDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *Service) runDue(ctx context.Context) {
	// Retention cleanup is independently retryable: a failed removal cannot be
	// hidden behind the succeeding snapshot that first exposed it.
	_ = s.GC(ctx)
	// Active age is derived from wall time, so refresh on every durable
	// scheduler pass even when no run becomes due.
	s.refreshMetrics(ctx)
	value, queued, err := s.catchUp(ctx)
	if err != nil || !queued {
		return
	}
	go func() { _, _ = s.Run(context.Background(), value.ID) }()
}

// CatchUp creates only the latest missed schedule boundary. Run calls
// Reconcile before RunScheduler, so an old active row never prevents a new
// catch-up from being evaluated indefinitely.
func (s *Service) CatchUp(ctx context.Context) error { _, _, err := s.catchUp(ctx); return err }
func (s *Service) catchUp(ctx context.Context) (Summary, bool, error) {
	if !s.scheduleAdmission() {
		return Summary{}, false, nil
	}
	settings, err := s.Settings(ctx)
	if err != nil || !settings.Enabled || settings.ScheduleCron == nil || *settings.ScheduleCron == "" {
		return Summary{}, false, err
	}
	location, err := time.LoadLocation(settings.Timezone)
	if err != nil {
		return Summary{}, false, fmt.Errorf("load backup timezone: %w", err)
	}
	schedule, err := cron.ParseStandard(*settings.ScheduleCron)
	if err != nil {
		return Summary{}, false, fmt.Errorf("parse backup schedule: %w", err)
	}
	now := s.now().In(location)
	var observed sql.NullString
	if err = s.db.QueryRowContext(ctx, `SELECT scheduled_for FROM backups WHERE trigger_kind='scheduled' ORDER BY scheduled_for DESC LIMIT 1`).Scan(&observed); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Summary{}, false, err
	}
	start, err := s.scheduleStart(ctx, observed, location)
	if err != nil {
		return Summary{}, false, err
	}
	due := latestScheduleBoundary(schedule, start, now)
	if due.IsZero() {
		return Summary{}, false, nil
	}
	value, admitted, err := s.queueScheduledAdmitted(ctx, due.UTC(), settings)
	if errors.Is(err, ErrActive) {
		return Summary{}, false, nil
	}
	if err != nil {
		return Summary{}, false, err
	}
	return value, admitted, nil
}

// queueScheduledAdmitted is the final scheduling fence. It starts the SQLite
// writer transaction before it rereads enabled settings and inserts the row, so
// a concurrent disable can never leave a scheduled run behind it.
func (s *Service) queueScheduledAdmitted(ctx context.Context, due time.Time, expected Settings) (Summary, bool, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Summary{}, false, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return Summary{}, false, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	current, err := s.settingsOn(ctx, conn)
	if err != nil {
		return Summary{}, false, err
	}
	if !s.scheduleAdmission() || !current.Enabled || !settingsEqual(current, expected) {
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return Summary{}, false, err
		}
		committed = true
		return Summary{}, false, nil
	}
	value := timestamp(due)
	result, err := conn.ExecContext(ctx, `INSERT INTO backups(status,stage,trigger_kind,execution_mode,scheduled_for,row_version,created_at,updated_at,triggered_by) VALUES('queued','queued','scheduled','online',?,1,?,?,NULL)`, value, timestamp(s.now()), timestamp(s.now()))
	if err != nil {
		if isActiveConstraint(err) {
			return Summary{}, false, ErrActive
		}
		return Summary{}, false, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Summary{}, false, err
	}
	queued, err := s.getOn(ctx, conn, id)
	if err != nil {
		return Summary{}, false, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return Summary{}, false, err
	}
	committed = true
	s.refreshMetrics(ctx)
	return queued, true, nil
}

func (s *Service) scheduleStart(ctx context.Context, observed sql.NullString, location *time.Location) (time.Time, error) {
	if observed.Valid {
		value, err := time.Parse(time.RFC3339Nano, observed.String)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse scheduled backup boundary: %w", err)
		}
		return value.In(location), nil
	}
	var enabledAt string
	if err := s.db.QueryRowContext(ctx, `SELECT schedule_enabled_at FROM backup_settings WHERE id=1 AND enabled=1`).Scan(&enabledAt); err != nil {
		return time.Time{}, err
	}
	value, err := time.Parse(time.RFC3339Nano, enabledAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse backup schedule enable time: %w", err)
	}
	return value.In(location), nil
}
func validateScheduleSettings(settings Settings) error {
	// Local is a Go process-environment alias, not a durable IANA authority.
	if settings.Timezone == "Local" {
		return errors.New("timezone must be UTC or an IANA timezone name")
	}
	if _, err := time.LoadLocation(settings.Timezone); err != nil {
		return fmt.Errorf("load backup timezone: %w", err)
	}
	if settings.ScheduleCron != nil && *settings.ScheduleCron != "" {
		if _, err := cron.ParseStandard(*settings.ScheduleCron); err != nil {
			return fmt.Errorf("parse backup schedule: %w", err)
		}
	}
	return nil
}

func latestScheduleBoundary(schedule cron.Schedule, after, now time.Time) time.Time {
	candidate := schedule.Next(after)
	if candidate.After(now) {
		return time.Time{}
	}
	for next := schedule.Next(candidate); !next.After(now); next = schedule.Next(candidate) {
		candidate = next
	}
	return candidate
}
