package knowledge

// Import batches are the only path by which pasted source material reaches an
// agent. The model proposes drafts; it never inserts reusable knowledge. A
// human confirmation remains the publication boundary.
//
// StartImport 通过共享执行器 Run 执行：会话复核、幂等重放、业务修改、台账与
// 审计统一提交；抽取 Attempt 经 attempt.CreateOn 创建，在同事务持久化用户
// 操作关联（ADR-0006）。抽取结果应用（CommitExtraction 等）是后台 Attempt
// 生命周期：执行器 Execute 以系统主体运行，关联从持久化 Attempt 恢复
// （attempt.LoadCorrelation），无关联的历史 Attempt 显式建立独立任务窗口，
// 绝不静默伪装用户身份。

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
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

const (
	SourceMaterial         = "source_material"
	SourceKnowledgeVersion = "knowledge_version"
	importInputKind        = "knowledge_extraction_v1"
	importRendererVersion  = "knowledge-extraction-v1"
	importResultKind       = "knowledge_extraction_result_v1"
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
	// ToolCatalog is the attempt's frozen model tool catalog (ADR-0004);
	// the snapshot digest covers it via this embedding.
	ToolCatalog *attempt.FrozenCatalog `json:"toolCatalog,omitempty"`
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
	empty := strings.TrimSpace(text) == ""
	digest := commandDigest(opImport, map[string]any{"text": text})
	outcome, err := execution.Run(ctx, service.runner, service.startImport, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: commandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (ImportResult, execution.Change, error) {
		if empty {
			return ImportResult{}, execution.Changed, rejectionOf(0, ErrEmptyImport)
		}
		provider, err := selectImportProvider(ctx, tx)
		if err != nil {
			return ImportResult{}, execution.Changed, err
		}
		now := service.nowText()
		materialDigest := sha256.Sum256([]byte(text))
		material, err := tx.ExecContext(ctx, `INSERT INTO source_materials(kind,digest,size_bytes,content,created_by,created_at)
			VALUES('knowledge_import',?,?,?,?,?)`, hex.EncodeToString(materialDigest[:]), len([]byte(text)), text, principalID, now)
		if err != nil {
			return ImportResult{}, execution.Changed, err
		}
		materialID, err := material.LastInsertId()
		if err != nil {
			return ImportResult{}, execution.Changed, err
		}
		batch, err := tx.ExecContext(ctx, `INSERT INTO knowledge_import_batches(source_material_id,state,created_by,created_at)
			VALUES(?, 'Processing', ?, ?)`, materialID, principalID, now)
		if err != nil {
			return ImportResult{}, execution.Changed, err
		}
		batchID, err := batch.LastInsertId()
		if err != nil {
			return ImportResult{}, execution.Changed, err
		}
		// attempt.CreateOn persists the caller's operation correlation onto the
		// new attempt in this same transaction (ADR-0006); the user's import
		// stays the correlation authority for its extraction lifecycle.
		attemptID, err := service.insertImportAttempt(ctx, tx, batchID, materialID, text, provider, now)
		if err != nil {
			return ImportResult{}, execution.Changed, err
		}
		detail, err := scanBatchDetailOn(ctx, tx, batchID)
		if err != nil {
			return ImportResult{}, execution.Changed, err
		}
		return ImportResult{Batch: detail, AttemptID: attemptID}, execution.Changed, nil
	}, func(result ImportResult) int64 {
		if result.Batch.ID == "" {
			return 0
		}
		return parseCandidateLocator(result.Batch.ID)
	})
	if err != nil {
		return ImportResult{}, service.translateCommandError(ctx, err)
	}
	return outcome.Result, nil
}

func selectImportProvider(ctx context.Context, q audit.Reader) (importProvider, error) {
	var selected importProvider
	var qualificationVersion, connectionVersion int64
	var outcome string
	err := q.QueryRowContext(ctx, `SELECT c.id,c.current_revision_id,c.current_credential_generation_id,q.probe_result_id,q.enabled_row_version,c.row_version,p.outcome
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
	err = q.QueryRowContext(ctx, `SELECT chat_model_id,context_budget_tokens,max_output_tokens,native_tool_calling_supported
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

func (service *Service) insertImportAttempt(ctx context.Context, w execution.Executor, batchID, materialID int64, text string, provider importProvider, now string) (int64, error) {
	// knowledge 抽取固定在原共享执行身份上（attempt.KnowledgeAgentVersion）：
	// 其 prompt 从未随 analysis prompt 代际演进，身份与输出契约不随
	// initial-analysis 升级漂移。
	// attempt.CreateOn centrally persists the caller's correlation metadata
	// onto the new attempt in this same transaction (ADR-0006).
	attemptID, err := attempt.CreateOn(ctx, w, `
		INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
		VALUES('knowledge_extraction','knowledge_import_batch',?,'Queued',?,?,?)`,
		batchID, attempt.ReleaseVersion(), attempt.KnowledgeAgentVersion, now)
	if err != nil {
		return 0, err
	}
	input := importInput{SchemaKind: importInputKind, AttemptID: attemptID, BatchID: batchID, Generation: 1, SourceMaterialID: materialID, Text: text}
	input.ModelContract.ModelID, input.ModelContract.ContextBudgetTokens, input.ModelContract.MaxOutputTokens = provider.ChatModelID, provider.ContextBudget, provider.MaxOutput
	// Freeze THIS attempt's tool catalog at creation (ADR-0004).
	catalogDocument, catalog, err := attempt.FrozenCatalogJSONForCreation(service.attempts.Catalogs, attempt.KnowledgeAgentVersion)
	if err != nil {
		return 0, err
	}
	input.ToolCatalog = catalog
	canonical, err := json.Marshal(input)
	if err != nil {
		return 0, err
	}
	sum := sha256.Sum256(canonical)
	snapshot, err := w.ExecContext(ctx, `INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,tool_catalog_json,created_at)
		VALUES(?,?,?,?,?,?)`, attemptID, importInputKind, importRendererVersion, hex.EncodeToString(sum[:]), string(catalogDocument), now)
	if err != nil {
		return 0, err
	}
	snapshotID, err := snapshot.LastInsertId()
	if err != nil {
		return 0, err
	}
	batchDigest := sha256.Sum256([]byte(fmt.Sprintf("knowledge-import-batch:%d", batchID)))
	if _, err = w.ExecContext(ctx, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,knowledge_import_batch_id)
		VALUES(?,1,'knowledge_import_batch',?,?)`, snapshotID, hex.EncodeToString(batchDigest[:]), batchID); err != nil {
		return 0, err
	}
	materialDigest := sha256.Sum256([]byte(fmt.Sprintf("source-material:%d", materialID)))
	if _, err = w.ExecContext(ctx, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,source_material_id)
		VALUES(?,2,'source_material',?,?)`, snapshotID, hex.EncodeToString(materialDigest[:]), materialID); err != nil {
		return 0, err
	}
	_, err = w.ExecContext(ctx, `INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at)
		VALUES(?,'chat_model',?,?,?,?,?)`, attemptID, provider.ConnectionID, provider.RevisionID, provider.CredentialGen, provider.ProbeResultID, now)
	return attemptID, err
}

// RebuildImportInput reconstructs the frozen import bytes using only durable
// source/batch records; dispatch rejects any drift.
func (service *Service) RebuildImportInput(ctx context.Context, attemptID int64) ([]byte, error) {
	var input importInput
	err := service.reader.QueryRowContext(ctx, `SELECT a.id,b.id,b.generation,s.id,s.content FROM execution_attempts a
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
	// The frozen catalog travels with the attempt: stored document only,
	// never re-derived from current enablement.
	toolCatalog, err := attempt.FrozenToolCatalogDoc(ctx, service.writer, attemptID)
	if err != nil {
		return nil, err
	}
	input.ToolCatalog = toolCatalog
	return json.Marshal(input)
}

// taskContext restores the persisted operation correlation of an extraction
// attempt so the background result application stays linked to the user
// operation that created it (ADR-0006 lifecycle correlation). A wired caller
// context passes through untouched; a legacy attempt without a persisted
// correlation gets an explicit fresh task window (never a fabricated user).
func (service *Service) taskContext(ctx context.Context, attemptID int64) (context.Context, error) {
	if _, ok := execution.FromContext(ctx); ok {
		return ctx, nil
	}
	correlation, found, err := attempt.LoadCorrelation(ctx, service.writer, attemptID)
	if err != nil {
		return nil, err
	}
	requestID := fmt.Sprintf("extraction-%d", attemptID)
	actor := execution.Principal{Kind: execution.PrincipalSystem}
	initiator := actor
	source := execution.Source{Kind: execution.SourceTask, RequestID: requestID}
	if found && correlation.OperationCorrelationID != "" {
		source.RequestID = fmt.Sprintf("task-%s", correlation.OperationCorrelationID)
		switch correlation.InitiatorType {
		case string(execution.PrincipalUser), string(execution.PrincipalService):
			initiator = execution.Principal{Kind: execution.PrincipalKind(correlation.InitiatorType), ID: correlation.InitiatorID}
		}
		return execution.WithMetadata(ctx, execution.Metadata{
			CorrelationID: correlation.OperationCorrelationID,
			Actor:         actor,
			Initiator:     initiator,
			Source:        source,
		})
	}
	correlationID, err := execution.NewCorrelationID()
	if err != nil {
		return nil, err
	}
	return execution.WithMetadata(ctx, execution.Metadata{
		CorrelationID: correlationID,
		Actor:         actor,
		Initiator:     initiator,
		Source:        source,
	})
}

// CommitExtraction validates a typed worker proposal then atomically stores the
// immutable suggestions, transitions the batch and seals the attempt. The
// application runs through the shared runner as an audited system operation
// whose correlation is restored from the persisted attempt.
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
	ctx, err := service.taskContext(ctx, attemptID)
	if err != nil {
		return err
	}
	_, err = execution.Execute(ctx, service.runner, service.extractionDone, func(tx *execution.Tx) (int64, error) {
		var batchID, materialID, generation int64
		var state, attemptState, callState string
		var leaseUntil sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT a.scope_id,a.state,a.lease_until,b.source_material_id,b.generation,b.state,m.status
			FROM execution_attempts a JOIN knowledge_import_batches b ON b.id=a.scope_id JOIN model_calls m ON m.id=? AND m.attempt_id=a.id
			WHERE a.id=? AND a.attempt_type='knowledge_extraction' AND a.boot_id=? AND a.connection_epoch=?`, proposal.ModelCallID, attemptID, bootID, epoch).
			Scan(&batchID, &attemptState, &leaseUntil, &materialID, &generation, &state, &callState)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, attempt.ErrLateResult
		}
		if err != nil {
			return 0, err
		}
		if batchID != proposal.BatchID || callState != "succeeded" {
			return 0, attempt.ErrLateResult
		}
		// ResultAck delivery is retried on the Runtime control stream. A retry of
		// the attempt's sealed adjudication is accepted by frozen identity alone —
		// never on the batch's later lifecycle or the remaining lease. The sealed
		// digest covers the complete raw proposal (items and the binding model
		// call), so any divergent payload — edited content, item-count change or a
		// re-bind onto another succeeded model call — is a late result
		// (RUNTIME-TASK-008, DATA-ATTEMPT-004).
		if attemptState == "Succeeded" {
			if sealedErr := service.matchesSealedSuggestion(ctx, tx, batchID, generation, proposal.ModelCallID, proposal.Items); sealedErr != nil {
				return 0, sealedErr
			}
			return batchID, nil
		}
		if attemptState != "Running" || state != "Processing" {
			return 0, attempt.ErrLateResult
		}
		// RFC3339Nano has variable fractional digits, so TEXT order is not time
		// order: the deadline is parsed and compared as a Go time. An expired
		// lease is a late result even before the sweeper converges the row.
		if !leaseUntil.Valid {
			return 0, attempt.ErrLateResult
		}
		deadline, deadlineErr := time.Parse(time.RFC3339Nano, leaseUntil.String)
		now, nowErr := time.Parse(time.RFC3339Nano, service.nowText())
		if deadlineErr != nil || nowErr != nil || !deadline.After(now) {
			return 0, attempt.ErrLateResult
		}
		for _, item := range proposal.Items {
			suggestion, scope, buildErr := buildSuggestion(materialID, proposal.ModelCallID, item.Title, item.Body, item.Scope)
			if buildErr != nil {
				return 0, buildErr
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO knowledge_candidates(import_batch_id,source_type,source_id,generation,state,original_suggestion_json,draft_title,draft_body,draft_scope_json,draft_revision,created_by,created_at)
				VALUES(?,?,?,?,'AwaitingConfirmation',?,?,?,?,0,?,?)`, batchID, SourceMaterial, materialID, generation, suggestion, item.Title, item.Body, scope, nil, service.nowText()); err != nil {
				return 0, err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE knowledge_import_batches SET state='AwaitingConfirmation',row_version=row_version+1 WHERE id=? AND state='Processing'`, batchID); err != nil {
			return 0, err
		}
		result, updateErr := tx.ExecContext(ctx, `UPDATE execution_attempts SET state='Succeeded',ended_at=?,row_version=row_version+1 WHERE id=? AND state='Running' AND boot_id=? AND connection_epoch=?`, service.nowText(), attemptID, bootID, epoch)
		if updateErr != nil {
			return 0, updateErr
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return 0, attempt.ErrLateResult
		}
		return batchID, nil
	}, func(batchID int64) int64 { return batchID })
	return err
}

func scopeOrEmpty(scope any) []byte {
	if value, ok := scope.(string); ok {
		return []byte(value)
	}
	return []byte("{}")
}

func scanBatchSummary(scan func(...any) error) (ImportBatchSummary, error) {
	var id, rowVersion, generation int64
	var state, createdAt string
	if err := scan(&id, &state, &rowVersion, &generation, &createdAt); err != nil {
		return ImportBatchSummary{}, err
	}
	return ImportBatchSummary{ID: fmt.Sprintf("%d", id), State: state, RowVersion: rowVersion, Generation: generation, CreatedAt: createdAt}, nil
}

func scanBatchDetailOn(ctx context.Context, q audit.Reader, batchID int64) (ImportBatchDetail, error) {
	batch, err := scanBatchSummary(q.QueryRowContext(ctx, `SELECT id,state,row_version,generation,created_at FROM knowledge_import_batches WHERE id=?`, batchID).Scan)
	if err != nil {
		return ImportBatchDetail{}, err
	}
	rows, err := q.QueryContext(ctx, `SELECT `+candidateColumns+` FROM knowledge_candidates c`+candidateSourceJoin+` WHERE c.import_batch_id=? ORDER BY c.id`, batchID)
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
	detail, err := scanBatchDetailOn(ctx, service.reader, batchID)
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
	rows, err := service.reader.QueryContext(ctx, `SELECT id,state,row_version,generation,created_at FROM knowledge_import_batches WHERE 1=1`+where+` ORDER BY created_at DESC,id DESC LIMIT ?`, args...)
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
func (service *Service) matchesSealedSuggestion(ctx context.Context, q audit.Reader, batchID, generation, modelCallID int64, items []struct {
	Title string          `json:"title"`
	Body  string          `json:"body"`
	Scope json.RawMessage `json:"scope,omitempty"`
},
) error {
	rows, err := q.QueryContext(ctx, `SELECT original_suggestion_json FROM knowledge_candidates WHERE import_batch_id=? AND generation=? ORDER BY id`, batchID, generation)
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
// (RUNTIME-TASK-008). The audited system step restores the attempt's
// persisted correlation.
func (service *Service) FailExtraction(ctx context.Context, attemptID int64, bootID string, epoch uint64, termination string) error {
	ctx, err := service.taskContext(ctx, attemptID)
	if err != nil {
		return err
	}
	_, execErr := execution.Execute(ctx, service.runner, service.extractionFail, func(tx *execution.Tx) (int64, error) {
		var batchID int64
		var attemptState string
		var priorTermination sql.NullString
		var leaseUntil sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT scope_id,state,termination_reason,lease_until FROM execution_attempts WHERE id=? AND attempt_type='knowledge_extraction' AND boot_id=? AND connection_epoch=?`, attemptID, bootID, epoch).
			Scan(&batchID, &attemptState, &priorTermination, &leaseUntil); errors.Is(err, sql.ErrNoRows) {
			return 0, attempt.ErrLateResult
		} else if err != nil {
			return 0, err
		}
		if attemptState == "Failed" {
			// A redelivered failure proposal replays the original adjudication.
			if !priorTermination.Valid || priorTermination.String != termination {
				return 0, attempt.ErrLateResult
			}
			return batchID, nil
		}
		if attemptState != "Running" {
			return 0, attempt.ErrLateResult
		}
		if !leaseUntil.Valid {
			return 0, attempt.ErrLateResult
		}
		deadline, deadlineErr := time.Parse(time.RFC3339Nano, leaseUntil.String)
		now, nowErr := time.Parse(time.RFC3339Nano, service.nowText())
		if deadlineErr != nil || nowErr != nil || !deadline.After(now) {
			return 0, attempt.ErrLateResult
		}
		if err := service.attempts.CommitResultOn(ctx, tx, attemptID, bootID, epoch, false, termination); err != nil {
			return 0, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE knowledge_import_batches SET state='Failed',row_version=row_version+1 WHERE id=? AND state='Processing'`, batchID)
		if err != nil {
			return 0, err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return 0, attempt.ErrLateResult
		}
		return batchID, nil
	}, func(batchID int64) int64 { return batchID })
	return execErr
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
	ctx, err := service.taskContext(ctx, attemptID)
	if err != nil {
		return err
	}
	_, execErr := execution.Execute(ctx, service.runner, service.extractionDrop, func(tx *execution.Tx) (int64, error) {
		var batchID int64
		if err := tx.QueryRowContext(ctx, `SELECT scope_id FROM execution_attempts WHERE id=? AND attempt_type='knowledge_extraction' AND state='Assigned' AND boot_id=? AND connection_epoch=?`, attemptID, bootID, epoch).Scan(&batchID); errors.Is(err, sql.ErrNoRows) {
			return 0, attempt.ErrLateResult
		} else if err != nil {
			return 0, err
		}
		if result, updateErr := tx.ExecContext(ctx, `UPDATE execution_attempts SET state='Failed',ended_at=?,termination_reason=?,row_version=row_version+1 WHERE id=? AND state='Assigned' AND boot_id=? AND connection_epoch=?`, service.nowText(), reason, attemptID, bootID, epoch); updateErr != nil {
			return 0, updateErr
		} else if affected, _ := result.RowsAffected(); affected != 1 {
			return 0, attempt.ErrLateResult
		}
		if result, updateErr := tx.ExecContext(ctx, `UPDATE knowledge_import_batches SET state='Failed',row_version=row_version+1 WHERE id=? AND state='Processing'`, batchID); updateErr != nil {
			return 0, updateErr
		} else if affected, _ := result.RowsAffected(); affected != 1 {
			return 0, attempt.ErrLateResult
		}
		return batchID, nil
	}, func(batchID int64) int64 { return batchID })
	return execErr
}

// InterruptExtraction closes the user-visible batch in the same SQLite
// transaction as lease/restart loss convergence, preventing an orphaned
// Processing batch after its sole extraction Attempt has stopped.
func (service *Service) InterruptExtraction(ctx context.Context, attemptID int64, reason string) error {
	ctx, err := service.taskContext(ctx, attemptID)
	if err != nil {
		return err
	}
	_, execErr := execution.Execute(ctx, service.runner, service.extractionBreak, func(tx *execution.Tx) (int64, error) {
		var batchID int64
		if err := tx.QueryRowContext(ctx, `SELECT scope_id FROM execution_attempts WHERE id=? AND attempt_type='knowledge_extraction'`, attemptID).Scan(&batchID); errors.Is(err, sql.ErrNoRows) {
			return 0, attempt.ErrLateResult
		} else if err != nil {
			return 0, err
		}
		final, err := service.attempts.InterruptOn(ctx, tx, attemptID, reason)
		if err != nil {
			return 0, err
		}
		if final == "Interrupted" {
			if result, updateErr := tx.ExecContext(ctx, `UPDATE knowledge_import_batches SET state='Failed',row_version=row_version+1 WHERE id=? AND state='Processing'`, batchID); updateErr != nil {
				return 0, updateErr
			} else if affected, _ := result.RowsAffected(); affected != 1 {
				return 0, attempt.ErrLateResult
			}
		}
		return batchID, nil
	}, func(batchID int64) int64 { return batchID })
	return execErr
}
