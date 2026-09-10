# PR-02 Logical Time and Scheduler Adaptation

This work remains Draft. It does not enable Controller production startup or
establish complete Controller workflow parity.

## Remaining Type Inventory

`database-pr02-type-inventory.json` is the machine-readable inventory of the
published baseline fields not converted in version 3: 143 DATETIME fields,
11 JSONB fields and one text-array field. It records the original physical
definition rather than inferring that every native JSON column is JSONB.
The inventory is a pre-version-4 coverage denominator, not a statement that
all listed domains have been adapted. Version-4 and version-5 adapters are matched
against those entries during acceptance.

## Scheduler Store

The coordination Runner now uses a database Backend. Acquisition and renewal
run through the same generic transaction and scheduler Store on all engines.
The original PostgreSQL construction and Fence interfaces remain explicit
compatibility bridges for existing callers. `AssertFenceTx` supports actual
Session fences without committing the caller's transaction, and rejects an
unsupported legacy-only fence rather than silently skipping it.

Acquisition and renewal use the captured database transaction timestamp.
Fencing instead checks database wall-clock time and holds the shared singleton
row lock through commit. MySQL/MariaDB recheck wall-clock time after acquiring
that lock, so waiting for a lock cannot authorize an already-expired lease.
The generic Store preserves owner identity, incarnation and epoch checks.

`SchedulerTimeSteps` converts only `scheduler_leadership.lease_until` and
`updated_at`; the separate `scheduler_leases` domain is not silently converted.
The new storage is signed PostgreSQL-epoch microseconds with finite-range and
infinity checks. Callers bind explicit Timestamp values rather than rewriting
all `time.Time` arguments globally.

## Historical Sentinel Decisions

The published year-1000 representation is ambiguous. A non-seed value cannot
be classified as finite or negative infinity from the DATETIME alone.
The append-only migration therefore keeps the source column, leaves the
shadow unresolved, and fails verification until an owner records a decision.
The only automatic negative-infinity decision is the exact never-acquired
scheduler singleton: id 1, zero instance UUID, incarnation 0, epoch 0 and the
published year-1000 lease value. Its generated `updated_at` remains finite.

The owner-only `time_migration_decisions` table records table, column, row key,
exact source value and either `finite` or `negative_infinity`. An existing
owner decision is never overwritten. The decision must match the old value;
it does not authorize translating a subsequently changed source.

Repair must acquire the same database migration GET_LOCK on a dedicated
connection, inspect the preserved source, and insert the decision using that
connection. The owner then releases the lock with confirmed unlock/connection
discard and invokes explicit checksum-bound migration repair. Ordinary owner
connections are also denied writes to guarded source tables while the
migration is incomplete. Runtime has no access to the decision table.

Repair repeats the complete data copy and null-safe verification. The runner
checks source/shadow equality again immediately before dropping source
columns; it cannot adopt an unverified shadow. Writer guards survive failure
and are removed only after the switch. Published version-1 through version-3
artifacts are unchanged; these helpers are inputs to a new version, not a
permission to rerun candidate DDL outside the recorded migration chain.

## Privd Attestation Times

Version 6 converts the eight privd attestation times: credential expiry,
consumption and creation, plus key creation, approval, activation, validity
bounds and revocation. Every value on this domain is a business-finite instant
written by Controller code; unlimited key validity is represented by NULL,
never by a PostgreSQL infinity. The migration therefore needs no per-row
infinity decisions: historical finite extremes such as a year-1000 DATETIME
convert as finite values, matching the populated-upgrade guarantee rather
than guessing a sentinel. The two CHECK constraints that order these columns
(expiry after creation, consumption not before creation, validity not before
activation, revocation paired with state) and the two secondary indexes that
contain them are dropped and re-added around the verified switch because
MySQL removes a dropped column from its indexes.

## Certificate Download Boundary

The existing Service download methods now use the generic artifact Store:
OpenArtifact takes the original global capacity lock before its eligibility
read and lease update; CompleteArtifact persists the consuming evidence before
the root RPC; finalization appends its audit event and asserts the scheduler
fence within the same transaction. Abort retains the outstanding grant lease.
Exact completion replay does not repeat the root mutation or audit append.
MySQL/MariaDB use the corresponding transaction lock record, never GET_LOCK.

Version 5 converts all six artifact operation times and certificate `not_after`
to checked PostgreSQL-epoch microseconds. Download eligibility distinguishes
NULL, both infinities and finite extremes; positive infinity does not extend
the bounded grant deadline. Other certificate times remain native. Certificate
issuance, revocation and maintenance writers still require adaptation: the
download-only backend constructor does not claim those methods are portable. Existing
owner-fencing RPC integration remains unchanged, not reimplemented by a
database-lock primitive.
