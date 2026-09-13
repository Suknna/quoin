# 业务声明改造验证记录（2026-09-12）

## 状态

源码改造、自动化回归及 Deployment 替换已完成。**当前部署版本的三条业务流程已取得真实点击成功证据，完成核对时间为 2026-09-13。** 本文下方记录本轮证据；既有 `gui-e2e-acceptance.md` 仅记录此前版本。CoreDNS 与 Plinth DNS 解析问题已修复。浏览器使用新建独立标签及截图定位的真实坐标点击完成验收，没有通过 API 操作替代产品点击。

## 自动化验证

- `go test ./...`：通过，包含后补的定时备份连接池死锁修复。
- `go vet ./...`：通过。
- 前端全量测试：148 项通过；类型检查、lint、生产构建通过。
- 精确前驱 schema 从旧部署镜像嵌入 SQL 提取，SHA-256：`3a95baf7b2ecab6a5f81fe084334fe953a5c23a3a71db3de1cc1d8c0ee107eb2`。
- 迁移测试使用上述物理 schema，保留旧发布约束；验证历史原文/摘要、新版本映射与当前指针、单发现项下的 up/mysql_up/redis_up 权限、旧嵌套标签规则转换、歧义回滚。
- 额外回归：规范化查询执行参数与摘要、资源重复 UPSERT 的实际行 ID、维护隔离、重复告警规则发布拒绝、停用业务不参与归属、无契约的验证定位记录。
- 前端保持 Kubernetes/浏览器巡检开发中；手动资源刷新显示运行状态、失败原因及无匹配结果。

## 构建

镜像命名空间 `quoin-acceptance`。五组件重新构建为 `k8s-declaration-20260912-r2`；Quoin 另补精确前驱预检及历史告警源转换后构建为 `k8s-declaration-20260912-r3`。当前 Deployment：Quoin r3，frontend/plinth/lintel/stele r2，全部 Ready。Gateway 和独立商城 Alertmanager 未替换。未推送或提交 Git。

## 升级前旧环境故障与恢复

旧工作台停留在会话验证；Stele 凭据快照持续超时。无会话请求快速返回 401，私有 readyz/metrics 快速返回 200，而需要数据库的请求等待。

根因：`backup/scheduler.go` 在午夜排队备份后仍持有唯一 SQLite 连接，又通过数据库池刷新指标，形成自等待。真实数据中备份 #4 的 scheduled_for 为 `2026-09-12T00:00:00Z`，最初状态为 queued，与代码路径吻合。新增测试启用真实指标对象，修复前超时，修复后通过。

恢复前使用 SQLite backup API 保存私有一致恢复副本，完整性检查为 ok。副本仅包含数据库，不等同于正式升级所需的全套备份。随后只重启旧镜像 Quoin，未更换镜像、未修改数据、未删除任务。Quoin/Stele 恢复 Ready；备份 #4 实际恢复为 succeeded。源码修复将指标刷新移到释放连接之后。

## 实际升级记录

- 浏览器恢复后真实点击登录、备份与保留、立即备份，备份 #5 succeeded。
- 页面关于/维护执行准备升级，升级备份 #6 succeeded，维护版本 4；私有指标 `quoin_upgrade_prepared=1`。
- 停止执行组件和 Quoin，保留所有卷，保存私有冷备份（包括匹配的秘密与运行时状态，不公开其内容）。
- 原镜像 preflight 因旧实现只接受零迁移历史而拒绝 `schema_history_present: 1 ledger rows`。没有删除账本或忽略失败；修复并测试新版本只读预检，严格允许精确 3a95 前驱和既有直接对话迁移 ID/摘要，仍保留全部维护/备份检查。
- r3 preflight 成功：backupId=6、maintenanceRevision=4、migrationHistory=1、manifest SHA-256 `709e7b80816b7dadaed8c3792cbc69e49af21dcd6dee7eb183a76e7f6f297a0b`。
- r3 migrate 成功，迁移历史为 2，退出维护。旧配置 1 保留为 superseded，新声明配置 2 published，映射 1→2。
- 旧商城无显式告警源限制；迁移仅从不可变历史已归属告警证明 source 1，写入配置 2 的显式来源，不授权其他来源。独立回归验证无历史时阻止迁移，不默认为所有启用源。
- 五组件 Deployment 替换并恢复 Ready。浏览器关于页实际显示 Quoin r3、Plinth/Lintel r2 已连接、未维护；设置不再有连接/告警源/标签契约重复入口。
- 自动资源刷新 #1 Completed，发现 6 个当前资源。历史数据库、报告、对话未重置。

## DNS 修复与进一步验收

经用户授权，保存共享 CoreDNS 原配置后，将 `coredns-custom` 中重复注入根 server block 的 hosts override 改为 `quoin-lab.quoin.internal:53` 独立 server block。CoreDNS 恢复 Ready，保留本地域名指向 192.168.1.200。

后续模型请求仍出现 EOF。相同无凭据 Go POST 在宿主网络可得到正常 401，但在 Plinth 容器解析环境中出现 EOF；设置 `ndots:1` 后同容器立即恢复 401。根因是 ndots:5 搜索域扩展与本机 DNS 代理假 IP 应答结合，错误解析外部模型域名。已仅对 Plinth Deployment 添加 dnsConfig.options.ndots=1 并完成滚动更新。配置备份和修复后 manifest 位于 `.artifacts/k8s-gui-mall-20260910/declaration-upgrade-20260912/`。

实际证据：
- 页面点击创建商城手动巡检 Run 307（23:17:45），三项采证成功，证据 #964–966；当时模型仍为 EOF，报告版本 0。该记录保留，不算成功报告验收。
- DNS 优先级修复后，定时 Run 311（23:35）自动生成报告 #178，Attempt 1274 成功；Run 312（23:40）自动生成报告 #179，Attempt 1279 成功，均约 9 秒。此为真实后台模型结果，尚未通过页面打开报告完成 GUI 核验。
- CoreDNS 和全部应用 Deployment 均 Ready。

当前点击验收仍未完成：浏览器自动化再次出现 DOM 点击无可见效果及 `browser screenshot activity capture failed for guest`，读取 DOM 正常。未通过 API 操作代替人工点击证据。仍缺新版本手动报告成功、AI SRE 对话成功、告警分析成功，以及定时报告的页面核验。当前新 Quoin 已包含定时备份死锁修复。


## 当前版本最终点击验收（2026-09-13）

| 用户目标 | 实际页面动作与结果 | 持久化核对 |
| --- | --- | --- |
| 告警接收并触发 AI 分析 | 独立 Alertmanager 接收受控 DeclarationAcceptanceVerified；在告警列表点开详情，核对 local-inspection-demo 归属及受控验收说明，点击 AI 分析，页面显示已完成、结论及证据 #1030–1032。 | occurrence 4、source 1、business 1；analysis 6、Attempt 1355 Succeeded。 |
| AI SRE 对话巡检 | 点击 AI SRE 进入新建工作区，选择本地商城巡检，输入查询 up/mysql_up/redis_up 的请求，点击创建并发送；页面回复真实采样值、时间、证据及可达性不等于业务健康的边界。 | Investigation 3、Attempt 1334 Succeeded，证据 #1015–1017。 |
| 手动巡检到报告 | 页面点击创建巡检、选择商城健康巡检并开始；Run 318 自动完成三项采证和模型分析，无需人工重新分析；关闭创建弹窗后报告可读。 | Run 318 manual，00:09:57 创建，00:10:05 报告 #186 v1，Attempt 1309 Succeeded，证据 #997–999。 |
| 定时巡检到报告 | 页面点击定时 Run 311，实际打开报告，核对 schedule、Succeeded、报告 v1及三个指标表格。 | Run 311 schedule，2026-09-12 23:35 创建，23:35:08 报告 #178，Attempt 1274 Succeeded，证据 #976–978。 |

时间为页面本地时间（Asia/Shanghai）；SQLite UTC 时间已核对。四份截图归档于 `.artifacts/k8s-gui-mall-20260910/evidence/declaration-gui/`：`alert-analysis-4.png`、`ai-sre-conversation-3.png`、`manual-run-318.png`、`scheduled-run-311.png`。

### 完成核对与边界

- 三条用户目标逐项由当前部署页面动作、页面结果和不可变执行记录共同证明，不使用旧版本验收或 Deployment Ready 代替。
- Run 307 的网络故障失败历史保留，不计入成功；新手动 Run 318 提供独立完整成功链路。
- 模型查询实际返回两个旧 VM exporter 的 up=0；这是声明覆盖范围内的真实观测，不是验收造数，也不自动证明业务事故。模型结论仍需运维人员结合业务指标判断。
- 本轮验收不等同于验证所有异常路径、多轮对话场景或完整业务 SLI；知识库、Kubernetes 接入与浏览器巡检仍未开放。
