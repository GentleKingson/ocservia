# ocservia

ocservia is an early-stage management platform for operating multiple
[ocserv](https://www.infradead.org/ocserv/) servers from a central control
plane. The project is being built in public, but it is not ready for production
use and does not yet provide a functional release.

The planned implementation combines a Go control plane, Rust transport and
node components using Iroh, a Vue and TypeScript web application, and
PostgreSQL. The platform is intended to expose narrowly scoped management
operations for ocserv. It will not provide a general-purpose remote shell,
generic file management, or arbitrary host command execution.

The real Iroh 1.0.x transport endpoint and its development and rollback modes
are documented in [docs/development/transportd.md](docs/development/transportd.md).
The one-time enrollment, approval, EndpointID pinning, revocation, and rollback
workflow is documented in
[docs/development/enrollment.md](docs/development/enrollment.md).
The unprivileged Agent, fixed read-only privd boundary, and systemd lifecycle
are documented in
[docs/development/agent-privd.md](docs/development/agent-privd.md).

## Status

The public contracts and local side-effect-free control-plane slice are under
active development. Contract contributors can use the pinned workflow in
[docs/development/contracts.md](docs/development/contracts.md). The local Go,
PostgreSQL, UDS transport stub, agent simulator, OpenTelemetry, and Web stack is documented in
[docs/development/control-plane.md](docs/development/control-plane.md).
Telemetry freshness, retention, read-only node APIs, and fleet views are
documented in [docs/development/telemetry.md](docs/development/telemetry.md).
The repeatable single-host P1 recovery and initial capacity harness is documented
in [docs/development/p1-resilience-capacity.md](docs/development/p1-resilience-capacity.md).

## Security

Please report suspected vulnerabilities privately as described in
[SECURITY.md](SECURITY.md). Do not disclose security issues in public GitHub
issues.

## License

Unless otherwise noted, ocservia is available under either the
[Apache License 2.0](LICENSE-APACHE) or the [MIT License](LICENSE-MIT), at your
option.
