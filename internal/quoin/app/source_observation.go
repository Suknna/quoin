// Source observation result adjudication (ADR-0004, ADR-0011): observation_run
// children execute locally through the metrics_discover internal tool
// (local_execution.go); this file owns the source_observation_result_v1
// ResultProposal adjudication boundary — digest verification, transactional
// commit via observation.CommitProposal, then the ResultAck. New results flow
// directly into CommitProposal from the local executor; the frame path stays
// for protocol completeness.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/observation"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
)

// handleSourceObservationResultProposal adjudicates the sealed
// source_observation_result_v1 payload: digest check, transactional commit
// with evidence and identity projection, then the ResultAck. The commit
// fences on the proposal's own frozen (boot, epoch) dispatch binding; the ack
// echoes the receiving envelope's epoch so stream frames stay matched.
func (service *RuntimeService) handleSourceObservationResultProposal(ctx context.Context, envelope *runtimev1.ControlEnvelope, proposal *runtimev1.ResultProposal) {
	ack := &runtimev1.ControlEnvelope{ConnectionEpoch: envelope.GetConnectionEpoch(), CorrelationId: envelope.GetCorrelationId(), BootId: envelope.GetBootId(), Msg: &runtimev1.ControlEnvelope_ResultAck{ResultAck: &runtimev1.ResultAck{AttemptId: proposal.GetAttemptId()}}}
	reject := func(detail string) {
		ack.GetResultAck().Accepted = false
		ack.GetResultAck().Detail = detail
		_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
		sharedops.LogEvent("quoin", "error", "source_observation.result_rejected", fmt.Sprintf("attempt=%d detail=%s", proposal.GetAttemptId(), detail))
	}
	if service.Observations == nil {
		reject("source observation is not wired")
		return
	}
	payload := proposal.GetPayload()
	if payload == nil || payload.GetSchemaKind() != "source_observation_result_v1" || len(payload.GetCanonicalJson()) == 0 {
		reject("expected source_observation_result_v1 payload")
		return
	}
	digest := sha256.Sum256(payload.GetCanonicalJson())
	if hex.EncodeToString(digest[:]) != hex.EncodeToString(payload.GetContentDigest()) {
		reject("content digest mismatch")
		return
	}
	if err := service.Observations.CommitProposal(ctx, proposal.GetAttemptId(), proposal.GetBootId(), proposal.GetConnectionEpoch(), payload.GetCanonicalJson()); err != nil {
		// Envelope/adjudication errors are static, bounded phrases; anything
		// else (storage, driver, trigger text) is logged with full detail
		// internally and reduced to a non-secret code on the wire so no
		// upstream or SQL text can leak back to the runtime.
		sharedops.LogEvent("quoin", "error", "source_observation.commit_failed", fmt.Sprintf("attempt=%d err=%v", proposal.GetAttemptId(), err))
		if errors.Is(err, observation.ErrNotFound) || errors.Is(err, observation.ErrNotObservable) ||
			errors.Is(err, attempt.ErrLateResult) {
			reject("source observation result does not close onto its frozen attempt")
			return
		}
		reject("source observation result replay conflicts or is not adjudicable")
		return
	}
	ack.GetResultAck().Accepted = true
	_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
}
