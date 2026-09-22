# 1.x contracts and compatibility

Status: contract inventory for the [1.0 support policy](support-policy.md).
`v1.0.1` is the first recommended production stable baseline; published
`v1.0.0` remains an upgradeable transitional release. Pre-1.0 deployments must
be redeployed, not upgraded in place into `1.x`.
This inventory freezes the review surface, not an unreleased version or an arbitrary `1.x` combination.
An unchanged ALPN, SemVer major, generated schema, or green source test is not
proof that two release artifacts interoperate. Production support follows the
linked policy; additions and withdrawals require explicit review before candidate
freeze. Exact candidate SHA, artifact identities and acceptance results belong
in the delivery report, not in this maintenance document.
The [reviewed rolling-window exclusions](#reviewed-rolling-window-exclusions)
retain the historical pre-1.0 T05 diagnostic scope. They are not support for
pre-1.0 entry into `1.x` or acceptance of an unreleased production candidate.

## Contract owners

| Surface | Candidate stable contract and authority | Not a public extension interface |
| --- | --- | --- |
| HTTP | [`openapi.yaml`](../../openapi/openapi.yaml): `/api/v1` resources, request/response schemas, strict JSON, authentication, authorization, idempotency, revisions, Problems, pagination and SSE cursor behavior. Preserve the [HTTP baseline](../development/http-baseline.md), including documented error/method precedence. Health/version aliases retain their current behavior. | Go handler/package layout, Web stores, component APIs and generated-client implementation. `/dev` and simulator routes are development-only. |
| CLI and configuration | Documented Controller `--role`, `--migrate-only`, `OCSERV_*` and exclusive `_FILE` settings; managed-node `agent.env`, `privd.env`, `relays.env` and one-shot enrollment/attestation commands. [Production deployment](../operations/production-deployment.md) and [Agent lifecycle](../operations/agent-lifecycle.md) own names, defaults, secret permissions and failure behavior. | Undocumented flags, debug/probe modes, test environment variables, log wording and local SQLite table access. |
| Installation and upgrade | Exact release pins; `deploy/production/install.sh`, `controller-bootstrap.sh`, `controller.sh`; `deploy/managed-node/install.sh`; verified node install/upgrade/rollback scripts. Preserve signed manifests, independently provisioned trust, same-target retry and protected confirmed/pending state. | Direct Compose/image replacement, source-tree node installation, unsigned archives and arbitrary package-manager downgrades. |
| Package names | `ocservia-agent-X.Y.Z-linux-{amd64,arm64}.tar.gz` plus checksum/signature; new DEBs `ocservia-agent_X.Y.Z-1_{amd64,arm64}.deb`; RPMs `ocservia-agent-X.Y.Z-1.{x86_64,aarch64}.rpm`; `controller-release-{amd64,arm64}.json` and existing amd64 alias `controller-release.json`. The [baseline registry](../../scripts/release-upgrade-baselines.json) alone resolves historical DEB filenames without `-1`. | Crate versions such as `0.1.0`, build-directory names and image config IDs as registry manifest digests. |
| Schema and transport | [`proto/`](../../proto/) owns message numbers, enums and services. ALPNs remain `ocserv-platform/enroll/1` and `ocserv-platform/agent/1`; enrollment proof is 1.1, session protocol is negotiated separately. Go/transportd UDS and Agent/privd local protocol are private matched-release interfaces. | Direct Go access to Iroh; arbitrary third-party privd clients; automatic acceptance of new signed-command fields because Protobuf normally permits them. |
| Signing and hashes | [Command authorization v1](../development/command-authorization-v1.md), session/artifact grants, connection fence v2, fence binding v2 and [privd receipt v1](../development/agent-privd.md) have frozen canonical transcripts. [Semantic v1](../development/command-semantic-hash-v1.md) remains frozen; [v2](../development/command-semantic-hash-v2.md) is current Controller issuance. Proto serialization is never canonical signing input. | Changing an existing hash/transcript to absorb new semantics; inferring semantic v2 from the historical capability string `command.semantic-hash.v1`. |
| Versions and durable state | Controller/transportd use one candidate source/release; Agent/privd/upgrader are one verified package. Mixed Controller/node versions require the finite matrix below. Preserve journals, root effect store plus HMAC/receipt keys, endpoint identity and revision/fence floors. | Independent Agent/privd swaps or numeric version classification as permission to dispatch. |
| Database | [Migration contract](../development/control-plane.md): owner-only serialized migrations, restricted runtime role, immutable applied history and explicit compatibility ranges. PostgreSQL schema numbers are not MySQL/MariaDB migration revisions. | Generic down-migration rollback, automatic force-clean, cross-engine migration, or treating additive DDL as automatically backward compatible. |

Generated code is disposable output of Proto/OpenAPI. Use the existing
`scripts/check-breaking.sh`, `scripts/generate.sh` and
`scripts/generated-clean.sh`; do not patch generated clients or relax the
explicit historical security-migration allowlist. The breaking command compares
with `origin/main`, not with every shipped release. Release-level behavioral
evidence remains necessary even when it passes.

## Approval principal boundary

Baseline 1.0 and T07 require **independently controlled requester and approver
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
baseline T07 and requiring separate human evidence when a deployment selects it.
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

## Finite release matrix

The supported native upgrade source for the `v1.0.1` baseline is the published
transitional `v1.0.0`, registered in
[`release-upgrade-baselines.json`](../../scripts/release-upgrade-baselines.json)
at commit `e85ab3fa90d1d5f6e4c53b56b2f5e0278f6da060`, PostgreSQL schema 36.
Require the four Agent/Controller x amd64/arm64 cells of the
[native upgrade workflow](../development/release-upgrade-validation.md) on the
exact candidate. Registration proves artifact identity, not successful fresh
bootstrap, an accepted upgrade, or arbitrary mixed-version application support.
Use a matched release for steady-state deployment; upgrade Controller first.
A temporary 1.x mixed-version window means exactly one pair: the published
`v1.0.0` Agent/privd/upgrader package against the candidate
Controller/transportd. Accept that window only after the `v1.0.0` x
amd64/arm64 application cells below pass on the exact candidate, collected in
one run/attempt together with the native upgrade gate; they are part of `1.x`
release acceptance, not optional diagnostics.

### Historical pre-1.0 diagnostics

The following matrix records historical coverage, not supported in-place
upgrades or rolling deployments into `1.x`. Pre-1.0 users must redeploy.
These optional diagnostic cells are not formal 1.x release requirements and
cannot replace the `v1.0.0` native upgrade gate, the `v1.0.0` mixed-version
application acceptance above, or matched-candidate acceptance.
The historical releases are real signed assets already registered in
[`release-upgrade-baselines.json`](../../scripts/release-upgrade-baselines.json):

| Baseline | Immutable source commit | Existing scope |
| --- | --- | --- |
| v0.6.0 | `cc8399641dc32083466a9c77369fcb8debf1ee48` | Historical native/application baseline; PostgreSQL schema 36 |
| v0.6.1 | `1805962fe1a98a22955b3105bfa8ebce7f2ea1eb` | Historical supplemental native/application baseline; PostgreSQL schema 36 |

Use the registry's checksum-manifest and independently anchored key pins, not a
new download's self-reported digest. Older entries remain registered for
historical lifecycle tests and artifact provenance, not as formal 1.x upgrade
sources. Retained runtime compatibility code is not an upgrade-support promise.

For **each** selected baseline — the `v1.0.0` mixed-version acceptance pair or
a historical diagnostic — and **each** native `amd64` / `arm64` architecture:

| Combination | Executable entry | Acceptance boundary |
| --- | --- | --- |
| Published Agent + matching privd/upgrader -> candidate Controller/transportd, dedicated authenticated Relays | `scripts/release-session-compatibility.sh run` | Six application cells total: the `v1.0.0` mixed-version pair plus the two historical diagnostic baselines, each on amd64/arm64. Required scope: enrollment, grant/fence/receipt, telemetry and approved reload; config revision/rejection and plan replay, CSR, issue/export/revoke approvals, P12 one-use download and persistent Agent/privd restart recovery. The systemd chain uses two real Relays for v0.6.0 and one for `v1.0.0` and v0.6.1; the real-process PKI chain uses two TLS Relays. No historical rebuild. Covered workflows require actual-artifact evidence; the two exclusions below are not positive acceptance. |
| v0.6.0 node, either architecture: uncertain non-idempotent mutation across all-Relay outage / owner change | Same systemd chain, retaining its strict automatic-recovery assertion | **Excluded: guaranteed automatic mutation recovery.** Query-only `Unknown` requires manual reconciliation. Preserve evidence, reconcile before resuming writes, then upgrade the verified matched node package under the path below. A green run or a newer version alone does not restore this promise. |
| v0.6.0 / v0.6.1 node, either architecture: positive ConfigPlan apply with candidate Controller | Same PKI chain checks rejection only | **Excluded: successful plan/apply/rollback.** Do not use positive configuration apply in this rolling window. Upgrade to a verified matched node package with a reviewed complete configuration contract and positive plan/apply/recovery acceptance before enabling it; no such qualifying release is established by this matrix. |
| Published pre-1.0 native package -> newer pre-1.0 candidate package | Existing `release-upgrade.yml`, `baseline_release` set explicitly | Historical DEB Ubuntu and RPM Rocky 9 coverage only; no path into 1.x. |
| Published pre-1.0 Controller -> newer pre-1.0 candidate Controller, PostgreSQL 17 | Same native workflow | Historical authenticated data/session retention, migration, same-target recovery and guarded rollback/base restore; no path into 1.x. |
| Candidate node -> historical Controller, or independently mixed Agent/privd | Not admitted to this candidate matrix | Upgrade Controller first; restore only a verified matched snapshot. No downgrade/security-equivalence promise. |

The application cells are single-host fixtures, not cross-fault-domain, native
package-manager upgrade, all-distro, production OIDC or formal G6 evidence.
The PKI chain reuses `scripts/database-controller-e2e.sh` on PostgreSQL 17.10
with verified published node binaries. It has a real authenticated TLS signer
fixture and real OpenSSL/ocserv processes, not a production CA/HSM. Its fixed
`systemctl` facade probes real ocserv and sends real signals; only the separate
systemd chain covers service-manager lifecycle. The default database E2E route
still uses its existing PostgreSQL 18 image; this does not expand production support.

### Reviewed rolling-window exclusions

**Historical decision: adopted for the finite pre-1.0 T05 rolling window.** Ordinary session,
approved reload, configuration-plan replay and certificate/P12 workflows remain
in scope. The two explicit exclusions in the matrix apply on both architectures; an observed
failure on amd64 does not establish an arm64 exemption. This narrows the
candidate compatibility promise, not the strict diagnostic tests, root
permissions or any already-supported deployment. The alternative of adding
new configuration semantics or automatic mutation reconciliation is deferred
to a separately reviewed matched-node contract and its runtime acceptance.

**Positive typed configuration apply is excluded for v0.6.0 and v0.6.1 nodes.**
The v1 allowlist cannot express a complete real ocserv configuration (`device`
is absent), planning parses the candidate as a complete file, and apply refuses
unresolved TLS SecretRefs. Do not bypass root validation or substitute a fake
parser. The matrix proves stale revision refusal, exact plan replay, real parser
rejection, denied apply and unchanged configuration/revision, not successful
apply or rollback. Historical adapter errors have no trusted root receipt;
the Agent retains `Unknown` with `privd_receipt_missing_or_malformed`, never
promoting it to success or a trusted final failure. Upgrade Controller/transportd
first, then the signed matched Agent/privd/upgrader package using the existing
[Agent lifecycle](../operations/agent-lifecycle.md) path. Enable positive apply
only after that exact combination has a reviewed complete configuration/TLS
SecretRef contract and passing positive plan/apply/recovery evidence. Neither
matching binary versions, negotiated capability names nor package-upgrade
success proves this. Until then, leave this workflow unused; this document
does not add or claim a version-based API/UI gate.
The existing same-source I15/I16/I17 checks do not fill that release-level gap.

Matched nodes with the separately negotiated `ocserv.config.complete.plan` and
`ocserv.config.complete.apply` capabilities use the
[complete node-local TLS profile](../operations/node-local-config-tls.md).
The profile does not widen historical-node support. Apply is reload-only:
startup authentication/listener/worker/socket/TLS bindings must already match
the node's protected active configuration, otherwise root rejects before
preparing an effect. Initial activation and TLS version/path changes require
an explicitly authorized operator maintenance restart, not an automatic
restart or an apparently successful reload. Final acceptance must include
actual VPN authentication after apply, exact rollback and durable recovery;
native parser/occtl health alone is insufficient.

Pending non-idempotent mutations have another explicit boundary: after an
uncertain dispatch and owner change, recovery may be query-only and retain
`Unknown` / `manual_reconciliation_required`. In particular, the v0.6.0
two-Relay cell cannot promise every queued reload will complete automatically
after an all-Relay outage. Do not resend an uncertain mutation, clear journals
or extend the deadline to call this a pass. The strict reload-recovery test
retains its failure; the independent PKI phase still runs and records its own
exit code. A later green run does not erase the earlier demonstrated boundary.
**Guaranteed automatic recovery in that v0.6.0 scenario is excluded.** Follow
[incident recovery](../operations/incident-recovery.md#transport-and-credentials):
stop new privileged writes, preserve the exact command ID, semantic hash,
owner/fence history, Agent journal and root effect/receipt evidence, and
restore the configured Relays without resetting endpoint identities. Compare
the operation result with trusted durable evidence for that exact command;
an online node or currently healthy ocserv is not proof of the earlier effect.
If evidence remains missing or conflicting, keep the operation `Unknown` and
the affected writes paused for operator reconciliation. Do not invent a
terminal result, edit durable state or issue a fresh mutation as a retry.
Upgrade through the verified matched-package lifecycle only after resolving
the outstanding uncertainty; upgrading is not itself reconciliation. A future
automatic-recovery promise needs exact-artifact fault/recovery evidence and
explicit contract review, not just a numerically newer node package.

The runner still exercises the broader all-Relay recovery scenario and
returns failure when its automatic-success assertion fails. Preserve that
cell's status, both phase exit codes and the original run/attempt unchanged.
For scoped contract acceptance, identify the exact excluded failure and
verify the required workflow checkpoints independently from the retained logs
and results. Do not relabel a failed aggregate as PASS or waive any other
failure. A missing required checkpoint still blocks the covered promise.
If the chain stops at the excluded uncertain-mutation failure, its later
replay and cold-start checkpoints are not run, not PASS; independent PKI
restart evidence does not establish those systemd recovery paths.

### Matched-release recovery boundary

**Guaranteed automatic recovery of an uncertain non-idempotent mutation is
excluded for matched releases too**, not only the historical v0.6.0 rolling
window. Matching Controller/transportd and Agent/privd/upgrader artifacts does
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
the result. Read-only recovery queries are not mutation retries. A package
upgrade is not reconciliation. Follow the existing
[incident recovery procedure](../operations/incident-recovery.md#transport-and-credentials).

Keep strict automatic-recovery probes unchanged. A green attempt does not
erase an earlier Unknown, and a failed strict aggregate remains failed. A
separate scoped `EXPECTED-UNKNOWN` assessment requires retained evidence of
the exact unresolved command, query-only behavior, preserved identity/state,
no duplicate mutation and paused conflicting writes. Missing observations are
NOT RUN or BLOCKED, not an expected-result waiver. Restoring a guaranteed
automatic-recovery promise requires reviewed durable-evidence semantics and
exact-candidate fault/restart acceptance, not repeated runs or longer timeouts.

### Fixture boundaries

The private CA requires a test-only systemd drop-in, derived from v0.6.0's
direct ExecStart or v0.6.1's launcher. Published binaries and installed units
remain unchanged. v0.6.0 rejects single-Relay custom mode (requires 2..8 URLs):
retain its two-Relay deployment, or upgrade the matched node package to v0.6.1
before selecting one Relay. Do not substitute duplicate URLs or weaken that
admission check. The disposable image masks systemd-binfmt rather than changing the
host's native-admission policy; admission is checked again after the chain.

### Execute one application cell

For authorized GitHub Actions execution, dispatch the existing
`release-upgrade.yml` on the exact candidate branch with `version`,
`baseline_release`, `candidate_sha`, and `session_compatibility=true`. The
application jobs build once per native architecture and run the published
`v1.0.0` upgrade-source pair plus both historical diagnostic baselines,
retaining all six cells in that run/attempt. They do not
publish images, run on ordinary PRs or replace the native upgrade jobs. For a
1.x candidate, keep `baseline_release=v1.0.0`; the `v1.0.0` application pair
is the mixed-version window evidence required by the finite matrix above, and
the pre-1.0 application pairs are diagnostics only that do not select the
native upgrade source.
For application-fixture iteration, `session_only=true` runs those six cells
without rebuilding native upgrade products. Its result cannot satisfy the
separate native-upgrade requirements; the default remains native upgrades.

On BuildServer, `fetch` and `verify` perform only bounded artifact download and
checksum/signature verification, without extraction, installation or execution:

```bash
export BASELINE_RELEASE=v0.6.1 PACKAGE_ARCH=arm64
export RELEASE_ASSET_DIR="$HOME/task-private/releases-v061-arm64"
bash scripts/release-session-compatibility.sh fetch
bash scripts/release-session-compatibility.sh verify
```

The destination must not already exist for `fetch`. Repeat for both registered
tags and architectures; a verified foreign-architecture archive is not a native
runtime pass. A corrupted or incomplete set fails instead of falling back.

Run `run` only on an explicitly authorized disposable native systemd VM/runner,
with a local Docker daemon and no emulation handlers. Never unregister binfmt
or run host installers on shared BuildServer. Use a clean exact candidate
checkout and [the existing single-Relay setup](../development/single-relay-validation.md#real-agent-chain).
Set `CANDIDATE_SHA`, `RUNNER_ARCH` (`X64` or `ARM64`), unique `RUN_ID`, private
`RUNNER_TEMP` and a new `ARTIFACT_DIR`. Supply the five existing image variables
and `RELEASE_WORKFLOW_IMAGE` as full local `sha256:` image IDs, with native
architecture. Control, transport, probe and workflow images must carry
`org.opencontainers.image.revision=CANDIDATE_SHA`
from their build, not a post-build relabel. Retain their build records and
registry manifest digests separately; a label alone is not build provenance.

```bash
bash scripts/release-session-compatibility.sh run
```

The wrapper verifies the old signed set, reuses the unchanged archive verifier
inside the isolated node, requires the signed matched node package and checks
the final Controller-observed Agent version, and records `compatibility-result.json`.
Missing inputs, mutable image tags, foreign/dirty source, wrong architecture,
wrong version, absent result or a failed chain cannot pass. Preflight failures
have no success result. A result is one cell only; require all four unique
tag/architecture cells on the same candidate, with one coherent CI run/attempt
when applicable. No automatic workflow dispatch or publishing is performed.

Retain sanitized command logs, start/end time and exit code, baseline pins,
candidate SHA, script/patch digest, exact package/image identities, native
architecture, topology, tool/Relay/ocserv/crypto/database versions and CI
run/attempt. Keep secrets out of evidence. After export remove only the named
task resources, not shared caches, daemon state or another task's fixtures.
`scripts/test-release-session-compatibility.sh` tests integrity/admission with
fixtures through the existing release-tools CI route; it is not a runtime cell.

## Database and recovery matrix

[Production database support](../operations/production-deployment.md#database-support)
remains authoritative. All rows use Controller `linux/amd64` and `linux/arm64`
artifacts; this does not certify an external server's OS/architecture. Record
the actual server, client and backup image versions/digests per acceptance run.

| Version and deployment | Existing verification entry | Recovery and uncovered scope |
| --- | --- | --- |
| PostgreSQL 17 bundled (current fixture 17.10) | `PG_MAJOR=17 DATABASE_TEST_SCOPE=compatibility scripts/database-integration.sh`; `scripts/i18-backup-restore-smoke.sh` | Verified base backup; [PITR](../operations/postgres-pitr-restore.md)/[failover](../operations/postgres-failover.md) only within their explicit topology/gates. Not established by database CI. |
| PostgreSQL 17 external | Same backend checks plus `scripts/i18-external-postgres-backup-restore-smoke.sh` | Verified TLS owner/runtime/backup connections and base restore. No automatic managed external HA/PITR. |
| MySQL 8.4.10 external | `ENGINE=mysql DATABASE_TEST_SCOPE=compatibility bash scripts/database-foundation-integration.sh`; `ENGINE=mysql scripts/i18-mysql-backup-restore-smoke.sh` | Backend-specific logical backup/isolated restore. No bundled deployment, cross-engine migration or PostgreSQL HA/PITR claim. |
| MariaDB 12.3.2 external | Same two entries with `ENGINE=mariadb` | Same logical-restore boundary, independently tested engine, not an alias for a MySQL pass. |
| PostgreSQL 18 CI; bundled MySQL/MariaDB | PG18 existing CI remains; bundled MySQL/MariaDB launcher rejects | Not production support. Keep tests and production admission distinct. |

T02 identified same-line patch candidates (PostgreSQL 17.11, MySQL container
8.4.12/native 8.4.11 plus vendor patches, MariaDB 12.3.3). They are **pending
compatibility and artifact review**, not replacements for the existing support
rows. T09 owns final image pins/inventory; T07 owns actual deployment evidence.
Verify migration/permissions and matching backup/restore before accepting a
patch set. Do not substitute PG18/G6 or silently remove old rows. SQLite
journal WAL-reset applicability and upgrade recovery remain T06/T08 work;
do not reset journals to make an upgrade pass.

## Freeze and rollout decisions

1. Keep schema authority, canonical versions and strict runtime admission
   unchanged. No unconditional application compatibility for `1.x`.
2. Upgrade patched dedicated Relays one at a time, then matched
   Controller/transportd with owner migration and a verified backend backup.
   Register/verify root receipt authority and update the matched node package
   only after the old-node/new-Controller cell is accepted. Stop privileged
   dispatch while capabilities/keys are not ready. A protocol-compatible old
   Relay is not a security-equivalent rollback.
3. Preserve the existing [Controller rollback guards](../how-to/controller-rollback.md)
   and [matched Agent rollback](../how-to/agent-rollback.md). Changed descriptors
   or incompatible schema require forward recovery or isolated backend restore,
   not down-migration or forced state edits. Reconcile Unknown before resuming.
4. Before the `v1.0.1` production-candidate freeze: collect all four native
   upgrade cells from `v1.0.0`, the `v1.0.0` node against candidate Controller
   application cells on both architectures for the mixed-version window, and
   matched-candidate application acceptance on
   the exact candidate. Historical pre-1.0 application cells are optional
   diagnostics, not a formal 1.x release gate. Retain their exclusions and
   original results without turning them into support promises. Review the
   database patch set, actual ocserv/distro/crypto versions and recovery
   evidence separately. T03 transport probes and T04 native ocserv
   1.5.0 tests remain evidence for their original artifacts, not this candidate's release gate.

T05's two compatibility decisions are settled within this finite scope, not
waivers for final-candidate acceptance. Unrun, failed, skipped and excluded
checks must stay distinct. A pending required check blocks its promise, not
preparation of the next task. Documentation-only convergence does not transfer
earlier runtime results to the documentation commit or a later freeze candidate.
