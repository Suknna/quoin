# ADR 0013: 模型可见上下文的单一契约（agentcontext）与防漂移守卫

- 状态：已接受
- 日期：2026-09-27
- 范围：internal/agentcontext（新）、internal/quoin/{analysis,investigation,inspection,attempt}、internal/plinth/{agent,worker}
- 参考：ADR-0011（Plinth 不 import Quoin 内部包）、ADR-0012（归一化语义冻结进 AI 输入）

## 背景

ADR-0012 把 severity/title/annotations/富化/视图关联/相关告警窗口冻结进了 initial_analysis 与 investigation 的 canonical 输入，字节与 digest 都正确。但生产验证（`.local-lab/evidence/production-monitoring-compatibility-review.md`）发现模型实际上看不到其中大部分事实：

- Plinth 侧 `agent.Input.Occurrence` 是一个**手抄的匿名子结构**，只声明了 id/state/labels 等旧字段——severity/title/resource/enrichment/correlations/relatedAlerts 在解码时被 `json.Unmarshal` 静默丢弃；
- investigation 的 `recentOccurrence` 同样丢掉 severity/title；
- 巡检报告的冻结计划绑定（planKey/params/scope/连接与模板标识）在 canonical 输入里存在，但消息装配从未渲染。

平台"具备"这些信息、agent"接收不到"——而且没有任何机制报警：Go 对未知 JSON 字段默认静默忽略，digest 只保证字节到达 worker，不保证字节进入 prompt。

### 根因

Quoin（输入生产者）与 Plinth/agent（提示词装配）**各自手写同一 JSON 形状的两份 Go 类型**，中间没有任何编译期或测试期的相等性约束。生产者加字段 → 消费者不知道 → 静默丢字段。这是典型的浅模块病：契约的知识分散在两个模块里，各改各的必然漂移。

## 决策

### 1. 模型可见事实单一定义：internal/agentcontext

新增共享包 `internal/agentcontext`，定义**模型可见**的跨进程事实类型：`Occurrence`、`RelatedAlert`、`RecentOccurrence`、`Correlation`、`Integration`、`InspectionPlan`、`InspectionCheck`。Quoin 的输入结构与 Plinth 的解码结构全部改为这些类型的**类型别名**（`type X = agentcontext.Y`），字段集与 JSON tag 由一处定义。

边界判据沿用 ADR-0012 的口径：**业务事实进 agentcontext；执行元数据（modelContract 预算、toolCatalog、连接 grant、内部 locator）留在各自 envelope**，Plinth 以 `json.RawMessage` 显式声明"已消费、不进 prompt"，绝不做成共享类型——它们不是模型可见事实。

别名必须保持与旧结构逐字段、逐 tag、逐序一致（本次全部一致），因此 canonical 字节与存量 digest 不变；历史 Attempt 的 RebuildInput 复现不受影响。

### 2. 新代 fail-closed 解码；旧代保留宽松解码

当前代（新 agent version）的四个 Parse 函数改用 `DisallowUnknownFields` 解码：生产者新增模型可见字段而消费者未跟进时，Attempt 在派发时**显式失败**（invalid input），绝不静默丢字段。尾随内容同样拒绝。

已在途/历史 Attempt 的执行身份不变：`initial-analysis-v3`、`investigation-v4`、`inspection-analysis-v4` 及更早版本各保留 `ParseLegacy*`（原宽松解码）与**原消息形状**（prior-context builder 精确复刻旧投影——旧代 occurrence 仍不含 severity 等），保证旧 Attempt 以其冻结时的 prompt digest 完成。

### 3. 代际升级（prompt 字节变了必须换身份）

本次模型消息字节实际变化，按既有机制升代，不走任何静默路径：

- initial-analysis：`v3 → v4`，renderer `v6 → v7`
- investigation：`v4 → v5`，renderer `v6 → v7`
- inspection-analysis：`v4 → v5`，renderer `v4 → v5`（输入侧 reportRenderer `v2 → v3`）
- `generationAccepts`/`catalogSchemaVersionFor`/`promptRendererVersionFor`/worker `verifyStart` 各自扩表，旧身份原样保留可执行。

### 4. 契约测试：消费者用生产者的真实类型钉住

- `internal/plinth/agent/projection_contract_test.go`：用 **Quoin 真实的** `analysis.Input`/`investigation.Input` 序列化后喂给 Plinth 的 Parse + Build，断言 occurrence/relatedAlerts 全字段进入模型消息、执行元数据不泄漏、prior 代不出现新字段。生产者改模型可见事实而消费者没跟，测试在这里红。
- `internal/quoin/inspection/report_projection_test.go`：同一钉法覆盖巡检报告（plan/checks/绑定标识进上下文，budget/catalog 不进）。
- `TestCurrentInputRejectsUnrecognizedFacts`：钉住 fail-closed 解码本身。

## 后果

- 「平台有、模型看不到」这一类缺陷从**静默漂移**变成**编译期单一定义 + 测试期契约钉住 + 运行期派发失败**三层拦截；后续给模型加事实只需改 agentcontext 一处，两侧同时可见。
- Plinth 生产代码仍不 import Quoin 内部包（ADR-0011 不变）；契约测试是 test-only 依赖，方向为 plinth→quoin、quoin/inspection→plinth/agent，无环。
- 旧代身份永远宽松解码 + 旧投影，直到其 Attempt 全部终局；每升一代需要扩三处表（generationAccepts、promptRendererVersionFor、verifyStart），这是有意的成本——改模型可见契约必须是有意识的版本变更。
- 首发无生产存量 Attempt，无数据迁移。
