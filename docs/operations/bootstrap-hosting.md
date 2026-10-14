# Stage-0 bootstrap hosting contract

The repository maintains two Stage-0 bootstrap scripts:

- `deploy/bootstrap/install-controller`
- `deploy/bootstrap/install-node`

When an operator hosts the public bootstrap endpoints, `install-node` is served
byte-for-byte from Git at `https://get.ocservia.example/install-node`, and
`install-controller` is served byte-for-byte from the `install-controller`
asset of the latest stable Release at
`https://get.ocservia.example/install-controller` (that Release asset matches
the Git source except for its stamped default version; see
[Quick mode](#quick-mode)). The public first-install guide also expects the
non-executable configuration template `install.env.example` to be served
byte-for-byte at `https://get.ocservia.example/install.env.example`. Because
external static-hosting infrastructure lives outside this repository, those
example endpoints are active only when deployed by an operator.

## Hosting requirements

Serve the endpoints from static HTTPS hosting with HSTS enabled and
`Content-Type: text/plain`. Responses must be static for all callers, without
per-user templating, query-parameter tokens, embedded secrets, or server-side
session state. Publish only from the Git source file (or, for
`install-controller`, the latest stable Release asset built from it), and never
host an RC asset.

After deploying the files, verify that the served bytes match the expected
source artifacts. `check-release-stage0.sh` first checks that the downloaded
Release asset differs from the Git source at that tag only in its stamped
default version:

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

Each `verify-bootstrap-endpoint.sh` invocation downloads the URL read-only over
HTTPS, enforces exact byte equality against the local file, and prints the
deployed SHA-256 digest for the deployment record.

## Trust model

Outside [Quick mode](#quick-mode), Stage-0 requires an explicit stable
`--version vX.Y.Z`, builds the corresponding GitHub Release URL for that tag,
and hands off execution to the matching `controller-bootstrap.sh` or
`managed-node-bootstrap.sh` Stage-1 asset. Stage-0 itself never reads
`install.env`, installs packages, runs the Controller lifecycle, enrolls or
approves a node, starts services, or crosses a privilege boundary; Stage-1 owns
all configuration, installation, enrollment, activation, and package or
lifecycle validation.

During the handoff, Stage-0 fetches the Stage-1 script over HTTPS with TLS
verification enabled, rejects download errors or empty responses, and runs the
downloaded file from a private temporary directory. Download authenticity in
this phase relies on the HTTPS endpoint and its PKI without separate release
keys, fingerprints, signed checksum manifests, or detached signatures, and
Stage-0 imposes no additional OpenSSL version requirement.

Operators should download Stage-0 locally, inspect its contents, and invoke it
with an explicit version tag. Never resolve the version dynamically from
`latest`, a branch name, or a commit hash; the only permitted default version
is the exact release tag stamped into a Release's own `install-controller`
asset for [Quick mode](#quick-mode).

## Intended entrypoints

Once the static endpoints are deployed and verified, the one-line convenience
wrappers below download Stage-0 to a temporary file, check for its trailing
`main "$@"` completeness marker, and only then execute it. Wrapping the steps
in a subshell surfaces any transport, empty-body, or truncated-download failure
to calling automation:

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

Beyond `--version`, Stage-0 accepts only `--root-lifecycle` and, on the
Controller entrypoint, `--check` and the [Quick mode](#quick-mode) flags. Pass
all configuration and secrets through the environment or protected host paths
consumed by Stage-1, as Stage-0 neither parses nor prints secret material. On a
managed node, automated bootstrap stops at `PENDING_APPROVAL`, leaving node
approval and service activation as explicit post-enrollment steps.

Stage-0 is solely a first-install bootstrapper. Subsequent Controller upgrades
and rollbacks run through `controller.sh` and its protected lifecycle state,
while managed-node upgrades and removals run through the authorized upgrader or
native package manager. Persist long-lived settings in `install.env` or the
installed service configuration so they do not depend on re-running the
bootstrap wrapper with CLI flags.

## Quick mode

The Controller Stage-0 script also supports a Quick installation preset:

```bash
bash "$stage0" --quick \
  --controller-domain vpn.example.com \
  --relay-domain relay.example.com \
  --acme-email ops@example.com \
  [--version vX.Y.Z] [--root-ca-passphrase-file PATH] [--root-ca-export-dir PATH] [--check]
```

At the Stage-0 layer, the script validates only argument syntax: two distinct
lowercase DNS names, a well-formed email address, and absolute paths for the
optional Root CA arguments (without reading the passphrase file or export
directory). Passing `--quick` implies `--root-lifecycle`. Quick-specific flags
are rejected unless `--quick` is set, and the node Stage-0 script rejects all
of them.

When `--version` is omitted under `--quick`, Stage-0 falls back to the
`DEFAULT_VERSION` value stamped into the Release asset by
`scripts/prepare-bootstrap-release-assets.sh` (an unstamped copy from Git
leaves `DEFAULT_VERSION` empty and therefore still requires `--version`).
Stage-0 prints the resolved release tag before downloading Stage-1, and an
explicit `--version` flag always overrides the stamped default. Only releases
that ship `deploy/production/quick-install.sh` (post-`v1.2.0` stable or RC
releases) support these flags; Stage-1 scripts from `v1.2.0` and earlier reject
them before modifying the host.

When Stage-1 (`controller-bootstrap.sh --quick`) starts, it ignores any local
`./install.env` and runs a read-only host preflight before cloning (including
when invoked with `--check`):

- Both `--controller-domain` and `--relay-domain` must resolve to the same IPv4
  addresses. If a resolved IPv4 address is not bound to a local interface, the
  preflight emits a warning because NAT or cloud elastic IP mappings cannot be
  verified locally. By contrast, an `AAAA` record pointing to an IPv6 address
  not bound to the host fails the preflight because ACME validation prefers
  IPv6.
- Host ports `TCP 443` and `UDP 7842` must be unused unless
  `/etc/ocservia/install.env` already exists from a previously interrupted
  Quick run, and upstream cloud firewalls must allow inbound traffic on both
  ports.

After the preflight succeeds, Stage-1 invokes the release's
`deploy/production/quick-install.sh`, which prepares the host, generates
protected secret material via `quick-materials.sh`, writes
`/etc/ocservia/install.env` (configured for Integrated mode, bundled
PostgreSQL, ACME TLS, and Local authentication), runs
`install.sh --root-lifecycle`, provisions the management workspace, and creates
the initial Local administrators `initial-admin` and `initial-approver`. Their
generated passwords are written only to root-owned files under
`/root/ocservia-initial-credentials`; distribute the two accounts to different
operators and change both passwords after first login.

Re-running the same Quick command resumes an interrupted Quick installation,
whereas running it against an existing non-Quick `/etc/ocservia/install.env`,
mismatched domain or email arguments, or an already-running Controller
deployment fails closed (use the manual installation workflow or
`controller.sh upgrade` for those hosts).
