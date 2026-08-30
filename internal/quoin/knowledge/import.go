package knowledge

// Import batches are the only path by which pasted source material reaches an
// agent. The model proposes drafts; it never inserts reusable knowledge. A
// human confirmation remains the publication boundary.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/auth"
)

const (
	SourceMaterial         = "source_material"
	SourceKnowledgeVersion = "knowledge_version"
	importInputKind        = "knowledge_extraction_v1"
	importRendererVersion  = "knowledge-extraction-v1"
	importResultKind       = "knowledge_extraction_result_v1"
	ledgerImport           = "knowledge_import.create"
	ledgerBatchConfirm     = "knowledge_import.confirm"
	ledgerBatchCancel      = "knowledge_import.cancel"
	ledgerStopReuse        = "knowledge_version.stop_reuse"
	ledgerCreateRevision   = "knowledge_revision.create"
)

var (
	// ErrModelProviderMissing is deliberately a service-unavailable condition:
	// no source/batch/attempt is stored before a qualifying provider exists.
	ErrModelProviderMissing = errors.New("no enabled qualified model provider")
	ErrEmptyImport          = errors.New("knowledge import text must not be empty")
	ErrInvalidExtraction    = errors.New("invalid knowledge extraction result")
)

// ImportBatchSummary is the durable batch projection.
type ImportBatchSummary struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	RowVersion int64  `json:"rowVersion"`
	Generation int64  `json:"generation"`
	CreatedAt  string `json:"createdAt"`
}

// ImportBatchDetail includes the model-created candidates. Candidates are
// retained after cancellation for audit, but cancellation fences edits and
// confirmation.
type ImportBatchDetail struct {
	ImportBatchSummary
	Candidates []CandidateSummary `json:"candidates"`
}

// ImportResult makes the newly queued attempt observable to the runtime
// dispatcher without exposing it on the HTTP contract.
type ImportResult struct {
	Batch     ImportBatchDetail
	AttemptID int64
}

type importProvider struct {
	ConnectionID  int64
	RevisionID    int64
	CredentialGen int64
	ProbeResultID int64
	ChatModelID   string
	ContextBudget int64
	MaxOutput     int64
}

type importInput struct {
	SchemaKind       string `json:"schemaKind"`
	AttemptID        int64  `json:"attemptId"`
	BatchID          int64  `json:"batchId"`
	Generation       int64  `json:"generation"`
	SourceMaterialID int64  `json:"sourceMaterialId"`
	Text             string `json:"text"`
	ModelContract    struct {
		ModelID             string `json:"modelId"`
		ContextBudgetTokens int64  `json:"contextBudgetTokens"`
		MaxOutputTokens     int64  `json:"maxOutputTokens"`
	} `json:"modelContract"`
}

// extractionProposal is intentionally closed: the worker returns only the
// draft fields Quoin can validate and persist. The proposal never names an
// aggregate or a current pointer.
type extractionProposal struct {
	SchemaKind  string `json:"schemaKind"`
	AttemptID   int64  `json:"attemptId"`
	BatchID     int64  `json:"batchId"`
	ModelCallID int64  `json:"modelCallId"`
	Items       []struct {
		Title string          `json:"title"`
		Body  string          `json:"body"`
		Scope json.RawMessage `json:"scope,omitempty"`
	} `json:"items"`
}

// StartImport persists the source material, Processing batch and a frozen
// chat attempt in one transaction. Nothing is written if provider selection
// fails, preventing stranded batches which cannot be processed.
func (service *Service) StartImport(ctx context.Context, principalID int64, commandID, text string) (ImportResult, error) {
	return service.StartImportAs(ctx, MutationActor{ID: principalID}, commandID, text)
}

func (service *Service) StartImportAs(ctx context.Context, actor MutationActor, commandID, text string) (ImportResult, error) {
	principalID := actor.ID
	empty := strings.TrimSpace(text) == ""
	digest := commandDigest(ledgerImport, map[string]any{"text": text})
	if record, ok, err := auth.LookupCommand(ctx, service.db, principalID, commandID); err != nil {
		return ImportResult{}, err
	} else if ok {
		return replayImport(record, digest)
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return ImportResult{}, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return ImportResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if err := verifyMutationActorOn(ctx, conn, actor); err != nil {
		return ImportResult{}, err
	}
	if record, ok, err := authLookup(ctx, conn, principalID, commandID); err != nil {
		return ImportResult{}, err
	} else if ok {
		result, replayErr := replayImport(record, digest)
		if replayErr == nil {
			_, replayErr = conn.ExecContext(ctx, "COMMIT")
			committed = replayErr == nil
		}
		return result, replayErr
	}
	if empty {
		return ImportResult{}, rejectScopedCommand(ctx, conn, principalID, commandID, ledgerImport, digest, "knowledge_import_batch", 0, service.nowText(), ErrEmptyImport)
	}
	provider, err := selectImportProvider(ctx, conn)
	if err != nil {
		return ImportResult{}, err
	}
	now := service.nowText()
	materialDigest := sha256.Sum256([]byte(text))
	material, err := conn.ExecContext(ctx, `INSERT INTO source_materials(kind,digest,size_bytes,content,created_by,created_at)
		VALUES('knowledge_import',?,?,?,?,?)`, hex.EncodeToString(materialDigest[:]), len([]byte(text)), text, principalID, now)
	if err != nil {
		return ImportResult{}, err
	}
	materialID, err := material.LastInsertId()
	if err != nil {
		return ImportResult{}, err
	}
	batch, err := conn.ExecContext(ctx, `INSERT INTO knowledge_import_batches(source_material_id,state,created_by,created_at)
		VALUES(?, 'Processing', ?, ?)`, materialID, principalID, now)
	if err != nil {
		return ImportResult{}, err
	}
	batchID, err := batch.LastInsertId()
	if err != nil {
		return ImportResult{}, err
	}
	attemptID, err := service.insertImportAttempt(ctx, conn, batchID, materialID, text, provider, now)
	if err != nil {
		return ImportResult{}, err
	}
	batchRowVersion := int64(1)
	if err := recordAudit(ctx, conn, principalID, commandID, ledgerImport, "knowledge_import_batch", batchID, &batchRowVersion, now); err != nil {
		return ImportResult{}, err
	}
	detail, err := scanBatchDetailOn(ctx, conn, batchID)
	if err != nil {
		return ImportResult{}, err
	}
	payload, err := json.Marshal(ImportResult{Batch: detail, AttemptID: attemptID})
	if err != nil {
		return ImportResult{}, err
	}
	if err := recordCommand(ctx, conn, principalID, commandID, ledgerImport, digest, authOutcomeCommitted, "knowledge_import_batch", batchID, string(payload)); err != nil {
		return ImportResult{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return ImportResult{}, err
	}
	committed = true
	return ImportResult{Batch: detail, AttemptID: attemptID}, nil
}

func selectImportProvider(ctx context.Context, conn *sql.Conn) (importProvider, error) {
	var selected importProvider
	var qualificationVersion, connectionVersion int64
	var outcome string
	err := conn.QueryRowContext(ctx, `SELECT c.id,c.current_revision_id,c.current_credential_generation_id,q.probe_result_id,q.enabled_row_version,c.row_version,p.outcome
		FROM connections c JOIN connection_enable_qualifications q ON q.connection_id=c.id
		JOIN connection_probe_results p ON p.id=q.probe_result_id
		WHERE c.type='model_provider' AND c.enabled=1 AND c.revalidation_required=0 ORDER BY q.id DESC LIMIT 1`).
		Scan(&selected.ConnectionID, &selected.RevisionID, &selected.CredentialGen, &selected.ProbeResultID, &qualificationVersion, &connectionVersion, &outcome)
	if errors.Is(err, sql.ErrNoRows) {
		return selected, ErrModelProviderMissing
	}
	if err != nil {
		return selected, err
	}
	if qualificationVersion != connectionVersion || outcome != "passed" {
		return selected, ErrModelProviderMissing
	}
	var nativeTools bool
	err = conn.QueryRowContext(ctx, `SELECT chat_model_id,context_budget_tokens,max_output_tokens,native_tool_calling_supported
		FROM model_provider_connection_probe_results WHERE probe_result_id=?`, selected.ProbeResultID).
		Scan(&selected.ChatModelID, &selected.ContextBudget, &selected.MaxOutput, &nativeTools)
	if err != nil {
		return selected, err
	}
	if selected.ChatModelID == "" || !nativeTools {
		return selected, ErrModelProviderMissing
	}
	return selected, nil
}

func (service *Service) insertImportAttempt(ctx context.Context, conn *sql.Conn, batchID, materialID int64, text string, provider importProvider, now string) (int64, error) {
	insert, err := conn.ExecContext(ctx, `INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
		VALUES('knowledge_extraction','knowledge_import_batch',?,'Queued',?,?,?)`, batchID, attempt.ReleaseVersion(), attempt.AgentVersion, now)
	if err != nil {
		return 0, err
	}
	attemptID, err := insert.LastInsertId()
	if err != nil {
		return 0, err
	}
	input := importInput{SchemaKind: importInputKind, AttemptID: attemptID, BatchID: batchID, Generation: 1, SourceMaterialID: materialID, Text: text}
	input.ModelContract.ModelID, input.ModelContract.ContextBudgetTokens, input.ModelContract.MaxOutputTokens = provider.ChatModelID, provider.ContextBudget, provider.MaxOutput
	canonical, err := json.Marshal(input)
	if err != nil {
		return 0, err
	}
	sum := sha256.Sum256(canonical)
	snapshot, err := conn.ExecContext(ctx, `INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at)
		VALUES(?,?,?,?,?)`, attemptID, importInputKind, importRendererVersion, hex.EncodeToString(sum[:]), now)
	if err != nil {
		return 0, err
	}
	snapshotID, err := snapshot.LastInsertId()
	if err != nil {
		return 0, err
	}
	batchDigest := sha256.Sum256([]byte(fmt.Sprintf("knowledge-import-batch:%d", batchID)))
	if _, err = conn.ExecContext(ctx, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,knowledge_import_batch_id)
		VALUES(?,1,'knowledge_import_batch',?,?)`, snapshotID, hex.EncodeToString(batchDigest[:]), batchID); err != nil {
		return 0, err
	}
	materialDigest := sha256.Sum256([]byte(fmt.Sprintf("source-material:%d", materialID)))
	if _, err = conn.ExecContext(ctx, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,source_material_id)
		VALUES(?,2,'source_material',?,?)`, snapshotID, hex.EncodeToString(materialDigest[:]), materialID); err != nil {
		return 0, err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at)
		VALUES(?,'chat_model',?,?,?,?,?)`, attemptID, provider.ConnectionID, provider.RevisionID, provider.CredentialGen, provider.ProbeResultID, now)
	return attemptID, err
}

// RebuildImportInput reconstructs the frozen import bytes using only durable
// source/batch records; dispatch rejects any drift.
func (service *Service) RebuildImportInput(ctx context.Context, attemptID int64) ([]byte, error) {
	var input importInput
	err := service.db.QueryRowContext(ctx, `SELECT a.id,b.id,b.generation,s.id,s.content FROM execution_attempts a
		JOIN knowledge_import_batches b ON b.id=a.scope_id JOIN source_materials s ON s.id=b.source_material_id
		WHERE a.id=? AND a.attempt_type='knowledge_extraction' AND a.scope_type='knowledge_import_batch'`, attemptID).
		Scan(&input.AttemptID, &input.BatchID, &input.Generation, &input.SourceMaterialID, &input.Text)
	if err != nil {
		return nil, err
	}
	modelID, budget, maximum, err := service.attempts.LookupChatContract(ctx, attemptID)
	if err != nil {
		return nil, err
	}
	input.SchemaKind = importInputKind
	input.ModelContract.ModelID, input.ModelContract.ContextBudgetTokens, input.ModelContract.MaxOutputTokens = modelID, budget, maximum
	return json.Marshal(input)
}

// CommitExtraction validates a typed worker proposal then atomically stores the
// immutable suggestions, transitions the batch and seals the attempt.
func (service *Service) CommitExtraction(ctx context.Context, attemptID int64, bootID string, epoch uint64, raw []byte) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var proposal extractionProposal
	if err := decoder.Decode(&proposal); err != nil {
		return fmt.Errorf("%w: not valid closed JSON: %v", ErrInvalidExtraction, err)
	}
	// A typed ResultPayload is exactly one closed JSON object; any trailing
	// byte after the first object — object, bracket or junk — is a protocol
	// violation, never silently ignored input.
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("%w: trailing data after the result object", ErrInvalidExtraction)
	}
	if proposal.SchemaKind != importResultKind || proposal.AttemptID != attemptID || proposal.BatchID < 1 || proposal.ModelCallID < 1 || len(proposal.Items) == 0 {
		return fmt.Errorf("%w: invalid identity envelope", ErrInvalidExtraction)
	}
	for _, item := range proposal.Items {
		if strings.TrimSpace(item.Title) == "" || strings.TrimSpace(item.Body) == "" {
			return fmt.Errorf("%w: empty candidate", ErrInvalidExtraction)
		}
		if len(item.Scope) > 0 {
			if _, err := normalizeScope(item.Scope); err != nil {
				return fmt.Errorf("%w: invalid candidate scope", ErrInvalidExtraction)
			}
		}
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var batchID, materialID, generation int64
	var state, attemptState, callState string
	var leaseUntil sql.NullString
	err = conn.QueryRowContext(ctx, `SELECT a.scope_id,a.state,a.lease_until,b.source_material_id,b.generation,b.state,m.status
		FROM execution_attempts a JOIN knowledge_import_batches b ON b.id=a.scope_id JOIN model_calls m ON m.id=? AND m.attempt_id=a.id
		WHERE a.id=? AND a.attempt_type='knowledge_extraction' AND a.boot_id=? AND a.connection_epoch=?`, proposal.ModelCallID, attemptID, bootID, epoch).
		Scan(&batchID, &attemptState, &leaseUntil, &materialID, &generation, &state, &callState)
	if errors.Is(err, sql.ErrNoRows) {
		return attempt.ErrLateResult
	}
	if err != nil {
		return err
	}
	if batchID != proposal.BatchID || callState != "succeeded" {
		return attempt.ErrLateResult
	}
	// ResultAck delivery is retried on the Runtime control stream. A retry of
	// the attempt's sealed adjudication is accepted by frozen identity alone —
	// never on the batch's later lifecycle or the remaining lease. The sealed
	// digest covers the complete raw proposal (items and the binding model
	// call), so any divergent payload — edited content, item-count change or a
	// re-bind onto another succeeded model call — is a late result
	// (RUNTIME-TASK-008, DATA-ATTEMPT-004).
	if attemptState == "Succeeded" {
		if sealedErr := service.matchesSealedSuggestion(ctx, conn, batchID, generation, proposal.ModelCallID, proposal.Items); sealedErr != nil {
			return sealedErr
		}
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}
		committed = true
		return nil
	}
	if attemptState != "Running" || state != "Processing" {
		return attempt.ErrLateResult
	}
	// RFC3339Nano has variable fractional digits, so TEXT order is not time
	// order: the deadline is parsed and compared as a Go time. An expired
	// lease is a late result even before the sweeper converges the row.
	if !leaseUntil.Valid {
		return attempt.ErrLateResult
	}
	deadline, deadlineErr := time.Parse(time.RFC3339Nano, leaseUntil.String)
	now, nowErr := time.Parse(time.RFC3339Nano, service.nowText())
	if deadlineErr != nil || nowErr != nil || !deadline.After(now) {
		return attempt.ErrLateResult
	}
	for _, item := range proposal.Items {
		suggestion, scope, buildErr := buildSuggestion(materialID, proposal.ModelCallID, item.Title, item.Body, item.Scope)
		if buildErr != nil {
			return buildErr
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO knowledge_candidates(import_batch_id,source_type,source_id,generation,state,original_suggestion_json,draft_title,draft_body,draft_scope_json,draft_revision,created_by,created_at)
			VALUES(?,?,?,?,'AwaitingConfirmation',?,?,?,?,0,?,?)`, batchID, SourceMaterial, materialID, generation, suggestion, item.Title, item.Body, scope, nil, service.nowText()); err != nil {
			return err
		}
	}
	if _, err = conn.ExecContext(ctx, `UPDATE knowledge_import_batches SET state='AwaitingConfirmation',row_version=row_version+1 WHERE id=? AND state='Processing'`, batchID); err != nil {
		return err
	}
	result, updateErr := conn.ExecContext(ctx, `UPDATE execution_attempts SET state='Succeeded',ended_at=?,row_version=row_version+1 WHERE id=? AND state='Running' AND boot_id=? AND connection_epoch=?`, service.nowText(), attemptID, bootID, epoch)
	if updateErr != nil {
		return updateErr
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return attempt.ErrLateResult
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func scopeOrEmpty(scope any) []byte {
	if value, ok := scope.(string); ok {
		return []byte(value)
	}
	return []byte("{}")
}

func replayImport(record authRecord, digest string) (ImportResult, error) {
	if record.RequestDigest != digest || record.ResultObjectType != "knowledge_import_batch" {
		return ImportResult{}, ErrCommandReused
	}
	if record.Outcome != authOutcomeCommitted {
		return ImportResult{}, replayRejection(record.ResultPayload)
	}
	var result ImportResult
	if err := json.Unmarshal([]byte(record.ResultPayload), &result); err != nil {
		return ImportResult{}, err
	}
	return result, nil
}

func scanBatchSummary(scan func(...any) error) (ImportBatchSummary, error) {
	var id, rowVersion, generation int64
	var state, createdAt string
	if err := scan(&id, &state, &rowVersion, &generation, &createdAt); err != nil {
		return ImportBatchSummary{}, err
	}
	return ImportBatchSummary{ID: fmt.Sprintf("%d", id), State: state, RowVersion: rowVersion, Generation: generation, CreatedAt: createdAt}, nil
}

func scanBatchDetailOn(ctx context.Context, conn *sql.Conn, batchID int64) (ImportBatchDetail, error) {
	batch, err := scanBatchSummary(conn.QueryRowContext(ctx, `SELECT id,state,row_version,generation,created_at FROM knowledge_import_batches WHERE id=?`, batchID).Scan)
	if err != nil {
		return ImportBatchDetail{}, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT `+candidateColumns+` FROM knowledge_candidates c WHERE c.import_batch_id=? ORDER BY c.id`, batchID)
	if err != nil {
		return ImportBatchDetail{}, err
	}
	defer rows.Close()
	candidates := make([]CandidateSummary, 0)
	for rows.Next() {
		item, scanErr := readCandidateRow(rows.Scan)
		if scanErr != nil {
			return ImportBatchDetail{}, scanErr
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		return ImportBatchDetail{}, err
	}
	return ImportBatchDetail{ImportBatchSummary: batch, Candidates: candidates}, nil
}

func (service *Service) GetImportBatch(ctx context.Context, batchID int64) (ImportBatchDetail, error) {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return ImportBatchDetail{}, err
	}
	defer conn.Close()
	detail, err := scanBatchDetailOn(ctx, conn, batchID)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportBatchDetail{}, ErrNotFound
	}
	return detail, err
}

// ImportBatchCursor binds import list filters to a created-at keyset edge.
type ImportBatchCursor struct {
	State, CreatedAt string
	ID               int64
}

// ListImportBatches pages batches newest first, binding an optional state
// filter into its cursor so it cannot silently change between requests.
func (service *Service) ListImportBatches(ctx context.Context, state string, after *ImportBatchCursor, limit int) ([]ImportBatchSummary, *ImportBatchCursor, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	if after != nil && after.State != state {
		return nil, nil, errors.New("import batch cursor filter mismatch")
	}
	where := ""
	args := []any{}
	if state != "" {
		where += " AND state=?"
		args = append(args, state)
	}
	if after != nil {
		where += " AND (created_at < ? OR (created_at=? AND id<?))"
		args = append(args, after.CreatedAt, after.CreatedAt, after.ID)
	}
	args = append(args, limit+1)
	rows, err := service.db.QueryContext(ctx, `SELECT id,state,row_version,generation,created_at FROM knowledge_import_batches WHERE 1=1`+where+` ORDER BY created_at DESC,id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items := make([]ImportBatchSummary, 0, limit)
	edges := make([]struct {
		at string
		id int64
	}, 0, limit+1)
	for rows.Next() {
		var item ImportBatchSummary
		var id int64
		if err := rows.Scan(&id, &item.State, &item.RowVersion, &item.Generation, &item.CreatedAt); err != nil {
			return nil, nil, err
		}
		item.ID = fmt.Sprintf("%d", id)
		items = append(items, item)
		edges = append(edges, struct {
			at string
			id int64
		}{item.CreatedAt, id})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *ImportBatchCursor
	if len(items) > limit {
		items = items[:limit]
		edge := edges[len(items)-1]
		next = &ImportBatchCursor{State: state, CreatedAt: edge.at, ID: edge.id}
	}
	return items, next, nil
}

// buildSuggestion renders the immutable original suggestion for one proposal
// item, canonical for the exact input (binding model call, title, body,
// normalized scope).
func buildSuggestion(materialID, modelCallID int64, title, body string, rawScope json.RawMessage) (string, any, error) {
	var scope any
	if len(rawScope) > 0 {
		normalized, err := normalizeScope(rawScope)
		if err != nil {
			return "", nil, err
		}
		scope = scopeValue(normalized)
	}
	suggestion, err := json.Marshal(map[string]any{"v": 1, "modelCallId": fmt.Sprintf("%d", modelCallID), "source": map[string]any{"type": SourceMaterial, "id": fmt.Sprintf("%d", materialID)}, "title": title, "body": body, "scope": json.RawMessage(scopeOrEmpty(scope))})
	if err != nil {
		return "", nil, err
	}
	return string(suggestion), scope, nil
}

// matchesSealedSuggestion proves a redelivered success proposal is the
// attempt's original adjudication: every item must reproduce, in order, the
// immutable suggestion sealed with its candidate. A divergent payload for the
// same attempt identity is a late result, never a re-acknowledged success.
func (service *Service) matchesSealedSuggestion(ctx context.Context, conn *sql.Conn, batchID, generation, modelCallID int64, items []struct {
	Title string          `json:"title"`
	Body  string          `json:"body"`
	Scope json.RawMessage `json:"scope,omitempty"`
}) error {
	rows, err := conn.QueryContext(ctx, `SELECT original_suggestion_json FROM knowledge_candidates WHERE import_batch_id=? AND generation=? ORDER BY id`, batchID, generation)
	if err != nil {
		return err
	}
	defer rows.Close()
	sealed := make([]string, 0, len(items))
	for rows.Next() {
		var suggestion string
		if err := rows.Scan(&suggestion); err != nil {
			return err
		}
		sealed = append(sealed, suggestion)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(sealed) != len(items) {
		return attempt.ErrLateResult
	}
	for index, item := range items {
		// The comparison targets the model-adjudicated content only: the
		// source identity (material id) is server-derived, not model output.
		// Scope canonicalization mirrors the commit path exactly: absent and
		// empty object both collapse to "{}".
		scopeJSON := []byte("{}")
		if len(item.Scope) > 0 {
			normalized, normalizeErr := normalizeScope(item.Scope)
			if normalizeErr != nil {
				return fmt.Errorf("%w: invalid candidate scope", ErrInvalidExtraction)
			}
			if normalized != "{}" {
				scopeJSON = []byte(normalized)
			}
		}
		replayed, replayErr := json.Marshal(map[string]any{"modelCallId": fmt.Sprintf("%d", modelCallID), "title": item.Title, "body": item.Body, "scope": json.RawMessage(scopeJSON)})
		if replayErr != nil {
			return replayErr
		}
		sealedContent, sealedErr := canonicalSuggestionContent(sealed[index])
		if sealedErr != nil {
			return sealedErr
		}
		if string(replayed) != sealedContent {
			return attempt.ErrLateResult
		}
	}
	return nil
}

// canonicalSuggestionContent re-marshals one suggestion's adjudicated content
// (title, body, scope) into a canonical string for replay comparison.
func canonicalSuggestionContent(suggestion string) (string, error) {
	var decoded struct {
		ModelCallID string          `json:"modelCallId"`
		Title       string          `json:"title"`
		Body        string          `json:"body"`
		Scope       json.RawMessage `json:"scope"`
	}
	if err := json.Unmarshal([]byte(suggestion), &decoded); err != nil {
		return "", fmt.Errorf("%w: sealed suggestion unreadable: %v", ErrInvalidExtraction, err)
	}
	canonical, err := json.Marshal(map[string]any{"modelCallId": decoded.ModelCallID, "title": decoded.Title, "body": decoded.Body, "scope": decoded.Scope})
	if err != nil {
		return "", err
	}
	return string(canonical), nil
}

// FailExtraction atomically terminalizes the worker attempt and its visible
// batch. A failed extraction can never leave a Processing batch without an
// owner to make progress. Replay of the already-committed failure is
// idempotent by frozen attempt identity and termination reason; an expired
// lease is a late result even before the sweeper converges the row
// (RUNTIME-TASK-008).
func (service *Service) FailExtraction(ctx context.Context, attemptID int64, bootID string, epoch uint64, termination string) error {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var batchID int64
	var attemptState string
	var priorTermination sql.NullString
	var leaseUntil sql.NullString
	if err = conn.QueryRowContext(ctx, `SELECT scope_id,state,termination_reason,lease_until FROM execution_attempts WHERE id=? AND attempt_type='knowledge_extraction' AND boot_id=? AND connection_epoch=?`, attemptID, bootID, epoch).
		Scan(&batchID, &attemptState, &priorTermination, &leaseUntil); errors.Is(err, sql.ErrNoRows) {
		return attempt.ErrLateResult
	} else if err != nil {
		return err
	}
	if attemptState == "Failed" {
		// A redelivered failure proposal replays the original adjudication.
		if !priorTermination.Valid || priorTermination.String != termination {
			return attempt.ErrLateResult
		}
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}
		committed = true
		return nil
	}
	if attemptState != "Running" {
		return attempt.ErrLateResult
	}
	if !leaseUntil.Valid {
		return attempt.ErrLateResult
	}
	deadline, deadlineErr := time.Parse(time.RFC3339Nano, leaseUntil.String)
	now, nowErr := time.Parse(time.RFC3339Nano, service.nowText())
	if deadlineErr != nil || nowErr != nil || !deadline.After(now) {
		return attempt.ErrLateResult
	}
	if err = service.Attempts().CommitResultOn(ctx, conn, attemptID, bootID, epoch, false, termination); err != nil {
		return err
	}
	result, err := conn.ExecContext(ctx, `UPDATE knowledge_import_batches SET state='Failed',row_version=row_version+1 WHERE id=? AND state='Processing'`, batchID)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return attempt.ErrLateResult
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	committed = err == nil
	return err
}

// RejectExtraction atomically closes a dispatched import after a terminal
// rejection. No-capacity deliberately remains Assigned for replay; the caller
// provides the closed protocol termination reason.
func (service *Service) RejectExtraction(ctx context.Context, attemptID int64, bootID string, epoch uint64, reason string) error {
	if reason == "no_capacity" {
		return nil
	}
	if reason != "provider_unavailable" && reason != "worker_protocol_error" {
		reason = "provider_unavailable"
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var batchID int64
	if err = conn.QueryRowContext(ctx, `SELECT scope_id FROM execution_attempts WHERE id=? AND attempt_type='knowledge_extraction' AND state='Assigned' AND boot_id=? AND connection_epoch=?`, attemptID, bootID, epoch).Scan(&batchID); errors.Is(err, sql.ErrNoRows) {
		return attempt.ErrLateResult
	} else if err != nil {
		return err
	}
	if result, updateErr := conn.ExecContext(ctx, `UPDATE execution_attempts SET state='Failed',ended_at=?,termination_reason=?,row_version=row_version+1 WHERE id=? AND state='Assigned' AND boot_id=? AND connection_epoch=?`, service.nowText(), reason, attemptID, bootID, epoch); updateErr != nil {
		return updateErr
	} else if affected, _ := result.RowsAffected(); affected != 1 {
		return attempt.ErrLateResult
	}
	if result, updateErr := conn.ExecContext(ctx, `UPDATE knowledge_import_batches SET state='Failed',row_version=row_version+1 WHERE id=? AND state='Processing'`, batchID); updateErr != nil {
		return updateErr
	} else if affected, _ := result.RowsAffected(); affected != 1 {
		return attempt.ErrLateResult
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// InterruptExtraction closes the user-visible batch in the same SQLite
// transaction as lease/restart loss convergence, preventing an orphaned
// Processing batch after its sole extraction Attempt has stopped.
func (service *Service) InterruptExtraction(ctx context.Context, attemptID int64, reason string) error {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var batchID int64
	if err = conn.QueryRowContext(ctx, `SELECT scope_id FROM execution_attempts WHERE id=? AND attempt_type='knowledge_extraction'`, attemptID).Scan(&batchID); errors.Is(err, sql.ErrNoRows) {
		return attempt.ErrLateResult
	} else if err != nil {
		return err
	}
	final, err := service.Attempts().InterruptOn(ctx, conn, attemptID, reason)
	if err != nil {
		return err
	}
	if final == "Interrupted" {
		if result, updateErr := conn.ExecContext(ctx, `UPDATE knowledge_import_batches SET state='Failed',row_version=row_version+1 WHERE id=? AND state='Processing'`, batchID); updateErr != nil {
			return updateErr
		} else if affected, _ := result.RowsAffected(); affected != 1 {
			return attempt.ErrLateResult
		}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}
