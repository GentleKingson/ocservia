# Install a managed node

Release version examples also accept an exact `vX.Y.Z-rc.N` candidate tag
(positive N without leading zeros). RCs are not recommended stable releases.

A managed node is an existing ocserv server with the ocservia node services installed beside it. The node still runs ocserv for VPN traffic. ocservia adds controlled management, health reporting, enrollment, and lifecycle operations from the Controller.

This guide covers the normal package-first installation path. Detailed package verification, manual archive installation, rollback, and uninstall behavior remain in the [Agent package lifecycle reference](../operations/agent-lifecycle.md).

Select matched published Controller and node artifacts for a fresh installation.
The v1.1.0 policy removes source-version windows but does not guarantee safe
cross-version operation or provide historical deployment/key conversion. Do not
reset an existing node's identity to make installation succeed. See the
[support policy](../reference/support-policy.md).

## Requirements

- The Controller is deployed and reachable through the production relays.
- The Controller EndpointID is available.
- The node is a Linux host with systemd, root access, and ocserv installed or ready to be managed.
- Supported managed-node platforms are:
  - Ubuntu 22.04/24.04/26.04 or Debian 12/13 on `x86_64` or `aarch64` using native `.deb` packages.
  - Rocky Linux 9 on `x86_64` or `aarch64` using native `.rpm` packages.
- Relay access token and Controller command verification key files are available on the node through protected paths.
- A bootstrap token is available if you want the installer to enroll the node in the same run.

Ubuntu 20.04 and Debian 11 are not supported for managed nodes because they are outside the verified native runtime baseline.

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
| `RELAY_URL_A`, `RELAY_URL_B` | Required HTTPS A; omit B or leave it empty. Nonempty B is rejected. |
| `RELAY_ACCESS_TOKEN_SOURCE` | Protected source file for the relay token. |
| `CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE` | Protected source file for the Controller command verification key. |
| `BOOTSTRAP_TOKEN_SOURCE` | Optional protected source file for one-run enrollment. |

The installer reads `./install.env` from the current directory. Shell variables override values from the file.

For a private Relay CA, provision its public PEM certificate bundle separately
at `/etc/ocservia-agent/relay-ca.pem` before enrollment. It must be a nonempty,
single-link regular file owned by `root:root`, mode `0444`, under root-owned
real directories without group/world write permission. The installer and
shipped service launcher use the same additional trust file; neither downloads
nor replaces it. An absent file retains public-root trust. An unsafe present
file fails closed; malformed PEM is rejected rather than disabling TLS.
This adds trusted roots, not a certificate pin. CA rotation/removal is a
deliberate operator action, separate from package or installer reruns.

## 2. Run the installer

Run the managed-node installer from the exact release checkout:

```bash
../ocservia-vX.Y.Z/deploy/managed-node/install.sh
```

For a deliberate whole-lifecycle-as-root run, add `--root-lifecycle`:

```bash
../ocservia-vX.Y.Z/deploy/managed-node/install.sh --root-lifecycle
```

The installer detects the platform, downloads the matching `.deb` or `.rpm` over HTTPS, freezes it in root-owned staging, installs the package, prepares node state, writes relay configuration, and prepares the persistent node identity. Enrollment and the installed service use the same sole Relay URL. Any nonempty B is rejected before installation or execution.

It does not approve the node and does not enable or start services.

## 3. Finish enrollment

The next step depends on whether `BOOTSTRAP_TOKEN_SOURCE` was configured.

| Installer result | What to do next |
| --- | --- |
| `ENROLLED_LOCAL` | Local enrollment is complete. Check the node's approval and connection state in the Controller; the installer does not observe them. |
| `ENROLLMENT_READY` | Create an endpoint-bound bootstrap token for the printed EndpointID, place it at `/etc/ocservia-agent/enrollment-token` as `root:ocserv-agent` with mode `0640`, and rerun the same installer. |

Use [Enroll a node](../how-to/enroll-node.md) for the Controller-side token and approval steps.

## 4. Approve and start services

After the installer reports `ENROLLED_LOCAL`, check the node in the Controller and approve it if pending. Then start the node services deliberately:

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

An already-active node should report `SERVICES_ACTIVE` without reinstalling the package, replacing identity files, approving the node, or starting services again. This confirms that both local services are enabled and active; verify approval, connectivity, and fresh telemetry separately in the Controller.

## Lifecycle after installation

- Upgrade with the next native package or the Controller-driven Agent upgrade workflow.
- Roll back with the matched package snapshot through `ocservia-agent-rollback`.
- Uninstall through `dpkg` or `rpm`; package scripts preserve identity, state, and configuration by default.
- Do not rerun a `latest` convenience installer as an implicit upgrade.

## Next steps

- [Enroll a node](../how-to/enroll-node.md)
- [Dedicated Relay](../how-to/dedicated-relay.md)
- [Upgrade the Agent](../how-to/agent-lifecycle.md#upgrade)
- [Roll back the Agent](../how-to/agent-lifecycle.md#rollback)
- [Agent package lifecycle reference](../operations/agent-lifecycle.md)
