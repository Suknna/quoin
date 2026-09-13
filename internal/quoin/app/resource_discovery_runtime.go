package app

import (
	"context"
	"fmt"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// dispatchResourceDiscoveryAttempt uses the same live-Plinth bind, frozen input
// and grant transport boundary as configuration verification. Resource refresh
// is explicitly non-browser work: every child is inspection_collection scoped
// to its resource_refresh_run.
func (service *RuntimeService) dispatchResourceDiscoveryAttempt(ctx context.Context, attemptID int64) error {
	if service.BusinessSystems == nil {
		return fmt.Errorf("business systems are not wired")
	}
	view, err := service.Slots.View(ctx, qruntime.SlotPlinth)
	if err != nil {
		return err
	}
	if !view.Connected || view.ConnectionEpoch == nil {
		return fmt.Errorf("plinth is not connected")
	}
	// Validate every immutable dispatch fact before the binding transition. A
	// malformed snapshot or missing grant must remain Queued for repair/retry,
	// rather than become Assigned with no frame a worker can execute.
	prepared, err := service.prepareResourceDiscoveryDispatch(ctx, attemptID)
	if err != nil {
		return err
	}
	attempts := service.BusinessSystems.ResourceRefreshAttempts()
	if err := attempts.BindToStream(ctx, attemptID, view.BootID, *view.ConnectionEpoch, attempt.DispatchLease, view.ReleaseVersion); err != nil {
		return err
	}
	return service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{
		ConnectionEpoch: *view.ConnectionEpoch,
		CorrelationId:   uint64(attemptID),
		BootId:          view.BootID,
		Msg: &runtimev1.ControlEnvelope_DispatchAttempt{DispatchAttempt: &runtimev1.DispatchAttempt{
			AttemptId: attemptID, AttemptType: runtimev1.AttemptType_ATTEMPT_TYPE_INSPECTION_COLLECTION,
			ScopeType: runtimev1.ScopeType_SCOPE_TYPE_RESOURCE_REFRESH_RUN, ScopeId: prepared.scopeID,
			LeaseDeadline: timestamppb.New(time.Now().UTC().Add(attempt.DispatchLease)),
			Input:         &runtimev1.AttemptInputSnapshot{SchemaKind: prepared.input.SchemaKind, CanonicalJson: prepared.input.CanonicalJSON, ContentDigest: prepared.input.ContentDigest, ConnectionGrants: prepared.grants},
		}},
	})
}

type resourceDiscoveryDispatchInput struct {
	scopeID int64
	input   attempt.DispatchInput
	grants  []*runtimev1.ConnectionGrant
}

func (service *RuntimeService) prepareResourceDiscoveryDispatch(ctx context.Context, attemptID int64) (resourceDiscoveryDispatchInput, error) {
	attempts := service.BusinessSystems.ResourceRefreshAttempts()
	input, err := attempts.DispatchInputFor(ctx, attemptID)
	if err != nil {
		return resourceDiscoveryDispatchInput{}, err
	}
	var scopeID int64
	if err := service.BusinessSystems.DB().QueryRowContext(ctx, `SELECT scope_id FROM execution_attempts WHERE id=? AND attempt_type='inspection_collection' AND scope_type='resource_refresh_run' AND state='Queued'`, attemptID).Scan(&scopeID); err != nil {
		return resourceDiscoveryDispatchInput{}, err
	}
	rows, err := service.BusinessSystems.DB().QueryContext(ctx, `
		SELECT id,connection_revision_id,credential_generation_id,purpose
		FROM attempt_connection_grants WHERE attempt_id=? ORDER BY id`, attemptID)
	if err != nil {
		return resourceDiscoveryDispatchInput{}, err
	}
	defer rows.Close()
	var grants []*runtimev1.ConnectionGrant
	for rows.Next() {
		grant := &runtimev1.ConnectionGrant{}
		if err := rows.Scan(&grant.GrantId, &grant.ConnectionRevisionId, &grant.CredentialGenerationId, &grant.Purpose); err != nil {
			return resourceDiscoveryDispatchInput{}, err
		}
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return resourceDiscoveryDispatchInput{}, err
	}
	return resourceDiscoveryDispatchInput{scopeID: scopeID, input: input, grants: grants}, nil
}

// dispatchQueuedResourceDiscoveryAttempts is safe to call after every
// scheduler pass and runtime reconnect. BindToStream provides the durable
// active-attempt fence, so duplicate kicks cannot dispatch concurrent work.
func (service *RuntimeService) dispatchQueuedResourceDiscoveryAttempts(ctx context.Context) {
	if service.BusinessSystems == nil {
		return
	}
	ids, err := service.BusinessSystems.QueuedResourceRefreshAttempts(ctx)
	if err != nil {
		sharedops.LogEvent("quoin", "error", "resource_discovery.queue_scan", err.Error())
		return
	}
	for _, id := range ids {
		if err := service.dispatchResourceDiscoveryAttempt(ctx, id); err != nil {
			sharedops.LogEvent("quoin", "error", "resource_discovery.queue_dispatch", fmt.Sprintf("attempt=%d %v", id, err))
		}
	}
}
