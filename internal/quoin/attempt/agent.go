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
func (service *Service) BeginModelCall(ctx context.Context, begin BeginCall) (int64, error) {
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
	var state, agentVersion string
	if err := conn.QueryRowContext(ctx, `SELECT state,agent_version FROM execution_attempts WHERE id=?`, begin.AttemptID).Scan(&state, &agentVersion); err != nil {
		return 0, err
	}
	if state != "Running" {
		return 0, fmt.Errorf("%w: attempt %d is %s", ErrLedgerDenied, begin.AttemptID, state)
	}
	// The attempt's chat grant must exist and its qualified probe result
	// must carry the exact contract the worker claims (ARCH-AGENT-003).
	// The lookup must run on the transaction connection: the production
	// pool is single-connection (SQLite writer serialization) and a pool
	// query here would self-deadlock against the open transaction.
	modelID, contextBudget, maxOutput, err := service.lookupChatContractOn(ctx, conn, begin.AttemptID)
	if err != nil {
		return 0, fmt.Errorf("%w: chat contract lookup: %v", ErrLedgerDenied, err)
	}
	if begin.ModelID != modelID {
		return 0, fmt.Errorf("%w: model id %q does not match the attempt's qualified model %q", ErrLedgerDenied, begin.ModelID, modelID)
	}
	if begin.ContextBudget != contextBudget || begin.MaxOutput != maxOutput {
		return 0, fmt.Errorf("%w: budget override refused (contract %d/%d, request %d/%d)", ErrLedgerDenied, contextBudget, maxOutput, begin.ContextBudget, begin.MaxOutput)
	}
	wantToolDigest, err := CanonicalToolsDigest(agentVersion)
	if err != nil {
		return 0, err
	}
	if begin.ToolSchemaDigest != wantToolDigest {
		return 0, fmt.Errorf("%w: tool schema digest mismatch (worker renders %s, catalog %s)", ErrLedgerDenied, begin.ToolSchemaDigest, wantToolDigest)
	}
	var grantID int64
	if err := conn.QueryRowContext(ctx, `SELECT id FROM attempt_connection_grants WHERE attempt_id=? AND purpose='chat_model' ORDER BY id LIMIT 1`, begin.AttemptID).Scan(&grantID); err != nil {
		return 0, fmt.Errorf("%w: chat_model grant missing: %v", ErrLedgerDenied, err)
	}
	// Replay of the same physical call (a lost BeginModelCallAck after a
	// stream drop) must return the original row instead of rejecting:
	// the frozen digests prove it is the same request (RUNTIME-AGENT-005
	// idempotent ledger; a divergent resend conflicts).
	var existingID int64
	var existingPrompt, existingTools, existingRendered string
	err = conn.QueryRowContext(ctx, `
		SELECT id, prompt_digest, tool_schema_digest, rendered_request_digest
		FROM model_calls WHERE attempt_id=? AND call_seq=? AND retry_seq=?`,
		begin.AttemptID, begin.CallSeq, begin.RetrySeq).Scan(&existingID, &existingPrompt, &existingTools, &existingRendered)
	if err == nil {
		if existingPrompt != begin.PromptDigest || existingTools != begin.ToolSchemaDigest || existingRendered != begin.RenderedDigest {
			return 0, fmt.Errorf("%w: replay of call %d carries divergent digests", ErrLedgerDenied, existingID)
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return 0, err
		}
		committed = true
		return existingID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if begin.RetrySeq > 0 {
		var prevID int64
		var previous, prevPrompt, prevTools, prevRendered string
		err = conn.QueryRowContext(ctx, `
			SELECT id,status,prompt_digest,tool_schema_digest,rendered_request_digest
			FROM model_calls WHERE attempt_id=? AND call_seq=? AND retry_seq=?`,
			begin.AttemptID, begin.CallSeq, begin.RetrySeq-1).Scan(&prevID, &previous, &prevPrompt, &prevTools, &prevRendered)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("%w: retry_seq %d has no failed predecessor", ErrLedgerDenied, begin.RetrySeq)
		}
		if err != nil {
			return 0, err
		}
		if previous == "running" &&
			prevPrompt == begin.PromptDigest && prevTools == begin.ToolSchemaDigest && prevRendered == begin.RenderedDigest {
			// Lost-ack alias: the runtime believed the predecessor failed and
			// resent with retry_seq+1, but identical digests prove the
			// predecessor IS this physical call (its Begin ack was lost with
			// a dropped stream). Return the live row instead of breaking the
			// retry chain (RUNTIME-AGENT-005 idempotent ledger).
			if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
				return 0, err
			}
			committed = true
			return prevID, nil
		}
		if previous != "failed" {
			return 0, fmt.Errorf("%w: retry_seq %d predecessor is %q", ErrLedgerDenied, begin.RetrySeq, previous)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO model_calls(attempt_id,call_seq,retry_seq,operation,model_id,connection_grant_id,
			prompt_renderer_version,agent_version,prompt_digest,tool_schema_version,tool_schema_digest,
			input_snapshot_digest,rendered_request_digest,context_budget_tokens,max_output_tokens,
			estimated_input_tokens,evicted_turn_count,status,started_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?, 'running', ?)`,
		begin.AttemptID, begin.CallSeq, begin.RetrySeq, "chat", begin.ModelID, grantID,
		agentVersion, agentVersion, begin.PromptDigest, toolSchemaVersionFor(agentVersion), begin.ToolSchemaDigest,
		begin.InputDigest, begin.RenderedDigest, begin.ContextBudget, begin.MaxOutput,
		begin.EstimatedInput, begin.EvictedTurns, now)
	if err != nil {
		return 0, err
	}
	callID, err := insert.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := service.writeCallInputItems(ctx, conn, callID, begin); err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return 0, err
	}
	committed = true
	return callID, nil
}

// writeCallInputItems validates and persists the ordered input lineage
// (ARCH-CONTEXT-006, trg_model_call_success_input).
func (service *Service) writeCallInputItems(ctx context.Context, conn *sql.Conn, callID int64, begin BeginCall) error {
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
	ToolCallID         int64
	ProviderIndex      uint32
	ProviderToolCallID string
	FailureMode        string
	Grants             []ToolGrant
	PreflightCode      string
	PreflightDetail    string
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
// Ack (ARCH-AGENT-006).
func (service *Service) CompleteModelCall(ctx context.Context, completion CompleteCall) ([]ToolAuthorization, error) {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var callAttempt int64
	var status, agentVersion string
	var callSeq int
	if err := conn.QueryRowContext(ctx, `SELECT m.attempt_id,m.status,m.call_seq,a.agent_version FROM model_calls m JOIN execution_attempts a ON a.id=m.attempt_id WHERE m.id=?`, completion.CallID).Scan(&callAttempt, &status, &callSeq, &agentVersion); err != nil {
		return nil, err
	}
	if callAttempt != completion.AttemptID {
		return nil, fmt.Errorf("%w: call %d belongs to attempt %d", ErrLedgerDenied, completion.CallID, callAttempt)
	}
	if status != "running" {
		// Replay of an already-sealed physical call (a lost
		// CompleteModelCallAck after a stream drop) must rebuild the
		// original Ack instead of rejecting (RUNTIME-AGENT-005); a
		// divergent resend conflicts.
		return service.replayCompleteModelCall(ctx, conn, completion, status)
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
		for _, tool := range completion.ProposedTools {
			def, known := LookupToolForAgentVersion(agentVersion, tool.ToolName)
			if !known {
				return nil, fmt.Errorf("%w: tool %q is not in the fixed catalog", ErrLedgerDenied, tool.ToolName)
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
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at)
			VALUES(?,1,?,?,?,?)`, completion.CallID, string(responseJSON), completion.ResponseDigest, completion.FinishReason, now); err != nil {
			return nil, err
		}
		usage := fmt.Sprintf(`{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d}`, completion.InputTokens, completion.OutputTokens, completion.TotalTokens)
		if _, err := conn.ExecContext(ctx, `
			UPDATE model_calls SET provider_request_id=?,usage_json=?,latency_ms=?,status='succeeded',ended_at=?
			WHERE id=? AND status='running'`, completion.ProviderRequestID, usage, completion.LatencyMS, now, completion.CallID); err != nil {
			return nil, err
		}
		for index, item := range validated {
			insert, err := conn.ExecContext(ctx, `
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
				resolution, err := service.ToolGrantResolver(ctx, conn, completion.AttemptID, toolCallID, item.definition)
				if err != nil {
					return nil, err
				}
				if resolution.PreflightCode != "" && len(resolution.Grants) != 0 {
					return nil, fmt.Errorf("%w: tool %s resolution mixes preflight and grants", ErrLedgerDenied, item.definition.Name)
				}
				if resolution.PreflightCode != "" {
					if _, err := conn.ExecContext(ctx, `UPDATE tool_calls SET preflight_error_code=?,preflight_error_detail=?,row_version=row_version+1 WHERE id=? AND status='pending'`, resolution.PreflightCode, resolution.PreflightDetail, toolCallID); err != nil {
						return nil, err
					}
				}
				authorization.Grants = resolution.Grants
				authorization.PreflightCode = resolution.PreflightCode
				authorization.PreflightDetail = resolution.PreflightDetail
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
			if _, err := conn.ExecContext(ctx, `
				INSERT INTO model_call_outputs(model_call_id,complete,response_json,response_digest,finish_reason,created_at)
				VALUES(?,0,?,?,?,?)`, completion.CallID, string(partialJSON), sha256Hex(partialJSON), completion.FinishReason, now); err != nil {
				return nil, err
			}
		}
		if _, err := conn.ExecContext(ctx, `
			UPDATE model_calls SET provider_request_id=?,latency_ms=?,status=?,termination_reason=?,ended_at=?
			WHERE id=? AND status='running'`,
			completion.ProviderRequestID, completion.LatencyMS, completion.Outcome, completion.FailureReason, now, completion.CallID); err != nil {
			return nil, err
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return nil, err
	}
	committed = true
	return authorizations, nil
}

// BeginToolCall moves one pending tool call to running after re-checking
// the attempt fence and the frozen connection grant closure
// (ARCH-TOOL-002/003, DATA-CONN-002: the execution authorization re-reads
// the connection state; a disable/rotation/rebind committed first
// refuses execution).
func (service *Service) BeginToolCall(ctx context.Context, attemptID, toolCallID int64) error {
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
	var state, agentVersion string
	if err := conn.QueryRowContext(ctx, `SELECT state,agent_version FROM execution_attempts WHERE id=?`, attemptID).Scan(&state, &agentVersion); err != nil {
		return err
	}
	if state != "Running" {
		return fmt.Errorf("%w: attempt %d is %s", ErrLedgerDenied, attemptID, state)
	}
	var callAttempt int64
	var status, toolName string
	if err := conn.QueryRowContext(ctx, `SELECT attempt_id,status,tool_name FROM tool_calls WHERE id=?`, toolCallID).Scan(&callAttempt, &status, &toolName); err != nil {
		return err
	}
	if callAttempt != attemptID {
		return fmt.Errorf("%w: tool call %d belongs to attempt %d", ErrLedgerDenied, toolCallID, callAttempt)
	}
	if status != "pending" {
		return fmt.Errorf("%w: tool call %d is %s", ErrLedgerDenied, toolCallID, status)
	}
	definition, known := LookupToolForAgentVersion(agentVersion, toolName)
	if !known {
		return fmt.Errorf("%w: tool %q is not in the fixed catalog", ErrLedgerDenied, toolName)
	}
	if needsConnectionGrant(definition) {
		var grantCount int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM tool_call_connection_grants WHERE tool_call_id=?`, toolCallID).Scan(&grantCount); err != nil {
			return err
		}
		// A zero-grant typed observation is the resolver's accepted routing
		// preflight. It must reach the model without credential validation.
		// Kubernetes grants are fenced independently at FetchCredentialGrant.
		// Valid sibling mappings must still execute if another mapping is stale.
		if grantCount != 0 && definition.Name != "kubernetes_read" {
			if service.ToolGrantValidator == nil {
				return fmt.Errorf("%w: tool %s has no grant validator wired", ErrLedgerDenied, toolName)
			}
			if err := service.ToolGrantValidator(ctx, conn, attemptID, toolCallID, definition); err != nil {
				return fmt.Errorf("%w: grant validation: %v", ErrLedgerDenied, err)
			}
		}
	}
	result, err := conn.ExecContext(ctx, `
		UPDATE tool_calls SET status='running', started_at=?, row_version=row_version+1
		WHERE id=? AND attempt_id=? AND status='pending'`, time.Now().UTC().Format(time.RFC3339Nano), toolCallID, attemptID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return fmt.Errorf("%w: tool call %d is not pending", ErrLedgerDenied, toolCallID)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// ExpectedToolResultSchema resolves the sole ResultPayload schema accepted for
// one persisted Tool Call. Runtime ingress calls this before any result write,
// so a compromised or buggy supervisor cannot relabel one fixed tool result as
// another contract.
func (service *Service) ExpectedToolResultSchema(ctx context.Context, toolCallID int64) (string, error) {
	var toolName, agentVersion string
	if err := service.db.QueryRowContext(ctx, `
		SELECT t.tool_name,a.agent_version
		FROM tool_calls t JOIN execution_attempts a ON a.id=t.attempt_id
		WHERE t.id=?`, toolCallID).Scan(&toolName, &agentVersion); err != nil {
		return "", err
	}
	definition, known := LookupToolForAgentVersion(agentVersion, toolName)
	if !known || definition.ResultSchemaKind == "" {
		return "", fmt.Errorf("%w: tool %q has no fixed result schema", ErrLedgerDenied, toolName)
	}
	return definition.ResultSchemaKind, nil
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
// ids for the CompleteToolCallAck.
func (service *Service) CompleteToolCall(ctx context.Context, result ToolResult) ([]int64, error) {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var callAttempt int64
	var status, failureMode, toolName, agentVersion string
	if err := conn.QueryRowContext(ctx, `SELECT t.attempt_id,t.status,t.failure_mode,t.tool_name,a.agent_version FROM tool_calls t JOIN execution_attempts a ON a.id=t.attempt_id WHERE t.id=?`, result.ToolCallID).Scan(&callAttempt, &status, &failureMode, &toolName, &agentVersion); err != nil {
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
		if err := conn.QueryRowContext(ctx, `SELECT owner_type, owner_id FROM artifacts WHERE id=?`, result.ArtifactID).Scan(&ownerType, &ownerTool); err != nil {
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
	// running at the Evidence INSERT), all in the same transaction.
	var evidenceIDs []int64
	if result.Outcome == "succeeded" && service.EvidenceWriter != nil {
		definition, known := LookupToolForAgentVersion(agentVersion, toolName)
		if !known {
			return nil, fmt.Errorf("%w: tool %q is not in the fixed catalog", ErrLedgerDenied, toolName)
		}
		if definition.ProducesEvidence {
			ids, err := service.EvidenceWriter(ctx, conn, result.AttemptID, result.ToolCallID, result.ArtifactID,
				[]byte(result.ResultJSON), definition.Name)
			if err != nil {
				return nil, fmt.Errorf("%w: evidence commit: %v", ErrLedgerDenied, err)
			}
			evidenceIDs = ids
		}
	}
	if _, err := conn.ExecContext(ctx, `
		UPDATE tool_calls SET status=?, result_json=?, result_artifact_id=?, error_detail=?, ended_at=?, row_version=row_version+1
		WHERE id=? AND attempt_id=? AND status='running'`,
		state, resultJSON, artifactID, errorDetail, now, result.ToolCallID, result.AttemptID); err != nil {
		return nil, err
	}
	// The frozen grant closure requires the tool call to be succeeded, so
	// the read grant is written AFTER the seal, still in the same
	// transaction (trg_attempt_artifact_grants_closure).
	if result.ArtifactID != 0 && service.ToolResultGrants != nil {
		if err := service.ToolResultGrants(ctx, conn, result.AttemptID, result.ArtifactID, result.ToolCallID); err != nil {
			return nil, fmt.Errorf("%w: tool result grant: %v", ErrLedgerDenied, err)
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return nil, err
	}
	committed = true
	return evidenceIDs, nil
}

// needsConnectionGrant reports whether the fixed tool definition freezes a
// connection binding inside the Tool Call persistence transaction
// (ARCH-INPUT-003): supervisor_typed observation tools resolve their
// deployment connection per tool call; the model never selects it.
func needsConnectionGrant(definition ToolDef) bool {
	// Artifact tools are supervisor typed but do not carry external secrets.
	return definition.Name == "thanos_query" || definition.Name == "kubernetes_read"
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
	err := service.db.QueryRowContext(ctx, `
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
	err := service.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM model_calls WHERE attempt_id=? AND call_seq=? AND status='succeeded'`,
		attemptID, callSeq).Scan(&count)
	return count > 0, err
}
