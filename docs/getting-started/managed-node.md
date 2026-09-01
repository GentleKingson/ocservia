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
- For a production node using dedicated relays, a clean checkout of the exact
  release tag is available for the relay drop-in files, or you will use the
  verified archive installation path described in the lifecycle reference.

## 1. Verify and choose the release package

Download the package, `SHA256SUMS`, and `SHA256SUMS.sig` for one exact release
from [GitHub Releases](https://github.com/GentleKingson/ocservia/releases).
Provision the trusted public key through a separate channel. Do not use only
the `release-signing.pub.pem` downloaded beside the package.

Set the package variables for the one file you will install:

```bash
export RELEASE_DIR=/protected/ocservia-agent-v0.3.0
export VERSION=0.3.0
export AGENT_PACKAGE="ocservia-agent_${VERSION}_amd64.deb"
export TRUSTED_RELEASE_KEY=/etc/ocservia/release-signing.pub.pem
export EXPECTED_RELEASE_KEY_SHA256="replace-with-64-lowercase-hex-fingerprint"
```

Set `AGENT_PACKAGE` to the exact package for the host: use
`ocservia-agent_${VERSION}_arm64.deb` on Debian or Ubuntu ARM, or
`ocservia-agent-${VERSION}-1.x86_64.rpm` / `ocservia-agent-${VERSION}-1.aarch64.rpm`
on RPM-based hosts. Verify the trusted key, the signed release manifest, and
the selected package before invoking any package manager as root:

```bash
test "$(openssl pkey -pubin -in "$TRUSTED_RELEASE_KEY" -outform DER \
  | sha256sum | awk '{print $1}')" = "$EXPECTED_RELEASE_KEY_SHA256"
(
  cd "$RELEASE_DIR"
  openssl pkeyutl -verify -rawin -pubin \
    -inkey "$TRUSTED_RELEASE_KEY" \
    -in SHA256SUMS -sigfile SHA256SUMS.sig
  grep -F "  ${AGENT_PACKAGE}" SHA256SUMS | sha256sum -c --strict -
)
```

The package's post-install verification is defense in depth. It must not be
the first trust decision for an unverified native package.

## 2. Install the package

On Debian or Ubuntu:

```bash
sudo dpkg -i "$RELEASE_DIR/$AGENT_PACKAGE"
```

On an RPM-based system:

```bash
sudo rpm -ivh "$RELEASE_DIR/$AGENT_PACKAGE"
```

The package installs the Agent, `privd`, the durable upgrader, and their
systemd units. It does not enable or start either service. A fresh native
package install also does not install the production relay drop-in; complete
step 5 before starting a production node.

## 3. Prepare sealing keys and enroll the node

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
Use [Enroll a node](../how-to/enroll-node.md), passing the same IDs and hashes,
then return here before approval.

## 4. Configure the Agent after enrollment

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

## 5. Install the production relay path

Do not start a production Agent with the base unit alone. For a native package
fresh install, install the checked-in drop-in from the same exact release
checkout before enabling the service:

```bash
cd /path/to/the-exact-release-checkout
sudo install -d -m 0755 \
  /usr/lib/systemd/system/ocservia-agent.service.d
sudo install -m 0644 \
  ./deploy/production/systemd/ocservia-agent-relays.conf \
  /usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf
sudo install -o root -g ocserv-agent -m 0640 \
  ./deploy/production/systemd/relays.env.example \
  /etc/ocservia-agent/relays.env
```

Set both dedicated HTTPS relay URLs in `/etc/ocservia-agent/relays.env` and
install the same protected token as `/etc/ocservia-agent/relay-access-token`
with owner `root:ocserv-agent` and mode `0640`. Use the exact ownership and
certificate requirements in [Dedicated relays](../how-to/dedicated-relays.md),
then run:

```bash
sudo systemctl daemon-reload
```

Alternatively, install the signed archive through the lifecycle reference with
`INSTALL_PRODUCTION_RELAYS=true`; that supported path installs the drop-in as
part of the verified install. Do not start the native package until either
that path or the explicit drop-in path above is complete.

## 6. Approve and start the node

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
