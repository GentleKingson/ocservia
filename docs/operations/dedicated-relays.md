# Dedicated relays

Run `deploy/production/relay/compose.yaml` on two separate hosts and failure domains. Each host publishes TCP 80/443 and UDP 7842 only. Use distinct DNS names and certificates, but provision the same high-entropy access token to both relays, the Controller transport service, and enrolled Agents through protected files.

Set `OCSERV_RELAY_SECRET_DIR`, a digest-pinned `OCSERV_RELAY_IMAGE`, and the relay hostname in `deploy/production/relay/relay.toml`, then validate and start each relay independently with `deploy/production/relay/compose.sh config --quiet` and `deploy/production/relay/compose.sh up -d`. Production Controller and Agent processes must use `--relay-mode custom`, both HTTPS URLs, and `--relay-token-file`; public/default relays are not a production fallback.

To rotate the token without losing both paths, add the replacement credential to relay A, Controller, and Agents; verify traffic; repeat for relay B; then remove the old credential. During a relay outage, keep the healthy relay serving, repair the failed host, and verify both relay URLs are advertised before declaring recovery. Do not rotate endpoint identity keys merely because one relay is unavailable.
