package investigation

// Cursor-paginated subresource projections (HTTP-PAGE-005): the unbounded
// message/attempt/tool-call histories of one investigation page by keyset
// (message seq / insertion-ordered id); each page carries only bounded
// result previews (artifact bodies stay behind their own endpoints).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

func (service *Service) ListMessages(ctx context.Context, investigationID int64, afterSeq int64, limit int) ([]InvestigationMessageItem, bool, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT id, seq, role, status, content, parent_message_id, attempt_id, created_at
		FROM investigation_messages WHERE investigation_id=? AND seq>?
		ORDER BY seq LIMIT ?`, investigationID, afterSeq, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items := []InvestigationMessageItem{}
	hasMore := false
	for rows.Next() {
		var item InvestigationMessageItem
		var parentID, attemptID sql.NullInt64
		var id int64
		if err := rows.Scan(&id, &item.Seq, &item.Role, &item.Status, &item.Content, &parentID, &attemptID, &item.CreatedAt); err != nil {
			return nil, false, err
		}
		if len(items) == limit {
			hasMore = true
			continue
		}
		item.ID = strconv.FormatInt(id, 10)
		item.Attachments = []MessageAttachment{}
		if parentID.Valid {
			item.ParentMessageID = strconv.FormatInt(parentID.Int64, 10)
		}
		if attemptID.Valid {
			item.AttemptID = strconv.FormatInt(attemptID.Int64, 10)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if err := service.attachEvidence(ctx, items); err != nil {
		return nil, false, err
	}
	if err := service.attachAttachments(ctx, items); err != nil {
		return nil, false, err
	}
	return items, hasMore, nil
}

// attachAttachments renders every message's ordered attachment summaries
// (the send transaction owns the ordinal; this projection only reads).
func (service *Service) attachAttachments(ctx context.Context, items []InvestigationMessageItem) error {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	arguments := make([]any, 0, len(ids))
	for _, id := range ids {
		arguments = append(arguments, id)
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT a.message_id, t.id, t.artifact_id, t.original_filename, ar.media_type,
		       t.size_bytes, t.digest, ar.body_expired, t.uploaded_at
		FROM investigation_message_attachments a
		JOIN text_attachments t ON t.id=a.attachment_id
		JOIN artifacts ar ON ar.id=t.artifact_id
		WHERE a.message_id IN (`+placeholders+`)
		ORDER BY a.message_id, a.ordinal`, arguments...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := map[string]*InvestigationMessageItem{}
	for index := range items {
		byID[items[index].ID] = &items[index]
	}
	for rows.Next() {
		var messageID, attachmentID, artifactID int64
		var expired int
		var attachment MessageAttachment
		if err := rows.Scan(&messageID, &attachmentID, &artifactID, &attachment.OriginalFilename,
			&attachment.MediaType, &attachment.SizeBytes, &attachment.Digest, &expired, &attachment.CreatedAt); err != nil {
			return err
		}
		attachment.ID = strconv.FormatInt(attachmentID, 10)
		attachment.ArtifactID = strconv.FormatInt(artifactID, 10)
		attachment.BodyExpired = expired == 1
		if item, ok := byID[strconv.FormatInt(messageID, 10)]; ok {
			item.Attachments = append(item.Attachments, attachment)
		}
	}
	return rows.Err()
}

func (service *Service) attachEvidence(ctx context.Context, items []InvestigationMessageItem) error {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	arguments := make([]any, 0, len(ids))
	for _, id := range ids {
		arguments = append(arguments, id)
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT message_id, evidence_id FROM investigation_message_evidence
		WHERE message_id IN (`+placeholders+`) ORDER BY message_id, ordinal`, arguments...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := map[string]*InvestigationMessageItem{}
	for index := range items {
		byID[items[index].ID] = &items[index]
	}
	for rows.Next() {
		var messageID, evidenceID int64
		if err := rows.Scan(&messageID, &evidenceID); err != nil {
			return err
		}
		if item, ok := byID[strconv.FormatInt(messageID, 10)]; ok {
			item.EvidenceIDs = append(item.EvidenceIDs, strconv.FormatInt(evidenceID, 10))
		}
	}
	return rows.Err()
}

// ListAttempts pages the attempt history in reverse id order (createdAt
// order; keyset on the id).
func (service *Service) ListAttempts(ctx context.Context, investigationID int64, afterID int64, limit int) ([]InvestigationAttemptItem, bool, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT id, attempt_type, state, row_version, started_at, ended_at, termination_reason, created_at
		FROM execution_attempts
		WHERE scope_type='investigation' AND scope_id=? AND id>?
		ORDER BY id LIMIT ?`, investigationID, afterID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items := []InvestigationAttemptItem{}
	hasMore := false
	for rows.Next() {
		var item InvestigationAttemptItem
		var id int64
		if err := rows.Scan(&id, &item.Type, &item.State, &item.RowVersion, &item.StartedAt, &item.EndedAt, &item.TerminationReason, &item.CreatedAt); err != nil {
			return nil, false, err
		}
		if len(items) == limit {
			hasMore = true
			continue
		}
		item.ID = strconv.FormatInt(id, 10)
		items = append(items, item)
	}
	return items, hasMore, rows.Err()
}

// ListToolCalls pages one attempt's tool-call timeline in (call_seq,
// tool_index) order (keyset on the insertion-ordered id).
func (service *Service) ListToolCalls(ctx context.Context, attemptID int64, afterID int64, limit int) ([]InvestigationToolCallItem, bool, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT id, attempt_id, model_call_id, call_seq, tool_index, provider_tool_call_id,
		       tool_name, tool_version, arguments_json, execution_mode, failure_mode, status,
		       row_version, result_json, result_artifact_id, error_detail, started_at, ended_at, created_at
		FROM tool_calls WHERE attempt_id=? AND id>?
		ORDER BY id LIMIT ?`, attemptID, afterID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items := []InvestigationToolCallItem{}
	hasMore := false
	for rows.Next() {
		var item InvestigationToolCallItem
		var id, attemptRowID, modelCallID int64
		var resultArtifact sql.NullInt64
		var resultJSON, errorDetail sql.NullString
		var argumentsJSON string
		if err := rows.Scan(&id, &attemptRowID, &modelCallID, &item.CallSeq, &item.ToolIndex,
			&item.ProviderToolCallID, &item.ToolName, &item.ToolVersion, &argumentsJSON,
			&item.ExecutionMode, &item.FailureMode, &item.Status, &item.RowVersion,
			&resultJSON, &resultArtifact, &errorDetail, &item.StartedAt, &item.EndedAt, &item.CreatedAt); err != nil {
			return nil, false, err
		}
		if len(items) == limit {
			hasMore = true
			continue
		}
		item.ID = strconv.FormatInt(id, 10)
		item.AttemptID = strconv.FormatInt(attemptRowID, 10)
		item.ModelCallID = strconv.FormatInt(modelCallID, 10)
		item.Arguments = json.RawMessage(argumentsJSON)
		if resultJSON.Valid {
			item.Result = json.RawMessage(resultJSON.String)
		}
		if resultArtifact.Valid {
			item.ResultArtifactID = strconv.FormatInt(resultArtifact.Int64, 10)
		}
		item.ErrorDetail = errorDetail.String
		items = append(items, item)
	}
	return items, hasMore, rows.Err()
}

// MessageFor loads one message with its investigation membership check
// (stream attachment and read paths).
func (service *Service) MessageFor(ctx context.Context, investigationID, messageID int64) (InvestigationMessageItem, error) {
	var item InvestigationMessageItem
	var id int64
	var parentID, attemptID sql.NullInt64
	err := service.db.QueryRowContext(ctx, `
		SELECT id, seq, role, status, content, parent_message_id, attempt_id, created_at
		FROM investigation_messages WHERE id=? AND investigation_id=?`, messageID, investigationID).
		Scan(&id, &item.Seq, &item.Role, &item.Status, &item.Content, &parentID, &attemptID, &item.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return InvestigationMessageItem{}, ErrNotFound
	}
	if err != nil {
		return InvestigationMessageItem{}, err
	}
	item.ID = strconv.FormatInt(id, 10)
	item.Attachments = []MessageAttachment{}
	if parentID.Valid {
		item.ParentMessageID = strconv.FormatInt(parentID.Int64, 10)
	}
	if attemptID.Valid {
		item.AttemptID = strconv.FormatInt(attemptID.Int64, 10)
	}
	// attachAttachments mutates through the slice's map, so read the item
	// back from the same slice it filled.
	projection := []InvestigationMessageItem{item}
	if err := service.attachAttachments(ctx, projection); err != nil {
		return InvestigationMessageItem{}, err
	}
	return projection[0], nil
}

// MessageAttempt resolves the attempt bound to one user message
// (stream attachment; HTTP-STREAM-004).
func (service *Service) MessageAttempt(ctx context.Context, investigationID, messageID int64) (int64, error) {
	var attemptID sql.NullInt64
	err := service.db.QueryRowContext(ctx, `
		SELECT attempt_id FROM investigation_messages WHERE id=? AND investigation_id=? AND role='user'`,
		messageID, investigationID).Scan(&attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if !attemptID.Valid {
		return 0, ErrNotFound
	}
	return attemptID.Int64, nil
}
