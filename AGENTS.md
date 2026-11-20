# ocservia 项目指引

仓库通用规则在本文件维护，`CLAUDE.md` 通过 `@AGENTS.md` 导入。查询、分析和审阅默认只读。实施前查看工作树差异并保留已有修改，沿调用路径在负责该行为的模块做最小改动。丢弃已有工作、破坏性操作，以及推送、创建或合并 PR、打标签、发布和部署，都须有明确授权。

## 从哪里改

| 位置 | 职责 |
| --- | --- |
| `control-plane/internal/` | Go Controller。领域服务负责业务不变量和事务，`control-plane/internal/platform/app` 负责装配。 |
| `signer/` | 独立的签发服务，持有签发密钥和证书账本。 |
| `rust/crates/` | `transportd` 负责控制面传输，`agent` 负责节点会话和命令日志，`privd` 执行受限特权操作，`upgrader` 负责软件包升级。 |
| `web/src/` | Vue 控制台、工作区状态和 API 适配层。 |
| `proto/`、`openapi/openapi.yaml` | 协议字段的权威来源。 |
| `deploy/`、`scripts/` | 安装与部署配置、构建和验证脚本。 |

跨模块改动先查[职责与调用方向](docs/architecture.md#ownership-and-task-navigation)。

## 改动必须保留的边界

以下为必须长期保持的硬契约（外部协议、业务不变量、安全边界与持久化格式）；Go/Rust/Web 内部包与目录拆分、未导出函数名、内部路由注册细节、以及同版本配对的私有 IPC（`proto/ocserv/platform/transport/v1` 与 `rust/crates/agent-protocol`）均允许在保留上述边界的前提下自由重构：

- **部署与存储范围**：生产仅支持单 Controller、一个 PostgreSQL 18.x 数据库、一个 Signer 和一个专用 Relay。部署拓扑以[生产支持范围](docs/operations/production-deployment.md#support-scope)为准。
- **三层独立鉴权与进程隔离**：HTTP 入口检查调用者会话与 `Origin`，领域服务在事务内复验身份、归属与状态，节点独立验证命令签名、`SessionGrantV1` 和连接所有权 fence。保持 Go 不依赖 Iroh、`transportd` 无数据库依赖、`privd` 无网络/数据库/通用 Shell 表面；修改一层时须保留其他层校验。
- **命令语义与加密转录**：命令已接受或已派发均不证明执行成功；结果为 `unknown` 时先按原命令 ID `RECONCILE_ONLY` 对账，保留 Agent journal 与已签名 `privd` 回执。命令签名与语义哈希必须使用各版本冻结的规范化字节输入，绝不改用 Protobuf 序列化字节。[命令与恢复](docs/reference/stable-contracts.md#commands-and-recovery)
- **数据库迁移与权限**：`control-plane/migrations/` 仅保留 `schema.sql`（空库初始化）和 `upgrade.sql`（从固定 `v1.2.0` 检查点前向升级）。迁移由 owner 通过 `--migrate-only` 串行执行并校验来源、内容与历史回执；Controller 运行期仅使用受限角色。[数据库契约](docs/reference/stable-contracts.md#database)

## 怎样验证

从仓库根目录选择覆盖变更的最小检查，在获准且具备依赖的隔离环境中运行。缺少工具链时按宿主和任务选择 bootstrap profile，例如 Go 检查用 `scripts/bootstrap.sh go-test`。[验证指引](docs/development/release-checks.md#choose-the-validation-scope)

直接运行 `go`、`cargo` 或 `npm` 前，在仓库根目录的 **Bash** 会话中执行 `source scripts/env.sh`，再进入目标模块。

| 变更 | 验证入口 |
| --- | --- |
| 文档或代理指令 | `make docs-check`、`make policy-check`、`git diff --check`（自动校验全仓 Markdown 相对链接与锚点）。 |
| Go（Controller 和 Signer） | `scripts/go-check.sh standard`、`scripts/transport-boundary-check.sh --go-only` |
| Rust | `make rust-check`、`scripts/transport-boundary-check.sh --rust-only`、`scripts/agent-boundary-check.sh` |
| Web | `scripts/web-check.sh basic` |

修改协议时编辑源文件。`make generate` 会重建 `control-plane/gen/proto`、`rust/crates/contracts/src/generated` 和 `web/src/api/generated`。先检查这三个目录的未提交工作，再生成并审阅差异。在已提交的干净候选上运行 `make generated-clean` 验证可重复生成；`buf breaking` 仅对外部 `agent/v1` 施加 `FILE` 级跨版本检查，同版本私有 IPC `transport/v1` 已解绑。[契约与生成](docs/reference/stable-contracts.md#schema-sources-and-generation)

交付时列出实际运行的检查、结果和未验证项。行为变更须有能检出回归的检查。PR 的 `Basic CI Result` 以候选 SHA 的运行结果为准。发布须有对应 SHA 的 Release Check，RC 的三台虚拟机验收证据还须与标签 SHA 核对并由人工确认。[发布策略](docs/development/release-checks.md#release-policy)
