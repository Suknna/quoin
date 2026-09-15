package businesssystem

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// ConvergeResourceRefreshCancelAck closes a resource-refresh child after a
// live CancelAck; every other scope is a no-op so the runtime can route all
// inspection_collection CancelAcks through it.
func (service *Service) ConvergeResourceRefreshCancelAck(ctx context.Context, attemptID int64) error {
	var scope string
	if err := service.db.QueryRowContext(ctx, `SELECT scope_type FROM execution_attempts WHERE id=?`, attemptID).Scan(&scope); err != nil {
		return err
	}
	if scope != "resource_refresh_run" {
		return nil
	}
	return service.RecordResourceRefreshTechnicalGap(ctx, attemptID, "cancelled")
}

// RecordResourceRefreshTechnicalGap closes a child that was fenced outside the
// normal ResultProposal path. The attempt transition has already happened;
// this transaction appends its immutable refresh log and converges the parent.
// RecordResourceRefreshTechnicalGap closes a child that was fenced outside
// the normal ResultProposal path through the shared execution runner
// (ADR-0006): the runner owns the transaction and records the automatic audit
// row. An already-recorded gap is an ErrNoTransition replay and records
// nothing.
func (service *Service) RecordResourceRefreshTechnicalGap(ctx context.Context, attemptID int64, reason string) error {
	scope, err := service.resultContext(ctx, attemptID)
	if err != nil {
		return err
	}
	_, err = execution.Execute(scope, service.runner, service.opRefreshGap,
		func(conn *execution.Tx) (struct{}, error) {
			return service.recordResourceRefreshGapOn(scope, conn, attemptID, reason)
		},
		func(struct{}) int64 { return 0 })
	if errors.Is(err, execution.ErrNoTransition) {
		// The gap row already exists: an idempotent replay records nothing.
		return nil
	}
	return err
}

// recordResourceRefreshGapOn is RecordResourceRefreshTechnicalGap's business
// stage on the runner-owned transaction.
func (service *Service) recordResourceRefreshGapOn(ctx context.Context, conn execution.Executor, attemptID int64, reason string) (struct{}, error) {
	var runID, systemID int64
	var discoveryKey string
	if err := conn.QueryRowContext(ctx, `SELECT a.scope_id,a.discovery_key,r.business_system_id FROM execution_attempts a JOIN resource_refresh_runs r ON r.id=a.scope_id WHERE a.id=? AND a.scope_type='resource_refresh_run' AND a.attempt_type='inspection_collection'`, attemptID).Scan(&runID, &discoveryKey, &systemID); err != nil {
		return struct{}{}, err
	}
	var exists int
	if err := conn.QueryRowContext(ctx, `SELECT 1 FROM observed_refresh_log WHERE resource_refresh_run_id=? AND discovery_key=?`, runID, discoveryKey).Scan(&exists); err == nil {
		return struct{}{}, fmt.Errorf("%w: resource refresh gap already recorded", execution.ErrNoTransition)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return struct{}{}, err
	}
	now := service.nowText()
	if _, err := conn.ExecContext(ctx, `INSERT INTO observed_refresh_log(resource_refresh_run_id,attempt_id,business_system_id,discovery_key,started_at,completed_at,complete,error_detail) VALUES(?,?,?,?,?,?,0,?)`, runID, attemptID, systemID, discoveryKey, now, now, fmt.Sprintf("technical gap: %s", reason)); err != nil {
		return struct{}{}, err
	}
	var pending int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_attempts WHERE scope_type='resource_refresh_run' AND scope_id=? AND state IN ('Queued','Assigned','Running','Cancelling')`, runID).Scan(&pending); err != nil {
		return struct{}{}, err
	}
	if pending == 0 {
		state := "Interrupted"
		if reason == "cancelled" {
			state = "Cancelled"
		}
		if _, err := conn.ExecContext(ctx, `UPDATE resource_refresh_runs SET state=?,result_detail=?,row_version=row_version+1 WHERE id=? AND state='Running'`, state, fmt.Sprintf("%s: %s", state, reason), runID); err != nil {
			return struct{}{}, err
		}
	}
	return struct{}{}, nil
}
