# P1 resilience and initial capacity validation

The P1 validation harness exercises the read-only control path with up to 500
side-effect-free simulated Agents. It uses a real 30-second heartbeat cadence
by default, emits bounded representative telemetry, and assigns deterministic
Direct and Relay path metadata. The harness also injects slow SSE consumption,
Controller and transport restarts, a temporary PostgreSQL outage, and an
interrupted operation whose outcome must remain explicit.
It also keeps 16 concurrent Viewer streams in the smoke profile and 100 in the
full profile. They share one workspace watcher across slow-consumer,
Controller-restart, and database-outage phases. The harness fails if watcher or
steady SQL query count grows with subscriber count, and records active and
rejected streams, healthy/unhealthy watcher and query counters, slow-consumer disconnects, file
descriptors, goroutines, RSS, and unrelated probe completion.

The authoritative profiles run on a standard GitHub-hosted `ubuntu-24.04`
runner. `P1 Smoke` runs for pull requests and `main` with 24 Agents, two
500-millisecond heartbeats, eight request submitters, and a 256-item queue. It
keeps every fault phase and resource-sample assertion while reducing load and
duration. `P1 Full Validation` is a manual workflow using the defaults below.

For optional local Linux reproduction with Docker, Compose, `curl`, and `jq`:

```bash
make p1-smoke
make p1-full
```

The defaults are 500 Agents, two heartbeats at 30-second intervals, 32 request
submitters, and bounded 2048-item transport queues. `REQUEST_CONCURRENCY` must
be an integer in `1..32`; configuration outside any I08 bound is rejected before
Docker or temporary resources are touched. Environment variables can reduce
the defaults for a smoke run but cannot increase them beyond the I08 envelope.

The transport stats writer publishes complete JSON snapshots through a
same-directory temporary file and atomic rename. The harness treats sampler
exit, malformed or incomplete samples, and missing phase coverage as failures.
At least ten valid samples must cover the capacity load, slow SSE, Controller
restart, transport interruption and recovery, and PostgreSQL pause and recovery.
An operation that was running when transport stopped must converge to `unknown`
within the bounded wait; `queued`, `dispatched`, `accepted`, and `running` are
never accepted as final outcomes. Output includes request and completion
p50/p95/p99, telemetry count, path mix, goroutines, Tokio simulator tasks, RSS,
file descriptors, database pool activity, SSE admission and watcher counters,
timestamps, phase counts, and sampler status.

This is initial single-host evidence, not a production capacity claim. Simulated
Relay metadata does not prove multi-host or multi-failure-domain Relay behavior.
The blocking G6 stability gate is a continuous 300-second run over the real
production command path and complete G6 SLO. A 24-hour soak is a nonblocking
long-term operational observation: it does not replace G6 and elapsed soak time
alone does not block a release. Production-scale and dedicated, independent
Relay A/B failure-domain validation remain G6 requirements. The script labels
every Docker resource with its Compose project and removes only that project's
containers, network, volumes, and locally built images on success, failure, or
interruption.

Both hosted profiles write under `RUNNER_TEMP` and upload run parameters,
request and completion metrics, the JSON summary, resource samples, slow-SSE
output, interrupted-operation state, disk snapshots, Compose logs, container
status, and the final exit status. A standard runner's disk, CPU, and memory
bound the result. The full job must fail rather than silently reduce its load if
that VM cannot complete the configured profile.
