# Release policy

CI PASS is the only release qualification. The flow is:

```text
PR -> Basic CI -> merge main
   -> manual Release Check
      - Full CI
      - Security
      - amd64 Business Smoke + four finite single-instance recoveries
   -> PASS -> operator version confirmation -> vX.Y.Z-rc.N or vX.Y.Z tag
   -> Release
      - amd64 build + Controller image security + install/image smoke
      - arm64 build + Controller image security + install/image smoke
      - publish Agent assets and Controller GHCR images only after both pass
```

`Basic CI Result` remains the required PR check. Release Check is a manual
`workflow_dispatch` on merged `main`; every required job must succeed. Failure,
cancellation, or an unexpected skip cannot report PASS. Each invocation runs the
checks anew. There is no inherited acceptance or candidate nomination.

After Release Check passes, the operator confirms the version and creates the
version tag. CI qualification belongs to that operator process. The tag workflow
builds from the tag, scans the exact Controller image archives on each native
architecture, and performs install/image smoke checks before publication;
it does not rerun Full CI, Security, or Business acceptance, or look up previous
workflow results or artifacts to establish release eligibility.

Release products are Agent archives, plain archive checksums, DEB/RPM packages,
Controller images, and the bootstrap assets their installers consume. A plain
checksum detects download corruption; it is not a signature or trust root.
`controller-release.json`, while needed by Integrated lifecycle consumers, is
ordinary deployment configuration. Release publication uses normal GitHub asset
replacement on reruns and version image tags. It maintains no separate signing,
provenance, registry binding, or immutable-release contract.

AgentUpgrade retains the Controller-authorized `target_version`,
`package_sha256`, and `architecture`. The upgrade verifies the archive SHA-256
against that command, refuses a mismatch, and then uses existing protected
staging, architecture checks, safe extraction, and lifecycle execution. Runtime
command authorization, approvals, owner/epoch/fence/lease, idempotency, the Agent
SQLite journal, privd receipts, Unknown reconciliation, semantic payload hashes,
command/fence/receipt signatures, and durable immutable operation intent remain
required. The operator-provisioned release catalog still supplies authorized
upgrade package digests.

## Database checkpoint release boundary

Phase A keeps one epoch-1 baseline per engine: PostgreSQL legacy revision 41
and MySQL legacy revision 31 bridge genuine old receipts into `schema_revisions`.
The active SQL pair is `schema.sql` plus `upgrade.sql`; new fresh databases stamp
one checkpoint and leave legacy journals empty. Retained SQL/JSON/descriptors
serve only the bounded bridge and its validation. The immutable bridge sources
are listed in `docs/database-migrations.sha256`.

Review and merge the six Phase A changes in dependency order: contract,
PostgreSQL bridge, MySQL bridge, PostgreSQL executor, MySQL executor, then
policy/CI/operations. An open stack must pass full Basic CI, Security Checks,
Native Business Diagnostics with production Signer and resilience, and both
backup/restore recovery jobs at its final head. These are review evidence;
Release Check itself remains main-only. After separately approved merges,
qualify the exact main commit and publish a real checkpoint release through the
existing release process. A build-only dispatch is not a checkpoint release.

Stop before deleting legacy sources or declaring the next epoch until that
release exists and its two journals and checksums are fixed. The next major's
sole previous-checkpoint window must reference that actual release and its
verified receipts. Never invent a tag, treat an RC/build-only run as this release,
merge the open stack automatically, or enable auto-merge to bypass the boundary.

## Release notes and changelog

GitHub Releases are the user entry point for each version: detailed changes,
pull requests, assets, and relevant upgrade notes or known issues. The Release
workflow retains native `gh release create --generate-notes`, configured by
[`.github/release.yml`](../../.github/release.yml). It groups merged PRs by labels:

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

Create these labels in the repository before applying them. Area labels are
optional. Use `release/internal` only for changes with no user-visible impact,
such as test refactors or CI-only maintenance; it excludes the PR from automatic
notes, including when other labels are present. Do not apply it to user-facing
security fixes or breaking changes.

The [PR template](../../.github/PULL_REQUEST_TEMPLATE.md) offers an optional,
one-sentence user-facing Release note. GitHub's generator uses PR titles and
labels, not this field; use descriptive user-facing titles and consult the field
when curating highlights or upgrade guidance. Empty notes are allowed, with no
custom parser or required-label CI check.

[`CHANGELOG.md`](../../CHANGELOG.md) keeps durable summaries of stable releases.
Start the concise format with the next release and preserve historical entries.
Omit empty sections and link to the full GitHub Release rather than copying its
PR list or workflow run evidence. Add Highlights, Upgrade notes, Known issues,
or a short Validation statement only when relevant and supported by actual
results; detailed evidence stays in Actions.

These presentation rules do not change Release Check qualification or the
native build, image scan, smoke, and publication gates. Supported candidates use
`X.Y.Z-rc.N`, where N is positive with no leading zeros; other prereleases and
build metadata are rejected. RC publication sets `--prerelease --latest=false`
and writes only exact RC image tags. Dispatch never publishes.

The notes base is the most recently published non-draft stable Release for
`rc.1` and final (excluding the current Release on a rerun). For `rc.N`, N > 1,
the immediately preceding RC must exist as a published prerelease. Missing
bases fail before image publication; skipping RC numbers is not supported.
This follows a linear release train, not semantic-version sorting across
parallel maintenance branches. Final needs its own commit and tag so installers
still see exactly one release identity at HEAD. Retain RC Release history.
Every candidate and final needs a fresh Release Check on its exact commit.

Package filenames and binary versions use `X.Y.Z-rc.N`; nFPM encodes native
metadata as `X.Y.Z~rc.N-1` (DEB) and Version `X.Y.Z~rc.N`, Release `1` (RPM).
To exercise three real packages on a disposable native systemd runner, use
`RC_LIFECYCLE=true RUN_ID=<unique-id> ARTIFACT_DIR=<path>
scripts/release-native-package-smoke.sh` with the native package tools prepared.
It builds `1.2.0-rc.1`, `1.2.0-rc.2`, and `1.2.0` and checks both upgrade steps.
Keep `STUB_BINARIES=false` for release acceptance; stub runs are scriptlet tests.
Run on both native architectures. A build-only RC rehearsal uses
`gh workflow run release.yml --ref <candidate-branch> -f version=1.2.0-rc.1`;
all product and asset jobs must pass and Publish must be skipped.

## Coverage ownership

| Behavior | Owner |
| --- | --- |
| Native online node, independently authenticated requester/approver, ConfigPlan apply, production Signer sealing, real VPN and internal TLS | amd64 Integrated Business Smoke |
| Controller/transport, Agent/privd, database and sole Relay recovery | Business Smoke with `run-resilience=true` |
| OIDC positive/negative paths; CSR, issue, one-use P12, revoke and persistence; real browser; offline queue and exact root effects | Manual integration |
| DEB/RPM install, retry/removal, state preservation and unsafe-package rejection on amd64/arm64 | Native package build/install smoke |
| Native Controller execution and exact archive OS vulnerability scans before smoke/upload, both architectures | Release Controller products |
| Source/dependency vulnerabilities and repository secrets | Security |
| Tagged-source build and GitHub/GHCR publication | Release |

Business builds local products and installs from a loopback registry on
disposable amd64 runners. Native package checks require host, daemon, image and
executed ELF architecture to agree; cross compilation is not native acceptance.
Checks retain architecture/version, protected staging and SHA validation.
Never install test packages on shared hosts or clear their binfmt handlers.
Current checks do not certify historical upgrades or mixed-version safety.

## Diagnostics and build rehearsal

For extended [Business integration](real-business-validation.md):

```bash
gh workflow run release-upgrade.yml --ref <branch> \
  -f version=0.0.0 -f purpose=integration \
  -f production_signer=true -f run-resilience=true
```

`purpose=smoke` selects the smaller scope independently of Signer selection;
`production_signer=false` uses standalone topology. Diagnostics do not publish,
write GHCR or read production Secrets, and cannot replace Release Check.

For a build-only release rehearsal:

```bash
gh workflow run release.yml --ref main -f version=0.0.0
```

Both native Agent and Controller build/security/smoke legs and asset preparation
must pass; Publish must be skipped. Focused checks use
`scripts/test-release-upgrade.sh`, package/installer tests, relevant
ShellCheck/actionlint and [documentation checks](testing.md). Cover authorized
AgentUpgrade digest success/refusal and Stage-0 download/checksum failures when
changing those paths. Do not publish or deploy production during validation.

Rerun failed owners with all their checks; selected recovery reruns its complete
Business job. Failure, cancellation, missing results and unexpected required
skips cannot pass. Caches accelerate builds but are not accepted products or
qualification. Retain evidence, then clean only task-owned resources.
