// Read models: the SourceObservationRun and SourceObservedResource DTO
// projections (docs/specs OpenAPI). Every locator is the real persisted row
// id; identity labels are decoded from the canonical identity_key encoding
// instead of being stored twice, so the equality authority cannot drift from
// its projection.
package observation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/audit"
)

// ListRuns pages a connection's observation runs newest-first. next is the
// id the next page must resume after (0 means end).
func (service *Service) ListRuns(ctx context.Context, connectionName string, after int64, limit int) ([]SourceObservationRun, int64, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var connectionID int64
	if err := service.runner.Reader().QueryRowContext(ctx, `SELECT id FROM connections WHERE name=?`, connectionName).Scan(&connectionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, err
	}
	rows, err := service.runner.Reader().QueryContext(ctx, `
		SELECT r.id,r.trigger_kind,r.state,r.row_version,r.evidence_at,r.result_detail,r.created_at
		FROM observation_runs r
		WHERE r.connection_id=? AND r.id>?
		ORDER BY r.id DESC
		LIMIT ?`, connectionID, after, limit+1)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]SourceObservationRun, 0, limit)
	var next int64
	for rows.Next() {
		var id int64
		var detail SourceObservationRun
		detail.ConnectionName = connectionName
		var evidenceAt, resultDetail sql.NullString
		if err := rows.Scan(&id, &detail.TriggerKind, &detail.State, &detail.RowVersion, &evidenceAt, &resultDetail, &detail.CreatedAt); err != nil {
			return nil, 0, err
		}
		if len(items) == limit {
			next = id
			break
		}
		detail.ID = fmt.Sprint(id)
		if evidenceAt.Valid {
			detail.EvidenceAt = &evidenceAt.String
		}
		if resultDetail.Valid {
			detail.ResultDetail = &resultDetail.String
		}
		items = append(items, detail)
	}
	return items, next, rows.Err()
}

// GetRun reads one Run with its frozen object children.
func (service *Service) GetRun(ctx context.Context, connectionName string, runID int64) (SourceObservationRun, error) {
	var connectionID int64
	var detail SourceObservationRun
	detail.ConnectionName = connectionName
	var evidenceAt, resultDetail sql.NullString
	err := service.runner.Reader().QueryRowContext(ctx, `
		SELECT r.connection_id,r.trigger_kind,r.state,r.row_version,r.evidence_at,r.result_detail,r.created_at
		FROM observation_runs r
		JOIN connections c ON c.id=r.connection_id
		WHERE r.id=? AND c.name=?`, runID, connectionName).Scan(
		&connectionID, &detail.TriggerKind, &detail.State, &detail.RowVersion, &evidenceAt, &resultDetail, &detail.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SourceObservationRun{}, ErrNotFound
	}
	if err != nil {
		return SourceObservationRun{}, err
	}
	detail.ID = fmt.Sprint(runID)
	if evidenceAt.Valid {
		detail.EvidenceAt = &evidenceAt.String
	}
	if resultDetail.Valid {
		detail.ResultDetail = &resultDetail.String
	}
	objects, err := service.runObjects(ctx, runID)
	if err != nil {
		return SourceObservationRun{}, err
	}
	detail.Objects = objects
	return detail, nil
}

// runDetailOn reads one Run with its children over the caller's read
// surface; StartRun uses it on the guarded runner transaction to seal the
// command's stored replay payload from the same snapshot it commits.
func (service *Service) runDetailOn(ctx context.Context, conn audit.Reader, connectionName string, runID int64) (SourceObservationRun, error) {
	var detail SourceObservationRun
	detail.ConnectionName = connectionName
	var evidenceAt, resultDetail sql.NullString
	err := conn.QueryRowContext(ctx, `SELECT trigger_kind,state,row_version,evidence_at,result_detail,created_at FROM observation_runs WHERE id=?`, runID).Scan(
		&detail.TriggerKind, &detail.State, &detail.RowVersion, &evidenceAt, &resultDetail, &detail.CreatedAt)
	if err != nil {
		return SourceObservationRun{}, err
	}
	detail.ID = fmt.Sprint(runID)
	if evidenceAt.Valid {
		detail.EvidenceAt = &evidenceAt.String
	}
	if resultDetail.Valid {
		detail.ResultDetail = &resultDetail.String
	}
	rows, err := conn.QueryContext(ctx, `SELECT object_type,status,gap_reason,attempt_id,evidence_id FROM observation_run_objects WHERE observation_run_id=? ORDER BY object_type`, runID)
	if err != nil {
		return SourceObservationRun{}, err
	}
	defer rows.Close()
	objects := []SourceObservationRunObject{}
	for rows.Next() {
		var object SourceObservationRunObject
		var objectType, status string
		var gapReason sql.NullString
		var attemptID, evidenceID sql.NullInt64
		if err := rows.Scan(&objectType, &status, &gapReason, &attemptID, &evidenceID); err != nil {
			return SourceObservationRun{}, err
		}
		object.ObjectType, object.Status = objectType, status
		if gapReason.Valid {
			object.GapReason = &gapReason.String
		}
		if attemptID.Valid {
			id := fmt.Sprint(attemptID.Int64)
			object.AttemptID = &id
		}
		if evidenceID.Valid {
			id := fmt.Sprint(evidenceID.Int64)
			object.EvidenceID = &id
		}
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		return SourceObservationRun{}, err
	}
	detail.Objects = objects
	return detail, nil
}

// runObjects lists the frozen per-object-type children of one Run.
func (service *Service) runObjects(ctx context.Context, runID int64) ([]SourceObservationRunObject, error) {
	rows, err := service.runner.Reader().QueryContext(ctx, `
		SELECT object_type,status,gap_reason,attempt_id,evidence_id
		FROM observation_run_objects
		WHERE observation_run_id=?
		ORDER BY object_type`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	objects := []SourceObservationRunObject{}
	for rows.Next() {
		var object SourceObservationRunObject
		var objectType, status string
		var gapReason sql.NullString
		var attemptID, evidenceID sql.NullInt64
		if err := rows.Scan(&objectType, &status, &gapReason, &attemptID, &evidenceID); err != nil {
			return nil, err
		}
		object.ObjectType, object.Status = objectType, status
		if gapReason.Valid {
			object.GapReason = &gapReason.String
		}
		if attemptID.Valid {
			id := fmt.Sprint(attemptID.Int64)
			object.AttemptID = &id
		}
		if evidenceID.Valid {
			id := fmt.Sprint(evidenceID.Int64)
			object.EvidenceID = &id
		}
		objects = append(objects, object)
	}
	return objects, rows.Err()
}

// ListResources pages a connection's observed source objects. state filters
// over the closed DTO vocabulary (observed/not_observed/stale); objectType
// filters the plugin vocabulary. An empty state keeps every identity row so
// absence history stays visible.
func (service *Service) ListResources(ctx context.Context, connectionName, objectType, state string, after int64, limit int) ([]ResourceSummary, int64, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var connectionID int64
	if err := service.runner.Reader().QueryRowContext(ctx, `SELECT id FROM connections WHERE name=?`, connectionName).Scan(&connectionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, err
	}
	query := `SELECT o.id,o.object_type,o.identity_key,o.display_name,o.labels_json,o.observed_at,o.current,o.stale,o.last_successful_refresh_at
		FROM observed_source_objects o WHERE o.connection_id=? AND o.id>?`
	args := []any{connectionID, after}
	if objectType != "" {
		query += ` AND o.object_type=?`
		args = append(args, objectType)
	}
	switch state {
	case "", "observed", "not_observed", "stale":
		if state != "" {
			query += ` AND ` + statePredicate(state)
		}
	default:
		return nil, 0, fmt.Errorf("invalid source resource state filter %q", state)
	}
	query += ` ORDER BY o.id`
	rows, err := service.runner.Reader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]ResourceSummary, 0, limit)
	var next, lastIncluded int64
	for rows.Next() {
		var id int64
		var item ResourceSummary
		item.ConnectionName = connectionName
		var labelsJSON string
		var displayName, observedAt, lastSuccess sql.NullString
		var current, stale int
		if err := rows.Scan(&id, &item.ObjectType, &item.IdentityKey, &displayName, &labelsJSON, &observedAt, &current, &stale, &lastSuccess); err != nil {
			return nil, 0, err
		}
		item.ID = fmt.Sprint(id)
		if displayName.Valid {
			item.DisplayName = &displayName.String
		}
		if err := json.Unmarshal([]byte(labelsJSON), &item.Labels); err != nil {
			return nil, 0, fmt.Errorf("decode observed source labels: %w", err)
		}
		item.IdentityLabels = DecodeIdentityKey(item.IdentityKey)
		var lastSuccessText string
		if lastSuccess.Valid {
			item.LastSuccessfulRefreshAt = &lastSuccess.String
			lastSuccessText = lastSuccess.String
		}
		item.State = ResourceState(current, stale, lastSuccessText, service.now())
		if observedAt.Valid {
			item.LastObservedAt = &observedAt.String
		}
		if state != "" && item.State != state {
			continue
		}
		if len(items) == limit {
			// The cursor is the last included id: the next page resumes after
			// it, so a filtered page never skips or repeats an item.
			next = lastIncluded
			break
		}
		items = append(items, item)
		lastIncluded = id
	}
	return items, next, rows.Err()
}

// statePredicate translates the closed DTO state onto persisted flags where
// possible. "stale" is time-derived, so that filter evaluates the TTL in Go
// against the candidate page; the SQL keeps it broad (current=0) and rows are
// filtered after decoding.
func statePredicate(state string) string {
	switch state {
	case "observed":
		return "o.current=1"
	case "not_observed":
		return "o.current=0"
	case "stale":
		return "o.current=0"
	}
	return "1=1"
}

// GetResource reads one observed source object of one connection.
func (service *Service) GetResource(ctx context.Context, connectionName string, resourceID int64) (ResourceSummary, error) {
	var item ResourceSummary
	item.ConnectionName = connectionName
	var labelsJSON string
	var displayName, observedAt, lastSuccess sql.NullString
	var current, stale int
	err := service.runner.Reader().QueryRowContext(ctx, `
		SELECT o.object_type,o.identity_key,o.display_name,o.labels_json,o.observed_at,o.current,o.stale,o.last_successful_refresh_at
		FROM observed_source_objects o
		JOIN connections c ON c.id=o.connection_id
		WHERE o.id=? AND c.name=?`, resourceID, connectionName).Scan(
		&item.ObjectType, &item.IdentityKey, &displayName, &labelsJSON, &observedAt, &current, &stale, &lastSuccess)
	if errors.Is(err, sql.ErrNoRows) {
		return ResourceSummary{}, ErrNotFound
	}
	if err != nil {
		return ResourceSummary{}, err
	}
	item.ID = fmt.Sprint(resourceID)
	if displayName.Valid {
		item.DisplayName = &displayName.String
	}
	if err := json.Unmarshal([]byte(labelsJSON), &item.Labels); err != nil {
		return ResourceSummary{}, fmt.Errorf("decode observed source labels: %w", err)
	}
	item.IdentityLabels = DecodeIdentityKey(item.IdentityKey)
	var lastSuccessText string
	if lastSuccess.Valid {
		item.LastSuccessfulRefreshAt = &lastSuccess.String
		lastSuccessText = lastSuccess.String
	}
	item.State = ResourceState(current, stale, lastSuccessText, service.now())
	if observedAt.Valid {
		item.LastObservedAt = &observedAt.String
	}
	return item, nil
}
