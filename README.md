# ocservia

[![CI](https://github.com/GentleKingson/ocservia/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/GentleKingson/ocservia/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/GentleKingson/ocservia)](https://github.com/GentleKingson/ocservia/releases/latest)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](#license)
[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8.svg?logo=go)](control-plane/go.mod)
[![Rust](https://img.shields.io/badge/Rust-2024%20edition-black.svg?logo=rust)](rust/Cargo.toml)
[![Vue](https://img.shields.io/badge/Frontend-Vue%203%20%2B%20TS-4FC08D.svg?logo=vuedotjs)](web/package.json)

ocservia is a distributed management system for [ocserv](https://ocserv.openconnect-vpn.net/) / OpenConnect VPN server clusters. The administrative interface handles node health monitoring, user credential and quota provisioning, session inspection, and signed configuration distribution. Network traffic terminates directly at ocserv instances, so data plane packets bypass the central controller.

[Try it locally](#-try-it-locally-in-2-minutes) · [Operating model](#-what-makes-ocservia-different) · [Architecture](#-architecture-at-a-glance) · [Production deployment](#-deploying-to-production) · [Documentation](#-documentation)

## Features

- Node monitoring for connection states, daemon versions, active sessions, IP bans, and telemetry streams.
- Centralized user management, group policies, traffic quotas, and expiration controls.
- Signed operations and audit records for session termination and IP ban revocation.
- Pre-deployment configuration validation. The system rolls back automatically on failure, or leaves the node for manual recovery if the rollback fails.
- Node enrollment uses single-use tokens and signed endpoint proofs, and requires manual Controller approval.
- Node installation, upgrades, rollbacks, and removals for verified releases.

<a id="-what-makes-ocservia-different"></a>
## Operating model

### VPN traffic stays on ocserv

The Controller does not route VPN traffic. If the Controller fails, management workflows stop, but active client VPN tunnels stay up because they terminate directly on the ocserv nodes.

### Privileged operations are bounded

The node agent runs as an unprivileged process. A dedicated daemon (`privd`) handles administrative actions that require root privileges, communicating with the agent over an authenticated Unix domain socket. The daemon exposes a fixed set of ocserv maintenance tasks; it does not run arbitrary binaries or shell commands.

### Sensitive changes require independent approval

Two people must approve sensitive changes using separate credentials and active sessions, preventing self-approval. Both authorizations are cryptographically bound to the command payload.

<a id="-architecture-at-a-glance"></a>
## Architecture

Clients use an HTTPS REST API to send commands and receive server-sent events (SSE) for state updates. The Go Controller talks to `transportd` over a local gRPC Unix domain socket. `transportd` manages the Iroh endpoint and connects to remote agents.

Production deployments use a single Relay. Multi-relay failover is not supported. The Relay handles network routing and EndpointID verification, working alongside Controller-signed session credentials and payload signatures.

```text
Browser (Vue 3 / TypeScript)
  |
  | HTTPS API / SSE
  v
Controller (Go) -----------------> PostgreSQL / MySQL
  |
  | gRPC over local Unix socket
  v
transportd (Rust / Iroh)
  |
  | Iroh connection, with one dedicated Relay
  v
Agent (Rust, unprivileged)
  |
  | Authenticated local Unix socket
  v
privd (Rust, privileged)
  |
  | Fixed ocserv-related operations
  v
ocserv
```

The Controller handles APIs, schedules jobs, manages state transitions, and writes audit logs. Node agents send periodic health telemetry, and `privd` applies node changes. PostgreSQL 18.x is the default database, and the Controller also supports MySQL 8.4 LTS. See the [support policy](docs/reference/support-policy.md) for supported database versions and backup boundaries.

See the [architecture and trust model](docs/architecture.md) for process boundaries and threat assumptions.

<a id="-try-it-locally-in-2-minutes"></a>
## Try it locally

The development environment runs the Controller, web interface, database, and synthetic agent nodes using Docker Compose. It does not bind to an active ocserv daemon. You need Git, Docker Compose, and free ports on 4173 and 8080.

```bash
git clone https://github.com/GentleKingson/ocservia.git
cd ocservia

# Launch local stack with simulated nodes
deploy/compose/compose.sh up --build -d
```

The web console runs at [http://127.0.0.1:4173](http://127.0.0.1:4173). You can check Controller readiness and build metadata at `http://127.0.0.1:8080/readyz` and `http://127.0.0.1:8080/version`.

To stop the environment and delete storage volumes, run `deploy/compose/compose.sh down --volumes`. See [Try ocservia locally](docs/getting-started/local-development.md) for full setup instructions.

<a id="-deploying-to-production"></a>
## Deploying to production

### 1. Deploy the Controller

Keep your production configuration files in a workspace outside the repository. Replace `vX.Y.Z` with an official release tag:

```bash
# Clone the pinned release
git clone --branch vX.Y.Z --single-branch --depth 1 \
  https://github.com/GentleKingson/ocservia.git ocservia-vX.Y.Z

mkdir ocservia-install && cd ocservia-install
cp ../ocservia-vX.Y.Z/install.env.example install.env
$EDITOR install.env

# Run the verified bootstrap
../ocservia-vX.Y.Z/deploy/production/controller-bootstrap.sh --version vX.Y.Z
```

The Controller supports local credentials, OpenID Connect (OIDC), or both. When you first deploy locally, you must create an admin account using a single-use bootstrap token. See [Production authentication](docs/operations/authentication.md) for details.

Standalone configurations run the Relay and Signer services on separate hosts. You can also run them on the Controller host, which requires extra configuration and permissions. Provision your secrets according to [Deploy the Controller](docs/getting-started/production.md) before bootstrapping.

### 2. Install a managed node

Run on each target ocserv host:

```bash
git clone --branch vX.Y.Z --single-branch --depth 1 \
  https://github.com/GentleKingson/ocservia.git ocservia-vX.Y.Z

mkdir ocservia-node-install && cd ocservia-node-install
cp ../ocservia-vX.Y.Z/install.env.example install.env
$EDITOR install.env

../ocservia-vX.Y.Z/deploy/managed-node/install.sh
```

If you run the script without an enrollment token, it outputs `ENROLLMENT_READY`. If you provide a token, the state changes to `ENROLLED_LOCAL`. This registers the node locally but does not grant network access. To finish onboarding, approve the node in the Controller interface, start the daemon processes, and check for incoming telemetry.

The installer does not auto-approve nodes or start daemons. See [Install a managed node](docs/getting-started/managed-node.md) and [Enroll a node](docs/how-to/enroll-node.md) for the full workflow.

<a id="-documentation"></a>
## Documentation

| Topic | Description |
| :--- | :--- |
| [Getting started](docs/README.md) | Local setup, production deployment, and first enrollment. |
| [Architecture and trust](docs/architecture.md) | System topology, trust boundaries, and process privileges. |
| [Operations and runbooks](docs/operations/production-deployment.md) | Deployment lifecycle, database configuration, and recovery references. |
| [Troubleshooting](docs/how-to/troubleshooting.md) | Startup, OIDC, telemetry, and enrollment diagnostics. |
| [Support and versioning policy](docs/reference/support-policy.md) | Supported platforms, deployment scope, and cross-version limits. |
| [Technical reference](docs/README.md#technical-reference) | API schemas, protocols, and implementation contracts. |

## Development

Toolchain versions are pinned in the repository. To install dependencies and run tests:

```bash
make bootstrap
make verify
```

See [Contributor validation](docs/development/testing.md) for prerequisites, test suites, and CI workflows.

## Releases and compatibility

Starting with [`v1.1.0`](https://github.com/GentleKingson/ocservia/releases/tag/v1.1.0), the project no longer tests compatibility between different versions. A successful install, upgrade, or rollback does not guarantee compatibility with the existing database state. Check the [support and versioning policy](docs/reference/support-policy.md) before deploying changes.

Pin production deployments to official [published releases](https://github.com/GentleKingson/ocservia/releases). Do not run development branches or release candidates in production. The [P1 resilience and capacity harness](docs/development/p1-resilience-capacity.md) simulates load and injects faults for synthetic agents, but you should still test releases on your own infrastructure.

## Security

Administrative commands require cryptographic authorization. Root execution on the node is restricted to a fixed set of operations. Sensitive changes require multi-party approval, replay protection, a command ledger, and signed receipts. If a command leaves the system in an ambiguous state, it blocks conflicting changes until the state is reconciled.

Please report vulnerabilities privately following [SECURITY.md](SECURITY.md).

## Community and support

- Open a [GitHub Issue](https://github.com/GentleKingson/ocservia/issues) for bug reports and questions. Include reproduction steps, your environment, and logs.
- Find release builds and archives on the [Releases page](https://github.com/GentleKingson/ocservia/releases).

<a id="license"></a>
## License and acknowledgements

ocservia uses the [Apache License 2.0](LICENSE). Third-party software notices are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

ocservia is an independent project that interfaces with the [ocserv](https://ocserv.openconnect-vpn.net/) and OpenConnect ecosystem.
