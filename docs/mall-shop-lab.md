# mall-shop 本地业务演练

## 1. 分工与演练边界

本演练分三段：

1. 先准备 mall 商城业务与 Prometheus、Alertmanager，确保业务可访问、指标可采集、规则能告警。
2. 提供 Quoin [构建部署手册](getting-started.md)与[使用手册](user-guide.md)。
3. 由首次使用者亲自构建部署 Quoin、完成初始化并接入上述来源。本次不替用户创建 Quoin 管理员、注册运行时或保存来源凭据。

业务标识按需求为 **`mall-shop`**。在 Prometheus 指标、告警与 Quoin 业务视图中统一使用标签 **`system_id="mall-shop"`**；用户描述中的 `system-id` 是业务标识，不要求把带连字符的字段名写为 PromQL 标签。

商城使用现有缓存的 `macrozheng/mall-tiny` Java 17 应用（镜像 `local/mall-tiny:3.x-a97360e`）。它是用于真实 Java/MySQL/Redis 依赖演练的商城精简实现，**不是完整 mall 微服务套件，也不是带完整顾客购物前台的生产商城**。Nginx 运行在真实 libvirt 虚拟机，不用容器冒充虚拟机。

```text
访问者 → Nginx 虚拟机 → mall-tiny Java Pod
                                ├─ MySQL Pod（保留数据卷）
                                └─ Redis Pod（保留数据卷）

Prometheus → JVM / node / nginx / mysql / redis exporters
     │
     └─ mall-shop 告警规则 → Alertmanager → 本地演练通知
                                               │
                              用户安装 Quoin 后另配置 Stele receiver
```

MySQL、Redis 位于 Kubernetes 并不要求 Quoin 使用 Kubernetes 来源插件：当前主线通过 Prometheus 观察 exporter 指标，通过 Alertmanager 接收告警。浏览器与 Kubernetes 插件计划后续接入；本演练使用当前可用的 Prometheus 和 Alertmanager 插件即可。

## 2. 数据与网络边界

本机已有 `mall-lab` 与 `quoin-lab` namespace、MySQL/Redis/Prometheus/Alertmanager 持久卷及 `quoin-lab-nginx` 虚拟机。本次复用并保留这些数据，不初始化或删除旧数据库，不删除 PVC/PV，不启动历史 Quoin 实例。

`.local-lab/` 为本机私有实验目录，包含缓存、配置和秘密，不进入 Git。它不是干净克隆仓库就自动拥有的内容。仓库中的文档与演练脚本必须区分“恢复本机已准备环境”与“在全新机器上供应虚拟机、镜像和数据”；本次交付目标是前者。

所有演练端口仅限可信本机/局域网。Prometheus、Alertmanager 与商城演示接口不应直接暴露公网。Quoin 后续的 HTTPS、验证码投递和来源令牌是独立安全边界，不复用历史接入凭据。

## 3. 本机已验证业务入口

| 服务 | 地址或位置 | 验证 |
| --- | --- | --- |
| Nginx 虚拟机商城入口 | <http://192.168.140.50/> | libvirt VM `quoin-lab-nginx` 运行中 |
| 经 Nginx 的 Java 健康检查 | <http://192.168.140.50/actuator/health> | HTTP 200，JSON `status: UP` |
| Java NodePort 直连 | <http://192.168.1.200:30081/actuator/health> | HTTP 200，JSON `status: UP` |
| Java API 文档 | <http://192.168.1.200:30081/swagger-ui/index.html> | HTTP 200 |
| MySQL | `mall-lab` 中的 `mall-mysql-0` | Running / Ready，ClusterIP `10.43.211.10:3306` |
| Redis | `mall-lab` 中的 `mall-redis-0` | Running / Ready，ClusterIP `10.43.211.20:6379` |

这些地址属于本次机器环境，不适用于所有安装；ClusterIP 不能假定从任意电脑可达。虚拟机 NAT 地址通常供宿主机访问；局域网另一台电脑可先使用宿主 NodePort。访问商城业务接口需要商城自身登录，**不是 Quoin 的 `admin/admin`**。本机账号保存在 `.local-lab/secrets/mall-lab.env`（权限 `0600`）的 `LAB_ADMIN_USER` / `LAB_ADMIN_PASSWORD`，请仅在本地查看，不提交或粘贴到工单。通过 `POST /admin/login` 登录后携带返回令牌访问 `/admin/info`、`/admin/list`。本次已真实验证登录、个人信息和管理员列表读取成功（列表共 7 条），证明应用可访问保留的 MySQL 数据。

此 pinned mall-tiny 版本没有 `/brand` 模块，不要套用其他 mall 教程的 `/brand/listAll`。未登录请求即使 HTTP 200，也可能是业务 `code: 401`，不能当作业务查询成功。

复验命令：

```bash
kubectl -n mall-lab get pods,svc
virsh -c qemu:///system list --all
curl --fail --max-time 15 http://192.168.140.50/actuator/health
curl --fail --max-time 15 http://192.168.1.200:30081/actuator/health
```

## 4. 监控入口与来源参数

| 服务 | 本机可用地址 | 说明 |
| --- | --- | --- |
| Prometheus | <http://192.168.1.200:30090/>；集群地址 <http://10.43.19.39:9090/> | `quoin-lab` namespace，保留 TSDB 卷 |
| Alertmanager | <http://10.43.100.206:9093/> | `quoin-lab` namespace，保留状态卷 |
| 本地通知接收器 | `http://192.168.1.200:19094/alerts` | 只用于演练取证，不是 Quoin 地址，不是生产通知渠道 |

Prometheus 目标已实测 **6/6 UP**：`prometheus-self`、`vm-node`、`vm-nginx`、`mall-tiny-jmx`、`mall-mysql-exporter`、`mall-redis-exporter`，都带 `system_id=mall-shop`。Prometheus 自监控也盖此演练标签；如果只看业务依赖，查询时加 `job!="prometheus-self"`。

本次已从独立 Docker 容器实测 Prometheus 的 `http://192.168.1.200:30090` 和两个监控 ClusterIP 均可达。**首次创建 Quoin Prometheus 接入推荐填写 `http://192.168.1.200:30090`，认证模式为 `none`**；这是本机私网演练配置，不是生产安全配置。NodePort 默认可能在所有节点接口暴露，并非天然仅绑定 LAN，须由宿主/边界防火墙限制到可信网络。

在宿主机浏览器也可直接使用上述 ClusterIP。其他机器不能假设 ClusterIP 可达；可在本机终端临时建立回环转发：

```bash
kubectl -n quoin-lab port-forward svc/prometheus 19090:9090
# 另一终端；Ctrl-C 只停止转发，不停止监控服务
kubectl -n quoin-lab port-forward svc/alertmanager 19093:9093
```

然后打开 <http://127.0.0.1:19090/> 和 <http://127.0.0.1:19093/>。**这两个 localhost 地址不能直接填入运行在容器中的 Quoin**。Quoin 指标访问发生在 Plinth 运行环境，须从该容器验证到 ClusterIP 的路由；如无法访问，可受限发布宿主 LAN 转发并使用宿主 LAN IP，不要对公网开放无认证监控端口。接入表单的真实 probe 是最终网络可达性验证。

## 5. 告警规则与实际演练结果

规则的仓库版本见 [`scripts/mall-shop/prometheus-rules.yaml`](../scripts/mall-shop/prometheus-rules.yaml)。当前六条规则：

| 规则 | 检查 |
| --- | --- |
| `MallShopVmTargetDown` | VM node/nginx exporter 的采集失败 |
| `MallShopNginxDown` | exporter 可达但 `nginx_up=0` |
| `MallShopMiddlewareTargetDown` | MySQL/Redis exporter 的采集失败 |
| `MallShopAppTargetDown` | Java JMX exporter 采集失败 |
| `MallShopMySQLUnavailable` | `mysql_up=0` |
| `MallShopRedisUnavailable` | `redis_up=0` |

每 15 秒评估，异常持续 40 秒后 firing。它们覆盖本次依赖可用性，不是完整下单成功率、订单延迟或数据库容量 SLO；目前没有可据实定义这些业务 SLO 的应用指标。

**2026-09-16 实测：** 将 `mall-redis-exporter` 临时缩为 0（只停止指标采集器，Redis 数据服务保持运行），观察真实 `up=0` 告警，随后恢复 1 副本：

- 本地接收器于 **11:08:43 +08:00** 收到 `MallShopMiddlewareTargetDown` 的 **firing** 通知。
- 于 **11:09:43 +08:00** 收到同规则的 **resolved** 通知。
- 最终 6/6 目标 `up=1`，`mysql_up=1`、`redis_up=1`、`nginx_up=1`；六条规则均 `health=ok`、`inactive`。

通知原始记录位于本机 `.artifacts/mall-shop-20260916/alertsink/deliveries.jsonl`，包含告警 payload，不提交 Git。**本次验证了 Prometheus → Alertmanager → 本地 HTTP 接收器，不是短信/邮件送达，也不是尚未安装的 Quoin 收到告警。**

可复验脚本为 [`scripts/mall-shop/alert-drill.sh`](../scripts/mall-shop/alert-drill.sh)，它会短暂中断 Redis 指标采集，不要在依赖该指标的其他重要操作期间运行。操作前先阅读脚本及检查目标 namespace。

```bash
bash scripts/mall-shop/alert-drill.sh
```

## 6. 本机恢复、停止与网络注意事项

以下命令依赖**本机保留的 `.local-lab`**，不是全新机器安装器。不执行删除 namespace/PVC/PV 或重新初始化数据库。

```bash
# 恢复本机业务：检查虚拟网络/VM状态后，仅在未运行时启动
virsh -c qemu:///system net-list --all
virsh -c qemu:///system list --all
# 未激活时才执行：virsh -c qemu:///system net-start quoin-lab
# VM关闭时才执行：virsh -c qemu:///system start quoin-lab-nginx
bash .local-lab/config/kubernetes/mall-lab/apply.sh
bash scripts/mall-shop/alert-sink.sh up
bash scripts/mall-shop/deploy-monitoring.sh
```

监控配置源分别为 `.local-lab/config/prometheus/prometheus.yml`、`.local-lab/config/alertmanager/alertmanager.yml`；Kubernetes 清单在各自 `k8s/` 子目录。规则源在仓库 `scripts/mall-shop/prometheus-rules.yaml`。`deploy-monitoring.sh` 会应用监控配置并维护本次带注释的 VM 采集防火墙规则，不修改其他防火墙规则。

本机透明代理与 libvirt 需要额外网络例外：

- 本轮恢复了优先级 **8000–8003** 的策略路由：集群目标 `10.43.0.0/16`、`10.42.0.0/16` 及 Pod 到这两个 CIDR 的路由使用 `main`，避免被代理 TUN 接管。它们是运行时规则，重启后可能消失。不要在不了解当前规则时盲目重复添加或删除；先看 `ip rule show`、路由和实际连接结果。
- `LIBVIRT_FWI` 放行**当前 Prometheus Pod IP**到 `192.168.140.0/24` 的 TCP `9100,9113`，注释为 `mall-shop-prometheus-to-vm`。Pod 重建换 IP 后需要重新执行监控部署脚本刷新；若 VM 两个 target down 而宿主可抓取，先查这里，而不是重装 VM。
- 精确回滚防火墙规则的参数见脚本，不删除 libvirt 默认链。策略路由撤销会影响本机集群网络，只有停用本实验并确认无其他集群依赖时才评估撤销。

若需暂时停止演练，先确认不再依赖监控和业务：

```bash
kubectl -n mall-lab scale deployment mall-tiny mall-mysql-exporter mall-redis-exporter --replicas=0
kubectl -n mall-lab scale statefulset mall-mysql mall-redis --replicas=0
kubectl -n quoin-lab scale statefulset prometheus alertmanager --replicas=0
virsh -c qemu:///system shutdown quoin-lab-nginx
bash scripts/mall-shop/alert-sink.sh down
```

这些命令保留数据卷；停止业务时可能产生告警。当前交付**未停止**，环境保持运行供你接入。

## 7. Quoin 接入时的成功标准

按[使用手册](user-guide.md)完成以下人工验收：

- Prometheus 接入真实验证成功并启用，可以观察 `mall-shop` 目标。
- 查询 `up{system_id="mall-shop"}` 返回当前采集结果；exporter 的 `up=1` 仅证明 exporter 可抓取，还需查看 `mysql_up`、`redis_up`、`nginx_up` 等依赖指标。
- Alertmanager 的新 receiver 使用 Quoin 本次创建的告警源凭据，而非 Stele 内部 service token；发出和恢复通知都应到达同一来源。
- 创建可选业务视图，标签范围为 `system_id: mall-shop`；不将业务标签误认为租户权限隔离。
- 运行一次巡检，查看 Evidence 与实际窗口；`Completed` 不等于健康，缺数据不等于零。
- 配置并验证模型供应商后，检查分析报告是否引用本次真实证据；没有模型配置时不宣称 AI 报告链路已通过。

本次构建与测试结果见[验证记录](acceptance/mall-shop-handbook-20260916.md)。
