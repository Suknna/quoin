# Quoin v1 — 巡检配置与 Journey 机器 Schema（inspection-config.md）

**状态：Draft**

**CATEGORY 前缀：`CFG`**（SPEC-TRACE-002）

**Non-normative：** 本文件记录已批准、尚未部署或验收的统一业务声明迁移目标。它不声称已替换当前运行时、API、SQL 或历史执行记录；在实施切片接受前，历史 E2E 文档仍如实保留其当时所验证的系统。目标态中，独立的业务系统声明是业务范围、指标范围、告警来源归属、资源策略和巡检定义的唯一配置权威。

业务声明的唯一机器权威是 [`contracts/schemas/business-system.schema.json`](contracts/schemas/business-system.schema.json)，稳定 `$id` 为 `https://github.com/Suknna/quoin/schemas/business-system.schema.json`，并使用 `apiVersion: quoin/v1` 与 `kind: BusinessSystem`。JSON Schema 拥有字段形状；本文件只说明其不能机械表达的边界、发布和迁移语义。旧 `business-system-config.schema.json`、`label-contract.schema.json` 及涉及其“当前/激活 Label Contract”的文字均为历史材料，**MUST NOT** 被当作目标态的并列或唯一配置权威。

## 1. 范围与权威

- **CFG-SCOPE-001 —** 目标态业务声明的机器结构 **MUST** 只由 [`contracts/schemas/business-system.schema.json`](contracts/schemas/business-system.schema.json) 拥有；本文 **MUST NOT** 复制字段清单，**MUST** 通过该路径和稳定 `$id` 引用。Schema **MUST** 使用 JSON Schema draft 2020-12 和封闭对象来拒绝未知字段（SPEC-AUTHORITY-001/002、SPEC-VERSION-002）。（来源：ADR-0003）
- **CFG-SCOPE-002 —** 一个 `BusinessSystem` **MUST** 自身声明指标接入引用、强制指标 labels、资源定义及其允许指标、告警源引用和告警自身 labels、以及巡检。它 **MUST NOT** 依赖活动的全局 Label Contract；不存在可在系统外覆盖或放宽其业务范围的全局 label 语义。每个巡检检查 **MUST** 通过 `resourceRef` 绑定一个声明的资源，指标仅可使用该资源的 `allowedMetrics`。（来源：ADR-0003、[CONTEXT「业务系统」](../../../CONTEXT.md#业务系统)）
- **CFG-SCOPE-003 —** 业务声明 **MUST NOT** 内嵌接入地址、供应商配置、凭据、Cookie/profile 或其他秘密，也 **MUST NOT** 包含 Agent 提示词、`expect`/健康阈值、模板、环境变量、循环、按 ObservedResource 动态展开、用户脚本或 Playwright 代码。接入、浏览器身份和模型供应商仍在各自管理边界中配置。（来源：ADR-0003）
- **CFG-SCOPE-004 —** 迁移尚未部署或接受。任何未来实现 **MUST** 同步迁移 Schema、解析/编译、持久化、HTTP、运行时与测试；在此之前不得以此文档宣称运行系统已采用目标态，也不得改写既有 E2E 证据。（来源：ADR-0003）

## 2. 严格 YAML 解析

- **CFG-YAML-001 —** 目标态只接受一个严格 UTF-8 YAML `BusinessSystem` 文档：先以 `yaml.v3` 解码为 `yaml.Node` 并执行词法/结构检查，再转换为规范 JSON，由 `business-system.schema.json` 校验字段形状和未知字段；**MUST NOT** 维护与 Schema 竞争的字段定义。解析器和 Schema 版本随接受的声明保存。（来源：ADR-0003）
- **CFG-YAML-002 —** 解析 **MUST** 拒绝多文档、重复 mapping key、anchor/alias/merge key、自定义 tag、非字符串字段名、尾随内容以及超过部署输入、AST 节点或嵌套深度上限的输入。错误 **MUST** 按字段路径报告；未知字段只由 Schema 的封闭对象规则拒绝。（来源：ADR-0003）
- **CFG-YAML-003 —** Schema 之外的校验 **MUST** 在解析投影上进行：时区必须由 IANA 数据库识别，巡检 schedule 必须由锁定的五字段 cron 解析器识别，稳定名称只在其真实父作用域唯一，identity labels 不得重复，且已发布、执行或引用的稳定名称不得重新分配。（来源：[CONTEXT「稳定身份保留」](../../../CONTEXT.md#稳定身份保留)、ADR-0003）

## 3. 业务系统配置文档

- **CFG-CONFIG-001 —** `BusinessSystem` 是同一版本化权威声明；任何将来提供的表单和 YAML 视图 **MUST** 生成、校验和发布相同版本，且不得创建竞争性元数据路径或秘密副本。指标 `connectionRef` 必填；未知、停用、无权或歧义引用必须拒绝，执行不得选择全局或首项接入。（来源：ADR-0003）
- **CFG-CONFIG-002 —** Schema 中的每个资源是稳定的资源范围：其 labels 与根指标 labels 合并，`discoveryMetric` 受该资源 `allowedMetrics` 限制，`identityLabels` 参与资源身份。资源发现只能由业务任务按需执行；验证或试运行只能留下其 Run 证据，不得覆盖正式 Observed Resource。（来源：[CONTEXT「观测资源」](../../../CONTEXT.md#观测资源)、ADR-0003）
- **CFG-REFRESH-001 —** 不得重新引入独立资源刷新、手动无归属扫描或接入/页面/发布/启动时采集。退役不删除历史声明版本、Run、Attempt 或 Observed Resource；已受理历史执行仍按其既有终态规则收口。（来源：[CONTEXT「观测资源」](../../../CONTEXT.md#观测资源)、ADR-0003）
- **CFG-CONFIG-003 —** Schema 拥有巡检及检查结构。每个检查 **MUST** 通过 `resourceRef` 指向同一声明的资源，并使用该资源允许的指标；表达式和问题是字面量，不得模板化或按 ObservedResource 动态展开。当前 Schema 仅声明 PromQL 检查；浏览器检查的目标态形状必须在实施时作为 Schema 变更单独接受。（来源：ADR-0003）
- **CFG-CONFIG-004 —** Kubernetes 运行时状态 **MUST NOT** 出现在业务声明或巡检配置中；它只供人工调查按需查询。（来源：[CONTEXT「Kubernetes 运行时状态」](../../../CONTEXT.md#kubernetes-运行时状态)、ADR-0003）
- **CFG-INSPECTRUN-001 —** 手工或调度 Inspection Run 的每个 check **MUST** 走与 Config Verification 同一机械采集合同：PromQL check 创建 `scope_type='run_check'` 的 `inspection_collection` 子 Attempt，Quoin 冻结 `inspection_promql_execution_v1` 与唯一 `config_thanos_query` grant 并派发 Plinth supervisor；Browser check 冻结 `inspection_collection_v1` 并派发 Lintel。PromQL ResultProposal 使用 `inspection_promql_result_v1`；success 的非 Artifact typed payload 必须是封闭 Prometheus `resultType`（`vector|matrix|scalar|string`）及相应非空 sample/series；range 同时返回实际 start/end/step，Quoin 按冻结 `evidenceAt`、rangeSeconds 与 stepSeconds 重验后原子写入同 Attempt 的完整 Evidence，error/gap 不制造 Evidence；所有路径同受 Attempt boot/epoch/cancel fence 裁决，Quoin 不解密或执行 PromQL。（来源：Issue #66、DATA-INSPECT-003、RUNTIME-TASK-003/011/012）
- **CFG-CRON-001 —** `cron` 缺省表示仅人工运行；存在时 **MUST** 恰为五个空白分隔字段，并通过 `github.com/robfig/cron/v3` v3.0.1 `ParseStandard`。`@every`/`@daily` 等 descriptor、秒字段和内嵌 `CRON_TZ`/`TZ` **MUST** 拒绝；时区只取配置根 `timezone`。（来源：CONTEXT「巡检计划」「业务系统配置版本」）

## 4. 历史 Label Contract

**Non-normative：** `label-contract.schema.json`、其上传/激活流程和依赖“当前 Label Contract”的旧条款只记录此前模型，不能作为目标态配置或标签语义的唯一权威。保留它们是为了正确解读历史版本、运行与 E2E 证据，不表示该迁移已经完成。

- **CFG-CONTRACT-001 —** 目标态 **MUST NOT** 创建、激活或解析全局 Label Contract，也 **MUST NOT** 要求 `targetLabelContractVersion`。业务和资源范围由每份 `BusinessSystem` 的 `metrics.matchLabels` 与资源 `matchLabels` 确定；告警归属由同一声明的 `alerts.matchLabels` 确定。（来源：ADR-0003）
- **CFG-CONTRACT-002 —** 旧 Label Contract 的历史记录、版本和已接受执行 **MUST** 按既有保留规则可读；它们 **MUST NOT** 被新的声明或运行时重新解释为目标态的活动全局契约。（来源：ADR-0003）

## 5. PromQL 语义校验

- **CFG-PROMQL-001 —** `discoveryMetric` 与每个检查 `expression` **MUST** 使用 Prometheus 官方 AST 解析器校验；不得用正则或字符串替换代替语义验证。（来源：ADR-0003）
- **CFG-PROMQL-002 —** 每个查询中的 VectorSelector **MUST** 使用声明资源合并后的强制 `matchLabels`；缺失时实现可以 AST 注入，出现同名但不精确相等的 matcher 则必须拒绝。业务系统标签名和值来自该 BusinessSystem 声明本身，不来自全局 Label Contract。（来源：ADR-0003）
- **CFG-PROMQL-003 —** 每个 VectorSelector 的指标名 **MUST** 符合目标 `resourceRef` 的 `allowedMetrics` 精确名或非空前缀通配符；一个表达式中的所有指标均受此限制。资源发现以该资源的 `discoveryMetric` 及合并 labels 建立当前范围，不得把历史或合成结果伪装为当前资源。（来源：ADR-0003）
- **CFG-PROMQL-004 —** range 查询执行 **MUST** 以真实开始采证的 `evidence_at` 为终点，保存实际 start/end/step（`range_seconds`/`step_seconds` 字面量）；校验与执行分离：上传只校验 AST 与归属，实际查询在采证时执行（DATA-CONFIG-003、CONTEXT「巡检项」）。（来源：CONTEXT「巡检项」「巡检运行」）

## 6. Journey Catalog

- **CFG-JOURNEY-001 —** Journey Catalog **MUST** 由 Lintel 中版本化 Playwright Journey 的同一机器来源生成，构建期同时嵌入 Quoin 与 Lintel。每个 entry **MUST** 声明稳定 `journey_id`、实现 `version`、不可变 steps digest、`journey|authentication_probe` purpose、封闭参数 Schema、封闭输出 Schema 与 Evidence kind 集合；普通 Journey 的集合非空且必须包含 `structured`，用于承载唯一 typed output，authentication probe 的集合固定为空且三态结果只进入 probe ledger；catalog 是生成产物，**MUST NOT** 暴露用户脚本或第二执行 DSL。（来源：CONTEXT「Journey Catalog」、Issue #14）
- **CFG-JOURNEY-002 —** catalog 生成对象 **MUST** 满足 I-JSON，并按 RFC 8785 JCS 生成 UTF-8 文件；digest 为原始字节 SHA-256。相同输入逐字节相同，Quoin/Lintel 嵌入同一文件且 `Hello.journey_catalog_digest` 严格相等，否则 `CATALOG_MISMATCH` 且 Lintel 不 Ready；配置版本与 Browser Identity Revision 保存创建时 provenance，Browser Operation 保存本次实际执行 digest/version；Run/Config Verification Run 不复制第二份 execution binding。（来源：CONTEXT「Journey Catalog」「协调升级」、Issue #14）
- **CFG-JOURNEY-003 —** Quoin **MUST** 在 Lintel 离线时静态校验所有 Journey 引用：稳定 ID 存在，缺省参数规范化 `{}` 后通过该 entry 的封闭 `params_schema`。Lintel 的 `browser_journey_result_v1` 只能携带 catalog 声明的输出：`outcome=success` 必须恰有一个 `primary=true` 的 structured Evidence proposal，其 `content` 通过同 entry `output_schema` 并作为 typed output 的唯一正文权威；`outcome=gap` 的 `evidence` 必须为空，只提交封闭 gap code、与 SQL 一致的 terminal reason、detail 与按完整 ResultProposal 计算的 digest，不制造占位 Evidence；若强制诊断 trace 提交失败，外层 gap 为 `artifact_commit_failed` 且必须另存 `originalGapCode=journey_failed`，不得覆盖原 step 失败分类。截图 proposal 只能引用已提交 Artifact，Lintel 不分配 Evidence ID。Quoin 按 operation 实际冻结的 catalog digest、Journey ID/version 和 output schema 重验 success payload，先创建 Evidence，再以不可变 `browser_journey_results` INSERT 作为唯一提交入口；该 ledger 保存 digest/outcome，success 另引用 primary Evidence，并由 SQL trigger 同一 statement 派生 check result、收口 operation 与 Attempt。未知参数/输出、完整 HTML dump、未声明自由 JSON 或 Evidence kind **MUST** 拒绝。生成管线必须真实编译每个 draft 2020-12 nested Schema，并拒绝外部/动态 ref 与非法 required。（来源：CONTEXT「Journey Catalog」、Issue #14 Q14.21）
- **CFG-JOURNEY-004 —** browser check 必须经业务声明中显式授权的浏览器接入引用运行；浏览器身份独立于业务配置创建，默认不跨业务复用。业务声明/YAML 不携带 profile、Cookie、密码或导航 allowlist；具体引用字段待 #95 实施时与 Schema、OpenAPI、SQL 和 Runtime 契约一起定义。Browser Identity Revision 继续使用版本化 authentication probe 与类型化参数，Journey 从接入身份的起始 URL 开始，应用层不复制网络策略。只有 Admin 管理浏览器身份与人工登录；Operator 只能在告警与 AI SRE 的授权上下文使用其工具结果，不能直接管理该接入或身份。（来源：[CONTEXT「Operator」「Admin」「浏览器身份」](../../../CONTEXT.md#人类角色)、[#95](https://github.com/Suknna/quoin/issues/95)、ADR-0002）
- **CFG-JOURNEY-005 —** 稳定 Journey ID 是行为契约：相邻正式 catalog **MUST** 保留旧 ID；参数 Schema、输出 Schema、purpose 或 Evidence kind 的任何变化必须使用新 ID。兼容实现修复可保持 ID，但 `version` 必须单调增加且 steps digest 改变；构建门机械拒绝 version 不递增、旧 ID 删除或同 version 改写 steps digest。revision/config version 保存的是创建时静态校验 catalog provenance；协调升级后，新 operation 对同一稳定 ID 自动采用当前 ready catalog 的兼容实现 version 并冻结实际 binding，不要求重建所有既有 revision。程序只验证这些结构事实，不判断修复是否保持业务语义。（来源：CONTEXT「Journey Catalog」、Issue #14）
- **CFG-JOURNEY-006 —** Journey **MUST NOT** 自动重跑整单或从中间步骤恢复；Playwright locator 固定 deadline 内等待不是重试。唯一的就绪例外是固定 authentication probe 对 `startUrl` 的**首次、尚未 HTTP commit** 的 CDP `page.goto` 超时：可重试一次，并在 trace 记录 `probe_navigation_readiness_retry`；不得重试已 commit 的导航、任何 Journey step、selector/action 或整个 Journey。失败创建本次 check 的结构化 gap 与强制 trace；再次采证只能由新 Run/Config Verification Run 和新 Attempt 发起。authentication probe 输出 Schema 必须封闭为 `Authenticated|Unauthenticated|Indeterminate` 三态，技术错误不得映射为未登录。（来源：Issue #14 Q14.7/Q14.20/Q14.21；T23 Chromium CDP readiness evidence）

## 7. 上传、发布与配置验证 Run

- **CFG-PUBLISH-001 —** 目标态发布顺序为：声明完成静态校验 → 可选验证 Run → 发布命令原子切换 current 指针。表单和 YAML 均编辑同一不可变候选版本；`current` 只指已发布版本，且不得建立“当前/最新草稿”第二指针。启停仍通过发布声明中的 `enabled` 完成。此为未来迁移语义，尚未宣称当前 API 或持久化已提供该流程。（来源：ADR-0003）
- **CFG-VERIFYRUN-001 —** 目标态 Config Verification Run 精确绑定一份不可变候选 BusinessSystem 声明，而不绑定全局 Label Contract。它验证该声明所引用的指标接入、资源范围和检查，并将 discovery/query 结果保留为该 Run 的证据，不覆盖正式 Observed Resource。具体状态机、HTTP 和持久化字段必须随迁移实现同步接受；本条不得被解释为这些变更已部署。（来源：ADR-0003）
- **CFG-VERIFYRUN-002 —** Config Verification Run **MUST** 使用与巡检一致的机械采集路径：每个 PromQL 检查创建 `scope_type='config_verification_run'` 的 `inspection_collection` 子 Attempt，冻结 `config_thanos_query` grant 并派发 Plinth supervisor；每个 Resource Discovery 同样创建该 scope 的子 Attempt，冻结 `config_verification_discovery_execution_v1` 输入与同一受选定接入约束的 grant。Quoin 不解密或执行 PromQL，supervisor 只能经 `FetchCredentialGrant` 取得该 Attempt 的短期指标凭据（ARCH-WORKER-002、DATA-CONN-002/008、RUNTIME-GRANT-001）。Discovery success 只把 discovery key、identity labels 与查询证据写回该 Config Verification Run，**MUST NOT** 写入 Observed Resource。浏览器检查仍派发 Lintel（DATA-BROWSER-003、RUNTIME-TASK-003）；在 Lintel 浏览器执行器落地前，含 browser 检查的草稿在创建命令中确定性拒绝（HTTP 503），**MUST NOT** 创建永不收敛的 Run 占用 active fence。每 check 结果写入 `config_verification_run_check_results`（以 `plan_key + check_key` 复合定位；每项均绑定精确子 Attempt；PromQL 已接受的 success 以该 Running Attempt 绑定完整 Evidence，error/gap 以密封 ResultProposal 的 digest/gap code 记录；Browser success/gap 必须通过 `browser_journey_results` 单一入口绑定子 Attempt、operation、result digest 与唯一 primary structured Evidence。没有 ResultProposal 的技术终止才使用 Failed/Cancelled/Interrupted Attempt 与无 Evidence/digest gap）。Evidence 必须指向本 Config Verification Run 且 `params_json.plan_key/check_key` 精确匹配（DATA-CONFIG-007）。（来源：[CONTEXT「观测资源」](../../../CONTEXT.md#观测资源)、CONTEXT「巡检项」「执行尝试」「浏览器操作记录」、[#97](https://github.com/Suknna/quoin/issues/97)）
- **CFG-EXPORT-001 —** 目标态允许导出候选或已发布的 BusinessSystem 原文，并提供不含凭据的 YAML 起始模板。历史 Label Contract 导出不构成目标态配置入口；Journey Catalog 仍不是用户输入。具体 HTTP operation 必须在迁移实现时接受。（来源：ADR-0003）

## 8. 验证要求

- **CFG-VALIDATION-001 —** `business-system.schema.json` **MUST** 由真实 draft 2020-12 校验器以正反例矩阵验证：有效 `quoin/v1`/`BusinessSystem`、封闭对象、必填 metadata/metrics/alerts/inspections、资源 labels 与允许指标、告警自有 labels、`resourceRef`，以及 instant/range 检查字段关系。反例必须覆盖未知字段、错误 apiVersion/kind、非法 label 或 metric、空/开放 labels、非法 resourceRef 与 range 缺少边界。（来源：ADR-0003）
- **CFG-VALIDATION-002 —** strict YAML 解析 **MUST** 以真实 `yaml.v3` `yaml.Node` 行为逐项验证：重复 key、anchor/alias/merge、自定义 tag、非字符串字段名、第二文档、尾随内容、超限（输入字节/AST 节点/深度）全部拒绝，合法单文档通过；未知字段拒绝由 CFG-VALIDATION-001 的真实 JSON Schema 反例证明。（来源：Issue #12 交付纪律）
- **CFG-VALIDATION-003 —** PromQL 校验 **MUST** 以官方 parser 验证：合法表达式通过；非法语法拒绝；VectorSelector 缺业务系统 label / 非精确 `=` / 值不等于 system key 拒绝；discovery selector 含 `offset`/`@`/聚合/`label_replace`/子查询拒绝；check expression 允许 `offset`/`@`/子查询（只需通过 AST 与归属校验）。（来源：Issue #8 交付纪律）
- **CFG-VALIDATION-004 —** SQL 投影 **MUST** 以 SQLite harness 验证：根投影列持久化与 `system_key` 匹配触发器、发布投影同步（同一事务、`row_version` 递增）、check 类型化 CHECK（instant/range 组合、跨 kind 排斥、零/负 range/step 拒绝）、`config_verification_runs` 生命周期（active 唯一、终态不可变、来源不可改、row_version 精确 +1、check 结果条件约束）、`scope_type='config_verification_run'` 与 journey 浏览器操作绑定（DATA-VALIDATION-002）。（来源：Issue #12 交付纪律）
- **CFG-VALIDATION-005 —** catalog 验证 **MUST** 覆盖：生成器拒绝重复稳定 ID；每个嵌套参数 Schema 通过 draft 2020-12 metaschema 编译且 required 只引用已声明参数；Journey 参数正反例按对应嵌套 Schema 执行；相同输入的两次独立生成产出完全相同的文件字节与 digest；Quoin/Lintel 嵌入字节相等；相邻正式 v1 catalog 的旧 ID/参数契约兼容门；`Hello.journey_catalog_digest` 不匹配拒绝（RUNTIME-VALIDATION-002）。（来源：Issue #12 验收条件）


### CFG-INSPECTRUN-002 — Immutable report closure

Mixed collection 已收口后，Quoin 以冻结的 config version/plan 和准确的 collection Evidence 集创建一次 `inspection_analysis`。模型只能撰写报告内容和选择冻结 locator；程序不预设健康 verdict 或 severity。`inspection_report_result_v1` 的 locator、payload digest 与 Evidence digest 由 Quoin 重验，并经单一 SQLite closure 形成不可修改的 Report。collection gap 仍是报告可见事实，不会删除其它 check 的 Evidence；分析失败不会产生占位 Report。
