# Native release upgrade validation

`Native Release Upgrade Validation` is an independent `workflow_dispatch`
workflow, not a publisher and not part of Basic CI. It accepts exactly these
five inputs:

| Input | Type / default | Meaning |
| --- | --- | --- |
| `version` | Required string | Candidate numeric X.Y.Z, strictly newer than the selected baseline |
| `baseline_release` | Required string; `v0.6.0` | Registered published baseline for prepare and native upgrades |
| `candidate_sha` | Required string | Exact full lowercase SHA of the dispatch branch |
| `session_compatibility` | Boolean; `false` | Also run both published v0.6.0/v0.6.1 application pairs on both architectures |
| `session_only` | Boolean; `false` | Run the four application cells instead of native upgrades, even when `session_compatibility=false` |

Choose the candidate branch in the Actions UI or with `gh --ref`. The SHA
must be the complete lowercase commit SHA of that branch and must equal
both the dispatch SHA and checkout HEAD. The candidate's numeric X.Y.Z
version must be strictly newer than the baseline.
These three identity inputs remain required in application-only mode: prepare
still freezes and verifies the selected registered baseline. The application
jobs always select both v0.6.0 and v0.6.1, independently of `baseline_release`.

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
tag/approval path. In default mode this workflow validates the registered old
release to exact candidate upgrade on four native units without publishing. Formal G6 is the
separate production-readiness/HA/PITR acceptance harness. None of these gates
substitutes for another or extends a backend's production support.

Dispatch modes are:

| `session_compatibility` | `session_only` | Jobs after prepare |
| --- | --- | --- |
| `false` | `false` | Four native upgrade units and `Native Upgrade Result` (default) |
| `true` | `false` | Native units/result plus four application cells |
| Either value | `true` | Four application cells only; both native matrices and `Native Upgrade Result` are skipped |

In native-upgrade mode, four mandatory cells run with fail-fast disabled:

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
Within native-upgrade mode there is no component/architecture skip input.
RPM names use x86_64/aarch64;
DEB metadata includes nfpm's `-1` revision, separate from the binary X.Y.Z.

Agent tests retain the old package's own production-relay installer. They
compare configuration, command/release trust, sealing keys, prepared endpoint
identity, relay configuration/token/drop-in, ownership and modes, execute
candidate binaries, reinstall identical packages, reject corrupt payloads
and unsafe upgrade prerequisites, and execute the installed rollback command.
Rollback restores runtime binaries/units, not the package-manager version.
Operator state remains byte-identical across upgrade, retry and rollback.
Package-owned Relay drop-ins and launchers are checked against the signed
candidate payload after upgrade, must remain unchanged on retry/rejection,
and must return to their exact baseline content or absence on rollback.
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

Excluded from native-upgrade evidence: database engine/major changes, MySQL/MariaDB historical upgrades,
all historical releases, all distributions, online Agent batches, cross-VM
or relay end-to-end behavior, full database regression/security/G6 acceptance.

### Optional published-node application matrix

Append `-f session_compatibility=true` to include application evidence with
the native gate, or `-f session_only=true` for application-fixture iteration.
Each native architecture builds candidate application images once and runs
both published node baselines without rebuilding their binaries. The second
baseline still runs after a first-baseline failure if the build succeeded and
the job was not cancelled. The independent PKI/config-rejection phase retains
its own exit code without hiding a session/reload/recovery failure.

See the [finite release matrix and adopted exclusions](../reference/stable-contracts.md#finite-release-matrix)
for topology, required workflows, the v0.6.0 uncertain-mutation recovery
boundary and excluded positive ConfigPlan apply for old nodes. The broader
strict recovery check is retained; a documented exclusion does not turn its
failed cell into PASS. Application-only success cannot satisfy native upgrade
requirements, and `Native Upgrade Result` does not aggregate application jobs.

## Build and trust boundaries

`build-release-agent.sh` and `build-release-controller.sh` are shared with
`release.yml`. Every native unit builds one candidate package set or four production
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
Its optional `deb_asset_release` must be a positive integer (not a string).
Omitting it selects the legacy `ocservia-agent_<version>_<arch>.deb` asset;
setting it to `1` selects `ocservia-agent_<version>-1_<arch>.deb`. Add it only
when registering a release actually published with the revisioned filename.
Existing baselines, including v0.6.1 and v0.6.2, retain their original metadata and asset
names, such as `ocservia-agent_0.6.2_amd64.deb`. Candidate packages always use
`-1`; their naming never determines the historical baseline filename.
The baseline smoke uses Node to share this resolver with upgrade prepare.

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

### v0.6.1 supplemental baseline

The registered v0.6.1 baseline is commit
`1805962fe1a98a22955b3105bfa8ebce7f2ea1eb`, schema 36, with checksum-manifest
SHA-256 `d562822bfdc55c784bf950a21c746f380a1cb6a7a2c86c6c801cfa25121df53b` and
the same independently anchored key fingerprint above. On 2026-09-18, its
signed checksums, both archive signatures, all 21 asset digests, both Controller
bundles and anonymous dual-platform indexes were verified using the historical
v0.4.0 package key. Immutable Release 389674680 and publication run 35061703465
were cross-checked. This metadata verification did not install or rebuild the
baseline and is not upgrade acceptance.

The existing v0.6.0 release smoke and manual default remain unchanged. To
cover the supplemental baseline, additionally dispatch this workflow with
`-f baseline_release=v0.6.1` and the exact candidate version/branch/SHA.
Require all four native units and the result gate on that same SHA. A v0.6.0
run cannot be reported as v0.6.1 upgrade evidence.

### Final-candidate identity

The published [v0.6.2 release](https://github.com/GentleKingson/ocservia/releases/tag/v0.6.2)
is registered at commit `518df6e9c488e58613c9cc194c896b4edfd57c2e`, PostgreSQL
schema 36 and OIDC authentication. Its checksum-manifest SHA-256 is
`a386f64d81f0ccb0b87482c3f4e4029d0a756b5e76c679b72d70d1afa23abc66`;
the signing key retains the independently anchored historical fingerprint above.
The baseline registry records its verification provenance. This registration
establishes artifact identity, not successful installation or upgrade.

A disposable T05 build labeled `version=0.6.2` from a
different source SHA is not that published release or a final T06 candidate.
For T06, explicitly select `-f baseline_release=v0.6.2` rather than the historical
manual default, then upgrade from those signed artifacts to the final frozen
candidate's actual numeric version (strictly newer than 0.6.2) and exact source SHA.
Prerelease strings such as `1.0.0-rc.1` are not accepted version inputs. Require fresh
Agent/Controller x amd64/arm64 evidence; T05's earlier eight native units do
not replace this gate. Do not infer artifact identity from a version string.

To register another baseline, independently establish its key, verify its
signed checksum manifest and both architectures' native packages and
Controller bundles, inspect its actual installation/authentication/database
contract, record the commit/capabilities/provenance, and review the data-file
change. Unregistered or incomplete releases fail before building. Do not
rebuild old sources or silently fall back to a different release.

## Evidence and reproduction

Artifacts are `upgrade-frozen-RUN-ATTEMPT`, one
`upgrade-COMPONENT-ARCH-RUN-ATTEMPT` per native unit, and optional
`session-ARCH-RUN-ATTEMPT` per application architecture, containing both
baseline directories and their `compatibility-result.json` phase outcomes.
The workflow requests seven days, but actual artifact expiry may be shorter;
check the API's `expires_at` and download evidence promptly. Keep structured
summaries, digests and a controlled archive, not just expiring artifact URLs.
Each native unit includes `result.json`, scenario names, architecture proof, tested-file
hashes and small logs. Controller bundles, version responses and lifecycle
states contain no private key or database credential. No image archives or
database contents are uploaded by this workflow.

When `session_only=false`, `Native Upgrade Result` requires successful prepare and both native matrices plus
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
