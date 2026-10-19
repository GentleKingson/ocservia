# Install a managed node

Release version examples also accept an exact `vX.Y.Z-rc.N` candidate tag (with a positive `N` and no leading zeros), though release candidates are not recommended for stable production use.

A managed node is an existing ocserv server with the ocservia node daemons installed alongside it. The local ocserv instance continues to terminate VPN tunnels and forward traffic, while the ocservia services handle enrollment, health telemetry, and signed administrative commands from the Controller.

This guide covers the standard package-first installation. Package verification details, manual archive installation, rollback, and uninstallation are covered in the [Agent package lifecycle reference](../operations/agent-lifecycle.md).

Always pair matched, published Controller and node artifacts when installing a fresh node. While the v1.1.0 policy removed software-version admission checks, it does not guarantee cross-version compatibility or provide automatic conversion for historical deployments and keys. Never reset an existing node's identity to force an installation through; see the [support policy](../reference/support-policy.md).

## Requirements

- A deployed Controller reachable through the production Relay.
- The Controller's `EndpointID`.
- A Linux host running systemd with root access and `ocserv` installed (or ready to be managed).
- One of the supported managed-node platforms:
  - Ubuntu 22.04/24.04/26.04 or Debian 12/13 on `x86_64` or `aarch64` using native `.deb` packages.
  - Rocky Linux 9 on `x86_64` or `aarch64` using native `.rpm` packages.
- Protected local files containing the Relay access token and the Controller command verification key.
- An optional bootstrap token file if you want the installer to complete local enrollment in the same run.
- A non-root launcher account with `sudo` privileges (the installer elevates individual commands via `sudo`; pass `--root-lifecycle` only when intentionally running the entire script as root), plus `curl`, `openssl`, `sha256sum`, `awk`, `git`, and outbound HTTPS access to GitHub Releases.

Ubuntu 20.04 and Debian 11 fall outside the verified native runtime baseline and are not supported for managed nodes.

## 1. Prepare the node configuration

Check out the exact release tag and create a working directory for node configuration outside the repository tree:

```bash
git clone --branch vX.Y.Z --single-branch --depth 1 \
  https://github.com/GentleKingson/ocservia.git ocservia-vX.Y.Z
mkdir ocservia-node-install && cd ocservia-node-install
cp ../ocservia-vX.Y.Z/install.env.example install.env
editor install.env
```

Fill in the managed-node section of `install.env` and leave the Controller section commented out or removed.

At a minimum, set the following parameters:

| Setting | Purpose |
| --- | --- |
| `CONTROLLER_ENDPOINT_ID` | Binds the node to the expected Controller identity. |
| `RELAY_URL_A`, `RELAY_URL_B` | Required literal HTTPS URL for Relay A (without credentials, query string, or fragment); omit `RELAY_URL_B` or leave it empty. Production supports only one dedicated Relay, and any non-empty B value (in `install.env` or an existing `relays.env`) is rejected. |
| `RELAY_ACCESS_TOKEN_SOURCE` | Protected file path containing the relay access token. |
| `CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE` | Protected file path containing the Controller command verification public key. |
| `BOOTSTRAP_TOKEN_SOURCE` | Optional protected file path containing a bootstrap token for single-run enrollment. |

The installer reads `./install.env` from the current working directory, with exported shell variables taking precedence over file entries.

When the Relay uses a private CA, install its public PEM certificate bundle beforehand at `/etc/ocservia-agent/relay-ca.pem` as a non-empty, single-link regular file owned by `root:root` with mode `0444`, placed under root-owned real directories without group or world write permissions. Both the installer and the installed service launcher read this file without downloading or overwriting it. If the file is absent, standard public-root trust applies; if the file exists with unsafe ownership/permissions or malformed PEM data, the process fails closed while keeping TLS verification active. This bundle adds trusted root CAs and does not act as a certificate pin. Rotating or removing the CA bundle is a separate operator action outside package or installer reruns.

## 2. Run the installer

Invoke the managed-node installer from the pinned release checkout:

```bash
../ocservia-vX.Y.Z/deploy/managed-node/install.sh
```

To run the entire installation lifecycle directly as `root`, pass `--root-lifecycle`:

```bash
../ocservia-vX.Y.Z/deploy/managed-node/install.sh --root-lifecycle
```

During execution, the script detects the host distribution, downloads the matching `.deb` or `.rpm` over HTTPS into a root-owned staging directory, installs the package, runs the `privd` host preflight checks, generates the two password-sealing keys if they do not already exist (existing keys are preserved), writes the relay configuration, and initializes the persistent node identity. Both enrollment and normal service runtime use the single configured Relay URL, rejecting any non-empty `RELAY_URL_B` before installation or startup.

The installer enables and starts the history-retention timer (`ocservia-agent-retention.timer`), while leaving the node unapproved and the `Agent` and `privd` services stopped. If a different version of `ocservia-agent` is already installed on the host, the installer exits without upgrading or downgrading the package.

## 3. Finish enrollment

What happens next depends on whether `BOOTSTRAP_TOKEN_SOURCE` was supplied:

| Installer result | What to do next |
| --- | --- |
| `ENROLLED_LOCAL` | Local enrollment succeeded. Check the node's approval and connection status in the Controller UI, as the local installer does not query Controller state. |
| `ENROLLMENT_READY` | No token was provided. Issue an endpoint-bound enrollment token (`expected_endpoint_id` set to the printed EndpointID), write it to `/etc/ocservia-agent/enrollment-token` owned by `root:ocserv-agent` with mode `0640` (or point `BOOTSTRAP_TOKEN_SOURCE` at a protected bootstrap token file), and rerun the installer. |

If enrollment fails or returns an ambiguous result, stop and keep the existing EndpointID, identity directory, and token files intact. When using `BOOTSTRAP_TOKEN_SOURCE`, rerun the installer with the same token file to recover the committed response; when using an endpoint-bound token, replay recovery is unavailable, so check the Controller inventory for that EndpointID before issuing a replacement token (see [Enroll a node](../how-to/enroll-node.md)). One-time token files and `BOOTSTRAP_TOKEN_SOURCE` are deleted only after enrollment succeeds.

Package installation, local enrollment, and active fleet management are distinct stages: `ENROLLED_LOCAL` and `SERVICES_ACTIVE` reflect local host state only (the installer reports `NOT_OBSERVED` for Controller trust, session connectivity, and telemetry freshness). A node is under active management only once the Controller shows it approved, connected, and reporting fresh telemetry. Controller-side token creation and approval workflows are detailed in [Enroll a node](../how-to/enroll-node.md).

## 4. Approve and start services

Once the installer outputs `ENROLLED_LOCAL`, review the pending node in the Controller and approve it using a second authorized principal. Starting the local daemons does not auto-approve the node. After approval, enable and start both services so the node connects to the Controller and begins accepting signed commands:

```bash
sudo systemctl enable --now ocservia-privd.service ocservia-agent.service
systemctl status ocservia-privd.service ocservia-agent.service
```

Verify that both systemd units are active and that the node appears online with current health data in the Controller inventory.

## 5. Verify reruns

After the node is approved and its services are running, you can rerun the same release installer with the original `install.env` and source paths as an idempotent convergence check:

```bash
../ocservia-vX.Y.Z/deploy/managed-node/install.sh
```

On an active node, the script outputs `SERVICES_ACTIVE` without reinstalling the package, overwriting the identity, keys, relay config, or `agent.env`, re-approving the node, or restarting the services. Instead, it re-executes the host preflight, verifies the enrolled identity and trust files (failing closed on any discrepancy), and cleans up any leftover enrollment token file. Controller-side approval, connection state, and telemetry freshness must still be checked in the Controller UI. Once the Agent has been upgraded or rolled back to a different version, do not run an older release's installer against the host, as it will halt on the version mismatch.

## Lifecycle after installation

- Upgrade nodes through the next native package release or the Controller-managed Agent upgrade workflow.
- Roll back to the matched package snapshot using `ocservia-agent-rollback`.
- Uninstall via `dpkg` or `rpm`, which stops and disables `Agent`, `privd`, and the retention timer (marking the node offline in the Controller) and removes the binaries and unit files while leaving node identity, local state, and configuration files intact on disk.
- Never rerun an unpinned or `latest` installer script as a shortcut for upgrading.

## Next steps

- [Enroll a node](../how-to/enroll-node.md)
- [Dedicated Relay](../how-to/dedicated-relay.md)
- [Upgrade the Agent](../how-to/agent-lifecycle.md#upgrade)
- [Roll back the Agent](../how-to/agent-lifecycle.md#rollback)
- [Agent package lifecycle reference](../operations/agent-lifecycle.md)
