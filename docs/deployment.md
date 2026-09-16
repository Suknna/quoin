# 部署

> 推荐使用 Kubernetes 部署，首次安装请阅读 [Kubernetes 启动指南](getting-started-kubernetes.md)。本文介绍部署配置、初始化与运行时注册；完成后按[使用手册](user-guide.md)接入来源。如果环境不具备 Kubernetes，可采用 Docker Compose 部署，详见 [Docker Compose 安装指南](getting-started.md)。

Quoin 默认交付五个独立服务：入口 `gateway`（stock Caddy）、`frontend`、`quoin`、`plinth` 与 `stele`。浏览器、API、SSE 共用一个 HTTPS Origin；告警发送方使用同一 Origin 的 `/stele/` 路径。Caddy 优先转发 `/api/*` 到 Quoin、`/stele/*` 到 Stele，其他请求到前端，因此 SPA fallback 不会吞掉 API、SSE 或 WebSocket。

Quoin 采用插件体系扩展平台接入。当前可用插件为 **Prometheus、Thanos、Alertmanager**，`enabledPlugins` 使用 `prometheus`、`thanos`、`alertmanager`。**浏览器与 Kubernetes 插件计划后续接入**，当前部署按上述五服务和三个插件配置。将 Quoin 部署在 Kubernetes 上与通过插件接入集群资源是不同能力。

Kubernetes Deployment controller 与 Docker Compose 管理部署生命周期。Quoin 不包含 `quoin-deploy`，不会创建、更新或删除 Kubernetes 工作负载、PVC、Secret、实例，也不会编排 Compose 安装、升级、备份或恢复。运维人员负责清单应用、镜像替换、扩缩容、数据卷和 Secret 生命周期。

不需要 Helm、Ingress Controller、cert-manager 或 ACME。Caddy 使用固定的 `caddy:2.10.2-alpine` 镜像；它不是本项目构建产物。

## 文件和权限

- [`deploy/kubernetes/quoin.yaml`](../deploy/kubernetes/quoin.yaml)：可直接 `kubectl apply` 的普通多文档 YAML，定义默认五服务、内部 Service、ConfigMap、PVC 和 Secret 挂载。
- [`deploy/kubernetes/ops-services.yaml`](../deploy/kubernetes/ops-services.yaml)：内部运维端口 Service；不得公开暴露。
- [`deploy/compose.yaml`](../deploy/compose.yaml) 与 [`deploy/config`](../deploy/config)：本地/辅助环境的同一拓扑示例。
- `deploy/secrets/`：仅供 Compose 示例的部署者私有目录，必须为 `0700`；不提交。Gateway TLS 文件应仅可被 Caddy UID 读取；Quoin 根密钥、Runtime TLS 身份和 Stele token 仅可被 Quoin UID 读取。

`quoin-data` 保存 SQLite 与 Artifact，`quoin-backups` 保存核心一致性备份，`plinth-state` 保存活动 Runtime 的长期状态。升级既有部署时，额外的历史卷应保留并单独评估，不作为新安装的依赖。不要删除或替换这些卷而不执行已批准的恢复流程。丢失 `quoin-data` 意味着权威业务数据丢失；丢失根密钥时，必须停机、独占数据库并执行 `quoin root-key rebind --config /etc/quoin/component.yaml`，随后重新录入受影响连接。丢失 Runtime 状态卷时，由 Admin 为原 Runtime slot 准备替换注册；不得创建额外 slot。普通 SQLite 备份不包含部署外部的 Stele service token、TLS 私钥或根密钥，Secret 必须由运维独立备份并与数据匹配保管。

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

先将 `quoin-config` 的 `publicOrigin` 改为精确公开 HTTPS Origin，并以发布的 digest 替换四个默认应用镜像。默认 gateway Service 是 `ClusterIP`；按集群网络条件由运维改为 `LoadBalancer` 或 `NodePort`。PVC 的 StorageClass、容量、备份策略和回收策略也由运维平台决定。

首次 Admin 初始化不需要部署编排创建任何用户：首次启动 Quoin 在空库上自动播种唯一待初始化的内置管理员（用户名/初始密码为公开默认值 `admin/admin`，仅初始化状态可用，永不签发工作台会话）。运维在浏览器打开公开 Origin，用默认凭据登录后直接进入统一初始化流程：设置正式密码、配置并测试验证消息投递（SMTP/HTTPS webhook；指向私网接收方时必须显式放通私网 CIDR 并提供 CA 证书，投递配置加密存库）、登记并真实验证管理员邮箱或手机号，最后原子完成初始化——默认密码入口永久关闭，返回登录页以正式密码加二级验证进入工作台。

Quoin 定位为内网运维平台，不在产品内提供首次安装的所有权证明机制：公开默认凭据在首次初始化完成前不具备服务端防抢占能力，**首次部署的访问边界由部署环境负责**。建议将 Quoin 部署于受控内网或受限管理网络（gateway 不对不受信网络发布，Kubernetes 中保持 `ClusterIP` 并按需经内网 ingress 暴露），完成管理员初始化后再按需扩大可达范围；部署与恢复密码不得进入命令行参数、环境变量或日志。

管理员恢复以离线命令为准：停止长期 Quoin workload，使用与**同一**数据卷和 Secret 挂载的受控一次性容器/Pod，通过 attached TTY 执行 `quoin admin recover --config /etc/quoin/component.yaml --mode password`（仅忘记密码：设置新的临时密码，保留已验证的二级验证方式，随后启动服务用该密码登录并完成统一初始化流程）或 `--mode factors`（所有因素均不可用：重置全部因素并设置新的临时密码，随后启动服务登录并重新绑定联系方式）。两种模式均撤销旧会话与挑战；不得恢复 `admin/admin` 默认密码，也不创建第二管理员；恢复后的统一初始化流程与首次安装完全一致，Web 登录页不提供独立的恢复入口。`quoin backup --offline`、`quoin restore` 和 `quoin migrate` 同样要求长期 Quoin 已停止且调用者独占 SQLite；运维负责提供正确的 PVC/目录和只读 Secret 挂载。

## Plinth 首次注册

管理员在“管理 → 运行时”选择“准备首次注册”，确认后页面仅显示一次完整的 `{slot,generation,token}` JSON。先停止目标 Plinth，保持原状态卷、组件配置和 Runtime CA 挂载，在同一镜像的一次性容器中执行 `plinth register --config /etc/quoin/component.yaml`，将完整 JSON 作为一行输入标准输入。不要把 token 放入命令参数、环境变量、YAML 或日志。成功后启动正常单副本，确认 `registered`、`已连接` 与 Ready；长期 token 由组件保存到状态卷的 `0600` 文件。短时令牌过期或已经消费时，应从管理页面重新准备，而非修改数据库或伪造凭据代。

## Compose

从仓库根目录运行：

```bash
mkdir -p deploy/secrets/gateway deploy/secrets/quoin
chmod 700 deploy/secrets deploy/secrets/gateway deploy/secrets/quoin
# 复制部署者保管的 TLS、根密钥、Runtime 身份和 Stele token。
docker compose -f deploy/compose.yaml up -d
```

`deploy/config/quoin.yaml` 的 `publicOrigin` 必须是实际访问的 `https://` 地址；Plinth 和 Stele 的 Runtime endpoint 保持 `https://quoin:8443`。仅 gateway 发布主机端口 `443`；应用、Runtime、webhook 和运维端口保持在 Compose 内部网络。命名卷保留 Quoin 数据/备份和 Plinth 状态；`docker compose down -v` 会删除它们，除非运维已按恢复策略导出并验证数据和匹配 Secret。

## 镜像构建

```bash
# 默认且唯一的主线组件集（frontend, quoin, plinth, stele）
deploy/images/build.sh
```

默认构建四个应用镜像：frontend、quoin、plinth、stele。`QUOIN_IMAGE_NAMESPACE`、`QUOIN_IMAGE_TAG`、`QUOIN_IMAGE_VERSIONS` 与 `QUOIN_IMAGE_GOPROXY` 可按发布流程覆盖。`otp-test` 是仅用于一次性 e2e-real 拓扑的验证码接收 fixture 镜像（`deploy/images/otp-test/Dockerfile`，打包 `test/support/otpdelivery`），不属于任何生产组件集，只通过 `QUOIN_IMAGE_COMPONENTS=...,otp-test` 显式构建。

## 核心备份、离线恢复和校验

已登录 Admin 可继续使用 Web 备份管理与定时策略创建核心一致性备份。在线管理不是在线覆盖恢复：恢复前必须停机并独占 SQLite。运维执行 `quoin backup --offline`、`quoin restore` 或 `quoin migrate` 前，应确认：目标数据目录/PVC、备份文件及其 checksum、版本兼容性、根密钥/Secret 匹配，以及没有其他 Quoin 进程持有数据库锁。

`quoin restore` 在隔离事务中撤销全部会话、流程、Runtime 注册、连接与告警源凭据，保留唯一管理员并清除其二级验证方式，将管理员改为待初始化状态并生成临时密码。临时密码仅在 attached TTY 输出一次；数据库仅保存密码哈希，不提供 Web 恢复令牌或独立恢复页面。完成停机后校验和 `quoin restore finalize`，再启动长期服务，使用 `admin` 与临时密码从普通登录页进入统一初始化流程，设置正式密码、重新验证联系方式，完成后返回登录页进行密码加二级验证登录。临时密码若遗失，在服务停止时执行 `quoin admin recover` 替换；不要删除数据库或恢复默认密码。恢复后还需检查 Runtime 重新注册与健康状态。

## 静态检查

```bash
docker compose -f deploy/compose.yaml config >/dev/null
kubectl apply --dry-run=client -f deploy/kubernetes/quoin.yaml
go test ./deploy/...
```
