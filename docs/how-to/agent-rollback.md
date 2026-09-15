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

## Single-relay compatibility

The pre-change implementation (verified at v0.5.2) requires two custom relay
URLs. Before rolling back to it, restore two valid, distinct HTTPS URLs in
`/etc/ocservia-agent/relays.env` and restore both relay services. The current
rollback entry point rejects a single-relay configuration before stopping
services or modifying files when the snapshot predates the optional-relay
launcher. Eight-entry historical snapshots remain readable; new snapshots
also record the launcher's presence or absence. Operator configuration,
identity and trust material are preserved, not silently rewritten.

The current archive verifier similarly guards a legacy candidate on an
installed production node. This cannot intercept an already-installed
historical script, direct execution of old package scripts, or a direct
package-manager downgrade. Do not use those paths with a single-relay
configuration. A valid dual configuration is a prerequisite, not a waiver of
the existing snapshot, trust, sealing-key or release compatibility checks.

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
