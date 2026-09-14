# 插件开发指南（ADR-0004）

面向在本仓库内为新平台/能力贡献插件的开发者。Quoin 使用**构建期显式注册**的插件：无动态 .so、无在线安装、无第二套 RPC。插件拥有自己的工具声明与执行实现；attempt 包只消费和校验——新增插件**不改核心 map/switch**。

一个**可编译、可运行的完整参考实现**见 `internal/plugins/example_plugin_test.go`（Descriptor + ConfigSchema + Tool + InspectionTemplate + ExecutionBundle[ToolExecutor/Collector] + 注册/绑定/启用解析全部断言）；本文各步骤与它一一对应。

## 1. 一个插件是什么

一个插件 = 一个 `plugins.Descriptor`（描述，含工具/模板目录）+ 按需的执行绑定 `plugins.ExecutionBundle`（真实出站执行）。部署 YAML `quoinConfig.enabledPlugins` 选择启用；未知 ID 启动失败；字段缺省时启用 `DefaultEnabled` 插件（prometheus/thanos/alertmanager/kubernetes；browser 需显式）：

```yaml
component: quoin
# ……其余 quoinConfig 字段……
enabledPlugins:            # 显式白名单；缺省 = 上面的默认主线
  - prometheus
  - thanos
  - alertmanager
  - kubernetes
  # - browser              # 可选插件：显式列出才会启用
```

## 2. 贡献一个新模型工具

1. **在你的插件包内声明实现**（不要改 attempt 核心；示例为节选，完整可编译版见 `internal/plugins/example_plugin_test.go`）：
   ```go
   package myplugin

   import (
       "context"
       "encoding/json"

       "github.com/Suknna/quoin/internal/quoin/attempt"
   )

   // Implementation 声明本插件的模型工具契约；执行体与声明同包演进。
   func Implementation() []attempt.ToolDef {
       return []attempt.ToolDef{{
           Name: "my_query", Version: "1",
           ExecutionMode: "supervisor_typed", FailureMode: "return_to_model",
           ResultSchemaKind: "my_query_result_v1",
           Description:      "在授权范围内执行只读查询。",
           Arguments:        map[string]attempt.ArgumentKind{"target": attempt.KindString},
           Required:         []string{"target"},
       }}
   }
   ```
2. **接线注册**：进程装配时 `attempt.RegisterImplementation(myplugin.Implementation()...)`（启动期一次性，重名/冻结后注册为确定性失败），随后 `attempt.BuildCatalogs(registry, enabled)` 冻结目录。
3. **声明描述符**：工具的 registry 声明**从你的 ToolDef 派生**，勿手抄第二份：
   ```go
   declaration, err := attempt.DescriptorTool(def) // name/version/location/failureMode/描述/参数schema 全部派生
   ```
   将其放入你的 `plugins.Descriptor.Tools` 并向注册表注册；描述符能力仅宣称有真实绑定的能力（`tools` ⇔ 目录非空；`execute_tool`/`probe`/`discover`/`collect` 必须先有绑定）。
4. **执行绑定（真实执行，不是声明）**：`supervisor_typed` 工具的执行体经执行路由注册接入 supervisor——绑定来源是插件自己的 `plugins.ExecutionBundle.ToolExecutor`。**注意：bundle 的 `Capabilities` 只列有非 nil 接口支撑的执行能力（`execute_tool`/`probe`/`discover`/`collect`）；`tools`/`inspection_templates` 是描述符侧能力，出现在 bundle 里会被注册拒绝**：
   ```go
   type myToolExecutor struct{}

   func (myToolExecutor) ExecuteTool(ctx context.Context, call *plugins.Call, request plugins.ToolRequest) (*plugins.ToolResult, error) {
       // call.Settings=已校验设置；call.Secrets.Resolve(ref) 逐引用解析凭据
       ...
   }

   bundle := plugins.ExecutionBundle{
       PluginID:     descriptor.ID,
       Location:     plugins.LocationPlinthSupervisor,
       Capabilities: []plugins.Capability{plugins.CapabilityExecuteTool}, // 精确=非nil接口集合
       ToolExecutor: myToolExecutor{},
   }
   if err := worker.RegisterPluginExecutor(descriptor, bundle); err != nil { /* wiring 失败 */ }
   ```
   注册即验证（归属/`execute_tool` 且 ToolExecutor 非 nil/位置=plinth_supervisor/与编译实现逐字段一致/参数 schema 规范化字节相等）；`plugins.Call`（冻结设置 + grant 支撑的秘密解析）由宿主 `worker.SetPluginCallHost` 在接线期提供，秘密只在 supervisor 进程解析，worker 子进程永不持凭据。`Run` 冻结注册表，未注册工具显式失败。
5. **代际策略**：工具按 `ExecutionLocation` 自动进入接受的 agent 代际目录（initial-analysis：worker_local|plinth_supervisor；investigation 另接受 lintel）。
6. **授权（必做，不可省略）**：新工具必须在 Quoin 控制面显式实现并接线 `attempt.Service` 的 `ToolGrantResolver`/`ToolGrantValidator`（来源/业务路由、归一化执行入参、TOCTOU 检查——可恢复歧义返回 PreflightCode，如 `target_ambiguous`；连接禁用/轮换在 FulfillGrant 事务内拒绝）。没有 resolver 的工具调用会 fail 整个 model call，这是设计行为。
7. **巡检模板（可选）**：`Descriptor.InspectionTemplates` 声明版本化模板，(ID, Version) 是 Run 冻结身份；`Collector.Collect` 执行确定性采证——`CollectRequest.Params` 携带冻结模板参数（metrics 插件为 PromQL 表达式与窗口秒数）、`EvidenceAt` 为观测点、`Targets` 为冻结目标集；采证成功不等于业务健康。巡检运行经 `ExecutionBundle.Collector` 真实消费该契约；宣称 `inspection_templates`/`collect` 前必须先绑定 Collector。

模板参数示例（冻结进 Run 的 `CollectRequest.Params`）：

```json
{"expression": "up{job=\"api\"}", "rangeSeconds": 3600, "stepSeconds": 60}
```

## 3. 冻结语义（为什么声明必须与实现一处演进）

每次 attempt 创建时冻结完整目录文档（完整参数 schema、结果 Schema 引用 digest、说明、失败模式），内嵌进快照摘要覆盖的规范输入并存 `attempt_input_snapshots.tool_catalog_json`：

- 事后修改 ToolDef 不改变已创建 attempt 的可见契约——历史 Model Call 字节可解释。
- 新 attempt 自动获得新目录；被停用插件的工具不进入新目录；授权时与已安装执行器逐字段兼容校验（参数按规范化 JSON 字节、结果 Schema 按 digest），漂移即显式拒绝。
- Worker 从输入快照内嵌目录渲染 provider schema，与 Quoin 共享同一 `Digest()`；`BeginModelCall` 校验不一致即拒绝。

## 4. 探测与观测

- `Prober`（`Probe(ctx, *Call, ProbeRequest) (*ProbeResult, error)`）与 `Discoverer`（`Discover(ctx, *Call, DiscoverRequest) (*DiscoverResult, error)`）是插件执行绑定接口：绑定后描述符才可宣称 `probe`/`discover` 能力。Discovery 报告不完整性（`Incomplete`），从不伪造缺失；observation identity =（来源接入，对象类型，规范来源身份），跨来源同名对象不合并。

## 5. 检查清单

- [ ] `plugins.Descriptor` 通过全部静态校验（ID/版本/能力一致性/封闭 ConfigSchema；秘密槽位由后端生成引用）
- [ ] 工具名全局唯一；描述符工具字段用 `attempt.DescriptorTool` 从实现派生，无手抄副本
- [ ] 实现经 `attempt.RegisterImplementation` 注册；执行绑定能力集合与非 nil 接口精确相等（描述性能力不进 bundle）
- [ ] `enabledPlugins` 白名单已含新插件（部署 YAML）
- [ ] 控制面 grant resolver/validator 已为该工具显式实现并接线
- [ ] 契约测试与集成测试更新，旧测试清理
