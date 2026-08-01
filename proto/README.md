# Protocol contracts

The schemas in this directory are the source of truth for Go-to-Rust IPC and
the agent protocol. Run `make generate`; do not edit files below the generated
Go or Rust directories.

Compatibility follows Buf's `FILE` policy. Published field numbers and names
must be reserved when removed, field numbers must never be reused, and every
enum starts with an `UNSPECIFIED` zero value.
