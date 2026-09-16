package investigation

// Create/Send commands (DATA-INVEST-001): both freeze the input, create
// the Execution Attempt and append the user message in one runner-owned
// transaction that also persists the durable command ledger row and the
// automatic audit event. Create also atomically creates the Investigation and
// its immutable provenance links — the "新建" UI opens only client-side blank
// input and no empty Investigation is ever persisted. A replayed client
// command returns its original result from the persisted ledger; a reused
// command id with a different request digest conflicts (HTTP-COMMAND-003) —
// the in-process replay table is gone.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/execution"
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
// Create preserves the source-only command surface for internal callers. Direct
// chat callers that deliberately bind a published business system use
// CreateWithBusinessSystem below.
func (service *Service) Create(ctx context.Context, principalID int64, clientCommandID, content string, attachmentIDs []int64, sources []SourceInput) (CreateResult, error) {
	return service.create(ctx, principalID, clientCommandID, content, attachmentIDs, sources, "")
}

// CreateWithBusinessSystem creates a direct chat with an explicit system key.
// The key is resolved to immutable published configuration/contract references
// in the same transaction; user prompt text never participates in authority.
func (service *Service) CreateWithBusinessSystem(ctx context.Context, principalID int64, clientCommandID, content string, attachmentIDs []int64, sources []SourceInput, businessSystemKey string) (CreateResult, error) {
	return service.create(ctx, principalID, clientCommandID, content, attachmentIDs, sources, businessSystemKey)
}

func (service *Service) create(ctx context.Context, principalID int64, clientCommandID, content string, attachmentIDs []int64, sources []SourceInput, businessSystemKey string) (CreateResult, error) {
	if err := validateMessage(content, len(attachmentIDs)); err != nil {
		return CreateResult{}, err
	}
	if err := validateAttachmentIDs(attachmentIDs); err != nil {
		return CreateResult{}, err
	}
	digest := commandDigest("create:"+businessSystemKey, content, nil, attachmentIDs, sources)
	outcome, err := execution.Run(ctx, service.runner, service.opCreate, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (CreateResult, execution.Change, error) {
		selected, err := selectModelProvider(ctx, tx)
		if err != nil {
			return CreateResult{}, execution.Changed, err
		}
		attachments, err := service.resolveAttachments(ctx, tx, principalID, attachmentIDs)
		if err != nil {
			return CreateResult{}, execution.Changed, err
		}
		businessContext, err := resolveBusinessContext(ctx, tx, businessSystemKey)
		if err != nil {
			return CreateResult{}, execution.Changed, err
		}
		now := service.nowText()
		insert, err := tx.ExecContext(ctx, `
			INSERT INTO investigations(created_by, created_at) VALUES(?,?)`, principalID, now)
		if err != nil {
			return CreateResult{}, execution.Changed, err
		}
		investigationID, err := insert.LastInsertId()
		if err != nil {
			return CreateResult{}, execution.Changed, err
		}
		if err := insertSources(ctx, tx, investigationID, principalID, sources, now); err != nil {
			return CreateResult{}, execution.Changed, err
		}
		messageID, attemptID, err := service.insertTurn(ctx, tx, investigationID, principalID, clientCommandID, content, attachments, businessContext, selected, now)
		if err != nil {
			return CreateResult{}, execution.Changed, err
		}
		return CreateResult{InvestigationID: investigationID, MessageID: messageID, AttemptID: attemptID}, execution.Changed, nil
	}, func(result CreateResult) int64 { return result.InvestigationID })
	if err != nil {
		return CreateResult{}, commandError(err)
	}
	return outcome.Result, nil
}

// Send appends one user turn: one runner transaction creating the attempt,
// freezing the input, appending the user message (with its ordered
// attachment references) and moving the head (DATA-INVEST-001), with the
// command ledger row and the automatic audit event in the same commit. The
// expected head fences concurrent writers; the one active attempt invariant
// rejects sends while a model attempt runs (DATA-INVEST-003).
func (service *Service) Send(ctx context.Context, principalID int64, clientCommandID string, investigationID int64, expectedHead *int64, content string, attachmentIDs []int64) (SendResult, error) {
	if err := validateMessage(content, len(attachmentIDs)); err != nil {
		return SendResult{}, err
	}
	if err := validateAttachmentIDs(attachmentIDs); err != nil {
		return SendResult{}, err
	}
	digest := commandDigest("send:"+strconv.FormatInt(investigationID, 10), content, expectedHead, attachmentIDs, nil)
	outcome, err := execution.Run(ctx, service.runner, service.opSend, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (SendResult, execution.Change, error) {
		var currentHead sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT current_head_message_id FROM investigations WHERE id=?`, investigationID).Scan(&currentHead); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return SendResult{}, execution.Changed, ErrNotFound
			}
			return SendResult{}, execution.Changed, err
		}
		if !headMatches(currentHead, expectedHead) {
			var head *int64
			if currentHead.Valid {
				value := currentHead.Int64
				head = &value
			}
			return SendResult{}, execution.Changed, &HeadConflictError{CurrentHead: head}
		}
		var active int64
		err := tx.QueryRowContext(ctx, `
			SELECT id FROM execution_attempts
			WHERE scope_type='investigation' AND scope_id=? AND state IN `+activeAttemptStates,
			investigationID).Scan(&active)
		if err == nil {
			return SendResult{}, execution.Changed, ErrActiveAttempt
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return SendResult{}, execution.Changed, err
		}
		selected, err := selectModelProvider(ctx, tx)
		if err != nil {
			return SendResult{}, execution.Changed, err
		}
		attachments, err := service.resolveAttachments(ctx, tx, principalID, attachmentIDs)
		if err != nil {
			return SendResult{}, execution.Changed, err
		}
		businessContext, err := businessContextForInvestigation(ctx, tx, investigationID)
		if err != nil {
			return SendResult{}, execution.Changed, err
		}
		now := service.nowText()
		messageID, attemptID, err := service.insertTurn(ctx, tx, investigationID, principalID, clientCommandID, content, attachments, businessContext, selected, now)
		if err != nil {
			return SendResult{}, execution.Changed, err
		}
		return SendResult{MessageID: messageID, AttemptID: attemptID}, execution.Changed, nil
	}, func(result SendResult) int64 { return result.MessageID })
	if err != nil {
		return SendResult{}, commandError(err)
	}
	return outcome.Result, nil
}

// commandError translates the shared runner's outcomes back to this family's
// stable exported errors; every other error — including the domain fence
// misses, which roll the transaction back without a durable record —
// surfaces verbatim exactly as before.
func commandError(err error) error {
	if errors.Is(err, execution.ErrCommandReused) {
		return ErrCommandReused
	}
	return err
}

func headMatches(current sql.NullInt64, expected *int64) bool {
	if expected == nil {
		return !current.Valid
	}
	return current.Valid && current.Int64 == *expected
}

// insertTurn appends one user turn inside the runner's transaction: the
// attempt row, the user message with its ordered attachment references
// and the head move, then the frozen input snapshot and grants
// (DATA-INVEST-001, DATA-ATTACH-001).
func (service *Service) insertTurn(ctx context.Context, tx writer, investigationID, principalID int64, clientCommandID, content string, attachments []resolvedAttachment, businessContext *frozenBusinessContext, selected provider, now string) (int64, int64, error) {
	// CreateOn centrally persists the command's correlation metadata onto
	// the new attempt in this same transaction (ADR-0006); a context
	// without execution metadata fails the creation.
	attemptID, err := attempt.CreateOn(ctx, tx, `
		INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
		VALUES('investigation','investigation',?,'Queued',?,?,?)`, investigationID, attempt.ReleaseVersion(), AgentVersion, now)
	if err != nil {
		return 0, 0, err
	}
	// The input freezes at this attempt's own user message (the message
	// row must exist before the snapshot can reference it).
	nextSeq, err := nextMessageSeq(ctx, tx, investigationID)
	if err != nil {
		return 0, 0, err
	}
	messageInsert, err := tx.ExecContext(ctx, `
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
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO investigation_message_attachments(message_id,attachment_id,ordinal)
			VALUES(?,?,?)`, messageID, attachment.AttachmentID, ordinal); err != nil {
			return 0, 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE investigations SET current_head_message_id=? WHERE id=?`, messageID, investigationID); err != nil {
		return 0, 0, err
	}
	if err := service.freezeInputSnapshot(ctx, tx, investigationID, attemptID, messageID, attachments, businessContext, selected, now); err != nil {
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
func (service *Service) freezeInputSnapshot(ctx context.Context, tx writer, investigationID, attemptID, turnMessageID int64, attachments []resolvedAttachment, businessContext *frozenBusinessContext, selected provider, now string) error {
	var cutoffSeq int64
	if err := tx.QueryRowContext(ctx, `
		SELECT seq FROM investigation_messages WHERE id=? AND investigation_id=?`, turnMessageID, investigationID).Scan(&cutoffSeq); err != nil {
		return err
	}
	// Freeze THIS attempt's tool catalog at creation (ADR-0004): the same
	// document is embedded in the digested input and stored on the snapshot
	// row, so later executions re-render the original bytes.
	catalogDocument, catalog, err := attempt.FrozenCatalogJSONForCreation(service.attempts.Catalogs, AgentVersion)
	if err != nil {
		return err
	}
	// ADR-0004: the enabled integrations are ALWAYS the attempt's source-
	// level authority — frozen as grant-eligible input items and rendered so
	// the model can name sourceRef. An explicit business key only adds
	// descriptive context; it can never grant or scope new work.
	integrations, err := enabledIntegrations(ctx, tx)
	if err != nil {
		return err
	}
	// Renderer v3: freeze Quoin's own recent alert history into the input.
	// Resolved occurrences vanish from instant ALERTS queries, so the model
	// must see the platform's record; the lineage keeps the digest stable.
	history, err := recentOccurrenceHistory(ctx, tx)
	if err != nil {
		return err
	}
	canonical, err := service.rebuildFor(ctx, tx, investigationID, cutoffSeq, businessContext, integrations, history, selected.ProbeResultID, catalog)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	snapshotInsert, err := tx.ExecContext(ctx, `
		INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,tool_catalog_json,created_at)
		VALUES(?,?,?,?,?,?)`, attemptID, SchemaKind, RendererVersion, hex.EncodeToString(digest[:]), string(catalogDocument), now)
	if err != nil {
		return err
	}
	snapshotID, err := snapshotInsert.LastInsertId()
	if err != nil {
		return err
	}
	itemCount, err := insertMessageLineage(ctx, tx, snapshotID, investigationID, cutoffSeq)
	if err != nil {
		return err
	}
	// History lineage: one item per frozen recent occurrence, ordered as
	// rendered (the rebuild reads the lineage back in item_seq order).
	for _, occurrence := range history {
		itemCount++
		occurrenceID, parseErr := strconv.ParseInt(occurrence.ID, 10, 64)
		if parseErr != nil {
			return parseErr
		}
		itemDigest := sha256.Sum256([]byte(fmt.Sprintf("occurrence:%d", occurrenceID)))
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,occurrence_id)
			VALUES(?,?,?,?,?)`, snapshotID, itemCount, "history_occurrence", hex.EncodeToString(itemDigest[:]), occurrenceID); err != nil {
			return err
		}
	}
	if businessContext != nil {
		// The declaration pair is frozen as descriptive context only; later
		// config publication cannot silently redirect an accepted Investigation.
		if err := insertBusinessContextLineage(ctx, tx, snapshotID, itemCount, *businessContext); err != nil {
			return err
		}
		itemCount += 2
	}
	written, err := insertSourceLineageItems(ctx, tx, snapshotID, int64(itemCount)+1)
	if err != nil {
		return err
	}
	itemCount += int(written)
	// Attachment lineage: one item per referenced artifact continuing the
	// message lineage's item_seq (the grant closure trigger requires the
	// (snapshot, artifact) pair; ARCH-CONTEXT-006 keeps the digest
	// rebuildable from durable references).
	for index, attachment := range attachments {
		itemDigest := sha256.Sum256([]byte(fmt.Sprintf("attachment:%d", attachment.AttachmentID)))
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,artifact_id)
			VALUES(?,?,?,?,?)`, snapshotID, itemCount+index+1, "attachment", hex.EncodeToString(itemDigest[:]), attachment.ArtifactID); err != nil {
			return err
		}
	}
	for _, attachment := range attachments {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO attempt_artifact_grants(attempt_id,artifact_id,source_kind,source_id,granted_at)
			VALUES(?,?,?,?,?)`, attemptID, attachment.ArtifactID, "input_snapshot", snapshotID, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at)
		VALUES(?,?,?,?,?,?,?)`,
		attemptID, "chat_model", selected.ConnectionID, selected.RevisionID, selected.CredentialGen, selected.ProbeResultID, now); err != nil {
		return err
	}
	return nil
}

// recentOccurrenceHistory selects Quoin's own most recent alert occurrences
// as the bounded history context (renderer v3). Only immutable projection
// facts render: state/resolvedAt may still change after the freeze and are
// deliberately excluded so the digest stays reproducible (ARCH-CONTEXT-006).
func recentOccurrenceHistory(ctx context.Context, tx writer) ([]RenderedRecentOccurrence, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT o.id, src.source_key, o.starts_at, o.labels_canonical
		FROM alert_occurrences o
		JOIN alert_sources src ON src.id=o.source_id
		ORDER BY o.first_seen_at DESC, o.id DESC
		LIMIT 10`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var occurrences []RenderedRecentOccurrence
	for rows.Next() {
		var id int64
		var labelsJSON string
		var occurrence RenderedRecentOccurrence
		if err := rows.Scan(&id, &occurrence.SourceKey, &occurrence.StartsAt, &labelsJSON); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(labelsJSON), &occurrence.Labels); err != nil {
			return nil, fmt.Errorf("decode recent occurrence %d labels: %w", id, err)
		}
		occurrence.ID = strconv.FormatInt(id, 10)
		occurrences = append(occurrences, occurrence)
	}
	return occurrences, rows.Err()
}

// insertMessageLineage persists one ordered input item per active-branch
// message of the turn (ARCH-CONTEXT-006: the digest covers the
// rebuildable lineage, not only the rendered bytes) and returns the number
// of items written (attachment items continue the sequence).
func insertMessageLineage(ctx context.Context, tx writer, snapshotID, investigationID, cutoffSeq int64) (int, error) {
	rows, err := tx.QueryContext(ctx, `
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
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,investigation_message_id)
			VALUES(?,?,?,?,?)`, snapshotID, seq, role, hex.EncodeToString(sourceDigest[:]), messageID); err != nil {
			return 0, err
		}
	}
	return seq, rows.Err()
}

func nextMessageSeq(ctx context.Context, tx audit.Reader, investigationID int64) (int64, error) {
	var seq sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
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
func insertSources(ctx context.Context, tx writer, investigationID, principalID int64, sources []SourceInput, now string) error {
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
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE id=?`, source.SourceID).Scan(&exists); err != nil {
			return err
		}
		if exists != 1 {
			return ErrSourceNotFound
		}
		if _, err := tx.ExecContext(ctx, `
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

// commandDigest is the deterministic command fingerprint persisted as the
// durable ledger's request digest (HTTP-COMMAND-002/003). The target prefix
// carries the scoped object (send/undo/stop/retry carry the investigation
// and, where applicable, the attempt locator) so one command id can never
// silently replay across targets; the attachment references are semantic
// request fields and must participate.
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
