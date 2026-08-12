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
一个 Quoin 部署所使用的唯一浏览器 Runtime，通过主动建立的运行通道接收任务，拥有独占 `lintel-state` 持久卷中的浏览器身份和长期 service token，并执行人工登录、确定性巡检和可审计探索。Trace、staging 和 Attempt 工作区可丢弃，需长期保存的结果上传 Quoin。
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
用户使用本地用户名和密码登录；User 使用稳定 ID，登录名稳定、显示名可改，只禁用不物理删除。密码按 NFC 规范化后使用 Argon2id PHC 哈希保存，接受 15–128 个 Unicode 字符，不使用字符组合规则或周期改密。登录使用统一失败信息、弱密码检查、有界失败冷却和部署级 Argon2 并发限制；不提供自助注册、邮件找回、CAPTCHA 或第一版 MFA。User 保存递增 `auth_revision`，Session 记录签发 revision 而不永久快照角色；禁用、角色变化和 Admin 重置密码在事务中递增 revision 并撤销该用户全部 Session。用户自行改密撤销其他 Session并更新当前 Session revision。所有入口读取当前 User 状态；权限写事务提交前再次核对 enabled、role 和 auth_revision，账号变更与业务写按 SQLite 提交顺序裁决。已受理后台任务不因 Session 失效而取消，并继续保留原操作者引用。

**同源 Web 会话**：
React、HTTP API、SSE 和 noVNC WebSocket 由同一 Quoin Origin 提供。浏览器只持有 32-byte 随机 opaque Session ID 的 Secure、HttpOnly、SameSite=Lax、Path=/ `__Host-quoin-session` Cookie；服务端 SQLite Session 记录承担空闲 12 小时、绝对 7 天、登出、用户禁用和强制撤销。允许同账号多个浏览器 Session，用户可查看和退出自己的其他 Session，Admin 可撤销某用户全部 Session；用户自行改密撤销除当前外的其他 Session。写请求受 Go CrossOriginProtection 保护，WebSocket 另校验 Origin，不支持带凭据的跨域 CORS。Session 登出、撤销或账号禁用时立即关闭对应 SSE 和 WebSocket，但不自动取消此前已经受理的后台任务。

**管理员离线恢复**：
首个 Admin 创建和全部 Admin 无法登录时的密码重置都只能通过停止 Quoin 后独占 SQLite 的本地命令完成；临时密码不进入参数、环境变量或日志，重置后撤销该账号全部 Session，并要求首次登录修改。

**服务身份**：
Plinth、Lintel 和 Stele 分别使用类型固定、只能访问自身 RPC 的长期 service token；TLS 只承担服务端身份和传输保护，不另建 mTLS 客户端身份。Quoin 固定只有 `plinth` 和 `lintel` 两个逻辑 Runtime slot；一次性注册令牌绑定 slot 与 credential generation，supervisor 将换得的长期 token 原子保存到权限 `0600` 的专用持久状态卷，Plinth worker 不得读取。状态卷丢失时由 Admin 替换原 slot，不创建第二个 Runtime。轮换采用“下发新 token→Runtime 持久化确认→吊销旧 token”的两阶段流程。Token 吊销时 Quoin 立即关闭对应长期控制流、浏览器流和上传流并拒绝重连；Stele 不注册为 Runtime，其 service token 由部署 Secret 文件提供。

**告警源凭据投影**：
Quoin 是逻辑告警源及其 Bearer 状态的唯一权威源，只保存高熵凭据 digest。Stele 通过自身 service token 获取版本化只读 digest 快照并仅在内存缓存；未加载快照时拒绝接收。Stele 提交 Delivery 时携带非秘密 `credential_id` 和快照版本，Quoin 在同一事务中再次检查来源启用状态、凭据有效性和归属；Delivery 与吊销事务按数据库提交顺序裁决，不使用墙钟宽限期。轮换期间一个来源最多短期同时保留新旧两个有效凭据。

**模型调用边界**：
模型供应商是 supervisor 持有的类型化外部连接。Plinth worker 通过本地 streaming ChatModel 协议提交 messages、tool schema 和非秘密生成参数；supervisor 只在内存注入 endpoint credential 并调用内部供应商。Provider API key、Authorization/Cookie、客户端私钥、Kubernetes Secret/kubeconfig、Browser profile/storage state、Quoin 根密钥、密码 hash、Session/token digest 和可逆连接密文不得进入 worker 环境、工作区、模型上下文、Evidence、Artifact 或普通日志。用户主动上传文本、外部日志和页面正文不做通用猜测式秘密扫描。

**领域写命令契约**：
所有经认证外部调用者发起的领域写命令都由客户端生成用户不可见的 `client_command_id`，按 `(principal_id, client_command_id)` 唯一，并保存命令类型、非秘密请求摘要和结果对象引用；相同 ID 与相同请求重放返回原结果，相同 ID 与不同请求返回冲突。修改当前状态或当前版本指针的命令还必须携带 `expected_row_version`；纯追加创建不强制 expected version。调度器用 `plan logical identity + scheduled_for UTC` 作为内部确定性 Run 创建键，并在同一事务绑定当时生效的业务系统配置和 Label Contract。Stele 继续使用 `relay_id`，Runtime 继续使用 `attempt_id + connection_epoch`，不强行改造成 HTTP 命令键。
_Avoid_: 每个 handler 自定义重试语义、最后写入者静默覆盖、把内部 Runtime 围栏混为客户端命令键

**审计与执行溯源**：
领域对象及其不可变版本仍是业务历史权威；另保存窄的 append-only Audit Event，只记录 actor 类型/ID、action、target 类型/ID/版本、client command/request ID、提交时间、成功或失败结果及领域记录引用，覆盖取消、重试、Undo、配置发布、知识确认/停用、用户与 Session、秘密/凭据、Runtime 替换、备份和恢复，不复制消息、Evidence、附件、Prompt 正文或秘密。每个 Execution Attempt 和低层 Model/Tool Call 保存实际供应商连接 revision/credential generation、模型 ID、Prompt/renderer/agent/tool-schema 版本或 digest、有序输入对象及 revision/digest、Quoin/Plinth/Lintel/Journey Catalog 版本、开始结束时间、usage、延迟、重试序号和结构化终止原因；输出正文继续由消息、Report、Candidate 和 Evidence 等领域记录承担。不得保存或展示隐藏思维链。结构化审计长期保留并进入备份。

## 告警与调查

**稳定身份保留**：
任何已经发布、执行过 Test Run 或被历史记录引用的稳定 ID/key 永远不能重新分配给另一个逻辑对象，包括 Business System、Logical Alert Source、Browser Identity、Connection 以及 discovery、plan、check。停用或从已发布 YAML 移除只形成 Disabled/Retired tombstone，不释放身份；以后再次出现同一 key 表示恢复原逻辑对象及其历史，新业务含义必须使用新 key。显示名称可以修改或复用。只有从未发布、从未运行且从未被引用的草稿/staging 对象可以物理清理。
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
围绕一个运维问题展开的一条对话线程，可以引用一个或多个告警发生、初步分析及相关证据，并形成诊断。
_Avoid_: 初步分析、事故、跨问题聊天

**撤回消息（Withdrawn Message）**：
用户只能 Undo 当前有效对话的最新用户回合。每个 Investigation 只有一个当前有效 head，同时最多一个 active 模型 Attempt；发送消息携带 client command ID 与 expected head，Quoin 在一个事务中追加消息并创建 Attempt，重复命令返回原结果，head 已变化则冲突。Undo 后，该用户消息及基于它产生的助手回复、工具调用、Evidence 引用和知识草稿成为只读非活动分支，保留审计但不进入后续上下文；依赖该回合的 active Attempt 立即取消，迟到结果只留审计。新消息从撤回前的有效 head 继续。第一版不支持任意历史撤回、分支切换/合并、原地编辑或重新生成分支。
_Avoid_: 删除消息、只隐藏输入而保留其结论、并发主线、通用分支管理器

**文本附件（Text Attachment）**：
用户附加到一条调查消息中的纯文本 Source Material，保留原始文件名、内容、大小、上传者和时间，并与该消息一同进入调查历史。每条消息最多一个，正文必须是有效 UTF-8、不得含 NUL、默认上限 10 MiB；不依赖扩展名判断内容。原始文件名只作为审计元数据，UI 显示转义后的 basename；Attempt 工作区固定写入 `attachments/<attachment-id>.txt` 并用 manifest 映射，不把用户文件名拼入路径。其精确字节和来源可追溯，但正文中的主张不自动成为已验证 Evidence。
_Avoid_: 任意文件、图片、压缩包、用户控制的工作区路径、确定性工具证据

**诊断（Diagnosis）**：
某个不可变模型输出基于已有证据形成的解释与结论；它可以是 Initial Analysis 的成功输出、某个 Inspection Report 版本，或用户明确选择的 Investigation assistant message/Attempt 输出。系统不维护会被后续回复覆盖的 Investigation 级“当前诊断”；诊断在被人确认前不代表已验证事实。
_Avoid_: 事实、告警、整个调查、可覆盖的当前结论

## 工作台投影

**三栏工作台**：
第一栏是默认只显示图标的全局导航，hover/focus 时解释用途；第二栏是当前模块的对象列表、筛选和主要操作；第三栏是选中对象的详情或工作区。登录后直接进入 `/alerts`，不建设独立仪表盘；全局模块为告警、调查、巡检、业务系统、知识和管理，Runtime 离线、登录失效和备份故障通过导航徽标及告警页状态区暴露。窄屏时第一栏变抽屉，列表与工作区分别全屏显示，返回时恢复筛选、滚动和选中状态；复杂 noVNC 登录提示优先使用桌面。

**首次设置投影**：
空告警页根据实际状态说明缺少什么并提供“完成初始设置”入口；管理模块提供可跳过、依赖驱动的设置清单，不建设阻塞使用的线性 Wizard。清单从权威状态派生并分别展示模型供应商、Thanos/Kubernetes Connection、Plinth/Lintel、Label Contract、Business System 配置、Browser Identity 登录、Stele 告警源和备份目标的就绪状态、依赖及直接修复入口；告警接入可用与巡检可用分别计算，不要求一次配齐全部能力。Admin 可直接处理，Operator 只看到需要 Admin 完成的项目；完成后清单折叠但仍可从管理页查看。
_Avoid_: 空页面、强制线性向导、把内部对象依赖留给用户推导、所有能力全配齐才允许使用

**告警列表与详情**：
告警模块第二栏默认显示 Firing Occurrence，并按真正状态转换的 Quoin commit 时间倒序；Resolved 进入历史筛选。第三栏确定性展示状态、来源时间与接收时间、业务系统、完整 labels/annotations、Delivery 时间线、按业务系统与 identity labels 精确匹配的 ObservedResource、机械候选关联及原因、Initial Analysis 历史和引用该 Occurrence 的 Investigation；未匹配资源时明确显示“未匹配到观测资源”。severity 只是原样展示和筛选的普通 label，Quoin 不定义顺序，模型不推断。普通列表不显示 IdentityConflict 等接入问题。详情 URL 为 `/alerts/:occurrence`；Occurrence resolved 后当前 URL 仍可查看并明确显示已恢复。

**实时投影**：
影响告警列表的事务在同一 SQLite 事务中产生单调递增 `alert_change_seq`；HTTP 快照返回 `snapshot_seq`，每个 Occurrence 返回 `row_version`。客户端首次建立 SSE 时携带 `after=snapshot_seq`，重连使用 `Last-Event-ID`；Quoin 回放其后的有界派生变更。SSE 可重复投递，客户端按 sequence 与 row version 幂等应用；游标过期时返回 `resync_required` 并重新读取完整快照。事件只携带 Occurrence ID、变化类型和版本，选中详情发现版本变化后重新读取。Resolved 从 Firing 列表移除，但已打开 URL 继续显示并标记已恢复。新告警非阻塞提示不打断当前详情；该变更流可丢弃、可重建，不是告警历史权威源。

**调查与巡检工作区**：
调查模块第二栏显示 Investigation 列表，第三栏使用 assistant-ui 对话工作区，URL 为 `/investigations/:investigation`；巡检运行 URL 为 `/inspections/runs/:run`。进行中的初步分析、调查和巡检立即显示已受理与真实执行阶段，用户可离开页面，完成或失败后在列表和详情持续可见。任务创建命令先在 Quoin 事务中保存业务对象和 Attempt，SSE 只是观察通道，断线不取消任务；任务变化使用单调 sequence 与对象 row version，进入页面先读 HTTP 快照再建立 SSE，重连有界回放，游标过期 `resync_required`。事件只传状态、工具阶段和版本，token delta/高频动画不持久化。最终消息、Report 或 Candidate 必须先原子持久化，任务随后才能 Succeeded。Tool Call 执行前创建记录并以真实时间戳单调推进，返回页面从 Attempt 快照恢复完整时间线；不伪造百分比、不展示或声称保存隐藏思维。noVNC 瞬断进入短暂 `AwaitingReconnect`，同一 Session 可重附着，宽限期后关闭 BrowserSession 释放身份锁，且不自动发布 profile generation。

## 资源与巡检

**标签契约（Label Contract）**：
部署级、版本化的 Prometheus label 语义契约。第一版只统一配置业务系统归属 label 名；告警归属、资源发现、巡检 PromQL 校验、资源关联和页面筛选共同引用同一个契约版本。资源身份 labels 仍由每个资源发现声明显式配置。管理页展示当前/草稿版本并提供验证和激活入口；零启用业务系统时首个契约通过静态校验即可直接激活。有启用系统时，页面逐系统展示兼容状态、目标配置版本、Test Run 结果和阻塞原因，只有全部系统都准备并验证兼容版本后，才与这些版本在一个事务中原子激活；任一失败则全部继续使用旧版本并显示原因。已开始的 Run 继续绑定旧版本，契约变更不重写历史 label 快照或归属。
_Avoid_: 代码写死 label、每业务系统重复定义归属 label、CMDB Schema、多个当前契约、部分切换、空部署循环依赖

**业务系统（Business System）**：
由用户在一份权威 YAML 中明确配置的稳定运维范围，根节点包含稳定 key、显示名和 enabled 状态，生命周期只有 `Enabled | Disabled`。首次上传静态合法且不存在的 key 时，一个事务创建 Disabled Business System 与第一份不可变草稿；创建草稿不表示启用或发布，Test Run 可在 Disabled 状态执行，只有显式发布才切换当前配置及 enabled 状态。后续停用/启用也通过发布 YAML 版本完成，管理页不提供与 YAML 竞争的创建或元数据编辑表单。有历史引用后不物理删除。停用后停止新的资源刷新、定时巡检和普通人工 Inspection Run，等待任务 `BusinessSystemDisabled`，已接受只读 Run 可完成并允许取消；告警继续接收并显示系统已停用，历史保留。重新启用前重验发布版本，启用后从未来周期继续，不补跑。
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
每个业务系统一份完整 YAML，根节点显式包含稳定业务系统 key/name/enabled，并以一个版本原子包含资源发现和全部巡检计划；每个系统只有一个当前已发布版本。每次上传创建不可变草稿并记录基线发布版本，Test Run 精确绑定草稿。发布命令携带 version ID 与 expected current published version ID，事务中重验并切换；不匹配则冲突。Label Contract 联合激活仍原子切换契约和全部兼容版本。根节点统一提供 IANA 时区和资源刷新 interval，discovery、plan、check 使用稳定 key。YAML 只接受一个 UTF-8 文档，拒绝重复 key、anchor/alias、merge、自定义 tag、非字符串字段名、第二文档和尾随内容，并设输入/AST/深度上限；上传时只解析一次，保存原文、parser/schema 版本、类型结构和 digest，运行只使用类型结构。管理页可按当前 Schema、Label Contract 与 Journey Catalog 下载起始模板，任意草稿/发布版本可导出；校验错误逐项展示字段路径、原因和可操作修复提示。未知字段、重复 key 或不兼容契约必须拒绝。
_Avoid_: 页面隐式归属、共享可覆盖草稿、资源与计划分别发布、运行时重新解析、自动热加载、表单与 YAML 双权威、要求用户先读内部 Schema

**浏览器身份（Browser Identity）**：
每个业务系统恰好一个共享持久化巡检身份，包含命名身份、Admin 管理的起始 URL、当前 profile generation/状态和版本化 authentication probe。Admin 在 Business System 详情创建/配置身份；Admin 与 Operator 都可从该详情进入 `/business-systems/:system/browser-login` 实时 noVNC 页面，完成登录后必须显式发布 generation。Inspection 的 `AuthenticationRequired`、徽标和失败详情直达该页面，登录后可返回原 Run；不建设 Lintel 技术控制台。Journey 从起始 URL/origin 出发，YAML 只给类型化参数或相对路径，明确 SSO 跨 origin 由版本化 Journey 实现；模型 Exploration 导航范围沿用已定边界。同一身份的登录、巡检、探索独占串行。Lintel 在 `lintel-state` 卷维护可写 profile，正常 Cookie/refresh token/IndexedDB/localStorage 更新持续保留；人工发布形成新 generation，自动变化不制造版本。Quoin 只存 generation/状态/时间/运行引用，备份不含 profile；卷丢失、损坏或升级不兼容时变 `AuthenticationRequired`。
_Avoid_: 用户密码、任意 YAML 绝对 URL、Playwright 脚本、Runtime 管理页登录、每次运行回滚状态、把 profile 当 Artifact

**Journey Catalog**：
由 Lintel 中版本化 Playwright Journey 及其参数 Schema 的同一机器可验证来源生成，并在构建时同时嵌入同版本 Quoin 和 Lintel。Journey ID 是稳定行为契约；兼容修复可保持 ID，参数或业务语义的破坏性变化必须使用新 ID。Quoin 可在 Lintel 离线时静态校验 YAML，Run 保存实际 catalog digest 和组件版本。
_Avoid_: 任意 Journey 字符串、Quoin 分发用户代码、旧配置绑定旧 Runtime

**浏览器操作记录**：
三类浏览器操作采用不同记录边界：人工登录只保存操作者、业务系统、起止时间、结果和新 generation，不记录键盘、不截图、不生成 trace；模型 Browser Exploration 保存结构化动作与结果日志，并为每个 Attempt 保存一份 trace，只把模型引用或失败诊断需要的截图单独保存为 Artifact，不重复保存完整 HTML、全量网络响应和逐动作截图；确定性 Journey 保存结构化步骤结果，失败时保留 trace，成功时只保留 Journey 明确产出或报告引用的截图。必要 Artifact 上传失败时必须标记证据不完整，不得显示为完整成功。
_Avoid_: 三种操作共用全量录制、录制人工登录秘密、同一页面多份重复正文

**巡检运行（Inspection Run）**：
一个已发布计划在调度时刻或人工触发下产生的不可变机械采证记录。权威状态只描述采证：`Queued | WaitingForCapacity | Running | Completed | CompletedWithGaps | Failed | Cancelled | Interrupted | SkippedOverlap`。Completed 表示全部检查形成完整 Evidence，不表示系统健康；CompletedWithGaps 表示采证已结束并冻结结果，但存在 RuntimeUnavailable、AuthenticationRequired、部分响应或检查失败等缺口，即使没有成功检查，只要完整记录每项缺口仍属此状态；Failed 只表示无法形成并提交有效冻结结果集合。模型分析 Attempt/Report 使用独立状态，分析失败不回写 Run，页面可显示“采证部分完成/分析失败”。同一计划不并发：重叠定时周期 SkippedOverlap且不补跑，人工触发展示当前 active Run。定时创建时 Runtime 离线则相应检查 RuntimeUnavailable、其他继续；在线无容量则 WaitingForCapacity，队列只在 Quoin，真正采证时生成 evidence_at。重试分析引用同一 Run，重新采证创建新 Run/evidence_at并以 rerun_of 引用旧 Run。
_Avoid_: 巡检计划、巡检报告、混合采证/分析状态、Succeeded=健康、离线补跑、Runtime 队列、跨时间追加

**执行尝试（Execution Attempt）**：
Plinth 或 Lintel 对同一个任务或 Run 的一次底层执行。Quoin 派发前持久化 attempt ID；Runtime 有 boot ID、递增 connection epoch、明确接受和有限 lease。lease 内同 boot 重连上报 active Attempt 调和而不重派；新 boot、lease 到期、身份吊销、崩溃或替换使 Attempt `Interrupted`。结果按 attempt ID 幂等，旧 epoch 迟到结果只审计。用户取消携带 command ID 与 expected version，Quoin 先事务提交 cancellation fence；Queued/WaitingForCapacity 直接 `Cancelled`，Running 先 `Cancelling`，Runtime 确认或取消 lease 到期后 Cancelled。成功与取消按 SQLite 提交顺序裁决：成功先提交则取消返回已完成；取消先提交则迟到结果不产生有效消息、Report 或 Candidate。取消前已提交 Evidence/Tool/Artifact 保留为部分结果；取消 Run 停止未开始及运行子 Attempt但不删除已完成检查，页面/SSE断线/登出不隐式取消。第一版不设 Agent Attempt 总时长、调用数或产物限制；每次模型/API 调用有部署内部有限 deadline。幂等只读 API 对瞬态错误有界重试并记录物理尝试；模型只在明确可重试且未收到输出时自动重试，不切换模型/供应商。部分 token 后失败不得成为有效输出，Attempt `Failed` 并保存 Timeout/RateLimited/ProviderUnavailable/InvalidResponse/ToolError/ArtifactCommitFailed 等原因。Tool 失败可成为 Evidence 缺口，模型失败不生成成功结果。每次 Attempt 用干净工作区，成功前所有引用 Artifact 必须上传校验提交。
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
结构化告警、调查、消息、诊断、报告、反馈、知识、Text Attachment 和 Knowledge Import Batch 原文长期保留。截图、Playwright trace 与大型工具响应正文等生成型大 Artifact 默认保留 90 天，使用一个部署级天数配置；到期后保留元数据、SHA-256、来源、时间和“正文已过期”状态。Raw Playwright trace 固定为敏感诊断 Artifact：官方 Trace Viewer 可查看完整 DOM snapshot、console、request/response headers 与 body，因此 trace 不进入模型上下文、普通附件、FTS 或通用 read/grep；Operator 只看结构化动作日志、错误和显式截图，raw trace 仅 Admin 经审计下载。实现验收必须在锁定版本注入 sentinel Cookie、Authorization header、DOM token 和响应内容，生成真实 trace 检查 ZIP。撤回消息、停用系统或停止复用知识不删除来源历史。备份保留 30 份是独立规则。

**一致备份**：
自动备份通过独立空闲 SQLite 连接执行 `VACUUM INTO` 生成单文件一致快照，再从快照枚举精确 Artifact hash 集合；Artifact GC 与复制阶段互斥。备份复制校验快照引用的 Artifact，最后以“临时文件写入并 `fsync` → 原子 rename → rename 后 `fsync` 父目录”的顺序耐久发布 DB/Artifact SHA-256 manifest；任一引用缺失或目录同步失败整次失败，新备份校验成功后才清理超出 30 份旧备份。FTS5 与 embedding 不是恢复业务事实所必需的权威数据；采用同库布局时，FTS5 shadow tables 与 embedding BLOB 会随 `VACUUM INTO` 物理进入快照，恢复后可校验、丢弃并重建。Attempt 工作区、临时文件和 Browser profile 不进入备份。Admin 可浏览、显式下载和立即触发备份，下载审计；恢复只由拥有 PVC/数据目录和根密钥 Secret 权限的部署操作者在 Quoin 停机时执行，Web Admin Session 不是恢复权限。备份目录只允许 Quoin UID 与部署操作者访问，Artifact 路径仅由 hash 推导。恢复后保持维护状态并暂停调度：清除全部 Web Session；通过 TTY 选择一个恢复 Admin 并设临时密码，其他用户待 Admin 重新确认启用；Runtime、Stele service token 和告警源 Bearer 全部失效并重新注册/轮换；Connection 密文保留但标记 `RevalidationRequired`；Browser Identity 变 `AuthenticationRequired`。Admin 检查后才退出恢复维护状态。

**单组件拓扑**：
第一版固定 Quoin、Plinth、Lintel、Stele 各一个 active replica，不提供 replicas 或 HPA 配置。Helm 与 Compose 均采用零重叠替换；Kubernetes Deployment 使用 `Recreate`。Quoin 持有 `quoin-data` 进程锁，Plinth/Lintel 持有各自状态目录锁，第二实例无法取得锁时启动失败。SQLite WAL 数据卷只支持本地或块存储，不支持 NFS/SMB；CSI 支持时优先 `ReadWriteOncePod`，否则使用 `ReadWriteOnce + Recreate + 应用锁`。Stele 无 PVC。

**协调升级**：
四个 OCI 镜像由一个 Release 版本统一决定，不承诺 N/N-1 在线滚动兼容。升级时 Quoin 进入维护状态，停止新人工任务和新调度，等待活跃任务/实时浏览器会话结束或由 Admin 明确取消；停止 Stele 后创建并完整验证升级前备份，再停止 Plinth、Lintel 和旧 Quoin。新 Quoin 独占数据目录执行前向 migration，完成前不 Ready，随后启动同版本 Plinth、Lintel、Stele；版本不匹配时组件不 Ready、不领任务、不接收正式告警。维护期间错过调度记录 RuntimeUnavailable 且不补跑。新版本接受新写入前可以恢复备份并回旧版本；接受新写入后禁止只回滚镜像，必须显式恢复升级前备份。

**健康语义**：
Quoin 取得数据目录锁并完成 migration 前不 Ready；Plinth/Lintel 只有在 token、版本握手和控制流被 Quoin 接受后 Ready；Stele 只有版本握手及告警源 digest 快照加载成功后 Ready。Quoin 暂时离线不得使 Runtime liveness 失败并触发循环重启；Compose `depends_on` 只改善启动顺序，不能替代组件自身重连和 Ready 判断。

## 通知投影

**后台任务提示**：
不建设邮件、浏览器 Push 或独立通知中心。Initial Analysis、Investigation Attempt、Inspection Run、Knowledge Import Batch 等权威对象的终态派生 UI 提示：用户在线时以非阻塞 toast 提示自己发起任务的成功、失败、取消或中断；离开后使用服务端 per-user 未查看标记在相关模块显示徽标，打开对象即标记已查看而不删除历史。正常成功的定时巡检不逐次打扰；`CompletedWithGaps`、`Failed`、`RuntimeUnavailable`、`AuthenticationRequired` 等异常定时结果向 Operator/Admin 显示，只有 Admin 能处理的用户、凭据、Runtime、备份问题只向 Admin 显示。通知不复制报告正文，也不能反向修改任务状态。
