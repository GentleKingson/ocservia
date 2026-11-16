# ocservia

[![CI](https://github.com/GentleKingson/ocservia/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/GentleKingson/ocservia/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/GentleKingson/ocservia)](https://github.com/GentleKingson/ocservia/releases/latest)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](#license)
[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8.svg?logo=go)](control-plane/go.mod)
[![Rust](https://img.shields.io/badge/Rust-2024%20edition-black.svg?logo=rust)](rust/Cargo.toml)
[![Vue](https://img.shields.io/badge/Frontend-Vue%203%20%2B%20TS-4FC08D.svg?logo=vuedotjs)](web/package.json)

ocservia is a central control plane for clusters of [ocserv](https://ocserv.openconnect-vpn.net/) (OpenConnect VPN) servers. From one web console or HTTP API, you can monitor node health, provision user credentials and traffic quotas, inspect active sessions, and distribute signed configuration updates, without opening a shell on each server.

ocservia does not replace ocserv. Every managed server keeps running ocserv, which terminates client tunnels and forwards their traffic. ocservia installs its node daemons alongside it and connects them to a central Controller.

[Try it locally](#-try-it-locally-in-2-minutes) · [Operating model](#-what-makes-ocservia-different) · [Architecture](#-architecture-at-a-glance) · [Production deployment](#-deploying-to-production) · [Documentation](#-documentation)

## Features

- Monitor node connection states, daemon versions, active sessions, IP bans, and telemetry streams.
- Manage user credentials centrally, with group policies, traffic quotas, and account expiration.
- Terminate sessions and revoke IP bans through cryptographically signed administrative operations, each with an audit record.
- Validate configuration before it is deployed. A failed deployment rolls back automatically; if the rollback itself fails, the node is left for manual recovery.
- Install and activate nodes with single-use tokens, signed endpoint proofs and automatic privd key registration.
- Install, upgrade, roll back, and remove managed nodes using verified, pinned releases.

<a id="-what-makes-ocservia-different"></a>
## Operating model

### VPN traffic stays on ocserv

Client VPN tunnels terminate directly on the ocserv nodes, so the Controller is never in the packet path. If the Controller becomes unavailable, management workflows pause, and client VPN sessions that are already established stay connected.

### Privileged operations are bounded

On each managed host, the node agent runs as an unprivileged process. When it needs a root-level action, it asks a local helper daemon, `privd`, over an authenticated Unix domain socket. `privd` exposes only a fixed set of ocserv maintenance tasks, so neither the agent nor the Controller can invoke arbitrary binaries or shell commands.

### One Admin can complete management operations

An authenticated Admin can manage nodes, users, configuration, certificates and upgrades without a second identity or approval. The Controller checks current permissions and binds signed commands to the requested content. Short-lived single-use installation tokens automate node activation and root-key registration while preserving TLS verification, key isolation, audit records and recovery checks.

### 1. Deploy the Controller

Download the installer from an exact GitHub Release (replace `vX.Y.Z`):

```bash
mkdir ocservia-install && cd ocservia-install
wget https://github.com/GentleKingson/ocservia/releases/download/vX.Y.Z/controller-bootstrap.sh -O install.sh
bash install.sh
```

The wizard detects the platform, collects Quick or Manual settings, checks prerequisites and asks for confirmation. It requires a TTY. For automation, use `bash install.sh --non-interactive --config ./install.env`; missing required configuration fails without prompting. Release scripts embed their version, so production installation needs neither Git nor a source checkout.

Operators sign in with local credentials, OpenID Connect (OIDC), or both. A fresh installation has no default administrator account. If you enable local login in a manual setup, create the first administrator with the separate one-shot bootstrap command described in [Production authentication](docs/operations/authentication.md#bootstrap-the-first-local-admin).

The default Standalone mode runs the Relay and the Signer on separate hosts. Integrated mode runs them on the Controller host under the root lifecycle and needs additional configuration. Before you run the bootstrap, provision the required secrets as described in [Production deployment](docs/operations/production-deployment.md#prepare-the-configuration).

For a new Integrated host, the Quick preset can generate the configuration, cryptographic material, and initial local administrator, as long as the stable or RC release you selected includes Quick mode. Releases up to v1.2.0 do not ship this preset (see [Quick mode](docs/operations/production-deployment.md#quick-mode)).

### 2. Install a managed node

In the Controller's **Nodes → Install node**, generate a short-lived installation token. On the ocserv host, download the matching release and enter the token at the hidden prompt:

```bash
mkdir ocservia-node-install && cd ocservia-node-install
wget --https-only --secure-protocol=TLSv1_2 https://github.com/GentleKingson/ocservia/releases/download/vX.Y.Z/managed-node-bootstrap.sh -O install.sh
bash install.sh --join-token
```

The installer securely retrieves configuration, installs the package, registers and activates the Agent, registers its root-owned privd key and starts the services. It reports `ONLINE` only after the Controller observes active trust and fresh telemetry. No second identity or confirmation is required. The host must trust the Controller's HTTPS certificate. See [Agent lifecycle](docs/operations/agent-lifecycle.md#one-token-installation) for prerequisites and safe interruption recovery.

<a id="-documentation"></a>
## Documentation

| Document | What it covers |
| :--- | :--- |
| [Architecture](docs/architecture.md) | Components, call chains, responsibility owners, and permission and state boundaries. |
| [Stable contracts](docs/reference/stable-contracts.md) | Rules that changes must keep: command delivery and receipts, identity and authorization, database artifacts, and protocol generation. |
| [Production deployment](docs/operations/production-deployment.md) | Support scope, Controller and Relay installation in Quick or manual, Standalone or Integrated form, and upgrade, rollback and uninstall. |
| [Agent lifecycle](docs/operations/agent-lifecycle.md) | Node installation, enrollment, activation, upgrade, rollback, rebind, and removal. |
| [Authentication](docs/operations/authentication.md) | Local, OIDC, and mixed sign-in, the first administrators, and identity and session operations. |
| [Incident recovery](docs/operations/incident-recovery.md) | Database and Signer backup and restore, Unknown command reconciliation, and credential recovery. |
| [Validation and release](docs/development/release-checks.md) | Checks for each kind of change, CI layers, three-VM acceptance, and the release process. |

The frozen wire formats for signed commands are specified in [command authorization v1](docs/development/command-authorization-v1.md), [command semantic hash v1](docs/development/command-semantic-hash-v1.md), and [command semantic hash v2](docs/development/command-semantic-hash-v2.md). HTTP endpoints and fields are defined in [`openapi/openapi.yaml`](openapi/openapi.yaml), and Protobuf messages in [`proto/`](proto/).

## Development

The repository pins its toolchain versions. To install the pinned toolchains and run the local verification suite:

```bash
make bootstrap
make verify
```

`make verify` is a broad local baseline; for most edits, pick a narrower check from [Validation and release](docs/development/release-checks.md#choose-the-validation-scope), which also covers host prerequisites and CI workflows. Running `make generate` overwrites all generated code, so do not edit `control-plane/gen/proto`, `rust/crates/contracts/src/generated`, or `web/src/api/generated` by hand.

## Releases and compatibility

Starting with [`v1.1.0`](https://github.com/GentleKingson/ocservia/releases/tag/v1.1.0), compatibility between different versions is no longer tested. A package install, upgrade, or rollback that succeeds does not guarantee compatibility with an existing database state. Read the [version policy](docs/operations/production-deployment.md#version-policy) before you change versions.

Pin production deployments to official [published releases](https://github.com/GentleKingson/ocservia/releases), and do not run development branches or release candidates in production. The [resilience and capacity harness](docs/development/release-checks.md#resilience-and-capacity) exercises load and fault injection against synthetic agents; you should still validate each target release on your own infrastructure.

## Security

Administrative commands require cryptographic authorization. Changes are covered by replay protection, a command ledger, and signed receipts. On the node, root execution is restricted to a fixed set of operations.

Accepting, dispatching, and executing a command are distinct states. When the Controller answers `202 Accepted`, it has atomically recorded the operation, its outbox entry, and the audit intent; that alone does not establish remote execution. A transport acknowledgement does not prove execution either. If a command's outcome is uncertain, its state remains Unknown until exact durable evidence establishes the result, and operators must pause conflicting changes while reconciliation is in progress. See the [recovery boundary](docs/reference/stable-contracts.md#matched-release-recovery-boundary) for details.

To report a suspected vulnerability, follow the private process in [SECURITY.md](SECURITY.md).

## Community and support

- Open a [GitHub Issue](https://github.com/GentleKingson/ocservia/issues) for bug reports and technical questions. Include reproduction steps, environment details, and relevant logs.
- Download release builds and archives from the [Releases page](https://github.com/GentleKingson/ocservia/releases).

<a id="license"></a>
## License and acknowledgements

ocservia is licensed under the [Apache License 2.0](LICENSE). Third-party software notices are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

ocservia is an independent project that interfaces with the [ocserv](https://ocserv.openconnect-vpn.net/) and OpenConnect ecosystem.
