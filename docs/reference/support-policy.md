# 1.0 support and versioning policy

## v1.1.0 policy reset (pending release)

This is the approved policy for the unreleased `v1.1.0` candidate, not a claim
that implementation or acceptance has finished. The 1.0 policy below records
the published line; its version windows, deprecation timetable and additive-only
`1.x` promise do not govern this explicitly breaking policy reset. Existing
published tags, artifacts and release history are unchanged.

`v1.1.0` removes software upgrade, downgrade and rollback compatibility
admission, historical migration compatibility probes, legacy deployment
auto-conversion, and historical release compatibility matrices and gates.
This includes the public `--schema-compatibility-check` CLI. There is no
minimum source or rollback version, fixed upgrade source, version allowlist,
force/skip switch, or replacement capability/schema-number version fence.
Install, upgrade and rollback remain explicit operations on verified targets;
version format and version display remain, but version ordering is not authority
to accept or reject an operation.

Keep artifact signatures, trusted keys, hashes, source commit and architecture
binding; real command capabilities; authorization and approvals; idempotency,
replay protection and ownership fences; Signer identity and monotonic revision;
transactions, atomic state commits and real failure propagation. Database
initialization, applied records, known migration content integrity and actual
service/database readiness remain required. Removing historical deployment
conversion must also remove its automatic network/configuration/data changes,
not leave destructive actions behind without their former guards.

Acceptance covers the current candidate on the supported architectures,
deployment modes and database products, with security, integration and resilience
checks. It does not certify historical upgrade/downgrade/migration paths.
Cross-version operations may fail or damage state; absence of a compatibility
rejection is not a safety guarantee. Never automatically reverse database
migrations, reset identities or overwrite persistent state. Existing installed
old scripts retain their original behavior. Operators needing a different
deployment layout must explicitly redeploy rather than expect legacy conversion.
Release notes must identify removed public entrypoints and these risks, and must
not describe `v1.1.0` as a fully backward-compatible SemVer minor.

## Published 1.0 policy

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
branch plus the candidate artifacts built from its exact SHA. The integrated
[Release Check](../development/release-checks.md) validates the exact products
before publication. Manual dry-run is optional debugging, not a second required
full execution before the tag workflow repeats the same checks.

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
  [`scripts/release-upgrade-baselines.json`](https://github.com/GentleKingson/ocservia/blob/d420b22018596d6741d55fa56bd19c4a767e5817/scripts/release-upgrade-baselines.json)
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
- **Image scanning and SBOM (adopted with the first `1.0.x` maintenance
  release, `v1.0.1`):** the release workflow scans every Controller image
  candidate with the pinned Syft and Grype tools on one frozen vulnerability
  database snapshot, before any release image is written to the registry: the
  scan job runs against the built image archives, and the reviewer-gated
  publish job may only push after the gate passes and must prove the images it
  loads and indexes match the scanned evidence by config digest. Each release
  ships a full SPDX SBOM and an OS-level vulnerability report per image and
  architecture, bound to the scanned evidence and to the pushed multi-platform
  digests in a security summary and bindings record attached to the release. A
  High or Critical OS-level finding with an available fix fails the publish
  and blocks the release until the affected base image is refreshed, unless
  the finding is covered by an exemption in
  `deploy/production/image-scan-exemptions.json` that binds the exact image,
  package, installed version, and vulnerability id, is recorded against the
  image's actual digest-pinned base image (resolved from the same Dockerfiles
  the build consumed, so a base refresh invalidates the entry), records a
  reason, and has not passed its review date — a new CVE on the same package,
  a changed base image, or an expired review date fails the gate again;
  findings without an
  available fix are recorded in the report without blocking. The gate covers
  the base-image (OS package) layer; the compiled first-party dependency graph
  remains covered by the dependency advisories above.
- **Deferred, with re-review conditions:** the same-line database patch
  candidates identified during contract review (PostgreSQL 17.11, MySQL
  8.4.12 container / 8.4.11 native, MariaDB 12.3.3) are **not** part of the
  1.0 support rows. Adoption requires the migration, permission, and matching
  backup/restore review described in
  [stable contracts](stable-contracts.md#database-and-recovery-matrix), plus
  new per-version acceptance evidence; the currently pinned and validated
  versions remain authoritative until then.
