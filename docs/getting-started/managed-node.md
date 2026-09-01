# Install a managed node

A managed node runs the unprivileged ocservia Agent beside `privd`, a small
root service for fixed typed ocserv operations. Install a published package;
do not build a package on the node.

## Before you begin

- The Controller is deployed and its EndpointID is available.
- The node is a Linux host with systemd and root access.
- The host architecture is `x86_64` or `aarch64`.
- You have the Controller command verification key and the two distinct
  password-sealing keys supplied through a protected channel.
- You can create a one-time token and approve the node. Follow [Enroll a
  node](../how-to/enroll-node.md) when you are ready.

## 1. Choose the release package

Download the package for the exact release from the [GitHub Releases](https://github.com/GentleKingson/ocservia/releases)
page:

| Host | Package |
| --- | --- |
| Debian or Ubuntu, `x86_64` | `ocservia-agent_<version>_amd64.deb` |
| Debian or Ubuntu, `aarch64` | `ocservia-agent_<version>_arm64.deb` |
| RPM-based Linux, `x86_64` | `ocservia-agent-<version>-1.x86_64.rpm` |
| RPM-based Linux, `aarch64` | `ocservia-agent-<version>-1.aarch64.rpm` |

The release also publishes signed `tar.gz` archives for advanced or manual
installation. Use the native package unless that path is not available for
your host. Verify the release key fingerprint out of band; the package
scriptlet then verifies its embedded signed archive before handing control to
the repository installer.

## 2. Install the package

On Debian or Ubuntu:

```bash
sudo dpkg -i ./ocservia-agent_<version>_<arch>.deb
```

On an RPM-based system:

```bash
sudo rpm -ivh ./ocservia-agent-<version>-1.<rpm-arch>.rpm
```

The package checks the host architecture and installs the Agent, `privd`, the
durable upgrader, and their systemd units. It does not enable or start either
service.

## 3. Configure the Agent

Provision the Controller verification key and two purpose-separated private
keys, then edit `/etc/ocservia-agent/agent.env` with the values returned by
enrollment:

```text
CONTROLLER_ENDPOINT_ID=<64-lowercase-hex-characters>
NODE_ID=<node-uuidv7>
CONTROLLER_COMMAND_VERIFICATION_KEY_FILE=/etc/ocservia-agent/controller-command-verification-key.pem
USER_PASSWORD_SEAL_KEY_ID=<user-key-id>
USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=<64-lowercase-hex-characters>
P12_PASSWORD_SEAL_KEY_ID=<p12-key-id>
P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256=<64-lowercase-hex-characters>
```

Install the private key files as root-owned mode-`0600` files. The verification
key must be root-owned and readable by the `ocserv-agent` group. The two
sealing keys must be distinct. Exact ownership, link, and ancestry rules are
in [Agent lifecycle reference](../operations/agent-lifecycle.md).

For a production node using dedicated relays, configure the relay drop-in,
`/etc/ocservia-agent/relays.env`, and
`/etc/ocservia-agent/relay-access-token` before starting the services. The
production relay setup is described in [Dedicated relays](../operations/dedicated-relays.md).

## 4. Enroll and start the node

Use [Enroll a node](../how-to/enroll-node.md) with the same persistent identity
directory. After the Controller shows the node as approved, start both units:

```bash
sudo systemctl enable --now ocservia-privd.service ocservia-agent.service
systemctl status ocservia-privd.service ocservia-agent.service
```

## 5. Verify

Confirm both units are active and the node appears online with a fresh
observation in the Controller inventory. If enrollment or startup fails, do
not replace the identity directory or generate a new Controller trust key just
to retry; see [Troubleshooting](../how-to/troubleshooting.md).
