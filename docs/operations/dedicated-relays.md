# Dedicated relays

Run one `deploy/production/relay/compose.yaml` instance on an independent host.
It publishes TCP 80/443 and UDP 7842 only. Provision the same high-entropy access
token to this Relay, Controller transport service and enrolled Agents through
protected files.

Set `OCSERV_RELAY_SECRET_DIR` to a launcher-owned mode-`0700` directory, install
TLS files as launcher-owned mode `0444`, and install `relay-access-token` as
UID/GID 65532 mode `0400`. Use a matching DNS name/certificate, the checked-in
`deploy/production/relay/relay.toml`, and a digest-pinned `OCSERV_RELAY_IMAGE`.
Validate and start it with `deploy/production/relay/compose.sh config --quiet`
and `deploy/production/relay/compose.sh up -d`. Production Controller and Agent
launchers use `--relay-mode custom`, exactly one HTTPS URL and a protected token
file. Nonempty Relay B is rejected. Public/default relays are not a fallback.

Maintenance or token rotation may interrupt communication. Restore the same
address and trust material, then verify fresh heartbeats and command results
without resetting identities or enrollment. See [configuration and migration](../how-to/dedicated-relays.md).

The shared Relay project token is not an Agent identity and cannot protect
Controller trust capacity. Keep transportd's five trust classes and reserved
known-Agent capacity enabled even when every expected connection uses the
dedicated relays. Direct and Relay paths pass the same snapshot, revocation,
authorization, registration-recheck, and enrollment-token invariants. IP or NAT
identity is never used to refill an EndpointID budget.
