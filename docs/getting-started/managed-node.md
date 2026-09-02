# Install a managed node

A managed node runs the unprivileged ocservia Agent beside `privd`, a small
root service for fixed typed ocserv operations. Install a published package;
do not build a package on the node.

## Before you begin

- The Controller is deployed and its EndpointID is available.
- The node is a Linux host with systemd and root access.
- The host architecture is `x86_64` or `aarch64`.
- You have an independently trusted release public key and its expected
  fingerprint.
- You can create a one-time token and approve the node. Follow [Enroll a
  node](../how-to/enroll-node.md) after preparing the sealing keys below.
- For a production node using dedicated relays, use the signed Agent archive
  installation path below or the native package with the production request
  marker below. Do not install production systemd files from an
  arbitrary source checkout.

## 1. Verify and choose the release package

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

## 2. Install the production Agent from the signed archive

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

On Debian or Ubuntu:

```bash
sudo dpkg -i "$RELEASE_DIR/$AGENT_PACKAGE"
```

On an RPM-based system:

```bash
sudo rpm -ivh "$RELEASE_DIR/$AGENT_PACKAGE"
```

The package installs the Agent, `privd`, the durable upgrader, and their
systemd units. It does not enable or start either service. For a production
node, create the one-shot production request before invoking the package
manager; the verified embedded payload then installs the production relay
drop-in and `relays.env`:

```bash
sudo install -d -o root -g root -m 0755 /etc/ocservia
sudo touch /etc/ocservia/agent-install-production-relays
```

The successful install consumes the request marker, and the installed relay
drop-in keeps later package upgrades on the production relay contract. Do not
copy `deploy/production/systemd` files from a source checkout into
`/usr/lib/systemd/system` under either path.

## 3. Prepare sealing keys

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

## 4. Configure dedicated relays before enrollment

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

## 5. Enroll the node

Use [Enroll a node](../how-to/enroll-node.md), passing the same sealing
descriptors and dedicated relay values. The one-time enrollment command must
use custom relay mode; configuring a later systemd drop-in cannot change that
already-completed CLI operation. Return here after enrollment prints the new
`NODE_ID`.

## 6. Configure the Agent after enrollment

After enrollment prints the pending UUIDv7 node ID, write that value and the
same descriptor values to `/etc/ocservia-agent/agent.env`:

```text
CONTROLLER_ENDPOINT_ID=<64-lowercase-hex-controller-endpoint-id>
NODE_ID=<node-uuidv7-returned-by-enrollment>
CONTROLLER_COMMAND_VERIFICATION_KEY_FILE=/etc/ocservia-agent/controller-command-verification-key.pem
USER_PASSWORD_SEAL_KEY_ID=<same-user-key-id-used-during-enrollment>
USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=<same-user-key-hash-used-during-enrollment>
P12_PASSWORD_SEAL_KEY_ID=<same-p12-key-id-used-during-enrollment>
P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256=<same-p12-key-hash-used-during-enrollment>
```

Provision the Controller command verification key as a root-owned key readable
by the `ocserv-agent` group. Keep both sealing private keys root-owned mode
`0600`; their exact paths are provided by the installed `privd.env`. The two
sealing keys must remain distinct. Exact ownership, link, and ancestry rules
are in [Agent lifecycle reference](../operations/agent-lifecycle.md).

## 7. Approve and start the node

Return to [Approve the node](../how-to/enroll-node.md#approve-the-node) and
submit the approval only after `agent.env`, the sealing keys, and the
production relay path are configured. Then enable both units:

```bash
sudo systemctl enable --now ocservia-privd.service ocservia-agent.service
systemctl status ocservia-privd.service ocservia-agent.service
```

Confirm both units are active and the node appears online with a fresh
observation in the Controller inventory. If enrollment or startup fails, do
not replace the identity directory or generate a new Controller trust key just
to retry; see [Troubleshooting](../how-to/troubleshooting.md).
