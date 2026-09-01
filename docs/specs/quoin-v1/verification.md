# Quoin v1 — 端到端验证规格（verification.md）

**状态：Draft**

**CATEGORY 前缀：`VERIFY`**

**Non-normative：** 本文件拥有 Contract Gate、Release Qualification 与 Deployment Acceptance 的执行层级、环境矩阵、故障编排、证据规则、清理门和 verdict 聚合语义。各领域行为断言继续由 `architecture.md`、`persistence.md`、`http-api.md`、`runtime-protocol.md`、`inspection-config.md`、`frontend.md`、`security.md` 与 `operations.md` 的稳定条款拥有；本文件只引用条款 ID，不复制字段、枚举或领域状态机正文。来源为 [Issue #21](https://github.com/Suknna/quoin/issues/21) 与 [`CONTEXT.md`](../../../CONTEXT.md) 的“验证声明分层”至“Lintel cleanup 离线恢复”。

## 1. 权威边界与机器资产

- **VERIFY-AUTHORITY-001 —** `contracts/verification-catalog.yaml` 及 `contracts/schemas/verification-catalog.schema.json` **MUST** 是跨域 scenario ID、层级、coverage root、cell、前置条件、依赖、proof、环境能力、执行器、故障原语、证据和清理要求的唯一机器权威；测试实现 **MUST** 只引用 scenario ID，且 **MUST NOT** 独立维护第二份 required 场景清单。（来源：Issue #21 Q21.2/Q21.24–Q21.27）
- **VERIFY-AUTHORITY-002 —** `contracts/connection-probes.yaml` 及 `contracts/schemas/connection-probes.schema.json` **MUST** 是 Model Provider、Thanos 与 Kubernetes probe action-set/version 的唯一机器权威；catalog **MUST** 只引用其 digest 或 action-set ID，且 **MUST NOT** 复制动作正文。（来源：Issue #21 Q21.36）
- **VERIFY-AUTHORITY-003 —** `contracts/schemas/verification-result.schema.json` **MUST** 是 Quoin 对 in-toto Statement v1 / Test Result v0.1 严格 profile、scenario result、Deployment Acceptance manifest locator 与 finalization receipt 投影的唯一 Schema；`contracts/schemas/verification-evidence.schema.json` **MUST** 单独约束其按 digest 引用的 evidence index，且 **MUST NOT** 定义第二种 Test Result Statement。Deployment Acceptance helper、typed locator 与 typed observation 的交换结构由 `contracts/schemas/deployment-verification.schema.json` 拥有，并由 `contracts/openapi.yaml` 的 HTTP request/response schema 直接引用或投影；Release manifest、JUnit、HTML、CI annotation、日志、截图和 Web UI **MUST NOT** 成为竞争 verdict 权威。（来源：Issue #21 Q21.3/Q21.4/Q21.35）
- **VERIFY-AUTHORITY-004 —** SQLite 行、HTTP 字段、Runtime message 与 Artifact kind 的结构 **MUST** 分别由 `contracts/sql/schema.sql`、`contracts/openapi.yaml`、`contracts/runtime.proto` 与对应 JSON Schema 拥有；本文件 **MUST** 只定义跨资产不变量和执行语义。（来源：SPEC-AUTHORITY-001..003、Issue #21）

## 2. 验证层级与触发

- **VERIFY-LAYER-001 —** 验证结论 **MUST** 严格分为 `contract_gate`、`release_qualification` 与 `deployment_acceptance`；低层 PASSED **MUST NOT** 推导高层 PASSED，项目控制 fixture **MUST NOT** 被描述为任意真实外部系统兼容。（来源：Issue #21 Q21.1）
- **VERIFY-LAYER-002 —** PR **MUST** 阻塞执行全部 applicable Contract Gate scenario；v1 **MUST NOT** 建设 source-to-scenario 影响选择器。main 定时任务 **MUST** 执行完整自动化矩阵；annotated SemVer tag **MUST** 针对该 tag 构建的不可变工件重新执行完整 Release Qualification 并补齐人工 observation。（来源：Issue #21 Q21.11）
- **VERIFY-LAYER-003 —** PR/nightly、不同 tag、不同 Release manifest digest 或不同 catalog/profile/contract digest 的结果 **MUST NOT** 成为当前 tag 的发布证据；每次重验 **MUST** 创建新 invocation。（来源：Issue #21 Q21.11/Q21.25/Q21.27）
- **VERIFY-LAYER-004 —** Deployment Acceptance **MUST** 证明一个已发布 Release 在具体站点的 point-in-time 部署事实，**MUST NOT** 写回或阻塞上游 Release manifest/发布状态，也 **MUST NOT** 声明持续健康。（来源：Issue #21 Q21.1/Q21.35/Q21.38）

## 3. Scenario 粒度、执行图与覆盖根

- **VERIFY-CATALOG-001 —** 一个 scenario **MUST** 是可独立裁决、重试、留证和清理的最小单元，并 **MUST** 使用 `setup/action/assert/teardown` 四阶段；不同资源所有者、故障原语、证据要求或清理范围 **MUST** 使用不同 scenario ID。（来源：Issue #21 Q21.24）
- **VERIFY-CATALOG-002 —** `depends_on` **MUST** 只形成同层 DAG；dependency 的 required cell applicability **MUST** 覆盖 dependent scenario。依赖未通过时 dependent **MUST** 记录带稳定 causal ID 的 `not_run`，且 **MUST NOT** 被报告为执行通过。（来源：Issue #21 Q21.24/Q21.26）
- **VERIFY-CATALOG-003 —** `proof_refs` **MUST** 只指向严格更低层，并 **MUST** 闭合到同一 tag qualification invocation、source commit、catalog/profile/contract digest 与 Release subject；上层仍 **MUST** 执行自身 action/assert。已声明的 `proof_refs[]` 若在同 tag 证据闭包中缺失，使上层 WARNED；空 `proof_refs[]` 明确表示该 scenario 不需要下层 prerequisite，不是 proof 缺失。proof FAILED 使上层 FAILED。（来源：Issue #21 Q21.27）
- **VERIFY-CATALOG-004 —** 已发布 scenario ID **MUST** 只允许退休；语义、断言或所属层变化 **MUST** 创建新 ID，旧 ID **MUST NOT** 被复用。退休项 **MUST** 保留 successor/reason 与历史结果可解释性。（来源：Issue #21 Q21.25）
- **VERIFY-CATALOG-005 —** `capability_definitions` **MUST** 是 deployment backend、architecture、Kubernetes/Docker/Compose exact version selector、browser evidence kind、fault backend 与 external stack 的封闭机器词表；scenario/cell 只能引用已声明 capability。每个 cell **MUST** 以 `always | deployment_target | for_each_current_object | deployment_target_for_each_current_object` 明确 applicability；第四种模式把与 manifest backend/architecture 精确匹配的 target cell 与 manifest 冻结的 current object 实例作笛卡尔展开。Deployment Acceptance 只把匹配的 target cell、冻结的 current object 实例、该笛卡尔展开项，或 `subject.kind=ui_surface` 且 backend-independent 的固定 typed-observation `always` cell 纳入分母；其它 `always` cell 不得借此进入站点验收。每项长期 evidence **MUST** 保存解析后的 backend、精确 architecture、适用版本/build、toolchain digest 与 capability IDs；suite Test Result 只保存该完整 environment matrix 的 digest 与 cell count，不得用单一环境冒充多 cell invocation。（来源：Issue #21 Q21.26/Q21.29/Q21.38）
- **VERIFY-CATALOG-006 —** `catalog_state=frozen` 只表示 scenario graph、cell、断言与引用契约已经机器冻结，并 **MUST NOT** 被解释为任何 Release Qualification 或 Deployment Acceptance cell 已执行或通过；只有绑定 exact catalog/profile/contract digest 的最终 signed invocation result 才能证明执行 verdict。故障注入 mechanism prototype 只决定 catalog 可实现性，不得冒充 `fault.storage` 的 Compose/Kubernetes 原生双架构结果。（来源：Issue #21 Q21.9/Q21.24/Q21.35）
- **VERIFY-COVERAGE-001 —** coverage checker **MUST** 只扫描稳定 `*-VALIDATION-*`、`OPS-VERIFY-*` 与 `UI-TEST-*` root；普通领域条款如需直接登记，**MUST** 先在所属主题文档新增或并入稳定 validation root，**MUST NOT** 扫描自由文本或逐句复制所有 `MUST`。（来源：Issue #21 Q21.26）
- **VERIFY-COVERAGE-002 —** 构建门 **MUST** 拒绝未覆盖 root、悬空 root、重复 scenario ID、无实现 required scenario、实现引用不存在 scenario、self-reference、依赖环、跨层 `depends_on`、同层/高层 `proof_refs`、cell applicability 不闭合，以及 Deployment Acceptance execution/cleanup timeout 超过 freshness budget。（来源：Issue #21 Q21.26/Q21.38）
- **VERIFY-COVERAGE-003 —** coverage report **MUST** 只声明“全部 declared validation root 均有 required scenario”，且 **MUST NOT** 声称每句领域 `MUST` 都是独立测试。（来源：Issue #21 Q21.26）

## 4. Verdict 与确定性职责

- **VERIFY-VERDICT-001 —** suite/scenario verdict **MUST** 为 `PASSED | WARNED | FAILED`；发布门 **MUST** 只接受 PASSED。全部 applicable required scenario 首次执行通过且无跳过、警告、冲突或必需 observation 缺失时才可 PASSED。（来源：Issue #21 Q21.5）
- **VERIFY-VERDICT-002 —** 产品或契约断言失败、明确 cleanup residue 与 verifier conflict **MUST** 为 FAILED；subject drift、环境不可用、人工取消、基础设施中断、cleanup indeterminate 与 not_run **MUST** 为 WARNED。active/unclassified **MUST** 返回 `verification_in_progress` 且 **MUST NOT** 生成最终结果；未知 outcome class **MUST** 是 verifier invariant failure/FAILED。（来源：Issue #21 Q21.5/Q21.34/Q21.37）
- **VERIFY-VERDICT-003 —** runner **MUST NOT** fail-fast；失败后 **MUST** 继续所有相互独立的 required scenario，teardown **MUST** 始终执行，自动重试 **MUST NOT** 隐藏先前 WARNED/FAILED。（来源：Issue #21 Q21.18）
- **VERIFY-VERDICT-004 —** 确定性程序 **MUST** 收集结构化事实、验证契约、计算 applicability/scenario/suite verdict 并生成报告；人工 **MUST** 只提交 catalog 要求的 typed observation；Agent/模型 **MUST** 只做分析、汇总和建议，且 **MUST NOT** 改写事实、提高或降低 severity、修改 required 门或把 WARNED/FAILED 改成 PASSED。（来源：Issue #21 Q21.10）
- **VERIFY-VERDICT-005 —** catalog 的 `diagnostic` scenario **MUST** 只由某个 required scenario 已持久化的 `FAILED | WARNED | not_run` 事实触发；未执行完成统一持久化为 `not_run`，不存在第二个 `incomplete` category。diagnostic 必须由该已持久化事实触发，并在 evidence 中携带该 required result 的稳定 causal ID；diagnostic 只追加事实，**MUST NOT** 进入 applicable required item 集、passed/warned/failed test 名单或 suite verdict，不拥有独立产品通过结论，也 **MUST NOT** 改写触发它的原结果。只有新 invocation 可重新资格化。（来源：Issue #21 Q21.28）

## 5. Test Result 与证据闭包

- **VERIFY-EVIDENCE-001 —** 每次 suite invocation **MUST** 生成一份符合 `contracts/schemas/verification-result.schema.json` 的 in-toto Statement v1，`predicateType` **MUST** 为 `https://in-toto.io/attestation/test-result/v0.1`；`result` 与 passed/warned/failed test 名单 **MUST** 只使用 catalog scenario ID/cell ID。（来源：Issue #21 Q21.3/Q21.4）
- **VERIFY-EVIDENCE-002 —** Statement subject **MUST** 只绑定被验收的不可变 Release 输出；随后生成且反向引用证据的 Release manifest **MUST NOT** 作为 Release Qualification subject。Deployment Acceptance profile **MAY** 把既有 Release manifest digest 作为站点验收 subject，但 **MUST NOT** 写回该 manifest。（来源：Issue #21 Q21.3/Q21.35）
- **VERIFY-EVIDENCE-003 —** configuration **MUST** 绑定 catalog、profile/schema、环境描述、工具锁、connection probe contract 及适用 contract digest；每个 scenario evidence index **MUST** 记录 invocation/cell、权威时间、环境 digest、工具版本、脱敏 argv、原始 exit code、逐断言 expected/actual/result、附件 digest/locator、cleanup outcome 与 causal/proof refs。（来源：Issue #21 Q21.6/Q21.7）
- **VERIFY-EVIDENCE-004 —** DSSE/Sigstore **MUST** 复用发布链；Release manifest **MUST** 只通过既有 `validation.<category>.evidence_sha256` 单向绑定分类 bundle，且 **MUST NOT** 建立哈希自引用、顶层竞争字段或第二发布索引。SHA-256 单独只证明内容自洽/损坏检测，不提供来源认证。（来源：Issue #21 Q21.3/Q21.35）
- **VERIFY-EVIDENCE-005 —** 原始 stdout/stderr、trace、截图、视频和 fixture transcript **MUST** 是 evidence attachment 而不是 verdict；判定所需事实 **MUST** 先提取为脱敏结构化 evidence。敏感 raw trace **MUST NOT** 成为 verdict 唯一证据。（来源：Issue #21 Q21.6/Q21.8/Q21.19）
- **VERIFY-EVIDENCE-005a —** Artifact upload 授权测试必须以真实 gRPC adapter 覆盖：有效 Lintel bearer 仅接受 browser-operation 的 generated trace/screenshot header，拒绝错误 owner、非 sensitive trace 与其他 kind；Plinth 既有上传仍可用，未认证与 Lintel Artifact 读取均被拒绝。Store fixture 必须覆盖浏览器 child Attempt/action/Operation owner closure 及 boot/epoch fence，且 Lintel client fixture 必须检查 header、连续 chunks、End 与确定性 upload ID。（来源：Issue #45）

## 6. Release Qualification 环境矩阵

- **VERIFY-MATRIX-001 —** 每个 qualification 启动时 **MUST** 解析 Kubernetes 官方当时维护的最近三个 minor 的最新 patch，在 evidence configuration 冻结精确版本，并在原生 amd64/arm64 上执行完整 Kubernetes required 矩阵；结论 **MUST** 只覆盖六个实际 cell，且 **MUST NOT** 外推 EOL minor、未执行 patch 或版本区间。（来源：Issue #21 Q21.12）
- **VERIFY-MATRIX-002 —** 每个 tag **MUST** 冻结一个当时 stable 的 Docker Engine + Compose CLI 精确版本对，并在原生 amd64/arm64 执行完整 Compose required 矩阵；结论 **MUST NOT** 外推兼容区间。这些验证环境版本 **MUST NOT** 进入产品供应链 `release-inputs.yaml`。（来源：Issue #21 Q21.13）
- **VERIFY-MATRIX-003 —** Web UI **MUST** 只声明双架构 Release 锁定 Playwright Chromium 与 amd64 上解析后冻结的精确 branded Chrome version/build；Lintel/noVNC **MUST** 只声明 Release 锁定 Chromium。v1 **MUST NOT** 声明 Firefox、Firefox ESR、Playwright WebKit 或真实 Safari 支持。（来源：Issue #21 Q21.14）
- **VERIFY-MATRIX-004 —** 每次 invocation **MUST** 使用唯一 Kubernetes namespace 或 Compose project、独立业务卷与测试身份；共享宿主只 **MAY** 为 host-published ports 分配唯一值，内部容器端口 **MUST** 保持产品固定值。普通场景 **MAY** 共享只读不可变 image digest；离线导入场景 **MUST** 使用 invocation-local registry 和独立数据卷并整体销毁。（来源：Issue #21 Q21.17）

## 7. 外部系统与模拟边界

- **VERIFY-EXTERNAL-001 —** Release Qualification **MUST** 使用官方 digest-pinned Prometheus、Alertmanager 与 Thanos 镜像验证真实协议 happy path、查询语义与 webhook；错误码、半响应、畸形响应和应用层超时 **MUST** 由 deterministic protocol fixture 验证，TCP timeout/reset **MUST** 由网络故障原语验证。（来源：Issue #21 Q21.15）
- **VERIFY-EXTERNAL-002 —** Model Provider 在 Release Qualification **MUST** 只使用 deterministic fixture；真实客户系统、生产凭据和真实模型供应商 **MUST** 只属于 Deployment Acceptance。（来源：Issue #21 Q21.15/Q21.16）
- **VERIFY-EXTERNAL-003 —** Release Qualification **MUST** 只使用合成数据、短期测试凭据和唯一 sentinel；生产凭据与生产数据 **MUST NOT** 进入流水线或公开 evidence。（来源：Issue #21 Q21.8）
- **VERIFY-EXTERNAL-004 —** 双架构声明 **MUST** 来自原生 amd64/arm64；QEMU **MAY** 作为构建或诊断证据，但 **MUST NOT** 满足原生运行 required cell。（来源：Issue #21 Q21.9/Q21.12/Q21.13）

## 8. 故障原语、时间与竞争

- **VERIFY-FAULT-001 —** catalog 的工具无关故障词表 **MUST** 封闭；进程、资源和网络原语 **MUST** 映射到 Docker/Kubernetes 原生 stop/kill/pod delete/NetworkPolicy/重建，TCP 原语 **MUST** 映射到 digest-pinned Toxiproxy 的 latency/timeout/reset_peer/bandwidth/limit_data。v1 **MUST NOT** 引入通用 Chaos 平台。（来源：Issue #21 Q21.16）
- **VERIFY-FAULT-002 —** ENOSPC、EDQUOT、EROFS、指定 fsync 失败和指定 rename 失败 **MUST** 是互不替代的 required 原语；v1 **MUST** 使用直接基于 `github.com/hanwen/go-fuse/v2@v2.9.0` loopback API 的 verification-only `quoin-faultfs`，只实现 path-scoped `write|fsync|rename → errno` 与 mount/unmount，不建设通用故障平台。catalog 冻结前的原生 linux/arm64 FUSE 原型已经逐项观察到 `ENOSPC(28)`、`EDQUOT(122)`、`EROFS(30)`、fsync/rename `EIO(5)` 及解除注入后的 write+fsync+rename 恢复；最终 Release Qualification 仍 **MUST** 在原生 amd64 与原生 arm64 的 Compose+Kubernetes cell 各自重跑。Chaos Mesh/toda v0.2.4 只发布 linux-amd64 工件且上游明确不维护 arm64，**MUST NOT** 成为本双架构门的执行器或 fallback。未证明项 **MUST NOT** 以 mock 或聚合“原子写失败”冒充 required。（来源：Issue #21 Q21.16；[go-fuse v2.9.0](https://github.com/hanwen/go-fuse/tree/v2.9.0)；[toda arm64 #37](https://github.com/chaos-mesh/toda/issues/37)）
- **VERIFY-FAULT-003 —** DNS、TLS、HTTP/SSE/gRPC framing 与 TCP transport fault **MUST** 由互不重叠的原语所有者执行；同一故障事实 **MUST NOT** 同时由 fixture、proxy 与平台网络策略竞争定义。（来源：Issue #21 Q21.15/Q21.16）
- **VERIFY-RACE-001 —** 事务竞争 **MUST** 使用显式 barrier、fence 与 scheduler trace 构造已声明 interleaving；固定 seed 状态机生成 **MAY** 补充诊断，seed、最小化失败序列和 trace **MUST** 入证据，但 **MUST NOT** 替代显式 required 交错。（来源：Issue #21 Q21.29/Q21.30）
- **VERIFY-TIME-001 —** Session、cooldown、lease、scheduler、expiry 与前端 timer **MUST** 使用各模块内部 package-private Clock/Timer 接缝；确定性测试 **MUST NOT** 用 wall-clock sleep。Release 二进制 **MUST NOT** 暴露改时钟 HTTP/gRPC 端点、配置键、环境变量或运行时开关；真实 timer smoke **MUST** 只经公开行为在 Release Qualification 执行。（来源：Issue #21 Q21.31）

## 9. 清理与资源归零

- **VERIFY-CLEANUP-001 —** teardown 前 **MUST** 先内容寻址持久化会随环境销毁的必需原始附件；teardown 与资源归零完成后才 **MAY** 计算 verdict、生成并签名唯一最终 Test Result。（来源：Issue #21 Q21.17）
- **VERIFY-CLEANUP-002 —** 正常 teardown 后 invocation 拥有的 Pod、Job、Service、Secret、PVC/volume、network、container、browser process、临时文件与临时 registry 数据 **MUST** 机械证明归零；产品/helper residue **MUST** 为 FAILED，CI/集群故障导致无法判断 **MUST** 为 WARNED。（来源：Issue #21 Q21.17）
- **VERIFY-CLEANUP-003 —** cleanup assertion **MUST** 使用 owner/fence/epoch 或等价不可变归属证明，且 **MUST NOT** 以宿主全局进程/资源计数替代 invocation 或 operation-scoped 归零。（来源：Issue #21 Q21.20/Q21.37）

## 10. 人工 observation 与非功能门

- **VERIFY-OBSERVATION-001 —** 同一确定性 verifier **MUST** 从 catalog 生成 observation 表单/向导并绑定 tag、invocation/cell、scenario ID、Release subject digest、observer 身份、开始/结束时间与封闭 typed 字段；Release Qualification observation 绑定 CI OIDC 身份，Deployment Acceptance observation 绑定发起 Admin Session；PR checklist、自由文本评论或 Approve **MUST NOT** 替代 typed observation。（来源：Issue #21 Q21.21）
- **VERIFY-OBSERVATION-002 —** Release Qualification 与每次 Deployment Acceptance 的 UI required observation **MUST** 固定为 Chromium amd64、Chromium arm64、branded Chrome amd64 三 browser/arch cell × 320、768、1024、1440 CSS px 四 viewport × normal/reduced motion 两模式，共 24 个 cell；每个 cell **MUST** 绑定实际 browser artifact/version、架构与 viewport，且 **MUST NOT** 乘 Compose/Kubernetes 后端或外推未观察 browser/viewport。（来源：Issue #21 Q21.33）
- **VERIFY-OBSERVATION-003 —** UI 自动化 **MUST** 由 CI 入口 `ci/verify-ui-automation` 直接驱动真实 Release browser artifact：两个 Playwright Chromium 架构分别使用对应发布工件，branded Chrome amd64 使用资格环境解析并冻结的真实 Chrome build；入口 **MUST NOT** 经 Lintel Runtime、版本化 Journey 或 Explorer 路径冒充普通 UI 自动化。自动化覆盖同一三个 browser/architecture cell × 四个 viewport，受 motion 影响的行为再乘 normal/reduced 两模式，并断言 DOM、键盘、焦点、target、reflow、axe 与领域行为。（来源：Issue #21 Q21.33）
- **VERIFY-NONFUNCTIONAL-001 —** v1 required 非功能集 **MUST** 只包含既有契约明确的 deadline/queue/concurrency 不变量、确定性竞争交错与资源归零；goroutine、FD、latency、throughput、CPU 和 memory **MUST** 先按固定采样点记录趋势而不阻塞发布，只有后续冻结 workload、runner、静默窗口、采样规则和数值预算后才 **MAY** 升级为 required。（来源：Issue #21 Q21.22）

## 11. Deployment Acceptance manifest 与 finalization

- **VERIFY-DA-001 —** Admin 启动 Deployment Acceptance 时，Quoin **MUST** 在一个 SQLite 事务创建不可变 append-only invocation manifest/items，冻结 server-generated invocation ID、Release subject、catalog/profile/schema、deployment config、public Origin、principal、开始时间、applicable-set digest 与服务端从权威 FK 生成的封闭 typed locator；manifest **MUST NOT** 拥有可变状态机/current pointer，启动后 **MUST NOT** 增删替换 scope。（来源：Issue #21 Q21.34）
- **VERIFY-DA-002 —** typed locator **MUST** 只允许 `deployment`、`connection`、`config`、`browser_identity` 与 `ui_observation` 机器 Schema variant；客户端自由字符串 locator **MUST** 被拒绝。current binding 漂移 **MUST** 形成 immutable subject-drift marker 并使该 item/suite 至少 WARNED，且旧 item **MUST NOT** 从分母移除或被新对象补位；若 item 已有 passed 执行结果，marker 保留该事实但禁止 PASSED，**MUST NOT** 为同 item 追加一个伪造的第二 result 而把 drift 错判为 verifier conflict。（来源：Issue #21 Q21.34）
- **VERIFY-DA-003 —** command 幂等 **MUST** 绑定 principal + client command ID + request digest；result 幂等 **MUST** 绑定 invocation item + canonical input/result digest。同 item 相同结果 **MUST** 返回原 immutable result，异结果 **MUST** 写带自身幂等键的 conflict marker 并 FAILED；helper 相同 report digest 重导入 **MUST** 幂等，不同报告占同一 item 或报告内重复/冲突 **MUST** 失败。（来源：Issue #21 Q21.34）
- **VERIFY-DA-004 —** finalizer **MUST** 只读取 manifest 冻结 item；active/unclassified item **MUST** 返回 `verification_in_progress`，not_run **MUST** 为 WARNED。所有 item 可定案后，finalizer **MUST** 先耐久 staging bundle Artifact，再在同一 SQLite writer transaction 复核 receipt 不存在及 manifest/applicable/item/result/helper-import/typed-observation/conflict/current-binding digest，冻结不晚于 deadline 的 `snapshot_at` 与 `finalized_at`，并提交唯一 append-only finalization receipt。（来源：Issue #21 Q21.34）
- **VERIFY-DA-005 —** finalization receipt **MUST** 是该 invocation 所有 item/result/helper import/observation/conflict/subject-drift evidence 写入的终止栅栏；receipt 后相同 finalization **MUST** 直接返回原 Artifact而不重读 current binding，非法迟到提交 **MUST** 只进通用 Audit Event。Browser 晚到 `stop_confirmed_at` **MAY** 继续作为 operation 调和事实，但 **MUST NOT** 进入冻结 verification digest 或改变原 verdict。（来源：Issue #21 Q21.34）
- **VERIFY-DA-006 —** `quoin-deploy verify` **MUST** 继续不持有产品用户或外部 Connection 凭据；Web UI 经 `GET .../{invocationId}/helper-request` 导出的 helper request 与 `POST .../{invocationId}/helper-reports` 导入的 helper report **MUST** 分别符合 `deployment-verification.schema.json#/$defs/helperRequest` 与 `#/$defs/helperReport`，helper request **MUST** 冻结并直接携带 manifest/item-set/catalog/profile/deployment/public-Origin digest、deadline 与每项 typed locator/input digest；helper report **MUST** 回显 Schema 中封闭的 invocation/manifest/item-set/catalog/profile/release/request digest 与每项 item ID/input digest，通过 exact request digest 对 deadline 和 typed locators 作不可歧义的摘要闭包，而 **MUST NOT** 冗余复制 report Schema 未定义的 locator/deadline 字段；并逐项携带 result digest、分类、断言、附件和 cleanup outcome；Schema 的 `x-quoin-unique-by: itemId` 是 Quoin deterministic validator 的强制词汇，request/report 内同一 item ID 重复即整份拒绝，不能以数组顺序、first/latest 或 `uniqueItems` 深相等替代。Connection Probe、Config Verification、Browser result 与 typed observation **MUST** 绑定同一 manifest/item/digest。standalone `helperReport` 是 OPS-HELPER-003 通用 `verification-report.json` 的严格 Deployment Acceptance payload；通用 envelope **MUST** 只按摘要单向引用该 payload，helperReport **MUST NOT** 反向引用 generic report 或自身，也不得另造较弱的第二份部署验收报告。（来源：Issue #21 Q21.32/Q21.34）

## 12. Deployment Acceptance 时间与保留

- **VERIFY-DA-TIME-001 —** 全部 required Deployment Acceptance scenario 的 `max_observation_age` 与 invocation observation window **MUST** 固定不超过 8 小时且 **MUST NOT** 用户配置：`deadline_at = started_at + 8h`，receipt `snapshot_at` **MUST** 位于 `[started_at,deadline_at]`，所有 result `observed_at`、observation submitted time、helper received time 与 drift observed time **MUST** 不晚于 `snapshot_at`。receipt `finalized_at` **MUST** 保存真实 commit 时间、位于 `[snapshot_at,deadline_at]` 且满足 `finalized_at - started_at <= 8h`；未能在 deadline 前完成耐久 staging 与 receipt commit 的 invocation **MUST NOT** 产生迟到最终结果，重新验收必须新建 invocation。finalizer **MUST NOT** 接受 deadline 后的新观察、扩窗、重新采证或推断当前健康。（来源：Issue #21 Q21.38）
- **VERIFY-DA-TIME-002 —** verdict 时间比较 **MUST** 只使用 Quoin 持久化的 commit/received 时间；helper 主机 wall-clock **MUST** 只作 provenance，`helper_report_received_at` **MUST** 参与 age/span。报告 **MUST** 保存 receipt 的真实 `snapshotAt`、observation window 与 `finalizedAt`，且 **MUST NOT** 包含 `validUntil` 或持续健康承诺。（来源：Issue #21 Q21.38）
- **VERIFY-DA-RETENTION-001 —** manifest/items/results/conflicts/receipt、canonical Test Result、typed observations、精确 helper report 与 evidence index **MUST** 使用既有 `long_term` retention class 并随备份恢复；`verification-evidence.schema.json` **MUST** 把 `structured_result|manifest|sigstore_bundle` 固定为 `long_term`，把 `stdout|stderr|logs|metrics|trace|screenshot|video|database` 固定为独立 `generated` Artifact；后者继承 Admin 共享保留设置，长期报告只保存 digest、类型、locator、retention/expiry 与正文到期状态，且 **MUST NOT** 通过嵌入 bundle 静默升级为长期保存。站点外部 OIDC/WORM/签名系统 **MAY** 包装报告，Quoin **MUST NOT** 为 Deployment Acceptance 新增长期签名密钥。（来源：Issue #21 Q21.35/Q21.39）

## 13. Connection Probe

- **VERIFY-CONNECTION-001 —** Model Provider 探测 **MUST** 清洁重构为唯一 `connection_probe` Execution Attempt（`scope_type=connection`）；SQL/proto/输入契约 **MUST** 使用封闭 `model_probe_chat | model_probe_embedding | thanos_probe | kubernetes_probe` grant purpose，冻结 revision/credential generation/root binding，且 **MUST NOT** 伪造 Tool Call/Business System、启动 worker/Agent/ReAct 或使用开放 purpose。（来源：Issue #21 Q21.36）
- **VERIFY-CONNECTION-002 —** `connection_probe_results` header **MUST** 与 Attempt 1:1，按 connection type 的 typed child **MUST** 拥有动作事实；旧 `model_provider_capabilities` **MUST** 删除并把能力字段迁入 Model Provider child，且 **MUST NOT** 保留并行旧表、latest-success 选择或兼容层。（来源：Issue #21 Q21.36）
- **VERIFY-CONNECTION-003 —** Model Provider action-set **MUST** 覆盖 streaming、native/multi tool call、cancel、usage、request ID 与 configured embedding dimension；只有 Model Provider Connection **MUST** 使用显式 `qualified_probe_result_id` 作为 enable 和普通 Model/Embedding grant 的强制闭包。pair/root/probe-contract 变化 **MUST** fail-closed、清空资格并要求重验；普通 grant **MUST** 再核对并冻结所选 result。（来源：Issue #21 Q21.36、ARCH-VALIDATION-003）
- **VERIFY-CONNECTION-004 —** Thanos probe **MUST** 通过 Plinth 实际配置/auth 执行固定 `vector(1)` 并只声明 Plinth Tool endpoint/auth/query path；Quoin PromQL path **MUST** 由 Config Verification 单独证明。Kubernetes probe **MUST** 验证 `/version`、`/api`、`/apis` 与 effective namespace 中 pods get/list、events list、pods/log get 的 SelfSubjectAccessReview，且 **MUST NOT** 外推其它权限或全部 Business System mapping。（来源：Issue #21 Q21.36）
- **VERIFY-CONNECTION-005 —** Thanos/Kubernetes **MUST NOT** 建立长期 qualification pointer或增加普通派发前置；fresh result **MUST** 首先是 invocation-scoped Deployment Acceptance 事实。显式 root revalidation **MAY** 引用闭合到 current pair/current probe contract 的成功 result，但 **MUST NOT** 改变普通 grant 规则。（来源：Issue #21 Q21.36）

## 14. 当前 Config 与 Browser 站点重证

- **VERIFY-CONFIG-001 —** `config_verification_runs` 与相关 API 类型 **MUST** 清洁泛化为唯一 `ConfigVerificationRun`；`purpose=prepublish` **MUST** 保持草稿/联合激活语义，`purpose=deployment_acceptance` **MUST** 只绑定 current published config/current Label Contract 并复用真实 PromQL/Journey/probe/Evidence/取消状态机，但 **MUST NOT** 移动发布 pointer 或成为联合激活证据。（来源：Issue #21 Q21.37）
- **VERIFY-BROWSER-001 —** `browser_operations` **MUST** 新增 `deployment_verification` kind 并复用既有 identity lock、容量队列、BrowserTunnel/noVNC、Session 吊销和 Stop 调和；它 **MUST** 冻结 manifest item、发起 Admin Session、identity revision 与 current generation marker，只允许该 Session 单 active attachment和同 boot 宽限内顺序重附着，并 **MUST** 在 Quoin/Lintel 双侧拒绝 profile publish。（来源：Issue #21 Q21.37）
- **VERIFY-BROWSER-002 —** Lintel **MUST** 在 identity lock 内从 current generation marker 对应的当前物理 profile 创建 deterministic disposable clone，证据绑定 marker、开始时 inventory/manifest observation 与时间；它 **MUST NOT** 声称重现 generation 发布时历史字节，也 **MUST NOT** 把 clone 写入 `browser_profile_generations`。（来源：Issue #21 Q21.37）
- **VERIFY-BROWSER-003 —** 每个 deployment verification operation **MUST** 在 cleanup timeout 内产生唯一 typed result，并分别保存功能 verdict 与 `clean | residue | indeterminate` cleanup outcome。clean **MUST** 逐项证明该 operation 的 process/cgroup、Chromium、x0vnc/noVNC、clone namespace、临时文件/runtime handle 与 slot lease 归零，并与 `stop_confirmed_at`/释放 lock 同事务；residue **MUST** FAILED，indeterminate **MUST** WARNED，后二者 **MUST** 继续持锁调和，后续清理成功 **MUST NOT** 改写 result 或转绿。（来源：Issue #21 Q21.37）
- **VERIFY-BROWSER-004 —** Lintel 每次 boot **MUST** 在 Browser Ready 前 inventory/sweep 整个 deterministic deployment-verification clone namespace；普通 `new_boot` 批量确认 **MUST** 排除该 kind，正常在线/boot 路径只允许 `new_boot_cleanup_confirmed` 释放其锁。Stop/cleanup report **MUST** 绑定 operation、原 start boot、cleanup boot/epoch、stop fence、clone identity 与 operation-scoped 资源计数。（来源：Issue #21 Q21.37）

## 15. Lintel cleanup 离线恢复

- **VERIFY-RECOVERY-001 —** 当未确认 Browser cleanup 与旧 Lintel token/state 损坏形成 replacement 互锁时，唯一恢复入口 **MUST** 是 deployment operator 的 `quoin-deploy compose|helm recover-lintel`；普通 Admin/Web UI **MUST NOT** 提供 force unlock。Deployment Acceptance 的 required recovery scenario **MUST** 由 helper setup 在隔离的 disposable identity/slot 上注入故障并真实产生一个 indeterminate/residual cleanup fence，再执行恢复；健康站点不得被要求凭空提供既有事故，也不得触碰现有业务 Browser Identity。（来源：Issue #21 Q21.40）
- **VERIFY-RECOVERY-002 —** helper **MUST** 停止全部产品组件，取得封闭 `LintelRecovery` maintenance state 与状态目录应用锁，并机械证明旧 workload/process 已 fence、旧加密存储为 `exclusively_reattached` 或 `retired`；随后 **MUST** 使用同 Release Quoin 镜像的一次性 `quoin maintenance recover-lintel` 挂载权威状态，在发布任何新状态前校验 report/backend/current slot/token generation，并在最后事务撤销旧 token、写 append-only recovery receipt/audit。相同 `(old_slot_id, old_token_generation, disposition_digest)` **MUST** 幂等，同键异 report **MUST** 冲突。（来源：Issue #21 Q21.40）
- **VERIFY-RECOVERY-003 —** `exclusively_reattached` **MUST** 只解除 Runtime replacement fence，原 Browser result/stop confirmation/identity/Browser-slot lock **MUST** 不变；新 Lintel 独占挂载后 **MUST** 在 Ready 前 sweep，并只以 `new_boot_cleanup_confirmed` 释放。`retired` **MUST** 由离线事务以 `externally_fenced_storage_retired` 为受影响 operation 写 stop confirmation、释放锁、把 Browser Identity 降为 `AuthenticationRequired` 并允许 replacement；旧 verification result **MUST NOT** 转绿。（来源：Issue #21 Q21.40）
- **VERIFY-RECOVERY-004 —** `recover-lintel` **MUST** 按 OPS-HELPER-004 同型执行 action-specific post-verify；任何 workload/storage/token/slot/app-lock 证明不足 **MUST** 在事务前稳定拒绝并要求完整离线恢复或重建。（来源：Issue #21 Q21.40）

## 16. Validation roots

- **VERIFY-VALIDATION-001 —** Contract Gate **MUST** 以真实 JSON Schema/YAML/OpenAPI/Proto/SQLite 解析器验证 catalog、connection probe contract、Test Result profile 与跨契约枚举/引用严格一致，并覆盖全部正反例、悬空引用、非法依赖/层级/cell、退休 ID 复用和不完整 evidence。（来源：Issue #21 Q21.2–Q21.7/Q21.24–Q21.27）
- **VERIFY-VALIDATION-004 —** Contract Gate **MUST** 从生产 handler/service registration 机械提取 HTTP route+method 与 gRPC service+method 面并分别与 `contracts/openapi.yaml`、`contracts/runtime.proto` 精确比较；Release 工件扫描 **MUST** 证明不存在测试专用 HTTP/gRPC 路由、Clock/transport 控制端点、测试环境变量、配置键或运行时开关。测试 seam 只允许 package-private 注入并在生产构造中绑定真实实现。（来源：Issue #21 Q21.29/Q21.31）
- **VERIFY-VALIDATION-005 —** Verification Catalog **MUST** 为 `VERIFY-EXTERNAL-001` 提供 required Release Qualification scenario，在 `release.native-matrix` 的每个 Compose/Kubernetes、amd64/arm64 qualification cell 上运行官方 digest-pinned Prometheus、Alertmanager、Thanos happy path，并把 deterministic protocol fixture 与 Toxiproxy TCP fault scenario 作为显式依赖/低层证明；不得只在文字或单元测试中声明外部协议兼容。（来源：Issue #22 最终一致性审阅）
- **VERIFY-VALIDATION-002 —** Release Qualification **MUST** 在 Compose/Kubernetes、原生双架构、精确外部栈与真实浏览器矩阵执行 catalog 全部 applicable required scenario，并证明 subject/configuration/toolchain/proof/evidence/cleanup 闭包；单个 build、lint、template 或安装成功 **MUST NOT** 代替运行验收。（来源：Issue #21 Q21.11–Q21.23）
- **VERIFY-VALIDATION-003 —** Deployment Acceptance **MUST** 对 helper、manifest/results/conflicts/receipt、Connection Probe、Config Verification、Browser/noVNC/cleanup、8 小时时间闭包、retention 与 `recover-lintel` 构造正常、失败、重放、并发、漂移、取消、崩溃和新 boot 对抗用例；只有零 verifier conflict 且全部 applicable required item PASSED 才可 PASSED。
- **VERIFY-BACKUP-001 —** 备份验证 **MUST** 证明 `backup_settings.schedule_enabled_at` 的启用、禁用、重新启用与无关设置更新语义，证明 scheduler 无历史 scheduled Run 时只以该锚点选择最后错过边界；真实 Chromium/Compose 路径 **MUST** 显示 `BackupSettings.backupTarget`，并经 test-side 物理备份卷故障而非产品测试控制面验证失败、恢复、manifest/checksum 下载、`BackupSummary.sizeBytes` 与归档成员字节总和精确一致，以及 cleanup。（来源：Issue #21 Q21.32–Q21.40）

## 17. 上游证据与明确边界

**Non-normative：** Quoin 复用下列上游格式/事实，但自己的 profile、catalog、矩阵与声明边界仍由本规格拥有：

- in-toto Statement v1 与 Test Result v0.1：https://github.com/in-toto/attestation/blob/main/spec/predicates/test-result.md
- Sigstore attestation verification：https://docs.sigstore.dev/cosign/verifying/attestation/
- Kubernetes maintained releases：https://kubernetes.io/releases/
- Kubernetes version skew policy：https://kubernetes.io/releases/version-skew-policy/
- Kubernetes SelfSubjectAccessReview / `kubectl auth can-i`：https://kubernetes.io/docs/reference/access-authn-authz/authorization/
- Docker Engine release notes：https://docs.docker.com/engine/release-notes/
- Docker Compose release notes：https://docs.docker.com/compose/releases/release-notes/
- go-fuse v2.9.0 loopback/FUSE API：https://github.com/hanwen/go-fuse/tree/v2.9.0
- Chaos Mesh/toda arm64 不支持的上游边界：https://github.com/chaos-mesh/toda/issues/37
