# Production deployment

The production example in `deploy/production/compose.yaml` runs the HTTPS gateway, control plane, transport service, PostgreSQL, OpenTelemetry collector, and backup worker. It publishes only TCP 443. Database, application, and observability traffic remain on internal networks.

Use digest-pinned images for every `OCSERV_*_IMAGE` variable. Put referenced secret files in an absolute, canonical, launcher-owned, mode-`0700` `OCSERV_SECRET_DIR` outside the checkout; every ancestor must be root- or launcher-owned and not group/world writable. General secrets must be launcher-owned mode `0444`: the private parent directory prevents host traversal while the read-only file allows each explicitly mounted non-root service to read it. The Ed25519 Controller command private key, `controller-command-signing-key.pem`, and the 32-byte lowercase-hex audit event key, `audit-event-key`, must be owned by UID/GID `65534:65532` with mode `0400`, matching the non-root Controller process. Set a non-secret stable identifier such as `OCSERV_AUDIT_EVENT_KEY_ID=audit-event-v1`; the identifier is stored with each event. The audit event key is independent from `audit-checkpoint-key` and must never be reused for checkpoints or another purpose. File-backed Compose secrets are bind mounts on supported deployments, so the source ownership is required even though the Compose target also declares it. The Iroh Controller key and relay token must be owned by UID/GID 65532 with mode `0400`. The launcher rejects missing files, symbolic links, unsafe host ancestry, and ownership or mode mismatches; the Controller loader additionally rejects a hard-linked audit event key and unsafe in-container ancestry. Do not place credentials in Compose environment variables.

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

Replacing `postgres-app-password`, `postgres-backup-password`, `database-app-url`, or `postgres.pgpass` by itself does **not** rotate the password verifier already stored by PostgreSQL. To rotate both runtime roles, prepare two single-link, launcher-owned mode-`0400` or `0600` password files in a launcher-owned mode-`0700` directory outside `OCSERV_SECRET_DIR`, then run:

```bash
export OCSERV_NEW_POSTGRES_APP_PASSWORD_FILE=/protected/new-app-password
export OCSERV_NEW_POSTGRES_BACKUP_PASSWORD_FILE=/protected/new-backup-password
export OCSERV_TERMINATE_OLD_POSTGRES_SESSIONS=true # incident rotations only
deploy/production/rotate-postgres-credentials.sh
```

The workflow holds an exclusive mode-`0600` lock in the private secret directory for the complete rotation lifecycle, then verifies the current credentials, executes real `ALTER ROLE` statements through the local administrative connection, verifies both new credentials and rejects both old credentials for new connections, atomically updates the four Compose secret sources, recreates the Control Plane and backup clients, and verifies their new connections. A waiting rotation reads its baseline only after the preceding rotation releases that lock. Recovery restores the previous verifiers and files only when the database and files still match either the baseline or values written by that invocation; unexpected later state is never overwritten. Keep any reported recovery snapshot protected and services stopped until recovery completes. The script never accepts passwords as command-line arguments and does not print them.

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
accepts only a fresh root-owned Docker volume, the current `65532:65532 0750`
transport volume, or the exact legacy `65532:65532 0770` state; it seals the
directory before inspecting it, removes only a trusted stale transport socket,
and finishes at `0750`.
All services mount this volume with copy-up disabled so the container runtime
cannot replace the initializer's validated ownership or mode from image data.
Unexpected owners, entries, links, or modes fail closed. Do not invoke Compose
directly or make either socket parent client-writable.

Before starting, validate rendered configuration without printing secret contents:

```bash
deploy/production/compose.sh config --quiet
deploy/production/compose.sh up -d
```

Migration `000021` introduces authenticated audit events. Before upgrading a database that already contains audit history, stop API writes and let the previous scheduler create a checkpoint covering each workspace's exact audit tail. Keep that checkpoint key available to the migration container. While holding the audit tables against concurrent writes, the migration preflight verifies every legacy chain and its exact tail checkpoint before applying any `000021` schema change. A failed preflight leaves the database at schema `20`, so the previous release remains usable. Do not bypass this check or rewrite old rows. After the one-shot migration succeeds, each new event carries a domain-separated HMAC, version, and key ID, and checkpoint creation first verifies the entire event chain.

Verify `/readyz`, an authenticated read, a node connection through each relay, OTLP delivery, and a restore from the newest backup. Never expose PostgreSQL, Unix sockets, Docker sockets, or host `/proc` and `/sys` mounts.
