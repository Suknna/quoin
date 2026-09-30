# LLM Wiki 知识模式一手来源研究（2026-09-30）

## 范围与方法

本轮为纯研究：调查把 Quoin 知识能力从「嵌入向量检索」转向「LLM Wiki」作为核心能力所需的一手依据。不做实现、不做设计决策、不修改任何代码；主线会话负责当前代码探索与方案设计，本报告只提供来源事实与相关性映射。

研究问题按主线会话已确认的方向定义：人与模型是**同等编辑者**（双方都有完整直接创建/读取/更新/删除权，模型可自动重组、维护、发布，无需人工批准）；需要 git 式不可变提交/版本历史、用户活动感知、被删除文件的恢复、人工直接增删；现有人工确认式生命周期将在设计中退役。这些是研究问题的输入，不是来源结论——本报告的职责是核对一手来源对每个议题实际说了什么。

**来源原则**：只采信一手来源——规范原文、官方项目仓库、官方文档站、项目自有 README。社交媒体与新闻转述仅作线索（本次出现过一条韩文转述提及 OKF，未采信，改以官方仓库直证）。所有引用都标注获取日期与验证方式。

**获取情况**：全部来源于 2026-09-30 获取。当日 GitHub REST API 的 gists 端点持续返回 502/限流，Karpathy gist 通过两条独立通道验证：gist HTML 页面（作者、标题、时间）与 `gist.githubusercontent.com` raw 全文。SQLite 文档通过官网页面抓取并转文本核对原文。

**范围收缩（按主线会话 2026-09-30 的细化要求）**：证据核心收敛为三块——Karpathy 模式的实际内容及其已记载局限、SQLite 检索、以及主线确认的三个优先项（直接自主 CRUD、提交历史、来源腐化）。第三方衍生实现不逐一追查：OKF 生态仓库只作成熟度的存在性信号（见「来源清单」尾注）。CommonMark/goldmark 由设计文档（`docs/llm-wiki-design.md`）独立引用并经主线核验，本笔记不重复覆盖。

**声称纪律**：本笔记只陈述证据能确立的内容。OKF 规范本体已按第一方全文核验（见模式 B 的「核验边界」），核验不到之点就地标注，不延伸搜索；全文不使用「生产就绪」「事实标准」「行业标准」等地位性表述，项目采用规模一律注明为该项目自述口径。

## 摘要

- Karpathy 的 LLM Wiki（2026-04）是一个**理念文件**而非实现：核心主张是让 LLM 增量构建并持续维护一个持久 wiki（markdown 互链文件集），知识「编译一次、保持最新」，而不是每次查询时从原始文档重新检索。模式本身**不含人工评审门槛**——作者明示人工参与 ingest 只是个人偏好，也可以无人监督批量处理。
- Google **Open Knowledge Format (OKF) v0.2**（`GoogleCloudPlatform/open-knowledge-format`，Apache-2.0）把上述理念里的「wiki 文件约定」提升为正式格式规范：markdown + YAML frontmatter，仅 `type` 必填；`verified`（人工确认）完全是**可选且仅为咨询信号**（规范原文：trust tiers "advisory signals, not access control"），`status` 缺省即为 `stable`——即格式层面**不要求人工批准才能发布**。
- 服务端多用户协作 wiki 的语义参照取自 **MediaWiki** 官方文档：每次编辑一条不可变修订；删除是软删除（正文保留、可管理员恢复、删除事件入日志）；全局 RecentChanges 活动流；而「编辑需评审」的 FlaggedRevs 只是一个**可选扩展**，不是核心——这直接佐证「人工评审不是协作 wiki 的必要条件」。
- 据 SQLite FTS5 官方分词规则推导：默认 unicode61 会把无分隔符的连续中文视为**一整个 token**，不提供中文词切分；trigram 分词器（3.34.0 引入）支持子串匹配但 **FTS 查询少于 3 个 unicode 字符不命中**；`detail=none` 省索引但查询 token 不得长于 3 字符。本研究未取得 Quoin 中文运维语料上的基准，不作性能主张。
- 「LLM 消除检索/消除幻觉」在任何一手来源中都没有主张；gist 只主张**维护成本近零与知识累积**，其「中等规模下 index 文件够用」是作者自述经验值，非基准测试。
- Karpathy 模式的一手材料同时记载了它的明确局限：刻意抽象（非实现）、规模主张止于中等规模经验值、图片处理自评笨拙、Lint 无自动化约定、模式不含并发/服务端语义——这些在设计时不应被当作已解决的问题。

---

## 模式 A：个人文件-Agent 模式 —— Karpathy「LLM Wiki」

**验证信息**：gist [karpathy/442a6bf555914893e9891c11519de94f](https://gist.github.com/karpathy/442a6bf555914893e9891c11519de94f)，页面归属作者 `karpathy`，标题 `llm-wiki`，页面头部时间 2026-04-04T16:25:13Z（获取日期 2026-09-30；GitHub API 当日不可用，经 HTML 页 + raw 全文双通道验证）。

### 核心主张（来源记载）

- gist 原文把现状概括为：多数人经验是 RAG——上传文件、查询时检索片段生成答案，「LLM 每次都在从头重新发现知识，没有累积」。
- 本模式的做法：LLM 不只是索引，而是「**增量构建并维护一个持久 wiki**」——读取新来源、提取关键信息、整合进既有 wiki：更新实体页、修订主题摘要、标注新数据与旧主张的矛盾。原文强调关键差异：「wiki 是持久、复利的产物」，交叉引用已就位、矛盾已标注、综合已反映全部已读内容，「知识编译一次然后保持最新，而不是每次查询重新推导」。
- 人的角色：「你从不（或极少）自己写 wiki——LLM 写并维护全部」。人负责选源、探索、提出好问题。
- 维护成本论证：人类放弃 wiki 是因为维护负担增长快于价值；LLM「不会厌倦、不会忘记更新交叉引用，能一次修改 15 个文件」，维护成本近零。（以上均为作者主张，非测量结果。）

### 三层架构（来源记载）

1. **原始来源（raw sources）**：人工策展的文档集合，**不可变**——LLM 只读不改，是事实之源。
2. **wiki**：LLM 生成的 markdown 文件目录（摘要、实体页、概念页、对比、总览、综合），LLM 完全拥有此层，人只读。
3. **schema**：一份给 LLM 的说明文档（如 CLAUDE.md / AGENTS.md），定义 wiki 结构、约定与 ingest/查询/维护工作流；作者称之为让 LLM 成为「有纪律的 wiki 维护者」的关键配置，人与 LLM 共同演化。

### 操作与索引（来源记载）

- **Ingest**：读源→与用户讨论要点→写摘要页→更新索引→更新相关实体/概念页（一次可能触碰 10–15 页）→追加日志。人工逐源参与是作者的**个人偏好**，原文明确「也可以少监督地批量摄入，取决于你」——模式不含评审关卡。
- **Query**：LLM 搜相关页、综合回答并带引用；**好的回答应回填为 wiki 新页**，让探索也复利。
- **Lint**：定期健康检查——页间矛盾、被新来源取代的旧主张、无入链孤儿页、被提及但缺页的重要概念、缺失交叉引用、可用网络搜索补齐的数据缺口。
- **index.md**（内容目录：每页一条链接+一行摘要，LLM 回答前先读索引再钻取）与 **log.md**（只追加的时间线，建议统一前缀如 `## [2026-04-02] ingest | 标题` 使其可被 `grep`/`tail` 解析）。规模表述「~100 源、数百页时 index 够用，避免 embedding RAG 基础设施」是**作者自述经验，非基准数据**。

### 工具（来源记载）

- 可选 CLI 搜索：作者点名 [qmd](https://github.com/tobi/qmd)（本地 markdown 搜索引擎，BM25/向量混合 + LLM 重排，CLI + MCP 两种形态）；也明示可以临时写个朴素搜索脚本。
- 推荐配套：Obsidian（阅读/图谱视图）、Obsidian Web Clipper（网页转 markdown 入库）、图片下载到本地、Marp 幻灯、Dataview frontmatter 查询。
- 「wiki 就是一个 git 仓库的 markdown 文件」——版本历史、分支、协作「免费」获得。

### 已记载的局限（来源记载；末条为笔者归纳）

- 「这份文档**刻意抽象**，描述的是想法而非具体实现」——目录结构、schema 约定、页面格式、工具全部「可选、模块化」。
- 规模主张是中等规模经验值（「~100 源、数百页」），无基准；原文同时承认「wiki 增长后你需要正经的搜索」——index 优先策略被作者自己限定在增长前期。
- 图片：LLM 不能一次读含内嵌图的 markdown；变通做法（文本先读、图片另看）被作者自评「有点笨拙但够用」。
- Lint 是「定期让 LLM 检查」的人工发起操作，原文未定义自动化调度或修复闭环。
- 维护成本论证（「LLM 不会厌倦、不会忘记更新交叉引用」）是论述而非测量；一次 ingest 触碰 10–15 页也是经验描述。
- （笔者归纳）「你从不（或极少）自己写 wiki」是对作者工作流的**描述而非存储层限制**：wiki 本体是 git 仓库，人工编辑即普通 git 操作。具体操作示例主要是个人 agent + Obsidian；原文也提出 business/team 场景，但**没有规定服务端并发、事务、权限或恢复协议**。它回答「谁来做维护」，不提供可直接移植的多人服务端实现。

## 模式 B：格式规范 —— Google Open Knowledge Format v0.2

**归属与验证**：仓库 [GoogleCloudPlatform/open-knowledge-format](https://github.com/GoogleCloudPlatform/open-knowledge-format)（Google 官方组织），Apache-2.0，创建 2026-08-11，最后 push 2026-08-21，645 stars（gh API 核实于 2026-09-30）。规范原文 `SPEC.md` v0.2 全文抓取核对。

**核验边界（2026-09-30）**：本节规范内容均经第一方 SPEC.md v0.2 全文（raw）核验，属已确证证据。以下各项**未核验、亦不作声称**：第三方 OKF 生态工具的质量与可用性；OKF 与 Karpathy gist 之间是否存在规范层面的引用关系；OKF 在任何生产系统的部署案例；OKF 的标准化地位（不作「生产就绪标准」类表述，见「范围与方法」的声称纪律）。

### 定位（来源记载）

- 「开放、人与 agent 友好的知识表示格式」——**纯格式规范**：一个 markdown 文件目录 + YAML frontmatter；无 schema 注册表、无中央权威、无必需工具。动机原文：知识语料「越来越多地由 agent 持续书写和维护」，因此消费者需要普通 markdown 约定不提供的一等答案：来源（provenance）、可信度（trust）、是否仍成立（freshness）、是否当前版本（lifecycle）、数值是否按认可方式算出（attestation）。

### 结构与字段（来源记载）

- **Bundle** = 目录树；**概念（concept）** = 一个 `.md` 文件（concept ID = 去掉 `.md` 的路径）；保留文件名 `index.md`（渐进披露目录）与 `log.md`（按日期倒序的更新日志，日期必须 ISO 8601 `YYYY-MM-DD`）。分发推荐 git 仓库，理由原文「provides history, attribution, and diffs」。
- Frontmatter 对所有概念一律必填的键是 `type`（特定类型可能另有必填项，例如 computation 的 runtime）；推荐 `title`/`description`/`resource`/`tags`。原文对未知键要求：往返转换 **SHOULD** 保留，且 **MUST NOT** 因未知键拒收，不能把 SHOULD 误写为 MUST。
- **溯源 `sources[]`**：每条 `resource` 必填（URL、bundle 内路径或范围描述）；可选信用信号 `author`、`usage_count`（配 `usage_window`）、`last_modified`。规范明确**不存信用分数**——分数主观、跨消费者不可移植、会过期，可信度由信号**推断**。逐句归因用 markdown 脚注键到 `sources[].id`；规范特意解释为何用稳定键而非位置索引：「agent 不断重写这些文档，位置索引在列表重排时会静默错配，稳定 id 能存活」。
- **信任 `generated` / `verified`**：`generated {by, at}` 记录内容如何产生；`verified [{by, at}]` 列表记录独立确认事件（人工签核、夜间进程可并存）。二者刻意分开——「写的人不必是确认的人」。信任层级由 `verified` 推导：无 `verified` ⇒ unverified；仅非 human actor ⇒ machine-confirmed；有 `human:<id>` ⇒ human-reviewed。规范原文：**「Trust tiers are advisory signals, not access control」**；无信任 frontmatter 的概念照样可消费，消费者**不得拒收**。
- **生命周期**：`status: draft | stable | deprecated`，缺省即 `stable`；`stale_after` 用绝对时刻而非相对 TTL，使过期判定成为纯时间比较。
- **Actor 约定**：agent/工具 `<producer>/<version>`、人 `human:<id>`、自动进程 `process:<id>`；信任分类只看 `human:` 前缀。
- **链接**：普通 markdown 链接（推荐 bundle 根绝对路径）；消费者**必须容忍断链**——「可能只是尚未写出的知识」。
- **Attested Computation**：数值类知识可声明 `runtime`/`parameters`/`executor`/`attester`；**agent 只能填声明过的参数值，不得编写或修改计算本身**；每次运行的 attestation（运行时、不入库）与文档级 `verified`（入库、慢速）是两回事。
- **一致性准则刻意宽容**：不得因缺失可选字段、未知 type、未知键、断链、缺 index.md 而拒收 bundle。
- **版本化**：`<major>.<minor>`；bundle 可在根 `index.md` 声明 `okf_version`；v0.1→v0.2 有两处破坏性变更（`timestamp`→`generated.at`、正文 `# Citations`→`sources` frontmatter）。

### 成熟度评估（基于获取事实的判断）

仓库约 7 周新、spec 处于 v0.2；围绕它的第三方仓库（awesome-okf、okf-skills、pi-llm-wiki 等）在 GitHub 搜索中集中出现于 2026-09 中下旬——格式处于**早期形成期**，尚无标准组织背书与多厂商共识的公开证据。官方仓库同时声明部分议题（receipt/verdict 线协议、attester ABI、沙箱、缓存）**「考虑中/推迟」**。

## 两种模式的分野：个人文件-Agent vs 服务端事务协作 wiki

（本节区分由主线会话的研究问题提出；下表内容逐格标注来源。）

| 维度 | 模式 A：个人文件-Agent（gist/Obsidian/git） | 服务端事务协作 wiki（MediaWiki 参照；Quoin 所处形态） |
| --- | --- | --- |
| 编辑与发布 | agent 直接写盘即生效；无发布概念（来源：gist） | 每次编辑产生一条不可变修订并立即可见，无需他人批准（来源：MediaWiki revision table；FlaggedRevs 评审是可选扩展） |
| 并发 | 无并发语义（单用户单 agent；来源：gist 场景设定） | 修订按序追加保留全部历史，冲突版本并存（来源：revision table 每编辑一条修订；自动消解语义无来源支持） |
| 版本历史 | git 提交历史（来源：gist「git 仓库免费获得版本历史」；OKF 推荐 git 同理） | revision 表逐条保存；删除时修订整体迁入 archive 表，恢复时迁回且 `ar_rev_id` 保留原 ID 使永久链接跨删除循环存活（来源：MediaWiki Manual:archive table） |
| 删除与恢复 | 文件删除 + `git restore` 从历史恢复（来源：git-scm 官方文档 git-restore） | 软删除：正文保留在 text 表「merely hidden」，管理员经 Special:Undelete 恢复；删除事件记入 logging 表；官方文档自认局限：archive 表本身不记删除时间（来源：Manual:archive table） |
| 活动感知 | log.md 前缀 grep、git log 归因（来源：gist、OKF actor/log 约定） | 全局 RecentChanges：倒序列出变更时间、页面大小变化、用户、编辑摘要（来源：Help:Recent changes） |
| 人工评审 | 不是模式组成部分；人工参与 ingest 是作者个人偏好，可无人监督批处理（来源：gist 原文） | 核心 MediaWiki 无评审门槛；评审（editor/reviewer 类）由 FlaggedRevs 扩展提供（Release status: stable）（来源：Extension:FlaggedRevs） |
| 权限/审计 | 无（文件系统之外无机制；来源：gist 未涉及） | 用户权限体系 + 日志表；服务端部署普遍需要（来源：MediaWiki 文档结构；Quoin 已有 audit/命令幂等契约——仓库事实） |

（笔者归纳）LLM Wiki 的**知识维护语义**来自模式 A，而 Quoin 是服务端多用户事务系统：把「LLM 持续维护知识」装进 Quoin，意味着为 wiki 层补齐 B 侧的版本、删除恢复、活动流与权限语义；两份一手材料（gist、OKF）都停在 A 侧，服务端语义需另找参照（MediaWiki 官方文档覆盖修订/删除恢复/活动流/评审扩展四类语义，本次已逐一核验；它与 LLM 无关）。

## 检索基础设施核实：SQLite FTS5 与中文

来源：SQLite 官方文档 [fts5.html](https://www.sqlite.org/fts5.html) 与 [changes.html](https://www.sqlite.org/changes.html)（获取并核对原文于 2026-09-30）。

- **unicode61（默认分词器）**：按 Unicode 6.1 类别切 token——「所有以 L 或 N 开头的类别」是 token 字符，「每段连续的 token 字符是一个 token」。CJK 汉字属 `Lo`（字母类）且汉字间无分隔符，故**连续中文文本会成为一整个 token**（据文档规则推导）：后果是按词匹配对中文无效，只有与整段完全一致的 token 才命中，或改用 trigram。
- **trigram 分词器**：为 FTS5 提供一般化**子串匹配**；查询/短语 token 可匹配行内任意字符序列。官方 changes 页确认其随 **3.34.0（2020-12-01）** 引入。
- **文档明列的硬约束**：
  - 少于 **3 个 unicode 字符**的子串在全文查询中**不命中任何行**——对中文即 1–2 字词查询经 FTS 无结果；
  - LIKE/GLOB 模式若无至少一段 ≥3 字符的非通配序列，FTS5 **退化为整表线性扫描**；
  - `case_sensitive 1` 时**只支持 GLOB 不支持 LIKE**；`remove_diacritics 1` 与 `case_sensitive 1` 互斥；
  - `detail=none`/`detail=column` 可显著省索引，但全文查询 token **不得长于 3 个 unicode 字符**——对中文短语检索是实质约束（LIKE/GLOB 仍可用，略慢）；
  - LIKE 带 `ESCAPE` 子句时索引无法优化该模式。
- **对中文知识库的含义（笔者归纳，标注）**：3 字及以上中文子串检索由 trigram 直接覆盖；1–2 字查询需要 LIKE 退化路径或输入侧补全策略；`detail` 取值应按实际查询长度分布权衡。官方文档**未给任何性能数字**；本文不作性能主张，任何选型前应以真实语料实测。

## Markdown 导入、版本与溯源

- **版本化两套已文档化做法**：git（OKF 推荐分发形态，理由「history, attribution, and diffs」；git 官方文档 [git-restore](https://git-scm.com/docs/git-restore) 正文已核验：`git restore [--source=<tree>] [--staged] [--worktree] <pathspec>`，默认从 index 恢复，`--source` 可从指定提交恢复、按 `<pathspec>` 定位——被删除文件可从仍有该文件的提交按路径找回，对应「被删除文件恢复」诉求）与数据库修订表（MediaWiki revision/archive 模型，见上）。
- **溯源**：OKF 的 `sources[]` + 键控脚注归因 + actor 约定 + `log.md` 是本轮已核验的格式级参考；本研究不声称它是唯一的溯源规范。gist 的 log.md 统一前缀 + unix 工具解析提供轻量的操作记录。两者都不要求集中式溯源服务。
- **导入面**：两个来源都以 markdown 为容器；OKF README 明示与既有工具兼容（「Notion, Obsidian, MkDocs, Hugo, Jekyll — already speak markdown plus YAML frontmatter」）。图片等非文本素材：gist 明确 LLM 不能一次读含内嵌图的 markdown，需文本先行、图片单独查看，并把图片本地化列为 workaround（来源承认的局限）。
- **schema 层**：gist 用 CLAUDE.md/AGENTS.md 一类的说明文档承载约定；[agents.md](https://agents.md/) 官方站自述为「开放格式……6 万+开源项目使用……agent 的 README」（获取 2026-09-30；采用规模为该站自述口径，本研究未独立核验）——模式 A 第三层（schema 文档）的公开约定载体。

## 模型工具面（给模型的检索/维护工具）

- gist 的立场（来源）：小规模下 index.md 优先、免索引；增长后再加 CLI 搜索，并点名 qmd。
- qmd（[tobi/qmd](https://github.com/tobi/qmd) README，获取 2026-09-30）：本地运行（node-llama-cpp + GGUF 模型），BM25 全文 + 向量语义 + LLM 重排，查询扩展按类型路由（lex→BM25，vec/hyde→向量），RRF 融合；输出形态面向 agent（`--json`/`--files`）；同时提供 CLI 与 MCP server（默认 stdio 子进程，或常驻 HTTP）。版本号与维护状态未在本次核验范围。
- （事实边界）以上是两个一手来源提供的工具形态事实：**CLI、MCP、结构化 JSON 输出**是它们面向「模型作为操作者」的共同接口形状。Quoin 侧如何映射到工具目录属于设计问题，留待主线处理。

## 知识腐化、冲突、删除：来源机制盘点

- **腐化/过期**：gist 的 Lint 操作（矛盾、被取代旧主张、孤儿页、缺页、缺交叉引用、数据缺口）；OKF 的 `stale_after`（绝对时刻判定）与 `status: deprecated`（保留供链接与历史）。
- **冲突**：gist 只主张 Lint「发现并标注」矛盾，未定义消解流程；OKF 无冲突消解语义；MediaWiki 以修订序保留全部历史版本（revision table 语义），自动合并/消解无来源支持。
- **删除**：格式层面（OKF）**没有删除语义**——只有 `deprecated` 状态，这是明确空白。已文档化的两套服务/工具级做法：git 文件删除 + restore（模式 A），MediaWiki archive 表软删除 + Undelete + logging 表记录（服务端）。MediaWiki 文档自认 archive 表不记删除时间、需查日志表——是引用其模型时应知的细节。
- **证据范围**：本轮核验的来源没有建立「LLM Wiki 消除检索需要」或「消除幻觉」的结论。gist 的「avoids the need for embedding-based RAG infrastructure」被限定在「中等规模（~100 源/数百页）下 index 够用」的经验语境；其后明确建议随规模增长增加搜索。不能把这种经验描述当作规模/正确性保证。

## 与 Quoin 的相关性（映射；不包含设计）

**现状事实**（引用仓库规格，2026-09-30 读档）：

- 知识域现状：诊断来源（初步分析/调查消息/巡检报告）经「整理为知识」生成 Candidate，人工确认后追加不可变 KnowledgeVersion 并原子切换 current 指针（`HTTP-KNOWLEDGE-002/003`、`UI-KNOWLEDGE-005`：修订同样走 Candidate+确认，不得原地覆盖）。
- 检索现状：派生投影 `knowledge_search_docs`、`knowledge_fts`、`embeddings` 可丢弃可重建、不得成为第二权威源（`DATA-SCOPE-003`）；Embedding 是独立 Attempt 类型、不建 Agent worker（`RUNTIME-AGENT-010`、架构规格 Embedding 行）。

**映射点**（笔者归纳，逐条标注）：

| LLM Wiki 概念 | Quoin 现状对应 | 备注 |
| --- | --- | --- |
| wiki 层（LLM 维护的 markdown 页） | KnowledgeVersion 正文（部分同向） | 现状单知识条目 = 版本化正文；无互链页/实体页结构 |
| schema 层（AGENTS.md 类约定文档） | 无对应物 | gist 视其为模式关键组件 |
| Lint 操作 | 无对应操作 | 腐化检测的来源形态 |
| index.md（目录+一行摘要） | `knowledge_search_docs`（部分重合） | 形态不同：投影表 vs 模型可读目录文件 |
| log.md / git log | 审计与操作关联契约已有 | 服务端活动感知基础已在（仓库事实） |
| git 版本/恢复 | KnowledgeVersion 追加式版本（同向）；删除恢复无对应 | 「删除文件恢复」是新增诉求 |
| 人工确认门槛 | 现为生命周期必经环节 | 按主线确认方向将退役；来源支撑见下 |

**关于「评审不是必要条件」的一手佐证**（按研究问题要求单列）：

1. gist：模式无评审环节；「人工参与 ingest」被作者明确表述为个人偏好，无人监督批处理是并列可选。
2. OKF v0.2：`verified` 可选；trust tier「advisory signals, not access control」；`status` 缺省即 `stable`——未经确认的知识在格式层面完全可发布可消费。
3. MediaWiki：核心编辑流无评审；FlaggedRevs 评审模型是可选扩展（stable 状态的 extension，非内置）。
4. （设计推论，非来源原话）这些来源提供的溯源、过期标记、修订史与恢复机制，可以作为 Quoin 自主编辑方案的参考；不能据此断言它们足以替代事实核验，或来源证明了自主编辑的安全性。

**张力点**（事实陈述）：gist/OKF 的运行假设是文件系统 + git；Quoin 契约是服务端事务（`client_command_id` 幂等、`expected_row_version`、审计同事务）。同权编辑 + 自动发布要进入 Quoin，需要服务端自建修订/恢复/活动流语义；本报告不给出方案，该差距的弥合属主线设计。

## 三个优先项的证据汇总（直接自主 CRUD / 提交历史 / 来源腐化）

按主线会话确认的三个优先项汇总已确证证据与边界；每条只列来源已记载内容，边界单列。

### 1. 直接自主 CRUD（人与模型同权）

- gist：wiki 层「The LLM owns this layer entirely」；「你从不（或极少）自己写 wiki」；人工参与 ingest 是偏好项，「也可以少监督地批量摄入」。存储是 git 仓库——人工编辑即普通 git 操作（笔者归纳：模式不设角色门槛，只有分工偏好）。
- OKF：规范自述知识可「authored by people, generated by agents」；生产者可为任何框架或脚本；`verified` 可选、trust tier 仅为咨询信号、`status` 缺省即 `stable`——格式层面 agent 产出可直接进入可消费状态，无需人工签署。
- MediaWiki：有相应编辑权限者的正常保存会形成修订；评审（FlaggedRevs）是可选扩展。这不表示任何账号都能编辑任意页面。
- 边界：本轮核验的页面未提供可直接用于 Quoin 的**自主模型多写者提交协议**；本研究未穷尽 MediaWiki 的并发机制，不能推断该产品没有编辑冲突处理。Quoin 的协议需结合其既有命令/事务契约设计。

### 2. 提交历史

- gist：「版本历史、分支、协作免费获得」——途径是把 wiki 放进 git 仓库。
- OKF：git 仓库为推荐分发形态（「history, attribution, and diffs」）；格式本身不定义提交/变更集概念，历史语义外包给 git。
- git 官方文档：`git restore [--source=<tree>] [--staged] [--worktree] <pathspec>`——恢复语义有正式文档；被删除文件从仍有该文件的提交按路径恢复。
- MediaWiki：revision/archive 双表——删除时修订整体迁入 archive、恢复时迁回且修订 ID 不变（`ar_rev_id` 使永久链接跨删除循环存活）；正文存储层「未删除、仅隐藏」。
- 边界：无任何来源定义「模型不能伪造自己没做过的提交」这类完整性保证；日志可信性须由系统自身设计承担（Quoin 设计稿已有「日志由程序从事实生成、模型不可自写」的同型要求，属设计立场而非来源结论）。

### 3. 来源腐化（source rot）

- gist：Lint 操作枚举六类腐化（页间矛盾、被新来源取代的旧主张、孤儿页、缺页、缺失交叉引用、可补数据缺口）；log.md 提供时间线。均为「让 LLM 定期做」的人工发起约定。
- OKF：`stale_after`（绝对时刻，纯时间比较判定过期）；`status: deprecated`（保留供链接与历史）；`sources[].last_modified`（来源自身最近变更时间）、`usage_count`/`usage_window`（活跃度信号——规范自述是「粗信号」，只宜读作存活与趋势）、`generated.at` 绑定「内容最近实质变更」。
- MediaWiki：本轮核验的修订/归档/活动流页面没有定义知识时效判断；未研究其他扩展，不能据此否定整个生态的维护能力。
- 边界：本轮来源没有给出已验证的端到端知识腐烂修复闭环。gist 有维护操作建议，但具体调度、并发、来源撤回传播及冲突裁决仍需设计与验收。

## 成熟度与局限

| 来源 | 成熟度 | 主要局限 |
| --- | --- | --- |
| Karpathy gist（2026-04） | 理念文件，单作者 | 自认「刻意抽象」；无实现、无测量；规模经验值不可外推 |
| OKF v0.2（2026-08） | 极新（约 7 周），Google 官方组织但非标准组织 | 生态刚起步；attestation 运行协议、attester ABI 等自述「推迟」；无生产规模公开案例 |
| MediaWiki 文档 | 开源，官方文档结构化（版本化 schema 手册、扩展 release status、兼容政策） | 与 LLM 无关；提供的是协作语义参照，非 LLM-wiki 实现 |
| SQLite FTS5 文档 | 成熟稳定 | 中文场景无专门章节；无性能数字；约束需实测 |
| qmd | 单作者社区项目 | 版本、维护、本地 GGUF 依赖的可用性未核验 |
| agents.md | 自述采用广泛（60k+ 项目，官方站口径） | 仅约定「给 agent 的说明文档」形态，与知识格式无绑定 |

## 未回答的问题

1. OKF 是否会获得多厂商共识或标准组织化；其验证器/连接器生态成熟度（当前仅官方参考实现 + 早期第三方）。
2. 「LLM 自动重组/维护 + 人机同权并发编辑」下适合 Quoin 的**写写冲突与合并策略**：本轮材料不足以直接决定；MediaWiki 更完整的编辑冲突机制未在本次展开。
3. index.md 优先策略在数千页/千级来源规模下是否仍有效：无任何基准。
4. 中文运维语料上 trigram / BM25 / 向量检索的实际召回与排序差异：需以真实语料实验，无来源数据。
5. 删除的合规语义（审计必须保留 vs 内容不可见 vs 可物理清除）如何在服务端组合：来源只提供机制（软删除+日志/恢复），不提供策略。
6. OKF Attested Computation 与 Quoin 巡检采证/证据链的对接可能性：规范侧「运行协议推迟」，需后续设计评估。
7. trigram 之外（如外部内容表、自定义分词）在 Quoin 既有 `knowledge_fts` 基础上的迁移成本：属实现调查，本轮未做。

## 来源清单

| # | 来源 | URL | 获取日期 | 验证方式 |
| --- | --- | --- | --- | --- |
| 1 | Karpathy「LLM Wiki」gist | https://gist.github.com/karpathy/442a6bf555914893e9891c11519de94f （raw: https://gist.githubusercontent.com/karpathy/442a6bf555914893e9891c11519de94f/raw ） | 2026-09-30 | raw 全文 + gist HTML 页（作者/标题/2026-04-04T16:25:13Z）；GitHub API 当日 502/限流 |
| 2 | OKF v0.2 规范 SPEC.md | https://github.com/GoogleCloudPlatform/open-knowledge-format/blob/main/SPEC.md | 2026-09-30 | raw 全文 + gh API 仓库元数据（创建 2026-08-11、push 2026-08-21、Apache-2.0、645 stars） |
| 3 | OKF README | https://github.com/GoogleCloudPlatform/open-knowledge-format#readme | 2026-09-30 | raw 全文 |
| 4 | SQLite FTS5 官方文档 | https://www.sqlite.org/fts5.html | 2026-09-30 | 官网抓取转文本，核对 unicode61/trigram 章节原文 |
| 5 | SQLite 版本历史（trigram 引入版本） | https://www.sqlite.org/changes.html | 2026-09-30 | 官网抓取，3.34.0（2020-12-01）条目含「Enhanced FTS5 to support trigram indexes」 |
| 6 | MediaWiki Manual:revision table | https://www.mediawiki.org/wiki/Manual:Revision_table | 2026-09-30 | 官网抓取（每页每次编辑一条修订元数据） |
| 7 | MediaWiki Manual:archive table | https://www.mediawiki.org/wiki/Manual:Archive_table | 2026-09-30 | 官网抓取（软删除/Undelete/正文保留/ar_rev_id/删除时间在 logging 表） |
| 8 | MediaWiki Help:Recent changes | https://www.mediawiki.org/wiki/Help:Recent_changes | 2026-09-30 | 官网抓取（全局倒序活动流） |
| 9 | MediaWiki Extension:FlaggedRevs | https://www.mediawiki.org/wiki/Extension:FlaggedRevs | 2026-09-30 | 官网抓取（extension、editor/reviewer 类、Release status: stable） |
| 10 | qmd README | https://github.com/tobi/qmd#readme | 2026-09-30 | raw 全文（BM25+向量+RRF+LLM 重排、CLI+MCP） |
| 11 | agents.md 官方站 | https://agents.md/ | 2026-09-30 | 页面 meta 自述（开放格式、60k+ 项目） |
| 12 | git-restore 官方文档 | https://git-scm.com/docs/git-restore | 2026-09-30 | 官网抓取正文（synopsis 与 `--source`/默认 index 恢复语义） |
| 13 | Quoin 仓库规格（相关性映射用） | /home/suknna/code/quoin/docs/specs/quoin-v1/{persistence,http-api,frontend,runtime-protocol,architecture}.md、CONTEXT.md | 2026-09-30 | 本地读档 |

（第三方生态仓库——awesome-okf、okf-skills、pi-llm-wiki 等——仅经 GitHub 搜索确认存在及更新时间，用于成熟度判断，内容未逐一审读，不作依据引用。CommonMark/goldmark 不在本笔记范围：设计文档已独立引用 goldmark 官方 README 并经主线核验。）
