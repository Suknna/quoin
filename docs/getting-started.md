# 从零构建部署 Quoin（Docker Compose）

> 推荐使用 [Kubernetes 启动指南](getting-started-kubernetes.md)。如果环境不具备 Kubernetes，可按本文采用 Docker Compose 部署。

本文供首次使用者**自己安装和初始化 Quoin**。业务环境已单独准备，参数见 [mall-shop 演练](mall-shop-lab.md)。默认五服务为 gateway、frontend、quoin、plinth、stele；当前可用插件为 Prometheus、Thanos、Alertmanager，浏览器与 Kubernetes 插件计划后续接入。

本手册对应当前源码。认证/审计整体切换在领域文档中仍标记为进行中，不等于安全全验收。[本轮验证记录](acceptance/mall-shop-handbook-20260916.md)区分了已验证的构建与未替用户执行的初始化。

## 0. 准备条件

- Linux、Docker Engine、`docker compose`、OpenSSL、Bash；可以使用 sudo 设置非 root 容器的挂载权限。
- 能访问镜像仓库及 Go/npm 依赖源。Dockerfile 内构建前端，不要求宿主安装 Node；源码测试另需 Go、Node、pnpm。
- 一个你控制的主机名，例如 `quoin.lab.example.com`，在浏览器机器及 Alertmanager 所在网络均能解析到这台宿主。可使用内部 DNS；不要把示例域名当成真实公网服务。
- **真实可用的验证码渠道**：TLS SMTP 或 HTTPS webhook。没有投递通道不能完成管理员初始化；仅本地演练可使用[附录中的 OTP fixture](#附录仅演练的验证码接收器)，仍须完成真实收码验证。
- 首次初始化前限制入口到可信管理网络。公开的 `admin/admin` 没有所有权防抢占能力。端口发布不是防火墙；不要向公网开放未初始化实例。

本机 443 已被 k3s 使用，本文使用 **8443**。选定 Origin 为 `https://quoin.lab.example.com:8443`，证书、配置和浏览器地址必须一致。实际操作时将域名换成你的名字，并准备好 DNS；本机演练也可将其映射到 `192.168.1.200`，但仅改宿主 `/etc/hosts` 不会自动影响 Pod DNS。

## 1. 构建四个镜像

在当前仓库根目录执行；新机器先 `git clone https://github.com/Suknna/quoin.git` 并进入仓库。

```bash
export QUOIN_IMAGE_TAG=v0.1.0-dev
bash deploy/images/build.sh
```

默认构建 `quoin/frontend`、`quoin/quoin`、`quoin/plinth`、`quoin/stele`。Caddy 使用 `caddy:2.10.2-alpine`，不是本项目构建产物。可覆盖 `QUOIN_IMAGE_NAMESPACE`、`QUOIN_IMAGE_TAG`、`QUOIN_IMAGE_VERSIONS`、`QUOIN_IMAGE_GOPROXY`，但后续镜像名必须一致。本文统一使用 `v0.1.0-dev`。

不要运行 `make e2e-real` 来代替安装：它建立隔离的开发验收环境，不是本次人工安装流程。

可选源码检查：

```bash
pnpm install --frozen-lockfile
make test vet web-typecheck web-lint web-test web-build
```

## 2. 建立不覆盖旧数据的独立部署目录

以下代码在仓库根目录执行。**如果目录已存在则停止检查，不覆盖它**。继续操作均在新目录中进行。

```bash
REPO="$PWD"
DEPLOY="$HOME/quoin-mall-user"
if [ -e "$DEPLOY" ]; then
  printf '部署目录已存在，请先核对其中数据：%s\n' "$DEPLOY"
else
  mkdir -p "$DEPLOY/config" "$DEPLOY/secrets/gateway" "$DEPLOY/secrets/quoin"
  cp "$REPO/deploy/compose.yaml" "$DEPLOY/compose.yaml"
  cp "$REPO/deploy/config/"*.yaml "$DEPLOY/config/"
  chmod 700 "$DEPLOY/secrets" "$DEPLOY/secrets/gateway" "$DEPLOY/secrets/quoin"
fi
```

只有确认它是本次新建目录，才继续。编辑副本 `compose.yaml`：

- 顶部 `name: quoin` 改为 **`name: quoin-mall-user`**，以隔离网络和数据卷。
- gateway 的 `ports: ["443:8443"]` 改为 **`ports: ["8443:8443"]`**。这会发布到宿主接口，务必通过网络边界限制访问。仅本机浏览器验证可暂用 `127.0.0.1:8443:8443`，但 Alertmanager Pod 无法向宿主回环回调，接入告警前需改成其可达的受限管理地址。

本文直接编辑副本，所有后续命令只用一个文件。若你自行采用多文件合并，端口列表应使用 `ports: !override ["8443:8443"]`，需要 Compose **2.24.4+**，而且每条后续命令都要带相同的 `-f` 列表；普通追加不会移除原来的 443 发布。

编辑副本 `config/quoin.yaml` 的已有字段，保留其他字段，不要用此片段覆盖整个配置：

```yaml
enabledPlugins: [alertmanager, prometheus, thanos]
publicOrigin: https://quoin.lab.example.com:8443
stelePublicURL: https://quoin.lab.example.com:8443/stele/alerts
```

`plinth.yaml` 和 `stele.yaml` 中 `quoinRuntimeEndpoint: https://quoin:8443` 保持不变。内部 Runtime TLS 与浏览器入口 TLS 是两套证书。

## 3. 创建卷并生成 Runtime 密钥

容器 UID：Quoin、Plinth、Stele 为 **65532**；gateway 为 **1000**。下面只操作本次新项目的卷。首次创建后设置卷根目录属主，不递归改动已有业务数据。

```bash
cd "$HOME/quoin-mall-user"
docker compose config --quiet
for v in quoin-data quoin-backups plinth-state plinth-workspaces; do
  docker volume create "quoin-mall-user_$v" >/dev/null
  docker run --rm -v "quoin-mall-user_$v:/v" alpine:3.20 chown 65532:65532 /v
done
for v in gateway-data gateway-config; do
  docker volume create "quoin-mall-user_$v" >/dev/null
  docker run --rm -v "quoin-mall-user_$v:/v" alpine:3.20 chown 1000:1000 /v
done
sudo chown 65532:65532 secrets/quoin
sudo chmod 700 secrets/quoin

docker run --rm --user 65532:65532 \
  -v "$PWD/config/quoin.yaml:/etc/quoin/component.yaml:ro" \
  -v "$PWD/secrets/quoin:/run/quoin-secrets" \
  -v quoin-mall-user_quoin-data:/var/lib/quoin/data \
  quoin/quoin:v0.1.0-dev secrets bootstrap --config /etc/quoin/component.yaml
```

此命令挂载了正常服务的**同一数据卷**，不能在已有实例恢复时省略它。空数据库且秘密目录为空才生成；全套秘密已存在时只校验；部分秘密存在或已有数据库但缺秘密时拒绝。运行中的实例不得用这个流程重置身份。

生成文件为 `root-key`、`runtime-ca.pem`、`runtime-ca.key`、`runtime-tls.crt`、`runtime-tls.key`、`stele-client.crt/key`、`plinth-client.crt/key`。根密钥是恰好 **32 字节原始二进制**，不是 Base64 或十六进制文本，文件权限 `0600`。Runtime 服务端证书包含 SAN `quoin`、`localhost`；两张组件客户端证书由同一 CA 签发（CN=stele / CN=plinth，ADR-0009）。不要手搓根密钥或复用历史验收秘密。

`root-key` 必须与数据库匹配备份。密钥丢失后的受控重新绑定会使旧连接秘密不可用，需要重新录入；不能靠生成新密钥恢复旧秘密。

## 4. 准备 Gateway TLS

已有内部 CA 时由其签发服务端证书，放为 `secrets/gateway/tls.crt`、`tls.key`。以下仅为新目录中的本地演练 CA 示例，私钥生成在仓库外；**不要对已有证书运行以免覆盖**。

```bash
cd "$HOME/quoin-mall-user"
umask 077
mkdir private-ca
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -keyout private-ca/ca.key -out private-ca/ca.crt -days 3650 \
  -subj '/CN=Quoin Mall User Lab CA' \
  -addext 'basicConstraints=critical,CA:TRUE' \
  -addext 'keyUsage=critical,keyCertSign,cRLSign'
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -keyout secrets/gateway/tls.key -out private-ca/gateway.csr \
  -subj '/CN=quoin.lab.example.com'
printf '%s\n' 'subjectAltName=DNS:quoin.lab.example.com' \
  'extendedKeyUsage=serverAuth' 'basicConstraints=critical,CA:FALSE' \
  > private-ca/gateway.ext
openssl x509 -req -in private-ca/gateway.csr \
  -CA private-ca/ca.crt -CAkey private-ca/ca.key -CAcreateserial \
  -out secrets/gateway/tls.crt -days 365 -extfile private-ca/gateway.ext
sudo chown -R 1000:1000 secrets/gateway
sudo chmod 700 secrets/gateway
sudo chmod 600 secrets/gateway/tls.key secrets/gateway/tls.crt
umask 022
```

将**公开证书** `private-ca/ca.crt` 导入浏览器信任，后续也提供给 Alertmanager 校验 Gateway；不要导出 CA 私钥。CA 私钥不挂入 gateway。Docker 将指定的 bind 源直接挂入容器，关键是挂载目录本身及其内部权限；不必把整个宿主秘密父目录开放为 `755`。

## 5. 启动并检查

```bash
cd "$HOME/quoin-mall-user"
docker compose up -d
docker compose ps
docker compose logs --tail=80 quoin plinth stele gateway
curl --fail --cacert private-ca/ca.crt \
  --resolve quoin.lab.example.com:8443:127.0.0.1 \
  https://quoin.lab.example.com:8443/ -o /dev/null
```

预期 Quoin/Stele/Plinth 的 liveness 检查通过，gateway 提供登录页面。Plinth 以部署 CA 签发的客户端证书自动连接（ADR-0009，无注册步骤）；readiness 在 Quoin 接受其 Hello 后翻绿。上述 curl 用正确域名校验证书，不使用 `-k` 跳过 TLS。

## 6. 人工完成管理员初始化

1. 浏览器打开你配置的精确 Origin。
2. 用 **`admin/admin`** 登录初始化入口。它不能签发工作台会话。
3. 设置正式密码；配置并**测试**验证码投递；登记并真实验证管理员联系方式。
4. 完成后默认密码永久关闭，返回登录页，以正式密码加二级验证登录。

SMTP 必须使用 STARTTLS 或 implicit TLS；webhook 必须 HTTPS。私网接收方要显式允许其最小 CIDR，并提供其 CA，不能全局关闭 TLS 校验。初始化不能省略投递测试或收码验证；如果没有真实邮件服务，使用下方演练 fixture。

## 7. Plinth 自动连接

Plinth 无注册步骤：组件身份是 secrets bootstrap 签发的客户端证书（CN=plinth），随配置挂载启动即连。管理员初始化完成后，在「设置 → 平台状态」确认 Plinth 显示**已连接**即可。若未连接，按顺序核对：组件配置的 `quoinRuntimeClientCertificateFile`/`quoinRuntimeClientPrivateKeyFile` 挂载、Runtime CA 一致、quoin 服务健康；Plinth 每 2 秒自动重连。

## 8. 接入 mall-shop

继续[使用手册](user-guide.md)：

- Prometheus 地址填写 **`http://192.168.1.200:30090`**（本机演练参数，`authType=none`），真实验证后启用。不要填写容器里的 `localhost`。
- 创建新的 Alertmanager 告警源，将其一次性 receiver 配置合入上游。告警源 bearer 是该告警源自己的凭据，与内部组件身份无关。Alertmanager 必须能解析 Gateway 域名、访问 8443 并信任 Gateway CA。
- 数据库、Java、Nginx 通过现有 exporter 指标接入；当前使用 Prometheus 插件查询这些指标；Kubernetes 资源直接接入将在后续插件中提供。

## 常见问题与停止

| 现象 | 检查 |
| --- | --- |
| `permission denied` | secrets/quoin 为 65532、gateway 目录及证书为 1000；卷根目录可写 |
| bootstrap 拒绝 | 核对同一数据卷与完整原秘密，不能重建数据库解决 |
| 证书错误 | SAN、实际域名与 CA 信任是否一致；不要默认跳过验证 |
| 登录卡在初始化 | 投递必须 TLS、私网 CIDR 允许且真实收码 |
| Plinth 未 Ready | 核对客户端证书挂载与 Runtime CA；Plinth 每 2 秒自动重连 |
| 指标验证失败 | 从容器到上游的路由、DNS、端口及认证；查看实际 probe 结果 |

暂时停止而不删除数据：`docker compose stop`；恢复：`docker compose up -d`。**不要 `down -v`**。离线管理员恢复、根密钥重新绑定、备份与恢复见[部署参考](deployment.md)，均不得绕过其停机/独占 SQLite 前提。

## 附录：仅演练的验证码接收器

复用仓库已有 `otp-test`，不另写短信或邮件服务。它只在 Docker 内网提供 HTTPS/implicit-TLS SMTP，记录明文验证码到私有 JSONL，不属于生产组件。仍由你人工填写真实收到的码，没有免验证入口。

在仓库根构建：

```bash
QUOIN_IMAGE_COMPONENTS=otp-test QUOIN_IMAGE_TAG=v0.1.0-dev bash deploy/images/build.sh
```

在新部署目录创建一次（已有 `fixture-otp` 时先检查，不重复生成覆盖）：

```bash
cd "$HOME/quoin-mall-user"
umask 077
mkdir -p fixture-otp/tls fixture-otp/records
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -keyout fixture-otp/tls/tls.key -out fixture-otp/tls/tls.crt -days 30 \
  -subj '/CN=otp-fixture' -addext 'subjectAltName=DNS:otp-fixture' \
  -addext 'basicConstraints=critical,CA:TRUE'
# 保留一份公开信任证书供界面填写，私钥只给 fixture 容器读取。
cp fixture-otp/tls/tls.crt fixture-otp/trust.pem
sudo chown -R 65532:65532 fixture-otp/tls fixture-otp/records
sudo chmod 700 fixture-otp/tls fixture-otp/records
sudo chmod 600 fixture-otp/tls/tls.key fixture-otp/tls/tls.crt
umask 022

docker run -d --name quoin-mall-user-otp --restart unless-stopped \
  --user 65532:65532 --network quoin-mall-user_default --network-alias otp-fixture \
  -v "$PWD/fixture-otp/tls:/tls:ro" \
  -v "$PWD/fixture-otp/records:/run/otp" \
  quoin/otp-test:v0.1.0-dev \
  --tls-cert=/tls/tls.crt --tls-key=/tls/tls.key \
  --https-listen=0.0.0.0:8445 --smtp-listen=0.0.0.0:8587 \
  --record=/run/otp/deliveries.jsonl

docker network inspect quoin-mall-user_default \
  --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}'
```

没有 `-p`，不把验证码接收器公开到宿主。初始化投递配置选择 HTTPS webhook：

- URL：**`https://otp-fixture:8445/otp`**（现有 fixture 接受 POST 路径，约定用 `/otp`）。
- CA：`fixture-otp/trust.pem` 的完整 PEM 内容。
- 允许私网 CIDR：上面 inspect 返回的**本项目实际 Docker 子网**，不要填 `0.0.0.0/0`。
- 选择邮箱联系方式，例如你用于演练的邮箱地址；这里不会给真实外部邮箱发信，验证码写到 fixture。

点击投递测试或发送验证码后，在本地终端读取最新记录：

```bash
docker exec quoin-mall-user-otp sh -c 'tail -n 1 /run/otp/deliveries.jsonl'
```

查看 JSON 的 `code` 并在界面提交。别把含验证码的终端输出发到工单或 Git。**后续每次登录仍依赖它**：在配置并验证新的正式投递通道前，不要停止 fixture。演练结束且不再依赖时可 `docker stop quoin-mall-user-otp`，保留秘密与数据直到确认可安全清理。
