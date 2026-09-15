# Quoin 统一认证重设计研究（2026-09-14）

## 范围

本轮为用户提出的重设计意见提供静态调查与设计建议，不是实施或安全验收。未修改认证代码、数据库或现有配置；工作区存在其他并行修改，未触碰。外部产品未做部署验证。研究结果不能替代具体依赖的维护状况、许可和集成测试。

## 当前代码核实

- 正常服务关闭 Huma OpenAPI、Docs、Schemas 自动路由：`internal/quoin/app/app.go:539-550`；维护服务同样关闭：`internal/quoin/app/maintenance.go:52-58`。这不是当前公开文档漏洞；没有进行线上 HTTP 验证。
- 当前允许多个管理员：`internal/quoin/auth/admin_commands.go:75` 接受创建 admin/operator；现有最后管理员守卫只保证至少一个有效管理员，不保证唯一。新要求需要修改数据库约束、创建/更新接口、恢复和迁移规则。
- `cmd/quoin/main.go:31-35` 仅暴露 admin create，没有独立 admin reset-password CLI。备份恢复内部的密码重置能力不等于日常管理员找回入口。
- 当前会话由随机 Cookie 令牌和 SQLite 记录共同实现：`internal/quoin/auth/service.go:185-219`。用户 revision 与数据库状态决定是否仍有效。
- 当前领域约定是本地认证、第一版无 MFA、离线创建管理员、不透明会话：`CONTEXT.md:47-56`。此次要求明确重新打开这些决定。

## 主流初始化与管理员恢复

- Grafana 默认 admin/admin，首次登录提示改密：[官方登录文档](https://grafana.com/docs/grafana/latest/setup-grafana/sign-in-to-grafana/)。提供 `grafana cli admin reset-admin-password`；恢复必须指向正确的配置与数据库，否则可能重置错误数据库：[官方 CLI 文档](https://grafana.com/docs/grafana/latest/administration/cli/)。
- GitLab 提供部署侧 rake 密码重置任务：[官方恢复文档](https://docs.gitlab.com/security/reset_user_password/)。
- Keycloak 提供临时 bootstrap 管理身份及恢复机制：[官方说明](https://www.keycloak.org/server/bootstrap-admin-recovery)。不适合原样复制到“只能有一个管理员”的 Quoin。

结论：内置 admin/admin 可以简化首次使用，但公开默认密码不是部署所有权证明。推荐空库自动生成唯一待初始化 admin；远程初始化还需本机受限文件中的一次性安装凭据（不放 URL、普通日志），或仅通过受保护本地安装通道进行。完成初始化永久关闭引导入口。默认密码不得成为普通会话凭据，也不得成为恢复后的密码。

普通忘记密码与二级验证丢失分开恢复：密码重置不自动取消已有第二因子；全部因素丢失通过部署侧恢复命令进入短时、一次性、仅限恢复的流程。撤销旧会话，重绑并验证联系方式后回到登录页。无需先备份恢复或直接编辑 SQL。

## PASETO：可选令牌格式，不是身份平台

[PASETO 规格](https://github.com/paseto-standard/paseto-spec)及[实现索引](https://paseto.io/)定义令牌加密/签名格式，不提供账户、二级验证、初始化、会话撤销或授权管理。

引入 PASETO 与保留 SQLite 会话并不冲突，也不必建立第二套 denylist：令牌携带 session ID，每次认证仍查询原 session 和当前用户状态即可。但对于目前同源单体，随机不透明令牌已经满足要求，PASETO 没有明确必要性。

若决定采用，建议固定 v4.local，仅由 Quoin 签发和消费，Secure/HttpOnly Cookie 承载；令牌只含最少主体/会话标识、用途、签发与到期信息，不含权限权威或联系方式。密钥与其他凭据隔离，明确轮换和备份恢复后失效策略。初始化、待 OTP 挑战和完整会话必须严格区分用途，服务端状态权威不变。不能使用不支持 v4 的库来实现 v4；实际库选型仍需版本、测试向量与维护核验，不能用生态评分代替密码学审查。

## 邮箱、短信二级验证

[NIST SP 800-63B-4](https://pages.nist.gov/800-63-4/sp800-63b.html)：邮箱不得作为该标准下的带外认证通道；PSTN/SMS 属受限方式；人工转录 OTP 不抗钓鱼。这是保证等级与安全边界，不是禁止产品按需求提供邮箱验证码。Quoin 可第一版同时支持邮箱与短信，但不能宣称因此符合 NIST AAL2 或抗钓鱼 MFA。

推荐规则：密码校验只建立短时认证流程，不发工作台会话；二级验证成功后原子消费挑战并签发会话。验证码短时、单次使用、累计尝试次数跨重发保持，按账户/目标/IP/全局限制发送和校验，限制费用。低熵验证码不能只存普通无密钥摘要，应采用有独立秘密的校验摘要等合适保护。投递失败不得绕过验证。变更联系方式撤销旧挑战和会话，并要求重新验证。初始化完成清除流程，回到登录页重新完成两步登录。

## 插件分层与外部能力

三个不同的扩展点：

1. 登录提供方：本地密码、OIDC、未来 GitHub 等。只证明身份，不赋予管理员权限。
2. 二级验证提供方：邮件 OTP、短信 OTP，未来 TOTP/WebAuthn。核心编排状态、限速、挑战消费与会话签发；具体因子实现遵循固定契约。
3. 投递连接：标准 SMTP 或成熟通知网关。发送成功不是验证成功。

[GitHub 官方 OAuth 登录流程](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps)不同于通用 OIDC：需要 OAuth 流程后查询用户身份，不能仅配置一个 OIDC issuer 就完成。可以新增独立 GitHub adapter 而不改认证编排；不能保证所有 OAuth 提供方零代码接入。通过身份代理接入则可由代理承接上游差异，Quoin 只实现标准 OIDC。外部身份按 issuer/provider + 稳定 subject 映射到管理员预建的操作员，禁止仅凭同名邮箱自动合并或提升权限。

### 已核实的成熟方案

- [ZITADEL MFA 官方文档](https://zitadel.com/docs/guides/integrate/login-ui/mfa)明确支持 OTP Email、OTP SMS，并提供自定义登录 UI 使用的 session/challenge API。它可作为完整身份权威，但邮件短信仍需要配置投递提供方；采用它意味着账户、因素、恢复职责的实质委托，不只是引入 Go 库。
- [Novu 集成说明](https://docs.novu.co/platform/integrations/overview)明确抽象邮件与短信投递，例子包括 SendGrid 与 Twilio，可切换底层提供方而不改应用调用。其能力是通知投递，不是 OTP 校验。
- [Novu 自托管说明](https://docs.novu.co/community/self-hosting-novu/overview)涉及额外运行组件，不能描述成零部署成本的插件；托管服务又涉及费用与联系方式/验证码交由第三方处理。尚未验证具体目标地区短信供应商、模板合规、到达率与延迟。

建议：在保留 Quoin 内置 admin、初始化和 shadcn 自定义两步登录的前提下，采用本地认证编排 + 独立因子插件；邮件复用 SMTP，短信通过一个成熟聚合网关协议（Novu 是已核实候选）连接，不维护厂商适配器集合。若要求连因子、上游身份差异也全部不维护，则改用完整身份平台，并接受额外基础设施和本地账号模型调整。不存在既没有外部权威/网关、又无需适配任意厂商的通用魔法接口。

## 前端基座

通过项目的 pnpm shadcn CLI 查询了当前 Radix/new-york-v4 配置及官方注册表。推荐 `@shadcn/login-03` 统一居中认证卡片，二级页面组合 `InputOTP`；组件支持六位输入、粘贴、受控状态和校验反馈：[官方 InputOTP](https://ui.shadcn.com/docs/components/radix/input-otp)。删除模板自助注册和未启用社交按钮，复用现有 Quoin 品牌。未安装组件或修改页面，未做浏览器验收。

## 新模型与迁移边界

固定一个内置 admin，其他均 operator；取消角色编辑与创建管理员的入口。数据库保证至多一个 admin，启动/迁移/恢复保证其存在，禁止删除、降级或禁用。已有多 admin 数据不得自动任选一个：离线迁移必须明确保留的主体，其余保留 ID 和审计引用转为 operator。旧会话/挑战失效，所有未完成新初始化的账户必须进入新流程，不自动重新写入默认密码。

本轮完成研究，不创建 ADR、不迁移数据、不改认证代码、不提交 Git。正式实施需要统一替换旧认证入口、规格、数据库及恢复契约和测试，而不是在原有逐 handler 鉴权上继续叠兼容层。
