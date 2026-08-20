package app

// Model-call ledger slice of the control stream (T08): Begin/Complete
// ModelCall for connection_probe qualification runs, with the frozen Ack
// envelope shapes. The probe grants arrive with the dispatch input; the
// Ack returns the grant id the supervisor fetches credentials through.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"

	"github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/Suknna/quoin/internal/quoin/connections/modelprovider"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
)

// handleBeginModelCall opens one running ledger row and returns its id with
// the model grant reference (RUNTIME-MODEL ledger).
func (service *RuntimeService) handleBeginModelCall(ctx context.Context, envelope *runtimev1.ControlEnvelope, begin *runtimev1.BeginModelCall) {
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
		sharedops.LogEvent("quoin", "error", "modelcall.begin_rejected", detail)
	}
	if service.Connections == nil {
		reject("connections not wired")
		return
	}
	operation := "chat"
	if begin.GetOperation() == runtimev1.ModelOperation_MODEL_OPERATION_EMBEDDING {
		operation = "embedding"
	}
	// The probe attempt carries model_probe_chat + model_probe_embedding
	// grants; pick the one matching this operation.
	purpose := "model_probe_chat"
	if operation == "embedding" {
		purpose = "model_probe_embedding"
	}
	var grantID int64
	err := service.Connections.DB().QueryRowContext(ctx, `SELECT id FROM attempt_connection_grants WHERE attempt_id=? AND purpose=? ORDER BY id LIMIT 1`, begin.GetAttemptId(), purpose).Scan(&grantID)
	if err != nil {
		reject(fmt.Sprintf("probe grant missing for %s: %v", purpose, err))
		return
	}
	// The chat probe exposes the frozen probe tool schemas; digests are the
	// SHA-256 of the fixed bodies carried by the frozen catalog.
	toolDigest := ""
	if operation == "chat" {
		sum := sha256.Sum256([]byte(modelprovider.ProbeRendererVersion + ":" + "fixed-probe-tools-v1"))
		toolDigest = hex.EncodeToString(sum[:])
	}
	// Digests travel as 64-char hex text (the driver hex-encodes once).
	inputDigest := string(begin.GetInputDigest())
	renderedDigest := string(begin.GetRenderedRequestDigest())
	promptDigest := string(begin.GetPromptDigest())
	if operation == "embedding" {
		promptDigest = ""
		toolDigest = ""
	}
	db := service.Connections.DB()
	callID, err := modelprovider.Begin(ctx, db, begin.GetAttemptId(), grantID, int(begin.GetCallSeq()), int(begin.GetRetrySeq()), operation, begin.GetModelId(), promptDigest, toolDigest, inputDigest, renderedDigest, int64(begin.GetContextBudgetTokens()), int64(begin.GetMaxOutputTokens()), 0, int(begin.GetEvictedTurnCount()))
	if err != nil {
		reject(err.Error())
		return
	}
	// Input lineage (trg_model_call_success_input): every succeeded call
	// needs persisted items — chat carries the frozen system contract and
	// tool schema synthetics, embedding carries the attempt snapshot.
	if err := modelprovider.WriteInputLineage(ctx, db, callID, operation, promptDigest, toolDigest, begin.GetAttemptId()); err != nil {
		reject(err.Error())
		return
	}
	var revisionID, generationID int64
	var connectionID int64
	if err := service.Connections.DB().QueryRowContext(ctx, `SELECT connection_id,connection_revision_id,credential_generation_id FROM attempt_connection_grants WHERE id=?`, grantID).Scan(&connectionID, &revisionID, &generationID); err != nil {
		reject(err.Error())
		return
	}
	ack.GetBeginModelCallAck().Accepted = true
	ack.GetBeginModelCallAck().ModelCallId = callID
	ack.GetBeginModelCallAck().ModelProviderGrant = &runtimev1.ConnectionGrant{
		GrantId: grantID, ConnectionRevisionId: revisionID, CredentialGenerationId: generationID, Purpose: purpose,
	}
	_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
}

// handleCompleteModelCall seals the ledger row and the canonical output.
func (service *RuntimeService) handleCompleteModelCall(ctx context.Context, envelope *runtimev1.ControlEnvelope, complete *runtimev1.CompleteModelCall) {
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
		sharedops.LogEvent("quoin", "error", "modelcall.complete_rejected", detail)
	}
	if service.Connections == nil {
		reject("connections not wired")
		return
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
	responseJSON, responseDigest := "{}", ""
	if complete.GetOutcome() == runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_SUCCEEDED {
		responseDigest = hex.EncodeToString(complete.GetResponseDigest())
		// The frozen output shape requires a tool_calls array (possibly
		// empty) on every chat response.
		body := map[string]any{"assistantText": complete.GetAssistantText(), "finishReason": complete.GetFinishReason(), "tool_calls": []any{}}
		if len(complete.GetEmbeddingVectors()) > 0 {
			body["embeddingVectors"] = len(complete.GetEmbeddingVectors())
		}
		encoded, err := jsonMarshal(body)
		if err != nil {
			reject(err.Error())
			return
		}
		responseJSON = encoded
	}
	completion := modelprovider.Completion{
		Outcome: outcome, FailureReason: failureReason,
		ProviderRequestID: complete.GetProviderRequestId(),
		LatencyMS:         int64(complete.GetLatencyMs()),
		InputTokens:       int64(complete.GetInputTokens()),
		OutputTokens:      int64(complete.GetOutputTokens()),
		TotalTokens:       int64(complete.GetTotalTokens()),
		FinishReason:      complete.GetFinishReason(),
		ResponseJSON:      responseJSON, ResponseDigest: responseDigest,
		ResponseComplete: complete.GetResponseComplete(),
	}
	if err := modelprovider.Complete(ctx, service.Connections.DB(), complete.GetAttemptId(), complete.GetModelCallId(), completion); err != nil {
		reject(err.Error())
		return
	}
	ack.GetCompleteModelCallAck().Accepted = true
	_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
}

func mapFailureReason(reason runtimev1.ModelCallFailureReason) string {
	switch reason {
	case runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_TIMEOUT:
		return "timeout"
	case runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_RATE_LIMITED:
		return "rate_limited"
	case runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_PROVIDER_UNAVAILABLE:
		return "provider_unavailable"
	case runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_CONTEXT_OVERFLOW:
		return "context_overflow"
	case runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_INVALID_RESPONSE:
		return "invalid_response"
	case runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_CANCELLED:
		return "cancelled"
	case runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_TRANSPORT_ERROR:
		return "transport_error"
	default:
		return "invalid_response"
	}
}

var _ = sql.ErrNoRows
var _ = connections.TypeModelProvider
