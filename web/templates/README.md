# 官方模板基线（阶段一）

这不是 Quoin 业务前端，也不是认证服务。仅供确认完整 sidebar-09 与 login-02 的原始布局；不得输入真实密码。邮件、GitHub、注册、找回密码及账号菜单都是上游示例，不代表 Quoin 已支持。登录提交在入口捕获并阻止，不发送请求或显示假成功。

## 运行

在仓库根目录运行：

```sh
pnpm --dir web build:templates
pnpm --dir web preview:templates
```

打开 http://127.0.0.1:5174/ ，选择浅色/深色预览。直接入口为 `/sidebar-09/` 与 `/login-02/`，支持 `?theme=light`、`?theme=dark`；无参数跟随启动时系统主题。

开发命令：`pnpm --dir web dev:templates`。

独立 Vite root 为本目录，生产产物为 `.artifacts/template-baseline/dist`。不改旧 web/index.html、src 入口、旧 Vite 配置、Go embed dist 或实验部署。新 CSS 使用 Tailwind `source(none)` 并仅扫描 templates，不导入旧 CSS。

## 来源与安装

`provenance.yaml` 记录所有生成组件、精确依赖、URL、快照及 SHA-256；`sources/` 保存完整官方 registry 和 MIT 许可。源码快照是来源存档，不是手工编写的业务配置。

实际成功命令（仓库根目录）：

```sh
pnpm dlx shadcn@4.21.0 add -c web https://ui.shadcn.com/r/styles/new-york-v4/sidebar-09.json https://ui.shadcn.com/r/styles/new-york-v4/login-02.json -y
pnpm dlx shadcn@4.21.0 add -c web https://ui.shadcn.com/r/styles/new-york-v4/index.json -y
```

CLI 安装完整区块及递归组件，但在 Vite 工程中跳过 `registry:page`。因此从同一原始 JSON 提取两个 page.tsx，仅将 registry import 改为本地 `@/components`，其余页面内容逐字一致。组件为 CLI 生成，不是看截图重写。

## 必要适配与原始行为

- `components.json` 使用 new-york-v4、rsc=false、aliases 指向 templates。工具 JSON 与已有 package/tsconfig 是工具契约例外，自有台账采用 YAML。
- 官方 index registry 提供基础工具函数和 CSS import，但没有完整颜色变量。`theme.css` 精确提取官方 `apps/v4/app/globals.css` 的 `@theme inline`、`:root`、`.dark` 三段；删去三个依赖 Next 字体变量的自引用，使用 Tailwind 默认字体栈。该字体差异已知，未宣称字体像素一致。
- 使用当前官方 v4 全局 token，不采用 CLI 追加的旧 HSL sidebar 变量覆盖它们。没有自行调色；完整上游 globals 快照已保存。
- placeholder.svg 与 shadcn.jpg 来自同一官方仓库 public 目录。仅用于模板对照，未替换成第三方照片；品牌与头像属于上游示例，正式业务接入移除。
- 入口阻止所有 form submit；只用于保证模板不会外送输入，不模拟认证。
- 单独加入 reduced-motion 规则关闭非必要动画；不修改区块结构。
- 原 sidebar 宽度 350px、icon 栏与对象栏、折叠、菜单全部保留。无 Resizable。窄屏原对象栏隐藏，原移动 Sheet 只显示导航，不提供邮件列表；下一阶段按已批准计划加入业务对象选择。
- 登录保留原版邮箱字段、GitHub/注册/找回密码及官方 placeholder，以便原样核对；基线确认后的真实认证阶段删除无后端入口并改用户名。
- 主区灰条来自上游 sidebar-09 page 的原始占位内容，不是业务加载中或已实现详情。

## 安装过程已解决的差异

- CLI init 的 `radix-nova` preset 名无效，命令失败未执行初始化；本项目没有切换到 nova。
- `/r/themes/neutral.json` 返回旧主题数据，没有 registry item 的 type，不能直接 add。未使用该 HSL 响应构造近似 v4 主题，改用官方 v4 globals 原文。
- 当前官方 registry 的 `cn` 实际就是 `cn@0.2.6`，CLI utils 为 `export { cn } from "cn"`，未擅自改回另一套工具实现。

## 已执行验证

- `pnpm --dir web build:templates`：通过，包括独立模板 TypeScript 检查。
- `pnpm --dir web typecheck`：通过。
- `pnpm --dir web lint`：通过。
- `pnpm --dir web test`：16 个文件、99 个测试全部通过。
- 脚本对照两 page 与来源快照：仅 import 适配，内容一致。
- 构建 CSS 不含旧 `.workbench`、`.global-nav` 选择器；新入口不导入旧应用。
- 实际浏览器：1440×900 sidebar/login 浅色，390×844 登录单栏、sidebar 深色移动抽屉已检查。
- 使用非真实凭据提交登录：URL 不变、无 /api/ 资源请求、无认证成功界面。
- 表单 Tab 可移动焦点；移动抽屉 Escape 关闭（若提示层打开，先关闭提示层，再关闭 Sheet）。

未执行业务 API、真实认证、模型、部署、后端联合测试；未做完整断点/200%缩放/reduced-motion系统偏好测试或官方在线截图逐像素对比。当前是基线交付，不代表最终业务/部署验收。

## 下一道门槛

用户确认模板结构与样式后，才能接真实登录与监控连接等业务流程。未经授权的 shadcn.io 区块没有安装。没有提交代码、修改实验部署或移除旧前端。
