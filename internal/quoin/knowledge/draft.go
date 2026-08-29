package knowledge

// draft.go implements the revisioned draft edit (DATA-KNOWLEDGE-004):
// only an AwaitingConfirmation candidate accepts edits, the command
// carries the expected draft revision compared in the same UPDATE, and a
// stale revision is a conflict that reports the authoritative current
// value. The original suggestion column is never touched. Command
// outcomes land in the durable ledger (HTTP-COMMAND-003/004).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/Suknna/quoin/internal/quoin/auth"
)

// ErrInvalidScope maps to 422: the scope edit was not a JSON object.
var ErrInvalidScope = errors.New("draft scope must be a JSON object")

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

// ErrEmptyEdit maps to 422: neither title nor body was provided.
var ErrEmptyEdit = errors.New("draft edit requires title or body")

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
	if scope != nil {
		normalized, err := normalizeScope(*scope)
		if err != nil {
			// Deterministic shape rejection: validated before any ledger row
			// exists, so a plain domain error is enough.
			return CandidateSummary{}, ErrInvalidScope
		}
		fields["scope"] = normalized
	}
	digest := commandDigest(ledgerEdit, fields)
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
	if title == nil && body == nil && scope == nil {
		// A deterministic validation rejection is ledger-recorded so the
		// same command id replays the same 422 (HTTP-COMMAND-004).
		return CandidateSummary{}, rejectCandidateCommand(ctx, conn, principalID, commandID, ledgerEdit, digest, candidateID, ErrEmptyEdit)
	}
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
		normalized, err := normalizeScope(*scope)
		if err != nil {
			return CandidateSummary{}, rejectCandidateCommand(ctx, conn, principalID, commandID, ledgerEdit, digest, candidateID, ErrInvalidScope)
		}
		set += ", draft_scope_json=?"
		args = append(args, scopeValue(normalized))
	}
	result, err := conn.ExecContext(ctx, `UPDATE knowledge_candidates SET `+set+` WHERE id=? AND state='AwaitingConfirmation' AND draft_revision=?`,
		append(args, candidateID, expectedRevision)...)
	if err != nil {
		return CandidateSummary{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return CandidateSummary{}, err
	}
	if affected == 0 {
		return CandidateSummary{}, rejectCandidateCommand(ctx, conn, principalID, commandID, ledgerEdit, digest, candidateID, service.editConflict(ctx, conn, candidateID))
	}
	now := service.nowText()
	if err := recordAudit(ctx, conn, principalID, ledgerEdit, "knowledge_candidate", candidateID, now); err != nil {
		return CandidateSummary{}, err
	}
	// Read back through the same connection (the pool may be fully
	// occupied while this handle is open).
	summary, err := scanCandidateOn(ctx, conn, candidateID)
	if err != nil {
		return CandidateSummary{}, err
	}
	if err := commitCandidateCommand(ctx, conn, principalID, commandID, ledgerEdit, digest, candidateID, summary); err != nil {
		return CandidateSummary{}, err
	}
	committed = true
	return summary, nil
}

// editConflict classifies a zero-row edit: missing candidate, terminal
// state or a stale expected revision.
func (service *Service) editConflict(ctx context.Context, conn *sql.Conn, candidateID int64) error {
	var state string
	var draftRevision int64
	err := conn.QueryRowContext(ctx, `SELECT state, draft_revision FROM knowledge_candidates WHERE id=?`, candidateID).Scan(&state, &draftRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state != StateAwaiting {
		return &StateConflict{State: state}
	}
	return &RevisionConflict{Current: draftRevision}
}

// GetCandidateSummary reads one candidate outside a transaction.
func (service *Service) GetCandidateSummary(ctx context.Context, candidateID int64) (CandidateSummary, error) {
	row := service.db.QueryRowContext(ctx, `SELECT `+candidateColumns+` FROM knowledge_candidates c WHERE c.id=?`, candidateID)
	summary, err := readCandidateRow(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return CandidateSummary{}, ErrNotFound
	}
	return summary, err
}

// replaySummary replays a ledger hit for a candidate command whose
// payload is the plain summary (edit/confirm/exclude).
func replaySummary(record authRecord, digest string) (CandidateSummary, error) {
	summary, _, err := replayCandidateOutcome(record, digest)
	return summary, err
}
