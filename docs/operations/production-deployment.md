# Production deployment

The production example in `deploy/production/compose.yaml` runs the HTTPS gateway, control plane, transport service, PostgreSQL, OpenTelemetry collector, and backup worker. It publishes only TCP 443. Database, application, and observability traffic remain on internal networks.

Use digest-pinned images for every `OCSERV_*_IMAGE` variable. Put referenced secret files on a protected host filesystem outside the checkout. Generate the Controller Iroh key into the protected `controller-iroh.key` secret before startup. Do not place credentials in Compose environment variables.

Production requires two independently operated dedicated relays:

```bash
export OCSERV_RELAY_URL_A=https://relay-a.example.com
export OCSERV_RELAY_URL_B=https://relay-b.example.com
```

The control plane runs `--role=all`. Terminate public TLS at the gateway. Configure the OIDC redirect URI as `https://$OCSERV_PUBLIC_HOST/api/v1/auth/callback` and use an HTTPS certificate signer.

Before starting, validate rendered configuration without printing secret contents:

```bash
docker compose -f deploy/production/compose.yaml config --quiet
docker compose -f deploy/production/compose.yaml up -d
```

Verify `/readyz`, an authenticated read, a node connection through each relay, OTLP delivery, and a restore from the newest backup. Never expose PostgreSQL, Unix sockets, Docker sockets, or host `/proc` and `/sys` mounts.
