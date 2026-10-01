# Current contracts and acceptance

## v1.1.0 transition

The published [v1.1.0 policy reset](support-policy.md#v110-policy-reset)
supersedes the software-version windows, historical release matrix and schema
compatibility admission of the published 1.0 policy. Software-version guards
and historical entrypoints have been removed. The
[release notes](https://github.com/GentleKingson/ocservia/releases/tag/v1.1.0)
record that release's acceptance; they do not certify later candidates.
Real command/protocol support, authorization,
artifact integrity, durable state, migration content integrity and current
readiness remain contracts, not substitutes for a software-version fence.

## Current candidate scope

Validate the exact candidate artifacts on supported architectures, deployment
modes and database products. Matching version labels, unchanged protocol
numbers and green source tests do not establish arbitrary cross-version safety.
The [published 1.0 inventory](https://github.com/GentleKingson/ocservia/blob/d420b22018596d6741d55fa56bd19c4a767e5817/docs/reference/stable-contracts.md)
retains the historical matrix, exclusions and results; its removed entrypoints
are not current requirements. Existing published artifacts are unchanged.

## Contract owners

| Surface | Candidate stable contract and authority | Not a public extension interface |
| --- | --- | --- |
| HTTP | [`openapi.yaml`](../../openapi/openapi.yaml): `/api/v1` resources, request/response schemas, strict JSON, authentication, authorization, idempotency, revisions, Problems, pagination and SSE cursor behavior. Preserve the [HTTP baseline](../development/http-baseline.md), including documented error/method precedence. Health/version aliases retain their current behavior. | Go handler/package layout, Web stores, component APIs and generated-client implementation. `/dev` and simulator routes are development-only. |
| CLI and configuration | Documented Controller `--role`, `--migrate-only`, `OCSERV_*` and exclusive `_FILE` settings; managed-node `agent.env`, `privd.env`, `relays.env` and one-shot enrollment/attestation commands. [Production deployment](../operations/production-deployment.md) and [Agent lifecycle](../operations/agent-lifecycle.md) own names, defaults, secret permissions and failure behavior. | Undocumented flags, debug/probe modes, test environment variables, log wording and local SQLite table access. |
| Installation and upgrade | Exact release pins; `deploy/production/install.sh`, `controller-bootstrap.sh`, `controller.sh`; `deploy/managed-node/install.sh`; verified node install/upgrade/rollback scripts. Preserve deployment configuration validation, HTTPS downloads, authorized upgrade digests, runtime trust, same-target retry and protected confirmed/pending state. | Direct Compose/image replacement, source-tree node installation, unsafe archive extraction and arbitrary package-manager downgrades. |
| Package names | `ocservia-agent-X.Y.Z-linux-{amd64,arm64}.tar.gz` plus plain checksum; new DEBs `ocservia-agent_X.Y.Z-1_{amd64,arm64}.deb`; RPMs `ocservia-agent-X.Y.Z-1.{x86_64,aarch64}.rpm`; `controller-release-{amd64,arm64}.json` and existing amd64 alias `controller-release.json`. | Crate versions such as `0.1.0`, build-directory names and image config IDs as registry manifest digests. |
| Schema and transport | [`proto/`](../../proto/) owns message numbers, enums and services. ALPNs remain `ocserv-platform/enroll/1` and `ocserv-platform/agent/1`; enrollment proof is 1.1, session protocol is negotiated separately. Go/transportd UDS and Agent/privd local protocol are private matched-release interfaces. | Direct Go access to Iroh; arbitrary third-party privd clients; automatic acceptance of new signed-command fields because Protobuf normally permits them. |
| Signing and hashes | [Command authorization v1](../development/command-authorization-v1.md), session/artifact grants, connection fence v2, fence binding v2 and [privd receipt v1](../development/agent-privd.md) have frozen canonical transcripts. [Semantic v1](../development/command-semantic-hash-v1.md) remains frozen; [v2](../development/command-semantic-hash-v2.md) is current Controller issuance. Proto serialization is never canonical signing input. | Changing an existing hash/transcript to absorb new semantics; inferring semantic v2 from the historical capability string `command.semantic-hash.v1`. |
| Versions and durable state | Controller/transportd use one candidate source/release; Agent/privd/upgrader are one verified package. Cross-version execution has no compatibility guarantee. Preserve journals, root effect store plus HMAC/receipt keys, endpoint identity and revision/fence floors. | Independent Agent/privd swaps or numeric version classification as permission to dispatch. |
| Database | [Migration contract](../development/control-plane.md): owner-only serialized migrations, restricted runtime role, execution receipts, known content integrity and actual SQL/dirty-error handling. PostgreSQL schema numbers are not MySQL/MariaDB migration revisions. | Generic down-migration rollback, automatic force-clean, cross-engine migration, or treating additive DDL as automatically backward compatible. |

Generated code is disposable output of Proto/OpenAPI. Use the existing
`scripts/check-breaking.sh`, `scripts/generate.sh` and
`scripts/generated-clean.sh`; do not patch generated clients or relax the
explicit historical security-migration allowlist. The breaking command compares
with `origin/main`, not with every shipped release. Release-level behavioral
evidence remains necessary even when it passes.

## Approval principal boundary

Baseline 1.0 and Business Smoke require **independently controlled requester and approver
principals**, not an organizational four-eyes or two-person rule. Each principal
must authenticate with its own credential and session and satisfy workspace
authorization and RBAC. Sensitive-operation approval remains bound to the exact
request/hash and resource/revision context; self-approval returns 403 and replay
must not expand or reuse consumed authority. Different identity IDs without
independent authentication are not acceptance evidence. The
[Local bootstrap procedure](../operations/authentication.md) provisions separate
principals; it does not attest to the number of people controlling them.

An automated run may exercise both principals through normal authentication
and isolated sessions. Record this as simulated two-principal acceptance,
never as verified human custody. Actual custody by two different people is an
additional production-hardening or enterprise security profile, excluded from
Business Smoke and requiring separate human evidence when a deployment selects it.
This reviewed scope does not remove RBAC, approval binding, self-approval
rejection, replay protection or approval requirements from runtime behavior.

## Session and command matrix

This is a behavioral policy matrix, not a list of already accepted release
pairs. [`enrollment.Service.AuthorizeSession`](../../control-plane/internal/enrollment/service.go),
[`transportd`](../../rust/crates/transportd/src/lib.rs),
[`Agent`](../../rust/crates/agent/src/main.rs) and privd own runtime admission.

| Offered state | Required behavior / upgrade path | Existing verification |
| --- | --- | --- |
| Session major other than 1; minor newer than 1 | Controller returns `INCOMPATIBLE_PROTOCOL` or `UPGRADE_REQUIRED`; transport admission also rejects unsupported protocol. Upgrade Controller/transportd before introducing a newer node protocol. | Enrollment trust lifecycle and transport handshake tests. |
| Grantless 1.0 | Read-only only: status, version, sessions, IP bans and config fingerprint. No mutation, artifact transfer or inferred future `.read` capability. Upgrade rather than forge a grant. | `TestEnrollmentBackendIntegration/legacy-capability-policy`, Agent `agent_accepts_only_explicit_grantless_read_only_sessions`, transport static-handshake and shared session-policy tests. |
| 1.1 without sealing descriptors | Controller has a legacy read-only fallback, not permission to mutate. Wrong descriptors are rejected. | `TestExistingActiveNodeBindsSealingKeysOnceIntegration`, enrollment lifecycle, Agent signed-read-only tests. |
| 1.1 missing/invalid grant, wrong capabilities or stale owner fence | Reject; refresh authorized session/owner term. Fencing capability alone is not a grant or approval. | Existing session/fence signing goldens and Agent/transport authority tests. |
| Privileged operation without registered receipt capability/key | No production legacy-success mode. Initialize/register root receipt key, upgrade the matched node package, then verify negotiated capability before dispatch. | Attestation safety and missing-root-receipt tests. |
| Unknown command field, wrong wire type or truncated nested message | Reject before journal/effect, including when signature/hash otherwise looks valid. | Shared strict-wire corpus and descriptor-drift checks, not a second wire suite. |
| Uncertain non-idempotent mutation, including matched releases | No guaranteed automatic recovery without exact authenticated durable evidence. Keep Unknown and pause conflicting writes for operator reconciliation; connectivity is not completion. | [Matched-release recovery boundary](#matched-release-recovery-boundary); strict recovery probes retain their original outcomes. |
| Semantic hash v1/v2 | Recompute the declared known version; current Controller emits v2. Unknown/missing/mismatched hashes and same-id cross-version journal conflicts fail closed. Retain old history; never rewrite stored v1 to v2. | Shared semantic goldens and Agent v1/hash-version-conflict tests. |
| Config revision drift | Authorization revision and applied config revision are independent. Plan binds actual `config_expected_revision`; Apply binds candidate/current hashes and desired revision. Reject stale/gapped effects; recover from exact durable evidence. | Existing ConfigPlan lookup/Apply tests and Agent/adapter revision, restart and recovery tests. |
| Historical/unattested CSR or lost P12 credentials | No new signing from migration-legacy CSR; obtain a fresh attested CSR. P12 remains encrypted, bounded and one-use. Web credentials exist only in the current SPA memory/expiry window, not across refresh or another tab. | Certificate/secret integration, real OpenSSL adapter tests and Web certificate receipt/recovery tests. |

The historical 18-payload strict-wire corpus, Go reflection, Rust descriptor
mutation coverage, signing/fence/receipt goldens, and existing Go/Rust tests are
retained; complete configuration payloads add their own strict-wire cases.
See [focused commands](../development/testing.md#command-wire-contracts).
Do not interpret fixture success as execution of an old published Agent.

## Current application acceptance

Use [Release Check](../development/release-checks.md) for actual current-product,
native package, business, integration, security and selected resilience gates.
There is no historical native upgrade, mixed-version or migration matrix.
Lower/equal/higher target regression fixtures test the operation mechanism,
not an old-release support promise.

Nodes implementing `ocserv.config.complete.plan` and
`ocserv.config.complete.apply` use the
[complete node-local TLS profile](../operations/node-local-config-tls.md).
Apply remains reload-only: startup authentication/listener/worker/socket/TLS
bindings must match protected active configuration. Initial activation and
TLS version/path changes need an explicitly authorized maintenance restart.
Actual VPN authentication, exact rollback and durable recovery remain acceptance
requirements; native parser or occtl health alone is insufficient.

### Matched-release recovery boundary

**Automatic recovery of an uncertain non-idempotent mutation is not guaranteed
even for matched releases**. Matching Controller/transportd and Agent/privd/upgrader artifacts does
not establish that an interrupted reload completed, or that it is safe to
execute it again. Online node status, a new fenced session and healthy ocserv
are not evidence of the historical command's outcome.

Existing query-only reconciliation may recover an exact authenticated root
response or an already terminal journal result. This is not permission to
infer success from current service health, absence of a log line or the mere
presence of a database row. Preserve and compare the exact command ID,
semantic hash and hash version, operation/idempotency identity, authorization
revision, owner/fence history, Agent journal and root effect/receipt. Verify
the receipt authority and its bindings before accepting a terminal result;
missing or conflicting evidence leaves the operation `Unknown`.

Until operator reconciliation resolves the uncertainty, pause subsequent
conflicting writes to the affected resource. This is an operational requirement,
not a claim that the API implements a new resource-wide lock. Do not clear the
journal or root effect store, edit durable state, reset endpoint identities,
resend the original mutation, or use the same or a new idempotency key to guess
the result. Read-only recovery queries are not mutation retries. The existing
explicit `RetryIfEffectAbsent` path remains available only after its required
effect-absence proof succeeds; an offline or healthy service alone does not
supply that proof. A package upgrade is not reconciliation. Follow the existing
[incident recovery procedure](../operations/incident-recovery.md#transport-and-credentials).

Keep strict automatic-recovery probes unchanged. A green attempt does not
erase an earlier Unknown, and a failed strict aggregate remains failed. A
separate scoped `EXPECTED-UNKNOWN` assessment requires retained evidence of
the exact unresolved command, query-only behavior, preserved identity/state,
no duplicate mutation and paused conflicting writes. Missing observations are
NOT RUN or BLOCKED, not an expected-result waiver. Restoring a guaranteed
automatic-recovery promise requires reviewed durable-evidence semantics and
exact-candidate fault/restart acceptance, not repeated runs or longer timeouts.

## Database and recovery matrix

[Production database support](../operations/production-deployment.md#database-support)
remains authoritative. All rows use Controller `linux/amd64` and `linux/arm64`
artifacts; this does not certify an external server's OS/architecture. Record
the actual server, client and backup image versions/digests per acceptance run.

| Version and deployment | Existing verification entry | Recovery and uncovered scope |
| --- | --- | --- |
| PostgreSQL 17 bundled (current fixture 17.10) | `PG_MAJOR=17 DATABASE_TEST_SCOPE=smoke scripts/database-integration.sh`; `scripts/i18-backup-restore-smoke.sh` | Verified base backup and isolated restore; restart coverage for the same instance. No cluster, automatic failover or PITR readiness claim. |
| PostgreSQL 17 external | Same backend checks plus `scripts/i18-external-postgres-backup-restore-smoke.sh` | Verified TLS owner/runtime/backup connections and base restore. No automatic managed external HA/PITR. |
| MySQL 8.4.10 external | `ENGINE=mysql DATABASE_TEST_SCOPE=smoke bash scripts/database-foundation-integration.sh`; `ENGINE=mysql scripts/i18-mysql-backup-restore-smoke.sh` | Backend-specific logical backup/isolated restore. No bundled deployment, cross-engine migration or PostgreSQL HA/PITR claim. |
| MariaDB 12.3.2 external | Same two entries with `ENGINE=mariadb` | Same logical-restore boundary, independently tested engine, not an alias for a MySQL pass. |
| PostgreSQL 18 CI; bundled MySQL/MariaDB | PG18 existing CI remains; bundled MySQL/MariaDB launcher rejects | Not production support. Keep tests and production admission distinct. |

T02 identified same-line patch candidates (PostgreSQL 17.11, MySQL container
8.4.12/native 8.4.11 plus vendor patches, MariaDB 12.3.3). They are **pending
compatibility and artifact review**, not replacements for the existing support
rows. T09 owns final image pins/inventory; Business Smoke owns actual deployment evidence.
Verify migration/permissions and matching backup/restore before accepting a
patch set. Do not substitute PG18/G6 or silently remove old rows. SQLite
journal WAL-reset applicability and upgrade recovery remain T06/T08 work;
do not reset journals to make an upgrade pass.

## CI and rollout decisions

Run manual Release Check on merged main and require Full CI, Security and
Integrated Business Smoke with single-instance recovery PASS. The operator then confirms the
version and creates its tag; formal Release builds, scans the exact Controller
images and smoke-tests both native architectures before publication. Historical compatibility gates are retired,
not skipped successes.

Preserve current trust, real capabilities, approvals, Signer identity/revision,
journals, receipts and replay/fencing state. Reconcile Unknown before conflicting
writes. Use verified explicit lifecycle targets; do not reverse SQL, reset
identity or restore over persistent state to force a target to start.
Cross-version execution can fail or damage state despite removal of admission.

Unrun, failed, skipped and excluded checks remain distinct. Required failures
still block publication, and any candidate change requires rechecking its
affected CI checks.
