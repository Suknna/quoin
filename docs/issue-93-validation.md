# Issue #93 实施验证记录

本记录保留此前架构变更的历史验证事实。Issue #93 随后改定为由运维管理部署生命周期；已删除应用内 Kubernetes/Compose 生命周期编排及其专用验收。它们不再是待完成项，也不应被重写为新架构已通过的证据。

## 已验证

- 完整 Go 测试使用 `sudo env PATH="$PATH" go test ./... -count=1` 通过；普通用户运行的升级网络隔离测试因 `unshare` 权限不足失败，同一测试在所需权限下通过。环境门控的发布矩阵测试并不因默认 Go 测试通过而被视为已执行。
- 契约生成一致性、完整契约校验和 `go vet ./...` 通过。
- 前端 typecheck、lint、22 个测试文件中的 83 项测试和独立生产构建通过。
- 修改后的 GitHub Actions 工作流经 actionlint 校验。
- 独立前端镜像在非 root、drop ALL、no-new-privileges 条件下实际启动；页面、深层路由、缓存策略和 API 不回落 SPA 的行为通过 HTTP 验证。
- 真实 Compose helper 安装完成秘密与管理员引导、六服务启动，以及健康、指标、日志、卷与拓扑验证；临时 Compose 项目已清理。
- 本机 Kubernetes v1.36.4+k3s1、linux/amd64 的独立 namespace 中，入口 Caddy、前端、Quoin、Plinth、Lintel、Stele 全部 Ready。使用真实管理员 prepare/reveal API 和容器 stdin 完成两个 Runtime 注册。
- 同一 Proto 契约下，Quoin `v0.1.0-issue93`、Plinth `v0.2.0-issue93`、Lintel `v0.3.0-issue93`、Stele `v0.4.0-issue93` 实际通信成功。测试镜像仅用于本次验证，不是已发布产品版本。
- 主会话浏览器通过真实 HTTPS 网关完成登录，并验证管理深层路由刷新后仍保持会话与页面加载。
- 代码规范与规格一致性双轴审查发现的发布包未固定镜像、构建选择遗漏、引导 Secret 覆盖、前端安全上下文和临时 RBAC 清理问题已修复并复查。

## 范围变更后的限制

- Kubernetes 专用 Lintel recovery、restore-isolation、`NOT_RUN` 占位及其专用验收已随应用内生命周期编排移出范围并删除；这不是本记录中“通过”的新事实。
- 未运行完整 Kubernetes 多版本及 amd64/arm64 发布矩阵、完整故障注入、离线导入与外部签名发布闭包。本机单节点冒烟不能替代这些发布资格证据。
- 本次未向远端推送代码、发布镜像或创建产品 Release。
