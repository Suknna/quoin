---
status: accepted
---

# 插件契约与冻结工具目录（ADR-0004 实施规范）

> **本文大部分条款已被 ADR-0011（2026-09-20）取代：** 插件体系 v2 改为接口 + 注册表 + 空白导入装配与泛型工具（`internal/plugins` 的 `Plugin`/`EventSource`/`ToolProvider`/`Tool[A,R]`），能力六分类、ExecutionBundle 与执行位置词表退役；现行权威见 [ADR-0011](../../../adr/0011-component-responsibility-and-plugin-v2.md) 与[插件开发指南](../../../plugin-development.md)。冻结工具目录、启用解析与"声明派生自实现"的不变式未变。下文仅作历史规范解读。

> **浏览器与 Kubernetes 插件已移除（2026-09）：** `browser` 与 `kubernetes` 插件（含描述符、工具实现与历史兼容层）已从代码与契约中彻底删除；历史冻结目录与旧库中的相关数据不再保证可解析。文中相关条款仅作历史解读。

本规范描述 ADR-0004 的实际类型、接口与接线。以代码为准：`internal/plugins`（契约）、`internal/quoin/attempt`（冻结目录与内建描述符）、`internal/quoin/app/plugins.go`（接线与管理目录）。

## 1. 契约包 internal/plugins

纯数据契约，无 Quoin 业务依赖，Plinth/Quoin 均可链接：

- `Descriptor`：稳定 `ID`（`^[a-z][a-z0-9_-]*$`，永不复用）、`Version`、`DisplayName`/`Description`、`Capabilities`、`DefaultEnabled`、`ConnectionKind`、`ConfigSchema`（可选封闭 JSON Schema，draft 2020-12、顶层 object、`additionalProperties:false`；秘密槽位由后端生成引用，用户不手填 ref）、`Tools []Tool`、`InspectionTemplates []InspectionTemplate`。
- `Tool`：`Name`（`^[a-z][a-z0-9_]*$`，跨插件全局唯一）、`Version`、`ExecutionLocation`（`worker_local|plinth_supervisor|quoin`）、`FailureMode`（`return_to_model|fail_attempt`）、`Description`。
- 能力一致性（注册时强制）：`tools` 能力 ⇔ 工具目录非空；`inspection_templates` ⇔ 模板目录非空；`execute_tool` 依附 `tools`；`collect` 依附 `inspection_templates`。
- `Registry`：`RegisterDescriptor` / `RegisterBundle` / `Descriptor(s)` / `Bundle` / `ToolOwner` / `ResolveEnabled`。注册错误是确定性构建/启动失败（`ErrDuplicateDescriptor`、`ErrDuplicateToolName`、`ErrInvalidDescriptor`、`ErrUndeclaredBundle`、`ErrInvalidBundle`、`ErrDuplicateBundle`、`ErrUnknownPlugin`、`ErrCatalogFrozen`）。
- 描述与执行分离：`RegisterDescriptor` 允许纯描述宿主（如 Quoin 控制面）；`RegisterBundle` 绑定本进程真实执行（`Prober/Discoverer/ToolExecutor/Collector`），并强制能力集合与非 nil 接口精确相等、工具执行位置一致。进程内绑定一致性由注册校验保证；跨组件覆盖（每个被宣称的执行能力在部署内确有绑定）是接线层的部署职责，目前以验收清单与本文档核对，尚无自动化矩阵。

### 执行绑定接口（真实签名，摘自 plugin.go / types.go）

四个可选执行接口（绑定于 `ExecutionBundle`，`plugin.go`）：

```go
type Prober interface {
    Probe(ctx context.Context, call *Call, request ProbeRequest) (*ProbeResult, error)
}
type Discoverer interface {
    Discover(ctx context.Context, call *Call, request DiscoverRequest) (*DiscoverResult, error)
}
type ToolExecutor interface {
    ExecuteTool(ctx context.Context, call *Call, request ToolRequest) (*ToolResult, error)
}
type Collector interface {
    Collect(ctx context.Context, call *Call, request CollectRequest) (*CollectResult, error)
}
```

`Call`（`plugin.go`）是执行宿主逐次构造的冻结调用：`PluginID`、`Settings json.RawMessage`（已按 ConfigSchema 校验的非秘密设置）、`SecretRefs map[string]string`（命名槽 → **不透明控制面凭据引用**；槽名不是固定协议，引用解析才是契约）、`Secrets SecretResolver`（`Resolve(ctx, ref) ([]byte, error)`：仅本次调用解析明文，永不入快照/日志/Evidence；worker 子进程与描述宿主永不持有）。

载荷语义（`types.go`）：

| 请求/结果 | 字段与语义 |
| --- | --- |
| `ProbeRequest` | `Timeout` 有界探测预算；被探测实例与其冻结设置经 `Call` 携带 |
| `ProbeResult` | `Reachable`/`LatencyMS`/`Detail`（有界非秘密失败说明） |
| `DiscoverRequest` | `ObjectType` 选择对象类别；`Limit` 硬结果预算 |
| `DiscoverResult` | `Objects []DiscoveredObject{ObjectType, CanonicalIdentity, DisplayName}`；`Incomplete=true` 表示截断/局部——观测身份=（来源连接, ObjectType, CanonicalIdentity），不完整不得表达缺失 |
| `ToolRequest` | `Name` 必须属于该插件声明目录；`ArgumentsJSON` 满足该工具冻结参数 schema |
| `ToolResult` | `Success`/`Payload`（工具结果 schema kind 的封闭 JSON）/`ErrorCode`/`ErrorDetail`（失败时有界、不含凭据） |
| `CollectRequest` | `TemplateID`/`TemplateVersion`（Run 冻结的模板身份）、`Params`（冻结模板参数对象，metrics 插件为 PromQL 表达式与窗口秒数）、`EvidenceAt`（RFC3339 观测点，范围模板窗口终点）、`Scope CollectScope{Kind}`（**强制非空**，封闭词表 `integration\|businessView\|objects`）、`Targets []CollectTarget{ObjectType, CanonicalIdentity, LabelConditions}` |
| `CollectResult` | `Checks []CheckObservation{CheckID, Succeeded, Detail, EvidenceJSON}`（确定性事实，模型只读不改）；`Incomplete` 标记未完成的采集轮 |

采集范围语义（`CollectScope`/`CollectTarget.LabelConditions`，执行实现 `internal/plinth/supervisor/inspection_plugin.go`）：

- `integration`：显式整源——接入本身即采集边界，无条件注入；
- `businessView`/`objects`：`LabelConditions` 是控制面冻结的**非空精确 label 条件**（空条件/空键/空值/非法 label 名一律拒绝），由官方 PromQL AST 逐 selector 注入——结果后过滤不能替代收窄；
- `businessView`/`objects` 每个 child 恰一个 target 且 labels 非空；不支持或缺失 scope 一律拒绝；
- Run 的业务视图 labels 为空时**拒绝并要求改为显式 `integration`**，绝不静默放宽到全源。

### 设置校验的真实分工（ConfigValidator 是可选扩展，当前无消费者）

`ConfigValidator`（`plugin.go`）是**可选**静态扩展接口：`ValidateConfig(settings json.RawMessage) error`——纯静态、无 I/O，任何宿主可持有，用于超出声明 Schema 的插件专属规则（跨字段约束等）。当前**代码库中没有任何调用方**；它不参与注册，也不在任何执行路径上被自动调用。

设置校验的实况分工：

- **注册时**：`Registry` 只校验 `ConfigSchema` 的**结构**（顶层 object、`additionalProperties:false`、`$schema` 为 draft 2020-12）——见 `internal/plugins/validate.go`。Schema 内容本身不被执行。
- **实例时**：`Registry.ValidateConfig(pluginID, settings)`（`internal/plugins/schema_validate.go`）用项目 JSON Schema 校验器（santhosh-tekuri/jsonschema/v6，每插件编译缓存）执行声明 schema：未知字段/缺必需字段/类型错误即 `ErrInvalidSettings` 拒绝；nil schema 的插件拒绝任何非空设置（不得自称无配置却有 Settings）。真实消费：metrics 的 Discover 与 Collect 路径在构造 Call 前（`newMetricsCall`，`internal/plinth/supervisor/metrics_call.go`）强制校验，prometheus/thanos 描述符声明与 `plinthconnections.MetricsConfig` typed 字段一致的真实 schema（凭据明文不进 Settings，经 `Secrets` 解析）。
- **覆盖边界（如实）**：metrics 的模型工具调用（thanos_query）与连接探测不走 plugins.Call——它们复用既有 Connections 域的强校验（grant 事务内 `ValidateGrantForExecution`/typed 连接配置解析）；`Prober` bundle 路径尚无生产绑定，接通时同样必须在构造 Call 前调用 `ValidateConfig`。`ConfigValidator` 仍是无消费者的可选扩展接口。

秘密解析语义：`SecretRefs` 是命名槽 → 不透明控制面凭据引用的映射；**槽名不是固定协议**（supervisor 的 metrics 路径内部使用 username/password/bearerToken 等键属宿主实现细节），契约是 `Secrets.Resolve(ctx, ref)` 逐引用解析、仅本次调用有效。

### 执行绑定到宿主路由的唯一适配（无第二机制）

`plugins.ExecutionBundle.ToolExecutor` 是工具执行的唯一声明来源；宿主的执行路由注册表只是它的**适配层**：

- 插件自持声明与实现：每个插件包同时拥有 Descriptor 与编译 ToolDef，工具声明由 `ToolDef.DescriptorTool()` 从实现派生，声明与实现在构造上不可漂移；`internal/plugins/builtin` 是内建插件的唯一声明源。
- Plinth 进程装配（`supervisor.HostRegistry`）：从共享 builtin 源注册全部内建描述符，并按描述符声明的能力绑定本进程真实执行 bundle——观测 Discoverer、巡检 Collector 与指标插件的 `ToolExecutor`（`internal/plinth/worker` 的执行适配器）。一个进程每插件恰有一个 bundle。
- 分发表装配（`worker.AssembleTypedExecutors(registry, table)`）：平台工具（artifact_read/artifact_grep）由宿主注册；每个 supervisor 位置的插件工具解析其 owner 描述符与绑定 bundle，经 `worker.RegisterPluginExecutor(descriptor, bundle, table)` 注册。注册即验证：bundle 归属描述符、携带 `execute_tool` 且 `ToolExecutor` 非 nil、位置为本进程、每个声明工具与装配实现表逐字段一致（版本/失败模式/执行位置/参数 schema 规范化字节相等）。装配完成即冻结：二次装配与冻结后注册是确定性 wiring 失败；未注册工具显式 `unknown_tool` 失败。
- 运行时消费者：supervisor 的 Runner 帧桥（`internal/plinth/worker` 的 `executeTool`→`executeTypedTool`）查装配表执行；`plugins.Call` 由 Runner 自身解析（冻结 grant → 连接配置与 SecretResolver），秘密只在 supervisor 进程解析，冻结失败码（如 `grant_missing`）经类型化错误保持线上语义。serve 接线（`internal/plinth/ops`）先装配 registry 与分发表再接受派发。
- 启用解析：`ResolveEnabled(nil)` 返回全部 `DefaultEnabled` 的插件（默认 prometheus/thanos/alertmanager）；非 nil 白名单中未知 ID 一律启动失败；**省略与空数组是不同的部署事实**（空数组=全部停用）。
- 同契约共享工具：同一工具契约（name/version/location/failureMode/description/parameters 逐字段相同）可被多个提供者插件声明（如 PromQL 查询之于 prometheus/thanos——授权按实际来源连接解析）。注册表保存一份规范声明，分叉声明被拒；冻结目录按工具名去重，`plugins` 溯源列全部启用的贡献提供者。因此 only-prometheus 与 only-thanos 部署都有真实查询工具。

## 2. 冻结工具目录 internal/quoin/attempt

- 单一权威：内建插件在其包内自持描述符与编译实现，描述声明派生自实现，版本/位置/说明不可能漂移。工具名全局唯一由注册表保证：每个工具名保存**一份规范声明**（首个注册者即 `ToolOwner`）；其他提供者插件只能以**逐字段相同的契约**再声明（分叉即 `ErrDuplicateToolName`），不会产生第二个目录条目。
- 组装：`attempt.BuildCatalogs(registry, implementations, enabled)` 从一个 registry 与实现表按代际策略（`generationAccepts`：initial-analysis 接受 `worker_local|plinth_supervisor`，investigation 同）生成每 agent 代的 `FrozenCatalog` **和**共享实现索引，并双向校验（每个声明必有逐字段一致的实现；每个非平台实现必有注册声明）。平台工具（bash/read/write/grep/artifact_read/artifact_grep）恒在；插件工具仅当其插件启用且位置被该代接受。新增插件不改核心 map/switch。
- 冻结文档 `FrozenCatalog`：`schemaVersion`、`agentVersion`、`plugins`（id+version 溯源）、`tools`（完整 name/version/executionMode/failureMode/resultSchemaKind/resultSchemaDigest/description/parameters）。参数与结果契约完整入文，不仅是名字。
- 存储：创建时写入 `attempt_input_snapshots.tool_catalog_json`（可空；NULL=非 agent 尝试或历史尝试），并将**同一文档**内嵌进规范输入 JSON 的 `toolCatalog` 字段——快照 `content_digest` 真正覆盖目录，读写同语义。恢复重建从存储列读取，不从当前启用重推导，历史 active 尝试零漂移。
- 执行：`BeginModelCall` 校验 worker 摘要 = 冻结目录 `Digest()`；`CompleteModelCall`/`BeginToolCall`/`ExpectedToolResultSchema` 按 attempt 冻结目录解析工具，并经 `InstalledDefinition()` 验证与已安装执行器逐字段兼容（参数按规范化 JSON 字节比较、结果 Schema 按 digest 比较），不兼容即显式拒绝——历史 Model Call 的实际字节保持可解释。

### 三表的真实消费位置

| 表 | 内容 | 运行时消费者 |
| --- | --- | --- |
| 描述表（plugins.Registry） | Descriptor/Tool 声明（含 Parameters schema） | `attempt.BuildCatalogs` 装配校验与目录冻结；`GET /api/v1/integrations/plugins`；执行宿主分发表装配的 owner 解析 |
| 实现表（装配派生的 ImplementationTable） | ToolDef（参数 schema/校验器/结果契约） | 目录装配；Quoin 授权 `Catalogs.InstalledDefinition`（每次 CompleteModelCall/BeginToolCall/ExpectedToolResultSchema）；结果载荷校验分发 |
| 执行路由表（typedExecutorRegistry，supervisor 进程） | tool name → TypedExecutor（平台执行器 + 插件 bundle 适配） | supervisor Runner 帧桥 `executeTool` 在收到 worker 子进程 TypedTool 帧时执行出站；`AssembleTypedExecutors` 装配即冻结 |
- Worker/Supervisor：无新协议。worker 与 supervisor 从既有 `StartAttempt.CanonicalJson` 提取冻结目录（`attempt.CatalogFromInputDocument`）渲染/摘要；legacy 输入回退到历史代际渲染。
- 旧尝试兼容：NULL 目录列的 agent 尝试回退到**字面冻结的历史目录文档**（`legacy_catalog.go`，钉住测试守卫）。它不随当前启用集合或编译实现变化：已安装实现按工具名+版本+契约与其匹配，漂移工具显式拒绝，不自动升级或扩权，也不用当前启用目录补齐。

## 3. 管理目录 HTTP

`GET /api/v1/integrations/plugins`（admin 会话；401/403 同全局语义）：

```json
{"items":[{
  "id":"prometheus","displayName":"Prometheus","description":"连接 Prometheus 实例：作为指标查询的来源接入，提供与 Thanos 同契约的只读 PromQL 模型工具；授权按实际来源连接解析。",
  "enabled":true,"version":"1",
  "capabilities":["collect","discover","inspection_templates","tools"]
}]}
```

- `items` 按 ID 稳定排序，包含未启用插件；`capabilities` 为描述能力词表（probe/discover/tools/execute_tool/inspection_templates/collect），仅宣称有真实绑定的能力。
- `enabled` 来自部署 YAML `quoinConfig.enabledPlugins`（缺省默认集）。

## 4. 验收边界

集成与契约测试验证目录端点、冻结摘要一致性（跨 Quoin/Plinth）、启用白名单和授权拒绝规则，但只是进入真实部署验收的前置条件。必须使用仓库 `deploy` Dockerfile 和 YAML 构建部署，并实际覆盖接入、观测、Agent Tools、巡检报告、可选业务视图与告警链路。MSW、测试替身和历史截图不能替代这些操作；修复后必须重建对应镜像并复验。
