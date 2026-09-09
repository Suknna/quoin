# 部署

Issue #93 的交付是六个独立服务：入口 `gateway`（stock Caddy）、`frontend`、`quoin`、`plinth`、`lintel` 和 `stele`。浏览器、API、SSE 与 noVNC WebSocket 共用一个 HTTPS Origin；告警发送方使用同一 Origin 的 `/stele/` 路径。Caddy 优先转发 `/api/*` 到 Quoin、`/stele/*` 到 Stele，其余请求到前端，因此 SPA fallback 不会吞掉 API、SSE 或 WebSocket 请求。

不需要 Helm、Ingress Controller、cert-manager 或 ACME。Caddy 使用固定的 `caddy:2.10.2-alpine` 镜像；它不是本项目构建产物。前端使用独立的 `quoin/frontend:v0.1.0-dev` 镜像，容器内静态 HTTP 监听 `8080`。

## Kubernetes

[`deploy/kubernetes/quoin.yaml`](../deploy/kubernetes/quoin.yaml) 是普通多文档 YAML，没有固定 `metadata.namespace`。先选择 namespace 并准备入口证书：

```bash
kubectl create namespace quoin
kubectl -n quoin create secret tls gateway-tls \
  --cert=/secure/path/tls.crt --key=/secure/path/tls.key
```

全新安装请直接继续下方“First Kubernetes installation”，不要重复创建运行时 Secret。只有接入**已有完整运行时秘密与已初始化数据卷**时，才使用下面的导入路径；禁止对已有数据重新生成根密钥：

```bash
kubectl -n quoin create secret generic quoin-secrets \
  --from-file=root-key=/secure/path/root-key \
  --from-file=runtime-ca.pem=/secure/path/runtime-ca.pem \
  --from-file=runtime-tls.crt=/secure/path/runtime-tls.crt \
  --from-file=runtime-tls.key=/secure/path/runtime-tls.key \
  --from-file=stele-service-token=/secure/path/stele-service-token
kubectl -n quoin apply -f deploy/kubernetes/quoin.yaml
```

`quoin-secrets` contains existing runtime identity material, not public certificates. It is generated/rotated through the existing Quoin bootstrap and credential procedures; never commit these files. `gateway-tls` must contain the public certificate as `tls.crt` and private key as `tls.key`; certificate issuance and replacement remain the deployer's responsibility.

Before applying, update `quoin-config`'s `publicOrigin` from `https://quoin.example.com` to the exact public HTTPS Origin. Replace the four application and one frontend image references with release digest references before production use. The Caddy image stays at the fixed stock tag. The manifest keeps the default gateway Service as `ClusterIP`; patch it to `LoadBalancer` or `NodePort` only if the target cluster requires that exposure mode:

```bash
kubectl -n quoin patch service gateway -p '{"spec":{"type":"LoadBalancer"}}'
```

The manifest uses fixed names within its namespace: deployments `gateway`, `frontend`, `quoin`, `plinth`, `lintel`, `stele`; services `gateway`, `frontend`, `quoin`, `stele`; ConfigMaps `gateway-config`, `quoin-config`, `plinth-config`, `lintel-config`, `stele-config`; PVCs `quoin-data`, `quoin-backups`, `plinth-state`, `lintel-state`. `quoin:8443` is intentionally retained as the internal Runtime TLS identity alias. Operational `9090` listeners are available only through the internal ClusterIP Services in [`deploy/kubernetes/ops-services.yaml`](../deploy/kubernetes/ops-services.yaml); they are not publicly exposed or routed by Caddy.

### First Kubernetes installation

Quoin must generate the runtime secret set and create the first administrator before the long-running Deployment is useful. Apply the ordinary first-install files in this order. Holding Quoin at zero avoids a database lock while the one-off Job and administrator Pod use the retained PVCs:

```bash
kubectl -n quoin apply -f deploy/kubernetes/quoin.yaml
kubectl -n quoin apply -f deploy/kubernetes/ops-services.yaml
kubectl -n quoin scale deployment/quoin --replicas=0
kubectl -n quoin create secret generic quoin-secrets
kubectl -n quoin apply -f deploy/kubernetes/bootstrap.yaml
kubectl -n quoin wait --for=condition=complete job/quoin-bootstrap --timeout=5m
kubectl -n quoin apply -f deploy/kubernetes/admin.yaml
kubectl -n quoin attach -it quoin-admin
kubectl -n quoin wait --for=jsonpath='{.status.phase}'=Succeeded pod/quoin-admin --timeout=5m
kubectl -n quoin delete pod/quoin-admin job/quoin-bootstrap \
  serviceaccount/quoin-bootstrap role/quoin-bootstrap rolebinding/quoin-bootstrap
kubectl -n quoin scale deployment/quoin --replicas=1
```

`bootstrap.yaml` runs the existing `quoin secrets bootstrap --config /etc/quoin/component.yaml --kubernetes-secret quoin-secrets` command. It mounts the **same** `quoin-data` and `quoin-backups` PVCs and an `emptyDir` at `/run/quoin-secrets`; its root init container performs `chown 65532:65532 /run/quoin-secrets && chmod 0700 /run/quoin-secrets`, then the Quoin container runs as UID 65532 with every capability dropped. Its ServiceAccount can only `get` and `update` the fixed `quoin-secrets` Secret. `admin.yaml` starts `quoin admin create` directly and requires the attached terminal prompts. Do not put credentials on a command line, redirect the TTY, or collect its output in logs.

The bootstrap and administrator Pods are intentionally ordinary operational manifests rather than another generated deployment format. They must mount the retained Quoin PVCs: creating an administrator on a disposable filesystem and then starting against a fresh PVC loses the administrator. Back up the generated `quoin-secrets` using the approved secret backup process; it contains the root key and Runtime identity material.

## Compose

[`deploy/compose.yaml`](../deploy/compose.yaml) provides the same six roles for a local or auxiliary environment. It deliberately uses direct files and named volumes, not the retired deployment renderer. It reads paths relative to the Compose file, so run it from the repository root with `docker compose -f deploy/compose.yaml …` or copy the file with its sibling [`deploy/config`](../deploy/config) directory. Set `deploy/config/quoin.yaml`'s `publicOrigin` to the public `https://` address; the runtime endpoint in Plinth, Lintel and Stele remains `https://quoin:8443`.

Create the private input directories and public TLS files before the first start:

```bash
mkdir -p deploy/secrets/gateway deploy/secrets/quoin
chmod 700 deploy/secrets deploy/secrets/gateway deploy/secrets/quoin
# Quoin's non-root UID must own the mutable bootstrap directory.
sudo chown 65532:65532 deploy/secrets/quoin
# Copy the deployer-issued public certificate and private key without changing names.
install -m 640 /secure/path/tls.crt deploy/secrets/gateway/tls.crt
install -m 640 /secure/path/tls.key deploy/secrets/gateway/tls.key
# The stock gateway runs as UID 1000; grant it read access without widening to world.
sudo chown -R 1000:1000 deploy/secrets/gateway
```

Bootstrap secrets against the persistent Compose volumes, then interactively create the first administrator. Only the one-off bootstrap command overrides the secret mount as writable; normal services keep it read-only. Run these commands from the repository root (or adjust the absolute bootstrap mount to your copied directory). They do not print secret contents, and `admin create` requires an attached TTY:

```bash
docker compose -f deploy/compose.yaml run --rm --no-deps \
  -v "$PWD/deploy/secrets/quoin:/run/quoin-secrets:rw" quoin \
  secrets bootstrap --config /etc/quoin/component.yaml
docker compose -f deploy/compose.yaml run --rm --no-deps quoin \
  admin create --config /etc/quoin/component.yaml
docker compose -f deploy/compose.yaml up -d
```

The only host-published port is gateway `443`; application, runtime, webhook, and operational ports remain internal. Send Alertmanager-compatible requests to `https://<origin>/stele/` with the configured bearer. Persistent named volumes preserve Quoin data/backups and Plinth/Lintel state across container replacement. Caddy state volumes contain only gateway runtime state and do not contain frontend assets or application credentials.

## Validation

Validate syntax without changing a cluster:

```bash
docker compose -f deploy/compose.yaml config >/dev/null
kubectl apply --dry-run=client -f deploy/kubernetes/quoin.yaml
```

For a live qualification, always create a disposable namespace and explicitly inspect the current Kubernetes context first. Do not apply this manifest to an existing workload namespace as a validation shortcut.
