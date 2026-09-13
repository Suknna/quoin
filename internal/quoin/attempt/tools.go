package attempt

// Fixed tool catalog for the initial-analysis-v1 agent (ARCH-WORKER-003,
// ARCH-OUTPUT-004). The catalog is the Quoin-side validation authority for
// every proposed tool call: name, version, execution mode, failure mode and
// argument shape. The worker renders the identical provider-facing tool
// schema; internal/plinth/worker/tools_test.go pins both canonical
// renderings byte-equal so drift fails the build, and BeginModelCall
// additionally rejects a tool_schema_digest mismatch at runtime.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// AgentVersion is the frozen executor generation for the T10 vertical. Both
// the worker binary and the dispatch row carry it (DATA-ATTEMPT-001).
const AgentVersion = "initial-analysis-v1"

// ToolSchemaVersion names the fixed callable tool-schema generation. Kubernetes
// observation is intentionally excluded while that capability remains in development.
// v5 makes resourceRef mandatory for every metrics observation. Quoin resolves
// that name only against the attempt's frozen compiled declaration.
const ToolSchemaVersion = "initial-analysis-tools-v5"

// ToolDef is one fixed tool in the catalog.
type ToolDef struct {
	Name             string
	Version          string
	ExecutionMode    string // worker_local | supervisor_typed | quoin_browser
	FailureMode      string // return_to_model | fail_attempt
	ResultSchemaKind string // exact ResultPayload.schema_kind accepted at runtime ingress
	Description      string
	// Arguments lists the accepted top-level argument keys with their
	// required kind; "required" keys must be present.
	Arguments map[string]ArgumentKind
	Required  []string
	// ProducesEvidence marks a supervisor_typed observation tool: a
	// succeeded execution commits deterministic Evidence together with
	// the Tool Call terminal state (ARCH-TOOL-005, DATA-EVIDENCE-001).
	ProducesEvidence bool
	// Parameters, when non-nil, is the complete frozen provider-facing JSON
	// Schema. It is reserved for closed union-shaped tools whose arguments
	// cannot be represented by the simple string/number map above.
	Parameters map[string]any
	// ValidateArguments is the matching Quoin-side ingress validator for
	// Parameters. It must reject unknown fields and unsupported union members.
	ValidateArguments func([]byte) error
}

// ArgumentKind is the JSON kind of one argument.
type ArgumentKind string

const (
	KindString ArgumentKind = "string"
	KindNumber ArgumentKind = "number"
)

// InitialAnalysisTools is the frozen tool set exposed to the model for
// initial-analysis attempts. Workspace tools run inside the worker sandbox;
// artifact tools execute on the supervisor through the Attempt-scoped
// ArtifactService (ARCH-WORKER-003, ARCH-OUTPUT-004).
var InitialAnalysisTools = []ToolDef{
	{
		Name: "bash", Version: "1", ExecutionMode: "worker_local", FailureMode: "return_to_model", ResultSchemaKind: "workspace_tool_result_v1",
		Description: "在当前一次性工作区执行一条 bash 命令（/bin/bash --noprofile --norc -c）。无网络、无凭据，只可访问工作区与只读系统路径。",
		Arguments:   map[string]ArgumentKind{"command": KindString},
		Required:    []string{"command"},
	},
	{
		Name: "read", Version: "1", ExecutionMode: "worker_local", FailureMode: "return_to_model", ResultSchemaKind: "workspace_tool_result_v1",
		Description: "读取工作区内一个文本文件的内容（相对路径）。",
		Arguments:   map[string]ArgumentKind{"path": KindString},
		Required:    []string{"path"},
	},
	{
		Name: "write", Version: "1", ExecutionMode: "worker_local", FailureMode: "return_to_model", ResultSchemaKind: "workspace_tool_result_v1",
		Description: "把文本内容写入工作区内一个文件（相对路径）。",
		Arguments:   map[string]ArgumentKind{"path": KindString, "content": KindString},
		Required:    []string{"path", "content"},
	},
	{
		Name: "grep", Version: "1", ExecutionMode: "worker_local", FailureMode: "return_to_model", ResultSchemaKind: "workspace_tool_result_v1",
		Description: "在工作区文件内按 RE2 正则搜索并返回匹配行。",
		Arguments:   map[string]ArgumentKind{"pattern": KindString, "path": KindString},
		Required:    []string{"pattern", "path"},
	},
	{
		Name: "artifact_read", Version: "1", ExecutionMode: "supervisor_typed", FailureMode: "return_to_model", ResultSchemaKind: "artifact_read_result_v1",
		Description: "按范围读取一个 Artifact 的文本片段；返回有界片段与 size/hash/eof/truncated。",
		Arguments:   map[string]ArgumentKind{"artifactId": KindString, "offset": KindNumber, "limit": KindNumber},
		Required:    []string{"artifactId"},
	},
	{
		Name: "artifact_grep", Version: "1", ExecutionMode: "supervisor_typed", FailureMode: "return_to_model", ResultSchemaKind: "artifact_grep_result_v1",
		Description: "在 Artifact 文本内按 RE2 正则搜索；返回有界匹配片段与截断标记。",
		Arguments:   map[string]ArgumentKind{"artifactId": KindString, "pattern": KindString},
		Required:    []string{"artifactId", "pattern"},
	},
	{
		Name: "thanos_query", Version: "2", ExecutionMode: "supervisor_typed", FailureMode: "return_to_model", ResultSchemaKind: "thanos_query_result_v1", ProducesEvidence: true,
		Description: "在当前尝试冻结的业务配置资源范围内执行只读 PromQL 即时查询。必须提供 resourceRef 和 query；Quoin 从不可变配置版本解析连接、允许指标和必需 labels，结果作为不可变 Evidence 封存。",

		Arguments: map[string]ArgumentKind{"resourceRef": KindString, "query": KindString},
		Required:  []string{"resourceRef", "query"},
	},
}

// BrowserTool is available only to investigations. It is executed by Quoin,
// never by the Plinth supervisor: requests are frozen and forwarded to a
// Lintel-owned, closed browser-action executor.
var BrowserTool = ToolDef{
	Name: "quoin_browser", Version: "1", ExecutionMode: "quoin_browser", FailureMode: "return_to_model", ResultSchemaKind: "browser_tool_result_v1",
	Description: "在已授权的浏览器身份中执行一个封闭的探索动作。只接受 open、页面导航、元素交互、受限读取、截图和会话关闭；不接受 JavaScript、HTTP、CDP 或 Playwright 指令。",
	Parameters:  browserToolParameters(), ValidateArguments: validateBrowserToolArguments,
}

// InvestigationTools retains chat-specific browser exploration while excluding
// Kubernetes from every model-callable schema during the development gate.
var InvestigationTools = append(append([]ToolDef{}, InitialAnalysisTools...), BrowserTool)

// ToolsForAgentVersion returns the only catalog valid for an agent generation.
func ToolsForAgentVersion(agentVersion string) []ToolDef {
	if agentVersion == "investigation-v1" {
		return InvestigationTools
	}
	return InitialAnalysisTools
}

// LookupToolForAgentVersion resolves a tool only within its mode-specific
// catalog; a tool accepted by Investigation cannot leak into Initial Analysis.
func LookupToolForAgentVersion(agentVersion, name string) (ToolDef, bool) {
	for _, tool := range ToolsForAgentVersion(agentVersion) {
		if tool.Name == name {
			return tool, true
		}
	}
	return ToolDef{}, false
}

// LookupTool retains the Initial Analysis compatibility seam for existing
// callers; new authorization code must select by agent version.
func LookupTool(name string) (ToolDef, bool) { return LookupToolForAgentVersion(AgentVersion, name) }

// ValidateToolArguments checks one proposed tool call's canonical argument
// object against the fixed catalog (ARCH-TOOL-001: structure is validated
// before the pending row exists).
func ValidateToolArguments(tool ToolDef, argumentsJSON []byte) error {
	if tool.ValidateArguments != nil {
		return tool.ValidateArguments(argumentsJSON)
	}
	if !jsonValid(argumentsJSON, "object") {
		return fmt.Errorf("tool %s arguments must be a JSON object", tool.Name)
	}
	var arguments map[string]any
	if err := json.Unmarshal(argumentsJSON, &arguments); err != nil {
		return fmt.Errorf("tool %s arguments unparseable: %w", tool.Name, err)
	}
	for _, key := range tool.Required {
		value, exists := arguments[key]
		if !exists {
			return fmt.Errorf("tool %s requires argument %q", tool.Name, key)
		}
		kind := tool.Arguments[key]
		valid := false
		switch kind {
		case KindString:
			text, ok := value.(string)
			valid = ok && text != ""
		case KindNumber:
			switch value.(type) {
			case float64, int, int64:
				valid = true
			}
		}
		if !valid {
			return fmt.Errorf("tool %s argument %q must be a non-empty %s", tool.Name, key, kind)
		}
	}
	for key := range arguments {
		if _, known := tool.Arguments[key]; !known {
			return fmt.Errorf("tool %s argument %q is not part of the fixed schema", tool.Name, key)
		}
	}
	return nil
}

// CanonicalToolsJSON renders the provider-facing tool schema in the frozen
// canonical order (OpenAI function tools shape). The worker must render the
// identical bytes; a mismatch is rejected at BeginModelCall.
func CanonicalToolsJSON(agentVersions ...string) ([]byte, error) {
	agentVersion := AgentVersion
	if len(agentVersions) == 1 {
		agentVersion = agentVersions[0]
	}
	catalog := ToolsForAgentVersion(agentVersion)
	tools := make([]any, 0, len(catalog))
	for _, tool := range catalog {
		parameters := tool.Parameters
		if parameters == nil {
			properties := map[string]any{}
			required := make([]string, 0, len(tool.Required))
			for key, kind := range tool.Arguments {
				properties[key] = map[string]any{"type": string(kind)}
			}
			for _, key := range tool.Required {
				required = append(required, key)
			}
			sort.Strings(required)
			parameters = map[string]any{"type": "object", "properties": properties, "required": required}
		}
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  parameters,
			},
		})
	}
	return json.Marshal(tools)
}

// CanonicalToolsDigest is the SHA-256 of CanonicalToolsJSON as hex text.
func CanonicalToolsDigest(agentVersions ...string) (string, error) {
	body, err := CanonicalToolsJSON(agentVersions...)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// toolSchemaVersionFor keeps model-call provenance tied to the catalog the
// attempt actually rendered. Investigation is a distinct fixed surface.
func toolSchemaVersionFor(agentVersion string) string {
	if agentVersion == "investigation-v1" {
		return "investigation-tools-v1"
	}
	return ToolSchemaVersion
}

// ValidateToolResultPayload enforces the closed result shape for contracts
// whose payload has security-relevant routing semantics. It is called at the
// Quoin ingress before CompleteToolCall can mutate the ledger.
func ValidateToolResultPayload(schemaKind string, canonical []byte) error {
	if schemaKind == "browser_tool_result_v1" {
		return validateBrowserToolResult(canonical)
	}
	return nil
}
