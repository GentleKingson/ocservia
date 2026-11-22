# Agent lifecycle

This is the operations manual for a managed node: install, enroll, activate, start, upgrade, rollback and removal.

A managed node is an existing ocserv server with the ocservia node daemons installed alongside it. The local ocserv instance still terminates VPN tunnels and forwards traffic. The ocservia services handle enrollment, health telemetry, and signed administrative commands from the Controller.

The v1.1.0 policy removed the software-version admission checks. That does not guarantee cross-version compatibility, and historical deployments and keys are not converted automatically. Operators must provision the target explicitly or redeploy without overwriting retained identity and persistent state. Never reset an existing node's identity to force an installation through. See the [version policy](production-deployment.md#version-policy).

## Node states

The token installer completes package installation, registration, activation,
root-key registration and startup in one run. It prints `ONLINE` only after
the Controller reports active trust, a registered privd key, an online session
and fresh telemetry. A local service process alone is not an online result.

Manual Agent CLI enrollment reports only a local binding. It does not establish
Controller reachability; use the documented recovery steps below and verify
Controller-observed state before declaring the node ready.

## Install a managed node

This section covers the standard package-first installation. Use published node artifacts from the same release as your Controller. The commands use `vX.Y.Z` as the release tag. See the [version policy](production-deployment.md#version-policy) for the tags they accept and the [release policy](../development/release-checks.md#release-policy) for RC naming and native package validation.

### Requirements

- A deployed HTTPS Controller and one dedicated production Relay.
- One Admin session to issue the installation token in the intended Workspace.
- A supported Linux node with systemd, root access and the ocserv host prerequisites
  in [Supported platforms](production-deployment.md#supported-platforms).
- `curl`, `openssl`, `sha256sum`, `awk`, `flock`, Python 3 and outbound HTTPS to
  the Controller and GitHub Releases. Python uses only its standard library.
- Host trust for the Controller HTTPS certificate. Install your private CA through
  the operating system trust mechanism when applicable; never disable TLS checks.
- Use the DNS servers specified by the deployment in the ocserv configuration.
  Leave `dnsmasq` out of node provisioning. Before removing an existing
  `dnsmasq` installation, check whether any other service depends on it.
  With a VPN client connected, resolve a hostname through the configured DNS
  and confirm the client's public egress matches the intended routing policy.

### One-token installation

In Nodes → Install node, enter the node name, environment and matching published
release. Generate a token, then run the displayed pinned installer command on
the node:

```bash
wget --https-only --secure-protocol=TLSv1_2 \
  https://github.com/GentleKingson/ocservia/releases/download/vX.Y.Z/managed-node-bootstrap.sh \
  --output-document=install.sh
bash ./install.sh --join-token --version vX.Y.Z
```

Paste the token at the hidden prompt. No administrator cookie, second account,
Relay secret transfer or manual activation is required. Unattended callers use
`--join-token-file /protected/token` with a private regular file; never put the
token itself in argv, URLs or shell history.

The root installer retrieves bounded configuration over verified HTTPS without
following credential-bearing redirects. It verifies and installs the native
package, preserves or creates node identity and distinct sealing keys, enrolls
with a derived Agent-only credential, registers the root-owned privd key,
starts Agent/privd and polls Controller-observed readiness. The installation
Token remains root-only; possession of the Agent credential cannot authorize a
privd key registration or rotation.

For interrupted runs, keep `/var/lib/ocservia-bootstrap` and all node identity,
sealing and privd key files intact, then run the same pinned installer with
`--resume-install`. Exact enrollment and key-registration retries return the
original binding. Expiration still blocks a new enrollment or first key
registration; inspect the existing node before issuing replacement authority.
An expired token never permits a different node or replacement privd key.

The no-argument installer and `--interactive` use the same hidden token prompt
as `--join-token`. `--non-interactive` requires `--join-token-file` or
`--resume-install`; it never falls back to configuration-driven enrollment.

The configuration-driven installer and its questionnaire are retired. `--config`
fails with migration guidance, and `install.env` never selects a node installation
flow. Existing manual enrollment and uncertain-response recovery use the Agent
CLI described under [Enroll the node](#enroll-the-node). Preserve the original
EndpointID, sealing keys and privd key; do not mint replacement authority to
work around an ambiguous response.

### Private Relay CA

If the Relay uses a private CA, install its public PEM certificate bundle at `/etc/ocservia-agent/relay-ca.pem` before you run the installer. The file must be:

- a non-empty regular file with a single link,
- owned by `root:root` with mode `0444`,
- under real, root-owned directories that are not group- or world-writable.

The installer and the installed service launcher both read this file. Neither downloads nor overwrites it. If the file is absent, the standard public root CAs are trusted. If the file has unsafe ownership or permissions, or contains malformed PEM data, the process fails closed and TLS verification stays active.

The bundle adds trusted root CAs; it is not a certificate pin. Rotating or removing it is a separate operator action, and neither a package update nor an installer rerun does it for you.

### Production Relay launcher

To configure production relays during installation, set `INSTALL_PRODUCTION_RELAYS=true` and populate `/etc/ocservia-agent/relays.env` with the required HTTPS `RELAY_URL_A`. Production supports only one dedicated Relay. Install the relay token at `/etc/ocservia-agent/relay-access-token` as `root:ocserv-agent` mode `0640`, or set `RELAY_ACCESS_TOKEN_SOURCE` so the installer can install it.

The production drop-in executes the packaged fixed launcher `/usr/libexec/ocservia/ocservia-agent-relays`, which constructs a single Relay URL argument and replaces itself with the Agent, leaving base development units and direct binary invocations unchanged. Archive and native installations, upgrades, rollback snapshots, and uninstall all include this launcher. The lower-level package installer and upgrades preserve operator configuration and identity. Uninstall removes the launcher but retains configuration and state unless the existing purge option is used.

### Unattended installation and recovery

Use the installer from the same pinned Release and a private token file:

```bash
bash install.sh --non-interactive --join-token-file /protected/install-token
bash install.sh --non-interactive --resume-install
```

The published installer embeds its tag. Automation invoking an unstamped script must pass the exact tag with `--version` for both installation and resume.

Both commands use the same package, identity, enrollment and root-key registration flow
as the hidden prompt. A retry preserves root bootstrap state and the existing
keys. Only Controller-confirmed active trust, a registered privd key, an online
session and fresh telemetry produce `ONLINE`.

If host preflight fails, its `check=` field identifies the first failed check.
For example, `check=occtl.show-users-json` refers to `occtl --json show users`.
Run `sudo /usr/libexec/ocservia/ocservia-privd --host-preflight` on the node to
recheck after correcting the reported issue. Then use the same pinned installer
with `--resume-install` to continue from the protected state.

### Package verification and staging

For candidate packages delivered over SSH, transfer the exact architecture
and build to a temporary path on the target. On connection reset or broken
pipe, remove the partial file before retrying. Retry the transfer at most
twice, waiting 2 seconds and then 5 seconds. Stop after the final failure or
any error other than a connection reset or broken pipe. After a successful
transfer, verify the staged package against the original candidate's artifact
verification data before handing it to the package manager.

Once its external endpoint is deployed and verified, the operator-hosted thin first-install chain separates each stage (see [Bootstrap launcher hosting](production-deployment.md#bootstrap-launcher-hosting)):

```text
bootstrap launcher -> exact vX.Y.Z versioned installer -> HTTPS native package
        -> identity/sealing -> bootstrap enrollment (oit1_ -> obt2_)
        -> active node + privd attestation -> service activation
```

The bootstrap launcher and the initial versioned installer and native package downloads rely on HTTPS. Before invoking the package manager, the installer copies the downloaded native package into root-owned staging and verifies that its bytes remain unchanged. That transport check applies only to initial installation; an AgentUpgrade instead verifies against the SHA256 carried in the already-authorized Controller command. The `oit1_` installation token authorizes the complete automatic flow, issuing a derived `obt2_` Agent credential, immediately activating `nodes.status = 'active'`, and registering the root-owned `privd` attestation key via `/api/v1/node-bootstrap/{attestation,register}`. The fixed Release installer and native package need no source checkout or Git. The bootstrap launcher plays no role in ongoing lifecycle operations.

The verifier copies only the archive into a unique `root:root` mode `0700` directory below `/var/lib/ocservia-upgrade/package-staging`, compares the copied archive SHA256 with the caller-supplied expected digest, rejects unsafe archive paths and member types, and extracts that root-owned copy. Install and upgrade scripts accept only the verified directory printed by the verifier; never extract or run installers from a download directory, and remove the verified staging directory after the lifecycle operation succeeds. `PACKAGE_ARCH` pins the package architecture (`amd64` or `arm64`), which the MANIFEST records as `arch=`. On a real host install (no `DESTDIR`), the verifier also checks host architecture equivalence (`x86_64` for `amd64`, `aarch64` for `arm64`) and rejects a foreign-architecture package before anything is staged.

### Build a package locally

Formal Releases build inside the isolated Rocky Linux builder via `scripts/build-release-agent.sh` in the Release workflow after [Release Check](../development/release-checks.md) passes. For a local development build on a native Linux host, build all three node binaries natively on the package's target architecture, then create a deterministic package. Set one explicit SemVer `VERSION` so the compiled Rust binaries (`OCSERV_AGENT_RELEASE_VERSION`), `package-agent.sh` (`VERSION`, checked against each binary's `--version` output), the archive name, and the SHA-256 verification input all match:

```bash
: "${VERSION:?set the SemVer package version, such as 1.2.3}"
(cd rust && OCSERV_AGENT_RELEASE_VERSION="${VERSION}" cargo build --locked --release \
  --package ocservia-agent --package ocservia-privd --package ocservia-upgrader)
OUTPUT_DIR=dist \
  VERSION="${VERSION}" PACKAGE_ARCH=amd64 SOURCE_DATE_EPOCH=1786147200 scripts/package-agent.sh
# A plain checksum detects transfer corruption; it is not an authorization key.
EXPECTED_SHA256="$(awk '{print $1}' "dist/ocservia-agent-${VERSION}-linux-amd64.tar.gz.sha256")"
VERIFIED_PACKAGE="$(sudo scripts/verify-agent-package.sh \
  "dist/ocservia-agent-${VERSION}-linux-amd64.tar.gz" "${EXPECTED_SHA256}")"
sudo INSTALL_PRODUCTION_RELAYS=true \
  "${VERIFIED_PACKAGE}/scripts/install-agent.sh"
```

Build native packages from the archive and checksum created above using the same `VERSION`:

```bash
OUTPUT_DIR=dist VERSION="${VERSION}" PACKAGE_ARCH=amd64 \
  SOURCE_DATE_EPOCH=1786147200 scripts/package-native-agent.sh
```

Package file names are listed under Package names in [Contract owners](../reference/stable-contracts.md#contract-owners).

### Native installer packages

Each release publishes the node archive and its plain `.sha256`, plus native DEB and RPM installers for both `amd64` and `arm64`.

The `.deb` and `.rpm` packages embed only the archive, its plain checksum, and the verifier under `/usr/share/ocservia-agent`, and depend on `openssl`, `python3`, and `util-linux`. Their scriptlets contain no layout logic: `postinst` refuses a host-architecture mismatch, verifies the archive into trusted staging, and runs the verified `install-agent.sh` on a fresh host or `upgrade-agent.sh` on an existing installation.

Installing or upgrading a package never enables or starts the Agent or privd units outside the one-token bootstrap flow. Operators using manual archive provisioning must first provision `/etc/ocservia-agent/agent.env` and the keys, then enable both units manually. An upgrade or an identical-package retry runs `systemctl try-restart` on `ocservia-privd` and `ocservia-agent`, restarting units that are already running while leaving stopped units stopped.

A production managed node requests the relay contract before invoking the package manager by creating `/etc/ocservia/agent-install-production-relays` as a regular file. `postinst` then passes `INSTALL_PRODUCTION_RELAYS=true` into the verified lifecycle on both fresh installs and upgrades, sourcing the production relay drop-in and `relays.env` exclusively from the verified embedded payload (never from a source checkout). This marker file is consumed only after the verified lifecycle succeeds, keeping a failed install or upgrade retryable. A real package removal (as opposed to an upgrade) deletes any unconsumed request so it cannot affect a later plain install. Once installed, the drop-in records the standing production intent, and subsequent upgrades reinstall it whenever the drop-in is already present, with or without the marker. Without the marker or an existing drop-in, a native install stays relay-free.

### Manual trust provisioning

For the manual archive path, provision trust before service activation. The commands below install existing key material that was provisioned independently (do not use generated example keys).

Before enabling the units, install the independently provisioned Controller command verification key and two distinct RSA private keys for user-password and P12-password unsealing. Edit `/etc/ocservia-agent/agent.env` with the Controller key path, controller EndpointID, node UUID, distinct sealing key IDs, and the lowercase SHA-256 of each public key's DER encoding:

```bash
sudo install -o root -g ocserv-agent -m 0640 \
  controller-command-verification-key.pem \
  /etc/ocservia-agent/controller-command-verification-key.pem
sudo install -o root -g root -m 0600 user-password-seal-private.pem \
  /etc/ocservia-agent/user-password-seal-private.pem
sudo install -o root -g root -m 0600 p12-password-seal-private.pem \
  /etc/ocservia-agent/p12-password-seal-private.pem
```

[Provisioned trust](../reference/stable-contracts.md#provisioned-trust) defines startup verification and key separation. Finish [enrollment](#enroll-the-node) before enabling the services.

## Enroll the node

The default Nodes wizard and unattended `install.sh` workflow use the [one-token installation](#one-token-installation) flow.
Its `oit1_` token expires within 15 minutes, binds the chosen Workspace, environment
and optional node name/EndpointID, and creates and activates a single node alongside
its root `privd` attestation key. The browser retains the token only in the open
dialog's memory and clears it on expiry, close or Workspace change. A lost
issuance response is ambiguous; consult audit records or wait for expiry before
issuing another token.

### Use the Nodes page wizard

Choose Install node, provide name/environment/release, create the token and run
the displayed command. The installer asks only for the token (or reads `--join-token-file`)
and completes enrollment, activation, root attestation registration, and startup without
a second human decision.

### Enroll with the API

An authenticated Admin issues the installation token:

```http
POST /api/v1/node-bootstrap-tokens
X-Workspace-ID: <workspace-uuidv7>
Content-Type: application/json

{
  "workspace_id": "<workspace-uuidv7>",
  "environment": "production",
  "expected_node_name": "<node-name>",
  "installation": true,
  "reason": "Install managed node"
}
```

The response returns the `oit1_` root token once. `ttl_seconds` can shorten the
15-minute maximum, and `expected_endpoint_id` can pre-bind a known EndpointID.
Never place the token in a URL, log, or shared shell history; stage it in a
protected root-owned file (mode `0600`) when invoking `--join-token-file`.

The Agent's `--enrollment-token-file` CLI accepts only a derived `obt2_` child
enrollment credential produced from an `oit1_` installation token (legacy `obt1_`
tokens and `installation: false` requests are retired and rejected). Keep the
original identity directory and exact `obt2_` credential when reconciling an
uncertain response; the standard installer does not load operator-supplied
credentials from `install.env`.

### Finish enrollment

The standard token installer completes root-key registration and startup, then
checks Controller readiness. Manual Agent CLI enrollment records only the local
binding; provision the trust files and start the daemons deliberately using the
steps below. A lost enrollment response must be reconciled with the original
EndpointID and credential before issuing replacement authority.

### Recover an uncertain enrollment response

If an enrollment response is lost or your SSH session drops mid-run, keep the original EndpointID, identity directory, and protected token material intact. Never generate a new identity or issue a new token just because the client did not receive a response:

- For an installation token, rerun the same pinned installer with `--resume-install`. For manual Agent CLI enrollment, repeat the exact command with the original identity directory and protected credential. The Controller's same-endpoint recovery path can return the record it already committed. The Controller rejects a replay if the presenting EndpointID differs or refuses any bootstrap enrollment for an EndpointID already present from another attempt.
- Always check node status by `EndpointID` in the Controller inventory before issuing any replacement token.

Once local enrollment is confirmed, the installer deletes only its fixed internal copy of the token. If power loss, `SIGKILL`, or another interruption stops the process between configuration commit and cleanup, rerunning the installer validates the existing identity and binding (failing closed on unsafe file metadata), completes pending token cleanup and root-key registration, and polls Controller readiness. Whenever enrollment fails or remains uncertain, the installer retains recovery material along with the long-term identity, Relay token, public trust files, and sealing private keys.

Root `privd` result attestation keys are registered automatically by the one-token bootstrap flow through `POST /api/v1/node-bootstrap/attestation` and `POST /api/v1/node-bootstrap/register`. The packaged default key path is `PRIVD_ATTESTATION_KEY_FILE=/var/lib/ocservia-privd/attestation.key` in `/etc/ocservia-agent/privd.env`, and receipt contents are defined in [Privd receipt v1](../reference/stable-contracts.md#privd-receipt-v1).

## Start and verify the node

Manual Agent CLI enrollment leaves systemd units stopped. Verify the trust files before starting the daemons; the standard token installer completes these steps automatically:

```bash
sudo systemctl enable --now ocservia-privd.service ocservia-agent.service
systemctl status ocservia-privd.service ocservia-agent.service
```

Local `systemctl` status shows only that the units run on the host. Confirm the node on the Controller. In the selected Workspace, open the node, or query `GET /api/v1/nodes/{node_id}` from an authorized workspace Admin session, and inspect `trust_status`, `connection_state` and `freshness`. The node is operational when they are `active`, `online` and `fresh` with a recent heartbeat. Never copy that administrator session onto the managed node.

For token installations, rerun the same release with `--resume-install`. The
installer skips the matching native package, validates the existing identity,
sealing keys and trust files, reuses the exact enrollment and privd binding,
and verifies readiness with the Controller. Missing or substituted enrolled
material fails closed. After an upgrade or rollback, do not run an older
release installer; a package-version mismatch stops before enrollment.

### VPN audit collection

The Agent reports occtl session observations on its normal heartbeat. For login and logout events, the managed complete configuration sets the two ocserv hooks when applied with the packaged wrapper installed. A package upgrade alone leaves an existing ocserv configuration in place. For an externally maintained configuration, add these directives or call the wrapper from your existing scripts, preserving their admission logic. Apply the change through your usual ocserv configuration procedure.

```ini
connect-script = /usr/libexec/ocservia/vpn-session-hook
disconnect-script = /usr/libexec/ocservia/vpn-session-hook
```

The wrapper bounds the collector's execution time and returns success to ocserv if accounting fails. The Python 3 collector drops root privileges before writing to SQLite. To verify collection, connect and disconnect a test VPN user, then search **Audit → VPN audit history → Session events** by username and node ID. Check the source IP, assigned VPN IP and logout counters. **Session snapshots** shows occtl observations. The first snapshot that reports a session absent gives an upper bound on when it ended.

Network collection is disabled by default. Install the distribution package providing `/usr/sbin/conntrack` (`conntrack` on Debian or Ubuntu). To collect byte counts, enable the kernel's `net.netfilter.nf_conntrack_acct` setting before test connections open. Check the setting and start the optional service on the VPN node:

```bash
sysctl net.netfilter.nf_conntrack_acct
sudo systemctl enable --now ocservia-network-audit.service
systemctl status ocservia-network-audit.service
```

From a connected VPN client, open and close a connection to a known destination routed through the tunnel. Search **Network connections** for the destination IP after conntrack emits its destroy event. Kernel flow expiry can follow application closure. Byte counts cover the original and reply directions and can be unavailable. Flows already open when the collector starts, flows missing a lifecycle event, ambiguous VPN address mappings and flows beyond the 65,536-entry tracking limit can leave gaps. To turn off network collection, run `sudo systemctl disable --now ocservia-network-audit.service`.

The Agent installer creates the private directory `/var/lib/ocservia-vpn-audit`. When the ocserv hooks are configured, they can initialize `events.sqlite` and record events before the Agent first starts. During a Controller outage, the Agent retains pending events for retry. A full 64 MiB queue or a failed hook can leave gaps while VPN service continues. For missing events, check free space on that filesystem and inspect the node logs:

```bash
sudo journalctl -u ocservia-agent.service -u ocservia-network-audit.service --since "30 minutes ago"
sudo journalctl --grep='VPN audit collection failed' --since "30 minutes ago"
```

## Revoke a node

An authorized Admin revokes a node directly, including a pending node:

```http
POST /api/v1/nodes/<node-uuidv7>/revocation
Content-Type: application/json

{"reason": "Revoke managed node"}
```

A `503 Revocation committed` response means the database change committed but
transport synchronization remains pending. Preserve request ID, reason,
revision and error, and follow [revocation recovery](incident-recovery.md#revocation-committed-with-synchronization-pending).

## Configuration and TLS operations

The rules for plans, hashes, revisions and node TLS are in [Configuration and node TLS](../reference/stable-contracts.md#configuration-and-node-tls).

### Plan and apply a configuration

Submit complete-profile plans from the Web node detail page. Apply directly from the authorized Admin session after the plan validates cleanly and remains unexpired. The plan and apply rules are in [Planning](../reference/stable-contracts.md#planning) and [Apply and rollback](../reference/stable-contracts.md#apply-and-rollback).

If privd returns `failed_critical`, an operator must repair and verify the node locally before a recovery workflow can clear the automation lock.

If an apply ends in an uncertain state, follow the [matched-release recovery boundary](../reference/stable-contracts.md#matched-release-recovery-boundary) and [Unknown command reconciliation](incident-recovery.md#unknown-command-reconciliation).

### Provision node-local TLS

Nodes supporting the complete profile negotiate `ocserv.config.complete.plan` and `ocserv.config.complete.apply`. See [Node-local TLS provisioning](../reference/stable-contracts.md#node-local-tls-provisioning) for the profile rules.

Create the dedicated unprivileged `ocservia-vpn` system user, with `ocservia-vpn` as its only group, that the complete profile pins as Ocserv's worker account (see [Node-local TLS provisioning](../reference/stable-contracts.md#node-local-tls-provisioning)).

Server TLS certificates and private keys remain on the managed node under root-owned regular single-link files with non-writable directory ancestry and no symlinks. The private key requires mode `0600` and the public certificate requires `0600` or `0644`. The leaf certificate must specify `CA:FALSE`, carry valid server-authentication usage, and match the private key. When an optional CA bundle is supplied, it must be a valid `CA:TRUE` certificate. All certificate and SPKI digests recorded in metadata are computed over public DER bytes only.

Register the bundle reference in the Controller via `POST /api/v1/secret-provider-refs` with provider `node-local-tls-v1`, an immutable version string, and public `key_path` metadata in the format given in [Node-local TLS provisioning](../reference/stable-contracts.md#node-local-tls-provisioning). The UUID returned by that API call identifies the bundle. On the managed node, run the provisioning helper as root:

```sh
bash deploy/managed-node/provision-config-tls.sh \
  NODE_UUID REFERENCE_UUID VERSION CERTIFICATE PRIVATE_KEY [CA_CERTIFICATE]
```

The helper refuses to overwrite an existing version directory. Under a shared resource lock coordinated with privd plan and apply operations, it writes a protected manifest and mode `0400 root:root` files inside `/etc/ocservia-agent/config-tls/<reference UUID>/<version>`, publishes the version directory atomically, and prints only public reference metadata. Keep previous bundle versions on disk so configuration rollback can reference them, and never edit or delete version directories by hand.

Before starting a newly installed `ocserv` instance, write an initial `/etc/ocserv/ocserv.conf` (`0600 root:root`) that conforms to the finite profile. Point it to the bundle's `server-cert.pem`, `server-key.pem`, and optional `ca-cert.pem` paths, and configure the `ocservia-vpn` worker account, plain `/etc/ocserv/ocpasswd` authentication, and the target listener ports. Then verify it with `ocserv -t -c /etc/ocserv/ocserv.conf`. For a server that is already running, the restart requirements are in [Node-local TLS provisioning](../reference/stable-contracts.md#node-local-tls-provisioning). Keep the previous configuration and TLS bundle intact so both can be restored under the same maintenance window if the restart fails.

If an operator changes any startup-only setting during maintenance, `ocserv` must be restarted and verified locally before automated configuration plans resume.

## Upgrade and rollback

Upgrade an existing managed node by installing an explicitly selected Agent package, or through a Controller-driven upgrade. The v1.1.0 scripts remove source-version windows and target-version ordering checks, but cross-version operation is still not guaranteed to be safe. Previously installed older scripts retain their original checks, and the lifecycle does not convert historical deployments; see the [version policy](production-deployment.md#version-policy). The package scripts do not automatically enroll keys, reset node identity, or purge existing certificate artifacts, and the former upgrade-time enrollment environment variables and marker are no longer used. Removing historical layout checks does not make arbitrary cross-version operations safe or grant an installed binary capabilities it does not implement.

### Upgrade

Native package installation runs the same verified repository lifecycle as the archive installer. An upgrade installs the matching complete node package: the Agent, privd and upgrader binaries, the rollback and verification tools, the production Relay launcher, and the systemd units. It keeps the existing identity, configuration and durable state. It does not enroll or authorize the node, and it cannot recover state left behind by a failed upgrade (use [Rollback](#rollback) for recovery).

Before you begin:

- Confirm that the verified target package matches the host architecture.
- Obtain the architecture-specific native package over HTTPS from the selected GitHub Release in accordance with [Native installer packages](#native-installer-packages). During installation, the native scriptlets verify the plain checksum of their embedded archive in protected staging. A Controller-driven AgentUpgrade instead verifies `package_sha256` directly from its already-authorized command.
- Check that `/etc/ocservia-agent/agent.env` and its trust and sealing keys are valid. Command protocol `1.1` fails closed: provision the Controller command verification public key before upgrading the Agent and privd pair. Both services load the key independently, and privd also pins `NODE_ID`. Schedule the binary rollout and Controller signing-key enablement within a single maintenance window, because new binaries reject unsigned legacy mutations at both the command journal and root-effect boundaries. During a signing-key rotation, keep both the old and new public keys pinned until old authorizations expire and all Unknown outcomes are reconciled.
- Schedule a current recovery window so you can verify the node after restart. Upgrading restarts both a running Agent and `privd`, which briefly interrupts the node's Relay session and any in-flight operation.
- Make sure no earlier install attempt on the node was left unfinished. An interrupted install can leave an `installing-package` record under `/var/lib/ocservia-upgrade`. While it exists, the lifecycle refuses any different package until you run a verified [rollback](#rollback). Retrying the exact same verified package clears the record.

Set `RELEASE_DIR` to the directory holding the architecture-specific native package you downloaded and verified, and `AGENT_PACKAGE` to its file name. Pass that exact verified path to the package manager; do not point it at another copy over a relative path.

On Debian or Ubuntu:

```bash
sudo dpkg -i "$RELEASE_DIR/$AGENT_PACKAGE"
```

On an RPM-based system:

```bash
sudo rpm -Uvh "$RELEASE_DIR/$AGENT_PACKAGE"
```

Before changing backups, binaries, systemd units, or services, `upgrade-agent.sh` runs a strict preflight against the existing trust configuration. It verifies that `agent.env` contains a single absolute `CONTROLLER_COMMAND_VERIFICATION_KEY_FILE` and that the referenced Ed25519 public key satisfies the combined Agent and privd requirements for file type, ownership, mode, link count, symlinks, size, and directory ancestry. The shared key must be independently provisioned as `root:ocserv-agent` mode `0440` or `0640` beneath root-controlled ancestry; an Agent-owned legacy key is rejected and never promoted into a root trust anchor. The preflight also requires two distinct enrolled sealing key IDs and public-key fingerprints, derives each fingerprint from its root-owned private key, and rejects missing, reused, mismatched, symlinked, or unsafe key files. A legacy two-line `agent.env` or an invalid key therefore halts the upgrade with provisioning instructions before installing an Agent that would fail to restart. These checks enforce current trust requirements regardless of the source software version.

Once the preflight passes, the script saves one matched snapshot under the root-only `/var/lib/ocservia-upgrade/upgrade-backup` hierarchy covering the previous Agent and privd binaries, both base systemd units, the production relay drop-in and launcher, the durable upgrader binary and unit, the verifier, the VPN session wrapper, the audit collector and the network audit unit. Keeping this directory outside privd's systemd-managed `StateDirectory` prevents service startup from rewriting rollback evidence ownership. A root-owned manifest binds the 12 snapshot digests, and rollback rejects unsafe ancestry, symlinks, hard links, ownership, modes, or file replacement. Optional files carry explicit `.previous` or `.absent` records that tell rollback whether to restore or remove the installed file. Upgrading to a different package overwrites the previous snapshot, leaving only the most recent pre-upgrade state available for restoration. Reinstalling an identical verified package keeps the existing snapshot intact and restarts the running services.

Before modifying package-owned files, the upgrade script persists the complete snapshot, atomically publishes the verified standalone rollback driver, and records the verified target digest in `/var/lib/ocservia-upgrade/installing-package`. If an upgrade is interrupted, a subsequent direct or native-package retry refuses to overwrite the existing snapshot; run the verified rollback first to restore the host, then retry the package operation. A matching completed retry clears a stale `installing-package` record only after verifying the installation commit, every package-owned file, and the rollback snapshot, while rollback clears the record only after all restores and removals are synced across filesystems. Neither path alters Controller binding authority or reverses database migrations.

To verify an upgrade, check the local units:

```bash
systemctl status ocservia-privd.service ocservia-agent.service
```

Then check the Controller inventory to confirm that the node is online with a fresh target-version observation. Local `systemctl` status only proves the services started on the host; you still need the Controller inventory to verify node trust and fresh data.

If preflight validation detects an unsafe trust configuration, host architecture, or upgrade snapshot, the package lifecycle aborts before modifying the installation. Fix the reported prerequisite and retry the same package. If the new binaries have already been installed and must be reverted, run the [rollback](#rollback) command. Never use a package-manager downgrade as a rollback mechanism, because it skips the matched snapshot contract.

### Rollback

Rollback restores the complete matched snapshot captured during the most recent upgrade. Both upgrade and rollback preserve endpoint identity, the durable Agent database, the command journal, and configuration. Rollback modifies only the installed Agent-side files on the host, leaving the Controller, its database, and the node's identity and enrollment state untouched.

Before you begin:

- Place the node in a controlled recovery window.
- Confirm that the root-only upgrade snapshot at `/var/lib/ocservia-upgrade/upgrade-backup` is present and unmodified.
- Reconcile any Controller operation affected by the outage. Running rollback stops the Agent, `privd`, and active upgrader units, and marks every non-terminal (`accepted` or `running`) local upgrade operation `rolled_back` so it cannot be re-applied afterwards.
- Check the manifest format. Current snapshots contain 12 entries. Four are required (Agent and privd binaries and base units). Eight record prior presence or absence for the Relay drop-in and launcher, upgrader binary and unit, verifier, `vpn-session-hook`, `vpn-audit.py` and `ocservia-network-audit.service`. The current driver also accepts complete legacy 9-entry snapshots for restoration. Legacy 8-entry snapshots support verification only.
- To validate the snapshot before changing anything on disk, run `sudo /usr/libexec/ocservia/ocservia-agent-rollback --verify-only`.

To roll back the complete matched snapshot, run:

```bash
sudo /usr/libexec/ocservia/ocservia-agent-rollback
```

The rollback command validates the complete snapshot and target destinations before stopping services. It restores binaries and units together (including the production relay drop-in and launcher state), reloads systemd, and starts privd before the Agent unconditionally, even if those services were stopped before rollback began. When a current snapshot records that the network collector unit was absent, rollback disables and removes it. If the session wrapper was absent, rollback installs a success-only executable at its path so a running ocserv can still invoke its configured hook. The local audit queue survives rollback. The command clears any pending `installing-package` record. The standalone rollback driver itself remains on disk so it can finish executing and re-read the snapshot if invoked again. Restoring binaries without their matching units is unsupported because the CLI and local wire contract can change together; never restore one binary on its own.

The verifier and rollback script do not infer a software version from a missing Relay launcher or reject a snapshot on that basis alone. Any launcher referenced by a service unit must still exist on disk, and all snapshot integrity, trust, and configuration checks continue to apply. Operator configuration, node identity, and persistent state are preserved without silent conversion or reset. Removing the version check does not guarantee that the restored binary will work with the current configuration or on-disk data, and older scripts already installed on the host keep their original behavior.

Rollback coordinates directly with the [durable upgrade state](#durable-self-upgrade-runner): it stops any running `ocservia-upgrader@*.service` instance, marks non-terminal operations `rolled_back` so a stale runner cannot re-apply the rolled-back release, and restores or removes the upgrader binary, the `ocservia-upgrader@.service` unit, and the installed verifier according to the matched snapshot (`.previous` restored, `.absent` removed).

A rollback also restores the previous release's security properties, so treat it as a temporary recovery measure and move to a fixed release promptly. The privd attestation rollback requirements are in [Privd receipt v1](../reference/stable-contracts.md#privd-receipt-v1).

To verify the rollback, run `systemctl status ocservia-privd.service ocservia-agent.service`. Before sending new work to the node, confirm in the Controller that the node has reconnected under the expected previous version and is reporting fresh capabilities and trust state. Completing the local rollback does not by itself prove that the Controller accepts the node.

If rollback fails, stop immediately and preserve both the snapshot and host diagnostics instead of repeatedly rerunning the upgrade or rollback commands. Rollback validates the entire snapshot before changing any files, so an error reported during validation leaves the installed files untouched. A failure after services have been stopped can leave the node offline. Never replace the snapshot with files from an arbitrary download directory.

### Controller-driven single-node upgrade

The console exposes one reconciled upgrade endpoint per node: `POST /api/v1/nodes/{node_id}/agent-upgrade` (workspace Admin session), which accepts a target version and a reason alongside a required `Idempotency-Key` header. Callers never supply a URL, file path, or package digest; the Controller looks up the digest in the operator-provisioned release catalog pointed to by `OCSERV_AGENT_RELEASE_MANIFEST` (an absolute path with no default). When that environment variable is unset, the Controller starts without a catalog and returns `503` on the agent-upgrade and agent-rollout endpoints. Enabling Controller-driven AgentUpgrade and rollouts requires a deployment-specific override that mounts the catalog file read-only into the control-plane container, because the shipped production Compose neither sets nor mounts `OCSERV_AGENT_RELEASE_MANIFEST`. The catalog file has this form:

```json
{
  "releases": [
    {
      "version": "1.2.3",
      "architecture": "amd64",
      "package_sha256": "<64 lowercase hex characters>"
    }
  ]
}
```

This manifest is the only digest source for the workflow and must list between 1 and 512 unique `(version, architecture)` entries; once `OCSERV_AGENT_RELEASE_MANIFEST` is set, a missing, unreadable, malformed, or ambiguous file aborts Controller startup. The Controller does not synchronize with GitHub or container registries, so preparing an upgrade consists of placing the archive in each node's local spool (see [Durable self-upgrade runner](#durable-self-upgrade-runner)) and adding its exact digest to the manifest. Under the v1.1.0 policy reset, the Controller accepts any explicit trusted target regardless of whether it is lower, equal, or higher than the current version or whether the source version is unknown; version classifications shown in the UI are informational and do not govern execution. Each request must still supply the node's current revision (`If-Match`) and preserve the exact `node + version + digest + architecture` release identity.

To dispatch the upgrade, the Controller requires the node's typed-command capability (`ocserv.agent.upgrade.v2` or `ocserv.agent.upgrade.v1`, preferring v2 when both are active) and binds that exact capability name into the signed command. Nodes advertising only v1 remain eligible despite lacking the former software downgrade fence; the Controller never fabricates capabilities or converts historical payloads. The already-installed runner executes the command, so previously installed code retains its own runtime behavior. Advertising a command capability does not guarantee cross-version safety or certify historical version combinations.

New upgrade operations start in the `queued` state and move to the non-terminal `accepted` state when the Agent acknowledges scheduling (acknowledgement alone never counts as completion). As the upgrader restarts the Agent and privd, the node temporarily disconnects as part of normal progress. The Controller determines terminal outcomes strictly from durable evidence: `succeeded` requires the node to reconnect and report the target Agent version alongside a successful local terminal result from the upgrader; an explicit local failure closes the operation as `failed`; and a matched rollback closes it as `rolled_back`. If an operation remains unresolved when the reconciliation window expires (`OCSERV_AGENT_UPGRADE_RECONCILE_TIMEOUT`, defaulting to 30 minutes with an accepted startup range of 1 minute to 24 hours; not forwarded by the shipped production Compose unless overridden), the Controller closes it conservatively as `unknown` and marks its command expired to prevent blind retries. Only one upgrade may be active per node at a time, so concurrent requests fail with a conflict until the active operation reaches a terminal state. Every terminal transition appends an audit record capturing the operator, source and target versions, and outcome.

During regular telemetry heartbeats, the Agent queries privd read-only for terminal upgrader outcomes and forwards them to the Controller, which accepts the first report per operation and cross-checks it against the node's observed version before marking the upgrade `succeeded`. Nodes still running an older privd omit these local outcome reports and resolve instead through version observation, explicit failure or rollback paths, or the conservative `unknown` timeout.

### Durable self-upgrade runner

Controller-driven upgrades run outside the Agent and privd processes. When privd accepts a Controller-signed `AgentUpgrade` command, it independently re-validates the typed release identity (version, SHA-256 digest, and architecture), commits an immutable root-owned intent under `/var/lib/ocservia-upgrade/operations/<operation-id>/`, and starts the on-demand unit `ocservia-upgrader@<operation-id>.service`. The unit uses `Type=exec`, so privd's handoff returns as soon as the runner binary starts. As a result, the upgrade never terminates the process responsible for reporting the command result. The unit omits an `[Install]` section because only privd starts it, and the runner refuses to execute unless a committed intent exists in the active binding's operations namespace.

Each operation directory contains three fixed `root:root` mode `0600` records written atomically via write, fsync, and rename: `intent` (holding the schema version, operation and command IDs, target version, package digest, architecture, and semantic payload hash, all immutable once committed), `state` (one of `accepted`, `running`, `succeeded`, `failed`, or `rolled_back`), and `result` (terminal evidence written upon completion). Replaying the same operation ID with an identical signed identity converges on the committed intent. Reusing an operation ID with a different identity is rejected. Only one active operation may exist per node. If privd loses its effect journal between committing the intent and recording journal completion, the durable intent store serves as the idempotency authority for this command family.

The runner loads packages exclusively from the fixed local spool directory `/var/lib/ocservia-upgrade/package-spool`, without performing URL fetches or accepting caller-supplied paths. Before issuing the upgrade command, the operator or provisioning pipeline must place `ocservia-agent-<version>-linux-<arch>.tar.gz` in that spool as a non-empty regular file (no symlinks or hard links, within the runner's size limit) owned by root and not writable by group or world; otherwise the upgrade fails with unsafe spool metadata. The runner verifies that the spool archive digest equals `package_sha256` in the durable intent and passes that exact digest to `/usr/libexec/ocservia/ocservia-agent-verify`, which copies the archive into root-owned staging, re-checks its SHA256, and extracts it safely. Any digest mismatch fails closed before the lifecycle runs. Once the verified marker matches the intent, the runner executes the package's `upgrade-agent.sh` script, re-verifies the installed binaries against the verified package, restarts `ocservia-privd` and `ocservia-agent`, and writes the terminal result. If the runner crashes or restarts mid-flight, a `running` operation whose installation commit record and installed binaries already match the package skips repeating the destructive lifecycle, and any validation failure records `failed` evidence before exiting non-zero.

All native DEB and RPM packages (`amd64` and `arm64`) include the durable runner: `install-agent.sh` installs the runner binary, the `ocservia-upgrader@.service` unit and the verifier, every package carries them, and `uninstall-agent.sh` removes them.

### Fleet rolling upgrades

`POST /api/v1/agent-rollouts` (workspace Admin session) upgrades a selected set of nodes to a single trusted target version in bounded batches. The creation request takes the target version, node IDs, an optional `batch_size` (defaulting to 5, bounded from 1 to 20), and a reason. Callers cannot set `stop_on_failure` in the request; the server fixes `StopOnFailure = true`, and the immutable rollout request hash covers the target version, sorted node set, batch size, and `stop_on_failure = true`. Every selected node's architecture must have a matching target entry in the operator-provisioned release catalog, and every selected node must advertise `ocserv.agent.upgrade.v1` or `ocserv.agent.upgrade.v2`.

Fleet version badges in the console and the recommended version displayed in Settings come from the operator-pinned `OCSERV_RECOMMENDED_AGENT_VERSION` (SemVer), which classifies observed versions without scheduling any upgrades on its own. Configure it in the Controller section of `install.env` or export it in the invoking shell before running the production installer or bootstrap, so the root lifecycle forwards the resolved value to the migrate and control-plane containers. An explicitly empty export overrides `install.env` and clears the recommendation.

When updating this setting on an existing installation, recreate the control-plane through the documented Controller lifecycle (see [Upgrade, rollback and uninstall](production-deployment.md#upgrade-rollback-and-uninstall)). Use `controller.sh start` to re-apply the current release's Compose project with the exported environment (`controller.sh upgrade`, by contrast, switches to a different release file), and export the variable in that invocation as well because `controller.sh` does not read `install.env`. Check Settings afterward to confirm the update took effect, since a Compose recreate run without the export silently retains or clears the prior value. When no recommendation is configured, observed versions remain visible without target comparison; invalid nonempty SemVer strings fail Controller configuration validation.

Batch 0 always runs as a mandatory single-node canary, and subsequent batches start only after that canary reaches `succeeded`. If the canary fails or is skipped, the rollout pauses; resuming it requeues the canary for a fresh attempt, and no operator action can bypass a successful canary.

Within each batch, every node follows the reconciled single-node upgrade lifecycle under its own stable operation ID, and nodes that reach `succeeded` are never redispatched. A batch advances only after all of its nodes reach a terminal outcome, pausing the rollout if any node finishes as `failed`, `unknown`, or `rolled_back`. If a node is ineligible when its batch dispatches (because it is offline, has a stale observation, withdrew its capability, has another upgrade active, or lacks a manifest entry), the Controller marks it `skipped` with a reason code and pauses the rollout. Resuming a paused rollout requeues the current batch's `failed`, `unknown`, `rolled_back`, and skipped-canary nodes for a fresh eligibility check, while skipped non-canary nodes remain `skipped` with their recorded reason. The rollout detail view tracks succeeded, failed, and skipped nodes per batch alongside the remaining count, so an overall rollout status of `succeeded` does not imply that every originally selected node was upgraded.

Rollout state persists across Controller restarts and console sessions. Viewing, listing, creating, or resuming a rollout requires an authenticated Admin session for that rollout's workspace.

## Remove a node package

Package removal (`dpkg -r`, `rpm -e`) runs the verified `uninstall-agent.sh`, which disables and stops the Agent, privd and optional network collector units. It removes the collector, units and production Relay launcher. The session wrapper stays in place for ocserv configurations that reference it. Identity, configuration and the pending audit queue survive ordinary removal. The Controller then shows the node offline. Neither `dpkg --purge` nor the package manager triggers the state purge; only `uninstall-agent.sh --purge-state` does so.

Passing `--purge-state` is irreversible and should only be done after revoking the node identity on the Controller (see [Revoke a node](#revoke-a-node)) and archiving required audit material. It deletes the Agent and privd state directories, the VPN audit spool (including events awaiting upload), `/var/lib/ocservia-upgrade` (including the rollback snapshot, preventing any subsequent rollback), and `/etc/ocservia-agent` (`agent.env`, keys, and relay token), and then removes the `ocserv-agent` user and group. Purging local state does not update the Controller, so revoke the node record there separately.

## Diagnostics

### Journal storage monitoring

Monitor journal storage independently of Agent connectivity. The command journal compacts terminal command entries after retention bounds, and normal authorized commands accumulate durable state over time. From a reviewed checkout, run this read-only Linux probe as `ocserv-agent` (or another account permitted to stat the state directory):

```bash
sudo -u ocserv-agent bash scripts/check-agent-storage.sh /var/lib/ocservia-agent/agent.db
```

Pass the actual database path when `--journal` overrides the default. The probe reports the logical sizes of the database, WAL, and SHM files, their combined allocated bytes, and available filesystem bytes and inodes without opening SQLite or checkpointing, truncating, or deleting any state. If the state files are missing or unreadable, the probe reports UNKNOWN (exit code 3).

Schedule this probe in the node's monitoring agent every 15 minutes with a 30-second execution timeout. Exit codes are 0 OK, 1 WARNING, 2 CRITICAL, and 3 UNKNOWN; configure alerts on exit codes 1, 2, or 3, on timeouts, or after two missed samples. The default warning and critical thresholds are 80% and 90% for both disk and inode usage, and can be tuned with `STORAGE_WARNING_PERCENT` and `STORAGE_CRITICAL_PERCENT`. These defaults are starting operational thresholds and do not guarantee capacity. The repository does not install a scheduler or notification destination automatically.

Retain the collected metrics in your monitoring backend to track `journal_bytes` and `journal_allocated_bytes` growth and forecast exhaustion from declining `filesystem_available_bytes`. Alert whenever projected runway drops below seven days, even if percentage thresholds have not yet fired. Record a baseline after deployment and verify that a synthetic warning reaches the on-call destination:

```bash
sudo -u ocserv-agent env STORAGE_WARNING_PERCENT=1 STORAGE_CRITICAL_PERCENT=99 \
  bash scripts/check-agent-storage.sh /var/lib/ocservia-agent/agent.db
```

When a warning fires, inspect journal growth alongside unrelated files on the same filesystem and plan capacity expansion. On a critical alert, stop initiating new command dispatches or rollouts to the affected node and expand storage before resuming operations. Always keep the database and WAL files together during controlled recovery; never delete command identities, revision fences, pending or unknown records, or WAL files to reclaim space, and do not introduce ad-hoc age-based cleanup until idempotency, signed replay, and recovery boundaries are explicitly preserved.

### UTC and network-monitor diagnostics

Use UTC when correlating Agent, privd, Controller and browser observations:

```sh
journalctl --utc -u ocservia-agent -u ocservia-privd --since '2026-09-30 00:00:00 UTC'
systemctl cat ocservia-agent.service
systemctl show ocservia-agent.service -p FragmentPath -p DropInPaths -p RestrictAddressFamilies
```

Startup `service.version`, the Agent version CLI and heartbeat use the same release version. Browser diagnostic timestamps show UTC and retain fractional precision; extended-year and infinite values remain textual.

The default Agent unit permits AF_NETLINK for the transport network monitor. When investigating a netlink initialization warning, inspect the effective unit and drop-ins on the affected host, then verify address/route-change observation in an isolated systemd node. A corrected repository default does not by itself explain a historical warning on an existing host. Keep the remaining sandbox settings intact, and note that Controller connectivity or heartbeat delivery alone does not confirm that the local network monitor is healthy.

## Troubleshooting

Retain the exact release version, host architecture, command output, and redacted logs with your incident record.

### Agent cannot enroll

Confirm that the expected EndpointID matches the one printed from the node's persistent identity directory and that the Controller EndpointID and Relay URL pins have not changed. The expected EndpointID, Controller EndpointID, and identity directory must stay aligned. Changing the Controller pin or replacing the identity directory represents a new trust decision.

Bootstrap and installation tokens are single-use and short-lived. Issue a new token only after confirming that the previous token was rejected or expired. After a failed or unknown enrollment response, always query the Controller by that EndpointID before issuing a new token, and follow [Recover an uncertain enrollment response](#recover-an-uncertain-enrollment-response) first.

If the Controller enrollment routes return `404`, the Controller has no `OCSERV_CONTROLLER_ENDPOINT_ID` configured. See [Prepare the configuration](production-deployment.md#prepare-the-configuration).

Configuration-only enrollment without an `oit1_` installation token leaves services stopped until local trust files and `privd` attestation are verified before enabling the daemons.

### Agent will not start after an upgrade

Inspect `journalctl -u ocservia-privd -u ocservia-agent` to see which check failed, then verify `/etc/ocservia-agent/agent.env`, the independently provisioned command verification key, both distinct sealing keys, `relays.env` (which requires a literal HTTPS `RELAY_URL_A`), and the relay access token and CA files it references. When an upgrade fails during preflight before modifying files on disk, fix the reported prerequisite and retry. If the new binaries were already installed and must be reverted, run [Rollback](#rollback) instead of copying binaries from an unverified directory.
