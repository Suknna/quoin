# Quoin v1 — Agent 执行架构（architecture.md）

**状态：Draft**（Issue #13、#14、#15、#16）
**CATEGORY 前缀：`ARCH`**（SPEC-TRACE-002）
领域语言权威：[CONTEXT.md](../../../CONTEXT.md)
跨组件机器契约：[contracts/runtime.proto](contracts/runtime.proto)
Plinth 本地机器契约：[contracts/quoin/plinth/worker/v1/agent_worker.proto](contracts/quoin/plinth/worker/v1/agent_worker.proto)
持久化机器契约：[contracts/sql/schema.sql](contracts/sql/schema.sql)
浏览器 Tool 机器契约：[contracts/schemas/browser-tool.schema.json](contracts/schemas/browser-tool.schema.json)
Journey Catalog 机器契约：[contracts/schemas/journey-catalog.schema.json](contracts/schemas/journey-catalog.schema.json)
前端人类交互契约：[frontend.md](frontend.md)
安全、认证、秘密与审计契约：[security.md](security.md)

## 1. 权威边界

- **ARCH-AUTH-001 —** Quoin **MUST** 是用户、配置、连接、凭据、任务、消息、Evidence、Artifact、Knowledge 与全部执行状态的唯一持久权威。Plinth、Lintel、Stele 与前端都只能持有可丢弃投影，**MUST NOT** 建立可独立续跑或反向覆盖 Quoin 的第二历史。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-AUTH-002 —** Quoin **MUST** 创建并裁决每个 Execution Attempt；Runtime 只执行由 Quoin 当前 `attempt_id + runtime_slot + boot_id + connection_epoch` 租约授权的工作。所有重放、恢复、取消和迟到结果均以 Quoin 的 SQLite 提交顺序为准。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-AUTH-003 —** 用户可见最终正文只由 `investigation_messages`、Initial Analysis 输出、Inspection Report、Knowledge Candidate 与 Evidence 等领域记录拥有。Model/Tool Call 保存版本、输入引用、连接绑定、usage、时间、结构化终止、规范可见响应与输出引用，**MUST NOT** 保存或展示隐藏思维链。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-AUTH-004 —** Prompt、Agent loop、工具 schema 与 deterministic renderer 随发布代码版本化；Admin **MUST NOT** 在线编辑 Prompt、工具清单、判断下限或 Agent 执行图。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-AUTH-005 —** 临时 token delta、进度、工作区路径、Eino 内部对象和上下文投影都不是领域事实，断线或进程退出后可以丢失；最终恢复只读取 Quoin 权威记录和尚未过期的 Artifact 正文。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-AUTH-006 —** React 前端是同源 HTTP/SSE/noVNC 的可丢弃投影：可在当前 history entry 保存滚动、折叠和布局，在当前页面内存保存未提交非秘密输入，但 **MUST NOT** 持久化领域状态、秘密、通知、任务进度或恢复 checkpoint；刷新后的全部可恢复业务事实必须来自 Quoin。（来源：[Issue #15](https://github.com/Suknna/quoin/issues/15)、[frontend.md](frontend.md)）
- **ARCH-AUTH-007 —** 本文只定义组件责任与数据流；认证/授权、Cookie/CSRF、密码、根密钥/AEAD、秘密 reveal、审计、维护模式及恢复后的信任重建的安全属性与程序化基线由 [security.md](security.md) 独占定义，机器字段分别由 OpenAPI 与 SQLite Schema 独占定义。（来源：[Issue #16](https://github.com/Suknna/quoin/issues/16)、[security.md](security.md)）

## 2. 组件与数据流

- **ARCH-COMP-001 —** 浏览器/Operator → Quoin 使用同源 HTTP、SSE、assistant-ui message stream 与 noVNC WebSocket；Plinth 和 Lintel 只主动向 Quoin 建立出站 gRPC，Stele 只向 Quoin 提交告警 Delivery。Runtime 之间 **MUST NOT** 直连。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-COMP-002 —** Quoin 负责认证授权、幂等命令、SQLite 事务、Artifact store、Runtime lease、连接版本解析、凭据解密与外部工具路由裁决；Quoin **MUST NOT** 直接调用模型供应商。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-COMP-003 —** Plinth 是部署级唯一无状态 Agent Runtime：领取固定工作类型，在每个 Attempt 的全新工作区中运行一个 worker，并由 supervisor 代为调用模型和类型化外部工具。Plinth **MUST NOT** 保存跨 Attempt Session、checkpoint、Agent memory 或工作区。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-COMP-004 —** Lintel 是无模型的确定性浏览器执行器：只执行 Quoin 明确创建并持久化的 Browser Operation，声明固定浏览器 slot 容量，持有 persistent profile 文件与瞬时 Chromium/Playwright 状态，但 **MUST NOT** 成为 Browser Identity、operation、probe 结论或历史的第二持久权威。Plinth 请求 Browser Tool 时只能经 Quoin 转发。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[Issue #14](https://github.com/Suknna/quoin/issues/14)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-COMP-005 —** Stele 只做告警协议归一化与凭据 digest 快照校验；它不参与 Agent、调查或工具执行。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）

## 3. 浏览器执行边界

- **ARCH-BROWSER-001 —** v1 Agent 的浏览器只读能力 **MUST** 由 Browser Identity 在目标业务系统中的只读账号权限强制；Quoin/Lintel **MUST NOT** 通过 HTTP method、DOM、按钮文字或模型声明猜测副作用，也不得以该猜测替代只读凭据。Lintel 只暴露版本化封闭动作集，禁止任意 JavaScript、Playwright 代码、CDP、raw HTTP 与文件上传/下载通道。（来源：[Issue #14](https://github.com/Suknna/quoin/issues/14)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-BROWSER-002 —** 应用层 **MUST NOT** 维护 Browser Identity origin allowlist；从 `startUrl` 出发可进入基础设施网络边界内可达的 origin，网络可达范围由 Kubernetes NetworkPolicy、代理与网络设备拥有。Lintel **MUST** 记录每次顶层导航、重定向链与新窗口 origin，但不得复制网络策略为第二配置面。（来源：[Issue #14](https://github.com/Suknna/quoin/issues/14)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-BROWSER-003 —** 模型可见页面观察 **MUST** 是有界、版本化结构投影：URL/origin/title、page 列表、可访问性树与可见文本投影、短期 element reference、交互状态及导航/dialog/popup 事件；不得返回完整 HTML/DOM、Cookie、storage state、请求/响应正文或浏览器内部对象。Screenshot 只能由显式动作产生。（来源：[Issue #14](https://github.com/Suknna/quoin/issues/14)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-BROWSER-004 —** 每次 manual login、Journey 或整个 Exploration Session **MUST** 启动独立 Chromium 进程并独占打开目标 persistent profile；进程、tab、CDP session、trace 与内存状态不跨 operation 复用。profile generation 是 Lintel 卷上的可变文件状态配合 Quoin 不可变 marker，不是可并发挂载的快照。（来源：[Issue #14](https://github.com/Suknna/quoin/issues/14)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-BROWSER-005 —** Quoin 是全局浏览器容量队列与身份锁的唯一裁决者；Lintel 只声明固定 slot 总数并报告占用。所有四类容量请求在派发前先创建同一 `browser_operations` 行，容量不足只按 `browser_operations.id` 的 SQLite 提交创建序使用全局 FIFO，`requestedAt` 只作显示时间；Run/Test Run 的等待提示直接由该行投影，不参与调度。不设租户配额、预留或抢占；同一 Browser Identity 已占用则立即返回 `IdentityBusy`，不得进入另一条无界身份队列。（来源：[Issue #14](https://github.com/Suknna/quoin/issues/14)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-BROWSER-006 —** Browser Identity 的稳定身份、不可变配置 Revision 与不可变 profile Generation **MUST** 分离。Revision 冻结 `startUrl`、版本化 authentication-probe Journey 与 typed params；Operation 准入时冻结实际 revision/profile。配置切换不制造 generation；profile 内容只能经同一操作者的 active manual-login operation + 成功 probe 显式发布。（来源：[Issue #14](https://github.com/Suknna/quoin/issues/14) Q14.1/Q14.6/Q14.13）
- **ARCH-BROWSER-007 —** authentication probe 是 Journey Catalog 中 `purpose=authentication_probe` 的唯一机器实现，结果仅为 `Authenticated|Unauthenticated|Indeterminate`。只有明确 `Unauthenticated` 或 profile 文件/revision 确定性不兼容才能把身份置 `AuthenticationRequired`；Runtime/页面/选择器技术故障只形成可见缺口，不得伪造退出登录事实。（来源：[Issue #14](https://github.com/Suknna/quoin/issues/14) Q14.2/Q14.4/Q14.7/Q14.17/Q14.23）
- **ARCH-BROWSER-008 —** Exploration 是父 Investigation Attempt 下一个有状态 Browser Operation；第一个 Browser Tool Call 创建 Session，后续每个 Tool Call 仍各自创建一个 `browser_exploration` 子 Attempt 并执行一个封闭动作。父终态、取消、lease/Runtime 失效或显式 close 必须提交领域终态并停止 Session；身份锁和物理容量只有在 Lintel 确认对应 operation 已停止或新 boot 证明旧进程不存在后释放；可恢复元素/导航错误只返回 Tool Result，不终止 Session。（来源：[Issue #14](https://github.com/Suknna/quoin/issues/14) Q14.3/Q14.24）
- **ARCH-BROWSER-009 —** manual-login operation 绑定发起用户且同时最多一个 noVNC attachment；只允许同一用户在同一 boot 宽限期内重附着，禁止旁观、接管和跨进程重启恢复。进入人工登录立即把身份置 `AuthenticationRequired`；未发布关闭、取消、宽限期到期或新 boot 均保持该状态。（来源：[Issue #14](https://github.com/Suknna/quoin/issues/14) Q14.1/Q14.12/Q14.19）
- **ARCH-BROWSER-010 —** Journey 的 typed params/output/probe 结果必须由单一版本化 Catalog 声明并由 Quoin 按 digest 验证；禁止业务 YAML 另行定义输出。每个 Exploration Session 只有一份在结束时原子提交的连续敏感 trace，子 Attempt 经同一 Browser Operation 派生引用而不携带未完成 Artifact locator；失败 Journey 只有一份强制诊断 trace；强制 trace 未提交时不得产生成功或伪装成原步骤错误。结构化动作日志不得复制输入值或页面正文。（来源：[Issue #14](https://github.com/Suknna/quoin/issues/14) Q14.11/Q14.18/Q14.21/Q14.22）

## 4. Plinth supervisor / worker 边界

- **ARCH-WORKER-001 —** 每个 Agent 工作模式的 Plinth Attempt（Initial Analysis、Investigation、Inspection Analysis、Knowledge Extraction） **MUST** 启动一个全新 worker 进程；Attempt 终态后该进程与工作区 **MUST** 销毁。技术重试创建新 Attempt、新 worker 与新工作区，**MUST NOT** 恢复 Eino 内存状态。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-WORKER-002 —** supervisor 独占 Runtime token、模型 API key、Thanos Basic Auth、Kubernetes credential 与 Artifact 下载能力；worker 只能接收冻结的非秘密输入、一次性工作区、固定工具 schema 和本地协议。秘密 **MUST NOT** 进入 worker 环境、参数、工作区、模型上下文、Artifact、Evidence 或普通日志。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-WORKER-003 —** worker 只拥有 `bash`、`read`、`write`、`grep` 等无凭据工作区工具。模型调用以及 Knowledge/Artifact/Thanos/Kubernetes/Browser 类型化工具分别由 supervisor 经 Quoin 或 Quoin→Lintel 执行；worker **MUST NOT** 获得 kubeconfig、外部通用 HTTP、Quoin token 或任意连接选择接口。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-WORKER-004 —** supervisor 与 worker **MUST** 使用 [contracts/quoin/plinth/worker/v1/agent_worker.proto](contracts/quoin/plinth/worker/v1/agent_worker.proto) 的 stdin/stdout framed protobuf；stdout 只承载协议，stderr 只承载有界非秘密诊断。未知 oneof、重复/倒退 `message_id`、Attempt 不匹配、帧超出部署级内存边界或 protobuf 解码失败都 **MUST** 终止 worker 并使 Attempt 进入结构化技术失败。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-WORKER-005 —** 本地帧是 4-byte unsigned big-endian payload 长度加 protobuf payload。实现 **MUST** 流式读取完整 frame、设置部署级有界 frame buffer、拒绝长度溢出，且 **MUST NOT** 把半帧、stderr 或 EOF 当作成功。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-WORKER-006 —** `StartAttempt` **MUST** 是 supervisor 首帧，`StartAttemptAck` **MUST** 是 worker 首个响应；在成功 Ack 前 worker 不得提出模型或工具请求。一个 worker 只接受一个 Attempt，任何第二个 `StartAttempt` 都是协议错误。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-WORKER-007 —** v1 在 Kubernetes Restricted PSS 下让 supervisor/worker 同容器、同 uid 运行，并由 worker 在处理任何 Attempt 输入前 fail-closed 建立 `no_new_privs`、Landlock ABI >= 6（只允许既定只读运行时路径与当前工作区、限制 ptrace domain、signal 和 abstract Unix socket）及进程内 seccomp（拒绝外部网络与 `ptrace`/`process_vm_*`/`kcmp`/`pidfd_getfd` 等跨进程能力）。supervisor **MUST** 清空 worker 环境、设置自身不可 dump、只继承 stdin/stdout/stderr、使用独立进程组和仅本 Attempt 可写工作区；内核必须 >= 6.12 且启用 Landlock。v1 **MUST NOT** 引入 user namespace、bubblewrap、额外 worker daemon 或第二套本地协议。（来源：Issue #13 Q13.4 A、Linux Landlock/Yama/procfs、Go `os/exec` 与 Kubernetes Restricted PSS 研究）
- **ARCH-WORKER-008 —** Plinth readiness 与每个新 worker 的 `StartAttemptAck` 前都 **MUST** 运行真实隔离自检：读取 supervisor 的 `/proc/<pid>/{environ,fd,mem,maps,ns}`、向 supervisor/域外进程发信号、建立外部网络连接、写入工作区外路径以及继承非 stdio FD 必须全部失败；工作区内读写与 framed stdio 必须成功。任一检查失败都以 `sandbox_unavailable` 拒绝 readiness/Attempt，**MUST NOT** 依赖 Yama 默认值、配置声明或静默降级。该 profile 不提供独立 PID namespace，worker 可见的世界可读 `/proc` 元数据（如 PID、状态、cgroup 与资源限制）是 v1 明确接受的残余边界。（来源：Issue #13 Q13.4 A）

## 5. 固定工作模式

| mode | worker | 模型循环 | 实时外部工具 | 输入与输出 |
| --- | --- | --- | --- | --- |
| Initial Analysis | 是 | 最小顺序 loop | Knowledge、全局 Thanos；**无 Kubernetes/Browser** | 冻结 Occurrence、当前业务系统/Label Contract、已有 Evidence；输出 Initial Analysis |
| Investigation | 是 | 最小顺序 loop | Knowledge、全局 Thanos、业务系统绑定的 Kubernetes、Browser；工作区工具 | 当前有效消息分支、来源引用、Artifact；输出 assistant Message |
| Inspection Analysis | 是 | 固定分析调用，可使用工作区工具 | Knowledge；**不重新查询 Thanos/Kubernetes/Browser** | 本 Run 冻结 Evidence；输出不可变 Report |
| Knowledge Extraction | 是 | 固定提取调用，可使用工作区工具 | **无实时运维工具** | 冻结来源材料；输出 Candidate proposals |
| Embedding | 否 | 无 Agent/Tool loop | supervisor 直接调用 active provider 的 Embedding API | 冻结 KnowledgeVersion 正文；输出向量与模型身份 |
| Model Provider Probe | 否 | 无 Agent/Tool loop | supervisor 对待启用 revision 执行真实 Chat/Embedding 黑盒探测 | 输出不可变 capability probe；不产生领域分析 |

- **ARCH-MODE-001 —** 上表是服务端固定矩阵，不是用户配置。Attempt 的 `type/scope_type`、input `schema_kind`、结果 `schema_kind` 与工具 schema digest **MUST** 闭合匹配；不匹配时在调用模型前失败。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-MODE-002 —** 一个部署只有一个 Agent 实现和一个 Plinth Runtime。固定 mode 可以复用同一引擎，但 **MUST NOT** 演化为动态多 Agent、通用 DAG、Eino Graph checkpoint 或用户可编排工作流。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-MODE-003 —** 不设置 Agent 总时长、总步骤、总 Tool Call、总 Artifact 或总 token 的产品上限。只允许每次外部调用 deadline、有界同请求重试，以及部署级并发、lease、背压和内存保护。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-MODE-004 —** 多 Tool Call **MUST** 按模型响应顺序逐个持久化和执行；同一 Attempt 内不得并行执行模型返回的 Tool Call。一个 Tool Call 内对同一 Business System 的多个 Kubernetes 绑定可由 supervisor 按稳定 connection ID 顺序查询并聚合，但不得把连接选择交给模型。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）

## 6. Eino 与模型调用

- **ARCH-AGENT-001 —** v1 锁定 `github.com/cloudwego/eino v0.9.13` 与 `github.com/cloudwego/eino-ext/components/model/openai v0.1.13`。Plinth 复用 ChatModel、OpenAI-compatible adapter、Message/Tool schema、stream 与 callback/usage 契约；**MUST NOT** 使用 `adk.ChatModelAgent/Runner` 的强制默认 20 轮执行循环。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-AGENT-002 —** worker 实现一个最小顺序 loop：构造上下文 → 请求一次 ChatModel → 若无 Tool Call 则提出最终结果；若有 Tool Call，则按顺序逐个执行并把已提交结果加入下一次模型输入。循环只因最终结果、用户取消、不可恢复错误或上下文无法容纳固定内容而终止。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-AGENT-003 —** 模型供应商只允许一个 enabled/current `model_provider` 连接。Chat Model revision 的 streaming 与 native tool-calling 能力 **MUST** 由 Quoin/Plinth 真实探测并持久化；客户端提交的 `toolsEnabled` **MUST NOT** 成为能力权威。模型列表与可用元数据优先从 OpenAI-compatible `/v1/models` 自动发现；接口缺失或 metadata 不足时才允许 Admin 补充 context window/output budget，因为该接口没有统一可靠字段。模型 ID、输出预算与 generation 参数由 Quoin 固定的 current capability/grant 与 Agent 契约决定；worker/模型不得在本地请求中选择或覆盖。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-AGENT-004 —** 不做跨模型或跨供应商 fallback。只有明确可重试、未产生任何可见 chunk 且 provider 调用没有不确定副作用时，supervisor 才可按既有有界退避重试；每次物理请求必须形成独立不可改写 Model Call 审计行。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-AGENT-005 —** provider request ID、模型 ID、实际 ConnectionRevision/CredentialGeneration、Prompt/renderer/agent/tool schema 版本或 digest、有序输入引用、开始结束、usage、延迟、物理 retry 序号与结构化终止原因 **MUST** 持久化。隐藏 reasoning/thinking **MUST** 在 adapter 边界丢弃，不得进入本地协议、token stream、日志或数据库。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-AGENT-006 —** `ChatModelStarted` 只能在 Quoin 已提交 `model_calls` 行后发送给 worker；provider 调用不得早于该提交。完整成功响应必须先由 supervisor 提交 Model Call 终态以及模型返回的全部 pending Tool Call，再交给 worker 继续。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-AGENT-007 —** 模型同时返回可见文字和 Tool Call 时，文字只可作为当前观察者的瞬态 delta；持久重建的 assistant tool-call message 只由 Model Call 与 Tool Call 记录构造。这样恢复不依赖未提交的中间旁白。无 Tool Call 的最终可见正文只由最终领域结果事务持久化。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-AGENT-008 —** token delta 是可丢弃观察事件：Quoin 可以在无 HTTP 观察者或下游背压时停止转发，但 **MUST NOT** 影响 worker/supervisor 独立组装完整响应。部分 token 后失败不得创建有效 assistant Message。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）

## 7. Investigation Chat 与领域目标路由

- **ARCH-CHAT-001 —** Investigation 页面 **MUST** 是主流 Chat 页面。直接新建后用户立即用自然语言输入，不得在进入对话前增加 Business System、连接、告警、Evidence、模型或工具选择向导。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-CHAT-002 —** 现有告警、Initial Analysis、Evidence 与 Inspection 入口保持；从这些对象进入 Chat 时，Quoin 自动写入不可变 Investigation source refs。直接新建可以没有结构化来源；v1 不提供进入 Chat 后追加来源的独立配置流程。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-CHAT-003 —** 模型可以从用户自然语言和 Quoin 提供的业务系统 catalog 识别人类领域目标，只能在 Tool 参数中提交 Business System key/名称或其他人类对象 locator；模型 **MUST NOT** 查看或选择 Connection、ConnectionRevision、CredentialGeneration、grant 或凭据。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-CHAT-004 —** Quoin 是路由权威：一个部署有一个全局 Thanos；每个 Business System 可由 Admin 绑定零到多个 Kubernetes Connection。Quoin 在 Tool Call 持久化事务中把领域目标确定性解析为当时 current revision/generation 并冻结为 Attempt connection grants。名称不存在或存在真实歧义时返回结构化 `target_not_found/target_ambiguous` 结果，由模型在 Chat 中自然追问，**MUST NOT** 猜连接。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-CHAT-005 —** Thanos Tool 必须携带 Business System 目标；Quoin/supervisor 根据该系统当时 published Label Contract 注入或校验强制 matcher。模型不得提供绕过 matcher 的 raw endpoint、认证头或连接 ID。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-CHAT-006 —** Kubernetes Tool 只在 Investigation mode 可用，接收 Business System 与 Kubernetes 资源领域参数；supervisor 对该系统全部绑定连接使用只读凭据按稳定顺序执行。v1 只提供发现、get/list、events 与 logs 等只读操作，不提供 exec、port-forward、apply、delete、patch 或通用 kubeconfig/shell。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-CHAT-007 —** Chat 页的持久工具时间线 **MUST** 从权威 `model_calls`/`tool_calls` 重建，不依赖瞬态 token delta、worker 内存或 `task_change_log`；HTTP 以 Investigation Attempt 下的独立游标分页 Tool Call 子资源返回工具名、参数、状态、模型可见有界结果/Artifact 引用与时间，避免详情响应嵌入无界历史。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、`http-api.md` HTTP-PAGE-005）

## 8. 非破坏性上下文投影

- **ARCH-CONTEXT-001 —** 完整 Investigation 消息、Tool Call、Evidence 与 Artifact 引用始终保留在 Quoin；每次 Model Call 的输入是从该唯一历史临时派生的非持久投影。系统 **MUST NOT** 生成或保存 compaction summary、replacement history、rolling memory 或跨 Attempt Agent cache。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-CONTEXT-002 —** 投影固定保留：system/Prompt 契约、当前用户消息、当前 Attempt 内已提交的 Model/Tool protocol group、服务端工具 schema及本次明确输入对象。其余历史按 active head 分支划分为完整 user turn；预算不足时按提交时间从最旧 turn 开始整体淘汰。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-CONTEXT-003 —** assistant Tool Call 与对应全部 Tool Result 是不可切割 protocol group；不得留下孤立 Tool Result、缺少响应的 Tool Call 或只保留被 Undo/withdrawn 分支的一部分。当前用户 turn 本身不得被淘汰。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-CONTEXT-004 —** 投影使用 active provider capability 的 `context_window_tokens - max_output_tokens` 作为输入预算，扣除 system/tool schema 与固定输入后从最新 turn 向前装入。token 估算器不是事实权威；若 provider 在未输出任何 chunk 前返回 context overflow，supervisor 可以在同一逻辑调用下再淘汰一个最旧完整 turn并形成新的物理 Model Call 审计。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-CONTEXT-005 —** 固定内容或当前 turn 自身仍无法容纳时 Attempt 以 `ContextTooLarge` 失败，错误告诉用户影响与恢复方式；不得暗中摘要、截断当前用户正文或切换更大模型。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-CONTEXT-006 —** 每个 Model Call **MUST** 持久化有序 input-item refs、各对象 revision/digest、预算与估算值，以及 supervisor 对实际 provider 规范请求计算的 digest；由固定 renderer/version/source 可独立重建核对，但上下文投影正文不成为可写历史。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）

## 9. 长 Tool 输出与 Artifact

- **ARCH-OUTPUT-001 —** Tool 输出字节数 `> 50 * 1024` 或行数 `> 2000` 时，执行端 **MUST** 使用有界流式 accumulator，把完整原始字节提交到 Quoin 现有 Artifact store；worker-local Tool 可先写入本 Attempt 一次性工作区再由 supervisor 流式上传；不得整体缓冲到内存，也不得建立 `.quoin/tool-results`、per-Attempt 持久目录或第二索引文件。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-OUTPUT-002 —** 长输出 Artifact 使用既有内容寻址 `artifact_blobs` 与逻辑 `artifacts`，`kind=tool_result`、`retention_kind=generated`，由 Tool Call 或 Execution Attempt 作为 owner；既有 generated Artifact 默认 90 天正文保留与部署级 GC 继续适用。ToolCall→Artifact 关系只由 Quoin 数据库拥有。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-OUTPUT-003 —** 模型上下文只接收有界预览、总行数/字节数、media type、SHA-256 与 Artifact locator。文件/搜索类结果优先保留 head，日志/命令类优先保留 tail；预览明确是派生内容，不能替代完整 Artifact。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-OUTPUT-004 —** 模型可通过固定 `artifact_read`/`artifact_grep` 工具按需读取 Artifact 文本。worker 只提交 Artifact locator、行范围或 RE2 pattern；supervisor 经 Attempt-scoped `ArtifactService.ReadText/GrepText` 调用 Quoin，返回有界片段与完整 Artifact size/hash/eof/truncated。任何一层 **MUST NOT** 暴露 Quoin PV/storage_key 或让 worker 任意遍历 Artifact store。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-OUTPUT-005 —** 上传、hash、fsync 或数据库登记失败时 Tool Call **MUST** 失败，不能只把截断预览当作成功结果。Artifact 提交先于 Tool Call 成功；取消 fence 前已经提交的 Artifact/Evidence 保留，fence 后完成的迟到结果只能按取消事务契约审计，不能进入后续模型输入。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-OUTPUT-006 —** `body_expired` Artifact 的逻辑元数据和引用继续存在；模型读取正文时返回结构化 `artifact_body_expired`。GC 后不得从工作区缓存或 Plinth 本地状态恢复为新的权威正文。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）

## 10. Tool Call、Evidence 与 Artifact 提交顺序

- **ARCH-TOOL-001 —** 模型返回的全部 Tool Call 必须先按响应顺序在一个 Quoin 事务中创建为 `pending`，然后 worker 才收到可继续响应；任何本地、supervisor 或 Browser 执行都不得早于持久 Tool Call ID。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-TOOL-002 —** worker 必须按 PreparedToolCall 顺序发送本地协议 `ExecuteToolCall`；supervisor 经 `BeginToolCall → BeginToolCallAck` 将既有 pending Tool Call 推进 running 后回 `ToolCallStarted`。本地工具收到该许可后才执行，大输出由 supervisor 上传 Artifact；类型化外部工具由 supervisor 执行。Browser Tool 由 Quoin 把既有 parent Tool Call 绑定到 child Browser Attempt，不能另建重复 parent Tool Call。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-TOOL-003 —** 外部观测只有在 Tool Call 开始事务已冻结实际 connection grant 与参数、并且完成事务已复核该 binding 且原子提交结果或 Artifact、Evidence 与 Tool Call 终态后才可返回模型。Evidence 必须绑定产生它的 Tool Call 和精确 ConnectionRevision/CredentialGeneration；模型不得自行伪造 Evidence ID。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-TOOL-004 —** Tool Result 返回 worker 前 Quoin 必须已经提交。控制流断开后的重放按 `(attempt_id, model_call_id, provider_tool_call_id)` 找回原 Tool Call；重复完成使用相同摘要返回原终态，不得再次执行外部副作用。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-TOOL-005 —** Knowledge 查询只返回已确认且当前可复用 KnowledgeVersion locator 与正文投影；它不产生 Evidence。Thanos/Kubernetes/Browser 观测产生 Evidence；本地工作区计算仅在其输出需要跨 Attempt 存活时产生 Artifact。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）

## 11. 输入快照、连接 grant 与 Artifact 读取

- **ARCH-INPUT-001 —** Quoin 在 Attempt 创建事务中冻结不可变 `attempt_input_snapshots` 与有序 `attempt_input_items`；派发前必须能从这些引用、历史不可变对象和 Artifact 重建相同 canonical JSON 与 digest。只保存 digest 而没有可解析输入引用不满足恢复契约。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-INPUT-002 —** 一个 Attempt 可以拥有多个 `attempt_connection_grants`。每个 binding 必须保存用途、Business System（如适用）、Connection、ConnectionRevision、CredentialGeneration 与非秘密 grant locator；grant 是该持久 binding 在当前 connection epoch 下的短期能力投影，不是只存在内存的路由事实。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-INPUT-003 —** Model Provider binding 在派发前冻结；Thanos 与 Kubernetes binding 都只能在模型提出具体 Tool Call 后，由 Quoin 在该 Tool Call 持久化事务中按冻结参数确定性解析并追加。追加后不可修改；连接轮换只影响后续新 binding/new Attempt。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-INPUT-004 —** Plinth 只能经 Runtime `ArtifactService.ReadText/GrepText` 读取当前 Running Attempt 已由 `attempt_artifact_grants` 授权的输入或 Tool Result Artifact；每次调用复核 slot/Attempt/boot/epoch 与正文未过期，Runtime/worker 不得使用 storage key、物理路径或未授权 locator。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-INPUT-005 —** Read/Grep 返回有界原始 UTF-8 片段、完整 size/hash/media type 与 eof/truncated；worker 可继续按行读取，但不得把局部响应重标为完整正文。二进制或不受支持 media type 在 v1 返回结构化 unsupported，不隐式下载到工作区。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）

## 12. 取消、重试与崩溃恢复

- **ARCH-RECOVERY-001 —** 用户取消先由 Quoin 提交 cancellation fence；之后 supervisor 才向 worker 发送 `CancelWorker`、取消 provider/工具 context 并终止进程组。worker 自行退出、HTTP 断线或本地 AbortController 都不是领域取消。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-RECOVERY-002 —** fence 后不得开始新 Model/Tool Call。Attempt、active Model Call 与 pending/running Tool Call 必须由一个 Quoin 终态事务闭合；父 Investigation Attempt 进入非成功终态时，同一事务必须把未派发的 Browser 子 Attempt 直接取消、把已经 Running 的子 Attempt推进 `Cancelling` 并向 Lintel 发取消，任何迟到子结果都不得把父 Tool Call 或父 Attempt 改回成功。迟到 provider/tool 结果按提交顺序只形成允许的审计/Artifact/Evidence，不得创建成功消息或 Report。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-RECOVERY-003 —** Plinth/Quoin/control stream 崩溃后，失去有效 lease 的 Running Attempt 进入 `Interrupted`；技术重试创建新 Attempt并复用领域对象创建时的冻结 input snapshot。不得从 worker 内存、Eino Graph、临时工作区或未提交 token 恢复。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-RECOVERY-004 —** ResultProposal 与 ResultAck 丢失时，Runtime peer 使用相同 Attempt/digest 重放；Quoin 只通过一个领域提交入口原子封存最终输出、引用与终态。Journey Result 以 `browser_journey_results` 单行 ledger 保存 operation 唯一 digest/outcome（success 另引用 primary Evidence，gap 不制造 Evidence）；该 INSERT 由 SQL trigger 同一 statement 派生 check result、收口 Browser Operation，并把 `inspection_collection` Attempt 置 `Succeeded`（只表示合法结果已提交，check 的 ok/gap 独立表达业务结论）。Browser Tool Result 则以父 `quoin_browser` Tool Call 终态 UPDATE 为提交入口，同一 statement 推进子 Attempt，禁止先单独提交子 Attempt 终态。相同 digest/outcome 的重复 proposal 从 ledger 重建同一 Ack；不同 digest/outcome 冲突。typed output 只存在于 primary structured Evidence 正文，不建立第二份 output authority。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[Issue #14](https://github.com/Suknna/quoin/issues/14)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-RECOVERY-005 —** provider retry 与领域 Attempt retry 是两个不同层级：provider retry 仅限没有任何输出且明确属于 `timeout|rate_limited|transport_error` 的同一逻辑模型调用，或增加旧回合淘汰后的无输出 `context_overflow`；provider unavailable、取消/终态 fence、invalid response 与 Artifact commit failure 不得自动重试。Attempt retry 总是新 Attempt。v1 不做 supervisor 内部 Tool retry；瞬态 Tool 失败作为已提交 Tool Result 返回模型，模型若再次请求会产生新的 Tool Call 审计。任何重试都不得切换已冻结的 connection revision、credential generation、模型或输入快照。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-RECOVERY-006 —** `CompleteModelCallAck` 丢失时 supervisor 以相同 model_call_id/digest/outcome 重放，Quoin 从已封存 Model Call 与 Tool Call IDs 重建同一 Ack；内容变化必须冲突。浏览器 `ToolResultDelivery` 以 tool_call_id 幂等，父端回 Ack；断线后只在父 Attempt 仍 Running、Tool 已终态且尚无后续 Model Call 输入引用该 Tool Result 时重发，已有后续输入引用则证明已消费。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)）

## 13. 可观测性与就绪

- **ARCH-OBS-001 —** Plinth readiness 至少验证：Runtime token state 文件权限；Artifact 临时目录原子写；sandbox 自检；Eino/provider adapter 初始化；active model revision 的 streaming/native tool capability；控制流可建立。任一失败必须暴露具体阶段并停止领取新 Attempt。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-OBS-002 —** 指标至少覆盖 active worker、按 mode/terminal reason 的 Attempt、Model/Tool latency 与 retry、token usage、Artifact spill bytes、context-evicted turns、sandbox launch failures、control reconnect 与 cancellation latency；指标 label 不得包含用户正文、Prompt、Tool 参数、Artifact path 或凭据。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-OBS-003 —** 普通日志只记录 locator、阶段、版本、耗时和结构化原因；本地协议原始 payload、模型 messages、Tool 原文、Evidence 正文和 Artifact 内容不得进入日志。provider request ID 可作为非秘密诊断字段保存和展示。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）

## 14. 验证门

- **ARCH-VALIDATION-001 —** 机器契约至少通过 protobuf build/lint、SQLite 完整 Schema 装载/触发器对抗测试、OpenAPI lint 与 JSON Schema fixtures；不能以编译成功代替交错行为验证。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-VALIDATION-002 —** Plinth 实现必须对抗验证：Tool 持久化前不得执行；取消先/结果先两种提交序；部分 token 后失败；HTTP 观察者断开；ResultAck 丢失重放；多 Kubernetes binding；连接轮换并发；Artifact 上传/Read/Grep 中断；正文 GC；worker 越权读取 `/proc`/状态目录/网络；父取消与 Browser 子执行结果的两种提交顺序；Succeeded Attempt 拒绝任何 active Model/Tool/Browser 子执行；Tool protocol group 上下文切割。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）
- **ARCH-VALIDATION-003 —** 模型供应商验收必须在实际目标 OpenAI-compatible endpoint 黑盒验证 streaming、native tool call、取消传播、usage 与 request ID；Embedding 配置时另验证维度。`/v1/models` 成功或 SDK 可构造不等于这些行为可用。（来源：[Issue #13](https://github.com/Suknna/quoin/issues/13)、[CONTEXT.md](../../../CONTEXT.md)）

## 15. 上游证据与取舍

- Eino 稳定版与 adapter：[`cloudwego/eino@v0.9.13`](https://github.com/cloudwego/eino/tree/v0.9.13)、[`cloudwego/eino-ext/components/model/openai@v0.1.13`](https://github.com/cloudwego/eino-ext/tree/components/model/openai/v0.1.13)。自定义最小 loop 用于避免 `ChatModelAgent` 把非正 `MaxIterations` 解释为固定默认值，而不是重写 Message/OpenAI adapter。
- 非破坏性上下文投影参考 Pi `context` hook：[`pi-mono@e429d90b`](https://github.com/earendil-works/pi-mono/blob/e429d90b800f9a37c8a5812f4c9c10a8cdcc85a7/packages/coding-agent/src/core/extensions/runner.ts#L984-L999)。Quoin 不采用 Pi/OpenCode/Codex/Claude Code 的持久摘要或 replacement history。
- 长输出流式 accumulator 参考 Pi：[`output-accumulator.ts`](https://github.com/earendil-works/pi-mono/blob/e429d90b800f9a37c8a5812f4c9c10a8cdcc85a7/packages/coding-agent/src/core/tools/output-accumulator.ts#L19-L118)；`50 KiB / 2000 行`与 Read/Grep 提示参考 OpenCode：[`truncate.ts`](https://github.com/anomalyco/opencode/blob/e23586af2623f1bc2e8e6965d2d7acf7bd03d5c3/packages/opencode/src/tool/truncate.ts#L12-L147)。Quoin 把完整正文提交到既有 Artifact store，不复制它们的临时持久目录。
