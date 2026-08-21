package supervisor

// Plinth supervisor task slice (T07): deterministic execution of the closed
// connection_probe action sets. The supervisor accepts a dispatched attempt,
// fetches its one attempt-scoped credential grant over the authenticated
// gRPC channel, runs the typed executor, and proposes the canonical typed
// result (RUNTIME-AGENT-010: no agent, no model, no ReAct).

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/plinth/agent"
	plinthconnections "github.com/Suknna/quoin/internal/plinth/connections"
	"github.com/Suknna/quoin/internal/plinth/model"
	"github.com/Suknna/quoin/internal/plinth/modelprovider"
	"github.com/Suknna/quoin/internal/plinth/runtime"
	"github.com/Suknna/quoin/internal/plinth/worker"
	"google.golang.org/grpc/metadata"
)

// Supervisor executes dispatched attempts on the live channel: the
// deterministic connection_probe action sets (T07/T08) and the initial
// analysis agent attempts (T10) via a fresh sandboxed worker process.
type Supervisor struct {
	Channel *runtime.Channel
	// WorkspaceRoot is the per-attempt workspace parent directory
	// (ARCH-WORKER-001: one fresh workspace per attempt).
	WorkspaceRoot string
}

// HandleDispatchAttempt runs one dispatched attempt to a typed terminal
// result proposal. The binding is the frozen (boot, epoch) identity of the
// dispatch; terminal proposals carry it so Quoin adjudicates against the
// frozen row binding even after same-boot reconnects (RUNTIME-TASK-008).
func (supervisor *Supervisor) HandleDispatchAttempt(parent context.Context, sink *runtime.FrameSink, client runtimev1.RuntimeControlClient, dispatch *runtimev1.DispatchAttempt, binding runtime.DispatchBinding, stopTask func(int64) bool) {
	attemptID := dispatch.GetAttemptId()

	// Supervisor scope: only connection_probe and initial_analysis
	// attempts exist here (RUNTIME-SCOPE).
	switch dispatch.GetAttemptType() {
	case runtimev1.AttemptType_ATTEMPT_TYPE_CONNECTION_PROBE:
		supervisor.runProbe(parent, sink, client, dispatch, binding, stopTask)
	case runtimev1.AttemptType_ATTEMPT_TYPE_INITIAL_ANALYSIS:
		supervisor.runAnalysis(parent, sink, client, dispatch, binding, stopTask)
	default:
		supervisor.reject(sink, attemptID, runtimev1.AttemptRejectReason_ATTEMPT_REJECT_REASON_INPUT_UNSUPPORTED, "supervisor does not execute this attempt type")
	}
}

// runAnalysis drives one initial-analysis attempt through a fresh worker
// process (ARCH-WORKER-001/002).
func (supervisor *Supervisor) runAnalysis(parent context.Context, sink *runtime.FrameSink, client runtimev1.RuntimeControlClient, dispatch *runtimev1.DispatchAttempt, binding runtime.DispatchBinding, stopTask func(int64) bool) {
	attemptID := dispatch.GetAttemptId()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	supervisor.Channel.RegisterTask(attemptID, cancel)
	defer stopTask(attemptID)
	input := dispatch.GetInput()
	if input == nil {
		supervisor.reject(sink, attemptID, runtimev1.AttemptRejectReason_ATTEMPT_REJECT_REASON_INPUT_UNSUPPORTED, "dispatch carries no input snapshot")
		return
	}
	// The frozen input snapshot carries the model contract (model id and
	// budgets, ARCH-AGENT-003); the supervisor resolves the base URL and
	// the API key through the attempt-scoped grant (never persisted).
	analysisInput, err := agent.ParseInput(input.GetCanonicalJson())
	if err != nil {
		supervisor.reject(sink, attemptID, runtimev1.AttemptRejectReason_ATTEMPT_REJECT_REASON_INPUT_UNSUPPORTED, "input snapshot carries no model contract")
		return
	}
	grant, ok := supervisor.primaryGrant(input, "chat_model")
	if !ok {
		supervisor.reject(sink, attemptID, runtimev1.AttemptRejectReason_ATTEMPT_REJECT_REASON_INPUT_UNSUPPORTED, "dispatch lacks the chat_model grant")
		return
	}
	grantCtx, grantCancel := context.WithTimeout(ctx, 15*time.Second)
	bearer, bearerErr := supervisor.Channel.BearerToken()
	if bearerErr != nil {
		grantCancel()
		supervisor.proposeFailure(sink, attemptID, "initial_analysis_output_v1", "读取状态卷 token 失败: "+bearerErr.Error())
		return
	}
	payload, err := client.FetchCredentialGrant(metadata.NewOutgoingContext(grantCtx, metadata.Pairs("authorization", "Bearer "+bearer)), &runtimev1.FetchCredentialGrantRequest{
		GrantId: grant.GetGrantId(), AttemptId: attemptID, BootId: binding.BootID, ConnectionEpoch: binding.Epoch,
	})
	grantCancel()
	if err != nil || payload.GetModelProvider() == nil {
		supervisor.proposeFailure(sink, attemptID, "initial_analysis_output_v1", "获取模型凭据 grant 失败")
		return
	}
	var config plinthconnections.ModelProviderConfig
	if err := json.Unmarshal(payload.GetRevisionConfigJson(), &config); err != nil || config.BaseURL == "" {
		supervisor.proposeFailure(sink, attemptID, "initial_analysis_output_v1", "模型供应商 revision 配置无法解析")
		return
	}
	runner := &worker.Runner{
		Sink: sink, Channel: supervisor.Channel, Client: client,
		Artifacts: supervisor.Channel.Artifacts,
		Binding:   binding,
		Config: worker.RunnerConfig{
			WorkspaceRoot: supervisor.WorkspaceRoot,
			ModelContract: model.Contract{
				ModelID: analysisInput.ModelContract.ModelID, BaseURL: config.BaseURL,
				APIKey:        payload.GetModelProvider().GetApiKey(),
				ContextBudget: analysisInput.ModelContract.ContextBudgetTokens,
				MaxOutput:     analysisInput.ModelContract.MaxOutputTokens,
				Streaming:     true,
			},
		},
	}
	runner.Run(ctx, attemptID, dispatch)
}

// primaryGrant returns the dispatch grant with the given purpose.
func (supervisor *Supervisor) primaryGrant(input *runtimev1.AttemptInputSnapshot, purpose string) (*runtimev1.ConnectionGrant, bool) {
	for _, grant := range input.GetConnectionGrants() {
		if grant.GetPurpose() == purpose {
			return grant, true
		}
	}
	return nil, false
}

// runProbe keeps the T07/T08 deterministic probe slice.
func (supervisor *Supervisor) runProbe(parent context.Context, sink *runtime.FrameSink, client runtimev1.RuntimeControlClient, dispatch *runtimev1.DispatchAttempt, binding runtime.DispatchBinding, stopTask func(int64) bool) {
	attemptID := dispatch.GetAttemptId()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	supervisor.Channel.RegisterTask(attemptID, cancel)
	defer stopTask(attemptID)
	if err := sink.Send(&runtimev1.ControlEnvelope{
		CorrelationId: uint64(attemptID),
		Msg:           &runtimev1.ControlEnvelope_AttemptAccept{AttemptAccept: &runtimev1.AttemptAccept{AttemptId: attemptID}},
	}); err != nil {
		sharedops.LogEvent("plinth", "error", "probe.accept_send", err.Error())
		return
	}
	sharedops.LogEvent("plinth", "info", "probe.accepted", fmt.Sprintf("attempt=%d", attemptID))

	input := dispatch.GetInput()
	grants := input.GetConnectionGrants()
	if len(grants) == 0 {
		supervisor.proposeFailure(sink, attemptID, "connection_probe_v1", "派发缺少凭据 grant")
		return
	}
	// Pick the primary grant: thanos/kubernetes probes carry one; the
	// model_provider qualification carries chat + embedding and every
	// action fetches through the chat grant.
	grant := grants[0]
	for _, candidate := range grants {
		if candidate.GetPurpose() == "model_probe_chat" || candidate.GetPurpose() == "thanos_probe" || candidate.GetPurpose() == "kubernetes_probe" {
			grant = candidate
			break
		}
	}
	grantCtx, grantCancel := context.WithTimeout(ctx, 15*time.Second)
	bearer, bearerErr := supervisor.Channel.BearerToken()
	if bearerErr != nil {
		grantCancel()
		supervisor.proposeFailure(sink, attemptID, "connection_probe_v1", "读取状态卷 token 失败: "+bearerErr.Error())
		return
	}
	grantPayload, err := client.FetchCredentialGrant(metadata.NewOutgoingContext(grantCtx, metadata.Pairs("authorization", "Bearer "+bearer)), &runtimev1.FetchCredentialGrantRequest{
		GrantId:         grant.GetGrantId(),
		AttemptId:       attemptID,
		BootId:          binding.BootID,
		ConnectionEpoch: binding.Epoch,
	})
	grantCancel()
	if err != nil {
		supervisor.proposeFailure(sink, attemptID, "connection_probe_v1", "获取凭据 grant 失败: "+err.Error())
		return
	}

	startedAt := time.Now().UTC()
	var (
		outcome    string
		detailJSON json.RawMessage
		schemaKind string
	)
	switch grantPayload.GetConnectionType() {
	case "thanos":
		var config plinthconnections.ThanosConfig
		var secret plinthconnections.ThanosSecret
		configErr := json.Unmarshal(grantPayload.GetRevisionConfigJson(), &config)
		if grantPayload.GetThanos() != nil {
			secret = plinthconnections.ThanosSecret{Username: grantPayload.GetThanos().GetUsername(), Password: grantPayload.GetThanos().GetPassword()}
		}
		schemaKind = "connection_probe_thanos_v1"
		if configErr != nil {
			outcome, detailJSON = "failed", mustJSON(map[string]any{"kind": "thanos", "query": "vector(1)", "error": "revision 配置无法解析: " + configErr.Error()})
			break
		}
		detail, runErr := plinthconnections.RunThanosProbe(ctx, config, secret)
		outcome = "passed"
		if runErr != nil {
			outcome = "failed"
			sharedops.LogEvent("plinth", "info", "probe.thanos_failed", runErr.Error())
			detailJSON = mustJSON(withError(mustJSON(detail), runErr))
		} else {
			detailJSON = mustJSON(detail)
		}
	case "kubernetes":
		var config plinthconnections.KubernetesConfig
		var secret plinthconnections.KubernetesSecret
		configErr := json.Unmarshal(grantPayload.GetRevisionConfigJson(), &config)
		if grantPayload.GetKubernetes() != nil {
			secret = plinthconnections.KubernetesSecret{Kubeconfig: grantPayload.GetKubernetes().GetKubeconfig()}
		}
		schemaKind = "connection_probe_kubernetes_v1"
		if configErr != nil {
			outcome, detailJSON = "failed", mustJSON(map[string]any{"kind": "kubernetes", "effectiveNamespace": "", "error": "revision 配置无法解析: " + configErr.Error()})
			break
		}
		detail, runErr := plinthconnections.RunKubernetesProbe(ctx, config, secret)
		outcome = "passed"
		if runErr != nil {
			outcome = "failed"
			sharedops.LogEvent("plinth", "info", "probe.kubernetes_failed", runErr.Error())
			detailJSON = mustJSON(withError(mustJSON(detail), runErr))
		} else {
			detailJSON = mustJSON(detail)
		}
	case "model_provider":
		sharedops.LogEvent("plinth", "info", "probe.model_provider_start", fmt.Sprintf("attempt=%d type=%s", attemptID, grantPayload.GetConnectionType()))
		var config plinthconnections.ModelProviderConfig
		var secret plinthconnections.ModelProviderSecret
		configErr := json.Unmarshal(grantPayload.GetRevisionConfigJson(), &config)
		if grantPayload.GetModelProvider() != nil {
			secret = plinthconnections.ModelProviderSecret{APIKey: grantPayload.GetModelProvider().GetApiKey()}
		}
		schemaKind = "connection_probe_model_provider_v1"
		if configErr != nil {
			sharedops.LogEvent("plinth", "error", "probe.model_provider_config", configErr.Error()+" raw="+string(grantPayload.GetRevisionConfigJson()))
			outcome, detailJSON = "failed", mustJSON(map[string]any{"kind": "model_provider", "error": "revision 配置无法解析: " + configErr.Error()})
			break
		}
		probeCtx := modelprovider.WithAttempt(ctx, attemptID)
		ledger := &modelprovider.StreamLedger{Sink: sink, Channel: supervisor.Channel}
		result := modelprovider.Run(probeCtx, modelprovider.Config{Type: config.Type, BaseURL: config.BaseURL, ChatModelID: config.ChatModelID, EmbeddingModelID: config.EmbeddingModelID, ContextBudgetTokens: config.ContextBudgetTokens, MaxOutputTokens: config.MaxOutputTokens}, secret.APIKey, config.EmbeddingModelID != "", ledger)
		outcome = "failed"
		if result.Passed {
			outcome = "passed"
		}
		detailJSON = mustJSON(modelProviderDetail(result, config))
	default:
		supervisor.reject(sink, attemptID, runtimev1.AttemptRejectReason_ATTEMPT_REJECT_REASON_INPUT_UNSUPPORTED, "model_provider capability probes arrive with T08")
		return
	}

	finishedAt := time.Now().UTC()
	payload := map[string]any{
		"outcome":    outcome,
		"detail":     detailJSON,
		"startedAt":  startedAt.Format(time.RFC3339Nano),
		"finishedAt": finishedAt.Format(time.RFC3339Nano),
	}
	canonical, err := json.Marshal(payload)
	if err != nil {
		supervisor.proposeFailure(sink, attemptID, schemaKind, "结果序列化失败: "+err.Error())
		return
	}
	digest := sha256.Sum256(canonical)
	if err := sink.Send(&runtimev1.ControlEnvelope{
		CorrelationId: uint64(attemptID),
		Msg: &runtimev1.ControlEnvelope_ResultProposal{ResultProposal: &runtimev1.ResultProposal{
			AttemptId:       attemptID,
			BootId:          binding.BootID,
			ConnectionEpoch: binding.Epoch,
			Outcome:         outcomeFor(outcome),
			Payload: &runtimev1.ResultPayload{
				SchemaKind:    schemaKind,
				CanonicalJson: canonical,
				ContentDigest: digest[:],
			},
		}},
	}); err != nil {
		sharedops.LogEvent("plinth", "error", "probe.result_send", err.Error())
	}
}

// HandleCancelAttempt stops one running probe and acknowledges.
func (supervisor *Supervisor) HandleCancelAttempt(ctx context.Context, sink *runtime.FrameSink, cancel *runtimev1.CancelAttempt, stopTask func(int64) bool) {
	stopped := stopTask(cancel.GetAttemptId())
	if err := sink.Send(&runtimev1.ControlEnvelope{
		CorrelationId: uint64(cancel.GetAttemptId()),
		Msg:           &runtimev1.ControlEnvelope_CancelAck{CancelAck: &runtimev1.CancelAck{AttemptId: cancel.GetAttemptId()}},
	}); err != nil {
		sharedops.LogEvent("plinth", "error", "probe.cancel_ack_send", err.Error())
	}
	sharedops.LogEvent("plinth", "info", "probe.cancelled", fmt.Sprintf("attempt=%d stopped=%v", cancel.GetAttemptId(), stopped))

}

func (supervisor *Supervisor) reject(sink *runtime.FrameSink, attemptID int64, reason runtimev1.AttemptRejectReason, detail string) {
	if err := sink.Send(&runtimev1.ControlEnvelope{
		CorrelationId: uint64(attemptID),
		Msg:           &runtimev1.ControlEnvelope_AttemptReject{AttemptReject: &runtimev1.AttemptReject{AttemptId: attemptID, Reason: reason}},
	}); err != nil {
		sharedops.LogEvent("plinth", "error", "probe.reject_send", err.Error())
	}
}

func (supervisor *Supervisor) proposeFailure(sink *runtime.FrameSink, attemptID int64, schemaKind, message string) {
	startedAt := time.Now().UTC()
	payload := map[string]any{
		"outcome":    "failed",
		"detail":     map[string]any{"error": message},
		"startedAt":  startedAt.Format(time.RFC3339Nano),
		"finishedAt": startedAt.Format(time.RFC3339Nano),
	}
	canonical, _ := json.Marshal(payload)
	digest := sha256.Sum256(canonical)
	_ = sink.Send(&runtimev1.ControlEnvelope{
		CorrelationId: uint64(attemptID),
		Msg: &runtimev1.ControlEnvelope_ResultProposal{ResultProposal: &runtimev1.ResultProposal{
			AttemptId:         attemptID,
			BootId:            sink.BootID(),
			ConnectionEpoch:   sink.Epoch(),
			Outcome:           runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED,
			TerminationReason: runtimev1.TerminationReason_TERMINATION_REASON_INVALID_RESPONSE,
			Payload: &runtimev1.ResultPayload{
				SchemaKind:    schemaKind,
				CanonicalJson: canonical,
				ContentDigest: digest[:],
			},
		}},
	})
	sharedops.LogEvent("plinth", "info", "probe.proposed_failure", message)
}

func outcomeFor(outcome string) runtimev1.AttemptOutcome {
	if outcome == "passed" {
		return runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED
	}
	return runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED
}

func mustJSON(value any) json.RawMessage {
	body, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage("{}")
	}
	return body
}

// withError attaches the deterministic failure reason to a typed detail so
// the stored detail_json explains why the probe failed.
func withError(detail json.RawMessage, err error) json.RawMessage {
	merged := map[string]any{}
	if json.Unmarshal(detail, &merged) == nil {
		merged["error"] = err.Error()
	}
	if body, marshalErr := json.Marshal(merged); marshalErr == nil {
		return body
	}
	return detail
}

// modelProviderDetail builds the frozen typed detail for the qualification.
func modelProviderDetail(result modelprovider.Outcome, config plinthconnections.ModelProviderConfig) map[string]any {
	detail := map[string]any{
		"kind":                       "model_provider",
		"chatModelId":                result.ChatModelID,
		"contextBudgetTokens":        config.ContextBudgetTokens,
		"maxOutputTokens":            config.MaxOutputTokens,
		"streamingSupported":         result.Capability.StreamingSupported,
		"nativeToolCallingSupported": result.Capability.NativeToolCalling,
		"multiToolCallSupported":     result.Capability.MultiToolCall,
		"cancellationObserved":       result.Capability.CancellationObserved,
		"usageObserved":              result.Capability.UsageObserved,
		"requestIdObserved":          result.Capability.RequestIDObserved,
		"embeddingSupported":         result.Capability.EmbeddingSupported,
	}
	if result.Capability.EmbeddingSupported {
		detail["embeddingModelId"] = result.EmbeddingModelID
		detail["embeddingVectorDim"] = result.Capability.EmbeddingVectorDim
	} else {
		detail["embeddingModelId"] = nil
		detail["embeddingVectorDim"] = nil
	}
	if !result.Passed {
		detail["error"] = result.Detail
	}
	return detail
}
