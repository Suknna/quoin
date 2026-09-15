# 认证与审计整改执行检查点（2026-09-15）

状态：**实现、整仓回归、r4 部署与全部复验已完成；未执行的仅剩显式交付边界（外部送达、正式生产恢复/回滚、双架构资格），不可作为对外发布声明。** 执行基线 `3ebed73`；所有变更未提交、未推送。

## 已完成的真实部署与浏览器检查

详见 [直接浏览器验收记录](acceptance/auth-audit-20260915/README.md)。

- 本次旧 `quoin-e2e-admin-init` Compose 容器、卷与运行目录已清理。没有继续编写或运行 E2E 脚本。
- 采用 `deploy/images/build.sh` 和 `deploy/kubernetes`，在独立命名空间 `quoin-auth-acceptance` 启动五个产品服务。Plinth 经管理员页面签发完整 JSON 和 CLI stdin 正常注册，重启后使用原长期凭据自动重连。
- 管理员初始化、真实 SMTP OTP、初始化后返回登录、正式双因素登录通过。
- 管理员创建操作员并指定收码目标；操作员初始化、刷新恢复、返回登录、正式密码加新 OTP 登录通过。
- 操作员无管理入口，直接访问审计被拒绝，收码目标不可自行修改。
- 审计按 `user.create` 查询得到迁移前的创建事件；访问与业务执行共享关联 ID。默认六个月，五个月不可提交；预览七个月无删除影响后调整成功。
- 管理员页面进入升级维护、自动备份 Safe 后，停机 Job preflight 与迁移成功。精确开发 schema `7bf60907…` 转换为 `98ea729c…`，保留账户、正式会话、联系方式与审计历史；未清空数据卷。
- 临时数据库检查 Pod、完成迁移 Job、空探针文件、未使用 E2E 脚本/辅助文件和本次旧数据库副本已清理。临时 SMTP 接收端仍保留且标记为 test-only，以维持隔离验收登录能力；不属于默认产品清单。

## 已通过的代码检查（对应当时快照）

- 前端 typecheck、lint、Vitest 及生产 build 通过；最新主代理运行仍全部通过。没有将 mock 组件测试当成真实后端验收。
- auth、app、recovery、execution、cmd/quoin、bootstrap、upgrade 的联合定向检查通过；其后的收口修改已由最终整仓全量回归覆盖（见下文）。
- 新增 observation/embedding 空闲扫描回归通过：没有实际工作不产生业务成功事件；事务内复检防竞争。错误不得吞掉后伪造成功。
- 精确 schema 简化迁移测试与 upgrade 全包通过；验证已初始化管理员、密码/revision、有效会话、联系方式、审计、AUTOINCREMENT 高水位保留，以及错误维护/伪造台账失败时不改数据。
- 核心只读审查发现跨用户 contactId 发送与混合 Cookie 准入问题；主代理复现失败并修复，auth/operations 全包通过。审查者重新确认两处修复。
- maintenance 离线 rebind/恢复已用共享执行器，真实只读 gate、审计失败回滚和重放测试通过。

## 后续核心边界复核

- `execution.Executor` 已改为具体 `*execution.Tx` 别名。单纯的私有接口标记仍可通过嵌入包装转发裸写；新增反向测试覆盖了这一实际绕过方式。
- `execution.Reader` 仅由 `OpenReadOnly` 工厂创建，内部连接池不可获取；每个连接都由 DSN 强制 `mode=ro` 和 `query_only(1)`。执行器拒绝任意 `*sql.DB` 和自定义包装，不再将一次连接级 PRAGMA 检查当成整个池的只读证明。
- 新增混合连接池反例：一个连接返回 `query_only=1`、另一个仍可写，整个池仍被拒绝。只读快照提供查询与结束快照的方法，不提供裸连接、Exec、ATTACH 或可修改 PRAGMA。
- 核心两项 P1 经审查者再次复核通过。执行器、bootstrap、backup 定向全包检查通过；认证、执行器、审计、投递、Attempt、Runtime、备份、恢复八个关键包的 race 检查通过。上述结论不替代其他业务包最终验收。
- 初始审计保留期与唯一管理员创建合并到同一自动审计事务；审计失败共同回滚，重启不覆盖保留设置、不重复记录种子事件。
- Artifact GC 的持久删除意图先于 unlink 与目录 fsync 提交；删除与目录同步结果分别记录，完成不确定绝不伪造成功。意图提交后重新检查引用，出现新引用时不删除；每次物理尝试使用新关联，不重放旧意图。

## 最终全量回归（冻结前快照）

- `go test ./... -count=1` 全部 75 个含测试包通过、0 失败（`/tmp/quoin-final-go-all-verified.log`）。此前一次运行中 artifact `TestGCRechecksReferencesAfterIntent` 因夹具缺少 `expires_at` 失败，修复后复跑通过。
- `go vet ./...` 无输出无发现；架构与契约门禁 `internal/contract` 通过；生成工件差异检查为空（无差异）。
- race 检查：关键八包（auth、execution、audit、delivery、attempt、runtime、backup、recovery）全部通过；alerts、maintenance、upgrade 附加 race 通过；artifact race 复跑通过（114.802s）。此前 artifact+backup 合并 race 运行中 artifact 失败（同一夹具问题）、backup 同跑通过（176.103s），该失败已被复跑取代。
- web typecheck、lint、37 个测试文件 / 269 个用例、frozen-lockfile 安装与生产构建全部通过。
- 早前一次生产构建曾暴露 app 包两处编译/方法缺失错误，已在最终运行前修复。

## r3/r4 部署与最终浏览器验证

详见 [直接浏览器验收记录](acceptance/auth-audit-20260915/README.md)。

- r3：`deploy/images/build.sh` 四个默认镜像（frontend、quoin、plinth、stele）滚动更新至 `quoin-auth-acceptance`，全部工作负载 Ready，原有 3 个 PVC 标识未变；管理员/操作员双因素登录、权限拒绝、渠道只读、Plinth 第 1 代连接与七个月保留复验通过。
- 最终审查发现审计 ObjectType+ID 对象定位权威缺陷（r3 部署中部分登录事件的对象类型与编号配错，操作者身份未改变）；修正进入 r4 冻结源，仅后端镜像为 r4（最后补丁无协议变化，其余镜像保持 r3）。r4 已滚动更新：6 个工作负载全部 Ready，3 个 PVC 标识未变；quoin pod 镜像 `sha256:dba92c33f5806ecd933e77558882fd63c7a9e80a1b37cf43b9aa6ce5ec47ff47`。
- r4 真实 GUI 复验通过：管理员正式密码→OTP 阶段（不进工作台）→显式发送经本地 TLS SMTP→新验证码→进入工作台；admin1/operator2 保持启用已初始化，七个月审计保留不变。新登录关联 `dab57ad0a0aa18b94763f9042ceca8d8` 含 6 个事件，主体/流程权威正确且无历史改写。关于页显示 `auth-audit-20260915-r4`、Plinth registered/connected（r3 第 1 代）、维护态未维护；运行时面板正常。

## r4 最终复跑确认（已收口）

- r4 冻结源整仓 `go test` 全部 75 包通过、exit 0（`/tmp/quoin-r4-go-all-verified.log`）；`go vet` 与空白差异检查通过（`/tmp/quoin-r4-vet-verified.log`、`/tmp/quoin-r4-diff-verified.log`）。首轮全仓运行仅新增 auth 夹具守卫失败、其余 74 包通过，夹具修正后复跑全绿；最终新增回归覆盖会话活动聚合与不等 ID 定向撤销拒绝，突变回退已证明测试能发现该缺陷。Standards/Spec 终审无未决问题，此前 P1 缺测试项以新增回归关闭。
- r4 auth+operations race 通过、exit 0：auth 115.923s、operations 1.138s（`/tmp/quoin-r4-auth-race-verified.log`）。至此实现、测试、浏览器与部署复验全部完成，无剩余实现或验证待办。

## 显式边界（未执行，非缺陷）

- 外部邮件/短信网关送达未授权、未验收；本地 TLS SMTP 夹具不是产品依赖。
- 正式生产恢复/回滚演练与双架构发布资格未执行；既有隔离 Kubernetes 迁移证据继续有效。
- 非部署 CI 入口文件已新增但未在 CI 中运行验证。
- 前端最新 typecheck、lint、37 个测试文件 / 269 个用例、生产构建通过；重新生成 API 类型没有产生差异。

## 证据边界

当前部署是本地隔离验收，不代表对外发布、外部邮件/手机实际送达或双架构发布资格。既有隔离 Kubernetes 迁移证据继续有效，本轮未新增生产恢复/回滚或双架构正式资格结论；退役 Lintel 和浏览器普通路由保持关闭，活动排空路径由执行器承接。未操作其他命名空间或既有用户数据卷；没有提交、推送或创建远程 PR。
