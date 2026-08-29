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
)

// Attempt lifecycle: technical-failure retry, the cancellation fence, the
// dispatch accept gate, the first-success seal and failure bookkeeping.
// Retry creates a fresh attempt for a technical failure (DATA-ANALYSIS-001).
// The domain input (alert occurrence context) is reused byte-for-byte from
// the frozen first snapshot; only the model contract may be re-resolved
// against the current enabled provider so a legitimate rotation does not
// brick retries. The same client command replays to its original attempt.
func (service *Service) Retry(ctx context.Context, analysisID, principalID int64, clientCommandID string) (int64, error) {
	if entry, ok := service.replayLookup(principalID, clientCommandID); ok {
		return entry.attemptID, nil
	}
	var state string
	if err := service.db.QueryRowContext(ctx, `SELECT state FROM initial_analyses WHERE id=?`, analysisID).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	if state != "Failed" {
		return 0, fmt.Errorf("%w: analysis %d is %s", ErrActiveConflict, analysisID, state)
	}
	// The frozen schema closes Failed as terminal (trg_initial_analyses_
	// terminal_immutable), so same-analysis retry has no legal transition;
	// the operator retry path is a fresh analysis on the same occurrence
	// (Create accepts it once the terminal one leaves the active set). The
	// endpoint answers the deterministic conflict instead of pretending the
	// reopen happened.
	return 0, fmt.Errorf("%w: analysis %d is Failed and the frozen schema closes Failed as terminal (operator retry creates a new analysis on the occurrence)", ErrActiveConflict, analysisID)
}

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
	DispatchRequired bool
}

// Cancel commits the idempotent cancellation fence (DATA-ATTEMPT-003,
// HTTP-COMMAND-005): success already committed wins by SQLite order;
// otherwise the active attempt closes to Cancelled/Cancelling and the
// analysis follows on CancelAck.
func (service *Service) Cancel(ctx context.Context, analysisID, principalID, expectedRowVersion int64, clientCommandID string) (CancelOutcome, error) {
	if entry, ok := service.replayLookup(principalID, clientCommandID); ok {
		detail, err := service.Get(ctx, analysisID)
		if err != nil {
			return CancelOutcome{}, err
		}
		// The original execution already dispatched the runtime cancel;
		// replays only report the committed outcome.
		return CancelOutcome{State: detail.State, RowVersion: detail.RowVersion, AttemptID: entry.attemptID}, nil
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return CancelOutcome{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return CancelOutcome{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var state string
	var rowVersion int64
	if err := conn.QueryRowContext(ctx, `SELECT state,row_version FROM initial_analyses WHERE id=?`, analysisID).Scan(&state, &rowVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CancelOutcome{}, ErrNotFound
		}
		return CancelOutcome{}, err
	}
	if rowVersion != expectedRowVersion {
		return CancelOutcome{}, &RowVersionError{Current: rowVersion}
	}
	now := service.nowText()
	if err := recordAudit(ctx, conn, "user", principalID, "initial_analysis.cancel", "success", "initial_analysis", analysisID, now); err != nil {
		return CancelOutcome{}, err
	}
	switch state {
	case "Succeeded":
		// Success won the commit-order race: report the completed object.
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return CancelOutcome{}, err
		}
		committed = true
		service.replayRemember(principalID, clientCommandID, replayEntry{analysisID: analysisID, attemptID: 0})
		return CancelOutcome{State: "Succeeded", RowVersion: rowVersion}, nil
	case "Failed", "Cancelled", "Interrupted":
		return CancelOutcome{}, fmt.Errorf("%w: analysis %d is %s", ErrActiveConflict, analysisID, state)
	}
	var attemptID int64
	if err := conn.QueryRowContext(ctx, `
		SELECT id FROM execution_attempts
		WHERE scope_type='analysis' AND scope_id=? AND state IN ('Queued','Assigned','Running','Cancelling')`,
		analysisID).Scan(&attemptID); err != nil {
		return CancelOutcome{}, err
	}
	// The fence must run on this transaction's connection: SQLite is
	// single-writer and attempt.CancelFence would open a second writer.
	attemptState, err := service.CancelFenceOn(ctx, conn, attemptID)
	if err != nil {
		return CancelOutcome{}, err
	}
	nextState := state
	outcome := CancelOutcome{AttemptID: attemptID}
	if attemptState == "Cancelled" {
		nextState = "Cancelled"
		if _, err := conn.ExecContext(ctx, `
			UPDATE initial_analyses SET state='Cancelled', row_version=row_version+1
			WHERE id=? AND state=?`, analysisID, state); err != nil {
			return CancelOutcome{}, err
		}
		rowVersion++
	} else {
		// Cancelling: the analysis stays Running until CancelAck; the app
		// layer dispatches CancelAttempt after this fence commits.
		nextState = "Running"
		outcome.DispatchRequired = true
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return CancelOutcome{}, err
	}
	committed = true
	service.replayRemember(principalID, clientCommandID, replayEntry{analysisID: analysisID, attemptID: attemptID})
	outcome.State = nextState
	outcome.RowVersion = rowVersion
	return outcome, nil
}

// CancelFenceOn is the conn-scoped variant of attempt.CancelFence used
// inside analysis transactions (SQLite single-writer: nested BEGIN would
// deadlock otherwise).
func (service *Service) CancelFenceOn(ctx context.Context, conn *sql.Conn, attemptID int64) (string, error) {
	return service.attempts.CancelFenceOn(ctx, conn, attemptID)
}

// AcceptAttempt records Assigned -> Running and moves the analysis to
// Running (RUNTIME-TASK-004). The attempt and analysis rows close in two
// sequential transactions: the frozen state machine keeps both updates
// legal regardless of order, and a crash between them leaves the
// T12 reconciler a deterministic repair (attempt Running + analysis
// Queued, or attempt Assigned + analysis Queued — both re-converge).
func (service *Service) AcceptAttempt(ctx context.Context, attemptID int64, bootID string, epoch uint64) error {
	if err := service.attempts.Accept(ctx, attemptID, bootID, epoch); err != nil {
		return err
	}
	var scopeID int64
	if err := service.db.QueryRowContext(ctx, `SELECT scope_id FROM execution_attempts WHERE id=?`, attemptID).Scan(&scopeID); err != nil {
		return err
	}
	result, err := service.db.ExecContext(ctx, `
		UPDATE initial_analyses SET state='Running', row_version=row_version+1
		WHERE id=? AND state='Queued'`, scopeID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return fmt.Errorf("analysis %d is not Queued; acceptance refused", scopeID)
	}
	return nil
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
// attempt + analysis terminal state in one transaction, DATA-ANALYSIS-002)
// or records the attempt failure. Late results lose against the
// cancellation fence (DATA-TX-005).
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
	var attemptType, scopeType string
	var scopeID int64
	var state string
	if err := conn.QueryRowContext(ctx, `SELECT attempt_type,scope_type,scope_id,state FROM execution_attempts WHERE id=?`, result.AttemptID).
		Scan(&attemptType, &scopeType, &scopeID, &state); err != nil {
		return err
	}
	if attemptType != "initial_analysis" || scopeType != "analysis" {
		return fmt.Errorf("attempt %d is not an initial analysis attempt", result.AttemptID)
	}
	if state != "Running" {
		// A replay of an already-adjudicated result rebuilds the original
		// verdict instead of surfacing a late-result error (RUNTIME-TASK-008
		// idempotent adjudication; the runtime retries its terminal
		// proposal until an ack survives). A divergent result stays a
		// late result.
		return service.replaySealedResult(ctx, conn, result, state)
	}
	var bootID string
	var epoch int64
	var leaseUntil string
	if err := conn.QueryRowContext(ctx, `SELECT boot_id,connection_epoch,lease_until FROM execution_attempts WHERE id=?`, result.AttemptID).Scan(&bootID, &epoch, &leaseUntil); err != nil {
		return err
	}
	if bootID != result.BootID || epoch != int64(result.Epoch) {
		return ErrLateResult
	}
	// RUNTIME-TASK-008: a result whose lease already burned down must not
	// produce a valid domain output even if the sweeper has not yet
	// converged the row (the sweep window is seconds; the lease is the
	// authority). Audit only.
	if leaseDeadline, err := time.Parse(time.RFC3339Nano, leaseUntil); err != nil || !service.now().Before(leaseDeadline) {
		return ErrLateResult
	}
	var modelID string
	if err := conn.QueryRowContext(ctx, `
		SELECT model_id FROM model_calls WHERE attempt_id=? AND status='succeeded'
		ORDER BY id DESC LIMIT 1`, result.AttemptID).Scan(&modelID); err != nil {
		return fmt.Errorf("result without a succeeded model call: %w", err)
	}
	if len(result.ArtifactIDs) > 0 || len(result.EvidenceIDs) > 0 {
		// T11 closes the Evidence/Artifact references (DATA-ANALYSIS-002):
		// every reference must belong to this attempt before the output
		// seals them; stray locators keep the seal deterministic.
		if err := validateReferences(ctx, conn, result); err != nil {
			return fmt.Errorf("evidence/artifact references do not close: %w", err)
		}
	}
	now := service.nowText()
	outputInsert, err := conn.ExecContext(ctx, `
		INSERT INTO initial_analysis_outputs(analysis_id,attempt_id,model_id,content,created_at)
		VALUES(?,?,?,?,?)`, scopeID, result.AttemptID, modelID, content, now)
	if err != nil {
		return err
	}
	outputID, err := outputInsert.LastInsertId()
	if err != nil {
		return err
	}
	for ordinal, evidenceID := range result.EvidenceIDs {
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO initial_analysis_output_evidence(output_id,evidence_id,ordinal)
			VALUES(?,?,?)`, outputID, evidenceID, ordinal); err != nil {
			return err
		}
	}
	for ordinal, artifactID := range result.ArtifactIDs {
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO initial_analysis_output_artifacts(output_id,artifact_id,ordinal)
			VALUES(?,?,?)`, outputID, artifactID, ordinal); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `
		UPDATE execution_attempts
		SET state='Succeeded', ended_at=?, row_version=row_version+1
		WHERE id=? AND state='Running' AND boot_id=? AND connection_epoch=?`,
		now, result.AttemptID, result.BootID, result.Epoch); err != nil {
		return err
	}
	// Crash-window repair: the accept's two transactions can die between
	// them (attempt Running, analysis still Queued). The attempt state is
	// the authority — promote the analysis in the same transaction so the
	// seal never strands a Queued analysis over a terminal attempt.
	if _, err := conn.ExecContext(ctx, `
		UPDATE initial_analyses SET state='Running', row_version=row_version+1
		WHERE id=? AND state='Queued'`, scopeID); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `
		UPDATE initial_analyses SET state='Succeeded', row_version=row_version+1
		WHERE id=? AND state='Running'`, scopeID); err != nil {
		return err
	}
	if err := recordAudit(ctx, conn, "system", 0, "initial_analysis.succeeded", "success", "initial_analysis", scopeID, now); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// commitFailure records a failed attempt and fails the analysis when it
// still owns the active attempt (DATA-ATTEMPT-005).
func (service *Service) commitFailure(ctx context.Context, result Result) error {
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
	var attemptType, scopeType string
	var scopeID int64
	if err := conn.QueryRowContext(ctx, `SELECT attempt_type,scope_type,scope_id FROM execution_attempts WHERE id=?`, result.AttemptID).
		Scan(&attemptType, &scopeType, &scopeID); err != nil {
		return err
	}
	if attemptType != "initial_analysis" || scopeType != "analysis" {
		return fmt.Errorf("attempt %d is not an initial analysis attempt", result.AttemptID)
	}
	var attemptState string
	if err := conn.QueryRowContext(ctx, `SELECT state FROM execution_attempts WHERE id=?`, result.AttemptID).Scan(&attemptState); err != nil {
		return err
	}
	if attemptState != "Running" {
		// Same replay contract as the success seal (RUNTIME-TASK-008):
		// an identical failure replays its original verdict.
		return service.replaySealedResult(ctx, conn, result, attemptState)
	}
	// RUNTIME-TASK-008: a burned-down lease rejects the result (audit only)
	// even before the sweeper converges the row.
	var leaseUntil string
	if err := conn.QueryRowContext(ctx, `SELECT lease_until FROM execution_attempts WHERE id=?`, result.AttemptID).Scan(&leaseUntil); err != nil {
		return err
	}
	if leaseDeadline, err := time.Parse(time.RFC3339Nano, leaseUntil); err != nil || !service.now().Before(leaseDeadline) {
		return ErrLateResult
	}
	now := service.nowText()
	reason := result.Termination
	if reason == "" {
		reason = "worker_protocol_error"
	}
	attemptUpdate, err := conn.ExecContext(ctx, `
		UPDATE execution_attempts
		SET state='Failed', ended_at=?, termination_reason=?, row_version=row_version+1
		WHERE id=? AND state='Running' AND boot_id=? AND connection_epoch=?`,
		now, reason, result.AttemptID, result.BootID, result.Epoch)
	if err != nil {
		return err
	}
	if affected, _ := attemptUpdate.RowsAffected(); affected != 1 {
		return ErrLateResult
	}
	if _, err := conn.ExecContext(ctx, `
		UPDATE initial_analyses SET state='Failed', row_version=row_version+1
		WHERE id=? AND state IN ('Queued','Running')`, scopeID); err != nil {
		return err
	}
	if err := recordAudit(ctx, conn, "system", 0, "initial_analysis.failed", "success", "initial_analysis", scopeID, now); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// CancelAck finishes the analysis cancellation once the runtime confirmed
// the attempt stopped (RUNTIME-CANCEL-003). Idempotent: an attempt the
// loss convergence already closed as Cancelled still converges its
// analysis (a late duplicate ack must not strand the analysis Running).
func (service *Service) CancelAck(ctx context.Context, attemptID int64) error {
	if err := service.attempts.CancelAck(ctx, attemptID); err != nil {
		var state string
		if lookupErr := service.db.QueryRowContext(ctx, `SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&state); lookupErr != nil || state != "Cancelled" {
			return err
		}
	}
	var scopeID int64
	if err := service.db.QueryRowContext(ctx, `SELECT scope_id FROM execution_attempts WHERE id=?`, attemptID).Scan(&scopeID); err != nil {
		return err
	}
	_, err := service.db.ExecContext(ctx, `
		UPDATE initial_analyses SET state='Cancelled', row_version=row_version+1
		WHERE id=? AND state IN ('Queued','Running')`, scopeID)
	return err
}

// validateReferences re-checks the sealed output's reference closure on
// the caller's transaction (DATA-ANALYSIS-002): every Evidence must belong
// to this attempt, every Artifact must be granted to this attempt through
// its own tool results, and neither list may repeat.
func validateReferences(ctx context.Context, conn *sql.Conn, result Result) error {
	seenEvidence := map[int64]bool{}
	for _, evidenceID := range result.EvidenceIDs {
		if evidenceID <= 0 || seenEvidence[evidenceID] {
			return fmt.Errorf("evidence %d is not a unique positive locator", evidenceID)
		}
		seenEvidence[evidenceID] = true
		var attemptID int64
		if err := conn.QueryRowContext(ctx, `SELECT attempt_id FROM evidence WHERE id=?`, evidenceID).Scan(&attemptID); err != nil {
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
		if err := conn.QueryRowContext(ctx, `
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
func (service *Service) replaySealedResult(ctx context.Context, conn *sql.Conn, result Result, state string) error {
	if !result.Succeeded {
		if state == "Failed" {
			var sealed string
			if err := conn.QueryRowContext(ctx, `SELECT termination_reason FROM execution_attempts WHERE id=?`, result.AttemptID).Scan(&sealed); err != nil {
				return err
			}
			if sealed == result.Termination || (sealed == "worker_protocol_error" && result.Termination == "") {
				return nil
			}
		}
		return ErrLateResult
	}
	if state != "Succeeded" {
		return ErrLateResult
	}
	var content string
	if err := conn.QueryRowContext(ctx, `SELECT content FROM initial_analysis_outputs WHERE attempt_id=?`, result.AttemptID).Scan(&content); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLateResult
		}
		return err
	}
	var proposed string
	if err := json.Unmarshal(result.Canonical, &proposed); err != nil || proposed != content {
		return ErrLateResult
	}
	return nil
}

// CommitInterruption closes one attempt with its loss reason and moves the
// analysis to the matching terminal state in the same transaction
// (RUNTIME-TASK-006): Interrupted analyses stay inspectable and the
// occurrence becomes eligible for a fresh analysis; a Cancelling attempt
// converges to Cancelled (the fence exception).
func (service *Service) CommitInterruption(ctx context.Context, attemptID int64, reason string) error {
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
	var scopeID int64
	if err := conn.QueryRowContext(ctx, `SELECT scope_id FROM execution_attempts WHERE id=?`, attemptID).Scan(&scopeID); err != nil {
		return err
	}
	final, err := service.attempts.InterruptOn(ctx, conn, attemptID, reason)
	if err != nil {
		return err
	}
	switch final {
	case "Interrupted":
		if _, err := conn.ExecContext(ctx, `
			UPDATE initial_analyses SET state='Interrupted', row_version=row_version+1
			WHERE id=? AND state IN ('Queued','Running')`, scopeID); err != nil {
			return err
		}
		if err := recordAudit(ctx, conn, "system", 0, "initial_analysis.interrupted", "success", "initial_analysis", scopeID, service.nowText()); err != nil {
			return err
		}
	case "Cancelled":
		if _, err := conn.ExecContext(ctx, `
			UPDATE initial_analyses SET state='Cancelled', row_version=row_version+1
			WHERE id=? AND state IN ('Queued','Running')`, scopeID); err != nil {
			return err
		}
		if err := recordAudit(ctx, conn, "system", 0, "initial_analysis.cancelled", "success", "initial_analysis", scopeID, service.nowText()); err != nil {
			return err
		}
	default:
		// The attempt already reached a terminal result; the analysis
		// followed it there. Nothing to converge.
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}
