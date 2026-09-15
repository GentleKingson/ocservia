# Configure dedicated relays

One dedicated HTTPS relay is a supported **non-redundant** production topology.
Two distinct relays in separate failure domains remain recommended for
redundancy. Both use `--relay-mode custom`, authenticated token files and the
same identity and command security checks. Public/default relays are never a
production fallback. A single relay does not disable the normal direct path.

## Before you begin

- One independent relay host, or two with distinct DNS names and certificates.
- The same high-entropy relay access token provisioned to all configured relays, the
  Controller transport service, and enrolled Agents.
- A digest-pinned relay image.

## Steps

1. On each relay host, provision `OCSERV_RELAY_SECRET_DIR` as a launcher-owned
   mode-`0700` directory containing `tls.crt`, `tls.key`, and
   `relay-access-token` with the ownership required by the launcher. Assign a
   distinct DNS name to each host and provision its matching certificate and
   key; use the checked-in `deploy/production/relay/relay.toml` configuration.
2. On each host, validate and start the relay:

   ```bash
   deploy/production/relay/compose.sh config --quiet
   deploy/production/relay/compose.sh up -d
   ```

3. Set Controller relay URLs before starting or restarting the
   Controller:

   ```bash
   export OCSERV_RELAY_URL_A=https://relay.example.com
   export OCSERV_RELAY_URL_B=
   ```

   These same entries work without `export` in the Controller's protected
   `install.env`. An explicitly exported empty B overrides a B in that file;
   an absent B also selects one relay. For two relays, set B to a distinct HTTPS
   address. A is always required, even when B exists. Nonempty invalid B and
   URLs that normalize to the same address are rejected, not ignored.

4. Configure each Agent with the production relay drop-in and launcher,
   the matching URLs, and the
   same token file before enabling its services. See [Agent lifecycle
   reference](../operations/agent-lifecycle.md) for the verified package path
   and exact file contract.

   ```dotenv
   RELAY_URL_A=https://relay.example.com
   RELAY_URL_B=
   ```

   These values belong in the managed node's `install.env` for installation,
   or `/etc/ocservia-agent/relays.env` for an explicit operator topology change.
   Do not edit the released Compose file or put token contents in either file.

## Verify

Verify both endpoints use the same effective list. Test traffic through each
configured relay. With one relay, its outage interrupts relay-dependent
traffic; restore the same address, certificate and credentials and check
reconnection, fresh heartbeats and command results. This is recovery, not
standby failover. With two relays, stop one and verify traffic through the
other before restoring it. Exclude direct paths when testing relay failure.

## Change topology

- Dual to single: upgrade both Controller and Agents including their launchers,
  verify operation, deliberately clear B in both configurations and restart
  the affected services, verify A-only traffic, then retire B.
- Single to dual: deploy and verify B first, set its distinct URL on both
  endpoints and restart the affected services, then verify both paths.
- There is no relay-list hot reload. An enrolled installer rerun only verifies
  its existing configuration; it must not change topology or replace tokens.
- Before an old-version rollback, restore valid distinct A/B configuration
  and both relay services. See [Agent rollback](agent-rollback.md) and
  [Controller rollback](controller-rollback.md) for additional guards.

The relay is an independent service, not embedded in the Controller. Sharing
a host is not validated here: both default deployments claim TCP 443 on all
addresses, so installing both unchanged on one IP is not a supported recipe.

## Troubleshooting

Do not rotate endpoint identity keys because one relay is unavailable. Do not
switch to public relays. Single-relay maintenance and token rotation can
interrupt communication; schedule a recovery window rather than applying a
two-path no-interruption procedure. Preserve identity, enrollment and pending
command journals, and verify authenticated traffic after restoring service.

See [Dedicated relays reference](../operations/dedicated-relays.md) for trust
capacity and path invariants.
