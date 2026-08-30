package supervisor

// embedding.go: the supervisor-direct Embedding work mode (T29,
// RUNTIME-AGENT-010). One dispatched embedding attempt fetches its
// attempt-scoped provider credential, runs the frozen batch through the
// modelprovider executor (one Begin/CompleteModelCall ledger pair) and
// proposes the closed typed result. No worker process is involved.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	plinthconnections "github.com/Suknna/quoin/internal/plinth/connections"
	"github.com/Suknna/quoin/internal/plinth/modelprovider"
	"github.com/Suknna/quoin/internal/plinth/runtime"
	"google.golang.org/grpc/metadata"
)

// runEmbedding executes one embedding attempt to a typed terminal proposal.
func (supervisor *Supervisor) runEmbedding(parent context.Context, sink *runtime.FrameSink, client runtimev1.RuntimeControlClient, dispatch *runtimev1.DispatchAttempt, binding runtime.DispatchBinding, stopTask func(int64) bool) {
	attemptID := dispatch.GetAttemptId()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	supervisor.Channel.RegisterTask(attemptID, cancel)
	defer stopTask(attemptID)
	if err := sink.Send(&runtimev1.ControlEnvelope{CorrelationId: uint64(attemptID), Msg: &runtimev1.ControlEnvelope_AttemptAccept{AttemptAccept: &runtimev1.AttemptAccept{AttemptId: attemptID}}}); err != nil {
		sharedops.LogEvent("plinth", "error", "embedding.accept_send", err.Error())
		return
	}
	proposeFailure := func(message string) {
		supervisor.proposeEmbeddingFailure(sink, attemptID, binding, message)
	}
	if dispatch.GetInput() == nil {
		proposeFailure("dispatch carries no input snapshot")
		return
	}
	input, err := modelprovider.ParseBatchInput(dispatch.GetInput().GetCanonicalJson(), attemptID)
	if err != nil {
		proposeFailure(err.Error())
		return
	}
	grant, ok := supervisor.primaryGrant(dispatch.GetInput(), "embedding")
	if !ok {
		proposeFailure("dispatch lacks the embedding grant")
		return
	}
	grantCtx, grantCancel := context.WithTimeout(ctx, 15*time.Second)
	bearer, bearerErr := supervisor.Channel.BearerToken()
	if bearerErr != nil {
		grantCancel()
		proposeFailure("读取状态卷 token 失败: " + bearerErr.Error())
		return
	}
	payload, err := client.FetchCredentialGrant(metadata.NewOutgoingContext(grantCtx, metadata.Pairs("authorization", "Bearer "+bearer)), &runtimev1.FetchCredentialGrantRequest{
		GrantId: grant.GetGrantId(), AttemptId: attemptID, BootId: binding.BootID, ConnectionEpoch: binding.Epoch,
	})
	grantCancel()
	if err != nil || payload.GetModelProvider() == nil {
		proposeFailure("获取模型凭据 grant 失败")
		return
	}
	var config plinthconnections.ModelProviderConfig
	if err := json.Unmarshal(payload.GetRevisionConfigJson(), &config); err != nil || config.BaseURL == "" {
		proposeFailure("模型供应商 revision 配置无法解析")
		return
	}
	batchCtx := modelprovider.WithAttempt(ctx, attemptID)
	ledger := &modelprovider.StreamLedger{Sink: sink, Channel: supervisor.Channel}
	canonical, digest, runErr := modelprovider.RunBatch(batchCtx, modelprovider.Config{Type: config.Type, BaseURL: config.BaseURL, ChatModelID: config.ChatModelID, EmbeddingModelID: config.EmbeddingModelID, ContextBudgetTokens: config.ContextBudgetTokens, MaxOutputTokens: config.MaxOutputTokens}, payload.GetModelProvider().GetApiKey(), input, ledger)
	if runErr != nil {
		sharedops.LogEvent("plinth", "info", "embedding.batch_failed", runErr.Error())
		supervisor.proposeEmbeddingTerminal(sink, attemptID, binding, "provider_unavailable", runErr.Error())
		return
	}
	if err := sink.Send(&runtimev1.ControlEnvelope{
		CorrelationId: uint64(attemptID),
		Msg: &runtimev1.ControlEnvelope_ResultProposal{ResultProposal: &runtimev1.ResultProposal{
			AttemptId: attemptID, BootId: binding.BootID, ConnectionEpoch: binding.Epoch,
			Outcome: runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED,
			Payload: &runtimev1.ResultPayload{SchemaKind: schemaKindFor(input.Mode), CanonicalJson: canonical, ContentDigest: digest[:]},
		}},
	}); err != nil {
		sharedops.LogEvent("plinth", "error", "embedding.result_send", err.Error())
	}
}

func schemaKindFor(mode string) string {
	if mode == "query" {
		return modelprovider.QuerySchemaKind
	}
	return modelprovider.ResultSchemaKind
}

// proposeEmbeddingFailure seals a pre-execution failure through the shared
// failure proposal shape with the embedding result schema.
func (supervisor *Supervisor) proposeEmbeddingFailure(sink *runtime.FrameSink, attemptID int64, binding runtime.DispatchBinding, message string) {
	supervisor.proposeEmbeddingTerminal(sink, attemptID, binding, "invalid_response", message)
}

func (supervisor *Supervisor) proposeEmbeddingTerminal(sink *runtime.FrameSink, attemptID int64, binding runtime.DispatchBinding, reason, message string) {
	startedAt := time.Now().UTC()
	payload, _ := json.Marshal(map[string]any{
		"outcome":    "failed",
		"detail":     map[string]any{"error": message},
		"startedAt":  startedAt.Format(time.RFC3339Nano),
		"finishedAt": startedAt.Format(time.RFC3339Nano),
	})
	digest := sha256.Sum256(payload)
	_ = sink.Send(&runtimev1.ControlEnvelope{
		CorrelationId: uint64(attemptID),
		Msg: &runtimev1.ControlEnvelope_ResultProposal{ResultProposal: &runtimev1.ResultProposal{
			AttemptId: attemptID, BootId: binding.BootID, ConnectionEpoch: binding.Epoch,
			Outcome:           runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED,
			TerminationReason: terminationReasonForText(reason),
			Payload:           &runtimev1.ResultPayload{SchemaKind: modelprovider.ResultSchemaKind, CanonicalJson: payload, ContentDigest: digest[:]},
		}},
	})
	sharedops.LogEvent("plinth", "info", "embedding.proposed_failure", message)
}

func terminationReasonForText(reason string) runtimev1.TerminationReason {
	switch reason {
	case "provider_unavailable":
		return runtimev1.TerminationReason_TERMINATION_REASON_PROVIDER_UNAVAILABLE
	case "rate_limited":
		return runtimev1.TerminationReason_TERMINATION_REASON_RATE_LIMITED
	case "timeout":
		return runtimev1.TerminationReason_TERMINATION_REASON_TIMEOUT
	}
	return runtimev1.TerminationReason_TERMINATION_REASON_INVALID_RESPONSE
}
