# 真实本地故障注入演练就绪度调研（nginx VM 停机 + Java Pod OOMKilled）

- **调研日期**：2026-09-13
- **Quoin 基线**：`a1773469392b9c2eea59d9108eb43c881e3c1f38`（`Implement explicit metrics business intake and controlled verification (#97)`；工作区存在未提交修改，本调研只读，不依赖 dirty 内容）
- **方法与边界**：纯静态阅读仓库源码、`.local-lab` 配置/记录、GitHub Issues（`gh` 只读）与官方文档；**未执行任何运行时命令**（无 kubectl / virsh / curl / 部署脚本），**未读取任何秘密文件**（`.env`、`.local-lab/secrets/`）。文中"已具备"仅指源码/配置层面可复用，不代表当前集群/VM 实际状态；所有实际状态须在执行轮用只读命令现勘。
- **本文件性质**：调研笔记（recon）。不修改任何配置、不发布声明、不触发部署。

## 执行后纠正（2026-09-13）

本文件以下内容是执行前静态调研，不应再用作当前运行状态判据。实际执行记录见 [故障验收报告](fault-injection-validation-20260913.md)。

- 真实模型已经通过探测并启用；文中沿用旧验收的“模型阻塞”不是本轮事实。
- 已部署独立平台与商城 namespace、全新数据卷、独立 Prometheus/Alertmanager 和 namespace 只读 kube-state-metrics；不再存在容器终止指标缺口。
- 用户另行明确批准删除此前两个验收 namespace 及对应运行数据；原始 mall-lab、共享集群和历史备份证据保留。
- 当前业务声明不需要任何独立 Label Contract，文中相关建议已过时。
- `OOMKilled` 证明 OOM 终止，单靠它不能区分容器限制和节点级压力。KSM resource_limits 是期望规格，不一定等于故障时的实际 cgroup 限制。
- nginx VM（192.168.140.50）与 Java Kubernetes 节点（suknna-homelab，192.168.1.200）是不同主机，不能关联其内存或启动时间。
- 抓取失败通常产生 `up=0`；目标移除或无该标签系列才可能为空，不能把空值简单当作网络故障。

## 0. 批准目标拆解

1. **干净隔离的重部署**：在不动现有 `mall-lab` 与 `local-inspection-demo` v3 的前提下，新建 namespace + 独立 PVC 重部署可复现的 Java 业务栈。
2. **nginx VM 停机注入**：对专用 VM `quoin-lab-nginx` 执行真实 shutdown（既有先例），并区分"整机停机 / 仅 nginx 进程停止 / 遥测链路缺失"。
3. **受控 Java Pod OOMKilled**：让容器真实触碰 cgroup 内存上限、被内核 OOM killer 杀死（`reason=OOMKilled`），而非伪造告警或仅看 exit 137。
4. **全链路判定**：Prometheus 规则 → Alertmanager → Stele → Quoin Occurrence → 初步分析（模型结论），对模型结论做对错判定；结论错误时修复产品侧（提示词/工具/声明），而不是放宽判定。

## 1. 现状资产地图（全部可直接复用的精确路径）

### 1.1 lab 业务栈（k3s，namespace `mall-lab`）

| 资产 | 路径 | 说明 |
| --- | --- | --- |
| 部署入口（幂等） | `/home/suknna/code/quoin/.local-lab/config/kubernetes/mall-lab/apply.sh` | 全栈依赖序部署；secrets 存在性/权限 guard；`provision-labadmin.sh` 集成 |
| Java 应用清单 | `/home/suknna/code/quoin/.local-lab/config/kubernetes/mall-lab/05-app.yaml` | Deployment `mall-tiny`；ConfigMap 外置配置 + JMX exporter；探针/资源均在其中 |
| 存储清单 | `/home/suknna/code/quoin/.local-lab/config/kubernetes/mall-lab/02-pv-pvc.yaml` | PV `mall-lab-data-mysql`(8Gi)/`mall-lab-data-redis`(2Gi)，hostPath `data/mysql`、`data/redis`，**reclaimPolicy: Retain**，storageClass `mall-lab-local` |
| MySQL/Redis/exporters | 同目录 `03-mysql.yaml` `04-redis.yaml` `06-exporters.yaml` | 全部 pinned ClusterIP 10.43.211.10/.20/.32/.33，NodePort 仅 app 30081 |
| 镜像构建 | `/home/suknna/code/quoin/.local-lab/config/docker/mall-tiny/Dockerfile` | **ENTRYPOINT `java -XX:MaxRAMPercentage=75.0` + jmx javaagent**；上游 jar 不修改 |
| 离线缓存 | `.local-lab/cache/images/`（含 `SHA256SUMS`）、`cache/maven/`、`cache/artifacts/`、`sources/mall-tiny`（pinned commit，见 `sources/mall-tiny/PINNED.md`） | 全离线重建能力已被 `.local-lab/STATUS.md` 记录验证 |
| 运行记录 | `/home/suknna/code/quoin/.local-lab/README.md`、`STATUS.md`、`RECORDS.md` | 端点、加固、持久化重启验证（含"pod 重建后健康自动恢复"证据 `logs/persistence-restart-20260908.log`） |

### 1.2 nginx VM（libvirt，VM 名 `quoin-lab-nginx`）

| 资产 | 路径 | 说明 |
| --- | --- | --- |
| 声明唯一权威 | `/home/suknna/code/quoin/.local-lab/config/vm/lab.yaml` | VM `quoin-lab-nginx`，IP `192.168.140.50`，1 vCPU/1GiB；`proxy_upstream: http://192.168.1.200:30081`；端口 80/9100/9113；node_exporter 1.9.1 + nginx-prometheus-exporter 1.4.2 |
| 生成器 | `/home/suknna/code/quoin/.local-lab/config/vm/scripts/generate.py` | 由 `lab.yaml` 生成 `.local-lab/libvirt/domain.xml`、`net.xml`、cloud-init seed（`libvirt/disks/seed.iso`、`root.qcow2` backing）；不承担 VM 启停 |
| nginx 站点声明 | `/home/suknna/code/quoin/.local-lab/config/vm/nginx.yaml` | 代理 server + 仅回环 `127.0.0.1:8081/stub_status` |
| 启停命令先例 | `/home/suknna/code/quoin/.local-lab/evidence/manual-acceptance/20260908-2054/report.md` §5（约 138–152 行） | **注入**：`virsh -c qemu:///system shutdown quoin-lab-nginx`；**恢复**：`virsh -c qemu:///system start quoin-lab-nginx`；已验证该真实故障可进入 Quoin（M12 通过） |
| 首次置备辅助 | `.local-lab/config/vm/scripts/wait-provisioned.sh`、`serve-cache.sh` | 全离线首次置备（gateway 缓存 HTTP） |

### 1.3 监控与告警链

| 环节 | 路径 | 要点 |
| --- | --- | --- |
| Prometheus 配置源 | `/home/suknna/code/quoin/.local-lab/config/prometheus/prometheus.yml` | 6 个 job：self / `vm-node`(192.168.140.50:9100) / `vm-nginx`(:9113) / `mall-mysql-exporter` / `mall-redis-exporter` / `mall-tiny-jmx`；每 job 盖章 `system_id=local-inspection-demo, data_origin=local_live`；**无 kube-state-metrics job** |
| Alertmanager 静态路由 | 同文件 29–32 行 | `alerting.alertmanagers` 指向 pinned ClusterIP `10.43.159.202:9093`（svc `local-inspection-alertmanager`） |
| k8s 化 Prometheus | `.local-lab/config/prometheus/k8s/`（`30-configmap.yaml` 由 `apply.sh` 从 `prometheus.yml` 重新生成） | namespace `quoin-lab`，StatefulSet `prometheus-0` |
| Alertmanager 栈 | `.local-lab/config/manual-alert-acceptance/alertmanager-stack.yaml` + `deploy-alertmanager.sh` | v0.28.1 digest-pinned；webhook → `http://10.43.56.123:8080`（quoin-stele），`send_resolved: true`，Bearer 走 `credentials_file`（凭据只在受限目录，见脚本 guard） |
| Prometheus 路由/规则热改 | `.local-lab/config/manual-alert-acceptance/prepare-prometheus-routing.sh` + `prometheus-rules.yaml` + `prometheus-rules-mount.yaml` | 备份→校验→单条 scope 规则（`LocalInspectionDemoTargetDown`，`up{job="vm-nginx",instance="192.168.140.50:9113"}==0 for 1m`）→reload |
| 声明导入脚本 | `/home/suknna/code/quoin/.local-lab/bin/lab-monitoring-import.py` | label contract + business system 上传、Thanos 连接 + 真实 probe（`vector(1)`）、配置验证与发布；阈值门禁式停止 |
| 巡检/声明源 | `.local-lab/config/inspections/business-system.yaml`、`label-contract.yaml` | 见 §3 证据缺口 |

### 1.4 产品内链路（源码）

| 环节 | 位置 | 证据 |
| --- | --- | --- |
| Stele webhook 接入 | `/home/suknna/code/quoin/internal/stele/webhook.go` | Bearer 对抗缓存快照认证；**204 只在 Quoin 提交后返回**，4xx 永久拒绝，5xx/503 可重试 |
| 归属判定 | `/home/suknna/code/quoin/internal/quoin/alerts/attribution.go` | 首个不可变 delivery item 时一次判定；不依赖活动全局 Label Contract（`loadAttribution` 已返回空 index，声明内告警标签为准） |
| 初步分析 API | `/home/suknna/code/quoin/internal/quoin/app/analysis_http.go:42-45` | `GET/POST /api/v1/alerts/{occurrenceId}/analyses...`（retry/cancel），SSE 在 `analysis_events.go` |
| 指标工具边界 | `/home/suknna/code/quoin/internal/quoin/config/businesssystem.go:221,266-272` | 查询指标必须命中资源 `allowedMetrics`（精确名或非空前缀通配）；`discoveryMetric` 必须在白名单内 |
| Kubernetes 观察工具 | `/home/suknna/code/quoin/internal/quoin/tools/kubernetes/kubernetes.go` | 仅 `kubernetes_read` 固定观察工具的 BusinessSystem 路由；连接型 Kubernetes 接入本身属于 #100（未开工） |

## 2. OOM 的"证明"与 exit 137 的区别（官方依据）

- **exit 137 = 128 + SIGKILL(9)**：只证明进程收到 SIGKILL，不证明原因。kubelet 在 liveness 探针失败宽限后、手动 delete pod、节点驱逐等场景同样以 SIGKILL 结束容器，退出码同为 137。因此"看到 137"不足以当 OOM 证据。
- **权威判据**：Kubernetes 官方资源管理文档明确：容器超过内存 limit 时由内核 OOM 子系统终止；`kubectl describe pod` 显示 `Reason: OOMKilled`、`Exit Code: 137` 且 Restart Count 增长即"容器超内存 limit"的证据。
  引用：<https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/>（"Memory limit: exceeding it triggers the kernel OOM subsystem…"；Troubleshooting 段 "`Reason: OOMKilled` and `Exit Code: 137` with an increasing Restart Count"）。
- **可被 Quoin 采集的权威指标**（kube-state-metrics，官方指标文档）：
  - `kube_pod_container_status_last_terminated_reason`（Gauge，EXPERIMENTAL；labels `namespace,pod,container,reason,uid`；help "Describes the last reason the container was in terminated state"）——`reason="OOMKilled"` 即 OOM 证据；
  - `kube_pod_container_status_restarts_total`（Counter，STABLE）——用于把 OOMKilled 绑定到"新近发生"。
  引用：<https://github.com/kubernetes/kube-state-metrics/blob/main/docs/metrics/workload/pod-metrics.md>
- **持续性陷阱**：`last_terminated_reason` 在容器以其它原因再次终止前**保持** `reason="OOMKilled" == 1`。单条件告警会常亮。必须与新鲜重启绑定，建议表达式（示意，正式值执行轮校准）：
  ```promql
  increase(kube_pod_container_status_restarts_total{namespace="<fault-ns>",container="mall-tiny"}[5m]) > 0
    and on(namespace,pod,container)
  kube_pod_container_status_last_terminated_reason{namespace="<fault-ns>",container="mall-tiny",reason="OOMKilled"} == 1
  ```
- **JVM 语义边界**：本镜像以 `-XX:MaxRAMPercentage=75.0` 运行（`Dockerfile` ENTRYPOINT）。若仅 Java 堆内 OOM（`java.lang.OutOfMemoryError`），JVM 默认**不会**退出，容器不会 OOMKilled；真实 OOMKilled 必须是 RSS（堆+元空间+线程栈+javaagent）突破 **cgroup limit**。因此注入手段应是压低 `resources.limits.memory`（声明式、可回滚、不改 jar），而不是给应用灌堆。当前 limit 1536Mi（`05-app.yaml:196-202`），稳态 RSS 需执行轮先实测再定新 limit。

## 3. 证据缺口：当前 allowedMetrics / 采集清单不足以证明 OOM

- 磁盘上的巡检声明（`.local-lab/config/inspections/business-system.yaml:25-41`）资源发现仅覆盖：
  `up`（scrape-targets）、`mysql_up`、`redis_up`、`java_lang_threading_threadcount`（app-runtime）。目标态 BusinessSystem 声明的资源 `allowedMetrics` 白名单同源（指南示例与 Schema：`docs/business-system-declaration-guide.md:42`、`docs/specs/quoin-v1/contracts/schemas/business-system.schema.json:52-57`）。
- Prometheus **没有任何容器层指标**来源：无 kube-state-metrics job（`prometheus.yml` 全文核实）。JMX 指标（堆使用率 `java_lang_memory_heapmemoryusage_*`、GC、RSS）只能证明"堆逼近上限后目标消失"的**旁证**，不能证明 OOMKilled。
- 结论：OOM 证明必须走 **metrics ingestion** 补齐：
  1. 部署 kube-state-metrics（官方镜像 digest-pinned，按 lab 惯例入 `cache/images/` + `SHA256SUMS`；k3s 默认不自带）；
  2. `prometheus.yml` 增加 job 并盖 `system_id` 等同一标签组，`./apply.sh`（k8s 目录）重生成 ConfigMap；
  3. 发布更新版 BusinessSystem 声明：新增资源（matchLabels 如 `job=kube-state-metrics` + namespace 维度），`discoveryMetric` 与 `allowedMetrics` 显式包含 `kube_pod_container_status_restarts_total`、`kube_pod_container_status_last_terminated_reason`（保留既有 `up` 等）——否则模型即使想查也被 `businesssystem.go` 白名单拒绝（这是**正确的边界行为**，应先补声明再注入故障）。

## 4. nginx：整机停机 vs 仅 nginx 停止 vs 遥测缺失

| 场景 | 可观察信号（现有采集下） | 归属结论强度 |
| --- | --- | --- |
| VM 整机 shutdown（先例） | `up{job="vm-nginx"}` 与 `up{job="vm-node"}` 同时 ==0 | 证明"该 VM 一切信号消失"，但**不能区分**"VM 宕机"与"宿主→VM 路由/抓取链路中断" |
| VM 内 `systemctl stop nginx`（未做过，建议本轮补） | `nginx_up{job="vm-nginx"}==0`（exporter 仍活着并上报失败），`up{job="vm-node"}==1` | 干净归因：nginx 进程停了，主机没死 |
| 遥测链路缺失（如 exporter 停、路由断） | 目标系列变 **stale/absent**，而非 `up==0` | `sum(up)` 仍为 0 与 EMPTY 的区别已在巡检 plan 4 中显式建模（`business-system.yaml` cross-layer-overview：全部宕机 → 0；采集链路断 → 空结果） |

- 不确定性结论：仅凭 `up==0` 断言"主机宕机"超出指标可证范围；判定模型结论对错时应以上面三行的**信号组合**为标准答案，注入时同步记录 ground truth（时间、命令、virsh 状态只读快照）。
- 恢复路径已验证：同一 VM `virsh start` 后 `up=1`（acceptance report §5 21:40:22 安全检查）。

## 5. Kubernetes 产品门禁：不得静默重开 #100

- Kubernetes 接入（kubeconfig/API 凭据表单、真实探测、namespace 范围、AI SRE 只读工具贯通）是 **#100 [OPEN]「Kubernetes 接入、业务范围与真实工具查询」** 的封闭范围（Blocked by #97，已解除但未开工）。`CONTEXT.md:428` 亦规定 Kubernetes probe 只证明 discovery + pods get/list、events list、pods/log get 的 SelfSubjectAccessReview，**不外推其它权限**。
- 因此本目标的 OOM 证据**不得**通过给产品接 kubeconfig/kubectl 实现——那等于静默实施 #100。正确路径是把 kube-state-metrics 指标纳入既有 Prometheus/Thanos 连接与 BusinessSystem 声明（#97 已交付"显式指标业务接入与受控验证"），模型只经授权工具按声明查询。k8s 侧的注入操作（压低 limit、观测 OOMKilled ground truth）是**部署方操作**，发生在产品之外，不进产品权限面。

## 6. 隔离默认：新 namespace + 独立 PVC + 不 purge

- 依据：`CONTEXT.md:407`（每次 invocation 唯一 namespace、独立业务卷、teardown 机械归零）；`mall-lab/README.md` scope 声明（只动 namespace `mall-lab`，不碰 `quoin-t43-disp`/`kube-system`）；`02-pv-pvc.yaml` 的 **Retain** 策略。
- 建议默认：
  - 新 namespace（如 `mall-fault-lab`），全栈复制部署（mysql+redis+app+exporters），**不复用** mall-lab 的 PVC/PV（新 PV 名 + 新 hostPath 子目录，如 `.local-lab/data/fault-lab/{mysql,redis}`）；
  - 不 purge 任何现有数据：现有 PV 保持 Retain 原样，演练 teardown 只清 fault-lab 自有对象（namespace 删除即回收其 PVC；PV 按策略处理需显式决定 Retain/Delete）；
  - **冲突清单（当前清单全是硬编码，需参数化或复制改值）**：NodePort 30081 已被 mall-lab 占用（fault-lab 换端口，如 30082）；pinned ClusterIP 10.43.211.10/.20/.30/.31/.32/.33、Prometheus Alertmanager 10.43.159.202、stele webhook 10.43.56.123 均需重新分配并回填 hostAliases/抓取目标；VM nginx 的 `proxy_upstream` 指向 30081——若要 nginx 停机演练与 OOM 演练互不影响，fault-lab app 可不改 VM 代理（VM 故障演练按先例独立进行）。
  - 告警与抓取：fault-lab 的 series 若也盖 `system_id=local-inspection-demo`，会与现有 v3 声明混流；建议新 `system_id`（如 `fault-injection-lab`）+ 新 Label Contract/新 BusinessSystem 声明版本，判定边界清晰。

## 7. 全链路判定与"修错"落点

- 链路：Prometheus 规则（`for`/pending→firing 语义，官方：<https://prometheus.io/docs/prometheus/latest/configuration/alerting_rules/>；synthetic `ALERTS` 系列）→ Alertmanager（group_wait 5s、`send_resolved: true`，官方配置参考：<https://prometheus.io/docs/alerting/latest/configuration/>）→ Stele webhook（Bearer，204=已提交）→ Quoin Occurrence + 归属 → 初步分析（`analysis_http.go:42-45`）。
- 判定标准（执行轮预登记，避免事后解释）：
  - OOM 注入：模型结论必须落到 ground truth（namespace/pod/container、`OOMKilled`、时间窗、limit 被突破的指标链：restarts + last_terminated_reason + 伴随的 `up{job=...jmx}` 消失与堆爬升旁证）；把"只有 exit 137"或"只有堆曲线"当结论即判错。
  - nginx 注入：区分 §4 三场景；把 vm-nginx `up==0` 直接断言"主机宕机"而 vm-node 仍 `up==1` 的，判错。
- "结论错误时修复"的落点（只记录，不在本轮实施）：优先核对是否**声明缺口**（allowedMetrics 未含结论所需指标 → 补声明重发布），其次 Thanos 工具查询注入/标签（`internal/quoin/tools/thanos/`、`internal/plinth/agent/agent.go:42,202` 的 AllowedMetrics 注入），最后才是分析提示词；不得为通过判定而放宽 ground truth。
- 前置阻塞（承接 acceptance report M13）：**模型供应商未启用**则"模型结论"环节无法执行。`config/manual-acceptance/ai-gateway.compose.yaml` + `nginx.conf.template` 已备但从未成功运行（镜像拉取超时，report §2.1）；产品单模型 Base URL 约束见 report。执行轮需先解决真实模型可用性。

## 8. 可复用脚本/文件清单（执行轮入口）

| 用途 | 命令/文件 |
| --- | --- |
| mall 栈幂等重部署 | `.local-lab/config/kubernetes/mall-lab/apply.sh`（fault-lab 需参数化副本：namespace/ClusterIP/NodePort/PV 名/secrets env 文件） |
| Prometheus 配置重生成+应用 | `.local-lab/config/prometheus/k8s/apply.sh` |
| Alertmanager 部署（带凭据 guard） | `.local-lab/config/manual-alert-acceptance/deploy-alertmanager.sh` |
| 路由/规则受控变更（备份→校验→reload） | `.local-lab/config/manual-alert-acceptance/prepare-prometheus-routing.sh` |
| 声明/连接导入与 probe | `.local-lab/bin/lab-monitoring-import.py`（注意：读写 `.local-lab/secrets/quoin/admin-credentials.yaml`，仅限授权执行） |
| VM 工件再生成 | `.local-lab/config/vm/scripts/generate.py` |
| VM 注入/恢复 | `virsh -c qemu:///system shutdown|start quoin-lab-nginx`（先例：acceptance report §5） |
| 产品一次性集成环境（如需另起产品面） | `make e2e-real-up` / `scripts/e2e-real/{up,down}.sh`（Compose 面，**不是** Java OOM 的路径） |

## 9. 就绪度结论与缺口清单

**可判定：目标在现有资产上可达成，无需改产品代码即可搭出台面；但当前不具备 OOM 证明能力，且模型结论环节存在前置阻塞。**

缺口（按执行顺序）：
1. kube-state-metrics 部署 + Prometheus job + 声明 allowedMetrics 扩展（§3）——OOM 证明的唯一 ingestion 路径；
2. fault-lab 隔离部署参数化（§6 冲突清单）；执行前实测 mall-tiny 稳态 RSS 以校准 OOM 注入 limit；
3. 真实模型供应商可用（§7 前置阻塞）；
4. nginx 进程级停止的差异化演练从未做过（先例只有整机 shutdown），建议本轮补齐以支撑 §4 的三场景判定；
5. 判定标准与 ground truth 记录格式在注入前预登记（§7），模型结论对错才有客观裁判依据。

## 10. 引用

- Kubernetes，资源管理（OOM kill / `Reason: OOMKilled` + `Exit Code: 137`）：<https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/>
- kube-state-metrics 官方指标文档（`kube_pod_container_status_last_terminated_reason`、`kube_pod_container_status_restarts_total`）：<https://github.com/kubernetes/kube-state-metrics/blob/main/docs/metrics/workload/pod-metrics.md>
- Prometheus alerting rules（`for`/pending/firing/`ALERTS`）：<https://prometheus.io/docs/prometheus/latest/configuration/alerting_rules/>
- Alertmanager 配置参考（webhook/`send_resolved`）：<https://prometheus.io/docs/alerting/latest/configuration/>
- 仓库先例：`.local-lab/evidence/manual-acceptance/20260908-2054/report.md`（真实 VM 停机告警贯通 Quoin；模型分析被前置阻塞）
- 仓库规格：`CONTEXT.md:386`（Contract Gate / Release Qualification / Deployment Acceptance 分层）、`CONTEXT.md:404`（封闭故障原语，v1 不引入通用 Chaos 平台）、`CONTEXT.md:407`（invocation 级 namespace/卷隔离与归零）、`CONTEXT.md:428`（Kubernetes probe 固定边界）
- GitHub Issues（`gh` 只读核实）：#100 OPEN（Kubernetes 接入，封闭范围）、#99 OPEN（告警分析与 AI SRE 工具按声明采集）、#97 CLOSED（显式指标业务接入，已交付）；**本仓库不存在 issue/PR #137**——"vs137"按容器退出码 137 解释（§2）。
