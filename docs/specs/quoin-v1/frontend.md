# Quoin v1 — 前端工作台与交互状态规格（frontend.md）

**状态：Draft**

**CATEGORY 前缀：`UI`**（SPEC-TRACE-002）

本文件定义 Quoin Web UI 的人类交互契约。HTTP 请求、响应字段与错误码的唯一机器权威是 [`contracts/openapi.yaml`](./contracts/openapi.yaml)；持久化状态机与不可变历史由 [`persistence.md`](./persistence.md) 和 [`contracts/sql/schema.sql`](./contracts/sql/schema.sql) 定义。本文件只规定这些领域事实如何组织、展示和操作，不建立第二份领域状态。

- **UI-SCOPE-001 —** [`contracts/schemas/frontend-state.schema.json`](./contracts/schemas/frontend-state.schema.json) 的稳定 `$id` `https://quoin.local/schemas/frontend-state.schema.json` 是前端可观察投影状态的 JSON Schema 权威；它只能约束路由、面板、等待、对话、阅读层等 UI 投影，不得重定义 OpenAPI/SQLite 已拥有的领域事实或生命周期。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-SCOPE-002 —** 本文件拥有人类可见组织、反馈、恢复、焦点与动效语义；`frontend-state.schema.json` 只机械表达其中可封闭的形状。两者与 OpenAPI 枚举不一致时必须修正规格资产，不得由实现任选其一。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）

规范词 **MUST / MUST NOT / SHOULD / MAY** 按 RFC 2119 解释。

## 1. 使用者、目标与设计边界

- **UI-BOUNDARY-001 —** 前端面向第一次使用 Quoin 的 Operator/Admin。界面必须围绕“看见问题、理解影响、采取合法下一步”组织，不得把 ID、枚举、YAML path、连接参数或内部状态机直接翻译成主界面。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-BOUNDARY-002 —** 程序只展示、筛选和机械聚合权威事实；模型负责分析文字。前端不得根据失败比例、告警数量或 `ok/gap` 自行生成“健康/不健康”结论。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-BOUNDARY-003 —** v1 使用简体中文单语言，不建立 i18n 框架。代码、labels、annotations、协议状态、日志与上游错误保留原文，并在其周围提供中文解释。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-BOUNDARY-004 —** 颜色自动跟随 `prefers-color-scheme`，不提供应用内主题或密度设置。布局只把 shadcn Sidebar/Resizable 已有的折叠、隐藏、拖动、键盘调整与基于 `autoSaveId` 的浏览器本地 layout restore 打开，不建设第二套布局系统或服务端个人偏好。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-BOUNDARY-005 —** 不建设 Dashboard、通知中心、铃铛收件箱、已读状态、浏览器通知权限流程、配置卡片墙、通用 JSON 表单或第二套 YAML 编辑器。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）

## 2. 信息架构与路由

### 2.1 三栏工作台

- **UI-SHELL-001 —** 宽屏只有三层：第一栏全局图标导航；第二栏当前模块对象列表、筛选与主要创建操作；第三栏对象详情或工作区。第三栏占剩余空间；第一栏可折叠为 icon rail/offcanvas；第二栏可折叠、隐藏和调整宽度。可观察布局状态只保存第一、二栏百分比，第三栏始终由剩余空间派生，禁止另存会与总和漂移的第三值。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-SHELL-002 —** 全局模块固定为：告警 `/alerts`、调查 `/investigations`、巡检 `/inspections`、业务系统 `/business-systems`、知识 `/knowledge`、管理 `/admin`。Admin 显示六项；Operator 不显示管理。登录成功默认进入 `/alerts`，不增加首页。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-SHELL-003 —** 导航图标必须在 hover 与 focus 时显示中文名称。徽标只投影可操作权威状态，例如 active/failed Attempt、巡检 gap、待确认知识、未确认接入问题、Runtime/备份故障；不得保存独立通知事实。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-SHELL-004 —** URL 未选对象时不得自动选择列表第一项。第三栏使用 shadcn `Empty`：一个语义图标、短标题、自然语言描述与至多一个主操作；不得纯空白或伪装成 Dashboard。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-SHELL-005 —** 第二栏普通对象列表使用语义化链接/按钮，不把页面实现为 `application`、`grid` 或自定义 roving-focus 应用。列表采用紧凑两行项目而非横向大表格或大卡片。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）

### 2.2 URL 与浏览器历史

- **UI-ROUTE-001 —** 模块、业务筛选、实际存在的可切换排序、选中对象和持久化长内容必须进入确定性 URL。v1 当前列表排序由领域契约固定，界面不得制造没有 OpenAPI 参数的统一排序状态。分页 cursor、滚动位置、临时展开和未提交输入只属于当前浏览器 history entry，不进入可分享 query。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-ROUTE-002 —** 跨模块关联跳转使用浏览器原生历史。后退必须回到来源对象、筛选和滚动位置；不得维护第二套面包屑栈或默认新开标签页。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-ROUTE-003 —** Evidence、Initial Analysis 输出、Inspection Report、Knowledge/Version、配置版本和 Observed Resource 使用嵌套路由铺满工作台。至少支持以下路由形态；具体 path parameter 对应 OpenAPI locator：（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
  - `/investigations/:investigationId/evidence/:evidenceId`
  - `/alerts/:occurrenceId/analyses/:analysisId`
  - `/inspections/runs/:runId/reports/:reportId`
  - `/knowledge/:knowledgeId/versions/:versionId`
  - `/business-systems/:systemKey/configs/:configVersionId`
  - `/business-systems/:systemKey/resources/:resourceId`
- **UI-ROUTE-004 —** 一次性秘密、未提交上传、表单草稿和对话输入不得进入 URL。工作台始终为一个浏览器 Page；铺满层不是新窗口或新的应用实例。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）

### 2.3 窄屏退化

- **UI-RESPONSIVE-001 —** 窄屏时第一栏变为 offcanvas drawer，第二栏列表与第三栏详情逐层全屏显示；详情顶部始终提供明确“返回列表”。320 CSS px 宽度不得出现页面级水平滚动，代码块/宽表格/noVNC 画布可在自身区域滚动或缩放。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-RESPONSIVE-002 —** 窄屏 noVNC 显示“复杂登录建议使用桌面”但不得禁用入口；复用 noVNC 触控与虚拟键盘，不增加复制账号、密码或 URL 的替代流程。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）

## 3. 通用列表、表单与反馈

### 3.1 列表与实时变化

- **UI-LIST-001 —** cursor 列表首次只读一页，底部提供明确“加载更多”。不得自动无限滚动或把 cursor 伪装为页码；加载结果追加到同一连续列表。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-LIST-002 —** 用户不在列表顶部时，新项目不得推动当前内容或抢焦点；显示“有 N 条新内容”，用户触发后合并并回到顶部。在顶部时可以直接合并，但不得自动打开详情。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-LIST-003 —** 列表 row version 变化时原位调和。对象已不存在或不能继续操作时，保留已渲染内容，显示明确状态条，禁用非法操作并提供返回列表；Session/权限撤销不允许继续读取旧页面。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-LIST-004 —** 主时间使用浏览器本地时区的明确绝对时间并显示时区；相对时间只作辅助。hover/focus 或详情提供原始 offset、UTC 和复制；持续时间单独显示。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）

### 3.2 表单、确认与结果

- **UI-FORM-001 —** 普通设置显式提交，不逐字段自动保存。客户端只做可机械判断的即时校验，服务端响应是权威。失败保留全部非秘密输入，页首显示错误摘要并聚焦首个字段错误；成功状态必须持续显示在对象中，toast 不能成为唯一证据。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-FORM-002 —** 只有立即撤销 Session/凭据、停用当前能力、替换 Runtime、切换发布指针、停止知识复用或丢弃未保存输入等高影响操作弹确认。确认直接说明对象、即时影响和恢复方式；不得要求输入对象名。读取、下载、测试、启动、重试和接入问题 acknowledge 不弹二次确认。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-FORM-003 —** 非即时操作提交后立即显示真实 pending/stage、用户是否还需行动及可用 Stop/Cancel。可计算时显示真实进度；不可计算时只显示阶段，不伪造百分比或完成时间。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-FORM-004 —** 后台终态始终落在所属对象与列表。用户在其他模块时可显示非阻塞 toast 与导航徽标并提供返回对象入口，但不得自动跳转。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-FORM-005 —** Stop/Cancel 提交后控件立即变为不可重复触发的“正在停止”，保留已完成内容与当前阶段；只有服务端确认 cancellation fence/终态后显示 `Cancelled`。提交失败恢复合法操作并解释原因。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-FORM-006 —** 部分完成不得映射为统一“部分成功”状态。界面并列显示父对象真实状态、每个子步骤终态与机械计数，例如“采证完成：8 项 ok / 2 项 gap；分析失败”；已完成 Evidence/Artifact 保持可读。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）

### 3.3 网络恢复与错误

- **UI-ERROR-001 —** SSE 断线、回放、cursor 过期和完整快照刷新在正常恢复期间静默执行，不向用户暴露 SSE、sequence、cursor 或 resync。当前数据与焦点不得被清空或重置；连续恢复失败才进入普通内部错误流程。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-ERROR-002 —** 只对网络中断、429、可恢复 5xx 与结果不确定的命令自动恢复。读取或复用同一 `client_command_id` 的命令总计尝试三次，间隔 1 秒、2 秒；验证错误、权限不足、密码错误等确定性结果不重试。结果不确定时不得生成新 command ID。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-ERROR-003 —** 三次均失败后持续显示“内部错误”、普通语言原因、“如持续发生请联系管理员”、立即重试与可复制诊断。停留 10 秒后自动重读当前对象一次；仍失败才回到所属列表上一层。用户刷新、后退、离开页面或成功重试后必须取消旧倒计时，不得在新页面继续回退。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-ERROR-004 —** 主错误先说明发生了什么、影响与下一步。可展开技术详情只包含稳定错误码、request/Attempt ID、阶段、必要上游原文及复制诊断；不得显示服务端堆栈、Authorization、Cookie、秘密或无关完整请求正文。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）

## 4. 认证、权限与个人入口

- **UI-AUTH-001 —** 无权写操作不显示；直接访问受限 URL 时显示工作区级 403，说明所需角色和返回位置，不伪装为 404，也不建设申请权限流程。服务端始终重新授权。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-AUTH-002 —** Session 失效时用不可绕过的重新登录层遮蔽受保护内容。仅在当前页面内存保留非秘密正文、附件引用和表单输入；同一 principal 重新登录后恢复原 URL 与输入，principal 改变或页面刷新则丢弃；一次性秘密永不恢复。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-AUTH-003 —** 临时密码验证后仍停留在登录页并进入“设置新密码”阶段；成功建立正常 Session 后才加载工作台。页面展示实际 15–128 Unicode 规则，允许粘贴、自动填充和密码管理器。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-AUTH-004 —** 全局导航底部头像菜单只含当前身份/角色、修改密码、我的 Session、审计记录和退出。“我的 Session”以全工作台层列出设备/浏览器、创建、最后活动与当前项，可逐个撤销其他 Session并说明 SSE/WebSocket 会立即断开。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-AUTH-005 —** `/audit` 是头像菜单打开的全工作台列表，不是第七个模块。Operator/Admin 都可按操作者类型、动作和时间筛选并查看结构化详情。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）

## 5. 告警工作台

- **UI-ALERT-001 —** `/alerts` 第二栏顶部使用“当前 / 历史 / 接入问题”三段；前两段只投影 Firing/Resolved Occurrence，接入问题是独立查询，不得伪装为第三种生命周期状态。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-ALERT-002 —** 普通告警项主行显示状态图标+文字、`alertname` 与关键时间；次行显示业务系统、可用的 severity 原值与必要徽标。选中、hover、focus 和未读变化不得只靠颜色。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-ALERT-003 —** 普通告警筛选只提供“当前/历史”、可搜索业务系统 combobox 与清除入口；不得展示 OpenAPI 不支持的全文、severity 排序或任意 label query。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-ALERT-004 —** 告警详情标题区提供一个“初步分析”主操作。运行后原位显示真实阶段与取消；正文优先显示最新成功输出，旧版本按时间列出并可展开，失败/取消记录不得消失。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-ALERT-005 —** 从告警或其 Initial Analysis 创建调查进入 `/investigations/new`，输入框上方显示不可变来源项并直接聚焦；首条消息被接受前不得创建空 Investigation。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-ALERT-006 —** 接入问题未确认项在前，列表显示 kind、逻辑来源、首次/最近发生时间和重复次数；详情先用普通语言解释影响，再展示 Delivery、item index、labels/fingerprint、截断数量与不可变事件历史。Operator 只读；Admin 原位“标记已处理”，完成后仍可用已处理筛选查看。同 signature 再发生时出现新的待处理对象。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）

## 6. 调查对话

- **UI-CHAT-001 —** Investigation 列表使用服务端机械派生 `displayTitle` 与 `lastActivityAt`；标题来自当前分支第一条有效用户消息，空白时回退到来源或“新调查 + 创建时间”，不新增可写标题或模型标题调用。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-CHAT-002 —** 点击“新建调查”只进入 `/investigations/new` 客户端空白输入。首条正文/附件被服务端接受时才原子创建 Investigation、首条 user Message、Attempt、head 与可选来源；未发送离开不留记录。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-CHAT-003 —** 用户在底部时流式内容自动跟随；向上阅读后停止滚动并显示“查看新回复”，后续 token 不得改变阅读位置或焦点。触发后回到底部继续跟随。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-CHAT-004 —** Tool Call 作为消息内状态卡显示工具名、真实阶段、耗时/终态和普通语言摘要；原始参数、输出与诊断原位折叠。窄屏不跳独立工具页。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-CHAT-005 —** 点击发送到服务端受理前，发送按钮保留原图标但禁用并投影 `requestPending`，此时不得显示可取消的 Attempt；受理并产生 active Attempt 后才原位变为方形停止按钮，点击后遵守 UI-FORM-005。failed Attempt 对应用户消息左侧显示环形箭头 Retry，使用同一消息创建新 Attempt。只有最新已发送 user 回合下方显示 Undo；确认后撤回该回合及后继，正文与全部附件恢复到输入区，旧分支保留审计。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-CHAT-006 —** 输入区支持选择、拖放和多份文本附件；正文或附件任一非空即可发送。发送前附件以文件图标悬浮于输入区并可预览/移除；发送后在消息下排列，超过三份折叠显示数量。不得设置附件个数上限，但每个附件及同一消息附件正文合计都受默认 10 MiB、部署可调边界。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-CHAT-007 —** 一次粘贴达到 16 KiB 或 200 行任一边界时，浏览器生成可预览、可移除的临时 `.txt` attachment；逐字输入不得在过程中突然转换。无效 UTF-8、NUL 或超限错误就近显示并保留正文。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-CHAT-008 —** Undo 后重发复用相同不可变 Artifact 字节，只建立新的消息—附件引用；不得重复复制 BLOB，已撤回分支的旧引用继续保留。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-CHAT-009 —** 点击 Evidence 进入确定性嵌套路由并铺满工作台；通过 `getEvidence` 读取来源、采集时间、工具/连接逻辑名、完整性、warnings/errors 与 inline JSON 或 Artifact 定位，Artifact 正文再经 `downloadArtifactContent` 复制/下载。关闭或后退回到原消息和滚动位置。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）

## 7. 长内容与诊断反馈

- **UI-READING-001 —** Evidence、完整 Initial Analysis 与 Inspection Report 采用同一全工作台阅读层。普通模式从右向左淡入并铺满；正文使用安全 Markdown 单列渲染，代码块/宽表格局部滚动，标题生成可折叠目录，Evidence 引用可继续打开对应路由。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-FEEDBACK-001 —** 每个不可变 Initial Analysis 输出、Inspection Report 版本和具体 assistant message 底部/菜单提供“记录实际结果”：已采纳、已执行、验证有效、不采纳。每次追加可选简短说明，原位显示最新值并可展开完整历史；四种值不被强制为逐级状态机。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-FEEDBACK-002 —** “不采纳”单独确认并说明：相关未确认 Candidate 进入 SourceInvalid，已确认 KnowledgeVersion 永久退出检索且不会自动复活；确认层允许填写说明。其他三种值直接追加，不弹确认。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-FEEDBACK-003 —** 成功 Initial Analysis/Inspection Report 标题区提供次要操作“整理为知识”；Investigation 只在用户明确选择的 assistant message 菜单提供。操作创建或返回同源 Candidate 并打开候选层；已存在不得重复创建，已不采纳来源不得新建。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）

## 8. 巡检工作台

- **UI-INSPECTION-001 —** 第二栏使用紧凑两行项：主行计划名、真实状态、关键时间；次行业务系统、触发方式、报告/gap 徽标。顶部只提供业务系统与状态筛选；`Completed` 不得翻译成“健康”。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-INSPECTION-002 —** 标题区“运行巡检”打开轻量选择层：搜索业务系统，再选择其已发布计划；从系统详情进入则预填。提交后进入新 Run；同计划已有 active Run时打开已有对象，不重复创建。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-INSPECTION-003 —** Run 详情是连续事实页：状态/时间、检查结果、Evidence gap、分析状态、报告版本；使用简短页内 section navigation，不拆成互相隐藏的 tabs。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-INSPECTION-004 —** 检查项默认一行显示检查名、`ok/gap`、采证时间和 Evidence 数；展开后显示原始 PromQL/Journey、参数、真实结果、warnings、gap code 与 Attempt。程序不得生成红黄绿健康结论。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-INSPECTION-005 —** “重新分析现有证据”与“重新采集”是两个动作：前者仅对 `Completed|CompletedWithGaps` 且没有 active analysis 的 Run 可用，不访问外部系统并创建新 Report；后者对 `Completed|CompletedWithGaps|Failed|Cancelled|Interrupted` Run 可用，创建新 Run 与 `evidence_at`。两者不弹确认，但提交前用一句话说明影响。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-INSPECTION-006 —** `AuthenticationRequired` 旁提供“重新登录”。发布新 Browser Profile 后回到原 Run；旧 gap 不修改、不自动补跑，页面明确提示重新采集会创建新 Run。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）

## 9. 知识工作台

- **UI-KNOWLEDGE-001 —** `/knowledge` 第二栏只有“知识 / 待确认 / 导入批次”三段，选择后第三栏显示详情；只有导入批次段提供“导入文本”。不建设知识 Dashboard 或空白知识表单。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-KNOWLEDGE-002 —** 一个自然语言搜索框同时请求 FTS5 与语义检索；结果分为“精确文本匹配”和“语义相似”两组，分别保留 score/status。相同 Knowledge 两组命中时只显示一次并标注两种依据；程序不得加权成统一总排名，也不得要求用户选择索引实现。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-KNOWLEDGE-003 —** Candidate 使用全工作台编辑层，只编辑标题、正文、适用范围；来源诊断/Evidence 只读。确认前保存递增 draft revision；冲突保留用户输入并显示最新 revision，不静默覆盖。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-KNOWLEDGE-004 —** Import Batch 从一份原文启动，立即显示可离开的 Processing；成功后在同一批次逐条修改/排除 Candidate，并提供事务性“确认当前全部”。任一 revision 冲突则全部不提交并定位冲突项。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-KNOWLEDGE-005 —** Knowledge 详情展示 current version、适用范围、来源诊断、反馈、检索资格、相对当前 EmbeddingGeneration 的 index 状态及不可变版本历史。“修订”用 current version 预填新 Candidate，确认后追加版本并切换 current 指针；不得原地覆盖。若本次修订 Candidate 被排除，旧流程保留历史，但同一 current version 仍可重新发起新的修订流程，不能形成永久死路。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-KNOWLEDGE-006 —** “停止复用”必须说明该版本永久退出检索并确认。需要恢复只能从 current Knowledge 发起修订并确认新版本；不得把旧版本开关改回去。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）

## 10. 业务系统与浏览器登录

- **UI-SYSTEM-001 —** 业务系统自己的配置版本、计划、Observed Resource 和 Browser Identity 只在 `/business-systems`；管理模块只保留部署级 Label Contract/Journey Catalog，不提供第二写入口。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-SYSTEM-002 —** 列表主行显示显示名、Enabled/Disabled；次行显示 current config version、资源新鲜度、Browser Identity 状态及待处理徽标。顶部只提供状态筛选与名称搜索。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-SYSTEM-003 —** 系统详情是连续页：当前状态、配置版本、巡检计划、Observed Resources、Browser Identity，并提供简短 section navigation；不拆为五个 tabs。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-SYSTEM-004 —** Admin“上传配置”打开全工作台层，可下载模板、拖放/选择单份 YAML，并展示目标 Label Contract 与 Journey Catalog provenance。失败按 YAML path 给原因和修复方式并保留文件；不得提供竞争性表单或自由编辑器。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-SYSTEM-005 —** 配置历史列出全部不可变版本。版本详情展示相对 current published version 的机械 YAML diff、静态校验、Config Verification Run 与兼容性。“运行测试”直接创建；“发布”确认后原子切换。冲突重新读取 current pointer，不覆盖其他版本。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-SYSTEM-006 —** Label Contract 激活使用全工作台 readiness：列出目标契约、每个 enabled 系统全部合法“配置版本 + Passed Config Verification Run”候选及阻塞原因；多候选由 Admin 选择，不以 latest 代替。全部选择后一次确认并原子激活。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-SYSTEM-007 —** Observed Resource 列表明确“当前观测到 / 当前未观测到 / 数据陈旧”，显示 discovery rule、identity labels、最后成功刷新和最后见到；不得把未观测到写成已删除。详情铺满工作台显示完整 labels、discovery rule、观测时间与当前/陈旧状态。v1 没有资源历史引用数据模型，界面不得制造该列表。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-SYSTEM-008 —** Browser Identity section 显示状态、revision、profile generation、最近 probe 和占用。Admin“配置身份”只填写显示名、起始 URL、认证 probe 与类型化参数并创建新 revision；Operator 只见“重新登录”。不得编辑 Cookie/profile 文件。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-SYSTEM-009 —** `/business-systems/:systemKey/browser-login` 的 noVNC 铺满工作台；顶部窄工具条显示系统、真实 operation 状态、连接恢复提示、发布登录状态和取消。发布成功自动关闭远程桌面并返回来源详情；关闭浏览器页面不等于取消。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-SYSTEM-010 —** 不提供独立 enable/disable 开关。Admin 通过 YAML 根 `enabled` 的新不可变版本发布；确认说明对定时巡检、资源刷新和告警展示的影响。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-SYSTEM-011 —** 系统详情必须提供“立即刷新”（Admin only）入口：提交后立即显示已受理状态（202 的 Run 投影）并以轮询展示进行中/完成结果，等待期间允许离开与返回；刷新结果直接投影为 Observed Resource 当前/陈旧列表（UI-SYSTEM-007），不提供与 `resource_refresh_runs` 竞争的本地状态或伪造进度百分比。Operator 只读可见列表与陈旧标记，不显示刷新入口。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、Issue #41）

## 11. Admin 管理工作区

- **UI-ADMIN-001 —** 第二栏按设置清单、用户、Label Contract/Journey Catalog、连接、模型供应商、告警源、Runtime、备份与 Artifact 保留、安全、审计分组，第三栏显示内容；不增加第四栏或卡片墙首页。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-ADMIN-002 —** 设置清单由权威状态派生，分别展示模型供应商、Thanos/Kubernetes、Plinth/Lintel、Label Contract、Business System、Browser Identity、Stele 告警源和备份目标的就绪、依赖及修复入口；不保存人工完成 checkbox，也不阻塞无关能力。全部就绪折叠为“核心能力已就绪”，故障/缺失时自动展开相关项。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-ADMIN-003 —** 用户列表显示用户名、显示名、角色、启用状态和最后登录；详情编辑显示名/角色/状态，并提供重置密码、撤销全部 Session。禁用、降级、重置和撤销说明现有登录会立即失效；最后一个有效 Admin 冲突显示服务端真实原因。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-ADMIN-004 —** 连接按 Thanos、Kubernetes、Model Provider 类型分组并使用类型化表单。详情分别显示 current revision、credential generation、enabled/unverified、最近真实测试与不可变历史；不得提供任意 URL + JSON 表单。连接表单每次首次提交生成用户不可见的 `client_command_id`；仅原请求网络重试复用，提交后用户修改 `password`/`kubeconfig`/`apiKey` 再提交时必须生成新 ID，避免持久化任何秘密比较 oracle（HTTP-COMMAND-002/003）。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-ADMIN-005 —** Model Provider 表单先经 `discoverProviderModels` 探测上游 `/v1/models`。多个 model ID 由 Admin 选择；零项、失败或 metadata 缺失时保留手工输入，不直接禁止保存。保存后状态为“尚未验证”；真实 probe 成功并显示实测 streaming/tool/embedding 能力后才可 enable。失败保留配置及结构化非秘密错误码/允许字段供重试修正，禁止展示供应商原始响应、header 或 request body，不自动删除或宣称能力。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-ADMIN-006 —** 一次性 Alert Source Bearer/Runtime token 在创建或轮换响应含 reveal handle 时立即打开全工作台层并消费；原文默认可见，提供复制和“关闭后无法再次查看”。前端不得自行缩短服务端固定 60 秒期限；同 Session 命令重放若仍返回原 handle 应继续同一流程，410 时只说明已过期/消费且必须轮换。秘密只存在当前页面内存，不进路由、toast、日志、下载或浏览器持久化。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[CONTEXT「认证与服务身份」](../../../CONTEXT.md#认证与服务身份)、[Issue #16](https://github.com/Suknna/quoin/issues/16)）
- **UI-ADMIN-007 —** Alert Source 详情显示全部 credential 的非秘密 ID、Active/Pending Retirement/Retired、创建、首次使用与退休时间。新 generation 首个成功 Delivery 后旧 generation 进入 Pending Retirement；界面持续提示 Admin 更新完成后显式退休，不按时间或一次成功自动消失，退休前说明立即影响。（来源：[CONTEXT「认证与服务身份」](../../../CONTEXT.md#认证与服务身份)、[Issue #16](https://github.com/Suknna/quoin/issues/16)）
- **UI-ADMIN-008 —** Runtime 只显示 Plinth/Lintel 两个固定 slot；每项分别显示已注册、当前在线、current/pending/retiring generation、最后见到与 active 工作阻塞。新 current 首次认证前显示“等待新 token 首次使用”，成功后旧 generation 持续显示 Pending Retirement 并提供 Admin 显式退休；系统不得按时间或一次成功自动退休。替换流程仍说明吊销/中断影响和等待新 Runtime 注册/连接。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[CONTEXT「认证与服务身份」](../../../CONTEXT.md#认证与服务身份)、[Issue #16](https://github.com/Suknna/quoin/issues/16)）
- **UI-ADMIN-009 —** 备份连续页顶部显示目标挂载、计划时间/IANA 时区/保留份数与最近成功，显式保存。下方显示 Backup Run 的 `queued|running|succeeded|failed`、真实阶段/错误、大小、checksum 和下载；active 记录原位调和，terminal 后不可变；立即备份受理后可离开等待，已有 active Run 时解释冲突而不重复创建。不得提供在线恢复按钮，只展示停机恢复说明与 manifest。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)、Issue #17 Q17.21）
- **UI-ADMIN-010 —** `contracts/openapi.yaml` 的 `MaintenanceState` 为恢复、升级和 root-key rebind 后唯一维护投影。登录后 active=true 时应用以全工作台维护页遮蔽普通业务入口，只展示 reason、逐项 Safe/Blocking、原因、直接修复入口与“退出维护”；Operator 不得绕过，Admin 点击退出后服务端重验全部条件，冲突刷新当前清单，不提供 force/skip。非维护时不得显示这套内部清单。（来源：[CONTEXT「存储、保留与部署」](../../../CONTEXT.md#存储保留与部署)、[Issue #16](https://github.com/Suknna/quoin/issues/16)）
- **UI-ADMIN-011 —** Artifact 保留设置只显示一个“生成型 Artifact 在线保留天数”字段（默认 90）及后果说明；保存携带 expected row version，只影响以后创建的 generated Artifact，必须明确告知既有到期时间不会回写。GC 周期、物理路径和部署卷不得暴露为产品设置。（来源：Issue #17 Q17.1、DATA-BACKUP-009）

## 12. 动效、键盘与可访问性

- **UI-A11Y-001 —** 最低验收为 WCAG 2.2 AA：全流程键盘可操作、无键盘陷阱、焦点可见且不被 sticky 区遮挡、交互目标至少 24×24 CSS px、320 CSS px reflow、状态变化通过 `aria-live` 或等价语义通知辅助技术。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-A11Y-002 —** `Tab` 按自然顺序到达按钮、链接和输入框，`Enter/Space` 激活；Sidebar、Dialog、Tabs、Resizable 只采用 shadcn/Radix/APG 既有键盘模式，不发明 Vim/单字母快捷键。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-A11Y-003 —** Dialog/抽屉打开时焦点不得进入背后页面，关闭回到触发控件。全工作台层打开时焦点进入标题；关闭/后退回到原消息/列表行和滚动位置。含未保存输入时 Escape/返回先走丢弃确认。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-MOTION-001 —** 普通模式全工作台层使用简短、可中断的从右向左淡入/淡出；推荐 180 ms `ease-out`，不得等待动画结束才受理操作。列表重排只在不改变阅读位置时使用。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
- **UI-MOTION-002 —** `prefers-reduced-motion: reduce` 时关闭位移、淡入淡出、列表重排与循环装饰动效并直接显示相同终态；等待仍保留静态阶段图标、文字和真实进度。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）

## 13. 验收矩阵

实现阶段必须用真实浏览器完成以下证据，单纯构建成功不等于通过：

1. **UI-TEST-001 路由恢复**：六模块、筛选、选中对象及每种持久化全工作台层刷新/深链恢复；后退恢复来源滚动与焦点；秘密/草稿不出现在 URL。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
2. **UI-TEST-002 响应式**：至少 320、768、1024、1440 CSS px 覆盖三栏、逐层全屏、noVNC、长 Markdown、代码块与文件列表；无非预期页面级横向滚动。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
3. **UI-TEST-003 键盘/焦点**：只用键盘完成登录、导航、筛选、发送/停止/重试/Undo、打开关闭工作台层、表单错误恢复与高影响确认；自动检查焦点可见/不被遮挡及 24×24 target。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
4. **UI-TEST-004 动效**：普通与 reduced-motion 分别观察进入、退出、列表变化、pending、成功、失败、取消；reduced-motion 必须保持等价静态反馈。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
5. **UI-TEST-005 网络状态**：用可控时钟/网络故障证明 1 s、2 s 重试、同 command ID、10 s 重读、失败回退及刷新/后退取消旧 timer；SSE replay/resync 正常路径不暴露技术术语。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
6. **UI-TEST-006 权限/认证**：Operator/Admin 导航差异、受限 URL 403、Session 撤销遮蔽、同/异 principal 草稿恢复、临时密码两阶段和一次性秘密内存边界。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
7. **UI-TEST-007 等待与终态**：对 Initial Analysis、Attempt、Inspection、Config Verification Run、Connection Probe、Browser Login、Backup 覆盖各自领域契约允许的排队、运行、成功、失败、离开后返回与部分完成；只对 OpenAPI 提供 cancellation fence 的对象覆盖取消中/Cancelled（Connection Probe 包含，Backup Run 不包含取消）；不得为不支持的动作制造按钮，也不得伪造百分比或健康结论。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
8. **UI-TEST-008 列表实时性**：cursor 加载更多、新内容缓冲、顶部合并、row-version 原位刷新、对象删除/失效保留阅读与返回入口。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
9. **UI-TEST-009 调查与附件**：首条消息原子创建、附件-only、多附件 >3 折叠、10 MiB aggregate、16 KiB/200 行 paste、Stop/Retry/Undo、Artifact 复用及对话滚动保护。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
10. **UI-TEST-010 领域工作台**：告警接入问题、诊断反馈/不采纳影响、知识 create-or-return/修订、巡检重新分析/重新采集、Browser Identity/noVNC、模型发现/手工回退/真实 probe、凭据轮换、Runtime 替换与备份下载逐路径验证。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）
11. **UI-TEST-011 自动化门**：TypeScript typecheck、lint、unit/component tests、OpenAPI client compatibility、`frontend-state.schema.json` 的 Draft 2020-12 正反 fixtures、axe-core（或等价）和生产构建全部退出 0；视觉/动效/焦点人工观察结果必须另行记录，不得由构建替代。（来源：[CONTEXT「工作台投影」](../../../CONTEXT.md#工作台投影)、[Issue #15](https://github.com/Suknna/quoin/issues/15)）

## 14. 外部依据

- WCAG 2.2 Recommendation: <https://www.w3.org/TR/WCAG22/>
- WAI-ARIA Authoring Practices Guide: <https://www.w3.org/WAI/ARIA/apg/>
- React 19 Actions: <https://react.dev/blog/2024/12/05/react-19>
- shadcn Sidebar: <https://ui.shadcn.com/docs/components/radix/sidebar>
- shadcn Resizable: <https://ui.shadcn.com/docs/components/radix/resizable>
- shadcn Empty: <https://ui.shadcn.com/docs/components/radix/empty>
- assistant-ui Thread / Tool UI: <https://www.assistant-ui.com/docs/ui/thread> · <https://www.assistant-ui.com/docs/tools/tool-ui>
