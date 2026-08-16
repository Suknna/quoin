# Quoin v1 — 安全规格（security.md）

**状态：Draft**

**CATEGORY 前缀：`SEC`**（SPEC-TRACE-002）

领域语言权威：[CONTEXT.md](../../../CONTEXT.md)

HTTP 机器契约：[contracts/openapi.yaml](contracts/openapi.yaml)

Runtime 机器契约：[contracts/runtime.proto](contracts/runtime.proto)

持久化机器契约：[contracts/sql/schema.sql](contracts/sql/schema.sql)

**Non-normative：** 本文件定义 Quoin v1 的威胁边界、用户与服务身份、Session、同源请求、授权、根密钥、可逆秘密、一次性 reveal、审计、日志、敏感下载和恢复后的信任重建。字段、路由、表与 wire message 仍分别由对应机器契约拥有，本文件只定义跨协议安全语义和失败裁决。规范条款来源为 [Issue #16](https://github.com/Suknna/quoin/issues/16) 及 [`CONTEXT.md`](../../../CONTEXT.md) 的稳定标题。

## 1. 范围与威胁边界

- **SEC-SCOPE-001 —** Quoin **MUST** 把未认证 HTTP/gRPC 输入、普通 User/Operator、Web Admin、模型输出、Plinth worker、上游 Provider/业务系统响应和浏览器页面内容视为不可信；任何一方都 **MUST NOT** 仅凭其自述扩大权限、选择凭据、改写审计或绕过领域状态机。（来源：Issue #16、CONTEXT「权限」「模型调用边界」「Plinth worker 隔离边界」）
- **SEC-SCOPE-002 —** 部署操作者对 Quoin 数据目录/PVC、只读根密钥挂载、Runtime 状态卷、Stele service token 与 TLS 终止层拥有基础设施权限。v1 的应用审计 **MUST** 防御应用用户和 Web Admin，**MUST NOT** 声称能够阻止拥有这些基础设施权限的操作者离线读取或改写本地数据。（来源：Issue #16 Q16.2/Q16.18、CONTEXT「审计与执行溯源」「一致备份」）
- **SEC-SCOPE-003 —** v1 **MUST NOT** 建设本地 Audit hash chain、外部 WORM、强制 KMS/Vault、应用层网络 allowlist 或通用 DLP；这些机制只有在存在独立外部信任锚或真实执行路径时才可由后续版本引入。（来源：Issue #16 Q16.1/Q16.17/Q16.18）
- **SEC-SCOPE-004 —** 网络可达性、TLS 私钥、HSTS、PV/目录权限、存储层加密和反向代理访问日志由实际部署层拥有；Quoin **MUST** 对自己产生的身份、授权、秘密、Cookie、响应头、审计和日志承担完整契约，**MUST NOT** 以“可由 ingress 处理”为由省略应用知道且能裁决的安全事实。（来源：Issue #16 Q16.2/Q16.9）

## 2. 本地账号与密码

- **SEC-PASSWORD-001 —** 密码建立、修改、临时密码转正式密码与离线重置 **MUST** 先按 NFC 规范化，再对完整值执行 15–128 Unicode 字符检查；密码 **MUST NOT** 被截断，且 **MUST NOT** 强制大小写、数字、符号组合或周期更换。（来源：Issue #16、CONTEXT「本地账号认证」）
- **SEC-PASSWORD-002 —** 密码建立和修改 **MUST** 将完整 NFC 值与内置 blocklist 及产品名、规范化用户名、显示名等上下文值比较，**MUST NOT** 对密码子串作字典猜测；登录校验 **MUST NOT** 再执行 blocklist。（来源：Issue #16 Q16.4、NIST SP 800-63B-4 3.1.1.2、CONTEXT「本地账号认证」）
- **SEC-PASSWORD-003 —** 内置 blocklist **MUST** 固定 SecLists `Passwords/Common-Credentials/100k-most-used-passwords-NCSC.txt` 于 commit `1a7bb9127eca9e6ff2fc0301c597fe6e16a0cb56`；原始文件为 835538 bytes、99840 行、SHA-256 `c2e5696882c603b76bb67a47ee970897e5a76fc4c3f5547abe3d0ca340c576e0`。该精确工件随 Quoin Release 嵌入，构建/发布验证必须核对 provenance；运行时 **MUST NOT** 联网查询 HIBP 或其他服务。（来源：Issue #16 Q16.4、SecLists MIT 上游 GitHub）
- **SEC-PASSWORD-004 —** 密码 PHC **MUST** 使用 Argon2id，最低参数为 `m=19 MiB,t=2,p=1`；只有在真实部署硬件上按目标并发完成基准后才 **MAY** 提高参数。验证器 **MUST** 接受现有合规 PHC 并可在成功登录后升级参数，**MUST NOT** 降低已有参数。（来源：CONTEXT「本地账号认证」、OWASP Password Storage）
- **SEC-PASSWORD-005 —** 用户不存在、密码错误和账号禁用 **MUST** 返回同一 401 响应；不存在用户 **MUST** 执行等价 dummy Argon2id 工作。密码、PHC、候选 blocklist 命中值与 dummy hash **MUST NOT** 进入日志、审计、指标标签或错误详情。（来源：CONTEXT「本地账号认证」）
- **SEC-PASSWORD-006 —** 单 Quoin 进程 **MUST** 以有界内存表按规范化用户名执行“15 分钟内失败 5 次后冷却 15 分钟”，不存在用户走同一路径；表满时 **MUST** 淘汰最旧状态并继续受全局登录速率与 Argon2 并发门保护，**MUST NOT** 把匿名用户名持续写入 SQLite。进程重启清零是 v1 接受的边界。（来源：Issue #16 Q16.5、CONTEXT「本地账号认证」）
- **SEC-PASSWORD-007 —** 离线创建/重置 Admin 的临时密码只可经 TTY 读取或一次显示；Web Admin 为其他用户重置时，临时密码只可作为受同源与 Session 保护的请求秘密接收，API **MUST NOT** 回显或保存明文。两条路径都 **MUST NOT** 把临时密码放入参数、环境变量、命令历史、审计或日志；首次登录后的 Session 保持受限，直到同一事务成功保存正式密码、清除强制改密状态并更新 Session revision。（来源：CONTEXT「管理员离线恢复」「本地账号认证」）

## 3. Session、同源请求与浏览器响应

- **SEC-SESSION-001 —** 浏览器 Session raw bearer **MUST** 是 CSPRNG 生成的 32-byte opaque 值，只通过 `__Host-quoin-session` Cookie 传输；Cookie **MUST** 设置 `Secure; HttpOnly; SameSite=Lax; Path=/`，**MUST NOT** 设置 `Domain`，HTTP API **MUST NOT** 接受 Authorization、查询参数或请求体中的 Session bearer。（来源：CONTEXT「同源 Web 会话」、HTTP-AUTH-001）
- **SEC-SESSION-002 —** SQLite **MUST** 只保存 Session bearer 的 32-byte SHA-256 digest；比较 **MUST** 使用固定长度、无早退的实现。登录成功必须签发新 bearer；登出、撤销、过期、账号禁用或 auth revision 不匹配后，该 bearer **MUST** 在全部 HTTP、SSE 与 WebSocket 入口立即失效。`revoked_at` 只允许由 NULL 前进到时间戳且不可清除；绝对期限创建后不可延长，活动时间与空闲期限只能同步前进且不得越过绝对期限，已撤销 Session 不得再刷新活动窗口。（来源：CONTEXT「同源 Web 会话」、DATA-AUTH-003/005）
- **SEC-SESSION-003 —** 受保护请求 **MUST** 每次读取当前 User 的 enabled、role 与 auth revision；前端隐藏入口、Session 创建时角色快照和已经建立的长连接 **MUST NOT** 替代当前授权检查。写事务 **MUST** 在提交前与业务写同事务复核。（来源：CONTEXT「本地账号认证」「权限」、HTTP-AUTH-003、DATA-TX-002）
- **SEC-CSRF-001 —** 全部浏览器写请求 **MUST** 先经过 Go `CrossOriginProtection`。携带 Session Cookie 的非安全方法若 `Sec-Fetch-Site` 与 `Origin` 同时缺失，或 `Origin` 存在但不精确等于规范公共 Origin，**MUST** 在读取业务正文前返回 403；v1 **MUST NOT** 提供 Cookie CLI 例外或同步 CSRF token。（来源：Issue #16 Q16.8、CONTEXT「同源 Web 会话」）
- **SEC-CSRF-002 —** `login` **MUST** 另执行认证前同源门：`Origin` 存在时只接受精确公共 Origin；没有 `Origin` 时只接受 `Sec-Fetch-Site: same-origin`；两者都缺失以及 `same-site|cross-site|none` **MUST** 返回 403，且 **MUST NOT** 执行 Argon2id 或建立 Session。（来源：Issue #16 Q16.14）
- **SEC-CSRF-003 —** noVNC WebSocket 升级 **MUST** 校验规范公共 Origin、当前 Session、当前 User 与 operation 发起者；带凭据跨域 CORS 与跨域 SSE **MUST NOT** 启用。公共 Origin **MUST** 来自单一部署配置，**MUST NOT** 由不可信 Host/Forwarded 头临时推导。（来源：CONTEXT「同源 Web 会话」、HTTP-NOVNC-002）
- **SEC-HEADER-001 —** Quoin HTML 与受保护响应 **MUST** 设置应用拥有的 CSP、`X-Content-Type-Options: nosniff` 与 `Referrer-Policy: no-referrer`；CSP **MUST** 以 `default-src 'self'` 和 `frame-ancestors 'none'` 为基线，只按锁定前端/noVNC 的真实资源需要显式放行，**MUST NOT** 使用 wildcard、`unsafe-eval` 或内联 script。（来源：Issue #16 Q16.9、OWASP HTTP Headers）
- **SEC-HEADER-002 —** Session、秘密、敏感 Artifact、raw trace、备份及其错误响应 **MUST** 设置 `Cache-Control: no-store`；登出成功响应 **MUST** 清除 Session Cookie 并发送 `Clear-Site-Data`，实际 TLS 终止层 **MUST** 独占 HSTS 配置。（来源：Issue #16 Q16.9）

## 4. 权限与服务身份

- **SEC-AUTHZ-001 —** 权限矩阵 **MUST** 以 CONTEXT「权限」和 HTTP-PERM-* 为唯一人类/HTTP 语义；所有 HTTP 与 gRPC 入口都 **MUST** 服务端检查主体类型、当前状态、操作权限和对象归属，**MUST NOT** 依赖前端、模型、worker 或调用方隐藏字段。（来源：CONTEXT「权限」、Issue #16）
- **SEC-AUTHZ-002 —** Quoin **MUST** 拒绝禁用或降级最后一个有效 Admin；`quoin admin create` **MUST** 只接受无 `users` 行的空白库，全部 Admin 无法登录时的 `quoin admin reset-password` **MUST** 只修改已存在 Admin；两者都要求长期 Quoin 停止且独占 SQLite。部署包 **MAY** 仅按 OPS-PACKAGE-003 用 attached TTY 包装空白库创建，**MUST NOT** 提供网络 bootstrap、邮件找回或安全问题。（来源：CONTEXT「管理员离线恢复」）
- **SEC-SERVICE-001 —** Plinth、Lintel 与 Stele token **MUST** 使用封闭主体类型和最小 RPC scope；Plinth/Lintel 长期 token 只保存 32-byte digest，Stele token 只由部署 Secret 文件提供。服务 token **MUST NOT** 被 Web Admin Session、worker 或模型复用。（来源：CONTEXT「服务身份」、RUNTIME-AUTH-*）
- **SEC-SERVICE-002 —** 每个 gRPC 请求和 stream 建立 **MUST** 复核 token、slot/service 类型、release version 与当前吊销状态；吊销、slot 替换或凭据退休 **MUST** 立即关闭对应控制流、浏览器流与上传流并禁止旧 token 重连。（来源：CONTEXT「服务身份」、RUNTIME-REVOKE-001）
- **SEC-SERVICE-003 —** Runtime token 轮换在新 token 已持久化确认并提升为 current 的同一原子切换中，旧 generation **MUST** 立即进入可认证的 `retiring` 角色，保证新 token 首次认证前仍可恢复；此时用户态显示“等待新 token 首次使用”。新 current 首次成功认证后才把该旧 generation 标示为 Pending Retirement，并允许 Admin 显式退休。同一 slot 始终只有一个 active connection epoch，新连接按 Runtime 协议替代旧 epoch。系统 **MUST NOT** 按时间或一次成功自动退休旧 token；retiring 角色、Pending Retirement 状态与首次使用时间 **MUST** 持久可见并审计。（来源：Issue #16 Q16.10、CONTEXT「服务身份」）
- **SEC-SERVICE-004 —** 告警源 Bearer 轮换期间 **MUST** 最多存在两个可认证 generation；新 generation 首次成功 Delivery 后，其 supersedes 指向的旧 generation **MUST** 进入 Pending Retirement；旧 generation 只有 Admin 显式命令才可退休，**MUST NOT** 使用墙钟 TTL 自动中断 Alertmanager。（来源：Issue #16 Q16.10、CONTEXT「告警源凭据投影」）

## 5. 根密钥与可逆连接秘密

- **SEC-KEY-001 —** 部署 **MUST** 提供恰好 32-byte 根密钥的只读文件；Quoin **MUST** 在启动时读取一次并锁定于进程内存，**MUST NOT** 从环境变量、SQLite、备份、HTTP、日志或 Runtime RPC 获取根密钥。（来源：Issue #16 Q16.1、CONTEXT「根密钥与可逆秘密」）
- **SEC-KEY-002 —** `root_key_state`（机器字段由 `contracts/sql/schema.sql` 拥有）**MUST** 保存当前 binding revision、随机 verifier nonce 与固定 verifier plaintext 的 AES-256-GCM 密文；启动时文件缺失、长度错误、随机源失败或 verifier 认证失败 **MUST** 使 Quoin Not Ready，只开放无秘密 liveness/diagnostic，**MUST NOT** 开放登录、普通 HTTP、Runtime 或 Stele RPC。（来源：Issue #16 Q16.11）
- **SEC-KEY-003 —** Credential Generation envelope **MUST** 使用 AES-256-GCM、CSPRNG 96-bit nonce、格式版本与当前 binding revision；AAD **MUST** 绑定固定 domain separator、connection locator、generation sequence、connection type 与 binding revision。nonce 生成失败或 AEAD seal/open 失败 **MUST** fail closed。（来源：Issue #16 Q16.1/Q16.11、CONTEXT「根密钥与可逆秘密」）
- **SEC-KEY-004 —** Credential Generation 创建事务 **MUST** 先锁定并分配同一 Connection 的 generation sequence，再生成 nonce/envelope 并一次性 INSERT；密文、nonce、AAD 身份、binding revision 与来源历史 **MUST NOT** 原地改写。旧 generation 只保留历史，**MUST NOT** 通过改密文模拟轮换。（来源：CONTEXT「连接」、DATA-CONN-001/004）
- **SEC-KEY-005 —** 运行期单条 envelope 认证失败 **MUST** 在任何外部请求前阻止对应 grant，原子把 Connection 置为不可派发的 RevalidationRequired 并写非秘密 Audit Event；系统 **MUST NOT** 回退为空凭据、旧明文、其他 generation 或错误 binding，也 **MUST NOT** 把密文/错误上下文发给 worker、模型、UI 或普通日志。（来源：Issue #16 Q16.11）
- **SEC-KEY-006 —** 永久丢失根密钥后的 rebind **MUST** 只由停机、独占 SQLite 的离线命令执行，并要求部署操作者显式确认全部现有可逆凭据将不可再解密；命令 **MUST** 创建新 verifier、递增 binding revision、把全部 Connection 隔离为需重新录入并进入 `RootKeyRebind` 维护原因。旧密文 **MUST** 保留为历史且不得再尝试解密。（来源：Issue #16 Q16.11）
- **SEC-KEY-007 —** v1 **MUST NOT** 支持同时加载多个根密钥、在线 rewrap 或自动 rebind。普通备份恢复使用原根密钥；若恢复时已确定该密钥不可得，恢复工具 **MUST** 在发布恢复库前直接建立 `RootKeyRebind` 隔离状态（不得先进入无法安全退出的 `Restore` 再切 reason），随后按 SEC-KEY-006 rebind 和重新录入凭据。（来源：Issue #16 Q16.1/Q16.11）
- **SEC-KEY-008 —** 连接命令的 `password`/`kubeconfig`/`apiKey` 值以及可供离线猜测的 hash、HMAC 或 verifier **MUST NOT** 进入命令摘要、SQLite、审计或日志。摘要只记录秘密字段存在性和非秘密语义；同一 `client_command_id` 的秘密重放一律丢弃并返回原结果，用户编辑秘密后的新提交必须生成新命令 ID（HTTP-COMMAND-002/003、DATA-COMMAND-002/003）。（来源：Issue #16 Q16.1/Q16.17 的机械后果、Issue #9 幂等契约）
- **SEC-KEY-009 —** Helm bootstrap Job 或 Compose bootstrap 命令只可在机器证明目标 SQLite/持久卷尚未初始化时生成根密钥、Stele service token 与内部 TLS 材料；任一持久状态已存在而对应 Secret/文件缺失时必须失败并要求恢复原材料，**MUST NOT** 自动生成替代值。Helm 模板 **MUST NOT** 使用 `randBytes` 等把秘密复制进 release history；生成内容只写目标 Kubernetes Secret 或权限受限的本地 secret 文件。（来源：Issue #17 Q17.40 B、OPS-SECRET-001..003）

## 6. 一次性秘密 reveal 与轮换

- **SEC-REVEAL-001 —** 告警 Bearer 与 Runtime 注册 token **MUST** 由 CSPRNG 生成 32-byte 原始值并使用无填充 base64url 文本；SQLite **MUST** 只保存其 SHA-256 digest 或对应非秘密 generation，raw 值只可存在于创建请求的服务端内存和一次 reveal 响应。（来源：CONTEXT「一次性秘密 Reveal」「服务身份」「告警源凭据投影」）
- **SEC-REVEAL-002 —** reveal handle **MUST** 是不可猜测的内存 capability，固定 60 秒、绑定发起 Session 与 `client_command_id`、最多成功消费一次；消费时 **MUST** 重验同一 Session 仍有效且当前 User 仍为 Admin。登出、撤销、禁用、降级或进程重启 **MUST** 使相关 handle 立即失效。（来源：Issue #16 Q16.3、CONTEXT「一次性秘密 Reveal」）
- **SEC-REVEAL-003 —** 同一 Session 以相同 `client_command_id` 重放已经提交的一次性秘密命令时，若内存中的原 handle 仍有效且未消费，服务端 **MUST** 返回同一个 handle；否则只返回非秘密原对象结果和 `revealAvailable=false`，**MUST NOT** 创建新 generation 或重新生成秘密。（来源：Issue #16 Q16.12）
- **SEC-REVEAL-004 —** reveal 服务端 **MUST** 先原子保留 handle、防止并发消费，提交不含 handle/raw secret 的“reveal authorized” Audit Event，再把 handle 标记 consumed 并发送响应；审计失败时 **MUST** 释放保留且不发送秘密。一旦 consumed，即使响应在传输中丢失也 **MUST NOT** 再次读取，只能用新命令创建替代 generation。（来源：Issue #16 Q16.12/Q16.15）
- **SEC-REVEAL-005 —** handle 与 raw secret **MUST NOT** 进入 SQLite、命令持久结果、审计字段、URL、前端路由/历史、toast、clipboard telemetry、日志、指标、Artifact、模型上下文或 crash dump；HTTP 错误 **MUST** 只说明失效/过期/已消费及重新创建路径。（来源：Issue #16 Q16.3/Q16.17）

## 7. 审计、错误与日志

- **SEC-AUDIT-001 —** 持久 Audit Event **MUST** 覆盖：登录成功；登出、Session 创建/撤销；全部已认证领域写成功和确定性拒绝；用户/角色/密码；秘密 reveal/轮换/退休；Runtime；维护、恢复、根密钥 rebind 与离线命令；敏感下载授权和已认证权限拒绝。事件 **MUST** 只保存非秘密 actor/action/target/result/locator。（来源：Issue #16 Q16.15、CONTEXT「审计与执行溯源」）
- **SEC-AUDIT-002 —** 匿名登录失败、CSRF/畸形匿名请求、无效 Runtime/Stele token、未认证 403/401 与 429 **MUST NOT** 逐条写入 SQLite；它们 **MUST** 只进入有界计数器和速率受限的非秘密运维日志，且不得保存密码、完整用户名、token/digest 或请求 body。（来源：Issue #16 Q16.5/Q16.15）
- **SEC-AUDIT-003 —** 强制 Audit Event 与领域状态变化 **MUST** 在同一 SQLite 事务提交；Audit INSERT 失败时领域写 **MUST** 回滚。确定性拒绝可在零领域变化的短事务写入；基础设施提交结果未知 **MUST NOT** 被另行写成权威 success/failure。（来源：Issue #16 Q16.15、DATA-AUDIT-004）
- **SEC-AUDIT-004 —** `audit_events` 与 targets **MUST** 由 SQL 禁止 UPDATE/DELETE；Web API **MUST NOT** 提供删除或改写入口。v1 **MUST NOT** 用同一 SQLite 内的 hash chain 产生无法兑现的防部署操作者篡改承诺。（来源：Issue #16 Q16.18）
- **SEC-LOG-001 —** Quoin、Plinth、Lintel 与 Stele 的普通日志、指标标签、持久诊断、Audit Event 和 UI 技术详情 **MUST** 使用字段白名单；默认 **MUST NOT** 记录请求/响应 body、HTTP headers、gRPC metadata、完整 URL query、protobuf/raw JSON dump、Provider 原始错误或任意对象格式化结果。（来源：Issue #16 Q16.17、CONTEXT「秘密与日志」）
- **SEC-LOG-002 —** 秘密值 **MUST** 使用不能被普通字符串化的封装类型；其格式化、JSON/Proto debug 或 error wrapping 结果只能是固定 `[REDACTED]`。外部适配器 **MUST** 先把上游失败映射为稳定错误码和允许字段，再写日志、数据库或 HTTP problem。（来源：Issue #16 Q16.17）
- **SEC-LOG-003 —** 用户主动上传的日志/文本和明确标为敏感的 raw trace **MAY** 包含用户提供的秘密且不做通用猜测式扫描；普通 logger、审计、搜索索引和模型输入 **MUST NOT** 自动复制 raw trace，用户上传正文只按其显式业务路径保留。（来源：CONTEXT「模型调用边界」「在线保留」、Issue #16 Q16.17）

## 8. 敏感下载

- **SEC-DOWNLOAD-001 —** `sensitive=1` Artifact、raw trace 与备份 **MUST** 只接受当前有效 Admin Session；v1 **MUST NOT** 为这些内容签发预签名、分享或脱离 Session 的 bearer URL，也 **MUST NOT** 重复要求同一密码作为伪 step-up。（来源：Issue #16 Q16.16、CONTEXT「敏感内容下载」）
- **SEC-DOWNLOAD-002 —** 服务端 **MUST** 在发送响应头和首字节前重验当前 Session/User/role 并提交“访问已授权” Audit Event；审计失败、授权变化或正文状态变化 **MUST** 在任何字节发送前拒绝。（来源：Issue #16 Q16.15/Q16.16）
- **SEC-DOWNLOAD-003 —** 活动下载流 **MUST** 绑定发起 Session；Session 撤销/过期、账号禁用或降级时服务端 **MUST** 中止剩余发送。每个 Range/续传请求 **MUST** 重新认证、授权和审计，**MUST NOT** 依赖先前请求的 capability。（来源：Issue #16 Q16.16）
- **SEC-DOWNLOAD-004 —** 敏感下载 **MUST** 设置 `Cache-Control: no-store`、`X-Content-Type-Options: nosniff` 与 attachment disposition；文件名只来自转义后的审计元数据 basename，**MUST NOT** 影响物理路径或响应头结构。（来源：Issue #16 Q16.9/Q16.16、HTTP-FILE-002/003）

## 9. 备份恢复、维护与信任重建

- **SEC-RESTORE-001 —** 备份归档 **MUST NOT** 做应用层整体加密；连接 secret 字段继续保持自身 AEAD envelope。备份介质机密性由目录/PV 权限、存储层加密、传输与 Admin 下载边界承担，manifest/checksum **MUST** 只表示完整性，不得被描述为保密。（来源：Issue #16 Q16.2、CONTEXT「一致备份」）
- **SEC-RESTORE-002 —** 恢复工具 **MUST** 在临时位置校验 manifest、checksum、SQLite integrity/foreign keys 与 Artifact 集合；发布恢复库前的最后一个离线事务 **MUST** 建立隔离状态，旧快照 **MUST NOT** 先作为可服务数据库启动再异步清理身份。（来源：Issue #16 Q16.7/Q16.13）
- **SEC-RESTORE-003 —** 恢复隔离事务 **MUST** 清除全部 Web Session、retire 全部 Runtime credential 与 Active 告警 Bearer、把固定 Runtime slot 置 revoked、禁用除 TTY 选定恢复 Admin 外的用户并要求该 Admin 改临时密码、把全部 Connection 置 RevalidationRequired、把 Browser Identity 置 AuthenticationRequired，并写入维护状态、逐对象清单与 system Audit Event。（来源：Issue #16 Q16.13、CONTEXT「一致备份」）
- **SEC-RESTORE-004 —** 普通 SQLite 恢复 **MUST NOT** 轮换数据库外的 Stele service token或根密钥；Stele token 只有在部署 Secret 泄漏或安全事件响应时轮换。恢复期间 Stele Delivery **MUST** 返回可重试 unavailable，**MUST NOT** 使用旧快照中的告警 Bearer接入。（来源：Issue #16 Q16.6、CONTEXT「服务身份」）
- **SEC-MAINT-001 —** 恢复、协调升级与根密钥 rebind **MUST** 复用 `maintenance_state` 单行聚合，并以封闭 reason 区分各自清单；状态与逐对象项目的机器字段、约束由 `contracts/sql/schema.sql` 拥有。维护进入和退出 **MUST** 审计，状态 **MUST NOT** 由进程重启自动清除。（来源：Issue #16 Q16.7/Q16.13）
- **SEC-MAINT-002 —** OpenAPI operation 默认在维护中拒绝；只有机器标记允许的登录、登出、当前用户、改密、维护/健康诊断与审计读取、Admin 信任重建与 `exitMaintenance` **MAY** 执行。`reason=Upgrade` 时还只允许 `prepareUpgrade` 调和预检与显式 `upgrade-drain` 取消既有工作；不得用该例外创建新任务或调用通用备份 API。SSE、业务上传下载、任务、调度和告警接入 **MUST** 拒绝；HTTP 使用 503，Stele 使用可重试 unavailable，Runtime 只允许注册/认证与状态重建而不得派发工作。（来源：Issue #16 Q16.7/Q16.13、Issue #17 Q17.10/Q17.21）
- **SEC-MAINT-003 —** 恢复清单 **MUST** 按“已重建或明确不可用”判定安全：User 已重新启用或保持 disabled；Connection 已重验/重录或保持 disabled；Runtime slot 已重新注册或保持 revoked；告警源已有新凭据或保持 disabled；Browser Identity 的 AuthenticationRequired 本身是安全状态。退出 **MUST NOT** 强迫所有可选集成 Ready。（来源：Issue #16 Q16.13）
- **SEC-MAINT-004 —** 升级清单 **MUST** 包含 active Attempt、active Browser Operation、升级前 Backup Run、版本、迁移和协调升级条件，**MUST NOT** 错误要求恢复身份；进入 Upgrade maintenance 后活动工作必须自然结束或由 Admin 显式取消，完整验证升级前备份成功后才允许停机。RootKeyRebind 清单 **MUST** 要求全部旧 binding Connection 已重新录入或保持 disabled。（来源：Issue #16 Q16.11/Q16.13、Issue #17 Q17.10/Q17.21）
- **SEC-MAINT-005 —** `exitMaintenance` **MUST** 是 Admin 领域写命令，携带 `client_command_id` 与当前 maintenance row version；同一事务 **MUST** 重验当前 Admin、reason 对应的非空清单、全部阻塞项目和并发版本，成功后退出并审计。active maintenance 的 row version 在进入后至退出事务前 **MUST** 冻结，禁止用空转版本推进脱离清单 revision。v1 **MUST NOT** 提供 force、skip、仅 UI checkbox 或自动退出路径。（来源：Issue #16 Q16.7/Q16.13）

## 10. 验证门

- **SEC-VALIDATION-001 —** 密码验证 **MUST** 覆盖 Unicode NFC、15/128 边界、完整值 blocklist、上下文值、dummy hash、五次冷却、窗口过期、内存表饱和与进程重启；验证 **MUST NOT** 只证明 Argon2id 可调用。（来源：Issue #16、NIST SP 800-63B-4）
- **SEC-VALIDATION-002 —** Session/CSRF fixtures **MUST** 覆盖 same-origin、same-site、cross-site、缺失 Fetch Metadata、缺失 Origin、两者缺失、错误公共 Origin、login 无 Cookie、WebSocket Origin、Session revision 变化与长连接即时关闭；每个允许/拒绝结果 **MUST** 与 OpenAPI/HTTP 条款一致。（来源：Issue #16 Q16.8/Q16.14）
- **SEC-VALIDATION-003 —** 根密钥验证 **MUST** 以真实 AES-256-GCM 工件覆盖正确 key、缺失/短 key、错误 key、verifier 损坏、nonce/tag/AAD/binding revision 篡改、单条密文损坏、随机源失败、离线 rebind 与旧 binding 不可下发；任何失败 **MUST NOT** 泄漏 plaintext 或把 Connection 继续派发。（来源：Issue #16 Q16.1/Q16.11）
- **SEC-VALIDATION-004 —** reveal 对抗测试 **MUST** 构造创建响应丢失、同命令重放、并发双消费、审计失败、Session 撤销/降级、过期、进程重启和消费后响应丢失；只有同 Session 的仍有效未消费 handle 可重放，raw secret **MUST** 最多成功读取一次。（来源：Issue #16 Q16.3/Q16.12）
- **SEC-VALIDATION-005 —** 恢复验证 **MUST** 从包含 active Session、Runtime token、告警 Bearer、enabled User/Connection、Ready Browser Identity 的真实备份恢复，并在任何服务入口开放前证明旧身份全部被隔离、Stele service token 保持外部有效、维护清单与退出事务符合 SEC-RESTORE-*/SEC-MAINT-*。（来源：Issue #16 Q16.6/Q16.7/Q16.13）
- **SEC-VALIDATION-006 —** 敏感下载验证 **MUST** 覆盖审计写失败前零字节、Operator 403、Admin 成功、Session 在首字节前和传输中被撤销、账号降级、Range 重认证、缓存/Disposition 头与无分享 URL。（来源：Issue #16 Q16.15/Q16.16）
- **SEC-VALIDATION-007 —** 秘密泄漏验证 **MUST** 向 Cookie、Authorization、密码、API key、kubeconfig、根密钥标记、Provider 错误回显、reveal handle/raw secret 与浏览器 trace 注入唯一 sentinel，并扫描 Quoin/Plinth/Lintel/Stele stdout、stderr、结构化日志、Audit Event、problem response 与 telemetry；普通输出任一命中 **MUST** 失败。明确敏感 raw trace 和用户主动上传正文只验证不被普通 logger/索引复制。（来源：Issue #16 Q16.17）
- **SEC-VALIDATION-008 —** 安全机器契约 **MUST** 通过 OpenAPI 零警告 lint、完整 SQLite Schema 装载、foreign key/integrity check、状态机反例、Markdown↔机器契约一致性和独立威胁复审；构建或单个 happy-path 测试成功 **MUST NOT** 代替上述证据。（来源：Issue #16、SPEC-AUTHORITY-003）

## 11. 上游依据与明确取舍

**Non-normative：** 当前外部依据为 NIST SP 800-63B-4、OWASP Password Storage / Authentication / Session Management / CSRF / HTTP Headers Cheat Sheets、Go `net/http.CrossOriginProtection` 文档，以及 SecLists MIT 仓库的 NCSC 100k 密码列表。Quoin 使用这些来源校准机制，不宣称获得任何外部合规认证。

- NIST SP 800-63B-4：https://pages.nist.gov/800-63-4/sp800-63b.html
- Go CrossOriginProtection：https://pkg.go.dev/net/http#CrossOriginProtection
- OWASP Cheat Sheet Series：https://cheatsheetseries.owasp.org/
- SecLists：https://github.com/danielmiessler/SecLists
- SecLists pinned file：https://github.com/danielmiessler/SecLists/blob/1a7bb9127eca9e6ff2fc0301c597fe6e16a0cb56/Passwords/Common-Credentials/100k-most-used-passwords-NCSC.txt
