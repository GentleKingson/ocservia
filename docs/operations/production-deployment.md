# Production deployment

The production example in `deploy/production/compose.yaml` runs the HTTPS gateway, control plane, transport service, PostgreSQL, OpenTelemetry collector, and backup worker. It publishes only TCP 443. Database, application, and observability traffic remain on internal networks.

Use digest-pinned images for every `OCSERV_*_IMAGE` variable. Put referenced secret files in a launcher-owned, mode-`0700` `OCSERV_SECRET_DIR` outside the checkout. General secrets must be launcher-owned mode `0444`: the private parent directory prevents host traversal while the read-only file allows each explicitly mounted non-root service to read it. The Ed25519 Controller command private key, `controller-command-signing-key.pem`, must be owned by UID/GID `65534:65532` with mode `0400`, matching the non-root Controller process. File-backed Compose secrets are bind mounts on supported deployments, so the source ownership is required even though the Compose target also declares it. The Iroh Controller key and relay token must be owned by UID/GID 65532 with mode `0400`. The launchers reject missing files, symbolic links, and any ownership or mode mismatch. Do not place credentials in Compose environment variables.

Generate the command key pair outside the checkout. Put only the private key in
`OCSERV_SECRET_DIR`; distribute the public key to Agents through the node
provisioning channel described in
`docs/development/command-authorization-v1.md`. `transportd` must never receive
the private key.

Provision the backup bind mount for the non-root PostgreSQL UID before startup. The launcher rejects missing, symbolic-link, incorrectly owned, or overly permissive paths:

```bash
sudo install -d -o 999 -g 999 -m 0700 "$OCSERV_BACKUP_DIR"
```

Launch the platform with `deploy/production/compose.sh up -d` and each dedicated relay with `deploy/production/relay/compose.sh up -d`. These launchers reject mutable image tags; direct Compose invocation is not a supported production path.

Backups retain the configured number of verified base backups. WAL cleanup is anchored to the oldest retained base backup, so point-in-time recovery remains possible across the retained window without allowing the local archive to grow forever. Monitor backup-worker health and the `LATEST` timestamp, copy each completed base backup plus its required WAL range to protected off-host storage, and confirm the off-host copy before reducing local retention.

Set `postgres.pgpass` to `postgres:5432:replication:ocservia_backup:<password>` using the same protected backup-role password supplied during first database initialization. The `replication` database field is required for `pg_basebackup` replication-protocol authentication. The backup entrypoint copies the read-only Compose secret into a private mode-0600 passfile before invoking libpq tools.

Production requires two independently operated dedicated relays:

```bash
export OCSERV_RELAY_URL_A=https://relay-a.example.com
export OCSERV_RELAY_URL_B=https://relay-b.example.com
```

The control plane runs `--role=all`. Terminate public TLS at the gateway. Configure the OIDC redirect URI as `https://$OCSERV_PUBLIC_HOST/api/v1/auth/callback` and use an HTTPS certificate signer.

The production containers intentionally use separate service identities:
Controller UID 65534, transportd UID 65532, and shared socket GID 65532. Keep
the Compose `OCSERV_TRANSPORT_UID`/`OCSERV_TRANSPORT_GID` and transportd
`--control-plane-uid`/`--control-plane-gid` values aligned with those service
users. The production launcher stops the Controller and transportd before
running a root-owned, network-disabled runtime initializer. The initializer
accepts only the current `65532:65532 0750` transport volume or the exact
legacy `65532:65532 0770` state, seals the directory before inspecting it,
removes only a trusted stale transport socket, and finishes at `0750`.
All services mount this volume with copy-up disabled so the container runtime
cannot replace the initializer's validated ownership or mode from image data.
Unexpected owners, entries, links, or modes fail closed. Do not invoke Compose
directly or make either socket parent client-writable.

Before starting, validate rendered configuration without printing secret contents:

```bash
deploy/production/compose.sh config --quiet
deploy/production/compose.sh up -d
```

Verify `/readyz`, an authenticated read, a node connection through each relay, OTLP delivery, and a restore from the newest backup. Never expose PostgreSQL, Unix sockets, Docker sockets, or host `/proc` and `/sys` mounts.
