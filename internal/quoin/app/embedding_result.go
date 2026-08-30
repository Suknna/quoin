package app

// embedding_result.go: the embedding ResultProposal adjudication slice. The
// supervisor's closed payload carries either a generation rebuild batch or a
// query embedding; both commit through the projection authority, which
// re-verifies binding, digests and dimensions before any write and drops
// late results with audit only (DATA-TX-012).

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/knowledge/embedding"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
)

// handleEmbeddingResultProposal verifies transport integrity and hands the
// payload to the projection authority; it always answers the one-shot fence.
func (service *RuntimeService) handleEmbeddingResultProposal(ctx context.Context, envelope *runtimev1.ControlEnvelope, proposal *runtimev1.ResultProposal) {
	ack := &runtimev1.ControlEnvelope{ConnectionEpoch: envelope.GetConnectionEpoch(), CorrelationId: envelope.GetCorrelationId(), BootId: envelope.GetBootId(), Msg: &runtimev1.ControlEnvelope_ResultAck{ResultAck: &runtimev1.ResultAck{AttemptId: proposal.GetAttemptId()}}}
	reject := func(reason string) {
		ack.GetResultAck().Accepted = false
		ack.GetResultAck().Detail = reason
		_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
		sharedops.LogEvent("quoin", "error", "embedding.result_rejected", fmt.Sprintf("attempt=%d reason=%s", proposal.GetAttemptId(), reason))
	}
	if service.Knowledge == nil {
		reject("knowledge service not wired")
		return
	}
	var err error
	if proposal.GetOutcome() == runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED {
		payload := proposal.GetPayload()
		if payload == nil || len(payload.GetCanonicalJson()) == 0 {
			reject("embedding payload incomplete")
			return
		}
		if payload.GetSchemaKind() != embedding.ResultSchemaKind && payload.GetSchemaKind() != embedding.QuerySchemaKind {
			reject("embedding payload schema kind unknown")
			return
		}
		sum := sha256.Sum256(payload.GetCanonicalJson())
		if string(sum[:]) != string(payload.GetContentDigest()) {
			reject("embedding payload digest mismatch")
			return
		}
		err = service.Knowledge.Embeddings().CommitResult(ctx, proposal.GetAttemptId(), proposal.GetBootId(), proposal.GetConnectionEpoch(), payload.GetCanonicalJson())
		if err != nil && errors.Is(err, embedding.ErrInvalidResult) {
			// A closed-contract violation is a failed attempt, never a retry.
			if failErr := service.Knowledge.Embeddings().Fail(ctx, proposal.GetAttemptId(), proposal.GetBootId(), proposal.GetConnectionEpoch(), "invalid_response"); failErr != nil {
				reject(failErr.Error())
				return
			}
			ack.GetResultAck().Accepted = true
			_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
			return
		}
	} else {
		reason := terminationReasonOf(proposal.GetTerminationReason())
		if reason == "" {
			reject("failed outcome requires a termination reason")
			return
		}
		err = service.Knowledge.Embeddings().Fail(ctx, proposal.GetAttemptId(), proposal.GetBootId(), proposal.GetConnectionEpoch(), reason)
	}
	if err != nil {
		if errors.Is(err, attempt.ErrLateResult) {
			reject("late result rejected")
		} else {
			// Transient authority failures keep the worker's durable
			// pending-result path retrying.
			sharedops.LogEvent("quoin", "error", "embedding.result_commit_failed", err.Error())
		}
		return
	}
	ack.GetResultAck().Accepted = true
	_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
	// A fresh generation may have settled: reconcile the projection so the
	// next batch (or nothing left to do) converges immediately.
	_ = service.Knowledge.Embeddings().Sweep(ctx)
}
