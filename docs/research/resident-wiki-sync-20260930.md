# Plinth 常驻 /wiki/ 副本同步策略研究

> 历史选型资料：保留 Git/git-sync/Kubernetes 的调查记录，不作为当前 Git 实施方案。最终协议见 [Issue #111](https://github.com/Suknna/quoin/issues/111) 与 [文档副本一致性设计](../llm-wiki-replica-consistency.md)。

- 日期：2026-09-30（证据为当日抓取的文档版本）
- 范围：仅设计研究，基于一手资料（git-sync README、git 官方文档、Kubernetes 官方文档）；不含代码变更
- 阶段说明：本文原先研究 Git 权威阶段的文件同步。用户随后允许文档数据库并确认当前一个 Plinth；最终方案见 `docs/llm-wiki-design.md` 与 `docs/llm-wiki-replica-consistency.md`，不以本文第 3 节的早期 Git 提交流程为实施协议。
- 可保留的结论：git-sync 不是可写双向复制；emptyDir 适合 Pod 内常驻副本；共享可变目录需要本地读写协调。Git 快进检查本身不能替代业务版本校验、草稿确认或故障恢复。

## 1. 需求约束

- Quoin 持有唯一持久权威 wiki 仓库（go-git 实现）；人类与 agent 均可直接编辑，无需人工审批。
- Plinth 跨 Pod 重建无状态，但在单个 Pod 生命周期内维护且仅维护一份常驻 `/wiki/` 副本，跨任务共享；不做每次尝试的快照拷贝/同步。
- Agent 用标准 read/write/bash/grep 直接操作 `/wiki/` 下的真实 `.md` 文件，对 git 无感知。
- 同步需处理：权威头推进、脏工作树、并发读写、网络中断。

## 2. 一手资料证据

### 2.1 kubernetes/git-sync：单向拉取 + worktree + 原子符号链接

事实（官方 README，master/v4 分支）：

- 定位是单向 sidecar 拉取器："git-sync is a simple command that **pulls** a git repository into a local directory, waits for a while, then repeats... it can pull files down from a repository so that an application can **consume** them."
- 发布机制："It 'publishes' each sync through a worktree and a named symlink. This ensures an **atomic** update - consumers will not see a partially constructed view of the local repository."；"git-sync will fetch the data _without_ checking it out, then create a new worktree, then change the symlink to point to that new worktree. git-sync does not currently have a no-symlink mode."
- 契约：`--root` 目录**不是**同步数据本身，"That directory may or may not respond to git commands - it's an implementation detail"；符号链接指向最近同步版本，其目标 basename 即同步的 git hash。
- 维护行为：`--git-gc`（auto/always/aggressive/off）在每次成功同步后运行垃圾回收。

解读与边界：

- **单次快照一致性**：符号链接原子翻转保证读者"不会看到半构造视图"——一次解析 symlink 后读到的多文件属于同一 revision。
- **时间上不稳定**：读者在两次解析 symlink 之间可跨版本；旧 worktree 会被 GC 回收，长期持有的旧路径可能失效。即"发布原子"≠"长期稳定快照"。
- **只进不出**：全部文档只描述 pull→publish 流程，没有任何 push 回写、写者、脏树保护语义；root 目录甚至"不保证响应 git 命令"。把需要写入、需要保留未提交修改的工作树交给它，修改会在 worktree 重建/翻转/GC 中丢失。**git-sync 不可用作可写双向复制。**
- 可借鉴点：原子发布思想（读一致性）与"数据目录是实现细节、通过显式契约消费"的设计意识。

### 2.2 git pull --ff-only 与脏工作树

- git-pull 官方文档："git pull --ff-only will only do 'fast-forward' updates: **it fails if your local branch has diverged from the remote branch**."（当前 master 文档已将其标为默认策略；实现仍应显式传参，不依赖 git 版本。）
- 脏树行为（git-merge 官方文档 "PRE-MERGE CHECKS" 节）："**git pull and git merge will stop without doing anything when local uncommitted changes overlap with files that git pull/git merge may need to update**"，且"will also abort if there are any changes registered in the index relative to the HEAD commit"；文档同时警告 "Running git merge with non-trivial uncommitted changes is discouraged"。
- 解读：git 原生提供两条所需语义——①本地分叉时拒绝盲目集成（ff-only 即"权威只进不退"）；②可能触碰未提交修改时中止而非覆盖。fetch 阶段只更新远端追踪引用与对象、不触碰工作树，脏树下安全。
- 推论边界：Git 对可能冲突的未提交修改有保护，但不等于一有脏文件就拒绝所有集成。项目要求“不触碰正在编辑的共享目录”仍要由控制器显式检查 dirty/租约状态，不能只依赖 pull 的默认行为。

### 2.3 git push：快进检查与预期引用值是不同条件

- git-push 官方文档："Usually, **git push refuses to update a remote ref that is not an ancestor** of the local ref used to overwrite it."——这是祖先/快进条件，不能直接等同于应用指定 `expectedHead` 的比较并交换语义。
- "--force-with-lease ... overrides this restriction **if the current value of the remote ref is the expected value. git push fails otherwise**."；"It is like taking a 'lease' on the ref **without explicitly locking it**, and the remote ref is updated only if the 'lease' is still valid."；`--force-with-lease=<refname>:<expect>` 可显式指定期望值。
- 设计立场：**权威历史不可改写**。提交路径默认普通 push（非快进失败 = 远端已被推进 = 需 rebase 后重试）；`--force-with-lease=<ref>:<expect>` 仅作为"提交时确认的基线"的显式校验防御手段；裸 `--force` 禁止，不推荐任何改写权威历史的操作。
- 工程修正：已核对 go-git v5.19.2 的 `options.go`，实际有 `PushOptions.ForceWithLease` 与 `RequireRemoteRefs`。先 fetch、客户端检查、再普通 push 不构成无竞态的服务端 CAS；并发裁决必须由服务器在更新时检查。见 https://github.com/go-git/go-git/blob/v5.19.2/options.go 。

### 2.4 Kubernetes emptyDir / PVC 生命周期

- emptyDir（官方文档）："the volume is created when the Pod is assigned to a node... **When a Pod is removed from a node for any reason, the data in the emptyDir is deleted permanently.**"；官方注记："A container crashing does *not* remove a Pod from a node. **The data in an emptyDir volume is safe across container crashes.**"
- PVC（官方文档 "Lifecycle of a volume and claim"）：PVC↔PV 绑定是独占的一对一映射；回收策略 Retain/Delete/Recycle 决定声明释放后卷的去向；卷独立于 Pod 生命周期持久。

对照：

| 事件 | emptyDir（/wiki/ 所在） | PVC |
|---|---|---|
| 容器崩溃重启 | 数据保留 | 数据保留 |
| Pod 重建/重调度 | 数据永久删除，重新 clone | 数据保留（可作暖启动缓存） |
| 节点丢失/不可恢复 | 不可依赖此缓存恢复，需要从权威重建 | 取决于存储类/拓扑 |

- 结论：**emptyDir 精确匹配"Pod 生命周期内常驻、跨 Pod 无状态"**。PVC 非必需（Quoin 才是持久权威），仅当 clone 冷启动成本被证明过高时再评估，默认不引入跨 Pod 状态与额外运维面。注意 emptyDir 受节点临时存储约束，应设置 sizeLimit 并配合 gc 策略。

## 3. 推荐方案：应用内受控同步 + 本地租约 + 乐观基线

**以下为 Git 选型阶段的早期候选，已经被文档版本方案替代。** 其中“锁只覆盖操作批次”“允许断网继续写”“冲突交人类裁决”也不满足后来确认的任务稳定读、Quoin 草稿确认和无需人工审批要求，不可照搬。最终本地租约覆盖需要稳定目录的整个任务阶段，远端数据库事务始终短小；这两种锁的作用域不同。

### 3.1 副本生命周期

- Pod 启动：emptyDir 挂载于 `/wiki/`；同步 actor 首次 clone Quoin 权威仓库（全量历史；wiki 体量小，避免 shallow 语义复杂度）。
- Pod 存续：所有任务共享同一棵工作树；actor 周期性 fetch + 尝试快进集成。
- Pod 重建：副本随 emptyDir 丢弃，重 clone 即自愈。副本任何时刻可弃、可重建，不承载唯一数据。

### 3.2 不变式（进程内强制）

- **I1 单写者**：每仓库至多一个写租约；经典"单写者/多读者"锁，树的结构性变更（集成 checkout、提交）需要排他窗口。
- **I2 稳定读**：读租约活跃期间，树不被集成或编辑触碰；获取读租约在有写租约活跃时等待。多文件读取因此天然原子。
- **I3 脏树保护**：集成前校验 `git status --porcelain` 为空；非空（或写租约活跃）则推迟集成，绝不 reset/clean/checkout 覆盖。
- **I4 无远程锁**：跨进程/跨 Pod 协调只发生在 git ref 上（快进/租约 CAS）；不存在、也不持有任何跨 LLM 推理时长的分布式锁。租约是进程内对象，只影响本 Pod 的读者/写者/同步器。

### 3.3 同步循环（sync actor，单 goroutine 串行执行全部结构性 git 操作）

1. `git fetch origin`（指数退避；失败仅记录，本地读/写不受影响）。
2. 有脏树或活跃写租约 → 跳过集成（保留脏态，这正是"同步不覆盖脏树"）。
3. 等待读租约排空（有界等待）。
4. `git merge --ff-only origin/main`（等价 `pull --ff-only` 的集成半段；失败=本地有未推送提交分叉 → 交由提交流水线 rebase 处理，不强行合并）。
5. 例行维护（gc、容量约束）。

### 3.4 写/提交流水线（乐观基线）

1. 获取写租约（等读者排空）；记录 base = 当前 HEAD（乐观基线）。
2. Agent 用标准工具编辑真实文件（read/write/bash/grep，对 git 无感知）；租约作用域是编辑操作批次，不横跨整个推理轮次。
3. 批次结束/显式 submit：`git add -A && git commit`；若期间 fetch 到 origin/main ≠ base → `git rebase origin/main`（只重写本地未推送提交，安全）。
4. 普通 `git push`；非快进失败（窗口期内远端被推进）→ 重新 fetch、rebase、有限次重试。
5. 冲突无法自动解决 → `rebase --abort`，保留本地提交与工作树内容，向 agent/人类回报冲突文件与双方版本（乐观并发：不阻塞、不丢写、最后写者不自动获胜）。
6. 释放写租约；看门狗超时回收僵尸租约，回收前先把脏态提交到本地分支，避免丢失。

### 3.5 一致性边界与已知限制

- 人类在 Quoin 直接编辑权威：与 Plinth 在途提交在 submit 时才碰撞，通过 rebase/三方合并消解；同文件同区域冲突必须回报裁决，禁止自动取舍。
- 不持租约的 ad-hoc 瞬时读（如裸 bash/grep 扫目录）可能跨集成边界混见两个版本——git checkout 非原子，git-sync README 亦明确指出。缓解：需要多文件一致性的任务由运行时自动包读租约；单文件瞬时读接受该限制。
- Plinth 本地提交在 push 前对 Quoin 与其他读者不可见（最终一致，push 即发布点）；权威头也可能暂时领先副本（同步延迟）。两者均为有限界的暂态。
- Pod 重建后有冷启动 clone 成本；副本容量受 emptyDir 限制。

### 3.6 故障与自愈

- 网络中断：读/写/本地提交照常；fetch/push 退避重试，恢复后自动追赶。
- Pod 重建：丢副本重 clone；无跨 Pod 状态需迁移。
- 副本损坏：clone/fsck 失败 → 备份脏文件到任务输出 → 删除目录重 clone。

## 4. 备选方案为何不采用

- **git-sync sidecar**：单向拉取 + 原子 symlink，为只读消费者设计；无 push、无写者、无脏树语义，worktree 重建 + GC 会丢弃用户修改。不可用作可写常驻副本（未来若有纯只读镜像需求可直接复用它）。
- **每次 attempt 快照 clone**：违背"常驻共享"要求；网络/磁盘开销大，副本间漂移。
- **CRDT / 通用 P2P 同步**：问题域是"单一权威 git 仓库 + 低频文件编辑"；git 的 commit/ref-CAS/merge 已是成熟的并发协调层，CRDT 只增加存储与语义复杂度。
- **跨 LLM 推理的分布式锁**：推理分钟级持锁造成排队与死锁面；本地租约 + git CAS 已覆盖一致性与并发需求。
- **改写 Quoin 权威**（force 推送、Plinth 直挂权威存储卷）：权威唯一且只进不退；Plinth 永不改写权威历史，不共享挂载权威存储。

## 5. 参考来源（一手资料，2026-09-30 抓取）

- kubernetes/git-sync README（master/v4）：https://github.com/kubernetes/git-sync —— 引言、What it produces and why - the contract、Why the symlink、`--git-gc` 旗标
- git-pull：https://git-scm.com/docs/git-pull —— `--ff-only` 集成策略
- git-merge：https://git-scm.com/docs/git-merge —— "PRE-MERGE CHECKS"（脏树中止语义）
- git-push：https://git-scm.com/docs/git-push —— 默认快进规则、`--force-with-lease[=<refname>[:<expect>]]`
- Kubernetes Volumes：https://kubernetes.io/docs/concepts/storage/volumes/#emptydir
- Kubernetes Persistent Volumes：https://kubernetes.io/docs/concepts/storage/persistent-volumes/#lifecycle-of-a-volume-and-claim
