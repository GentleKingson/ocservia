# Release policy

Every release qualifies through a passing CI run along this pipeline:

```text
PR -> Basic CI -> merge main
   -> manual Release Check
      - Full CI
      - Security
      - amd64 Business Smoke + four finite single-instance recoveries
      - amd64 Quick Install against a Pebble ACME directory
   -> PASS -> operator version confirmation -> vX.Y.Z-rc.N or vX.Y.Z tag
   -> Release
      - amd64 build + Controller image security + install/image smoke
      - arm64 build + Controller image security + install/image smoke
      - publish Agent assets and Controller GHCR images only after both pass
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
version and pushes the version tag. That operator step carries the CI
qualification boundary: the tag-triggered Release workflow builds directly from
the tag, scans the built Controller image archives on both native
architectures, and runs install and image smoke checks before publishing,
without rerunning Full CI, Security, or Business acceptance or querying
previous workflow runs.

Published release artifacts consist of Agent archives, plain archive checksums,
DEB and RPM packages, Controller container images, and the bootstrap files
consumed by the installers. Plain checksums guard against download corruption
without acting as cryptographic signatures or a trust root, and
`controller-release.json` serves as standard deployment configuration for
Integrated lifecycle consumers. Rerunning a release replaces GitHub Release
assets and updates version image tags in place; the pipeline does not maintain
separate artifact signing, provenance attestations, registry bindings, or
immutable-release guarantees.

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
remain in force: runtime command authorization, multi-principal approvals,
owner/epoch/fence/lease checks, idempotency, the Agent SQLite journal, privd
receipts, Unknown outcome reconciliation, semantic payload hashes,
command/fence/receipt signatures, and durable immutable operation intent. The
operator-provisioned release catalog continues to supply the authorized upgrade
package digests.

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
`gh release edit --notes-file`, both first-time publication and reruns are
idempotent. Do not add `GentleKingson` to `changelog.exclude.authors` in
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

When generating release notes, `rc.1` and final stable releases diff against
the most recently published non-draft stable Release (ignoring the target
release itself on reruns). For `rc.N` with N > 1, the immediately preceding RC
must already exist as a published prerelease; skipping RC numbers is unsupported
and a missing base fails the workflow before image publication. This model
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

| Behavior | Owner |
| --- | --- |
| Native online node, browser login/Workspace/user creation/approval/reload/ConfigPlan apply, production Signer sealing, real VPN and internal TLS | amd64 Integrated Business Smoke |
| Controller/transport, Agent/privd, bundled PostgreSQL and sole Relay recovery (no Business-level MySQL recovery) | Business Smoke with `run-resilience=true` |
| OIDC positive/negative paths; CSR, issue, one-use P12, revoke and persistence (including browser checks); offline queue and exact root effects; one-node browser canary rollout to a locally built next Agent version | Manual integration |
| DEB/RPM install, retry/removal, state preservation and unsafe-package rejection on amd64/arm64 | Native package build/install smoke |
| Native Controller execution and exact archive OS vulnerability scans before smoke/upload, both architectures | Release Controller products |
| Source/dependency vulnerabilities and repository secrets, within the [tool scope](github-actions.md#independent-security-checks) (Go advisories cover `control-plane` and `signer`) | Security |
| Tagged-source build and GitHub/GHCR publication | Release |

Business workflows build candidate artifacts locally and install from a
loopback registry on disposable amd64 runners. Native package checks require
the host kernel, Docker daemon, container image, and executed ELF binaries to
share the same architecture, so cross-compilation alone never counts as native
acceptance. Those checks enforce architecture and version matching, root-owned
staging permissions, and SHA-256 verification. Never install test packages on a
shared machine or clear its binfmt handlers. Passing the current suites does
not certify upgrades from historical releases or mixed-version clusters.

## Diagnostics and build rehearsal

To run the extended [Business integration](real-business-validation.md) suite
on a branch:

```bash
gh workflow run release-upgrade.yml --ref <branch> \
  -f version=0.0.0 -f purpose=integration \
  -f production_signer=true -f run-resilience=true
```

Setting `purpose=smoke` narrows the run to the Smoke scope regardless of Signer
mode, while `production_signer=false` switches to the standalone topology.
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
[documentation checks](testing.md). When modifying upgrade or bootstrap paths,
include tests for authorized AgentUpgrade digest acceptance and rejection as
well as Stage-0 download and checksum failures. Never publish release assets or
touch production deployments during validation.

If a validation suite fails, rerun its owning workflow with the full set of
checks (for recovery failures, rerun the entire Business job). Failed,
cancelled, missing, or unexpectedly skipped required checks block qualification,
and build caches speed up execution without serving as accepted release
products or qualification evidence. Save the run evidence first, and then clean
up only the resources created for that task.
