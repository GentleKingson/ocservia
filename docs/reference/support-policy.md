# Support and versioning policy

## v1.1.0 policy reset

This policy took effect with the [`v1.1.0` release](https://github.com/GentleKingson/ocservia/releases/tag/v1.1.0). The `v1.1.0` release notes link the acceptance and publication runs for that tag, while every subsequent candidate requires its own validation run. This policy reset explicitly supersedes the version windows, deprecation schedule, and additive-only `1.x` compatibility promises of the archived 1.0 policy, while leaving previously published git tags, release artifacts, and historical records intact.

Starting in `v1.1.0`, the codebase removes software upgrade, downgrade, and rollback version admission gates, historical migration compatibility probes, automatic conversion of legacy deployments, and multi-release compatibility matrices. The public `--schema-compatibility-check` CLI flag and the `/readyz` response's `schema_version` field have also been removed; readiness probes now check zero-row reads against current core tables alongside event-stream health without inspecting migration history ranges. Install, upgrade, and rollback remain explicit lifecycle commands targeting verified release artifacts, and while version strings are still validated and displayed, numeric version ordering is not used to admit or reject an operation.

Removing software-version admission does not relax runtime security or state invariants. Command signatures, trusted keys, Controller-authorized upgrade digests and target architectures, negotiated command capabilities, RBAC and dual-principal approvals, idempotency, replay protection, owner fences, Signer identity and monotonic revisions, and atomic database transactions all remain mandatory. Likewise, removing legacy deployment auto-conversion means removing the automatic network, configuration, and data mutations that accompanied it.

Database schema-source admission also remains strictly enforced: the owner migration runner rejects unknown schema epochs, revisions, or checksums, schema drift, and non-empty databases that lack a valid migration receipt. The only supported prior database checkpoint is the fixed `v1.2.0` schema defined in the [database migration contract](../development/control-plane.md#current-sql-artifacts-and-bounded-upgrades), which serves as an immutable schema baseline and does not define a minimum Controller or Agent runtime version. Freshly initialized databases and databases upgraded from the `v1.2.0` checkpoint must converge to the exact same schema while retaining their respective execution receipts, and empty-database initialization never overwrites an existing database.

Release acceptance validates the current candidate across the supported architectures, deployment modes, and database engines through security, integration, and resilience suites. Beyond the single bounded `v1.2.0` database schema transition, historical upgrade, downgrade, and migration paths are not tested. Running mismatched component versions can fail or corrupt state even without a version-admission gate; operators must never reverse database migrations, reset node or Signer identities, or overwrite persistent state to force a rollout, and changing deployment topologies requires a clean redeployment. Release notes must list removed public entrypoints and state explicitly that `v1.1.0` is a policy-breaking release despite its minor version number.

## Published history and version format

The [published 1.0 policy](https://github.com/GentleKingson/ocservia/blob/d420b22018596d6741d55fa56bd19c4a767e5817/docs/reference/support-policy.md) remains in git history as the record of the original 1.0 line, without governing `v1.1.0` or later releases. Nothing in this policy certifies an unreleased commit or an unexecuted validation suite for production use.

Release tags follow `X.Y.Z` for stable releases and `X.Y.Z-rc.N` (with a positive integer `N` and no leading zeros) for release candidates. Both RC and stable builds run the same [Release Check](../development/release-checks.md) workflow, where `workflow_dispatch` runs act as non-publishing dry runs and tag-triggered runs build, scan, and smoke-test artifacts before publishing through the protected release environment. Promoting an RC to a final release creates a separate `X.Y.Z` tag and GitHub Release while retaining the RC tags in history.

Lifecycle commands operate on explicitly pinned release artifacts without relying on bridge releases or overriding OS package-manager rules, while database schema admission continues to follow the migration contract above. Commands that settle in an `Unknown` state must be handled through [incident recovery](../operations/incident-recovery.md) without blind replay.

## Supported platforms

Each row below summarizes the current validated baseline and links to the document that owns its detailed requirements:

| Surface | Supported scope | Authority |
| --- | --- | --- |
| Controller OS/architecture | Linux `amd64` and `arm64` release artifacts | [Production deployment](../operations/production-deployment.md) |
| Agent native packages | DEB on Ubuntu 24.04, RPM on Rocky 9, `amd64`/`arm64` | [Agent lifecycle](../operations/agent-lifecycle.md), [package validation](../development/release-checks.md) |
| Databases | PostgreSQL 18.x bundled or external; MySQL 8.4 LTS external | [Database support](../operations/production-deployment.md#database-support) |
| Managed ocserv | Adapter-admitted `1.2.x`, `1.3.x`, `1.4.x`, and `1.5.0` | See note below |
| Relays | Dedicated relay hosts running the matched vendored `iroh`/`iroh-relay` release | [Dedicated Relay](../how-to/dedicated-relay.md) |
| Authentication | Local only, OIDC only, or Local + OIDC | [Authentication](../operations/authentication.md) |
| Web console browsers | Chrome 111+, Safari 16.4+, Firefox 128+, Chromium 111+ derivatives; Quick checks Chromium, Full adds Playwright Firefox and WebKit | [Supported browsers](../development/web.md#supported-browsers) |

On managed nodes, the ocserv adapter accepts version strings matching `1.2.x`, `1.3.x`, `1.4.x`, and `1.5.0` from live `ocserv` output. Full release acceptance (native adapter integration plus real-VPN business validation) runs against `1.5.0`, whereas the `1.2.x`, `1.3.x`, and `1.4.x` lines share the same adapter code paths and parser regression fixtures without separate per-line release certification. Future `ocserv` versions require explicit adapter and compatibility review before admission, and new database patch releases or OS distributions are adopted only after artifact and migration verification.

## Deployment capability distinctions

Every supported production deployment runs a single Controller instance, one relational database, and one dedicated Relay (either on separate hosts in Standalone mode or co-located in Integrated mode, with bundled PostgreSQL or external PostgreSQL/MySQL), managing one or more node Agents. Controller high availability, multi-relay failover, database replication clusters, automatic PostgreSQL failover, and point-in-time recovery (PITR) are outside the supported scope, and Controller rebind moves nodes between two independent single-Controller deployments without providing active-passive HA; see [resilience coverage and migration decision](../development/resilience.md).

In addition, operational readiness distinguishes between three pairs of states that are often conflated:

1. Single-relay recovery versus multi-relay redundancy: production supports a single dedicated Relay. If that Relay goes offline, relay-routed control connections pause until the same Relay host and identity recover and pending commands are reconciled; automatic failover to a second Relay is not supported (see [dedicated Relay](../how-to/dedicated-relay.md)).
2. Package installation versus active online management: installing the node package and leaving its services stopped does not put a host under fleet management. A node is actively managed only after completing enrollment, receiving Controller approval, starting the matched `Agent` and `privd` daemons, and establishing an authorized transport session (see [enroll a node](../how-to/enroll-node.md) and [agent lifecycle](../operations/agent-lifecycle.md)).
3. Backup creation versus verified restoration: scheduled backups produce PostgreSQL base backups or MySQL logical dumps, whereas restoration is a separate procedure that must be validated in an isolated environment. Neither a completed backup job nor a passing restore-verifier check substitutes for owner-role schema migration admission, and redirecting a live Controller to a restored MySQL instance requires a manual cutover (see [PostgreSQL backup](../operations/database-backup-restore.md#postgresql) and [MySQL backup](../operations/database-backup-restore.md#mysql)).

## Security review posture

- Source dependency advisories (`govulncheck` for Go, `cargo audit` and `cargo deny` for Rust including the relay lockfile, and `npm audit` for Web) along with repository secret scanning run in the scheduled Security Checks workflow and must pass on the release candidate; vulnerability reporting and supported versions are covered in [SECURITY.md](../../SECURITY.md).
- During release builds, `build-controller-images` scans every built Controller image archive on both native architectures (`amd64` and `arm64`) using pinned Syft and Grype versions against a freshly updated vulnerability database before running image smoke tests or uploading release assets (temporary scan databases are discarded after the job). Any High or Critical OS vulnerability with an upstream fix fails the build unless a non-expired entry in `deploy/production/image-scan-exemptions.json` matches the exact image, package name, installed version, CVE ID, digest-pinned base image, and documented rationale; unfixed findings remain visible in the CI logs, and a failure on either architecture blocks release publication.
- Updating pinned database images or patch levels requires migration, role-permission, SQL-artifact, and backup/restore verification under [stable contracts](stable-contracts.md#database-and-recovery-matrix) before replacing the existing pins.
