# PR-02 Version 3 Storage Adapters

This remains Draft, not complete PR-02 acceptance. Controller startup still
rejects MySQL/MariaDB, including development Controller startup. Domain tests
are not authenticated Controller workflow acceptance.

This document records the version-3 milestone. The appended
[version-4 workflows](database-pr02-controller-workflows.md) supersede the
corresponding remaining-item statuses below without rewriting version 3.

## Append-only history

Version 3 follows the exact version-2 artifact checksum. Both genuine version-1
lineages have independently executed plans for MySQL 8.4.10 and MariaDB 12.3.2.
Published version-1/version-2 artifacts and receipts are not rewritten. The
runner validates the entire ordered history, not merely its highest version.
Version 3 does not claim to execute PostgreSQL's historical migrations.

Every DDL/data step has its own receipt. Object names are separate from step
names, allowing several real steps on one table. Data backfills verify values,
not just DDL. A running revision refuses normal startup; explicit repair names
the latest checksum and does not discard dirty evidence. Migration lock waits
use bounded ten-second server intervals, with a two-minute overall limit and
the caller's cancellation. Unlock still uses independent bounded cleanup and
discards an unconfirmed physical connection.

Before copying data, temporary triggers exclude writers not holding the
migration lock. Relevant FK parents are guarded too, since cascading deletes
do not invoke child triggers. These guards survive process death. Repair checks
the recorded guards before resuming and verifies source/shadow equality again
immediately before consuming a source column. An owner repair must acquire the
same migration lock on its dedicated connection; changing a source does not
authorize adopting stale copied data. Temporary guards are removed only after
the storage changes. All guard DDL is recorded in the manifest.

## Actual adapters

- Identity profile conflict handling retains full issuer/subject values,
  disabled-identity denial and the conflict lock through the caller's session
  transaction. The production OIDC path calls this domain Store.
- Observed group replacement/read preserves array dimensions, lower bounds,
  empty arrays and NULL elements. Security event insertion/read preserves
  JSONB decimal values outside native MySQL JSON's numeric range. Pure SQL
  validators and triggers reject invalid values even when bypassing Go codecs.
- Five observed-state/history time columns use checked signed PostgreSQL-epoch
  microseconds. Backfills preserve year 1000 as finite, without guessing old
  sentinel intent. Other time columns remain a separate acceptance blocker.
- Telemetry sample insertion, history and maintenance borrow the same Tx.
  Monthly ordinary InnoDB tables retain node RESTRICT and batch CASCADE FKs.
  Catalog locks protect table lifetime. Owner provisioning verifies and moves
  that month's legacy rows atomically; rollback retains the source rows.
- Command admission/backlog uses the original global lock scopes and order.
  PostgreSQL retains advisory transaction locks; MySQL/MariaDB use dedicated
  transaction lock records, not GET_LOCK. No automatic transaction retry occurs.

Server errors that roll back the whole MySQL/MariaDB transaction poison its Go
wrapper and close the socket. Exec, unscanned QueryRow errors and unread result
drain errors are covered. Continuing a failed transaction or committing it is
refused rather than allowing later writes to become autocommit operations.

## Owner maintenance

The test/development-only foundation CLI supports:

```sh
ocserv-db-foundation --mode telemetry-provision --month 2026-09
ocserv-db-foundation --mode telemetry-collect
ocserv-db-foundation --mode grant-test-privileges
```

Use the schema owner connection for provisioning/collection, never runtime.
Provisioning is repeatable and repairs a planned month's interrupted DDL;
collection only drops retired, structurally verified tables. Grant the fixed
test accounts again after provisioning new months. No automatic global
legacy backfill or production scheduler is claimed.

The retirement procedure inherits PostgreSQL's cutoff range of 90 days ago
through now. Runtime/maintenance receive EXECUTE, not catalog UPDATE or DDL.
Its definer must be the schema-scoped owner, not a global administrator. Do not
grant runtime CREATE/ALTER ROUTINE, definer-changing administrator privileges
or GRANT OPTION. Exact-key guard UPDATE permission exists only to permit
locking reads; a trigger rejects every actual update. Side tables and migration
metadata remain unwritable by runtime, and audit mutation grants do not change.

## Remaining acceptance

The other JSON/array/time fields and lease sentinel meaning still require
actual adapters and append-only migrations. The full Controller transport,
read-model, upgrade and authentication transactions remain PostgreSQL-owned.
Temporary WrapTx bridges preserve their existing physical transaction; the
driver ratchet records these exact transitional sites, not general permission
for new business-layer SQL. Audit, auth lifecycle/rate limits, RBAC and
certificate advisory-lock workflows remain unported. Complete Controller
workflow parity, automatic legacy telemetry migration and final phase review
are still outstanding. Do not mark ready, deploy, release or merge.
