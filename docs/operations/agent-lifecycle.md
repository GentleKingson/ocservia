# Agent package lifecycle

> **Technical reference.** For the operator path, start with [Install a managed
> node](../getting-started/managed-node.md), [Upgrade the Agent](../how-to/agent-upgrade.md),
> or [Roll back the Agent](../how-to/agent-rollback.md). This document retains
> package construction, verified staging, sealing-key migration, and durable
> lifecycle contracts.

Build the Agent and privd release binaries, then create a deterministic signed package:

```bash
OUTPUT_DIR=dist AGENT_SIGNING_KEY=/secure/release-ed25519.key \
  VERSION=1.0.0 PACKAGE_ARCH=amd64 SOURCE_DATE_EPOCH=1786147200 scripts/package-agent.sh
VERIFIED_PACKAGE="$(sudo AGENT_TRUSTED_KEY_SHA256=<pinned-public-key-der-sha256> \
  scripts/verify-agent-package.sh dist/ocservia-agent-1.0.0-linux-amd64.tar.gz \
  dist/ocservia-agent-1.0.0-linux-amd64.tar.gz.sha256 \
  dist/ocservia-agent-1.0.0-linux-amd64.tar.gz.sha256.sig \
  /etc/ocservia/release-signing.pub.pem)"
sudo INSTALL_PRODUCTION_RELAYS=true \
  "${VERIFIED_PACKAGE}/scripts/install-agent.sh"
```

Provision the verification public key and its DER SHA-256 fingerprint through a separate trusted channel; never trust the `.pub.pem` published beside a package. Verify signature, trusted-key fingerprint, checksum, and archive contents before extracting as root. Set `INSTALL_PRODUCTION_RELAYS=true` during installation and fill `/etc/ocservia-agent/relays.env` with both dedicated HTTPS relay URLs. Install the relay token at `/etc/ocservia-agent/relay-access-token` as `root:ocserv-agent` mode `0640`.

The verifier copies the archive, signed checksum, signature, and pinned public
key into a unique `root:root` mode `0700` directory below
`/var/lib/ocservia-upgrade/package-staging`. It verifies the exact copied checksum,
computes the exact copied archive digest, rejects unsafe archive paths and
member types, and extracts that same root-owned archive. Install and upgrade
scripts accept only the verified directory printed by the verifier. Do not
extract or run installers from a download directory, and remove the verified
staging directory after the lifecycle operation succeeds.

`PACKAGE_ARCH` pins the package architecture (`amd64` or `arm64`); the MANIFEST
records it as `arch=`. On a real host install (no `DESTDIR`) the verifier also
rejects a foreign-architecture package — `x86_64` ↔ `amd64`, `aarch64` ↔
`arm64` — before anything is staged.

## Native installer packages

Each release publishes the verified archive plus native installers for both
architectures: `ocservia-agent-<version>-linux-{amd64,arm64}.tar.gz` with its
`.sha256`/`.sha256.sig` sidecars, `ocservia-agent_<version>_{amd64,arm64}.deb`,
`ocservia-agent-<version>-1.{x86_64,aarch64}.rpm`, one `SHA256SUMS` covering
the six packages, and, on formal Controller releases, the Controller manifests
`controller-release.json`, `controller-release-amd64.json`, and
`controller-release-arm64.json` with their checksums,
the Ed25519 `SHA256SUMS.sig`, and `release-signing.pub.pem`.
All of them trust the same release key whose DER SHA-256 fingerprint is pinned
out of band.

The `.deb` and `.rpm` embed the signed archive triple, the release public key,
the pinned fingerprint, and the verifier under `/usr/share/ocservia-agent`.
Their scriptlets contain no layout logic: `postinst` refuses a host-architecture
mismatch, verifies the archive into trusted staging, and runs the verified
`install-agent.sh` (fresh host) or `upgrade-agent.sh` (existing installation).
Installing or upgrading never enables or starts a service — provision
`/etc/ocservia-agent/agent.env` and the keys first, then enable both units
manually. Package removal runs the verified `uninstall-agent.sh`, preserving
identity, state, and configuration by default.

Package-manager downgrade is not a supported rollback path: it would skip the
matched snapshot contract. Roll back only with
`sudo /usr/libexec/ocservia/ocservia-agent-rollback`, then install the fixed
release.

`upgrade-agent.sh` first verifies that the existing `agent.env` contains exactly
one absolute `CONTROLLER_COMMAND_VERIFICATION_KEY_FILE` and that the referenced
Ed25519 public key satisfies the intersection of the Agent and privd type,
ownership, mode, link, symlink, size, and ancestry requirements. The shared key
must be independently provisioned as `root:ocserv-agent` mode `0440` or `0640`
beneath root-controlled ancestry. An Agent-owned legacy key is rejected rather
than promoted into a root trust anchor. This preflight happens before backups,
binaries, systemd units, or services are changed. A legacy two-line `agent.env`,
missing key, or unsafe key therefore stops the upgrade with provisioning
instructions instead of installing an Agent that cannot restart.
It also requires two distinct enrolled sealing key IDs and public-key
fingerprints, derives each fingerprint from its root-owned private key, and
rejects missing, reused, mismatched, symlinked, or unsafe key files before
changing installed state. For the first upgrade from a release without
purpose-separated sealing keys, issue a fresh enrollment token bound to the
node's existing EndpointID, make the root-protected token file readable by the
`ocserv-agent` group, and run the upgrade with the token path and environment:

```bash
sudo ENROLLMENT_TOKEN_FILE=/etc/ocservia-agent/sealing-key-enrollment.token \
  ENROLLMENT_ENVIRONMENT=production \
  /path/to/verified-package/scripts/upgrade-agent.sh
```

Before replacing any installed file, the upgrade runs the verified new Agent
binary as `ocserv-agent`. The Controller accepts this migration only for the
same workspace and EndpointID, only when the node has no sealing-key binding,
and only when the signed capability advertisement exactly matches the stored
supported set. It binds both descriptors atomically, consumes the token, and
returns the existing node UUID. The upgrade compares that UUID with `NODE_ID`
and records an exact root-owned marker. A missing token, wrong endpoint,
changed capability set, partial key binding, or different returned node stops
the upgrade before binary, unit, backup, or service changes. Delete the token
file after a successful upgrade. A rolled-back legacy Agent may reconnect only
with read-only capabilities; mutation capability requires the two exact bound
descriptors.
After the preflight, the script retains one matched snapshot of the previous
Agent and privd binaries, both base systemd units, and the production relay
drop-in presence and content under the root-only
`/var/lib/ocservia-upgrade/upgrade-backup` hierarchy. This directory is outside
privd's systemd-managed `StateDirectory`, so service startup cannot rewrite
rollback evidence ownership. A root-owned manifest binds
the exact snapshot digests, and rollback rejects unsafe ancestry, symlinks,
hard links, ownership, modes, or replacement. It also preserves endpoint
identity, the durable Agent database, journal, and configuration. Verify service health and
Controller connectivity after upgrade. To roll back the complete matched
snapshot, run:

```bash
sudo /usr/libexec/ocservia/ocservia-agent-rollback
```

The command validates the complete snapshot before stopping either unit, then
restores binaries and units together, reloads systemd, and starts privd before
Agent. Restoring only the binaries is unsupported because their CLI and local
wire contract may require the matching units. A rollback also restores the
previous release's security properties, so use it only for a controlled
recovery window and return to a fixed release promptly. `uninstall-agent.sh` preserves
identity and journal by default;
`--purge-state` is irreversible and is appropriate only after revoking the node
identity and preserving required audit material.

## Durable self-upgrade runner

Controller-driven upgrades never execute inside the Agent or privd. When privd
accepts a Controller-signed `AgentUpgrade` command it independently re-validates
the typed release identity (version, SHA-256 digest, architecture), commits an
immutable root-owned intent under
`/var/lib/ocservia-upgrade/operations/<operation-id>/`, and starts the fixed
on-demand unit `ocservia-upgrader@<operation-id>.service`. privd's
responsibility ends there: the unit is `Type=exec`, so the handoff returns as
soon as the runner binary starts, and no upgrade can destroy the process that
still owes the command result. The unit has no `[Install]` section — it exists
only to be started by privd — and refuses to run without the committed intent
(`ConditionPathExists`).

Each operation directory holds three fixed records, all root-owned mode `0600`,
written atomically (write, fsync, rename): `intent` (schema version, operation
and command IDs, target version, package digest, architecture, semantic
payload hash — immutable after commit), `state` (one of `accepted`, `running`,
`succeeded`, `failed`, `rolled_back`), and `result` (terminal evidence written
when the operation finishes). Replaying the same operation ID with the same
signed identity converges on the committed intent; the same operation ID with
a changed identity is rejected, and only one active operation may exist per
node. If privd loses its effect journal between the intent commit and the
journal completion, the durable intent store remains the idempotency
authority for this command family.

The runner resolves packages only from the fixed local spool
`/var/lib/ocservia-upgrade/package-spool` — the operator or provisioning
pipeline places the signed release triple
(`ocservia-agent-<version>-linux-<arch>.tar.gz` plus `.sha256` and
`.sha256.sig`) there before issuing the upgrade. There is no URL fetch and no
caller-selected path. The runner requires the spool archive digest to equal the
signed intent digest, then re-verifies the package through the installed
`/usr/libexec/ocservia/ocservia-agent-verify` with the pinned trust anchors
`/etc/ocservia/release-signing.pub.pem` and
`/etc/ocservia/trusted-release-key.sha256` (DER SHA-256 fingerprint, provisioned
out of band exactly like the installation key). Only after the verified marker
matches the intent does it run the package's own `upgrade-agent.sh` lifecycle,
re-check the installed binaries against the verified package, restart
`ocservia-privd` and `ocservia-agent`, and write the terminal result. A crash at
any point converges on restart: a `running` operation whose installed binaries
already match the package skips the destructive lifecycle instead of repeating
it, and every refusal persists `failed` evidence before exiting non-zero.

Rollback interacts with the durable state explicitly:
`ocservia-agent-rollback` stops any `ocservia-upgrader@*.service` instance,
marks non-terminal operations `rolled_back` so a stale runner cannot re-apply
the rolled-back release, and restores or removes the upgrader binary, the
`ocservia-upgrader@.service` unit, and the installed verifier exactly as
recorded in the matched snapshot (`.previous` restored, `.absent` removed). The
same three files are installed by `install-agent.sh`, carried in every signed
package, and removed by `uninstall-agent.sh`, so all six native package formats
ship the durable runner.

Command protocol `1.1` is fail closed: provision the Controller command
verification public key before upgrading the Agent and privd pair. Both
services load it independently, and privd also pins `NODE_ID`. New binaries
reject unsigned legacy mutations at both the command journal and root-effect
boundaries, so schedule rollout and Controller signing-key enablement as one
maintenance window. Keep both old and new public keys pinned during a
signing-key rotation until old authorizations have expired and all Unknown
outcomes have been reconciled.

## Controller-driven single-node upgrades

The console exposes one reconciled upgrade per node:
`POST /api/v1/nodes/{node_id}/agent-upgrade` (RBAC action `agent.upgrade`,
Operator role) accepts only a target version, an approval ID, and a reason.
Callers never supply a URL, path, or package digest. The Controller resolves
the digest from its operator-provisioned trusted release manifest, configured
with `OCSERV_AGENT_RELEASE_MANIFEST` (default
`/etc/ocservia/agent-releases.json`):

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

The manifest is the only digest source for this workflow. It must contain
between 1 and 512 unique `(version, architecture)` releases; a missing,
unreadable, malformed, or ambiguous file fails Controller startup. There is no
GitHub or registry synchronization: publishing a release means placing the
signed package triple in each node's local spool and adding the exact digest
to this file. The API additionally rejects a target that is not newer than the
node's observed agent version, and the request must carry the node's current
revision (`If-Match`) plus an independent approval bound to the exact
`node + version + digest + architecture` release identity.

The Controller only schedules upgrades from nodes that advertise the
fence-capable `ocserv.agent.upgrade.v2` capability. The upgrade is executed
by the runner that is already installed on the node, so the first
N → N+1 hop can only be protected when that runner carries the
execution-time downgrade fence and the installation commit record. Nodes
still advertising `ocserv.agent.upgrade.v1` are ineligible: seed the first
fence-capable package manually (or with the native installers), approve its
`v2` capability, and Controller-driven upgrades start from there. Upgrading
an existing 0.1.x installation with the native package manager (`dpkg -i`,
`rpm -Uvh`) preserves node identity, configuration, and durable state, and
leaves services stopped rather than auto-enabled. The release pipeline
exercises the published v0.1.1 DEB → candidate hop on both amd64 and arm64;
RPM package-manager upgrade semantics are covered by the native
fabricated-version lifecycle smoke, and a published historical RPM baseline
leg is not yet implemented.

The operation is created `queued`, and the agent's scheduling acknowledgement
moves it to the non-terminal `accepted` state — an acknowledged schedule is
never success. The node is then expected to disconnect while the upgrader
restarts the Agent and privd; a disconnect during this window is normal
progress, never a failure. Terminal outcomes are decided only from durable
Controller-side evidence: success additionally requires the node to be back
online with a fresh observation of the target agent version together with the
upgrader's terminal local result. An explicit local failure closes the
operation as `failed`, a matched rollback as `rolled_back`, and an operation
that is still unresolved after the bounded reconciliation window
(`OCSERV_AGENT_UPGRADE_RECONCILE_TIMEOUT`, default 30 minutes, accepted
range 1 minute to 24 hours at startup) closes conservatively as `unknown`
with its command marked expired so nothing is retried blind. Only one upgrade
may be active per node; a second attempt fails with a conflict until the
previous operation is terminal. Every terminal outcome appends an audit
record covering the operator, approval identity, from/to versions, and
outcome.

The Agent reports the upgrader's terminal local outcomes read-only through a
fixed privd query surfaced in its regular telemetry; the Controller accepts
the first report per operation and cross-checks it against the node's
observed version before concluding success. Nodes still running a pre-upgrade
privd simply report no outcomes; their operations still resolve through the
version observation, the failure and rollback paths, or the conservative
`unknown` deadline.

## Fleet rolling upgrades (rollouts)

`POST /api/v1/agent-rollouts` (RBAC action `agent.upgrade`, Operator role)
upgrades a selected set of nodes to one trusted target version in bounded
batches. The rollout creation request carries the target version, node IDs, an
optional batch size, reason, and approval ID. `batch_size` defaults to 5 and is
bounded from 1 through 20. It does not accept `stop_on_failure`.

The referenced `agent.rollout` approval binding additionally includes the
target version, sorted node set, batch size, and `stop_on_failure`. The current
server contract requires that policy to be true; rollout creation fixes
`StopOnFailure = true` and does not expose it as a caller-selectable field. The
target must exist in the trusted release manifest for every selected node's
architecture, and every selected node must advertise the
`ocserv.agent.upgrade.v2` capability.

The console's fleet version badges and the recommended version shown in
Settings are driven by the operator-pinned `OCSERV_RECOMMENDED_AGENT_VERSION`
(SemVer); it classifies observed versions but never schedules anything by
itself.

Batch 0 is a mandatory single-node canary. Later batches start only after the
canary reaches `succeeded`; a failed or skipped canary pauses the rollout, and
resuming requeues that canary for a fresh attempt — no operator decision can
replace the mandatory successful canary.

Each node follows the reconciled single-node upgrade lifecycle above under
its own stable operation ID; a succeeding node is never redispatched. A batch
advances only when every node in it has a terminal outcome, and any failed,
unknown, or rolled-back node pauses the rollout. A node that became
ineligible when its batch was dispatched (offline, stale observation,
capability withdrawn, version already current or ahead, another upgrade
active, or no manifest entry) is marked `skipped` with a reason code and the
rollout pauses. Resuming requeues the failed, unknown, rolled-back, and
skipped-canary nodes of the current batch for a fresh eligibility check; a
skipped non-canary node stays skipped and keeps its reason. The rollout
detail view therefore reports succeeded, failed, and skipped nodes per batch
plus the remaining count: a rollout that finished `succeeded` does not imply
every selected node was upgraded.

Rollout state is durable: it survives Controller restarts and console
sessions unchanged. Reading a rollout requires `operation.read`; creating and
resuming require `agent.upgrade`; a rollout is visible and resumable only
inside its workspace.
