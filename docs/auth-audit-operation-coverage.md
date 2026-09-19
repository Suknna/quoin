# 认证与审计活动操作覆盖清单

执行基线 `3ebed73`。下列活动入口已完成执行器和准入转换；整仓回归（含架构门禁与 auth+operations race）及隔离命名空间 r3/r4 部署复验已全部通过，活动业务待接入写入为 0。数量为初始盘点参考，以注册覆盖、架构门禁和行为测试共同确认，不以 grep 数量证明完成。外部邮件/短信送达未授权未验收；正式生产恢复/回滚与双架构发布资格未在本轮范围执行。

## 活动入口

- 正常 Huma API：128 个操作（2026-09 精简后），以既有 OperationID 为稳定标识。
- 原生 mux：alerts/events SSE、Artifact内容、备份下载、调查附件上传，共4个。
- 维护 API：固定认证/审计/维护状态与退出，Restore/RootKeyRebind/Upgrade按原因白名单。未允许入口不能默认公开。
- 服务入口：RuntimeControl、SteleRelay、ArtifactService实际注册方法；精确方法以 protobuf service 定义与注册代码为准。
- 后台：Artifact GC、备份调度、升级调和、lease sweeper、巡检调度、来源观测。
- CLI：serve、secrets bootstrap、admin recover、backup、restore/finalize、root-key rebind、migrate/preflight及历史维护入口；旧首装 admin create 已随安装凭据退役一并移除。

## 转换分组

| 组 | 现有操作标识/范围 | 权限 | 目标 | 状态 |
| --- | --- | --- | --- | --- |
| 认证 | login/getCurrentUser/changeOwnPassword/logout及新流程 | 公开登录、受限流程、正常会话分别处理 | 流程事件自动记录，登录后才签Session | 已转换 |
| 用户 | listUsers/createUser/updateUser/resetUserPassword/revokeUserSessions | Admin | 统一准入及命令执行器 | 已转换 |
| 会话 | listOwnSessions/revokeOwnSession | User且自己的会话 | 自动访问/命令 | 已转换 |
| 审计 | listAuditEvents、新设置/预览 | Admin | 访问一次，不递归 | 已转换 |
| 系统 | getMaintenanceState/getAdminAbout/prepareUpgrade/exitMaintenance | 状态读取按现有边界，其余Admin | 自动访问/命令 | 已转换 |
| 告警查询 | listAlerts/getAlertOccurrence/listAlertObservations | User | 默认查询元信息 | 已转换 |
| 接入问题 | listIntakeIssues/acknowledgeIntakeIssue | Admin | 查询/命令 | 已转换 |
| 告警源 | listAlertSources/getAlertSource/getAlertmanagerReceiverConfig/createAlertSource/listAlertSourceCredentials/rotateAlertSourceCredential/retireAlertSourceCredential/disableAlertSource/revealAlertSourceCredential | Admin | 持久命令取代内存重放，敏感读取前审计 | 已转换 |
| Runtime | prepareRuntimeRegistration/revealRuntimeRegistrationToken/retireRuntimeCredential | Admin | 命令及敏感读取 | 已转换 |
| 连接 | listConnections/createConnection/getConnection/probeConnection/enableConnection/disableConnection/discoverProviderModels/rotateConnectionCredential/getConnectionProbeAttempt/cancelConnectionProbeAttempt/listConnectionProbeResults/listConnectionRevisions/listCredentialGenerations | Admin | 统一操作；外部调用不能冒充同库事务 | 已转换 |
| 插件 | listIntegrationPlugins | Admin | 查询 | 已转换 |
| 业务上下文 | listBusinessContext | User | 查询 | 已转换 |
| 分析 | createInitialAnalysis/listInitialAnalyses/getInitialAnalysis/listInitialAnalysisAttempts/retryInitialAnalysis/cancelInitialAnalysis | User | 访问/命令 | 已转换 |
| 证据 | getEvidence/getArtifactMetadata及原生Artifact下载 | User，敏感对象另需Admin | 输出前审计并保留对象约束 | 已转换 |
| 备份 | listBackups/triggerBackup/getBackup/getBackupSettings/updateBackupSettings/getArtifactRetentionSettings/updateArtifactRetentionSettings及下载 | Admin | 共享执行器，开始/完成分开 | 已转换 |
| 调查 | listInvestigations/createInvestigation/getInvestigation/listInvestigationMessages/sendInvestigationMessage/streamInvestigationMessage/listInvestigationAttempts/listAttemptToolCalls/undoInvestigationMessage/retryInvestigationAttempt/cancelInvestigationAttempt及上传 | User | 持久命令、流访问、任务关联 | 已转换 |
| 历史业务系统 | （HTTP 只读解释面已于 2026-09 精简整体移除；`business_systems` 表仍作为告警/分析/调查的业务维度被现行 SQL 读取） | Admin | — | 已退役 |
| 巡检 | cancelInspectionRun/listInspectionRuns/createInspectionRun/getInspectionRun/listInspectionReports/getInspectionReport/retryInspectionAnalysis/rerunInspection/listPluginInspectionPlans/getPluginInspectionPlan/createPluginInspectionPlan/updatePluginInspectionPlan | Admin | 访问/命令/后台关联 | 已转换 |
| 业务视图 | listBusinessViews/createBusinessView/getBusinessView/updateBusinessView | Admin | 访问/命令 | 已转换 |
| 知识/反馈 | cancelKnowledgeImportBatch/appendDiagnosisFeedback/listDiagnosisFeedback/createAnalysisKnowledgeCandidate/createInvestigationKnowledgeCandidate/createReportKnowledgeCandidate/listKnowledgeCandidates/getKnowledgeCandidate/editKnowledgeCandidateDraft/confirmKnowledgeCandidate/excludeKnowledgeCandidate/searchKnowledge/getKnowledge/listKnowledgeVersions/getKnowledgeVersion/importKnowledgeBatch/listKnowledgeImportBatches/getKnowledgeImportBatch/confirmKnowledgeBatch/createKnowledgeRevisionCandidate/stopKnowledgeReuse | User | 访问/命令/任务关联 | 已转换 |
| 后台及服务 | 任务接受/结果/取消/调和、上传与工具调用、Stele受理 | 明确Service/System，保留原发起者 | 复用attempt/epoch/relay幂等，不冒充人类会话 | 已转换 |
| CLI/维护 | 恢复（admin recover）、备份、升级、root-key rebind | 部署/系统主体 | 同一执行权威，低层schema操作最小例外 | 已转换 |

## 例外要求

- livez/readyz/metrics、SSE心跳和逐帧遥测不逐条复制到业务审计；连接建立与用户命令不能豁免。
- 匿名爆破、无效令牌、畸形请求进入有界安全日志/指标。已认证拒绝不能因HTTP状态码被混成匿名例外。
- 外部告警原始载荷由既有投递/观测权威保存，不复制秘密或全部正文到审计；摄入受理与业务动作仍明确分类。
- 退役浏览器工作台/WS不重新接线，Lintel槽位仍拒绝；退役 browser/kubernetes 的剩余写入面（排空/恢复）由注册执行器操作承载，不再整体豁免，退役 mutator（kubernetes 连接映射 Create/Retire）已按零活动调用方删除，只读解释路由保留。
- 心跳租约续期（attempt.RenewLeaseForBoot）是集中登记的遥测例外：有 fence、可收敛、每 tick 不产生审计行；真实状态迁移永不进入该清单。
- 低层数据库初始化/迁移（bootstrap、upgrade 迁移权威、maintenance offline/recovery 连接构造、restore 隔离 PRAGMA）与审计写入自身是精确枚举的 lowlevel 例外；不存在任意 audit:false。

## 门禁与验收

`internal/contract/execution_architecture_test.go` 是架构门禁之一（与运行时事务守卫、mode=ro 只读探测、审计白名单共同生效）：精确收缩基线（sql_open、tx_control_sql、tx_method、audit_insert、sql_write_exec 五类规则）。sql_write_exec 按接收者类型判定——可证原始句柄（*sql.DB/*sql.Conn/*sql.Tx 参数/字段、`store.db.Conn` 链式取连接、DB() 能力导出）执行字面 INSERT/UPDATE/DELETE/REPLACE 即违规；受守卫 `execution.Executor = *Tx`（具体指针别名，不能由裸句柄或嵌入包装替代）上的业务语句不标记；读快照经 `Reader.BeginSnapshot`（只读事务工厂）与 opaque `execution.Reader`（零值即失败关闭，任意池拒收）。无法证明的接收者交由运行时守卫，基线不因推测膨胀。基线只能收缩：新位点必须走执行器，或按精确逐符号归入 lowlevel（schema/迁移权威、offline 连接构造）、telemetry（心跳租约续期，集中登记）、retired（退役浏览器排空，逐符号附关闭边界测试，重新激活即门禁失败）。当前 pending business = 0，executionAdoptionComplete = true。

## 基线问题（已收口）

分散手工审计与台账（businesssystem audit/rejectVerification/recordCommandResult、attempt auditLifecycleOn、backup recordDownloadAudit、auth RecordCommand 业务调用方等）已全部移除，写入统一经 execution.Run/Execute。原始句柄逃逸以 sealed Executor、只读 Reader 注入与上述静态门禁共同收口；静态扫描未命中不证明路径无审计，运行验收仍以行为测试与审计查询为准。
