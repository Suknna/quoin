package app

// Manual Inspection runtime slice (T24, CFG-INSPECTRUN-001/002, ADR-0011):
// run_check collection children execute locally (local_execution.go) through
// the metrics_collect internal tool; only inspection_analysis report attempts
// still dispatch to the live Plinth stream. This file carries that dispatch,
// the local cancellation convergence of collection children, and the
// ResultProposal adjudication boundary for inspection_promql_result_v1 /
// inspection_plugin_result_v1 / inspection_report_result_v1.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/inspection"
	"github.com/Suknna/quoin/internal/quoin/inspection/scheduler"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// dispatchInspectionAnalysis binds one Queued inspection_analysis attempt to
// the live Plinth stream as an agent attempt.
func (service *RuntimeService) dispatchInspectionAnalysis(ctx context.Context, attemptID int64) error {
	if service.Inspections == nil {
		return fmt.Errorf("inspections are not wired")
	}
	view, err := service.Slots.View(ctx, qruntime.SlotPlinth)
	if err != nil {
		return err
	}
	if !view.Connected || view.ConnectionEpoch == nil {
		return fmt.Errorf("plinth is not connected")
	}
	attempts := service.Inspections.Attempts()
	if err := attempts.BindToStream(ctx, attemptID, view.BootID, *view.ConnectionEpoch, attempt.DispatchLease, view.ReleaseVersion); err != nil {
		return err
	}
	input, err := attempts.DispatchInputFor(ctx, attemptID)
	if err != nil {
		return err
	}
	var scopeID int64
	if err := service.Inspections.Reader().QueryRowContext(ctx, `SELECT scope_id FROM execution_attempts WHERE id=?`, attemptID).Scan(&scopeID); err != nil {
		return err
	}
	operationCorrelationID, err := dispatchOperationCorrelation(ctx, service.Inspections.Reader(), attemptID)
	if err != nil {
		return err
	}
	var grants []*runtimev1.ConnectionGrant
	for _, grant := range input.Grants {
		grants = append(grants, &runtimev1.ConnectionGrant{
			GrantId: grant.GrantID, ConnectionRevisionId: grant.ConnectionRevisionID,
			CredentialGenerationId: grant.CredentialGenerationID, Purpose: grant.Purpose,
			ConnectionProbeResultId: grant.ConnectionProbeResultID,
		})
	}
	return service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{
		ConnectionEpoch: *view.ConnectionEpoch,
		CorrelationId:   uint64(attemptID),
		BootId:          view.BootID,
		Msg: &runtimev1.ControlEnvelope_DispatchAttempt{DispatchAttempt: &runtimev1.DispatchAttempt{
			AttemptId: attemptID, AttemptType: runtimev1.AttemptType_ATTEMPT_TYPE_INSPECTION_ANALYSIS,
			ScopeType: runtimev1.ScopeType_SCOPE_TYPE_RUN, ScopeId: scopeID, OperationCorrelationId: operationCorrelationID,
			LeaseDeadline: timestamppb.New(time.Now().UTC().Add(attempt.DispatchLease)),
			Input:         &runtimev1.AttemptInputSnapshot{SchemaKind: input.SchemaKind, CanonicalJson: input.CanonicalJSON, ContentDigest: input.ContentDigest, ConnectionGrants: grants, AgentVersion: input.AgentVersion},
		}},
	})
}

// dispatchInspectionCancellation routes an already-committed inspection fence
// (ADR-0011): report analysis runs on Plinth and keeps the CancelAttempt
// frame; collection children execute locally, so their fence converges to
// Cancelled in-process without a runtime round trip.
func (service *RuntimeService) dispatchInspectionCancellation(ctx context.Context, attemptID int64) error {
	if service.Inspections == nil {
		return fmt.Errorf("inspections are not wired")
	}
	var attemptType string
	if err := service.Inspections.Reader().QueryRowContext(ctx, `SELECT attempt_type FROM execution_attempts WHERE id=?`, attemptID).Scan(&attemptType); err != nil {
		return err
	}
	if attemptType == "inspection_collection" {
		service.finalizeCancellation(ctx, attemptID, attemptType)
		return nil
	}
	if attemptType != "inspection_analysis" {
		return fmt.Errorf("attempt %d is not an inspection cancellation target", attemptID)
	}
	view, err := service.Slots.View(ctx, qruntime.SlotPlinth)
	if err != nil {
		return err
	}
	if !view.Connected || view.ConnectionEpoch == nil {
		// The durable fence is enough while disconnected: stream-loss / lease
		// convergence will close it without pretending the send succeeded.
		return nil
	}
	return service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{
		ConnectionEpoch: *view.ConnectionEpoch,
		CorrelationId:   uint64(attemptID),
		BootId:          view.BootID,
		Msg:             &runtimev1.ControlEnvelope_CancelAttempt{CancelAttempt: &runtimev1.CancelAttempt{AttemptId: attemptID}},
	})
}

// dispatchQueuedInspections sweeps the queued report analyses (collection
// children are consumed by the local execution loop).
func (service *RuntimeService) dispatchQueuedInspections(ctx context.Context) {
	if service.Inspections == nil {
		return
	}
	view, err := service.Slots.View(ctx, qruntime.SlotPlinth)
	if err != nil || !view.Connected || view.ConnectionEpoch == nil {
		return
	}
	analysisIDs, scanErr := service.Inspections.QueuedAnalysisAttempts(ctx)
	if scanErr != nil {
		sharedops.LogEvent("quoin", "error", "inspection.analysis_queue_scan", scanErr.Error())
		return
	}
	for _, id := range analysisIDs {
		if dispatchErr := service.dispatchInspectionAnalysis(ctx, id); dispatchErr != nil {
			sharedops.LogEvent("quoin", "error", "inspection.analysis_queue_dispatch", dispatchErr.Error())
		}
	}
}

// RunInspectionScheduler starts the durable minute scheduler after every
// Quoin boot. SQLite keys, rather than process memory, make repeated startup
// ticks safe. Collection availability now tracks the Stele gateway (the
// collector's platform transport, ADR-0011); a boundary observed with the
// gateway down records its durable runtime_unavailable gap.
func (service *RuntimeService) RunInspectionScheduler(ctx context.Context) {
	if service.Inspections == nil {
		return
	}
	availability := func(ctx context.Context) inspection.RuntimeAvailability {
		if service.SteleGateway == nil {
			return inspection.RuntimeAvailability{}
		}
		connected, _ := service.SteleGateway.Connected()
		return inspection.RuntimeAvailability{Collection: connected}
	}
	if blocking := service.MaintenanceBlocking; blocking != nil {
		inner := availability
		availability = func(ctx context.Context) inspection.RuntimeAvailability {
			if blocking(ctx) {
				return inspection.RuntimeAvailability{}
			}
			return inner(ctx)
		}
	}
	scheduler.New(service.Inspections, availability).AfterTick(service.kickLocalExecution).Run(ctx, func(err error) {
		sharedops.LogEvent("quoin", "error", "inspection.schedule", err.Error())
	})
}

// handleInspectionPromQLResultProposal adjudicates inspection_promql_result_v1.
func (service *RuntimeService) handleInspectionPromQLResultProposal(ctx context.Context, envelope *runtimev1.ControlEnvelope, proposal *runtimev1.ResultProposal) {
	ack := &runtimev1.ControlEnvelope{
		ConnectionEpoch: envelope.GetConnectionEpoch(), CorrelationId: envelope.GetCorrelationId(), BootId: envelope.GetBootId(),
		Msg: &runtimev1.ControlEnvelope_ResultAck{ResultAck: &runtimev1.ResultAck{AttemptId: proposal.GetAttemptId()}},
	}
	reject := func(reason string) {
		ack.GetResultAck().Accepted, ack.GetResultAck().Detail = false, reason
		_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
		sharedops.LogEvent("quoin", "error", "inspection.result_rejected", fmt.Sprintf("attempt=%d reason=%s", proposal.GetAttemptId(), reason))
	}
	if service.Inspections == nil {
		reject("inspections are not wired")
		return
	}
	payload := proposal.GetPayload()
	if payload == nil || payload.GetSchemaKind() != "inspection_promql_result_v1" || len(payload.GetCanonicalJson()) == 0 {
		reject("expected inspection_promql_result_v1 payload")
		return
	}
	digest := sha256.Sum256(payload.GetCanonicalJson())
	if hex.EncodeToString(digest[:]) != hex.EncodeToString(payload.GetContentDigest()) {
		reject("content digest mismatch")
		return
	}
	if err := service.Inspections.CommitPromQLProposal(ctx, proposal.GetAttemptId(), proposal.GetBootId(), proposal.GetConnectionEpoch(), payload.GetCanonicalJson()); err != nil {
		reject(err.Error())
		return
	}
	ack.GetResultAck().Accepted = true
	_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
	// The commit may have closed the collection and created the analysis
	// attempt; dispatch it without waiting for an unrelated runtime event.
	go service.dispatchQueuedInspections(context.Background())
}

// handleInspectionReportResultProposal adjudicates
// inspection_report_result_v1 against the frozen report facts.
func (service *RuntimeService) handleInspectionReportResultProposal(ctx context.Context, envelope *runtimev1.ControlEnvelope, proposal *runtimev1.ResultProposal) {
	ack := &runtimev1.ControlEnvelope{
		ConnectionEpoch: envelope.GetConnectionEpoch(), CorrelationId: envelope.GetCorrelationId(), BootId: envelope.GetBootId(),
		Msg: &runtimev1.ControlEnvelope_ResultAck{ResultAck: &runtimev1.ResultAck{AttemptId: proposal.GetAttemptId()}},
	}
	reject := func(reason string) {
		ack.GetResultAck().Accepted, ack.GetResultAck().Detail = false, reason
		_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
		sharedops.LogEvent("quoin", "error", "inspection.report_rejected", fmt.Sprintf("attempt=%d reason=%s", proposal.GetAttemptId(), reason))
	}
	if service.Inspections == nil {
		reject("inspections are not wired")
		return
	}
	payload := proposal.GetPayload()
	if payload == nil || payload.GetSchemaKind() != "inspection_report_result_v1" || len(payload.GetCanonicalJson()) == 0 {
		reject("expected inspection_report_result_v1 payload")
		return
	}
	digest := sha256.Sum256(payload.GetCanonicalJson())
	if hex.EncodeToString(digest[:]) != hex.EncodeToString(payload.GetContentDigest()) {
		reject("content digest mismatch")
		return
	}
	if proposal.GetOutcome() == runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED {
		termination := terminationReasonOf(proposal.GetTerminationReason())
		if termination == "" {
			reject("failed outcome requires a termination reason")
			return
		}
		if err := service.Inspections.Attempts().CommitResult(ctx, proposal.GetAttemptId(), proposal.GetBootId(), proposal.GetConnectionEpoch(), false, termination); err != nil {
			reject(err.Error())
			return
		}
		ack.GetResultAck().Accepted = true
		_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
		return
	}
	if proposal.GetOutcome() != runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED {
		reject("unsupported inspection report outcome")
		return
	}
	if err := service.Inspections.CommitReportProposal(ctx, proposal.GetAttemptId(), proposal.GetBootId(), proposal.GetConnectionEpoch(), payload.GetCanonicalJson()); err != nil {
		reject(err.Error())
		return
	}
	ack.GetResultAck().Accepted = true
	_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
}

// AttemptTypeOfInspection narrows the routing helper for package tests.
func AttemptTypeOfInspection(service *RuntimeService, ctx context.Context, attemptID int64) (string, error) {
	return service.attemptTypeOf(ctx, attemptID)
}

var _ = inspection.ErrNotFound

// handleInspectionPluginResultProposal adjudicates inspection_plugin_result_v1
// (ADR-0004 plan-run plugin collection) with the same digest/ack discipline as
// the historical PromQL results.
func (service *RuntimeService) handleInspectionPluginResultProposal(ctx context.Context, envelope *runtimev1.ControlEnvelope, proposal *runtimev1.ResultProposal) {
	ack := &runtimev1.ControlEnvelope{
		ConnectionEpoch: envelope.GetConnectionEpoch(), CorrelationId: envelope.GetCorrelationId(), BootId: envelope.GetBootId(),
		Msg: &runtimev1.ControlEnvelope_ResultAck{ResultAck: &runtimev1.ResultAck{AttemptId: proposal.GetAttemptId()}},
	}
	reject := func(reason string) {
		ack.GetResultAck().Accepted, ack.GetResultAck().Detail = false, reason
		_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
		sharedops.LogEvent("quoin", "error", "inspection.plugin_result_rejected", fmt.Sprintf("attempt=%d reason=%s", proposal.GetAttemptId(), reason))
	}
	if service.Inspections == nil {
		reject("inspections are not wired")
		return
	}
	payload := proposal.GetPayload()
	if payload == nil || payload.GetSchemaKind() != "inspection_plugin_result_v1" || len(payload.GetCanonicalJson()) == 0 {
		reject("expected inspection_plugin_result_v1 payload")
		return
	}
	digest := sha256.Sum256(payload.GetCanonicalJson())
	if hex.EncodeToString(digest[:]) != hex.EncodeToString(payload.GetContentDigest()) {
		reject("content digest mismatch")
		return
	}
	if err := service.Inspections.CommitPluginProposal(ctx, proposal.GetAttemptId(), proposal.GetBootId(), proposal.GetConnectionEpoch(), payload.GetCanonicalJson()); err != nil {
		reject(err.Error())
		return
	}
	ack.GetResultAck().Accepted = true
	_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
	// The commit may have closed the collection and created the analysis
	// attempt; dispatch it without waiting for an unrelated runtime event.
	go service.dispatchQueuedInspections(context.Background())
}
