# Native release upgrade validation

`Native Release Upgrade Validation` is an independent `workflow_dispatch`
workflow, not a publisher and not part of Basic CI. Only `version`,
`baseline_release` (default `v0.6.0`), and `candidate_sha` are accepted.
Choose the candidate branch in the Actions UI or with `gh --ref`. The SHA
must be the complete lowercase commit SHA of that branch and must equal
both the dispatch SHA and checkout HEAD. The candidate's numeric X.Y.Z
version must be strictly newer than the baseline.

The maintained `.github/workflows/release-upgrade.yml` is the complete manual
workflow. Dispatch only after the candidate edits are committed and pushed to
the selected branch. Replace the branch and numeric version placeholders;
resolve and inspect the full 40-character lowercase SHA before dispatch, and
do not move the branch while the dispatch is being created:

```bash
branch='<candidate-branch>'
version='<candidate-X.Y.Z>'
sha=$(gh api "repos/GentleKingson/ocservia/commits/$branch" --jq .sha)
gh workflow run release-upgrade.yml --repo GentleKingson/ocservia \
  --ref "$branch" -f version="$version" -f baseline_release=v0.6.0 \
  -f candidate_sha="$sha"
```

## Scope

Quick/Full Basic CI checks change regressions. `release.yml` builds packages
and images, runs its release smoke checks, and publishes only through its
tag/approval path. This workflow validates the registered old release to exact
candidate upgrade on four native units without publishing. Formal G6 is the
separate production-readiness/HA/PITR acceptance harness. None of these gates
substitutes for another or extends a backend's production support.

Four mandatory cells run with fail-fast disabled:

| Cell | Native runner | Required upgrade path |
| --- | --- | --- |
| agent-amd64 | ubuntu-24.04 / X64 / x86_64 | Published DEB on Ubuntu and RPM on systemd Rocky 9 |
| agent-arm64 | ubuntu-24.04-arm / ARM64 / aarch64 | Published DEB on Ubuntu and RPM on systemd Rocky 9 |
| controller-amd64 | ubuntu-24.04 / X64 / x86_64 | Published Controller, PostgreSQL, guarded in-place upgrade |
| controller-arm64 | ubuntu-24.04-arm / ARM64 / aarch64 | Published Controller, PostgreSQL, guarded in-place upgrade |

The host, local Docker daemon, image platform and executable ELF architecture
must agree. Active binfmt handlers and remote Docker daemons are refused.
Each disposable hosted runner first unregisters all preinstalled binfmt
handlers, including LLVM's runtime handler, then executes the unchanged
strict audit. This preparation is not run on shared BuildServer.
There is no component/architecture skip input. RPM names use x86_64/aarch64;
DEB metadata includes nfpm's `-1` revision, separate from the binary X.Y.Z.

Agent tests retain the old package's own production-relay installer. They
compare configuration, command/release trust, sealing keys, prepared endpoint
identity, relay configuration/token/drop-in, ownership and modes, execute
candidate binaries, reinstall identical packages, reject corrupt payloads
and unsafe upgrade prerequisites, and execute the installed rollback command.
Rollback restores runtime binaries/units, not the package-manager version.
An identical retry first uses the rollback command's `--verify-only` snapshot
validation: missing manifests, corrupt members or unsafe installed rollback
scripts fail before any retry restart. A broken snapshot is not silently
replaced with candidate files.
Unconfigured installs stay disabled. Restart requests during rollback are
not proof of healthy services or a fresh online Controller report.

Controller tests use clean exact old/candidate checkouts, unchanged signed
old manifests and digest-pinned images. A private fixture creates PostgreSQL
credentials, PKI, identity keys and an OIDC provider with a password-protected
principal and one-use PKCE codes. The fixture CA is mounted into only the
running test Controller's trust-store namespace; TLS verification and real
production authentication remain enabled. No host trust store is changed.
The native, pinned Node fixture container joins the unchanged internal
application network at its reserved test address; its host port is loopback
only. The CA is written inside the container's writable tmpfs before mounting,
not copied through Docker's read-only-root archive interface.
Relay/signer/OTLP fixture addresses do not certify those external protocols.

The old Controller creates the authenticated session and audited bootstrap
token. Its database contains a workspace, restricted workspace, role binding
and node inventory before the candidate touches it. The gate compares these
records, keeps the old session working, checks permission denial, writes a new
authenticated token, checks migration and production release smoke, and
verifies lifecycle state and identical-target idempotence. Stopping the local
candidate registry induces a real pull failure; the old confirmed state and
failed pending evidence must survive, then the identical manifest is retried.

Rollback must either succeed and revalidate the old version, or fail with the
specific changed-production-descriptor guard, corroborated by the source
diff without changing confirmed state. Other failures do not pass as expected
refusals. The old production backup worker creates a physical base backup
after old data exists; a separate PostgreSQL instance verifies and restores it.
This focused restore is not the full G6 PITR/failover matrix.

Excluded: database engine/major changes, MySQL/MariaDB historical upgrades,
all historical releases, all distributions, online Agent batches, cross-VM
or relay end-to-end behavior, full database regression/security/G6 acceptance.

## Build and trust boundaries

`build-release-agent.sh` and `build-release-controller.sh` are shared with
`release.yml`. Every cell builds one candidate package set or four production
image archives. Tests consume those exact files and retain their digests.
Agent builds use `build-agent-binaries.sh` and the digest-pinned native
Rocky 9 build container for all three common tar/DEB/RPM payload binaries.
The builder checks native architecture and glibc 2.34, isolates Cargo objects
from Ubuntu-built objects, and executes every binary before packaging.
The locked toolchain, Cargo locks and four Controller Dockerfiles are unchanged.
BuildKit exports archives without registry access; the test daemon loads and
pushes those archives to a registry bound only to `127.0.0.1:5000`. The actual
registry manifest digests, never image config IDs, enter the generated
candidate manifest. No candidate image is pushed externally.

Only source-read permissions are used. Signing keys are ephemeral, outside
the checkout/cache/artifacts. The original tag-only publisher, security
dependency, production re-signing and `release-publishing` approval remain
unchanged. Ordinary same-source native smoke remains a separate installer
regression, not cross-version evidence.

`release-upgrade-baselines.json` owns historical checksum pins and capabilities.
The default v0.6.0 is bound to commit
`cc8399641dc32083466a9c77369fcb8debf1ee48`, schema 36, checksum-manifest SHA-256
`26f4ab236630ff52777dabf5723cd3d5814022d4e08ab4079768117dd4e262df`, and DER key SHA-256
`b0156efe8c67273d773be595fa34546d086950961d8fa33b5f7bfe6297e80369`.
On 2026-09-14, the key was recovered from the real v0.4.0 arm64 DEB after
checking its digest against the already repository-pinned v0.4.0 SHA256SUMS.
That independent historical key verified v0.5.0 SHA256SUMS.sig. On 2026-09-15
the same anchor verified v0.5.2's signatures and all 21 published assets, both
Controller bundles, and anonymous dual-platform indexes. Immutable Release
388897497, direct tag commit and successful publication run 34934040575 were
cross-checked. On 2026-09-16, the same historical anchor verified v0.6.0's
signed checksums and all 21 published assets, both Controller bundles at
schema 36, and anonymous dual-platform image indexes. Immutable Release
388971765, direct tag commit and successful publication run 34945045708 were
cross-checked. The key was not accepted through trust-on-first-download.
v0.5.0, v0.5.1 and v0.5.2 remain registered as historical data, not final-baseline
recommendations or fallbacks. v0.5.1 fixed v0.5.0's PostgreSQL/gateway startup
defects, but its immutable RPM privd requires GLIBC_2.39 and cannot execute on
supported Rocky 9. The genuine v0.5.2 release fixes the common payload ABI;
both native release jobs execute all three binaries on glibc 2.34. Its
release/fresh-install acceptance does not replace this four-unit upgrade gate.

To register another baseline, independently establish its key, verify its
signed checksum manifest and both architectures' native packages and
Controller bundles, inspect its actual installation/authentication/database
contract, record the commit/capabilities/provenance, and review the data-file
change. Unregistered or incomplete releases fail before building. Do not
rebuild old sources or silently fall back to a different release.

## Evidence and reproduction

Artifacts are `upgrade-frozen-RUN-ATTEMPT` and one
`upgrade-COMPONENT-ARCH-RUN-ATTEMPT` per cell. The workflow requests seven days,
but repository policy currently caps retention at one day; download evidence
promptly. Each
cell includes `result.json`, scenario names, architecture proof, tested-file
hashes and small logs. Controller bundles, version responses and lifecycle
states contain no private key or database credential. No image archives or
database contents are uploaded by this workflow.

`Native Upgrade Result` requires successful prepare and both matrices plus
four unique, complete, matching result documents. Missing, failed, skipped,
cancelled, foreign-SHA/baseline/architecture or mixed-attempt evidence cannot
pass. Use **Re-run all jobs**: re-running only failed jobs cannot combine old
attempt artifacts into a new complete gate. Timing covers measured unit
execution, not GitHub queue time or billed-minute rounding. No cold-cache
duration guarantee is made.
Any new candidate commit, including documentation or test-only edits, requires
a fresh four-cell run **to claim native upgrade acceptance for that SHA**.
This is not a requirement to rerun the gate for every documentation-only PR;
existing evidence remains attached to its original SHA. Record final run links/results in the
PR and external evidence report rather than changing the tested commit merely
to embed its own SHA or run URL.

Run local checks through `ssh BuildServer`, in a fresh checkout. Use
`bash scripts/test-release-upgrade.sh`, Bash syntax, ShellCheck, actionlint,
and `bash scripts/docs-check.sh`. The focused
`test-agent-upgrade-retry.sh` requires an isolated root container and uses
stub binaries with DESTDIR; it is explicitly not upgrade acceptance.

Never run the host-installing Agent smoke on shared BuildServer. Reproduce it
on a new native systemd VM/runner. Controller reproduction must use a fresh
isolated Docker daemon: production fixes its Compose project name, and the
script refuses existing containers or volumes. Keep the same frozen inputs
and candidate artifacts. Preserve sanitized diagnostics before removing only
the resources created by that reproduction.
