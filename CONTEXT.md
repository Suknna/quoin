# Quoin 运维

Quoin 帮助内部运维团队基于监控证据调查告警、执行巡检并沉淀经过确认的运维知识。它面向单一组织，不承担 CMDB 或事故管理系统的职责。

> **浏览器与 Kubernetes 插件移除（2026-09）：** 受控浏览器业务（Lintel 运行时、Browser Identity/Operation、journey）与 kubernetes 插件已从代码与契约中彻底移除；历史冻结 attempt 与旧库中的浏览器数据不再保证可解析。本文其余涉及浏览器/Kubernetes 插件的条款仅作历史解读。

> **统一 mTLS 组件认证（2026-09，ADR-0009）：** 内部组件认证收敛为单一 PKI：部署 CA（runtime-ca）签发 Stele/Plinth 客户端证书（CN=stele / CN=plinth），quoin:8443 强制 mTLS 并按 CN 授权服务。注册制（一次性注册令牌、长期 Bearer、两阶段轮换、`runtime_slots`/`runtime_credentials` 权威）与 Stele service token 整体退役；组件启动即认证，无注册步骤。本文其余涉及注册/token 轮换的条款仅作历史解读，现行权威见 [ADR-0009](docs/adr/0009-unified-mtls-component-auth.md)。
> **三组件职责规范与插件体系 v2（2026-09-20，ADR-0011，实施已落地）：** [ADR-0011](docs/adr/0011-component-responsibility-and-plugin-v2.md) 重写了三组件职责——Stele 升级为外部网关（入向队列化立即 ACK、出向凭证注入/限流/执行，own 本地 SQLite 运行态），Plinth 收窄为对插件体系零感知的推理沙箱，Quoin 成为插件注册中心与工具编排中枢（业务库唯一写者、对外部平台凭据只写不读）。下文 Quoin/Plinth/Stele 三个角色条目已按新职责改写；旧「Stele 同步落库后 ACK」条款被显式取代。

## 契约术语

**Proto 权威契约**：
Quoin、Plinth 和 Stele 共同遵循的统一组件通信契约，由项目权威 protobuf 文件集合定义；它不同于组件各自的发布版本。
_Avoid_: probe、连接探测、组件发布版本、各接口独立兼容版本

**Proto 契约指纹**：
完整 Proto 权威契约的确定性内容标识，用于判定组件所依据的契约是否一致，而不是判断两个发布版本是否相同。
_Avoid_: 发布版本号、兼容性版本号、单个接口版本

## 运行角色

**Quoin**：
运维系统的控制面，拥有用户配置、任务状态、历史、反馈和知识等权威记录。
_Avoid_: Agent Runtime

**Plinth**：
一个 Quoin 部署所使用的唯一推理沙箱（Agent Runtime），对插件体系零感知（编译级：不 import 插件与 Quoin 包）。协议只有「任务 + 工具目录 → 文本或 tool_call」：通过主动建立的运行通道领取任务，在每次 Execution Attempt 的全新可丢弃工作区中调用模型与沙箱本地工具（bash/read/write/grep）；quoin_routed 工具（外部平台与 artifact 工具）由 Quoin 编排执行并经 ExternalToolResult 帧推回，supervisor 只转发。输入只从 Quoin 当前有效历史和 Artifact 重建；只有显式返回并提交到 Quoin 的消息、Evidence 和 Artifact 可以跨 Attempt 存活。
_Avoid_: Quoin、跨轮持久工作区、第二份调查历史、插件执行宿主、外部平台直连（模型供应商除外）

**Stele**：
独立的外部网关，所有外部平台交互的唯一出入口（ADR-0011）。入向：`POST /webhook/{source}` 按来源找到 EventSource 插件做协议解析，归一化 Event 在本地 SQLite 事务入队后立即 ACK（202）；可靠性由队列指数退避重试与死信承担，外部重试压力不再传导给 Quoin。出向：经长期网关流接收 Quoin 下发的平台调用，解析连接材料（按需 Acquire 并缓存）、按连接限流、执行传输并回传原始响应。它只做传输与协议转换，永远不碰业务逻辑——不知道消息该派给哪个 Plinth（Quoin 的事），不知道工具该不该调（Quoin 的权限判断）；它的判断只有：签名对不对、配额超没超、平台通没通。Own 本地运行态 SQLite：事件队列、去重键、限流计数、token 缓存、死信与预留的拉取游标；不拥有任何权威业务状态。
_Avoid_: Quoin、告警存储、Agent Runtime、业务语义判断、直接落业务库

> **认证目标更新（2026-09-20，ADR-0010，实施已落地）：** [ADR-0010](docs/adr/0010-oidc-auth-and-local-emergency.md) 与重写后的[统一认证设计](docs/authentication-design.md) 接管下文认证目标：登录入口为配置驱动的 AuthProvider 注册表——OIDC（授权码+PKCE+state/nonce，token 用后即弃，identities(issuer,subject) 唯一键 + 开放 JIT，角色恒 operator）是日常通道；本地密码登录降级为单步应急通道（IdP 故障维护用），平台内 OTP/两段流程/投递配置整体退役，双因子职责移交 IdP。初始管理员密码首启随机生成写入 dataDirectory 的 0600 文件（24 小时未改密作废，`quoin admin recover` 再武装），公开默认 admin/admin 废除。角色模型冻结为唯一内置本地 admin + 全员 operator；登录页由公开 GET /api/v1/auth/config 投影配置驱动渲染，前端 /login 为规范入口。审计自动记录与操作关联目标仍由 [ADR-0006](docs/adr/0006-automatic-audit-and-operation-correlation.md) 确认，见[审计设计](docs/audit-design.md)。

## 人类角色

**Operator**：
普通用户只使用告警与 AI SRE（对话和知识）：可在这些上下文中使用已获授权的业务信息和工具，但不得直接管理接入、业务系统、巡检配置、凭据、用户、Runtime 或任何配置发布；没有人工登录例外。服务端必须逐请求强制此边界，前端隐藏导航不能代替授权。
_Avoid_: 只读观察者、系统管理员、按业务系统隔离的角色、人工登录例外

**Admin**：
管理员负责全部接入、凭据、巡检配置、业务视图、平台维护及用户/角色/Session、模型供应商、Runtime、逻辑告警源凭据、备份和安全设置；同时仍可使用业务功能，包括告警与 AI SRE。业务声明管理与配置发布已随 ADR 0004 退出主线，历史记录只读保留；活动的全局标签契约已退役，业务范围由来源接入与业务视图组织。系统始终必须保留至少一个有效 Admin。
_Avoid_: 超级租户、外部身份提供方、日常任务专属角色

## 认证与服务身份

**本地账号认证**：
登录为配置驱动的 AuthProvider 注册表（ADR-0010）：OIDC 是日常通道（授权码+PKCE+state/nonce，token 用后即弃，identities(issuer,subject) 唯一键，首登开放 JIT 建档、角色恒 operator，email 仅入 user_contacts 展示、永不参与匹配）；本地密码登录是单步应急通道——密码校验与完整会话签发同事务完成，成功/失败各落一行 auth.login.local 审计，受限会话（password_change_required）除 me/password/logout 外一律 403，首登强制改密复用该机制。用户使用稳定 ID，登录名稳定、显示名可改，只禁用不物理删除。密码按 NFC 规范化后使用 Argon2id PHC 哈希保存，接受 15–128 个 Unicode 字符，不使用字符组合规则或周期改密；创建、修改、临时密码转正式密码和离线重置时使用随 Quoin Release 固定 SecLists `100k-most-used-passwords-NCSC.txt` 上游 commit 与 checksum、运行期不联网的常见/已泄漏密码 blocklist，并追加产品名、用户名和显示名等上下文值；登录时不再做 blocklist 检查。登录统一返回失败信息；单 Quoin 进程在有界内存中按规范化用户名执行 15 分钟内失败 5 次后冷却 15 分钟，不存在用户走同一路径，进程重启清零可接受；同一进程还限制全局登录速率与 Argon2 并发。User 保存递增 `auth_revision`，Session 记录签发 revision 而不永久快照角色；禁用和 Admin 重置密码在事务中递增 revision 并撤销该用户全部 Session。用户自行改密撤销其他 Session并更新当前 Session revision，同时清除初始密码期限并置 initialized。所有入口读取当前 User 状态；权限写事务提交前再次核对 enabled、role 和 auth_revision。已受理后台任务不因 Session 失效而取消，并继续保留原操作者引用。

**同源 Web 会话**：
React、HTTP API 和 SSE 由同一 Quoin Origin 提供。浏览器只持有 32-byte 随机 opaque Session ID 的 Secure、HttpOnly、SameSite=Lax、Path=/ `__Host-quoin-session` Cookie；服务端 SQLite Session 记录承担空闲 12 小时、绝对 7 天、登出、用户禁用和强制撤销。允许同账号多个浏览器 Session，用户可查看和退出自己的其他 Session，Admin 可撤销某用户全部 Session；用户自行改密撤销除当前外的其他 Session。写请求受 Go CrossOriginProtection 保护；携带 Session Cookie 的非安全方法若同时缺少 `Sec-Fetch-Site` 与 `Origin` 则拒绝，存在 `Origin` 时必须精确等于公共 Origin；`POST /auth/login` 另在认证前执行同源门：有 `Origin` 时必须精确相等，没有 `Origin` 时只接受 `Sec-Fetch-Site: same-origin`，两者都缺失以及 `same-site|cross-site` 均拒绝；WebSocket 另校验 Origin，不支持带凭据的跨域 CORS，也不提供 Cookie CLI 兼容入口。Quoin 负责与应用内容相关的 CSP、`frame-ancestors`、`nosniff`、Referrer Policy、敏感响应 `no-store` 与登出 `Clear-Site-Data`，实际 TLS 终止层独占 HSTS。Session 登出、撤销或账号禁用时立即关闭对应 SSE 和 WebSocket，但不自动取消此前已经受理的后台任务。

**管理员离线恢复**：
唯一内置 Admin 的找回只能通过停止长期 Quoin 后独占数据库的 `quoin admin recover` 完成，凭据只经 attached TTY：`--mode password` 由操作者在 TTY 输入新的临时密码，`--mode factors` 额外重置全部收码因素并生成只在 TTY 打印一次的临时密码。临时密码不进入参数、环境变量、Secret、history、日志或数据库明文，不设单独有效期，再次执行恢复会取代先前的临时密码。恢复后服务重启，管理员以该临时密码正常登录，进入与首次安装完全一致的统一初始化流程（设置正式密码并验证收码渠道），完成前不建立工作台会话；不恢复默认密码、不创建第二管理员、不提供网络 bootstrap 或邮件找回。

**服务身份**：
Plinth 和 Stele 的组件身份是部署 CA（runtime-ca）签发的客户端证书（CN=plinth / CN=stele），内部 gRPC 全部 mTLS：quoin:8443 以 `RequireAndVerifyClientCert` 校验客户端证书并按已验证链叶证书 CN 授权服务——RuntimeControl/ArtifactService 仅 CN=plinth，SteleRelay 仅 CN=stele（ADR-0009）。证书由部署方用仓库脚本生成（或等价 PKI 流程）、经部署 Secret 只读挂载，与 CA 同寿命；轮换经 `scripts/generate-deployment-secrets.sh --issue-client-certs --force` 重新签发后更新 Secret 并重启组件，属显式运维操作。不存在注册流程、一次性令牌或持久凭据状态：Plinth 状态卷丢失后以同一证书自动重连；Plinth worker 不接触证书私钥之外的部署秘密。持有 CA 私钥即可签发任意组件身份，该私钥只存在于部署 secrets 目录（Compose）或 Kubernetes Secret（K8s 部署），与数据卷同等级保管；普通 SQLite 备份恢复不改变这些部署身份。

**告警源凭据投影**：
Quoin 是逻辑告警源及其 Bearer 状态的唯一权威源，只保存高熵凭据 digest。Stele 以自身客户端证书经 mTLS 认证后获取版本化只读 digest 快照并仅在内存缓存；未加载快照时拒绝接收。Stele 提交 Delivery 时携带非秘密 `credential_id` 和快照版本，Quoin 在同一事务中再次检查来源启用状态、凭据有效性和归属；Delivery 与吊销事务按数据库提交顺序裁决，不使用墙钟宽限期。轮换期间一个来源最多同时保留新旧两个有效凭据；新值首次成功使用后进入 Pending Retirement，由 Admin 显式吊销旧值，不设自动 TTL，并持续显示与审计未收口状态。

> **告警归一化层（2026-09-20，ADR-0012）：** [ADR-0012](docs/adr/0012-alert-normalization-layer.md) 在 intake 事务内建立「归一化（插件第三能力 AlertNormalizer：severity 四级词表+序数/title/annotations 冻结）→ 富化（enrichment_rules 静态 mapping，快照冻结溯源）→ 去重（(source,fingerprint,starts_at) 实例幂等，不变）→ 关联（视图多命中全记录）」四段流水线；告警语义成为一等列，视图从归属权威重定位为关联维度；`alerts_recent` 平台只读工具（读 Quoin 自有数据=平台工具，读外部平台=插件工具的归属判据）供给三条 AI 路径的相关告警上下文；business_systems/label_contracts 休眠域整域退役，AI 业务上下文改由富化快照+关联供给。ADR-0008 双归属读模型整体取代。

> **外部平台凭据边界（2026-09-20，ADR-0011）：** 业务平台连接（connections 域）凭据延续 AES-GCM 信封存储，但供给路径改为 Stele 按需获取：Stele 经 `SteleRelay.AcquireConnectionCredential`（CN=stele、mTLS）拉取连接材料并在本地缓存，动态凭证（token 刷新等）生命周期管理归 Stele；Quoin 对外部平台凭据**只写不读**（解密仅为按需投递，自身不使用）。模型供应商凭据不变，仍由 Plinth supervisor 经 FetchCredentialGrant 按 attempt 获取。

**一次性秘密 Reveal**：
创建或轮换告警 Bearer 等一次性秘密时（Runtime 注册 token 已随 ADR-0009 退役），命令响应只返回绑定发起 Session 的 reveal handle。handle 固定存活 60 秒、仅内存保存、最多成功消费一次；消费时必须是同一仍有效且当前仍为 Admin 的 Session。同一 Session 以同一 `client_command_id` 重放创建命令时，若内存 handle 仍有效且未消费则返回同一个 handle；过期、Session 改变或进程重启后只返回 `revealAvailable=false`，不创建新凭据。reveal 一旦在服务端消费，即使响应丢失也不能再次读取，只能创建替代 generation。登出、Session 撤销、账号禁用或降级以及 Quoin 重启都立即使关联 handle 失效；handle 与原始秘密都不进入数据库、审计、URL、toast、日志、模型上下文或命令持久结果。

**根密钥与可逆秘密**：
部署提供单一 32-byte 根密钥，只通过部署 Secret（K8s 部署为 Kubernetes Secret，Compose 为只读文件）挂载，不进入 SQLite、备份或日志。连接凭据使用 AES-256-GCM envelope 保存，envelope 携带格式版本、随机 nonce、root-key binding revision 与 ciphertext/tag，AAD 绑定 Credential Generation 的稳定身份和类型；SQLite 另保存不含秘密的 AEAD verifier。Quoin 启动只加载一次根密钥；文件缺失、长度错误或 verifier 不匹配时保持 Not Ready，只提供无秘密健康诊断。确认密钥永久丢失后，部署操作者必须停止 Quoin 并独占 SQLite 执行离线 rebind：绑定新密钥、递增 binding revision、把全部 Connection 置为不可派发且需重新录入，并直接进入 `RootKeyRebind` 维护状态；旧密文只保留历史、不再尝试解密。运行期单条 envelope 认证失败只隔离对应 Connection 并审计，不回退为空值、明文或旧 revision。v1 不提供多 key keyring、在线重加密或外部 Vault/KMS 集成。

**模型调用边界**：
模型供应商是 supervisor 持有的类型化外部连接。配置时先真实请求 OpenAI-compatible `/v1/models`；返回多个 ID 时由 Admin 明确选择 Chat 与 Embedding model，不自动取第一项，接口缺失、空列表或目标未列出时允许手工填写 model ID。上下文容量等未声明且无法可靠实测的元数据允许手工补充，缺少能力声明不阻止保存为“尚未验证”；流式输出、native/multi Tool Call、取消与 Embedding dimension 等可实测能力仍由最小真实请求验证，成功后才能启用普通 Agent 任务，失败保留配置以及结构化非秘密错误码和允许字段，不复制供应商原始响应。Plinth worker 通过本地 framed protobuf ChatModel 协议提交 messages、固定 tool schema 与可复核的非秘密请求摘要；模型 ID、输出预算和 generation 参数由 Quoin 当前 capability/grant 与固定 Agent 契约决定，worker/模型不得选择或覆盖。supervisor 先经 Quoin 持久化物理 Model Call，再只在内存注入 endpoint credential 并调用内部供应商。Provider API key、Authorization/Cookie、客户端私钥、Quoin 根密钥、密码 hash、Session/token digest 和可逆连接密文不得进入 worker 环境、工作区、模型上下文、Evidence、Artifact 或普通日志。用户主动上传文本、外部日志和页面正文不做通用猜测式秘密扫描。模型 Provider revision 在启用前必须由 Plinth supervisor 真实执行 Chat streaming、native/multi Tool Call、取消、usage/request ID 与 Embedding/dimension 探测；Embedding 和 provider probe 不启动 Agent worker。Provider SDK 隐式重试关闭；同一逻辑调用的自动物理重试只允许无任何响应的 `timeout|rate_limited|transport_error`，以及增加旧回合淘汰后的无响应 `context_overflow`，不对 provider unavailable、取消/终态 fence、invalid response 或 Artifact 提交失败自动重试。

**Plinth worker 隔离边界**：
v1 的 supervisor 与每 Attempt 新 worker 同容器、同 uid；worker 在处理 Attempt 输入前必须 fail-closed 建立 `no_new_privs`、Landlock ABI >= 6 与进程内 seccomp，只能访问既定只读运行时路径、当前一次性工作区和 framed stdio，不能读取 supervisor 的敏感 `/proc` 文件、发域外信号、建立外部网络连接、写工作区外路径或继承非 stdio FD。Plinth readiness 与每个 worker Ack 前都实际执行这些对抗检查，任一失败即 `sandbox_unavailable`，不得静默降级。v1 接受同 PID namespace 下世界可读的非秘密进程元数据可见，不引入 user namespace、bubblewrap、额外 worker daemon 或第二套本地协议。

**领域写命令契约**：
所有经认证外部调用者发起的领域写命令都由客户端生成用户不可见的 `client_command_id`，按 `(principal_id, client_command_id)` 唯一，并保存命令类型、非秘密请求摘要和结果对象引用；相同 ID 与相同请求重放返回原结果，相同 ID 与不同请求返回冲突。修改当前状态或当前版本指针的命令还必须携带 `expected_row_version`；纯追加创建不强制 expected version。调度器用 `plan logical identity + scheduled_for UTC` 作为内部确定性 Run 创建键，并在同一事务绑定计划当前冻结的接入与模板；历史执行继续保留其旧绑定。Stele 继续使用 `relay_id`，Runtime 继续使用 `attempt_id + connection_epoch`，不强行改造成 HTTP 命令键。
_Avoid_: 每个 handler 自定义重试语义、最后写入者静默覆盖、把内部 Runtime 围栏混为客户端命令键

**操作关联（Correlation）**：
一次完整业务操作从发起到终结的关联身份，覆盖其验证、排队、执行尝试与结果。它不同于单次请求、命令幂等键、登录会话或执行尝试，不证明权限；后台代执行保留原始发起者和实际执行主体。
_Avoid_: 权限凭据、会话 ID、幂等键、把所有用户活动合并为一次操作

> **审计目标更新（已确认、未实施）：** [ADR-0006](docs/adr/0006-automatic-audit-and-operation-correlation.md) 规定操作默认自动审计、集中受控例外、全生命周期关联，以及系统管理中的统一审计入口；默认且最低保留六个自然月，可延长。它替代下文无期限保留及“账号、Session 与审计投影”中的旧头像菜单入口约定；旧条文不表示新机制已经实现。

**审计与执行溯源**：
领域对象及其不可变版本仍是业务历史权威；另保存窄的 append-only Audit Event，只记录 actor 类型/ID、action、target 类型/ID/版本、client command/request ID、提交时间、成功或确定性拒绝结果及领域记录引用，不复制消息、Evidence、附件、Prompt 正文或秘密。持久审计覆盖登录成功、登出与 Session 生命周期、全部已认证领域写成功及确定性拒绝、用户/角色/密码、秘密 reveal/轮换、Runtime、维护/恢复/离线命令，以及敏感下载的授权和已认证权限拒绝；匿名登录失败、CSRF/畸形匿名请求、无效 Runtime/Stele token 与 429 只进入有界指标和不含密码/完整用户名/credential 的运维日志，不写 SQLite。强制 Audit Event 与领域状态写同事务，审计失败则领域写回滚；敏感下载必须先提交访问审计再发送响应头和首字节；基础设施提交结果未知只记诊断，不伪造权威失败。Audit Event 防御应用用户和 Web Admin，不声称防御拥有 PVC/数据目录 root 权限的部署操作者；v1 不建本地 hash chain 或外部 WORM。每个 Execution Attempt 和低层 Model/Tool Call 保存实际供应商连接 revision/credential generation、模型 ID、Prompt/renderer/agent/tool-schema 版本或 digest、有序输入对象及 revision/digest、Quoin/Plinth 版本、开始结束时间、usage、延迟、重试序号、规范可见模型响应和结构化终止原因；最终领域输出正文继续由消息、Report、Candidate 和 Evidence 等记录承担。不得保存或展示隐藏思维链。结构化审计长期保留并进入备份。

## 告警与调查

**稳定身份保留**：
任何已经发布、执行过 Config Verification Run 或被历史记录引用的稳定 ID/key 永远不能重新分配给另一个逻辑对象，包括 Business System、Logical Alert Source、Connection 以及 plan、check（历史声明中的 discovery key 随声明投影表退役，key 语义仍冻结在历史 declaration_json 中）。停用或从已发布 YAML 移除只形成 Disabled/Retired tombstone，不释放身份；以后再次出现同一 key 表示恢复原逻辑对象及其历史，新业务含义必须使用新 key。显示名称可以修改或复用。只有从未发布、从未运行且从未被引用的草稿/staging 对象可以物理清理。
_Avoid_: 退役后复用 key、隐藏 UUID 与用户 key 双重身份、因显示名变化切断历史

**逻辑告警源（Logical Alert Source）**：
一个具有稳定来源身份的告警发送方；同一 HA Alertmanager 集群的副本共享一个来源身份，不同告警源使用不同的认证凭据和 source ID。
_Avoid_: 单个 Alertmanager Pod、Stele 实例、告警送达

**告警送达（Alert Delivery）**：
逻辑告警源向 Stele 发起并由 Quoin 持久化的一次原始通知请求。Quoin 保存精确原始 body、协议、来源、Stele 接收时间、提交时间、完整性和逐项处理结果。整体 JSON 或 `alerts[]` 无法可靠枚举时记录 Rejected Delivery，不更新任何告警发生并返回非 2xx；顶层可解析时先预检全部项目，在一个事务中处理正常项目并隔离 `FingerprintMismatch`、`IdentityConflict` 等异常项目，不能让数组第一项获胜，也不能让异常项目阻塞其他正常状态更新。Alertmanager 的顶层 status 和 groupKey 只作为分组通知元数据；`truncatedAlerts > 0` 时正常处理已包含项目、永久记录不完整事实并在接入状态中提示，commit 后仍返回 `204`，不对未知的缺失项作任何生命周期推断。
_Avoid_: 告警记录、告警事件、按 payload 合并的请求、用分组状态改写单条告警、单项异常拒绝整个可解析 Delivery

**告警发生（Alert Occurrence）**：
一个具有唯一监控身份的上游 Alertmanager 告警从触发到恢复的生命周期。Alertmanager v1 以 `source_id + fingerprint + normalized startsAt` 定位一个发生：`source_id` 隔离逻辑告警源，fingerprint 延续 Alertmanager 基于完整 labels 的告警身份，startsAt 区分同一身份的不同触发周期。Quoin 同时保存不可变完整 labels 快照，并在关联每次送达前逐项复核；同一三元组出现不同 labels 时记录 IdentityConflict，绝不静默合并。生命周期状态只有 `Firing | Resolved`，只能由载荷中对应 `alerts[i].status` 推进；不得使用顶层 status、groupKey、endsAt、截断后的缺失、来源停用或长期无通知推断恢复或 Unknown。Firing 只表示最近一次有效观察为 firing 且尚未收到 resolved，不承诺目标此刻仍异常。平台内部故障不是 Alert Occurrence，也不得伪造 Delivery 或上游业务归属。
_Avoid_: 告警送达、平台内部故障、事故、分组通知状态、出站 Connection 身份、只按 fingerprint 跨触发周期合并、用 body hash 或 groupKey 去重、把接入完整性混入生命周期状态

**告警视图归属（Alert View Attribution，[ADR 0008](docs/adr/0008-business-view-alert-attribution.md)）**：
Alert Occurrence 首次接收时一次性判定的业务归属权威：候选业务视图必须显式声明交付 Alertmanager 告警源（`alertSourceKeys` 非空）且全部精确标签条件命中。唯一匹配 `attributed`、多匹配 `ambiguous`、无匹配 `unattributed`；空标签条件不构成兜底匹配。判定结果与候选完整快照冻结在独立投影表，终身不重算；读模型展示的 key/name 取自冻结快照。旧 business_system 归属字段与旧归属证据停写、只读保留，历史未归属不重算、不伪造；平台故障与无过滤读取之外的归属过滤永不拼接平台行。
_Avoid_: 旧声明归属、用 Prom connection 顶替 AM 告警源、空标签条件吞掉一切、静默随机归属、按当前视图配置重算历史、重新归属改写历史

**平台故障（Platform Fault）**：
平台内部组件的断连、不可用或执行故障，以明确的平台来源、组件、原因和发生/恢复时间进入统一告警读模型。它有独立来源映射与生命周期，不伪造 Alertmanager Delivery、Alert Occurrence 或业务归属；状态未知必须如实表达，不能推测健康。
_Avoid_: Alertmanager Occurrence、伪造上游告警、独立平台异常中心、关于页徽标替代故障记录

**告警观察（Alert Observation）**：
一个有效 `alerts[i]` 在 Quoin 中形成的不可变观察，保存 Delivery、项目索引、观察状态、来源声明的 startsAt/endsAt、Stele 接收时间、Quoin 提交时间以及它对 Occurrence 的作用。Quoin commit 顺序是系统实际观察顺序；页面状态变化时间使用真正完成转换的 commit 时间，来源时间单独展示。resolved-first、重复通知和 resolved 后迟到 firing 都保留观察，迟到 firing 不重新打开已恢复 Occurrence。
_Avoid_: 可覆盖的当前状态、按来源时间重排历史、隐藏乱序

**告警接入问题（Alert Intake Issue）**：
与普通告警生命周期分离的接入质量事实，包括 IdentityConflict、FingerprintMismatch、DeliveryTruncated 等。冲突项不进入普通告警列表；截断只标记 Delivery 和来源，因为被省略的具体告警不可知。接入问题管理（列表、详情、历史与确认）仅 Admin 可读和操作；Operator 不得进入或直接读取该管理 API，只能在告警页看到授权的非秘密影响提示。Admin 确认已处理不删除或改写历史，后续再次发生会重新出现。
_Avoid_: 第三种告警状态、自动恢复、事故

**初步分析（Initial Analysis）**：
用户针对一个告警发生触发的一次性模型分析；创建后立即可见，状态为 `Queued | Running | Succeeded | Failed | Cancelled | Interrupted`。数据库强制同一 Alert Occurrence 同时最多一个 active Initial Analysis，双击或命令重试返回同一记录。技术失败重试在同一 Initial Analysis 下创建新 Attempt 并复用创建时输入快照；第一个合法成功结果原子封存为不可变输出。成功后再次“重新分析”创建新的 Initial Analysis，旧结果保留。它可以调用只读工具补充 Evidence，不接受后续对话，但可以连同其引用被 Investigation 使用；Succeeded 只表示模型分析完成，不表示告警正常或诊断已验证。
_Avoid_: 调查、对话、可覆盖结果、已验证诊断

**调查（Investigation）**：
围绕一个运维问题展开的一条对话线程，可以引用一个或多个告警发生、初步分析及相关证据，并形成诊断。Investigation 页面就是主流 Chat 页面：直接新建后立即输入自然语言，不要求先选业务系统、连接、告警、Evidence、模型或工具；从既有告警、Initial Analysis、Evidence 或 Inspection 入口进入时自动保留来源引用。模型只识别人类领域对象并以 `sourceRef` 等人类可读定位指名来源，Quoin 才按已启用接入与冻结授权确定性路由 Tool 参数；真实歧义由模型在对话中追问。
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
某个不可变模型输出基于已有证据形成的解释与结论；它可以是 Initial Analysis 的成功输出、某个 Inspection Report 版本，或用户明确选择的 Investigation assistant message/Attempt 输出。面向人的诊断简短回答当前问题，保留影响判断的关键事实、证据引用与不确定性，并在有依据时建议下一步；原始观测和取证过程不是诊断正文，仍归属于对应告警、巡检运行或调查回合。显式要求结构化格式的输出仍保留该格式，阅读投影不得伪造摘要或改写不可变原文。系统不维护会被后续回复覆盖的 Investigation 级“当前诊断”；诊断在被人确认前不代表已验证事实。
_Avoid_: 事实、告警、整个调查、可覆盖的当前结论

## 工作台投影

**三栏工作台**：
第一栏是默认只显示图标的全局导航，hover/focus 时解释用途；全局入口只有运维中心、AI SRE 和管理员设置。运维中心包含告警列表、故障复盘、巡检、业务视图和接入管理子页，不建设独立全局接入中心；AI SRE 包含对话和知识子页；管理员设置仅对 Admin 可见。告警列表以 URL query `id` 选中统一告警读模型中的对象时在右侧覆盖式详情中打开；上游 Alertmanager Occurrence 与平台故障保留各自来源和生命周期，不能混淆。业务视图提供表单和 YAML 编辑同一对象的可选范围组织；接入管理先展示支持的平台目录与已接入实例，进入平台专属配置表单及说明。内部组件管理不面向普通用户；管理员在设置“关于”查看组件版本和连接状态，平台故障仍在统一告警中可见。页面位置不放宽服务端权限。工作台直接启用所选 shadcn `sidebar-09`/Sidebar 与 Resizable primitives 已有的展开、折叠、隐藏、拖动调整、键盘调整和浏览器本地布局恢复能力，不另造平行布局系统；第一栏保持图标导航语义，第二栏可折叠或调整宽度，第三栏使用剩余空间。URL 未选择对象时不自动选择列表第一项，第三栏使用 shadcn `Empty` 的图标、标题、自然语言描述和至多一个主操作说明当前可做什么；不得留白或伪装成 Dashboard。窄屏时第一栏变抽屉，告警详情及列表在需要时分别全屏显示且详情内容独立滚动。

**工作台展示约定**：
界面使用紧凑但不拥挤的运维信息密度，列表优先展示状态、对象名、关键时间和业务系统，完整内容进入详情，不提供密度设置。颜色跟随系统明暗偏好，不提供应用内主题设置。v1 界面使用简体中文，代码、labels、annotations、协议状态、日志和上游错误保留原文，不建立无实际消费者的 i18n 机制。业务筛选与实际存在的可切换排序进入 URL query，形成可刷新和可分享的确定性视图；v1 当前列表排序由领域契约固定，不暴露没有服务端契约的统一排序控件。分页游标、滚动位置、临时展开和选中状态属于当前浏览器历史项，返回时恢复并用服务端快照/SSE 调和。cursor 列表首次只读取一页，底部由明确的“加载更多”触发下一页，不伪造页码、不自动无限滚动；加载后仍是同一连续列表。实时新项目到达且用户不在顶部时，保持当前可视内容与焦点不动并显示“有 N 条新内容”，用户触发后合并并回到顶部；已在顶部时可直接合并，但不得抢焦点或自动打开详情。跨模块关联跳转使用浏览器原生历史，返回到来源详情，不维护第二套面包屑栈，也不默认新开标签页。
_Avoid_: 大卡片列表、密度/主题配置页、把滚动像素写进可分享 URL、自定义导航历史、自动无限滚动、实时插入导致阅读位置跳动

**首次设置投影**：
空告警页根据实际状态说明缺少什么并提供“完成初始设置”入口；管理模块提供可跳过、依赖驱动的设置清单，不建设阻塞使用的线性 Wizard。清单从权威状态派生并分别展示模型供应商、接入验证与启用、Plinth、Stele 告警源和备份目标的就绪状态、依赖及直接修复入口；告警接入可用与巡检可用分别计算，不要求一次配齐全部能力。Admin 可在管理模块直接处理；Operator 不显示管理入口，只在告警等相关模块看到“需要管理员完成”的结果与影响，不暴露不可进入的配置清单。设置清单始终由权威状态派生且保留：全部就绪时折叠为一行“核心能力已就绪”，有故障或未完成依赖时自动展开受影响项并直达对应接入、Runtime、告警源或备份详情；不保存用户勾选的完成状态。
_Avoid_: 空页面、强制线性向导、把内部对象依赖留给用户推导、所有能力全配齐才允许使用

**管理工作区**：
管理模块只对 Admin 出现在全局导航中；第二栏按设置清单、用户、模型供应商、备份、安全、审计和“关于”分组，第三栏显示所选列表、详情或设置，不增加第四栏或卡片墙管理首页。“关于”向管理员展示平台及内部组件的版本、连接状态和受保护维护入口；它不是独立平台异常中心。运维中心的业务视图和接入管理承担视图组织与接入的操作入口；权限始终由服务端裁决，前端隐藏不是权限边界。
_Avoid_: Operator 管理入口、普通用户内部组件管理、独立平台异常中心、第四栏、管理 Dashboard 卡片墙

**操作、表单与反馈**：
只有会立即撤销 Session/凭据、停用当前能力、替换 Runtime、切换已发布版本或丢弃未保存输入的高影响操作才确认；确认直接说明影响对象与恢复方式，不要求输入对象名。普通读取、下载、测试、启动、重试不确认并立即反馈。普通设置显式提交、不逐字段自动保存；客户端只做机械即时校验，服务端结果是权威。失败保留全部输入、聚焦首个错误并提供页首摘要；成功在当前对象状态中持续可见，toast 只能补充。业务系统声明可由等价表单或 YAML 编辑，二者走相同的静态校验、Config Verification Run 与发布路径，不建立第二份声明、第二表单保存路径或通用 JSON/YAML 编辑器。后台任务终态在所属列表/详情持续显示；用户位于其他模块时用导航徽标和非阻塞 toast 补充完成/失败并可返回对象，但不建设通知数据库、铃铛收件箱、已读状态或强制浏览器系统通知；六个导航图标只投影 active/failed Attempt、巡检 gap、待确认知识、未确认接入问题、Runtime/备份故障等可操作权威状态，重新打开应用时从对象状态重建，不只用 toast，也不自动跳转。时间主显示为带时区的浏览器本地绝对时间，相对时间只作辅助；hover/focus 或详情提供原始 offset 时间、UTC 与复制，持续时间单独显示。对象按 row version 原位刷新；若已不存在或不再可操作，保留已渲染内容并显示状态条、禁用非法操作和提供返回列表，Session/权限撤销仍立即结束访问。
_Avoid_: 每次写操作确认、仅 toast 表达结果、自动保存半完成配置、错误后清空输入、伪造页码或时间、静默保留可提交的陈旧页面

**跨模块交互状态**：
Evidence、Initial Analysis、Inspection Report、Knowledge、配置版本和 Observed Resource 等已持久化长内容使用确定性嵌套路由铺满工作台；浏览器后退关闭阅读层，刷新和分享恢复同一对象。一次性秘密、未提交上传和未保存表单不进入可分享 URL。Stop/Cancel 提交后按钮立即变为不可重复触发的“正在停止”，保留当前阶段和已完成内容；只有服务端确认 cancellation fence/终态后才显示 `Cancelled`，失败则恢复合法操作并说明原因，用户可离开等待。部分完成不发明统一领域状态，而是并列显示父对象真实状态、每个子步骤终态和机械计数，已完成 Evidence/Artifact 继续可读，失败项原位提供合法恢复动作，程序不按比例生成健康结论。

无权执行的写动作不显示；直接访问受限 URL 时显示工作区级 403，使用普通语言说明所需角色和返回入口，不伪装为对象不存在，也不建设申请权限流程。Session 失效时立即卸载工作区并直接进入完整登录页面：受保护内容、未提交草稿和页面内存中的秘密随之丢弃，不保留遮蔽层，也不做同用户草稿恢复；重新登录（无论 principal 是否相同）都从全新工作区开始，会话草稿不写浏览器持久存储。临时密码登录停留在同一认证页面的初始化步骤：验证临时凭据、设置正式密码并完成收码渠道验证后，初始化完成即返回登录页，不建立 Session；只有随后的正常两步登录成功后才加载工作台数据。

对网络中断、429、可恢复 5xx 和结果不确定的命令默认自动恢复：读取与复用同一 `client_command_id` 的命令总计尝试三次，重试间隔 1 秒、2 秒；验证失败、权限不足等确定性错误不重试。三次失败后显示“内部错误”、普通语言原因、“如持续发生请联系管理员”和可复制诊断；停留 10 秒后自动重读当前对象一次，仍失败则回到所属列表上一层。倒计时只存在当前页面内存，用户主动刷新、后退、离开或成功重试后立即取消，绝不在新页面继续旧回退。错误默认先用自然语言说明发生了什么、影响和下一步，并在表单页首或对象状态中持续显示；可展开技术详情只包含稳定错误码、request/Attempt ID、阶段、必要上游原文与复制诊断，禁止堆栈、Authorization、Cookie、秘密或整份无关请求正文。

普通对象列表使用语义化链接/按钮与自然 Tab 顺序，`Tab` 可到达控件、`Enter/Space` 可操作；Sidebar、Dialog、Tabs、Resizable 等沿用 shadcn/Radix/APG 键盘行为，不把整个页面做成 application/grid，也不发明隐藏快捷键。抽屉/确认框打开时焦点不进入背后页面，关闭后回到触发控件；全工作台内容打开时焦点进入标题，关闭/后退后回到原消息或列表行及原滚动位置；未保存输入继续遵循丢弃确认。窄屏详情顶部始终提供明确“返回列表”，不依赖浏览器手势。普通模式的全工作台层使用简短、可中断的从右向左渐入/渐出；`prefers-reduced-motion` 时关闭位移、淡入淡出、列表重排和循环装饰动效，直接显示相同终态，同时保留静态阶段图标、文字和真实进度。
_Avoid_: 瞬时秘密深链、本地伪造 Cancelled、统一 partial-success、按失败比例判健康、403 伪装 404、Session 草稿持久化、会话过期遮罩层与同用户草稿恢复、临时密码进入工作台、确定性错误重试、换 command ID 重试、跨页面遗留回退定时器、原始堆栈主错误、全局隐藏快捷键、reduced-motion 丢失等待反馈

**告警列表与详情**：
告警列表默认显示 Firing Occurrence，并按真正状态转换的 Quoin commit 时间倒序；Resolved 进入历史筛选。列表项使用紧凑两行而不是横向表格或大卡片：主行显示状态图标与文字、alertname 和关键时间，次行显示业务系统、可用的 severity 原值和必要状态徽标，选中/hover/focus/变化不只靠颜色表达。列表顶部固定“当前/历史”分段控件与业务系统可搜索 combobox，有筛选时可直接清除；不展示服务端不支持的任意 label 构造器、全文查询或 severity 顺序。统一列表通过 items 和 columns 配置标题、状态、次要信息、时间和受支持操作，加载、空和失败状态与真实数据区分。详情以右侧覆盖式抽屉显示，概览 Tab 响应式双列展示真实描述/注释和完整 labels/annotations，时间线 Tab 只展示真实 Alert Observation；没有独立历史数据时不伪造历史 Tab。AI 分析 Tab 才读取、创建或复用 Initial Analysis：首次进入仅在成功读取到没有分析时创建，活跃任务优先附着并轮询至终态，成功或较新的失败结果按真实时间选择，失败/中断提供显式重试。关闭详情不取消任务，不提供认领、手动关闭、聊天或 Investigation 创建；输出保留 Evidence/Attempt 状态和现有 Evidence 阅读能力。severity 只是原样展示和筛选的普通 label，Quoin 不定义顺序，模型不推断。普通列表不显示 IdentityConflict 等接入问题；接入问题迁至 Admin 管理模块的“告警接入问题”入口，保留查看和确认，确认不删除或改写历史，后续再次发生会重新出现。详情选中状态使用 `id` query，而非 `/alerts/:occurrence` 路径；Occurrence resolved 后当前 URL 仍可查看并明确显示已恢复。

**实时投影**：
影响告警列表的事务在同一 SQLite 事务中产生单调递增 `alert_change_seq`；HTTP 快照返回 `snapshot_seq`，每个 Occurrence 返回 `row_version`。客户端首次建立 SSE 时携带 `after=snapshot_seq`，重连使用 `Last-Event-ID`；Quoin 回放其后的有界派生变更。断线、重连、回放和游标过期后的完整快照刷新均在前端静默完成，不向普通用户暴露 SSE、sequence、cursor、resync 等无可操作价值的技术名词，不清空当前阅读位置或抢焦点；多次恢复失败后统一进入普通内部错误恢复流程。SSE 可重复投递，客户端按 sequence 与 row version 幂等应用；游标过期时重新读取完整快照。事件只携带 Occurrence ID、变化类型和版本，选中详情发现版本变化后重新读取。Resolved 从 Firing 列表移除，但已打开 URL 继续显示并标记已恢复。新告警非阻塞提示不打断当前详情；该变更流可丢弃、可重建，不是告警历史权威源。

**调查与巡检工作区**：
调查模块第二栏显示 Investigation 列表，第三栏使用 assistant-ui 对话工作区，既有调查 URL 为 `/investigations/:investigation`。列表标题由程序从当前分支第一条有效用户消息机械生成，空白时回退为关联来源或“新调查 + 创建时间”，不持久化独立标题、不调用模型；列表按当前分支最后消息/Attempt 活动时间倒序。点击新建先进入 `/investigations/new` 空白对话，第一条消息被服务端接受时才原子创建 Investigation、消息和 Attempt；未发送即离开不产生空记录，也不先要求标题、业务系统、告警、模型或工具。从告警进入时在发送框上方显示当前 Occurrence 与用户选中 Initial Analysis 的不可变来源项并直接聚焦输入，第一条消息提交时与来源原子写入。用户位于底部时跟随新 token/message；用户向上阅读后停止自动滚动并显示“查看新回复”，不得抢焦点或改变阅读位置。失败 Attempt 对应的用户消息左侧显示环形重试按钮，点击后按既有消息创建新 Attempt；active Attempt 期间发送按钮变为方形停止按钮，点击提交 cancellation fence，终态后恢复发送按钮。Tool Call 在对话中显示为可折叠状态卡片，默认展示工具名、真实阶段、耗时或终态与人类可读摘要，原始参数、输出和诊断详情原位展开；窄屏不为工具调用增加第二页面或上下分屏。点击 Evidence 引用后，内容从右向左渐入并铺满整个工作台，关闭后恢复原消息与滚动位置；Initial Analysis 完整正文与 Inspection Report 也使用同一全工作台阅读层，详情只保留状态、摘要和版本入口；减少动态效果模式直接切换到同一终态。巡检运行 URL 为 `/inspections/runs/:run`。进行中的初步分析、调查和巡检立即显示已受理与真实执行阶段，用户可离开页面，完成或失败后在列表和详情持续可见。任务创建命令先在 Quoin 事务中保存业务对象和 Attempt，SSE 只是观察通道，断线不取消任务；任务变化使用单调 sequence 与对象 row version，进入页面先读 HTTP 快照再建立 SSE，重连有界回放，游标过期 `resync_required`。事件只传状态、工具阶段和版本，token delta/高频动画不持久化。最终消息、Report 或 Candidate 必须先原子持久化，任务随后才能 Succeeded。Tool Call 执行前创建记录并以真实时间戳单调推进，返回页面从 Attempt 快照恢复完整时间线；不伪造百分比、不展示或声称保存隐藏思维。

**巡检工作台投影**：
巡检模块第二栏使用紧凑两行 Run 列表：主行显示计划名、真实采证状态和关键时间，次行显示来源接入、人工/调度触发方式、报告与缺口徽标；顶部只提供服务端支持的计划和状态筛选，`Completed` 不翻译为“健康”。标题区的“运行巡检”通过轻量选择层选择独立巡检计划（范围覆盖整个接入、业务视图或显式对象集合），从接入详情进入时按接入预选；同计划已有 active Run 时直接打开，不创建重复项。Run 详情为一个连续页面，先展示状态与时间、分析状态及最新可读报告，再展示检查结果、Evidence 缺口和运行资料；报告不存在或分析失败时如实展示状态，不生成替代结论。提供简短页内 section navigation，不拆成隐藏上下文的多 tab，报告生成要求与执行详情按需展开。每个检查默认显示名称、`ok/gap`、采证时间与 Evidence 数量，展开后显示原始 PromQL、类型化参数、真实结果、warnings、gap code 和相关 Attempt；程序不生成系统健康结论。页面分开显示“重新分析现有证据”和“重新采集”：前者只创建新 Report 版本，后者创建新 Run 与 `evidence_at`；根据当前失败/缺口推荐其一，但都不弹确认框，也不合并成含糊的“重试”。
_Avoid_: Run 卡片墙、`Completed=健康`、隐藏检查事实、通用重试、登录后改写或自动补跑旧 Run

**知识工作台投影**：
知识模块第二栏分为“知识 / 待确认 / 导入批次”，第三栏显示所选详情；只有导入批次提供“导入文本”，不提供空白知识表单或知识 Dashboard。人类检索只有一个自然语言输入，结果分列“精确文本匹配”和“语义相似”，分别保留命中依据、分数与索引状态；同一 Knowledge 双重命中时只显示一次并标出两种依据，程序不合成统一相关性总分，也不要求用户先选择 FTS5/向量实现。Knowledge Candidate 从来源诊断、待确认列表进入一个从右向左铺满工作台的编辑层，只编辑标题、正文和适用范围，来源诊断/Evidence 只读；草稿按 revision 保存，冲突时保留本地输入并展示最新版本，不静默覆盖。成功 Initial Analysis 与 Inspection Report 标题区提供次要“整理为知识”，Investigation 只在用户明确选中的 assistant message 菜单提供；点击创建或返回同源 AwaitingConfirmation Candidate 并直接打开编辑层，已有 Candidate 不重复创建，已被标记“不采纳”的诊断不能再创建。导入批次接收一份粘贴原文后立即进入可离开的真实 Processing 状态；成功后在同一批次逐条展开修改或排除 Candidate，并以一次事务确认当前全部，任一 revision 冲突则全不提交并定位冲突项。

每个不可变 Diagnosis 正文底部提供“记录实际结果”：已采纳、已执行、验证有效、不采纳。反馈精确绑定 Initial Analysis 输出、Inspection Report 版本或具体 Investigation assistant message，追加不可变事件并原位显示最新投影与历史；程序不强制四种反馈必须按顺序经过，也不维护 Investigation 级总反馈。每次反馈可选填简短说明；正向反馈直接追加，“不采纳”须确认相关 Candidate 将变为 SourceInvalid、已确认 KnowledgeVersion 将永久退出检索。知识详情展示当前版本、范围、来源诊断、反馈、检索/index 状态和不可变历史；“修订”以当前版本预填待确认草稿，确认后创建下一不可变版本，不原地覆盖；一次修订 Candidate 被排除后保留历史，但同一 current version 仍可重新发起新修订，不能形成永久死路；“停止复用”经影响确认使该版本粘性退出，恢复只能修订并重新确认新版本。
_Avoid_: 混排正式知识与候选、索引实现选择器、程序融合排名、窄弹窗编辑长正文、模型自动写入、逐条跨页面确认

**业务纳管历史投影**：
业务系统模块已退出运维中心导航，其运维职责由接入管理与业务视图承接；既有业务系统与配置版本经只读入口保留，用于追溯旧声明的绑定关系，不提供新的声明编辑、发布或启停操作；验证 Run 与 Observed Resource 的持久面已随 2026-09 退役迁移物理删除，也不发明“当前草稿”。Observed Resource 历史列表明确区分“当前观测到 / 当前未观测到 / 数据陈旧”，不把未观测到解释为删除；新的观测事实由来源级观测拥有，观测范围来自接入。

**Label Contract 激活投影（历史）**：
全局契约的联合激活界面已随业务声明一同退出主线；既有激活记录与相关 Run 只读保留，用于解读历史配置切换，不得作为新配置或标签语义的入口。
_Avoid_: 双配置入口、Business System 卡片墙、latest draft、上传即发布、可编辑 CMDB、Cookie/profile 文件编辑、部分激活

**账号、Session 与审计投影**：
全局导航底部头像菜单只包含当前身份/角色、修改密码、我的 Session、审计记录和退出；低频账号操作不占主导航。我的 Session 使用全工作台层列出设备/浏览器、创建时间、最后活动并标记当前 Session，其他 Session 可逐个撤销，确认明确说明对应 SSE/WebSocket 会立即断开。Admin 用户管理列表显示用户名、显示名、角色、启用状态和最后登录；详情原位修改显示名/角色/状态，并提供重置密码和撤销全部 Session。禁用、降级、重置密码与撤销 Session 均说明现有登录影响；最后一个有效 Admin 的服务端冲突原样解释，不通过隐藏按钮冒充不可能。仅 Admin 可从头像菜单进入 `/audit` 全工作台审计列表，按 actor type、action 与时间筛选并查看结构化事件；它不是第七个常驻模块，返回恢复原业务页面。

**连接、凭据与 Runtime 管理投影**：
管理页按 Thanos、模型供应商等真实 Connection kind 使用类型化表单，只收集该类型真实的非秘密字段和凭据，不提供任意 URL+JSON 编辑器。详情分开显示当前 ConnectionRevision、CredentialGeneration、启用/重验状态、最近真实测试与不可变历史；Operator 只在相关业务页面看到非秘密连接状态与影响。模型供应商创建/轮换后显示“尚未验证”：先列出 `/v1/models` 返回 ID 供 Admin 选择，列表缺失时提供手工 model ID 与未声明元数据输入；真实 capability probe 可后台运行，成功后显示实测能力并允许启用，失败保留 revision/generation、手工输入和结构化非秘密错误码/允许字段，不复制供应商原始响应，已启用供应商轮换时先停用且不在 probe 前自动恢复。Alert Source 详情显示凭据的非秘密 ID、Active/Pending Retirement/Retired、创建/首次使用/退休时间；轮换后新旧两个 generation 可认证，新值首次成功使用后旧值进入 Pending Retirement，并提示“更新 Alertmanager → 确认新凭据已使用 → 显式吊销旧凭据”，程序不自动猜测切换完成或自动吊销。创建/轮换返回 reveal handle 时，前端立即调用一次 reveal 并打开铺满工作台的一次性秘密层：原文可见且可复制，明确关闭后不能再次查看；秘密只在当前页面内存存在，不进入 URL、toast、日志、下载或浏览器持久存储，关闭后只能通过新轮换取得。

组件状态页展示 Plinth 的当前在线状态（boot、最后见到时间、对端版本）；不存在注册状态或凭据轮换展示（ADR-0009）。备份页为连续页面：顶部显示目标挂载状态、计划时间、IANA 时区、保留份数与最近成功，并显式保存设置；下方展示不可变备份记录、真实阶段/错误、大小、checksum 和下载，“立即备份”受理后可离开。失败在告警页/管理徽标持续提示并可重试；Web UI 不提供在线恢复，只提供停机恢复说明与所选备份 manifest 信息。
_Avoid_: 个人设置全局模块、通用连接 JSON、秘密持久化、自动吊销旧凭据、健康/异常单灯、动态 Runtime slot、在线覆盖恢复

## 资源与巡检

### 插件化接入、观测与巡检（ADR 0004，主线已实施）

[ADR 0004](docs/adr/0004-plugin-capability-registry.md) 把接入、自动观测、模型工具、巡检与可选业务视图从业务声明前置中解耦。主线链路——接入验证并启用→启用接入的默认来源级观测→Agent 工具按冻结授权与 `sourceRef` 定来源→独立巡检计划按整个接入／显式对象／业务视图定范围→基于 Evidence 的不可变报告——已随实现落地并由主线集成验证覆盖。旧声明、Run 与 Evidence 保持其历史解释，禁止按新模型重写过去事实。

**插件（Plugin）**：随组件构建发布的可信能力实现，以稳定 ID、版本、封闭配置和能力描述显式注册。插件在自己包内同时拥有能力描述与编译工具实现，描述声明由实现派生，二者不可漂移；插件按需提供 Probe、Discover、模型 Tools、ExecuteTool、巡检模板及 Collect，控制面描述与运行时执行经同一插件注册机制的执行绑定连接。部署 YAML 选择启用集合；缺省启用 prometheus、thanos、alertmanager。browser 与 kubernetes 插件已连同其描述符与历史兼容层彻底移除，历史冻结 attempt 与旧库中的相关数据不再保证可解析。不提供动态 `.so`、在线安装任意代码或另一套插件 RPC（[ADR 0007](docs/adr/0007-unified-plugin-assembly.md)）。
_Avoid_: 第二工具表、核心表硬编码插件工具、旁路执行器注册、动态插件加载

**接入启用（Integration Enablement）**：接入先经真实 probe 验证再显式启用；启用是接入从“已配置”进入“可观测、可授权、可巡检”的唯一门槛。启用事务内幂等创建该接入仅人工运行的默认基础巡检计划，失败整体回滚，不会出现“已启用却无即用计划”的中间态；停用只阻止新派发。启用不要求任何业务声明。

**插件工具目录（Plugin Tool Catalog）**：由已启用插件及本次 Agent 授权生成的规范有序工具集合，冻结参数／结果 Schema、工具版本、执行位置与摘要。模型和 worker 使用同一目录；每次执行仍重新验证接入授权、插件启用和取消状态。新任务目录、实现查找与执行分发表由同一次注册装配派生，不存在第二来源。已创建任务的 `tool_catalog_json` 是授权快照而非注册表：按工具名+版本+契约与装配实现匹配，实现漂移显式拒绝，不自动升级或扩权；缺失快照的历史任务回退到固定冻结的历史目录文档，不用当前启用目录补齐。初步分析与调查把已启用接入冻结为来源级授权输入：模型只能以 `sourceRef` 等人类可读定位指名来源，来源连接由 Tool Call 事务冻结的 grant 决定，来源歧义返回可恢复的预检结果，绝不回退“第一个”接入。目录不包含秘密，不允许模型扩大权限或注入工具。

**业务视图（Business View）**：对来源接入范围与明确标签条件的可选版本化组织，可附加业务说明；表单与 YAML 编辑同一对象，按 row version 冲突裁决。视图不拥有资源身份、凭据或额外权限，只收窄候选。没有业务视图也可接收告警、观测对象、使用已授权工具和执行基础巡检；消费者（当前是巡检计划与告警归属）在各自消费时冻结所用视图内容，视图后续修改不改写历史。视图可通过显式 `alertSourceKeys` 参与告警归属（见"告警视图归属"）；接入（connection）与告警源（alert source）是不同身份空间，互不顶替。

**来源级观测（Source-scoped Observation）**：已验证并启用的接入通过其插件 Discoverer 执行的有界观测；启用、手动刷新与默认周期调度共用同一准入，同一接入同时最多一个观测 Run，定时去重键为 `scheduled_for`。观测对象身份由接入、对象类型和规范来源身份（按插件声明 identity labels 规范编码）确定，不依赖业务分组，跨来源同名对象不合并；只有同一冻结范围完整成功才能把未再见到的对象标记为未观测。失败、截断和局部结果不得清空资源或推断物理删除；陈旧是显式标注的事实，不是观测推断。

**独立巡检计划（Inspection Plan）**：直接绑定一个来源接入与一个插件巡检模板的计划，不再内嵌于业务声明。范围三选一：整个接入、显式对象集合或业务视图（业务视图仍固定与计划自己的接入相交，绝不全源查询）。Run 创建时确定性展开并冻结目标、模板版本、参数、查询窗口、接入修订与授权；执行中不扩大目标，重新采证必须创建新 Run。定时巡检需要显式启用计划并配置标准五字段 cron（时区由计划自身提供），同一计划同时最多一个 Run，重叠定时周期 SkippedOverlap 且不补跑。

**插件巡检模板（Plugin Inspection Template）**：插件贡献的版本化确定性采证定义，参数是封闭字面量并经 AST 静态校验。检查由程序机械执行并形成 Evidence；模型基于持久化 Evidence 与缺口生成不可变报告，不直接写权威报告。定时巡检／模型报告需要明确启用，采证成功不等于业务健康。

下述「历史标签契约」「业务系统」「观测资源」「业务系统配置版本」属于已被替换的旧活动模型条款：BusinessSystem 声明的写入与发布入口、独立资源刷新调度和业务绑定巡检计划均已按 ADR 0004 移除；Config Verification 引擎与 `config_discoveries`/`observed_resources` 表已随 2026-09 退役迁移物理删除（历史版本正文与 `config_resource_scopes` 仍可读），；其余既有条款只描述既有记录的解读方式。

**历史标签契约（Historical Label Contract）**：
曾作为部署级、版本化 Prometheus 业务归属 label 语义的配置模型。它及其版本、激活、关联 Run 和 E2E 记录只为解读和保留既有历史而存在，不能充当活动全局标签权威。它先被 ADR 0003 的按声明标签语义取代，随后又被 ADR 0004 的来源级接入与业务视图取代；这些层次只用于按当时模型解读历史记录。
_Avoid_: 把历史契约当当前权威、从旧记录推断新声明、多个当前契约、部分切换

**业务系统（Business System，历史模型）**：
曾由一份版本化 `quoin/v1` `BusinessSystem` 声明界定并承担业务配置唯一权威的稳定运维范围，生命周期只有 `Enabled | Disabled`。ADR 0004 后业务声明不再拥有接入选择、资源范围、查询授权和巡检计划，声明的创建、编辑与发布入口已移除；既有系统、配置版本及其 Run、Evidence 按当时冻结的声明只读保留，停用系统的告警与历史继续可见。旧声明的告警归属证据仅作历史事实展示（ADR 0008 后新归属由业务视图拥有），不再派生任何采集、授权或调度。
_Avoid_: YAML 与数据库双配置权威、接入即扫描、租户、Kubernetes 集群、自动推断服务、停用删除历史

**观测资源（Observed Resource，历史模型）**：
旧模型中由具有业务身份声明的已发布任务产生、稳定身份为 `BusinessSystem ID + ResourceDiscovery key + 按 label 名排序的 identity label/value map` 的运行对象事实（该投影表已随 2026-09 退役迁移物理删除）；它不是人工维护的 CMDB 资产记录或实时资产库存。独立资源刷新调度已随 ADR 0004 移除，新的观测一律写入来源级观测对象；既有 Observed Resource 及其观测时间、来源任务与配置版本保持只读历史，不按来源身份合并或改写。“只有完整范围成功观测才能表达未再观测到，失败或缺口不得清空资源或推断物理删除”的规则继续适用于新模型。
_Avoid_: 资产、CMDB 条目、Kubernetes 对象快照、独立周期资源刷新、用 `/series` 元数据证明当前资源状态、完整 labels fingerprint 身份

**连接（Connection）**：
Quoin 访问一个外部运行系统时使用的稳定命名身份与访问边界，由系统中的多个用户和任务共享。第一版使用明确类型的 Prometheus/Thanos 和模型供应商连接；Runtime、Stele 告警源和 Business System 不是 Connection。地址、TLS、CA、用户名等非秘密配置形成不可变 `ConnectionRevision`；密码、API key 等秘密独立形成加密 `CredentialGeneration`。修改或轮换创建新 revision/generation 并原子切换当前指针，不原地覆盖。Attempt 派发前在事务中检查连接启用并绑定实际 revision/generation，Attempt、Evidence 和审计只记 ID。普通切换不影响已经被 Runtime 接受的 Attempt，它使用内存旧快照完成；停用连接时阻止新派发，等待任务以 `ConnectionDisabled` 结束，已接受的只读 Attempt 可完成并允许用户取消。历史保留非秘密 generation 元数据，旧秘密不再下发。
_Avoid_: 无类型 URL+凭据、可覆盖配置、长期可下发旧秘密、用户凭据、巡检计划、Runtime 身份

**指标接入（Metrics Integration）**：
Prometheus 或 Thanos 类型的接入（Connection），保存访问能力而不代表任何业务用途；同一平台可有多个接入。工具、观测与巡检引用必须解析到明确的接入，缺失或歧义时显式失败，不能回退到全局或“第一个”接入。Prometheus 与 Thanos 是不同的平台类型；认证模式只限 `none`、HTTP Basic 和 Bearer，TLS 仍使用接入的既有类型化配置，不能以跳过证书校验替代 TLS 配置。接入地址、凭据与 TLS 秘密只在接入边界管理。模板 PromQL 使用 AST 校验并受插件模板声明约束，不依赖业务声明或全局标签语义。
_Avoid_: 全局唯一 Thanos、缺失或歧义引用时回退、每业务系统私有凭据、Grafana 数据源、Thanos StoreAPI、字符串改写 PromQL

**接入（Integration）**：
用户从支持的平台目录配置并管理的访问能力，当前包括 Alertmanager、Prometheus 和 Thanos（浏览器与 Kubernetes 接入已移除，历史连接与记录仅按旧库解读，不再保证可解析）。接入本身是配置、验证、启用、观测与授权的直接对象。接入的保存和浏览不采集业务资源；验证并启用后按其插件能力开始来源级观测，并进入 Agent 工具授权与巡检计划范围。Run/Attempt 冻结实际使用的接入修订。
_Avoid_: 全局接入中心、业务配置中的秘密副本、接入即扫描、强行统一的存储类型

**巡检项（Inspection Check）**：
一次 Run 中由计划按其范围确定性展开并冻结的独立检查，具有跨版本稳定 key 和真实采证结果。模板参数（如 PromQL 表达式与窗口秒数）是封闭字面量并经 AST 静态校验，不支持模板变量、环境变量、循环或按对象动态展开；多个目标必须显式展开为多条 check。程序机械执行检查并形成 Evidence，模型统一分析全部证据；range 查询以真实开始采证的 `evidence_at` 为终点并保存实际 start/end/step。YAML 不提供 `expect` 或断言规则。
_Avoid_: 诊断、巡检报告、由程序猜测的检查、健康阈值规则引擎、动态 fan-out、通用模板或 DSL

**业务系统配置版本（Business System Configuration Version，历史模型）**：
旧模型中每个业务系统的完整版本化权威声明，原子包含业务系统 name/enabled、接入引用、指标与资源范围、资源身份规则、告警来源和告警 labels、以及全部巡检计划；每个系统只有一个当前已发布版本，机器形状由 `business-system.schema.json` 拥有。该写入、发布与联合激活主面已随 ADR 0004 移除；既有版本及其 Schema、严格 YAML 解析与 Config Verification Run 记录仍用于解读历史声明、历史 Run 和迁移映射，不接受新草稿或发布，也不得被当作新配置的权威。
_Avoid_: 页面隐式归属、缺失指标引用、共享可覆盖草稿、运行时重新解析、表单与 YAML 双权威、要求用户先读内部 Schema

**巡检运行（Inspection Run）**：
一个独立巡检计划在调度时刻或人工触发下产生的不可变机械采证记录，Run 创建时冻结计划绑定与接入修订；旧业务声明计划的 Run 以其冻结声明版本解释，新 Run 一律来自独立计划。权威状态只描述采证：`Queued | Running | Completed | CompletedWithGaps | Failed | Cancelled | Interrupted | SkippedOverlap`。Completed 表示全部检查形成完整 Evidence，不表示系统健康；CompletedWithGaps 表示采证已结束并冻结结果，但存在 RuntimeUnavailable、AuthenticationRequired、部分响应或检查失败等缺口，即使没有成功检查，只要完整记录每项缺口仍属此状态；Failed 只表示无法形成并提交有效冻结结果集合。模型分析 Attempt/Report 使用独立状态，分析失败不回写 Run，页面可显示“采证部分完成/分析失败”。同一计划不并发：重叠定时周期 SkippedOverlap且不补跑，人工触发展示当前 active Run。定时创建时 Runtime 离线则相应检查 RuntimeUnavailable、其他继续。重试分析引用同一 Run，重新采证创建新 Run/evidence_at并以 rerun_of 引用旧 Run。
_Avoid_: 巡检计划、巡检报告、混合采证/分析状态、Succeeded=健康、离线补跑、Runtime 队列、跨时间追加

**执行尝试（Execution Attempt）**：
Plinth 对同一个任务或 Run 的一次底层执行。Quoin 派发前持久化 attempt ID；Runtime 有 boot ID、递增 connection epoch、明确接受和有限 lease。lease 内同 boot 重连上报 active Attempt 调和而不重派；新 boot、lease 到期、身份吊销、崩溃或替换使 Attempt `Interrupted`。结果按 attempt ID 幂等，旧 epoch 迟到结果只审计。用户取消携带 command ID 与 expected version，Quoin 先事务提交 cancellation fence；Queued/Assigned 直接 `Cancelled`，Running 先 `Cancelling`，Runtime 确认或取消 lease 到期后 Cancelled。成功与取消按 SQLite 提交顺序裁决：成功先提交则取消返回已完成；取消先提交则迟到结果不产生有效消息、Report 或 Candidate。取消前已提交 Evidence/Tool/Artifact 保留为部分结果；取消 Run 停止未开始及运行子 Attempt但不删除已完成检查，页面/SSE断线/登出不隐式取消。第一版不设 Agent Attempt 总时长、调用数或产物限制；每次模型/API 调用有部署内部有限 deadline。幂等只读 API 对瞬态错误有界重试并记录物理尝试；模型只在明确可重试且未收到输出时自动重试，不切换模型/供应商。部分 token 后失败不得成为有效输出，Attempt `Failed` 并保存 Timeout/RateLimited/ProviderUnavailable/InvalidResponse/ToolError/ArtifactCommitFailed 等原因。Tool 失败可成为 Evidence 缺口，模型失败不生成成功结果。每次 Attempt 用干净工作区，成功前所有引用 Artifact 必须上传校验提交。
_Avoid_: 无限离线执行、前端取消标记、透明无限重试、部分模型输出成功、Agent 总预算规则引擎、恢复未提交工作区

**巡检报告（Inspection Report）**：
一次 Inspection Run 的一次成功模型分析形成的不可变版本，精确引用本次使用的 Evidence 集合、模型、Prompt 和 Execution Attempt，并包含程序产生的检查事实与证据缺口，以及模型产生的摘要、分析、诊断、建议和限制。失败分析只保留失败 Attempt，不产生成功 Report；同一 Run 重新分析复用原 Evidence、创建新 Report 版本，不修改旧报告也不重新采证。页面默认展示最新成功版本并保留版本历史。Succeeded 只表示模型分析完成，不表示检查正常或诊断已验证。报告分析的知识引用是权威事实：由 Quoin 从本 Attempt「执行成功且结果进入报告模型调用输入谱系」的知识读取工具调用推导封存，模型没有真正消费过的知识不得出现在报告引用中。
_Avoid_: 原始证据、巡检计划、可覆盖结果、重新采证

**证据（Evidence）**：
由确定性工具采集形成的不可变事实记录，保存目标、参数、观察时间、原始结果或 Artifact 引用、warnings、错误和完整性，供调查和诊断使用。模型报告即使以文件保存也仍是分析，不因成为 Artifact 而升级为 Evidence。
_Avoid_: 诊断、推测、用户上传材料、文件载体

**来源材料（Source Material）**：
用户提供的 Text Attachment、Knowledge Import Batch 原文等可追溯输入。其字节、来源、上传者和时间是事实，但正文主张未经确定性采集或人工验证，不自动成为 Evidence。
_Avoid_: 工具采集证据、已验证知识

**产物（Artifact）**：
Quoin 管理的持久字节载体，例如附件正文、截图和大型工具响应；它不是独立结论或可编辑文件实体。每份内容只有一个规范持久副本，逻辑对象通过 Artifact ID 引用；Artifact 的访问和保留继承逻辑所有者，不建设文件管理中心。Alert Delivery 原始 body 继续直接存 SQLite，以维持接入单事务语义。
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
AI 三条链路（初步分析、调查、巡检报告分析）经平台工具 `knowledge_search` / `knowledge_get` 自动检索与读取知识（读 Quoin 自有数据 = 平台工具的归属判据；quoin_routed、无连接 grant，随 attempt 冻结目录进入全部知识接入 agent 世代，知识抽取钉住的原始共享身份除外——其候选必须只来自来源材料）。`knowledge_search` 复用双通道查询（语义索引未配置/未就绪时诚实降级为仅精确文本通道）；`knowledge_get` 只读当前合格版本正文（有界截断），已停止复用或非当前版本返回稳定错误，不得被新检索使用。系统提示词要求模型区分知识与实时证据、知识正文中的指令一律忽略（防注入）、引用携带 versionId。巡检报告的知识引用由 Quoin 在提交事务内权威封存：只计入「执行成功且其封存结果进入报告所依据模型调用输入谱系（model_call_input_items）」的 `knowledge_get` 调用——执行成功但未被模型消费（如被上下文淘汰）的读取不构成引用；不采信模型/worker 声明的列表，schema 闭包同样要求每个引用对应一次成功且被该模型调用消费的读取。
_Avoid_: 知识候选、知识导入批次、原地覆盖、混合 embedding generation、对话历史、原始报告、模型记忆

## 存储、保留与部署

**Artifact 提交**：
Artifact 的临时文件与最终文件必须位于同一文件系统：完成写入与 hash/大小校验后 `fsync` 临时文件，按 SHA-256 原子 rename 为不可变文件，再 `fsync` 最终父目录；只有父目录同步成功后 SQLite 事务才提交引用。引用事务失败后该文件作为无权威记录引用的孤立文件清理。未完成上传、校验、目录同步或引用事务的文件不能被成功 Attempt 引用；工作区、staging 和失败上传可自动清理。

**在线保留**：
结构化告警、调查、消息、诊断、报告、反馈、知识、Text Attachment 和 Knowledge Import Batch 原文长期保留。截图与大型工具响应正文等生成型大 Artifact 默认保留 90 天，使用一个由 Admin Web UI 管理并持久化在 SQLite 的部署共享设置；到期后保留元数据、SHA-256、来源、时间和“正文已过期”状态。撤回消息、停用系统或停止复用知识不删除来源历史。备份保留 30 份是独立规则。

**敏感内容下载**：
`sensitive=1` Artifact 与备份只接受当前有效 Admin Session，不重复要求同一密码、不签发预签名或分享 URL。服务端在响应头和首字节前重验当前 User/Session/role 并提交非秘密访问审计，审计失败即拒绝；活动流绑定该 Session，Session 撤销、账号禁用或降级时立即中止剩余发送。每次 Range/续传请求都重新认证并审计，响应使用 `no-store`、`nosniff` 与 attachment disposition。

**秘密与日志**：
普通日志、指标标签、审计、持久诊断和 UI 技术详情使用字段白名单，默认不记录请求/响应 body、headers、gRPC metadata、完整 URL query 或任意对象 dump。秘密类型不可被普通字符串化，只能输出固定 `[REDACTED]`；外部适配器先映射稳定错误码与允许字段，再进入日志或数据库。验收向 Cookie、Authorization、密码、API key、根密钥标记和 provider 回显注入唯一值，并扫描四组件 stdout/stderr、结构化日志与 telemetry，任一命中失败。用户主动上传文本不做通用猜测式扫描，但不得被普通 logger 复制。四组件只向 stdout/stderr 输出 UTF-8 JSON Lines，不写或轮转容器内日志文件；固定字段至少包含 UTC timestamp、level、component、release、稳定 code 与 message，可带非秘密 correlation ID。

**运行配置权威**：
Admin 可理解的运维设置由 Admin Web UI + SQLite 作为唯一权威，包括备份时间、时区、保留份数与生成型 Artifact 保留天数；部署与每进程配置只保存启动时不可推导的非秘密基础设施事实，二者字段不得重叠。镜像选择是 Release manifest 中不可变 digest 的投影，不是部署者可随意改写的配置。配置在进程启动时读取且整个进程生命周期不可变；变更以显式零重叠重启生效，不提供 watcher 或 SIGHUP reload。

**进程配置输入**：
`contracts/schemas/deployment-config.schema.json` 继续是 Quoin、Plinth、Stele 各自非秘密进程配置的机器权威；每个进程使用固定只读 YAML 配置和独立的只读秘密文件路径，拒绝未知字段。该 Schema 不再规定 Helm 安装投影或复杂部署生成流程；不得为同一字段建立环境变量、重复 CLI flag 或优先级。

**公开入口与运维端点**：
Caddy 是唯一公共入口，加载部署者提供的 TLS Secret（Compose 使用等价只读证书文件）并终止 TLS；不强制 Ingress Controller、cert-manager、ACME 或公网证书。Caddy 在单一 public Origin 下按明确优先级转发前端页面、Quoin API/SSE 与 Stele 入口，SPA fallback 不得吞掉 API、认证、实时或告警请求。Quoin、Plinth、Stele 仍分别提供不进入 OpenAPI、不做应用认证的内部 `/livez`、`/readyz`、`/metrics`；运维端口不得公开。

**服务暴露拓扑**：
五个服务角色为入口 Caddy、前端静态服务、Quoin、Plinth、Stele。前端独立构建、发布并实际提供生产静态文件，不运行开发服务器，也不通过共享卷把资源交给 Caddy 托管；开发服务器仅可在开发环境代理后端请求。Kubernetes 以直接可审阅、可应用的普通 YAML 交付，入口 Service 的暴露类型由集群网络条件决定；Compose 作为简单辅助部署提供同一服务角色。Plinth 只主动出站连接 Quoin，Runtime gRPC 与所有 ops 端口不进入公共入口。既有内部端口、卷、Secret、健康检查与数据归属边界保持不变。

**指标机器契约**：
`contracts/metrics.yaml` 是 Quoin、Plinth、Stele 自定义 metric family 的唯一机器权威，定义 family name、type、HELP、label names 与封闭 label values；只由 metrics 拥有的枚举可以在其中定义，maintenance reason、Runtime outcome、Attempt kind 等已有机器权威的集合必须引用 `schema.sql`/`runtime.proto` 等所有者，并由 fixture 断言投影集合严格相等。HTTP 指标只使用封闭 `route_group`、`method`、`status_class`，gRPC 只使用封闭 `rpc_group` 与 canonical status code；完整 URL、path/query 值、用户、对象 ID、每个 OpenAPI operation 与动态错误文本不得成为 label。每组件从启动起导出无 label 的 `<component>_ready`；Quoin 另导出无 label 的 `quoin_accepting_work` 与 `quoin_maintenance{reason=<schema.sql 权威集合>}`，精确 not-ready reason 只在 `/readyz` 固定 JSON 与 JSON 日志中出现。所有预知序列启动即显式导出 0。`*_total` 是允许进程重启归零的内存 counter，不扫描 SQLite 历史或增加指标持久表；active/in-progress/firing/slot/ready 等 gauge 才从当前 SQLite 或内存权威投影。

**Prometheus 告警规则**：
`operations.md` 定义最小推荐规则；普通 Kubernetes YAML 和 Compose 不创建 Grafana dashboard、通知器或 Alertmanager 路由，也不强制 Prometheus Operator。通知与路由继续由部署者现有监控栈拥有。

**SQLite 运行耐久**：
锁定的 SQLite 构建使用 WAL 与 `synchronous=FULL`；FULL/NORMAL 不暴露为部署开关。每条连接仍必须在执行领域 SQL 前设置并读回 `foreign_keys=ON` 与 `recursive_triggers=ON`。恢复首先以完整发布的 manifest + checksum 为门禁，再附加执行 `integrity_check` 与 `foreign_key_check`；PRAGMA 成功不能替代 manifest 完整性。

**一致备份**：
自动备份通过独立空闲 SQLite 连接执行 `VACUUM INTO` 生成单文件一致快照，再从快照枚举精确 Artifact hash 集合；Artifact GC 与复制阶段互斥。备份复制校验快照引用的 Artifact，最后以“临时文件写入并 `fsync` → 原子 rename → rename 后 `fsync` 父目录”的顺序耐久发布 DB/Artifact SHA-256 manifest；任一引用缺失或目录同步失败整次失败，新备份校验成功后才清理超出 30 份旧备份。FTS5 与 embedding 不是恢复业务事实所必需的权威数据；采用同库布局时，FTS5 shadow tables 与 embedding BLOB 会随 `VACUUM INTO` 物理进入快照，恢复后可校验、丢弃并重建。Attempt 工作区与临时文件不进入备份。备份归档不做应用层整体加密；其中连接凭据字段仍保持自身 AEAD envelope，其余内容的机密性由独立 PV/目录权限、存储层加密、传输与 Admin 下载边界负责，manifest/checksum 只负责完整性。Admin 可浏览、显式下载和立即触发备份，下载审计；恢复只由拥有 PVC/数据目录和根密钥 Secret 权限的部署操作者在 Quoin 停机时执行，Web Admin Session 不是恢复权限。恢复实现只复用同一 Release 的 Quoin 镜像与二进制中的 `quoin restore` 子命令：Kubernetes YAML 用一次性 Pod/Job 包装，Compose 用 `docker compose run --rm` 包装；不要求宿主机安装第二套 CLI，也不维护独立 restore 镜像。操作者先停止业务工作负载，再挂载数据卷、备份卷和根密钥文件；TTY 只承担已定案的恢复 Admin 选择与临时密码一次显示。备份目录只允许 Quoin UID 与部署操作者访问，Artifact 路径仅由 hash 推导。恢复、升级和根密钥 rebind 复用 SQLite 中的单行维护状态与按对象维护清单；离线工具在发布恢复库前的最后事务写入，恢复事务同时清除全部 Web Session、retire 全部告警源 Bearer、禁用除 TTY 选定恢复 Admin 外的用户并给该 Admin 设置强制改密的临时密码、把全部 Connection 置为不可派发的 `RevalidationRequired`，并写 system Audit Event；数据库外的组件客户端证书与 CA 不因普通恢复改变。维护期间只开放登录/登出/当前用户/改密、维护与健康诊断读取、Admin 信任重建操作及退出维护，普通任务、告警接入、调度、SSE 与业务下载上传统一拒绝。恢复退出按“安全收口”而非“全部能力 Ready”裁决：用户必须已重新启用或保持禁用，Connection 必须重验/重录或保持 disabled，告警源必须有新凭据或保持 disabled 即为安全；这些隔离状态由恢复事务先建立，因此不强迫恢复可选能力。升级使用独立版本/迁移清单，不重做恢复身份清单。退出由 Admin 以 command ID 与 expected maintenance row version 显式提交并在同一事务重验全部阻塞项，不提供通用绕过。

**备份运行状态**：
`backups` 是可查询的受限状态机聚合，而不是只能在结束时追加的终态记录：状态为 `queued|running|succeeded|failed`，阶段为 `queued|preflight|database_snapshot|artifact_copy|manifest_publish|completed`，触发来源为 `manual|scheduled|upgrade`；只允许相邻前向迁移，终态后不可修改且任何状态均禁止 DELETE，失败必须记录稳定 `error_code`、`retryable` 与有界详情；任一时刻最多一个 active backup。立即备份先持久化 active row 再返回 202，同一 `client_command_id` 重试返回同一 row，其它手动触发返回携带 active ID 的冲突；定时触发与 active run 合并，不排第二份；升级前备份使用同一聚合并标记 `trigger_kind=upgrade`。Quoin 启动时必须先把上一进程遗留的 `queued|running` 行收敛为 `failed`，记录稳定的进程中断错误码和结束时间，然后才开放新触发。停机错过多个计划时最多补最新一份，不逐个回放；失败不删除旧成功备份，下一个正常周期继续；第一版不提供取消备份。

**Artifact GC 协调**：
唯一 Quoin 进程内有一个 artifact-storage coordinator；备份从快照枚举到复制完成期间独占，GC 只执行有界小批次，备份到达后完成当前批次即让出。GC 在启动后和固定周期唤醒，健康运行时保证到期对象 24 小时内处理；周期与 batch size 是内部调优，不进入 Admin 或部署配置。单 Quoin + 数据目录进程锁已经排除第二 writer，不增加 SQLite lease、独立 GC 进程或 sidecar。

**单组件拓扑**：
第一版固定 Quoin、Plinth、Stele 各一个 active replica，不提供 replicas 或 HPA。Kubernetes YAML 与 Compose 均采用零重叠替换；Quoin、Plinth 的既有状态目录锁、卷隔离、SQLite 存储限制与 Artifact 同文件系统原子发布边界保持不变。Caddy 与前端的独立服务角色不改变这些有状态组件的单实例约束。

**发布架构与镜像运行时**：
Quoin、Plinth、Stele、前端是四个应用镜像；Caddy 使用固定版本的第三方镜像。发布版本、镜像 digest 与来源信息继续用于展示、审计与溯源，但不是 Runtime 通信准入条件。任一 Proto 权威契约文件变化必须重建全部应用；OpenAPI 变化必须重建 Quoin 与前端；仅应用实现变化且契约不变时可只重建受影响应用。双架构、镜像锁、非 root 运行、持久卷、工具隔离及供应链验证要求继续适用。

**首次秘密引导**：
部署秘密（根密钥、部署 CA 与内部 TLS/组件客户端证书）由部署方在目标环境外用仓库脚本 `scripts/generate-deployment-secrets.sh`（或等价 PKI 流程）生成，再以 kubectl 创建/更新 Kubernetes Secret 或放入权限受限的本地目录，正常容器只读挂载；产品进程不在集群内生成或改写任何部署秘密。已有持久状态而秘密缺失、部分存在或无效时必须人工恢复原材料，升级不得自动重建；秘密不得进入镜像、前端资产、部署 YAML、环境变量、发布记录或日志。

**组件身份供给**：
Plinth 与 Stele 的客户端证书/私钥经部署 Secret 只读挂载，配置文件指向挂载路径；部署方用 `scripts/generate-deployment-secrets.sh` 首装生成，其 `--issue-client-certs`（可加 `--force`）为存量部署补签或轮换。证书不得进入镜像、版本控制、日志或模型上下文。组件启动即认证，无注册流程（ADR-0009）。

**发布工件权威与分发**：
`release-manifest.json` 记录四个应用镜像的独立发布版本、不可变 digest、来源、依赖锁、Compose 与离线资产、签名和验收摘要。发布运行清单只消费 digest，不使用 `latest`；删除 Helm/Chart 工件不移除适用的镜像、Compose、离线、SBOM、provenance 或 Sigstore 完整性校验。

**安装、运维与恢复操作者路径**：
主要安装路径是普通 Kubernetes YAML，Compose 为辅助路径；两者提供同一五服务角色，且不要求 Helm、Chart、values 或复杂生成器。Kubernetes 生命周期由部署者使用普通 `kubectl` 管理清单，并按需运行同一 Release Quoin 镜像的一次性 `quoin admin create`、`quoin backup --offline`、`quoin restore` 或 `quoin migrate`；不承诺 `quoin-deploy kubernetes install|backup|restore|upgrade`。在线备份和其他产品写操作仍由已登录 Admin 通过 UI 发起。现有 `quoin-deploy kubernetes verify` 仅是 catalog 驱动的 qualification 入口；Compose 的辅助命令面不扩展为 Kubernetes DSL。

**协调升级**：
跨 Proto 契约升级必须协调更新，不承诺零停机滚动升级；发布版本不同本身不阻止已满足其他前提的组件通信。Kubernetes 升级由部署者以 `kubectl` 按维护、备份、迁移和 rollout 的既有安全顺序执行；新 Quoin 启动后，只有 Proto 契约指纹一致且认证/安全前提满足的 Plinth、Stele 才能 Ready。接受新写入后的回退必须显式恢复升级前备份，不得只回滚镜像。

**Compose 生命周期**：
Compose 提供 Caddy、前端、Quoin、Plinth、Stele 五服务的简单配置。后端服务可使用 `restart: unless-stopped`；运行中 Quoin 重启仍只依赖各组件自己的重连与 Ready 契约，不得隐式传播为全栈重启。前端生产服务不运行开发服务器。

**健康语义**：
Quoin 取得数据目录锁并完成 migration 前不 Ready；maintenance 时仍以 `mode=maintenance`、`acceptingWork=false` 表达安全隔离。Plinth 只有在 token、Proto 契约指纹和控制流被 Quoin 接受后 Ready；Stele 只有在同一指纹握手及告警凭据 digest 快照加载成功后 Ready。`/livez` 只检查本进程可推进性，`/readyz` 检查组件职责；其固定响应形状由 `contracts/schemas/readiness-response.schema.json` 独占。

**优雅关停**：
Kubernetes `terminationGracePeriodSeconds` 与 Compose `stop_grace_period` 统一为 60 秒。四组件收到 SIGTERM 后立即停止新准入并进入 draining，最多使用前 45 秒完成当前 SQLite 事务、Artifact 原子发布、in-flight HTTP/Stele 请求、Runtime GoAway/结果确认，至少留下 15 秒关闭连接和退出。不得等待任意长的模型工作自然完成；未完成工作按既有 fence/reconcile 语义收口，满足续走条件的调和继续，否则才进入 Interrupted/技术终止，不得强制打断可调和 Attempt或伪装成 Cancelled/Success。清单不使用 sleep 型 preStop hook，核心关停只由幂等 SIGTERM handler 实现。

**资源边界**：
第一版不默认设置 CPU/内存 limits，Kubernetes YAML requests/limits 可选且默认空；Compose named volume 不施加应用层容量限制。Kubernetes PVC 容量直接影响数据安全，必须由部署操作者显式填写。Quoin 不以任务数、Artifact 大小或静默删历史实现应用层资源配额，也不定义统一剩余百分比阈值。已知大小的 Artifact/备份操作必须针对目标目录和所需字节做精确 preflight；任何 `ENOSPC`、`EDQUOT`、`EROFS` 或持久化 `fsync`/rename 失败都把对应 storage health 置为不可写、使 Quoin `/readyz` 失败、令 `quoin_storage_writable` 为 0，并在 SQLite 仍可写时把当前领域任务或 Backup Run 持久化为稳定失败。恢复必须在同一目标目录通过真实 create→write→fsync→rename→父目录 fsync→unlink probe，且当前操作的精确 preflight 通过；不得靠百分比自动清除、静默删历史、预留隐藏文件或自动扩容。

**部署、恢复与升级验收**：
CI 必须在一次性真实 Docker Compose 环境和真实 Kubernetes 测试集群（kind 或等价）分别完成安装与运行验收，不能用 template/lint/build 替代。最小矩阵必须在原生 amd64 与原生 arm64 各自覆盖：四个应用镜像按多平台 index digest 启动、探针与 metrics scrape、首次自动秘密引导及既有数据缺秘密 fail-closed、首次 Admin 登录、Plinth Bash/固定工具目录与 Landlock/seccomp 对抗自检、Stele Delivery、含 Artifact 的成功备份、停机恢复、恢复后的 Session/Runtime/告警凭据失效、从上一正式 Release 动态生成数据后升级、接受新写入前回滚、容器/Deployment/Compose service 重建并复用既有 PVC/named volume 后数据仍在、SIGTERM 关停、存储故障与 metrics sentinel 泄漏扫描；另必须实际验证离线归档签名、解包、registry 导入和 digest 读回。普通 Kubernetes YAML 解析/应用、Compose config、OpenAPI/SQL/proto 校验继续作为更低层门禁，但不能据此声称真实安装、恢复或升级通过。

**验证声明分层**：
历史上的验证分层（Contract Gate / Release Qualification / Deployment Acceptance）已随验证驱动与部署编排退役：当前可执行验证是 `go test`/`go vet`、产品内 Config Verification Run（prepublish 机械采证）与发布物签名验证。Release Qualification 矩阵、站点级 Deployment Acceptance（manifest/helper/receipt 机制）在驱动重建前不再执行；相关 DB 表与 API 契约已删除。

**验证规范与目录权威**：
各领域稳定条款继续独占行为断言；`verification.md` 只拥有执行层级、环境矩阵、故障编排、证据规则与 verdict 聚合的规范语义，并按稳定条款 ID 引用所有者，不复制字段、枚举或行为正文。`contracts/verification-catalog.yaml` 及其 Schema 是跨域 scenario 登记的唯一机器权威，只拥有条款组合、前置条件、环境能力、故障原语、可观察断言、必需证据与清理要求；测试实现必须声明 scenario ID，CI 机械检查 required scenario 无缺失、重复或悬空引用。

**发布验证证据**：
发布结果复用 in-toto Statement v1 与 Test Result predicate v0.1 的一次 suite invocation 语义，并由严格 Quoin profile 约束。subject 只绑定被验收的不可变 Release 输出，不把随后生成且反向引用证据的 Release manifest 自身列为 subject；configuration 绑定验证目录、环境描述与工具锁；passed/warned/failed 名单只接受 scenario ID。DSSE 信封签名及 Sigstore bundle 复用现有发布链，Release manifest 通过既有 `validation.<category>.evidence_sha256` 逐分类单向绑定对应证据 bundle，禁止建立哈希自引用、顶层竞争字段或第二发布索引。JUnit、HTML、CI annotation、日志和截图都只是同一权威结果的投影或 digest 附件。

**验证 verdict 与证据纪律**：
Suite 状态为 `PASSED | WARNED | FAILED`，发布门只接受 PASSED：本 invocation 的全部 applicable required scenario 必须首次执行通过且无跳过/警告；基础设施中断、结果不确定或诊断重跑后才通过均为 WARNED；产品/契约断言失败为 FAILED。每次重跑创建新 invocation，旧结果不可覆盖；不适用只能由 catalog 前置条件机械判定并记录理由。每个 scenario 的结构化 evidence index 记录 invocation、时间、环境 digest、工具版本、脱敏 argv、exit code、逐断言 expected/actual/result、附件 SHA-256 与清理结果。v1 catalog 不再包含人工观察 scenario；历史 UI 观测条款随浏览器运行时移除，不建立人工 checklist verdict。

**验证环境与职责边界**：
Release Qualification 只使用合成数据、短期测试凭据与唯一 sentinel，禁止生产凭据和生产数据进入流水线；公开 evidence 必须通过 sentinel/秘密扫描，敏感 trace 只留受限存储并在公开报告记录 digest、分类和受限 locator。纯状态机、SQL 约束与 HTTP/Proto framing 可以由确定性 harness 证明；调度、Pod/PV/NetworkPolicy/Ingress/Compose 生命周期必须真实运行；双架构声明必须来自原生 amd64/arm64，QEMU 只能作为构建或辅助诊断证据。确定性程序收集事实、校验契约、计算 scenario/suite verdict 并生成签名报告；Agent/模型只做失败分析、汇总和建议，不得改写结果、降低 required 门或把 WARNED/FAILED 改成 PASSED。

**验证触发与支持矩阵**：
验证分层保留为设计目标，但其 CI 执行链（原 release.yml / release-qualification.yml 及 quoin-deploy/qualify/aggregate 驱动）已随部署编排退役一并移除；当前可执行验证收敛为 `go test`/`go vet` 与发布物签名验证；验证驱动（quoin-verify/quoin-faultfs 及其 catalog）已随本决定移除，Release Qualification 矩阵在驱动重建前不再执行。

**外部系统与故障执行**：
Release Qualification 使用官方 digest-pinned Prometheus、Alertmanager、Thanos 镜像验证真实协议 happy path、查询语义和 webhook；错误码、半响应、畸形响应及应用层响应超时由 deterministic protocol fixture 拥有，传输层 TCP timeout/reset 由网络故障原语拥有；Model Provider 继续只使用 deterministic fixture，真实客户系统、生产凭据和真实模型供应商只属于 Deployment Acceptance。catalog 只声明工具无关的封闭故障原语：已有执行路径的进程、资源与网络原语映射到 Docker/Kubernetes 原生 stop/kill/pod delete/NetworkPolicy/重建操作，TCP 原语映射到 digest-pinned Toxiproxy 的 latency/timeout/reset_peer/bandwidth/limit_data；v1 不引入通用 Chaos 平台。ENOSPC、EDQUOT、EROFS、指定 fsync 失败和指定 rename 失败是互不替代的精确 required 原语；冻结 catalog 前必须通过一次性 Compose+Kubernetes 原型逐项证明 operation、注入点、所需 privilege、expected errno 与清理路径，未证明项不得以聚合“原子写失败”或 mock 冒充。

**Invocation 隔离、执行与清理**：
每次 invocation 使用唯一 Kubernetes namespace/Compose project、独立业务卷和测试身份；共享宿主只为 host-published ports 分配唯一值，内部容器端口固定，普通场景共享只读不可变 image digest，只有离线导入场景创建 invocation-local 临时 registry 及独立数据卷并整体销毁。teardown 前只内容寻址持久化会随环境销毁的原始附件；teardown 与资源归零检查完成后才计算 verdict、生成并签名该 invocation 唯一最终 Test Result bundle。正常 teardown 后 invocation 拥有的 Pod、Job、Service、Secret、PVC、volume、network、container、临时文件和临时 registry 数据必须机械证明归零；产品/helper 遗留为 FAILED，CI/集群故障导致无法判断清理结果为 WARNED。runner 不 fail-fast：失败后继续所有相互独立的 required scenario，依赖失败项以稳定 causal ID 记录 not_run；required 断言失败为 FAILED，因环境或前置失败不能执行为 WARNED，teardown 始终执行，失败自动重试不得隐藏先前结果。diagnostic scenario 只由已持久化的 FAILED/WARNED/not_run（未完成统一表示为 not_run）触发并携带 causal result ID；它不进入 required 分母或 suite verdict，不得改写触发结果，新 invocation 才能重验。

**证据保留、人工观察与非功能门**：
与 verdict 有关的脱敏、签名、digest 绑定 evidence bundle 与 manifest 随 tag/Release 永久保留，runner 机原始目录在所需附件上传成功后删除；敏感原始 trace 不得成为 verdict 唯一证据，需要短期保留时先上传受限私有 CI artifact storage 并按部署方策略到期删除，判定所需事实必须先提取为脱敏结构化证据。v1 required 非功能集只包含既有契约明确的 deadline/队列/并发不变量、确定性竞争交错以及 invocation 所有资源精确归零；goroutine、FD、延迟、吞吐、CPU、内存先按固定采样点记录趋势，不阻塞发布，只有后续明确固定 workload、pinned runner、静默窗口、采样规则和数值预算后才能升级为 required gate。catalog 使用封闭 capability 词表（至少 deployment、architecture、Kubernetes exact version、Docker/Compose exact versions、privilege/fault backend、external stack），scenario 声明 required cells 与合法不适用条件；mandatory cell 缺能力在 preflight 形成 WARNED，禁止运行时自由字符串 skip，catalog 明确排除的无意义组合不进入分母。

**验证执行图与覆盖根**：
catalog scenario 是最小可独立裁决、重试、留证和清理的原子；只允许 `setup/action/assert/teardown` 四阶段，同层 `depends_on` 形成 DAG，低层 `proof_refs` 只能引用严格更低层且同一 tag qualification invocation、同一 source/catalog/contract/Release subject 闭包的结果。上层 scenario 仍必须真实执行自身 action/assert；已声明的 proof_ref 在同 tag 闭包缺失只能使上层 WARNED，空 proof_refs 表示没有下层 prerequisite、不是证明缺失；低层 FAILED 则上层 FAILED。稳定 validation root 只扫描 `*-VALIDATION-*`、`OPS-VERIFY-*`，catalog 必须覆盖全部 declared root；构建门机械拒绝未覆盖/悬空/重复 ID、无实现、依赖环、跨层同级依赖、非法 proof、cell applicability 不闭合以及 Deployment Acceptance timeout 超过 freshness budget。scenario ID 一旦发布只能退休，语义变化必须新 ID，禁止旧 ID 换实现或换断言后继续复用。

**验证故障与竞争执行**：
事务竞争必须由显式 barrier、fence 和 scheduler trace 精确执行已声明 interleaving；固定 seed 状态机生成只作诊断补充，不能替代显式 required 交错。存储故障必须分别精确注入 ENOSPC、EDQUOT、EROFS、指定 fsync 失败和指定 rename 失败；v1 使用直接基于 go-fuse v2.9.0 loopback API 的最小 verification-only `quoin-faultfs`，只提供 path-scoped write/fsync/rename→errno 与 mount/unmount，不建设通用故障平台。原生 linux/arm64 原型已观察五种故障与解除注入后的恢复；最终双架构 Compose/Kubernetes required cell 仍各自重跑。上游 toda v0.2.4 无 arm64 维护或工件，不得作为执行器/fallback；未证明项不得 required。网络故障使用项目控制的 Toxiproxy/NetworkPolicy，DNS、TLS 与 HTTP/SSE/gRPC framing 使用各自 fixture，禁止所有权重叠。session、cooldown、lease、scheduler、expiry 与前端 timer 使用模块内部 Clock/Timer 接缝，确定性测试禁止 wall-clock sleep；真实 timer smoke 只留 Release Qualification。

**发布高层场景**：
Release Qualification 以真实业务旅程为高层 scenario；普通 happy-path 不重复执行已经由 tag invocation Contract Gate 证明的状态机、权限和竞态，只通过严格闭包的 `proof_refs` 绑定。浏览器自动化与 UI 观测场景已随浏览器运行时一并移除，v1 不再声明任何 browser evidence 或 `ui_observation` 要求。


**Connection Probe 与资格选择**：
Model Provider 探测清洁重构为唯一 `connection_probe` Execution Attempt；`contracts/connection-probes.yaml` 及其 Schema 是三类 action-set/version 的唯一机器权威，catalog 只引用 digest。probe 使用封闭 grant purpose，冻结 current revision/credential generation/root binding，由 Plinth supervisor 直接执行，不启动 worker/Agent/ReAct；`connection_probe_results` header 与按类型 typed child 拥有事实，不另建 Run 生命周期，旧 `model_provider_capabilities` 删除且不保留兼容层。Model Provider probe 保留完整 streaming/tool/cancel/usage/request-id/embedding 序列，并且只有 Model Provider Connection 使用显式 `qualified_probe_result_id` 作为 enable 和普通 Model/Embedding grant 的强制资格闭包，任何 pair/root/probe-contract 语义变化都 fail-closed 并要求重验。Thanos 不建立长期资格 pointer，也不增加普通派发前置；其 fresh probe 主要是 invocation-scoped 站点证据，当前 root revalidation 可显式引用成功结果。Thanos Plinth probe 只证明 Plinth Tool 的固定 `vector(1)` 路径，Quoin PromQL 路径由 Config Verification 单独证明。

## 任务终态提示

**后台任务提示**：
不建设邮件、浏览器 Push、通知数据库、铃铛收件箱、已读/未读或 per-user 未查看状态。Initial Analysis、Investigation Attempt、Inspection Run、Knowledge Import Batch 等权威对象的终态持续保留在原对象、模块列表和详情中；模块徽标只能由当前可操作权威状态即时派生，打开对象不得产生“已查看”写入。用户在线时可用非阻塞 toast 提示自己发起任务的成功、失败、取消或中断；离开后返回时直接从权威对象查询结果。正常成功的定时巡检不逐次打扰；`CompletedWithGaps`、`Failed`、`RuntimeUnavailable`、`AuthenticationRequired` 等异常结果在其所属模块持续可见，只有 Admin 能处理的用户、凭据、Runtime、备份问题只向 Admin 显示。提示不复制报告正文，也不能反向修改任务状态。
