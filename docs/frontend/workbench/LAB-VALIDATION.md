# 实验环境恢复与真实页面验证

日期：2026-09-08。范围：现有环境恢复、新前端认证与 Thanos 联调，不切换部署镜像。

## 恢复

确认原 manifest 副本均为 1 后，将 quoin-lab 的 quoin-quoin、quoin-plinth、quoin-lintel、quoin-stele Deployment 从 0 恢复为 1。独立 Prometheus 来源 `.local-lab/config/prometheus/k8s/40-statefulset.yaml` 同样为单副本，恢复其 StatefulSet 为 1。最终均 Ready 1/1。

所有 5 个 PVC 保持 Bound；没有删除卷、重建数据库、改 Secret、轮换凭据或替换镜像。既有连接 local-prometheus 保持已启用、rowVersion 3。

## 本机 HTTPS 联调

新前端： https://127.0.0.1:18443/ 。Vite 只监听 loopback，通过同源 `/api/` 转发到真实实验后端；保持 TLS 验证，不改写 Origin 或绕过 CSRF。

使用现有实验 CA 为 localhost/127.0.0.1 签发 7 天联调证书，文件在忽略目录 `.artifacts/new-workbench/tls/`，没有更改系统 hosts、安装新根 CA 或修改部署 TLS。凭据和私钥不写入本记录。

```sh
NODE_EXTRA_CA_CERTS="$PWD/.local-lab/config/kubernetes/tls/quoin-lab-ca.crt" \
QUOIN_PREVIEW_TLS_CERT="$PWD/.artifacts/new-workbench/tls/localhost.crt" \
QUOIN_PREVIEW_TLS_KEY="$PWD/.artifacts/new-workbench/tls/localhost.key" \
pnpm --dir web dev
```

## 实际通过的页面操作

- 通过新登录表单使用既有管理员认证成功；未调用脚本/API代替登录。
- 读取原 local-prometheus 和历史探测，确认数据保留。
- 页面发起探测 attempt 95，Succeeded；result 2 passed，真实 vector(1) 返回单样本值 1。
- 页面创建 `ui-validation-20260908`，保存为未启用，rowVersion 2，指向已恢复 Prometheus。
- 页面发起该连接探测 attempt 96，Succeeded；result 3 passed，真实单样本值 1。
- 页面尝试启用该连接，服务端拒绝第二个同类型连接，原连接仍启用。修复前端把 active_conflict 误报为版本冲突后复测，准确展示“同类型已有一个启用的连接；请先停用它。”
- HTTPS页面刷新后会话和连接hash深链恢复，真实探测历史仍可读取。

## 联调修复

- 只有 row_version_conflict 使用版本刷新说明，其余409保留实际服务端原因。
- probe POST后首次GET已经终态时也读取结果，避免快速完成时详情缺结果。
- Vite增加可选本机TLS证书/私钥配置，必须成对提供；不关闭上游校验。

构建、lint、16个新入口测试通过。

## 未通过/未执行

新连接成功启用未执行：现有 local-prometheus 已占用同类型唯一启用位置。没有为了验收停用它；验收连接保留为未启用，不自动删除。

首次强制改密未重演（现有管理员已完成）；完整角色/会话撤销/维护切换/故障恢复浏览器矩阵、登录后全尺寸视觉验收和镜像部署验收仍未全部完成。本记录不意味着整个阶段最终验收通过。
