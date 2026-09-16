# mall-shop 手册与构建验证记录（2026-09-16）

## 范围

本次任务准备 mall-shop 业务环境和上游监控，并补齐 Quoin 初次使用文档。Quoin 的正式安装、管理员初始化、模型供应商配置、来源接入与浏览器端巡检由用户按手册进行，本记录不将这些未执行的步骤宣称为已验收。

业务与告警实测结果见 [mall-shop 演练手册](../mall-shop-lab.md)。

## 已执行检查

| 检查 | 结果 |
| --- | --- |
| `go test ./... -count=1` | 首轮失败；完整复跑通过，详见下文 |
| `go vet ./...` | 通过 |
| `make web-typecheck web-lint web-test web-build` | 通过；37 个测试文件、276 个测试通过，生产前端构建成功 |
| `docker compose -f deploy/compose.yaml config --quiet` | 通过 |
| `kubectl apply --dry-run=client -f deploy/kubernetes/quoin.yaml` | 通过；未部署 Quoin |
| `QUOIN_IMAGE_TAG=mall-handbook-20260916 bash deploy/images/build.sh` | frontend、quoin、plinth、stele 四镜像全部构建成功；未启动 Quoin |

首轮 Go 测试唯一失败位于 `internal/plinth/worker` 的 `TestSandboxSubprocess`，输出 `Establish: landlock: operation not permitted`。没有修改源码或关闭沙箱；随后单项 `go test ./internal/plinth/worker -run '^TestSandboxSubprocess$' -count=1` 通过，再次完整运行 `go test ./... -count=1` 通过。首次失败原因尚未证实，不能将其描述为已修复缺陷。

本机 Go 为 1.27.1，依赖 sonic 提示超出其优化支持范围并回退到 `encoding/json`；镜像仍使用仓库固定工具链，不需要为此修改项目依赖。

## 文档与业务闭环补充验证

- 新手手册的 Bash 代码块通过 `bash -n`；独立 Compose 项目 `quoin-mall-user` 经 `docker compose config --format json` 验证，卷名和单一 8443 发布与手册一致。
- 本次新增/修改手册的相对文件链接全部存在；`git diff --check` 通过。
- `scripts/mall-shop/` Shell/Python 语法通过；Prometheus 官方 `promtool check rules` 校验通过，共 6 条规则。
- 商城 Java 与 Nginx 转发的 `/actuator/health` 均 HTTP 200 且 `status=UP`；真实登录后 `/admin/info` 与 `/admin/list` 读取成功，列表 7 条。
- `shellcheck scripts/mall-shop/*.sh` 通过；监控重复部署脚本实际执行通过，重启 Prometheus/Alertmanager 后自动刷新 Pod IP 防火墙例外，5/5 业务目标和 6/6 规则恢复。日志 `/tmp/quoin-mall-redeploy.log`。
- Prometheus 六个目标均带 `system_id=mall-shop` 且 `up=1`；`mysql_up`、`redis_up`、`nginx_up` 均为 1。
- 从独立 Docker 容器验证 Prometheus NodePort `192.168.1.200:30090` 及监控 ClusterIP 可达。
- 真正暂停 Redis exporter 后，接收器于 11:08:43 +08:00 收到 firing，恢复后于 11:09:43 收到 resolved；六条规则最终均为 inactive/ok。验证链路终点为本地 HTTP 接收器，不是未部署的 Quoin。

## 本地验证产物

构建使用独立标签，不覆盖用户已有 `v0.1.0-dev` 镜像：

| 镜像 | 本机镜像 ID |
| --- | --- |
| `quoin/frontend:mall-handbook-20260916` | `3f865ee22174` |
| `quoin/quoin:mall-handbook-20260916` | `6e530b1523f8` |
| `quoin/plinth:mall-handbook-20260916` | `62ed65cad4db` |
| `quoin/stele:mall-handbook-20260916` | `73f71521f541` |

本机临时日志：`/tmp/quoin-mall-go-test.log`、`/tmp/quoin-mall-go-test-retry.log`、`/tmp/quoin-mall-go-vet.log`、`/tmp/quoin-mall-web-checks.log`、`/tmp/quoin-mall-images.log`、`/tmp/quoin-mall-k8s-dry-run.log`。这些不是长期归档，也不应包含实际部署凭据。

## 未执行及边界

- 未提交或推送 Git 变更，未修改远端 GitHub About；仓库简介文案保存在 [repository-description.md](../repository-description.md)。
- 未替用户创建 Quoin 管理员、注册 Plinth、配置模型密钥或接入 mall-shop 告警源。
- 没有进行生产可用性、备份恢复或跨机器网络验收；本机实验结论不等同于生产认证。
- 没有调用历史端到端脚本自动完成用户本次要亲自体验的初始化流程。
