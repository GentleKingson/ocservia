# G6 readiness harness

The G6 production-readiness gate requires a real multi-host topology: at
least two instances of every control-plane role across at least two failure
domains, a PostgreSQL primary and a streaming standby on distinct hosts, at
least fifty real Agents, and a fault-free 300-second observation window
with continuous resource sampling. The manual
`.github/workflows/g6-readiness.yml` caller invokes the formal profile in
`.github/workflows/g6-harness-core.yml`, which builds that topology from two
concurrent GitHub-hosted `ubuntu-24.04` runners that
rendezvous through run-scoped workflow artifacts, the same two-VM pattern
as `docs/development/g6-ha-pitr-topology.md`, and evaluates the frozen
contract in `docs/acceptance/g6-slo.yaml` end to end.

G6 runs only through manual `workflow_dispatch` in `g6-readiness.yml`,
before a release or a major architecture change. It is not part of ordinary
PR Basic CI. The caller passes `profile=formal`, the selected `authority`,
and `candidate_sha=${{ github.sha }}` to the reusable core. The PR smoke
entry point and all smoke jobs have been removed, with no replacement G6
checks. G6-specific file changes select only Basic CI's docs check.

A bounded producer job builds the candidate-labeled control-plane, transportd,
relay, probe, and Agent images once and includes the exact PostgreSQL support
image in one checksummed archive. It records every image ID and publishes that
immutable release set to both failure-domain runners. Each runner verifies and
loads the same bytes before the concurrent topology starts, then overlays those
images onto its independent runtime state instead of rebuilding them locally.

## Authorities

The dispatch input selects the evidence authority:

- `engineering` (default) — the rehearsal. The full production paths run —
  real transportd, Iroh, Agents, failover, PITR, outbox crash windows, relay
  transitions — but the verdict is fenced non-final. A rehearsal finds
  harness and deployment defects; it can never sign off G6.
- `production_readiness` — the formal run. Both failure-domain jobs and the
  verifier bind to the `g6-production-readiness` GitHub environment, so the
  repository owner can require independent approval before any formal
  evidence is produced; the rehearsal authority has its own environment.

The verifier (`scripts/verify-g6-evidence.mjs`, driven by the shared
`scripts/g6-contract-lib.mjs`) recomputes every metric from the raw
artifact bytes and only awards a final pass to
`production_readiness` evidence with a non-single-host failure-domain
class.

## Topology

One compose project per failure domain from `deploy/g6-readiness/compose.yaml`:

```text
era 1 — fd-alpha (VM A): postgres primary, api, worker, scheduler,
                          transportd, relay-a, 25 Agents
        fd-beta  (VM B): postgres streaming standby, relay-b, 25 Agents
era 2 — fd-beta  promotes the standby and runs the full control plane
        (transportd reuses the controller key handed over by rendezvous,
        so every Agent redials the same controller NodeId)
        fd-alpha  verifies PITR, rejoins as the standby, and recovers its
        roles against the promoted primary; relay-a is then stopped
```

Cross-VM paths (replication, peer API, peer relay) run over the pinned
harness-only tunnel (`rust/crates/g6-tunnel`) described in the HA/PITR
topology doc. All credentials are test-only, generated per run on VM A,
and shared to VM B through 1-day rendezvous artifacts; they never appear
in the published evidence, which an independent `gitleaks` job scans
before the run is accepted.

## Scenario sequence

1. Both failure domains exchange tunnel identities; VM A brings up the
   primary, migrates, starts its role split and relay-a, then enrolls and
   approves the 50-node Agent fleet (25 on VM A, 25 on VM B). After the
   complete trust snapshot is loaded, each domain starts one Agent canary,
   requires the controller read model to report it active, online, and fresh,
   and only then starts the remaining local Agents in bounded batches. A
   failed readiness cycle captures the controller response and Agent logs and
   permits one bounded fleet restart before failing closed.
2. VM B clones the primary through the tunnel, joins as the streaming
   standby, starts relay-b, and enrolls its Agents.
3. VM B opens one production command per node behind an Agent execution
   barrier, freezes a durable exact-population proof that all fifty are
   non-terminal and result-free, then releases the barriers and starts a
   second fleet-wide production wave. VM A
   takes and verifies the PITR base backup, then records marker A, the restore
   point, and marker B with their originating transaction identities. It
   switches WAL after marker B and waits for that completed segment to reach
   the archive before failure injection can continue.
4. VM A is isolated under load (primary failure, API instance loss). VM B
   promotes the standby, then proves the isolated former primary cannot write,
   and re-points the era-2 control plane at the promoted primary while the
   load commands are still open.
5. VM A verifies the PITR restore target, rejoins as the standby, proves the
   rejoined instance rejects writes as read-only, recovers its roles, and
   stops relay-a. VM A publishes only the control evidence needed for the
   remaining scenarios and keeps all 25 Agents alive; VM B proves
   authenticated traffic moves to relay-b and exercises the direct↔relay path
   transitions.
6. VM B runs the scheduler-leadership and connection-owner failover
   scenarios with stale-term rejection proofs, the three outbox crash
   windows (claim-before-send, send-before-mark, and an ingress barrier after
   result receipt but before database commit), the
   bulk-disconnect reconnect storm, and then the bounded 300-second
   fault-free window with ≥50 concurrent commands and continuous resource
   sampling. VM B then captures the 50-node final session inventory and
   requests a final freeze. Only then does VM A snapshot all 25 durable Agent
   journals and its final container inventory. Each runtime job publishes its
   own raw evidence and an exact source manifest; neither runtime job assembles
   or verifies the cross-domain bundle.
7. A dedicated assembly job validates both raw source manifests before
   `scripts/build-g6-evidence.mjs` assembles the evidence bundle purely
   from trusted producers — the authoritative database tables, the
   per-Agent durable journals from both failure domains, the live transportd
   session inventory, the fenced-probe outputs, and the runner clocks. Durable
   effect completeness is checked over every synthetic command in the run,
   while HTTP latency, availability, and dispatch SLOs retain the bounded
   window's accepted-request population. The frozen verifier evaluates that
   bundle. Separate jobs scan both raw artifacts and the assembled bundle for
   secrets, recompute the environment identity, independently verify the
   published bundle, and aggregate every bound layer into the final gate.

## Artifacts

Run-scoped workflow artifacts carry the rendezvous, immutable raw domain
evidence (`g6-rd-raw-fd-a-*` and `g6-rd-raw-fd-b-*`), the partial or complete
assembled bundle (`g6-rd-evidence-bundle-*`), the independent verdict, the
secret-scan result, and the final gate result. Redacted per-domain diagnostics
are kept for five days. The bundle records the topology, the release manifest with
every component image digest, the raw structured artifacts, and every
artifact digest, all bound to the run's environment id and candidate
commit SHA.

Every rendezvous wait also reads the producer Job from the exact workflow
attempt. A failed producer step ends the peer wait immediately, while a
successful producer gets only a short artifact-propagation grace period.
GitHub API calls and downloads have bounded connection, transfer, and retry
budgets. Diagnostics and cleanup have separate hard limits; cleanup uses a
support image cached before isolation and is forbidden from pulling images.
