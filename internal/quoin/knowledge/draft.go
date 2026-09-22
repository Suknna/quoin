package knowledge

// draft.go implements the revisioned draft edit (DATA-KNOWLEDGE-004):
// only an AwaitingConfirmation candidate accepts edits, the command
// carries the expected draft revision compared in the same UPDATE, and a
// stale revision is a conflict that reports the authoritative current
// value. The original suggestion column is never touched. Command
// outcomes land in the durable ledger through the shared runner
// (HTTP-COMMAND-003/004).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// ErrInvalidScope maps to 422: the scope edit was not a JSON object.
var ErrInvalidScope = errors.New("draft scope must be a JSON object")

// ErrEmptyEdit maps to 422: neither title nor body was provided.
var ErrEmptyEdit = errors.New("draft edit requires title or body")

// normalizeScope validates one scope edit as a JSON object and returns
// its canonical serialization (empty object allowed: it clears scope).
func normalizeScope(scope json.RawMessage) (string, error) {
	var decoded map[string]any
	if err := json.Unmarshal(scope, &decoded); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return "", err
	}
	return string(canonical), nil
}

// scopeValue maps a canonical scope to the stored column value: an empty
// object clears the column (no scope restriction).
func scopeValue(normalized string) any {
	if normalized == "{}" {
		return nil
	}
	return normalized
}

// EditDraft applies one draft edit (at least one of title/body/scope
// present) and returns the updated summary. A non-nil scope must be a
// JSON object; an empty object clears the scope.
func (service *Service) EditDraft(ctx context.Context, principalID int64, commandID string, candidateID, expectedRevision int64, title, body *string, scope *json.RawMessage) (CandidateSummary, error) {
	fields := map[string]any{"candidateId": candidateID, "expectedRevision": expectedRevision}
	if title != nil {
		fields["title"] = *title
	}
	if body != nil {
		fields["body"] = *body
	}
	var normalizedScope *string
	var normalizedScopeStr string
	if scope != nil {
		normalized, err := normalizeScope(*scope)
		if err != nil {
			// Deterministic shape rejection: validated before any ledger row
			// exists, so a plain domain error is enough.
			return CandidateSummary{}, ErrInvalidScope
		}
		fields["scope"] = normalized
		comparison := normalized
		if scopeValue(normalized) == nil {
			comparison = ""
		}
		normalizedScope = &comparison
		normalizedScopeStr = normalized
	}
	if title == nil && body == nil && scope == nil {
		return CandidateSummary{}, ErrEmptyEdit
	}
	digest := commandDigest(opEdit, fields)
	outcome, err := execution.Run(ctx, service.runner, service.edit, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: commandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (CandidateSummary, execution.Change, error) {
		set := "draft_revision=draft_revision+1, row_version=row_version+1"
		args := make([]any, 0, 5)
		if title != nil {
			set += ", draft_title=?"
			args = append(args, *title)
		}
		if body != nil {
			set += ", draft_body=?"
			args = append(args, *body)
		}
		if scope != nil {
			set += ", draft_scope_json=?"
			args = append(args, scopeValue(normalizedScopeStr))
		}
		var currentTitle, currentBody, currentScope string
		if err := tx.QueryRowContext(ctx, `SELECT draft_title,draft_body,COALESCE(draft_scope_json,'') FROM knowledge_candidates WHERE id=? AND state='AwaitingConfirmation' AND draft_revision=? AND (import_batch_id IS NULL OR EXISTS (SELECT 1 FROM knowledge_import_batches b WHERE b.id=import_batch_id AND b.state='AwaitingConfirmation'))`, candidateID, expectedRevision).Scan(&currentTitle, &currentBody, &currentScope); err == nil {
			unchanged := (title == nil || *title == currentTitle) && (body == nil || *body == currentBody) && (normalizedScope == nil || *normalizedScope == currentScope)
			if unchanged {
				summary, scanErr := scanCandidateOn(ctx, tx, candidateID)
				if scanErr != nil {
					return CandidateSummary{}, execution.Changed, scanErr
				}
				return summary, execution.Unchanged, nil
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return CandidateSummary{}, execution.Changed, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE knowledge_candidates SET `+set+` WHERE id=? AND state='AwaitingConfirmation' AND draft_revision=? AND (import_batch_id IS NULL OR EXISTS (SELECT 1 FROM knowledge_import_batches b WHERE b.id=import_batch_id AND b.state='AwaitingConfirmation'))`,
			append(args, candidateID, expectedRevision)...)
		if err != nil {
			return CandidateSummary{}, execution.Changed, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return CandidateSummary{}, execution.Changed, err
		}
		if affected == 0 {
			kind, infraErr := service.editConflict(ctx, tx, candidateID)
			if infraErr != nil {
				return CandidateSummary{}, execution.Changed, infraErr
			}
			return CandidateSummary{}, execution.Changed, rejectionOf(candidateID, kind)
		}
		summary, err := scanCandidateOn(ctx, tx, candidateID)
		if err != nil {
			return CandidateSummary{}, execution.Changed, err
		}
		return summary, execution.Changed, nil
	}, func(summary CandidateSummary) int64 {
		return parseCandidateLocator(summary.ID)
	})
	if err != nil {
		return CandidateSummary{}, service.translateCommandError(ctx, err)
	}
	return outcome.Result, nil
}

// editConflict classifies a zero-row edit: missing candidate, terminal
// state or a stale expected revision.
func (service *Service) editConflict(ctx context.Context, q queryer, candidateID int64) (error, error) {
	var state string
	var batchState sql.NullString
	var draftRevision int64
	err := q.QueryRowContext(ctx, `SELECT c.state, c.draft_revision, b.state FROM knowledge_candidates c LEFT JOIN knowledge_import_batches b ON b.id=c.import_batch_id WHERE c.id=?`, candidateID).Scan(&state, &draftRevision, &batchState)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound, nil
	}
	if err != nil {
		return nil, err
	}
	if state != StateAwaiting {
		return &StateConflict{State: state}, nil
	}
	if batchState.Valid && batchState.String != "AwaitingConfirmation" {
		return &StateConflict{State: batchState.String}, nil
	}
	return &RevisionConflict{Current: draftRevision}, nil
}

// GetCandidateSummary reads one candidate outside a transaction.
func (service *Service) GetCandidateSummary(ctx context.Context, candidateID int64) (CandidateSummary, error) {
	row := service.reader.QueryRowContext(ctx, `SELECT `+candidateColumns+` FROM knowledge_candidates c`+candidateSourceJoin+` WHERE c.id=?`, candidateID)
	summary, err := readCandidateRow(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return CandidateSummary{}, ErrNotFound
	}
	return summary, err
}

// rejectionOf 把类型化拒绝翻译成执行器的确定性拒绝（code 承载类别，Detail
// 承载状态令牌或人类可读文本，ObjectID 携带对象引用供重放时重读权威版本）。
func rejectionOf(objectID int64, rejection error) *execution.Rejection {
	var revision *RevisionConflict
	var rowVersion *RowVersionConflict
	var state *StateConflict
	switch {
	case errors.As(rejection, &revision):
		return &execution.Rejection{Code: codeRevisionConflict, Detail: rejection.Error(), ObjectID: objectID}
	case errors.As(rejection, &rowVersion):
		return &execution.Rejection{Code: codeRowVersionConflict, Detail: rejection.Error(), ObjectID: objectID}
	case errors.As(rejection, &state):
		return &execution.Rejection{Code: codeStateConflict, Detail: state.State, ObjectID: objectID}
	case errors.Is(rejection, ErrSourceRejected):
		return &execution.Rejection{Code: codeSourceRejected, Detail: rejection.Error(), ObjectID: objectID}
	case errors.Is(rejection, ErrSourceShape):
		return &execution.Rejection{Code: codeSourceShape, Detail: rejection.Error(), ObjectID: objectID}
	case errors.Is(rejection, ErrNotFound):
		return &execution.Rejection{Code: codeNotFound, Detail: rejection.Error(), ObjectID: objectID}
	case errors.Is(rejection, ErrEmptyEdit):
		return &execution.Rejection{Code: codeEmptyEdit, Detail: rejection.Error(), ObjectID: objectID}
	case errors.Is(rejection, ErrPartialBatchConfirm):
		return &execution.Rejection{Code: codePartialBatch, Detail: rejection.Error(), ObjectID: objectID}
	default:
		return &execution.Rejection{Code: "unknown", Detail: rejection.Error(), ObjectID: objectID}
	}
}

func parseCandidateLocator(value string) int64 {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return id
}
