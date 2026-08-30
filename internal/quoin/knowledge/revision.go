package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/auth"
)

// CreateRevisionCandidate snapshots the current immutable version into an
// editable candidate. Its expected pointer fence prevents a stale UI from
// proposing a revision against a superseded current version.
func (service *Service) CreateRevisionCandidate(ctx context.Context, principalID int64, commandID string, knowledgeID, expectedVersionID, expectedRowVersion int64) (CandidateSummary, bool, error) {
	return service.CreateRevisionCandidateAs(ctx, MutationActor{ID: principalID}, commandID, knowledgeID, expectedVersionID, expectedRowVersion)
}

func (service *Service) CreateRevisionCandidateAs(ctx context.Context, actor MutationActor, commandID string, knowledgeID, expectedVersionID, expectedRowVersion int64) (CandidateSummary, bool, error) {
	principalID := actor.ID
	digest := commandDigest(ledgerCreateRevision, map[string]any{"knowledgeId": knowledgeID, "expectedCurrentVersionId": expectedVersionID, "expectedRowVersion": expectedRowVersion})
	if record, ok, err := auth.LookupCommand(ctx, service.db, principalID, commandID); err != nil {
		return CandidateSummary{}, false, err
	} else if ok {
		return replayRevisionCreate(record, digest)
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return CandidateSummary{}, false, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return CandidateSummary{}, false, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if err := verifyMutationActorOn(ctx, conn, actor); err != nil {
		return CandidateSummary{}, false, err
	}
	if record, ok, err := authLookup(ctx, conn, principalID, commandID); err != nil {
		return CandidateSummary{}, false, err
	} else if ok {
		summary, created, replayErr := replayRevisionCreate(record, digest)
		if replayErr == nil {
			_, replayErr = conn.ExecContext(ctx, "COMMIT")
			committed = replayErr == nil
		}
		return summary, created, replayErr
	}
	var currentID, rowVersion int64
	var title, body string
	var scope sql.NullString
	err = conn.QueryRowContext(ctx, `SELECT k.current_version_id,k.row_version,v.title,v.body,v.scope_json FROM reusable_knowledge k JOIN knowledge_versions v ON v.id=k.current_version_id WHERE k.id=?`, knowledgeID).Scan(&currentID, &rowVersion, &title, &body, &scope)
	if errors.Is(err, sql.ErrNoRows) {
		return CandidateSummary{}, false, rejectCandidateCommand(ctx, conn, principalID, commandID, ledgerCreateRevision, digest, 0, ErrNotFound)
	}
	if err != nil {
		return CandidateSummary{}, false, err
	}
	if currentID != expectedVersionID || rowVersion != expectedRowVersion {
		return CandidateSummary{}, false, rejectCandidateCommand(ctx, conn, principalID, commandID, ledgerCreateRevision, digest, 0, &RowVersionConflict{Current: rowVersion})
	}
	// This partial unique index is the authority for single live revision.
	var existingID int64
	err = conn.QueryRowContext(ctx, `SELECT id FROM knowledge_candidates WHERE source_type='knowledge_version' AND source_id=? AND target_knowledge_id=? AND state IN ('AwaitingConfirmation','Confirmed') ORDER BY id DESC LIMIT 1`, currentID, knowledgeID).Scan(&existingID)
	if err == nil {
		summary, scanErr := scanCandidateOn(ctx, conn, existingID)
		if scanErr != nil {
			return CandidateSummary{}, false, scanErr
		}
		if err := commitRevisionCreate(ctx, conn, principalID, commandID, digest, existingID, summary, false); err != nil {
			return CandidateSummary{}, false, err
		}
		committed = true
		return summary, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return CandidateSummary{}, false, err
	}
	original, _ := json.Marshal(map[string]any{"v": 1, "source": map[string]any{"type": SourceKnowledgeVersion, "id": fmt.Sprintf("%d", currentID)}, "title": title, "body": body})
	now := service.nowText()
	result, err := conn.ExecContext(ctx, `INSERT INTO knowledge_candidates(source_type,source_id,target_knowledge_id,state,original_suggestion_json,draft_title,draft_body,draft_scope_json,created_by,created_at)
		VALUES('knowledge_version',?,?, 'AwaitingConfirmation',?,?,?,?,?,?)`, currentID, knowledgeID, string(original), title, body, scope, principalID, now)
	if err != nil {
		return CandidateSummary{}, false, err
	}
	candidateID, err := result.LastInsertId()
	if err != nil {
		return CandidateSummary{}, false, err
	}
	if err = recordAudit(ctx, conn, principalID, commandID, ledgerCreateRevision, "knowledge_candidate", candidateID, nil, now); err != nil {
		return CandidateSummary{}, false, err
	}
	summary, err := scanCandidateOn(ctx, conn, candidateID)
	if err != nil {
		return CandidateSummary{}, false, err
	}
	if err = commitRevisionCreate(ctx, conn, principalID, commandID, digest, candidateID, summary, true); err != nil {
		return CandidateSummary{}, false, err
	}
	committed = true
	return summary, true, nil
}

type revisionCreatePayload struct {
	Summary CandidateSummary `json:"summary"`
	Created bool             `json:"created"`
}

func commitRevisionCreate(ctx context.Context, conn *sql.Conn, principalID int64, commandID, digest string, candidateID int64, summary CandidateSummary, created bool) error {
	body, err := json.Marshal(revisionCreatePayload{Summary: summary, Created: created})
	if err != nil {
		return err
	}
	if err := recordCommand(ctx, conn, principalID, commandID, ledgerCreateRevision, digest, authOutcomeCommitted, "knowledge_candidate", candidateID, string(body)); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

func replayRevisionCreate(record authRecord, digest string) (CandidateSummary, bool, error) {
	if record.RequestDigest != digest || record.ResultObjectType != "knowledge_candidate" {
		return CandidateSummary{}, false, ErrCommandReused
	}
	if record.Outcome == authOutcomeRejected {
		return CandidateSummary{}, false, replayRejection(record.ResultPayload)
	}
	if record.Outcome != authOutcomeCommitted {
		return CandidateSummary{}, false, ErrCommandReused
	}
	var payload revisionCreatePayload
	if err := json.Unmarshal([]byte(record.ResultPayload), &payload); err != nil {
		return CandidateSummary{}, false, err
	}
	return payload.Summary, payload.Created, nil
}

func replayStopReuse(record authRecord, digest string) error {
	if record.RequestDigest != digest || record.ResultObjectType != "knowledge_version" {
		return ErrCommandReused
	}
	if record.Outcome == authOutcomeCommitted {
		return nil
	}
	return replayRejection(record.ResultPayload)
}

// StopReuse has a retrieval-state row version (not the immutable version row)
// as its concurrency authority. The delete removes the FTS document in this
// same transaction; the FTS trigger maintains external-content consistency.
func (service *Service) StopReuse(ctx context.Context, principalID int64, commandID string, knowledgeID, versionID, expectedRowVersion int64) error {
	return service.StopReuseAs(ctx, MutationActor{ID: principalID}, commandID, knowledgeID, versionID, expectedRowVersion)
}

func (service *Service) StopReuseAs(ctx context.Context, actor MutationActor, commandID string, knowledgeID, versionID, expectedRowVersion int64) error {
	principalID := actor.ID
	digest := commandDigest(ledgerStopReuse, map[string]any{"knowledgeId": knowledgeID, "versionId": versionID, "expectedRowVersion": expectedRowVersion})
	if record, ok, err := auth.LookupCommand(ctx, service.db, principalID, commandID); err != nil {
		return err
	} else if ok {
		return replayStopReuse(record, digest)
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if err := verifyMutationActorOn(ctx, conn, actor); err != nil {
		return err
	}
	if record, ok, err := authLookup(ctx, conn, principalID, commandID); err != nil {
		return err
	} else if ok {
		replayErr := replayStopReuse(record, digest)
		if replayErr == nil {
			_, replayErr = conn.ExecContext(ctx, "COMMIT")
			committed = replayErr == nil
		}
		return replayErr
	}
	var currentRowVersion int64
	var exited bool
	if err := conn.QueryRowContext(ctx, `SELECT r.row_version,r.exited FROM knowledge_versions v JOIN knowledge_version_retrieval_state r ON r.knowledge_version_id=v.id WHERE v.id=? AND v.knowledge_id=?`, versionID, knowledgeID).Scan(&currentRowVersion, &exited); errors.Is(err, sql.ErrNoRows) {
		return rejectScopedCommand(ctx, conn, principalID, commandID, ledgerStopReuse, digest, "knowledge_version", versionID, service.nowText(), ErrNotFound)
	} else if err != nil {
		return err
	}
	if expectedRowVersion != currentRowVersion {
		return rejectScopedCommand(ctx, conn, principalID, commandID, ledgerStopReuse, digest, "knowledge_version", versionID, service.nowText(), &RowVersionConflict{Current: currentRowVersion})
	}
	if exited {
		return rejectScopedCommand(ctx, conn, principalID, commandID, ledgerStopReuse, digest, "knowledge_version", versionID, service.nowText(), &StateConflict{State: "Exited"})
	}
	result, err := conn.ExecContext(ctx, `UPDATE knowledge_version_retrieval_state SET exited=1,exited_at=?,exit_reason='stopped',updated_at=?,row_version=row_version+1 WHERE knowledge_version_id=? AND row_version=?`, service.nowText(), service.nowText(), versionID, expectedRowVersion)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("stop reuse update lost its transaction fence")
	}
	if _, err = conn.ExecContext(ctx, `DELETE FROM knowledge_search_docs WHERE knowledge_version_id=?`, versionID); err != nil {
		return err
	}
	stoppedRowVersion := expectedRowVersion + 1
	if err = recordAudit(ctx, conn, principalID, commandID, ledgerStopReuse, "knowledge_version", versionID, &stoppedRowVersion, service.nowText()); err != nil {
		return err
	}
	if err = recordCommand(ctx, conn, principalID, commandID, ledgerStopReuse, digest, authOutcomeCommitted, "knowledge_version", versionID, "{}"); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	committed = err == nil
	return err
}
