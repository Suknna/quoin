# Quoin v1 Runtime 协议

**状态：Draft**

**Non-normative：** 本文件（CATEGORY=`RUNTIME`）承载 Quoin、Plinth、Lintel、Stele 四组件之间的 gRPC 协议语义：身份、版本握手、控制流、任务派发与调和、lease/fencing、浏览器会话隧道、Artifact 上传、连接凭据 grant、token 注册与轮换、吊销与告警接入。机器可表达的 service、RPC、message、stream 与 wire-level 枚举由 [`contracts/runtime.proto`](contracts/runtime.proto)（package `quoin.runtime.v1`，SPEC-VERSION-002）唯一拥有（SPEC-AUTHORITY-001/002）；本文件只通过相对路径与稳定符号引用它，不复制字段清单。持久化权威为 `persistence.md` 与 `contracts/sql/schema.sql`；前端→Quoin 的 HTTP 面（含 reveal 流程）为 `http-api.md` 与 `contracts/openapi.yaml`（HTTP-SCOPE-003）。

## 1. 范围与权威

- **RUNTIME-SCOPE-001 —** 四组件 gRPC 的 service、RPC、message、stream 方向与枚举值 **MUST** 只由 `contracts/runtime.proto`（`quoin.runtime.v1`）定义；本文件 **MUST NOT** 声明 proto 中不存在的服务、消息、字段或枚举值（SPEC-AUTHORITY-001/003）。
- **RUNTIME-SCOPE-002 —** 本文件 **MUST** 只拥有无法由 proto 表达的跨流语义：握手顺序、身份裁决、lease/fencing、调和、提交顺序裁决、故障与恢复；不重复 proto 已表达的形状。（来源：README「权威源与事实所有权」、Issue #8）
- **RUNTIME-SCOPE-003 —** locator（attempt、operation、artifact、source、credential 等）在 proto 中 **MUST** 使用 `int64`，与 SQLite INTEGER 及 HTTP 十进制字符串表示一致（DATA-IDENT-002）；`0` 表示无该 locator，边界校验见 RUNTIME-VALIDATION-004。
- **RUNTIME-SCOPE-004 —** 数值型调优参数（heartbeat 周期、lease 时长、上传 chunk 大小、流队列深度、重附着宽限期、轮换窗口）**MUST** 由部署配置与原型测量决定，本文件与 proto **MUST NOT** 冻结具体数值；proto 只提供承载这些值的字段类型与语义（Timestamp/Duration/uint64/uint32）。（来源：CONTEXT「执行尝试」「健康语义」、Issue #11 研究结论）

## 2. 身份与认证

- **RUNTIME-AUTH-001 —** Plinth、Lintel **MUST** 主动出站拨号 Quoin gRPC server（维持只出站部署，Q2 冻结）；TLS **MUST** 只承担服务端身份与传输保护，**MUST NOT** 建立 mTLS 客户端身份。（来源：CONTEXT「服务身份」、架构记忆 #5860）
- **RUNTIME-AUTH-002 —** 长期服务 token **MUST** 经 gRPC metadata（`authorization: Bearer <token文本>`）认证，**MUST NOT** 出现在任何消息体（唯一受保护例外：`RegisterRuntimeResponse.long_term_token` 与 `IssueToken.token` 是下发路径，只经已认证/已注册 TLS 通道出现一次）；raw token 只存在于 supervisor 内存、权限 `0600` 的专用持久状态卷与认证瞬间；数据库只存 digest（DATA-RUNTIME-002、CONTEXT「模型调用边界」）。
- **RUNTIME-AUTH-003 —** 连接认证裁决 **MUST** 满足：token digest 等于该 slot `runtime_slots.current_credential_id` 指向的 `runtime_credentials.token_digest`、该行 `confirmed_at` 非空且 `retired_at` 为空（DATA-RUNTIME-001/002）；`state='revoked'` 期间旧凭据一律拒绝，未被 current 指针选中的已确认历史行 **MUST NOT** 被接受。
- **RUNTIME-AUTH-004 —** 认证失败（令牌无效、slot 吊销、版本不匹配、epoch 回退）**MUST** 在握手阶段以 `HelloAck{accepted=false, reject_reason}` 拒绝并关闭流；组件 **MUST NOT** 进入 Ready。（来源：CONTEXT「服务身份」「健康语义」）
- **RUNTIME-AUTH-005 —** 连接凭据与模型供应商凭据 **MUST** 只由 supervisor 持有并注入类型化工具；Plinth/Lintel worker **MUST NOT** 读取状态卷、环境变量或任何凭据文件；`FetchCredentialGrant` 是 supervisor-only RPC（RUNTIME-GRANT-002），worker 进程不得持有该调用能力（CONTEXT「模型调用边界」、架构记忆 #5868）。
- **RUNTIME-AUTH-006 —** 全部 token 的文本编码 **MUST** 是 32 随机字节的 base64url（无填充）ASCII 文本：长期 token、一次性注册令牌与 HTTP reveal 产出的注册令牌都是同一编码；metadata 的 `authorization: Bearer` 值、0600 状态卷文件内容与 `RegisterRuntimeRequest.one_time_token`/`RegisterRuntimeResponse.long_term_token`/`IssueToken.token` 均为该文本。数据库 `runtime_credentials.token_digest`/`alert_source_credentials.digest` **MUST** 是解码后原始 32 字节的 SHA-256（`length(token_digest)=32`，DATA-RUNTIME-002）。内容 digest（`AttemptInputSnapshot.content_digest`/`ResultPayload.content_digest`/`ArtifactUploadHeader.sha256`）是原始 32 字节二进制，用 `bytes` 承载，**MUST NOT** 做文本编码。（来源：CONTEXT「服务身份」、Issue #11 第一轮审阅）

## 3. 连接与握手（`RuntimeControl.Connect`）

- **RUNTIME-CTRL-001 —** 每个 slot **MUST** 至多一条活动控制流；Quoin 在内存临界区维护连接投影，新连接（`connection_epoch` 更大）**MUST** 替换旧连接并关闭旧流，旧流上未完成的领域动作 **MUST NOT** 产生权威状态（RUNTIME-CTRL-009、RUNTIME-TASK-005/008）。控制流与 Browser/Artifact 流是不同 RPC；v1 **MAY** 共用同一 TCP/HTTP-2 channel，**MUST NOT** 强制独立 channel；若原型证明互相干扰，部署配置再拆分。（来源：CONTEXT「服务身份」、Issue #11 任务约束）
- **RUNTIME-CTRL-002 —** 连接首帧 **MUST** 为 `Hello`（slot、boot_id、connection_epoch、release_version）；Quoin **MUST** 先完成认证与握手校验再回 `HelloAck`；握手完成前任何其他消息 **MUST** 被忽略或拒绝。
- **RUNTIME-CTRL-003 —** `release_version` **MUST** 与 Quoin 严格相等（`==`），**MUST NOT** 做任何协商或前缀匹配；不匹配时 `HelloAck{reject_reason=VERSION_MISMATCH}` 且组件不 Ready（RUNTIME-VERSION-001）。
- **RUNTIME-CTRL-004 —** `boot_id` **MUST** 在每次进程启动时重新生成；`connection_epoch` **MUST** 由 Runtime 在每次连接时递增（>= 1），Quoin **MUST** 拒绝非单调（<= 上次记录的 epoch）的连接（`EPOCH_STALE`）。
- **RUNTIME-CTRL-005 —** `Heartbeat` **MUST NOT** 改写任何持久状态：只更新内存瞬时投影（`connected`/`lastSeenAt`/`bootId`/`connectionEpoch`，HTTP-RUNTIME-001），**MUST NOT** 递增 `runtime_slots.row_version`；`Capacity` 只作派发决策提示，不是权威事实。（来源：CONTEXT「健康语义」、DATA-RUNTIME-001）
- **RUNTIME-CTRL-006 —** 每条流的每一端 **MUST** 使用单一 send loop / recv loop；发往对端的消息 **MUST** 进入有界队列，队列溢出 **MUST** 可观察（部署内部指标）且 **MUST NOT** 静默丢弃领域消息（派发、取消、结果）。
- **RUNTIME-CTRL-007 —** `GoAway` 是 Quoin 结束连接的尽力而为通知；实际关闭以 RPC 结束为准，客户端 **MUST NOT** 依赖 `GoAway` 送达；`SHUTTING_DOWN` 表示可重试的暂时不可用，`REVOKED`/`ROTATED`/`REPLACED` 表示必须按 RUNTIME-REVOKE-001 处理。
- **RUNTIME-CTRL-008 —** 物理消息重复投递 **MUST** 是幂等的：领域事实按 `attempt_id`+`boot_id`+`connection_epoch` 去重；对端不重复提交。
- **RUNTIME-CTRL-009 —** 每个 `ControlEnvelope` **MUST** 携带四个 envelope 字段（proto 强制形状，本条款定义语义）：`message_id`（当前连接内按方向单调递增且唯一，用于对 gRPC 缓冲重放/透明重试的物理去重：同 `(slot, boot, epoch, direction, message_id)` 的重复投递 **MUST** 被忽略、不重复执行）、`connection_epoch` 与 `boot_id`（除 `Hello` 外 **MUST** 等于 Quoin 当前已接受流上下文；不一致 **MUST** 丢弃且最多审计，**MUST NOT** 产生任何状态变化）、`correlation_id`（请求-响应对关联：`Hello`↔`HelloAck`、`DispatchAttempt`↔`AttemptAccept/AttemptReject`、`ReconcileRequest`↔`ReconcileReport`、`ResultProposal`↔`ResultAck`、`CancelAttempt`↔`CancelAck`、`IssueToken`↔`TokenPersisted`、`RequestBrowserSubExecution`↔`BrowserSubExecutionAck`、`ToolResultDelivery` 回带原请求关联；0 = 无）。**所有**会写权威状态的消息（`AttemptAccept`/`AttemptReject`/`ResultProposal`/`CancelAck`/`TokenPersisted`/`ReconcileReport`/`RequestBrowserSubExecution` 等）在事务提交前 **MUST** 复核当前活动流上下文（slot+boot+epoch）与 envelope 一致；旧流迟到消息（新 epoch 已接受后到达的旧流 buffered 消息）一律按上文丢弃/审计，旧流上未完成的领域动作 **MUST NOT** 产生权威状态（RUNTIME-CTRL-001）。`Hello` 的 envelope 字段描述提议的连接身份（epoch 为提议值，可不同于当前活动流）。（来源：Issue #4 研究证据、Issue #11 第一轮审阅）

## 4. 任务派发与调和

- **RUNTIME-TASK-001 —** Quoin **MUST** 先持久化 `execution_attempts`（`attempt_id` 权威）再派发：派发事务在同一事务内设置 `runtime_slot`/`boot_id`/`connection_epoch`/`lease_until`（`Assigned`）并复查目标 slot `registered` 且 current 指向已确认未退休凭据（DATA-ATTEMPT-001、DATA-RUNTIME-001）；事务提交后 **MUST** 才发送 `DispatchAttempt`。
- **RUNTIME-TASK-002 —** `DispatchAttempt` **MUST** 携带 attempt_id、attempt_type、scope_type、scope_id、可选 check_key、`lease_deadline`（有限 lease）、完整 `AttemptInputSnapshot`（RUNTIME-TASK-011）与可选的 `requested_by_tool_call_id`；lease 时长数值为部署配置（RUNTIME-SCOPE-004）。
- **RUNTIME-TASK-003 —** Attempt 类型与 slot 的固定映射 **MUST** 与 schema CHECK 一致：`browser_exploration`/`inspection_collection` 只派发 `lintel`，`initial_analysis`/`investigation`/`inspection_analysis` 只派发 `plinth`（DATA-RUNTIME-001b、DATA-ATTEMPT-001）。
- **RUNTIME-TASK-004 —** Runtime **MUST** 对每个 `DispatchAttempt` 回 `AttemptAccept` 或 `AttemptReject`：Accept 后 Quoin 写 `accepted_at` 并转 `Running`；`NO_CAPACITY` 拒绝时 Attempt **MUST** 保持 `Assigned`（绑定不可改），Quoin **MAY** 幂等重发同一 `DispatchAttempt`（同 attempt_id/boot/epoch），超过部署内部重试策略后按 RUNTIME-TASK-009 终止；`INPUT_UNSUPPORTED`/`INTERNAL` 拒绝时 Quoin **MUST** 转 `Failed`（`provider_unavailable`）。
- **RUNTIME-TASK-005 —** lease 内同 boot 重连（`Hello` 的 boot_id 相同且 epoch 更大、lease 未到期）**MUST NOT** 重派：Quoin **MUST** 发送 `ReconcileRequest`（该 slot 当前 active 集合），Runtime **MUST** 回 `ReconcileReport`（实际运行集合），双方按 attempt_id 对齐；`Assigned` 未接受的 Attempt **MAY** 在调和后幂等重发。
- **RUNTIME-TASK-006 —** 新 boot（boot_id 不同）**MUST** 使旧 boot 绑定且仍 active 的 Attempt 转 `Interrupted`（`lease_expired`）；slot 替换使绑定该 slot 的 Attempt 转 `Interrupted`（`replaced`）；凭据吊销使 Attempt 转 `Interrupted`（`revoked`）；lease 到期无续期同样转 `Interrupted`（`lease_expired`）。（来源：CONTEXT「执行尝试」、DATA-ATTEMPT-001）
- **RUNTIME-TASK-007 —** `lease_until` **MAY** 由心跳或活动消息续期（每次 UPDATE 恰好递增 `row_version`，DATA-ATTEMPT-006）；续期判定数值为部署配置。
- **RUNTIME-TASK-008 —** 结果 **MUST** 经 `ResultProposal` 提交（attempt_id + boot_id + connection_epoch + outcome + 可选 termination_reason + artifact_ids + `ResultPayload`）；Quoin **MUST** 按 attempt_id+boot+epoch 幂等裁决：旧 epoch、lease 已到期或 cancellation fence 已提交的迟到结果 **MUST NOT** 产生有效领域输出，只留审计（DATA-ATTEMPT-004）；`ResultAck{accepted}` 回传裁决。
- **RUNTIME-TASK-009 —** 部分 token 后失败 **MUST NOT** 成为有效输出：Attempt `Failed` + 结构化 `TerminationReason`（与 schema CHECK 枚举一致）；成功前所有引用 Artifact **MUST** 经 `ArtifactService.Upload` 上传校验提交（DATA-ATTEMPT-005、DATA-ARTIFACT-006）。
- **RUNTIME-TASK-010 —** `AttemptProgress` **MUST NOT** 成为权威状态来源：显示性进度；权威进度由 `tool_calls`/`evidence` 持久记录承担（DATA-EVIDENCE-001）。
- **RUNTIME-TASK-011 —** 任务执行输入 **MUST** 由 `AttemptInputSnapshot` 完整承载（**MUST NOT** 只传 digest 让对端自行重建）：`schema_kind` 非空且为已登记 schema（`investigation_v1`/`initial_analysis_v1`/`inspection_collection_v1`/`inspection_analysis_v1`/`browser_exploration_v1`，具体字段由 #13/#14 承接）、`canonical_json` 非空且 `content_digest` = SHA-256(canonical_json)、`artifact_refs` 只读引用、`connection_grants` 为非秘密 grant 引用（RUNTIME-GRANT-001）。对端 **MUST** 拒绝 digest 不符或未知 `schema_kind`（`AttemptReject{INPUT_UNSUPPORTED}`）。输入快照 **MUST NOT** 包含任何秘密（秘密只经 `FetchCredentialGrant` 下发，RUNTIME-GRANT-001）。
- **RUNTIME-TASK-012 —** 模型/分析输出 **MUST** 由 `ResultProposal.payload`（`ResultPayload`）承载：`outcome=SUCCEEDED` 时 **MUST** 携带 payload，`schema_kind` 与 attempt_type 匹配（`assistant_message_v1`（investigation）、`inspection_report_v1`（inspection_analysis）、`initial_analysis_v1`（initial_analysis）、`browser_operation_v1`（browser_exploration/inspection_collection）等，具体字段由 #13/#14 承接），`content_digest` = SHA-256(canonical_json)。Quoin **MUST** 校验 schema_kind 与 digest 后在同一结果提交事务内原子持久化到对应领域记录（`investigation_messages`/`inspection_reports`/`initial_analysis_outputs`/`browser_operations.log_json`），随后才转 `Succeeded` 并回 `ResultAck{accepted=true}`；digest 不符、schema_kind 未知或与 attempt_type 不匹配 **MUST** 转 `Failed`（`invalid_response`）且 payload 不落库。**MUST NOT** 依赖未定义实现层的补通道传递输出。

## 5. 取消与结果裁决

- **RUNTIME-CANCEL-001 —** 用户取消 **MUST** 先由 Quoin 事务提交 cancellation fence（`Queued` 直接 `Cancelled`；`Running` 先转 `Cancelling`），事务提交后 **MUST** 才发送 `CancelAttempt`（DATA-ATTEMPT-003、HTTP-COMMAND-005）。
- **RUNTIME-CANCEL-002 —** 成功结果与取消 **MUST** 按 SQLite 提交顺序裁决（DATA-TX-005）：成功先提交则取消返回已完成对象；取消先提交则迟到结果不产生有效消息/Report/Candidate，只留审计。
- **RUNTIME-CANCEL-003 —** Runtime 收到 `CancelAttempt` 停止后 **MUST** 回 `CancelAck`；Quoin 收到后转 `Cancelled`。fence 与 transport detach 并发时 **MUST NOT** 停留在 `Cancelling` 中间态：lease 到期或流关闭后 Quoin 按 RUNTIME-TASK-006 收敛终态。旧流迟到的 `CancelAck`/`AttemptAccept` 按 RUNTIME-CTRL-009 envelope fence 丢弃。

## 6. 跨 Runtime 子执行（Plinth → Quoin → Lintel）

- **RUNTIME-CROSS-001 —** Plinth 的浏览器工具请求 **MUST** 经 `RequestBrowserSubExecution`（控制流，Plinth -> Quoin）提交：携带 `request_id`（父 Attempt 内唯一，写入 params 快照持久化）、`parent_attempt_id` 与 `BrowserSubExecutionInput`（非秘密冻结参数，`schema_kind="browser_tool_v1"`，含 identity/journey 引用与 `request_id`）。Plinth 与 Lintel **MUST NOT** 直接通信（Q93 冻结，架构记忆 #5980）。
- **RUNTIME-CROSS-002 —** Quoin **MUST** 在同一事务内：创建父 Attempt 的 `tool_calls` 行（status=`pending`，arguments_json = 规范参数 JSON）并创建 `browser_exploration` 子 Attempt（`requested_by_tool_call_id` 指向新 tool_call 行；DATA-ATTEMPT-007），随后派发 lintel（RUNTIME-TASK-003）并以 `BrowserSubExecutionAck`（accepted=true + tool_call_id + child_attempt_id）应答。幂等：相同 `(parent_attempt_id, request_id)`（以 tool_calls.arguments_json 中的 request_id 查询）重放 **MUST** 返回原 tool_call_id 与 child_attempt_id，**MUST NOT** 重复创建；旧 epoch/boot 请求按 RUNTIME-CTRL-009 丢弃。拒绝返回 `accepted=false` + 封闭的 `BrowserSubExecutionRejectReason`：父 Attempt 不属于 plinth、非 Running 或已取消=`PARENT_NOT_RUNNING`，输入 schema/digest/引用无效=`INPUT_UNSUPPORTED`，旧 boot/epoch=`STALE_STREAM`，持久化或派发内部错误=`INTERNAL`；拒绝响应中的 locator 为 0（RUNTIME-VALIDATION-004）。
- **RUNTIME-CROSS-003 —** 子 Attempt 派发 lintel（RUNTIME-TASK-003）；父 Attempt 取消 fence 与子结果按 DATA-TX-005 提交顺序裁决：父取消先提交则子结果不产生有效消息/Report/Candidate，只留审计；子结果先提交则保留。
- **RUNTIME-CROSS-004 —** 子 Attempt 结果提交（`ResultProposal`，payload `browser_operation_v1`）时 Quoin **MUST** 在同一事务更新对应 `tool_calls` 行（`pending`->`succeeded`/`failed`）并持久化浏览器操作结果，随后向父 Attempt 所在 Plinth 控制流发送 `ToolResultDelivery`（tool_call_id、success、evidence_ids、payload；correlation_id 关联原请求）；父 Attempt 据此继续模型循环。**MUST NOT** 建立通用任务 DAG：子执行只存在于"Plinth 工具调用请求 Lintel 浏览器操作"这一条路径（DATA-ATTEMPT-007）。

## 7. 浏览器隧道（`BrowserTunnel.Open`）

- **RUNTIME-BROWSER-001 —** 每个活动 BrowserSession **MUST** 使用一条独立 bidi stream；持久审计权威是 `browser_operations`（DATA-BROWSER-003），BrowserSession（session_id、宽限期）是内存瞬时投影，**MUST NOT** 新增持久表（DATA-SCOPE-002、DATA-RUNTIME-001）。
- **RUNTIME-BROWSER-002 —** 流首帧 **MUST** 为 `BrowserSessionOpen`（operation_id、identity_id、slot、boot_id、connection_epoch；同 boot 宽限期内重附着携带 `reconnect_session_id`）；Quoin **MUST** 校验：slot 为 `LINTEL`、连接身份匹配、对应 Browser Operation 存在且该身份无其他活动会话（独占串行，DATA-BROWSER-003）；通过后回 `BrowserSessionOpenAck`（session_id + 重附着宽限期截止）。
- **RUNTIME-BROWSER-003 —** noVNC/RFB 字节 **MUST** 在 `BrowserFrameData.payload` 双向透明中继：Quoin **MUST NOT** 解析、校验或改写字节内容（HTTP-NOVNC-001 的 noVNC 字节由 Quoin 转入 Lintel 的 gRPC 隧道，架构记忆 #5860）。
- **RUNTIME-BROWSER-004 —** 会话断开（流结束且无 close 原因）后身份锁 **MUST** 保持到重附着宽限期截止；宽限期内同 session_id、**同 boot** 重附着 **MUST** 被接受，超期 **MUST** 释放身份锁并使会话进入 `GRACE_EXPIRED` 关闭；宽限期数值为部署配置（RUNTIME-SCOPE-004）。
- **RUNTIME-BROWSER-005 —** 会话/操作吊销、账号禁用或 Session 登出 **MUST** 立即以 `BrowserSessionClose`（`SESSION_REVOKED`）关闭对应隧道；`BrowserCloseReason` 枚举 **MUST** 与 proto 一致。
- **RUNTIME-BROWSER-006 —** Lintel 控制流吊销或替换 **MUST** 立即关闭该连接上的全部 BrowserSession 流（`SLOT_REVOKED`/`SLOT_REPLACED`）；Browser Operation 的持久记录 **MUST NOT** 因流关闭而改写（result 仍为空 = 未完成，替换 fence 继续拦截，DATA-RUNTIME-001b）。
- **RUNTIME-BROWSER-007 —** Lintel **新 boot** 被 Quoin 接受（`Hello` boot_id 不同且握手成功）时，Quoin **MUST** 立即关闭旧 boot 的全部 BrowserSession 流（`NEW_BOOT`）并**立即释放**这些会话持有的身份锁——重附着宽限期只适用于同 boot 瞬时断线（RUNTIME-BROWSER-004），跨 boot 无宽限期；对应 Browser Operation 的持久记录保持不变（result 仍为空），新 boot 可重新开始操作。连接上下文（boot_id/connection_epoch）**MUST** 与 `BrowserSessionOpen` 一致，不一致拒绝打开。

## 8. Artifact 上传（`ArtifactService.Upload`）

- **RUNTIME-UPLOAD-001 —** 每个上传 **MUST** 使用独立 client-stream：首帧 **MUST** 为 `ArtifactUploadHeader`（upload_id、attempt_id、boot_id、connection_epoch、owner、kind、retention_kind、sensitive、size_bytes、sha256），随后 0..N 个 `ArtifactUploadChunk`（offset 连续），可显式 `ArtifactUploadEnd`；Quoin 在收到 `size_bytes` 字节或 `end` 帧后裁决。
- **RUNTIME-UPLOAD-002 —** v1 **MUST** 整单重传，**MUST NOT** 实现 offset 续传；`upload_id` 在重试生命周期内稳定：同 upload_id 同摘要重试 **MUST** 返回原 `artifact_id`，同 upload_id 不同摘要/owner **MUST** 冲突拒绝（DATA-ARTIFACT-006）。
- **RUNTIME-UPLOAD-003 —** Quoin 提交 **MUST** 依 DATA-ARTIFACT-001 的耐久顺序：临时文件写入并校验 hash/大小 → fsync 临时文件 → 按 SHA-256 原子 rename → fsync 最终父目录 → 才提交 SQLite 引用事务（`artifact_blobs` + `artifacts` + ledger `uploading->committed`）；引用事务失败 **MUST** 将文件作为孤儿清理。
- **RUNTIME-UPLOAD-004 —** `committed` 裁决 **MUST** 满足全部正向条件（DATA-ARTIFACT-006 与 SQL 触发器）：Attempt `Running` 且 `boot_id`/`connection_epoch` 非空并与 upload 一致；JOIN 的 `artifacts`+`artifact_blobs` 与 ledger 的 kind/retention_kind/sensitive/owner_type/owner_id/sha256/size_bytes 精确匹配；任一条件不满足 **MUST** 置 `rejected` 并返回 `UploadRejectReason`（旧 Attempt 终态、替换后 ABA、boot/epoch 不符、元数据不符均不得 commit）。
- **RUNTIME-UPLOAD-005 —** `trace` 上传 **MUST** `sensitive=true`（DATA-ARTIFACT-003）；ledger 来源字段与 `artifact_id`/`committed_at` 提交后 **MUST NOT** 改写；历史 **MUST NOT** 删除。
- **RUNTIME-UPLOAD-006 —** chunk 大小与流背压数值为部署配置（RUNTIME-SCOPE-004）；队列溢出 **MUST** 可观察且不静默丢弃。

## 9. 注册与轮换

- **RUNTIME-REG-001 —** 一次性注册令牌 **MUST** 只存在于内存（短生命周期、单次消费、绑定 slot **与 credential generation**，RUNTIME-AUTH-006 文本编码），**MUST NOT** 落库（SQLite/日志/审计/Artifact/命令结果均禁止）；HTTP 面经 `replaceRuntimeSlot` + `revealRuntimeRegistrationToken` 两段式 reveal，reveal 结果携带 slot、generation 与 registrationToken（HTTP-COMMAND-012）。
- **RUNTIME-REG-002 —** `RuntimeControl.Register` **MUST** 只接受处于注册窗口（slot `unregistered` 或 `revoked`）的请求：Quoin 验证一次性令牌与内存令牌记录匹配（slot **和 generation 都须一致**）、`release_version` 严格相等，随后在同一事务创建已确认 credential（`confirmed_at` 非空，`trg_runtime_credentials_insert_confirmed_registration_only` 允许窗口、generation 与请求一致）、设置 current 指针、slot 转 `registered`，并返回长期 token 与同一 generation；supervisor **MUST** 将返回的 token 原子持久化到权限 `0600` 专用状态卷。响应丢失 **MUST NOT** 重放同一一次性令牌：slot 已 `registered` 时 `Register` 拒绝（`FAILED_PRECONDITION`），恢复路径为 Admin 重新执行 `replaceRuntimeSlot`（HTTP-COMMAND-012）。
- **RUNTIME-REG-003 —** `Register` 在 slot 已 `registered` 时 **MUST** 拒绝（`FAILED_PRECONDITION`，detail=`ALREADY_REGISTERED`）；同一 slot 的并发注册由一次性令牌单次消费裁决；generation 不匹配 **MUST** 拒绝（`FAILED_PRECONDITION`）。
- **RUNTIME-REG-004 —** 长期 token 两阶段轮换 **MUST** 只走已认证控制流：① Quoin 建 credential（`confirmed_at` 空）并设 `pending_credential_id`，发送 `IssueToken`（新 token，RUNTIME-AUTH-006 文本编码）；② Runtime **MUST** 原子持久化新 token 到 0600 状态卷后回 `TokenPersisted`；③ Quoin 写 `confirmed_at`（须仍被 pending 引用）并单条 UPDATE 原子提升（current=pending、pending 清空，AFTER 触发器自动 retire 旧 current，DATA-RUNTIME-002）；④ Quoin 以 `GoAway{ROTATED}` 关闭旧 token 认证的连接，Runtime 以新 token 重连。轮换窗口数值为部署配置（RUNTIME-SCOPE-004）。
- **RUNTIME-REG-005 —** 轮换期间认证 **MUST** 只接受 `registered` + current 指针指向的行（DATA-RUNTIME-002）；pending 行 **MUST NOT** 被认证接受。
- **RUNTIME-REG-006 —** 轮换中止（Admin 中止）**MUST** 先清 `pending_credential_id`（AFTER 触发器自动 retire 原 pending），随后以 `GoAway` 或继续当前连接结束；已退休凭据 **MUST NOT** 再被确认或重新设回 pending（DATA-RUNTIME-002）。
- **RUNTIME-REG-007 —** `Register` 失败 **MUST** 使用 canonical gRPC status（RUNTIME-ERROR-001）：`INVALID_ARGUMENT`（请求缺字段/generation=0/token 文本非 base64url 或解码非 32 字节）、`UNAUTHENTICATED`（令牌未知/过期/已消费）、`FAILED_PRECONDITION`（slot 不在注册窗口——含 `ALREADY_REGISTERED`——或 generation 与令牌记录不一致）、`PERMISSION_DENIED`（令牌绑定 slot 与请求 slot 不一致）、`NOT_FOUND`（未知 slot，不应发生）、`INTERNAL`（瞬态内部错误）。detail 只含非秘密说明。

## 10. 吊销与关闭

- **RUNTIME-REVOKE-001 —** 凭据吊销、slot 替换或账号级吊销 **MUST** 立即关闭对应 Runtime 的全部 Control/Browser/Upload 流并拒绝重连（Q214 冻结、架构记忆 #6034）；关闭是权威，`GoAway`/`BrowserSessionClose` 是尽力而为通知。
- **RUNTIME-REVOKE-002 —** 替换 fence **MUST** 与 DATA-RUNTIME-001b 一致：`plinth` 置 `revoked` 前无绑定该 slot 的 active Attempt（`Assigned`/`Running`/`Cancelling`）；`lintel` 置 `revoked` 前无任何 `browser_operations.result IS NULL`；BrowserSession 瞬时锁在 Quoin 内存临界区与替换事务同 fence。
- **RUNTIME-REVOKE-003 —** 替换与派发 **MUST** 按 SQLite 提交顺序裁决：替换先提交则后续派发被拒（DATA-RUNTIME-001 触发器兜底）；派发先提交则替换被拒；revoked 期间旧凭据一律拒绝。

## 11. 连接凭据 grant（`RuntimeControl.FetchCredentialGrant`）

- **RUNTIME-GRANT-001 —** Attempt 派发时绑定的连接凭据 **MUST** 以非秘密 `ConnectionGrant` 引用随 `AttemptInputSnapshot` 下发（`grant_id`/`connection_revision_id`/`credential_generation_id`/`purpose`，对应 Attempt 派发事务绑定的 revision/generation，DATA-CONN-002）；秘密正文 **MUST NOT** 进入 `DispatchAttempt`、输入快照、日志或审计。
- **RUNTIME-GRANT-002 —** supervisor **MUST** 经 `FetchCredentialGrant`（unary，Bearer 认证）获取秘密：Quoin 校验 grant 存在且属于请求的 Attempt、Attempt 处于 `Running`、`boot_id`/`connection_epoch` 与派发绑定一致、请求 slot 为 `plinth`（Lintel 无连接凭据，请求一律 `PERMISSION_DENIED`）；成功响应携带非秘密 `revision_config_json`（DATA-CONN-005 类型化投影）与解密后的类型化 secret（`connection_type` 对应 thanos/kubernetes/model_provider）。worker 进程 **MUST NOT** 调用本 RPC（RUNTIME-AUTH-005）。
- **RUNTIME-GRANT-003 —** grant 生命周期 **MUST** 只存在于 Attempt 内存期间：派发时创建、Attempt 达到终态（Succeeded/Failed/Cancelled/Interrupted）时 Quoin **MUST** 清除 grant 并释放内存秘密；Attempt 未 `Running` 或 grant 已清除时 `FetchCredentialGrant` **MUST** 返回 `FAILED_PRECONDITION`（或 `NOT_FOUND`，detail 说明）；supervisor 侧 **MUST** 在 Attempt 终态后丢弃内存秘密，**MUST NOT** 落盘、进入日志/审计或响应快照。成功响应经 TLS 每次可重取（Attempt Running 期间幂等），但 supervisor **MUST NOT** 持久化。

## 12. Stele 告警接入（`SteleRelay`）

- **RUNTIME-STELE-001 —** Stele **MUST NOT** 注册为 Runtime（无 slot、不持有 Runtime 凭据）；其 service token 由部署 Secret 文件提供（CONTEXT「服务身份」），经 metadata 认证 `SteleRelay` 两个 unary RPC。
- **RUNTIME-STELE-002 —** Stele **MUST** 经 `GetCredentialSnapshot` 获取版本化只读凭据 digest 快照并仅内存缓存；未成功加载快照 **MUST** 拒绝接收 Delivery；快照版本单调递增，Stele 提交时回传（DATA-ALERT-008）。
- **RUNTIME-STELE-003 —** `Deliver` **MUST** 携带 `relay_id`（每次外部请求生成，内部重试复用）、`source_id`、`credential_id`、`credential_snapshot_version`（>= 1）、`protocol`、精确原始 `body` 与 `received_at`；Quoin **MUST** 在 Delivery 事务内重验来源启用、凭据有效性与归属，relay_id 幂等（DATA-ALERT-001/008）。
- **RUNTIME-STELE-004 —** `DeliveryRelayResponse` 状态 **MUST** 映射 HTTP 语义：`ACCEPTED`=204（单事务保存 Delivery、处理结果并更新全部正常 Occurrence 后才返回）；`REJECTED`=4xx（永久拒绝，Alertmanager 不重试）；`UNAVAILABLE`=5xx（可重试）。提交失败或结果不确定 **MUST NOT** 返回 ACCEPTED（CONTEXT「Stele」）。
- **RUNTIME-STELE-005 —** 每个 unary 请求 **MUST** 携带 `release_version`（机器契约字段），与 Quoin 严格相等（RUNTIME-VERSION-001）；不匹配 **MUST** 返回 `FAILED_PRECONDITION` 且 **MUST NOT** 处理请求（Stele 不 Ready、拒绝接收 Delivery）；`GetCredentialSnapshotResponse.quoin_release_version` 供 Stele 快速核对，不一致时 Stele **MUST** 视为版本不匹配。

## 13. 版本与兼容

- **RUNTIME-VERSION-001 —** 四组件 **MUST** 以同一 Release 部署：`release_version` 严格相等（Connect 握手、Register、Stele unary 全部适用），**MUST NOT** 支持协商、前缀匹配或 N/N-1 混跑（CONTEXT「协调升级」）。
- **RUNTIME-VERSION-002 —** proto 字段编号一经分配 **MUST** 永不复用；删除字段 **MUST** 使用 `reserved` 声明并保留编号；oneof 成员与普通字段共享编号空间（SPEC-VERSION-003）。
- **RUNTIME-VERSION-003 —** 本文件所有新增字段（envelope 四字段、AttemptInputSnapshot/ResultPayload、子执行消息、grant 消息、release_version 字段）**MUST** 在 proto 与 descriptor 中可机器校验；任何一方实现缺失即视为版本不匹配，**MUST NOT** 静默降级。

## 14. 错误与边界验证

- **RUNTIME-ERROR-001 —** unary RPC（`Register`/`FetchCredentialGrant`/`GetCredentialSnapshot`/`Deliver`）失败 **MUST** 使用 canonical gRPC status：`INVALID_ARGUMENT`（缺字段/越界/格式错误）、`UNAUTHENTICATED`（令牌缺失或无效）、`PERMISSION_DENIED`（身份有效但无权，如 lintel 请求 grant）、`NOT_FOUND`（未知资源）、`FAILED_PRECONDITION`（状态前提不满足，如注册窗口外、Attempt 非 Running、版本不匹配）、`INTERNAL`（瞬态内部错误）。流内拒绝使用封闭枚举（`HelloRejectReason`/`AttemptRejectReason`/`UploadRejectReason`/`BrowserCloseReason`/`BrowserSubExecutionRejectReason`）；`*_UNSPECIFIED=0` 在请求/拒绝响应需要原因时一律无效（RUNTIME-VALIDATION-004）。detail **MUST NOT** 含秘密。
- **RUNTIME-VALIDATION-001 —** `contracts/runtime.proto` **MUST** 通过 proto 编译与 lint（buf STANDARD；本规格显式声明的例外：`SERVICE_SUFFIX`/`RPC_REQUEST_STANDARD_NAME`/`RPC_RESPONSE_STANDARD_NAME`（bidi 流用 envelope 而非 Request/Response 命名）、`RPC_REQUEST_RESPONSE_UNIQUE`（`Connect`/`Open` 双向同一信封类型）、`PACKAGE_DIRECTORY_MATCH`（README SPEC-STRUCTURE-003 固定 `contracts/` 目录，不按 package 建子目录）），并生成 descriptor 检查：service/method 数量与 streaming 方向、字段编号唯一且不复用、oneof/reserved 合法、枚举值与 `schema.sql` 对应 CHECK 枚举一致、新增字段（envelope `message_id`/`connection_epoch`/`correlation_id`/`boot_id`、`AttemptInputSnapshot`、`ResultPayload`、子执行与 grant 消息、`release_version` 字段）存在。（来源：Issue #8「机器契约目录」、Issue #11 交付纪律）
- **RUNTIME-VALIDATION-002 —** 协议交错用例 **MUST** 登记并覆盖：握手拒绝矩阵（令牌/slot/版本/epoch）、派发→Accept/Reject、同 boot 重连调和（不重派）、新 boot Interrupted、结果与取消提交顺序（成功先提交/取消先提交/detach+fence）、迟到结果只审计、轮换全流程（下发→确认→提升→旧连接关闭）与中止、替换与派发提交顺序、上传 commit 正向条件与 ABA/epoch 拒绝、BrowserSession 重附着与宽限期过期、新 boot 立即释放旧 boot 会话与身份锁、Stele relay 幂等与 204/4xx/5xx 分类（DATA-TX-016）。
- **RUNTIME-VALIDATION-003 —** 生产验证 **MUST** 覆盖原型未证明的路径：真实 ingress 的 gRPC 缓冲/keepalive、channel 复用下的流间干扰（必要时部署配置拆 channel）、noVNC 断线重附着与身份锁释放、轮换窗口内的断连恢复、grant 获取与 Attempt 终态的竞态、gRPC 透明重试导致的 envelope 重复投递（HTTP-VALIDATION-004 对应面）。
- **RUNTIME-VALIDATION-004 —** proto3 边界校验 **MUST** 在本文件与实现层明确：必填身份/目标 locator（如 attempt、scope、operation、identity、artifact、source、credential）`0`/负数拒绝；明确声明为可选引用的 locator 允许 `0 = 无`（`DispatchAttempt.requested_by_tool_call_id`、`AttemptProgress.tool_call_id`、被拒绝的 `BrowserSubExecutionAck.tool_call_id`/`child_attempt_id`），但负数仍拒绝，条件要求存在时必须 `>= 1`；必填 `uint64` 字段 `0` 拒绝（`generation`/`connection_epoch`/`size_bytes`/`snapshot_version` 等），明确允许无关联的 `correlation_id` 可为 0；`string` 必填字段空串拒绝；`bytes` digest 长度必须 32；枚举 `*_UNSPECIFIED=0` 值在请求中拒绝；`release_version` 非空；`oneof` 互斥由 proto3 保证（wire 层重复字段按 last-one-wins 处理，接收方 **MUST** 拒绝无法判定的畸形消息）。wire fixture（Go/Node 构造与解码）**MUST** 覆盖全部上述负例与各 finding 对应消息形态（旧流迟到 `AttemptAccept`/`CancelAck` 的 envelope epoch/boot 不匹配、输入/grant 快照、子执行请求/应答、Stele 版本、token 编码与 generation、新 boot 浏览器会话释放）。（来源：Issue #11 第一轮与第二轮审阅）
