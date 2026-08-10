# Agent package lifecycle

Build the Agent and privd release binaries, then create a deterministic signed package:

```bash
OUTPUT_DIR=dist AGENT_SIGNING_KEY=/secure/release-ed25519.key \
  VERSION=1.0.0 SOURCE_DATE_EPOCH=1786147200 scripts/package-agent.sh
AGENT_TRUSTED_KEY_SHA256=<pinned-public-key-der-sha256> \
  scripts/verify-agent-package.sh dist/ocservia-agent-1.0.0-linux-amd64.tar.gz \
  dist/ocservia-agent-1.0.0-linux-amd64.tar.gz.sha256 \
  dist/ocservia-agent-1.0.0-linux-amd64.tar.gz.sha256.sig \
  /etc/ocservia/release-signing.pub.pem
```

Provision the verification public key and its DER SHA-256 fingerprint through a separate trusted channel; never trust the `.pub.pem` published beside a package. Verify signature, trusted-key fingerprint, checksum, and archive contents before extracting as root. Set `INSTALL_PRODUCTION_RELAYS=true` during installation and fill `/etc/ocservia-agent/relays.env` with both dedicated HTTPS relay URLs. Install the relay token at `/etc/ocservia-agent/relay-access-token` as `root:ocserv-agent` mode `0640`.

`upgrade-agent.sh` first verifies that the existing `agent.env` contains exactly
one absolute `CONTROLLER_COMMAND_VERIFICATION_KEY_FILE` and that the referenced
Ed25519 public key satisfies the Agent's type, ownership, mode, link, symlink,
size, and ancestry requirements. This preflight happens before backups,
binaries, systemd units, or services are changed. A legacy two-line
`agent.env`, missing key, or unsafe key therefore stops the upgrade with
provisioning instructions instead of installing an Agent that cannot restart.
After the preflight, the script retains the previous binaries and preserves
endpoint identity, the durable Agent database, journal, and configuration.
Verify service health and Controller connectivity after upgrade. To roll back,
stop both units, restore the previous binaries, reload systemd, and start privd
before Agent. `uninstall-agent.sh` preserves identity and journal by default;
`--purge-state` is irreversible and is appropriate only after revoking the node
identity and preserving required audit material.

Command protocol `1.1` is fail closed: provision the Controller command
verification public key before upgrading the Agent. A new Agent rejects unsigned
legacy mutations, so schedule the Agent rollout and Controller signing-key
enablement as one maintenance window. Keep both old and new public keys pinned
during a signing-key rotation until old authorizations have expired and all
Unknown outcomes have been reconciled.
