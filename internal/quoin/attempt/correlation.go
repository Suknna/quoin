package attempt

// Business operation correlation persistence (ADR-0006 / audit phase 6):
// one opaque correlation identity plus its original initiator travels with
// every execution_attempts row. Quoin persists the authoritative association
// in the attempt-creating transaction and echoes only the correlation
// identity to the runtime on dispatch; every reply path joins attempts by id
// and never trusts a runtime-supplied correlation.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// ErrAttemptMissing reports a correlation operation against an
// execution_attempts row that does not exist.
var ErrAttemptMissing = errors.New("attempt: execution_attempts row does not exist")

// Correlation is the persisted business operation association of one
// attempt. InitiatorType mirrors the frozen column CHECK
// ('user'|'service'|'system') and stays empty when the row predates
// correlation or carries no row-scoped initiator.
type Correlation struct {
	OperationCorrelationID string
	InitiatorType          string
	InitiatorID            int64
}

// PersistCorrelationOn stores the execution metadata carried by ctx onto the
// attempt row: the operation correlation plus the original initiator (which
// may differ from the acting principal of this very step). It must run in
// the same transaction that creates the attempt (ADR-0006: create
// task/attempt and save correlation atomically) — CreateOn is the wired way
// to do both; direct calls are for the not-yet-centralized creators.
//
// The persisted association is immutable once set (ADR-0006): the UPDATE is
// guarded by `operation_correlation_id IS NULL`, so a repeat call with the
// same metadata is an idempotent no-op and a different identity is rejected
// — correlation can never be overwritten, re-rooted or forged after the
// fact. New operations fail closed: a context without execution metadata is
// an error (execution.ErrMissingContext), never an anonymous "assume
// system" fallback.
func PersistCorrelationOn(ctx context.Context, db execution.Executor, attemptID int64) error {
	if attemptID < 1 {
		return fmt.Errorf("attempt: persist correlation requires a positive attempt id, got %d", attemptID)
	}
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	// row_version +1 is mandatory: the frozen schema trigger rejects any
	// execution_attempts UPDATE that does not advance it exactly once. The
	// guard keeps the write to the single NULL → value transition, so
	// idempotent repeats never bump the row version at all.
	result, err := db.ExecContext(ctx,
		`UPDATE execution_attempts
		 SET operation_correlation_id=?, initiator_type=?, initiator_id=?, row_version=row_version+1
		 WHERE id=? AND operation_correlation_id IS NULL`,
		meta.CorrelationID, string(meta.Initiator.Kind), meta.Initiator.ID, attemptID)
	if err != nil {
		return fmt.Errorf("attempt: persist correlation on attempt %d: %w", attemptID, err)
	}
	if affected, rowsErr := result.RowsAffected(); rowsErr == nil && affected == 1 {
		return nil
	}
	// Either the row is missing or the association is already set: compare
	// explicitly so an idempotent repeat succeeds and any different identity
	// is rejected loudly.
	stored, _, loadErr := LoadCorrelation(ctx, db, attemptID)
	if loadErr != nil {
		return loadErr
	}
	if stored.OperationCorrelationID == meta.CorrelationID &&
		stored.InitiatorType == string(meta.Initiator.Kind) &&
		stored.InitiatorID == meta.Initiator.ID {
		return nil
	}
	return fmt.Errorf("attempt: attempt %d already carries correlation %q (initiator %s/%d); the persisted association is immutable (ADR-0006)",
		attemptID, stored.OperationCorrelationID, stored.InitiatorType, stored.InitiatorID)
}

// CreateOn creates one execution attempt inside the caller's transaction and
// automatically persists the correlation metadata carried by ctx onto the
// new row — the centralized creation path (ADR-0006: create task/attempt and
// save the correlation, initiator and source in one transaction). The INSERT
// runs first, the new row id is captured, then the correlation is persisted;
// any failure fails the whole creation so no attempt can ever exist without
// its association. INSERT-level errors (validation triggers, unique scopes)
// are returned raw so creator-specific mappings (e.g. busy classification)
// stay with the caller.
func CreateOn(ctx context.Context, db execution.Executor, insertQuery string, insertArgs ...any) (int64, error) {
	result, err := db.ExecContext(ctx, insertQuery, insertArgs...)
	if err != nil {
		return 0, err
	}
	attemptID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("attempt: create: last insert id: %w", err)
	}
	if err := PersistCorrelationOn(ctx, db, attemptID); err != nil {
		return 0, err
	}
	return attemptID, nil
}

// LoadCorrelation reads the persisted association. The second return value
// is the explicit legacy marker: rows created before correlation existed
// keep NULL columns and report (Correlation{}, false, nil) — absent is a
// fact that must stay visible, never a fabricated identity and never an
// error. A missing row, in contrast, is ErrAttemptMissing.
func LoadCorrelation(ctx context.Context, db audit.Reader, attemptID int64) (Correlation, bool, error) {
	var (
		correlation   sql.NullString
		initiatorType sql.NullString
		initiatorID   sql.NullInt64
	)
	err := db.QueryRowContext(ctx,
		`SELECT operation_correlation_id, initiator_type, initiator_id FROM execution_attempts WHERE id=?`,
		attemptID).Scan(&correlation, &initiatorType, &initiatorID)
	if errors.Is(err, sql.ErrNoRows) {
		return Correlation{}, false, ErrAttemptMissing
	}
	if err != nil {
		return Correlation{}, false, fmt.Errorf("attempt: load correlation on attempt %d: %w", attemptID, err)
	}
	if !correlation.Valid {
		return Correlation{}, false, nil
	}
	return Correlation{
		OperationCorrelationID: correlation.String,
		InitiatorType:          initiatorType.String,
		InitiatorID:            initiatorID.Int64,
	}, true, nil
}

// restoreLifecycleAuthority was replaced by lifecycleAuthority
// (runner.go): the machine identity of a standalone stage is resolved from
// the persisted association BEFORE the runner transaction opens, because the
// runner captures execution metadata at entry and owns the audit row.

// Lifecycle audit vocabulary of the shared attempt machine (ADR-0006): the
// standalone (transaction-owning) stages are registered runner operations
// whose names are these facts — the automatic audit rows keep the same
// action identity. ObjectType values name the primary row the stage wrote.
// The transaction-composable On stages record nothing themselves: their
// callers compose them inside their own audited executor operations.
const (
	auditActionModelCallBegin    = "attempt.model_call.begin"
	auditActionModelCallComplete = "attempt.model_call.complete"
	auditActionToolCallBegin     = "attempt.tool_call.begin"
	auditActionToolCallCancel    = "attempt.tool_call.cancel_pending"
	auditActionToolCallComplete  = "attempt.tool_call.complete"
	auditActionCancelFence       = "attempt.cancel_fence"
	auditActionDispatchBind      = "attempt.dispatch.bind"
	auditActionDispatchAccept    = "attempt.dispatch.accept"
	auditActionResultCommit      = "attempt.result.commit"
	auditActionCancelAck         = "attempt.cancel_ack"
	auditActionInterrupt         = "attempt.interrupt"
	auditActionLeaseSweep        = "attempt.lease_sweep"

	auditRefExecutionAttempt = "execution_attempt"
	auditRefModelCall        = "model_call"
	auditRefToolCall         = "tool_call"
)
