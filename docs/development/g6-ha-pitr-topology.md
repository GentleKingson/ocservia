# G6 HA/PITR cross-VM harness

The G6 production-readiness environment requires the PostgreSQL primary and
standby, and every control-plane role, to span at least two real failure
domains — single-host container topologies cannot substitute. GitHub-hosted
runners cannot accept inbound connections, so the harness ships a
harness-only tunnel, `rust/crates/g6-tunnel` (`ocservia-g6-tunnel`), that
forwards PostgreSQL TCP traffic over an authenticated Iroh connection whose
peer node ID is pinned in both directions. The tunnel never runs in
production.

Topology (one compose project per failure domain from
`deploy/g6-ha-pitr/compose.yaml`):

```text
failure domain fd-alpha (VM A): postgres primary, api, worker, scheduler,
                                transportd, g6-tunnel serve/forward
failure domain fd-beta  (VM B): postgres standby, api, worker, scheduler,
                                transportd, g6-tunnel serve/forward
```

The two VMs prove they are distinct hosts by comparing SHA-256 hashes of
their kernel boot IDs; the hashes are recorded in the topology evidence and
the raw identifiers never leave the runner.

## Scenario

`gh workflow run g6-ha-pitr.yml --ref <ref>` runs on manual dispatch only:

1. Both failure domains exchange tunnel node IDs through run-scoped workflow
   artifacts and start pinned tunnels.
2. fd-a brings up the primary (streaming WAL, WAL archiving, data checksums),
   migrates, and starts its role split; fd-b clones the primary with
   `pg_basebackup` through the tunnel, imports the cloned cluster's
   credentials, and joins as the streaming standby, then starts its own role
   split against the remote primary through the tunnel. fd-a completes the
   acknowledged marker load and base backup first; only then, at failover
   readiness, does fd-b raise the standby to the confirmed synchronous
   standby (never at cluster init, which would block every commit before a
   standby exists), so every later write on the primary — the PITR markers
   and the outage declaration — is synchronously replicated.
3. fd-a writes acknowledged marker transactions and takes a verified base
   backup (`pg_basebackup` + `pg_verifybackup`).
4. fd-a verifies PITR on a separate restored instance that pauses at a named
   restore point between a before/after marker pair.
5. fd-a fences itself: outage marker, roles stopped, primary stopped, and
   write probes against the isolated primary that must all fail. Every probe
   writes a run-unique id with an upsert, so a writable instance always
   accepts the probe and only writability can decide the outcome.
6. fd-b promotes, clears `synchronous_standby_names`, publishes the true
   promotion boundary, recovers every role against the new primary, and
   reconciles the acknowledged markers.
7. fd-a receives the promotion record and probes its fenced former primary
   again — the dual-primary window starts at the promotion, so these
   post-promotion probes (not the isolation probes) feed the
   `dual_primary_write_accepts` evidence. fd-b fails the run unless the echoed
   promotion record matches its own byte for byte and every probe timestamp
   sits at or after that boundary.
8. fd-a recovers its roles through the tunnel to the new primary, rewinds
   with `pg_rewind`, rejoins as a streaming standby, and re-verifies that the
   rejoined instance rejects writes while read-only — and only the standby's
   own read-only SQLSTATE counts as rejection, never another SQL error. fd-b
   confirms the rejoined standby is streaming before it assembles the merged
   evidence, so the published timeline includes `old_primary_rejoined`.

## Evidence artifacts

The `g6-ha-evidence-*` workflow artifact carries public-safe structured
evidence whose shapes match the frozen G6 artifact contracts:

- `postgres-recovery.json` — `postgres_recovery` kind: outage declaration,
  service restoration, acknowledged markers, failover isolation with the
  true promotion boundary, dual-primary write probes taken after the
  promotion (plus the read-only rejections after rejoin), and present
  transaction IDs on the promoted primary.
- `pitr-report.json` — `pitr_report` kind: before/after markers, restore
  point, and paused-at-target verification.
- `timeline.jsonl` — `timeline` kind: the events this stage genuinely
  observes for `dual_primary_prevention` and `pitr_target_restore`. The
  `load_started`/`load_stopped` bracket of `database_failover_during_load`
  is deliberately not emitted here — this stage's failover happens after the
  marker load completes — and belongs to the later real-agent run whose
  failover overlaps live command, telemetry, and outbox traffic. Timestamps
  are stamped by the fd-b evidence owner when it observes each event, so the
  merged timeline stays monotonic across two runner clocks; RPO uses
  primary-database timestamps for the same reason.
- `topology.json` — stage topology snapshot with opaque failure-domain
  aliases and distinct-host attestations.
- `verification-summary.json` — measured RPO/RTO against the limits read at
  runtime from `docs/acceptance/g6-slo.yaml` (never hardcoded in the
  harness), acknowledged-transaction loss, dual-primary accepts, and PITR
  marker outcomes.

Harness thresholds are always read from `g6-slo.yaml`; regression protection
lives in `scripts/test-g6-ha-pitr-workflow.sh`, which fails if a threshold is
copied into shell code, a required timeline event stops being produced, the
load bracket events are emitted by this stage, fd-b skips importing the peer
cluster credentials, the environment id leaves the frozen
`g6-[a-z0-9]{8,32}` pattern, a write probe stops upserting run-unique ids or
accepts a non-read-only SQL error as post-rejoin rejection, finalize stops
binding probe timestamps to the recorded promotion boundary, or the
post-promotion probes disappear from either failure domain's flow.

## Timing diagnostics

Every HA/PITR run also emits non-authoritative timing telemetry so
orchestration costs stay measurable without touching verdicts. Both failure
domains wrap runner preparation, the toolchain bootstrap, the control-plane
and transportd image builds, the tunnel build, every harness phase, every
rendezvous artifact wait, diagnostics, cleanup, and the final upload with
`scripts/g6-timing.sh` calls that are always guarded: a timing failure can
never fail or pass a run, and each wrapped wait or phase keeps its own exit
status.

The per-domain `g6-timing-ha-fd-{a,b}-*` artifacts (retained five days) record
per-stage durations, compose image IDs and sizes, the tunnel binary size, the
WAL archive and base-backup footprints, and the rendezvous count plus
cumulative wait milliseconds, all bound to the run ID, attempt, and candidate
SHA. The policy test fails if a timing call stops being guarded, a required
stage stops being timed, a rendezvous wait stops preserving its result, or the
timing upload becomes required for a green run.

## Verification boundary

This stage deploys api, worker, scheduler, transportd, and PostgreSQL across
two real hosts and proves the PostgreSQL HA/PITR contract end to end. It does
not yet deploy the dedicated relay pair or the real-agent fleet, and it does
not emit the full twelve-artifact G6 evidence bundle; those land with the
real-agent harness stage that produces the final G6 verdict evidence. The
tunnel exists because hosted runners have no inbound connectivity; multi-zone
and multi-region failure domains remain unproven on this infrastructure and
stay subject to the frozen G6 topology contract.
