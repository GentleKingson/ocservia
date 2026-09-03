# ocservia

[![CI](https://github.com/GentleKingson/ocservia/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/GentleKingson/ocservia/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/GentleKingson/ocservia)](https://github.com/GentleKingson/ocservia/releases/latest)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](#license)

**A self-hosted management plane for fleets of [ocserv](https://ocserv.openconnect-vpn.net/) / OpenConnect VPN servers.**

ocservia lets you operate several independent ocserv nodes from one Web console and API: fleet inventory and health, users and groups, sessions and IP bans, configuration workflows, and signed Agent packages — with enrollment, RBAC, and audit controls behind privileged actions, plus independent approvals for selected high-risk changes.

ocserv itself remains the VPN server and data plane. ocservia is not a VPN server, not a replacement for ocserv, and not a general-purpose remote-administration tool: there is no SSH, remote shell, or arbitrary command path. Every remote effect is typed, narrowly scoped, authorization-checked, and audited.

Under the hood, ocservia combines a Go control plane, Rust transport and node components, a Vue and TypeScript Web console, PostgreSQL, [Iroh](https://www.iroh.computer/) connectivity, and OpenTelemetry. See [Architecture](docs/architecture.md).

> [!NOTE]
> ocservia is pre-1.0 and intended for controlled deployment and evaluation. Compatibility may change between minor release lines. Production deployments are operator-validated, no production SLA is provided, and the production topology in this repository is a hardened reference, not a production-readiness guarantee. See [Releases](https://github.com/GentleKingson/ocservia/releases) for the latest published version.

## Features

- **Fleet visibility** — node inventory, online/offline state, ocserv status and version, sessions, IP bans, and telemetry with bounded history.
- **Users and groups** — desired user, group, quota, and UTC expiry state with convergence tracking and recovery evidence.
- **Sessions and bans** — audited session disconnect and terminate, IP unban, and a fixed `ocserv.service` reload.
- **Configuration management** — deterministic rendering, allowlisted directives, side-effect-free validation, secret-safe diffs, approved atomic apply, and rollback.
- **Enrollment and node trust** — one-time tokens, explicit node approval, EndpointID pinning, capability negotiation, and revocation.
- **RBAC, approvals, and audit** — OIDC login with PKCE, workspace-scoped roles, independent approvals for high-risk changes, and authenticated append-only audit chains.
- **Agent lifecycle and fleet upgrades** — signed packages, reconciled single-node upgrades with durable self-upgrade runs, and bounded rolling rollouts behind a mandatory canary.
- **Production operations** — an HTTPS-only reference deployment with dedicated relays, PostgreSQL backup and PITR, credential rotation, and guarded install, upgrade, rollback, and uninstall.

## Try it locally

The fastest way to explore ocservia is the side-effect-free development stack. It runs simulated Agents behind a bounded transport stub — it does not run the real Iroh transport, connect to a real ocserv server, or perform privileged operations.

Prerequisites: Git, Docker Engine with the Compose v2 plugin (or Docker Desktop), and free local ports `4173` and `8080`.

```bash
git clone https://github.com/GentleKingson/ocservia.git
cd ocservia

docker compose -f deploy/compose/compose.yaml up --build -d
```

Open the Web console at `http://127.0.0.1:4173`. The control plane serves `http://127.0.0.1:8080/livez`, `/readyz`, and `/version`.

Logs, teardown, simulator behavior, and configuration are described in [Control-plane development](docs/development/control-plane.md).

## Production deployment

Production deployments use the guarded Controller lifecycle in [`deploy/production/`](deploy/production/): digest-pinned images, protected file-backed secrets outside the checkout, and launcher-validated runtime paths. Direct `docker compose` invocation is not a supported production path.

Supported Controller hosts: Ubuntu 20.04/22.04/24.04/26.04 and Debian 11/12/13 on `amd64` or `arm64`. Ubuntu 20.04 requires an existing compatible Docker installation.

Install from a clean checkout of the release you are deploying — the lifecycle entrypoint verifies that the checkout HEAD matches the release manifest's `source_commit`:

```bash
git clone --branch <release-tag> --depth 1 \
  https://github.com/GentleKingson/ocservia.git
cd ocservia
```

With the production environment and secrets provisioned (see [Deploy the Controller](docs/getting-started/production.md)), one command bootstraps the host, downloads the release bundle for the host architecture, and installs the Controller:

```bash
deploy/production/install.sh
```

On a host that already has Docker, run it as the lifecycle launcher user with Docker daemon access already granted. On a fresh host without Docker, run `deploy/production/install.sh --root-lifecycle` instead — a deliberate whole-lifecycle-as-root install; the installer forwards only the allowlisted production `OCSERV_*` settings through a controlled `sudo env`, a fresh Docker installation grants no non-root daemon access, the installer never changes Docker permissions, and plain whole-script `sudo` without the flag stays rejected.

The underlying steps also remain available individually: `bootstrap-host.sh check|install` prepares host prerequisites, and `controller.sh install --release-file <manifest>` activates the manifest matching the host architecture. The same entrypoint manages `upgrade`, `rollback`, `start`, and `uninstall`. Start with [Deploy the Controller](docs/getting-started/production.md); the full install, upgrade, rollback, recovery, and security contracts remain in [Production deployment reference](docs/operations/production-deployment.md).

## Managed nodes

Each managed ocserv host runs the ocservia Agent (unprivileged) and `privd` (root, no network listener) as signed native packages: `.deb` (amd64, arm64), `.rpm` (x86_64, aarch64), and a signed `.tar.gz` archive, published with checksums and an Ed25519 signature in every [release](https://github.com/GentleKingson/ocservia/releases). Verification trusts a release-signing public key whose fingerprint is pinned out of band — never a key copied from the same bundle.

Install a production managed node with the one-command bootstrap — the self-contained `deploy/managed-node/install.sh` for the exact release, run with `--version vX.Y.Z` from any directory holding the node configuration (no Git checkout required; inside a clean release-tag checkout it also runs without the flag). The `--version` mode requires a release that ships this Stage-1 bootstrap; older releases keep the checkout-based flow. It verifies the release out of band, installs the matching native package, and stops at `ENROLLMENT_READY`, then at `PENDING_APPROVAL` after enrollment; the services start only after an operator independently approves the node.

Start with [Install a managed node](docs/getting-started/managed-node.md) and [Enroll a node](docs/how-to/enroll-node.md). Package, rollback, and trust internals remain in [Agent package lifecycle](docs/operations/agent-lifecycle.md) and [Node enrollment reference](docs/development/enrollment.md).

## Documentation

The documentation index lives in [`docs/README.md`](docs/README.md). Frequently used entry points:

- [Architecture and trust boundaries](docs/architecture.md)
- [Production deployment](docs/getting-started/production.md)
- [Managed node installation](docs/getting-started/managed-node.md)
- [Technical reference](docs/reference/README.md)
- [Control-plane development](docs/development/control-plane.md)
- [Contributor validation](docs/development/testing.md)

## Developing

The repository pins its toolchains. Bootstrap them and run the local validation baseline:

```bash
make bootstrap
make verify
```

GitHub Actions remains the authoritative merge-time environment. Changes must preserve the narrow-operation security boundary — never introduce a generic shell, arbitrary executable, caller-selected path, or caller-selected systemd unit — and keep public HTTP and Protobuf contracts additive. See [Contributor validation](docs/development/testing.md) and [Technical reference](docs/reference/README.md).

## Status

ocservia is pre-1.0: minor release lines may introduce compatibility changes, and no production SLA or capacity guarantee is provided. See [Releases](https://github.com/GentleKingson/ocservia/releases) for published versions and assets. The P1 harness exercises up to 500 side-effect-free simulated Agents on a single GitHub-hosted runner — initial engineering evidence, not a production capacity claim. Multi-host, long-duration, and deployment-specific validation remain the operator's responsibility.

## Security

ocservia is designed around explicit trust boundaries rather than broad remote administration. Agents run unprivileged; privileged effects cross a narrow, typed boundary — `privd` has no TCP listener and accepts no arbitrary program, path, service, or raw command; there is no remote shell; high-risk changes require approval by a different authorized principal; and business writes commit together with authenticated, append-only audit events. The full boundary reference is in [Architecture](docs/architecture.md#trust-boundaries).

Report security issues privately as described in [SECURITY.md](SECURITY.md). Do not disclose vulnerabilities in public issues or pull requests.

## Support

Use [GitHub issues](https://github.com/GentleKingson/ocservia/issues) for reproducible non-security bugs and scoped feature discussions. Include the exact commit, environment, expected and actual behavior, and redacted diagnostics.

## License

Unless otherwise noted, ocservia is licensed under the Apache License 2.0. See [LICENSE](LICENSE) for the full text and [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) for third-party attribution.

## Acknowledgements

ocservia builds on the work of the [ocserv](https://ocserv.openconnect-vpn.net/) and OpenConnect communities, [Iroh](https://www.iroh.computer/), PostgreSQL, OpenTelemetry, Go, Rust, Vue, and the wider open-source ecosystem. ocservia is an independent project and is not a general-purpose remote-management tool.
