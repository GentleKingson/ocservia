# Release Check

One release workflow owns the candidate from build to publication. Tag runs
validate once before Publish; a manual dry-run is a debugging option, not a
second mandatory release round. `Basic CI Result` keeps its existing name
and PR routing. Release Check is not a required check for every PR.

| Responsibility | Release execution |
| --- | --- |
| Full CI | Existing key Go/Rust/Web/database checks once |
| Build | One actual Agent/privd/upgrader package build and Controller image set per supported architecture |
| Packages | Current-candidate native package smoke and complete dual-architecture package identity checks |
| Business Smoke | Signed deployment, separate authenticated principals, apply, real VPN, automatic rollback and VPN again |
| Integration | Change-selected OIDC, PKI, browser and distinct recovery assertions |
| Resilience | Change-selected two-domain fault/recovery/correctness |
| Security | Existing source/dependency checks and exact-image scanning/SBOM |
| Release Check | All selected required jobs must succeed |
| Publish | Existing protected environment, signing and permissions; tested products only |

Per-architecture producers finish after build, basic smoke, sealing and upload.
Business, Resilience and artifact checks consume the verified current products.
Historical native upgrades and mixed-version application compatibility are no
longer release gates or diagnostic modes. No historical package is needed to
prepare the candidate; version format and exact checkout/workflow SHA remain
mandatory, as do producer manifest and payload digests.

Native test-image preparation runs alongside product builds. Shared probe/Relay
images and the tunnel are sealed for current Business and Resilience checks.
Consumers use producer artifact IDs, verify
manifest and payload digests, and check image architecture and source labels.
No cache hit or mutable registry tag substitutes for the same-run artifact.
G6 wraps the verified Agent payload; its fault domains retain isolated state.
Release Check requires all selected consumers to succeed, transitively gating
their fixture producers. Focused fixture and gate tests run through
`bash scripts/test-release-upgrade.sh` on BuildServer.

Release Rust compilation caches are accelerators, not candidate artifacts.
Agent restores only its architecture/builder-specific Rocky target directory;
the cache key also binds the toolchain, lockfile, manifests, Cargo configuration
and build scripts. A source-SHA suffix permits a new cache after changed source,
with fallback only inside that same build identity. Every hit still runs the
locked release build and native ABI/version checks before packaging and smoke.
The Ubuntu test target and Controller compilation objects are never restored
into this directory.

The transport Dockerfile uses pinned cargo-chef to derive a dependency recipe
from the complete workspace. Dependency cooking and the final locked build use
the same toolchain, package, release profile and Cargo configuration; the patched
vendor sources and real workspace sources are copied before the final build.
Existing per-architecture BuildKit exports include the dependency layer, without
requiring an unexported cache mount or a second compiler-cache backend.

GitHub cache visibility still applies: caches created on one release tag are
not automatically available to another tag. A successful dry-run on trusted
`main` can populate default-branch caches accessible to subsequent releases;
this change does not add scheduled or automatic prewarming. Include cache
restore/save and BuildKit export time when assessing net build savings, and do
not assume a same-branch warm run represents the next tag's cold start.

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
This comparison selects current checks; it does not impose an upgrade baseline.

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
