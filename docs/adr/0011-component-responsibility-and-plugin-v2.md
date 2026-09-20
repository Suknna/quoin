# ADR 0011: 三组件职责规范与插件体系 v2（网关边界）

- 状态：已接受
- 日期：2026-09-20
- 范围：runtime.proto、internal/plugins（SDK 重构）、internal/plugins/builtin、quoin/plinth/stele 三组件、deployment-config schema、部署物（Stele 状态卷）、CONTEXT/specs 文档
- 取代：CONTEXT「Stele」条目的同步转交条款与 ADR-0004/0007 的能力/装配细节（其"声明派生自实现"不变式保留）

## 背景

此前三组件的职责边界有两条遗留裂缝：

1. **外部平台交互散落两处**：告警入向由 Stele 同步转交（Stele 无任何本地状态，Quoin 落库后才
   ACK），而指标工具（thanos_query）、连接探测、来源观测与巡检采集作为 attempt 在 **Plinth
   supervisor** 内直接访问外部平台。Plinth 因此必须感知插件体系（ExecutionBundle、
   Discoverer/Collector/ToolExecutor 绑定、metrics 凭据 grant），换模型运行时或扩副本都要牵扯
   插件执行宿主。
2. **插件装配是显式函数调用**（ADR-0007 的取舍）：描述符目录、实现表与派发分表在多个宿主分别
   显式装配，能力词表（probe/discover/tools/execute_tool/inspection_templates/collect）与
   执行位置词表（worker_local/plinth_supervisor/quoin）表达了"谁执行"而非"谁负责"。

本次把职责按"控制面 / 网关 / 推理"重切，为后续按平台扩展插件体系铺平地基。

## 决策

### 1. 组件职责定稿

```
平台A ──┐                                        ┌── 工具调用（出向）
平台B ──┼──▶ Stele Webhook ──▶ Quoin 核心 ──▶ Plinth Agent 运行时
平台C ──┘    （入向网关）      （调度中枢）      （模型推理）
                                 │
                            插件注册中心
                           （工具执行也走这里）
```

- **Quoin（调度中枢）**：插件注册中心；启动时聚合所有 ToolProvider 的工具形成工具目录；消费
  Stele 队列中的归一化 Event 路由进业务域并派发任务给 Plinth（RuntimeControl gRPC 流，机制不
  变）；收到 tool_call → 按名查 Tool → 权限/审计/派发 → Handler 执行 → 结果封存（tool_calls
  终态 + Evidence + Artifact）→ 经新的 `ExternalToolResult` 帧推回 Plinth。业务库（SQLite）
  唯一写者：任务、会话、租户、权限、审计、调度。**对外部平台凭据只写不读**：存储加密信封，仅为
  Stele 的按需 Acquire 解密投递，自身从不使用。
- **Stele（外部网关）**：所有外部平台交互的唯一出入口。入向 `POST /webhook/{source}` → 找到
  EventSource 插件 → VerifyAndParse → 归一化 Event 写入**本地 SQLite 队列** → 立即 ACK；出向
  经长期网关流（`SteleRelay.Connect`）接收 Quoin 下发的 `ExecutePlatformCall`：解析连接材料
  （按需 `AcquireConnectionCredential` 并缓存）、按连接限流、执行 HTTP、回传原始响应。**只做
  传输与协议转换，永远不碰业务逻辑**；Stele 的判断只有三件事：签名对不对、配额超没超、平台通
  不通。Own 本地 SQLite：事件队列、去重键、限流计数、token 缓存、死信、（预留）拉取游标。动态
  凭证的生命周期管理归 Stele（v1 只实现 bearer-static，token 刷新留接口）。保持独立二进制与
  mTLS CN=stele 身份（ADR-0009 不变），部署新增状态卷。
- **Plinth（推理沙箱）**：对插件体系**零感知**——`cmd/plinth` 不再 import 插件包与 Quoin 包
  （编译级保证）。协议只剩：「任务 + 工具目录 → 文本或 tool_call」。worker 沙箱本地工具
  （bash/read/write/grep）不变；quoin_routed 工具（含 artifact_read/artifact_grep）由 Quoin
  执行后推结果，supervisor 只转发；模型供应商调用保持 supervisor 直连（FetchCredentialGrant
  收窄为仅 model_provider）。无状态：会话与结果归 Quoin，沙箱临时文件易失。

### 2. 入向 ACK 语义反转（显式取代条款）

CONTEXT「Stele」条目原规定："Quoin 只有在一个 SQLite 事务中保存 Delivery……后，Stele 才向
Alertmanager 返回 204；_Avoid_: 先返回 2xx 再异步持久化"。本 ADR **显式取代**该条款：

- Stele 在**本地事务持久入队后**即返回 `202 Accepted`；可靠性由队列重试 + 死信承担。
- Quoin 侧业务拒绝（凭据吊销、指纹冲突、身份冲突）发生在 ACK 之后，以既有 intake_issues 机制
  记录；传输失败由 Stele 指数退避重试，超限/超龄进入死信并保留原文可重放。
- 每个事件以 Stele 生成的全局唯一 `event_id` 幂等（沿袭 relay_id 的 UNIQUE 裁决语义）。

### 3. 插件体系 v2：接口 + 注册表 + 空白导入 + 泛型工具

- **不用** Go 标准库 `plugin` 包（无 dlopen、无动态安装，延续 ADR-0004）；采用经典"接口 + 注
  册表 + 空白导入"模式：每个插件文件 `init()` 调 `plugins.Register(Plugin{...})`，宿主
  （cmd/quoin、cmd/stele）空白导入 `internal/plugins/builtin` 即完成装配；注册表首次读取即冻
  结，重复 ID/词表外名称/共享工具契约分歧一律启动期 panic（fail-fast 不变）。
- 能力收敛为两类：**EventSource**（入向：`Kind()` + `VerifyAndParse` → 归一化 Event）与
  **ToolProvider**（出向：`Tools()`）。六类 Capability、ExecutionBundle、
  Prober/Discoverer/ToolExecutor/Collector 接口与执行位置词表整体退役；探测/发现/采集改为插件
  的**内部工具**（`Internal:true`，不进模型目录），由 Quoin 调度器直接经 Stele 调用——所有外部
  交互收敛到同一条执行路径。
- **泛型工具**：`Tool[A, R]` 类型安全实现，manifest（描述 + 参数 JSON Schema）从 A 的
  struct tag 反射派生（`json` 定属性、无 `omitempty` 即必填、`doc` 补描述，可显式覆盖），
  保留 ADR-0007 "声明派生自实现、不可漂移"的不变式；JSON 只在边界编解码一次，Handler 内部全类
  型安全。模型可见的只有描述与参数 schema。
- Handler 的平台 I/O 只经注入的 `PlatformCaller`（method + 相对路径；scheme/host 永不出现，
  SSRF 面由已配置连接收敛）；凭据、TLS 与限流对 Handler 不可见。共享契约工具（prometheus 与
  thanos 的 PromQL 查询）按 manifest 全等去重，目录单条目、溯源列全部启用的贡献者（不变）。

### 4. 契约与迁移要点

- runtime.proto：SteleRelay 新增 `Connect`（网关双向流）、`DeliverEvents`（批量幂等转交）、
  `AcquireConnectionCredential`（按需连接材料）；`Deliver` 退役。RuntimeControl 新增
  server→client 帧 `ExternalToolResult`；`ToolExecutionMode` 的 SUPERVISOR_TYPED 退役，新增
  `QUOIN_ROUTED`。契约指纹更新，三镜像同批替换。
- 工具版本：thanos_query v3→v4、artifact_read/grep v1→v2（执行位置变更；在途旧 attempt 的冻结
  目录会因 drift 被拒绝授权，升级窗口内不保留跨版本在途执行）。
- alertmanager 插件从纯描述改为 EventSource（协议解析前移到边缘）；归一化事件 payload 保持
  Alertmanager webhook 投影形状，Quoin 摄取事务（occurrence 状态机、业务归因、intake issues）
  不变。
- SteleConfig 新增 `dataDirectory`；compose/k8s 新增 stele 状态卷（1Gi）。
- 业务库仍为 SQLite；PostgreSQL/MySQL 迁移明确不在本 ADR 范围。

## 后果

- Plinth 大幅瘦身（删除插件执行、探测、观测、巡检采集 runner 与 metrics 凭据路径），换模型运
  行时、扩副本不再牵扯插件；新增平台插件不再改动任何核心表或 Plinth。
- 每次出向工具调用多两跳（Quoin→Stele→平台），换取：Quoin 的统一权限/审计/派发点、Plinth 零
  凭据（模型 key 除外）、凭据只写不读与动态凭证生命周期的单一归属。
- 告警入向从"同步落库后 ACK"变为"本地入队即 ACK"：外部重试压力前置到 Stele 队列，Quoin 短暂
  不可用不再向 Alertmanager 回 5xx；代价是 Quoin 侧业务拒绝不再影响外部 HTTP 状态（改为 intake
  issue 记录）。
- ADR-0004/0007 中与本次冲突的细节（能力词表、执行位置词表、显式装配方式、Retired 描述符条款）
  以本 ADR 为准；其冻结目录、双向校验、声明派生等核心不变式全部保留。
