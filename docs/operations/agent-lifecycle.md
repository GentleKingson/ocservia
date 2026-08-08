# Agent package lifecycle

Build the Agent and privd release binaries, then create a deterministic signed package:

```bash
OUTPUT_DIR=dist AGENT_SIGNING_KEY=/secure/release-ed25519.key \
  VERSION=1.0.0 SOURCE_DATE_EPOCH=1786147200 scripts/package-agent.sh
scripts/verify-agent-package.sh dist/ocservia-agent-1.0.0-linux-amd64.tar.gz \
  dist/ocservia-agent-1.0.0-linux-amd64.tar.gz.sha256 \
  dist/ocservia-agent-1.0.0-linux-amd64.tar.gz.sha256.sig \
  dist/ocservia-agent-1.0.0-linux-amd64.tar.gz.sha256.pub.pem
```

Distribute the verification public key separately from packages. Verify signature, checksum, and archive contents before extracting as root. Set `INSTALL_PRODUCTION_RELAYS=true` during installation and fill `/etc/ocservia-agent/relays.env` with both dedicated HTTPS relay URLs plus a protected token-file path.

`upgrade-agent.sh` retains the previous binaries and preserves endpoint identity, the durable Agent database, journal, and configuration. Verify service health and Controller connectivity after upgrade. To roll back, stop both units, restore the previous binaries, reload systemd, and start privd before Agent. `uninstall-agent.sh` preserves identity and journal by default; `--purge-state` is irreversible and is appropriate only after revoking the node identity and preserving required audit material.
