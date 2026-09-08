# 第一条真实业务切片：认证与 Thanos

用户已确认官方模板。本入口使用同一 new-york-v4 UI 与主题，接真实 API；模板对照仍保留在 5174，不混入旧 Workbench CSS，不模拟服务端成功。

## 启动与构建

仓库根目录：

```sh
NODE_EXTRA_CA_CERTS="$PWD/.local-lab/config/kubernetes/tls/quoin-lab-ca.crt" pnpm --dir web dev:workbench
pnpm --dir web build:workbench
pnpm --dir web test:workbench
```

本机预览 http://127.0.0.1:5175/ 。Vite 仅监听 loopback，API 使用相对路径，代理到实际 `https://quoin-lab.quoin.internal`，保持上游 TLS 校验；没有跨域 API origin 开关。正式验收仍须在可信同源 HTTPS 下验证 Secure session cookie，不能将本机 HTTP 预览当作已完成认证部署验收。

独立 root 为 `web/workbench`，构建输出 `.artifacts/new-workbench/dist`；不覆盖旧 `internal/gen/web/dist`，不发布镜像。

## 当前环境阻塞

本轮只读检查发现 quoin-lab 的 Quoin/Plinth/Lintel/Stele 工作负载副本为 0、Service 无 endpoints，公共 `/livez` 返回 503。没有启动或恢复这些资源。历史 local-prometheus 已启用及 Runtime 已连接记录不是当前状态证据。

因此单元测试 fixtures 只能证明前端分支与 API 调用契约，不能证明真实登录、Thanos探测、启用通过。不得使用真实凭据反复尝试当前不可用后端。

## 来源与边界

- 完整官方 sidebar-09、login-02 及基础主题来源快照、许可、hash见 `../templates/provenance.yaml` 与 `../templates/sources/`。
- 登录模板改为用户名密码，删除 GitHub/注册/找回；原双栏结构和官方 placeholder 保留。
- 监控连接用官方表单组件，不安装 shadcn.io 或 AWS 结构。业务表单不建立模型/provider等无关写入口。
- Session失效、强制改密、维护态、冲突、等待和外部失败必须呈现真实结果；不能把不存在的服务显示为空列表或已健康。
- 新 API 层为相对路径，使用生成的认证类型，按现有后端连接契约发送命令及并发 fence。
- 模型提供方与 Thanos 复用同一个 Connections API 的联合类型；侧栏按类型分组，旧 `#connection/<name>` 深链仍按服务返回的类型打开对应详情。其他连接类型不显示为模型提供方。
- 模型提供方创建使用官方 Field、Input、Command 与 Popover 组件（`shadcn@4.21.0` 安装记录在 `templates/`），发现请求为 `POST /api/v1/model-providers/discover`。`available: false` 是明确的手工填写路径，不冒充 HTTP 失败；Base URL 或 API Key 变更会废弃旧发现结果，API Key 在会话暂停时立即清空且从不回显或缓存。
- 模型提供方启用仅提交与当前 revision、credential generation 精确闭合的最新 passed probe result；Thanos 继续不发送资格字段。探测详情直接呈现后端 `details` 与 outcome，不虚构能力分数、进度或动作通过数；新建时按现有后端契约必填 embedding 模型；未验证不能显示通过。
- 当前 `.local-lab/RECORDS.md` 明确没有获批的 provider base URL、API Key 或模型 ID。因此未调用任何外部 provider，未创建虚构 provider，也没有以 mock 代替真实预览。

本轮未提交代码、未变更实验部署。后端恢复后的实际浏览器主链和可信HTTPS会话验证仍是阻塞验收项。

## 本轮验证结果与保留项

- 新入口生产构建、独立类型检查、全工程 ESLint 通过。
- 新增 14 个组件/API 测试通过：认证503重试、首次改密分流、operator只读、Session失效遮蔽、秘密清理、同/异主体草稿恢复、退出401/503，以及Thanos启用请求体、probe权威GET等。
- 原有前端16文件99测试及类型检查通过。
- 真实浏览器仅验证了当前后端不可用提示和点击重试的等待→失败过程；没有注入mock登录或绕过页面。
- 独立审阅指出并已修复Thanos资格字段误用、probe假时间/版本、登出401处理、维护刷新重叠、秘密恢复问题。完整轮询乱序/维护切换自动化覆盖及真实后端联合复验仍未完成，不宣称独立最终验收通过。
- 当前不能验收真实登录、创建/探测/启用、可信HTTPS Cookie或登录后工作台全尺寸浏览器行为。需先明确恢复停用实验资源，保留PVC、Secret、现有配置和凭据，不自动重建。

模型阶段验证：21个新入口测试、构建、全工程lint通过；浏览器通过正常入口打开模型表单和官方Command/Popover选择器，未发出真实模型发现或创建请求。现有后端契约未修改。缺少获准base URL、API key、对话及embedding模型ID，真实探测/启用仍阻塞。独立审阅发现的切换连接时旧详情可操作问题已以详情加载状态和旧请求取消修复。
