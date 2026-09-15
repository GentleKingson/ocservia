# Native upgrade implementation report

Date: 2026-09-15. Implementation branch: `codex/native-release-upgrade`.
The workflow-registration and published-baseline blockers are closed. This
source report describes the candidate before dispatch; it does not claim
four-cell PASS. The exact final SHA, run/attempt and results are recorded in
PR #205 and downloaded evidence without changing the tested commit. This is
not release authorization.

## Identities and changes

- Rebased main: `c44c34ae7d48d0d0de7f7c02276233c4b8bd367e`, including #206's
  registration placeholder. The conflict retains #205's full implementation.
- Stable baseline: immutable `v0.5.2`, commit
  `2bbbbbbc7dd4a9b328acfaae291fdaf11a35bfd6`, PostgreSQL schema 30.
  Real checksum/signing-key pins are independently verified; v0.5.0/v0.5.1 remain
  in the historical registry, not overwritten or silently substituted.
- Candidate version: `0.6.0`. The final pushed branch commit is the candidate;
  obtain its exact SHA using the command in the
  [runbook](release-upgrade-validation.md). Local preliminary baseline
  reproduction used main's SHA, not the final candidate, and never upgraded.
- Added `.github/workflows/release-upgrade.yml`, frozen baseline/input/native
  and four-result contracts (`scripts/release-upgrade-*`), authenticated OIDC
  fixture, real Controller lifecycle smoke, and credential-log redaction.
- Extracted `scripts/build-release-agent.sh` and
  `scripts/build-release-controller.sh`; `release.yml` shares them without
  changing publishing dependencies, permissions or approval conditions.
- Carried #209's native Rocky 9 ABI builder into the shared Agent build and
  existing native lifecycle fixtures. All three payloads execute against
  glibc 2.34 with isolated Cargo objects before packaging; candidate DEB/RPM
  smoke also executes all three installed binaries in their real runtimes.
- Updated the bootstrap cache-order test to locate the actual shared build
  command rather than its obsolete step name, rejecting a missing build.
- Extended `scripts/release-baseline-upgrade-smoke.sh` with shared baseline
  data and `scripts/release-agent-state-check.sh`, retaining its real native
  package/systemd lifecycle. Both workflow defaults and the release package
  smoke now select the real v0.5.2 baseline.
- Reused the proven fresh-install fixture corrections from #207/#208: a native
  OIDC container joins the internal application network, and the test CA is
  written into container tmpfs before its namespace-only trust-store mount.
  No published baseline descriptor, manifest, signature or image is modified.
- Fixed `scripts/upgrade-agent.sh`: a verified identical-package retry no
  longer overwrites the old matched rollback snapshot with candidate files.
  Retry now also requires the existing snapshot to pass the real rollback
  validator (`rollback-agent.sh --verify-only`) before returning success or
  restarting services. The installed rollback script must be a regular,
  non-symlinked root:root 0755 file with one link. File comparison still
  permits re-upgrade after rollback.
- Added `scripts/test-release-upgrade.{sh,mjs}` and
  `scripts/test-agent-upgrade-retry.sh`; updated the shared-build assertions
  in `scripts/test-controller-release-manifest.sh` and GitHub Actions docs.

## Executed checks

The following focused checks passed in the earlier implementation/review
rounds; they are not evidence for a later candidate SHA. Updated focused
validation and Actions results are recorded in PR #205. All local execution
uses native ARM64 BuildServer. Tools requiring additional
dependencies ran in disposable containers; no native packages, users or
systemd units were installed on the shared host.

| Check | Result |
| --- | --- |
| Bash syntax on changed shell scripts | PASS |
| ShellCheck, warning severity | PASS |
| actionlint 1.7.7, both release workflows | PASS |
| Input, numerical version, baseline/assets, source/native identity contracts | PASS |
| Four-result completeness, failure/skip/cancel and mixed-attempt rejection | PASS |
| Diagnostic secret redaction and workflow/publishing contracts | PASS |
| Real installer retry/rollback/re-upgrade with stub binaries in DESTDIR | PASS, unit regression only |
| `scripts/docs-check.sh` | PASS |
| Controller manifest test, including shared build/publishing assertions | PASS after replacing the stale schema-30 assertion with dynamic migration head |
| Retry with missing manifest, corrupt snapshot member or unsafe installed rollback script | PASS: rejected before modification |

The original review run found that the manifest test stopped at its stale
schema-30 assertion before reaching the changed build assertions. The review
follow-up computes the expected head from `control-plane/migrations/*.up.sql`
instead of hard-coding 30 or 36, and reruns the whole test. Shared build and
publication conditions are also checked by the focused contract test.
No candidate package/image build or published-package upgrade is claimed by
these static and fixture tests.

## Real acceptance

| Required Actions cell | Result |
| --- | --- |
| agent-amd64 (DEB + RPM) | NOT RUN |
| agent-arm64 (DEB + RPM) | NOT RUN |
| controller-amd64 (PostgreSQL) | NOT RUN |
| controller-arm64 (PostgreSQL) | NOT RUN |

At source-report preparation the new candidate has not been dispatched. Use
the PR's exact-SHA run record for the current result, not this pre-dispatch
table. Registration is complete and the runbook command selects v0.5.2.
Only four same-attempt passing results plus `Native Upgrade Result` permit
acceptance. No runner-minute or runtime guarantee is inferred from local checks.

## Historical baseline failures (resolved)

A separate ARM64 baseline-only preflight used a newly created Docker-in-Docker
daemon, the actual v0.5.0 production install entrypoint, unmodified signed
manifest and digest-pinned published images. Baseline image architecture and
the four first-party ELF headers were checked. PostgreSQL never became
healthy; no old business data, candidate upgrade or downstream scenario ran.
The sanitized service log contains:

```text
postgres-1 | chmod: changing permissions of '/var/lib/postgresql/data': Operation not permitted
postgres-1 | chmod: changing permissions of '/var/run/postgresql': Operation not permitted
postgres-1 | find: '/var/run/postgresql': Permission denied
```

The old descriptor dropped all capabilities while PostgreSQL started as root
against postgres-owned runtime directories. It was not evidence of a candidate
regression or a completed ARM64 cell. The published baseline was not edited,
rebuilt, re-signed, replaced with v0.4.0 or granted bypass capabilities.

PR #207 fixed PostgreSQL startup identity and Caddy's file capability on the
v0.5 maintenance line; #208 resolved independent release-readiness blockers.
Their complete native fresh-install evidence and official v0.5.1 publication
run 34916370091 close that baseline blocker, not #205's upgrade acceptance.

The subsequent v0.5.1-to-v0.6.0 run 34922328905 on candidate
`92fa333becf317a0861a80d9f2ee97b23d060f6c` passed both Controller units and
the DEB paths, but failed both actual RPM baselines: privd required
GLIBC_2.39 on Rocky 9's glibc 2.34. The aggregate correctly returned NOT PASS.
No historical asset or runtime was changed to mask the failure. #209 fixed
the common payload ABI, and v0.5.2 was genuinely published in run 34934040575
with all ten jobs successful. All 21 assets, signatures and Controller
bundles/indexes were independently verified against the historical trust
anchor. The old run is retained only as failure evidence, not reused for
the new candidate's four-cell acceptance.
The Draft PR remains non-merge-ready until the new exact-SHA four-cell run passes.
