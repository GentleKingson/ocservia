# P1 resilience and initial capacity validation

The P1 validation harness drives the read-only control path with up to 500
side-effect-free simulated Agents. By default, those simulated Agents send
heartbeats every 30 seconds, emit bounded telemetry, and carry deterministic
Direct and Relay path metadata. During the run, the harness injects slow SSE
consumers, restarts the Controller, sends SIGKILL to the `transportd-stub`
container before restarting it (without running a real transportd, Agent,
Relay, or native node), pauses PostgreSQL (`compose pause postgres`), and
tracks an in-flight operation across the outage to confirm that its final state
remains explicit.

In parallel, the run holds open 16 concurrent Viewer SSE streams in the smoke
profile and 100 in the full profile. All Viewer streams share a single
workspace watcher across the slow-consumer, Controller-restart, and
database-outage phases. If watcher instances or steady-state SQL query counts
scale with the number of subscribers, the run fails. Along the way, the
collector records active and rejected stream counts, healthy and unhealthy
watcher and query counters, slow-consumer disconnects, file descriptors,
goroutines, RSS, and completion of an unrelated probe.

Both profiles are manual script-level acceptance checks outside Basic CI, and
have no dedicated GitHub Actions workflow. Running `make verify` executes only
the bounds self-test (`scripts/test-p1-resilience-capacity.sh`) without
starting either capacity profile. On a Linux host equipped with Docker,
Compose, `curl`, and `jq`, invoke the profiles directly:

```bash
make p1-smoke
make p1-full
```

`make p1-smoke` keeps all fault phases and resource assertions while scaling
the workload down to 24 Agents, two 500-millisecond heartbeats, eight request
submitters, a 256-item transport queue, and a minimum of eight resource samples
(defaulting to ten). `make p1-full` runs with the full defaults: 500 Agents,
two heartbeats spaced 30 seconds apart, 32 request submitters, and bounded
2048-item transport queues. `REQUEST_CONCURRENCY` must be an integer in
`1..32`, and any parameter outside the I08 envelope is rejected before the
script touches Docker or creates temporary directories. The I08 upper bounds
are 500 Agents, 32 heartbeats, a 30000 ms heartbeat interval, 32 submitters,
and a 4096-item queue; environment overrides can adjust settings within that
envelope (for instance, raising heartbeat count or queue depth above the
defaults), but cannot exceed those maxima.

To avoid partial reads, the transport stats writer writes each JSON snapshot to
a temporary file in the same directory and renames it atomically. A premature
sampler exit, malformed or incomplete snapshot, or missing phase observation
fails the run. At least ten valid samples (unless overridden down to the smoke
minimum) must span the capacity load, slow SSE, Controller restart, transport
interruption and recovery, and PostgreSQL pause and recovery phases. Any
operation that was running when transport stopped must converge to `unknown`
within the bounded wait window; `queued`, `dispatched`, `accepted`, and
`running` are rejected as terminal states. The final report captures request
and completion p50/p95/p99 latencies, telemetry volume, path distribution,
goroutines, Tokio simulator tasks, RSS, file descriptors, database pool
activity, SSE admission and watcher counters, timestamps, per-phase sample
counts, and sampler exit status.

Because the entire topology runs simulated components on a single host, passing
this harness does not establish production capacity or multi-host Relay
behavior; real service recovery is covered under
[Business resilience](resilience.md). Every Docker resource created during the
run carries the Compose project label, and the cleanup trap deletes only that
project's containers, networks, volumes, and locally built images on success,
failure, or signal interruption.

At completion, the run directory holds the input parameters, request and
completion metrics, JSON summary, resource samples, slow-SSE output,
interrupted-operation state, disk snapshots, Compose logs, container statuses,
and final exit code. Point `ARTIFACT_DIR` outside the temporary working
directory if you want to keep those files after cleanup, since no workflow
uploads them. The script does not preflight host disk, CPU, or memory capacity:
an undersized machine surfaces as a timeout or failed assertion, and the
harness never lowers the configured load behind the scenes.
