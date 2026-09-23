# Changelog

All notable changes to ocservia are documented in this file. Release dates and
per-release details are on the
[GitHub Releases](https://github.com/GentleKingson/ocservia/releases) page;
this file records the change categories per release line. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and ocservia uses
[Semantic Versioning](https://semver.org/).

## [1.0.1]

Designated as the first recommended stable production baseline once published.
`v1.0.0` remains a supported in-place upgrade source; pre-1.0 installations
must be redeployed to enter `1.x`. See the
[1.0 support and versioning policy](docs/reference/support-policy.md).

### Added

- Controller image security gate with per-image, per-architecture SPDX SBOMs
  and vulnerability reports bound to the exact release images. Fixable High
  and Critical OS-package findings block publication unless covered by an
  explicit, unexpired exemption.
- Registered `v1.0.0` native upgrade baseline and application compatibility
  checks for published `v1.0.0` nodes against the candidate Controller on
  `amd64` and `arm64`.

### Changed

- Unified release acceptance builds candidate products once and reuses them
  for upgrade, compatibility, business, and image-security checks before
  publication. Integration and resilience checks are selected from the
  complete change set since the last published stable release.

### Fixed

- Controller bootstrap clones public source files with container-readable
  permissions, so PostgreSQL initialization and OTel configuration work for
  non-root container users. Source roots, secrets, and lifecycle state keep
  their protected permissions.
- G6 database connection-growth measurement uses a bounded warm-up baseline;
  resource-sampler failures retain diagnostic details.

## [1.0.0]

This section identifies the release line, not its publication status. The
actual GitHub Release determines publication and the release date.

### Added

- Complete-configuration planning for matched nodes: complete-file ConfigPlan
  with the node-local TLS profile, explicit capability negotiation, logical,
  materialized, and command hashes, transactionally bound approval, exact
  rollback, and durable recovery. Root rejects changed non-reloadable
  startup bindings before effects; initial activation and TLS path migration
  require separately authorized maintenance.
- Browser-based approval review and decision for configuration and
  certificate workflows, including fresh-certificate approvals and immutable
  generated summary maps.
- Published 1.0 stable-contract inventory and finite compatibility matrix:
  frozen HTTP, CLI/configuration, installation, package-name, protocol,
  signing, and durable-state contract surfaces with reviewed rolling-window
  exclusions.
- Verified native upgrade baseline for `v0.6.2`, registered with checksum
  manifests and independently anchored key pins.
- Formal G6 production-readiness acceptance evidence: two-failure-domain HA
  with PostgreSQL primary failure, old-primary write fencing, PITR restore,
  scheduler and connection-owner takeover with stale-term rejection, the three
  outbox crash-window recoveries, dual-relay authenticated failover,
  direct↔relay transitions, reconnect-storm recovery, and a continuously
  sampled fault-free window with fifty real Agents under the frozen SLO
  contract.
- 1.0 support and versioning policy: SemVer scope, deprecation windows,
  Controller/node version windows, platform support index, deployment
  capability distinctions, and security review posture.

### Changed

- Transport security baseline: vendored `iroh`/`iroh-relay` pinned to `1.2.0`
  with a hardened relay configuration baseline.
- Controller HTTP modules, Web feature ownership, and API transport were
  reorganized internally; the public `/api/v1` contract and the locked HTTP
  behavior baseline are unchanged.

### Fixed

- ocserv adapter cancellation hardening, with real ocserv `1.5.0`
  compatibility validation (concurrent mutations, replay, live-session
  termination, and invalid-configuration rejection coverage).
- ConfigPlan now binds the current configuration revision instead of a stale
  one, and restored operations bind to the selected certificate.
- Node-detail asynchronous workflows are isolated from unrelated Web state.
- Strict-wire contract coverage completed for all production command payloads;
  unknown fields, wrong wire types, and truncated nested messages are rejected
  before journal or effect.

## [0.6.2] - 2026-09-19

Bundled PostgreSQL initialization no longer passes application or backup
passwords through process arguments. See the
[v0.6.2 release notes](https://github.com/GentleKingson/ocservia/releases/tag/v0.6.2).

## [0.6.1] - 2026-09-16

Dedicated relay deployment: a single authenticated relay is supported (without
redundancy), a dedicated relay-egress network separates external relay
connectivity, and package lifecycle handles the versioned Agent relay launcher
and systemd drop-in. See the
[v0.6.1 release notes](https://github.com/GentleKingson/ocservia/releases/tag/v0.6.1).

## [0.6.0] - 2026-09-15

Unified local/OIDC authentication, PostgreSQL database boundaries with external
MySQL/MariaDB support and backup/restore workflows, shared native Agent payload
builds at the Rocky Linux 9 / glibc 2.34 ABI floor with real DEB/RPM lifecycle
checks, and manual native upgrade validation against pinned `v0.5.2` assets.
See the
[v0.6.0 release notes](https://github.com/GentleKingson/ocservia/releases/tag/v0.6.0).
