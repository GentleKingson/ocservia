# ocservia

[![CI](https://github.com/GentleKingson/ocservia/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/GentleKingson/ocservia/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/GentleKingson/ocservia)](https://github.com/GentleKingson/ocservia/releases/latest)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](#license)

**ocservia is a self-hosted Web console and API for managing multiple [ocserv](https://ocserv.openconnect-vpn.net/) / OpenConnect VPN servers.**

ocservia does not replace ocserv and does not carry VPN traffic. Each VPN server still runs ocserv. ocservia gives operators one place to deploy managed nodes, watch their health, manage users and groups, review sessions and bans, prepare configuration changes, and run controlled upgrades.

> [!NOTE]
> ocservia is pre-1.0. Use it in controlled deployments, pin the release version you install, and read the linked deployment guides before production use.

## Core features

- **Fleet view** — see all managed ocserv servers, their online state, ocserv status, versions, sessions, bans, and recent health data.
- **User and group management** — manage desired users, groups, quotas, and expiry dates from the Controller.
- **Session and ban actions** — disconnect sessions, terminate sessions, unban IP addresses, and reload the fixed ocserv service path with audit records.
- **Configuration workflow** — render and validate ocserv configuration before applying it, with review and rollback paths for risky changes.
- **Node enrollment** — install a managed node, enroll it to the Controller, approve it, and then start the node services deliberately.
- **Lifecycle operations** — install, upgrade, roll back, and uninstall the Controller and managed nodes through pinned releases and signed packages.

## Architecture at a glance

```text
Operator browser
      |
      v
Controller Web/API  --->  PostgreSQL and backups
      |
      v
Dedicated relays
      |
      v
Managed node services  --->  local ocserv server
```

The main pieces are:

- **Controller** — the central Web console, API, database access, scheduling, audit records, and lifecycle commands.
- **Managed node** — the services installed beside ocserv on each VPN server so the Controller can observe and manage it.
- **Dedicated relays** — the network path used by the Controller and managed nodes to communicate in production.
- **PostgreSQL and backups** — persistent Controller data and recovery state.
- **External services** — login, certificates, and monitoring endpoints prepared by the operator.

See [Architecture](docs/architecture.md) for the plain-language system diagram and trust model.

## Try it locally

The local stack is for exploration. It uses simulated nodes and does not connect to a real ocserv server.

Prerequisites: Git, Docker Engine with the Compose v2 plugin or Docker Desktop, and free local ports `4173` and `8080`.

```bash
git clone https://github.com/GentleKingson/ocservia.git
cd ocservia

docker compose -f deploy/compose/compose.yaml up --build -d
```

Open the Web console at `http://127.0.0.1:4173`. The Controller exposes `http://127.0.0.1:8080/livez`, `/readyz`, and `/version`.

See [Try ocservia locally](docs/getting-started/local-development.md) for stop commands and local notes.

## Deploy the Controller

Use an exact release tag and keep your local configuration outside the release checkout:

```bash
git clone --branch vX.Y.Z --single-branch --depth 1 \
  https://github.com/GentleKingson/ocservia.git ocservia-vX.Y.Z
mkdir ocservia-install && cd ocservia-install
cp ../ocservia-vX.Y.Z/install.env.example install.env
editor install.env

../ocservia-vX.Y.Z/deploy/production/controller-bootstrap.sh \
  --version vX.Y.Z
```

Edit only the Controller section in `install.env`. The template references operator-provided secrets, keys, certificates, relay details, and backup paths; it does not generate them for you.

See [Deploy the Controller](docs/getting-started/production.md) for the full deployment path.

## Install a managed node

Run this on each ocserv server that should be managed by the Controller:

```bash
git clone --branch vX.Y.Z --single-branch --depth 1 \
  https://github.com/GentleKingson/ocservia.git ocservia-vX.Y.Z
mkdir ocservia-node-install && cd ocservia-node-install
cp ../ocservia-vX.Y.Z/install.env.example install.env
editor install.env

../ocservia-vX.Y.Z/deploy/managed-node/install.sh
```

Edit only the managed-node section in `install.env`. After the installer reaches the pending approval state, approve the node in the Controller and then start the node services.

See [Install a managed node](docs/getting-started/managed-node.md) and [Enroll a node](docs/how-to/enroll-node.md).

## Documentation

- [Documentation index](docs/README.md)
- [Try locally](docs/getting-started/local-development.md)
- [Deploy the Controller](docs/getting-started/production.md)
- [Install a managed node](docs/getting-started/managed-node.md)
- [Architecture](docs/architecture.md)
- [Troubleshooting](docs/how-to/troubleshooting.md)
- [Technical reference](docs/reference/README.md)

## Developing

The repository pins its toolchains. Bootstrap them and run the local validation baseline:

```bash
make bootstrap
make verify
```

GitHub Actions remains the merge-time validation environment. See [Contributor validation](docs/development/testing.md) for details.

## Status

ocservia is pre-1.0. Minor release lines may introduce compatibility changes, and production validation remains the operator's responsibility. See [Releases](https://github.com/GentleKingson/ocservia/releases) for published versions and assets.

The P1 harness provides manual, single-host validation outside Basic CI, not a production capacity guarantee. See [P1 resilience and capacity](docs/development/p1-resilience-capacity.md) for details.

## Security

ocservia is designed to avoid broad remote administration. It has no remote shell path, privileged operations are limited to fixed ocserv actions, and high-risk changes are recorded and reviewed.

Report security issues privately as described in [SECURITY.md](SECURITY.md). Do not disclose vulnerabilities in public issues or pull requests.

## Support

Use [GitHub issues](https://github.com/GentleKingson/ocservia/issues) for reproducible non-security bugs and scoped feature discussions. Include the exact commit, environment, expected behavior, actual behavior, and redacted diagnostics.

## License

Unless otherwise noted, ocservia is licensed under the Apache License 2.0. See [LICENSE](LICENSE) for the full text and [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) for third-party attribution.

## Acknowledgements

ocservia builds on the work of the ocserv and OpenConnect communities and the wider open-source ecosystem. ocservia is an independent project.
