# PR-02 Draft: MySQL/MariaDB foundation

## Status and blockers

This is a **Draft, not complete database support and not ready for phase
acceptance**. Controller startup still rejects both new selectors, including
development startup. The separate `ocserv-db-foundation` command permits only
explicit `test` or `development` environments. No business store was switched
from PostgreSQL. No production migration, release, deployment, or merge is
part of this PR.

The connection, migration/recovery, and fresh-account permission foundation is
executable. The two baseline manifests contain all 69 existing logical tables
plus `business_locks`; their SQL and postcondition fingerprints were exercised
independently on the pinned servers. **That does not establish semantic
equivalence of the entire schema.** Outstanding acceptance blockers are:

1. The draft uses `VARCHAR(191)` for indexed PostgreSQL `text`, including
   unbounded issuer/subject keys and some fields whose existing checks permit
   more than 191 characters. This is a proposed, unapproved restriction, not
   an equivalent mapping. See `database-pr02-bounded-keys.tsv`. No prefix-only
   unique indexes or collision-prone hash substitutions are presented as
   exact uniqueness. These keys need a reviewed contract or an exact alternate
   design before the baseline can be accepted.
2. JSON is native MySQL JSON and MariaDB's validated JSON alias, not PostgreSQL
   jsonb equality/canonicalization. The member-list codec preserves SQL NULL,
   empty arrays and NULL elements, but accepts only one-dimensional arrays.
   SQL currently checks array shape/cardinality, not every element's type.
   Multidimensional arrays/lower bounds and SQL-side NUL rejection in text/JSON
   are not implemented; regexp-engine edge equivalence remains unproven. These must not be silently
   accepted as full PostgreSQL type semantics.
3. `DATETIME(6)` represents finite UTC instants, not PostgreSQL's entire
   timestamp range. `1000-01-01` is the proposed expired-lease sentinel for
   historical `-infinity`; the reserved-range contract has not been approved.
   PostgreSQL transaction-start `now()` and wall-clock functions cannot be
   indiscriminately substituted by MySQL statement time in future stores.
4. Telemetry has an unpartitioned InnoDB logical table and equivalent query
   index in this draft. PostgreSQL partition functions/retention safeguards
   have **not** been ported. Maintenance receives only scoped telemetry
   SELECT/DELETE, not a DDL or SECURITY DEFINER substitute. This is not a claim
   of equivalent partition maintenance.
5. The transaction lock primitive is tested, but actual business call sites
   remain PostgreSQL-owned. The lock-scope/order inventory below is a porting
   obligation, not evidence that MySQL business transactions have been tested.

Do not remove the production gate or mark this PR ready based on green
foundation tests. The complete acceptance request is not yet satisfied.

## Baseline and connection policy

- Base: fetched `origin/main`, `65026afc0beac680983532e1d5a0bea41dfa1a82`,
  containing merged PR-01 (#192); initially clean worktree.
- New branch: `pr-02-mysql-mariadb-foundation`.
- `database/sql` with `github.com/go-sql-driver/mysql v1.9.3`; no ORM, SQL
  string translation at runtime, or replacement of the pgx adapter.
- MySQL **8.4.10** and MariaDB **12.3.2** are separate selectors, manifests,
  checksums and CI jobs. Open rejects a different flavor/version. Docker
  image digests are pinned in `scripts/database-foundation-integration.sh`.
- DSN syntax is the driver's `user:password@tcp(host:port)/database?tls=true`,
  never inferred from a PostgreSQL URL. Only the `tls` query parameter is
  permitted; arbitrary session variables, multi-statements, LOCAL INFILE,
  interpolation and insecure TLS fallback are not configurable through DSN.
- Verified TLS 1.2+ uses the host name and system roots or the explicit
  `OCSERV_DATABASE_TLS_CA_FILE`. `tls=skip-verify` and `tls=preferred` are
  rejected. Explicit `tls=false` is allowed only for literal loopback test
  endpoints, not DNS names or remote addresses.
- Dial timeout: 5s; read/write timeout: 30s; pool maximum: 20, idle maximum: 2;
  lifetime: 1h, idle lifetime: 5m. Callers provide operation deadlines.
- Every new session sets UTC (`time_zone='+00:00'`), READ COMMITTED, strict SQL
  modes, utf8mb4 and the backend's binary NO PAD connection collation. Explicit
  repeatable-read/serializable transactions use `sql.TxOptions`.
- Go time values are truncated to microseconds. MySQL also sets
  `TIME_TRUNCATE_FRACTIONAL`; MariaDB explicitly omits `TIME_ROUND_FRACTIONAL`
  from its fixed SQL mode. Defaults use `CURRENT_TIMESTAMP(6)`.
- The foundation CLI reuses private, launcher-owned regular secret-file
  checks. Inline and `_FILE` settings conflict even when inline is empty.
  Driver logs are disabled; errors retain neutral categories/cancellation,
  not server messages containing values, DSNs or credential paths.

Primary references: [driver configuration](https://github.com/go-sql-driver/mysql/tree/v1.9.3),
[MySQL fractional precision](https://dev.mysql.com/doc/refman/8.4/en/fractional-seconds.html),
[MariaDB SQL modes](https://mariadb.com/docs/server/server-management/variables-and-modes/sql-mode).

## Transaction finalization

Each business transaction owns a dedicated `sql.Conn` and native `sql.Tx`.
Begin remains request-cancellable, but the transaction lifetime is detached
after Begin so request cancellation cannot race `database.Within`'s independent
five-second rollback context with database/sql's automatic rollback.

Commit and Rollback install a context watchdog that closes the exact underlying
network connection if their context expires. It does not wait for the driver's
connection mutex, abandon an in-flight finalizer goroutine, or merely return
the connection to the pool. Finalization joins the watchdog before connection
reuse; failed finalization evicts the connection. An already-cancelled Commit
does not send COMMIT. A timeout after sending COMMIT still has an **unknown
commit outcome**, not a rollback guarantee, and must not be blindly retried.

## Mapping details

| PostgreSQL contract | Draft representation |
| --- | --- |
| UUID | `VARBINARY(16)` plus exact byte-length CHECK; RFC byte order, no time-byte swapping or short-input padding |
| bytea | `LONGBLOB`; indexed binary fields use `VARBINARY(512)` and retain existing length checks |
| jsonb | Backend JSON type, existing object/array CHECKs mapped; semantic limitations above |
| inet | Family byte + prefix-length byte + 4/16 address bytes; host bits and IPv4-mapped IPv6 identity preserved; NULL is separate |
| text[] | JSON member list and explicit `TextArray` codec; limitations above |
| nullable values | SQL NULL, not zero UUID, empty string, empty blob or empty JSON array |
| ordinary uniqueness | Full-column unique keys, normal distinct NULLs; indexed-text limit remains a blocker |
| NULLS NOT DISTINCT | Role binding's nullable resource key gets a tagged generated value, separating NULL from every UUID including zero UUID |
| partial unique indexes | Generated nullable predicate flag appended to the full key; outside the predicate the NULL flag disables uniqueness |
| non-unique partial indexes | Full indexes retaining key order, not a filtered storage promise; query-plan/performance equivalence is unproven |
| case/trailing spaces | MySQL `utf8mb4_0900_bin`, MariaDB `utf8mb4_nopad_bin`; exact case and trailing spaces distinguish keys |
| CHECK/FK | Enforced CHECKs and InnoDB foreign keys with existing delete actions, including composite workspace/node ownership |
| identity sequences | AUTO_INCREMENT for transport ingest and operation event sequence; not PostgreSQL sequence API compatibility |
| audit append-only | Runtime INSERT/SELECT only; row UPDATE/DELETE triggers also reject owner mutations; runtime TRUNCATE/DDL denied |

The draft schema was authored from a real PostgreSQL 17 catalog after applying
the unchanged 1..34 SQL in an isolated database. The catalog was used to
inventory final tables/constraints, **not** inserted as an applied PostgreSQL
history on either new backend. Non-business physical partition children are
not counted as logical tables. Historical SQL bytes and the PostgreSQL runner
remain unchanged.

## Migration and repair

Each backend embeds its own `manifest.json`. Manifest version **1** maps
explicitly to logical Controller schema **34..34**. This is a logical shape
mapping for the isolated schema tool, not permission to run Controller stores.
Every step has its own SHA-256 of SQL and a pinned postcondition fingerprint;
the entire manifest also has a SHA-256, stored in `backend_migrations`.
The manifests are immutable after acceptance. They are not loaded from the
database, and a migration never learns/accepts a changed schema fingerprint.

`backend_migrations` and `backend_migration_steps` are independent of
PostgreSQL `schema_migrations`. The runner:

1. Acquires a dedicated `sql.Conn`, then a database-scoped `GET_LOCK`, requiring
   an explicit non-NULL result of 1. Failed/uncertain acquisition discards the
   physical connection. There is no pool-level GET_LOCK call.
2. Creates and checks the metadata table definitions. Refuses to adopt an
   existing business schema as an empty initialization. Persists dirty state
   before any business DDL.
3. Journals each statement as running, executes one DDL/DML statement, checks
   its postcondition, then marks the step verified. Implicit DDL commit is
   expected and is not described as transactional rollback.
4. Rejects unknown/reordered/checksum-conflicting journal entries and schema
   drift. Default startup/check/migrate rejects dirty/in-progress state.
5. Only after every step is verified, atomically inserts the logical
   compatibility row and advances version/compatibility/clean metadata in an
   InnoDB transaction. No PostgreSQL migration rows are manufactured.
6. Releases the lock with an independent bounded cleanup context and verifies
   RELEASE_LOCK returned 1. Otherwise `Conn.Raw` returns `driver.ErrBadConn`,
   which removes/closes the actual driver connection; `Conn.Close` alone is
   not used as the eviction mechanism. Unlock uncertainty is returned as an
   error even if all schema work succeeded.

For an interrupted initialization, stop competing migration processes and
inspect the journal and failing object's actual definition. Run
`--mode=manifest-checksum` to identify the reviewed embedded artifact, then
`--mode=repair --repair-checksum=<reviewed SHA-256>`. Repair records a repair
count/time and resumes only if the interrupted object is absent or already
matches its exact postcondition. Other partial/foreign objects require manual
owner investigation. There is no force-clean flag, checksum rewriting,
down-migration, automatic retry, audit deletion or fencing-epoch reset.

## Accounts and lock scopes

The administrator creates three distinct accounts. Owner has schema-scoped
DDL/DML and GRANT OPTION, **not** global SUPER or account-management privileges.
The test server sets `log_bin_trust_function_creators=1` so this scoped owner can
install audit triggers with binlogging enabled. This is an explicit server
prerequisite, not a grant added to runtime.

`--mode=grant-test-privileges` targets fresh, non-inheriting `ocservia_app` and
`ocservia_maintenance` accounts. It applies an explicit table/column allowlist,
not database-wide runtime grants. Provisioning an existing account with extra
privileges/role inheritance is outside this helper's contract and must be
audited separately; the helper does not silently revoke existing authority.
Runtime cannot alter migration metadata, execute DDL, rewrite audit rows or
change protected bootstrap/event columns. Maintenance can SELECT/DELETE only
the three telemetry data/rollup tables. It cannot repair migrations.

Business locking uses InnoDB `business_locks` rows on the **caller-owned
transaction**. INSERT/duplicate no-op UPDATE and SELECT FOR UPDATE serialize
the key through commit/rollback. No GET_LOCK, secondary transaction, key sorting
or retry is used. The new adapter does not advertise native advisory locks.

| Existing scope | Future row-lock key / ordering obligation |
| --- | --- |
| RBAC and Local lifecycle, 734821032 | `global:734821032`, shared by both modules |
| Local attempt capacity, 734821033 | `global:734821033`, before attempt row cleanup/admission |
| Certificate operation serialization, 6820260817 | `global:6820260817` |
| Command active admission, 0x4f435356434d444c | `global:0x4f435356434d444c`, environment-wide, not per node |
| Backlog admission, 0x4f4353564241434b | `global:0x4f4353564241434b`, before node/workspace counts |
| Audit workspace chain | `audit:<canonical workspace UUID>`, at the current chain-lock point |

These keys must be wired in the domain-store port while retaining existing
surrounding row-lock and audit order. Current PostgreSQL call sites are not
changed by this foundation PR.

## Validation

All execution is via `ssh BuildServer`, isolated checkout
`/root/ocservia-pr02.YX4Vwr`. See `database-pr02-validation.md` for commands,
results and remaining limitations. CI now retains PostgreSQL 17/18 and adds
separate digest-pinned MySQL/MariaDB jobs, with fail-fast disabled.
