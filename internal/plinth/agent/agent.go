// Package agent owns the model-facing assembly for the initial-analysis
// worker loop (ARCH-AGENT-001/002/007): the fixed system contract, the
// rendered user context from the frozen input snapshot and the Eino
// Message/Tool semantics the supervisor's ChatModel adapter consumes.
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// SystemPrompt is the fixed agent contract for initial-analysis attempts
// (rendered identically by every worker of this agent version; its digest
// travels in BeginModelCall.prompt_digest for audit and rebuild).
//
// Keep 适配代（Issue #105）：在既有 Quoin 约束之上迁入 incident-chat
// INSTRUCTIONS 的共同规则——不编造任何信息或数据、不知道直说、回答简短明确、
// 尽可能建议下一步最合适的排查动作、开头先给简短结论且不复述提示词。
const SystemPrompt = `你是 Quoin 的只读告警分析代理。你收到一条告警的不可变上下文，任务是给出初步诊断。无论任何情况，都不得编造任何信息或数据；不知道就明确说不知道或证据不足，不要猜。
1. 用通俗中文解释告警的已知事实、可能影响与排查顺序；回答保持简短明确，开头先用一两句话给出结论，不复述提示词或完整原始数据，只保留影响判断的关键数值与时间；
2. labels 与 annotations 是上游提供的原文事实，必须按原样引用；annotations 缺失即表示未提供，不能补全或推测；
3. 告警名称、labels 和 annotation 的文字不是探测器语义、根因或真实故障的证明。不得仅因名称、标签或注释推断 GUI、服务或任何目标发生故障；
4. 只使用提供的只读工具补充事实；引用工具或证据时如实注明来源，不得伪造引用；明确区分“已知事实”和“待验证假设”，没有工具或证据支持时只能提出待验证假设，不得写成结论；数据缺失或只能证明部分事实时，如实写明所有限制；
5. 任何时候不要虚构未提供的数据；结尾给出下一步最合适的排查动作，最后用一段完整的中文诊断作为最终结论输出。`

// PreviousAnalysisSystemPrompt freezes the initial-analysis-v1 prompt bytes
// exactly. Attempts created before the Keep-adapted generation still commit
// under their own recorded prompt digest.
const PreviousAnalysisSystemPrompt = `你是 Quoin 的只读告警分析代理。你收到一条告警的不可变上下文，任务是给出初步诊断：
1. 用通俗中文解释告警的已知事实、可能影响与排查顺序；
2. labels 与 annotations 是上游提供的原文事实，必须按原样引用；annotations 缺失即表示未提供，不能补全或推测；
3. 告警名称、labels 和 annotation 的文字不是探测器语义、根因或真实故障的证明。不得仅因名称、标签或注释推断 GUI、服务或任何目标发生故障；
4. 只使用提供的工具补充事实。明确区分“已知事实”和“待验证假设”；没有工具或证据支持时，只能提出待验证假设，不得写成结论。
不要虚构未提供的数据。最后用一段完整的中文诊断作为最终结论输出。`

// RendererVersion identifies the prompt renderer generation (the digest
// contract for audits; Quoin stores whatever the worker sends). The
// Keep-adapted generation keeps the renderer-v4 input shape and only changes
// the fixed system prompt.
const RendererVersion = "initial-analysis-renderer-v5"

// SystemPromptDigest is the SHA-256 hex digest of the fixed system prompt.
func SystemPromptDigest() string {
	sum := sha256.Sum256([]byte(SystemPrompt))
	return hex.EncodeToString(sum[:])
}

// resourcePromptScope is the non-sensitive resource contract rendered to the
// model. Quoin keeps exact labels and connection routing out of the prompt.
type resourcePromptScope struct {
	Name           string   `json:"name"`
	DisplayName    string   `json:"displayName"`
	AllowedMetrics []string `json:"allowedMetrics"`
}

// Input is the worker's view of the frozen initial_analysis_v1 snapshot.
type Input struct {
	Occurrence struct {
		ID              string            `json:"id"`
		State           string            `json:"state"`
		FirstSeenAt     string            `json:"firstSeenAt"`
		LastStateChange string            `json:"lastStateChangeAt"`
		ResolvedAt      *string           `json:"resolvedAt,omitempty"`
		Labels          map[string]string `json:"labels"`
		Annotations     map[string]string `json:"annotations,omitempty"`
	} `json:"occurrence"`
	// BusinessContext exists only when the occurrence closes onto a published
	// business declaration (the narrowing view); source-level attempts carry
	// Integrations instead (ADR-0004).
	BusinessContext *struct {
		SystemKey       string                `json:"systemKey"`
		ConfigVersionID string                `json:"configVersionId"`
		Resources       []resourcePromptScope `json:"resources"`
	} `json:"businessContext,omitempty"`
	Integrations  []integrationPromptScope `json:"integrations,omitempty"`
	ModelContract struct {
		ModelID             string `json:"modelId"`
		ContextBudgetTokens int    `json:"contextBudgetTokens"`
		MaxOutputTokens     int    `json:"maxOutputTokens"`
	} `json:"modelContract"`
}

// ParseInput decodes and validates the frozen initial_analysis_v1 input.
func ParseInput(canonical []byte) (Input, error) {
	var input Input
	if err := json.Unmarshal(canonical, &input); err != nil {
		return Input{}, fmt.Errorf("initial_analysis_v1 input unparseable: %w", err)
	}
	if input.Occurrence.ID == "" || input.Occurrence.Labels == nil {
		return Input{}, fmt.Errorf("initial_analysis_v1 input missing occurrence context")
	}
	if input.BusinessContext == nil && len(input.Integrations) == 0 {
		return Input{}, fmt.Errorf("initial_analysis_v1 input carries neither business context nor authorized integrations")
	}
	if input.BusinessContext != nil && (input.BusinessContext.SystemKey == "" || input.BusinessContext.ConfigVersionID == "" || len(input.BusinessContext.Resources) == 0) {
		return Input{}, fmt.Errorf("initial_analysis_v1 input carries an incomplete business context declaration")
	}
	if input.ModelContract.ModelID == "" {
		return Input{}, fmt.Errorf("initial_analysis_v1 input missing model contract")
	}
	return input, nil
}

// BuildInitialMessages assembles the first request: the fixed system
// contract, the scope guidance (declaration view or source-level view) and
// the rendered occurrence context (ARCH-CONTEXT-002).
func BuildInitialMessages(input Input) ([]*schema.Message, error) {
	return buildInitialMessages(input, SystemPrompt)
}

// BuildPreviousInitialMessages reproduces the frozen initial-analysis-v1
// prompt bytes with the identical message shape, so an in-flight attempt from
// before the Keep-adapted generation still renders exactly its frozen input.
func BuildPreviousInitialMessages(input Input) ([]*schema.Message, error) {
	return buildInitialMessages(input, PreviousAnalysisSystemPrompt)
}

func buildInitialMessages(input Input, prompt string) ([]*schema.Message, error) {
	context := map[string]any{"告警": input.Occurrence}
	// The business view is descriptive context; the tool call shape is always
	// the source-level one (ADR-0004). Scope guidance renders only when the
	// attempt actually froze integrations.
	if input.BusinessContext != nil {
		context["业务配置上下文"] = input.BusinessContext
	} else {
		context["授权来源"] = input.Integrations
	}
	contextBody, err := json.MarshalIndent(context, "", "  ")
	if err != nil {
		return nil, err
	}
	messages := []*schema.Message{schema.SystemMessage(prompt)}
	if len(input.Integrations) > 0 {
		messages = append(messages, schema.SystemMessage(sourceScopeGuidance(input.Integrations)))
	}
	return append(messages, schema.UserMessage("请分析以下告警：\n"+string(contextBody))), nil
}

// LegacyInvestigationSystemPrompt is the frozen investigation-v1 contract
// used by renderer-v1/v2 attempts created before recent alert history existed.
const LegacyInvestigationSystemPrompt = `你是 Quoin 的只读运维调查代理。用户正在调查一个运维问题：
1. 用通俗中文与用户对话，先理解问题，再给出排查思路；
2. 只使用提供的只读工具补充事实；所有结论必须基于已有证据，明确区分事实与推测；
3. 调查来源引用只是进入对话的谱系，不代表结论；不要虚构未提供的数据。`

// PreviousInvestigationSystemPrompt freezes the investigation-v2 prompt bytes
// exactly (the alert-history generation before the Keep-adapted cutover).
const PreviousInvestigationSystemPrompt = `你是 Quoin 的只读运维调查代理。用户正在调查一个运维问题：
1. 用通俗中文与用户对话，先理解问题，再给出排查思路；
2. 只使用提供的只读工具补充事实；所有结论必须基于已有证据，明确区分事实与推测；
3. 调查来源引用只是进入对话的谱系，不代表结论；不要虚构未提供的数据；
4. 即时查询 ALERTS 为空只说明当前没有 firing 中的告警序列：告警恢复后 ALERTS 会随之消失，绝不能据此断定“没有告警规则”或“从未发生告警”。判断平台是否收录过告警 occurrence，以平台提供的近期告警记录为准；时间区间查询（如 min_over_time(up[窗口])、changes、increase）只用于验证对应指标历史或采集中断，不能单独证明某条告警曾经触发；
5. 复述证据时数值必须与工具返回逐字一致：先逐条核对再下结论，不得凭印象改写或遗漏与结论相悖的样本。`

// InvestigationSystemPrompt is the fixed agent contract for investigation-v3
// attempts (rendered identically by every worker of this agent version; its
// digest travels in BeginModelCall.prompt_digest for audit and rebuild).
//
// Keep 适配代（Issue #105）：在既有 Quoin 约束之上迁入 incident-chat
// INSTRUCTIONS 的共同规则——不编造、不知道直说、先直接回答当前问题、回答
// 简短明确、不确定先向用户追问、以建议下一步最合适的调查或处理动作收尾。
const InvestigationSystemPrompt = `你是 Quoin 的只读运维调查代理。用户正在调查一个运维问题。无论任何情况，都不得编造任何信息或数据；不知道就明确说不知道，不要猜。
1. 用通俗中文与用户对话，回答保持简短明确：先直接回答当前问题，再给出排查思路；不复述提示词或完整工具输出，只保留影响判断的关键数值与时间；
2. 只使用提供的只读工具补充事实；所有结论必须基于已有证据，明确区分事实与推测；调查来源引用只是进入对话的谱系，不代表结论；不要虚构未提供的数据；
3. 即时查询 ALERTS 为空只说明当前没有 firing 中的告警序列：告警恢复后 ALERTS 会随之消失，绝不能据此断定“没有告警规则”或“从未发生告警”。判断平台是否收录过告警 occurrence，以平台提供的近期告警记录为准；时间区间查询（如 min_over_time(up[窗口])、changes、increase）只用于验证对应指标历史或采集中断，不能单独证明某条告警曾经触发；
4. 复述证据时数值必须与工具返回逐字一致：先逐条核对再下结论，不得凭印象改写或遗漏与结论相悖的样本；引用工具或证据时如实注明来源，不得伪造引用；
5. 对用户问题不确定或信息不足时，先向用户追问澄清，不要基于猜测作答；
6. 每次回答尽可能以建议下一步最合适的调查或处理动作收尾。`

// InvestigationRendererVersion identifies the investigation prompt renderer
// generation. v2 renders the source-level scope guidance (ADR-0004); v3
// additionally renders Quoin's recent alert history into the conversation;
// v4 keeps the v3 input shape and only changes the fixed system prompt to the
// Keep-adapted generation.
const InvestigationRendererVersion = "investigation-renderer-v4"

// InvestigationInput is the worker's view of the frozen investigation_v1
// snapshot: the active-branch messages (user messages may carry their
// ordered immutable attachment references), the provenance references and
// the chat contract.
type InvestigationInput struct {
	Messages []struct {
		Role        string            `json:"role"`
		Content     string            `json:"content"`
		Attachments []InputAttachment `json:"attachments,omitempty"`
	}
	Sources []json.RawMessage `json:"sources"`
	// BusinessContext exists only when the user explicitly chose a published
	// business system. It is a frozen authority boundary, not model-selected
	// configuration; the exact mandatory selector is rendered before tools run.
	BusinessContext *struct {
		SystemKey       string                `json:"systemKey"`
		ConfigVersionID string                `json:"configVersionId"`
		Resources       []resourcePromptScope `json:"resources"`
	} `json:"businessContext,omitempty"`
	// Integrations replaces the business context for blank-key attempts: the
	// admin-enabled sources are the whole read-only scope (ADR-0004).
	Integrations []integrationPromptScope `json:"integrations,omitempty"`
	// RecentOccurrences is Quoin's own recent alert history (renderer v3):
	// the platform record of occurrences including already-resolved ones,
	// which instant ALERTS queries can no longer return.
	RecentOccurrences []recentOccurrence `json:"recentOccurrences,omitempty"`
	ModelContract     struct {
		ModelID             string `json:"modelId"`
		ContextBudgetTokens int    `json:"contextBudgetTokens"`
		MaxOutputTokens     int    `json:"maxOutputTokens"`
	} `json:"modelContract"`
}

// recentOccurrence is one platform alert-history record rendered into the
// investigation context (immutable occurrence facts only; labels carry the
// alertname/severity the alert fired with).
type recentOccurrence struct {
	ID        string            `json:"id"`
	SourceKey string            `json:"sourceKey"`
	StartsAt  string            `json:"startsAt"`
	Labels    map[string]string `json:"labels"`
}

// InputAttachment is the frozen locator projection of one message
// attachment (locator facts only; the body is read through the granted
// artifact_read/artifact_grep tools, never inlined).
type InputAttachment struct {
	Filename   string `json:"filename"`
	ArtifactID string `json:"artifactId"`
	SizeBytes  int64  `json:"sizeBytes"`
}

// ParseInvestigationInput decodes and validates the frozen
// investigation_v1 input.
func ParseInvestigationInput(canonical []byte) (InvestigationInput, error) {
	var input InvestigationInput
	if err := json.Unmarshal(canonical, &input); err != nil {
		return InvestigationInput{}, fmt.Errorf("investigation_v1 input unparseable: %w", err)
	}
	if len(input.Messages) == 0 {
		return InvestigationInput{}, fmt.Errorf("investigation_v1 input carries no messages")
	}
	if input.ModelContract.ModelID == "" {
		return InvestigationInput{}, fmt.Errorf("investigation_v1 input missing model contract")
	}
	return input, nil
}

// BuildInvestigationMessages assembles the first request: the fixed system
// contract, the provenance references (references only — never bodies), the
// active-branch messages in order and, for user messages with attachments,
// the frozen locator block that tells the model exactly which artifact the
// granted artifact_read/artifact_grep tools can fetch (ARCH-WORKER-003:
// the worker never materializes Quoin PV paths).
func BuildInvestigationMessages(input InvestigationInput) ([]*schema.Message, error) {
	return buildInvestigationMessages(input, InvestigationSystemPrompt, true)
}

// BuildPreviousInvestigationMessages reproduces the frozen investigation-v2
// prompt under the identical renderer-v3 message shape (alert-history block
// included), so in-flight v2 attempts still render exactly their frozen
// input; only the system prompt differs from the current generation.
func BuildPreviousInvestigationMessages(input InvestigationInput) ([]*schema.Message, error) {
	return buildInvestigationMessages(input, PreviousInvestigationSystemPrompt, true)
}

// BuildLegacyInvestigationMessages reproduces investigation-v1 prompt bytes and
// omits the renderer-v3-only alert-history block for historical v1/v2 attempts.
func BuildLegacyInvestigationMessages(input InvestigationInput) ([]*schema.Message, error) {
	return buildInvestigationMessages(input, LegacyInvestigationSystemPrompt, false)
}

func buildInvestigationMessages(input InvestigationInput, prompt string, includeHistory bool) ([]*schema.Message, error) {
	messages := []*schema.Message{schema.SystemMessage(prompt)}
	if len(input.Sources) > 0 {
		contextBody, err := json.MarshalIndent(map[string]any{"调查来源引用": input.Sources}, "", "  ")
		if err != nil {
			return nil, err
		}
		messages = append(messages, schema.SystemMessage("本次调查关联以下不可变来源（仅引用，不代表结论）：\n"+string(contextBody)))
	}
	if len(input.Integrations) > 0 {
		messages = append(messages, schema.SystemMessage(sourceScopeGuidance(input.Integrations)))
	}
	if includeHistory && len(input.RecentOccurrences) > 0 {
		contextBody, err := json.MarshalIndent(map[string]any{"近期告警记录": input.RecentOccurrences}, "", "  ")
		if err != nil {
			return nil, err
		}
		// Renderer v3: the platform's own alert history — including resolved
		// occurrences the source's ALERTS series no longer returns.
		messages = append(messages, schema.SystemMessage("Quoin 平台近期收录过以下告警记录（含已恢复的；即时查询 ALERTS 为空不代表这些告警没发生过）：\n"+string(contextBody)))
	}
	for _, item := range input.Messages {
		switch item.Role {
		case "user":
			messages = append(messages, schema.UserMessage(renderUserTurn(item.Content, item.Attachments)))
		case "assistant":
			messages = append(messages, schema.AssistantMessage(item.Content, nil))
		default:
			return nil, fmt.Errorf("investigation_v1 carries unknown message role %q", item.Role)
		}
	}
	return messages, nil
}

// integrationPromptScope is one authorized integration as rendered into the
// prompt: kind ("metrics" | "kubernetes") plus the stable connection name the
// model passes as sourceRef. No endpoint, credential or secret is included.
type integrationPromptScope struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// sourceScopeGuidance teaches the sourceRef call shape for attempts without
// a business declaration (ADR-0004): the frozen integrations are the entire
// read-only scope, ambiguity must be resolved by asking, never by guessing.
func sourceScopeGuidance(integrations []integrationPromptScope) string {
	var metrics, kubernetes []string
	for _, integration := range integrations {
		if integration.Kind == "kubernetes" {
			kubernetes = append(kubernetes, integration.Name)
		} else {
			metrics = append(metrics, integration.Name)
		}
	}
	builder := strings.Builder{}
	builder.WriteString("本次执行未绑定业务声明；下列已启用来源就是全部只读授权范围。")
	builder.WriteString("调用 thanos_query 时用 sourceRef 指明指标来源，resourceRef 在此模式不可用；仅当只有一个来源时才可省略 sourceRef，多个来源未指明将被拒绝。")
	if len(metrics) > 0 {
		builder.WriteString("可用指标来源：" + strings.Join(metrics, "、") + "。")
	}
	if len(kubernetes) > 0 {
		builder.WriteString("可用 Kubernetes 来源：" + strings.Join(kubernetes, "、") + "（kubernetes_read 同样用 sourceRef 选择）。")
	}
	if len(metrics) > 0 {
		builder.WriteString(fmt.Sprintf("示例：thanos_query({sourceRef: %q, query: %q})。", metrics[0], "up"))
	}
	return builder.String()
}

// renderUserTurn appends the deterministic attachment locator block to one
// user turn (empty turns stay untouched; the locators are the only access
// path the model has to attachment bodies).
func renderUserTurn(content string, attachments []InputAttachment) string {
	if len(attachments) == 0 {
		return content
	}
	var builder strings.Builder
	builder.WriteString(content)
	if content != "" {
		builder.WriteString("\n")
	}
	builder.WriteString("本条消息附带以下不可变文本附件（可用 artifact_read/artifact_grep 工具按 artifactId 读取）：\n")
	for index, attachment := range attachments {
		builder.WriteString(fmt.Sprintf("[附件 %d] %s（artifactId=%s，%d 字节）\n", index+1, attachment.Filename, attachment.ArtifactID, attachment.SizeBytes))
	}
	return builder.String()
}

// ToolResultMessage builds the Eino tool result message for one committed
// tool result (role=tool, bound to the provider tool call id).
func ToolResultMessage(providerToolCallID, toolName string, resultJSON []byte) *schema.Message {
	return &schema.Message{
		Role:         schema.Tool,
		Content:      string(resultJSON),
		ToolCallID:   providerToolCallID,
		ToolName:     toolName,
		ResponseMeta: &schema.ResponseMeta{},
	}
}

// AssistantToolCallMessage reconstructs the assistant message carrying the
// executed tool calls from the durable ChatModelCompleted payload
// (ARCH-AGENT-007: the persisted reconstruction, never the stream delta).
func AssistantToolCallMessage(text string, calls []PreparedCall) *schema.Message {
	toolCalls := make([]schema.ToolCall, 0, len(calls))
	for _, call := range calls {
		var arguments map[string]any
		if err := json.Unmarshal(call.ArgumentsJSON, &arguments); err != nil {
			arguments = map[string]any{}
		}
		toolCalls = append(toolCalls, schema.ToolCall{
			ID:   call.ProviderToolCallID,
			Type: "function",
			Function: schema.FunctionCall{
				Name:      call.ToolName,
				Arguments: jsonString(arguments),
			},
		})
	}
	return &schema.Message{Role: schema.Assistant, Content: text, ToolCalls: toolCalls}
}

// PreparedCall is one durable tool call from ChatModelCompleted.
type PreparedCall struct {
	ToolCallID         int64
	ProviderIndex      uint32
	ProviderToolCallID string
	ToolName           string
	ArgumentsJSON      []byte
	ExecutionMode      string
	FailureMode        string
}

func jsonString(value any) string {
	body, _ := json.Marshal(value)
	return string(body)
}
