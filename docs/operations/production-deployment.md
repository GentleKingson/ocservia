# Production deployment

Release version examples also accept an exact `vX.Y.Z-rc.N` candidate tag
(positive N without leading zeros). RCs are not recommended stable releases.

> **Technical reference.** For a first deployment, start with [Deploy the
> Controller](../getting-started/production.md). This document retains the
> detailed release, filesystem, security, lifecycle, rollback, and recovery
> contracts.

After its external endpoint is deployed and verified, the operator-hosted thin
first-install chain for a **manual** installation (the operator prepares
`install.env` and all protected material) preserves the existing lifecycle
authorities:

```text
Stage-0 -> exact vX.Y.Z Stage-1 -> install.env -> durable clean checkout
        -> production/install.sh -> deployment configuration -> controller.sh
        -> smoke/readiness
```

Stage-0 is a convenience boundary. Its first bytes rely on the static HTTPS
endpoint. Stage-0 downloads the versioned Stage-1 asset over HTTPS. Stage-1
prepares a durable checkout, and controller.sh validates ordinary deployment
configuration and protected local state before activation.
`install.env` stays in the operator's configuration directory, separate from
the durable release checkout.
Until that hosting has operational ownership and byte-verification evidence,
the public first-install guide obtains Stage-1 from a clean exact-release checkout.

**Quick** (`controller-bootstrap.sh --quick`, a Release that ships
`deploy/production/quick-install.sh` is required) is a preset over this same
installer, not a second one. It ignores `./install.env`, implies
`--root-lifecycle`, and runs a read-only preflight first: both names resolve to
the same IPv4 addresses, any AAAA record is on this host, TCP 443 and UDP 7842
are free (unless retrying an interrupted Quick run), and `getent`, `ip` and `ss`
exist. `quick-install.sh` then prepares the
host, generates the protected material with `quick-materials.sh`, writes
`/etc/ocservia/install.env` (Integrated, bundled PostgreSQL, ACME, Local
authentication), runs `install.sh --root-lifecycle`, provisions the management
workspace and creates the Local administrators `initial-admin` and
`initial-approver`. It only installs a new host and refuses an existing
deployment, a `/etc/ocservia/install.env` it did not generate, or different
domains or email on a rerun. Later `controller.sh` and
`compose.sh` commands do not read that file; export the same effective
settings, as for any other installation. See [Quick
mode](bootstrap-hosting.md#quick-mode) and [Quick install
materials](../development/certificates-and-signer.md#quick-install-materials).
Every other combination below uses the manual path.

## Deployment combinations

| Choice | Standalone (default) | Integrated |
| --- | --- | --- |
| Install path | Manual only | Manual, or Quick (new host) |
| Lifecycle user | Launcher with Docker access, or `--root-lifecycle` | Root: `--root-lifecycle`; `compose.sh` rejects non-root |
| Manifest / Compose | v1 or v2 manifest; `docker compose up --wait` | v2 manifest only; Compose >= 2.24.4 |
| DNS names | Controller name | Two distinct lowercase names: `OCSERV_PUBLIC_HOST`, `OCSERV_RELAY_PUBLIC_HOST` |
| Controller / Relay TLS | `tls.crt` and `tls.key` in `OCSERV_SECRET_DIR`; ACME is not supported | Manual: those files in `OCSERV_SECRET_DIR` and `OCSERV_RELAY_SECRET_DIR`; or `OCSERV_TLS_MODE=acme` with `OCSERV_ACME_EMAIL` (Quick always uses ACME) |
| Relay | Operator-run HTTPS Relay: `OCSERV_RELAY_URL_A` required | Bundled single Relay; URL derived from `OCSERV_RELAY_PUBLIC_HOST` |
| Signer | External HTTPS endpoint in `OCSERV_CERTIFICATE_SIGNER_URL` plus `certificate-signer-token` | Bundled Signer at `https://signer:9443/sign`; manual: operator-prepared Signer files; Quick: generated |
| Published ports | TCP 443 | TCP 443 (Edge) and UDP 7842 (Relay) |
| `uninstall --purge-data` | Supported | Refused |

In both modes exactly one dedicated Relay is supported and
`OCSERV_RELAY_URL_B` must be empty. The database (bundled or external
PostgreSQL 18, or external MySQL 8.4 LTS) and the login mode (Local only, OIDC
only, or Local + OIDC) are independent of the mode for a manual installation;
Quick fixes bundled PostgreSQL and Local only. Mode, database and TLS mode are
recorded in `deployment-profile.json` at first install and cannot change in an
existing installation.

## Database support

The production launcher combines `deploy/production/compose.yaml` with one
database descriptor. Bundled PostgreSQL 18 remains the default and runs the HTTPS
gateway, control plane, transport service, PostgreSQL, and backup worker. An
external PostgreSQL 18 descriptor is also implemented; it uses a dedicated
egress network and requires `sslmode=verify-full` plus `database-ca.pem` for
owner, runtime, and backup connections. External MySQL 8.4 LTS is supported with verified TLS and backend-specific logical backups.
Bundled MySQL is rejected.
Standalone publishes only TCP 443. Integrated mode additionally publishes
UDP 7842 for its Relay; see the [integrated topology](../../deploy/production/integrated/README.md). Bundled database, application, and
observability traffic remain on internal networks. External database
deployments additionally attach database clients to the dedicated non-internal
`database-egress` network; no database port is published by ocservia.

| Backend | Deployment | Production support |
| --- | --- | --- |
| PostgreSQL 18 | bundled or external | Yes; bundled remains the default |
| MySQL 8.4 LTS | external only | Yes |
| MySQL | bundled | No |

PostgreSQL 17 and MariaDB are no longer supported. Before starting this
Controller against a PostgreSQL 17 database, the operator must complete a
controlled PostgreSQL major upgrade with `pg_upgrade` or dump/reload, preserving
a verified backup and rollback copy. Application migrations do not upgrade the
PostgreSQL server or its physical data format. MariaDB cannot be switched in
place by changing `OCSERV_DATABASE_BACKEND` to `mysql`; cross-engine conversion
is not provided by this project.

PostgreSQL 18 containers mount the named volume at `/var/lib/postgresql` and use
`PGDATA=/var/lib/postgresql/18/docker`. An existing PostgreSQL 17 volume must not
be reused as if it were an empty PostgreSQL 18 volume. Complete the operator-led
upgrade before activating the new layout; keep the old volume intact for recovery.

Select a non-default descriptor explicitly:

```dotenv
OCSERV_DATABASE_BACKEND=postgres
OCSERV_DATABASE_DEPLOYMENT=external
OCSERV_DATABASE_BACKUP_HOST=postgres.example.com
```

External MySQL example:

```dotenv
OCSERV_DATABASE_BACKEND=mysql
OCSERV_DATABASE_DEPLOYMENT=external
OCSERV_DATABASE_BACKUP_HOST=mysql.example.com
OCSERV_DATABASE_BACKUP_PORT=3306
OCSERV_DATABASE_BACKUP_NAME=ocservia
OCSERV_DATABASE_BACKUP_USER=ocservia_backup
OCSERV_DATABASE_BACKUP_IMAGE=registry.example.com/ocservia-mysql-backup@sha256:<digest>
```

For MySQL, use `external`, provide separate owner and runtime DSNs in
`database-owner-url` and `database-app-url`, and provision `database-ca.pem`
plus `database-backup.cnf` in the protected secret directory. Only `migrate`
receives the owner DSN. The runtime receives the application DSN and CA, never
the owner credential. Both DSNs must use `tls=true`; the production descriptor
mounts `database-ca.pem` as the trust root. The backend-specific backup image must use an explicit version tag or SHA-256
reference through `OCSERV_DATABASE_BACKUP_IMAGE` for standalone v1, or the
selected backend image in the v2 deployment configuration. Snapshot restore, PITR, failover, and
cross-engine movement are separate procedures; no PostgreSQL G6, HA, or PITR
claim applies to MySQL.

For Local only, OIDC only, or Local + OIDC configuration, login behavior and
one-shot first-admin creation, follow [Production authentication](authentication.md).
OIDC is optional when Local is enabled; `compose.sh` selects the OIDC overlay
only when configured. All modes retain protected session-key file input. A
manual installation may use any of the three modes; Quick configures Local only.

## Optional observability

Unset or empty `OCSERV_OTEL_BACKEND_ENDPOINT` disables OTLP export and the
OpenTelemetry Collector. The default topology is gateway -> control plane ->
PostgreSQL and transportd -> managed nodes, with a separate backup worker.
A nonempty endpoint automatically adds control plane -> Collector (OTLP 4317)
-> operator backend (mTLS) through the `observability` Compose profile.
Provision `otel-client.crt`, `otel-client.key`, and `otel-ca.crt` in
`OCSERV_SECRET_DIR` with launcher ownership and mode `0444` before enabling it.
Missing files or invalid permissions fail closed before startup. They are not
required when OTEL is disabled. On activation, disabling OTEL stops and removes
any existing Collector in the same project; `down` also includes the optional
Collector. The launcher ignores inherited `COMPOSE_PROFILES`, `COMPOSE_ENV_FILES`,
and automatic Compose `.env` files, using only the exported lifecycle configuration;
use the endpoint setting, not a manually selected profile, to enable observability.
Both release manifest schemas include `otel` for later opt-in; their image
inventories are listed under [Release manifests](#release-manifests).

The optional OTEL layout is part of the target's production deployment files.
The lifecycle resolves the verified target source rather than requiring its
descriptor to equal the current one. This does not convert historical layouts
or guarantee that an arbitrary target can run against existing state.

## Production secrets

Use explicit `vX.Y.Z` / `vX.Y.Z-rc.N` tags or SHA-256 references for prebuilt
`OCSERV_*_IMAGE` values. Keep secrets outside the checkout in a canonical absolute
`OCSERV_SECRET_DIR`. Every ancestor must be root- or launcher-owned without
group/world write permission. The launcher rejects missing files, symlinks and
ownership/mode mismatches. Never put credentials in Compose environment values.

| Path or file class | Host owner | Mode / additional checks |
| --- | --- | --- |
| `OCSERV_SECRET_DIR` | Launcher | `0700`; private parent prevents host traversal |
| General secrets, including session, OIDC and enabled OTEL files | Launcher | `0444`; only explicitly mounted services receive them |
| `controller-command-signing-key.pem`, `audit-event-key` | `65534:65532` | `0400`; audit loader also rejects hard links and unsafe container ancestry |
| `controller-iroh.key`, `relay-access-token` | `65532:65532` | `0400` |
| `controller-command-verification-key.pem` | `0:65532` | `0440`, one-link regular file |
| Optional `relay-ca.pem` | `0:0` | `0444`, nonempty one-link regular file |
| `OCSERV_BACKUP_DIR` | `999:999` | `0700`, no symlinks |

File-backed Compose secrets are bind mounts, so host ownership matters even when
the descriptor declares a target owner. `audit-event-key` is an independent
32-byte key encoded as lowercase hex; never reuse the audit checkpoint key.
`OCSERV_AUDIT_EVENT_KEY_ID` is its stable, non-secret identifier stored with events.
`tls.crt` and `tls.key` are required here only with `OCSERV_TLS_MODE=manual`
(the default); Integrated ACME omits them. Integrated Relay and Signer files have additional
[secret/state requirements](../../deploy/production/integrated/README.md#lifecycle-configuration).

This table is what a manual installation must prepare. Quick generates the
Controller files above with these owners and modes, except the TLS pair that
ACME obtains, and the Signer files. Quick configures neither OIDC nor OTEL.

For a private Relay CA, provision the public PEM bundle as
`${OCSERV_SECRET_DIR}/relay-ca.pem`: a nonempty one-link `root:root` regular
file, mode `0444`, with the same safe ancestry. `compose.sh` selects the shipped
`compose.relay-ca.yaml` overlay only when this file exists and mounts it only
into transportd. Unsafe present files fail closed; invalid PEM makes transportd
refuse startup. No CA file means the existing public roots, not disabled TLS.
This is additional trust, not pinning. Provision the same CA through the
[managed-node trust path](../getting-started/managed-node.md#1-prepare-the-node-configuration)
for enrollment and the Agent service; do not patch launchers or rendered
descriptors. Rotation/removal is an explicit operator trust change, not an
automatic install/upgrade action.

For a manual installation, generate the command key pair outside the checkout
(Quick generates it). Put the private key in
`OCSERV_SECRET_DIR` and its Ed25519 SPKI public key in
`controller-command-verification-key.pem` in the same directory. The public key
must be a one-link regular file owned by `0:65532`, mode `0440`; transportd mounts
only this public key to verify connection fences. Provision
it before installing or upgrading to this descriptor, and rotate it with the
matching signing key. Distribute the same public key to Agents through the node
provisioning channel in [command authorization](../development/command-authorization-v1.md).
`transportd` must never receive the private key. Missing, unreadable or mismatched
verification keys fail closed; do not remove fencing to make a node connect.
Rollback must satisfy the target's actual secret and mount requirements;
descriptor equality is not an admission gate.

Provision the backup bind mount for the non-root PostgreSQL UID before startup. The launcher rejects missing, symbolic-link, incorrectly owned, or overly permissive paths:

```bash
sudo install -d -o 999 -g 999 -m 0700 "$OCSERV_BACKUP_DIR"
```

### PostgreSQL initialization updates

Bundled PostgreSQL role initialization passes passwords through standard input.
Its scripts come from the selected release checkout's `postgres-init` directory;
replacing a Controller image or Agent package does not replace them. The official
PostgreSQL image runs them only on an empty data directory. Do not delete a volume
or rebuild a database to rerun initialization. Assess any historical credential
exposure separately and use [credential rotation](incident-recovery.md#postgresql-credential-rotation)
when required.

## Release manifests

The v1 schema supports standalone installations with six roles: `gateway`,
`control`, `transport`, `backup`, `postgres` and `otel`. The v2
reader additionally requires `signer_state_version: 1` and four exact image
roles: `edge`, `relay`, `signer`, `mysql_backup`. It retains
the same per-architecture filenames and protected local file rules. Integrated requires
v2; see [Integrated configuration](../../deploy/production/integrated/README.md#lifecycle-configuration).
V2 selects backend backup image references from this configuration rather than
`OCSERV_DATABASE_BACKUP_IMAGE`. First-party images use explicit version tags.

Formal GitHub Releases publish the Controller release manifests
`controller-release-amd64.json` and `controller-release-arm64.json`
alongside the Agent assets, plus the byte-identical
`controller-release.json` alias of the amd64 manifest for existing operators.
They also publish `controller-bootstrap.sh` and `managed-node-bootstrap.sh` as
versioned Stage-1 entrypoints downloaded over HTTPS.
Each configuration maps the images used by its platform and deployment mode.
V2 includes ten roles: eight first-party GHCR images use `vX.Y.Z` tags,
while PostgreSQL and OpenTelemetry retain pinned third-party references.
First-party packages must be public for installation without registry
credentials; publishing credentials and package visibility remain repository
administration concerns. Native builds and assembled version images cover
`linux/amd64` and `linux/arm64`, and each configuration records its platform.
Before any
Compose activation the lifecycle entrypoint asks the Docker daemon for its
server architecture and fails closed when the manifest platform does not match
the Docker host platform, so install the manifest variant that matches the
host.

The architecture-specific manifest (`controller-release-<arch>.json`; `install.sh`
downloads the one matching the host into `release-bundles/<tag>/` under the state
root) is ordinary deployment configuration. Download it over HTTPS and keep it in a protected directory.
The lifecycle validates its JSON structure, architecture, source checkout and
image mapping before Compose config, pull or activation. It requires no release
public key, signature, signed checksum manifest or provenance evidence. Local
configuration files must be regular root- or launcher-owned files, without
symlink ancestry or group/world write permission. Rollback and start use the
protected local lifecycle state and do not need the original download.

## Host and configuration contract

| Host | Architecture | Bootstrap policy |
| --- | --- | --- |
| Ubuntu 22.04 / 24.04 / 26.04; Debian 12 / 13 | amd64, arm64 | Installs missing prerequisites and Docker if absent |
| Debian 11 | amd64, arm64 | Bootstrappable; regular LTS ended 2026-08-31, arrange ELTS or equivalent maintenance |
| Ubuntu 20.04 | amd64, arm64 | Existing compatible Docker only; arrange ESM or equivalent maintenance |

`bootstrap-host.sh check` is read-only. Its `install` command installs missing
`jq`, `flock`, `curl`, `openssl` and CA certificates through apt. When Docker is
absent on a bootstrappable host, it uses Docker's distribution-specific official
apt repository. It preserves compatible existing Docker and refuses conflicting
runtimes or an unavailable Compose v2 plugin; it never uninstalls them. Docker
must support `docker compose up --wait`.

The launcher is the bootstrap's sudo-invoking user, or root for a root lifecycle.
That user must already be able to reach the Docker daemon. Bootstrap never
changes group membership, socket permissions, firewall rules or Docker TCP
listeners. Fresh hosts without Docker require the installer's explicit
`--root-lifecycle`, or separately prepared Docker access. The root path forwards
only allowlisted settings and removes `SUDO_USER`; do not substitute `sudo -E`.

The state root defaults to `/var/lib/ocservia-controller`: canonical absolute
path, safe root/launcher-owned ancestry without group/world write, launcher-owned
mode `0700`. Bootstrap can also create `OCSERV_BACKUP_DIR` as `999:999 0700`.
Incorrect existing ownership or modes are reported, not repaired. Secrets,
trust material and host security maintenance remain operator-provisioned.

For a manual installation the installer loads `./install.env` from its working
directory; explicit exported settings override it,
including empty values. (Quick ignores `./install.env` and writes its own
`/etc/ocservia/install.env`.) Subsequent lifecycle and Compose commands need the same
effective exported environment; they do not reload that file. Lifecycle commands
restore image settings from their selected manifest. See the complete
[first-install procedure](../getting-started/production.md) for configuration,
secrets, host preparation and activation.

## Lifecycle state and failure recovery

Command examples live in [Controller lifecycle](../how-to/controller-lifecycle.md).
Use `controller.sh` for install, upgrade, rollback, start and uninstall.
Implementation: [controller.sh](../../deploy/production/controller.sh) and
[compose.sh](../../deploy/production/compose.sh).
The versioned bootstrap is only a first-install entrypoint; rerunning an unpinned
Stage-0 script is not an upgrade procedure.

`install` requires Docker Compose v2 with
`docker compose up --wait` support. It takes an exclusive local lock in
`/var/lib/ocservia-controller`, rejects an existing
`current-release.json`, validates that the checkout HEAD and clean working tree
match the manifest `source_commit`, runs the guarded Compose preflight, pulls
the selected version-tagged or SHA-256 image references, and starts the dependency graph with
`up -d --wait`. It then runs
`deploy/production/controller-release-smoke.sh`, which reuses Compose health,
probes the public HTTPS `/api/v1/readyz` and `/api/v1/version` routes, verifies
the target version and source commit, and retains the existing transport socket
and backup freshness health boundaries. Before activation it atomically records
the target and lifecycle phase in `pending-release.json`. A failed install or
upgrade retains that file with failure evidence; only a retry with the
identical target may continue. Only after the smoke passes is the manifest
committed as `current-release.json` (and, for upgrades,
`previous-release.json`), all with mode `0600`. Upgrade pending evidence also
records the pre-activation current manifest so recovery can restore the
previous-release state before clearing completed evidence. It does not create
or rotate secrets, certificates, or identity keys.

Run the lifecycle with the validated target configuration. Its `source_commit`
must be available locally (fetch the exact commit from the trusted repository
if the checkout is shallow). The lifecycle reuses the matching clean checkout,
or retains a separate clean Git checkout under the protected state root; it
never edits an existing checkout to force a match. Compose and smoke come from
that target source, including during later start/uninstall. Retained source
checkouts are not database backups and are not removed by runtime uninstall.

Upgrade validates the confirmed current state and target first, checks the
target `source_commit` against a clean checkout before any Compose operation,
checks the current descriptor's dependency and backup health, renders the target Compose configuration,
pulls target images while the current release is still running, then runs the
existing database migration and `up -d --wait` dependency graph. Release smoke
must pass before it atomically rolls the complete manifests into
`previous-release.json` and `current-release.json`. An identical manifest is a
no-op. Lower, equal and higher software version targets use the same execution
path; a different artifact with the same version string is not a no-op.
Failures after activation return non-zero without redeploying old
images, running down migrations, or changing confirmed release state.

Rollback uses only the protected `previous-release.json`; it never accepts an
operator-selected manifest. It does not require a lower version, equal migration
numbers or unchanged deployment descriptors, and performs no historical database
compatibility preflight. The exact target source and selected images must
still be available and satisfy the actual deployment's requirements. This is
not a guarantee that an arbitrary previous release can use the existing data.

Rollback renders and pulls the previous image references, then requires the
functional release smoke to confirm the previous version and source commit
before exchanging confirmed state. It starts the normal target Compose graph
with `up -d --wait`, including required forward initialization, never a database
down migration or restore. A failure after activation leaves
confirmed state unchanged and retains pending failure evidence for a same-target
retry; it does not automatically redeploy the current images.
[Backend-specific recovery](incident-recovery.md#database-recovery) is the
disaster-recovery boundary, not an application rollback mechanism.

### Start and uninstall

`start` resolves the retained current manifest's image inventory and exact source,
runs the target Compose graph with `up -d --wait` and release smoke, and leaves
confirmed state unchanged. Keep the production environment, secrets and backup
directory available. Do not use `install` when confirmed state already exists.

Uninstall takes the lifecycle lock and runs protected Compose `down`. It retains
named database/transport/trust volumes, external databases, confirmed manifests,
source checkouts, backups and secrets. Repeating a successful uninstall is safe.
`--purge-data` additionally removes production project volumes and local release
state after successful shutdown; integrated mode refuses it. The lock, secrets,
backup directory, external databases, off-host backups, source checkouts, images
and unrelated volumes remain. Purge is neither secure erase nor database recovery.

Both uninstall forms refuse pending transactions. Shutdown failure leaves
confirmed state and data untouched. Failed volume purge retains lifecycle state;
failed state cleanup reports residual paths. Neither operation re-enters Stage-0.

The target's owner-only database initialization validates supported SQL artifact
checksums and execution receipts, serializes execution and propagates SQL or
partial-execution failures. There is no software-version/schema-range admission
policy, but unsupported epochs, unknown/noncontiguous receipts and invalid content
still fail closed. Readiness checks current core reads, permissions and event
streams; it does not certify historical database compatibility. Use
[SQL artifact contracts](../development/control-plane.md#current-sql-artifacts-and-bounded-upgrades)
and [backend recovery](incident-recovery.md#database-recovery), never edit receipts
to force startup.

Lifecycle acceptance keeps separate evidence for five boundaries: Compose
container health and dependency readiness; functional release identity from the
release smoke; explicit target application and failure-state handling; current
database initialization, permissions and business behavior; and
disaster recovery through the selected backend's documented procedure. Passing one boundary
does not establish the others.

## Backup and credential operations

Use [database backup and restore](database-backup-restore.md) for retention,
backup-role connection files, off-host copies and isolated restore verification.
Use [PostgreSQL credential rotation](incident-recovery.md#postgresql-credential-rotation)
to update database verifiers and secret files together. Neither image replacement
nor replacing a secret file rotates a database password.

## Relay and application networking

Standalone uses one independently deployed HTTPS Relay:

```bash
export OCSERV_RELAY_URL_A=https://relay.example.com
export OCSERV_RELAY_URL_B=
```

A is required; B must be absent or empty. Nonempty B is rejected before install
or process execution (`install.sh`, `compose.sh` and the transportd wrapper all
check it). Exactly one dedicated Relay is supported: B is not optional
redundancy, and no Relay failover is provided. Recovering from a Relay loss means
restoring that original Relay with its address and trust material, not switching
to another one. These values can be set in `install.env` without editing
release files. Integrated mode derives its Relay URL from its public host (a
conflicting A, or any nonempty B, is rejected); use
[its configuration contract](../../deploy/production/integrated/README.md#lifecycle-configuration)
instead of overriding standalone Relay settings. For an existing A/B deployment, deliberately clear B on both
Controller and Agents while preserving identity and trust material, then
restart and verify A-only traffic. See [dedicated Relay configuration](../how-to/dedicated-relay.md).

Only transportd joins the non-internal `relay-egress` network, for DNS and
outbound HTTPS to the Relay by its HTTPS name: an independently deployed Relay in
Standalone, the same-host Relay through its public name in Integrated. The
application, database and observability networks remain internal; this adds no
published Controller ports. The existing socket healthcheck does not establish relay
reachability. Validate TLS, token-authenticated relay traffic and fresh Agent
observations separately. Normal production direct connectivity is unchanged.

The control plane runs `--role=all`. Public TLS terminates at the gateway (behind Edge in Integrated); use an HTTPS certificate signer (the external endpoint in Standalone, the bundled Signer in Integrated). Set `OCSERV_PUBLIC_ORIGIN` to the public HTTPS origin. When OIDC is enabled, configure its redirect URI as `https://$OCSERV_PUBLIC_HOST/api/v1/auth/callback`; its origin must match `OCSERV_PUBLIC_ORIGIN`.

### Gateway response headers

Both gateway configurations, [`Caddyfile`](../../deploy/production/Caddyfile)
(Standalone) and [`integrated/site.caddy`](../../deploy/production/integrated/site.caddy)
(Integrated), send HSTS, `nosniff`, `X-Frame-Options: DENY` and
`Referrer-Policy: no-referrer` on every response, and the same Content Security
Policy in **Report-Only** mode: `default-src 'self'; object-src 'none'; base-uri
'none'; frame-ancestors 'none'; form-action 'self'`. Report-Only blocks nothing
and names no report endpoint; violations appear only in the browser console.
Keep the two policies identical. Before switching the header to
`Content-Security-Policy`, run the browser specs against both gateways over HTTPS
in Chromium, Firefox and WebKit with no violations; if enforcement breaks a page,
return only this header to Report-Only and keep the other headers. No reported
violation covers only the exercised paths and is not proof of the absence of XSS.

<a id="authentication-request-budgets"></a>
### Gateway addressing and proxy trust

Request limits and trusted-client-IP handling are documented in
[authentication](authentication.md#authentication-request-budgets-and-proxy-trust).

Production Compose assigns the gateway the static application-network address
`172.30.240.2` and defaults `OCSERV_AUTH_TRUSTED_PROXY_CIDRS` to that address's
`/32`. Recreating the gateway therefore preserves its trusted source address.
The application subnet is `172.30.240.0/24`; dynamic allocations use only
`172.30.240.128/25`, so other services cannot acquire the gateway address while
it is absent. This pairs Compose's [static service address and IPAM configuration](https://docs.docker.com/reference/compose-file/services/#ipv4_address)
rather than relying on a previously observed dynamic IP.

If that subnet overlaps host/VPN routes or another Docker network, set
`OCSERV_APPLICATION_SUBNET`, `OCSERV_APPLICATION_IP_RANGE`, and
`OCSERV_GATEWAY_APPLICATION_IP` together in the Controller environment or
`install.env`. Keep the gateway IP inside the subnet, outside the dynamic range,
and distinct from the network's bridge gateway. The default trusted `/32`
automatically follows `OCSERV_GATEWAY_APPLICATION_IP`. An explicit
`OCSERV_AUTH_TRUSTED_PROXY_CIDRS` overrides that default and must be updated
when the chosen static IP changes; an explicitly empty value trusts no proxy.
Integrated rejects any value other than the gateway's own `/32`.

Legacy application-network detection and automatic conversion have been removed.
Changing an existing network layout requires an explicit operator redeployment;
the lifecycle does not remove networks or attached services to force it to fit.
Preserve durable state and follow [incident recovery](incident-recovery.md)
when a target cannot start with the existing deployment. A missing compatibility
rejection is not proof that cross-version state is safe.

Do not trust the entire application subnet or publish the Controller port.

## Runtime boundaries and verification

The reference Controller enables bounded shared SSE fan-out with 128 global,
8 identity, 4 session, 32 workspace, 16 resource, and 64 watcher limits. These
defaults reserve PostgreSQL and HTTP capacity for unrelated API work in the P1
single-VM profile. Raising them requires rerunning `make p1-full`. The gateway
is defense in depth only; do not remove application admission, session
revalidation, bounded queues, or watcher backoff. `/readyz` reports unavailable
while an active durable-event watcher is in database backoff; alert on
`sse_unhealthy_watchers` and verify cursor catch-up rather than restarting a
healthy fan-out loop repeatedly.

Privileged commands require root-authenticated privd key enrollment, key
rotation state, receipt evidence and certificate receipt bindings. Deploy Controller
support first, register each root-owned key with its one-time credential, then
approve `privd_result_attestation_v1` and upgrade privd and Agent. Until this is
complete, the node remains readable but privileged certificate, secret,
configuration, service, user, and session mutations fail closed. Missing proof
never enables a legacy success path.

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
```

Before a major database cutover, first upgrade existing environments to the
stable v1.2.0 checkpoint at `169102557cd610847c9f6ac2083336cdcf82c483` and retain a
verified backup. Confirm its epoch-1/revision-0 receipt before installing the
new major; the epoch-2 binary cannot replay earlier migrations. Preserve the
real audit event and checkpoint keys. Historical audit events remain business
data: startup still verifies the chain and refuses an unauthenticated legacy
tail unless its exact tail is checkpointed. Do not rewrite audit evidence or
schema receipts to bypass either check. Stop old writers before migration and
use the normal owner process; only a successful verified cutover permits
starting the new runtime. Rollback uses the old binary with its compatible
backup, not a schema downgrade.

Activate through the lifecycle, then verify `/api/v1/readyz`, an authenticated
read, a node connection through the configured relay, OTLP delivery when enabled, and a restore from the newest backup. Never expose PostgreSQL, Unix sockets, Docker sockets, or host `/proc` and `/sys` mounts.
