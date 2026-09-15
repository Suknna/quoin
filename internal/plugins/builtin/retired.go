package builtin

// Retired plugin implementations (受控退役): the browser and kubernetes
// plugins are out of the active business, but their compiled tool
// implementations stay under the SAME registration mechanism — the
// descriptors below remain registered flagged Retired as the declaration
// authority, so frozen historical attempts keep resolving their contracts
// by tool name + version + agreement, while enablement and every
// newly-frozen catalog exclude them. Nothing here can re-advertise or
// re-enable a retired plugin.

import (
	"github.com/Suknna/quoin/internal/plugins"
)

// BrowserTool is the compiled quoin_browser implementation, kept for
// ingress validation and frozen-catalog compatibility of historical
// executions. It enters no newly frozen catalog: its descriptor is retired.
// v2 is a breaking locator change (ADR-0004): open addresses the identity
// by its standalone identityKey instead of the retired businessSystemKey,
// so a frozen v1 catalog drift-rejects explicitly instead of being
// reinterpreted.
var BrowserTool = plugins.ToolDef{
	Name: "quoin_browser", Version: "2", ExecutionMode: "quoin_browser", FailureMode: "return_to_model", ResultSchemaKind: "browser_tool_result_v1",
	Description: "在已授权的浏览器身份中执行一个封闭的探索动作。open 通过独立身份的稳定 identityKey 定位身份（不再使用业务系统）。只接受 open、页面导航、元素交互、受限读取、截图和会话关闭；不接受 JavaScript、HTTP、CDP 或 Playwright 指令。",
	Parameters:  browserToolParameters(), ValidateArguments: validateBrowserToolArguments, ValidateResult: validateBrowserToolResult,
}

// KubernetesReadTool is the compiled kubernetes_read implementation. The
// kubernetes plugin is retired; the Plinth supervisor keeps serving the
// tool for attempts whose frozen catalog predates the retirement, and the
// compiled definition keeps frozen-catalog compatibility and ingress
// validation exact.
var KubernetesReadTool = plugins.ToolDef{
	Name: "kubernetes_read", Version: "1", ExecutionMode: "supervisor_typed", FailureMode: "return_to_model", ResultSchemaKind: "kubernetes_read_result_v1", ProducesEvidence: true, RequiresConnectionGrant: true,
	Description: "对已启用 Kubernetes 来源执行只读观察动作。operation 必填；namespace/name/container 按动作需要提供；来源有歧义时用 sourceRef 显式命名，或提供 businessSystem 按业务视图收窄。Quoin 逐个冻结 grant 独立执行，结果作为不可变 Evidence 封存。",
	Arguments: map[string]plugins.ArgumentKind{
		"operation": plugins.KindString, "namespace": plugins.KindString, "name": plugins.KindString,
		"container": plugins.KindString, "businessSystem": plugins.KindString, "sourceRef": plugins.KindString,
	},
	Required: []string{"operation"},
}

// browserDescriptor is the retired browser plugin declaration: the
// declaration authority of the compiled quoin_browser implementation, never
// advertised in the management catalog and never enableable.
func browserDescriptor() plugins.Descriptor {
	return plugins.Descriptor{
		ID:             plugins.BrowserID,
		Version:        "1",
		DisplayName:    "Browser",
		Description:    "受控浏览器探索（已退役）：描述仅保留为历史冻结目录的实现声明权威，不再通告或启用。",
		Retired:        true,
		Capabilities:   []plugins.Capability{plugins.CapabilityTools},
		Tools:          []plugins.Tool{mustDescriptorTool(BrowserTool)},
		ConnectionKind: "browser",
	}
}

// kubernetesDescriptor is the retired kubernetes plugin declaration: the
// declaration authority of the compiled kubernetes_read implementation.
func kubernetesDescriptor() plugins.Descriptor {
	return plugins.Descriptor{
		ID:             plugins.KubernetesID,
		Version:        "1",
		DisplayName:    "Kubernetes",
		Description:    "受限 Kubernetes 集群访问接入（已退役）：描述仅保留为历史冻结目录的实现声明权威，不再通告或启用。",
		Retired:        true,
		Capabilities:   []plugins.Capability{plugins.CapabilityTools},
		Tools:          []plugins.Tool{mustDescriptorTool(KubernetesReadTool)},
		ConnectionKind: "kubernetes",
	}
}

// PluginTools returns the compiled tool implementations the ACTIVE built-in
// plugins contribute (dedup shared contracts).
func PluginTools() []plugins.ToolDef {
	return []plugins.ToolDef{QueryTool}
}

// RetiredTools returns the compiled implementations the retired plugins
// keep for frozen-history compatibility.
func RetiredTools() []plugins.ToolDef {
	return []plugins.ToolDef{BrowserTool, KubernetesReadTool}
}
