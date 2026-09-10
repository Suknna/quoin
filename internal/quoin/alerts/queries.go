package alerts

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
)

// Queries owns the unified alert read projections. Upstream occurrences and
// platform faults retain distinct storage and lifecycles but share this safe,
// non-secret representation for the existing alert list and detail surfaces.
type OccurrenceSummary struct {
	// ID is an occurrence locator or platform:<fault-id>; Source is mandatory so
	// consumers cannot mistake an internal fault for an Alertmanager occurrence.
	ID              string            `json:"id"`
	Source          string            `json:"source"`
	State           string            `json:"state"`
	RowVersion      int64             `json:"rowVersion"`
	BusinessSystem  *string           `json:"businessSystemKey,omitempty"`
	Component       string            `json:"component,omitempty"`
	Reason          string            `json:"reason,omitempty"`
	FirstSeenAt     string            `json:"firstSeenAt"`
	LastStateChange string            `json:"lastStateChangeAt"`
	ResolvedAt      *string           `json:"resolvedAt,omitempty"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations,omitempty"`
}

type AlertSnapshot struct {
	SnapshotSeq int64               `json:"snapshotSeq"`
	Items       []OccurrenceSummary `json:"items"`
	NextCursor  string              `json:"nextCursor,omitempty"`
}

func occurrenceSummary(id int64, state string, version int64, businessKey sql.NullString, first, changed string, resolved sql.NullString, labelsJSON string) (OccurrenceSummary, error) {
	summary := OccurrenceSummary{ID: strconv.FormatInt(id, 10), Source: "alertmanager", State: state, RowVersion: version, FirstSeenAt: first, LastStateChange: changed}
	if businessKey.Valid {
		value := businessKey.String
		summary.BusinessSystem = &value
	}
	if resolved.Valid {
		value := resolved.String
		summary.ResolvedAt = &value
	}
	if err := json.Unmarshal([]byte(labelsJSON), &summary.Labels); err != nil {
		return OccurrenceSummary{}, err
	}
	return summary, nil
}

func platformFaultSummary(id int64, component, reason, state string, version int64, first, last string, resolved sql.NullString) OccurrenceSummary {
	// Repeat disconnects only advance last_seen_at and emit no change-log event.
	// Sort unified rows by a state transition time so those diagnostic repeats
	// cannot reorder a client view without a corresponding SSE notification.
	lifecycleAt := first
	if state == "Resolved" && resolved.Valid {
		lifecycleAt = resolved.String
	}
	summary := OccurrenceSummary{
		ID: "platform:" + strconv.FormatInt(id, 10), Source: "platform", State: state, RowVersion: version,
		Component: component, Reason: reason, FirstSeenAt: first, LastStateChange: lifecycleAt,
		Labels:      map[string]string{"alertname": "Platform component unavailable", "component": component, "severity": "warning"},
		Annotations: map[string]string{"summary": "内部组件 " + component + " 不可用", "description": reason},
	}
	if resolved.Valid {
		value := resolved.String
		summary.ResolvedAt = &value
	}
	return summary
}

// AlertSnapshot returns occurrences and independently durable platform faults.
// Platform rows intentionally ignore businessSystemKey: no business was declared
// and filtering them would imply fabricated attribution.
func (service *Service) AlertSnapshot(ctx context.Context, state string, businessSystemKey string) (AlertSnapshot, error) {
	if state != "Firing" && state != "Resolved" {
		state = "Firing"
	}
	tx, err := service.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AlertSnapshot{}, err
	}
	defer tx.Rollback()
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM alert_change_log`).Scan(&seq); err != nil {
		return AlertSnapshot{}, err
	}
	conditions, args := `o.state=?`, []any{state}
	if businessSystemKey != "" {
		conditions += ` AND o.business_system_id=(SELECT id FROM business_systems WHERE key=?)`
		args = append(args, businessSystemKey)
	}
	rows, err := tx.QueryContext(ctx, `SELECT o.id,o.state,o.row_version,bs.key,o.first_seen_at,o.last_state_change_at,o.resolved_at,o.labels_canonical FROM alert_occurrences o LEFT JOIN business_systems bs ON bs.id=o.business_system_id WHERE `+conditions, args...)
	if err != nil {
		return AlertSnapshot{}, err
	}
	items := []OccurrenceSummary{}
	for rows.Next() {
		var id, version int64
		var summaryState, first, changed, labels string
		var businessKey, resolved sql.NullString
		if err := rows.Scan(&id, &summaryState, &version, &businessKey, &first, &changed, &resolved, &labels); err != nil {
			rows.Close()
			return AlertSnapshot{}, err
		}
		summary, err := occurrenceSummary(id, summaryState, version, businessKey, first, changed, resolved, labels)
		if err != nil {
			rows.Close()
			return AlertSnapshot{}, err
		}
		items = append(items, summary)
	}
	if err := rows.Close(); err != nil {
		return AlertSnapshot{}, err
	}
	if businessSystemKey == "" {
		faultRows, err := tx.QueryContext(ctx, `SELECT id,component,reason,state,row_version,first_seen_at,last_seen_at,resolved_at FROM platform_faults WHERE state=?`, state)
		if err != nil {
			return AlertSnapshot{}, err
		}
		for faultRows.Next() {
			var id, version int64
			var component, reason, faultState, first, last string
			var resolved sql.NullString
			if err := faultRows.Scan(&id, &component, &reason, &faultState, &version, &first, &last, &resolved); err != nil {
				faultRows.Close()
				return AlertSnapshot{}, err
			}
			items = append(items, platformFaultSummary(id, component, reason, faultState, version, first, last, resolved))
		}
		if err := faultRows.Close(); err != nil {
			return AlertSnapshot{}, err
		}
	}
	// Lexical RFC3339Nano order is chronological UTC order; this keeps the union
	// deterministic without collapsing its source identities.
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			if items[j].LastStateChange > items[i].LastStateChange || (items[j].LastStateChange == items[i].LastStateChange && items[j].ID > items[i].ID) {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return AlertSnapshot{}, err
	}
	return AlertSnapshot{SnapshotSeq: seq, Items: items}, nil
}

// GetAlert resolves either member of the explicit unified alert union.
func (service *Service) GetAlert(ctx context.Context, alertID string) (OccurrenceSummary, error) {
	if strings.HasPrefix(alertID, "platform:") {
		id, err := strconv.ParseInt(strings.TrimPrefix(alertID, "platform:"), 10, 64)
		if err != nil || id <= 0 {
			return OccurrenceSummary{}, sql.ErrNoRows
		}
		var component, reason, state, first, last string
		var version int64
		var resolved sql.NullString
		err = service.db.QueryRowContext(ctx, `SELECT component,reason,state,row_version,first_seen_at,last_seen_at,resolved_at FROM platform_faults WHERE id=?`, id).Scan(&component, &reason, &state, &version, &first, &last, &resolved)
		if err != nil {
			return OccurrenceSummary{}, err
		}
		return platformFaultSummary(id, component, reason, state, version, first, last, resolved), nil
	}
	id, err := strconv.ParseInt(alertID, 10, 64)
	if err != nil || id <= 0 {
		return OccurrenceSummary{}, sql.ErrNoRows
	}
	var summaryState, first, changed, labels string
	var version int64
	var businessKey, resolved sql.NullString
	err = service.db.QueryRowContext(ctx, `SELECT o.state,o.row_version,bs.key,o.first_seen_at,o.last_state_change_at,o.resolved_at,o.labels_canonical FROM alert_occurrences o LEFT JOIN business_systems bs ON bs.id=o.business_system_id WHERE o.id=?`, id).Scan(&summaryState, &version, &businessKey, &first, &changed, &resolved, &labels)
	if err != nil {
		return OccurrenceSummary{}, err
	}
	return occurrenceSummary(id, summaryState, version, businessKey, first, changed, resolved, labels)
}

// GetOccurrence preserves the upstream-only public domain seam for callers
// which genuinely require an Alertmanager occurrence.
func (service *Service) GetOccurrence(ctx context.Context, occurrenceID int64) (OccurrenceSummary, error) {
	return service.GetAlert(ctx, strconv.FormatInt(occurrenceID, 10))
}

type Observation struct {
	ID            string  `json:"id"`
	ObservedState string  `json:"observedState"`
	StartsAt      string  `json:"startsAt"`
	EndsAt        *string `json:"endsAt,omitempty"`
	ReceivedAt    string  `json:"receivedAt"`
	CommittedAt   string  `json:"committedAt"`
	Effect        string  `json:"effect"`
}

// ListObservations remains upstream-only because platform fault history is its
// first/last/recovery lifecycle, not invented Alertmanager observations.
func (service *Service) ListObservations(ctx context.Context, alertID string) ([]Observation, error) {
	if strings.HasPrefix(alertID, "platform:") {
		return []Observation{}, nil
	}
	occurrenceID, err := strconv.ParseInt(alertID, 10, 64)
	if err != nil || occurrenceID <= 0 {
		return nil, sql.ErrNoRows
	}
	rows, err := service.db.QueryContext(ctx, `SELECT id, observed_state, starts_at_source, ends_at_source, received_at, committed_at, effect FROM alert_observations WHERE occurrence_id=? ORDER BY committed_at DESC,id DESC`, occurrenceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	observations := []Observation{}
	for rows.Next() {
		var item Observation
		var id int64
		var ends sql.NullString
		if err := rows.Scan(&id, &item.ObservedState, &item.StartsAt, &ends, &item.ReceivedAt, &item.CommittedAt, &item.Effect); err != nil {
			return nil, err
		}
		item.ID = strconv.FormatInt(id, 10)
		if ends.Valid {
			value := ends.String
			item.EndsAt = &value
		}
		observations = append(observations, item)
	}
	return observations, rows.Err()
}

type IntakeIssue struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	IssueKey        string `json:"issueKey"`
	DetailJSON      string `json:"detailJson"`
	FirstSeenAt     string `json:"firstSeenAt"`
	LastSeenAt      string `json:"lastSeenAt"`
	OccurrenceCount int    `json:"occurrenceCount"`
	RowVersion      int64  `json:"rowVersion"`
}

func (service *Service) ListIntakeIssues(ctx context.Context, acknowledged bool) ([]IntakeIssue, error) {
	filter := "acknowledged_at IS NULL"
	if acknowledged {
		filter = "acknowledged_at IS NOT NULL"
	}
	rows, err := service.db.QueryContext(ctx, `SELECT id,kind,issue_key,detail_json,first_seen_at,last_seen_at,occurrence_count,row_version FROM alert_intake_issues WHERE `+filter+` ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []IntakeIssue{}
	for rows.Next() {
		var item IntakeIssue
		var id int64
		if err := rows.Scan(&id, &item.Kind, &item.IssueKey, &item.DetailJSON, &item.FirstSeenAt, &item.LastSeenAt, &item.OccurrenceCount, &item.RowVersion); err != nil {
			return nil, err
		}
		item.ID = strconv.FormatInt(id, 10)
		items = append(items, item)
	}
	return items, rows.Err()
}
func (service *Service) AcknowledgeIntakeIssue(ctx context.Context, issueID int64, actorID int64, expectedRowVersion int64, timestamp string) (bool, error) {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return false, err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()
	result, err := conn.ExecContext(ctx, `UPDATE alert_intake_issues SET acknowledged_at=?,acknowledged_by=?,row_version=row_version+1 WHERE id=? AND row_version=? AND acknowledged_at IS NULL`, timestamp, actorID, issueID, expectedRowVersion)
	if err != nil {
		return false, err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return false, nil
	}
	if err = service.recordAudit(ctx, conn, "user", actorID, "alert_intake_issue.acknowledge", "success", "alert_intake_issue", issueID, timestamp); err != nil {
		return false, err
	}
	if _, err = conn.ExecContext(ctx, `COMMIT`); err != nil {
		return false, err
	}
	return true, nil
}
