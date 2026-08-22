package investigation

// Latest-turn Undo (DATA-INVEST-002): one transaction withdraws the latest
// user turn and every successor (assistant replies, tool calls, evidence
// references, knowledge drafts stay as a read-only withdrawn branch), fences
// the turn's active attempt (Queued/Assigned close directly; Running closes
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
	AttemptState string
	// DispatchRequired is true when the attempt moved Running -> Cancelling:
	// the app layer must deliver the runtime CancelAttempt frame after the
	// commit (RUNTIME-CANCEL-001). Replayed commands never re-dispatch.
	DispatchRequired bool
}

// undoDigest fingerprints the undo command's semantic fields (the scoped
// investigation and the head fence; HTTP-COMMAND-002).
func undoDigest(investigationID, expectedHead int64) string {
	return commandDigest("undo:"+strconv.FormatInt(investigationID, 10)+"@"+strconv.FormatInt(expectedHead, 10), "", nil, nil, nil)
}

// Undo withdraws the latest user turn and all successors in one
// transaction (DATA-INVEST-002). A replayed command reports the current
// authoritative state; a reused command id with a different request
// conflicts (HTTP-COMMAND-003).
func (service *Service) Undo(ctx context.Context, principalID int64, clientCommandID string, investigationID, expectedHead int64) (UndoOutcome, error) {
	digest := undoDigest(investigationID, expectedHead)
	if _, ok, err := service.replayLookup(principalID, clientCommandID, digest); err != nil {
		return UndoOutcome{}, err
	} else if ok {
		// The withdrawal already committed; the caller re-reads the
		// authoritative projection (the wire response is the current
		// detail, exactly like the original execution).
		return UndoOutcome{InvestigationID: investigationID}, nil
	}
	if expectedHead <= 0 {
		return UndoOutcome{}, &HeadConflictError{}
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return UndoOutcome{}, err
	}
	committed := false
	notifyAttempt := int64(0)
	// Defer order is load-bearing on a single-connection pool: the
	// rollback guard runs first, then the connection returns to the pool,
	// and only then may the terminal notification query the store (LIFO).
	defer func() {
		if notifyAttempt != 0 {
			service.NotifyTerminal(context.Background(), notifyAttempt)
		}
	}()
	defer conn.Close()
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return UndoOutcome{}, err
	}
	// Second replay check inside the serialized transaction.
	if _, ok, err := service.replayLookup(principalID, clientCommandID, digest); err != nil {
		return UndoOutcome{}, err
	} else if ok {
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return UndoOutcome{}, err
		}
		committed = true
		return UndoOutcome{InvestigationID: investigationID}, nil
	}
	var currentHead sql.NullInt64
	if err := conn.QueryRowContext(ctx, `
		SELECT current_head_message_id FROM investigations WHERE id=?`, investigationID).Scan(&currentHead); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UndoOutcome{}, ErrNotFound
		}
		return UndoOutcome{}, err
	}
	if !currentHead.Valid || currentHead.Int64 != expectedHead {
		var head *int64
		if currentHead.Valid {
			value := currentHead.Int64
			head = &value
		}
		return UndoOutcome{}, &HeadConflictError{CurrentHead: head}
	}
	// The withdrawn set is the latest active user turn and every successor:
	// appends always continue after the whole history (nextMessageSeq reads
	// MAX(seq) over all messages), so the withdrawal set is a suffix and
	// the remaining active messages keep one contiguous branch.
	var userMessageID, userSeq int64
	err = conn.QueryRowContext(ctx, `
		SELECT id, seq FROM investigation_messages
		WHERE investigation_id=? AND status='active' AND role='user'
		ORDER BY seq DESC LIMIT 1`, investigationID).Scan(&userMessageID, &userSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return UndoOutcome{}, ErrNoUserTurn
	}
	if err != nil {
		return UndoOutcome{}, err
	}
	withdraw, err := conn.ExecContext(ctx, `
		UPDATE investigation_messages SET status='withdrawn'
		WHERE investigation_id=? AND seq>=? AND status='active'`, investigationID, userSeq)
	if err != nil {
		return UndoOutcome{}, err
	}
	withdrawn, _ := withdraw.RowsAffected()
	var newHead sql.NullInt64
	if err := conn.QueryRowContext(ctx, `
		SELECT id FROM investigation_messages
		WHERE investigation_id=? AND status='active'
		ORDER BY seq DESC LIMIT 1`, investigationID).Scan(&newHead); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return UndoOutcome{}, err
	}
	if newHead.Valid {
		if _, err := conn.ExecContext(ctx, `
			UPDATE investigations SET current_head_message_id=? WHERE id=?`, newHead.Int64, investigationID); err != nil {
			return UndoOutcome{}, err
		}
	} else if _, err := conn.ExecContext(ctx, `
		UPDATE investigations SET current_head_message_id=NULL WHERE id=?`, investigationID); err != nil {
		return UndoOutcome{}, err
	}
	outcome := UndoOutcome{InvestigationID: investigationID, Withdrawn: withdrawn}
	if newHead.Valid {
		head := newHead.Int64
		outcome.NewHead = &head
	}
	// The single active attempt (if any) depends on this turn by
	// construction: sends and retries are fenced on "no active attempt",
	// so while one exists its user message is the latest active user
	// message being withdrawn.
	var activeAttempt sql.NullInt64
	if err := conn.QueryRowContext(ctx, `
		SELECT id FROM execution_attempts
		WHERE scope_type='investigation' AND scope_id=? AND state IN `+activeAttemptStates+` LIMIT 1`,
		investigationID).Scan(&activeAttempt); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return UndoOutcome{}, err
	}
	now := service.nowText()
	if activeAttempt.Valid {
		state, err := service.attempts.CancelFenceOn(ctx, conn, activeAttempt.Int64)
		if err != nil {
			return UndoOutcome{}, err
		}
		outcome.AttemptID = activeAttempt.Int64
		outcome.AttemptState = state
		if state == "Cancelling" {
			// A Running attempt the runtime must be told to stop; the
			// fence itself is already committed by the time the dispatch
			// runs (RUNTIME-CANCEL-001).
			outcome.DispatchRequired = true
		}
	}
	if err := recordAudit(ctx, conn, "user", principalID, "investigation.message.undo", "success", "investigation", investigationID, now); err != nil {
		return UndoOutcome{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return UndoOutcome{}, err
	}
	committed = true
	service.replayRemember(principalID, clientCommandID, replayEntry{investigationID: investigationID, attemptID: outcome.AttemptID, digest: digest})
	if outcome.AttemptID != 0 && outcome.AttemptState == "Cancelled" {
		// A directly-cancelled attempt has no runtime ack to wait for:
		// close the attached stream with the cancelled terminal view (the
		// deferred call runs after the connection is back in the pool).
		notifyAttempt = outcome.AttemptID
	}
	return outcome, nil
}
