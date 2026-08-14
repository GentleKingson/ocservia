# G6 acceptance contracts

This directory contains the public, machine-readable contracts consumed by the
G6 production-readiness harness. It does not contain credentials, private
topology details, or acceptance evidence.

- `g6-slo.yaml` is the only machine-readable source for G6 thresholds,
  comparators, units, scopes, and required timeline events.
- `g6-evidence-schema.json` defines untrusted measured evidence.
- `g6-topology-schema.json` defines the public-safe deployed topology record.
- `g6-verdict-schema.json` defines the independently computed verdict.

The harness records actual values and source artifact digests. It does not
authoritatively declare limits, comparisons, per-item results, or the final
result. `scripts/verify-g6-evidence.mjs` loads the exact SLO, topology, release
manifest, and evidence files plus the raw artifact files under `--artifact-root`;
checks their digests and candidate identity; re-hashes every declared artifact
file itself and rejects missing files, content digest mismatches, path
traversal, and symbolic links; and independently recomputes every result.

Every artifact declares a kind the verifier recognizes: exactly one
`resource_samples` CSV, exactly one `timeline` JSONL file, and any number of
opaque `harness_log` files. Each SLO metric freezes an explicit trust boundary.
Metrics with a `derivation` — the bounded-window sampling span, maximum gap,
and valid sample count — are recomputed by the verifier directly from the
verified `resource_samples` artifact, and evidence that declares a different
value is rejected. The timeline artifact is parsed line by line with strict
RFC 3339 timestamps, unique event identifiers, strictly increasing sequences,
and non-decreasing timestamps; every observation must reference the timeline
artifact, every declared event must exist in it, and the SLO-required events
must occur there in their frozen order. All remaining metrics are marked
`declared_by_harness: true`: their `actual` values stay untrusted harness
inputs, and while any required metric keeps that marking the verifier cannot
award a final G6 pass — it emits the failure reason
`final pass requires verified metric producers`. Each such metric must
graduate to an artifact `derivation` or an attested producer in a later
contract revision before its evidence can become production G6 acceptance.
The protected workflow supplies the expected authority, opaque environment
ID, and failure-domain class separately, so the verifier rejects relabeled
evidence.

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

Topology identifiers are opaque aliases. They must not encode provider account
names, real hostnames, IP addresses, internal domains, database URLs, relay
URLs, secret names, or credentials. The schema intentionally has no arbitrary
label map. Published evidence bundles also require an independent secret scan
in the production-readiness workflow.

The 300-second window is the bounded G6 regression gate. Longer soak tests may
be useful operational observations, but they are not a substitute for this
gate and are not release-blocking by elapsed time alone.
