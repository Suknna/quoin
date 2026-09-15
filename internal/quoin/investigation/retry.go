package investigation

// Explicit Retry (DATA-INVEST-002): one new Execution Attempt re-answers a
// Failed attempt's user message while that message is still active on the
// current branch. The retried message row is reused verbatim (one user
// message per attempt; the new attempt resolves its input cutoff through
// the frozen lineage), a withdrawn or branch-left message conflicts, and a
// running attempt blocks the retry until it terminates.

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

var (
	// ErrAttemptNotFailed reports a retry against an attempt that is not
	// in the Failed terminal state (still active or another terminal).
	ErrAttemptNotFailed = errors.New("only a failed attempt can be retried")
	// ErrRetryMessageWithdrawn reports a retry whose user message was
	// withdrawn (or otherwise left the active branch): the withdrawn
	// branch must never re-enter the model context (DATA-INVEST-002).
	ErrRetryMessageWithdrawn = errors.New("the retried message is withdrawn")
)

// retryResult is the durable ledger payload of one retry command.
type retryResult struct {
	AttemptID int64 `json:"attemptId"`
}

// retryDigest fingerprints the retry command's semantic fields (the scoped
// investigation and the failed attempt locator; HTTP-COMMAND-002).
func retryDigest(investigationID, attemptID int64) string {
	return commandDigest("retry:"+strconv.FormatInt(investigationID, 10)+"@"+strconv.FormatInt(attemptID, 10), "", nil, nil, nil)
}

// Retry creates a new attempt re-answering the failed attempt's user
// message and returns the new attempt id. The command is idempotent through
// the durable ledger: a replay returns the originally created attempt
// (HTTP-COMMAND-003).
func (service *Service) Retry(ctx context.Context, principalID int64, clientCommandID string, investigationID, attemptID int64) (int64, error) {
	digest := retryDigest(investigationID, attemptID)
	outcome, err := execution.Run(ctx, service.runner, service.opRetry, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (retryResult, execution.Change, error) {
		var attemptType, scopeType, state string
		var scopeID int64
		err := tx.QueryRowContext(ctx, `
			SELECT attempt_type, scope_type, scope_id, state
			FROM execution_attempts WHERE id=?`, attemptID).
			Scan(&attemptType, &scopeType, &scopeID, &state)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && (scopeType != "investigation" || scopeID != investigationID)) {
			return retryResult{}, execution.Changed, ErrNotFound
		}
		if err != nil {
			return retryResult{}, execution.Changed, err
		}
		if state != "Failed" {
			return retryResult{}, execution.Changed, ErrAttemptNotFailed
		}
		var messageID, messageSeq int64
		var messageStatus string
		err = tx.QueryRowContext(ctx, `
			SELECT id, seq, status FROM investigation_messages
			WHERE attempt_id=? AND role='user'`, attemptID).Scan(&messageID, &messageSeq, &messageStatus)
		if errors.Is(err, sql.ErrNoRows) {
			return retryResult{}, execution.Changed, ErrNotFound
		}
		if err != nil {
			return retryResult{}, execution.Changed, err
		}
		if messageStatus != "active" {
			return retryResult{}, execution.Changed, ErrRetryMessageWithdrawn
		}
		var active int64
		err = tx.QueryRowContext(ctx, `
			SELECT id FROM execution_attempts
			WHERE scope_type='investigation' AND scope_id=? AND state IN `+activeAttemptStates,
			investigationID).Scan(&active)
		if err == nil {
			return retryResult{}, execution.Changed, ErrActiveAttempt
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return retryResult{}, execution.Changed, err
		}
		selected, err := selectModelProvider(ctx, tx)
		if err != nil {
			return retryResult{}, execution.Changed, err
		}
		// The turn's attachments are the durable message references (the send
		// transaction validated them against the message-level boundary; the
		// bodies are immutable, so the same totals hold).
		attachments, err := messageAttachments(ctx, tx, messageID)
		if err != nil {
			return retryResult{}, execution.Changed, err
		}
		now := service.nowText()
		// CreateOn centrally persists the command's correlation metadata onto
		// the new attempt in this same transaction (ADR-0006); a context
		// without execution metadata fails the creation.
		newAttemptID, err := attempt.CreateOn(ctx, tx, `
			INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
			VALUES('investigation','investigation',?,'Queued',?,?,?)`, investigationID, attempt.ReleaseVersion(), AgentVersion, now)
		if err != nil {
			return retryResult{}, execution.Changed, err
		}
		// The snapshot's cutoff is the retried message itself: attemptUserMessage
		// resolves the same message through the frozen lineage at dispatch time.
		businessContext, err := businessContextForInvestigation(ctx, tx, investigationID)
		if err != nil {
			return retryResult{}, execution.Changed, err
		}
		if err := service.freezeInputSnapshot(ctx, tx, investigationID, newAttemptID, messageID, attachments, businessContext, selected, now); err != nil {
			return retryResult{}, execution.Changed, err
		}
		return retryResult{AttemptID: newAttemptID}, execution.Changed, nil
	}, func(result retryResult) int64 { return result.AttemptID })
	if err != nil {
		return 0, commandError(err)
	}
	return outcome.Result.AttemptID, nil
}

// messageAttachments loads one message's ordered immutable attachment
// references (the retry input reuses the same artifacts without copying;
// DATA-ATTACH-001 / UI-CHAT-008).
func messageAttachments(ctx context.Context, tx writer, messageID int64) ([]resolvedAttachment, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT t.id, t.artifact_id, t.original_filename, t.size_bytes
		FROM investigation_message_attachments a
		JOIN text_attachments t ON t.id=a.attachment_id
		WHERE a.message_id=? ORDER BY a.ordinal`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attachments := []resolvedAttachment{}
	for rows.Next() {
		var item resolvedAttachment
		if err := rows.Scan(&item.AttachmentID, &item.ArtifactID, &item.Filename, &item.SizeBytes); err != nil {
			return nil, err
		}
		attachments = append(attachments, item)
	}
	return attachments, rows.Err()
}
