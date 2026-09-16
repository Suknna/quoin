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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/plugins/builtin"
)

// AgentVersion is the frozen executor generation for the T10 vertical. Both
// the worker binary and the dispatch row carry it (DATA-ATTEMPT-001).
const AgentVersion = "initial-analysis-v1"

// InspectionAgentVersion is the inspection report analysis' own frozen
// executor generation. The inspection prompt evolves independently of the
// initial-analysis prompt, so its attempts and model calls carry a distinct
// version identity instead of silently drifting under the shared
// initial-analysis generation; the dispatch row (inspection_analysis
// creation) and the worker mode must agree on it exactly.
const InspectionAgentVersion = "inspection-analysis-v1"

// ToolSchemaVersion names the fixed callable tool-schema generation of the
// initial-analysis catalog. Quoin resolves tool names only against a frozen
// catalog document or the fixed legacy fallback — never against this
// package's live tables.
const ToolSchemaVersion = "initial-analysis-tools-v5"

// Aliases keep the attempt-side vocabulary on the shared plugin contract:
// ToolDef and its kinds live in internal/plugins so plugin packages own
// their compiled implementations without importing the attempt core.
type (
	ToolDef      = plugins.ToolDef
	ArgumentKind = plugins.ArgumentKind
)

const (
	KindString = plugins.KindString
	KindNumber = plugins.KindNumber
)

// platformTools are the compiled tools no plugin owns: the disposable
// workspace tools run inside the worker sandbox and the artifact tools
// executing on the supervisor through the Attempt-scoped ArtifactService
// (ARCH-WORKER-003, ARCH-OUTPUT-004). Read/write/bash and their siblings
// stay platform-owned; plugins never redefine them.
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
}

// Implementations assembles this binary's complete compiled implementation
// table: platform tools first (stable order), then every plugin-owned
// implementation (active and retired) from the shared builtin declarations.
// It is the single input both catalog assembly and dispatch assembly
// consume; there is no other tool table.
func Implementations() []ToolDef {
	all := make([]ToolDef, 0, len(platformTools)+len(builtin.PluginTools())+len(builtin.RetiredTools()))
	all = append(all, platformTools...)
	all = append(all, builtin.PluginTools()...)
	all = append(all, builtin.RetiredTools()...)
	return all
}

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

// CanonicalToolsJSON renders the FROZEN legacy generation catalog into the
// provider-facing tool schema bytes (OpenAI function tools shape). It serves
// only attempts created before per-attempt freezing: current attempts render
// their stored catalog document, so these bytes can never drift with the
// installed implementation.
func CanonicalToolsJSON(agentVersions ...string) ([]byte, error) {
	catalog := legacyGenerationCatalog(legacyAgentVersion(agentVersions))
	return catalog.ProviderToolsJSON()
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

func legacyAgentVersion(agentVersions []string) string {
	if len(agentVersions) == 1 {
		return agentVersions[0]
	}
	return AgentVersion
}
