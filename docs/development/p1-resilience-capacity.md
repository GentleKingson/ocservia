# P1 resilience and initial capacity validation

The P1 validation harness exercises the read-only control path with up to 500
side-effect-free simulated Agents. It uses a real 30-second heartbeat cadence
by default, emits bounded representative telemetry, and assigns deterministic
Direct and Relay path metadata. The harness also injects slow SSE consumption,
Controller restarts, a SIGKILL and restart of the `transportd-stub` container
(not a real transportd, Agent, Relay or native node), a temporary PostgreSQL
outage (`compose pause postgres`), and an interrupted operation whose outcome
must remain explicit.
It also keeps 16 concurrent Viewer streams in the smoke profile and 100 in the
full profile. They share one workspace watcher across slow-consumer,
Controller-restart, and database-outage phases. The harness fails if watcher or
steady SQL query count grows with subscriber count, and records active and
rejected streams, healthy/unhealthy watcher and query counters, slow-consumer disconnects, file
descriptors, goroutines, RSS, and unrelated probe completion.

Both profiles are script-level manual acceptance, not part of Basic CI;
there is no P1 GitHub Actions workflow. `make verify` runs only the bounds
self-test `scripts/test-p1-resilience-capacity.sh`, not either profile.
`make p1-smoke` uses 24 Agents, two 500-millisecond heartbeats, eight request
submitters, a 256-item queue and a minimum of eight resource samples (default
ten). It keeps every fault phase and the other resource-sample assertions while
reducing load and duration. `make p1-full` uses the defaults below.

Run on a suitable Linux server with Docker, Compose, `curl`, and `jq`:

```bash
make p1-smoke
make p1-full
```

The defaults are 500 Agents, two heartbeats at 30-second intervals, 32 request
submitters, and bounded 2048-item transport queues. `REQUEST_CONCURRENCY` must
be an integer in `1..32`; configuration outside any I08 bound is rejected before
Docker or temporary resources are touched. The I08 envelope maxima are 500
Agents, 32 heartbeats, a 30000 ms interval, 32 submitters and a 4096-item queue.
Environment variables may move any setting within the envelope, so heartbeats and
queue capacity can be raised above these defaults but nothing can exceed a maximum.

The transport stats writer publishes complete JSON snapshots through a
same-directory temporary file and atomic rename. The harness treats sampler
exit, malformed or incomplete samples, and missing phase coverage as failures.
At least the minimum number of valid samples (ten unless overridden) must cover the capacity load, slow SSE, Controller
restart, transport interruption and recovery, and PostgreSQL pause and recovery.
An operation that was running when transport stopped must converge to `unknown`
within the bounded wait; `queued`, `dispatched`, `accepted`, and `running` are
never accepted as final outcomes. Output includes request and completion
p50/p95/p99, telemetry count, path mix, goroutines, Tokio simulator tasks, RSS,
file descriptors, database pool activity, SSE admission and watcher counters,
timestamps, phase counts, and sampler status.

These simulated single-host results establish neither production capacity nor
multi-host Relay behavior. Real recovery belongs to [Business resilience](resilience.md).
The script labels
every Docker resource with its Compose project and removes only that project's
containers, network, volumes, and locally built images on success, failure, or
interruption.

The harness records run parameters,
request and completion metrics, the JSON summary, resource samples, slow-SSE
output, interrupted-operation state, disk snapshots, Compose logs, container
status, and the final exit status. Set `ARTIFACT_DIR` outside the temporary
run directory to retain these diagnostics; no workflow uploads them.
The harness has no host capacity probe: insufficient disk, CPU or memory shows up
as a timeout or failed assertion, and the harness never silently reduces its
configured load.
