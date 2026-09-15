package knowledge

// confirm.go implements the human confirmation boundary: one Confirm
// command (expected draft revision) transitions an AwaitingConfirmation
// diagnosis-source candidate to Confirmed and creates, in the same
// transaction, the Reusable Knowledge aggregate, its first immutable
// KnowledgeVersion, the retrieval state and the eligible FTS projection
// (DATA-KNOWLEDGE-001/002/004). The model never publishes: only this
// user-driven command creates knowledge. Both commands run through the
// shared runner: replay, rejections, ledger and audit are the runner's.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// Confirm confirms one candidate and returns the updated summary carrying
// confirmedKnowledgeId.
func (service *Service) Confirm(ctx context.Context, principalID int64, commandID string, candidateID, expectedRevision int64) (CandidateSummary, error) {
	digest := commandDigest(opConfirm, map[string]any{
		"candidateId":      candidateID,
		"expectedRevision": expectedRevision,
	})
	outcome, err := execution.Run(ctx, service.runner, service.confirm, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: commandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (CandidateSummary, execution.Change, error) {
		var state, sourceType string
		var batchState sql.NullString
		var importBatchID sql.NullInt64
		var draftRevision, sourceID, targetKnowledgeID int64
		var draftTitle, draftBody, draftScope sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT c.state, c.source_type, c.source_id, COALESCE(c.target_knowledge_id,0), c.draft_revision, c.draft_title, c.draft_body, c.draft_scope_json, c.import_batch_id, b.state FROM knowledge_candidates c LEFT JOIN knowledge_import_batches b ON b.id=c.import_batch_id WHERE c.id=?`, candidateID).
			Scan(&state, &sourceType, &sourceID, &targetKnowledgeID, &draftRevision, &draftTitle, &draftBody, &draftScope, &importBatchID, &batchState)
		if errors.Is(err, sql.ErrNoRows) {
			return CandidateSummary{}, execution.Changed, rejectionOf(candidateID, ErrNotFound)
		}
		if err != nil {
			return CandidateSummary{}, execution.Changed, err
		}
		switch {
		case state == StateConfirmed:
			// Each candidate confirms at most once; a different command id is
			// a deterministic conflict (DATA-KNOWLEDGE-004).
			return CandidateSummary{}, execution.Changed, rejectionOf(candidateID, &StateConflict{State: state})
		case state != StateAwaiting:
			return CandidateSummary{}, execution.Changed, rejectionOf(candidateID, &StateConflict{State: state})
		case batchState.Valid && batchState.String != "AwaitingConfirmation":
			return CandidateSummary{}, execution.Changed, rejectionOf(candidateID, &StateConflict{State: batchState.String})
		case draftRevision != expectedRevision:
			return CandidateSummary{}, execution.Changed, rejectionOf(candidateID, &RevisionConflict{Current: draftRevision})
		}
		now := service.nowText()
		title, body := draftValues(draftTitle, draftBody)
		var scopeValue any
		if draftScope.Valid && draftScope.String != "" && draftScope.String != "{}" {
			specific := draftScope.String
			scopeValue = &specific
		}
		// 1. A normal candidate creates an aggregate. A knowledge_version candidate
		// appends to its target aggregate instead — a revision can never fork a
		// second Reusable Knowledge identity.
		knowledgeID := targetKnowledgeID
		versionSeq := int64(1)
		if knowledgeID == 0 {
			knowledgeInsert, insertErr := tx.ExecContext(ctx, `INSERT INTO reusable_knowledge(created_by,created_at) VALUES(?,?)`, principalID, now)
			if insertErr != nil {
				return CandidateSummary{}, execution.Changed, insertErr
			}
			knowledgeID, err = knowledgeInsert.LastInsertId()
			if err != nil {
				return CandidateSummary{}, execution.Changed, err
			}
		} else if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version_seq),0)+1 FROM knowledge_versions WHERE knowledge_id=?`, knowledgeID).Scan(&versionSeq); err != nil {
			return CandidateSummary{}, execution.Changed, err
		}
		// 2. The candidate flips to Confirmed bound to this knowledge (the
		// version-insert trigger requires exactly this closure).
		result, err := tx.ExecContext(ctx, `
			UPDATE knowledge_candidates
			SET state='Confirmed', confirmed_knowledge_id=?, row_version=row_version+1
			WHERE id=? AND state='AwaitingConfirmation' AND draft_revision=?`,
			knowledgeID, candidateID, expectedRevision)
		if err != nil {
			return CandidateSummary{}, execution.Changed, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return CandidateSummary{}, execution.Changed, err
		}
		if affected == 0 {
			return CandidateSummary{}, execution.Changed, rejectionOf(candidateID, &RevisionConflict{Current: draftRevision + 1})
		}
		// 3. The next immutable version (append-only).
		versionInsert, err := tx.ExecContext(ctx, `
			INSERT INTO knowledge_versions(knowledge_id,version_seq,title,body,scope_json,source_candidate_id,created_by,created_at)
			VALUES(?,?,?,?,?,?,?,?)`, knowledgeID, versionSeq, title, body, scopeValue, candidateID, principalID, now)
		if err != nil {
			return CandidateSummary{}, execution.Changed, err
		}
		versionID, err := versionInsert.LastInsertId()
		if err != nil {
			return CandidateSummary{}, execution.Changed, err
		}
		// 4. Retire the former current document before advancing the pointer so
		// search remains exactly current ∧ eligible (new knowledge has NULL).
		if _, err := tx.ExecContext(ctx, `DELETE FROM knowledge_search_docs WHERE knowledge_version_id=(SELECT current_version_id FROM reusable_knowledge WHERE id=?)`, knowledgeID); err != nil {
			return CandidateSummary{}, execution.Changed, err
		}
		// The current pointer advances in the same transaction (DATA-TX-008).
		if _, err := tx.ExecContext(ctx, `UPDATE reusable_knowledge SET current_version_id=?, row_version=row_version+1 WHERE id=?`, versionID, knowledgeID); err != nil {
			return CandidateSummary{}, execution.Changed, err
		}
		// 5. Retrieval state + the eligible FTS projection row (the schema
		// trigger syncs knowledge_fts).
		if _, err := tx.ExecContext(ctx, `INSERT INTO knowledge_version_retrieval_state(knowledge_version_id,updated_at) VALUES(?,?)`, versionID, now); err != nil {
			return CandidateSummary{}, execution.Changed, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO knowledge_search_docs(knowledge_version_id,title,body) VALUES(?,?,?)`, versionID, title, body); err != nil {
			return CandidateSummary{}, execution.Changed, err
		}
		if err := completeImportBatchIfNoPending(ctx, tx, importBatchID); err != nil {
			return CandidateSummary{}, execution.Changed, err
		}
		summary, err := scanCandidateOn(ctx, tx, candidateID)
		if err != nil {
			return CandidateSummary{}, execution.Changed, err
		}
		if summary.ConfirmedKnowledgeID != fmt.Sprintf("%d", knowledgeID) {
			return CandidateSummary{}, execution.Changed, errors.New("confirmed knowledge binding missing after commit")
		}
		return summary, execution.Changed, nil
	}, func(summary CandidateSummary) int64 { return parseCandidateLocator(summary.ID) })
	if err != nil {
		return CandidateSummary{}, service.translateCommandError(ctx, err)
	}
	return outcome.Result, nil
}

// Exclude removes an AwaitingConfirmation candidate from confirmation
// (user decision; VersionedCommandRequest fence).
func (service *Service) Exclude(ctx context.Context, principalID int64, commandID string, candidateID, expectedRowVersion int64) (CandidateSummary, error) {
	digest := commandDigest(opExclude, map[string]any{
		"candidateId":        candidateID,
		"expectedRowVersion": expectedRowVersion,
	})
	outcome, err := execution.Run(ctx, service.runner, service.exclude, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: commandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (CandidateSummary, execution.Change, error) {
		var importBatchID sql.NullInt64
		var batchState sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT c.import_batch_id,b.state FROM knowledge_candidates c LEFT JOIN knowledge_import_batches b ON b.id=c.import_batch_id WHERE c.id=?`, candidateID).Scan(&importBatchID, &batchState); errors.Is(err, sql.ErrNoRows) {
			return CandidateSummary{}, execution.Changed, rejectionOf(candidateID, ErrNotFound)
		} else if err != nil {
			return CandidateSummary{}, execution.Changed, err
		}
		if batchState.Valid && batchState.String != "AwaitingConfirmation" {
			return CandidateSummary{}, execution.Changed, rejectionOf(candidateID, &StateConflict{State: batchState.String})
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE knowledge_candidates
			SET state='Excluded', row_version=row_version+1
			WHERE id=? AND state='AwaitingConfirmation' AND row_version=?`, candidateID, expectedRowVersion)
		if err != nil {
			return CandidateSummary{}, execution.Changed, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return CandidateSummary{}, execution.Changed, err
		}
		if affected == 0 {
			kind, infraErr := service.excludeConflict(ctx, tx, candidateID)
			if infraErr != nil {
				return CandidateSummary{}, execution.Changed, infraErr
			}
			return CandidateSummary{}, execution.Changed, rejectionOf(candidateID, kind)
		}
		summary, err := scanCandidateOn(ctx, tx, candidateID)
		if err != nil {
			return CandidateSummary{}, execution.Changed, err
		}
		if err := completeImportBatchIfNoPending(ctx, tx, importBatchID); err != nil {
			return CandidateSummary{}, execution.Changed, err
		}
		return summary, execution.Changed, nil
	}, func(summary CandidateSummary) int64 { return parseCandidateLocator(summary.ID) })
	if err != nil {
		return CandidateSummary{}, service.translateCommandError(ctx, err)
	}
	return outcome.Result, nil
}

// excludeConflict classifies a zero-row exclude.
func (service *Service) excludeConflict(ctx context.Context, q queryer, candidateID int64) (error, error) {
	var state string
	var rowVersion int64
	err := q.QueryRowContext(ctx, `SELECT state, row_version FROM knowledge_candidates WHERE id=?`, candidateID).Scan(&state, &rowVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound, nil
	}
	if err != nil {
		return nil, err
	}
	if state != StateAwaiting {
		return &StateConflict{State: state}, nil
	}
	return &RowVersionConflict{Current: rowVersion}, nil
}

func draftValues(title, body sql.NullString) (string, string) {
	titleValue := ""
	if title.Valid {
		titleValue = title.String
	}
	bodyValue := ""
	if body.Valid {
		bodyValue = body.String
	}
	if titleValue == "" {
		titleValue = "未命名知识"
	}
	if bodyValue == "" {
		bodyValue = titleValue
	}
	return titleValue, bodyValue
}

// completeImportBatchIfNoPending closes an import batch once the user has
// decided every candidate in its current generation. The caller owns the same
// runner transaction as the confirm/exclude command.
func completeImportBatchIfNoPending(ctx context.Context, w writer, batchID sql.NullInt64) error {
	if !batchID.Valid {
		return nil
	}
	_, err := w.ExecContext(ctx, `UPDATE knowledge_import_batches
		SET state='Completed',row_version=row_version+1
		WHERE id=? AND state='AwaitingConfirmation'
		AND NOT EXISTS (
			SELECT 1 FROM knowledge_candidates c
			WHERE c.import_batch_id=knowledge_import_batches.id
			AND c.generation=knowledge_import_batches.generation
			AND c.state='AwaitingConfirmation'
		)`, batchID.Int64)
	return err
}
