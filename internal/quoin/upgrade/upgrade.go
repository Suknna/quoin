// Package upgrade owns the SQL-authoritative Upgrade maintenance aggregate:
// the idempotent prepare command that enters Upgrade maintenance with its
// deterministic drain checklist, the background reconciler that projects
// active work and orchestrates the verified pre-upgrade backup, and the
// first-release schema gate executed by `quoin migrate`.
//
// Checklist item contract (this package is the authority):
//
//   - ActiveAttempt items use object_key `attempt/<attempt_id>`.
//   - ActiveBrowserOperation items use object_key `operation/<operation_id>`.
//   - BackupPreflight uses object_key `pre_upgrade_backup`.
//   - Blocking detail_code is `<state>|<directive>`; Safe detail_code is
//     `drained` (work items) or `backup_verified` (BackupPreflight).
//   - A directive is either `converge` (no user cancel path; the running
//     T12 sweeps or a Runtime reconnect own convergence) or
//     `cancel:<endpointKey>:<path params joined by />:<rowVersion>` naming
//     the one upgrade-drain HTTP cancel command and its expected row
//     version. Maintenance exposes no domain read endpoints, so this
//     projection is the only contract-legal channel for those parameters.
package upgrade

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/maintenance"
)

// Reason mirrors the frozen SQL enum value for the Upgrade maintenance reason.
const Reason = "Upgrade"

const (
	commandPrepare = "upgrade.prepare"

	kindActiveAttempt = "ActiveAttempt"
	kindActiveBrowser = "ActiveBrowserOperation"
	kindBackup        = "BackupPreflight"

	backupObjectKey = "pre_upgrade_backup"

	detailDrained       = "drained"
	detailBackupPending = "backup_pending"
	detailBackupFailed  = "backup_failed"
	detailBackupDone    = "backup_verified"
)

var (
	ErrConflict      = maintenance.ErrConflict
	ErrCommandReused = maintenance.ErrCommandReused
)

// Service owns the prepare command and checklist projection.
type Service struct {
	db  *sql.DB
	now func() time.Time
}

func NewService(db *sql.DB) *Service {
	return &Service{db: db, now: time.Now}
}

// SetClock is the process-boundary seam for deterministic tests.
func (service *Service) SetClock(now func() time.Time) {
	if now != nil {
		service.now = now
	}
}

func (service *Service) timestamp() string {
	return service.now().UTC().Format(time.RFC3339Nano)
}

type PrepareRequest struct {
	ActorID, ExpectedRowVersion int64
	ClientCommandID             string
}

// Prepare is the Admin's idempotent versioned command (HTTP-MAINT-005). The
// first call enters Upgrade maintenance and freezes the deterministic
// checklist in the same transaction; later calls with new command ids
// continue the same revision and re-arm the pre-upgrade backup after a
// failure. There is intentionally no force/skip path.
func (service *Service) Prepare(ctx context.Context, request PrepareRequest) (maintenance.State, error) {
	if request.ActorID < 1 || request.ExpectedRowVersion < 1 || request.ClientCommandID == "" {
		return maintenance.State{}, fmt.Errorf("%w: required request field", ErrConflict)
	}
	digest := auth.DigestCommand(commandPrepare, map[string]any{"expectedReason": Reason, "expectedRowVersion": request.ExpectedRowVersion})
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return maintenance.State{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return maintenance.State{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if prior, found, err := auth.LookupCommandOn(ctx, conn, request.ActorID, request.ClientCommandID); err != nil {
		return maintenance.State{}, err
	} else if found {
		if prior.CommandType != commandPrepare || prior.RequestDigest != digest {
			return maintenance.State{}, ErrCommandReused
		}
		if prior.Outcome == auth.OutcomeCommitted {
			return maintenance.StateOn(ctx, conn)
		}
		return maintenance.State{}, ErrConflict
	}
	var enabled int
	var role string
	if err := conn.QueryRowContext(ctx, `SELECT enabled,role FROM users WHERE id=?`, request.ActorID).Scan(&enabled, &role); err != nil || enabled != 1 || role != "admin" {
		return maintenance.State{}, ErrConflict
	}
	var active int
	var reason string
	var current int64
	if err := conn.QueryRowContext(ctx, `SELECT active,COALESCE(reason,''),row_version FROM maintenance_state WHERE id=1`).Scan(&active, &reason, &current); err != nil {
		return maintenance.State{}, err
	}
	if active == 1 && reason != Reason {
		return maintenance.State{}, ErrConflict
	}
	if current != request.ExpectedRowVersion {
		return maintenance.State{}, ErrConflict
	}
	now := service.timestamp()
	revision := current
	if active == 0 {
		revision = current + 1
		result, err := conn.ExecContext(ctx, `UPDATE maintenance_state SET active=1,reason=?,entered_at=?,entered_by_type='user',entered_by_id=?,exited_at=NULL,exited_by_type=NULL,exited_by_id=NULL,row_version=row_version+1 WHERE id=1 AND active=0 AND row_version=?`, Reason, now, request.ActorID, current)
		if err != nil {
			return maintenance.State{}, err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return maintenance.State{}, ErrConflict
		}
		if err := projectChecklist(ctx, conn, revision, now); err != nil {
			return maintenance.State{}, err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO audit_events(actor_type,actor_id,action,client_command_id,outcome,domain_ref_type,domain_ref_id,created_at) VALUES('user',?,'maintenance.upgrade.prepare',?,'success','maintenance',?,?)`, request.ActorID, request.ClientCommandID, revision, now); err != nil {
			return maintenance.State{}, err
		}
	}
	if err := auth.RecordCommand(ctx, conn, request.ActorID, request.ClientCommandID, commandPrepare, digest, auth.OutcomeCommitted, "maintenance", revision, `{"reason":"Upgrade"}`); err != nil {
		return maintenance.State{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return maintenance.State{}, err
	}
	committed = true
	return maintenance.StateOn(ctx, conn)
}

// The active-work state sets are frozen by the SQL predicates below:
// execution_attempts in Queued/Assigned/Running/Cancelling can still accept
// runtime work or produce durable writes; browser_operations in
// Queued/WaitingForCapacity/Starting/Running/AwaitingReconnect can still
// produce work. The stop/cleanup fence columns belong to the terminal-state
// Lintel closure and cannot un-terminalize an operation.

// projectChecklist freezes the deterministic entry snapshot: one item per
// active attempt, one per active browser operation, plus the always-present
// pre-upgrade backup preflight. Existing rows are never downgraded from Safe
// (attempt and operation lifecycles are forward-only).
func projectChecklist(ctx context.Context, conn *sql.Conn, revision int64, now string) error {
	if _, err := conn.ExecContext(ctx, `INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(maintenance_revision,kind,object_key) DO NOTHING`, revision, kindBackup, backupObjectKey, "Blocking", detailBackupPending, now); err != nil {
		return err
	}
	rows, err := conn.QueryContext(ctx, `
	SELECT a.id, a.scope_type, a.scope_id, a.state, a.requested_by_tool_call_id
	FROM execution_attempts a
	WHERE a.state IN ('Queued','Assigned','Running','Cancelling')
	  AND NOT (a.scope_type='run_check' AND EXISTS (SELECT 1 FROM inspection_check_results x WHERE x.attempt_id=a.id))
	ORDER BY a.id`)
	if err != nil {
		return err
	}
	type activeAttempt struct {
		id                            int64
		scopeType, state              string
		scopeID                       int64
		parent                        sql.NullInt64
	}
	attempts := []activeAttempt{}
	for rows.Next() {
		var item activeAttempt
		if err := rows.Scan(&item.id, &item.scopeType, &item.scopeID, &item.state, &item.parent); err != nil {
			rows.Close()
			return err
		}
		attempts = append(attempts, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range attempts {
		directive, err := attemptDirective(ctx, conn, item.id, item.scopeType, item.scopeID, item.state, item.parent)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(maintenance_revision,kind,object_key) DO NOTHING`, revision, kindActiveAttempt, fmt.Sprintf("attempt/%d", item.id), "Blocking", strings.ToLower(item.state)+"|"+directive, now); err != nil {
			return err
		}
	}
	operationRows, err := conn.QueryContext(ctx, `
		SELECT o.id, o.kind, o.state, o.owner_attempt_id FROM browser_operations o
		WHERE o.state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect')
		ORDER BY o.id`)
	if err != nil {
		return err
	}
	type activeOperation struct {
		id                     int64
		kind, state            string
		owner                  sql.NullInt64
	}
	operations := []activeOperation{}
	for operationRows.Next() {
		var item activeOperation
		if err := operationRows.Scan(&item.id, &item.kind, &item.state, &item.owner); err != nil {
			operationRows.Close()
			return err
		}
		operations = append(operations, item)
	}
	if err := operationRows.Close(); err != nil {
		return err
	}
	for _, item := range operations {
		directive, err := operationDirective(ctx, conn, item.id, item.kind, item.state, item.owner)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(maintenance_revision,kind,object_key) DO NOTHING`, revision, kindActiveBrowser, fmt.Sprintf("operation/%d", item.id), "Blocking", strings.ToLower(item.state)+"|"+directive, now); err != nil {
			return err
		}
	}
	return nil
}
