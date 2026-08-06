# Agent and privd

The node runtime is split into an unprivileged `ocservia-agent` and a small
root `ocservia-privd`. The Agent owns network connectivity and local SQLite
state. Privd has no TCP listener. It accepts eight typed reads on
`/run/ocserv-platform/privd.sock`: service status, Ocserv version, sessions, IP
bans, the fingerprint of `/etc/ocserv/ocserv.conf`, and hash-free users and
groups derived from the fixed Ocserv password file, plus a non-secret
desired-effect-store check used only for Unknown reconciliation. Its nine typed mutations
cover session disconnect/terminate, IP unban, service reload, user
create/disable/enable/password rotation, and authoritative group application.

Privd verifies the Unix peer UID before decoding a request. It maps each RPC to
a compiled-in executable and argument array. RPC input cannot select a program,
systemd unit, occtl argument, or filesystem path. Child stdout and stderr are
drained separately with independent limits and a deadline, then parsed into
stable DTOs. Raw child output is never returned across the privilege boundary.
The privd unit keeps only `CAP_DAC_OVERRIDE`, which packaged Ocserv requires to
connect to its mode `0711` control socket, and blocks all IP traffic with
`IPAddressDeny=any`.

Desired user/group mutations use a root-only bounded SQLite store under
`/var/lib/ocservia-privd`. The store is local only and is not a business
database or network service. Authenticated records bind the command identity,
semantic payload hash, revision, expiry, and authoritative file transition.
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

Edit `/etc/ocservia-agent/agent.env` with the approved controller EndpointID
and node UUID before enabling the units:

```bash
sudo systemctl enable --now ocservia-privd.service ocservia-agent.service
systemctl status ocservia-privd.service ocservia-agent.service
```

The installer creates the locked `ocserv-agent` service account and preserves
an existing enrollment configuration. `upgrade-agent.sh` keeps one previous
binary pair under the private state directory before restarting the units.

## Rollback and removal

To roll back an upgrade, stop both units, restore both `.previous` binaries
from `/var/lib/ocservia-agent/upgrade-backup`, run `systemctl daemon-reload`,
then start privd before the Agent. The two binaries must always be rolled back
together.

`sudo ./scripts/uninstall-agent.sh` removes units and binaries but retains node
identity, the Agent journal, and privd desired-effect evidence. Use
`--purge-state` only after the node has been revoked, no Unknown command remains,
and the retained identity and reconciliation evidence are no longer needed.
