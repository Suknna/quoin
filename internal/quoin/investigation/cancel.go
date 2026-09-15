package investigation

// Explicit Stop (HTTP-COMMAND-005, DATA-ATTEMPT-003): the idempotent
// cancellation fence is its own versioned durable command — transport aborts
// never express domain cancellation. Success already committed answers the
// completed object (200, never 409); the fence moves Queued to Cancelled
// directly and Assigned/Running to Cancelling, after which the app layer
// delivers the runtime CancelAttempt frame and the runtime's CancelAck (or
// the loss convergence) finishes Cancelled — no fenced middle state
// survives (DATA-TX-005).

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// StopOutcome reports the fence decision.
type StopOutcome struct {
	AttemptID  int64
	State      string
	RowVersion int64
	// DispatchRequired is true when the fence moved Assigned/Running ->
	// Cancelling: the app layer must send the runtime CancelAttempt frame after the
	// commit (RUNTIME-CANCEL-001). Replays and already-Cancelling fences
	// never re-dispatch.
	DispatchRequired bool `json:",omitempty"`
}

// stopDigest fingerprints the stop command's semantic fields (the scoped
// investigation, the attempt locator and the row-version fence;
// HTTP-COMMAND-002).
func stopDigest(investigationID, attemptID, expectedRowVersion int64) string {
	target := "stop:" + strconv.FormatInt(investigationID, 10) + "@" + strconv.FormatInt(attemptID, 10) + "#v" + strconv.FormatInt(expectedRowVersion, 10)
	return commandDigest(target, "", nil, nil, nil)
}

// Cancel commits the idempotent cancellation fence for one investigation
// attempt. The expected row version fences concurrent transitions, except
// that an attempt already in a terminal state answers with the completed
// object regardless of the expected version (HTTP-COMMAND-005: the race
// resolves as "already completed", never as a version conflict).
func (service *Service) Cancel(ctx context.Context, principalID int64, clientCommandID string, investigationID, attemptID, expectedRowVersion int64) (StopOutcome, error) {
	digest := stopDigest(investigationID, attemptID, expectedRowVersion)
	// dispatch is the post-commit runtime-notification decision of the FIRST
	// execution. It is deliberately not part of the replayable ledger
	// payload: a replayed command never re-dispatches the runtime cancel,
	// exactly like the previous in-process replay.
	dispatch := false
	notify := int64(0)
	outcome, err := execution.Run(ctx, service.runner, service.opStop, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (StopOutcome, execution.Change, error) {
		var attemptType, scopeType, state string
		var scopeID, rowVersion int64
		err := tx.QueryRowContext(ctx, `
			SELECT attempt_type, scope_type, scope_id, state, row_version
			FROM execution_attempts WHERE id=?`, attemptID).
			Scan(&attemptType, &scopeType, &scopeID, &state, &rowVersion)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && (scopeType != "investigation" || scopeID != investigationID)) {
			return StopOutcome{}, execution.Unchanged, ErrNotFound
		}
		if err != nil {
			return StopOutcome{}, execution.Unchanged, err
		}
		switch state {
		case "Succeeded", "Failed", "Cancelled", "Interrupted":
			// Success (or another terminal state) won the commit-order race:
			// answer the completed object, not a conflict (HTTP-COMMAND-005).
			return StopOutcome{AttemptID: attemptID, State: state, RowVersion: rowVersion}, execution.Unchanged, nil
		}
		if rowVersion != expectedRowVersion {
			return StopOutcome{}, execution.Unchanged, &attempt.RowVersionError{ID: attemptID, Current: rowVersion}
		}
		fenceState, err := service.cancelFenceOn(ctx, tx, attemptID)
		if err != nil {
			return StopOutcome{}, execution.Unchanged, err
		}
		// CancelFenceOn may be a durable no-op when an already-claimed normal close
		// won the SQLite ordering point. Return the row that actually committed, not
		// a fabricated +1 version.
		committedState, committedVersion, err := attemptStateOn(ctx, tx, attemptID)
		if err != nil {
			return StopOutcome{}, execution.Unchanged, err
		}
		if fenceState == "Cancelling" {
			// Assigned may already have a DispatchAttempt frame in flight. Like a
			// Running attempt, it owes the Runtime one post-commit cancel frame;
			// only a replayed Cancelling fence avoids duplicate dispatch.
			dispatch = state == "Assigned" || state == "Running"
		}
		if fenceState == "Cancelled" {
			// No runtime ack will arrive for a directly-cancelled attempt:
			// close the attached stream with the cancelled terminal view (the
			// caller notifies after the runner committed).
			notify = attemptID
		}
		return StopOutcome{AttemptID: attemptID, State: committedState, RowVersion: committedVersion}, execution.Changed, nil
	}, func(outcome StopOutcome) int64 { return outcome.AttemptID })
	if err != nil {
		return StopOutcome{}, commandError(err)
	}
	if !outcome.Replayed {
		outcome.Result.DispatchRequired = dispatch
		if notify != 0 {
			service.NotifyTerminal(context.Background(), notify)
		}
	}
	return outcome.Result, nil
}

// cancelFenceOn is the executor-scoped cancellation fence, mirroring the
// shared attempt machine's CancelFenceOn state machine (the attempt package
// exposes it only on *sql.Conn; the runner's guarded transaction composes
// through writer). Queued closes as Cancelled directly;
// Assigned/Running close to Cancelling because an Assigned DispatchAttempt
// may already be in flight; terminal attempts are returned unchanged.
func (service *Service) cancelFenceOn(ctx context.Context, tx writer, attemptID int64) (string, error) {
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&state); err != nil {
		return "", err
	}
	switch state {
	case "Succeeded", "Failed", "Cancelled", "Interrupted":
		return state, nil
	case "Queued":
		result, err := tx.ExecContext(ctx, `
			UPDATE execution_attempts
			SET state='Cancelled', ended_at=?, termination_reason='cancelled', row_version=row_version+1
			WHERE id=? AND state=?`, service.nowText(), attemptID, state)
		if err != nil {
			return "", err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return "", errors.New("attempt " + strconv.FormatInt(attemptID, 10) + " cancellation fence lost the race")
		}
		return "Cancelled", nil
	case "Assigned", "Running", "Cancelling":
		// Assigned is a dispatch-commit state, not proof that the runtime has
		// not started: it receives the same durable cancellation and replay
		// treatment as Running (RUNTIME-CANCEL-001). Whether the UPDATE won
		// or the attempt was already Cancelling, the state is the same (the
		// fence is idempotent).
		if _, err := tx.ExecContext(ctx, `
			UPDATE execution_attempts SET state='Cancelling', row_version=row_version+1
			WHERE id=? AND state IN ('Assigned','Running')`, attemptID); err != nil {
			return "", err
		}
		return "Cancelling", nil
	default:
		return "", errors.New("attempt " + strconv.FormatInt(attemptID, 10) + " has unknown state " + state)
	}
}

// attemptStateOn reads one attempt's current state and row version on the
// caller's transaction.
func attemptStateOn(ctx context.Context, tx audit.Reader, attemptID int64) (string, int64, error) {
	var state string
	var rowVersion int64
	err := tx.QueryRowContext(ctx, `SELECT state, row_version FROM execution_attempts WHERE id=?`, attemptID).
		Scan(&state, &rowVersion)
	if err != nil {
		return "", 0, err
	}
	return state, rowVersion, nil
}
