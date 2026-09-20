# ADR 0012: 告警归一化层（富化→去重→关联）与 business_systems 域退役

- 状态：已接受
- 日期：2026-09-20
- 范围：internal/plugins（第三能力 AlertNormalizer）、sql/schema.sql（告警语义列/富化与关联新表/业务系统域删表）、internal/quoin/{alerts,businessview,analysis,investigation,app}、openapi、web、文档
- 取代：ADR-0008 的双归属读模型（视图归属三态状态机 + business_system 停写保留）整体退役
- 参考：Keep（keephq/keep）的 enrichment/dedup/correlation 架构、Alerta 的统一告警模型与 severity 词表、PagerDuty PD-CEF、BigPanda 的 event→alert→incident 分层

## 背景

ADR-0011 之后,入向已在 Stele 网关按来源归一化入队,但 Quoin 侧的告警语义仍是「裸 Alertmanager」:
- 语义字段(severity/title/annotations)无一落库,annotations 靠读时从原始 body `json_extract` 现算,groupLabels/externalURL 等直接丢弃;
- 业务归属是 ADR-0008 的双轨读模型:旧 business_system 归属停写保留(列恒 NULL,analysis 的业务上下文实际已悬空——**新告警的 AI 分析拿不到任何业务上下文**),新视图归属是三态单值状态机(attributed/ambiguous/unattributed);
- business_systems/label_contracts 只读域已休眠(无写入方、无声明入口),仅旧路径在读。

本 ADR 按 Keep 的三段流水线范式重设计这一层:把不同来源、不同字段结构的告警映射到同一套语义,并让三条 AI 路径(告警→分析、对话分析、巡检→分析)消费同一套归一化产物。

## 决策

### 1. 统一告警语义（首观测冻结落列）

`alert_occurrences` 增加归一语义投影,首观测一次性冻结:

- `severity`:封闭四级词表 + 序数 critical(4) > high(3) > warning(2) > info(1);来源词表外的值降级 info 并保留原始值(`SeverityRaw` 不落库,原始 severity label 原样留在 labels)。词表只四级——我们是分析系统不是值班系统,四级+序数已覆盖 AI 排序需求。
- `title`（alertname）、`annotations_canonical`（全量 annotations JSON 冻结）、`resource`（instance→job 推断）。
- 平台故障走同一投影,消灭读侧伪造 labels 的特例。
- 告警实例模型不变:幂等键仍是 (source, fingerprint, starts_at),observations 仍是事件流（= 去重前事件计数）。

### 2. 归一化器是插件体系的第三种能力

`Plugin.AlertNormalizer`（纯函数、零业务依赖）:把本插件 EventSource 产出的归一化 payload 映射到统一语义（severity 映射表、title、annotations、resource 推断）。注册表校验:提供 normalizer 必须同时提供 EventSource。alertmanager 内建实现;未来来源(zabbix/grafana)各带各的映射。「不同来源不同结构 → 同一套语义」由插件承载,Quoin 统一执行——与工具/事件源同构,业务语义不进 Stele（ADR-0011 边界不变）。

### 3. intake 四段流水线（一个事务内,首观测执行一次）

1. **Normalize**:按 source_kind 取 normalizer → 语义列。无 normalizer 的源缺省 + intake issue（`normalizer_missing`）,事件不丢。
2. **Enrich**:静态 mapping 规则（`enrichment_rules`:label 条件 + 可选来源 key → outputs 字段如 team/service/owner;多命中按 priority 叠加、后不覆盖先）求值,结果连同规则溯源冻结进 `alert_enrichments`（occurrence 1:1）。规则修改不影响历史（冻结哲学,同视图归属）。
3. **Dedup**:现状即正确——(source, fingerprint, starts_at) 实例幂等 + observations 事件计数,不动。
4. **Correlate**:业务视图命中从「三态单值归属」泛化为**多命中全记录**（`alert_occurrence_correlations`,冻结 view_key/display_name）;取消 attributed/ambiguous/unattributed——多命中是富信息不是错误。派生上下文（同视图 open 计数等）查询时算,不落库。

富化规则 v1 只有静态 mapping + 视图关联;正则抽取、工作流富化、拓扑、手动富化明确不做（v1 边界）。

### 4. 工具归属判据

**读/操作外部平台 → 插件工具（经 Stele 网关、受插件启停门控）;读/操作 Quoin 自有权威数据 → 平台工具（Quoin 内部执行、基础目录常驻）。**

据此新增平台只读模型工具 `alerts_recent`（与 artifact_read/artifact_grep 同族:quoin_routed、无连接 grant、return_to_model）:按视图/severity/时间窗查询归一化告警摘要。归一化告警库是跨源产物,不 scoped 到任何 EventSource 插件——alertmanager 保持纯事件源。

### 5. 三条 AI 路径的上下文供给

- **告警→initial_analysis**:输入冻结 = 归一语义（severity/title/annotations_canonical）+ 富化快照 + 关联（viewKey/displayName）+ 相关告警窗口（同视图或同来源近 24h 最近 10 条:id/severity/title/state/startsAt）。删除悬空的 resolveBusinessContext 与 config lineage。
- **对话分析 investigation**:创建参数 businessSystemKey 删除;occurrence 来源链接保留并渲染增强（带归一语义+富化）;**Send 支持消息中追加告警来源**（「+号」交互的后端能力）;RecentOccurrences 上下文供给从 business_system 换为按关联视图。
- **巡检→inspection_analysis**:输入不变（run 冻结证据 + 视图 scope,视图模型不动）;`alerts_recent` 工具进全部 agent 世代目录,模型自动拉取相关告警,无人工介入。

### 6. business_systems 域整域退役（首发清理的延续）

business_systems/business_system_config_versions/config_alert_source_refs/config_alert_label_conditions/config_resource_scopes/config_plans/config_checks/label_contracts/label_contract_state/label_contract_activations 十张表、config/ 声明解析器、`/business-context` API、attempt_input_items.business_system_config_version_id 与 attempt_connection_grants.business_system_id 列、analysis/investigation 的旧路径全部删除。首发无存量数据,无迁移。knowledge/observation/maintenance 已验证零耦合。

## 后果

- 告警语义从「读时猜」变为「写时冻结」:severity/title/annotations 成为可查询、可过滤、可冻结进 AI 输入的一等列;annotations 反解逻辑消灭。
- 视图从「归属权威」变为「关联维度之一」+ 富化规则成为新的可运营配置面（Admin 在设置-平台管理）;前端告警面单轨化（viewKey + severity + 富化徽标）,双归属诊断卡删除。
- 新来源接入成本 = 一个 EventSource + 一个 AlertNormalizer（都是纯函数）+ 可选富化/视图配置;Quoin 侧零改动。
- AI 分析终于有了稳定的业务上下文供给（富化快照 + 关联 + 相关告警窗口）,不再依赖已休眠的声明域。
- 不引入 incident/事件实体:我们的关联产物是规则型上下文（多视图命中 + 相关告警窗口）,没有值班响应流去消费 incident 状态机;未来若产品进入事件响应,可在此之上加层。

## 与其它 ADR 的关系

- ADR-0011（组件职责/插件 v2）:本 ADR 是其插件体系在「告警语义」维度的第一次应用;网关边界不变。
- ADR-0008（视图归属）:三态归属状态机与停写双轨整体取代;business_views 实体保留并重定位为关联维度与巡检 scope。
- ADR-0004（插件能力注册）:AlertNormalizer 遵循同一注册表/冻结/校验模型。
