# Quoin 运维

Quoin 帮助内部运维团队基于监控证据调查告警、执行巡检并沉淀经过确认的运维知识。它面向单一组织，不承担 CMDB 或事故管理系统的职责。

## 运行角色

**Quoin**：
运维系统的控制面，拥有用户配置、任务状态、历史、反馈和知识等权威记录。
_Avoid_: Agent Runtime、浏览器 Runtime

**Plinth**：
一个 Quoin 部署所使用的唯一 Agent Runtime，通过主动建立的运行通道领取调查和分析任务，并在每次 Execution Attempt 的全新可丢弃工作区中调用模型与工具。输入只从 Quoin 当前有效历史和 Artifact 重建；只有显式返回并提交到 Quoin 的消息、Evidence 和 Artifact 可以跨 Attempt 存活。
_Avoid_: Quoin、浏览器 Runtime、跨轮持久工作区、第二份调查历史

**Lintel**：
一个 Quoin 部署所使用的唯一浏览器 Runtime，通过主动建立的运行通道接收任务，拥有独占 `lintel-state` 持久卷中的浏览器身份和长期 service token，并执行人工登录、确定性巡检和可审计探索。它声明可同时承载的浏览器操作总容量；人工登录、Journey 和 Exploration 共享该容量，同一身份仍严格独占，容量不足只在 Quoin 排队。Trace、staging 和 Attempt 工作区可丢弃，需长期保存的结果上传 Quoin。
_Avoid_: Quoin、Agent Runtime、Quoin 数据卷、浏览器 profile 备份源

**Stele**：
独立、无状态的告警协议入口。第一版负责 Alertmanager Webhook 的 HTTP 监听、来源认证、请求体限制以及精确原始请求转交，后续可以增加其他告警接收协议；它不解析告警领域语义，不拥有数据库、持久队列、告警历史或诊断等权威业务状态。每次外部 HTTP 请求生成一个 `relay_id`，同一次 Stele→Quoin 内部转交重试必须复用该 ID，Quoin 对其幂等。Quoin 只有在一个 SQLite 事务中保存 Delivery、处理结果并更新全部正常 Occurrence 后，Stele 才向 Alertmanager 返回 `204`；提交失败或结果不确定时返回非 2xx。Alertmanager 自己发起的重试是新的外部请求和新的 Delivery，不按正文去重。
_Avoid_: Quoin、告警存储、Agent Runtime、消息队列、先返回 2xx 再异步持久化

## 人类角色

**Operator**：
负责日常运维操作：查看全部运维数据，发起、取消和重试分析与巡检，处理调查、反馈和知识，并为已有浏览器身份重新登录和发布 profile；不管理系统连接、秘密、用户、Runtime 或权威配置发布。
_Avoid_: 只读观察者、系统管理员、按业务系统隔离的角色

**Admin**：
Operator 的权限超集，并负责用户、角色、Session、业务系统、标签契约、连接与秘密、模型供应商、巡检配置与调度发布、Runtime、逻辑告警源凭据、备份和安全设置。系统始终必须保留至少一个有效 Admin。
_Avoid_: 超级租户、外部身份提供方、日常任务专属角色

## 认证与服务身份

**本地账号认证**：
用户使用本地用户名和密码登录；User 使用稳定 ID，登录名稳定、显示名可改，只禁用不物理删除。密码按 NFC 规范化后使用 Argon2id PHC 哈希保存，接受 15–128 个 Unicode 字符，不使用字符组合规则或周期改密。创建、修改、临时密码转正式密码和离线重置时，使用随 Quoin Release 固定 SecLists `100k-most-used-passwords-NCSC.txt` 上游 commit 与 checksum、运行期不联网的常见/已泄漏密码 blocklist，并追加产品名、用户名和显示名等上下文值；登录时不再做 blocklist 检查。登录统一返回失败信息；单 Quoin 进程在有界内存中按规范化用户名执行 15 分钟内失败 5 次后冷却 15 分钟，不存在用户走同一路径，进程重启清零可接受；同一进程还限制全局登录速率与 Argon2 并发。不提供自助注册、邮件找回、CAPTCHA 或第一版 MFA。User 保存递增 `auth_revision`，Session 记录签发 revision 而不永久快照角色；禁用、角色变化和 Admin 重置密码在事务中递增 revision 并撤销该用户全部 Session。用户自行改密撤销其他 Session并更新当前 Session revision。所有入口读取当前 User 状态；权限写事务提交前再次核对 enabled、role 和 auth_revision，账号变更与业务写按 SQLite 提交顺序裁决。已受理后台任务不因 Session 失效而取消，并继续保留原操作者引用。

**同源 Web 会话**：
React、HTTP API、SSE 和 noVNC WebSocket 由同一 Quoin Origin 提供。浏览器只持有 32-byte 随机 opaque Session ID 的 Secure、HttpOnly、SameSite=Lax、Path=/ `__Host-quoin-session` Cookie；服务端 SQLite Session 记录承担空闲 12 小时、绝对 7 天、登出、用户禁用和强制撤销。允许同账号多个浏览器 Session，用户可查看和退出自己的其他 Session，Admin 可撤销某用户全部 Session；用户自行改密撤销除当前外的其他 Session。写请求受 Go CrossOriginProtection 保护；携带 Session Cookie 的非安全方法若同时缺少 `Sec-Fetch-Site` 与 `Origin` 则拒绝，存在 `Origin` 时必须精确等于公共 Origin；`POST /auth/login` 另在认证前执行同源门：有 `Origin` 时必须精确相等，没有 `Origin` 时只接受 `Sec-Fetch-Site: same-origin`，两者都缺失以及 `same-site|cross-site` 均拒绝；WebSocket 另校验 Origin，不支持带凭据的跨域 CORS，也不提供 Cookie CLI 兼容入口。Quoin 负责与应用内容相关的 CSP、`frame-ancestors`、`nosniff`、Referrer Policy、敏感响应 `no-store` 与登出 `Clear-Site-Data`，实际 TLS 终止层独占 HSTS。Session 登出、撤销或账号禁用时立即关闭对应 SSE 和 WebSocket，但不自动取消此前已经受理的后台任务。

**管理员离线恢复**：
首个 Admin 创建和全部 Admin 无法登录时的密码重置都只能通过停止长期 Quoin 后独占 SQLite 的本地命令完成：`quoin admin create` 只接受无 `users` 行的空白库，`quoin admin reset-password` 只修改已存在 Admin；部署安装向导只可在启动长期 workload 前以 attached TTY 包装前者。临时密码不进入参数、环境变量、Secret、history 或日志，创建/重置后要求首次登录修改，重置还撤销该账号全部 Session。

**服务身份**：
Plinth、Lintel 和 Stele 分别使用类型固定、只能访问自身 RPC 的长期 service token；TLS 只承担服务端身份和传输保护，不另建 mTLS 客户端身份。Quoin 固定只有 `plinth` 和 `lintel` 两个逻辑 Runtime slot；空库中的两个 slot 起始为 `unregistered`，Admin 可直接为其准备首次注册令牌，无需先“替换”不存在的凭据。一次性注册令牌绑定 slot 与 credential generation，supervisor 将换得的长期 token 原子保存到权限 `0600` 的专用持久状态卷，Plinth worker 不得读取。状态卷丢失时由 Admin 为原 slot 准备替换注册，不创建第二个 Runtime。轮换采用“下发新 token→Runtime 持久化确认→原子提升新 current 并把旧 generation 放入可认证 retiring 角色→新 token 首次成功认证后显示 Pending Retirement→Admin 显式吊销旧 token”的两阶段切换；新值首次认证前旧值仍可恢复连接但同一 slot 只有一个生效 connection epoch，不设置自动 TTL，记录新 token 首次成功使用时间、操作者和未收口状态。Token 吊销时 Quoin 立即关闭对应长期控制流、浏览器流和上传流并拒绝重连；Stele 不注册为 Runtime，其 service token 由部署 Secret 文件提供，普通 SQLite 备份恢复不改变该外部部署身份，只有 Secret 泄漏或安全事件响应才轮换。

**告警源凭据投影**：
Quoin 是逻辑告警源及其 Bearer 状态的唯一权威源，只保存高熵凭据 digest。Stele 通过自身 service token 获取版本化只读 digest 快照并仅在内存缓存；未加载快照时拒绝接收。Stele 提交 Delivery 时携带非秘密 `credential_id` 和快照版本，Quoin 在同一事务中再次检查来源启用状态、凭据有效性和归属；Delivery 与吊销事务按数据库提交顺序裁决，不使用墙钟宽限期。轮换期间一个来源最多同时保留新旧两个有效凭据；新值首次成功使用后进入 Pending Retirement，由 Admin 显式吊销旧值，不设自动 TTL，并持续显示与审计未收口状态。

**一次性秘密 Reveal**：
创建或轮换告警 Bearer、Runtime 注册 token 等一次性秘密时，命令响应只返回绑定发起 Session 的 reveal handle。handle 固定存活 60 秒、仅内存保存、最多成功消费一次；消费时必须是同一仍有效且当前仍为 Admin 的 Session。同一 Session 以同一 `client_command_id` 重放创建命令时，若内存 handle 仍有效且未消费则返回同一个 handle；过期、Session 改变或进程重启后只返回 `revealAvailable=false`，不创建新凭据。reveal 一旦在服务端消费，即使响应丢失也不能再次读取，只能创建替代 generation。登出、Session 撤销、账号禁用或降级以及 Quoin 重启都立即使关联 handle 失效；handle 与原始秘密都不进入数据库、审计、URL、toast、日志、模型上下文或命令持久结果。

**根密钥与可逆秘密**：
部署提供单一 32-byte 根密钥，只通过只读文件或 Kubernetes Secret 挂载，不进入 SQLite、备份或日志。连接凭据使用 AES-256-GCM envelope 保存，envelope 携带格式版本、随机 nonce、root-key binding revision 与 ciphertext/tag，AAD 绑定 Credential Generation 的稳定身份和类型；SQLite 另保存不含秘密的 AEAD verifier。Quoin 启动只加载一次根密钥；文件缺失、长度错误或 verifier 不匹配时保持 Not Ready，只提供无秘密健康诊断。确认密钥永久丢失后，部署操作者必须停止 Quoin 并独占 SQLite 执行离线 rebind：绑定新密钥、递增 binding revision、把全部 Connection 置为不可派发且需重新录入，并直接进入 `RootKeyRebind` 维护状态；旧密文只保留历史、不再尝试解密。运行期单条 envelope 认证失败只隔离对应 Connection 并审计，不回退为空值、明文或旧 revision。v1 不提供多 key keyring、在线重加密或外部 Vault/KMS 集成。

**模型调用边界**：
模型供应商是 supervisor 持有的类型化外部连接。配置时先真实请求 OpenAI-compatible `/v1/models`；返回多个 ID 时由 Admin 明确选择 Chat 与 Embedding model，不自动取第一项，接口缺失、空列表或目标未列出时允许手工填写 model ID。上下文容量等未声明且无法可靠实测的元数据允许手工补充，缺少能力声明不阻止保存为“尚未验证”；流式输出、native/multi Tool Call、取消与 Embedding dimension 等可实测能力仍由最小真实请求验证，成功后才能启用普通 Agent 任务，失败保留配置以及结构化非秘密错误码和允许字段，不复制供应商原始响应。Plinth worker 通过本地 framed protobuf ChatModel 协议提交 messages、固定 tool schema 与可复核的非秘密请求摘要；模型 ID、输出预算和 generation 参数由 Quoin 当前 capability/grant 与固定 Agent 契约决定，worker/模型不得选择或覆盖。supervisor 先经 Quoin 持久化物理 Model Call，再只在内存注入 endpoint credential 并调用内部供应商。Provider API key、Authorization/Cookie、客户端私钥、Kubernetes Secret/kubeconfig、Browser profile/storage state、Quoin 根密钥、密码 hash、Session/token digest 和可逆连接密文不得进入 worker 环境、工作区、模型上下文、Evidence、Artifact 或普通日志。用户主动上传文本、外部日志和页面正文不做通用猜测式秘密扫描。模型 Provider revision 在启用前必须由 Plinth supervisor 真实执行 Chat streaming、native/multi Tool Call、取消、usage/request ID 与 Embedding/dimension 探测；Embedding 和 provider probe 不启动 Agent worker。Provider SDK 隐式重试关闭；同一逻辑调用的自动物理重试只允许无任何响应的 `timeout|rate_limited|transport_error`，以及增加旧回合淘汰后的无响应 `context_overflow`，不对 provider unavailable、取消/终态 fence、invalid response 或 Artifact 提交失败自动重试。

**Plinth worker 隔离边界**：
v1 的 supervisor 与每 Attempt 新 worker 同容器、同 uid；worker 在处理 Attempt 输入前必须 fail-closed 建立 `no_new_privs`、Landlock ABI >= 6 与进程内 seccomp，只能访问既定只读运行时路径、当前一次性工作区和 framed stdio，不能读取 supervisor 的敏感 `/proc` 文件、发域外信号、建立外部网络连接、写工作区外路径或继承非 stdio FD。Plinth readiness 与每个 worker Ack 前都实际执行这些对抗检查，任一失败即 `sandbox_unavailable`，不得静默降级。v1 接受同 PID namespace 下世界可读的非秘密进程元数据可见，不引入 user namespace、bubblewrap、额外 worker daemon 或第二套本地协议。

**领域写命令契约**：
所有经认证外部调用者发起的领域写命令都由客户端生成用户不可见的 `client_command_id`，按 `(principal_id, client_command_id)` 唯一，并保存命令类型、非秘密请求摘要和结果对象引用；相同 ID 与相同请求重放返回原结果，相同 ID 与不同请求返回冲突。修改当前状态或当前版本指针的命令还必须携带 `expected_row_version`；纯追加创建不强制 expected version。调度器用 `plan logical identity + scheduled_for UTC` 作为内部确定性 Run 创建键，并在同一事务绑定当时生效的业务系统配置和 Label Contract。Stele 继续使用 `relay_id`，Runtime 继续使用 `attempt_id + connection_epoch`，不强行改造成 HTTP 命令键。
_Avoid_: 每个 handler 自定义重试语义、最后写入者静默覆盖、把内部 Runtime 围栏混为客户端命令键

**审计与执行溯源**：
领域对象及其不可变版本仍是业务历史权威；另保存窄的 append-only Audit Event，只记录 actor 类型/ID、action、target 类型/ID/版本、client command/request ID、提交时间、成功或确定性拒绝结果及领域记录引用，不复制消息、Evidence、附件、Prompt 正文或秘密。持久审计覆盖登录成功、登出与 Session 生命周期、全部已认证领域写成功及确定性拒绝、用户/角色/密码、秘密 reveal/轮换、Runtime、维护/恢复/离线命令，以及敏感下载的授权和已认证权限拒绝；匿名登录失败、CSRF/畸形匿名请求、无效 Runtime/Stele token 与 429 只进入有界指标和不含密码/完整用户名/credential 的运维日志，不写 SQLite。强制 Audit Event 与领域状态写同事务，审计失败则领域写回滚；敏感下载必须先提交访问审计再发送响应头和首字节；基础设施提交结果未知只记诊断，不伪造权威失败。Audit Event 防御应用用户和 Web Admin，不声称防御拥有 PVC/数据目录 root 权限的部署操作者；v1 不建本地 hash chain 或外部 WORM。每个 Execution Attempt 和低层 Model/Tool Call 保存实际供应商连接 revision/credential generation、模型 ID、Prompt/renderer/agent/tool-schema 版本或 digest、有序输入对象及 revision/digest、Quoin/Plinth/Lintel/Journey Catalog 版本、开始结束时间、usage、延迟、重试序号、规范可见模型响应和结构化终止原因；最终领域输出正文继续由消息、Report、Candidate 和 Evidence 等记录承担。不得保存或展示隐藏思维链。结构化审计长期保留并进入备份。

## 告警与调查

**稳定身份保留**：
任何已经发布、执行过 Config Verification Run 或被历史记录引用的稳定 ID/key 永远不能重新分配给另一个逻辑对象，包括 Business System、Logical Alert Source、Browser Identity、Connection 以及 discovery、plan、check。停用或从已发布 YAML 移除只形成 Disabled/Retired tombstone，不释放身份；以后再次出现同一 key 表示恢复原逻辑对象及其历史，新业务含义必须使用新 key。显示名称可以修改或复用。只有从未发布、从未运行且从未被引用的草稿/staging 对象可以物理清理。
_Avoid_: 退役后复用 key、隐藏 UUID 与用户 key 双重身份、因显示名变化切断历史

**逻辑告警源（Logical Alert Source）**：
一个具有稳定来源身份的告警发送方；同一 HA Alertmanager 集群的副本共享一个来源身份，不同告警源使用不同的认证凭据和 source ID。
_Avoid_: 单个 Alertmanager Pod、Stele 实例、告警送达

**告警送达（Alert Delivery）**：
逻辑告警源向 Stele 发起并由 Quoin 持久化的一次原始通知请求。Quoin 保存精确原始 body、协议、来源、Stele 接收时间、提交时间、完整性和逐项处理结果。整体 JSON 或 `alerts[]` 无法可靠枚举时记录 Rejected Delivery，不更新任何告警发生并返回非 2xx；顶层可解析时先预检全部项目，在一个事务中处理正常项目并隔离 `FingerprintMismatch`、`IdentityConflict` 等异常项目，不能让数组第一项获胜，也不能让异常项目阻塞其他正常状态更新。Alertmanager 的顶层 status 和 groupKey 只作为分组通知元数据；`truncatedAlerts > 0` 时正常处理已包含项目、永久记录不完整事实并在接入状态中提示，commit 后仍返回 `204`，不对未知的缺失项作任何生命周期推断。
_Avoid_: 告警记录、告警事件、按 payload 合并的请求、用分组状态改写单条告警、单项异常拒绝整个可解析 Delivery

**告警发生（Alert Occurrence）**：
一个具有唯一监控身份的告警从触发到恢复的生命周期。Alertmanager v1 以 `source_id + fingerprint + normalized startsAt` 定位一个发生：`source_id` 隔离逻辑告警源，fingerprint 延续 Alertmanager 基于完整 labels 的告警身份，startsAt 区分同一身份的不同触发周期。Quoin 同时保存不可变完整 labels 快照，并在关联每次送达前逐项复核；同一三元组出现不同 labels 时记录 IdentityConflict，绝不静默合并。生命周期状态只有 `Firing | Resolved`，只能由载荷中对应 `alerts[i].status` 推进；不得使用顶层 status、groupKey、endsAt、截断后的缺失、来源停用或长期无通知推断恢复或 Unknown。Firing 只表示最近一次有效观察为 firing 且尚未收到 resolved，不承诺目标此刻仍异常。未来协议必须定义自己的协议原生 occurrence key，不与 Alertmanager 告警自动合并。
_Avoid_: 告警送达、事故、分组通知状态、出站 Connection 身份、只按 fingerprint 跨触发周期合并、用 body hash 或 groupKey 去重、把接入完整性混入生命周期状态

**告警观察（Alert Observation）**：
一个有效 `alerts[i]` 在 Quoin 中形成的不可变观察，保存 Delivery、项目索引、观察状态、来源声明的 startsAt/endsAt、Stele 接收时间、Quoin 提交时间以及它对 Occurrence 的作用。Quoin commit 顺序是系统实际观察顺序；页面状态变化时间使用真正完成转换的 commit 时间，来源时间单独展示。resolved-first、重复通知和 resolved 后迟到 firing 都保留观察，迟到 firing 不重新打开已恢复 Occurrence。
_Avoid_: 可覆盖的当前状态、按来源时间重排历史、隐藏乱序

**告警接入问题（Alert Intake Issue）**：
与普通告警生命周期分离的接入质量事实，包括 IdentityConflict、FingerprintMismatch、DeliveryTruncated 等。冲突项不进入普通告警列表；截断只标记 Delivery 和来源，因为被省略的具体告警不可知。问题可由 Admin 确认已处理，但确认不删除或改写历史，后续再次发生会重新出现。
_Avoid_: 第三种告警状态、自动恢复、事故

**初步分析（Initial Analysis）**：
用户针对一个告警发生触发的一次性模型分析；创建后立即可见，状态为 `Queued | Running | Succeeded | Failed | Cancelled | Interrupted`。数据库强制同一 Alert Occurrence 同时最多一个 active Initial Analysis，双击或命令重试返回同一记录。技术失败重试在同一 Initial Analysis 下创建新 Attempt 并复用创建时输入快照；第一个合法成功结果原子封存为不可变输出。成功后再次“重新分析”创建新的 Initial Analysis，旧结果保留。它可以调用只读工具补充 Evidence，不接受后续对话，但可以连同其引用被 Investigation 使用；Succeeded 只表示模型分析完成，不表示告警正常或诊断已验证。
_Avoid_: 调查、对话、可覆盖结果、已验证诊断

**调查（Investigation）**：
围绕一个运维问题展开的一条对话线程，可以引用一个或多个告警发生、初步分析及相关证据，并形成诊断。Investigation 页面就是主流 Chat 页面：直接新建后立即输入自然语言，不要求先选业务系统、连接、告警、Evidence、模型或工具；从既有告警、Initial Analysis、Evidence 或 Inspection 入口进入时自动保留来源引用。模型只识别人类领域对象，Quoin 才把 Tool 参数中的业务系统确定性路由到全局 Thanos 或该系统绑定的 Kubernetes Connection；真实歧义由模型在对话中追问。
_Avoid_: 初步分析、事故、跨问题聊天、进入 Chat 前的配置向导、让模型选择连接或凭据

**模型上下文投影（Model Context Projection）**：
一次具体 Model Call 从 Quoin 唯一完整历史派生的瞬时有序输入。它固定保留 System/Prompt、当前用户回合、当前 Attempt 已提交的协议组和必要输入，超出 active model budget 时按提交时间淘汰最旧完整 user turn；Tool Call 与全部 Tool Result 共同保留或淘汰。投影不写回历史，不生成摘要、replacement history、rolling memory 或跨 Attempt cache。固定内容仍超限时明确失败为 ContextTooLarge。
_Avoid_: 对话历史、长期摘要、Agent memory、截断当前用户输入、孤立 Tool Result

**长工具输出（Long Tool Output）**：
达到 50 KiB 或 2000 行任一阈值的 Tool Result 正文。完整字节以现有 generated `tool_result` Artifact 持久化并按既有保留策略清理，模型只得到带 size/hash/media type 的有界 head/tail 预览和 Artifact locator，再经 Attempt-scoped `artifact_read`/`artifact_grep` 分段读取；不得新增 per-Attempt 持久目录、暴露 Quoin PV 路径或把预览冒充完整正文。
_Avoid_: 消息正文、`.quoin/tool-results`、第二索引、无限内存缓冲、过期后从 Runtime 缓存恢复

**撤回消息（Withdrawn Message）**：
用户只能 Undo 当前有效对话的最新用户回合。每个 Investigation 只有一个当前有效 head，同时最多一个 active 模型 Attempt；发送消息携带 client command ID 与 expected head，Quoin 在一个事务中追加消息并创建 Attempt，重复命令返回原结果，head 已变化则冲突。Undo 入口显示在最新用户消息下方；提交后，该用户消息及基于它产生的助手回复、工具调用、Evidence 引用和知识草稿成为只读非活动分支，保留审计但不进入后续上下文，依赖该回合的 active Attempt 立即取消，迟到结果只留审计；撤回消息正文与全部附件项一起返回发送框供用户修正后重新发送，新消息从撤回前的有效 head 继续。附件重发只新建消息—附件引用并复用同一份不可变 Artifact 字节，不复制 BLOB；撤回分支仍保留原引用。第一版不支持任意历史撤回、分支切换/合并、原地编辑或重新生成分支。
_Avoid_: 删除消息、只隐藏输入而保留其结论、并发主线、通用分支管理器

**文本附件（Text Attachment）**：
用户附加到一条调查消息中的纯文本 Source Material，保留原始文件名、内容、大小、上传者和时间，并与该消息一同进入调查历史；一条消息可以携带任意份文本附件，不另设个数限制，但全部附件合计受一个默认 10 MiB、可由部署调整的消息级边界约束，单个文件也不得超过该边界。每份正文必须是有效 UTF-8、不得含 NUL；不依赖扩展名判断内容。输入区将待发送附件显示为悬浮文件图标，发送后在用户消息正文下排列文件项，超过三份时默认折叠并明确显示剩余数量；发送条件为非空正文或至少一份附件，不要求用户为附件补写“见附件”。一次粘贴达到 16 KiB 或 200 行任一边界时，客户端确定性转换为可预览、可移除的临时 `.txt` 附件项；逐字输入不在过程中自动转换。原始文件名只作为审计元数据，UI 显示转义后的 basename；Attempt 工作区固定写入 `attachments/<attachment-id>.txt` 并用 manifest 映射，不把用户文件名拼入路径。其精确字节和来源可追溯，但正文中的主张不自动成为已验证 Evidence。
_Avoid_: 任意文件、图片、压缩包、用户控制的工作区路径、确定性工具证据

**诊断（Diagnosis）**：
某个不可变模型输出基于已有证据形成的解释与结论；它可以是 Initial Analysis 的成功输出、某个 Inspection Report 版本，或用户明确选择的 Investigation assistant message/Attempt 输出。系统不维护会被后续回复覆盖的 Investigation 级“当前诊断”；诊断在被人确认前不代表已验证事实。
_Avoid_: 事实、告警、整个调查、可覆盖的当前结论

## 工作台投影

**三栏工作台**：
第一栏是默认只显示图标的全局导航，hover/focus 时解释用途；第二栏是当前模块的对象列表、筛选和主要操作；第三栏是选中对象的详情或工作区。登录后直接进入 `/alerts`，不建设独立仪表盘；Admin 的全局模块为告警、调查、巡检、业务系统、知识和管理，Operator 不显示管理入口，Runtime 离线、登录失效和备份故障通过可见模块的导航徽标及告警页状态区暴露。工作台直接启用所选 shadcn `sidebar-09`/Sidebar 与 Resizable primitives 已有的展开、折叠、隐藏、拖动调整、键盘调整和浏览器本地布局恢复能力，不另造平行布局系统；第一栏保持图标导航语义，第二栏可折叠或调整宽度，第三栏使用剩余空间。URL 未选择对象时不自动选择列表第一项，第三栏使用 shadcn `Empty` 的图标、标题、自然语言描述和至多一个主操作说明当前可做什么；不得留白或伪装成 Dashboard。窄屏时第一栏变抽屉，列表与工作区分别全屏显示；复杂 noVNC 登录明确提示优先使用桌面，但不禁用入口，也不增加复制秘密的替代流程。

**工作台展示约定**：
界面使用紧凑但不拥挤的运维信息密度，列表优先展示状态、对象名、关键时间和业务系统，完整内容进入详情，不提供密度设置。颜色跟随系统明暗偏好，不提供应用内主题设置。v1 界面使用简体中文，代码、labels、annotations、协议状态、日志和上游错误保留原文，不建立无实际消费者的 i18n 机制。业务筛选与实际存在的可切换排序进入 URL query，形成可刷新和可分享的确定性视图；v1 当前列表排序由领域契约固定，不暴露没有服务端契约的统一排序控件。分页游标、滚动位置、临时展开和选中状态属于当前浏览器历史项，返回时恢复并用服务端快照/SSE 调和。cursor 列表首次只读取一页，底部由明确的“加载更多”触发下一页，不伪造页码、不自动无限滚动；加载后仍是同一连续列表。实时新项目到达且用户不在顶部时，保持当前可视内容与焦点不动并显示“有 N 条新内容”，用户触发后合并并回到顶部；已在顶部时可直接合并，但不得抢焦点或自动打开详情。跨模块关联跳转使用浏览器原生历史，返回到来源详情，不维护第二套面包屑栈，也不默认新开标签页。
_Avoid_: 大卡片列表、密度/主题配置页、把滚动像素写进可分享 URL、自定义导航历史、自动无限滚动、实时插入导致阅读位置跳动

**首次设置投影**：
空告警页根据实际状态说明缺少什么并提供“完成初始设置”入口；管理模块提供可跳过、依赖驱动的设置清单，不建设阻塞使用的线性 Wizard。清单从权威状态派生并分别展示模型供应商、Thanos/Kubernetes Connection、Plinth/Lintel、Label Contract、Business System 配置、Browser Identity 登录、Stele 告警源和备份目标的就绪状态、依赖及直接修复入口；告警接入可用与巡检可用分别计算，不要求一次配齐全部能力。Admin 可在管理模块直接处理；Operator 不显示管理入口，只在告警等相关模块看到“需要管理员完成”的结果与影响，不暴露不可进入的配置清单。设置清单始终由权威状态派生且保留：全部就绪时折叠为一行“核心能力已就绪”，有故障或未完成依赖时自动展开受影响项并直达对应连接、Runtime、业务系统、Browser Identity、告警源或备份详情；不保存用户勾选的完成状态。
_Avoid_: 空页面、强制线性向导、把内部对象依赖留给用户推导、所有能力全配齐才允许使用

**管理工作区**：
管理模块只对 Admin 出现在全局导航中；第二栏按设置清单、用户、Label Contract/Journey Catalog、连接、模型供应商、告警源、Runtime、备份、安全和审计分组，第三栏显示所选列表、详情或设置，不增加第四栏或卡片墙管理首页。每个业务系统自己的配置版本、计划、Observed Resource 与 Browser Identity 只在全局“业务系统”模块管理，管理模块不得建立第二入口。权限始终由服务端裁决，前端隐藏不是权限边界。
_Avoid_: Operator 管理入口、独立只读系统状态模块、第四栏、管理 Dashboard 卡片墙

**操作、表单与反馈**：
只有会立即撤销 Session/凭据、停用当前能力、替换 Runtime、切换已发布版本或丢弃未保存输入的高影响操作才确认；确认直接说明影响对象与恢复方式，不要求输入对象名。普通读取、下载、测试、启动、重试不确认并立即反馈。普通设置显式提交、不逐字段自动保存；客户端只做机械即时校验，服务端结果是权威。失败保留全部输入、聚焦首个错误并提供页首摘要；成功在当前对象状态中持续可见，toast 只能补充。业务系统仍走 YAML 上传、静态校验、Config Verification Run 与发布，不建立第二套表单或通用 JSON/YAML 编辑器。后台任务终态在所属列表/详情持续显示；用户位于其他模块时用导航徽标和非阻塞 toast 补充完成/失败并可返回对象，但不建设通知数据库、铃铛收件箱、已读状态或强制浏览器系统通知；六个导航图标只投影 active/failed Attempt、巡检 gap、待确认知识、未确认接入问题、Runtime/备份故障等可操作权威状态，重新打开应用时从对象状态重建，不只用 toast，也不自动跳转。时间主显示为带时区的浏览器本地绝对时间，相对时间只作辅助；hover/focus 或详情提供原始 offset 时间、UTC 与复制，持续时间单独显示。对象按 row version 原位刷新；若已不存在或不再可操作，保留已渲染内容并显示状态条、禁用非法操作和提供返回列表，Session/权限撤销仍立即结束访问。
_Avoid_: 每次写操作确认、仅 toast 表达结果、自动保存半完成配置、错误后清空输入、伪造页码或时间、静默保留可提交的陈旧页面

**跨模块交互状态**：
Evidence、Initial Analysis、Inspection Report、Knowledge、配置版本和 Observed Resource 等已持久化长内容使用确定性嵌套路由铺满工作台；浏览器后退关闭阅读层，刷新和分享恢复同一对象。一次性秘密、未提交上传和未保存表单不进入可分享 URL。Stop/Cancel 提交后按钮立即变为不可重复触发的“正在停止”，保留当前阶段和已完成内容；只有服务端确认 cancellation fence/终态后才显示 `Cancelled`，失败则恢复合法操作并说明原因，用户可离开等待。部分完成不发明统一领域状态，而是并列显示父对象真实状态、每个子步骤终态和机械计数，已完成 Evidence/Artifact 继续可读，失败项原位提供合法恢复动作，程序不按比例生成健康结论。

无权执行的写动作不显示；直接访问受限 URL 时显示工作区级 403，使用普通语言说明所需角色和返回入口，不伪装为对象不存在，也不建设申请权限流程。Session 失效时以不可绕过的重新登录层遮蔽应用，受保护内容不再可见；仅在当前页面内存暂存非秘密正文、附件引用和表单输入，不写浏览器持久存储，同一 principal 重新登录后恢复原 URL 与输入，principal 变化或刷新则丢弃，一次性秘密永不恢复。临时密码登录始终停留在登录页的第二阶段：先验证临时凭据，再在同一认证页面要求设置新密码，成功建立正常 Session 后才加载工作台数据。

对网络中断、429、可恢复 5xx 和结果不确定的命令默认自动恢复：读取与复用同一 `client_command_id` 的命令总计尝试三次，重试间隔 1 秒、2 秒；验证失败、权限不足等确定性错误不重试。三次失败后显示“内部错误”、普通语言原因、“如持续发生请联系管理员”和可复制诊断；停留 10 秒后自动重读当前对象一次，仍失败则回到所属列表上一层。倒计时只存在当前页面内存，用户主动刷新、后退、离开或成功重试后立即取消，绝不在新页面继续旧回退。错误默认先用自然语言说明发生了什么、影响和下一步，并在表单页首或对象状态中持续显示；可展开技术详情只包含稳定错误码、request/Attempt ID、阶段、必要上游原文与复制诊断，禁止堆栈、Authorization、Cookie、秘密或整份无关请求正文。

普通对象列表使用语义化链接/按钮与自然 Tab 顺序，`Tab` 可到达控件、`Enter/Space` 可操作；Sidebar、Dialog、Tabs、Resizable 等沿用 shadcn/Radix/APG 键盘行为，不把整个页面做成 application/grid，也不发明隐藏快捷键。抽屉/确认框打开时焦点不进入背后页面，关闭后回到触发控件；全工作台内容打开时焦点进入标题，关闭/后退后回到原消息或列表行及原滚动位置；未保存输入继续遵循丢弃确认。窄屏详情顶部始终提供明确“返回列表”，不依赖浏览器手势。普通模式的全工作台层使用简短、可中断的从右向左渐入/渐出；`prefers-reduced-motion` 时关闭位移、淡入淡出、列表重排和循环装饰动效，直接显示相同终态，同时保留静态阶段图标、文字和真实进度。
_Avoid_: 瞬时秘密深链、本地伪造 Cancelled、统一 partial-success、按失败比例判健康、403 伪装 404、Session 草稿持久化、临时密码进入工作台、确定性错误重试、换 command ID 重试、跨页面遗留回退定时器、原始堆栈主错误、全局隐藏快捷键、reduced-motion 丢失等待反馈

**告警列表与详情**：
告警模块第二栏默认显示 Firing Occurrence，并按真正状态转换的 Quoin commit 时间倒序；Resolved 进入历史筛选。列表项使用紧凑两行而不是横向表格或大卡片：主行显示状态图标与文字、alertname 和关键时间，次行显示业务系统、可用的 severity 原值和必要状态徽标，选中/hover/focus/变化不只靠颜色表达。列表顶部固定“当前/历史”分段控件与业务系统可搜索 combobox，有筛选时可直接清除；不展示服务端不支持的任意 label 构造器、全文查询或 severity 顺序。第三栏确定性展示状态、来源时间与接收时间、业务系统、完整 labels/annotations、Delivery 时间线、按业务系统与 identity labels 精确匹配的 ObservedResource、机械候选关联及原因、Initial Analysis 历史和引用该 Occurrence 的 Investigation；未匹配资源时明确显示“未匹配到观测资源”。详情标题区只有一个“初步分析”主操作，运行后原位显示真实阶段与取消；正文优先展示最新成功结果的状态、摘要及 Evidence/Attempt 状态，旧成功、失败和取消记录进入按时间排列的版本历史；点击“查看完整分析”后使用与 Evidence 相同的从右向左渐入并铺满整个工作台的阅读层，关闭后恢复原详情位置。severity 只是原样展示和筛选的普通 label，Quoin 不定义顺序，模型不推断。普通列表不显示 IdentityConflict 等接入问题。告警第二栏顶部使用“当前 / 历史 / 接入问题”三个分段；接入问题有独立列表与 URL query，不混入 Occurrence，也不新增全局模块。未确认问题优先显示类型、逻辑告警源、首次/最近发生时间和重复次数；详情先用普通语言解释影响，再展示关联 Delivery、项目索引、冲突 labels/fingerprint、截断数量与不可变历史。Operator 只读，Admin 可原位“标记已处理”且不弹第二个确认框；确认不删除历史，已处理项仍可筛选，后续再次发生形成新的待处理事实。存在未确认接入问题时告警导航图标显示可解释警示徽标。详情 URL 为 `/alerts/:occurrence`；Occurrence resolved 后当前 URL 仍可查看并明确显示已恢复。

**实时投影**：
影响告警列表的事务在同一 SQLite 事务中产生单调递增 `alert_change_seq`；HTTP 快照返回 `snapshot_seq`，每个 Occurrence 返回 `row_version`。客户端首次建立 SSE 时携带 `after=snapshot_seq`，重连使用 `Last-Event-ID`；Quoin 回放其后的有界派生变更。断线、重连、回放和游标过期后的完整快照刷新均在前端静默完成，不向普通用户暴露 SSE、sequence、cursor、resync 等无可操作价值的技术名词，不清空当前阅读位置或抢焦点；多次恢复失败后统一进入普通内部错误恢复流程。SSE 可重复投递，客户端按 sequence 与 row version 幂等应用；游标过期时重新读取完整快照。事件只携带 Occurrence ID、变化类型和版本，选中详情发现版本变化后重新读取。Resolved 从 Firing 列表移除，但已打开 URL 继续显示并标记已恢复。新告警非阻塞提示不打断当前详情；该变更流可丢弃、可重建，不是告警历史权威源。

**调查与巡检工作区**：
调查模块第二栏显示 Investigation 列表，第三栏使用 assistant-ui 对话工作区，既有调查 URL 为 `/investigations/:investigation`。列表标题由程序从当前分支第一条有效用户消息机械生成，空白时回退为关联来源或“新调查 + 创建时间”，不持久化独立标题、不调用模型；列表按当前分支最后消息/Attempt 活动时间倒序。点击新建先进入 `/investigations/new` 空白对话，第一条消息被服务端接受时才原子创建 Investigation、消息和 Attempt；未发送即离开不产生空记录，也不先要求标题、业务系统、告警、模型或工具。从告警进入时在发送框上方显示当前 Occurrence 与用户选中 Initial Analysis 的不可变来源项并直接聚焦输入，第一条消息提交时与来源原子写入。用户位于底部时跟随新 token/message；用户向上阅读后停止自动滚动并显示“查看新回复”，不得抢焦点或改变阅读位置。失败 Attempt 对应的用户消息左侧显示环形重试按钮，点击后按既有消息创建新 Attempt；active Attempt 期间发送按钮变为方形停止按钮，点击提交 cancellation fence，终态后恢复发送按钮。Tool Call 在对话中显示为可折叠状态卡片，默认展示工具名、真实阶段、耗时或终态与人类可读摘要，原始参数、输出和诊断详情原位展开；窄屏不为工具调用增加第二页面或上下分屏。点击 Evidence 引用后，内容从右向左渐入并铺满整个工作台，关闭后恢复原消息与滚动位置；Initial Analysis 完整正文与 Inspection Report 也使用同一全工作台阅读层，详情只保留状态、摘要和版本入口；减少动态效果模式直接切换到同一终态。巡检运行 URL 为 `/inspections/runs/:run`。进行中的初步分析、调查和巡检立即显示已受理与真实执行阶段，用户可离开页面，完成或失败后在列表和详情持续可见。任务创建命令先在 Quoin 事务中保存业务对象和 Attempt，SSE 只是观察通道，断线不取消任务；任务变化使用单调 sequence 与对象 row version，进入页面先读 HTTP 快照再建立 SSE，重连有界回放，游标过期 `resync_required`。事件只传状态、工具阶段和版本，token delta/高频动画不持久化。最终消息、Report 或 Candidate 必须先原子持久化，任务随后才能 Succeeded。Tool Call 执行前创建记录并以真实时间戳单调推进，返回页面从 Attempt 快照恢复完整时间线；不伪造百分比、不展示或声称保存隐藏思维。noVNC 瞬断进入短暂 `AwaitingReconnect`，同一 Session 可重附着，宽限期后关闭 BrowserSession 释放身份锁，且不自动发布 profile generation。

**巡检工作台投影**：
巡检模块第二栏使用紧凑两行 Run 列表：主行显示计划名、真实采证状态和关键时间，次行显示业务系统、人工/调度触发方式、报告与缺口徽标；顶部只提供服务端支持的业务系统和状态筛选，`Completed` 不翻译为“健康”。标题区的“运行巡检”通过轻量选择层选择业务系统及其已发布计划，从业务系统详情进入时预填；同计划已有 active Run 时直接打开，不创建重复项。Run 详情为一个连续页面，按状态与时间、检查结果、Evidence 缺口、分析状态、报告版本排列，并提供简短页内 section navigation，不拆成隐藏上下文的多 tab。每个检查默认显示名称、`ok/gap`、采证时间与 Evidence 数量，展开后显示原始 PromQL/Journey、类型化参数、真实结果、warnings、gap code 和相关 Attempt；程序不生成系统健康结论。页面分开显示“重新分析现有证据”和“重新采集”：前者只创建新 Report 版本，后者创建新 Run 与 `evidence_at`；根据当前失败/缺口推荐其一，但都不弹确认框，也不合并成含糊的“重试”。`AuthenticationRequired` 直达该业务系统 noVNC；发布新 profile 后返回旧 Run，旧 gap 不改写、不自动补跑，用户显式重新采集。
_Avoid_: Run 卡片墙、`Completed=健康`、隐藏检查事实、通用重试、登录后改写或自动补跑旧 Run

**知识工作台投影**：
知识模块第二栏分为“知识 / 待确认 / 导入批次”，第三栏显示所选详情；只有导入批次提供“导入文本”，不提供空白知识表单或知识 Dashboard。人类检索只有一个自然语言输入，结果分列“精确文本匹配”和“语义相似”，分别保留命中依据、分数与索引状态；同一 Knowledge 双重命中时只显示一次并标出两种依据，程序不合成统一相关性总分，也不要求用户先选择 FTS5/向量实现。Knowledge Candidate 从来源诊断、待确认列表进入一个从右向左铺满工作台的编辑层，只编辑标题、正文和适用范围，来源诊断/Evidence 只读；草稿按 revision 保存，冲突时保留本地输入并展示最新版本，不静默覆盖。成功 Initial Analysis 与 Inspection Report 标题区提供次要“整理为知识”，Investigation 只在用户明确选中的 assistant message 菜单提供；点击创建或返回同源 AwaitingConfirmation Candidate 并直接打开编辑层，已有 Candidate 不重复创建，已被标记“不采纳”的诊断不能再创建。导入批次接收一份粘贴原文后立即进入可离开的真实 Processing 状态；成功后在同一批次逐条展开修改或排除 Candidate，并以一次事务确认当前全部，任一 revision 冲突则全不提交并定位冲突项。

每个不可变 Diagnosis 正文底部提供“记录实际结果”：已采纳、已执行、验证有效、不采纳。反馈精确绑定 Initial Analysis 输出、Inspection Report 版本或具体 Investigation assistant message，追加不可变事件并原位显示最新投影与历史；程序不强制四种反馈必须按顺序经过，也不维护 Investigation 级总反馈。每次反馈可选填简短说明；正向反馈直接追加，“不采纳”须确认相关 Candidate 将变为 SourceInvalid、已确认 KnowledgeVersion 将永久退出检索。知识详情展示当前版本、范围、来源诊断、反馈、检索/index 状态和不可变历史；“修订”以当前版本预填待确认草稿，确认后创建下一不可变版本，不原地覆盖；一次修订 Candidate 被排除后保留历史，但同一 current version 仍可重新发起新修订，不能形成永久死路；“停止复用”经影响确认使该版本粘性退出，恢复只能修订并重新确认新版本。
_Avoid_: 混排正式知识与候选、索引实现选择器、程序融合排名、窄弹窗编辑长正文、模型自动写入、逐条跨页面确认

**业务系统工作台投影**：
业务系统模块第二栏使用紧凑两行列表：主行显示系统名称与 `Enabled|Disabled`，次行显示当前配置版本、资源数据新鲜度、Browser Identity 状态和待处理徽标；顶部只有状态筛选和名称搜索。第三栏为连续详情页，依次展示当前状态、配置版本、巡检计划、Observed Resource 与 Browser Identity，并用简短页内 section navigation；关联 Run、资源和不可变版本使用确定性子路由或全工作台阅读层，返回后恢复原位置。Admin 从列表标题或配置 section 进入全工作台上传层，可下载起始模板、拖放或选择一份 YAML，并查看目标 Label Contract 与 Journey Catalog provenance；失败保留文件并逐项显示 YAML path、原因和修复方法，不提供 YAML 编辑器或平行表单。版本历史逐项显示真实状态，不发明“当前草稿”；版本详情机械展示相对当前发布版本的 YAML diff、静态校验、Config Verification Run 历史和契约兼容性。“运行测试”创建独立 Config Verification Run；“发布”经一次影响确认原子切换，冲突后重新读取权威指针且不覆盖其他版本。业务系统启停只由新 YAML 版本的根 `enabled` 字段发布完成，不增加开关；Disabled 系统和历史对 Operator 仍可见，但不能创建新的普通巡检。

Observed Resource 列表明确区分“当前观测到 / 当前未观测到 / 数据陈旧”，显示 discovery、身份 labels、最后成功刷新与最后见到时间；点击后铺满工作台查看完整 labels、discovery、观测时间与当前/陈旧状态，不把未观测到解释为删除；v1 没有资源历史引用数据模型，不制造该列表。Browser Identity section 显示当前状态、revision、profile generation、最近 probe 与占用情况；Admin 配置层只编辑显示名、起始 URL、authentication probe 与类型化参数并创建新 revision，Operator 只能对既有身份执行重新登录。`/business-systems/:system/browser-login` 中 noVNC 铺满工作台，顶部固定窄工具条显示业务系统、真实 operation 状态、重连提示、发布与取消；发布成功关闭远程桌面并回到来源详情，关闭页面不隐式取消，窄屏保留入口并提示桌面体验更可靠。

**Label Contract 激活投影**：
管理页使用全工作台 readiness 视图，展示目标契约、每个已启用业务系统的全部合法“配置版本 + Passed Config Verification Run”候选和阻塞原因；多个合法候选必须由 Admin 明确选择，不以“最新”代替。全部系统选择完整后经一次影响确认原子激活；阻塞项直接跳转对应业务系统版本或 Config Verification Run，禁止先切契约再逐系统修复。
_Avoid_: 双配置入口、Business System 卡片墙、latest draft、上传即发布、可编辑 CMDB、Cookie/profile 文件编辑、部分激活

**账号、Session 与审计投影**：
全局导航底部头像菜单只包含当前身份/角色、修改密码、我的 Session、审计记录和退出；低频账号操作不占主导航。我的 Session 使用全工作台层列出设备/浏览器、创建时间、最后活动并标记当前 Session，其他 Session 可逐个撤销，确认明确说明对应 SSE/WebSocket 会立即断开。Admin 用户管理列表显示用户名、显示名、角色、启用状态和最后登录；详情原位修改显示名/角色/状态，并提供重置密码和撤销全部 Session。禁用、降级、重置密码与撤销 Session 均说明现有登录影响；最后一个有效 Admin 的服务端冲突原样解释，不通过隐藏按钮冒充不可能。所有登录用户可从头像菜单进入 `/audit` 全工作台审计列表，按 actor type、action 与时间筛选并查看结构化事件；它不是第七个常驻模块，返回恢复原业务页面。

**连接、凭据与 Runtime 管理投影**：
管理页按 Thanos、Kubernetes、模型供应商等真实 Connection kind 使用类型化表单，只收集该类型真实的非秘密字段和凭据，不提供任意 URL+JSON 编辑器。详情分开显示当前 ConnectionRevision、CredentialGeneration、启用/重验状态、最近真实测试与不可变历史；Operator 只在相关业务页面看到非秘密连接状态与影响。模型供应商创建/轮换后显示“尚未验证”：先列出 `/v1/models` 返回 ID 供 Admin 选择，列表缺失时提供手工 model ID 与未声明元数据输入；真实 capability probe 可后台运行，成功后显示实测能力并允许启用，失败保留 revision/generation、手工输入和结构化非秘密错误码/允许字段，不复制供应商原始响应，已启用供应商轮换时先停用且不在 probe 前自动恢复。Alert Source 详情显示凭据的非秘密 ID、Active/Pending Retirement/Retired、创建/首次使用/退休时间；轮换后新旧两个 generation 可认证，新值首次成功使用后旧值进入 Pending Retirement，并提示“更新 Alertmanager → 确认新凭据已使用 → 显式吊销旧凭据”，程序不自动猜测切换完成或自动吊销。创建/轮换返回 reveal handle 时，前端立即调用一次 reveal 并打开铺满工作台的一次性秘密层：原文可见且可复制，明确关闭后不能再次查看；秘密只在当前页面内存存在，不进入 URL、toast、日志、下载或浏览器持久存储，关闭后只能通过新轮换取得。

Plinth/Lintel 两个固定 Runtime slot 各自分离展示持久注册状态与当前在线状态，并显示 generation、最后见到时间和 active 工作阻塞。首次注册直接为 unregistered slot 准备令牌；替换先确认吊销与能力中断影响，服务端 fence 通过后使用同一一次性秘密层显示注册令牌，随后持续显示“等待新 Runtime 注册/连接”，不建设可新增 slot 的 Runtime 管理器。备份页为连续页面：顶部显示目标挂载状态、计划时间、IANA 时区、保留份数与最近成功，并显式保存设置；下方展示不可变备份记录、真实阶段/错误、大小、checksum 和下载，“立即备份”受理后可离开。失败在告警页/管理徽标持续提示并可重试；Web UI 不提供在线恢复，只提供停机恢复说明与所选备份 manifest 信息。
_Avoid_: 个人设置全局模块、通用连接 JSON、秘密持久化、自动吊销旧凭据、健康/异常单灯、动态 Runtime slot、在线覆盖恢复

## 资源与巡检

**标签契约（Label Contract）**：
部署级、版本化的 Prometheus label 语义契约。第一版只统一配置业务系统归属 label 名；告警归属、资源发现、巡检 PromQL 校验、资源关联和页面筛选共同引用同一个契约版本。资源身份 labels 仍由每个资源发现声明显式配置。管理页展示当前/草稿版本并提供验证和激活入口；零启用业务系统时首个契约通过静态校验即可直接激活。有启用系统时，页面逐系统展示兼容状态、目标配置版本、Config Verification Run 结果和阻塞原因，只有全部系统都准备并验证兼容版本后，才与这些版本在一个事务中原子激活；任一失败则全部继续使用旧版本并显示原因。已开始的 Run 继续绑定旧版本，契约变更不重写历史 label 快照或归属。
_Avoid_: 代码写死 label、每业务系统重复定义归属 label、CMDB Schema、多个当前契约、部分切换、空部署循环依赖

**业务系统（Business System）**：
由用户在一份权威 YAML 中明确配置的稳定运维范围，根节点包含稳定 key、显示名和 enabled 状态，生命周期只有 `Enabled | Disabled`。首次上传静态合法且不存在的 key 时，一个事务创建 Disabled Business System 与第一份不可变草稿；创建草稿不表示启用或发布，Config Verification Run 可在 Disabled 状态执行，只有显式发布才切换当前配置及 enabled 状态。后续停用/启用也通过发布 YAML 版本完成，管理页不提供与 YAML 竞争的创建或元数据编辑表单。有历史引用后不物理删除。停用后停止新的资源刷新、定时巡检和普通人工 Inspection Run，等待任务 `BusinessSystemDisabled`，已接受只读 Run 可完成并允许取消；告警继续接收并显示系统已停用，历史保留。重新启用前重验发布版本，启用后从未来周期继续，不补跑。
_Avoid_: 独立创建表单、YAML 与数据库双配置权威、租户、Kubernetes 集群、自动推断服务、停用删除历史

**观测资源（Observed Resource）**：
由 Prometheus labels 识别并描述的运行对象；这些 labels 是 Quoin 对该资源身份和归属的权威事实。它不是人工维护的 CMDB 资产记录。稳定身份为 `BusinessSystem ID + ResourceDiscovery key + 按 label 名排序的 identity label/value map`；显示名称、数组顺序和非身份 labels 不参与身份。业务系统 YAML 显式声明 discovery 稳定 key、instant vector selector、身份 labels 和统一刷新周期；Quoin 在明确的 `observed_at` 调用 Prometheus instant query。只有无 warnings、无部分响应的完整成功刷新，才能把本轮未出现的资源标记为当前未观测到；不完整刷新保留上次成功状态并暴露 `last_successful_refresh_at` 和数据陈旧。身份 label 缺失或同一身份 tuple 出现冲突 labels 时记录发现错误，不以空值或最后一条覆盖。
_Avoid_: 资产、CMDB 条目、Kubernetes 对象快照、用 `/series` 元数据证明当前资源状态、完整 labels fingerprint 身份

**Kubernetes 运行时状态（Kubernetes Runtime State）**：
人工调查时由模型通过只读类型化工具按需获取的当前对象、状态、事件和受控日志，不作为长期资产权威源，也不作为第一版定时巡检 YAML 的检查类型。
_Avoid_: CMDB 资产、观测资源身份、定时 Kubernetes 巡检项

**连接（Connection）**：
Quoin 访问一个外部运行系统时使用的稳定命名身份与访问边界，由系统中的多个用户和任务共享。第一版使用明确类型的 Prometheus/Thanos、Kubernetes 和模型供应商连接；Runtime、Stele 告警源、Browser Identity 和 Business System 不是 Connection。地址、TLS、CA、用户名等非秘密配置形成不可变 `ConnectionRevision`；密码、API key、kubeconfig 等秘密独立形成加密 `CredentialGeneration`。修改或轮换创建新 revision/generation 并原子切换当前指针，不原地覆盖。Attempt 派发前在事务中检查连接启用并绑定实际 revision/generation，Attempt、Evidence 和审计只记 ID。普通切换不影响已经被 Runtime 接受的 Attempt，它使用内存旧快照完成；停用连接时阻止新派发，等待任务以 `ConnectionDisabled` 结束，已接受的只读 Attempt 可完成并允许用户取消。历史保留非秘密 generation 元数据，旧秘密不再下发。
_Avoid_: 无类型 URL+凭据、可覆盖配置、长期可下发旧秘密、用户凭据、巡检计划、Runtime 身份

**指标查询连接（Metrics Query Connection）**：
一个 Quoin 部署共享的唯一 Thanos Query 入口；它实现 Prometheus HTTP v1 API，供全部业务系统执行 PromQL 与 labels 查询。ObservedResource 当前刷新使用指定时刻的 instant query，不使用 `/series` 证明当前存在。第一版必须支持 HTTP Basic Auth，并可配置 TLS 与自定义 CA。PromQL 校验使用 AST 而非正则：每个 VectorSelector 都必须以当前 Label Contract 的业务系统 label 精确 `=` 当前 system key；YAML 不能指定 Thanos URL。Resource Discovery 必须是单个 instant vector selector，不允许 `offset`、`@`、聚合或 `label_replace` 把历史/合成结果伪装为当前资源。业务系统不分别拥有 Prometheus 连接。
_Avoid_: 每业务系统 Prometheus、Grafana 数据源、Thanos StoreAPI、字符串改写 PromQL

**巡检计划（Inspection Plan）**：
用户为一个业务系统明确配置的一组类型化巡检项，具有跨版本稳定的用户可读 key 和可修改显示名。每个计划可配置一个标准五字段 cron；缺少 cron 表示仅人工运行，时区由该业务系统配置根节点统一提供。一个计划同时最多一个 active Run。
_Avoid_: 巡检报告、调查、跨业务系统的自动推断模板、任意 Agent 提示词、每检查独立调度

**巡检项（Inspection Check）**：
巡检计划中的一个独立检查，具有跨版本稳定 key、可修改显示名和明确 `analysis_question`，第一版选择 PromQL 或浏览器巡检；程序校验后机械执行，模型统一分析全部证据。每个 check 在一次 Run 中恰好执行一次，表达式和 Journey 参数均为字面量，不支持模板、环境变量、循环或按 ObservedResource 动态展开；多个目标必须显式写多条 check。Kubernetes 只供人工调查按需查询，不进入定时配置。PromQL 支持 instant 与 range query；range 以真实开始采证的 `evidence_at` 为终点并保存实际 start/end/step。YAML 不提供 `expect` 或断言规则；Journey 只保留完成浏览器动作所必需的内部检查。
_Avoid_: 诊断、巡检报告、由程序猜测的检查、Kubernetes 定时检查、健康阈值规则引擎、动态 fan-out、通用模板或 DSL

**业务系统配置版本（Business System Configuration Version）**：
每个业务系统一份完整 YAML，根节点显式包含稳定业务系统 key/name/enabled，并以一个版本原子包含资源发现和全部巡检计划；每个系统只有一个当前已发布版本。每次上传创建不可变草稿并记录基线发布版本，Config Verification Run 精确绑定草稿。发布命令携带 version ID 与 expected current published version ID，事务中重验并切换；不匹配则冲突。Label Contract 联合激活仍原子切换契约和全部兼容版本。根节点统一提供 IANA 时区和资源刷新 interval，discovery、plan、check 使用稳定 key。YAML 只接受一个 UTF-8 文档，拒绝重复 key、anchor/alias、merge、自定义 tag、非字符串字段名、第二文档和尾随内容，并设输入/AST/深度上限；上传时只解析一次，保存原文、parser/schema 版本、类型结构和 digest，运行只使用类型结构。管理页可按当前 Schema、Label Contract 与 Journey Catalog 下载起始模板，任意草稿/发布版本可导出；校验错误逐项展示字段路径、原因和可操作修复提示。未知字段、重复 key 或不兼容契约必须拒绝。
_Avoid_: 页面隐式归属、共享可覆盖草稿、资源与计划分别发布、运行时重新解析、自动热加载、表单与 YAML 双权威、要求用户先读内部 Schema

**浏览器身份（Browser Identity）**：
每个业务系统恰好一个共享持久化巡检身份，只保存稳定身份和当前配置 revision/profile generation/状态指针。不可变 Browser Identity Revision 包含 Admin 管理的起始 URL、对 Journey Catalog 中版本化 authentication probe 的引用及类型化参数和创建者；每次修改创建新 revision，每个 Browser Operation 准入时冻结实际 revision。Profile generation 另行记录精确 Chromium revision。Admin 在 Business System 详情创建/配置身份；Admin 与 Operator 都可从该详情进入 `/business-systems/:system/browser-login` 实时 noVNC 页面，完成登录后必须显式发布 generation。Manual-login operation 绑定发起用户且同时只允许一个 active noVNC WebSocket；连接仍活跃时拒绝第二连接，断线后只有同一用户可在宽限期内重附着，其他用户不能旁观、接管或代发。发布必须绑定当前操作者仍活动的人工登录并原子创建 generation、成功结束该操作，同时关闭 noVNC 与 Chromium；无活动登录不能发布。Inspection 的 `AuthenticationRequired`、徽标和失败详情直达该页面，登录后可返回原 Run；不建设 Lintel 技术控制台。Journey 从起始 URL/origin 出发，YAML 只给类型化参数或相对路径，明确 SSO 跨 origin 由版本化 Journey 实现；模型 Exploration 的导航与动作边界由“浏览器探索会话”定义。同一身份的登录、巡检、探索严格独占，但不建立每身份等待队列：身份已占用时人工请求立即返回 `IdentityBusy` 冲突，Inspection/Config Verification Run 的 check 立即形成同名 gap 且不补跑；只有 Lintel 全局 slot 不足才进入 Quoin 的 `WaitingForCapacity` FIFO。Lintel 在 `lintel-state` 卷维护可写 profile，正常 Cookie/refresh token/IndexedDB/localStorage 更新持续保留；人工发布形成新 generation，自动变化不制造版本。Journey 与 Exploration 在准入后、执行任何业务动作前运行本 operation 冻结 revision 的 authentication probe；人工登录只在显式发布 generation 前运行同一 probe。切换当前 Browser Identity Revision 后立即以新配置对现有 profile 重新探测，成功则保持 `Ready` 且不创建 generation，明确未登录才转 `AuthenticationRequired`，后续每次操作记录实际 revision。Probe 结果区分 `Authenticated | Unauthenticated | Indeterminate`：只有 `Unauthenticated` 改变身份状态；Runtime 不可用、超时、浏览器崩溃或探测实现失效等 `Indeterminate` 只使本次操作形成 `AuthenticationProbeUnavailable` 缺口，不把技术故障伪装为凭据失效。Journey 在第一步前与最终成功提交前探测，Exploration 在 Session 建立与成功关闭前探测；中途明确匹配未登录条件时立即终止。Quoin 只存 generation/状态/时间/运行引用，备份不含 profile。每次 Lintel 新 boot 建立控制流后，Quoin 调和全部 current profile inventory；Lintel 只检查原子 manifest、目录存在性与精确 Chromium revision，不启动 Chromium，缺失、manifest 损坏或 revision 不符立即置 `AuthenticationRequired`，真实登录有效性仍由操作准入 probe 判断。人工登录和浏览器操作不跨 Quoin 或 Lintel 进程重启恢复：Quoin 启动调和遗留操作时只执行关闭清理，不恢复业务动作；同一 Lintel boot 仍存活者先尽力提交适用的强制 trace/probe，再收口 `Interrupted` 并确认 Chromium/隧道停止；旧 boot 不可达则明确记录 trace 不可得；新 Lintel boot 可证明旧进程已不存在并确认释放。只有同一次运行中的瞬时 noVNC 断线允许在有限宽限期内重附着。进入可写人工登录时立即把身份置为 `AuthenticationRequired`；关闭、取消、宽限到期或重启而未发布时保持该状态，只有 probe 成功并原子发布新 generation 才恢复 `Ready`。每次人工登录、Journey 或完整 Exploration Session 都启动独立 Chromium 进程，独占打开 persistent profile，结束后关闭；profile 数据持久，但 tab、弹窗、CDP/trace 和进程内状态不跨操作复用。
_Avoid_: 用户密码、任意 YAML 绝对 URL、Playwright 脚本、Runtime 管理页登录、每次运行回滚状态、把 profile 当 Artifact、跨 Runtime 重启恢复登录会话、原地尝试打开 revision 不匹配的 profile

**浏览器探索会话（Browser Exploration Session）**：
一次 Investigation 中由 Plinth 模型通过多轮 Browser Tool Call 驱动的有状态浏览器交互。首个调用创建会话并取得 Browser Identity 独占权，后续调用携带同一会话身份，每次 Tool Call 仍分别形成一个 Lintel 子 Attempt。v1 封闭动作只有 session/page open/close/switch、goto/back/forward/reload、click/fill/select/check/uncheck/press/scroll、read/screenshot/wait_for 与 dialog accept/dismiss；禁止任意 JavaScript、Playwright 代码、CDP、raw HTTP、文件上传和下载，意外下载被阻止并返回 `DownloadBlocked`。`ElementNotFound`、`ElementNotUnique`、`ActionTimeout`、`NavigationFailed`、`DialogBlocked`、`DownloadBlocked` 与短期 element reference 失效是返回模型且不结束 Session 的可恢复 Tool Result；明确未登录、profile/Chromium 崩溃、Runtime/协议失效、trace 提交失败、取消、父 Attempt 终态或 lease 丢失才结束 Session，分类由固定 Tool 契约拥有，模型不得覆盖。每个动作返回有界结构化观察：当前 URL/origin/title、page 列表、可访问性树与可见文本投影、短期 element reference/role/name/state、导航/重定向/popup/dialog 事件以及明确的 truncation/原始大小/观察版本；不返回完整 HTML/DOM、storage state、Cookie、网络正文或浏览器内部对象，Screenshot 只由显式动作取得。Browser Identity 必须使用业务系统自身的只读账号，外部系统权限是禁止业务写入的权威边界；Lintel 不从 DOM 文案、控件类型或 HTTP method 猜测副作用。Exploration 可导航到基础设施网络边界内任意可达 origin，所有顶层导航、重定向链与新窗口 origin 都进入结构化日志；应用层不维护重复的 origin allowlist。显式关闭、父 Investigation Attempt 准备终态、取消、lease 到期或 Runtime 断开触发会话收口；显式取消时父 Attempt 先进入 `Cancelling` 并在会话 trace/operation 终局后进入 `Cancelled`；自然成功、失败或中断时父 Attempt 保持 `Running`，先收口会话再直接进入目标终态。身份锁仍须等物理停止确认后释放。Lintel 只机械执行动作，不拥有第二个模型或自主探索目标。
_Avoid_: 每次 Tool Call 从起始 URL 重开、一个自然语言目标交给 Lintel 自主探索、跨父 Attempt 复用会话、把瞬时页面会话当持久 profile generation、用 DOM/按钮名猜测是否只读、应用层 origin allowlist

**Journey Catalog**：
由 Lintel 中版本化 Playwright Journey 的同一机器可验证来源生成，并在构建时同时嵌入同版本 Quoin 和 Lintel。每个 Journey 同源声明类型化参数 Schema、不可变步骤定义/version、类型化 output Schema、允许产出的文本事实/截图/结构化 Evidence kind；authentication probe 另声明专用三态结果且 Evidence kind 集合为空，结果只进入 probe ledger。Journey ID 是稳定行为契约；兼容实现修复可保持 ID 并递增 version，参数、步骤业务语义或输出/Evidence 契约变化必须使用新 ID。Identity Revision 与配置版本记录创建时静态校验所用 Catalog provenance，但不永久 pin 整份 Catalog；协调升级后，新 Browser Operation 对同一稳定 ID 自动采用当前 ready Catalog 的兼容实现 version，并冻结本次实际 digest/version。Lintel 只能返回 Catalog 声明的输出：success 必须恰有一个 structured primary Evidence proposal，其正文是 typed output 的唯一权威实例；业务 gap 的 Evidence 列表固定为空，只携带封闭 gap 事实。Quoin 按 operation 实际冻结的 catalog digest、Journey ID/version 与 output Schema 验证 success payload，并以不可变 Journey Result ledger 保存完整结果摘要与 outcome（success 另引用 primary Evidence），单次 INSERT 原子派生 check result 并收口 operation/Attempt；禁止通用 HTML dump、自由 JSON 或空占位 Evidence。Quoin 可在 Lintel 离线时静态校验 YAML；实际 catalog binding 只由每个 Browser Operation 保存，Run/Config Verification Run 不复制第二份执行权威。Journey 不自动整单重跑或从中间步骤恢复；Playwright locator 在固定 deadline 内等待不算重试，失败后重新采证必须创建新的 Run/Config Verification Run 与 Attempt。
_Avoid_: 任意 Journey 字符串、Quoin 分发用户代码、旧配置绑定旧 Runtime

**浏览器操作记录**：
三类浏览器操作采用不同记录边界：人工登录只保存操作者、业务系统、起止时间、结果和新 generation，不记录键盘、不截图、不生成 trace；一个模型 Browser Exploration Session 保存逐 Tool Call 的结构化动作与结果日志；日志只含动作类型、locator 的非秘密描述、page/origin、时间、结果、错误码、观察摘要 hash/大小和 Artifact 引用，不保存实际输入值或复制页面正文，模型看到的有界观察只由对应 Tool Result/Evidence 持有。Session 形成一份由全部子 Attempt 共同引用的连续敏感 trace，只把模型引用或失败诊断需要的截图单独保存为 Artifact，不重复保存完整 HTML、全量网络响应和逐动作截图；正常结束提交完整 trace，取消、崩溃或断流时尽力提交并明确标记 `incomplete`，不得伪装完整。完整 trace 是 Exploration 成功的强制审计 Artifact，提交失败使 Session 以 `ArtifactCommitFailed` 结束，已提交历史仍保留。确定性 Journey 的 success 以 primary structured Evidence 保存类型化结果正文；业务 gap 不制造 Evidence，只由 check result 与 operation 唯一的不可变 Journey Result ledger 保存 gap code、诊断和完整结果重放身份。失败时必须保留 trace，成功时只保留 Journey 声明产出或报告引用的截图；失败 trace 也提交失败时 check 记录 `ArtifactCommitFailed`，不能只显示原步骤错误。
_Avoid_: 三种操作共用全量录制、录制人工登录秘密、同一页面多份重复正文

**巡检运行（Inspection Run）**：
一个已发布计划在调度时刻或人工触发下产生的不可变机械采证记录。权威状态只描述采证：`Queued | Running | Completed | CompletedWithGaps | Failed | Cancelled | Interrupted | SkippedOverlap`。Completed 表示全部检查形成完整 Evidence，不表示系统健康；CompletedWithGaps 表示采证已结束并冻结结果，但存在 RuntimeUnavailable、AuthenticationRequired、部分响应或检查失败等缺口，即使没有成功检查，只要完整记录每项缺口仍属此状态；Failed 只表示无法形成并提交有效冻结结果集合。模型分析 Attempt/Report 使用独立状态，分析失败不回写 Run，页面可显示“采证部分完成/分析失败”。同一计划不并发：重叠定时周期 SkippedOverlap且不补跑，人工触发展示当前 active Run。定时创建时 Runtime 离线则相应检查 RuntimeUnavailable、其他继续；在线无浏览器容量时 Run 已进入 `Running` 并生成 `evidence_at`，只有对应 Browser Operation 进入 `WaitingForCapacity`，队列只在 Quoin。重试分析引用同一 Run，重新采证创建新 Run/evidence_at并以 rerun_of 引用旧 Run。
_Avoid_: 巡检计划、巡检报告、混合采证/分析状态、Succeeded=健康、离线补跑、Runtime 队列、跨时间追加

**执行尝试（Execution Attempt）**：
Plinth 或 Lintel 对同一个任务或 Run 的一次底层执行。Quoin 派发前持久化 attempt ID；Runtime 有 boot ID、递增 connection epoch、明确接受和有限 lease。lease 内同 boot 重连上报 active Attempt 调和而不重派；新 boot、lease 到期、身份吊销、崩溃或替换使 Attempt `Interrupted`。结果按 attempt ID 幂等，旧 epoch 迟到结果只审计。用户取消携带 command ID 与 expected version，Quoin 先事务提交 cancellation fence；Queued/Assigned 直接 `Cancelled`，Running 先 `Cancelling`，Runtime 确认或取消 lease 到期后 Cancelled。成功与取消按 SQLite 提交顺序裁决：成功先提交则取消返回已完成；取消先提交则迟到结果不产生有效消息、Report 或 Candidate。取消前已提交 Evidence/Tool/Artifact 保留为部分结果；取消 Run 停止未开始及运行子 Attempt但不删除已完成检查，页面/SSE断线/登出不隐式取消。第一版不设 Agent Attempt 总时长、调用数或产物限制；每次模型/API 调用有部署内部有限 deadline。幂等只读 API 对瞬态错误有界重试并记录物理尝试；模型只在明确可重试且未收到输出时自动重试，不切换模型/供应商。部分 token 后失败不得成为有效输出，Attempt `Failed` 并保存 Timeout/RateLimited/ProviderUnavailable/InvalidResponse/ToolError/ArtifactCommitFailed 等原因。Tool 失败可成为 Evidence 缺口，模型失败不生成成功结果。每次 Attempt 用干净工作区，成功前所有引用 Artifact 必须上传校验提交。
_Avoid_: 无限离线执行、前端取消标记、透明无限重试、部分模型输出成功、Agent 总预算规则引擎、恢复未提交工作区

**巡检报告（Inspection Report）**：
一次 Inspection Run 的一次成功模型分析形成的不可变版本，精确引用本次使用的 Evidence 集合、模型、Prompt 和 Execution Attempt，并包含程序产生的检查事实与证据缺口，以及模型产生的摘要、分析、诊断、建议和限制。失败分析只保留失败 Attempt，不产生成功 Report；同一 Run 重新分析复用原 Evidence、创建新 Report 版本，不修改旧报告也不重新采证。页面默认展示最新成功版本并保留版本历史。Succeeded 只表示模型分析完成，不表示检查正常或诊断已验证。
_Avoid_: 原始证据、巡检计划、可覆盖结果、重新采证

**证据（Evidence）**：
由确定性工具采集形成的不可变事实记录，保存目标、参数、观察时间、原始结果或 Artifact 引用、warnings、错误和完整性，供调查和诊断使用。模型报告即使以文件保存也仍是分析，不因成为 Artifact 而升级为 Evidence。
_Avoid_: 诊断、推测、用户上传材料、文件载体

**来源材料（Source Material）**：
用户提供的 Text Attachment、Knowledge Import Batch 原文等可追溯输入。其字节、来源、上传者和时间是事实，但正文主张未经确定性采集或人工验证，不自动成为 Evidence。
_Avoid_: 工具采集证据、已验证知识

**产物（Artifact）**：
Quoin 管理的持久字节载体，例如附件正文、截图、Playwright trace 和大型工具响应；它不是独立结论或可编辑文件实体。每份内容只有一个规范持久副本，逻辑对象通过 Artifact ID 引用；Artifact 的访问和保留继承逻辑所有者，不建设文件管理中心。Alert Delivery 原始 body 继续直接存 SQLite，以维持接入单事务语义。Raw Playwright trace 固定继承敏感诊断 Artifact 的访问与保留规则。
_Avoid_: Evidence、Source Material、模型结论、Runtime 本地路径、重复 BLOB

## 知识沉淀

**诊断反馈（Diagnosis Feedback）**：
运维人员对某个不可变诊断输出后续状态的 append-only 事件；目标精确指向 Initial Analysis 成功输出、Inspection Report 版本或用户选择的 Investigation assistant message/Attempt 输出，不关联整个 Investigation 或 Business System。最新事件形成 `None | Adopted | Executed | VerifiedEffective | Rejected` 投影，更正只追加事件不覆盖历史。Knowledge Candidate/KnowledgeVersion 保存精确来源；只有该来源当前 Rejected 时触发 SourceInvalid/退出检索，之后反馈更正也不自动复活旧知识，仍需创建并确认新版本。
_Avoid_: 模型自评、隐式点赞、调查级当前诊断、覆盖旧反馈、自动复活知识

**知识候选（Knowledge Candidate）**：
模型从初步分析、巡检诊断、知识导入批次或经用户明确要求保留的调查历史中整理出的待确认条目，状态为 `AwaitingConfirmation | Confirmed | Excluded | Superseded | SourceInvalid`。调查中只能由用户用自然语言主动表达保留意图后生成；Initial Analysis 与 Inspection Report 在最终分析区提供次要“整理为知识”操作；知识页面提供粘贴原文的批量导入入口，任何路径都不得自动写入候选或正式知识。模型原始建议不可变，用户修改产生递增 `draft_revision`；同一整理流程只有最新 generation 的最新草稿可确认，新一轮成功整理使旧未确认 generation 变为 Superseded。确认携带 command ID 和 expected revision，过期 revision 必须冲突；每个 Candidate 最多创建一个 Reusable Knowledge，相同命令重试返回原 Knowledge ID。来源调查回合在确认前被 Undo 或来源诊断被拒绝时进入 SourceInvalid，不能确认。确认表示未来复用许可，不等于已验证有效。
_Avoid_: 可复用知识、自动沉淀、原地覆盖、模型自行触发、每条回复的保存按钮、按正文相似度去重

**知识导入批次（Knowledge Import Batch）**：
用户主动粘贴的一段 Source Material 及其派生过程，状态为 `Processing | AwaitingConfirmation | Failed | Completed | Cancelled`。模型拆分整理为 Candidate，原文和候选确认前不参与检索。批量确认在一个事务校验全部 expected revision，全成或全不成；stale revision 冲突并返回最新草稿。Batch 只按当前 generation 中仍可操作的 Candidate 计算：存在 AwaitingConfirmation 则等待；不再有可确认 Candidate 时 Completed，即使未创建任何 Knowledge。SourceInvalid 是不可确认终态，旧 generation 的 Superseded 不阻塞完成；Cancelled 是 batch 级 fence，禁止后续编辑/确认但保留 Candidate，不把失效或替代伪装成用户选择的 Excluded。相同命令不得重复创建 Knowledge，也不按标题、正文 hash 或 embedding 相似度自动合并。
_Avoid_: 自动导入、永久等待批次、后台改写状态语义、逐条部分提交、可复用知识、已验证 Evidence

**可复用知识（Reusable Knowledge）**：
由人明确确认可以在未来调查中复用的稳定聚合，包含多个不可变 `KnowledgeVersion`，同一时刻最多一个 current version。修改标题、正文、范围、条件、限制或恢复复用必须创建并重新确认新版本；验证状态、Diagnosis Feedback、来源拒绝和停止复用通过追加事件及当前投影记录，不改写历史正文。来源拒绝或停止复用在同一事务中使版本退出正常检索；正文仍有价值时基于新有效来源创建新版本。FTS5 与 embedding 是同一当前有效正文的派生索引：current/资格变化时 FTS5 在同一 SQLite 事务更新；Embedding 按 `knowledge_version_id + embedding_model_generation` 异步生成，提交时复核版本/generation并丢弃迟到结果，Pending/Failed 不撤销正式知识，该知识仍可由 FTS5 检索并显示语义索引状态。换模型时完整构建新 generation、校验后原子切换，一次 cosine 检索不得混用模型。检索同时提供 FTS5 trigram 与 embedding cosine 两个工具，由模型综合，程序不设阈值或固定融合排名，并始终过滤 current、未停用、来源有效版本。
_Avoid_: 知识候选、知识导入批次、原地覆盖、混合 embedding generation、对话历史、原始报告、模型记忆

## 存储、保留与部署

**Artifact 提交**：
Artifact 的临时文件与最终文件必须位于同一文件系统：完成写入与 hash/大小校验后 `fsync` 临时文件，按 SHA-256 原子 rename 为不可变文件，再 `fsync` 最终父目录；只有父目录同步成功后 SQLite 事务才提交引用。引用事务失败后该文件作为无权威记录引用的孤立文件清理。未完成上传、校验、目录同步或引用事务的文件不能被成功 Attempt 引用；工作区、staging 和失败上传可自动清理。

**在线保留**：
结构化告警、调查、消息、诊断、报告、反馈、知识、Text Attachment 和 Knowledge Import Batch 原文长期保留。截图、Playwright trace 与大型工具响应正文等生成型大 Artifact 默认保留 90 天，使用一个由 Admin Web UI 管理并持久化在 SQLite 的部署共享设置；到期后保留元数据、SHA-256、来源、时间和“正文已过期”状态。Raw Playwright trace 固定为敏感诊断 Artifact：官方 Trace Viewer 可查看完整 DOM snapshot、console、request/response headers 与 body，因此 trace 不进入模型上下文、普通附件、FTS 或通用 read/grep；Operator 只看结构化动作日志、错误和显式截图，raw trace 仅 Admin 经审计下载。实现验收必须在锁定版本注入 sentinel Cookie、Authorization header、DOM token 和响应内容，生成真实 trace 检查 ZIP。撤回消息、停用系统或停止复用知识不删除来源历史。备份保留 30 份是独立规则。

**敏感内容下载**：
`sensitive=1` Artifact、raw trace 与备份只接受当前有效 Admin Session，不重复要求同一密码、不签发预签名或分享 URL。服务端在响应头和首字节前重验当前 User/Session/role 并提交非秘密访问审计，审计失败即拒绝；活动流绑定该 Session，Session 撤销、账号禁用或降级时立即中止剩余发送。每次 Range/续传请求都重新认证并审计，响应使用 `no-store`、`nosniff` 与 attachment disposition。

**秘密与日志**：
普通日志、指标标签、审计、持久诊断和 UI 技术详情使用字段白名单，默认不记录请求/响应 body、headers、gRPC metadata、完整 URL query 或任意对象 dump。秘密类型不可被普通字符串化，只能输出固定 `[REDACTED]`；外部适配器先映射稳定错误码与允许字段，再进入日志或数据库。验收向 Cookie、Authorization、密码、API key、kubeconfig、根密钥标记、provider 回显和浏览器 sentinel 注入唯一值，并扫描四组件 stdout/stderr、结构化日志与 telemetry，任一命中失败。用户主动上传文本与明确标为敏感的 raw trace 不做通用猜测式扫描，但不得被普通 logger 复制。四组件只向 stdout/stderr 输出 UTF-8 JSON Lines，不写或轮转容器内日志文件；固定字段至少包含 UTC timestamp、level、component、release、稳定 code 与 message，可带非秘密 correlation ID。

**运行配置权威**：
人需要理解和修改的运维设置由 Admin Web UI + SQLite 作为唯一权威，包括备份时间、时区、保留份数与生成型 Artifact 保留天数；部署配置只保存启动时不可推导的基础设施事实，包括 public Origin、卷路径、TLS/Secret 路径和内部调优值，二者字段不得重叠。监听地址不属于进程部署配置：容器内部 listener 使用产品固定常量，只有 Compose host publish port 与 Kubernetes Ingress/Service 映射属于打包输入。`contracts/schemas/deployment-config.schema.json` 是跨 Helm/Compose 的唯一机器权威；Chart `values.schema.json` 与 Compose 配置示例只能由它生成或经机械映射校验。镜像选择不属于可独立修改的部署配置，只能是 Release manifest 中不可变 digest 的投影。部署配置在进程启动时读取并在该进程生命周期内不可变，修改后走显式零重叠重启，不实现文件 watcher 或 SIGHUP 重载；Admin Web UI + SQLite 设置继续按各自领域 command 生效，根密钥只在启动时读取一次，Runtime/告警凭据只走已有显式轮换协议。

**进程配置输入**：
`deployment-config.schema.json` 在 `$defs` 中分别拥有 Quoin、Plinth、Lintel、Stele 的非秘密配置；每个进程只接受固定 `--config /etc/quoin/<component>.yaml` 的只读 YAML 文件，并在启动时按同一 JSON Schema 语义拒绝未知字段。Helm values 与 Compose 最少输入由生成器投影成每组件所需的最小文档，只挂给对应组件；秘密字段只保存独立只读文件路径。不得再为每个字段建设环境变量或重复 CLI flag 及优先级，环境只保留容器运行时本身需要的机制。

**公开入口与运维端点**：
Quoin 不签发证书，也不捆绑 Ingress Controller、cert-manager、Caddy 或 Nginx；Helm 接入部署者已有的 IngressClass 与 TLS Secret，Compose 接入已有反向代理与证书。public Origin 必须显式配置，不能从 Host/Forwarded 临时推导。四组件各自使用与产品接口分离的内部运维监听器，固定提供 `/livez`、`/readyz`、`/metrics`；这些端点不进入产品 OpenAPI、不做应用层认证。Helm 为每组件提供 ClusterIP-only 运维 Service并允许普通 annotations，可选生成 ServiceMonitor/PodMonitor，但只有部署者显式启用且集群已有对应 CRD 时才创建；基础 Chart 不强依赖 Prometheus Operator，也不猜部署专属 NetworkPolicy。Compose 的运维端口只在内部 network 暴露，不 publish 到 host。第一版只输出 classic histogram，不提供 native/classic 切换；桶由原型和实际 SLO 确定。所有指标 label 必须来自封闭低基数集合，不得使用用户、Session、Attempt、URL、错误文本或其它无界值。

**服务暴露拓扑**：
Helm 把 Quoin 的 public HTTP、内部 Runtime gRPC 与 ops listener 分成三个 ClusterIP Service，把 Stele 的 Alertmanager webhook 与 ops listener 分成两个 ClusterIP Service；Plinth/Lintel 只主动出站连接 Quoin，除各自 ops Service 外不创建业务入站 Service。只有 Quoin public 与 Stele webhook 可由部署者已有 Ingress 接入，Runtime gRPC 与全部 ops Service 禁止进入 public Ingress。Compose 默认只把 Quoin public 与 Stele webhook publish 到 `127.0.0.1` 的可配置端口，供宿主反向代理或 Alertmanager 使用；Runtime gRPC 与全部 ops listener 只 `expose` 在内部 network。部署者把外部代理接入同一 network 时以 `publishMode=internal-network-only` 关闭 loopback publish。容器内部端口固定为 Quoin public `8080`、Runtime gRPC `8443`、四组件 ops `9090`，Stele webhook 在自身网络命名空间复用 `8080`；Chart Service 由模板以这些常量固定投影，进程配置不允许改写内部 host/port。

**指标机器契约**：
`contracts/metrics.yaml` 是四组件自定义 metric family 的唯一机器权威，定义 family name、type、HELP、label names 与封闭 label values；只由 metrics 拥有的枚举可以在其中定义，maintenance reason、Runtime outcome、Attempt kind 等已有机器权威的集合必须引用 `schema.sql`/`runtime.proto` 等所有者，并由 fixture 断言投影集合严格相等。HTTP 指标只使用封闭 `route_group`、`method`、`status_class`，gRPC 只使用封闭 `rpc_group` 与 canonical status code；完整 URL、path/query 值、用户、对象 ID、每个 OpenAPI operation 与动态错误文本不得成为 label。每组件从启动起导出无 label 的 `<component>_ready`；Quoin 另导出无 label 的 `quoin_accepting_work` 与 `quoin_maintenance{reason=<schema.sql 权威集合>}`，精确 not-ready reason 只在 `/readyz` 固定 JSON 与 JSON 日志中出现。所有预知序列启动即显式导出 0。`*_total` 是允许进程重启归零的内存 counter，不扫描 SQLite 历史或增加指标持久表；active/in-progress/firing/slot/ready 等 gauge 才从当前 SQLite 或内存权威投影。

**Prometheus 告警规则**：
`operations.md` 定义最小推荐规则；只有部署者显式启用且集群已经安装 Prometheus Operator CRD 时，Chart 才生成同语义 `PrometheusRule`。规则至少覆盖组件持续 not-ready、Quoin 持续不接工作、备份失败/陈旧/卡住、Runtime 持续离线、stream queue overflow、storage 不可写、Artifact GC 陈旧与 Stele 持续 unavailable。基础 Chart 不创建 Grafana dashboard、不内置通知器、不强依赖 Operator；通知路由继续由部署者现有 Prometheus/Alertmanager 拥有。

**SQLite 运行耐久**：
锁定的 SQLite 构建使用 WAL 与 `synchronous=FULL`；FULL/NORMAL 不暴露为部署开关。每条连接仍必须在执行领域 SQL 前设置并读回 `foreign_keys=ON` 与 `recursive_triggers=ON`。恢复首先以完整发布的 manifest + checksum 为门禁，再附加执行 `integrity_check` 与 `foreign_key_check`；PRAGMA 成功不能替代 manifest 完整性。

**一致备份**：
自动备份通过独立空闲 SQLite 连接执行 `VACUUM INTO` 生成单文件一致快照，再从快照枚举精确 Artifact hash 集合；Artifact GC 与复制阶段互斥。备份复制校验快照引用的 Artifact，最后以“临时文件写入并 `fsync` → 原子 rename → rename 后 `fsync` 父目录”的顺序耐久发布 DB/Artifact SHA-256 manifest；任一引用缺失或目录同步失败整次失败，新备份校验成功后才清理超出 30 份旧备份。FTS5 与 embedding 不是恢复业务事实所必需的权威数据；采用同库布局时，FTS5 shadow tables 与 embedding BLOB 会随 `VACUUM INTO` 物理进入快照，恢复后可校验、丢弃并重建。Attempt 工作区、临时文件和 Browser profile 不进入备份。备份归档不做应用层整体加密；其中连接凭据字段仍保持自身 AEAD envelope，其余内容的机密性由独立 PV/目录权限、存储层加密、传输与 Admin 下载边界负责，manifest/checksum 只负责完整性。Admin 可浏览、显式下载和立即触发备份，下载审计；恢复只由拥有 PVC/数据目录和根密钥 Secret 权限的部署操作者在 Quoin 停机时执行，Web Admin Session 不是恢复权限。恢复实现只复用同一 Release 的 Quoin 镜像与二进制中的 `quoin restore` 子命令：Helm 用一次性 Pod/Job 包装，Compose 用 `docker compose run --rm` 包装；不要求宿主机安装第二套 CLI，也不维护独立 restore 镜像。操作者先停止业务工作负载，再挂载数据卷、备份卷和根密钥文件；TTY 只承担已定案的恢复 Admin 选择与临时密码一次显示。备份目录只允许 Quoin UID 与部署操作者访问，Artifact 路径仅由 hash 推导。恢复、升级和根密钥 rebind 复用 SQLite 中的单行维护状态与按对象维护清单；离线工具在发布恢复库前的最后事务写入，恢复事务同时清除全部 Web Session、retire 全部 Runtime credential 与告警源 Bearer、revoked 两个 Runtime slot、禁用除 TTY 选定恢复 Admin 外的用户并给该 Admin 设置强制改密的临时密码、把全部 Connection 置为不可派发的 `RevalidationRequired`、Browser Identity 置为 `AuthenticationRequired`，并写 system Audit Event；数据库外 Stele service token 不因普通恢复改变。维护期间只开放登录/登出/当前用户/改密、维护与健康诊断读取、Admin 信任重建操作及退出维护，普通任务、告警接入、调度、SSE 与业务下载上传统一拒绝。恢复退出按“安全收口”而非“全部能力 Ready”裁决：用户必须已重新启用或保持禁用，Connection 必须重验/重录或保持 disabled，Runtime slot 必须重新注册或保持 revoked，告警源必须有新凭据或保持 disabled，Browser Identity 保持 `AuthenticationRequired` 即为安全；这些隔离状态由恢复事务先建立，因此不强迫恢复可选能力。升级使用独立版本/迁移清单，不重做恢复身份清单。退出由 Admin 以 command ID 与 expected maintenance row version 显式提交并在同一事务重验全部阻塞项，不提供通用绕过。

**备份运行状态**：
`backups` 是可查询的受限状态机聚合，而不是只能在结束时追加的终态记录：状态为 `queued|running|succeeded|failed`，阶段为 `queued|preflight|database_snapshot|artifact_copy|manifest_publish|completed`，触发来源为 `manual|scheduled|upgrade`；只允许相邻前向迁移，终态后不可修改且任何状态均禁止 DELETE，失败必须记录稳定 `error_code`、`retryable` 与有界详情；任一时刻最多一个 active backup。立即备份先持久化 active row 再返回 202，同一 `client_command_id` 重试返回同一 row，其它手动触发返回携带 active ID 的冲突；定时触发与 active run 合并，不排第二份；升级前备份使用同一聚合并标记 `trigger_kind=upgrade`。Quoin 启动时必须先把上一进程遗留的 `queued|running` 行收敛为 `failed`，记录稳定的进程中断错误码和结束时间，然后才开放新触发。停机错过多个计划时最多补最新一份，不逐个回放；失败不删除旧成功备份，下一个正常周期继续；第一版不提供取消备份。

**Artifact GC 协调**：
唯一 Quoin 进程内有一个 artifact-storage coordinator；备份从快照枚举到复制完成期间独占，GC 只执行有界小批次，备份到达后完成当前批次即让出。GC 在启动后和固定周期唤醒，健康运行时保证到期对象 24 小时内处理；周期与 batch size 是内部调优，不进入 Admin 或部署配置。单 Quoin + 数据目录进程锁已经排除第二 writer，不增加 SQLite lease、独立 GC 进程或 sidecar。

**单组件拓扑**：
第一版固定 Quoin、Plinth、Lintel、Stele 各一个 active replica，不提供 replicas 或 HPA 配置。Helm 与 Compose 均采用零重叠替换；Kubernetes Deployment 使用 `Recreate`。Quoin 持有 `quoin-data` 进程锁，Plinth/Lintel 持有各自状态目录锁，第二实例无法取得锁时启动失败。每个锁都由进程在状态目录固定 lock file 上取得非阻塞内核 advisory exclusive lock并持有文件描述符至退出；lock file 不是 PID 权威，不检查或删除“陈旧 PID”。Quoin 服务、migration、restore 与 root-key rebind 争用同一个 `quoin-data` 锁，失败时立即用稳定错误码退出。SQLite WAL 数据卷只支持本地或块存储，不支持 NFS/SMB；CSI 支持时优先 `ReadWriteOncePod`，否则使用 `ReadWriteOnce + Recreate + 应用锁`。`quoin-data` 同卷保存 SQLite、Artifact 发布 staging 与 Artifact 最终文件，确保 staging→final 同文件系统原子 rename；`quoin-backup` 必须是独立卷；`plinth-state` 与 `lintel-state` 各自独立；Stele 无持久卷。只有 Attempt workspace、普通 `/tmp` 与 Lintel 专用 `/dev/shm` 使用可丢弃卷。

**发布架构与镜像运行时**：
一个 Release 同时发布 `linux/amd64` 与 `linux/arm64`，四镜像都形成多平台 OCI index；Release manifest 同时记录 index digest 和每个平台 manifest digest。两种架构必须在原生 runner 上执行同等安装、Runtime、浏览器、noVNC、身份卷、trace、备份恢复及升级验收，QEMU-only、只完成镜像构建或模拟 Lintel 不构成架构支持。Lintel 使用与锁定 Playwright 版本匹配的同一浏览器 revision，各架构下载工件分别锁 SHA-256，只有双架构完整验证同时通过才可发布。

Quoin、Stele 固定使用 digest-pinned Debian 13 `static-debian13:nonroot`，因此二进制必须纯静态并通过动态依赖扫描；不保留隐式 `base/cc` fallback；若真实动态链接审计证明必须改用 `base/cc`，必须先显式修改本权威记录与 release input lock 并重跑双架构完整验收，构建逻辑不得条件选择。Plinth 使用 digest-pinned Debian 13 slim nonroot，必须提供真实 `/bin/bash` 及固定本地工具集；Lintel 使用 digest-pinned Debian 13 slim browser runtime。`contracts/release-inputs.yaml` 唯一锁定基础镜像 index/per-platform digest、Playwright/Chromium 双架构工件、Playwright browser/native-deps 来源和 Lintel 直接安装 Debian package/x11vnc 的双架构完整版本；`contracts/plinth-worker-tools.yaml` 是 Plinth worker 可执行工具、绝对路径、每架构直接安装 Debian package 版本和只读运行时路径的唯一机器目录。Bash Tool 固定在当前 Attempt 工作区执行 `/bin/bash --noprofile --norc -c`，使用清空后的环境；shell 的存在不是安全边界，凭据隔离、Landlock、seccomp 与真实对抗自检才是边界。所有 base、browser、源仓拥有的直接安装 package 与工具在 release source 中锁 digest/version，更新只能经独立依赖变更和双架构完整验收，容器启动时不得安装或下载。

**容器执行边界**：
四镜像使用固定非 root UID/GID；Kubernetes 设置 `runAsNonRoot=true`、`allowPrivilegeEscalation=false`、只读 root filesystem、drop `ALL` capabilities 与 `seccompProfile=RuntimeDefault`，持久卷使用匹配 `fsGroup` 和 `fsGroupChangePolicy=OnRootMismatch`，Chart 不提供关闭这些边界的普通 values 开关。四个正常运行 Pod 均设置 `automountServiceAccountToken=false`，Chart 不创建业务 RBAC；Plinth Kubernetes 工具仍只使用 Quoin 下发的连接凭据，restore/upgrade 只复用部署操作者已有的 helm/kubectl 权限。

Lintel 启动 Chromium 时固定使用 `--no-sandbox`，不要求 user-namespace sandbox、sandbox self-test 或 setuid sandbox。这是明确接受“Chromium 内部 sandbox 全部关闭”的运行边界，不是探测失败后的临时降级；剩余隔离只由非 root 容器、只读 root filesystem、capability/seccomp、Lintel 专用身份卷及基础设施网络边界承担。

Lintel 在 Kubernetes 以 memory-backed `emptyDir`、在 Compose 以等价 tmpfs/shm 挂载独占 `/dev/shm`，Helm/Compose 省略容量时默认 1 GiB并允许部署者显式调整；生成器把解析后的字节下限投影为 Lintel `minimumShmBytes`。启动时必须验证该目录可写且实测总容量达到该值，不足则启动失败且 not-ready；不得静默添加 `--disable-dev-shm-usage` 回退普通 `/tmp`。全部固定 browser slots 共用该容量，应用不动态扩容。

**首次秘密引导**：
第一版默认提供无需人工拼接随机字节或 OpenSSL 命令的自动引导，但只允许在确认 `quoin-data` 尚无任何部署事实的空白首次安装执行。Helm 使用同一 Quoin 镜像运行一次性 bootstrap Job：Chart 以 `lookup` 保证已有固定命名 Secret 绝不被重渲染或覆盖，只在对象不存在时先创建不含 secret value 且 uninstall 保留的空 Secret，再创建短生命周期 ServiceAccount/Role；Job 经 Kubernetes API 把生成结果原子写入该 Secret，Role 只允许读取和更新这个确定名称，Job/ServiceAccount/Role 成功后删除，四个正常 Pod 继续 `automountServiceAccountToken=false`。Job 在 Secret 已存在时只验证长度、SAN 与证书链；Secret 缺失时必须先确认目标 `quoin-data` claim 不存在或确实为空，已有 claim/SQLite/Artifact/引导完成标记但秘密缺失或部分存在时 fail closed。Compose 使用同一 Quoin 镜像的一次性 bootstrap service，以目录锁、`O_EXCL`、`0600`、文件及父目录 `fsync` 把 32-byte 根密钥、32-byte Stele service token 和部署私有内部 TLS CA/server certificate 写入部署目录外的独立秘密目录；Quoin 与 Stele 只读挂载同一 Stele token 文件，其他正常容器只挂载自身所需材料。升级永不自动重建。Chart 模板禁止 `randBytes` 或把 secret value 放进 values、环境变量、Helm release history、Release manifest、日志；已有 Secret/file 可直接复用，自动引导不接管轮换。

**Runtime 首次注册与替换**：
Plinth/Lintel 正常进程只从各自状态卷固定 `0600` 文件读取长期 token，不接受一次性注册令牌的配置字段、环境变量、argv 或 Kubernetes Secret。首次注册或替换时，Helm/Compose 薄向导逐个停止目标 Runtime 并证明状态卷锁空闲，使用同一组件镜像运行 `runtime register --token-stdin` 一次性子命令；操作者在 Web UI 为对应 slot reveal 60 秒令牌后只经 attached stdin 传入，子命令经 TLS 注册并将返回的长期 token 以临时文件、`fsync`、原子 rename、父目录 `fsync` 写入状态卷，全程不回显或记录 token。成功后删除一次性 Pod/container 并启动正常单副本；失败保持停止并重新签发令牌。注册令牌不得进入 YAML、shell argv/history、文件、Secret、Job/Pod metadata、Helm history 或日志。

**发布工件权威与分发**：
`contracts/schemas/release-manifest.schema.json` 定义每个 annotated SemVer tag 唯一的 `release-manifest.json`，记录 source commit、四个多平台 image index/per-platform digest、浏览器 revision、OCI Chart digest、Compose bundle SHA-256、配置/数据库/proto/catalog version 与验收摘要；这些 version/revision 必须从各自机器权威或依赖锁定文件机械读取并断言相等，不允许手填。Playwright 浏览器锁还固定 tag 下 `browsers.json` 与 registry 源码的 SHA-256，并按其 Debian 13 平台映射分别派生 x64 CFT URL 与 arm64 non-CFT URL；不同路径族不代表不同 revision。运行只消费 digest，不发布 `latest`。同版本 Chart 以 OCI artifact 发布并附同一生成的 `.tgz`；Compose 发布只含 digest-pinned `compose.yaml`、最少输入模板、Schema 与安装/升级/恢复向导的 `.tar.gz`，不把 release manifest 放进该 bundle，从而避免 manifest 记录自身容器的哈希。Chart 与 Compose 都只能投影同一 manifest，不维护独立镜像版本表。

四镜像由 BuildKit 生成 SPDX SBOM attestation 与 SLSA provenance v1 `mode=min`；CI 以 Sigstore/cosign OIDC keyless 身份签署四个 image index、各平台 manifest、OCI Chart、release manifest、Compose bundle、两个架构的 `quoin-deploy` 及离线归档，并保存可离线携带的 Sigstore bundle；Release manifest 以封闭 `sigstore_bundles` 映射记录每个外部 bundle 的可发现资产名。发布门验证 certificate identity/issuer、每个外部 Sigstore bundle 的签名 subject、SBOM/provenance subject 与 release manifest 中对应对象 digest 全部相等；签名 subject 不写回被签的 manifest，且不保存长期签名私钥。每版另发布一个包含四镜像完整 amd64+arm64 OCI index、Chart、Compose、release manifest 及内部验证材料的离线 `.tar.zst`；归档自身的签名/bundle 必须作为外部 sidecar，不能放入归档形成哈希自引用。导入向导复用标准 OCI 工具复制到目标 registry 并保持 digest，随后逐项读回 index/per-platform digest；只有完全等于 release manifest 才能部署。

**安装、运维与恢复操作者路径**：
每版必须同时交付 OCI Helm Chart、Compose bundle、离线 OCI 归档和从同一源码为 linux/amd64、linux/arm64 分别构建的 `quoin-deploy` 单文件薄 helper，部署者下载对应资产后统一以 `quoin-deploy` 调用。helper 只能编排操作者本机既有的 `helm`/`kubectl` 或 `docker compose`，不得复制部署状态或成为第二套恢复格式；在线备份/升级写动作只由已登录 Admin 在 Web UI 发起，helper 不接收 Web 凭据或调用产品写 API，只观察无认证 ops 指标；离线 backup/restore 则复用同一 Release Quoin 镜像的领域子命令与相同 SQLite/Artifact 契约；普通路径固定为 `compose|helm install|upgrade|backup|restore|verify`，并覆盖空白秘密 bootstrap、长期 workload 启动前经 attached TTY 离线创建首个 Admin、逐个 Runtime 注册/替换、升级 drain/自动备份、停机恢复、`install --offline-archive` 离线导入和结构化验收报告。Helm/Compose 只接受 `contracts/schemas/deployment-config.schema.json` 定义的最少人工输入，其余配置由工件生成；产品 Web UI 继续拥有备份计划、保留和其它业务设置。

**协调升级**：
四个 OCI 镜像由一个 Release 版本统一决定，不承诺 N/N-1 在线滚动兼容。Release manifest 记录四镜像不可变 digest，Helm/Compose 默认只消费该 digest；SemVer tag 仅用于人类识别，组件握手版本来自同一构建元数据。升级 helper 指示已登录 Admin 在 Web UI 执行 `prepareUpgrade`，由 Quoin 进入维护状态、停止新人工任务和新调度并显示活跃 Attempt、浏览器操作与备份 preflight 清单；只有 Admin 明确同意时才通过现有 UI 取消命令取消剩余对象。helper 不持有 Web 认证，只轮询无认证 `quoin_upgrade_prepared`；Quoin 在清单安全后以 `trigger_kind=upgrade` 创建并完整验证同一 Backup Run，只有当前 Upgrade maintenance revision 全部 Safe 且备份 succeeded 时该 gauge 才为 1。随后 helper 停止 Stele、Plinth、Lintel 和旧 Quoin，并用同一 Quoin 镜像离线复核 SQLite maintenance revision、清单与备份 digest。新 Quoin 独占数据目录执行前向 migration，完成前不 Ready，随后启动同版本 Plinth、Lintel、Stele；版本不匹配时组件不 Ready、不领任务、不接收正式告警。维护期间错过调度记录 RuntimeUnavailable 且不补跑。发布包提供薄的、平台专属的交互式升级向导，复用 `helm`/`kubectl` 或 `docker compose` 机械执行该序列；只在仍有 active task 是否取消以及恢复/回滚等真实业务选择处询问，不用 Helm hooks 或启动钩子隐式迁移、自动取消任务。新版本接受新写入前可以恢复备份并回旧版本；接受新写入后禁止只回滚镜像，必须显式恢复升级前备份。Git 只保存版本化声明式 seed scenario 和期望断言，不提交人工制作的 SQLite/PVC 二进制；首版只验 fresh install，之后每版从上一正式 release manifest 取得真实 digest，启动 N-1 并经公开 HTTP/Runtime 路径创建用户、连接、告警、Attempt、Artifact、知识和成功备份，再验证 N-1→N Helm/Compose 协调升级、migration、恢复及接受新写入前回滚。生成数据与 manifest 只作为该次 CI 证据，不成为长期 fixture 权威。

**Compose 生命周期**：
四服务使用 `restart: unless-stopped`。Plinth、Lintel 与 Stele 可以用 `depends_on: quoin: condition: service_healthy` 改善首次启动顺序，但运行中 Quoin 重启仍只依赖各组件自己的重连与 Ready 契约；禁止用 Compose `restart: true` 把 Quoin 的显式重启传播成隐式全栈重启。完整版本升级只能走协调升级向导，状态目录应用锁仍是 Compose 容器重建可能重叠时的最终裁决。

**健康语义**：
Quoin 取得数据目录锁并完成 migration 前不 Ready；migration 完成且 maintenance-safe API 可用后，即使仍处于 maintenance，`/readyz` 也返回 200，并明确 `mode=maintenance`、`acceptingWork=false`，普通任务仍由业务门禁拒绝而不能把维护 UI/API 从 Service backend 移除。Plinth/Lintel 只有在 token、版本握手和控制流被 Quoin 接受后 Ready；Stele 只有版本握手及告警源 digest 快照加载成功后 Ready。Quoin 暂时离线时 Runtime 保持 live 但 not ready，不得触发循环重启；Compose `depends_on` 只改善启动顺序，不能替代组件自身重连和 Ready 判断。`startupProbe` 只等待内部运维监听器启动；`/livez` 只检查本进程仍能推进且无本地不可恢复 fatal state，不依赖 Quoin peer、外部系统、磁盘余量或 maintenance；`/readyz` 才检查组件能否承担职责。`/readyz` 响应的字段、必填关系与封闭值由 `contracts/schemas/readiness-response.schema.json` 独占，不含动态错误文本；探针数值由打包清单拥有并通过慢 migration/慢启动测试，不进入普通用户设置。

**优雅关停**：
Kubernetes `terminationGracePeriodSeconds` 与 Compose `stop_grace_period` 统一为 60 秒。四组件收到 SIGTERM 后立即停止新准入并进入 draining，最多使用前 45 秒完成当前 SQLite 事务、Artifact 原子发布、in-flight HTTP/Stele 请求、Runtime GoAway/结果确认与 Browser stop/incomplete trace 收口，至少留下 15 秒关闭连接和退出。不得等待任意长的模型或浏览器工作自然完成；未完成工作按既有 fence/reconcile 语义收口，满足续走条件的调和继续，否则才进入 Interrupted/技术终止，不得强制打断可调和 Attempt或伪装成 Cancelled/Success。清单不使用 sleep 型 preStop hook，核心关停只由幂等 SIGTERM handler 实现。

**资源边界**：
第一版不默认设置 CPU/内存 limits，Helm requests/limits 可选且默认空；Compose named volume 不施加应用层容量限制。Kubernetes PVC 容量直接影响数据安全，必须由部署操作者显式填写。Quoin 不以任务数、Artifact 大小或静默删历史实现应用层资源配额，也不定义统一剩余百分比阈值。已知大小的 Artifact/备份操作必须针对目标目录和所需字节做精确 preflight；任何 `ENOSPC`、`EDQUOT`、`EROFS` 或持久化 `fsync`/rename 失败都把对应 storage health 置为不可写、使 Quoin `/readyz` 失败、令 `quoin_storage_writable` 为 0，并在 SQLite 仍可写时把当前领域任务或 Backup Run 持久化为稳定失败。恢复必须在同一目标目录通过真实 create→write→fsync→rename→父目录 fsync→unlink probe，且当前操作的精确 preflight 通过；不得靠百分比自动清除、静默删历史、预留隐藏文件或自动扩容。

**部署、恢复与升级验收**：
CI 必须在一次性真实 Docker Compose 环境和真实 Kubernetes 测试集群（kind 或等价）分别完成安装与运行验收，不能用 template/lint/build 替代。最小矩阵必须在原生 amd64 与原生 arm64 各自覆盖：四镜像按多平台 index digest 启动、探针与 metrics scrape、首次自动秘密引导及既有数据缺秘密 fail-closed、首次 Admin 登录、Plinth Bash/固定工具目录与 Landlock/seccomp 对抗自检、Lintel 真浏览器/noVNC/身份卷/trace、Stele Delivery、含 Artifact 的成功备份、停机恢复、恢复后的 Session/Runtime/告警凭据失效、从上一正式 Release 动态生成数据后升级、接受新写入前回滚、容器/Deployment/Compose service 重建并复用既有 PVC/named volume 后数据仍在、SIGTERM 关停、存储故障与 metrics sentinel 泄漏扫描；另必须实际验证离线归档签名、解包、registry 导入和 digest 读回。Helm lint/schema、Compose config、OpenAPI/SQL/proto 校验继续作为更低层门禁，但不能据此声称真实安装、恢复或升级通过。

**验证声明分层**：
验证结论严格分为 Contract Gate、Release Qualification 与 Deployment Acceptance。Contract Gate 只证明机器契约、Schema、静态规则和确定性状态机/集成断言；Release Qualification 只证明某一组不可变 Release 工件在项目控制的真实 Compose、Kubernetes、浏览器和原生双架构环境中通过发布矩阵；Deployment Acceptance 才证明该工件在具体站点的真实 ingress、存储、网络、Thanos、Model Provider 等部署事实。每层只能声明实际覆盖范围，低层通过不得推导高层通过，项目控制的协议 fixture 也不得被表述为任意真实外部系统兼容。

**验证规范与目录权威**：
各领域稳定条款继续独占行为断言；`verification.md` 只拥有执行层级、环境矩阵、故障编排、证据规则与 verdict 聚合的规范语义，并按稳定条款 ID 引用所有者，不复制字段、枚举或行为正文。`contracts/verification-catalog.yaml` 及其 Schema 是跨域 scenario 登记的唯一机器权威，只拥有条款组合、前置条件、环境能力、故障原语、可观察断言、必需证据与清理要求；测试实现必须声明 scenario ID，CI 机械检查 required scenario 无缺失、重复或悬空引用。

**发布验证证据**：
发布结果复用 in-toto Statement v1 与 Test Result predicate v0.1 的一次 suite invocation 语义，并由严格 Quoin profile 约束。subject 只绑定被验收的不可变 Release 输出，不把随后生成且反向引用证据的 Release manifest 自身列为 subject；configuration 绑定验证目录、环境描述与工具锁；passed/warned/failed 名单只接受 scenario ID。DSSE 信封签名及 Sigstore bundle 复用现有发布链，Release manifest 通过既有 `validation.<category>.evidence_sha256` 逐分类单向绑定对应证据 bundle，禁止建立哈希自引用、顶层竞争字段或第二发布索引。JUnit、HTML、CI annotation、日志和截图都只是同一权威结果的投影或 digest 附件。

**验证 verdict 与证据纪律**：
Suite 状态为 `PASSED | WARNED | FAILED`，发布门只接受 PASSED：本 invocation 的全部 applicable required scenario 必须首次执行通过且无跳过/警告；基础设施中断、结果不确定、诊断重跑后才通过或必需人工观察未完成均为 WARNED；产品/契约断言失败为 FAILED。每次重跑创建新 invocation，旧结果不可覆盖；不适用只能由 catalog 前置条件机械判定并记录理由。每个 scenario 的结构化 evidence index 记录 invocation、时间、环境 digest、工具版本、脱敏 argv、exit code、逐断言 expected/actual/result、附件 SHA-256 与清理结果。人工观察使用 typed observation，经确定性 verdict 程序校验后仍以同一 scenario ID 进入统一结果列表并绑定 observation digest，不建立人工 checklist verdict。

**验证环境与职责边界**：
Release Qualification 只使用合成数据、短期测试凭据与唯一 sentinel，禁止生产凭据和生产数据进入流水线；公开 evidence 必须通过 sentinel/秘密扫描，敏感 trace 只留受限存储并在公开报告记录 digest、分类和受限 locator。纯状态机、SQL 约束与 HTTP/Proto framing 可以由确定性 harness 证明；调度、Pod/PV/NetworkPolicy/Ingress/Compose 生命周期必须真实运行；DOM、render、noVNC、WebSocket 与 accessibility tree 必须使用真实浏览器；双架构声明必须来自原生 amd64/arm64，QEMU 只能作为构建或辅助诊断证据。确定性程序收集事实、校验契约、计算 scenario/suite verdict 并生成签名报告；人工只提交 catalog 要求的 typed observation；Agent/模型只做失败分析、汇总和建议，不得改写结果、降低 required 门或把 WARNED/FAILED 改成 PASSED。

**验证触发与支持矩阵**：
PR 阻塞执行完整 Contract Gate（含全部确定性状态机/集成 scenario），v1 不建设 source-to-scenario 影响选择器；main 定时任务执行完整自动化矩阵；annotated SemVer tag 只针对该 tag 构建的不可变工件重新执行完整 Release Qualification 并补齐人工 observation，PR/nightly 结果不能成为 tag 发布证据，不同 tag/release manifest digest 之间也禁止复用通过证据。每次 qualification 启动时解析 Kubernetes 官方当时维护的最近三个 minor 的最新 patch，并在 evidence statement configuration 的环境描述/工具锁中冻结精确版本；三个版本都在原生 amd64/arm64 上执行完整 Kubernetes 矩阵，只声明这六个精确 cell，不外推 EOL minor、未执行 patch 或中间版本区间。Compose 每个 tag 同样只冻结一个当时 stable 的 Docker Engine+Compose CLI 精确版本对并在双架构完整验证，不外推兼容区间；这些测试环境版本都不进入产品供应链 `release-inputs.yaml`。Web UI 只声明双架构 Release 锁定 Playwright Chromium 与 amd64 上解析后冻结的精确 branded Chrome version/build；Lintel/noVNC 只声明 Release 锁定 Chromium，v1 不声明 Firefox、Firefox ESR、Playwright WebKit 或真实 Safari 支持。

**外部系统与故障执行**：
Release Qualification 使用官方 digest-pinned Prometheus、Alertmanager、Thanos 镜像验证真实协议 happy path、查询语义和 webhook；错误码、半响应、畸形响应及应用层响应超时由 deterministic protocol fixture 拥有，传输层 TCP timeout/reset 由网络故障原语拥有；Model Provider 继续只使用 deterministic fixture，真实客户系统、生产凭据和真实模型供应商只属于 Deployment Acceptance。catalog 只声明工具无关的封闭故障原语：已有执行路径的进程、资源与网络原语映射到 Docker/Kubernetes 原生 stop/kill/pod delete/NetworkPolicy/重建操作，TCP 原语映射到 digest-pinned Toxiproxy 的 latency/timeout/reset_peer/bandwidth/limit_data；v1 不引入通用 Chaos 平台。ENOSPC、EDQUOT、EROFS、指定 fsync 失败和指定 rename 失败是互不替代的精确 required 原语；冻结 catalog 前必须通过一次性 Compose+Kubernetes 原型逐项证明 operation、注入点、所需 privilege、expected errno 与清理路径，未证明项不得以聚合“原子写失败”或 mock 冒充。

**Invocation 隔离、执行与清理**：
每次 invocation 使用唯一 Kubernetes namespace/Compose project、独立业务卷和测试身份；共享宿主只为 host-published ports 分配唯一值，内部容器端口固定，普通场景共享只读不可变 image digest，只有离线导入场景创建 invocation-local 临时 registry 及独立数据卷并整体销毁。teardown 前只内容寻址持久化会随环境销毁的原始附件；teardown 与资源归零检查完成后才计算 verdict、生成并签名该 invocation 唯一最终 Test Result bundle。正常 teardown 后 invocation 拥有的 Pod、Job、Service、Secret、PVC、volume、network、container、browser process、临时文件和临时 registry 数据必须机械证明归零；产品/helper 遗留为 FAILED，CI/集群故障导致无法判断清理结果为 WARNED。runner 不 fail-fast：失败后继续所有相互独立的 required scenario，依赖失败项以稳定 causal ID 记录 not_run；required 断言失败为 FAILED，因环境或前置失败不能执行为 WARNED，teardown 始终执行，失败自动重试不得隐藏先前结果。diagnostic scenario 只由已持久化的 FAILED/WARNED/not_run（未完成统一表示为 not_run）触发并携带 causal result ID；它不进入 required 分母或 suite verdict，不得改写触发结果，新 invocation 才能重验。

**证据保留、人工观察与非功能门**：
与 verdict 有关的脱敏、签名、digest 绑定 evidence bundle 与 manifest 随 tag/Release 永久保留，runner 本机原始目录在所需附件上传成功后删除；敏感原始 trace 不得成为 verdict 唯一证据，需要短期保留时先上传受限私有 CI artifact storage 并按部署方策略到期删除，判定所需事实必须先提取为脱敏结构化证据。同一确定性 verifier 从 catalog 生成交互式 observation 表单/向导，强制绑定 tag、invocation ID、scenario ID、该 invocation subject 的不可变 Release 输出 digest、观察者 OIDC 身份、开始/结束时间和封闭 typed 字段，再生成同一 DSSE/Sigstore evidence statement；PR checklist、自由文本评论或单纯 Approve 不能替代。v1 required 非功能集只包含既有契约明确的 deadline/队列/并发不变量、确定性竞争交错以及 invocation 所有资源精确归零；goroutine、FD、延迟、吞吐、CPU、内存先按固定采样点记录趋势，不阻塞发布，只有后续明确固定 workload、pinned runner、静默窗口、采样规则和数值预算后才能升级为 required gate。catalog 使用封闭 capability 词表（至少 deployment、architecture、Kubernetes exact version、Docker/Compose exact versions、browser evidence kind、privilege/fault backend、external stack），scenario 声明 required cells 与合法不适用条件；mandatory cell 缺能力在 preflight 形成 WARNED，禁止运行时自由字符串 skip，catalog 明确排除的无意义组合不进入分母。

**验证执行图与覆盖根**：
catalog scenario 是最小可独立裁决、重试、留证和清理的原子；只允许 `setup/action/assert/teardown` 四阶段，同层 `depends_on` 形成 DAG，低层 `proof_refs` 只能引用严格更低层且同一 tag qualification invocation、同一 source/catalog/contract/Release subject 闭包的结果。上层 scenario 仍必须真实执行自身 action/assert；已声明的 proof_ref 在同 tag 闭包缺失只能使上层 WARNED，空 proof_refs 表示没有下层 prerequisite、不是证明缺失；低层 FAILED 则上层 FAILED。稳定 validation root 只扫描 `*-VALIDATION-*`、`OPS-VERIFY-*`、`UI-TEST-*`，catalog 必须覆盖全部 declared root；构建门机械拒绝未覆盖/悬空/重复 ID、无实现、依赖环、跨层同级依赖、非法 proof、cell applicability 不闭合以及 Deployment Acceptance timeout 超过 freshness budget。scenario ID 一旦发布只能退休，语义变化必须新 ID，禁止旧 ID 换实现或换断言后继续复用。

**验证故障与竞争执行**：
事务竞争必须由显式 barrier、fence 和 scheduler trace 精确执行已声明 interleaving；固定 seed 状态机生成只作诊断补充，不能替代显式 required 交错。存储故障必须分别精确注入 ENOSPC、EDQUOT、EROFS、指定 fsync 失败和指定 rename 失败；v1 使用直接基于 go-fuse v2.9.0 loopback API 的最小 verification-only `quoin-faultfs`，只提供 path-scoped write/fsync/rename→errno 与 mount/unmount，不建设通用故障平台。原生 linux/arm64 原型已观察五种故障与解除注入后的恢复；最终双架构 Compose/Kubernetes required cell 仍各自重跑。上游 toda v0.2.4 无 arm64 维护或工件，不得作为执行器/fallback；未证明项不得 required。网络故障使用项目控制的 Toxiproxy/NetworkPolicy，DNS、TLS 与 HTTP/SSE/gRPC framing 使用各自 fixture，禁止所有权重叠。session、cooldown、lease、scheduler、expiry 与前端 timer 使用模块内部 Clock/Timer 接缝，确定性测试禁止 wall-clock sleep；真实 timer smoke 只留 Release Qualification。

**发布高层场景与 UI 观察**：
Release Qualification 以真实业务旅程为高层 scenario；普通 happy-path 不重复执行已经由 tag invocation Contract Gate 证明的状态机、权限和竞态，只通过严格闭包的 `proof_refs` 绑定。交互式表单、noVNC、typed reconnection、视觉/减少动态效果 observation 与真实浏览器过程仍必须在高层执行。Release Qualification 与每次 Deployment Acceptance 的 UI required observation 都固定为 3 个 browser/arch cell（Chromium amd64、Chromium arm64、branded Chrome amd64）× 4 个 viewport（320、768、1024、1440 CSS px）× 2 个 motion 模式，共 24 条；每条绑定实际 viewport、browser build、架构和 observer 身份，不乘 Compose/Kubernetes 后端，也不得向未观察的 browser/viewport 外推；Deployment Acceptance 将这 24 个 backend-independent `always` cell 冻结成 `ui_observation` items，并只允许发起 Admin Session 经 typed observation 路由提交。

**Deployment Acceptance invocation**：
每次站点验收由 Quoin 在一个 SQLite 事务创建不可变、只有 append 的 invocation manifest/items；它冻结 Release、catalog/profile/schema、部署配置、public Origin、principal、开始时间、applicable-set digest 以及服务端从权威 FK 生成的封闭 typed locator。manifest 没有可变状态机/current pointer，启动后不得增加、删除或替换 scope；current binding 漂移追加 subject-drift marker 并使 item/suite 至少 WARNED，重验必须新 invocation；若 item 已有 immutable passed result，保留该执行事实但 marker 禁止 PASSED，不追加伪造第二 result 或误造 verifier conflict。command 幂等与 result 幂等分离；同 item 同 canonical input/result 返回原结果，异结果写幂等 conflict marker 并 FAILED，禁止 first/latest/绿色项获胜。active/unclassified item 只能返回 `verification_in_progress`，不得生成最终证据。唯一 append-only finalization receipt 在 Artifact 耐久 staging 后以单 writer 事务复核全部集合 digest 并冻结唯一 Test Result；receipt 是该 invocation 所有 evidence 写入的终止栅栏，之后只允许返回原 Artifact，晚到非法提交只进通用审计。功能断言/cleanup residue/verifier conflict 为 FAILED；subject drift、环境不可用、人工取消、基础设施中断、cleanup indeterminate/not_run 为 WARNED；未知类别是 verifier invariant failure/FAILED。

**Deployment Acceptance 时间与保留**：
v1 全部 required 站点 scenario 的 `max_observation_age` 与 suite observation/snapshot span 固定不超过 8 小时且不可配置；`snapshot_at` 与保存真实提交时间的 `finalized_at` 都不得晚于 manifest deadline，所有结果 commit/received time 也必须在窗口内；未能在 deadline 前完成 Artifact staging 与 receipt commit 时不得迟到定案，重验必须新建 invocation。verdict 只比较 Quoin 持久化的 commit/received 时间，helper 主机 wall-clock 只作 provenance。报告保存真实 observation window，但不含 `validUntil` 或持续健康承诺。manifest/items/results/conflicts/receipt、canonical Test Result、typed observations、精确 helper report 与 evidence index 使用既有 `long_term` retention class 并随备份恢复；截图、trace、视频和其它大型字节仍是独立 generated Artifact，继承 Admin 共享保留设置（默认 90 天），长期报告只保存 digest、locator、retention/expiry 与正文是否已到期。外部 OIDC/WORM/签名系统可以包装报告，Quoin 不为 Deployment Acceptance 新增长期签名密钥。

**Connection Probe 与资格选择**：
Model Provider 探测清洁重构为唯一 `connection_probe` Execution Attempt；`contracts/connection-probes.yaml` 及其 Schema 是三类 action-set/version 的唯一机器权威，catalog 只引用 digest。probe 使用封闭 grant purpose，冻结 current revision/credential generation/root binding，由 Plinth supervisor 直接执行，不启动 worker/Agent/ReAct；`connection_probe_results` header 与按类型 typed child 拥有事实，不另建 Run 生命周期，旧 `model_provider_capabilities` 删除且不保留兼容层。Model Provider probe 保留完整 streaming/tool/cancel/usage/request-id/embedding 序列，并且只有 Model Provider Connection 使用显式 `qualified_probe_result_id` 作为 enable 和普通 Model/Embedding grant 的强制资格闭包，任何 pair/root/probe-contract 语义变化都 fail-closed 并要求重验。Thanos/Kubernetes 不建立长期资格 pointer，也不增加普通派发前置；其 fresh probe 主要是 invocation-scoped 站点证据，当前 root revalidation 可显式引用成功结果。Thanos Plinth probe 只证明 Plinth Tool 的固定 `vector(1)` 路径，Quoin PromQL 路径由 Config Verification 单独证明；Kubernetes probe 固定验证 discovery 与 effective namespace 内 pods get/list、events list、pods/log get 的 SelfSubjectAccessReview，不外推其它权限或 Business System mapping。

**当前配置与 Browser 站点重证**：
`config_verification_runs` 清洁泛化为唯一 `ConfigVerificationRun`：`prepublish` 继续服务草稿联合激活，`deployment_acceptance` 只绑定 current published config/current Label Contract，复用真实 PromQL/Journey/probe/Evidence/取消状态机但绝不成为发布证据或移动发布 pointer。`browser_operations` 新增 `deployment_verification` kind，冻结 manifest item、发起 Admin Session、identity revision 与 current generation marker，持有既有 identity/capacity fence；Lintel 从 marker 对应的当前物理 profile 建立 deterministic disposable clone，不声称重现历史字节。它复用同一 BrowserTunnel/noVNC、只允许发起 Session 单 active attachment 和宽限内顺序重附着，双侧拒绝 profile publish；功能 observation 与 cleanup 分开。每个 operation 在 timeout 内产生唯一 typed result，cleanup 为 `clean|residue|indeterminate`；clean 必须以 operation-scoped process/browser/tunnel/clone/temp/runtime/slot 逐项归零和 Stop fence 证明，residue 为 FAILED，indeterminate 为 WARNED，后二者保留锁并继续调和，之后清理成功不得把旧结果转绿。新 Lintel boot 必须在 Browser Ready 前 sweep deterministic clone namespace；正常在线/boot 路径只有 `new_boot_cleanup_confirmed` 可释放该 kind 的锁。

**Lintel cleanup 离线恢复**：
当未确认 Browser cleanup 与旧 Lintel token/state 损坏形成 replacement 互锁时，唯一恢复入口是 deployment operator 的 `quoin-deploy compose|helm recover-lintel`；普通 Admin/Web UI 没有 force unlock。Deployment Acceptance 的 required recovery scenario 由 helper setup 在隔离 disposable identity/slot 上注入故障并真实产生 indeterminate/residual cleanup fence 后执行，不要求健康站点已有事故，也不触碰业务 Browser Identity。helper 停止全部组件，取得封闭 `LintelRecovery` maintenance state 与状态目录应用锁，机械证明旧 workload/process 已 fence，旧存储为 `exclusively_reattached` 或 `retired`，再用同 Release Quoin 镜像的一次性 `quoin maintenance recover-lintel` 挂载权威状态并在最后事务撤销旧 token、写幂等 recovery receipt/audit。reattached 只解除 Runtime replacement fence，原 Browser locks/result 不变，新 Lintel 必须独占挂载并以 `new_boot_cleanup_confirmed` sweep 后释放；retired 以封闭 `externally_fenced_storage_retired` stop basis 释放受影响 operation/locks、把 Browser Identity 降为 `AuthenticationRequired`，但旧 verification result 永不转绿。任何 workload/storage/token/slot/app-lock 证明不足都在事务前稳定拒绝并要求完整离线恢复或重建。

## 任务终态提示

**后台任务提示**：
不建设邮件、浏览器 Push、通知数据库、铃铛收件箱、已读/未读或 per-user 未查看状态。Initial Analysis、Investigation Attempt、Inspection Run、Knowledge Import Batch 等权威对象的终态持续保留在原对象、模块列表和详情中；模块徽标只能由当前可操作权威状态即时派生，打开对象不得产生“已查看”写入。用户在线时可用非阻塞 toast 提示自己发起任务的成功、失败、取消或中断；离开后返回时直接从权威对象查询结果。正常成功的定时巡检不逐次打扰；`CompletedWithGaps`、`Failed`、`RuntimeUnavailable`、`AuthenticationRequired` 等异常结果在其所属模块持续可见，只有 Admin 能处理的用户、凭据、Runtime、备份问题只向 Admin 显示。提示不复制报告正文，也不能反向修改任务状态。
