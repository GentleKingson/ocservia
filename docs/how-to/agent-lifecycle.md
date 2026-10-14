# Agent lifecycle

## Upgrade

Upgrade an existing managed node by installing an explicitly selected Agent
package. Native package installation runs the same verified repository
lifecycle as the archive installer and replaces only the Agent and `privd`
binaries. It does not enroll, approve, or authorize the node, and it cannot
recover state left behind by a failed upgrade (use [Rollback](#rollback) for
recovery).

Although the v1.1.0 scripts remove source-version windows and target-version
ordering checks, cross-version operation is still not guaranteed to be safe.
Previously installed older scripts retain their original checks, and the
lifecycle does not convert historical deployments; see the
[support policy](../reference/support-policy.md).

### Before you begin

- Confirm that the verified target package matches the host architecture.
- Obtain the architecture-specific native package over HTTPS from the selected
  GitHub Release in accordance with the
  [native package trust contract](../operations/agent-lifecycle.md#native-installer-packages).
  During installation, the native scriptlets verify the plain checksum of their
  embedded archive in protected staging, whereas a Controller-driven
  AgentUpgrade verifies `package_sha256` directly from its already-authorized
  command.
- Check that `/etc/ocservia-agent/agent.env` and its trust and sealing keys are
  valid.
- Schedule a current recovery window so you can verify the node after restart.
  Upgrading restarts both a running Agent and `privd`, which briefly interrupts
  the node's Relay session and any in-flight operation.
- Make sure no earlier install attempt on the node was left unfinished. When an
  interrupted install leaves an `installing-package` record under
  `/var/lib/ocservia-upgrade`, the lifecycle refuses any different package
  until you run a verified [rollback](#rollback), though retrying the exact
  same verified package clears the record.

### Command

Set `RELEASE_DIR` to the directory holding the architecture-specific native
package you downloaded and verified under the trust contract above, and
`AGENT_PACKAGE` to its file name. Pass that exact verified path to the package
manager; do not point it at another copy over a relative path.

On Debian or Ubuntu:

```bash
sudo dpkg -i "$RELEASE_DIR/$AGENT_PACKAGE"
```

On an RPM-based system:

```bash
sudo rpm -Uvh "$RELEASE_DIR/$AGENT_PACKAGE"
```

Before replacing any files, the upgrade preflight validates the existing trust
configuration and creates a single matched rollback snapshot. Upgrading to a
different package overwrites the previous snapshot, leaving only the most
recent pre-upgrade state available for restoration. Reinstalling an identical
verified package keeps the existing snapshot intact and restarts the running
services. Never use a package-manager downgrade as a rollback mechanism.

### Verify

```bash
systemctl status ocservia-privd.service ocservia-agent.service
```

Check the Controller inventory to confirm that the node is online with a fresh
target-version observation. Local `systemctl` status only proves the services
started on the host; you still need the Controller inventory to verify node
trust and fresh data.

### If it fails

If preflight validation detects an unsafe trust configuration, host
architecture, or upgrade snapshot, the package lifecycle aborts before
modifying the installation. Fix the reported prerequisite and retry the same
package. If the new binaries have already been installed and must be reverted,
run the [Agent rollback](#rollback) command.

Details on verified staging, explicit trust provisioning, and Controller-driven
rollout behavior are covered in the
[Agent lifecycle reference](../operations/agent-lifecycle.md).

## Rollback

Restore the complete matched Agent and `privd` snapshot captured during the
most recent upgrade. Rollback modifies only the installed Agent-side files on
the host, leaving the Controller, its database, and the node's identity and
enrollment state untouched.

### Before you begin

- Place the node in a controlled recovery window.
- Confirm that the root-only upgrade snapshot at
  `/var/lib/ocservia-upgrade/upgrade-backup` is present and unmodified.
- Reconcile any Controller operation affected by the outage. Running rollback
  stops the Agent, `privd`, active upgrader units, and the retention job, and
  marks every non-terminal (`accepted` or `running`) local upgrade operation
  `rolled_back` so it cannot be re-applied afterwards.
- Ensure the snapshot is complete with all 13 recorded entries. Snapshots from
  older releases with fewer entries are refused because they do not record
  prior rebind and retention state.
- If you want to validate the snapshot before changing anything on disk, run
  `sudo /usr/libexec/ocservia/ocservia-agent-rollback --verify-only`.

### Command

```bash
sudo /usr/libexec/ocservia/ocservia-agent-rollback
```

The rollback script restores the Agent and `privd` binaries, their matching
systemd units, and the production relay drop-in and launcher state as a single
set; never restore one binary on its own. Once the files are restored, the
script clears any pending `installing-package` record and starts `privd` and
the Agent unconditionally, even if those services were stopped before rollback
began.

### Target requirements

In v1.1.0, the verifier and rollback script do not infer a software version
from a missing Relay launcher or reject a snapshot on that basis alone. Any
launcher referenced by a service unit must still exist on disk, and all
snapshot integrity, trust, and configuration checks continue to apply.
Operator configuration, node identity, and persistent state are preserved
without silent conversion or reset.

Removing the version check does not guarantee that the restored binary will
work with the current configuration or on-disk data, and older scripts already
installed on the host keep their original behavior. Always use the rollback
script to restore the matched snapshot, as a package-manager downgrade does not
perform this recovery.

### Verify

```bash
systemctl status ocservia-privd.service ocservia-agent.service
```

Before sending new work to the node, confirm in the Controller that the node
has reconnected under the expected previous version and is reporting fresh
capabilities and trust state. Completing the local rollback does not by itself
prove that the Controller accepts the node.

### If it fails

If rollback fails, stop immediately and preserve both the snapshot and host
diagnostics instead of repeatedly rerunning the upgrade or rollback commands.
Because rollback validates the entire snapshot before changing any files, an
error reported during validation leaves the installed files untouched, whereas
a failure after services have been stopped can leave the node offline. Never
replace the snapshot with files from an arbitrary download directory. See the
[Agent lifecycle reference](../operations/agent-lifecycle.md) for snapshot
validation and recovery semantics.
