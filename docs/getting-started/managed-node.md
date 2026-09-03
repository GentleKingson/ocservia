# Install a managed node

A managed node runs the unprivileged ocservia Agent beside `privd`, a small
root service for fixed typed ocserv operations. Install a published package;
do not build a package on the node.

## Before you begin

- The Controller is deployed and its EndpointID is available.
- The node is a Linux host with systemd and root access.
- The host is one of the supported managed-node platforms: `x86_64` or
  `aarch64` on Ubuntu 22.04/24.04/26.04 or Debian 12/13 (native `.deb`), or
  Rocky Linux 9 (native `.rpm`). The one-command bootstrap and the native
  package verification require OpenSSL 3, so Ubuntu 20.04 and Debian 11
  (OpenSSL 1.1.1) are not supported managed-node platforms. Other platforms
  fail closed.
- You have an independently trusted release public key and its expected
  fingerprint.
- You can create a one-time token and approve the node. Follow [Enroll a
  node](../how-to/enroll-node.md) after preparing the sealing keys below.
- For a production node using dedicated relays, use the one-command bootstrap
  below, the signed Agent archive installation path below, or the native
  package with the production request marker below. Do not install production
  systemd files from an arbitrary source checkout.

## 1. One-command bootstrap

The supported path is the managed-node installer, run as a single
self-contained file with the release pinned through `--version vX.Y.Z`. No
Git checkout is required: the installer detects the platform, downloads that
exact release's `SHA256SUMS`, its signature, and the matching native package
(`.deb` on the Debian family, `.rpm` on Rocky Linux 9), and verifies the
out-of-band release trust — trusted key fingerprint, `SHA256SUMS.sig`, and
the selected package digest — **before** any package manager runs as root.
It then prepares the production node state (sealing keys, relay URLs, relay
access token, command verification key, persistent identity) and stops at
`ENROLLMENT_READY`. It never approves the node, never enables or starts a
service, and never weakens `expected_endpoint_id` binding.

Obtain `install.sh` for the exact release you are installing — a published
release tag is immutable, so never fetch the script from a branch or `main`
— and run it from any directory holding the node configuration:

```bash
curl -fsSL --proto '=https' --tlsv1.2 \
  -o install.sh \
  https://raw.githubusercontent.com/GentleKingson/ocservia/vX.Y.Z/deploy/managed-node/install.sh
chmod 0700 install.sh
export CONTROLLER_ENDPOINT_ID="replace-with-64-lowercase-hex-controller-endpoint-id"
export RELAY_URL_A="https://relay-a.example.com"
export RELAY_URL_B="https://relay-b.example.com"
export RELAY_ACCESS_TOKEN_SOURCE=/protected/relay-access-token
export CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE=/protected/controller-command-verification-key.pem
export TRUSTED_RELEASE_KEY=/etc/ocservia/release-signing.pub.pem
export EXPECTED_RELEASE_KEY_SHA256="replace-with-64-lowercase-hex-fingerprint"
./install.sh --version vX.Y.Z
```

Instead of exporting every variable, you can keep the node configuration in
`./install.env` in the directory you run the installer from: copy
`install.env.example` from the repository, delete the Controller section,
and uncomment and edit the managed-node entries. The installer embeds a
strict, non-executing loader (the same contract as
`deploy/lib/install-env.sh`): it only accepts the documented allowlisted
keys as literal `KEY=VALUE` lines, and it fails closed on unknown keys,
malformed lines, or unsafe file metadata (symlinks, group/world-writable
permissions). Variables exported in the shell always win over the file.

Compatibility path: the installer also runs from a clean checkout of an
exact release tag without `--version`, deriving the release identity from
the Git tag; `install.env` is git-ignored there, so it never makes the
clean-release-checkout check fail.

```bash
git clone --branch vX.Y.Z --depth 1 https://github.com/GentleKingson/ocservia
cd ocservia
deploy/managed-node/install.sh
```

`TRUSTED_RELEASE_KEY` defaults to `/etc/ocservia/release-signing.pub.pem` and
`EXPECTED_RELEASE_KEY_SHA256` is otherwise read from
`/etc/ocservia/trusted-release-key.sha256`; provision both out of band, exactly
like the durable upgrader trust anchors. The two source files must be
protected provisioned copies of the relay access token and the Controller
command verification public key. Optional inputs:
`USER_PASSWORD_SEAL_KEY_ID` (default `user-password-v1`),
`P12_PASSWORD_SEAL_KEY_ID` (default `p12-password-v1`), and
`ENROLLMENT_ENVIRONMENT` (default `production`).

The first successful run prints `ENROLLMENT_READY` and the node's EndpointID.
Create a short-lived one-time token with
`expected_endpoint_id=<printed EndpointID>` as described in [Enroll a
node](../how-to/enroll-node.md), install it as
`/etc/ocservia-agent/enrollment-token` (`root:ocserv-agent`, mode `0640`), and
rerun the same command. The rerun skips the completed steps, runs the
enrollment with the prepared identity and sealing descriptors, writes the
final `/etc/ocservia-agent/agent.env` atomically, consumes the one-time token
file, and prints `PENDING_APPROVAL` with the new `NODE_ID`. Repeated runs
converge on the same state: existing identity, sealing keys, relay token, and
trust material are preserved, an installed package is never reinstalled or
downgraded, and an already-enrolled node is never enrolled again. When the
native package of this exact release is already installed under the
production relay contract, the rerun also skips the release download and the
out-of-band trust verification — they protect the package-manager invocation,
which no longer happens. After the approval and the enable step below, a
rerun observes both services enabled and active and prints `SERVICES_ACTIVE`
without changing anything; it cannot observe Controller-side approval, so
confirm the node reports online in the Controller inventory.

Run the installer as the operator launcher user; it elevates through scoped
per-command `sudo` only. A deliberate whole-lifecycle-as-root run is available
with `deploy/managed-node/install.sh --root-lifecycle`.

After `PENDING_APPROVAL`, continue with [Approve the
node](../how-to/enroll-node.md#approve-the-node); `/etc/ocservia-agent/agent.env`,
the sealing keys, and the relay configuration are already complete at that
point. Enable both services only after approval, as in step 8 below.

## 2. Verify and choose the release package

Download the signed Agent archive, its checksum sidecars, the native package
you may install, `SHA256SUMS`, and `SHA256SUMS.sig` for one exact release from
[GitHub Releases](https://github.com/GentleKingson/ocservia/releases).
Provision the trusted public key through a separate channel. Do not use only
the `release-signing.pub.pem` downloaded beside the package.

Set the package variables for the release artifacts you will use:

```bash
export VERSION="replace-with-release-version"
export RELEASE_DIR="/protected/ocservia-agent-${VERSION}"
export AGENT_ARCHIVE="ocservia-agent-${VERSION}-linux-amd64.tar.gz"
export AGENT_PACKAGE="ocservia-agent_${VERSION}_amd64.deb"
export TRUSTED_RELEASE_KEY=/etc/ocservia/release-signing.pub.pem
export EXPECTED_RELEASE_KEY_SHA256="replace-with-64-lowercase-hex-fingerprint"
```

Set `AGENT_ARCHIVE` and `AGENT_PACKAGE` to the exact artifacts for the host:
use `linux-arm64.tar.gz` and `ocservia-agent_${VERSION}_arm64.deb` on Debian or
Ubuntu ARM, or `linux-amd64.tar.gz` / `linux-arm64.tar.gz` and
`ocservia-agent-${VERSION}-1.x86_64.rpm` /
`ocservia-agent-${VERSION}-1.aarch64.rpm` on RPM-based hosts. Verify the
trusted key, the signed release manifest, and both selected artifacts before
invoking any package manager as root:

```bash
test "$(openssl pkey -pubin -in "$TRUSTED_RELEASE_KEY" -outform DER \
  | sha256sum | awk '{print $1}')" = "$EXPECTED_RELEASE_KEY_SHA256"
(
  cd "$RELEASE_DIR"
  openssl pkeyutl -verify -rawin -pubin \
    -inkey "$TRUSTED_RELEASE_KEY" \
    -in SHA256SUMS -sigfile SHA256SUMS.sig
  grep -F "  ${AGENT_ARCHIVE}" SHA256SUMS | sha256sum -c --strict -
  grep -F "  ${AGENT_PACKAGE}" SHA256SUMS | sha256sum -c --strict -
)
```

The package's post-install verification is defense in depth. It must not be
the first trust decision for an unverified native package.

## 3. Install the production Agent from the signed archive

For production, use the signed archive path described in the [Agent lifecycle
reference](../operations/agent-lifecycle.md). Run the verifier from a
separately trusted release-tooling checkout; it is only a verifier source, not
the source of any production systemd file. The install script must run from
the root-owned staging directory printed by that verifier:

```bash
export RELEASE_TOOLING=/path/to/trusted/ocservia
VERIFIED_PACKAGE="$(sudo AGENT_TRUSTED_KEY_SHA256="$EXPECTED_RELEASE_KEY_SHA256" \
  "$RELEASE_TOOLING/scripts/verify-agent-package.sh" \
  "$RELEASE_DIR/$AGENT_ARCHIVE" \
  "$RELEASE_DIR/$AGENT_ARCHIVE.sha256" \
  "$RELEASE_DIR/$AGENT_ARCHIVE.sha256.sig" \
  "$TRUSTED_RELEASE_KEY")"
sudo INSTALL_PRODUCTION_RELAYS=true \
  "$VERIFIED_PACKAGE/scripts/install-agent.sh"
```

This installs the production relay drop-in and `relays.env.example` from the
verified archive. Do not copy `deploy/production/systemd` files from a source
checkout directly into `/usr/lib/systemd/system`.

If a native package is required instead, install only the exact package whose
external release signature and checksum were verified in step 1.

For a production node, first create the one-shot production request. Do this
before invoking the package manager — `postinst` reads it while configuring
the package, and the verified embedded payload then installs the production
relay drop-in and `relays.env`:

```bash
sudo install -d -o root -g root -m 0755 /etc/ocservia
sudo touch /etc/ocservia/agent-install-production-relays
```

Then install the package. On Debian or Ubuntu:

```bash
sudo dpkg -i "$RELEASE_DIR/$AGENT_PACKAGE"
```

On an RPM-based system:

```bash
sudo rpm -ivh "$RELEASE_DIR/$AGENT_PACKAGE"
```

The package installs the Agent, `privd`, the durable upgrader, and their
systemd units — with the production relay drop-in and `relays.env` when the
request marker was present, without them on a non-production node. It does
not enable or start either service. The successful install consumes the
request marker, the installed relay drop-in keeps later package upgrades on
the production relay contract, and removing the package also retires any
unconsumed request. Do not copy `deploy/production/systemd` files from a
source checkout into `/usr/lib/systemd/system` under either path.

## 4. Prepare sealing keys

Prepare two distinct RSA private keys before enrollment. If they were already
provisioned through a protected channel, do not regenerate them:

```bash
sudo openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
  -out /etc/ocservia-agent/user-password-seal-private.pem
sudo openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
  -out /etc/ocservia-agent/p12-password-seal-private.pem
sudo chmod 0600 \
  /etc/ocservia-agent/user-password-seal-private.pem \
  /etc/ocservia-agent/p12-password-seal-private.pem

export CONTROLLER_ENDPOINT_ID="replace-with-64-lowercase-hex-controller-endpoint-id"
export USER_PASSWORD_SEAL_KEY_ID=user-password-v1
export P12_PASSWORD_SEAL_KEY_ID=p12-password-v1
export USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256="$(sudo openssl rsa \
  -in /etc/ocservia-agent/user-password-seal-private.pem \
  -pubout -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
export P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256="$(sudo openssl rsa \
  -in /etc/ocservia-agent/p12-password-seal-private.pem \
  -pubout -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
```

The four sealing-key descriptor values are enrollment inputs. Enrollment
returns the new `NODE_ID`; it does not invent or return these descriptors.
Keep these variables available for the relay and enrollment steps below.

## 5. Configure dedicated relays before enrollment

Set both dedicated HTTPS relay URLs in `/etc/ocservia-agent/relays.env`, which
was installed by the signed archive path or the production native package
install, and install the protected relay token as
`/etc/ocservia-agent/relay-access-token` with owner
`root:ocserv-agent` and mode `0640`:

```bash
export RELAY_URL_A="https://relay-a.example.com"
export RELAY_URL_B="https://relay-b.example.com"

sudoedit /etc/ocservia-agent/relays.env
sudo install -o root -g ocserv-agent -m 0640 \
  /protected/relay-access-token /etc/ocservia-agent/relay-access-token
sudo systemctl daemon-reload
```

Use the exact ownership and certificate requirements in [Dedicated relays](../how-to/dedicated-relays.md).
Keep `RELAY_URL_A` and `RELAY_URL_B` exported: the one-time enrollment CLI
must receive the same custom relay configuration explicitly.

## 6. Enroll the node

Use [Enroll a node](../how-to/enroll-node.md), passing the same sealing
descriptors and dedicated relay values. The one-time enrollment command must
use custom relay mode; configuring a later systemd drop-in cannot change that
already-completed CLI operation. Return here after enrollment prints the new
`NODE_ID`.

## 7. Configure the Agent after enrollment

After enrollment prints the pending UUIDv7 node ID, write that value, the
node's prepared EndpointID, and the same descriptor values to
`/etc/ocservia-agent/agent.env`:

```text
CONTROLLER_ENDPOINT_ID=<64-lowercase-hex-controller-endpoint-id>
NODE_ID=<node-uuidv7-returned-by-enrollment>
AGENT_ENDPOINT_ID=<64-lowercase-hex-endpoint-id-printed-when-the-identity-was-prepared>
CONTROLLER_COMMAND_VERIFICATION_KEY_FILE=/etc/ocservia-agent/controller-command-verification-key.pem
USER_PASSWORD_SEAL_KEY_ID=<same-user-key-id-used-during-enrollment>
USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=<same-user-key-hash-used-during-enrollment>
P12_PASSWORD_SEAL_KEY_ID=<same-p12-key-id-used-during-enrollment>
P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256=<same-p12-key-hash-used-during-enrollment>
```

`AGENT_ENDPOINT_ID` records the EndpointID the one-time enrollment token
bound. Rerunning `deploy/managed-node/install.sh` compares the loaded
identity against this binding and fails closed if the endpoint key no longer
derives it.

Provision the Controller command verification key as a root-owned key readable
by the `ocserv-agent` group. Keep both sealing private keys root-owned mode
`0600`; their exact paths are provided by the installed `privd.env`. The two
sealing keys must remain distinct. Exact ownership, link, and ancestry rules
are in [Agent lifecycle reference](../operations/agent-lifecycle.md).

## 8. Approve and start the node

Return to [Approve the node](../how-to/enroll-node.md#approve-the-node) and
submit the approval only after `agent.env`, the sealing keys, and the
production relay path are configured. Then enable both units — this is the
deliberate activation step, and it stays outside the bootstrap:

```bash
sudo systemctl enable --now ocservia-privd.service ocservia-agent.service
systemctl status ocservia-privd.service ocservia-agent.service
```

Confirm both units are active and the node appears online with a fresh
observation in the Controller inventory. Rerunning
`deploy/managed-node/install.sh` at this point is a read-only convergence
check: it prints `SERVICES_ACTIVE` and changes nothing. If enrollment or
startup fails, do not replace the identity directory or generate a new
Controller trust key just to retry; see
[Troubleshooting](../how-to/troubleshooting.md).
