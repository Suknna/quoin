package businesssystem

import (
	"context"
	"database/sql"
	"fmt"
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
func (service *Service) RecordResourceRefreshTechnicalGap(ctx context.Context, attemptID int64, reason string) error {
	conn, err := service.db.Conn(ctx)
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
	var runID, systemID int64
	var discoveryKey string
	if err = conn.QueryRowContext(ctx, `SELECT a.scope_id,a.discovery_key,r.business_system_id FROM execution_attempts a JOIN resource_refresh_runs r ON r.id=a.scope_id WHERE a.id=? AND a.scope_type='resource_refresh_run' AND a.attempt_type='inspection_collection'`, attemptID).Scan(&runID, &discoveryKey, &systemID); err != nil {
		return err
	}
	var exists int
	if err = conn.QueryRowContext(ctx, `SELECT 1 FROM observed_refresh_log WHERE resource_refresh_run_id=? AND discovery_key=?`, runID, discoveryKey).Scan(&exists); err == nil {
		if _, err = conn.ExecContext(ctx, `COMMIT`); err != nil {
			return err
		}
		committed = true
		return nil
	} else if err != sql.ErrNoRows {
		return err
	}
	now := service.nowText()
	if _, err = conn.ExecContext(ctx, `INSERT INTO observed_refresh_log(resource_refresh_run_id,attempt_id,business_system_id,discovery_key,started_at,completed_at,complete,error_detail) VALUES(?,?,?,?,?,?,0,?)`, runID, attemptID, systemID, discoveryKey, now, now, fmt.Sprintf("technical gap: %s", reason)); err != nil {
		return err
	}
	var pending int
	if err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_attempts WHERE scope_type='resource_refresh_run' AND scope_id=? AND state IN ('Queued','Assigned','Running','Cancelling')`, runID).Scan(&pending); err != nil {
		return err
	}
	if pending == 0 {
		state := "Interrupted"
		if reason == "cancelled" {
			state = "Cancelled"
		}
		if _, err = conn.ExecContext(ctx, `UPDATE resource_refresh_runs SET state=?,result_detail=?,row_version=row_version+1 WHERE id=? AND state='Running'`, state, fmt.Sprintf("%s: %s", state, reason), runID); err != nil {
			return err
		}
	}
	if _, err = conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}
