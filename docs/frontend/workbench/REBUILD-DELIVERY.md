# 新前端重构交付与联合验收

## 范围与授权

按本会话确认，改为完成代码重构后统一验收。未取得 shadcn.io 源码/许可的候选已获明确授权使用官方组件组合替代，不仿制第三方截图。保留完整官方 sidebar-09、login-02 基线及来源快照。

## 唯一正式入口

- 源码：`web/src/main.tsx`、`web/src/app/App.tsx`。
- 外壳：`WorkspaceShell.tsx`，原 sidebar-09 的窄图标栏、第二栏对象列表、350px 尺寸体系、顶部位置；窄屏官方 Sheet。
- 路由：history API、业务路径深链、未知路径 Empty 404、按域 lazy 加载。
- 正式构建：`pnpm --dir web build`，输出 `internal/gen/web/dist` 供 Go embed。
- 本机已启动 HTTPS 联调入口：`https://127.0.0.1:18443/`。使用现有实验 CA 签发的短期本机证书；没有关闭上游校验。
- 官方模板对照独立在 `web/src`，不作为正式业务入口。
- 旧 `web/src` 的页面、布局 TSX 和 CSS 已移除；保留被新入口使用的 API、类型、流协议、纯逻辑及相应测试。旧视觉测试随实现删除，实时测试迁为 hook harness。

## 页面/组件矩阵

所有组合来自 `https://ui.shadcn.com/docs/components/radix/` 对应组件；CLI 固定 `shadcn@4.21.0`，样式 new-york-v4，源码和MIT见 `../templates/sources/`。未安装第三方block。

| 区域 | 新实现 | 官方UI归属 |
|---|---|---|
| 全局、认证 | 登录/强制改密、Session失效保护、账户切换、维护协调、404/不可用 | sidebar-09/login-02；Dialog/Alert/Empty/Button/Field/Input |
| 告警 | 当前/历史/intake、系统筛选、详情/observations、确认、初步分析/重试/取消、证据与调查来源 | Tabs/Select/Accordion/Badge/Separator/Alert/Dialog/Button |
| 调查 | 首条原子创建、来源、附件、消息、流式增量、Tool Call、Stop/Retry/Undo、反馈/候选 | Field/Textarea/Input/Dialog/Collapsible/Accordion/AlertDialog/Button |
| Evidence | 完整性、来源参数、Artifact、字段化内容与折叠原始数据，返回来源上下文 | 全视口Dialog/ScrollArea/Table/Accordion/Badge |
| 巡检 | 运行选择、连续详情、检查项/gap、报告版本、引用、反馈/候选、重新分析/采集 | Dialog/Select/Accordion/Separator/Alert/Badge/Button |
| 业务系统 | YAML上传与字段错误、版本/diff、验证/发布、资源及刷新、K8s映射、Browser Identity/noVNC | Field/Input/Select/Table/ScrollArea/Accordion/AlertDialog；diff@9/noVNC专用能力 |
| 知识 | FTS/语义分组、候选编辑/确认/排除、文本导入批次、版本选择/修订/停止复用 | Field/Input/Textarea/Checkbox/Select/Table/Accordion/AlertDialog |
| 连接 | Thanos/Kubernetes/model_provider创建、发现、探测/取消、启停、轮换、revision/generation | Field/Input/Textarea/Checkbox/Command/Popover/Select/AlertDialog |
| 管理 | 用户/角色/密码重置、Runtime注册与退休、告警源凭据、Label上传/readiness/激活、Journey、备份/保留/下载、维护/部署验证、审计 | Table/Field/Input/Select/Accordion/Badge/Alert/Dialog/AlertDialog/Button |
| 个人 | 只读资料、修改密码、我的Session分页与撤销、审计 | Field/Input/Table/Alert/AlertDialog/Button |

源组件组合不代替领域状态机：写操作依照现有 API，秘密仅内存显示；版本/命令fence、合法取消与原子发布由后端裁决。不新增 enabled YAML独立开关、在线恢复、模型数据实体或Journey激活接口。

## 已执行自动化检查

- 正式 `pnpm --dir web build`：通过。
- `pnpm --dir web lint`：通过。
- `pnpm --dir web typecheck:workbench`：通过。
- `pnpm --dir web test`：20文件、79测试通过。
- 路由级分包：入口约203KB（gzip约65KB），最大延迟系统模块约216KB；不再通过调大阈值隐藏首包告警。
- `git diff --check`：通过。
- 根 `.env` 忽略有效，未被跟踪；内容和密钥未写入本交付文档或正式前端产物。
- 未手改生成API类型、未修改后端领域契约；无提交或推送。

独立审阅已发现并修复过：管理/知识路由前缀、分析retry误用、Browser Identity首次配置、备份下载/设置、Label激活、Runtime秘密晚到回写、账户当前会话撤销、连接切换旧对象误操作、登录与连接字段重复ID等。自动化检查不是全业务正确性证明。

## 联合验收仍须逐项完成

用户要求本轮先完成重构、再共同验收，以下不冒充已通过：

1. 全六模块的桌面/窄屏、键盘焦点、主题、reduced-motion与返回位置视觉验收。
2. 真实登录/Session撤销/权限/维护分流矩阵。此前登录与Thanos创建/探测/冲突已有记录，不能代替重构后的全量复验。
3. DeepSeek：根 `.env` 已授权作为测试材料；其中没有独立embedding配置。当前服务端创建要求embedding模型，不能把对话模型冒充embedding，也不能用fixture模型报告冒充通过。
4. 仅通过页面完成监控/模型→YAML验证发布→巡检→Evidence/报告的完整闭环；还需真实模型能力条件。
5. 告警→分析→调查、工具/流取消/重试/撤回、知识导入/确认/修订的真实失败与冲突场景。
6. noVNC、Kubernetes、备份下载、Runtime/Label原子激活的实际依赖和秘密边界。
7. 新前端已写入正式静态构建目录，但没有构建/推送/切换新镜像；部署版本及数据保护/回退验收尚未执行。

不能以源码覆盖、79个测试或构建成功代替以上验收。实验环境已恢复，既有 local-prometheus 仍启用，验收连接 ui-validation-20260908 仍未启用；没有为测试擅自切换它们。
