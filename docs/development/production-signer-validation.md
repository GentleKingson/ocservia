# P2 validation record

Date: 2026-09-25. Starting work SHA:
`f1f44630058fe38ee1a052907879a099d1a54088`.
Planning reference: `e861b72130d4d409883f68d01805566f26cbafbc`.
Results cover the uncommitted P2 working changes, not a published commit or CI.

## Review and implementation

The P0 contract adds two obligations beyond the attachment's abbreviated
steps: signed CRL export and an authenticated Controller enrollment export.
Both are implemented. SQLite was a suggestion, not a compatibility boundary;
the implementation uses pinned bbolt 1.4.3 for a single-writer local ledger.
No HSM/KMS, HA, new public registration endpoint, command-signing migration,
production Secret access or deployment was introduced.

Changed surfaces:

- `signer/`: independent service, restrictive CSR policy, durable sign/revoke,
  controlled import/disable, two-purpose sealing, CRL, health, backup/restore.
- `deploy/production/signer.Dockerfile`: non-root scratch runtime.
- Controller certificate client/config/assembly: dedicated CA trust input.
- Controller enrollment service and `cmd/ocserv-sealing-export`: approved
  binding export through existing stores; export binary added to its Dockerfile.
- `scripts/export-node-sealing-keys.py`: root-protected node public export.
- Focused Go and existing Rust adapter tests; workspace module registration;
  [runtime and handoff documentation](production-signer.md).

## Executed checks

All commands ran through `ssh BuildServer`, in task-owned storage and containers
on Linux ARM64. Go 1.26.6 and Rust 1.97.1 matched the repository locks. All keys,
tokens, database accounts and node identities were disposable fixtures.

| Result | Command or operation | Evidence boundary |
| --- | --- | --- |
| PASS | Signer `go test -race -count=1 ./...` and `go vet ./...` | Concurrent exact replay, CSR conflict/policy, revoke binding/persistence, four subprocess crash boundaries, corruption, missing ledger, wrong issuer, snapshot recovery floor, import negatives, disabled bindings, both purposes, CRL signature and advancing number |
| PASS | Workspace `go test -race -count=1 ./signer` | Workspace dependency resolution also tested |
| PASS | Controller `go test -race -count=1 -skip Integration ./internal/certificates ./internal/platform/config ./internal/platform/app ./internal/enrollment ./cmd/ocserv-sealing-export` | Dedicated trust, SAN rejection, default trust isolation, existing redirect tests, config/assembly and enrollment units; explicit integration checks below |
| PASS | `go test -race -count=1 ./internal/enrollment -run '^TestEnrollmentBackendIntegration$' -v` | Real PostgreSQL 17.10, migrated using owner, test through restricted runtime account; approved export, pending/revoked rejection, wrong workspace/node/endpoint/approval |
| PASS | `cargo test --locked -p ocservia-ocserv-adapter certificate_p12_is_encrypted_bounded_and_replayable` | Go test helper enabled; node script public export, actual adapter/OpenSSL decryption for both purposes, encrypted P12 and wrong-purpose cases |
| PASS | `cargo fmt --all --check` and `cargo clippy --locked -p ocservia-ocserv-adapter --tests -- -D warnings` | No Rust production behavior changed |
| PASS | `docker build -f deploy/production/signer.Dockerfile ...` | ARM64 local image; final runtime image config SHA-256 `a7d8ac2618a0645b2bf2c014e1a8034b673c36b883f3e9e07c3981abffbd0dc0` |
| PASS | Image `init`, `import`, `serve`, verified HTTPS `health` | UID/GID 65532, read-only root, capabilities dropped, no-new-privileges; only loopback test publication |
| PASS | `go test -race -count=1 ./internal/certificates -run '^TestProductionSignerIntegration$' -v` | Existing HTTPSigner against the actual image: sign, Controller chain validation, duplicate revoke, historical replay, wrong token, both seal purposes |
| PASS | Image `backup`, `inspect`, `restore --minimum-revision 3`, then `serve`/HTTPS health on the restored ledger | Snapshot state version 1/revision 3; runtime permission and restart checks, not just file existence |
| PASS | `git diff --cached --check` and `bash scripts/docs-check.sh` | Task-isolated candidate index; user's index unchanged |

The first TLS negative test reused an existing connection after changing the
test transport's server name; closing its idle connection fixed the test.
Initial copied-tree ownership and missing shared fixture data caused unrelated
unit failures. Initial macOS AppleDouble archive entries also caused migration
filename rejection. Only task-owned copy metadata/artifacts were corrected;
security assertions and migration validation were not relaxed. The initial
Rust formatting check failed on newly inserted test code; rustfmt corrected it.
The affected checks were rerun successfully.

## Remaining acceptance boundaries

- NOT_RUN: full Controller/agent/privd daemon business E2E and Registry-pulled
  production-path acceptance. The Rust check runs the real decryption adapter,
  not a deployed privd RPC process. Do not relabel it as full node E2E.
- NOT_RUN: deployed CRL distribution/refresh/enforcement, VPN session effects,
  AMD64 image smoke, and MySQL/MariaDB export integration in this task.
- Controller export JSON relies on a protected operator channel and file
  provenance, not a portable cryptographic export signature.
- Disabling retired node bindings remains an explicit offline operation.
  Backups cannot prove freshness without an independently reconciled revision.

These boundaries are P3/P4/P5/P6 handoffs in the runtime document, not claims of
production readiness. No commit, push, merge, Registry upload or production
deployment was performed. Task-owned test containers, images, private fixture
keys and directories are removed after retaining this record; shared caches and
unrelated tasks are not cleaned.

## Actions acceptance follow-up

The operator subsequently authorized disposable GitHub-hosted runners and
candidate GHCR publication. Run
[36149926748](https://github.com/GentleKingson/ocservia/actions/runs/36149926748)
tested candidate `5a3c4033e7d5bbf57457a8c6d40d1e927d9c83a2` with
`purpose=integration`, `production_signer=true`, candidate version `1.0.2`.
Six candidate images were published with unique run-bound tags and pulled by
digest. The real daemon certificate CSR/issue/P12/one-use/revoke/restart checks
passed, with matching Controller, Agent journal and privd root results.
Signed CRL operator distribution and SIGHUP refresh rejected the revoked
certificate while an unrevoked control certificate still authenticated;
CRL number advanced from 3 to 6.

The overall run FAILED in the later browser configuration apply check and is
not full business acceptance. The additional CRL ocserv had used the default
occtl socket path; ocserv 1.2.4 removes that path on shutdown even when its
control socket is disabled. This interfered with the primary node's health
check. The harness now assigns separate occtl/PID paths and asserts the
primary socket identity and actual occtl query survive CRL-node shutdown.
All gates must be rerun on the corrected candidate; earlier partial results
must not be inherited as its acceptance. Final run evidence belongs to the
exact tested SHA and the acceptance PR, which is created only after success.
