// Package invalidation owns the DATA-TX-011 source withdrawal transition:
// when a diagnosis source is rejected or an investigation turn is undone,
// the same transaction flips the source's still-operable candidates to
// SourceInvalid and permanently exits every KnowledgeVersion the source
// produced (sticky exit: the schema forbids reactivation, and
// knowledge_search_docs rows disappear with the FTS5 index through its
// triggers).
package invalidation

import (
	"context"
	"database/sql"
)

// Source type constants of the knowledge candidate sources this package
// invalidates (diagnosis_feedback.target_type CHECK vocabulary).
const (
	SourceAnalysisOutput       = "initial_analysis_output"
	SourceReport               = "inspection_report"
	SourceInvestigationMessage = "investigation_message"
)

// Outcome reports one invalidation pass over a set of withdrawn sources.
type Outcome struct {
	CandidatesInvalidated int64
	VersionsExited        int64
}

// Apply invalidates every knowledge candidate and confirmed version whose
// source is one of the given immutable diagnosis sources. It runs inside
// the caller's open transaction (Undo or the feedback append) so the
// withdrawal commits atomically with the domain event that caused it.
// This is the single DATA-TX-011 writer for both rejected feedback and
// undone turns.
func Apply(ctx context.Context, conn *sql.Conn, sourceType string, sourceIDs []int64, now string) (Outcome, error) {
	var outcome Outcome
	if len(sourceIDs) == 0 {
		return outcome, nil
	}
	// Pending candidates of the withdrawn sources become unconfirmable.
	for _, sourceID := range sourceIDs {
		result, err := conn.ExecContext(ctx, `
			UPDATE knowledge_candidates
			SET state='SourceInvalid', row_version=row_version+1
			WHERE source_type=? AND source_id=? AND state='AwaitingConfirmation'`,
			sourceType, sourceID)
		if err != nil {
			return outcome, err
		}
		if affected, lookupErr := result.RowsAffected(); lookupErr == nil {
			outcome.CandidatesInvalidated += affected
		}
	}
	// Every immutable version the source produced exits retrieval
	// permanently; already-exited rows keep their original exit facts
	// (idempotent for repeated events — the sticky-exit trigger forbids
	// touching them again).
	for _, sourceID := range sourceIDs {
		rows, err := conn.QueryContext(ctx, `
			SELECT v.id FROM knowledge_versions v
			JOIN knowledge_candidates c ON c.id=v.source_candidate_id
			WHERE c.source_type=? AND c.source_id=?`, sourceType, sourceID)
		if err != nil {
			return outcome, err
		}
		var versionIDs []int64
		for rows.Next() {
			var versionID int64
			if err := rows.Scan(&versionID); err != nil {
				rows.Close()
				return outcome, err
			}
			versionIDs = append(versionIDs, versionID)
		}
		if err := rows.Close(); err != nil {
			return outcome, err
		}
		exited, exitErr := exitVersions(ctx, conn, versionIDs, now)
		if exitErr != nil {
			return outcome, exitErr
		}
		outcome.VersionsExited += exited
	}
	return outcome, nil
}

// exitVersions applies the sticky retrieval exit and projection deletion
// for versions that have not exited yet; any failure aborts the caller's
// whole transaction (DATA-TX-011 is all-or-nothing).
func exitVersions(ctx context.Context, conn *sql.Conn, versionIDs []int64, now string) (int64, error) {
	var exited int64
	for _, versionID := range versionIDs {
		result, err := conn.ExecContext(ctx, `
			UPDATE knowledge_version_retrieval_state
			SET exited=1, exited_at=?, exit_reason='source_rejected', updated_at=?, row_version=row_version+1
			WHERE knowledge_version_id=? AND exited=0`, now, now, versionID)
		if err != nil {
			return exited, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return exited, err
		}
		if affected == 1 {
			exited++
			if _, err := conn.ExecContext(ctx, `DELETE FROM knowledge_search_docs WHERE knowledge_version_id=?`, versionID); err != nil {
				return exited, err
			}
		}
	}
	return exited, nil
}

var _ = sql.ErrNoRows
