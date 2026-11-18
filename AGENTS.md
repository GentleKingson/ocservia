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

- 生产支持单 Controller、一个 PostgreSQL 18.x 数据库和一个专用 Relay。部署拓扑以[生产支持范围](docs/operations/production-deployment.md#support-scope)为准。
- HTTP 入口检查调用者权限，领域服务在事务内复验相关状态与归属，节点验证命令签名和连接所有权 fence。修改一层时须保留其他层的校验。
- 命令已接受或已派发都不足以证明执行成功。结果为 `unknown` 时先按原命令 ID 对账，保留 Agent journal 和已签名的特权执行回执。命令签名与语义哈希使用各版本规定的规范化输入，不能改用 Protobuf 序列化字节。[命令与恢复](docs/reference/stable-contracts.md#commands-and-recovery)
- 在 `control-plane/migrations/` 中，`schema.sql` 用于空库初始化，`upgrade.sql` 用于已有库的前向升级。迁移由 owner 串行执行，校验 schema 来源、内容和历史回执。Controller 日常运行使用独立的受限角色。[数据库契约](docs/reference/stable-contracts.md#database)

## 怎样验证

从仓库根目录选择覆盖变更的最小检查，在获准且具备依赖的隔离环境中运行。缺少工具链时按宿主和任务选择 bootstrap profile，例如 Go 检查用 `scripts/bootstrap.sh go-test`。[验证指引](docs/development/release-checks.md#choose-the-validation-scope)

直接运行 `go`、`cargo` 或 `npm` 前，在仓库根目录的 **Bash** 会话中执行 `source scripts/env.sh`，再进入目标模块。

| 变更 | 验证入口 |
| --- | --- |
| 文档或代理指令 | `make docs-check`、`make policy-check`、`git diff --check`，并核对改动的链接和锚点。 |
| Go（Controller 和 Signer） | `scripts/go-check.sh standard` |
| Rust | `make rust-check` |
| Web | `scripts/web-check.sh basic` |

修改协议时编辑源文件。`make generate` 会重建 `control-plane/gen/proto`、`rust/crates/contracts/src/generated` 和 `web/src/api/generated`。先检查这三个目录的未提交工作，再生成并审阅差异。在已提交的干净候选上运行 `make generated-clean` 验证可重复生成。[契约与生成](docs/reference/stable-contracts.md#schema-sources-and-generation)

交付时列出实际运行的检查、结果和未验证项。行为变更须有能检出回归的检查。PR 的 `Basic CI Result` 以候选 SHA 的运行结果为准。发布须有对应 SHA 的 Release Check，RC 的三台虚拟机验收证据还须与标签 SHA 核对并由人工确认。[发布策略](docs/development/release-checks.md#release-policy)
