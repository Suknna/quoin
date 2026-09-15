package upgrade

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// commandReconcile is the audited background operation identity for one
// projection pass that can mutate the drain checklist or queue the pre-upgrade
// backup.
const commandReconcile = "upgrade.reconcile"

// BackupRunner is the projection the reconciler needs from the backup
// authority: execute one freshly queued upgrade run to completion. The
// concrete implementation is backup.Service.Run.
type BackupRunner interface {
	RunUpgrade(ctx context.Context, id int64) error
}

// Reconciler projects the drain checklist, orchestrates the verified
// pre-upgrade backup and drives the quoin_upgrade_prepared gauge. It mutates
// only maintenance_items and backups; draining itself always goes through
// the frozen upgrade-drain cancel commands or the running T12 sweeps.
//
// Every pass that can mutate runs through the shared execution runner as one
// audited system execution (scheduler source, fresh per-scope correlation);
// a fully prepared window can no longer mutate, so its quiet passes record
// nothing.
type Reconciler struct {
	db        *sql.DB
	backups   BackupRunner
	runner    *execution.Runner
	reconcile *execution.Operation
	prepared  func(bool)
	interval  time.Duration
	wake      chan struct{}
}

func NewReconciler(db *sql.DB, backups BackupRunner) *Reconciler {
	reconciler := &Reconciler{db: db, backups: backups, interval: 2 * time.Second, wake: make(chan struct{}, 1)}
	reconciler.runner = execution.NewRunner(db, execution.NewRegistry(), nil)
	registered, err := reconciler.runner.Register(execution.Operation{
		Name:       commandReconcile,
		Class:      execution.ClassWrite,
		ObjectType: "maintenance",
		// The reconcile loop is a background mutation: it runs only as the
		// system principal. A user context here is a wiring error, never an
		// authorized actor.
		Authorize: func(ctx context.Context, tx *execution.Tx) error {
			meta, err := execution.Require(ctx)
			if err != nil {
				return err
			}
			if meta.Actor.Kind != execution.PrincipalSystem {
				return fmt.Errorf("upgrade: %s runs only as the system principal, got %s", commandReconcile, meta.Actor.Kind)
			}
			return nil
		},
	})
	if err != nil {
		panic(fmt.Sprintf("upgrade: register %s: %v", commandReconcile, err))
	}
	reconciler.reconcile = registered
	return reconciler
}

// SetPrepared installs the ops gauge projector.
func (reconciler *Reconciler) SetPrepared(prepared func(bool)) {
	reconciler.prepared = prepared
}

// Notify wakes the loop immediately; the prepare HTTP command calls it after
// commit so the checklist appears without waiting for a tick.
func (reconciler *Reconciler) Notify() {
	select {
	case reconciler.wake <- struct{}{}:
	default:
	}
}

// Run owns the process-lifetime loop. Errors never stop reconciliation.
func (reconciler *Reconciler) Run(ctx context.Context) {
	ticker := time.NewTicker(reconciler.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-reconciler.wake:
		}
		if _, err := reconciler.Reconcile(ctx); err != nil {
			sharedops.LogEvent("quoin", "error", "upgrade.reconcile_failed", err.Error())
		}
	}
}

// reconcileProjection is one pass's verdict. Revision doubles as the audit
// object reference.
type reconcileProjection struct {
	revision      int64
	backupID      int64
	run, prepared bool
}

// Reconcile executes one pass and reports whether the current revision is
// fully prepared (all items Safe plus a succeeded upgrade backup). A pass on
// a window that can still change runs as one audited system execution; the
// projection transaction closes before any backup execution or recursion:
// the production database allows exactly one connection.
func (reconciler *Reconciler) Reconcile(ctx context.Context) (bool, error) {
	ctx = reconciler.backgroundContext(ctx)
	backupID, run, prepared, err := reconciler.projectGated(ctx)
	if err != nil {
		return false, err
	}
	if run && backupID != 0 {
		// Execute the freshly queued backup outside the projection
		// transaction; its own state machine owns every stage transition.
		if err := reconciler.backups.RunUpgrade(ctx, backupID); err != nil {
			sharedops.LogEvent("quoin", "error", "upgrade.backup_failed", fmt.Sprintf("backup=%d %v", backupID, err))
		}
		// The terminal run state decides the next projection; observe it once.
		_, _, settled, _ := reconciler.projectGated(ctx)
		reconciler.projectPrepared(settled)
		return false, nil
	}
	reconciler.projectPrepared(prepared)
	return prepared, nil
}

// projectGated runs every projection pass through the ONE audited system
// execution. Inside the runner transaction the quiet precondition decides:
// a pass with nothing left to change (window irrelevant or fully prepared)
// returns a quietPass verdict — execution.ErrNoTransition, so the runner
// discards the read-only stage and records no ledger and no audit row —
// while a pass whose window can still change converges the checklist and
// queues work as one audited mutation. There is no separate unaudited
// transaction left, and the precondition is re-read inside the transaction,
// so no mutation can escape the audited path through a stale gate read.
func (reconciler *Reconciler) projectGated(ctx context.Context) (int64, bool, bool, error) {
	projection, err := execution.Execute(ctx, reconciler.runner, reconciler.reconcile, func(tx *execution.Tx) (reconcileProjection, error) {
		quiet, err := quietPreconditionOn(ctx, tx)
		if err != nil {
			return reconcileProjection{}, err
		}
		if quiet {
			// Nothing may change: the quiet verdict reports prepared without
			// a recorded fact. The typed quietPass carries the verdict back
			// past the runner while remaining an ErrNoTransition.
			return reconcileProjection{}, &quietPass{}
		}
		revision, backupID, run, prepared, err := reconciler.projectOn(ctx, tx)
		return reconcileProjection{revision: revision, backupID: backupID, run: run, prepared: prepared}, err
	}, func(projection reconcileProjection) int64 { return projection.revision })
	var quiet *quietPass
	if errors.As(err, &quiet) {
		return 0, false, true, nil
	}
	if err != nil {
		return 0, false, false, err
	}
	return projection.backupID, projection.run, projection.prepared, nil
}

// quietPass marks the quiet reconcile verdict: nothing may change in this
// pass, so the runner records nothing (it is an execution.ErrNoTransition)
// and the caller learns the window is fully prepared.
type quietPass struct{}

func (q *quietPass) Error() string { return "upgrade: reconcile window is quiet; nothing to project" }
func (q *quietPass) Is(target error) bool { return target == execution.ErrNoTransition }

// backgroundContext restores the loop's execution scope. When the persisted
// ledger links the window to a user's committed upgrade.prepare, the passes
// inherit that originating operation's correlation and initiator — the whole
// upgrade stays one correlated business operation with the system principal as
// the acting executor. Only a window without such a record falls back to a
// fresh per-scope correlation. A caller-supplied metadata scope (tests,
// deliberate re-rooting) is preserved untouched.
func (reconciler *Reconciler) backgroundContext(ctx context.Context) context.Context {
	if _, ok := execution.FromContext(ctx); ok {
		return ctx
	}
	if meta, ok := reconciler.originatingPrepareScope(ctx); ok {
		if withMetadata, err := execution.ReplaceMetadata(ctx, meta); err == nil {
			return withMetadata
		}
	}
	correlation, err := execution.NewCorrelationID()
	if err != nil {
		sharedops.LogEvent("quoin", "error", "upgrade.reconcile_correlation_failed", err.Error())
		return ctx
	}
	withMetadata, err := execution.ReplaceMetadata(ctx, execution.Metadata{
		CorrelationID: correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem, ID: 0},
		Source:        execution.Source{Kind: execution.SourceScheduler},
	})
	if err != nil {
		sharedops.LogEvent("quoin", "error", "upgrade.reconcile_correlation_failed", err.Error())
		return ctx
	}
	return withMetadata
}

// originatingPrepareScope reads the persisted originating operation from the
// durable command ledger: the latest committed upgrade.prepare carries the
// user's correlation and identity. ok is false when no such record exists (a
// legacy window entered before the unified runner).
func (reconciler *Reconciler) originatingPrepareScope(ctx context.Context) (execution.Metadata, bool) {
	conn, err := reconciler.db.Conn(ctx)
	if err != nil {
		return execution.Metadata{}, false
	}
	defer conn.Close()
	var principalType, correlation string
	var principalID int64
	err = conn.QueryRowContext(ctx, `SELECT principal_type,principal_id,COALESCE(correlation_id,'') FROM client_commands WHERE command_type=? AND outcome='committed' ORDER BY created_at DESC LIMIT 1`, commandPrepare).Scan(&principalType, &principalID, &correlation)
	if err != nil || correlation == "" || principalType != string(execution.PrincipalUser) || principalID < 1 {
		return execution.Metadata{}, false
	}
	return execution.Metadata{
		CorrelationID: correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem, ID: 0},
		Initiator:     execution.Principal{Kind: execution.PrincipalUser, ID: principalID},
		Source:        execution.Source{Kind: execution.SourceScheduler},
	}, true
}

func quietPreconditionOn(ctx context.Context, conn projectionExecutor) (bool, error) {
	var active int
	var reason string
	var revision int64
	if err := conn.QueryRowContext(ctx, `SELECT active,COALESCE(reason,''),row_version FROM maintenance_state WHERE id=1`).Scan(&active, &reason, &revision); err != nil {
		return false, err
	}
	if active != 1 || reason != Reason {
		return true, nil
	}
	var blocking int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM maintenance_items WHERE maintenance_revision=? AND safe_state='Blocking'`, revision).Scan(&blocking); err != nil {
		return false, err
	}
	if blocking != 0 {
		return false, nil
	}
	var enteredAt string
	if err := conn.QueryRowContext(ctx, `SELECT entered_at FROM maintenance_state WHERE id=1`).Scan(&enteredAt); err != nil {
		return false, err
	}
	succeeded, err := succeededUpgradeBackup(ctx, conn, enteredAt)
	if err != nil {
		return false, err
	}
	return succeeded != nil, nil
}

// projectOn is the shared projection body. The runner calls it on its own
// guarded transaction; project calls it on its own. Every read, including the
// prepared verdict, stays on this one open transaction executor.
func (reconciler *Reconciler) projectOn(ctx context.Context, conn projectionExecutor) (revision int64, backupID int64, run bool, prepared bool, err error) {
	var active int
	var reason string
	if err = conn.QueryRowContext(ctx, `SELECT active,COALESCE(reason,''),row_version FROM maintenance_state WHERE id=1`).Scan(&active, &reason, &revision); err != nil {
		return 0, 0, false, false, err
	}
	if active != 1 || reason != Reason {
		return 0, 0, false, false, nil
	}
	var enteredAt string
	var enteredBy int64
	if err = conn.QueryRowContext(ctx, `SELECT entered_at,COALESCE(entered_by_id,0) FROM maintenance_state WHERE id=1`).Scan(&enteredAt, &enteredBy); err != nil {
		return 0, 0, false, false, err
	}
	if err = reconcileChecklist(ctx, conn, revision); err != nil {
		return 0, 0, false, false, err
	}
	succeeded, err := succeededUpgradeBackup(ctx, conn, enteredAt)
	if err != nil {
		return 0, 0, false, false, err
	}
	if succeeded != nil {
		if _, err = conn.ExecContext(ctx, `UPDATE maintenance_items SET safe_state='Safe',detail_code=?,updated_at=? WHERE maintenance_revision=? AND kind=? AND object_key=? AND safe_state='Blocking'`, detailBackupDone, time.Now().UTC().Format(time.RFC3339Nano), revision, kindBackup, backupObjectKey); err != nil {
			return 0, 0, false, false, err
		}
	}
	backupID, run, err = planUpgradeBackup(ctx, conn, revision, enteredAt, enteredBy, succeeded == nil)
	if err != nil {
		return 0, 0, false, false, err
	}
	var blocking int
	if err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM maintenance_items WHERE maintenance_revision=? AND safe_state='Blocking'`, revision).Scan(&blocking); err != nil {
		return 0, 0, false, false, err
	}
	prepared = blocking == 0 && succeeded != nil
	return revision, backupID, run, prepared, nil
}

func (reconciler *Reconciler) projectPrepared(prepared bool) {
	if reconciler.prepared != nil {
		reconciler.prepared(prepared)
	}
}

// reconcileChecklist converges work items: still-active work refreshes its
// Blocking projection (cancel row versions may have moved), terminal work
// freezes as Safe. The entry snapshot already created the rows; inserts here
// are defensive against any projection gap.
func reconcileChecklist(ctx context.Context, conn projectionExecutor, revision int64) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := conn.ExecContext(ctx, `UPDATE maintenance_items SET safe_state='Safe',detail_code=?,updated_at=? WHERE maintenance_revision=? AND kind='ActiveAttempt' AND safe_state='Blocking' AND object_key IN (SELECT 'attempt/'||a.id FROM execution_attempts a WHERE a.state IN ('Succeeded','Failed','Cancelled','Interrupted') OR EXISTS (SELECT 1 FROM inspection_check_results x WHERE x.attempt_id=a.id))`, detailDrained, now, revision); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE maintenance_items SET safe_state='Safe',detail_code=?,updated_at=? WHERE maintenance_revision=? AND kind='ActiveBrowserOperation' AND safe_state='Blocking' AND object_key IN (SELECT 'operation/'||o.id FROM browser_operations o WHERE o.state IN ('Succeeded','Failed','Cancelled','Interrupted'))`, detailDrained, now, revision); err != nil {
		return err
	}
	if err := projectChecklist(ctx, conn, revision, now); err != nil {
		return err
	}
	// projectChecklist only inserts; a still-Blocking row may need its cancel
	// row version refreshed after a concurrent domain change.
	rows, err := conn.QueryContext(ctx, `SELECT a.id,a.scope_type,a.scope_id,a.state,a.requested_by_tool_call_id FROM execution_attempts a JOIN maintenance_items m ON m.object_key='attempt/'||a.id AND m.maintenance_revision=? WHERE m.kind='ActiveAttempt' AND m.safe_state='Blocking' AND a.state IN ('Queued','Assigned','Running','Cancelling')`, revision)
	if err != nil {
		return err
	}
	type activeAttempt struct {
		id               int64
		scopeType, state string
		scopeID          int64
		parent           sql.NullInt64
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
		if _, err := conn.ExecContext(ctx, `UPDATE maintenance_items SET detail_code=?,updated_at=? WHERE maintenance_revision=? AND kind='ActiveAttempt' AND object_key=? AND safe_state='Blocking'`, lowercase(item.state)+"|"+directive, now, revision, fmt.Sprintf("attempt/%d", item.id)); err != nil {
			return err
		}
	}
	operationRows, err := conn.QueryContext(ctx, `SELECT o.id,o.kind,o.state,o.owner_attempt_id FROM browser_operations o JOIN maintenance_items m ON m.object_key='operation/'||o.id AND m.maintenance_revision=? WHERE m.kind='ActiveBrowserOperation' AND m.safe_state='Blocking' AND o.state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect')`, revision)
	if err != nil {
		return err
	}
	type activeOperation struct {
		id          int64
		kind, state string
		owner       sql.NullInt64
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
		if _, err := conn.ExecContext(ctx, `UPDATE maintenance_items SET detail_code=?,updated_at=? WHERE maintenance_revision=? AND kind='ActiveBrowserOperation' AND object_key=? AND safe_state='Blocking'`, lowercase(item.state)+"|"+directive, now, revision, fmt.Sprintf("operation/%d", item.id)); err != nil {
			return err
		}
	}
	return nil
}

// succeededUpgradeBackup returns the upgrade backup bound to the current
// maintenance window: created after the revision entered and succeeded.
func succeededUpgradeBackup(ctx context.Context, conn projectionExecutor, enteredAt string) (*int64, error) {
	var id int64
	err := conn.QueryRowContext(ctx, `SELECT id FROM backups WHERE trigger_kind='upgrade' AND status='succeeded' AND created_at>=? ORDER BY id DESC LIMIT 1`, enteredAt).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// planUpgradeBackup applies the frozen admission: create the online upgrade
// run only when no work is Blocking, no succeeded run exists, no backup of
// any kind is active, and either no upgrade attempt failed in this window or
// the Admin committed a newer prepare command after that failure. The unique
// active index makes the INSERT the commit-order race decider.
func planUpgradeBackup(ctx context.Context, conn projectionExecutor, revision int64, enteredAt string, enteredBy int64, needBackup bool) (int64, bool, error) {
	if !needBackup {
		return 0, false, nil
	}
	var blocking int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM maintenance_items WHERE maintenance_revision=? AND kind IN ('ActiveAttempt','ActiveBrowserOperation') AND safe_state='Blocking'`, revision).Scan(&blocking); err != nil {
		return 0, false, err
	}
	if blocking != 0 {
		return 0, false, nil
	}
	var activeBackup int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM backups WHERE status IN ('queued','running')`).Scan(&activeBackup); err != nil {
		return 0, false, err
	}
	if activeBackup != 0 {
		return 0, false, nil
	}
	var failedAt string
	err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(updated_at),'') FROM backups WHERE trigger_kind='upgrade' AND status='failed' AND created_at>=?`, enteredAt).Scan(&failedAt)
	if err != nil {
		return 0, false, err
	}
	if failedAt != "" {
		var rearmAt string
		err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(created_at),'') FROM client_commands WHERE command_type='upgrade.prepare' AND outcome='committed'`).Scan(&rearmAt)
		if err != nil {
			return 0, false, err
		}
		if rearmAt <= failedAt {
			return 0, false, nil
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var triggered any
	if enteredBy > 0 {
		triggered = enteredBy
	}
	result, err := conn.ExecContext(ctx, `INSERT INTO backups(status,stage,trigger_kind,execution_mode,scheduled_for,row_version,created_at,updated_at,triggered_by) VALUES('queued','queued','upgrade','online',NULL,1,?,?,?)`, now, now, triggered)
	if err != nil {
		// Another writer created the active run first: the unique active
		// index already decided the commit order, this pass only observes it.
		if strings.Contains(err.Error(), "ux_backups_active") || strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return 0, false, nil
		}
		return 0, false, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

func lowercase(value string) string {
	return strings.ToLower(value)
}
