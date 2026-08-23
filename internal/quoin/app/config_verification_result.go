package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
)

func (service *RuntimeService) handleVerificationResultProposal(ctx context.Context, envelope *runtimev1.ControlEnvelope, proposal *runtimev1.ResultProposal) {
	ack := &runtimev1.ControlEnvelope{
		ConnectionEpoch: envelope.GetConnectionEpoch(), CorrelationId: envelope.GetCorrelationId(), BootId: envelope.GetBootId(),
		Msg: &runtimev1.ControlEnvelope_ResultAck{ResultAck: &runtimev1.ResultAck{AttemptId: proposal.GetAttemptId()}},
	}
	reject := func(reason string) {
		ack.GetResultAck().Accepted, ack.GetResultAck().Detail = false, reason
		_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
		sharedops.LogEvent("quoin", "error", "config_verification.result_rejected", fmt.Sprintf("attempt=%d reason=%s", proposal.GetAttemptId(), reason))
	}
	if service.BusinessSystems == nil {
		reject("business systems are not wired")
		return
	}
	payload := proposal.GetPayload()
	if payload == nil || payload.GetSchemaKind() != "config_promql_result_v1" || len(payload.GetCanonicalJson()) == 0 {
		reject("expected config_promql_result_v1 payload")
		return
	}
	digest := sha256.Sum256(payload.GetCanonicalJson())
	if hex.EncodeToString(digest[:]) != hex.EncodeToString(payload.GetContentDigest()) {
		reject("content digest mismatch")
		return
	}
	if err := service.BusinessSystems.CommitVerificationProposal(ctx, proposal.GetAttemptId(), proposal.GetBootId(), proposal.GetConnectionEpoch(), payload.GetCanonicalJson()); err != nil {
		reject(err.Error())
		return
	}
	ack.GetResultAck().Accepted = true
	_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
}
