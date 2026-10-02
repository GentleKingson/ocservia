# ocservia

[![CI](https://github.com/GentleKingson/ocservia/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/GentleKingson/ocservia/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/GentleKingson/ocservia)](https://github.com/GentleKingson/ocservia/releases/latest)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](#license)
[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8.svg?logo=go)](control-plane/go.mod)
[![Rust](https://img.shields.io/badge/Rust-2024%20edition-black.svg?logo=rust)](rust/Cargo.toml)
[![Vue](https://img.shields.io/badge/Frontend-Vue%203%20%2B%20TS-4FC08D.svg?logo=vuedotjs)](web/package.json)

ocservia is a distributed management system for [ocserv](https://ocserv.openconnect-vpn.net/) / OpenConnect VPN server clusters. The administrative interface provides node health monitoring, user credential and quota provisioning, session inspection, and signed configuration distribution. Network traffic terminates directly at ocserv instances; data plane packets bypass the central controller.

[Try it locally](#-try-it-locally-in-2-minutes) · [Operating model](#-what-makes-ocservia-different) · [Architecture](#-architecture-at-a-glance) · [Production deployment](#-deploying-to-production) · [Documentation](#-documentation)

## Features

- Node monitoring covering connection states, daemon versions, active sessions, IP bans, and telemetry streams.
- User management, group policies, traffic quotas, and expiration controls centralized in the Controller.
- Session termination and IP ban revocation executed via signed operations with audit records.
- Pre-deployment configuration validation with automatic rollback upon failure, deferring to manual operator recovery if restoration fails.
- Node enrollment secured by single-use tokens and signed endpoint-possession proofs, completed by manual Controller approval.
- Lifecycle management (installation, upgrades, rollbacks, and removals) targeting verified releases without implicit cross-version guarantees.

<a id="-what-makes-ocservia-different"></a>
## Operating model

### VPN traffic stays on ocserv

The Controller operates strictly out-of-band and does not route VPN traffic. An interruption or failure of the Controller affects management workflows only; active client VPN tunnels terminate directly on ocserv and persist unaffected, barring underlying node hardware faults or daemon-level restarts.

### Privileged operations are bounded

The node agent runs as an unprivileged process. Administrative actions requiring root privileges are handled by a dedicated daemon (`privd`) that communicates with the agent over an authenticated Unix domain socket. The daemon exposes a closed, predefined set of ocserv maintenance tasks, preventing the execution of arbitrary binaries or shell invocations.

### Sensitive changes require independent approval

High-impact operations require dual authorization cryptographically bound to the target command payload. Dispatch requires separate authentication from distinct principals holding independent credentials and active sessions, prohibiting self-approval. This mechanism establishes logical separation of duty, while assuming the security of the underlying credential issuance.

<a id="-architecture-at-a-glance"></a>
## Architecture

Clients interact with the Controller via an HTTPS REST API for command dispatch and server-sent events (SSE) for streaming state updates. The Go Controller communicates with `transportd` through a local gRPC Unix domain socket; `transportd` manages the Iroh endpoint and establishes peer connections to remote agents.

Production topologies specify a single dedicated Relay. Network routing via the Relay and EndpointID verification operate in tandem with Controller-signed session credentials and payload signatures. Multi-relay failover is explicitly unsupported.

```text
Browser (Vue 3 / TypeScript)
  |
  | HTTPS API / SSE
  v
Controller (Go) -----------------> PostgreSQL / MySQL / MariaDB
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

The Controller coordinates API handling, job scheduling, state-machine transitions, and immutable audit logs. Node agents publish periodic health telemetry, and `privd` applies authorized node changes. PostgreSQL serves as the default database backend, with MySQL 8.4 and MariaDB 12.3 supported for external instances. Supported database releases and backup boundaries are documented in the [support policy](docs/reference/support-policy.md).

See the [architecture and trust model](docs/architecture.md) for comprehensive process boundaries and threat assumptions.

<a id="-try-it-locally-in-2-minutes"></a>
## Try it locally

The development environment provisions the Controller, administrative web interface, database, and synthetic agent nodes using Docker Compose, without binding to an active ocserv daemon. Local deployment requires Git, Docker Compose, and unbound ports 4173 and 8080.

```bash
git clone https://github.com/GentleKingson/ocservia.git
cd ocservia

# Launch local stack with simulated nodes
deploy/compose/compose.sh up --build -d
```

The web console binds to [http://127.0.0.1:4173](http://127.0.0.1:4173). Controller readiness and build metadata can be queried at `http://127.0.0.1:8080/readyz` and `http://127.0.0.1:8080/version`.

To terminate the environment and purge persistent storage volumes, execute `deploy/compose/compose.sh down --volumes`. Comprehensive setup instructions are provided in [Try ocservia locally](docs/getting-started/local-development.md).

<a id="-deploying-to-production"></a>
## Deploying to production

### 1. Deploy the Controller

Store production configuration files in a dedicated workspace outside the repository directory. Substitute `vX.Y.Z` with an official release tag:

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

Authentication supports local credentials, OpenID Connect (OIDC), or hybrid local and OIDC modes. In initial local deployments, administrative credentials must be established via the single-use bootstrap token workflow described in [Production authentication](docs/operations/authentication.md).

In standalone configurations, the Relay and Signer services reside on isolated hosts. Alternatively, integrated configurations colocate these services on the Controller host, which requires explicit configuration parameters and elevated lifecycle permissions. Secret material must be provisioned according to [Deploy the Controller](docs/getting-started/production.md) prior to bootstrapping.

### 2. Install a managed node

Execute on each target ocserv host:

```bash
git clone --branch vX.Y.Z --single-branch --depth 1 \
  https://github.com/GentleKingson/ocservia.git ocservia-vX.Y.Z

mkdir ocservia-node-install && cd ocservia-node-install
cp ../ocservia-vX.Y.Z/install.env.example install.env
$EDITOR install.env

../ocservia-vX.Y.Z/deploy/managed-node/install.sh
```

If invoked without an enrollment token, the installer outputs `ENROLLMENT_READY`. Supplying a valid token advances the state to `ENROLLED_LOCAL`, indicating local registration without granting authorization or network access. To complete onboarding, inspect the node within the Controller interface, confirm approval, start daemon processes manually, and observe incoming telemetry.

The installation script intentionally omits automatic approval and daemon startup. Detailed workflows are documented in [Install a managed node](docs/getting-started/managed-node.md) and [Enroll a node](docs/how-to/enroll-node.md).

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

Toolchain versions are pinned across the repository. To install requisite dependencies and execute test validation:

```bash
make bootstrap
make verify
```

Refer to [Contributor validation](docs/development/testing.md) for toolchain prerequisites, component-specific test suites, and continuous integration workflows.

## Releases and compatibility

Effective as of [`v1.1.0`](https://github.com/GentleKingson/ocservia/releases/tag/v1.1.0), the project deprecated automated compatibility validation across heterogeneous versions. Although published under a minor version identifier, this represents a backward-incompatible governance shift: validation of an installation, upgrade, or rollback target does not guarantee runtime compatibility with existing database state. Deployments must adhere to the [current support and versioning policy](docs/reference/support-policy.md).

Production deployments must pin specific, official [published releases](https://github.com/GentleKingson/ocservia/releases). Development branches and release candidates are unsuitable for production environments. The [P1 resilience and capacity harness](docs/development/p1-resilience-capacity.md) provides synthetic load and fault injection for simulated agents; it does not substitute for empirical qualification on production node infrastructure.

## Security

Administrative commands dispatched across the management plane require cryptographic authorization, and local root execution is restricted to an invariant set of daemon operations. High-risk administrative transitions require multi-party approval, replay protection, durable command ledgers, and signed execution receipts. If command execution yields an ambiguous state, subsequent conflicting transitions are blocked until state reconciliation completes.

Vulnerability disclosures must be submitted privately following the guidelines in [SECURITY.md](SECURITY.md), rather than via public issue trackers.

## Community and support

- Bug reports and technical inquiries should be submitted via [GitHub Issues](https://github.com/GentleKingson/ocservia/issues), accompanied by reproduction steps, environment specifications, and sanitized logs.
- Release artifacts and distribution archives are published on the [Releases page](https://github.com/GentleKingson/ocservia/releases).

<a id="license"></a>
## License and acknowledgements

ocservia is distributed under the [Apache License 2.0](LICENSE). Third-party software notices and attribution are detailed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

ocservia is an independent project that interfaces with the [ocserv](https://ocserv.openconnect-vpn.net/) and OpenConnect ecosystem.
