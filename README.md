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

## Status

Repository and project governance are currently being established. Source
code, deployment guidance, and compatibility commitments will be added as
implementation milestones are completed and verified. Contract contributors
can use the pinned workflow in
[docs/development/contracts.md](docs/development/contracts.md).

## Security

Please report suspected vulnerabilities privately as described in
[SECURITY.md](SECURITY.md). Do not disclose security issues in public GitHub
issues.

## License

Unless otherwise noted, ocservia is available under either the
[Apache License 2.0](LICENSE-APACHE) or the [MIT License](LICENSE-MIT), at your
option.
