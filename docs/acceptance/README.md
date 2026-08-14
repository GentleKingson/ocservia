# G6 acceptance contracts

This directory contains the public, machine-readable contracts consumed by the
G6 production-readiness harness. It does not contain credentials, private
topology details, or acceptance evidence.

- `g6-slo.yaml` is the only machine-readable source for G6 thresholds.
- `g6-evidence-schema.json` defines the final verdict document.
- `g6-topology-schema.json` defines the public-safe deployed topology record.

The harness must load the SLO file rather than copying threshold values into
scripts. A run is invalid if its candidate commit, release manifest, or
component digests do not match the deployed prerelease artifacts.

The 300-second window is the bounded G6 regression gate. Longer soak tests may
be useful operational observations, but they are not a substitute for this
gate and are not release-blocking by elapsed time alone.
