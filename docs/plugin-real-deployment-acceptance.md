# 插件主线真实部署验收（2026-09-13）

## 当前结论

**默认接入即用主线及业务视图范围链路已完成真实部署点击验证；完整验收保留明确边界，不能宣称所有场景全通过。** browser 可选部署、独立身份与远程 Chromium 启动/取消已验证；真实业务登录、profile 发布及业务 Journey/浏览器工具成功链路因实验缺完整业务 Web 而阻塞。模型报告存在一处已记录的文字计数错误，旧报告版本的 UI 切换能力也未通过本轮验收。下表只记录实际 Dockerfile 镜像、Kubernetes 部署与 Browser Use 点击结果，单元测试、MSW、历史实验结果不替代真实验收。

## 部署与实验边界

- 使用 `deploy/images/build.sh` 与 `deploy/images/*/Dockerfile`，以 `deploy/kubernetes/quoin.yaml`、`deploy/config/*.yaml` 为权威，实验 overlay 位于 `.artifacts/plugin-acceptance-20260913/manifests/`。
- 隔离 namespace：`quoin-plugin-acceptance`；独立 Retain 数据卷，未清空验收数据库、未直接改写 `schema_state`。
- 浏览器 origin：`https://quoin-lab.quoin.internal:30443`，保持 TLS、真实身份认证及运行时注册。
- 本次按失败修复循环实际部署并点击 r2–r9 相关镜像。最终运行 Quoin r9、Plinth r7、frontend r5、Stele r4、Lintel r4；只重建有代码变化的组件，镜像代号不同不表示协议不兼容。管理员会话、连接、数据库、报告与运行时注册在 rollout 中保留。部署配置 Schema 的说明同步已包含在 r4 及之后相关构建中。最终现场保留 browser 显式启用配置；默认关闭阶段的通过证据在启用之前单独完成。
- 源码与镜像构建记录：`.artifacts/plugin-acceptance-20260913/source-provenance.txt`、`build.log`、`build-r2.log`、`README.md`。未提交或推送本次改动。
- 实验实际目录为 `.local-lab/`。mall/MySQL/Redis/exporters 及真实 Prometheus 已恢复，Prometheus Service 为 `10.43.100.205:9090`。缺失的 libvirt VM 对应监控目标仍 down，不能据此宣称整个实验业务健康。
- 操作披露：恢复 mall 时确认 MySQL 使用新空数据目录；执行了现有首次初始化加固 SQL，禁用六个公开种子账户并使用未变更秘密创建 labadmin。此后停止账户与 SQL 配置动作；未重新开放默认账户。Swagger 仅为 API 文档页面，不冒充完整业务 Web Journey。

## 本次真实点击记录

| 场景 | 实际结果 | 状态 |
| --- | --- | --- |
| 正式管理员登录 | 离线正式 bootstrap 后，真实登录页面完成临时密码登录与强制首登改密，进入工作台 | 已通过 |
| 默认插件目录 | 接入管理显示 Alertmanager、Kubernetes、Prometheus、Thanos；无 browser 配置卡片 | 已通过 |
| 默认运行时页面 | r2 暴露 Lintel 注册行；修复后 r3 真实运行时页面仅显示已连接 Plinth，Lintel 行及注册入口消失 | r3 复验通过 |
| Plinth 注册 | UI prepare/reveal，一次性令牌经正式 `plinth register` stdin 消费；UI 显示 registered、generation 1、已连接 | 已通过 |
| Prometheus 创建 | UI 创建 `mall-prometheus`，无认证、未勾选跳过 TLS；没有创建 BusinessSystem 或业务视图 | 已通过创建 |
| 验证失败保护 | 首次验证期间 Plinth 尚未注册，UI 显示失败且连接保持停用；节点连接后服务端记录原 probe attempt 1 passed | 已验证保护，未算启用通过 |
| Prometheus 实例详情 | r2 错用数字 ID；r3 点击同一行管理后进入 `/integrations/prometheus/mall-prometheus`，详情读取成功，点击验证并启用成功 | r3 复验通过 |
| 模型提供方 | UI 创建 `lab-deepseek`，真实网关连接 DeepSeek 与本地 Ollama；probe attempt 2 passed，随后确认启用 | 已通过 |
| 自动来源观测与对象详情 | 启用后自动观测 Run 1 Completed，显示 6 个真实目标；打开 MySQL 对象详情核对来源与 job/instance 身份 | r3 已通过 |
| Agent 插件 Tool/Evidence | `/investigations/1` 不绑定业务系统，Succeeded；工具记录显示 5 条 thanos_query succeeded；打开 Evidence 2 与原始正文核对 up 的 6 个真实样本 | r3 已通过 |
| 巡检、报告、重分析与重采集 | r4 采证后报告受 NULL 谱系重建错误阻塞；r5 修复后列表可读。原分析最终 Interrupted，UI 对同一 Run 1/Evidence 17 重新分析成功生成 v1，再次分析生成 v2；重新采证另建 Run 2/Evidence 21，自动生成独立 v1 | r5 链路通过，模型文本仍需核对 |
| 可选业务视图 | r7 Run 3/Evidence 30 仅 1 条 MySQL；r8 视图改为 Redis、行版本 2，旧 Run 3 仍保持 MySQL。Run 4 摘要不一致被拒后由 UI 正式取消；r9 从原 Run 3 新建 Run 5，Evidence 37 与报告 v1 仍仅 MySQL，沿用原冻结范围 | 范围、编辑、旧历史及冻结范围重采证通过 |
| Alertmanager 告警与恢复 | UI 创建 mall-alertmanager，真实 Prometheus→Alertmanager→Stele 投递 QuoinAcceptanceCanary；UI 未归属 Firing，AI 分析完成并关联 Evidence 8–13。移除本次 Canary/CanaryProbe2 规则后，r4 UI 当前告警 0、两条告警历史均 Resolved | 已通过 |
| browser opt-in | 正式可选 overlay 启用后 Lintel 注册连接成功、插件卡片与认证探测目录出现；UI 创建独立身份 mall-web-acceptance，启动真实 noVNC/Chromium 并显示实验 Swagger 页面，随后取消、未发布 profile。完整业务 Web 缺失，真实登录后发布及业务 Journey/浏览器工具成功链路未执行 | 部署/身份/远程启动通过，业务登录场景阻塞 |

### 已验证模型能力

模型配置为 `deepseek-chat`，Embedding 为 `all-minilm:22m`，上下文预算 64000，输出预算 8192。2026-09-13T10:57:00Z 探测结果包括：native tool calling、multi-tool calls、streaming、usage、request ID、cancellation 均支持，Embedding 向量维度 384。该结果来自实际界面的探测历史，不是仅验证网关返回 HTTP 400。

专用网关覆盖对话请求的上游 Authorization，移除 Embedding 请求的 Authorization；UI 必填 API Key 使用非秘密客户端占位值，未读取或公开真实上游 API Key。

### 报告质量与恢复边界

- Run 1 的首次分析在旧代码错误与部署切换后变为 `Interrupted`，并非自动生成成功；经正式 UI“重新分析现有证据”恢复，未修改数据库状态、未重新采集或伪造旧报告。
- Run 1 的 v1/v2 均引用原 Evidence 17；Run 2 的 v1 引用新 Evidence 21。r5 UI 默认展示当前报告版本，不能在该页切换旧版本。只读存储核对确认 report 1/2 分别为 Run 1 v1/v2，冻结证据摘要相同（`fe3b7b3db0778380f48e04b7f215ea1a…`）；report 3 为 Run 2 v1，摘要不同（`3db4e2d15128484c18e33cde50510caf…`）。此项辅助存储证据不冒充 UI 历史版本切换验收。
- 显式 `objects` 范围已通过冻结标签与真实 PromQL httptest（含聚合/子查询、矛盾匹配器、缺条件拒绝）验证，本轮未另建显式对象计划做独立浏览器点击；业务视图范围则完成真实部署点击与原始证据核对。Kubernetes 新集群接入、全量发现与主机接入不属于本次新增交付。
- Run 2 v1 原始表格列出 4 个 PaaS 目标，但模型文字汇总误写成 3 个。这是实际模型输出的计数错误，不是采证结果；验收不把它隐藏或将所有生成内容视为正确。原始 Evidence、时间戳和表格可供人工核对。

### 失败与修复要求

- 运行时注册令牌 TTL 为 prepare 后 60 秒，reveal 不延长 TTL。最初超时未注册成功；改为受限文件更新后立即 attached-stdin 消费，未放宽 TTL 或认证。
- 默认 Lintel 行、Prometheus 数字 ID 导航、重复巡检弹窗、NULL 谱系重建、单连接嵌套查询死锁、计划审计 INSERT、视图 PUT Schema、范围未施加到 PromQL、重采证遗漏 scopeKind 均在本次实现/真实验收中发现，修复后重建对应镜像复验。
- 旧 Run 4 缺 scopeKind 的 canonical 不可通过修改摘要变为有效；UI 取消收口保留失败历史，再从原 Run 3 创建正确的新 Run 5。
- Gateway 曾因 overlay TLS 层级错误无法启动，Stele 曾因 service token 字节长度错误无法启动，均按实际日志修正；未放宽 TLS、认证或秘密长度检查。

### 脱敏浏览器截图

- [r4 告警历史：两条实验告警均已恢复、当前告警为 0](acceptance/plugin-20260913/alerts-resolved-r4.png)。同时保留 rollout 期间 Plinth 断连后已收敛的历史平台故障，不删除或掩盖这些记录。
- [r5 原 Run 1：Evidence 17、报告 v2，与新 Run 2 同时保留](acceptance/plugin-20260913/report-v2-r5.png)。
- [browser opt-in：真实 Lintel Chromium 经 noVNC 显示实验 Swagger](acceptance/plugin-20260913/browser-novnc-started.png)。此图仅证明远程浏览器启动/传输，不证明业务登录或 Journey 成功。

## 自动化检查（不是浏览器通过替代品）

最终冻结源码已完成 `go test ./... -count=1 -timeout=90s`、`go vet ./...`、前端 `typecheck`、`lint`、`test`、`build`，组合命令 exit 0；`git diff --check` 通过。增加了嵌入契约与权威文件逐字节同步测试、真实 Huma HTTP CRUD 回归、实际 DispatchInputFor 摘要往返、PromQL 上游 httptest 范围约束及 fail-closed 测试。scope 采集器定向 `-race` 测试通过。日志为 `/tmp/quoin-plugin-final-go.log`、`/tmp/quoin-plugin-final-vet.log`、`/tmp/quoin-plugin-final-web.log`；这些检查不替代上表真实浏览器证据。

## 最终运行镜像身份

2026-09-13 21:11 的只读现场核对（所有应用 pod Ready；具体构建命令与源码工作树指纹见实验 provenance）：

| 组件 | 镜像标签 | ImageID SHA-256 |
| --- | --- | --- |
| Quoin | plugin-20260913-r9 | `3ce772e99c88a3cdb55eae7ae9427b27e009e7c3a8d77c7f025adbb98e8bb904` |
| Plinth | plugin-20260913-r7 | `a47328cfb426c9fb8586cb4a76f83ac86871bb6d067e1f096b03db93977fb291` |
| frontend | plugin-20260913-r5 | `c4bea288d8957577e090874effad59214fdfc50b97cc15497ca6c3c3080678c0` |
| Stele | plugin-20260913-r4 | `35ce5a932d5c38508159df1e04bdb310a19504baad085d9554ea06feb4c76a17` |
| Lintel | plugin-20260913-r4 | `3a8fc7afde09a51aa4182734912ba957e1cc3d2fd498e213c695f296adf47c72` |

SQL 权威与嵌入副本 SHA-256 均为 `d67fc107b5f8a978eae6b609393f6ec5563978234dc5167da7c1d021315b6269`；运行中 Quoin 启动 schema gate 通过。未修改数据库 schema_state 绕过校验。最终前端测试为 **31 文件、213 用例通过**。

## 秘密与证据处理

凭据只保存在隔离验收目录的受限文件和正式应用秘密存储中，不写入本记录、截图或 URL。一次性令牌显示后使用 UI 清除。后续截图仅在无密码、令牌、cookie 或秘密输入时采集。
