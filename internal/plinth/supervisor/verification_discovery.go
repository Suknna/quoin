package supervisor

// Verification discovery uses the same production PromQL request chain as the
// retired refresh worker, but its input/result are Config Verification-owned.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	plinthconnections "github.com/Suknna/quoin/internal/plinth/connections"
	"github.com/Suknna/quoin/internal/plinth/runtime"
	"google.golang.org/grpc/metadata"
)

func (supervisor *Supervisor) runVerificationDiscovery(parent context.Context, sink *runtime.FrameSink, client runtimev1.RuntimeControlClient, dispatch *runtimev1.DispatchAttempt, binding runtime.DispatchBinding, stopTask func(int64) bool) {
	attemptID := dispatch.GetAttemptId()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	supervisor.Channel.RegisterTask(attemptID, cancel)
	defer stopTask(attemptID)
	if sink.Send(&runtimev1.ControlEnvelope{CorrelationId: uint64(attemptID), Msg: &runtimev1.ControlEnvelope_AttemptAccept{AttemptAccept: &runtimev1.AttemptAccept{AttemptId: attemptID}}}) != nil {
		return
	}
	var input struct {
		SchemaKind        string   `json:"schemaKind"`
		AttemptID         int64    `json:"attemptId"`
		VerificationRunID int64    `json:"verificationRunId"`
		DiscoveryKey      string   `json:"discoveryKey"`
		Selector          string   `json:"selector"`
		IdentityLabels    []string `json:"identityLabels"`
		GrantID           int64    `json:"grantId"`
	}
	if dispatch.GetInput() == nil || json.Unmarshal(dispatch.GetInput().GetCanonicalJson(), &input) != nil || input.SchemaKind != "config_verification_discovery_execution_v1" || input.AttemptID != attemptID || input.VerificationRunID != dispatch.GetScopeId() || input.DiscoveryKey == "" || input.Selector == "" {
		supervisor.proposeVerificationDiscovery(sink, attemptID, binding, input, "error", nil, []string{"invalid verification discovery input"}, "query_failed")
		return
	}
	grant, ok := supervisor.primaryGrant(dispatch.GetInput(), "config_thanos_query")
	if !ok || grant.GetGrantId() != input.GrantID {
		supervisor.proposeVerificationDiscovery(sink, attemptID, binding, input, "error", nil, []string{"missing metrics grant"}, "query_failed")
		return
	}
	bearer, err := supervisor.Channel.BearerToken()
	if err != nil {
		supervisor.proposeVerificationDiscovery(sink, attemptID, binding, input, "error", nil, []string{"runtime credential unavailable"}, "query_failed")
		return
	}
	grantCtx, grantCancel := context.WithTimeout(ctx, 15*time.Second)
	payload, err := client.FetchCredentialGrant(metadata.NewOutgoingContext(grantCtx, metadata.Pairs("authorization", "Bearer "+bearer)), &runtimev1.FetchCredentialGrantRequest{GrantId: grant.GetGrantId(), AttemptId: attemptID, BootId: binding.BootID, ConnectionEpoch: binding.Epoch})
	grantCancel()
	if err != nil || payload.GetThanos() == nil {
		supervisor.proposeVerificationDiscovery(sink, attemptID, binding, input, "error", nil, []string{"credential grant unavailable"}, "query_failed")
		return
	}
	var config plinthconnections.ThanosConfig
	if json.Unmarshal(payload.GetRevisionConfigJson(), &config) != nil {
		supervisor.proposeVerificationDiscovery(sink, attemptID, binding, input, "error", nil, []string{"invalid metrics config"}, "query_failed")
		return
	}
	raw, warnings, err := plinthconnections.RunPromQL(ctx, config, plinthconnections.ThanosSecret{Username: payload.GetThanos().GetUsername(), Password: payload.GetThanos().GetPassword(), BearerToken: payload.GetThanos().GetBearerToken()}, "instant", input.Selector, nil, nil)
	if err != nil {
		supervisor.proposeVerificationDiscovery(sink, attemptID, binding, input, "error", nil, []string{err.Error()}, "query_failed")
		return
	}
	if len(warnings) > 0 {
		supervisor.proposeVerificationDiscovery(sink, attemptID, binding, input, "gap", nil, warnings, "partial_response")
		return
	}
	var response struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &response) != nil {
		supervisor.proposeVerificationDiscovery(sink, attemptID, binding, input, "error", nil, []string{"invalid Prometheus response"}, "query_failed")
		return
	}
	series := make([]map[string]any, 0, len(response.Data.Result))
	for _, item := range response.Data.Result {
		if len(item.Value) != 2 {
			supervisor.proposeVerificationDiscovery(sink, attemptID, binding, input, "error", nil, []string{"invalid Prometheus sample"}, "query_failed")
			return
		}
		value, ok := item.Value[1].(string)
		if !ok {
			supervisor.proposeVerificationDiscovery(sink, attemptID, binding, input, "error", nil, []string{"invalid Prometheus value"}, "query_failed")
			return
		}
		series = append(series, map[string]any{"labels": item.Metric, "timestamp": item.Value[0], "value": value})
	}
	supervisor.proposeVerificationDiscovery(sink, attemptID, binding, input, "success", series, nil, "")
}
func (supervisor *Supervisor) proposeVerificationDiscovery(sink *runtime.FrameSink, attemptID int64, binding runtime.DispatchBinding, input struct {
	SchemaKind        string   `json:"schemaKind"`
	AttemptID         int64    `json:"attemptId"`
	VerificationRunID int64    `json:"verificationRunId"`
	DiscoveryKey      string   `json:"discoveryKey"`
	Selector          string   `json:"selector"`
	IdentityLabels    []string `json:"identityLabels"`
	GrantID           int64    `json:"grantId"`
}, outcome string, series any, messages []string, gapReason string) {
	if series == nil {
		series = []any{}
	}
	var gap any
	if gapReason != "" {
		gap = gapReason
	}
	errors := []string{}
	if outcome == "error" {
		errors = messages
	}
	canonical, err := json.Marshal(map[string]any{"schemaKind": "config_verification_discovery_result_v1", "attemptId": attemptID, "verificationRunId": input.VerificationRunID, "discoveryKey": input.DiscoveryKey, "outcome": outcome, "observedAt": time.Now().UTC().Format(time.RFC3339Nano), "series": series, "warnings": messages, "errors": errors, "gapReason": gap})
	if err != nil {
		return
	}
	digest := sha256.Sum256(canonical)
	_ = sink.Send(&runtimev1.ControlEnvelope{CorrelationId: uint64(attemptID), Msg: &runtimev1.ControlEnvelope_ResultProposal{ResultProposal: &runtimev1.ResultProposal{AttemptId: attemptID, BootId: binding.BootID, ConnectionEpoch: binding.Epoch, Outcome: outcomeFor(map[string]string{"success": "passed", "gap": "failed", "error": "failed"}[outcome]), Payload: &runtimev1.ResultPayload{SchemaKind: "config_verification_discovery_result_v1", CanonicalJson: canonical, ContentDigest: digest[:]}}}})
}
