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

	"github.com/cloudwego/eino/schema"
)

// SystemPrompt is the fixed agent contract for initial-analysis attempts
// (rendered identically by every worker of this agent version; its digest
// travels in BeginModelCall.prompt_digest for audit and rebuild).
const SystemPrompt = `你是 Quoin 的只读告警分析代理。你收到一条告警的不可变上下文，任务是给出初步诊断：
1. 用通俗中文解释告警含义与可能影响；
2. 结合 labels 与 annotations 提出最可能的故障方向与排查顺序；
3. 只使用提供的工具补充事实；所有结论必须基于已有证据，明确区分事实与推测。
不要虚构未提供的数据。最后用一段完整的中文诊断作为最终结论输出。`

// RendererVersion identifies the prompt renderer generation (the digest
// contract for audits; Quoin stores whatever the worker sends).
const RendererVersion = "initial-analysis-renderer-v1"

// SystemPromptDigest is the SHA-256 hex digest of the fixed system prompt.
func SystemPromptDigest() string {
	sum := sha256.Sum256([]byte(SystemPrompt))
	return hex.EncodeToString(sum[:])
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
	if input.ModelContract.ModelID == "" {
		return Input{}, fmt.Errorf("initial_analysis_v1 input missing model contract")
	}
	return input, nil
}

// BuildInitialMessages assembles the first request: the fixed system
// contract plus the rendered occurrence context (ARCH-CONTEXT-002).
func BuildInitialMessages(input Input) ([]*schema.Message, error) {
	contextBody, err := json.MarshalIndent(map[string]any{
		"告警": input.Occurrence,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return []*schema.Message{
		schema.SystemMessage(SystemPrompt),
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
// snapshot: the active-branch messages, the provenance references and the
// chat contract.
type InvestigationInput struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Sources       []json.RawMessage `json:"sources"`
	ModelContract struct {
		ModelID             string `json:"modelId"`
		ContextBudgetTokens int    `json:"contextBudgetTokens"`
		MaxOutputTokens     int    `json:"maxOutputTokens"`
	} `json:"modelContract"`
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
// contract, the provenance references (references only — never bodies) and
// the active-branch messages in order.
func BuildInvestigationMessages(input InvestigationInput) ([]*schema.Message, error) {
	messages := []*schema.Message{schema.SystemMessage(InvestigationSystemPrompt)}
	if len(input.Sources) > 0 {
		contextBody, err := json.MarshalIndent(map[string]any{"调查来源引用": input.Sources}, "", "  ")
		if err != nil {
			return nil, err
		}
		messages = append(messages, schema.SystemMessage("本次调查关联以下不可变来源（仅引用，不代表结论）：\n"+string(contextBody)))
	}
	for _, item := range input.Messages {
		switch item.Role {
		case "user":
			messages = append(messages, schema.UserMessage(item.Content))
		case "assistant":
			messages = append(messages, schema.AssistantMessage(item.Content, nil))
		default:
			return nil, fmt.Errorf("investigation_v1 carries unknown message role %q", item.Role)
		}
	}
	return messages, nil
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
