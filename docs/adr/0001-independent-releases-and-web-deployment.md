---
status: accepted
---

# 以权威契约约束构建，采用同源的前后端独立部署

本次设计讨论已收敛，用户确认采用下述方案。本 ADR 记录目标架构，不表示代码、构建或部署迁移已经完成。

## Proto 权威契约

移除 Quoin、Plinth、Lintel 和 Stele 的发布版本绑定。发布版本保留用于展示和执行溯源，身份认证及其他安全校验不变。通信准入以统一 Proto 权威契约的指纹一致为必要条件，不按通信接口分别管理兼容版本，也不引入协议降级或多代兼容层。

Proto 权威契约是以下权威 protobuf 文件的集合，用户此前称为 `probe`，不指连接探测：

- `docs/specs/quoin-v1/contracts/runtime.proto`
- `docs/specs/quoin-v1/contracts/quoin/plinth/worker/v1/agent_worker.proto`

复用现有 SHA-256 摘要能力，为完整权威文件集合生成确定性统一指纹。运行时比较该指纹而不是组件发布版本；指纹不同则明确拒绝通信。契约源文件变化即触发重新构建，不区分兼容字段增加与破坏性变更，不以手工协议版本号替代权威文件一致性。

## 构建边界

- Proto 权威契约变化：重新构建 Quoin、Plinth、Lintel、Stele、前端五个应用镜像。
- HTTP API 权威契约（`docs/specs/quoin-v1/contracts/openapi.yaml`）变化：重新构建 Quoin 和前端。
- 仅某组件实现变化且权威契约不变：只需重新构建该组件。
- 入口 Caddy 使用固定版本的第三方镜像，不因业务契约变化重新构建。

前端具有独立开发、构建和发布产物，不再嵌入 Quoin 二进制。独立发布不意味着契约变化后仍支持混用旧产物，不引入多代 API 兼容层。重新构建规则不等于保证跨契约滚动升级无中断。

## 六服务部署与同源入口

六个常驻服务容器为入口 Caddy、前端静态 HTTP 服务、Quoin、Plinth、Lintel、Stele。入口 Caddy 负责证书加载、TLS 终止和反向代理；前端容器携带构建产物并实际提供静态文件，不运行开发服务器，也不向共享卷复制资源交由入口 Caddy 托管。

浏览器仍访问单一公共 Origin；入口分流页面请求与后端 API、SSE、WebSocket 请求，开发环境由前端开发服务器代理后端请求。独立发布不意味着启用带凭据的跨域 CORS，现有会话认证与来源校验边界保留。

部署以 Kubernetes 为主，直接提供简单 YAML 清单，移除 Helm，不再把 Chart 作为交付或发布要求。默认由 Kubernetes TLS Secret 提供证书，入口 Caddy 加载证书并终止 TLS，通过标准 Service 对外暴露；不强制依赖 Ingress Controller、cert-manager 或公网 ACME。Service 的具体暴露类型由目标集群网络条件决定，不假定所有集群都有 LoadBalancer 实现。默认方案不承担自动申请证书，证书提供与更新由部署者负责。

Compose 作为辅助部署方式保留，直接提供简单 Compose 配置文件，不要求用户经过复杂部署配置生成流程。六服务指服务角色划分，不要求在 Kubernetes 中放入同一个 Pod。

## 被替换的旧约束

本决策替换现有规格中“部署不捆绑 Caddy”、Helm 交付要求、前端二进制嵌入式发布，以及运行通信要求发布版本完全相同的约束；其他身份、安全和权威状态边界不变。实施时必须同步清理旧实现、规格与验收，不保留 Helm 或嵌入式前端兼容路径。现有代码与旧规格尚未完成迁移时，以上目标架构边界以本 ADR 为准。
