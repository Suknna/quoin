---
status: accepted
---

# 以单一 BusinessSystem 声明拥有业务范围

## Context

Quoin 需要一个可由用户审核的单一业务范围来源。此前模型将业务系统配置、全局 Label Contract 和相关激活关联在不同资产中，令指标范围、资源策略和告警归属需要跨对象解释。新批准的 `BusinessSystem` JSON Schema 已定义 `apiVersion: quoin/v1`、`kind: BusinessSystem` 的封闭机器形状。

这一决定定义目标架构，不是实施完成声明。当前 API、SQL、解析/编译、运行时和 E2E 记录仍可能表达旧模型；既有 E2E 文档是其执行时的历史证据，不能被本 ADR 回溯改写。

## Decision

以 `contracts/schemas/business-system.schema.json` 为目标态唯一机器配置权威。每份声明自身拥有：

- 一个指标 `connectionRef`，以及适用于业务范围的 `matchLabels`；
- 资源的补充 labels、发现指标、身份 labels 和 allowed metrics；
- 告警源引用与告警自身 labels；
- 通过资源 `resourceRef` 绑定的巡检检查。

不保留活动全局 Label Contract。业务、资源和查询范围来自同一份声明；告警来源使用自身的 labels，不继承全局归属约定。接入、浏览器身份和模型供应商仍是独立管理对象，声明只引用它们而不保存秘密。

所有未来表单和 YAML 编辑视图必须读写同一不可变声明版本。迁移必须作为一个同步实施切片，更新 Schema、解析/编译、持久化、HTTP、运行时和测试；在该切片部署并接受前，不得宣称目标模型已运行。旧 Label Contract、旧声明、历史 Run 和 E2E 证据继续按既有保留规则可读，不能被重新解释为新模型的活动来源。

## Consequences

- 用户和运行时有一个清晰的业务范围来源，查询的 label 强制和 allowed metric 校验都可从 `resourceRef` 确定。
- 不再需要全局 label 激活或跨系统联合切换来定义目标态业务归属。
- 迁移是跨层变更，不能仅添加 Schema 或仅改变前端；在实施前，文档必须使用未来时态。
- 历史模型会在一段时间内与目标文档并存，因此旧文档必须明确标为历史，而不是被删改成虚假的已迁移事实。
