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
)

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

// Service owns the embedding projection lifecycle over the shared database.
type Service struct {
	db  *sql.DB
	now func() time.Time

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

// NewService builds the embedding projection service.
func NewService(db *sql.DB) *Service {
	return &Service{db: db, now: time.Now, waiters: map[int64]chan queryOutcome{}, queryTexts: map[int64]string{}}
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
func (service *Service) selectProvider(ctx context.Context, conn *sql.Conn) (provider, bool, error) {
	var selected provider
	var qualificationVersion, connectionVersion int64
	var outcome string
	var embeddingModel sql.NullString
	var embeddingSupported bool
	var embeddingDim sql.NullInt64
	err := conn.QueryRowContext(ctx, `
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
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	selected, ok, err := service.selectProvider(ctx, conn)
	if err != nil {
		return err
	}
	if !ok {
		// Without a qualified provider there is nothing to reconcile; close
		// the serialized window as a committed no-op.
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return err
		}
		committed = true
		return nil
	}
	if err := service.sweepOn(ctx, conn, selected); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// convergeOrphanQueryAttempts interrupts Queued embedding attempts whose
// input cannot be rebuilt because their live HTTP originator is gone (the
// query text is request-scoped by design). Without this convergence a
// restart-leaved query attempt would hold the generation scope forever.
func (service *Service) convergeOrphanQueryAttempts(ctx context.Context, conn *sql.Conn) {
	rows, err := conn.QueryContext(ctx, `
		SELECT a.id FROM execution_attempts a
		WHERE a.attempt_type='embedding' AND a.state='Queued'
		  AND EXISTS (SELECT 1 FROM attempt_input_items i
		              JOIN attempt_input_snapshots s ON s.id=i.snapshot_id
		              WHERE s.attempt_id=a.id AND i.item_role='user')`)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return
	}
	for _, id := range ids {
		// The registry is in-memory by design (the query text is
		// request-scoped), so absence is exactly the restart signature; no
		// pool access happens inside this open transaction.
		if !service.hasLiveOriginator(id) {
			// The frozen state machine closes an unbound Queued attempt as
			// Cancelled (Interrupted requires a dispatch binding).
			if _, err := conn.ExecContext(ctx, `
				UPDATE execution_attempts SET state='Cancelled', ended_at=?, termination_reason='cancelled', row_version=row_version+1
				WHERE id=? AND state='Queued'`, service.nowText(), id); err != nil {
				return
			}
		}
	}
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
func (service *Service) sweepOn(ctx context.Context, conn *sql.Conn, selected provider) error {
	service.convergeOrphanQueryAttempts(ctx, conn)
	modelName, modelVersion := selected.identity()
	// 1. Drift: the current generation must match the provider identity.
	var currentID int64
	var currentModel, currentVersion string
	err := conn.QueryRowContext(ctx, `SELECT id, model_name, model_version FROM embedding_generations WHERE state='current'`).Scan(&currentID, &currentModel, &currentVersion)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// First generation (or all retired): build one.
		if _, err := service.ensureGeneration(ctx, conn, selected); err != nil {
			return err
		}
	case err != nil:
		return err
	case currentModel == modelName && currentVersion == modelVersion:
		// Serving the right identity: backfill newly eligible versions into
		// the current generation (append-only projection growth).
		if err := service.enqueuePending(ctx, conn, currentID, selected); err != nil {
			return err
		}
		return service.maybeCreateAttempt(ctx, conn, currentID, selected)
	default:
		// Model/revision drift: build the next generation for the new
		// identity; the old current keeps serving until the atomic switch.
		if _, err := service.ensureGeneration(ctx, conn, selected); err != nil {
			return err
		}
	}
	// 2. Drive the building generation of this identity.
	buildingID, ok, err := service.buildingGeneration(ctx, conn, modelName, modelVersion)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := service.enqueuePending(ctx, conn, buildingID, selected); err != nil {
		return err
	}
	// 3. Finalize when nothing is pending and no attempt is active; failed
	// rows are terminal per-version states and do not block the switch.
	finalized, err := service.finalizeIfSettled(ctx, conn, buildingID)
	if err != nil {
		return err
	}
	if finalized {
		return nil
	}
	return service.maybeCreateAttempt(ctx, conn, buildingID, selected)
}

// ensureGeneration creates the building generation for the provider identity
// when none exists, returning its id.
func (service *Service) ensureGeneration(ctx context.Context, conn *sql.Conn, selected provider) (int64, error) {
	modelName, modelVersion := selected.identity()
	if id, ok, err := service.buildingGeneration(ctx, conn, modelName, modelVersion); err != nil || ok {
		return id, err
	}
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO embedding_generations(model_name, model_version, generation, state, vector_dim, created_at)
		VALUES(?,?,COALESCE((SELECT MAX(generation) FROM embedding_generations),0)+1,'building',?,?)`,
		modelName, modelVersion, selected.VectorDim, service.nowText())
	if err != nil {
		return 0, err
	}
	return insert.LastInsertId()
}

func (service *Service) buildingGeneration(ctx context.Context, conn *sql.Conn, modelName, modelVersion string) (int64, bool, error) {
	var id int64
	err := conn.QueryRowContext(ctx, `SELECT id FROM embedding_generations WHERE state='building' AND model_name=? AND model_version=? ORDER BY generation DESC LIMIT 1`, modelName, modelVersion).Scan(&id)
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
func (service *Service) enqueuePending(ctx context.Context, conn *sql.Conn, generationID int64, selected provider) error {
	_, err := conn.ExecContext(ctx, `
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
func (service *Service) maybeCreateAttempt(ctx context.Context, conn *sql.Conn, generationID int64, selected provider) error {
	pending, err := service.pendingCount(ctx, conn, generationID)
	if err != nil || pending == 0 {
		return err
	}
	active, err := service.activeAttemptID(ctx, conn, generationID)
	if err != nil || active != 0 {
		return err
	}
	_, err = service.insertRebuildAttempt(ctx, conn, generationID, selected)
	return err
}

func (service *Service) pendingCount(ctx context.Context, conn *sql.Conn, generationID int64) (int64, error) {
	var count int64
	err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM embeddings WHERE embedding_generation_id=? AND state='pending'`, generationID).Scan(&count)
	return count, err
}

func (service *Service) failedCount(ctx context.Context, conn *sql.Conn, generationID int64) (int64, error) {
	var count int64
	err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM embeddings WHERE embedding_generation_id=? AND state='failed'`, generationID).Scan(&count)
	return count, err
}

func (service *Service) activeAttemptID(ctx context.Context, conn *sql.Conn, generationID int64) (int64, error) {
	var id int64
	err := conn.QueryRowContext(ctx, `
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
func (service *Service) finalizeIfSettled(ctx context.Context, conn *sql.Conn, buildingID int64) (bool, error) {
	pending, err := service.pendingCount(ctx, conn, buildingID)
	if err != nil || pending > 0 {
		return false, err
	}
	failed, err := service.failedCount(ctx, conn, buildingID)
	if err != nil || failed > 0 {
		return false, err
	}
	active, err := service.activeAttemptID(ctx, conn, buildingID)
	if err != nil || active != 0 {
		return false, err
	}
	now := service.nowText()
	if _, err := conn.ExecContext(ctx, `UPDATE embedding_generations SET state='retired' WHERE state='current'`); err != nil {
		return false, err
	}
	result, err := conn.ExecContext(ctx, `
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
	var view GenerationView
	err := service.db.QueryRowContext(ctx, `
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
	var one int
	err := service.db.QueryRowContext(ctx, `SELECT 1 FROM embedding_generations WHERE state='building' LIMIT 1`).Scan(&one)
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
	current, hasCurrent, err := service.CurrentGeneration(ctx)
	if err != nil {
		return nil, err
	}
	building, err := service.BuildingExists(ctx)
	if err != nil {
		return nil, err
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
			err := service.db.QueryRowContext(ctx, `
				SELECT e.state FROM embeddings e
				WHERE e.knowledge_version_id=? AND e.embedding_generation_id=?`, id, current.ID).Scan(&state)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				// No row for the serving generation: stale when older
				// generations embedded this text before, pending otherwise.
				var older int64
				olderErr := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM embeddings WHERE knowledge_version_id=? AND state='ready'`, id).Scan(&older)
				if olderErr != nil {
					return nil, olderErr
				}
				if older > 0 {
					states[id] = "stale"
				} else if building {
					states[id] = "rebuilding"
				} else {
					states[id] = "pending"
				}
			case err != nil:
				return nil, err
			default:
				states[id] = state.String
			}
		}
	}
	return states, nil
}

// queryOutcome delivers one query-attempt result to its HTTP waiter.
type queryOutcome struct {
	vector []float32
	err    error
}
