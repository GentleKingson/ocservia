# ocservia 项目指引

`CLAUDE.md` 通过 `@AGENTS.md` 导入本文件；个人本机偏好放 `~/.codex/AGENTS.md`，勿向仓库提交 `AGENTS.override.md`。

- **边界与自治**：查询、分析与审阅默认只读。修改前检查工作树并保留已有改动；常规小修复在所属模块复用既有抽象做最小改动并保持调用方依赖行为，重构与减负任务优先坍缩退化抽象（单实现接口、多数据库遗留层、冗余转换垫片）并顺手清理受影响的死代码与孤儿脚本。授权内自主推进至验证闭环；丢弃已有工作、破坏性/不可逆操作，以及推送、创建或合并 PR、打标签、发布和部署，均须明确授权。
- **并行子代理**：跨模块只读调研、多系统根因追踪或独立维度审阅可按需并行使用 subagents；代码写入与 `make generate` 保持串行，避免工作区冲突。
- **真相源与文档同步**：代码与协议源码（`proto/`、`openapi/openapi.yaml` 及实现）为实时真相源；文档与代码冲突时不得盲从旧文档。凡改动协议、CLI/环境变量、部署配置或硬契约，须在同一变更中同步更新受影响文档、删除已作废规范，并运行 `make docs-check`。

## 从哪里改

| 位置 | 职责边界 |
| --- | --- |
| `control-plane/internal/` | Go Controller：领域包负责业务不变量与事务，`platform/app` 负责装配（领域包不反向导入）。 |
| `signer/` | 独立签发服务，持有签发密钥与证书账本（仅 HTTPS 通信）。 |
| `rust/crates/` | `transportd` 控制面传输，`agent` 节点会话与命令日志，`privd` 受限特权操作，`upgrader` 软件包升级。 |
| `web/src/` | Vue 控制台、工作区状态与薄封装生成客户端的 `api/*.ts`（避免全局运行时递归转换层）。 |
| `proto/`、`openapi/openapi.yaml` | 协议唯一权威源；禁手改 `control-plane/gen/proto`、`rust/crates/contracts/src/generated`、`web/src/api/generated`。 |

节点二进制、hook 与 systemd 单元由原生 DEB/RPM 管理。部署配置与脚本位于 `deploy/`、`scripts/`；跨模块改动先查[职责与调用方向](docs/architecture.md#ownership-and-task-navigation)。

## 改动必须保留的边界

守住以下外部协议、业务不变量、安全与持久化硬契约的前提下，内部包拆分、未导出符号、路由装配及同版本私有 IPC（`proto/ocserv/platform/transport/v1`、`rust/crates/agent-protocol`）可自由重构：

- **部署与存储范围**：生产仅支持单 Controller、一个 PostgreSQL 18.x 数据库、一个 Signer 和一个专用 Relay。[生产支持范围](docs/operations/production-deployment.md#support-scope)
- **三层独立鉴权与隔离**：HTTP 入口校验会话与 `Origin`，领域服务事务内复验身份、归属与状态，节点独立验证命令签名、`SessionGrantV1` 与连接所有权 fence。保持 Go 无 Iroh、`transportd` 无数据库、`privd` 无网络/数据库/通用 Shell 表面；改一层须保留其余层校验，但**同一进程/同一信任边界内避免重复等价校验**。
- **命令语义与加密转录**：已接受或已派发均不证明执行成功；`unknown` 先按原命令 ID `RECONCILE_ONLY` 对账，保留 Agent journal 与已签名 `privd` 回执。命令签名、语义哈希与回执转录统一使用 RFC 8949 确定性 CBOR（已移除 v1 非 CBOR 格式），禁用 Protobuf 序列化字节。[命令与恢复](docs/reference/stable-contracts.md#commands-and-recovery)
- **数据库迁移与权限**：v2.0.0 从空库建立 Goose 基线，禁止为 pre-v2 旧数据形态新增兼容分支或双重哈希。owner 经 `--migrate-only` 在 PostgreSQL 会话咨询锁内按序执行 `control-plane/migrations/sql/` 的编号迁移。Controller 运行时以受限角色核对 `goose_db_version`。[数据库契约](docs/reference/stable-contracts.md#database)
- **工程克制与启动边界**：非密码学/非完整性场景（如内部相等比较、开发模拟 ID、已比对的临时文件）禁用 SHA-256；启动与热路径避免随历史线性增长的无界 `O(N)` 全量扫描（如全表重算审计链或全账本重验签）。

## 极简测试原则（防膨胀硬纪律）

- **默认不新增测试**：常规功能改动与重构**默认不新增测试文件，只更新受影响的现有测试**；仅当涉及**核心主流程**或**高危安全/数据底线**（三层鉴权隔离、`privd` 提权边界、CBOR 命令签发与 `RECONCILE_ONLY` 幂等对账、PostgreSQL 迁移与最小权限）时才允许补测，且新增前须说明防哪类严重故障。
- **明确禁写清单**：禁止测试纯透传/胶水代码、参数排列组合、UI 样式与组件渲染细节、私有内部实现；禁止同一行为在单测/集成/E2E 跨层重复测试；禁止编写 Shell/CI/部署脚本单元测试（脚本统一靠 `shellcheck`、`bash -n` 与构建烟测）。
- **体量与速度红线**：单测试文件及 Rust 内联 `#[cfg(test)]` 块不超过 **350 行**（硬门槛 **400 行**），单测试函数不超过 **60 行**；禁止在单元测试中写固定 `sleep`（必须用 `testing/synctest`、`tokio::time::pause()` 或事件通知）；禁止在测试循环中重复生成 RSA 密钥或反复建删库；Go PostgreSQL 集成测试一律标注 `//go:build integration`。
- **改删同步清理**：删除或重构业务代码时，必须同步删掉失效、脆弱或跨层重复的旧测试，不留孤儿测试与空 benchmark。

## 验证与交付

从仓库根目录在获准隔离环境中运行最小覆盖检查；缺工具链执行 `scripts/bootstrap.sh <profile>`（如 `go-test`），直接运行 `go`、`cargo`、`npm` 前先在根目录 **Bash** 执行 `source scripts/env.sh`。[验证指引](docs/development/release-checks.md#choose-the-validation-scope)

| 变更 | 验证入口 |
| --- | --- |
| 文档或代理配置 | `make docs-check`、`make policy-check`、`scripts/test-hygiene-check.sh`、`git diff --check` |
| Go（Controller、Signer） | `scripts/go-check.sh standard`、`scripts/transport-boundary-check.sh --go-only`；涉及迁移或数据库再跑 `scripts/database-postgres-smoke.sh` |
| Rust | `make rust-check`、`scripts/transport-boundary-check.sh --rust-only`、`scripts/agent-boundary-check.sh` |
| Web | `scripts/web-check.sh basic` |
| Workflow、脚本或部署 | `scripts/lint.sh source-style`、`scripts/test-hygiene-check.sh` |
| 协议 | 确认生成目录无未提交改动后 `make generate` 并审阅差异；干净提交上跑 `make generated-clean`（`buf breaking` 仅约束外部 `agent/v1`，私有 `transport/v1` 已解绑）。[契约与生成](docs/reference/stable-contracts.md#schema-sources-and-generation) |

- **共享资源与收尾**：共享宿主机时节制并发构建与测试数，重任务按需独占运行；收尾前停止本次启动的后台进程，仅删除本次创建的临时文件，测试临时目录不得写入仓库源码树。
- **完成与发布门禁**：最终回复须自包含且不含内部简写，列出关键改动、实际检查与结果及未验证项。PR `Basic CI Result` 与发布 `Release Check` 以候选 SHA 为准，正式发布须在 `Release Check` 通过后经人工确认打标。[发布策略](docs/development/release-checks.md#release-policy)

## Code Review Rules

- 重点拦截破坏上述硬边界、跨事务/崩溃恢复窗口、命令幂等对账、严重过度工程、同边界过度防御、非密码学 SHA-256 滥用、启动 `O(N)` 全量扫描，以及无充分理由大段新增细碎测试、单元测试引入真实 `sleep`、缺失 `//go:build integration` 标签或拉高全仓测试比例的变更；仅报告有明确触发路径与实际影响的缺陷或验证缺口，格式、lint 与生成一致性交由 CI 裁决。
