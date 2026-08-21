package app

// Initial Analysis runtime slice (T10): dispatch of agent attempts to the
// live Plinth stream, envelope adjudication on the way back (accept,
// result proposal, cancel ack), the agent model/tool call ledger handlers
// and the ArtifactService gRPC surface (Upload/ReadText/GrepText).

import (
	"context"
	"errors"
	"fmt"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/analysis"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// analysisLeaseWindow returns the dispatch lease window for agent
// attempts: attempt.DispatchLease is the single frozen authority
// (RUNTIME-SCOPE-004).
func analysisLeaseWindow() time.Duration {
	return attempt.DispatchLease
}

// dispatchAnalysisAttempt binds one Queued agent attempt to the live
// Plinth stream and sends the DispatchAttempt frame (RUNTIME-TASK-001/002).
func (service *RuntimeService) dispatchAnalysisAttempt(ctx context.Context, attemptID int64) error {
	view, err := service.Slots.View(ctx, qruntime.SlotPlinth)
	if err != nil {
		return err
	}
	if !view.Connected || view.ConnectionEpoch == nil {
		return fmt.Errorf("plinth is not connected")
	}
	if err := service.Analyses.Attempts().BindToStream(ctx, attemptID, view.BootID, *view.ConnectionEpoch, analysisLeaseWindow()); err != nil {
		return err
	}
	input, err := service.Analyses.Attempts().DispatchInputFor(ctx, attemptID)
	if err != nil {
		return err
	}
	var scopeID int64
	if err := service.Analyses.DB().QueryRowContext(ctx, `SELECT scope_id FROM execution_attempts WHERE id=?`, attemptID).Scan(&scopeID); err != nil {
		return err
	}
	var artifactRefs []*runtimev1.ArtifactRef
	for _, ref := range input.ArtifactRefs {
		artifactRefs = append(artifactRefs, &runtimev1.ArtifactRef{
			ArtifactId: ref.ArtifactID, Role: ref.Role, MediaType: ref.MediaType,
			SizeBytes: uint64(ref.SizeBytes), Sha256: ref.SHA256, BodyExpired: ref.BodyExpired,
		})
	}
	var grants []*runtimev1.ConnectionGrant
	for _, grant := range input.Grants {
		grants = append(grants, &runtimev1.ConnectionGrant{
			GrantId: grant.GrantID, ConnectionRevisionId: grant.ConnectionRevisionID,
			CredentialGenerationId: grant.CredentialGenerationID, Purpose: grant.Purpose,
			ConnectionProbeResultId: grant.ConnectionProbeResultID,
		})
	}
	envelope := &runtimev1.ControlEnvelope{
		ConnectionEpoch: *view.ConnectionEpoch,
		CorrelationId:   uint64(attemptID),
		BootId:          view.BootID,
		Msg: &runtimev1.ControlEnvelope_DispatchAttempt{
			DispatchAttempt: &runtimev1.DispatchAttempt{
				AttemptId:     attemptID,
				AttemptType:   runtimev1.AttemptType_ATTEMPT_TYPE_INITIAL_ANALYSIS,
				ScopeType:     runtimev1.ScopeType_SCOPE_TYPE_ANALYSIS,
				ScopeId:       scopeID,
				LeaseDeadline: timestamppb.New(time.Now().UTC().Add(analysisLeaseWindow())),
				Input: &runtimev1.AttemptInputSnapshot{
					SchemaKind:       input.SchemaKind,
					CanonicalJson:    input.CanonicalJSON,
					ContentDigest:    input.ContentDigest,
					ArtifactRefs:     artifactRefs,
					ConnectionGrants: grants,
					AgentVersion:     input.AgentVersion,
				},
			},
		},
	}
	return service.sendEnvelope(qruntime.SlotPlinth, envelope)
}

// dispatchQueuedAnalyses binds and dispatches every Queued agent attempt
// after a Plinth stream attaches (created while disconnected).
func (service *RuntimeService) dispatchQueuedAnalyses(ctx context.Context) {
	if service.Analyses == nil {
		return
	}
	view, err := service.Slots.View(ctx, qruntime.SlotPlinth)
	if err != nil || !view.Connected || view.ConnectionEpoch == nil {
		return
	}
	ids, err := service.Analyses.Attempts().QueuedAgentAttempts(ctx, "initial_analysis")
	if err != nil {
		sharedops.LogEvent("quoin", "error", "analysis.queue_scan", err.Error())
		return
	}
	for _, id := range ids {
		if err := service.dispatchAnalysisAttempt(ctx, id); err != nil {
			sharedops.LogEvent("quoin", "error", "analysis.queue_dispatch", err.Error())
		}
	}
}

// handleAttemptAcceptRouted records Assigned -> Running for whichever task
// slice owns the attempt (RUNTIME-TASK-004).
func (service *RuntimeService) handleAttemptAcceptRouted(ctx context.Context, envelope *runtimev1.ControlEnvelope, accept *runtimev1.AttemptAccept) {
	attemptType, err := service.attemptTypeOf(ctx, accept.GetAttemptId())
	if err != nil {
		sharedops.LogEvent("quoin", "error", "accept.lookup_failed", err.Error())
		return
	}
	switch attemptType {
	case "initial_analysis":
		if err := service.Analyses.AcceptAttempt(ctx, accept.GetAttemptId(), envelope.GetBootId(), envelope.GetConnectionEpoch()); err != nil {
			sharedops.LogEvent("quoin", "error", "analysis.accept_failed", err.Error())
		}
	case "investigation":
		if service.Investigations != nil {
			if err := service.Investigations.AcceptAttempt(ctx, accept.GetAttemptId(), envelope.GetBootId(), envelope.GetConnectionEpoch()); err != nil {
				sharedops.LogEvent("quoin", "error", "investigation.accept_failed", err.Error())
			}
		}
	case "connection_probe":
		if service.Connections != nil {
			if err := service.Connections.AcceptProbe(ctx, accept.GetAttemptId(), envelope.GetBootId(), envelope.GetConnectionEpoch()); err != nil {
				sharedops.LogEvent("quoin", "error", "probe.accept_failed", err.Error())
			}
		}
	default:
		sharedops.LogEvent("quoin", "info", "accept.unhandled_type", attemptType)
	}
}

// handleResultProposalRouted adjudicates a result by attempt type; agent
// attempts seal through the analysis aggregate (DATA-ANALYSIS-002), probes
// keep the T07 closure.
func (service *RuntimeService) handleResultProposalRouted(ctx context.Context, envelope *runtimev1.ControlEnvelope, proposal *runtimev1.ResultProposal) {
	attemptType, err := service.attemptTypeOf(ctx, proposal.GetAttemptId())
	if err != nil {
		sharedops.LogEvent("quoin", "error", "result.lookup_failed", err.Error())
		return
	}
	if attemptType == "connection_probe" {
		service.handleResultProposal(ctx, envelope, proposal)
		return
	}
	if attemptType == "investigation" {
		if service.InvestigationRuntime != nil {
			service.InvestigationRuntime.HandleResultProposal(ctx, envelope, proposal)
		} else {
			sharedops.LogEvent("quoin", "info", "result.investigation_unwired", "")
		}
		return
	}
	if attemptType != "initial_analysis" {
		sharedops.LogEvent("quoin", "info", "result.unhandled_type", attemptType)
		return
	}
	ack := &runtimev1.ControlEnvelope{
		ConnectionEpoch: envelope.GetConnectionEpoch(),
		CorrelationId:   envelope.GetCorrelationId(),
		BootId:          envelope.GetBootId(),
		Msg:             &runtimev1.ControlEnvelope_ResultAck{ResultAck: &runtimev1.ResultAck{AttemptId: proposal.GetAttemptId()}},
	}
	reject := func(reason string) {
		ack.GetResultAck().Accepted = false
		ack.GetResultAck().Detail = reason
		_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
		sharedops.LogEvent("quoin", "error", "analysis.result_rejected", fmt.Sprintf("attempt=%d reason=%s", proposal.GetAttemptId(), reason))
	}
	payload := proposal.GetPayload()
	if payload == nil || payload.GetSchemaKind() == "" || len(payload.GetCanonicalJson()) == 0 {
		reject("result payload incomplete")
		return
	}
	succeeded := proposal.GetOutcome() == runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED
	termination := ""
	if !succeeded {
		termination = terminationReasonOf(proposal.GetTerminationReason())
		if termination == "" {
			reject("failed outcome requires a termination reason")
			return
		}
	}
	err = service.Analyses.CommitResult(ctx, analysis.Result{
		AttemptID: proposal.GetAttemptId(), BootID: proposal.GetBootId(),
		Epoch: proposal.GetConnectionEpoch(), Succeeded: succeeded, Termination: termination,
		SchemaKind: payload.GetSchemaKind(), Canonical: payload.GetCanonicalJson(),
		Digest: payload.GetContentDigest(), EvidenceIDs: payload.GetEvidenceIds(), ArtifactIDs: payload.GetArtifactIds(),
	})
	if err != nil {
		if errors.Is(err, analysis.ErrLateResult) || errors.Is(err, attempt.ErrLateResult) {
			reject("late result rejected")
			return
		}
		reject(err.Error())
		return
	}
	ack.GetResultAck().Accepted = true
	_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
	sharedops.LogEvent("quoin", "info", "analysis.result_committed", fmt.Sprintf("attempt=%d succeeded=%v", proposal.GetAttemptId(), succeeded))
}

// handleCancelAckRouted finishes Cancelling -> Cancelled for the owning
// slice (RUNTIME-CANCEL-003).
func (service *RuntimeService) handleCancelAckRouted(ctx context.Context, ack *runtimev1.CancelAck) {
	attemptType, err := service.attemptTypeOf(ctx, ack.GetAttemptId())
	if err != nil {
		sharedops.LogEvent("quoin", "error", "cancel_ack.lookup_failed", err.Error())
		return
	}
	switch attemptType {
	case "initial_analysis":
		if err := service.Analyses.CancelAck(ctx, ack.GetAttemptId()); err != nil {
			sharedops.LogEvent("quoin", "error", "analysis.cancel_ack", err.Error())
		}
	case "investigation":
		if service.Investigations != nil {
			if err := service.Investigations.CancelAck(ctx, ack.GetAttemptId()); err != nil {
				sharedops.LogEvent("quoin", "error", "investigation.cancel_ack", err.Error())
			}
		}
	case "connection_probe":
		if service.Connections != nil {
			if err := service.Connections.RecordCancelAck(ctx, ack.GetAttemptId()); err != nil {
				sharedops.LogEvent("quoin", "error", "probe.cancel_ack", err.Error())
			}
		}
	default:
		sharedops.LogEvent("quoin", "info", "cancel_ack.unhandled_type", attemptType)
	}
}

// attemptTypeOf returns the durable attempt type for envelope routing.
func (service *RuntimeService) attemptTypeOf(ctx context.Context, attemptID int64) (string, error) {
	if service.Analyses == nil {
		return "", errors.New("analysis service not wired")
	}
	var attemptType string
	err := service.Analyses.DB().QueryRowContext(ctx, `SELECT attempt_type FROM execution_attempts WHERE id=?`, attemptID).Scan(&attemptType)
	return attemptType, err
}
