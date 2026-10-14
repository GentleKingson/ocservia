# Agent package lifecycle

Release version examples also accept an exact `vX.Y.Z-rc.N` candidate tag
(positive N without leading zeros). RCs are not recommended stable releases.

> For day-to-day operator procedures, start with [Install a managed
> node](../getting-started/managed-node.md), [Upgrade the Agent](../how-to/agent-lifecycle.md#upgrade),
> or [Roll back the Agent](../how-to/agent-lifecycle.md#rollback). The sections below
> cover package construction, verified staging, trust provisioning, and durable
> lifecycle contracts.

The published v1.1.0 [policy reset](../reference/support-policy.md) drops source
version windows and target ordering while keeping artifact integrity and current
trust requirements intact. Because cross-version safety is not guaranteed and
historical layout or key conversion no longer runs automatically, operators must
provision the target explicitly or redeploy without overwriting retained
identity and persistent state.

Once its external endpoint is deployed and verified, the operator-hosted thin
first-install chain separates each stage:

```text
Stage-0 -> exact vX.Y.Z Stage-1 -> HTTPS native package
        -> identity/sealing -> Bootstrap Enrollment -> ENROLLED_LOCAL
        -> independent approval -> service activation
```

Stage-0 and the initial Stage-1 and native package downloads rely on HTTPS.
Before invoking the package manager, the installer copies the downloaded native
package into root-owned staging and verifies that its bytes remain unchanged.
That transport check applies only to initial installation; an AgentUpgrade
instead verifies against the SHA256 carried in the already-authorized Controller
command. Likewise, a Bootstrap Token can create or recover only the same pending
node and cannot approve it, keeping package installation, enrollment, approval,
and service activation across separate authority boundaries. Until external
hosting has operational ownership and byte-verification evidence, the public
first-install guide obtains the installer from a clean exact-release checkout.
Once installed, the native package has no runtime dependency on Git.

Build all three node binaries natively on the package's target architecture,
then create a deterministic package. Formal Releases build inside the isolated
Rocky Linux builder via `scripts/build-release-agent.sh` in the Release workflow
after [Release Check](../development/release-checks.md) passes. For a local
development build on a native Linux host, set one explicit SemVer `VERSION` so
the compiled Rust binaries (`OCSERV_AGENT_RELEASE_VERSION`), `package-agent.sh`
(`VERSION`, checked against each binary's `--version` output), the archive name,
and the SHA-256 verification input all match:

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

The verifier copies only the archive into a unique `root:root` mode `0700`
directory below `/var/lib/ocservia-upgrade/package-staging`, compares the copied
archive SHA256 with the caller-supplied expected digest, rejects unsafe archive
paths and member types, and extracts that root-owned copy. Install and upgrade
scripts accept only the verified directory printed by the verifier; never
extract or run installers from a download directory, and remove the verified
staging directory after the lifecycle operation succeeds. `PACKAGE_ARCH` pins
the package architecture (`amd64` or `arm64`), which the MANIFEST records as
`arch=`. On a real host install (no `DESTDIR`), the verifier also checks host
architecture equivalence (`x86_64` for `amd64`, `aarch64` for `arm64`) and
rejects a foreign-architecture package before anything is staged.

To configure production relays during installation, set
`INSTALL_PRODUCTION_RELAYS=true` and populate `/etc/ocservia-agent/relays.env`
with the required HTTPS `RELAY_URL_A`. Omit `RELAY_URL_B` or leave it empty; the
managed-node installer rejects a nonempty B before installation, and the
production launcher rejects it before Agent execution. Install the relay token
at `/etc/ocservia-agent/relay-access-token` as `root:ocserv-agent` mode `0640`.
The production drop-in executes the packaged fixed launcher
`/usr/libexec/ocservia/ocservia-agent-relays`, which constructs a single Relay
URL argument and replaces itself with the Agent, leaving base development units
and direct binary invocations unchanged. Archive and native installations,
upgrades, rollback snapshots, and uninstall all include this launcher. The
lower-level package installer and upgrades preserve operator configuration and
identity, whereas uninstall removes the launcher while retaining configuration
and state unless the existing purge option is used.

## Manual trust provisioning

For the manual archive path, provision trust before service activation; the
[managed-node installer](../getting-started/managed-node.md) owns package-first
preparation. The following commands install existing key material that was
provisioned independently (do not use generated example keys).

Before enabling the units, install the independently provisioned Controller
command verification key and two distinct RSA private keys for user-password
and P12-password unsealing. Edit `/etc/ocservia-agent/agent.env` with the
Controller key path, controller EndpointID, node UUID, distinct sealing key
IDs, and the lowercase SHA-256 of each public key's DER encoding:

```bash
sudo install -o root -g ocserv-agent -m 0640 \
  controller-command-verification-key.pem \
  /etc/ocservia-agent/controller-command-verification-key.pem
sudo install -o root -g root -m 0600 user-password-seal-private.pem \
  /etc/ocservia-agent/user-password-seal-private.pem
sudo install -o root -g root -m 0600 p12-password-seal-private.pem \
  /etc/ocservia-agent/p12-password-seal-private.pem
```

The [Agent/privd boundary](../development/agent-privd.md#provisioned-trust)
defines startup verification and key separation. Finish
[enrollment and independent approval](../how-to/enroll-node.md) before enabling
the services.

## Native installer packages

Build native packages from the archive and checksum created above using the
same `VERSION`:

```bash
OUTPUT_DIR=dist VERSION="${VERSION}" PACKAGE_ARCH=amd64 \
  SOURCE_DATE_EPOCH=1786147200 scripts/package-native-agent.sh
```

DEB and RPM use package release `1`; architecture names are `amd64`/`arm64`
for DEB and `x86_64`/`aarch64` for RPM. Published historical asset names remain
unchanged. See [release policy](../development/release-checks.md) for RC naming
and native package validation.

Each release publishes the archive and its plain `.sha256`, plus native DEB
and RPM installers for both `amd64` and `arm64`. Controller assets include
`controller-release-{amd64,arm64}.json`, the amd64 `controller-release.json`
alias, and the versioned `controller-bootstrap.sh` and
`managed-node-bootstrap.sh` entrypoints. The JSON files carry deployment
configuration whose version image tags select the published GHCR images.

The `.deb` and `.rpm` packages embed only the archive, its plain checksum, and
the verifier under `/usr/share/ocservia-agent`, and depend on `openssl`,
`python3`, and `util-linux`. Their scriptlets contain no layout logic:
`postinst` refuses a host-architecture mismatch, verifies the archive into
trusted staging, and runs the verified `install-agent.sh` on a fresh host or
`upgrade-agent.sh` on an existing installation. Installing or upgrading a
package never enables or starts the Agent or privd units; operators must first
provision `/etc/ocservia-agent/agent.env` and the keys, then enable both units
manually. The independent history-retention timer is the only exception:
installation enables it and starts it unless an install is still unfinished. An
upgrade or an identical-package retry runs `systemctl try-restart` on
`ocservia-privd` and `ocservia-agent`, restarting units that are already
running while leaving stopped units stopped. Package removal (`dpkg -r`,
`rpm -e`) runs the verified `uninstall-agent.sh`, which disables and stops the
Agent, privd, and retention timer and removes the binaries and units while
preserving identity, state, and configuration. Neither `dpkg --purge` nor the
package manager triggers the state purge; only `uninstall-agent.sh --purge-state`
does so, as described below.

A production managed node requests the relay contract before invoking the
package manager by creating `/etc/ocservia/agent-install-production-relays`
as a regular file. `postinst` then passes `INSTALL_PRODUCTION_RELAYS=true`
into the verified lifecycle on both fresh installs and upgrades, sourcing the
production relay drop-in and `relays.env` exclusively from the verified
embedded payload (never from a source checkout). This marker file is consumed
only after the verified lifecycle succeeds, keeping a failed install or upgrade
retryable. A real package removal (as opposed to an upgrade) deletes any
unconsumed request so it cannot affect a later plain install. Once installed,
the drop-in records the standing production intent, and subsequent upgrades
reinstall it whenever the drop-in is already present, with or without the
marker. Without the marker or an existing drop-in, a native install stays
relay-free.

Package-manager downgrade is unsupported as a rollback path because it skips
the matched snapshot contract. Always roll back with
`sudo /usr/libexec/ocservia/ocservia-agent-rollback` before installing the
fixed release. Stage-0 also plays no role in ongoing lifecycle operations:
managed-node upgrades proceed through a verified package or the durable
Controller-driven upgrader, and native package removal runs through `dpkg` or
`rpm`.

Before changing backups, binaries, systemd units, or services,
`upgrade-agent.sh` runs a strict preflight against the existing trust
configuration. It verifies that `agent.env` contains a single absolute
`CONTROLLER_COMMAND_VERIFICATION_KEY_FILE` and that the referenced Ed25519
public key satisfies the combined Agent and privd requirements for file type,
ownership, mode, link count, symlinks, size, and directory ancestry. The shared
key must be independently provisioned as `root:ocserv-agent` mode `0440` or
`0640` beneath root-controlled ancestry; an Agent-owned legacy key is rejected
and never promoted into a root trust anchor. The preflight also requires two
distinct enrolled sealing key IDs and public-key fingerprints, derives each
fingerprint from its root-owned private key, and rejects missing, reused,
mismatched, symlinked, or unsafe key files. A legacy two-line `agent.env` or an
invalid key therefore halts the upgrade with provisioning instructions before
installing an Agent that would fail to restart. These checks enforce current
trust requirements regardless of the source software version. The v1.1.0
package scripts do not automatically enroll or rebind keys, reset node
identity, or purge existing certificate artifacts when applying a target, and
the former upgrade-time enrollment environment variables and marker are no
longer used. Operators must provision the target's required trust configuration
explicitly. Removing historical layout checks does not make arbitrary
cross-version operations safe or grant an installed binary capabilities it does
not implement.

Once the preflight passes, the script saves one matched snapshot under the
root-only `/var/lib/ocservia-upgrade/upgrade-backup` hierarchy covering the
previous Agent and privd binaries, both base systemd units, the durable
upgrader and verifier, the production relay drop-in and launcher, the rebind
helper, the retention executable, and the retention service and timer. Keeping
this directory outside privd's systemd-managed `StateDirectory` prevents
service startup from rewriting rollback evidence ownership. A root-owned
manifest binds the exact snapshot digests, and rollback rejects unsafe
ancestry, symlinks, hard links, ownership, modes, or file replacement. Optional
files carry explicit `.previous` or `.absent` records that tell rollback
whether to restore or remove the installed file. Retention depends on its
matching rebind helper, the retention service depends on the retention
executable, and the timer depends on the service, whereas a rebind helper
without retention remains a valid layout. Both upgrade and rollback preserve
endpoint identity, the durable Agent database, the command journal, and
configuration. Verify service health and Controller connectivity after an
upgrade. To roll back the complete matched snapshot, run:

```bash
sudo /usr/libexec/ocservia/ocservia-agent-rollback
```

The rollback command validates the complete snapshot and target destinations
before stopping services. It stops retention before replacing the retention
executable and imported rebind helper, restores binaries and units together,
reloads systemd, and starts privd before the Agent. A restored retention timer
is restarted after the completed lifecycle releases its lock so persistent
timer catch-up runs against the restored files, whereas an explicitly absent
timer is disabled and removed. The standalone rollback driver itself remains on
disk so it can finish executing and re-read the snapshot if invoked again.
Restoring binaries without their matching units is unsupported because the CLI
and local wire contract can change together. Because a rollback also restores
the previous release's security properties, treat it as a temporary recovery
measure and move to a fixed release promptly.

By default, `uninstall-agent.sh` preserves node identity and the command
journal. Passing `--purge-state` is irreversible and should only be done after
revoking the node identity on the Controller and archiving required audit
material. It deletes the Agent and privd state directories,
`/var/lib/ocservia-upgrade` (including the rollback snapshot, preventing any
subsequent rollback), and `/etc/ocservia-agent` (`agent.env`, keys, and relay
token), and then removes the `ocserv-agent` user and group. Because purging
local state does not update the Controller, revoke the node record there
separately.

Current snapshots contain 13 fixed records. Older 8- and 9-record snapshots
remain readable with `ocservia-agent-rollback --verify-only`, but they lack
prior rebind and retention evidence, and 8-record snapshots also lack launcher
evidence. Because missing records do not prove that the prior files were absent
on disk, a mutating rollback refuses incomplete snapshots before stopping
services. Recovering from an incomplete snapshot requires either a complete
trusted matched snapshot or an independently reviewed manual restoration of the
missing artifacts; no force or skip flag is provided.

Before modifying package-owned files, the upgrade script persists the complete
snapshot, atomically publishes the verified standalone rollback driver, and
records the verified target digest in
`/var/lib/ocservia-upgrade/installing-package`. If an upgrade is interrupted, a
subsequent direct or native-package retry refuses to overwrite the existing
snapshot; run the verified rollback first to restore the host, then retry the
package operation. A matching completed retry clears a stale
`installing-package` record only after verifying the installation commit, every
package-owned file, and the rollback snapshot, while rollback clears the record
only after all restores and removals are synced across filesystems. Neither
path alters Controller binding authority or reverses database migrations. When
an identical retry completes, it also starts an enabled retention timer after
releasing the lifecycle lock, recovering service activation if a prior run was
interrupted after the installation commit (explicitly disabled or masked timers
are left untouched).

## Journal storage monitoring

For an explicit local authority transfer, follow
[Rebind an Agent](../how-to/rebind-controller.md). The
[Controller rebind and retention contract](../development/controller-rebind.md)
defines its authority-transfer and cleanup boundaries.

Because the command journal has no automatic retention policy and normal
authorized commands accumulate durable state over time, monitor journal storage
independently of Agent connectivity. From a reviewed checkout, run this
read-only Linux probe as `ocserv-agent` (or another account permitted to stat
the state directory):

```bash
sudo -u ocserv-agent bash scripts/check-agent-storage.sh /var/lib/ocservia-agent/agent.db
```

Pass the actual database path when `--journal` overrides the default. When an
active Controller binding exists (for example after a rebind), the Agent writes
its journal to `/var/lib/ocservia-agent/bindings/<node-id>/agent.db` instead of
the default path, so monitor that file. The probe reports the logical sizes of
the database, WAL, and SHM files, their combined allocated bytes, and available
filesystem bytes and inodes without opening SQLite or checkpointing,
truncating, or deleting any state. If the state files are missing or
unreadable, the probe reports UNKNOWN (exit code 3).

Schedule this probe in the node's monitoring agent every 15 minutes with a
30-second execution timeout. Exit codes are 0 OK, 1 WARNING, 2 CRITICAL, and
3 UNKNOWN; configure alerts on exit codes 1, 2, or 3, on timeouts, or after two
missed samples. The default warning and critical thresholds are 80% and 90% for
both disk and inode usage, and can be tuned with `STORAGE_WARNING_PERCENT` and
`STORAGE_CRITICAL_PERCENT`. These defaults serve as starting operational
thresholds and do not guarantee capacity. The repository does not install a
scheduler or notification destination automatically.

Retain the collected metrics in your monitoring backend to track
`journal_bytes` and `journal_allocated_bytes` growth and forecast exhaustion
from declining `filesystem_available_bytes`. Alert whenever projected runway
drops below seven days, even if percentage thresholds have not yet fired.
Record a baseline after deployment and verify that a synthetic warning reaches
the on-call destination:

```bash
sudo -u ocserv-agent env STORAGE_WARNING_PERCENT=1 STORAGE_CRITICAL_PERCENT=99 \
  bash scripts/check-agent-storage.sh /var/lib/ocservia-agent/agent.db
```

When a warning fires, inspect journal growth alongside unrelated files on the
same filesystem and plan capacity expansion. On a critical alert, stop
initiating new command dispatches or rollouts to the affected node and expand
storage before resuming operations. Always keep the database and WAL files
together during controlled recovery; never delete command identities, revision
fences, pending or unknown records, or WAL files to reclaim space, and do not
introduce age-based cleanup until idempotency, signed replay, and recovery
retention boundaries are explicitly defined.

## Durable self-upgrade runner

Controller-driven upgrades run outside the Agent and privd processes. When
privd accepts a Controller-signed `AgentUpgrade` command, it independently
re-validates the typed release identity (version, SHA-256 digest, and
architecture), commits an immutable root-owned intent under
`/var/lib/ocservia-upgrade/operations/<operation-id>/`, and starts the
on-demand unit `ocservia-upgrader@<operation-id>.service`. Because the unit
uses `Type=exec`, privd's handoff returns as soon as the runner binary starts,
so the upgrade never terminates the process responsible for reporting the
command result. The unit omits an `[Install]` section because only privd starts
it, and the runner refuses to execute unless a committed intent exists in the
active binding's operations namespace.

Each operation directory contains three fixed `root:root` mode `0600` records
written atomically via write, fsync, and rename: `intent` (holding the schema
version, operation and command IDs, target version, package digest,
architecture, and semantic payload hash, all immutable once committed), `state`
(one of `accepted`, `running`, `succeeded`, `failed`, or `rolled_back`), and
`result` (terminal evidence written upon completion). Replaying the same
operation ID with an identical signed identity converges on the committed
intent, whereas reusing an operation ID with a different identity is rejected.
Only one active operation may exist per node. If privd loses its effect journal
between committing the intent and recording journal completion, the durable
intent store serves as the idempotency authority for this command family.

The runner loads packages exclusively from the fixed local spool directory
`/var/lib/ocservia-upgrade/package-spool`, without performing URL fetches or
accepting caller-supplied paths. Before issuing the upgrade command, the
operator or provisioning pipeline must place
`ocservia-agent-<version>-linux-<arch>.tar.gz` in that spool as a non-empty
regular file (no symlinks or hard links, within the runner's size limit) owned
by root and not writable by group or world; otherwise the upgrade fails with
unsafe spool metadata. The runner verifies that the spool archive digest equals
`package_sha256` in the durable intent and passes that exact digest to
`/usr/libexec/ocservia/ocservia-agent-verify`, which copies the archive into
root-owned staging, re-checks its SHA256, and extracts it safely. Any digest
mismatch fails closed before the lifecycle runs. Once the verified marker
matches the intent, the runner executes the package's `upgrade-agent.sh`
script, re-verifies the installed binaries against the verified package,
restarts `ocservia-privd` and `ocservia-agent`, and writes the terminal result.
If the runner crashes or restarts mid-flight, a `running` operation whose
installation commit record and installed binaries already match the package
skips repeating the destructive lifecycle, and any validation failure records
`failed` evidence before exiting non-zero.

Rollback coordinates directly with this durable state:
`ocservia-agent-rollback` stops any running `ocservia-upgrader@*.service`
instance, marks non-terminal operations `rolled_back` so a stale runner cannot
re-apply the rolled-back release, and restores or removes the upgrader binary,
the `ocservia-upgrader@.service` unit, and the installed verifier according to
the matched snapshot (`.previous` restored, `.absent` removed). Because
`install-agent.sh` installs those three files, every package carries them, and
`uninstall-agent.sh` removes them, all native DEB and RPM packages (`amd64` and
`arm64`) include the durable runner.

Command protocol `1.1` fails closed: provision the Controller command
verification public key before upgrading the Agent and privd pair. Both
services load the key independently, and privd also pins `NODE_ID`. Because new
binaries reject unsigned legacy mutations at both the command journal and
root-effect boundaries, schedule the binary rollout and Controller signing-key
enablement within a single maintenance window. During a signing-key rotation,
keep both the old and new public keys pinned until old authorizations expire
and all Unknown outcomes are reconciled.

## Controller-driven single-node upgrades

The console exposes one reconciled upgrade endpoint per node:
`POST /api/v1/nodes/{node_id}/agent-upgrade` (RBAC action `agent.upgrade`,
Operator role), which accepts only a target version, an approval ID, and a
reason alongside a required `Idempotency-Key` header. Callers never supply a
URL, file path, or package digest; the Controller looks up the digest in the
operator-provisioned release catalog pointed to by
`OCSERV_AGENT_RELEASE_MANIFEST` (an absolute path with no default). When that
environment variable is unset, the Controller starts without a catalog and
returns `503` on the agent-upgrade and agent-rollout endpoints. Because the
shipped production Compose neither sets nor mounts
`OCSERV_AGENT_RELEASE_MANIFEST`, enabling Controller-driven AgentUpgrade and
rollouts requires a deployment-specific override that mounts the catalog file
read-only into the control-plane container:

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

This manifest serves as the sole digest source for the workflow and must list
between 1 and 512 unique `(version, architecture)` entries; once
`OCSERV_AGENT_RELEASE_MANIFEST` is set, a missing, unreadable, malformed, or
ambiguous file aborts Controller startup. The Controller does not synchronize
with GitHub or container registries, so preparing an upgrade consists of
placing the archive in each node's local spool and adding its exact digest to
the manifest. Under the v1.1.0 policy reset, the Controller accepts any
explicit trusted target regardless of whether it is lower, equal, or higher
than the current version or whether the source version is unknown; version
classifications shown in the UI are informational and do not govern execution.
Each request must still supply the node's current revision (`If-Match`) and an
independent approval bound to the exact `node + version + digest + architecture`
release identity.

To dispatch the upgrade, the Controller requires an approved typed-command
capability on the node (`ocserv.agent.upgrade.v2` or `ocserv.agent.upgrade.v1`,
preferring v2 when both are approved) and binds that exact capability name into
the signed command. Nodes advertising only v1 remain eligible despite lacking
the former software downgrade fence; the Controller never fabricates
capabilities or converts historical payloads. Because the already-installed
runner executes the command, previously installed code retains its own runtime
behavior, and advertising a command capability does not guarantee cross-version
safety or certify historical version combinations.

New upgrade operations start in the `queued` state and move to the non-terminal
`accepted` state when the Agent acknowledges scheduling (acknowledgement alone
never counts as completion). As the upgrader restarts the Agent and privd, the
node temporarily disconnects as part of normal progress. The Controller
determines terminal outcomes strictly from durable evidence: `succeeded`
requires the node to reconnect and report the target Agent version alongside a
successful local terminal result from the upgrader; an explicit local failure
closes the operation as `failed`; and a matched rollback closes it as
`rolled_back`. If an operation remains unresolved when the reconciliation
window expires (`OCSERV_AGENT_UPGRADE_RECONCILE_TIMEOUT`, defaulting to 30
minutes with an accepted startup range of 1 minute to 24 hours; not forwarded
by the shipped production Compose unless overridden), the Controller closes it
conservatively as `unknown` and marks its command expired to prevent blind
retries. Only one upgrade may be active per node at a time, so concurrent
requests fail with a conflict until the active operation reaches a terminal
state. Every terminal transition appends an audit record capturing the
operator, approval identity, source and target versions, and outcome.

During regular telemetry heartbeats, the Agent queries privd read-only for
terminal upgrader outcomes and forwards them to the Controller, which accepts
the first report per operation and cross-checks it against the node's observed
version before marking the upgrade `succeeded`. Nodes still running an older
privd omit these local outcome reports and resolve instead through version
observation, explicit failure or rollback paths, or the conservative `unknown`
timeout.

## Fleet rolling upgrades (rollouts)

`POST /api/v1/agent-rollouts` (RBAC action `agent.upgrade`, Operator role)
upgrades a selected set of nodes to a single trusted target version in bounded
batches. The creation request takes the target version, node IDs, an optional
`batch_size` (defaulting to 5, bounded from 1 to 20), a reason, and an approval
ID. Callers cannot set `stop_on_failure` in the request; the server fixes
`StopOnFailure = true`, and the referenced `agent.rollout` approval binding
covers the target version, sorted node set, batch size, and
`stop_on_failure = true`. Every selected node's architecture must have a
matching target entry in the operator-provisioned release catalog, and every
selected node must advertise an approved `ocserv.agent.upgrade.v1` or
`ocserv.agent.upgrade.v2` capability.

Fleet version badges in the console and the recommended version displayed in
Settings come from the operator-pinned `OCSERV_RECOMMENDED_AGENT_VERSION`
(SemVer), which classifies observed versions without scheduling any upgrades on
its own. Configure it in the Controller section of `install.env` or export it
in the invoking shell before running the production installer or bootstrap so
the root lifecycle forwards the resolved value to the migrate and control-plane
containers; an explicitly empty export overrides `install.env` and clears the
recommendation. When updating this setting on an existing installation,
recreate the control-plane through the documented Controller lifecycle (see
[production deployment](production-deployment.md)): use `controller.sh start`
to re-apply the current release's Compose project with the exported environment
(whereas `controller.sh upgrade` switches to a different release file), and
export the variable in that invocation as well because `controller.sh` does not
read `install.env`. Check Settings afterward to confirm the update took effect,
since a Compose recreate run without the export silently retains or clears the
prior value. When no recommendation is configured, observed versions remain
visible without target comparison; invalid nonempty SemVer strings fail
Controller configuration validation.

Batch 0 always runs as a mandatory single-node canary, and subsequent batches
start only after that canary reaches `succeeded`. If the canary fails or is
skipped, the rollout pauses; resuming it requeues the canary for a fresh
attempt, and no operator action can bypass a successful canary.

Within each batch, every node follows the reconciled single-node upgrade
lifecycle under its own stable operation ID, and nodes that reach `succeeded`
are never redispatched. A batch advances only after all of its nodes reach a
terminal outcome, pausing the rollout if any node finishes as `failed`,
`unknown`, or `rolled_back`. If a node is ineligible when its batch dispatches
(because it is offline, has a stale observation, withdrew its capability, has
another upgrade active, or lacks a manifest entry), the Controller marks it
`skipped` with a reason code and pauses the rollout. Resuming a paused rollout
requeues the current batch's `failed`, `unknown`, `rolled_back`, and
skipped-canary nodes for a fresh eligibility check, while skipped non-canary
nodes remain `skipped` with their recorded reason. Because the rollout detail
view tracks succeeded, failed, and skipped nodes per batch alongside the
remaining count, an overall rollout status of `succeeded` does not imply that
every originally selected node was upgraded.

Rollout state persists across Controller restarts and console sessions.
Viewing a rollout requires `operation.read`, creating or resuming one requires
`agent.upgrade`, and each rollout is visible and resumable only within its own
workspace.


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
in an isolated systemd node. A corrected repository default does not by itself
explain a historical warning on an existing host. Keep the remaining sandbox
settings intact, and note that Controller connectivity or heartbeat delivery
alone does not confirm that the local network monitor is healthy.
