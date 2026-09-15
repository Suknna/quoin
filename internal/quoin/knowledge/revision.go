package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// CreateRevisionCandidate snapshots the current immutable version into an
// editable candidate. Its expected pointer fence prevents a stale UI from
// proposing a revision against a superseded current version.
func (service *Service) CreateRevisionCandidate(ctx context.Context, principalID int64, commandID string, knowledgeID, expectedVersionID, expectedRowVersion int64) (CandidateSummary, bool, error) {
	digest := commandDigest(opCreateRevison, map[string]any{"knowledgeId": knowledgeID, "expectedCurrentVersionId": expectedVersionID, "expectedRowVersion": expectedRowVersion})
	outcome, err := execution.Run(ctx, service.runner, service.createRevision, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: commandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (revisionCreatePayload, execution.Change, error) {
		var currentID, rowVersion int64
		var title, body string
		var scope sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT k.current_version_id,k.row_version,v.title,v.body,v.scope_json FROM reusable_knowledge k JOIN knowledge_versions v ON v.id=k.current_version_id WHERE k.id=?`, knowledgeID).Scan(&currentID, &rowVersion, &title, &body, &scope)
		if errors.Is(err, sql.ErrNoRows) {
			return revisionCreatePayload{}, execution.Changed, rejectionOf(0, ErrNotFound)
		}
		if err != nil {
			return revisionCreatePayload{}, execution.Changed, err
		}
		if currentID != expectedVersionID || rowVersion != expectedRowVersion {
			return revisionCreatePayload{}, execution.Changed, rejectionOf(0, &RowVersionConflict{Current: rowVersion})
		}
		// This partial unique index is the authority for single live revision.
		var existingID int64
		err = tx.QueryRowContext(ctx, `SELECT id FROM knowledge_candidates WHERE source_type='knowledge_version' AND source_id=? AND target_knowledge_id=? AND state IN ('AwaitingConfirmation','Confirmed') ORDER BY id DESC LIMIT 1`, currentID, knowledgeID).Scan(&existingID)
		if err == nil {
			summary, scanErr := scanCandidateOn(ctx, tx, existingID)
			if scanErr != nil {
				return revisionCreatePayload{}, execution.Changed, scanErr
			}
			return revisionCreatePayload{Summary: summary, Created: false}, execution.Unchanged, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return revisionCreatePayload{}, execution.Changed, err
		}
		original, _ := json.Marshal(map[string]any{"v": 1, "source": map[string]any{"type": SourceKnowledgeVersion, "id": fmt.Sprintf("%d", currentID)}, "title": title, "body": body})
		now := service.nowText()
		result, err := tx.ExecContext(ctx, `INSERT INTO knowledge_candidates(source_type,source_id,target_knowledge_id,state,original_suggestion_json,draft_title,draft_body,draft_scope_json,created_by,created_at)
			VALUES('knowledge_version',?,?, 'AwaitingConfirmation',?,?,?,?,?,?)`, currentID, knowledgeID, string(original), title, body, scope, principalID, now)
		if err != nil {
			return revisionCreatePayload{}, execution.Changed, err
		}
		candidateID, err := result.LastInsertId()
		if err != nil {
			return revisionCreatePayload{}, execution.Changed, err
		}
		summary, err := scanCandidateOn(ctx, tx, candidateID)
		if err != nil {
			return revisionCreatePayload{}, execution.Changed, err
		}
		return revisionCreatePayload{Summary: summary, Created: true}, execution.Changed, nil
	}, func(payload revisionCreatePayload) int64 { return parseCandidateLocator(payload.Summary.ID) })
	if err != nil {
		return CandidateSummary{}, false, service.translateCommandError(ctx, err)
	}
	return outcome.Result.Summary, outcome.Result.Created, nil
}

// revisionCreatePayload is the frozen ledger payload shape of the revision
// create command (summary plus whether this command created the candidate).
type revisionCreatePayload struct {
	Summary CandidateSummary `json:"summary"`
	Created bool             `json:"created"`
}

// StopReuse has a retrieval-state row version (not the immutable version row)
// as its concurrency authority. The delete removes the FTS document in this
// same transaction; the FTS trigger maintains external-content consistency.
func (service *Service) StopReuse(ctx context.Context, principalID int64, commandID string, knowledgeID, versionID, expectedRowVersion int64) error {
	digest := commandDigest(opStopReuse, map[string]any{"knowledgeId": knowledgeID, "versionId": versionID, "expectedRowVersion": expectedRowVersion})
	_, err := execution.Run(ctx, service.runner, service.stopReuse, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: commandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (stopReuseResult, execution.Change, error) {
		var currentRowVersion int64
		var exited bool
		if err := tx.QueryRowContext(ctx, `SELECT r.row_version,r.exited FROM knowledge_versions v JOIN knowledge_version_retrieval_state r ON r.knowledge_version_id=v.id WHERE v.id=? AND v.knowledge_id=?`, versionID, knowledgeID).Scan(&currentRowVersion, &exited); errors.Is(err, sql.ErrNoRows) {
			return stopReuseResult{}, execution.Changed, rejectionOf(versionID, ErrNotFound)
		} else if err != nil {
			return stopReuseResult{}, execution.Changed, err
		}
		if expectedRowVersion != currentRowVersion {
			return stopReuseResult{}, execution.Changed, rejectionOf(versionID, &RowVersionConflict{Current: currentRowVersion})
		}
		if exited {
			return stopReuseResult{}, execution.Changed, rejectionOf(versionID, &StateConflict{State: "Exited"})
		}
		now := service.nowText()
		result, err := tx.ExecContext(ctx, `UPDATE knowledge_version_retrieval_state SET exited=1,exited_at=?,exit_reason='stopped',updated_at=?,row_version=row_version+1 WHERE knowledge_version_id=? AND row_version=?`, now, now, versionID, expectedRowVersion)
		if err != nil {
			return stopReuseResult{}, execution.Changed, err
		}
		affected, _ := result.RowsAffected()
		if affected == 0 {
			return stopReuseResult{}, execution.Changed, fmt.Errorf("stop reuse update lost its transaction fence")
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM knowledge_search_docs WHERE knowledge_version_id=?`, versionID); err != nil {
			return stopReuseResult{}, execution.Changed, err
		}
		return stopReuseResult{StoppedRowVersion: expectedRowVersion + 1}, execution.Changed, nil
	}, func(result stopReuseResult) int64 { return versionID })
	if err != nil {
		return service.translateCommandError(ctx, err)
	}
	return nil
}

// stopReuseResult carries the post-command retrieval-state row version so the
// audit target can pin the applicable version.
type stopReuseResult struct {
	StoppedRowVersion int64 `json:"stoppedRowVersion"`
}
