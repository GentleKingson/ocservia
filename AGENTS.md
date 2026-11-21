# ocservia 项目指引

`CLAUDE.md` 通过 `@AGENTS.md` 导入本文件；个人本机偏好放 `~/.codex/AGENTS.md`，勿向仓库提交 `AGENTS.override.md`。

- **边界与自治**：查询、分析与审阅默认只读。修改前检查工作树并保留已有改动，在所属模块复用既有抽象做最小改动并保持调用方依赖行为。授权内自主推进至验证闭环；丢弃已有工作、破坏性/不可逆操作，以及推送、创建或合并 PR、打标签、发布和部署，均须明确授权。
- **并行子代理**：跨模块只读调研、多系统根因追踪或独立维度审阅可按需并行使用 subagents；代码写入与 `make generate` 保持串行，避免工作区冲突。

## 从哪里改

| 位置 | 职责边界 |
| --- | --- |
| `control-plane/internal/` | Go Controller：领域包负责业务不变量与事务，`platform/app` 负责装配（领域包不反向导入）。 |
| `signer/` | 独立签发服务，持有签发密钥与证书账本（仅 HTTPS 通信）。 |
| `rust/crates/` | `transportd` 控制面传输，`agent` 节点会话与命令日志，`privd` 受限特权操作，`upgrader` 软件包升级。 |
| `web/src/` | Vue 控制台、工作区状态与手写 `api/*.ts` 适配层。 |
| `proto/`、`openapi/openapi.yaml` | 协议唯一权威源；禁手改 `control-plane/gen/proto`、`rust/crates/contracts/src/generated`、`web/src/api/generated`。 |

部署配置与脚本位于 `deploy/`、`scripts/`；跨模块改动先查[职责与调用方向](docs/architecture.md#ownership-and-task-navigation)。

## 改动必须保留的边界

守住以下外部协议、业务不变量、安全与持久化硬契约的前提下，内部包拆分、未导出符号、路由装配及同版本私有 IPC（`proto/ocserv/platform/transport/v1`、`rust/crates/agent-protocol`）可自由重构：

- **部署与存储范围**：生产仅支持单 Controller、一个 PostgreSQL 18.x 数据库、一个 Signer 和一个专用 Relay。[生产支持范围](docs/operations/production-deployment.md#support-scope)
- **三层独立鉴权与隔离**：HTTP 入口校验会话与 `Origin`，领域服务事务内复验身份、归属与状态，节点独立验证命令签名、`SessionGrantV1` 与连接所有权 fence。保持 Go 无 Iroh、`transportd` 无数据库、`privd` 无网络/数据库/通用 Shell 表面；改一层须保留其余层校验。
- **命令语义与加密转录**：已接受或已派发均不证明执行成功；`unknown` 先按原命令 ID `RECONCILE_ONLY` 对账，保留 Agent journal 与已签名 `privd` 回执。命令签名与语义哈希须用各版本冻结的规范化字节输入，禁用 Protobuf 序列化字节。[命令与恢复](docs/reference/stable-contracts.md#commands-and-recovery)
- **数据库迁移与权限**：`control-plane/migrations/` 仅留 `schema.sql`（空库初始化）和 `upgrade.sql`（自 `v1.2.0` 检查点前向升级）。owner 经 `--migrate-only` 串行迁移并校验来源、内容与历史回执；Controller 运行期仅用受限角色。[数据库契约](docs/reference/stable-contracts.md#database)

## 验证与交付

从仓库根目录在获准隔离环境中运行最小覆盖检查；缺工具链执行 `scripts/bootstrap.sh <profile>`（如 `go-test`），直接运行 `go`、`cargo`、`npm` 前先在根目录 **Bash** 执行 `source scripts/env.sh`。[验证指引](docs/development/release-checks.md#choose-the-validation-scope)

| 变更 | 验证入口 |
| --- | --- |
| 文档或代理配置 | `make docs-check`、`make policy-check`、`git diff --check` |
| Go（Controller、Signer） | `scripts/go-check.sh standard`、`scripts/transport-boundary-check.sh --go-only` |
| Rust | `make rust-check`、`scripts/transport-boundary-check.sh --rust-only`、`scripts/agent-boundary-check.sh` |
| Web | `scripts/web-check.sh basic`；视觉或交互改动按实际展示尺寸核验目标交互（如 `make e2e`） |
| 协议 | 确认生成目录无未提交改动后 `make generate` 并审阅差异；干净提交上跑 `make generated-clean`（`buf breaking` 仅约束外部 `agent/v1`，私有 `transport/v1` 已解绑）。[契约与生成](docs/reference/stable-contracts.md#schema-sources-and-generation) |

- **共享资源与收尾**：共享宿主机时节制并发构建与测试数，重任务按需独占运行；收尾前停止本次启动的后台进程，仅删除本次创建的临时文件。
- **完成与发布门禁**：行为变更须留能检出回归的检查（优先复用现有测试）。最终回复须自包含且不含内部简写，列出关键改动、实际检查与结果及未验证项。PR `Basic CI Result` 与发布 `Release Check` 以候选 SHA 为准，正式发布须在 `Release Check` 通过后经人工确认打标。[发布策略](docs/development/release-checks.md#release-policy)

## Code Review Rules

- 重点拦截破坏上述四项硬边界、跨事务/崩溃恢复窗口或命令幂等对账的变更；仅报告有明确触发路径与实际影响的缺陷或验证缺口，格式、lint 与生成一致性交由 CI 裁决。
