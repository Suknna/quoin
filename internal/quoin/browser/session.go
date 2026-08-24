package browser

import (
	"context"
	"database/sql"
	"time"
)

// RevokeSession terminates that session's active manual logins. It is invoked
// by the session authority before the cookie is revoked, so a stale browser
// transport cannot outlive its authenticated principal.
// CloseRevokedSessions joins the session authority rather than trying to
// duplicate all password/reset/admin revocation paths in the browser layer.
func (service *Service) CloseRevokedSessions(ctx context.Context) ([]int64, error) {
	rows, err := service.db.QueryContext(ctx, `SELECT DISTINCT o.actor_session_id FROM browser_operations o JOIN sessions s ON s.id=o.actor_session_id WHERE o.kind='manual_login' AND s.revoked_at IS NOT NULL AND o.state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		sessions = append(sessions, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var operations []int64
	for _, sessionID := range sessions {
		ids, closeErr := service.RevokeSession(ctx, sessionID)
		if closeErr != nil {
			return nil, closeErr
		}
		operations = append(operations, ids...)
	}
	return operations, nil
}

func (service *Service) RevokeSession(ctx context.Context, sessionID int64) ([]int64, error) {
	if sessionID < 1 {
		return nil, ErrInvalid
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	now := service.now().UTC().Format(time.RFC3339Nano)
	rows, err := conn.QueryContext(ctx, `SELECT id,start_dispatched_at IS NOT NULL FROM browser_operations WHERE kind='manual_login' AND actor_session_id=? AND state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect')`, sessionID)
	if err != nil {
		return nil, err
	}
	type operation struct {
		id         int64
		dispatched bool
	}
	var operations []operation
	for rows.Next() {
		var item operation
		if err := rows.Scan(&item.id, &item.dispatched); err != nil {
			rows.Close()
			return nil, err
		}
		operations = append(operations, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(operations))
	for _, operation := range operations {
		var result sql.Result
		if operation.dispatched {
			result, err = conn.ExecContext(ctx, `UPDATE browser_operations SET state='Cancelled',ended_at=?,terminal_reason='session_revoked',row_version=row_version+1 WHERE id=? AND state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect')`, now, operation.id)
		} else {
			result, err = conn.ExecContext(ctx, `UPDATE browser_operations SET state='Cancelled',ended_at=?,terminal_reason='session_revoked',stop_confirmed_at=?,stop_confirmation_basis='not_dispatched',row_version=row_version+1 WHERE id=? AND state IN ('Queued','WaitingForCapacity')`, now, now, operation.id)
		}
		if err != nil {
			return nil, err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return nil, ErrConflict
		}
		ids = append(ids, operation.id)
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return ids, nil
}
