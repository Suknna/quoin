package attempt

// Built-in plugin descriptors (ADR-0004). The TOOL declarations are derived
// from this package's compiled implementation tables — the descriptor and
// the ToolDef are one authority, so version/location/description drift is
// structurally impossible. Only the plugin-level identity metadata (names,
// descriptions, connection kinds, enablement defaults) is authored here.
// A host process loads these into its registry; the package registers
// nothing by itself.

import (
	"fmt"

	"github.com/Suknna/quoin/internal/plugins"
)

// executionModeLocation is the reverse of LocationExecutionModes.
var executionModeLocation = map[string]plugins.ExecutionLocation{
	"worker_local":     plugins.LocationWorkerLocal,
	"supervisor_typed": plugins.LocationPlinthSupervisor,
	"quoin_browser":    plugins.LocationLintel,
}

// DescriptorTool projects a plugin-owned ToolDef into its registry
// declaration. Plugin packages derive their descriptor tool fields from
// their own implementation with this helper — the declaration and the
// implementation stay one authority without editing attempt core.
func DescriptorTool(def ToolDef) (plugins.Tool, error) {
	location, ok := executionModeLocation[def.ExecutionMode]
	if !ok {
		return plugins.Tool{}, fmt.Errorf("tool %s has no descriptor execution location", def.Name)
	}
	return plugins.Tool{
		Name: def.Name, Version: def.Version, ExecutionLocation: location,
		FailureMode: def.FailureMode, Description: def.Description,
		Parameters: CanonicalToolParameters(def),
	}, nil
}

// builtinDescriptorTool projects one compiled definition into its registry
// declaration.
func builtinDescriptorTool(name string) plugins.Tool {
	def, ok := compiledToolByName(name)
	if !ok {
		panic("built-in tool " + name + " must be part of the compiled catalog")
	}
	location, ok := executionModeLocation[def.ExecutionMode]
	if !ok {
		panic("built-in tool " + name + " has no descriptor execution location")
	}
	return plugins.Tool{
		Name: def.Name, Version: def.Version, ExecutionLocation: location,
		FailureMode: def.FailureMode, Description: def.Description,
		Parameters: CanonicalToolParameters(def),
	}
}

// metricsConfigSchema is the real instance-settings schema of the metrics
// plugins: exactly the typed connection revision fields the supervisor's
// executor parses (plinthconnections.MetricsConfig). baseUrl is the only
// mandatory field; `type` is constrained per plugin when present.
func metricsConfigSchema(connectionType string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"type":            map[string]any{"type": "string", "enum": []any{connectionType}},
			"baseUrl":         map[string]any{"type": "string", "minLength": 1},
			"tlsCaPem":        map[string]any{"type": "string"},
			"tlsServerName":   map[string]any{"type": "string"},
			"tlsSkipVerify":   map[string]any{"type": "boolean"},
			"authType":        map[string]any{"type": "string", "enum": []any{"none", "basic", "bearer"}},
			"username":        map[string]any{"type": "string"},
		},
		"required": []any{"type", "baseUrl"},
	}
}

// BuiltinDescriptors returns the built-in plugin descriptors in stable ID
// order. Enablement defaults: the metrics/alerting/kubernetes mainline is
// enabled when the deployment YAML is silent. The browser plugin descriptor
// has been retired from the active catalog (受控浏览器退役): the quoin_browser
// tool implementation tables stay compiled for recovery, but no descriptor
// advertises them, so resolve/enablement/registration surfaces never expose
// Lintel again. Descriptors claim only capabilities with real bindings in
// this architecture: CapabilityTools for the plugins whose model tools the
// attempt catalog actually serves.
func BuiltinDescriptors() []plugins.Descriptor {
	return []plugins.Descriptor{
		{
			ID:          plugins.PrometheusID,
			Version:     "1",
			DisplayName: "Prometheus",
			Description: "连接 Prometheus 实例：作为指标查询的来源接入，提供与 Thanos 同契约的只读 PromQL 模型工具；授权按实际来源连接解析。",
			// 有界观测声明与 Plinth 进程中真实注册的 Discoverer 执行绑定
			// 同切片落地（internal/plinth/supervisor），声明不再空悬。
			Capabilities: []plugins.Capability{plugins.CapabilityDiscover, plugins.CapabilityTools, plugins.CapabilityInspectionTemplates, plugins.CapabilityCollect},
			DiscoverObjects: []plugins.DiscoverObject{
				{ObjectType: "target", IdentityLabels: []string{"job", "instance"}, Query: "up", Limit: 500},
			},
			// 确定性巡检模板目录（与 Plinth supervisor 的执行绑定一一对应）。
			InspectionTemplates: []plugins.InspectionTemplate{
				{ID: "promql_instant", Version: "1", Title: "PromQL 即时查询", Description: "以 evidence_at 为观测点执行一次即时向量查询"},
				{ID: "promql_range", Version: "1", Title: "PromQL 范围查询", Description: "以 evidence_at 为终点执行一次范围查询并保存实际窗口"},
			},
			// 同契约共享工具：与 thanos 插件声明完全相同的 PromQL 查询工具，
			// 启用任一提供者即有真实查询能力；目录去重，溯源列全部启用的
			// 贡献提供者。
			Tools:          []plugins.Tool{builtinDescriptorTool("thanos_query")},
			ConfigSchema:   metricsConfigSchema("prometheus"),
			DefaultEnabled: true,
			ConnectionKind: "prometheus",
		},
		{
			ID:           plugins.ThanosID,
			Version:      "1",
			DisplayName:  "Thanos",
			Description:  "连接 Prometheus 兼容的全局查询层：提供资源范围内只读 PromQL 模型工具与来源接入。",
			Capabilities: []plugins.Capability{plugins.CapabilityDiscover, plugins.CapabilityTools, plugins.CapabilityInspectionTemplates, plugins.CapabilityCollect},
			DiscoverObjects: []plugins.DiscoverObject{
				{ObjectType: "target", IdentityLabels: []string{"job", "instance"}, Query: "up", Limit: 500},
			},
			InspectionTemplates: []plugins.InspectionTemplate{
				{ID: "promql_instant", Version: "1", Title: "PromQL 即时查询", Description: "以 evidence_at 为观测点执行一次即时向量查询"},
				{ID: "promql_range", Version: "1", Title: "PromQL 范围查询", Description: "以 evidence_at 为终点执行一次范围查询并保存实际窗口"},
			},
			ConfigSchema:   metricsConfigSchema("thanos"),
			DefaultEnabled: true,
			ConnectionKind: "thanos",
			Tools:          []plugins.Tool{builtinDescriptorTool("thanos_query")},
		},
		{
			ID:             plugins.AlertmanagerID,
			Version:        "1",
			DisplayName:    "Alertmanager",
			Description:    "接收上游 Alertmanager 告警来源：接收继续遵循 Stele 的转交与 Quoin 事务确认边界。",
			DefaultEnabled: true,
			ConnectionKind: "alertmanager",
		},
		{
			ID:             plugins.KubernetesID,
			Version:        "1",
			DisplayName:    "Kubernetes",
			Description:    "受限 Kubernetes 集群访问接入：来源连接与凭据轮换；观测与模型工具能力按切片交付。",
			DefaultEnabled: true,
			ConnectionKind: "kubernetes",
		},
	}
}
