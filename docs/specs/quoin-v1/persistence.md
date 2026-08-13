# Quoin v1 — 数据与事务规格（persistence.md）

**状态：Draft**

**CATEGORY 前缀：`DATA`**（SPEC-TRACE-002）

**Non-normative：** 本文件是 Quoin v1 的 SQLite 数据模型、聚合关系、唯一约束、事务程序、迁移、Artifact 与派生索引的规范性说明。机器可表达的表、列、索引、外键、CHECK 与触发器只由 [`contracts/sql/schema.sql`](contracts/sql/schema.sql) 承载；本文件解释其语义、跨聚合事务、生命周期与迁移约束，**不复制完整字段清单**（SPEC-AUTHORITY-002）。规范条款来源为 [`CONTEXT.md`](../../../CONTEXT.md) 稳定标题、[Issue #9](https://github.com/Suknna/quoin/issues/9)（Q9.1 A / Q9.2 A）、[Issue #8](https://github.com/Suknna/quoin/issues/8)、[Issue #2](https://github.com/Suknna/quoin/issues/2)、[Issue #20](https://github.com/Suknna/quoin/issues/20) 及证据 commit（`prototype/issue-20-sqlite-persistence@582042e4d2bb740ff6a1b9feab94875e7c5360ff`、`research/sqlite-capabilities@2cfb8841d9798bd27114e44b4576f2f1ad1a4ea7`）。

## 1. 范围与权威

- **DATA-SCOPE-001 —** `contracts/sql/schema.sql` **MUST** 是 Quoin v1 当前完整 SQLite Schema 的唯一机器权威；本文件 **MUST NOT** 复制其表/列/约束清单，引用时 **MUST** 使用相对路径与稳定 symbol（表名、约束名、索引名）。（来源：Issue #8「机器契约目录」）
- **DATA-SCOPE-002 —** 以下事实在 v1 **MUST NOT** 持久化到 SQLite，并由本文件明确解释其归属：Journey Catalog 内容（构建期嵌入 Quoin/Lintel，`inspection_runs` 只保存 digest 与版本）；Stele 告警源凭据只读快照（仅内存缓存，`alert_deliveries` 只保存非秘密 `credential_id` 与快照版本）；Kubernetes 运行时状态（人工调查按需只读查询，不建表）；Execution Attempt 工作区与 Browser profile（各自卷/目录，不进入备份）；未查看通知的 toast 内存态（服务端只持久化 `user_viewed` 投影）。（来源：CONTEXT「服务身份」「Kubernetes 运行时状态」「浏览器身份」「后台任务提示」）
- **DATA-SCOPE-003 —** 派生投影（`knowledge_search_docs`、`knowledge_fts`、`embeddings`、`alert_change_log`、`task_change_log`、`user_viewed`）**MUST** 可丢弃、可重建，**MUST NOT** 成为对应领域事实的第二权威源。（来源：CONTEXT「可复用知识」「实时投影」「调查与巡检工作区」「后台任务提示」）

## 2. 账号与会话

- **DATA-AUTH-001 —** `users` **MUST** 持久化 `password_change_required` 与 `password_change_required_at`：离线创建首个 Admin、Admin 离线重置密码与备份恢复流程 **MUST** 在同一事务置位（`password_change_required=1` 时必须有置位时间戳，CHECK 强制）；成功改密 **MUST** 在同一事务清除标志；清除标志 **MUST NOT** 由其他业务命令隐式完成。（来源：CONTEXT「本地账号认证」「管理员离线恢复」「一致备份」）
- **DATA-AUTH-002 —** `password_change_required=1` 的用户登录后 **MUST** 只获得受限 Session：**MUST** 只能调用自身状态读取、改密与登出；权限层 **MUST** 以该持久状态裁决，**MUST NOT** 依赖前端隐藏入口。（来源：CONTEXT「本地账号认证」「管理员离线恢复」）
- **DATA-AUTH-003 —** `sessions` **MUST** 只保存 raw 32-byte bearer 的固定长度 SHA-256 digest（`session_token_digest`，UNIQUE、BLOB 32 字节）；raw bearer **MUST** 只存在于浏览器 Cookie 与认证瞬间内存，**MUST NOT** 写入 SQLite、日志或审计。（来源：CONTEXT「同源 Web 会话」「模型调用边界」）

## 3. 技术 locator 与身份规则

- **DATA-IDENT-001 —** Quoin 生成、且没有用户稳定 key 的记录 **MUST** 使用正 64-bit SQLite INTEGER locator（`INTEGER PRIMARY KEY CHECK (id > 0)`）。需要保证已分配 ID 永不复用的领域 locator 与单调序列（如 `alert_change_log.id`）**MUST** 使用 `AUTOINCREMENT`；单行表（`schema_state`、`backup_settings`）与纯投影/连接表 **MAY** 使用普通 `INTEGER PRIMARY KEY`（Q9.1 A）。（来源：[Issue #9](https://github.com/Suknna/quoin/issues/9) Q9.1）
- **DATA-IDENT-002 —** locator 的 HTTP 十进制字符串表示与 Proto `int64` 表示 **MUST** 由 `http-api.md`（#10）与 `runtime-protocol.md`（#11）定义；本文件只定义存储形态为 INTEGER。（来源：[Issue #9](https://github.com/Suknna/quoin/issues/9) Q9.1）
- **DATA-IDENT-003 —** 用户稳定 key（`business_systems.key`、`alert_sources.source_key`、`connections.name`、`config_discoveries.discovery_key`、`config_plans.plan_key`、`config_checks.check_key`、`runtime_slots.slot` 固定两行）以及复合领域身份（`alert_occurrences` 三元组、`observed_resources` 三元组）**MUST** 是相等性权威且不可改写（schema 触发器冻结）；locator **MUST NOT** 参与领域去重或业务相等判断，**MUST NOT** 以隐藏 UUID 替代稳定身份。（来源：CONTEXT「稳定身份保留」「服务身份」）
- **DATA-IDENT-004 —** ID 允许间隙，**MUST NOT** 被解释为业务时间或提交顺序；顺序 **MUST** 只由显式序列承载（`alert_change_log.id`、`investigation_messages.seq`、`model_calls.call_seq` 等）。（来源：CONTEXT「实时投影」「撤回消息」）
- **DATA-IDENT-005 —** 已发布、已执行 Test Run 或被历史引用的稳定 key **MUST** 以 tombstone（Disabled/Retired）保留，**MUST NOT** 释放给新对象；仅从未发布、从未运行且从未被引用的草稿/staging 可在 DATA-TX-015 程序下物理清理。（来源：CONTEXT「稳定身份保留」）
- **DATA-IDENT-006 —** Alertmanager fingerprint **MUST** 保存为上游 64-bit 无符号值的 8 字节大端 BLOB（`alert_occurrences.fingerprint`、`alert_delivery_items.fingerprint`，CHECK `length = 8`）；**MUST NOT** 使用有符号整数或 SHA-256 替代。（来源：CONTEXT「告警发生」、Issue #9）
- **DATA-IDENT-007 —** `starts_at` **MUST** 无损规范化为 UTC RFC3339Nano 文本并参与 Occurrence 身份；规范化 **MUST NOT** 截断时间精度，**MUST NOT** 因时区/亚秒差异拆分同一告警。（来源：CONTEXT「告警发生」）

## 4. 领域写命令与并发前提

- **DATA-COMMAND-001 —** 所有经认证外部调用者发起的领域写命令 **MUST** 以 `(principal_type, principal_id, client_command_id)` 唯一（`client_commands` UNIQUE），保存命令类型、非秘密请求摘要、确定性结果与结果对象引用。（来源：CONTEXT「领域写命令契约」）
- **DATA-COMMAND-002 —** 请求摘要 **MUST** 覆盖全部语义请求字段，**包括** `expected_row_version`、`expected_head_message_id` 等并发前提；重试携带与原请求相同的摘要。摘要 **MUST NOT** 包含 nonce、时间戳或其他不影响语义的字段。（来源：CONTEXT「领域写命令契约」、Issue #9 Q9.1 确认；Rationale：若摘要排除 `expected_*`，版本前提不同的两个写请求会被误判为同一命令。）
- **DATA-COMMAND-003 —** 相同 ID 且相同摘要重放 **MUST** 返回原确定性结果（`result_payload_json`）；相同 ID 但不同摘要 **MUST** 返回冲突，**MUST NOT** 静默以新请求执行。（来源：CONTEXT「领域写命令契约」）
- **DATA-COMMAND-004 —** 确定性业务拒绝（版本冲突、fence 拒绝、校验失败）**MUST** 以 `outcome='rejected_known'` 持久化结果；基础设施失败或提交结果未知 **MUST NOT** 被伪造为权威成功或失败，调用方 **MUST** 复用同一 `client_command_id` 重试。（来源：CONTEXT「领域写命令契约」）
- **DATA-COMMAND-005 —** 修改当前状态或当前版本指针的命令 **MUST** 携带 `expected_row_version` 并在同一 UPDATE 的 WHERE 中比较；纯追加创建 **MUST NOT** 强制 expected version。（来源：CONTEXT「领域写命令契约」）
- **DATA-COMMAND-006 —** 调度器创建定时 Run **MUST** 使用 `(business_system_id, plan_key, scheduled_for)` 确定性键（`ux_inspection_run_scheduled` 部分唯一索引），该键在 Run 进入任何终态后仍 **MUST NOT** 被复用（不补跑）；同一事务 **MUST** 绑定当时生效的 `config_version_id` 与 `label_contract_version_id`。（来源：CONTEXT「领域写命令契约」「巡检运行」）

## 5. 审计与执行溯源

- **DATA-AUDIT-001 —** `audit_events` / `audit_event_targets` **MUST** 追加且不可改写（触发器禁止 UPDATE/DELETE），记录 actor、action、target（类型/ID/版本）、命令 ID、提交时间、结果与领域记录引用。（来源：CONTEXT「审计与执行溯源」）
- **DATA-AUDIT-002 —** 审计 **MUST NOT** 复制消息、Evidence、附件、Prompt 正文或秘密；隐藏思维链 **MUST NOT** 被存储或展示。（来源：CONTEXT「审计与执行溯源」「模型调用边界」）
- **DATA-AUDIT-003 —** 每个 `execution_attempts` / `model_calls` **MUST** 保存实际供应商连接 revision/credential generation、模型 ID、Prompt/工具 schema/输入快照 digest、usage、延迟、重试序号与结构化终止原因；输出正文由消息、Report、Candidate、Evidence 等领域表承担。`model_calls`/`tool_calls` 的终态（`succeeded`/`failed`/`cancelled`）**MUST** 不可变（schema 触发器冻结）。（来源：CONTEXT「审计与执行溯源」「执行尝试」）
- **DATA-AUDIT-004 —** 确定性业务拒绝 **MUST** 以零领域状态变更的短事务同时提交命令 outcome 与审计事件；提交结果未知的基础设施失败 **MUST NOT** 写入权威失败审计。（来源：CONTEXT「领域写命令契约」「审计与执行溯源」）

## 6. 告警接入与生命周期

- **DATA-ALERT-001 —** `alert_deliveries.body` **MUST** 以 BLOB 直接存 SQLite（不存 Artifact）以维持接入单事务语义与精确原始字节（可能非 UTF-8），`body_size_bytes` 以字节计并由 CHECK 与 BLOB 长度一致；`relay_id` **MUST** 唯一（重试幂等）。整体 JSON 或 `alerts[]` 无法可靠枚举时记录 `status='rejected'` 且 **MUST NOT** 更新任何 Occurrence。（来源：CONTEXT「告警送达」「产物」）
- **DATA-ALERT-002 —** 顶层可解析时 **MUST** 在同一 SQLite 事务中预检全部 `alert_delivery_items`：正常项更新 Occurrence/Observation，`identity_conflict`/`fingerprint_mismatch` 项进入 `alert_intake_issues`；数组第一项 **MUST NOT** 因先处理而获胜，异常项 **MUST NOT** 阻塞正常项。（来源：CONTEXT「告警送达」「告警接入问题」）
- **DATA-ALERT-003 —** `truncatedAlerts > 0` 时 **MUST** 正常处理已包含项、以 `kind='delivery_truncated'` 永久标记 Delivery 与来源，commit 后返回 204；**MUST NOT** 对未知缺失项作任何生命周期推断。（来源：CONTEXT「告警送达」）
- **DATA-ALERT-004 —** Occurrence 身份 **MUST** 为 `(source_id, fingerprint, starts_at)` 唯一三元组；不可变完整 labels 快照（`labels_canonical` + `labels_digest` + `alert_occurrence_labels` 子表）**MUST** 在关联每次送达前逐项复核，同一三元组不同 labels 记录 `identity_conflict`，**MUST NOT** 静默合并；身份与 labels 快照字段由 schema 触发器冻结（`trg_alert_occurrences_identity_immutable`、`trg_alert_occurrence_labels_no_update`）。（来源：CONTEXT「告警发生」「告警接入问题」）
- **DATA-ALERT-005 —** Occurrence 生命周期只有 `Firing | Resolved`，**MUST** 只由载荷中对应 `alerts[i].status` 推进；**MUST NOT** 使用顶层 status、groupKey、endsAt、截断缺失、来源停用或长期无通知推断；Resolved **MUST NOT** 被重新打开（触发器 `trg_alert_occurrence_no_reopen`）。（来源：CONTEXT「告警发生」）
- **DATA-ALERT-006 —** 每个有效项 **MUST** 形成不可变 `alert_observations`，记录 `effect`（`initial_firing`/`repeat_firing`/`resolved`/`resolved_first`/`late_firing_after_resolved`）；resolved-first 与 resolved 后迟到 firing **MUST** 保留观察且迟到 firing 不重开 Occurrence。`observed_state` 与 `effect` **MUST** 一致（`firing` 只对应三种 firing effect，`resolved` 只对应 `resolved`/`resolved_first`，schema CHECK 强制）。（来源：CONTEXT「告警观察」）
- **DATA-ALERT-007 —** 影响告警列表的 Occurrence 变更 **MUST** 在同一事务内维护 `row_version`（应用在 UPDATE 中递增）、`last_state_change_at`/`resolved_at`，并由触发器（`trg_alert_change_log_insert`/`trg_alert_change_log_state`）写入 `alert_change_log`。（来源：CONTEXT「实时投影」「告警列表与详情」；Rationale：SQLite 触发器不支持 `SET NEW`，时间与版本字段由应用在同一条 UPDATE 中维护。）
- **DATA-ALERT-008 —** Delivery 事务 **MUST** 重验来源启用、凭据有效性与归属（`credential_id` + 快照版本）；Delivery 与凭据吊销按 SQLite 提交顺序裁决，**MUST NOT** 使用墙钟宽限期。（来源：CONTEXT「告警源凭据投影」）
- **DATA-ALERT-009 —** 每个来源轮换期间 **MUST** 最多同时两个 `Active` 凭据（触发器强制），Retired 凭据 **MUST NOT** 复活；`retired_at` 由应用在状态变更 UPDATE 中写入，且 `state` 与 `retired_at` 的一致性 **MUST** 由 CHECK 强制（Retired 必须有 `retired_at`，Active 必须无）；凭据来源字段（`source_id`/`digest`）与历史 **MUST** 不可改写、不可删除（`trg_alert_source_credentials_origin_immutable`、`trg_alert_source_credentials_no_delete`）。（来源：CONTEXT「告警源凭据投影」）

## 7. 初步分析与调查

- **DATA-ANALYSIS-001 —** 同一 Occurrence **MUST** 同时最多一个 active Initial Analysis（`ux_initial_analysis_active`：`Queued`/`Running`）；双击或命令重试 **MUST** 返回同一记录（命令幂等）。`Interrupted` **MUST** 视为终态；终态（`Succeeded`/`Failed`/`Cancelled`/`Interrupted`）**MUST** 不可变（`trg_initial_analyses_terminal_immutable`）。（来源：CONTEXT「初步分析」）
- **DATA-ANALYSIS-002 —** 第一个合法成功结果 **MUST** 原子封存为不可变 `initial_analysis_outputs`（每分析最多一个，UNIQUE）；重新分析 **MUST** 创建新的 Initial Analysis，旧结果保留。（来源：CONTEXT「初步分析」）
- **DATA-INVEST-001 —** 每个 Investigation **MUST** 只有一个当前有效 head（`investigations.current_head_message_id`）；发送消息 **MUST** 在单事务中追加消息并创建 Execution Attempt，携带 `client_command_id` 与 `expected_head_message_id`，head 已变化 **MUST** 返回冲突。（来源：CONTEXT「撤回消息」）
- **DATA-INVEST-002 —** Undo **MUST** 只允许撤回最新用户回合，并在同一事务中：将该消息及全部后继（助手回复、工具调用、Evidence 引用、知识草稿）标为只读 `withdrawn`、取消依赖该回合的 active Attempt；迟到结果只留审计。新消息 **MUST** 从撤回前的有效 head 继续。第一版 **MUST NOT** 支持任意历史撤回、分支切换/合并、原地编辑或重新生成分支。（来源：CONTEXT「撤回消息」）
- **DATA-INVEST-003 —** 每个 Investigation **MUST** 同时最多一个 active 模型 Attempt（`ux_execution_attempt_active_scope`；`Cancelling` 计入 active）。（来源：CONTEXT「撤回消息」「执行尝试」）
- **DATA-INVEST-004 —** `investigation_messages` 的正文/角色/顺序 **MUST** 不可变（触发器只允许 `active` → `withdrawn`，`trg_investigation_messages_no_unwithdraw` 拒绝 withdrawn 复活），消息历史 **MUST NOT** 被删除。（来源：CONTEXT「撤回消息」「在线保留」）
- **DATA-ATTACH-001 —** 每条消息 **MUST** 最多一个 `text_attachments`（UNIQUE）；正文必须是有效 UTF-8、无 NUL、默认 ≤10 MiB；正文 **MUST** 作为 Artifact 保存（`artifact_id`），`source_materials` 只承担来源登记（`kind='text_attachment'` 时 `content` 为 NULL），原始文件名只作审计元数据。（来源：CONTEXT「文本附件」「产物」）

## 8. 配置与发布

- **DATA-CONFIG-001 —** 每个 Business System **MUST** 同时最多一个已发布配置版本（`ux_business_config_published`）；发布命令 **MUST** 携带 version ID 与 expected current published version ID，事务中重验并切换，不匹配 **MUST** 冲突。已发布版本 **MUST NOT** 退回 `draft`，`superseded` 是终态（`trg_business_config_versions_no_unpublish`、`trg_business_config_versions_superseded_immutable`）。（来源：CONTEXT「业务系统配置版本」）
- **DATA-CONFIG-002 —** Label Contract 联合激活 **MUST** 原子：零启用业务系统时首个契约经静态校验直接激活；有启用系统时只有全部系统准备并验证兼容版本后，**MUST** 在同一事务切换契约 current 与全部兼容配置版本；任一失败 **MUST** 全部继续使用旧版本。已开始的 Run **MUST** 继续绑定旧版本，契约变更 **MUST NOT** 重写历史 label 快照。`retired` 契约 **MUST** 不可再激活（`trg_label_contracts_retired_immutable`）。（来源：CONTEXT「标签契约」）
- **DATA-CONFIG-003 —** 配置版本与契约正文 **MUST** 不可变（触发器只允许 `state`/发布字段变化）；digest **MUST** 覆盖全部语义内容；上传 **MUST** 只解析一次并保存原文、parser/schema 版本与类型结构，运行 **MUST** 只使用类型结构。（来源：CONTEXT「业务系统配置版本」）
- **DATA-CONFIG-004 —** discovery/plan/check 使用跨版本稳定的用户 key（`config_discoveries.discovery_key` 等），显示名可改、key 不可复用；每个版本内 `UNIQUE (config_version_id, key)`。（来源：CONTEXT「业务系统配置版本」「巡检计划」「巡检项」）

## 9. 观测资源

- **DATA-OBSERVED-001 —** ObservedResource 身份 **MUST** 为 `(business_system_id, discovery_key, identity_key)`，其中 `identity_key` 是按 label 名排序的 identity label/value 规范编码（相等性权威，UNIQUE）；`identity_digest` 只作查找/校验和；显示名、数组顺序与非身份 labels **MUST NOT** 参与身份。（来源：CONTEXT「观测资源」）
- **DATA-OBSERVED-002 —** 只有无 warnings、无部分响应的完整成功刷新 **MUST** 才能把本轮未出现资源标记为未观测（`current=0`）；不完整刷新 **MUST** 保留上次成功状态并暴露 `last_successful_refresh_at` 与 `stale`。（来源：CONTEXT「观测资源」）
- **DATA-OBSERVED-003 —** identity label 缺失或同一身份 tuple 冲突 labels **MUST** 记录发现错误（`observed_refresh_log`），**MUST NOT** 以空值或最后一条覆盖。（来源：CONTEXT「观测资源」）

## 10. 连接与凭据

- **DATA-CONN-001 —** `connection_revisions`（非秘密配置）与 `credential_generations`（AEAD 密文）**MUST** 不可变（追加触发器）；current 指针（`connections.current_revision_id`、`current_credential_generation_id`）**MUST** 原子切换，旧秘密 **MUST NOT** 再下发；历史保留非秘密 generation 元数据。（来源：CONTEXT「连接」）
- **DATA-CONN-002 —** Attempt 派发前 **MUST** 在同一事务中检查连接启用并绑定实际 revision/generation；停用连接 **MUST** 阻止新派发，已接受 Attempt 使用内存旧快照完成（`ConnectionDisabled` 终止原因）。（来源：CONTEXT「连接」「执行尝试」）
- **DATA-CONN-003 —** `revalidation_required=1` 的连接 **MUST** 阻止一切新派发，直到 Admin 显式成功重验后才清除标志；恢复流程按 DATA-BACKUP-005 置位。（来源：CONTEXT「一致备份」、Issue #9 确认）
- **DATA-CONN-004 —** 密文的 AEAD envelope、密钥管理与格式由 `security.md`（#16）定义；本 Schema 只保存 `ciphertext BLOB` 与非秘密 `meta_json`。（来源：CONTEXT「模型调用边界」「连接」）

## 11. 浏览器身份

- **DATA-BROWSER-001 —** 每个 Business System **MUST** 恰好一个 Browser Identity（UNIQUE）；`browser_profile_generations` 不可变，只有显式发布生成新 generation；Quoin **MUST NOT** 保存 profile 内容（备份不含 profile）。（来源：CONTEXT「浏览器身份」）
- **DATA-BROWSER-002 —** 卷丢失、损坏或升级不兼容 **MUST** 使身份进入 `AuthenticationRequired`；重新登录恢复路径按 DATA-BACKUP-005 与「浏览器身份」执行。（来源：CONTEXT「浏览器身份」「一致备份」）
- **DATA-BROWSER-003 —** 同一身份的登录、巡检、探索 **MUST** 独占串行；`browser_operations` 记录三类操作边界（人工登录不记录键盘/截图/trace；Exploration 每 Attempt 一份 trace；Journey 失败时保留 trace），必要 Artifact 上传失败 **MUST** 标记证据不完整。操作 `result` 一旦产生 **MUST** 终态不可变且必须带 `ended_at`（CHECK + `trg_browser_operations_result_immutable`）。（来源：CONTEXT「浏览器身份」「浏览器操作记录」）

## 12. 巡检运行与执行尝试

- **DATA-INSPECT-001 —** 同一计划 **MUST** 同时最多一个 active Run（`ux_inspection_run_active` 部分唯一索引：`Queued`/`WaitingForCapacity`/`Running`，含人工触发；定时确定性键见 DATA-COMMAND-006）；重叠定时周期在检测到 active Run 时以 `SkippedOverlap` 直接落库（不在 active 集合内，索引允许），不补跑；人工触发展示当前 active Run。（来源：CONTEXT「巡检运行」）
- **DATA-INSPECT-002 —** `evidence_at` **MUST** 只在真正采证开始时生成，**MUST NOT** 在排队时伪造；Run 权威状态只描述采证（九态 CHECK），模型分析使用独立 Attempt/Report 状态，分析失败 **MUST NOT** 回写 Run。Run 终态（`Completed`/`CompletedWithGaps`/`Failed`/`Cancelled`/`Interrupted`/`SkippedOverlap`）**MUST** 不可变（`trg_inspection_runs_terminal_immutable`）。（来源：CONTEXT「巡检运行」「巡检报告」）
- **DATA-INSPECT-003 —** `inspection_check_results` 的状态一致性 **MUST** 由 CHECK 强制：`ok` 必须有 `evidence_id`，`error`/`gap` 必须有 `gap_reason`。（来源：CONTEXT「巡检运行」「证据」）
- **DATA-ATTEMPT-001 —** Attempt **MUST** 在派发前持久化（`execution_attempts`），保存 boot ID、递增 connection epoch、接受状态与有限 lease；lease 内同 boot 重连 **MUST** 上报 active Attempt 调和而不重派；新 boot、lease 到期、吊销、崩溃或替换使 Attempt `Interrupted`。（来源：CONTEXT「执行尝试」）
- **DATA-ATTEMPT-002 —** active 集合 **MUST** 为 `Queued | Assigned | Running | Cancelling`；`Cancelling` 计入 active，`Interrupted`/`Succeeded`/`Failed`/`Cancelled` 为终态且 **MUST** 不可变（`trg_execution_attempts_terminal_immutable`）；每个 `(scope_type, scope_id)` 同时最多一个 active（`ux_execution_attempt_active_scope`，`check_key IS NULL`）；巡检采集子 Attempt（`check_key` 非空）不参与该唯一约束；Attempt 归属字段（`attempt_type`/`scope_type`/`scope_id`/`check_key`）与派发绑定的 `connection_revision_id`/`credential_generation_id`（一旦非空）**MUST** 不可变（schema 触发器冻结）。（来源：CONTEXT「执行尝试」「撤回消息」「初步分析」）
- **DATA-ATTEMPT-003 —** 用户取消 **MUST** 先事务提交 cancellation fence；`Queued/WaitingForCapacity` 直接 `Cancelled`，`Running` 先 `Cancelling`，Runtime 确认或 lease 到期后 `Cancelled`；成功与取消按 SQLite 提交顺序裁决（DATA-TX-005）。（来源：CONTEXT「执行尝试」）
- **DATA-ATTEMPT-004 —** 结果 **MUST** 按 attempt ID 幂等；旧 epoch 迟到结果只审计；取消前已提交的 Evidence/Tool/Artifact **MUST** 保留为部分结果；页面/SSE 断线/登出 **MUST NOT** 隐式取消。（来源：CONTEXT「执行尝试」「撤回消息」）
- **DATA-ATTEMPT-005 —** 部分 token 后的失败 **MUST NOT** 成为有效输出（Attempt `Failed` + 结构化终止原因）；Tool 失败可成为 Evidence 缺口；每次 Attempt 使用干净工作区，成功前所有引用 Artifact **MUST** 上传校验提交。（来源：CONTEXT「执行尝试」）
- **DATA-ATTEMPT-006 —** `row_version` **MUST** 由应用在每次 UPDATE 中显式递增，`expected_row_version` 的比较与递增在同一条 UPDATE 的 WHERE/SET 中完成；`initial_analyses`、`inspection_runs` 同规则。（来源：CONTEXT「执行尝试」「领域写命令契约」；Rationale：SQLite 触发器不支持 `SET NEW`。）

## 13. 证据与报告

- **DATA-EVIDENCE-001 —** `evidence` **MUST** 不可变（追加触发器），记录目标、参数、观察时间、原始结果或 Artifact 引用、warnings、errors 与完整性；正文位置 **MUST** 恰好一个（CHECK：`result_json` 与 `artifact_id` 二选一且至少一个）；Tool Call 执行前 **MUST** 创建记录并以真实时间戳推进。模型报告以文件保存也仍是分析，**MUST NOT** 因成为 Artifact 而升级为 Evidence。（来源：CONTEXT「证据」「调查与巡检工作区」）
- **DATA-REPORT-001 —** `inspection_reports` **MUST** 不可变版本（`UNIQUE (run_id, version)`），精确引用 Evidence 集合、模型、Prompt 与 Attempt；重新分析 **MUST** 复用原 Evidence、创建新 Report 版本，**MUST NOT** 修改旧报告或重新采证；`Succeeded` 只表示模型分析完成。（来源：CONTEXT「巡检报告」）

## 14. 知识沉淀

- **DATA-KNOWLEDGE-001 —** `knowledge_versions` **MUST** 不可变（追加触发器）；`reusable_knowledge.current_version_id` **MUST** 同时指向至多一个版本；修改标题、正文、范围、条件、限制或恢复复用 **MUST** 创建并重新确认新版本。（来源：CONTEXT「可复用知识」）
- **DATA-KNOWLEDGE-002 —** 检索资格 **MUST** 为 `current ∧ 未停用 ∧ 来源有效 ∧ 未 exit`；资格变化 **MUST** 在同一 SQLite 事务中增删 `knowledge_search_docs` 并由触发器同步 `knowledge_fts`（FTS5 external-content trigram，稳定显式整数 rowid = `knowledge_version_id`；知识版本不可变，仅有 insert/delete 同步路径）。（来源：CONTEXT「可复用知识」）
- **DATA-KNOWLEDGE-003 —** 来源拒绝或停止复用使版本退出检索后 **MUST** 永不自动复活（`knowledge_version_retrieval_state.exited` 单调粘性，触发器强制；退出状态行禁止物理删除 `trg_knowledge_retrieval_state_no_delete`，且 `exited=1` 时 `exited_at`/`exit_reason` 必须非空 CHECK）；恢复复用 **MUST** 创建并确认新版本。（来源：CONTEXT「诊断反馈」「可复用知识」）
- **DATA-KNOWLEDGE-004 —** Candidate 模型原始建议 **MUST** 不可变（触发器），用户修改产生递增 `draft_revision`；同一整理流程只有最新 generation 的最新草稿可确认；确认 **MUST** 携带命令 ID 与 expected revision，stale revision **MUST** 冲突；每个 Candidate **MUST** 最多创建一个 Reusable Knowledge（`ux_knowledge_candidate_confirmed`）。Candidate 来源类型与 `diagnosis_feedback` 目标类型统一为 `initial_analysis_output`/`inspection_report`/`investigation_message`；导入批次候选使用 `source_material` 且必须有 `import_batch_id`（schema CHECK 强制）。Candidate 终态（`Confirmed`/`Excluded`/`Superseded`/`SourceInvalid`）**MUST** 不可变（`trg_knowledge_candidates_terminal_immutable`）。（来源：CONTEXT「知识候选」）
- **DATA-KNOWLEDGE-005 —** 批量确认 **MUST** 在单个事务校验全部 expected revision，全成或全不成；Batch 只在当前 generation 中仍可操作的 Candidate 上计算完成状态；`SourceInvalid` 是不可确认终态，`Cancelled` 是 batch 级 fence（禁止后续编辑/确认但保留 Candidate）。Batch 终态（`Failed`/`Completed`/`Cancelled`）**MUST** 不可变（`trg_knowledge_import_batches_terminal_immutable`）。（来源：CONTEXT「知识导入批次」）
- **DATA-KNOWLEDGE-006 —** 相同命令 **MUST NOT** 重复创建 Knowledge；**MUST NOT** 按标题、正文 hash 或 embedding 相似度自动合并。（来源：CONTEXT「知识导入批次」「知识候选」）

## 15. 派生索引：FTS5、Embedding 与 SSE

- **DATA-DERIVED-001 —** `knowledge_fts` **MUST** 采用 external-content trigram 并随 `knowledge_search_docs` 在触发器内同步（insert/update/delete）；FTS 索引 **MUST** 可校验、丢弃并重建（rebuild 命令）。查询 **MUST** 针对 FTS5 表使用 `MATCH`（或 FTS5 表上的 LIKE）；普通 content 表上的 LIKE **MUST NOT** 被假定走 trigram 索引（#20 实测：planner 不改写）。（来源：CONTEXT「可复用知识」、Issue #20 s4）
- **DATA-EMBED-001 —** Embedding 键 **MUST** 为 `(knowledge_version_id, embedding_model_generation)`；提交时 **MUST** 复核版本/generation 并丢弃迟到结果；`Pending/Failed` **MUST NOT** 撤销正式知识。状态与向量由 schema 强制一致（`ready` 必须有非空且 4 字节对齐的向量，`pending`/`failed` 必须无向量）；向量字节长度与 generation `vector_dim` 一致（触发器校验）。（来源：CONTEXT「可复用知识」）
- **DATA-EMBED-002 —** 换模型 **MUST** 完整构建新 generation、校验后原子切换（`ux_embedding_generation_current`）；一次 cosine 检索 **MUST NOT** 混用 generation；程序 **MUST NOT** 设阈值或固定融合排名。`embedding_generations` 的 model/version/generation **MUST** 不可变；`vector_dim` 一旦设置或该 generation 已有 embeddings **MUST** 不可变更（`trg_embedding_generations_origin_immutable`、`trg_embedding_generations_vector_dim_immutable`）。（来源：CONTEXT「可复用知识」）
- **DATA-SSE-001 —** `alert_change_log` **MUST** 追加且禁止 UPDATE（触发器），其 `id`（AUTOINCREMENT）即单调递增 `alert_change_seq`，在同一事务内由数据库分配、永不复用；HTTP 快照 `snapshot_seq` **MUST** 取当前最大 `alert_change_log.id`。（来源：CONTEXT「实时投影」）
- **DATA-SSE-002 —** 客户端 `after`/`Last-Event-ID` 游标小于保留窗口内最小 `id` 时 **MUST** 返回 `resync_required`；日志按部署配置保留有界窗口（GC 可 DELETE）；日志可丢弃、可重建，**MUST NOT** 成为告警历史权威源。（来源：CONTEXT「实时投影」）
- **DATA-SSE-003 —** 变更事件只携带 Occurrence ID、变化类型（`created`/`state_changed`）与 row version；客户端按 sequence 与 row version 幂等应用，详情在版本变化后重新读取。（来源：CONTEXT「实时投影」）
- **DATA-SSE-004 —** `task_change_log` **MUST** 是任务快照/SSE 的有界派生变更投影：Initial Analysis、Execution Attempt、Inspection Run/Report、Tool Call、Knowledge Import Batch/Candidate 与 Browser Operation 的创建与状态/阶段变化 **MUST** 在同一事务内写入 `object_type`/`object_id`/`row_version`/`change_type`（触发器保证）；其 `id`（AUTOINCREMENT）即单调递增 `task_change_seq`，**MUST NOT** 被复用；**MUST** 禁止 UPDATE，**MAY** 按保留窗口 DELETE（GC）；可丢弃、可重建，**MUST NOT** 成为任务历史权威源。（来源：CONTEXT「调查与巡检工作区」「后台任务提示」）
- **DATA-SSE-005 —** 所有可观察任务对象 **MUST** 携带 `row_version`：`initial_analyses`、`execution_attempts`、`inspection_runs`、`tool_calls`、`knowledge_import_batches`、`knowledge_candidates`、`browser_operations`（schema 已统一）；`row_version` **MUST** 由应用在每次状态 UPDATE 中显式递增，`expected_row_version` 的比较与递增在同一条 UPDATE 的 WHERE/SET 中完成（SQLite 触发器不支持 `SET NEW`）。（来源：CONTEXT「调查与巡检工作区」「领域写命令契约」）
- **DATA-SSE-006 —** 任务快照/SSE 客户端契约与告警一致：进入页面 **MUST** 先读 HTTP 快照（`snapshot_seq` 取当前最大 `task_change_log.id`）再建立 SSE；重连 **MUST** 使用 `Last-Event-ID`；游标过期 **MUST** 返回 `resync_required` 并重新读取完整快照；事件只传对象 ID、变化类型与 row version。（来源：CONTEXT「调查与巡检工作区」「实时投影」）

## 16. Artifact 与保留

- **DATA-ARTIFACT-001 —** Artifact 发布 **MUST** 依序执行：临时文件与最终文件同一文件系统 → 写入并校验 hash/大小 → `fsync` 临时文件 → 按 SHA-256 原子 rename → `fsync` 最终父目录 → 才提交 SQLite 引用事务（同一事务写入 `artifact_blobs` 与 `artifacts` 逻辑引用）；引用事务失败后 **MUST** 将文件作为孤儿清理。（来源：CONTEXT「Artifact 提交」、Issue #20 s6）
- **DATA-ARTIFACT-002 —** 每份内容 **MUST** 只有一个规范持久物理副本：`artifact_blobs.sha256` UNIQUE、`storage_key` 仅由 hash 推导；`artifacts` 通过 `blob_id` 引用物理副本，**MUST** 允许同一 blob 被多个逻辑 Artifact 以不同 `owner`/`kind`/`sensitive`/`retention` 引用（相同字节可同时是普通 attachment 与敏感 trace）。（来源：CONTEXT「产物」）
- **DATA-ARTIFACT-003 —** 访问、下载授权、保留与审计 **MUST** 按逻辑 `artifacts` 裁决：Raw Playwright trace **MUST** 固定 `sensitive=1`（`artifacts` CHECK 强制），不进入模型上下文、普通附件、FTS 或通用 read/grep，仅 Admin 经审计下载；生成型大 Artifact 默认保留 90 天（部署级配置），到期 **MUST** 保留元数据、SHA-256、来源、时间并置 `body_expired=1`；逻辑来源字段（`blob_id`/`kind`/`sensitive`/`retention_kind`/`owner`）**MUST** 不可改写（`trg_artifacts_origin_immutable`），逻辑 Artifact 元数据 **MUST NOT** 物理删除（`trg_artifacts_no_delete`），物理 blob 身份 **MUST** 不可改写（`trg_artifact_blobs_no_update`）。（来源：CONTEXT「在线保留」「浏览器操作记录」「产物」）
- **DATA-ARTIFACT-004 —** 物理 blob **MUST** 只在不再被任何逻辑 `artifacts` 引用（或全部引用已 `body_expired`）且与备份/GC 互斥时清理；单个逻辑引用过期 **MUST NOT** 删除仍被其他未过期引用需要的 blob；`body_expired` 是逻辑 Artifact 属性，不表示物理文件已删除。（来源：CONTEXT「一致备份」「产物」）
- **DATA-ARTIFACT-005 —** 撤回消息、停用系统或停止复用知识 **MUST NOT** 删除来源历史；结构化告警、调查、消息、诊断、报告、反馈、知识、Text Attachment 与 Import Batch 原文长期保留。（来源：CONTEXT「在线保留」）

## 17. 备份与恢复

- **DATA-BACKUP-001 —** 备份 **MUST** 通过独立空闲连接执行 `VACUUM INTO` 生成单文件一致快照，再从快照枚举被 `artifacts` 引用的 `artifact_blobs` 精确 hash 集合；Artifact GC 与复制阶段 **MUST** 互斥；复制校验后 **MUST** 以“临时写入并 fsync → 原子 rename → rename 后 fsync 父目录”耐久发布 manifest；任一引用缺失或目录同步失败整次失败。（来源：CONTEXT「一致备份」、Issue #20 s6/s7）
- **DATA-BACKUP-002 —** 新备份校验成功后才 **MUST** 清理超出保留份数（默认 30）的旧备份；保留选择 **MUST** 按有效 manifest 的 mtime 取最新，**MUST NOT** 按随机目录名。（来源：CONTEXT「一致备份」、Issue #20 s7 根因）
- **DATA-BACKUP-003 —** FTS5 shadow tables 与 embedding BLOB **MUST** 被视为随 `VACUUM INTO` 物理进入快照的派生数据（非恢复必需权威），恢复后 **MUST** 可校验、丢弃并重建。（来源：CONTEXT「一致备份」、Issue #2）
- **DATA-BACKUP-004 —** `integrity_check` **MUST NOT** 单独作为恢复门槛：中断的 `VACUUM INTO` 可能留下通过完整性检查但为空的快照（#20 s8 实测）；只有完整成功发布且校验通过的 manifest+checksum **MUST** 作为可恢复备份门槛。（来源：CONTEXT「一致备份」、Issue #20 s8）
- **DATA-BACKUP-005 —** 恢复后 **MUST** 保持维护状态并暂停调度：清除全部 Web Session；通过 TTY 选择恢复 Admin 并设临时密码；Runtime、Stele service token 与告警源 Bearer 全部失效并重新注册/轮换；Connection 密文保留但置 `revalidation_required=1`（DATA-CONN-003 阻断派发）；Browser Identity 置 `AuthenticationRequired`；Admin 检查后才退出维护状态。（来源：CONTEXT「一致备份」）
- **DATA-BACKUP-006 —** 备份目录只允许 Quoin UID 与部署操作者访问；恢复只由拥有 PVC/数据目录与根密钥权限的部署操作者在停机时执行；Admin 可浏览、显式下载和立即触发备份，下载审计。（来源：CONTEXT「一致备份」）
- **DATA-BACKUP-007 —** 备份记录（`backups`）**MUST** 追加且不可改写（`trg_backups_no_update`）；`status='succeeded'` **MUST** 带 `db_sha256`、`manifest_sha256` 与 `manifest_path`（CHECK 强制），`failed` 记录保留 `error_detail`。（来源：CONTEXT「一致备份」、Issue #20）

## 18. 迁移与 Schema 版本

- **DATA-MIGRATION-001 —** `contracts/sql/schema.sql` **MUST** 始终是当前完整 Schema 的唯一机器权威；当前没有已发布的旧 Schema，**MUST NOT** 创建虚构 migration 文件；未来 migration 只是“旧 Schema → 当前 Schema”的不可变过渡程序，**MUST NOT** 成为第二份当前 Schema 权威。（来源：Issue #9 Q9.2、Issue #8「机器契约目录」）
- **DATA-MIGRATION-002 —** 最新 v1 二进制 **MUST** 支持从任意此前正式发布的 v1 Schema 直接原地升级，顺序执行完整的不可变前向 migration 链（Q9.2 A）。（来源：[Issue #9](https://github.com/Suknna/quoin/issues/9) Q9.2）
- **DATA-MIGRATION-003 —** 每个 migration **MUST** 在独占数据库状态下单独原子提交；`migration_ledger` **MUST** 追加记录 migration ID 与 digest（追加触发器）；未知版本、新于程序的版本或 digest 不匹配 **MUST** 拒绝启动。（来源：Issue #9 Q9.2、CONTEXT「健康语义」）
- **DATA-MIGRATION-004 —** 迁移中途失败 **MUST** 保持 Not Ready，可从最后成功版本重试或恢复升级前备份；接受新写入后 **MUST NOT** 只回滚镜像，**MUST** 显式恢复升级前备份。（来源：CONTEXT「协调升级」）
- **DATA-MIGRATION-005 —** 迁移完成后的最终结构 **MUST** 与从当前 `contracts/sql/schema.sql` 干净创建的库在表/列/约束/索引上等价（DATA-VALIDATION-003 验证）。（来源：[Issue #9](https://github.com/Suknna/quoin/issues/9) Q9.2）
- **DATA-MIGRATION-006 —** `schema_state` 单行（`id=1`）记录当前 schema 版本与 digest；干净创建时 **MUST** 初始化 `schema_state` 与 `runtime_slots` 固定两行（`plinth`/`lintel`）；Quoin 取得数据目录锁并完成 migration 前 **MUST NOT** Ready。（来源：CONTEXT「健康语义」）

## 19. 事务程序与提交顺序裁决

**Non-normative：** SQLite WAL 单写者模型下，所有并发不变量最终依赖提交顺序；本文件要求写事务使用 `BEGIN IMMEDIATE` 并在同一事务内完成下列重验/裁决步骤。DATA-TX-001 至 DATA-TX-016 为规范条款，各自定义确定的事务步骤与提交顺序裁决规则。

- **DATA-TX-001 —** 所有领域写事务 **MUST** 以 `BEGIN IMMEDIATE` 获取写锁；状态竞争 **MUST** 按 SQLite 提交顺序裁决，**MUST NOT** 使用来源时间或墙钟决定先后。（来源：CONTEXT「告警观察」「告警源凭据投影」）
- **DATA-TX-002 —** 权限写事务提交前 **MUST** 在同一事务重验用户 `enabled`、`role`、`auth_revision`；账号变更（禁用/角色变化/改密）事务与业务写事务按提交顺序裁决：账号变更先提交则业务写冲突，业务写先提交则有效。（来源：CONTEXT「本地账号认证」）
- **DATA-TX-003 —** Delivery 事务 **MUST** 在提交前重验来源启用、凭据有效性与归属；吊销事务先提交则 Delivery 被拒绝且不创建 Occurrence，Delivery 先提交则有效。两事务按提交顺序裁决。（来源：CONTEXT「告警源凭据投影」）
- **DATA-TX-004 —** Attempt 派发事务 **MUST** 检查连接 `enabled` 与 `revalidation_required=0` 并绑定实际 revision/generation；停用或 RevalidationRequired 先提交则派发拒绝；已接受 Attempt 用内存旧快照完成。（来源：CONTEXT「连接」「执行尝试」、Issue #9 确认）
- **DATA-TX-005 —** 取消 fence 提交与成功结果提交按 SQLite 提交顺序裁决：成功先提交则取消返回已完成；取消（fence）先提交则迟到结果不产生有效消息/Report/Candidate，只留审计（DATA-ATTEMPT-003/004）。fence 与 transport detach 并发时，一旦 fence 提交任务 **MUST** 终态为 `Cancelled`，**MUST NOT** 停留在 `fenced` 中间态。（来源：CONTEXT「执行尝试」、Issue #18 原型证据）
- **DATA-TX-006 —** Undo 事务 **MUST** 在同一事务内完成依赖遍历（消息、工具调用、Evidence 引用、知识草稿）并取消 active Attempt；迟到模型输出到达时按 attempt 幂等拒绝，只写审计。（来源：CONTEXT「撤回消息」「知识候选」）
- **DATA-TX-007 —** Label Contract 多系统激活 **MUST** 在同一事务内校验全部系统兼容版本并原子切换契约 current 与全部兼容配置版本；任一失败 **MUST** 全部回滚。（来源：CONTEXT「标签契约」）
- **DATA-TX-008 —** 所有 current 指针切换（连接 revision/credential、配置版本、Knowledge current、Browser generation、Investigation head）**MUST** 在新版本插入后的同一事务内更新指针；任何时刻 **MUST** 恰有一个 current（或按状态机允许零个，如无配置草稿发布前）；指针指向的对象 **MUST** 属于同一聚合（schema 触发器校验归属）。（来源：CONTEXT「连接」「业务系统配置版本」「可复用知识」「浏览器身份」「撤回消息」）
- **DATA-TX-009 —** 定时 Run 创建 **MUST** 以 `(business_system_id, plan_key, scheduled_for)` 唯一键插入，同一事务绑定当时生效配置与 Label Contract；重复键 **MUST** 返回幂等结果或 `SkippedOverlap`，**MUST NOT** 并发创建两个 Run。（来源：CONTEXT「巡检运行」「领域写命令契约」）
- **DATA-TX-010 —** 批量确认 **MUST** 在单个事务内校验全部 Candidate 的 expected revision，全成或全不成；stale revision **MUST** 冲突并返回最新草稿。（来源：CONTEXT「知识导入批次」）
- **DATA-TX-011 —** 来源诊断反馈变为 `Rejected` 或来源回合被 Undo **MUST** 在同一事务内：将相关 Candidate 置 `SourceInvalid`、将相关 KnowledgeVersion 写入 `retrieval_state.exited=1` 并从 `knowledge_search_docs` 删除（触发器同步 FTS5）。（来源：CONTEXT「诊断反馈」「知识候选」「可复用知识」）
- **DATA-TX-012 —** Embedding 异步结果提交 **MUST** 复核 `(knowledge_version_id, embedding_generation_id)` 与版本状态，过期/迟到结果 **MUST** 丢弃只记审计。（来源：CONTEXT「可复用知识」）
- **DATA-TX-013 —** 影响告警列表的 Occurrence 变更与 `alert_change_log` 插入 **MUST** 处于同一事务（触发器保证；应用侧同事务写 `row_version` 等字段）。（来源：CONTEXT「实时投影」）
- **DATA-TX-014 —** 任何禁用、降级或删除 Admin 的命令 **MUST** 在同一事务校验剩余有效 Admin 数量 ≥ 1，否则 **MUST** 冲突。（来源：CONTEXT「Admin」）
- **DATA-TX-015 —** 物理清理 **MUST** 仅限可证明从未发布、从未执行 Test Run、从未被引用的草稿/staging；清理前 **MUST** 在事务中复核引用计数，清理后 **MUST** 写审计。（来源：CONTEXT「稳定身份保留」）
- **DATA-TX-016 —** DATA-TX-001 至 DATA-TX-015 的每个事务程序 **MUST** 在 `verification.md` 登记对应的对抗性交错用例（成功先提交/取消先提交、吊销与 Delivery、Undo 与迟到结果、激活部分失败等）。（来源：[Issue #9](https://github.com/Suknna/quoin/issues/9) 交付纪律）

## 20. 验证要求

- **DATA-VALIDATION-001 —** 本 Schema **MUST** 在锁定 modernc SQLite 构建（v1.56.0，SQLite 3.53.3，`ENABLE_FTS5`）与开发用 SQLite（3.50+）上均可完整执行，并 `PRAGMA integrity_check`、`PRAGMA foreign_key_check` 通过。（来源：[Issue #20](https://github.com/Suknna/quoin/issues/20)、[Issue #9](https://github.com/Suknna/quoin/issues/9)）
- **DATA-VALIDATION-002 —** 专项约束验证 **MUST** 覆盖：locator 不复用；命令唯一键；摘要长度；审计/证据/报告/知识版本/连接 revision/凭据 generation 追加不可变；active 部分唯一（IA、Attempt、Run）；定时 Run 唯一；指纹 8 字节；labels JSON；凭据最多两 Active 且不可复活；FTS5 增删改同步；检索退出粘性；FK RESTRICT 阻断删除；json_valid 拒绝非法 JSON；Evidence 正文位置唯一；强制改密状态一致性（标志与时间戳双向一致）；Session digest 长度 32 且唯一；`task_change_log` 与状态变化同事务、append-only、GC 可删除；同一 blob 被多个 owner/kind/retention 逻辑引用；trace 必须 `sensitive=1`；逻辑 Artifact 元数据不可删除；`artifact_blobs` 不可改写。（来源：[Issue #9](https://github.com/Suknna/quoin/issues/9) 交付纪律）
- **DATA-VALIDATION-003 —** 迁移验证 **MUST** 从每个已发布历史 fixture 升级到当前版本，并断言最终 `sqlite_master`（表/列/约束/索引/触发器）与当前 `schema.sql` 干净创建等价（DATA-MIGRATION-005）。（来源：[Issue #9](https://github.com/Suknna/quoin/issues/9) Q9.2）
- **DATA-VALIDATION-004 —** 备份/恢复验证 **MUST** 演练中断的 `VACUUM INTO`（kill -9）并证明 manifest+checksum 门槛拒绝损坏/空快照（DATA-BACKUP-004）。（来源：[Issue #20](https://github.com/Suknna/quoin/issues/20) s8）
