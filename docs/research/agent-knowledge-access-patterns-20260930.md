# 主流 Agent / Wiki 项目的知识访问方式调研（2026-09-30）

> 研究阶段说明：本文中的 Quoin 对照包含当时的 Git 候选。最终选择已改为 SQLite 权威 + Plinth 常驻文件副本；通用文件工具方向保留。实施以 [Issue #111](https://github.com/Suknna/quoin/issues/111) 与 [最终设计](../llm-wiki-design.md) 为准。

范围：仅调研「知识/记忆如何被访问」——工具形态（标准 read/write/bash/grep vs 专用工具）、上下文注入（会话启动自动加载 vs 模型主动检索）、索引/记忆的加载与维护触发。不含存储选型与实现方案。结论全部来自官方文档、官方仓库源码或作者原文，链接见各节。

## 结论速览

| 系统 | 持久层 | 模型侧访问工具 | 自动注入 | 模型主动检索 | 索引/维护触发 |
|---|---|---|---|---|---|
| Claude Code | CLAUDE.md / auto memory（纯 md） | 内置 Read/Edit/Bash/Glob 等 | 是：启动时加载；子目录/路径规则按需 | 是：Grep/Read | 无索引；文件由人或 Claude 编辑 |
| Claude Agent SDK | 同上 + memory tool（`/memories` 文件） | 内置工具 + `memory` 工具（客户端实现） | 同 Claude Code；memory tool 启用后模型开始任务前自动查看目录 | 是 | 无索引；模型调用 view/create/… 读写 |
| OpenAI Codex | AGENTS.md（纯 md） | `shell`/`unified_exec`、`apply_patch`、`view_image`、`web_search` | 是：每次 run 构建指令链（默认 ≤32 KiB） | 是：shell 内 grep 等 | 无索引 |
| Karpathy LLM Wiki | 普通 md 目录 + git | 复用所在编码 agent 的标准工具 | 部分：schema 走 CLAUDE.md/AGENTS.md 机制 | 是：先读 index.md 再钻取 | index.md / log.md 由 LLM 维护（软件无索引） |
| Basic Memory | md 文件（唯一事实源）+ 派生 SQLite 索引 | 专用 MCP 工具 | 否：上下文按需构建 | 是：`search_notes` / `build_context` | SQLite FTS5；MCP 写入即更新，外部改动靠 `sync` / `sync --watch` |

要点纠偏：**没有一家把整个 wiki 自动注入上下文**；自动注入的只有小型指令/schema 文件，知识内容一律靠模型主动检索。也没有一家默认必须用向量（Basic Memory v0.19.0 起才加入向量混合搜索，Karpathy 明确规避 embedding 基础设施，Claude Code/Codex 无索引）。

## 1. Claude Code

- **工具形态**：内置文件与 shell 类工具。官方 SDK 文档示例即 `allowed_tools=["Read","Edit","Bash"]`、`["Bash","Glob"]`，并说明 SDK「内置读取文件、运行命令、编辑代码的工具」。Claude Code 本体即这套工具（[Agent SDK overview](https://platform.claude.com/docs/en/agent-sdk/overview)）。
- **自动注入（会话启动）**：每个会话从空上下文开始；两种跨会话机制均为 md 文件——CLAUDE.md（人写）+ auto memory（Claude 依纠正/偏好自动记笔记），**两者都在每次会话开始时加载**，且作为 user message 注入而非 system prompt（[memory 文档](https://code.claude.com/docs/en/memory)）。
  - 加载规则：cwd 及所有祖先目录的 CLAUDE.md/CLAUDE.local.md 启动时拼接加载；`@path` import 递归展开（最多 4 跳）也在启动时；**子目录 CLAUDE.md 不预载，待 Claude 读到该目录文件时才载入**；`.claude/rules/` 可用 `paths:` frontmatter 做路径作用域规则，同样按需触发。
  - 大小约束：单文件建议 <200 行，>4 MiB 直接跳过——官方以「上下文预算」而非「建索引」来控制注入量。
  - 兼容 AGENTS.md：**默认仅当工作目录及祖先无任何 CLAUDE.md/CLAUDE.local.md 时才读 AGENTS.md**（可经 `Project instructions` 设置改为两者都读）。注意：文档存在 ≠ 一定加载。
- **主动检索**：知识内容不在注入范围内，靠 Grep/Read 按需查。
- **维护触发**：CLAUDE.md 由人编辑或明确要求 Claude 写入；auto memory 由 Claude 在被要求「记住…」时写入，均为纯 md，`/memory` 可浏览编辑；`/compact` 后项目根 CLAUDE.md 会从磁盘重读并重注入。

## 2. Claude Agent SDK

- **工具形态**：「与 Claude Code 相同的工具、agent loop 与上下文管理」，内置工具由 SDK 提供执行；另有 Anthropic 提供的 **memory tool**（`{"type":"memory_20250818","name":"memory"}`）。SDK 还可通过 `setting_sources` 加载 Claude Code 的文件式配置（Skills、Slash commands、Memory=CLAUDE.md、Plugins）（[SDK overview](https://platform.claude.com/docs/en/agent-sdk/overview)）。
- **memory tool**（[memory-tool 文档](https://platform.claude.com/docs/en/agents-and-tools/tool-use/memory-tool)）：**客户端实现**——模型只发出文件操作请求（view/create/str_replace/insert 等命令），存储位置与后端由宿主应用决定（磁盘文件、数据库均可），路径限制在 `/memories` 前缀下。定位是 **just-in-time 检索**：不预先加载全部记忆，「启用后 Claude 在开始任务前会自动查看 memory 目录」，随后按需读写。仍无索引——靠目录 `view` 与文件读取。
- **维护触发**：模型在会话中主动写；持久化逻辑完全由宿主实现。

## 3. OpenAI Codex

- **自动注入**：Codex 在动手前读取 AGENTS.md，**每次 run（TUI 为每个会话）构建一次指令链**，发现顺序（[AGENTS.md 指南](https://developers.openai.com/codex/guides/agents-md)）：
  1. 全局：`~/.codex/AGENTS.override.md`，否则 `AGENTS.md`（取第一个非空）；
  2. 项目：从项目根（通常为 git root）向下走到 cwd，每目录至多一个文件（优先 `AGENTS.override.md` > `AGENTS.md` > `project_doc_fallback_filenames`）；
  3. 合并：根→cwd 顺序拼接，靠近 cwd 的后出现（实际生效优先）；空文件跳过；总计达 `project_doc_max_bytes`（**默认 32 KiB**）即截断。
- **工具形态**：以沙箱 shell 为核心（[CLI features](https://developers.openai.com/codex/cli/features)）：审批模式描述为「在工作目录内读文件、编辑、运行命令」。官方 [local-config](https://developers.openai.com/codex/local-config) 的 `[features]` 表可见工具开关：`unified_exec`（PTY-backed exec 工具）、`shell_snapshot`、`apply_patch_freeform`（freeform `apply_patch` 工具）、`view_image_tool`（默认开）、`web_search_request`；仓库源码的工具处理器亦为 shell/unified_exec、apply_patch、mcp、multi_agents（[codex-rs/core/src/tools/handlers/](https://github.com/openai/codex/tree/main/codex-rs/core/src/tools/handlers)）。文件编辑通过 `apply_patch` 补丁而非逐字 Write；`web_search` 为内置工具，CLI 默认走 OpenAI 缓存结果，`--search` / `web_search="live"` 才实况抓取。
- **主动检索 / 索引**：无知识索引；模型在 shell 里 grep/cat 自行检索。

## 4. Karpathy LLM Wiki（作者原文 gist）

主源：[LLM Wiki — gist.github.com/karpathy](https://gist.github.com/karpathy/442a6bf555914893e9891c11519de94f)（作者自述为「idea file」，设计为直接粘贴给 OpenAI Codex / Claude Code / OpenCode 等编码 agent 使用）；后续推广帖见 [X/status/2040470801506541998](https://x.com/karpathy/status/2040470801506541998)。

- **本质**：不是产品，是**跑在普通编码 agent 上、只用其标准文件工具的模式**。原文：「The wiki is just a git repo of markdown files」。作者工作流即 Claude Code/Codex 一侧 + Obsidian 一侧实时浏览。
- **三层架构**：raw sources（不可变原始文档，唯一事实源）→ wiki（LLM 全权读写的互链 md 页面）→ schema（一份 CLAUDE.md/AGENTS.md，定义结构、约定与 ingest/query/lint 工作流——复用各 agent 的自动注入机制）。
- **访问方式 = 主动检索 + 自维护索引**：query 时「LLM 先读 index.md 找到相关页，再钻取阅读」。index.md（全目录+一行摘要，每次 ingest 更新）与 log.md（追加式日志）是**由 LLM 维护的 md 索引**，不是软件索引；作者明确说这一套「在 ~100 源、数百页规模下出奇地好，**避免了 embedding 式 RAG 基础设施**」。
- **维护触发**：ingest（新源入 wiki，一次可能改 10–15 页）、query（好答案回填成新页）、lint（定期健康检查）均由人发起会话驱动；无后台进程、无 watcher。
- **可选扩展**：规模变大后可引入外部搜索（如 [qmd](https://github.com/tobi/qmd)：本地 BM25/向量混合，提供 CLI 或 MCP 两种接法）——明确标注为可选阶段。

## 5. Basic Memory（basicmachines-co/basic-memory）

主源：[官方 README](https://github.com/basicmachines-co/basic-memory/blob/master/README.md) 与仓库源码；文档站 [docs.basicmemory.com](https://docs.basicmemory.com)。

- **文件权威性（已验证）**：「keeping everything in simple Markdown files」；技术实现第一条即「Stores everything in Markdown files」；本地知识图谱/SQLite 索引是**从文件派生**的（「Maintains the local knowledge graph derived from the files」「Just local files indexed in a local SQLite database」），并提供 `basic-memory doctor` 校验文件↔数据库一致性。人可直接用 Obsidian 等编辑器改文件——人与 LLM 同权编辑、以文件为准。
- **模型访问 = 专用 MCP 工具**（stdio MCP server，README 工具清单原文）：内容管理 `write_note` / `read_note` / `read_content` / `view_note` / `edit_note` / `move_note` / `delete_note`；图谱导航 `build_context`（沿 memory:// URI 与 [[wikilink]] 关系展开）/ `recent_activity` / `list_directory`；搜索 `search` / `search_notes`（支持过滤：类型/日期/标签等）；项目管理 `list_memory_projects` / `get_current_project` / `sync_status` 等。模型**不**用通用 Read/Grep 直接翻目录，而是走这些语义化工具。
- **索引（已验证）**：SQLite **FTS5 虚拟表** `search_index`，索引实体（title/permalink/content）、Observation、Relation（[src/basic_memory/models/search.py](https://github.com/basicmachines-co/basic-memory/blob/master/src/basic_memory/models/search.py)）。v0.19.0 起为**全文+向量（FastEmbed）混合搜索**（README「What's New」；是否默认开启未在本次调研范围内验证）。
- **注入方式**：无整库/整文件自动注入；上下文由模型按需调用 `read_note` / `build_context` 构建。
- **索引维护触发**：经 MCP 写入时同步写文件+更新索引；**文件被外部（人）改动时依赖同步进程**——`basic-memory sync`（一次性）或 `sync --watch`（文件 watcher 实时，README 建议常驻运行）。即：**watcher 是可选组件，不常驻时依赖手动/定期 sync**。

## 6. 与 Quoin 已确认设计对照（事实对比，不改变设计）

已确认的 Quoin 设计（2026-09-30）：Quoin 持权威 Wiki 文件——普通完整 git workdir 的 .md 是唯一持久存储（无 SQLite 镜像，go-git，人与模型同权编辑，不做迁移）；Plinth 任务工作区保持临时。每任务流程：Quoin pin 一个已授权的 Git 文件快照交付任务 → 模型用普通 read/write/bash/grep 作业 → 返回差异/变更包 → Quoin 校验 base 后提交；不是持续双向覆盖，也不引入强制 `wiki_*` 专用工具。Quoin 当前代码事实（工具实现、快照缺口）见同日姊妹篇 `quoin-workspace-tools-20260930.md`（代码基线 `105e828`），本节只做模式对比。

- **工具形态**：与 Claude Code / Codex / Karpathy 同族——通用文件工具 + grep，无知识专用工具、无派生索引库。样本中没有任何主流系统要求知识库必须配专用 read/search 工具；唯一带专用工具的样本是 Basic Memory（MCP + FTS5 索引 + 可选同步进程），它同时带来派生索引与常驻同步两类额外基础设施——即 Quoin 已确认设计所避开的复杂度。
- **注入方式**：四个系统都只自动注入小型指令/schema 文件（CLAUDE.md、AGENTS.md、Karpathy 的 schema；Codex 默认上限 32 KiB，Claude Code 建议 200 行/上限 4 MiB），知识正文从不整库注入，一律模型主动检索。Quoin 的任务入口目录说明/scope 属于前一类，Wiki 正文属于后一类——与主流一致。
- **写入路径差异（样本中无先例）**：被调研系统全是「直接改活文件」——Claude Code/Codex 直接改当前工作区、靠 git 回滚与审批兜底；Karpathy 的 LLM 直接写 wiki 仓库；Basic Memory 经 MCP 即时写活文件（v0.19 加 overwrite 防护）。Quoin 的「pin 基线 → 变更包 → 校验后提交」形态上更接近补丁/PR 审查流，是比样本更严格的写入闸门；效果差异（如基线漂移导致的校验失败率）属实现问题，超出本调研证据范围。
- **权威方向相反于 Karpathy**：Karpathy 模式中 raw sources 不可变、wiki 归 LLM 全权；Quoin 中 wiki 文件本身即权威，模型编辑须经校验合并。与 Basic Memory「文件为事实源、写入经工具居中」意图相近，但 Quoin 用通用工具而非语义化 MCP 工具达成。
- **索引层面**：Claude Code/Codex = 无索引；Karpathy = 模型自维护 index.md/log.md（作者自述 ~百级源、数百页内够用，再大才引入 qmd 类外部检索）；Basic Memory = FTS5（v0.19 起加可选向量混合）。Quoin 现阶段同 CC/Codex：无索引、靠 grep + git；「模型可维护的 md 索引页」是样本中实际存在的零基建中间态（事实陈述，非设计要求）。
- **方法论提醒（样本中反复印证）**：框架「提供工具」≠「实际使用/加载」——Claude Code 默认存在 CLAUDE.md 时不读 AGENTS.md；Codex `web_search` 默认仅缓存结果；Basic Memory 实时同步是可选常驻进程。评估 Quoin 工具接缝时同样应以「触发条件与默认值」而非「工具是否存在」为准。

## 参考链接（均为官方/作者原文）

- Claude Code memory：https://code.claude.com/docs/en/memory
- Claude Agent SDK overview：https://platform.claude.com/docs/en/agent-sdk/overview
- Claude memory tool：https://platform.claude.com/docs/en/agents-and-tools/tool-use/memory-tool
- Codex AGENTS.md：https://developers.openai.com/codex/guides/agents-md
- Codex CLI features：https://developers.openai.com/codex/cli/features
- Codex 本地配置（features 表）：https://developers.openai.com/codex/local-config
- codex-rs 工具处理器源码：https://github.com/openai/codex/tree/main/codex-rs/core/src/tools/handlers
- Karpathy「LLM Wiki」gist：https://gist.github.com/karpathy/442a6bf555914893e9891c11519de94f
- Karpathy 后续 X 帖：https://x.com/karpathy/status/2040470801506541998
- Basic Memory README：https://github.com/basicmachines-co/basic-memory/blob/master/README.md
- Basic Memory 搜索索引源码：https://github.com/basicmachines-co/basic-memory/blob/master/src/basic_memory/models/search.py
- Basic Memory 文档站：https://docs.basicmemory.com
