// Package embedding owns the derived semantic index projection (T29): the
// embedding_generations lifecycle and the embeddings rows keyed by
// (knowledge_version_id, embedding_generation_id). The projection is
// rebuildable and never a second authority for knowledge eligibility
// (DATA-SCOPE-003): eligibility itself stays current ∧ 未停用 ∧ 来源有效 ∧
// 未 exit, expressed by knowledge_search_docs. Provider calls happen only
// through persisted `embedding` Execution Attempts dispatched to the Plinth
// supervisor (RUNTIME-AGENT-010: no worker, no ReAct loop).
package embedding

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// 执行器操作（审计 action）：扫掠与查询创建是独立的后台/用户操作；结果
// 应用与密封是运行时结果路径的系统操作（Attempt 冻结身份在业务阶段裁决）。
const (
	opSweep       = "embedding.generation.sweep"
	opResultTake  = "embedding.result.take"
	opResultSeal  = "embedding.result.seal"
	opQueryCreate = "embedding.query.create"
)

// queryer/executor 是本包的查询与事务面：执行器事务、只读读源和普通连接
// 都满足，使既有扫描实现无需复刻。
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type executor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// writer is the sealed mutation-helper surface (execution.Executor).
type writer = execution.Executor

// Input schema kinds are the frozen versioned wire shapes of the embedding
// attempt input/result (trg_attempt_input_snapshot_closure maps attempt type
// `embedding` to embedding_v1).
const (
	InputSchemaKind  = "embedding_v1"
	RendererVersion  = "embedding-input-v1"
	ResultSchemaKind = "embedding_generation_result_v1"
	QuerySchemaKind  = "embedding_query_result_v1"
)

// ErrNoProvider reports that no enabled model provider connection currently
// qualifies for embeddings; the semantic channel stays honestly empty.
var ErrNoProvider = errors.New("no qualified embedding model provider")

// ErrBusy reports that the generation scope already holds an active
// embedding attempt; the caller skips the semantic channel this round.
var ErrBusy = errors.New("embedding generation already has an active attempt")

// Service owns the embedding projection lifecycle. 写路径（扫掠、查询创建、
// 结果应用与密封）全部经共享执行器 Execute 以自动审计运行：扫掠是显式的
// 系统/调度根操作（无关联上下文时建立自己的操作窗口，绝不伪装用户）；结果
// 应用恢复持久化 Attempt 的关联；查询创建属于发起搜索的用户会话。读路径走
// 窄化的 audit.Reader（多读一致性经只读快照）。服务不持有原始数据库句柄。
type Service struct {
	reader audit.Reader
	writer *sql.DB
	runner *execution.Runner
	now    func() time.Time

	sweep       *execution.Operation
	resultTake  *execution.Operation
	resultSeal  *execution.Operation
	queryCreate *execution.Operation

	// dispatcher delivers one Queued embedding attempt to the live Plinth
	// stream; nil in unit contexts (attempts stay Queued until a sweep
	// dispatches them).
	dispatcher func(ctx context.Context, attemptID int64) error

	mu sync.Mutex
	// waiters delivers one query-attempt outcome to its live HTTP request.
	waiters map[int64]chan queryOutcome
	// queryTexts holds the request-scoped query text of live query attempts
	// (the query is not durable domain state).
	queryTexts map[int64]string
}

// NewService builds the embedding projection service. 组合层迁移前的兼容
// 构造：reader 与执行器共用同一数据库句柄。
func NewService(db *sql.DB) *Service {
	service, err := NewServiceWithReader(db, db, execution.NewRunner(db, execution.NewRegistry(), nil))
	if err != nil {
		panic("embedding: compose default service: " + err.Error())
	}
	return service
}

// NewServiceWithReader 装配模块：reader 是读路径查询面，writer 是子聚合构
// 造所需的写权威组合依赖（只流入构造，不再经访问器外泄），runner 是组合层
// 共享的执行器（操作注册进其注册表，同名重复注册即失败）。
func NewServiceWithReader(reader audit.Reader, writer *sql.DB, runner *execution.Runner) (*Service, error) {
	if reader == nil {
		return nil, errors.New("embedding: read capability is required")
	}
	if writer == nil {
		return nil, errors.New("embedding: composition writer is required")
	}
	if runner == nil {
		return nil, errors.New("embedding: command runner is required")
	}
	register := func(name, objectType string, authorize func(context.Context, *execution.Tx) error) *execution.Operation {
		op, err := runner.Register(execution.Operation{Name: name, Class: execution.ClassWrite, ObjectType: objectType, Authorize: authorize})
		if err != nil {
			panic("embedding: register " + name + ": " + err.Error())
		}
		return op
	}
	system := func(ctx context.Context, _ *execution.Tx) error { return authorizeSystem(ctx) }
	user := func(ctx context.Context, tx *execution.Tx) error { return auth.VerifyExecutionSession(ctx, tx, "") }
	both := func(ctx context.Context, tx *execution.Tx) error {
		meta, err := execution.Require(ctx)
		if err != nil {
			return err
		}
		if meta.Actor.Kind == execution.PrincipalSystem {
			return nil
		}
		return auth.VerifyExecutionSession(ctx, tx, "")
	}
	service := &Service{reader: reader, writer: writer, runner: runner, now: time.Now, waiters: map[int64]chan queryOutcome{}, queryTexts: map[int64]string{}}
	service.sweep = register(opSweep, "embedding_generation", both)
	service.resultTake = register(opResultTake, "embedding_generation", system)
	service.resultSeal = register(opResultSeal, "embedding_generation", system)
	service.queryCreate = register(opQueryCreate, "embedding_generation", user)
	return service, nil
}

// authorizeSystem 复核结果应用步骤的系统主体身份；权威裁决是业务阶段内的
// attempt 冻结身份（attempt id、boot、epoch）。
func authorizeSystem(ctx context.Context) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalSystem {
		return fmt.Errorf("embedding: actor kind %q may not apply embedding results", meta.Actor.Kind)
	}
	return nil
}

// snapshot 在支持事务的只读读源上提供一致性快照读（deferred 只读事务）；
// 仅支持逐查询的读面直接执行，不伪造事务语义。
func (service *Service) snapshot(ctx context.Context, fn func(q audit.Reader) error) error {
	type beginTx interface {
		BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
	}
	if db, ok := service.reader.(beginTx); ok {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	return fn(service.reader)
}

// SetDispatcher wires the runtime dispatch hook (called with the attempt id
// right after a query attempt is committed Queued).
func (service *Service) SetDispatcher(dispatch func(ctx context.Context, attemptID int64) error) {
	service.dispatcher = dispatch
}

func (service *Service) nowText() string {
	return service.now().UTC().Format(time.RFC3339Nano)
}

// provider is the current embedding-qualified model provider projection.
type provider struct {
	ConnectionID    int64
	RevisionID      int64
	CredentialGen   int64
	ProbeResultID   int64
	EmbeddingModel  string
	VectorDim       int64
	ChatModelID     string
	ContextBudget   int64
	MaxOutputTokens int64
}

// identity binds one generation to the frozen provider configuration:
// the embedding model id of the enabled connection revision.
func (selected provider) identity() (string, string) {
	return selected.EmbeddingModel, fmt.Sprintf("conn/%d/rev/%d", selected.ConnectionID, selected.RevisionID)
}

// selectProvider resolves the enabled, currently-qualified model provider
// with a frozen embedding capability (DATA-CONN-008: the embedding grant
// closes over the explicitly selected qualified probe result).
func (service *Service) selectProvider(ctx context.Context, q queryer) (provider, bool, error) {
	var selected provider
	var qualificationVersion, connectionVersion int64
	var outcome string
	var embeddingModel sql.NullString
	var embeddingSupported bool
	var embeddingDim sql.NullInt64
	err := q.QueryRowContext(ctx, `
		SELECT c.id, c.current_revision_id, c.current_credential_generation_id, q.probe_result_id,
		       q.enabled_row_version, c.row_version, p.outcome,
		       e.chat_model_id, e.context_budget_tokens, e.max_output_tokens,
		       e.embedding_model_id, e.embedding_supported, e.embedding_vector_dim
		FROM connections c
		JOIN connection_enable_qualifications q ON q.connection_id=c.id
		JOIN connection_probe_results p ON p.id=q.probe_result_id
		JOIN model_provider_connection_probe_results e ON e.probe_result_id=p.id
		WHERE c.type='model_provider' AND c.enabled=1 AND c.revalidation_required=0
		ORDER BY q.id DESC LIMIT 1`).
		Scan(&selected.ConnectionID, &selected.RevisionID, &selected.CredentialGen, &selected.ProbeResultID,
			&qualificationVersion, &connectionVersion, &outcome,
			&selected.ChatModelID, &selected.ContextBudget, &selected.MaxOutputTokens,
			&embeddingModel, &embeddingSupported, &embeddingDim)
	if errors.Is(err, sql.ErrNoRows) {
		return selected, false, nil
	}
	if err != nil {
		return selected, false, err
	}
	if qualificationVersion != connectionVersion || outcome != "passed" || !embeddingSupported || !embeddingModel.Valid || !embeddingDim.Valid || embeddingDim.Int64 < 1 {
		return selected, false, nil
	}
	selected.EmbeddingModel = embeddingModel.String
	selected.VectorDim = embeddingDim.Int64
	return selected, true, nil
}

// Sweep reconciles the whole projection in one serialized transaction:
// drift detection, pending-row enqueue, attempt creation and generation
// finalization. It is idempotent and safe to run from any trigger
// (plinth attach, lease tick, search).
func (service *Service) Sweep(ctx context.Context) error {
	// The sweep is a trusted background entry point: each serialized window
	// is its own business operation and carries a fresh correlation, so the
	// centralized attempt creation can persist it (ADR-0006). A context that
	// already carries metadata (wired callers, search-driven sweeps) is
	// passed through untouched — the audit records the honest actor.
	if _, ok := execution.FromContext(ctx); !ok {
		correlationID, corrErr := execution.NewCorrelationID()
		if corrErr != nil {
			return corrErr
		}
		enriched, metaErr := execution.WithMetadata(ctx, execution.Metadata{
			CorrelationID: correlationID,
			Actor:         execution.Principal{Kind: execution.PrincipalSystem},
			Source:        execution.Source{Kind: execution.SourceScheduler},
		})
		if metaErr != nil {
			return metaErr
		}
		ctx = enriched
	}
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind == execution.PrincipalSystem {
		var needed bool
		if err := service.snapshot(ctx, func(reader audit.Reader) error {
			var err error
			needed, err = service.sweepNeeded(ctx, reader)
			return err
		}); err != nil {
			return err
		}
		if !needed {
			return nil
		}
	}
	_, err = execution.Execute(ctx, service.runner, service.sweep, func(tx *execution.Tx) (int64, error) {
		if meta.Actor.Kind == execution.PrincipalSystem {
			needed, err := service.sweepNeeded(ctx, tx)
			if err != nil {
				return 0, err
			}
			if !needed {
				return 0, execution.ErrNoTransition
			}
		}
		selected, ok, err := service.selectProvider(ctx, tx)
		if err != nil {
			return 0, err
		}
		if !ok {
			// Without a qualified provider there is nothing to reconcile.
			return 0, nil
		}
		if err := service.sweepOn(ctx, tx, selected); err != nil {
			return 0, err
		}
		return 0, nil
	}, func(int64) int64 { return 0 })
	if errors.Is(err, execution.ErrNoTransition) {
		return nil
	}
	return err
}

// sweepNeeded keeps idle scheduler ticks outside the operation ledger. All
// selected work is re-evaluated by sweepOn inside the audited transaction.
func (service *Service) sweepNeeded(ctx context.Context, reader audit.Reader) (bool, error) {
	selected, ok, err := service.selectProvider(ctx, reader)
	if err != nil || !ok {
		return false, err
	}
	rows, err := reader.QueryContext(ctx, `SELECT a.id FROM execution_attempts a
		WHERE a.attempt_type='embedding' AND a.state='Queued'
		AND EXISTS (SELECT 1 FROM attempt_input_items i JOIN attempt_input_snapshots s ON s.id=i.snapshot_id
			WHERE s.attempt_id=a.id AND i.item_role='user')`)
	if err != nil {
		return false, err
	}
	orphan := false
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		if !service.hasLiveOriginator(id) {
			orphan = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || orphan {
		return orphan, err
	}
	current, hasCurrent, err := currentGenerationOn(ctx, reader)
	if err != nil {
		return false, err
	}
	model, version := selected.identity()
	currentMatches := hasCurrent && current.ModelName == model && current.ModelVersion == version
	generationID := current.ID
	if !currentMatches {
		generationID, ok, err = service.buildingGeneration(ctx, reader, model, version)
		if err != nil || !ok {
			return !ok, err
		}
	}
	var missing bool
	if err := reader.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM knowledge_search_docs d
		WHERE length(d.title)+length(d.body)>0
		AND NOT EXISTS (SELECT 1 FROM embeddings e WHERE e.knowledge_version_id=d.knowledge_version_id AND e.embedding_generation_id=?))`, generationID).Scan(&missing); err != nil {
		return false, err
	}
	if missing {
		return true, nil
	}
	active, err := service.activeAttemptID(ctx, reader, generationID)
	if err != nil || active != 0 {
		return false, err
	}
	pending, err := service.pendingCount(ctx, reader, generationID)
	if err != nil || pending > 0 {
		return pending > 0, err
	}
	if currentMatches {
		return false, nil
	}
	failed, err := service.failedCount(ctx, reader, generationID)
	return failed == 0, err
}

// convergeOrphanQueryAttempts interrupts Queued embedding attempts whose
// input cannot be rebuilt because their live HTTP originator is gone (the
// query text is request-scoped by design). Without this convergence a
// restart-leaved query attempt would hold the generation scope forever.
func (service *Service) convergeOrphanQueryAttempts(ctx context.Context, tx *execution.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT a.id FROM execution_attempts a
		WHERE a.attempt_type='embedding' AND a.state='Queued'
		  AND EXISTS (SELECT 1 FROM attempt_input_items i
		              JOIN attempt_input_snapshots s ON s.id=i.snapshot_id
		              WHERE s.attempt_id=a.id AND i.item_role='user')`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		// The registry is in-memory by design (the query text is
		// request-scoped), so absence is exactly the restart signature; no
		// pool access happens inside this open transaction.
		if !service.hasLiveOriginator(id) {
			// The frozen state machine closes an unbound Queued attempt as
			// Cancelled (Interrupted requires a dispatch binding).
			if _, err := tx.ExecContext(ctx, `
				UPDATE execution_attempts SET state='Cancelled', ended_at=?, termination_reason='cancelled', row_version=row_version+1
				WHERE id=? AND state='Queued'`, service.nowText(), id); err != nil {
				return err
			}
		}
	}
	return nil
}

// hasLiveOriginator reports whether the in-process registry still holds the
// request-scoped query text of one query attempt.
func (service *Service) hasLiveOriginator(attemptID int64) bool {
	service.mu.Lock()
	defer service.mu.Unlock()
	_, ok := service.queryTexts[attemptID]
	return ok
}

// sweepOn runs one reconciled sweep inside the caller's transaction.
func (service *Service) sweepOn(ctx context.Context, tx *execution.Tx, selected provider) error {
	if err := service.convergeOrphanQueryAttempts(ctx, tx); err != nil {
		return err
	}
	modelName, modelVersion := selected.identity()
	// 1. Drift: the current generation must match the provider identity.
	var currentID int64
	var currentModel, currentVersion string
	err := tx.QueryRowContext(ctx, `SELECT id, model_name, model_version FROM embedding_generations WHERE state='current'`).Scan(&currentID, &currentModel, &currentVersion)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// First generation (or all retired): build one.
		if _, err := service.ensureGeneration(ctx, tx, selected); err != nil {
			return err
		}
	case err != nil:
		return err
	case currentModel == modelName && currentVersion == modelVersion:
		// Serving the right identity: backfill newly eligible versions into
		// the current generation (append-only projection growth).
		if err := service.enqueuePending(ctx, tx, currentID, selected); err != nil {
			return err
		}
		return service.maybeCreateAttempt(ctx, tx, currentID, selected)
	default:
		// Model/revision drift: build the next generation for the new
		// identity; the old current keeps serving until the atomic switch.
		if _, err := service.ensureGeneration(ctx, tx, selected); err != nil {
			return err
		}
	}
	// 2. Drive the building generation of this identity.
	buildingID, ok, err := service.buildingGeneration(ctx, tx, modelName, modelVersion)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := service.enqueuePending(ctx, tx, buildingID, selected); err != nil {
		return err
	}
	// 3. Finalize when nothing is pending and no attempt is active; failed
	// rows are terminal per-version states and do not block the switch.
	finalized, err := service.finalizeIfSettled(ctx, tx, buildingID)
	if err != nil {
		return err
	}
	if finalized {
		return nil
	}
	return service.maybeCreateAttempt(ctx, tx, buildingID, selected)
}

// ensureGeneration creates the building generation for the provider identity
// when none exists, returning its id.
func (service *Service) ensureGeneration(ctx context.Context, w executor, selected provider) (int64, error) {
	modelName, modelVersion := selected.identity()
	if id, ok, err := service.buildingGeneration(ctx, w, modelName, modelVersion); err != nil || ok {
		return id, err
	}
	insert, err := w.ExecContext(ctx, `
		INSERT INTO embedding_generations(model_name, model_version, generation, state, vector_dim, created_at)
		VALUES(?,?,COALESCE((SELECT MAX(generation) FROM embedding_generations),0)+1,'building',?,?)`,
		modelName, modelVersion, selected.VectorDim, service.nowText())
	if err != nil {
		return 0, err
	}
	return insert.LastInsertId()
}

func (service *Service) buildingGeneration(ctx context.Context, q queryer, modelName, modelVersion string) (int64, bool, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM embedding_generations WHERE state='building' AND model_name=? AND model_version=? ORDER BY generation DESC LIMIT 1`, modelName, modelVersion).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// enqueuePending inserts pending embeddings rows for every eligible version
// (current ∧ not exited, i.e. present in knowledge_search_docs) that has no
// row in the target generation yet.
func (service *Service) enqueuePending(ctx context.Context, w executor, generationID int64, selected provider) error {
	_, err := w.ExecContext(ctx, `
		INSERT INTO embeddings(knowledge_version_id, embedding_generation_id, state, updated_at)
		SELECT d.knowledge_version_id, ?, 'pending', ?
		FROM knowledge_search_docs d
		WHERE NOT EXISTS (
			SELECT 1 FROM embeddings e
			WHERE e.knowledge_version_id=d.knowledge_version_id AND e.embedding_generation_id=?)
		AND length(d.title) + length(d.body) > 0`,
		generationID, service.nowText(), generationID)
	return err
}

// maybeCreateAttempt creates one rebuild attempt when the generation holds
// pending rows and no active attempt owns the scope (DATA-ATTEMPT-002:
// one active per embedding_generation).
func (service *Service) maybeCreateAttempt(ctx context.Context, tx *execution.Tx, generationID int64, selected provider) error {
	pending, err := service.pendingCount(ctx, tx, generationID)
	if err != nil || pending == 0 {
		return err
	}
	active, err := service.activeAttemptID(ctx, tx, generationID)
	if err != nil || active != 0 {
		return err
	}
	_, err = service.insertRebuildAttempt(ctx, tx, generationID, selected)
	return err
}

func (service *Service) pendingCount(ctx context.Context, q queryer, generationID int64) (int64, error) {
	var count int64
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM embeddings WHERE embedding_generation_id=? AND state='pending'`, generationID).Scan(&count)
	return count, err
}

func (service *Service) failedCount(ctx context.Context, q queryer, generationID int64) (int64, error) {
	var count int64
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM embeddings WHERE embedding_generation_id=? AND state='failed'`, generationID).Scan(&count)
	return count, err
}

func (service *Service) activeAttemptID(ctx context.Context, q queryer, generationID int64) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `
		SELECT id FROM execution_attempts
		WHERE attempt_type='embedding' AND scope_type='embedding_generation' AND scope_id=?
		  AND state IN ('Queued','Assigned','Running','Cancelling') LIMIT 1`, generationID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// finalizeIfSettled atomically switches a settled building generation to
// current (validating the switch) and retires the previous current. A
// generation is settled only when it is COMPLETE: every eligible version
// has a ready vector — no pending and no failed rows — and no attempt is
// active (DATA-EMBED-002: 完整构建、校验后原子切换). A failed batch keeps
// the old generation serving; recovery is a fresh generation, never a
// partial switch.
func (service *Service) finalizeIfSettled(ctx context.Context, w executor, buildingID int64) (bool, error) {
	pending, err := service.pendingCount(ctx, w, buildingID)
	if err != nil || pending > 0 {
		return false, err
	}
	failed, err := service.failedCount(ctx, w, buildingID)
	if err != nil || failed > 0 {
		return false, err
	}
	active, err := service.activeAttemptID(ctx, w, buildingID)
	if err != nil || active != 0 {
		return false, err
	}
	now := service.nowText()
	if _, err := w.ExecContext(ctx, `UPDATE embedding_generations SET state='retired' WHERE state='current'`); err != nil {
		return false, err
	}
	result, err := w.ExecContext(ctx, `
		UPDATE embedding_generations SET state='current', built_at=COALESCE(built_at,?), validated_at=COALESCE(validated_at,?)
		WHERE id=? AND state='building'`, now, now, buildingID)
	if err != nil {
		return false, err
	}
	affected, _ := result.RowsAffected()
	return affected == 1, nil
}

// CurrentGeneration returns the serving generation, if any.
func (service *Service) CurrentGeneration(ctx context.Context) (GenerationView, bool, error) {
	return currentGenerationOn(ctx, service.reader)
}

func currentGenerationOn(ctx context.Context, reader audit.Reader) (GenerationView, bool, error) {
	var view GenerationView
	err := reader.QueryRowContext(ctx, `
		SELECT id, model_name, model_version, generation, COALESCE(vector_dim,0)
		FROM embedding_generations WHERE state='current'`).
		Scan(&view.ID, &view.ModelName, &view.ModelVersion, &view.Generation, &view.VectorDim)
	if errors.Is(err, sql.ErrNoRows) {
		return view, false, nil
	}
	if err != nil {
		return view, false, err
	}
	return view, true, nil
}

// GenerationView is the serving generation projection.
type GenerationView struct {
	ID           int64
	ModelName    string
	ModelVersion string
	Generation   int64
	VectorDim    int64
}

// BuildingExists reports whether any building generation exists (the index
// is being replaced; used for the honest rebuilding index state).
func (service *Service) BuildingExists(ctx context.Context) (bool, error) {
	return buildingExistsOn(ctx, service.reader)
}

func buildingExistsOn(ctx context.Context, reader audit.Reader) (bool, error) {
	var one int
	err := reader.QueryRowContext(ctx, `SELECT 1 FROM embedding_generations WHERE state='building' LIMIT 1`).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// EmbeddingState resolves one version's derived index state relative to the
// current generation (never a second eligibility authority).
func (service *Service) EmbeddingState(ctx context.Context, knowledgeVersionID int64) (string, error) {
	states, err := service.EmbeddingStates(ctx, []int64{knowledgeVersionID})
	if err != nil {
		return "", err
	}
	return states[knowledgeVersionID], nil
}

// EmbeddingStates resolves the derived index state for many versions at
// once. States: not_configured | pending | ready | failed | stale |
// rebuilding.
func (service *Service) EmbeddingStates(ctx context.Context, versionIDs []int64) (map[int64]string, error) {
	states := make(map[int64]string, len(versionIDs))
	if len(versionIDs) == 0 {
		return states, nil
	}
	// Current generation, building flag and per-version rows read inside one
	// read-only snapshot so a concurrent switch cannot split the projection
	// across two generations.
	err := service.snapshot(ctx, func(q audit.Reader) error {
		current, hasCurrent, err := currentGenerationOn(ctx, q)
		if err != nil {
			return err
		}
		building, err := buildingExistsOn(ctx, q)
		if err != nil {
			return err
		}
		for _, id := range versionIDs {
			switch {
			case !hasCurrent:
				if building {
					states[id] = "rebuilding"
				} else {
					states[id] = "not_configured"
				}
			default:
				var state sql.NullString
				err := q.QueryRowContext(ctx, `
					SELECT e.state FROM embeddings e
					WHERE e.knowledge_version_id=? AND e.embedding_generation_id=?`, id, current.ID).Scan(&state)
				switch {
				case errors.Is(err, sql.ErrNoRows):
					// No row for the serving generation: stale when older
					// generations embedded this text before, pending otherwise.
					var older int64
					olderErr := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM embeddings WHERE knowledge_version_id=? AND state='ready'`, id).Scan(&older)
					if olderErr != nil {
						return olderErr
					}
					if older > 0 {
						states[id] = "stale"
					} else if building {
						states[id] = "rebuilding"
					} else {
						states[id] = "pending"
					}
				case err != nil:
					return err
				default:
					states[id] = state.String
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return states, nil
}

// queryOutcome delivers one query-attempt result to its HTTP waiter.
type queryOutcome struct {
	vector []float32
	err    error
}
