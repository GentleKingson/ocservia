# Validation and release

Pick the smallest validation target that covers what you changed. Direct `go`, `cargo`, or `npm` commands require `source scripts/env.sh` from the repository root first, and Go checks require a private `0700` `TMPDIR` (handled automatically by `scripts/go-check.sh`).

## Choose the validation scope

| Scope | Primary entrypoint | Notes |
| --- | --- | --- |
| Fast workspace tests | `make test` | Runs unit tests across Go (`control-plane`, `signer`), Rust (`--lib --bins`), and Web (`npm test`). |
| Go (`control-plane`, `signer`) | `scripts/go-check.sh standard` | Runs `gofmt`, `go vet`, and `go test -count=1`; use `full` for `-race`. |
| Rust | `make rust-check` | Runs `cargo fmt --check`, `cargo clippy -- -D warnings`, and `cargo test --workspace --all-features --lib --bins`. |
| Web | `scripts/web-check.sh basic` | Runs Prettier, ESLint, `vue-tsc`, Vitest, and Vite build; `full` adds desktop Playwright E2E. |
| PostgreSQL 18 smoke | `scripts/database-postgres-smoke.sh` | Starts one disposable PostgreSQL 18 container and runs `go test -tags=integration` core smoke checks. |
| Docs, scripts & hygiene | `make docs-check`, `scripts/lint.sh source-style`, `scripts/test-hygiene-check.sh` | Validates Markdown links, ShellCheck/Ruff, and test-to-prod line ratio/file limits. |
| Full local baseline | `make verify` | Runs lint, breaking checks, Go, Rust, Web, boundary checks, Gitleaks, licenses, generated cleanliness, and test hygiene. |

Go PostgreSQL integration tests carry `//go:build integration` so routine `go test ./...` runs stay fast and pure in-memory.

### Command wire contracts

Go's `commandauth.TestStrictWireCommandFixtures` and Rust's `contracts::strict_wire` suite share `testdata/command-strict-wire.json` to keep wire allowlists and descriptors aligned across languages:

```bash
source scripts/env.sh
(cd control-plane && go test -count=1 ./internal/commandauth ./internal/semanticpayload ./internal/privdattestation)
(cd rust && cargo test --locked --lib -p ocservia-contracts -p ocservia-command-authorization -p ocservia-privd-attestation)
```

When intentionally updating wire fixtures, run the Go test with `UPDATE_STRICT_WIRE_FIXTURES=1`, rerun both suites without it, and inspect the diff.

### Basic CI

[`.github/workflows/ci.yml`](../../.github/workflows/ci.yml) runs `scripts/ci-relevance.sh` to select only the affected jobs (`docs`, `ci-tools`, `go`, `rust`, `web`, `database-smoke`) and aggregates them into the required `Basic CI Result` check. Automatic triggers run `quick` (`go-check.sh standard`); manual `workflow_dispatch` accepts `quick` or `full`.

## Release policy

- **Qualification (`release-check.yml`)**: Runs on `main` for a target version (`X.Y.Z` or `X.Y.Z-rc.N`). It resolves same-commit Basic CI (`Basic CI Result`) and Security (`security.yml`) evidence, verifies contracts and PostgreSQL 18 smoke if needed, builds native `amd64`/`arm64` Agent packages and Controller images (with image smoke via `scripts/release-controller-image-smoke.sh` and asset verification via `scripts/validate-release-packages.sh`), and writes the `release-check.json` receipt.
- **Publication (`release.yml`)**: Triggered on signed/approved `v*.*.*` tag pushes after `Release Check` passes, publishing immutable GitHub Release assets and container images.
