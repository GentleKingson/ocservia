# Current package validation

Use [Release Check](release-checks.md) for current-candidate validation.
Historical native upgrade, mixed-version and migration compatibility workflows,
scripts and baseline registry are removed. The
[published 1.0 procedure](https://github.com/GentleKingson/ocservia/blob/d420b22018596d6741d55fa56bd19c4a767e5817/docs/development/release-upgrade-validation.md)
is retained in Git history, not as a runnable current acceptance entry.

The `Release Diagnostics` workflow accepts only `smoke` or `integration`,
without `baseline_release` or a version-order requirement:

```bash
gh workflow run release-upgrade.yml --ref <candidate-branch> \
  -f version=1.1.0 -f purpose=smoke
```

Choose an exact candidate branch. Dispatch derives its source SHA and checks
checkout identity; version remains plain `X.Y.Z`. The default
`production_signer=false` mode has read-only Registry permissions and does not
publish packages or images. It is not a complete Release Check.

With explicit candidate Registry-write authorization, select
`purpose=integration` and `production_signer=true` to build and publish
run-bound candidate GHCR images and execute current native Integrated/real
Signer acceptance. This uses the production Signer implementation in isolated
tests, not a production deployment. It neither creates a release tag nor
publishes a formal GitHub Release or uses the production release signing key.

Formal tag publication requires this Integrated mode to succeed on `main` for
the exact tag SHA/version, with the accepted artifacts still available. A
default smoke/integration diagnostic pass or a `release.yml` dry-run alone
does not satisfy that requirement. Follow the [publication sequence](release-checks.md).

## Current native products

Both `amd64` and `arm64` packages retain signature, digest, source and
architecture validation and current installation smoke. The host, Docker
daemon, image platform and executed ELF architecture must agree; cross
compilation is not native acceptance. Never clear host binfmt handlers or
install test packages on shared BuildServer.

Preserve package retries, state retention, unsafe-package rejection, matched
snapshots and actual service readiness. Removing historical acceptance is not
a guarantee that arbitrary cross-version operations will work safely.

## Trust and reruns

Diagnostic builds use ephemeral signing keys, not production credentials.
Consumers use producer-supplied artifact IDs and verify the trusted candidate
manifest and exact product digests. Source SHA alone is not binary identity.
Publication must validate its own actual signed products and image digests.

Independent failed units can rerun using their producer-bound inputs, without
substituting another candidate or latest-success artifacts. Rebuilding a product
requires its dependent checks again. Shared cross-host resilience belongs to
one fault timeline and must rerun together. Preserve failed attempts; do not
turn skipped historical checks into a compatibility PASS.

Local syntax and contract checks run in an isolated BuildServer checkout:
`bash scripts/test-release-upgrade.sh`, relevant ShellCheck/actionlint and
documentation checks. Actual native installation belongs on the existing
authorized disposable runners. Remove only task-owned temporary resources.
