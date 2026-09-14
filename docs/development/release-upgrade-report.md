# Native upgrade implementation report

Date: 2026-09-14. Implementation branch: `codex/native-release-upgrade`.
Implementation is submitted for review; real four-cell upgrade acceptance is
**BLOCKED / NOT RUN**, not PASS. This is not release authorization.

## Identities and changes

- Rechecked main: `e9016da71e5b447d3532e8981e5ca08844de88ff`.
- Stable baseline: immutable `v0.5.0`, commit
  `519275567a65f5785260e353ef02ab3fabf30244`, PostgreSQL schema 30.
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
- Extended `scripts/release-baseline-upgrade-smoke.sh` with shared baseline
  data and `scripts/release-agent-state-check.sh`, retaining its real native
  package/systemd lifecycle. Existing user edits selecting v0.5.0 are retained.
- Fixed `scripts/upgrade-agent.sh`: a verified identical-package retry no
  longer overwrites the old matched rollback snapshot with candidate files.
  File comparison still permits re-upgrade after rollback.
- Added `scripts/test-release-upgrade.{sh,mjs}` and
  `scripts/test-agent-upgrade-retry.sh`; updated the shared-build assertions
  in `scripts/test-controller-release-manifest.sh` and GitHub Actions docs.

## Executed checks

All execution was on native ARM64 BuildServer. Tools requiring additional
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
| Existing Controller manifest test | FAIL: pre-existing schema-30 assertion, current migrations reach 36 |

The existing manifest test fails before the changed build assertions. Its
unrelated hard-coded migration expectation was not changed. Shared build and
publication conditions are also checked by the new focused contract test.
No candidate package/image build or published-package upgrade is claimed by
these static and fixture tests.

## Real acceptance

| Required Actions cell | Result |
| --- | --- |
| agent-amd64 (DEB + RPM) | NOT RUN |
| agent-arm64 (DEB + RPM) | NOT RUN |
| controller-amd64 (PostgreSQL) | NOT RUN |
| controller-arm64 (PostgreSQL) | NOT RUN |

Actions run URL: none. The new workflow is not registered on the default
branch. A Draft PR and `--ref` cannot bypass GitHub's dispatch requirement;
no merge or automatic trigger was used. The runbook contains the command to
use after normal default-branch registration. No runner-minute or runtime
guarantee is inferred from local checks.

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

The old descriptor drops all capabilities while PostgreSQL starts as root
against postgres-owned runtime directories. This is a baseline-start blocker
in the tested isolated environment, not evidence of a candidate regression
or a completed ARM64 cell. The baseline was not edited, rebuilt, re-signed,
replaced with v0.4.0 or granted bypass capabilities. Candidate migration,
OIDC/data preservation, failure/recovery and restore paths remain unverified
in integration and must not be treated as accepted based on their code alone.

Proceeding requires ordinary default-branch workflow registration and a
reviewed resolution of the genuine baseline installation blocker without
misrepresenting modified assets as the published baseline. The Draft PR is
kept non-merge-ready pending real acceptance.
