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
and the production relay drop-in and launcher state together. Do not restore only one
binary.

## Target requirements

The v1.1.0 verifier and rollback script no longer infer a software version from
the absence of a Relay launcher or reject it on that basis. A launcher actually
referenced by a service unit must still exist, and snapshot integrity, trust
and configuration requirements still apply. Operator configuration, identity
and persistent state are preserved, not silently converted or reset.

The selected binary may fail with the existing configuration or data. Absence
of a version rejection is not a cross-version safety guarantee, and installed
old scripts retain their original behavior. Package-manager downgrade is not
the matched-snapshot recovery operation described here.

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
