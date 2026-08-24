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

// ToolSchemaVersion names the fixed tool-schema generation. T11 added
// thanos_query (typed read-only Thanos evidence), which changes the
// canonical schema bytes: the generation advances so both sides and the
// persisted model_calls.tool_schema_version stay consistent.
const ToolSchemaVersion = "initial-analysis-tools-v3"

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
		Name: "thanos_query", Version: "1", ExecutionMode: "supervisor_typed", FailureMode: "return_to_model", ResultSchemaKind: "thanos_query_result_v1", ProducesEvidence: true,
		Description: "对全局 Thanos 执行一次只读 PromQL 即时查询（instant query）；模型只提供 query 表达式，连接与凭据由 Quoin 确定性解析，结果作为不可变 Evidence 封存。",
		Arguments:   map[string]ArgumentKind{"query": KindString},
		Required:    []string{"query"},
	},
}

// KubernetesReadTool is exposed only to the investigation agent. Its domain
// target is deterministically resolved by Quoin; it never accepts a connection
// identifier or credential locator from the model.
var KubernetesReadTool = ToolDef{
	Name: "kubernetes_read", Version: "1", ExecutionMode: "supervisor_typed", FailureMode: "return_to_model", ResultSchemaKind: "kubernetes_read_result_v1", ProducesEvidence: true,
	Description: "对指定业务系统已绑定的 Kubernetes 连接执行固定只读操作。businessSystem 只接受业务系统 key 或名称；operation 只能为 discovery、pod_get、pod_list、events_list、pod_logs，绝不接受连接或凭据。",
	Arguments:   map[string]ArgumentKind{"businessSystem": KindString, "operation": KindString, "namespace": KindString, "name": KindString, "container": KindString},
	Required:    []string{"businessSystem", "operation"},
}

// InvestigationTools is intentionally a distinct catalog: ARCH-CHAT-006
// permits Kubernetes observation only during an Investigation.
var InvestigationTools = append(append([]ToolDef{}, InitialAnalysisTools...), KubernetesReadTool)

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
	if tool.Name == "kubernetes_read" {
		operation, _ := arguments["operation"].(string)
		require := func(key string) error {
			if value, _ := arguments[key].(string); value == "" {
				return fmt.Errorf("tool kubernetes_read operation %s requires %s", operation, key)
			}
			return nil
		}
		forbid := func(keys ...string) error {
			for _, key := range keys {
				if _, present := arguments[key]; present {
					return fmt.Errorf("tool kubernetes_read argument %q is not allowed for operation %s", key, operation)
				}
			}
			return nil
		}
		switch operation {
		case "discovery":
			if err := forbid("namespace", "name", "container"); err != nil {
				return err
			}
		case "pod_list", "events_list":
			if err := require("namespace"); err != nil {
				return err
			}
			if err := forbid("name", "container"); err != nil {
				return err
			}
		case "pod_get":
			if err := require("namespace"); err != nil {
				return err
			}
			if err := require("name"); err != nil {
				return err
			}
			if err := forbid("container"); err != nil {
				return err
			}
		case "pod_logs":
			if err := require("namespace"); err != nil {
				return err
			}
			if err := require("name"); err != nil {
				return err
			}
			if err := require("container"); err != nil {
				return err
			}
		default:
			return fmt.Errorf("tool kubernetes_read operation %q is not allowed", operation)
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
		properties := map[string]any{}
		required := make([]string, 0, len(tool.Required))
		for key, kind := range tool.Arguments {
			properties[key] = map[string]any{"type": string(kind)}
		}
		for _, key := range tool.Required {
			required = append(required, key)
		}
		sort.Strings(required)
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters": map[string]any{
					"type":       "object",
					"properties": properties,
					"required":   required,
				},
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
	if schemaKind != "kubernetes_read_result_v1" {
		return nil
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &body); err != nil {
		return fmt.Errorf("kubernetes result must be JSON: %w", err)
	}
	allowed := map[string]bool{"success": true, "operation": true, "results": true, "errorCode": true, "errorDetail": true, "artifact": true}
	for key := range body {
		if !allowed[key] {
			return fmt.Errorf("kubernetes result field %q is not allowed", key)
		}
	}
	var success bool
	if raw, ok := body["success"]; !ok || json.Unmarshal(raw, &success) != nil {
		return fmt.Errorf("kubernetes result requires boolean success")
	}
	// Routing preflight failures have no operation or result list.
	if _, hasOperation := body["operation"]; !hasOperation {
		return validateKubernetesFailure(body)
	}
	var operation string
	if err := json.Unmarshal(body["operation"], &operation); err != nil || operation == "" {
		return fmt.Errorf("kubernetes result requires operation")
	}
	var results []json.RawMessage
	if raw, ok := body["results"]; !ok || json.Unmarshal(raw, &results) != nil || len(results) == 0 {
		return fmt.Errorf("kubernetes result requires non-empty results")
	}
	for _, raw := range results {
		if err := validateKubernetesMappingResult(raw); err != nil {
			return err
		}
	}
	if success {
		if _, exists := body["errorCode"]; exists {
			return fmt.Errorf("successful kubernetes result cannot carry errorCode")
		}
	} else if err := validateKubernetesFailure(body); err != nil {
		return err
	}
	if raw, exists := body["artifact"]; exists {
		var artifact map[string]json.RawMessage
		if json.Unmarshal(raw, &artifact) != nil || len(artifact) != 2 || artifact["id"] == nil || artifact["mediaType"] == nil {
			return fmt.Errorf("kubernetes result artifact must contain only id and mediaType")
		}
		var id, mediaType string
		if json.Unmarshal(artifact["id"], &id) != nil || id == "" || json.Unmarshal(artifact["mediaType"], &mediaType) != nil || mediaType == "" {
			return fmt.Errorf("kubernetes result artifact fields must be non-empty strings")
		}
	}
	return nil
}

func validateKubernetesFailure(body map[string]json.RawMessage) error {
	var code, detail string
	if json.Unmarshal(body["errorCode"], &code) != nil || code == "" || json.Unmarshal(body["errorDetail"], &detail) != nil || detail == "" {
		return fmt.Errorf("failed kubernetes result requires errorCode and errorDetail")
	}
	return nil
}

func validateKubernetesMappingResult(raw json.RawMessage) error {
	var result map[string]json.RawMessage
	if json.Unmarshal(raw, &result) != nil {
		return fmt.Errorf("kubernetes result item must be an object")
	}
	allowed := map[string]bool{"success": true, "output": true, "truncated": true, "errorCode": true, "errorDetail": true}
	for key := range result {
		if !allowed[key] {
			return fmt.Errorf("kubernetes result item field %q is not allowed", key)
		}
	}
	var success bool
	if rawSuccess, ok := result["success"]; !ok || json.Unmarshal(rawSuccess, &success) != nil {
		return fmt.Errorf("kubernetes result item requires boolean success")
	}
	if success {
		var output string
		var truncated bool
		if json.Unmarshal(result["output"], &output) != nil || json.Unmarshal(result["truncated"], &truncated) != nil {
			return fmt.Errorf("successful kubernetes result item requires output and truncated")
		}
		return nil
	}
	return validateKubernetesFailure(result)
}
