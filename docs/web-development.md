# Web 开发与端到端测试

## UI 验收

每次前端调整必须遵守 [`docs/frontend/ui-acceptance.md`](./frontend/ui-acceptance.md) 的组件约束，并保留该文件要求的真实浏览器截图和操作证据；自动化测试、类型检查和构建成功均不能替代。仅修改文档而未调整页面时无需执行页面检查，但应在交付说明中注明。

## 本地模拟工作台

默认开发命令不依赖 Docker、后端或任何真实凭据：

```bash
pnpm --dir web dev
```

它在浏览器内启动 MSW。首次启动默认进入已认证的管理员场景；随后会在 `localStorage` 记住最后选择的场景。页面右下角始终显示紧凑的 **“开发预览 · 模拟数据”** 按钮，点击后展开场景和重置控制；若要恢复管理员场景，在下拉框选择“管理员”。可切换管理员、操作员、登录、首次改密、会话过期、服务不可用、维护、空数据、慢响应和并发冲突场景。切换或重置场景会清除模拟状态并重新挂载应用，以便认证状态重新读取。

仅供本地模拟使用的假账号如下，不是部署凭据，也不会访问任何真实系统：

```text
管理员：admin / demo-admin-password
操作员：operator / demo-operator-password
```

MSW 对未声明的业务请求默认报错，避免静默落到真实网络。模拟开发服务器不配置 API 代理；应用业务 WebSocket 会被阻止，而 Vite HMR 保持可用。HTTP、EventSource 和 WebSocket 的 mock 行为由独立测试覆盖。模拟模式不支持远程浏览器人工登录或真实备份创建、下载和恢复；这些能力必须在受控的真实服务环境验证。

## 真实服务开发

```bash
QUOIN_API_ORIGIN=https://quoin.example.test pnpm --dir web dev:real
```

`dev:real` 不启动 mock、fixture、预览面板或 mock service worker。为安全注销当前 origin 下由 Quoin 注册的陈旧 mock worker，它会短暂加载清理模块；不会触碰其他应用的 worker。随后仅使用指定的同源 API Origin 与生产相同的 API/SSE/WebSocket 路径。

## Playwright

```bash
pnpm --dir web test:e2e
```

默认端到端套件只启动本地 Vite mock 工作台，不会启动 Docker，也不允许真实 HTTP、SSE 或 WebSocket。它覆盖工作台主要页面、详情导航、认证场景和本地写入行为。

还可在没有部署的情况下验证 `dev:real` 会清理 Quoin mock worker 并改走真实 API 代理（使用仓库内本地 API stub）：

```bash
pnpm --dir web test:e2e:real-local
```

真实系统验证是显式选择的，且只在受控环境中运行：

```bash
QUOIN_REAL_E2E_BASE_URL=https://quoin.example.test pnpm --dir web test:e2e:real
```

该命令要求已部署、可访问的 Quoin 环境和测试专用账号/数据；它不会自动创建 Docker 或历史测试栈。此前 28 个面向完整部署栈的回归旅程保留在 `web/e2e/real/legacy/`，默认不收集，因为它们依赖已经删除的 `test/e2e` Docker fixture。只有提供兼容 fixture 时才用 `QUOIN_REAL_E2E_LEGACY=1` 明确运行它们；这避免把过期编排伪装成可用的默认测试，同时保留每个业务回归意图。

生产构建不包含 MSW worker、mock fixture、开发预览面板或任何启动 mock 的代码。前端目录迁移与运行结构见 [`docs/frontend/workbench/README.md`](./frontend/workbench/README.md)，部署边界见 [`docs/deployment.md`](./deployment.md)。本地 mock 和 local-stub 测试不构成生产认证、真实 Runtime、备份恢复或外部浏览器流程的验收；这些仍需要受控真实环境的显式 `test:e2e:real` 验证。
