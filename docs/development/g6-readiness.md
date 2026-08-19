# G6 readiness harness

The G6 production-readiness gate requires a real multi-host topology: at
least two instances of every control-plane role across at least two failure
domains, a PostgreSQL primary and a streaming standby on distinct hosts, at
least fifty real Agents, and a fault-free 300-second observation window
with continuous resource sampling. The readiness harness
(`.github/workflows/g6-readiness.yml`, manual dispatch only) builds that
topology from two concurrent GitHub-hosted `ubuntu-24.04` runners that
rendezvous through run-scoped workflow artifacts, the same two-VM pattern
as `docs/development/g6-ha-pitr-topology.md`, and evaluates the frozen
contract in `docs/acceptance/g6-slo.yaml` end to end.

A bounded producer job builds the candidate-labeled Agent image once, records
its image ID and archive digest, and publishes that immutable release image to
both failure-domain runners. Each runner verifies and loads the same bytes
before the concurrent topology starts. This prevents independent package or
compiler builds from creating different runtime image identities for one
release component while preserving independent runtime state on the two VMs.

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
                          transportd, relay-a, 28 Agents
        fd-beta  (VM B): postgres streaming standby, relay-b, 27 Agents
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
   approves the 55-node Agent fleet (28 on VM A, 27 on VM B). After the
   complete trust snapshot is loaded, each domain starts one Agent canary,
   requires the controller read model to report it active, online, and fresh,
   and only then starts the remaining local Agents in bounded batches. A
   failed readiness cycle captures the controller response and Agent logs and
   permits one bounded fleet restart before failing closed.
2. VM B clones the primary through the tunnel, joins as the streaming
   standby, starts relay-b, and enrolls its Agents.
3. VM B opens one production command per node behind an Agent execution
   barrier and confirms at least fifty are non-terminal, then holds dispatch
   admission while a second fleet-wide wave remains due in the outbox. VM A
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
   remaining scenarios and keeps all 28 Agents alive; VM B proves
   authenticated traffic moves to relay-b and exercises the direct↔relay path
   transitions.
6. VM B runs the scheduler-leadership and connection-owner failover
   scenarios with stale-term rejection proofs, the three outbox crash
   windows (claim-before-send, send-before-mark, and an ingress barrier after
   result receipt but before database commit), the
   bulk-disconnect reconnect storm, and then the bounded 300-second
   fault-free window with ≥50 concurrent commands and continuous resource
   sampling. VM B then captures the 55-node final session inventory and
   requests a final freeze. Only then does VM A snapshot all 28 durable Agent
   journals and its final container inventory; VM B waits for that snapshot
   before assembly, so cleanup cannot race the final probes.
7. `scripts/build-g6-evidence.mjs` assembles the evidence bundle purely
   from trusted producers — the authoritative database tables, the
   per-Agent durable journals from both failure domains, the live transportd
   session inventory, the fenced-probe outputs, and the runner clocks. Durable
   effect completeness is checked over every synthetic command in the run,
   while HTTP latency, availability, and dispatch SLOs retain the bounded
   window's accepted-request population. The frozen verifier evaluates that
   bundle; an independent verifier job recomputes the environment identity
   from the run identity, re-evaluates the published bundle, and requires its
   verdict to match byte for byte.

## Artifacts

Run-scoped workflow artifacts carry the rendezvous and the published
evidence (`g6-rd-evidence-bundle-*` with its `verdict.json`, plus the
independent `g6-rd-verdict-*`); redacted per-domain diagnostics are kept
for five days. The bundle records the topology, the release manifest with
every component image digest, the raw structured artifacts, and every
artifact digest, all bound to the run's environment id and candidate
commit SHA.

Every rendezvous wait also reads the producer Job from the exact workflow
attempt. A failed producer step ends the peer wait immediately, while a
successful producer gets only a short artifact-propagation grace period.
GitHub API calls and downloads have bounded connection, transfer, and retry
budgets. Diagnostics and cleanup have separate hard limits; cleanup uses a
support image cached before isolation and is forbidden from pulling images.
