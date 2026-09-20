// Run admission: the StartRun command creates one complete durable observation
// work graph (root, one child per plugin-declared object type, frozen inputs,
// source grants) inside the execution runner's audited transaction — a manual
// refresh under the administrator's verified session, a scheduler tick or
// enablement kick under the explicit system scope — and the durable
// reconciliation commands that keep automatic observation convergent
// (schedule ticks, connection-disable cancellation). The active-run and
// scheduled-run partial indexes are the cross-process dedupe authority;
// callers may safely retry any command here.
package observation

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/tools/thanos"
)

// runDetail read note: runDetailOn/activeRunOn compose on the audit.Reader
// read surface — the guarded runner transaction satisfies it, so the
// admission business stage still reads the same uncommitted snapshot.

// executionInput is the frozen source_observation_execution_v1 canonical
// snapshot. Every discovery fact (object type, query, identity labels, limit)
// is copied from the plugin descriptor at admission; the Plinth supervisor
// re-validates the frozen copy against the same descriptor before executing
// through the bound Discoverer, so declaration and execution cannot drift.
type executionInput struct {
	SchemaKind       string   `json:"schemaKind"`
	AttemptID        int64    `json:"attemptId"`
	ObservationRunID int64    `json:"observationRunId"`
	PluginID         string   `json:"pluginId"`
	ObjectType       string   `json:"objectType"`
	Query            string   `json:"query"`
	IdentityLabels   []string `json:"identityLabels"`
	Limit            int      `json:"limit"`
	GrantID          int64    `json:"grantId"`
}

// StartRun admits one complete bounded observation for one connection.
// triggerKind is "manual", "schedule" or "enablement"; only "schedule" may
// carry scheduledFor (the deterministic dedup tick).
//
// Origins and their identities:
//   - manual: the caller's context must carry the administrator's execution
//     metadata (user actor with its verified session); the operation
//     authorization re-verifies the session inside the transaction via
//     auth.VerifyExecutionSession. There is no fallback without it.
//   - schedule/enablement: the explicit system scope is attached here
//     (scheduler or internal source), so the durable tick is audited under
//     the system principal and its children inherit the correlation.
//
// Replaying a committed clientCommandID returns the stored result; an
// already-active Run is returned unchanged instead of forking a second root.
func (service *Service) StartRun(ctx context.Context, principalID int64, clientCommandID, connectionName, triggerKind string, scheduledFor *string) (SourceObservationRun, error) {
	switch triggerKind {
	case "manual", "enablement":
		if scheduledFor != nil {
			return SourceObservationRun{}, fmt.Errorf("%s observation trigger must not carry a scheduled tick", triggerKind)
		}
	case "schedule":
		if scheduledFor == nil {
			return SourceObservationRun{}, errors.New("scheduled observation trigger requires its scheduled tick")
		}
	default:
		return SourceObservationRun{}, fmt.Errorf("invalid source observation trigger kind %q", triggerKind)
	}
	digest := commandDigest(connectionName, triggerKind, scheduledFor)
	command := execution.Command{ClientCommandID: clientCommandID, Digest: digest}
	switch triggerKind {
	case "manual":
		if principalID < 1 {
			return SourceObservationRun{}, errors.New("manual source observation requires an administrator principal")
		}
		command.PrincipalType, command.PrincipalID = string(execution.PrincipalUser), principalID
	default:
		source := execution.SourceScheduler
		if triggerKind == "enablement" {
			source = execution.SourceInternal
		}
		scoped, err := ensureSystemScope(ctx, source)
		if err != nil {
			return SourceObservationRun{}, err
		}
		ctx = scoped
		command.PrincipalType, command.PrincipalID = string(execution.PrincipalSystem), 0
	}
	outcome, err := execution.Run(ctx, service.runner, service.startOp, command,
		func(tx *execution.Tx) (SourceObservationRun, execution.Change, error) {
			if err := admissionFence(ctx, tx); err != nil {
				return SourceObservationRun{}, execution.Unchanged, err
			}
			var connectionID int64
			var connectionType string
			err := tx.QueryRowContext(ctx, `SELECT id,type FROM connections WHERE name=? AND enabled=1 AND revalidation_required=0`, connectionName).Scan(&connectionID, &connectionType)
			if errors.Is(err, sql.ErrNoRows) {
				return SourceObservationRun{}, execution.Unchanged, fmt.Errorf("%w: %s", ErrNotObservable, connectionName)
			}
			if err != nil {
				return SourceObservationRun{}, execution.Unchanged, err
			}
			// The descriptor is resolved inside the business stage against the
			// frozen process catalog; holding no descriptor decision outside
			// the authorization keeps one fence for the whole admission.
			plugin, err := service.discoverableDescriptor(connectionType)
			if err != nil {
				return SourceObservationRun{}, execution.Unchanged, err
			}
			// At most one active Run per connection (frozen partial index): a
			// manual or enablement kick piggybacks on the active root instead
			// of forking.
			if existing, found, err := service.activeRunOn(ctx, tx, connectionID, connectionName); err != nil {
				return SourceObservationRun{}, execution.Unchanged, err
			} else if found {
				return existing, execution.Unchanged, nil
			}
			// The scheduler uses actor 0, which is intentionally not a users
			// row; the nullable creator records that distinction without
			// fabricating a principal.
			var createdBy any = command.PrincipalID
			if command.PrincipalID == 0 {
				createdBy = nil
			}
			now := service.nowText()
			result, err := tx.ExecContext(ctx, `INSERT INTO observation_runs(connection_id,plugin_id,trigger_kind,scheduled_for,state,row_version,created_by,created_at) VALUES(?,?,?,?, 'Queued',1,?,?)`,
				connectionID, plugin.ID, triggerKind, scheduledFor, createdBy, now)
			if err != nil {
				return SourceObservationRun{}, execution.Unchanged, err
			}
			runID, err := result.LastInsertId()
			if err != nil {
				return SourceObservationRun{}, execution.Unchanged, err
			}
			// Evidence time is generated when observation truly starts;
			// children are created after the root turns Running so the scope
			// trigger always sees an active root and the dispatcher never
			// meets an orphaned root.
			if _, err := tx.ExecContext(ctx, `UPDATE observation_runs SET state='Running',evidence_at=?,row_version=2 WHERE id=?`, now, runID); err != nil {
				return SourceObservationRun{}, execution.Unchanged, err
			}
			if err := createObservationAttempts(ctx, tx, runID, connectionID, plugin, now); err != nil {
				return SourceObservationRun{}, execution.Unchanged, err
			}
			detail, err := service.runDetailOn(ctx, tx, connectionName, runID)
			if err != nil {
				return SourceObservationRun{}, execution.Unchanged, err
			}
			return detail, execution.Changed, nil
		},
		func(detail SourceObservationRun) int64 {
			id, _ := strconv.ParseInt(detail.ID, 10, 64)
			return id
		})
	if err != nil {
		if errors.Is(err, execution.ErrCommandReused) {
			return SourceObservationRun{}, ErrCommandReused
		}
		return SourceObservationRun{}, err
	}
	return outcome.Result, nil
}

// activeRunOn returns the currently active Run of one connection, if any.
func (service *Service) activeRunOn(ctx context.Context, conn audit.Reader, connectionID int64, connectionName string) (SourceObservationRun, bool, error) {
	var runID int64
	err := conn.QueryRowContext(ctx, `SELECT id FROM observation_runs WHERE connection_id=? AND state IN ('Queued','Running') ORDER BY id DESC LIMIT 1`, connectionID).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return SourceObservationRun{}, false, nil
	}
	if err != nil {
		return SourceObservationRun{}, false, err
	}
	detail, err := service.runDetailOn(ctx, conn, connectionName, runID)
	if err != nil {
		return SourceObservationRun{}, false, err
	}
	return detail, true, nil
}

// createObservationAttempts freezes one supervisor-only discovery child per
// plugin-declared object type. The descriptor owns the object vocabulary;
// this loop only projects it durably, so a plugin that declares more object
// types extends observation without touching the admission core. Children are
// created through attempt.CreateOn so each inherits the admission's
// correlation (the administrator's session scope or the scheduler's system
// scope) — an attempt can never exist without its association.
func createObservationAttempts(ctx context.Context, conn execution.Executor, runID, connectionID int64, descriptor plugins.Plugin, now string) error {
	for _, object := range descriptor.DiscoverObjects {
		input := executionInput{
			SchemaKind:       ExecutionSchemaKind,
			ObservationRunID: runID,
			PluginID:         descriptor.ID,
			ObjectType:       object.ObjectType,
			Query:            object.Query,
			IdentityLabels:   object.IdentityLabels,
			Limit:            object.Limit,
		}
		// The object row must exist with a NULL attempt binding before the
		// child attempt: the frozen scope trigger only admits an
		// observation_run attempt whose object row is still unclaimed.
		if _, err := conn.ExecContext(ctx, `INSERT INTO observation_run_objects(observation_run_id,object_type,status,gap_reason,created_at) VALUES(?,?,'gap','runtime_unavailable',?)`,
			runID, input.ObjectType, now); err != nil {
			return err
		}
		attemptID, err := attempt.CreateOn(ctx, conn, `INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,discovery_key,state,quoin_release_version,created_at) VALUES('inspection_collection','observation_run',?,?,'Queued',?,?)`,
			runID, input.ObjectType, attempt.ReleaseVersion(), now)
		if err != nil {
			return err
		}
		input.AttemptID = attemptID
		// The grant binds the attempt to the connection's exact current
		// (revision, generation) pair — a real source grant, not a business
		// config projection. Resolution refuses disabled or rotated connections.
		grant, err := thanos.ResolveConfigGrantForConnection(ctx, conn, input.AttemptID, connectionID)
		if err != nil {
			return err
		}
		input.GrantID = grant.GrantID
		canonical, err := json.Marshal(input)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(canonical)
		snapshot, err := conn.ExecContext(ctx, `INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(?,?,?,?,?)`,
			input.AttemptID, ExecutionSchemaKind, executionRendererVersion, hex.EncodeToString(digest[:]), now)
		if err != nil {
			return err
		}
		snapshotID, err := snapshot.LastInsertId()
		if err != nil {
			return err
		}
		revisionDigest := sha256.Sum256([]byte(fmt.Sprintf("connection_revision:%d", grant.ConnectionRevisionID)))
		// The input lineage is the frozen connection revision itself — the
		// same fixed-mode source the probe attempts bind. Waiting for
		// Implementation: trg_attempt_input_item_closure still narrows
		// connection_revision_id to connection_probe attempts; bc14 owns the
		// trigger amendment that admits observation_run scope (coordinated
		// 2026-09-13).
		if _, err := conn.ExecContext(ctx, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id) VALUES(?,1,'connection_revision',?,?)`,
			snapshotID, hex.EncodeToString(revisionDigest[:]), grant.ConnectionRevisionID); err != nil {
			return err
		}
		// The child binds back to its object row so result adjudication and
		// the run completion fence resolve through one frozen join.
		if _, err := conn.ExecContext(ctx, `UPDATE observation_run_objects SET attempt_id=? WHERE observation_run_id=? AND object_type=?`, input.AttemptID, runID, input.ObjectType); err != nil {
			return err
		}
	}
	return nil
}

// Attempts configures the generic attempt transitions with this package's
// deterministic input rebuild, mirroring the resource refresh slice. The
// ephemeral machine shares the parent's validated read-only reader so its
// standalone reads use the same fail-closed seam; before composition wires
// it the forwarded fail-closed reader is refused by the child's own probe
// and the child simply stays unwired — its reads fail closed identically.
// Callers that only compose *On transaction mutations need no reader at all.
func (service *Service) Attempts() *attempt.Service {
	attempts := attempt.NewService(service.db)
	attempts.SnapshotRebuilder = service.rebuildObservationAttempt
	_ = attempts.SetReader(service.runner.Reader())
	return attempts
}

// ConvergeTechnicalGap terminates one permanently unexecutable Queued child:
// a rebuild failure means the frozen input can never be honestly dispatched
// again (plugin metadata drift, missing grant), so the child records an
// honest plugin_unavailable gap and the Run converges instead of retrying
// the same failure every scheduler pass. Transient runtime unavailability
// never reaches this path — those children stay Queued by design. The
// convergence runs as an audited system operation.
func (service *Service) ConvergeTechnicalGap(ctx context.Context, attemptID int64, reason string) error {
	ctx, err := ensureSystemScope(ctx, execution.SourceInternal)
	if err != nil {
		return err
	}
	var runID int64
	_, err = execution.Execute(ctx, service.runner, service.convergeOp, func(tx *execution.Tx) (struct{}, error) {
		err := tx.QueryRowContext(ctx, `SELECT scope_id FROM execution_attempts WHERE id=? AND scope_type='observation_run' AND state='Queued'`, attemptID).Scan(&runID)
		if errors.Is(err, sql.ErrNoRows) {
			// Already dispatched or terminal: nothing to converge.
			runID = 0
			return struct{}{}, nil
		}
		if err != nil {
			return struct{}{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE execution_attempts SET state='Failed',ended_at=? WHERE id=? AND state='Queued'`, service.nowText(), attemptID); err != nil {
			return struct{}{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE observation_run_objects SET status='gap',gap_reason='plugin_unavailable' WHERE attempt_id=? AND result_digest IS NULL`, attemptID); err != nil {
			return struct{}{}, err
		}
		if err := service.convergeRunOn(ctx, tx, runID); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, nil
	}, func(struct{}) int64 { return runID })
	return err
}

// QueuedObservationAttempts lists dispatchable children whose root is still
// Running; the dispatcher picks them up after admission and on reconnect.
func (service *Service) QueuedObservationAttempts(ctx context.Context) ([]int64, error) {
	rows, err := service.runner.Reader().QueryContext(ctx, `SELECT a.id FROM execution_attempts a JOIN observation_runs r ON r.id=a.scope_id WHERE a.attempt_type='inspection_collection' AND a.scope_type='observation_run' AND a.state='Queued' AND r.state='Running' ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// rebuildObservationAttempt deterministically reconstructs the frozen input
// from authoritative rows; the recomputed digest must equal the sealed
// snapshot digest or dispatch refuses to lie about the attempt's inputs. The
// descriptor lookup uses the frozen plugin id — a plugin that left the
// deployment (or changed its declared discovery metadata) fails the rebuild
// honestly instead of dispatching re-interpreted inputs.
func (service *Service) rebuildObservationAttempt(ctx context.Context, attemptID int64) ([]byte, error) {
	var input executionInput
	var pluginID string
	var grant sql.NullInt64
	err := service.runner.Reader().QueryRowContext(ctx, `
		SELECT a.scope_id,a.discovery_key,r.plugin_id,
		       (SELECT id FROM attempt_connection_grants WHERE attempt_id=a.id AND purpose='config_thanos_query')
		FROM execution_attempts a
		JOIN observation_runs r ON r.id=a.scope_id
		WHERE a.id=? AND a.attempt_type='inspection_collection' AND a.scope_type='observation_run'`, attemptID).Scan(&input.ObservationRunID, &input.ObjectType, &pluginID, &grant)
	if err != nil {
		return nil, err
	}
	if !grant.Valid {
		return nil, fmt.Errorf("attempt %d has no frozen source grant", attemptID)
	}
	descriptor, exists := service.registry.Plugin(pluginID)
	if !exists {
		return nil, fmt.Errorf("attempt %d binds plugin %q which is no longer registered", attemptID, pluginID)
	}
	object, err := discoverObject(descriptor, input.ObjectType)
	if err != nil {
		return nil, err
	}
	input.SchemaKind, input.AttemptID, input.PluginID = ExecutionSchemaKind, attemptID, pluginID
	input.Query, input.IdentityLabels, input.Limit = object.Query, object.IdentityLabels, object.Limit
	input.GrantID = grant.Int64
	canonical, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(canonical)
	var expected string
	if err := service.runner.Reader().QueryRowContext(ctx, `SELECT content_digest FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&expected); err != nil {
		return nil, err
	}
	if expected != hex.EncodeToString(digest[:]) {
		return nil, errors.New("source observation input digest no longer matches frozen snapshot")
	}
	return canonical, nil
}

// maintenanceFence reads the maintenance singleton over the pool. An absent
// row means normal operation; an unreadable source is a scheduler fault.
func (service *Service) maintenanceFence(ctx context.Context) error {
	var maintenanceActive int
	if err := service.runner.Reader().QueryRowContext(ctx, `SELECT COALESCE((SELECT active FROM maintenance_state WHERE id=1),0)`).Scan(&maintenanceActive); err != nil {
		return fmt.Errorf("read observation maintenance fence: %w", err)
	}
	if maintenanceActive != 0 {
		return ErrMaintenanceActive
	}
	return nil
}

// AdmitDue cancels observation for connections that lost observability, then
// admits due schedule ticks. It is the whole durable scheduling brain; the app
// coordinator only supplies the clock and the dispatch kick. The whole pass
// runs under one explicit system scope (scheduler source, one shared
// correlation) so every admitted tick and every audit record of the pass is
// attributable to the scheduler as one operation.
func (service *Service) AdmitDue(ctx context.Context, now time.Time) error {
	// The maintenance fence leads every pass: an unreadable fence is a
	// scheduler-visible fault, and active maintenance is a normal admission
	// pause (nil), not a per-second error. StartRun re-checks the fence
	// inside its own writer transaction as the authoritative gate.
	if err := service.maintenanceFence(ctx); err != nil {
		if errors.Is(err, ErrMaintenanceActive) {
			return nil
		}
		return err
	}
	ctx, err := ensureSystemScope(ctx, execution.SourceScheduler)
	if err != nil {
		return err
	}
	if _, err := service.CancelUnobservable(ctx); err != nil {
		return err
	}
	candidates, err := service.dueCandidates(ctx)
	if err != nil {
		return err
	}
	interval := time.Duration(DefaultIntervalSeconds) * time.Second
	for _, item := range candidates {
		if item.lastRuns.Valid {
			last, parseErr := time.Parse(time.RFC3339Nano, item.lastRuns.String)
			if parseErr != nil {
				return fmt.Errorf("parse last source observation for %s: %w", item.name, parseErr)
			}
			if now.Before(last.Add(interval)) {
				continue
			}
		}
		// The UTC interval boundary is the durable dedupe tick: a failed Run
		// retries at the next declared cadence without forking extra roots.
		tick := now.UTC().Truncate(interval).Format(time.RFC3339)
		if _, err := service.StartRun(ctx, 0, fmt.Sprintf("source-observation:%d:%s", item.id, tick), item.name, "schedule", &tick); err != nil && !errors.Is(err, ErrNotObservable) {
			return fmt.Errorf("start source observation for %s: %w", item.name, err)
		}
	}
	return nil
}

// dueCandidate is one connection eligible for a schedule tick read.
type dueCandidate struct {
	name     string
	id       int64
	lastRuns sql.NullString
}

// dueCandidates materializes the due connection snapshot. This is a pure read
// with no transaction of its own: StartRun's writer transaction plus the
// active-run/scheduled-tick partial indexes are the admission fence, and the
// read must release its connection before those commands run (production
// SQLite runs MaxOpenConns(1)).
func (service *Service) dueCandidates(ctx context.Context) ([]dueCandidate, error) {
	kinds := service.enabledDiscoverKinds()
	if len(kinds) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",")
	args := make([]any, 0, len(kinds))
	for _, kind := range kinds {
		args = append(args, kind)
	}
	query := fmt.Sprintf(`
		SELECT c.id,c.name,
		       (SELECT MAX(r.created_at) FROM observation_runs r
		        WHERE r.connection_id=c.id AND r.state IN ('Completed','CompletedWithWarnings','Failed'))
		FROM connections c
		WHERE c.enabled=1 AND c.revalidation_required=0
		  AND c.type IN (%s)
		  AND NOT EXISTS (SELECT 1 FROM observation_runs active
		                  WHERE active.connection_id=c.id AND active.state IN ('Queued','Running'))
		ORDER BY c.id`, placeholders)
	rows, err := service.runner.Reader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list source observation candidates: %w", err)
	}
	defer rows.Close()
	candidates := []dueCandidate{}
	for rows.Next() {
		var item dueCandidate
		if err := rows.Scan(&item.id, &item.name, &item.lastRuns); err != nil {
			return nil, err
		}
		candidates = append(candidates, item)
	}
	return candidates, rows.Err()
}

type cancellationCandidate struct {
	runID     int64
	reason    string
	attemptID int64
}

func (service *Service) cancellationCandidates(ctx context.Context, reader audit.Reader) ([]cancellationCandidate, error) {
	enabledKinds := map[string]bool{}
	for _, kind := range service.enabledDiscoverKinds() {
		enabledKinds[kind] = true
	}
	rows, err := reader.QueryContext(ctx, `
		SELECT r.id,c.type,c.enabled,c.revalidation_required,a.id
		FROM observation_runs r
		JOIN connections c ON c.id=r.connection_id
		JOIN execution_attempts a ON a.scope_type='observation_run' AND a.scope_id=r.id AND a.state IN ('Queued','Assigned','Running','Cancelling')
		WHERE r.state IN ('Queued','Running')
		ORDER BY r.id,a.id`)
	if err != nil {
		return nil, fmt.Errorf("list active source observation runs: %w", err)
	}
	defer rows.Close()
	var candidates []cancellationCandidate
	for rows.Next() {
		var item cancellationCandidate
		var connectionType string
		var enabled, revalidation int
		if err := rows.Scan(&item.runID, &connectionType, &enabled, &revalidation, &item.attemptID); err != nil {
			return nil, err
		}
		switch {
		case enabled != 1 || revalidation != 0:
			item.reason = "connection is disabled"
		case !enabledKinds[connectionType]:
			item.reason = "no enabled discover-capable plugin for this connection kind"
		default:
			continue
		}
		candidates = append(candidates, item)
	}
	return candidates, rows.Err()
}

// CancelUnobservable cancels active Runs whose connection was disabled or
// whose plugin lost deployment enablement. It returns the child attempt ids
// that need a runtime CancelAttempt dispatch kick; Queued children are fenced
// here, Running ones keep their terminal adjudication to the runtime path.
// The reconciliation is an audited system operation: called from the
// scheduler pass it reuses the pass's correlation, a direct caller gets a
// fresh explicit system scope.
func (service *Service) CancelUnobservable(ctx context.Context) ([]int64, error) {
	ctx, err := ensureSystemScope(ctx, execution.SourceScheduler)
	if err != nil {
		return nil, err
	}
	if err := requireObservationSystem(ctx, nil); err != nil {
		return nil, err
	}
	candidates, err := service.cancellationCandidates(ctx, service.runner.Reader())
	if err != nil || len(candidates) == 0 {
		return nil, err
	}
	// The scheduler's idle scan is not a business operation. A candidate only
	// opens the audited write window; its current eligibility is rechecked there.
	var cancelled []int64
	_, err = execution.Execute(ctx, service.runner, service.cancelOp, func(tx *execution.Tx) ([]int64, error) {
		victims, err := service.cancellationCandidates(ctx, tx)
		if err != nil {
			return nil, err
		}
		if len(victims) == 0 {
			return nil, execution.ErrNoTransition
		}
		seenRun := map[int64]string{}
		for _, item := range victims {
			seenRun[item.runID] = item.reason
		}
		cancelled = make([]int64, 0, len(victims))
		attempts := attempt.NewService(service.db)
		for _, item := range victims {
			if _, err := attempts.CancelFenceOn(ctx, tx, item.attemptID); err != nil {
				return nil, err
			}
			cancelled = append(cancelled, item.attemptID)
		}
		for runID, reason := range seenRun {
			if _, err := tx.ExecContext(ctx, `UPDATE observation_runs SET state='Cancelled',result_detail=?,row_version=row_version+1 WHERE id=? AND state IN ('Queued','Running')`, reason, runID); err != nil {
				return nil, err
			}
			// An object child that never received its result keeps an honest
			// gap reason; its attempt row carries the precise cancel state.
			if _, err := tx.ExecContext(ctx, `UPDATE observation_run_objects SET status='gap',gap_reason='cancelled' WHERE observation_run_id=? AND status='gap' AND gap_reason='runtime_unavailable'`, runID); err != nil {
				return nil, err
			}
		}
		return cancelled, nil
	}, func([]int64) int64 { return 0 })
	if errors.Is(err, execution.ErrNoTransition) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cancelled, nil
}
