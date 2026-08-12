# Agent package lifecycle

Build the Agent and privd release binaries, then create a deterministic signed package:

```bash
OUTPUT_DIR=dist AGENT_SIGNING_KEY=/secure/release-ed25519.key \
  VERSION=1.0.0 SOURCE_DATE_EPOCH=1786147200 scripts/package-agent.sh
VERIFIED_PACKAGE="$(sudo AGENT_TRUSTED_KEY_SHA256=<pinned-public-key-der-sha256> \
  scripts/verify-agent-package.sh dist/ocservia-agent-1.0.0-linux-amd64.tar.gz \
  dist/ocservia-agent-1.0.0-linux-amd64.tar.gz.sha256 \
  dist/ocservia-agent-1.0.0-linux-amd64.tar.gz.sha256.sig \
  /etc/ocservia/release-signing.pub.pem)"
sudo INSTALL_PRODUCTION_RELAYS=true \
  "${VERIFIED_PACKAGE}/scripts/install-agent.sh"
```

Provision the verification public key and its DER SHA-256 fingerprint through a separate trusted channel; never trust the `.pub.pem` published beside a package. Verify signature, trusted-key fingerprint, checksum, and archive contents before extracting as root. Set `INSTALL_PRODUCTION_RELAYS=true` during installation and fill `/etc/ocservia-agent/relays.env` with both dedicated HTTPS relay URLs. Install the relay token at `/etc/ocservia-agent/relay-access-token` as `root:ocserv-agent` mode `0640`.

The verifier copies the archive, signed checksum, signature, and pinned public
key into a unique `root:root` mode `0700` directory below
`/var/lib/ocservia-upgrade/package-staging`. It verifies the exact copied checksum,
computes the exact copied archive digest, rejects unsafe archive paths and
member types, and extracts that same root-owned archive. Install and upgrade
scripts accept only the verified directory printed by the verifier. Do not
extract or run installers from a download directory, and remove the verified
staging directory after the lifecycle operation succeeds.

`upgrade-agent.sh` first verifies that the existing `agent.env` contains exactly
one absolute `CONTROLLER_COMMAND_VERIFICATION_KEY_FILE` and that the referenced
Ed25519 public key satisfies the intersection of the Agent and privd type,
ownership, mode, link, symlink, size, and ancestry requirements. The shared key
must be independently provisioned as `root:ocserv-agent` mode `0440` or `0640`
beneath root-controlled ancestry. An Agent-owned legacy key is rejected rather
than promoted into a root trust anchor. This preflight happens before backups,
binaries, systemd units, or services are changed. A legacy two-line `agent.env`,
missing key, or unsafe key therefore stops the upgrade with provisioning
instructions instead of installing an Agent that cannot restart.
It also requires two distinct enrolled sealing key IDs and public-key
fingerprints, derives each fingerprint from its root-owned private key, and
rejects missing, reused, mismatched, symlinked, or unsafe key files before
changing installed state. For the first upgrade from a release without
purpose-separated sealing keys, issue a fresh enrollment token bound to the
node's existing EndpointID, make the root-protected token file readable by the
`ocserv-agent` group, and run the upgrade with the token path and environment:

```bash
sudo ENROLLMENT_TOKEN_FILE=/etc/ocservia-agent/sealing-key-enrollment.token \
  ENROLLMENT_ENVIRONMENT=production \
  /path/to/verified-package/scripts/upgrade-agent.sh
```

Before replacing any installed file, the upgrade runs the verified new Agent
binary as `ocserv-agent`. The Controller accepts this migration only for the
same workspace and EndpointID, only when the node has no sealing-key binding,
and only when the signed capability advertisement exactly matches the stored
supported set. It binds both descriptors atomically, consumes the token, and
returns the existing node UUID. The upgrade compares that UUID with `NODE_ID`
and records an exact root-owned marker. A missing token, wrong endpoint,
changed capability set, partial key binding, or different returned node stops
the upgrade before binary, unit, backup, or service changes. Delete the token
file after a successful upgrade. A rolled-back legacy Agent may reconnect only
with read-only capabilities; mutation capability requires the two exact bound
descriptors.
After the preflight, the script retains one matched snapshot of the previous
Agent and privd binaries, both base systemd units, and the production relay
drop-in presence and content under the root-only
`/var/lib/ocservia-upgrade/upgrade-backup` hierarchy. This directory is outside
privd's systemd-managed `StateDirectory`, so service startup cannot rewrite
rollback evidence ownership. A root-owned manifest binds
the exact snapshot digests, and rollback rejects unsafe ancestry, symlinks,
hard links, ownership, modes, or replacement. It also preserves endpoint
identity, the durable Agent database, journal, and configuration. Verify service health and
Controller connectivity after upgrade. To roll back the complete matched
snapshot, run:

```bash
sudo /usr/libexec/ocservia/ocservia-agent-rollback
```

The command validates the complete snapshot before stopping either unit, then
restores binaries and units together, reloads systemd, and starts privd before
Agent. Restoring only the binaries is unsupported because their CLI and local
wire contract may require the matching units. A rollback also restores the
previous release's security properties, so use it only for a controlled
recovery window and return to a fixed release promptly. `uninstall-agent.sh` preserves
identity and journal by default;
`--purge-state` is irreversible and is appropriate only after revoking the node
identity and preserving required audit material.

Command protocol `1.1` is fail closed: provision the Controller command
verification public key before upgrading the Agent and privd pair. Both
services load it independently, and privd also pins `NODE_ID`. New binaries
reject unsigned legacy mutations at both the command journal and root-effect
boundaries, so schedule rollout and Controller signing-key enablement as one
maintenance window. Keep both old and new public keys pinned during a
signing-key rotation until old authorizations have expired and all Unknown
outcomes have been reconciled.
