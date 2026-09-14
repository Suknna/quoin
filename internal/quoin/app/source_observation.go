// Source observation runtime slice (ADR-0004): dispatch of observation_run
// children to the live Plinth slot through the same frozen bind, grant
// transport and result adjudication boundary as every other attempt type.
// Quoin never executes discovery itself; the supervisor runs it through the
// bound plugin Discoverer and this file only carries authority across the
// wire.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/observation"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// filterTransientDispatchError separates permanent input defects from
// transient runtime unavailability: only the former converges as a technical
// gap. It returns the bounded permanent reason, or nil for transient.
func filterTransientDispatchError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "plinth is not connected") || strings.Contains(err.Error(), "slot") {
		return nil
	}
	if errors.Is(err, attempt.ErrLateResult) || strings.Contains(err.Error(), "no rows") {
		// The attempt moved on concurrently: nothing to converge.
		return nil
	}
	return err
}

// dispatchSourceObservationAttempt binds one Queued observation child to the
// live Plinth stream and sends the frozen dispatch envelope. A malformed
// snapshot or missing grant stays Queued for repair/retry instead of becoming
// an Assigned attempt no worker can execute.
func (service *RuntimeService) dispatchSourceObservationAttempt(ctx context.Context, attemptID int64) error {
	if service.Observations == nil {
		return fmt.Errorf("source observation is not wired")
	}
	view, err := service.Slots.View(ctx, qruntime.SlotPlinth)
	if err != nil {
		return err
	}
	if !view.Connected || view.ConnectionEpoch == nil {
		return fmt.Errorf("plinth is not connected")
	}
	prepared, err := service.prepareSourceObservationDispatch(ctx, attemptID)
	if err != nil {
		return err
	}
	if err := service.Observations.Attempts().BindToStream(ctx, attemptID, view.BootID, *view.ConnectionEpoch, attempt.DispatchLease, view.ReleaseVersion); err != nil {
		return err
	}
	return service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{
		ConnectionEpoch: *view.ConnectionEpoch,
		CorrelationId:   uint64(attemptID),
		BootId:          view.BootID,
		Msg: &runtimev1.ControlEnvelope_DispatchAttempt{DispatchAttempt: &runtimev1.DispatchAttempt{
			AttemptId: attemptID, AttemptType: runtimev1.AttemptType_ATTEMPT_TYPE_INSPECTION_COLLECTION,
			ScopeType: runtimev1.ScopeType_SCOPE_TYPE_OBSERVATION_RUN, ScopeId: prepared.scopeID,
			LeaseDeadline: timestamppb.New(time.Now().UTC().Add(attempt.DispatchLease)),
			Input:         &runtimev1.AttemptInputSnapshot{SchemaKind: prepared.input.SchemaKind, CanonicalJson: prepared.input.CanonicalJSON, ContentDigest: prepared.input.ContentDigest, ConnectionGrants: prepared.grants},
		}},
	})
}

type sourceObservationDispatchInput struct {
	scopeID int64
	input   attempt.DispatchInput
	grants  []*runtimev1.ConnectionGrant
}

// prepareSourceObservationDispatch validates every immutable dispatch fact
// before the binding transition, mirroring the resource refresh slice.
func (service *RuntimeService) prepareSourceObservationDispatch(ctx context.Context, attemptID int64) (sourceObservationDispatchInput, error) {
	input, err := service.Observations.Attempts().DispatchInputFor(ctx, attemptID)
	if err != nil {
		return sourceObservationDispatchInput{}, err
	}
	var scopeID int64
	if err := service.Observations.DB().QueryRowContext(ctx, `SELECT scope_id FROM execution_attempts WHERE id=? AND attempt_type='inspection_collection' AND scope_type='observation_run' AND state='Queued'`, attemptID).Scan(&scopeID); err != nil {
		return sourceObservationDispatchInput{}, err
	}
	rows, err := service.Observations.DB().QueryContext(ctx, `
		SELECT id,connection_revision_id,credential_generation_id,purpose
		FROM attempt_connection_grants WHERE attempt_id=? ORDER BY id`, attemptID)
	if err != nil {
		return sourceObservationDispatchInput{}, err
	}
	defer rows.Close()
	var grants []*runtimev1.ConnectionGrant
	for rows.Next() {
		grant := &runtimev1.ConnectionGrant{}
		if err := rows.Scan(&grant.GrantId, &grant.ConnectionRevisionId, &grant.CredentialGenerationId, &grant.Purpose); err != nil {
			return sourceObservationDispatchInput{}, err
		}
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return sourceObservationDispatchInput{}, err
	}
	return sourceObservationDispatchInput{scopeID: scopeID, input: input, grants: grants}, nil
}

// dispatchQueuedSourceObservationAttempts is safe to call after every
// scheduler pass and runtime reconnect. BindToStream provides the durable
// active-attempt fence, so duplicate kicks cannot dispatch concurrent work.
func (service *RuntimeService) dispatchQueuedSourceObservationAttempts(ctx context.Context) {
	if service.Observations == nil {
		return
	}
	ids, err := service.Observations.QueuedObservationAttempts(ctx)
	if err != nil {
		sharedops.LogEvent("quoin", "error", "source_observation.queue_scan", err.Error())
		return
	}
	for _, id := range ids {
		if err := service.dispatchSourceObservationAttempt(ctx, id); err != nil {
			sharedops.LogEvent("quoin", "error", "source_observation.queue_dispatch", fmt.Sprintf("attempt=%d %v", id, err))
			// A dispatch input that cannot be rebuilt will never become
			// dispatchable: converge it as a technical gap instead of
			// retrying the same failure every pass. Runtime unavailability
			// is filtered before this point and stays Queued.
			if inputErr := filterTransientDispatchError(err); inputErr != nil {
				if gapErr := service.Observations.ConvergeTechnicalGap(ctx, id, inputErr.Error()); gapErr != nil {
					sharedops.LogEvent("quoin", "error", "source_observation.gap_converge", fmt.Sprintf("attempt=%d %v", id, gapErr))
				}
			}
		}
	}
}

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
