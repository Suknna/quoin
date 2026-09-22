# Quoin 使用手册

面向已完成初次部署（[Kubernetes](getting-started-kubernetes.md) / [Docker Compose](getting-started.md)）的管理员（Admin）与普通用户（Operator）：告警、来源观测、AI SRE 与巡检的日常使用。角色边界由服务端逐请求强制——Operator 只使用告警与 AI SRE，不进入管理页面。

能力与接入模型的总说明见 [docs/integration-inspection-guide.md](integration-inspection-guide.md)（当前可用插件为 Prometheus、Thanos、Alertmanager）。本文以当前 HEAD（含巡检证据上下文与可自定义报告，commit `0ebde26`）为准。

## 界面总览

登录后分为两个工作区：

- **AI SRE**：对话式分析与告警初步分析。
- **运维中心**（仅 Admin 完整可用）：**告警列表**、**巡检**、**业务视图**、**接入管理**（故障复盘入口未开放）。
- **管理**（仅 Admin）：**关于**、**用户**、**模型提供方**、**备份与保留**、**审计**、**运行时**。

## 接入管理

入口：运维中心 → **接入管理**。活动平台目录为 **Alertmanager、Prometheus、Thanos**。同一平台可有多个接入。

以下创建、验证、启用步骤针对 **Prometheus/Thanos 指标接入**；Alertmanager 使用下一节的告警源凭据与 receiver 流程，不套用指标 probe。

1. **创建**：选择平台进入专属表单。Prometheus/Thanos 收集 `baseUrl`、认证模式（`none`／HTTP Basic／Bearer）与 TLS（私有 CA 填 `tlsCaPem`，可设 `tlsServerName`，内网自签不要轻易 `tlsSkipVerify`）。秘密只提交一次，不回显、不进 URL 或日志。
2. **验证（probe）**：新建接入先停用；验证创建真实探测 Attempt。验证失败不会自动启用，也不会触发采集；已启用接入的一次探测失败同样**不等于**自动停用。
3. **启用**：启用的唯一门槛是通过的 probe。启用事务内自动创建**默认基础巡检计划**（仅人工运行，不产生定时模型费用）并开始默认来源级观测；创建失败整体回滚。
4. **停用/轮换**：停用只阻止新派发；轮换创建新凭据代次并原子切换。
5. **模型提供方**（管理 → 模型提供方）：配置 `baseUrl`、`chatModelId`、可选 `embeddingModelId` 与 API Key。验证会实测 streaming、tool call 与取消；仅在配置了 Embedding 模型时验证 Embedding 及维度。**AI SRE 对话与巡检报告分析都依赖已启用的模型提供方**；未启用时 Run 可以采证、查看证据，但无法生成报告版本。

mall-shop 建议：先建 Prometheus 接入（`baseUrl` 用宿主 LAN IP/`host-gateway`，见 getting-started 第八步），验证并启用；Alertmanager 走下面的告警源。

## 告警与告警源

- **创建告警源**：运维中心 → 接入管理 → Alertmanager。填写**来源键**（如 `mall-shop-alertmanager`，不要放凭据或内部地址）；系统生成面向部署公共入口（`publicOrigin`）的 Alertmanager receiver 配置，**bearer 令牌只显示一次**——立即复制并配置进 lab Alertmanager 的 `alertmanager.yml`（receiver + route）。告警经 gateway 回传到 `/stele/webhook/alertmanager`，由 Stele 接收。
- 该 bearer 是**告警源凭据**，与 Stele→Quoin 的内部 `stele-service-token` 无关；支持轮换（最多同时两代，旧代按轮换语义退休）。
- **告警列表**：运维中心 → 告警列表查看接收到的告警与状态流转；AI SRE 会对告警做初步分析（产生 Evidence 与模型调用）。

## 来源观测

每个已启用接入的详情中有**观测资源**列表：区分"当前观测到 / 当前未观测到"及陈旧标注，可手动**刷新观测**（同一接入同时只有一个观测 Run）。观测身份 = 来源接入 + 对象类型 + 规范来源身份，跨来源同名对象不合并。失败或截断的结果不会清空资源、也不推断删除；"未观测到"只来自一次完整成功的观测。

mall-shop 的 mall-tiny（JVM）、MySQL、Redis 以 lab Prometheus 已抓取的 exporter 指标出现；当前通过指标插件观测，不需要直接连接数据库或 K3s API。

## 巡检

入口：运维中心 → **巡检**。计划独立于业务声明，直接绑定**一个接入**与一个插件模板（当前 `promql_instant`／`promql_range`，参数是封闭的 PromQL 表达式与窗口秒数）。

**计划字段**：

- 范围三选一：**整个接入** / **显式对象集合**（用已观测对象）/ **业务视图**。Run 创建时冻结精确标签条件，采证前经 PromQL AST 注入每个向量选择器；业务视图无标签条件时不能创建范围 Run。
- 调度：留空 = 仅人工运行；标准五字段 cron + 时区即定时。**定时巡检触发模型报告费用，需明确启用**。同一计划同时最多一个运行中的 Run；重叠定时周期记 `SkippedOverlap`，不补跑。
- **分析语义（本次新能力，Run 创建时冻结）**：
  - **检查说明**（可选，≤2000 字）：这项检查在观测什么、如何解读结果；
  - **指标单位**（可选，≤100 字，如 %、ms、个）：结果数值的语义单位；
  - **初始报告要求**（可选，≤4000 字）：你希望报告遵循的用户级要求。

  三者在 Run 创建时冻结，之后修改计划**不改写已存在 Run**；Run 详情的"冻结的分析配置"面板可查。

**运行与报告**：

- Run 详情按状态与时间、检查项（`ok`／`gap`，附采证时间与 Evidence）排列。**采证完成（Completed / CompletedWithGaps）只表示证据采集事实，不表示系统健康**；gap 逐项如实记录，`gap ≠ 0` 请先看证据再下结论。
- **报告版本由模型生成**：每版报告标注版本号、模型与"本次报告要求"，正文引用的 Evidence 可点开溯源。报告分析在配好模型提供方后可用。
- **重新分析现有证据**：复用本 Run 冻结的 Evidence 生成新报告版本，不重新采证。弹框中"仅本次自定义报告要求"关闭 = 沿用 Run 冻结的初始报告要求；开启后可编辑，**清空表示本次显式无附加要求**，非空则仅本次覆盖——绝不改写旧证据或旧报告的含义。
- **重新采证**：创建新 Run 并引用旧 Run（`rerun_of`）；与"重新分析"相互独立，没有含糊的"重试"。

### 第一次 mall-shop 巡检：可照填示例

1. 在接入管理创建 `mall-shop-prometheus`，地址 `http://192.168.1.200:30090`、认证 `none`，真实验证后启用。
2. 打开观测资源并刷新，确认商城 exporter 目标存在；本次环境完整清单见[演练手册](mall-shop-lab.md)。
3. 到巡检页新建计划，选择刚才的接入、`promql_instant`、范围“整个接入”，调度留空。表达式填：

   ```promql
   up{system_id="mall-shop",job!="prometheus-self"}
   ```

4. 检查说明填“查看商城采集目标是否可达；1 仅证明 exporter 可抓取，不代表业务依赖完全健康”，指标单位填“布尔值（0/1）”。初始报告要求填“逐项列出目标、采证时间、异常和证据引用；区分缺数据与零值，不把 exporter 可达等同业务正常”。
5. 保存后运行，打开 Run 的检查结果和 Evidence，核对实际标签和值。尚未配置模型时先验采证，不宣称报告已生成。
6. 配好模型提供方后查看报告，必要时“重新分析现有证据”；若要确认演练恢复后的最新健康状态，应“重新采证”。

进一步分别建立 `mysql_up{system_id="mall-shop"}`、`redis_up{system_id="mall-shop"}`、`nginx_up{system_id="mall-shop"}` 检查。它们比单独 `up` 更接近对应依赖的服务状态，但仍不等同于完整下单交易成功。

## 业务视图（可选）

入口：运维中心 → **业务视图**。视图是对**一个接入**范围加明确 label 条件的可选组织（例如以 `system_id: mall-shop` 圈定商城指标），可附加业务说明；表单与 YAML 编辑同一对象。视图不拥有资源、凭据或额外权限，可用于收窄巡检范围及配置告警业务归属；不创建视图也能接收告警、观测资源、使用工具与基础巡检。

### 配置告警归属

新告警按**业务视图中的告警源与标签条件**匹配，不再依赖旧的已发布业务声明：

1. 新建或编辑业务视图，例如键 `mall-shop`、名称“mall 商城”。
2. 若同时用于指标巡检，指标接入选择 `mall-shop-prometheus`。
3. 在告警来源中显式选择 `mall-shop-alertmanager`（对应 `alertSourceKeys`）。指标接入与告警来源是不同对象，选择 Prometheus 不会自动授权所有 Alertmanager 来源。
4. 标签条件设置 `system_id=mall-shop`，保存。
5. 触发一次新的告警周期，在告警详情查看业务视图归属；告警列表也支持按业务视图筛选。

唯一视图匹配时归属成功，多视图同时匹配时标记冲突，无匹配时显示未归属。未选择告警源的视图不参与归属；选择告警源时必须有非空标签条件，避免无条件匹配全部告警。

归属及候选视图的名称、来源、条件在首次接收时冻结；之后修改视图不重算历史告警。升级前已接收的记录保留原历史快照，新配置需用**新触发周期**验证。此归属只提供业务组织信息，不增加 AI SRE 查询权限。

旧的 BusinessSystem 声明创建/发布界面已从产品移除，[docs/business-system-declaration-guide.md](business-system-declaration-guide.md) 仅为解读历史记录而保留。

## AI SRE

**当前没有按业务系统配置 AI SRE 查询范围的能力。** 对话任务的候选指标来源是任务创建时符合条件的已启用接入；选择业务上下文不会自动限制为该业务的指标。系统内部的来源授权用于约束任务可使用的接入和凭据，不等于按业务隔离。巡检计划中的业务视图标签范围只约束相应巡检采证，不会自动应用到 AI SRE 对话。

对话与告警初步分析可使用已启用接入的只读工具（如 `thanos_query`，PromQL 即时查询）。模型只提交 `query` 等人类可读参数；来源接入由系统按冻结授权解析，有歧义时模型用 `sourceRef` 指名或向你追问。**模型看不到也选不了地址、凭据或连接配置**；工具结果作为不可变 Evidence 保存，可在对话与告警详情查看。

对话创建执行回合时还会冻结平台最近收录的最多 10 条告警的来源、标签与开始时间，供模型识别“曾收到告警”这一事实；这不是完整告警历史检索，也不携带实时恢复状态。历史条目不扩大指标工具权限。即时 `ALERTS` 查询为空不能证明没有告警规则或从未触发告警；应结合平台记录、历史指标和实际采证时间判断。

模型报告仍需核对原始证据，执行状态 `Succeeded` 只表示任务完成，不保证每个推断或格式约束都正确。要求机器消费的 JSON 时，应明确字段、类型及禁止围栏，并在消费端解析校验；长度要求应写清是否计入空格、标点与英文字母。

## 审计

管理 → **审计**。当前实现按自动审计设计关联操作、主体、来源与结果，秘密不应进入审计（[ADR-0006](adr/0006-automatic-audit-and-operation-correlation.md)）。领域文档仍将整体切换标为进行中，不能把本手册理解为全部操作覆盖与安全验收已完成；具体覆盖见[审计操作覆盖](auth-audit-operation-coverage.md)。保留期默认且最小 **6 个日历月**，只能通过部署配置 `audit.retentionMonths` 延长，界面不可缩短。

## 用户与账号

管理 → **用户**：唯一内置管理员，初始化后以正式密码 + 二级验证登录。忘记密码或因素全部不可用时走离线恢复（`quoin admin recover --mode password|factors`，需停机与 attached TTY），见 [docs/deployment.md](deployment.md)；登录页没有自助恢复入口。

## 备份与保留

管理 → **备份与保留**：默认每日 `0 0 * * *`（UTC）核心一致性备份、保留 30 份，可调整定时与策略。在线备份管理**不是**在线恢复：`quoin restore` / `quoin backup --offline` / `quoin migrate` 都要求停机并独占数据库，流程见 [docs/deployment.md](deployment.md)。普通 SQLite 备份不含 Stele service token、TLS 私钥与根密钥——这些 Secret 由你独立备份。

## 能力边界（不要期待不存在的功能）

- 当前可用插件为 Prometheus、Thanos、Alertmanager；浏览器与 Kubernetes 插件已移除。
- 无 SSH、无 MySQL/Redis 直连连接类型：数据库类对象经 Prometheus exporter 指标 + Alertmanager 告警观测。
- 故障复盘入口与知识库"整理为知识候选"按钮尚未开放（界面明确标注"开发中"）。
- 巡检报告是模型生成的分析文本：事实以 Evidence 为准，`Completed` 采证状态与健康结论是两回事。

## 常见问题

| 问题 | 处理 |
| --- | --- |
| 收不到验证码 / 登录二级验证失败 | 管理/初始化投递配置核对 TLS 模式、CA、私网 CIDR；投递失败不会降级为密码单因素登录 |
| 巡检 Run 有报告但没有"重新采证" | Run 未到终态时只能取消；终态后可重新采证（新 Run）或重新分析 |
| 定时巡检没有模型报告 | 定时触发的报告需显式启用（涉及模型费用）；确认模型提供方已启用 |
| 告警没有到达 | 核对 lab Alertmanager receiver URL（`publicOrigin` + `/stele/webhook/alertmanager`）与 bearer；轮换后旧 bearer 失效 |
| 观测资源为空 | 确认 lab Prometheus 真的抓到了对应 exporter；手动"刷新观测"后查看 Run 结果 |
