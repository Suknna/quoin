package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/auth"
)

type BatchConfirmation struct{ CandidateID, ExpectedRevision int64 }

var ErrPartialBatchConfirm = errors.New("batch confirm must name every candidate still awaiting confirmation")

// ConfirmBatch validates every requested revision before publishing any. A
// confirm names the whole current awaiting set (确认当前全部): omitting a
// candidate is a deterministic rejection, not a way to publish a subset.
func (service *Service) ConfirmBatch(ctx context.Context, principalID int64, commandID string, batchID int64, items []BatchConfirmation) (ImportBatchDetail, error) {
	return service.ConfirmBatchAs(ctx, MutationActor{ID: principalID}, commandID, batchID, items)
}

func (service *Service) ConfirmBatchAs(ctx context.Context, actor MutationActor, commandID string, batchID int64, items []BatchConfirmation) (ImportBatchDetail, error) {
	principalID := actor.ID
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
	digest := commandDigest(ledgerBatchConfirm, map[string]any{"batchId": batchID, "items": encoded})
	if record, ok, err := auth.LookupCommand(ctx, service.db, principalID, commandID); err != nil {
		return ImportBatchDetail{}, err
	} else if ok {
		return replayBatch(record, digest)
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return ImportBatchDetail{}, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return ImportBatchDetail{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if err := verifyMutationActorOn(ctx, conn, actor); err != nil {
		return ImportBatchDetail{}, err
	}
	if record, ok, err := authLookup(ctx, conn, principalID, commandID); err != nil {
		return ImportBatchDetail{}, err
	} else if ok {
		result, replayErr := replayBatch(record, digest)
		if replayErr == nil {
			_, replayErr = conn.ExecContext(ctx, "COMMIT")
			committed = replayErr == nil
		}
		return result, replayErr
	}
	var state string
	if err = conn.QueryRowContext(ctx, `SELECT state FROM knowledge_import_batches WHERE id=?`, batchID).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		return ImportBatchDetail{}, rejectScopedCommand(ctx, conn, principalID, commandID, ledgerBatchConfirm, digest, "knowledge_import_batch", batchID, service.nowText(), ErrNotFound)
	} else if err != nil {
		return ImportBatchDetail{}, err
	}
	if state != "AwaitingConfirmation" {
		return ImportBatchDetail{}, rejectScopedCommand(ctx, conn, principalID, commandID, ledgerBatchConfirm, digest, "knowledge_import_batch", batchID, service.nowText(), &StateConflict{State: state})
	}
	// The frozen UI contract confirms the current generation as one set: the
	// request must cover every candidate of this batch still awaiting
	// confirmation. Items are unique and each is verified below to belong to
	// this batch in the awaiting state, so equal counts prove exact coverage.
	var awaiting int
	if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM knowledge_candidates WHERE import_batch_id=? AND state='AwaitingConfirmation'`, batchID).Scan(&awaiting); err != nil {
		return ImportBatchDetail{}, err
	}
	if awaiting != len(items) {
		return ImportBatchDetail{}, rejectScopedCommand(ctx, conn, principalID, commandID, ledgerBatchConfirm, digest, "knowledge_import_batch", batchID, service.nowText(), ErrPartialBatchConfirm)
	}
	// All preconditions are verified first; no version has been inserted yet.
	for _, item := range items {
		var candidateBatch, revision int64
		var candidateState string
		err = conn.QueryRowContext(ctx, `SELECT import_batch_id,state,draft_revision FROM knowledge_candidates WHERE id=?`, item.CandidateID).Scan(&candidateBatch, &candidateState, &revision)
		if errors.Is(err, sql.ErrNoRows) {
			return ImportBatchDetail{}, rejectScopedCommand(ctx, conn, principalID, commandID, ledgerBatchConfirm, digest, "knowledge_import_batch", batchID, service.nowText(), ErrNotFound)
		}
		if err != nil {
			return ImportBatchDetail{}, err
		}
		if candidateBatch != batchID || candidateState != StateAwaiting {
			return ImportBatchDetail{}, rejectScopedCommand(ctx, conn, principalID, commandID, ledgerBatchConfirm, digest, "knowledge_import_batch", batchID, service.nowText(), &StateConflict{State: candidateState})
		}
		if revision != item.ExpectedRevision {
			return ImportBatchDetail{}, rejectScopedCommand(ctx, conn, principalID, commandID, ledgerBatchConfirm, digest, "knowledge_import_batch", batchID, service.nowText(), &RevisionConflict{Current: revision})
		}
	}
	for _, item := range items {
		if err = service.confirmImportedCandidateOn(ctx, conn, principalID, item.CandidateID, item.ExpectedRevision); err != nil {
			return ImportBatchDetail{}, err
		}
	}
	// Exact coverage above means every awaiting candidate was confirmed, so
	// no candidate of the current generation remains awaiting: the batch
	// completes in this same transaction.
	if _, err = conn.ExecContext(ctx, `UPDATE knowledge_import_batches SET state='Completed',row_version=row_version+1 WHERE id=? AND state='AwaitingConfirmation'`, batchID); err != nil {
		return ImportBatchDetail{}, err
	}
	detail, err := scanBatchDetailOn(ctx, conn, batchID)
	if err != nil {
		return ImportBatchDetail{}, err
	}
	payload, err := json.Marshal(detail)
	if err != nil {
		return ImportBatchDetail{}, err
	}
	if err = recordAudit(ctx, conn, principalID, commandID, ledgerBatchConfirm, "knowledge_import_batch", batchID, &detail.RowVersion, service.nowText()); err != nil {
		return ImportBatchDetail{}, err
	}
	if err = recordCommand(ctx, conn, principalID, commandID, ledgerBatchConfirm, digest, authOutcomeCommitted, "knowledge_import_batch", batchID, string(payload)); err != nil {
		return ImportBatchDetail{}, err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	committed = err == nil
	return detail, err
}

func (service *Service) confirmImportedCandidateOn(ctx context.Context, conn *sql.Conn, principalID, candidateID, expectedRevision int64) error {
	var draftTitle, draftBody, scope sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT draft_title,draft_body,draft_scope_json FROM knowledge_candidates WHERE id=?`, candidateID).Scan(&draftTitle, &draftBody, &scope); err != nil {
		return err
	}
	title, body := draftValues(draftTitle, draftBody)
	now := service.nowText()
	insert, err := conn.ExecContext(ctx, `INSERT INTO reusable_knowledge(created_by,created_at) VALUES(?,?)`, principalID, now)
	if err != nil {
		return err
	}
	knowledgeID, err := insert.LastInsertId()
	if err != nil {
		return err
	}
	result, err := conn.ExecContext(ctx, `UPDATE knowledge_candidates SET state='Confirmed',confirmed_knowledge_id=?,row_version=row_version+1 WHERE id=? AND state='AwaitingConfirmation' AND draft_revision=?`, knowledgeID, candidateID, expectedRevision)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return &RevisionConflict{Current: expectedRevision}
	}
	version, err := conn.ExecContext(ctx, `INSERT INTO knowledge_versions(knowledge_id,version_seq,title,body,scope_json,source_candidate_id,created_by,created_at) VALUES(?,1,?,?,?,?,?,?)`, knowledgeID, title, body, scope, candidateID, principalID, now)
	if err != nil {
		return err
	}
	versionID, err := version.LastInsertId()
	if err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, `UPDATE reusable_knowledge SET current_version_id=?,row_version=row_version+1 WHERE id=?`, versionID, knowledgeID); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO knowledge_version_retrieval_state(knowledge_version_id,updated_at) VALUES(?,?)`, versionID, now); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO knowledge_search_docs(knowledge_version_id,title,body) VALUES(?,?,?)`, versionID, title, body)
	return err
}

// CancelBatch atomically fences the batch and its extraction Attempt. The
// caller sends CancelAttempt after this durable decision commits.
func (service *Service) CancelBatch(ctx context.Context, principalID int64, commandID string, batchID, expectedRowVersion int64) (ImportBatchDetail, int64, error) {
	return service.CancelBatchAs(ctx, MutationActor{ID: principalID}, commandID, batchID, expectedRowVersion)
}

func (service *Service) CancelBatchAs(ctx context.Context, actor MutationActor, commandID string, batchID, expectedRowVersion int64) (ImportBatchDetail, int64, error) {
	principalID := actor.ID
	digest := commandDigest(ledgerBatchCancel, map[string]any{"batchId": batchID, "expectedRowVersion": expectedRowVersion})
	if record, ok, err := auth.LookupCommand(ctx, service.db, principalID, commandID); err != nil {
		return ImportBatchDetail{}, 0, err
	} else if ok {
		detail, replayErr := replayBatch(record, digest)
		return detail, 0, replayErr
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return ImportBatchDetail{}, 0, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return ImportBatchDetail{}, 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if err := verifyMutationActorOn(ctx, conn, actor); err != nil {
		return ImportBatchDetail{}, 0, err
	}
	if record, ok, err := authLookup(ctx, conn, principalID, commandID); err != nil {
		return ImportBatchDetail{}, 0, err
	} else if ok {
		detail, replayErr := replayBatch(record, digest)
		if replayErr == nil {
			_, replayErr = conn.ExecContext(ctx, "COMMIT")
			committed = replayErr == nil
		}
		return detail, 0, replayErr
	}
	var attemptID, currentRowVersion int64
	var state string
	err = conn.QueryRowContext(ctx, `SELECT b.state,b.row_version,a.id FROM knowledge_import_batches b JOIN execution_attempts a ON a.scope_id=b.id AND a.attempt_type='knowledge_extraction' WHERE b.id=?`, batchID).Scan(&state, &currentRowVersion, &attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportBatchDetail{}, 0, rejectScopedCommand(ctx, conn, principalID, commandID, ledgerBatchCancel, digest, "knowledge_import_batch", batchID, service.nowText(), ErrNotFound)
	}
	if err != nil {
		return ImportBatchDetail{}, 0, err
	}
	if state != "Processing" && state != "AwaitingConfirmation" {
		// A terminal command that lost to a successful confirmation reports the
		// winner's authority as an idempotent 200, never a synthetic conflict.
		detail, detailErr := scanBatchDetailOn(ctx, conn, batchID)
		if detailErr != nil {
			return ImportBatchDetail{}, 0, detailErr
		}
		body, marshalErr := json.Marshal(detail)
		if marshalErr != nil {
			return ImportBatchDetail{}, 0, marshalErr
		}
		detailRowVersion := detail.RowVersion
		if auditErr := recordAudit(ctx, conn, principalID, commandID, ledgerBatchCancel, "knowledge_import_batch", batchID, &detailRowVersion, service.nowText()); auditErr != nil {
			return ImportBatchDetail{}, 0, auditErr
		}
		if commandErr := recordCommand(ctx, conn, principalID, commandID, ledgerBatchCancel, digest, authOutcomeCommitted, "knowledge_import_batch", batchID, string(body)); commandErr != nil {
			return ImportBatchDetail{}, 0, commandErr
		}
		if _, commitErr := conn.ExecContext(ctx, "COMMIT"); commitErr != nil {
			return ImportBatchDetail{}, 0, commitErr
		}
		committed = true
		return detail, 0, nil
	}
	if expectedRowVersion != currentRowVersion {
		return ImportBatchDetail{}, 0, rejectScopedCommand(ctx, conn, principalID, commandID, ledgerBatchCancel, digest, "knowledge_import_batch", batchID, service.nowText(), &RowVersionConflict{Current: currentRowVersion})
	}
	result, err := conn.ExecContext(ctx, `UPDATE knowledge_import_batches SET state='Cancelled',row_version=row_version+1 WHERE id=? AND row_version=? AND state=?`, batchID, expectedRowVersion, state)
	if err != nil {
		return ImportBatchDetail{}, 0, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ImportBatchDetail{}, 0, fmt.Errorf("cancel batch update lost its transaction fence")
	}
	attemptState, err := service.Attempts().CancelFenceOn(ctx, conn, attemptID)
	if err != nil {
		return ImportBatchDetail{}, 0, err
	}
	detail, err := scanBatchDetailOn(ctx, conn, batchID)
	if err != nil {
		return ImportBatchDetail{}, 0, err
	}
	body, _ := json.Marshal(detail)
	cancelRowVersion := detail.RowVersion
	if err = recordAudit(ctx, conn, principalID, commandID, ledgerBatchCancel, "knowledge_import_batch", batchID, &cancelRowVersion, service.nowText()); err != nil {
		return ImportBatchDetail{}, 0, err
	}
	if err = recordCommand(ctx, conn, principalID, commandID, ledgerBatchCancel, digest, authOutcomeCommitted, "knowledge_import_batch", batchID, string(body)); err != nil {
		return ImportBatchDetail{}, 0, err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	committed = err == nil
	if err != nil {
		return ImportBatchDetail{}, 0, err
	}
	if attemptState == "Cancelling" {
		return detail, attemptID, nil
	}
	return detail, 0, nil
}

func replayBatch(record authRecord, digest string) (ImportBatchDetail, error) {
	if record.RequestDigest != digest || record.ResultObjectType != "knowledge_import_batch" {
		return ImportBatchDetail{}, ErrCommandReused
	}
	if record.Outcome == authOutcomeRejected {
		return ImportBatchDetail{}, replayRejection(record.ResultPayload)
	}
	if record.Outcome != authOutcomeCommitted {
		return ImportBatchDetail{}, ErrCommandReused
	}
	var detail ImportBatchDetail
	if err := json.Unmarshal([]byte(record.ResultPayload), &detail); err != nil {
		return ImportBatchDetail{}, err
	}
	return detail, nil
}
