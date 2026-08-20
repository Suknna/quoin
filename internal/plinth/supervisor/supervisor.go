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
	plinthconnections "github.com/Suknna/quoin/internal/plinth/connections"
	"github.com/Suknna/quoin/internal/plinth/runtime"
	"google.golang.org/grpc/metadata"
)

// Supervisor executes connection_probe dispatches on the live channel.
type Supervisor struct {
	Channel *runtime.Channel
}

// HandleDispatchAttempt runs one dispatched probe attempt to a typed
// terminal result proposal.
func (supervisor *Supervisor) HandleDispatchAttempt(parent context.Context, sink *runtime.FrameSink, client runtimev1.RuntimeControlClient, dispatch *runtimev1.DispatchAttempt, stopTask func(int64) bool) {
	attemptID := dispatch.GetAttemptId()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	supervisor.Channel.RegisterTask(attemptID, cancel)
	defer stopTask(attemptID)

	// Supervisor scope: only connection_probe attempts exist here
	// (RUNTIME-SCOPE: agent-worker attempts arrive with their own tickets).
	if dispatch.GetAttemptType() != runtimev1.AttemptType_ATTEMPT_TYPE_CONNECTION_PROBE {
		supervisor.reject(sink, attemptID, runtimev1.AttemptRejectReason_ATTEMPT_REJECT_REASON_INPUT_UNSUPPORTED, "supervisor only executes connection_probe attempts")
		return
	}
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
	if len(grants) != 1 {
		supervisor.proposeFailure(sink, attemptID, "connection_probe_v1", "派发缺少唯一的凭据 grant")
		return
	}
	grantCtx, grantCancel := context.WithTimeout(ctx, 15*time.Second)
	bearer, bearerErr := supervisor.Channel.BearerToken()
	if bearerErr != nil {
		grantCancel()
		supervisor.proposeFailure(sink, attemptID, "connection_probe_v1", "读取状态卷 token 失败: "+bearerErr.Error())
		return
	}
	grant, err := client.FetchCredentialGrant(metadata.NewOutgoingContext(grantCtx, metadata.Pairs("authorization", "Bearer "+bearer)), &runtimev1.FetchCredentialGrantRequest{
		GrantId:         grants[0].GetGrantId(),
		AttemptId:       attemptID,
		BootId:          sink.BootID(),
		ConnectionEpoch: sink.Epoch(),
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
	switch grant.GetConnectionType() {
	case "thanos":
		var config plinthconnections.ThanosConfig
		var secret plinthconnections.ThanosSecret
		configErr := json.Unmarshal(grant.GetRevisionConfigJson(), &config)
		if grant.GetThanos() != nil {
			secret = plinthconnections.ThanosSecret{Username: grant.GetThanos().GetUsername(), Password: grant.GetThanos().GetPassword()}
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
		configErr := json.Unmarshal(grant.GetRevisionConfigJson(), &config)
		if grant.GetKubernetes() != nil {
			secret = plinthconnections.KubernetesSecret{Kubeconfig: grant.GetKubernetes().GetKubeconfig()}
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
	default:
		// model_provider capability probes are T08's supervisor slice.
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
			BootId:          sink.BootID(),
			ConnectionEpoch: sink.Epoch(),
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
