# Upgrade the Agent

Apply an explicitly selected signed Agent package on a managed node. Native package
installation invokes the same verified repository lifecycle as the archive
path.

The v1.1.0 scripts do not impose a source-version window or target-version
ordering. This does not guarantee safe cross-version operation; installed old
scripts retain their behavior. Historical deployment conversion is not provided.
See the [support policy](../reference/support-policy.md).

## Before you begin

- The verified target package matches the host architecture.
- Follow the [native package trust contract](../operations/agent-lifecycle.md#native-installer-packages):
  verify the out-of-band release-key fingerprint, `SHA256SUMS.sig`, and the
  selected package checksum before invoking the package manager as root.
- `/etc/ocservia-agent/agent.env` and its trust/sealing keys are valid.
- You have a current recovery window and can verify the node after restart.

## Command

Use the same `RELEASE_DIR` and `AGENT_PACKAGE` values whose external release
signature and checksum were verified above. The package-manager command must
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

## Verify

```bash
systemctl status ocservia-privd.service ocservia-agent.service
```

Confirm the node is online with a fresh target-version observation in the
Controller inventory.

## If it fails

The package lifecycle fails before modification when trust, architecture, or
the upgrade snapshot is unsafe. Fix the reported prerequisite and retry the
same package. If the new pair was installed and must be restored, use the
[Agent rollback](agent-rollback.md) command.

See [Agent lifecycle reference](../operations/agent-lifecycle.md) for verified
staging, explicit trust provisioning, and Controller-driven rollout behavior.
