package knowledge

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

type BatchConfirmation struct{ CandidateID, ExpectedRevision int64 }

var ErrPartialBatchConfirm = errors.New("batch confirm must name every candidate still awaiting confirmation")

// ConfirmBatch validates every requested revision before publishing any. A
// confirm names the whole current awaiting set (确认当前全部): omitting a
// candidate is a deterministic rejection, not a way to publish a subset.
func (service *Service) ConfirmBatch(ctx context.Context, principalID int64, commandID string, batchID int64, items []BatchConfirmation) (ImportBatchDetail, error) {
	if len(items) == 0 {
		return ImportBatchDetail{}, ErrEmptyEdit
	}
	seen := map[int64]bool{}
	encoded := make([]map[string]int64, 0, len(items))
	for _, item := range items {
		if item.CandidateID < 1 || seen[item.CandidateID] {
			return ImportBatchDetail{}, ErrEmptyEdit
		}
		seen[item.CandidateID] = true
		encoded = append(encoded, map[string]int64{"candidateId": item.CandidateID, "expectedRevision": item.ExpectedRevision})
	}
	digest := commandDigest(opBatchConfirm, map[string]any{"batchId": batchID, "items": encoded})
	outcome, err := execution.Run(ctx, service.runner, service.confirmBatch, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: commandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (ImportBatchDetail, execution.Change, error) {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM knowledge_import_batches WHERE id=?`, batchID).Scan(&state); errors.Is(err, sql.ErrNoRows) {
			return ImportBatchDetail{}, execution.Changed, rejectionOf(batchID, ErrNotFound)
		} else if err != nil {
			return ImportBatchDetail{}, execution.Changed, err
		}
		if state != "AwaitingConfirmation" {
			return ImportBatchDetail{}, execution.Changed, rejectionOf(batchID, &StateConflict{State: state})
		}
		// The frozen UI contract confirms the current generation as one set: the
		// request must cover every candidate of this batch still awaiting
		// confirmation. Items are unique and each is verified below to belong to
		// this batch in the awaiting state, so equal counts prove exact coverage.
		var awaiting int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM knowledge_candidates WHERE import_batch_id=? AND state='AwaitingConfirmation'`, batchID).Scan(&awaiting); err != nil {
			return ImportBatchDetail{}, execution.Changed, err
		}
		if awaiting != len(items) {
			return ImportBatchDetail{}, execution.Changed, rejectionOf(batchID, ErrPartialBatchConfirm)
		}
		// All preconditions are verified first; no version has been inserted yet.
		for _, item := range items {
			var candidateBatch, revision int64
			var candidateState string
			err := tx.QueryRowContext(ctx, `SELECT import_batch_id,state,draft_revision FROM knowledge_candidates WHERE id=?`, item.CandidateID).Scan(&candidateBatch, &candidateState, &revision)
			if errors.Is(err, sql.ErrNoRows) {
				return ImportBatchDetail{}, execution.Changed, rejectionOf(batchID, ErrNotFound)
			}
			if err != nil {
				return ImportBatchDetail{}, execution.Changed, err
			}
			if candidateBatch != batchID || candidateState != StateAwaiting {
				return ImportBatchDetail{}, execution.Changed, rejectionOf(batchID, &StateConflict{State: candidateState})
			}
			if revision != item.ExpectedRevision {
				return ImportBatchDetail{}, execution.Changed, rejectionOf(batchID, &RevisionConflict{Current: revision})
			}
		}
		for _, item := range items {
			if err := service.confirmImportedCandidateOn(ctx, tx, tx, principalID, item.CandidateID, item.ExpectedRevision); err != nil {
				return ImportBatchDetail{}, execution.Changed, err
			}
		}
		// Exact coverage above means every awaiting candidate was confirmed, so
		// no candidate of the current generation remains awaiting: the batch
		// completes in this same transaction.
		if _, err := tx.ExecContext(ctx, `UPDATE knowledge_import_batches SET state='Completed',row_version=row_version+1 WHERE id=? AND state='AwaitingConfirmation'`, batchID); err != nil {
			return ImportBatchDetail{}, execution.Changed, err
		}
		detail, err := scanBatchDetailOn(ctx, tx, batchID)
		if err != nil {
			return ImportBatchDetail{}, execution.Changed, err
		}
		return detail, execution.Changed, nil
	}, func(detail ImportBatchDetail) int64 { return batchID })
	if err != nil {
		return ImportBatchDetail{}, service.translateCommandError(ctx, err)
	}
	return outcome.Result, nil
}

func (service *Service) confirmImportedCandidateOn(ctx context.Context, w writer, q queryer, principalID, candidateID, expectedRevision int64) error {
	var draftTitle, draftBody, scope sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT draft_title,draft_body,draft_scope_json FROM knowledge_candidates WHERE id=?`, candidateID).Scan(&draftTitle, &draftBody, &scope); err != nil {
		return err
	}
	title, body := draftValues(draftTitle, draftBody)
	now := service.nowText()
	insert, err := w.ExecContext(ctx, `INSERT INTO reusable_knowledge(created_by,created_at) VALUES(?,?)`, principalID, now)
	if err != nil {
		return err
	}
	knowledgeID, err := insert.LastInsertId()
	if err != nil {
		return err
	}
	result, err := w.ExecContext(ctx, `UPDATE knowledge_candidates SET state='Confirmed',confirmed_knowledge_id=?,row_version=row_version+1 WHERE id=? AND state='AwaitingConfirmation' AND draft_revision=?`, knowledgeID, candidateID, expectedRevision)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return &RevisionConflict{Current: expectedRevision}
	}
	version, err := w.ExecContext(ctx, `INSERT INTO knowledge_versions(knowledge_id,version_seq,title,body,scope_json,source_candidate_id,created_by,created_at) VALUES(?,1,?,?,?,?,?,?)`, knowledgeID, title, body, scope, candidateID, principalID, now)
	if err != nil {
		return err
	}
	versionID, err := version.LastInsertId()
	if err != nil {
		return err
	}
	if _, err = w.ExecContext(ctx, `UPDATE reusable_knowledge SET current_version_id=?,row_version=row_version+1 WHERE id=?`, versionID, knowledgeID); err != nil {
		return err
	}
	if _, err = w.ExecContext(ctx, `INSERT INTO knowledge_version_retrieval_state(knowledge_version_id,updated_at) VALUES(?,?)`, versionID, now); err != nil {
		return err
	}
	_, err = w.ExecContext(ctx, `INSERT INTO knowledge_search_docs(knowledge_version_id,title,body) VALUES(?,?,?)`, versionID, title, body)
	return err
}

// CancelBatch atomically fences the batch and its extraction Attempt. The
// caller sends CancelAttempt after this durable decision commits.
func (service *Service) CancelBatch(ctx context.Context, principalID int64, commandID string, batchID, expectedRowVersion int64) (ImportBatchDetail, int64, error) {
	digest := commandDigest(opBatchCancel, map[string]any{"batchId": batchID, "expectedRowVersion": expectedRowVersion})
	cancelled, err := execution.Run(ctx, service.runner, service.cancelBatch, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: commandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (batchCancelOutcome, execution.Change, error) {
		var attemptID, currentRowVersion int64
		var state string
		err := tx.QueryRowContext(ctx, `SELECT b.state,b.row_version,a.id FROM knowledge_import_batches b JOIN execution_attempts a ON a.scope_id=b.id AND a.attempt_type='knowledge_extraction' WHERE b.id=?`, batchID).Scan(&state, &currentRowVersion, &attemptID)
		if errors.Is(err, sql.ErrNoRows) {
			return batchCancelOutcome{}, execution.Changed, rejectionOf(batchID, ErrNotFound)
		}
		if err != nil {
			return batchCancelOutcome{}, execution.Changed, err
		}
		if state != "Processing" && state != "AwaitingConfirmation" {
			// A terminal command that lost to a successful confirmation reports the
			// winner's authority as an idempotent 200, never a synthetic conflict.
			detail, detailErr := scanBatchDetailOn(ctx, tx, batchID)
			if detailErr != nil {
				return batchCancelOutcome{}, execution.Changed, detailErr
			}
			return batchCancelOutcome{Detail: detail, Unchanged: true}, execution.Unchanged, nil
		}
		if expectedRowVersion != currentRowVersion {
			return batchCancelOutcome{}, execution.Changed, rejectionOf(batchID, &RowVersionConflict{Current: currentRowVersion})
		}
		result, err := tx.ExecContext(ctx, `UPDATE knowledge_import_batches SET state='Cancelled',row_version=row_version+1 WHERE id=? AND row_version=? AND state=?`, batchID, expectedRowVersion, state)
		if err != nil {
			return batchCancelOutcome{}, execution.Changed, err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return batchCancelOutcome{}, execution.Changed, errors.New("cancel batch update lost its transaction fence")
		}
		attemptState, err := service.attempts.CancelFenceOn(ctx, tx, attemptID)
		if err != nil {
			return batchCancelOutcome{}, execution.Changed, err
		}
		detail, err := scanBatchDetailOn(ctx, tx, batchID)
		if err != nil {
			return batchCancelOutcome{}, execution.Changed, err
		}
		outcome := batchCancelOutcome{Detail: detail}
		if attemptState == "Cancelling" {
			outcome.CancelledAttemptID = attemptID
		}
		return outcome, execution.Changed, nil
	}, func(outcome batchCancelOutcome) int64 { return batchID })
	if err != nil {
		return ImportBatchDetail{}, 0, service.translateCommandError(ctx, err)
	}
	return cancelled.Result.Detail, cancelled.Result.CancelledAttemptID, nil
}

// batchCancelOutcome carries the cancelled batch projection plus the attempt
// the caller must still cancel downstream (0 when the attempt already ended).
type batchCancelOutcome struct {
	Detail             ImportBatchDetail `json:"detail"`
	CancelledAttemptID int64             `json:"cancelledAttemptId,omitempty"`
	Unchanged          bool              `json:"unchanged,omitempty"`
}
