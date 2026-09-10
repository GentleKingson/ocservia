# PR-03 Draft: Authentication, RBAC, Approvals and Audit

Base: PR-01 (#192) and PR-02 (#193), merged through `7d0e1ad`.
MySQL/MariaDB production admission remains disabled. No schema migration,
historical audit rewrite, deployment or merge is part of this change.

## Implementation

PR-02 already moved the Local/OIDC, RBAC, approval and audit workflows to the
common Store/Tx boundary with backend-owned SQL. PR-03 reuses those adapters
instead of duplicating business logic or adding another storage abstraction.

Management, initialization and capacity keep their existing lock namespaces:
PostgreSQL transaction advisory locks and MySQL/MariaDB `business_locks` records.
The latter are inserted/locked in the caller's transaction, so an absent
bootstrap singleton does not leave initialization unprotected. Audit append
retains its per-workspace transaction lock; verification remains repeatable-read.

`audit.AppendChainTx` now converts its clock to the persisted microsecond value
before hashing. It also parses summaries to the logical JSONB representation
before passing them to the unchanged canonicalizer. In particular, whitespace
around a top-level JSON `null` cannot make the writer include a field which the
reader omits. Hash/MAC domains, versions, canonicalization, checkpoint signatures
and historical verification are unchanged. Existing v1 float64 numeric rounding
is deliberately preserved, not presented as a newly lossless signature scheme.

## Regression Coverage

The same restricted-runtime API/backend fixtures now cover:

- Concurrent capacity reservations, including a populated table's capacity.
- Concurrent first initialization with no singleton; exactly one winner. A
  separate lock-wait assertion proves serialization even before that row exists.
- Rollback of both credentials, bootstrap state and audit on initialization failure.
- Fixed management workspace and cross-workspace authorization denial.
- Independent administrator/approver protection and concurrent attempts to remove
  either member of the last effective two-administrator combination.
- Password-reset approval consumption, credential change, session revocation and
  audit rollback in the same transaction; successful reset and replay rejection.
- Eligible legacy completion, rejection of a different workspace or pre-existing
  second elevated identity, unchanged original credential and one-shot completion.
- Exact Local/OIDC issuer/subject distinctions and exact approval action/resource
  identifiers, including case and trailing spaces under each engine's collation.
- Runtime denial of UPDATE, DELETE and TRUNCATE for events and checkpoints.

The existing shared audit/RBAC workflow additionally covers eight concurrent
appenders and an append/checkpoint committed between the verifier's checkpoint
and event reads. It verifies the original snapshot and then the newly committed
tail. Summaries include large numeric values, arrays, whitespace and JSON null;
readback must reproduce the persisted hashes and MACs.

The existing HTTP workflow still exercises login/logout, cookies, self password
change, independent approval, reset, disable and session invalidation. Existing
authentication required-test entries and browser regressions are retained.

## CI Gates

`scripts/required-go-tests.txt` adds exact required names for the precision/JSON
regressions, the three cross-backend authentication workflows and the shared
audit workflow. A missing, skipped or failed required test fails the gate.

The existing PostgreSQL integration script runs the authentication backend group
on a separate schema-34 database clone and checks fixture cleanup before dropping
it. The existing MySQL/MariaDB script runs the same group with its isolated
per-test databases. Owner connections only provision fixtures, migrations and
failure-injection constraints; business workflows use `ocservia_app`.

## Validation

All execution is through `ssh BuildServer`, in the isolated checkout
`/root/ocservia-pr03.GDWmJO`. The scoped runner and raw logs are retained there.
The following checks passed on 2026-09-10:

| Check | Evidence under `artifacts/` |
| --- | --- |
| PostgreSQL 17 backend authentication and audit workflows; existing 22 authentication, 8 API and 1 lifecycle required entries; audit integration tests | `pg17-3.log`, `pg17-final.log` |
| PostgreSQL 18 identical backend workflows and existing required entries | `pg18.log` |
| MySQL 8.4.10 backend authentication, approval/audit workflows, expiry after lock waits, exact-key lock ordering and full-range authentication time writes | `mysql-2.log`, `mysql-final.log` |
| MariaDB 12.3.2 identical backend workflows | `mariadb.log` |
| Final safety workflow, including absent-singleton lock contention and exact approval identifiers, on all four database versions | `safety-pg17.log`, `safety-pg18.log`, `safety-mysql.log`, `safety-mariadb.log` |
| All Controller packages compile; 42 required unit entries with race detection; legacy and new audit payload tests; targeted vet; migration checksums and shell/required-test guard checks | `compile.log`, `unit.log`, `audit-unit.log`, `vet.log`, `static-final.log` |
| Web typecheck/build, 32 focused tests, generated-client authentication check and all 12 required browser regressions | `web.log` |

Database workflows ran with `-race`, real engines and restricted runtime users.
Required-test gates and final safety runs reported no skipped required tests.
The final absent-singleton assertion was run separately on every engine after
the broader suites; its exact subtest name is also retained in the CI manifest.

Initial unsuccessful attempts are retained, not counted as passes: `pg17.log`
and `mysql.log` exposed a missing test-container temporary-directory mount;
`pg17-2.log` exposed an unscoped fixture constraint against preceding audit data;
`unit-ancestry-failure.log` exposed copied source ownership rejected by the
existing secure-key ancestry guard. The runner uses a mounted private temporary
directory, fault constraints are workspace-scoped, and only the isolated test
copy's ownership was corrected. No security check was relaxed.

This is focused PR-03 verification, not a rerun of the full migration/rollback
scripts or a green GitHub CI claim. Browser checks use the existing browser
fixtures; real backend HTTP handlers are covered separately, not described as
full browser-to-database E2E. Production IdP/TLS infrastructure and deployment
were not exercised. Keep the PR in Draft pending CI and review; no merge or
release is part of this change.
