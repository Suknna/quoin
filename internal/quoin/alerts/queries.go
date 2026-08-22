package alerts

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
)

// Queries owns the read-side projections the HTTP/SSE surface consumes:
// snapshot list, occurrence detail, and observation timeline.

type OccurrenceSummary struct {
	ID              string            `json:"id"`
	State           string            `json:"state"`
	RowVersion      int64             `json:"rowVersion"`
	BusinessSystem  *string           `json:"businessSystemKey,omitempty"`
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

// AlertSnapshot returns the occurrence snapshot filtered by state (Firing
// is the default current view; Resolved is the history view) plus the
// current alert_change_log high-water (DATA-SSE-009 / HTTP-PAGE-006).
// businessSystemKey filters to occurrences attributed to that system;
// 未归属 occurrences only appear in the unfiltered view.
func (service *Service) AlertSnapshot(ctx context.Context, state string, businessSystemKey string) (AlertSnapshot, error) {
	if state != "Firing" && state != "Resolved" {
		state = "Firing"
	}
	// seq and the member rows must come from one SQLite read transaction. If
	// they came from separate statement snapshots, a delivery committed between
	// them could be neither represented by the returned items nor replayed by
	// SSE after=seq.
	tx, err := service.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AlertSnapshot{}, err
	}
	defer tx.Rollback()
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM alert_change_log`).Scan(&seq); err != nil {
		return AlertSnapshot{}, err
	}
	conditions := `o.state=?`
	args := []any{state}
	if businessSystemKey != "" {
		conditions += ` AND o.business_system_id=(SELECT id FROM business_systems WHERE key=?)`
		args = append(args, businessSystemKey)
	}
	rows, err := tx.QueryContext(ctx, `SELECT o.id, o.state, o.row_version, bs.key, o.first_seen_at, o.last_state_change_at, o.resolved_at, o.labels_canonical FROM alert_occurrences o LEFT JOIN business_systems bs ON bs.id=o.business_system_id WHERE `+conditions+` ORDER BY o.last_state_change_at DESC, o.id DESC`, args...)
	if err != nil {
		return AlertSnapshot{}, err
	}
	defer rows.Close()
	snapshot := AlertSnapshot{SnapshotSeq: seq}
	for rows.Next() {
		var summary OccurrenceSummary
		var id int64
		var businessKey sql.NullString
		var resolvedAt sql.NullString
		var labelsJSON string
		if err := rows.Scan(&id, &summary.State, &summary.RowVersion, &businessKey, &summary.FirstSeenAt, &summary.LastStateChange, &resolvedAt, &labelsJSON); err != nil {
			return AlertSnapshot{}, err
		}
		summary.ID = strconv.FormatInt(id, 10)
		if businessKey.Valid {
			value := businessKey.String
			summary.BusinessSystem = &value
		}
		if resolvedAt.Valid {
			summary.ResolvedAt = &resolvedAt.String
		}
		if err := json.Unmarshal([]byte(labelsJSON), &summary.Labels); err != nil {
			return AlertSnapshot{}, err
		}
		snapshot.Items = append(snapshot.Items, summary)
	}
	if snapshot.Items == nil {
		snapshot.Items = []OccurrenceSummary{}
	}
	if err := rows.Err(); err != nil {
		return AlertSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return AlertSnapshot{}, err
	}
	return snapshot, nil
}

// GetOccurrence returns the detail projection for one occurrence.
func (service *Service) GetOccurrence(ctx context.Context, occurrenceID int64) (OccurrenceSummary, error) {
	var summary OccurrenceSummary
	var id int64
	var businessKey sql.NullString
	var resolvedAt sql.NullString
	var labelsJSON string
	err := service.db.QueryRowContext(ctx, `SELECT o.id, o.state, o.row_version, bs.key, o.first_seen_at, o.last_state_change_at, o.resolved_at, o.labels_canonical FROM alert_occurrences o LEFT JOIN business_systems bs ON bs.id=o.business_system_id WHERE o.id=?`, occurrenceID).
		Scan(&id, &summary.State, &summary.RowVersion, &businessKey, &summary.FirstSeenAt, &summary.LastStateChange, &resolvedAt, &labelsJSON)
	if err != nil {
		return OccurrenceSummary{}, err
	}
	summary.ID = strconv.FormatInt(id, 10)
	if businessKey.Valid {
		value := businessKey.String
		summary.BusinessSystem = &value
	}
	if resolvedAt.Valid {
		summary.ResolvedAt = &resolvedAt.String
	}
	if err := json.Unmarshal([]byte(labelsJSON), &summary.Labels); err != nil {
		return OccurrenceSummary{}, err
	}
	return summary, nil
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

// ListObservations returns the immutable observation timeline for one
// occurrence (newest first).
func (service *Service) ListObservations(ctx context.Context, occurrenceID int64) ([]Observation, error) {
	rows, err := service.db.QueryContext(ctx, `SELECT id, observed_state, starts_at_source, ends_at_source, received_at, committed_at, effect FROM alert_observations WHERE occurrence_id=? ORDER BY committed_at DESC, id DESC`, occurrenceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	observations := []Observation{}
	for rows.Next() {
		var observation Observation
		var id int64
		var endsAt sql.NullString
		if err := rows.Scan(&id, &observation.ObservedState, &observation.StartsAt, &endsAt, &observation.ReceivedAt, &observation.CommittedAt, &observation.Effect); err != nil {
			return nil, err
		}
		observation.ID = strconv.FormatInt(id, 10)
		if endsAt.Valid {
			observation.EndsAt = &endsAt.String
		}
		observations = append(observations, observation)
	}
	return observations, rows.Err()
}

// IntakeIssue is the admin/operator-facing aggregation of one unacknowledged
// intake problem (DATA-ALERT-011).
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

// ListIntakeIssues returns intake issues; acknowledged=true selects the
// acknowledged history instead of the open set (HTTP-INTAKE-001).
func (service *Service) ListIntakeIssues(ctx context.Context, acknowledged bool) ([]IntakeIssue, error) {
	filter := "acknowledged_at IS NULL"
	if acknowledged {
		filter = "acknowledged_at IS NOT NULL"
	}
	rows, err := service.db.QueryContext(ctx, `SELECT id, kind, issue_key, detail_json, first_seen_at, last_seen_at, occurrence_count, row_version FROM alert_intake_issues WHERE `+filter+` ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	issues := []IntakeIssue{}
	for rows.Next() {
		var issue IntakeIssue
		var id int64
		if err := rows.Scan(&id, &issue.Kind, &issue.IssueKey, &issue.DetailJSON, &issue.FirstSeenAt, &issue.LastSeenAt, &issue.OccurrenceCount, &issue.RowVersion); err != nil {
			return nil, err
		}
		issue.ID = strconv.FormatInt(id, 10)
		issues = append(issues, issue)
	}
	return issues, rows.Err()
}

// AcknowledgeIntakeIssue is the Admin-only one-way confirmation
// (DATA-ALERT-011, HTTP-INTAKE-001).
func (service *Service) AcknowledgeIntakeIssue(ctx context.Context, issueID int64, actorID int64, expectedRowVersion int64, timestamp string) (bool, error) {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return false, err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()
	result, err := conn.ExecContext(ctx, `UPDATE alert_intake_issues SET acknowledged_at=?, acknowledged_by=?, row_version=row_version+1 WHERE id=? AND row_version=? AND acknowledged_at IS NULL`,
		timestamp, actorID, issueID, expectedRowVersion)
	if err != nil {
		return false, err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return false, nil
	}
	if err := service.recordAudit(ctx, conn, "user", actorID, "alert_intake_issue.acknowledge", "success", "alert_intake_issue", issueID, timestamp); err != nil {
		return false, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return false, err
	}
	return true, nil
}
