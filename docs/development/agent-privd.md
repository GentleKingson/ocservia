# Agent and privd

The node runtime is split into an unprivileged `ocservia-agent` and a small
root `ocservia-privd`. The Agent owns network connectivity and local SQLite
state. Privd has no TCP listener. It accepts seven unauthenticated local reads on
`/run/ocserv-platform/privd.sock`: service status, Ocserv version, sessions, IP
bans, the fingerprint of `/etc/ocserv/ocserv.conf`, and hash-free users and
groups derived from the fixed Ocserv password file. Desired-effect observation
requires the original signed command, as do all configuration, certificate,
session, IP, service, password, and group operations.

Privd verifies the Unix peer UID before decoding a request, but UID admission is
only the first layer. Every privileged request carries the original
Controller-signed command. Privd pins its own Controller keyring and node ID,
independently verifies signature, expiry, claims, and recomputed semantic hash,
then derives the fixed operation and effect identity from the signed typed
payload. Agent-selected mutation arguments are not accepted. RPC input cannot
select a program, systemd unit, occtl argument, or filesystem path. Child stdout
and stderr are drained separately with independent limits and a deadline, then
parsed into stable DTOs. Raw child output is never returned across the
privilege boundary.
The privd unit keeps only `CAP_DAC_OVERRIDE`, which packaged Ocserv requires to
connect to its mode `0711` control socket, and blocks all IP traffic with
`IPAddressDeny=any`.

Privileged mutations use a root-only bounded SQLite store under
`/var/lib/ocservia-privd`. The store is local only and is not a business
database or network service. Authenticated command records bind node, command,
operation, idempotency, action, authorization and effect revisions, semantic
payload hash, effect kind, resource, expiry, and the bounded successful result.
An exact replay returns that result without repeating the root effect. Desired
user/group/config records additionally bind the authoritative file transition.
The authenticated store identity makes missing or mismatched database/key state
fail closed. Privd resolves every prepared whole-file transition before another
user/group mutation, so later changes cannot erase earlier recovery proof. Only
an exact prepared record with a matching before-state proves an absent effect.
The unit grants write access only to this state directory and the fixed Ocserv
directory. Keep the generated HMAC key and database together during backup,
restore, and binary rollback.

## Build and install

```bash
cd rust
cargo build --locked --release --package ocservia-agent --package ocservia-privd
cd ..
OUTPUT_DIR=dist AGENT_SIGNING_KEY=/secure/release-ed25519.key \
  VERSION=1.0.0 SOURCE_DATE_EPOCH=1786147200 ./scripts/package-agent.sh
VERIFIED_PACKAGE="$(sudo AGENT_TRUSTED_KEY_SHA256=<pinned-public-key-der-sha256> \
  ./scripts/verify-agent-package.sh dist/ocservia-agent-1.0.0-linux-amd64.tar.gz \
  dist/ocservia-agent-1.0.0-linux-amd64.tar.gz.sha256 \
  dist/ocservia-agent-1.0.0-linux-amd64.tar.gz.sha256.sig \
  /etc/ocservia/release-signing.pub.pem)"
sudo "${VERIFIED_PACKAGE}/scripts/install-agent.sh"
```

The verifier is the only supported extraction path. It stages and verifies the
exact archive below root-only `/var/lib/ocservia-upgrade/package-staging`; the
installer refuses a source tree or an independently extracted download.

Before enabling the units, install the independently provisioned Controller
command verification key and two distinct RSA private keys for user-password
and P12-password unsealing. Edit `/etc/ocservia-agent/agent.env` with the
Controller key path, controller EndpointID, node UUID, distinct sealing key
IDs, and the lowercase SHA-256 of each public key's DER encoding:

```bash
sudo install -o root -g ocserv-agent -m 0640 \
  controller-command-verification-key.pem \
  /etc/ocservia-agent/controller-command-verification-key.pem
sudo install -o root -g root -m 0600 user-password-seal-private.pem \
  /etc/ocservia-agent/user-password-seal-private.pem
sudo install -o root -g root -m 0600 p12-password-seal-private.pem \
  /etc/ocservia-agent/p12-password-seal-private.pem
```

Both services refuse production startup without the pinned Ed25519 public key
and exact purpose-separated sealing key configuration. Privd derives each RSA
public key at startup, compares it with the enrolled fingerprint, and refuses
reused key pairs.
The command verification key uses descriptor-relative no-follow loading; the
sealing keys require root ownership, private mode, regular single-link files,
and safe ancestry. Privd also reads the configured `NODE_ID`
and rejects a valid Controller proof for any other node.
Enrollment signs and persists both public-key descriptors. Later sessions and
password operations fail closed if the advertised or selected key ID no longer
matches that enrollment binding.

Then enable the services:

```bash
sudo systemctl enable --now ocservia-privd.service ocservia-agent.service
systemctl status ocservia-privd.service ocservia-agent.service
```

The installer creates the locked `ocserv-agent` service account and preserves
an existing enrollment configuration. Before changing any installed file,
`upgrade-agent.sh` rejects legacy configuration that does not name a valid,
safely provisioned Ed25519 Controller command verification key. After this
preflight it keeps one matched snapshot of the previous binary pair, base
systemd units, and production relay drop-in state under the root-only
`/var/lib/ocservia-upgrade` hierarchy, outside privd's systemd-managed runtime
state, before restarting the units. Privd publishes `/etc/ocserv/ocpasswd`
as a one-link `root:root` mode `0600` regular file through descriptor-relative,
no-follow operations and rejects unsafe legacy ownership, modes, links, or
parent ancestry instead of inheriting them.

## Rollback and removal

Run `sudo /usr/libexec/ocservia/ocservia-agent-rollback` to roll back an
upgrade. It validates and restores the Agent binary, privd binary, both base
units, and the relay drop-in state from
`/var/lib/ocservia-upgrade/upgrade-backup`, after verifying its root-only
ancestry and trusted digest manifest. It then reloads systemd and starts privd
before the Agent. Restoring either binary without its matched unit and peer
binary is unsupported.

`sudo ./scripts/uninstall-agent.sh` removes units and binaries but retains node
identity, the Agent journal, and privd desired-effect evidence. Use
`--purge-state` only after the node has been revoked, no Unknown command remains,
and the retained identity and reconciliation evidence are no longer needed.
