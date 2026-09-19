package investigation

// Result adjudication (DATA-INVEST-001/002, RUNTIME-TASK-008): the
// ResultProposal commits the assistant message, the head move and the
// attempt terminal state in one runner-owned transaction that also carries
// the automatic audit event; late results (withdrawn branch, burned lease,
// wrong boot/epoch) stay audit-only. The transient delta feed is notified
// only after the commit, so the stream never observes a terminal state the
// durable store does not have.
//
// 生命周期非用户变更走执行器 Execute（无命令台账）：任务作用域从 attempt 行的
// 持久关联恢复（attempt.LoadCorrelation），审计由执行器自动落库。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// Result is the adjudicated ResultProposal payload.
// ErrPendingBrowserCleanup means a frozen terminal was rechecked while an
// Exploration obligation still existed. It is retried by reconciliation; it is
// not a late model result.
var ErrPendingBrowserCleanup = errors.New("browser cleanup remains pending")

// errSealedResultReplayed marks the idempotent re-delivery of an already
// sealed terminal verdict (RUNTIME-TASK-008). It is a plain skip sentinel,
// never a durable fact: the losing redelivery changed nothing, so the runner
// rolls the no-op transaction back and records nothing, exactly like the
// previous empty-commit path.
var errSealedResultReplayed = errors.New("investigation result already sealed")

// unit is the empty business result of the single-statement lifecycle facts.
type unit struct{}

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

// pendingTerminalProposal is the exact natural ResultProposal held while an
// Exploration owned by the same Attempt seals its mandatory terminal trace and
// physical Stop. Its digest makes a replay idempotent without terminalizing the
// parent before that cleanup obligation is discharged.
type pendingTerminalProposal struct {
	AttemptID   int64           `json:"attemptId"`
	BootID      string          `json:"bootId"`
	Epoch       uint64          `json:"epoch"`
	Succeeded   bool            `json:"succeeded"`
	Termination string          `json:"termination,omitempty"`
	SchemaKind  string          `json:"schemaKind"`
	Canonical   json.RawMessage `json:"canonical"`
	Digest      string          `json:"digest"`
	EvidenceIDs []int64         `json:"evidenceIds"`
	ArtifactIDs []int64         `json:"artifactIds"`
	ModelCallID int64           `json:"modelCallId"`
}

func pendingProposalFor(result Result, modelCallID int64) ([]byte, []byte, error) {
	body, err := json.Marshal(pendingTerminalProposal{
		AttemptID: result.AttemptID, BootID: result.BootID, Epoch: result.Epoch,
		Succeeded: result.Succeeded, Termination: result.Termination,
		SchemaKind: result.SchemaKind, Canonical: json.RawMessage(result.Canonical),
		Digest: hex.EncodeToString(result.Digest), EvidenceIDs: result.EvidenceIDs,
		ArtifactIDs: result.ArtifactIDs, ModelCallID: modelCallID,
	})
	if err != nil {
		return nil, nil, err
	}
	digest := sha256.Sum256(body)
	return body, digest[:], nil
}

// AcceptAttempt records Assigned -> Running (RUNTIME-TASK-004) as one
// audited system fact. The investigation aggregate has no separate state
// row; the attempt row is the authority.
func (service *Service) AcceptAttempt(ctx context.Context, attemptID int64, bootID string, epoch uint64) error {
	lifecycleCtx, err := service.lifecycleContext(ctx, attemptID)
	if err != nil {
		return err
	}
	_, err = execution.Execute(lifecycleCtx, service.runner, service.opAttemptAccepted, func(tx *execution.Tx) (unit, error) {
		return unit{}, service.acceptAttemptOn(lifecycleCtx, tx, attemptID, bootID)
	}, func(unit) int64 { return attemptID })
	return err
}

// acceptAttemptOn mirrors the shared attempt machine's Accept fence on the
// runner's guarded transaction (the attempt package exposes it only as an
// auto-commit pool write; the fenced UPDATE is the frozen identity): the
// fence matches the dispatch boot, the binding epoch is immutable once set.
// The epoch is transport context only and never part of the accept fence.
func (service *Service) acceptAttemptOn(ctx context.Context, tx writer, attemptID int64, bootID string) error {
	now := service.nowText()
	result, err := tx.ExecContext(ctx, `
		UPDATE execution_attempts
		SET state='Running', accepted_at=?, started_at=?, row_version=row_version+1
		WHERE id=? AND state='Assigned' AND boot_id=?`,
		now, now, attemptID, bootID)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return fmt.Errorf("attempt %d acceptance refused (not Assigned or binding mismatch)", attemptID)
	}
	return nil
}

// CancelAck finalizes a cancelled attempt (RUNTIME-CANCEL-003) as one
// audited system fact and closes the transient delta feed with the cancelled
// terminal state.
func (service *Service) CancelAck(ctx context.Context, attemptID int64) error {
	lifecycleCtx, err := service.lifecycleContext(ctx, attemptID)
	if err != nil {
		return err
	}
	_, err = execution.Execute(lifecycleCtx, service.runner, service.opAttemptCancelAck, func(tx *execution.Tx) (unit, error) {
		return unit{}, service.cancelAckOn(lifecycleCtx, tx, attemptID)
	}, func(unit) int64 { return attemptID })
	if err != nil {
		return err
	}
	service.NotifyTerminal(ctx, attemptID)
	return nil
}

// cancelAckOn mirrors the shared attempt machine's CancelAck transition
// (Cancelling -> Cancelled) on the runner's guarded transaction. An attempt
// the loss convergence already closed as Cancelled is tolerated so a late
// duplicate ack still converges its stream view; anything else is refused.
func (service *Service) cancelAckOn(ctx context.Context, tx writer, attemptID int64) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE execution_attempts
		SET state='Cancelled', ended_at=?, termination_reason='cancelled', row_version=row_version+1
		WHERE id=? AND state='Cancelling'`, service.nowText(), attemptID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 1 {
		return nil
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&state); err != nil {
		return err
	}
	if state == "Cancelled" {
		return nil
	}
	return fmt.Errorf("attempt %d is not Cancelling", attemptID)
}

// CommitResult adjudicates one result proposal. Success seals the
// assistant message, its Evidence/Artifact references, the head move and
// the attempt terminal state in one transaction (DATA-INVEST-001/003).
// A result for a withdrawn user message is a late result (audit only,
// DATA-INVEST-002).
func (service *Service) CommitResult(ctx context.Context, result Result) error {
	return service.commitResult(ctx, result, true, 0)
}

// commitResult is shared by initial proposal adjudication and the durable
// pending-terminal drain. Only the initial path may stage a pending result.
// pendingModelCallID is non-zero only when draining the frozen work item; it
// binds that drain to the exact model call recorded before cleanup began.
func (service *Service) commitResult(ctx context.Context, result Result, allowPending bool, pendingModelCallID int64) error {
	if !result.Succeeded {
		return service.commitFailure(ctx, result)
	}
	if result.SchemaKind != OutputSchemaKind {
		return fmt.Errorf("unexpected result schema kind %q", result.SchemaKind)
	}
	digest := sha256.Sum256(result.Canonical)
	if hex.EncodeToString(digest[:]) != hex.EncodeToString(result.Digest) {
		return errors.New("result content digest mismatch")
	}
	var content string
	if err := json.Unmarshal(result.Canonical, &content); err != nil {
		return errors.New("investigation output must be a JSON string")
	}
	lifecycleCtx, err := service.lifecycleContext(ctx, result.AttemptID)
	if err != nil {
		return err
	}
	notify := int64(0)
	type committed struct {
		InvestigationID int64
	}
	_, err = execution.Execute(lifecycleCtx, service.runner, service.opAssistantCommitted, func(tx *execution.Tx) (committed, error) {
		attemptState, investigationID, userMessageID, err := attemptBinding(lifecycleCtx, tx, result)
		if err != nil {
			return committed{}, err
		}
		if attemptState != "Running" {
			// An already-adjudicated result replays the original verdict
			// (RUNTIME-TASK-008 idempotent adjudication); a divergent result
			// stays a late result. The no-op redelivery records nothing.
			return committed{}, service.replaySealedResult(lifecycleCtx, tx, result, attemptState)
		}
		if err := verifyLease(lifecycleCtx, tx, service.now(), result); err != nil {
			return committed{}, err
		}
		var userStatus string
		if err := tx.QueryRowContext(lifecycleCtx, `SELECT status FROM investigation_messages WHERE id=?`, userMessageID).Scan(&userStatus); err != nil {
			return committed{}, err
		}
		if userStatus != "active" {
			// The branch was withdrawn after the turn started: the committed
			// assistant reply must not re-enter the active branch
			// (DATA-INVEST-002).
			return committed{}, ErrLateResult
		}
		var modelID string
		var modelCallID int64
		if err := tx.QueryRowContext(lifecycleCtx, `
			SELECT id,model_id FROM model_calls WHERE attempt_id=? AND status='succeeded'
			ORDER BY id DESC LIMIT 1`, result.AttemptID).Scan(&modelCallID, &modelID); err != nil {
			return committed{}, fmt.Errorf("result without a succeeded model call: %w", err)
		}
		if pendingModelCallID != 0 && modelCallID != pendingModelCallID {
			return committed{}, ErrLateResult
		}
		if len(result.ArtifactIDs) > 0 || len(result.EvidenceIDs) > 0 {
			if err := validateReferences(lifecycleCtx, tx, result); err != nil {
				return committed{}, fmt.Errorf("evidence/artifact references do not close: %w", err)
			}
		}
		now := service.nowText()
		nextSeq, err := nextMessageSeq(lifecycleCtx, tx, investigationID)
		if err != nil {
			return committed{}, err
		}
		assistantInsert, err := tx.ExecContext(lifecycleCtx, `
			INSERT INTO investigation_messages(investigation_id,attempt_id,seq,role,status,content,parent_message_id,created_at)
			VALUES(?,?,?,'assistant','active',?,?,?)`, investigationID, result.AttemptID, nextSeq, content, userMessageID, now)
		if err != nil {
			return committed{}, err
		}
		assistantMessageID, err := assistantInsert.LastInsertId()
		if err != nil {
			return committed{}, err
		}
		for ordinal, evidenceID := range result.EvidenceIDs {
			if _, err := tx.ExecContext(lifecycleCtx, `
				INSERT INTO investigation_message_evidence(message_id,evidence_id,ordinal)
				VALUES(?,?,?)`, assistantMessageID, evidenceID, ordinal); err != nil {
				return committed{}, err
			}
		}
		for ordinal, artifactID := range result.ArtifactIDs {
			if _, err := tx.ExecContext(lifecycleCtx, `
				INSERT INTO investigation_message_artifacts(message_id,artifact_id,ordinal)
				VALUES(?,?,?)`, assistantMessageID, artifactID, ordinal); err != nil {
				return committed{}, err
			}
		}
		if _, err := tx.ExecContext(lifecycleCtx, `
			UPDATE execution_attempts
			SET state='Succeeded', ended_at=?, row_version=row_version+1
			WHERE id=? AND state='Running' AND boot_id=? AND connection_epoch=?`,
			now, result.AttemptID, result.BootID, result.Epoch); err != nil {
			return committed{}, err
		}
		if _, err := tx.ExecContext(lifecycleCtx, `
			UPDATE investigations SET current_head_message_id=? WHERE id=?`, assistantMessageID, investigationID); err != nil {
			return committed{}, err
		}
		if pendingModelCallID != 0 {
			if _, err := tx.ExecContext(lifecycleCtx, `DELETE FROM pending_attempt_terminals WHERE attempt_id=? AND model_call_id=?`, result.AttemptID, pendingModelCallID); err != nil {
				return committed{}, err
			}
		}
		// The stream never observes a terminal state before the runner
		// committed: notification happens after Execute returns.
		notify = result.AttemptID
		return committed{InvestigationID: investigationID}, nil
	}, func(value committed) int64 { return value.InvestigationID })
	if err != nil {
		if errors.Is(err, errSealedResultReplayed) {
			return nil
		}
		return err
	}
	if notify != 0 {
		service.NotifyTerminal(context.Background(), notify)
	}
	return nil
}

// HasPendingTerminal reports whether a result proposal is durably awaiting
// browser cleanup. Runtime must defer ResultAck while this remains true.
func (service *Service) HasPendingTerminal(ctx context.Context, attemptID int64) bool {
	var found int
	return service.runner.Reader().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pending_attempt_terminals WHERE attempt_id=?)`, attemptID).Scan(&found) == nil && found == 1
}

// CommitPendingTerminal revalidates and commits the frozen proposal only after
// Runtime has observed that no browser cleanup obligation remains. The delete is
// in the same transaction as the terminal parent write, so a crash retries the
// exact immutable proposal rather than manufacturing a new result.
func (service *Service) CommitPendingTerminal(ctx context.Context, attemptID int64) (Result, bool, error) {
	var source, targetState string
	// recovery_loss rows intentionally contain no model proposal or digest.
	// Scan nullable columns as nullable before dispatching on source; otherwise a
	// valid loss work item is silently undrainable at the SQL boundary.
	var proposalJSON sql.NullString
	var storedDigest []byte
	var terminalReason sql.NullString
	if err := service.runner.Reader().QueryRowContext(ctx, `SELECT source,target_state,proposal_json,proposal_digest,terminal_reason FROM pending_attempt_terminals WHERE attempt_id=?`, attemptID).Scan(&source, &targetState, &proposalJSON, &storedDigest, &terminalReason); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Result{}, false, nil
		}
		return Result{}, false, err
	}
	if source == "recovery_loss" {
		return service.commitRecoveryLossPending(ctx, attemptID, targetState, terminalReason.String)
	}
	if source != "model_result" {
		return Result{}, false, errors.New("pending terminal source is invalid")
	}
	if !proposalJSON.Valid || len(storedDigest) != sha256.Size {
		return Result{}, false, errors.New("model-result pending terminal is malformed")
	}
	actual := sha256.Sum256([]byte(proposalJSON.String))
	if !bytes.Equal(actual[:], storedDigest) {
		return Result{}, false, errors.New("pending terminal proposal digest mismatch")
	}
	var proposal pendingTerminalProposal
	if err := json.Unmarshal([]byte(proposalJSON.String), &proposal); err != nil {
		return Result{}, false, fmt.Errorf("parse pending terminal proposal: %w", err)
	}
	if proposal.AttemptID != attemptID || proposal.ModelCallID < 1 {
		return Result{}, false, errors.New("pending terminal proposal binding malformed")
	}
	if (proposal.Succeeded && targetState != "Succeeded") || (!proposal.Succeeded && targetState != "Failed") {
		return Result{}, false, errors.New("pending terminal target state does not match proposal")
	}
	var digest []byte
	if proposal.Succeeded {
		var err error
		digest, err = hex.DecodeString(proposal.Digest)
		if err != nil || len(digest) != sha256.Size {
			return Result{}, false, errors.New("pending terminal proposal result digest malformed")
		}
	}
	result := Result{AttemptID: proposal.AttemptID, BootID: proposal.BootID, Epoch: proposal.Epoch, Succeeded: proposal.Succeeded,
		Termination: proposal.Termination, SchemaKind: proposal.SchemaKind, Canonical: []byte(proposal.Canonical), Digest: digest,
		EvidenceIDs: proposal.EvidenceIDs, ArtifactIDs: proposal.ArtifactIDs}
	if err := service.commitResult(ctx, result, false, proposal.ModelCallID); err != nil {
		return Result{}, false, err
	}
	return result, true, nil
}

// commitRecoveryLossPending is the no-model half of the pending-terminal state
// machine. It is reached only after Runtime verified every browser operation is
// terminal and Stop-acknowledged, so it atomically publishes Interrupted and
// consumes the immutable recovery-loss work item.
func (service *Service) commitRecoveryLossPending(ctx context.Context, attemptID int64, targetState, reason string) (Result, bool, error) {
	if targetState != "Interrupted" || reason == "" {
		return Result{}, false, errors.New("recovery-loss pending terminal is malformed")
	}
	lifecycleCtx, err := service.lifecycleContext(ctx, attemptID)
	if err != nil {
		return Result{}, false, err
	}
	type drained struct {
		AttemptID int64
	}
	notify := false
	_, err = execution.Execute(lifecycleCtx, service.runner, service.opRecoveryLoss, func(tx *execution.Tx) (drained, error) {
		result, err := tx.ExecContext(lifecycleCtx, `UPDATE execution_attempts
			SET state='Interrupted', ended_at=?, termination_reason=?, row_version=row_version+1
			WHERE id=? AND state='Running'`, service.nowText(), reason, attemptID)
		if err != nil {
			return drained{}, err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return drained{}, ErrLateResult
		}
		if _, err := tx.ExecContext(lifecycleCtx, `DELETE FROM pending_attempt_terminals WHERE attempt_id=? AND source='recovery_loss'`, attemptID); err != nil {
			return drained{}, err
		}
		// NotifyTerminal reads the same deliberately single-connection store.
		// It must run only after the runner returned the transaction
		// connection, or a valid recovery-loss drain deadlocks after
		// committing its Interrupted state.
		notify = true
		return drained{AttemptID: attemptID}, nil
	}, func(value drained) int64 { return value.AttemptID })
	if err != nil {
		return Result{}, false, err
	}
	if notify {
		service.NotifyTerminal(context.Background(), attemptID)
	}
	return Result{AttemptID: attemptID}, true, nil
}

// commitFailure records a failed attempt (DATA-ATTEMPT-005): no assistant
// message, no head move; the user turn stays answerable through a later
// retry ticket.
func (service *Service) commitFailure(ctx context.Context, result Result) error {
	lifecycleCtx, err := service.lifecycleContext(ctx, result.AttemptID)
	if err != nil {
		return err
	}
	notify := int64(0)
	type failed struct {
		InvestigationID int64
	}
	_, err = execution.Execute(lifecycleCtx, service.runner, service.opAttemptFailed, func(tx *execution.Tx) (failed, error) {
		attemptState, investigationID, _, err := attemptBinding(lifecycleCtx, tx, result)
		if err != nil {
			return failed{}, err
		}
		if attemptState != "Running" {
			// Same replay contract as the success seal (RUNTIME-TASK-008):
			// an identical failure replays its original verdict; the no-op
			// redelivery records nothing.
			return failed{}, service.replaySealedResult(lifecycleCtx, tx, result, attemptState)
		}
		if err := verifyLease(lifecycleCtx, tx, service.now(), result); err != nil {
			return failed{}, err
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
		// Draining a frozen failed terminal must consume its sole work item in the
		// same transaction as the parent transition. The delete is harmless for an
		// ordinary direct failure, where no pending row exists.
		if _, err := tx.ExecContext(lifecycleCtx, `DELETE FROM pending_attempt_terminals WHERE attempt_id=?`, result.AttemptID); err != nil {
			return failed{}, err
		}
		notify = result.AttemptID
		return failed{InvestigationID: investigationID}, nil
	}, func(value failed) int64 { return value.InvestigationID })
	if err != nil {
		if errors.Is(err, errSealedResultReplayed) {
			return nil
		}
		return err
	}
	if notify != 0 {
		service.NotifyTerminal(context.Background(), notify)
	}
	return nil
}

// attemptBinding verifies the attempt identity (type/scope/boot/epoch) and
// returns the current state with the investigation and user message ids.
func attemptBinding(ctx context.Context, tx audit.Reader, result Result) (string, int64, int64, error) {
	var attemptType, scopeType, state, bootID string
	var investigationID int64
	var epoch int64
	if err := tx.QueryRowContext(ctx, `
		SELECT attempt_type,scope_type,scope_id,state,boot_id,connection_epoch
		FROM execution_attempts WHERE id=?`, result.AttemptID).
		Scan(&attemptType, &scopeType, &investigationID, &state, &bootID, &epoch); err != nil {
		return "", 0, 0, err
	}
	if attemptType != "investigation" || scopeType != "investigation" {
		return "", 0, 0, fmt.Errorf("attempt %d is not an investigation attempt", result.AttemptID)
	}
	if state == "Running" && (bootID != result.BootID || epoch != int64(result.Epoch)) {
		return state, investigationID, 0, ErrLateResult
	}
	// A retry attempt owns no user message row: the binding resolves
	// through the frozen input lineage instead (attemptUserMessage).
	userMessageID, _, err := attemptUserMessage(ctx, tx, result.AttemptID)
	if err != nil {
		return state, investigationID, 0, err
	}
	return state, investigationID, userMessageID, nil
}

// verifyLease enforces RUNTIME-TASK-008: a result whose lease already
// burned down must not produce a valid domain output even if the sweeper
// has not yet converged the row (audit only).
func verifyLease(ctx context.Context, tx audit.Reader, now time.Time, result Result) error {
	var leaseUntil string
	if err := tx.QueryRowContext(ctx, `SELECT lease_until FROM execution_attempts WHERE id=?`, result.AttemptID).Scan(&leaseUntil); err != nil {
		return err
	}
	if deadline, err := time.Parse(time.RFC3339Nano, leaseUntil); err != nil || !now.Before(deadline) {
		return ErrLateResult
	}
	return nil
}

// replaySealedResult rebuilds the original verdict for one terminal
// attempt: an identical success (this attempt's sealed assistant message
// matches the proposal bytes) or an identical failure (same termination
// reason) replays so the runtime's reliable-delivery retry observes the
// original ResultAck (RUNTIME-TASK-008) — reported through the
// errSealedResultReplayed skip sentinel, which records nothing; anything
// else stays a late result (audit only, DATA-ATTEMPT-004).
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
	if err := tx.QueryRowContext(ctx, `
		SELECT content FROM investigation_messages WHERE attempt_id=? AND role='assistant'`, result.AttemptID).Scan(&content); err != nil {
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

// validateReferences re-checks the sealed message's reference closure on
// the caller's transaction: every Evidence must belong to this attempt,
// every Artifact must be granted to this attempt through its own tool
// results, and neither list may repeat.
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
