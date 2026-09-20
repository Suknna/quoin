package app

// 连接探测收口（T07 的 ADR-0011 形态）：探测不再派发 Plinth，本地执行器
// （local_execution.go）构建与原 supervisor 相同语义的 canonical 载荷后经
// commitProbeResultPayload 收口；本文件同时承载 FetchCredentialGrant RPC——
// 它仍服务 model_provider 模型调用 grant（Plinth）。

import (
	"context"
	"encoding/json"
	"fmt"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/connections"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// sendEnvelope stamps per-direction ids and forwards only through the exact
// boot/epoch stream embedded in the message. A stale dispatch must fail rather
// than a successor stream.
func (service *RuntimeService) sendEnvelope(slot string, envelope *runtimev1.ControlEnvelope) error {
	if service.sendEnvelopeForTest != nil {
		return service.sendEnvelopeForTest(slot, envelope)
	}
	return service.Slots.SendToFenced(slot, envelope.GetBootId(), envelope.GetConnectionEpoch(), func(messageID uint64, sender qruntime.StreamSender) error {
		envelope.MessageId = messageID
		return sender(envelope)
	})
}

// dispatchOperationCorrelation reads the stored association for one dispatch
// frame. The persisted row is the single correlation authority (ADR-0006):
// every producer echoes it verbatim; a legacy row dispatches with an empty
// id and a missing row fails the dispatch.
func dispatchOperationCorrelation(ctx context.Context, db audit.Reader, attemptID int64) (string, error) {
	correlation, _, err := attempt.LoadCorrelation(ctx, db, attemptID)
	if err != nil {
		return "", err
	}
	return correlation.OperationCorrelationID, nil
}

// probeResultJSON is the supervisor's canonical result payload body.
type probeResultJSON struct {
	Outcome    string          `json:"outcome"`
	Detail     json.RawMessage `json:"detail"`
	StartedAt  string          `json:"startedAt"`
	FinishedAt string          `json:"finishedAt"`
}

// commitProbeResultPayload 是连接探测结果的唯一收口：解析 canonical 载荷、
// 校验 outcome 与类型化子行形状，然后在同一事务内提交 header + typed child +
// attempt 终态（RUNTIME-AGENT-010）。本地执行器与（历史协议的）ResultProposal
// 帧处理共用此路径；boot/epoch 是结果的派发绑定围栏。
func (service *RuntimeService) commitProbeResultPayload(ctx context.Context, attemptID int64, schemaKind string, canonical []byte, bootID string, epoch uint64) error {
	if service.Connections == nil {
		return fmt.Errorf("connections not wired")
	}
	var parsed probeResultJSON
	if err := json.Unmarshal(canonical, &parsed); err != nil {
		return fmt.Errorf("result payload unparseable: %w", err)
	}
	if parsed.Outcome != "passed" && parsed.Outcome != "failed" {
		return fmt.Errorf("outcome must be passed or failed")
	}
	resultDigest := sha256DigestOf(schemaKind, canonical)
	typed := connections.TypedProbeResult{
		Outcome:      parsed.Outcome,
		Detail:       parsed.Detail,
		ResultDigest: resultDigest,
		StartedAt:    parsed.StartedAt,
		FinishedAt:   parsed.FinishedAt,
	}
	child, err := parseTypedChild(schemaKind, parsed.Detail)
	if err != nil {
		return err
	}
	return service.Connections.CommitProbeResult(ctx, attemptID, bootID, epoch, typed, child)
}

// handleResultProposal adjudicates a connection_probe result proposal arriving
// on the control stream (a legacy producer; new probes execute locally and call
// commitProbeResultPayload directly).
func (service *RuntimeService) handleResultProposal(ctx context.Context, envelope *runtimev1.ControlEnvelope, proposal *runtimev1.ResultProposal) {
	if service.Connections == nil {
		return
	}
	ack := &runtimev1.ControlEnvelope{
		ConnectionEpoch: envelope.GetConnectionEpoch(),
		CorrelationId:   envelope.GetCorrelationId(),
		BootId:          envelope.GetBootId(),
		Msg:             &runtimev1.ControlEnvelope_ResultAck{ResultAck: &runtimev1.ResultAck{AttemptId: proposal.GetAttemptId()}},
	}
	reject := func(reason string) {
		ack.GetResultAck().Accepted = false
		ack.GetResultAck().Detail = reason
		_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
		sharedops.LogEvent("quoin", "error", "probe.result_rejected", "attempt="+fmt.Sprint(proposal.GetAttemptId())+" reason="+reason)
	}
	payload := proposal.GetPayload()
	if payload == nil || payload.GetSchemaKind() == "" || len(payload.GetCanonicalJson()) == 0 {
		reject("result payload incomplete")
		return
	}
	if !digestMatches(payload.GetCanonicalJson(), payload.GetContentDigest()) {
		reject("content digest mismatch")
		return
	}
	if err := service.commitProbeResultPayload(ctx, proposal.GetAttemptId(), payload.GetSchemaKind(), payload.GetCanonicalJson(), proposal.GetBootId(), proposal.GetConnectionEpoch()); err != nil {
		reject(err.Error())
		return
	}
	ack.GetResultAck().Accepted = true
	_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
}

// FetchCredentialGrant decrypts the attempt-scoped secret after re-checking
// the grant/attempt/boot/epoch binding (RUNTIME-GRANT-001, DATA-CONN-002).
func (service *RuntimeService) FetchCredentialGrant(ctx context.Context, request *runtimev1.FetchCredentialGrantRequest) (*runtimev1.FetchCredentialGrantResponse, error) {
	if service.Connections == nil {
		return nil, status.Error(codes.Unavailable, "connections not wired")
	}
	// The calling runtime's mTLS client identity (CN=plinth) authorizes the
	// call; grant fencing then re-checks the attempt binding (RUNTIME-GRANT-001).
	if !requireComponentIdentity(ctx, qruntime.SlotPlinth) {
		return nil, status.Error(codes.Unauthenticated, "plinth client identity required")
	}
	payload, err := service.Connections.FulfillGrant(ctx, request.GetGrantId(), request.GetAttemptId(), request.GetBootId(), request.GetConnectionEpoch())
	if err != nil {
		sharedops.LogEvent("quoin", "error", "grant.fulfill_denied", fmt.Sprintf("grant=%d attempt=%d: %v", request.GetGrantId(), request.GetAttemptId(), err))
		return nil, status.Error(codes.PermissionDenied, "grant denied")
	}
	response := &runtimev1.FetchCredentialGrantResponse{
		GrantId:                payload.GrantID,
		AttemptId:              payload.AttemptID,
		ConnectionRevisionId:   payload.ConnectionRevisionID,
		CredentialGenerationId: payload.CredentialGeneration,
		ConnectionType:         payload.ConnectionType,
		RevisionConfigJson:     payload.RevisionConfigJSON,
	}
	switch {
	case payload.ModelProvider != nil:
		// ADR-0011 收窄：FetchCredentialGrant 只服务 model_provider grant
		// （Plinth 模型调用）。metrics 凭据不经此信道——出向指标执行已迁至
		// Stele 网关，连接材料按需经 SteleRelay.AcquireConnectionCredential
		// 投递；旧的 thanos oneof 成员已从契约删除。
		response.Secret = &runtimev1.FetchCredentialGrantResponse_ModelProvider{ModelProvider: &runtimev1.ModelProviderCredentialSecret{ApiKey: payload.ModelProvider.APIKey}}
	default:
		return nil, status.Error(codes.PermissionDenied, "grant denied")
	}
	return response, nil
}

// thanosDetail is the supervisor's canonical thanos detail JSON.
type thanosDetail struct {
	Kind         string `json:"kind"`
	Query        string `json:"query"`
	ResponseType string `json:"responseType"`
	SampleCount  int    `json:"sampleCount"`
	SampleValue  string `json:"sampleValue"`
}

// parseTypedChild validates the schema kind and canonical detail against the
// frozen typed-child CHECK contract before the closure transaction runs.
func parseTypedChild(schemaKind string, detail json.RawMessage) (*connections.TypedChild, error) {
	switch schemaKind {
	case "connection_probe_prometheus_v1", "connection_probe_thanos_v1":
		var parsed thanosDetail
		if err := json.Unmarshal(detail, &parsed); err != nil {
			return nil, fmt.Errorf("metrics detail unparseable: %w", err)
		}
		expectedKind := "thanos"
		if schemaKind == "connection_probe_prometheus_v1" {
			expectedKind = "prometheus"
		}
		if parsed.Kind != expectedKind {
			return nil, fmt.Errorf("%s detail kind mismatch", expectedKind)
		}
		// The typed-child columns carry the frozen ACTION constants
		// (query=vector(1), type=vector, count=1 — schema CHECK); the
		// observed values and the failure reason live in detail_json.
		sample := parsed.SampleValue
		if parsed.ResponseType != "vector" || parsed.SampleCount != 1 || sample != "1" {
			sample = ""
		}
		return &connections.TypedChild{Thanos: &connections.ThanosProbeChild{
			Query: "vector(1)", ResponseType: "vector",
			SampleCount: 1, SampleValue: sample,
			DetailJSON: string(detail),
		}}, nil
	case "connection_probe_model_provider_v1":
		var parsed modelProviderDetailJSON
		if err := json.Unmarshal(detail, &parsed); err != nil {
			return nil, fmt.Errorf("model provider detail unparseable: %w", err)
		}
		if parsed.Kind != "model_provider" {
			return nil, fmt.Errorf("model provider detail kind mismatch")
		}
		return &connections.TypedChild{ModelProvider: &connections.ModelProviderProbeChild{
			ChatModelID: parsed.ChatModelID, EmbeddingModelID: parsed.EmbeddingModelID,
			ContextBudgetTokens: parsed.ContextBudgetTokens, MaxOutputTokens: parsed.MaxOutputTokens,
			StreamingSupported: parsed.StreamingSupported, NativeToolCallingSupported: parsed.NativeToolCallingSupported,
			MultiToolCallSupported: parsed.MultiToolCallSupported, CancellationObserved: parsed.CancellationObserved,
			UsageObserved: parsed.UsageObserved, RequestIDObserved: parsed.RequestIDObserved,
			EmbeddingSupported: parsed.EmbeddingSupported, EmbeddingVectorDim: parsed.EmbeddingVectorDim,
			DetailJSON: string(detail),
		}}, nil
	default:
		return nil, fmt.Errorf("unknown probe result schema kind %q", schemaKind)
	}
}

// modelProviderDetailJSON is the supervisor's canonical qualification detail.
type modelProviderDetailJSON struct {
	Kind                       string  `json:"kind"`
	ChatModelID                string  `json:"chatModelId"`
	EmbeddingModelID           *string `json:"embeddingModelId"`
	ContextBudgetTokens        int     `json:"contextBudgetTokens"`
	MaxOutputTokens            int     `json:"maxOutputTokens"`
	StreamingSupported         bool    `json:"streamingSupported"`
	NativeToolCallingSupported bool    `json:"nativeToolCallingSupported"`
	MultiToolCallSupported     bool    `json:"multiToolCallSupported"`
	CancellationObserved       bool    `json:"cancellationObserved"`
	UsageObserved              bool    `json:"usageObserved"`
	RequestIDObserved          bool    `json:"requestIdObserved"`
	EmbeddingSupported         bool    `json:"embeddingSupported"`
	EmbeddingVectorDim         int     `json:"embeddingVectorDim,omitempty"`
}
