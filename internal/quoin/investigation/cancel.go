package investigation

// Explicit Stop (HTTP-COMMAND-005, DATA-ATTEMPT-003): the idempotent
// cancellation fence is its own versioned command — transport aborts never
// express domain cancellation. Success already committed answers the
// completed object (200, never 409); the fence moves Queued/Assigned to
// Cancelled directly and Running to Cancelling, after which the app layer
// delivers the runtime CancelAttempt frame and the runtime's CancelAck (or
// the loss convergence) finishes Cancelled — no fenced middle state
// survives (DATA-TX-005).

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/attempt"
)

// StopOutcome reports the fence decision.
type StopOutcome struct {
	AttemptID  int64
	State      string
	RowVersion int64
	// DispatchRequired is true when the fence moved Running -> Cancelling:
	// the app layer must send the runtime CancelAttempt frame after the
	// commit (RUNTIME-CANCEL-001). Replays and already-Cancelling fences
	// never re-dispatch.
	DispatchRequired bool
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
	if entry, ok, err := service.replayLookup(principalID, clientCommandID, digest); err != nil {
		return StopOutcome{}, err
	} else if ok {
		// The fence already committed; report the current authoritative
		// state (the replay never re-dispatches the runtime cancel).
		state, version, err := service.attemptState(ctx, attemptID)
		if err != nil {
			return StopOutcome{}, err
		}
		return StopOutcome{AttemptID: entry.attemptID, State: state, RowVersion: version}, nil
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return StopOutcome{}, err
	}
	committed := false
	notifyAttempt := int64(0)
	// Same load-bearing defer order as the result adjudication: the
	// terminal notification may only query the store after the connection
	// is back in the pool (LIFO).
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
		return StopOutcome{}, err
	}
	if entry, ok, err := service.replayLookup(principalID, clientCommandID, digest); err != nil {
		return StopOutcome{}, err
	} else if ok {
		state, version, err := service.attemptStateOn(ctx, conn, attemptID)
		if err != nil {
			return StopOutcome{}, err
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return StopOutcome{}, err
		}
		committed = true
		return StopOutcome{AttemptID: entry.attemptID, State: state, RowVersion: version}, nil
	}
	var attemptType, scopeType, state string
	var scopeID, rowVersion int64
	err = conn.QueryRowContext(ctx, `
		SELECT attempt_type, scope_type, scope_id, state, row_version
		FROM execution_attempts WHERE id=?`, attemptID).
		Scan(&attemptType, &scopeType, &scopeID, &state, &rowVersion)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (scopeType != "investigation" || scopeID != investigationID)) {
		return StopOutcome{}, ErrNotFound
	}
	if err != nil {
		return StopOutcome{}, err
	}
	now := service.nowText()
	switch state {
	case "Succeeded", "Failed", "Cancelled", "Interrupted":
		// Success (or another terminal state) won the commit-order race:
		// answer the completed object, not a conflict (HTTP-COMMAND-005).
		if err := recordAudit(ctx, conn, "user", principalID, "investigation.attempt.stop", "success", "investigation", investigationID, now); err != nil {
			return StopOutcome{}, err
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return StopOutcome{}, err
		}
		committed = true
		service.replayRemember(principalID, clientCommandID, replayEntry{investigationID: investigationID, attemptID: attemptID, digest: digest})
		return StopOutcome{AttemptID: attemptID, State: state, RowVersion: rowVersion}, nil
	}
	if rowVersion != expectedRowVersion {
		return StopOutcome{}, &attempt.RowVersionError{ID: attemptID, Current: rowVersion}
	}
	if err := recordAudit(ctx, conn, "user", principalID, "investigation.attempt.stop", "success", "investigation", investigationID, now); err != nil {
		return StopOutcome{}, err
	}
	fenceState, err := service.attempts.CancelFenceOn(ctx, conn, attemptID)
	if err != nil {
		return StopOutcome{}, err
	}
	outcome := StopOutcome{AttemptID: attemptID, State: fenceState, RowVersion: rowVersion + 1}
	if fenceState == "Cancelling" {
		// Idempotent on an already-Cancelling attempt, but only a
		// Running -> Cancelling move owes the runtime a cancel frame.
		outcome.DispatchRequired = state == "Running"
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return StopOutcome{}, err
	}
	committed = true
	service.replayRemember(principalID, clientCommandID, replayEntry{investigationID: investigationID, attemptID: attemptID, digest: digest})
	if fenceState == "Cancelled" {
		// No runtime ack will arrive for a directly-cancelled attempt:
		// close the attached stream with the cancelled terminal view (the
		// deferred call runs after the connection is back in the pool).
		notifyAttempt = attemptID
	}
	return outcome, nil
}

// attemptState reads one attempt's current state and row version.
func (service *Service) attemptState(ctx context.Context, attemptID int64) (string, int64, error) {
	var state string
	var rowVersion int64
	err := service.db.QueryRowContext(ctx, `SELECT state, row_version FROM execution_attempts WHERE id=?`, attemptID).
		Scan(&state, &rowVersion)
	if err != nil {
		return "", 0, err
	}
	return state, rowVersion, nil
}

// attemptStateOn is the conn-scoped variant of attemptState.
func (service *Service) attemptStateOn(ctx context.Context, conn *sql.Conn, attemptID int64) (string, int64, error) {
	var state string
	var rowVersion int64
	err := conn.QueryRowContext(ctx, `SELECT state, row_version FROM execution_attempts WHERE id=?`, attemptID).
		Scan(&state, &rowVersion)
	if err != nil {
		return "", 0, err
	}
	return state, rowVersion, nil
}
