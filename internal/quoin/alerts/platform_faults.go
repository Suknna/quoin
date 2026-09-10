package alerts

import (
	"context"
	"database/sql"
	"time"
)

// PlatformFaultReporter projects real internal component disconnect and
// execution-outcome facts into the unified alert read model. It deliberately
// owns neither Alertmanager Deliveries nor business attribution: those
// authorities remain upstream-only.
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
	if component != "plinth" && component != "lintel" {
		return nil
	}
	const reason = "runtime_control_stream_disconnected"
	now := reporter.now().UTC().Format(time.RFC3339Nano)
	if connected {
		_, err := reporter.service.db.ExecContext(ctx, `UPDATE platform_faults
			SET state='Resolved', resolved_at=?, last_seen_at=?, row_version=row_version+1
			WHERE component=? AND reason=? AND state='Firing'`, now, now, component, reason)
		return err
	}
	return reporter.observeFault(ctx, component, reason, now)
}

// ObserveExecutionOutcome projects the one reachable, runtime-owned Plinth
// failure. Its caller supplies an authoritative commit sequence, not an attempt
// ID: concurrently created attempts may complete in the opposite order. A
// later committed success is positive evidence for worker recovery, while a
// heartbeat is intentionally not accepted here.
func (reporter *PlatformFaultReporter) ObserveExecutionOutcome(ctx context.Context, commitSequence int64, succeeded bool, terminationReason string) error {
	if commitSequence <= 0 || (!succeeded && !isExecutionFaultReason(terminationReason)) {
		return nil
	}
	conn, err := reporter.service.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if err = reporter.ObserveExecutionOutcomeOn(ctx, conn, commitSequence, succeeded, terminationReason); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

func isExecutionFaultReason(reason string) bool {
	return reason == "worker_protocol_error"
}

// ObserveExecutionOutcomeOn is the transaction-composable form used by the
// authoritative attempt lifecycle. The terminal audit sequence is assigned
// under the lifecycle's BEGIN IMMEDIATE transaction, making platform state and
// attempt state all-or-nothing across a process crash.
func (reporter *PlatformFaultReporter) ObserveExecutionOutcomeOn(ctx context.Context, conn *sql.Conn, commitSequence int64, succeeded bool, terminationReason string) error {
	if commitSequence <= 0 || (!succeeded && !isExecutionFaultReason(terminationReason)) {
		return nil
	}
	var id, lastCommitSequence int64
	var state string
	err := conn.QueryRowContext(ctx, `SELECT id,state,last_execution_commit_sequence
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
			_, err = conn.ExecContext(ctx, `UPDATE platform_faults
				SET state='Resolved', resolved_at=?, last_seen_at=?, last_execution_commit_sequence=?, row_version=row_version+1 WHERE id=?`, now, now, commitSequence, id)
		} else {
			_, err = conn.ExecContext(ctx, `UPDATE platform_faults SET last_execution_commit_sequence=? WHERE id=?`, commitSequence, id)
		}
		return err
	}
	if err == sql.ErrNoRows || state == "Resolved" {
		_, err = conn.ExecContext(ctx, `INSERT INTO platform_faults(component,reason,state,first_seen_at,last_seen_at,last_execution_commit_sequence)
			VALUES('plinth','worker_protocol_error','Firing',?,?,?)`, now, now, commitSequence)
		return err
	}
	_, err = conn.ExecContext(ctx, `UPDATE platform_faults SET last_seen_at=?, last_execution_commit_sequence=? WHERE id=?`, now, commitSequence, id)
	return err
}

func (reporter *PlatformFaultReporter) observeFault(ctx context.Context, component, reason, now string) error {
	conn, err := reporter.service.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var id int64
	err = conn.QueryRowContext(ctx, `SELECT id FROM platform_faults WHERE component=? AND reason=? AND state='Firing'`, component, reason).Scan(&id)
	switch err {
	case nil:
		_, err = conn.ExecContext(ctx, `UPDATE platform_faults SET last_seen_at=? WHERE id=?`, now, id)
	case sql.ErrNoRows:
		_, err = conn.ExecContext(ctx, `INSERT INTO platform_faults(component,reason,state,first_seen_at,last_seen_at) VALUES(?,?,'Firing',?,?)`, component, reason, now, now)
	}
	if err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}
