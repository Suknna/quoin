# 插件开发指南（ADR-0011 插件体系 v2）

本指南面向为 Quoin 编写插件的开发者。现行体系是**接口 + 注册表 + 空白导入**的经典编译期装配：
不用 Go 标准库 `plugin` 包（无 dlopen、无动态安装），每个插件文件 `init()` 自注册，宿主进程空白
导入插件包即完成装配；装配失败（重复 ID、词表外名称、共享工具契约分歧）在启动期 panic。

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
        DefaultEnabled: false,
        ConfigSchema:   myConfigSchema, // 实例设置的封闭 JSON Schema（draft 2020-12）
        EventSource:    mySource{},     // 可选：入向能力
        Tools:          myTools{},      // 可选：出向能力
        // 可选声明目录（调度器消费，执行走内部工具）：
        // DiscoverObjects / InspectionTemplates
    })
}
```

宿主装配：`cmd/quoin` 与 `cmd/stele` 已空白导入 `internal/plugins/builtin`。新增插件在该包加一个
文件即被两个宿主同时装配；新宿主只需同样的空白导入。注册表首次读取即冻结，之后不可再注册。

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
契约（参考 builtin/alertmanager.go：payload 保持平台 wire 投影形状）。

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
  现、确定性采集——见 builtin/metrics.go 的 `metrics_probe`/`metrics_discover`/`metrics_collect`）。
- **共享契约**：多个插件可贡献同名同 manifest 的工具（如 prometheus 与 thanos 共享
  `thanos_query`）；目录单条目，溯源列全部启用的贡献者，manifest 分歧是装配错误。

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
