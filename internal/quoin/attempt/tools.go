package attempt

// The platform tool table plus the shared tool-contract machinery
// (ARCH-WORKER-003, ARCH-OUTPUT-004). PLATFORM tools (workspace + artifact)
// are owned here and never by a plugin. PLUGIN tools live with their owning
// plugin (internal/plugins/builtin) and reach this package only through the
// assembled registry: implementation lookup, frozen catalogs and the
// executing hosts' dispatch tables all derive from that one assembly
// (ADR-0004). The frozen historical fallback (legacy_catalog.go) is the
// only other tool source, and it is fixed data, never a registration path.

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/Suknna/quoin/internal/plugins"
)

// AgentVersion is the frozen executor generation for initial-analysis
// attempts (DATA-ATTEMPT-001). Both the worker binary and the dispatch row
// carry it; the Keep 提示词迁入 advanced the analysis prompt to its own
// generation while every older identity stays executable, and the 知识接入
// generation advances it again (knowledge retrieval tools + usage rules).
const AgentVersion = "initial-analysis-v3"

// KnowledgeAgentVersion pins knowledge extraction to its ORIGINAL shared
// executor identity: the knowledge prompt never evolved with the analysis
// prompt, so new knowledge attempts keep the original identity and output
// contract instead of silently riding a new analysis generation.
const KnowledgeAgentVersion = "initial-analysis-v1"

// InspectionAgentVersion is the inspection report analysis' own frozen
// executor generation. The inspection prompt evolves independently of the
// initial-analysis prompt, so its attempts and model calls carry a distinct
// version identity instead of silently drifting under the shared
// initial-analysis generation; the dispatch row (inspection_analysis
// creation) and the worker mode must agree on it exactly. The 知识接入
// generation is also the first to freeze a per-attempt tool catalog for
// inspection analyses.
const InspectionAgentVersion = "inspection-analysis-v4"

// ToolSchemaVersion names the fixed callable tool-schema generation of the
// initial-analysis catalog. Quoin resolves tool names only against a frozen
// catalog document — never against this package's live tables.
const ToolSchemaVersion = "initial-analysis-tools-v5"

// Aliases keep the attempt-side vocabulary on the shared plugin contract:
// ToolDef and its kinds live in internal/plugins so plugin packages own
// their compiled implementations without importing the attempt core.
type (
	ToolDef      = plugins.ToolDef
	ArgumentKind = plugins.ArgumentKind
	ToolEntry    = plugins.ToolEntry
)

const (
	KindString = plugins.KindString
	KindNumber = plugins.KindNumber
)

// platformTools are the compiled tools no plugin owns: the disposable
// workspace tools run inside the worker sandbox (worker_local) and the
// artifact tools execute inside Quoin against the Attempt-scoped artifact
// store (quoin_routed, ADR-0011 — they no longer run in the supervisor).
// Read/write/bash and their siblings stay platform-owned; plugins never
// redefine them.
var platformTools = []ToolDef{
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
		Name: "artifact_read", Version: "2", ExecutionMode: "quoin_routed", FailureMode: "return_to_model", ResultSchemaKind: "artifact_read_result_v1",
		Description: "按范围读取一个 Artifact 的文本片段；返回有界片段与 size/hash/eof/truncated。",
		Arguments:   map[string]ArgumentKind{"artifactId": KindString, "offset": KindNumber, "limit": KindNumber},
		Required:    []string{"artifactId"},
	},
	{
		Name: "artifact_grep", Version: "2", ExecutionMode: "quoin_routed", FailureMode: "return_to_model", ResultSchemaKind: "artifact_grep_result_v1",
		Description: "在 Artifact 文本内按 RE2 正则搜索；返回有界匹配片段与截断标记。",
		Arguments:   map[string]ArgumentKind{"artifactId": KindString, "pattern": KindString},
		Required:    []string{"artifactId", "pattern"},
	},
	alertsRecentTool(),
	knowledgeSearchTool(),
	knowledgeGetTool(),
}

// knowledgeSearchTool 是检索 Quoin 自有知识库的平台工具（ADR-0012 归属判据：
// 读 Quoin 自有数据 = 平台工具）。复用知识域的双通道查询服务（FTS5 trigram +
// embedding cosine，程序不融合排名、不设阈值），语义通道未配置/不可用时诚实
// 降级为仅精确文本通道。参数带数值边界，超出 attempt 简单 Arguments/Required
// 词表的表达力，因此与插件工具同款手写 Parameters + ValidateArguments 闭包
// （拒绝未知字段与越界值）。
func knowledgeSearchTool() ToolDef {
	parameters := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "自然语言或关键词检索词；同时用于精确文本匹配与语义相似两个通道。",
			},
			"limit": map[string]any{
				"type":        "number",
				"minimum":     1,
				"maximum":     10,
				"description": "每个通道返回条数上限；缺省 5，上限 10。",
			},
		},
		"required": []any{"query"},
	}
	return ToolDef{
		Name: "knowledge_search", Version: "1", ExecutionMode: "quoin_routed", FailureMode: "return_to_model",
		ResultSchemaKind: "knowledge_search_result_v1",
		Description:      "检索 Quoin 知识库中经人工确认、当前可复用的运维知识：同一检索词分别返回“精确文本匹配”与“语义相似”两个通道的命中（knowledgeId/versionId/标题/分数），不合成统一排名。知识是历史经验的沉淀参考，不代表当前系统实时状态；命中后用 knowledge_get 按 versionId 读取正文并引用该版本。",
		Arguments:        map[string]ArgumentKind{"query": KindString, "limit": KindNumber},
		Parameters:       parameters,
		ValidateArguments: func(raw []byte) error {
			var arguments map[string]any
			if err := json.Unmarshal(raw, &arguments); err != nil {
				return fmt.Errorf("tool knowledge_search arguments unparseable: %w", err)
			}
			for key := range arguments {
				switch key {
				case "query", "limit":
				default:
					return fmt.Errorf("tool knowledge_search argument %q is not part of the fixed schema", key)
				}
			}
			query, exists := arguments["query"]
			if !exists {
				return fmt.Errorf("tool knowledge_search requires argument %q", "query")
			}
			if text, ok := query.(string); !ok || strings.TrimSpace(text) == "" {
				return fmt.Errorf("tool knowledge_search argument %q must be a non-empty string", "query")
			}
			if value, exists := arguments["limit"]; exists && value != nil {
				number, ok := value.(float64)
				if !ok || number < 1 || number > 10 {
					return fmt.Errorf("tool knowledge_search argument %q must be a number between 1 and 10", "limit")
				}
			}
			return nil
		},
	}
}

// knowledgeGetTool 按 versionId 读取一份当前合格（current ∧ 未停用 ∧ 来源有效）
// 知识版本正文。已停止复用（exited）或不再是当前版本的定位符返回稳定错误：
// 停止复用的知识不能被新检索使用（DATA-KNOWLEDGE-007）。
func knowledgeGetTool() ToolDef {
	parameters := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"versionId": map[string]any{
				"type":        "number",
				"description": "knowledge_search 命中携带的知识版本定位符（十进制正整数）。",
			},
		},
		"required": []any{"versionId"},
	}
	return ToolDef{
		Name: "knowledge_get", Version: "1", ExecutionMode: "quoin_routed", FailureMode: "return_to_model",
		ResultSchemaKind: "knowledge_get_result_v1",
		Description:      "读取一份可复用知识版本的标题、正文与适用范围/条件/限制（正文超长时有界截断并标记 truncated）。只有当前合格（未停用复用、来源有效）的版本可读；引用知识时必须以返回的 versionId 标注可追踪版本。",
		Arguments:        map[string]ArgumentKind{"versionId": KindNumber},
		Parameters:       parameters,
		ValidateArguments: func(raw []byte) error {
			var arguments map[string]any
			if err := json.Unmarshal(raw, &arguments); err != nil {
				return fmt.Errorf("tool knowledge_get arguments unparseable: %w", err)
			}
			for key := range arguments {
				switch key {
				case "versionId":
				default:
					return fmt.Errorf("tool knowledge_get argument %q is not part of the fixed schema", key)
				}
			}
			value, exists := arguments["versionId"]
			if !exists {
				return fmt.Errorf("tool knowledge_get requires argument %q", "versionId")
			}
			number, ok := value.(float64)
			if !ok || number < 1 || number != math.Trunc(number) {
				return fmt.Errorf("tool knowledge_get argument %q must be a positive integer", "versionId")
			}
			return nil
		},
	}
}

// alertsRecentTool 是读取 Quoin 自有告警库的平台工具（ADR-0012 工具归属判据：
// 读 Quoin 自有数据 = 平台工具，不属于任何插件）。参数带枚举与数值边界，
// 超出 attempt 简单 Arguments/Required 词表的表达力，因此与插件工具同款
// 手写 Parameters + ValidateArguments 闭包（拒绝未知字段与越界值）。
func alertsRecentTool() ToolDef {
	parameters := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"viewKey": map[string]any{
				"type":        "string",
				"description": "按业务视图过滤：只返回首观测时关联到该 viewKey 的告警；缺省不过滤。",
			},
			"severityMin": map[string]any{
				"type":        "string",
				"enum":        []any{"critical", "high", "warning", "info"},
				"description": "最低 severity（含）；缺省为 info（不过滤）。",
			},
			"hours": map[string]any{
				"type":        "number",
				"minimum":     1,
				"maximum":     168,
				"description": "时间窗（小时），按告警 startsAt 回看；缺省 24，上限 168。",
			},
			"limit": map[string]any{
				"type":        "number",
				"minimum":     1,
				"maximum":     50,
				"description": "返回条数上限；缺省 10，上限 50。",
			},
		},
	}
	return ToolDef{
		Name: "alerts_recent", Version: "1", ExecutionMode: "quoin_routed", FailureMode: "return_to_model",
		ResultSchemaKind: "alerts_recent_result_v1", Description: "查询 Quoin 告警库中的近期告警（归一化语义）：可按业务视图、最低 severity、时间窗过滤，返回 id/severity/title/state/startsAt/labels 摘要。用于分析时获取相关告警上下文。",
		Arguments:  map[string]ArgumentKind{"viewKey": KindString, "severityMin": KindString, "hours": KindNumber, "limit": KindNumber},
		Parameters: parameters,
		ValidateArguments: func(raw []byte) error {
			var arguments map[string]any
			if err := json.Unmarshal(raw, &arguments); err != nil {
				return fmt.Errorf("tool alerts_recent arguments unparseable: %w", err)
			}
			for key := range arguments {
				switch key {
				case "viewKey", "severityMin", "hours", "limit":
				default:
					return fmt.Errorf("tool alerts_recent argument %q is not part of the fixed schema", key)
				}
			}
			if value, exists := arguments["viewKey"]; exists {
				if text, ok := value.(string); !ok || text == "" {
					return fmt.Errorf("tool alerts_recent argument %q must be a non-empty string", "viewKey")
				}
			}
			if value, exists := arguments["severityMin"]; exists {
				severity, ok := value.(string)
				if !ok || !plugins.ValidSeverity(plugins.Severity(severity)) {
					return fmt.Errorf("tool alerts_recent argument %q must be one of critical/high/warning/info", "severityMin")
				}
			}
			bounds := map[string]struct{ min, max, fallback float64 }{
				"hours": {1, 168, 24},
				"limit": {1, 50, 10},
			}
			for key, bound := range bounds {
				value, exists := arguments[key]
				if !exists || value == nil {
					continue
				}
				number, ok := value.(float64)
				if !ok || number < bound.min || number > bound.max {
					return fmt.Errorf("tool alerts_recent argument %q must be a number between %v and %v", key, bound.min, bound.max)
				}
			}
			return nil
		},
	}
}

// PlatformImplementations returns the compiled platform tool table (stable
// order). Plugin tool implementations join through the plugin registry's
// ToolEntry set inside BuildCatalogs — there is no other tool table.

// ValidateToolArguments checks one proposed tool call's canonical argument
// object against the frozen tool contract (ARCH-TOOL-001: structure is
// validated before the pending row exists).
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

// ValidateToolResultPayload enforces the closed result shape for contracts
// whose payload has security-relevant routing semantics. Dispatch goes
// through the caller's assembled implementation table — a result validator
// exists only where a compiled tool declares one — and is called at the
// Quoin ingress before CompleteToolCall can mutate the ledger.
func ValidateToolResultPayload(table *ImplementationTable, schemaKind string, canonical []byte) error {
	for _, def := range table.Definitions() {
		if def.ResultSchemaKind == schemaKind && def.ValidateResult != nil {
			return def.ValidateResult(canonical)
		}
	}
	return nil
}

// PlatformImplementations returns the compiled platform tool table in
// stable order; BuildCatalogs joins it with the plugin registry entries.
func PlatformImplementations() []ToolDef {
	all := make([]ToolDef, 0, len(platformTools))
	all = append(all, platformTools...)
	return all
}
