# Quoin 统一认证设计

- 日期：2026-09-20。本文替代 2026-09-14 版本（密码后强制二级验证与投递体系已整体退役）。
- 状态：目标设计，依据 [ADR-0010](adr/0010-oidc-auth-and-local-emergency.md)（修订 [ADR-0005](adr/0005-unified-authentication-foundation.md) 三处结论）；实现、迁移与机器契约切换以 spec issue 为准。
- 范围：登录通道、用户生命周期、初始管理员、会话与接口准入。审计自动化见 [ADR-0006](adr/0006-automatic-audit-and-operation-correlation.md) 及[审计设计](audit-design.md)。
- 本文解释认证边界与取舍；HTTP、SQL 和部署配置的精确字段以同步后的机器契约及部署手册为准。

## 1. 身份与用户生命周期

一个部署固定一个内置 `admin`，其余用户均为 `operator`；角色冻结，不提供身份升降级、自定义角色、权限组或组映射（无 admin_groups / role_sync）。管理员具有全部业务及管理能力，不能删除、禁用或降级，充当"最后管理员"锚点，且永远仅本地登录，不参与任何外部身份同步。数据库触发器与 partial unique index 继续保证恰好一个管理员。

用户来源由 `identities` 表表达：外部身份行 `(issuer, subject)` 唯一，users 表不设来源列，auth_source 由有无 identity 行派生（local / oidc）。**禁止仅凭 email 关联已有本地账号**；OIDC 的 email 只写入 `user_contacts` 作展示，`email_verified=false` 时如实标记未验证，永不参与身份匹配。本地账号与外部身份的显式绑定（设置内操作并复验本地密码）不在本版范围，identities 表为其与多 IdP 预留建模空间。

本地操作员由管理员创建，只禁用不物理删除，稳定用户 ID 和登录名不变。外部操作员由 OIDC 首次登录 JIT 建档（同一事务创建 users 行与 identities 行），无初始化流程、无本地密码、不可本地登录；管理员可禁用，禁用后即使 IdP 认证通过也拒绝登录并拒绝会话续期（现有 enabled 检查）。启用不恢复旧会话。

## 2. 系统首次启动与管理员初始化

空库首次启动自动创建唯一待初始化的 admin，初始密码由 crypto/rand 随机生成，写入 dataDirectory 下 `initial-admin-password` 文件（0600，与 SQLite 同卷，只读根文件系统部署无需新增挂载）；启动日志只报文件路径与 24 小时有效期，**绝不输出密码本身**。运维经 `kubectl exec` / `docker compose exec` 读取文件（同构 GitLab initial_root_password 模式）。生成幂等：文件已存在且 users 为空时不重新生成；过期时间戳记于数据库引导记录，登录时校验。

24 小时内未完成首次改密则该随机密码**作废**（严于 GitLab 的仅删文件），登录拒绝并提示走 `quoin admin recover` 重新生成（再给 24 小时）。公开默认 `admin/admin` 兜底废除；已有库、重启、升级和恢复均不能重新生成默认密码。首次部署的访问边界由部署环境负责（受控内网或受限管理网络），沿用 2026-09-15 决定。

管理员初始化流程只剩一步：以初始密码登录 → 建立受限会话（`password_change_required`）→ 设置符合正式策略的新密码 → 会话解除受限、删除密码文件并写审计。不再配置投递、不再登记收码联系方式。

## 3. 操作员生命周期

本地操作员：管理员创建账户并指定临时凭据 → 用户登录建立受限会话 → 强制改密后正常使用。临时密码仅可进入受限会话，正式改密或再次重置后失效。

外部操作员：OIDC 首次登录 JIT 建档即用，无任何初始化步骤。claims 映射：username 取 `preferred_username`（缺失回退 sub 派生），与既有登录名冲突时确定性加后缀自愈（如 `jane1`）；display_name 取 `name`；email 进 `user_contacts` 仅作展示。档案字段（display_name、email）以首登快照为准，不随后续登录覆盖。

管理员重置本地操作员密码会撤销其全部会话；禁用立即生效。外部用户不可删除、不可重置密码（密码由统一平台管理），仅可禁用/启用。

## 4. 登录通道

登录方式由 `authentication` 配置段声明，代码侧统一为 AuthProvider 注册表（`Kind / BeginLogin / CompleteLogin / Logout`）；`GET /api/v1/auth/config` 公开端点由注册表投影，返回 `{local: {enabled, visible}, oidc: {enabled, label, iconUrl}}`，不含任何敏感配置。前端据此配置驱动渲染，无登录方式分支代码。`local.enabled` 与 `oidc.enabled` 同为 false 时配置校验拒绝启动。

```yaml
authentication:
  local: {enabled: true, visible: true}
  oidc:
    enabled: false
    issuer: https://sso.example.com
    client_id: quoin
    redirect_url: https://quoin.example.com/api/v1/auth/oidc/callback
    label: 统一身份登录    # 缺省用 issuer 主机名
    icon_url: ""
  secretsFile: /run/quoin-secrets/auth-secrets.yaml   # 内含 oidc client_secret
```

**本地通道（应急）**：单步 `POST /api/v1/auth/login`，用户名密码校验通过即建立会话，不再有密码后二级验证与两段流程状态机。保留现有 Argon2id 密码策略（15–128 Unicode 字符、NCSC 弱口令黑名单、禁含用户名/产品名）、登录失败统一响应与限流（15 分钟 5 次失败冷却）、不存在用户走 dummy hash 防时序侧信道。`local.visible=false` 时登录页不渲染本地表单，但 `/login?local=1` 与 POST 接口始终可用（`local.enabled=true` 前提下）。每次本地登录成功强制写审计；部署启用 OIDC 时，本地登录会话在前端全程显示全局应急提示条（"您正在使用本地应急账号，仅供 IdP 故障时维护使用"）。

**OIDC 通道（日常）**：授权码 + PKCE S256 + state + nonce，scope 固定 `openid profile email`。BeginLogin 生成 state/nonce 存短时 HttpOnly cookie 并 302 到 IdP（state 携带 return_to 深链）；HandleCallback 在后端完成 code 换 token、ID Token 校验（iss/aud/exp/nonce）与 claims 提取，token 用后即弃，**refresh_token 不落库**。成功后按 (issuer, sub) 查 identities：命中即登录该用户（enabled 检查照常），未命中即 JIT 建档。失败一律 302 到 `/login?error=<码>`，错误码不含秘密。discovery 懒加载并定时重试，IdP 不可达不阻塞启动与本地登录，SSO 点击时返回"暂时不可用"。登出一律本地会话撤销，不做 RP-initiated logout。

审计事件九类沿用 audit_events 体系与 retentionMonths：`login_success` / `login_failure`（含方式 local|oidc、IP、用户名）、`oidc_callback_failure`、`account_created`、`account_disabled` / `account_enabled`、`password_reset`、`bootstrap_admin_created`、`initial_password_expired`。

## 5. 会话权威

继续采用密码学安全随机源生成的不透明令牌，通过 Secure、HttpOnly、SameSite=Lax、Path=/ 的同源 Cookie 持有；服务端保存摘要与会话记录（本地单步登录与 OIDC 回调在同一 sessions 表建会话）。随机源失败拒绝签发，不回退可预测值。无 PASETO、JWT 或可配置令牌格式。

业务只接收当前认证结果，不解析令牌。服务端会话与当前用户状态决定身份是否有效；每个请求实时读取 users 表（enabled、role、auth_revision），禁止永久快照角色作为权限权威。`GET /api/v1/auth/me` 返回 authSource（local / oidc），供前端应急提示条与账户展示。

保留空闲 12 小时、绝对 7 天默认值，按节流更新真实有效活动；SSE 心跳、后台轮询不能无限续期。保留多设备会话和按会话撤销。修改/重置密码、禁用撤销旧会话；`password_change_required` 受限会话除 me / password / logout 外一律 403。敏感写提交前再次验证当前身份。

## 6. 管理员恢复

`quoin admin recover` 保持 CLI-only（停止长期进程、独占数据库、attached TTY），只剩密码模式：设置或打印一次临时密码，撤销旧会话并审计。初始密码过期（24 小时未改密）后由 recover 重新生成随机密码文件并重置期限。恢复后的登录按凭据与账号状态机械分类：未初始化则进入与首次安装一致的改密流程，已初始化则直接可用。不设独立的 recovery 流程类别，不得恢复 admin/admin、创建第二管理员。

## 7. 扩展契约

认证提供方为可信、显式注册并随版本交付的 AuthProvider，不是在线安装任意代码。本版交付 LocalProvider（单步密码）与 OIDCProvider（通用 OIDC 授权码流程）；未来提供方新增实现并重新构建发布，不承诺任意厂商零代码接入。

核心拥有注册表、账户映射（identities）、限流与会话签发。`identities` 表为将来的显式绑定（设置内操作、复验本地密码）与多 IdP 预留；届时绑定仅写入新的 (issuer, subject) 行，users 表与本地密码不受影响，仍然永不按 email 自动合并。

## 8. 接口准入与前端

每个注册入口必须声明准入类别；未声明不能默认公开，注册/契约测试应失败。`GET /api/v1/auth/config` 与 OIDC 两个路由为公开类别；公开、正常用户、管理员分别校验，受限会话凭据不能被普通会话解析器接受。保留现有 Cookie 同源/CSRF、安全响应头、敏感响应 no-store 防护；OIDC 回调为顶层 GET 重定向，同源门按导航请求放行。维护模式两级表面同步注册认证路由。

前端引入 `/login` 真实路由渲染认证页（现有状态机保留）：未登录访问任意路径仍原位渲染认证页；`/login` 为规范入口与 OIDC 回调落点（成功 302 到 `/` 或 state 携带的深链，失败 302 到 `/login?error=<码>`）；已登录访问 `/login` 依 `/api/v1/auth/me` 跳转工作台；唯一可见方式且为 OIDC 时自动跳转 SSO，`?login=1` 强制展示登录页。登录页按 auth/config 渲染本地表单 / SSO 按钮 / 错误提示条，前端不接触 token。

管理端（admin 角色，本地与外部管理员界面一致——本版外部用户恒为 operator，实际仅内置 admin 可见）：用户管理页增加 auth_source 维度（外部用户只读、可禁用、密码项展示"由统一平台管理"）；审计页增加登录方式过滤与本地登录高亮；新增只读认证配置页 `/settings/platform/authentication`（issuer、client_id、secret 脱敏、local/oidc 开关，注明修改方式为配置文件+重启，不提供在线编辑）。

## 9. 迁移与验收边界

迁移经 canonical rebuild 一次切换，不并行保留旧活动登录路径：`identities` 表新增；`auth_flows`、`auth_delivery_settings` 表与 OTP 相关审计事件类型、投递配置端点及配置段 preset 退役；现有用户全部视为本地来源；现有会话继续有效（sessions 表与校验机制不变）；e2e 凭据命名漂移（runner 导出 `ADMIN_DEFAULT_PASSWORD`、spec 读取 `FINAL_PASSWORD`）一并修复。旧认证实现与相关规格、OpenAPI、mock、测试同步切换。

验收至少覆盖：`admin/admin` 不存在且随机密码 24 小时作废、recover 可再生；OTP 任何路径不可达；state/nonce 缺失或不匹配拒绝、重放拒绝；email 永不触发账号关联；JIT 撞名确定性后缀自愈；禁用外部用户 IdP 认证通过仍拒绝且会话即时失效；client_secret 不入 YAML/日志/auth/config 响应；双 enabled=false 拒绝启动；IdP 不可达时本地登录可用；local.visible=false 时 `/login?local=1` 可登；本地登录写审计并显示应急提示条；`password_change_required` 受限会话豁免清单正确；假 IdP 集成测试（discovery/jwks/authorize/token）全绿。
