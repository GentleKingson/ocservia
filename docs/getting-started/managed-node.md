# Install a managed node

A managed node is an existing ocserv server with the ocservia node daemons installed alongside it. The local ocserv instance still terminates VPN tunnels and forwards traffic. The ocservia services handle enrollment, health telemetry, and signed administrative commands from the Controller.

This guide covers the standard package-first installation. The [Agent package lifecycle reference](../operations/agent-lifecycle.md) covers package verification, manual archive installation, rollback, and uninstallation.

When you install a fresh node, use published node artifacts from the same release as your Controller. The commands below use `vX.Y.Z` as the release tag. They also accept an exact `vX.Y.Z-rc.N` candidate tag (with a positive `N` and no leading zeros), but release candidates are not recommended for stable production use.

The v1.1.0 policy removed the software-version admission checks. That does not guarantee cross-version compatibility, and historical deployments and keys are not converted automatically. Never reset an existing node's identity to force an installation through. See the [support policy](../reference/support-policy.md) for details.

## Requirements

- A deployed Controller reachable through the production Relay.
- The Controller's `EndpointID`.
- A Linux host running systemd with root access and `ocserv` installed (or ready to be managed).
- One of the supported managed-node platforms:
  - Ubuntu 22.04/24.04/26.04 or Debian 12/13 on `x86_64` or `aarch64` using native `.deb` packages.
  - Rocky Linux 9 on `x86_64` or `aarch64` using native `.rpm` packages.
- Protected local files containing the Relay access token and the Controller command verification key.
- An optional bootstrap token file if you want the installer to complete local enrollment in the same run.
- A non-root launcher account with `sudo` privileges. The installer elevates individual commands with `sudo`; pass `--root-lifecycle` only when you intend to run the whole script as root.
- `curl`, `openssl`, `sha256sum`, `awk`, `git`, and outbound HTTPS access to GitHub Releases.

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

The installer reads `./install.env` from the current working directory. Variables exported in the shell take precedence over entries in the file.

If the Relay uses a private CA, install its public PEM certificate bundle at `/etc/ocservia-agent/relay-ca.pem` before you run the installer. The file must be:

- a non-empty regular file with a single link,
- owned by `root:root` with mode `0444`,
- under real, root-owned directories that are not group- or world-writable.

The installer and the installed service launcher both read this file. Neither downloads nor overwrites it. If the file is absent, the standard public root CAs are trusted. If the file has unsafe ownership or permissions, or contains malformed PEM data, the process fails closed and TLS verification stays active.

The bundle adds trusted root CAs; it is not a certificate pin. Rotating or removing it is a separate operator action, and neither a package update nor an installer rerun does it for you.

## 2. Run the installer

Invoke the managed-node installer from the pinned release checkout:

```bash
../ocservia-vX.Y.Z/deploy/managed-node/install.sh
```

To run the entire installation lifecycle directly as `root`, pass `--root-lifecycle`:

```bash
../ocservia-vX.Y.Z/deploy/managed-node/install.sh --root-lifecycle
```

The script then:

1. Detects the host distribution.
2. Downloads the matching `.deb` or `.rpm` over HTTPS into a root-owned staging directory and installs it.
3. Runs the `privd` host preflight checks.
4. Generates the two password-sealing keys if they do not exist yet. Existing keys are preserved.
5. Writes the relay configuration and initializes the persistent node identity.

Enrollment and the running services both use the single configured Relay URL. A non-empty `RELAY_URL_B` is rejected before installation or startup.

The installer enables and starts the history-retention timer (`ocservia-agent-retention.timer`). It leaves the node unapproved and the `Agent` and `privd` services stopped. If a different version of `ocservia-agent` is already installed on the host, the installer exits without upgrading or downgrading the package.

## 3. Finish enrollment

What happens next depends on whether `BOOTSTRAP_TOKEN_SOURCE` was supplied:

| Installer result | What to do next |
| --- | --- |
| `ENROLLED_LOCAL` | Local enrollment succeeded. The installer does not query Controller state, so check the node's approval and connection status in the Controller UI. |
| `ENROLLMENT_READY` | No token was provided. Issue an endpoint-bound enrollment token (`expected_endpoint_id` set to the printed EndpointID), write it to `/etc/ocservia-agent/enrollment-token` owned by `root:ocserv-agent` with mode `0640` (or point `BOOTSTRAP_TOKEN_SOURCE` at a protected bootstrap token file), and rerun the installer. |

If enrollment fails or the result is ambiguous, stop. Keep the existing EndpointID, identity directory, and token files as they are, then recover according to the token type:

- With `BOOTSTRAP_TOKEN_SOURCE`, rerun the installer with the same token file to recover the committed response.
- With an endpoint-bound token, replay recovery is not available. Check the Controller inventory for that EndpointID before you issue a replacement token (see [Enroll a node](../how-to/enroll-node.md)).

The installer deletes one-time token files and the `BOOTSTRAP_TOKEN_SOURCE` file only after enrollment succeeds.

Package installation, local enrollment, and active fleet management are separate stages. `ENROLLED_LOCAL` and `SERVICES_ACTIVE` describe local host state only; the installer reports `NOT_OBSERVED` for Controller trust, session connectivity, and telemetry freshness. A node is under active management only when the Controller shows it approved, connected, and reporting fresh telemetry. [Enroll a node](../how-to/enroll-node.md) describes token creation and approval on the Controller side.

## 4. Approve and start services

When the installer reports `ENROLLED_LOCAL`, review the pending node in the Controller and have a second authorized principal approve it. Starting the local daemons does not approve the node.

After approval, enable and start both services. The node then connects to the Controller and starts accepting signed commands:

```bash
sudo systemctl enable --now ocservia-privd.service ocservia-agent.service
systemctl status ocservia-privd.service ocservia-agent.service
```

Verify that both systemd units are active and that the node appears online with current health data in the Controller inventory.

## 5. Verify reruns

Once the node is approved and its services are running, you can rerun the same release installer with the original `install.env` and source paths. The rerun is idempotent and works as a convergence check:

```bash
../ocservia-vX.Y.Z/deploy/managed-node/install.sh
```

On an active node, the script reports `SERVICES_ACTIVE`. It does not reinstall the package, overwrite the identity, keys, relay configuration, or `agent.env`, re-approve the node, or restart the services. It does rerun the host preflight, verify the enrolled identity and trust files (failing closed on any discrepancy), and remove any leftover enrollment token file. You still need to check approval, connection state, and telemetry freshness in the Controller UI.

After the Agent has been upgraded or rolled back to a different version, do not run an older release's installer on the host. It stops on the version mismatch.

## Lifecycle after installation

- Upgrade nodes through the next native package release or the Controller-managed Agent upgrade workflow.
- Roll back to the matched package snapshot using `ocservia-agent-rollback`.
- Uninstall with `dpkg` or `rpm`. This stops and disables `Agent`, `privd`, and the retention timer, so the Controller shows the node offline. It removes the binaries and unit files but leaves the node identity, local state, and configuration files on disk.
- Never rerun an unpinned or `latest` installer script as a shortcut for upgrading.

## Next steps

- [Enroll a node](../how-to/enroll-node.md)
- [Dedicated Relay](../how-to/dedicated-relay.md)
- [Upgrade the Agent](../how-to/agent-lifecycle.md#upgrade)
- [Roll back the Agent](../how-to/agent-lifecycle.md#rollback)
- [Agent package lifecycle reference](../operations/agent-lifecycle.md)
