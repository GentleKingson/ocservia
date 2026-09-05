# Install a managed node

A managed node is an existing ocserv server with the ocservia node services installed beside it. The node still runs ocserv for VPN traffic. ocservia adds controlled management, health reporting, enrollment, and lifecycle operations from the Controller.

This guide covers the normal package-first installation path. Detailed package verification, manual archive installation, rollback, and uninstall behavior remain in the [Agent package lifecycle reference](../operations/agent-lifecycle.md).

## Requirements

- The Controller is deployed and reachable through the production relays.
- The Controller EndpointID is available.
- The node is a Linux host with systemd, root access, and ocserv installed or ready to be managed.
- Supported managed-node platforms are:
  - Ubuntu 22.04/24.04/26.04 or Debian 12/13 on `x86_64` or `aarch64` using native `.deb` packages.
  - Rocky Linux 9 on `x86_64` or `aarch64` using native `.rpm` packages.
- The release-signing public key and expected SHA-256 fingerprint are provisioned through a protected channel.
- Relay access token and Controller command verification key files are available on the node through protected paths.
- A bootstrap token is available if you want the installer to enroll the node in the same run.

Ubuntu 20.04 and Debian 11 are not supported for managed nodes because the native package verification path requires OpenSSL 3.

## 1. Prepare the node configuration

Use an exact release tag and keep node configuration outside the release checkout:

```bash
git clone --branch vX.Y.Z --single-branch --depth 1 \
  https://github.com/GentleKingson/ocservia.git ocservia-vX.Y.Z
mkdir ocservia-node-install && cd ocservia-node-install
cp ../ocservia-vX.Y.Z/install.env.example install.env
editor install.env
```

Edit only the managed-node section in `install.env`. Delete or leave commented the Controller section.

Configure at least:

| Setting | Purpose |
| --- | --- |
| `CONTROLLER_ENDPOINT_ID` | Binds this node to the expected Controller identity. |
| `RELAY_URL_A`, `RELAY_URL_B` | Production relay URLs. |
| `RELAY_ACCESS_TOKEN_SOURCE` | Protected source file for the relay token. |
| `CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE` | Protected source file for the Controller command verification key. |
| `TRUSTED_RELEASE_KEY` | Trusted release-signing public key. |
| `EXPECTED_RELEASE_KEY_SHA256` | Expected fingerprint of the trusted release key. |
| `BOOTSTRAP_TOKEN_SOURCE` | Optional protected source file for one-run enrollment. |

The installer reads `./install.env` from the current directory. Shell variables override values from the file.

## 2. Run the installer

Run the managed-node installer from the exact release checkout:

```bash
../ocservia-vX.Y.Z/deploy/managed-node/install.sh
```

For a deliberate whole-lifecycle-as-root run, add `--root-lifecycle`:

```bash
../ocservia-vX.Y.Z/deploy/managed-node/install.sh --root-lifecycle
```

The installer detects the platform, downloads the matching `.deb` or `.rpm`, verifies the release trust, installs the package, prepares node state, writes relay configuration, and prepares the persistent node identity.

It does not approve the node and does not enable or start services.

## 3. Finish enrollment

The next step depends on whether `BOOTSTRAP_TOKEN_SOURCE` was configured.

| Installer result | What to do next |
| --- | --- |
| `PENDING_APPROVAL` | The node enrolled successfully and is waiting for Controller approval. Continue to approval. |
| `ENROLLMENT_READY` | Create an endpoint-bound bootstrap token for the printed EndpointID, place it at `/etc/ocservia-agent/enrollment-token` as `root:ocserv-agent` with mode `0640`, and rerun the same installer. |

Use [Enroll a node](../how-to/enroll-node.md) for the Controller-side token and approval steps.

## 4. Approve and start services

After the node reaches `PENDING_APPROVAL`, approve it in the Controller. Then start the node services deliberately:

```bash
sudo systemctl enable --now ocservia-privd.service ocservia-agent.service
systemctl status ocservia-privd.service ocservia-agent.service
```

Confirm that both services are active and that the node appears online in the Controller inventory with fresh health data.

## 5. Verify reruns

After approval and service activation, rerun the same installer as a read-only convergence check:

```bash
../ocservia-vX.Y.Z/deploy/managed-node/install.sh
```

A healthy already-active node should report `SERVICES_ACTIVE` without reinstalling the package, replacing identity files, approving the node, or starting services again.

## Lifecycle after installation

- Upgrade with the next signed native package or the Controller-driven Agent upgrade workflow.
- Roll back with the matched package snapshot through `ocservia-agent-rollback`.
- Uninstall through `dpkg` or `rpm`; package scripts preserve identity, state, and configuration by default.
- Do not rerun a `latest` convenience installer as an implicit upgrade.

## Next steps

- [Enroll a node](../how-to/enroll-node.md)
- [Dedicated relays](../how-to/dedicated-relays.md)
- [Upgrade the Agent](../how-to/agent-upgrade.md)
- [Roll back the Agent](../how-to/agent-rollback.md)
- [Agent package lifecycle reference](../operations/agent-lifecycle.md)
