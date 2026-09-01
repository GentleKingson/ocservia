# Configure dedicated relays

Run two relay instances in separate failure domains and configure the
Controller and Agents to use both. Public/default relays are not a production
fallback.

## Before you begin

- Two relay hosts with distinct DNS names and certificates.
- The same high-entropy relay access token provisioned to both relays, the
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

3. Set both Controller relay URLs before starting or restarting the
   Controller:

   ```bash
   export OCSERV_RELAY_URL_A=https://relay-a.example.com
   export OCSERV_RELAY_URL_B=https://relay-b.example.com
   ```

4. Configure each Agent with the production relay drop-in, both URLs, and the
   same token file before enabling its services. See [Agent lifecycle
   reference](../operations/agent-lifecycle.md) for the verified package path
   and exact file contract.

## Verify

Verify the Controller and Agent are configured with both URLs. Test an Agent
connection through each relay independently, then stop one relay and confirm
the Agent reconnects through the other before restoring the failed host.

## Troubleshooting

Do not rotate endpoint identity keys because one relay is unavailable. Do not
switch to public relays. For token rotation, add the replacement to relay A,
the Controller, and Agents first, verify traffic, repeat for relay B, then
remove the old token.

See [Dedicated relays reference](../operations/dedicated-relays.md) for trust
capacity and path invariants.
