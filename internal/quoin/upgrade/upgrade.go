// Package upgrade owns the SQL-authoritative Upgrade maintenance aggregate:
// the idempotent prepare command that enters Upgrade maintenance with its
// deterministic drain checklist, the background reconciler that projects
// active work and orchestrates the verified pre-upgrade backup, and the
// first-release schema gate executed by `quoin migrate`.
//
// Checklist item contract (this package is the authority):
//
//   - ActiveAttempt items use object_key `attempt/<attempt_id>`.
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
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/maintenance"
)

// Reason mirrors the frozen SQL enum value for the Upgrade maintenance reason.
const Reason = "Upgrade"

const (
	commandPrepare = "upgrade.prepare"

	kindActiveAttempt = "ActiveAttempt"
	kindBackup        = "BackupPreflight"

	backupObjectKey = "pre_upgrade_backup"

	detailDrained       = "drained"
	detailBackupPending = "backup_pending"
	detailBackupFailed  = "backup_failed"
	detailBackupDone    = "backup_verified"
)

type projectionExecutor = execution.Executor

var (
	ErrConflict      = maintenance.ErrConflict
	ErrCommandReused = maintenance.ErrCommandReused
)

// Service owns the prepare command and checklist projection. Prepare runs
// through the shared execution runner: the verified admin session authorizes
// in-transaction, the ledger row (with its correlation) and the audit event
// are recorded automatically in the same transaction.
type Service struct {
	runner  *execution.Runner
	prepare *execution.Operation
	now     func() time.Time
}

func NewService(db *sql.DB) *Service {
	service := &Service{now: time.Now}
	service.runner = execution.NewRunner(db, execution.NewRegistry(), nil)
	registered, err := service.runner.Register(execution.Operation{
		Name:       commandPrepare,
		Class:      execution.ClassWrite,
		ObjectType: "maintenance",
		// Prepare is a user command: the shared in-transaction verification
		// re-checks the session proof reference (unrevoked, unexpired, current
		// revision, initialized, no pending password change, admin role).
		Authorize: func(ctx context.Context, tx *execution.Tx) error {
			return auth.VerifyExecutionSession(ctx, tx, "admin")
		},
	})
	if err != nil {
		panic(fmt.Sprintf("upgrade: register %s: %v", commandPrepare, err))
	}
	service.prepare = registered
	return service
}

func (service *Service) SetReader(reader execution.Reader) error {
	return service.runner.SetReader(reader)
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
	outcome, err := execution.Run(ctx, service.runner, service.prepare, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     request.ActorID,
		ClientCommandID: request.ClientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (maintenance.State, execution.Change, error) {
		return service.prepareOn(ctx, tx, request)
	}, func(state maintenance.State) int64 { return state.RowVersion })
	if err != nil {
		return maintenance.State{}, maintenance.MapRejectionConflict(err)
	}
	// Legacy ledger rows (pre-runner releases) carry only their compact
	// {"reason":"Upgrade"} payload: the state projection decodes with row
	// version zero. The old replay contract returns the live projection —
	// preserve it.
	if outcome.Replayed && outcome.Result.RowVersion == 0 {
		return maintenance.StateOn(ctx, service.runner.Reader())
	}
	return outcome.Result, nil
}

// prepareOn is the business stage inside the runner-owned transaction. The
// caller's identity and admin role were already verified in-transaction by
// the operation's Authorize callback, so the duplicate user check is gone.
func (service *Service) prepareOn(ctx context.Context, tx *execution.Tx, request PrepareRequest) (maintenance.State, execution.Change, error) {
	var active int
	var reason string
	var current int64
	if err := tx.QueryRowContext(ctx, `SELECT active,COALESCE(reason,''),row_version FROM maintenance_state WHERE id=1`).Scan(&active, &reason, &current); err != nil {
		return maintenance.State{}, execution.Changed, err
	}
	if active == 1 && reason != Reason {
		return maintenance.State{}, execution.Changed, maintenance.ConflictRejection(fmt.Sprintf("maintenance %q is active", reason))
	}
	if current != request.ExpectedRowVersion {
		return maintenance.State{}, execution.Changed, maintenance.ConflictRejection("upgrade maintenance window moved")
	}
	if active == 0 {
		now := service.timestamp()
		revision := current + 1
		result, err := tx.ExecContext(ctx, `UPDATE maintenance_state SET active=1,reason=?,entered_at=?,entered_by_type='user',entered_by_id=?,exited_at=NULL,exited_by_type=NULL,exited_by_id=NULL,row_version=row_version+1 WHERE id=1 AND active=0 AND row_version=?`, Reason, now, request.ActorID, current)
		if err != nil {
			return maintenance.State{}, execution.Changed, err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return maintenance.State{}, execution.Changed, maintenance.ConflictRejection("upgrade maintenance window moved during entry")
		}
		if err := projectChecklist(ctx, tx, revision, now); err != nil {
			return maintenance.State{}, execution.Changed, err
		}
	}
	state, err := maintenance.StateOn(ctx, tx)
	if err != nil {
		return maintenance.State{}, execution.Changed, err
	}
	if active == 0 {
		return state, execution.Changed, nil
	}
	// Continuing an already-active window changes nothing; the runner still
	// records the continuation with its unified unchanged classification.
	return state, execution.Unchanged, nil
}

// The active-work state set is frozen by the SQL predicate below:
// execution_attempts in Queued/Assigned/Running/Cancelling can still accept
// runtime work or produce durable writes.

// projectChecklist freezes the deterministic entry snapshot: one item per
// active attempt plus the always-present pre-upgrade backup preflight.
// Existing rows are never downgraded from Safe (attempt lifecycles are
// forward-only).
func projectChecklist(ctx context.Context, conn projectionExecutor, revision int64, now string) error {
	if _, err := conn.ExecContext(ctx, `INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(maintenance_revision,kind,object_key) DO NOTHING`, revision, kindBackup, backupObjectKey, "Blocking", detailBackupPending, now); err != nil {
		return err
	}
	rows, err := conn.QueryContext(ctx, `
	SELECT a.id, a.scope_type, a.scope_id, a.state
	FROM execution_attempts a
	WHERE a.state IN ('Queued','Assigned','Running','Cancelling')
	  AND NOT (a.scope_type='run_check' AND EXISTS (SELECT 1 FROM inspection_check_results x WHERE x.attempt_id=a.id))
	ORDER BY a.id`)
	if err != nil {
		return err
	}
	type activeAttempt struct {
		id               int64
		scopeType, state string
		scopeID          int64
	}
	attempts := []activeAttempt{}
	for rows.Next() {
		var item activeAttempt
		if err := rows.Scan(&item.id, &item.scopeType, &item.scopeID, &item.state); err != nil {
			rows.Close()
			return err
		}
		attempts = append(attempts, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range attempts {
		directive, err := attemptDirective(ctx, conn, item.id, item.scopeType, item.scopeID, item.state)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(maintenance_revision,kind,object_key) DO NOTHING`, revision, kindActiveAttempt, fmt.Sprintf("attempt/%d", item.id), "Blocking", strings.ToLower(item.state)+"|"+directive, now); err != nil {
			return err
		}
	}
	return nil
}
