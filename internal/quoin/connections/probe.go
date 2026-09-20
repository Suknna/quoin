package connections

// Connection Probe closure (T07, ADR-0011 本地执行形态): StartProbe creates the
// canonical connection_probe Execution Attempt with a frozen input snapshot
// and the dedicated probe grant in one audited runner transaction
// (HTTP-COMMAND-013); execution itself is Quoin-local (the app-layer local
// execution loop binds, runs the metrics_probe internal tool over the Stele
// gateway and calls CommitProbeResult) — the ONLY terminal write path:
// header + typed child + attempt terminal state in one transaction
// (RUNTIME-AGENT-010).
//
// Attempt creation goes through attempt.CreateOn so the caller's execution
// metadata (correlation, initiator) is persisted atomically with the attempt
// row (ADR-0006); a context without execution metadata fails closed. The
// executor-driven lifecycle steps (accept, result commit, cancel ack,
// interrupt, queued bind) run under the attempt-scoped system task context
// (probeLifecycleContext: persisted correlation restored, unrelated
// inherited metadata rejected) with their automatic audit rows.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// ProbeContractDigest computes the SHA-256 of the frozen
// contracts/connection-probes.yaml content as carried by the generated
// assets (probe_contract_source must be set by the wiring test/launcher).
var ProbeContractSource func() string

func ProbeContractDigest() (string, error) {
	if ProbeContractSource == nil {
		return "", errors.New("probe contract source not wired")
	}
	sum := sha256.Sum256([]byte(ProbeContractSource()))
	return hex.EncodeToString(sum[:]), nil
}

// ActionSet identifies the frozen action set for a connection type.
func ActionSet(connectionType string) (string, int, error) {
	switch connectionType {
	case TypePrometheus:
		return "prometheus-query-v1", 1, nil
	case TypeThanos:
		return "thanos-query-v1", 1, nil
	case TypeModelProvider:
		return "model-provider-capabilities-v1", 1, nil
	default:
		return "", 0, fmt.Errorf("unknown connection type %q", connectionType)
	}
}

// ProbeInput is the canonical connection_probe input snapshot
// (schema_kind connection_probe_v1).
type ProbeInput struct {
	ConnectionName string `json:"connectionName"`
}

// probeStartResult carries the created attempt out of the audited business
// stage: the attempt id (the audit domain reference).
type probeStartResult struct {
	attemptID int64
}

// StartProbe creates the Attempt (Queued) and freezes the input snapshot and
// the probe grant in one audited runner transaction (HTTP-COMMAND-013,
// ADR-0011). Execution is local: the Quoin local execution loop binds and
// runs the probe through the metrics_probe internal tool over the Stele
// gateway, so creation no longer couples to any runtime stream binding.
func (service *Service) StartProbe(ctx context.Context, name string) (int64, error) {
	summary, err := service.Get(ctx, name)
	if err != nil {
		return 0, err
	}
	input, err := json.Marshal(ProbeInput{ConnectionName: summary.Name})
	if err != nil {
		return 0, err
	}
	inputDigest := sha256.Sum256(input)
	result, err := execution.Execute(ctx, service.commands.runner, service.commands.probeStart, func(tx *execution.Tx) (probeStartResult, error) {
		// One active probe per connection (ux_execution_attempt_active_scope).
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_attempts WHERE attempt_type='connection_probe' AND scope_type='connection' AND scope_id=? AND state IN ('Queued','Assigned','Running','Cancelling')`, summary.ID).Scan(&active); err != nil {
			return probeStartResult{}, err
		}
		if active > 0 {
			return probeStartResult{}, rejectionOf(ErrActiveConflict, codeActiveConflict, "another probe attempt is still active", 0)
		}
		now := timestampOf(service.now)
		// connection_probe attempts carry NO agent_version: the supervisor is
		// deterministic program execution, not an agent generation (frozen
		// dispatch trigger requires agent_version IS NULL for this type).
		// CreateOn centrally persists the caller's correlation metadata onto
		// the new attempt in this same transaction (ADR-0006); a context
		// without execution metadata fails the creation.
		attemptID, err := attempt.CreateOn(ctx, tx, `INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,created_at) VALUES('connection_probe','connection',?,'Queued',?,?)`, summary.ID, releaseVersion, now)
		if err != nil {
			return probeStartResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(?, 'connection_probe_v1', 'v1', ?, ?)`, attemptID, hex.EncodeToString(inputDigest[:]), now); err != nil {
			return probeStartResult{}, err
		}
		snapshotID, err := lastInsertID(ctx, tx, `SELECT id FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID)
		if err != nil {
			return probeStartResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id) VALUES(?,1,'connection_config',?,?)`, snapshotID, hex.EncodeToString(inputDigest[:]), summary.CurrentRevisionID); err != nil {
			return probeStartResult{}, err
		}
		// Probe grants bind the current pair (DATA-CONN-008). The frozen
		// dispatch trigger requires model_provider attempts to carry BOTH the
		// chat and the embedding probe purposes; other types carry one.
		purposes := []string{summary.Type + "_probe"}
		if summary.Type == TypeModelProvider {
			purposes = []string{"model_probe_chat", "model_probe_embedding"}
		}
		for _, purpose := range purposes {
			if _, err := tx.ExecContext(ctx, `INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at) VALUES(?,?,?,?,?,?)`, attemptID, purpose, summary.ID, summary.CurrentRevisionID, summary.CurrentGenerationID, now); err != nil {
				return probeStartResult{}, err
			}
		}
		// The attempt stays Queued: the local execution loop claims it
		// (Queued→Assigned→Running under the local binding identity).
		return probeStartResult{attemptID: attemptID}, nil
	}, func(result probeStartResult) int64 { return result.attemptID })
	if err != nil {
		return 0, domainError(err)
	}
	return result.attemptID, nil
}

func lastInsertID(ctx context.Context, reader interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, query string, args ...any) (int64, error) {
	var id int64
	if err := reader.QueryRowContext(ctx, query, args...).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// releaseVersion is injected by the wiring (buildinfo).
var releaseVersion = "v0.1.0-dev"

func SetReleaseVersion(version string) { releaseVersion = version }

// TypedProbeResult is the supervisor's canonical typed observation.
type TypedProbeResult struct {
	Outcome      string          `json:"outcome"` // passed | failed
	Detail       json.RawMessage `json:"detail"`
	ResultDigest string          `json:"resultDigest"`
	StartedAt    string          `json:"startedAt"`
	FinishedAt   string          `json:"finishedAt"`
}

// ThanosProbeChild carries the thanos typed-child columns.
type ThanosProbeChild struct {
	Query        string
	ResponseType string
	SampleCount  int
	SampleValue  string
	DetailJSON   string
}

// ModelProviderProbeChild carries the model provider typed-child columns.
type ModelProviderProbeChild struct {
	ChatModelID                string
	EmbeddingModelID           *string
	ContextBudgetTokens        int
	MaxOutputTokens            int
	StreamingSupported         bool
	NativeToolCallingSupported bool
	MultiToolCallSupported     bool
	CancellationObserved       bool
	UsageObserved              bool
	RequestIDObserved          bool
	EmbeddingSupported         bool
	EmbeddingVectorDim         int
	DetailJSON                 string
}

// TypedChild selects the connection-type closed child row variant.
type TypedChild struct {
	Thanos        *ThanosProbeChild
	ModelProvider *ModelProviderProbeChild
}

// CommitProbeResult is the single terminal closure: header +
// typed child + attempt terminal in one audited transaction. In-flight
// cancellation obeys commit order — a late result after a cancellation fence
// is rejected (RUNTIME-CANCEL-002). The system task scope is explicit: a
// wired caller keeps its own correlation, every other caller gets a fresh
// system scope so the result commit is always attributable and associated
// with its attempt.
func (service *Service) CommitProbeResult(ctx context.Context, attemptID int64, bootID string, epoch uint64, result TypedProbeResult, child *TypedChild) error {
	ctx, err := service.probeLifecycleContext(ctx, attemptID)
	if err != nil {
		return err
	}
	if _, err := execution.Execute(ctx, service.commands.runner, service.commands.probeResult, func(tx *execution.Tx) (int64, error) {
		var state string
		var scopeID int64
		var attemptBoot sql.NullString
		var attemptEpoch sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT state,scope_id,boot_id,connection_epoch FROM execution_attempts WHERE id=?`, attemptID).Scan(&state, &scopeID, &attemptBoot, &attemptEpoch); err != nil {
			return 0, err
		}
		if state != "Running" {
			return 0, fmt.Errorf("attempt %d is %s: results close only over Running attempts (late results only audit)", attemptID, state)
		}
		if attemptBoot.Valid && attemptBoot.String != bootID {
			return 0, fmt.Errorf("boot fence mismatch")
		}
		if attemptEpoch.Valid && attemptEpoch.Int64 != int64(epoch) {
			return 0, fmt.Errorf("epoch fence mismatch")
		}
		var connectionType string
		var revisionID, generationID int64
		var bindingRevision int
		// The closure binds the pair the attempt's grant froze — NOT the
		// connection's current pointers: a rotation committed while the probe
		// was in flight must not silently re-attribute the result to the new
		// pair (T09 revoke/rotate-vs-result commit-order race).
		if err := tx.QueryRowContext(ctx, `
			SELECT c.type, ag.connection_revision_id, ag.credential_generation_id
			FROM connections c
			JOIN attempt_connection_grants ag ON ag.attempt_id=? AND ag.connection_id=c.id
			ORDER BY ag.id LIMIT 1`, attemptID).Scan(&connectionType, &revisionID, &generationID); err != nil {
			return 0, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT binding_revision FROM root_key_state WHERE id=1`).Scan(&bindingRevision); err != nil {
			return 0, err
		}
		actionSetID, actionSetVersion, err := ActionSet(connectionType)
		if err != nil {
			return 0, err
		}
		contractDigest, err := ProbeContractDigest()
		if err != nil {
			return 0, err
		}
		now := timestampOf(service.now)
		headerInsert, err := tx.ExecContext(ctx, `INSERT INTO connection_probe_results(attempt_id,connection_id,connection_type,connection_revision_id,credential_generation_id,root_binding_revision,action_set_id,action_set_version,probe_contract_digest,outcome,result_digest,started_at,finished_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			attemptID, scopeID, connectionType, revisionID, generationID, bindingRevision, actionSetID, actionSetVersion, contractDigest, result.Outcome, result.ResultDigest, result.StartedAt, result.FinishedAt, now)
		if err != nil {
			return 0, err
		}
		headerID, err := headerInsert.LastInsertId()
		if err != nil {
			return 0, err
		}
		if child == nil {
			return 0, fmt.Errorf("typed child missing for connection type %s", connectionType)
		}
		switch connectionType {
		case TypePrometheus, TypeThanos:
			if child.Thanos == nil {
				return 0, fmt.Errorf("metrics probe result requires the Prometheus-compatible typed child")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO thanos_connection_probe_results(probe_result_id,query,response_type,sample_count,sample_value,detail_json) VALUES(?,?,?,?,?,?)`,
				headerID, child.Thanos.Query, child.Thanos.ResponseType, child.Thanos.SampleCount, child.Thanos.SampleValue, child.Thanos.DetailJSON); err != nil {
				return 0, err
			}
		case TypeModelProvider:
			if child.ModelProvider == nil {
				return 0, fmt.Errorf("model provider probe result requires the model provider typed child")
			}
			var embeddingModelID any
			var embeddingVectorDim any
			if child.ModelProvider.EmbeddingSupported {
				if child.ModelProvider.EmbeddingModelID == nil || *child.ModelProvider.EmbeddingModelID == "" {
					return 0, fmt.Errorf("embedding-supported probe requires embeddingModelId")
				}
				if child.ModelProvider.EmbeddingVectorDim < 1 {
					return 0, fmt.Errorf("embedding-supported probe requires a positive vector dimension")
				}
				embeddingModelID = *child.ModelProvider.EmbeddingModelID
				embeddingVectorDim = child.ModelProvider.EmbeddingVectorDim
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO model_provider_connection_probe_results(probe_result_id,chat_model_id,embedding_model_id,context_budget_tokens,max_output_tokens,streaming_supported,native_tool_calling_supported,multi_tool_call_supported,cancellation_observed,usage_observed,request_id_observed,embedding_supported,embedding_vector_dim,detail_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				headerID, child.ModelProvider.ChatModelID, embeddingModelID, child.ModelProvider.ContextBudgetTokens, child.ModelProvider.MaxOutputTokens,
				boolInt(child.ModelProvider.StreamingSupported), boolInt(child.ModelProvider.NativeToolCallingSupported), boolInt(child.ModelProvider.MultiToolCallSupported),
				boolInt(child.ModelProvider.CancellationObserved), boolInt(child.ModelProvider.UsageObserved), boolInt(child.ModelProvider.RequestIDObserved),
				boolInt(child.ModelProvider.EmbeddingSupported), embeddingVectorDim, child.ModelProvider.DetailJSON); err != nil {
				return 0, err
			}
		default:
			return 0, fmt.Errorf("connection type %q has no supervisor probe child", connectionType)
		}
		// Terminal state and termination reason commit in one versioned update.
		terminalState := "Succeeded"
		terminationReason := any(nil)
		if result.Outcome != "passed" {
			terminalState = "Failed"
			terminationReason = "invalid_response"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE execution_attempts SET state=?,ended_at=?,termination_reason=?,row_version=row_version+1 WHERE id=? AND state='Running'`, terminalState, result.FinishedAt, terminationReason, attemptID); err != nil {
			return 0, err
		}
		return attemptID, nil
	}, identity); err != nil {
		return err
	}
	ops.LogEvent("quoin", "info", "connection.probe_committed", "attempt="+fmt.Sprint(attemptID)+" outcome="+result.Outcome)
	return nil
}

// identity is the object-id accessor for attempt-scoped operations.
func identity(attemptID int64) int64 { return attemptID }

// AcceptProbe moves Assigned → Running (AttemptAccept) as an audited system
// mutation. The fence matches the dispatch boot only: the frozen schema makes
// the binding epoch immutable once set, so a re-dispatched Assigned probe
// accepts on the newer epoch of the same boot (RUNTIME-TASK-005); the inbound
// envelope fence already proved the frame arrived on the current stream.
func (service *Service) AcceptProbe(ctx context.Context, attemptID int64, bootID string, epoch uint64) error {
	ctx, err := service.probeLifecycleContext(ctx, attemptID)
	if err != nil {
		return err
	}
	_ = epoch // transport context only; see the fence note above
	_, err = execution.Execute(ctx, service.commands.runner, service.commands.probeAccept, func(tx *execution.Tx) (int64, error) {
		result, err := tx.ExecContext(ctx, `UPDATE execution_attempts SET state='Running',accepted_at=?,row_version=row_version+1 WHERE id=? AND state='Assigned' AND boot_id=?`, timestampOf(service.now), attemptID, bootID)
		if err != nil {
			return 0, err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return 0, fmt.Errorf("attempt %d not in the expected Assigned state for this boot", attemptID)
		}
		return attemptID, nil
	}, identity)
	return err
}

// AttemptView is the AttemptSummary projection for connection probes.
type AttemptView struct {
	ID                int64
	State             string
	RowVersion        int64
	CreatedAt         string
	StartedAt         string
	EndedAt           string
	TerminationReason string
}

// Attempt reads one connection_probe attempt by id and owning connection.
func (service *Service) Attempt(ctx context.Context, connectionID, attemptID int64) (AttemptView, error) {
	var view AttemptView
	var started, ended, reason sql.NullString
	err := service.read().QueryRowContext(ctx, `SELECT id,state,row_version,created_at,started_at,ended_at,termination_reason FROM execution_attempts WHERE id=? AND attempt_type='connection_probe' AND scope_type='connection' AND scope_id=?`, attemptID, connectionID).
		Scan(&view.ID, &view.State, &view.RowVersion, &view.CreatedAt, &started, &ended, &reason)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AttemptView{}, ErrNotFound
		}
		return AttemptView{}, err
	}
	view.StartedAt, view.EndedAt, view.TerminationReason = started.String, ended.String, reason.String
	return view, nil
}

// ActiveProbeAttempt returns the connection's active probe attempt, if any.
func (service *Service) ActiveProbeAttempt(ctx context.Context, connectionID int64) (AttemptView, bool, error) {
	var view AttemptView
	var started, ended, reason sql.NullString
	err := service.read().QueryRowContext(ctx, `SELECT id,state,row_version,created_at,started_at,ended_at,termination_reason FROM execution_attempts WHERE attempt_type='connection_probe' AND scope_type='connection' AND scope_id=? AND state IN ('Queued','Assigned','Running','Cancelling') ORDER BY id DESC LIMIT 1`, connectionID).
		Scan(&view.ID, &view.State, &view.RowVersion, &view.CreatedAt, &started, &ended, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return AttemptView{}, false, nil
	}
	if err != nil {
		return AttemptView{}, false, err
	}
	view.StartedAt, view.EndedAt, view.TerminationReason = started.String, ended.String, reason.String
	return view, true, nil
}

// ProbeResultView is the immutable ConnectionProbeResult projection.
type ProbeResultView struct {
	ID                     int64
	AttemptID              int64
	ConnectionType         string
	ConnectionRevisionID   int64
	CredentialGenerationID int64
	RootBindingRevision    int
	ActionSetID            string
	ActionSetVersion       int
	ProbeContractDigest    string
	Outcome                string
	ResultDigest           string
	StartedAt              string
	FinishedAt             string
	DetailJSON             string
}

// ProbeResults lists the immutable typed result history (newest first).
func (service *Service) ProbeResults(ctx context.Context, connectionID int64, after string, limit int) ([]ProbeResultView, bool, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := service.read().QueryContext(ctx, `SELECT r.id,r.attempt_id,r.connection_type,r.connection_revision_id,r.credential_generation_id,r.root_binding_revision,r.action_set_id,r.action_set_version,r.probe_contract_digest,r.outcome,r.result_digest,r.started_at,r.finished_at,COALESCE((SELECT t.detail_json FROM thanos_connection_probe_results t WHERE t.probe_result_id=r.id),''),COALESCE((SELECT m.detail_json FROM model_provider_connection_probe_results m WHERE m.probe_result_id=r.id),'') FROM connection_probe_results r WHERE r.connection_id=? AND (?='' OR r.id < ?) ORDER BY r.id DESC LIMIT ?`, connectionID, after, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var results []ProbeResultView
	for rows.Next() {
		var view ProbeResultView
		var thanosDetail, mpDetail string
		if err := rows.Scan(&view.ID, &view.AttemptID, &view.ConnectionType, &view.ConnectionRevisionID, &view.CredentialGenerationID, &view.RootBindingRevision, &view.ActionSetID, &view.ActionSetVersion, &view.ProbeContractDigest, &view.Outcome, &view.ResultDigest, &view.StartedAt, &view.FinishedAt, &thanosDetail, &mpDetail); err != nil {
			return nil, false, err
		}
		view.DetailJSON = thanosDetail
		if view.DetailJSON == "" {
			view.DetailJSON = mpDetail
		}
		results = append(results, view)
	}
	more := false
	if len(results) > limit {
		more = true
		results = results[:limit]
	}
	return results, more, rows.Err()
}

// RevisionView is the non-secret revision projection.
type RevisionView struct {
	ID          int64
	RevisionSeq int64
	ConfigJSON  string
	CreatedAt   string
}

// Revisions lists non-secret revision history.
func (service *Service) Revisions(ctx context.Context, connectionID int64, after string, limit int) ([]RevisionView, bool, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := service.read().QueryContext(ctx, `SELECT id,revision_seq,config_json,created_at FROM connection_revisions WHERE connection_id=? AND (?='' OR revision_seq < ?) ORDER BY revision_seq DESC LIMIT ?`, connectionID, after, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var revisions []RevisionView
	for rows.Next() {
		var view RevisionView
		if err := rows.Scan(&view.ID, &view.RevisionSeq, &view.ConfigJSON, &view.CreatedAt); err != nil {
			return nil, false, err
		}
		revisions = append(revisions, view)
	}
	more := false
	if len(revisions) > limit {
		more = true
		revisions = revisions[:limit]
	}
	return revisions, more, rows.Err()
}

// GenerationView is the non-secret credential generation projection.
type GenerationView struct {
	ID            int64
	GenerationSeq int64
	CreatedBy     int64
	CreatedAt     string
}

// Generations lists non-secret generation history.
func (service *Service) Generations(ctx context.Context, connectionID int64, after string, limit int) ([]GenerationView, bool, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := service.read().QueryContext(ctx, `SELECT id,generation_seq,COALESCE(created_by,0),created_at FROM credential_generations WHERE connection_id=? AND (?='' OR generation_seq < ?) ORDER BY generation_seq DESC LIMIT ?`, connectionID, after, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var generations []GenerationView
	for rows.Next() {
		var view GenerationView
		if err := rows.Scan(&view.ID, &view.GenerationSeq, &view.CreatedBy, &view.CreatedAt); err != nil {
			return nil, false, err
		}
		generations = append(generations, view)
	}
	more := false
	if len(generations) > limit {
		more = true
		generations = generations[:limit]
	}
	return generations, more, rows.Err()
}

// Counts returns revision and generation totals for ConnectionDetail.
func (service *Service) Counts(ctx context.Context, connectionID int64) (revisions, generations int, err error) {
	if err = service.read().QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM connection_revisions WHERE connection_id=?),(SELECT COUNT(*) FROM credential_generations WHERE connection_id=?)`, connectionID, connectionID).Scan(&revisions, &generations); err != nil {
		return 0, 0, err
	}
	return revisions, generations, nil
}
