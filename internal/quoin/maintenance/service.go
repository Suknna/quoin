// Package maintenance owns the SQL-authoritative maintenance aggregate.
package maintenance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

var (
	ErrConflict = errors.New("maintenance conflict")
	// ErrCommandReused is the shared runner identity: replaying a client
	// command id with a different request surfaces the runner's own error so
	// callers matching either value keep working.
	ErrCommandReused = execution.ErrCommandReused
)

// RejectionConflict is the stable runner rejection code for every
// deterministic maintenance conflict. The runner persists it durably
// (rejected_known ledger row plus rejected audit event) and the services map
// it back to the public ErrConflict so existing callers keep their contract.
const RejectionConflict = "maintenance_conflict"

// MapRejectionConflict translates a recorded maintenance-conflict rejection
// back into the public error. Any other error passes through verbatim.
func MapRejectionConflict(err error) error {
	var rejection *execution.Rejection
	if errors.As(err, &rejection) && rejection.Code == RejectionConflict {
		return fmt.Errorf("%w: %s", ErrConflict, rejection.Detail)
	}
	return err
}

// conflictRejection builds the deterministic conflict rejection the runner
// records for this maintenance aggregate.
func conflictRejection(detail string) *execution.Rejection {
	return &execution.Rejection{Code: RejectionConflict, Detail: detail}
}

// ConflictRejection is the exported constructor for sister services in this
// aggregate (upgrade prepare) whose conflicts share the same stable code and
// public-error mapping.
func ConflictRejection(detail string) *execution.Rejection {
	return conflictRejection(detail)
}

type State struct {
	Active     bool
	Reason     string
	RowVersion int64
	Items      []Item
}

type Item struct {
	Kind, ObjectKey, SafeState, DetailCode string
}

type ExitRequest struct {
	ActorID, ExpectedRowVersion     int64
	ExpectedReason, ClientCommandID string
}

type Service struct {
	db                *sql.DB
	runner            *execution.Runner
	exit              *execution.Operation
	adminPasswordSafe *execution.Operation
}

func NewService(db *sql.DB) *Service {
	service := &Service{db: db}
	service.runner = execution.NewRunner(db, execution.NewRegistry(), nil)
	register, err := service.runner.Register(execution.Operation{
		Name:       "maintenance.exit",
		Class:      execution.ClassWrite,
		ObjectType: "maintenance",
		// Exit is a user command: the shared in-transaction verification
		// re-checks the session proof reference (unrevoked, unexpired, current
		// revision, initialized, no pending password change, admin role).
		Authorize: func(ctx context.Context, tx *execution.Tx) error {
			return auth.VerifyExecutionSession(ctx, tx, "admin")
		},
	})
	if err != nil {
		panic(fmt.Sprintf("maintenance: register maintenance.exit: %v", err))
	}
	service.exit = register
	safe, err := service.runner.Register(execution.Operation{
		Name:       "maintenance.admin_password_safe",
		Class:      execution.ClassWrite,
		ObjectType: "maintenance",
		// The password-safe edge belongs to the user's own change-password
		// business operation, whose command just rotated the session — the
		// session proof reference in the metadata is intentionally stale here.
		// Authorization is therefore the actor's own identity and enabled
		// state; the Restore checklist item matching exacts the rest.
		Authorize: func(ctx context.Context, tx *execution.Tx) error {
			meta, err := execution.Require(ctx)
			if err != nil {
				return err
			}
			if meta.Actor.Kind != execution.PrincipalUser || meta.Actor.ID < 1 {
				return fmt.Errorf("maintenance: %s runs only as its owning user", "maintenance.admin_password_safe")
			}
			var enabled int
			if err := tx.QueryRowContext(ctx, `SELECT enabled FROM users WHERE id=?`, meta.Actor.ID).Scan(&enabled); err != nil {
				return err
			}
			if enabled != 1 {
				return fmt.Errorf("maintenance: the owning user %d is disabled", meta.Actor.ID)
			}
			return nil
		},
	})
	if err != nil {
		panic(fmt.Sprintf("maintenance: register maintenance.admin_password_safe: %v", err))
	}
	service.adminPasswordSafe = safe
	return service
}

func (service *Service) SetReader(reader execution.Reader) error {
	return service.runner.SetReader(reader)
}

func (service *Service) State(ctx context.Context) (State, error) {
	return stateOn(ctx, service.runner.Reader())
}

// MarkAdminPasswordSafe is the deterministic completion edge created only by a
// successful own-password change while Restore maintenance is active. It runs
// through the shared runner as one audited mutation: the audit event carries
// the owning user's actor and the change-password operation's correlation, and
// deterministic conflicts are recorded as rejections then mapped back to the
// public ErrConflict.
func (service *Service) MarkAdminPasswordSafe(ctx context.Context, actorID int64) error {
	if actorID < 1 {
		return fmt.Errorf("%w: required actor", ErrConflict)
	}
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.ID != actorID {
		return fmt.Errorf("%w: actor does not own the password-safe edge", ErrConflict)
	}
	_, err = execution.Execute(ctx, service.runner, service.adminPasswordSafe, func(tx *execution.Tx) (int64, error) {
		return service.markAdminPasswordSafeOn(ctx, tx, actorID)
	}, func(revision int64) int64 { return revision })
	return MapRejectionConflict(err)
}

func (service *Service) markAdminPasswordSafeOn(ctx context.Context, tx *execution.Tx, actorID int64) (int64, error) {
	var active int
	var reason string
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT active,COALESCE(reason,''),row_version FROM maintenance_state WHERE id=1`).Scan(&active, &reason, &revision); err != nil {
		return 0, err
	}
	if active == 0 {
		// The window already closed (a sister edge won the race): nothing to
		// mark, the invocation is still the recorded fact.
		return revision, nil
	}
	if reason != "Restore" {
		return revision, conflictRejection(fmt.Sprintf("maintenance %s is active", reason))
	}
	result, err := tx.ExecContext(ctx, `UPDATE maintenance_items SET safe_state='Safe',detail_code='password_changed',updated_at=? WHERE maintenance_revision=? AND kind='AdminPassword' AND object_key=? AND safe_state='Blocking'`, time.Now().UTC().Format(time.RFC3339Nano), revision, fmt.Sprint(actorID))
	if err != nil {
		return revision, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return revision, conflictRejection("no blocking AdminPassword checklist item for this user")
	}
	return revision, nil
}

// Exit atomically rechecks the caller, frozen revision and every checklist
// item. There is intentionally no force/skip path.
//
// The command runs through the shared execution runner: the verified session
// authorizes in-transaction, the ledger row (with its correlation) and the
// audit event are written automatically in the same transaction, and
// deterministic conflicts roll back and surface verbatim without any record —
// exactly the pre-runner contract.
func (service *Service) Exit(ctx context.Context, request ExitRequest) (State, error) {
	if request.ActorID < 1 || request.ExpectedRowVersion < 1 || request.ExpectedReason == "" || request.ClientCommandID == "" {
		return State{}, fmt.Errorf("%w: required request field", ErrConflict)
	}
	digest := auth.DigestCommand("maintenance.exit", map[string]any{"expectedReason": request.ExpectedReason, "expectedRowVersion": request.ExpectedRowVersion})
	outcome, err := execution.Run(ctx, service.runner, service.exit, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     request.ActorID,
		ClientCommandID: request.ClientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (State, execution.Change, error) {
		return service.exitOn(ctx, tx, request)
	}, func(state State) int64 { return state.RowVersion })
	if err != nil {
		return State{}, MapRejectionConflict(err)
	}
	// Legacy ledger rows (pre-runner releases) carry only {"exited":true}: the
	// state projection decodes with row version zero. The old replay contract
	// returns the live projection — preserve it.
	if outcome.Replayed && outcome.Result.RowVersion == 0 {
		return service.State(ctx)
	}
	return outcome.Result, nil
}

func (service *Service) exitOn(ctx context.Context, tx *execution.Tx, request ExitRequest) (State, execution.Change, error) {
	var active int
	var reason string
	var current int64
	if err := tx.QueryRowContext(ctx, `SELECT active,COALESCE(reason,''),row_version FROM maintenance_state WHERE id=1`).Scan(&active, &reason, &current); err != nil {
		return State{}, execution.Changed, err
	}
	if active != 1 || reason != request.ExpectedReason || current != request.ExpectedRowVersion {
		return State{}, execution.Changed, conflictRejection("maintenance window moved or is not exitable")
	}
	var total, blocking int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN safe_state='Blocking' THEN 1 ELSE 0 END),0) FROM maintenance_items WHERE maintenance_revision=?`, current).Scan(&total, &blocking); err != nil {
		return State{}, execution.Changed, err
	}
	if total == 0 || blocking != 0 {
		return State{}, execution.Changed, conflictRejection("checklist items are still blocking")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `UPDATE maintenance_state SET active=0,reason=NULL,entered_at=NULL,entered_by_type=NULL,entered_by_id=NULL,exited_at=?,exited_by_type='user',exited_by_id=?,row_version=row_version+1 WHERE id=1 AND active=1 AND row_version=?`, now, request.ActorID, current)
	if err != nil {
		return State{}, execution.Changed, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return State{}, execution.Changed, conflictRejection("maintenance window moved during exit")
	}
	state, err := stateOn(ctx, tx)
	if err != nil {
		return State{}, execution.Changed, err
	}
	return state, execution.Changed, nil
}

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// StateOn reads the SQL-authoritative state and frozen-revision checklist
// through an already-open connection so sister domain commands can return
// the same projection inside their own transaction boundary.
func StateOn(ctx context.Context, queryer rowQueryer) (State, error) {
	return stateOn(ctx, queryer)
}

func stateOn(ctx context.Context, queryer rowQueryer) (State, error) {
	var state State
	var active int
	if err := queryer.QueryRowContext(ctx, `SELECT active,COALESCE(reason,''),row_version FROM maintenance_state WHERE id=1`).Scan(&active, &state.Reason, &state.RowVersion); err != nil {
		return State{}, err
	}
	state.Active = active == 1
	if !state.Active {
		return state, nil
	}
	rows, err := queryer.QueryContext(ctx, `SELECT kind,object_key,safe_state,detail_code FROM maintenance_items WHERE maintenance_revision=? ORDER BY kind,object_key`, state.RowVersion)
	if err != nil {
		return State{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var item Item
		if err := rows.Scan(&item.Kind, &item.ObjectKey, &item.SafeState, &item.DetailCode); err != nil {
			return State{}, err
		}
		state.Items = append(state.Items, item)
	}
	if err := rows.Err(); err != nil {
		return State{}, err
	}
	return state, nil
}

// ---- Offline stopped-service operations (shared execution authority) ----
//
// Root-key rebind and the Lintel recovery finalizer run while the long-lived
// process is stopped, over their own verified database handle, so they cannot
// compose onto the live service runner. They still go through the same
// execution authority (ADR-0006): one registered write operation per command,
// one runner-owned IMMEDIATE transaction whose automatic audit row carries a
// real per-operation correlation and the CLI source, and a full rollback when
// anything — including the audit write — fails. The raw open/verify boundary
// outside the runner is the minimal controlled exception: the replacement
// root key must be verified against the database before the runner can trust
// that database at all (see rebind.go).

// offlineOperations holds the two offline operation declarations over one
// per-invocation registry. They never join the live service registry: the
// commands exist only inside their own stopped-CLI process.
type offlineOperations struct {
	rebind   *execution.Operation
	finalize *execution.Operation
}

// newOfflineRunner composes the offline runner over an already-verified
// database handle.
func newOfflineRunner(db *sql.DB) (*execution.Runner, offlineOperations) {
	registry := execution.NewRegistry()
	register := func(op execution.Operation) *execution.Operation {
		stored, err := registry.Register(op)
		if err != nil {
			// Static declarations: a duplicate or invalid name is a
			// programmer error that must surface immediately, not per command.
			panic(fmt.Sprintf("maintenance: register offline operation %q: %v", op.Name, err))
		}
		return stored
	}
	ops := offlineOperations{
		rebind: register(execution.Operation{
			Name:       "root_key.rebind",
			Class:      execution.ClassWrite,
			ObjectType: "maintenance",
			Authorize:  authorizeOfflineSystem,
		}),
		finalize: register(execution.Operation{
			Name:       "lintel_recovery.finalize",
			Class:      execution.ClassWrite,
			ObjectType: "maintenance",
			Authorize:  authorizeOfflineSystem,
		}),
	}
	return execution.NewRunner(db, registry, nil), ops
}

// authorizeOfflineSystem admits only the system principal entering through
// the CLI — the same pairing the offline administrator recovery requires. No
// HTTP, scheduler or task metadata can ever drive a stopped-service command.
func authorizeOfflineSystem(ctx context.Context, _ *execution.Tx) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
		return fmt.Errorf("maintenance: offline commands are reserved for the system principal")
	}
	if meta.Source.Kind != execution.SourceCLI {
		return fmt.Errorf("maintenance: offline command source must be cli, got %q", meta.Source.Kind)
	}
	return nil
}

// withOfflineMetadata attaches the fresh trusted-entry metadata an offline
// command executes under. Like the offline recovery it refuses a caller
// correlation: the stopped-process command is its own operation root, so no
// unrelated correlation can leak into its audit rows.
func withOfflineMetadata(ctx context.Context) (context.Context, error) {
	if _, exists := execution.FromContext(ctx); exists {
		return nil, fmt.Errorf("maintenance: offline commands require a context without execution metadata")
	}
	correlation, err := execution.NewCorrelationID()
	if err != nil {
		return nil, err
	}
	return execution.WithMetadata(ctx, execution.Metadata{
		CorrelationID: correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Source:        execution.Source{Kind: execution.SourceCLI},
	})
}
