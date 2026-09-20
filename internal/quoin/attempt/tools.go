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

	"github.com/Suknna/quoin/internal/plugins"
)

// AgentVersion is the frozen executor generation for initial-analysis
// attempts (DATA-ATTEMPT-001). Both the worker binary and the dispatch row
// carry it; the Keep 提示词迁入 advances the analysis prompt to its own
// generation while every older identity stays executable.
const AgentVersion = "initial-analysis-v2"

// PreviousAgentVersion retains the initial-analysis-v1 identity: in-flight
// analysis attempts still commit under it, it is the legacy inspection alias
// (the shared generation inspection attempts originally rode), and in-flight
// knowledge attempts carry it.
const PreviousAgentVersion = "initial-analysis-v1"

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
// creation) and the worker mode must agree on it exactly.
const InspectionAgentVersion = "inspection-analysis-v3"

// ReportComplianceInspectionAgentVersion retains the report-compliance
// inspection prompt generation for already-created attempts.
const ReportComplianceInspectionAgentVersion = "inspection-analysis-v2"

// PreviousInspectionAgentVersion retains execution compatibility for attempts
// created with the first dedicated inspection prompt generation.
const PreviousInspectionAgentVersion = "inspection-analysis-v1"

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
