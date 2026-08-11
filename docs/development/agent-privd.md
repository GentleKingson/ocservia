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
sudo ./scripts/install-agent.sh
```

Before enabling the units, install the independently provisioned Controller
command verification key and edit `/etc/ocservia-agent/agent.env` with its path,
the approved controller EndpointID, and the node UUID:

```bash
sudo install -o root -g ocserv-agent -m 0640 \
  controller-command-verification-key.pem \
  /etc/ocservia-agent/controller-command-verification-key.pem
```

Both services refuse production startup without this pinned Ed25519 public key.
They independently validate it with no-follow, owner, mode, regular-file,
single-link, and safe-ancestry checks. Privd also reads the configured `NODE_ID`
and rejects a valid Controller proof for any other node.

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
systemd units, and production relay drop-in state under the private state
directory before restarting the units.

## Rollback and removal

Run `sudo /usr/libexec/ocservia/ocservia-agent-rollback` to roll back an
upgrade. It validates and restores the Agent binary, privd binary, both base
units, and the relay drop-in state from
`/var/lib/ocservia-agent/upgrade-backup`, then reloads systemd and starts privd
before the Agent. Restoring either binary without its matched unit and peer
binary is unsupported.

`sudo ./scripts/uninstall-agent.sh` removes units and binaries but retains node
identity, the Agent journal, and privd desired-effect evidence. Use
`--purge-state` only after the node has been revoked, no Unknown command remains,
and the retained identity and reconciliation evidence are no longer needed.
