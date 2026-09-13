# 干净环境真实故障验收（2026-09-13）

## 环境和验收边界

- 平台：`quoin-fault-lab-20260913`，入口 <https://192.168.1.200:30447/>。
- 业务：`mall-fault-lab-20260913`，独立 Java/MySQL/Redis 和新数据卷。
- 初始化采用支持的 API，不把 API 操作称为 GUI 点击验收。本次记录验证真实故障、告警链路和模型诊断，不复用旧 GUI 验收作为证据。
- 平台权威数据库重新初始化，初始仅 1 个管理员，业务、连接、告警、报告、调查和迁移账本均为空；新密钥、CA 和运行时配对凭据。
- 使用独立 Prometheus、Alertmanager、namespace 只读 kube-state-metrics 和已探测启用的真实模型 `deepseek-v4-flash`。
- 用户明确批准删除旧 `quoin-gui-mall-20260910`、`quoin-e2e-mall-20260910` 及对应运行数据；保留历史备份、源码、证据和原始 `mall-lab`。
- 未重新开放产品 Kubernetes、知识库、浏览器巡检能力，未提交或推送代码。

## 方法

故障前七个 Prometheus 目标均为 `up=1`。故障必须由 Prometheus 规则生成，经 Alertmanager → Stele → Quoin 接收；没有手工 POST 伪造告警。模型分析通过支持的分析 API 发起。注入命令和预期答案单独留在操作者证据中，不放入模型可见告警注解。

模型输出的 `Succeeded` 仅表示执行完成。另行检查证据 ID 对应的真实 `thanos_query` 参数、返回值、时间和资源身份，判断诊断是否正确。

## nginx VM 停机

- 实际目标：专用 VM `quoin-lab-nginx`，`192.168.140.50`。
- 执行 `virsh shutdown` 后确认虚拟机关闭。
- 真实规则：node/nginx 两个 exporter 同时 `up=0` 持续 45 秒。
- occurrence **3**，analysis **1**，attempt **13**，证据 **4–13**，首见 `04:49:22Z`。
- 实际采证：host/nginx `up=0`；Java/MySQL/Redis `up=1`；VM 内资源指标无样本。
- 模型定位到同一 VM 的监测失联，提出主机或网络共同原因，明确没有 hypervisor 遥测，不能断言精确关机。
- 判定：**故障定位有条件通过，精确关机根因未确认**。输出没有虚构关机证据，但不能把这种合理不确定性包装成精确根因成功。
- 已执行 `virsh start`，七目标恢复，occurrence 在 `04:57:22Z` Resolved。

证据目录：`.artifacts/fault-lab-20260913/evidence/nginx-host/`，含首份输出、10 条实际工具证据、恢复和判定。

## Java 首次 OOM：发现诊断错误

- Pod：`mall-tiny-55b4658f66-6hr4j`，UID `f2d52609-6231-497c-9858-10617cf71fe7`。
- 容器：`mall-tiny`；节点 `suknna-homelab`（`192.168.1.200`），与 nginx VM 不同。
- 在节点无 MemoryPressure 时，使用 Docker 对这个既有隔离容器将实际 cgroup 内存上限临时降至 128MiB；没有 SIGKILL，也没有向节点灌入大量内存。
- Kubernetes 确认 `OOMKilled`、退出码 137、`05:00:44Z` 终止，重启计数从 0 增到 1。
- Pod 期望配置始终保留 limit 1536MiB/request 768MiB。旧容器已停止，因此恢复旧容器限制命令返回失败；Kubernetes 自动创建新容器，实际运行时核对为 1536MiB，随后 Ready。
- 规则限定 `container="mall-tiny"`，同时要求 OOM 原因、新近终止时间（300 秒）和重启增长，避免其它容器或粘滞 OOM 原因误报。
- occurrence **4**，analysis **2**，attempt **32**，证据 **14–35**，首见 `05:02:01Z`。
- 模型正确确认一次近期 OOM、重启和恢复，但错误地把 nginx VM 的内存及启动时间归给 Java 节点，进一步提出了无关主机重启关联。
- 判定：**未通过**。22 条工具证据完整并不能消除模型跨主机错误关联。

## 修正与复测

业务声明版本 **2** 明确区分 nginx VM 与 Java 节点，并说明 KSM resource_limits 是期望规格，不代表故障时实际运行时上限。指标权限不扩大，告警不添加注入答案；优先修正真实的资源上下文缺口，而不是直接给模型答案。

首次告警 Resolved 后再次对相同隔离 Java 容器执行同样有界注入。`05:05:48Z` 再次确认 OOMKilled，重启计数从 1 增至 2；Pod 随后已 Ready。新发生的告警使用新版声明，旧输出与首次归属保持不变。

复测材料在 `.artifacts/fault-lab-20260913/evidence/java-oom-retest/`。最终判定以该目录的 `verdict.yaml` 和下方执行结果为准。

## 最终执行结果与未通过项

第二次注入产生 occurrence **5**，analysis **3**，attempt **39**（证据 36–57）。拓扑误关联消失，但输出中途截断，且错误声称即时查询不能检查历史变化，判定仍未通过。

随后通过支持的连接轮换流程将模型输出上限从 4096 增至 8192，重新探测通过（probe result 3）并启用。业务声明版本 **3** 补充 `increase(...[10m])` 的合法历史查询示例。对同一真实 occurrence 5 创建 analysis **4**、attempt **47**，没有再次注入或制造新告警。

最终 18 条实际工具证据（58–75）均完整。模型确实执行 `increase(kube_pod_container_status_restarts_total[10m])`，返回约 1.026，正确将其解释为近 10 分钟约一次重启；确认 OOM 时间、当前运行状态、不同主机的身份边界，并区分规则时间窗消解和根因修复。完整输出及判定保存在 `.artifacts/fault-lab-20260913/evidence/java-oom-final/`。

**本轮只能判为故障事件定位部分通过，不能宣称精确根因全部通过。** 模型没有实际运行时 cgroup 限制变化或 hypervisor 状态，不能识别精确的“临时降至 128MiB”和“VM 关闭”诱因。最终输出仍有两个不够准确的表述：确认 OOM 后仍泛称不能证明任何目标真实故障；把带 namespace/container 过滤后查不到的 `up` 误述为 KSM job 不暴露该序列。前者应区分已确认容器终止与未量化业务影响，后者应解释标签范围，而不是否认采集器本身的健康序列。

精确根因验收受模型可见证据限制：需要真实、连续的 hypervisor 和容器运行时配置／OOM 遥测，不能把操作者注入记录转换成预期答案冒充独立监控证据。本轮没有新开这些基础设施权限或伪造这类指标，也没有将未通过项改判成功。

最终恢复检查：occurrence 3、4、5 全部 Resolved；七个 scrape 目标全部 up=1；nginx VM 运行；Java Pod Ready、累计重启 2 次，Docker 实际内存上限回到 1536MiB。第二次 OOM 告警在 `05:11:02Z` 恢复。节点 MemoryPressure=False。详见 `.artifacts/fault-lab-20260913/evidence/final-recovery.yaml`。

## 运维边界

- Prometheus 在线 `promtool check config` 与 3 条规则校验通过。
- libvirt 仅允许当前专用 Prometheus Pod `10.42.0.110/32` 到 nginx VM `192.168.140.50` 的 TCP 9100/9113，注释 `quoin-fault-lab-20260913-monitoring`；本轮创建的过期 `10.42.0.109/32` 规则已删除。
- 该规则依赖 Pod IP，Pod 重建后需要重新核对并替换；不是已经实现的自动网络恢复能力。没有开放整个 Pod 网段，也没有修改共享默认拒绝规则。
- 凭据只保留在私有 artifacts 下，不在本报告中展示。
- 运行时注入不更改 Deployment 的内存规格，不把重启后的 1536MiB 误当作注入瞬间 128MiB。

官方依据：<https://kubernetes.io/docs/tasks/configure-pod-container/assign-memory-resource/>。本轮复用已有 Docker/Kubernetes 能力和平台声明／指标工具，没有引入新的故障注入框架。
