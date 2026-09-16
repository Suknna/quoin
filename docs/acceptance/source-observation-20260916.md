# 来源自动观测故障排查（2026-09-16）

## 现场现象

本机 Kubernetes 部署中，通过 Web 创建 `mall-shop-prometheus`（`http://192.168.1.200:30090`），接入真实 probe 通过并启用；自动观测 Run 持续 Running，观测对象为空。“停用 → 重新验证并启用”后，新任务再次复现。

关键运行日志（无秘密）：

```text
connection.probe_committed: attempt=4 outcome=passed
config_verification.accept_failed: attempt: load correlation on attempt 5: context canceled
source_observation.commit_failed: attempt=5 err=attempt is not running; late result rejected
source_observation.result_rejected: attempt=5 detail=source observation result does not close onto its frozen attempt
```

Quoin 重启后，旧任务重派还有以下错误：

```text
reconcile.redispatch: attempt 2 has no inspection rebuilder for source_observation_execution_v1
```

现有 `go test ./internal/quoin/observation ./internal/quoin/inspection -count=1` 通过，说明现有测试没有覆盖现场触发的跨请求任务生命周期问题，不能以该测试通过代替修复验收。

## 修复与验收结果

- `inspection_collection` 按真实 scope 路由到观测、巡检或配置验证的任务服务；未知 scope 和未接线 owner 拒绝处理，不回退到其他业务服务。
- 已接收的 Accept/Result/CancelAck 使用保留上下文值的有界裁决上下文（30 秒），不会因控制流断开立即取消持久状态转换；原有状态、boot、epoch 校验保留。
- 补齐来源观测的重连输入重建、取消、失联与租约过期后的父 Run 收敛。
- 新增实际路由和冻结输入的回归测试，覆盖已取消上下文下的接收、取消闭合、重派及中断幂等收敛。
- `go test ./... -count=1`、`go vet ./...` 全部通过；日志 `/tmp/quoin-observation-full-test.log`、`/tmp/quoin-observation-vet.log`。

本机已构建并部署 `quoin/quoin:observation-fix-20260916`，只更新 Quoin 镜像。原 Plinth 身份和状态卷不变；受控重启 Plinth 让旧 boot 的卡住任务通过正常失联收敛结束，没有直接修改运行数据库。本地部署副本 `$HOME/quoin-k8s/quoin.yaml` 已同步新镜像引用。

浏览器真实验收：19:28:06 观测 Run 3 为 `Completed`，显示六个 target；19:33:06 定期观测再次成功，随后点击“刷新观测”，运行时日志确认 attempt 8 同样 `accepted=true`。六个目标为 MySQL exporter、Redis exporter、Java JMX、VM node、VM nginx、Prometheus self。

启动恢复时旧 epoch 的重派被现有连接围栏拒绝，随后通过新 boot 中断并结束旧任务；没有绕过冻结绑定校验。“最近验证”字段仍显示“尚未记录”，这是独立的展示问题，不代表实际 probe 或观测没有成功，本次未修改该字段语义。

## 与告警归属的区别

`mall-shop-alertmanager` 的真实通知链路已经通过：Quoin 告警时间线显示 18:29:43 firing、18:30:43 resolved，告警标签包含 `system_id=mall-shop`。

告警“未归属”与上述观测故障是不同问题：

- 当前归属实现 `internal/quoin/alerts/attribution.go` 仍查询启用业务系统的已发布声明版本，匹配告警源引用和声明标签条件。
- 单有 `system_id=mall-shop` 不会自动创建或匹配业务系统；业务视图不参与该归属查询。
- 新安装没有旧的已发布声明，而当前主线已经没有声明发布入口，因此新用户无法通过现有界面完成该类归属。这是当前接入模型与遗留归属实现的衔接缺口，不是用户漏配 exporter 标签。
- 归属结论在告警首次接收时冻结；不能通过改写数据库把已有告警标成“已归属”。

本次观测修复不调整告警归属模型，不创建旧业务声明，不改变已有告警历史。
