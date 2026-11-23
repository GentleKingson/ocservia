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

Set `RELAY_URL_A` to the HTTPS URL of the dedicated production Relay. Install the
token at `/etc/ocservia-agent/relay-access-token` as `root:ocserv-agent` mode
`0640`, or pass its path in `RELAY_ACCESS_TOKEN_SOURCE`.

The production drop-in runs the packaged
`/usr/libexec/ocservia/ocservia-agent-relays` launcher. Native package upgrades
preserve operator configuration and identity. Package removal retains
configuration and state for recovery.

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

The installer downloads the pinned native package over HTTPS. To verify it against an independently obtained digest, pass `--package-sha256 HEX`. It stages the file under root ownership and retains the package in `/var/lib/ocservia-upgrade/package-spool/` for rollback. A one-token installation then provisions identity, exchanges the installation token, registers privd attestation and activates the services.

### Build a package locally

Build the release binaries on their target architecture, then package those binaries using the pinned nFPM tool:

```bash
source scripts/env.sh
export VERSION=2.0.0 PACKAGE_ARCH=amd64 SOURCE_DATE_EPOCH=1786147200
export OUTPUT_DIR="$PWD/dist"
(cd rust && OCSERV_AGENT_RELEASE_VERSION="$VERSION" cargo build --locked --release \
  -p ocservia-agent -p ocservia-privd -p ocservia-upgrader)
bash scripts/package-native-agent.sh
```

### Native installer packages

Each release publishes DEB and RPM packages for amd64 and arm64. The package manager owns the binaries, hook files and systemd units. Inspect an installed file with `dpkg -S /usr/libexec/ocservia/ocservia-agent` or `rpm -qf /usr/libexec/ocservia/ocservia-agent`. On RPM systems, `rpm -V ocservia-agent` verifies the installed payload against the package database. Complete enrollment and provisioning before enabling the services.

### Manual trust provisioning

For a manual native installation, provision trust before service activation. The commands below install existing key material that was provisioned independently (do not use generated example keys).

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

### Upgrade

Use the pinned native package for the node's architecture and distribution. Keep the previous package in the root-owned package spool, together with backups of the node's configuration, keys and journals. On Debian systems install the candidate with `dpkg -i /path/to/package.deb`. On RPM systems use `rpm -U /path/to/package.rpm`. Verify the installed version and the health of both node services.

### Rollback

To recover a manual upgrade, reinstall the previous DEB with `dpkg -i`, or the previous RPM with `rpm -U --oldpackage`. Check the Agent and privd units and confirm the observed Agent version on the Controller. The package manager restores package-owned files. Restoring operator configuration or application data requires a compatible backup.

When moving a node from a release with the archive-based runner, install the first native-managed release with the system package manager. Keep that DEB or RPM under its release filename in the root-owned package spool (mode 0600) before issuing the next Controller-driven upgrade.

### Controller-driven single-node upgrade

The console exposes one reconciled upgrade endpoint per node: `POST /api/v1/nodes/{node_id}/agent-upgrade` (workspace Admin session), which accepts a target version and a reason alongside a required `Idempotency-Key` header. Callers never supply a URL, file path, or package digest; the Controller looks up the digest in the operator-provisioned release catalog pointed to by `OCSERV_AGENT_RELEASE_MANIFEST` (an absolute path with no default). When that environment variable is unset, the Controller starts without a catalog and returns `503` on the agent-upgrade and agent-rollout endpoints. Enabling Controller-driven AgentUpgrade and rollouts requires a deployment-specific override that mounts the catalog file read-only into the control-plane container, because the shipped production Compose neither sets nor mounts `OCSERV_AGENT_RELEASE_MANIFEST`. The catalog file has this form:

```json
{
  "releases": [
    {
      "version": "1.2.3",
      "architecture": "amd64",
      "package_format": "deb",
      "package_sha256": "<64 lowercase hex characters>"
    }
  ]
}
```

This manifest is the only digest source for the workflow and must list between 1 and 512 unique `(version, architecture, package_format)` entries; once `OCSERV_AGENT_RELEASE_MANIFEST` is set, a missing, unreadable, malformed, or ambiguous file aborts Controller startup. The Controller does not synchronize with GitHub or container registries, so the operator records the exact digest of each target DEB or RPM from the pinned release. Under the v1.1.0 policy reset, the Controller accepts any explicit trusted target regardless of whether it is lower, equal, or higher than the current version or whether the source version is unknown; version classifications shown in the UI are informational and do not govern execution. Each request must still supply the node's current revision (`If-Match`) and preserve the exact `node + version + digest + architecture` release identity.

To dispatch the upgrade, the Controller requires the node's typed-command capability (`ocserv.agent.upgrade.v2` or `ocserv.agent.upgrade.v1`, preferring v2 when both are active) and binds that exact capability name into the signed command. Nodes advertising only v1 remain eligible despite lacking the former software downgrade fence; the Controller never fabricates capabilities or converts historical payloads. The already-installed runner executes the command, so previously installed code retains its own runtime behavior. Advertising a command capability does not guarantee cross-version safety or certify historical version combinations.

New upgrade operations start in the `queued` state and move to the non-terminal `accepted` state when the Agent acknowledges scheduling (acknowledgement alone never counts as completion). As the upgrader restarts the Agent and privd, the node temporarily disconnects as part of normal progress. The Controller determines terminal outcomes strictly from durable evidence: `succeeded` requires the node to reconnect and report the target Agent version alongside a successful local terminal result from the upgrader; an explicit local failure closes the operation as `failed`; and a matched rollback closes it as `rolled_back`. If an operation remains unresolved when the reconciliation window expires (`OCSERV_AGENT_UPGRADE_RECONCILE_TIMEOUT`, defaulting to 30 minutes with an accepted startup range of 1 minute to 24 hours; not forwarded by the shipped production Compose unless overridden), the Controller closes it conservatively as `unknown` and marks its command expired to prevent blind retries. Only one upgrade may be active per node at a time, so concurrent requests fail with a conflict until the active operation reaches a terminal state. Every terminal transition appends an audit record capturing the operator, source and target versions, and outcome.

During regular telemetry heartbeats, the Agent queries privd read-only for terminal upgrader outcomes and forwards them to the Controller, which accepts the first report per operation and cross-checks it against the node's observed version before marking the upgrade `succeeded`. Nodes still running an older privd omit these local outcome reports and resolve instead through version observation, explicit failure or rollback paths, or the conservative `unknown` timeout.

### Durable self-upgrade runner

Controller-driven upgrades run outside the Agent and privd processes. When privd accepts a Controller-signed `AgentUpgrade` command, it independently re-validates the typed release identity (version, SHA-256 digest, and architecture), commits an immutable root-owned intent under `/var/lib/ocservia-upgrade/operations/<operation-id>/`, and starts the on-demand unit `ocservia-upgrader@<operation-id>.service`. The unit uses `Type=exec`, so privd's handoff returns as soon as the runner binary starts. As a result, the upgrade never terminates the process responsible for reporting the command result. The unit omits an `[Install]` section because only privd starts it, and the runner refuses to execute unless a committed intent exists in the active binding's operations namespace.

Each operation directory contains three fixed `root:root` mode `0600` records written atomically via write, fsync, and rename: `intent` (holding the schema version, operation and command IDs, target version, package digest, architecture, and semantic payload hash, all immutable once committed), `state` (one of `accepted`, `running`, `succeeded`, `failed`, or `rolled_back`), and `result` (terminal evidence written upon completion). Replaying the same operation ID with an identical signed identity converges on the committed intent. Reusing an operation ID with a different identity is rejected. Only one active operation may exist per node. If privd loses its effect journal between committing the intent and recording journal completion, the durable intent store serves as the idempotency authority for this command family.

On a cache miss, the runner downloads from the exact tag in the project’s GitHub Releases. It uses the installed native package format and the architecture and version from the authorized operation to select the release asset. It verifies the target against the signed SHA-256 digest before invoking the package manager. Keep the exact currently installed package in the root-owned spool under its release filename. This gives the runner a local downgrade path when a service health check fails. Inspect the terminal result and observed version after recovery.

### Fleet rolling upgrades

`POST /api/v1/agent-rollouts` (workspace Admin session) upgrades a selected set of nodes to a single trusted target version in bounded batches. The creation request takes the target version, node IDs, an optional `batch_size` (defaulting to 5, bounded from 1 to 20), and a reason. Callers cannot set `stop_on_failure` in the request; the server fixes `StopOnFailure = true`, and the immutable rollout request hash covers the target version, sorted node set, batch size, and `stop_on_failure = true`. Every selected node's architecture must have a matching target entry in the operator-provisioned release catalog, and every selected node must advertise `ocserv.agent.upgrade.v1` or `ocserv.agent.upgrade.v2`.

Fleet version badges in the console and the recommended version displayed in Settings come from the operator-pinned `OCSERV_RECOMMENDED_AGENT_VERSION` (SemVer), which classifies observed versions without scheduling any upgrades on its own. Configure it in the Controller section of `install.env` or export it in the invoking shell before running the production installer or bootstrap, so the root lifecycle forwards the resolved value to the migrate and control-plane containers. An explicitly empty export overrides `install.env` and clears the recommendation.

When updating this setting on an existing installation, recreate the control-plane through the documented Controller lifecycle (see [Upgrade, rollback and uninstall](production-deployment.md#upgrade-rollback-and-uninstall)). Use `controller.sh start` to re-apply the current release's Compose project with the exported environment (`controller.sh upgrade`, by contrast, switches to a different release file), and export the variable in that invocation as well because `controller.sh` does not read `install.env`. Check Settings afterward to confirm the update took effect, since a Compose recreate run without the export silently retains or clears the prior value. When no recommendation is configured, observed versions remain visible without target comparison; invalid nonempty SemVer strings fail Controller configuration validation.

Batch 0 always runs as a mandatory single-node canary, and subsequent batches start only after that canary reaches `succeeded`. If the canary fails or is skipped, the rollout pauses; resuming it requeues the canary for a fresh attempt, and no operator action can bypass a successful canary.

Within each batch, every node follows the reconciled single-node upgrade lifecycle under its own stable operation ID, and nodes that reach `succeeded` are never redispatched. A batch advances only after all of its nodes reach a terminal outcome, pausing the rollout if any node finishes as `failed`, `unknown`, or `rolled_back`. If a node is ineligible when its batch dispatches (because it is offline, has a stale observation, withdrew its capability, has another upgrade active, or lacks a manifest entry), the Controller marks it `skipped` with a reason code and pauses the rollout. Resuming a paused rollout requeues the current batch's `failed`, `unknown`, `rolled_back`, and skipped-canary nodes for a fresh eligibility check, while skipped non-canary nodes remain `skipped` with their recorded reason. The rollout detail view tracks succeeded, failed, and skipped nodes per batch alongside the remaining count, so an overall rollout status of `succeeded` does not imply that every originally selected node was upgraded.

Rollout state persists across Controller restarts and console sessions. Viewing, listing, creating, or resuming a rollout requires an authenticated Admin session for that rollout's workspace.

## Remove a node package

Run `dpkg -r ocservia-agent` or `rpm -e ocservia-agent` to stop the services and remove the package-owned files. Node identity, journals and operational state are separate from the installed payload. Revoke the node on the Controller before permanently retiring it, and archive any audit records required for retention.

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
