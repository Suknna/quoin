package modelprovider

// Model-call ledger for connection_probe qualification (T08): Begin opens
// one physical provider call as a running model_calls row bound to the
// probe attempt's chat/embedding grant; Complete seals usage and the
// canonical output (or the failure) with the frozen CHECK contract. The
// probe's fixed prompts are frozen profiles, so the renderer/agent versions
// are constants of this executor generation.
//
// Every ledger mutation runs through the shared execution runner (ADR-0006)
// so the automatic audit row commits in the same transaction as the ledger
// change; no raw transaction is opened here. The attempt's PERSISTED
// association is the correlation authority: an unwired caller (the normal
// supervisor stream after the originating request ended) is re-rooted onto
// the restored correlation and original initiator with the system task
// actor, an inherited context is accepted only for the system principal
// whose correlation matches the attempt's persisted one, and anything else
// is rejected instead of passed through blindly. Attempt creation itself is
// owned by the attempt lifecycle (attempt.CreateOn) — this package never
// creates attempts and never nests transactions.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// ProbeRendererVersion identifies the frozen probe prompt renderer.
const ProbeRendererVersion = "connection-probe-v1"

// ProbeAgentVersion identifies the deterministic supervisor executor.
const ProbeAgentVersion = "probe-supervisor-v1"

// ErrLedgerDenied reports fenced or invalid ledger writes. These fences are
// supervisor-protocol order rules (mirroring RUNTIME-AGENT-010's late-result
// handling): they surface as plain errors, so a replayed or out-of-order
// ledger step records nothing — the winning transition is the single audited
// fact.
var ErrLedgerDenied = errors.New("model call ledger denied")

// Stable operation identities of the model-call ledger.
const (
	opModelCallBegin    = "connection.model_call.begin"
	opModelCallLineage  = "connection.model_call.input_lineage"
	opModelCallComplete = "connection.model_call.complete"

	objectModelCall = "model_call"
)

// ledgerOperations holds the package's static operation declarations. The
// declarations are registered once; each execution builds its own runner over
// the caller's database pool on top of the shared registry.
type ledgerOperations struct {
	registry *execution.Registry
	begin    *execution.Operation
	lineage  *execution.Operation
	complete *execution.Operation
}

var ledgerCommands struct {
	sync.Once
	ops ledgerOperations
}

func ledgerOperationsOnce() ledgerOperations {
	ledgerCommands.Do(func() {
		registry := execution.NewRegistry()
		ops := ledgerOperations{registry: registry}
		for _, declaration := range []struct {
			target **execution.Operation
			name   string
		}{
			{&ops.begin, opModelCallBegin},
			{&ops.lineage, opModelCallLineage},
			{&ops.complete, opModelCallComplete},
		} {
			operation, err := registry.Register(execution.Operation{
				Name: declaration.name, Class: execution.ClassWrite,
				ObjectType: objectModelCall, Authorize: requireModelCallSystem,
			})
			if err != nil {
				panic(fmt.Sprintf("modelprovider: register operation %q: %v", declaration.name, err))
			}
			*declaration.target = operation
		}
		ledgerCommands.ops = ops
	})
	return ledgerCommands.ops
}

// requireModelCallSystem confines the supervisor-driven ledger writes to the
// explicit system task scope (never a client channel, no session fallback).
func requireModelCallSystem(ctx context.Context, _ *execution.Tx) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
		return fmt.Errorf("modelprovider: ledger operation requires the system principal, got %s/%d", meta.Actor.Kind, meta.Actor.ID)
	}
	if meta.Source.Kind == execution.SourceHTTP {
		return fmt.Errorf("modelprovider: ledger operation cannot arrive from the %s channel", meta.Source.Kind)
	}
	return nil
}

// attemptScope resolves the execution scope of one ledger step on the given
// attempt. It mirrors connections.probeLifecycleContext semantics — the
// model-provider ledger cannot import its parent package, so the seam is
// duplicated deliberately and must stay behaviorally identical: restore the
// persisted correlation for unwired callers, reject unrelated inherited
// metadata, keep unknown (pre-correlation) lineage visibly unknown with an
// explicit fresh system scope. The correlation read is pure and runs only on
// the composition-injected read-only reader, never on the write pool.
func attemptScope(ctx context.Context, reader audit.Reader, attemptID int64) (context.Context, error) {
	stored, found, err := attempt.LoadCorrelation(ctx, reader, attemptID)
	if errors.Is(err, attempt.ErrAttemptMissing) {
		return nil, fmt.Errorf("modelprovider: ledger attempt %d does not exist", attemptID)
	}
	if err != nil {
		return nil, err
	}
	if meta, wired := execution.FromContext(ctx); wired {
		if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 || meta.Source.Kind == execution.SourceHTTP {
			return nil, fmt.Errorf("modelprovider: ledger operation requires the system task principal, got %s/%d via %s", meta.Actor.Kind, meta.Actor.ID, meta.Source.Kind)
		}
		if found && stored.OperationCorrelationID != meta.CorrelationID {
			return nil, fmt.Errorf("modelprovider: inherited metadata carries unrelated correlation %q for attempt %d (persisted %q)", meta.CorrelationID, attemptID, stored.OperationCorrelationID)
		}
		return ctx, nil
	}
	if found && stored.OperationCorrelationID != "" &&
		((stored.InitiatorType == string(execution.PrincipalSystem) && stored.InitiatorID == 0) ||
			((stored.InitiatorType == string(execution.PrincipalUser) || stored.InitiatorType == string(execution.PrincipalService)) && stored.InitiatorID > 0)) {
		return execution.ReplaceMetadata(ctx, execution.Metadata{
			CorrelationID: stored.OperationCorrelationID,
			Actor:         execution.Principal{Kind: execution.PrincipalSystem},
			Initiator:     execution.Principal{Kind: execution.PrincipalKind(stored.InitiatorType), ID: stored.InitiatorID},
			Source:        execution.Source{Kind: execution.SourceTask},
		})
	}
	correlation, err := execution.NewCorrelationID()
	if err != nil {
		return nil, err
	}
	return execution.WithMetadata(ctx, execution.Metadata{
		CorrelationID: correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Source:        execution.Source{Kind: execution.SourceTask},
	})
}

// Begin opens one running model_calls row for the probe attempt as an
// audited mutation. Natural idempotence: the frozen (attempt, call_seq,
// retry_seq) identity makes a replayed Begin fail on its unique key with no
// durable record. reader is the composition's trusted read-only capability
// (execution.Reader, e.g. the parent service's Reader()): the step's own
// runner validates it — a raw pool is rejected — and it serves only the pure
// correlation read; the write pool stays the explicit db argument.
func Begin(ctx context.Context, db *sql.DB, reader audit.Reader, attemptID, grantID int64, callSeq, retrySeq int, operation, modelID string, promptDigest, toolSchemaDigest, inputDigest, renderedDigest string, contextBudget, maxOutput int64, estimatedInput int, evictedTurns int) (int64, error) {
	ops := ledgerOperationsOnce()
	runner := execution.NewRunner(db, ops.registry, audit.NewWriter())
	if err := runner.SetReader(reader); err != nil {
		return 0, err
	}
	ctx, err := attemptScope(ctx, runner.Reader(), attemptID)
	if err != nil {
		return 0, err
	}
	return execution.Execute(ctx, runner, ops.begin, func(tx *execution.Tx) (int64, error) {
		// The attempt must still be live and bound to this grant.
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&state); err != nil {
			return 0, err
		}
		if state != "Running" {
			return 0, fmt.Errorf("%w: attempt %d is %s", ErrLedgerDenied, attemptID, state)
		}
		var grantAttempt int64
		if err := tx.QueryRowContext(ctx, `SELECT attempt_id FROM attempt_connection_grants WHERE id=?`, grantID).Scan(&grantAttempt); err != nil {
			return 0, err
		}
		if grantAttempt != attemptID {
			return 0, fmt.Errorf("%w: grant belongs to attempt %d", ErrLedgerDenied, grantAttempt)
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		var insert sql.Result
		if operation == "chat" {
			insert, err = tx.ExecContext(ctx, `INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,estimated_input_tokens,evicted_turn_count,status,started_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?, 'running', ?)`,
				attemptID, callSeq, retrySeq, operation, modelID, grantID, ProbeRendererVersion, ProbeAgentVersion, promptDigest, toolSchemaDigest, toolSchemaDigest, inputDigest, renderedDigest, contextBudget, maxOutput, estimatedInput, evictedTurns, now)
		} else {
			insert, err = tx.ExecContext(ctx, `INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,input_snapshot_digest,rendered_request_digest,estimated_input_tokens,status,started_at) VALUES(?,?,?,?,?,?,?,?,?, 'running', ?)`,
				attemptID, callSeq, retrySeq, operation, modelID, grantID, inputDigest, renderedDigest, estimatedInput, now)
		}
		if err != nil {
			return 0, err
		}
		return insert.LastInsertId()
	}, func(callID int64) int64 { return callID })
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

// Complete seals one model call — status, usage and the canonical output in
// the same audited transaction (DATA-MODEL ledger closure). Natural
// idempotence: the guarded status='running' fence makes a replayed Complete
// fail with no durable record. reader follows the Begin contract: validated
// read-only scope for the correlation read only.
func Complete(ctx context.Context, db *sql.DB, reader audit.Reader, attemptID, callID int64, completion Completion) error {
	ops := ledgerOperationsOnce()
	runner := execution.NewRunner(db, ops.registry, audit.NewWriter())
	if err := runner.SetReader(reader); err != nil {
		return err
	}
	ctx, err := attemptScope(ctx, runner.Reader(), attemptID)
	if err != nil {
		return err
	}
	_, err = execution.Execute(ctx, runner, ops.complete, func(tx *execution.Tx) (int64, error) {
		var state string
		var ledgerAttempt int64
		if err := tx.QueryRowContext(ctx, `SELECT attempt_id,status FROM model_calls WHERE id=?`, callID).Scan(&ledgerAttempt, &state); err != nil {
			return 0, err
		}
		if ledgerAttempt != attemptID {
			return 0, fmt.Errorf("%w: call %d belongs to attempt %d", ErrLedgerDenied, callID, ledgerAttempt)
		}
		if state != "running" {
			return 0, fmt.Errorf("%w: call %d already %s", ErrLedgerDenied, callID, state)
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
			if _, err := tx.ExecContext(ctx, `INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at) VALUES(?,?,?,?,?,?)`,
				callID, complete, completion.ResponseJSON, completion.ResponseDigest, completion.FinishReason, now); err != nil {
				return 0, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE model_calls SET provider_request_id=?,usage_json=?,latency_ms=?,status='succeeded',ended_at=? WHERE id=? AND status='running'`,
				completion.ProviderRequestID, usage, completion.LatencyMS, now, callID); err != nil {
				return 0, err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE model_calls SET provider_request_id=?,latency_ms=?,status=?,termination_reason=?,ended_at=? WHERE id=? AND status='running'`,
				completion.ProviderRequestID, completion.LatencyMS, completion.Outcome, completion.FailureReason, now, callID); err != nil {
				return 0, err
			}
		}
		return callID, nil
	}, func(callID int64) int64 { return callID })
	return err
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
// embedding carries the attempt's input snapshot row. The insert is an
// audited mutation on the attempt's restored scope. reader follows the Begin
// contract: validated read-only scope for the correlation read only.
func WriteInputLineage(ctx context.Context, db *sql.DB, reader audit.Reader, callID int64, operation, promptDigest, toolDigest string, attemptID int64) error {
	ops := ledgerOperationsOnce()
	runner := execution.NewRunner(db, ops.registry, audit.NewWriter())
	if err := runner.SetReader(reader); err != nil {
		return err
	}
	ctx, err := attemptScope(ctx, runner.Reader(), attemptID)
	if err != nil {
		return err
	}
	_, err = execution.Execute(ctx, runner, ops.lineage, func(tx *execution.Tx) (int64, error) {
		if operation == "chat" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,1,'system',?,'system_contract')`, callID, promptDigest); err != nil {
				return 0, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,synthetic_kind) VALUES(?,2,'system',?,'tool_schema')`, callID, toolDigest); err != nil {
				return 0, err
			}
		} else {
			var snapshotID int64
			if err := tx.QueryRowContext(ctx, `SELECT id FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&snapshotID); err != nil {
				return 0, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,attempt_input_snapshot_id) VALUES(?,1,'user',?,?)`, callID, strings.Repeat("0", 64), snapshotID); err != nil {
				return 0, err
			}
		}
		return callID, nil
	}, func(callID int64) int64 { return callID })
	return err
}
