# Deploy the Controller

This guide covers a first production deployment with the guarded Controller
lifecycle. It keeps the operator path short; the exact filesystem, release,
secret, and rollback contracts are in [Production deployment reference](../operations/production-deployment.md).

## Requirements

- Ubuntu 20.04, 22.04, 24.04, or 26.04, or Debian 11, 12, or 13, on `amd64`
  or `arm64`. Ubuntu 20.04 requires an existing compatible Docker
  installation (Docker Engine with Compose v2 and `docker compose up --wait`);
  automatic Docker bootstrap is unavailable on that legacy host, which needs
  Ubuntu Pro/ESM or an equivalent maintenance strategy. Debian 11 left regular
  Debian LTS security maintenance on 2026-08-31 and needs Debian ELTS or an
  equivalent for production.
- Git, curl, and the ability to prepare a durable clean checkout. The versioned
  bootstrap creates and verifies that checkout for the recommended path.
- A DNS name and HTTPS certificate for the Controller.
- An OIDC issuer and client, an HTTPS certificate signer, and a TLS OTLP
  backend.
- Two independently operated dedicated relays with distinct DNS names.
- A protected backup location and a Controller Iroh identity whose public
  EndpointID is known.
- A lifecycle launcher user with Docker daemon access, or the ability to run
  the lifecycle as root.
- The release-signing public key and its expected fingerprint provisioned
  through a channel separate from the release bundle.

The repository does not generate production secrets, trust keys, certificates,
relay tokens, or passwords. Prepare those through the operator's protected
provisioning process.

## 1. Prepare configuration

Keep installation configuration in its own protected directory, separate from
the versioned source checkout:

```bash
git clone --branch vX.Y.Z --single-branch --depth 1 \
  https://github.com/GentleKingson/ocservia.git ocservia-vX.Y.Z
mkdir ocservia-install && cd ocservia-install
cp ../ocservia-vX.Y.Z/install.env.example install.env
editor install.env
```

The configuration file is a strict, non-executing `KEY=VALUE` contract; it is
not a secret generator. Provision every secret and trust file it references
through the operator's protected channel before installation.

## 2. Install the pinned release

Run the versioned Stage-1 from the exact release checkout:

```bash
../ocservia-vX.Y.Z/deploy/production/controller-bootstrap.sh \
  --version vX.Y.Z
```

Stage-1 reads `install.env` from the current directory and creates a durable
launcher-owned clean checkout at
`${OCSERV_CONTROLLER_SOURCE_ROOT:-$HOME/.local/share/ocservia/controller/releases}/vX.Y.Z`.
It then invokes that checkout's `deploy/production/install.sh`. The working
configuration directory and the source checkout therefore remain separate.

The Controller flow is:

```text
Stage-0 -> exact vX.Y.Z Stage-1 -> install.env -> durable clean checkout
        -> production/install.sh -> signed manifest -> controller.sh
        -> smoke/readiness
```

The durable checkout must be clean and match the signed manifest's
`source_commit`; `controller.sh` remains the authority for release verification,
digest-pinned images, activation, and lifecycle state.

The thin Stage-0 entrypoint becomes an alternative only after an operator has
deployed the static endpoint and verified its bytes. Do not use a bare
`curl | bash` pipeline. The fail-closed form downloads to a temporary file,
checks curl's status and the final completeness marker, and only then executes
Stage-0. It can additionally verify the immutable Stage-1 Release asset with
the out-of-band Ed25519 key and fingerprint. See [Stage-0 bootstrap
hosting](../operations/bootstrap-hosting.md) for the exact command and trust
boundary.

## 3. Release bundle

`deploy/production/install.sh` downloads the release bundle for the host
architecture automatically, so this step only describes what is downloaded.
From the [GitHub Releases](https://github.com/GentleKingson/ocservia/releases)
page for the selected release tag, the relevant assets are:

- `controller-release-amd64.json` and its `.sha256`, or
- `controller-release-arm64.json` and its `.sha256`,

together with `SHA256SUMS` and `SHA256SUMS.sig`.

Keep the selected manifest, `SHA256SUMS`, and `SHA256SUMS.sig` together in one
protected directory when provisioning them manually. Do not use the
`release-signing.pub.pem` published beside the bundle as the trust anchor. Set
`OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY` to the independently provisioned key.

## 4. Compatibility checkout path

Ubuntu 20.04 and Debian 11 cannot use the verified Stage-0 path because their
distribution OpenSSL cannot verify Ed25519 in this contract. They retain the
clean exact-release checkout path. It is also available for operators who do
not deploy the static Stage-0 endpoint:

```bash
git clone --branch <release-tag> --depth 1 \
  https://github.com/GentleKingson/ocservia.git
cd ocservia

export OCSERV_BACKUP_DIR=/protected/ocservia-backups
```

The supported hosts are Ubuntu 20.04/22.04/24.04/26.04 and Debian 11/12/13 on
`amd64` or `arm64`. When Docker is absent, the bootstrap installs Docker
Engine and the Compose plugin from Docker's official apt repository for the
detected distribution — except on Ubuntu 20.04, which supports only an
already-installed compatible Docker. The bootstrap never changes Docker
permissions, the firewall, or production trust material.

The optional static Stage-0 convenience entrypoint has a narrower verified
platform contract because its Ed25519 release-manifest verification requires
OpenSSL 3. Ubuntu 20.04 and Debian 11 retain full Controller support but must
use the clean exact release checkout path documented here rather than
`install-controller`. See the [Stage-0 bootstrap hosting
contract](../operations/bootstrap-hosting.md) for the entrypoint and trust
details.

`deploy/production/install.sh` (step 7) runs the host bootstrap itself. To
prepare or verify the host separately:

```bash
deploy/production/bootstrap-host.sh check \
  --backup-dir "$OCSERV_BACKUP_DIR"
sudo deploy/production/bootstrap-host.sh install \
  --backup-dir "$OCSERV_BACKUP_DIR"
```

## 5. Configure required production settings

Export the values used by the production Compose file. Keep this environment
in the same protected operator session used for the lifecycle command:

```bash
export OCSERV_PUBLIC_HOST=controller.example.com
export OCSERV_SECRET_DIR=/protected/ocservia-secrets
export OCSERV_BACKUP_DIR=/protected/ocservia-backups
export OCSERV_OIDC_ISSUER=https://id.example.com
export OCSERV_OIDC_CLIENT_ID=ocservia
export OCSERV_CERTIFICATE_SIGNER_URL=https://pki.example.com/v1
export OCSERV_OTEL_BACKEND_ENDPOINT=otel.example.com:4317
export OCSERV_AUDIT_EVENT_KEY_ID=audit-event-v1
export OCSERV_CONTROLLER_ENDPOINT_ID=<64-lowercase-hex-characters>
export OCSERV_RELAY_URL_A=https://relay-a.example.com
export OCSERV_RELAY_URL_B=https://relay-b.example.com
export OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY=/etc/ocservia/controller-release-signing.pub.pem
```

Instead of exporting every variable, you can keep the configuration in
`./install.env` in the directory you run the installer from (normally the
checkout root): copy `install.env.example` from the repository root, delete
the managed-node section, and uncomment and edit the Controller entries.
`install.env` is git-ignored, so it never makes the clean-release-checkout
check fail. The file is parsed by a strict, non-executing loader
(`deploy/lib/install-env.sh`): it only accepts the documented allowlisted
keys as literal `KEY=VALUE` lines, and it fails closed on unknown keys,
malformed lines, or unsafe file metadata (symlinks, group/world-writable
permissions). Variables exported in the shell always win over the file.

`OCSERV_CONTROLLER_ENDPOINT_ID` must match the public identity derived from
the protected `controller-iroh.key`. The two relay URLs are required for the
production transport path; public relays are not a fallback.

## 6. Configure secrets and trust

Place the required files in `OCSERV_SECRET_DIR`:

```text
tls.crt
tls.key
postgres-owner-password
postgres-app-password
postgres-backup-password
postgres.pgpass
database-owner-url
database-app-url
oidc-client-secret
session-key
audit-checkpoint-key
audit-event-key
controller-command-signing-key.pem
certificate-signer-token
relay-access-token
controller-iroh.key
otel-client.crt
otel-client.key
otel-ca.crt
```

The directory must be an absolute, canonical mode-`0700` directory outside the
checkout. The files have different ownership and mode requirements, so do not
apply one broad permission rule to all of them. See [Production security and
lifecycle details](../operations/production-deployment.md) before starting.

Configure the OIDC redirect URI as
`https://<OCSERV_PUBLIC_HOST>/api/v1/auth/callback`. Configure each relay with
the same protected access token and its own certificate and key; see
[Dedicated relays](../operations/dedicated-relays.md).

## 7. Direct checkout installation

How the single-command installer runs depends on the Docker state of the host:

- On a host that already has Docker with daemon access granted to the
  lifecycle launcher user, run it as that launcher user (not as a whole-script
  `sudo` — it invokes `sudo` itself only for the host bootstrap):

  ```bash
  deploy/production/install.sh
  ```

- On a fresh host without Docker, run the deliberate root lifecycle instead:

  ```bash
  deploy/production/install.sh --root-lifecycle
  ```

  A freshly installed Docker grants no non-root daemon access and the
  installer never modifies the Docker permission model, so a non-root
  launcher on a Docker-less host fails closed up front rather than mutating
  the host and failing after the Docker install. `--root-lifecycle` obtains
  root through a controlled `sudo env`, forwards only the allowlisted
  production `OCSERV_*` settings from this operator session, and runs the
  whole Controller lifecycle — including the state root — as root. Do not replace
  this with `sudo -E`; plain whole-script `sudo` without the flag stays
  rejected. The alternative is to install Docker separately first and
  deliberately grant the launcher Docker daemon access per Docker's official
  post-install steps, then run the installer as the launcher.

The installer verifies that the checkout is a clean exact `vX.Y.Z` release
tag, selects the manifest matching the host architecture (`amd64` or `arm64`),
bootstraps the host, downloads the release bundle into
`<state-root>/release-bundles/vX.Y.Z`, and activates it through
`controller.sh install`.

The equivalent manual path is the manifest matching the Docker daemon
architecture:

```bash
# amd64
deploy/production/controller.sh install \
  --release-file /protected/release/controller-release-amd64.json

# arm64
deploy/production/controller.sh install \
  --release-file /protected/release/controller-release-arm64.json
```

The lifecycle verifies the signed release bundle, the clean checkout
`source_commit`, the platform, and the digest-pinned images before activation.
It then starts the dependency graph and runs the release smoke check. Do not
replace this command with `docker compose up -d`.

## 8. Verify the deployment

The install command must finish successfully. You can validate the public
readiness and version endpoints:

```bash
curl --fail --silent --show-error \
  "https://${OCSERV_PUBLIC_HOST}/api/v1/readyz"
curl --fail --silent --show-error \
  "https://${OCSERV_PUBLIC_HOST}/api/v1/version"
```

Then verify an authenticated read, a node connection through each relay, OTLP
delivery, and the newest backup. A release failure leaves pending evidence;
retry only the same target after correcting its cause. See [Controller upgrade](../how-to/controller-upgrade.md),
[Controller rollback](../how-to/controller-rollback.md), and [Troubleshooting](../how-to/troubleshooting.md).

For later upgrades, prepare a clean checkout of the next exact release under
the durable source root, then run `controller.sh upgrade` from that checkout
with the new signed manifest. The current versioned bootstrap is a first-install
entrypoint, not an upgrade command. Do not rerun an unpinned or latest
convenience script as an implicit upgrade. Rollback continues to use protected
lifecycle state, and uninstall continues to use `controller.sh uninstall`;
Stage-0 is not a long-term upgrade, rollback, or uninstall manager.

## Next steps

- [Install a managed node](managed-node.md)
- [Enroll a node](../how-to/enroll-node.md)
- [Configure dedicated relays](../operations/dedicated-relays.md)
- [Back up and restore PostgreSQL](../operations/postgres-backup.md)
