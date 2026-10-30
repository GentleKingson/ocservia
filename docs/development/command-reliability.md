# Command delivery and recovery

## Controller delivery

All Controller-issued commands follow the same delivery pipeline. Either
Operations `CreateSynthetic` (which, despite the name, also serves the
configuration, certificate, and Agent-upgrade services) or the user and group
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

The frozen CommandAuthorizationV1 transcript already signs the protocol string,
so this revision adds no wire fields or signing-layout changes. An original
`1.1` `EXECUTE_OR_REPLAY` dispatch can be upgraded during its first recovery
because its deadline has not been renewed. Existing `1.1` recovery envelopes
may have lost that deadline: they remain observation-only, and old signed
`RETRY_IF_EFFECT_ABSENT` grants can only replay a retained root result. Missing
or pruned results remain `unknown` and require operator reconciliation. Deploy
matched Controller/transportd and Agent/privd packages; older nodes may reject
the new command revision until upgraded.

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

## Agent journal

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
resource-revision fences, and `ConfigApply` additionally checks privd's
root-owned prepared, applied, or absent effect record. Reconciliation can
reconstruct a lost Agent journal entry only from an exact durable effect
identity in privd; a matching current configuration file hash by itself never
proves that an earlier command executed.

You can run the focused fault-injection matrix locally with:

```bash
./scripts/i10-agent-journal.sh
```

CI does not invoke the wrapper script separately, because the `rust` job in
Basic CI already runs these workspace tests (including the command-journal and
Agent crash-boundary cases). The test matrix exercises 100 duplicate deliveries,
lost acknowledgements, daemon restarts, process aborts at every persistence and
effect crash point, idempotency-key and command-ID collisions, cross-language
semantic hash vectors, explicit retry-after-absence flows, command expiry,
clock skew, revision and capability mismatches, cancellation, payload size
bounds, and SQLite read-only, disk-full, and corruption faults.

On the Controller side, the configured database backend persists structured
command results alongside their semantic hash version (`0` for legacy, `1` for
canonical v1, or `2` for session-authority v2, which are protocol identifiers
independent of database migration numbers). Every ingested `command_result`
event must decode cleanly and satisfy all state, identity, hash-version,
digest, size, and timestamp checks, or the entire ingestion transaction rolls
back. Development simulation runs use a separate `simulation_result` event
type, and Agent-reported timestamps are stored only as historical metadata
without overwriting Controller-observed authority timestamps.

## Recovery

Before performing a [Controller rollback](../how-to/controller-lifecycle.md#rollback),
[Agent rollback](../how-to/agent-lifecycle.md#rollback), or
[database recovery](../operations/incident-recovery.md#database-recovery), stop
affected writers and command dispatch, reconcile in-flight work, and preserve
both the Agent's SQLite journal and the Controller's operation, command, and
result tables. The system provides no down migrations or cross-version
downgrade compatibility, so recover via a forward fix or a planned isolated
restore, and never delete the local journal or clear replay protection to force
an older binary to start.
