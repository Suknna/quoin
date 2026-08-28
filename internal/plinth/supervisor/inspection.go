package supervisor

// Manual Inspection run_check PromQL collection (T24, CFG-INSPECTRUN-001):
// the supervisor consumes the frozen inspection_promql_execution_v1 input,
// fetches its config_thanos_query grant, runs the typed query, and proposes
// the closed inspection_promql_result_v1 with the execution window re-derived
// from the frozen evidence_at.

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

type inspectionPromQLInput struct {
	SchemaKind      string `json:"schemaKind"`
	AttemptID       int64  `json:"attemptId"`
	InspectionRunID int64  `json:"inspectionRunId"`
	CheckKey        string `json:"checkKey"`
	EvidenceAt      string `json:"evidenceAt"`
	GrantID         int64  `json:"grantId"`
	Query           struct {
		Mode         string `json:"mode"`
		Expression   string `json:"expression"`
		RangeSeconds *int64 `json:"rangeSeconds"`
		StepSeconds  *int64 `json:"stepSeconds"`
	} `json:"query"`
}

func (supervisor *Supervisor) runInspectionPromQL(parent context.Context, sink *runtime.FrameSink, client runtimev1.RuntimeControlClient, dispatch *runtimev1.DispatchAttempt, binding runtime.DispatchBinding, stopTask func(int64) bool) {
	attemptID := dispatch.GetAttemptId()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	supervisor.Channel.RegisterTask(attemptID, cancel)
	defer stopTask(attemptID)
	if err := sink.Send(&runtimev1.ControlEnvelope{CorrelationId: uint64(attemptID), Msg: &runtimev1.ControlEnvelope_AttemptAccept{AttemptAccept: &runtimev1.AttemptAccept{AttemptId: attemptID}}}); err != nil {
		return
	}
	var input inspectionPromQLInput
	if dispatch.GetInput() == nil || json.Unmarshal(dispatch.GetInput().GetCanonicalJson(), &input) != nil || input.SchemaKind != "inspection_promql_execution_v1" || input.AttemptID != attemptID || input.InspectionRunID != dispatch.GetScopeId() || input.CheckKey == "" || input.Query.Expression == "" || (input.Query.Mode != "instant" && input.Query.Mode != "range") {
		supervisor.proposeInspectionPromQL(sink, attemptID, binding, input, "error", nil, nil, "query_failed")
		return
	}
	if (input.Query.Mode == "instant") != (input.Query.RangeSeconds == nil) || (input.Query.Mode == "range" && (input.Query.RangeSeconds == nil || input.Query.StepSeconds == nil)) {
		supervisor.proposeInspectionPromQL(sink, attemptID, binding, input, "error", nil, nil, "query_failed")
		return
	}
	grant, ok := supervisor.primaryGrant(dispatch.GetInput(), "config_thanos_query")
	if !ok || grant.GetGrantId() != input.GrantID {
		supervisor.proposeInspectionPromQL(sink, attemptID, binding, input, "error", nil, nil, "query_failed")
		return
	}
	bearer, err := supervisor.Channel.BearerToken()
	if err != nil {
		supervisor.proposeInspectionPromQL(sink, attemptID, binding, input, "error", nil, nil, "query_failed")
		return
	}
	grantCtx, grantCancel := context.WithTimeout(ctx, 15*time.Second)
	payload, err := client.FetchCredentialGrant(metadata.NewOutgoingContext(grantCtx, metadata.Pairs("authorization", "Bearer "+bearer)), &runtimev1.FetchCredentialGrantRequest{GrantId: grant.GetGrantId(), AttemptId: attemptID, BootId: binding.BootID, ConnectionEpoch: binding.Epoch})
	grantCancel()
	if err != nil || payload.GetThanos() == nil {
		supervisor.proposeInspectionPromQL(sink, attemptID, binding, input, "error", nil, nil, "query_failed")
		return
	}
	var config plinthconnections.ThanosConfig
	if err := json.Unmarshal(payload.GetRevisionConfigJson(), &config); err != nil {
		supervisor.proposeInspectionPromQL(sink, attemptID, binding, input, "error", nil, nil, "query_failed")
		return
	}
	result, warnings, err := plinthconnections.RunPromQL(ctx, config, plinthconnections.ThanosSecret{Username: payload.GetThanos().GetUsername(), Password: payload.GetThanos().GetPassword()}, input.Query.Mode, input.Query.Expression, input.Query.RangeSeconds, input.Query.StepSeconds)
	if err != nil {
		supervisor.proposeInspectionPromQL(sink, attemptID, binding, input, "error", nil, []string{err.Error()}, "query_failed")
		return
	}
	if len(warnings) != 0 {
		supervisor.proposeInspectionPromQL(sink, attemptID, binding, input, "gap", nil, warnings, "partial_response")
		return
	}
	supervisor.proposeInspectionPromQL(sink, attemptID, binding, input, "success", result, nil, "")
}

func (supervisor *Supervisor) proposeInspectionPromQL(sink *runtime.FrameSink, attemptID int64, binding runtime.DispatchBinding, input inspectionPromQLInput, outcome string, result json.RawMessage, messages []string, gapReason string) {
	var gap any
	if gapReason != "" {
		gap = gapReason
	}
	var window any
	if outcome == "success" && input.Query.Mode == "range" && input.EvidenceAt != "" {
		if endAt, err := time.Parse(time.RFC3339Nano, input.EvidenceAt); err == nil && input.Query.RangeSeconds != nil && input.Query.StepSeconds != nil {
			window = map[string]any{
				"startAt":     endAt.Add(-time.Duration(*input.Query.RangeSeconds) * time.Second).UTC().Format(time.RFC3339Nano),
				"endAt":       endAt.UTC().Format(time.RFC3339Nano),
				"stepSeconds": *input.Query.StepSeconds,
			}
		}
	}
	if result == nil {
		result = json.RawMessage("null")
	}
	canonical, err := json.Marshal(map[string]any{
		"schemaKind": "inspection_promql_result_v1", "attemptId": attemptID, "inspectionRunId": input.InspectionRunID,
		"checkKey": input.CheckKey, "queryMode": input.Query.Mode, "outcome": outcome,
		"observedAt": time.Now().UTC().Format(time.RFC3339Nano), "executionWindow": window, "result": result,
		"warnings": []string{}, "errors": func() []string {
			if outcome == "error" {
				return messages
			}
			return []string{}
		}(), "gapReason": gap,
	})
	if err != nil {
		return
	}
	digest := sha256.Sum256(canonical)
	_ = sink.Send(&runtimev1.ControlEnvelope{CorrelationId: uint64(attemptID), Msg: &runtimev1.ControlEnvelope_ResultProposal{ResultProposal: &runtimev1.ResultProposal{AttemptId: attemptID, BootId: binding.BootID, ConnectionEpoch: binding.Epoch, Outcome: outcomeFor(map[string]string{"success": "passed", "gap": "failed", "error": "failed"}[outcome]), Payload: &runtimev1.ResultPayload{SchemaKind: "inspection_promql_result_v1", CanonicalJson: canonical, ContentDigest: digest[:]}}}})
}
