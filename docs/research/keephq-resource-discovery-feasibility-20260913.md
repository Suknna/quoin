# Keep HQ 资源发现能力借鉴可行性（2026-09-13）

- **调研日期**：2026-09-13
- **Quoin 基线**：`a1773469392b9c2eea59d9108eb43c881e3c1f38`（HEAD）+ 大量未提交工作区修改；Quoin 引用均为当前工作区状态
- **Keep 基线**：**未固定 hash，查询的是 `keephq/keep` 当前 `main`**（`https://github.com/keephq/keep/blob/main/`）；下文 Keep 引用均为该次查询时点快照，不称固定源码基线
- **方法与边界**：纯静态评估，未启动服务、未执行测试、未连接任何运行环境，**不构成验收**。2026-09-10《资源纳管与发现》（`docs/research/resource-governance-and-discovery.md`）保留为历史基线；本文只写增量结论与新证据，不重复其论证与行号。
- **引用约定**：Quoin 事实用 `绝对路径:行号`（当前工作区）；Keep 事实用 `keephq/keep` main 仓库相对路径；无引用来源的陈述不进入本文。
- **一句话结论**：可做。借鉴 Keep 的 provider 能力声明与连贯接入 UX，用窄 Go 接口 + 成熟 SDK 补"机器可读资源源能力描述"与可选关系投影；不搬 Keep Python/Next 整体框架，不做接入即全局扫描。

## 1. 概念对照（防混同，增量于 09-10 研究）

| 概念 | Quoin 语义 | Keep 语义 | 可比性 |
| --- | --- | --- | --- |
| 接入能力 | 本笔记提议新增的"机器可读资源源能力描述"（尚不存在） | provider 能力标记（如 `BaseTopologyProvider`）+ 安装时 mandatory scopes 校验 | 可借鉴思路 |
| 服务关系 | 无独立模型；Observed Resource 是声明派生的资源投影 | TopologyService/Dependency/Application，source_provider_id 进身份键 | 形似质异：投影 vs 事实保留 |
| 拓扑同步 | 完整成功才置旧 current=0，gap 不清扫，禁删 + append-only 日志 | 按 provider 先删再插的源投影替换 | 语义相反，不可互换 |
| 周期采集 | 已实现 scheduler，但与 accepted ADR 冲突（见 3.2） | 无独立 topology scheduler，经 alerts/query 后台受 KEEP_PULL_INTERVAL 门控 | 触发机制不同 |

## 2. 三层范围与判定

| 层 | 内容 | 判定 |
| --- | --- | --- |
| 一 | 既有接入目录/实例管理之上，补统一**机器可读资源源能力描述** | 可做，真实缺口（见 3.1、5.1） |
| 二 | 带**来源 + 观测时间**的服务关系投影，仅作补充读模型 | 可选，须不覆盖 Observed Resource（见 5.2） |
| 三 | 先全局库存后业务归属 / 接入即定时扫描 | 与 ADR 0002/0003 冲突，属新领域决策，不是 UI 调整（见 6） |

## 3. Quoin 事实（当前工作区，逐项引用）

### 3.1 已有真实闭环，不必新建

- 资源发现由声明驱动：`discoveryMetric`、`identityLabels`、`allowedMetrics` 定义于 `/home/suknna/code/quoin/docs/specs/quoin-v1/contracts/schemas/business-system.schema.json:50-58`。
- 刷新启动单事务：根 Run + 子 Attempts + 冻结 grant，`/home/suknna/code/quoin/internal/quoin/businesssystem/resource_refresh_start.go:17-123`；Attempts 处理见 `/home/suknna/code/quoin/internal/quoin/businesssystem/resource_refresh_attempts.go:30-85,113-147`。
- 结果语义：完整成功按 system+discovery 将旧 `current` 置 0 再 upsert，gap 不清扫；observed 资源禁删、append-only 日志，`/home/suknna/code/quoin/internal/quoin/businesssystem/resource_refresh_result.go:84-215`；`/home/suknna/code/quoin/internal/gen/contracts/schema.sql:757-772,2765-2768,3574-3576`。
- 接入目录/连接生命周期/probe 与 Journey 目录均已存在：`/home/suknna/code/quoin/web/src/features/integrations/ui.tsx:29-43,65-86`。
- **真正缺口**：统一的机器可读"资源源能力描述"与关系拓扑模型；不要再推荐新建目录或调度器。

### 3.2 重要冲突发现：周期扫描已实现，但与 accepted ADR 冲突

- 真实周期 scheduler 已存在：1s 轮询、声明间隔、tick 持久幂等键、维护栅栏，`/home/suknna/code/quoin/internal/quoin/app/resource_discovery_scheduler.go:45-58,79-155,160-184`；接线 `/home/suknna/code/quoin/internal/quoin/app/app.go:468`。
- 但 ADR 0002 明确"不再保留……独立周期资源刷新"（`/home/suknna/code/quoin/docs/adr/0002-integration-declarations-and-unified-platform-faults.md:7`）；CONTEXT 同边界（`/home/suknna/code/quoin/CONTEXT.md:214-220`：接入保存/浏览/启用不采集；K8s 仅调查时按需查询）。
- ADR 0003 的单一声明权威**不构成**对恢复周期扫描的自动授权：`/home/suknna/code/quoin/docs/adr/0003-unified-business-system-declaration.md`。
- 处理建议（仅为提案，不擅自更改 ADR）：若保留持续发现，应**显式重新决策**为"已发布 BusinessSystem 范围内的受控周期观测"，不做接入即全局扫描，保留现有权限与历史语义；该决策须走新 ADR。

## 4. Keep 事实（main 快照，逐项引用）

引用根：`https://github.com/keephq/keep/blob/main/`。

### 4.1 能力与接入

- `BaseTopologyProvider.pull_topology` 返回 services + application_relations：`keep/providers/base/base_provider.py`。
- 安装校验 mandatory scopes；secret manager 独立配置、独立于 Provider 行：`keep/providers/providers_service.py`。
- `POST /topology/pull` 只对已安装且支持该能力的 provider 生效：`keep/api/routes/topology.py`。

### 4.2 拓扑模型与同步

- 拓扑模型：TopologyService 唯一键 `tenant_id, service, environment, source_provider_id`（source_provider_id 进身份键，**无跨 provider 可靠身份归一**）；依赖含 `service_id/depends_on_service_id/protocol`；Application 多对多：`keep/api/models/db/topology.py`。
- 同步语义：按 provider **先删再插**，没有 Quoin"未观测但保留"语义：`keep/api/tasks/process_topology_task.py`。
- 拓扑导入不可手工修改（`is_manual`）；无证据支持通用资产库存能力。

### 4.3 调度与许可

- 拉取触发：`pull_data_from_providers` 经 alerts/query 后台任务，受 `KEEP_PULL_INTERVAL`（默认 10080 分钟）门控并顺带拉拓扑；**未发现独立 topology scheduler**（其他触发未穷举，不作绝对否定）：`keep/api/routes/preset.py`、`keep/api/consts.py`。
- 许可：主 LICENSE 为 MIT，`ee/LICENSE` 专有，**不可直接复用**：`LICENSE`、`ee/LICENSE`。

## 5. 借鉴与不借鉴

### 5.1 第一层：能力元数据（借鉴 Keep capability 声明思路）

- Keep 用能力标记（如 `BaseTopologyProvider`）+ 安装时 scopes 校验，让"接入后能做什么"显性化；Quoin 对应缺口是"这个 Thanos/Kubernetes 连接能提供什么资源源"没有统一机器描述。
- 建议形态（提案）：平台目录声明静态能力 + 实例 probe 冻结实测能力，与既有模型供应商 capability/probe 语义对齐；用**窄 Go 接口 + 成熟 SDK** 实现，不移植 Keep Python 运行时，不复制其调度。
- 接入 UX 可借鉴"入口 → 凭据 → 校验 → 结果可见"的连贯性，但沿用 Quoin 工作台投影规范，不搬 Keep Provider tile 页面结构。

### 5.2 第二层：关系拓扑投影（可选，有硬边界）

- 若引入服务关系投影，必须自带**来源**（哪个接入/哪次采集）与**观测时间**；可参考 Keep 的 source_provider_id 进键做法隔离来源，但**不得**采用先删再插的替换语义，**不得**并入或覆盖 Observed Resource 身份（Quoin 保留事实语义见 3.1 引用）。
- 关系投影不解决跨来源身份归一：Keep 自身也以 source_provider_id 隔离来源（见 4.2），归一属新领域问题，不在本项目承诺内。

## 6. 第三层：明确越界项

- "先全局发现库存、后划业务归属"：需新增 discovered-object 身份/归属/冲突/保留/权限规则，突破 ADR 0003 单声明权威与"资源是声明派生投影"边界，须新 ADR，不是导航调整。
- "接入即定时扫描"：与 ADR 0002:7、CONTEXT:214-220 直接冲突。
- Kubernetes 资源发现：超出 CONTEXT 现约定（K8s 仅调查时按需只读查询），同样超本文范围。
- 当前已实现的周期 scheduler 属 3.2 节冲突，须先经显式领域决策定去留，再谈第一/二层实施顺序。

## 7. 边界与未验证

- 本轮全部结论为静态阅读；未运行测试、未连接运行环境，不声称任何验收。
- 09-10 研究中的 UI 具体问题（角色隐藏、分页、详情等）本轮**未重验**，以其文档为准，不代表当前状态。
- Keep 侧为 main 未固定快照，行号级引用在定稿采纳前需按当时 main 复核；"未发现独立 scheduler"为穷举未尽下的相对结论。
- ee 目录代码因许可不可复用，任何借鉴仅限 MIT 主库与公开文档语义。

## 8. 建议后续

1. 先裁决 3.2 节冲突：对已实现的周期 scheduler 走新 ADR（建议定为"已发布 BusinessSystem 范围内受控周期观测"）。
2. 第一层立项：统一资源源能力描述的窄 Go 接口与接入目录元数据展示。
3. 第二层另行评估：关系投影的来源/观测时间模型与 Observed Resource 隔离方案。

## 9. 定稿前核对清单

- [ ] Quoin 引用行号在实施切片合并后是否仍成立（当前基线含未提交修改）。
- [ ] Keep 引用按采纳时点 main 复核一次，并在文首记录复核日期。
- [ ] 3.2 节 scheduler 冲突的 ADR 提案已提交 issue 跟踪后再启动第一层实施。
