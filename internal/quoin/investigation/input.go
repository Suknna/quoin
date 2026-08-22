package investigation

// Deterministic input projection (ARCH-CONTEXT-006): the snapshot row
// stores only the schema kind and digest; the canonical investigation_v1
// bytes are rebuilt on demand from the durable message/source references
// and the attempt's frozen chat contract, and must reproduce the frozen
// digest exactly before any dispatch. Every rendered fact is immutable at
// the time the snapshot freezes (message rows are append-only, provenance
// references never change, the chat contract is bound to the attempt).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Input is the rendered investigation_v1 snapshot: the active-branch
// messages of the turn, the immutable provenance references and the frozen
// chat contract (ARCH-AGENT-003: the worker renders these values, never
// selects them).
type Input struct {
	Messages      []MessageInput   `json:"messages"`
	Sources       []RenderedSource `json:"sources"`
	ModelContract ModelContract    `json:"modelContract"`
}

// MessageInput is one active-branch message of the turn; user messages
// may carry their ordered immutable attachment references (locator facts
// only — the bodies stay behind the granted artifact_read/grep tools).
type MessageInput struct {
	Role        string            `json:"role"` // user | assistant
	Content     string            `json:"content"`
	Attachments []InputAttachment `json:"attachments,omitempty"`
	// id is the durable locator used only while assembling the canonical
	// projection; it never serializes (the frozen investigation_v1 shape
	// carries role/content/attachments).
	id int64 `json:"-"`
}

// InputAttachment is the model-facing locator projection of one message
// attachment (the frozen artifact identity, never the body).
type InputAttachment struct {
	Filename   string `json:"filename"`
	ArtifactID string `json:"artifactId"`
	SizeBytes  int64  `json:"sizeBytes"`
}

// RenderedSource is one immutable provenance reference with its frozen
// context (references only — never source bodies; the create command
// resolves the display facts once and the rows never change).
type RenderedSource struct {
	Type     string          `json:"type"`
	SourceID string          `json:"sourceId"`
	Context  json.RawMessage `json:"context"`
}

// ModelContract is the frozen chat contract of the attempt.
type ModelContract struct {
	ModelID             string `json:"modelId"`
	ContextBudgetTokens int    `json:"contextBudgetTokens"`
	MaxOutputTokens     int    `json:"maxOutputTokens"`
}

// attachInputAttachments renders every message's ordered attachment
// references onto the input projection (DATA-ATTACH-001: the send
// transaction owns the ordinal; the projection only reads it back).
func attachInputAttachments(ctx context.Context, queries queryer, messages []MessageInput, messageIDs []int64) error {
	if len(messageIDs) == 0 {
		return nil
	}
	placeholders := make([]string, 0, len(messageIDs))
	arguments := make([]any, 0, len(messageIDs))
	for _, id := range messageIDs {
		placeholders = append(placeholders, "?")
		arguments = append(arguments, id)
	}
	rows, err := queries.QueryContext(ctx, `
		SELECT a.message_id, t.original_filename, t.artifact_id, t.size_bytes
		FROM investigation_message_attachments a
		JOIN text_attachments t ON t.id=a.attachment_id
		WHERE a.message_id IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY a.message_id, a.ordinal`, arguments...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[int64]*MessageInput, len(messages))
	for index := range messages {
		byID[messages[index].id] = &messages[index]
	}
	for rows.Next() {
		var messageID, artifactID int64
		var attachment InputAttachment
		if err := rows.Scan(&messageID, &attachment.Filename, &artifactID, &attachment.SizeBytes); err != nil {
			return err
		}
		attachment.ArtifactID = strconv.FormatInt(artifactID, 10)
		if message, ok := byID[messageID]; ok {
			message.Attachments = append(message.Attachments, attachment)
		}
	}
	return rows.Err()
}

type occurrenceSourceContext struct {
	ID          string            `json:"id"`
	FirstSeenAt string            `json:"firstSeenAt"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type analysisSourceContext struct {
	ID           string `json:"id"`
	OccurrenceID string `json:"occurrenceId"`
	CreatedAt    string `json:"createdAt"`
}

type evidenceSourceContext struct {
	ID         string `json:"id"`
	TargetType string `json:"targetType"`
	TargetID   string `json:"targetId"`
	ObservedAt string `json:"observedAt"`
}

type reportSourceContext struct {
	ID        string `json:"id"`
	RunID     string `json:"runId"`
	Version   int64  `json:"version"`
	CreatedAt string `json:"createdAt"`
}

// provider is the resolved enabled model provider contract for one attempt.
type provider struct {
	ConnectionID  int64
	RevisionID    int64
	CredentialGen int64
	ProbeResultID int64
	ChatModelID   string
	ContextBudget int
	MaxOutput     int
}

// RebuildInput rebuilds the canonical investigation_v1 input for one
// attempt from its message lineage and the attempt's frozen chat_model
// grant. The dispatch path verifies the digest against the snapshot row,
// so any drift here fails dispatch instead of silently diverging.
func (service *Service) RebuildInput(ctx context.Context, attemptID int64) ([]byte, error) {
	var investigationID int64
	var probeResultID int64
	err := service.db.QueryRowContext(ctx, `
		SELECT a.scope_id, g.qualified_probe_result_id
		FROM execution_attempts a
		JOIN attempt_connection_grants g ON g.attempt_id=a.id AND g.purpose='chat_model'
		WHERE a.id=? AND a.attempt_type='investigation'`, attemptID).Scan(&investigationID, &probeResultID)
	if err != nil {
		return nil, fmt.Errorf("attempt %d investigation binding missing: %w", attemptID, err)
	}
	_, cutoffSeq, err := attemptUserMessage(ctx, service.db, attemptID)
	if err != nil {
		return nil, err
	}
	return service.rebuildFor(ctx, service.db, investigationID, cutoffSeq, probeResultID)
}

// attemptUserMessage resolves the user message an attempt answers. Send
// and create attempts own their message row (attempt_id points back), so
// the fast path reads it directly; a retry attempt deliberately reuses
// the failed attempt's message (the frozen schema keeps one user message
// per attempt), so the fallback resolves the cutoff through the frozen
// input lineage instead (DATA-INVEST-002).
func attemptUserMessage(ctx context.Context, queries queryer, attemptID int64) (int64, int64, error) {
	var messageID, seq int64
	err := queries.QueryRowContext(ctx, `
		SELECT id, seq FROM investigation_messages WHERE attempt_id=? AND role='user'`, attemptID).
		Scan(&messageID, &seq)
	if err == nil {
		return messageID, seq, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, err
	}
	err = queries.QueryRowContext(ctx, `
		SELECT i.investigation_message_id, m.seq
		FROM attempt_input_items i
		JOIN attempt_input_snapshots s ON s.id=i.snapshot_id
		JOIN investigation_messages m ON m.id=i.investigation_message_id
		WHERE s.attempt_id=? AND i.item_role='user'
		ORDER BY i.item_seq DESC LIMIT 1`, attemptID).Scan(&messageID, &seq)
	if err != nil {
		return 0, 0, fmt.Errorf("attempt %d user message missing: %w", attemptID, err)
	}
	return messageID, seq, nil
}

// rebuildFor renders the canonical input for one attempt from the
// investigation's durable rows; the message set freezes at the turn's
// cutoff seq (create/send/retry pass their own user message, dispatch
// rebuilds resolve it through attemptUserMessage).
func (service *Service) rebuildFor(ctx context.Context, queries queryer, investigationID, cutoffSeq, probeResultID int64) ([]byte, error) {
	var input Input
	rows, err := queries.QueryContext(ctx, `
		SELECT id, role, content FROM investigation_messages
		WHERE investigation_id=? AND status='active' AND seq<=?
		ORDER BY seq`, investigationID, cutoffSeq)
	if err != nil {
		return nil, err
	}
	messageIDs := []int64{}
	for rows.Next() {
		var id int64
		var message MessageInput
		if err := rows.Scan(&id, &message.Role, &message.Content); err != nil {
			rows.Close()
			return nil, err
		}
		message.id = id
		input.Messages = append(input.Messages, message)
		messageIDs = append(messageIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(input.Messages) == 0 {
		return nil, errors.New("investigation input renders no active messages")
	}
	if err := attachInputAttachments(ctx, queries, input.Messages, messageIDs); err != nil {
		return nil, err
	}
	sources, err := service.renderSources(ctx, queries, investigationID)
	if err != nil {
		return nil, err
	}
	input.Sources = sources
	contract := ModelContract{}
	if err := queries.QueryRowContext(ctx, `
		SELECT chat_model_id, context_budget_tokens, max_output_tokens
		FROM model_provider_connection_probe_results WHERE probe_result_id=?`, probeResultID).
		Scan(&contract.ModelID, &contract.ContextBudgetTokens, &contract.MaxOutputTokens); err != nil {
		return nil, err
	}
	input.ModelContract = contract
	return json.Marshal(input)
}

// renderSources projects the immutable provenance references with their
// frozen context facts (immutable columns only, so the digest stays
// reproducible forever).
func (service *Service) renderSources(ctx context.Context, queries queryer, investigationID int64) ([]RenderedSource, error) {
	rows, err := queries.QueryContext(ctx, `
		SELECT id, occurrence_id, initial_analysis_id, evidence_id, inspection_report_id
		FROM investigation_source_links WHERE investigation_id=? ORDER BY id`, investigationID)
	if err != nil {
		return nil, err
	}
	// The link rows must be fully drained before the per-source context
	// queries run: on a single-connection pool an open rows object pins
	// the connection and a nested query would deadlock.
	type linkRef struct {
		linkID                                         int64
		occurrenceID, analysisID, evidenceID, reportID sql.NullInt64
	}
	var refs []linkRef
	for rows.Next() {
		var ref linkRef
		if err := rows.Scan(&ref.linkID, &ref.occurrenceID, &ref.analysisID, &ref.evidenceID, &ref.reportID); err != nil {
			rows.Close()
			return nil, err
		}
		refs = append(refs, ref)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var sources []RenderedSource
	for _, ref := range refs {
		var source RenderedSource
		switch {
		case ref.occurrenceID.Valid:
			source, err = service.renderOccurrenceSource(ctx, queries, ref.occurrenceID.Int64)
		case ref.analysisID.Valid:
			source, err = service.renderAnalysisSource(ctx, queries, ref.analysisID.Int64)
		case ref.evidenceID.Valid:
			source, err = service.renderEvidenceSource(ctx, queries, ref.evidenceID.Int64)
		case ref.reportID.Valid:
			source, err = service.renderReportSource(ctx, queries, ref.reportID.Int64)
		}
		if err != nil {
			return nil, fmt.Errorf("investigation source %d: %w", ref.linkID, err)
		}
		sources = append(sources, source)
	}
	return sources, nil
}

func (service *Service) renderOccurrenceSource(ctx context.Context, queries queryer, occurrenceID int64) (RenderedSource, error) {
	var context occurrenceSourceContext
	var labelsJSON string
	if err := queries.QueryRowContext(ctx, `
		SELECT first_seen_at, labels_canonical FROM alert_occurrences WHERE id=?`, occurrenceID).
		Scan(&context.FirstSeenAt, &labelsJSON); err != nil {
		return RenderedSource{}, err
	}
	context.ID = strconv.FormatInt(occurrenceID, 10)
	if err := json.Unmarshal([]byte(labelsJSON), &context.Labels); err != nil {
		return RenderedSource{}, err
	}
	encoded, err := json.Marshal(context)
	if err != nil {
		return RenderedSource{}, err
	}
	return RenderedSource{Type: "occurrence", SourceID: context.ID, Context: encoded}, nil
}

func (service *Service) renderAnalysisSource(ctx context.Context, queries queryer, analysisID int64) (RenderedSource, error) {
	var context analysisSourceContext
	if err := queries.QueryRowContext(ctx, `
		SELECT occurrence_id, created_at FROM initial_analyses WHERE id=?`, analysisID).
		Scan(&context.OccurrenceID, &context.CreatedAt); err != nil {
		return RenderedSource{}, err
	}
	context.ID = strconv.FormatInt(analysisID, 10)
	encoded, err := json.Marshal(context)
	if err != nil {
		return RenderedSource{}, err
	}
	return RenderedSource{Type: "initial_analysis", SourceID: context.ID, Context: encoded}, nil
}

func (service *Service) renderEvidenceSource(ctx context.Context, queries queryer, evidenceID int64) (RenderedSource, error) {
	var context evidenceSourceContext
	var targetID int64
	if err := queries.QueryRowContext(ctx, `
		SELECT target_type, target_id, observed_at FROM evidence WHERE id=?`, evidenceID).
		Scan(&context.TargetType, &targetID, &context.ObservedAt); err != nil {
		return RenderedSource{}, err
	}
	context.ID = strconv.FormatInt(evidenceID, 10)
	context.TargetID = strconv.FormatInt(targetID, 10)
	encoded, err := json.Marshal(context)
	if err != nil {
		return RenderedSource{}, err
	}
	return RenderedSource{Type: "evidence", SourceID: context.ID, Context: encoded}, nil
}

func (service *Service) renderReportSource(ctx context.Context, queries queryer, reportID int64) (RenderedSource, error) {
	var context reportSourceContext
	var runID int64
	if err := queries.QueryRowContext(ctx, `
		SELECT run_id, version, created_at FROM inspection_reports WHERE id=?`, reportID).
		Scan(&runID, &context.Version, &context.CreatedAt); err != nil {
		return RenderedSource{}, err
	}
	context.ID = strconv.FormatInt(reportID, 10)
	context.RunID = strconv.FormatInt(runID, 10)
	encoded, err := json.Marshal(context)
	if err != nil {
		return RenderedSource{}, err
	}
	return RenderedSource{Type: "inspection_report", SourceID: context.ID, Context: encoded}, nil
}

// selectModelProvider resolves the single enabled model provider and its
// qualification (DATA-CONN-003: one enabled provider; the explicit
// qualification must close onto the current pair).
func selectModelProvider(ctx context.Context, queries queryer) (provider, error) {
	var selected provider
	var qualificationRowVersion, connectionRowVersion int64
	var probeOutcome string
	err := queries.QueryRowContext(ctx, `
		SELECT c.id, c.current_revision_id, c.current_credential_generation_id,
		       q.probe_result_id, q.enabled_row_version, c.row_version, p.outcome
		FROM connections c
		JOIN connection_enable_qualifications q ON q.connection_id=c.id
		JOIN connection_probe_results p ON p.id=q.probe_result_id
		WHERE c.type='model_provider' AND c.enabled=1 AND c.revalidation_required=0
		ORDER BY q.id DESC LIMIT 1`).
		Scan(&selected.ConnectionID, &selected.RevisionID, &selected.CredentialGen,
			&selected.ProbeResultID, &qualificationRowVersion, &connectionRowVersion, &probeOutcome)
	if errors.Is(err, sql.ErrNoRows) {
		return provider{}, ErrModelProviderMissing
	}
	if err != nil {
		return provider{}, err
	}
	if qualificationRowVersion != connectionRowVersion || probeOutcome != "passed" {
		return provider{}, ErrModelProviderMissing
	}
	var nativeToolCalling bool
	if err := queries.QueryRowContext(ctx, `
		SELECT chat_model_id, context_budget_tokens, max_output_tokens, native_tool_calling_supported
		FROM model_provider_connection_probe_results WHERE probe_result_id=?`,
		selected.ProbeResultID).
		Scan(&selected.ChatModelID, &selected.ContextBudget, &selected.MaxOutput, &nativeToolCalling); err != nil {
		return provider{}, err
	}
	if selected.ChatModelID == "" || !nativeToolCalling {
		return provider{}, ErrModelProviderMissing
	}
	return selected, nil
}

// queryer is the minimal query surface the rebuild needs (a pool handle
// outside transactions, the transaction connection inside them — SQLite is
// single-writer and a nested pool fetch would deadlock).
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}
