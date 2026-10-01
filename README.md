# ocservia

[![CI](https://github.com/GentleKingson/ocservia/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/GentleKingson/ocservia/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/GentleKingson/ocservia)](https://github.com/GentleKingson/ocservia/releases/latest)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](#license)
[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8.svg?logo=go)](control-plane/go.mod)
[![Rust](https://img.shields.io/badge/Rust-2024%20edition-black.svg?logo=rust)](rust/Cargo.toml)
[![Vue](https://img.shields.io/badge/Frontend-Vue%203%20%2B%20TS-4FC08D.svg?logo=vuedotjs)](web/package.json)

**A calm, security-minded operations plane for your [ocserv](https://ocserv.openconnect-vpn.net/) / OpenConnect VPN fleets.**

Managing a fleet of OpenConnect servers shouldn't mean keeping a dozen SSH terminals open, juggling config drifts by hand, or holding your breath during a config reload. **ocservia** gives operators a unified, beautifully crafted control plane to observe fleet health, manage users and quotas, review active sessions, and execute signed configuration changes — **without touching or proxying a single byte of your VPN traffic.**

[Quick Playground](#-try-it-locally-in-2-minutes) · [Engineering Principles](#-what-makes-ocservia-different) · [Architecture](#-architecture-at-a-glance) · [Production Deployment](#-deploying-to-production) · [Documentation](#-documentation)

---

## ✨ What ocservia does for you

- 🌐 **Unified Fleet Cockpit** — Observe all your ocserv nodes in real time. Track connection states, daemon versions, active sessions, ban lists, and live telemetry in one central dashboard.
- 👥 **Centralized User & Quota Orchestration** — Define users, assign group policies, adjust bandwidth quotas, and schedule expiration dates centrally from the Controller instead of editing files on every host.
- ⚡ **Surgical Session & Ban Actions** — Disconnect stuck sessions, terminate abusive connections, or unban client IP addresses with one click. Every action is cryptographically signed and logged with full audit trails.
- 🛡️ **Config Staging with Rollback Guarantees** — Render and validate configuration diffs before they ever touch production. Risky fleet-wide rollouts support a built-in "two-man rule" approval workflow so four eyes can review before execution.
- 🤝 **Deliberate Node Enrollment** — Managed nodes introduce themselves with signed tokens and wait in pending state until an operator explicitly approves them before receiving tasks.
- 🔄 **Predictable Lifecycle Management** — Install, upgrade, roll back, or decommission Controller and node agents with pinned releases, verified cryptographic checksums, and zero unexpected side-effects.

---

## 🛡️ What makes ocservia different

We built ocservia because we operate infrastructure ourselves and know the anxieties of production maintenance. We designed it around three core principles:

### 1. A Sidecar, Not a Tollgate (Zero Traffic Intrusion)
ocservia runs **alongside** ocserv, never in the middle of your VPN packets. Your users connect directly to ocserv with full native wire speed. If the Controller stops, restarts, or loses its database connection, **your active VPN tunnels do not drop a single connection**.

### 2. Surgical Scalpels, Never a Backdoor (No Arbitrary Remote Shells)
Many management panels take shortcuts by running arbitrary `sh -c` strings as root over the network. **ocservia strictly rejects this.** Our Rust node agent splits execution into an unprivileged communication process and a sandboxed privileged daemon (`privd`). Privileged actions are restricted to a pre-compiled set of fixed ocserv operations, validated over Unix domain sockets with local attestation.

### 3. The "Two-Man Rule" for Risky Changes
To prevent accidental fat-finger disasters at 3 AM, high-risk operations (such as major configuration updates or batch commands) can require approval from an independent authorized operator before the Controller signs and dispatches the task.

---

## 📐 Architecture at a glance

```text
  Operator Browser (Vue 3 / TypeScript)
                 │  HTTPS / WSS
                 ▼
     ┌───────────────────────┐
     │  Controller (Go)      │ ───► Supported DB (PostgreSQL 17 / MySQL / MariaDB)
     └───────────────────────┘
                 │  mTLS / Pinned Relays
                 ▼
     ┌───────────────────────┐
     │   Dedicated Relays    │ ◄─── (Isolates nodes from public ingress)
     └───────────────────────┘
                 │
                 ▼
┌──────────────────────────────────────────────────────────┐
│  Managed Node (Each VPN Server)                          │
│                                                          │
│   ┌──────────────────┐         Local UDS /               │
│   │ Rust Agent       │ ── Attested Protocol ──┐          │
│   └──────────────────┘                        ▼          │
│                                      ┌─────────────────┐ │
│                                      │ Rust privd      │ │
│                                      └─────────────────┘ │
│                                               │          │
│                                        Fixed occtl calls │
│                                               ▼          │
│                                      ┌─────────────────┐ │
│                                      │ ocserv Daemon   │ │
│                                      └─────────────────┘ │
└──────────────────────────────────────────────────────────┘
```

- **Controller** — Modular Go backend managing the Web UI, API, scheduling, state machines, and transactional audit journals.
- **Relays** — Low-footprint proxies that allow nodes behind NAT/firewalls to maintain secure, outbound-only control channels.
- **Node Agent & privd** — Ultra-lightweight Rust services maintaining local health heartbeats and executing signed operational tasks with least-privilege isolation.
- **Database** — Reliable transactional storage supporting PostgreSQL (default), MySQL 8.4, or MariaDB 12.3 with verified backups and isolated restore procedures.

*Read the complete [Architecture & Trust Model](docs/architecture.md) for details.*

---

## ☕ Try it locally in 2 minutes

Want to explore the web console and test fleet workflows without touching real servers? The local playground spins up the Controller, the Web UI, and simulated nodes with realistic telemetry:

```bash
git clone https://github.com/GentleKingson/ocservia.git
cd ocservia

# Launch local stack with simulated nodes
deploy/compose/compose.sh up --build -d
```

- **Web Console**: Open [http://127.0.0.1:4173](http://127.0.0.1:4173) in your browser.
- **Controller Health**: Verify readiness at `http://127.0.0.1:8080/readyz` and `/version`.

*To tear down cleanly and wipe temporary volumes, run `deploy/compose/compose.sh down --volumes`.*  
*See [Try ocservia locally](docs/getting-started/local-development.md) for details.*

---

## 🚀 Deploying to production

### 1. Deploy the Controller

Keep your production settings in a dedicated directory outside the source checkout:

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

Configure authentication as **Local only**, **OIDC only**, or **Local + OIDC**. First-time Local setups initialize credentials through the one-shot protected token workflow described in [Production authentication](docs/operations/authentication.md). Full instructions are in [Deploy the Controller](docs/getting-started/production.md).

### 2. Install a Managed Node

On each ocserv server you wish to manage:

```bash
git clone --branch vX.Y.Z --single-branch --depth 1 \
  https://github.com/GentleKingson/ocservia.git ocservia-vX.Y.Z

mkdir ocservia-node-install && cd ocservia-node-install
cp ../ocservia-vX.Y.Z/install.env.example install.env
$EDITOR install.env

../ocservia-vX.Y.Z/deploy/managed-node/install.sh
```

Once installed, the node enters a `pending` approval state. Review and approve the new node in the Controller Web console, then start the node services. See [Install a managed node](docs/getting-started/managed-node.md) and [Enroll a node](docs/how-to/enroll-node.md).

---

## 📚 Documentation

| Topic | Description |
| :--- | :--- |
| **[Getting Started](docs/README.md)** | Start here: local setup, production deployment, and first enrollment. |
| **[Architecture & Trust](docs/architecture.md)** | System topology, trust boundaries, and least-privilege security model. |
| **[Operations & Runbooks](docs/operations/production-deployment.md)** | Single Relay recovery and backup/restore (Postgres & MySQL). |
| **[Troubleshooting Guide](docs/how-to/troubleshooting.md)** | Diagnostic checklists for startup, OIDC, telemetry, and agent enrollment. |
| **[Support & Versioning Policy](docs/reference/support-policy.md)** | Current support contracts, platform matrix, and cross-version policies. |
| **[Technical Reference](docs/reference/README.md)** | Detailed API schemas, gRPC protocols, and internal specifications. |

---

## 🛠️ Developing & Contributing

We pin all toolchains to guarantee deterministic builds across environments:

```bash
make bootstrap
make verify
```

GitHub Actions enforces strict merge validation. See [Contributor validation](docs/development/testing.md) for testing guidelines and build workflows.

---

## 📌 Release & Stability Notice

> [!NOTE]
> [`v1.1.0`](https://github.com/GentleKingson/ocservia/releases/tag/v1.1.0) is published. Starting with `v1.1.0`, software-version compatibility admission and the previous cross-version support promise are removed. This is a breaking policy change despite the minor version number; an accepted target is not a guarantee of cross-version safety. Follow the [current support and versioning policy](docs/reference/support-policy.md).
>
> Always pin an exact [published release](https://github.com/GentleKingson/ocservia/releases). The default branch and unreleased candidates are not published production releases. The P1 test harness provides single-host resilience validation outside basic CI; see [P1 resilience and capacity](docs/development/p1-resilience-capacity.md).

---

## 🔒 Security

ocservia is designed from the ground up for zero-trust environments. Privileged operations are strictly constrained to fixed ocserv actions, commands require cryptographic authorization, and high-risk changes require auditable multi-operator sign-off.

If you discover a security vulnerability, please report it responsibly by following our instructions in [SECURITY.md](SECURITY.md). **Do not open public GitHub issues for security reports.**

---

## 💬 Community & Support

- **Bug Reports & Discussions**: Open a [GitHub Issue](https://github.com/GentleKingson/ocservia/issues) with reproduction steps, environment details, and sanitized logs.
- **Releases**: Track upcoming candidates and verified assets on our [Releases page](https://github.com/GentleKingson/ocservia/releases).

---

## 📄 License & Acknowledgements

ocservia is open-source software licensed under the [Apache License 2.0](LICENSE). See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) for third-party credits and notices.

*ocservia is an independent project proudly building upon the work of the [ocserv](https://ocserv.openconnect-vpn.net/) and OpenConnect open-source communities.*

