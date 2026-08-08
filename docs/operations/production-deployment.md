# Production deployment

The production example in `deploy/production/compose.yaml` runs the HTTPS gateway, control plane, transport service, PostgreSQL, OpenTelemetry collector, and backup worker. It publishes only TCP 443. Database, application, and observability traffic remain on internal networks.

Use digest-pinned images for every `OCSERV_*_IMAGE` variable. Put referenced secret files on a protected host filesystem outside the checkout. Generate the Controller Iroh key into the protected `controller-iroh.key` secret before startup. The Controller key and relay token must be owned by UID 65532 with mode `0600`; other secret files must not be group/world writable. Do not place credentials in Compose environment variables.

Launch the platform with `deploy/production/compose.sh up -d` and each dedicated relay with `deploy/production/relay/compose.sh up -d`. These launchers reject mutable image tags; direct Compose invocation is not a supported production path.

Backups retain the configured number of verified base backups. WAL cleanup is anchored to the oldest retained base backup, so point-in-time recovery remains possible across the retained window without allowing the local archive to grow forever. Monitor backup-worker health and the `LATEST` timestamp, copy each completed base backup plus its required WAL range to protected off-host storage, and confirm the off-host copy before reducing local retention.

Set `postgres.pgpass` to `postgres:5432:ocservia:ocservia_backup:<password>` using the same protected backup-role password supplied during first database initialization. The backup entrypoint copies the read-only Compose secret into a private mode-0600 passfile before invoking libpq tools.

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
