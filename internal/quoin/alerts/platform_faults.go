package alerts

import (
	"context"
	"database/sql"
	"time"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// PlatformFaultReporter projects real internal component disconnect and
// execution-outcome facts into the unified alert read model. It deliberately
// owns neither Alertmanager Deliveries nor business attribution: those
// authorities remain upstream-only.
//
// Ownership (ADR-0006): the standalone observations run through the shared
// execution runner — one runner-owned IMMEDIATE transaction carrying the
// machine-entry authorization, the lifecycle write and the automatic audit
// event; no transaction control and no hand-written audit remain here.
// ObserveExecutionOutcomeOn stays deliberately transaction-composable: when
// the authoritative attempt lifecycle already owns a runner transaction (the
// analysis terminal projection), the fault write joins that outer transaction
// instead of nesting a second one, keeping platform state and attempt state
// all-or-nothing across a process crash.
type PlatformFaultReporter struct {
	service *Service
	now     func() time.Time
}

func NewPlatformFaultReporter(service *Service) *PlatformFaultReporter {
	return &PlatformFaultReporter{service: service, now: time.Now}
}

// ObserveRuntimeConnection converges one runtime slot's connection fact. A
// disconnect opens or repeats one durable fault; a later reconnect resolves the
// current lifecycle. SQLite's open identity index prevents duplicate faults.
func (reporter *PlatformFaultReporter) ObserveRuntimeConnection(ctx context.Context, component string, connected bool) error {
	if component != "plinth" {
		return nil
	}
	ctx, err := reporter.service.machineScope(ctx)
	if err != nil {
		return err
	}
	_, err = execution.Execute(ctx, reporter.service.runner, reporter.service.ops.faultLive,
		func(tx *execution.Tx) (int64, error) {
			return reporter.observeRuntimeConnectionOn(ctx, tx, component, connected)
		},
		func(faultID int64) int64 { return faultID })
	return err
}

// observeRuntimeConnectionOn is the transaction business stage behind
// ObserveRuntimeConnection; it returns the touched fault's id (0 when the
// observation changed nothing).
func (reporter *PlatformFaultReporter) observeRuntimeConnectionOn(ctx context.Context, tx execution.Executor, component string, connected bool) (int64, error) {
	const reason = "runtime_control_stream_disconnected"
	now := reporter.now().UTC().Format(time.RFC3339Nano)
	if connected {
		// A reconnect converges any current lifecycle to Resolved and never
		// originates a new fault.
		return resolveRuntimeFaultOn(ctx, tx, component, reason, now)
	}
	return openOrRepeatFaultOn(ctx, tx, component, reason, now)
}

// resolveRuntimeFaultOn converges one component's firing lifecycle to
// Resolved and returns the touched fault id.
func resolveRuntimeFaultOn(ctx context.Context, tx execution.Executor, component, reason, now string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM platform_faults WHERE component=? AND reason=? AND state='Firing'`, component, reason).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE platform_faults
		SET state='Resolved', resolved_at=?, last_seen_at=?, row_version=row_version+1
		WHERE id=? AND state='Firing'`, now, now, id); err != nil {
		return 0, err
	}
	return id, nil
}

// openOrRepeatFaultOn opens one firing lifecycle or advances the existing
// one's diagnostics; the open-identity select makes repeats idempotent.
func openOrRepeatFaultOn(ctx context.Context, tx execution.Executor, component, reason, now string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM platform_faults WHERE component=? AND reason=? AND state='Firing'`, component, reason).Scan(&id)
	switch {
	case err == nil:
		_, err = tx.ExecContext(ctx, `UPDATE platform_faults SET last_seen_at=? WHERE id=?`, now, id)
		return id, err
	case err == sql.ErrNoRows:
		result, insertErr := tx.ExecContext(ctx, `INSERT INTO platform_faults(component,reason,state,first_seen_at,last_seen_at) VALUES(?,?,'Firing',?,?)`, component, reason, now, now)
		if insertErr != nil {
			return 0, insertErr
		}
		id, insertErr = result.LastInsertId()
		return id, insertErr
	default:
		return 0, err
	}
}

func isExecutionFaultReason(reason string) bool {
	return reason == "worker_protocol_error"
}

// ObserveExecutionOutcome projects the one reachable, runtime-owned Plinth
// failure through the shared runner. Its caller supplies an authoritative
// commit sequence, not an attempt ID: concurrently created attempts may
// complete in the opposite order. A later committed success is positive
// evidence for worker recovery, while a heartbeat is intentionally not
// accepted here. Inputs that can never be platform facts return before any
// execution, so they produce neither state change nor audit event.
func (reporter *PlatformFaultReporter) ObserveExecutionOutcome(ctx context.Context, commitSequence int64, succeeded bool, terminationReason string) error {
	if commitSequence <= 0 || (!succeeded && !isExecutionFaultReason(terminationReason)) {
		return nil
	}
	ctx, err := reporter.service.machineScope(ctx)
	if err != nil {
		return err
	}
	_, err = execution.Execute(ctx, reporter.service.runner, reporter.service.ops.faultExec,
		func(tx *execution.Tx) (int64, error) {
			if err := reporter.ObserveExecutionOutcomeOn(ctx, tx, commitSequence, succeeded, terminationReason); err != nil {
				return 0, err
			}
			var faultID int64
			err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM platform_faults WHERE component='plinth' AND reason='worker_protocol_error'`).Scan(&faultID)
			return faultID, err
		},
		func(faultID int64) int64 { return faultID })
	return err
}

// Transaction is the structural execution-transaction surface the projector
// composes on: the shared execution runner's guarded *execution.Tx and a
// plain *sql.Conn both satisfy it without either package importing the
// other (ADR-0006 lifecycle projection).
type Transaction interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ObserveExecutionOutcomeOn is the transaction-composable form used by the
// authoritative attempt lifecycle. The terminal audit sequence is assigned
// under the lifecycle's transaction, making platform state and attempt
// state all-or-nothing across a process crash.
func (reporter *PlatformFaultReporter) ObserveExecutionOutcomeOn(ctx context.Context, tx Transaction, commitSequence int64, succeeded bool, terminationReason string) error {
	if commitSequence <= 0 || (!succeeded && !isExecutionFaultReason(terminationReason)) {
		return nil
	}
	var id, lastCommitSequence int64
	var state string
	err := tx.QueryRowContext(ctx, `SELECT id,state,last_execution_commit_sequence
		FROM platform_faults WHERE component='plinth' AND reason='worker_protocol_error' ORDER BY id DESC LIMIT 1`).
		Scan(&id, &state, &lastCommitSequence)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil && commitSequence <= lastCommitSequence {
		return nil
	}
	now := reporter.now().UTC().Format(time.RFC3339Nano)
	if succeeded {
		if err == sql.ErrNoRows {
			return nil
		}
		if state == "Firing" {
			_, err = tx.ExecContext(ctx, `UPDATE platform_faults
				SET state='Resolved', resolved_at=?, last_seen_at=?, last_execution_commit_sequence=?, row_version=row_version+1 WHERE id=?`, now, now, commitSequence, id)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE platform_faults SET last_execution_commit_sequence=? WHERE id=?`, commitSequence, id)
		}
		return err
	}
	if err == sql.ErrNoRows || state == "Resolved" {
		_, err = tx.ExecContext(ctx, `INSERT INTO platform_faults(component,reason,state,first_seen_at,last_seen_at,last_execution_commit_sequence)
			VALUES('plinth','worker_protocol_error','Firing',?,?,?)`, now, now, commitSequence)
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE platform_faults SET last_seen_at=?, last_execution_commit_sequence=? WHERE id=?`, now, commitSequence, id)
	return err
}
