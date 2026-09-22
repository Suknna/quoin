# 部署

> 推荐使用 Kubernetes 部署，首次安装请阅读 [Kubernetes 启动指南](getting-started-kubernetes.md)。本文介绍部署配置、初始化与组件身份；完成后按[使用手册](user-guide.md)接入来源。如果环境不具备 Kubernetes，可采用 Docker Compose 部署，详见 [Docker Compose 安装指南](getting-started.md)。

Quoin 默认交付五个独立服务：入口 `gateway`（stock Caddy）、`frontend`、`quoin`、`plinth` 与 `stele`。浏览器、API、SSE 共用一个 HTTPS Origin；告警发送方使用同一 Origin 的 `/stele/` 路径。Caddy 优先转发 `/api/*` 到 Quoin、`/stele/*` 到 Stele，其他请求到前端，因此 SPA fallback 不会吞掉 API、SSE 或 WebSocket。

Quoin 采用插件体系扩展平台接入。当前可用插件为 **Prometheus、Thanos、Alertmanager**，`enabledPlugins` 使用 `prometheus`、`thanos`、`alertmanager`。浏览器与 Kubernetes 插件已移除，当前部署按上述五服务和三个插件配置。将 Quoin 部署在 Kubernetes 上与通过插件接入集群资源是不同能力。

Kubernetes Deployment controller 与 Docker Compose 管理部署生命周期。Quoin 不包含 `quoin-deploy`，不会创建、更新或删除 Kubernetes 工作负载、PVC、Secret、实例，也不会编排 Compose 安装、升级、备份或恢复。运维人员负责清单应用、镜像替换、扩缩容、数据卷和 Secret 生命周期。

不需要 Helm、Ingress Controller、cert-manager 或 ACME。Caddy 使用固定的 `caddy:2.10.2-alpine` 镜像；它不是本项目构建产物。

## 文件和权限

- [`deploy/kubernetes/quoin.yaml`](../deploy/kubernetes/quoin.yaml)：可直接 `kubectl apply` 的普通多文档 YAML，定义默认五服务、内部 Service、ConfigMap、PVC 和 Secret 挂载。
- [`deploy/kubernetes/ops-services.yaml`](../deploy/kubernetes/ops-services.yaml)：内部运维端口 Service；不得公开暴露。
- [`deploy/compose.yaml`](../deploy/compose.yaml) 与 [`deploy/config`](../deploy/config)：本地/辅助环境的同一拓扑示例。
- `deploy/secrets/`：仅供 Compose 示例的部署者私有目录，必须为 `0700`；不提交。Gateway TLS 文件应仅可被 Caddy UID 读取；Quoin 根密钥、Runtime TLS 身份和组件客户端证书仅可被 Quoin UID 读取。

`quoin-data` 保存 SQLite 与 Artifact，`quoin-backups` 保存核心一致性备份，`plinth-state` 保存活动 Runtime 的工作区状态。升级既有部署时，额外的历史卷应保留并单独评估，不作为新安装的依赖。不要删除或替换这些卷而不执行已批准的恢复流程。丢失 `quoin-data` 意味着权威业务数据丢失；丢失根密钥时，必须停机、独占数据库并执行 `quoin root-key rebind --config /etc/quoin/component.yaml`，随后重新录入受影响连接。普通 SQLite 备份不包含部署外部的组件客户端证书、TLS 私钥或根密钥，Secret 必须由运维独立备份并与数据匹配保管。

## 组件身份（mTLS）

内部组件认证是**一套 PKI**（[ADR-0009](adr/0009-unified-mtls-component-auth.md)）：部署方用仓库脚本 `scripts/generate-deployment-secrets.sh` 生成部署 CA（runtime-ca）并为 Stele、Plinth 各签发一张客户端证书（CN=stele / CN=plinth）。quoin 的 Runtime gRPC 监听（:8443）强制校验客户端证书；Stele 与 Plinth 拨号时出示各自证书，**启动即认证，不存在注册步骤**——部署完成、组件配置与 Secret 挂载正确，Plinth 就会自动连接并在「平台状态」页显示已连接。

丢失 Plinth 状态卷不再是凭据事故：组件身份来自 Secret 挂载，状态卷重建后直接重连。客户端证书与 CA 同寿命（10 年）；轮换是显式运维操作：

```bash
# 从既有部署 CA 重新签发组件客户端证书（--force 覆盖已有文件）
bash scripts/generate-deployment-secrets.sh <secrets-dir> --issue-client-certs --force
# Kubernetes：用 kubectl 以新文件重建 quoin-secrets（见下），再 rollout restart
```

轮换后更新部署 Secret 并重启 Stele/Plinth。持有 CA 私钥即可签发任意组件身份，该私钥只存在于部署 secrets 目录/Kubernetes Secret，与数据卷同等级保管。

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
  --from-file=stele-client.crt=/secure/path/stele-client.crt \
  --from-file=stele-client.key=/secure/path/stele-client.key \
  --from-file=plinth-client.crt=/secure/path/plinth-client.crt \
  --from-file=plinth-client.key=/secure/path/plinth-client.key
kubectl -n quoin apply -f deploy/kubernetes/quoin.yaml
kubectl -n quoin apply -f deploy/kubernetes/ops-services.yaml
```

上述 `quoin-secrets` 内容由部署方用仓库脚本生成（见 Kubernetes 启动指南）：`scripts/generate-deployment-secrets.sh <secrets-dir>` 产出 root-key、runtime-ca、runtime-tls 与两张组件客户端证书，再由运维以 kubectl 从文件创建 Secret。

先将 `quoin-config` 的 `publicOrigin` 改为精确公开 HTTPS Origin，并以发布的 tag 或 digest 替换四个默认应用镜像。默认 gateway Service 是 `ClusterIP`；按集群网络条件由运维改为 `LoadBalancer` 或 `NodePort`。PVC 的 StorageClass、容量、备份策略和回收策略也由运维平台决定。

首次 Admin 初始化不需要部署编排创建任何用户：首次启动 Quoin 在空库上自动播种唯一待初始化的内置管理员，初始密码由进程随机生成并写入数据目录 `initial-admin-password` 文件（0600，与 SQLite 同卷；启动日志只报路径与 24 小时期限）。运维用 `kubectl exec`/`docker compose exec` 读取该文件后，以 `admin` + 初始密码登录——单步登录签发受限会话，强制设置正式密码后自动进入工作台；初始密码随之永久失效（24 小时未改密则作废，见下文恢复）。不存在公开默认密码。

Quoin 定位为内网运维平台，不在产品内提供首次安装的所有权证明机制：公开默认凭据在首次初始化完成前不具备服务端防抢占能力，**首次部署的访问边界由部署环境负责**。建议将 Quoin 部署于受控内网或受限管理网络（gateway 不对不受信网络发布，Kubernetes 中保持 `ClusterIP` 并按需经内网 ingress 暴露），完成管理员初始化后再按需扩大可达范围；部署与恢复密码不得进入命令行参数、环境变量或日志。

管理员恢复以离线命令为准：停止长期 Quoin workload，使用与**同一**数据卷和 Secret 挂载的受控一次性容器/Pod，通过 attached TTY 执行 `quoin admin recover --config /etc/quoin/component.yaml`。部署仍在待初始化（初始密码过期或遗失）时，该命令重新生成随机初始密码、重写数据卷内的 0600 文件并重置 24 小时期限；管理员已初始化但被锁出时，命令经 TTY 交互设置临时密码。两种形态均撤销旧会话；不存在 `admin/admin` 默认密码，也不创建第二管理员；Web 登录页不提供独立的恢复入口。`quoin backup --offline`、`quoin restore` 和 `quoin migrate` 同样要求长期 Quoin 已停止且调用者独占 SQLite；运维负责提供正确的 PVC/目录和只读 Secret 挂载。

## Compose

从仓库根目录运行：

```bash
mkdir -p deploy/secrets/gateway
chmod 700 deploy/secrets deploy/secrets/gateway
# 运维脚本生成部署密钥（root-key、runtime-ca、runtime-tls、组件客户端证书）
bash scripts/generate-deployment-secrets.sh deploy/secrets/quoin
docker compose -f deploy/compose.yaml up -d
```

`deploy/config/quoin.yaml` 的 `publicOrigin` 必须是实际访问的 `https://` 地址；Plinth 和 Stele 的 Runtime endpoint 保持 `https://quoin:8443`。仅 gateway 发布主机端口 `443`；应用、Runtime、webhook 和运维端口保持在 Compose 内部网络。命名卷保留 Quoin 数据/备份和 Plinth 状态；`docker compose down -v` 会删除它们，除非运维已按恢复策略导出并验证数据和匹配 Secret。

## 镜像构建

日常安装优先从 [GitHub Releases](https://github.com/Suknna/quoin/releases) 取得与主机架构一致的离线镜像包：校验同名 `.sha256` 文件后执行 `docker load -i <包名>`。每个发布包包含 frontend、quoin、plinth、stele 和固定的 Caddy 镜像；不包含任何数据、Secret 或 TLS 证书。完整的版本规则和发布步骤见[发布指南](releasing.md)。

```bash
# 默认且唯一的主线组件集（frontend, quoin, plinth, stele）
deploy/images/build.sh
# 或单个组件（等价快捷方式）
make image COMPONENT=quoin VERSION=v1.0.2
```

默认构建四个应用镜像：frontend、quoin、plinth、stele。`QUOIN_IMAGE_NAMESPACE`、`QUOIN_IMAGE_TAG`、`QUOIN_IMAGE_VERSIONS` 与 `QUOIN_IMAGE_GOPROXY` 可按发布流程覆盖。`otp-test` 是仅用于一次性 e2e-real 拓扑的验证码接收 fixture 镜像（`deploy/images/otp-test/Dockerfile`，打包 `test/support/otpdelivery`），不属于任何生产组件集。

## 单组件热修升级

五个服务职责独立、镜像独立，故障修复**只需重新构建并替换对应组件的镜像**，其余组件不动：

1. **构建**：`make image COMPONENT=<frontend|quoin|plinth|stele> VERSION=<新版本号>`。
   版本号必须显式指定且不复用旧 tag（`/readyz` 的 `release` 字段按镜像报告版本，
   同 tag 变异无法审计）。
2. **分发**（本地构建 + 离线导入场景）：`docker save quoin/<component>:<版本> | gzip > <component>.tar.gz`，
   拷贝到生产节点后 `docker load < <component>.tar.gz`（或经私有 registry push/pull）。
3. **替换**：
   - Compose：`QUOIN_IMAGE=quoin/quoin:<版本> docker compose -f deploy/compose.yaml up -d quoin`
     ——只重建该服务；每服务都有独立的 `QUOIN_IMAGE`/`QUOIN_PLINTH_IMAGE`/`QUOIN_STELE_IMAGE`/`QUOIN_FRONTEND_IMAGE` 覆盖变量。
   - Kubernetes：`kubectl -n quoin set image deployment/<component> <component>=quoin/<component>:<版本>`
     ——只滚动该 Deployment。quoin 因 SQLite 独占使用 `Recreate` 策略，升级瞬间短暂停机属预期；
     frontend/plinth/stele 亦为单副本，但无状态或有状态卷重挂载即可恢复。
4. **验证**：`curl -k https://<ops-端点>:9090/readyz`（Kubernetes 经 `ops-services.yaml` 的 Service）
   查看 `release` 字段已变为新版本；Plinth/Stele 再看「平台状态」页已连接。

**版本偏斜政策**：任意组件可独立升级，前提是 Proto 契约指纹一致——连接握手（Hello/Deliver）
会强校验指纹并显式失败，不会静默错配。契约指纹变化的版本（如统一 mTLS 重构这一版）必须
五镜像同批替换，由发版流程保证。

## 核心备份、离线恢复和校验

已登录 Admin 可继续使用 Web 备份管理与定时策略创建核心一致性备份。在线管理不是在线覆盖恢复：恢复前必须停机并独占 SQLite。运维执行 `quoin backup --offline`、`quoin restore` 或 `quoin migrate` 前，应确认：目标数据目录/PVC、备份文件及其 checksum、版本兼容性、根密钥/Secret 匹配，以及没有其他 Quoin 进程持有数据库锁。

`quoin restore` 在隔离事务中撤销全部会话、连接与告警源凭据，保留唯一管理员并置其入待初始化状态（强制改密标记），生成临时密码。临时密码仅在 attached TTY 输出一次；数据库仅保存密码哈希，不提供 Web 恢复令牌或独立恢复页面。完成停机后校验和 `quoin restore finalize`，再启动长期服务，使用 `admin` 与临时密码从登录页进入受限会话、设置正式密码后直接进入工作台。临时密码若遗失，在服务停止时执行 `quoin admin recover` 替换；不要删除数据库或恢复默认密码。组件身份来自部署 Secret，恢复后 Plinth 自动重连，在「平台状态」页确认即可。

`quoin migrate`（含 `preflight` 子命令）是版本切换时的 schema 门：它只承认与当前镜像完全一致的
canonical schema（维护窗口已验证时退出 Upgrade 维护态）；摘要不符的数据库来自未发布构建，
没有迁移路径，需重建数据目录后重试。

## 静态检查

```bash
docker compose -f deploy/compose.yaml config >/dev/null
kubectl apply --dry-run=client -f deploy/kubernetes/quoin.yaml
go test ./deploy/...
```
