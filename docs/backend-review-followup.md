# 后端审阅修复验证记录

## 范围与状态

基线 `ec5d3bb`；修复分支 `fix/backend-review-followup`。本轮分类提交保留本地，未推送、未部署，未重建任何已有数据库。

已实现：同 boot 工具结果补发及提前到达缓冲、等待超时、连接心跳退出；Stele 关停与旧流清理围栏；长响应写期限转发；模型供应商真实资格探测、调用前账本准入、失败终结及可靠结果投递；凭据拒绝接入问题；本地死信查询重放；登录审计主体；敏感下载逐块会话检查；HTTP/gRPC 指标及 gRPC recovery；Operator 业务视图安全投影、连接最近探测；退役 OTP UI 清理、分页与知识列表入口；冻结工具目录中立渲染。

## 已执行验证

- `go build ./...`
- `go vet ./...`
- `go test ./... -count=1`
- `go test -race ./internal/plinth/runtime ./internal/quoin/runtime ./internal/quoin/app ./internal/stele`
- `pnpm --dir web typecheck`
- `pnpm --dir web lint`
- `pnpm --dir web test`：34 文件、262 用例通过。
- `pnpm --dir web build`
- `pnpm --dir web test:e2e`：6 用例通过。
- `pnpm --dir web test:e2e:real-local`：1 用例通过；使用 API stub，不是三组件真实部署验收。

## 明确未完成的验证与边界

- 并行独立审查因提供方额度耗尽而中断，没有完成最终双轴独立审查。测试通过不等于全后端没有 panic、漏审计或契约差异。
- 未执行 Docker 三组件实际部署验收或 k3s 实机验收。验收脚本 OTP 残留及 Stele 卷初始化已修正，但不能宣称实际部署通过。
- 探测取得凭据之前的提前失败仍沿用 supervisor 通用失败载荷；该载荷与 typed probe result 的衔接需要进一步修复和全链路测试。不能把本轮探测恢复视为所有失败情形均已闭合。
- 知识候选/导入批次新增入口当前只呈现首个服务端分页；新增列表的慢请求交错与刷新覆盖已加载页尚缺专门行为测试。
- 模型探测账本顺序已经修复，但完整的审计摘要与真实模型请求字节一致性仍需专项验证。
- tool-call 级 CancelToolCall 帧不存在于当前 proto，因此等待超时没有新增此帧；并非已实现服务端工具取消。
- HTTP 指标沿用契约有界 method 词表，词表之外的方法不计入这四个 family；TLS 握手失败也早于 gRPC interceptor。

## 延期议题

- https://github.com/Suknna/quoin/issues/108 ：Assigned attempt 同 boot 重连的幂等重投递契约。
- https://github.com/Suknna/quoin/issues/109 ：attempt 围栏校验与派发信封有限收敛。

## 本地死信操作

在 Stele 主机用运维身份读取其实际组件配置：

```sh
stele dead-letters list --config /etc/quoin/component.yaml --limit 50
stele dead-letters replay --config /etc/quoin/component.yaml --ids event-id
```

列表以只读方式打开已有队列；重放在事务中恢复原事件 ID、重置重试计数，并由正常投递链验证凭据。旧队列缺少凭据或凭据已轮换时，仅在确认事件来源与新凭据归属后同时提供 `--credential-id` 和 `--snapshot-version`。不要把完整 payload 输出复制到公共日志或 issue；载荷可能含敏感业务数据。重放不是跳过 Quoin 授权校验的入口。
