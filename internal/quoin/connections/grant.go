package connections

// Credential grant fulfillment (T07): FetchCredentialGrant is the only path
// by which a sealed connection secret leaves storage — decrypted per
// attempt, fenced on the grant/attempt/boot/epoch binding, and unavailable
// once the attempt reaches a terminal state (DATA-CONN-002). Cancellation
// follows the commit-order fence: a result proposal landing after a
// committed Cancelling fence is rejected (RUNTIME-CANCEL-002).
//
// The user-origin cancellation and the runtime-driven closure steps
// (interrupt, cancel ack, queued bind) are audited runner mutations; grant
// fulfillment stays a fenced read on the write pool — it performs no state
// change but needs the IMMEDIATE serialization so a terminal commit racing
// the read is still ordered (SQLite single writer).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/execution"
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
//
// The reveal runs through the family's execution runner (ADR-0006): the
// runner owns the IMMEDIATE transaction so the fenced read stays ordered
// against a racing terminal commit, and the automatic audit row records the
// sensitive grant reveal BEFORE any secret leaves storage (DATA-CONN-002).
// The returned payload carries the secrets only in memory — the runner
// persists no result payload for Execute operations, so nothing secret can
// enter the audit or any ledger. The execution scope is resolved through
// probeLifecycleContext: the attempt's persisted association is authoritative.
func (service *Service) FulfillGrant(ctx context.Context, grantID, attemptID int64, bootID string, epoch uint64) (GrantPayload, error) {
	// The grant row is the binding authority: the classic denial semantics
	// (unknown grant, foreign attempt) resolve before any scope is built, so
	// a denied reveal keeps its exact error and records nothing.
	var grantAttempt int64
	if err := service.reader.QueryRowContext(ctx, `SELECT attempt_id FROM attempt_connection_grants WHERE id=?`, grantID).Scan(&grantAttempt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return GrantPayload{}, ErrGrantDenied
		}
		return GrantPayload{}, err
	}
	if grantAttempt != attemptID {
		return GrantPayload{}, fmt.Errorf("%w: grant belongs to attempt %d", ErrGrantDenied, grantAttempt)
	}
	scope, err := service.probeLifecycleContext(ctx, grantAttempt)
	if err != nil {
		return GrantPayload{}, err
	}
	payload, err := execution.Execute(scope, service.commands.runner, service.commands.grantFulfill,
		func(tx *execution.Tx) (GrantPayload, error) {
			return service.fulfillGrantOn(scope, tx, grantID, attemptID, bootID, epoch)
		},
		func(GrantPayload) int64 { return grantID })
	if err != nil {
		return GrantPayload{}, err
	}
	return payload, nil
}

// fulfillGrantOn is FulfillGrant's fenced business stage on the runner-owned
// transaction. Every binding check runs here, before the decryption.
func (service *Service) fulfillGrantOn(ctx context.Context, tx *execution.Tx, grantID, attemptID int64, bootID string, epoch uint64) (GrantPayload, error) {
	conn := tx
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
	return payload, nil
}

// CancelProbe commits the cancellation fence for one Running probe attempt:
// the cancelled typed result is inserted while the attempt is still Running
// (the result-closure trigger requires it), then the same transaction moves
// the attempt to Cancelling (RUNTIME-CANCEL-001/002: late runtime results
// are rejected afterwards because results close only over Running).
// Cancellation is available only once the attempt is Running; queued or
// assigned probes dispatch first (the frozen state machine admits no other
// cancelled closure for connection_probe). The fence is an audited admin
// mutation under the attempt row-version fence.
func (service *Service) CancelProbe(ctx context.Context, attemptID int64, expectedRow int64) error {
	_, err := execution.Execute(ctx, service.commands.runner, service.commands.probeCancel, func(tx *execution.Tx) (int64, error) {
		var scopeID int64
		var state string
		var rowVersion int64
		if err := tx.QueryRowContext(ctx, `SELECT scope_id,state,row_version FROM execution_attempts WHERE id=?`, attemptID).Scan(&scopeID, &state, &rowVersion); err != nil {
			return 0, err
		}
		if rowVersion != expectedRow {
			return 0, &versionRejection{current: rowVersion, id: attemptID, rejection: execution.Rejection{Code: codeRowVersion, Detail: "attempt was modified concurrently", ObjectID: attemptID}}
		}
		if state != "Running" {
			return 0, rejectionOf(ErrActiveConflict, codeActiveConflict, "only a running probe attempt can be cancelled", attemptID)
		}
		// The cancelled closure binds the pair the attempt's grant froze —
		// NOT the connection's current pointers: a rotation committed while the
		// probe was in flight must not break the cancellation closure (T09).
		var connectionType string
		var revisionID, generationID int64
		if err := tx.QueryRowContext(ctx, `
			SELECT c.type, ag.connection_revision_id, ag.credential_generation_id
			FROM connections c
			JOIN attempt_connection_grants ag ON ag.attempt_id=? AND ag.connection_id=c.id
			ORDER BY ag.id LIMIT 1`, attemptID).Scan(&connectionType, &revisionID, &generationID); err != nil {
			return 0, err
		}
		var bindingRevision int
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
			attemptID, scopeID, connectionType, revisionID, generationID, bindingRevision, actionSetID, actionSetVersion, contractDigest, "cancelled", cancelDigest(attemptID), now, now, now)
		if err != nil {
			return 0, err
		}
		headerID, err := headerInsert.LastInsertId()
		if err != nil {
			return 0, err
		}
		if err := writeCancelledChild(ctx, tx, headerID, connectionType, revisionID); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE execution_attempts SET state='Cancelling',row_version=row_version+1 WHERE id=? AND state='Running'`, attemptID); err != nil {
			return 0, err
		}
		return attemptID, nil
	}, identity)
	if err != nil {
		return domainError(err)
	}
	return nil
}

// errProbeAlreadyTerminal marks an interrupt pass whose attempt already
// closed: the winning closure is the single audited fact and the loser
// records nothing (the caller sees a converged idempotent success).
var errProbeAlreadyTerminal = errors.New("connection probe attempt already terminal")

// InterruptProbe closes a probe with its immutable interrupted typed result
// before advancing the Attempt terminal state. Generic Attempt interruption
// cannot be used here because the SQL terminal fence requires the typed
// result to exist first; restart/reconciliation therefore uses this dedicated
// audited closure path. An already-terminal attempt converges silently (no
// state change, no audit row). Assigned (dispatched but never accepted, e.g.
// a Plinth reject before AttemptAccept) converges the same way: the typed
// interrupted result records that no observation ever ran.
func (service *Service) InterruptProbe(ctx context.Context, attemptID int64, reason string) error {
	ctx, err := service.probeLifecycleContext(ctx, attemptID)
	if err != nil {
		return err
	}
	_, err = execution.Execute(ctx, service.commands.runner, service.commands.probeInterrupt, func(tx *execution.Tx) (int64, error) {
		var state string
		var scopeID int64
		if err := tx.QueryRowContext(ctx, `SELECT state,scope_id FROM execution_attempts WHERE id=?`, attemptID).Scan(&state, &scopeID); err != nil {
			return 0, err
		}
		if state == "Interrupted" || state == "Succeeded" || state == "Failed" || state == "Cancelled" {
			return 0, errProbeAlreadyTerminal
		}
		if state == "Assigned" {
			// 已派发但从未被 Accept（如 Plinth 在 Accept 前拒绝）：没有任何
			// 观察发生，typed 结果无可封存——终态行与原因就是全部事实记录
			//（schema 对 Assigned → Interrupted 豁免 typed 结果闭包）。
			if _, err := tx.ExecContext(ctx, `UPDATE execution_attempts SET state='Interrupted',ended_at=?,termination_reason=?,row_version=row_version+1 WHERE id=? AND state='Assigned'`, timestampOf(service.now), reason, attemptID); err != nil {
				return 0, err
			}
			return attemptID, nil
		}
		if state != "Running" {
			return 0, fmt.Errorf("connection probe %d is %s; interrupted closure requires Running or Assigned", attemptID, state)
		}
		var connectionType string
		var revisionID, generationID int64
		if err := tx.QueryRowContext(ctx, `SELECT c.type,g.connection_revision_id,g.credential_generation_id FROM connections c JOIN attempt_connection_grants g ON g.connection_id=c.id WHERE g.attempt_id=? ORDER BY g.id LIMIT 1`, attemptID).Scan(&connectionType, &revisionID, &generationID); err != nil {
			return 0, err
		}
		var bindingRevision int
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
		insert, err := tx.ExecContext(ctx, `INSERT INTO connection_probe_results(attempt_id,connection_id,connection_type,connection_revision_id,credential_generation_id,root_binding_revision,action_set_id,action_set_version,probe_contract_digest,outcome,result_digest,started_at,finished_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, attemptID, scopeID, connectionType, revisionID, generationID, bindingRevision, actionSetID, actionSetVersion, contractDigest, "interrupted", interruptionDigest(attemptID, reason), now, now, now)
		if err != nil {
			return 0, err
		}
		headerID, err := insert.LastInsertId()
		if err != nil {
			return 0, err
		}
		if err := writeInterruptedChild(ctx, tx, headerID, connectionType, revisionID); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE execution_attempts SET state='Interrupted',ended_at=?,termination_reason=?,row_version=row_version+1 WHERE id=? AND state=?`, now, reason, attemptID, state); err != nil {
			return 0, err
		}
		return attemptID, nil
	}, identity)
	if errors.Is(err, errProbeAlreadyTerminal) {
		return nil
	}
	return err
}

// RecordCancelAck finalizes Cancelling -> Cancelled once the runtime
// confirms the attempt stopped (RUNTIME-CANCEL-003); the cancelled result
// already exists from the fence transaction. The finalization is an audited
// system mutation.
func (service *Service) RecordCancelAck(ctx context.Context, attemptID int64) error {
	ctx, err := service.probeLifecycleContext(ctx, attemptID)
	if err != nil {
		return err
	}
	_, err = execution.Execute(ctx, service.commands.runner, service.commands.probeCancelAck, func(tx *execution.Tx) (int64, error) {
		result, err := tx.ExecContext(ctx, `UPDATE execution_attempts SET state='Cancelled',ended_at=?,termination_reason='cancelled',row_version=row_version+1 WHERE id=? AND state='Cancelling'`, timestampOf(service.now), attemptID)
		if err != nil {
			return 0, err
		}
		rows, _ := result.RowsAffected()
		if rows != 1 {
			return 0, fmt.Errorf("attempt %d is not in Cancelling state", attemptID)
		}
		return attemptID, nil
	}, identity)
	return err
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

// writeInterruptedChild persists the frozen action-set shaped child row for
// an interrupted probe (values are the contract constants; outcome carries
// the interruption semantics).
func writeInterruptedChild(ctx context.Context, tx execution.Executor, headerID int64, connectionType string, connectionID int64) error {
	return writeTerminalProbeChild(ctx, tx, headerID, connectionType, connectionID, "interrupted")
}

func writeCancelledChild(ctx context.Context, tx execution.Executor, headerID int64, connectionType string, connectionID int64) error {
	return writeTerminalProbeChild(ctx, tx, headerID, connectionType, connectionID, "cancelled")
}

// writeTerminalProbeChild preserves a closed typed child for a terminal probe
// that never produced an upstream observation. Its terminal outcome is
// explicit in detail_json; no successful capability fact is manufactured.
// revisionID is the header's frozen revision, which can differ from the
// connection's current pointer after an in-flight rotation.
func writeTerminalProbeChild(ctx context.Context, tx execution.Executor, headerID int64, connectionType string, revisionID int64, terminal string) error {
	switch connectionType {
	case TypePrometheus, TypeThanos:
		_, err := tx.ExecContext(ctx, `INSERT INTO thanos_connection_probe_results(probe_result_id,query,response_type,sample_count,sample_value,detail_json) VALUES(?,?,?,?,?,?)`,
			headerID, "vector(1)", "vector", 1, "1", fmt.Sprintf(`{"kind":%q,%q:true}`, connectionType, terminal))
		return err
	case TypeModelProvider:
		var configJSON string
		_ = tx.QueryRowContext(ctx, `SELECT config_json FROM connection_revisions WHERE id=?`, revisionID).Scan(&configJSON)
		var config struct {
			ChatModelID         string `json:"chatModelId"`
			EmbeddingModelID    string `json:"embeddingModelId"`
			ContextBudgetTokens int    `json:"contextBudgetTokens"`
			MaxOutputTokens     int    `json:"maxOutputTokens"`
		}
		_ = json.Unmarshal([]byte(configJSON), &config)
		_, err := tx.ExecContext(ctx, `INSERT INTO model_provider_connection_probe_results(probe_result_id,chat_model_id,embedding_model_id,context_budget_tokens,max_output_tokens,streaming_supported,native_tool_calling_supported,multi_tool_call_supported,cancellation_observed,usage_observed,request_id_observed,embedding_supported,embedding_vector_dim,detail_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			headerID, config.ChatModelID, nil, config.ContextBudgetTokens, config.MaxOutputTokens, 0, 0, 0, 0, 0, 0, 0, nil, fmt.Sprintf(`{"kind":"model_provider",%q:true}`, terminal))
		return err
	default:
		return fmt.Errorf("connection type %q has no supervisor probe child", connectionType)
	}
}

// QueuedProbeAttempts lists connection_probe attempts still waiting for a
// live Plinth stream (created while the slot was disconnected).
func (service *Service) QueuedProbeAttempts(ctx context.Context) ([]int64, error) {
	rows, err := service.reader.QueryContext(ctx, `SELECT id FROM execution_attempts WHERE attempt_type='connection_probe' AND state='Queued' ORDER BY id`)
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

// errProbeBindSuperseded marks a dispatcher that lost the conditional
// Queued→Assigned race: the winner's transition is the single audited fact
// and the loser records nothing.
var errProbeBindSuperseded = errors.New("probe bind was superseded by another dispatcher")

// bindResult carries the dispatch tuple out of the audited bind transaction.
type bindResult struct {
	summary Summary
	grantID int64
	input   []byte
}

// BindQueuedToStream moves one Queued probe to Assigned against the given
// live binding and returns the dispatch tuple; it is a no-op (ok=false)
// when another dispatcher won the race. The bind is an audited system
// mutation guarded by the frozen snapshot digest.
func (service *Service) BindQueuedToStream(ctx context.Context, attemptID int64, bootID string, epoch uint64, lease time.Duration) (Summary, int64, []byte, bool, error) {
	var scopeName string
	var contentDigest string
	err := service.reader.QueryRowContext(ctx, `
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
	ctx, err = service.probeLifecycleContext(ctx, attemptID)
	if err != nil {
		return Summary{}, 0, nil, false, err
	}
	result, err := execution.Execute(ctx, service.commands.runner, service.commands.probeBind, func(tx *execution.Tx) (bindResult, error) {
		var grantID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM attempt_connection_grants WHERE attempt_id=?`, attemptID).Scan(&grantID); err != nil {
			return bindResult{}, err
		}
		// A competing dispatcher can change this attempt after the initial
		// snapshot read. Commit only if this transaction won the conditional
		// Queued→Assigned transition; otherwise the documented no-op wins over
		// dispatching a grant for an attempt owned by another stream.
		updated, err := tx.ExecContext(ctx, `UPDATE execution_attempts SET state='Assigned',runtime_slot='plinth',boot_id=?,connection_epoch=?,lease_until=?,runtime_release_version=?,row_version=row_version+1 WHERE id=? AND state='Queued'`, bootID, epoch, timestampOf(func() time.Time { return service.now().UTC().Add(lease) }), releaseVersion, attemptID)
		if err != nil {
			return bindResult{}, err
		}
		rows, err := updated.RowsAffected()
		if err != nil {
			return bindResult{}, err
		}
		if rows != 1 {
			return bindResult{}, errProbeBindSuperseded
		}
		return bindResult{summary: summary, grantID: grantID, input: input}, nil
	}, func(bindResult) int64 { return attemptID })
	if errors.Is(err, errProbeBindSuperseded) {
		return Summary{}, 0, nil, false, nil
	}
	if err != nil {
		return Summary{}, 0, nil, false, err
	}
	return result.summary, result.grantID, result.input, true, nil
}
