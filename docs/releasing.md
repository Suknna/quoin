# 发布 Quoin

Quoin 的发布以 Git tag 为准。创建并推送符合规则的 tag 后，GitHub Actions 会完成构建、发布镜像、打包离线镜像并创建 GitHub Release。

## 版本号

Tag 必须是 `vX.Y.Z`，例如 `v0.1.0`：

- **`v0.Y.Z`（测试阶段）**：第一个稳定版本之前固定 X 为 `0`；Y 可包含功能或接口的不兼容变化，Z 为缺陷修复。
- **`vX.Y.Z`（稳定阶段，从 `v1.0.0` 开始）**：X 为不兼容的主版本，Y 为向后兼容的新功能，Z 为向后兼容的缺陷修复。

不要复用或移动已发布的 tag。需要更改时，发布新的版本号。

## 发布流程

1. 在 `main` 合入已验证的变更，运行项目检查。
2. 确认版本号并创建带注释 tag：

   ```bash
   git tag -a v0.1.0 -m "Quoin v0.1.0"
   git push origin v0.1.0
   ```

3. 等待 **Release Quoin** workflow 成功。它在原生 `linux/amd64` 与 `linux/arm64` runner 构建四个应用镜像，发布多架构 GHCR 镜像，并上传两个 Docker 离线镜像包和 SHA-256 校验文件。
4. 在 GitHub Release 页面确认下载文件、校验值和自动生成的变更说明。说明以简洁的中文开头；GitHub 会在后续版本自动补充本次 tag 与上一个 tag 之间的提交/PR 列表。维护者可在发布页面补充面向用户的迁移说明。

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
