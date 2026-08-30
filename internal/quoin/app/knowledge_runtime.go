package app

// Knowledge extraction runtime slice. It dispatches the frozen source-material
// snapshot through the existing outbound Plinth stream; no handler or worker
// has direct model-provider access.

import (
	"context"
	"fmt"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (service *RuntimeService) dispatchKnowledgeExtractionAttempt(ctx context.Context, attemptID int64) error {
	if service.Knowledge == nil {
		return fmt.Errorf("knowledge service not wired")
	}
	view, err := service.Slots.View(ctx, qruntime.SlotPlinth)
	if err != nil {
		return err
	}
	if !view.Connected || view.ConnectionEpoch == nil {
		return fmt.Errorf("plinth is not connected")
	}
	attempts := service.Knowledge.Attempts()
	if err := attempts.BindToStream(ctx, attemptID, view.BootID, *view.ConnectionEpoch, attempt.DispatchLease); err != nil {
		return err
	}
	input, err := attempts.DispatchInputFor(ctx, attemptID)
	if err != nil {
		return err
	}
	var scopeID int64
	if err := service.Knowledge.DB().QueryRowContext(ctx, `SELECT scope_id FROM execution_attempts WHERE id=?`, attemptID).Scan(&scopeID); err != nil {
		return err
	}
	grants := make([]*runtimev1.ConnectionGrant, 0, len(input.Grants))
	for _, grant := range input.Grants {
		grants = append(grants, &runtimev1.ConnectionGrant{GrantId: grant.GrantID, ConnectionRevisionId: grant.ConnectionRevisionID, CredentialGenerationId: grant.CredentialGenerationID, Purpose: grant.Purpose, ConnectionProbeResultId: grant.ConnectionProbeResultID})
	}
	return service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{ConnectionEpoch: *view.ConnectionEpoch, CorrelationId: uint64(attemptID), BootId: view.BootID, Msg: &runtimev1.ControlEnvelope_DispatchAttempt{DispatchAttempt: &runtimev1.DispatchAttempt{
		AttemptId: attemptID, AttemptType: runtimev1.AttemptType_ATTEMPT_TYPE_KNOWLEDGE_EXTRACTION, ScopeType: runtimev1.ScopeType_SCOPE_TYPE_KNOWLEDGE_IMPORT_BATCH, ScopeId: scopeID, LeaseDeadline: timestamppb.New(time.Now().UTC().Add(attempt.DispatchLease)),
		Input: &runtimev1.AttemptInputSnapshot{SchemaKind: input.SchemaKind, CanonicalJson: input.CanonicalJSON, ContentDigest: input.ContentDigest, ConnectionGrants: grants, AgentVersion: input.AgentVersion},
	}}})
}

func (service *RuntimeService) dispatchQueuedKnowledgeExtractions(ctx context.Context) {
	if service.Knowledge == nil {
		return
	}
	ids, err := service.Knowledge.Attempts().QueuedAgentAttempts(ctx, "knowledge_extraction")
	if err != nil {
		return
	}
	for _, id := range ids {
		_ = service.dispatchKnowledgeExtractionAttempt(ctx, id)
	}
}
