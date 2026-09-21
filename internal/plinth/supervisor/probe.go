package supervisor

// probe.go: model_provider 资格探测（CONTEXT「模型调用边界」：Provider
// revision 在启用前必须由 Plinth supervisor 真实执行 Chat streaming、
// native/multi Tool Call、取消、usage/request ID 与 Embedding/dimension
// 探测，且不启动 Agent worker）。metrics 类型探测已随 ADR-0011 移交
// Quoin 本地执行，派发到这里的按 INPUT_UNSUPPORTED 拒绝。
//
// 形态与 runEmbedding 相同（supervisor-direct）：AttemptAccept →
// FetchCredentialGrant(model_probe_chat) → modelprovider.Run 六项冻结动作
// （每个动作一对 Begin/CompleteModelCall ledger 往返，Quoin 侧按
// model_probe_chat/model_probe_embedding purpose 下发凭据）→ 封闭
// connection_probe_model_provider_v1 结果。

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	plinthconnections "github.com/Suknna/quoin/internal/plinth/connections"
	"github.com/Suknna/quoin/internal/plinth/modelprovider"
	"github.com/Suknna/quoin/internal/plinth/runtime"
)

// probeResultSchemaKind 是资格探测结果的封闭 schema kind（Quoin 侧
// parseTypedChild 与 enable 闭包触发器按此词表裁决）。
const probeResultSchemaKind = "connection_probe_model_provider_v1"

// runProbe 执行一次 model_provider 资格探测到封闭终态提案。
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
	// 取得凭据前失败也必须以 typed probe result 契约收口（Quoin 侧
	// parseTypedChild 只认 connection_probe_model_provider_v1；通用 failure
	// payload 会被拒绝并遗留 Running attempt）。detail 与日志都只携带稳定
	// 非秘密错误码——RPC 错误原文/供应商响应细节可能含秘密，绝不外发；
	// 配置派生列由 Quoin 从冻结 revision 权威补齐。
	startedAt := time.Now().UTC()
	typedFailure := func(errorCode string) {
		sharedops.LogEvent("plinth", "info", "probe.pre_credential_failure", fmt.Sprintf("attempt=%d code=%s", attemptID, errorCode))
		supervisor.proposeProbeResult(sink, attemptID, binding, startedAt, "failed", mustJSON(map[string]any{"kind": "model_provider", "error": errorCode}))
	}
	// 主 grant 是 model_probe_chat：六项动作经同一凭据信道获取（embedding
	// 动作由 Quoin 侧 BeginModelCall 按 operation 切到 model_probe_embedding
	// purpose）。metrics 探测 grant（prometheus_probe/thanos_probe）已由
	// Quoin 本地执行，派发到这里的按不支持拒绝。
	var grant *runtimev1.ConnectionGrant
	for _, candidate := range grants {
		if candidate.GetPurpose() == "model_probe_chat" {
			grant = candidate
			break
		}
	}
	if grant == nil {
		typedFailure("missing_model_probe_chat_grant")
		return
	}
	grantCtx, grantCancel := context.WithTimeout(ctx, 15*time.Second)
	grantPayload, err := client.FetchCredentialGrant(grantCtx, &runtimev1.FetchCredentialGrantRequest{
		GrantId:         grant.GetGrantId(),
		AttemptId:       attemptID,
		BootId:          binding.BootID,
		ConnectionEpoch: binding.Epoch,
	})
	grantCancel()
	if err != nil {
		// RPC 错误原文（可能含上游响应细节/秘密）不进 typed detail，也不进
		// 日志，只以稳定码收口。
		typedFailure("credential_grant_fetch_failed")
		return
	}
	if grantPayload.GetConnectionType() != "model_provider" || grantPayload.GetModelProvider() == nil {
		typedFailure("credential_grant_type_mismatch")
		return
	}

	var config plinthconnections.ModelProviderConfig
	if err := json.Unmarshal(grantPayload.GetRevisionConfigJson(), &config); err != nil {
		sharedops.LogEvent("plinth", "info", "probe.revision_unparseable", fmt.Sprintf("attempt=%d code=revision_config_unparseable", attemptID))
		supervisor.proposeProbeResult(sink, attemptID, binding, startedAt, "failed", mustJSON(map[string]any{"kind": "model_provider", "error": "revision_config_unparseable"}))
		return
	}
	// 旧 revision 可能缺少预算元数据：在派发边界归一一次，让真实
	// BeginModelCall 事实与 typed child 共享同一份有效固定预算。
	probeConfig := modelprovider.NormalizeProbeConfig(modelprovider.Config{Type: config.Type, BaseURL: config.BaseURL, ChatModelID: config.ChatModelID, EmbeddingModelID: config.EmbeddingModelID, ContextBudgetTokens: config.ContextBudgetTokens, MaxOutputTokens: config.MaxOutputTokens})
	config.ContextBudgetTokens, config.MaxOutputTokens = probeConfig.ContextBudgetTokens, probeConfig.MaxOutputTokens
	probeCtx := modelprovider.WithAttempt(ctx, attemptID)
	ledger := &modelprovider.StreamLedger{Sink: sink, Channel: supervisor.Channel}
	result := modelprovider.Run(probeCtx, probeConfig, grantPayload.GetModelProvider().GetApiKey(), config.EmbeddingModelID != "", ledger)
	outcome := "failed"
	if result.Passed {
		outcome = "passed"
	}
	supervisor.proposeProbeResult(sink, attemptID, binding, startedAt, outcome, mustJSON(modelProviderDetail(result, config)))
}

// proposeProbeResult 封闭一次探测的 canonical 结果（与原 Plinth 探测的
// probeResultJSON 形状一致：outcome/detail/startedAt/finishedAt）。
func (supervisor *Supervisor) proposeProbeResult(sink *runtime.FrameSink, attemptID int64, binding runtime.DispatchBinding, startedAt time.Time, outcome string, detailJSON json.RawMessage) {
	payload := map[string]any{
		"outcome":    outcome,
		"detail":     detailJSON,
		"startedAt":  startedAt.Format(time.RFC3339Nano),
		"finishedAt": time.Now().UTC().Format(time.RFC3339Nano),
	}
	canonical, err := json.Marshal(payload)
	if err != nil {
		supervisor.proposeFailure(sink, attemptID, probeResultSchemaKind, "结果序列化失败: "+err.Error())
		return
	}
	digest := sha256.Sum256(canonical)
	wire := runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED
	if outcome == "passed" {
		wire = runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED
	}
	supervisor.Channel.RegisterResult(&runtimev1.ResultProposal{
		AttemptId: attemptID, BootId: binding.BootID, ConnectionEpoch: binding.Epoch,
		Outcome: wire,
		Payload: &runtimev1.ResultPayload{SchemaKind: probeResultSchemaKind, CanonicalJson: canonical, ContentDigest: digest[:]},
	})
}

func mustJSON(value any) json.RawMessage {
	body, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage("{}")
	}
	return body
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
