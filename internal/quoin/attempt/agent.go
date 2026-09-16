package attempt

// Agent model-call and tool-call ledger (ARCH-AGENT-005/006,
// ARCH-TOOL-001..004): every physical provider call and every tool
// execution exists as a durable row before the runtime may proceed. The
// probe-specific ledger in internal/quoin/connections/modelprovider stays
// untouched; agent attempts (initial_analysis and later modes) commit
// through this package.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// ErrLedgerDenied reports fenced or invalid agent ledger writes.
var ErrLedgerDenied = errors.New("agent ledger denied")

// ModelInputItem is one validated input lineage item of a model call.
type ModelInputItem struct {
	Sequence      uint32
	ItemKind      string // snapshot | message | prior_call | tool_call | evidence | artifact | knowledge | system_contract | tool_schema
	ItemID        int64
	ContentDigest string
	Role          string // system | user | assistant | tool
}

// BeginCall carries the BeginModelCall payload for a chat agent call.
type BeginCall struct {
	AttemptID        int64
	CallSeq          int
	RetrySeq         int
	ModelID          string
	PromptDigest     string
	ToolSchemaDigest string
	InputDigest      string
	RenderedDigest   string
	InputItems       []ModelInputItem
	ContextBudget    int64
	MaxOutput        int64
	EstimatedInput   int
	EvictedTurns     int
}

// BeginModelCall opens one running model_calls row after re-checking the
// frozen chat contract (model id, budgets, tool schema digest) against the
// attempt's qualified grant (ARCH-AGENT-003). Returns the durable call id.
//
// The standalone stage runs through the shared execution runner (ADR-0006):
// the runner owns the transaction and records the automatic audit fact on it,
// attributed to the system runtime authority on the attempt's persisted
// association. The two natural idempotence returns — the digest-identical
// replay and the lost-ack retry alias — changed nothing and record nothing,
// so their fast path resolves before the runner transaction opens; the same
// fences re-run inside the transaction for the race window.
func (service *Service) BeginModelCall(ctx context.Context, begin BeginCall) (int64, error) {
	if id, replayed, err := service.replayModelCallBeginFast(ctx, begin); replayed || err != nil {
		return id, err
	}
	authority, err := service.lifecycleAuthority(ctx, begin.AttemptID)
	if err != nil {
		return 0, err
	}
	return execution.Execute(authority, service.runner, service.opModelCallBegin,
		func(tx *execution.Tx) (int64, error) {
			return service.beginModelCallOn(authority, tx, begin)
		},
		func(callID int64) int64 { return callID })
}

// replayModelCallBeginFast resolves the two benign idempotence returns on
// the pool (read-only) so a replay neither opens a write transaction nor
// adds an audit fact. Anything that is not provably the benign replay —
// including every denial — falls through to the runner stage, which re-derives
// the authoritative decision inside its transaction.
func (service *Service) replayModelCallBeginFast(ctx context.Context, begin BeginCall) (int64, bool, error) {
	var state string
	if err := service.Reader().QueryRowContext(ctx, `SELECT state FROM execution_attempts WHERE id=?`, begin.AttemptID).Scan(&state); err != nil {
		return 0, false, err
	}
	if state != "Running" {
		return 0, false, nil
	}
	return modelCallBeginReplay(ctx, service.Reader(), begin)
}

// modelCallBeginReplay inspects the persisted model_calls rows for the two
// natural idempotence returns of BeginModelCall (RUNTIME-AGENT-005):
//
//   - digest-identical replay of the same physical call (a lost Begin ack
//     after a stream drop) returns the original row;
//   - the lost-ack retry alias (identical digests on retry_seq+1 whose
//     predecessor is still running) returns the live predecessor row.
//
// A divergent replay is a deterministic denial surfaced as the same
// ErrLedgerDenied the full stage produces (it records nothing either way).
// found=false with a nil error means "not a replay": the caller proceeds
// with its full validation.
func modelCallBeginReplay(ctx context.Context, queries audit.Reader, begin BeginCall) (int64, bool, error) {
	var existingID int64
	var existingPrompt, existingTools, existingRendered string
	err := queries.QueryRowContext(ctx, `
		SELECT id, prompt_digest, tool_schema_digest, rendered_request_digest
		FROM model_calls WHERE attempt_id=? AND call_seq=? AND retry_seq=?`,
		begin.AttemptID, begin.CallSeq, begin.RetrySeq).Scan(&existingID, &existingPrompt, &existingTools, &existingRendered)
	if err == nil {
		if existingPrompt != begin.PromptDigest || existingTools != begin.ToolSchemaDigest || existingRendered != begin.RenderedDigest {
			return 0, false, fmt.Errorf("%w: replay of call %d carries divergent digests", ErrLedgerDenied, existingID)
		}
		return existingID, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	if begin.RetrySeq > 0 {
		var prevID int64
		var previous, prevPrompt, prevTools, prevRendered string
		err = queries.QueryRowContext(ctx, `
			SELECT id,status,prompt_digest,tool_schema_digest,rendered_request_digest
			FROM model_calls WHERE attempt_id=? AND call_seq=? AND retry_seq=?`,
			begin.AttemptID, begin.CallSeq, begin.RetrySeq-1).Scan(&prevID, &previous, &prevPrompt, &prevTools, &prevRendered)
		if errors.Is(err, sql.ErrNoRows) || err != nil {
			return 0, false, nil // no predecessor: the full stage reports it
		}
		if previous == "running" &&
			prevPrompt == begin.PromptDigest && prevTools == begin.ToolSchemaDigest && prevRendered == begin.RenderedDigest {
			return prevID, true, nil
		}
	}
	return 0, false, nil
}

// beginModelCallOn is BeginModelCall's business stage on the runner-owned
// transaction. Every check from the contract to the retry chain re-runs
// here: the fast replay path only short-circuits the benign case.
func (service *Service) beginModelCallOn(ctx context.Context, tx *execution.Tx, begin BeginCall) (int64, error) {
	var state, agentVersion string
	if err := tx.QueryRowContext(ctx, `SELECT state,agent_version FROM execution_attempts WHERE id=?`, begin.AttemptID).Scan(&state, &agentVersion); err != nil {
		return 0, err
	}
	if state != "Running" {
		return 0, fmt.Errorf("%w: attempt %d is %s", ErrLedgerDenied, begin.AttemptID, state)
	}
	// The attempt's chat grant must exist and its qualified probe result
	// must carry the exact contract the worker claims (ARCH-AGENT-003).
	// The lookup runs on the runner's transaction connection: the production
	// pool is single-connection (SQLite writer serialization) and a pool
	// query here would self-deadlock against the open transaction.
	modelID, contextBudget, maxOutput, err := service.lookupChatContractOn(ctx, tx, begin.AttemptID)
	if err != nil {
		return 0, fmt.Errorf("%w: chat contract lookup: %v", ErrLedgerDenied, err)
	}
	if begin.ModelID != modelID {
		return 0, fmt.Errorf("%w: model id %q does not match the attempt's qualified model %q", ErrLedgerDenied, begin.ModelID, modelID)
	}
	if begin.ContextBudget != contextBudget || begin.MaxOutput != maxOutput {
		return 0, fmt.Errorf("%w: budget override refused (contract %d/%d, request %d/%d)", ErrLedgerDenied, contextBudget, maxOutput, begin.ContextBudget, begin.MaxOutput)
	}
	// The offered catalog is THIS attempt's frozen document (ADR-0004):
	// recovery re-renders the original bytes, so enablement changes never
	// drift a historical active attempt's digest.
	catalog, err := frozenToolCatalogOn(ctx, tx, begin.AttemptID)
	if err != nil {
		return 0, fmt.Errorf("%w: frozen tool catalog: %v", ErrLedgerDenied, err)
	}
	wantToolDigest, err := catalog.Digest()
	if err != nil {
		return 0, err
	}
	if begin.ToolSchemaDigest != wantToolDigest {
		return 0, fmt.Errorf("%w: tool schema digest mismatch (worker renders %s, frozen catalog %s)", ErrLedgerDenied, begin.ToolSchemaDigest, wantToolDigest)
	}
	var grantID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM attempt_connection_grants WHERE attempt_id=? AND purpose='chat_model' ORDER BY id LIMIT 1`, begin.AttemptID).Scan(&grantID); err != nil {
		return 0, fmt.Errorf("%w: chat_model grant missing: %v", ErrLedgerDenied, err)
	}
	// Replay of the same physical call (a lost BeginModelCallAck after a
	// stream drop) must return the original row instead of rejecting:
	// the frozen digests prove it is the same request (RUNTIME-AGENT-005
	// idempotent ledger; a divergent resend conflicts). The re-check runs on
	// the transaction for the race window against a concurrent begin.
	existingID, replayed, err := modelCallBeginReplay(ctx, tx, begin)
	if err != nil || replayed {
		return existingID, err
	}
	if begin.RetrySeq > 0 {
		var previous string
		err = tx.QueryRowContext(ctx, `
			SELECT status FROM model_calls WHERE attempt_id=? AND call_seq=? AND retry_seq=?`,
			begin.AttemptID, begin.CallSeq, begin.RetrySeq-1).Scan(&previous)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("%w: retry_seq %d has no failed predecessor", ErrLedgerDenied, begin.RetrySeq)
		}
		if err != nil {
			return 0, err
		}
		// A running predecessor with identical digests was already returned
		// by the replay check above; anything but a sealed failure may not
		// be retried.
		if previous != "failed" {
			return 0, fmt.Errorf("%w: retry_seq %d predecessor is %q", ErrLedgerDenied, begin.RetrySeq, previous)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	insert, err := tx.ExecContext(ctx, `
		INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,
			prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,
			input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,
			estimated_input_tokens,evicted_turn_count,status,started_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?, 'running', ?)`,
		begin.AttemptID, begin.CallSeq, begin.RetrySeq, "chat", begin.ModelID, grantID,
		promptRendererVersionFor(agentVersion), agentVersion, begin.PromptDigest, catalog.SchemaVersion, begin.ToolSchemaDigest,
		begin.InputDigest, begin.RenderedDigest, begin.ContextBudget, begin.MaxOutput,
		begin.EstimatedInput, begin.EvictedTurns, now)
	if err != nil {
		return 0, err
	}
	callID, err := insert.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := service.writeCallInputItems(ctx, tx, callID, begin); err != nil {
		return 0, err
	}
	return callID, nil
}

// writeCallInputItems validates and persists the ordered input lineage
// (ARCH-CONTEXT-006, trg_model_call_success_input).
func (service *Service) writeCallInputItems(ctx context.Context, conn execution.Executor, callID int64, begin BeginCall) error {
	if len(begin.InputItems) == 0 {
		return fmt.Errorf("%w: model call input items are empty", ErrLedgerDenied)
	}
	seq := uint32(0)
	for _, item := range begin.InputItems {
		seq++
		if item.Sequence != seq {
			return fmt.Errorf("%w: input item sequence must be contiguous from 1 (got %d at %d)", ErrLedgerDenied, item.Sequence, seq)
		}
		var (
			snapshotID, messageID, priorCallID, toolCallID, evidenceID, artifactID, knowledgeID sql.NullInt64
			synthetic                                                                           sql.NullString
		)
		role := item.Role
		switch item.ItemKind {
		case "snapshot":
			if role != "system" {
				return fmt.Errorf("%w: chat snapshot item role must be system", ErrLedgerDenied)
			}
			if item.ItemID == 0 {
				// The worker cannot know the snapshot row id (Quoin owns the
				// row); resolve it from the attempt and verify the digest.
				var storedDigest string
				if err := conn.QueryRowContext(ctx, `SELECT content_digest FROM attempt_input_snapshots WHERE attempt_id=?`, begin.AttemptID).Scan(&storedDigest); err != nil {
					return fmt.Errorf("%w: attempt input snapshot missing: %v", ErrLedgerDenied, err)
				}
				if storedDigest != item.ContentDigest {
					return fmt.Errorf("%w: snapshot item digest mismatch (stored %s, worker %s)", ErrLedgerDenied, storedDigest, item.ContentDigest)
				}
				if err := conn.QueryRowContext(ctx, `SELECT id FROM attempt_input_snapshots WHERE attempt_id=?`, begin.AttemptID).Scan(&item.ItemID); err != nil {
					return fmt.Errorf("%w: attempt input snapshot missing: %v", ErrLedgerDenied, err)
				}
			}
			snapshotID = sql.NullInt64{Int64: item.ItemID, Valid: true}
		case "message":
			messageID = sql.NullInt64{Int64: item.ItemID, Valid: true}
		case "prior_call":
			if role != "assistant" {
				return fmt.Errorf("%w: prior model call role must be assistant", ErrLedgerDenied)
			}
			priorCallID = sql.NullInt64{Int64: item.ItemID, Valid: true}
		case "tool_call":
			if role != "assistant" && role != "tool" {
				return fmt.Errorf("%w: tool call item role must be assistant or tool", ErrLedgerDenied)
			}
			toolCallID = sql.NullInt64{Int64: item.ItemID, Valid: true}
		case "evidence":
			evidenceID = sql.NullInt64{Int64: item.ItemID, Valid: true}
		case "artifact":
			artifactID = sql.NullInt64{Int64: item.ItemID, Valid: true}
		case "knowledge":
			knowledgeID = sql.NullInt64{Int64: item.ItemID, Valid: true}
		case "system_contract", "tool_schema":
			if role != "system" {
				return fmt.Errorf("%w: synthetic item role must be system", ErrLedgerDenied)
			}
			synthetic = sql.NullString{String: item.ItemKind, Valid: true}
		default:
			return fmt.Errorf("%w: unknown input item kind %q", ErrLedgerDenied, item.ItemKind)
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO model_call_input_items(model_call_id,item_seq,item_role,source_digest,
				attempt_input_snapshot_id,investigation_message_id,prior_model_call_id,tool_call_id,
				evidence_id,artifact_id,knowledge_version_id,synthetic_kind)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			callID, item.Sequence, role, item.ContentDigest,
			snapshotID, messageID, priorCallID, toolCallID, evidenceID, artifactID, knowledgeID, synthetic); err != nil {
			return err
		}
	}
	return nil
}

// ProposedTool is one tool call proposed by a completed model response.
type ProposedTool struct {
	ProviderIndex      uint32
	ProviderToolCallID string
	ToolName           string
	ArgumentsJSON      []byte
	ArgumentsDigest    string
}

// ToolAuthorization is the CompleteModelCallAck authorization for one
// pending tool call (ARCH-TOOL-001). Grants are the non-secret connection
// bindings frozen for observation tools (ARCH-INPUT-003).
type ToolAuthorization struct {
	ToolCallID               int64
	ProviderIndex            uint32
	ProviderToolCallID       string
	FailureMode              string
	Grants                   []ToolGrant
	PreflightCode            string
	PreflightDetail          string
	ExecutionArgumentsJSON   []byte
	ExecutionArgumentsDigest []byte
}

// CompleteCall seals one model call and creates the pending tool_calls rows
// for the proposed calls in the same transaction.
type CompleteCall struct {
	AttemptID         int64
	CallID            int64
	Outcome           string // succeeded | failed | cancelled
	FailureReason     string
	ProviderRequestID string
	LatencyMS         int64
	InputTokens       int64
	OutputTokens      int64
	TotalTokens       int64
	FinishReason      string
	AssistantText     string
	ProposedTools     []ProposedTool
	ResponseDigest    string
	ResponseComplete  bool
}

// CanonicalChatResponse is the canonical JSON both sides digest for a
// completed chat response: the visible assistant text plus the ordered tool
// calls (ARCH-AGENT-007). The plinth model executor must render identical
// bytes (pinned by internal/plinth/model/canonical_test.go).
type CanonicalChatResponse struct {
	AssistantText string          `json:"assistantText"`
	ToolCalls     []CanonicalTool `json:"toolCalls"`
}

// CanonicalTool is one proposed tool call inside the canonical response.
type CanonicalTool struct {
	ProviderToolCallID string `json:"providerToolCallId"`
	ToolName           string `json:"toolName"`
	Arguments          any    `json:"arguments"`
}

// CanonicalChatResponseJSON renders and digests the canonical response.
func CanonicalChatResponseJSON(assistantText string, tools []ProposedTool) (body []byte, digestHex string, err error) {
	canonical := CanonicalChatResponse{AssistantText: assistantText, ToolCalls: []CanonicalTool{}}
	for _, tool := range tools {
		var arguments any
		if err := json.Unmarshal(tool.ArgumentsJSON, &arguments); err != nil {
			return nil, "", fmt.Errorf("tool %s arguments unparseable for canonical response: %w", tool.ToolName, err)
		}
		canonical.ToolCalls = append(canonical.ToolCalls, CanonicalTool{
			ProviderToolCallID: tool.ProviderToolCallID,
			ToolName:           tool.ToolName,
			Arguments:          arguments,
		})
	}
	body, err = json.Marshal(canonical)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:]), nil
}

// CompleteModelCall seals the ledger row, stores the canonical output and
// creates the pending tool_calls rows, returning the authorizations for the
// Ack (ARCH-AGENT-006). The standalone stage runs through the shared
// execution runner (ADR-0006): the runner owns the transaction and records
// the automatic audit fact with the sealed outcome (succeeded seals as
// success; failed and cancelled seal as failure — see
// auditOutcomeForLedgerOutcome). The replay path (an already-sealed call
// whose Ack was lost) rebuilds the original Ack and records nothing, so it
// resolves before the runner transaction opens; the status fence re-runs
// inside for the race window.
func (service *Service) CompleteModelCall(ctx context.Context, completion CompleteCall) ([]ToolAuthorization, error) {
	var callAttempt int64
	var status string
	if err := service.Reader().QueryRowContext(ctx, `SELECT attempt_id,status FROM model_calls WHERE id=?`, completion.CallID).Scan(&callAttempt, &status); err != nil {
		return nil, err
	}
	if callAttempt != completion.AttemptID {
		return nil, fmt.Errorf("%w: call %d belongs to attempt %d", ErrLedgerDenied, completion.CallID, callAttempt)
	}
	if status != "running" {
		// Replay of an already-sealed physical call (a lost
		// CompleteModelCallAck after a stream drop) must rebuild the
		// original Ack instead of rejecting (RUNTIME-AGENT-005); a
		// divergent resend conflicts. The rebuild is read-only, records
		// nothing and runs before the runner transaction.
		return service.replayCompleteModelCall(ctx, service.Reader(), completion, status)
	}
	authority, err := service.lifecycleAuthority(ctx, completion.AttemptID)
	if err != nil {
		return nil, err
	}
	authorizations, err := execution.Execute(authority, service.runner, service.opModelCallComplete,
		func(tx *execution.Tx) ([]ToolAuthorization, error) {
			return service.completeModelCallOn(authority, tx, completion)
		},
		func([]ToolAuthorization) int64 { return completion.CallID })
	var recorded *execution.RecordedFailure
	if errors.As(err, &recorded) {
		// A failed or cancelled seal is a committed domain fact, not a stage
		// error: the runner recorded its failure audit and committed the
		// sealed state. The ack confirms the seal (RUNTIME-AGENT-005).
		return nil, nil
	}
	return authorizations, err
}

// completeModelCallOn is CompleteModelCall's business stage on the
// runner-owned transaction.
func (service *Service) completeModelCallOn(ctx context.Context, tx *execution.Tx, completion CompleteCall) ([]ToolAuthorization, error) {
	var callAttempt int64
	var status, agentVersion string
	var callSeq int
	if err := tx.QueryRowContext(ctx, `SELECT m.attempt_id,m.status,m.call_seq,a.agent_version FROM model_calls m JOIN execution_attempts a ON a.id=m.attempt_id WHERE m.id=?`, completion.CallID).Scan(&callAttempt, &status, &callSeq, &agentVersion); err != nil {
		return nil, err
	}
	if callAttempt != completion.AttemptID {
		return nil, fmt.Errorf("%w: call %d belongs to attempt %d", ErrLedgerDenied, completion.CallID, callAttempt)
	}
	if status != "running" {
		// Lost the race against a concurrent seal: rebuild the original Ack
		// on the transaction (RUNTIME-AGENT-005 replay).
		return service.replayCompleteModelCall(ctx, tx, completion, status)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var authorizations []ToolAuthorization
	if completion.Outcome == "succeeded" {
		if !completion.ResponseComplete {
			return nil, fmt.Errorf("%w: partial output cannot seal a succeeded call", ErrLedgerDenied)
		}
		_, wantDigest, err := CanonicalChatResponseJSON(completion.AssistantText, completion.ProposedTools)
		if err != nil {
			return nil, err
		}
		if completion.ResponseDigest != wantDigest {
			return nil, fmt.Errorf("%w: response digest mismatch (runtime %s, rebuilt %s)", ErrLedgerDenied, completion.ResponseDigest, wantDigest)
		}
		// Validate every proposed tool BEFORE any write; the frozen tool
		// catalog is the only authority for name/mode/arguments.
		// The frozen stored shape is snake_case (trg_model_call_output_shape
		// reads '$.tool_calls'); the canonical digest document is separate
		// and keeps camelCase (CanonicalChatResponseJSON).
		output := map[string]any{"assistantText": completion.AssistantText, "finishReason": completion.FinishReason, "tool_calls": []any{}}
		type validatedTool struct {
			proposed   ProposedTool
			definition ToolDef
		}
		validated := make([]validatedTool, 0, len(completion.ProposedTools))
		catalog, err := frozenToolCatalogOn(ctx, tx, completion.AttemptID)
		if err != nil {
			return nil, fmt.Errorf("%w: frozen tool catalog: %v", ErrLedgerDenied, err)
		}
		for _, tool := range completion.ProposedTools {
			frozen, known := catalog.Lookup(tool.ToolName)
			if !known {
				return nil, fmt.Errorf("%w: tool %q is not in the attempt's frozen catalog", ErrLedgerDenied, tool.ToolName)
			}
			// Compatibility verification against the installed executor; the
			// frozen bytes stay the Model Call provenance (ADR-0004).
			def, err := service.Catalogs.InstalledDefinition(frozen)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrLedgerDenied, err)
			}
			if err := ValidateToolArguments(def, tool.ArgumentsJSON); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrLedgerDenied, err)
			}
			if tool.ProviderToolCallID == "" {
				return nil, fmt.Errorf("%w: provider tool call id is empty", ErrLedgerDenied)
			}
			argumentsDigest := sha256.Sum256(tool.ArgumentsJSON)
			if hex.EncodeToString(argumentsDigest[:]) != tool.ArgumentsDigest {
				return nil, fmt.Errorf("%w: tool %s arguments digest mismatch", ErrLedgerDenied, tool.ToolName)
			}
			validated = append(validated, validatedTool{proposed: tool, definition: def})
			var arguments any
			_ = json.Unmarshal(tool.ArgumentsJSON, &arguments)
			output["tool_calls"] = append(output["tool_calls"].([]any), map[string]any{
				"id": tool.ProviderToolCallID, "name": tool.ToolName, "arguments": arguments,
			})
		}
		// Seal the model call BEFORE inserting tool_calls rows
		// (trg_tool_call_closure: tool calls may only be inserted pending
		// after a successful model call with a complete output in the same
		// Running attempt).
		responseJSON, err := json.Marshal(output)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at)
			VALUES(?,1,?,?,?,?)`, completion.CallID, string(responseJSON), completion.ResponseDigest, completion.FinishReason, now); err != nil {
			return nil, err
		}
		usage := fmt.Sprintf(`{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d}`, completion.InputTokens, completion.OutputTokens, completion.TotalTokens)
		if _, err := tx.ExecContext(ctx, `
			UPDATE model_calls SET provider_request_id=?,usage_json=?,latency_ms=?,status='succeeded',ended_at=?
			WHERE id=? AND status='running'`, completion.ProviderRequestID, usage, completion.LatencyMS, now, completion.CallID); err != nil {
			return nil, err
		}
		for index, item := range validated {
			insert, err := tx.ExecContext(ctx, `
				INSERT INTO tool_calls(attempt_id,model_call_id,call_seq,tool_index,provider_tool_call_id,
					tool_name,tool_version,arguments_json,arguments_digest,execution_mode,failure_mode,status,created_at)
				VALUES(?,?,?,?,?,?,?,?,?,?,?, 'pending', ?)`,
				completion.AttemptID, completion.CallID, callSeq, index, item.proposed.ProviderToolCallID,
				item.proposed.ToolName, item.definition.Version, string(item.proposed.ArgumentsJSON), item.proposed.ArgumentsDigest,
				item.definition.ExecutionMode, item.definition.FailureMode, now)
			if err != nil {
				return nil, err
			}
			toolCallID, err := insert.LastInsertId()
			if err != nil {
				return nil, err
			}
			authorization := ToolAuthorization{
				ToolCallID: toolCallID, ProviderIndex: uint32(index),
				ProviderToolCallID: item.proposed.ProviderToolCallID, FailureMode: item.definition.FailureMode,
			}
			// Observation tools freeze their connection binding inside this
			// same transaction (ARCH-INPUT-003); an unresolvable route fails
			// the whole model call (RUNTIME-AGENT-005).
			if needsConnectionGrant(item.definition) {
				if service.ToolGrantResolver == nil {
					return nil, fmt.Errorf("%w: tool %s needs a grant resolver", ErrLedgerDenied, item.definition.Name)
				}
				resolution, err := service.ToolGrantResolver(ctx, tx, completion.AttemptID, toolCallID, item.definition)
				if err != nil {
					return nil, err
				}
				if resolution.PreflightCode != "" && len(resolution.Grants) != 0 {
					return nil, fmt.Errorf("%w: tool %s resolution mixes preflight and grants", ErrLedgerDenied, item.definition.Name)
				}
				if resolution.PreflightCode != "" {
					if _, err := tx.ExecContext(ctx, `UPDATE tool_calls SET preflight_error_code=?,preflight_error_detail=?,row_version=row_version+1 WHERE id=? AND status='pending'`, resolution.PreflightCode, resolution.PreflightDetail, toolCallID); err != nil {
						return nil, err
					}
				}
				authorization.Grants = resolution.Grants
				authorization.PreflightCode = resolution.PreflightCode
				authorization.PreflightDetail = resolution.PreflightDetail
				// A recoverable preflight (e.g. ambiguous source) carries NO
				// normalized execution inputs by design: reading them here
				// would fail the whole response instead of returning the
				// preflight question to the model. Only a real grant
				// resolution freezes execution arguments.
				if item.definition.Name == "thanos_query" && authorization.PreflightCode == "" {
					var executionJSON, executionDigest string
					if err := tx.QueryRowContext(ctx, `SELECT arguments_json,arguments_digest FROM tool_call_execution_inputs WHERE tool_call_id=?`, toolCallID).Scan(&executionJSON, &executionDigest); err != nil {
						return nil, fmt.Errorf("%w: normalized execution arguments missing: %v", ErrLedgerDenied, err)
					}
					digest, err := hex.DecodeString(executionDigest)
					if err != nil || len(digest) != sha256.Size {
						return nil, fmt.Errorf("%w: normalized execution arguments digest invalid", ErrLedgerDenied)
					}
					authorization.ExecutionArgumentsJSON = []byte(executionJSON)
					authorization.ExecutionArgumentsDigest = digest
				}
			}
			authorizations = append(authorizations, authorization)
		}
	} else {
		if completion.FailureReason == "" {
			return nil, fmt.Errorf("%w: non-success model call requires a termination reason", ErrLedgerDenied)
		}
		// Any already-exposed partial response persists as an incomplete
		// output row (RUNTIME-AGENT-005, DATA-AUDIT-003): it is physical
		// audit only and can never seal the call.
		if completion.AssistantText != "" {
			partial := map[string]any{"assistantText": completion.AssistantText, "finishReason": completion.FinishReason, "tool_calls": []any{}}
			partialJSON, err := json.Marshal(partial)
			if err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at)
				VALUES(?,0,?,?,?,?)`, completion.CallID, string(partialJSON), sha256Hex(partialJSON), completion.FinishReason, now); err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE model_calls SET provider_request_id=?,latency_ms=?,status=?,termination_reason=?,ended_at=?
			WHERE id=? AND status='running'`,
			completion.ProviderRequestID, completion.LatencyMS, completion.Outcome, completion.FailureReason, now, completion.CallID); err != nil {
			return nil, err
		}
		// A failed or cancelled physical call is an expected durable outcome,
		// not a stage error: the runner keeps the sealed state and records the
		// audit fact with the failure outcome (execution.RecordedFailure).
		return nil, &execution.RecordedFailure{
			Code:     "attempt_model_call_" + completion.Outcome,
			Detail:   fmt.Sprintf("model call %d sealed as %s", completion.CallID, completion.Outcome),
			ObjectID: completion.CallID,
		}
	}
	return authorizations, nil
}

// BeginToolCall moves one pending tool call to running after re-checking
// the attempt fence and the frozen connection grant closure
// (ARCH-TOOL-002/003, DATA-CONN-002: the execution authorization re-reads
// the connection state; a disable/rotation/rebind committed first
// refuses execution). The standalone stage runs through the shared
// execution runner (ADR-0006): the runner owns the transaction and records
// the automatic audit fact on it; every denied write rolls back and records
// nothing.
func (service *Service) BeginToolCall(ctx context.Context, attemptID, toolCallID int64) error {
	authority, err := service.lifecycleAuthority(ctx, attemptID)
	if err != nil {
		return err
	}
	_, err = execution.Execute(authority, service.runner, service.opToolCallBegin,
		func(tx *execution.Tx) (struct{}, error) {
			var state string
			if err := tx.QueryRowContext(ctx, `SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&state); err != nil {
				return struct{}{}, err
			}
			if state != "Running" {
				return struct{}{}, fmt.Errorf("%w: attempt %d is %s", ErrLedgerDenied, attemptID, state)
			}
			var callAttempt int64
			var status, toolName string
			if err := tx.QueryRowContext(ctx, `SELECT attempt_id,status,tool_name FROM tool_calls WHERE id=?`, toolCallID).Scan(&callAttempt, &status, &toolName); err != nil {
				return struct{}{}, err
			}
			if callAttempt != attemptID {
				return struct{}{}, fmt.Errorf("%w: tool call %d belongs to attempt %d", ErrLedgerDenied, toolCallID, callAttempt)
			}
			if status != "pending" {
				return struct{}{}, fmt.Errorf("%w: tool call %d is %s", ErrLedgerDenied, toolCallID, status)
			}
			catalog, err := frozenToolCatalogOn(ctx, tx, attemptID)
			if err != nil {
				return struct{}{}, fmt.Errorf("%w: frozen tool catalog: %v", ErrLedgerDenied, err)
			}
			frozen, known := catalog.Lookup(toolName)
			if !known {
				return struct{}{}, fmt.Errorf("%w: tool %q is not in the attempt's frozen catalog", ErrLedgerDenied, toolName)
			}
			definition, err := service.Catalogs.InstalledDefinition(frozen)
			if err != nil {
				return struct{}{}, fmt.Errorf("%w: %v", ErrLedgerDenied, err)
			}
			if needsConnectionGrant(definition) {
				var grantCount int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tool_call_connection_grants WHERE tool_call_id=?`, toolCallID).Scan(&grantCount); err != nil {
					return struct{}{}, err
				}
				// A zero-grant typed observation is the resolver's accepted routing
				// preflight. It must reach the model without credential validation.
				if grantCount != 0 {
					if service.ToolGrantValidator == nil {
						return struct{}{}, fmt.Errorf("%w: tool %s has no grant validator wired", ErrLedgerDenied, toolName)
					}
					if err := service.ToolGrantValidator(ctx, tx, attemptID, toolCallID, definition); err != nil {
						return struct{}{}, fmt.Errorf("%w: grant validation: %v", ErrLedgerDenied, err)
					}
				}
			}
			result, err := tx.ExecContext(ctx, `
				UPDATE tool_calls SET status='running', started_at=?, row_version=row_version+1
				WHERE id=? AND attempt_id=? AND status='pending'`, time.Now().UTC().Format(time.RFC3339Nano), toolCallID, attemptID)
			if err != nil {
				return struct{}{}, err
			}
			if affected, _ := result.RowsAffected(); affected != 1 {
				return struct{}{}, fmt.Errorf("%w: tool call %d is not pending", ErrLedgerDenied, toolCallID)
			}
			return struct{}{}, nil
		},
		func(struct{}) int64 { return toolCallID })
	return err
}

// ExpectedToolResultSchema resolves the sole ResultPayload schema accepted for
// one persisted Tool Call. Runtime ingress calls this before any result write,
// so a compromised or buggy supervisor cannot relabel one fixed tool result as
// another contract.
func (service *Service) ExpectedToolResultSchema(ctx context.Context, toolCallID int64) (string, error) {
	var toolName, agentVersion string
	var attemptID int64
	if err := service.Reader().QueryRowContext(ctx, `
		SELECT t.tool_name,t.attempt_id,a.agent_version
		FROM tool_calls t JOIN execution_attempts a ON a.id=t.attempt_id
		WHERE t.id=?`, toolCallID).Scan(&toolName, &attemptID, &agentVersion); err != nil {
		return "", err
	}
	catalog, err := service.FrozenToolCatalog(ctx, attemptID)
	if err != nil {
		return "", err
	}
	frozen, known := catalog.Lookup(toolName)
	if !known || frozen.ResultSchemaKind == "" {
		return "", fmt.Errorf("%w: tool %q has no fixed result schema", ErrLedgerDenied, toolName)
	}
	return frozen.ResultSchemaKind, nil
}

// ToolResult is the sealed outcome of one tool execution.
type ToolResult struct {
	AttemptID   int64
	ToolCallID  int64
	Outcome     string // succeeded | failed | cancelled
	ResultJSON  string // bounded model-visible preview (succeeded, or failed with return_to_model)
	ArtifactID  int64  // 0 when no long body exists
	ErrorCode   string
	ErrorDetail string
}

// CompleteToolCall seals one tool execution: the canonical result preview,
// the artifact link, the deterministic Evidence of observation tools and
// the terminal state in one transaction (ARCH-TOOL-003/005,
// ARCH-OUTPUT-005, DATA-EVIDENCE-001). It returns the committed evidence
// ids for the CompleteToolCallAck. The standalone stage runs through the
// shared execution runner (ADR-0006): the runner owns the transaction and
// records the automatic audit fact with the sealed outcome.
func (service *Service) CompleteToolCall(ctx context.Context, result ToolResult) ([]int64, error) {
	authority, err := service.lifecycleAuthority(ctx, result.AttemptID)
	if err != nil {
		return nil, err
	}
	var evidenceIDs []int64
	evidenceIDs, err = execution.Execute(authority, service.runner, service.opToolCallComplete,
		func(tx *execution.Tx) ([]int64, error) {
			var callAttempt int64
			var status, failureMode, toolName string
			if err := tx.QueryRowContext(ctx, `SELECT t.attempt_id,t.status,t.failure_mode,t.tool_name FROM tool_calls t WHERE t.id=?`, result.ToolCallID).Scan(&callAttempt, &status, &failureMode, &toolName); err != nil {
				return nil, err
			}
			if callAttempt != result.AttemptID {
				return nil, fmt.Errorf("%w: tool call %d belongs to attempt %d", ErrLedgerDenied, result.ToolCallID, callAttempt)
			}
			if status != "running" {
				return nil, fmt.Errorf("%w: tool call %d is %s", ErrLedgerDenied, result.ToolCallID, status)
			}
			if result.Outcome == "succeeded" {
				if result.ResultJSON == "" {
					return nil, fmt.Errorf("%w: succeeded tool call requires a result preview", ErrLedgerDenied)
				}
				if !jsonValid([]byte(result.ResultJSON), "object") {
					return nil, fmt.Errorf("%w: tool result preview must be a JSON object", ErrLedgerDenied)
				}
			} else {
				if result.ErrorCode == "" {
					return nil, fmt.Errorf("%w: non-success tool call requires an error code", ErrLedgerDenied)
				}
				if failureMode == "return_to_model" && result.Outcome == "failed" && result.ResultJSON == "" {
					return nil, fmt.Errorf("%w: return_to_model failure requires a model-visible result", ErrLedgerDenied)
				}
				if result.Outcome == "failed" && result.ResultJSON != "" && !jsonValid([]byte(result.ResultJSON), "object") {
					return nil, fmt.Errorf("%w: tool failure preview must be a JSON object", ErrLedgerDenied)
				}
			}
			if result.ArtifactID != 0 {
				var ownerTool, ownerType string
				if err := tx.QueryRowContext(ctx, `SELECT owner_type, owner_id FROM artifacts WHERE id=?`, result.ArtifactID).Scan(&ownerType, &ownerTool); err != nil {
					return nil, fmt.Errorf("%w: artifact %d unknown: %v", ErrLedgerDenied, result.ArtifactID, err)
				}
				if ownerType != "tool_call" || ownerTool != fmt.Sprint(result.ToolCallID) {
					return nil, fmt.Errorf("%w: artifact %d is not owned by tool call %d", ErrLedgerDenied, result.ArtifactID, result.ToolCallID)
				}
			}
			now := time.Now().UTC().Format(time.RFC3339Nano)
			state := map[string]string{"succeeded": "succeeded", "failed": "failed", "cancelled": "cancelled"}[result.Outcome]
			if state == "" {
				return nil, fmt.Errorf("%w: unknown tool outcome %q", ErrLedgerDenied, result.Outcome)
			}
			var artifactID sql.NullInt64
			if result.ArtifactID != 0 {
				artifactID = sql.NullInt64{Int64: result.ArtifactID, Valid: true}
			}
			var resultJSON sql.NullString
			if result.ResultJSON != "" {
				resultJSON = sql.NullString{String: result.ResultJSON, Valid: true}
			}
			var errorDetail sql.NullString
			if result.ErrorDetail != "" {
				errorDetail = sql.NullString{String: result.ErrorDetail, Valid: true}
			}
			// Observation tools commit their deterministic Evidence BEFORE the
			// terminal state advances (DATA-EVIDENCE-001 and the frozen
			// trg_evidence_attempt_tool_closure demand the Tool Call still
			// running at the Evidence INSERT), all in the same transaction. The
			// evidence behaviour is resolved through the attempt's OWN frozen
			// catalog and the assembly's implementation table.
			if result.Outcome == "succeeded" && service.EvidenceWriter != nil {
				catalog, err := frozenToolCatalogOn(ctx, tx, result.AttemptID)
				if err != nil {
					return nil, fmt.Errorf("%w: frozen tool catalog: %v", ErrLedgerDenied, err)
				}
				frozenTool, known := catalog.Lookup(toolName)
				if !known {
					return nil, fmt.Errorf("%w: tool %q is not in the attempt's frozen catalog", ErrLedgerDenied, toolName)
				}
				definition, err := service.Catalogs.InstalledDefinition(frozenTool)
				if err != nil {
					return nil, fmt.Errorf("%w: %v", ErrLedgerDenied, err)
				}
				if definition.ProducesEvidence {
					ids, err := service.EvidenceWriter(ctx, tx, result.AttemptID, result.ToolCallID, result.ArtifactID,
						[]byte(result.ResultJSON), definition.Name)
					if err != nil {
						return nil, fmt.Errorf("%w: evidence commit: %v", ErrLedgerDenied, err)
					}
					evidenceIDs = ids
				}
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE tool_calls SET status=?, result_json=?, result_artifact_id=?, error_detail=?, ended_at=?, row_version=row_version+1
				WHERE id=? AND attempt_id=? AND status='running'`,
				state, resultJSON, artifactID, errorDetail, now, result.ToolCallID, result.AttemptID); err != nil {
				return nil, err
			}
			// The frozen grant closure requires the tool call to be succeeded, so
			// the read grant is written AFTER the seal, still in the same
			// transaction (trg_attempt_artifact_grants_closure).
			if result.ArtifactID != 0 && service.ToolResultGrants != nil {
				if err := service.ToolResultGrants(ctx, tx, result.AttemptID, result.ArtifactID, result.ToolCallID); err != nil {
					return nil, fmt.Errorf("%w: tool result grant: %v", ErrLedgerDenied, err)
				}
			}
			if result.Outcome != "succeeded" {
				// A failed or cancelled seal is an expected durable outcome,
				// not a stage error: the runner records the failure audit and
				// keeps the sealed state (execution.RecordedFailure).
				return nil, &execution.RecordedFailure{
					Code:     "attempt_tool_call_" + result.Outcome,
					Detail:   fmt.Sprintf("tool call %d sealed as %s", result.ToolCallID, result.Outcome),
					ObjectID: result.ToolCallID,
				}
			}
			return evidenceIDs, nil
		},
		func([]int64) int64 { return result.ToolCallID })
	var recorded *execution.RecordedFailure
	if errors.As(err, &recorded) {
		// The failed/cancelled seal committed; no evidence exists (evidence
		// is only produced on success).
		return nil, nil
	}
	return evidenceIDs, err
}

// needsConnectionGrant reports whether the fixed tool definition freezes a
// connection binding inside the Tool Call persistence transaction
// (ARCH-INPUT-003): supervisor_typed observation tools resolve their
// deployment connection per tool call; the model never selects it.
func needsConnectionGrant(definition ToolDef) bool {
	return definition.RequiresConnectionGrant
}

// ToolCallView is the read projection of one tool call (used by the tool
// call begin/complete handlers to build wire responses).
type ToolCallView struct {
	ID             int64
	AttemptID      int64
	CallSeq        int
	ToolIndex      int
	ToolName       string
	ExecutionMode  string
	FailureMode    string
	Status         string
	ResultJSON     *string
	ResultArtifact int64
	ErrorDetail    *string
}

// GetToolCall returns one tool call row.
func (service *Service) GetToolCall(ctx context.Context, toolCallID int64) (ToolCallView, error) {
	var view ToolCallView
	var resultJSON, errorDetail sql.NullString
	var artifactID sql.NullInt64
	err := service.Reader().QueryRowContext(ctx, `
		SELECT id,attempt_id,call_seq,tool_index,tool_name,execution_mode,failure_mode,status,result_json,result_artifact_id,error_detail
		FROM tool_calls WHERE id=?`, toolCallID).
		Scan(&view.ID, &view.AttemptID, &view.CallSeq, &view.ToolIndex, &view.ToolName,
			&view.ExecutionMode, &view.FailureMode, &view.Status, &resultJSON, &artifactID, &errorDetail)
	if err != nil {
		return ToolCallView{}, err
	}
	if resultJSON.Valid {
		view.ResultJSON = &resultJSON.String
	}
	if errorDetail.Valid {
		view.ErrorDetail = &errorDetail.String
	}
	if artifactID.Valid {
		view.ResultArtifact = artifactID.Int64
	}
	return view, nil
}

// HasSucceededChatCall reports whether the attempt already carries a
// succeeded chat call (call_seq ordering sanity for the worker).
func (service *Service) HasSucceededChatCall(ctx context.Context, attemptID int64, callSeq int) (bool, error) {
	var count int
	err := service.Reader().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM model_calls WHERE attempt_id=? AND call_seq=? AND status='succeeded'`,
		attemptID, callSeq).Scan(&count)
	return count > 0, err
}

// promptRendererVersionFor records the immutable input renderer independently
// from the executor generation carried by the attempt.
// promptRendererVersionFor records the immutable input renderer
// independently from the executor generation carried by the attempt. The
// literals MUST match the snapshot renderer_version constants the owning
// scope writes (analysis.RendererVersion / investigation.RendererVersion):
// b58's source-integration input shape is renderer v4 / investigation v2.
// The inspection report prompt has its own renderer generation: its prompt
// text evolves independently of the initial-analysis prompt, so changing it
// MUST bump InspectionAgentVersion and this mapping together — a model call
// row never records a renderer version whose prompt bytes it did not see.
func promptRendererVersionFor(agentVersion string) string {
	if agentVersion == "investigation-v1" {
		return "investigation-renderer-v2"
	}
	if agentVersion == InspectionAgentVersion {
		return "inspection-analysis-renderer-v1"
	}
	return "initial-analysis-renderer-v4"
}
