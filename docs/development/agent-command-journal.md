# Agent command journal

The Agent stores command acceptance and terminal results in its owner-controlled
SQLite database. Each side effect is bound to one idempotency key, one command
ID, and one semantic payload hash. The key and command ID are independently
unique. Only an exact match replays a stored result; either identity being
reused for another command is rejected before execution.

A command is validated and durably accepted before the typed synthetic effect
runs. Synthetic effects, their execution counter, and the terminal result are
committed in one SQLite transaction. The result is therefore durable before a
response frame is sent, and a failed commit cannot leave an effect recorded
without its terminal result.

Incomplete records become `unknown` on ordinary replay. Delivery mode is
explicit: `RECONCILE_ONLY` observes durable state without execution, while
`RETRY_IF_EFFECT_ABSENT` is accepted only after reconciliation persisted proof
that the effect is absent. A matching effect completes reconciliation without
executing again. Mutating execution is serial, and inbound command streams are
bounded to eight per Agent connection. Delivery mode is not part of the
semantic hash because it controls recovery rather than the side effect.

Run the focused fault matrix with:

```bash
./scripts/i10-agent-journal.sh
```

The matrix covers 100 duplicate deliveries, acknowledgement loss, restart,
every persistence and effect crash boundary, key and command identity
conflicts, cross-language semantic hash vectors, explicit safe retry, expiry,
clock skew, revision, capability, cancellation, size limits, and SQLite
read-only, full, and corrupt failures.

Database schema version 7 stores structured Agent command result history in
PostgreSQL. Every `command_result` event must decode and satisfy its state,
identity, hash, size, and time constraints; invalid results roll back the whole
ingestion transaction. Development simulation completion uses the distinct
`simulation_result` event type. Agent timestamps remain history data and never
replace Controller-observed authority timestamps.

Binary rollback must stop new command dispatch and preserve the Agent SQLite
journal. Applying the version 7 down migration deletes result history and
simulation-result events, so use it only after that destructive loss is
accepted. The Agent journal schema is forward compatible and should not be
deleted during rollback.
