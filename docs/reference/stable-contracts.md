# Stable contracts

This document lists the rules that must not break: invariants, protocol generation rules, receipts, and the database artifact and recovery contracts. Operator steps live in [Production deployment](../operations/production-deployment.md), [Agent lifecycle](../operations/agent-lifecycle.md), [Authentication](../operations/authentication.md) and [Incident recovery](../operations/incident-recovery.md). The frozen byte formats are [command authorization v1](../development/command-authorization-v1.md), [semantic hash v1](../development/command-semantic-hash-v1.md) and [semantic hash v2](../development/command-semantic-hash-v2.md).

The published [v1.1.0 policy reset](../operations/production-deployment.md#version-policy) replaced the software-version windows, historical release matrix, and schema-compatibility admission rules from the 1.0 policy, removing the software-version guards and legacy compatibility entrypoints. The [release notes](https://github.com/GentleKingson/ocservia/releases/tag/v1.1.0) record acceptance for `v1.1.0` itself, but every subsequent candidate must pass its own validation. Runtime protocol negotiation, command authorization, artifact digest checks, durable state invariants, migration content integrity, and live readiness probes remain active contracts without acting as a software-version fence.

Each candidate build is validated against its exact artifacts on the supported architectures, deployment modes, and database engines. Matching version strings, unchanged protocol numbers, or passing unit tests do not guarantee cross-version runtime compatibility. Historical matrices, exclusions, and test records from the 1.0 line remain archived in the [published 1.0 inventory](https://github.com/GentleKingson/ocservia/blob/d420b22018596d6741d55fa56bd19c4a767e5817/docs/reference/stable-contracts.md). The entrypoints removed after 1.0 are no longer part of current acceptance, and existing published release artifacts remain unchanged.

## Contract owners

| Surface | Candidate stable contract and authority | Not a public extension interface |
| --- | --- | --- |
| HTTP | [`openapi.yaml`](../../openapi/openapi.yaml): `/api/v1` resources, request and response schemas, strict JSON decoding, authentication, authorization, idempotency, revisions, Problem responses, pagination, and SSE cursor behavior. [HTTP contracts](#http-contracts) govern error and method precedence, and health/version aliases keep their documented behavior. | Go handler and package layout, Web store internals, Vue component APIs, and generated client internals. `/dev` and simulator routes are restricted to development environments. |
| CLI and configuration | Legacy Controller `--role=all` (optional; other values are rejected in every environment), `--migrate-only`, `--provision-management-workspace`, `--bootstrap-local-admin`, `OCSERV_*`, and mutually exclusive `_FILE` variables (the database URL, certificate-signer token, session key, audit checkpoint key, audit event key, and break-glass hash all follow the `_FILE` convention); managed-node `agent.env`, `privd.env`, `relays.env`, and one-shot enrollment or attestation subcommands. [Production deployment](../operations/production-deployment.md) and [Agent lifecycle](../operations/agent-lifecycle.md) define variable names, defaults, secret file permissions, and failure modes. | Undocumented flags, debug or probe switches, test-only environment variables, log phrasing, and direct SQLite table access. |
| Installation and upgrade | Exact release pins; `deploy/production/install.sh`, `controller-bootstrap.sh`, and `controller.sh`; `deploy/managed-node/install.sh`; and verified node install, upgrade, and rollback scripts. These entrypoints enforce configuration validation, HTTPS downloads, authorized upgrade digests, runtime trust checks, same-target retry semantics, and protected confirmed/pending state. | Replacing Compose files or container images directly, installing nodes from a source checkout, extracting archives without verification, or downgrading packages directly through the OS package manager. |
| Package names | `ocservia-agent-X.Y.Z-linux-{amd64,arm64}.tar.gz` with its checksum file; DEB packages `ocservia-agent_X.Y.Z-1_{amd64,arm64}.deb`; RPM packages `ocservia-agent-X.Y.Z-1.{x86_64,aarch64}.rpm`; `controller-release-{amd64,arm64}.json` and the `amd64` alias `controller-release.json`. | Workspace crate versions such as `0.1.0`, temporary build directory names, or treating container image config IDs as registry manifest digests. |
| Schema and transport | [`proto/`](../../proto/) defines message field numbers, enums, and gRPC services (see [Schema sources and generation](#schema-sources-and-generation)). Iroh ALPNs are `ocserv-platform/enroll/1` and `ocserv-platform/agent/1`, with enrollment proof version `1.1` and separately negotiated session versions. The Go/`transportd` Unix socket and the `Agent`/`privd` local socket are private interfaces paired within a single release. | Calling Iroh directly from Go, connecting third-party clients to `privd`, or assuming signed commands accept unknown Protobuf fields automatically. |
| Signing and hashes | [Command authorization v1](../development/command-authorization-v1.md), session and artifact grants, connection fence v2, fence binding v2, and [privd receipt v1](#privd-receipt-v1) use frozen canonical byte transcripts. [Semantic hash v1](../development/command-semantic-hash-v1.md) remains frozen for historical verification while the Controller issues [v2](../development/command-semantic-hash-v2.md). Raw Protobuf wire bytes are never used as canonical signing input. | Mutating an existing hash or transcript version to cover new fields, or inferring semantic hash v2 support from the `command.semantic-hash.v1` capability string. |
| Versions and durable state | Controller and `transportd` ship from a single release commit, and `Agent`, `privd`, and `upgrader` ship together in one verified node package. Cross-version combinations carry no compatibility guarantee. Upgrades and restarts preserve local journals, the root effect store, HMAC and receipt keys, endpoint identities, and revision/fence floors. | Swapping `Agent` and `privd` binaries independently, or using numeric version comparisons as dispatch authorization. |
| Database | [Database contract](#database): PostgreSQL 18.x serialized owner-role migrations, least-privilege runtime role, durable execution receipts, verified SQL checksums, and explicit rejection of unknown or drifted schemas. | Retired backends (including MySQL and MariaDB), down-migrations, automatic force-clean on dirty schemas, cross-engine data migration, or assuming additive DDL is backward compatible across versions. |

## Commands and recovery

### Command states

A command moves through separate states, and each state proves only what its name says.

- Accepted: the HTTP request returned `202 Accepted`. The operation, the typed signed command, the outbox event and the audit intent were committed in one database transaction. Acceptance says nothing about delivery or execution.
- Dispatched: the outbox worker claimed the row and `transportd` wrote the frame to the Agent stream. A transport acknowledgement means only that the frame was written. It does not mean the Agent accepted or executed the command.
- Executed: the Agent journal holds a terminal result for the exact command. A privileged result also needs a verified privd receipt (see [Privd receipt v1](#privd-receipt-v1)). Only a verified privd receipt or a terminal Agent journal entry settles a result.
- Unknown: the outcome cannot be proven, for example after a lost acknowledgement, an expired send lease, a stream that closed before a terminal frame, a missing or malformed proof, or a rejected privileged result without a valid receipt.

An `Unknown` command is never guessed and never resent for execution. It is reconciled with query-only delivery, and the Controller sends a retry only after authenticated durable evidence proves that the effect is absent. Unknown is also the result when a privileged outcome carries no verified receipt: later telemetry, daemon health, missing error logs or database timestamps do not settle it. Clients render the persisted Operation lifecycle and never treat `202 Accepted` or a transport acknowledgement as completed execution. The operator procedure is in [Unknown command reconciliation](../operations/incident-recovery.md#unknown-command-reconciliation).

### Controller delivery

All Controller-issued commands follow the same delivery pipeline. Either
`operations.Service.Create` (used by configuration, certificate and Agent
upgrade services) or the user and group
service signs and queues a typed command. The outbox worker dispatches it
through `transportd`, and the Agent journal and privd record the resulting
effect. The side-effect-free synthetic command endpoint exercises this full
pipeline without mutating host state.

Callers queue a typed `noop` or `echo` command via
`POST /api/v1/nodes/{node_id}/synthetic-commands`. Each request requires an
`Idempotency-Key` header along with either `If-Match: "revision-N"` or the
matching `expected_version` body field. When accepted, the endpoint returns
`202 Accepted`, the operation resource, and its `Location` header. Re-submitting
the same `Idempotency-Key` with identical parameters returns the existing
operation. Reusing that key with different input produces an RFC 9457 conflict
response.

Backend-owned stores commit the operation intent, typed Protobuf command,
outbox event, and audit intent together in a single database transaction.
Inside the signed envelope, the command idempotency key is the operation ID,
while the HTTP `Idempotency-Key` deduplicates only the API request itself.
Outbox workers claim pending rows using `FOR UPDATE SKIP LOCKED`, acquire a
single bounded lease per node, and commit that claim before calling
`transportd`.

A transport acknowledgement indicates only that the frame was written to the
Agent stream; it is recorded after the RPC returns and does not imply that the
Agent accepted or executed the command. The execution result arrives
asynchronously. If the transport send fails before completion, the worker
releases its claim and retries the identical signed envelope after one second,
relying on the Agent journal to replay the stored result if the earlier attempt
already arrived. If a worker's lease expires while a send is in flight, the
attempt transitions to `unknown` because the stream write may or may not have
reached the Agent. The Controller never guesses an outcome or resends the
command for execution. Instead, it issues a re-signed `RECONCILE_ONLY`
follow-up that queries the Agent journal up to the bounded attempt limit. Only
when reconciliation proves that the effect never occurred may the Controller
sign and send a `RETRY_IF_EFFECT_ABSENT` command.

Command envelope protocol `1.2` keeps its signed `expires_at` as the immutable
execution deadline. Recovery renews only the database's observation/outbox
window; the signed command deadline never changes. An expired, signed
`RECONCILE_ONLY` command may read authenticated durable evidence under the
current session, capability and owner fence, with a bounded transport timeout.
Both Controller retry admission and privd execution still reject an expired
execution deadline.

The frozen [Command authorization v1](../development/command-authorization-v1.md)
transcript already signs the protocol string, so this revision adds no wire
fields or signing-layout changes. An original `1.1` `EXECUTE_OR_REPLAY` dispatch
can be upgraded during its first recovery because its deadline has not been
renewed. Existing `1.1` recovery envelopes may have lost that deadline: they
remain observation-only, and old signed `RETRY_IF_EFFECT_ABSENT` grants can only
replay a retained root result. Missing or pruned results remain `unknown` and
require operator reconciliation. Deploy matched Controller/transportd and
Agent/privd packages; older nodes may reject the new command revision until
upgraded. The semantic hash formats are frozen in
[v1](../development/command-semantic-hash-v1.md) and
[v2](../development/command-semantic-hash-v2.md).

Whenever a database commit acknowledgement is uncertain, the service performs a
bounded readback of the original immutable intent, the specific attempt lease
or completed attempt, or the exact owner term and deadline, failing closed if
the expected record is missing, expired, or superseded. Services never allocate
a replacement intent or call `transportd` to mask a database error, and
repeating bookkeeping for an already committed transition is idempotent even
before an Agent result arrives.

Clients can inspect operation progress over REST or subscribe to the resumable
`/api/v1/operations/{operation_id}/events` SSE stream.
`/api/v1/operations/queue-metrics` reports unpublished event count, oldest age,
queue depth, and `unknown` count for the authorized workspace (only the
development principal sees Controller-wide totals). Across both supported
database backends, dispatch and recovery rely only on the worker polling the
outbox every 200 ms and reaping expired leases once per second.

On the Controller side, the configured database backend persists structured
command results alongside their semantic hash version (`0` for legacy, `1` for
canonical v1, or `2` for session-authority v2, which are protocol identifiers
independent of database migration numbers). Every ingested `command_result`
event must decode cleanly and satisfy all state, identity, hash-version,
digest, size, and timestamp checks, or the entire ingestion transaction rolls
back. Development simulation runs use a separate `simulation_result` event
type, and Agent-reported timestamps are stored only as historical metadata
without overwriting Controller-observed authority timestamps.

### Agent journal

The Agent records command acceptance and terminal outcomes in its
owner-controlled SQLite database, binding each side effect to a unique
idempotency key, a unique command ID, and a semantic payload hash with its hash
version (the Controller issues v2, while the Agent accepts both v1 and v2).
The idempotency key and command ID are enforced as independent unique
constraints, so only an exact match across all bound fields replays a stored
result. Reusing either identifier for a different command, or resending the
same command under a different hash version, is rejected prior to execution.

Every incoming command is validated and durably persisted as accepted before
its typed effect runs. For synthetic commands, the effect, execution counter,
and terminal result commit in a single SQLite transaction. For external Ocserv
mutations, the journal transitions the record to `running` before calling privd
and commits the bounded terminal result before acknowledging completion; if
persisting that terminal record fails, the Agent reports `unknown` instead of
fabricating a status.

On ingress, both the Controller transport path and the Agent command stream
validate raw Protobuf wire framing before decoding messages, rejecting unknown
fields or known fields with mismatched wire types in both envelopes and nested
payloads. Strict wire checking keeps future schema additions from silently
altering the behavior of a side-effecting command without an explicit protocol
version bump.

When an incomplete journal record is encountered during ordinary replay, the
Agent reports `unknown`. Recovery uses explicit delivery modes: `RECONCILE_ONLY`
inspects durable state without running side effects, and
`RETRY_IF_EFFECT_ABSENT` is permitted only after reconciliation has durably
recorded proof that the effect is absent. If reconciliation finds a matching
completed effect, it returns that result without re-executing the command.

Uncertain `ServiceReload` outcomes require manual operator reconciliation
because querying service status cannot prove whether a reload took place.
`SessionDisconnect`, `SessionTerminate`, `IpBanRemove`, and `ServiceReload`
are never retried on an unsigned `effect_absent` report. The Controller keeps
them `unknown` for reconciliation or manual handling, and privd answers a
`RETRY_IF_EFFECT_ABSENT` grant for these kinds only by replaying an existing
signed result, never by executing again. A `Rejected` result for a command
that requires a privd receipt is likewise kept `unknown` unless it carries a
valid proof.

Mutating commands execute serially on the node, with inbound command streams
capped at eight per Agent connection. Delivery mode stays outside the semantic
hash because it governs recovery flow and leaves the command's target side
effect unchanged.

Desired user, group, and configuration mutations enforce independent durable
resource-revision fences, and `ConfigApply` also checks privd's
root-owned prepared, applied, or absent effect record. Reconciliation can
reconstruct a lost Agent journal entry only from an exact durable effect
identity in privd; a matching current configuration file hash by itself never
proves that an earlier command executed.

You can run the Rust workspace tests (including the command-journal and Agent crash-boundary suite) locally with:

```bash
make rust-check
```

Basic CI runs this full workspace check in the `rust` job. The test matrix exercises duplicate deliveries,
lost acknowledgements, daemon restarts, process aborts at every persistence and
effect crash point, idempotency-key and command-ID collisions, cross-language
semantic hash vectors, explicit retry-after-absence flows, command expiry,
clock skew, revision and capability mismatches, cancellation, payload size
bounds, and SQLite read-only, disk-full, and corruption faults.

### Session and command matrix

The table below summarizes runtime admission rules enforced by [`enrollment.Service.AuthorizeSession`](../../control-plane/internal/enrollment/service.go), [`transportd`](../../rust/crates/transportd/src/lib.rs), [`Agent`](../../rust/crates/agent/src/main.rs), and `privd`:

| Offered state | Required behavior / upgrade path | Existing verification |
| --- | --- | --- |
| Session major other than 1; minor newer than 1 | Controller returns `INCOMPATIBLE_PROTOCOL` or `UPGRADE_REQUIRED`, and `transportd` rejects the session handshake. Upgrade the Controller and `transportd` before deploying a newer node protocol. | Enrollment trust lifecycle and transport handshake tests. |
| Grantless 1.0 | Restricted to read-only queries (status, version, sessions, IP bans, and config fingerprint); all mutations, artifact transfers, and inferred `.read` capabilities are rejected. Upgrade the node package to obtain session grants. | `TestEnrollmentBackendIntegration/legacy-capability-policy`, Agent `agent_accepts_only_explicit_grantless_read_only_sessions`, transport static-handshake, and shared session-policy tests. |
| 1.1 without sealing descriptors | Controller falls back to read-only access and blocks mutations; mismatched sealing descriptors are rejected. | `TestExistingActiveNodeBindsSealingKeysOnceIntegration`, enrollment lifecycle, and Agent signed-read-only tests. |
| 1.1 missing/invalid grant, wrong capabilities or stale owner fence | Reject the command and refresh the authorized session or owner fence term. Advertising fencing capability alone does not grant execution authority. | Session and fence signing goldens plus Agent and transport authority tests. |
| Privileged operation without registered receipt capability/key | Fail closed in production. Register the root receipt key, upgrade to a matched node package, and confirm the negotiated capability before dispatching mutations. | Attestation safety and missing-root-receipt tests. |
| Unknown command field, wrong wire type or truncated nested message | Reject during wire decoding before journaling or executing any side effect, even if the outer signature or hash appears valid. | Shared strict-wire corpus and descriptor-drift checks. |
| Uncertain non-idempotent mutation, including matched releases | Leave the operation in `Unknown` unless authenticated durable evidence proves the outcome, and pause conflicting writes until operator reconciliation finishes. | [Matched-release recovery boundary](#matched-release-recovery-boundary); strict recovery probes preserve their recorded outcomes. |
| Semantic hash v1/v2 | Recompute and verify the declared hash version (the Controller currently emits v2). Unknown, missing, or mismatched hashes and same-ID cross-version journal collisions fail closed. Existing v1 journal entries stay as written and are never rewritten to v2. | Shared semantic hash goldens and Agent v1/hash-version-conflict tests. |
| Config revision drift | Authorization revisions and applied configuration revisions are tracked separately: `Plan` binds `config_expected_revision`, and `Apply` binds the current and candidate hashes alongside the target revision. Stale or gapped transitions are rejected and recovered from durable evidence. | ConfigPlan lookup/Apply tests and Agent/adapter revision, restart, and recovery tests. |
| Historical/unattested CSR or lost P12 credentials | Reject unattested migration-era CSRs and require a fresh attested CSR. Encrypted P12 bundles remain bounded and single-use, and web download credentials live only in the active browser tab's memory until expiry. | Certificate and secret integration tests, OpenSSL adapter tests, and Web certificate workflow-reference recovery tests. |

The 20-payload strict-wire test corpus, Go reflection checks, Rust descriptor-mutation tests, and canonical signing/fence/receipt goldens cover all reachable command fields, including complete-configuration payloads; see [focused commands](../development/release-checks.md#command-wire-contracts). Passing wire-fixture tests verifies current decoder strictness and does not certify runtime compatibility with older published Agent binaries.

### Matched-release recovery boundary

Even when the Controller, `transportd`, `Agent`, `privd`, and `upgrader` all come from the same release, an interrupted non-idempotent mutation is not guaranteed to self-heal automatically. Seeing the node online with a fresh fenced session and a healthy `ocserv` process does not prove whether an in-flight reload ran before the interruption or whether re-running it is safe.

Automated query-only reconciliation (`RECONCILE_ONLY`) can settle an ambiguous command only when it recovers an authenticated `privd` receipt or an existing terminal entry in the `Agent` journal. It never infers completion from current daemon health, missing error logs, or database row timestamps. Before marking an operation terminal, reconciliation verifies the exact command ID, semantic hash and version, operation/idempotency identity, authorization revision, owner-fence lineage, `Agent` journal entry, and signed root receipt. If any piece is missing or inconsistent, the operation remains `Unknown`.

While an operation is `Unknown`, operators must pause conflicting writes to that node or resource. Do not delete the local SQLite journal or root effect store, hand-edit database tables, reset node `EndpointID`s, or resubmit the mutation under the same or a new idempotency key. Read-only reconciliation queries are safe. The explicit `RetryIfEffectAbsent` path may be used only after cryptographic proof confirms that no privileged effect occurred on the host (a stopped or healthy daemon does not by itself prove effect absence, and upgrading the node package does not reconcile pending commands). `SessionDisconnect`, `SessionTerminate`, `IpBanRemove`, and `ServiceReload` are never re-executed through that path; privd only replays an existing signed result for them. For step-by-step operator instructions, see [Unknown command reconciliation](../operations/incident-recovery.md#unknown-command-reconciliation).

In automated validation suites, strict recovery probes keep their recorded pass/fail status: a later passing run does not clear an earlier `Unknown`, and a scoped `EXPECTED-UNKNOWN` classification requires captured evidence of the unresolved command, query-only reconciliation, preserved node identity and journal state, absence of duplicate mutations, and paused conflicting writes. Missing test evidence is recorded as `NOT RUN` or `BLOCKED`.

Before performing a [Controller rollback](../operations/production-deployment.md#rollback), an [Agent rollback](../operations/agent-lifecycle.md#upgrade-and-rollback), or a [database recovery](../operations/incident-recovery.md#database-recovery), stop affected writers and command dispatch, reconcile in-flight work, and preserve both the Agent's SQLite journal and the Controller's operation, command, and result tables. Recover through a forward fix or a planned isolated restore (see [Database and recovery matrix](#database-and-recovery-matrix)), and never delete the local journal or clear replay protection to force an older binary to start. Across upgrades and rollbacks, preserve the matched Agent/privd/upgrader package set, Controller trust, endpoint identity, local journals, and root effect and key state, since replacing package binaries alone does not reconcile Unknown commands.

### Controlled session and service operations

Four typed commands govern active VPN sessions, IP bans, and the local daemon:

- `session.disconnect` targets one numeric session ID on one node boot;
- `session.terminate` targets the same identity and also invalidates its reconnect cookie;
- `ip_ban.remove` targets one canonical IPv4 or IPv6 address;
- `service.reload` targets only `ocserv.service`.

Reloading the VPN daemon requires an authenticated workspace Admin session. It
does not require a second authorization identity or approval record.

Every mutation request supplies a reason string, a unique `Idempotency-Key`, an optional `ttl_seconds` (1 to 300, defaulting to 60), and the current node revision in `If-Match` or `expected_version`. Session commands must also include the boot ID reported by the node API. When the Controller accepts a request, the endpoint returns `202 Accepted` with an asynchronous Operation record. Clients must render that Operation's persisted lifecycle state and never treat initial queueing or transport dispatch as completed execution.

Idempotency keys are scoped to the workspace and bind the target node, typed command payload, expected revision, delivery parameters, acting principal, authorization action, reason, and TTL. Reusing an idempotency key for a different node or mutation payload fails with a conflict. Request and trace IDs identify an individual HTTP attempt and may vary across otherwise identical replays.

Before committing the Operation, Command, outbox event, and audit intent, the Controller verifies that the target node holds the matching active capability and that any targeted session or IP ban is present in current observed state. Once the command reaches the node, the Agent revalidates the signed envelope, expiration timestamp, revision, capability, semantic hash, target syntax, and boot identity before forwarding the request to privd.

Privd never executes arbitrary shell strings. It maps the four command types to fixed, bounded argument vectors: `occtl disconnect id`, `occtl terminate id`, `occtl unban ip`, and `systemctl reload ocserv.service --no-ask-password`.

`occtl terminate` exists only from Ocserv 1.4.2. The Agent negotiates `ocserv.session.terminate` for a session only when privd reports such a version (enrollment keeps the static capability set). Privd rejects a terminate on an older or unparsable version before running any child, and that is a definite failure rather than `unknown`.

Duplicate deliveries replay the result already recorded in the local journal. If an execution outcome is interrupted or ambiguous, the command remains `unknown`. A privileged result without a verified privd receipt stays `unknown`; the Controller result handler (`control-plane/internal/transportevents/result.go`) converts an unverified privileged result to `unknown` and does not accept it as terminal. Later node telemetry, the absence of the session or IP ban, and daemon health never settle the result and never justify a retry. `session.disconnect`, `session.terminate`, `ip_ban.remove` and `service.reload` are never re-executed after an uncertain outcome; privd only replays an existing signed result for them. Only persisted, signed proof of effect absence can authorize the explicit retry path of other command kinds.

Each command type uses a distinct protobuf oneof tag and requires the node to advertise the corresponding capability, so unsupported commands are rejected before dispatch. Before rolling back a Controller or Agent binary, stop creating and dispatching these four command types and allow all nonterminal Operations to drain or reconcile. Keep both the Agent SQLite journal and the Controller command history intact so duplicate deliveries and `unknown` outcomes remain reconcilable. When an older binary cannot read the schema state, see [Database and recovery matrix](#database-and-recovery-matrix).

### Privd receipt v1

Privileged mutations record their state in a bounded, root-only SQLite store
under `/var/lib/ocservia-privd` that never leaves the host. Each
authenticated command record binds the node, command, operation, idempotency
key, action, authorization and effect revisions, semantic payload hash, effect
kind, resource, expiry, and bounded successful result. An exact replay returns
the stored result without repeating the root effect. Records for desired user,
group, and configuration changes also bind the authoritative file transition.

The store's identity is authenticated, so a missing or mismatched database or
HMAC key fails closed. Privd resolves any prepared whole-file transition before
starting another user or group mutation, so later writes cannot overwrite
recovery evidence. Only an exact prepared record with a matching before-state
proves that an effect is absent. Operators must keep the generated HMAC key and
SQLite database together during backup, restore, and binary rollback.

Every terminal privileged response includes a `PrivdResultReceiptV1` covering
either a success or a terminal rejection with error code `privd_rejected` or
`capacity_exceeded`. Transient errors such as an unavailable adapter omit the
receipt and remain non-terminal. For `agent.upgrade`, the initial receipt
records only that the upgrade was "scheduled". The upgrader's final durable
outcome is retrieved later through the attested upgrade-result read.

Privd signs each receipt with a per-node Ed25519 key kept in the root-owned
mode-`0700` state directory as a mode-`0600` single-link regular file. Key
generation uses random bytes written with create-new semantics, file fsync,
atomic rename, and parent-directory fsync. Privd refuses to run if the key's
owner, mode, length, file type, link count, or symlink state is unsafe.

The canonical receipt format is independent of Protobuf wire encoding. It binds
the node, command, operation, idempotency key, semantic hash, command and result
kinds, exact result digest, effect identity and revision, acceptance and
completion timestamps, and certificate-specific digests. The root effect store
commits the exact result bytes and signed proof before returning success. On an
exact duplicate request, it replays those identical bytes and signature.

The Agent treats this proof as opaque bytes. It commits the result and proof
together in a single SQLite journal transaction for later replay, and it marks
the command Unknown if the proof is missing or malformed. Transportd checks only
the bounded shape and version before forwarding the payload unchanged.

On the Controller, the result handler reconstructs the canonical transcript. It
accepts a privileged terminal outcome (success or rejection) only when its
claims match the issued command and the signing key is an independently
registered, active, in-window key for that node. If verification fails, the
Controller emits audit and security-alert records and routes the command into
reconciliation without advancing desired state.

During reconciliation, the Agent first queries privd for the exact
authenticated effect record. When a matching root record exists, privd returns
the original result bytes, receipt, and signature, which the Agent atomically
restores to its local journal before reporting a terminal result. Read-only
observation of desired effects can prove that an effect never took place and
unlock a safe retry. Synthesizing a privileged success or failure still requires
the original receipt. If root evidence is missing or conflicting, the command
stays Unknown.

Privd signs a fixed binary transcript independent of Protobuf marshal output. The
transcript starts with the ASCII domain string
`ocservia/privd-result-receipt/v1` followed by a NUL byte. Enum and version
fields are encoded as unsigned 32-bit big-endian integers, and the effect
sequence uses an unsigned 64-bit big-endian integer. Every byte slice and UTF-8
string is preceded by an unsigned 32-bit big-endian byte length. Timestamps
consist of signed 64-bit seconds followed by unsigned 32-bit nanoseconds, both
big-endian, where seconds must be nonnegative and nanoseconds less than one
billion. Booleans and optional-presence markers occupy a single byte (`0` or
`1`), keeping absent fields distinct from present-but-empty values.

Fields appear in a fixed order: receipt version, node ID, key ID, command ID,
operation ID, idempotency key, semantic-hash version and digest, command kind,
result kind, terminal state, exact result digest, error-code digest, effect
record ID and sequence, accepted and completed timestamps, replay marker, and
the optional certificate binding. When present, the certificate binding orders
the certificate ID, CSR digest, public-key digest, requested-subject digest,
and root effect record ID. UUIDs are 16-byte UUIDv7 values, digests and Ed25519
public keys are 32 bytes, effect IDs are 16 to 32 bytes, and signatures are 64
bytes. The complete canonical transcript is bounded to 2048 bytes and the
encoded proof to 64 KiB. Unknown receipt versions, out-of-range enums, lengths,
or timestamps fail closed, and unknown Protobuf fields are excluded from the
transcript.

Establishing key trust requires a random one-time Controller challenge
bound to the installation token and consumed through
`POST /api/v1/node-bootstrap/attestation` and `POST /api/v1/node-bootstrap/register`
(see [One-token installation](../operations/agent-lifecycle.md#one-token-installation)).
The bootstrap endpoints enforce bounded per-process request and in-flight
budgets, returning `429` and `Retry-After` before any body parsing or database
work when exceeded.

The helper ships inside the `ocservia-privd` binary (`attestation-registration`), so verified package
installs and rollbacks never lack the provisioning tool. The Agent cannot read
the key or credential, so it cannot register a self-chosen key.

Active keys are retained with explicit states (`active`, `retired`, `revoked`),
and explicit revocation clears active trust and increments the node authorization
revision. The initial key registration path runs once per installation via
[Enroll the node](../operations/agent-lifecycle.md#enroll-the-node).

Production runs have no legacy-success fallback, and historical certificate rows
remain read-only history until a new attested CSR is generated. Rolling back
requires stopping privileged dispatch, reconciling Unknown work, and restoring
the Controller, Agent, privd, root effect store, and key state as a matched set
without ever rolling back a single peer in isolation.

### Provisioned trust

Both node services refuse to start in production unless the pinned Ed25519
public key and the purpose-separated sealing keys are configured. At startup,
privd derives each RSA public key from its private key, checks it against the
enrolled fingerprint, and rejects reused key pairs. The command verification
key is loaded through descriptor-relative, no-follow operations, while sealing
keys must be root-owned regular single-link files with private permissions and
safe directory ancestry. Privd also reads `NODE_ID` from its configuration and
rejects otherwise valid Controller proofs issued for any other node. Enrollment
signs and persists both public-key descriptors so that subsequent session and
password operations fail closed if the advertised or selected key ID diverges
from the enrolled binding.

When updating `/etc/ocserv/ocpasswd`, privd writes a single-link `root:root`
mode `0600` regular file using descriptor-relative, no-follow file operations,
rejecting any existing file or parent directory with unsafe ownership,
permissions, hardlinks, or symlinks.

## Configuration and node TLS

### Planning

Configuration planning renders a typed template for a target node, checks the
node's current configuration revision and advertised capability matrix, and
dispatches the immutable candidate to the Agent for side-effect-free
validation. Templates are restricted to an allowlisted set of Ocserv
directives, with node-scoped variables referenced as `${NAME}` and
secret-bearing directives represented by structured `SecretRef` objects; the
API rejects inline secret values and caller-chosen target paths. Rendering is
deterministic so equivalent inputs always produce identical canonical output
and the same SHA-256 candidate hash.

The [complete node-local TLS profile](#node-local-tls-provisioning) uses its
own capability identifiers and typed command payloads. During planning, it
resolves immutable protected TLS bundles on the node and validates the complete
native configuration without omitting directives or emitting partial-validation
warnings. Its validation result returns both the logical candidate hash and the
exact materialized file hash, which are bound alongside the plan ID, previous
file hash, revision, and expiry into the subsequent operation. Nodes that
do not support the complete profile reject these commands outright with no
fallback to the legacy profile.

Under the legacy profile, privd writes the validation candidate to a temporary
file next to the fixed Ocserv configuration, checks every directive through a
bounded structural parser, and runs the fixed Ocserv binary to validate the
non-secret directive subset. Unresolved `SecretRef` lines are omitted only from
the temporary file passed to the native parser and surface as typed warnings,
without materializing any secret key on disk. Privd deletes the temporary file
as soon as validation finishes, compares the active configuration's fingerprint
before and after the check, and rejects the plan if the underlying file changed
in the meantime. Planning never modifies the active configuration file or
reloads Ocserv.

Calling `GET /config-plans/{plan_id}` returns the candidate hash, operation
state, validation status, warnings, and a secret-safe diff that omits both
`SecretRef` keys and active secret values. An authorized Admin can apply only a plan that has validated cleanly and remains unexpired.
The operation binds the candidate hash, node ID, expected revision and expiration time. Neither planning nor apply ever
masks a stale caller revision by re-reading and substituting the current
revision.

### Apply and rollback

Applying a configuration requires current authorization and an unexpired, remotely validated plan. Within a
single transaction, the Controller locks the node row, re-verifies the
configuration revision, validation fingerprint, negotiated capability, and
automation lock, rechecks the active identity and session, and commits the Operation, signed
command, outbox entry, audit intent, and apply record. When using the complete
profile, the apply command also pins the plan ID, logical and materialized
hashes, previous file hash, revision, and expiry. Root revalidates both the
immutable TLS bundle and the full native configuration, and legacy command and
receipt formats stay unchanged.

On the node, privd never accepts a caller-supplied file path or executable
name. Holding the fixed configuration lock, it:

1. Verifies the active file's fingerprint.
2. Writes backup and staging files in the same directory with preserved
   permissions and ownership.
3. Fsyncs the staged file, atomically renames it into place, and fsyncs the
   parent directory.
4. Reloads the fixed Ocserv service.
5. Verifies the parser check, systemd unit status, and an `occtl` session query.

Privd retains up to ten successful backup files per node.

If the reload or post-reload health check fails, privd restores the exact
backup bytes, reloads the service, and re-verifies the fingerprint and health
probes before returning `rolled_back`. If restoring the backup or verifying the
rolled-back service also fails, privd returns `failed_critical`, causing the
Controller to lock configuration automation for that node and raise a critical
alert. The operator response is in
[Plan and apply a configuration](../operations/agent-lifecycle.md#plan-and-apply-a-configuration).

Every queued apply increments a monotonic desired effect revision, including
attempts that end in `rolled_back` or `Unknown`. This desired effect revision is
tracked separately from the applied configuration revision, so an
`A -> B -> A` configuration rollback still advances the effect revision and
never reuses an earlier number. Privd durably binds
`config_apply / ocserv.conf / desired_revision` to the command ID, idempotency
key, and semantic hash so applied and rolled-back evidence remains valid even
after the command's authorization proof expires. Recovery can extend the
database observation window while retaining the original signed execution
deadline, as described in [Controller delivery](#controller-delivery). Neither
matching file contents nor a reconstructed Agent journal can make an expired
command authorization executable again.

### Complete profile contract

The complete configuration profile is gated by dedicated capabilities and typed
Protobuf payloads. Older nodes reject complete-profile commands without falling
back to v1. Existing v1 payloads, hashes, and receipts remain untouched.
Advertising a capability at handshake time does not bypass runtime validation
when a command executes.

Within the finite grammar of the complete profile, configurations may use plain
authentication against `/etc/ocserv/ocpasswd`, the fixed `/run/ocserv.socket`
control socket, and operator-provisioned node-local TLS material. Unknown or
duplicate directives, `include` statements, hook scripts, caller-chosen file
paths, and unresolved `SecretRef` entries are rejected.

Each opaque TLS reference binds an immutable version, node UUID, allowed
directive slot, public certificate fingerprint, and SPKI fingerprint. Privd
resolves file paths strictly beneath its fixed root-owned trust directory and
rejects unsafe parent permissions, symlinks, hardlinks, wrong file ownership or
modes, missing versions, mismatched node or slot bindings, expired
certificates, or private keys that do not match the public certificate. Private
key bytes and digests of private key material never enter command payloads, API
responses, audit logs, or Controller database tables. The on-disk bundle
manifest is only a local trust input, read under a shared file lock that
prevents version removal or replacement while a plan, apply, or rollback is in
progress.

The protocol keeps three digests distinct: the logical candidate identity
(which pins the node, directive values, reference UUIDs, versions, and public
fingerprint bindings), the exact materialized file hash, and the signed command
semantic hash. The committed operation binds the plan, both configuration hashes,
expected current revision and expiry in the same transaction as command, outbox
and audit. The domain service revalidates the caller's session and resource
permission. Reusing an idempotency key returns the original operation only for
the same intent; different content conflicts. A fresh key must still satisfy
current plan, node revision and active-operation guards.

Wire messages are generated from the Protobuf and OpenAPI contracts, backed by
shared Go and Rust canonical hash vectors and strict-wire negative tests that
cover the complete-profile payloads without altering existing v1 or v2 hashes.
If any command, hash, fence, root effect, journal, or receipt evidence is
missing or inconsistent, the operation stays `Unknown` and is never cleared
automatically or resent as a fresh mutation.

Release acceptance requires passing focused protocol, Controller transaction,
root path, and TLS unit tests together with end-to-end native parser, apply,
reload, exact byte-level rollback, maintenance restart, and browser checks.
[Release policy](../development/release-checks.md#release-policy) defines the
required exact-candidate business and native package gates across both
supported CPU architectures. Contract review, unit tests, or checks from
earlier candidates do not satisfy release acceptance.

### Node-local TLS provisioning

Nodes supporting the complete profile negotiate `ocserv.config.complete.plan`
and `ocserv.config.complete.apply`, enforcing all invariants from the
[complete profile contract](#complete-profile-contract). The operator steps are
in [Configuration and TLS operations](../operations/agent-lifecycle.md#configuration-and-tls-operations).

The complete profile runs Ocserv's worker under a dedicated unprivileged
`ocservia-vpn` system user whose primary and sole group is `ocservia-vpn`.
Complete-profile configurations pin Ocserv's worker account to `ocservia-vpn`
while leaving the `ocservia-agent` and `ocservia-privd` service accounts and
capabilities unchanged.

The file ownership, mode and certificate requirements for the node-local
bundle, and the provisioning helper, are in
[Provision node-local TLS](../operations/agent-lifecycle.md#provision-node-local-tls).

The bundle reference's public `key_path` metadata is formatted as lowercase hex
digests:

```text
<node UUID>/<certificate DER SHA-256>/<SPKI DER SHA-256>/<CA DER SHA-256 or none>
```

On a server that is already running, initial activation of the complete profile
or any change to the TLS reference or version requires a separately authorized
maintenance restart, because `SIGHUP` does not reload startup-only bindings and
no Agent or privd command restarts the daemon.

In a complete-profile template, `server-cert`, `server-key`, and optional
`ca-cert` all reference the same `SecretRef` UUID, and privd's root resolver
maps each directive to its fixed file slot within that bundle. When creating a
plan, the Controller resolves the reference's current version and pins it in
the signed command so a subsequent `SecretRef` rotation does not alter an
existing plan. Both planning and apply reject any candidate that changes the
TLS path or version, listener ports, authentication method, worker account, or
control socket path relative to the active `/etc/ocserv/ocserv.conf`. They also
fail closed if the active file contains `include` directives, virtual hosts,
duplicate directives, or settings outside the finite grammar.

Every complete-profile template must specify `auth`, `cookie-timeout`,
`device`, `dns`, `ipv4-network`, `max-clients`, `max-same-clients`,
`server-cert`, `server-key`, `socket-file`, `tcp-port`, and `udp-port`, and may
optionally include a single `route` directive set to one canonical IPv4 CIDR or
`default`. The `auth` and `socket-file` directives are locked to their fixed
production paths, and callers cannot supply custom TLS file paths, `include`
directives, or hook scripts.

The node detail page in the Web UI submits complete-profile plans. Once a plan
validates without warnings, it records the logical candidate hash, exact
materialized file hash, previous file hash, revision, and expiry for the Admin to inspect before apply. Replaying an apply request cannot authorize
a different operation. During apply, privd re-verifies the TLS bundle and runs
the native `ocserv` parser over the complete file before atomically replacing
`/etc/ocserv/ocserv.conf` and reloading the daemon, restoring the exact
previous file bytes if any post-reload health check fails.

If an apply ends in an uncertain state, never submit a new apply request or
hand-edit the Agent journal, root effect store, or Controller database rows;
preserve all command, hash, fence, and receipt records and follow the
[matched-release recovery boundary](#matched-release-recovery-boundary).

## Enrollment and transport

### Enrollment

The standard installation and bootstrap flow issues a single-use token via
`POST /api/v1/node-bootstrap-tokens` (`installation: true` returns an `oit1_`
root installation token; omitting it returns an `obt1_` bootstrap token
persisted only as a SHA-256 digest and unbound until a valid endpoint
possession proof arrives). When the token creation request specifies the
optional `expected_endpoint_id`, the Controller rejects any other endpoint
before consuming the token. In a single database transaction, enrollment
commits the endpoint binding, node row, sealing keys, advertised capabilities,
token consumption, and audit event. The Controller rejects any `EndpointID`
already present in its database regardless of node state, so a bootstrap token
can never re-enroll an endpoint that has already been bound or revoked.

For token-only installation, `installation: true` issues an `oit1_` root token
with an HTTPS Controller origin. The root installer exchanges it through
`POST /api/v1/node-bootstrap/config` for pinned Controller identity, public
command key, Relay configuration and a one-way derived `obt2_` Agent token. Only
the child-token hash is persisted. Its atomic consumption immediately creates
and activates (`nodes.status = 'active'`) one node; EndpointID possession proof
and sealing-key bindings remain mandatory. The root token never enters
Agent-readable files or command arguments.

`POST /api/v1/node-bootstrap/attestation` produces a node-bound root
registration challenge; `POST /api/v1/node-bootstrap/register` verifies the
`PrivdAttestationRegistrationV1` signature and registers only the node's first
active `privd` attestation key. The first registration must complete within the
original TTL. Exact same-endpoint/same-key recovery remains possible after
expiry, but a different or revoked key cannot be registered using that token.
`POST /api/v1/node-bootstrap/status` returns only that bound node's readiness.
All four endpoints are bounded, rate-limited, no-store,
credential-authenticated without an administrator browser session, and reject
Agent child credentials.

Manual Agent CLI recovery pre-generates or loads the long-lived endpoint
identity on the host (`--prepare-enrollment`), then uses the existing protected
`obt1_` credential through `--enrollment-token-file`. The standard installer
accepts installation tokens or retained bootstrap state, never install.env or
operator-supplied child-token configuration. Once consumed, an `obt1_` bootstrap token may be
replayed only by that same `EndpointID` while the node has not yet transitioned
to another endpoint or state, returning the committed node ID. The command lines
are in [Enroll the node](../operations/agent-lifecycle.md#enroll-the-node).

On the node, the Agent stores its endpoint private key inside an owner-only
local directory alongside the pinned Controller `EndpointID`. Through the
enrollment ALPN, the Agent submits the token and public Agent metadata together
with an Ed25519 signature produced by its Endpoint `SecretKey` over a
versioned, domain-separated canonical transcript that binds the token digest,
`EndpointID`, protocol version, Agent metadata, environment, nonce, timestamp,
and sorted capability list. The Controller verifies this signature
independently, treating the `EndpointID` reported by `transportd` as an
additional channel binding.

The token file passed to the Agent must be a protected regular file (an absolute
non-symlink path readable only by the Agent user, or owned by root and
group-readable by the Agent group). See
[Recover an uncertain enrollment response](../operations/agent-lifecycle.md#recover-an-uncertain-enrollment-response)
before retrying an interrupted enrollment.

To revoke a node from any non-revoked state, call
`POST /api/v1/nodes/{node_id}/revocation` with an authorized session and a
nonempty reason.
Revocation is recorded durably in the database, blocks future handshakes, and
instructs `transportd` to close any active connection. The database transaction
also queues a durable trust-convergence task, so receiving a `503` from the
revocation endpoint after commit indicates only that `transportd`
synchronization is still pending. The database state change itself remains
committed. The background worker retries the exact trust
revision until `transportd` replies `applied` with that retained state and
revision, keeping stale or rejected responses pending and closing a revoked
node's connection only after its exact tombstone is applied. Meanwhile,
Controller event ingress checks every incoming event's node ID and `EndpointID`
against active database trust, rejecting events from any lingering revoked
session before convergence finishes.

When the revocation request itself completes the fenced trust update and then
the fenced connection close, it records them on the queued task for exactly that
node, endpoint, revision and reason, so the worker does not retry a close whose
connection, and with it the fencing owner, is already gone. A failed close stays
pending. The worker never
counts lost connection ownership as delivery, and any other intent stays with
the worker.

An RPC timeout, cancellation or unavailable response can occur before or after
the mutation. Neither that error nor a later owner/connection snapshot proves
the historical result. An identical trust update is idempotent for the same
node, EndpointID, state and revision, but still requires a valid binding for its
operation identity (which also covers the reason). Applying a higher revision
can itself close the old connection, so a lost update acknowledgement can also
remove the owner needed for a retry. Retries are conditional on fencing and
cannot guarantee eventual completion.

`CloseNode` verifies its binding before removing the current connection; its
`NOT_FOUND` response is treated by the Go client as satisfied current absence.
That response does not identify which earlier operation, if any, closed the
connection. Accordingly, `close_applied` records satisfaction of the existing
close contract, not a durable receipt for a particular historical close.
`GetNodeConnection` and `GetOwnerFence` expose current in-memory snapshots, not
operation receipts. Restart loads authoritative database trust into transportd's
in-memory policy before accepting connections, but does not reconstruct lost
RPC acknowledgements or settle queue records. A failed or nonmatching
`RecordSynchronized` can leave work pending even after transport effects ran.
The API logs a persistence error but does not undo committed revocation.
See the [read-only incident checks](../operations/incident-recovery.md#revocation-committed-with-synchronization-pending).

When claiming trust-convergence work, the worker computes lease deadlines from
a fresh database wall-clock read after acquiring the claim lock, and uses the
retained attempt counter to fence late RPC responses even if the same worker
reclaims the same revision, preventing equal or older revisions from
overwriting newer intent. Each claim holds a 10-second lease that the worker
renews before every external call and at one-third lease intervals while a
transport RPC is in flight. Losing lease renewal cancels the in-flight RPC. The
worker completes a final renewal check before writing completion bookkeeping so
a delayed response cannot clobber a superseding update. If closing a revoked
connection fails, the worker retries the close without re-sending the already
acknowledged trust update.

During the regular Agent handshake, the Agent advertises its supported
capabilities and the Controller returns the sorted intersection with the node's
registered database capability set, so advertising unknown capabilities
never blocks enrollment or connection. Under handshake protocol `1.1`, the
Controller signs that negotiated subset, the `EndpointID`, node ID,
authorization revision, and expiration time into `SessionGrantV1`. Protocol
`1.0` sessions are restricted to read-only capabilities.

Configuring `OCSERV_CONTROLLER_ENDPOINT_ID` enables the HTTP enrollment routes
(which otherwise return `404`) and starts the trust service on
`OCSERV_TRUST_SOCKET` in the Controller process. Aside from one-shot
database maintenance commands, production startup fails if the endpoint ID is
omitted:

```text
OCSERV_CONTROLLER_ENDPOINT_ID=<64 lowercase hex characters>
OCSERV_TRUST_SOCKET=/run/ocserv-trust/control-plane.sock
```

Configure `OCSERV_TRANSPORT_UID` and `OCSERV_TRANSPORT_GID` with the exact
numeric UID and GID of `ocservia-transportd`; production startup refuses a
missing transport identity or a transport UID that matches the Controller UID.
The directory housing `OCSERV_TRUST_SOCKET` must already exist, be owned by the
control-plane user without group or world write permissions, and remain
separate from the transport socket directory so a compromised transport process
cannot replace the trust socket. The configured `OCSERV_CONTROLLER_ENDPOINT_ID`
must match the public key derived from `ocservia-transportd`'s controller key.
When connecting over the Unix domain socket, both peers verify the remote
process UID, the socket file's UID, GID, and mode, its non-writable directory
ancestry, and its stable device and inode numbers. Graceful shutdown unlinks
only the specific socket inode created by that server instance.

Before running database recovery, stop new node enrollments and allow pending
trust transitions to converge, preserving all consumed token records, endpoint
bindings, node capabilities, and revocation history.

### Transport

`ocservia-transportd` manages a single Iroh endpoint that routes only
`ocserv-platform/enroll/1` and `ocserv-platform/agent/1`, exposing a versioned
gRPC service to the Go Controller over a mode `0660` Unix domain socket so Go
packages never import Iroh types directly. The daemon holds neither a database
client nor database credentials. Both its administrative socket and the trust
socket enforce exact configured peer UIDs. The transport and control-plane
containers share the numeric `ocservia` group (GID `65532`) under distinct
non-root UIDs, so only group members can traverse the runtime directory and
reach the socket path. Group membership grants only filesystem traversal and
never substitutes for peer UID authentication.

The Controller identity key is passed via `--key-file`, pointing to an absolute,
non-symlink regular file owned by the service user with zero group or world
permission bits and containing either 32 raw bytes or 64 lowercase hexadecimal
characters. The key file stays out of container images, the source tree,
command-line flags, and logs. At startup, the daemon logs only the derived
public endpoint identifier.

In the normal database-backed lifecycle, `ocservia-transportd` starts with
`--trust-socket /run/ocserv-trust/control-plane.sock`,
`--control-plane-uid 65534`, and `--control-plane-gid 65532` (where endpoint
IDs are 32-byte lowercase hexadecimal public keys and node IDs are UUIDv7
values). Under this configuration, the Iroh connection hook admits known Agent
`EndpointID`s from a bounded authoritative snapshot and keeps the Agent ALPN
fail-closed until the initial snapshot synchronization completes. Unknown
enrollment `EndpointID`s are constrained by a dedicated local pre-auth class
that never triggers a Controller database query before application-layer frame
validation.

Both sides of the trust socket verify peer and filesystem identity. The trust
client checks the socket file and server UID, and the Controller trust server
independently verifies `transportd`'s UID. Both require a non-symlink Unix
socket with exact UID, GID, and mode beneath trusted, non-writable parent
directories, and both reject any pathname identity change during connection
setup.

`transportd` requires `--trust-socket` in production so all authorization and
trust updates flow from the live Controller database, and refuses a missing or
unreachable trust socket. Under `--trust-socket`, the Controller issues signed
protocol `1.1` session grants and revocations over the Unix socket boundary,
which `transportd` cannot mint on its own.

For isolated local development, `--relay-mode default` uses the public relay
set and is never used in production. `--relay-mode disabled` is for isolated
direct-path tests. The
endpoint enforces bounds on handshake payload size and duration, frame and
flow-control windows, remotely initiated streams, active connections, idle
timeout, connection rate, event retention, and UDS subscribers. Trust admission
is divided into five independent classes: known-Agent handshake, Agent
authorization, Agent registration recheck, unknown-enrollment pre-auth, and
enrollment completion. Each class maintains its own rolling rate window,
concurrency semaphore, bounded metrics, and rejection reason, so unknown
enrollment traffic can never consume capacity reserved for enrolled Agents.
When the bounded identity table fills, new unknown identities are shed without
evicting existing entries. Each class keeps in-process counters for accepted
attempts, rate and capacity rejections, and checks in flight, keyed only by the
fixed class name and never by raw `EndpointID`s or tokens. `transportd` does
not export these counters as metrics.

On the enrollment ALPN, `transportd` reads a single bounded application frame
under a strict deadline to extract the format-checked one-time token. Still
within the unknown pre-auth budget, the Controller first verifies the endpoint
possession proof and confirms that the token is unconsumed without mutating
database state. Only after that read-only authority check succeeds does the
request enter the enrollment-completion class, where a second transactional
call locks and atomically consumes the token alongside the durable node write,
rejecting any concurrent or subsequent replay. Node activation, revocation, and
endpoint rotation advance the synchronized trust snapshot, and revoked
identities remain fail-closed even if the Controller becomes unreachable.
Direct and Relay connections share the same internal admission classes. Relay
authentication acts only as defense in depth and neither establishes an
Agent principal nor taps reserved Controller capacity.

Connection status queries return the Agent instance ID, active direct or relay
path with path details, RTT, connection establishment timestamp, and last-seen
timestamp, together with the negotiated capability set, authorization revision,
and signed-session expiration time. Database-backed sessions accept a nonzero
revision and expiry only from the Controller handshake response. Before
opening an Agent stream for command dispatch or artifact transfer, `transportd`
verifies the session mode, negotiated capability, authorization revision, and
grant expiry, and closes any connection whose retained session grant is
superseded by a higher authoritative trust revision.

Calling `SendCommand` returns as soon as `transportd` writes the command frame
to the Agent stream and closes the send side, before the Agent accepts or
executes the command. The Agent's execution outcome is read asynchronously and
published later as a `command_result` event. If a command stream closes early,
times out, or yields a malformed frame prior to a terminal event, `transportd`
emits an `unknown` result. A command stream carries exactly one terminal frame;
a Heartbeat or any other non-result frame on it is malformed and is never
published as telemetry. In every such case `transportd` emits the `unknown`
result that the Controller must reconcile instead of treating it as a clean
failure or resending it as a new command.

When one to eight `--controller-verification-key-file` flags are configured, `transportd`
also validates the Controller-signed owner-fence carriers on command, artifact,
and trust-update requests. Once a node registers a fence, those carriers are
mandatory on every subsequent call.

Trust synchronization calls return `applied`, `stale`, or `rejected` along with
the exact retained state and revision, and connection side effects run only
after an authoritative transition advances that retained state. Once an
`EndpointID` is revoked, its tombstone and original node binding are permanent:
neither a stale message nor a higher-revision `Active` update can reactivate or
rebind a revoked endpoint.

The Rust workspace pins Iroh in `Cargo.toml` (version in `toolchains.lock` and
`Cargo.lock`) using a provenance-bound patch of that exact `crates.io` release,
with the full dependency graph locked in `Cargo.lock`. Production Agent and
`transportd` launchers configure a single dedicated Relay URL (`RELAY_URL_A` /
`OCSERV_RELAY_URL_A`), and production supports only one dedicated Relay (see
[Dedicated Relay](../operations/production-deployment.md#dedicated-relay)).

Internal transport or relay behavior can shift across patch releases without
altering the Go gRPC contract, so any Iroh patch update must still pass direct, relay, ALPN-rejection, path-event, shutdown,
dependency, audit, and license checks.

The standalone `iroh-relay` build is pinned separately under `iroh` in
`toolchains.lock`: `scripts/build-relay.sh` verifies the upstream crate archive
and compiles the unmodified CLI against `deploy/production/relay.Cargo.lock` for
the production Relay image (`deploy/production/relay.Dockerfile`). The Rust base
image tag reflects the Rust compiler version, not an Iroh endpoint or Relay
protocol version. Never substitute an unpinned `cargo install --version` build,
because the standalone binary must use the reviewed lock independently of the
endpoint workspace.

Rolling back to an older protocol-compatible binary is not automatically safe
from a security standpoint: older Iroh releases reintroduce the oversized Relay
batch and predictable mapped-address flaws, and building Relay from its upstream
lockfile reintroduces an older `rustls` advisory. When rolling back, use a prior
build that already includes those security fixes, or isolate the affected
service without disabling authentication or resetting journals and revisions.
Upgrade `transportd` and Agents without altering keys, ALPN strings, session
grants, or authorization revisions. Production upgrades always require an
explicit version inventory, authorization, and maintenance window. When
servicing the Relay, preserve its URL and TLS material, and after bringing it
back online verify that Agents establish fresh connections and reconcile pending
command results.

In local development, `ocservia-transportd-stub` is the side-effect-free
default and must never be used for production recovery. During incident
recovery, preserve bootstrap tokens, endpoint bindings, node capabilities,
and the Controller identity key (see
[database recovery](../operations/incident-recovery.md#database-recovery)).

### Controller history compaction

The Controller scheduler compacts aged terminal command payloads, retired-node
snapshots, and aged audit/security detail in periodic background passes that
process bounded, indexed batches inside short transactions while checking the
scheduler leadership fence. Existing telemetry retention keeps its established
14-day raw, 90-day five-minute, and 13-calendar-month hourly windows.

Compaction replaces eligible non-`config_plan` terminal command payloads with an
evidence header after `OCSERV_COMMAND_DETAIL_RETENTION_DAYS` (default 90,
range 30 to 365) once expired and free of active/unknown attempts or pending
projections, retaining command identities, signatures, semantic hashes,
revisions, and receipts. Retired-node cleanup
(`OCSERV_RETIRED_NODE_RETENTION_DAYS`, default 90, range 30 to 730) deletes
bounded session snapshots for revoked nodes while keeping node and endpoint
tombstones. Audit and security-event compaction
(`OCSERV_AUDIT_RETENTION_DAYS`, default 365, range 90 to 2555) re-authenticates
compacted rows under the `ocservia/audit-detail-compaction/v1` MAC domain while
keeping hash-chain and checkpoint verification intact.

## Telemetry

The unprivileged Agent streams read-only node observations to the Controller.
Every 30 seconds it emits a bounded telemetry batch over a dedicated
unidirectional Iroh stream. The Web UI queries the persisted data through the
HTTP API.

A node appears offline once its most recent heartbeat is older than 90 seconds
or if it has never reported one. The API computes freshness (`fresh`, `stale`,
`never`) at read time. Background scheduler maintenance persists the offline
state transition separately.

### Data classes and limits

- Telemetry separates security observations, current health snapshots,
  aggregate metrics, and raw history into distinct priority tiers.
- The local Agent buffer is capped at 64 MiB, and buffered telemetry is
  eligible for offline recovery for at most five minutes. When the buffer fills,
  the Agent evicts the oldest raw history before dropping aggregate, health, or
  security records, and reports the resulting drop counts.
- Each wire batch is bounded to 512 KiB. High-cardinality session, username,
  and client IP attributes belong only to the node session read model and are
  never attached as metric labels.
- In PostgreSQL, raw telemetry samples land in monthly table partitions.
- Background scheduler maintenance computes 5-minute and 1-hour rollups and
  enforces the 14-day, 90-day, and 13-month retention windows idempotently.
  Each rollup pass recomputes the full 14-day lateness window starting from the
  resolution's complete UTC bucket. Each retention pass deletes up to 1,000
  expired rows per rollup table and retires at most one expired month.
  Retention cutoffs are evaluated against server time, cannot advance into the
  future, and always aggregate the oldest retirement candidate before its raw
  partition is retired.
- On ingress, the Controller accepts snapshot, metric, and
  security-observation timestamps ranging from 14 days in the past up to five
  minutes in the future. It rejects out-of-window events before selecting a
  partition.

### Transport ingestion recovery

Before advancing its durable event cursor, the Controller classifies any
authenticated transport ingestion failure. Transient database or transaction
errors leave the cursor at its previous position and trigger bounded reconnect
backoff.

When an event carries a permanently invalid business payload, the Controller
rolls back the payload write, records bounded quarantine metadata without the
raw payload, and advances the durable cursor within the same transaction. It
raises a high-severity security alert for each quarantined event. This keeps a
malformed payload from one node from stalling ingestion for the rest of the
fleet.

Clients read current fleet and node state from `GET /api/v1/nodes`,
`GET /api/v1/nodes/{node_id}`, and the node `sessions` resource.

Historical series are queried from the node `telemetry` resource with a
required metric name, a resolution of `raw`, `5m` (default), or `1h`, and an
optional RFC 3339 `since` timestamp (defaulting to 24 hours ago). The resource
returns up to 2,000 data points, ordered from oldest to newest starting at
`since`.

The SSE stream is only an invalidation signal, so clients fetch authoritative
state via REST after connecting or reconnecting. A reconnecting client may
supply its last seen event ID as a cursor. The request returns `400`
(`invalid-cursor`) if that event is no longer visible within the caller's
authorized scope.

### Telemetry upgrade and recovery

Schema setup and upgrades must run through PostgreSQL's owner-only
initialization path. A Controller binary rollback ([Rollback](../operations/production-deployment.md#rollback))
provides no cross-version schema compatibility (see
[Database and recovery matrix](#database-and-recovery-matrix)).

During [recovery](../operations/incident-recovery.md#database-recovery), stop
affected writers and preserve telemetry history, quarantine records, ingestion
cursors, and node trust state. Resolve the underlying fault before resuming
ingestion, and never wipe stored evidence to make an older binary start.

## Users, groups and policy

### Desired and observed state

VPN users and groups are scoped to an individual node. The control plane persists desired state and accepts mutations as asynchronous operations. It tracks Agent observations separately. API clients should render the returned convergence status and never treat `202 Accepted` as proof that the change has already taken effect on the node.

Every mutation requires an `Idempotency-Key`, the expected desired version in `If-Match` or the request body, and a reason string. `revision-0` is valid only when creating a brand-new user or group.

A new mutation of the same kind can supersede an earlier command for the same resource only while that earlier command is still `queued` and holds no node command lease. A group apply can replace a queued group apply, a password rotation a queued password rotation, a disable a queued disable, and an enable a queued enable. Create never supersedes.

The API rejects the request with `409 desired-revision-pending` if a different mutation kind is queued, or if the current revision is already dispatched, accepted, running, unknown, or superseded. Once a revision ends in a failed, expired, rolled-back, or safely rejected state, only a replacement of the same kind is permitted. Submitting a different kind returns `409 desired-revision-recovery-required`. The service never merges distinct mutation kinds, because a later command does not carry the earlier command's password ciphertext or lock intent.

Password endpoints accept only a versioned `SealedSecretV1` envelope carrying the `user_password` purpose, the enrolled user-password key ID, and an RSA-OAEP-SHA256 ciphertext; plaintext passwords and legacy untyped ciphertext fields are rejected. PKCS#12 export passwords use a separate enrolled key pair and the `certificate_p12_password` purpose, so ciphertexts cannot be replayed across the two workflows. On the node, privd keeps both private keys root-only and verifies the signed, typed command envelope before choosing the decryption key.

To apply a password change, the root adapter decrypts the ciphertext with fixed `openssl` arguments and updates a same-directory staging copy of `/etc/ocserv/ocpasswd` using the fixed `ocpasswd` binary. User creation requires the target username to be absent from the authoritative file. Password rotation requires the record to exist already. The adapter preserves existing group memberships and lock markers, then fsyncs and atomically renames the staging file into place.

Each child process runs under a five-second timeout within an overall 20-second RPC budget. Cancellation-safe cleanup deletes the invocation's uniquely named staging file, and privd purges only recognized stale staging filenames at startup before accepting RPCs. Any conflict, timeout, cancellation, or child-process error leaves `/etc/ocserv/ocpasswd` untouched. Password material or hashes never appear in API responses, telemetry observations, audit events, logs, or traces.

Under plain authentication, `/etc/ocserv/ocpasswd` stores group memberships in the second colon-delimited field of each user line. Applying a group updates that field across every affected user record while preserving password hashes and lock markers, and commits through an fsynced same-directory atomic rename. The Agent derives observed groups directly from `/etc/ocserv/ocpasswd` without maintaining a separate sidecar file. Usernames, group names, and member identifiers must match the ASCII regular expression `^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`. This prevents callers from injecting file paths or executable arguments.

Per-node capacity is capped at 384 managed users, 384 managed groups, 384 authoritative users, 384 authoritative groups, 384 members per group, and 384 total non-sentinel user-to-group memberships. During convergence, telemetry may report up to 768 groups when empty managed-group tombstones coexist with a disjoint set of authoritative groups. The state API can return up to 1,536 unioned desired and observed records when managed and authoritative names are completely disjoint. These bounds keep a full authoritative snapshot, its tombstones, and a maximum-size group replacement inside the fixed 64 KiB local RPC frame and 512 KiB telemetry batch.

When the Controller has a fresh, complete node snapshot, the API counts the union of desired and observed users and rejects any user creation that would push the node past 384 users. The API similarly rejects oversized group replacements or new managed groups above the limit before creating an operation. An older Controller that dispatches an oversized payload receives a terminal payload rejection instead of leaving the command in an Unknown state.

On the node, the root adapter enforces the bound independently. It refuses to create a user against a full `/etc/ocserv/ocpasswd` before decrypting the password or running `ocpasswd`, and it reparses every staged file before recording recovery evidence or renaming the file over the authoritative path.

Nodes whose existing `/etc/ocserv/ocpasswd` exceeds the snapshot bound must trim their local accounts before enrolling. The Agent refuses to send partial snapshots that could make omitted users appear deleted.

Automatic drift repair is disabled. Privd instead maintains a bounded, root-only desired-effect store at `/var/lib/ocservia-privd/desired-effects.sqlite3`, authenticated with a dedicated mode `0600` HMAC key and a verified store identity. The database retains the latest record for each mutation kind and target resource, up to 65,536 active rows. Each row binds the command ID, idempotency key, canonical semantic payload hash, revision, expiration timestamp, and authenticated before and after snapshots of the affected `/etc/ocserv/ocpasswd` records, without storing cleartext passwords or password hashes. Applied rows are garbage-collected only after their command TTL can no longer pass Agent expiry validation, while unresolved prepared evidence is kept indefinitely.

Privd commits a `prepared` record before the atomic rename and promotes it to `applied` immediately afterward. Before running any subsequent `/etc/ocserv/ocpasswd` mutation, privd reconciles every authenticated prepared record against the current file on disk:

- If the file matches the recorded after-state, the record is promoted to applied.
- If the file still matches the before-state, unrelated mutations are blocked.
- Any other file state fails closed.

Recovery queries return `APPLIED_EXACT`, `SUPERSEDED_BY_NEWER_REVISION`, `ABSENT`, or `UNKNOWN`. Returning `ABSENT` requires an intact, authenticated prepared record whose before-state still matches the live file. A missing row, older snapshot, deleted SQLite file, missing store identity, or modified file always resolves to `UNKNOWN` and never authorizes a retry. Once a newer revision of the same mutation kind is accepted, older payloads become permanently ineligible for retry.

Always back up and restore `/var/lib/ocservia-privd/desired-effects.sqlite3` and its HMAC key together. Losing either one leaves in-flight Unknown operations requiring manual operator reconciliation, and automatic replay is never permitted.

The Agent atomically records each reconciled command result in its own SQLite journal, alongside the last applied desired revision for the affected user or group. It then includes that revision in its hash-free telemetry snapshots. Resource convergence requires both that reported applied revision and the public state fingerprint to match desired state. A node that is offline with queued commands keeps its resources in `offline_pending`, including password rotations whose public fingerprint does not change on apply. Operators should inspect the linked Operation before deciding whether to submit a replacement revision.

### Quota and expiry

Traffic quotas are expressed in integer bytes up to JavaScript's maximum safe integer (`9007199254740991`). Each policy tracks receive, transmit, or combined bytes over either a UTC calendar month (resetting at `00:00:00Z` on the first of the month) or the account's lifetime; `none` policies always carry a zero byte limit. Account expiration must be an RFC 3339 whole-second UTC timestamp ending in `Z` (zero nanoseconds; timestamps with fractional seconds are rejected without automatic truncation). This way operators and background schedulers evaluate the exact same cutoff.

As session telemetry arrives, the Controller converts monotonic per-session byte counters into durable monthly and lifetime usage totals. Duplicate or replayed telemetry batches add zero bytes, and observations older than the stored session cursor are dropped. When a newer observation reports a lower byte count than the cursor (for example after a session or counter reset), the ingestion path starts a new counter epoch and adds the new reading directly instead of guessing what happened in the gap.

Policy enforcement runs in the scheduler under a database-backed leadership lease and a reentrant scan loop, so restarting the scheduler resumes the scan with deterministic idempotency keys. When an account crosses its quota or expiration timestamp, the scheduler enqueues a standard typed `user_disable` desired-state operation and leaves node command serialization to the worker. Uncertain command outcomes follow the normal Unknown reconciliation path.

At the start of a new UTC calendar month, the scheduler re-enables a user through a deterministic enable operation only if all of the following hold:

- The user's policy is monthly and unexpired.
- An earlier month's quota enforcement under the same policy version disabled the account, and the user is still at that disabled revision or that enforcement operation is still pending.
- Current-month usage is below the limit.

Accounts that have expired or were disabled by an administrator or another policy revision stay disabled. Every scheduler pass emits a `user_operations.scheduler.run` trace span and structured completion log entry (and `alert_kind=user_operations.scheduler_failed` on failure), coordinated with [Controller assembly and roles](../architecture.md#controller-assembly-and-roles) and [`scheduler.go`](../../control-plane/internal/platform/app/scheduler.go).

### User and group recovery

Before intervening manually, stop the scheduler and pause affected API writes, then reconcile any active node commands. Rolling back user or group state is always a forward desired-state mutation. Submit the prior group membership list, or issue an explicit enable or disable against the current resource revision (enable and disable toggle only the lock marker in `/etc/ocserv/ocpasswd` without touching the password hash or group column). Keep all desired and observed state rows, Agent journals, privd desired-effect records, and policy, usage, and enforcement history intact. Recover from incompatible database state as described in [Database and recovery matrix](#database-and-recovery-matrix).

## Current application acceptance

Candidate builds are gated by [Release Check](../development/release-checks.md), which covers native packages, business workflows, integration suites, security scans, and single-instance resilience checks. The project does not maintain a cross-version native upgrade or migration matrix; target-version regression fixtures test the upgrade mechanism itself without promising compatibility with historical releases.

Nodes that advertise `ocserv.config.complete.plan` and `ocserv.config.complete.apply` follow the [complete node-local TLS profile](#node-local-tls-provisioning). `Apply` performs a daemon reload only, so startup-only settings (authentication mode, listeners, worker privilege settings, socket paths, and TLS file paths) must match the active protected configuration. Acceptance requires end-to-end VPN client authentication, verified rollback, and durable state recovery beyond simple config-parser or `occtl` status checks.

## Identity, authorization and audit

Production API access uses Local single-admin authentication (`OCSERV_LOCAL_AUTH_ENABLED=true`). Every production configuration requires a 32-byte session encryption key, a 32-byte audit checkpoint key, and an independent 32-byte audit event authentication key paired with a stable key ID. Browsers receive only Secure, HttpOnly, SameSite session cookies, and credentials are never written to browser storage.

See [production authentication](../operations/authentication.md#configuration-and-secrets) for deployment examples, secret mounts, the authentication and audit environment variables, first-admin bootstrap, and login workflows. Symmetric keys are encoded as 64 lowercase hexadecimal characters. The audit event key file must be a process-owned, single-link regular file with mode `0400` or `0600` beneath root-owned or process-owned non-writable parent directories. Production startup aborts if Local authentication is disabled or if any required session or audit key is missing. Raw audit event key environment values are rejected in production and permitted only inside the repository's development and test fixtures. Development authentication is described under [Development authentication](#development-authentication).

Authorization evaluates a principal's workspace administrator membership in `workspace_admins` (and, when enabled, Break-glass emergency credentials). Collection endpoints read `X-Workspace-ID`; if the header is omitted, the server selects the caller's single authorized workspace and rejects the request when zero or multiple workspaces match. Object-addressed routes resolve the target object's owning workspace directly from storage and return `404` when the workspace is outside the caller's `workspace_admins` set so tampering with an ID in the URL never confirms that foreign resources exist. Hiding buttons in the browser UI never substitutes for authorization; the backend checks are the sole enforcement boundary.

One workspace administrator in `workspace_admins` can independently perform
every administrative operation within that workspace. There is no multi-role
RBAC matrix, mandatory second human identity, approval request, or
`X-Approval-ID`. Route-level authorization and transaction-time checks of the
active identity, session, and workspace admin membership remain separate
boundaries. Configuration revisions, exact command hashes, idempotency, node
ownership fences, and root-signed receipts still protect execution.

Audit intent records commit in the same transaction as the business write, and terminal Agent command results append a separate follow-up audit event. Every newly inserted audit row authenticates its canonical event hash with a domain-separated application HMAC, while signed checkpoints use a separate key. Audit identities, original hashes and MACs, and checkpoints are immutable, and ordinary UPDATE, DELETE, and TRUNCATE statements are denied at the database level. If an audit insert fails, the enclosing business transaction rolls back.

A bounded retention job can clear aged reason strings and before/after detail summaries through a narrow database routine after verifying each row's original hash and MAC. Compaction re-authenticates the retained columns and original proof under the dedicated `ocservia/audit-detail-compaction/v1` MAC domain without altering hash-chain links or checkpoint digests. The audit verification endpoint validates both original and compaction MACs alongside the hash chain and latest checkpoint, reporting `compacted_events` so operators can distinguish verified compacted records from full-detail rows. Legacy unsigned rows and their transition anchor are never compacted. Chain hashing uses the persisted microsecond timestamp, and JSON summaries are parsed into their logical representation before canonicalization so writers and readers produce identical digests regardless of whitespace around a top-level JSON `null`. Historical v1 float64 numeric rounding stays intact instead of being re-signed. Appending an audit event acquires the per-workspace transaction lock, while verification reads run at repeatable-read isolation.

### Local authentication core

The internal `auth.Service` backs Local single-admin authentication (`auth.Config.LocalEnabled`), provisions credentials with `CreateLocalCredential`, and verifies passwords through `AuthenticateLocal`.

Password hashes live only in `local_credentials`, keyed by `identity_id`. Local identities set their issuer to `local` and their subject to the normalized username (trimmed, lowercased, up to 128 bytes, starting with an ASCII letter or digit and containing only ASCII letters, digits, `.`, `_`, and `-`). Every password write passes through `ValidateNewPassword` inside `hashPassword`, covering first-admin bootstrap and self-service password changes. Historical 1..1024-byte credentials continue to verify as stored; see the [Local password policy](../operations/authentication.md#local-new-password-policy).

Credential hashing uses `golang.org/x/crypto/argon2` Argon2id v19 with an independent 16-byte random salt, a 32-byte tag, and self-contained PHC parameter strings. New hashes are written at 19 MiB memory, two passes, and one lane in line with the [OWASP recommendation](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html#argon2id). Verification honors the stored parameters within hard resource caps (at most 64 MiB, five passes, and four lanes). Login attempts for nonexistent usernames run a dummy Argon2id verification at the current write cost to equalize timing. Before inserting a session, Local login re-locks the credential row and rechecks the account's disabled status. Session cookies, AEAD encryption, request authentication, logout, auditing, and break-glass behavior share the same session validation pipeline.

Local login depends on the release's complete backend schema, including `local_auth_attempts` and its expiry index. The runtime database role needs only SELECT/INSERT/UPDATE/DELETE on that table, never Owner or TRUNCATE privileges. Before reading a credential or running the KDF, `AuthenticateLocal` acquires a database-backed single-flight lease in a short transaction, then runs Argon2id verification after committing so no database connection or row lock is held during hashing. Only an observed wrong password records a failed attempt. When authentication succeeds, the session-creation transaction clears the matching lease; password changes invalidate attempt state inside their own transaction so an in-flight failure from an older credential epoch cannot tarnish the new credential. Self-service password changes use a separate server-built `password-change:<identity UUID>` attempt key that no login username can match, so anonymous login failures never block a signed-in admin's change and wrong current passwords back off without locking login. See [Local account failure backoff](../operations/authentication.md#local-account-failure-backoff-r2) for backoff parameters, expiry rules, table capacity, HTTP responses, and upgrade requirements.

#### Attempt completion and expiry

Database errors and malformed password hashes do not count as bad password attempts, and submitting the right password for a disabled account fails login without incrementing the failure counter. Once verification observes an incorrect password, failure recording runs under a detached, bounded context so client request cancellation cannot skip the write. If the caller cancels before verification starts, the reservation is released without recording a failure. A process crash, ambiguous commit, or failed completion leaves the row leased for at most the lease TTL before admission recovers, never silently turning an interrupted check into a password failure. Holders of an expired lease cannot clear newer attempt state or open a session.

Attempt expiration uses the database wall clock. Before allocating capacity for a new login attempt, admission deletes expired rows via the expiry index once their observation window, cooldown period, and active lease have all elapsed. When login traffic is idle, expired rows may sit on disk until the next admission pass, but total table size stays bounded. Brute-forcing random usernames cannot evict active lockout state: once the table reaches capacity, untracked usernames receive a generic 503 response while tracked accounts keep their backoff state. Any database error during cleanup, admission, or completion also fails closed with a generic 503 instead of allowing unbounded password checks. PostgreSQL relies on autovacuum to reclaim dead tuples from these deletes.

### Local initialization and lifecycle

Before bootstrapping the single Local administrator, run database migrations with `--migrate-only`, provision the management workspace with `--provision-management-workspace`, and pass its explicit UUIDv7 to `--bootstrap-local-admin` (or let `quick-install.sh` run the same sequence). Follow the [operator bootstrap procedure](../operations/authentication.md#bootstrap-the-first-local-admin) for secret file mounts and one-shot CLI invocations.

The bootstrap secret reader strips at most one trailing LF (and preceding CR),
opens the descriptor nonblocking and requires a process-owned regular file with
private permissions, no final symlink and no hard links. The password must
satisfy the shared policy; there are no default credentials, plaintext password
environment values or password CLI flags.

The explicit bootstrap command creates one Local identity, its Argon2id
credential, a `workspace_admins` row for the target workspace, initialization
marker (`local_auth_bootstrap`), and audit event in one transaction, then exits
without starting listeners or workers. Missing workspaces or failed writes roll
everything back. Concurrent or repeated bootstrap fails closed. Normal startup
never reads bootstrap passwords or reconciles credentials from disk. Preserve
existing markers, identities, `workspace_admins`, and audit records during
upgrades; do not reopen bootstrap to recover an account.

Self-service password rotation via `POST /api/v1/auth/change-password` requires a valid, non-Break-glass Local session for the caller's own identity and an exact trusted `Origin` header; strict JSON decoding rejects any request body that includes a target identity or workspace field. Failed current-password checks share the login backoff tracker, and new passwords must pass the standard password policy. Inside the transaction, the service locks the identity row, rechecks disabled status, session validity, and the verified credential hash (the credential epoch), fences the attempt lease, writes the new password hash, revokes all sessions for that identity, and appends the audit event so the caller must log in again. Break-glass and development principals cannot hold Local credentials. There are no `/local-users` or `/role-bindings` management routes, and self-registration and MFA are unsupported. If the administrator loses access, follow [Lost Local administration authority](../operations/incident-recovery.md#lost-local-administration-authority); bootstrap can never be reopened.

Local identity management follows the OWASP guidelines for [least privilege and deny-by-default authorization](https://cheatsheetseries.owasp.org/cheatsheets/Authorization_Cheat_Sheet.html) and [current-password verification for password changes](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html).

### Break-glass

Local authentication does not replace the independent offline break-glass mechanism, which keeps its existing credential rotation, critical alerting, audit logging, and short session lifetime rules.

A break-glass session satisfies route-level workspace authorization checks so an operator can perform emergency recovery. Its short lifetime, single-use credential and audit requirements also apply to direct management writes.

The break-glass secret and its digest are configured as described in [Break-glass and deployment checks](../operations/authentication.md#break-glass-and-deployment-checks). The digest is unsalted, so the secret's entropy is the only defence against offline guessing if the digest leaks. Each break-glass session expires after 15 minutes, emits a critical alert and workspace audit event on login, and consumes the configured token digest so the credential cannot be used again until the operator rotates the offline secret and updates the digest.

### Single Admin authorization boundary

There is a single workspace-scoped Admin role (`workspace_admins`), with no
multi-role RBAC matrix or approval-queue indirection. Hiding a UI action never
authorizes a write, and new operations do not create synthetic self-approved
records.

The security model separates human authorization from machine attestation:
an authenticated Admin authorizes management or issues a bounded installation
token; machines then prove their endpoint and root key possession. Automatic
activation must not let the Agent process register a self-selected privd key.

Official guidance informing this boundary:

| Official source | Applicable control |
| --- | --- |
| [Kubernetes TLS bootstrap](https://kubernetes.io/docs/reference/setup-tools/kubeadm/implementation-details/) and [join discovery](https://kubernetes.io/docs/reference/setup-tools/kubeadm/kubeadm-join/) | Bounded bootstrap credentials can authorize automatic node admission once discovery and identity anchors are verified. |
| [SPIRE join-token attestation](https://github.com/spiffe/spire/blob/main/doc/plugin_server_nodeattestor_jointoken.md) and [SPIRE101](https://github.com/spiffe/spire/blob/main/doc/SPIRE101.md) | Server-issued one-time tokens, a short TTL (SPIRE defaults to 600 seconds), and a bound node identity can replace a manual admission decision. |
| [OWASP authorization](https://cheatsheetseries.owasp.org/cheatsheets/Authorization_Cheat_Sheet.html) and [secrets management](https://cheatsheetseries.owasp.org/cheatsheets/Secrets_Management_Cheat_Sheet.html) | Deny by default, check each resource, restrict secret visibility, expire credentials, and audit without recording secrets. |
| [OWASP transaction authorization](https://cheatsheetseries.owasp.org/cheatsheets/Transaction_Authorization_Cheat_Sheet.html) | Atomically consume single-use credentials and preserve exact operation intent and replay protection. |

These references support the selected controls; they do not certify ocservia or
require a second human identity for every administrative operation.

### Web sessions and workspaces

These rules bind the Web console to the backend contracts above. Handwritten modules under `web/src/api` wrap the generated `@ocservia/api-client` package and stay free of UI state: they never instantiate authentication stores, choose a Workspace, trigger router navigation, or classify terminal operation states.

Session handling:

- `api/transport.ts` configures the authenticated fetch wrapper with the `/api/v1` base URL, `same-origin` credentials, the development-only bearer header, optional request signal propagation, and idempotency key generation. When a call returns 401, the transport notifies the session coordinator (`shared/session.ts`) and returns the original response to the caller without retrying. The coordinator owns the unauthenticated redirect guard and the saved post-login return path.
- Post-login return paths are validated as internal console routes and consumed at most once. Both the remembered Workspace ID and the login return path are stored in `sessionStorage` on a best-effort basis: if storage is unavailable or throws, the console proceeds without a saved preference and never blocks login, Workspace selection, or change notifications.
- `api/platform.ts` provides an independent, in-flight-only authentication probe. Probing `/workspaces` through it checks session validity without mutating the cached Workspace list or current selection.
- Certificate artifact downloads authenticate with their one-time grant token and the same-origin session cookie, without a Workspace header. SSE stream URLs never carry bearer tokens in query parameters.

Workspace selection:

- `api/workspace.ts` is the single source of truth for the active and authorized Workspace list, coalesced in-flight discovery, and the Workspace generation counter. Selecting a Workspace accepts only authorized IDs and increments the generation counter on every change.
- Asynchronous callers fence their responses against both the active Workspace ID and the generation counter, so a switch away from and back to the same Workspace still invalidates earlier in-flight reads. Running the independent authentication probe never alters that Workspace state.

Unconfirmed operations:

- The client never adds automatic mutation retries and never resends a mutation whose response was lost. This covers rollout creation and resume and every node workflow request.
- Closing a workflow dialog aborts its in-flight reads and polling loops and resets its transient form state, but never aborts an already-sent mutation request. If an accepted operation or plan ID arrives after the dialog closes, the shared receipt and ticket helpers in `features/node-workflow.ts` record it, so reopening the workflow waits for the in-flight request to settle and then reloads authorized server state. Accepted server operations stay tracked in Fleet after a feature composable is disposed.
- ConfigPlan workflows capture only a known, nonnegative JavaScript-safe configuration revision from the node detail response. They never substitute node version and never rebase or automatically retry a stale revision.
- For a standalone node operation, an `unknown` state renders as a warning while Fleet continues polling for convergence. For an Agent upgrade or rollout node item, `unknown` is treated as a terminal failure.
- `effective_actions` on node detail responses is advisory UI guidance. Every write endpoint enforces its own server-side authorization and domain validation on submission.

Certificate and P12 receipt limits:

- One-time certificate download grants stay in memory only, scoped to the active Workspace, node, and certificate ID and bounded by their expiration timestamp. Grant tokens are never written to persistent storage, so a full page reload, tab close, or second browser tab cannot recover the secret. A download triggered just before closing the dialog still runs to completion.
- Stored certificate operation receipts are restored only when the receipt's resource ID matches the currently selected certificate. Switching to another certificate or viewing a node with no active certificate neither fetches nor displays the older receipt's operation.
- Plaintext user passwords never enter receipts, URLs, logs, analytics, or browser storage (see [Sealing contract](#sealing-contract)).

## HTTP contracts

[`openapi/openapi.yaml`](../../openapi/openapi.yaml) lists the endpoints, fields, and schemas. This section holds the rules OpenAPI cannot express. Splitting handlers across packages organizes the codebase without replacing runtime authorization checks or breaking up domain transactions.

### Request pipeline

Requests pass through the middleware chain in fixed order: `otelhttp -> requestContext -> limitBody -> timeout -> trackRequests -> routeErrors -> ServeMux -> route wrapper -> handler`. Every `/api/` response defaults to `Cache-Control: no-store`; a handler that sets its own policy overrides it, as the event streams do with `no-cache`. Standard HTTP endpoints run under `http.TimeoutHandler`. SSE streams bypass the request timeout while still remaining tracked for graceful teardown. On shutdown, the server closes SSE admission and hubs, initiates HTTP server shutdown, closes active connections once the deadline expires, and waits for in-flight inner handlers to drain. The HTTP layer never takes ownership of closing the shared database pool or domain services. Request contexts propagate caller cancellation and deadlines all the way into domain services.

### Methods and dispatch

Production method metadata comes directly from ordinary `HandleFunc` registrations. Duplicate method declarations, unsupported or overlapping route patterns, and mux conflicts panic at startup. Once construction finishes, the routing rules are read-only and isolated per Server.

Route patterns must declare an explicit HTTP method, a hostless path, literal path segments, and full-segment `{name}` wildcards. The registrar rejects subtree patterns, remainder wildcards, `{$}`, partially embedded parameters, percent-escaped literals, dot segments, and structurally equivalent routes that differ only by parameter name. Routes sharing an identical path shape may register different HTTP methods while keeping separate authorization guards per method.

Derived route lookup runs ahead of `compatibilityRouteMethod`, including method denials. The compatibility table retains action-dependent paths for session, IP-ban, user, certificate, and SecretRef routes, plus the operation-detail prefix rule, but it cannot override method denials derived for `summary` or `queue-metrics`.

Static routes match the full path string directly. Parameterized routes use Trim/Split matching over `URL.Path` without extra URL decoding or path normalization, passing the original request and parameter names straight through to ServeMux. Registering GET never implicitly enables HEAD. Unknown paths normally return a Problem 404, though certain trailing-slash URLs hit plain-text 404 and doubled leading slashes can return a 307 redirect. Any routing change must preserve authentication and error ordering along with those path-handling differences. `control-plane/internal/api/routes_baseline_test.go` verifies the live route inventory, wrappers, and allowed methods directly; historical route counts are not part of the contract.

### Authorization and error mapping

Every route behind `requireOperationAuth` authenticates the caller, enforces browser mutation protections (`validateBrowserMutation`), resolves the target resource and owning workspace (or checks the selected workspace via `selectWorkspace`), verifies `workspace_admins` membership through `AuthorizeWorkspace`, and injects trusted request context. Handlers never trust a client-supplied workspace header on its own to establish authority, and domain services keep transaction-time identity and workspace checks intact.

Strict JSON decoding enforces media-type, UTF-8, unknown-field rejection, rejection of exactly repeated object member names, single-value bodies, and the shared request body size limit; a body over that limit returns `413 request-too-large`. Route authorization returns 403 for workspace-scoped denials and 404 for a missing resource. A resource in a workspace where the caller holds no `workspace_admins` entry also returns 404, so the status never confirms that it exists. An explicitly selected workspace the caller cannot access returns 403 whether or not it exists. A workspace or resource lookup that fails returns `503 database-unavailable` rather than a denial.

### ConfigPlan requests

Both Create and Apply call `requireIdempotencyKey` to trim the header and return the standard missing-key Problem when the header is blank.

Validation order differs slightly by action. Create checks service availability before parsing the node ID. Apply validates the plan ID, trimmed idempotency key, JSON body before invoking the domain service. If the backing service is disabled, the request fails at that existing validation step after passing outer authentication, Origin verification, and resource authorization. Create responds with a Plan, and Apply responds with an Operation along with its Location and replay headers. A 202 response confirms that the Controller committed the intent to its database; Agent execution on the node remains asynchronous.

When a candidate configuration references secrets, `allowConfigPlanSecret` resolves each secret's workspace and caller `workspace_admins` membership (allowing development principals matching the optional selected workspace). Plans without secret references do not require the Certificates service to be enabled. Referencing a secret when the Certificates dependency is unconfigured returns 503, and any secret lookup, workspace mismatch, or authorization failure returns 403 before Create runs. Inside the domain transaction, the service still revalidates stored configuration state, expected version, and node ownership.

A node's `config_revision` is read from `node_config_state.revision`, which tracks configuration state independently of `nodes.version` and `desired_revision`. If no row exists yet, the revision is reported as zero. Database read errors fail the request. The OpenAPI schema leaves the field optional so clients can tolerate older servers.

Replaying an Apply request for the same Plan and workspace idempotency key reuses the already-committed desired revision and the original command's `expected_version`. A new idempotency key evaluates against live node state. Full intent comparison, revision locking, and resource authorization all occur inside the domain transaction.

### User policy

User policy reads and writes (`GET` and `PUT /api/v1/nodes/{node_id}/users/{username}/policy`) run behind `requireOperationAuth`, resolving the node's workspace and returning `404` when missing or unauthorized. Writes validate the idempotency key before decoding JSON, require RFC3339 timestamps with trailing `Z` and whole-second precision, preserve `expected_version` from the request body, trim the reason string, and return `200` with `ETag` revision and replay headers. Idempotent replays read and return the current policy record. Unconfigured service capabilities fail closed after the outer authentication and route guard.

### Upgrade requests

The Operations service owns trusted upgrade target preparation. Request syntax validation precedes node lookup, architecture resolution and release catalog lookup. Unknown architecture fails closed. Single-node upgrades may queue work for an offline node, while rollout admission requires readiness.

Idempotent replay still prepares the target and checks current authorization. If the node has already reached the target, it returns `target-not-newer`. Version checks, node capabilities, current session authority and active-upgrade guards remain inside the domain transaction.

### Event streams

SSE requires explicit, valid configuration at startup; invalid or incomplete settings fail server construction and close any already-allocated resources. All limits and intervals come from validated `OCSERV_SSE_*` settings, and invalid values abort startup. The hubs do not support runtime reconfiguration or per-request hub construction.

Platform and operation streams share a single admission manager. Safe production defaults cap concurrency at 128 streams globally, 8 per identity, 4 per session, 32 per workspace, and 16 per resource, backed by at most 64 active database watchers and a 128-event queue per subscriber. Admission runs before SSE headers are written: exceeding a per-scope limit returns 429, exhausting global stream or watcher capacity returns 503, and both responses include `Retry-After`. The manager deletes empty scope counters so the admission maps stay bounded by the global stream count.

Individual subscribers never run their own database polling tickers. Instead, a single reference-counted watcher per active workspace or operation polls the durable event tables, starting when the first subscriber connects, and fans events out through bounded subscriber queues, so a burst of 1, 10, or 100 subscribers still generates only one steady-state query stream for that scope. On database errors, watchers back off exponentially with jitter up to 10 seconds. If a subscriber's queue fills up, the server disconnects that client so it can reconnect from its cursor without stalling peers. Watchers shut down once their last subscriber leaves, and server shutdown cancels all active watchers. Active streams reauthenticate and reauthorize every 30 seconds and close at session expiry or the 30-minute hard limit, whichever comes first; operation streams drain their terminal event before closing. SSE clients resume with `Last-Event-ID`, and the REST collection remains the authoritative source for full state rebuilds.

Runtime metrics track active and unhealthy watcher counts, query volume, and backoff duration across the bounded series `sse_active_streams`, `sse_rejected_streams`, `sse_watchers`, `sse_unhealthy_watchers`, `sse_sql_queries`, `sse_slow_consumer_disconnects`, and `sse_database_backoff_seconds`. High-cardinality identifiers such as identity, session, workspace, and operation IDs are never used as metric keys or labels. Readiness fails while any active watcher is recovering from a durable event-table query error and returns to healthy once cursor catch-up succeeds.

### Development authentication

Development authentication is for local development only and is off by default. Setting `OCSERV_DEV_AUTH` authenticates every request as the development principal and is accepted only when `environment=development` and the HTTP listener is bound to loopback. Non-loopback development stacks must configure an explicit bearer credential in `OCSERV_DEV_AUTH_TOKEN` (at least 32 characters, sent in `Authorization: Bearer`, accepted only when `environment=development`) and pass the same value to the browser development server as `VITE_DEV_AUTH_TOKEN`. Production rejects both modes. `OCSERV_ENVIRONMENT` defaults to `development` only while the HTTP listener is loopback. The Controller refuses to start on a non-loopback listener unless the variable is set explicitly, so a reachable Controller never runs as development by omission.

The development principal has no persisted identity, session, or `workspace_admins` row, so workspace/resource checks are bypassed for it (though `/api/v1/events/stream` still requires a valid UUIDv7 `workspace_id`). `/api/v1/auth/change-password` rejects it outright. It cannot execute identity-bound administrative mutations, which require a persisted active identity and session; test those flows with Local administrator authentication. The simulation and development runtime routes also require the development principal and a loopback, `localhost`, or `web` Host header, so a rebound browser origin cannot reach them.

## Database

The Controller is a modular Go binary backed by a single PostgreSQL 18.x relational database (deployment: `bundled` or `external`). Unsupported or retired `OCSERV_DATABASE_BACKEND` values (including `mysql` and `mariadb`) fail startup immediately without falling back to PostgreSQL, and unverified cross-engine automatic migration is unsupported. For production database admission and deployment-profile protection rules, consult [Database support](../operations/production-deployment.md#database-support).

Database migrations run in a separate one-shot process invoked with `--migrate-only`. The process uses an owner connection in `OCSERV_DATABASE_URL` and names the unprivileged runtime account in `OCSERV_RUNTIME_DATABASE_ROLE`. The long-running control plane receives only the runtime role credentials. During the one-shot run, the migrator applies schema changes, verifies audit event authentication, and only then grants ordinary data access to the runtime role, so a failed migration never reaches the grant step. Migration execution serializes schema changes and validates previously applied content. At runtime, the service has read and append access to audit events, plus authenticated compaction of aged detail through dedicated database routines. PostgreSQL grants `SELECT`, `INSERT`, and a column-level `UPDATE(event_hash)` needed by locking reads, while immutable triggers block any actual row modification.

PostgreSQL coordinates migrations with an advisory lock and transactional SQL execution. Before touching the schema, the migrator rejects unknown epochs, revisions, checksums, schema drift, and nonempty databases that lack supported provenance. The Controller's migration command never performs automatic force-cleans or downgrades.

PostgreSQL maintains two active SQL files: [`schema.sql`](../../control-plane/migrations/schema.sql) and [`upgrade.sql`](../../control-plane/migrations/upgrade.sql) in `control-plane/migrations/`. The `schema.sql` header carries the current epoch and revision, so read them there instead of from this document. Migration state is tracked only in `schema_revisions`, with no numbered SQL or JSON migration files and no snapshot descriptors. Schema identity is determined by the epoch, revision, and raw artifact checksums, independent of legacy filename counters or Controller compatibility ranges. The [version policy](../operations/production-deployment.md#version-policy) removed software-version admission gates, but schema-source admission remains mandatory in the migration run and continues to reject unknown epochs, revisions, and checksums.

Migration admission, meaning the full SQL checksum and receipt verification, runs only in the owner `--migrate-only` entry point. Normal Controller startup without `--migrate-only` verifies audit authenticity and checks required telemetry objects and runtime privileges. It does not check receipts, checksums, or schema drift. The `/readyz` endpoint executes a zero-row read against core tables (workspaces, nodes, operations) and checks event-stream health, without reading `schema_revisions`. A passing `/readyz` response therefore indicates runtime connectivity and is not evidence of schema identity.

### SQL artifact format 1

The parser recognizes whole-line `-- ocservia:` markers at column zero. Files
use LF endings and end with LF. It does not split on SQL semicolons or interpret
SQL syntax. SQL outside a step, malformed/reserved markers, duplicate headers,
unknown fields, and unmatched boundaries are errors.

A schema artifact starts with these headers (each exactly once; an upgrade
artifact omits `revision`, see below):

```sql
-- ocservia:artifact=schema
-- ocservia:format=1
-- ocservia:engine=postgresql
-- ocservia:epoch=1
-- ocservia:revision=0

-- ocservia:step=001:journal
CREATE TABLE schema_revisions (...);
-- ocservia:end-step
```

The supported engine is `postgresql`. Epochs are positive decimal integers.
Schema artifacts declare their current checkpoint revision and have at least
one step. Revision zero is the epoch baseline. Step ordinals run
from `001` through `999` without gaps; names are unique within a revision and
match `[a-z][a-z0-9_]{0,63}`.

Upgrade artifacts omit the revision header. Each revision uses
`-- ocservia:revision=N` and `-- ocservia:end-revision`, with at least one step.
Revisions start at one and are contiguous in file order. An upgrade artifact
may have no revisions when the baseline already represents the current schema.

Executors require a `-- ocservia:baseline={...}` upgrade header containing the
original epoch-baseline schema checksum and step count. Backend verification
metadata can be included in that receipt. For example:

```text
{"checksum":"<64 lowercase hex characters>","steps":1,"metadata":{"catalog_sha256":"<64 lowercase hex characters>"}}
```

A revision may immediately start with `-- ocservia:checkpoint={...}` using the
same receipt format. This pins a fresh schema snapshot at that revision. JSON
receipt keys use the canonical order `checksum`, `steps`, optional `metadata`;
unknown or duplicate keys, noncanonical encoding, and invalid hashes/counts
are rejected. A fresh install stamps one schema checkpoint at its declared
revision, without inventing executed rows for the revisions it covers. Later
revisions are appended with their SQL block checksums. The two receipt forms
are accepted only at their explicitly pinned journal positions.

For a schema checkpoint above revision zero, a `history-sha256` header binds
the covered SQL revisions. Its input is the engine, epoch and baseline checksum
separated by tabs and terminated by LF, followed by one `revision<TAB>checksum<LF>`
record per covered revision. This detects mutations even when a fresh database
has never executed those SQL revisions.

A previous-epoch transition requires all three checkpoint headers:

```sql
-- ocservia:previous-checkpoint-epoch=1
-- ocservia:previous-checkpoint-revision=0
-- ocservia:previous-checkpoint-ref=v1.2.0
```

The previous epoch must be the current epoch minus one. Before ordinary
revisions, exactly one `-- ocservia:transition=1:0->2:0` block must match the
declared checkpoint and end with `-- ocservia:end-transition`. The reference
must be fixed by the release process; parsing it does not establish trust in
that release or a database.

Executors also require `previous-checkpoint-receipt` to pin the exact
journal checksum, step count, and necessary backend schema verification metadata
at that checkpoint. A syntactically valid release reference is insufficient.

One `-- ocservia:metadata={...}` line may immediately follow a step marker.
Its JSON object carries backend-specific verification metadata. The shared
parser validates the JSON boundary; the backend owns its fields, object
identities, fingerprints, and postcondition semantics. This permits SQL routine
bodies with internal semicolons while keeping executable SQL separate from
verification metadata.

Artifact and schema-baseline SHA-256 cover every file byte. Revision and
transition SHA-256 cover bytes after the opening boundary through the byte
before the closing boundary, including step markers and metadata. A revision's
optional checkpoint receipt is excluded from that SQL block hash to avoid a
circular dependency with its covered schema checksum; it remains covered by
the full artifact checksum. A previous-epoch transition keeps its raw SQL
block checksum and step count in the new epoch revision-zero row; its retained
previous receipt distinguishes it from a fresh schema checkpoint. Step SHA-256
covers the raw SQL byte range after its marker/metadata through the byte before
`end-step`. No whitespace or line ending normalization occurs. Database
admission, journal validation, and transactional execution remain the
responsibility of the PostgreSQL migration runner.

### Upgrade checkpoint

`schema.sql` runs only against an empty database; initialization never overwrites, adopts, or cleans an existing database. Nonempty PostgreSQL databases must match either the current epoch's known receipt history or the single supported prior checkpoint: `v1.2.0@169102557cd610847c9f6ac2083336cdcf82c483`, epoch 1 / revision 0. That immutable release is the upgrade anchor as main advances. Databases older than v1.2.0 must upgrade with v1.2.0 first, because a major candidate cannot replay removed historical transitions. The checkpoint is a database contract and is unrelated to the product's maintenance status. See [Database checkpoint release boundary](../development/release-checks.md#database-checkpoint-release-boundary) for the release rules.

| Engine | v1.2.0 checkpoint schema receipt |
| --- | --- |
| PostgreSQL 18.x | `d837335f22c70858f4e2a5277332e478ecd6032e7b55f57d6512484bfab6f172` |

Fresh installations and checkpoint-upgraded databases must converge on the same business schema, mandatory seeds, and runtime ACLs, even though their execution receipts differ by path. A fresh PostgreSQL install writes a single schema receipt at the declared revision, derived from `schema.sql`. A checkpoint upgrade verifies the epoch-1 receipt, deletes it within the transition transaction, and records the epoch-2 baseline and each subsequent forward revision. A forward revision that recreates routines such as `audit_compact_detail`, `security_compact_details` or the `audit_events_reject_update` trigger drops their runtime `EXECUTE` grants, so the owner migration's runtime re-authorization must follow every upgrade. Never rewrite receipt history to force those tables to look identical.

PostgreSQL runs initial installation or the checkpoint transition inside a single transaction, and commits each subsequent forward revision in its own transaction. During the checkpoint transition, PostgreSQL verifies the existing schema against the pinned checkpoint fingerprint, validates the target schema, writes the verified epoch baseline, runs the legacy metadata cleanup from `upgrade.sql`, and validates the final state before committing. The cleanup drops `schema_migrations` and `schema_snapshot_origin` and removes the epoch-1 receipt, while leaving `controller_schema_compatibility` untouched and unread by non-test Go code. Any failure rolls the entire transaction back.

When adding changes within the current epoch, append forward revisions to `upgrade.sql` without altering already published block bytes, and update `schema.sql` to produce the equivalent fresh schema along with its bound history checksum. Repeated owner maintenance runs may reapply grants and provision calendar-dependent telemetry objects, but they never replay fresh SQL. Verify both the fresh/current revision paths and the fixed release transition with:

```bash
bash scripts/database-artifact-policy.sh all
bash scripts/database-postgres-snapshot.sh check
```

These commands verify the artifacts without modifying tracked files. PostgreSQL's author command can regenerate current SQL from the fixed checkpoint transition, and `check` compares the resulting schema, ACLs, and mandatory seeds between a fresh initialization and an upgrade from the immutable checkpoint, pulling historical fixtures from that Git release tag instead of an archived migration directory. Installation timestamps are generated at runtime, while deterministic seed values must match byte for byte. See [SQL artifact acceptance](../development/release-checks.md#sql-artifact-acceptance) for required failure and backup/restore test coverage.

### Transactions and stores

Business stores coordinate transaction ownership, atomic audit and domain writes, bounded cleanup, and fencing through `internal/database`. PostgreSQL implements business locks with transaction advisory locks. A commit acknowledgement error leaves the transaction outcome unknown, so callers must not blindly replay writes.

Within a single business operation, bind every participating domain Store to the same `database.Tx` inside `database.Within`. Never construct a store from the connection pool inside that callback or split a logical transaction across multiple callbacks. `database.Within` does not retry. For database-only claim, completion, reaping, and lease work, use the opt-in `database.WithinRetry` helper, which makes at most three attempts and retries only when the transaction was a deadlock victim or hit a serialization failure with an acknowledged rollback. Connection loss, lock timeouts, ordinary constraint errors, and unknown commit outcomes return immediately without retry, and callbacks passed to `database.WithinRetry` must never perform transport calls or other external mutations. Store methods translate driver errors into backend-neutral categories such as `database.ErrDeadlock` and `database.ErrSerialization`, keeping driver types and SQLSTATE codes out of business packages.

Lease and fence checks read a fresh database wall-clock timestamp after acquiring their locks so time spent waiting on a lock cannot revive an expired term; transaction-stable timestamps remain available for logical ordering. Authority takeovers increment the retained epoch in place instead of deleting and recreating records, so stale holders continue to fail their exact owner, incarnation, connection, and epoch predicates.

UUIDs and logical timestamps cross the store boundary as typed Go values and are stored in native PostgreSQL `uuid` and `timestamptz` columns. Always check the owning store and appended migration instead of guessing a column's representation from its name. For a concrete workspace preparation example, see [database preparation](../operations/authentication.md#database-preparation).

### Database and recovery matrix

Authoritative database requirements live in [Database support](../operations/production-deployment.md#database-support). All rows below are validated with `linux/amd64` and `linux/arm64` Controller artifacts, and each acceptance record captures the exact server, client, and backup image digests used:

| Version and deployment | Existing verification entry | Recovery and uncovered scope |
| --- | --- | --- |
| PostgreSQL 18.x bundled | `PG_MAJOR=18 DATABASE_TEST_SCOPE=smoke scripts/database-integration.sh`; `scripts/postgres-backup-restore-smoke.sh` | Base backup, isolated restore verification, and single-instance restart recovery. Does not cover clustered HA, automatic failover, or PITR. |
| PostgreSQL 18.x external | Same backend checks plus `scripts/external-postgres-backup-restore-smoke.sh` | TLS-verified owner/runtime/backup connections and isolated base restore. Does not manage external HA or PITR. |
| Retired backends (PostgreSQL 17, MySQL, MariaDB) | Rejected fail-closed by startup, Compose, and lifecycle entrypoints | Unsupported; no silent backend switching, data purge, or cross-engine automatic migration. |

Adopting a new database patch release requires reviewing migrations, role permissions, SQL artifacts, and backup/restore behavior before updating the pinned digests, with runtime evidence captured in Business Smoke. Never wipe node SQLite journals to work around a database or package upgrade check.

Additive schema changes do not by themselves guarantee cross-version compatibility, and operators must never edit published migration SQL or discard data to force a binary to start. Likewise, migration receipts are not backups, and an untested backup file is not a verified restore. The repository provides no down-migration SQL, so rolling back the Controller binary never reverses schema changes or guarantees that an older binary can read current database state. Recover through a forward fix or a planned isolated restore by following the [PostgreSQL recovery](../operations/incident-recovery.md#database-recovery) procedure (verified base backup with isolated restore), which does not claim HA or PITR equivalence, and review [schema provenance after restore](../operations/incident-recovery.md#schema-provenance-after-restore).

## Certificates and Signer

### Certificate lifecycle and CSR attestation

Managed nodes generate certificate private keys locally and never expose them to the control plane. The Controller receives a signed CSR and public-key digest. It forwards the CSR to a configured external PKI signer after validating the Admin-authorized, content-bound operation.

The Controller stores neither a CA private key nor any node private key, and a CSR self-signature alone does not prove privileged execution on the node. A request therefore enters `csr_ready` only after the Controller verifies a root privd receipt that binds the certificate ID, CSR and public-key digests, requested-subject digest, node, command, operation, idempotency key, and root effect record. Immediately before calling the external signer, the Controller locks the certificate row and rechecks the exact CSR digest, receipt-bound request and version, node, persisted authorization binding, and non-revoked key state. P12 export and certificate-key revocation enforce the same terminal-result attestation rule.

Managed-node certificates and secret sealing are distinct from the Controller's own HTTPS listener certificate (configured via manual files or ACME in Integrated mode), which the Signer neither issues nor serves.

The upstream service must treat signing and revocation as idempotent by certificate ID and support node-targeted secret sealing. The TLS CA bundle authenticates the HTTPS connection only and confers no authority to sign client certificates. See [Signer and certificate materials](../operations/production-deployment.md#signer-and-certificate-materials) for the client variables, file paths, permissions, and runtime settings.

When the signer is unreachable, certificate issuance transitions to `signer_unavailable` (which can be resumed by retrying the same persisted operation) and revocation transitions to `revocation_unknown` (which a retry may submit again). Both paths return a service-unavailable problem response without marking the operation successful.

Thirty days before `not_after`, a certificate enters `expiring` and triggers a high-severity alert. Revocation calls the external signer idempotently first and, once that succeeds, releases the node operation to remove only the UUID-derived node-local key. In the control plane, secret provider records store only the provider name, opaque key path, version, and lifecycle state while the secret values themselves stay in the external provider. Rotating a secret records the new external version and appends an authenticated audit event without copying the secret value into the Controller.

### P12 export and single-use download

When exporting a P12 bundle, the Controller generates a fresh random password and an independent random artifact token, returning both only in the initial HTTP response and never persisting them in the Controller database. The password is sealed to the node's dedicated P12-password public key, which cannot be decrypted by the separate user-password key. On the node, privd decrypts the typed secret locally, writes an encrypted UUID-addressed artifact into its fixed root-owned spool, and records the certificate and version, operation, digest, byte size, expiry, and state in its authenticated effect store. The root mapping enters `prepared` before any staging file is created and advances to `available` only after the final artifact is published, so certificate revocation can clean up both interrupted staging files and completed artifacts.

Downloading an exported P12 bundle requires standard node authorization, the separate `X-Artifact-Token` header, and a short-lived Controller-signed `ArtifactGrantV1` bound to the node, artifact, certificate and version, operation, requester, purpose, maximum size, and unique grant ID. Both the Agent and privd verify the grant independently. Only one grant may lease an artifact at a time, and an interrupted lease remains locked until its bounded expiry elapses. The root ledger also tracks the exact chunk offset so a second stream cannot replay the same grant from offset zero.

Streaming the bytes does not itself consume the artifact. Once the Control Plane receives the full stream and verifies its size and digest, it sends a separate finalize request carrying the same signed grant to the Agent and privd, which durably marks the grant consumed and deletes the local P12 file. Consumed grants cannot be used to fetch the artifact again. An exact finalize retry may still acknowledge the already-consumed root record without reopening the file or repeating deletion, so a dropped network response never strands the Controller lease.

Revoking a certificate invalidates all outstanding grants and deletes any mapped P12 and staging files. Time-based cleanup remains solely as crash recovery for expired or orphaned files.

### Signer custody and idempotency

The production Signer is a standalone Go module and process operating within the [Integrated custody boundary](../architecture.md#certificates-secrets-and-signer) for node certificates and secret sealing; it does not sign Controller commands, and the Python business signer in the repository exists only as a test fixture.

Key custody and permissions are partitioned across four roles:

- The offline root CA private key is never mounted into the Signer container; Quick install exports it in encrypted form for operator custody.
- Signer holds only the online issuing intermediate key, its own HTTPS private key, and the bbolt ledger containing issued certificates, node sealing-key bindings, and the audit log. It never receives Controller database credentials.
- The Controller holds neither a CA private key nor any node key, authenticating to Signer through `certificates.HTTPSigner` using a shared Bearer token and the public HTTPS CA bundle.
- Each managed node keeps its certificate private key and its two sealing private keys (`user_password` and `certificate_p12_password`) restricted to root, exporting only public key material so Signer can seal secrets to those imported public keys.

On the write path, the Controller validates operation authorization and the privd receipt before sending an authenticated HTTPS `POST /sign` or `/sign/revoke` request, and Signer commits the change to its bbolt ledger before replying. Recovery across the two services is coupled: restoring the Signer ledger without the matching issuer CA, or without reconciling Controller certificate state and all subsequent issuances, revocations, key imports, and key disables, leaves the deployment inconsistent (see [Signer backup and restore](../operations/incident-recovery.md#signer-backup-and-restore)). A committed revocation updates the ledger and subsequent CRL exports, while active VPN sessions remain connected until enforced by the verifier.

Both writes are idempotent by certificate ID. Repeating `/sign` with the same CSR returns the original issuance, and a different CSR returns `409`. Repeating `/sign/revoke` with the same serial and reason returns `204` without changing the original revocation time, a different serial or reason returns `409`, and an unknown ID returns `404`. A `204` therefore does not say which request performed the revocation; the ledger's revocation state and the CRL are authoritative.

The server exposes four POST endpoints: `/sign`, `/sign/revoke`, `/sign/seal`, and `/sign/public-key`. JSON request bodies are capped at 64 KiB, while raw sealing payloads are bounded to 512 bytes at ingress and further constrained by the RSA-OAEP payload limit (190 bytes for RSA-2048). Concurrency is limited to 32 in-flight business requests, with additional requests rejected as `503`. Header, read, write, and idle timeouts are fixed at 5, 10, 15, and 30 seconds, and error responses return fixed status messages without echoing request content. `GET /healthz` returns `204` while the ledger is readable, the issuer certificate has sufficient remaining lifetime, and no storage error has latched readiness off, without performing any signing operation. Recovery from a latched storage fault is in [Signer unavailable](../operations/incident-recovery.md#signer-unavailable).

Normal `serve` startup refuses to run if the ledger is absent, empty, incompatible, corrupt, owned by another user, or bound to a different issuer, and never initializes a replacement automatically, so `init` is not a way to repair lost state. At startup, the service verifies that the intermediate-and-root chain matches the intermediate private key, carries CA certificate-signing and CRL-signing key usages, and has at least 24h of remaining validity; it never generates CA, node, or TLS keys of its own.

### Policy and durable effects

The `rsa-client-v1-24h` signing policy accepts CSRs using RSA-2048, RSA-3072, or RSA-4096 keys with public exponent 65537, signed with SHA-256, SHA-384, or SHA-512 RSA or RSA-PSS, containing one CN and up to 32 DNS SANs. Both CN and DNS SAN values must be bounded printable ASCII strings free of whitespace and path separators; any non-DNS SAN or other X.509 extension is rejected. Issued certificates carry only digital-signature, key-encipherment, and `clientAuth` usages without CA privileges, are valid for 24h within the issuer's validity window, and are verified against the CA chain before being persisted. Subject authorization, operation binding, and privd receipt validation remain Controller responsibilities, as a CSR self-signature never establishes authorization.

Each certificate's unsigned 128-bit UUID is used directly as its positive X.509 serial number, so a single certificate ID can never receive multiple serials. Before returning a response, a single-writer bbolt transaction binds the certificate UUID, exact CSR bytes and digest, issued certificate chain, serial number, and audit record. If the process crashes before that transaction commits, no certificate is exposed externally. If a response is lost after commit, replaying the identical CSR returns the stored certificate chain, while submitting a different CSR for the same ID conflicts even after revocation.

Revocation commits the timestamp and exact reason string and rejects any later attempt that alters the serial or reason. Replaying an earlier sign request never clears a revocation or reissues the certificate.

Running `crl` reads durable revocations from the ledger and writes a signed PEM CRL to stdout with a one-hour `nextUpdate` window and the current audit revision as the CRL number. The operator steps are in [CRL refresh](../operations/incident-recovery.md#crl-refresh). The X.509 CRL reason code extension is omitted because the ledger stores free-form reason text. Receiving a `204` from `/sign/revoke` confirms only that the ledger recorded the revocation. It does not clean up node files, terminate active VPN sessions, or distribute the updated CRL, and the system exposes no public CRL endpoint or OCSP responder.

The ledger uses state format version 1 backed by bbolt (version pinned in [`signer/go.mod`](../../signer/go.mod)) with an exclusive process lock, transaction-consistent snapshots, and default synchronous commits enabled. The underlying local filesystem must honor `fsync`; network filesystems, multi-node replication, and high-availability clustering are unsupported. Four buckets organize state: `meta` (schema version, issuer SHA-256, policy name, and record checksums), `issued` (certificate records keyed by UUID), `bindings` (node key mappings), and `audit` (monotonic revision log). On startup, Signer runs bbolt's consistency check and validates all record checksums, certificate signatures, CSR bindings, and imported key descriptors. These checksums detect accidental storage corruption; they do not defend against deliberate rewriting by an authorized owner of the state volume. Never edit or downgrade the ledger file by hand. Backup, restore floors, and the audit high-water mark are in [Signer backup and restore](../operations/incident-recovery.md#signer-backup-and-restore).

### Trusted public-key transfer

Signer provides neither automatic synchronization nor in-place key rotation, and exposes no HTTP endpoint for key registration. Only a protected operator can import or disable node bindings. The steps are in [Trusted public-key transfer](../operations/production-deployment.md#trusted-public-key-transfer). The transfer enforces these rules:

- `/usr/local/bin/ocserv-sealing-export` is an OS-protected administrative CLI that authenticates directly to the database without exposing a public API. The database credential file must be a single-link regular file (mode `0400` or `0600`, 1 to 4096 bytes, owned by root or the process) at an absolute canonical path without symlinks or group- or world-writable ancestors, and is read from the verified file descriptor without a second path lookup. Inside a repeatable-read transaction using the domain stores, the command locks the node row, verifies that the node is active or offline with an active endpoint, and verifies the workspace and endpoint binding before exporting the persisted key descriptors. The OS-protected database credential remains the authority boundary. It rejects pending or revoked nodes and mismatched identities.
- The JSON carries no self-authenticating signature, so security depends on file provenance and the trusted operator transfer channel.
- The Controller export must be under 15 minutes old. `import` verifies that both files agree on identities, purposes, versions, key IDs, and DER hashes across two distinct RSA keys, then atomically commits the provenance record and audit event before you restart Signer. Repeating an identical import is a no-op. Attempting to replace an existing binding fails.
- Disabled bindings remain permanently recorded in the ledger and cannot be reactivated or overwritten, so a newly enrolled node identity requires a fresh verified import. Revoking a node in the Controller does not automatically disable Signer's offline key mapping, so always complete both Controller retirement and the Signer disable step.

### Sealing contract

During normal operation, Signer's HTTP sealing handler uses only mappings created by `import`, encrypting passwords with RSA-OAEP-SHA256 and MGF1-SHA256 (empty label, standard base64 output) without persisting or logging plaintext passwords. Responses echo `version: 1`, the exact purpose, and the key ID; missing or disabled bindings return `403`, and the two sealing purposes may never share a key or descriptor.

Requests to `/sign/seal` pass `X-Ocservia-Node-ID` and an exact `X-Ocservia-Seal-Purpose` header (`user_password` or `certificate_p12_password`). The response must echo `version: 1`, the requested purpose, the enrolled purpose-specific `key_id`, and the base64 ciphertext; the Controller rejects any response with a missing, substituted, or unregistered binding.

The authenticated `POST /sign/public-key` endpoint accepts requests containing only `node_id` and `purpose=user_password`. It reads the active durable binding created by `import` and returns the workspace, node, endpoint, version, key ID, SHA-256 fingerprint, and standard base64-encoded RSA SPKI DER under the same Bearer authentication, concurrency limits, and JSON size bounds as other Signer endpoints. Missing or disabled bindings fail even across restarts, no private key material is ever exposed, and the 15-minute freshness window enforced during `import` does not act as a runtime key expiration.

On the Controller, `GET /api/v1/nodes/{node_id}/user-password-sealing-key` requires an authorized workspace Admin session and queries Signer through the Controller's configured HTTPS Signer client and dedicated CA pool. Before returning the key with `Cache-Control: no-store`, the enrollment service verifies the Signer response against the current node, workspace, and endpoint state, checks that `ocserv.users.write` is active for the node, confirms that the purpose, version, key ID, and fingerprint match the enrolled record, and validates the SPKI digest, RSA modulus size, and public exponent. State or descriptor mismatches return `409`, while an unreachable Signer returns `503`. Rotating a sealing key requires completing the trusted provisioning flow; this read endpoint never imports or updates bindings.

In the Web UI, the Create user and Rotate password dialogs accept a plaintext password in a secure browser context and use WebCrypto to encrypt its UTF-8 bytes with RSA-OAEP-SHA256 (MGF1-SHA256, empty label) into a version-1 `user_password` envelope, so only ciphertext is sent to the mutation API. The browser clears the form input before starting asynchronous requests and zeros temporary byte buffers upon completion, failure, or stale cancellation. Closing the dialog, switching nodes or workspaces, or unmounting the component aborts any in-flight key lookup. Plaintext passwords never enter execution receipts, Web workflow references, URLs, logs, analytics, or browser storage, and a failed submission requires the operator to type the password again. All existing mutation authorization, desired-revision, signed-command, and privd checks still apply on the backend.

## Schema sources and generation

[`openapi/openapi.yaml`](../../openapi/openapi.yaml) and the Protobuf definitions under [`proto/`](../../proto/) are the normative sources for HTTP endpoints, messages, and field tags. The schemas under `proto/` are the source of truth for the Go/`transportd` gRPC services (`TransportService`, `TrustService`) and the Agent protocol messages they carry. Running `make generate` (`scripts/generate.sh`, [`proto/buf.gen.yaml`](../../proto/buf.gen.yaml)) regenerates `control-plane/gen/proto`, `rust/crates/contracts/src/generated`, and `web/src/api/generated`; never edit files under those generated directories by hand. The local `Agent`/`privd` IPC protocol is not defined in `proto/`. It is maintained as handwritten Rust types in `rust/crates/agent-protocol`.

Internal source renames do not rewrite external representations: `local_slice_jobs`,
`local-simulator`, historical `i14-` idempotency keys, signed canonical inputs,
journals and executed SQL receipts retain their bytes. Browser workflow
references retain the `ocservia.node-workflow.` storage prefix, kind and ID
fields; they locate server state and are never execution receipts or authority.

Buf lints with `STANDARD` (one documented exception in [`proto/buf.yaml`](../../proto/buf.yaml)) and checks compatibility with the `FILE` policy. Removed published field numbers and names must be reserved, field numbers must never be reused, and every enum starts with an `UNSPECIFIED` zero value.

Toolchain setup and the choice of checks are in [Choose the validation scope](../development/release-checks.md#choose-the-validation-scope). Contract breaking checks (`scripts/check-breaking.sh [base-ref]`, honoring the explicit security-migration allowlist) and generated-code cleanliness checks (`scripts/generated-clean.sh`) run in Basic CI when contract inputs or outputs change and on manual checks. CI supplies the PR merge-base or push before SHA; manual workflows require a non-self `contract_base_ref`. Standalone breaking checks default to `origin/main`. Regeneration requires a clean committed candidate and compares both staged and unstaged outputs with HEAD. For CI job routing, see [Path routing](../development/release-checks.md#path-routing). Passing a schema diff check does not replace release-level behavioral testing.

### oneOf object-map conversion

`openapi/templates/typescript-fetch/modelOneOf.mustache` is the TypeScript Fetch template from the OpenAPI Generator artifact pinned in [`toolchains.lock`](../../toolchains.lock). The only local changes preserve free-form object-map branches during deserialization and serialization. The upstream template otherwise falls through to `{}`, silently discarding free-form object content. Array/model branches retain their existing behavior. The upstream source is attributed in [THIRD_PARTY_NOTICES](../../THIRD_PARTY_NOTICES.md).

Regenerate with `scripts/generate.sh`. Do not edit generated TypeScript. Generated-client tests cover object content and typed arrays. Recheck this override whenever the generator is upgraded.

## Upstream adapters

Behavior adapted from `mmtaee/ocserv-dashboard` is evaluated per upstream change, with the classification recorded in the machine-readable manifests `docs/upstream/v4.9-post1.manifest.json` and `docs/upstream/v4.9-post2.manifest.json`. Source coordinates and license are in [THIRD_PARTY_NOTICES](../../THIRD_PARTY_NOTICES.md). No upstream source files are copied verbatim into the repository. The boundaries below remain enforced.

- Upstream-facing policy fields stay in `web/src/upstream/`. Node scoping, generated client calls, integer-byte conversion, UTC normalization, operation tracking, and workspace authorization remain in `web/src/adapters/` and the Go control plane. Numeric form binding is enforced at the frontend adapter boundary.
- Quota and account-expiration rules are implemented through integer byte counters, explicit receive/transmit/combined accounting modes, UTC billing periods, desired-policy versus observed-usage separation, a database-leased scheduler, and typed disable commands, without running upstream GORM models or migrations.
- Rejected: direct local `occtl` calls, unmediated password or config file edits, Docker-socket execution, file-based cron journals, untracked background goroutines, permanent deletion of expired user records, wrapping local shell commands in GORM transactions, running synchronous restore calls with HTTP timeout exemptions, and importing legacy SQLite or password files from another product. ocservia does not expose a dashboard backup-upload endpoint.
- In ocservia, `privd` serializes password creation, rotation, enable/disable, and group updates through a single adapter `Arc<Mutex>` that covers reading, staging, and atomic publication. Because that lock is in-process, external tools must not modify the managed `ocpasswd` file while `privd` is running.
- Per-user policy enforcement (quota limits and RFC3339 `Z` expiration checks) is enforced by the scheduler under database leadership locks without external cron jobs (see [Users, groups and policy](#users-groups-and-policy)).

### Adapter safeguards

Password and configuration publication uses unique staging files in the target directory, descriptor-relative `O_NOFOLLOW` access, ownership/mode/hardlink checks, and signed root effect records. Any in-flight staged transition is resolved before the next mutation runs, and command replay relies only on the authenticated `privd` receipt or `Agent` journal (inspecting disk files alone never synthesizes a receipt, so a crash between applying an effect and signing its receipt settles as `Unknown`).

When a child command times out or is canceled, the adapter removes `ocpasswd`'s adjacent `<staging>.tmp` file. At startup, cleanup touches only private, root-owned, single-link UUIDv7 staging files (including their `.tmp` companions) and fails closed if it encounters unsafe legacy leftovers. `privd` runs under `UMask=0077`, while [Ocpasswd 1.5.0](https://gitlab.com/openconnect/ocserv/-/blob/1.5.0/src/ocpasswd/ocpasswd.c) sets `umask(066)` and creates `0600` scratch files before root ownership normalization; the `privd` Unix socket is explicitly set to `0660`, and configuration publication restores the target file's mode. A single execution deadline covers child `stdin`, process exit, and output pipes, backed by process-group termination if grandchild processes outlive the child.

Output parsers follow the tagged ocserv source and binary output where older man-page descriptions differ. Older-release test fixtures cover parser regressions without certifying full package or security acceptance, and a new ocserv point release requires explicit validation before admission. Unreleased upstream path-hardening patches are not imported; ocservia enforces its own path and identifier validation at the Controller and `privd` boundaries.

## CI and rollout decisions

Release qualification follows [Release policy](../development/release-checks.md#release-policy). Removed 1.0 compatibility gates are retired by policy and must not be reported as skipped passes.

Across all rollouts, preserve node trust records, negotiated capabilities, historical authorization evidence, Signer identity and revision counters, local command journals, root receipts, and owner-fence floors. Reconcile any `Unknown` operations before issuing new mutations to the same target. Use only the documented install, upgrade, and rollback entrypoints, without reversing SQL migrations or resetting persistent identities.

Unrun, failed, skipped, and out-of-scope checks are tracked separately. Any commit change invalidates prior CI results for the affected suites.
