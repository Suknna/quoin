package browser

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ReconnectGrace is a release-frozen transport grace period. It is deliberately
// not deployment configuration: browser attachment recovery is a protocol
// behavior, not an operator-tunable product setting.
const ReconnectGrace = 30 * time.Second

// AwaitReconnect records that the only noVNC attachment disappeared without
// publishing or cancelling the active manual login. It is idempotent so both
// WebSocket and BrowserTunnel teardown may converge on the same operation.
func (service *Service) AwaitReconnect(ctx context.Context, operationID int64) (time.Time, error) {
	if operationID < 1 {
		return time.Time{}, ErrInvalid
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return time.Time{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	deadline := service.now().UTC().Add(ReconnectGrace)
	result, err := conn.ExecContext(ctx, `UPDATE browser_operations SET state='AwaitingReconnect',reconnect_deadline=?,row_version=row_version+1 WHERE id=? AND kind='manual_login' AND state='Running'`, deadline.Format(time.RFC3339Nano), operationID)
	if err != nil {
		return time.Time{}, err
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		var state string
		var existing sql.NullString
		if err := conn.QueryRowContext(ctx, `SELECT state,reconnect_deadline FROM browser_operations WHERE id=? AND kind='manual_login'`, operationID).Scan(&state, &existing); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return time.Time{}, ErrNotFound
			}
			return time.Time{}, err
		}
		if state != "AwaitingReconnect" || !existing.Valid {
			return time.Time{}, ErrConflict
		}
		deadline, err = time.Parse(time.RFC3339Nano, existing.String)
		if err != nil {
			return time.Time{}, err
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return time.Time{}, err
	}
	committed = true
	return deadline, nil
}

// ResumeReconnect consumes the bounded reattachment window before opaque RFB
// forwarding resumes. The caller has already reserved the unique in-memory
// attachment; this transaction remains the durable lifecycle authority.
func (service *Service) ResumeReconnect(ctx context.Context, operationID int64) error {
	now := service.now().UTC().Format(time.RFC3339Nano)
	result, err := service.db.ExecContext(ctx, `UPDATE browser_operations SET state='Running',reconnect_deadline=NULL,row_version=row_version+1 WHERE id=? AND kind='manual_login' AND state='AwaitingReconnect' AND reconnect_deadline>?`, operationID, now)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrConflict
	}
	return nil
}

// ExpireReconnect atomically turns an elapsed grace period into an operation
// terminal fence. The caller must then close the transport and request Stop.
func (service *Service) ExpireReconnect(ctx context.Context, operationID int64) (bool, error) {
	now := service.now().UTC().Format(time.RFC3339Nano)
	result, err := service.db.ExecContext(ctx, `UPDATE browser_operations SET state='Cancelled',ended_at=?,reconnect_deadline=NULL,terminal_reason='grace_expired',row_version=row_version+1 WHERE id=? AND kind='manual_login' AND state='AwaitingReconnect' AND reconnect_deadline<=?`, now, operationID, now)
	if err != nil {
		return false, err
	}
	changed, _ := result.RowsAffected()
	return changed == 1, nil
}
