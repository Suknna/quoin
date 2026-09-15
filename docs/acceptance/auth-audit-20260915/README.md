# 认证、审计与 Kubernetes 直接浏览器验收（2026-09-15）

状态：r2 首次验收、r3 完整复验与 r4 最终浏览器复验均通过，r4 整仓测试复跑、vet 与 auth+operations race 已全部确认。本文不代表对外发布或外部送达结论。

## 环境与边界

- 独立命名空间：`quoin-auth-acceptance`。按 `deploy/kubernetes/quoin.yaml` 与 `ops-services.yaml` 构建和启动 gateway、frontend、quoin、plinth、stele；不部署已退役的 Lintel 或浏览器插件。
- 入口：`https://localhost:18443`，经 gateway Service 的本机端口转发。`publicOrigin` 已在命名空间 ConfigMap 对齐并随后端更新生效。
- k3s 节点采用 Docker 运行时；使用本机 Docker 构建镜像，不执行不存在的 containerd 导入。
- 本次直接浏览器点击、填表验收，没有新增或运行 Playwright E2E 脚本；旧 `quoin-e2e-admin-init` Compose 容器与卷已清理。
- SMTP 测试接收端仅在此命名空间临时部署。实际 TLS 网络投递验证成功，不等于外部邮箱或真实手机送达；未使用收费网关。
- 集群服务 DNS 在当前宿主网络中被解析为代理假地址；只在此隔离命名空间覆盖实际 Service 地址和测试 SMTP hostAlias，未修改全局 DNS。默认产品清单仍使用服务名称。
- 既有其他命名空间和数据卷未修改。测试密码、验证码、Cookie、根密钥与 Runtime token 不写入本记录。

## 直接浏览器证据

| 行为 | 实际结果 |
| --- | --- |
| 管理员统一初始化 | 默认凭据进入改密、投递配置和联系方式验证；真实 SMTP 验证码通过；完成后回到登录页，不创建工作台会话。 |
| 管理员正式登录 | 新密码后停在 OTP 页；新的验证码通过后进入工作台。 |
| 创建操作员 | 管理员设置临时密码与收码邮箱，创建后显示未初始化；没有角色提升或第二管理员入口。 |
| 操作员初始化 | 临时密码只进入改密；验证管理员指定邮箱；刷新已验证流程仍恢复完成页；完成后返回登录页。 |
| 操作员正式登录 | 正式密码加新的 OTP 后进入工作台；没有系统管理入口。 |
| 操作员权限 | 直接访问 `/admin/audit` 显示无权访问；个人收码渠道明确只读、需联系管理员变更。 |
| 账户与会话管理 | 管理员停用、重新启用测试操作员均成功；撤销管理员自身全部会话后，当前工作台立即返回登录页。 |
| 审计关联 | 创建操作员的 `createUser` 访问和 `user.create` 执行事件具有相同关联 ID，执行记录显示对象及持久命令 ID。 |
| 保留期 | 默认六个自然月；输入五个月时预览与确认均禁用；七个月预览影响为零条后保存成功。 |
| Plinth 注册 | 管理页显示完整 `{slot,generation,token}` JSON，经 `/plinth register --config /etc/quoin/component.yaml` 标准输入消费；状态文件权限 `0600`，页面显示 registered / 已连接。 |

脱敏截图：[操作关联](audit-correlation.png)、[操作员审计拒绝](operator-audit-denied.png)、[390px 登录页面](mobile-login.png)。桌面与窄屏登录均未提供 Web 管理员恢复或安装凭据入口。

## 保留数据的升级演练

1. 管理员通过“关于平台 → 准备升级”进入 Upgrade 维护。
2. 自动备份完成后，维护项目为 `BackupPreflight / Safe / backup_verified`。
3. 停止本命名空间 quoin、plinth、stele，使用相同卷与 Secret 的一次性 Job 执行 `quoin migrate preflight`；认证的 schema 与备份检查通过。
4. 执行精确的未发布中间 schema 简化迁移：`7bf6090749699d897896221c44e973f57f6be8eb4b23466b11705cf440f29244` → `98ea729cbfb0f88ca5c89c4245eb48cdf7b48c02f60f0ca3df66e2bcfff63abd`。这是已捕获的开发构建，不冒充已发布前置版本。
5. 迁移台账新增 `20260915_auth_simplification_v1`，不重置用户、不更换密码、不撤销正式会话、不删除历史审计。仅移除废弃安装凭据表和不可恢复的旧 Web recovery 流程及其挑战。
6. 重启后确认原管理员会话继续有效、原操作员仍存在、迁移前创建用户事件可查询、Plinth 使用原长期凭据自动重连。
7. 完成的迁移 Job 和本次临时数据库检查 Pod 已删除，产品 PVC 保留。

升级前备份 ID：`1`。备份 manifest SHA-256：`6a963be4bb6d146667aa50978d6ff1ce37d203eab2afe72b1994555c32000ed7`。保留策略调为七个月发生在此备份之后。

## 本轮发现并修复

- 认证完成条件必须使用本流程持久的 `factorVerified`，不能把联系方式过去验证过等同于本流程验证成功。
- 初始化刷新后仍可返回投递配置；正式 login 流程始终进入 OTP，不回到改密页。
- 初始化 flow Cookie 与陈旧 session Cookie 并存时，读、写投递配置都以同一 flow 身份校验；TLS HTTP 测试覆盖 PUT 后 GET 和错误身份拒绝。
- CLI 恢复没有未实现的密码到期声明；全部因素恢复禁用旧联系方式而不删除外键历史。
- Runtime 注册页面原来只显示裸 token，无法直接用于 CLI；已改为完整一次性 JSON，仍只存页面内存并可立即清除。
- 空闲 observation/embedding 扫描曾每秒产生成功审计。已在读阶段筛选实际待处理工作，并在事务内再次裁决；空闲扫描和竞争后无变化不产生业务事件，真实变更仍自动审计。

## 核心审查后的安全回归

只读代码审查发现并通过先失败后修复的 Go 回归验证两处问题：

- 外部用户的数值 `contactId` 曾可触发向非本人目标发送验证码。现已在持久挑战和网络投递之前检查联系方式属于当前流程用户且已启用；测试证明没有收件地址回显、没有额外发送、没有消耗持久发送预算。
- `LevelFlowOrAdmin` 曾优先采用旧正式会话而与处理器的 flow 优先规则不一致。现统一选择所提供的 flow，拒绝错误/过期 flow 回退到管理员会话，同时保留当前流程的主体和关联；覆盖有效操作员、无关管理员、会话读取故障、错误流程类型等组合。

上述修复的 auth 和 operations 全包测试已通过；后端镜像已从 `auth-audit-20260915-r2` 经 r3 重建复验，最终审查发现的 ObjectType+ID 对象定位权威缺陷由 r4 修正并复验（见下节），r2 截图不作为修复后的验收证据。

## r3/r4 重新部署与最终浏览器复验（2026-09-16）

- r3：`deploy/images/build.sh` 四个默认镜像（frontend、quoin、plinth、stele）滚动更新至 `quoin-auth-acceptance`，全部工作负载 Ready，原有 3 个 PVC 标识未变。复验通过：管理员与操作员正式密码加新 OTP 进入工作台；无管理导航且直访管理页被拒；收码渠道只读；Plinth 第 1 代 registered/connected；七个月保留保持、五个月提交禁用。截图：[操作员渠道只读且无管理入口](final-operator-contacts-readonly.png)。
- 最终审查发现审计 ObjectType+ID 对象定位权威缺陷（r3 部署中部分登录事件的对象类型与编号配错，操作者身份未改变）。修正仅涉及后端且无协议变化：r4 后端镜像滚动更新（quoin pod 镜像 `sha256:dba92c33f5806ecd933e77558882fd63c7a9e80a1b37cf43b9aa6ce5ec47ff47`，镜像清单 `sha256:bd379788a227597aeca0d660b13b1a42fca6e917c89b24aa23482a1bebc41c03`），其余镜像保持 r3；6 个工作负载全部 Ready，3 个 PVC 标识未变。
- r4 浏览器复验通过：管理员正式密码停在 OTP 页（不进工作台），显式发送经本地 TLS SMTP 后以新验证码进入工作台；管理员与操作员保持启用且已初始化，七个月审计保留不变。新登录关联 `dab57ad0a0aa18b94763f9042ceca8d8` 共 6 个事件，其中开始登录指向用户 1，挑战签发与登录完成指向流程 10，投递结果指向挑战 9；另有发送与校验的两条访问事实，其未定位单行的对象 ID 为 0。权威执行事件的对象类型与真实编号配对正确。旧的追加式审计记录未改写，因此 r4 之前开发验收产生的错误对象定位仍保留为历史。截图：[r4 登录关联](final-r4-login-correlation.png)。
- r4 关于页显示 `auth-audit-20260915-r4`，Plinth registered/connected（r3 第 1 代），维护态未维护且无安全检查项，运行时面板正常。截图：[r4 关于与运行时](final-r4-about-runtime.png)。
- Docker 构建上下文为显式允许清单，排除 `.artifacts`、docs 与仓库本地状态；旧 e2e Compose 容器已不在本机容器列表，无关的既有验收容器不属本次清理范围。

## 边界（未执行，非缺陷）

- 外部邮件/短信网关送达未授权、未验收；本地 TLS SMTP 夹具不是产品依赖，也不构成外部送达证明。
- 正式生产恢复/回滚演练与双架构发布资格不在本轮隔离验收范围，未执行也未宣称；既有隔离 Kubernetes 迁移证据继续有效。
- r4 冻结源整仓 `go test`（75 包全过、exit 0）、`go vet`、`git diff --check` 及 auth+operations race（auth 115.923s、operations 1.138s，exit 0）均已确认通过。此前重新生成 API 类型未产生差异，SQL 镜像与 proto 契约检查通过。
- 新增非部署 CI 工作流及 Makefile 检查入口已接线，本地对应检查通过；未提交或推送，因此没有远程 CI 运行结论。

完整命令、通过数量、镜像与截图摘要见[最终验收清单](final-verification.yaml)。清单只保存非秘密结果；其中 `/tmp` 日志路径是本次会话的本机证据位置，不保证跨机器或长期保留。
