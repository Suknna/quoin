package inspection

import (
	"context"
	"fmt"
	"time"
)

// MissingDailyDate is an explicit, read-only recovery hint, not a report or
// an implied healthy day. It never creates a Run or backfills by itself.
type MissingDailyDate struct {
	ConfigKey string `json:"configKey"`
	LocalDate string `json:"localDate"`
	Reason    string `json:"reason"`
}

// MissingDailyReports names the last 30 eligible local dates for one enabled
// configuration that have no report identity. The current config's schedule
// can only be trusted since its most recent edit; older dates are omitted
// rather than falsely attributed to a schedule we no longer have.
func (s *Service) MissingDailyReports(ctx context.Context, configKey string) ([]MissingDailyDate, error) {
	config, err := s.GetDailyReportConfig(ctx, configKey)
	if err != nil {
		return nil, err
	}
	missing := []MissingDailyDate{}
	if !config.Enabled {
		return missing, nil
	}
	location, err := time.LoadLocation(config.Timezone)
	if err != nil {
		return nil, fmt.Errorf("daily report config %q: timezone: %w", configKey, err)
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, config.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("daily report config %q: updated_at: %w", configKey, err)
	}
	trigger, err := time.Parse("15:04", config.TriggerTime)
	if err != nil {
		return nil, fmt.Errorf("daily report config %q: trigger_time: %w", configKey, err)
	}
	now := s.clock()
	localToday := now.In(location)
	today := time.Date(localToday.Year(), localToday.Month(), localToday.Day(), 0, 0, 0, 0, time.UTC)
	oldest := today.AddDate(0, 0, -30).Format("2006-01-02")
	rows, err := s.reader.QueryContext(ctx, `SELECT local_date FROM inspection_daily_reports WHERE config_id=? AND local_date>=? AND local_date<?`, config.configID, oldest, today.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	present := map[string]bool{}
	for rows.Next() {
		var date string
		if err := rows.Scan(&date); err != nil {
			return nil, err
		}
		present[date] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for offset := 1; offset <= 30; offset++ {
		day := today.AddDate(0, 0, -offset)
		localDate := day.Format("2006-01-02")
		// The report for local day D is due at the configured wall time on
		// D+1. Fall-back duplicates collapse into one date identity. A spring
		// forward nonexistent wall time is marked missing for manual recovery.
		due := time.Date(day.Year(), day.Month(), day.Day()+1, trigger.Hour(), trigger.Minute(), 0, 0, location)
		if due.Before(updatedAt) || due.After(now) || present[localDate] {
			continue
		}
		reason := "not_scheduled"
		if due.In(location).Format("15:04") != config.TriggerTime {
			reason = "trigger_nonexistent"
		}
		missing = append(missing, MissingDailyDate{ConfigKey: configKey, LocalDate: localDate, Reason: reason})
	}
	return missing, nil
}
