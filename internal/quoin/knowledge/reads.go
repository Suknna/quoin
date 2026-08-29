package knowledge

// reads.go holds the read projections: candidate detail/list, the
// knowledge browse list, and the immutable version views. Cursors are
// typed keyset values; the HTTP layer encodes them opaquely.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// CandidateCursor pages the candidate list: awaiting-first flag, then
// createdAt DESC, id DESC (the frozen default ordering). Filter binds the
// active list filters into the cursor (HTTP-PAGE-001).
type CandidateCursor struct {
	AwaitingFirst int
	CreatedAt     string
	ID            int64
	Filter        ListFilter
}

// ListFilter narrows the candidate list; the cursor binds the same values.
type ListFilter struct {
	State      string
	SourceType string
}

// KnowledgeCursor pages the browse list by the current version's creation
// time (createdAt DESC, id DESC).
type KnowledgeCursor struct {
	CreatedAt string
	ID        int64
}

// VersionCursor pages versions newest-first (id DESC).
type VersionCursor struct {
	ID int64
}

// ListResult carries one page plus the next keyset cursor.
type ListResult[T any] struct {
	Items []T
	Next  *T
}

const candidateColumns = `
	c.id, c.source_type, c.source_id, c.state, c.row_version, c.generation,
	c.draft_revision, COALESCE(c.draft_title,''), COALESCE(c.draft_body,''), c.draft_scope_json,
	COALESCE(c.target_knowledge_id,0), COALESCE(c.confirmed_knowledge_id,0)`

// scanCandidateOn reads one candidate row through the given connection.
func scanCandidateOn(ctx context.Context, conn *sql.Conn, candidateID int64) (CandidateSummary, error) {
	row := conn.QueryRowContext(ctx, `SELECT `+candidateColumns+` FROM knowledge_candidates c WHERE c.id=?`, candidateID)
	return readCandidateRow(row.Scan)
}

func (service *Service) candidateSummaryOn(ctx context.Context, conn *sql.Conn, candidateID int64) (CandidateSummary, error) {
	return scanCandidateOn(ctx, conn, candidateID)
}

// candidateBySource returns any-state candidate of one immutable source.
func (service *Service) candidateBySource(ctx context.Context, conn *sql.Conn, sourceType string, sourceID int64) (CandidateSummary, bool, error) {
	row := conn.QueryRowContext(ctx, `SELECT `+candidateColumns+` FROM knowledge_candidates c WHERE c.source_type=? AND c.source_id=? ORDER BY c.id LIMIT 1`, sourceType, sourceID)
	summary, err := readCandidateRow(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return CandidateSummary{}, false, nil
	}
	if err != nil {
		return CandidateSummary{}, false, err
	}
	return summary, true, nil
}

// readCandidateRow materializes the wire summary from one row scan.
func readCandidateRow(scan func(dest ...any) error) (CandidateSummary, error) {
	var id, sourceID, rowVersion, generation, draftRevision, targetKnowledge, confirmedKnowledge int64
	var sourceType, state, draftTitle, draftBody string
	var draftScope sql.NullString
	if err := scan(&id, &sourceType, &sourceID, &state, &rowVersion, &generation, &draftRevision, &draftTitle, &draftBody, &draftScope, &targetKnowledge, &confirmedKnowledge); err != nil {
		return CandidateSummary{}, err
	}
	summary := CandidateSummary{
		ID:            fmt.Sprintf("%d", id),
		SourceType:    sourceType,
		SourceID:      fmt.Sprintf("%d", sourceID),
		State:         state,
		RowVersion:    rowVersion,
		Generation:    generation,
		DraftRevision: draftRevision,
		DraftTitle:    draftTitle,
		DraftBody:     draftBody,
	}
	if draftScope.Valid && draftScope.String != "" {
		summary.DraftScope = json.RawMessage(draftScope.String)
	}
	if targetKnowledge != 0 {
		summary.TargetKnowledgeID = fmt.Sprintf("%d", targetKnowledge)
	}
	if confirmedKnowledge != 0 {
		summary.ConfirmedKnowledgeID = fmt.Sprintf("%d", confirmedKnowledge)
	}
	return summary, nil
}

// GetCandidate returns the candidate detail with the immutable original
// suggestion projection.
func (service *Service) GetCandidate(ctx context.Context, candidateID int64) (CandidateDetail, error) {
	row := service.db.QueryRowContext(ctx, `
		SELECT `+candidateColumns+`, c.original_suggestion_json
		FROM knowledge_candidates c WHERE c.id=?`, candidateID)
	var summary CandidateSummary
	var id, sourceID, targetKnowledge, confirmedKnowledge int64
	var draftScope sql.NullString
	var original []byte
	if err := row.Scan(&id, &summary.SourceType, &sourceID, &summary.State, &summary.RowVersion, &summary.Generation,
		&summary.DraftRevision, &summary.DraftTitle, &summary.DraftBody, &draftScope, &targetKnowledge, &confirmedKnowledge, &original); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CandidateDetail{}, ErrNotFound
		}
		return CandidateDetail{}, err
	}
	summary.ID = fmt.Sprintf("%d", id)
	summary.SourceID = fmt.Sprintf("%d", sourceID)
	if targetKnowledge != 0 {
		summary.TargetKnowledgeID = fmt.Sprintf("%d", targetKnowledge)
	}
	if confirmedKnowledge != 0 {
		summary.ConfirmedKnowledgeID = fmt.Sprintf("%d", confirmedKnowledge)
	}
	if draftScope.Valid && draftScope.String != "" {
		summary.DraftScope = json.RawMessage(draftScope.String)
	}
	return CandidateDetail{CandidateSummary: summary, OriginalSuggestion: append([]byte(nil), original...)}, nil
}

// ListCandidates pages candidates with the frozen default ordering
// (awaiting first, then createdAt DESC) or the filtered ordering.
func (service *Service) ListCandidates(ctx context.Context, filter ListFilter, after *CandidateCursor, limit int) ([]CandidateSummary, *CandidateCursor, error) {
	where := ""
	args := make([]any, 0, 6)
	if filter.State != "" {
		where += " AND c.state=?"
		args = append(args, filter.State)
	}
	if filter.SourceType != "" {
		where += " AND c.source_type=?"
		args = append(args, filter.SourceType)
	}
	orderExpr := "(c.state='AwaitingConfirmation') DESC, c.created_at DESC, c.id DESC"
	if after != nil {
		awaiting := 0
		if after.AwaitingFirst == 1 {
			awaiting = 1
		}
		where += " AND ((c.state='AwaitingConfirmation')=? OR ((c.state='AwaitingConfirmation')=? AND (c.created_at < ? OR (c.created_at = ? AND c.id < ?))))"
		args = append(args, awaiting, awaiting, after.CreatedAt, after.CreatedAt, after.ID)
	}
	query := `SELECT ` + candidateColumns + `, c.created_at, (c.state='AwaitingConfirmation') FROM knowledge_candidates c WHERE 1=1` + where + ` ORDER BY ` + orderExpr + ` LIMIT ?`
	args = append(args, limit+1)
	rows, err := service.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items := make([]CandidateSummary, 0, limit)
	type boundary struct {
		awaiting  int
		createdAt string
		id        int64
	}
	boundaries := make([]boundary, 0, limit+1)
	for rows.Next() {
		var id, sourceID, rowVersion, generation, draftRevision, targetKnowledge, confirmedKnowledge int64
		var sourceType, state, draftTitle, draftBody, createdAt string
		var draftScope sql.NullString
		var awaiting int
		if err := rows.Scan(&id, &sourceType, &sourceID, &state, &rowVersion, &generation, &draftRevision, &draftTitle, &draftBody, &draftScope, &targetKnowledge, &confirmedKnowledge, &createdAt, &awaiting); err != nil {
			return nil, nil, err
		}
		summary := CandidateSummary{
			ID: fmt.Sprintf("%d", id), SourceType: sourceType, SourceID: fmt.Sprintf("%d", sourceID),
			State: state, RowVersion: rowVersion, Generation: generation, DraftRevision: draftRevision,
			DraftTitle: draftTitle, DraftBody: draftBody,
		}
		if draftScope.Valid && draftScope.String != "" {
			summary.DraftScope = json.RawMessage(draftScope.String)
		}
		if targetKnowledge != 0 {
			summary.TargetKnowledgeID = fmt.Sprintf("%d", targetKnowledge)
		}
		if confirmedKnowledge != 0 {
			summary.ConfirmedKnowledgeID = fmt.Sprintf("%d", confirmedKnowledge)
		}
		items = append(items, summary)
		boundaries = append(boundaries, boundary{awaiting: awaiting, createdAt: createdAt, id: id})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *CandidateCursor
	if len(items) > limit {
		items = items[:limit]
		// The keyset boundary is the last EMITTED row; recording the
		// (limit+1)-th row would skip it on the next page.
		edge := boundaries[len(items)-1]
		next = &CandidateCursor{AwaitingFirst: edge.awaiting, CreatedAt: edge.createdAt, ID: edge.id}
	}
	return items, next, nil
}

// Browse lists eligible knowledge (current ∧ not exited) ordered by the
// current version's creation time.
func (service *Service) Browse(ctx context.Context, after *KnowledgeCursor, limit int) ([]KnowledgeSummary, *KnowledgeCursor, error) {
	where := ""
	args := make([]any, 0, 4)
	where += ` AND k.current_version_id IS NOT NULL AND NOT EXISTS (
		SELECT 1 FROM knowledge_version_retrieval_state s
		WHERE s.knowledge_version_id=k.current_version_id AND s.exited=1)`
	if after != nil {
		where += ` AND (v.created_at < ? OR (v.created_at = ? AND k.id < ?))`
		args = append(args, after.CreatedAt, after.CreatedAt, after.ID)
	}
	query := `
		SELECT k.id, v.title, v.id, v.version_seq, k.row_version, v.created_at
		FROM reusable_knowledge k JOIN knowledge_versions v ON v.id=k.current_version_id
		WHERE 1=1` + where + ` ORDER BY v.created_at DESC, k.id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := service.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items := make([]KnowledgeSummary, 0, limit)
	type edge struct {
		createdAt string
		id        int64
	}
	edges := make([]edge, 0, limit+1)
	for rows.Next() {
		var knowledgeID, versionID, versionSeq, rowVersion int64
		var title, createdAt string
		if err := rows.Scan(&knowledgeID, &title, &versionID, &versionSeq, &rowVersion, &createdAt); err != nil {
			return nil, nil, err
		}
		items = append(items, KnowledgeSummary{
			ID: fmt.Sprintf("%d", knowledgeID), Title: title, CurrentVersionID: fmt.Sprintf("%d", versionID),
			CurrentVersionSeq: versionSeq, Eligible: true, RowVersion: rowVersion,
		})
		edges = append(edges, edge{createdAt: createdAt, id: knowledgeID})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *KnowledgeCursor
	if len(items) > limit {
		items = items[:limit]
		// The keyset boundary is the last EMITTED row.
		boundary := edges[len(items)-1]
		next = &KnowledgeCursor{CreatedAt: boundary.createdAt, ID: boundary.id}
	}
	return items, next, nil
}

// GetKnowledge returns the aggregate detail (bounded version count only;
// history pages through ListVersions).
func (service *Service) GetKnowledge(ctx context.Context, knowledgeID int64) (KnowledgeDetail, error) {
	row := service.db.QueryRowContext(ctx, `
		SELECT k.id, v.title, v.id, v.version_seq, k.row_version,
		       (SELECT COUNT(*) FROM knowledge_versions x WHERE x.knowledge_id=k.id),
		       NOT EXISTS (SELECT 1 FROM knowledge_version_retrieval_state s WHERE s.knowledge_version_id=k.current_version_id AND s.exited=1)
		FROM reusable_knowledge k JOIN knowledge_versions v ON v.id=k.current_version_id
		WHERE k.id=?`, knowledgeID)
	var id, versionID, versionSeq, rowVersion, versionCount int64
	var title string
	var eligible bool
	if err := row.Scan(&id, &title, &versionID, &versionSeq, &rowVersion, &versionCount, &eligible); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return KnowledgeDetail{}, ErrNotFound
		}
		return KnowledgeDetail{}, err
	}
	return KnowledgeDetail{
		KnowledgeSummary: KnowledgeSummary{
			ID: fmt.Sprintf("%d", id), Title: title, CurrentVersionID: fmt.Sprintf("%d", versionID),
			CurrentVersionSeq: versionSeq, Eligible: eligible, RowVersion: rowVersion,
		},
		VersionCount: versionCount,
	}, nil
}

// ListVersions pages the immutable version history newest-first.
func (service *Service) ListVersions(ctx context.Context, knowledgeID int64, after *VersionCursor, limit int) ([]VersionSummary, *VersionCursor, error) {
	where := " AND v.knowledge_id=?"
	args := []any{knowledgeID}
	if after != nil {
		where += " AND v.id < ?"
		args = append(args, after.ID)
	}
	query := `
		SELECT v.id, v.version_seq, v.title, v.source_candidate_id, v.created_at,
		       NOT EXISTS (SELECT 1 FROM knowledge_version_retrieval_state s WHERE s.knowledge_version_id=v.id AND s.exited=1),
		       s.row_version
		FROM knowledge_versions v JOIN knowledge_version_retrieval_state s ON s.knowledge_version_id=v.id
		WHERE 1=1` + where + ` ORDER BY v.id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := service.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items := make([]VersionSummary, 0, limit)
	ids := make([]int64, 0, limit+1)
	for rows.Next() {
		var id, versionSeq, sourceCandidate, retrievalRowVersion int64
		var title, createdAt string
		var eligible bool
		if err := rows.Scan(&id, &versionSeq, &title, &sourceCandidate, &createdAt, &eligible, &retrievalRowVersion); err != nil {
			return nil, nil, err
		}
		items = append(items, VersionSummary{
			ID: fmt.Sprintf("%d", id), VersionSeq: versionSeq, Title: title,
			SourceCandidateID: fmt.Sprintf("%d", sourceCandidate), EmbeddingState: "not_configured",
			CreatedAt: createdAt, Eligible: eligible, RetrievalStateRowVersion: retrievalRowVersion,
		})
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *VersionCursor
	if len(items) > limit {
		items = items[:limit]
		// The keyset boundary is the last EMITTED row.
		next = &VersionCursor{ID: ids[len(items)-1]}
	}
	return items, next, nil
}

// GetVersion returns one immutable version detail within its knowledge.
func (service *Service) GetVersion(ctx context.Context, knowledgeID, versionID int64) (VersionDetail, error) {
	row := service.db.QueryRowContext(ctx, `
		SELECT v.id, v.version_seq, v.title, v.body, v.scope_json, v.conditions_json, v.limitations_json,
		       v.source_candidate_id, v.created_at, s.exited, s.exited_at, s.exit_reason, s.row_version
		FROM knowledge_versions v
		JOIN knowledge_version_retrieval_state s ON s.knowledge_version_id=v.id
		WHERE v.knowledge_id=? AND v.id=?`, knowledgeID, versionID)
	var id, versionSeq, sourceCandidate, retrievalRowVersion, exited int64
	var title, body, createdAt string
	var scope, conditions, limitations, exitedAt, exitReason sql.NullString
	if err := row.Scan(&id, &versionSeq, &title, &body, &scope, &conditions, &limitations, &sourceCandidate, &createdAt, &exited, &exitedAt, &exitReason, &retrievalRowVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return VersionDetail{}, ErrNotFound
		}
		return VersionDetail{}, err
	}
	detail := VersionDetail{
		ID: fmt.Sprintf("%d", id), VersionSeq: versionSeq, Title: title, Body: body,
		SourceCandidateID: fmt.Sprintf("%d", sourceCandidate), CreatedAt: createdAt,
		Eligible: exited == 0, RetrievalStateRowVersion: retrievalRowVersion, EmbeddingState: "not_configured",
	}
	if scope.Valid {
		detail.Scope = json.RawMessage(scope.String)
	}
	if conditions.Valid {
		detail.Conditions = json.RawMessage(conditions.String)
	}
	if limitations.Valid {
		detail.Limitations = json.RawMessage(limitations.String)
	}
	if exitedAt.Valid {
		detail.ExitedAt = exitedAt.String
	}
	if exitReason.Valid {
		detail.ExitReason = exitReason.String
	}
	return detail, nil
}
