# Dedicated relays

Run `deploy/production/relay/compose.yaml` on one independent host for supported non-redundant operation, or two separate hosts and failure domains for recommended redundancy. Each host publishes TCP 80/443 and UDP 7842 only. Use distinct DNS names and certificates when running two, but provision the same high-entropy access token to every configured relay, the Controller transport service, and enrolled Agents through protected files.

Set `OCSERV_RELAY_SECRET_DIR` to a launcher-owned mode-`0700` directory, install its TLS files as launcher-owned mode `0444`, and install `relay-access-token` as UID/GID 65532 mode `0400`. Assign each host a distinct DNS name and matching certificate, use the checked-in `deploy/production/relay/relay.toml`, and set a digest-pinned `OCSERV_RELAY_IMAGE`. Validate and start each relay independently with `deploy/production/relay/compose.sh config --quiet` and `deploy/production/relay/compose.sh up -d`. Production Controller and Agent processes use `--relay-mode custom`, one or two HTTPS URLs, and `--relay-token-file`; public/default relays are not a production fallback. The binaries accept 1-8 distinct custom URLs; the A/B installers expose 1-2.

Single-relay maintenance or token rotation can interrupt communication. Restore the same address and trust material, then verify fresh heartbeats and results without resetting identities or enrollment. Two-relay maintenance may use the healthy alternate path only after that path has been verified. Recovery through the original relay is not alternate-relay failover. Do not rotate endpoint identity keys merely because a relay is unavailable. See [configuration and topology migration](../how-to/dedicated-relays.md) for examples and restart order.

The shared Relay project token is not an Agent identity and cannot protect
Controller trust capacity. Keep transportd's five trust classes and reserved
known-Agent capacity enabled even when every expected connection uses the
dedicated relays. Direct and Relay paths pass the same snapshot, revocation,
authorization, registration-recheck, and enrollment-token invariants. IP or NAT
identity is never used to refill an EndpointID budget.
