package worker

// 冻结契约钉子(ADR-0011 之后 Plinth 对插件体系零感知,不再与
// Quoin 侧 attempt 包交叉钉住):worker 的 provider 工具目录渲染、执行
// 模式词表归一(含 supervisor_typed -> quoin_routed 的迁移映射)与 agent
// 版本身份都在本地钉死,防止静默漂移。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
)

// fixtureCatalogInput 构造一个内嵌冻结目录的 canonical input 文档
// ({toolCatalog:{tools:[...]}}),覆盖 worker_local 与 quoin_routed 两种模式。
func fixtureCatalogInput(t *testing.T) []byte {
	t.Helper()
	catalog := map[string]any{
		"schemaVersion": "tools-v1",
		"agentVersion":  WorkerAgentVersion,
		"tools": []map[string]any{
			{"name": "bash", "version": "1", "executionMode": "worker_local", "failureMode": "return_to_model", "description": "在工作区执行一条 bash 命令。", "parameters": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}, "required": []string{"command"}}},
			{"name": "thanos_query", "version": "4", "executionMode": "quoin_routed", "failureMode": "return_to_model", "description": "执行一条 PromQL 即时查询。", "parameters": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}}},
		},
	}
	body, err := json.Marshal(map[string]any{"toolCatalog": catalog})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestProviderToolsJSONForInputRendersFrozenCatalog(t *testing.T) {
	input := fixtureCatalogInput(t)
	rendered, err := ProviderToolsJSONForInput(input, WorkerAgentVersion)
	if err != nil {
		t.Fatal(err)
	}
	// 渲染形状冻结:嵌套 OpenAI function 对象,逐工具 name/description/
	// parameters 原样来自冻结目录。
	var tools []map[string]any
	if err := json.Unmarshal(rendered, &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("rendered %d tools, want 2", len(tools))
	}
	first := tools[0]["function"].(map[string]any)
	if first["name"] != "bash" || tools[0]["type"] != "function" {
		t.Fatalf("first rendered tool drifted: %s", rendered)
	}
	if _, ok := first["parameters"].(map[string]any); !ok {
		t.Fatalf("parameters must render as an object: %s", rendered)
	}
	digest, err := ProviderToolsDigestForInput(input, WorkerAgentVersion)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(rendered)
	if digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest drift: %s != %s", digest, hex.EncodeToString(sum[:]))
	}
}

func TestProviderToolsJSONForInputRejectsLegacyInput(t *testing.T) {
	if _, err := ProviderToolsJSONForInput([]byte(`{"schemaKind":"initial_analysis_v1"}`), WorkerAgentVersion); err == nil {
		t.Fatal("legacy input without a frozen catalog must fail explicitly")
	}
	if _, err := ExecutionModesForInput([]byte(`{}`)); err == nil {
		t.Fatal("execution mode resolution must fail without a frozen catalog")
	}
}

func TestExecutionModesForInputNormalizesVocabulary(t *testing.T) {
	modes, err := ExecutionModesForInput(fixtureCatalogInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if modes["bash"] != "TOOL_EXECUTION_MODE_WORKER_LOCAL" {
		t.Fatalf("bash must be worker_local, got %q", modes["bash"])
	}
	if modes["thanos_query"] != "TOOL_EXECUTION_MODE_QUOIN_ROUTED" {
		t.Fatalf("thanos_query must be quoin_routed, got %q", modes["thanos_query"])
	}
}

func TestExecutionModesForInputRejectsUnknownMode(t *testing.T) {
	input := []byte(`{"toolCatalog":{"tools":[{"name":"odd","executionMode":"elsewhere","description":"d","parameters":{"type":"object"}}]}}`)
	if _, err := ExecutionModesForInput(input); err == nil {
		t.Fatal("unknown execution mode must fail fast")
	}
}

// Agent 版本身份钉子:与 Quoin 侧 attempt/investigation 常量的相等性原本
// 由跨包测试钉住;ADR-0011 后 Plinth 不再 import Quoin 内部包,这里改为
// 钉住字面量——身份变更必须是有意识的契约变更。
func TestAgentVersionIdentityPins(t *testing.T) {
	pins := map[string]string{
		"WorkerAgentVersion":                             WorkerAgentVersion,
		"PreviousAnalysisAgentVersion":                   PreviousAnalysisAgentVersion,
		"LegacyInitialAnalysisAgentVersion":              LegacyInitialAnalysisAgentVersion,
		"KnowledgeExtractionAgentVersion":                KnowledgeExtractionAgentVersion,
		"InspectionAnalysisAgentVersion":                 InspectionAnalysisAgentVersion,
		"KeptInspectionAnalysisAgentVersion":             KeptInspectionAnalysisAgentVersion,
		"ReportComplianceInspectionAnalysisAgentVersion": ReportComplianceInspectionAnalysisAgentVersion,
		"PreviousInspectionAnalysisAgentVersion":         PreviousInspectionAnalysisAgentVersion,
		"WorkerInvestigationAgentVersion":                WorkerInvestigationAgentVersion,
		"KeptInvestigationAgentVersion":                  KeptInvestigationAgentVersion,
		"PreviousInvestigationAgentVersion":              PreviousInvestigationAgentVersion,
	}
	want := map[string]string{
		"WorkerAgentVersion":                             "initial-analysis-v3",
		"PreviousAnalysisAgentVersion":                   "initial-analysis-v2",
		"LegacyInitialAnalysisAgentVersion":              "initial-analysis-v1",
		"KnowledgeExtractionAgentVersion":                "initial-analysis-v1",
		"InspectionAnalysisAgentVersion":                 "inspection-analysis-v4",
		"KeptInspectionAnalysisAgentVersion":             "inspection-analysis-v3",
		"ReportComplianceInspectionAnalysisAgentVersion": "inspection-analysis-v2",
		"PreviousInspectionAnalysisAgentVersion":         "inspection-analysis-v1",
		"WorkerInvestigationAgentVersion":                "investigation-v4",
		"KeptInvestigationAgentVersion":                  "investigation-v3",
		"PreviousInvestigationAgentVersion":              "investigation-v2",
	}
	for name, value := range pins {
		if value != want[name] {
			t.Fatalf("%s drift: got %s, want %s", name, value, want[name])
		}
	}
	if LegacyInvestigationAgentVersion != "investigation-v1" {
		t.Fatalf("legacy investigation identity drift: %s", LegacyInvestigationAgentVersion)
	}
}

func TestReadOnlyRuntimePathsParse(t *testing.T) {
	paths, err := ReadOnlyRuntimePaths()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, path := range paths {
		found[path] = true
	}
	for _, required := range []string{"/usr/bin", "/usr/lib", "/etc/alternatives", "/etc/ld.so.cache", "/dev/null", "/dev/urandom", "/bin/bash"} {
		if !found[required] {
			t.Fatalf("frozen readonly path %s missing", required)
		}
	}
	_ = gencontracts.PlinthWorkerToolsYAML
}
