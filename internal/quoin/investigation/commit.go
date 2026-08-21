package investigation

// Result adjudication (DATA-INVEST-001/002, RUNTIME-TASK-008): the
// ResultProposal commits the assistant message, the head move and the
// attempt terminal state in one transaction; late results (withdrawn
// branch, burned lease, wrong boot/epoch) stay audit-only. The transient
// delta feed is notified only after the commit, so the stream never
// observes a terminal state the durable store does not have.

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

// AcceptAttempt records Assigned -> Running (RUNTIME-TASK-004). The
// investigation aggregate has no separate state row; the attempt row is
// the authority.
func (service *Service) AcceptAttempt(ctx context.Context, attemptID int64, bootID string, epoch uint64) error {
	return service.attempts.Accept(ctx, attemptID, bootID, epoch)
}

// CancelAck finalizes a cancelled attempt (RUNTIME-CANCEL-003) and closes
// the transient delta feed with the cancelled terminal state.
func (service *Service) CancelAck(ctx context.Context, attemptID int64) error {
	if err := service.attempts.CancelAck(ctx, attemptID); err != nil {
		var state string
		if lookupErr := service.db.QueryRowContext(ctx, `SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&state); lookupErr != nil || state != "Cancelled" {
			return err
		}
	}
	service.NotifyTerminal(ctx, attemptID)
	return nil
}

// CommitResult adjudicates one result proposal. Success seals the
// assistant message, its Evidence/Artifact references, the head move and
// the attempt terminal state in one transaction (DATA-INVEST-001/003).
// A result for a withdrawn user message is a late result (audit only,
// DATA-INVEST-002).
func (service *Service) CommitResult(ctx context.Context, result Result) error {
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
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return err
	}
	committed := false
	notifyAttempt := int64(0)
	// Defer order is load-bearing on a single-connection pool: at return
	// the rollback guard runs first, then the connection is returned, and
	// only then does the terminal notification query the store (LIFO).
	defer func() {
		if notifyAttempt != 0 {
			service.NotifyTerminal(context.Background(), notifyAttempt)
		}
	}()
	defer conn.Close()
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	attemptState, investigationID, userMessageID, err := service.attemptBinding(ctx, conn, result)
	if err != nil {
		return err
	}
	if attemptState != "Running" {
		// An already-adjudicated result replays the original verdict
		// (RUNTIME-TASK-008 idempotent adjudication); a divergent result
		// stays a late result.
		return service.replaySealedResult(ctx, conn, result, attemptState)
	}
	if err := verifyLease(ctx, conn, service.now(), result); err != nil {
		return err
	}
	var userStatus string
	if err := conn.QueryRowContext(ctx, `SELECT status FROM investigation_messages WHERE id=?`, userMessageID).Scan(&userStatus); err != nil {
		return err
	}
	if userStatus != "active" {
		// The branch was withdrawn after the turn started: the committed
		// assistant reply must not re-enter the active branch
		// (DATA-INVEST-002).
		return ErrLateResult
	}
	var modelID string
	if err := conn.QueryRowContext(ctx, `
		SELECT model_id FROM model_calls WHERE attempt_id=? AND status='succeeded'
		ORDER BY id DESC LIMIT 1`, result.AttemptID).Scan(&modelID); err != nil {
		return fmt.Errorf("result without a succeeded model call: %w", err)
	}
	if len(result.ArtifactIDs) > 0 || len(result.EvidenceIDs) > 0 {
		if err := validateReferences(ctx, conn, result); err != nil {
			return fmt.Errorf("evidence/artifact references do not close: %w", err)
		}
	}
	now := service.nowText()
	nextSeq, err := nextMessageSeq(ctx, conn, investigationID)
	if err != nil {
		return err
	}
	assistantInsert, err := conn.ExecContext(ctx, `
		INSERT INTO investigation_messages(investigation_id,attempt_id,seq,role,status,content,parent_message_id,created_at)
		VALUES(?,?,?,'assistant','active',?,?,?)`, investigationID, result.AttemptID, nextSeq, content, userMessageID, now)
	if err != nil {
		return err
	}
	assistantMessageID, err := assistantInsert.LastInsertId()
	if err != nil {
		return err
	}
	for ordinal, evidenceID := range result.EvidenceIDs {
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO investigation_message_evidence(message_id,evidence_id,ordinal)
			VALUES(?,?,?)`, assistantMessageID, evidenceID, ordinal); err != nil {
			return err
		}
	}
	for ordinal, artifactID := range result.ArtifactIDs {
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO investigation_message_artifacts(message_id,artifact_id,ordinal)
			VALUES(?,?,?)`, assistantMessageID, artifactID, ordinal); err != nil {
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
	if _, err := conn.ExecContext(ctx, `
		UPDATE investigations SET current_head_message_id=? WHERE id=?`, assistantMessageID, investigationID); err != nil {
		return err
	}
	if err := recordAudit(ctx, conn, "system", 0, "investigation.assistant_committed", "success", "investigation", investigationID, now); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	notifyAttempt = result.AttemptID
	return nil
}

// commitFailure records a failed attempt (DATA-ATTEMPT-005): no assistant
// message, no head move; the user turn stays answerable through a later
// retry ticket.
func (service *Service) commitFailure(ctx context.Context, result Result) error {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return err
	}
	committed := false
	notifyAttempt := int64(0)
	// Same load-bearing defer order as CommitResult.
	defer func() {
		if notifyAttempt != 0 {
			service.NotifyTerminal(context.Background(), notifyAttempt)
		}
	}()
	defer conn.Close()
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	attemptState, investigationID, _, err := service.attemptBinding(ctx, conn, result)
	if err != nil {
		return err
	}
	if attemptState != "Running" {
		return service.replaySealedResult(ctx, conn, result, attemptState)
	}
	if err := verifyLease(ctx, conn, service.now(), result); err != nil {
		return err
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
	// The audit's domain reference is the investigation, never the attempt
	// (audit rows are the authoritative record; DATA-AUDIT-001).
	if err := recordAudit(ctx, conn, "system", 0, "investigation.attempt_failed", "success", "investigation", investigationID, now); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	notifyAttempt = result.AttemptID
	return nil
}

// attemptBinding verifies the attempt identity (type/scope/boot/epoch) and
// returns the current state with the investigation and user message ids.
func (service *Service) attemptBinding(ctx context.Context, conn *sql.Conn, result Result) (string, int64, int64, error) {
	var attemptType, scopeType, state, bootID string
	var investigationID int64
	var epoch int64
	if err := conn.QueryRowContext(ctx, `
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
	var userMessageID int64
	if err := conn.QueryRowContext(ctx, `
		SELECT id FROM investigation_messages WHERE attempt_id=? AND role='user'`, result.AttemptID).Scan(&userMessageID); err != nil {
		return state, investigationID, 0, err
	}
	return state, investigationID, userMessageID, nil
}

// verifyLease enforces RUNTIME-TASK-008: a result whose lease already
// burned down must not produce a valid domain output even if the sweeper
// has not yet converged the row (audit only).
func verifyLease(ctx context.Context, conn *sql.Conn, now time.Time, result Result) error {
	var leaseUntil string
	if err := conn.QueryRowContext(ctx, `SELECT lease_until FROM execution_attempts WHERE id=?`, result.AttemptID).Scan(&leaseUntil); err != nil {
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
	if err := conn.QueryRowContext(ctx, `
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
	return nil
}

// validateReferences re-checks the sealed message's reference closure on
// the caller's transaction: every Evidence must belong to this attempt,
// every Artifact must be granted to this attempt through its own tool
// results, and neither list may repeat.
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
