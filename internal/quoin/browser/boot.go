package browser

import (
	"context"
	"time"
)

// InterruptOldBootOperations records the semantic end of browser work whose
// physical start was bound to an older Lintel boot. It deliberately leaves the
// physical cleanup fence open: only the successor boot's explicit cleanup
// acknowledgement can make the identity reusable.
func (service *Service) InterruptOldBootOperations(ctx context.Context, bootID string, epoch uint64) ([]int64, error) {
	if bootID == "" || epoch == 0 {
		return nil, ErrInvalid
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	rows, err := conn.QueryContext(ctx, `SELECT id FROM browser_operations
		WHERE state IN ('Starting','Running','AwaitingReconnect')
		  AND start_dispatched_at IS NOT NULL
		  AND lintel_boot_id <> ?`, bootID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(ids) != 0 {
		now := service.now().UTC().Format(time.RFC3339Nano)
		if _, err := conn.ExecContext(ctx, `UPDATE browser_operations
			SET state='Interrupted', ended_at=?, terminal_reason='new_boot', row_version=row_version+1
			WHERE state IN ('Starting','Running','AwaitingReconnect')
			  AND start_dispatched_at IS NOT NULL
			  AND lintel_boot_id <> ?`, now, bootID); err != nil {
			return nil, err
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return ids, nil
}
