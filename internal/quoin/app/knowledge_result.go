package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/knowledge"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
)

// handleKnowledgeExtractionResultProposal is the model-output authority. It
// verifies transport integrity, lets the domain atomically publish only human
// drafts, and always answers the worker's one-shot proposal fence.
func (service *RuntimeService) handleKnowledgeExtractionResultProposal(ctx context.Context, envelope *runtimev1.ControlEnvelope, proposal *runtimev1.ResultProposal) {
	ack := &runtimev1.ControlEnvelope{ConnectionEpoch: envelope.GetConnectionEpoch(), CorrelationId: envelope.GetCorrelationId(), BootId: envelope.GetBootId(), Msg: &runtimev1.ControlEnvelope_ResultAck{ResultAck: &runtimev1.ResultAck{AttemptId: proposal.GetAttemptId()}}}
	reject := func(reason string) {
		ack.GetResultAck().Accepted = false
		ack.GetResultAck().Detail = reason
		_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
		sharedops.LogEvent("quoin", "error", "knowledge.result_rejected", fmt.Sprintf("attempt=%d reason=%s", proposal.GetAttemptId(), reason))
	}
	if service.Knowledge == nil {
		reject("knowledge service not wired")
		return
	}
	var err error
	if proposal.GetOutcome() == runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED {
		payload := proposal.GetPayload()
		if payload == nil || payload.GetSchemaKind() != "knowledge_extraction_result_v1" || len(payload.GetCanonicalJson()) == 0 {
			reject("knowledge extraction payload incomplete")
			return
		}
		sum := sha256.Sum256(payload.GetCanonicalJson())
		if string(sum[:]) != string(payload.GetContentDigest()) {
			reject("knowledge extraction payload digest mismatch")
			return
		}
		err = service.Knowledge.CommitExtraction(ctx, proposal.GetAttemptId(), proposal.GetBootId(), proposal.GetConnectionEpoch(), payload.GetCanonicalJson())
	} else {
		reason := terminationReasonOf(proposal.GetTerminationReason())
		if reason == "" {
			reject("failed outcome requires a termination reason")
			return
		}
		err = service.Knowledge.FailExtraction(ctx, proposal.GetAttemptId(), proposal.GetBootId(), proposal.GetConnectionEpoch(), reason)
	}
	if err != nil {
		if errors.Is(err, attempt.ErrLateResult) {
			reject("late result rejected")
		} else if errors.Is(err, knowledge.ErrInvalidExtraction) {
			if failErr := service.Knowledge.FailExtraction(ctx, proposal.GetAttemptId(), proposal.GetBootId(), proposal.GetConnectionEpoch(), "invalid_response"); failErr != nil {
				reject(failErr.Error())
				return
			}
			ack.GetResultAck().Accepted = true
			_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
		} else {
			// Do not ACK a transient authority failure: the worker's durable
			// pending-result path must retain and retry the proposal.
			sharedops.LogEvent("quoin", "error", "knowledge.result_commit_failed", err.Error())
		}
		return
	}
	ack.GetResultAck().Accepted = true
	_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
}
