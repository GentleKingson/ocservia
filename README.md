# ocservia

[![CI](https://github.com/GentleKingson/ocservia/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/GentleKingson/ocservia/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/GentleKingson/ocservia)](https://github.com/GentleKingson/ocservia/releases/latest)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](#license-and-acknowledgements)
[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8.svg?logo=go)](control-plane/go.mod)
[![Rust](https://img.shields.io/badge/Rust-2024%20edition-black.svg?logo=rust)](rust/Cargo.toml)
[![Vue](https://img.shields.io/badge/Frontend-Vue%203%20%2B%20TS-4FC08D.svg?logo=vuedotjs)](web/package.json)

ocservia is a central control plane for [ocserv](https://ocserv.openconnect-vpn.net/) (OpenConnect VPN) servers. From a single Web console or HTTP API, operators monitor node health, manage user credentials and traffic quotas, inspect active sessions, and distribute signed configuration updates without opening a shell on each host.

ocservia runs alongside ocserv rather than replacing it. Each managed server keeps running ocserv to terminate client tunnels and forward traffic, while ocservia's node daemons connect back to a central Controller.

[Operating model](#operating-model) · [Deploy the Controller](#1-deploy-the-controller) · [Install a managed node](#2-install-a-managed-node) · [Documentation](#documentation) · [Development](#development)

## Features

- Monitor node connection states, daemon versions, active sessions, IP bans, and telemetry streams.
- Manage user credentials centrally with group policies, traffic quotas, and account expiration.
- Terminate sessions and revoke IP bans via signed administrative commands, each recorded in an audit log.
- Validate configuration changes before applying them. Failed deployments roll back automatically; if a rollback fails, the node stops automated changes for manual recovery.
- Enroll and activate nodes with single-use tokens, signed endpoint proofs, and automatic privd key registration.
- Install, upgrade, roll back, and remove managed nodes using pinned releases.

<a id="-what-makes-ocservia-different"></a>
## Operating model

### VPN traffic stays on ocserv

Client VPN tunnels terminate on the ocserv nodes. The Controller is never in the packet path. If the Controller goes down, management operations pause while established VPN sessions stay connected.

### Privileged operations are bounded

On each managed host, the Agent runs as an unprivileged process. For root actions, it calls `privd` over an authenticated Unix domain socket. `privd` exposes only a fixed set of ocserv maintenance routines, so the Agent and Controller cannot run arbitrary binaries or shell commands.

### One Admin can complete management operations

One authenticated administrator manages nodes, users, configuration, certificates, and upgrades without a second identity or approval step. The Controller checks workspace permissions and binds signed commands to the requested payload. Short-lived single-use installation tokens automate node activation and root-key registration. TLS verification, key isolation, audit records, and recovery checks still apply.

### 1. Deploy the Controller

Download the installer from an exact GitHub Release (replace `vX.Y.Z`):

```bash
mkdir ocservia-install && cd ocservia-install
wget https://github.com/GentleKingson/ocservia/releases/download/vX.Y.Z/controller-bootstrap.sh -O install.sh
bash install.sh
```

The wizard detects the platform, collects Quick or Manual settings, checks prerequisites, and asks for confirmation. It requires a TTY. For automation, run `bash install.sh --non-interactive --config ./install.env`; missing required settings fail without prompting. Release scripts embed their version, so production installation does not require Git or a source checkout.

Operators authenticate with local credentials (`OCSERV_LOCAL_AUTH_ENABLED=true`). Fresh installations have no default administrator account. In a manual setup, create the single local administrator with the one-shot bootstrap command in [Production authentication](docs/operations/authentication.md#bootstrap-the-first-local-admin).

Standalone mode (the default) runs the Relay and Signer on separate hosts. Integrated mode runs them on the Controller host under the root lifecycle and needs additional configuration. Provision the required secrets before running bootstrap as described in [Production deployment](docs/operations/production-deployment.md#prepare-the-configuration).

For a new Integrated host, the Quick preset generates the configuration, cryptographic material, and initial local administrator on stable or RC releases after v1.2.0 (see [Quick mode](docs/operations/production-deployment.md#quick-mode)).

### 2. Install a managed node

In the Controller's **Nodes → Install node**, generate a short-lived installation token. On the ocserv host, download the matching release and enter the token at the hidden prompt:

```bash
mkdir ocservia-node-install && cd ocservia-node-install
wget --https-only --secure-protocol=TLSv1_2 https://github.com/GentleKingson/ocservia/releases/download/vX.Y.Z/managed-node-bootstrap.sh -O install.sh
bash install.sh --join-token
```

The installer fetches configuration over verified HTTPS, installs the package, registers and activates the Agent, registers its root-owned privd key, and starts the services. It reports `ONLINE` only after the Controller observes active trust and fresh telemetry. No second identity or confirmation is needed, and the host must trust the Controller's HTTPS certificate. See [Agent lifecycle](docs/operations/agent-lifecycle.md#one-token-installation) for prerequisites and recovery from interrupted runs.

<a id="-documentation"></a>
## Documentation

| Document | What it covers |
| :--- | :--- |
| [Architecture](docs/architecture.md) | Components, call chains, responsibility owners, and permission and state boundaries. |
| [Stable contracts](docs/reference/stable-contracts.md) | Rules that changes must keep: command delivery and receipts, identity and authorization, database artifacts, and protocol generation. |
| [Production deployment](docs/operations/production-deployment.md) | Support scope, Controller and Relay installation in Quick or manual, Standalone or Integrated form, and upgrade, rollback and uninstall. |
| [Agent lifecycle](docs/operations/agent-lifecycle.md) | Node installation, enrollment, upgrade, rollback, and removal. |
| [Authentication](docs/operations/authentication.md) | Single local administrator authentication, bootstrap, and session operations. |
| [Incident recovery](docs/operations/incident-recovery.md) | Database and Signer backup and restore, unknown command reconciliation, and credential recovery. |
| [Validation and release](docs/development/release-checks.md) | Checks for each kind of change, CI layers, business acceptance, and the release process. |

The frozen wire formats for signed commands are specified in [command authorization v1](docs/development/command-authorization-v1.md) and [command semantic hash v2](docs/development/command-semantic-hash-v2.md). HTTP endpoints and fields are defined in [`openapi/openapi.yaml`](openapi/openapi.yaml), and Protobuf messages in [`proto/`](proto/).

## Development

The repository pins its toolchain versions. To install the pinned toolchains and run the local verification suite:

```bash
make bootstrap
make verify
```

`make verify` is a broad local baseline; for most edits, pick a narrower check from [Validation and release](docs/development/release-checks.md#choose-the-validation-scope), which also covers host prerequisites and CI workflows. Running `make generate` overwrites all generated code, so do not edit `control-plane/gen/proto`, `rust/crates/contracts/src/generated`, or `web/src/api/generated` by hand.

## Releases and compatibility

Starting with [`v1.1.0`](https://github.com/GentleKingson/ocservia/releases/tag/v1.1.0), compatibility between different versions is no longer tested. A package install, upgrade, or rollback that succeeds does not guarantee compatibility with an existing database state. Read the [version policy](docs/operations/production-deployment.md#version-policy) before you change versions.

Pin production deployments to official [published releases](https://github.com/GentleKingson/ocservia/releases), and do not run development branches or release candidates in production. Validate each target release on your own infrastructure following [Validation and release](docs/development/release-checks.md#choose-the-validation-scope).

## VPN audit history

Open **Audit → VPN audit history** to search the current workspace by node, username, and time. Session events show logins and logouts. Session snapshots show the last counters seen by occtl and the first snapshot where a previously observed session was absent. Network connections also filter by exact destination IP. Each page returns up to 200 matches; **Load older records** continues the query, and **Export results** downloads all loaded rows as JSON with byte counts preserved as decimal strings.

Applying the complete configuration enables session hooks. Externally managed configurations must call the packaged hooks. Network auditing is opt-in per node and records connection metadata. The Controller retains session events and closed snapshots for 90 days and network records for 30 days. [Node setup and verification](docs/operations/agent-lifecycle.md#vpn-audit-collection) · [Query and retention contract](docs/reference/stable-contracts.md#vpn-audit-history)

## Security

Administrative commands require cryptographic authorization, replay protection, a command ledger, and signed receipts. Root execution on each node is limited to a fixed set of operations.

Accepting, dispatching, and executing a command are distinct states. When the Controller returns `202 Accepted`, it has committed the operation, its outbox entry, and the audit intent in one database transaction; that does not mean the command ran on the node. A transport acknowledgement does not prove execution either. If a command's outcome is uncertain, its state remains unknown until durable evidence settles the result, and operators must pause conflicting changes during reconciliation. See the [recovery boundary](docs/reference/stable-contracts.md#matched-release-recovery-boundary).

To report a suspected vulnerability, follow the private process in [SECURITY.md](SECURITY.md).

## Community and support

- Open a [GitHub Issue](https://github.com/GentleKingson/ocservia/issues) for bug reports and technical questions. Include reproduction steps, environment details, and relevant logs.
- Download release builds and archives from the [Releases page](https://github.com/GentleKingson/ocservia/releases).

<a id="license"></a>
## License and acknowledgements

ocservia is licensed under the [Apache License 2.0](LICENSE). Third-party software notices are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

ocservia is an independent project that interfaces with the [ocserv](https://ocserv.openconnect-vpn.net/) and OpenConnect ecosystem.
