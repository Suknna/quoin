# 从零构建部署 Quoin（Kubernetes）

推荐使用 Kubernetes 部署 Quoin。如果环境不具备 Kubernetes，可使用 [Docker Compose 安装指南](getting-started.md)。本文从镜像构建开始，完成首次启动、管理员初始化、Plinth 自动连接和监控接入。

当前可用插件为 **Prometheus、Thanos、Alertmanager**，浏览器与 Kubernetes 资源接入插件计划后续提供。将 Quoin 部署在 Kubernetes 上不依赖 Kubernetes 资源接入插件。

## 0. 前置条件与安装约定

- 一个可用的 Kubernetes 集群，`kubectl` 已指向目标集群，具有创建 namespace、Deployment、Service、ConfigMap、Secret 和 PVC 的权限。
- 可供应 `ReadWriteOnce` 卷的 StorageClass。仓库默认申请 `quoin-data` 10Gi、`quoin-backups` 10Gi、`plinth-state` 10Gi；部署前按实际清单和容量调整。
- 构建机有 Docker、Bash、OpenSSL，可访问基础镜像和构建依赖。集群节点可以拉取构建后的镜像。
- 一个浏览器和 Alertmanager 都能访问的 HTTPS 地址，以及对应证书。
- **可用的 TLS SMTP 或 HTTPS webhook 验证码投递渠道**。首次初始化和后续登录都需要真实收码，不能只配置一个无法收信的邮箱。

本文选择独立 namespace **`quoin`**，示例通过 gateway NodePort **30443** 访问，Origin 为 **`https://quoin.example.com:30443`**。将示例域名换成自己的域名，并在浏览器端和告警发送方所在网络配置 DNS 指向可达的节点地址。证书 SAN 必须覆盖域名，证书不包含端口。

先检查环境，不修改当前上下文，也不复用已有 Quoin 数据：

```bash
kubectl config current-context
kubectl get nodes
kubectl get storageclass
kubectl get namespace quoin
```

最后一条在全新安装时预期返回 NotFound；**如果 namespace 已存在，先确认是否有已有部署，不能直接执行本指南覆盖 Secret 或重建身份。** 本文是全新安装流程，不是升级或恢复流程。

> NodePort 并非只绑定私网接口。首次初始化前通过防火墙或管理网络限制访问；默认 `admin/admin` 不提供防抢占能力。不要在不受信网络上开放未初始化实例。

## 1. 构建并分发镜像

在仓库根目录执行（新机器先克隆仓库并进入目录）：

```bash
export QUOIN_IMAGE_TAG=v0.1.0-dev
bash deploy/images/build.sh
```

构建四个应用镜像：`quoin/frontend`、`quoin/quoin`、`quoin/plinth`、`quoin/stele`。gateway 使用 `caddy:2.10.2-alpine`。

多节点或远程集群推荐推送到自己的镜像仓库。将下面的 `registry.example.com/team` 替换成你有权限使用的仓库，登录后执行：

```bash
REGISTRY=registry.example.com/team
for component in frontend quoin plinth stele; do
  docker tag "quoin/$component:v0.1.0-dev" "$REGISTRY/$component:v0.1.0-dev"
  docker push "$REGISTRY/$component:v0.1.0-dev"
done
```

私有仓库需在 namespace 中配置 `imagePullSecrets`，并在各 Deployment 的 Pod spec 中引用。镜像地址必须与后续清单完全一致。生产交付建议固定不可变 digest。

**本地镜像不是自动对所有 Kubernetes 可见。** 本次 mall-shop 所在单节点 k3s 使用 Docker runtime，已实测共享宿主 Docker 镜像，可直接保留 `quoin/<component>:v0.1.0-dev`。其他 containerd 集群需推送仓库或按其文档导入各节点，不能把这一特例当通用步骤。

## 2. 建立部署副本

仍在仓库根执行，部署目录放在 Git 仓库外。若目录已存在，命令拒绝继续，先检查而不是覆盖。

```bash
bash -eu <<'SH'
DEPLOY="$HOME/quoin-k8s"
if [ -e "$DEPLOY" ]; then
  printf '目录已存在，请先核对：%s\n' "$DEPLOY" >&2
  exit 1
fi
umask 077
mkdir -p "$DEPLOY/config" "$DEPLOY/secrets/runtime" "$DEPLOY/secrets/gateway" "$DEPLOY/bootstrap-data"
cp deploy/kubernetes/quoin.yaml "$DEPLOY/quoin.yaml"
cp deploy/kubernetes/ops-services.yaml "$DEPLOY/ops-services.yaml"
cp deploy/config/quoin.yaml "$DEPLOY/config/bootstrap.yaml"
SH
```

编辑副本 `$HOME/quoin-k8s/quoin.yaml`：

1. 将四个应用 Deployment 的 `image` 改成第 1 步实际可拉取的地址。
2. 在 `quoin-config` 的 `data.component.yaml` 多行配置内设置：

   ```yaml
   publicOrigin: https://quoin.example.com:30443
   stelePublicURL: https://quoin.example.com:30443/stele/alerts
   ```

   保持其他字段，特别是数据路径与 Secret 文件路径。`enabledPlugins` 为 `[alertmanager, prometheus, thanos]`。
3. 为三个 PVC 选择合适的 `storageClassName`；有默认 StorageClass 时可以不填。对于 `WaitForFirstConsumer`，PVC 在 Pod 被调度前 Pending 是正常现象。
4. 找到 **名为 gateway 的 Service**，改为下面的形式；不要修改内部 quoin/stele Service 的类型：

   ```yaml
   apiVersion: v1
   kind: Service
   metadata:
     name: gateway
   spec:
     type: NodePort
     selector:
       app.kubernetes.io/name: quoin
       app.kubernetes.io/component: gateway
     ports:
       - name: https
         port: 443
         targetPort: https
         nodePort: 30443
   ```

若 30443 已被其他 Service 使用，选择集群允许范围内的空闲端口，同时修改两个 URL。具备 LoadBalancer 时也可选择该方式；Origin 必须与用户最终访问地址一致。不要只修改浏览器端口而保留旧 Origin。

`plinth-config` 与 `stele-config` 的 `quoinRuntimeEndpoint: https://quoin:8443` 保持不变。这里的 `quoin` 是同 namespace 的 Service 名，必须与 Runtime 证书 SAN 一致。

## 3. 准备 Runtime 密钥与 Gateway 证书

### 3.1 仅首次空库生成 Runtime 密钥

复用 Quoin 自带 `secrets bootstrap`，不手写根密钥。此处在构建机的一次性容器里为**尚未创建的全新部署**生成身份；不启动 Quoin、不创建管理员。

```bash
cd "$HOME/quoin-k8s"
sudo chown 65532:65532 secrets/runtime bootstrap-data
sudo chmod 700 secrets/runtime bootstrap-data
# Docker bind 配置文件需允许容器UID读取；其中只有部署配置，没有秘密。
chmod 644 config/bootstrap.yaml

docker run --rm --user 65532:65532 \
  -v "$PWD/config/bootstrap.yaml:/etc/quoin/component.yaml:ro" \
  -v "$PWD/secrets/runtime:/run/quoin-secrets" \
  -v "$PWD/bootstrap-data:/var/lib/quoin/data" \
  quoin/quoin:v0.1.0-dev secrets bootstrap --config /etc/quoin/component.yaml
```

预期生成 `root-key`、`runtime-ca.pem`、`runtime-ca.key`、`runtime-tls.crt`、`runtime-tls.key`、`stele-client.crt/key`、`plinth-client.crt/key`。根密钥为 32 字节原始二进制；Runtime 服务端证书覆盖 `quoin`；组件客户端证书由同一 CA 签发（CN=stele / CN=plinth，ADR-0009）。全套秘密已存在时命令验证而不覆盖，部分存在则拒绝。

#### 确认密钥已生成

生成文件位于宿主机的 `$HOME/quoin-k8s/secrets/runtime/`，不是仓库目录。使用 sudo 查看文件名、权限与大小，不读取密钥内容：

```bash
sudo ls -lh "$HOME/quoin-k8s/secrets/runtime"
sudo stat -c '%n | %s bytes | mode=%a | uid=%u gid=%g' \
  "$HOME/quoin-k8s/secrets/runtime/"{root-key,runtime-ca.pem,runtime-ca.key,runtime-tls.crt,runtime-tls.key,stele-client.crt,stele-client.key,plinth-client.crt,plinth-client.key}
```

检查上述九个文件均存在，属主 UID 为 `65532`，文件权限为 `600`；`root-key` 应为 **32 字节**。证书和 PEM 私钥大小可能随生成结果变化，不要求固定字节数。

**普通用户看不到文件不等于生成失败。** 本步骤将目录属主设为容器用户 `65532`、目录权限设为 `0700`，因此普通宿主用户可能无法展开目录或收到 `Permission denied`。这是密钥保护措施，应通过上述 sudo 命令检查，不要因此重新生成密钥，也不要将目录或文件改成所有用户可读。

如果 sudo 检查仍显示文件缺失，再核对 bootstrap 命令的退出结果、错误输出和挂载路径；部分文件存在时不要删除后盲目重试。检查通过即可继续 **3.2 Gateway TLS**，无需查看或复制密钥内容到终端。

**禁止对已有 PVC 用这个空的本地目录重新生成密钥。** 已有部署必须保留与其数据库匹配的原始身份。`runtime-ca.key` 离线保管，不装入运行 Pod；所有秘密应独立备份。

### 3.2 Gateway TLS

使用组织 CA 签发的证书，准备为 `secrets/gateway/tls.crt` 与 `tls.key`，域名须与 Origin 一致。没有现成 CA 时，以下可用于本地演练；仅在新目录执行，不覆盖已有证书：

```bash
cd "$HOME/quoin-k8s"
umask 077
mkdir private-ca
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -keyout private-ca/ca.key -out private-ca/ca.crt -days 3650 \
  -subj '/CN=Quoin Kubernetes Lab CA' \
  -addext 'basicConstraints=critical,CA:TRUE' \
  -addext 'keyUsage=critical,keyCertSign,cRLSign'
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -keyout secrets/gateway/tls.key -out private-ca/gateway.csr \
  -subj '/CN=quoin.example.com'
printf '%s\n' 'subjectAltName=DNS:quoin.example.com' \
  'extendedKeyUsage=serverAuth' 'basicConstraints=critical,CA:FALSE' \
  > private-ca/gateway.ext
openssl x509 -req -in private-ca/gateway.csr \
  -CA private-ca/ca.crt -CAkey private-ca/ca.key -CAcreateserial \
  -out secrets/gateway/tls.crt -days 365 -extfile private-ca/gateway.ext
umask 022
```

浏览器和后续 Alertmanager 需信任 `private-ca/ca.crt`；只分发 CA 公开证书，不分发私钥。Runtime CA 与 Gateway CA 用途不同，不能混填。

## 4. 创建 namespace、Secret 并启动

```bash
cd "$HOME/quoin-k8s"
kubectl create namespace quoin
kubectl -n quoin create secret tls gateway-tls \
  --cert=secrets/gateway/tls.crt --key=secrets/gateway/tls.key
```

Runtime 文件由 UID 65532 拥有，普通宿主用户不可读。下面用本地 `sudo cat` 把指定文件通过匿名管道交给 **当前用户的 kubectl**，不使用 root 的 kubeconfig、不把秘密值放入参数或日志：

```bash
kubectl -n quoin create secret generic quoin-secrets \
  --from-file=root-key=<(sudo cat secrets/runtime/root-key) \
  --from-file=runtime-ca.pem=<(sudo cat secrets/runtime/runtime-ca.pem) \
  --from-file=runtime-tls.crt=<(sudo cat secrets/runtime/runtime-tls.crt) \
  --from-file=runtime-tls.key=<(sudo cat secrets/runtime/runtime-tls.key) \
  --from-file=stele-client.crt=<(sudo cat secrets/runtime/stele-client.crt) \
  --from-file=stele-client.key=<(sudo cat secrets/runtime/stele-client.key) \
  --from-file=plinth-client.crt=<(sudo cat secrets/runtime/plinth-client.crt) \
  --from-file=plinth-client.key=<(sudo cat secrets/runtime/plinth-client.key)
```

该命令需 **Bash**，不要加 `set -x`。核对文件确实存在、可读且 sudo 认证已就绪后再执行；导入失败时停止检查，不继续启动。不要把 Secret 输出为 YAML 提交到仓库。

```bash
kubectl -n quoin apply --dry-run=client -f quoin.yaml
kubectl -n quoin apply -f quoin.yaml
kubectl -n quoin get pvc,pods,svc
kubectl -n quoin rollout status deployment/quoin --timeout=180s
kubectl -n quoin rollout status deployment/stele --timeout=180s
kubectl -n quoin rollout status deployment/frontend --timeout=180s
kubectl -n quoin rollout status deployment/gateway --timeout=180s
```

Quoin 使用 SQLite，保持 **一个副本**；Plinth 也保持单副本和原状态卷。清单已配置非 root 用户和 `fsGroup`，不要为了启动临时加 privileged。若存储驱动不支持预期的卷权限，检查 PVC、驱动和挂载权限，而不是删除数据。

首次 Plinth 需等待 Quoin 接受其 Hello 才 Ready；**此时不要求所有 Pod Ready**。先完成管理员初始化，再看第 6 步的确认。

`ops-services.yaml` 是可选的内部运维 Service，不需要对外发布，也不是登录入口。

### 访问验证

将域名解析到节点地址，浏览器访问 `https://quoin.example.com:30443`。本机 mall-shop 集群节点为 `192.168.1.200`，可先用 curl 验证服务与 TLS，不依赖 DNS：

```bash
curl --fail --cacert private-ca/ca.crt \
  --resolve quoin.example.com:30443:192.168.1.200 \
  https://quoin.example.com:30443/ -o /dev/null
```

其他机器替换节点 IP。正式 CA 使用系统信任或对应 CA 文件。不推荐 `-k` 跳过验证。curl 的 `--resolve` 不会替浏览器或 Alertmanager 配置 DNS。

## 5. 管理员初始化与验证码

1. 使用 `admin/admin` 进入初始化页面，它不能直接登录工作台。
2. 设置正式密码，配置 TLS SMTP 或 HTTPS webhook 并测试投递。
3. 私网接收方配置最小 `allowPrivateCIDRs`，私有 CA 填入 `rootCaPem`。地址必须从 **Quoin Pod 内**可达，Pod 内的 `localhost` 不是节点。
4. 登记管理员联系方式，输入实际收到的验证码并完成初始化。
5. 使用正式密码和二级验证重新登录。

没有投递渠道时应先准备渠道，不能绕过初始化。测试夹具只适合隔离演练，不是生产邮件/短信服务；Compose 指南中的 Docker 网络地址也不能直接照搬为 Kubernetes 地址。

## 6. Plinth 自动连接

Plinth 无注册步骤：组件身份是 bootstrap 签发的客户端证书（CN=plinth），已随 `quoin-secrets` 挂载到 Deployment。管理员初始化完成后，在「设置 → 平台状态」确认 Plinth 显示**已连接**即可。若未连接，按顺序核对：`plinth-client.crt/key` 已进入 Secret 并被 Deployment 挂载、Runtime CA 一致、quoin Pod 健康；Plinth 每 2 秒自动重连。

## 7. 接入监控并完成首次巡检

按[使用手册](user-guide.md)进行：

- 创建 Prometheus/Thanos 接入，真实验证后启用；从观测资源核对目标。
- 创建 Alertmanager 告警源，保存一次性 receiver 配置。告警源 bearer 是该告警源自己的凭据，与内部组件身份无关。
- 让 Alertmanager 可解析 Gateway 域名、访问 30443 并信任 Gateway CA；配置 receiver 后验证 firing/resolved。
- 创建巡检计划并采证；配置并启用模型提供方后使用 AI SRE 和分析报告。

本机 mall-shop 的 Prometheus 地址为 `http://192.168.1.200:30090`；同集群也可使用其 Service 地址，实际信息见 [mall-shop 演练手册](mall-shop-lab.md)。是否可达以 Plinth 的真实 probe 为准，不以浏览器能打开代替。

## 8. 排障与日常维护

```bash
kubectl -n quoin get pods,pvc,svc
kubectl -n quoin get events --sort-by=.lastTimestamp
kubectl -n quoin logs deployment/quoin --tail=100
kubectl -n quoin logs deployment/stele --tail=100
kubectl -n quoin logs deployment/plinth --tail=100
```

| 现象 | 检查 |
| --- | --- |
| ImagePullBackOff | 镜像是否推送或导入正确 runtime、节点能否访问仓库、imagePullSecrets |
| PVC Pending | StorageClass、容量、绑定模式、节点调度；不要先删除 PVC |
| CrashLoopBackOff / permission denied | Secret 键名、文件内容、PVC 驱动对 fsGroup 的支持；查看日志 |
| Gateway 502 | quoin/stele/frontend 是否 Ready，内部 Service 是否有 endpoints |
| Plinth 未 Ready | 核对客户端证书挂载、Runtime CA、Service 名 quoin 与 DNS；不扩大副本数 |
| 证书或登录 Origin 错误 | 域名、端口、SAN、publicOrigin、stelePublicURL 是否一致 |
| 收不到验证码 | Pod 出站网络、TLS、私网 CIDR 和投递渠道真实可用性 |
| 配置修改未生效 | 多处配置/Secret 使用 subPath 挂载；应用更新后需受控重启相应 Deployment |

部署配置以本地 `quoin.yaml` 副本为准；修改后重新 apply，并按变更对象执行 `kubectl rollout restart deployment/<名称>`。不要在运行中的实例重新生成根密钥。备份恢复和离线维护见[部署参考](deployment.md)。

暂时停止可将各 Deployment 缩为 0，保留 PVC 与 Secret；恢复时恢复原单副本。**删除 namespace 会同时删除其 PVC 和 Secret，绝不能当作排障或重置密码步骤。**

## 完成检查

- [ ] 镜像可拉取，三个 PVC 正常绑定，五个服务运行正常。
- [ ] HTTPS 证书可信，浏览器 Origin 与配置一致。
- [ ] 管理员初始化完成，正式密码加二级验证可登录。
- [ ] Plinth 已连接且 Ready。
- [ ] 指标接入验证成功，观测目标可见。
- [ ] Alertmanager 新告警源能收到 firing 与 resolved。
- [ ] 首次巡检能查看真实 Evidence；启用模型后可查看分析报告。

本文提供人工安装步骤，不表示上述检查已在你的新实例上自动完成。
