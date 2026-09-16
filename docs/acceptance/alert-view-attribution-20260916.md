# 业务视图告警归属升级验证（2026-09-16）

## 已实现

新告警不再查询旧已发布业务声明。业务视图显式配置 `alertSourceKeys` 和非空标签条件；唯一匹配 attributed，多匹配 ambiguous，无匹配 unattributed。Prometheus connection 与 Alertmanager source 身份分离。首次归属冻结候选名称、键、来源和条件；SQL 禁止更新/删除快照；列表按业务视图过滤时不混入平台故障。

历史告警保留旧归属事实，不回填或重算。具体规则见 ADR-0008 和使用手册。

## 测试

- `go test ./... -count=1` 通过。
- `go vet ./...` 通过。
- `pnpm --dir web typecheck` 通过。
- `pnpm --dir web test` 通过，37 文件、278 测试。
- 前后端镜像 `view-attribution-20260916-r2` 构建成功。
- 独立审查发现的快照不可变约束、名称漂移及平台故障筛选问题均已修复并有回归测试。

## 本机部署

通过已登录管理员会话调用正式升级准备接口；维护清单 Safe，自动升级备份 id=1 succeeded。停止 quoin/plinth/stele 后，在原数据卷和原密钥挂载上运行旧镜像 `migrate preflight` 和新镜像 `migrate`，两步均成功。

迁移记录：`20260916_alert_view_attribution_v1`。新前后端镜像均为 `view-attribution-20260916-r2`，本地 `$HOME/quoin-k8s/quoin.yaml` 已同步。五个 Pod 均 Ready，maintenance 已退出。原告警记录保留，账号、运行时身份与接入没有重新初始化。

## 浏览器与真实告警验收

正式升级撤销了旧管理员会话（迁移输出 revokedSessionCount=1）。用户完成正常登录后，通过浏览器创建业务视图 `mall-shop`（id=1，显示名“mall 商城”），选择指标连接 `mall-shop-prometheus`、告警来源 `mall-shop-alertmanager`，精确标签条件为 `system_id=mall-shop`。没有绕过二级验证。

再次运行 `scripts/mall-shop/alert-drill.sh`，仅暂停 Redis exporter，恢复原副本数。2026-09-16T13:37:48.339Z 触发的 `MallShopMiddlewareTargetDown` 形成 occurrence 2，13:38:58Z 已恢复。数据库只读核验 `status=attributed, attributed_view_id=1`；浏览器详情显示“归属视图 · mall 商城”，并说明首次接收时唯一匹配、证据已冻结。

之前 occurrence 1 仍显示历史未归属，没有回填或重算。真实归属验收通过。后续模型和巡检验收单独记录，不能由归属成功推断模型结论正确。
