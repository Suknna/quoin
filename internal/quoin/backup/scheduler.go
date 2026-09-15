package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/Suknna/quoin/internal/quoin/execution"
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
	value, commandCtx, queued, err := s.catchUp(ctx)
	if err != nil || !queued {
		return
	}
	go func() {
		// The queued run's correlation and original initiator are restored onto
		// a fresh background task scope: the goroutine must outlive this
		// scheduler pass without keeping its cancellation, while the Run it
		// executes stays the same business operation the queue recorded.
		taskCtx, err := s.detachedTaskContext(commandCtx)
		if err != nil {
			return
		}
		_, _ = s.Run(taskCtx, value.ID)
	}()
}

// CatchUp creates only the latest missed schedule boundary. Run calls
// Reconcile before RunScheduler, so an old active row never prevents a new
// catch-up from being evaluated indefinitely.
func (s *Service) CatchUp(ctx context.Context) error { _, _, _, err := s.catchUp(ctx); return err }
func (s *Service) catchUp(ctx context.Context) (Summary, context.Context, bool, error) {
	if !s.scheduleAdmission() {
		return Summary{}, nil, false, nil
	}
	settings, err := s.Settings(ctx)
	if err != nil || !settings.Enabled || settings.ScheduleCron == nil || *settings.ScheduleCron == "" {
		return Summary{}, nil, false, err
	}
	location, err := time.LoadLocation(settings.Timezone)
	if err != nil {
		return Summary{}, nil, false, fmt.Errorf("load backup timezone: %w", err)
	}
	schedule, err := cron.ParseStandard(*settings.ScheduleCron)
	if err != nil {
		return Summary{}, nil, false, fmt.Errorf("parse backup schedule: %w", err)
	}
	now := s.now().In(location)
	var observed sql.NullString
	if err = s.reader.QueryRowContext(ctx, `SELECT scheduled_for FROM backups WHERE trigger_kind='scheduled' ORDER BY scheduled_for DESC LIMIT 1`).Scan(&observed); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Summary{}, nil, false, err
	}
	start, err := s.scheduleStart(ctx, observed, location)
	if err != nil {
		return Summary{}, nil, false, err
	}
	due := latestScheduleBoundary(schedule, start, now)
	if due.IsZero() {
		return Summary{}, nil, false, nil
	}
	// A due boundary is a new scheduling operation: establish the system
	// scheduler scope here, at the entry, for the durable queue mutation.
	commandCtx, err := s.executionContext(ctx, execution.SourceScheduler)
	if err != nil {
		return Summary{}, nil, false, err
	}
	value, admitted, err := s.queueScheduledAdmitted(commandCtx, due.UTC(), settings)
	if errors.Is(err, ErrActive) {
		return Summary{}, nil, false, nil
	}
	if err != nil {
		return Summary{}, nil, false, err
	}
	// queueScheduledAdmitted holds the sole production SQLite connection until
	// it returns. Project only after that boundary so the metrics query cannot
	// wait on its own pool lease.
	if admitted {
		s.refreshMetrics(ctx)
	}
	return value, commandCtx, admitted, nil
}

// queueScheduledAdmitted is the final scheduling fence. The runner-owned IMMEDIATE
// transaction rereads enabled settings and inserts the row, so a concurrent
// disable can never leave a scheduled run behind it. A superseded fence is a
// plain no-op; an already-active run surfaces ErrActive for the caller to
// swallow — neither leaves a durable trace.
func (s *Service) queueScheduledAdmitted(ctx context.Context, due time.Time, expected Settings) (Summary, bool, error) {
	meta, metaErr := execution.Require(ctx)
	if metaErr != nil {
		return Summary{}, false, metaErr
	}
	outcome, err := execution.Execute(ctx, s.commands.runner, s.commands.schedule, func(tx *execution.Tx) (Summary, error) {
		current, err := s.settingsOn(ctx, tx)
		if err != nil {
			return Summary{}, err
		}
		if !s.scheduleAdmission() || !current.Enabled || !settingsEqual(current, expected) {
			return Summary{}, errScheduleSuperseded
		}
		value := timestamp(due)
		result, err := tx.ExecContext(ctx, `INSERT INTO backups(status,stage,trigger_kind,execution_mode,scheduled_for,row_version,created_at,updated_at,triggered_by,correlation_id,initiator_type) VALUES('queued','queued','scheduled','online',?,1,?,?,NULL,?,?)`, value, timestamp(s.now()), timestamp(s.now()), meta.CorrelationID, string(meta.Initiator.Kind))
		if err != nil {
			if isActiveConstraint(err) {
				return Summary{}, ErrActive
			}
			return Summary{}, err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return Summary{}, err
		}
		return scanSummary(ctx, tx, id)
	}, func(value Summary) int64 { return mustInt(value.ID) })
	if err != nil {
		if errors.Is(err, errScheduleSuperseded) {
			return Summary{}, false, nil
		}
		return Summary{}, false, err
	}
	return outcome, true, nil
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
	if err := s.reader.QueryRowContext(ctx, `SELECT schedule_enabled_at FROM backup_settings WHERE id=1 AND enabled=1`).Scan(&enabledAt); err != nil {
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
