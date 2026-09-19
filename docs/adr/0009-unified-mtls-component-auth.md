# ADR 0009: 统一 mTLS 组件认证（退役注册制与 service token）

- 状态：已接受
- 日期：2026-09-19
- 范围：runtime.proto、deployment-config schema、sql/schema.sql、quoin/plinth/stele 三组件、部署物与文档

## 背景

在此之前的内部认证有两套并行机制：

1. **Plinth 注册制**：管理员在 Admin UI 执行 prepare/reveal 取得 60 秒一次性注册令牌，经
   attached stdin 灌给 `plinth register`，换取长期 Bearer token 落状态卷。配套
   `runtime_slots`/`runtime_credentials` 两张表、约 35 个触发器、两阶段轮换与显式退休命令，
   以及 Lintel 恢复注册子系统。
2. **Stele service token**：`quoin secrets bootstrap` 生成 32 字节静态 token，Stele 以
   Bearer 直连。

两套机制带来三个问题：部署后必须在 Web UI 完成注册操作（部署不可声明式完成）；凭据生命周期
状态机复杂（注册窗口、pending/retiring 指针、row_version 并发前提）；同一部署内存在两种
身份语义。

## 决策

对齐 Kubernetes 的组件认证模型：**一个部署 CA，组件各持 CA 签发的客户端证书，内部 gRPC
全部 mTLS，服务端按已验证链叶证书 CN 授权服务**。

- `quoin secrets bootstrap` 用现有 runtime-ca 额外签发 `stele-client.crt/key`（CN=stele）与
  `plinth-client.crt/key`（CN=plinth），有效期与 CA 对齐（10 年）。
- quoin:8443 的 TLS 由 gRPC 自身终结（grpc.Creds），强制
  `RequireAndVerifyClientCert`；RuntimeControl 与 ArtifactService 仅接受 CN=plinth，
  SteleRelay 仅接受 CN=stele。
- Stele service token 与 Plinth 注册制整体退役：Register RPC、prepare/reveal/retire HTTP
  命令、一次性令牌铸造、`runtime_slots`/`runtime_credentials`/`lintel_recovery_receipts`
  表及其触发器全部删除；Plinth 无任何持久凭据状态，状态卷丢失即重连。
- 存量部署经 `quoin secrets issue-client-certs`（从既有 CA 补签，`--force` 轮换）与
  `quoin migrate` 一次性升级；本版契约指纹变化，五镜像须同批替换。
- Plinth/Lintel/Stele 组件配置各增加客户端证书/私钥路径字段；quoin 配置增加
  `runtimeClientCaFile`。

## 后果

- 部署即认证：配置文件 + Secret 挂载完成全部身份供给，无任何前端操作。
- 信任域收敛为密钥卷本身：持有 CA 私钥即可签发任意组件身份（与 Kubernetes 集群 CA 同权），
  该私钥只存在于部署 secrets 目录/Kubernetes Secret。
- 证书到期轮换是显式运维操作（`issue-client-certs --force` + 更新 Secret + 重启组件）；
  kubelet 式自动轮换未纳入本期。
- Hello 拒绝原因与 GoAway 原因枚举删除凭据类取值（wire 编号 reserved）。
