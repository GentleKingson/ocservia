# MySQL and MariaDB backup and restore validation

This is a pre-support operations contract. It does not enable production use;
that gate remains closed until PR-09 independently validates the complete
matrix. It covers pinned MySQL 8.4.10 and MariaDB 12.3.2 only.

The backend-specific images built from `backup.mysql.Dockerfile` and
`backup.mariadb.Dockerfile` use the matching native client. The worker creates
a single-transaction logical dump with triggers, routines, events, binary data,
and explicit database creation, records server flavor/version metadata, writes
SHA-256 checksums, atomically advances `LATEST`, and bounds retention. It does
not copy a database volume. Database accounts and server-global grants are
provisioning state and are not reconstructed from a schema dump; recreate them
from the protected provisioning source and re-run least-privilege checks before
cutover.

`database-backup.cnf` is a mode-`0444` Compose secret inside the private
launcher-owned secret directory. Its `[client]` section contains only the
dedicated backup account, TLS settings, and external endpoint. The entrypoint
copies it to a mode-`0600` temporary file before invoking the client. The backup
account needs the minimum privileges required to read tables and views and dump
triggers, routines, and events; it must not own the schema or migration tables.
For the pinned clients, grant `SELECT`, `SHOW VIEW`, `TRIGGER`, and `EVENT` on
`ocservia.*`. MySQL 8.4 additionally needs the global `SHOW_ROUTINE` dynamic
privilege; MariaDB 12.3 instead needs `SELECT` on `mysql.proc`. The dump uses
`--no-tablespaces`, so the account does not need `PROCESS`.

Restore only into a new isolated server. Run the matching image with
`scripts/mysql-restore-verify.sh`, an absolute completed backup directory, and
a mode-`0600` target administrator client file. The verifier rejects checksum
damage, backend mismatch, unsafe artifacts, and a pre-existing target database,
then reports schema compatibility, audit authentication shape, identities,
scheduler epoch, idempotency rows, and unfinished operations/commands.

The restore tool never redirects the Controller or grants command authority.
Without `--old-writers-fenced` it exits with status 3 after verification. Even
with that acknowledgement, a live restored scheduler lease is rejected and the
tool keeps command authority blocked. Before cutover, an operator must also run
an isolated Controller with the real audit keys so its startup verifies the
audit chain and schema/runtime permissions. Only after old writers are fenced,
that verification passes, pending work is reconciled, and the authority source
is unambiguous may the operator redirect traffic and allow commands.

Logical restore is not PITR, database failover, a storage snapshot, or
cross-engine migration. Those procedures require independent evidence and are
not supplied by this tooling.
