# Wiki 存储选型对比：Quoin SQLite 复用 vs CouchDB / Syncthing / CRDT（go-git 基线）

- 日期：2026-09-30（所有来源均为当日抓取；核验通道见 §9）
- 范围：设计研究；仅本文件；不含代码与提交。不重复 `go-git-wiki-storage-20260930.md` 的深潜内容，按需引用其结论。
- 方法：一手来源，逐条引文标注出处与核验状态；未核验的点明确标注，不作断言。

## 0. 输入决策（主线澄清，2026-09-30）

1. 权威 Markdown **可以**存入文档数据库；Plinth `/wiki` 仍是普通 `.md` 文件（投影）。
2. 当前部署只有 **1 个 Plinth**，不存在多个活跃 Pod 并发写。
3. 主线倾向：**复用现有 Quoin SQLite** 作版本化 Markdown 文档库（无向量、无第二个 md 镜像）；draft/publication/change feed 事务化；Plinth 常驻文件投影。
4. 复用既有**不可变知识版本**与 **command/audit 账本**，不重建 Git 能力。

## 1. 结论先行（在上述约束下）

- **推荐 SQLite 复用**：在本次比较的方案中，它能复用既有运行组件、事务和领域版本机制。但 SQLite 不原生提供 Wiki 的文件投影、变化 feed、编辑权限或语义冲突裁决。人类与一个 Plinth 仍可能同时编辑，不能说没有并发冲突；当前没有必须引入多主复制/实时字符协作的需求，新增产品的收益尚不足以抵消接入成本，未测量“收益≈0”。
- **go-git 方案被替代**：不是 Git 语义错误，而是 go-git 已核验的实现短板（dotgit 无 fsync、ref 发布非原子可见、对象/ref 无事务）恰好是 SQLite 引擎层原生解决的（§5.5）；「不可变轮次历史」由既有知识版本表继承，无需 git 对象库。
- 主线已核对并选择复用现有 `synchronous(FULL)`（`internal/quoin/bootstrap/database.go`）；投影协议在 `docs/llm-wiki-replica-consistency.md` 定义，本文只给引擎证据和边界。
- 重评触发条件见 §7。

## 2. 需求 → 存储语义映射

| 已确认需求 | 需要的存储语义 | SQLite 原生机制 | 证据 |
|---|---|---|---|
| 一次草稿保存/一次跨页发布原子生效 | 各操作各自事务 all-or-nothing | 事务原子提交；不能跨模型推理持有事务 | §3.1 |
| 每完成一轮先持久化再 ACK | 崩溃/掉电后的持久性 | WAL + `synchronous` 设置 | §3.2 |
| 导出一致的文档清单 | 数据库读快照 | WAL 读事务快照；任务期间文件稳定另由本地租约保证 | §3.2 |
| `/wiki` 投影与权威一致 | 有序变更流 | 需在发布事务中新增 Wiki changes feed，并实现 §8 投影协议；不是现有原生 SQLite 能力 | 设计建议 |
| 历史/审计 | 不可变版本 + 审计流 | 既有不可变知识版本 + command/audit 账本（主线确认存在） | 项目现状 |
| 并发写 | 单写者串行 | 权威=Quoin 单进程；引擎层同时仅一个写者 | §3.1/§3.2 |

## 3. SQLite 一手证据

### 3.1 原子提交与崩溃（atomiccommit.html，已核验）

- 「Atomic commit means that either all database changes within a single transaction occur or none of them occur.」——一次草稿保存或一次多文档发布可各自在一个短事务内原子完成；两者之间的模型活动不能包含在数据库长事务中。
- 「SQLite has the important property that transactions appear to be atomic even if the transaction is interrupted by an operating system crash or power failure.」——对应「先持久再 ACK」。
- 「there can only be a single reserved lock on the database file. Hence only a single process can be attempting to write to the database at one time.」（rollback 模式语境；WAL 语境的单写者见 §3.2）。
- 边界（原文自认）：持久性以 OS fsync 正常为前提——「We are told that the flush and fsync primitives are broken on some versions of Windows and Linux… nothing that SQLite can do to test for or remedy the situation.」

### 3.2 WAL（wal.html，已核验）

- 并发：「WAL provides more concurrency as readers do not block writers and a writer does not block readers. Reading and writing can proceed concurrently.」
- 读快照：「for any particular reader, the end mark is unchanged for the duration of the transaction, thus ensuring that a single read transaction only sees the database content as it existed at a single point in time.」
- 单写者：「since there is only one WAL file, there can only be one writer at a time.」
- 持久性分级：「Writers sync the WAL on every transaction commit if PRAGMA synchronous is set to FULL but omit this sync if PRAGMA synchronous is set to NORMAL.」；NORMAL 下「transactions are no longer durable and might rollback following a power failure or hard reset.」
  → 本项目已选择复用 FULL；NORMAL 模式下若唯一正文丢失，账本重放也不能保证找回，因此不能以“可重放”代替可靠持久确认。
- 部署边界：「All processes using a database must be on the same host computer; WAL does not work over a network filesystem.」——Quoin 卷必须是本地文件系统。
- 备份边界：「The WAL file is part of the persistent state of the database and should be kept with the database if the database is copied or moved.」
- 长读事务的代价：「a long-running read transaction can prevent a checkpointer from making progress.」→ 「读任务稳定视图」不应靠长事务实现，按既有租约设计在投影层保证（§8）。

## 4. 对照矩阵

| 维度 | Quoin SQLite 复用 | CouchDB | Syncthing | Automerge / Yjs | go-git（被替代基线） |
|---|---|---|---|---|---|
| 权威形态 | SQLite 内版本化 Markdown（主线已允许 DB 权威） | JSON 文档 + `_rev` | 对等复制的普通文件目录（无权威概念） | CRDT 文档状态（Automerge 为二进制格式，已核验） | Git 对象库（md 展开） |
| `/wiki` 落地 | 常驻投影，change feed 驱动 | 需自建 DB→文件投影管道 | 本身即文件，但无事务/无轮次 | 需自建导出适配器 | 本身即文件 |
| 整轮原子性 | 原生事务 ✅ | `_bulk_docs` 官方明示 non-atomic（§5.2） | 无（逐文件复制） | 无跨文档事务 | commit 原子，但发布原子可见性与持久化短板见既有核验笔记 |
| 历史/审计 | 既有版本表 + 账本 ✅ | `_rev` 非内容历史：compaction 丢弃非叶版本体（§5.2） | versioning 仅归档「远端替换」副本 | 需显式保存快照 | 完整 commit 历史 |
| 并发冲突语义 | 引擎序列化写事务；应用仍须校验旧版本，避免逻辑丢写 | 单节点 409 拒绝；多副本确定性胜者+冲突副本（§5.2） | 文件级冲突副本（默认配置见 §5.3） | 共享数据结构收敛，语义仍需裁决 | 分叉及业务提交需编排 |
| 当前条件下的判断 | 复用价值明确 | 复制能力不是当前必要条件 | 不能代替业务发布 | 缺少必须使用实时协作的场景，仍需文件差分适配 | 若无 Git 工作流硬需求，额外维护两套发布机制收益不明确 |
| 新增依赖/运维 | 无（引擎已在用） | 新服务 + 复制协议 + 投影 | 新守护进程 + 集群成员 + 冲突治理 | 新依赖 + 二进制格式 + 导出层 | go-git 依赖 + 恢复协议 |
| 结论 | ✅ 确认 | ❌ 否决（§7 触发再评） | ❌ 否决 | ❌ 否决 | 被替代 |

## 5. 逐项关键差异

### 5.1 SQLite 复用 —— 确认

- 原子存储需求有原生支持，领域版本与账本可复用；Wiki 投影/变更协议仍需实现，不新增数据库运行组件。
- 相对 go-git 的**真实增益**（非偏好）：引擎层自带 fsync 管理的原子提交（§3.1）、WAL 读快照（§3.2）、单写者串行——对应 go-git dotgit 已核验缺失的三件事（无 fsync、ref 覆盖写非原子可见、对象/ref 无事务，见 `go-git-wiki-storage-20260930.md` §4/§5）。
- 既有不可变知识版本 + command/audit 账本支持历史/审计的重构；还需同事务记录完整 Wiki 变更批次，不能直接把通用审计日志当成完整复制日志。

前提与注意：
- 权威=Quoin 单进程写者，与引擎单写者一致；未来多进程写同一库时写者仍需单点化（WAL 也只允许一个写者）。
- 主线已经核对现有引擎配置并选择 FULL；发布确认必须在事务真正完成后发出（§3.2）。
- WAL 文件随库走、网络文件系统不可用（§3.2 边界）。
- 投影层自愈与原子发布见 §8。

### 5.2 CouchDB —— 否决（当前约束下）

一手引文（apache/couchdb 官方仓库 main 分支文档源文件，当日核验；docs 站对本抓取通道不可达，见 §9）：

- 批量语义（`api/database/bulk-api.rst`）：「Bulk document operations are **non-atomic**. This means that CouchDB does not guarantee that any individual document included in the bulk update (or insert) will be saved when you send the request. … In the event of a crash, some of the documents may have been successfully saved, while others lost.」以及「It is not intended as a way to perform ``ACID``-like transactions in CouchDB, the only transaction boundary within CouchDB is a single update to a single database.」→ 整轮原子仍需应用层补偿；SQLite 直接给。同文件中已无 `all_or_nothing` 参数（1.x 时代选项，现行文档已移除）。
- 冲突模型（`replication/conflicts.rst`）：默认读「CouchDB picks one arbitrary revision as the \\"winner\\", using a deterministic algorithm so that the same choice will be made on all peers」；合并是应用的责任——「Any sensible business-card application will, at minimum, have to present the conflicting versions to Alice and allow her to create a new version incorporating information from them all.」
- 单节点行为与本需求同构：单节点下 CouchDB 靠 `_rev` 前置检查拒绝并发更新——「When working on a single node, CouchDB will avoid creating conflicting revisions by returning a 409 error. … If that ``_rev`` has already been superseded, the update is rejected with a 409 response.」→ 单写者场景它提供的就是乐观锁，与 Quoin 草稿 CAS 同类；其复制/MVCC 机器在单副本下无用。
- 历史语义（`maintenance/compaction.rst` + `conflicts.rst`）：compaction「removing unused and old data from database」；「Old documents revisions are replaced with small amount of metadata called ``tombstone`` … The number of stored revisions (and their ``tombstones``) can be configured by using the ``_revs_limit`` URL endpoint.」；conflicts.rst 更直接：「When you compact a database, the bodies of all the non-leaf documents are discarded. However, the list of historical _revs is retained … There is \\"revision pruning\\" to stop this getting arbitrarily large.」→ `_rev` 保留的是并发控制树，不是内容历史；「版本化 Markdown 库」仍要自建历史表，那不如直接在 SQLite 建。

成本：新增 Erlang 服务、复制协议运维、投影管道、备份形态变化——全部为当前不存在的需求买单。

### 5.3 Syncthing —— 否决

- 官方定位：设备间文件同步（FAQ：「Syncthing is an application that lets you synchronize your files across multiple devices」），且「Is Syncthing my ideal backup application? No…」——无权威概念、无事务、无整轮原子性。
- 冲突语义=文件级冲突副本：config 页 `maxConflicts`——「The maximum number of conflict copies to keep around for any given file. The default is `10`」（已核验）→ 冲突处理被推给人类/调用方，对「整轮编辑原子可见」无帮助。
- versioning 与需求方向相反（已核验）：「Versioning applies to changes received *from other devices*… If Alice changes a file locally on her own computer Syncthing will not and can not archive the old version.」——本地权威侧的改动不归档，而本项目要求的是权威侧每轮持久。
- send-only / receive-only（config 页已核验：「`sendonly`…it will not be modified by Syncthing on this device」「`receiveonly`…it will not propagate changes to other devices」）确实可拼出「单写者+只读订阅者」拓扑，但这正是 Quoin+投影已实现并带事务/审计的形态；引入 Syncthing 只会形成平行复制层与双权威风险。

### 5.4 Automerge / Yjs —— 否决（当前约束下）

一手引文（官方 README / 官方文档站，当日核验）：

- Automerge（`automerge/automerge` README）：「Automerge is a library which provides fast implementations of several different CRDTs, a compact compression format for these CRDTs, and a sync protocol for efficiently transmitting those changes over the network.」；文档形态为二进制（官方提供「the binary format spec」）；`automerge-repo` README：「pluggable networking and storage」，存储适配器为 IndexedDB（浏览器）与 nodefs（文件系统）。
- Yjs（docs.yjs.dev）：「Yjs is a high-performance CRDT for building collaborative applications that sync automatically.」；共享类型「can be manipulated … and automatically merge without merge conflicts」；「Most shared editing solutions depend on a single source of truth - a central server - to perform conflict resolution. Yjs doesn't need a central source of truth.」；持久化形态：「persistence providers that store document updates in a database」。

否决理由：
- CRDT 可在适配器将文本编辑转换为保留因果基线的细粒度操作后发挥作用；整文件 write 并不必然只能实现为删除+全量重插，但这种差分/基线适配不会自动出现。当前单副本写独占，没有足够理由为这层能力增加结构化状态和适配成本。
- CRDT 不解决语义冲突：两轮对同一段落的不同重写，字符合并后仍可能产出错乱文本，需要人或模型裁决；「哪一轮胜出」是业务决策，不是 CRDT 提供的（「without merge conflicts」指 CRDT 层面收敛，不等于语义正确）。
- 权威态变为结构化 CRDT 状态（Automerge 明确为二进制格式），`/wiki` 需导出适配器。主线虽已允许 DB 权威，但换来的是单写者下无用的合并能力，纯增依赖与格式负担；SQLite 内存 Markdown 行更贴近「无第二个 md 镜像」的约束。

### 5.5 go-git 基线 —— 被替代

- 既有核验（`go-git-wiki-storage-20260930.md`）：loose object/ref 写入无 fsync；ref 覆盖写非原子可见；对象与 ref 无多对象事务；PackRefs/gc 禁区多。
- 既有核验（`resident-wiki-sync-20260930.md`）：同步设计依赖 git ff/CAS 语义。存储换成 SQLite 事务+change feed 后，这些 git 特定机制不再需要；但其中 **I1–I3 不变式（单写者、稳定读、同步不覆盖脏投影）在投影层继续适用**，数据源由 `git fetch` 换为 change feed 应用。
- 替代不是否定 Git 思想：轮次不可变历史由知识版本表继承。若未来需要直接 clone / git 生态互操作，再重评（§7）。

## 6. 明确不主张

1. 不主张 CRDT 解决语义冲突，或仅接入库便让任意整文件 write 获得正确合并；也不否认经过正确差分适配后可能获得的收益。
2. 不主张 CouchDB `_rev` 是持久内容历史；非叶版本正文可能因 compaction 消失。文档中的历史 revision 元数据修剪不能不加区分地解释为当前删除标记或业务历史的保留承诺。
3. 不主张换存储引擎自动解决「一份可变共享目录没有快照」的问题——`/wiki` 的读稳定性由投影协议+租约保证，与引擎无关。
4. 不主张 SQLite `synchronous=NORMAL` 下每轮提交掉电持久（§3.2 原文语义相反）；持久级别是显式设计决策。
5. 不主张 SQLite 跨网络/多主机共享（WAL 明确不支持，§3.2）。

## 7. 重评触发条件

- 出现**多个活跃写者 Pod**（真实并发写）→ 重评 CouchDB（文档级收敛：确定性胜者+冲突副本）或 CRDT（内容级字符合并）；届时必须先定义写粒度（整文件 vs 段内小步）。
- 需要与人侧多设备（笔记本/手机）**双向文件同步** → Syncthing 作为外围复制层变得合理（仍不做权威）。
- agent 编辑粒度改为**段内小步编辑**且人机并发成为常态 → CRDT 重新进入视野。
- Quoin 需要多进程并发写同一 SQLite → 写者单点化仍优先，届时再评估并发扩展。

## 8. `/wiki` 投影协议注意点（设计要点，非实现）

- 文件投影的一批多文件变更不因逐文件 rename 或代际标记而自动原子。需在本地读写协调器的排他窗口完成并核验全部变更后才推进 appliedSeq、允许新读者。不能把单文件 rename 原子性外推为任意目录替换/长任务快照保证。
- 自愈：投影是可弃缓存；Plinth 启动即从 Quoin 全量重建并校验，任何投影损坏的恢复路径=重建，不修文件。
- 读稳定：读任务期间投影不被推进——沿用既有 I1–I3 不变式（`resident-wiki-sync-20260930.md` §3.2），数据源换为 change feed；不要用长读 SQLite 事务冒充稳定读（§3.2 checkpoint 代价）。
- change feed：发布事务新增单调变化序号和完整变更批次，操作账本负责命令幂等；两者角色不同。投影应用必须幂等，旧游标可通过一致快照恢复。

## 9. 来源清单与核验状态（均为 2026-09-30 抓取）

已核验原文：
- https://www.sqlite.org/atomiccommit.html （§3.1；web-reader 官网抓取）
- https://www.sqlite.org/wal.html （§3.2；同上）
- https://docs.syncthing.net/users/versioning.html （§5.3；同上）
- https://docs.syncthing.net/users/faq.html （§5.3；同上；注：该页「What if there is a conflict?」小节正文在本次渲染中缺失，冲突治理引文采信 config 页 `maxConflicts`）
- https://docs.syncthing.net/users/config.html （§5.3；同上）
- https://docs.yjs.dev/ （§5.4；同上）
- https://raw.githubusercontent.com/automerge/automerge/main/README.md （§5.4；官方仓库）
- https://raw.githubusercontent.com/automerge/automerge-repo/main/README.md （§5.4；官方仓库）

已核验（官方仓库文档源文件，因 docs.couchdb.apache.org 对本抓取通道不可达——DNS 解析异常——改用同源官方仓库 main 分支 rst 原文，内容与官网文档同源生成）：
- `src/docs/src/replication/conflicts.rst` @ apache/couchdb main （§5.2）
- `src/docs/src/api/database/bulk-api.rst` @ apache/couchdb main （§5.2）
- `src/docs/src/maintenance/compaction.rst` @ apache/couchdb main （§5.2）

项目内既有核验（直接引用，不重复）：
- `docs/research/go-git-wiki-storage-20260930.md`
- `docs/research/resident-wiki-sync-20260930.md`
