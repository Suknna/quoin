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

// retryDigest fingerprints the retry command's semantic fields (the scoped
// investigation and the failed attempt locator; HTTP-COMMAND-002).
func retryDigest(investigationID, attemptID int64) string {
	return commandDigest("retry:"+strconv.FormatInt(investigationID, 10)+"@"+strconv.FormatInt(attemptID, 10), "", nil, nil, nil)
}

// Retry creates a new attempt re-answering the failed attempt's user
// message and returns the new attempt id. The command is idempotent: a
// replay returns the originally created attempt (HTTP-COMMAND-003).
func (service *Service) Retry(ctx context.Context, principalID int64, clientCommandID string, investigationID, attemptID int64) (int64, error) {
	digest := retryDigest(investigationID, attemptID)
	if entry, ok, err := service.replayLookup(principalID, clientCommandID, digest); err != nil {
		return 0, err
	} else if ok {
		return entry.attemptID, nil
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if entry, ok, err := service.replayLookup(principalID, clientCommandID, digest); err != nil {
		return 0, err
	} else if ok {
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return 0, err
		}
		committed = true
		return entry.attemptID, nil
	}
	var attemptType, scopeType, state string
	var scopeID int64
	err = conn.QueryRowContext(ctx, `
		SELECT attempt_type, scope_type, scope_id, state
		FROM execution_attempts WHERE id=?`, attemptID).
		Scan(&attemptType, &scopeType, &scopeID, &state)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (scopeType != "investigation" || scopeID != investigationID)) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if state != "Failed" {
		return 0, ErrAttemptNotFailed
	}
	var messageID, messageSeq int64
	var messageStatus string
	err = conn.QueryRowContext(ctx, `
		SELECT id, seq, status FROM investigation_messages
		WHERE attempt_id=? AND role='user'`, attemptID).Scan(&messageID, &messageSeq, &messageStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if messageStatus != "active" {
		return 0, ErrRetryMessageWithdrawn
	}
	var active int64
	err = conn.QueryRowContext(ctx, `
		SELECT id FROM execution_attempts
		WHERE scope_type='investigation' AND scope_id=? AND state IN `+activeAttemptStates,
		investigationID).Scan(&active)
	if err == nil {
		return 0, ErrActiveAttempt
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	selected, err := selectModelProvider(ctx, conn)
	if err != nil {
		return 0, err
	}
	// The turn's attachments are the durable message references (the send
	// transaction validated them against the message-level boundary; the
	// bodies are immutable, so the same totals hold).
	attachments, err := messageAttachments(ctx, conn, messageID)
	if err != nil {
		return 0, err
	}
	now := service.nowText()
	attemptInsert, err := conn.ExecContext(ctx, `
		INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
		VALUES('investigation','investigation',?,'Queued',?,?,?)`, investigationID, attempt.ReleaseVersion(), AgentVersion, now)
	if err != nil {
		return 0, err
	}
	newAttemptID, err := attemptInsert.LastInsertId()
	if err != nil {
		return 0, err
	}
	// The snapshot's cutoff is the retried message itself: attemptUserMessage
	// resolves the same message through the frozen lineage at dispatch time.
	if err := service.freezeInputSnapshot(ctx, conn, investigationID, newAttemptID, messageID, attachments, selected, now); err != nil {
		return 0, err
	}
	if err := recordAudit(ctx, conn, "user", principalID, "investigation.attempt.retry", "success", "investigation", investigationID, now); err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return 0, err
	}
	committed = true
	service.replayRemember(principalID, clientCommandID, replayEntry{investigationID: investigationID, attemptID: newAttemptID, digest: digest})
	return newAttemptID, nil
}

// messageAttachments loads one message's ordered immutable attachment
// references (the retry input reuses the same artifacts without copying;
// DATA-ATTACH-001 / UI-CHAT-008).
func messageAttachments(ctx context.Context, conn *sql.Conn, messageID int64) ([]resolvedAttachment, error) {
	rows, err := conn.QueryContext(ctx, `
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
