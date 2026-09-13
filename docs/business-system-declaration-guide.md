# BusinessSystem 声明指南

**状态：目标态指南，尚未部署或验收。** 本指南说明已批准的统一声明来源；它不表示当前 Quoin API、存储、运行时或现有 E2E 环境已经采用该模型。现有 E2E 文档继续记录其执行当时验证的契约和结果。

业务系统声明是目标态唯一的业务配置权威。机器字段与封闭对象规则由 [`docs/specs/quoin-v1/contracts/schemas/business-system.schema.json`](specs/quoin-v1/contracts/schemas/business-system.schema.json) 拥有，稳定 `$id` 为 `https://github.com/Suknna/quoin/schemas/business-system.schema.json`。本文不替代 Schema。

## 声明的范围

一份 `apiVersion: quoin/v1`、`kind: BusinessSystem` YAML 声明：

- 用 `metrics.connectionRef` 选择唯一的指标接入；地址、认证和其他秘密不进入 YAML。
- 用 `metrics.matchLabels` 声明所有资源与查询共享的强制范围；资源自己的 `matchLabels` 只能补充，不能冲突或放宽该范围。
- 对每个资源声明发现指标、稳定 identity labels 和可使用的指标白名单。
- 用 `alerts.sourceRefs` 和告警自身的 `alerts.matchLabels` 选择和约束告警来源。省略或留空告警标签时继承公共指标标签；显式告警标签可以与指标标签不同。来源列表为空时不自动归属，整个流程不依赖全局 Label Contract。
- 用巡检检查的 `resourceRef` 把每个 PromQL 查询绑定到一个资源，从而继承该资源的 labels 与 `allowedMetrics`。

目标态没有活动的全局 Label Contract。旧 Label Contract 文档和记录仅用于理解与保留历史，不能作为新声明的配置权威。

## 有效示例

以下示例声明商城 Java 应用的指标权限。`connectionRef` 和 `sourceRefs` 必须替换为运维中心已配置的接入名称，标签值必须与实际采集数据一致。`metadata.name` 仅是平台内业务键，不要求等于上游标签值。

```yaml
apiVersion: quoin/v1
kind: BusinessSystem
metadata:
  name: mall
  displayName: 本地商城
  description: 商城 Java 应用采集可达性与线程指标
spec:
  metrics:
    connectionRef: mall-live-prometheus
    matchLabels:
      system_id: local-inspection-demo
    resources:
      - name: application
        displayName: Java 应用
        matchLabels:
          job: mall-tiny-jmx
        discoveryMetric: up
        identityLabels: [job, instance]
        allowedMetrics: [up, jvm_threads_current]
  alerts:
    sourceRefs: [mall-gui-alertmanager]
    matchLabels: {}
  inspections:
    - name: health
      displayName: 商城健康巡检
      schedule: "*/5 * * * *"
      timezone: Asia/Shanghai
      checks:
        - name: java-up
          resourceRef: application
          expression: up
          question: 说明采集可达性与结论边界，不把采集成功等同于业务健康。
```

编译器会通过 PromQL AST 为每个向量选择器补充声明中的必需标签，因此检查中的 `expression: up` 可以保持简洁。如果表达式显式提供必需标签，必须与声明完全一致；查询也不能使用该资源组 `allowedMetrics` 以外的指标。名称、描述和 `question` 都不能扩大查询权限。

不填写 `spec.discovery.refresh` 时默认每 5 分钟刷新。发布新版本后，调度器在下一次扫描（正常运行时最多约 1 秒）纳入发现；进程重启时也立即扫描持久化的当前版本，不依赖浏览器保持打开。资源组可以继续添加 MySQL、Redis 等范围，每条检查通过 `resourceRef` 选择对应组，不把多个组的标签交叉组合成新权限。

## 编辑和迁移边界

在未来实现中，表单和 YAML 必须生成同一不可变声明版本，并走相同的静态校验、验证和发布流程。不得通过表单另存业务元数据、通过 YAML 另存范围，或把凭据复制到任一编辑视图。

本指南不指示用户迁移现有配置，也不定义已运行系统的兼容策略。实际迁移必须在接受相应 Schema、解析/编译、API、持久化、运行时和测试变更的同一实施切片中完成。详情见 [`inspection-config.md`](specs/quoin-v1/inspection-config.md) 和 [ADR-0003](adr/0003-unified-business-system-declaration.md)。
