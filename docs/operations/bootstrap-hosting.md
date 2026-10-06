# Stage-0 bootstrap hosting contract

The repository-owned Stage-0 sources are:

- `deploy/bootstrap/install-controller`
- `deploy/bootstrap/install-node`

They are intended to be deployed at
`https://get.ocservia.example/install-controller` and
`https://get.ocservia.example/install-node`. `install-node` is deployed
byte-for-byte from Git. `install-controller` is deployed byte-for-byte from the
`install-controller` asset of the latest stable Release: that asset equals the
Git source except for its stamped default version (see
[Quick mode](#quick-mode)). This repository does not contain
the external static-hosting infrastructure, so it does not claim that those
example endpoints are live.

The Quick Start also expects the repository's `install.env.example` to be
served byte-for-byte at
`https://get.ocservia.example/install.env.example`. It is configuration input,
not executable Stage-0 code.

## Hosting requirements

The deployment must be static HTTPS hosting with HSTS enabled. Both responses
must use `Content-Type: text/plain`; they must not be templated per user,
receive tokens in query parameters, contain secrets, or depend on server-side
session state. The Git source file, or for `install-controller` the stable
Release asset built from it, is the sole source of truth. Never host an RC asset.

After deployment, compare the served bytes with the expected source artifact.
`check-release-stage0.sh` first confirms that the downloaded Release asset
differs from the Git source at that tag only in its default version:

```bash
scripts/check-release-stage0.sh "$RELEASE_ASSETS/install-controller" vX.Y.Z
scripts/verify-bootstrap-endpoint.sh \
  https://get.ocservia.example/install-controller \
  "$RELEASE_ASSETS/install-controller"
scripts/verify-bootstrap-endpoint.sh \
  https://get.ocservia.example/install-node \
  deploy/bootstrap/install-node
scripts/verify-bootstrap-endpoint.sh \
  https://get.ocservia.example/install.env.example \
  install.env.example
```

The verifier performs a read-only HTTPS download, requires byte equality, and
prints the deployed SHA-256 digest for the deployment record.

## Trust model

Stage-0 accepts only an explicit stable `--version vX.Y.Z`. It constructs a
single GitHub Release URL under that version and hands off only to the matching
`controller-bootstrap.sh` or `managed-node-bootstrap.sh` Stage-1 asset. It does
not read `install.env`, install packages, invoke the Controller lifecycle,
enroll a node, approve a node, start services, or cross a privilege boundary.

Stage-0 downloads the selected asset over HTTPS with TLS verification enabled,
refuses download errors and empty bodies, and executes it from a private
short-lived directory. It does not require a release key, fingerprint, signed
checksum manifest, or signature. The HTTPS endpoint and its PKI supply download
trust; this path has no independent artifact authenticity claim. Stage-0 does
not add an OpenSSL version requirement.

Download Stage-0 locally, inspect it, and run it with the explicit version.
Do not source the version from `latest`, a branch, or a commit. The only
default is the exact tag stamped into a Release's own `install-controller`
asset, used by [Quick mode](#quick-mode). Stage-1 owns
configuration, installation, enrollment and activation, including package and
lifecycle validation. Stage-0 does not bypass those requirements.

## Intended entrypoints

After the static endpoints are deployed and their bytes have been verified,
the convenience forms first download Stage-0, require its final completeness
marker, and only then execute it. The surrounding subshell makes a transport,
empty-body, or truncated-body failure visible to automation:

```bash
(
  set -eu
  stage0="$(mktemp)" || exit 1
  trap 'rm -f -- "$stage0"' EXIT
  trap 'exit 1' HUP INT TERM
  curl -fsSL --proto '=https' --tlsv1.2 -o "$stage0" \
    https://get.ocservia.example/install-controller || exit 1
  test "$(tail -n 1 "$stage0")" = 'main "$@"' || exit 1
  bash "$stage0" --version vX.Y.Z
)

(
  set -eu
  stage0="$(mktemp)" || exit 1
  trap 'rm -f -- "$stage0"' EXIT
  trap 'exit 1' HUP INT TERM
  curl -fsSL --proto '=https' --tlsv1.2 -o "$stage0" \
    https://get.ocservia.example/install-node || exit 1
  test "$(tail -n 1 "$stage0")" = 'main "$@"' || exit 1
  bash "$stage0" --version vX.Y.Z
)
```

Only `--root-lifecycle` and, for the Controller, `--check` and the
[Quick mode](#quick-mode) options are accepted beyond the required version. All configuration and protected material stay in the
environment or protected paths for Stage-1; Stage-0 neither parses nor prints
their contents. Managed-node automation may reach `PENDING_APPROVAL`, never
Approval, and service activation remains deliberate.

Stage-0 is not a long-term upgrade, rollback, uninstall, or service manager.
Controller upgrades and rollback continue through `controller.sh` and its
protected lifecycle state. Managed-node upgrades and removal continue through
the authorized upgrader or native package-manager contract. Long-lived settings
belong in `install.env` or the installed service configuration, not in arguments
that must be replayed through the convenience script.

## Quick mode

The Controller Stage-0 also accepts:

```bash
bash "$stage0" --quick \
  --controller-domain vpn.example.com \
  --relay-domain relay.example.com \
  --acme-email ops@example.com \
  [--version vX.Y.Z] [--root-ca-passphrase-file PATH] [--root-ca-export-dir PATH] [--check]
```

Stage-0 only checks the shape of these values: two distinct lowercase DNS
names, a plain email address and absolute root CA paths. It does not read the
passphrase file or the export directory. `--quick` implies `--root-lifecycle`.
Quick-only options are rejected without `--quick`, and the Node Stage-0 rejects
them all.

When `--version` is omitted, quick mode uses the `DEFAULT_VERSION` stamped into
the Release asset by `scripts/prepare-bootstrap-release-assets.sh`. The Git
source leaves it empty, so an unstamped copy requires `--version`. Stage-0
prints the selected release before downloading Stage-1, and an explicit
`--version` always takes precedence. Outside quick mode there is no default.
Only releases whose Stage-1 implements quick mode accept these options; older
Stage-1 assets reject them before making changes.
