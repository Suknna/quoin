package alerts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// Queries owns the unified alert read projections. Upstream occurrences and
// platform faults retain distinct storage and lifecycles but share this safe,
// non-secret representation for the existing alert list and detail surfaces.
type AttributionDiagnostic struct {
	Status                    string `json:"status"`
	CandidateSystemIDs        string `json:"candidateSystemIdsJson"`
	CandidateConfigVersionIDs string `json:"candidateConfigVersionIdsJson"`
	Reason                    string `json:"reasonJson"`
}

// ViewAttributionDiagnostic surfaces the real business-view attribution
// (ADR-0008). Attributed rows freeze their unique view identity; ambiguous
// rows keep every candidate snapshot; unattributed rows carry the reason.
// BusinessSystem/Attribution above are legacy declaration history and are
// never recomputed.
type ViewAttributionDiagnostic struct {
	Status         string `json:"status"`
	ViewKey        string `json:"viewKey,omitempty"`
	ViewName       string `json:"viewName,omitempty"`
	CandidatesJSON string `json:"candidatesJson"`
	Reason         string `json:"reasonJson"`
	CreatedAt      string `json:"createdAt"`
}

type OccurrenceSummary struct {
	// ID is an occurrence locator or platform:<fault-id>; Source is mandatory so
	// consumers cannot mistake an internal fault for an Alertmanager occurrence.
	ID              string                     `json:"id"`
	Source          string                     `json:"source"`
	State           string                     `json:"state"`
	RowVersion      int64                      `json:"rowVersion"`
	BusinessSystem  *string                    `json:"businessSystemKey,omitempty"`
	Attribution     *AttributionDiagnostic     `json:"attribution,omitempty"`
	ViewAttribution *ViewAttributionDiagnostic `json:"viewAttribution,omitempty"`
	Component       string                     `json:"component,omitempty"`
	Reason          string                     `json:"reason,omitempty"`
	FirstSeenAt     string                     `json:"firstSeenAt"`
	LastStateChange string                     `json:"lastStateChangeAt"`
	ResolvedAt      *string                    `json:"resolvedAt,omitempty"`
	Labels          map[string]string          `json:"labels"`
	Annotations     map[string]string          `json:"annotations,omitempty"`
}

type AlertSnapshot struct {
	SnapshotSeq int64               `json:"snapshotSeq"`
	Items       []OccurrenceSummary `json:"items"`
	NextCursor  string              `json:"nextCursor,omitempty"`
}

// viewAttributionColumns is the shared projection list for the frozen
// business-view attribution joined by both alert read queries. The surfaced
// key/name are read from the frozen candidates snapshot's first element, so a
// later view rename or retirement can never drift the historical identity.
const viewAttributionColumns = `va.status, json_extract(va.candidates_json,'$[0].viewKey'), json_extract(va.candidates_json,'$[0].displayName'), va.candidates_json, va.reason_json, va.created_at`

// viewAttributionJoin is the shared LEFT JOIN of the frozen projection;
// attributed_view_id stays available for filtering, never for display.
const viewAttributionJoin = `
			LEFT JOIN alert_occurrence_view_attributions va ON va.occurrence_id=o.id`

func scanViewAttribution(status, viewKey, viewName, candidates, reason, createdAt sql.NullString) *ViewAttributionDiagnostic {
	if !status.Valid {
		return nil
	}
	return &ViewAttributionDiagnostic{
		Status: status.String, ViewKey: viewKey.String, ViewName: viewName.String,
		CandidatesJSON: candidates.String, Reason: reason.String, CreatedAt: createdAt.String,
	}
}

func occurrenceSummary(id int64, state string, version int64, businessKey sql.NullString, attributionStatus, attributionSystemIDs, attributionConfigIDs, attributionReason sql.NullString, first, changed string, resolved sql.NullString, labelsJSON string, annotationsJSON sql.NullString) (OccurrenceSummary, error) {
	summary := OccurrenceSummary{ID: strconv.FormatInt(id, 10), Source: "alertmanager", State: state, RowVersion: version, FirstSeenAt: first, LastStateChange: changed}
	if businessKey.Valid {
		value := businessKey.String
		summary.BusinessSystem = &value
	}
	if attributionStatus.Valid {
		summary.Attribution = &AttributionDiagnostic{
			Status: attributionStatus.String, CandidateSystemIDs: attributionSystemIDs.String,
			CandidateConfigVersionIDs: attributionConfigIDs.String, Reason: attributionReason.String,
		}
	}
	if resolved.Valid {
		value := resolved.String
		summary.ResolvedAt = &value
	}
	if err := json.Unmarshal([]byte(labelsJSON), &summary.Labels); err != nil {
		return OccurrenceSummary{}, err
	}
	if annotationsJSON.Valid && annotationsJSON.String != "" {
		if err := json.Unmarshal([]byte(annotationsJSON.String), &summary.Annotations); err != nil {
			return OccurrenceSummary{}, err
		}
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
// and filtering them would imply fabricated attribution. viewKey filters by the
// frozen business-view attribution (ADR-0008); businessSystemKey stays as the
// legacy declaration filter.
//
// The watermark query and both unions run inside ONE read-only snapshot
// transaction opened on the trusted execution.Reader (BeginSnapshot), so the
// returned SnapshotSeq and items always describe the same committed state and
// every statement degrades together: any failure rolls the whole snapshot
// back, and only a clean read reaches the single Commit.
func (service *Service) AlertSnapshot(ctx context.Context, state string, businessSystemKey string, viewKey string) (AlertSnapshot, error) {
	if state != "Firing" && state != "Resolved" {
		state = "Firing"
	}
	snapshot, err := service.runner.Reader().BeginSnapshot(ctx)
	if err != nil {
		return AlertSnapshot{}, err
	}
	defer snapshot.Rollback()
	var seq int64
	if err := snapshot.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM alert_change_log`).Scan(&seq); err != nil {
		return AlertSnapshot{}, err
	}
	conditions, args := `o.state=?`, []any{state}
	if businessSystemKey != "" {
		conditions += ` AND o.business_system_id=(SELECT id FROM business_systems WHERE key=?)`
		args = append(args, businessSystemKey)
	}
	if viewKey != "" {
		conditions += ` AND va.attributed_view_id=(SELECT id FROM business_views WHERE view_key=?)`
		args = append(args, viewKey)
	}
	rows, err := snapshot.QueryContext(ctx, `SELECT o.id,o.state,o.row_version,bs.key,attribution.status,attribution.candidate_system_ids_json,attribution.candidate_config_version_ids_json,attribution.reason_json,`+viewAttributionColumns+`,o.first_seen_at,o.last_state_change_at,o.resolved_at,o.labels_canonical,
			(SELECT json_extract(d.body, '$.alerts[' || item.item_index || '].annotations')

			 FROM alert_observations observation
			 JOIN alert_delivery_items item ON item.id=observation.delivery_item_id
			 JOIN alert_deliveries d ON d.id=observation.delivery_id
			 WHERE observation.occurrence_id=o.id
			 ORDER BY observation.committed_at ASC, observation.id ASC LIMIT 1)
			FROM alert_occurrences o
			LEFT JOIN business_systems bs ON bs.id=o.business_system_id
			LEFT JOIN alert_occurrence_attributions attribution ON attribution.occurrence_id=o.id
			`+viewAttributionJoin+`
			WHERE `+conditions, args...)
	if err != nil {
		return AlertSnapshot{}, err
	}
	items := []OccurrenceSummary{}
	for rows.Next() {
		var id, version int64
		var summaryState, first, changed, labels string
		var businessKey, attributionStatus, attributionSystemIDs, attributionConfigIDs, attributionReason, resolved, annotations sql.NullString
		var viewStatus, viewKeyDB, viewName, viewCandidates, viewReason, viewCreatedAt sql.NullString
		if err := rows.Scan(&id, &summaryState, &version, &businessKey, &attributionStatus, &attributionSystemIDs, &attributionConfigIDs, &attributionReason,
			&viewStatus, &viewKeyDB, &viewName, &viewCandidates, &viewReason, &viewCreatedAt,
			&first, &changed, &resolved, &labels, &annotations); err != nil {

			rows.Close()
			return AlertSnapshot{}, err
		}
		summary, err := occurrenceSummary(id, summaryState, version, businessKey, attributionStatus, attributionSystemIDs, attributionConfigIDs, attributionReason, first, changed, resolved, labels, annotations)
		if err != nil {
			rows.Close()
			return AlertSnapshot{}, err
		}
		summary.ViewAttribution = scanViewAttribution(viewStatus, viewKeyDB, viewName, viewCandidates, viewReason, viewCreatedAt)
		items = append(items, summary)
	}
	if err := rows.Close(); err != nil {
		return AlertSnapshot{}, err
	}
	// Platform faults join the unified list only on unfiltered reads: both
	// attribution filters scope to Alertmanager occurrences, and listing them
	// beside a view-filtered request would imply fabricated attribution.
	if businessSystemKey == "" && viewKey == "" {
		faultRows, err := snapshot.QueryContext(ctx, `SELECT id,component,reason,state,row_version,first_seen_at,last_seen_at,resolved_at FROM platform_faults WHERE state=?`, state)
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
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].LastStateChange == items[j].LastStateChange {
			return items[i].ID > items[j].ID
		}
		return items[i].LastStateChange > items[j].LastStateChange
	})
	if err := snapshot.Commit(); err != nil {
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
		err = service.runner.Reader().QueryRowContext(ctx, `SELECT component,reason,state,row_version,first_seen_at,last_seen_at,resolved_at FROM platform_faults WHERE id=?`, id).Scan(&component, &reason, &state, &version, &first, &last, &resolved)
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
	var businessKey, attributionStatus, attributionSystemIDs, attributionConfigIDs, attributionReason, resolved, annotations sql.NullString
	var viewStatus, viewKeyDB, viewName, viewCandidates, viewReason, viewCreatedAt sql.NullString
	err = service.runner.Reader().QueryRowContext(ctx, `SELECT o.state,o.row_version,bs.key,attribution.status,attribution.candidate_system_ids_json,attribution.candidate_config_version_ids_json,attribution.reason_json,`+viewAttributionColumns+`,o.first_seen_at,o.last_state_change_at,o.resolved_at,o.labels_canonical,
		(SELECT json_extract(d.body, '$.alerts[' || item.item_index || '].annotations')
		 FROM alert_observations observation
		 JOIN alert_delivery_items item ON item.id=observation.delivery_item_id
		 JOIN alert_deliveries d ON d.id=observation.delivery_id
		 WHERE observation.occurrence_id=o.id
		 ORDER BY observation.committed_at ASC, observation.id ASC LIMIT 1)
		FROM alert_occurrences o
		LEFT JOIN business_systems bs ON bs.id=o.business_system_id
		LEFT JOIN alert_occurrence_attributions attribution ON attribution.occurrence_id=o.id
		`+viewAttributionJoin+`
		WHERE o.id=?`, id).Scan(&summaryState, &version, &businessKey, &attributionStatus, &attributionSystemIDs, &attributionConfigIDs, &attributionReason,
		&viewStatus, &viewKeyDB, &viewName, &viewCandidates, &viewReason, &viewCreatedAt,
		&first, &changed, &resolved, &labels, &annotations)
	if err != nil {
		return OccurrenceSummary{}, err
	}
	summary, err := occurrenceSummary(id, summaryState, version, businessKey, attributionStatus, attributionSystemIDs, attributionConfigIDs, attributionReason, first, changed, resolved, labels, annotations)
	if err != nil {
		return OccurrenceSummary{}, err
	}
	summary.ViewAttribution = scanViewAttribution(viewStatus, viewKeyDB, viewName, viewCandidates, viewReason, viewCreatedAt)
	return summary, nil
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
	rows, err := service.runner.Reader().QueryContext(ctx, `SELECT id, observed_state, starts_at_source, ends_at_source, received_at, committed_at, effect FROM alert_observations WHERE occurrence_id=? ORDER BY committed_at DESC,id DESC`, occurrenceID)
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
	rows, err := service.runner.Reader().QueryContext(ctx, `SELECT id,kind,issue_key,detail_json,first_seen_at,last_seen_at,occurrence_count,row_version FROM alert_intake_issues WHERE `+filter+` ORDER BY last_seen_at DESC`)
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

// AcknowledgeIntakeIssue is the Admin-only, one-way sticky acknowledgment of
// one intake issue, executed through the shared runner: one runner-owned
// IMMEDIATE transaction carrying the administrator session re-check, the
// fenced UPDATE and the automatic audit event (ADR-0006). The acknowledged
// actor is the verified execution-metadata principal — the caller-supplied
// actorID must match it exactly, so a caller can never acknowledge under
// someone else's identity. A stale expectedRowVersion or an already
// acknowledged issue is a recorded deterministic rejection that changes
// nothing; the caller-visible outcome stays (false, nil), the historical
// conflict shape.
func (service *Service) AcknowledgeIntakeIssue(ctx context.Context, issueID int64, actorID int64, expectedRowVersion int64, timestamp string) (bool, error) {
	meta, err := execution.Require(ctx)
	if err != nil {
		return false, err
	}
	if meta.Actor.Kind != execution.PrincipalUser || meta.Actor.ID != actorID {
		return false, fmt.Errorf("alerts: acknowledgment actor %d does not match the execution metadata principal", actorID)
	}
	_, err = execution.Execute(ctx, service.runner, service.ops.ackIntake,
		func(tx *execution.Tx) (bool, error) {
			result, err := tx.ExecContext(ctx, `UPDATE alert_intake_issues SET acknowledged_at=?,acknowledged_by=?,row_version=row_version+1 WHERE id=? AND row_version=? AND acknowledged_at IS NULL`, timestamp, meta.Actor.ID, issueID, expectedRowVersion)
			if err != nil {
				return false, err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return false, err
			}
			if affected == 0 {
				return false, &execution.Rejection{Code: CodeRowVersionConflict, Detail: "接入问题版本已变化或已确认", ObjectID: issueID}
			}
			return true, nil
		},
		func(bool) int64 { return issueID })
	if err != nil {
		var rejection *execution.Rejection
		if errors.As(err, &rejection) && rejection.Code == CodeRowVersionConflict {
			// The rejection was recorded; the caller sees the same
			// not-applied outcome it always has.
			return false, nil
		}
		return false, err
	}
	return true, nil
}
