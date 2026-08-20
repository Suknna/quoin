package modelprovider

// Model-call ledger for connection_probe qualification (T08): Begin opens
// one physical provider call as a running model_calls row bound to the
// probe attempt's chat/embedding grant; Complete seals usage and the
// canonical output (or the failure) with the frozen CHECK contract. The
// probe's fixed prompts are frozen profiles, so the renderer/agent versions
// are constants of this executor generation.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ProbeRendererVersion identifies the frozen probe prompt renderer.
const ProbeRendererVersion = "connection-probe-v1"

// ProbeAgentVersion identifies the deterministic supervisor executor.
const ProbeAgentVersion = "probe-supervisor-v1"

// ErrLedgerDenied reports fenced or invalid ledger writes.
var ErrLedgerDenied = errors.New("model call ledger denied")

// Begin opens one running model_calls row for the probe attempt.
func Begin(ctx context.Context, db *sql.DB, attemptID, grantID int64, callSeq, retrySeq int, operation, modelID string, promptDigest, toolSchemaDigest, inputDigest, renderedDigest string, contextBudget, maxOutput int64, estimatedInput int, evictedTurns int) (int64, error) {
	conn, err := db.Conn(ctx)
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
	// The attempt must still be live and bound to this grant.
	var state string
	if err := conn.QueryRowContext(ctx, `SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&state); err != nil {
		return 0, err
	}
	if state != "Running" {
		return 0, fmt.Errorf("%w: attempt %d is %s", ErrLedgerDenied, attemptID, state)
	}
	var grantAttempt int64
	if err := conn.QueryRowContext(ctx, `SELECT attempt_id FROM attempt_connection_grants WHERE id=?`, grantID).Scan(&grantAttempt); err != nil {
		return 0, err
	}
	if grantAttempt != attemptID {
		return 0, fmt.Errorf("%w: grant belongs to attempt %d", ErrLedgerDenied, grantAttempt)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var insert interface{ LastInsertId() (int64, error) }
	if operation == "chat" {
		insert, err = conn.ExecContext(ctx, `INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,evicted_turn_count,status,started_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?, 'running', ?)`,
			attemptID, callSeq, retrySeq, operation, modelID, grantID, ProbeRendererVersion, ProbeAgentVersion, promptDigest, toolSchemaDigest, toolSchemaDigest, inputDigest, renderedDigest, contextBudget, maxOutput, estimatedInput, evictedTurns, now)
	} else {
		insert, err = conn.ExecContext(ctx, `INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,input_snapshot_digest,rendered_request_digest,estimated_input_tokens,status,started_at) VALUES(?,?,?,?,?,?,?,?,?, 'running', ?)`,
			attemptID, callSeq, retrySeq, operation, modelID, grantID, inputDigest, renderedDigest, estimatedInput, now)
	}
	if err != nil {
		return 0, err
	}
	callID, err := insert.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return 0, err
	}
	committed = true
	return callID, nil
}

// Completion is the sealed canonical result of one model call.
type Completion struct {
	Outcome           string // succeeded | failed | cancelled
	FailureReason     string // required unless succeeded
	ProviderRequestID string
	LatencyMS         int64
	InputTokens       int64
	OutputTokens      int64
	TotalTokens       int64
	FinishReason      string
	ResponseJSON      string // canonical response body (succeeded)
	ResponseDigest    string
	ResponseComplete  bool
}

// Complete seals one model call: status, usage and the canonical output in
// the same transaction (DATA-MODEL ledger closure).
func Complete(ctx context.Context, db *sql.DB, attemptID, callID int64, completion Completion) error {
	conn, err := db.Conn(ctx)
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
	var ledgerAttempt int64
	if err := conn.QueryRowContext(ctx, `SELECT attempt_id,status FROM model_calls WHERE id=?`, callID).Scan(&ledgerAttempt, &state); err != nil {
		return err
	}
	if ledgerAttempt != attemptID {
		return fmt.Errorf("%w: call %d belongs to attempt %d", ErrLedgerDenied, callID, ledgerAttempt)
	}
	if state != "running" {
		return fmt.Errorf("%w: call %d already %s", ErrLedgerDenied, callID, state)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	usage := fmt.Sprintf(`{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d}`, completion.InputTokens, completion.OutputTokens, completion.TotalTokens)
	if completion.Outcome == "succeeded" {
		// The sealed output must exist BEFORE the status moves to
		// succeeded (trg_model_call_success_output fires on the UPDATE).
		complete := 0
		if completion.ResponseComplete {
			complete = 1
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,?,?,?,?,?)`,
			callID, complete, completion.ResponseJSON, completion.ResponseDigest, completion.FinishReason, now); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE model_calls SET provider_request_id=?,usage_json=?,latency_ms=?,status='succeeded',ended_at=? WHERE id=? AND status='running'`,
			completion.ProviderRequestID, usage, completion.LatencyMS, now, callID); err != nil {
			return err
		}
	} else {
		if _, err := conn.ExecContext(ctx, `UPDATE model_calls SET provider_request_id=?,latency_ms=?,status=?,termination_reason=?,ended_at=? WHERE id=? AND status='running'`,
			completion.ProviderRequestID, completion.LatencyMS, completion.Outcome, completion.FailureReason, now, callID); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// DiscoveredModel is one /v1/models entry with its non-secret metadata.
type DiscoveredModel struct {
	ID       string
	Metadata map[string]any
}

// DiscoverUpstream lists the models of an OpenAI-compatible endpoint from
// the Quoin host side (input helper only — never a qualification signal;
// the API key exists only in request memory).
func DiscoverUpstream(ctx context.Context, baseURL, apiKey string) ([]DiscoveredModel, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(baseURL, "/")+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var listing struct {
		Data []struct {
			ID       string         `json:"id"`
			Metadata map[string]any `json:"metadata"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		return nil, err
	}
	models := make([]DiscoveredModel, 0, len(listing.Data))
	for _, entry := range listing.Data {
		models = append(models, DiscoveredModel{ID: entry.ID, Metadata: entry.Metadata})
	}
	return models, nil
}

// WriteInputLineage persists the frozen input items of one probe model
// call: chat carries the system-contract and tool-schema synthetics,
// embedding carries the attempt's input snapshot row.
func WriteInputLineage(ctx context.Context, db *sql.DB, callID int64, operation, promptDigest, toolDigest string, attemptID int64) error {
	conn, err := db.Conn(ctx)
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
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if operation == "chat" {
		if _, err := conn.ExecContext(ctx, `INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,1,'system',?,'system_contract')`, callID, promptDigest); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,2,'system',?,'tool_schema')`, callID, toolDigest); err != nil {
			return err
		}
	} else {
		var snapshotID int64
		if err := conn.QueryRowContext(ctx, `SELECT id FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&snapshotID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,attempt_input_snapshot_id) VALUES(?,1,'user',?,?)`, callID, strings.Repeat("0", 64), snapshotID); err != nil {
			return err
		}
	}
	_ = now
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}
