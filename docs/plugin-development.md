# 插件开发指南（ADR-0011 插件体系 v2）

本指南面向为 Quoin 编写插件的开发者。现行体系是**接口 + 注册表 + 空白导入**的经典编译期装配：
不用 Go 标准库 `plugin` 包（无 dlopen、无动态安装），每个插件包 `init()` 自注册，宿主进程空白
导入选定插件包即完成装配；装配失败（重复 ID、词表外名称、共享工具契约分歧）在启动期 panic。

权威决策见 [ADR-0011](adr/0011-component-responsibility-and-plugin-v2.md)；体系前身（能力六分类、
ExecutionBundle、显式装配）见 ADR-0004/0007，其"声明派生自实现、不可漂移"与冻结目录不变式在
v2 中完整保留。

## 职责边界（一个插件在链路中的位置）

```
平台A ──┐                                        ┌── 工具调用（出向）
平台B ──┼──▶ Stele Webhook ──▶ Quoin 核心 ──▶ Plinth Agent 运行时
平台C ──┘    （入向网关）      （调度中枢）      （模型推理）
                                 │
                            插件注册中心
                           （工具执行也走这里）
```

一个插件可以提供两类能力，按网关边界划分：

| 能力 | 接口 | 执行宿主 | 说明 |
| --- | --- | --- | --- |
| 入向（EventSource） | `VerifyAndParse` | Stele webhook 层 | 把平台数据推/拉进来，归一化成统一事件 |
| 出向（ToolProvider） | 泛型 `Tool[A, R]` | Quoin（权限/审计/派发）+ Stele（凭证/限流/执行） | 模型可见部分只有描述与参数 schema |

**Plinth 对插件体系零感知**（编译级保证）：它的协议只有「任务 + 工具目录 → 文本或 tool_call」。

## 插件骨架

```go
package myplatform

import "github.com/Suknna/quoin/internal/plugins"

func init() {
    plugins.Register(plugins.Plugin{
        ID: "myplatform", Version: "1",
        DisplayName: "My Platform", Description: "……",
        ConnectionKind: "myplatform",   // 插件绑定的外部平台类型
        ConnectionTransport: plugins.ConnectionTransportHTTP,
        ConnectionAuthModes: []string{plugins.AuthModeNone, plugins.AuthModeBasic, plugins.AuthModeBearer},
        ConnectionProbePath: "/health?scope=read-only", // 可选；GET、只接受 HTTP 200
        DefaultEnabled: false,
        ConfigSchema:   myConfigSchema, // 实例设置的封闭 JSON Schema（draft 2020-12）
        EventSource:    mySource{},     // 可选：入向能力
        EventTypes:     []string{"alerts.batch"}, // 入向类型必须显式声明
        AlertIdentity:  plugins.AlertIdentityExternal, // 告警来源选一种固定身份模式
        AlertNormalizer: myAlertNormalizer{}, // 与告警身份模式一起声明
        PostCommitSubscriptions: []plugins.PostCommitSubscription{{EventType: plugins.FactAlertObservationCommitted}},
        PostCommitHandler: mySubscriber{}, // 可选；声明与处理器必须同时提供
        Tools:          myTools{},      // 可选：出向能力
        // 可选声明目录（调度器消费，执行走内部工具）：
        // DiscoverObjects / InspectionTemplates
    })
}
```

插件实现放在项目根目录 `plugins/<插件包>/`；契约和注册表仍在 `internal/plugins`。宿主装配：
`cmd/quoin/main.go` 与 `cmd/stele/main.go` 显式空白导入 `plugins/alertmanager` 和
`plugins/metrics`（后者共享实现并注册 prometheus、thanos）。新增插件须在需要该能力的宿主
`main.go` 中空白导入它的包；测试若依赖进程默认注册表，也须导入所需插件包。Plinth 不导入插件。
注册表首次读取即冻结，之后不可再注册。

## 入向：EventSource

```go
type mySource struct{}

func (mySource) Kind() string { return "myplatform" } // URL 段与来源协议身份

func (mySource) VerifyAndParse(ctx context.Context, req plugins.InboundRequest) ([]plugins.Event, error) {
    // req.Header / req.Body / req.ReceivedAt
    // 职责：协议级校验（载荷形状、来源特有签名）+ 归一化。业务语义（occurrence
    // 状态机、去重策略、归因）绝不在此——那是 Quoin 消费者的事。
    return []plugins.Event{{Type: "alerts.batch", Payload: normalized}}, nil
}
```

Stele 网关负责 Bearer/digest 认证（"签名对不对"）与 16MiB body 上限；解析失败在**入队前**拒绝
（HTTP 400），解析成功即本地入队并返回 202。`Event.Payload` 是你与 Quoin 消费者之间的归一化
契约（参考 `plugins/alertmanager/alertmanager.go`：payload 保持平台 wire 投影形状）。
`Plugin.ID` 是插件包的身份，`EventSource.Kind()` 是来源协议身份，两者可以不同；
管理目录会分别返回 `id` 和 `sourceKind`，创建实例时提交的是后者，不能把插件 ID
当成 URL kind 或来源 protocol。
`test/plugins/synthetic` 是仓库内的最小跨来源告警样例：编译期独立构造
EventSource + Normalizer + externalId 身份模式，测试从 Stele webhook、本地队列、
Quoin Relay 到 SQLite 及统一告警读模型，不需要在主流程为其增添品牌分支。
插件必须声明所有可能产出的 `EventTypes`；Stele 会在入队前拒绝未声明的类型，
Quoin 同样在接收时复核。声明一个新类型不等于核心已有其投影处理器，当前仅 `alerts.batch` 可消费。
编译入站清单指纹会在 Stele→Quoin 的快照/投递 RPC 上交叉校验；改动事件契约时应
更新插件 Version，并同批替换两宿主镜像。在更换载荷版本前先排空 Stele 本地队列，
因为旧排队事件尚无逐条 payload version 可供新消费者迁移。

## 提交后事件订阅

插件只能声明 `internal/plugins/postcommit.go` 列出的有限事实类型：告警观察提交、
日报窗口到期、巡检检查结果/证据提交、日报封存。Quoin 在权威事务内写有界事件引用，
提交后才异步回调 `HandlePostCommitFact(ctx, fact)`；回调拿不到业务库句柄、平台凭据、
原始正文，不能改变已完成的 Stele/Quoin ACK 裁决。`fact.ID` 是稳定的事件身份，
**处理器必须自己按 `(插件 ID, fact.ID)` 幂等**，因为崩溃/重试后可能再次调用。
有界重试后进入死信；管理员可从接入管理页查看或使用
`GET /api/v1/integrations/plugin-events/deadletters` 读取有界诊断，确认后通过
`POST /api/v1/integrations/plugin-events/deadletters/{deliveryId}/replay` 显式重放。
没有启用订阅者时不产生额外业务事件行。
新增领域语义不能仅靠订阅者凭空扩展核心事实词表，仍需有版本的核心契约变更。

## 受控 HTTP 接入与探测

有出站 HTTP 连接的插件须声明 `ConnectionKind`、`ConnectionTransportHTTP` 和允许的
`ConnectionAuthModes`。启用一个新连接前必须通过其插件声明的只读
`ConnectionProbePath`：Quoin 冻结 GET/路径/期望状态 200，经 Stele 网关执行
并在 SQL 中按当前 revision + 凭据 generation 校验探测资格。探测地址只能是
相对于该连接 endpoint 的绝对路径（可带 query，不可包含网络主机或凭据）；
Stele 不跟随平台响应的跳转，避免携凭据越过连接边界。未声明探测路径的 HTTP 类型
可以参与有限网关执行，但**不能通过 probe 启用新实例**，不要把它当完整接入能力。
模型供应商凭据仍只走 Plinth 路径，不属于插件 HTTP 连接。

## 出向：泛型工具

```go
type queryArgs struct {
    Query string `json:"query" doc:"查询表达式"`        // 无 omitempty = 必填
    Scope string `json:"scope,omitempty" doc:"范围限定"` // 可选
}

type queryResult struct {
    Success bool   `json:"success"`
    Output  string `json:"output"`
}

var queryTool = plugins.Tool[queryArgs, queryResult]{
    Name: "myplatform_query", Version: "1",
    FailureMode: plugins.FailureReturnToModel,   // 或 FailureFailAttempt
    ResultKind:  "myplatform_query_result_v1",   // ResultPayload.schema_kind
    Description: "对 My Platform 执行只读查询……",   // 模型可见
    ProducesEvidence:        true,
    RequiresConnectionGrant: true,               // 模型不选连接，Quoin 冻结 grant
    Timeout:           30 * time.Second,
    RateLimitPerMinute: 60,
    Handler: func(t *plugins.ToolContext, args queryArgs) (queryResult, error) {
        response, err := t.Platform.Call(t.Context, plugins.PlatformRequest{
            Method: "GET", Path: "/api/v1/query",
            Query: url.Values{"q": {args.Query}},
        })
        if err != nil { return queryResult{}, err }
        // 长正文经 t.Spill 溢出为 tool_result Artifact（可选）
        return queryResult{Success: true, Output: string(response.Body)}, nil
    },
}

type myTools struct{}

func (myTools) Tools() []plugins.ToolEntry {
    return []plugins.ToolEntry{queryTool.Entry("myplatform")}
}
```

要点：

- **类型安全**：参数在边界严格解码（未知字段/缺必填直接拒绝），Handler 内部全程 typed；manifest
  的参数 JSON Schema 从 `queryArgs` 反射派生（`json` tag 定属性与必填、`doc` tag 补描述），
  可用 `Schema` 字段显式覆盖。声明派生自实现，构造上不可漂移。
- **平台 I/O 只经 `t.Platform`**：只有 method + 相对路径；scheme/host、凭据、TLS、限流对 Handler
  不可见（Stele 网关注入与执行）。任何 HTTP 状态（含平台 4xx/5xx）都是传输成功，语义由你解释。
- **错误分类**：网关哨兵（`plugins.ErrPlatformRateLimited` 等）会被 Quoin 编排层映射为稳定错误
  码；结构化失败应像示例一样写进结果 payload（success=false），模型可见可重试。
- **内部工具**：`Internal: true` 的工具不进模型目录，仅供 Quoin 调度器调用（连接探测、有界发
  现、确定性采集——见 `plugins/metrics/metrics.go` 的 `metrics_probe`/`metrics_discover`/`metrics_collect`）。
- **共享契约**：多个插件可贡献同名同 manifest 的工具（如 prometheus 与 thanos 共享
  `thanos_query`）；目录单条目，溯源列全部启用的贡献者，manifest 分歧是装配错误。

## 告警归一化(ADR-0012,入向插件的第三能力)

提供告警入向的插件 SHOULD 同时实现 `AlertNormalizer`——把本来源的 EventSource payload 映射到
统一告警语义(纯函数、零业务依赖,业务解释归 Quoin):

```go
type myNormalizer struct{}

func (myNormalizer) NormalizeAlert(payload []byte) ([]plugins.NormalizedAlert, error) {
    // payload 是你的 EventSource 产出的归一化事件文档
    return []plugins.NormalizedAlert{{
        Severity:    plugins.SeverityCritical, // 四级词表:Critical/High/Warning/Info(带序数)
        SeverityRaw: raw,                      // 来源原始值,审计用
        Title:       name,
        Annotations: annotations,              // 全量冻结
        Resource:    instance,
    }}, nil
}

func init() {
    plugins.Register(plugins.Plugin{
        // ...
        EventSource:    mySource{},
        AlertNormalizer: myNormalizer{}, // 必须与 EventSource 同插件(kind 一致)
    })
}
```

- severity 映射表放插件里(参考 `plugins/alertmanager/alertmanager.go`);词表外的值降级 `Info`,Quoin 侧不会丢事件。
- Quoin 的 intake 流水线(归一化→富化→去重→关联)在首观测事务内执行你的 normalizer,语义列
  (severity/title/annotations_canonical/resource)冻结后不可变;缺 normalizer 的来源记
  `normalizer_missing` intake issue 并用缺省语义。
- 业务语义(occurrence 状态机、富化规则、视图关联)绝不在插件里。

### 多来源告警的现阶段接缝（ADR-0014，实施中）

告警源创建、Quoin Relay 与告警列表/详情不再把 `alertmanager` 当作唯一来源：
注册的 EventSource + AlertNormalizer 可以创建自己的来源；Relay 只接收其
`alerts.batch` 事件，并在事务内核对事件来源与来源凭据的归属。接入侧需要将
`alerts.batch` 的载荷转换为当前的批次结构（含每条完整 labels、startsAt、status）；
`AlertIdentityLabels` 要求 fingerprint 与 labels 一致，`AlertIdentityExternal` 要求每条携带
非空的 `externalId`、不得同时携带 fingerprint。外部身份原文首观测冻结、SHA-256
摘要用于来源内索引；缺失/冲突项不进入告警发生。此阶段**尚未完成**协议版本协商、
订阅派发或跨来源日报。新平台若无法安全映射该身份模型，不应伪造标签以接入，
应等待统一规范载荷的下一阶段实施。
部署启用集合与来源实例的启用状态分别受控：未启用的插件不能创建新的告警源，
已有来源的 Stele 凭据快照标记为不可接收，Quoin Relay 也拒绝新事件；
历史告警保持可读。

## 实例设置与校验

`ConfigSchema` 是实例设置文档（连接 revision 的非秘密部分）的封闭 JSON Schema：
`additionalProperties: false`、顶层 object。Quoin 在写入连接 revision 时用
`registry.ValidateConfig(pluginID, settings)` 校验；跨字段静态规则可实现 `Validator
ConfigValidator`（纯函数、无 I/O）。

## 当前内置插件

- **prometheus / thanos**：共享 `thanos_query`（v4，quoin_routed）模型工具；内部工具
  `metrics_probe`/`metrics_discover`/`metrics_collect` 支撑连接探测、来源观测与巡检采集；
  声明目录 DiscoverObjects（target）与 InspectionTemplates（promql_instant/promql_range）。
- **alertmanager**：EventSource（Alertmanager webhook 协议解析 → `alerts.batch` 归一化事件）。

## 测试与上线清单

1. 插件包内单测：注册校验、VerifyAndParse 的合法/非法样例、Handler 的 typed 往返（用 stub
   `PlatformCaller`）。
2. 目录装配：`attempt.BuildCatalogs` 对启用集的双向校验（平台工具优先、内部工具不进模型目录）。
3. 新模型可见工具必须在 Quoin 侧显式接线 ToolGrantResolver/ToolGrantValidator（ARCH-INPUT-003：
  连接由授权冻结，模型不得选择）。
4. 改动工具契约一律递增 `Version`（如 thanos_query v3→v4 的执行位置迁移）；已冻结目录按版本与
  manifest 全等校验，drift 会被显式拒绝而非静默重解释。
