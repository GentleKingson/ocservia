# Support and versioning policy

## v1.1.0 policy reset

This policy took effect with the published
[`v1.1.0` release](https://github.com/GentleKingson/ocservia/releases/tag/v1.1.0).
Its release notes link the exact-source acceptance and publication runs; later
candidates require their own validation. The archived 1.0 policy
records the published line; its version windows, deprecation timetable and additive-only
`1.x` promise do not govern this explicitly breaking policy reset. Existing
published tags, artifacts and release history are unchanged.

`v1.1.0` removes software upgrade, downgrade and rollback compatibility
admission, historical migration compatibility probes, legacy deployment
auto-conversion, and historical release compatibility matrices and gates.
This includes the public `--schema-compatibility-check` CLI and the readiness
response's `schema_version` field. Readiness checks current core table reads,
database permissions and event-stream health, not migration history or a
Controller schema range. There is no
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

## Published history and version format

The [published 1.0 policy](https://github.com/GentleKingson/ocservia/blob/d420b22018596d6741d55fa56bd19c4a767e5817/docs/reference/support-policy.md)
records the original additive-only promise, fixed upgrade source, deprecation
schedule and finite release matrix. Those software-version promises do not
govern the v1.1.0 reset; past release facts and immutable assets are unchanged.
This document does not turn an unreleased candidate or an unrun check into a
production recommendation.

Release identities remain plain `X.Y.Z` SemVer syntax. Version ordering is not
execution authority. The explicitly chosen v1.1.0 release removes public
surfaces and compatibility guarantees despite its minor-version label; it must
not be described as fully backward compatible. A candidate is frozen source
plus its exact products, not a `-rc.N` publishing scheme.

[Release Check](../development/release-checks.md) validates current products.
Dispatch remains a non-publishing dry-run; tag-triggered publication retains
its checks, protected environment and signing boundaries. Explicit lifecycle
operations use verified targets, not a minimum source version or a bridge
release. Package-manager behavior is not overridden. Unknown outcomes require
[incident recovery](../operations/incident-recovery.md), not blind replay.

## Supported platforms

Each row is owned by its authoritative document; this section is an index, not
a second support matrix.

| Surface | Supported scope | Authority |
| --- | --- | --- |
| Controller OS/architecture | Linux `amd64` and `arm64` release artifacts | [Production deployment](../operations/production-deployment.md) |
| Agent native packages | DEB on Ubuntu 24.04, RPM on Rocky 9, `amd64`/`arm64` | [Agent lifecycle](../operations/agent-lifecycle.md), [current package validation](../development/release-upgrade-validation.md) |
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

Each supported deployment uses one Controller, one database instance and one
dedicated Relay. The database choices and supported versions above, integrated
/ standalone deployments, bundled / external database modes and multi-Agent
management remain unchanged. Components may run on separate hosts. Controller
HA, multiple Relay failover, database clusters, PostgreSQL automatic failover
and PITR readiness are outside the supported scope. Rebind uses two independent
Controller deployments; it is not HA. See the
[resilience coverage and migration decision](../development/resilience.md).

Three pairs are deliberately not conflated:

1. **Single-relay recovery vs redundancy.** One dedicated Relay is supported.
   Its outage may interrupt management connections; recovery uses the original
   Relay, identity and command. Multiple Relay failover is outside support.
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
   PostgreSQL backups retain their required WAL and verification; isolated
   restore is separate from HA/PITR readiness. MySQL/MariaDB restore uses a new isolated
   server with the restore verifier, and redirecting a live Controller is a
   guarded manual cutover. See
   [PostgreSQL backup](../operations/postgres-backup.md),
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
