# 资源纳管与发现：Quoin 现状、体验与 Keep 对照研究

- **调研日期**：2026-09-10
- **Quoin 基线**：`d4d7d0e66a9d0891812e32e9bf138e529df69cf6`
- **Keep 基线**：`118b2dc0c7a45f8a22b6317b983cdba6f5b54b5e`
- **方法与边界**：静态阅读 Quoin 前后端、数据模型、规格、测试与 Keep 源码/官方文档；**未启动服务、未执行测试、未连接任何外部系统**。因此“已实现”仅指源码中存在的实现与测试，不等于部署环境已验证；尤其不以 MSW 离线预览或遗留 E2E 当作真实后端验收。
## 1. 执行摘要

1. Quoin 已有真实的“资源刷新”后端闭环：Admin 触发或调度创建 `resource_refresh_run`，为每条 YAML discovery 冻结已发布配置、Label Contract 与 Thanos grant，交由 Plinth 查询，结果收敛为 `observed_resources` 与不可变刷新日志。它不是 mock 或纯规格设计。
   - 证据：`/home/suknna/code/quoin/internal/quoin/businesssystem/resource_refresh.go:44-171`
   - 证据：`/home/suknna/code/quoin/internal/quoin/businesssystem/resource_refresh_result.go:33-172`

2. Quoin 的“资源”刻意不是 CMDB 资产：Observed Resource 是 **业务系统 YAML 中规则所导出的 Prometheus labels 身份投影**；Kubernetes 连接只服务于调查时的按需只读查询，既不是资源发现源也不是资源长期权威。
   - 证据：`/home/suknna/code/quoin/CONTEXT.md:207-225`
   - 证据：`/home/suknna/code/quoin/docs/specs/quoin-v1/inspection-config.md:29-35`

3. 当前产品体验没有把这个真实闭环讲清楚：业务系统页把“资源”和 Kubernetes 映射塞进同一折叠区；刷新只显示父 Run 状态，当前资源只显示 `current=true` 的首 100 条，无来源规则详情、子 discovery 结果、gap 原因、历史或可分享资源详情。更严重的是 UI 没有按角色隐藏刷新，尽管后端要求 Admin。
   - 证据：`/home/suknna/code/quoin/web/src/features/systems/ui/index.tsx:74-85`
   - 后端权限证据：`/home/suknna/code/quoin/internal/quoin/app/config/resource_refresh.go:11-38`

4. Keep 也不是通用 CMDB 或“扫描所有资产”的发现引擎。Keep 的 Provider 是外部系统连接实例；provider factory 扫描代码目录的行为是插件枚举，而不是发现用户基础设施资产。其拓扑是由支持 `BaseTopologyProvider` 的已安装 Provider 拉取的服务关系投影。
   - 官方概览：[Service Topology](https://docs.keephq.dev/overview/servicetopology)
   - 固定源码：[topology provider 基类](https://github.com/keephq/keep/blob/118b2dc0c7a45f8a22b6317b983cdba6f5b54b5e/keep/providers/base/base_provider.py#L928-L930)

5. 用户对“Keep 更容易接入且更快看见结果”的直觉有依据，但不能推导为 Keep 的发现能力更强。Keep 把 Provider tile → 凭据 Connect → 安装 → 可选拓扑 Pull 的路径做成连贯交互；Quoin 把连接、YAML discovery 规则、发布、刷新和资源结果分散在多个概念和页面层次。

6. 近程改进应复用 Quoin 的 `Connection + YAML discovery + refresh run + runtime` 现有模型，补齐“接入—规则—结果”连续交互、预览、来源、详情和刷新可见性；YAML 仍声明资源源。若改为“先全局发现、后划业务归属”，则是明确的领域模型变更，不能伪装成导航或菜单调整。
## 2. 概念对照：哪些名词可比，哪些不能混同

| 维度 | Quoin 当前定义 | Keep 当前定义/行为 | 结论 |
| --- | --- | --- | --- |
| 连接 | Thanos、Kubernetes、Model Provider 的稳定访问边界；配置 revision 与秘密 generation 分离 | Provider 是安装后的外部系统连接实例 | 可比：都是接入边界，不是资源本身 |
| 资源/服务 | Observed Resource 是 Prometheus labels 导出的当前投影 | Topology Service/Application/Dependency 是服务拓扑模型 | 不可直接等同；Keep 服务不是 Quoin 的资源资产 |
| 发现 | 已发布 YAML 中显式声明 selector，Plinth 执行 instant query | Provider 能力驱动的拓扑 pull；如 Cilium 从 Hubble flow 推断服务依赖 | 都是“从外部事实生成投影”，输入与目标不同 |
| 候选/审批 | `Knowledge Candidate` 是知识沉淀候选，不是资源候选 | 手工/导入拓扑有不同编辑权限，不是通用资源审批 | 不应把知识候选误称为资源纳管流程 |
| 同步/删除 | 完整 refresh 把未见旧资源标为 `current=false`，保留历史行 | topology pull 先清旧 provider 相关服务/依赖后写新；provider 删除是否级联未证实 | 生命周期取舍相反：Quoin 保留资源事实，Keep 的导入拓扑按源替换 |

Quoin 的 Connection 定义及其 revision/generation、启停影响由领域文档直接规定：
`/home/suknna/code/quoin/CONTEXT.md:219-225`。

Observed Resource 的稳定身份是 `BusinessSystem + discovery key + 排序后的 identity labels`；完整成功才可把本轮未出现的对象置为当前未观测，不完整刷新保留旧状态：
`/home/suknna/code/quoin/CONTEXT.md:211-213`。
## 3. Quoin：真实后端实现

### 3.1 创建与接入

- Business System 不是独立表单创建的资产对象。首次上传静态合法 YAML，才会原子创建 Disabled system 与不可变草稿；只有发布才切换当前配置和 `enabled`。
  - ` /home/suknna/code/quoin/CONTEXT.md:207-209`
  - `/home/suknna/code/quoin/internal/quoin/businesssystem/service.go:79-299`
- Resource discovery 由 YAML 的 `resource_discoveries` 声明：稳定 key、instant VectorSelector、identity labels 和统一 refresh interval；运行时读取持久化类型投影，不重新解释 YAML。
  - `/home/suknna/code/quoin/docs/specs/quoin-v1/inspection-config.md:20-35`
  - `/home/suknna/code/quoin/internal/gen/contracts/schema.sql:618-626`
- Connection 创建、读取、probe、enable、disable、rotate 有真实 HTTP 路由与后端服务；类型封闭为 Thanos、Kubernetes、Model Provider。
  - `/home/suknna/code/quoin/internal/quoin/app/connections.go:148-355`
  - `/home/suknna/code/quoin/internal/gen/contracts/schema.sql:710-719`

### 3.2 扫描/刷新与调度

手工刷新 `POST /business-systems/{systemKey}/resources:refresh` 要求 Admin。服务端以命令幂等键重放，冻结当前已发布 config/contract，拒绝同一系统并行 active run；每个 discovery 创建一个 `inspection_collection` Attempt 和一个 `config_thanos_query` grant。

- HTTP 与权限：`/home/suknna/code/quoin/internal/quoin/app/config/resource_refresh.go:11-38`
- Run 创建和冻结：`/home/suknna/code/quoin/internal/quoin/businesssystem/resource_refresh.go:44-171`
- API 注册：`/home/suknna/code/quoin/internal/quoin/app/config/http.go:52-72`
- Run 状态/唯一 active fence：`/home/suknna/code/quoin/internal/gen/contracts/schema.sql:1284-1307`

正常运行时，Quoin 启动 resource-refresh scheduler；每分钟检查 enabled 且已发布的系统，到期并且没有 active run 才建 scheduled run。维护模式不创建、不补跑。Plinth 控制流重新连接后也会扫描 queued refresh 并派发。

- 调度规则：`/home/suknna/code/quoin/internal/quoin/businesssystem/resource_refresh_scheduler.go:10-126`
- runtime 派发和每分钟 scheduler：`/home/suknna/code/quoin/internal/quoin/app/config_verification_runtime.go:88-173`
- 启动 wiring：`/home/suknna/code/quoin/internal/quoin/app/app.go:367-426`
- Plinth reconnect 派发：`/home/suknna/code/quoin/internal/quoin/app/runtime_service.go:302-315`

### 3.3 结果、当前性与“删除”

Plinth 的 proposal 经 Quoin 校验后处理。成功时，先将同系统/同 discovery 既有 `current=1` 的资源置为 `0`，再按 identity upsert 本轮 series；gap/error 不清除旧 current 资源。每条 discovery 的结果都记入 append-only `observed_refresh_log`，父 Run 汇聚为 `Completed`、`CompletedWithWarnings` 或 `Failed`。

- 结果提交：`/home/suknna/code/quoin/internal/quoin/businesssystem/resource_refresh_result.go:33-172`
- 列表的新鲜度是 `last_successful_refresh_at + interval` 的派生值，不是可独立写入的真相：`/home/suknna/code/quoin/internal/quoin/businesssystem/observed_resources.go:26-103`
- 资源表与 refresh log：`/home/suknna/code/quoin/internal/gen/contracts/schema.sql:663-704`
- 历史资源禁止 DELETE：`/home/suknna/code/quoin/internal/gen/contracts/schema.sql:3383-3384`

因此 Quoin 没有“审批候选资源后入库”或“删除资产”的流程。这里的“非当前”是一次完整观测的事实，不是删除；“陈旧”表示刷新时间超过配置 interval。资源候选/确认概念只属于 Knowledge Candidate：
`/home/suknna/code/quoin/CONTEXT.md:285-295`。

### 3.4 取消、故障与自动恢复

自动恢复的边界是 Attempt/Runtime 调和，而不是背后悄悄重跑扫描。新 boot 或 lease 丢失会中断旧 Attempt；resource-refresh child 随后写一条技术 gap，父 Run 收敛为 `Interrupted` 或 `Cancelled`。成功结果与取消按数据库提交顺序裁决。

- 运行时丢失调和：`/home/suknna/code/quoin/internal/quoin/app/runtime_reconcile.go:70-174`
- 取消收敛：`/home/suknna/code/quoin/internal/quoin/app/runtime_reconcile.go:215-292`
- refresh 技术 gap/父 Run 收敛：`/home/suknna/code/quoin/internal/quoin/businesssystem/resource_refresh_convergence.go:9-78`
- 后端覆盖：`/home/suknna/code/quoin/internal/quoin/businesssystem/resource_refresh_test.go:24-228`
## 4. Quoin：当前导航与体验审查

### 4.1 入口导航

当前窄 rail 只有“告警中心、AI SRE、巡检、业务系统”；知识被归到 AI SRE 二级导航，连接藏在 Admin。对 Admin 而言，连接作为接入前置条件的可发现性偏低；对 Operator，资源读取与刷新边界也不够明显。

- 模块定义与归属：`/home/suknna/code/quoin/web/src/app/WorkspaceShell.tsx:42-57`
- rail、模块切换与 Admin 入口：`/home/suknna/code/quoin/web/src/app/WorkspaceShell.tsx:95-107`

这与规格的六模块/三栏、URL 选中对象、不自动选首项并不完全一致：
`/home/suknna/code/quoin/docs/specs/quoin-v1/frontend.md:23-44`。

### 4.2 业务系统页的实际行为

- `useSystemsModule` 读取列表后，若尚无 selected 会自动读取第一项，不以 URL 作为选中资源的权威。
- 列表只显示名称和“已发布/未启用”，没有资源新鲜度、Browser Identity 或待处理状态。
- 系统详情把“资源与 Kubernetes 映射”合并；当前资源请求固定 `current=true&limit=100`，没有加载更多、非当前过滤或资源详情入口。
- “刷新资源”按钮只依据 maintenance/suspended 禁用，未检查 `user.role`；后端却明确只允许 Admin。
- 刷新成功后前端每 1.5 秒轮询父 Run；页面不呈现 discovery 子 Attempt、warnings/gap、`resultDetail`、取消能力或来源规则说明。

- 页面实现：`/home/suknna/code/quoin/web/src/features/systems/ui/index.tsx:74-85`
- 前端 API 的 current-only 与单页限制：`/home/suknna/code/quoin/web/src/features/admin/business-systems/api.ts:207-236`
- 服务端有 list/get Observed Resource 的真实端点：`/home/suknna/code/quoin/internal/quoin/app/config/resource_refresh.go:65-142`

规格反而已明确要求：Admin-only “立即刷新”、离开后可返回、真实终态轮询，及当前观测/当前未观测/陈旧三态和资源全页详情：
`/home/suknna/code/quoin/docs/specs/quoin-v1/frontend.md:130-142`。

结论是 **后端能力和规格意图均比现行系统页体验完整**；主要缺口是正式前端投影没有兑现，而不只是后端缺少扫描。

### 4.3 Connection 与 Model Provider 体验

连接管理页面真实调用后端，按 Thanos/Kubernetes/Model Provider 分组，支持 probe、启停、轮换、revision/generation 和 probe 历史。Model Provider 的“发现模型”只调用 `/v1/models` 辅助填写，API key 不缓存、不回显；发现失败仍允许手填，真正 enable 对应 passed probe。

- 连接操作 UI：`/home/suknna/code/quoin/web/src/features/admin/ui/ConnectionsModule.tsx:141-178`
- 模型发现/手填语义：`/home/suknna/code/quoin/web/src/features/admin/ui/ModelProviderEditor.tsx:140-196`
- 后端 discovery 的 request-memory 边界：`/home/suknna/code/quoin/internal/quoin/app/connections.go:674-725`
- 规格要求：`/home/suknna/code/quoin/docs/specs/quoin-v1/frontend.md:146-151`

这是现有可复用的“接入 → 探测 → 启用 → 历史”交互骨架；资源 discovery 不应另造平行的连接模型。
## 5. 离线预览、测试与真实功能的边界

默认 `pnpm --dir web dev` 启动 MSW，右下角显示“开发预览 · 模拟数据”，有管理员、操作员、空数据、慢响应和冲突等情境；dev mock 不会访问真实系统。生产 build 不包含 mock worker/panel。`test:e2e:real` 需要显式提供真实目标，legacy 完整旅程默认不收集。

- 开发/真实/测试边界：`/home/suknna/code/quoin/docs/web-development.md:3-52`
- mock bootstrap：`/home/suknna/code/quoin/web/src/main.tsx:7-32`
- preview 面板：`/home/suknna/code/quoin/web/src/mocks/PreviewPanel.tsx:31-63`
- mock E2E：`/home/suknna/code/quoin/web/e2e/mock/coverage.spec.ts:33-148`
- real E2E 默认忽略 legacy：`/home/suknna/code/quoin/web/playwright.real.config.ts:3-17`

资源刷新有扎实的后端状态机测试，但前端系统模块单测主要覆盖 YAML 上传、发布和 Browser Identity，没有刷新 UI 测试。遗留真实 E2E 中有 resource refresh 场景，但其旧 selector/文案与当前 UI 实现已漂移，且默认不运行。

- 后端测试：`/home/suknna/code/quoin/internal/quoin/businesssystem/resource_refresh_test.go:24-228`
- 前端测试范围：`/home/suknna/code/quoin/web/src/features/systems/ui/index.test.tsx:33-85`
- 遗留 refresh E2E：`/home/suknna/code/quoin/web/e2e/real/legacy/admin-business-systems.spec.ts:148-293`

## 6. Keep 对照研究（静态源码与官方文档）

### 6.1 Provider 接入不是资产发现

Keep 的 Providers 页面从 tile 进入 Connect 抽屉，填写 provider 表单，再调用 `POST /providers/install` 安装实例。这给用户一个明确的接入起点；但 provider factory 对代码目录的扫描是插件/实现枚举，不能解释成对外部资源的自动扫描。

- Provider tiles：[`providers-tiles.tsx:50-55,110-124`](https://github.com/keephq/keep/blob/118b2dc0c7a45f8a22b6317b983cdba6f5b54b5e/keep-ui/app/(keep)/providers/providers-tiles.tsx#L50-L55)
- 表单提交：[`provider-form.tsx:416-434`](https://github.com/keephq/keep/blob/118b2dc0c7a45f8a22b6317b983cdba6f5b54b5e/keep-ui/app/(keep)/providers/provider-form.tsx#L416-L434)
- 安装路由：[`providers.py:470-513`](https://github.com/keephq/keep/blob/118b2dc0c7a45f8a22b6317b983cdba6f5b54b5e/keep/api/routes/providers.py#L470-L513)

### 6.2 拓扑发现的入口、能力与范围

Topology 空态会引导用户连接支持 topology label 的 Provider；独立 Service Topology 页提供 “Pull from providers”。`POST /topology/pull` 只对已安装且实现 `BaseTopologyProvider` 的 Provider 生效，不表示安装后立即扫描。

- 空态入口：[`topology-map.tsx:679-710`](https://github.com/keephq/keep/blob/118b2dc0c7a45f8a22b6317b983cdba6f5b54b5e/keep-ui/app/(keep)/topology/ui/map/topology-map.tsx#L679-L710)
- Pull 入口：[`topology-client.tsx:32-71`](https://github.com/keephq/keep/blob/118b2dc0c7a45f8a22b6317b983cdba6f5b54b5e/keep-ui/app/(keep)/topology/topology-client.tsx#L32-L71)
- Pull 路由：[`topology.py:157-210`](https://github.com/keephq/keep/blob/118b2dc0c7a45f8a22b6317b983cdba6f5b54b5e/keep/api/routes/topology.py#L157-L210)
- capability 基类：[`base_provider.py:928-930`](https://github.com/keephq/keep/blob/118b2dc0c7a45f8a22b6317b983cdba6f5b54b5e/keep/providers/base/base_provider.py#L928-L930)

Cilium 的实现也说明边界：它读取最近 1000 条 Hubble flow，归纳服务间依赖；这不是 Kubernetes 资源清单的全量同步。

- Cilium 查询/变换：[`cilium_provider.py:96-157,207-301`](https://github.com/keephq/keep/blob/118b2dc0c7a45f8a22b6317b983cdba6f5b54b5e/keep/providers/cilium_provider/cilium_provider.py#L96-L157)

官方说明：

- [Keep Service Topology](https://docs.keephq.dev/overview/servicetopology)
- [How Keep gets alerts](https://docs.keephq.dev/overview/howdoeskeepgetmyalerts)
- [Cilium Provider](https://docs.keephq.dev/providers/documentation/cilium-provider)

### 6.3 Keep 拓扑的数据与同步语义

Keep 的核心模型为 TopologyService、TopologyApplication、TopologyDependency，并以 tenant + service + environment + source_provider_id 约束源身份。拓扑同步先删除该 provider 的旧服务依赖并提交，再写入新集合；这是源投影替换，并不提供 Quoin 这种“未观测但保留”的资源历史语义。

- 数据模型：[`topology.py:10-113`](https://github.com/keephq/keep/blob/118b2dc0c7a45f8a22b6317b983cdba6f5b54b5e/keep/api/models/db/topology.py#L10-L113)
- 同步替换：[`process_topology_task.py:22-85,90-163`](https://github.com/keephq/keep/blob/118b2dc0c7a45f8a22b6317b983cdba6f5b54b5e/keep/api/tasks/process_topology_task.py#L22-L85)
- 手工拓扑可编辑、导入服务限制手工修改：[`topologies_service.py:448-546`](https://github.com/keephq/keep/blob/118b2dc0c7a45f8a22b6317b983cdba6f5b54b5e/keep/topologies/topologies_service.py#L448-L546)

Provider 通用删除路径中未见统一 topology 清理调用；基于静态阅读，**不能宣称**删除 provider 一定会级联清理其 topology。

- 删除路径：[`providers_service.py:317-369`](https://github.com/keephq/keep/blob/118b2dc0c7a45f8a22b6317b983cdba6f5b54b5e/keep/providers/providers_service.py#L317-L369)

### 6.4 Keep 的拉取频率与恢复含义

Keep 可由 `/alerts/query` 后台任务触发拉取；默认 provider 拉取间隔是 10080 分钟。安装时可按 capability 启用 pulling，但没有证据表明“安装即立刻完整扫描”，也不能把该机制说成独立的连续资产 discovery scheduler。

- alerts query：[`alerts.py:195-214`](https://github.com/keephq/keep/blob/118b2dc0c7a45f8a22b6317b983cdba6f5b54b5e/keep/api/routes/alerts.py#L195-L214)
- preset/能力配置：[`preset.py:48-108`](https://github.com/keephq/keep/blob/118b2dc0c7a45f8a22b6317b983cdba6f5b54b5e/keep/api/routes/preset.py#L48-L108)
- 默认间隔：[`consts.py:9-11`](https://github.com/keephq/keep/blob/118b2dc0c7a45f8a22b6317b983cdba6f5b54b5e/keep/consts.py#L9-L11)

## 7. 关键错位与建议

### 7.1 用户逻辑为何会感到错位

用户通常按“我先接一个数据源，再选择/看到它发现的对象，最后决定如何纳管”的心理顺序行动。Quoin 实际顺序是“先写并发布业务系统 YAML，再由其中声明的规则驱动 Thanos 刷新”；这个配置优先的领域选择是有效的，但 UI 没有用连续步骤、来源说明、结果详情来解释它。因此用户会把“看不到资源发现入口/结果”误判为“没有发现能力”。

### 7.2 近程建议：不变更领域模型

1. 在业务系统详情明确呈现一条受控路径：**选择已启用 Thanos 连接 → 查看 YAML discovery 规则与 identity labels → 发布版本 → 刷新/等待调度 → 查看本次 Run 和资源结果**。不引入独立资产库。
2. 把资源区从“Kubernetes 映射”中拆出：资源展示 discovery 名称/selector（适当脱敏）、当前/未观测/陈旧、最后成功刷新、Run 状态、warnings/gap 和到资源详情的路由。
3. 兑现既有规格：按 Admin/Operator 隐藏刷新按钮；支持 cursor “加载更多”、`current=false` 浏览和详情/深链；不自动选第一条业务系统。
4. 复用 ConnectionsModule 的真实 probe/history 模式，为 refresh 提供“已受理、子 discovery 进展、终态、失败恢复方式”；不要捏造百分比，也不要创建与 `resource_refresh_runs` 竞争的客户端状态。
5. 将默认 MSW 预览清晰作为演示证据，真实环境验证另列为验收项；修复或替换已漂移的 legacy E2E，不能以其存在声称当前界面已经验证。

### 7.3 远程分叉：需要先做产品/领域决策

若目标变成“跨连接先全局发现，再人工/规则划到 Business System”，需要新增独立的 discovered-object 身份、归属、冲突、保留和权限规则，并重新定义 Thanos/Kubernetes/拓扑来源的权威关系。这会突破 Quoin 当前“资源是 YAML 派生投影、业务系统是用户明确配置范围”的边界，属于领域模型迁移，而不是复制 Keep Provider tiles 或调整导航即可完成的工作。

## 8. 最终判断

- **已实现且真实**：Quoin 的 Connection lifecycle、YAML 规则解析/发布、resource refresh Run、Plinth dispatch、结果收敛、调度、Attempt 取消与恢复。
- **仅离线 preview 可证明**：当前前端默认开发模式下的页面、角色、空数据、慢响应和冲突演示；不能证明真实连接、Thanos、Runtime 或生产认证。
- **设计/规格尚未完全兑现**：三栏 URL 驱动导航、资源详情/深链/全状态列表、Admin-only refresh UI、连续“接入—规则—结果”体验、完整真实浏览器验收。
- **Keep 可借鉴的不是“通用资产发现引擎”**，而是 Provider capability 的显性入口、从接入到结果的连贯引导、来源可见性与导入/手工编辑边界。Quoin 应保留其配置优先和事实保留的资源语义，再改进可理解性与可操作性。
