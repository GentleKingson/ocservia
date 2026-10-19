# Configure a dedicated Relay

Every supported deployment uses a single dedicated HTTPS Relay running in
custom mode with authenticated token files and the standard identity and
command verification checks. The instructions below cover a Standalone Relay on
its own host; in an Integrated deployment, the Relay runs on the Controller
host and derives its URL from `OCSERV_RELAY_PUBLIC_HOST` (see the
[Integrated lifecycle configuration](../../deploy/production/integrated/README.md#lifecycle-configuration)).
Public or default relays are never used as a production fallback. Because
normal direct connectivity remains active alongside the Relay, any Relay
recovery test must prove that traffic actually depends on the Relay path.

Agent enrollment, Agent sessions, and transportd disable public endpoint address
publishing and lookup. They use the configured Relay addresses; an EndpointID
alone is not enough to discover a peer. They resolve the Relay name only
through the host's system DNS configuration and hosts file, with iroh's public
fallback resolvers disabled. If those cannot resolve the name, the Relay
connection fails and logs the resolution error; existing reconnection keeps
retrying the configured resolvers. Public nameservers listed in the host's own
DNS configuration remain an operator dependency.

## Before you begin

- One independent Relay host with a DNS name and matching TLS certificate.
- A single high-entropy relay access token provisioned to the Relay, the
  Controller transport service, and all enrolled Agents.
- A prebuilt relay container image referenced by an explicit `vX.Y.Z` tag or
  SHA256 digest.

## Steps

1. On the Relay host, create `OCSERV_RELAY_SECRET_DIR` as a launcher-owned
   directory with mode `0700` containing `tls.crt`, `tls.key`, and
   `relay-access-token`. Both TLS files must be owned by the launcher user with
   mode `0444`, while `relay-access-token` must be owned by UID/GID 65532 with
   mode `0400`. Export `OCSERV_RELAY_IMAGE` with an explicit version tag or
   SHA-256 reference, assign a DNS name to the host with its matching
   certificate and key, and use the checked-in
   `deploy/production/relay/relay.toml` configuration. The standalone Relay
   publishes TCP 80/443 and UDP 7842.
2. On the Relay host, validate and start the relay:

   ```bash
   deploy/production/relay/compose.sh config --quiet
   deploy/production/relay/compose.sh up -d
   ```

3. Configure the Controller relay URLs before starting or restarting the
   Controller:

   ```bash
   export OCSERV_RELAY_URL_A=https://relay.example.com
   export OCSERV_RELAY_URL_B=
   ```

   You can also define these variables in the Controller's protected
   `install.env`. `OCSERV_RELAY_URL_A` is required, whereas
   `OCSERV_RELAY_URL_B` must be absent or empty (a non-empty B value causes
   validation to fail before any side effects occur). Exporting an explicitly
   empty `OCSERV_RELAY_URL_B` in the shell overrides any B entry present in
   `install.env`.

4. Before enabling services on each managed node, configure the Agent with the
   production relay drop-in and launcher, the matching URLs, and the same token
   file; see the [Agent lifecycle reference](../operations/agent-lifecycle.md)
   for the verified package path and file contract.

   ```dotenv
   RELAY_URL_A=https://relay.example.com
   RELAY_URL_B=
   ```

   Place these values in the managed node's `install.env` during initial
   installation, or in `/etc/ocservia-agent/relays.env` when making an explicit
   operator topology change. Never edit the released Compose file or place raw
   token contents in either environment file.

## Verify

Confirm that the Controller and every Agent use the same single Relay URL.
Because an outage on that Relay interrupts Relay-dependent traffic, recovery
requires restoring the Relay with the same address, certificate, and
credentials, then verifying reconnection, fresh heartbeats, and command
results. Preserve pending command state and reconcile any Unknown outcomes
before performing an explicit safe retry.

## Migrate an existing A/B deployment

During a planned maintenance window, clear the B entry in both the Controller
and Agent configurations before running the new installers or starting the new
launchers, while keeping A and all identity, enrollment, token, and journal
material intact. Because services do not hot-reload the Relay list, restart the
affected services and verify A-only traffic before retiring B. Rerunning the
installer on an already-enrolled node validates the existing configuration and
refuses any persisted non-empty B entry without rewriting topology or replacing
tokens. If you later need to roll back, check the previous release's actual
requirements first; see [Agent rollback](agent-lifecycle.md#rollback) and
[Controller rollback](controller-lifecycle.md#rollback).

The Standalone Relay runs as an independent service outside the Controller
stack. Running a Standalone Relay and a Controller on the same host is not
validated because both default deployments bind TCP 443 across all addresses,
making an unmodified co-hosted setup on a single IP unsupported. For a
single-host deployment, use Integrated mode instead.

## Troubleshooting

If the Relay becomes unavailable, do not rotate endpoint identity keys or switch
to public relays. Because operating a single Relay means maintenance and token
rotation can briefly interrupt communication, schedule a recovery window
instead of attempting a two-path zero-interruption rotation. Preserve identity,
enrollment, and pending command journals, and verify authenticated traffic once
service is restored.

## Trust and capacity

Sharing the Relay access token only authenticates access to the relay
transport; it does not act as an Agent identity or a Controller capacity guard.
Keep all five trust classes and the reserved known-Agent capacity enabled in
transportd. Direct and Relay paths enforce the same snapshot, revocation,
authorization, registration-recheck, and enrollment-token rules, and IP or NAT
identity must never be used to refill an EndpointID budget.
