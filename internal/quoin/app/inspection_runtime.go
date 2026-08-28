package app

// Manual Inspection runtime slice (T24, CFG-INSPECTRUN-001/002): dispatch of
// run_check PromQL children and inspection_analysis attempts to the live
// Plinth stream, plus the ResultProposal adjudication boundary for
// inspection_promql_result_v1 and inspection_report_result_v1.

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
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// dispatchInspectionAttempt binds one pre-frozen run_check PromQL child to the
// live Plinth stream and sends the DispatchAttempt frame.
func (service *RuntimeService) dispatchInspectionAttempt(ctx context.Context, attemptID int64) error {
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
	if err := attempts.BindToStream(ctx, attemptID, view.BootID, *view.ConnectionEpoch, attempt.DispatchLease); err != nil {
		return err
	}
	input, err := attempts.DispatchInputFor(ctx, attemptID)
	if err != nil {
		return err
	}
	var scopeID int64
	if err := service.Inspections.DB().QueryRowContext(ctx, `SELECT scope_id FROM execution_attempts WHERE id=?`, attemptID).Scan(&scopeID); err != nil {
		return err
	}
	rows, err := service.Inspections.DB().QueryContext(ctx, `SELECT id,connection_revision_id,credential_generation_id,purpose FROM attempt_connection_grants WHERE attempt_id=? ORDER BY id`, attemptID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var grants []*runtimev1.ConnectionGrant
	for rows.Next() {
		grant := &runtimev1.ConnectionGrant{}
		if err := rows.Scan(&grant.GrantId, &grant.ConnectionRevisionId, &grant.CredentialGenerationId, &grant.Purpose); err != nil {
			return err
		}
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{
		ConnectionEpoch: *view.ConnectionEpoch,
		CorrelationId:   uint64(attemptID),
		BootId:          view.BootID,
		Msg: &runtimev1.ControlEnvelope_DispatchAttempt{DispatchAttempt: &runtimev1.DispatchAttempt{
			AttemptId: attemptID, AttemptType: runtimev1.AttemptType_ATTEMPT_TYPE_INSPECTION_COLLECTION,
			ScopeType: runtimev1.ScopeType_SCOPE_TYPE_RUN_CHECK, ScopeId: scopeID,
			LeaseDeadline: timestamppb.New(time.Now().UTC().Add(attempt.DispatchLease)),
			Input:         &runtimev1.AttemptInputSnapshot{SchemaKind: input.SchemaKind, CanonicalJson: input.CanonicalJSON, ContentDigest: input.ContentDigest, ConnectionGrants: grants},
		}},
	})
}

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
	if err := attempts.BindToStream(ctx, attemptID, view.BootID, *view.ConnectionEpoch, attempt.DispatchLease); err != nil {
		return err
	}
	input, err := attempts.DispatchInputFor(ctx, attemptID)
	if err != nil {
		return err
	}
	var scopeID int64
	if err := service.Inspections.DB().QueryRowContext(ctx, `SELECT scope_id FROM execution_attempts WHERE id=?`, attemptID).Scan(&scopeID); err != nil {
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
			ScopeType: runtimev1.ScopeType_SCOPE_TYPE_RUN, ScopeId: scopeID,
			LeaseDeadline: timestamppb.New(time.Now().UTC().Add(attempt.DispatchLease)),
			Input:         &runtimev1.AttemptInputSnapshot{SchemaKind: input.SchemaKind, CanonicalJson: input.CanonicalJSON, ContentDigest: input.ContentDigest, ConnectionGrants: grants, AgentVersion: input.AgentVersion},
		}},
	})
}

// dispatchQueuedInspections sweeps both inspection queues after a Plinth
// stream attaches (created while disconnected converge here).
func (service *RuntimeService) dispatchQueuedInspections(ctx context.Context) {
	if service.Inspections == nil {
		return
	}
	view, err := service.Slots.View(ctx, qruntime.SlotPlinth)
	if err != nil || !view.Connected || view.ConnectionEpoch == nil {
		return
	}
	promqlIDs, err := service.Inspections.QueuedPromQLAttempts(ctx)
	if err != nil {
		sharedops.LogEvent("quoin", "error", "inspection.queue_scan", err.Error())
		return
	}
	for _, id := range promqlIDs {
		if err := service.dispatchInspectionAttempt(ctx, id); err != nil {
			sharedops.LogEvent("quoin", "error", "inspection.queue_dispatch", err.Error())
		}
	}
	analysisIDs, err := service.Inspections.QueuedAnalysisAttempts(ctx)
	if err != nil {
		sharedops.LogEvent("quoin", "error", "inspection.analysis_queue_scan", err.Error())
		return
	}
	for _, id := range analysisIDs {
		if err := service.dispatchInspectionAnalysis(ctx, id); err != nil {
			sharedops.LogEvent("quoin", "error", "inspection.analysis_queue_dispatch", err.Error())
		}
	}
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
