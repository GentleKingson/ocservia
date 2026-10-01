# Agent package lifecycle

> **Technical reference.** For the operator path, start with [Install a managed
> node](../getting-started/managed-node.md), [Upgrade the Agent](../how-to/agent-upgrade.md),
> or [Roll back the Agent](../how-to/agent-rollback.md). This document retains
> package construction, verified staging, trust provisioning, and durable
> lifecycle contracts.

The published v1.1.0 [policy reset](../reference/support-policy.md) removes source
version windows and target ordering, not artifact integrity or current trust
requirements. Cross-version safety is not guaranteed. Historical layout/key
conversion is no longer automatic; provision the target explicitly or redeploy
without overwriting retained identity and persistent state.

After its external endpoint is deployed and verified, the operator-hosted thin
first-install chain is deliberately split:

```text
Stage-0 -> exact vX.Y.Z Stage-1 -> HTTPS native package
        -> identity/sealing -> Bootstrap Enrollment -> ENROLLED_LOCAL
        -> independent approval -> service activation
```

Stage-0 and initial Stage-1/native package downloads rely on HTTPS. The
installer freezes the downloaded native package in root-owned staging and
checks that its bytes remain unchanged before invoking the package manager.
This transport check does not authorize an AgentUpgrade: upgrades use the
SHA256 in the already-authorized Controller command. A Bootstrap Token can
create or recover only the same pending node; it cannot approve the node.
Package installation, enrollment, approval and service activation retain their
existing authority boundaries.
Until that hosting has operational ownership and byte-verification evidence,
the public Quick Start obtains the installer from a clean exact-release
checkout; the installed native package has no runtime dependency on Git.

Build the Agent and privd release binaries, then create a deterministic package:

```bash
OUTPUT_DIR=dist \
  VERSION=1.0.0 PACKAGE_ARCH=amd64 SOURCE_DATE_EPOCH=1786147200 scripts/package-agent.sh
# A plain checksum detects transfer corruption; it is not an authorization key.
EXPECTED_SHA256="$(awk '{print $1}' dist/ocservia-agent-1.0.0-linux-amd64.tar.gz.sha256)"
VERIFIED_PACKAGE="$(sudo scripts/verify-agent-package.sh \
  dist/ocservia-agent-1.0.0-linux-amd64.tar.gz "${EXPECTED_SHA256}")"
sudo INSTALL_PRODUCTION_RELAYS=true \
  "${VERIFIED_PACKAGE}/scripts/install-agent.sh"
```

The verifier freezes the archive in root-owned staging, compares its SHA256 with the supplied expected digest, and checks archive contents before extraction. Set `INSTALL_PRODUCTION_RELAYS=true` during installation and fill `/etc/ocservia-agent/relays.env` with required HTTPS `RELAY_URL_A`. Omit `RELAY_URL_B` or leave it empty; the managed-node installer rejects nonempty B before installation, and the production launcher rejects it before Agent execution. The lower-level package installer preserves operator configuration. Install the relay token at `/etc/ocservia-agent/relay-access-token` as `root:ocserv-agent` mode `0640`.

The production drop-in executes the packaged fixed launcher
`/usr/libexec/ocservia/ocservia-agent-relays`, which constructs exactly one Relay URL
argument and then replaces itself with the Agent. Base development units and
direct binary invocations are unchanged. Archive and native installations,
upgrades, rollback snapshots, and uninstall include this launcher; upgrades
preserve operator configuration and identity. Uninstall removes the launcher
but retains configuration and state unless the existing purge option is used.

The verifier copies only the archive into a unique `root:root` mode `0700` directory below
`/var/lib/ocservia-upgrade/package-staging`. It compares the copied archive SHA256
with the caller-supplied expected digest, rejects unsafe archive paths and
member types, and extracts that same root-owned archive. Install and upgrade
scripts accept only the verified directory printed by the verifier. Do not
extract or run installers from a download directory, and remove the verified
staging directory after the lifecycle operation succeeds.

`PACKAGE_ARCH` pins the package architecture (`amd64` or `arm64`); the MANIFEST
records it as `arch=`. On a real host install (no `DESTDIR`) the verifier also
rejects a foreign-architecture package — `x86_64` ↔ `amd64`, `aarch64` ↔
`arm64` — before anything is staged.

## Native installer packages

Each release publishes the archive and its plain `.sha256`, plus native DEB
and RPM installers for both `amd64` and `arm64`. Controller assets include
`controller-release-{amd64,arm64}.json`, the amd64 `controller-release.json`
alias, and the versioned `controller-bootstrap.sh` and
`managed-node-bootstrap.sh` entrypoints. The JSON files are deployment
configuration; their version image tags select the published GHCR images.

The `.deb` and `.rpm` embed only the archive, its plain checksum, and the
verifier under `/usr/share/ocservia-agent`.
Their scriptlets contain no layout logic: `postinst` refuses a host-architecture
mismatch, verifies the archive into trusted staging, and runs the verified
`install-agent.sh` (fresh host) or `upgrade-agent.sh` (existing installation).
Installing or upgrading never enables or starts a service — provision
`/etc/ocservia-agent/agent.env` and the keys first, then enable both units
manually. Package removal runs the verified `uninstall-agent.sh`, preserving
identity, state, and configuration by default.

A production managed node requests the relay contract before invoking the
package manager by creating `/etc/ocservia/agent-install-production-relays`
as a regular file. `postinst` then passes `INSTALL_PRODUCTION_RELAYS=true`
into the verified lifecycle, on a fresh install and on an upgrade, so the
production relay drop-in and `relays.env` always come from the verified
embedded payload — never from a source checkout. The request is one-shot: it
is consumed only after the verified lifecycle succeeds, so a failed install
or upgrade stays retryable; a real package removal (not an upgrade) retires
an unconsumed request, so it cannot surprise a later plain install. The
installed drop-in carries the standing production intent, and later upgrades
reinstall it whenever the drop-in is already present, with or without the
marker. Without the marker a native install stays relay-free, exactly as
before.

Package-manager downgrade is not a supported rollback path: it would skip the
matched snapshot contract. Roll back only with
`sudo /usr/libexec/ocservia/ocservia-agent-rollback`, then install the fixed
release.

Stage-0 is not a long-term lifecycle manager. A managed-node upgrade continues
through a verified package or the durable Controller-driven upgrader.
Native package removal continues through `dpkg` or `rpm`, invoking the verified
uninstall scriptlet and preserving identity, state, and configuration unless
the operator separately chooses the irreversible purge flow.

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
changing installed state. These are current trust requirements, not source
software version checks. The v1.1.0 package scripts do not automatically enroll
or rebind keys, reset node identity, or purge existing certificate artifacts
when applying a target. The former upgrade-time enrollment environment
variables and marker are no longer used. Provision the target's required
trust configuration explicitly; missing or invalid configuration still fails.
Removing historical layout checks does not make arbitrary cross-version
operations safe or give an installed binary capabilities it does not implement.
After the preflight, the script retains one matched snapshot of the previous
Agent and privd binaries, both base systemd units, and the production relay
drop-in and launcher presence and content under the root-only
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

## Journal storage monitoring

For an explicit local authority transfer, follow
[Rebind an Agent](../how-to/rebind-controller.md). The
[Controller rebind and retention contract](../development/controller-rebind.md)
defines its authority-transfer and cleanup boundaries.

The command journal has no automatic retention policy. Monitor it independently
of Agent connectivity; normal authorized commands also accumulate durable state.
From a reviewed checkout, run this read-only Linux probe as `ocserv-agent` (or
another account allowed to stat the state directory):

```bash
sudo -u ocserv-agent bash scripts/check-agent-storage.sh /var/lib/ocservia-agent/agent.db
```

Use the actual path when `--journal` overrides the default. The probe reports
the database, WAL and SHM logical sizes, their combined allocated bytes, and
filesystem available bytes/inodes. It does not open SQLite, checkpoint, truncate,
or delete anything. Missing or unreadable state is UNKNOWN, not healthy.

Register this command in the node's existing monitoring scheduler at a 15-minute
interval, with a 30-second execution timeout. Exit codes are 0 OK, 1 WARNING,
2 CRITICAL, and 3 UNKNOWN; alert on 1/2/3, timeout, or two missed samples. Default
warning/critical thresholds are 80%/90% for either disk or inode usage, adjustable
with `STORAGE_WARNING_PERCENT` and `STORAGE_CRITICAL_PERCENT`. They are initial
operational thresholds, not measured capacity guarantees. The repository does
not install a scheduler or notification destination automatically.

Retain samples in the existing monitoring backend. Track `journal_bytes` and
`journal_allocated_bytes` growth, and forecast exhaustion from declining
`filesystem_available_bytes`; alert when projected runway is below seven days,
even before percentage thresholds fire. Record a baseline after deployment and
confirm that a synthetic warning reaches the on-call destination:

```bash
sudo -u ocserv-agent env STORAGE_WARNING_PERCENT=1 STORAGE_CRITICAL_PERCENT=99 \
  bash scripts/check-agent-storage.sh /var/lib/ocservia-agent/agent.db
```

On warning, inspect growth and unrelated files on the same filesystem and plan
capacity. On critical, stop initiating new command dispatch/rollouts to the
affected node and provision storage before resuming. Preserve the database and
WAL together during controlled recovery. Never delete command identities, revision
fences, pending/unknown records, or WAL files to reclaim space. Do not add age-based
cleanup until the idempotency, signed replay and recovery retention boundaries
are explicitly defined.

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
(validated by the runner in the active binding's operations namespace).

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
pipeline places `ocservia-agent-<version>-linux-<arch>.tar.gz` there before
issuing the upgrade. There is no URL fetch and no caller-selected path. The
runner requires the spool archive digest to equal `package_sha256` in the
already-authorized command's durable intent. It passes that exact digest to
`/usr/libexec/ocservia/ocservia-agent-verify`, which freezes the archive in
root-owned staging, compares its SHA256 again, and safely extracts it. A digest
mismatch fails closed without running the lifecycle. Only after the verified marker
matches the intent does it run the package's own `upgrade-agent.sh` lifecycle,
re-check the installed binaries against the verified package, restart
`ocservia-privd` and `ocservia-agent`, and write the terminal result. A crash at
any point converges on restart: a `running` operation whose installation
commit record and installed binaries match the package skips the destructive lifecycle instead of repeating
it, and every refusal persists `failed` evidence before exiting non-zero.

Rollback interacts with the durable state explicitly:
`ocservia-agent-rollback` stops any `ocservia-upgrader@*.service` instance,
marks non-terminal operations `rolled_back` so a stale runner cannot re-apply
the rolled-back release, and restores or removes the upgrader binary, the
`ocservia-upgrader@.service` unit, and the installed verifier exactly as
recorded in the matched snapshot (`.previous` restored, `.absent` removed). The
same three files are installed by `install-agent.sh`, carried in every
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
GitHub or registry synchronization: preparing an upgrade means placing the
archive in each node's local spool and adding its exact digest
to this file. Under the v1.1.0 policy reset, an explicit trusted target is
not rejected because it is lower, equal, higher or the source version is unknown.
The displayed version classification is informational, not execution authority.
The request must still carry the node's current
revision (`If-Match`) plus an independent approval bound to the exact
`node + version + digest + architecture` release identity.

The Controller requires a real approved typed-command capability, either
`ocserv.agent.upgrade.v2` or `ocserv.agent.upgrade.v1`. It selects an actual
approved name and binds that exact name into the signed command; v2 is
preferred when both are approved. A v1-only node is not excluded merely for
lacking the former software downgrade fence. No capability is fabricated,
and no historical payload conversion is performed.

The installed runner executes the command. Previously installed code retains
its behavior; accepting a command capability is not a promise of cross-version
safety. Current-candidate validation does not certify historical combinations.

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
architecture, and every selected node must advertise an approved
`ocserv.agent.upgrade.v1` or `ocserv.agent.upgrade.v2` capability.

The console's fleet version badges and the recommended version shown in
Settings are driven by the operator-pinned `OCSERV_RECOMMENDED_AGENT_VERSION`
(SemVer); it classifies observed versions but never schedules anything by
itself. Set it in the Controller section of `install.env` or export it in
the invoking shell before the production installer/bootstrap. The resolved
value is forwarded through the root lifecycle to the migrate and control-plane
containers. An explicitly empty export overrides the file and clears the
recommendation. Recreate the control-plane through the documented Controller
upgrade procedure after changing this setting on an existing installation
(see [production deployment](production-deployment.md)). Export the value in
that lifecycle invocation too: `controller.sh` does not load `install.env`.
Without a recommendation, observed versions remain visible and cannot be
compared to a target. Invalid nonempty SemVer values are rejected by Controller
configuration validation.

Batch 0 is a mandatory single-node canary. Later batches start only after the
canary reaches `succeeded`; a failed or skipped canary pauses the rollout, and
resuming requeues that canary for a fresh attempt — no operator decision can
replace the mandatory successful canary.

Each node follows the reconciled single-node upgrade lifecycle above under
its own stable operation ID; a succeeding node is never redispatched. A batch
advances only when every node in it has a terminal outcome, and any failed,
unknown, or rolled-back node pauses the rollout. A node that became
ineligible when its batch was dispatched (offline, stale observation,
capability withdrawn, another upgrade
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


## UTC and network-monitor diagnostics

Use UTC when correlating Agent, privd, Controller and browser observations:

```sh
journalctl --utc -u ocservia-agent -u ocservia-privd --since '2026-09-30 00:00:00 UTC'
systemctl cat ocservia-agent.service
systemctl show ocservia-agent.service -p FragmentPath -p DropInPaths -p RestrictAddressFamilies
```

Startup `service.version`, the Agent version CLI and heartbeat use the same
release version. Browser diagnostic timestamps show UTC and retain fractional
precision; extended-year and infinite values remain textual.

The default Agent unit permits AF_NETLINK for the transport network monitor.
When investigating a netlink initialization warning, inspect the effective unit
and drop-ins on the affected host, then verify address/route-change observation
in an isolated systemd node. A corrected repository default does not prove the
cause of a historical host warning. Keep the other sandbox settings intact;
Controller connectivity or a heartbeat alone does not prove the network monitor
is healthy.
