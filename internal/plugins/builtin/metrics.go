// Package builtin declares the deployment's built-in plugins (ADR-0004):
// each plugin owns its descriptor AND its compiled tool implementations in
// one place, so declaration and implementation are one authority and adding
// a plugin never edits a core table. The package registers nothing by
// itself — hosts load these declarations into their process
// plugins.Registry, and every consumer (frozen catalogs, implementation
// lookup, executor dispatch) assembles from that one registry.
//
// The metrics plugins (prometheus/thanos) share one PromQL query tool
// contract: the catalog keeps a single entry and provenance lists every
// enabled contributing provider; authorization resolves the actual source
// connection.
package builtin

import (
	"github.com/Suknna/quoin/internal/plugins"
)

// metricsConfigSchema is the real instance-settings schema of the metrics
// plugins: exactly the typed connection revision fields the supervisor's
// executors parse (plinthconnections.MetricsConfig/ThanosConfig). baseUrl is
// the only mandatory field; `type` is constrained per plugin when present.
func metricsConfigSchema(connectionType string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"type":          map[string]any{"type": "string", "enum": []any{connectionType}},
			"baseUrl":       map[string]any{"type": "string", "minLength": 1},
			"tlsCaPem":      map[string]any{"type": "string"},
			"tlsServerName": map[string]any{"type": "string"},
			"tlsSkipVerify": map[string]any{"type": "boolean"},
			"authType":      map[string]any{"type": "string", "enum": []any{"none", "basic", "bearer"}},
			"username":      map[string]any{"type": "string"},
		},
		"required": []any{"type", "baseUrl"},
	}
}

// QueryTool is the metrics plugins' shared compiled PromQL query tool: the
// complete frozen contract (version, execution location, result schema,
// model-facing description) owned by the plugin, not by any core table.
// v3 makes sourceRef the only optional locator and drops resourceRef.
var QueryTool = plugins.ToolDef{
	Name: "thanos_query", Version: "3", ExecutionMode: "supervisor_typed", FailureMode: "return_to_model", ResultSchemaKind: "thanos_query_result_v1", ProducesEvidence: true, RequiresConnectionGrant: true,
	Description: "执行只读 PromQL 即时查询。必须提供 query；sourceRef 可选，仅在来源有歧义时显式命名来源连接，Quoin 按冻结授权解析连接、范围与必需 labels，结果作为不可变 Evidence 封存。",
	Arguments:   map[string]plugins.ArgumentKind{"query": plugins.KindString, "sourceRef": plugins.KindString},
	Required:    []string{"query"},
}

// prometheusInspectionTemplates / thanosInspectionTemplates share the same
// deterministic PromQL collection catalog; the (pluginID, templateID,
// version) triple is the identity the inspection plans bind.
func promQLInspectionTemplates() []plugins.InspectionTemplate {
	return []plugins.InspectionTemplate{
		{ID: "promql_instant", Version: "1", Title: "PromQL 即时查询", Description: "以 evidence_at 为观测点执行一次即时向量查询"},
		{ID: "promql_range", Version: "1", Title: "PromQL 范围查询", Description: "以 evidence_at 为终点执行一次范围查询并保存实际窗口"},
	}
}

func promQLDiscoverObjects() []plugins.DiscoverObject {
	return []plugins.DiscoverObject{
		{ObjectType: "target", IdentityLabels: []string{"job", "instance"}, Query: "up", Limit: 500},
	}
}

// mustDescriptorTool projects one owned implementation into its registry
// declaration; a built-in tool whose mode has no descriptor location is a
// programming error pinned by tests, not a runtime condition.
func mustDescriptorTool(def plugins.ToolDef) plugins.Tool {
	tool, err := def.DescriptorTool()
	if err != nil {
		panic("built-in tool " + def.Name + " has no descriptor execution location: " + err.Error())
	}
	return tool
}

// Descriptors returns the built-in plugin descriptors in stable ID order.
// Enablement defaults: the metrics/alerting mainline is enabled when the
// deployment YAML is silent. Descriptors claim only capabilities with real
// bindings in this architecture.
func Descriptors() []plugins.Descriptor {
	queryDeclaration := mustDescriptorTool(QueryTool)
	return []plugins.Descriptor{
		{
			ID:          plugins.PrometheusID,
			Version:     "1",
			DisplayName: "Prometheus",
			Description: "连接 Prometheus 实例：作为指标查询的来源接入，提供与 Thanos 同契约的只读 PromQL 模型工具；授权按实际来源连接解析。",
			// 有界观测声明与 Plinth 进程中真实注册的 Discoverer 执行绑定
			// 同切片落地（internal/plinth/supervisor）。
			Capabilities:        []plugins.Capability{plugins.CapabilityDiscover, plugins.CapabilityTools, plugins.CapabilityExecuteTool, plugins.CapabilityInspectionTemplates, plugins.CapabilityCollect},
			DiscoverObjects:     promQLDiscoverObjects(),
			InspectionTemplates: promQLInspectionTemplates(),
			// 同契约共享工具：与 thanos 插件声明完全相同的 PromQL 查询工具，
			// 启用任一提供者即有真实查询能力；目录去重，溯源列全部启用的
			// 贡献提供者。
			Tools:          []plugins.Tool{queryDeclaration},
			ConfigSchema:   metricsConfigSchema("prometheus"),
			DefaultEnabled: true,
			ConnectionKind: "prometheus",
		},
		{
			ID:                  plugins.ThanosID,
			Version:             "1",
			DisplayName:         "Thanos",
			Description:         "连接 Prometheus 兼容的全局查询层：提供资源范围内只读 PromQL 模型工具与来源接入。",
			Capabilities:        []plugins.Capability{plugins.CapabilityDiscover, plugins.CapabilityTools, plugins.CapabilityExecuteTool, plugins.CapabilityInspectionTemplates, plugins.CapabilityCollect},
			DiscoverObjects:     promQLDiscoverObjects(),
			InspectionTemplates: promQLInspectionTemplates(),
			ConfigSchema:        metricsConfigSchema("thanos"),
			DefaultEnabled:      true,
			ConnectionKind:      "thanos",
			Tools:               []plugins.Tool{queryDeclaration},
		},
		{
			ID:             plugins.AlertmanagerID,
			Version:        "1",
			DisplayName:    "Alertmanager",
			Description:    "接收上游 Alertmanager 告警来源：接收继续遵循 Stele 的转交与 Quoin 事务确认边界。",
			DefaultEnabled: true,
			ConnectionKind: "alertmanager",
		},
	}
}

// PluginTools returns the compiled tool implementations the built-in
// plugins contribute (dedup shared contracts).
func PluginTools() []plugins.ToolDef {
	return []plugins.ToolDef{QueryTool}
}
