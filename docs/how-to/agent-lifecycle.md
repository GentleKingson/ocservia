# Agent lifecycle

## Upgrade

Apply an explicitly selected Agent package on a managed node that is already
installed. Native package installation invokes the same verified repository
lifecycle as the archive path. This procedure replaces the Agent and `privd`
binaries only; it does not enroll, approve or authorize the node, and it does
not restore anything a failed upgrade left behind (use [Rollback](#rollback)).

The v1.1.0 scripts do not impose a source-version window or target-version
ordering. This does not guarantee safe cross-version operation; installed old
scripts retain their behavior. Historical deployment conversion is not provided.
See the [support policy](../reference/support-policy.md).

### Before you begin

- The verified target package matches the host architecture.
- Follow the [native package trust contract](../operations/agent-lifecycle.md#native-installer-packages):
  download the architecture-specific native package over HTTPS from the
  selected GitHub Release. Native scriptlets verify their embedded archive's
  plain checksum in protected staging. Controller-driven AgentUpgrade instead
  verifies `package_sha256` in its already-authorized command.
- `/etc/ocservia-agent/agent.env` and its trust/sealing keys are valid.
- You have a current recovery window and can verify the node after restart.
  A running Agent and `privd` are restarted by the upgrade, which briefly
  interrupts the node's Relay session and any in-flight operation.
- No earlier install attempt on this node is unfinished. If an
  `installing-package` record exists under `/var/lib/ocservia-upgrade` from an
  interrupted install, a different package is refused until you run a verified
  [rollback](#rollback); retrying the identical verified package clears it.

### Command

`RELEASE_DIR` is the directory holding the architecture-specific native
package you downloaded and verified under the trust contract above, and
`AGENT_PACKAGE` is its file name. The package-manager command must install
that exact verified path, not a different relative-path copy.

On Debian or Ubuntu:

```bash
sudo dpkg -i "$RELEASE_DIR/$AGENT_PACKAGE"
```

On an RPM-based system:

```bash
sudo rpm -Uvh "$RELEASE_DIR/$AGENT_PACKAGE"
```

Do not use package-manager downgrade as rollback. The upgrade preflight checks
the existing trust configuration before replacing files and creates one
matched rollback snapshot. Each upgrade to a different package replaces the
previous snapshot, so only the last pre-upgrade state can be restored.
Reinstalling an identical verified package preserves the existing snapshot,
and restarts the running services.

### Verify

```bash
systemctl status ocservia-privd.service ocservia-agent.service
```

Confirm the node is online with a fresh target-version observation in the
Controller inventory. Service status alone is local evidence; it does not show
that the Controller trusts the node or sees fresh data.

### If it fails

The package lifecycle fails before modification when trust, architecture, or
the upgrade snapshot is unsafe. Fix the reported prerequisite and retry the
same package. If the new pair was installed and must be restored, use the
[Agent rollback](#rollback) command.

See [Agent lifecycle reference](../operations/agent-lifecycle.md) for verified
staging, explicit trust provisioning, and Controller-driven rollout behavior.

## Rollback

Restore the complete matched Agent and `privd` snapshot created by the last
upgrade. This changes only the installed Agent-side files; it does not touch
the Controller, its database, or node identity and enrollment state.

### Before you begin

- The node is in a controlled recovery window.
- The root-only upgrade snapshot at `/var/lib/ocservia-upgrade/upgrade-backup`
  is present and has not been modified.
- Any Controller operation affected by the outage has been reconciled.
  Rollback stops the Agent, `privd`, running upgrader units and the retention
  job, and marks every non-terminal (`accepted` or `running`) local upgrade
  operation `rolled_back` so it cannot be re-applied afterwards.
- The snapshot is complete (all 13 recorded entries). Older snapshots with
  fewer records are refused for restoration because the prior rebind and
  retention state is unknown.
- Optional: check the snapshot first without changing anything:
  `sudo /usr/libexec/ocservia/ocservia-agent-rollback --verify-only`.

### Command

```bash
sudo /usr/libexec/ocservia/ocservia-agent-rollback
```

This restores the Agent and `privd` binaries, their matching systemd units,
and the production relay drop-in and launcher state together. Do not restore only one
binary. After restoring, it starts `privd` and the Agent unconditionally, even
if they were not running before, and clears any pending `installing-package`
record.

### Target requirements

The v1.1.0 verifier and rollback script no longer infer a software version from
the absence of a Relay launcher or reject it on that basis. A launcher actually
referenced by a service unit must still exist, and snapshot integrity, trust
and configuration requirements still apply. Operator configuration, identity
and persistent state are preserved, not silently converted or reset.

The selected binary may fail with the existing configuration or data. Absence
of a version rejection is not a cross-version safety guarantee, and installed
old scripts retain their original behavior. Package-manager downgrade is not
the matched-snapshot recovery operation described here.

### Verify

```bash
systemctl status ocservia-privd.service ocservia-agent.service
```

Confirm the node reconnects with the expected previous version and that the
Controller shows fresh capabilities and trust state before you send it work.
Rollback does not by itself prove that the Controller accepts the node.

### If it fails

Stop and preserve the snapshot and diagnostics; do not rerun the upgrade or
the rollback repeatedly. Rollback validates the whole snapshot before it
changes anything; an error reported at that stage leaves the installed files as
they were, while a failure after services were stopped can leave the node down.
Do not replace the snapshot
with files from an arbitrary download directory. See [Agent lifecycle
reference](../operations/agent-lifecycle.md) for snapshot validation and
recovery semantics.
