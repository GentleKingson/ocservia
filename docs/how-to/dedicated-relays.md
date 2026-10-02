# Configure dedicated relays

Each supported deployment uses one dedicated HTTPS Relay with custom mode,
authenticated token files and the existing identity/command security checks.
Public/default relays are never a production fallback. Normal direct
connectivity remains available; a recovery test must prove dependence on the Relay.

## Before you begin

- One independent Relay host with a DNS name and certificate.
- The same high-entropy relay access token provisioned to the Relay, the
  Controller transport service, and enrolled Agents.
- A prebuilt relay image with an explicit `vX.Y.Z` tag or SHA256 reference.

## Steps

1. On the Relay host, provision `OCSERV_RELAY_SECRET_DIR` as a launcher-owned
   mode-`0700` directory containing `tls.crt`, `tls.key`, and
   `relay-access-token`. TLS files are launcher-owned mode `0444`; the token
   is UID/GID 65532 mode `0400`. Set an explicit version-tagged or SHA-256
   `OCSERV_RELAY_IMAGE`. The standalone Relay publishes TCP 80/443 and UDP 7842.
   Assign a DNS name to the host and provision its matching certificate and
   key; use the checked-in `deploy/production/relay/relay.toml` configuration.
2. On the Relay host, validate and start the relay:

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

   These entries also work in the Controller's protected `install.env`.
   A is required. B must be absent or empty; nonempty B fails before side effects.
   An explicitly empty shell B overrides a B in `install.env`.

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

Verify Controller and Agent use the same sole Relay URL. Its outage can interrupt
Relay-dependent traffic. Restore the same address, certificate and credentials,
then check reconnection, fresh heartbeats and command results. Preserve pending
command state and reconcile Unknown outcomes before any explicit safe retry.

## Migrate an existing A/B deployment

During a planned interruption, deliberately clear B in both Controller and
Agent configurations before running the new installers or starting the new
launchers. Keep A and identity, enrollment, token and journal material intact.
Restart the affected services and verify A-only traffic before retiring B.
An enrolled installer rerun verifies existing configuration and refuses a
persisted nonempty B; it does not rewrite topology or replace tokens.
There is no Relay-list hot reload. Check the actual old release requirements
before rollback; see [Agent rollback](agent-lifecycle.md#rollback) and
[Controller rollback](controller-lifecycle.md#rollback).

The relay is an independent service, not embedded in the Controller. Sharing
a host is not validated here: both default deployments claim TCP 443 on all
addresses, so installing both unchanged on one IP is not a supported recipe.

## Troubleshooting

Do not rotate endpoint identity keys because one relay is unavailable. Do not
switch to public relays. Single-relay maintenance and token rotation can
interrupt communication; schedule a recovery window rather than applying a
two-path no-interruption procedure. Preserve identity, enrollment and pending
command journals, and verify authenticated traffic after restoring service.

## Trust and capacity

The shared Relay token is not an Agent identity or a Controller capacity guard.
Keep transportd's five trust classes and reserved known-Agent capacity enabled.
Direct and Relay paths enforce the same snapshot, revocation, authorization,
registration-recheck and enrollment-token rules. IP/NAT identity must never
refill an EndpointID budget.
