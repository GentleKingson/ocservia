# Agent and privd

The node runtime is split into an unprivileged `ocservia-agent` and a small
root `ocservia-privd`. The Agent owns network connectivity and local SQLite
state. Privd has no TCP listener and accepts only five typed read operations on
`/run/ocserv-platform/privd.sock`: service status, Ocserv version, sessions, IP
bans, and the fingerprint of `/etc/ocserv/ocserv.conf`.

Privd verifies the Unix peer UID before decoding a request. It maps each RPC to
a compiled-in executable and argument array. RPC input cannot select a program,
systemd unit, occtl argument, or filesystem path. Child stdout and stderr are
drained separately with independent limits and a deadline, then parsed into
stable DTOs. Raw child output is never returned across the privilege boundary.
The privd unit keeps only `CAP_DAC_OVERRIDE`, which packaged Ocserv requires to
connect to its mode `0711` control socket, and blocks all IP traffic with
`IPAddressDeny=any`.

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
identity and the SQLite journal. Use `--purge-state` only after the node has
been revoked and its retained identity is no longer needed.
