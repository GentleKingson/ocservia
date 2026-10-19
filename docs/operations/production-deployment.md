# Production deployment

Release version examples throughout this reference also accept an exact
`vX.Y.Z-rc.N` candidate tag (positive N without leading zeros), though release
candidates are not recommended for stable production use.

> For a step-by-step first deployment, start with [Deploy the
> Controller](../getting-started/production.md). The sections below specify the
> production contracts for releases, host filesystems, secrets, lifecycle
> transitions, rollback, and recovery.

Once its external HTTPS endpoint is deployed and verified, the operator-hosted
thin bootstrap chain for a manual installation (where the operator prepares
`install.env` and all protected secret material) delegates authority along the
same lifecycle boundaries:

```text
Stage-0 -> exact vX.Y.Z Stage-1 -> install.env -> durable clean checkout
        -> production/install.sh -> deployment configuration -> controller.sh
        -> smoke/readiness
```

In this sequence, Stage-0 acts only as a convenience wrapper whose initial
bytes come from the static HTTPS endpoint and fetch the versioned Stage-1
script over HTTPS. Stage-1 then prepares a durable release checkout, leaving
`install.env` in the operator's configuration directory outside that checkout,
and hands off to controller.sh to validate the deployment configuration and
protected local state before activating any services. Until that static
bootstrap host has operational ownership and byte-verification evidence, the
public first-install guide obtains Stage-1 directly from a clean, exact-release
checkout.

Quick mode (`controller-bootstrap.sh --quick`, which requires a Release that
ships `deploy/production/quick-install.sh`) layers a single-host preset over
this same installer. It ignores `./install.env`, implies `--root-lifecycle`, and
opens with a read-only preflight verifying that `getent`, `ip`, and `ss` exist,
both configured DNS names resolve to the same IPv4 addresses, any AAAA record
belongs to the local host, and TCP 443 and UDP 7842 are free (unless retrying
an interrupted Quick run). Once preflight passes, `quick-install.sh` prepares
the host, invokes `quick-materials.sh` to generate the protected material,
writes `/etc/ocservia/install.env` (configuring Integrated mode, bundled
PostgreSQL, ACME, and Local authentication), runs `install.sh --root-lifecycle`,
provisions the management workspace, and creates the Local administrators
`initial-admin` and `initial-approver`. Quick mode installs new hosts only: it
refuses an existing deployment, a `/etc/ocservia/install.env` file it did not
generate, or changed domain or email values on a rerun. Subsequent
`controller.sh` and `compose.sh` invocations do not read that file
automatically, so operators must export the same effective settings as in any
other installation. See [Quick mode](bootstrap-hosting.md#quick-mode) and
[Quick install materials](../development/certificates-and-signer.md#quick-install-materials)
for details; every other deployment combination below follows the manual path.

## Deployment combinations

| Choice | Standalone (default) | Integrated |
| --- | --- | --- |
| Install path | Manual only | Manual, or Quick (new host) |
| Lifecycle user | Launcher with Docker access, or `--root-lifecycle` | Root: `--root-lifecycle`; `compose.sh` rejects non-root |
| Manifest / Compose | v1 or v2 manifest; `docker compose up --wait` | v2 manifest only; Compose >= 2.24.4 |
| DNS names | Controller name | Two distinct lowercase names: `OCSERV_PUBLIC_HOST`, `OCSERV_RELAY_PUBLIC_HOST` |
| Controller / Relay TLS | `tls.crt` and `tls.key` in `OCSERV_SECRET_DIR`; ACME is not supported | Manual: those files in `OCSERV_SECRET_DIR` and `OCSERV_RELAY_SECRET_DIR`; or `OCSERV_TLS_MODE=acme` with `OCSERV_ACME_EMAIL` (Quick always uses ACME) |
| Relay | Operator-run HTTPS Relay: `OCSERV_RELAY_URL_A` required | Bundled single Relay; URL derived from `OCSERV_RELAY_PUBLIC_HOST` |
| Signer | External HTTPS endpoint in `OCSERV_CERTIFICATE_SIGNER_URL` plus `certificate-signer-token`, on a [private listener](../development/certificates-and-signer.md#runtime-contract) | Bundled Signer at `https://signer:9443/sign`; manual: operator-prepared Signer files; Quick: generated |
| Published ports | TCP 443 | TCP 443 (Edge) and UDP 7842 (Relay) |
| `uninstall --purge-data` | Supported | Refused |

Both Standalone and Integrated deployments support only one dedicated Relay and
require `OCSERV_RELAY_URL_B` to be empty. In a manual installation, the
database backend (bundled or external PostgreSQL 18, or external MySQL 8.4 LTS)
and the login mode (Local only, OIDC only, or Local + OIDC) can be chosen
independently of the deployment mode, whereas Quick mode fixes bundled
PostgreSQL and Local-only login. At first install, the chosen deployment mode,
database backend, and TLS mode are recorded in `deployment-profile.json` and
cannot be changed on an existing installation.

## Database support

At startup, the production launcher pairs `deploy/production/compose.yaml` with
a single database descriptor. The default bundled PostgreSQL 18 configuration
runs the HTTPS gateway, control plane, transport service, PostgreSQL server,
and backup worker together, keeping database, application, and observability
traffic on internal Docker networks. Its Compose descriptor sets
`OCSERV_DATABASE_BUNDLED_POSTGRES=true`, the only declaration under which a
production PostgreSQL URL without `OCSERV_DATABASE_TLS_CA_FILE` may target the
`postgres` service (otherwise only loopback is accepted). External database deployments attach
database client containers to the dedicated non-internal `database-egress`
network without publishing any database port from ocservia: external PostgreSQL
18 requires `sslmode=verify-full` with `database-ca.pem` across owner, runtime,
and backup connections, while external MySQL 8.4 LTS requires verified TLS and
uses backend-specific logical backups. Bundled MySQL is rejected. On the host
side, Standalone mode publishes only TCP 443, whereas Integrated mode also
publishes UDP 7842 for its bundled Relay (see the
[integrated topology](../../deploy/production/integrated/README.md)).

| Backend | Deployment | Production support |
| --- | --- | --- |
| PostgreSQL 18 | bundled or external | Yes; bundled remains the default |
| MySQL 8.4 LTS | external only | Yes |
| MySQL | bundled | No |

Support for PostgreSQL 17 and MariaDB has been removed. Because application
schema migrations never upgrade the database engine or its physical data format,
operators running PostgreSQL 17 must complete a controlled major upgrade via
`pg_upgrade` or dump/reload before starting this Controller, preserving a
verified backup and rollback copy throughout. PostgreSQL 18 containers mount
their named volume at `/var/lib/postgresql` with
`PGDATA=/var/lib/postgresql/18/docker`; never mount an existing PostgreSQL 17
volume into a PostgreSQL 18 container as if it were empty, and keep the old
volume intact for recovery until the operator-led upgrade is verified. MariaDB
deployments cannot be switched in place by setting `OCSERV_DATABASE_BACKEND` to
`mysql`, and this project provides no cross-engine conversion tooling.

To use a non-default database descriptor, select it explicitly:

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

When configuring external MySQL, set `external` deployment mode and place
separate owner and runtime DSNs in `database-owner-url` and `database-app-url`
alongside `database-ca.pem` and `database-backup.cnf` in the protected secret
directory. Both DSNs must set `tls=true` against the mounted `database-ca.pem`
trust root; only `migrate` receives the owner DSN, while the runtime receives
only the application DSN and CA certificate. Pin the backend-specific backup
image to an explicit version tag or SHA-256 reference via
`OCSERV_DATABASE_BACKUP_IMAGE` for standalone v1 manifests, or through the
selected backend image field in v2 deployment configurations. Snapshot restore,
PITR, failover, and cross-engine migration remain separate procedures, and no
PostgreSQL G6, HA, or PITR claim applies to MySQL.

Details on Local only, OIDC only, and Local + OIDC configuration, login
behavior, and one-shot first-admin creation live in
[Production authentication](authentication.md). Every mode requires a protected
session-key file; OIDC remains optional when Local login is enabled, and
`compose.sh` selects the OIDC overlay only when configured. Manual
installations may use any of the three login modes, whereas Quick mode
configures Local only.

## Optional observability

Leaving `OCSERV_OTEL_BACKEND_ENDPOINT` unset or empty disables OTLP export and
omits the OpenTelemetry Collector, keeping the default topology of gateway ->
control plane -> PostgreSQL and transportd -> managed nodes alongside a
separate backup worker. Setting a nonempty endpoint activates the
`observability` Compose profile and adds control plane -> Collector (OTLP 4317)
-> operator backend (mTLS). Before enabling that endpoint, provision
`otel-client.crt`, `otel-client.key`, and `otel-ca.crt` inside
`OCSERV_SECRET_DIR` with launcher ownership and mode `0444`; missing files or
invalid permissions fail closed before startup, whereas disabled OTEL does not
require them.

If OTEL is later disabled, the next activation stops and removes any existing
Collector container in the same project, and `down` also includes the optional
Collector. Because the launcher ignores inherited `COMPOSE_PROFILES`,
`COMPOSE_ENV_FILES`, and automatic Compose `.env` files in favor of the
exported lifecycle configuration, enable observability through the endpoint
setting and never by selecting a Compose profile manually. Both release
manifest schemas include `otel` for later opt-in; their image inventories are
listed under [Release manifests](#release-manifests).

The optional OTEL layout lives inside the target release's production
deployment files. During lifecycle transitions, the runner resolves the
verified target source directly without requiring its descriptor to equal the
currently active one, though this neither converts historical layouts nor
guarantees that an arbitrary target can run against existing state.

## Production secrets

All prebuilt `OCSERV_*_IMAGE` settings must use explicit `vX.Y.Z` /
`vX.Y.Z-rc.N` tags or SHA-256 references. Store secrets outside the checkout in
a canonical absolute `OCSERV_SECRET_DIR` whose ancestor directories are owned by
root or the launcher user without group or world write permission. The launcher
rejects missing files, symlinks, and ownership or mode mismatches, and
credentials must never be placed in Compose environment values.

| Path or file class | Host owner | Mode / additional checks |
| --- | --- | --- |
| `OCSERV_SECRET_DIR` | Launcher | `0700`; private parent prevents host traversal |
| General secrets, including session, OIDC and enabled OTEL files | Launcher | `0444`; only explicitly mounted services receive them |
| `controller-command-signing-key.pem`, `audit-event-key` | `65534:65532` | `0400`; audit loader also rejects hard links and unsafe container ancestry |
| `controller-iroh.key`, `relay-access-token` | `65532:65532` | `0400` |
| `controller-command-verification-key.pem` | `0:65532` | `0440`, one-link regular file |
| Optional `relay-ca.pem` | `0:0` | `0444`, nonempty one-link regular file |
| `OCSERV_BACKUP_DIR` | `999:999` | `0700`, no symlinks |

Because file-backed Compose secrets are host bind mounts, host file ownership
matters even when the Compose descriptor declares a target container owner.
Provision `audit-event-key` as an independent 32-byte key encoded in lowercase
hex, never reusing the audit checkpoint key, and set `OCSERV_AUDIT_EVENT_KEY_ID`
to its stable, non-secret identifier stored with events. The gateway files
`tls.crt` and `tls.key` are required in `OCSERV_SECRET_DIR` only when
`OCSERV_TLS_MODE=manual` (the default) is active; Integrated ACME omits them.
Integrated Relay and Signer files have additional
[secret/state requirements](../../deploy/production/integrated/README.md#lifecycle-configuration).
Manual installations must prepare the files in this table before running the
installer, whereas Quick mode generates the Controller files above with these
owners and modes (except the TLS pair obtained by ACME) along with the Signer
files, while leaving OIDC and OTEL unconfigured.

When the Relay uses a private CA, provision the public PEM bundle at
`${OCSERV_SECRET_DIR}/relay-ca.pem` as a nonempty, one-link `root:root` regular
file with mode `0444` and the same safe directory ancestry. `compose.sh`
includes the shipped `compose.relay-ca.yaml` overlay only when this file exists
and mounts it into transportd alone; unsafe files fail closed in the launcher,
and invalid PEM causes transportd to refuse startup. Omitting `relay-ca.pem`
keeps the existing public root trust (TLS is never disabled), and providing it
adds trust roots without enabling certificate pinning. Distribute the same CA
to nodes through the
[managed-node trust path](../getting-started/managed-node.md#1-prepare-the-node-configuration)
for enrollment and the Agent service without patching launchers or rendered
descriptors. Rotating or removing `relay-ca.pem` is an explicit operator trust
change that install and upgrade runs never perform automatically.

Manual installations must also generate the command signing key pair outside
the checkout before installing or upgrading to this descriptor (Quick mode
generates it automatically). Place the private key in `OCSERV_SECRET_DIR` and
write its Ed25519 SPKI public key to `controller-command-verification-key.pem`
in the same directory as a one-link regular file owned by `0:65532` with mode
`0440`. Only this public key is mounted into `transportd` to verify connection
fences; `transportd` must never receive the private key. Rotate the
verification key together with the matching signing key, and distribute the
same public key to Agents through the node provisioning channel in
[command authorization](../development/command-authorization-v1.md). Missing,
unreadable, or mismatched verification keys fail closed, and fencing must never
be removed to force a node connection. During rollback, the deployment must
satisfy the target release's actual secret and mount requirements, as descriptor
equality is not an admission gate.

Create the backup bind mount for the non-root PostgreSQL UID before startup, as
the launcher rejects missing, symbolic-link, incorrectly owned, or overly
permissive paths:

```bash
sudo install -d -o 999 -g 999 -m 0700 "$OCSERV_BACKUP_DIR"
```

### PostgreSQL initialization updates

During first-time setup of bundled PostgreSQL, role initialization scripts read
passwords from standard input. Those scripts come from the selected release
checkout's `postgres-init` directory and run inside the official PostgreSQL
container only when the data directory is empty; replacing a Controller image
or Agent package does not re-run them. Never delete a database volume or
rebuild a database just to rerun initialization. Evaluate any historical
credential exposure separately and follow
[credential rotation](incident-recovery.md#postgresql-credential-rotation) when
needed.

## Release manifests

Release manifests use either the v1 or v2 schema. The v1 schema supports
standalone installations with six image roles: `gateway`, `control`,
`transport`, `backup`, `postgres`, and `otel`. The v2 reader keeps the same
per-architecture filenames and protected local file rules while adding
`signer_state_version: 1` and four additional image roles (`edge`, `relay`,
`signer`, and `mysql_backup`), for ten roles in total. Integrated deployments
require v2 (see
[Integrated configuration](../../deploy/production/integrated/README.md#lifecycle-configuration)),
and v2 selects backend backup image references directly from the manifest
configuration instead of reading `OCSERV_DATABASE_BACKUP_IMAGE`. Across both
schemas, eight first-party GHCR images use explicit `vX.Y.Z` tags, while
PostgreSQL and OpenTelemetry retain pinned third-party references.

Formal GitHub Releases publish the architecture-specific Controller manifests
`controller-release-amd64.json` and `controller-release-arm64.json` alongside
the Agent assets, a byte-identical `controller-release.json` alias of the amd64
manifest for existing operators, and the versioned Stage-1 entrypoints
`controller-bootstrap.sh` and `managed-node-bootstrap.sh` for HTTPS download.
Native builds and assembled version images cover `linux/amd64` and
`linux/arm64`, with each manifest recording its target platform and image map.
First-party packages must remain public so installations can pull them without
registry credentials; publishing credentials and package visibility remain
repository administration concerns. Before any Compose activation, the
lifecycle entrypoint queries the Docker daemon for its server architecture and
fails closed when the manifest platform disagrees with the host, so always
install the manifest variant matching the Docker host platform.

The architecture-specific manifest (`controller-release-<arch>.json`, which
`install.sh` downloads into `release-bundles/<tag>/` under the state root) is
ordinary deployment configuration: download it over HTTPS and keep it in a
protected directory as a regular root- or launcher-owned file without symlink
ancestry or group and world write permission. Before running Compose config,
pull, or activation, the lifecycle validates the manifest's JSON structure,
architecture, source checkout, and image mapping, without requiring a release
public key, signature, signed checksum manifest, or provenance evidence. Later
`rollback` and `start` runs use the copies saved in protected local lifecycle
state and do not need the original download.

## Host and configuration contract

| Host | Architecture | Bootstrap policy |
| --- | --- | --- |
| Ubuntu 22.04 / 24.04 / 26.04; Debian 12 / 13 | amd64, arm64 | Installs missing prerequisites and Docker if absent |
| Debian 11 | amd64, arm64 | Bootstrappable; regular LTS ended 2026-08-31, arrange ELTS or equivalent maintenance |
| Ubuntu 20.04 | amd64, arm64 | Existing compatible Docker only; arrange ESM or equivalent maintenance |

Running `bootstrap-host.sh check` performs a read-only inspection, while its
`install` command uses apt to install missing `jq`, `flock`, `curl`, `openssl`,
and CA certificates. When Docker is absent on a bootstrappable host, `install`
configures Docker's distribution-specific official apt repository; it preserves
compatible existing Docker installations and aborts without uninstalling
anything if it finds conflicting runtimes or an unavailable Compose v2 plugin.
The Docker engine must support `docker compose up --wait`.

The lifecycle launcher user is either the bootstrap's sudo-invoking user or
root for a root lifecycle, and that account must already be able to reach the
Docker daemon. Because bootstrap never alters group membership, socket
permissions, firewall rules, or Docker TCP listeners, fresh hosts without
Docker require passing `--root-lifecycle` to the installer or preparing Docker
access beforehand. The root lifecycle path forwards only allowlisted settings
and strips `SUDO_USER`; never substitute `sudo -E`.

By default, the state root is `/var/lib/ocservia-controller`, which must be a
canonical absolute path with mode `0700`, owned by the launcher user, under
root- or launcher-owned parent directories without group or world write
permission. Bootstrap can also create `OCSERV_BACKUP_DIR` with owner `999:999`
and mode `0700`, but if existing paths have wrong ownership or permissions,
bootstrap reports them and exits without repairing them in place. Secrets,
trust material, and ongoing host security maintenance remain
operator-provisioned.

During a manual installation, the installer loads `./install.env` from its
working directory, while explicit exported settings (including empty values)
override that file; Quick mode ignores `./install.env` and writes its own
`/etc/ocservia/install.env`. Subsequent lifecycle and Compose commands do not
reload that file and require the same effective exported environment, while
lifecycle commands restore image settings from their selected manifest. See the
complete [first-install procedure](../getting-started/production.md) for
configuration, secrets, host preparation, and activation.

## Lifecycle state and failure recovery

Command examples live in
[Controller lifecycle](../how-to/controller-lifecycle.md). Use `controller.sh`
(implemented in [controller.sh](../../deploy/production/controller.sh) and
[compose.sh](../../deploy/production/compose.sh)) for install, upgrade,
rollback, start, and uninstall; the versioned bootstrap script is only a
first-install entrypoint, and rerunning an unpinned Stage-0 script is not an
upgrade procedure.

Running `install` requires Docker Compose v2 with `docker compose up --wait`
support. After acquiring an exclusive local lock in
`/var/lib/ocservia-controller` and rejecting any existing
`current-release.json`, `install` verifies that the checkout HEAD and clean
working tree match the manifest's `source_commit`, runs the guarded Compose
preflight, and atomically records the target and lifecycle phase in
`pending-release.json` before activation. It then pulls the version-tagged or
SHA-256 image references, starts the dependency graph with `up -d --wait`, and
runs `deploy/production/controller-release-smoke.sh`, which checks Compose
health, probes the public HTTPS `/api/v1/readyz` and `/api/v1/version` routes
to confirm the target version and source commit, and verifies the transport
socket and backup freshness health boundaries. Only after that smoke check
passes does the lifecycle commit the manifest as `current-release.json` (and,
for upgrades, `previous-release.json`), all with mode `0600`. If an install or
upgrade fails, `pending-release.json` remains on disk with failure evidence and
permits only a retry against the identical target; during an upgrade, the
pending file also records the pre-activation current manifest so recovery can
populate `previous-release.json` before clearing completed evidence. Lifecycle
commands never create or rotate secrets, certificates, or identity keys.

Run every lifecycle command with the validated target configuration and ensure
its `source_commit` is available locally (fetching the exact commit from the
trusted repository if the checkout is shallow). The lifecycle either reuses the
caller's matching clean checkout or retains a separate clean Git checkout under
the protected state root, never modifying an existing checkout to force a
match. Compose definitions and smoke checks run from that target source,
including during later `start` and `uninstall` operations. Source checkouts
retained under the state root are not database backups and are not removed by
runtime uninstall.

An `upgrade` run first validates the confirmed current state and target
manifest, checks the target `source_commit` against a clean checkout before any
Compose operation, and verifies the current deployment's dependency and backup
health. While the current release stays online, `upgrade` renders the target
Compose configuration and pulls the target images, then executes the forward
database migration and starts the `up -d --wait` dependency graph. Passing an
identical manifest is a no-op, whereas any different artifact (whether its
version string is lower, equal, or higher) follows the full execution path.
Once the release smoke check passes, `upgrade` atomically rolls the complete
manifests into `previous-release.json` and `current-release.json`. Any failure
after activation returns non-zero without redeploying old images, running down
migrations, or altering confirmed release state.

A `rollback` run reads only the protected `previous-release.json` and never
accepts an operator-selected manifest. It enforces no version ordering, equal
migration counts, or descriptor equality checks, and runs no historical
database compatibility preflight, so rollback does not guarantee that an
arbitrary previous release can operate against the existing database state.
Provided the exact target source and images remain available and satisfy the
deployment's requirements, `rollback` renders and pulls the previous image
references, starts the normal target Compose graph with `up -d --wait`
(including required forward initialization, never a database down migration or
restore), and swaps the confirmed state files only after the functional release
smoke check confirms the previous version and source commit. If rollback fails
after activation, confirmed state remains unchanged and pending failure
evidence is retained for a same-target retry without automatically redeploying
the current images. Full database restoration belongs to
[Backend-specific recovery](incident-recovery.md#database-recovery) as a
separate disaster-recovery boundary outside application rollback.

### Start and uninstall

When confirmed release state already exists, use `start` (never `install`) to
bring the services up: `start` resolves the image inventory and exact source
from `current-release.json`, runs the target Compose graph with `up -d --wait`
and release smoke verification, and leaves confirmed state unchanged. Keep the
production environment, secrets, and backup directory available.

Running `uninstall` acquires the lifecycle lock and executes protected Compose
`down`, retaining named database, transport, and trust volumes, external
databases, confirmed manifests, source checkouts, backups, and secrets so that
repeating a completed uninstall is safe. Adding `--purge-data` in Standalone
mode removes the production project volumes and local release state after a
clean shutdown (Integrated mode rejects `--purge-data`), while keeping the
lock, secrets, backup directory, external databases, off-host backups, source
checkouts, container images, and unrelated volumes intact. Volume purge is
neither a secure erase nor a database recovery mechanism. Both uninstall modes
refuse to run while a pending transaction exists, leave confirmed state and
data untouched if shutdown fails, retain lifecycle state if volume purge fails,
report residual paths if state cleanup fails, and never re-enter Stage-0.

During startup, the target's owner-only database initialization validates
supported SQL artifact checksums and execution receipts, serializes migrations,
and propagates any SQL or partial-execution failure. Although there is no
software-version or schema-range admission gate, unsupported epochs, unknown or
noncontiguous receipts, and invalid SQL content still fail closed. Readiness
probes verify current core reads, database permissions, and event streams
without certifying historical database compatibility; follow
[SQL artifact contracts](../development/control-plane.md#current-sql-artifacts-and-bounded-upgrades)
and [backend recovery](incident-recovery.md#database-recovery), and never edit
receipts to force startup.

Across the lifecycle, five acceptance boundaries are tracked with separate
evidence, and passing one does not establish the others: Compose container
health and dependency readiness, functional release identity from the smoke
check, explicit target application and failure-state handling, current database
initialization, permissions, and business behavior, and disaster recovery
through the selected backend's documented procedure.

## Backup and credential operations

Retention rules, backup-role connection files, off-host copies, and isolated
restore verification are documented in
[database backup and restore](database-backup-restore.md). Because neither
replacing a container image nor updating a secret file on disk changes a
password stored inside the database, follow
[PostgreSQL credential rotation](incident-recovery.md#postgresql-credential-rotation)
to update database password verifiers and Compose secret files together.

## Relay and application networking

Standalone deployments use one independently deployed HTTPS Relay:

```bash
export OCSERV_RELAY_URL_A=https://relay.example.com
export OCSERV_RELAY_URL_B=
```

Here A is required and B must be absent or empty; `install.sh`, `compose.sh`,
and the transportd wrapper all reject a nonempty B before installation or
process startup. Because exactly one dedicated Relay is supported without
Relay failover, B does not provide optional redundancy, and recovering from a
Relay loss requires restoring that original Relay at the same address with its
existing trust material. In Standalone mode, set these variables in
`install.env` without editing release files. Integrated mode derives its Relay
URL from its public host and rejects a conflicting A or any nonempty B; use
[its configuration contract](../../deploy/production/integrated/README.md#lifecycle-configuration)
instead of overriding standalone Relay settings. When migrating an existing A/B
deployment, deliberately clear B on both the Controller and Agents while
preserving identity and trust material, then restart and verify A-only traffic
(see [dedicated Relay configuration](../how-to/dedicated-relay.md)).

Only transportd joins the non-internal `relay-egress` network, using it for DNS
and outbound HTTPS to the Relay by its HTTPS hostname (the independent Relay in
Standalone mode, or the same-host Relay via its public name in Integrated
mode). The application, database, and observability networks remain internal
and add no published Controller ports. Because the local socket healthcheck
does not test relay reachability, validate TLS, token-authenticated relay
traffic, and fresh Agent observations separately. Normal production direct
connectivity remains unchanged.

The control plane runs with `--role=all`. Public TLS terminates at the gateway
(behind Edge in Integrated mode), and node certificates come from an HTTPS
certificate signer (the external endpoint in Standalone mode, or the bundled
Signer in Integrated mode). Set `OCSERV_PUBLIC_ORIGIN` to the public HTTPS
origin; when OIDC is enabled, configure its redirect URI as
`https://$OCSERV_PUBLIC_HOST/api/v1/auth/callback` with an origin matching
`OCSERV_PUBLIC_ORIGIN`.

### Gateway response headers

Both gateway configurations, [`Caddyfile`](../../deploy/production/Caddyfile)
(Standalone) and
[`integrated/site.caddy`](../../deploy/production/integrated/site.caddy)
(Integrated), emit HSTS, `nosniff`, `X-Frame-Options: DENY`,
`Referrer-Policy: no-referrer`, and the enforced `Content-Security-Policy`
(`default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors
'none'; form-action 'self'`) on every response. No report endpoint is
configured, so blocked loads appear only in the browser console, and passing
browser specs on exercised paths does not prove the absence of XSS across the
application. Because responses generated directly by Caddy for handler errors
(such as a 502 when the control plane is unreachable) skip the site's deferred
`header` block, each file repeats the headers inside `handle_errors`. Keep all
four header blocks identical;
[`test-controller-gateway-headers.py`](../../scripts/test-controller-gateway-headers.py)
verifies both static and 502 responses across both configurations.

If CSP enforcement breaks a page, roll back at the release level instead of
editing an installed copy, since the Standalone Caddyfile is built into the
gateway image and Integrated mode mounts its gateway files from a release
checkout that the lifecycle requires to stay clean. Either run
[Controller rollback](../how-to/controller-lifecycle.md#rollback) to return to
a previous Report-Only release, or upgrade to a release built from a commit
that renames only this header to `Content-Security-Policy-Report-Only` across
all four blocks while keeping the remaining headers intact.

<a id="authentication-request-budgets"></a>
### Gateway addressing and proxy trust

Request rate budgets and trusted-client-IP handling are documented in
[authentication](authentication.md#authentication-request-budgets-and-proxy-trust).
Production Compose configures the application network as `172.30.240.0/24` with
dynamic container allocations restricted to `172.30.240.128/25`, assigns the
gateway the static address `172.30.240.2`, and defaults
`OCSERV_AUTH_TRUSTED_PROXY_CIDRS` to that `/32`. Combining Compose's
[static service address and IPAM configuration](https://docs.docker.com/reference/compose-file/services/#ipv4_address)
in this way prevents other containers from taking `172.30.240.2` while the
gateway is down and ensures that recreating the gateway preserves its trusted
source address without relying on a previously observed dynamic IP. Never trust
the entire application subnet or publish the internal Controller port.

When `172.30.240.0/24` overlaps host or VPN routes or another Docker network,
set `OCSERV_APPLICATION_SUBNET`, `OCSERV_APPLICATION_IP_RANGE`, and
`OCSERV_GATEWAY_APPLICATION_IP` together in the Controller environment or
`install.env`, keeping the gateway IP inside the subnet, outside the dynamic
range, and distinct from the bridge gateway. The default trusted `/32`
follows `OCSERV_GATEWAY_APPLICATION_IP` automatically; an explicit
`OCSERV_AUTH_TRUSTED_PROXY_CIDRS` overrides that default and must be updated
whenever the static gateway IP changes (an explicitly empty value trusts no
proxy, while Integrated mode rejects any value other than the gateway's own
`/32`).

Automatic detection and conversion of legacy application networks have been
removed. Changing an existing network layout requires an explicit operator
redeployment, as the lifecycle never removes existing networks or attached
services to force a new layout to fit. If a target release cannot start with
the existing deployment, preserve durable state and follow
[incident recovery](incident-recovery.md), noting that the absence of an
upfront compatibility rejection does not prove cross-version state is safe.

## Runtime boundaries and verification

The reference Controller bounds shared SSE fan-out to 128 global connections, 8
per identity, 4 per session, 32 per workspace, 16 per resource, and 64 active
watchers, reserving PostgreSQL and HTTP capacity for non-streaming API work in
the P1 single-VM profile; raising any of these limits requires rerunning
`make p1-full`. Because the gateway serves only as defense in depth, do not
remove application admission checks, session revalidation, bounded queues, or
watcher backoff. While an active durable-event watcher is in database backoff,
`/readyz` reports unavailable; alert on `sse_unhealthy_watchers` and verify
cursor catch-up instead of repeatedly restarting a healthy fan-out loop.

Privileged node commands require root-authenticated privd key enrollment, key
rotation state, receipt evidence, and certificate receipt bindings. Deploy
Controller support first, register each root-owned key with its one-time
credential, approve `privd_result_attestation_v1`, and then upgrade privd and
the Agent. Until that rollout completes, the node remains readable, while
privileged certificate, secret, configuration, service, user, and session
mutations fail closed without ever falling back to a legacy unattested path.

The production containers run under separate unprivileged service identities:
Controller UID 65534, transportd UID 65532, and shared socket GID 65532. Keep
Compose's `OCSERV_TRANSPORT_UID`/`OCSERV_TRANSPORT_GID` settings and
transportd's `--control-plane-uid`/`--control-plane-gid` flags aligned with
those IDs. Before starting application containers, the production launcher
stops the Controller and transportd and runs a root-owned, network-disabled
runtime initializer that accepts only a fresh root-owned Docker volume, the
current `65532:65532 0750` transport volume, or the exact legacy
`65532:65532 0770` state. It seals the directory before inspecting it, removes
only a trusted stale transport socket, and finishes at mode `0750`. Every
service mounts this volume with copy-up disabled so the container runtime
cannot overwrite the validated ownership or mode from image data, and any
unexpected owner, entry, link, or mode fails closed. Never invoke Compose
directly or make either socket parent directory writable by the client service.

Before starting, validate the rendered Compose configuration without printing
secret contents:

```bash
deploy/production/compose.sh config --quiet
```

Before a major database cutover, first upgrade existing environments to the
stable v1.2.0 checkpoint at `169102557cd610847c9f6ac2083336cdcf82c483`, confirm
its epoch-1/revision-0 receipt, and retain a verified backup, because the
epoch-2 binary cannot replay earlier migrations. Keep the production audit
event and checkpoint keys intact: historical audit events remain business data,
and startup verifies the chain and refuses an unauthenticated legacy tail
unless that exact tail is checkpointed. Never rewrite audit evidence or schema
receipts to bypass either check. Stop old writers before migration and use the
normal owner migration process, starting the new runtime only after a
verified cutover succeeds; if rollback is required, restore the compatible
backup with the old binary instead of attempting a schema downgrade.

After activating the deployment through the lifecycle tooling, verify
`/api/v1/readyz`, an authenticated read, a node connection through the
configured relay, OTLP delivery when enabled, and an isolated restore from the
newest backup. Never expose PostgreSQL, internal Unix sockets, Docker sockets,
or host `/proc` and `/sys` mounts.
