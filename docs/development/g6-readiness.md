# Resilience regression

The reusable `g6-harness-core.yml` executes a finite cross-host fault regression.
Release invokes it when [release selection](release-checks.md) requires it.
`g6-readiness.yml` remains an explicitly authorized manual diagnostic entrypoint.
Neither result certifies all production faults or a deployment's long-term SLO.

The execution has three logical stages: prepare frozen products and helper
binaries, execute two independent fault domains, calculate one result.
There is no assembly/verifier double calculation, result-comparison job,
independent scan-result artifact, or aggregate of aggregates.
Release supplies the exact tested Controller images and Agent payload.
Probe/tunnel and orchestration helpers compile separately without changing
production Cargo feature sets or ABI. Standalone diagnostics may build their
own candidate and are not release evidence for another build.

Each fault domain preserves required operation IDs, epochs, receipts, ordering,
runtime status and cleanup registry. Before raw upload, scan for secrets.
At consumption, download the actual producer artifact ID, check the producer
manifest digest, then validate every declared file and run binding.
The result job requires both runtimes to succeed and computes the verdict once.
It scans generated diagnostics before upload. Missing observations, failed
fault injection, interrupted topology or a crashed harness never count as PASS.

Engineering and production-readiness authority remain execution context and
select the existing environments. Valid engineering execution returns success;
it is not forced to fail solely to signal that it is not a production certification.

## Correctness and diagnostics

Wrong fencing, duplicate effects, acknowledged transaction loss, dual-primary
writes, incorrect PITR markers, lost durable state, invalid root receipts and
failure to recover inside bounded waits remain blocking. Schema/integrity and
required-event validation remain blocking regardless of assessment purpose.

Original measurement limits stay in [g6-slo.yaml](../acceptance/g6-slo.yaml).
`diagnosticMetrics` in
[`g6-contract-lib.mjs`](../../scripts/g6-contract-lib.mjs) identifies sampling,
short-window ratios/latencies and small resource-growth measurements that do
not block `assessment=resilience`. Their actual values and original
pass/fail comparisons remain in the result. `assessment=performance` evaluates
the original limits. Neither mode treats a crash, OOM, deadlock or missing
measurement as runner noise.

The current topology still uses 50 Agents and its existing observation window.
A smaller functional topology must be established by actual execution before
changing the count; static tests cannot establish that minimum. Performance
and capacity interpretation remains separate from finite correctness.

## Execution and reruns

Use `gh workflow run g6-readiness.yml --ref <candidate> -f authority=engineering
-f purpose=resilience` only with hosted-run authorization. For original metric
assessment select `purpose=performance`. Do not substitute shared BuildServer
for a disposable cross-host topology.

Both fault domains share one fault timeline, run attempt and bounded rendezvous.
Rerun the whole Resilience group after a domain failure, not just one domain
against another attempt's checkpoint. Independent package architecture jobs
do not have this restriction. There are no automatic mutation retries to green.

The existing [HA/PITR topology](g6-ha-pitr-topology.md) and
[acceptance contracts](../acceptance/README.md) define the detailed runtime
facts. Diagnostics are short-lived and sanitized; never upload plaintext
credentials, session cookies, private keys or raw databases.
