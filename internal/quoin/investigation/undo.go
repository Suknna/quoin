package investigation

// Latest-turn Undo (DATA-INVEST-002): one transaction withdraws the latest
// user turn and every successor (assistant replies, tool calls, evidence
// references, knowledge drafts stay as a read-only withdrawn branch), fences
// the turn's active attempt (Queued closes directly; Assigned/Running close
// to Cancelling with the runtime cancel dispatched after commit) and moves
// the head to the last remaining active message — NULL when the whole
// branch is withdrawn, after which the next send carries an explicit
// expected_head_message_id=null. Late results for the withdrawn turn stay
// audit-only (CommitResult rejects non-active user messages); withdrawn
// messages never re-enter a new attempt input snapshot (schema trigger).

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/knowledge/invalidation"
)

// ErrNoUserTurn reports an undo against a head that owns no active user
// turn (defensive: the invariant keeps this unobservable — a non-null head
// always has an active user turn underneath).
var ErrNoUserTurn = errors.New("investigation has no active user turn to withdraw")

// UndoOutcome reports the committed withdrawal facts.
type UndoOutcome struct {
	InvestigationID int64
	// NewHead is nil when the whole active branch was withdrawn (the next
	// send must carry expected_head_message_id=null).
	NewHead *int64
	// Withdrawn counts the messages that flipped active -> withdrawn.
	Withdrawn int64
	// AttemptID is the fenced attempt (0 when no active attempt depended
	// on the turn).
	AttemptID int64
	// AttemptState is the fenced attempt state after the transaction.
	AttemptState string `json:",omitempty"`
	// DispatchRequired is true when the attempt moved Assigned/Running ->
	// Cancelling: the app layer must deliver the runtime CancelAttempt frame after the
	// commit (RUNTIME-CANCEL-001). Replayed commands never re-dispatch.
	DispatchRequired bool `json:",omitempty"`
}

// undoDigest fingerprints the undo command's semantic fields (the scoped
// investigation and the head fence; HTTP-COMMAND-002).
func undoDigest(investigationID, expectedHead int64) string {
	return commandDigest("undo:"+strconv.FormatInt(investigationID, 10)+"@"+strconv.FormatInt(expectedHead, 10), "", nil, nil, nil)
}

// Undo withdraws the latest user turn and all successors in one runner
// transaction, with the command ledger row and the automatic audit event in
// the same commit (DATA-INVEST-002). A replayed command returns the stored
// outcome; a reused command id with a different request conflicts
// (HTTP-COMMAND-003).
func (service *Service) Undo(ctx context.Context, principalID int64, clientCommandID string, investigationID, expectedHead int64) (UndoOutcome, error) {
	digest := undoDigest(investigationID, expectedHead)
	if expectedHead <= 0 {
		return UndoOutcome{}, &HeadConflictError{}
	}
	// dispatch and streamClose are the post-commit runtime/stream
	// notification decisions of the FIRST execution. They are deliberately
	// not part of the replayable ledger payload: a replayed command never
	// re-dispatches the runtime cancel nor re-closes a settled stream.
	dispatch := false
	notify := int64(0)
	outcome, err := execution.Run(ctx, service.runner, service.opUndo, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (UndoOutcome, execution.Change, error) {
		var currentHead sql.NullInt64
		if err := tx.QueryRowContext(ctx, `
			SELECT current_head_message_id FROM investigations WHERE id=?`, investigationID).Scan(&currentHead); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return UndoOutcome{}, execution.Changed, ErrNotFound
			}
			return UndoOutcome{}, execution.Changed, err
		}
		if !currentHead.Valid || currentHead.Int64 != expectedHead {
			var head *int64
			if currentHead.Valid {
				value := currentHead.Int64
				head = &value
			}
			return UndoOutcome{}, execution.Changed, &HeadConflictError{CurrentHead: head}
		}
		// The withdrawn set is the latest active user turn and every successor:
		// appends always continue after the whole history (nextMessageSeq reads
		// MAX(seq) over all messages), so the withdrawal set is a suffix and
		// the remaining active messages keep one contiguous branch.
		var userMessageID, userSeq int64
		err := tx.QueryRowContext(ctx, `
			SELECT id, seq FROM investigation_messages
			WHERE investigation_id=? AND status='active' AND role='user'
			ORDER BY seq DESC LIMIT 1`, investigationID).Scan(&userMessageID, &userSeq)
		if errors.Is(err, sql.ErrNoRows) {
			return UndoOutcome{}, execution.Changed, ErrNoUserTurn
		}
		if err != nil {
			return UndoOutcome{}, execution.Changed, err
		}
		withdraw, err := tx.ExecContext(ctx, `
			UPDATE investigation_messages SET status='withdrawn'
			WHERE investigation_id=? AND seq>=? AND status='active'`, investigationID, userSeq)
		if err != nil {
			return UndoOutcome{}, execution.Changed, err
		}
		withdrawn, _ := withdraw.RowsAffected()
		// DATA-TX-011 in the same transaction: knowledge candidates sourced
		// from the withdrawn assistant messages become SourceInvalid and every
		// version those sources produced permanently exits retrieval.
		withdrawnSources, sourcesErr := tx.QueryContext(ctx, `
			SELECT id FROM investigation_messages
			WHERE investigation_id=? AND seq>=? AND role='assistant' AND status='withdrawn'`, investigationID, userSeq)
		if sourcesErr != nil {
			return UndoOutcome{}, execution.Changed, sourcesErr
		}
		var withdrawnMessageIDs []int64
		for withdrawnSources.Next() {
			var messageID int64
			if scanErr := withdrawnSources.Scan(&messageID); scanErr != nil {
				withdrawnSources.Close()
				return UndoOutcome{}, execution.Changed, scanErr
			}
			withdrawnMessageIDs = append(withdrawnMessageIDs, messageID)
		}
		if closeErr := withdrawnSources.Close(); closeErr != nil {
			return UndoOutcome{}, execution.Changed, closeErr
		}
		if _, invalidationErr := invalidation.Apply(ctx, tx, invalidation.SourceInvestigationMessage, withdrawnMessageIDs, service.nowText()); invalidationErr != nil {
			return UndoOutcome{}, execution.Changed, invalidationErr
		}
		var newHead sql.NullInt64
		if err := tx.QueryRowContext(ctx, `
			SELECT id FROM investigation_messages
			WHERE investigation_id=? AND status='active'
			ORDER BY seq DESC LIMIT 1`, investigationID).Scan(&newHead); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return UndoOutcome{}, execution.Changed, err
		}
		if newHead.Valid {
			if _, err := tx.ExecContext(ctx, `
				UPDATE investigations SET current_head_message_id=? WHERE id=?`, newHead.Int64, investigationID); err != nil {
				return UndoOutcome{}, execution.Changed, err
			}
		} else if _, err := tx.ExecContext(ctx, `
			UPDATE investigations SET current_head_message_id=NULL WHERE id=?`, investigationID); err != nil {
			return UndoOutcome{}, execution.Changed, err
		}
		result := UndoOutcome{InvestigationID: investigationID, Withdrawn: withdrawn}
		if newHead.Valid {
			head := newHead.Int64
			result.NewHead = &head
		}
		// The single active attempt (if any) depends on this turn by
		// construction: sends and retries are fenced on "no active attempt",
		// so while one exists its user message is the latest active user
		// message being withdrawn.
		var activeAttempt sql.NullInt64
		if err := tx.QueryRowContext(ctx, `
			SELECT id FROM execution_attempts
			WHERE scope_type='investigation' AND scope_id=? AND state IN `+activeAttemptStates+` LIMIT 1`,
			investigationID).Scan(&activeAttempt); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return UndoOutcome{}, execution.Changed, err
		}
		if activeAttempt.Valid {
			state, err := service.cancelFenceOn(ctx, tx, activeAttempt.Int64)
			if err != nil {
				return UndoOutcome{}, execution.Changed, err
			}
			result.AttemptID = activeAttempt.Int64
			result.AttemptState = state
			if state == "Cancelling" {
				// A Running attempt the runtime must be told to stop; the
				// fence itself is already committed by the time the dispatch
				// runs (RUNTIME-CANCEL-001).
				dispatch = true
			}
			if state == "Cancelled" {
				// A directly-cancelled attempt has no runtime ack to wait
				// for: close the attached stream with the cancelled terminal
				// view (the caller notifies after the runner committed).
				notify = activeAttempt.Int64
			}
		}
		return result, execution.Changed, nil
	}, func(result UndoOutcome) int64 { return result.InvestigationID })
	if err != nil {
		return UndoOutcome{}, commandError(err)
	}
	if !outcome.Replayed {
		outcome.Result.DispatchRequired = dispatch
		if notify != 0 {
			service.NotifyTerminal(context.Background(), notify)
		}
	}
	return outcome.Result, nil
}
