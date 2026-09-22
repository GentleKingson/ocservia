# 1.0 support and versioning policy

Status: this is the support policy for the ocservia **1.0 release line**.
`v1.0.1` is the first recommended stable baseline for production deployments.
`v1.0.0` is an already published, upgradeable transitional 1.0 release, not
the recommended production baseline. Pre-1.0 installations have no supported
in-place upgrade path into formal `1.x`; operators must redeploy, not upgrade
through `v1.0.0` as a bridge.
This document defines policy only. Contract surfaces are owned by
[stable contracts](stable-contracts.md); platform facts are owned by the
operational guides referenced below and are not duplicated here.
Production recommendations apply to published tagged artifacts; this policy
does not turn an unreleased candidate or an unrun acceptance check into a pass.

## Versioning

ocservia follows [Semantic Versioning](https://semver.org/) for the 1.0 line.

- The public contract is exactly the surface frozen in
  [stable contracts](stable-contracts.md): the OpenAPI `/api/v1` HTTP contract,
  documented CLI and configuration settings, installation and upgrade scripts,
  published package names, protocol ALPNs and message numbers, signing and
  semantic-hash transcripts, and durable state obligations.
- Within `1.x`, public contract changes are additive. Removing or breaking a
  public contract surface requires a new major version (`2.0`) and a reviewed
  migration path.
- Package-internal details — Go/Rust/Web internals, generated code, image
  config identities, database schema numbers — are not public contract and may
  change within the line as long as the documented upgrade path preserves data.

Releases are plain `X.Y.Z` SemVer. The release workflow rejects non-SemVer
input, and there is no `-rc.N` scheme: a **release candidate** is a frozen
branch plus the candidate artifacts built from its exact SHA, validated by the
existing dry-run dispatch, native upgrade jobs, and session-compatibility cells
on that SHA before the numeric release is tagged.

## Compatibility and deprecation

- **Controller and node version window.** The supported deployment runs a
  matched Controller/transportd release with a matched Agent/privd/upgrader
  package, as defined by the version and durable-state contract in
  [stable contracts](stable-contracts.md). Mixing a published historical
  1.x node with a newer Controller is supported only inside the
  [finite release matrix](stable-contracts.md#finite-release-matrix) and its
  recorded exclusions, with the published `v1.0.0`-node against
  candidate-Controller application cells as the exact-pair evidence; upgrade
  the Controller first. There is no
  security-equivalent downgrade promise.
- **Deprecation.** A public contract surface may be deprecated only through a
  release-notes announcement in the minor release that introduces the
  deprecation. A deprecated surface keeps working through at least the next
  minor release; removal happens no earlier than the following minor release
  and requires its own release-note entry. Anything not listed in
  [stable contracts](stable-contracts.md) is private and can change without a
  deprecation notice.
- **Upgrade and recovery paths.** `v1.0.0` is the earliest supported source for
  in-place upgrades within `1.x`, including the transition to `v1.0.1`.
  Pre-1.0 deployments must follow the fresh
  [Controller](../getting-started/production.md) and
  [managed-node](../getting-started/managed-node.md) deployment paths instead.
  Baseline artifact identities are registered in
  [`scripts/release-upgrade-baselines.json`](../../scripts/release-upgrade-baselines.json)
  (currently through `v1.0.0`); historical pre-1.0 entries are retained for
  provenance and regression diagnostics, not as supported paths into `1.x`.
  Actual upgrade acceptance requires the
  [native release upgrade workflow](../development/release-upgrade-validation.md).
  Rollback follows the guarded
  [Controller rollback](../how-to/controller-rollback.md) and
  [matched Agent rollback](../how-to/agent-rollback.md) procedures; interrupted
  or uncertain operations follow [incident recovery](../operations/incident-recovery.md).

## Supported platforms

Each row is owned by its authoritative document; this section is an index, not
a second support matrix.

| Surface | Supported scope | Authority |
| --- | --- | --- |
| Controller OS/architecture | Linux `amd64` and `arm64` release artifacts | [Production deployment](../operations/production-deployment.md) |
| Agent native packages | DEB on Ubuntu 24.04, RPM on Rocky 9, `amd64`/`arm64` | [Agent lifecycle](../operations/agent-lifecycle.md), [native upgrade validation](../development/release-upgrade-validation.md) |
| Databases | PostgreSQL 17 bundled or external; MySQL 8.4.10 external; MariaDB 12.3.2 external | [Database support](../operations/production-deployment.md#database-support) |
| Managed ocserv | Adapter-admitted `1.2.x`, `1.3.x`, `1.4.x`, and `1.5.0` | See note below |
| Relays | Dedicated relay hosts running the matched vendored `iroh`/`iroh-relay` release | [Dedicated relays](../operations/dedicated-relays.md) |
| Authentication | Local only, OIDC only, or Local + OIDC | [Authentication](../operations/authentication.md) |

Managed-ocserv note: the node adapter admits exactly `1.2.x`, `1.3.x`, `1.4.x`,
and `1.5.0` from real ocserv version output. `1.5.0` is the release-validated
version, with native adapter validation and real-VPN business acceptance
evidence; the older admitted lines run the same adapter paths without a
per-line acceptance guarantee. Recognition of a version string is not a
support claim for future ocserv releases.

Support rows state what ocservia has actually validated. Upstream releases
(database point releases, new ocserv lines, new distro versions) are adopted
only after compatibility and artifact review, never automatically.

## Deployment capability distinctions

Three pairs are deliberately not conflated:

1. **Single-relay availability vs dual-relay redundancy.** One relay host on
   independent infrastructure is a supported, non-redundant deployment. The
   redundancy promise — authenticated failover from relay A to an independent
   relay B, plus direct↔relay path transitions — is proven only with two relays
   on separate failure domains and is part of the formal readiness acceptance.
   See [dedicated relays](../operations/dedicated-relays.md).
2. **Package installed vs really managed online.** A node with packages
   installed and services stopped is not fleet-managed. Online management
   requires enrollment, Controller approval, a started matched node, and an
   authorized active session; fleet lifecycle evidence covers that full path.
   See [enroll a node](../how-to/enroll-node.md) and
   [agent lifecycle](../operations/agent-lifecycle.md).
3. **Backup created vs backup restorable.** Producing a backup artifact
   (PostgreSQL base backup, MySQL/MariaDB logical dump) is a scheduled
   operation; **restore is a separate, separately validated procedure**.
   PostgreSQL restore, PITR, and failover are validated within their explicit
   topology gates; MySQL/MariaDB restore is validated into a new isolated
   server with the restore verifier, and redirecting a live Controller is a
   guarded manual cutover. See
   [PostgreSQL backup](../operations/postgres-backup.md),
   [PITR](../operations/postgres-pitr-restore.md),
   [failover](../operations/postgres-failover.md), and
   [MySQL/MariaDB backup](../operations/mysql-backup.md).

## Security review posture

- Dependency advisories (Go `govulncheck`, Rust `cargo audit`/`cargo deny`
  including the relay lockfile, npm audit) and repository secret scanning run
  through the scheduled Security Checks workflow and must be green on the
  release candidate. See [SECURITY.md](../../SECURITY.md) for reporting and
  supported-version policy.
- **Deferred, with re-review conditions:** automated base-image (OS-level)
  vulnerability scanning of published container images and SBOM generation are
  not part of the 1.0 release chain. Images are digest-pinned and rebuilt from
  locked inputs, and dependency advisories cover the compiled first-party
  dependency graph. Re-review condition: adopt an image scanner and SBOM
  emission into the release workflow, with results recorded per published
  digest, before or during the first `1.0.x` line maintenance release.
- **Deferred, with re-review conditions:** the same-line database patch
  candidates identified during contract review (PostgreSQL 17.11, MySQL
  8.4.12 container / 8.4.11 native, MariaDB 12.3.3) are **not** part of the
  1.0 support rows. Adoption requires the migration, permission, and matching
  backup/restore review described in
  [stable contracts](stable-contracts.md#database-and-recovery-matrix), plus
  new per-version acceptance evidence; the currently pinned and validated
  versions remain authoritative until then.
