# Stage-0 bootstrap hosting contract

The repository-owned Stage-0 sources are:

- `deploy/bootstrap/install-controller`
- `deploy/bootstrap/install-node`

They are intended to be deployed byte-for-byte at
`https://get.ocservia.example/install-controller` and
`https://get.ocservia.example/install-node`. This repository does not contain
the external static-hosting infrastructure, so it does not claim that those
example endpoints are live.

## Hosting requirements

The deployment must be static HTTPS hosting with HSTS enabled. Both responses
must use `Content-Type: text/plain`; they must not be templated per user,
receive tokens in query parameters, contain secrets, or depend on server-side
session state. The corresponding Git source file is the sole source of truth.

After deployment, compare the served bytes with the expected source artifact:

```bash
scripts/verify-bootstrap-endpoint.sh \
  https://get.ocservia.example/install-controller \
  deploy/bootstrap/install-controller
scripts/verify-bootstrap-endpoint.sh \
  https://get.ocservia.example/install-node \
  deploy/bootstrap/install-node
```

The verifier performs a read-only HTTPS download, requires byte equality, and
prints the deployed SHA-256 digest for the deployment record.

## Trust model

Stage-0 accepts only an explicit stable `--version vX.Y.Z`. It constructs a
single GitHub Release URL under that version and hands off only to the matching
`controller-bootstrap.sh` or `managed-node-bootstrap.sh` Stage-1 asset. It does
not read `install.env`, install packages, invoke the Controller lifecycle,
enroll a node, approve a node, start services, or cross a privilege boundary.

When both `TRUSTED_RELEASE_KEY` and `EXPECTED_RELEASE_KEY_SHA256` are exported,
Stage-0 downloads `SHA256SUMS` and `SHA256SUMS.sig`, verifies the independently
provisioned public-key fingerprint and manifest signature, then verifies the
selected Stage-1 digest before execution. Configure both values through the
operator's protected provisioning channel. Stage-1 and the existing lifecycle
remain responsible for configuration, package and release verification,
installation, enrollment, and activation.

This Ed25519 verification path requires OpenSSL 3. That matches the managed-node
support matrix. The Controller itself also supports Ubuntu 20.04 and Debian 11,
but their distribution OpenSSL 1.1.1 cannot perform this verification. On those
two Controller platforms, do not use Stage-0; continue to use the clean exact
release checkout path in [Deploy the Controller](../getting-started/production.md).
This narrows only the optional Controller Stage-0 entrypoint, not the existing
Controller support matrix.

The first bytes in a `curl | bash` flow cannot authenticate themselves. Before
Stage-0 has started, that flow relies only on the HTTPS endpoint and its PKI;
verifying the downloaded Stage-1 does not provide out-of-band authenticity for
the already executing Stage-0 bytes. With no release trust anchor configured,
Stage-0 warns and Stage-1 authenticity also relies on HTTPS.

For a hardened first-byte path, download Stage-0 to local protected storage,
compare its digest with the reviewed repository source through an independent
channel, inspect it, and only then run that local file with the out-of-band key
and fingerprint exported. An operator may instead download Stage-1,
`SHA256SUMS`, and `SHA256SUMS.sig`, verify them locally with that trust anchor,
and execute the verified Stage-1 directly. Do not source a version from
`latest`, a branch, or a commit, and do not download the public key from the
same release as the artifact it is meant to authenticate.

## Intended entrypoints

After the static endpoints are deployed and their bytes have been verified,
the convenience forms first download Stage-0, require its final completeness
marker, and only then execute it. The surrounding subshell makes a transport,
empty-body, or truncated-body failure visible to automation:

```bash
export TRUSTED_RELEASE_KEY=/etc/ocservia/release-signing.pub.pem
export EXPECTED_RELEASE_KEY_SHA256=<64-lowercase-hex-fingerprint>
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

Only `--root-lifecycle` and, for the Controller, `--check` are accepted beyond
the required version. All configuration and protected material stay in the
environment or protected paths for Stage-1; Stage-0 neither parses nor prints
their contents. Managed-node automation may reach `PENDING_APPROVAL`, never
Approval, and service activation remains deliberate.
