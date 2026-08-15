# PostgreSQL failover, former-primary fencing, and rejoin

This runbook covers manual failover from a failed PostgreSQL primary to a
streaming standby, fencing of the former primary, and its rejoin as a standby.
Failover is always an explicit operator decision: the platform does not
auto-promote standbys, because an unreachable primary is not proof of death
and an automatic promotion without reliable fencing can create two writable
primaries.

Trigger: primary host loss, unrecoverable primary corruption, or a planned
primary replacement. Before promoting, confirm the primary is truly isolated
(stop it, or isolate its network) — a paused primary that can still accept
writes is the dual-primary hazard.

## Failover steps

1. Stop application writers on every failure domain (control-plane `api`,
   `worker`, and `scheduler` roles) and record the outage declaration time.
2. Fence the failed primary: stop PostgreSQL on it and keep it stopped. While
   it is fenced, probe its write path from the client side; every attempt must
   fail. A fenced former primary must never accept a write between isolation
   and rejoin.
3. Promote the standby: `SELECT pg_promote(wait := true);`.
4. If the old primary ran with `synchronous_standby_names`, immediately clear
   it on the new primary and reload, otherwise every write blocks waiting for
   a standby that no longer exists:
   `ALTER SYSTEM SET synchronous_standby_names = ''; SELECT pg_reload_conf();`
5. Verify the new primary is writable with a probe transaction, then restart
   the control-plane roles against the new primary.
6. Reconcile acknowledged transactions: every transaction acknowledged to a
   client before the outage must be present on the new primary. Any loss means
   the failover violated its durability contract; stop and escalate instead of
   papering over missing rows.

Expected recovery evidence: new primary writable, API readiness restored,
worker and scheduler reconnected, zero dual-primary write accepts, zero
acknowledged-transaction loss, and RTO/RPO within the limits frozen in
`docs/acceptance/g6-slo.yaml` (`database_rto_seconds`, `database_rpo_seconds`).

## Rejoin of the former primary

The former primary may only rejoin as a standby, never as a peer primary.

1. Keep the old cluster stopped and confirm a clean shutdown state. If it was
   killed mid-write, start it once and stop it cleanly first; `pg_rewind`
   requires a clean-shutdown data directory.
2. Rewind it against the new primary (requires `wal_log_hints` or, as
   configured in this repository, `--data-checksums` initdb):
   `pg_rewind --target-pgdata=... --source-server='<new primary>';`
3. Point it at the new primary (`primary_conninfo` with a fresh
   `application_name`), create `standby.signal`, and start it.
4. Verify on the new primary that the rejoined instance appears in
   `pg_stat_replication` and reaches `streaming` state. Only then restore
   synchronous replication if the deployment uses it, using the rejoined
   instance's `application_name`.

## Rollback and abort

If promotion fails or the new primary cannot accept writes, stop, keep the
fenced old primary stopped, and restore from base backup plus WAL following
`postgres-pitr-restore.md` instead of forcing either cluster back writable.
Never start the old primary writable to "save time"; a split-brain write costs
more than any outage window.

## Credential rotation during recovery

Failover is a natural point to rotate PostgreSQL credentials: after roles are
stable, run `deploy/production/rotate-postgres-credentials.sh` (validated by
`scripts/i18-postgres-credential-rotation.sh`) so any credential that existed
during the incident is retired. Update the replication and application roles
together and restart dependent roles after rotation.

## Evidence collection

The cross-VM harness that exercises this entire runbook is
`.github/workflows/g6-ha-pitr.yml` (manual dispatch) driven by
`scripts/g6-ha-pitr-fd-a.sh` and `scripts/g6-ha-pitr-fd-b.sh`. It records the
outage declaration, promotion, dual-primary probes, reconciliation, RTO/RPO,
and rejoin into the `g6-ha-evidence-*` artifact; see
`docs/development/g6-ha-pitr-topology.md` for the artifact inventory.
