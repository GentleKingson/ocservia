# Package and upgrade validation

The release graph is [Release Check](release-checks.md). Each architecture
builds its actual Agent/privd/upgrader package set and Controller images once;
native installation and supported-baseline upgrade tests consume those bytes.
The standalone `Release Diagnostics` workflow is for debugging, not another
mandatory release run and never a complete Release Check.

## Supported matrix

| Component | Architectures | Required behavior |
| --- | --- | --- |
| Agent | amd64, arm64 | DEB on Ubuntu 24.04 and RPM on Rocky 9; install, v1.0.0 upgrade, state preservation, retry, rejection and rollback |
| Controller | amd64, arm64 | Published v1.0.0 PostgreSQL state, authenticated read/write, migration, interrupted upgrade recovery, data preservation, rollback contract and backup restore |
| Application | amd64, arm64 | Published v1.0.0 node against the candidate Controller and transport |

The v1.0.0 transitional baseline is registered in
[`release-upgrade-baselines.json`](../../scripts/release-upgrade-baselines.json).
Its registration establishes artifact identity, not upgrade success.
Historical v0.6.x entries remain available to diagnostic fetch/verify tools;
they are not supported upgrade paths into 1.x and do not run in the release
matrix. Pre-1.0 operators must redeploy. A 2.x migration requires a separately
reviewed contract; the current entrypoint rejects it.

Bundled PostgreSQL backup health uses a 60-second start period. Engine 25+ uses
its default 5-second startup probe interval; `start_interval` is deliberately
omitted to retain older Compose compatibility. Older engines retain the slow
5-minute probe interval. The LATEST freshness predicate, steady-state interval,
timeout and retries are unchanged. A first backup taking longer than the start
period can still wait for the next steady-state probe.
See [Docker healthcheck timing](https://docs.docker.com/reference/dockerfile/#healthcheck).

The released v1.0.0 descriptor is never patched to accelerate validation.
Changing `compose.postgres.yaml` triggers the existing production descriptor
rollback rejection contract; shorter rejection is not a rollback speedup.
The focused `bash scripts/test-backup-startup-healthcheck.sh` check runs only
on BuildServer with Engine 25+, in uniquely named, network-isolated containers.
It tests the production health predicate and schedule with fresh, missing and
stale LATEST markers, not backup contents or a full database restore.

The host, local Docker daemon, image platform and executable ELF architecture
must agree. Binaries are actually executed. An unrelated binfmt handler is
not evidence that the candidate uses emulation; no host handlers are cleared.

## Diagnostic entrypoint

Choose an exact candidate branch; the workflow derives its SHA from dispatch
and requires checkout HEAD to match. Version is plain X.Y.Z, newer than the
registered baseline for upgrade/compatibility purposes.

```bash
gh workflow run release-upgrade.yml --ref <candidate-branch> \
  -f version=1.0.1 -f baseline_release=v1.0.0 -f purpose=upgrade
```

`purpose` is one of `upgrade`, `compatibility`, `smoke`, `integration`.
There are no interacting boolean mode flags. Smoke and Integration are amd64
business diagnostics and do not use the selected upgrade baseline.

## Trust and reruns

Builds use ephemeral signing keys, never production release credentials.
Consumers download producer-supplied artifact IDs and fail explicitly if the
trusted candidate-manifest digest or any product digest differs. Source SHA
alone is not a binary identity. Final publication may re-sign and repackage,
but must retain the tested payload archives and validate the resulting
signatures, embedded trust root and installer payloads.

Each native unit validates its own required scenarios and identity before
returning success. The aggregate uses GitHub job results, not another collection
of result documents. Independent failed units can rerun without discarding
successful architecture/component jobs. Frozen baseline input is downloaded
by the prepare job's artifact ID and checked against its producer digest,
even when prepare came from an earlier attempt of this same run.
Rerunning a product producer reruns its dependent tests; never combine results
from a different candidate or substitute a latest-success artifact.
Shared cross-host Resilience is different: both fault domains belong to one
timeline and must rerun together.

Local syntax/behavior checks run only in an isolated BuildServer checkout:
`bash scripts/test-release-upgrade.sh`, affected ShellCheck/actionlint checks,
and documentation checks. Actual host installation belongs only on authorized
disposable native VMs or hosted runners. Never run these installers on shared
BuildServer, alter its systemd/users/network, or clear its Docker daemon.
