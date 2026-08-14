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
manifest, and evidence files; checks their digests and candidate identity; and
independently recomputes every result. The protected workflow supplies the
expected authority, opaque environment ID, and failure-domain class separately;
the verifier rejects evidence that relabels them. A final pass also requires a
`production_readiness` authority and a multi-host, multi-zone, or multi-region
topology.

Topology identifiers are opaque aliases. They must not encode provider account
names, real hostnames, IP addresses, internal domains, database URLs, relay
URLs, secret names, or credentials. The schema intentionally has no arbitrary
label map. Published evidence bundles also require an independent secret scan
in the production-readiness workflow.

The 300-second window is the bounded G6 regression gate. Longer soak tests may
be useful operational observations, but they are not a substitute for this
gate and are not release-blocking by elapsed time alone.
