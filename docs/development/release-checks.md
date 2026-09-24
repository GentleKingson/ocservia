# Release Check

One release workflow owns the candidate from build to publication. Tag runs
validate once before Publish; a manual dry-run is a debugging option, not a
second mandatory release round. `Basic CI Result` keeps its existing name
and PR routing. Release Check is not a required check for every PR.

| Responsibility | Release execution |
| --- | --- |
| Full CI | Existing key Go/Rust/Web/database checks once |
| Build | One actual Agent/privd/upgrader package build and Controller image set per supported architecture |
| Package & Upgrade | Native DEB/RPM installation and registered v1.0.0 upgrade/data preservation; candidate images reused |
| Application compatibility | v1.0.0 node against candidate on amd64 and arm64 |
| Business Smoke | Signed deployment, separate authenticated principals, apply, real VPN, automatic rollback and VPN again |
| Integration | Change-selected OIDC, PKI, browser and distinct recovery assertions |
| Resilience | Change-selected two-domain fault/recovery/correctness |
| Security | Existing source/dependency checks and exact-image scanning/SBOM |
| Release Check | All selected required jobs must succeed |
| Publish | Existing protected environment, signing and permissions; tested products only |

Per-architecture producers finish after build, basic smoke, sealing and upload.
Agent and Controller upgrades consume those artifacts in separate native jobs,
alongside compatibility, business, Resilience and artifact checks; none of those
checks waits for upgrade results. Both architectures' upgrades remain mandatory
Release Check inputs. Single-architecture dry runs also run their upgrades.

Native test-image preparation runs alongside product builds. Shared probe/Relay
images and the tunnel are sealed separately from session-only system/toolchain
bases and the RPM systemd image. Consumers use producer artifact IDs, verify
manifest and payload digests, and check image architecture and source labels.
No cache hit or mutable registry tag substitutes for the same-run artifact.
The compatibility workflow binds the prepared tools to the verified candidate
transport image only after the candidate arrives; G6 likewise wraps the verified
Agent payload. Neither path rebuilds a different candidate. Standalone diagnostic
entrypoints retain their own builds.

Only images and helper binaries are shared. Upgrade runners and both G6 fault
domains retain separate containers, databases, volumes and systemd state. RPM
diagnostics separate image preparation, systemd readiness and package lifecycle
time in `rpm-timings.tsv`; the producer build step records the expensive image
preparation before the candidate is available. A single-architecture diagnostic
prepares only its RPM environment, not unused compatibility/G6 helpers.

Each architecture prepares helpers, session bases and RPM in independent
workflow invocations. Controller upgrades depend only on candidate products;
Agent upgrades additionally wait for RPM. Business and Resilience need helpers,
while Compatibility needs helpers and session bases. Release Check requires all
selected consumers to succeed, transitively gating their fixture producers.
Download/load and archive costs must be included in measured timing;
fewer builds alone is not a wall-time
or runner-minute performance claim. Focused fixture tests run through
`bash scripts/test-release-upgrade.sh` on BuildServer.

The single executable selection table is
[`scripts/release-selection.mjs`](../../scripts/release-selection.mjs).
It resolves the last published stable Release and compares its complete tree
with the exact candidate, rather than inspecting only the final PR.
Web/client/gateway/authentication paths select Integration; shared protocol
and Controller paths select both specialties; recovery/Agent/Relay paths select
Resilience. Ordinary Markdown documentation does not select heavy specialties.
Support/acceptance policies, build/toolchain/deployment changes and unknown
paths select both. Modified checking tools select their owning scope.
An unavailable baseline/diff selects full supported scope. The first release
whose predecessor predates the selector also selects full scope.
This baseline is not the separately registered product upgrade baseline.

Selected jobs that fail, are cancelled, unexpectedly skip, or have no result
block publication. Unselected specialties must be skipped and carry the
selection reason, not PASS. Single-architecture diagnostics skip Release Check;
they cannot report complete release acceptance.
Selected Integration includes the core deployment/apply/VPN/rollback chain,
so it replaces standalone Business Smoke rather than initializing a second
environment. Release Check requires exactly the corresponding job outcome.

Build producers export actual artifact IDs and small candidate manifests
(SHA, version, architecture, filenames, SHA-256). Every product consumer checks
the trusted producer manifest digest and payload hashes explicitly, independently
of the download action's archive validation. Never look up the
latest successful artifact or guess a producer attempt from the consumer.
Re-signing/repackaging may change installers but cannot rebuild or alter the
tested payload archive. Final signature, trust root and payload validation
remain required. Read-only checks never obtain the production signing key.

Use `release.yml` with `version=1.0.1`, `arch=all` on the candidate branch
for the initial full dry-run. This does not publish, create tags or change
production Secrets. Follow [native validation](release-upgrade-validation.md)
and [Resilience](g6-readiness.md) for environment and rerun boundaries.

Preserve actual job/step timings, cache state and sanitized failure details.
Report measured wall time and runner-minutes separately from source-derived
build counts; do not label expected savings or queued/unrun work as measured.
There is no long-term evidence inheritance service or additional sign-off gate.
