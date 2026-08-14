# Quoin v1 技术规格

**状态：Draft**

**Non-normative：** 本目录承载 Quoin v1 的规范性技术契约。目标是把 [`CONTEXT.md`](../../../CONTEXT.md) 中已经冻结的领域语言与业务边界，落实为可供后续实施规划和验证直接使用的数据、API、组件协议、配置、前端、安全、运维与验收规格；本目录不包含生产实现或实施任务拆分。

全部 Wayfinder 决策、依据与研究索引见 [Quoin v1 技术规格地图](https://github.com/Suknna/quoin/issues/1)。本文件的规范条款使用 `SPEC` 类别，来源均为已关闭的 [确定 v1 规格结构与规范资产边界](https://github.com/Suknna/quoin/issues/8)。

## 阅读顺序

**Non-normative：** 计划中的规范性文档按以下顺序阅读：

1. `architecture.md`：系统边界、四组件职责、跨组件不变量和全局状态模型。
2. `persistence.md`：SQLite 数据模型、聚合关系、唯一约束、事务、迁移、Artifact 与派生索引。
3. `http-api.md`：Huma HTTP API、认证授权、命令幂等、错误、分页、快照、SSE 与对话流。
4. `runtime-protocol.md`：Quoin、Plinth、Lintel、Stele 的身份、版本握手、控制流、任务、lease、fencing、浏览器会话、文件上传与告警接入。
5. `inspection-config.md`：业务系统 YAML、Label Contract、Resource Discovery、Inspection Plan/Check 与 Journey Catalog。
6. `frontend.md`：三栏工作台、URL、核心流程、状态反馈、可访问性和可执行 UI 验收场景。
7. `security.md`：威胁路径、用户与服务身份、Session、CSRF、权限、秘密、审计和恢复后的信任重建。
8. `operations.md`：Compose、Helm、存储卷、锁、健康检查、备份恢复、保留、升级、配置与可观测性。
9. `verification.md`：端到端验收矩阵、故障注入、证据要求与验证边界。

- **SPEC-STRUCTURE-001 —** 规格文件 **MUST** 在首次产生真实内容时创建，且 **MUST NOT** 创建空占位文件。（来源：[确定 v1 规格结构与规范资产边界](https://github.com/Suknna/quoin/issues/8)）
- **SPEC-STRUCTURE-002 —** `README.md` **MUST** 只定义规格结构、事实所有权、规范语言、追溯和交付规则，且 **MUST NOT** 复制各主题文件拥有的规范条款。（来源：[确定 v1 规格结构与规范资产边界](https://github.com/Suknna/quoin/issues/8)）

## 机器契约目录

机器可验证资产集中存放在 `contracts/`：

```text
contracts/
├── openapi.yaml
├── runtime.proto
├── quoin/
│   └── plinth/
│       └── worker/
│           └── v1/
│               └── agent_worker.proto
├── schemas/
│   └── *.schema.json
└── sql/
    └── schema.sql
```

| 路径 | 拥有的机器契约 |
| --- | --- |
| `contracts/openapi.yaml` | HTTP 路由、方法、输入输出结构、状态码和可机械表达的 HTTP 契约 |
| `contracts/runtime.proto` | 四组件 gRPC service、RPC、message、stream 与 wire-level 枚举 |
| `contracts/quoin/plinth/worker/v1/agent_worker.proto` | Plinth supervisor ↔ worker 本地 stdio framed protobuf message 与 wire-level 枚举 |
| `contracts/schemas/*.schema.json` | 业务系统 YAML、Label Contract、Journey Catalog 及其他独立文档格式 |
| `contracts/sql/schema.sql` | SQLite 表、列、索引、外键、检查约束和可由数据库表达的唯一约束 |

- **SPEC-STRUCTURE-003 —** 九份主题 Markdown **MUST** 保持在 `docs/specs/quoin-v1/` 根目录；机器契约 **MUST** 使用上表所列的 `contracts/` 路径，并按 `SPEC-STRUCTURE-001` 延迟创建。（来源：[确定 v1 规格结构与规范资产边界](https://github.com/Suknna/quoin/issues/8)）

## 权威源与事实所有权

| 事实类别 | 权威源 | 其他文件如何引用 |
| --- | --- | --- |
| 领域术语、业务概念、产品边界和稳定业务规则 | [`CONTEXT.md`](../../../CONTEXT.md) | 规格引用对应标题，不重新定义同义概念 |
| HTTP 字段、类型、路由和状态码 | `contracts/openapi.yaml` | `http-api.md` 解释跨请求语义、事务和状态转换 |
| 跨组件 gRPC service、message、字段、枚举与 stream 形状 | `contracts/runtime.proto` | `runtime-protocol.md` 解释握手、lease、fencing、重连和调和 |
| Plinth supervisor ↔ worker 本地 stdio message、字段与枚举 | `contracts/quoin/plinth/worker/v1/agent_worker.proto` | `architecture.md` 解释 framing、sandbox、执行顺序和故障语义 |
| YAML/JSON 文档字段、类型和结构约束 | `contracts/schemas/*.schema.json` | `inspection-config.md` 解释发布、兼容、运行和错误语义 |
| SQLite 表、列、索引、外键和数据库约束 | `contracts/sql/schema.sql` | `persistence.md` 解释聚合、事务、生命周期和迁移约束 |
| 跨协议不变量、事务边界、状态转换、权限、运行行为和无法由单份机器契约表达的约束 | 对应主题 Markdown | 机器契约只承载其可机械表达部分 |
| 决策理由、备选方案、外部证据与原型结论 | 已关闭 Wayfinder ticket 及其证据 commit | 规格只保留必要的来源链接，不复制研究报告 |

- **SPEC-AUTHORITY-001 —** 每类事实 **MUST** 只有上表所列的一个权威所有者；非权威文件 **MUST NOT** 建立可独立修改的第二份定义。（来源：[确定 v1 规格结构与规范资产边界](https://github.com/Suknna/quoin/issues/8)）
- **SPEC-AUTHORITY-002 —** Markdown **MUST NOT** 人工复制由 OpenAPI、Proto、JSON Schema 或 SQL 拥有的完整字段清单，并 **MUST** 通过路径与稳定符号引用机器契约。（来源：[确定 v1 规格结构与规范资产边界](https://github.com/Suknna/quoin/issues/8)）
- **SPEC-AUTHORITY-003 —** 机器契约与 Markdown 出现矛盾时，规格验证 **MUST** 失败；验证程序 **MUST NOT** 通过“Markdown 优先”或“机器契约优先”自动掩盖漂移。（来源：[确定 v1 规格结构与规范资产边界](https://github.com/Suknna/quoin/issues/8)）

## 规范性语言

**Non-normative：** 正文使用中文，RFC 2119 风格关键词的含义如下：`MUST` / `MUST NOT` 表示强制约束，`SHOULD` / `SHOULD NOT` 表示偏离时需要具体理由和等价证据的强烈建议，`MAY` 表示允许但不强制的实现选择。

- **SPEC-LANGUAGE-001 —** 规范正文 **MUST** 使用中文，并 **MUST** 使用 `MUST`、`MUST NOT`、`SHOULD`、`SHOULD NOT`、`MAY` 表达约束强度。（来源：[确定 v1 规格结构与规范资产边界](https://github.com/Suknna/quoin/issues/8)）
- **SPEC-LANGUAGE-002 —** 原因、示例和其他说明性内容 **MUST** 分别标识为 **Rationale**、**Example** 或 **Non-normative**；未使用规范关键词的普通说明 **MUST NOT** 被解释为隐藏的验收要求。（来源：[确定 v1 规格结构与规范资产边界](https://github.com/Suknna/quoin/issues/8)）

## 条款 ID 与追溯

规范条款 ID 采用以下格式：

```text
<CATEGORY>-<SUBJECT>-<NNN>
```

**Example：** `DATA-ARTIFACT-001`、`HTTP-COMMAND-003`、`RPC-FENCE-002`、`UI-WAIT-004`。

- **SPEC-TRACE-001 —** 每条独立的 `MUST`、`MUST NOT`、`SHOULD`、`SHOULD NOT` 或 `MAY` 规范条款 **MUST** 拥有稳定 ID；标题、Rationale、Example、Non-normative 和研究摘要 **MUST NOT** 分配规范条款 ID。（来源：[确定 v1 规格结构与规范资产边界](https://github.com/Suknna/quoin/issues/8)）
- **SPEC-TRACE-002 —** 每个主题文件 **MUST** 在首次定义条款时声明自己的 `CATEGORY` 前缀；同一 ID **MUST NOT** 被重新分配给不同语义，删除或被替代的 ID **MUST NOT** 复用。（来源：[确定 v1 规格结构与规范资产边界](https://github.com/Suknna/quoin/issues/8)）
- **SPEC-TRACE-003 —** 每条规范性条款 **MUST** 链接到至少一个来源：`CONTEXT.md` 稳定标题、已关闭 Wayfinder ticket 或 research/prototype commit；来源 **MUST NOT** 使用易漂移的文件行号。（来源：[确定 v1 规格结构与规范资产边界](https://github.com/Suknna/quoin/issues/8)）
- **SPEC-TRACE-004 —** 跨文件引用 **MUST** 使用条款 ID；引用机器契约时 **MUST** 同时给出相对路径和稳定 operation、message、schema `$id`、表、约束或其他 symbol 名。（来源：[确定 v1 规格结构与规范资产边界](https://github.com/Suknna/quoin/issues/8)）

## 版本与冻结

- **SPEC-VERSION-001 —** Quoin v1 规格在全部地图子票关闭、`Not yet specified` 清空或移入 `Out of scope`、规格文件与机器契约一致且最终独立审阅达到零 BLOCKER / 零 MAJOR 前 **MUST** 保持 **Draft**；全部条件满足后 **MUST** 标为 **Frozen**。（来源：[确定 v1 规格结构与规范资产边界](https://github.com/Suknna/quoin/issues/8)）
- **SPEC-VERSION-002 —** HTTP API **MUST** 使用 `/api/v1`，Proto package **MUST** 使用 `quoin.runtime.v1`，JSON Schema **MUST** 使用稳定 `$id`。（来源：[确定 v1 规格结构与规范资产边界](https://github.com/Suknna/quoin/issues/8)）
- **SPEC-VERSION-003 —** Frozen 前的修正 **MUST** 直接更新 v1，且 **MUST NOT** 人为维护 `v1.1`、`v1.2` 文档版本；Frozen 后的兼容修正 **MAY** 保持 v1，破坏机器契约或已冻结语义的变化 **MUST** 创建 v2。（来源：[确定 v1 规格结构与规范资产边界](https://github.com/Suknna/quoin/issues/8)）
- **SPEC-VERSION-004 —** Git 历史、决策票和证据分支 **MUST** 保存修订记录，本目录 **MUST NOT** 另行人工维护 Changelog。（来源：[确定 v1 规格结构与规范资产边界](https://github.com/Suknna/quoin/issues/8)）

## 每张规格票的交付纪律

- **SPEC-DELIVERY-001 —** 每张规格票 **MUST** 只更新该票拥有的规格文件和机器契约资产，并 **MUST** 执行与变更匹配的机器解析、一致性和专项验证。（来源：[确定 v1 规格结构与规范资产边界](https://github.com/Suknna/quoin/issues/8)）
- **SPEC-DELIVERY-002 —** 每张规格票 **MUST** 形成一个独立的本地逻辑提交，且 **MUST NOT** 混入其他票或无关整理。（来源：[确定 v1 规格结构与规范资产边界](https://github.com/Suknna/quoin/issues/8)）
- **SPEC-DELIVERY-003 —** Resolution comment **MUST** 记录最终决策、文件路径、commit SHA、实际命令、退出码、关键输出和未覆盖项；完成后 **MUST** 关闭该票，并 **MUST** 向地图的 **Decisions so far** 追加一行带名称和链接的索引。（来源：[确定 v1 规格结构与规范资产边界](https://github.com/Suknna/quoin/issues/8)）
- **SPEC-DELIVERY-004 —** Research 与 Prototype 资产 **MUST** 保存在未合并到 `main` 的 throwaway 分支，规格 **MUST** 通过 ticket 和不可变 commit 引用这些证据；除非用户明确要求，本地规格提交、research 分支和 prototype 分支 **MUST NOT** push。（来源：[确定 v1 规格结构与规范资产边界](https://github.com/Suknna/quoin/issues/8)）
