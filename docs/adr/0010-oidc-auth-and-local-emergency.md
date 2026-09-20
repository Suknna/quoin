---
status: accepted
---

# OIDC 外部认证与本地应急通道

2026-09-20 经三轮方案拷问定稿：登录入口改为配置驱动的 AuthProvider 注册表——通用 OIDC（授权码 + PKCE）作为日常登录通道，本地密码登录降级为 IdP 故障时的应急维护通道；平台内 OTP 二级验证整体退役，双因子职责移交 IdP。本决定修订 [ADR-0005](0005-unified-authentication-foundation.md) 的三处结论（`admin/admin` 公开默认密码、密码后强制二级验证、外部登录仅作未来方向），保留其唯一内置管理员、服务端权威会话、同源 Cookie 防护与审计自动化。详细流程与约束见[统一认证设计](../authentication-design.md)。

## 取舍

- 登录方式由 `authentication` 配置段声明，代码侧统一为 AuthProvider 注册表（Kind / BeginLogin / CompleteLogin / Logout）；`GET /api/v1/auth/config` 由注册表投影，前端配置驱动渲染，无登录方式分支代码。`local.enabled` 与 `oidc.enabled` 同时为 false 时配置校验拒绝启动。`client_secret` 只经 secretsFile 机制读取，不入 YAML、日志或公开配置端点。
- OIDC 采用授权码 + PKCE S256 + state + nonce，token 交换、ID Token 校验与会话建立全部在后端完成；token 用后即弃，不保存 refresh_token。discovery 懒加载并定时重试，IdP 不可达不阻塞启动与本地登录。
- 外部身份存独立 `identities` 表（UNIQUE(issuer, subject)），users 表不增加来源列，auth_source 由有无 identity 行派生。禁止仅凭 email 关联已有本地账号；email 只进 `user_contacts` 作展示，未验证时如实标记。本地账号与外部身份的显式绑定不在本版范围。
- OIDC 首次登录开放 JIT 建档，角色恒为 operator。角色模型冻结为唯一内置本地 admin + 全员 operator，不引入 admin_groups、role_sync 或任何组映射；内置 admin 永远仅本地登录，充当"最后管理员"锚点（既有触发器保证不可降级、禁用、删除）。
- OTP 子系统（挑战、投递配置三件套、两段认证流程状态机）整体退役；本地登录单步化（密码通过即建会话），首次改密复用 `password_change_required` 受限会话机制。应急通道的安全姿态 = 强密码策略 + Argon2id + 登录限流 + 审计高亮 + 前端全局应急提示条。
- 初始管理员密码改为首次启动随机生成，写入 dataDirectory 下 0600 文件（与 SQLite 同卷，只读根文件系统部署无需新增挂载），日志只报路径与期限、绝不输出密码；24 小时内未完成首次改密则密码作废，经 `quoin admin recover` 重新生成。公开默认 `admin/admin` 兜底废除。
- 登出一律本地会话撤销，不做 RP-initiated logout。

## 替代范围与迁移

本决定反转 ADR-0005 两处原文（公开默认密码、强制二级验证）与 2026-09-19 未实施评估的"无 JIT、二级验证可选化"方向；其余认证基础（Argon2id 密码策略、不透明服务端会话、同源防护、审计自动化、唯一管理员触发器）不变。[统一认证设计](../authentication-design.md) 已按本决定重写；`auth_flows`、`auth_delivery_settings` 表及 OTP 相关审计事件类型随迁移退役，`identities` 表经 canonical rebuild 新增，现有用户全部视为本地来源，现有会话继续有效。CONTEXT.md 认证段落随实现切换同步。
