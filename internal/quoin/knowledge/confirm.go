package knowledge

// confirm.go implements the human confirmation boundary: one Confirm
// command (expected draft revision) transitions an AwaitingConfirmation
// diagnosis-source candidate to Confirmed and creates, in the same
// transaction, the Reusable Knowledge aggregate, its first immutable
// KnowledgeVersion, the retrieval state and the eligible FTS projection
// (DATA-KNOWLEDGE-001/002/004). The model never publishes: only this
// user-driven command creates knowledge.

import (
	"context"
	"database/sql"
	"errors"

	"fmt"
	"github.com/Suknna/quoin/internal/quoin/auth"
)

// Confirm confirms one candidate and returns the updated summary carrying
// confirmedKnowledgeId.
func (service *Service) Confirm(ctx context.Context, principalID int64, commandID string, candidateID, expectedRevision int64) (CandidateSummary, error) {
	return service.ConfirmAs(ctx, MutationActor{ID: principalID}, commandID, candidateID, expectedRevision)
}

func (service *Service) ConfirmAs(ctx context.Context, actor MutationActor, commandID string, candidateID, expectedRevision int64) (CandidateSummary, error) {
	principalID := actor.ID
	digest := commandDigest(ledgerConfirm, map[string]any{
		"candidateId":      candidateID,
		"expectedRevision": expectedRevision,
	})
	if record, ok, err := auth.LookupCommand(ctx, service.db, principalID, commandID); err != nil {
		return CandidateSummary{}, err
	} else if ok {
		return replaySummary(record, digest)
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return CandidateSummary{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return CandidateSummary{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if err := verifyMutationActorOn(ctx, conn, actor); err != nil {
		return CandidateSummary{}, err
	}
	if record, ok, err := authLookup(ctx, conn, principalID, commandID); err != nil {
		return CandidateSummary{}, err
	} else if ok {
		summary, replayErr := replaySummary(record, digest)
		if replayErr == nil {
			if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
				return CandidateSummary{}, err
			}
			committed = true
		}
		return summary, replayErr
	}
	var state, sourceType string
	var batchState sql.NullString
	var importBatchID sql.NullInt64
	var draftRevision, sourceID, targetKnowledgeID int64
	var draftTitle, draftBody, draftScope sql.NullString
	err = conn.QueryRowContext(ctx, `SELECT c.state, c.source_type, c.source_id, COALESCE(c.target_knowledge_id,0), c.draft_revision, c.draft_title, c.draft_body, c.draft_scope_json, c.import_batch_id, b.state FROM knowledge_candidates c LEFT JOIN knowledge_import_batches b ON b.id=c.import_batch_id WHERE c.id=?`, candidateID).
		Scan(&state, &sourceType, &sourceID, &targetKnowledgeID, &draftRevision, &draftTitle, &draftBody, &draftScope, &importBatchID, &batchState)
	if errors.Is(err, sql.ErrNoRows) {
		return CandidateSummary{}, rejectCandidateCommand(ctx, conn, principalID, commandID, ledgerConfirm, digest, candidateID, ErrNotFound)
	}
	if err != nil {
		return CandidateSummary{}, err
	}
	switch {
	case state == StateConfirmed:
		// Each candidate confirms at most once; a different command id is
		// a deterministic conflict (DATA-KNOWLEDGE-004).
		return CandidateSummary{}, rejectCandidateCommand(ctx, conn, principalID, commandID, ledgerConfirm, digest, candidateID, &StateConflict{State: state})
	case state != StateAwaiting:
		return CandidateSummary{}, rejectCandidateCommand(ctx, conn, principalID, commandID, ledgerConfirm, digest, candidateID, &StateConflict{State: state})
	case batchState.Valid && batchState.String != "AwaitingConfirmation":
		return CandidateSummary{}, rejectCandidateCommand(ctx, conn, principalID, commandID, ledgerConfirm, digest, candidateID, &StateConflict{State: batchState.String})
	case draftRevision != expectedRevision:
		return CandidateSummary{}, rejectCandidateCommand(ctx, conn, principalID, commandID, ledgerConfirm, digest, candidateID, &RevisionConflict{Current: draftRevision})
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
		knowledgeInsert, insertErr := conn.ExecContext(ctx, `INSERT INTO reusable_knowledge(created_by,created_at) VALUES(?,?)`, principalID, now)
		if insertErr != nil {
			return CandidateSummary{}, insertErr
		}
		knowledgeID, err = knowledgeInsert.LastInsertId()
		if err != nil {
			return CandidateSummary{}, err
		}
	} else if err = conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(version_seq),0)+1 FROM knowledge_versions WHERE knowledge_id=?`, knowledgeID).Scan(&versionSeq); err != nil {
		return CandidateSummary{}, err
	}
	// 2. The candidate flips to Confirmed bound to this knowledge (the
	// version-insert trigger requires exactly this closure).
	result, err := conn.ExecContext(ctx, `
		UPDATE knowledge_candidates
		SET state='Confirmed', confirmed_knowledge_id=?, row_version=row_version+1
		WHERE id=? AND state='AwaitingConfirmation' AND draft_revision=?`,
		knowledgeID, candidateID, expectedRevision)
	if err != nil {
		return CandidateSummary{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return CandidateSummary{}, err
	}
	if affected == 0 {
		return CandidateSummary{}, rejectCandidateCommand(ctx, conn, principalID, commandID, ledgerConfirm, digest, candidateID, &RevisionConflict{Current: draftRevision + 1})
	}
	// 3. The next immutable version (append-only).
	versionInsert, err := conn.ExecContext(ctx, `
		INSERT INTO knowledge_versions(knowledge_id,version_seq,title,body,scope_json,source_candidate_id,created_by,created_at)
		VALUES(?,?,?,?,?,?,?,?)`, knowledgeID, versionSeq, title, body, scopeValue, candidateID, principalID, now)
	if err != nil {
		return CandidateSummary{}, err
	}
	versionID, err := versionInsert.LastInsertId()
	if err != nil {
		return CandidateSummary{}, err
	}
	// 4. Retire the former current document before advancing the pointer so
	// search remains exactly current ∧ eligible (new knowledge has NULL).
	if _, err := conn.ExecContext(ctx, `DELETE FROM knowledge_search_docs WHERE knowledge_version_id=(SELECT current_version_id FROM reusable_knowledge WHERE id=?)`, knowledgeID); err != nil {
		return CandidateSummary{}, err
	}
	// The current pointer advances in the same transaction (DATA-TX-008).
	if _, err := conn.ExecContext(ctx, `UPDATE reusable_knowledge SET current_version_id=?, row_version=row_version+1 WHERE id=?`, versionID, knowledgeID); err != nil {
		return CandidateSummary{}, err
	}
	// 5. Retrieval state + the eligible FTS projection row (the schema
	// trigger syncs knowledge_fts).
	if _, err := conn.ExecContext(ctx, `INSERT INTO knowledge_version_retrieval_state(knowledge_version_id,updated_at) VALUES(?,?)`, versionID, now); err != nil {
		return CandidateSummary{}, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO knowledge_search_docs(knowledge_version_id,title,body) VALUES(?,?,?)`, versionID, title, body); err != nil {
		return CandidateSummary{}, err
	}
	if err := completeImportBatchIfNoPending(ctx, conn, importBatchID); err != nil {
		return CandidateSummary{}, err
	}
	// Read back through the same connection: the pool may be fully
	// occupied while this handle is open.
	summary, err := scanCandidateOn(ctx, conn, candidateID)
	if err != nil {
		return CandidateSummary{}, err
	}
	if err := recordAudit(ctx, conn, principalID, commandID, ledgerConfirm, "knowledge_candidate", candidateID, &summary.RowVersion, now); err != nil {
		return CandidateSummary{}, err
	}
	if summary.ConfirmedKnowledgeID != fmt.Sprintf("%d", knowledgeID) {
		return CandidateSummary{}, errors.New("confirmed knowledge binding missing after commit")
	}
	if err := commitCandidateCommand(ctx, conn, principalID, commandID, ledgerConfirm, digest, candidateID, summary); err != nil {
		return CandidateSummary{}, err
	}
	committed = true
	return summary, nil
}

// Exclude removes an AwaitingConfirmation candidate from confirmation
// (user decision; VersionedCommandRequest fence).
func (service *Service) Exclude(ctx context.Context, principalID int64, commandID string, candidateID, expectedRowVersion int64) (CandidateSummary, error) {
	return service.ExcludeAs(ctx, MutationActor{ID: principalID}, commandID, candidateID, expectedRowVersion)
}

func (service *Service) ExcludeAs(ctx context.Context, actor MutationActor, commandID string, candidateID, expectedRowVersion int64) (CandidateSummary, error) {
	principalID := actor.ID
	digest := commandDigest(ledgerExclude, map[string]any{
		"candidateId":        candidateID,
		"expectedRowVersion": expectedRowVersion,
	})
	if record, ok, err := auth.LookupCommand(ctx, service.db, principalID, commandID); err != nil {
		return CandidateSummary{}, err
	} else if ok {
		return replaySummary(record, digest)
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return CandidateSummary{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return CandidateSummary{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if err := verifyMutationActorOn(ctx, conn, actor); err != nil {
		return CandidateSummary{}, err
	}
	if record, ok, err := authLookup(ctx, conn, principalID, commandID); err != nil {
		return CandidateSummary{}, err
	} else if ok {
		summary, replayErr := replaySummary(record, digest)
		if replayErr == nil {
			if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
				return CandidateSummary{}, err
			}
			committed = true
		}
		return summary, replayErr
	}
	var importBatchID sql.NullInt64
	var batchState sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT c.import_batch_id,b.state FROM knowledge_candidates c LEFT JOIN knowledge_import_batches b ON b.id=c.import_batch_id WHERE c.id=?`, candidateID).Scan(&importBatchID, &batchState); errors.Is(err, sql.ErrNoRows) {
		return CandidateSummary{}, rejectCandidateCommand(ctx, conn, principalID, commandID, ledgerExclude, digest, candidateID, ErrNotFound)
	} else if err != nil {
		return CandidateSummary{}, err
	}
	if batchState.Valid && batchState.String != "AwaitingConfirmation" {
		return CandidateSummary{}, rejectCandidateCommand(ctx, conn, principalID, commandID, ledgerExclude, digest, candidateID, &StateConflict{State: batchState.String})
	}
	result, err := conn.ExecContext(ctx, `
		UPDATE knowledge_candidates
		SET state='Excluded', row_version=row_version+1
		WHERE id=? AND state='AwaitingConfirmation' AND row_version=?`, candidateID, expectedRowVersion)
	if err != nil {
		return CandidateSummary{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return CandidateSummary{}, err
	}
	if affected == 0 {
		return CandidateSummary{}, rejectCandidateCommand(ctx, conn, principalID, commandID, ledgerExclude, digest, candidateID, service.excludeConflict(ctx, conn, candidateID))
	}
	now := service.nowText()
	summary, err := scanCandidateOn(ctx, conn, candidateID)
	if err != nil {
		return CandidateSummary{}, err
	}
	if err := recordAudit(ctx, conn, principalID, commandID, ledgerExclude, "knowledge_candidate", candidateID, &summary.RowVersion, now); err != nil {
		return CandidateSummary{}, err
	}
	if err := completeImportBatchIfNoPending(ctx, conn, importBatchID); err != nil {
		return CandidateSummary{}, err
	}
	if err := commitCandidateCommand(ctx, conn, principalID, commandID, ledgerExclude, digest, candidateID, summary); err != nil {
		return CandidateSummary{}, err
	}
	committed = true
	return summary, nil
}

// excludeConflict classifies a zero-row exclude.
func (service *Service) excludeConflict(ctx context.Context, conn *sql.Conn, candidateID int64) error {
	var state string
	var rowVersion int64
	err := conn.QueryRowContext(ctx, `SELECT state, row_version FROM knowledge_candidates WHERE id=?`, candidateID).Scan(&state, &rowVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state != StateAwaiting {
		return &StateConflict{State: state}
	}
	return &RowVersionConflict{Current: rowVersion}
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
// immediate transaction as the confirm/exclude command.
func completeImportBatchIfNoPending(ctx context.Context, conn *sql.Conn, batchID sql.NullInt64) error {
	if !batchID.Valid {
		return nil
	}
	_, err := conn.ExecContext(ctx, `UPDATE knowledge_import_batches
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
