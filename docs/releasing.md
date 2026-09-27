# 发布 Quoin

Quoin 的发布以 Git tag 为准。推荐通过 **Prepare release** 工作流准备并发布；**Release Quoin** 工作流负责构建、发布镜像、打包离线镜像并创建 GitHub Release。

## 版本号

Tag 必须是 `vX.Y.Z`，例如 `v0.1.0`：

- **`v0.Y.Z`（测试阶段）**：第一个稳定版本之前固定 X 为 `0`；Y 可包含功能或接口的不兼容变化，Z 为缺陷修复。
- **`vX.Y.Z`（稳定阶段，从 `v1.0.0` 开始）**：X 为不兼容的主版本，Y 为向后兼容的新功能，Z 为向后兼容的缺陷修复。

不要复用或移动已发布的 tag。需要更改时，发布新的版本号。

## 发布流程

1. 在 `main` 合入要发布的变更，且确保比上一个 release tag 多至少一个提交。
2. 在 GitHub Actions 中运行 **Prepare release**，选择 `fix` 或 `feature`。也可用 `gh workflow run prepare-release.yml --ref main -f bump=fix`。工作流按最近的可达 SemVer tag 计算下一版（例如 `v0.1.2` + `fix` → `v0.1.3`，`feature` → `v0.2.0`），逐文件核对并更新镜像、部署和文档的固定版本引用，运行 Go/前端检查，提交到 `main` 并推送注释 tag。缺失或数量变化的版本引用会使其失败，不会静默跳过。
3. 工作流显式调度 **Release Quoin**：GitHub 的 `GITHUB_TOKEN` 推送 tag 不会再触发 tag-push 工作流，不能只等 push 事件。它在原生 `linux/amd64` 与 `linux/arm64` runner 构建四个应用镜像，发布多架构 GHCR 镜像，并上传两个 Docker 离线镜像包和 SHA-256 校验文件。
4. 等待 **Release Quoin** 成功；在 GitHub Release 页面确认下载文件、校验值和自动生成的变更说明。维护者可在发布页面补充面向用户的迁移说明。若准备提交已推送但 tag 未推送，重跑 **Prepare release** 会核对已提交版本并续推 tag；若 tag 已推送而调度失败，可通过现有 **Release Quoin** 的 `workflow_dispatch`，以该 tag 为 `version` 重新启动。不要移动或复用 tag。

**权限前提**：仓库 Actions 需要允许 `GITHUB_TOKEN` 写入内容并调度工作流，`main` 的保护规则须允许该发布身份推送。权限不足时准备工作流会失败；不得靠重写历史或强制推送绕过。若组织不允许机器人直推 `main`，应改为生成发布准备 PR，由维护者合并后再创建 tag。

手工恢复路径（准备工作流不可用时）：在 `main` 合入已验证的版本引用变更后，确认目标版本号，再创建并推送注释 tag：

   ```bash
   git tag -a v0.1.3 -m "Quoin v0.1.3"
   git push origin v0.1.3
   ```

手工推送的 tag 会直接触发 **Release Quoin**；它仍会验证 tag 内容并完成全部发布检查。

## 离线安装包

每个发布提供两个文件，分别用于 x86-64 与 ARM64 主机：

- `quoin-vX.Y.Z-linux-amd64-images.tar`
- `quoin-vX.Y.Z-linux-arm64-images.tar`

每个包包含 `frontend`、`quoin`、`plinth`、`stele` 四个 Quoin 应用镜像，以及部署所需的固定 Caddy 镜像。先校验，再导入与主机架构相符的包：

```bash
sha256sum -c quoin-vX.Y.Z-linux-amd64-images.tar.sha256
docker load -i quoin-vX.Y.Z-linux-amd64-images.tar
```

导入后按 [部署参考](deployment.md) 配置 Secret、TLS 和 `publicOrigin`，并将 Compose 或 Kubernetes 清单中的镜像 tag 改为该发布 tag。离线包不包含数据、私钥、TLS 证书或部署配置。

## 发布方式的依据

该流程采用主流的不可变 tag、双原生架构构建、多架构镜像索引、发布资产校验和自动 Release notes 方式：

- [GitHub Actions：发布 Docker 镜像](https://docs.github.com/actions/publishing-packages/publishing-docker-images)
- [GitHub：自动生成 Release notes](https://docs.github.com/repositories/releasing-projects-on-github/automatically-generated-release-notes)
- [Docker：GitHub Actions 多平台构建](https://docs.docker.com/build/ci/github-actions/multi-platform/)
- [Docker：`docker image save`](https://docs.docker.com/reference/cli/docker/image/save/)
