# Command delivery and recovery

## Controller delivery

Controller-issued commands share one delivery path: Operations
`CreateSynthetic` (despite its name, also used by the configuration,
certificate and Agent-upgrade services) or the user/group service signs and
queues the typed command, the outbox worker dispatches it through transportd,
and the Agent journal and privd record the effect. The side-effect-free
synthetic command route below exercises that path without real node mutations.

Clients queue a typed `noop` or `echo` command with
`POST /api/v1/nodes/{node_id}/synthetic-commands`. Every request must include an
`Idempotency-Key` and either an `If-Match: "revision-N"` header or the matching
`expected_version` body field. A successful request returns `202 Accepted`, an
operation resource, and its `Location`. Reusing a key with identical input
returns the original operation; reusing it with different input returns an RFC
9457 conflict.

The operation intent, typed Protobuf command, outbox event, and audit intent are
committed in one database transaction through backend-owned stores. The signed
envelope's command idempotency key is the operation ID, not the HTTP
`Idempotency-Key`, which only deduplicates the API request. Workers claim
available outbox rows with `FOR UPDATE SKIP LOCKED`, acquire one bounded lease
per node, commit the claim, and only then call transportd.

A transport acknowledgement means only that the frame was written to the Agent
stream; it is recorded after the network call and is neither execution nor a
result. The Agent result returns asynchronously. A send error releases the claim
and retries the same signed envelope after one second; the Agent journal replays
rather than re-executes an exact duplicate. A claim that expires while sending is
`unknown` because the write may or may not have happened. It is never guessed to
have failed or succeeded, is not resent for execution, and receives a
Controller-re-signed `RECONCILE_ONLY` follow-up that only observes the Agent
journal, up to the bounded attempt limit. A reconciliation that proves the
effect absent permits a Controller-signed `RETRY_IF_EFFECT_ABSENT` command.

When a commit acknowledgement is uncertain, services perform a bounded readback
of the original immutable intent, the exact attempt/lease or completed attempt,
or the exact owner term and deadline, and fail closed on missing, expired or
superseded evidence. They do not allocate a replacement intent or invoke
transport to resolve a database error. Repeated successful bookkeeping is
idempotent without requiring an Agent result to have arrived already.

Operation state is available through REST and the resumable
`/api/v1/operations/{operation_id}/events` SSE stream. Queue health is exposed
at `/api/v1/operations/queue-metrics`, including unpublished count, oldest age,
queue depth, and unknown count. The worker polls the outbox every 200 ms and
reaps expired leases every second. The PostgreSQL backend also emits
`pg_notify` hints on enqueue, but nothing listens for them; polling is the only
dispatch and recovery mechanism.

## Agent journal

The Agent stores command acceptance and terminal results in its owner-controlled
SQLite database. Each side effect is bound to one idempotency key, one command
ID, and one semantic payload hash and hash version (the Controller currently
issues v2; the Agent accepts v1 and v2). The key and command ID are independently
unique. Only an exact match replays a stored result; either identity being
reused for another command, or the same command with a different hash version,
is rejected before execution.

A command is validated and durably accepted before its typed effect runs.
Synthetic effects, their execution counter, and the terminal result are
committed in one SQLite transaction. External Ocserv effects transition to
`running` before the privd call and persist the bounded terminal result before
acknowledgement. A failure to persist that result produces `unknown`, never a
guessed result.

Command ingress performs strict raw-wire validation before Protobuf decoding.
Unknown fields and known fields with incompatible wire types are rejected in
the Controller transport path and the Agent command stream, including nested
payload messages. This keeps a schema extension from changing the meaning of a
side-effecting command without an explicit protocol update.

Incomplete records become `unknown` on ordinary replay. Delivery mode is
explicit: `RECONCILE_ONLY` observes durable state without execution, while
`RETRY_IF_EFFECT_ABSENT` is accepted only after reconciliation persisted proof
that the effect is absent. A matching effect completes reconciliation without
executing again. A service reload with an uncertain result requires manual
reconciliation because service status cannot prove that a reload occurred.
Mutating execution is serial, and inbound command streams are bounded to eight
per Agent connection. Delivery mode is not part of the semantic hash because it
controls recovery rather than the side effect.

Desired user, group, and configuration effects have independent durable
resource revision fences. ConfigApply additionally relies on privd's root-owned
prepared/applied/absent effect record. Reconciliation may rebuild a lost Agent
journal only from an exact durable effect identity; a matching current config
hash alone is never evidence that an old command executed.

Run the focused fault matrix with:

```bash
./scripts/i10-agent-journal.sh
```

Basic CI's `rust` job executes these workspace tests,
including the named command-journal and Agent crash cases, so CI does not run
the focused script a second time. The focused command remains useful for local
reproduction.

The matrix covers 100 duplicate deliveries, acknowledgement loss, restart,
process aborts at every persistence and effect crash boundary, key and command identity
conflicts, cross-language semantic hash vectors, explicit safe retry, expiry,
clock skew, revision, capability, cancellation, size limits, and SQLite
read-only, full, and corrupt failures.

The selected Controller backend stores structured command results and their
hash version: legacy (`0`), canonical v1 (`1`) or session-authority v2 (`2`).
Backend migration numbers are not protocol versions.
Every `command_result` event must decode and satisfy its state,
identity, hash-version, hash, size, and time constraints; invalid results roll
back the whole ingestion transaction. Development simulation completion uses
the distinct `simulation_result` event type. Agent timestamps remain history
data and never replace Controller-observed authority timestamps.

## Recovery

For [Controller rollback](../how-to/controller-lifecycle.md#rollback),
[Agent rollback](../how-to/agent-lifecycle.md#rollback) or
[database recovery](../operations/incident-recovery.md#database-recovery), stop
affected writers and dispatch, reconcile active work, and preserve the Agent
SQLite journal and Controller operation, command and result history. There are
no down migrations or cross-version compatibility guarantees. Use a forward fix
or planned isolated restore; never delete the journal or reset replay protection
to make an older binary start.
