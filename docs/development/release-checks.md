# Release policy

Every release qualifies through a passing CI run along this pipeline:

```text
PR -> Basic CI -> merge main
   -> manual Release Check
      - Full CI
      - Security
      - amd64 and arm64 Release products: build + Controller image security
        + package/image smoke (same reusable workflow as Release)
      - amd64 and arm64 Business Smoke + four finite single-instance recoveries
      - amd64 and arm64 Quick Install against a Pebble ACME directory
   -> PASS -> operator version confirmation -> vX.Y.Z-rc.N or vX.Y.Z tag
   -> Release
      - amd64 build + Controller image security + install/image smoke
      - arm64 build + Controller image security + install/image smoke
      - require a successful Release Check for the exact tag/build SHA
      - publish Agent assets and Controller GHCR images only after these gates pass
```

On pull requests, `Basic CI Result` remains the required check. Once changes
merge to `main`, an operator triggers Release Check manually via
`workflow_dispatch`. Every required job in that workflow must pass; a failure,
cancellation, or unexpected skip blocks qualification, and each run executes
from scratch without inheriting earlier results or nominating a persistent
candidate. Within Release Check, Full CI refers to the Basic CI Full profile
(current-candidate checks across supported units; see
[Basic CI](github-actions.md)), not comprehensive acceptance.

Once Release Check succeeds on the target commit, the operator confirms the
version and pushes the version tag. The tag-triggered Release workflow builds directly from
the tag, scans the built Controller image archives on both native
architectures, and runs install and image smoke checks before publishing.
Inside the existing `release-publishing` environment, before any image or Release
write, `publish` verifies that the current tag resolves to the build SHA and
queries `release-check.yml` runs for that exact SHA on `main`, triggered by
`workflow_dispatch`. The latest run must be completed successfully; missing,
failed, cancelled, or unfinished qualification and API errors block publication,
including reruns. An older PASS cannot override a newer unsuccessful run. The
accepted run URL and SHA are recorded in the job summary. Build-only dispatches
remain exempt, and Release does not rerun Full CI, Security, or Business acceptance.

Three-VM acceptance remains independent of this automated gate. For the next RC,
the operator must record its run/evidence reference and verified source SHA
alongside Release Check and the tag commit, and confirm all three SHAs match
before approving publication. Decide whether to make it a hard gate only after
the real acceptance runs are stable.

Published release artifacts consist of Agent archives, plain archive checksums,
DEB and RPM packages, Controller container images, and the bootstrap files
consumed by the installers. Plain checksums guard against download corruption
without acting as cryptographic signatures or a trust root, and
`controller-release.json` is the standard deployment configuration for
Integrated lifecycle consumers. Publication is one-shot: an existing Release
(including a draft) or any exact-version GHCR tag blocks publication. No asset
upload uses `--clobber`, and a rerun never replaces an existing version.

During SQL artifact builds, the tooling reads the current `schema.sql` headers
to record PostgreSQL `database_migration` as `{"epoch": N, "revision": R}`, and
the source commit binds the matching schema and upgrade files. Existing
launchers also accept the positive integer format found in earlier published
manifests, so generating a new manifest does not require historical migration
files.

On the node side, AgentUpgrade keeps the Controller-authorized
`target_version`, `package_sha256`, and `architecture` fields. Before running
protected staging, architecture verification, safe extraction, and service
lifecycle transitions, the upgrade path checks the downloaded archive's SHA-256
against the command and aborts on any mismatch. All standard command guarantees
remain in force: runtime command authorization, independent two-principal approvals,
owner/epoch/fence/lease checks, idempotency, the Agent SQLite journal, privd
receipts, Unknown outcome reconciliation, semantic payload hashes,
command/fence/receipt signatures, and durable immutable operation intent. The
operator-provisioned release catalog continues to supply the authorized upgrade
package digests.

### Immutable publication and recovery

Before the first publication with this workflow, a repository administrator must
select **Settings → General → Releases → Enable release immutability** (see
[GitHub's configuration guide](https://docs.github.com/en/code-security/how-tos/secure-your-supply-chain/establish-provenance-and-integrity/prevent-release-changes)).
This protects future releases only; it does not retrofit historical releases.
The setting API requires Administration permission, which the publishing
`GITHUB_TOKEN` does not have. Keep the existing job permissions: do not add an
admin token. Check the setting before approving the `release-publishing`
environment. The workflow verifies `immutable: true` after publication and fails
if GitHub does not enforce it. A failure at that final check can leave a public,
mutable Release; it is not a successful publication and must be investigated.

After the same-SHA Release Check gate and both native product builds, publication:

1. Requires all 14 expected assets and all 16 image archives. Lists all GitHub
   Releases, including drafts, and probes all 24 GHCR tags (eight roles, each with
   `vVERSION`, `vVERSION-amd64`, and `vVERSION-arm64`). Only an explicit registry
   `404 MANIFEST_UNKNOWN` means an absent tag; authentication, rate-limit,
   transport, malformed-response, and server errors fail closed.
2. Creates a new Draft with generated notes and the exact build SHA. Note cleanup
   happens while the Release is still a draft.
3. Uploads without replacement and verifies the remote asset inventory, uploaded
   state, size, and GitHub-provided SHA-256 against every local file.
4. Rechecks each image tag immediately before writing it. Verifies each pushed
   image's configuration digest against the scanned archive's loaded image ID,
   then builds the version index from those registry digests and requires both
   `linux/amd64` and `linux/arm64` entries. A Docker push reporting `unknown blob`
   gets at most two retries, after 5 and 15 seconds, against the same loaded
   image. Each retry first requires the tag to remain absent; a written tag or
   registry lookup error stops publication. Other push errors are not retried.
   Failed commands retain both output streams for diagnosis, and the publishing
   job records the Docker, storage-driver and Buildx versions.
5. Rechecks assets, all image tag digests, draft identity, and the Git tag SHA,
   then publishes the Draft. Success requires a published immutable Release,
   complete verified assets, unchanged image digests, and RC exclusion from Latest.

The per-ref Actions concurrency group serializes same-tag runs without cancelling
an active publication. GHCR has no atomic create-only tag operation in this flow:
its checks protect this workflow's writes, but cannot prevent an independent
writer racing the check or changing tags later. Restrict package write access to
this publication path. GitHub Release immutability does not protect GHCR tags.
There is no cross-service transaction: a failure can leave a Draft and some GHCR
tags. The workflow does not delete, overwrite, or automatically resume those
objects. Preserve the failed run evidence, inspect the partial state, and use a
new version with its own qualification. Do not delete and recreate a released
version to retry. The next RC compares against the highest-numbered earlier
published RC of the same version series, skipping failed drafts and unused
numbers. For example, if RC.6 failed after creating a draft, RC.7 uses published
RC.5 as its notes base; RC.6 remains untouched.

If no Release or version image tags were created, rerunning failed jobs is safe
while the required artifacts remain available. Consumers download the immutable
artifact IDs returned by the successful upstream jobs, including jobs retained
from an earlier attempt of this same run. They do not reconstruct names using
the consumer's incremented attempt number. Missing IDs fail closed; expired
artifacts require a full rerun. Once a draft or version image tag exists, even
a full rerun is blocked by the one-shot preflight and requires a new version.

GitHub automatically creates its platform release attestation for an immutable
Release. This flow adds no separate signing service, key management, or custom
attestation system. Plain archive checksums retain their existing role.

## Database checkpoint release boundary

Stable v1.2.0 is the single prior database checkpoint; its fixed source commit,
epoch and revision numbers, and schema receipts are specified in the
[database migration contract](control-plane.md#current-sql-artifacts-and-bounded-upgrades).
Databases on older releases must upgrade to v1.2.0 before moving to epoch 2,
and commits landing on main do not shift that checkpoint.

Any major database change must pass the
[SQL artifact acceptance checks](testing.md#sql-artifact-acceptance),
including fresh-install versus checkpoint-upgrade equivalence and both
backup/restore smoke paths. While a build-only Release run can validate package
construction ahead of time, a major release still requires a passing Release
Check on the exact merged main commit before the operator tags the release;
branch diagnostic runs and earlier release results do not satisfy that gate.

## Release notes and changelog

GitHub Releases serve as the primary user-facing record for each version,
bringing together change summaries, merged pull requests, downloadable assets,
upgrade notes, and known issues. The Release workflow generates notes through
`gh release create --generate-notes` using the category rules in
[`.github/release.yml`](../../.github/release.yml):

| Category | Label |
| --- | --- |
| Breaking changes | `release/breaking` |
| Security | `release/security` |
| Controller | `area/controller` |
| Agent | `area/agent` |
| Relay & Transport | `area/relay` |
| Database & Backup | `area/database` |
| Authentication & Authorization | `area/auth` |
| Web & API | `area/web` |
| Deployment & Operations | `area/deployment` |
| Dependencies | `area/dependencies` |
| Other changes | Unmatched PRs |

Create these labels in the repository before using them. Area labels are
optional. Reserve `release/internal` for changes that have no user-visible
effect (such as test refactoring or CI maintenance), since applying it omits
the pull request from generated notes even when other labels are present; never
attach `release/internal` to breaking changes or user-facing security fixes.

After GitHub generates the initial notes body (`gh release create
--generate-notes` with [`.github/release.yml`](../../.github/release.yml)),
[`scripts/strip-release-owner-attribution.py`](../../scripts/strip-release-owner-attribution.py)
removes the repeated ` by @GentleKingson` suffix from PR list items while
leaving external contributor attributions, PR links, category headings, manual
notes, and the `Full Changelog` link untouched. Because the workflow compares
the stripped text against the current release body before calling
`gh release edit --notes-file`, cleanup is idempotent within draft preparation.
Existing releases are rejected before note preparation on reruns. Do not add
`GentleKingson` to `changelog.exclude.authors` in
`.github/release.yml`, as GitHub would drop those pull requests from the notes
altogether. Historical releases that are already published are left as-is.

Pull requests also include an optional one-sentence release note field in the
[PR template](../../.github/PULL_REQUEST_TEMPLATE.md). Because GitHub's
automatic generator reads PR titles and labels instead of the PR body, write
clear user-facing PR titles and use the template field when drafting manual
highlights or upgrade guidance. Leaving the field blank is fine, and no custom
parser or label-check job enforces it in CI.

[`CHANGELOG.md`](../../CHANGELOG.md) tracks concise, durable summaries of
stable releases while keeping older entries intact. For new releases, omit
empty headings, link to the corresponding GitHub Release instead of duplicating
its PR list or workflow logs, and include Highlights, Upgrade notes, Known
issues, or a brief Validation note only when backed by actual results (leaving
detailed run evidence in GitHub Actions).

None of these note-formatting rules alter Release Check qualification or the
native build, image-security, smoke, and publication gates. Release candidates
must use `X.Y.Z-rc.N` where N is a positive integer without leading zeros; any
other prerelease tag or build metadata is rejected. Publishing an RC passes
`--prerelease --latest=false` and pushes only the exact RC image tags, while
manual workflow dispatches never publish assets or images.

For `v2.0.0-rc.2`, `v2.0.0-rc.3` and `v2.0.0-rc.4`, the workflow prepends the
corresponding checked-in notes ([RC.2](../../.github/release-notes/v2.0.0-rc.2.md),
[RC.3](../../.github/release-notes/v2.0.0-rc.3.md),
[RC.4](../../.github/release-notes/v2.0.0-rc.4.md)) using
GitHub's `--notes` option alongside `--generate-notes`. This preserves the
generated categories, PR list, contributor credits and comparison link.
Each supplement applies only to its exact version; reruns leave existing Release
bodies and assets untouched.

When generating release notes, `rc.1` and final stable releases diff against
the most recently published non-draft stable Release (ignoring the target
release itself on reruns). For `rc.N` with N > 1, the highest-numbered lower RC
in the same `X.Y.Z` series must exist as a published prerelease. Drafts, the
current or later RC, and other series are excluded, with all release pages
searched. Failed or unused numbers may be skipped; no published earlier RC or
an API error fails the workflow before image publication. This model
follows a single linear release train instead of sorting semantic versions
across parallel maintenance branches. Keep published RC releases in history,
create a dedicated commit and tag for the final release so installers observe a
single release identity at HEAD, and run a fresh Release Check on the exact
commit for every RC and final tag.

While archive filenames and compiled binaries report `X.Y.Z-rc.N`, nFPM maps
native package versions to `X.Y.Z~rc.N-1` for DEB and Version `X.Y.Z~rc.N`,
Release `1` for RPM. Package validation separates three distinct scopes:

1. Same-source relabeled native package lifecycle: when invoked without
   `PRODUCTS_DIR`,
   [`scripts/release-native-package-smoke.sh`](../../scripts/release-native-package-smoke.sh)
   rebuilds the current checkout under synthetic version labels (`1.0.0 -> 1.0.1`
   by default, or `1.2.0-rc.1 -> 1.2.0-rc.2 -> 1.2.0` when `RC_LIFECYCLE=true`)
   on a disposable native systemd runner:

   ```bash
   RC_LIFECYCLE=true RUN_ID=<unique-id> ARTIFACT_DIR=<path> \
     scripts/release-native-package-smoke.sh
   ```

   Use `STUB_BINARIES=false` to exercise real compiled binaries;
   `STUB_BINARIES=true` is reserved for fast scriptlet checks in PR CI. Running
   the fixed `1.2.0` sequence verifies DEB and RPM `~rc.N` version sorting,
   upgrade and rollback state snapshots, and package scriptlet transitions,
   though relabeling a single source tree does not prove cross-release binary
   upgrade compatibility for an upcoming RC.
2. Current candidate product build and install smoke: inside
   [`.github/workflows/release-products.yml`](../../.github/workflows/release-products.yml),
   passing `PRODUCTS_DIR` and `VERSION` feeds the candidate packages built for
   `VERSION` into `scripts/release-native-package-smoke.sh` on native `amd64`
   and `arm64` runners, checking ELF architecture identity, fresh installation,
   same-version reinstall and upgrade, rollback, package removal, state
   preservation, and rejection of corrupted archives.
3. Cross-historical-release compatibility: outside the promises of the
   [support policy](../reference/support-policy.md) and not certified by either
   smoke check above.

To rehearse an RC build across `amd64` and `arm64` (running both the candidate
product smoke for `version` and the `RC_LIFECYCLE=true` sequence while skipping
`publish`), trigger:

```bash
gh workflow run release.yml --ref <candidate-branch> -f version=<X.Y.Z>-rc.1
```

## Coverage ownership

### Published installation assets

After the next RC's Release workflow has finished uploading assets, use the
existing Release Diagnostics entrypoint on a disposable native runner:

```bash
gh workflow run release-upgrade.yml --ref <harness-ref> \
  -f purpose=release-assets -f version=<X.Y.Z-rc.N> -f arch=amd64
```

`harness-ref` must contain the published-asset smoke. `version` is the already
published version without `v`; `latest`, branch names, and the local `0.0.0`
fixture are rejected. Select `arch=arm64` for a separate native arm64 run if
that architecture needs acceptance. No Controller/Relay or three-VM deployment
is created, and this diagnostic never builds or publishes assets.

[`release-asset-smoke.sh`](../../scripts/release-asset-smoke.sh) downloads the
exact Release's Agent archive, SHA-256 sidecar, DEB, RPM and managed-node
bootstrap over HTTPS. It verifies the archive checksum and compares the
bootstrap with the same-tag raw source used by the Nodes wizard. It runs the
existing managed-node release contracts against that downloaded script
(download-failure and staging-digest fault injection), then feeds the actual
downloaded packages to the existing native installation Smoke, including real
binary version/architecture checks and corrupted embedded-payload rejection.
The native packages carry their archive checksum internally; the recorded
DEB/RPM download hashes are evidence, not separate publisher signatures.

The run retains the requested tag, harness SHA, architecture, exact URLs,
downloaded hashes, installer contract log and native installation logs for seven
days. `release-assets-summary.txt` reports `RELEASE_ASSET_DOWNLOAD=PASS` only
after every check succeeds; download, checksum, source mismatch, skipped
contracts or installation failure fails the run. Preserve the Actions run URL
and evidence for the exact RC before closing the release-asset validation gap.

This verifies published downloads and native package installation. Installer
fault injection uses mocked host commands and is labeled separately; it does
not repeat browser enrollment, Controller approval or live Relay connectivity.
The three-VM wizard run's `RELEASE_ASSET_DOWNLOAD=NOT_TESTED` remains accurate
for its local same-SHA `v0.0.0` inputs. A future smoke entrypoint or an offline
self-test alone does not close that gap: a successful published-RC run is still
required.

### Existing checks

| Behavior | Owner |
| --- | --- |
| Native bootstrap-token registration and online node, browser login/Workspace/user creation/approval/reload/ConfigPlan apply, production Signer sealing, real VPN and internal TLS | Integrated Business Smoke, amd64 and arm64 with the same checkpoint gate |
| Controller/transport, Agent/privd, bundled PostgreSQL and sole Relay recovery (no Business-level MySQL recovery) | Business Smoke with `run-resilience=true` |
| Terminal real-node revocation: rejected unauthorized/unapproved requests, approved revoke with synchronous trust update and close, disconnect and no new privd effect from old authority | Business Smoke and manual integration |
| OIDC positive/negative paths; native session disconnect and IP-ban removal; CSR, issue, one-use P12, revoke and persistence (including browser checks); offline queue and exact root effects; one-node browser canary rollout to a locally built next Agent version | Manual integration |
| DEB/RPM install, retry/removal, state preservation and unsafe-package rejection on amd64/arm64 | Native package build/install smoke (Release Check and Release) |
| Native Controller execution and exact archive OS vulnerability scans before smoke/upload, both architectures | Release Controller products (Release Check and Release) |
| Source/dependency vulnerabilities and repository secrets, within the [tool scope](github-actions.md#independent-security-checks) (Go advisories cover `control-plane` and `signer`) | Security |
| Tagged-source build and GitHub/GHCR publication | Release |
| Fixed-tag GitHub Release download, published checksum, native installation and rejection | Release Diagnostics with `purpose=release-assets` after publication |

Business and Quick Install workflows build candidate artifacts locally and
install from a loopback registry on disposable native runners of the selected
architecture; they first require the runner, kernel, dpkg and Docker
architectures to match and check every loaded Controller image's platform. Native package checks require
the host kernel, Docker daemon, container image, and executed ELF binaries to
share the same architecture, so cross-compilation alone never counts as native
acceptance. Those checks enforce architecture and version matching, root-owned
staging permissions, and SHA-256 verification. Never install test packages on a
shared machine or clear its binfmt handlers. Passing the current suites does
not certify upgrades from historical releases or mixed-version clusters.

## Diagnostics and build rehearsal

### Native platform diagnostics

The dispatch-only `release-upgrade.yml` purposes below are separate from Business,
Basic CI, Release Check and release publication. They build the current source's
real Agent package and adapter test ELF on matching GitHub-hosted runners, verify
the fixed [ocserv-backport 1.5.0-2 release](https://github.com/GentleKingson/ocserv-backport/releases/tag/1.5.0-2)
metadata, `SHA256SUMS`, package digest and dpkg fields, then run the existing I13
authentication, password rotation, disable/enable, group, occtl parsing, terminate,
concurrency and file-permission fixture. Download, dependency or validation
failures fail the job; no emulation or mock counts as native evidence.

```bash
gh workflow run release-upgrade.yml --ref <candidate-branch> -f purpose=native-ubuntu-arm64
gh workflow run release-upgrade.yml --ref <candidate-branch> -f purpose=native-debian13
```

Ubuntu uses `ubuntu-24.04-arm`. Debian uses `ubuntu-24.04` and
`ubuntu-24.04-arm`, each running the same-architecture official
`docker.io/library/debian:trixie-slim` index pinned at
`sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f`.
Only the isolated Debian container receives TUN and `NET_ADMIN`; it has no host
network, Docker socket or systemd. Its Agent DEB dependencies, layout and verified
payload executables are checked without invoking systemd-dependent scriptlets.
This proves PACKAGE and limited NATIVE_RUNTIME, and an executed I13 fixture
additionally proves adapter NATIVE_OPERATIONS. It proves neither Debian systemd
service lifecycle nor CONTROLLER_E2E. Those require a separately authorized native
VM and complete Business acceptance. Ubuntu I13 also does not prove Controller E2E.

Evidence artifacts retain the run ID/attempt, source SHA, kernel/Docker
architecture, package metadata/digests, runtime log, I13 summary and cleanup result
for seven days. The adapter libtest ELF may be passed explicitly through
`OCSERVIA_I13_TEST_BINARY` so Debian needs no second Rust toolchain; the I13
driver first checks that it contains exactly the requested ignored test. This
option is for matching native ELF binaries only. Platform diagnostics do not
broaden the [support policy](../reference/support-policy.md) or replace a
candidate's formal Release Check. No Controller, database or production secrets
are involved.

To run the extended [Business integration](real-business-validation.md) suite
on a branch:

```bash
gh workflow run release-upgrade.yml --ref <branch> \
  -f version=0.0.0 -f purpose=integration \
  -f production_signer=true -f run-resilience=true
```

`arch` (default `amd64`) selects the native `ubuntu-24.04` or
`ubuntu-24.04-arm` runner. Setting `purpose=smoke` narrows the run to the Smoke scope regardless of Signer
mode, while `production_signer=false` switches to the standalone topology.
The default `ocserv_package=distribution` keeps the runner's Ubuntu ocserv;
`ocserv_package=backport-1.5.0` installs the pinned, SHA256-checked
[ocserv-backport](https://github.com/GentleKingson/ocserv-backport/releases/tag/1.5.0-2)
1.5.0 package instead, and the run then also requires the session terminate
checkpoints described in [Business integration](real-business-validation.md).
Diagnostic runs never publish releases, push to GHCR, or read production
Secrets, and they do not substitute for Release Check.

To rehearse a build-only release without publishing:

```bash
gh workflow run release.yml --ref main -f version=0.0.0
```

In a rehearsal run, the native Agent and Controller build, image-security, and
smoke jobs must pass alongside asset preparation, and the Publish job must be
skipped. For local validation of release changes, run
`scripts/test-release-upgrade.sh` (which invokes
`scripts/test-release-workflows.rb`), the package and installer test scripts,
ShellCheck (`scripts/lint.sh`), any local workflow linter you have installed
(the repository does not bundle actionlint), and the
[documentation checks](testing.md). The release self-tests include
`scripts/test-release-publication.py`, an offline `0.0.0-rc.1` publication
rehearsal with GitHub/GHCR doubles. It checks collisions, partial uploads, image
and index mismatches, API failures, and missing immutability without making
external writes; it is not evidence of a real registry push or native build.
When modifying upgrade or bootstrap paths,
include tests for authorized AgentUpgrade digest acceptance and rejection as
well as Stage-0 download and checksum failures. Never publish release assets or
touch production deployments during validation.

If a validation suite fails, rerun its owning workflow with the full set of
checks (for recovery failures, rerun the entire Business job). Failed,
cancelled, missing, or unexpectedly skipped required checks block qualification,
and build caches speed up execution without serving as accepted release
products or qualification evidence. Save the run evidence first, and then clean
up only the resources created for that task.
