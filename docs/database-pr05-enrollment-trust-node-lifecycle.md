# PR-05 Draft: Enrollment, Trust and Node Lifecycle

Base: PR-01 through PR-04 are merged, including PR-04 `58a0734` (#195).
The plan was reviewed against the existing adapters before implementation.
PR-02 already moved these workflows behind backend-owned stores. PR-05 reuses
them rather than adding another enrollment or attestation implementation.
MySQL/MariaDB remain test/development-only. No Agent/privd SQLite, wire protocol,
signature format, production admission or runtime grants change.

## Schema and Adapter Checklist

| Surface | Final storage contract reviewed | PR-05 action |
| --- | --- | --- |
| Nodes | Workspace/node composite identity, status, authorization revision, version and logical clocks; PostgreSQL JSONB labels and checked MySQL/MariaDB JSONB representation | Reuse enrollment store and v23 shared storage |
| Enrollment and bootstrap tokens | SHA-256 only, unique 32-byte digest, expiry ordering, nullable consumption pairs, composite workspace/node FK; bootstrap endpoint bound on first consumption | Recheck expiry immediately before consumption; preserve pending-node replay |
| Endpoint keys | Unique 32-byte EndpointID, node PK, state/revocation-time pair | Preserve global uniqueness, exact endpoint proof and revoked denial |
| Sealing keys | Node/purpose PK; node/key-ID and node/digest uniqueness; version/purpose checks; 32-byte digest | Preserve insert-only key binding and byte round trips |
| Capabilities | Node/capability PK, explicit approved flag, exact capability strings | Reuse enrollment/attestation stores and shared negotiation checks |
| Trust convergence | One row per node, positive revision, nonnegative retained attempt count, paired owner/deadline, independent update/close flags | Fence claim mutations with node/revision/worker/attempt and a live lease |
| Attestation credentials | UUIDv7 credential, node and creator references, unique 32-byte secret digest, nonce/context digests, expiry and consumption clocks | Check expiry after both credential and node locks |
| Attestation keys | Node/key PK, globally unique key ID/public key, unique registration credential, validity/revocation checks, predecessor/successor IDs | Preserve signed registration, bounded rotation, rollback and revocation |
| Sealed payload and signatures | Existing binary command envelopes and protobuf bytes; EndpointID and attestation proof are validated in shared services | No encoding or protocol changes |

PostgreSQL uses UUID/bytea/timestamptz. MySQL/MariaDB retain length-checked
VARBINARY UUIDs, VARBINARY/BLOB identity bytes and signed PostgreSQL-epoch
microsecond clocks. Attestation v6, trust v18, token v19 and shared-node v23
conversions remain unchanged. Capability/key text uses exact no-pad binary
collations (`utf8mb4_0900_bin` / `utf8mb4_nopad_bin`), not case-folded identity
comparisons. Binary keys and hashes are never cast to collated text.
No migration, historical manifest or checksum needs rewriting.

API token/node handlers, trustserver and transportclient already call the shared
services or typed stores. Their SQL boundaries and protocol checks are retained.
Sealing keys still have only SELECT/INSERT runtime grants. Attestation keys and
credentials retain their existing SELECT/INSERT/UPDATE grants, without DELETE,
DDL or new trust-on-first-use paths.

## Behavioral Changes

Trust claims use fresh database wall time, with the deadline measured after the
claim lock. Renewal, completion and retry lock the exact claim before checking
wall time, so neither transaction-stable clocks nor lock waits can revive an
expired lease. The retained attempt counter fences late calls even when the
same worker reclaims the same revision. Higher revisions invalidate ownership;
equal/older revisions cannot reset newer intent.

The worker renews the ten-second lease before each external mutation and every
third of the lease while transport is in flight. Lost renewal cancels transport.
Renewal finishes before completion bookkeeping; late results cannot overwrite
superseding intent. A failed close retries without replaying an acknowledged
trust update. Existing connection-owner fencing and stable operation IDs remain.
This does not claim exactly-once network delivery or the ability to retract a
request already accepted by transport.

Enrollment rechecks token expiry after admission/node/key waits and immediately
before consumption. Failure rolls back the node, endpoint, sealing keys and
capabilities. Bootstrap preflight and replay both require the original workspace,
endpoint and pending node. Already consumed bootstrap tokens can still replay
for that exact pending binding after expiry; they cannot enroll another node.
Attestation samples its validity clock only after credential and active-node
locks. Credential, predecessor/successor, capability, revision and audit writes
remain in their original transaction; no transaction retry wraps transport.

## Acceptance Coverage

The required `backend-enrollment` group includes the existing shared enrollment
and full-range storage workflows plus focused regression cases for expired
claims, same-worker reclaim, expiry across a lock wait, superseding revisions,
in-flight renewal/cancellation, token rollback, competing bootstrap endpoints,
cross-workspace endpoint conflicts, concurrent attestation consumption, duplicate
global keys, rotation rollback, node revocation and credential lock-wait expiry.

`test-enrollment-restart.sh` coordinates a real restart of its caller-owned
disposable database with a committed test fixture. It verifies retained token
consumption, node/workspace binding, revoked state, sealing keys, claim metadata
and close-only recovery by a new worker. It is required by both full database
entry points. An explicitly bound ephemeral loopback port remains stable across
the restart (selected with Python 3's standard socket library). Ordinary package
tests do not restart shared databases.

## Validation

Validation ran only through `ssh BuildServer`, in the isolated checkout
`/root/ocservia-pr05.exWPhK`, using `golang:1.26.6-bookworm`.

| Check | Result |
| --- | --- |
| Controller compile-only `go test -run '^$' ./...`, scoped `go vet`, Controller build | Passed |
| PostgreSQL 17 and 18 shared lifecycle acceptance, with `-race` | 17/17 required entries on each version; none skipped |
| MySQL 8.4.10 and MariaDB 12.3.2 shared lifecycle acceptance, with `-race` | 17/17 required entries on each engine; none skipped |
| Real database restart and close-only recovery, all four server variants, with `-race` | 1/1 required entry on each; none skipped |
| Existing PostgreSQL enrollment and privdattestation suites | Passed on 17 and 18 |
| Existing MySQL/MariaDB trust convergence, privd attestation and runtime privilege tests | All three selected tests passed on each engine |
| Trustserver, transportclient, command signatures, protocol contracts and database boundary tests, with `-race` | Passed |
| Five targeted API enrollment/authentication/TTL/error tests | Passed |
| Shell syntax, required-test guard fixtures and full all/current/history routing | Passed |
| Both database entry points and restart helper, ShellCheck | Passed |
| All 68 PostgreSQL historical migration checksums | Passed; no migrations changed |

Evidence includes `{pg17,pg18,mysql,mariadb}-enrollment.json`, the corresponding
restart logs/JSON, MySQL/MariaDB native logs, `static.log`, and
`protocol-final.log`. Socket regressions initially rejected the copied checkout's
untrusted UID; only the disposable checkout ownership was corrected, then the
unchanged socket and transport suites passed. No ancestry check was weakened.

ShellCheck reports two pre-existing warnings in `test-required-go-tests.sh`
(SC2043 at line 34 and SC2034 at line 154), confirmed against the base commit.
They remain unchanged; that script's executable routing/signal/selection tests
passed. The full historical rollback matrix and multi-process Agent/transport
fault harness were not rerun for this scoped change. Test containers and
unneeded runtime fixtures were removed; validation logs are retained.

This PR is Draft; do not merge, deploy or release it.
