package investigation

// Create/Send commands (DATA-INVEST-001): both freeze the input, create
// the Execution Attempt and append the user message in one SQLite
// transaction. Create also atomically creates the Investigation and its
// immutable provenance links — the "新建" UI opens only client-side blank
// input and no empty Investigation is ever persisted.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Suknna/quoin/internal/quoin/attempt"
)

// maxContentRunes mirrors the frozen MessageContentInput maxLength
// (HTTP-VALIDATION-004).
const maxContentRunes = 262144

// activeAttemptStates is the frozen active-state set (DATA-ATTEMPT-002):
// the send fence and the list projection must share the exact set.
const activeAttemptStates = "('Queued','Assigned','Running','Cancelling')"

// Create renders the input snapshot, creates the Investigation, its
// sources, the first user message (with its ordered attachment references)
// and the first attempt in one transaction (DATA-INVEST-001). A replayed
// client command returns its original result; a reused command id with a
// different request digest conflicts (HTTP-COMMAND-003).
func (service *Service) Create(ctx context.Context, principalID int64, clientCommandID, content string, attachmentIDs []int64, sources []SourceInput) (CreateResult, error) {
	digest := commandDigest("create", content, nil, attachmentIDs, sources)
	if entry, ok, err := service.replayLookup(principalID, clientCommandID, digest); err != nil {
		return CreateResult{}, err
	} else if ok {
		return CreateResult{InvestigationID: entry.investigationID, MessageID: entry.messageID, AttemptID: entry.attemptID}, nil
	}
	if err := validateMessage(content, len(attachmentIDs)); err != nil {
		return CreateResult{}, err
	}
	if err := validateAttachmentIDs(attachmentIDs); err != nil {
		return CreateResult{}, err
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return CreateResult{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return CreateResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	// Second replay check inside the serialized transaction (a concurrent
	// same-command request may have committed just before this writer).
	if entry, ok, err := service.replayLookup(principalID, clientCommandID, digest); err != nil {
		return CreateResult{}, err
	} else if ok {
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return CreateResult{}, err
		}
		committed = true
		return CreateResult{InvestigationID: entry.investigationID, MessageID: entry.messageID, AttemptID: entry.attemptID}, nil
	}
	selected, err := selectModelProvider(ctx, conn)
	if err != nil {
		return CreateResult{}, err
	}
	attachments, err := service.resolveAttachments(ctx, conn, principalID, attachmentIDs)
	if err != nil {
		return CreateResult{}, err
	}
	now := service.nowText()
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO investigations(created_by, created_at) VALUES(?,?)`, principalID, now)
	if err != nil {
		return CreateResult{}, err
	}
	investigationID, err := insert.LastInsertId()
	if err != nil {
		return CreateResult{}, err
	}
	if err := insertSources(ctx, conn, investigationID, principalID, sources, now); err != nil {
		return CreateResult{}, err
	}
	messageID, attemptID, err := service.insertTurn(ctx, conn, investigationID, principalID, clientCommandID, content, attachments, selected, now)
	if err != nil {
		return CreateResult{}, err
	}
	if err := recordAudit(ctx, conn, "user", principalID, "investigation.create", "success", "investigation", investigationID, now); err != nil {
		return CreateResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return CreateResult{}, err
	}
	committed = true
	result := CreateResult{InvestigationID: investigationID, MessageID: messageID, AttemptID: attemptID}
	service.replayRemember(principalID, clientCommandID, replayEntry{investigationID: investigationID, messageID: messageID, attemptID: attemptID, digest: digest})
	return result, nil
}

// Send appends one user turn: single transaction creating the attempt,
// freezing the input, appending the user message (with its ordered
// attachment references) and moving the head (DATA-INVEST-001). The
// expected head fences concurrent writers; the one active attempt
// invariant rejects sends while a model attempt runs (DATA-INVEST-003).
func (service *Service) Send(ctx context.Context, principalID int64, clientCommandID string, investigationID int64, expectedHead *int64, content string, attachmentIDs []int64) (SendResult, error) {
	digest := commandDigest("send:"+strconv.FormatInt(investigationID, 10), content, expectedHead, attachmentIDs, nil)
	if entry, ok, err := service.replayLookup(principalID, clientCommandID, digest); err != nil {
		return SendResult{}, err
	} else if ok {
		return SendResult{MessageID: entry.messageID, AttemptID: entry.attemptID}, nil
	}
	if err := validateMessage(content, len(attachmentIDs)); err != nil {
		return SendResult{}, err
	}
	if err := validateAttachmentIDs(attachmentIDs); err != nil {
		return SendResult{}, err
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return SendResult{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return SendResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if entry, ok, err := service.replayLookup(principalID, clientCommandID, digest); err != nil {
		return SendResult{}, err
	} else if ok {
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return SendResult{}, err
		}
		committed = true
		return SendResult{MessageID: entry.messageID, AttemptID: entry.attemptID}, nil
	}
	var currentHead sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT current_head_message_id FROM investigations WHERE id=?`, investigationID).Scan(&currentHead); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SendResult{}, ErrNotFound
		}
		return SendResult{}, err
	}
	if !headMatches(currentHead, expectedHead) {
		var head *int64
		if currentHead.Valid {
			value := currentHead.Int64
			head = &value
		}
		return SendResult{}, &HeadConflictError{CurrentHead: head}
	}
	var active int64
	err = conn.QueryRowContext(ctx, `
		SELECT id FROM execution_attempts
		WHERE scope_type='investigation' AND scope_id=? AND state IN `+activeAttemptStates,
		investigationID).Scan(&active)
	if err == nil {
		return SendResult{}, ErrActiveAttempt
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return SendResult{}, err
	}
	selected, err := selectModelProvider(ctx, conn)
	if err != nil {
		return SendResult{}, err
	}
	attachments, err := service.resolveAttachments(ctx, conn, principalID, attachmentIDs)
	if err != nil {
		return SendResult{}, err
	}
	now := service.nowText()
	messageID, attemptID, err := service.insertTurn(ctx, conn, investigationID, principalID, clientCommandID, content, attachments, selected, now)
	if err != nil {
		return SendResult{}, err
	}
	if err := recordAudit(ctx, conn, "user", principalID, "investigation.message.send", "success", "investigation", investigationID, now); err != nil {
		return SendResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return SendResult{}, err
	}
	committed = true
	result := SendResult{MessageID: messageID, AttemptID: attemptID}
	service.replayRemember(principalID, clientCommandID, replayEntry{investigationID: investigationID, messageID: messageID, attemptID: attemptID, digest: digest})
	return result, nil
}

func headMatches(current sql.NullInt64, expected *int64) bool {
	if expected == nil {
		return !current.Valid
	}
	return current.Valid && current.Int64 == *expected
}

// insertTurn appends one user turn inside the caller's transaction: the
// attempt row, the user message with its ordered attachment references
// and the head move, then the frozen input snapshot and grants
// (DATA-INVEST-001, DATA-ATTACH-001).
func (service *Service) insertTurn(ctx context.Context, conn *sql.Conn, investigationID, principalID int64, clientCommandID, content string, attachments []resolvedAttachment, selected provider, now string) (int64, int64, error) {
	attemptInsert, err := conn.ExecContext(ctx, `
		INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
		VALUES('investigation','investigation',?,'Queued',?,?,?)`, investigationID, attempt.ReleaseVersion(), AgentVersion, now)
	if err != nil {
		return 0, 0, err
	}
	attemptID, err := attemptInsert.LastInsertId()
	if err != nil {
		return 0, 0, err
	}
	// The input freezes at this attempt's own user message (the message
	// row must exist before the snapshot can reference it).
	nextSeq, err := nextMessageSeq(ctx, conn, investigationID)
	if err != nil {
		return 0, 0, err
	}
	messageInsert, err := conn.ExecContext(ctx, `
		INSERT INTO investigation_messages(investigation_id,attempt_id,seq,role,status,content,client_command_id,created_at)
		VALUES(?,?,?,'user','active',?,?,?)`, investigationID, attemptID, nextSeq, content, clientCommandID, now)
	if err != nil {
		return 0, 0, err
	}
	messageID, err := messageInsert.LastInsertId()
	if err != nil {
		return 0, 0, err
	}
	for ordinal, attachment := range attachments {
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO investigation_message_attachments(message_id,attachment_id,ordinal)
			VALUES(?,?,?)`, messageID, attachment.AttachmentID, ordinal); err != nil {
			return 0, 0, err
		}
	}
	if _, err := conn.ExecContext(ctx, `
		UPDATE investigations SET current_head_message_id=? WHERE id=?`, messageID, investigationID); err != nil {
		return 0, 0, err
	}
	if err := service.freezeInputSnapshot(ctx, conn, investigationID, attemptID, messageID, attachments, selected, now); err != nil {
		return 0, 0, err
	}
	return messageID, attemptID, nil
}

// freezeInputSnapshot persists the frozen input snapshot, the ordered
// lineage items (one per active-branch message up to the turn's own user
// message, then the turn's attachment artifacts), the input artifact
// read grants and the chat_model grant. Send/create and retry share it:
// the digest always freezes over the rebuildable lineage
// (ARCH-CONTEXT-006) and the input-item trigger keeps withdrawn messages
// out of new snapshots (DATA-INVEST-002). turnMessageID is the user
// message this attempt answers — the newly created attempt of a retry
// owns no message row of its own, so the cutoff cannot be derived from
// the attempt at freeze time.
func (service *Service) freezeInputSnapshot(ctx context.Context, conn *sql.Conn, investigationID, attemptID, turnMessageID int64, attachments []resolvedAttachment, selected provider, now string) error {
	var cutoffSeq int64
	if err := conn.QueryRowContext(ctx, `
		SELECT seq FROM investigation_messages WHERE id=? AND investigation_id=?`, turnMessageID, investigationID).Scan(&cutoffSeq); err != nil {
		return err
	}
	canonical, err := service.rebuildFor(ctx, conn, investigationID, cutoffSeq, selected.ProbeResultID)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	snapshotInsert, err := conn.ExecContext(ctx, `
		INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at)
		VALUES(?,?,?,?,?)`, attemptID, SchemaKind, RendererVersion, hex.EncodeToString(digest[:]), now)
	if err != nil {
		return err
	}
	snapshotID, err := snapshotInsert.LastInsertId()
	if err != nil {
		return err
	}
	itemCount, err := insertMessageLineage(ctx, conn, snapshotID, investigationID, cutoffSeq)
	if err != nil {
		return err
	}
	// Attachment lineage: one item per referenced artifact continuing the
	// message lineage's item_seq (the grant closure trigger requires the
	// (snapshot, artifact) pair; ARCH-CONTEXT-006 keeps the digest
	// rebuildable from durable references).
	for index, attachment := range attachments {
		itemDigest := sha256.Sum256([]byte(fmt.Sprintf("attachment:%d", attachment.AttachmentID)))
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,artifact_id)
			VALUES(?,?,?,?,?)`, snapshotID, itemCount+index+1, "attachment", hex.EncodeToString(itemDigest[:]), attachment.ArtifactID); err != nil {
			return err
		}
	}
	for _, attachment := range attachments {
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO attempt_artifact_grants(attempt_id,artifact_id,source_kind,source_id,granted_at)
			VALUES(?,?,?,?,?)`, attemptID, attachment.ArtifactID, "input_snapshot", snapshotID, now); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at)
		VALUES(?,?,?,?,?,?,?)`,
		attemptID, "chat_model", selected.ConnectionID, selected.RevisionID, selected.CredentialGen, selected.ProbeResultID, now); err != nil {
		return err
	}
	return nil
}

// insertMessageLineage persists one ordered input item per active-branch
// message of the turn (ARCH-CONTEXT-006: the digest covers the
// rebuildable lineage, not only the rendered bytes) and returns the number
// of items written (attachment items continue the sequence).
func insertMessageLineage(ctx context.Context, conn *sql.Conn, snapshotID, investigationID, cutoffSeq int64) (int, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT id, role FROM investigation_messages
		WHERE investigation_id=? AND status='active' AND seq<=?
		ORDER BY seq`, investigationID, cutoffSeq)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	seq := 0
	for rows.Next() {
		var messageID int64
		var role string
		if err := rows.Scan(&messageID, &role); err != nil {
			return 0, err
		}
		seq++
		sourceDigest := sha256.Sum256([]byte(fmt.Sprintf("message:%d", messageID)))
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,investigation_message_id)
			VALUES(?,?,?,?,?)`, snapshotID, seq, role, hex.EncodeToString(sourceDigest[:]), messageID); err != nil {
			return 0, err
		}
	}
	return seq, rows.Err()
}

func nextMessageSeq(ctx context.Context, conn *sql.Conn, investigationID int64) (int64, error) {
	var seq sql.NullInt64
	if err := conn.QueryRowContext(ctx, `
		SELECT MAX(seq) FROM investigation_messages WHERE investigation_id=?`, investigationID).Scan(&seq); err != nil {
		return 0, err
	}
	if seq.Valid {
		return seq.Int64 + 1, nil
	}
	return 1, nil
}

// insertSources verifies and freezes the create command's provenance
// references (immutable after creation; the create transaction is the only
// write path — DATA-INVEST-004).
func insertSources(ctx context.Context, conn *sql.Conn, investigationID, principalID int64, sources []SourceInput, now string) error {
	if len(sources) > 100 {
		return ErrInvalidSource
	}
	seen := map[string]bool{}
	for _, source := range sources {
		key := source.Type + ":" + fmt.Sprint(source.SourceID)
		if seen[key] {
			return ErrInvalidSource
		}
		seen[key] = true
		var column string
		switch source.Type {
		case "occurrence":
			column = "occurrence_id"
		case "initial_analysis":
			column = "initial_analysis_id"
		case "evidence":
			column = "evidence_id"
		case "inspection_report":
			column = "inspection_report_id"
		default:
			return ErrInvalidSource
		}
		if source.SourceID <= 0 {
			return ErrInvalidSource
		}
		var table string
		switch source.Type {
		case "occurrence":
			table = "alert_occurrences"
		case "initial_analysis":
			table = "initial_analyses"
		case "evidence":
			table = "evidence"
		case "inspection_report":
			table = "inspection_reports"
		}
		var exists int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE id=?`, source.SourceID).Scan(&exists); err != nil {
			return err
		}
		if exists != 1 {
			return ErrSourceNotFound
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO investigation_source_links(investigation_id,`+column+`,linked_by,linked_at)
			VALUES(?,?,?,?)`, investigationID, source.SourceID, principalID, now); err != nil {
			return err
		}
	}
	return nil
}

// validateMessage enforces the frozen message-level boundaries: non-empty
// text within the length cap or at least one attachment (attachment-only
// turns are legal; an empty-everything turn is not).
func validateMessage(content string, attachmentCount int) error {
	if strings.TrimSpace(content) == "" && attachmentCount == 0 {
		return ErrMessageInvalid
	}
	if utf8.RuneCountInString(content) > maxContentRunes {
		return ErrMessageInvalid
	}
	return nil
}

// validateAttachmentIDs rejects duplicate references up front (the wire
// schema declares uniqueItems; a duplicated id is a deterministic 422).
func validateAttachmentIDs(ids []int64) error {
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			return ErrAttachmentInvalidRef
		}
		seen[id] = true
	}
	return nil
}

// commandDigest is the deterministic command fingerprint used by the
// replay ledger (HTTP-COMMAND-002/003). The target prefix carries the
// scoped object (send/undo/stop/retry carry the investigation and, where
// applicable, the attempt locator) so one command id can never silently
// replay across targets; the attachment references are semantic request
// fields and must participate.
func commandDigest(target, content string, expectedHead *int64, attachmentIDs []int64, sources []SourceInput) string {
	var builder strings.Builder
	builder.WriteString("target:")
	builder.WriteString(target)
	builder.WriteString("\ncontent:")
	builder.WriteString(content)
	builder.WriteString("\nhead:")
	if expectedHead == nil {
		builder.WriteString("null")
	} else {
		builder.WriteString(fmt.Sprint(*expectedHead))
	}
	for _, id := range attachmentIDs {
		builder.WriteString("\nattachment:")
		builder.WriteString(fmt.Sprint(id))
	}
	for _, source := range sources {
		builder.WriteString("\nsource:")
		builder.WriteString(source.Type)
		builder.WriteString(":")
		builder.WriteString(fmt.Sprint(source.SourceID))
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}
