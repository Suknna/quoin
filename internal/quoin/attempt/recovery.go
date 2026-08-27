package attempt

// Recovery semantics for dispatched attempts (T12, RUNTIME-TASK-005/006/
// 007, RUNTIME-CANCEL-003): loss interruption on new boot, slot replacement
// or lease expiry; heartbeat/reconcile lease renewal; same-boot re-dispatch
// rebinding; and the periodic lease sweeper that converges every active
// attempt whose lease burned down without renewal. Commit order stays with
// SQLite: every transition is a single fenced UPDATE.

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// LossReasons are the execution_attempts.termination_reason values that may
// interrupt an active attempt (schema CHECK): a new boot maps to lease_expired
// (RUNTIME-TASK-006), slot replacement to replaced and credential revocation
// to revoked.
var LossReasons = map[string]bool{
	"lease_expired": true,
	"replaced":      true,
	"revoked":       true,
}

// Interrupt converges one active attempt to its loss terminal state
// (RUNTIME-TASK-006): Assigned/Running close as Interrupted with the loss
// reason; an attempt whose cancellation fence already committed converges to
// Cancelled instead (the fence exception); terminal attempts are returned
// unchanged so callers can stay idempotent.
func (service *Service) Interrupt(ctx context.Context, attemptID int64, reason string) (string, error) {
	if !LossReasons[reason] {
		return "", fmt.Errorf("attempt %d loss reason %q is not a closed interruption reason", attemptID, reason)
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return "", err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	final, err := service.InterruptOn(ctx, conn, attemptID, reason)
	if err != nil {
		return "", err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return "", err
	}
	committed = true
	return final, nil
}

// InterruptOn is the conn-scoped variant of Interrupt for callers composing
// the loss convergence with their own scope updates in one transaction
// (SQLite single-writer forbids nested BEGIN).
func (service *Service) InterruptOn(ctx context.Context, conn *sql.Conn, attemptID int64, reason string) (string, error) {
	var state string
	if err := conn.QueryRowContext(ctx, `SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&state); err != nil {
		return "", err
	}
	now := service.nowText()
	switch state {
	case "Succeeded", "Failed", "Cancelled", "Interrupted":
		// Loss raced a terminal result: the result keeps the state it won.
		return state, nil
	case "Cancelling":
		// The cancellation fence already committed; loss converges it to
		// Cancelled (RUNTIME-TASK-006 fence exception).
		result, err := conn.ExecContext(ctx, `
			UPDATE execution_attempts
			SET state='Cancelled', ended_at=?, termination_reason='cancelled', row_version=row_version+1
			WHERE id=? AND state='Cancelling'`, now, attemptID)
		if err != nil {
			return "", err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return "", fmt.Errorf("attempt %d loss convergence lost the race", attemptID)
		}
		return "Cancelled", nil
	case "Queued", "Assigned", "Running":
		result, err := conn.ExecContext(ctx, `
			UPDATE execution_attempts
			SET state='Interrupted', ended_at=?, termination_reason=?, row_version=row_version+1
			WHERE id=? AND state=?`, now, reason, attemptID, state)
		if err != nil {
			return "", err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return "", fmt.Errorf("attempt %d interruption lost the race", attemptID)
		}
		return "Interrupted", nil
	default:
		return "", fmt.Errorf("attempt %d has unknown state %q", attemptID, state)
	}
}

// ActiveOfSlot lists the active attempts bound to one runtime slot with
// their dispatch binding, so the reconnect adjudication can split them by
// boot and lease (RUNTIME-TASK-005/006).
func (service *Service) ActiveOfSlot(ctx context.Context, slot string) ([]View, error) {
	rows, err := service.db.QueryContext(ctx, `
		SELECT id, attempt_type, scope_type, scope_id, state, row_version, runtime_slot,
		       boot_id, connection_epoch, started_at, ended_at, termination_reason, created_at, lease_until
		FROM execution_attempts
		WHERE runtime_slot=? AND state IN ('Assigned','Running','Cancelling')
		ORDER BY id`, slot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var views []View
	for rows.Next() {
		var view View
		var slotName, boot sql.NullString
		var epoch sql.NullInt64
		var started, ended, reason, lease sql.NullString
		if err := rows.Scan(&view.ID, &view.AttemptType, &view.ScopeType, &view.ScopeID, &view.State, &view.RowVersion,
			&slotName, &boot, &epoch, &started, &ended, &reason, &view.CreatedAt, &lease); err != nil {
			return nil, err
		}
		if slotName.Valid {
			view.RuntimeSlot = &slotName.String
		}
		if boot.Valid {
			view.BootID = &boot.String
		}
		if epoch.Valid {
			view.ConnectionEpoch = &epoch.Int64
		}
		if started.Valid {
			view.StartedAt = &started.String
		}
		if ended.Valid {
			view.EndedAt = &ended.String
		}
		if reason.Valid {
			view.TerminationReason = &reason.String
		}
		if lease.Valid {
			view.LeaseUntil = &lease.String
		}
		views = append(views, view)
	}
	return views, rows.Err()
}

// RenewLeaseForBoot extends the lease of every still-leased active attempt
// bound to (slot, boot) (RUNTIME-TASK-007): heartbeats and reconcile reports
// arrive on the accepted stream, so the envelope fence already proved the
// caller is the current connection. Attempts whose lease already burned
// down are NOT resurrected — the sweeper owns expired rows. Each renewal
// bumps row_version exactly once.
func (service *Service) RenewLeaseForBoot(ctx context.Context, slot, bootID string, window time.Duration) error {
	deadline := service.now().Add(window).Format(time.RFC3339Nano)
	_, err := service.db.ExecContext(ctx, `
		UPDATE execution_attempts
		SET lease_until=?, row_version=row_version+1
		WHERE runtime_slot=? AND boot_id=? AND state IN ('Assigned','Running','Cancelling') AND lease_until > ?`,
		deadline, slot, bootID, service.nowText())
	return err
}

// Swept is one lease-sweep outcome for the caller's scope routing.
type Swept struct {
	AttemptID int64
	Type      string
	ScopeType string
	ScopeID   int64
	Final     string // Interrupted | Cancelled
	// DeferredLoss means this row owns browser cleanup (or is the investigation
	// parent itself). Runtime must first create the durable recovery-loss work
	// item; a generic lease update may never bypass that trace closure.
	DeferredLoss           bool
	BrowserParentAttemptID int64
}

// SweepExpired converges every active attempt whose lease has burned down
// without renewal (RUNTIME-TASK-006): Assigned/Running → Interrupted
// (lease_expired), Cancelling → Cancelled. The caller routes each outcome to
// the owning scope aggregate. Queued attempts carry no lease and are never
// swept.
func (service *Service) SweepExpired(ctx context.Context) ([]Swept, error) {
	now := service.nowText()
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	rows, err := conn.QueryContext(ctx, `
		SELECT a.id, a.attempt_type, a.scope_type, a.scope_id, a.state,
		       CASE WHEN a.attempt_type='investigation' OR EXISTS (
		         SELECT 1 FROM browser_exploration_child_bindings b
		         JOIN browser_operations o ON o.id=b.operation_id
		         WHERE b.child_attempt_id=a.id AND o.kind='exploration'
		           AND (o.state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect') OR o.stop_confirmed_at IS NULL)
		       ) THEN 1 ELSE 0 END,
		       COALESCE((SELECT b.parent_attempt_id FROM browser_exploration_child_bindings b
		         JOIN browser_operations o ON o.id=b.operation_id
		         WHERE b.child_attempt_id=a.id AND o.kind='exploration'
		         ORDER BY b.operation_id DESC LIMIT 1),0)
		FROM execution_attempts a
		WHERE a.state IN ('Assigned','Running','Cancelling') AND a.lease_until <= ? ORDER BY a.id`, now)
	if err != nil {
		return nil, err
	}
	var swept []Swept
	var ids []int64
	var states []string
	for rows.Next() {
		var item Swept
		var state string
		var deferred int
		if err := rows.Scan(&item.AttemptID, &item.Type, &item.ScopeType, &item.ScopeID, &state, &deferred, &item.BrowserParentAttemptID); err != nil {
			rows.Close()
			return nil, err
		}
		// A cancellation fence does not waive the mandatory exploration trace.
		// Route every browser obligation, including Cancelling children/parents,
		// through the parent closure state machine; only its trace action may
		// eventually acknowledge the terminal attempt.
		item.DeferredLoss = deferred != 0
		ids = append(ids, item.AttemptID)
		states = append(states, state)
		swept = append(swept, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index, id := range ids {
		if swept[index].DeferredLoss {
			// This is deliberately a typed candidate, not a terminal transition.
			// The caller creates recovery_loss before it observes/dispatches browser
			// cleanup, eliminating the lease-sweep trace-loss race.
			continue
		}
		if states[index] == "Cancelling" {
			result, err := conn.ExecContext(ctx, `
				UPDATE execution_attempts
				SET state='Cancelled', ended_at=?, termination_reason='cancelled', row_version=row_version+1
				WHERE id=? AND state='Cancelling'`, now, id)
			if err != nil {
				return nil, err
			}
			if affected, _ := result.RowsAffected(); affected != 1 {
				return nil, fmt.Errorf("sweep lost the cancel race on attempt %d", id)
			}
			swept[index].Final = "Cancelled"
			continue
		}
		result, err := conn.ExecContext(ctx, `
			UPDATE execution_attempts
			SET state='Interrupted', ended_at=?, termination_reason='lease_expired', row_version=row_version+1
			WHERE id=? AND state=?`, now, id, states[index])
		if err != nil {
			return nil, err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return nil, fmt.Errorf("sweep lost the interrupt race on attempt %d", id)
		}
		swept[index].Final = "Interrupted"
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return nil, err
	}
	committed = true
	return swept, nil
}
