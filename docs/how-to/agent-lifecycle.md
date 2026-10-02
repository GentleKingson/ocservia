# Agent lifecycle

## Upgrade

Apply an explicitly selected Agent package on a managed node. Native package
installation invokes the same verified repository lifecycle as the archive
path.

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

### Command

Use the same `RELEASE_DIR` and `AGENT_PACKAGE` values selected and downloaded above. The package-manager command must
install that exact verified path, not a different relative-path copy.

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
matched rollback snapshot.

### Verify

```bash
systemctl status ocservia-privd.service ocservia-agent.service
```

Confirm the node is online with a fresh target-version observation in the
Controller inventory.

### If it fails

The package lifecycle fails before modification when trust, architecture, or
the upgrade snapshot is unsafe. Fix the reported prerequisite and retry the
same package. If the new pair was installed and must be restored, use the
[Agent rollback](#rollback) command.

See [Agent lifecycle reference](../operations/agent-lifecycle.md) for verified
staging, explicit trust provisioning, and Controller-driven rollout behavior.

## Rollback

Restore the complete matched Agent and `privd` snapshot created by the last
successful upgrade.

### Before you begin

- The node is in a controlled recovery window.
- The root-only upgrade snapshot at `/var/lib/ocservia-upgrade/upgrade-backup`
  is present and has not been modified.
- Any Controller operation affected by the outage has been reconciled.

### Command

```bash
sudo /usr/libexec/ocservia/ocservia-agent-rollback
```

This restores the Agent and `privd` binaries, their matching systemd units,
and the production relay drop-in and launcher state together. Do not restore only one
binary.

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
Controller does not dispatch work until its capabilities and trust state are
freshly observed.

### If it fails

Stop and preserve the snapshot and diagnostics. Do not replace the snapshot
with files from an arbitrary download directory. See [Agent lifecycle
reference](../operations/agent-lifecycle.md) for snapshot validation and
recovery semantics.
