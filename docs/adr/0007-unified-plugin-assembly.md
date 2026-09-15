---
status: accepted
---

# 单一插件装配：目录、实现查找与执行分发表同源派生

## Context

ADR 0004 确立了插件注册机制（Descriptor / ExecutionBundle），但实施中并存了多条工具来源路径：`attempt` 核心硬编码插件工具表（thanos_query、kubernetes_read 与平台工具混在同一编译表）、无消费者的 `RegisterImplementation` 实现注册点、worker `init()` 旁路注册的插件执行器，以及 `RegisterPluginExecutor`/`SetPluginCallHost` 这条从未接线的 ExecutionBundle 缝。同一工具契约因此存在四份可漂移的表述，"声明不能伪装不存在的实现"只靠人工纪律维持。同时受控浏览器与 Kubernetes 插件已退役，其编译实现必须继续服务历史冻结目录，却不允许重新通告或启用。

## Decision

**（2026-09-14 确认并实施）只保留一套 `internal/plugins` Registry/Descriptor/ExecutionBundle 机制，一切工具消费方由同一次装配结果派生。**

- **插件自持声明与实现**。每个插件在自己的包里同时拥有 Descriptor 与编译 ToolDef；描述符的工具声明由实现派生（`ToolDef.DescriptorTool()`），声明与实现在构造上不可漂移。`internal/plugins/builtin` 是内建插件的唯一声明源；平台工具（bash/read/write/grep/artifact_read/artifact_grep）仍归 attempt 核心，插件永不重定义。
- **统一装配**。`attempt.BuildCatalogs(registry, implementations, enabled)` 从一个 registry 加实现表产出 `Catalogs{各代冻结目录, 实现索引}`，并双向校验：每个注册描述符的工具必须在实现表中逐字段一致；每个非平台实现必须有注册声明。执行宿主的分发表（`worker.AssembleTypedExecutors`）从同一 registry 与实现表派生：平台执行器由宿主注册，插件工具经其 `ExecutionBundle.ToolExecutor` 绑定注册（授予解析经 `Runner.CallFor` 缝），装配即冻结。装配失败是启动失败，不存在运行时补注册。
- **退役即声明权威**。退役插件（browser、kubernetes）的描述符以 `Retired` 标志留在同一注册机制中：仍是其编译实现的声明权威，使历史冻结目录可按工具名+版本+契约解析实现；但 `ResolveEnabled` 像拒绝未知 ID 一样拒绝退役 ID，管理目录不再通告，任何新生成的冻结目录都不含其工具。kubernetes_read 的执行适配器作为退役绑定留在装配表中，仅服务退役前冻结的历史在途尝试。
- **历史是快照不是注册表**。`tool_catalog_json` 授权按工具名+版本+契约与装配实现匹配，漂移显式拒绝、不自动升级或扩权。NULL 旧目录的回退是字面冻结的历史文档（独立常量，钉住测试守卫），与当前启用集合和实现完全解耦。
- **删除被替代路径**：`RegisterImplementation`/`implementationRegistry`、attempt 内硬编码插件工具表、`init()` 旁路执行器注册及未接线的旧缝全部移除；不保留兼容层。历史数据（冻结目录、Evidence、连接）不动。

## Consequences

- 新增插件 = 新增一个插件包（声明+实现）+ 执行宿主绑定；不改任何核心表。
- 共享契约工具（prometheus/thanos 的 PromQL 查询）在目录中仍只有一个条目，溯源列全部启用的贡献提供者，授权按实际来源连接解析。
- ToolDef 类型随契约下沉到 `internal/plugins`，attempt 以类型别名保持既有词汇。
- 工具契约的可见失败码（如 `grant_missing`）经类型化错误穿越 Call 缝，保持既有线上语义。

## Verification

以测试为准：装配唯一来源与双向一致性、声明漂移/无主实现/重复实现拒绝、启用过滤与退役不通告、分发表形状与装配后冻结、共享工具去重、历史冻结匹配与版本漂移显式拒绝、NULL 回退固定（`internal/quoin/attempt/assembly_test.go`、`legacy_catalog_test.go`、`internal/plinth/worker/typedexecutors_assembly_test.go`、`internal/plugins` 注册边界测试）。接口规范见 `docs/specs/quoin-v1/plugins.md` 与 `docs/plugin-development.md`。
