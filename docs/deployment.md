# 部署

Quoin 交付六个独立服务：入口 `gateway`（stock Caddy）、`frontend`、`quoin`、`plinth`、`lintel` 与 `stele`。浏览器、API、SSE 与 noVNC WebSocket 共用一个 HTTPS Origin；告警发送方使用同一 Origin 的 `/stele/` 路径。Caddy 优先转发 `/api/*` 到 Quoin、`/stele/*` 到 Stele，其他请求到前端，因此 SPA fallback 不会吞掉 API、SSE 或 WebSocket。

Kubernetes Deployment controller 与 Docker Compose 管理部署生命周期。Quoin 不包含 `quoin-deploy`，不会创建、更新或删除 Kubernetes 工作负载、PVC、Secret、实例，也不会编排 Compose 安装、升级、备份或恢复。运维人员负责清单应用、镜像替换、扩缩容、数据卷和 Secret 生命周期。

不需要 Helm、Ingress Controller、cert-manager 或 ACME。Caddy 使用固定的 `caddy:2.10.2-alpine` 镜像；它不是本项目构建产物。

## 文件和权限

- [`deploy/kubernetes/quoin.yaml`](../deploy/kubernetes/quoin.yaml)：可直接 `kubectl apply` 的普通多文档 YAML，定义六服务、内部 Service、ConfigMap、PVC 和 Secret 挂载。
- [`deploy/kubernetes/ops-services.yaml`](../deploy/kubernetes/ops-services.yaml)：内部运维端口 Service；不得公开暴露。
- [`deploy/compose.yaml`](../deploy/compose.yaml) 与 [`deploy/config`](../deploy/config)：本地/辅助环境的同一六服务示例。
- `deploy/secrets/`：仅供 Compose 示例的部署者私有目录，必须为 `0700`；不提交。Gateway TLS 文件应仅可被 Caddy UID 读取；Quoin 根密钥、Runtime TLS 身份和 Stele token 仅可被 Quoin UID 读取。

`quoin-data` 保存 SQLite 与 Artifact，`quoin-backups` 保存核心一致性备份，`plinth-state` 与 `lintel-state` 保存各 Runtime 的长期状态。不要删除或替换这些卷而不执行已批准的恢复流程。丢失 `quoin-data` 意味着权威业务数据丢失；丢失根密钥时，必须停机、独占数据库并执行 `quoin maintenance rebind-root-key`，随后重新录入受影响连接。丢失 Runtime 状态卷时，由 Admin 为原 Runtime slot 准备替换注册；不得创建额外 slot。普通 SQLite 备份不包含部署外部的 Stele service token、TLS 私钥或根密钥，Secret 必须由运维独立备份并与数据匹配保管。

## Kubernetes

选择 namespace，创建部署者持有的 TLS 和应用 Secret，然后应用普通清单：

```bash
kubectl create namespace quoin
kubectl -n quoin create secret tls gateway-tls \
  --cert=/secure/path/tls.crt --key=/secure/path/tls.key
kubectl -n quoin create secret generic quoin-secrets \
  --from-file=root-key=/secure/path/root-key \
  --from-file=runtime-ca.pem=/secure/path/runtime-ca.pem \
  --from-file=runtime-tls.crt=/secure/path/runtime-tls.crt \
  --from-file=runtime-tls.key=/secure/path/runtime-tls.key \
  --from-file=stele-service-token=/secure/path/stele-service-token
kubectl -n quoin apply -f deploy/kubernetes/quoin.yaml
kubectl -n quoin apply -f deploy/kubernetes/ops-services.yaml
```

先将 `quoin-config` 的 `publicOrigin` 改为精确公开 HTTPS Origin，并以发布的 digest 替换五个应用镜像。默认 gateway Service 是 `ClusterIP`；按集群网络条件由运维改为 `LoadBalancer` 或 `NodePort`。PVC 的 StorageClass、容量、备份策略和回收策略也由运维平台决定。

首次 Admin 初始化是核心应用的离线命令，而不是部署编排：停止长期 Quoin workload，使用与**同一**数据卷和 Secret 挂载的受控一次性容器/Pod，通过 attached TTY 执行 `quoin admin create --config /etc/quoin/component.yaml`，成功后再启动服务。临时密码不得进入命令行、环境变量或日志。`quoin admin reset-password`、`quoin backup --offline`、`quoin restore`、`quoin migrate` 和 `quoin maintenance recover-lintel` 同样要求长期 Quoin 已停止且调用者独占 SQLite；运维负责提供正确的 PVC/目录和只读 Secret 挂载。

## Compose

从仓库根目录运行：

```bash
mkdir -p deploy/secrets/gateway deploy/secrets/quoin
chmod 700 deploy/secrets deploy/secrets/gateway deploy/secrets/quoin
# 复制部署者保管的 TLS、根密钥、Runtime 身份和 Stele token。
docker compose -f deploy/compose.yaml up -d
```

`deploy/config/quoin.yaml` 的 `publicOrigin` 必须是公开 `https://` 地址；Plinth、Lintel 和 Stele 的 Runtime endpoint 保持 `https://quoin:8443`。仅 gateway 发布主机端口 `443`；应用、Runtime、webhook 和运维端口保持在 Compose 内部网络。命名卷保留 Quoin 数据/备份和 Plinth/Lintel 状态；`docker compose down -v` 会删除它们，除非运维已按恢复策略导出并验证数据和匹配 Secret。

## 核心备份、离线恢复和校验

已登录 Admin 可继续使用 Web 备份管理与定时策略创建核心一致性备份。在线管理不是在线覆盖恢复：恢复前必须停机并独占 SQLite。运维执行 `quoin backup --offline`、`quoin restore` 或 `quoin migrate` 前，应确认：目标数据目录/PVC、备份文件及其 checksum、版本兼容性、根密钥/Secret 匹配，以及没有其他 Quoin 进程持有数据库锁。恢复后先执行应用提供的校验，再启动长期服务并检查 Runtime 注册与健康状态。

## 静态检查

```bash
docker compose -f deploy/compose.yaml config >/dev/null
kubectl apply --dry-run=client -f deploy/kubernetes/quoin.yaml
```
