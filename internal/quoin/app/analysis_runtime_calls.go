package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
)

// Agent model/tool call ledger handlers: envelope adjudication for
// BeginModelCall/CompleteModelCall/BeginToolCall/CompleteToolCall plus
// the wire mappers for input items, roles, failure modes and termination
// reasons.

// handleBeginModelCallRouted opens the physical model call row for the
// owning slice: probes keep the T08 fixed-profile ledger; agent attempts
// commit through the attempt package (ARCH-AGENT-006).
func (service *RuntimeService) handleBeginModelCallRouted(ctx context.Context, envelope *runtimev1.ControlEnvelope, begin *runtimev1.BeginModelCall) {
	attemptType, err := service.attemptTypeOf(ctx, begin.GetAttemptId())
	if err != nil {
		service.rejectBegin(ctx, envelope, begin, "attempt lookup failed: "+err.Error())
		return
	}
	if attemptType == "connection_probe" {
		service.handleBeginModelCall(ctx, envelope, begin)
		return
	}
	if attemptType != "initial_analysis" {
		service.rejectBegin(ctx, envelope, begin, "attempt type not handled")
		return
	}
	ack := &runtimev1.ControlEnvelope{
		ConnectionEpoch: envelope.GetConnectionEpoch(),
		CorrelationId:   envelope.GetCorrelationId(),
		BootId:          envelope.GetBootId(),
		Msg:             &runtimev1.ControlEnvelope_BeginModelCallAck{BeginModelCallAck: &runtimev1.BeginModelCallAck{AttemptId: begin.GetAttemptId()}},
	}
	reject := func(detail string) {
		ack.GetBeginModelCallAck().Accepted = false
		ack.GetBeginModelCallAck().Detail = detail
		_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
		sharedops.LogEvent("quoin", "error", "modelcall.agent_begin_rejected", detail)
	}
	if begin.GetOperation() != runtimev1.ModelOperation_MODEL_OPERATION_CHAT {
		reject("agent attempts only carry chat calls in this slice")
		return
	}
	var items []attempt.ModelInputItem
	for _, item := range begin.GetInputItems() {
		items = append(items, attempt.ModelInputItem{
			Sequence: item.GetSequence(), ItemKind: inputItemKindOf(item.GetItemKind()),
			ItemID: item.GetItemId(), ContentDigest: hex.EncodeToString(item.GetContentDigest()),
			Role: inputRoleOf(item.GetRole()),
		})
	}
	callID, err := service.Analyses.Attempts().BeginModelCall(ctx, attempt.BeginCall{
		AttemptID: begin.GetAttemptId(), CallSeq: int(begin.GetCallSeq()), RetrySeq: int(begin.GetRetrySeq()),
		ModelID: begin.GetModelId(), PromptDigest: hex.EncodeToString(begin.GetPromptDigest()),
		ToolSchemaDigest: hex.EncodeToString(begin.GetToolSchemaDigest()),
		InputDigest:      hex.EncodeToString(begin.GetInputDigest()), RenderedDigest: hex.EncodeToString(begin.GetRenderedRequestDigest()),
		InputItems: items, ContextBudget: int64(begin.GetContextBudgetTokens()), MaxOutput: int64(begin.GetMaxOutputTokens()),
		EstimatedInput: 0, EvictedTurns: int(begin.GetEvictedTurnCount()),
	})
	if err != nil {
		reject(err.Error())
		return
	}
	var grantID, revisionID, generationID int64
	if err := service.Analyses.DB().QueryRowContext(ctx, `
		SELECT id,connection_revision_id,credential_generation_id FROM attempt_connection_grants
		WHERE attempt_id=? AND purpose='chat_model' ORDER BY id LIMIT 1`, begin.GetAttemptId()).Scan(&grantID, &revisionID, &generationID); err != nil {
		reject(err.Error())
		return
	}
	ack.GetBeginModelCallAck().Accepted = true
	ack.GetBeginModelCallAck().ModelCallId = callID
	ack.GetBeginModelCallAck().ModelProviderGrant = &runtimev1.ConnectionGrant{
		GrantId: grantID, ConnectionRevisionId: revisionID, CredentialGenerationId: generationID, Purpose: "chat_model",
	}
	_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
}

func (service *RuntimeService) rejectBegin(ctx context.Context, envelope *runtimev1.ControlEnvelope, begin *runtimev1.BeginModelCall, detail string) {
	_ = service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{
		ConnectionEpoch: envelope.GetConnectionEpoch(),
		CorrelationId:   envelope.GetCorrelationId(),
		BootId:          envelope.GetBootId(),
		Msg:             &runtimev1.ControlEnvelope_BeginModelCallAck{BeginModelCallAck: &runtimev1.BeginModelCallAck{AttemptId: begin.GetAttemptId(), Accepted: false, Detail: detail}},
	})
}

// handleCompleteModelCallRouted seals the model call for the owning slice;
// agent completions create the pending tool_calls rows and answer the
// authorizations (ARCH-TOOL-001).
func (service *RuntimeService) handleCompleteModelCallRouted(ctx context.Context, envelope *runtimev1.ControlEnvelope, complete *runtimev1.CompleteModelCall) {
	attemptType, err := service.attemptTypeOf(ctx, complete.GetAttemptId())
	if err != nil {
		service.rejectComplete(ctx, envelope, complete, "attempt lookup failed: "+err.Error())
		return
	}
	if attemptType == "connection_probe" {
		service.handleCompleteModelCall(ctx, envelope, complete)
		return
	}
	if attemptType != "initial_analysis" {
		service.rejectComplete(ctx, envelope, complete, "attempt type not handled")
		return
	}
	ack := &runtimev1.ControlEnvelope{
		ConnectionEpoch: envelope.GetConnectionEpoch(),
		CorrelationId:   envelope.GetCorrelationId(),
		BootId:          envelope.GetBootId(),
		Msg:             &runtimev1.ControlEnvelope_CompleteModelCallAck{CompleteModelCallAck: &runtimev1.CompleteModelCallAck{AttemptId: complete.GetAttemptId(), ModelCallId: complete.GetModelCallId()}},
	}
	reject := func(detail string) {
		ack.GetCompleteModelCallAck().Accepted = false
		ack.GetCompleteModelCallAck().Detail = detail
		_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
		sharedops.LogEvent("quoin", "error", "modelcall.agent_complete_rejected", detail)
	}
	outcome := "succeeded"
	failureReason := ""
	switch complete.GetOutcome() {
	case runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_FAILED:
		outcome = "failed"
		failureReason = mapFailureReason(complete.GetFailureReason())
	case runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_CANCELLED:
		outcome = "cancelled"
		failureReason = "cancelled"
	case runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_SUCCEEDED:
	default:
		reject("outcome unspecified")
		return
	}
	var proposed []attempt.ProposedTool
	for _, tool := range complete.GetToolCalls() {
		proposed = append(proposed, attempt.ProposedTool{
			ProviderIndex: tool.GetProviderIndex(), ProviderToolCallID: tool.GetProviderToolCallId(),
			ToolName: tool.GetToolName(), ArgumentsJSON: tool.GetArgumentsJson(),
			ArgumentsDigest: hex.EncodeToString(tool.GetArgumentsDigest()),
		})
	}
	authorizations, err := service.Analyses.Attempts().CompleteModelCall(ctx, attempt.CompleteCall{
		AttemptID: complete.GetAttemptId(), CallID: complete.GetModelCallId(),
		Outcome: outcome, FailureReason: failureReason,
		ProviderRequestID: complete.GetProviderRequestId(), LatencyMS: int64(complete.GetLatencyMs()),
		InputTokens: int64(complete.GetInputTokens()), OutputTokens: int64(complete.GetOutputTokens()),
		TotalTokens: int64(complete.GetTotalTokens()), FinishReason: complete.GetFinishReason(),
		AssistantText: complete.GetAssistantText(), ProposedTools: proposed,
		ResponseDigest: hex.EncodeToString(complete.GetResponseDigest()), ResponseComplete: complete.GetResponseComplete(),
	})
	if err != nil {
		reject(err.Error())
		return
	}
	for _, authorization := range authorizations {
		ack.GetCompleteModelCallAck().ToolCalls = append(ack.GetCompleteModelCallAck().ToolCalls, &runtimev1.ToolCallAuthorization{
			ToolCallId: authorization.ToolCallID, ProviderIndex: authorization.ProviderIndex,
			ProviderToolCallId: authorization.ProviderToolCallID, FailureMode: failureModeOf(authorization.FailureMode),
		})
	}
	ack.GetCompleteModelCallAck().Accepted = true
	_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
}

func (service *RuntimeService) rejectComplete(ctx context.Context, envelope *runtimev1.ControlEnvelope, complete *runtimev1.CompleteModelCall, detail string) {
	_ = service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{
		ConnectionEpoch: envelope.GetConnectionEpoch(),
		CorrelationId:   envelope.GetCorrelationId(),
		BootId:          envelope.GetBootId(),
		Msg:             &runtimev1.ControlEnvelope_CompleteModelCallAck{CompleteModelCallAck: &runtimev1.CompleteModelCallAck{AttemptId: complete.GetAttemptId(), ModelCallId: complete.GetModelCallId(), Accepted: false, Detail: detail}},
	})
}

// handleBeginToolCallRouted moves one pending tool call to running behind
// the attempt fence (ARCH-TOOL-002).
func (service *RuntimeService) handleBeginToolCallRouted(ctx context.Context, envelope *runtimev1.ControlEnvelope, begin *runtimev1.BeginToolCall) {
	ack := &runtimev1.ControlEnvelope{
		ConnectionEpoch: envelope.GetConnectionEpoch(),
		CorrelationId:   envelope.GetCorrelationId(),
		BootId:          envelope.GetBootId(),
		Msg:             &runtimev1.ControlEnvelope_BeginToolCallAck{BeginToolCallAck: &runtimev1.BeginToolCallAck{AttemptId: begin.GetAttemptId(), ToolCallId: begin.GetToolCallId()}},
	}
	if err := service.Analyses.Attempts().BeginToolCall(ctx, begin.GetAttemptId(), begin.GetToolCallId()); err != nil {
		ack.GetBeginToolCallAck().Accepted = false
		ack.GetBeginToolCallAck().Detail = err.Error()
		sharedops.LogEvent("quoin", "error", "toolcall.begin_rejected", err.Error())
	} else {
		ack.GetBeginToolCallAck().Accepted = true
	}
	_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
}

// handleCompleteToolCallRouted seals one tool execution (ARCH-TOOL-003);
// the committed payload and artifact ref travel back to the worker.
func (service *RuntimeService) handleCompleteToolCallRouted(ctx context.Context, envelope *runtimev1.ControlEnvelope, complete *runtimev1.CompleteToolCall) {
	ack := &runtimev1.ControlEnvelope{
		ConnectionEpoch: envelope.GetConnectionEpoch(),
		CorrelationId:   envelope.GetCorrelationId(),
		BootId:          envelope.GetBootId(),
		Msg:             &runtimev1.ControlEnvelope_CompleteToolCallAck{CompleteToolCallAck: &runtimev1.CompleteToolCallAck{AttemptId: complete.GetAttemptId(), ToolCallId: complete.GetToolCallId()}},
	}
	reject := func(detail string) {
		ack.GetCompleteToolCallAck().Accepted = false
		ack.GetCompleteToolCallAck().Detail = detail
		_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
		sharedops.LogEvent("quoin", "error", "toolcall.complete_rejected", detail)
	}
	outcome := "succeeded"
	switch complete.GetOutcome() {
	case runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_FAILED:
		outcome = "failed"
	case runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_CANCELLED:
		outcome = "cancelled"
	case runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_SUCCEEDED:
	default:
		reject("outcome unspecified")
		return
	}
	payload := complete.GetPayload()
	var resultJSON string
	if payload != nil && len(payload.GetCanonicalJson()) > 0 {
		digest := sha256.Sum256(payload.GetCanonicalJson())
		if hex.EncodeToString(digest[:]) != hex.EncodeToString(payload.GetContentDigest()) {
			reject("result payload digest mismatch")
			return
		}
		resultJSON = string(payload.GetCanonicalJson())
	}
	err := service.Analyses.Attempts().CompleteToolCall(ctx, attempt.ToolResult{
		AttemptID: complete.GetAttemptId(), ToolCallID: complete.GetToolCallId(),
		Outcome: outcome, ResultJSON: resultJSON, ArtifactID: complete.GetArtifactId(),
		ErrorCode: complete.GetErrorCode(), ErrorDetail: complete.GetErrorDetail(),
	})
	if err != nil {
		reject(err.Error())
		return
	}
	ack.GetCompleteToolCallAck().Accepted = true
	ack.GetCompleteToolCallAck().EvidenceIds = nil
	if payload != nil {
		ack.GetCompleteToolCallAck().CommittedPayload = &runtimev1.ResultPayload{
			SchemaKind: payload.GetSchemaKind(), CanonicalJson: payload.GetCanonicalJson(),
			ContentDigest: payload.GetContentDigest(), EvidenceIds: nil, ArtifactIds: nil,
		}
	}
	if complete.GetArtifactId() != 0 {
		ref, err := service.Artifacts.RefFor(ctx, complete.GetAttemptId(), complete.GetArtifactId())
		if err != nil {
			reject(err.Error())
			return
		}
		ack.GetCompleteToolCallAck().ArtifactRef = &runtimev1.ArtifactRef{
			ArtifactId: ref.ArtifactID, Role: "tool_result", MediaType: ref.MediaType,
			SizeBytes: uint64(ref.SizeBytes), Sha256: ref.SHA256, BodyExpired: ref.BodyExpired,
		}
	}
	_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
}

// dispatchCancelRouted sends the committed cancellation fence to the
// runtime that owns the attempt (RUNTIME-CANCEL-001..003), routed by
// attempt type.
func (service *RuntimeService) dispatchCancelRouted(ctx context.Context, attemptID int64) error {
	attemptType, err := service.attemptTypeOf(ctx, attemptID)
	if err != nil {
		return err
	}
	if attemptType == "connection_probe" {
		return service.dispatchCancel(ctx, attemptID)
	}
	if attemptType != "initial_analysis" {
		return fmt.Errorf("attempt %d type %s has no cancel dispatcher", attemptID, attemptType)
	}
	var boot sql.NullString
	var epoch sql.NullInt64
	row := service.Analyses.DB().QueryRowContext(ctx, `SELECT boot_id,connection_epoch FROM execution_attempts WHERE id=?`, attemptID)
	if err := row.Scan(&boot, &epoch); err != nil {
		return err
	}
	if !boot.Valid || !epoch.Valid {
		// Unbound attempts finalize locally.
		return service.Analyses.CancelAck(ctx, attemptID)
	}
	return service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{
		ConnectionEpoch: uint64(epoch.Int64),
		CorrelationId:   uint64(attemptID),
		BootId:          boot.String,
		Msg:             &runtimev1.ControlEnvelope_CancelAttempt{CancelAttempt: &runtimev1.CancelAttempt{AttemptId: attemptID}},
	})
}
func inputItemKindOf(kind runtimev1.ModelInputItemKind) string {
	switch kind {
	case runtimev1.ModelInputItemKind_MODEL_INPUT_ITEM_KIND_ATTEMPT_INPUT_SNAPSHOT:
		return "snapshot"
	case runtimev1.ModelInputItemKind_MODEL_INPUT_ITEM_KIND_INVESTIGATION_MESSAGE:
		return "message"
	case runtimev1.ModelInputItemKind_MODEL_INPUT_ITEM_KIND_PRIOR_MODEL_CALL:
		return "prior_call"
	case runtimev1.ModelInputItemKind_MODEL_INPUT_ITEM_KIND_TOOL_CALL:
		return "tool_call"
	case runtimev1.ModelInputItemKind_MODEL_INPUT_ITEM_KIND_EVIDENCE:
		return "evidence"
	case runtimev1.ModelInputItemKind_MODEL_INPUT_ITEM_KIND_ARTIFACT:
		return "artifact"
	case runtimev1.ModelInputItemKind_MODEL_INPUT_ITEM_KIND_KNOWLEDGE_VERSION:
		return "knowledge"
	case runtimev1.ModelInputItemKind_MODEL_INPUT_ITEM_KIND_SYSTEM_CONTRACT:
		return "system_contract"
	case runtimev1.ModelInputItemKind_MODEL_INPUT_ITEM_KIND_TOOL_SCHEMA:
		return "tool_schema"
	default:
		return ""
	}
}

func inputRoleOf(role runtimev1.ModelInputRole) string {
	switch role {
	case runtimev1.ModelInputRole_MODEL_INPUT_ROLE_SYSTEM:
		return "system"
	case runtimev1.ModelInputRole_MODEL_INPUT_ROLE_USER:
		return "user"
	case runtimev1.ModelInputRole_MODEL_INPUT_ROLE_ASSISTANT:
		return "assistant"
	case runtimev1.ModelInputRole_MODEL_INPUT_ROLE_TOOL:
		return "tool"
	default:
		return ""
	}
}

func failureModeOf(mode string) runtimev1.ToolFailureMode {
	if mode == "fail_attempt" {
		return runtimev1.ToolFailureMode_TOOL_FAILURE_MODE_FAIL_ATTEMPT
	}
	return runtimev1.ToolFailureMode_TOOL_FAILURE_MODE_RETURN_TO_MODEL
}

func terminationReasonOf(reason runtimev1.TerminationReason) string {
	switch reason {
	case runtimev1.TerminationReason_TERMINATION_REASON_TIMEOUT:
		return "timeout"
	case runtimev1.TerminationReason_TERMINATION_REASON_RATE_LIMITED:
		return "rate_limited"
	case runtimev1.TerminationReason_TERMINATION_REASON_PROVIDER_UNAVAILABLE:
		return "provider_unavailable"
	case runtimev1.TerminationReason_TERMINATION_REASON_INVALID_RESPONSE:
		return "invalid_response"
	case runtimev1.TerminationReason_TERMINATION_REASON_TOOL_ERROR:
		return "tool_error"
	case runtimev1.TerminationReason_TERMINATION_REASON_ARTIFACT_COMMIT_FAILED:
		return "artifact_commit_failed"
	case runtimev1.TerminationReason_TERMINATION_REASON_CANCELLED:
		return "cancelled"
	case runtimev1.TerminationReason_TERMINATION_REASON_CONNECTION_DISABLED:
		return "connection_disabled"
	case runtimev1.TerminationReason_TERMINATION_REASON_BUSINESS_SYSTEM_DISABLED:
		return "business_system_disabled"
	case runtimev1.TerminationReason_TERMINATION_REASON_LEASE_EXPIRED:
		return "lease_expired"
	case runtimev1.TerminationReason_TERMINATION_REASON_REPLACED:
		return "replaced"
	case runtimev1.TerminationReason_TERMINATION_REASON_REVOKED:
		return "revoked"
	case runtimev1.TerminationReason_TERMINATION_REASON_CONTEXT_TOO_LARGE:
		return "context_too_large"
	case runtimev1.TerminationReason_TERMINATION_REASON_ARTIFACT_BODY_EXPIRED:
		return "artifact_body_expired"
	case runtimev1.TerminationReason_TERMINATION_REASON_SANDBOX_UNAVAILABLE:
		return "sandbox_unavailable"
	case runtimev1.TerminationReason_TERMINATION_REASON_WORKER_PROTOCOL_ERROR:
		return "worker_protocol_error"
	default:
		return ""
	}
}
