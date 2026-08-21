package connections

// Connection Probe closure (T07): probeConnection creates the canonical
// connection_probe Execution Attempt with a frozen input snapshot and the
// dedicated probe grant in one transaction (HTTP-COMMAND-013), dispatches it
// to the live Plinth control stream, and CommitProbeResult is the ONLY
// terminal write path: header + typed child + attempt terminal state in one
// transaction (RUNTIME-AGENT-010).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	sharedops "github.com/Suknna/quoin/internal/ops"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
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
	case TypeThanos:
		return "thanos-query-v1", 1, nil
	case TypeKubernetes:
		return "kubernetes-read-capabilities-v1", 1, nil
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

// StartProbe creates the Attempt (Queued → Assigned with the current Plinth
// binding), freezes the input snapshot, creates the probe grant, then hands
// the dispatch envelope to the live stream (DATA-CONN-002: probe may run
// while disabled/revalidation; the grant still binds the current pair and
// current root binding).
func (service *Service) StartProbe(ctx context.Context, name string, runtimes *qruntime.Service, dispatch Dispatcher) (int64, error) {
	summary, err := service.Get(ctx, name)
	if err != nil {
		return 0, err
	}
	actionSetID, actionSetVersion, err := ActionSet(summary.Type)
	if err != nil {
		return 0, err
	}
	contractDigest, err := ProbeContractDigest()
	if err != nil {
		return 0, err
	}
	input, err := json.Marshal(ProbeInput{ConnectionName: summary.Name})
	if err != nil {
		return 0, err
	}
	inputDigest := sha256.Sum256(input)
	// Resolve the live binding BEFORE opening the write transaction: View
	// also draws from the single-connection pool, so calling it inside the
	// transaction would self-deadlock.
	var binding *qruntime.SlotView
	if runtimes != nil {
		view, viewErr := runtimes.View(ctx, qruntime.SlotPlinth)
		if viewErr == nil {
			binding = &view
		}
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	// One active probe per connection (ux_execution_attempt_active_scope).
	var active int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_attempts WHERE attempt_type='connection_probe' AND scope_type='connection' AND scope_id=? AND state IN ('Queued','Assigned','Running','Cancelling')`, summary.ID).Scan(&active); err != nil {
		return 0, err
	}
	if active > 0 {
		return 0, ErrActiveConflict
	}
	now := service.now().UTC().Format(time.RFC3339Nano)
	// connection_probe attempts carry NO agent_version: the supervisor is
	// deterministic program execution, not an agent generation (frozen
	// dispatch trigger requires agent_version IS NULL for this type).
	attemptInsert, err := conn.ExecContext(ctx, `INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,created_at) VALUES('connection_probe','connection',?,'Queued',?,?)`, summary.ID, releaseVersion, now)
	if err != nil {
		return 0, err
	}
	attemptID, err := attemptInsert.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(?, 'connection_probe_v1', 'v1', ?, ?)`, attemptID, hex.EncodeToString(inputDigest[:]), now); err != nil {
		return 0, err
	}
	snapshotID, err := lastInsertID(ctx, conn, `SELECT id FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID)
	if err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id) VALUES(?,1,'connection_config',?,?)`, snapshotID, hex.EncodeToString(inputDigest[:]), summary.CurrentRevisionID); err != nil {
		return 0, err
	}
	// Probe grants bind the current pair (DATA-CONN-008). The frozen
	// dispatch trigger requires model_provider attempts to carry BOTH the
	// chat and the embedding probe purposes; other types carry one.
	purposes := []string{summary.Type + "_probe"}
	if summary.Type == TypeModelProvider {
		purposes = []string{"model_probe_chat", "model_probe_embedding"}
	}
	var grantID int64
	for _, purpose := range purposes {
		grantInsert, grantErr := conn.ExecContext(ctx, `INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at) VALUES(?,?,?,?,?,?)`, attemptID, purpose, summary.ID, summary.CurrentRevisionID, summary.CurrentGenerationID, now)
		if grantErr != nil {
			return 0, grantErr
		}
		if purpose == purposes[0] {
			grantID, grantErr = grantInsert.LastInsertId()
			if grantErr != nil {
				return 0, grantErr
			}
		}
	}
	// Assign to the live plinth stream (slot must be registered).
	if binding == nil || binding.State != qruntime.StateRegistered || !binding.Connected {
		// Attempt stays Queued; the dispatcher retries when plinth connects.
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return 0, err
		}
		committed = true
		return attemptID, nil
	}
	lease := service.now().UTC().Add(5 * time.Minute).Format(time.RFC3339Nano)
	if _, err := conn.ExecContext(ctx, `UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id=?,connection_epoch=?,lease_until=?,runtime_release_version=?,row_version=row_version+1 WHERE id=? AND state='Queued'`, binding.BootID, *binding.ConnectionEpoch, lease, releaseVersion, attemptID); err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return 0, err
	}
	committed = true
	// Release the pooled connection before the dispatch callback: the
	// dispatcher re-reads the attempt grants from the same single-connection
	// pool and would otherwise self-deadlock.
	conn.Close()
	if dispatch != nil {
		dispatch(attemptID, summary, *binding.ConnectionEpoch, binding.BootID, grantID, input, contractDigest, actionSetID, actionSetVersion)
	}
	return attemptID, nil
}

func lastInsertID(ctx context.Context, conn *sql.Conn, query string, args ...any) (int64, error) {
	var id int64
	if err := conn.QueryRowContext(ctx, query, args...).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// releaseVersion is injected by the wiring (buildinfo).
var releaseVersion = "v0.1.0-dev"

func SetReleaseVersion(version string) { releaseVersion = version }

// Dispatcher receives the probe dispatch envelope after the transaction
// commits; production forwards it over the Plinth control stream.
type Dispatcher func(attemptID int64, summary Summary, epoch uint64, bootID string, grantID int64, input []byte, contractDigest, actionSetID string, actionSetVersion int)

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

// KubernetesProbeChild carries the kubernetes typed-child columns.
type KubernetesProbeChild struct {
	EffectiveNamespace string
	VersionOK          bool
	CoreDiscoveryOK    bool
	GroupedDiscoveryOK bool
	PodsGetAllowed     bool
	PodsListAllowed    bool
	EventsListAllowed  bool
	PodsLogGetAllowed  bool
	DetailJSON         string
}

// TypedChild selects the connection-type closed child row variant.
type TypedChild struct {
	Thanos        *ThanosProbeChild
	Kubernetes    *KubernetesProbeChild
	ModelProvider *ModelProviderProbeChild
}

// CommitProbeResult is the single terminal closure: header +
// typed child + attempt terminal in one transaction. In-flight cancellation
// obeys commit order — a late result after a cancellation fence is rejected
// (RUNTIME-CANCEL-002).
func (service *Service) CommitProbeResult(ctx context.Context, attemptID int64, bootID string, epoch uint64, result TypedProbeResult, child *TypedChild) error {
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
	var state string
	var scopeID int64
	var attemptBoot sql.NullString
	var attemptEpoch sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT state,scope_id,boot_id,connection_epoch FROM execution_attempts WHERE id=?`, attemptID).Scan(&state, &scopeID, &attemptBoot, &attemptEpoch); err != nil {
		return err
	}
	if state != "Running" {
		return fmt.Errorf("attempt %d is %s: results close only over Running attempts (late results only audit)", attemptID, state)
	}
	if attemptBoot.Valid && attemptBoot.String != bootID {
		return fmt.Errorf("boot fence mismatch")
	}
	if attemptEpoch.Valid && attemptEpoch.Int64 != int64(epoch) {
		return fmt.Errorf("epoch fence mismatch")
	}
	var connectionType string
	var revisionID, generationID int64
	var bindingRevision int
	// The closure binds the pair the attempt's grant froze — NOT the
	// connection's current pointers: a rotation committed while the probe
	// was in flight must not silently re-attribute the result to the new
	// pair (T09 revoke/rotate-vs-result commit-order race).
	if err := conn.QueryRowContext(ctx, `
		SELECT c.type, ag.connection_revision_id, ag.credential_generation_id
		FROM connections c
		JOIN attempt_connection_grants ag ON ag.attempt_id=? AND ag.connection_id=c.id
		ORDER BY ag.id LIMIT 1`, attemptID).Scan(&connectionType, &revisionID, &generationID); err != nil {
		return err
	}
	if err := conn.QueryRowContext(ctx, `SELECT binding_revision FROM root_key_state WHERE id=1`).Scan(&bindingRevision); err != nil {
		return err
	}
	actionSetID, actionSetVersion, err := ActionSet(connectionType)
	if err != nil {
		return err
	}
	contractDigest, err := ProbeContractDigest()
	if err != nil {
		return err
	}
	now := service.now().UTC().Format(time.RFC3339Nano)
	headerInsert, err := conn.ExecContext(ctx, `INSERT INTO connection_probe_results(attempt_id,connection_id,connection_type,connection_revision_id,credential_generation_id,root_binding_revision,action_set_id,action_set_version,probe_contract_digest,outcome,result_digest,started_at,finished_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		attemptID, scopeID, connectionType, revisionID, generationID, bindingRevision, actionSetID, actionSetVersion, contractDigest, result.Outcome, result.ResultDigest, result.StartedAt, result.FinishedAt, now)
	if err != nil {
		return err
	}
	headerID, err := headerInsert.LastInsertId()
	if err != nil {
		return err
	}
	if child == nil {
		return fmt.Errorf("typed child missing for connection type %s", connectionType)
	}
	switch connectionType {
	case TypeThanos:
		if child.Thanos == nil {
			return fmt.Errorf("thanos probe result requires the thanos typed child")
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO thanos_connection_probe_results(probe_result_id,query,response_type,sample_count,sample_value,detail_json) VALUES(?,?,?,?,?,?)`,
			headerID, child.Thanos.Query, child.Thanos.ResponseType, child.Thanos.SampleCount, child.Thanos.SampleValue, child.Thanos.DetailJSON); err != nil {
			return err
		}
	case TypeKubernetes:
		if child.Kubernetes == nil {
			return fmt.Errorf("kubernetes probe result requires the kubernetes typed child")
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO kubernetes_connection_probe_results(probe_result_id,effective_namespace,version_ok,core_discovery_ok,grouped_discovery_ok,pods_get_allowed,pods_list_allowed,events_list_allowed,pods_log_get_allowed,detail_json) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			headerID, child.Kubernetes.EffectiveNamespace, boolInt(child.Kubernetes.VersionOK), boolInt(child.Kubernetes.CoreDiscoveryOK), boolInt(child.Kubernetes.GroupedDiscoveryOK), boolInt(child.Kubernetes.PodsGetAllowed), boolInt(child.Kubernetes.PodsListAllowed), boolInt(child.Kubernetes.EventsListAllowed), boolInt(child.Kubernetes.PodsLogGetAllowed), child.Kubernetes.DetailJSON); err != nil {
			return err
		}
	case TypeModelProvider:
		if child.ModelProvider == nil {
			return fmt.Errorf("model provider probe result requires the model provider typed child")
		}
		var embeddingModelID any
		var embeddingVectorDim any
		if child.ModelProvider.EmbeddingSupported {
			if child.ModelProvider.EmbeddingModelID == nil || *child.ModelProvider.EmbeddingModelID == "" {
				return fmt.Errorf("embedding-supported probe requires embeddingModelId")
			}
			if child.ModelProvider.EmbeddingVectorDim < 1 {
				return fmt.Errorf("embedding-supported probe requires a positive vector dimension")
			}
			embeddingModelID = *child.ModelProvider.EmbeddingModelID
			embeddingVectorDim = child.ModelProvider.EmbeddingVectorDim
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO model_provider_connection_probe_results(probe_result_id,chat_model_id,embedding_model_id,context_budget_tokens,max_output_tokens,streaming_supported,native_tool_calling_supported,multi_tool_call_supported,cancellation_observed,usage_observed,request_id_observed,embedding_supported,embedding_vector_dim,detail_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			headerID, child.ModelProvider.ChatModelID, embeddingModelID, child.ModelProvider.ContextBudgetTokens, child.ModelProvider.MaxOutputTokens,
			boolInt(child.ModelProvider.StreamingSupported), boolInt(child.ModelProvider.NativeToolCallingSupported), boolInt(child.ModelProvider.MultiToolCallSupported),
			boolInt(child.ModelProvider.CancellationObserved), boolInt(child.ModelProvider.UsageObserved), boolInt(child.ModelProvider.RequestIDObserved),
			boolInt(child.ModelProvider.EmbeddingSupported), embeddingVectorDim, child.ModelProvider.DetailJSON); err != nil {
			return err
		}
	default:
		return fmt.Errorf("connection type %q has no supervisor probe child", connectionType)
	}
	// Terminal state and termination reason commit in one versioned update.
	terminalState := "Succeeded"
	terminationReason := any(nil)
	if result.Outcome != "passed" {
		terminalState = "Failed"
		terminationReason = "invalid_response"
	}
	if _, err := conn.ExecContext(ctx, `UPDATE execution_attempts SET state=?,ended_at=?,termination_reason=?,row_version=row_version+1 WHERE id=? AND state='Running'`, terminalState, result.FinishedAt, terminationReason, attemptID); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	sharedops.LogEvent("quoin", "info", "connection.probe_committed", "attempt="+fmt.Sprint(attemptID)+" outcome="+result.Outcome)
	return nil
}

// AcceptProbe moves Assigned → Running (AttemptAccept). The fence matches
// the dispatch boot only: the frozen schema makes the binding epoch
// immutable once set, so a re-dispatched Assigned probe accepts on the
// newer epoch of the same boot (RUNTIME-TASK-005); the inbound envelope
// fence already proved the frame arrived on the current stream.
func (service *Service) AcceptProbe(ctx context.Context, attemptID int64, bootID string, epoch uint64) error {
	_ = epoch // transport context only; see the fence note above
	result, err := service.db.ExecContext(ctx, `UPDATE execution_attempts SET state='Running',accepted_at=?,row_version=row_version+1 WHERE id=? AND state='Assigned' AND boot_id=?`, service.now().UTC().Format(time.RFC3339Nano), attemptID, bootID)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return fmt.Errorf("attempt %d not in the expected Assigned state for this boot", attemptID)
	}
	return nil
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
	err := service.db.QueryRowContext(ctx, `SELECT id,state,row_version,created_at,started_at,ended_at,termination_reason FROM execution_attempts WHERE id=? AND attempt_type='connection_probe' AND scope_type='connection' AND scope_id=?`, attemptID, connectionID).
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
	err := service.db.QueryRowContext(ctx, `SELECT id,state,row_version,created_at,started_at,ended_at,termination_reason FROM execution_attempts WHERE attempt_type='connection_probe' AND scope_type='connection' AND scope_id=? AND state IN ('Queued','Assigned','Running','Cancelling') ORDER BY id DESC LIMIT 1`, connectionID).
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
	rows, err := service.db.QueryContext(ctx, `SELECT r.id,r.attempt_id,r.connection_type,r.connection_revision_id,r.credential_generation_id,r.root_binding_revision,r.action_set_id,r.action_set_version,r.probe_contract_digest,r.outcome,r.result_digest,r.started_at,r.finished_at,COALESCE((SELECT t.detail_json FROM thanos_connection_probe_results t WHERE t.probe_result_id=r.id),''),COALESCE((SELECT k.detail_json FROM kubernetes_connection_probe_results k WHERE k.probe_result_id=r.id),''),COALESCE((SELECT m.detail_json FROM model_provider_connection_probe_results m WHERE m.probe_result_id=r.id),'') FROM connection_probe_results r WHERE r.connection_id=? AND (?='' OR r.id < ?) ORDER BY r.id DESC LIMIT ?`, connectionID, after, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var results []ProbeResultView
	for rows.Next() {
		var view ProbeResultView
		var thanosDetail, k8sDetail, mpDetail string
		if err := rows.Scan(&view.ID, &view.AttemptID, &view.ConnectionType, &view.ConnectionRevisionID, &view.CredentialGenerationID, &view.RootBindingRevision, &view.ActionSetID, &view.ActionSetVersion, &view.ProbeContractDigest, &view.Outcome, &view.ResultDigest, &view.StartedAt, &view.FinishedAt, &thanosDetail, &k8sDetail, &mpDetail); err != nil {
			return nil, false, err
		}
		view.DetailJSON = thanosDetail
		if view.DetailJSON == "" {
			view.DetailJSON = k8sDetail
		}
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
	rows, err := service.db.QueryContext(ctx, `SELECT id,revision_seq,config_json,created_at FROM connection_revisions WHERE connection_id=? AND (?='' OR revision_seq < ?) ORDER BY revision_seq DESC LIMIT ?`, connectionID, after, after, limit+1)
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
	rows, err := service.db.QueryContext(ctx, `SELECT id,generation_seq,COALESCE(created_by,0),created_at FROM credential_generations WHERE connection_id=? AND (?='' OR generation_seq < ?) ORDER BY generation_seq DESC LIMIT ?`, connectionID, after, after, limit+1)
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
	if err = service.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM connection_revisions WHERE connection_id=?),(SELECT COUNT(*) FROM credential_generations WHERE connection_id=?)`, connectionID, connectionID).Scan(&revisions, &generations); err != nil {
		return 0, 0, err
	}
	return revisions, generations, nil
}
