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

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/config"
)

// Input is the rendered investigation_v1 snapshot: the active-branch
// messages of the turn, the immutable provenance references and the frozen
// chat contract (ARCH-AGENT-003: the worker renders these values, never
// selects them).
type Input struct {
	Messages        []MessageInput           `json:"messages"`
	Sources         []RenderedSource         `json:"sources"`
	BusinessContext *RenderedBusinessContext `json:"businessContext,omitempty"`
	// Integrations is the frozen source-level authority (ADR-0004): the
	// admin-enabled integrations at creation, rendered exactly when the
	// attempt has no business context. The declaration view narrows; this
	// list is the whole read-only scope.
	Integrations []RenderedIntegration `json:"integrations,omitempty"`
	// RecentOccurrences is Quoin's own recent alert history (renderer v3):
	// resolved occurrences disappear from instant ALERTS queries, so the
	// prompt context must carry them for "summarize current alerts" work.
	// Only immutable occurrence facts render — state/resolvedAt may still
	// change after the freeze, which would break digest reproduction.
	RecentOccurrences []RenderedRecentOccurrence `json:"recentOccurrences,omitempty"`
	ModelContract     ModelContract              `json:"modelContract"`
	// ToolCatalog is the attempt's frozen model tool catalog (ADR-0004);
	// the snapshot digest covers it via this embedding.
	ToolCatalog *attempt.FrozenCatalog `json:"toolCatalog,omitempty"`
}

// RenderedIntegration is one admin-enabled integration frozen into the
// attempt input. Kind is "metrics" (Prometheus/Thanos) or "kubernetes";
// credentials and endpoints never appear.
type RenderedIntegration struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// RenderedRecentOccurrence is one frozen history occurrence rendered into
// the investigation prompt context (immutable facts only).
type RenderedRecentOccurrence struct {
	ID        string            `json:"id"`
	SourceKey string            `json:"sourceKey"`
	StartsAt  string            `json:"startsAt"`
	Labels    map[string]string `json:"labels"`
}

// RenderedBusinessContext exposes the frozen declaration's resource choices;
// credentials, connection routing and mandatory labels remain in Quoin.
type RenderedBusinessContext struct {
	SystemKey       string                      `json:"systemKey"`
	ConfigVersionID string                      `json:"configVersionId"`
	Resources       []config.ResourceProjection `json:"resources"`
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
// so any drift here fails dispatch instead of silently diverging. The
// frozen tool catalog is read from the stored column, never re-derived
// from current enablement.
func (service *Service) RebuildInput(ctx context.Context, attemptID int64) ([]byte, error) {
	var investigationID, probeResultID int64
	var rendererVersion string
	err := service.runner.Reader().QueryRowContext(ctx, `
		SELECT a.scope_id, g.qualified_probe_result_id, s.renderer_version
		FROM execution_attempts a
		JOIN attempt_connection_grants g ON g.attempt_id=a.id AND g.purpose='chat_model'
		JOIN attempt_input_snapshots s ON s.attempt_id=a.id
		WHERE a.id=? AND a.attempt_type='investigation'`, attemptID).Scan(&investigationID, &probeResultID, &rendererVersion)
	if err != nil {
		return nil, fmt.Errorf("attempt %d investigation binding missing: %w", attemptID, err)
	}
	_, cutoffSeq, err := attemptUserMessage(ctx, service.runner.Reader(), attemptID)
	if err != nil {
		return nil, err
	}
	businessContext, err := businessContextForAttempt(ctx, service.runner.Reader(), attemptID)
	if err != nil {
		return nil, err
	}
	toolCatalog, err := attempt.FrozenToolCatalogDoc(ctx, service.runner.Reader(), attemptID)
	if err != nil {
		return nil, err
	}
	// Renderer v2 renders the frozen integrations — the source-level
	// authority of every new attempt (ADR-0004). v3 additionally renders
	// Quoin's recent alert history frozen as lineage items. v1/v2 snapshots
	// keep their exact historical bytes; old attempts are never
	// re-interpreted.
	var integrations []RenderedIntegration
	if rendererVersion != "investigation-renderer-v1" {
		integrations, err = frozenIntegrations(ctx, service.runner.Reader(), attemptID)
		if err != nil {
			return nil, err
		}
	}
	var history []RenderedRecentOccurrence
	if rendererVersion == RendererVersion {
		history, err = frozenRecentOccurrences(ctx, service.runner.Reader(), attemptID)
		if err != nil {
			return nil, err
		}
	}
	return service.rebuildFor(ctx, service.runner.Reader(), investigationID, cutoffSeq, businessContext, integrations, history, probeResultID, toolCatalog)
}

// frozenIntegrations reconstructs the frozen source-level authority from the
// attempt's item lineage: the exact connections and revisions frozen at
// creation, so later enablement churn cannot re-interpret the snapshot.
func frozenIntegrations(ctx context.Context, queries queryer, attemptID int64) ([]RenderedIntegration, error) {
	rows, err := queries.QueryContext(ctx, `
		SELECT CASE WHEN c.type='kubernetes' THEN 'kubernetes' ELSE 'metrics' END AS kind, c.name
		FROM attempt_input_snapshots snapshot
		JOIN attempt_input_items item ON item.snapshot_id=snapshot.id
			AND item.item_role IN ('metrics_source','kubernetes_source')
			AND item.connection_revision_id IS NOT NULL
		JOIN connection_revisions r ON r.id=item.connection_revision_id
		JOIN connections c ON c.id=r.connection_id
		WHERE snapshot.attempt_id=?
		ORDER BY c.name, kind`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var integrations []RenderedIntegration
	for rows.Next() {
		var integration RenderedIntegration
		if err := rows.Scan(&integration.Kind, &integration.Name); err != nil {
			return nil, err
		}
		integrations = append(integrations, integration)
	}
	return integrations, rows.Err()
}

// frozenRecentOccurrences reconstructs the frozen alert-history lineage:
// the occurrences frozen into the snapshot at creation, read back through
// the immutable lineage so later occurrence state changes cannot drift the
// digest.
func frozenRecentOccurrences(ctx context.Context, queries queryer, attemptID int64) ([]RenderedRecentOccurrence, error) {
	rows, err := queries.QueryContext(ctx, `
		SELECT o.id, src.source_key, o.starts_at, o.labels_canonical
		FROM attempt_input_snapshots snapshot
		JOIN attempt_input_items item ON item.snapshot_id=snapshot.id
			AND item.item_role='history_occurrence' AND item.occurrence_id IS NOT NULL
		JOIN alert_occurrences o ON o.id=item.occurrence_id
		JOIN alert_sources src ON src.id=o.source_id
		WHERE snapshot.attempt_id=?
		ORDER BY item.item_seq`, attemptID)
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
			return nil, fmt.Errorf("decode history occurrence %d labels: %w", id, err)
		}
		occurrence.ID = strconv.FormatInt(id, 10)
		occurrences = append(occurrences, occurrence)
	}
	return occurrences, rows.Err()
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
func (service *Service) rebuildFor(ctx context.Context, queries queryer, investigationID, cutoffSeq int64, businessContext *frozenBusinessContext, integrations []RenderedIntegration, history []RenderedRecentOccurrence, probeResultID int64, toolCatalog *attempt.FrozenCatalog) ([]byte, error) {
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
	if businessContext != nil {
		input.BusinessContext = &RenderedBusinessContext{
			SystemKey:       businessContext.SystemKey,
			ConfigVersionID: strconv.FormatInt(businessContext.ConfigVersionID, 10),
			Resources:       append([]config.ResourceProjection(nil), businessContext.Resources...),
		}
	}
	input.Integrations = integrations
	input.RecentOccurrences = history
	contract := ModelContract{}
	if err := queries.QueryRowContext(ctx, `
		SELECT chat_model_id, context_budget_tokens, max_output_tokens
		FROM model_provider_connection_probe_results WHERE probe_result_id=?`, probeResultID).
		Scan(&contract.ModelID, &contract.ContextBudgetTokens, &contract.MaxOutputTokens); err != nil {
		return nil, err
	}
	input.ModelContract = contract
	input.ToolCatalog = toolCatalog
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
