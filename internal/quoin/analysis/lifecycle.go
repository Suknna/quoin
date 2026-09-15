package analysis

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// Attempt lifecycle: technical-failure retry, the cancellation fence, the
// dispatch accept gate, the first-success seal and failure bookkeeping.
// Retry preserves the terminal failed analysis and creates a fresh analysis
// for its occurrence. The schema intentionally makes terminal analyses
// immutable, so re-opening the old row or copying its historical attempt would
// both violate the audit boundary. Create re-resolves the currently qualified
// provider and renders a new immutable input snapshot, allowing an operator to
// retry after a repaired provider/configuration or repaired runtime software.
//
// 用户命令（retry/cancel）走执行器 Run（持久台账+自动审计）；全部生命周期
// 变更（接受、取消确认、丢失收敛、成功封存、失败记账）走执行器 Execute，
// 任务作用域从 attempt 行的持久关联恢复（attempt.LoadCorrelation），自动审计
// 由执行器落库。平台故障投影通过 ProjectTerminalOutcome 钩子在执行器事务内
// 完成，提交序使用执行器实际插入的审计事件 id，平台
// 状态与 attempt 状态跨进程崩溃保持全有或全无。
func (service *Service) Retry(ctx context.Context, analysisID, principalID int64, clientCommandID string) (CreateResult, error) {
	var occurrenceID int64
	var state string
	if err := service.runner.Reader().QueryRowContext(ctx, `SELECT occurrence_id,state FROM initial_analyses WHERE id=?`, analysisID).Scan(&occurrenceID, &state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CreateResult{}, ErrNotFound
		}
		return CreateResult{}, err
	}
	if state != "Failed" && state != "Interrupted" {
		return CreateResult{}, fmt.Errorf("%w: analysis %d is %s", ErrActiveConflict, analysisID, state)
	}
	return service.create(ctx, occurrenceID, principalID, clientCommandID, "retry", analysisID)
}

// errSealedResultReplayed marks the idempotent re-delivery of an already
// sealed terminal verdict (RUNTIME-TASK-008). It is a plain skip sentinel,
// never a durable fact: the losing redelivery changed nothing, so the runner
// rolls the no-op transaction back and records nothing.
var errSealedResultReplayed = errors.New("analysis result already sealed")

// CancelOutcome classifies what the cancellation fence decided.
type CancelOutcome struct {
	State      string // current analysis state after the fence
	RowVersion int64
	// AttemptID identifies the attempt the fence touched (0 when the
	// analysis was already terminal).
	AttemptID int64
	// DispatchRequired is true when the fence moved an Assigned or Running
	// attempt to Cancelling: the app layer must send CancelAttempt to the runtime
	// (RUNTIME-CANCEL-001). Replayed commands never re-dispatch.
	DispatchRequired bool `json:",omitempty"`
}

// Cancel commits the idempotent cancellation fence (DATA-ATTEMPT-003,
// HTTP-COMMAND-005) as one durable command: the runner transaction holds the
// fence, the command ledger row and the automatic audit event. Success
// already committed wins by SQLite order; otherwise the active attempt
// closes to Cancelled/Cancelling and the analysis follows on CancelAck.
func (service *Service) Cancel(ctx context.Context, analysisID, principalID, expectedRowVersion int64, clientCommandID string) (CancelOutcome, error) {
	// dispatch is the post-commit runtime-notification decision of the FIRST
	// execution; it never enters the replayable ledger payload, so a replay
	// never re-dispatches the runtime cancel.
	dispatch := false
	outcome, err := execution.Run(ctx, service.runner, service.opCancel, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest:          auth.DigestCommand("cancel", map[string]any{"analysisId": analysisID, "expectedRowVersion": expectedRowVersion}),
	}, func(tx *execution.Tx) (CancelOutcome, execution.Change, error) {
		var state string
		var rowVersion int64
		if err := tx.QueryRowContext(ctx, `SELECT state,row_version FROM initial_analyses WHERE id=?`, analysisID).Scan(&state, &rowVersion); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return CancelOutcome{}, execution.Unchanged, ErrNotFound
			}
			return CancelOutcome{}, execution.Unchanged, err
		}
		if rowVersion != expectedRowVersion {
			return CancelOutcome{}, execution.Unchanged, &RowVersionError{Current: rowVersion}
		}
		switch state {
		case "Succeeded":
			// Success won the commit-order race: report the completed object.
			return CancelOutcome{State: "Succeeded", RowVersion: rowVersion}, execution.Unchanged, nil
		case "Failed", "Cancelled", "Interrupted":
			return CancelOutcome{}, execution.Unchanged, fmt.Errorf("%w: analysis %d is %s", ErrActiveConflict, analysisID, state)
		}
		var attemptID int64
		if err := tx.QueryRowContext(ctx, `
			SELECT id FROM execution_attempts
			WHERE scope_type='analysis' AND scope_id=? AND state IN ('Queued','Assigned','Running','Cancelling')`,
			analysisID).Scan(&attemptID); err != nil {
			return CancelOutcome{}, execution.Unchanged, err
		}
		// The fence must run on this transaction: SQLite is single-writer and
		// a nested pool fetch would deadlock (the executor-scoped mirror of
		// the shared attempt machine's CancelFenceOn).
		attemptState, err := service.cancelFenceOn(ctx, tx, attemptID)
		if err != nil {
			return CancelOutcome{}, execution.Unchanged, err
		}
		nextState := state
		result := CancelOutcome{AttemptID: attemptID}
		if attemptState == "Cancelled" {
			nextState = "Cancelled"
			if _, err := tx.ExecContext(ctx, `
				UPDATE initial_analyses SET state='Cancelled', row_version=row_version+1
				WHERE id=? AND state=?`, analysisID, state); err != nil {
				return CancelOutcome{}, execution.Unchanged, err
			}
			rowVersion++
		} else {
			// Cancelling: the analysis stays Running until CancelAck; the app
			// layer dispatches CancelAttempt after this fence commits.
			nextState = "Running"
			dispatch = true
		}
		result.State = nextState
		result.RowVersion = rowVersion
		return result, execution.Changed, nil
	}, func(outcome CancelOutcome) int64 { return outcome.AttemptID })
	if err != nil {
		if errors.Is(err, execution.ErrCommandReused) {
			return CancelOutcome{}, ErrCommandReplayMismatch
		}
		return CancelOutcome{}, err
	}
	if !outcome.Replayed {
		outcome.Result.DispatchRequired = dispatch
	}
	return outcome.Result, nil
}

// cancelFenceOn is the executor-scoped cancellation fence, mirroring the
// shared attempt machine's CancelFenceOn state machine (the attempt package
// exposes it only on *sql.Conn; the runner's guarded transaction composes
// through the local writer surface). Queued closes as Cancelled directly;
// Assigned/Running close to Cancelling because an Assigned DispatchAttempt
// may already be in flight; terminal attempts are returned unchanged.
func (service *Service) cancelFenceOn(ctx context.Context, tx writer, attemptID int64) (string, error) {
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&state); err != nil {
		return "", err
	}
	switch state {
	case "Succeeded", "Failed", "Cancelled", "Interrupted":
		return state, nil
	case "Queued":
		result, err := tx.ExecContext(ctx, `
			UPDATE execution_attempts
			SET state='Cancelled', ended_at=?, termination_reason='cancelled', row_version=row_version+1
			WHERE id=? AND state=?`, service.nowText(), attemptID, state)
		if err != nil {
			return "", err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return "", fmt.Errorf("attempt %d cancellation fence lost the race", attemptID)
		}
		return "Cancelled", nil
	case "Assigned", "Running", "Cancelling":
		// Assigned is a dispatch-commit state, not proof that the runtime has
		// not started: it receives the same durable cancellation and replay
		// treatment as Running (RUNTIME-CANCEL-001). Whether the UPDATE won
		// or the attempt was already Cancelling, the state is the same (the
		// fence is idempotent).
		if _, err := tx.ExecContext(ctx, `
			UPDATE execution_attempts SET state='Cancelling', row_version=row_version+1
			WHERE id=? AND state IN ('Assigned','Running')`, attemptID); err != nil {
			return "", err
		}
		return "Cancelling", nil
	default:
		return "", fmt.Errorf("attempt %d has unknown state %q", attemptID, state)
	}
}

// AcceptAttempt records Assigned -> Running and moves the analysis to
// Running (RUNTIME-TASK-004) as one audited system fact. The attempt and
// analysis rows close in the same runner transaction (the frozen state
// machine stays legal regardless of order; committing both together removes
// the accept crash window the reconciler used to repair).
func (service *Service) AcceptAttempt(ctx context.Context, attemptID int64, bootID string, epoch uint64) error {
	lifecycleCtx, err := service.lifecycleContext(ctx, attemptID)
	if err != nil {
		return err
	}
	type accepted struct {
		AnalysisID int64
	}
	_, err = execution.Execute(lifecycleCtx, service.runner, service.opAccepted, func(tx *execution.Tx) (accepted, error) {
		// The fence matches the dispatch boot; the binding epoch is
		// immutable once set (the shared attempt machine's Accept fence).
		result, err := tx.ExecContext(lifecycleCtx, `
			UPDATE execution_attempts
			SET state='Running', accepted_at=?, started_at=?, row_version=row_version+1
			WHERE id=? AND state='Assigned' AND boot_id=?`,
			service.nowText(), service.nowText(), attemptID, bootID)
		if err != nil {
			return accepted{}, err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return accepted{}, fmt.Errorf("attempt %d acceptance refused (not Assigned or binding mismatch)", attemptID)
		}
		var scopeID int64
		if err := tx.QueryRowContext(lifecycleCtx, `SELECT scope_id FROM execution_attempts WHERE id=?`, attemptID).Scan(&scopeID); err != nil {
			return accepted{}, err
		}
		result2, err := tx.ExecContext(lifecycleCtx, `
			UPDATE initial_analyses SET state='Running', row_version=row_version+1
			WHERE id=? AND state='Queued'`, scopeID)
		if err != nil {
			return accepted{}, err
		}
		if affected, _ := result2.RowsAffected(); affected != 1 {
			return accepted{}, fmt.Errorf("analysis %d is not Queued; acceptance refused", scopeID)
		}
		return accepted{AnalysisID: scopeID}, nil
	}, func(value accepted) int64 { return value.AnalysisID })
	return err
}

// Result is the adjudicated ResultProposal payload.
type Result struct {
	AttemptID   int64
	BootID      string
	Epoch       uint64
	Succeeded   bool
	Termination string
	SchemaKind  string
	Canonical   []byte
	Digest      []byte
	EvidenceIDs []int64
	ArtifactIDs []int64
}

// CommitResult seals the first legal success (output row + references +
// attempt + analysis terminal state in one runner transaction with the
// automatic audit event, DATA-ANALYSIS-002) or records the attempt failure.
// Late results lose against the cancellation fence (DATA-TX-005).
func (service *Service) CommitResult(ctx context.Context, result Result) error {
	if !result.Succeeded {
		return service.commitFailure(ctx, result)
	}
	if result.SchemaKind != OutputSchemaKind {
		return fmt.Errorf("unexpected result schema kind %q", result.SchemaKind)
	}
	digest := sha256.Sum256(result.Canonical)
	if hex.EncodeToString(digest[:]) != hex.EncodeToString(result.Digest) {
		return fmt.Errorf("result content digest mismatch")
	}
	var content string
	if err := json.Unmarshal(result.Canonical, &content); err != nil {
		return fmt.Errorf("analysis output must be a JSON string: %w", err)
	}
	lifecycleCtx, err := service.lifecycleContext(ctx, result.AttemptID)
	if err != nil {
		return err
	}
	type sealed struct {
		AnalysisID int64
	}
	_, err = execution.Execute(lifecycleCtx, service.runner, service.opSucceeded, func(tx *execution.Tx) (sealed, error) {
		var attemptType, scopeType string
		var scopeID int64
		var state string
		if err := tx.QueryRowContext(lifecycleCtx, `SELECT attempt_type,scope_type,scope_id,state FROM execution_attempts WHERE id=?`, result.AttemptID).
			Scan(&attemptType, &scopeType, &scopeID, &state); err != nil {
			return sealed{}, err
		}
		if attemptType != "initial_analysis" || scopeType != "analysis" {
			return sealed{}, fmt.Errorf("attempt %d is not an initial analysis attempt", result.AttemptID)
		}
		if state != "Running" {
			// A replay of an already-adjudicated result rebuilds the original
			// verdict instead of surfacing a late-result error (RUNTIME-TASK-008
			// idempotent adjudication; the runtime retries its terminal
			// proposal until an ack survives). The no-op redelivery records
			// nothing; a divergent result stays a late result.
			return sealed{}, service.replaySealedResult(lifecycleCtx, tx, result, state)
		}
		var bootID string
		var epoch int64
		var leaseUntil string
		if err := tx.QueryRowContext(lifecycleCtx, `SELECT boot_id,connection_epoch,lease_until FROM execution_attempts WHERE id=?`, result.AttemptID).Scan(&bootID, &epoch, &leaseUntil); err != nil {
			return sealed{}, err
		}
		if bootID != result.BootID || epoch != int64(result.Epoch) {
			return sealed{}, ErrLateResult
		}
		// RUNTIME-TASK-008: a result whose lease already burned down must not
		// produce a valid domain output even if the sweeper has not yet
		// converged the row (the sweep window is seconds; the lease is the
		// authority). Audit only.
		if leaseDeadline, err := time.Parse(time.RFC3339Nano, leaseUntil); err != nil || !service.now().Before(leaseDeadline) {
			return sealed{}, ErrLateResult
		}
		var modelID string
		if err := tx.QueryRowContext(lifecycleCtx, `
			SELECT model_id FROM model_calls WHERE attempt_id=? AND status='succeeded'
			ORDER BY id DESC LIMIT 1`, result.AttemptID).Scan(&modelID); err != nil {
			return sealed{}, fmt.Errorf("result without a succeeded model call: %w", err)
		}
		if len(result.ArtifactIDs) > 0 || len(result.EvidenceIDs) > 0 {
			// T11 closes the Evidence/Artifact references (DATA-ANALYSIS-002):
			// every reference must belong to this attempt before the output
			// seals them; stray locators keep the seal deterministic.
			if err := validateReferences(lifecycleCtx, tx, result); err != nil {
				return sealed{}, fmt.Errorf("evidence/artifact references do not close: %w", err)
			}
		}
		now := service.nowText()
		outputInsert, err := tx.ExecContext(lifecycleCtx, `
			INSERT INTO initial_analysis_outputs(analysis_id,attempt_id,model_id,content,created_at)
			VALUES(?,?,?,?,?)`, scopeID, result.AttemptID, modelID, content, now)
		if err != nil {
			return sealed{}, err
		}
		outputID, err := outputInsert.LastInsertId()
		if err != nil {
			return sealed{}, err
		}
		for ordinal, evidenceID := range result.EvidenceIDs {
			if _, err := tx.ExecContext(lifecycleCtx, `
				INSERT INTO initial_analysis_output_evidence(output_id,evidence_id,ordinal)
				VALUES(?,?,?)`, outputID, evidenceID, ordinal); err != nil {
				return sealed{}, err
			}
		}
		for ordinal, artifactID := range result.ArtifactIDs {
			if _, err := tx.ExecContext(lifecycleCtx, `
				INSERT INTO initial_analysis_output_artifacts(output_id,artifact_id,ordinal)
				VALUES(?,?,?)`, outputID, artifactID, ordinal); err != nil {
				return sealed{}, err
			}
		}
		if _, err := tx.ExecContext(lifecycleCtx, `
			UPDATE execution_attempts
			SET state='Succeeded', ended_at=?, row_version=row_version+1
			WHERE id=? AND state='Running' AND boot_id=? AND connection_epoch=?`,
			now, result.AttemptID, result.BootID, result.Epoch); err != nil {
			return sealed{}, err
		}
		// Crash-window repair: the accept's two transactions can die between
		// them (attempt Running, analysis still Queued). The attempt state is
		// the authority — promote the analysis in the same transaction so the
		// seal never strands a Queued analysis over a terminal attempt.
		if _, err := tx.ExecContext(lifecycleCtx, `
			UPDATE initial_analyses SET state='Running', row_version=row_version+1
			WHERE id=? AND state='Queued'`, scopeID); err != nil {
			return sealed{}, err
		}
		if _, err := tx.ExecContext(lifecycleCtx, `
			UPDATE initial_analyses SET state='Succeeded', row_version=row_version+1
			WHERE id=? AND state='Running'`, scopeID); err != nil {
			return sealed{}, err
		}
		// The platform-fault projection runs inside the runner transaction,
		// atomically with the terminal transition and its automatic audit
		// event (a successful ResultAck never outlives a missing projection).
		if service.ProjectTerminalOutcome != nil {
			if err := tx.AfterAudit(func(ctx context.Context, tx *execution.Tx, eventID int64) error {
				return service.ProjectTerminalOutcome(ctx, tx, eventID, true, "")
			}); err != nil {
				return sealed{}, err
			}
		}
		return sealed{AnalysisID: scopeID}, nil
	}, func(value sealed) int64 { return value.AnalysisID })
	if err != nil {
		if errors.Is(err, errSealedResultReplayed) {
			return nil
		}
		return err
	}
	return nil
}

func (service *Service) commitFailure(ctx context.Context, result Result) error {
	lifecycleCtx, err := service.lifecycleContext(ctx, result.AttemptID)
	if err != nil {
		return err
	}
	type failed struct {
		AnalysisID int64
	}
	_, err = execution.Execute(lifecycleCtx, service.runner, service.opFailed, func(tx *execution.Tx) (failed, error) {
		var attemptType, scopeType string
		var scopeID int64
		if err := tx.QueryRowContext(lifecycleCtx, `SELECT attempt_type,scope_type,scope_id FROM execution_attempts WHERE id=?`, result.AttemptID).
			Scan(&attemptType, &scopeType, &scopeID); err != nil {
			return failed{}, err
		}
		if attemptType != "initial_analysis" || scopeType != "analysis" {
			return failed{}, fmt.Errorf("attempt %d is not an initial analysis attempt", result.AttemptID)
		}
		var attemptState string
		if err := tx.QueryRowContext(lifecycleCtx, `SELECT state FROM execution_attempts WHERE id=?`, result.AttemptID).Scan(&attemptState); err != nil {
			return failed{}, err
		}
		if attemptState != "Running" {
			// Same replay contract as the success seal (RUNTIME-TASK-008):
			// an identical failure replays its original verdict; the no-op
			// redelivery records nothing.
			return failed{}, service.replaySealedResult(lifecycleCtx, tx, result, attemptState)
		}
		// RUNTIME-TASK-008: a burned-down lease rejects the result (audit only)
		// even before the sweeper converges the row.
		var leaseUntil string
		if err := tx.QueryRowContext(lifecycleCtx, `SELECT lease_until FROM execution_attempts WHERE id=?`, result.AttemptID).Scan(&leaseUntil); err != nil {
			return failed{}, err
		}
		if leaseDeadline, err := time.Parse(time.RFC3339Nano, leaseUntil); err != nil || !service.now().Before(leaseDeadline) {
			return failed{}, ErrLateResult
		}
		now := service.nowText()
		reason := result.Termination
		if reason == "" {
			reason = "worker_protocol_error"
		}
		attemptUpdate, err := tx.ExecContext(lifecycleCtx, `
			UPDATE execution_attempts
			SET state='Failed', ended_at=?, termination_reason=?, row_version=row_version+1
			WHERE id=? AND state='Running' AND boot_id=? AND connection_epoch=?`,
			now, reason, result.AttemptID, result.BootID, result.Epoch)
		if err != nil {
			return failed{}, err
		}
		if affected, _ := attemptUpdate.RowsAffected(); affected != 1 {
			return failed{}, ErrLateResult
		}
		if _, err := tx.ExecContext(lifecycleCtx, `
			UPDATE initial_analyses SET state='Failed', row_version=row_version+1
			WHERE id=? AND state IN ('Queued','Running')`, scopeID); err != nil {
			return failed{}, err
		}
		// The platform-fault projection is the only current reachable
		// worker-launch failure authority: it must not be separable from the
		// committed attempt state (same transaction, same commit).
		if service.ProjectTerminalOutcome != nil {
			if err := tx.AfterAudit(func(ctx context.Context, tx *execution.Tx, eventID int64) error {
				return service.ProjectTerminalOutcome(ctx, tx, eventID, false, reason)
			}); err != nil {
				return failed{}, err
			}
		}
		return failed{AnalysisID: scopeID}, nil
	}, func(value failed) int64 { return value.AnalysisID })
	if err != nil {
		if errors.Is(err, errSealedResultReplayed) {
			return nil
		}
		return err
	}
	return nil
}

// CancelAck finishes the analysis cancellation once the runtime confirmed
// the attempt stopped (RUNTIME-CANCEL-003) as one audited system fact.
// Idempotent: an attempt the loss convergence already closed as Cancelled
// still converges its analysis (a late duplicate ack must not strand the
// analysis Running).
func (service *Service) CancelAck(ctx context.Context, attemptID int64) error {
	lifecycleCtx, err := service.lifecycleContext(ctx, attemptID)
	if err != nil {
		return err
	}
	type acked struct {
		AnalysisID int64
	}
	_, err = execution.Execute(lifecycleCtx, service.runner, service.opCancelAck, func(tx *execution.Tx) (acked, error) {
		// The shared attempt machine's CancelAck transition on this
		// transaction (Cancelling -> Cancelled); an already-Cancelled attempt
		// is tolerated so the analysis still converges.
		result, err := tx.ExecContext(lifecycleCtx, `
			UPDATE execution_attempts
			SET state='Cancelled', ended_at=?, termination_reason='cancelled', row_version=row_version+1
			WHERE id=? AND state='Cancelling'`, service.nowText(), attemptID)
		if err != nil {
			return acked{}, err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			var state string
			if err := tx.QueryRowContext(lifecycleCtx, `SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&state); err != nil {
				return acked{}, err
			}
			if state != "Cancelled" {
				return acked{}, fmt.Errorf("attempt %d is not Cancelling", attemptID)
			}
		}
		var scopeID int64
		if err := tx.QueryRowContext(lifecycleCtx, `SELECT scope_id FROM execution_attempts WHERE id=?`, attemptID).Scan(&scopeID); err != nil {
			return acked{}, err
		}
		if _, err := tx.ExecContext(lifecycleCtx, `
			UPDATE initial_analyses SET state='Cancelled', row_version=row_version+1
			WHERE id=? AND state IN ('Queued','Running')`, scopeID); err != nil {
			return acked{}, err
		}
		return acked{AnalysisID: scopeID}, nil
	}, func(value acked) int64 { return value.AnalysisID })
	return err
}

// CommitInterruption closes one attempt with its loss reason and moves the
// analysis to the matching terminal state in the same transaction
// (RUNTIME-TASK-006): Interrupted analyses stay inspectable and the
// occurrence becomes eligible for a fresh analysis; a Cancelling attempt
// converges to Cancelled (the fence exception). The audited operation is
// selected from the pre-read state so the automatic audit keeps the frozen
// action vocabulary; an already-terminal attempt converges nothing and
// records nothing.
func (service *Service) CommitInterruption(ctx context.Context, attemptID int64, reason string) error {
	if !attempt.LossReasons[reason] {
		return fmt.Errorf("attempt %d loss reason %q is not a closed interruption reason", attemptID, reason)
	}
	var state string
	if err := service.runner.Reader().QueryRowContext(ctx, `SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&state); err != nil {
		return err
	}
	switch state {
	case "Succeeded", "Failed", "Cancelled", "Interrupted":
		// Loss raced a terminal result: the result keeps the state it won.
		return nil
	case "Cancelling":
		state = "converge-cancelled"
	default:
	}
	op := service.opInterrupted
	if state == "converge-cancelled" {
		op = service.opCancelled
	}
	lifecycleCtx, err := service.lifecycleContext(ctx, attemptID)
	if err != nil {
		return err
	}
	type converged struct {
		AnalysisID int64
	}
	_, err = execution.Execute(lifecycleCtx, service.runner, op, func(tx *execution.Tx) (converged, error) {
		final, err := service.interruptOn(lifecycleCtx, tx, attemptID, reason)
		if err != nil {
			return converged{}, err
		}
		var scopeID int64
		if err := tx.QueryRowContext(lifecycleCtx, `SELECT scope_id FROM execution_attempts WHERE id=?`, attemptID).Scan(&scopeID); err != nil {
			return converged{}, err
		}
		nextState := "Interrupted"
		if final == "Cancelled" {
			nextState = "Cancelled"
		}
		if _, err := tx.ExecContext(lifecycleCtx, `
			UPDATE initial_analyses SET state=?, row_version=row_version+1
			WHERE id=? AND state IN ('Queued','Running')`, nextState, scopeID); err != nil {
			return converged{}, err
		}
		return converged{AnalysisID: scopeID}, nil
	}, func(value converged) int64 { return value.AnalysisID })
	return err
}

// interruptOn is the executor-scoped mirror of the shared attempt machine's
// InterruptOn: Assigned/Running close as Interrupted with the loss reason;
// an attempt whose cancellation fence already committed converges to
// Cancelled instead (the fence exception).
func (service *Service) interruptOn(ctx context.Context, tx writer, attemptID int64, reason string) (string, error) {
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&state); err != nil {
		return "", err
	}
	now := service.nowText()
	switch state {
	case "Succeeded", "Failed", "Cancelled", "Interrupted":
		// Loss raced a terminal result: the result keeps the state it won.
		return state, nil
	case "Cancelling":
		// The cancellation fence already committed; loss converges it to
		// Cancelled (RUNTIME-TASK-006 fence exception).
		result, err := tx.ExecContext(ctx, `
			UPDATE execution_attempts
			SET state='Cancelled', ended_at=?, termination_reason='cancelled', row_version=row_version+1
			WHERE id=? AND state='Cancelling'`, now, attemptID)
		if err != nil {
			return "", err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return "", fmt.Errorf("attempt %d loss convergence lost the race", attemptID)
		}
		return "Cancelled", nil
	case "Queued", "Assigned", "Running":
		result, err := tx.ExecContext(ctx, `
			UPDATE execution_attempts
			SET state='Interrupted', ended_at=?, termination_reason=?, row_version=row_version+1
			WHERE id=? AND state=?`, now, reason, attemptID, state)
		if err != nil {
			return "", err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return "", fmt.Errorf("attempt %d interruption lost the race", attemptID)
		}
		return "Interrupted", nil
	default:
		return "", fmt.Errorf("attempt %d has unknown state %q", attemptID, state)
	}
}

// validateReferences re-checks the sealed output's reference closure on
// the caller's transaction (DATA-ANALYSIS-002): every Evidence must belong
// to this attempt, every Artifact must be granted to this attempt through
// its own tool results, and neither list may repeat.
func validateReferences(ctx context.Context, tx audit.Reader, result Result) error {
	seenEvidence := map[int64]bool{}
	for _, evidenceID := range result.EvidenceIDs {
		if evidenceID <= 0 || seenEvidence[evidenceID] {
			return fmt.Errorf("evidence %d is not a unique positive locator", evidenceID)
		}
		seenEvidence[evidenceID] = true
		var attemptID int64
		if err := tx.QueryRowContext(ctx, `SELECT attempt_id FROM evidence WHERE id=?`, evidenceID).Scan(&attemptID); err != nil {
			return fmt.Errorf("evidence %d: %w", evidenceID, err)
		}
		if attemptID != result.AttemptID {
			return fmt.Errorf("evidence %d belongs to attempt %d, not %d", evidenceID, attemptID, result.AttemptID)
		}
	}
	seenArtifacts := map[int64]bool{}
	for _, artifactID := range result.ArtifactIDs {
		if artifactID <= 0 || seenArtifacts[artifactID] {
			return fmt.Errorf("artifact %d is not a unique positive locator", artifactID)
		}
		seenArtifacts[artifactID] = true
		var granted int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM attempt_artifact_grants WHERE attempt_id=? AND artifact_id=?`,
			result.AttemptID, artifactID).Scan(&granted); err != nil {
			return err
		}
		if granted != 1 {
			return fmt.Errorf("artifact %d is not granted to attempt %d", artifactID, result.AttemptID)
		}
	}
	return nil
}

// replaySealedResult rebuilds the original verdict for one terminal
// attempt: an identical success (this attempt's sealed output row matches
// the proposal bytes) or an identical failure (same termination reason)
// replays as success so the runtime's reliable-delivery retry observes the
// original ResultAck (RUNTIME-TASK-008); anything else stays a late result
// (audit only, DATA-ATTEMPT-004).
func (service *Service) replaySealedResult(ctx context.Context, tx audit.Reader, result Result, state string) error {
	if !result.Succeeded {
		if state == "Failed" {
			var sealed string
			if err := tx.QueryRowContext(ctx, `SELECT termination_reason FROM execution_attempts WHERE id=?`, result.AttemptID).Scan(&sealed); err != nil {
				return err
			}
			if sealed == result.Termination || (sealed == "worker_protocol_error" && result.Termination == "") {
				return errSealedResultReplayed
			}
		}
		return ErrLateResult
	}
	if state != "Succeeded" {
		return ErrLateResult
	}
	var content string
	if err := tx.QueryRowContext(ctx, `SELECT content FROM initial_analysis_outputs WHERE attempt_id=?`, result.AttemptID).Scan(&content); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLateResult
		}
		return err
	}
	var proposed string
	if err := json.Unmarshal(result.Canonical, &proposed); err != nil || proposed != content {
		return ErrLateResult
	}
	return errSealedResultReplayed
}
