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

// dispatchVerificationAttempt binds and dispatches a pre-frozen PromQL check.
// The snapshot includes only query facts and an opaque grant locator; secrets
// remain inside FetchCredentialGrant on the supervisor side.
func (service *RuntimeService) dispatchVerificationAttempt(ctx context.Context, attemptID int64) error {
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
	attempts := service.BusinessSystems.VerificationAttempts()
	if err := attempts.BindToStream(ctx, attemptID, view.BootID, *view.ConnectionEpoch, attempt.DispatchLease); err != nil {
		return err
	}
	input, err := attempts.DispatchInputFor(ctx, attemptID)
	if err != nil {
		return err
	}
	var scopeID int64
	if err := service.BusinessSystems.DB().QueryRowContext(ctx, `SELECT scope_id FROM execution_attempts WHERE id=?`, attemptID).Scan(&scopeID); err != nil {
		return err
	}
	var grants []*runtimev1.ConnectionGrant
	rows, err := service.BusinessSystems.DB().QueryContext(ctx, `
		SELECT id,connection_revision_id,credential_generation_id,purpose
		FROM attempt_connection_grants WHERE attempt_id=? ORDER BY id`, attemptID)
	if err != nil {
		return err
	}
	for rows.Next() {
		grant := &runtimev1.ConnectionGrant{}
		if err := rows.Scan(&grant.GrantId, &grant.ConnectionRevisionId, &grant.CredentialGenerationId, &grant.Purpose); err != nil {
			rows.Close()
			return err
		}
		grants = append(grants, grant)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{
		ConnectionEpoch: *view.ConnectionEpoch,
		CorrelationId:   uint64(attemptID),
		BootId:          view.BootID,
		Msg: &runtimev1.ControlEnvelope_DispatchAttempt{DispatchAttempt: &runtimev1.DispatchAttempt{
			AttemptId: attemptID, AttemptType: runtimev1.AttemptType_ATTEMPT_TYPE_INSPECTION_COLLECTION,
			ScopeType: runtimev1.ScopeType_SCOPE_TYPE_CONFIG_VERIFICATION_RUN, ScopeId: scopeID,
			LeaseDeadline: timestamppb.New(time.Now().UTC().Add(attempt.DispatchLease)),
			Input:         &runtimev1.AttemptInputSnapshot{SchemaKind: input.SchemaKind, CanonicalJson: input.CanonicalJSON, ContentDigest: input.ContentDigest, ConnectionGrants: grants},
		}},
	})
}

func (service *RuntimeService) dispatchQueuedVerificationAttempts(ctx context.Context) {
	if service.BusinessSystems == nil {
		return
	}
	ids, err := service.BusinessSystems.QueuedVerificationAttempts(ctx)
	if err != nil {
		sharedops.LogEvent("quoin", "error", "config_verification.queue_scan", err.Error())
		return
	}
	for _, id := range ids {
		if err := service.dispatchVerificationAttempt(ctx, id); err != nil {
			sharedops.LogEvent("quoin", "error", "config_verification.queue_dispatch", err.Error())
		}
	}
}

func (service *RuntimeService) dispatchResourceRefreshAttempt(ctx context.Context, attemptID int64) error {
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
	attempts := service.BusinessSystems.ResourceRefreshAttempts()
	if err := attempts.BindToStream(ctx, attemptID, view.BootID, *view.ConnectionEpoch, attempt.DispatchLease); err != nil {
		return err
	}
	input, err := attempts.DispatchInputFor(ctx, attemptID)
	if err != nil {
		return err
	}
	var scopeID int64
	if err := service.BusinessSystems.DB().QueryRowContext(ctx, `SELECT scope_id FROM execution_attempts WHERE id=?`, attemptID).Scan(&scopeID); err != nil {
		return err
	}
	rows, err := service.BusinessSystems.DB().QueryContext(ctx, `SELECT id,connection_revision_id,credential_generation_id,purpose FROM attempt_connection_grants WHERE attempt_id=? ORDER BY id`, attemptID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var grants []*runtimev1.ConnectionGrant
	for rows.Next() {
		g := &runtimev1.ConnectionGrant{}
		if err := rows.Scan(&g.GrantId, &g.ConnectionRevisionId, &g.CredentialGenerationId, &g.Purpose); err != nil {
			return err
		}
		grants = append(grants, g)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{ConnectionEpoch: *view.ConnectionEpoch, CorrelationId: uint64(attemptID), BootId: view.BootID, Msg: &runtimev1.ControlEnvelope_DispatchAttempt{DispatchAttempt: &runtimev1.DispatchAttempt{AttemptId: attemptID, AttemptType: runtimev1.AttemptType_ATTEMPT_TYPE_INSPECTION_COLLECTION, ScopeType: runtimev1.ScopeType_SCOPE_TYPE_RESOURCE_REFRESH_RUN, ScopeId: scopeID, LeaseDeadline: timestamppb.New(time.Now().UTC().Add(attempt.DispatchLease)), Input: &runtimev1.AttemptInputSnapshot{SchemaKind: input.SchemaKind, CanonicalJson: input.CanonicalJSON, ContentDigest: input.ContentDigest, ConnectionGrants: grants}}}})
}

func (service *RuntimeService) RunResourceRefreshScheduler(ctx context.Context) {
	if service.BusinessSystems == nil {
		return
	}
	run := func() {
		if _, err := service.BusinessSystems.StartDueResourceRefreshes(ctx); err != nil {
			sharedops.LogEvent("quoin", "error", "resource_refresh.schedule", err.Error())
		}
		service.dispatchQueuedResourceRefreshAttempts(ctx)
	}
	run()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

func (service *RuntimeService) dispatchQueuedResourceRefreshAttempts(ctx context.Context) {
	if service.BusinessSystems == nil {
		return
	}
	ids, err := service.BusinessSystems.QueuedResourceRefreshAttempts(ctx)
	if err != nil {
		sharedops.LogEvent("quoin", "error", "resource_refresh.queue_scan", err.Error())
		return
	}
	for _, id := range ids {
		if err := service.dispatchResourceRefreshAttempt(ctx, id); err != nil {
			sharedops.LogEvent("quoin", "error", "resource_refresh.queue_dispatch", err.Error())
		}
	}
}
