# PR-02 Exact Natural Keys

The candidate appended migration is authored by
`mysql.LongKeyMigrationSteps`. This authoring input is not itself a published
migration or evidence of complete Controller support. The reviewed per-engine
manifest must record its SQL, object fingerprints, and data postconditions.

All eleven previously unapproved VARCHAR(191) columns become LONGTEXT without
changing their logical column names. Reads and ordinary writes use those same
columns. Existing NUL checks remain in force. `commands.resource_key` is not a
unique key: its index retains node, resource type, and creation time, while the
complete resource key remains a query filter.

Nine natural unique constraints use private, per-table exact-key side tables.
Their owner primary keys reference the original row with ON UPDATE CASCADE and
ON DELETE CASCADE. Rollup and enforcement tables gain an internal unsigned
64-bit auto-increment owner key; the original natural key remains logically
unique. Separate node indexes preserve FK support when replacing the original
composite primary keys.

Each complete key is a binary concatenation of every component, prefixed by its
eight-byte byte length. This distinguishes component boundaries, string case,
and trailing spaces. There is no prefix-unique index, hash-unique index, digest
equality shortcut, or collision-dependent exception. The operations constraint
excludes NULL idempotency keys. The receipt constraint applies only to verified
rows with all unique-key components non-NULL, matching the original partial
unique index and NULL-distinct behavior.

AFTER INSERT/UPDATE triggers acquire a transaction-held guard row for that
constraint and perform a current locking read of complete side-table values.
Conflicts raise error 1062. An update replaces its own side row atomically. FK
cascades reclaim side rows even when InnoDB does not execute child-table
triggers. No business advisory lock is replaced with a session GET_LOCK.

The conservative concurrency tradeoff is explicit: writes to each natural
constraint serialize on one guard and exact-key lookup scans that constraint's
side table. This preserves equality without adding collision machinery, but is
not a claim of production throughput readiness. The Controller production gate
remains closed.

MySQL requires an additional write privilege to lock a selected guard row.
Runtime therefore receives SELECT and UPDATE(key_name), but an immutable-guard
trigger rejects every actual update, including no-ops. INSERT and DELETE remain
denied. Runtime cannot modify guards or side tables; owner-defined triggers
maintain private rows. Natural-key upserts must
not use ON DUPLICATE KEY UPDATE: a trigger conflict is not a native-index
conflict. The backend's `LockExactKey` supports a borrowed transaction's
lookup/insert/update sequence; `UpsertIdentity` implements the OIDC conflict-row
lock, metadata update, and disabled-identity refusal. Controller call sites
must use the corresponding domain Store; testing this adapter alone is not
Controller workflow acceptance.

Upgrade backfill inserts only missing owner rows and verifies both missing and
inconsistent keys without rewriting existing key bytes. Explicit repair may
retry that operation but cannot adopt an inconsistent existing side row. Any
change to a key component's physical representation, including timestamp to
integer conversion, must precede the initial backfill or have a separately
recorded rebuild; changing only the original column leaves invalid key bytes.
