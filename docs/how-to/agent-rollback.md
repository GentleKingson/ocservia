# Roll back the Agent

Restore the complete matched Agent and `privd` snapshot created by the last
successful upgrade.

## Before you begin

- The node is in a controlled recovery window.
- The root-only upgrade snapshot at `/var/lib/ocservia-upgrade/upgrade-backup`
  is present and has not been modified.
- Any Controller operation affected by the outage has been reconciled.

## Command

```bash
sudo /usr/libexec/ocservia/ocservia-agent-rollback
```

This restores the Agent and `privd` binaries, their matching systemd units,
and the production relay drop-in state together. Do not restore only one
binary.

## Verify

```bash
systemctl status ocservia-privd.service ocservia-agent.service
```

Confirm the node reconnects with the expected previous version and that the
Controller does not dispatch work until its capabilities and trust state are
freshly observed.

## If it fails

Stop and preserve the snapshot and diagnostics. Do not replace the snapshot
with files from an arbitrary download directory. See [Agent lifecycle
reference](../operations/agent-lifecycle.md) for snapshot validation and
recovery semantics.
