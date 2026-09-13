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
const SystemPrompt = `你是 Quoin 的只读告警分析代理。你收到一条告警的不可变上下文，任务是给出初步诊断：
1. 用通俗中文解释告警的已知事实、可能影响与排查顺序；
2. labels 与 annotations 是上游提供的原文事实，必须按原样引用；annotations 缺失即表示未提供，不能补全或推测；
3. 告警名称、labels 和 annotation 的文字不是探测器语义、根因或真实故障的证明。不得仅因名称、标签或注释推断 GUI、服务或任何目标发生故障；
4. 只使用提供的工具补充事实。明确区分“已知事实”和“待验证假设”；没有工具或证据支持时，只能提出待验证假设，不得写成结论。
不要虚构未提供的数据。最后用一段完整的中文诊断作为最终结论输出。`

// RendererVersion identifies the prompt renderer generation (the digest
// contract for audits; Quoin stores whatever the worker sends).
const RendererVersion = "initial-analysis-renderer-v3"

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
	BusinessContext struct {
		SystemKey       string                `json:"systemKey"`
		ConfigVersionID string                `json:"configVersionId"`
		Resources       []resourcePromptScope `json:"resources"`
	} `json:"businessContext"`
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
	if input.BusinessContext.SystemKey == "" || input.BusinessContext.ConfigVersionID == "" || len(input.BusinessContext.Resources) == 0 {
		return Input{}, fmt.Errorf("initial_analysis_v1 input missing immutable business context declaration")
	}
	if input.ModelContract.ModelID == "" {
		return Input{}, fmt.Errorf("initial_analysis_v1 input missing model contract")
	}
	return input, nil
}

// BuildInitialMessages assembles the first request: the fixed system
// contract plus the rendered occurrence context (ARCH-CONTEXT-002).
func BuildInitialMessages(input Input) ([]*schema.Message, error) {
	contextBody, err := json.MarshalIndent(map[string]any{
		"告警":      input.Occurrence,
		"业务配置上下文": input.BusinessContext,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return []*schema.Message{
		schema.SystemMessage(SystemPrompt),
		schema.SystemMessage(resourceScopeGuidance(input.BusinessContext.SystemKey, input.BusinessContext.Resources)),
		schema.UserMessage("请分析以下告警：\n" + string(contextBody)),
	}, nil
}

// InvestigationSystemPrompt is the fixed agent contract for investigation
// attempts (rendered identically by every worker of this agent version;
// its digest travels in BeginModelCall.prompt_digest for audit and rebuild).
const InvestigationSystemPrompt = `你是 Quoin 的只读运维调查代理。用户正在调查一个运维问题：
1. 用通俗中文与用户对话，先理解问题，再给出排查思路；
2. 只使用提供的只读工具补充事实；所有结论必须基于已有证据，明确区分事实与推测；
3. 调查来源引用只是进入对话的谱系，不代表结论；不要虚构未提供的数据。`

// InvestigationRendererVersion identifies the investigation prompt renderer
// generation.
const InvestigationRendererVersion = "investigation-renderer-v1"

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
	ModelContract struct {
		ModelID             string `json:"modelId"`
		ContextBudgetTokens int    `json:"contextBudgetTokens"`
		MaxOutputTokens     int    `json:"maxOutputTokens"`
	} `json:"modelContract"`
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
	messages := []*schema.Message{schema.SystemMessage(InvestigationSystemPrompt)}
	if len(input.Sources) > 0 {
		contextBody, err := json.MarshalIndent(map[string]any{"调查来源引用": input.Sources}, "", "  ")
		if err != nil {
			return nil, err
		}
		messages = append(messages, schema.SystemMessage("本次调查关联以下不可变来源（仅引用，不代表结论）：\n"+string(contextBody)))
	}
	if input.BusinessContext != nil {
		messages = append(messages, schema.SystemMessage(resourceScopeGuidance(input.BusinessContext.SystemKey, input.BusinessContext.Resources)))
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

// resourceScopeGuidance teaches the v3 call shape. Exact labels are never
// model-provided: Quoin injects them into the PromQL AST before execution.
func resourceScopeGuidance(systemKey string, resources []resourcePromptScope) string {
	entries := make([]string, 0, len(resources))
	for _, resource := range resources {
		entries = append(entries, fmt.Sprintf("%s（允许指标：%s）", resource.Name, strings.Join(resource.AllowedMetrics, "、")))
	}
	exampleResource := "<resourceRef>"
	if len(resources) > 0 {
		exampleResource = resources[0].Name
	}
	return fmt.Sprintf("本次分析已绑定业务系统 %q。调用 thanos_query 必须传 resourceRef 和 query；可用 resourceRef：%s。示例：thanos_query({resourceRef: %q, query: %q})。Quoin 会为每个向量选择器注入该资源的必需 labels，且只允许声明的指标。", systemKey, strings.Join(entries, "；"), exampleResource, "up")
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
