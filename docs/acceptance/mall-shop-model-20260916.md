# mall-shop 真实模型与报告验收（2026-09-16）

## 方法与边界

在已有 mall-shop Kubernetes/虚拟机环境中，通过已登录管理员浏览器操作。使用实际 `deepseek-flash` 模型，不以任务 `Succeeded` 代替正确性验收；以原始 Evidence、工具参数和报告原文交叉核验。数据库仅 `mode=ro` 查询，未直接修改业务数据、凭据或会话。全部新巡检计划仅人工运行，不引入定时模型费用。

## 真实告警与归属

- 13:36:49Z 暂停 `mall-lab/mall-redis-exporter`，原副本数 1；Redis 服务未停止。
- 13:37:59Z 确认 Prometheus firing、Alertmanager active 与新 webhook firing 投递。
- 13:37:59Z 恢复副本；13:39:00Z 确认 up=1、规则 inactive 与新 resolved 投递。
- 新 occurrence 2 首次归属业务视图 `mall-shop`（id=1）；旧 occurrence 1 保持历史未归属。
- 告警详情初步分析正确区分 exporter 抓取中断与 Redis 数据面故障。原始证据 84–113 支持 13:37:00Z–13:38:15Z 连续 6 个零值采样、约 90 秒监控盲区；Redis 可用性在盲区缺数据而非返回零。分析没有宣称商城交易正常或 Redis 已宕机。

## 对话发现与修复

原对话 1 / attempt 35 不通过：模型声称没有历史告警、过去一小时 up 全为 1。原始工具证据 42 实际包含 Redis exporter 最低值 0，且 Quoin 已收录 occurrence 2。

根因分两层：

1. investigation 冻结输入未提供平台告警记录；恢复后的即时 ALERTS 为空不能证明从未触发或没有告警规则。
2. 模型把工具已返回的 0 错误复述为 1。这不是 Prometheus 查询时间或工具执行错误。

修复为新回合冻结平台最近最多 10 条告警的不可变事实（ID、来源键、标签、开始时间），按 lineage 重建；不引入实时生命周期字段，避免冻结输入漂移。工具来源授权独立且不扩大。新增约束要求区分平台告警事实、历史指标与即时状态，并逐项核对数值。

新回合采用 `investigation-v2` / `investigation-renderer-v3`；旧回合继续旧提示词与渲染代际，不静默改变历史执行契约。回归覆盖最新十条排序、跨来源记录、可变状态不改变快照、不可变标签约束及旧/新执行版本路由。

部署 `quoin/quoin:model-evidence-20260916` 与 `quoin/plinth:model-evidence-20260916` 后，对话 5 使用与对话 1 完全相同的问题复测：正确识别 occurrence 1/2、历史最低值 0、约90秒抓取盲区，说明恢复为根据当前指标的推断，并明确未知业务影响。没有再声称不存在历史告警。

### 四个预设实测结论

| 预设 | 实际回合 | 结论 |
| --- | --- | --- |
| 告警总结与影响排序 | 对话1 attempt35失败；对话5 attempt56复测 | 修复后核心验收通过；原问题逐字相同，history包含occurrence1/2，最低值0、90秒盲区与证据一致 |
| 最近30分钟错误率 | 对话2 attempt49 | 通过；先核实前提，指出缺业务错误率指标，不虚构错误率升高；基础与组件指标仅作旁证 |
| 安全缓解与回滚 | 对话3 attempt51 | 核心验收通过；区分exporter与Redis，分只读验证/条件性操作/回滚，未执行变更，不猜测namespace或Deployment |
| 延迟关联根因 | 对话4 attempt52 | 通过；说明缺服务级延迟指标，Redis命令延迟不等于商城请求延迟，不编造业务p95/p99 |

抽核发现缓解建议中一处采证时刻标注不精确：Redis uptime 41957 的实际评估时刻22:13:44被标为22:14:31。数值及连续运行推断有证据支持，未影响故障判定，但该原始回答并非逐字零缺陷，使用具体时间时应以Evidence为准。

## 三种巡检

| 计划 / Run | 输出约束 | 初次核验 |
| --- | --- | --- |
| `mall-redis-table` / 1 | 仅 Markdown 表格，固定五列 | 通过；原文仅3行表格，证据115、值1及采证时间一致 |
| `mall-mysql-json` / 2 | 无围栏的单个 JSON，固定7字段及类型 | 通过；`json.loads`成功，字段和类型全部符合，证据116及值1一致 |
| `mall-overall-sections` / 3 | 结论、异常、证据、建议四章节；结论≤100字；建议≤3条 | 章节、8条序列与证据119一致；结论长度边界未完全遵守，继续复测 |

MySQL JSON 固定字段为 `component,status,value,observedAt,evidenceIds,limitations,recommendations`。Redis/整体报告均未把抓取成功等同交易健康。整体表达式为 `{__name__=~"up|mysql_up|redis_up|nginx_up",system_id="mall-shop",job!="prometheus-self"}`，返回5条up及3条服务指标。

整体 Run3 v1 结论含空格107字符、去空白100字符。通过“重新分析现有证据”创建 v2，明确100字符包含字母、数字、空格和标点，并要求不重复索取已给出的0/1检查语义；原证据及原报告仍保留。该次仍未遵守长度，不能标为全项通过。

## 已执行的工程验证

- 完整 `go test ./...` 与 `go vet ./...` 通过（告警上下文修复版本）。
- `pnpm --dir web typecheck` 通过。
- `pnpm --dir web test`：37文件、278测试通过。
- `git diff --check` 通过。
- 修复部署后五个服务 Pod Ready，Redis exporter 恢复1/1。

## 巡检格式修复与最终复测

Run3 v2 结论严格计数为109字符，不符合100上限。冻结要求已正确到达模型，未发现系统提示词要求长摘要的冲突；属于模型依从失败。新增通用提示词约束：遵循冻结格式、字段、章节和长度，输出前逐项自检；使用检查说明已提供的取值含义，不再泛称所有检查均缺阈值。没有硬编码本次标题或100字符，没有解析任意自然语言规则或静默截断输出。

新巡检回合采用 `inspection-analysis-v2` / `inspection-analysis-renderer-v2`，保留之前两代提示词及路由。定向测试最初有一处新增文案断言与实际提示词措辞不一致，修正后通过；随后完整 `go test ./...`、`go vet ./...`、`git diff --check` 全部通过。

部署最终镜像 `quoin/quoin:report-compliance-20260916`、`quoin/plinth:report-compliance-20260916` 后，以与v2逐字相同的要求对同一Run重分析，生成v3 / attempt60。结论61字符，满足硬性100字符上限（未达到“尽量60”的软目标）；四章节次序正确、建议3条、8行证据表仍逐条对应Evidence119。三版 `evidence_digest` 完全一致，原Run配置及旧报告保留。摘要将目标概括为“4个exporter”，而证据实际有5条up（含Java JMX）；此概括容易歧义，目标清单应以8行明细为准，不把摘要当作精确目标计数。

因此：表格与JSON本次严格格式通过；整体报告经过修复后硬性格式通过，但仍保留上述摘要用词/软目标限制。提示词自检不能提供数学意义的可靠性保证，不能以一次成功宣称所有模型输出都会合规。

最终两个后端镜像已同步到本地 `$HOME/quoin-k8s/quoin.yaml`，未提交或推送Git。本记录保留失败样本，不删除或改写原始模型回答。
