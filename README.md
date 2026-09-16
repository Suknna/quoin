# Quoin

**AI SRE 工作台，让告警排查、指标查询和日常巡检围绕可追溯的证据展开。**

[English](README.en.md) · [部署指南](docs/getting-started-kubernetes.md) · [使用手册](docs/user-guide.md)

Quoin 连接现有的 Alertmanager、Prometheus 和 Thanos，将告警、指标与 AI 分析汇集到一个工作台。运维人员可以从告警开始排查，通过对话查询指标，也可以运行巡检并查看带有证据引用的分析报告。

## 核心能力

- **告警管理**：接收 Alertmanager 告警，查看状态变化，结合指标进行分析。
- **AI SRE**：通过对话查询已接入的 Prometheus/Thanos 指标，结合告警和查询结果辅助排障，并保留分析使用的证据。
- **指标观测**：验证并启用指标来源，查看采集目标及其观测状态。
- **巡检与报告**：执行即时或时间范围查询，按来源或业务视图组织巡检，自定义检查说明与报告要求。支持重新采证，也支持基于已有证据重新分析。
- **认证与审计**：提供管理员与普通用户角色、二级验证、执行审计及备份管理。

AI 分析需要配置并启用模型提供方。Quoin 复用现有监控数据，不替代 Prometheus，也不负责安装业务服务或 exporter。

## 插件体系

Quoin 通过插件扩展平台接入能力，将不同来源的告警、指标查询、资源观测与巡检能力接入统一工作台。每个插件提供其支持的能力，不要求所有插件具备相同功能。

| 插件 | 状态 | 能力或方向 |
| --- | --- | --- |
| Prometheus | 当前可用 | 指标查询、采集目标观测与巡检采证 |
| Thanos | 当前可用 | Prometheus 兼容指标查询、目标观测与巡检采证 |
| Alertmanager | 当前可用 | 告警接收与来源管理 |
| 浏览器 | 后续计划接入 | 浏览器场景接入 |
| Kubernetes（K8s） | 后续计划接入 | 集群资源接入 |

当前插件随 Quoin 构建发布。扩展机制见[插件开发文档](docs/plugin-development.md)。计划中的插件尚不属于当前可用功能，具体能力以后续版本为准。

## 工作方式

```text
业务服务 / exporters ──→ Prometheus / Thanos
                                  │
                           指标查询与巡检采证
                                  │
Alertmanager ──→ Stele ──→ Quoin ←──→ Plinth
                            │
                       Web 工作台
```

默认部署包含五个服务：

| 服务 | 职责 |
| --- | --- |
| gateway | HTTPS 入口，统一转发前端、API 和告警请求 |
| frontend | Web 工作台 |
| quoin | 业务 API、数据存储、认证与任务管理 |
| plinth | 执行运行时，承担查询与分析任务 |
| stele | 接收并转交 Alertmanager 告警 |

## 快速开始

**推荐使用 Kubernetes 部署。** 仓库提供普通 Kubernetes YAML 清单，无需 Helm。

1. **准备环境**：Kubernetes 集群、持久化存储、HTTPS 域名与证书，以及用于登录验证码的 SMTP 或 HTTPS 投递渠道。
2. **构建镜像**：在仓库根目录执行 `make images`，将四个应用镜像推送到集群可访问的镜像仓库，或加载到相应节点。
3. **配置并部署**：按照 [Kubernetes 部署指南](docs/getting-started-kubernetes.md)准备 Secret，设置公开访问地址、镜像引用和存储配置，再应用清单。
4. **初始化管理员**：打开 Web 页面，用 `admin/admin` 进入初始化，设置正式密码并验证联系方式。完成后默认密码失效。
5. **接入监控**：注册 Plinth，按[使用手册](docs/user-guide.md)接入 Prometheus/Thanos 和 Alertmanager，开始查询与巡检；使用 AI 分析前配置模型提供方。

如果环境不具备 Kubernetes，可采用 Docker Compose 部署，详见 [Docker Compose 安装指南](docs/getting-started.md)。

> 首次初始化前，请限制部署入口的访问范围，防止公开默认凭据被他人使用。密钥、连接凭据和验证码不要提交到 Git。

## 构建与开发

源码构建要求 Go **1.25.8 或更高兼容版本**（以 `go.mod` 为准）。前端使用 **pnpm 10.30.3**，推荐 Node.js 24。容器构建使用 Dockerfile 中固定的工具链。

```bash
pnpm install --frozen-lockfile
make test
make vet
make web-typecheck web-lint web-test web-build
make images
```

`make images` 构建 `frontend`、`quoin`、`plinth`、`stele`，不启动服务。默认镜像名为 `quoin/<component>:v0.1.0-dev`；镜像标签与构建参数见[镜像构建说明](docs/deployment.md#镜像构建)。

## 文档

| 文档 | 内容 |
| --- | --- |
| [Kubernetes 部署](docs/getting-started-kubernetes.md) | 推荐部署方式、TLS、Secret 与持久化 |
| [使用手册](docs/user-guide.md) | 接入管理、告警、AI SRE、巡检与报告 |
| [部署与运维参考](docs/deployment.md) | 服务拓扑、备份恢复与离线管理 |
| [Docker Compose 安装](docs/getting-started.md) | 不具备 Kubernetes 环境时的部署方式 |
| [接入与巡检参考](docs/integration-inspection-guide.md) | 来源观测、业务视图与巡检语义 |
| [前端开发](docs/web-development.md) | 前端开发与测试 |
| [插件开发](docs/plugin-development.md) | 内建插件开发契约 |

## 项目状态

Quoin 正在持续开发，并将逐步扩展插件接入能力。生产使用前，请评估所用版本，并验证访问控制、证书信任、验证码投递与备份恢复流程。AI 生成的分析应结合原始证据判断。

问题和功能建议请提交至 [GitHub Issues](https://github.com/Suknna/quoin/issues)。报告问题时附上版本、复现步骤和脱敏日志。
