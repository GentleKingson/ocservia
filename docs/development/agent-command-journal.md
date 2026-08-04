# Agent command journal

The Agent stores command acceptance and terminal results in its owner-controlled
SQLite database. A command is validated and durably accepted before the typed
synthetic effect runs. The result is persisted before the response frame is
sent. Duplicate delivery with the same idempotency key and semantic payload
returns the stored result; a different payload is rejected.

Incomplete records become `unknown` on replay. The Agent first checks the
durable synthetic effect record. A matching effect completes reconciliation
without executing again. An absent effect remains unknown until the caller
selects the explicit synthetic safe-retry path. Mutating execution is serial,
and inbound command streams are bounded to eight per Agent connection.

Run the focused fault matrix with:

```bash
./scripts/i10-agent-journal.sh
```

The matrix covers 100 duplicate deliveries, acknowledgement loss, restart,
every persistence and effect crash boundary, same-key payload conflicts,
expiry, clock skew, revision, capability, cancellation, size limits, and
SQLite read-only, full, and corrupt failures.

Database schema version 7 stores structured Agent command result history in
PostgreSQL. Binary rollback must stop new command dispatch and preserve the
Agent SQLite journal. Apply the version 7 down migration only after the result
history is no longer needed; the Agent journal schema is forward compatible
and should not be deleted during rollback.
