---
status: accepted
---

# 告警归属改用业务视图的显式来源约束与标签条件

## Context

告警归属（alert attribution）原以旧业务声明为权威：交付首收时评估 `business_systems.current_config_version_id` 指向的已发布版本及其 `config_alert_source_refs`/`config_alert_label_conditions`（旧表按测试夹具提供），唯一匹配写入 `alert_occurrences.business_system_id` 并冻结旧 `alert_occurrence_attributions` 证据。ADR 0004 之后业务声明的写入与发布入口已移除，旧权威不再有真实供给：新告警永远无匹配、永远未归属，前端只能显示"未归属"并提示用户去声明业务系统——一条已不存在的路径。

业务视图（ADR 0004）已经承载"来源接入范围 + 精确标签条件"的组织语义，且被巡检计划消费；它是现行的、有真实用户界面的范围模型。但它面向 Prometheus/Thanos 接入（connection），而告警交付的身份是 Alertmanager 逻辑告警源（`alert_sources.source_key`）：两者是不同的身份空间，数字 ID 不可互换，也不存在"用 Prom connection 顶替 AM 源"或"无声明即跨源匹配"的合法默认。

## Decision

**（2026-09-16 确认并实施）告警归属的唯一权威改为业务视图的显式告警源约束 + 精确标签条件；归属证据冻结在独立投影，旧 business_system 归属字段停写、仅作历史。**

- **视图参与告警归属必须显式声明 AM 来源**。`business_views` 新增 `alert_source_keys_json`（默认 `[]`）：空数组 = 该视图不参与告警归属；非空时逐 key 对照 `alert_sources.source_key` 精确校验（去重、稳定序），且必须至少一个精确标签条件——空标签条件绝不构成吞掉一切的兜底匹配。connectionName 与 alertSourceKeys 是两种身份，写路径与匹配路径都互不替换。
- **首收一次性判定，三态封闭**。首条不可变 delivery item 创建 Occurrence 的同一事务中评估全部视图：候选 = 视图 `alert_source_keys_json` 含交付源 key 且全部标签条件精确命中。唯一匹配 → `attributed`；多匹配 → `ambiguous`；无匹配 → `unattributed`（原因区分 `source_mismatch`/`label_mismatch`）。同一 occurrence 终身不重算；后续视图改名、改条件只影响新 occurrence。
- **独立冻结投影，不扩旧模型**。新表 `alert_occurrence_view_attributions`：`occurrence_id` 主键、封闭三态 `status`、可空 `attributed_view_id`（RESTRICT，视图不可删除故不级联）、`candidates_json` 按序冻结每个候选视图完整快照（viewId/viewKey/displayName/scope，唯一归属与多候选歧义均可追溯）、`reason_json`、delivery/item 溯源列；BEFORE UPDATE/DELETE 触发器拒绝改写，INSERT 触发器强制条目闭合到同一 Delivery。读模型展示的 key/name 一律取自 `candidates_json[0]` 冻结快照，绝不 join 当前视图行，改名/退役不漂移。`alert_occurrences.business_system_id` 与旧 `alert_occurrence_attributions` 停写、旧行原样保留；不写旧表、不做 dual-write、不伪造历史归属，历史未归属记录不重算。
- **schema 变更走正式前任迁移**。前发布 canonical digest（`281f533f…`）钉死为 `20260916_alert_view_attribution_v1` 的前任：纯加列/加表、copy-safe、需 Upgrade 维护窗口 + 升级备份门禁，经 `quoin migrate preflight`/`migrate` 转换；不在运行库上直接改 schema。业务视图补 key 不可改写、行不可删除触发器（退役不复用）。
- **读模型与 UI 服从新归属**。统一告警读模型暴露 `viewAttribution`（status/viewKey/viewName/candidatesJson/reasonJson/createdAt）；告警中心按业务视图过滤与下拉选择（旧 `businessSystemKey` 过滤保留兼容），徽标与详情展示真实归属与交付来源；平台故障只出现在无归属过滤的读取中。不提示用户"先声明业务系统"；旧声明证据仅在无新投影的历史行上作为历史事实展示。

## Consequences

- 新告警立即获得真实归属；运维入口是业务视图管理（勾选 AM 源 + 标签条件），无独立 reassign API——修正视图配置后由新告警的新 occurrence 证明归属。
- 旧 business_system 归属成为纯历史维度：列表/详情中仅在历史行展示，永不参与新匹配；旧声明相关提示全部退役。
- 多视图重叠会明确产生 `ambiguous` 而非随机归属，重叠即配置错误的信号。
- `ambiguous`/`unattributed` 的完整候选与原因冻结在投影中，审计与事后追溯不依赖当前视图状态。

## Verification

以测试为准：唯一匹配冻结快照、显式来源与标签必要条件、空标签条件不兜底、歧义候选快照冻结、write-once、schema 触发器 SQL 拒绝、冻结名字不随改名漂移、viewKey 过滤不拼接平台故障、迁移保留旧 alerts/视图历史并钉死前任身份（`internal/quoin/alerts/attribution_test.go`、`internal/quoin/businessview/service_test.go`、`internal/quoin/upgrade/alertviewattribution_test.go`）。
