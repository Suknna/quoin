package app

// embedding_runtime.go: the embedding attempt dispatch slice (T29). Embedding
// attempts run supervisor-direct on the outbound Plinth stream with no agent
// worker (RUNTIME-AGENT-010); this slice binds the frozen input, forwards it
// and lets the supervisor's typed executor propose the closed result.

import (
	"context"
	"fmt"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// dispatchEmbeddingAttempt delivers one Queued embedding attempt over the
// live Plinth stream.
func (service *RuntimeService) dispatchEmbeddingAttempt(ctx context.Context, attemptID int64) error {
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
		AttemptId: attemptID, AttemptType: runtimev1.AttemptType_ATTEMPT_TYPE_EMBEDDING, ScopeType: runtimev1.ScopeType_SCOPE_TYPE_EMBEDDING_GENERATION, ScopeId: scopeID, LeaseDeadline: timestamppb.New(time.Now().UTC().Add(attempt.DispatchLease)),
		Input: &runtimev1.AttemptInputSnapshot{SchemaKind: input.SchemaKind, CanonicalJson: input.CanonicalJSON, ContentDigest: input.ContentDigest, ConnectionGrants: grants},
	}}})
}

// dispatchQueuedEmbeddings first reconciles the semantic projection (drift
// detection, pending enqueue, attempt creation, settled generation switch),
// then delivers every Queued embedding attempt.
func (service *RuntimeService) dispatchQueuedEmbeddings(ctx context.Context) {
	if service.Knowledge == nil {
		return
	}
	if err := service.Knowledge.Embeddings().Sweep(ctx); err != nil {
		return
	}
	ids, err := service.Knowledge.Attempts().QueuedAgentAttempts(ctx, "embedding")
	if err != nil {
		return
	}
	for _, id := range ids {
		_ = service.dispatchEmbeddingAttempt(ctx, id)
	}
}

// dispatchEmbeddingAttemptForSearch adapts the dispatch to the embedding
// service's synchronous kick signature.
func (service *RuntimeService) dispatchEmbeddingAttemptForSearch(ctx context.Context, attemptID int64) error {
	return service.dispatchEmbeddingAttempt(ctx, attemptID)
}
