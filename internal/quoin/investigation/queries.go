package investigation

// Read projections (DATA-INVEST-005, HTTP-PAGE-005): detail embeds only
// bounded arrays and counts; the unbounded message/attempt/tool-call
// histories stay cursor-paginated subresources. last_activity_at and
// displayTitle are mechanically derived at read time from the durable rows
// — no second writable projection exists.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// titleRunes bounds the derived display title (single-line summary of the
// first active user message; DATA-INVEST-005/UI-CHAT-001).
const titleRunes = 80

// InvestigationSummary is the list projection (InvestigationSummary).
type InvestigationSummary struct {
	ID              string `json:"id"`
	DisplayTitle    string `json:"displayTitle"`
	LastActivityAt  string `json:"lastActivityAt"`
	CreatedAt       string `json:"createdAt"`
	CreatedBy       string `json:"createdBy"`
	HeadMessageID   string `json:"headMessageId,omitempty"`
	ActiveAttemptID string `json:"activeAttemptId,omitempty"`
}

// InvestigationSourceSummary is one immutable provenance reference (InvestigationSourceSummary).
type InvestigationSourceSummary struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	SourceID string `json:"sourceId"`
	LinkedBy string `json:"linkedBy,omitempty"`
	LinkedAt string `json:"linkedAt"`
}

// InvestigationDetail is the get/create response projection (InvestigationDetail).
type InvestigationDetail struct {
	InvestigationSummary
	MessageCount int64                        `json:"messageCount"`
	AttemptCount int64                        `json:"attemptCount"`
	Sources      []InvestigationSourceSummary `json:"sources"`
}

// InvestigationMessageItem is the listInvestigationMessages projection (MessageSummary).
type InvestigationMessageItem struct {
	ID              string              `json:"id"`
	Seq             int64               `json:"seq"`
	Role            string              `json:"role"`
	Status          string              `json:"status"`
	Content         string              `json:"content"`
	ParentMessageID string              `json:"parentMessageId,omitempty"`
	Attachments     []MessageAttachment `json:"attachments"`
	AttemptID       string              `json:"attemptId,omitempty"`
	EvidenceIDs     []string            `json:"evidenceIds"`
	CreatedAt       string              `json:"createdAt"`
}

// MessageAttachment is one ordered immutable attachment reference of a
// message (the TextAttachmentSummary wire shape).
type MessageAttachment struct {
	ID               string `json:"id"`
	ArtifactID       string `json:"artifactId"`
	OriginalFilename string `json:"originalFilename"`
	MediaType        string `json:"mediaType"`
	SizeBytes        int64  `json:"sizeBytes"`
	Digest           string `json:"digest"`
	BodyExpired      bool   `json:"bodyExpired"`
	CreatedAt        string `json:"createdAt"`
}

// InvestigationAttemptItem is the listInvestigationAttempts projection (AttemptSummary).
type InvestigationAttemptItem struct {
	ID                string  `json:"id"`
	Type              string  `json:"type"`
	State             string  `json:"state"`
	RowVersion        int64   `json:"rowVersion"`
	StartedAt         *string `json:"startedAt,omitempty"`
	EndedAt           *string `json:"endedAt,omitempty"`
	TerminationReason *string `json:"terminationReason,omitempty"`
	CreatedAt         string  `json:"createdAt"`
}

// InvestigationToolCallItem is the listAttemptToolCalls projection (ToolCallSummary).
type InvestigationToolCallItem struct {
	ID                 string          `json:"id"`
	AttemptID          string          `json:"attemptId"`
	ModelCallID        string          `json:"modelCallId"`
	CallSeq            int64           `json:"callSeq"`
	ToolIndex          int64           `json:"toolIndex"`
	ProviderToolCallID string          `json:"providerToolCallId"`
	ToolName           string          `json:"toolName"`
	ToolVersion        string          `json:"toolVersion"`
	Arguments          json.RawMessage `json:"arguments"`
	ExecutionMode      string          `json:"executionMode"`
	FailureMode        string          `json:"failureMode"`
	Status             string          `json:"status"`
	RowVersion         int64           `json:"rowVersion"`
	Result             json.RawMessage `json:"result,omitempty"`
	ResultArtifactID   string          `json:"resultArtifactId,omitempty"`
	ErrorDetail        string          `json:"errorDetail,omitempty"`
	StartedAt          *string         `json:"startedAt,omitempty"`
	EndedAt            *string         `json:"endedAt,omitempty"`
	CreatedAt          string          `json:"createdAt"`
}

// Get loads the investigation detail (head, active attempt, counts and the
// bounded source list).
func (service *Service) Get(ctx context.Context, investigationID int64) (InvestigationDetail, error) {
	var detail InvestigationDetail
	var headID sql.NullInt64
	var createdBy sql.NullInt64
	if err := service.db.QueryRowContext(ctx, `
		SELECT current_head_message_id, created_by, created_at FROM investigations WHERE id=?`,
		investigationID).Scan(&headID, &createdBy, &detail.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return InvestigationDetail{}, ErrNotFound
		}
		return InvestigationDetail{}, err
	}
	detail.ID = strconv.FormatInt(investigationID, 10)
	if headID.Valid {
		detail.HeadMessageID = strconv.FormatInt(headID.Int64, 10)
	}
	if createdBy.Valid {
		detail.CreatedBy = strconv.FormatInt(createdBy.Int64, 10)
	}
	activeID, err := service.attempts.ActiveAttempt(ctx, "investigation", investigationID)
	if err != nil {
		return InvestigationDetail{}, err
	}
	if activeID != 0 {
		detail.ActiveAttemptID = strconv.FormatInt(activeID, 10)
	}
	if err := service.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM investigation_messages WHERE investigation_id=?`, investigationID).Scan(&detail.MessageCount); err != nil {
		return InvestigationDetail{}, err
	}
	if err := service.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM execution_attempts WHERE scope_type='investigation' AND scope_id=?`, investigationID).Scan(&detail.AttemptCount); err != nil {
		return InvestigationDetail{}, err
	}
	sources, err := service.listSources(ctx, investigationID)
	if err != nil {
		return InvestigationDetail{}, err
	}
	detail.Sources = sources
	title, activity, err := service.deriveHead(ctx, investigationID)
	if err != nil {
		return InvestigationDetail{}, err
	}
	detail.DisplayTitle = title
	detail.LastActivityAt = activity
	return detail, nil
}

// List returns one page of investigation summaries keyed by the derived
// (last_activity_at, id) ordering (DATA-INVEST-005: the cursor carries the
// returned value and the locator, and the page compares on the same
// expression).
func (service *Service) List(ctx context.Context, after *InvestigationListCursor, limit int) ([]InvestigationSummary, *InvestigationListCursor, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	nextLimit := limit + 1
	if after == nil {
		rows, err = service.db.QueryContext(ctx, listQuery(``, `ORDER BY last_activity_at DESC, id DESC`), nextLimit)
	} else {
		rows, err = service.db.QueryContext(ctx, listQuery(` WHERE last_activity_at < ? OR (last_activity_at = ? AND id < ?)`, `ORDER BY last_activity_at DESC, id DESC`), nextLimit, after.LastActivityAt, after.LastActivityAt, after.ID)
	}
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items := []InvestigationSummary{}
	var next *InvestigationListCursor
	for rows.Next() {
		var summary InvestigationSummary
		var id int64
		if err := rows.Scan(&id, &summary.LastActivityAt, &summary.CreatedAt, &summary.CreatedBy, &summary.HeadMessageID, &summary.ActiveAttemptID); err != nil {
			return nil, nil, err
		}
		summary.ID = strconv.FormatInt(id, 10)
		if len(items) == limit {
			next = &InvestigationListCursor{ID: id, LastActivityAt: summary.LastActivityAt}
			continue
		}
		items = append(items, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	// Display titles batch off the first active user message of each item.
	if len(items) > 0 {
		if err := service.attachTitles(ctx, items); err != nil {
			return nil, nil, err
		}
	}
	return items, next, nil
}

// listQuery renders the keyset list query with the derived activity
// expression inlined once (the projection and the comparison must share
// the exact expression; DATA-INVEST-005).
func listQuery(where, order string) string {
	return `SELECT i.id, ` + listDerived("i") + ` AS last_activity_at, i.created_at,
		COALESCE(i.created_by,''), COALESCE(i.current_head_message_id,''),
		COALESCE((SELECT id FROM execution_attempts a WHERE a.scope_type='investigation' AND a.scope_id=i.id
			AND a.state IN ` + activeAttemptStates + ` LIMIT 1),'')
	FROM investigations i` + where + ` ` + order + ` LIMIT ?`
}

// InvestigationListCursor is the opaque keyset cursor for the investigation list
// (carries the returned activity value and the locator).
type InvestigationListCursor struct {
	ID             int64
	LastActivityAt string
}

// attachTitles derives displayTitle for one page: the bounded single-line
// summary of the first active user message, falling back to the immutable
// source display name or the creation time (UI-CHAT-001).
func (service *Service) attachTitles(ctx context.Context, items []InvestigationSummary) error {
	ids := make([]string, 0, len(items))
	byID := map[int64]*InvestigationSummary{}
	for index := range items {
		id, err := strconv.ParseInt(items[index].ID, 10, 64)
		if err != nil {
			return err
		}
		ids = append(ids, items[index].ID)
		byID[id] = &items[index]
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	arguments := make([]any, 0, len(ids))
	for _, id := range ids {
		arguments = append(arguments, id)
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT m.investigation_id, m.content FROM investigation_messages m
		WHERE m.status='active' AND m.role='user'
		  AND m.seq=(SELECT MIN(seq) FROM investigation_messages m2
		             WHERE m2.investigation_id=m.investigation_id AND m2.status='active' AND m2.role='user')
		  AND m.investigation_id IN (`+placeholders+`)`, arguments...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var investigationID int64
		var content string
		if err := rows.Scan(&investigationID, &content); err != nil {
			return err
		}
		if item, ok := byID[investigationID]; ok {
			item.DisplayTitle = deriveTitle(content)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Blank-content messages fall back to the source display name or the
	// creation time.
	for _, item := range items {
		if item.DisplayTitle == "" {
			item.DisplayTitle = service.fallbackTitle(ctx, item)
		}
	}
	return nil
}

func deriveTitle(content string) string {
	text := strings.Join(strings.Fields(content), " ")
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) > titleRunes {
		return string(runes[:titleRunes])
	}
	return text
}

func (service *Service) fallbackTitle(ctx context.Context, item InvestigationSummary) string {
	var kind string
	var sourceID int64
	err := service.db.QueryRowContext(ctx, `
		SELECT 'occurrence', occurrence_id FROM investigation_source_links
		WHERE investigation_id=? AND occurrence_id IS NOT NULL LIMIT 1`, item.ID).
		Scan(&kind, &sourceID)
	if err == nil {
		if name, ok := service.occurrenceDisplayName(ctx, sourceID); ok {
			return name
		}
	}
	return "新调查 " + item.CreatedAt
}

func (service *Service) occurrenceDisplayName(ctx context.Context, occurrenceID int64) (string, bool) {
	var labelsJSON string
	if err := service.db.QueryRowContext(ctx, `
		SELECT labels_canonical FROM alert_occurrences WHERE id=?`, occurrenceID).Scan(&labelsJSON); err != nil {
		return "", false
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(labelsJSON), &labels); err != nil {
		return "", false
	}
	if name := labels["alertname"]; name != "" {
		return name, true
	}
	return "", false
}

// deriveHead derives displayTitle and lastActivityAt for one detail read
// (shared expression with the list projection).
func (service *Service) deriveHead(ctx context.Context, investigationID int64) (string, string, error) {
	var activity string
	var created string
	if err := service.db.QueryRowContext(ctx, `
		SELECT `+listDerived(`i`)+`, created_at FROM investigations i WHERE i.id=?`, investigationID).
		Scan(&activity, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", ErrNotFound
		}
		return "", "", err
	}
	var content string
	err := service.db.QueryRowContext(ctx, `
		SELECT content FROM investigation_messages
		WHERE investigation_id=? AND status='active' AND role='user'
		ORDER BY seq LIMIT 1`, investigationID).Scan(&content)
	title := ""
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return "", "", err
	default:
		title = deriveTitle(content)
	}
	if title == "" {
		// One fallback rule for list and detail reads (UI-CHAT-001).
		title = service.fallbackTitle(ctx, InvestigationSummary{ID: strconv.FormatInt(investigationID, 10), CreatedAt: created})
	}
	return title, activity, nil
}

func listDerived(alias string) string {
	return `COALESCE(
		(SELECT MAX(t) FROM (
			SELECT created_at AS t FROM investigation_messages WHERE investigation_id=` + alias + `.id AND status='active'
			UNION ALL SELECT created_at FROM execution_attempts WHERE scope_type='investigation' AND scope_id=` + alias + `.id
			UNION ALL SELECT accepted_at FROM execution_attempts WHERE scope_type='investigation' AND scope_id=` + alias + `.id AND accepted_at IS NOT NULL
			UNION ALL SELECT started_at FROM execution_attempts WHERE scope_type='investigation' AND scope_id=` + alias + `.id AND started_at IS NOT NULL
			UNION ALL SELECT ended_at FROM execution_attempts WHERE scope_type='investigation' AND scope_id=` + alias + `.id AND ended_at IS NOT NULL
		)),
		` + alias + `.created_at)`
}

// listSources projects the bounded immutable provenance list.
func (service *Service) listSources(ctx context.Context, investigationID int64) ([]InvestigationSourceSummary, error) {
	rows, err := service.db.QueryContext(ctx, `
		SELECT id, occurrence_id, initial_analysis_id, evidence_id, inspection_report_id, linked_by, linked_at
		FROM investigation_source_links WHERE investigation_id=? ORDER BY id`, investigationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sources := []InvestigationSourceSummary{}
	for rows.Next() {
		var linkID, linkedBy int64
		var occurrenceID, analysisID, evidenceID, reportID sql.NullInt64
		var linkedAt string
		if err := rows.Scan(&linkID, &occurrenceID, &analysisID, &evidenceID, &reportID, &linkedBy, &linkedAt); err != nil {
			return nil, err
		}
		source := InvestigationSourceSummary{
			ID:       strconv.FormatInt(linkID, 10),
			LinkedBy: strconv.FormatInt(linkedBy, 10),
			LinkedAt: linkedAt,
		}
		switch {
		case occurrenceID.Valid:
			source.Type = "occurrence"
			source.SourceID = strconv.FormatInt(occurrenceID.Int64, 10)
		case analysisID.Valid:
			source.Type = "initial_analysis"
			source.SourceID = strconv.FormatInt(analysisID.Int64, 10)
		case evidenceID.Valid:
			source.Type = "evidence"
			source.SourceID = strconv.FormatInt(evidenceID.Int64, 10)
		case reportID.Valid:
			source.Type = "inspection_report"
			source.SourceID = strconv.FormatInt(reportID.Int64, 10)
		}
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

// ListMessages pages the message history in seq order (HTTP-PAGE-005;
// keyset on the unique seq).
