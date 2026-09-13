package connections

// Credential grant fulfillment (T07): FetchCredentialGrant is the only path
// by which a sealed connection secret leaves storage — decrypted per
// attempt, fenced on the grant/attempt/boot/epoch binding, and unavailable
// once the attempt reaches a terminal state (DATA-CONN-002). Cancellation
// follows the commit-order fence: a result proposal landing after a
// committed Cancelling fence is rejected (RUNTIME-CANCEL-002).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/tools/kubernetes"
	"github.com/Suknna/quoin/internal/quoin/tools/thanos"
)

// GrantPayload is the typed fulfillment returned to the runtime over the
// authenticated gRPC channel (never persisted in this shape).
type GrantPayload struct {
	GrantID              int64
	AttemptID            int64
	ConnectionRevisionID int64
	CredentialGeneration int64
	ConnectionType       string
	RevisionConfigJSON   json.RawMessage
	Metrics              *MetricsCredentialSecret
	// Thanos remains a compatibility alias for existing internal callers. New
	// code must use Metrics; both fields point at equivalent non-persisted data.
	Thanos        *MetricsCredentialSecret
	Kubernetes    *KubernetesCredentialSecret
	ModelProvider *ModelProviderCredentialSecret
}

// MetricsCredentialSecret mirrors runtime.proto ThanosCredentialSecret. The
// established wire slot carries both Prometheus and Thanos credentials; the
// connection_type discriminator preserves their distinct semantics.
type MetricsCredentialSecret struct {
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
	BearerToken string `json:"bearerToken,omitempty"`
}

// KubernetesCredentialSecret mirrors runtime.proto KubernetesCredentialSecret.
type KubernetesCredentialSecret struct {
	Kubeconfig string `json:"kubeconfig"`
}

// ModelProviderCredentialSecret mirrors runtime.proto ModelProviderCredentialSecret.
type ModelProviderCredentialSecret struct {
	APIKey string `json:"apiKey"`
}

// ErrGrantDenied reports fenced or terminal grant fulfillment.
var ErrGrantDenied = errors.New("credential grant denied")

// FulfillGrant decrypts the sealed secret for one active attempt after
// re-checking every binding (replay after terminal state is denied).
func (service *Service) FulfillGrant(ctx context.Context, grantID, attemptID int64, bootID string, epoch uint64) (GrantPayload, error) {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return GrantPayload{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return GrantPayload{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var purpose string
	var grantAttempt, connectionID, revisionID, generationID int64
	if err := conn.QueryRowContext(ctx, `SELECT attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id FROM attempt_connection_grants WHERE id=?`, grantID).Scan(&grantAttempt, &purpose, &connectionID, &revisionID, &generationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return GrantPayload{}, ErrGrantDenied
		}
		return GrantPayload{}, err
	}
	// The runtime's claimed attempt must be the grant's own attempt.
	if grantAttempt != attemptID {
		return GrantPayload{}, fmt.Errorf("%w: grant belongs to attempt %d", ErrGrantDenied, grantAttempt)
	}
	var state string
	var attemptType string
	var attemptBoot sql.NullString
	var attemptEpoch sql.NullInt64
	var scopeID int64
	if err := conn.QueryRowContext(ctx, `SELECT state,attempt_type,scope_id,boot_id,connection_epoch FROM execution_attempts WHERE id=?`, attemptID).Scan(&state, &attemptType, &scopeID, &attemptBoot, &attemptEpoch); err != nil {
		return GrantPayload{}, err
	}
	if state != "Assigned" && state != "Running" {
		return GrantPayload{}, fmt.Errorf("%w: attempt %d is terminal", ErrGrantDenied, attemptID)
	}
	if attemptBoot.Valid && attemptBoot.String != bootID {
		return GrantPayload{}, fmt.Errorf("%w: boot fence", ErrGrantDenied)
	}
	if attemptEpoch.Valid && attemptEpoch.Int64 != int64(epoch) {
		return GrantPayload{}, fmt.Errorf("%w: epoch fence", ErrGrantDenied)
	}
	// Probe attempts scope on the connection itself; agent attempts bind
	// the connection through the grant row created inside the tool call
	// persistence transaction (ARCH-INPUT-003), so the scope fence only
	// applies to the probe closure.
	if attemptType == "connection_probe" && scopeID != connectionID {
		return GrantPayload{}, fmt.Errorf("%w: grant does not belong to the attempt's connection", ErrGrantDenied)
	}
	var connectionType string
	if err := conn.QueryRowContext(ctx, `SELECT c.type FROM credential_generations g JOIN connections c ON c.id=g.connection_id WHERE g.id=? AND g.connection_id=?`, generationID, connectionID).Scan(&connectionType); err != nil {
		return GrantPayload{}, err
	}
	var revisionConfig sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT config_json FROM connection_revisions WHERE id=? AND connection_id=?`, revisionID, connectionID).Scan(&revisionConfig); err != nil {
		return GrantPayload{}, err
	}
	payload := GrantPayload{
		GrantID:              grantID,
		AttemptID:            attemptID,
		ConnectionRevisionID: revisionID,
		CredentialGeneration: generationID,
		ConnectionType:       connectionType,
		RevisionConfigJSON:   json.RawMessage("null"),
	}
	if revisionConfig.Valid && revisionConfig.String != "" {
		payload.RevisionConfigJSON = json.RawMessage(revisionConfig.String)
	}
	// Config Verification freezes a grant for reproducibility, but must not
	// execute a grant invalidated by a committed disable/rotation/rebind.
	// Re-read its currentness in this same write transaction before decrypting.
	if purpose == "config_thanos_query" {
		if err := thanos.ValidateConfigGrantForExecution(ctx, conn, attemptID); err != nil {
			return GrantPayload{}, fmt.Errorf("%w: %v", ErrGrantDenied, err)
		}
	}
	if purpose == kubernetes.ReadPurpose {
		if err := kubernetes.ValidateGrantForFulfillment(ctx, conn, attemptID, grantID); err != nil {
			return GrantPayload{}, fmt.Errorf("%w: %v", ErrGrantDenied, err)
		}
	}
	// Decryption happens inside the same fenced transaction so a terminal
	// commit racing this read is still ordered (SQLite single writer).
	secret, err := service.openGenerationOn(ctx, conn, generationID)
	if err != nil {
		return GrantPayload{}, err
	}
	switch {
	case secret.Prometheus != nil:
		payload.Metrics = &MetricsCredentialSecret{Username: secret.Prometheus.Username, Password: secret.Prometheus.Password, BearerToken: secret.Prometheus.BearerToken}
	case secret.Thanos != nil:
		payload.Metrics = &MetricsCredentialSecret{Username: secret.Thanos.Username, Password: secret.Thanos.Password, BearerToken: secret.Thanos.BearerToken}
		payload.Thanos = payload.Metrics
	case secret.Kubernetes != nil:
		payload.Kubernetes = &KubernetesCredentialSecret{Kubeconfig: secret.Kubernetes.Kubeconfig}
	case secret.ModelProvider != nil:
		payload.ModelProvider = &ModelProviderCredentialSecret{APIKey: secret.ModelProvider.APIKey}
	case connectionType == TypePrometheus || connectionType == TypeThanos:
		// Metrics endpoints may run without auth. The explicit empty carrier
		// is still a valid grant so every connection retains an independent
		// credential generation and dispatch snapshot.
		payload.Metrics = &MetricsCredentialSecret{}
	default:
		return GrantPayload{}, fmt.Errorf("sealed secret carries no typed variant for %q", connectionType)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return GrantPayload{}, err
	}
	committed = true
	return payload, nil
}

// CancelProbe commits the cancellation fence for one Running probe attempt:
// the cancelled typed result is inserted while the attempt is still Running
// (the result-closure trigger requires it), then the same transaction moves
// the attempt to Cancelling (RUNTIME-CANCEL-001/002: late runtime results
// are rejected afterwards because results close only over Running).
// Cancellation is available only once the attempt is Running; queued or
// assigned probes dispatch first (the frozen state machine admits no other
// cancelled closure for connection_probe).
func (service *Service) CancelProbe(ctx context.Context, attemptID int64, expectedRow int64) error {
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
	var scopeID int64
	var state string
	var rowVersion int64
	if err := conn.QueryRowContext(ctx, `SELECT scope_id,state,row_version FROM execution_attempts WHERE id=?`, attemptID).Scan(&scopeID, &state, &rowVersion); err != nil {
		return err
	}
	if rowVersion != expectedRow {
		return &RowVersionError{ID: attemptID, Current: rowVersion}
	}
	if state != "Running" {
		return ErrActiveConflict
	}
	// The cancelled closure binds the pair the attempt's grant froze —
	// NOT the connection's current pointers: a rotation committed while the
	// probe was in flight must not break the cancellation closure (T09).
	var connectionType string
	var revisionID, generationID int64
	if err := conn.QueryRowContext(ctx, `
		SELECT c.type, ag.connection_revision_id, ag.credential_generation_id
		FROM connections c
		JOIN attempt_connection_grants ag ON ag.attempt_id=? AND ag.connection_id=c.id
		ORDER BY ag.id LIMIT 1`, attemptID).Scan(&connectionType, &revisionID, &generationID); err != nil {
		return err
	}
	var bindingRevision int
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
		attemptID, scopeID, connectionType, revisionID, generationID, bindingRevision, actionSetID, actionSetVersion, contractDigest, "cancelled", cancelDigest(attemptID), now, now, now)
	if err != nil {
		return err
	}
	headerID, err := headerInsert.LastInsertId()
	if err != nil {
		return err
	}
	if err := writeCancelledChild(ctx, conn, headerID, connectionType, revisionID); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE execution_attempts SET state='Cancelling',row_version=row_version+1 WHERE id=? AND state='Running'`, attemptID); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// InterruptProbe closes a Running probe with its immutable interrupted typed
// result before advancing the Attempt terminal state. Generic Attempt
// interruption cannot be used here because the SQL terminal fence requires
// the typed result to exist first; restart/reconciliation therefore uses this
// dedicated closure path.
func (service *Service) InterruptProbe(ctx context.Context, attemptID int64, reason string) error {
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
	if err := conn.QueryRowContext(ctx, `SELECT state,scope_id FROM execution_attempts WHERE id=?`, attemptID).Scan(&state, &scopeID); err != nil {
		return err
	}
	if state == "Interrupted" || state == "Succeeded" || state == "Failed" || state == "Cancelled" {
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return err
		}
		committed = true
		return nil
	}
	if state != "Running" {
		return fmt.Errorf("connection probe %d is %s; interrupted closure requires Running", attemptID, state)
	}
	var connectionType string
	var revisionID, generationID int64
	if err := conn.QueryRowContext(ctx, `SELECT c.type,g.connection_revision_id,g.credential_generation_id FROM connections c JOIN attempt_connection_grants g ON g.connection_id=c.id WHERE g.attempt_id=? ORDER BY g.id LIMIT 1`, attemptID).Scan(&connectionType, &revisionID, &generationID); err != nil {
		return err
	}
	var bindingRevision int
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
	insert, err := conn.ExecContext(ctx, `INSERT INTO connection_probe_results(attempt_id,connection_id,connection_type,connection_revision_id,credential_generation_id,root_binding_revision,action_set_id,action_set_version,probe_contract_digest,outcome,result_digest,started_at,finished_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, attemptID, scopeID, connectionType, revisionID, generationID, bindingRevision, actionSetID, actionSetVersion, contractDigest, "interrupted", interruptionDigest(attemptID, reason), now, now, now)
	if err != nil {
		return err
	}
	headerID, err := insert.LastInsertId()
	if err != nil {
		return err
	}
	if err := writeInterruptedChild(ctx, conn, headerID, connectionType, revisionID); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE execution_attempts SET state='Interrupted',ended_at=?,termination_reason=?,row_version=row_version+1 WHERE id=? AND state='Running'`, now, reason, attemptID); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// RecordCancelAck finalizes Cancelling -> Cancelled once the runtime
// confirms the attempt stopped (RUNTIME-CANCEL-003); the cancelled result
// already exists from the fence transaction.
func (service *Service) RecordCancelAck(ctx context.Context, attemptID int64) error {
	result, err := service.db.ExecContext(ctx, `UPDATE execution_attempts SET state='Cancelled',ended_at=?,termination_reason='cancelled',row_version=row_version+1 WHERE id=? AND state='Cancelling'`, service.now().UTC().Format(time.RFC3339Nano), attemptID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return fmt.Errorf("attempt %d is not in Cancelling state", attemptID)
	}
	return nil
}

// cancelDigest derives the deterministic digest of a cancelled closure.
func cancelDigest(attemptID int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("cancelled:%d", attemptID)))
	return hex.EncodeToString(sum[:])
}

func interruptionDigest(attemptID int64, reason string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("interrupted:%d:%s", attemptID, reason)))
	return hex.EncodeToString(sum[:])
}

// writeCancelledChild persists the frozen action-set shaped child row for a
// cancelled probe (values are the contract constants; outcome carries the
// cancellation semantics).
func writeInterruptedChild(ctx context.Context, conn *sql.Conn, headerID int64, connectionType string, connectionID int64) error {
	return writeTerminalProbeChild(ctx, conn, headerID, connectionType, connectionID, "interrupted")
}

func writeCancelledChild(ctx context.Context, conn *sql.Conn, headerID int64, connectionType string, connectionID int64) error {
	return writeTerminalProbeChild(ctx, conn, headerID, connectionType, connectionID, "cancelled")
}

// writeTerminalProbeChild preserves a closed typed child for a terminal probe
// that never produced an upstream observation. Its terminal outcome is
// explicit in detail_json; no successful capability fact is manufactured.
// revisionID is the header's frozen revision, which can differ from the
// connection's current pointer after an in-flight rotation.
func writeTerminalProbeChild(ctx context.Context, conn *sql.Conn, headerID int64, connectionType string, revisionID int64, terminal string) error {
	switch connectionType {
	case TypePrometheus, TypeThanos:
		_, err := conn.ExecContext(ctx, `INSERT INTO thanos_connection_probe_results(probe_result_id,query,response_type,sample_count,sample_value,detail_json) VALUES(?,?,?,?,?,?)`,
			headerID, "vector(1)", "vector", 1, "1", fmt.Sprintf(`{"kind":%q,%q:true}`, connectionType, terminal))
		return err
	case TypeKubernetes:
		effective := "default"
		var configJSON string
		if err := conn.QueryRowContext(ctx, `SELECT config_json FROM connection_revisions WHERE id=?`, revisionID).Scan(&configJSON); err == nil {
			var config struct {
				DefaultNamespace string `json:"defaultNamespace"`
			}
			if json.Unmarshal([]byte(configJSON), &config) == nil && config.DefaultNamespace != "" {
				effective = config.DefaultNamespace
			}
		}
		_, err := conn.ExecContext(ctx, `INSERT INTO kubernetes_connection_probe_results(probe_result_id,effective_namespace,version_ok,core_discovery_ok,grouped_discovery_ok,pods_get_allowed,pods_list_allowed,events_list_allowed,pods_log_get_allowed,detail_json) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			headerID, effective, 0, 0, 0, 0, 0, 0, 0, fmt.Sprintf(`{"kind":"kubernetes",%q:true}`, terminal))
		return err
	case TypeModelProvider:
		var configJSON string
		_ = conn.QueryRowContext(ctx, `SELECT config_json FROM connection_revisions WHERE id=?`, revisionID).Scan(&configJSON)
		var config struct {
			ChatModelID         string `json:"chatModelId"`
			EmbeddingModelID    string `json:"embeddingModelId"`
			ContextBudgetTokens int    `json:"contextBudgetTokens"`
			MaxOutputTokens     int    `json:"maxOutputTokens"`
		}
		_ = json.Unmarshal([]byte(configJSON), &config)
		_, err := conn.ExecContext(ctx, `INSERT INTO model_provider_connection_probe_results(probe_result_id,chat_model_id,embedding_model_id,context_budget_tokens,max_output_tokens,streaming_supported,native_tool_calling_supported,multi_tool_call_supported,cancellation_observed,usage_observed,request_id_observed,embedding_supported,embedding_vector_dim,detail_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			headerID, config.ChatModelID, nil, config.ContextBudgetTokens, config.MaxOutputTokens, 0, 0, 0, 0, 0, 0, 0, nil, fmt.Sprintf(`{"kind":"model_provider",%q:true}`, terminal))
		return err
	default:
		return fmt.Errorf("connection type %q has no supervisor probe child", connectionType)
	}
}

// QueuedProbeAttempts lists connection_probe attempts still waiting for a
// live Plinth stream (created while the slot was disconnected).
func (service *Service) QueuedProbeAttempts(ctx context.Context) ([]int64, error) {
	rows, err := service.db.QueryContext(ctx, `SELECT id FROM execution_attempts WHERE attempt_type='connection_probe' AND state='Queued' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// BindQueuedToStream moves one Queued probe to Assigned against the given
// live binding and returns the dispatch tuple; it is a no-op (ok=false)
// when another dispatcher won the race.
func (service *Service) BindQueuedToStream(ctx context.Context, attemptID int64, bootID string, epoch uint64, lease time.Duration) (Summary, int64, []byte, bool, error) {
	var scopeName string
	var contentDigest string
	err := service.db.QueryRowContext(ctx, `
		SELECT c.name, s.content_digest
		FROM execution_attempts a
		JOIN attempt_input_snapshots s ON s.attempt_id=a.id
		JOIN connections c ON c.id=a.scope_id
		WHERE a.id=? AND a.state='Queued' AND a.attempt_type='connection_probe'`, attemptID).Scan(&scopeName, &contentDigest)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Summary{}, 0, nil, false, nil
		}
		return Summary{}, 0, nil, false, err
	}
	summary, err := service.Get(ctx, scopeName)
	if err != nil {
		return Summary{}, 0, nil, false, err
	}
	// The canonical input is rebuilt deterministically and must still match
	// the frozen snapshot digest (input immutability, DATA-ATTEMPT-003).
	input, err := json.Marshal(ProbeInput{ConnectionName: scopeName})
	if err != nil {
		return Summary{}, 0, nil, false, err
	}
	rebuilt := sha256.Sum256(input)
	if hex.EncodeToString(rebuilt[:]) != contentDigest {
		return Summary{}, 0, nil, false, fmt.Errorf("input snapshot digest mismatch for attempt %d", attemptID)
	}
	var grantID int64
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return Summary{}, 0, nil, false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return Summary{}, 0, nil, false, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if err := conn.QueryRowContext(ctx, `SELECT id FROM attempt_connection_grants WHERE attempt_id=?`, attemptID).Scan(&grantID); err != nil {
		return Summary{}, 0, nil, false, err
	}
	// A competing dispatcher can change this attempt after the initial snapshot
	// read. Commit only if this transaction won the conditional Queued→Assigned
	// transition; otherwise return the documented no-op instead of dispatching a
	// grant for an attempt owned by another stream.
	updated, err := conn.ExecContext(ctx, `UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id=?,connection_epoch=?,lease_until=?,runtime_release_version=?,row_version=row_version+1 WHERE id=? AND state='Queued'`, bootID, epoch, service.now().UTC().Add(lease).Format(time.RFC3339Nano), releaseVersion, attemptID)
	if err != nil {
		return Summary{}, 0, nil, false, err
	}
	rows, err := updated.RowsAffected()
	if err != nil {
		return Summary{}, 0, nil, false, err
	}
	if rows != 1 {
		return Summary{}, 0, nil, false, nil
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return Summary{}, 0, nil, false, err
	}
	committed = true
	return summary, grantID, input, true, nil
}
