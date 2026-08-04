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
committed in one PostgreSQL transaction. Workers claim available outbox rows
with `FOR UPDATE SKIP LOCKED`, acquire one bounded lease per node, commit the
claim, and only then call transportd. A successful transport acknowledgement is
recorded after the network call. An expired claim is either redelivered or,
after the bounded attempt limit, retained as `unknown`; it is never guessed to
have failed or succeeded.

Operation state is available through REST and the resumable
`/api/v1/operations/{operation_id}/events` SSE stream. Queue health is exposed
at `/api/v1/operations/queue-metrics`, including unpublished count, oldest age,
queue depth, and unknown count. PostgreSQL notifications are wakeups only;
polling remains the recovery mechanism.

Rollback disables the worker/API version first, waits for active leases to
expire, and then applies migration `000006_operations_outbox.down.sql`. The down
migration removes I09 command history, so production rollback should normally
be a forward fix while retained operations still require investigation.
