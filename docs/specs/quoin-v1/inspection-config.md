# Quoin v1 — 巡检配置与来源观测机器 Schema（inspection-config.md）

**状态：Draft**

**CATEGORY 前缀：`CFG`**（SPEC-TRACE-002）

**Non-normative：** [ADR-0004](../../adr/0004-plugin-capability-registry.md) 的插件化主线已实施：巡检由独立计划拥有（一个来源接入 + 一个插件模板 + integration/objects/businessView 范围），接入验证并启用后自动进行来源级观测，业务视图可选。`BusinessSystem` 声明、全局 Label Contract 与配置发布已退出主线；其 Schema、YAML 规则与 Config Verification Run 材料仅保留用于解读既有历史记录，**MUST NOT** 被当作现行配置权威。历史 E2E 文档继续记录其执行当时验证的契约和结果，不因此改写。

## 1. 独立巡检计划（现行）

- **CFG-PLAN-001 —** 巡检计划 **MUST** 直接绑定恰好一个来源接入与一个插件巡检模板（`(plugin_id, template_id, template_version)`；`template_version` 为空表示 Run 创建时冻结当时 ready 版本）。字段形状由 [`contracts/sql/schema.sql`](contracts/sql/schema.sql) 的 `inspection_plans` 独占；本文 **MUST NOT** 复制字段清单。范围 `scope_kind` 封闭为 `integration | business_view | objects`，与 `scope_json` 的形状约束由 Schema CHECK 强制。计划 key 使用 `^[a-z][a-z0-9-]{0,62}$`，退役不复用。（来源：[ADR-0004](../../adr/0004-plugin-capability-registry.md)、[CONTEXT「独立巡检计划」](../../../CONTEXT.md#独立巡检计划inspection-plan)）
- **CFG-PLAN-002 —** 接入启用事务内 **MUST** 幂等创建仅人工运行的默认基础计划：确定性派生 key（`basic-<connectionName>`，超长/非法时以完整 SHA-256 前缀替代）、`promql_instant` 模板与 `up` 参数、`scope_kind='integration'`；同名计划已存在时原样保留，创建失败 **MUST** 回滚整个启用。非指标接入不创建默认计划。（来源：[ADR-0004](../../adr/0004-plugin-capability-registry.md)）
- **CFG-PLAN-003 —** `scope_kind='objects'` **MUST** 携带显式对象集合（objectType + 来源身份）；`scope_kind='business_view'` **MUST** 引用业务视图 key，Run 创建时冻结所用视图内容并固定与计划自己的接入相交，**MUST NOT** 全源查询；视图后续修改 **MUST NOT** 改写已创建 Run。（来源：[CONTEXT「业务视图」](../../../CONTEXT.md#业务视图business-view)）
- **CFG-PLANRUN-001 —** Run 创建时 **MUST** 在同一事务确定性展开并冻结目标、模板版本、封闭参数、查询窗口、接入 revision/generation 与授权；执行中 **MUST NOT** 扩大目标，重新采证 **MUST** 创建新 Run 并以 `rerun_of` 引用旧 Run。每个展开检查成为 `scope_type='run_check'` 的 `inspection_collection` 子 Attempt，冻结 `inspection_plugin_execution_v1` 输入与唯一 `config_thanos_query` grant 并派发 Plinth supervisor 执行插件 Collector。（来源：[Issue #66](https://github.com/Suknna/quoin/issues/66)、RUNTIME-TASK-003/011/012、[ADR-0004](../../adr/0004-plugin-capability-registry.md)）
- **CFG-PLANRUN-002 —** 采集结果 **MUST** 经 Quoin 对冻结输入与 result digest 重验后，在同一 Attempt 事务原子写入完整 Evidence 并收口子 Attempt；error/gap 不制造 Evidence，只记录封闭 gap 事实。历史声明 Run 的 PromQL 子 Attempt 重建 `inspection_promql_execution_v1`；新旧路径同受 Attempt boot/epoch/cancel fence 裁决，Quoin 不解密或执行 PromQL。（来源：Issue #66、DATA-INSPECT-003）
- **CFG-CRON-001 —** 计划 `cron` 缺省表示仅人工运行；存在时 **MUST** 恰为五个空白分隔字段，并通过 `github.com/robfig/cron/v3` v3.0.1 `ParseStandard`。`@every`/`@daily` 等 descriptor、秒字段和内嵌 `CRON_TZ`/`TZ` **MUST** 拒绝；时区只取计划自身的 `timezone`（IANA），调度按分钟边界生成 `scheduled_for` UTC 去重键。（来源：[CONTEXT「独立巡检计划」](../../../CONTEXT.md#独立巡检计划inspection-plan)）
- **CFG-SEMANTICS-001 —** 计划 **MAY** 携带可选分析语义字段：`check_description`（检查说明，≤2000）、`metric_unit`（指标单位，≤100）与 `report_instructions`（初始报告要求，≤4000，用户级）。Run 创建时 **MUST** 把计划当前显示名与这三个字段确定性冻结进 `inspection_runs` 的对应 `frozen_*` 列（origin 触发器保证不可变）；计划后续修改 **MUST NOT** 改写任何已存在 Run，重新采证 **MUST** 逐字段复制源 Run 的冻结值而不读计划当前定义。字段形状由 [`contracts/sql/schema.sql`](contracts/sql/schema.sql) 独占，字段长度校验在应用与 schema CHECK 两侧一致。（来源：[ADR-0004](../../adr/0004-plugin-capability-registry.md)）
- **CFG-INSPECTRUN-002 — Immutable report closure：** Mixed collection 已收口后，Quoin 以冻结的计划/Run 绑定和准确的 collection Evidence 集创建一次 `inspection_analysis`。模型只能撰写报告内容和选择冻结 locator；程序不预设健康 verdict 或 severity。`inspection_report_result_v1` 的 locator、payload digest 与 Evidence digest 由 Quoin 重验，并经单一 SQLite closure 形成不可修改的 Report。collection gap 仍是报告可见事实，不会删除其它 check 的 Evidence；分析失败不会产生占位 Report。（来源：RUNTIME-TASK-013）
- **CFG-INSPECTRUN-003 — 分析输入冻结与逐检查项清单：** 计划 Run 的 `inspection_analysis_v1` 输入 **MUST** 冻结实际生效的全部分析要求：Run 冻结的检查说明/单位/初始报告要求（绝不回读计划当前定义）、逐检查项结构化清单（check key、名称、说明、单位、冻结的表达式与范围/步长字面量、真实执行窗口与步长、observedAt、真实 warnings、显式 gap 原因、Evidence/Artifact 对应）。检查结果行 **MUST** 在提交时一次性冻结采集元数据（observedAt/warnings/执行窗口事实，`meta_json`，成功与 gap 同一来源；gap 只有元数据没有 Evidence，清单据此保持缺口可见）；元数据 JSON 形状损坏 **MUST** 携带 run/check 身份失败，**MUST NOT** 静默吞掉。gap 检查项 **MUST NOT** 伪造成功事实或把缺数据当成 0；未定义阈值的检查项 **MUST NOT** 被程序或系统提示预设健康判断，证据优先原则保留在系统提示，报告要求只出现在用户级消息。重建（rebuild）**MUST** 只读不可变行（Run 冻结列、检查目录、检查结果及其元数据、Evidence、工件归属与 `inspection_analysis_requirements`），并逐字节命中冻结 digest；旧 Attempt（无行）**MUST** 保持其历史 canonical 字节。报告版本投影 **MUST** 展示该版本分析实际生效的要求（Attempt 不可变行优先，Run 冻结值回退）。（来源：[ADR-0004](../../adr/0004-plugin-capability-registry.md)）
- **CFG-INSPECTRUN-004 — 重分析的报告要求三态：** 显式重分析 **MAY** 携带 `reportInstructions`（≤4000，按字符计数）三态：字段缺省 = 继承 Run 冻结的初始报告要求；空串 = 本次分析显式无要求（清除）；非空文本 = 仅本次覆盖。三态 **MUST** 随本次 Attempt 冻结进 `inspection_analysis_requirements`（CREATE 后不可改；行只能绑定 `inspection_analysis × run` Attempt）并进入本次输入快照 digest，只对本次生成的报告版本生效，**MUST NOT** 改写 Run 冻结值、旧 Evidence 或任何旧报告的含义；自动（收敛触发）分析 **MUST** 只使用 Run 冻结值。巡检分析拥有独立版本身份（`inspection-analysis-v1` 生成与其渲染器代次）：报告 prompt 演进 **MUST** 同步推进该身份，绝不在 initial-analysis 共享身份下静默漂移。（来源：[ADR-0004](../../adr/0004-plugin-capability-registry.md)）

## 2. 来源级观测（现行）

- **CFG-OBS-001 —** 已验证并启用的接入，且其平台类型在部署启用集中恰有一个 Discover 能力插件时，Quoin **MUST** 按自身权威准入有界观测（默认周期调度、接入启用与手动刷新共用同一准入）；不要求任何业务声明或业务视图。两个启用插件声明同一平台类型是部署歧义，**MUST** fail-closed。（来源：[ADR-0004](../../adr/0004-plugin-capability-registry.md)）
- **CFG-OBS-002 —** 观测对象身份 **MUST** 为 `(来源接入, 对象类型, 规范来源身份)`，规范来源身份由插件声明的 identity labels 按名排序规范编码；跨来源同名对象 **MUST NOT** 合并。对象类型、发现查询与每轮预算是插件描述元数据，Quoin 在派发前冻结并在执行端复核，声明 **MUST NOT** 与执行漂移。（来源：[CONTEXT「来源级观测」](../../../CONTEXT.md#来源级观测source-scoped-observation)）
- **CFG-OBS-003 —** 同一接入同时最多一个 active Run；`scheduled_for` 是定时去重键，手动/启用准入与 active Run 合并而不分叉。失败、截断和局部结果 **MUST NOT** 清空对象或推断物理删除；只有同一冻结范围完整成功才能把未再见到的对象置为未观测，`stale` 是显式事实而 **MUST NOT** 由观测推断。（来源：[CONTEXT「来源级观测」](../../../CONTEXT.md#来源级观测source-scoped-observation)）
- **CFG-OBS-004 —** 来源级观测 **MUST NOT** 产生任何业务声明资源投影：历史 `observed_resources` 表及其身份标签表已随 `20260920_retire_declared_discovery_v1` 迁移整体删除，现行发现事实只落在 `observation_run_objects` 与 `config_resource_scopes`（第 5 节）。（来源：[ADR-0004](../../adr/0004-plugin-capability-registry.md)）

## 3. Journey Catalog（历史；已随浏览器移除）

Journey Catalog 与浏览器巡检条款已随浏览器运行时一并移除；本节仅保留标题以维持既有引用的可解读性。

## 4. PromQL 模板校验（现行）

- **CFG-PROMQL-001 —** 插件模板参数中的每个 PromQL 表达式 **MUST** 使用 Prometheus 官方 AST 解析器静态校验；不得用正则或字符串替换代替语义验证。校验在计划保存与 Run 冻结时进行，实际查询在采证时经冻结 grant 执行。（来源：[ADR-0004](../../adr/0004-plugin-capability-registry.md)）
- **CFG-PROMQL-004 —** range 模板执行 **MUST** 以真实开始采证的 `evidence_at` 为窗口终点，保存实际 start/end/step（`range_seconds`/`step_seconds` 字面量）；校验与执行分离。（来源：DATA-CONFIG-003、[CONTEXT「巡检项」](../../../CONTEXT.md#巡检项inspection-check)）

## 5. 历史模型（只读；不得作为现行配置权威）

**Non-normative：** 本节条款描述已被 ADR-0004 替换的旧模型，用于解读历史声明、历史 Run 与 E2E 证据；Config Verification 引擎及其持久面、`config_discoveries`/`observed_resources`/`observed_resource_identity_labels` 表已随 2026-09 退役迁移物理删除（声明事实仍可从历史版本的 `declaration_json`/`yaml_body` 与 `config_resource_scopes` 读取）。任何实现 **MUST NOT** 恢复其写入路径、调度或发布语义。

- **CFG-SCOPE-001 —** 历史 `BusinessSystem` 声明的机器结构由 [`contracts/schemas/business-system.schema.json`](contracts/schemas/business-system.schema.json)（稳定 `$id`、`apiVersion: quoin/v1`、`kind: BusinessSystem`、draft 2020-12 封闭对象）继续拥有，仅用于解释既有版本。（来源：ADR-0003，已被 ADR-0004 替代）
- **CFG-YAML-001/002/003 —** 严格 YAML 解析规则（单文档、拒绝重复 key/anchor/merge/自定义 tag/尾随内容、超限拒绝、IANA 时区与五字段 cron、稳定名称唯一与退役不复用）只描述既有声明材料的解析事实；该解析管线不再接受新声明。（来源：ADR-0003，已被 ADR-0004 替代）
- **CFG-CONFIG-001..004、CFG-REFRESH-001 —** 业务声明作为唯一配置权威、`resourceRef` 强绑定检查、`metrics.connectionRef` 必填、独立资源刷新禁止等条款只约束历史数据解读；现行模型中巡检计划独立于任何业务声明，采集由插件模板与计划范围决定。（来源：ADR-0003，已被 ADR-0004 替代）
- **CFG-CONTRACT-001/002 —** 全局 Label Contract **MUST NOT** 被创建、激活或解析为现行配置；其历史版本、激活与 Run 按既有保留规则可读，**MUST NOT** 被新声明或运行时重新解释。（来源：ADR-0003，已被 ADR-0004 替代）
- **CFG-PUBLISH-001、CFG-VERIFYRUN-001/002、CFG-EXPORT-001 —** 声明草稿发布顺序、Config Verification Run 绑定草稿、YAML 导出与 `prepublish` 联合激活是历史流程；Config Verification 引擎的全部持久面已随 2026-09 退役迁移删除，本节仅用于解读历史 Release 工件。（来源：ADR-0003、#97，已被 ADR-0004 替代）
- **CFG-PROMQL-002/003 —** 按声明资源合并 `matchLabels` 强制注入与 `allowedMetrics` 精确名/前缀约束仅适用于历史声明 Attempt 的冻结解释；现行模板参数校验由 CFG-PROMQL-001 与插件模板声明拥有。（来源：ADR-0003，已被 ADR-0004 替代）

## 6. 验证要求

- **CFG-VALIDATION-001 —** `business-system.schema.json` **MUST** 继续由真实 draft 2020-12 校验器以正反例矩阵守护（有效 `quoin/v1`/`BusinessSystem`、封闭对象、必填 metadata/metrics/alerts/inspections、资源 labels 与允许指标、告警自有 labels、`resourceRef`、instant/range 字段关系及全部反例），作为历史 Schema 的回归门。（来源：Issue #12 交付纪律、ADR-0004 历史保留要求）
- **CFG-VALIDATION-002 —** strict YAML 解析 **MUST** 以真实 `yaml.v3` `yaml.Node` 行为逐项回归验证：重复 key、anchor/alias/merge、自定义 tag、非字符串字段名、第二文档、尾随内容、超限全部拒绝，合法单文档通过；该管线仅服务于历史材料解析。（来源：Issue #12 交付纪律）
- **CFG-VALIDATION-003 —** PromQL 校验 **MUST** 以官方 parser 验证现行模板参数与历史声明投影：合法表达式通过；非法语法拒绝；历史投影中 VectorSelector 缺业务系统 label / 非精确 `=` / 值不等于 system key 拒绝；discovery selector 含 `offset`/`@`/聚合/`label_replace`/子查询拒绝；check/template expression 允许 `offset`/`@`/子查询（只需通过 AST 与归属校验）。（来源：Issue #8 交付纪律）
- **CFG-VALIDATION-004 —** SQL 投影 **MUST** 以 SQLite harness 继续守护历史投影触发器（根投影列与 `system_key` 匹配、发布投影同步、check 类型化 CHECK、`scope_type` 绑定）以及现行 `inspection_plans` 范围 CHECK、`inspection_runs` 新计划必填/历史列禁写约束（DATA-VALIDATION-002）。（来源：Issue #12 交付纪律、ADR-0004）
