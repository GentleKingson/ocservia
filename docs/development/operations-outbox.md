# Operations and transactional outbox

I09 introduces the side-effect-free synthetic command path used to validate
durable asynchronous delivery before real node mutations are enabled.

Clients queue a typed `noop` or `echo` command with
`POST /api/v1/nodes/{node_id}/synthetic-commands`. Every request must include an
`Idempotency-Key` and either an `If-Match: "revision-N"` header or the matching
`expected_version` body field. A successful request returns `202 Accepted`, an
operation resource, and its `Location`. Reusing a key with identical input
returns the original operation; reusing it with different input returns an RFC
9457 conflict.

The operation intent, typed Protobuf command, outbox event, and audit intent are
committed in one database transaction through backend-owned stores. Workers claim available outbox rows
with `FOR UPDATE SKIP LOCKED`, acquire one bounded lease per node, commit the
claim, and only then call transportd. A successful transport acknowledgement is
recorded after the network call. An expired claim is either redelivered or,
after the bounded attempt limit, retained as `unknown`; it is never guessed to
have failed or succeeded.

When a commit acknowledgement is uncertain, services perform a bounded readback
of the original immutable intent, the exact attempt/lease or completed attempt,
or the exact owner term and deadline, and fail closed on missing, expired or
superseded evidence. They do not allocate a replacement intent or invoke
transport to resolve a database error. Repeated successful bookkeeping is
idempotent without requiring an Agent result to have arrived already.

Operation state is available through REST and the resumable
`/api/v1/operations/{operation_id}/events` SSE stream. Queue health is exposed
at `/api/v1/operations/queue-metrics`, including unpublished count, oldest age,
queue depth, and unknown count. Where used, PostgreSQL notifications are wakeups only;
polling remains the recovery mechanism.

Use the [Controller lifecycle](../how-to/controller-rollback.md) and
backend-specific recovery for an installed release. The current tree has no
database down-migration path. Stop affected writers and reconcile active work;
preserve operation and command history for investigation. Prefer a forward fix
to destructive schema removal. A binary rollback does not reverse database
changes or certify cross-version operation.
