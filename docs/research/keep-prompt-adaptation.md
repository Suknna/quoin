# Keep 提示词迁入对照：Keep 原文 → Quoin 规则 → 未采用条款（2026-09-17）

- **目的**：记录 Issue #105「Keep 提示词迁入是首期必交付」在后端的落地对照，供核查确实以实际 Keep 提示词为起点做了中文化与 Quoin 领域适配，而不是自行写一段泛泛的"结论先行"替代。
- **Keep 基线**：`keephq/keep` commit `99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760`，实际源码位于 `/tmp/keep-research`（浅克隆，静态阅读，未运行 Keep）。
- **Quoin 落点**（本次变更，均在 Go 后端）：
  - `internal/plinth/agent/agent.go`：`SystemPrompt`（initial-analysis-v2 代）、`InvestigationSystemPrompt`（investigation-v3 代）及其冻结的上一代常量与消息装配。
  - `internal/plinth/agent/inspection.go`：`InspectionSystemPrompt`（inspection-analysis-v3 代）及冻结的 v1/v2 代常量。
  - `internal/plinth/worker/worker.go`、`internal/plinth/worker/tools.go`、`internal/plinth/supervisor/supervisor.go`：按 `(schema_kind, agent_version)` 的代际路由与 prompt/digest 绑定。
  - `internal/quoin/attempt/tools.go`、`internal/quoin/attempt/agent.go`、`internal/quoin/attempt/catalog.go`、`internal/quoin/investigation/service.go`、`internal/quoin/knowledge/import.go`：执行身份、renderer 溯源映射与冻结目录。
- **代际纪律**：纯 prompt 文本变化，输入快照形状不变（analysis 快照 renderer 仍为 `initial-analysis-renderer-v4`，investigation 仍为 `investigation-renderer-v3`，巡检快照仍为 `v1`）；每类任务新增一代执行身份并保留全部旧代，在途旧 Attempt 逐字节按冻结 prompt 完成（见第 4 节）。

## 1. Keep 原文（pinned 源码逐字）

### 1.1 incident chat INSTRUCTIONS（`keep-ui/app/(keep)/incidents/[id]/chat/incident-chat.tsx:31-36`）

```
DO NOT, NO MATTER WHAT, MAKE UP ANY INFORMATION OR DATA. If you dont know - just say you don't know. Its ok. You are an expert incident resolver who's capable of resolving incidents in a variety of ways. You can get traces from providers, search for traces, create incidents, update incident name and summary, and more. You can also ask the user for information if you need it.
You should always answer short and concise answers, always trying to suggest the next best action to investigate or resolve the incident.
Any time you're not sure about something, ask the user for clarification.
If you used some provider's method to get data, present the icon of the provider you used.
If you think your response is relevant for the root cause analysis, add a "root_cause" tag to the message and call the "enrichRCA" method to enrich the incident.
```

### 1.2 独立摘要任务（`keep-ui/app/(keep)/incidents/[id]/incident-overview.tsx:102-105`，CopilotTask）

```
Generate a short concise summary of the incident based on the context of the alerts and the title of the incident. Don't repeat prompt.
```

## 2. 迁入映射：Keep 原文 → Quoin 对应规则

Keep 的共同规则在三个入口分别落实为各自系统提示词中的具体条款；下表"落点"均指新代提示词中的原文子串（可被 `internal/plinth/agent/keep_prompt_test.go` 的断言锚定）。

| # | Keep 原文（1.1 / 1.2） | Quoin 对应规则（中文化 + 领域适配） | 落点 |
| --- | --- | --- | --- |
| 1 | `DO NOT, NO MATTER WHAT, MAKE UP ANY INFORMATION OR DATA.` | 「无论任何情况，都不得编造任何信息或数据」+ 保留 Quoin 原有的「任何时候不要虚构未提供的数据」（分析/调查）与「不得引用未读取的内容」（巡检证据先读后写）。 | 三处新代 prompt 首句 |
| 2 | `If you dont know - just say you don't know. Its ok.` | 「不知道就明确说不知道，不要猜」（分析/调查/巡检统一措辞）。 | 三处新代 prompt 首句 |
| 3 | `You are an expert incident resolver ...` | 保留既有身份声明并延续：「你是 Quoin 的只读告警分析代理 / 只读运维调查代理 / 只读巡检报告代理」。专长语义由 Quoin 的只读工具与冻结证据承载，不声称 Quoin 不存在的技能清单。 | 三处新代 prompt 首句 |
| 4 | `You should always answer short and concise answers` | 「回答保持简短明确」「报告保持简短明确」；并按 Keep 摘要任务（1.2）落为：分析「开头先用一两句话给出结论」、巡检「开头先用一小段简短摘要给出整体结果」、调查「先直接回答当前问题」。 | 三处新代 prompt 第 1 条 |
| 5 | `Don't repeat prompt.`（摘要任务） | 「不复述提示词或完整原始数据」「不复述提示词、检查项清单或完整原始数据」「不复述提示词或完整工具输出」；正文只保留影响判断的关键数值与时间。 | 三处新代 prompt 第 1 条 |
| 6 | `always trying to suggest the next best action to investigate or resolve the incident` | 分析：「结尾给出下一步最合适的排查动作」；巡检：「结尾给出下一步最合适的核查或处理动作建议」；调查：「每次回答尽可能以建议下一步最合适的调查或处理动作收尾」。 | 三处新代 prompt 收尾条 |
| 7 | `Any time you're not sure about something, ask the user for clarification.` | 调查（唯一有往返对话的入口）：「对用户问题不确定或信息不足时，先向用户追问澄清，不要基于猜测作答」。分析与巡检无对话通道，按场景适配为既有的事实/假设边界与限制显式化（见第 3 行 4/5 条及未采用表 #6）。 | 调查新代 prompt 第 5 条 |
| 8 | `If you used some provider's method to get data, present the icon of the provider you used.` | 意图迁入、形式适配：「引用工具或证据时如实注明来源，不得伪造引用」。Quoin 的来源展示是真实 Evidence/工具引用，模型不生成图标，也不凭空标注来源。 | 分析第 4 条、调查第 4 条（巡检由证据先读与逐项引用覆盖） |
| 9 | （1.2）`Generate a short concise summary ... based on the context of the alerts` | Initial Analysis 的开头结论与 Inspection Report 的默认简短摘要（见 #4/#5）；巡检是对该模式的领域适配，Keep 公开源码没有对应的巡检提示词，不声称 Keep 有此能力。 | 分析/巡检新代 prompt |

同时逐条保留的 Quoin 既有约束（不以简短为由删除）：labels/annotations 原样引用、缺失不补全；名称/标签/注释不得推断故障；只读工具边界；「已知事实/待验证假设」「事实与推测」边界；数据缺口如实（巡检缺口不得当 0 或正常，未定义阈值不判断健康）；ALERTS 为空的解释边界与平台记录为准；数值与工具返回逐字一致；巡检证据先读、冻结报告要求严格执行并优先于默认行文约定、约束冲突显式指出；不外推整体业务健康。

## 3. 未采用条款（因领域/安全边界）

| # | Keep 原文条款 | 未采用原因 | Quoin 替代 |
| --- | --- | --- | --- |
| 1 | `You can get traces from providers, search for traces` | Keep 的 provider trace 工具在 Quoin 不存在；提示词不得声明不存在的工具。 | 既有冻结目录中的只读工具（`thanos_query`、`kubernetes_read` 历史代、workspace/artifact 工具），由冻结 catalog 决定，不由 prompt 声明。 |
| 2 | `create incidents, update incident name and summary, and more` | 同上，且 Quoin 模型无任何写权限（只读代理边界）。 | 无；分析/调查产出只经不可变输出提交。 |
| 3 | `You can also ask the user for information if you need it.`（分析/巡检场景） | Initial Analysis 是一次性任务、巡检报告无对话通道，不存在"向用户追问"的机制。 | 分析：待验证假设 + 限制显式化；调查（有对话）：完整迁入追问规则。 |
| 4 | `present the icon of the provider you used` | 图标是前端 provider 标识意图；Quoin 模型不生成图标，凭模型输出渲染来源图标属于伪造来源。 | 真实证据引用与来源归属（映射表 #8）；展示层归属由 UI 层按真实 Evidence 完成。 |
| 5 | `add a "root_cause" tag to the message and call the "enrichRCA" method` | `root_cause` 标签与 `enrichRCA` 是 Keep 专属工具，Quoin 不存在；迁入会凭空声明工具并引入第二套 RCA 通道。 | 保留其目的（形成可复用结论、关联依据）：既有诊断、知识整理（Candidate/Knowledge）与证据引用机制，不为提示词迁入新建 RCA 能力。 |
| 6 | `Incident resolver` 的隐含全技能人设（`capable of resolving incidents in a variety of ways`） | 可能诱导模型给出超出证据的"解决"承诺，与「Succeeded 只表示模型分析完成，不表示诊断已验证」冲突。 | 身份声明限定为只读分析/调查/巡检代理，结论必须区分事实与假设。 |
| 7 | Keep workflow builder 聊天指令、EE 服务端摘要任务等 | 与本功能无关，且服务端摘要任务函数在开源仓库缺失（见 `docs/research/keep-ai-presentation.md`），无可复制能力。 | 不迁移。 |

## 4. 版本、digest 与路由（在途兼容）

| 任务 | 新执行身份（新 Attempt） | 冻结保留的旧身份 | 旧身份 → 冻结 prompt | 新 renderer 溯源（model_calls.prompt_renderer_version） |
| --- | --- | --- | --- | --- |
| Initial Analysis | `initial-analysis-v2`（`attempt.AgentVersion`） | `initial-analysis-v1`（`attempt.PreviousAgentVersion`） | `PreviousAnalysisSystemPrompt` + `BuildPreviousInitialMessages`（消息同形） | v2 → `initial-analysis-renderer-v5`；v1 → `initial-analysis-renderer-v4`（原值不变） |
| Inspection Report | `inspection-analysis-v3`（`attempt.InspectionAgentVersion`） | `inspection-analysis-v2`（`ReportComplianceInspectionAgentVersion`）、`inspection-analysis-v1`（`PreviousInspectionAgentVersion`）、旧共享 `initial-analysis-v1` | v2 → `ReportComplianceInspectionSystemPrompt`；v1 → `PreviousInspectionSystemPrompt`；共享 → `LegacyInspectionSystemPrompt` | v3 → `inspection-analysis-renderer-v3`；v2 → `…-v2`；v1 → `…-v1`（均原值不变） |
| Investigation | `investigation-v3`（`investigation.AgentVersion`） | `investigation-v2`（`investigation.PreviousAgentVersion`）、`investigation-v1` | v2 → `PreviousInvestigationSystemPrompt` + `BuildPreviousInvestigationMessages`（保留 renderer-v3 历史块）；v1 → `LegacyInvestigationSystemPrompt`（无历史块） | v3 → `investigation-renderer-v4`；v2 → `…-v3`；v1 → `…-v2`（均原值不变） |
| Knowledge 抽取 | `initial-analysis-v1`（`attempt.KnowledgeAgentVersion`，显式固定原共享身份） | — | — | 其 prompt 从未演进，不随 analysis 代际升级；映射沿用 `initial-analysis-renderer-v4`（与既有行为一致） |

- 路由闭环：attempt 行 `agent_version` → dispatch → worker `verifyStart` 的 `(schema_kind, agent_version)` 代际路由（各代绑定各自 prompt 与消息装配）→ supervisor 同表选择 systemPrompt 供 `prompt_digest` → `promptRendererVersionFor` 记录 renderer 溯源；新代冻结目录经 `generationAccepts`（新增 `initial-analysis-v2`、`investigation-v3`，接受位置与前代一致）与 `catalogSchemaVersionFor`（investigation-v3 → `investigation-tools-v3`）装配。
- 输入快照形状未变：analysis/investigation 快照 renderer 常量不动，重建路径（`analysis/input.go`、`investigation/input.go`）不受影响；巡检快照仍为 `v1`。
- 字节兼容保证：旧 prompt 文本以独立常量逐字冻结并由测试锚定原文（`keep_prompt_test.go`、`inspection_identity_test.go`）；旧代消息装配仅系统提示词不同（`TestBuildPreviousInitialMessagesMatchesFrozenShape`）。

## 5. 测试与验收边界

- 新增/调整的回归：`internal/plinth/agent/keep_prompt_test.go`（三入口消息实际包含适配规则、旧代 prompt 原文锚定、旧代消息同形）、`internal/plinth/worker/inspection_identity_test.go`（四代巡检、三代调查、两代分析、knowledge 身份钉住、跨类型拒绝）、`internal/plinth/worker/tools_test.go`（双端身份镜像钉住）、`internal/quoin/attempt/inspection_version_test.go`（renderer 映射新旧逐值）、`internal/quoin/attempt/catalog_test.go`（新代冻结目录与 NULL-catalog 回退）。
- 这些测试验证的是"实际构造给模型的消息包含适配后的规则"，不能替代真实模型输出验收（简洁程度、下一步可执行性、证据真实性、限制保留需按 Issue #105 用同一组输入做迁入前后对比，缺少模型环境时如实记录未完成）。
- 直连模型的新旧 prompt 对比（若执行）属于提示词层面的补充观察：固定合成非敏感输入、逐字输出对比，不构成端到端工具调用验收；其结论不进入提示词常量或测试断言。

## 6. 直连模型新旧 prompt 对比（追加验收，2026-09-17）

- **方式**：固定合成非敏感输入（告警 occurrence JSON、巡检两条证据、一句调查提问），经部署 `.env` 的 OpenAI-compatible 端点各做一次同输入旧/新系统提示词对比；凭据只在脚本进程内存读取（`/tmp/prompt_compare.py`），不进入命令参数、日志、仓库或本文。巡检场景用最小工具回路回放两条固定合成证据（一条有数据、一条 `gapReason` 空结果），其余为单轮。
- **观察（节选归纳，非全文）**：
  - Initial Analysis：旧代以"已知事实"全量复述 labels/annotations 开头再给影响与排查顺序；新代先给"结论"段（含"不能证明 checkout 服务本身故障"的边界），显式列"未提供的关键信息"与"证据限制"，结尾落到"下一步最合适的动作是：从 thanos-demo 拉取…序列…比对"。关键数值与时间逐字保留。
  - Inspection Report：新旧代都先发起 `artifact_read` 读取全部证据（证据先读保持）；拿到证据后，新代以更短的总体结论开头、缺口如实（"不可当作 0 或正常"）、不对未定义阈值判健康；且新代显式识别出"结尾下一步建议"与本次冻结报告要求（仅一段结论+两列表格）的冲突，并**按冻结要求执行、不另设下一步章节**——冻结要求优先条款按预期生效。
  - Investigation：旧代开头即输出对不存在工具（`mcp__prometheus__query`）的调用意图；新代先答"我现在无法判断是否还在发生"（不知道直说），向用户追问环境/口径/时间三点，再给排查顺序（保留"ALERTS 为空≠没发生过告警"边界），并以"下一步"收尾。
- **限制**：单模型、单场景、各一次，模型为带推理开销的 reasoning 模型，输出长度与措辞不代表全部模型表现；巡检证据为脚本回放而非 Quoin 冻结 Artifact 通道；未覆盖多轮对话、工具失败/取消、上下文淘汰等真实执行路径。**该对比只能说明提示词层面的行为方向，不构成端到端验收**；Issue #105 要求的页面级验收与真实 Attempt 链路对比仍需在集成环境中完成。
