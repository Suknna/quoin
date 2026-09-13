# 演示范围与验收标准

本次演示聚焦三条真实业务链路，不使用伪造的模型回复或报告替代运行结果。

## 演示范围

| 能力 | 演示要求 |
| --- | --- |
| 告警分析 | Alertmanager 告警进入 Stele/Quoin，形成业务告警，再发起真实模型初步分析，展示结果与证据。 |
| YAML 巡检 | 用户提交业务系统 YAML，完成校验与发布，手动执行 PromQL 指标巡检，展示采证结果及真实模型生成的报告。 |
| 浏览器巡检 | 前端显示“开发中”，不开放浏览器身份配置、人工登录或 Journey 执行；已存储的配置和巡检数据保持不变。 |
| AI SRE | 使用 shadcn 官方聊天组件承载已有 Investigation API，支持多轮对话、工具调用、证据、附件和停止回复。 |
| 知识库 | 前端显示“开发中”，不开放知识整理与检索工作流。 |
| Kubernetes | 接入入口置灰并显示“开发中”，模型工具注册不提供 Kubernetes 工具。 |

## 已有实现与验收边界

三条主链路原本已有 API、领域状态和执行逻辑。代码存在或单元测试通过不等于真实环境验收通过：

- 告警接收成功不等于模型分析成功。
- YAML 配置校验成功不等于巡检采证或报告生成成功。
- 聊天消息成功提交不等于助手回复已经持久化。
- 报告缺失时必须展示真实状态，不能用示例文本伪装完成。

本次改动复用上述实现，修复现场联调中发现的阻塞。实际 GUI 操作、真实模型结果、备份迁移和演示步骤见 [Kubernetes GUI 端到端验收](gui-e2e-acceptance.md)。

## 聊天界面来源

聊天原语通过官方 shadcn registry 安装：

```bash
pnpm dlx shadcn@latest add message-scroller message bubble --yes
```

使用 `@shadcn/react` 的 MessageScroller 自动滚动能力，以及 Message/Bubble 组件；保留已有 `assistant-stream` 协议和 Quoin Investigation 业务接口，不另建聊天后端。界面保留证据、工具调用、附件、反馈、撤回与重试，知识库操作显示为不可用。

## 环境与凭据

- 原实验入口：<https://quoin-lab.quoin.internal/>。本次未完成当前版本的三条真实模型业务链路验收，不能视为可用演示交付。
- 当前 Kubernetes GUI 验收入口：<https://192.168.1.200:30447/>，命名空间 `quoin-gui-mall-20260910`。这是独立环境，不是原 `e2e-97` 数据的恢复。
- 较早的新建测试实例：<https://localhost:8445/>，保留不动，不作为当前 GUI 验收入口。
- 界面开发预览：<http://127.0.0.1:5175/>，明确使用模拟数据，仅用于外观与前端交互检查。
- 模型凭据来源：仓库根目录 `.env`，仅用于已授权模型服务。
- 当前 GUI 验收管理员凭据：`.artifacts/k8s-gui-mall-20260910/credentials.yaml`，字段 `admin.username` / `admin.password`。原实验环境凭据与此独立，不能混用。
- 联调依赖：`.local-lab` 中的 Java 服务、Prometheus、Alertmanager、MySQL 和 Redis。

不要将任何密码、模型 key、会话 Cookie 或告警接收凭据复制到本文件、截图、Git 或公开日志。不要重建数据库或删除已有实验环境状态来规避运行问题。
