# Quoin 当前 Agent 通用工具核对

- 日期：2026-09-30；代码基线 `105e828`。
- 范围：核对当前实现，不代表工具实际线上调用频次；未读取线上运行数据，未修改代码。
- 关联设计：[Git 原生 LLM Wiki](../llm-wiki-design.md)。

## 1. 当前执行位置

`internal/quoin/attempt/tools.go:82-106` 把 bash/read/write/grep 注册为核心 `worker_local` 工具。不是插件，也不是 Quoin 数据库读取工具。

一次调用的路径：模型提出 tool_call → Quoin 执行 BeginToolCall 围栏 → Plinth worker 在沙箱执行 → supervisor 封存工具结果 → 模型读取返回值。

它们是模型按需选用的能力，不是程序每次都会依次执行的固定步骤。工具 schema 来自每个 Attempt 冻结目录；工具存在并不证明模型在某次任务里实际调用，更不等于已经实现长期记忆。

Quoin 持久对象的另一条路径是 `quoin_routed`：例如 `artifact_read/artifact_grep`、`knowledge_search/knowledge_get`、`daily_report_get` 在 Quoin 读取实际业务对象，Plinth 转发结果。两条路径最终都返回工具结果，但可访问的数据不同。

依据：`internal/plinth/worker/typedtools.go:24-61`、`worker.go:687-758`。

## 2. 四个工具做什么

| 工具 | 实际实现 | 当前参数/能力边界 |
|---|---|---|
| read | `os.ReadFile` 读取工作区相对路径 | 只有 path；没有 offset/limit、小节参数；不是知识查询 |
| write | 创建父目录后 `os.WriteFile` | path+content；创建或覆盖整个文件；没有 patch/append 契约；不自动提交 Git |
| grep | `/usr/bin/grep -nE pattern target` | 搜一个指定文件；没有 -r/-R，也没有目录 glob；实际是 GNU grep 扩展正则 |
| bash | `/bin/bash --noprofile --norc -c command` | 当前工作目录固定为 Attempt workspace；清理环境，无平台凭据，沙箱禁止外部网络 |

依据：`internal/plinth/worker/tools.go:265-401`。

`grep` 的模型描述在 `attempt/tools.go:103` 写的是 RE2，但执行器实际调用 GNU grep `-E`。这是当前描述与实现的差异，不能把现有工具当成已经具备某些文件 Agent 的递归仓库搜索接口。

冻结的镜像清单提供 `rg/jq/awk/sed/sort/diff/find` 等，所以可通过 bash 做递归文本搜索、JSON 提取、计算与比较。清单没有 git、Python 或任意包安装能力；不能假设 Agent 可执行与开发机相同的工具链。

依据：`docs/specs/quoin-v1/contracts/plinth-worker-tools.yaml`。

这些能力适合处理已进入本任务的文本、生成草稿、局部计算；它们不会凭工具名称自动知道“知识库”“某份日报”在哪。

## 3. 文件从哪来、能保存多久

- `runner.go:158-173` 建立 `${WorkspaceRoot}/attempt-<id>`，终态路径上 `RemoveAll` 清理。
- `runner.go:235-250` 通过 `StartAttempt` 帧发送 canonical JSON 和 Artifact **定位信息**，没有将其全部下载成工作区文件。
- 已读的 worker 初始化/工具路径中，没有 Wiki checkout、知识目录挂载或原始知识正文自动物化。
- 所以 write 写出的普通文件通常是本任务临时文件；不会因为写了 `wiki/foo.md` 就变成持久 Wiki。
- 发生进程硬崩溃可能有清理未执行的残留，这不构成可依赖的跨任务记忆或恢复协议。

目前知识正文依赖 `knowledge_search/knowledge_get` 远程核心工具；巡检证据用 artifact 工具；日报事实用 `daily_report_get`。本地四个工具可以加工数据，不负责找到这些持久对象。

## 4. 输出处理

`tools.go:404-433` 对正常工具输出超过 50 KiB 或 2000 行时落工作区临时文件，给模型有界尾部预览；`typedtools.go:129-195` 上传为 tool_result Artifact 并封存定位，随后移除该输出临时文件。

这条机制保存的是**超长工具输出**，不是自动备份工作区每个文件。它也不等于 read 支持完整的分段文件阅读；过长 Markdown 应有可继续读取的路径，不能只给最后一段。

## 5. 对 Wiki 接入选择的影响

事实与提案要区分：

1. 当前实现证明已有可复用的通用文件工具和沙箱，**不证明当前已经能阅读持久 Wiki**。
2. 若把某个已授权 Git commit 的知识文件展开到任务工作区，模型确实可以用原有 read/bash+rg 等主动搜索；无需仅因文件属于 Wiki 就必定新增一套 read 工具。
3. 这需要明确的快照交付/权限范围、初始目录说明、长文分页与结果提交。Quoin 和 Plinth 可分进程/机器，不能假定能直接 bind 同一个本地路径。
4. 如果只交付部分文件，必须告诉模型子集范围；要查未加载页面，需要明确的加载/远端查询途径。不能让一个局部 grep 的无匹配表示全库无知识。
5. 通用 bash 可以读和转换多份文件，但现有输入谱系只封存其工具调用和输出，并不自动记录每个被 shell 读取的文件版本。若要求精确页级知识引用，需要补快照 manifest/引用核验；不能声称已有通用工具天然满足。
6. file-first 可以复用现有工具习惯；API-first 可以复用现有 Quoin 路由、资格核查和权威读取。二者的代价不同，不存在“主流 Agent 要求 Wiki 必须有 wiki_find/wiki_read”的统一规则。

当前设计中的专用 Wiki 工具仍是方案选择，不是现有实现，也不是一手资料证明的行业必选。下一步应在同一真实任务上比较：文件快照 + 通用工具，和 Quoin 核心按需读取。重点是数据交付、读取可追溯与提交闭环，而不只是工具名称。

## 6. 若优先复用文件工具，缺的是这些接缝

| 缺口 | 所需行为（设计建议，尚未实现） |
|---|---|
| Wiki 不在 worker 文件系统内 | Quoin 从某个 Git commit 导出已授权范围，随任务交付给 Plinth；只做可销毁的任务缓存 |
| 模型不知道哪里找知识 | 任务入口给出 `knowledge/index.md`、scope、时间、snapshot manifest；目录内容有界，正文模型按需读 |
| read 没有范围参数 | 复用 read 工具身份并升代，增加 offset/limit 或行区间，让长文可读全 |
| grep 是单文件 | 增强目录/glob 支持并保持有界结果；初期也可用现有 bash+rg，不把“有 rg”说成 grep 工具已递归 |
| 普通 write 不持久 | 维护任务在临时目录编辑后，由核心计算相对 base 的差异，检查冲突并用 go-git 发布；保留一个明确的提交动作，而不是把每次 write 当 Commit |
| 本地调用缺页级引用 | 文件 manifest 绑定 Git blob；read/grep 的封存结果带实际读取文件版本。bash 运算结果是派生材料，不能自动冒充已读过所有源页 |
| 只能看到交付子集 | 子集范围必须显式；超出范围的问题需要远端 find/load 或另一次快照交付，不能谎称已搜全库 |

以上不会新增第二套持久知识库，但会新增**快照交付与结果发布机制**。API-first 则不用预先交付全部文件，代价是专用工具调用与服务侧查询实现。应比较完整的 Implementation 成本，而不是只比较模型看到的工具数量。

## 7. Eino 已经提供 edit_file，项目尚未接入

2026-09-30 实读已安装模块 `github.com/cloudwego/eino@v0.9.13`，不是根据工具名称推断：

- `adk/middlewares/filesystem/filesystem.go:40-47` 定义 `read_file/write_file/edit_file/glob/grep/execute` 等工具名。
- 同文件 `ToolConfig.Name` 支持工具命名配置；`Config.Backend`/`EditFileToolConfig` 负责启用文件后端和编辑工具。
- `filesystem.go:813-844` 的 `edit_file` 接收 `file_path/old_string/new_string/replace_all`，调用 `filesystem.Backend.Edit`。
- `adk/filesystem/backend.go:161-177` 明确约定：非 replace_all 模式下 old_string 必须恰好出现一次，否则失败。实际遵守由 Backend 保证；核心模块包含 InMemoryBackend，不能把它当成已经写入 Pod 磁盘的后端。
- 本项目 Go 源码未导入 Eino ADK filesystem 中间件；当前工具是 `attempt/tools.go` 自定义声明、`worker/tools.go` 自定义执行。依赖 Eino 的模型适配与消息类型，不等于自动暴露它全部预制工具。

因此“当前没有 edit”只指项目当前可调用目录，不指框架缺乏编辑能力。没有 edit 时可以 read 完整正文再 write 整篇，或用 bash/sed 局部修改；对大文件，不能拿截断的 read 输出去覆盖全文。

设计建议：优先复用 Eino 的 edit_file 工具/契约，对接受限磁盘 Backend 和既有 worker 调用围栏/草稿确认；可命名为 edit，避免为 Wiki 再造专用编辑工具。中间件接入与后端适配尚未实施，不能宣称只添加一个名字就自动具备文件编辑和同步。

一手源码：
- https://github.com/cloudwego/eino/blob/v0.9.13/adk/middlewares/filesystem/filesystem.go
- https://github.com/cloudwego/eino/blob/v0.9.13/adk/filesystem/backend.go
- https://github.com/cloudwego/eino/blob/v0.9.13/adk/filesystem/backend_inmemory.go
