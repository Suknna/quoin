package browser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"google.golang.org/protobuf/proto"
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
		// Close active action facts before the Operation moves terminal. The parent
		// Tool Call remains the sole writer of the browser child Attempt terminal
		// state through trg_tool_calls_close_browser_child.
		if err := sealInterruptedExplorationActions(ctx, conn, ids, now, "browser exploration interrupted by a new Lintel boot"); err != nil {
			return nil, err
		}
		// A claimed complete trace without a delivered terminal result cannot be
		// treated as complete after this boot loss. Lintel seals complete traces
		// only after clean Stop, so this durable downgrade is the crash-before-upload
		// recovery branch and preserves the single-trace terminal classification.
		if err := downgradeInterruptedExplorationClaims(ctx, conn, ids, now); err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE browser_operations
			SET state='Interrupted', ended_at=?, terminal_reason='new_boot', row_version=row_version+1
			WHERE state IN ('Starting','Running','AwaitingReconnect')
			  AND start_dispatched_at IS NOT NULL
			  AND lintel_boot_id <> ?`, now, bootID); err != nil {
			return nil, err
		}
		// An interruption can occur before the first action was audited. The
		// durable child binding covers both that gap and normal active actions.
		if _, err := conn.ExecContext(ctx, `UPDATE tool_calls
			SET status='failed', ended_at=?, result_json=json_object(
				'outcome','session_closed',
				'action',COALESCE((SELECT action_kind FROM browser_exploration_actions a WHERE a.tool_call_id=tool_calls.id ORDER BY a.action_seq DESC LIMIT 1),'open'),
				'error',json_object('code','ParentTerminated','message','browser exploration interrupted by a new Lintel boot','retryableInSession',json('false')),
				'sessionId',CAST((SELECT operation_id FROM browser_exploration_child_bindings b WHERE b.tool_call_id=tool_calls.id) AS TEXT)
			), error_detail='browser exploration interrupted by a new Lintel boot', row_version=row_version+1
			WHERE status='running' AND execution_mode='quoin_browser' AND id IN (
				SELECT b.tool_call_id FROM browser_exploration_child_bindings b
				JOIN browser_operations o ON o.id=b.operation_id
				WHERE o.state='Interrupted' AND o.terminal_reason='new_boot' AND o.ended_at=?
			)`, now, now); err != nil {
			return nil, err
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return ids, nil
}

// downgradeInterruptedExplorationClaims converts only a claim that has not
// produced a durable trace. If trace installation committed before Lintel died,
// its artifact ID and digest are atomically bound to the claim by artifact.Store;
// preserve and attach that evidence to the interrupted Operation rather than
// lying that the immutable complete trace was incomplete or leaving it orphaned.
func downgradeInterruptedExplorationClaims(ctx context.Context, conn *sql.Conn, operationIDs []int64, now string) error {
	for _, operationID := range operationIDs {
		if _, err := conn.ExecContext(ctx, `UPDATE browser_operations
			SET trace_artifact_id=(SELECT trace_artifact_id FROM browser_exploration_terminal_claims
				WHERE operation_id=? AND state='artifact_committed_complete'),trace_integrity='complete',row_version=row_version+1
			WHERE id=? AND trace_artifact_id IS NULL AND EXISTS(
				SELECT 1 FROM browser_exploration_terminal_claims
				WHERE operation_id=? AND state='artifact_committed_complete')`, operationID, operationID, operationID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE browser_exploration_terminal_claims
			SET state='downgraded_incomplete',finalized_at=?
			WHERE operation_id=? AND state='claimed_complete'`, now, operationID); err != nil {
			return err
		}
	}
	return nil
}

// InterruptForQuoinRestart closes every active operation when Quoin has lost
// its in-memory control-plane lifetime. The same Lintel boot may still own a
// Chromium process, but no Browser Operation is recoverable across a Quoin
// restart; a later Stop fence performs the physical cleanup before identity
// reuse. The durable reason is shutdown, not new_boot, because Lintel itself
// did not restart.
// InterruptMissingPhysicalOperations converges the other half of Lintel's
// complete Hello/Heartbeat snapshot. If a durable active operation bound to the
// reporting boot is absent from that snapshot, Lintel has authoritatively lost
// its physical process and any staged trace. This is not an unknown Start
// outcome: complete snapshots include Manager.starting and trace staging.
func (service *Service) InterruptMissingPhysicalOperations(ctx context.Context, bootID string, epoch uint64, reported []int64) ([]int64, error) {
	if bootID == "" || epoch == 0 {
		return nil, ErrInvalid
	}
	present := make(map[int64]struct{}, len(reported))
	for _, id := range reported {
		if id > 0 {
			present[id] = struct{}{}
		}
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
	rows, err := conn.QueryContext(ctx, `SELECT id FROM browser_operations
		WHERE state IN ('Starting','Running','AwaitingReconnect')
		  AND start_dispatched_at IS NOT NULL AND lintel_boot_id=?
		  AND lintel_connection_epoch<=?`, bootID, epoch)
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
		if _, ok := present[id]; !ok {
			ids = append(ids, id)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, err
		}
		committed = true
		return nil, nil
	}
	now := service.now().UTC().Format(time.RFC3339Nano)
	if err := sealInterruptedExplorationActions(ctx, conn, ids, now, "browser operation absent from Lintel physical snapshot"); err != nil {
		return nil, err
	}
	if err := downgradeInterruptedExplorationClaims(ctx, conn, ids, now); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := conn.ExecContext(ctx, `UPDATE browser_operations
			SET state='Interrupted',ended_at=?,terminal_reason='browser_crashed',
			    stop_confirmed_at=?,stop_confirmation_basis='inventory_absent',row_version=row_version+1
			WHERE id=? AND state IN ('Starting','Running','AwaitingReconnect')`, now, now, id); err != nil {
			return nil, err
		}
	}
	if _, err := conn.ExecContext(ctx, `UPDATE tool_calls
		SET status='failed',ended_at=?,result_json=json_object(
			'outcome','session_closed',
			'action',COALESCE((SELECT action_kind FROM browser_exploration_actions a WHERE a.tool_call_id=tool_calls.id ORDER BY a.action_seq DESC LIMIT 1),'open'),
			'error',json_object('code','ParentTerminated','message','browser process was absent from Lintel physical snapshot','retryableInSession',json('false')),
			'sessionId',CAST((SELECT operation_id FROM browser_exploration_child_bindings b WHERE b.tool_call_id=tool_calls.id) AS TEXT)
		),error_detail='browser process was absent from Lintel physical snapshot',row_version=row_version+1
		WHERE status='running' AND execution_mode='quoin_browser' AND id IN (
			SELECT b.tool_call_id FROM browser_exploration_child_bindings b
			JOIN browser_operations o ON o.id=b.operation_id
			WHERE o.state='Interrupted' AND o.terminal_reason='browser_crashed' AND o.ended_at=?
		)`, now, now); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return ids, nil
}

func (service *Service) InterruptForQuoinRestart(ctx context.Context, bootID string) ([]int64, error) {
	if bootID == "" {
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
		  AND start_dispatched_at IS NOT NULL AND lintel_boot_id=?`, bootID)
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
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(ids) != 0 {
		now := service.now().UTC().Format(time.RFC3339Nano)
		if err := sealInterruptedExplorationActions(ctx, conn, ids, now, "browser exploration interrupted by Quoin restart"); err != nil {
			return nil, err
		}
		if err := downgradeInterruptedExplorationClaims(ctx, conn, ids, now); err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE browser_operations
			SET state='Interrupted', ended_at=?, terminal_reason='shutdown', row_version=row_version+1
			WHERE state IN ('Starting','Running','AwaitingReconnect')
			  AND start_dispatched_at IS NOT NULL AND lintel_boot_id=?`, now, bootID); err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE tool_calls
			SET status='failed', ended_at=?, result_json=json_object(
				'outcome','session_closed',
				'action',COALESCE((SELECT action_kind FROM browser_exploration_actions a WHERE a.tool_call_id=tool_calls.id ORDER BY a.action_seq DESC LIMIT 1),'open'),
				'error',json_object('code','ParentTerminated','message','browser exploration interrupted by Quoin restart','retryableInSession',json('false')),
				'sessionId',CAST((SELECT operation_id FROM browser_exploration_child_bindings b WHERE b.tool_call_id=tool_calls.id) AS TEXT)
			), error_detail='browser exploration interrupted by Quoin restart', row_version=row_version+1
			WHERE status='running' AND execution_mode='quoin_browser' AND id IN (
				SELECT b.tool_call_id FROM browser_exploration_child_bindings b
				JOIN browser_operations o ON o.id=b.operation_id
				WHERE o.state='Interrupted' AND o.terminal_reason='shutdown' AND o.ended_at=?
			)`, now, now); err != nil {
			return nil, err
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return ids, nil
}

// sealInterruptedExplorationActions closes each in-flight action with the same
// deterministic result digest that the normal Lintel result path uses. A zero
// placeholder is not an admissible replay identity: the action ledger requires
// a digest for every terminal fact, including a Quoin- or boot-owned closure.
func sealInterruptedExplorationActions(ctx context.Context, conn *sql.Conn, operationIDs []int64, endedAt, detail string) error {
	for _, operationID := range operationIDs {
		rows, err := conn.QueryContext(ctx, `SELECT a.id,a.child_attempt_id,a.tool_call_id,b.parent_attempt_id
			FROM browser_exploration_actions a
			JOIN browser_exploration_child_bindings b ON b.child_attempt_id=a.child_attempt_id
			WHERE a.operation_id=? AND a.outcome IS NULL`, operationID)
		if err != nil {
			return err
		}
		type action struct{ id, childID, toolCallID, parentID int64 }
		var actions []action
		for rows.Next() {
			var item action
			if err := rows.Scan(&item.id, &item.childID, &item.toolCallID, &item.parentID); err != nil {
				_ = rows.Close()
				return err
			}
			actions = append(actions, item)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, action := range actions {
			result := &runtimev1.BrowserExplorationActionResult{
				OperationId: operationID, ChildAttemptId: action.childID, ParentAttemptId: action.parentID, ToolCallId: action.toolCallID,
				SessionTerminal: true, ErrorCode: "ParentTerminated", ErrorDetail: detail,
			}
			encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(result)
			if err != nil {
				return fmt.Errorf("marshal interrupted browser action %d: %w", action.id, err)
			}
			digest := sha256.Sum256(encoded)
			if _, err := conn.ExecContext(ctx, `UPDATE browser_exploration_actions
				SET outcome='session_closed',error_code='ParentTerminated',ended_at=?,result_digest=?
				WHERE id=? AND outcome IS NULL`, endedAt, digest[:], action.id); err != nil {
				return err
			}
		}
	}
	return nil
}
