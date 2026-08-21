package analysis

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// Read projections for the HTTP surface (get/list/attempts).

// VerifyOccurrence fences one analysis to its path occurrence: the frozen
// route shape is /alerts/{occurrenceId}/analyses/{analysisId}, so a
// cross-occurrence locator must answer 404, not the other occurrence's
// data.
func (service *Service) VerifyOccurrence(ctx context.Context, analysisID, occurrenceID int64) error {
	var stored int64
	err := service.db.QueryRowContext(ctx, `SELECT occurrence_id FROM initial_analyses WHERE id=?`, analysisID).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) || stored != occurrenceID {
		return ErrNotFound
	}
	return err
}

// Get returns the analysis detail projection.
func (service *Service) Get(ctx context.Context, analysisID int64) (Detail, error) {
	var detail Detail
	var id int64
	err := service.db.QueryRowContext(ctx, `
		SELECT id,state,row_version,created_at FROM initial_analyses WHERE id=?`, analysisID).
		Scan(&id, &detail.State, &detail.RowVersion, &detail.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Detail{}, ErrNotFound
	}
	if err != nil {
		return Detail{}, err
	}
	detail.ID = strconv.FormatInt(id, 10)
	if err := service.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM execution_attempts WHERE scope_type='analysis' AND scope_id=?`, analysisID).
		Scan(&detail.AttemptCount); err != nil {
		return Detail{}, err
	}
	var outputID int64
	var modelID, content, outputCreated string
	err = service.db.QueryRowContext(ctx, `
		SELECT id,model_id,content,created_at FROM initial_analysis_outputs WHERE analysis_id=?`,
		analysisID).Scan(&outputID, &modelID, &content, &outputCreated)
	if errors.Is(err, sql.ErrNoRows) {
		return detail, nil
	}
	if err != nil {
		return Detail{}, err
	}
	detail.Output = &Output{
		ID: strconv.FormatInt(outputID, 10), ModelID: modelID, Content: content, CreatedAt: outputCreated,
		EvidenceIDs: []string{},
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT evidence_id FROM initial_analysis_output_evidence WHERE output_id=? ORDER BY ordinal`, outputID)
	if err != nil {
		return Detail{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var evidenceID int64
		if err := rows.Scan(&evidenceID); err != nil {
			return Detail{}, err
		}
		detail.Output.EvidenceIDs = append(detail.Output.EvidenceIDs, strconv.FormatInt(evidenceID, 10))
	}
	return detail, rows.Err()
}

// ListByOccurrence returns the analysis history of one occurrence, newest
// first (keyset pagination on id).
func (service *Service) ListByOccurrence(ctx context.Context, occurrenceID int64, after int64, limit int) ([]Summary, int64, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT id,state,row_version,created_at FROM initial_analyses
		WHERE occurrence_id=? AND (?=0 OR id<?) ORDER BY id DESC LIMIT ?`,
		occurrenceID, after, after, limit+1)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []Summary{}
	var nextAfter int64
	for rows.Next() {
		var summary Summary
		var id int64
		if err := rows.Scan(&id, &summary.State, &summary.RowVersion, &summary.CreatedAt); err != nil {
			return nil, 0, err
		}
		summary.ID = strconv.FormatInt(id, 10)
		items = append(items, summary)
		nextAfter = id
	}
	if len(items) > limit {
		items = items[:limit]
		last, _ := strconv.ParseInt(items[len(items)-1].ID, 10, 64)
		nextAfter = last
	} else {
		nextAfter = 0
	}
	return items, nextAfter, rows.Err()
}

// ListAttempts returns the attempt history of one analysis, oldest first
// execution order (retry appends).
func (service *Service) ListAttempts(ctx context.Context, analysisID int64, after int64, limit int) ([]AttemptItem, int64, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT id,attempt_type,state,row_version,started_at,ended_at,termination_reason,created_at
		FROM execution_attempts
		WHERE scope_type='analysis' AND scope_id=? AND (?=0 OR id>?) ORDER BY id ASC LIMIT ?`,
		analysisID, after, after, limit+1)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []AttemptItem{}
	var nextAfter int64
	for rows.Next() {
		var item AttemptItem
		var id int64
		var started, ended, reason sql.NullString
		if err := rows.Scan(&id, &item.Type, &item.State, &item.RowVersion, &started, &ended, &reason, &item.CreatedAt); err != nil {
			return nil, 0, err
		}
		item.ID = strconv.FormatInt(id, 10)
		if started.Valid {
			item.StartedAt = &started.String
		}
		if ended.Valid {
			item.EndedAt = &ended.String
		}
		if reason.Valid {
			item.TerminationReason = &reason.String
		}
		items = append(items, item)
		nextAfter = id
	}
	if len(items) > limit {
		items = items[:limit]
	} else {
		nextAfter = 0
	}
	return items, nextAfter, rows.Err()
}
