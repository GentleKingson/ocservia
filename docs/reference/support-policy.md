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
response's `schema_version` field. Readiness checks zero-row reads of current
core tables and event-stream health, not migration history or a Controller
schema range. There is no
minimum source or rollback version, fixed upgrade source, version allowlist,
force/skip switch, or replacement capability/schema-number version fence.
Install, upgrade and rollback remain explicit operations on verified targets;
version format and version display remain, but version ordering is not authority
to accept or reject an operation.

Keep runtime command signatures and trusted keys, Controller-authorized
upgrade package digests and architecture; real command capabilities;
authorization and approvals; idempotency,
replay protection and ownership fences; Signer identity and monotonic revision;
transactions, atomic state commits and real failure propagation. Database
initialization, applied records, known migration content integrity and actual
service/database readiness remain required. Removing historical deployment
conversion must also remove its automatic network/configuration/data changes,
not leave destructive actions behind without their former guards.

This reset removed software-version admission only. It did not remove database
schema-source admission: the owner migration run still refuses unknown schema
epochs, revisions and checksums, schema drift, and a nonempty database without
a trusted receipt. The sole previous database checkpoint is the fixed v1.2.0
release, defined in the
[database migration contract](../development/control-plane.md#current-sql-artifacts-and-bounded-upgrades);
it is an immutable schema source, not a minimum Controller or Agent version, and
it does not move when main advances. Empty-database initialization is not a way
to overwrite an existing database, and fresh and checkpoint-upgraded databases
must be schema-equivalent without having identical receipts.

Acceptance covers the current candidate on the supported architectures,
deployment modes and database products, with security, integration and resilience
checks. It does not certify historical upgrade/downgrade/migration paths beyond
the one bounded v1.2.0 database transition that the migration contract defines.
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
govern the v1.1.0 reset; historical release facts remain unchanged.
This document does not turn an unreleased candidate or an unrun check into a
production recommendation.

Release identities are stable `X.Y.Z` or candidate `X.Y.Z-rc.N` (positive N,
without leading zeros). RCs are candidates, not recommended stable versions.
RC and stable share the same Release Check. Final is a separate tag and Release;
RC tags and Releases remain in the history. Version ordering is not
execution authority. The explicitly chosen v1.1.0 release removes public
surfaces and compatibility guarantees despite its minor-version label; it must
not be described as fully backward compatible. Release builds use the
explicitly selected version tag.

[Release Check](../development/release-checks.md) validates current products.
Dispatch remains a non-publishing dry-run; tag-triggered publication builds
and smoke-tests products before publishing through the protected environment.
Explicit software lifecycle
operations use verified targets, not a minimum source version or a bridge
release; this does not apply to database schema admission, which is described
above. Package-manager behavior is not overridden. Unknown outcomes require
[incident recovery](../operations/incident-recovery.md), not blind replay.

## Supported platforms

Each row is owned by its authoritative document; this section is an index, not
a second support matrix.

| Surface | Supported scope | Authority |
| --- | --- | --- |
| Controller OS/architecture | Linux `amd64` and `arm64` release artifacts | [Production deployment](../operations/production-deployment.md) |
| Agent native packages | DEB on Ubuntu 24.04, RPM on Rocky 9, `amd64`/`arm64` | [Agent lifecycle](../operations/agent-lifecycle.md), [package validation](../development/release-checks.md) |
| Databases | PostgreSQL 18.x bundled or external; MySQL 8.4 LTS external | [Database support](../operations/production-deployment.md#database-support) |
| Managed ocserv | Adapter-admitted `1.2.x`, `1.3.x`, `1.4.x`, and `1.5.0` | See note below |
| Relays | Dedicated relay hosts running the matched vendored `iroh`/`iroh-relay` release | [Dedicated Relay](../how-to/dedicated-relay.md) |
| Authentication | Local only, OIDC only, or Local + OIDC | [Authentication](../operations/authentication.md) |
| Web console browsers | Chrome 111+, Safari 16.4+, Firefox 128+, Chromium 111+ derivatives; automated checks cover Chromium only | [Supported browsers](../development/web.md#supported-browsers) |

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
HA, multiple Relay failover, database replication clusters, PostgreSQL automatic failover
and PITR readiness are outside the supported scope. Rebind uses two independent
Controller deployments; it is not HA. See the
[resilience coverage and migration decision](../development/resilience.md).

Three pairs are deliberately not conflated:

1. **Single-relay recovery vs redundancy.** One dedicated Relay is supported.
   Its outage may interrupt management connections; recovery uses the original
   Relay, identity and command. Multiple Relay failover is outside support.
   See [dedicated Relay](../how-to/dedicated-relay.md).
2. **Package installed vs really managed online.** A node with packages
   installed and services stopped is not fleet-managed. Online management
   requires enrollment, Controller approval, a started matched node, and an
   authorized active session; fleet lifecycle evidence covers that full path.
   See [enroll a node](../how-to/enroll-node.md) and
   [agent lifecycle](../operations/agent-lifecycle.md).
3. **Backup created vs backup restorable.** Producing a backup artifact
   (PostgreSQL base backup, MySQL logical dump) is a scheduled
   operation; **restore is a separate, separately validated procedure**.
   A backup file existing, or a backup job succeeding, is not a verified
   restore, and a restore verifier passing is not schema admission (the owner
   migration run does that). PostgreSQL backups retain their required WAL and verification; isolated
   restore is separate from HA/PITR readiness. MySQL restore uses a new isolated
   server with the restore verifier, and redirecting a live Controller is a
   guarded manual cutover. See
   [PostgreSQL backup](../operations/database-backup-restore.md#postgresql),
   [MySQL backup](../operations/database-backup-restore.md#mysql).

## Security review posture

- Dependency advisories (Go `govulncheck`, Rust `cargo audit`/`cargo deny`
  including the relay lockfile, npm audit) and repository secret scanning run
  through the scheduled Security Checks workflow and must be green on the
  release candidate. See [SECURITY.md](../../SECURITY.md) for reporting and
  supported-version policy.
- **Image scanning:** Release `build-controller-images` scans every exact
  Controller release archive on both native architectures (amd64 and arm64),
  before image smoke and artifact upload, with pinned Syft and Grype tools and an updated
  vulnerability database. Temporary OS scan data is removed after the gate;
  it is not published as Release assets. A High or Critical OS finding with
  an available fix fails CI unless a reviewed entry in
  `deploy/production/image-scan-exemptions.json` matches the exact image,
  package, installed version, vulnerability id, actual digest-pinned base,
  reason and unexpired review date. A new CVE, changed package/base or expired
  review fails again. Findings without a fix remain visible in CI logs.
  Compiled first-party dependencies remain covered by the source advisory
  scans above. Formal Release builds, scans and smoke-tests after the manual CI
  gate; either architecture failing blocks publication. Business Smoke owns
  runtime acceptance and does not scan images.
- New database patch sets need migration, permission, artifact and backup/restore
  review plus candidate-specific evidence under
  [stable contracts](stable-contracts.md#database-and-recovery-matrix).
  Existing validated pins remain authoritative until adoption.
