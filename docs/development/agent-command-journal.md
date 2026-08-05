# Agent command journal

The Agent stores command acceptance and terminal results in its owner-controlled
SQLite database. Each side effect is bound to one idempotency key, one command
ID, and one semantic payload hash. The key and command ID are independently
unique. Only an exact match replays a stored result; either identity being
reused for another command is rejected before execution.

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

Run the focused fault matrix with:

```bash
./scripts/i10-agent-journal.sh
```

The matrix covers 100 duplicate deliveries, acknowledgement loss, restart,
process aborts at every persistence and effect crash boundary, key and command identity
conflicts, cross-language semantic hash vectors, explicit safe retry, expiry,
clock skew, revision, capability, cancellation, size limits, and SQLite
read-only, full, and corrupt failures.

Database schema version 9 stores structured Agent command result history in
PostgreSQL. Migration 8 persists the semantic hash algorithm version, and
migration 9 restricts it to the supported legacy (`0`) and canonical v1 (`1`)
values. Every `command_result` event must decode and satisfy its state,
identity, hash-version, hash, size, and time constraints; invalid results roll
back the whole ingestion transaction. Development simulation completion uses
the distinct `simulation_result` event type. Agent timestamps remain history
data and never replace Controller-observed authority timestamps.

Binary rollback must stop new command dispatch and preserve the Agent SQLite
journal. Applying the version 9 down migration removes the supported-version
constraint; applying version 8 down removes the persisted hash-version column;
applying version 7 down deletes result history and simulation-result events.
Use the version 7 rollback only after that destructive loss is accepted. The
Agent journal schema is forward compatible and should not be deleted during
rollback.
