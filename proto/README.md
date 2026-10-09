# Protocol contracts

The schemas in this directory are the source of truth for the Go/transportd
gRPC services (`TransportService`, `TrustService`) and the Agent protocol
messages they carry. `make generate` (`scripts/generate.sh`, `buf.gen.yaml`)
replaces the generated Go code in `control-plane/gen/proto` and the Rust
bindings in `rust/crates/contracts/src/generated`; do not edit those
directories. The Agent/privd local protocol is hand-written in
`rust/crates/agent-protocol`, not defined here.

Buf lints with `STANDARD` (one documented exception in `buf.yaml`) and checks
compatibility with the `FILE` policy. Removed published field numbers and names
must be reserved, field numbers must never be reused, and every enum starts
with an `UNSPECIFIED` zero value.
