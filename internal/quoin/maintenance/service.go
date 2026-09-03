// Package maintenance owns the SQL-authoritative maintenance aggregate.
package maintenance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/auth"
)

var (
	ErrConflict      = errors.New("maintenance conflict")
	ErrCommandReused = errors.New("maintenance command id reused")
)

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

type Service struct{ db *sql.DB }

func NewService(db *sql.DB) *Service { return &Service{db: db} }

func (service *Service) State(ctx context.Context) (State, error) {
	return stateOn(ctx, service.db)
}

// MarkAdminPasswordSafe is the deterministic completion edge created only by a
// successful own-password change while Restore maintenance is active.
func (service *Service) MarkAdminPasswordSafe(ctx context.Context, actorID int64) error {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var active int
	var reason string
	var revision int64
	if err := conn.QueryRowContext(ctx, `SELECT active,COALESCE(reason,''),row_version FROM maintenance_state WHERE id=1`).Scan(&active, &reason, &revision); err != nil {
		return err
	}
	if active == 0 {
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return err
		}
		committed = true
		return nil
	}
	if reason != "Restore" {
		return ErrConflict
	}
	result, err := conn.ExecContext(ctx, `UPDATE maintenance_items SET safe_state='Safe',detail_code='password_changed',updated_at=? WHERE maintenance_revision=? AND kind='AdminPassword' AND object_key=? AND safe_state='Blocking'`, time.Now().UTC().Format(time.RFC3339Nano), revision, fmt.Sprint(actorID))
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrConflict
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// Exit atomically rechecks the caller, frozen revision and every checklist
// item. There is intentionally no force/skip path.
func (service *Service) Exit(ctx context.Context, request ExitRequest) (State, error) {
	if request.ActorID < 1 || request.ExpectedRowVersion < 1 || request.ExpectedReason == "" || request.ClientCommandID == "" {
		return State{}, fmt.Errorf("%w: required request field", ErrConflict)
	}
	digest := auth.DigestCommand("maintenance.exit", map[string]any{"expectedReason": request.ExpectedReason, "expectedRowVersion": request.ExpectedRowVersion})
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return State{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return State{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if prior, found, err := auth.LookupCommandOn(ctx, conn, request.ActorID, request.ClientCommandID); err != nil {
		return State{}, err
	} else if found {
		if prior.CommandType != "maintenance.exit" || prior.RequestDigest != digest {
			return State{}, ErrCommandReused
		}
		if prior.Outcome == auth.OutcomeCommitted {
			return stateOn(ctx, conn)
		}
		return State{}, ErrConflict
	}
	var enabled int
	var role string
	if err := conn.QueryRowContext(ctx, `SELECT enabled,role FROM users WHERE id=?`, request.ActorID).Scan(&enabled, &role); err != nil || enabled != 1 || role != "admin" {
		return State{}, ErrConflict
	}
	var active int
	var reason string
	var current int64
	if err := conn.QueryRowContext(ctx, `SELECT active,COALESCE(reason,''),row_version FROM maintenance_state WHERE id=1`).Scan(&active, &reason, &current); err != nil {
		return State{}, err
	}
	if active != 1 || reason != request.ExpectedReason || current != request.ExpectedRowVersion || reason == "LintelRecovery" {
		return State{}, ErrConflict
	}
	var total, blocking int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN safe_state='Blocking' THEN 1 ELSE 0 END),0) FROM maintenance_items WHERE maintenance_revision=?`, current).Scan(&total, &blocking); err != nil {
		return State{}, err
	}
	if total == 0 || blocking != 0 {
		return State{}, ErrConflict
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := conn.ExecContext(ctx, `UPDATE maintenance_state SET active=0,reason=NULL,entered_at=NULL,entered_by_type=NULL,entered_by_id=NULL,exited_at=?,exited_by_type='user',exited_by_id=?,row_version=row_version+1 WHERE id=1 AND active=1 AND row_version=?`, now, request.ActorID, current)
	if err != nil {
		return State{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return State{}, ErrConflict
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO audit_events(actor_type,actor_id,action,client_command_id,outcome,domain_ref_type,domain_ref_id,created_at) VALUES('user',?,'maintenance.exit',?,'success','maintenance',?,?)`, request.ActorID, request.ClientCommandID, current, now); err != nil {
		return State{}, err
	}
	if err := auth.RecordCommand(ctx, conn, request.ActorID, request.ClientCommandID, "maintenance.exit", digest, auth.OutcomeCommitted, "maintenance", current, `{"exited":true}`); err != nil {
		return State{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return State{}, err
	}
	committed = true
	return stateOn(ctx, conn)
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
