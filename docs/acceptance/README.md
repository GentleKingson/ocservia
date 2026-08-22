# G6 acceptance contracts

This directory contains the public, machine-readable contracts consumed by the
G6 production-readiness harness. It does not contain credentials, private
topology details, or acceptance evidence.

- `g6-slo.yaml` is the only machine-readable source for G6 thresholds,
  comparators, units, scopes, and required timeline events.
- `g6-evidence-schema.json` defines untrusted measured evidence.
- `g6-topology-schema.json` defines the public-safe deployed topology record.
- `g6-verdict-schema.json` defines the independently computed verdict.

The workflow preserves failure provenance as a sequence of separately uploaded
and independently bound layers. Both runtime jobs emit immutable raw evidence,
then a dedicated job assembles the cross-domain bundle, a separate job scans
all raw and assembled artifacts for secrets, an independent job verifies the
bundle, and the final gate aggregates those results. Every layer repeats the
exact candidate SHA, workflow run ID and attempt, `environment_id`, and
authority, plus the frozen release manifest digest; a mismatch fails closed.

- `ocservia.g6-runtime-result.v1` records each failure domain's terminal state.
- `ocservia.g6-source-manifest.v1` binds every raw file by path, size, and digest.
- `ocservia.g6-assembly-result.v1` preserves bundle-construction diagnostics.
- `ocservia.g6-secret-scan-result.v1` records the independent redacted scan.
- `ocservia.g6-gate-result.v1` aggregates all preceding layers without
  converting an engineering rehearsal into a final production-readiness pass.

The harness records actual values and source artifact digests. It does not
authoritatively declare limits, comparisons, per-item results, or the final
result. `scripts/verify-g6-evidence.mjs` loads the exact SLO, topology, release
manifest, and evidence files plus the raw artifact files under `--artifact-root`;
checks their digests and candidate identity; re-hashes every declared artifact
file itself and rejects missing files, content digest mismatches, path
traversal, and symbolic links; and independently recomputes every result.

## Verified metric producers

Every artifact declares a kind the verifier recognizes, and a final evidence
bundle must contain exactly one artifact of each structured kind plus any
number of opaque `harness_log` files:

- `resource_samples` — bounded-window process sampling CSV for RSS, file
  descriptors, tasks, queue depth, and PostgreSQL connections;
- `timeline` — the ordered observation timeline;
- `epoch_events` — the connection-owner and scheduler epoch event log,
  including each registered authority's immutable tuple and lease deadline,
  plus exact-term fenced scheduler maintenance identifiers, their durable
  marker timestamps, and the independently observed commit boundary used for
  scheduler-takeover timing;
- `authority_cut` — the DB-clock owner and scheduler lease tuples plus the
  complete transport authority/session term projections immediately before
  and after that cut and the live scheduler term's durable maintenance marker;
- `command_trace` — the command dispatch/accept/effect/result trace;
- `outbox_snapshot` — the final transactional-outbox and reconciliation
  snapshot;
- `http_samples` — per-request read and enqueue outcome samples;
- `telemetry_snapshot` — per-agent telemetry freshness at the final boundary;
- `audit_correlation` — the audit intent/result correlation report;
- `postgres_recovery` — LSN, acknowledged-transaction markers, failover, and
  dual-primary probes;
- `pitr_report` — the PITR marker and restore-point report;
- `agent_sessions` — the unique authorized physical-session inventory,
  authority leases, session expiries, and reconnect storm record;
- `relay_transitions` — the relay and path transition report.

Every record and row of every structured artifact repeats the run's
`environment_id` and `candidate_sha`, so an artifact swapped in from another
run, environment, or build is rejected even when the evidence digest is
updated to match the swapped bytes. All artifact timestamps must stay inside
the evidence window, except lease deadlines, which must extend beyond their
DB-clock cut and may outlive the window. Line-oriented artifacts must use LF
endings, strictly increasing sequences, non-decreasing timestamps, and unique
event, command, agent, transaction, and effect identifiers. Epochs must
strictly increase per node and per leadership domain. The final active epoch
tuples and live lease deadlines must exactly match the DB-clock authority cut
and session inventory, and completed owner, scheduler, and relay takeovers
must actually be recorded, so an empty or reordered log cannot produce a
vacuous pass.

The authority cut's `before_complete_at` is sampled from PostgreSQL immediately
after the entire before-cut transport inventory completes. Its
`after_start_at` is sampled from the same database clock immediately before the
after-cut inventory starts. Both boundaries and `cut_at` retain microsecond
precision. The verifier requires `before_complete_at < cut_at < after_start_at`,
identical immutable physical-session and signed-owner tuples across the two
inventories, live session and owner-lease deadlines across the cut, and exact
agreement between each bracketed owner term and the database cut.
The projection intentionally excludes transient path, round-trip-time, and
last-seen diagnostics; it retains every identity, signed owner-fence,
authorization, connection, lease, and session-lifetime field used by the
independent proof.

Each SLO metric freezes an explicit trust boundary. Every metric in the frozen
contract now carries a `derivation`: the verifier recomputes both the value
and the sample count directly from the verified artifact bytes — ratios,
nearest-rank percentiles, growth trends, takeover intervals, and consistency
counts alike — and evidence that declares a different value or an inflated
sample count is rejected. Evidence cannot lower a classification bound to
game a ratio: the telemetry freshness bound and command dispatch bound are
themselves derived metrics limited by the SLO, and the verifier applies the
recorded bound when recomputing the paired ratio.

Population semantics are part of the contract, not a harness choice:

- the telemetry snapshot must cover exactly the authorized connected agents
  derived from the session inventory — a fresh-looking subset cannot stand
  in for the fleet, and the freshness ratio is classified over that whole
  population;
- the command dispatch ratio classifies every accepted (enqueued) command:
  an undispatched command is a miss, only each command's first dispatch
  attempt can satisfy the bound, repeated dispatch retries never inflate
  sample counts, and a command carries exactly one terminal result;
- reconnect storm recovery is derived per agent as
  `max(reconnected_at) - bulk_disconnect_at` over the authorized connected
  sessions, each of which must predate the bulk disconnect and reconnect on
  its own afterwards — a harness-declared completion time is not trusted;
- a relay takeover is proven only by authenticated session traffic through a
  replacement relay after the failed relay went down, not by a control-plane
  "active" record;
- the accepted-write population is one identity chain: every enqueue HTTP
  attempt carries a unique `request_id`, attempts for one logical enqueue
  retain one idempotency key, and only the exact known pre-mutation
  stale-revision 409
  may continue with a higher requested revision inside the frozen three-
  attempt bound after the prior response completes. Success is derived once
  per logical enqueue, while durable-enqueue latency is the terminal accepted
  POST's wire latency rather than client-side revision lookup time. The final
  successful command, outbox row, audit write, and command-trace enqueue must
  retain that identity chain. A command accepted anywhere and missing from
  the trace leaves the dispatch ratio denominator and is rejected;
- PITR markers must use distinct transaction identifiers, so the
  present/absent restore pair cannot contradict itself;
- evidence may not claim more authorized real agents than the topology
  binds.

The timeline artifact is parsed line by line with strict RFC 3339 timestamps,
unique event identifiers, strictly increasing sequences, and non-decreasing
timestamps; every observation must reference the timeline artifact, every
declared event must exist in it, and the SLO-required events must occur there
in their frozen order.

`declared_by_harness: true` remains a recognized but non-graduated state:
while any required metric keeps that marking the verifier cannot award a
final G6 pass — it emits the failure reason
`final pass requires verified metric producers`. Such a metric must graduate
to an artifact `derivation`, or to an attested producer for infrastructure
facts that genuinely cannot be recomputed from recorded bytes, in a later
contract revision; no current G6 metric requires an attested producer. The
protected workflow supplies the expected authority, opaque environment ID,
and failure-domain class separately, so the verifier rejects relabeled
evidence. Engineering-authority runs receive the same full per-metric
verdicts, but a final pass always requires `production_readiness` authority.

A final pass also requires a `production_readiness` authority, a multi-host,
multi-zone, or multi-region topology, and the topology role requirements
frozen in `g6-slo.yaml`: minimum instance counts and failure-domain spread for
API, worker, scheduler, transportd, and relay instances, at least fifty bound
agent instances, single primary and standby PostgreSQL instances, a
release-manifest component binding for every role, and a distinct failure
domain for the PostgreSQL primary and standby. Every topology instance must
name its release component, the exact component name and digest pair must
exist in the release manifest, and evidence may not claim more authorized
real agents than the topology binds.

The public fixtures under `testdata/g6/` are regenerated by
`scripts/generate-g6-test-fixtures.mjs`, which derives the passing evidence
document from the verifier's own recomputation; `--check` fails when the
committed fixtures drift from that output.

Topology identifiers are opaque aliases. They must not encode provider account
names, real hostnames, IP addresses, internal domains, database URLs, relay
URLs, secret names, or credentials. The schema intentionally has no arbitrary
label map. Published evidence bundles also require an independent secret scan
in the production-readiness workflow.

The 300-second window is the bounded G6 regression gate. Longer soak tests may
be useful operational observations, but they are not a substitute for this
gate and are not release-blocking by elapsed time alone.
