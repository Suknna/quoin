# Quoin v1 — 巡检配置与 Journey 机器 Schema（inspection-config.md）

**状态：Draft**

**CATEGORY 前缀：`CFG`**（SPEC-TRACE-002）

**Non-normative：** 本文件承载业务系统 YAML、Label Contract、Resource Discovery、Inspection Plan/Check、PromQL 校验、浏览器 Journey 引用与 Journey Catalog 的文档形状、严格解析、静态校验、发布、Config Verification Run 与导出语义。机器可表达的结构约束只由 [`contracts/schemas/`](contracts/schemas/) 下的 JSON Schema 唯一拥有（SPEC-AUTHORITY-001/002）：

| 文档 | 唯一机器权威 |
| --- | --- |
| 业务系统配置（parsed YAML shape） | [`contracts/schemas/business-system-config.schema.json`](contracts/schemas/business-system-config.schema.json) |
| Label Contract（parsed YAML shape） | [`contracts/schemas/label-contract.schema.json`](contracts/schemas/label-contract.schema.json) |
| Journey Catalog（构建期生成 JSON artifact） | [`contracts/schemas/journey-catalog.schema.json`](contracts/schemas/journey-catalog.schema.json) |

本文件只拥有无法由 JSON Schema 表达的语义：strict YAML 词法/解析限制、PromQL 官方 AST 语义校验、Journey Catalog 生成与嵌入、上传/发布/Config Verification Run 事务语义。持久化投影与事务约束见 `persistence.md`（DATA-CONFIG-001..008）；HTTP 面见 `http-api.md`（HTTP-CONFIG-001..005）与 `contracts/openapi.yaml`；Lintel 握手 catalog digest 校验见 `runtime-protocol.md`（RUNTIME-CTRL-010）。来源为 [`CONTEXT.md`](../../../CONTEXT.md) 稳定标题（「标签契约」「业务系统」「观测资源」「指标查询连接」「巡检计划」「巡检项」「业务系统配置版本」「Journey Catalog」）。

## 1. 范围与权威

- **CFG-SCOPE-001 —** 三种人类可编辑/构建期文档的机器结构 **MUST** 只由上表 `contracts/schemas/*.schema.json` 拥有；本文件 **MUST NOT** 复制字段清单，**MUST** 以相对路径与稳定 `$id` 引用；三个 Schema **MUST** 使用 JSON Schema draft 2020-12 与稳定 `$id`（SPEC-VERSION-002）。（来源：Issue #8「权威源与事实所有权」）
- **CFG-SCOPE-002 —** 业务系统配置与 Label Contract **MUST NOT** 包含：任何连接/供应商/地址/凭据/秘密、任意 Agent 提示词、`expect`/断言/健康阈值规则、模板/环境变量/循环/按 ObservedResource 动态展开、绝对导航 URL（Journey 只给类型化参数或相对路径）、用户脚本或 Playwright 代码（CONTEXT「巡检项」「指标查询连接」「浏览器身份」）。Schema 以封闭对象（`additionalProperties: false`）机械拒绝未知字段。（来源：CONTEXT「巡检项」「浏览器身份」）
- **CFG-SCOPE-003 —** 每次上传 **MUST** 只解析一次（CFG-YAML-001）并持久化原文、parser/schema 版本、digest 与全部类型化投影；运行（刷新、Config Verification Run、定时巡检）**MUST** 只使用持久化类型结构，**MUST NOT** 重新解析 YAML（DATA-CONFIG-003）。（来源：CONTEXT「业务系统配置版本」）

## 2. 严格 YAML 解析（业务系统配置与 Label Contract 共用）

- **CFG-YAML-001 —** 两种人类输入文档 **MUST** 使用同一严格单文档 YAML 解析机制：先以 `yaml.v3` 解码为 `yaml.Node` 并逐节点执行下述 YAML 词法与结构检查，再转换为规范 JSON（`contracts/schemas/*.schema.json` 的 parsed shape）并由 JSON Schema 校验字段形状与未知字段；**MUST NOT** 手工维护一份与 Schema 竞争的 Go struct 字段模型。解析器与 Schema 版本 **MUST** 随文档持久化（`parser_version`/`schema_version`）。（来源：CONTEXT「业务系统配置版本」）
- **CFG-YAML-002 —** YAML 结构解析 **MUST** 拒绝：非单个 UTF-8 文档（多文档/`---` 第二文档）；重复映射 key（同层同 key 两次声明）；anchor/alias 与 merge key（`&`/`*`/`<<`）；自定义 tag（`!foo`）；非字符串字段名（数字/布尔/null 键）；尾随内容；超过部署配置上限的输入字节数、AST 节点数与嵌套深度（默认 10 MiB / 100k 节点 / 128 层，HTTP-FILE-002）。未知字段只由对应 JSON Schema 的封闭对象拒绝（CFG-SCOPE-002），不得在 YAML 层复制字段清单。错误 **MUST** 以 `fieldErrors`（`path`/`reason`/`remediation`）逐项返回（HTTP-ERROR-001）。（来源：CONTEXT「业务系统配置版本」）
- **CFG-YAML-003 —** 语义校验（Schema 之外）**MUST** 在解析投影上执行并逐项报告：时区值必须由运行环境的 IANA 时区数据库解析；调度表达式必须由项目锁定的标准五字段 cron 解析器接受，缺省表示仅人工运行；同类稳定 key 必须在其真实父作用域内唯一。Config Verification Run 结果与子 Attempt 以 `plan_key + check_key` 复合定位检查项；check key 只在所属 plan 内唯一，不同 plan 与不同领域类型之间不施加虚假的共享命名空间。身份 label 不得重复；已发布、已执行或已引用的稳定 key 不得重新分配（DATA-CONFIG-004）。（来源：CONTEXT「巡检计划」「业务系统配置版本」「稳定身份保留」）

## 3. 业务系统配置文档

- **CFG-CONFIG-001 —** 根字段与子对象形状只由业务系统配置 Schema 拥有。根稳定 key 跨版本不可改写；显示状态、时区与统一刷新周期在发布时成为 Business System 的不可变版本投影。首次上传一个尚不存在的稳定 key 时，Quoin **MUST** 在同一事务创建 Disabled Business System 与第一份草稿；不得要求先通过独立表单创建系统。（来源：CONTEXT「业务系统配置版本」「观测资源」）
- **CFG-CONFIG-002 —** Resource Discovery 的结构由 Schema 拥有；`resource_discoveries[].key` 在该数组内必须唯一。语义上每项表示一个稳定身份的当前资源发现规则，刷新周期统一取配置根值。其 selector 必须满足 CFG-PROMQL-002 的即时 VectorSelector 约束，身份 labels 参与 DATA-OBSERVED-001 的规范身份编码。（来源：CONTEXT「观测资源」「业务系统配置版本」）
- **CFG-REFRESH-001 —** 已发布配置的 Resource Discovery **MUST** 由独立 `resource_refresh_runs` 执行：scheduler 以根刷新周期创建 schedule Run，Admin 可手动创建 Run；每一 discovery 创建 `scope_type='resource_refresh_run'`、`discovery_key` 精确绑定的 `inspection_collection` 子 Attempt。Quoin 冻结当前已发布 config/Label Contract 与 `config_thanos_query` grant，但不解密或执行 PromQL；Plinth supervisor 只能以 `FetchCredentialGrant` 取得短期 Thanos 凭据并执行 instant selector。Run/Attempt、部分成功、取消与 stale 语义由 DATA-OBSERVED-002..004 和 RUNTIME-TASK-001..012 机械裁决；不得把刷新伪装成 Config Verification check。（来源：Issue #41 A 决策、CONTEXT「观测资源」）
- **CFG-CONFIG-003 —** Plan 与 Check 的封闭结构由 Schema 拥有；`inspection_plans[].key` 在计划数组内唯一，每个计划的 `checks[].key` 在该计划内唯一；不同作用域之间允许复用相同 key，不建立跨类型唯一性。计划可不调度且可暂时没有检查项；PromQL 检查保存字面量查询及执行窗口，Browser 检查引用嵌入 Catalog 中的稳定 Journey 并按其参数 Schema 验证。两类检查都不得引入模板、环境变量、循环或按 ObservedResource 动态展开；多个目标显式写成多条检查。（来源：CONTEXT「巡检计划」「巡检项」「Journey Catalog」）
- **CFG-CONFIG-004 —** Kubernetes 运行时状态 **MUST NOT** 出现在巡检配置中（只供人工调查按需查询，CONTEXT「Kubernetes 运行时状态」「巡检项」）。（来源：CONTEXT「Kubernetes 运行时状态」）
- **CFG-CRON-001 —** `cron` 缺省表示仅人工运行；存在时 **MUST** 恰为五个空白分隔字段，并通过 `github.com/robfig/cron/v3` v3.0.1 `ParseStandard`。`@every`/`@daily` 等 descriptor、秒字段和内嵌 `CRON_TZ`/`TZ` **MUST** 拒绝；时区只取配置根 `timezone`。（来源：CONTEXT「巡检计划」「业务系统配置版本」）

## 4. Label Contract

- **CFG-CONTRACT-001 —** Label Contract 人类输入 **MUST** 为 strict YAML-only（Q12.1 B）：与业务系统配置共用 CFG-YAML-001/002/003 的同一解析机制与上限；`createLabelContractDraft` 为 multipart YAML 上传（HTTP-CONFIG-004），**MUST NOT** 接受 JSON 输入；`downloadLabelContractTemplate` 输出 `application/yaml`。（来源：Issue #12 Q12.1 决策）
- **CFG-CONTRACT-002 —** parsed shape 为 `label_contract.business_system_label`（Prometheus label 名，`contracts/schemas/label-contract.schema.json`）；第一版只统一配置业务系统归属 label 名；告警归属、资源发现、巡检 PromQL 校验、资源关联与页面筛选共同引用同一版本契约（CONTEXT「标签契约」）。（来源：CONTEXT「标签契约」）
- **CFG-CONTRACT-003 —** 配置上传 **MUST** 携带显式目标 Label Contract 版本（`targetLabelContractVersion`），校验 **MUST** 针对该版本进行，**MUST NOT** 静默使用当前激活契约（DATA-CONFIG-002/003）；契约 `retired` 不可作为目标（DATA-CONFIG-002）；契约状态（draft/active/retired）是 `label_contract_state` 指针的派生镜像，上传只以 `version` 标识目标。（来源：CONTEXT「标签契约」）
- **CFG-CONTRACT-004 —** 联合激活语义（零启用系统静态激活 / 全系统兼容版本同事务原子切换 / Config Verification Run 证据）由 DATA-CONFIG-002/006/007 与 HTTP-CONFIG-002/005 拥有；本文件不重复。（来源：CONTEXT「标签契约」）

## 5. PromQL 语义校验

- **CFG-PROMQL-001 —** 所有 `expression` 与 discovery `selector` **MUST** 在上传时以 Prometheus 官方 Go parser（`github.com/prometheus/prometheus/promql/parser`，与部署锁定版本一致的 AST）解析校验，**MUST NOT** 使用正则或字符串改写做语义校验；解析失败、未知函数/聚合器 **MUST** 以 `fieldErrors` 拒绝（422）。（来源：CONTEXT「指标查询连接」）
- **CFG-PROMQL-002 —** 每个 VectorSelector **MUST** 满足目标 Label Contract 的归属约束：selector 内必须存在 `business_system_label = "<system_key>"` 的精确 `=` 匹配（label 名与值均来自目标契约版本与当前 system key；AST 检查 `VectorSelector.LabelMatchers`，`MatchEqual` 且值等于 system key；其它 label 匹配任意）；缺失或不等价（`!=`/`=~`/`!~`）**MUST** 拒绝（CONTEXT「指标查询连接」）。（来源：CONTEXT「指标查询连接」「标签契约」）
- **CFG-PROMQL-003 —** discovery `selector` **MUST** 是单个 instant VectorSelector：AST 必须是 `VectorSelector` 本身，禁止 `offset`/`@` 修饰、聚合表达式（`sum`/`avg` 等）、`label_replace`/`label_join` 与子查询（`[x:y]`）——防止历史/合成结果伪装为当前资源（CONTEXT「观测资源」「指标查询连接」）。check 的 `expression` 允许 `offset`/`@` 修饰与子查询（不在 Discovery 范围限制内；CONTEXT「巡检项」只约束字面量语义与归属，不额外限制 PromQL 功能子集）。（来源：CONTEXT「观测资源」「指标查询连接」「巡检项」）
- **CFG-PROMQL-004 —** range 查询执行 **MUST** 以真实开始采证的 `evidence_at` 为终点，保存实际 start/end/step（`range_seconds`/`step_seconds` 字面量）；校验与执行分离：上传只校验 AST 与归属，实际查询在采证时执行（DATA-CONFIG-003、CONTEXT「巡检项」）。（来源：CONTEXT「巡检项」「巡检运行」）

## 6. Journey Catalog

- **CFG-JOURNEY-001 —** Journey Catalog **MUST** 由 Lintel 中版本化 Playwright Journey 的同一机器来源生成，构建期同时嵌入 Quoin 与 Lintel。每个 entry **MUST** 声明稳定 `journey_id`、实现 `version`、不可变 steps digest、`journey|authentication_probe` purpose、封闭参数 Schema、封闭输出 Schema 与 Evidence kind 集合；普通 Journey 的集合非空且必须包含 `structured`，用于承载唯一 typed output，authentication probe 的集合固定为空且三态结果只进入 probe ledger；catalog 是生成产物，**MUST NOT** 暴露用户脚本或第二执行 DSL。（来源：CONTEXT「Journey Catalog」、Issue #14）
- **CFG-JOURNEY-002 —** catalog 生成对象 **MUST** 满足 I-JSON，并按 RFC 8785 JCS 生成 UTF-8 文件；digest 为原始字节 SHA-256。相同输入逐字节相同，Quoin/Lintel 嵌入同一文件且 `Hello.journey_catalog_digest` 严格相等，否则 `CATALOG_MISMATCH` 且 Lintel 不 Ready；配置版本与 Browser Identity Revision 保存创建时 provenance，Browser Operation 保存本次实际执行 digest/version；Run/Config Verification Run 不复制第二份 execution binding。（来源：CONTEXT「Journey Catalog」「协调升级」、Issue #14）
- **CFG-JOURNEY-003 —** Quoin **MUST** 在 Lintel 离线时静态校验所有 Journey 引用：稳定 ID 存在，缺省参数规范化 `{}` 后通过该 entry 的封闭 `params_schema`。Lintel 的 `browser_journey_result_v1` 只能携带 catalog 声明的输出：`outcome=success` 必须恰有一个 `primary=true` 的 structured Evidence proposal，其 `content` 通过同 entry `output_schema` 并作为 typed output 的唯一正文权威；`outcome=gap` 的 `evidence` 必须为空，只提交封闭 gap code、与 SQL 一致的 terminal reason、detail 与按完整 ResultProposal 计算的 digest，不制造占位 Evidence；若强制诊断 trace 提交失败，外层 gap 为 `artifact_commit_failed` 且必须另存 `originalGapCode=journey_failed`，不得覆盖原 step 失败分类。截图 proposal 只能引用已提交 Artifact，Lintel 不分配 Evidence ID。Quoin 按 operation 实际冻结的 catalog digest、Journey ID/version 和 output schema 重验 success payload，先创建 Evidence，再以不可变 `browser_journey_results` INSERT 作为唯一提交入口；该 ledger 保存 digest/outcome，success 另引用 primary Evidence，并由 SQL trigger 同一 statement 派生 check result、收口 operation 与 Attempt。未知参数/输出、完整 HTML dump、未声明自由 JSON 或 Evidence kind **MUST** 拒绝。生成管线必须真实编译每个 draft 2020-12 nested Schema，并拒绝外部/动态 ref 与非法 required。（来源：CONTEXT「Journey Catalog」、Issue #14 Q14.21）
- **CFG-JOURNEY-004 —** browser check 机械使用所属 Business System 唯一 Browser Identity；YAML **MUST NOT** 选择 identity、携带绝对 URL、密码或导航 allowlist。Browser Identity Revision 引用一个 `purpose=authentication_probe` 的 Journey ID 与类型化参数；普通 browser check 只能引用 `purpose=journey`。Journey 从 identity `startUrl` 开始，可按版本化步骤完成 SSO 跨 origin；应用层不复制网络策略。（来源：CONTEXT「浏览器身份」、Issue #14）
- **CFG-JOURNEY-005 —** 稳定 Journey ID 是行为契约：相邻正式 catalog **MUST** 保留旧 ID；参数 Schema、输出 Schema、purpose 或 Evidence kind 的任何变化必须使用新 ID。兼容实现修复可保持 ID，但 `version` 必须单调增加且 steps digest 改变；构建门机械拒绝 version 不递增、旧 ID 删除或同 version 改写 steps digest。revision/config version 保存的是创建时静态校验 catalog provenance；协调升级后，新 operation 对同一稳定 ID 自动采用当前 ready catalog 的兼容实现 version 并冻结实际 binding，不要求重建所有既有 revision。程序只验证这些结构事实，不判断修复是否保持业务语义。（来源：CONTEXT「Journey Catalog」、Issue #14）
- **CFG-JOURNEY-006 —** Journey **MUST NOT** 自动重跑整单或从中间步骤恢复；Playwright locator 固定 deadline 内等待不是重试。失败创建本次 check 的结构化 gap 与强制 trace；再次采证只能由新 Run/Config Verification Run 和新 Attempt 发起。authentication probe 输出 Schema 必须封闭为 `Authenticated|Unauthenticated|Indeterminate` 三态，技术错误不得映射为未登录。（来源：Issue #14 Q14.7/Q14.20/Q14.21）

## 7. 上传、发布与配置验证 Run

- **CFG-PUBLISH-001 —** 发布顺序：草稿上传并完成全部静态校验 → 可选 Config Verification Run → `publishBusinessSystemConfig` 原子切换 current 指针并同步根投影；普通发布只允许目标为当前 Label Contract 的草稿，候选契约配置只能随该契约联合激活原子切换（DATA-CONFIG-001/002）。`current` 只表示已发布版本；草稿是不可变候选版本，不另建“当前/最新草稿”指针，任一尚未发布的草稿均可显式 Config Verification Run、导出或发布，发布命令仍以当前已发布指针与行版本作为并发 fence。停用/启用也经发布 YAML 版本完成，管理页 **MUST NOT** 提供与 YAML 竞争的创建/编辑表单。（来源：CONTEXT「业务系统」「业务系统配置版本」）
- **CFG-VERIFYRUN-001 —** 配置验证 Run **MUST** 独立持久化（`config_verification_runs`，DATA-CONFIG-007）：精确绑定任一尚未发布的不可变草稿配置版本（不引入“当前/最新草稿”第二指针）与目标 Label Contract 版本；不复制 Catalog binding，实际 Journey operation 在准入时冻结当前兼容 Catalog；状态机由 SQL 机器权威封闭为创建后开始、成功或终止；Journey 等待浏览器 slot 时 Config Verification Run 保持 `Running`（`Passed` 必须覆盖绑定配置版本全部 check 且每项 ok+Evidence；只有 `Failed|Cancelled|Interrupted` 携带人工可读 `result_detail`）；Disabled 系统可执行；**MUST NOT** 与巡检 one-active-plan 约束交互；完整生命周期 HTTP 面：创建（202 `ConfigVerificationRunDetail`）、详情重读（GET）、取消（`clientCommandId`+`expectedRowVersion`）、SSE 推送（`objectType=config_verification_run`，HTTP-CONFIG-003）。取消命令 **MUST** 在同一事务将 Config Verification Run 置 `Cancelled` 并 fence active 浏览器子 Attempt（Queued/Assigned→Cancelled，Running→Cancelling）；Config Verification Run 终态立即拒绝迟到结果，Cancelling 子 Attempt 仍占 active 槽位直至形成终态，成功与取消按 SQLite 提交顺序裁决（DATA-TX-005）。`Passed` 可作为联合激活的 `verificationRunId` 证据（DATA-CONFIG-002/007）。（来源：CONTEXT「业务系统配置版本」「标签契约」）
- **CFG-VERIFYRUN-002 —** Config Verification Run 执行 **MUST** 使用与巡检一致的机械采集路径：每个 PromQL 检查创建 `scope_type='config_verification_run'` 的 `inspection_collection` 子 Attempt，由 Quoin 冻结 `config_thanos_query` grant 并派发 Plinth supervisor；Quoin 不解密或执行 PromQL，supervisor 只能经 `FetchCredentialGrant` 取得该 Attempt 的短期 Thanos 凭据（ARCH-WORKER-002、DATA-CONN-002/008、RUNTIME-GRANT-001）。浏览器检查仍派发 Lintel（DATA-BROWSER-003、RUNTIME-TASK-003）；在 Lintel 浏览器执行器落地前，含 browser 检查的草稿在创建命令中确定性拒绝（HTTP 503），**MUST NOT** 创建永不收敛的 Run 占用 active fence。每 check 结果写入 `config_verification_run_check_results`（以 `plan_key + check_key` 复合定位；每项均绑定精确子 Attempt；PromQL 已接受的 success 以该 Running Attempt 绑定完整 Evidence，error/gap 以密封 ResultProposal 的 digest/gap code 记录；Browser success/gap 必须通过 `browser_journey_results` 单一入口绑定子 Attempt、operation、result digest 与唯一 primary structured Evidence。没有 ResultProposal 的技术终止才使用 Failed/Cancelled/Interrupted Attempt 与无 Evidence/digest gap）。Evidence 必须指向本 Config Verification Run 且 `params_json.plan_key/check_key` 精确匹配（DATA-CONFIG-007）。（来源：CONTEXT「巡检项」「执行尝试」「浏览器操作记录」）
- **CFG-EXPORT-001 —** 任意草稿/已发布版本可导出（`getBusinessSystemConfig`/`getLabelContract` 返回原文）；业务系统与 Label Contract 起始模板输出 `application/yaml` 且不含凭据。Journey Catalog 不是用户输入，不提供模板、上传或编辑端点；只通过 `getJourneyCatalog` 读取当前构建期嵌入内容（DATA-CONFIG-008）。（来源：CONTEXT「业务系统配置版本」「Journey Catalog」）

## 8. 验证要求

- **CFG-VALIDATION-001 —** 三个 JSON Schema **MUST** 以真实 draft 2020-12 校验器（AJV 2020）通过正反例矩阵：正例覆盖各封闭变体（promql instant/range、browser、空 journey_params）；反例覆盖未知字段、跨 kind 字段、instant+range 字段并存、range 缺 step、缺根字段、非法 label 名、重复 key（数组深等值）、journey 参数开放对象、catalog 缺 `additionalProperties: false`、非法 journey_id。（来源：Issue #12 交付纪律）
- **CFG-VALIDATION-002 —** strict YAML 解析 **MUST** 以真实 `yaml.v3` `yaml.Node` 行为逐项验证：重复 key、anchor/alias/merge、自定义 tag、非字符串字段名、第二文档、尾随内容、超限（输入字节/AST 节点/深度）全部拒绝，合法单文档通过；未知字段拒绝由 CFG-VALIDATION-001 的真实 JSON Schema 反例证明。（来源：Issue #12 交付纪律）
- **CFG-VALIDATION-003 —** PromQL 校验 **MUST** 以官方 parser 验证：合法表达式通过；非法语法拒绝；VectorSelector 缺业务系统 label / 非精确 `=` / 值不等于 system key 拒绝；discovery selector 含 `offset`/`@`/聚合/`label_replace`/子查询拒绝；check expression 允许 `offset`/`@`/子查询（只需通过 AST 与归属校验）。（来源：Issue #8 交付纪律）
- **CFG-VALIDATION-004 —** SQL 投影 **MUST** 以 SQLite harness 验证：根投影列持久化与 `system_key` 匹配触发器、发布投影同步（同一事务、`row_version` 递增）、check 类型化 CHECK（instant/range 组合、跨 kind 排斥、零/负 range/step 拒绝）、`config_verification_runs` 生命周期（active 唯一、终态不可变、来源不可改、row_version 精确 +1、check 结果条件约束）、`scope_type='config_verification_run'` 与 journey 浏览器操作绑定（DATA-VALIDATION-002）。（来源：Issue #12 交付纪律）
- **CFG-VALIDATION-005 —** catalog 验证 **MUST** 覆盖：生成器拒绝重复稳定 ID；每个嵌套参数 Schema 通过 draft 2020-12 metaschema 编译且 required 只引用已声明参数；Journey 参数正反例按对应嵌套 Schema 执行；相同输入的两次独立生成产出完全相同的文件字节与 digest；Quoin/Lintel 嵌入字节相等；相邻正式 v1 catalog 的旧 ID/参数契约兼容门；`Hello.journey_catalog_digest` 不匹配拒绝（RUNTIME-VALIDATION-002）。（来源：Issue #12 验收条件）
