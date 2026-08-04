# P1 resilience and initial capacity validation

The P1 validation harness exercises the read-only control path with up to 500
side-effect-free simulated Agents. It uses a real 30-second heartbeat cadence
by default, emits bounded representative telemetry, and assigns deterministic
Direct and Relay path metadata. The harness also injects slow SSE consumption,
Controller and transport restarts, a temporary PostgreSQL outage, and an
interrupted operation whose outcome must remain explicit.

Run it on an isolated Linux host with Docker, Compose, `curl`, and `jq`:

```bash
RUN_ID="I08-$(date -u +%Y%m%dT%H%M%SZ)-$(git rev-parse --short HEAD)"
RUN_ID="$RUN_ID" ./scripts/p1-resilience-capacity.sh
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
file descriptors, database pool activity, timestamps, phase counts, and sampler
status.

This is initial single-host evidence, not a production capacity claim. Simulated
Relay metadata does not prove multi-host or multi-failure-domain Relay behavior.
Production-scale, 24-hour, and dedicated Relay A/B validation remain release
gates. The script labels every Docker resource with its Compose project and
removes only that project's containers, network, volumes, and locally built
images on success, failure, or interruption.
