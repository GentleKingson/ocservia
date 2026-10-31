# Agent lifecycle

This is the operations manual for a managed node: install, enroll, independent approval, start, upgrade, rollback, rebind, retention and removal.

A managed node is an existing ocserv server with the ocservia node daemons installed alongside it. The local ocserv instance still terminates VPN tunnels and forwards traffic. The ocservia services handle enrollment, health telemetry, and signed administrative commands from the Controller.

The v1.1.0 policy removed the software-version admission checks. That does not guarantee cross-version compatibility, and historical deployments and keys are not converted automatically. Operators must provision the target explicitly or redeploy without overwriting retained identity and persistent state. Never reset an existing node's identity to force an installation through. See the [version policy](production-deployment.md#version-policy).

## Node states

Package installation, local enrollment, approval and active fleet management are separate stages. The installer reports only host-side progress.

| State | Meaning |
| --- | --- |
| `ENROLLMENT_READY` | The package is installed and the identity is prepared, but no enrollment token was supplied. Agent and privd are stopped. |
| `ENROLLED_LOCAL` | The local Node ID is persisted. The Controller registration is `PENDING_APPROVAL` (`pending` in the inventory). The node is not approved, running or online. |
| Approved | A second authorized principal approved the node and the activation call returned `status: active`. The local services are still stopped until the operator enables them. |
| `SERVICES_ACTIVE` | Both local systemd units are enabled and active. This describes local state only. |
| Operational | The Controller shows `trust_status=active`, `connection_state=online` and `freshness=fresh` with a recent heartbeat. |

The installer has no administrator API session, so it reports `CONTROLLER_TRUST_STATUS`, `CONTROLLER_CONNECTION_STATE` and `CONTROLLER_FRESHNESS` as `NOT_OBSERVED`. Local installation success never means the Controller has observed the node. A node is under active management only in the operational state.

## Install a managed node

This section covers the standard package-first installation. Use published node artifacts from the same release as your Controller. The commands use `vX.Y.Z` as the release tag. See the [version policy](production-deployment.md#version-policy) for the tags they accept and the [release policy](../development/release-checks.md#release-policy) for RC naming and native package validation.

### Requirements

- A deployed Controller reachable through the production Relay (see [Dedicated Relay](production-deployment.md#dedicated-relay)).
- The Controller's `EndpointID` and the target workspace UUIDv7.
- A Linux host running systemd with root access and `ocserv` installed (or ready to be managed), on a platform listed in [Supported platforms](production-deployment.md#supported-platforms). Managed nodes use native `.deb` or `.rpm` packages.
- Protected local files containing the Relay access token and the Controller command verification key.
- An optional bootstrap token file if you want the installer to complete local enrollment in the same run.
- A non-root launcher account with `sudo` privileges. The installer elevates individual commands with `sudo`; pass `--root-lifecycle` only when you intend to run the whole script as root.
- `curl`, `openssl`, `sha256sum`, `awk`, `git`, and outbound HTTPS access to GitHub Releases.
- An authenticated requester API client with permission to create a node bootstrap token and request node approval (SecurityAdmin or PlatformAdmin), and a second authorized principal who can review and approve the request independently.
- Sealing keys for your chosen workflow: the standard installer generates any missing sealing keys automatically. The advanced manual path requires two distinct sealing private keys already provisioned on the node along with their key IDs and public-key SHA-256 descriptors.

### Prepare the node configuration

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

### Production Relay launcher

To configure production relays during installation, set `INSTALL_PRODUCTION_RELAYS=true` and populate `/etc/ocservia-agent/relays.env` with the required HTTPS `RELAY_URL_A`. Omit `RELAY_URL_B` or leave it empty; the managed-node installer rejects a nonempty B before installation, and the production launcher rejects it before Agent execution. Install the relay token at `/etc/ocservia-agent/relay-access-token` as `root:ocserv-agent` mode `0640`, or set `RELAY_ACCESS_TOKEN_SOURCE` so the installer can install it.

The production drop-in executes the packaged fixed launcher `/usr/libexec/ocservia/ocservia-agent-relays`, which constructs a single Relay URL argument and replaces itself with the Agent, leaving base development units and direct binary invocations unchanged. Archive and native installations, upgrades, rollback snapshots, and uninstall all include this launcher. The lower-level package installer and upgrades preserve operator configuration and identity. Uninstall removes the launcher but retains configuration and state unless the existing purge option is used.

### Run the installer

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

The installer enables and starts the history-retention timer (`ocservia-agent-retention.timer`). It leaves the node unapproved and the `Agent` and `privd` services stopped. If a different version of `ocservia-agent` is already installed on the host, the installer exits without upgrading or downgrading the package. Never rerun an unpinned or `latest` installer script as a shortcut for upgrading.

### Package verification and staging

Once its external endpoint is deployed and verified, the operator-hosted thin first-install chain separates each stage (see [Stage-0 bootstrap hosting](production-deployment.md#stage-0-bootstrap-hosting)):

```text
Stage-0 -> exact vX.Y.Z Stage-1 -> HTTPS native package
        -> identity/sealing -> Bootstrap Enrollment -> ENROLLED_LOCAL
        -> independent approval -> service activation
```

Stage-0 and the initial Stage-1 and native package downloads rely on HTTPS. Before invoking the package manager, the installer copies the downloaded native package into root-owned staging and verifies that its bytes remain unchanged. That transport check applies only to initial installation; an AgentUpgrade instead verifies against the SHA256 carried in the already-authorized Controller command. Likewise, a Bootstrap Token can create or recover only the same pending node and cannot approve it, keeping package installation, enrollment, approval, and service activation across separate authority boundaries. Until external hosting has operational ownership and byte-verification evidence, the first-install procedure above obtains the installer from a clean exact-release checkout. Once installed, the native package has no runtime dependency on Git. Stage-0 plays no role in ongoing lifecycle operations.

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

Installing or upgrading a package never enables or starts the Agent or privd units. Operators must first provision `/etc/ocservia-agent/agent.env` and the keys, then enable both units manually. The independent history-retention timer is the only exception: installation enables it and starts it unless an install is still unfinished. An upgrade or an identical-package retry runs `systemctl try-restart` on `ocservia-privd` and `ocservia-agent`, restarting units that are already running while leaving stopped units stopped.

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

[Provisioned trust](../reference/stable-contracts.md#provisioned-trust) defines startup verification and key separation. Finish [enrollment](#enroll-the-node) and [independent approval](#approve-the-node) before enabling the services.

## Enroll the node

Enrolling a managed node binds its persistent EndpointID to a node record on the Controller in a pending state. Enrollment does not activate the node. An operator must complete the separate approval workflow afterward. The rules for the EndpointID, tokens and session grants are in [Enrollment and transport](../reference/stable-contracts.md#enrollment-and-transport).

A Bootstrap Token authorizes a single narrowly scoped enrollment transaction that creates a pending node bound to the presenting EndpointID. Possessing the token does not grant `node.approve`, select approved capabilities, activate the node, or start local services; approval requires a separate content-bound decision by a second authorized principal.

### Use the Nodes page wizard

Open Nodes, then Install node, including when the inventory is empty. This entry prepares the existing installer flow; it does not deploy over SSH or change enrollment or approval permissions.

1. Confirm the current authorized Workspace, then enter:

   - A node name unique within that Workspace (including existing pending nodes).
   - The environment.
   - A published matching Controller/Agent Release tag (`vX.Y.Z`, or `vX.Y.Z-rc.N`).
   - The Controller EndpointID.
   - One dedicated HTTPS Relay URL.
   - Three distinct protected file paths on the Agent host.

   The Controller EndpointID is not the new Agent's EndpointID. The UI accepts conservative literal names and paths and rejects newline injection, credential-bearing URLs, query strings and floating versions. It validates the tag format; the administrator must confirm that the matching release exists. A duplicate name can fail during enrollment even if token issuance succeeds. The name is bound to the token; editing `install.env` cannot rename that binding.
2. Copy the generated `install.env` into a private working directory on the Agent host with mode `0600`. It contains public configuration and file paths only. Node name, Workspace and release are comments. The name and Workspace bind the Bootstrap Token request, and the release is passed explicitly to `--version`. Do not source the file; the installer's strict loader reads it. Existing shell variables take precedence, so check for stale values before running the installer. The pinned installer commands and host requirements are visible before token issuance. Provision the Relay access token and trusted Controller command verification public key first. For a slow first installation on a fresh, unenrolled host with no existing enrollment-token file:

   - Temporarily set `BOOTSTRAP_TOKEN_SOURCE=` in `install.env` and remove any shell override for that variable. Run the displayed pinned installer to download/install the package, check the host and prepare keys.
   - Wait for `ENROLLMENT_READY`; Agent/privd are still not started. Restore the configured Bootstrap token file path, then issue and save the token as below and rerun the same installer version with the same identity.
   - Never remove an existing identity or token file to force this path. If a previous enrollment outcome is unknown, use the [recovery flow](#recover-an-uncertain-enrollment-response).

3. Only when the host is ready to enroll, click Create bootstrap token. Its 900-second lifetime starts at issuance, not at first use. This sends one POST with a 900-second TTL and the selected Workspace in both `X-Workspace-ID` and the JSON body. Authorization remains enforced by the Controller. Transfer the displayed token through a protected channel to the configured Bootstrap token file, readable only by its owner (`0600`) and accessible to the installer launcher. Provision the Relay access token and trusted Controller Ed25519 command verification public key PEM at the other paths. All three must be non-empty regular files, not symlinks. Never transfer the Controller private key or an administrator session.
4. After saving the token, run or rerun the fixed-release installer commands. Download the official `deploy/managed-node/install.sh` from the selected tag and run `bash ./install.sh --version vX.Y.Z` in the directory holding `install.env`, as a non-root launcher with sudo privileges. The commands contain no token values. Host requirements and package verification remain those in [Install a managed node](#install-a-managed-node).
5. Record the printed node ID. `ENROLLED_LOCAL` does not mean approved, running or online. Continue with [independent approval](#approve-the-node), then [start and verify the node](#start-and-verify-the-node).

The Web keeps the token only in the mounted dialog's memory. It clears the value on expiry, continuing to instructions, closing, changing Workspace or leaving the page, and ignores late responses from abandoned contexts. There is no token download, persistence or automatic retry. The browser clipboard is outside this lifecycle: if you manually copy the token, clear your clipboard after transferring it securely.

A `403` means the caller lacks authorization; `400` means the request was rejected. A network failure, server failure or unreadable response may have issued a token even when none is displayed. Check Controller audit records or wait for expiry before explicitly starting another issuance. Reopening does not recover an issued token. For an uncertain installation response, follow [enrollment recovery](#recover-an-uncertain-enrollment-response) with the original identity and token binding; never delete identity material to register again.

### Enroll with the API

1. With an authenticated API client, create a short-lived bootstrap token.

   ```http
   POST /api/v1/node-bootstrap-tokens
   X-Workspace-ID: <workspace-uuidv7>
   Content-Type: application/json

   {
     "workspace_id": "<workspace-uuidv7>",
     "environment": "production",
     "expected_node_name": "<node-name>",
     "reason": "Bootstrap managed node"
   }
   ```

   Specify a `workspace_id` that the caller is authorized to access; the `X-Workspace-ID` header may be omitted only when the caller belongs to exactly one workspace. The API returns the plaintext `obt1_` token once, and it expires within 15 minutes (`ttl_seconds` may only shorten that window). Including an optional `expected_endpoint_id` pre-binds the token so the Controller rejects any other presenting endpoint before consuming it. Write the token to a protected file, and never expose it in a URL, command-line argument, log, or shared shell history.

2. Point `BOOTSTRAP_TOKEN_SOURCE` at the protected token file and run the version-pinned Stage-1 installer. The installer prepares the local identity, enrolls immediately with the Controller, deletes the plaintext token source after enrollment succeeds, writes `agent.env` atomically, and stops at `ENROLLED_LOCAL` without enabling or starting any service.

   ```bash
   export BOOTSTRAP_TOKEN_SOURCE=/protected/node-bootstrap-token
   ./install.sh --version vX.Y.Z
   ```

   The token source must be a non-empty regular file (never a symlink) with no group or world permissions, and you must also supply the Controller, Relay, and command-key inputs listed under [Prepare the node configuration](#prepare-the-node-configuration).

   Record the printed UUIDv7 node ID and continue at [Approve the node](#approve-the-node).

### Advanced endpoint-bound enrollment

When no bootstrap token source is provided, you can use the two-step endpoint-bound enrollment flow.

1. On the node, prepare its persistent identity and record the printed EndpointID:

   ```bash
   sudo -u ocserv-agent /usr/libexec/ocservia/ocservia-agent \
     --identity-dir /var/lib/ocservia-agent/identity \
     --controller "$CONTROLLER_ENDPOINT_ID" \
     --prepare-enrollment
   ```

   Identity preparation runs offline without contacting the Controller or opening a transport session, and reusing the same identity directory preserves the EndpointID across runs.

2. Issue a short-lived endpoint-bound enrollment token, replacing the placeholders with the target workspace and the exact EndpointID from step 1:

   ```http
   POST /api/v1/enrollment-tokens
   X-Workspace-ID: <workspace-uuidv7>
   Content-Type: application/json

   {
     "workspace_id": "<workspace-uuidv7>",
     "environment": "production",
     "expected_node_name": "<node-name>",
     "expected_endpoint_id": "<64-lowercase-hex-endpoint-id>",
     "reason": "Enroll managed node"
   }
   ```

   The plaintext token is returned once and expires within 15 minutes. Follow the same workspace rule as step 1 of the bootstrap flow, and never place the token in a URL, log, or shared shell history.

3. Copy the token to a root-owned file readable by the Agent group:

   ```bash
   sudo install -o root -g ocserv-agent -m 0640 \
     /protected/enrollment-token /etc/ocservia-agent/enrollment-token
   ```

4. Run enrollment against the same identity directory:

   ```bash
   sudo -u ocserv-agent /usr/libexec/ocservia/ocservia-agent \
     --identity-dir /var/lib/ocservia-agent/identity \
     --controller "$CONTROLLER_ENDPOINT_ID" \
     --enrollment-token-file /etc/ocservia-agent/enrollment-token \
     --enrollment-environment production \
     --user-password-seal-key-id "$USER_PASSWORD_SEAL_KEY_ID" \
     --user-password-seal-public-key-sha256 "$USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256" \
     --p12-password-seal-key-id "$P12_PASSWORD_SEAL_KEY_ID" \
     --p12-password-seal-public-key-sha256 "$P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256" \
     --relay-mode custom \
     --relay-url "${RELAY_URL_A:?set the dedicated HTTPS relay URL}" \
     --relay-token-file /etc/ocservia-agent/relay-access-token
   ```

   During enrollment, the Agent signs and persists both sealing public-key descriptors. The command prints the pending UUIDv7 node ID and exits without opening a mutation-capable Agent session. Record the node ID and remove `/etc/ocservia-agent/enrollment-token`. The key descriptors are enrollment inputs. In `agent.env`, record the returned `NODE_ID` alongside the `AGENT_ENDPOINT_ID` binding from step 1.

   Before approval, complete the remaining host configuration: [manual trust provisioning](#manual-trust-provisioning) and, for production, the [Production Relay launcher](#production-relay-launcher).

### Finish enrollment

What happens after the installer depends on whether `BOOTSTRAP_TOKEN_SOURCE` was supplied:

| Installer result | What to do next |
| --- | --- |
| `ENROLLED_LOCAL` | Local enrollment succeeded. The installer does not query Controller state, so check the node's approval and connection status in the Controller UI. |
| `ENROLLMENT_READY` | No token was provided. Issue an endpoint-bound enrollment token (`expected_endpoint_id` set to the printed EndpointID, see [Advanced endpoint-bound enrollment](#advanced-endpoint-bound-enrollment)), write it to `/etc/ocservia-agent/enrollment-token` owned by `root:ocserv-agent` with mode `0640` (or point `BOOTSTRAP_TOKEN_SOURCE` at a protected bootstrap token file), and rerun the installer. |

The installer deletes one-time token files and the `BOOTSTRAP_TOKEN_SOURCE` file only after enrollment succeeds. If enrollment fails or the result is ambiguous, stop and follow the next section.

### Recover an uncertain enrollment response

If an enrollment response is lost or your SSH session drops mid-run, keep the original EndpointID, identity directory, and protected token material intact. Never generate a new identity or issue a new token just because the client did not receive a response. Recover according to the token type:

- With a bootstrap token (`BOOTSTRAP_TOKEN_SOURCE`), rerun the same installer with the original bootstrap binding so the Controller's same-endpoint recovery path can return the enrollment record it already committed. Retrying with the same bootstrap token and persistent EndpointID returns the same pending node for as long as the node remains pending. The Controller rejects the replay if the presenting EndpointID differs or if the node has already been approved, and it refuses any bootstrap enrollment for an EndpointID already present in its database.
- Endpoint-bound enrollment tokens do not support that bootstrap replay contract, and they are single-use. Before deciding whether a new token is needed after an interrupted endpoint-bound run, an administrator must look up the node by its original EndpointID in the Controller inventory. A replacement token on its own does not prove the earlier attempt failed.

Once local enrollment is confirmed, the installer deletes only its fixed internal copy of the enrollment token. The installer does not convert that copy to a root-only file: it stays `root:ocserv-agent` with mode `0640` so the Agent can read it. If power loss, `SIGKILL`, or another interruption stops the process between configuration commit and cleanup, rerunning the installer validates the existing identity and binding (failing closed on unsafe file metadata), completes any pending token cleanup, and prints the local summary. The external bootstrap token source file is deleted only by the enrollment invocation that actually consumed it, so a later rerun will not delete an arbitrary newly configured source path. Whenever enrollment fails or remains uncertain, the installer retains recovery material along with the long-term identity, Relay token, public trust files, and sealing private keys.

## Approve the node

Approval needs two independently authenticated principals. A requester creates a content-bound `node.approve` request, a second authorized principal approves it, and the requester then activates the node with the approved request ID. See the [approval principal boundary](../reference/stable-contracts.md#approval-principal-boundary).

1. Once the Agent configuration and production relay path are in place, use the requester account to create a content-bound `node.approve` request:

   ```http
   POST /api/v1/approval-requests
   X-Workspace-ID: <workspace-uuidv7>
   Content-Type: application/json

   {
     "action": "node.approve",
     "resource_type": "node",
     "resource_id": "<node-uuidv7>",
     "reason": "Approve managed node",
     "ttl_seconds": 600,
     "node_approval": {
       "labels": {"environment": "production"},
       "policy": "<approved-node-policy>",
       "capabilities": ["<capabilities-approved-for-this-node>"]
     }
   }
   ```

   Record the returned UUIDv7 `id` as `APPROVAL_ID` and its `request_hash` as `APPROVAL_REQUEST_HASH`. Request only the capabilities required by the node's policy, because the independent approver reviews the exact `labels`, `policy`, and `capabilities` in this payload. The requested capabilities must be a non-empty subset of those the node advertised during enrollment.

2. Have a second authorized principal review the request content and approve it by binding the decision to the returned `request_hash`:

   ```http
   POST /api/v1/approval-requests/<approval-uuidv7>:approve
   Content-Type: application/json

   {
     "reason": "Independent node approval review",
     "expected_request_hash": "<approval-request-hash>"
   }
   ```

3. Using the requester account, activate the pending node with the approved request ID:

   ```http
   POST /api/v1/nodes/<node-uuidv7>/approval
   X-Approval-ID: <approved-request-uuidv7>
   Content-Type: application/json

   {
     "labels": {"environment": "production"},
     "policy": "<approved-node-policy>",
     "capabilities": ["<capabilities-approved-for-this-node>"],
     "reason": "Approve managed node"
   }
   ```

   The `labels`, `policy`, and `capabilities` in this activation payload must match the approved `node.approve` request exactly. Keeping the same `reason` string makes the audit trail easier to follow. Node activation always requires a valid approved request UUIDv7 in `X-Approval-ID`; independent approval cannot be bypassed by policy.

A completed activation returns `status: active`. The activation idempotency rules are in [Enrollment](../reference/stable-contracts.md#enrollment). If the API responds with `503` ("Trust update pending"), the approval has already been committed in the database while transport trust synchronization is still catching up. Either wait for background convergence or repeat the identical request with the same `X-Approval-ID`, without creating a second approval request.

If an approval request expires before activation, create a fresh approval request bound to the current content and have an independent approver review it; an expired approval never requires a new node identity. A pending node cannot execute privileged mutations until it has been approved and activated.

### Register the privd attestation key

Privileged mutations need a registered privd result attestation key. Until the key is registered and `privd_result_attestation_v1` is approved, the node remains readable while privileged mutations fail closed. Receipt contents are defined in [Privd receipt v1](../reference/stable-contracts.md#privd-receipt-v1).

The one-time Controller credential, delivered exclusively to root provisioning, is issued through `POST /api/v1/nodes/{node_id}/privd-attestation-credentials`. The operator runs the installed root-only helper

```text
ocservia-privd attestation-registration <key-path> <node-uuidv7> <controller-nonce-hex> <credential-context-sha256-hex>
```

and submits its public JSON output alongside the one-time credential to `POST /api/v1/nodes/{node_id}/privd-attestation-keys:register` (request and response schemas are in [`openapi.yaml`](../../openapi/openapi.yaml)). The packaged default key path is `PRIVD_ATTESTATION_KEY_FILE=/var/lib/ocservia-privd/attestation.key` in `/etc/ocservia-agent/privd.env`. The registration endpoint's request budget and the helper's guarantees are in [Privd receipt v1](../reference/stable-contracts.md#privd-receipt-v1).

Roll out upgrades in strict order:

1. Deploy the receipt-aware Controller and its database migration.
2. Initialize and independently register the privd key, consuming the root-authenticated credential to approve `privd_result_attestation_v1`.
3. Upgrade privd and the Agent.
4. Confirm that the Agent advertises and negotiates that capability before dispatching privileged commands.

### Rotate the privd attestation key

Key rotation provisions a new key path and credential. The overlap limit and the effect of explicit revocation (`POST /api/v1/nodes/{node_id}/privd-attestation-keys:revoke`) are in [Privd receipt v1](../reference/stable-contracts.md#privd-receipt-v1). During an operator-approved rotation, update the root-managed `PRIVD_ATTESTATION_KEY_FILE` setting in `privd.env` to point to a fresh file name, register and verify the new key before retiring the old one, and never overwrite or copy a private key in place.

## Start and verify the node

Neither the bootstrap installer nor manual enrollment enables or starts systemd units, and starting the local daemons does not approve the node. After approval and attestation registration, enable and start both services. The node then connects to the Controller and starts accepting signed commands:

```bash
sudo systemctl enable --now ocservia-privd.service ocservia-agent.service
systemctl status ocservia-privd.service ocservia-agent.service
```

Local `systemctl` status shows only that the units run on the host. Confirm the node on the Controller. In the selected Workspace, open the node, or query `GET /api/v1/nodes/{node_id}` from an authorized workspace session, and inspect `trust_status`, `connection_state` and `freshness`. The node is operational when they are `active`, `online` and `fresh` with a recent heartbeat. The `status` field returned by the approval and activation POST tracks a different state transition. Never copy that administrator session onto the managed node.

Once the node is approved and its services are running, you can rerun the same release installer with the original `install.env` and source paths. The rerun is idempotent and works as a convergence check:

```bash
../ocservia-vX.Y.Z/deploy/managed-node/install.sh
```

On an active node, the script reports `SERVICES_ACTIVE`. It does not reinstall the package, overwrite the identity, keys, relay configuration, or `agent.env`, re-approve the node, or restart the services. It does rerun the host preflight, verify the enrolled identity and trust files (failing closed on any discrepancy), and remove any leftover enrollment token file. You still need to check approval, connection state, and telemetry freshness on the Controller. After the Agent has been upgraded or rolled back to a different version, do not run an older release's installer on the host. It stops on the version mismatch.

## Revoke a node

Node revocation follows the same independent two-principal approval pattern as activation and works from any non-revoked state, including a pending node. You also use this flow on a reachable source Controller when rebinding a node to another Controller.

1. As the requester (a principal holding `node.revoke`, such as SecurityAdmin or PlatformAdmin), create a content-bound revocation request:

   ```http
   POST /api/v1/approval-requests
   X-Workspace-ID: <workspace-uuidv7>
   Content-Type: application/json

   {
     "action": "node.revoke",
     "resource_type": "node",
     "resource_id": "<node-uuidv7>",
     "reason": "Revoke managed node",
     "ttl_seconds": 600
   }
   ```

2. Have a different SecurityAdmin or PlatformAdmin verify and approve the returned request hash, as in step 2 of [Approve the node](#approve-the-node).
3. As the requester, submit the revocation with the approved request ID:

   ```http
   POST /api/v1/nodes/<node-uuidv7>/revocation
   X-Approval-ID: <approved-request-uuidv7>
   Content-Type: application/json

   {"reason": "Revoke managed node"}
   ```

   The response reports `status: revoked`. If you receive a `503` ("Revocation committed") response, the revocation is already committed in the database but transport trust synchronization is incomplete. Preserve the original Request ID, approval ID, reason, revision and error, and follow [Revocation committed with synchronization pending](incident-recovery.md#revocation-committed-with-synchronization-pending) before retrying or requesting another approval.

## Configuration and TLS operations

The rules for plans, hashes, revisions and node TLS are in [Configuration and node TLS](../reference/stable-contracts.md#configuration-and-node-tls). This section covers the operator steps.

### Plan and apply a configuration

Submit complete-profile plans from the Web node detail page. Request a production apply approval only after the plan has validated cleanly and remains unexpired, and have a second principal review it before apply. The plan, approval and apply rules are in [Planning](../reference/stable-contracts.md#planning) and [Apply and rollback](../reference/stable-contracts.md#apply-and-rollback).

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

Upgrade an existing managed node by installing an explicitly selected Agent package, or through a Controller-driven upgrade. The v1.1.0 scripts remove source-version windows and target-version ordering checks, but cross-version operation is still not guaranteed to be safe. Previously installed older scripts retain their original checks, and the lifecycle does not convert historical deployments; see the [version policy](production-deployment.md#version-policy). The package scripts do not automatically enroll or rebind keys, reset node identity, or purge existing certificate artifacts, and the former upgrade-time enrollment environment variables and marker are no longer used. Removing historical layout checks does not make arbitrary cross-version operations safe or grant an installed binary capabilities it does not implement.

### Upgrade

Native package installation runs the same verified repository lifecycle as the archive installer. An upgrade installs the matching complete node package: the Agent, privd and upgrader binaries, the rollback, retention, rebind and verification tools, the production Relay launcher, and the systemd units and retention timer. It keeps the existing identity, configuration and durable state. It does not enroll, approve, or authorize the node, and it cannot recover state left behind by a failed upgrade (use [Rollback](#rollback) for recovery).

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

Once the preflight passes, the script saves one matched snapshot under the root-only `/var/lib/ocservia-upgrade/upgrade-backup` hierarchy covering the previous Agent and privd binaries, both base systemd units, the durable upgrader and verifier, the production relay drop-in and launcher, the rebind helper, the retention executable, and the retention service and timer. Keeping this directory outside privd's systemd-managed `StateDirectory` prevents service startup from rewriting rollback evidence ownership. A root-owned manifest binds the exact snapshot digests, and rollback rejects unsafe ancestry, symlinks, hard links, ownership, modes, or file replacement. Optional files carry explicit `.previous` or `.absent` records that tell rollback whether to restore or remove the installed file. Retention depends on its matching rebind helper, the retention service depends on the retention executable, and the timer depends on the service. A rebind helper without retention remains a valid layout. Upgrading to a different package overwrites the previous snapshot, leaving only the most recent pre-upgrade state available for restoration. Reinstalling an identical verified package keeps the existing snapshot intact and restarts the running services.

Before modifying package-owned files, the upgrade script persists the complete snapshot, atomically publishes the verified standalone rollback driver, and records the verified target digest in `/var/lib/ocservia-upgrade/installing-package`. If an upgrade is interrupted, a subsequent direct or native-package retry refuses to overwrite the existing snapshot; run the verified rollback first to restore the host, then retry the package operation. A matching completed retry clears a stale `installing-package` record only after verifying the installation commit, every package-owned file, and the rollback snapshot, while rollback clears the record only after all restores and removals are synced across filesystems. Neither path alters Controller binding authority or reverses database migrations. When an identical retry completes, it also starts an enabled retention timer after releasing the lifecycle lock, recovering service activation if a prior run was interrupted after the installation commit (explicitly disabled or masked timers are left untouched).

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
- Reconcile any Controller operation affected by the outage. Running rollback stops the Agent, `privd`, active upgrader units, and the retention job, and marks every non-terminal (`accepted` or `running`) local upgrade operation `rolled_back` so it cannot be re-applied afterwards.
- Ensure the snapshot is complete with all 13 recorded entries. Older 8- and 9-record snapshots remain readable with `ocservia-agent-rollback --verify-only`, but they lack prior rebind and retention evidence, and 8-record snapshots also lack launcher evidence. A mutating rollback refuses incomplete snapshots before stopping services, because missing records do not prove that the prior files were absent on disk. Recovering from an incomplete snapshot requires either a complete trusted matched snapshot or an independently reviewed manual restoration of the missing artifacts; no force or skip flag is provided.
- To validate the snapshot before changing anything on disk, run `sudo /usr/libexec/ocservia/ocservia-agent-rollback --verify-only`.

To roll back the complete matched snapshot, run:

```bash
sudo /usr/libexec/ocservia/ocservia-agent-rollback
```

The rollback command validates the complete snapshot and target destinations before stopping services. It stops retention before replacing the retention executable and imported rebind helper, restores binaries and units together (including the production relay drop-in and launcher state), reloads systemd, and starts privd before the Agent unconditionally, even if those services were stopped before rollback began. A restored retention timer is restarted after the completed lifecycle releases its lock so persistent timer catch-up runs against the restored files. An explicitly absent timer is disabled and removed. The command clears any pending `installing-package` record. The standalone rollback driver itself remains on disk so it can finish executing and re-read the snapshot if invoked again. Restoring binaries without their matching units is unsupported because the CLI and local wire contract can change together; never restore one binary on its own.

The verifier and rollback script do not infer a software version from a missing Relay launcher or reject a snapshot on that basis alone. Any launcher referenced by a service unit must still exist on disk, and all snapshot integrity, trust, and configuration checks continue to apply. Operator configuration, node identity, and persistent state are preserved without silent conversion or reset. Removing the version check does not guarantee that the restored binary will work with the current configuration or on-disk data, and older scripts already installed on the host keep their original behavior.

Rollback coordinates directly with the [durable upgrade state](#durable-self-upgrade-runner): it stops any running `ocservia-upgrader@*.service` instance, marks non-terminal operations `rolled_back` so a stale runner cannot re-apply the rolled-back release, and restores or removes the upgrader binary, the `ocservia-upgrader@.service` unit, and the installed verifier according to the matched snapshot (`.previous` restored, `.absent` removed).

A rollback also restores the previous release's security properties, so treat it as a temporary recovery measure and move to a fixed release promptly. The privd attestation rollback requirements are in [Privd receipt v1](../reference/stable-contracts.md#privd-receipt-v1).

To verify the rollback, run `systemctl status ocservia-privd.service ocservia-agent.service`. Before sending new work to the node, confirm in the Controller that the node has reconnected under the expected previous version and is reporting fresh capabilities and trust state. Completing the local rollback does not by itself prove that the Controller accepts the node.

If rollback fails, stop immediately and preserve both the snapshot and host diagnostics instead of repeatedly rerunning the upgrade or rollback commands. Rollback validates the entire snapshot before changing any files, so an error reported during validation leaves the installed files untouched. A failure after services have been stopped can leave the node offline. Never replace the snapshot with files from an arbitrary download directory.

### Controller-driven single-node upgrade

The console exposes one reconciled upgrade endpoint per node: `POST /api/v1/nodes/{node_id}/agent-upgrade` (RBAC action `agent.upgrade`, Operator role), which accepts only a target version, an approval ID, and a reason alongside a required `Idempotency-Key` header. Callers never supply a URL, file path, or package digest; the Controller looks up the digest in the operator-provisioned release catalog pointed to by `OCSERV_AGENT_RELEASE_MANIFEST` (an absolute path with no default). When that environment variable is unset, the Controller starts without a catalog and returns `503` on the agent-upgrade and agent-rollout endpoints. Enabling Controller-driven AgentUpgrade and rollouts requires a deployment-specific override that mounts the catalog file read-only into the control-plane container, because the shipped production Compose neither sets nor mounts `OCSERV_AGENT_RELEASE_MANIFEST`. The catalog file has this form:

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

This manifest is the only digest source for the workflow and must list between 1 and 512 unique `(version, architecture)` entries; once `OCSERV_AGENT_RELEASE_MANIFEST` is set, a missing, unreadable, malformed, or ambiguous file aborts Controller startup. The Controller does not synchronize with GitHub or container registries, so preparing an upgrade consists of placing the archive in each node's local spool (see [Durable self-upgrade runner](#durable-self-upgrade-runner)) and adding its exact digest to the manifest. Under the v1.1.0 policy reset, the Controller accepts any explicit trusted target regardless of whether it is lower, equal, or higher than the current version or whether the source version is unknown; version classifications shown in the UI are informational and do not govern execution. Each request must still supply the node's current revision (`If-Match`) and an independent approval bound to the exact `node + version + digest + architecture` release identity.

To dispatch the upgrade, the Controller requires an approved typed-command capability on the node (`ocserv.agent.upgrade.v2` or `ocserv.agent.upgrade.v1`, preferring v2 when both are approved) and binds that exact capability name into the signed command. Nodes advertising only v1 remain eligible despite lacking the former software downgrade fence; the Controller never fabricates capabilities or converts historical payloads. The already-installed runner executes the command, so previously installed code retains its own runtime behavior. Advertising a command capability does not guarantee cross-version safety or certify historical version combinations.

New upgrade operations start in the `queued` state and move to the non-terminal `accepted` state when the Agent acknowledges scheduling (acknowledgement alone never counts as completion). As the upgrader restarts the Agent and privd, the node temporarily disconnects as part of normal progress. The Controller determines terminal outcomes strictly from durable evidence: `succeeded` requires the node to reconnect and report the target Agent version alongside a successful local terminal result from the upgrader; an explicit local failure closes the operation as `failed`; and a matched rollback closes it as `rolled_back`. If an operation remains unresolved when the reconciliation window expires (`OCSERV_AGENT_UPGRADE_RECONCILE_TIMEOUT`, defaulting to 30 minutes with an accepted startup range of 1 minute to 24 hours; not forwarded by the shipped production Compose unless overridden), the Controller closes it conservatively as `unknown` and marks its command expired to prevent blind retries. Only one upgrade may be active per node at a time, so concurrent requests fail with a conflict until the active operation reaches a terminal state. Every terminal transition appends an audit record capturing the operator, approval identity, source and target versions, and outcome.

During regular telemetry heartbeats, the Agent queries privd read-only for terminal upgrader outcomes and forwards them to the Controller, which accepts the first report per operation and cross-checks it against the node's observed version before marking the upgrade `succeeded`. Nodes still running an older privd omit these local outcome reports and resolve instead through version observation, explicit failure or rollback paths, or the conservative `unknown` timeout.

### Durable self-upgrade runner

Controller-driven upgrades run outside the Agent and privd processes. When privd accepts a Controller-signed `AgentUpgrade` command, it independently re-validates the typed release identity (version, SHA-256 digest, and architecture), commits an immutable root-owned intent under `/var/lib/ocservia-upgrade/operations/<operation-id>/`, and starts the on-demand unit `ocservia-upgrader@<operation-id>.service`. The unit uses `Type=exec`, so privd's handoff returns as soon as the runner binary starts. As a result, the upgrade never terminates the process responsible for reporting the command result. The unit omits an `[Install]` section because only privd starts it, and the runner refuses to execute unless a committed intent exists in the active binding's operations namespace.

Each operation directory contains three fixed `root:root` mode `0600` records written atomically via write, fsync, and rename: `intent` (holding the schema version, operation and command IDs, target version, package digest, architecture, and semantic payload hash, all immutable once committed), `state` (one of `accepted`, `running`, `succeeded`, `failed`, or `rolled_back`), and `result` (terminal evidence written upon completion). Replaying the same operation ID with an identical signed identity converges on the committed intent. Reusing an operation ID with a different identity is rejected. Only one active operation may exist per node. If privd loses its effect journal between committing the intent and recording journal completion, the durable intent store serves as the idempotency authority for this command family.

The runner loads packages exclusively from the fixed local spool directory `/var/lib/ocservia-upgrade/package-spool`, without performing URL fetches or accepting caller-supplied paths. Before issuing the upgrade command, the operator or provisioning pipeline must place `ocservia-agent-<version>-linux-<arch>.tar.gz` in that spool as a non-empty regular file (no symlinks or hard links, within the runner's size limit) owned by root and not writable by group or world; otherwise the upgrade fails with unsafe spool metadata. The runner verifies that the spool archive digest equals `package_sha256` in the durable intent and passes that exact digest to `/usr/libexec/ocservia/ocservia-agent-verify`, which copies the archive into root-owned staging, re-checks its SHA256, and extracts it safely. Any digest mismatch fails closed before the lifecycle runs. Once the verified marker matches the intent, the runner executes the package's `upgrade-agent.sh` script, re-verifies the installed binaries against the verified package, restarts `ocservia-privd` and `ocservia-agent`, and writes the terminal result. If the runner crashes or restarts mid-flight, a `running` operation whose installation commit record and installed binaries already match the package skips repeating the destructive lifecycle, and any validation failure records `failed` evidence before exiting non-zero.

All native DEB and RPM packages (`amd64` and `arm64`) include the durable runner: `install-agent.sh` installs the runner binary, the `ocservia-upgrader@.service` unit and the verifier, every package carries them, and `uninstall-agent.sh` removes them.

### Fleet rolling upgrades

`POST /api/v1/agent-rollouts` (RBAC action `agent.upgrade`, Operator role) upgrades a selected set of nodes to a single trusted target version in bounded batches. The creation request takes the target version, node IDs, an optional `batch_size` (defaulting to 5, bounded from 1 to 20), a reason, and an approval ID. Callers cannot set `stop_on_failure` in the request; the server fixes `StopOnFailure = true`, and the referenced `agent.rollout` approval binding covers the target version, sorted node set, batch size, and `stop_on_failure = true`. Every selected node's architecture must have a matching target entry in the operator-provisioned release catalog, and every selected node must advertise an approved `ocserv.agent.upgrade.v1` or `ocserv.agent.upgrade.v2` capability.

Fleet version badges in the console and the recommended version displayed in Settings come from the operator-pinned `OCSERV_RECOMMENDED_AGENT_VERSION` (SemVer), which classifies observed versions without scheduling any upgrades on its own. Configure it in the Controller section of `install.env` or export it in the invoking shell before running the production installer or bootstrap, so the root lifecycle forwards the resolved value to the migrate and control-plane containers. An explicitly empty export overrides `install.env` and clears the recommendation.

When updating this setting on an existing installation, recreate the control-plane through the documented Controller lifecycle (see [Upgrade, rollback and uninstall](production-deployment.md#upgrade-rollback-and-uninstall)). Use `controller.sh start` to re-apply the current release's Compose project with the exported environment (`controller.sh upgrade`, by contrast, switches to a different release file), and export the variable in that invocation as well because `controller.sh` does not read `install.env`. Check Settings afterward to confirm the update took effect, since a Compose recreate run without the export silently retains or clears the prior value. When no recommendation is configured, observed versions remain visible without target comparison; invalid nonempty SemVer strings fail Controller configuration validation.

Batch 0 always runs as a mandatory single-node canary, and subsequent batches start only after that canary reaches `succeeded`. If the canary fails or is skipped, the rollout pauses; resuming it requeues the canary for a fresh attempt, and no operator action can bypass a successful canary.

Within each batch, every node follows the reconciled single-node upgrade lifecycle under its own stable operation ID, and nodes that reach `succeeded` are never redispatched. A batch advances only after all of its nodes reach a terminal outcome, pausing the rollout if any node finishes as `failed`, `unknown`, or `rolled_back`. If a node is ineligible when its batch dispatches (because it is offline, has a stale observation, withdrew its capability, has another upgrade active, or lacks a manifest entry), the Controller marks it `skipped` with a reason code and pauses the rollout. Resuming a paused rollout requeues the current batch's `failed`, `unknown`, `rolled_back`, and skipped-canary nodes for a fresh eligibility check, while skipped non-canary nodes remain `skipped` with their recorded reason. The rollout detail view tracks succeeded, failed, and skipped nodes per batch alongside the remaining count, so an overall rollout status of `succeeded` does not imply that every originally selected node was upgraded.

Rollout state persists across Controller restarts and console sessions. Viewing a rollout requires `operation.read`, and listing a workspace's rollouts requires that permission at workspace scope (node-scoped bindings do not list them). Creating or resuming a rollout requires `agent.upgrade`, and each rollout is visible and resumable only within its own workspace.

## Rebind to another Controller

Run the packaged `ocservia-agent-rebind` CLI locally on the managed node as root when moving an Agent to another Controller during planned maintenance or recovery. An operator must still approve the node on the target Controller. What rebinding preserves and what it does not copy, and the authority, fence and quarantine rules, are in [Controller rebind](../reference/stable-contracts.md#controller-rebind).

Before starting, install a matched release that includes the rebind CLI, Agent, privd, and upgrader together on a host with Python 3, OpenSSL, and util-linux. The node must use the packaged systemd launchers and their standard state paths; custom launchers or state arguments are refused because their recovery evidence cannot be inferred safely. The initial `agent.env` must retain its independently verified `AGENT_ENDPOINT_ID` enrollment binding. Once a node has been rebound, bootstrap installation reruns refuse it, so future upgrades must use the native package lifecycle.

Independently provision the target Controller EndpointID and Ed25519 command verification public key on the host, storing the key and bootstrap token as protected root-owned regular files under trusted root-owned parent directories. The existing relay configuration must already reach the target Controller through a single dedicated Relay, with `RELAY_URL_B` omitted or left empty in `relays.env`. If `RELAY_URL_B` is non-empty, `prepare` (including `--dry-run`), `resume`, and any uncommitted operation will abort before enrollment, operation creation, or service shutdown. `status` inspection and post-publication recovery remain available.

1. If the old Controller is still reachable, revoke its node record using the standard independently approved [node revocation flow](#revoke-a-node). When the old Controller is permanently unavailable, declare that state with `--source-disposition unreachable` in step 3; contacting the old Controller is not required, and the CLI records the disposition you declare without probing it.
2. On the new Controller, create a short-lived bootstrap token via `POST /api/v1/node-bootstrap-tokens` with `expected_endpoint_id` set to the existing Agent EndpointID so the Controller checks that binding before consuming the token. Rebinding uses the existing possession proof and recoverable `obt1_` token protocol (no other token type is accepted) and always assigns a new NodeID. The target must be a different Controller with a different command key, and its `environment` must match the token's. Its database must have no record of this EndpointID in any state: a Controller that has previously held the endpoint (even in `revoked` state) refuses the enrollment.
3. Prepare the rebind and enroll with the target Controller, supplying a reason and the source disposition:

   ```sh
   sudo /usr/libexec/ocservia/ocservia-agent-rebind prepare \
     --controller <target-controller-endpoint-id> \
     --command-key-file /etc/ocservia-agent/new-controller.pub.pem \
     --token-file /etc/ocservia-agent/new-controller.token \
     --environment production \
     --source-disposition revoked \
     --reason 'Replace the retired Controller'
   ```

   Pass `--dry-run` to validate host prerequisites without enrolling or publishing authority changes. During a normal run, `prepare` prints `PREPARED` along with a local operation UUID before contacting the Controller, followed by `ENROLLED_LOCAL` and the new pending NodeID.
4. Complete the standard [approval and activation procedure](#approve-the-node) on the target Controller. The node must be active before `commit` can verify a session, and bootstrap token possession does not authorize approval (if the enrollment response in step 3 is still unknown locally, recover it before approving the node). Privileged mutations also require [registering the node's privd attestation key](#register-the-privd-attestation-key) and approving `privd_result_attestation_v1`. Until that attestation is approved, the node remains readable while privileged mutations fail closed.
5. Commit the confirmed operation:

   ```sh
   sudo /usr/libexec/ocservia/ocservia-agent-rebind commit <operation-uuid>
   ```

Running `commit` stops the Agent, privd, and any active upgrade runners, publishes a single root-owned authority record at `/etc/ocservia-agent/active-binding`, and starts privd before the Agent. It then waits up to about a minute for a signed session grant from the new Controller and verifies it independently through privd before recording `VERIFIED` and marking the source binding sealed in the operation record while leaving the historical source files in place. The authority selector covers the Controller endpoint, command key, new NodeID, and all derived durable namespaces, so an old Controller that comes back online later cannot regain authority.

### Resume and inspect

```sh
sudo /usr/libexec/ocservia/ocservia-agent-rebind status <operation-uuid>
sudo /usr/libexec/ocservia/ocservia-agent-rebind resume <operation-uuid>
```

Use `resume` when preparation or enrollment is interrupted before authority publication. A response timeout leaves the original token and identity in place, so rerunning `resume` recovers that token's same pending node on the target Controller. Do not create a second token or node record to work around an unknown response. Only one unfinished operation is permitted at a time. Supplying an incorrect pin, key, or token leaves the source authority untouched, and incomplete filesystem staging is preserved for diagnosis and refused instead of being repaired by generating a new endpoint key. Any failure before the selector is committed leaves the source binding active and untouched.

Once `commit` has published the authority record, rerun `commit` to retry service startup and session verification. If restarting services or verifying the session fails at that stage, the target binding remains committed with the source evidence intact. The node never reverts automatically to the old Controller. Resolve the target approval, network connectivity, or host health issue and rerun `commit`. If target commands may already have run, moving back to the original Controller requires a new explicit rebind operation, and a Controller that previously held the endpoint refuses it. A stale operation cannot override a newer binding, and package upgrades and rollbacks share the lifecycle lock and refuse binaries that cannot enforce an existing binding.

Local operation records are stored under `/var/lib/ocservia-rebind/<operation-uuid>` and must be kept alongside the source databases for recovery. If `commit` finds unfinished commands in the Agent journal, a missing journal, prepared privileged effects, incomplete upgrades, or unreadable evidence when publishing the binding, it quarantines privileged mutations for the target Controller while still permitting read-only operation and new authority verification. The CLI provides no command to clear that quarantine; preserve and reconcile the underlying records, and never alter file timestamps or delete a state database to clear quarantine. Rebind performs no historical cleanup. See [Rebind and retention limits](incident-recovery.md#rebind-and-retention-limits).

### Independent local retention

Historical compaction runs independently of rebind, on its own schedule. Package installation enables a single host-wide `ocservia-agent-retention.timer` that runs hourly with a randomized delay (inspect failures with `journalctl -u ocservia-agent-retention.service`). To override the default windows, create a root-owned `/etc/ocservia-agent/retention.env` with mode `0600`:

```ini
OCSERV_AGENT_RETIRED_BINDING_RETENTION_DAYS=30
OCSERV_REBIND_HISTORY_RETENTION_DAYS=365
```

Permitted values are listed in the retention table in [Independent retention](../reference/stable-contracts.md#independent-retention); zero, negative, unknown, or duplicate settings are rejected as errors. Every run validates `retention.env` before touching disk, and you can run `sudo /usr/libexec/ocservia/ocservia-agent-retention` directly to validate the file and execute one bounded cleanup pass.

On the Controller, the scheduler compacts terminal command payloads and retired-node snapshots on its own. The cutoff variables, defaults and safety boundaries for both the Controller and the nodes are in [Independent retention](../reference/stable-contracts.md#independent-retention).

## Remove a node package

Package removal (`dpkg -r`, `rpm -e`) runs the verified `uninstall-agent.sh`, which disables and stops the Agent, privd, and retention timer and removes the binaries and units (including the production Relay launcher) while preserving identity, state, and configuration. The Controller then shows the node offline. Neither `dpkg --purge` nor the package manager triggers the state purge; only `uninstall-agent.sh --purge-state` does so.

Passing `--purge-state` is irreversible and should only be done after revoking the node identity on the Controller (see [Revoke a node](#revoke-a-node)) and archiving required audit material. It deletes the Agent and privd state directories, `/var/lib/ocservia-upgrade` (including the rollback snapshot, preventing any subsequent rollback), and `/etc/ocservia-agent` (`agent.env`, keys, and relay token), and then removes the `ocserv-agent` user and group. Purging local state does not update the Controller, so revoke the node record there separately.

## Diagnostics

### Journal storage monitoring

Monitor journal storage independently of Agent connectivity. The command journal has no automatic retention policy, and normal authorized commands accumulate durable state over time. From a reviewed checkout, run this read-only Linux probe as `ocserv-agent` (or another account permitted to stat the state directory):

```bash
sudo -u ocserv-agent bash scripts/check-agent-storage.sh /var/lib/ocservia-agent/agent.db
```

Pass the actual database path when `--journal` overrides the default. When an active Controller binding exists (for example after a rebind), the Agent writes its journal to `/var/lib/ocservia-agent/bindings/<node-id>/agent.db` instead of the default path, so monitor that file. The probe reports the logical sizes of the database, WAL, and SHM files, their combined allocated bytes, and available filesystem bytes and inodes without opening SQLite or checkpointing, truncating, or deleting any state. If the state files are missing or unreadable, the probe reports UNKNOWN (exit code 3).

Schedule this probe in the node's monitoring agent every 15 minutes with a 30-second execution timeout. Exit codes are 0 OK, 1 WARNING, 2 CRITICAL, and 3 UNKNOWN; configure alerts on exit codes 1, 2, or 3, on timeouts, or after two missed samples. The default warning and critical thresholds are 80% and 90% for both disk and inode usage, and can be tuned with `STORAGE_WARNING_PERCENT` and `STORAGE_CRITICAL_PERCENT`. These defaults are starting operational thresholds and do not guarantee capacity. The repository does not install a scheduler or notification destination automatically.

Retain the collected metrics in your monitoring backend to track `journal_bytes` and `journal_allocated_bytes` growth and forecast exhaustion from declining `filesystem_available_bytes`. Alert whenever projected runway drops below seven days, even if percentage thresholds have not yet fired. Record a baseline after deployment and verify that a synthetic warning reaches the on-call destination:

```bash
sudo -u ocserv-agent env STORAGE_WARNING_PERCENT=1 STORAGE_CRITICAL_PERCENT=99 \
  bash scripts/check-agent-storage.sh /var/lib/ocservia-agent/agent.db
```

When a warning fires, inspect journal growth alongside unrelated files on the same filesystem and plan capacity expansion. On a critical alert, stop initiating new command dispatches or rollouts to the affected node and expand storage before resuming operations. Always keep the database and WAL files together during controlled recovery; never delete command identities, revision fences, pending or unknown records, or WAL files to reclaim space, and do not introduce age-based cleanup until idempotency, signed replay, and recovery retention boundaries are explicitly defined.

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

Enrollment and bootstrap tokens are single-use and short-lived. Issue a new token only after confirming that the previous token was rejected or expired. After a failed or unknown enrollment response, always query the Controller by that EndpointID before issuing a new token, and follow [Recover an uncertain enrollment response](#recover-an-uncertain-enrollment-response) first.

If the Controller enrollment routes return `404`, the Controller has no `OCSERV_CONTROLLER_ENDPOINT_ID` configured. See [Prepare the configuration](production-deployment.md#prepare-the-configuration).

Enrollment only creates a pending node record. A different authorized principal must approve the node before it can open a normal mutation-capable session, even if the host already shows `ENROLLED_LOCAL` or active local services. See [Approve the node](#approve-the-node).

### Agent will not start after an upgrade

Inspect `journalctl -u ocservia-privd -u ocservia-agent` to see which check failed, then verify `/etc/ocservia-agent/agent.env`, the independently provisioned command verification key, both distinct sealing keys, `relays.env` (which requires a literal HTTPS `RELAY_URL_A` and rejects any non-empty `RELAY_URL_B`), and the relay access token and CA files it references. When an upgrade fails during preflight before modifying files on disk, fix the reported prerequisite and retry. If the new binaries were already installed and must be reverted, run [Rollback](#rollback) instead of copying binaries from an unverified directory.
