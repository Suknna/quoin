package supervisor

// Plinth supervisor task slice (ADR-0011): Plinth 是纯推理沙箱——只执行
// 模型推理(四种 agent attempt 经 worker 沙箱 + EMBEDDING 直连模型)。
// 连接探测、来源观测、巡检采集与插件工具执行已全部移交 Quoin(权限/
// 审计/派发)+ Stele(凭证/限流/传输),不再派发 Plinth;收到这些类型的
// DispatchAttempt 一律按 INPUT_UNSUPPORTED 拒绝。

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	plinthagent "github.com/Suknna/quoin/internal/plinth/agent"
	plinthconnections "github.com/Suknna/quoin/internal/plinth/connections"
	"github.com/Suknna/quoin/internal/plinth/model"
	"github.com/Suknna/quoin/internal/plinth/runtime"
	"github.com/Suknna/quoin/internal/plinth/worker"
)

// Supervisor executes dispatched inference attempts on the live channel:
// every agent attempt runs through a fresh sandboxed worker process, and
// embedding attempts run supervisor-direct through the modelprovider
// executor.
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
	// The dispatch's persisted business correlation rides the task context
	// for local diagnostics only (ADR-0006): it grants nothing, and replies
	// join attempts by id instead of echoing any runtime-supplied identity.
	parent = dispatchContext(parent, dispatch)

	// Supervisor scope (ADR-0011): 推理沙箱只承载 EMBEDDING 与四种 agent
	// attempt;CONNECTION_PROBE / OBSERVATION_RUN / INSPECTION_COLLECTION 已
	// 改由 Quoin 直接经 Stele 执行,不应再派发到这里——收到了也显式拒绝。
	switch dispatch.GetAttemptType() {
	case runtimev1.AttemptType_ATTEMPT_TYPE_EMBEDDING:
		supervisor.runEmbedding(parent, sink, client, dispatch, binding, stopTask)
	case runtimev1.AttemptType_ATTEMPT_TYPE_INITIAL_ANALYSIS, runtimev1.AttemptType_ATTEMPT_TYPE_INVESTIGATION, runtimev1.AttemptType_ATTEMPT_TYPE_INSPECTION_ANALYSIS, runtimev1.AttemptType_ATTEMPT_TYPE_KNOWLEDGE_EXTRACTION:
		supervisor.runAgent(parent, sink, client, dispatch, binding, stopTask)
	default:
		supervisor.reject(sink, attemptID, runtimev1.AttemptRejectReason_ATTEMPT_REJECT_REASON_INPUT_UNSUPPORTED, "supervisor does not execute this attempt type")
	}
}

// runAgent drives one initial-analysis, investigation, inspection-analysis
// or knowledge-extraction attempt through a fresh worker process
// (ARCH-WORKER-001/002); the attempt type selects the worker's work mode
// and failure payload schema.
func (supervisor *Supervisor) runAgent(parent context.Context, sink *runtime.FrameSink, client runtimev1.RuntimeControlClient, dispatch *runtimev1.DispatchAttempt, binding runtime.DispatchBinding, stopTask func(int64) bool) {
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
	failureSchema := "initial_analysis_output_v1"
	systemPrompt := plinthagent.SystemPrompt
	if dispatch.GetAttemptType() == runtimev1.AttemptType_ATTEMPT_TYPE_INVESTIGATION {
		failureSchema = "investigation_output_v1"
		switch input.GetAgentVersion() {
		case worker.LegacyInvestigationAgentVersion:
			systemPrompt = plinthagent.LegacyInvestigationSystemPrompt
		case worker.PreviousInvestigationAgentVersion:
			systemPrompt = plinthagent.PreviousInvestigationSystemPrompt
		case worker.KeptInvestigationAgentVersion:
			systemPrompt = plinthagent.KeptInvestigationSystemPrompt
		case worker.WorkerInvestigationAgentVersion:
			systemPrompt = plinthagent.InvestigationSystemPrompt
		default:
			supervisor.reject(sink, attemptID, runtimev1.AttemptRejectReason_ATTEMPT_REJECT_REASON_INPUT_UNSUPPORTED, "unsupported investigation agent version")
			return
		}
	} else if dispatch.GetAttemptType() == runtimev1.AttemptType_ATTEMPT_TYPE_INSPECTION_ANALYSIS {
		failureSchema = "inspection_report_result_v1"
		switch input.GetAgentVersion() {
		case worker.InspectionAnalysisAgentVersion:
			systemPrompt = plinthagent.InspectionSystemPrompt
		case worker.KeptInspectionAnalysisAgentVersion:
			systemPrompt = plinthagent.KeptInspectionSystemPrompt
		case worker.ReportComplianceInspectionAnalysisAgentVersion:
			systemPrompt = plinthagent.ReportComplianceInspectionSystemPrompt
		case worker.PreviousInspectionAnalysisAgentVersion:
			systemPrompt = plinthagent.PreviousInspectionSystemPrompt
		case worker.LegacyInitialAnalysisAgentVersion:
			systemPrompt = plinthagent.LegacyInspectionSystemPrompt
		default:
			supervisor.reject(sink, attemptID, runtimev1.AttemptRejectReason_ATTEMPT_REJECT_REASON_INPUT_UNSUPPORTED, "unsupported inspection agent version")
			return
		}
	} else if dispatch.GetAttemptType() == runtimev1.AttemptType_ATTEMPT_TYPE_INITIAL_ANALYSIS {
		// 初步分析的 prompt 已分代：旧身份必须绑定冻结的上一代 prompt，
		// BeginModelCall 记录的 prompt_digest 才与实际渲染一致。
		switch input.GetAgentVersion() {
		case worker.WorkerAgentVersion:
			systemPrompt = plinthagent.SystemPrompt
		case worker.PreviousAnalysisAgentVersion:
			systemPrompt = plinthagent.KeptAnalysisSystemPrompt
		case worker.LegacyInitialAnalysisAgentVersion:
			systemPrompt = plinthagent.PreviousAnalysisSystemPrompt
		default:
			supervisor.reject(sink, attemptID, runtimev1.AttemptRejectReason_ATTEMPT_REJECT_REASON_INPUT_UNSUPPORTED, "unsupported initial-analysis agent version")
			return
		}
	} else if dispatch.GetAttemptType() == runtimev1.AttemptType_ATTEMPT_TYPE_KNOWLEDGE_EXTRACTION {
		failureSchema = worker.KnowledgeExtractionOutputSchemaKind
		systemPrompt = plinthagent.KnowledgeExtractionSystemPrompt
	}
	failPreAccept := func(detail string) {
		if dispatch.GetAttemptType() == runtimev1.AttemptType_ATTEMPT_TYPE_KNOWLEDGE_EXTRACTION {
			supervisor.reject(sink, attemptID, runtimev1.AttemptRejectReason_ATTEMPT_REJECT_REASON_INTERNAL, detail)
			return
		}
		supervisor.proposeFailure(sink, attemptID, failureSchema, detail)
	}
	// The frozen input snapshot carries the model contract (model id and
	// budgets, ARCH-AGENT-003); the supervisor resolves the base URL and
	// the API key through the attempt-scoped grant (never persisted).
	contract, err := parseModelContract(input.GetCanonicalJson())
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
	payload, err := client.FetchCredentialGrant(grantCtx, &runtimev1.FetchCredentialGrantRequest{
		GrantId: grant.GetGrantId(), AttemptId: attemptID, BootId: binding.BootID, ConnectionEpoch: binding.Epoch,
	})
	grantCancel()
	// ADR-0011 之后 FetchCredentialGrant 只剩 model_provider 一个 oneof
	// 成员:模型推理是 Plinth 保留的唯一凭据消费场景。
	if err != nil || payload.GetModelProvider() == nil {
		failPreAccept("获取模型凭据 grant 失败")
		return
	}
	var config plinthconnections.ModelProviderConfig
	if err := json.Unmarshal(payload.GetRevisionConfigJson(), &config); err != nil || config.BaseURL == "" {
		failPreAccept("模型供应商 revision 配置无法解析")
		return
	}
	runner := &worker.Runner{
		Sink: sink, Channel: supervisor.Channel, Client: client,
		Artifacts: supervisor.Channel.Artifacts,
		Binding:   binding,
		Config: worker.RunnerConfig{
			WorkspaceRoot: supervisor.WorkspaceRoot,
			ModelContract: model.Contract{
				ModelID: contract.ModelContract.ModelID, BaseURL: config.BaseURL,
				APIKey:        payload.GetModelProvider().GetApiKey(),
				ContextBudget: contract.ModelContract.ContextBudgetTokens,
				MaxOutput:     contract.ModelContract.MaxOutputTokens,
				Streaming:     true,
				SystemPrompt:  systemPrompt,
			},
		},
	}
	runner.Run(ctx, attemptID, dispatch)
}

// parseModelContract extracts the frozen chat contract shared by both
// agent input schemas (initial_analysis_v1 / investigation_v1).
func parseModelContract(canonical []byte) (struct {
	ModelContract struct {
		ModelID             string `json:"modelId"`
		ContextBudgetTokens int    `json:"contextBudgetTokens"`
		MaxOutputTokens     int    `json:"maxOutputTokens"`
	} `json:"modelContract"`
}, error,
) {
	var contract struct {
		ModelContract struct {
			ModelID             string `json:"modelId"`
			ContextBudgetTokens int    `json:"contextBudgetTokens"`
			MaxOutputTokens     int    `json:"maxOutputTokens"`
		} `json:"modelContract"`
	}
	if err := json.Unmarshal(canonical, &contract); err != nil {
		return contract, err
	}
	if contract.ModelContract.ModelID == "" {
		return contract, errors.New("model contract missing")
	}
	return contract, nil
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

// HandleCancelAttempt stops one running attempt and acknowledges.
func (supervisor *Supervisor) HandleCancelAttempt(ctx context.Context, sink *runtime.FrameSink, cancel *runtimev1.CancelAttempt, stopTask func(int64) bool) {
	stopped := stopTask(cancel.GetAttemptId())
	if err := sink.Send(&runtimev1.ControlEnvelope{
		CorrelationId: uint64(cancel.GetAttemptId()),
		Msg:           &runtimev1.ControlEnvelope_CancelAck{CancelAck: &runtimev1.CancelAck{AttemptId: cancel.GetAttemptId()}},
	}); err != nil {
		sharedops.LogEvent("plinth", "error", "supervisor.cancel_ack_send", err.Error())
	}
	sharedops.LogEvent("plinth", "info", "supervisor.cancelled", fmt.Sprintf("attempt=%d stopped=%v", cancel.GetAttemptId(), stopped))
}

func (supervisor *Supervisor) reject(sink *runtime.FrameSink, attemptID int64, reason runtimev1.AttemptRejectReason, detail string) {
	if err := sink.Send(&runtimev1.ControlEnvelope{
		CorrelationId: uint64(attemptID),
		Msg:           &runtimev1.ControlEnvelope_AttemptReject{AttemptReject: &runtimev1.AttemptReject{AttemptId: attemptID, Reason: reason}},
	}); err != nil {
		sharedops.LogEvent("plinth", "error", "supervisor.reject_send", err.Error())
	}
	sharedops.LogEvent("plinth", "info", "supervisor.rejected", fmt.Sprintf("attempt=%d reason=%s", attemptID, reason))
}

// proposeFailure seals a pre-worker technical failure as the attempt's
// terminal result for reliable delivery (used by the agent grant/config
// paths after AttemptAccept is still possible).
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
	sharedops.LogEvent("plinth", "info", "supervisor.proposed_failure", message)
}
