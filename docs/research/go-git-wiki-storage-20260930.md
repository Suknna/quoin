# go-git 作为 Wiki 唯一权威存储的调研（Git 全量仓库方案）

> 历史选型资料：Git 权威候选已被后续 SQLite 文档方案取代。能力核验供参考，实施遵循 [Issue #111](https://github.com/Suknna/quoin/issues/111) 与 [最终设计](../llm-wiki-design.md)，不据此引入 go-git 或 Git 权威存储。

- 日期：2026-09-30
- 结论版本锚点（均已实际 clone 验证）：
  - 稳定版：**v5.19.2** @ `3eeb238da61eb9c7a324f3ee04f990ce89175642`（2026-07-29），`go.mod` 声明 `go 1.25.0` —— 与 Go 1.25 工具链完全匹配
  - v6 线仍为 alpha：v6.0.0-alpha.5 @ `fc18716c`（2026-07-29）；main HEAD @ `00c8f8e3`（2026-09-29）→ **生产应 pin v5.19.2**
  - 安全底线：v5.13.1（2025-01-02）为官方安全修复版本（含 goproxy 依赖安全升级等），任何 pin 不得低于它
- 依据：go-git / go-billy 源码（上述 tag）+ Git 官方文档。本文所有行为结论均标注源码出处。
- **选型更新**：用户随后确认以真实 `.md` 文件落地，采用 `${dataDirectory}/wiki/` 下的普通 Git 工作目录和 `.git/`，不采用本文最初研究的 bare 推荐。bare API 说明保留为库能力参考，最终设计以 `docs/llm-wiki-design.md` 为准。
- **保证范围修正**：单 ref 协作写者间的锁内比较/更新，不等于无锁读者原子可见、与原生 Git 所有写法互斥，或断电持久。后文按这个范围解释 CAS。

## 1. 版本与兼容性

| 项 | 结论 | 依据 |
|---|---|---|
| Go 1.25 | ✅ v5.19.2 的 `go.mod` 即 `go 1.25.0` | `go.mod` |
| 依赖 pin | `github.com/go-git/go-git/v5 v5.19.2` | 上述 tag |
| SHA-256 | 不可用：需 build tag `sha256`，且 fetch/push 均不支持 → 保持 SHA-1 | `COMPATIBILITY.md` SHA256 节 |
| `file://` 传输 | shell 出真实 git 二进制，非纯 Go → 避免使用 | `COMPATIBILITY.md` Transport 节 |
| 许可 | Apache-2.0 | LICENSE |

## 2. bare 仓库初始化

- 推荐：`git.PlainInitWithOptions(path, &PlainInitOptions{Bare: true, InitOptions: InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")}})`；内部等价于 `osfs.New(path)` 直接作为 git 目录 + `filesystem.NewStorage` + `Init(s, nil)`（`repository.go:258-299`）。
- **注意默认分支是 `refs/heads/master`**（`plumbing.Master`，`plumbing/reference.go:260`），要 `main` 必须显式传 `DefaultBranch`。
- `Init` 对非空库返回 `ErrRepositoryAlreadyExists`（以 HEAD 是否存在判断，`repository.go:109-116`）；已存在时改用 `PlainOpen`。
- worktree 为 nil → `Worktree()` 返回 `ErrIsBareRepository`（`repository.go:1528-1531`）。bare 不涉及工作目录/index 同步，但对象/ref 自身仍有并发与持久性问题。用户已选择普通仓库，因此实施还需覆盖工作目录与 HEAD 不一致、人工未提交修改的保护和恢复。

## 3. 写入路径：blob → tree → commit（手工 plumbing）

bare 库没有 `Worktree.Add/Commit`，必须手工组装（全部为已验证的公开 API）：

1. blob：`storer.NewEncodedObject()`（返回 `plumbing.MemoryObject`）→ `SetType(plumbing.BlobObject)` → `Writer()` 写内容 → `repo.Storer.SetEncodedObject(obj)` 得到内容哈希（`storage/filesystem/object.go:126-154`）。
2. tree：构造 `object.Tree{Entries: []object.TreeEntry{...}}` → `tree.Encode(obj)` → `SetEncodedObject`（`plumbing/object/tree.go:387`）。
3. commit：构造 `object.Commit{Author/Committer/Message/TreeHash/ParentHashes}` → `commit.Encode(obj)` → `SetEncodedObject`（`plumbing/object/commit.go:286`）。
4. 移动分支指针：`repo.Storer.CheckAndSetReference(new, old)`（见 §4）。

落盘实现（`storage/filesystem/dotgit/writers.go:291-339`）：写入 `objects/pack/tmp_obj_*` 临时文件（zlib loose 格式）→ `rename` 到 `objects/xx/yyyy...`。内容寻址去重（已存在则跳过）。**无文件锁、无 fsync**。

读取：`repo.CommitObject/TreeObject`（即 `GetCommit/GetTree`）、`object.NewTreeWalker(tree, recursive, seen)` 递归遍历树；`blob.Reader()` 取内容。

## 4. 引用与 CheckAndSetReference 语义

接口契约（`plumbing/storer/reference.go:17-29`）：`CheckAndSetReference(new, old)` 当 `old != nil` 时，先校验 `old.Name()` 当前存储值与 `old` 一致，不一致则报错且不更新；一致则写入 `new`。

文件系统实现（`storage/filesystem/dotgit/dotgit_setref.go:21-52`）：
- 打开 ref 文件（`O_RDWR|O_CREATE`，仅当 `old==nil` 才加 `O_TRUNC`）
- `f.Lock()` —— go-billy v5.9.0 POSIX 实现为 `unix.Flock(fd, LOCK_EX)`（`go-billy/osfs/os_posix.go:13-17`），**是真实的跨进程排他锁**
- 锁内 `checkReferenceAndTruncate`：比对旧值（loose 文件为空时回落读 `packed-refs`），不一致返回 `storage.ErrReferenceHasChanged`（"reference has changed concurrently"，`storage/storer.go:10`）；一致则 seek(0)+truncate(0)+写入新值
- 解锁随 `Close`

### 跨进程原子性的真实边界

| 操作 | 跨进程原子性 | 说明 |
|---|---|---|
| 单 ref CAS（`old!=nil`） | **仅协作写者间条件更新串行化** | POSIX/go-billy 同 ref 文件锁内检查和写入；不是读者原子可见，也不保证与采用 rename/其他锁协议的外部写者互斥 |
| 单 ref 覆盖写（`SetReference`，`old==nil`） | 最后写者胜，但非原子可见 | `O_TRUNC` 发生在拿锁**之前**，锁内写前存在空文件窗口 |
| 读 ref | **无锁，非原子** | 并发写时可能瞬时读到空文件（`ErrEmptyRefFile`，`dotgit.go:716-718`）或半写内容 → 读侧需重试 |
| **创建尚不存在的 ref** | **CAS 无法表达** | 新建空文件读出为空 → 回落 `packed-refs` 找不到 → 返回 `plumbing.ErrReferenceNotFound`，CAS 失败（`dotgit.go:727-751`）。创建 ref 必须在自己的锁内先 `Reference` 探测再 `SetReference` |
| 多 ref 事务 | **无** | Git 的 `git update-ref --stdin`（start/prepare/commit，锁文件事务，见 git-update-ref 文档）在 go-git 没有对应物；`storage/transactional` 只是进程内暂存，不解决跨进程 |
| `PackRefs()` | 非并发安全 | 源码注释明言"仅在文件系统视图操作期间不更新"的假设下工作（`dotgit.go:1111-1121`）→ 权威库不要启用 PackRefs，或只在写锁内做 |

**结论：已核验路径中的 flock 能串行化遵守同一 ref 文件锁的 CAS 写者；不能把它概括成全部 Git 参与者可依赖的原子发布协议。对象与 ref、多 ref 没有事务，读者无锁也可能看到截断窗口。因此仍需应用统一的仓库访问/发布协议。**

## 5. 持久化（durability）与崩溃恢复

- 已核验的 loose object 与 ref 写入路径没有显式文件/目录 fsync。`rename` 的命名原子性不能替代断电持久性；不得从 `Close` 返回成功推导所有对象及引用已经持久落盘。
- 对照 Git：git 自身默认 `core.fsync=committed,-loose-object`，loose object 同样不 fsync（git-config 文档）；但 git 可用 `core.fsync=all` 加固且 ref 更新有锁文件协议，go-git **没有 fsync 配置点**。
- 崩溃残留：`objects/pack/tmp_obj_*`、`tmp_pack_*`、packed-refs 临时文件；fetch/push 写 pack 时 `.idx` 是直接 `Create`（非 temp+rename，`writers.go:133-176`）→ 崩溃可能留下**损坏的 idx**。
- 恢复策略（推荐落到设计里）：
  1. 写流程设计为**幂等可重放**：崩溃后重做"建 blob/tree/commit + CAS ref"即可（内容寻址保证重复写无害）；
  2. 启动恢复先校验、保全异常文件；确认不再被使用的临时对象才可清理。pack 本体损坏不能靠删除 idx 修复，不能未核验就自动删除索引/引用；
  3. 读 ref 遇空/半写不能直接当成空仓库；在协调锁内重新核查，持续异常进入显式恢复；
  4. **不能保证仓库结构不会腐化**：ref 原地 truncate/write、未同步对象均有中断窗口。发布需额外的原子替换/同步与恢复设计，并经目标文件系统故障测试证明；幂等重试不能恢复已经丢失且没有可靠来源的唯一正文。

## 6. 并发模型与线程安全

- **go-git 实例不是 goroutine 安全的**：`ObjectStorage` 无任何互斥，且**读路径也会写缓存**——`requireIndex()` 懒建 `s.index` map、packfile 缓存 `s.packfiles`（`storage/filesystem/object.go:53-76, 237-282`）；`Repository`/`Storer` 结构体均无锁。并发调用同一实例可产生数据竞争。
- `Options.ExclusiveAccess`（`storage/filesystem/storage.go:27-31`）进一步假设打开期间目录无外部修改（用缓存的对象/包列表）。
- bare 的库能力研究不含 worktree/index；最终普通仓库选型需一并串行化工作目录/index 修改。
- **设计结论：每个 wiki 一个单写者**——进程内 `sync.Mutex` 串行化本进程写者；跨进程（多进程共库）再叠加 per-wiki 锁文件（`flock` `<wiki>/writer.lock`）。单 ref CAS 只能解决"同一个 ref"的竞争，解决不了"对象写入+多 ref+缓存实例"的整体一致。

## 7. gc / repack / shallow 陷阱

`COMPATIBILITY.md`（v5.19.2）明确：`gc ❌ repack ❌ prune ❌ fsck ❌ reflog ❌`；`fetch/push/clone --depth ✅`，shallow 能力 ✅。

- 能力矩阵的命令支持标记不能推导“没有对象删除 API”：源码实际提供 `Repository.Prune/DeleteObject`。不能声称历史 blob 永不丢失；保留要求依赖可达性、正确维护、对象完整性与备份。
- 使用外部 git 维护前须核对所产出的 pack/index 格式兼容性。仅凭一条 repack 命令不能承诺安全；维护应停写、校验、保留恢复手段并重建受缓存影响的 Storage 实例。
- **不要使用 shallow**（`Depth` fetch）：权威库需要全量历史，shallow 与"报告历史可比"目标冲突，且会写出 `.git/shallow` 影响遍历语义。
- `Repository.Prune/DeleteObject`（`prune.go`）存在但只处理 loose 对象；**权威库永不调用**，否则可能删掉暂未 ref 到达的新写入对象。

## 8. 删除文件恢复与历史报告的可达性

- **恢复已删除页面 = 从仍然可达且对象完整的历史 commit/tree 中找到 blob，重建包含该路径的 tree，再提交一次**。需要明确的可达性与保留策略，不能只依赖暂时不运行 GC。
- 历史报告（供模型间对比）：每份报告一个 commit，内容为 blob。两种挂法：
  1. 直接追加到主分支链（最简单，天然按时间序）；
  2. 独立命名空间 ref（如 `refs/wiki/reports/<id>`，gitrepository-layout 允许 `refs/` 下任意子目录，prune 保留其中所有可达对象）→ 便于按报告索引，但 ref 文件数量增多。
- 推荐 1+2 结合：报告 commit 同时是主链祖先，`refs/wiki/reports/<id>` 仅作快速索引。
- 关键不变量：**报告 commit 从创建起就必须从某个 ref 可达**；一旦处于不可达状态，外部 gc 可能将其移入 go-git 读不了的 cruft pack。

## 9. 远端 push（可选，不涉秘钥）

- `PushOptions` 支持 `Auth`（ssh/basic）、`Atomic`（多 ref 原子推送，需服务端支持）、`RequireRemoteRefs`（远端 CAS）、`ForceWithLease`（`options.go:267-330`）。
- 本设计权威库是本地 bare 目录，直接 `filesystem` 读写即可；**push 仅作异地备份副本，不做双权威**。
- 凭据从环境/secret 注入 `Auth`，绝不写入仓库或配置文件；不用 `file://`（会 shell 出 git 二进制）。

## 10. 实施约束清单与推荐

**按用户最终选择**：每库一个普通 Git 仓库（不启用 Bare，显式设置 main），Markdown 以文件展开，`.git/` 保存完整版本历史；go-git v5.19.2 是本次核验基线。Git 是唯一 Wiki/报告内容权威；现有 Quoin 运行数据库保留身份、任务状态和内容定位，不复制 Wiki 正文。

必须遵守的约束：
1. **per-wiki 单写者**：进程内 mutex + 跨进程 `flock(writer.lock)`；一切"创建/删除 ref、PackRefs、外部 git 工具"只在写锁内进行。分支推进用 `CheckAndSetReference(old=当前值)` 做乐观锁，`ErrReferenceHasChanged` 即重读重试。
2. **创建 ref 不能走 CAS**（见 §4 表），在写锁内探测+`SetReference`。
3. **读侧隔离**：避免并发使用同一可变缓存实例；使用独立 Storage/Repository 或同一排他 mutex 串行化全部访问。仅持共享读锁不足以保护会修改缓存的并发读。
4. **崩溃恢复**：围绕受管理操作记录、对象/ref/工作区校验设计恢复；不能仅靠重新执行写或删除 idx 就承诺恢复。主 ref 发布的原子可见性与磁盘同步需单独解决。
5. **永不调用 Prune/DeleteObject/PackRefs**（写锁外）；不用 shallow；不用 `file://`。
6. **不声明 SQLite↔Git 跨资源原子性**。即使 SQLite 只留任务元数据，仍需处理 Git 已发布、任务尚未封存的窗口。单写者/CAS 是并发机制，不能替代故障持久性或多资源恢复协议。

## 11. 参考链接（perm 允许 v5.19.2）

- 仓库：https://github.com/go-git/go-git ；go-billy：https://github.com/go-git/go-billy
- ref CAS 实现：https://github.com/go-git/go-git/blob/v5.19.2/storage/filesystem/dotgit/dotgit_setref.go
- CAS 语义接口：https://github.com/go-git/go-git/blob/v5.19.2/plumbing/storer/reference.go ；`ErrReferenceHasChanged`：https://github.com/go-git/go-git/blob/v5.19.2/storage/storer.go
- 对象写入/读取：https://github.com/go-git/go-git/blob/v5.19.2/storage/filesystem/object.go ；临时文件+rename：https://github.com/go-git/go-git/blob/v5.19.2/storage/filesystem/dotgit/writers.go
- Repository/Init/Worktree：https://github.com/go-git/go-git/blob/v5.19.2/repository.go ；Prune：https://github.com/go-git/go-git/blob/v5.19.2/prune.go
- 能力矩阵（gc/repack/shallow/传输）：https://github.com/go-git/go-git/blob/v5.19.2/COMPATIBILITY.md
- flock 证据：https://github.com/go-git/go-billy/blob/v5.9.0/osfs/os_posix.go
- Git 文档：https://git-scm.com/docs/git-update-ref （lockfile 事务语义对照）；https://git-scm.com/docs/git-config （core.fsync 默认 `committed,-loose-object`）；https://git-scm.com/docs/gitrepository-layout （bare 布局与 refs 命名空间）
