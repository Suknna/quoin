# Keep AI 呈现层研究：提示词、输出契约、前端渲染与信息分层（2026-09-17）

- **调研日期**：2026-09-17
- **Keep 基线**：`keephq/keep` `main` HEAD **commit `99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760`**（2026-09-13），浅克隆至 `/tmp/keep-research` 后静态阅读；下文 Keep 引用一律用该 SHA 的 GitHub blob 链接 + 行号。
- **Quoin 基线**：当前工作区状态；Quoin 事实用 `绝对路径:行号` 引用。
- **方法与边界**：纯静态源码阅读，未启动 Keep 服务、未执行其代码；行号来自本地 checkout（与 pinned SHA 应一致，如有漂移以文件为锚）。克隆为 `--depth 1`，无法核对历史演进；不涉及 Quoin 业务代码修改。无引用来源的陈述不进入本文。
- **一句话结论**：Keep 的"AI 呈现"是三层拼装——前端 CopilotKit 承担全部提示词编排与工具调用（incident chat、workflow builder chat），后端只有一处带严格 `json_schema` 输出契约的告警聚类（`ai_suggestion_bl.py`），服务端 incident 摘要生成在开源仓库中被 EE 门控且**任务函数本身缺失**（`process_summary_generation` 全仓无定义），不构成可运行能力；前端信息分层靠"每条告警自带 `description_format` + 统一 `FormattedContent`/`MarkdownHTML` 消毒管道 + 工具结果内联卡片"实现。

## 1. 能力真实性总账（先给判定，防以名推测）

| 能力 | 真实性 | 证据 |
| --- | --- | --- |
| Incident chat（CopilotKit，前端编排） | 真实实现，开源可用（需 `OPEN_AI_API_KEY`） | [page.client.tsx:18](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/chat/page.client.tsx#L18)、[route.ts:1-42](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/api/copilotkit/route.ts#L1-L42) |
| 服务端 incident AI 摘要（EE 路径） | **仅入队，函数不在仓库**；`EE_ENABLED`+Redis+告警数>5 三重门控 | [incidents_bl.py:228-247](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep/api/bl/incidents_bl.py#L228-L247)；全仓 grep `process_summary_generation` 仅 1 处引用（下文 2.1） |
| 前端 AI Summary 按钮（CopilotTask 客户端生成） | 真实实现 | [incident-overview.tsx:101-158](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/incident-overview.tsx#L101-L158) |
| RCA（root cause analysis） | **无独立 RCA 引擎**：聊天内人工"Add to RCA"或模型调用 `enrichRCA` 追加单条要点，存 incident enrichments | [incident-chat.tsx:164-190](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/chat/incident-chat.tsx#L164-L190)、[RootCauseAnalysis.tsx:18-24](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/components/ui/RootCauseAnalysis.tsx#L18-L24) |
| AI 告警聚类建 incident（结构化输出） | 真实实现，`json_schema` 强契约 + Pydantic 二次校验 | [ai_suggestion_bl.py:407-466](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep/api/bl/ai_suggestion_bl.py#L407-L466) |
| Workflow builder AI chat | 真实实现，含人在环确认 | [WorkflowBuilderChat.tsx:163+](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/features/workflows/ai-assistant/ui/WorkflowBuilderChat.tsx#L163) |
| AI Plugins / 外部算法落地页 | 壳在仓库，算法本体是外部服务（需 `KEEP_EXTERNAL_AI_TRANSFORMERS_URL`），默认空 | [db.py:5519-5539](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep/api/core/db.py#L5519-L5539) |
| `ee/` 目录 | 只有 identitymanager（Keycloak/Auth0/AzureAD），无 AI 代码 | [ee 目录树](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/ee) |
| Preset 名称生成 | 小功能：CopilotTask 从 CEL 查询生成 preset 名 | [create-or-update-preset-form.tsx:102-118](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/features/presets/create-or-update-preset/ui/create-or-update-preset-form.tsx#L102-L118) |

## 2. Incident AI Summary：两条路径，一条不可运行

### 2.1 服务端 EE 路径：入队了，但函数缺失

`IncidentsBl.__generate_summary` 的门控条件为 `ee_enabled and self.redis and fingerprints_count > MIN_INCIDENT_ALERTS_FOR_SUMMARY_GENERATION(默认5) and not incident.user_summary`，随后向 arq 队列入队 `process_summary_generation`：[incidents_bl.py:228-247](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep/api/bl/incidents_bl.py#L228-L247)。其中 `ee_enabled = os.environ.get("EE_ENABLED", "false") == "true"`（[incidents_bl.py:44-48](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep/api/bl/incidents_bl.py#L44-L48)）。

关键核实：在该 pinned commit 全仓范围内（`*.py/*.yml/*.yaml/*.toml`，排除 `.git`），任务名 `process_summary_generation` 仅发现 `incidents_bl.py:237` 的入队调用；另有 `keep/api/models/db/ai_suggestion.py:11` 的相关枚举常量 `SUMMARY_GENERATION = "summary_generation"`，它不是任务函数定义。arq worker 默认注册表只含 process_event/process_topology/process_incident 三个任务（[arq_worker.py:30-52](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep/api/arq_worker.py#L30-L52)），其余函数须经 `ARQ_BACKGROUND_FUNCTIONS` 环境变量以 `import_string` 注入（[arq_worker.py:53-63](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep/api/arq_worker.py#L53-L63)），仓库内没有任何模块定义它。**结论：开源版该路径只能入队、无消费者执行，真正的摘要生成代码不在开源仓库。**

### 2.2 开源实际路径：前端 CopilotTask 客户端生成

Incident 详情页 Summary 字段旁的 "AI Summary" 按钮（[incident-overview.tsx:146-157](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/incident-overview.tsx#L146-L157)，`disabled={!generatingSummary || !config?.OPEN_AI_API_KEY_SET}`）触发一个浏览器端 `CopilotTask`，指令原文：

> `Generate a short concise summary of the incident based on the context of the alerts and the title of the incident. Don't repeat prompt.`（[incident-overview.tsx:101-104](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/incident-overview.tsx#L101-L104)

任务的落盘方式是前端定义的 `setGeneratedSummary` action：模型产出即调用它，把文本经 `updateIncident(..., {user_summary: summary}, true)` 写回（第二参数 `generatedByAi=true`）（[incident-overview.tsx:85-99](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/incident-overview.tsx#L85-L99)。任务上下文由 `useCopilotReadable` 注入 incident alerts 与标题（同文件 77-84）。**即：该路径由客户端声明上下文、任务指令与回写 action，经 Next.js 的 CopilotKit 服务端路由调用模型，再通过更新接口写入 `user_summary`。这条生成路径未呈现 Quoin 式的冻结 Attempt 中间态；不能据此推断整个服务端没有审计，也不能理解为浏览器直接持有 OpenAI key。**

## 3. Incident chat：提示词、工具契约与流式工具展示

前端栈为 CopilotKit：`/api/copilotkit` Next.js route 在浏览器服务侧持有 OpenAI key 并构造 `CopilotRuntime` + `OpenAIAdapter`（[route.ts:7-27](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/api/copilotkit/route.ts#L7-L27)）；页面仅在 `config.OPEN_AI_API_KEY_SET` 时挂载 chat（[page.client.tsx:17-20](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/chat/page.client.tsx#L17-L20)）。

### 3.1 系统指令（INSTRUCTIONS 原文）

[incident-chat.tsx:31-36](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/chat/incident-chat.tsx#L31-L36)：

> `DO NOT, NO MATTER WHAT, MAKE UP ANY INFORMATION OR DATA. If you dont know - just say you don't know. Its ok. You are an expert incident resolver ... You should always answer short and concise answers, always trying to suggest the next best action to investigate or resolve the incident. ... If you used some provider's method to get data, present the icon of the provider you used. If you think your response is relevant for the root cause analysis, add a "root_cause" tag to the message and call the "enrichRCA" method to enrich the incident.`

要点：反虚构置于首位；"下一步最佳动作"导向；要求回答**短而具体**；用 provider 图标标注数据出处；给 RCA 追加定义了"打 tag + 调 action"的显式协议。

### 3.2 上下文注入（声明式 Readable）

`useCopilotReadable` 注入：聊天用户、`incidentDetails`（整个 incident DTO）、`alerts`（整批告警 DTO）、可拉 trace 的 provider id 列表、已装 provider 及其可调用方法清单（[incident-chat.tsx:135-155](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/chat/incident-chat.tsx#L135-L155)。即模型可见的"证据面"= 实体 + 告警 + provider 能力目录，全部为前端声明式提供。

### 3.3 工具（useCopilotAction）契约与信息分层

6 个前端定义工具（参数即输出契约，全部为声明式 description + 类型）：

| 工具 | 行号 | 作用 | 结果去向 |
| --- | --- | --- | --- |
| `enrichRCA` | [164-190](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/chat/incident-chat.tsx#L164-L190) | 追加一条 RCA 要点 `{content, providerType}` | 写 `incident.enrichments.rca_points`（持久、进实体） |
| `invokeProviderMethod` | [193-229](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/chat/incident-chat.tsx#L193-L229) | 万能 provider 方法调用 | 非 string 结果写入 enrichments（按 func_name 键） |
| `createIncident` | [231-313](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/chat/incident-chat.tsx#L231-L313) | 在外部工单系统建单（severity enum `SEV-1..UNKNOWN`） | 回填 `incident_url/id/provider/title` 到 enrichments |
| `invokeGetTrace` | [315-370](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/chat/incident-chat.tsx#L315-L370) | 拉 provider trace | trace 存 `enrichments.traces[traceId]`；**自带 `render` 回调做内联流式展示** |
| `searchTraces` | [371-400](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/chat/incident-chat.tsx#L371-L400) | 按 `alert.alert_query` 搜 trace | 返回值进对话 |
| `updateIncidentNameAndSummary` | [402-430](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/chat/incident-chat.tsx#L402-L430) | 改名/摘要 | 写回实体 |

信息分层设计：**RCA 要点、外部单号、trace 等结果除参与聊天外，还可写入 incident enrichments，供实体页面单独展示**。这说明结果有独立归属，但不意味着它们会从聊天流删除，也不保证模型正文不会重复证据。

### 3.4 流式工具展示（render 回调）

`invokeGetTrace` 是唯一带自定义 `render` 的工具（[incident-chat.tsx:356-368](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/chat/incident-chat.tsx#L356-L368)：`executing/inProgress` → loading 按钮；`complete` → `<SimpleTraceViewer trace={result}/>`；否则 "Trace not found" 卡片）。`SimpleTraceViewer` 把 span 树算成层级时间线并带 span 详情 tooltip（含 `db.statement` 等元数据）（[TraceViewer.tsx:49-221](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/shared/ui/TraceViewer/TraceViewer.tsx#L49-L221)。其余工具执行期间仅由 CopilotKit 默认样式展示（本仓库 chat CSS 未覆盖 markdown 区）。

### 3.5 会话持久化与"Add to RCA"

- 消息按 incident id 存 localStorage，刷新后按 `TextMessage/ActionExecutionMessage/ResultMessage` 三类还原（[incident-chat.tsx:56-91](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/chat/incident-chat.tsx#L56-L91)。
- 用 MutationObserver 扫 DOM 给每条 assistant 消息注入"Add to RCA"按钮，点击后把该条消息文本交给 `rcaTask`（一个 `CopilotTask`，指令为把响应加进 rca points 并写入外部 incident timeline）（[incident-chat.tsx:318-321](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/chat/incident-chat.tsx#L318-L321)、DOM 注入 [434-537](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/chat/incident-chat.tsx#L434-L537)。
- incident 无告警时整个 chat 不渲染（`EmptyStateCard`，[546-553](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/chat/incident-chat.tsx#L546-L553)。

## 4. RCA 的实际形态

RCA 不是独立服务端能力，而是三种入口汇入同一存储 `incident.enrichments.rca_points`：

1. 模型按 INSTRUCTIONS 协议主动调 `enrichRCA`（3.3 表）；
2. 用户点击"Add to RCA"→ CopilotTask 从消息提取 → 同一 action；
3. 详情页渲染：`incident-overview.tsx:554-556` 检测 `"rca_points" in incident.enrichments` 后渲染 [RootCauseAnalysis.tsx](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/components/ui/RootCauseAnalysis.tsx#L18-L90)——一个带脉动绿点的 "Investigation" 徽标 + HoverCard 弹层，逐条列出要点：provider 图标 + **纯文本 content（无 markdown、无时间线、无证据回链）**。

## 5. 结构化输出契约：AI 告警聚类（后端唯一强契约点）

`AISuggestionBl.suggest_incidents`（[ai_suggestion_bl.py:233-336](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep/api/bl/ai_suggestion_bl.py#L233-L336)，≤50 条告警）：

- **缓存**：以告警 fingerprints 排序后 sha256 为输入哈希，命中即复用历史 suggestion（[243-256](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep/api/bl/ai_suggestion_bl.py#L243-L256)。
- **系统提示词**（[370-391](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep/api/bl/ai_suggestion_bl.py#L370-L391)，原文摘录）：`You are an advanced AI system specializing in IT operations and incident management. Your task is to analyze the provided IT operations alerts and topology data, and cluster them into meaningful incidents. ... For each incident: 1. Assess its severity 2. Recommend initial actions ... 3. Provide a confidence score (0.0 to 1.0) ... 4. Explain how the confidence score was calculated ...`
- **用户提示词**：`Alert N: {alert.dict() JSON}` 逐条 + `Topology N: {...}` 逐条 + `Provide your analysis and clustering in the specified JSON format.`（[393-405](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep/api/bl/ai_suggestion_bl.py#L393-L405。
- **输出契约**：OpenAI `response_format={"type":"json_schema", ...}`，根 schema `{incidents: [...]}`，每项 required：`incident_name / alerts(1-based 序号) / reasoning / severity / recommended_actions / confidence_score / confidence_explanation`，severity enum `[critical, high, warning, info, low]`，temperature 0.2（可经 env 关闭）（[407-466](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep/api/bl/ai_suggestion_bl.py#L407-L466、[ai_utils.py:16-22](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep/api/utils/ai_utils.py#L16-L22)。
- **二次校验**：返回文本再过 Pydantic `IncidentClustering.parse_raw`（[271](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep/api/bl/ai_suggestion_bl.py#L271），模型见 [incident.py:314-334](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep/api/models/incident.py#L314-L334。
- **落地与反馈**：聚类结果转成 `is_candidate=true` 的候选 incident；用户逐条接受/拒绝后 `commit_incidents` 才真正建 incident，并把每条的修改作为 `AIFeedback` 落库（[301-349](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep/api/bl/ai_suggestion_bl.py#L301-L349。
- **缺陷样本**：json_schema 的 severity 枚举（小写 5 值）与 Pydantic `IncidentCandidate.severity` 的 `Field(enum=["Low","Medium","High","Critical"])`（[320-323](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep/api/models/incident.py#L320-L323）不一致；Pydantic v1 的 `Field(enum=)` 只进 JSON Schema 文档、不做值校验，所以运行不炸，但两份契约已经漂移——**双契约必须单一来源**的教训。

## 6. Workflow builder chat：人在环（renderAndWaitForResponse）

- 系统指令 [GENERAL_INSTRUCTIONS](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/features/workflows/ai-assistant/lib/constants.ts#L1-L227)：把 trigger/step/action/assert/threshold/foreach 的 JSON 形状全部内联进提示词（作者自注 `TODO: replace with zod schema`），并给出 `{{alert.<property>}}` 模板语法。
- 上下文注入：当前 workflow 摘要（nodes/edges 投影，[utils.ts:36-50](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/features/workflows/ai-assistant/lib/utils.ts#L36-L50）、已装 provider 列表（经 `convert` 压缩为 `type, id` 串）、可用步骤 toolbox、可选告警字段（[WorkflowBuilderChat.tsx:84-148](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/features/workflows/ai-assistant/ui/WorkflowBuilderChat.tsx#L84-L148。
- 动态建议：`useCopilotChatSuggestions` 按 workflow 状态给 1-3 条下一句建议（[150-157](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/features/workflows/ai-assistant/ui/WorkflowBuilderChat.tsx#L150-L157。
- **人在环核心**：`addManualTrigger/addAlertTrigger/addIntervalTrigger/addIncidentTrigger/addAction/addStep` 等全部用 `renderAndWaitForResponse`（[214, 256, 317, 381, 429, 479, 610, 738, 862, 945](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/features/workflows/ai-assistant/ui/WorkflowBuilderChat.tsx#L610)。流程：模型发起工具调用 → 聊天流里渲染预览卡片（`AddTriggerOrStepSkeleton` → `StepPreview` + "Do you want to add this trigger to the workflow?" + `Add (⌘+Enter)` / `No` 按钮，[AddTriggerUI.tsx:92-125](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/features/workflows/ai-assistant/ui/AddTriggerUI.tsx#L92-L125）→ 用户接受则真正改 workflow 画布并 `respond({status:"complete", message})`，拒绝则 `respond({status:"declined"})`——**工具结果回传给模型继续推理**，形成"提议-确认-反馈"闭环；完成后用 `SuggestionStatus` 图标 + 文案回显终态（[SuggestionStatus.tsx:13-44](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/features/workflows/ai-assistant/ui/SuggestionStatus.tsx#L13-L44）。
- 参数描述承载结构约束的技巧：如 `addBeforeNodeId` 的 description 直接写明 true/false 分支节点 id 后缀约定 `__empty_true/__empty_false`（[569-578](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/features/workflows/ai-assistant/ui/WorkflowBuilderChat.tsx#L569-L578）。
- 前端还有一层真实防错：模型给的 step/trigger 定义必须通过 Zod（`V2StepTriggerSchema.parse` 等，[306-310](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/features/workflows/ai-assistant/ui/WorkflowBuilderChat.tsx#L306-L310）才进画布。

## 7. 前端渲染分层：markdown 组件与原始证据

### 7.1 格式声明进数据契约

`AlertDto.description_format: str|None`，注释 `Can be 'markdown' or 'html'`，后端 validator 白名单校验（[alert.py:94, 240-248](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep/api/models/alert.py#L94-L248）。**渲染格式是每条告警数据自身的字段，而不是前端猜测**——不同 provider 推来的告警描述格式各异，由数据声明解决歧义。

### 7.2 统一渲染管道

- [FormattedContent.tsx](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/shared/ui/FormattedContent/FormattedContent.tsx#L26-L100)：三格式 `markdown | html | plain`；html 走 rehype `parse → sanitize → stringify` 后 `dangerouslySetInnerHTML`；`plain` 剥标签用于表格单元格 line-clamp。
- [MarkdownHTML.tsx](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/shared/ui/MarkdownHTML/MarkdownHTML.tsx#L1-L18)：`react-markdown + remark-gfm + remark-rehype + rehype-raw + rehype-sanitize`，注释明确"唯一允许渲染 markdown/HTML 的组件"。
- 应用点：incident summary（prose 容器，[incident-overview.tsx:112-115](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/incident-overview.tsx#L112-L115）、incident 列表摘要（`format="html" plain` + line-clamp-2，[incidents-table.tsx:196-203](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/features/incidents/incident-list/ui/incidents-table.tsx#L196-L203）、告警详情侧栏 Description（[alertSidebarFields.tsx:121-128](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/features/alerts/alert-detail-sidebar/lib/alertSidebarFields.tsx#L121-L128)、AI 建 incident 卡片中的告警描述（[alert-create-incident-ai-card.tsx:110-115](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/features/alerts/alert-create-incident-ai/ui/alert-create-incident-ai-card.tsx#L110-L115。

### 7.3 时间线/活动/证据

- incident timeline 的事件详情面板：告警名+fingerprint 为标题，`FormattedContent(content=alert.description, format=alert.description_format)` 为正文，右侧 4 列网格列 Date/Action/Description/Severity/Source(provider 图标)/Status（[incident-timeline.tsx:56-100](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/timeline/incident-timeline.tsx#L56-L100。
- activity 评论区：含 mention/HTML 时走 `FormattedContent format="html"`，否则纯文本（[IncidentActivityItem.tsx:19-27](https://github.com/keephq/keep/blob/99e3ffd4e2a63bd3e04d1bc48be3af6048d9b760/keep-ui/app/%28keep%29/incidents/%5Bid%5D/activity/ui/IncidentActivityItem.tsx#L19-L27。
- 聊天消息本身的 markdown 渲染来自 `@copilotkit/react-ui` 库默认实现（仓库未覆写）。

## 8. 与 Quoin 对照：可借鉴与不可照搬

Quoin 现状（仅列对照所需事实）：investigation 走 plinth 冻结输入，`investigation-v2` 系统提示词固定于 [internal/plinth/agent/agent.go:127-132](/home/suknna/code/quoin/internal/plinth/agent/agent.go)（数值逐字一致、区分平台告警事实与即时状态）；renderer 代际 `investigation-renderer-v3`（[internal/quoin/investigation/service.go:49](/home/suknna/code/quoin/internal/quoin/investigation/service.go)）；巡检报告提示词 v2 明确"输入提供了冻结的报告要求，必须严格执行其中声明的格式、字段、章节和长度等约束"（[internal/plinth/agent/inspection.go:19-21](/home/suknna/code/quoin/internal/plinth/agent/inspection.go)）；前端 assistant 消息用 ReactMarkdown+remarkGfm 安全渲染、原始 HTML 逃逸，工具调用在底部折叠面板里看原始 JSON，证据按 id 按钮回链（[web/src/features/investigation/ui/index.tsx:153-172](/home/suknna/code/quoin/web/src/features/investigation/ui/index.tsx)）。

**澄清（对上一轮截图的 JSON 输出）**：该 JSON 出自巡检计划冻结报告要求（如 `mall-mysql-json` 的"无围栏单个 JSON、固定 7 字段"约束，见 [docs/acceptance/mall-shop-model-20260916.md](/home/suknna/code/quoin/docs/acceptance/mall-shop-model-20260916.md) "三种巡检"节），并由 InspectionSystemPrompt v2 的冻结要求条款强制执行——**不是模型自发选择 JSON**，本研究所述以此为准。

### 8.1 可借鉴

1. **渲染格式进数据契约**（Keep `description_format`）：Quoin 的 evidence/告警类数据若将来支持富文本描述，应在数据上声明格式并配白名单 validator，而不是渲染端猜。
2. **工具结果内联流式卡片**（Keep `invokeGetTrace.render`：loading → 结果卡片）：Quoin 目前工具调用收在底部 `ToolCalls` 抽屉（点开才见）；对"拉一段 trace/日志"这类中间证据，内联渲染比抽屉更符合阅读顺序。可作 investigation 前端的增量选项，不必替换现有审计视图。
3. **人在环 respond 回传**（Keep `renderAndWaitForResponse`）：Quoin 的"安全缓解与回滚"类建议若将来允许执行，Keep 的"提议卡片 → 接受/拒绝 → 把用户决定作为工具结果回传模型继续推理"是现成模式；Quoin 已有 undo/retry 与反馈按钮，缺的是把决定回灌给模型的环节。
4. **结论性内容与聊天分离**（Keep `rca_points` 进 enrichments）：与 Quoin "证据/结论落库、聊天只是触发器"的方向一致；Keep 额外的 provider 图标注源（INSTRUCTIONS 里"用了哪个 provider 就展示其图标"）可借鉴为证据来源可视化。
5. **suggestion 缓存 + 用户反馈落库**（Keep AISuggestion/AIFeedback，输入哈希复用 + 接受/拒绝记录）：Quoin 巡检若做"重新分析"，可参考其输入哈希缓存与逐条采纳反馈表。
6. **参数 description 承载结构约束**（Keep `addBeforeNodeId` 的分支 id 约定写在参数描述里）：Quoin 的工具 schema（tool_catalog）也可把"必须先读 artifact 再引用"等使用约束写进参数/工具描述，减少系统提示词膨胀。

### 8.2 不能照搬

1. **key 与编排位置**：Keep 把 OpenAI key 放在 Next.js route、提示词与工具编排全在浏览器（第 3 节）。Quoin 的模型调用必须经 plinth 冻结审计（prompt_digest/tool_schema_digest/rendered_request_digest，[internal/quoin/attempt/agent.go:215-220](/home/suknna/code/quoin/internal/plinth/agent/agent.go)），**不可能也不应该**把这套 CopilotKit 式客户端编排搬过来；能借鉴的只是 UI 呈现模式，不是执行架构。
2. **客户端生成直接覆盖权威字段**：Keep 前端 AI Summary 直接写 `user_summary` 且服务端无审计中间态（2.2）；与 Quoin "报告必须基于逐字核对证据、原文留痕"的纪律冲突，Quoin 的等价物必须走 attempt/evidence 链。
3. **MutationObserver DOM 注入按钮**（3.5）：绕过 React 树、依赖 CopilotKit 内部 DOM 结构，属脆弱实现；Quoin 需要同类按钮应走组件层。
4. **双契约漂移**：Keep json_schema 与 Pydantic 枚举不一致（第 5 节缺陷样本）。Quoin 的冻结报告要求和提示词自检不等于程序化 schema 校验，不能保证规避契约漂移；增加结构化输出时应避免维护两份独立手写 schema。
5. **"无告警则 chat 不可用"前提**：Keep incident chat 依赖 alerts 注入（3.5）；Quoin investigation 以用户问题为中心、告警只是可选上下文，不适用该门控。
6. **EE 门控的"影子代码"**：`__generate_summary` 这类引用了不存在函数的代码路径会误导读者（2.1）。Quoin 不保留死代码的约定（AGENTS.md 架构节）与 Keep 形成对照，无需效仿。

## 9. 检索范围与限制（重申）

- 只读 pinned commit `99e3ffd4`（2026-09-13 main HEAD）；`--depth 1` 无历史；未运行任何 Keep 代码，"真实性"判定基于源码可达性而非运行验证。
- 已覆盖：`keep/api/bl/incidents_bl.py`、`keep/api/bl/ai_suggestion_bl.py`、`keep/api/routes/ai.py`、`keep/api/arq_worker.py`、`keep/api/tasks/process_incident_task.py`、`keep/api/models/{incident,alert,ai_external}.py`、`keep/api/core/db.py`（外部 AI 段）、`keep-ui` incident chat/overview/timeline/activity、workflow AI assistant 全目录、alerts AI 建 incident、presets AI 表单、`shared/ui/{FormattedContent,MarkdownHTML,TraceViewer}`、`app/api/copilotkit/route.ts`、`ee/` 目录。
- 未展开：CopilotKit 库内部实现（外部依赖，未读其源码）、litellm/openai/vllm provider 细节（它们是 workflow 数据 provider，与呈现层无关）、Keep workflow 引擎本身。
