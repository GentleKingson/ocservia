# Production deployment

This manual covers a production Controller and its dedicated Relay. It follows
the order of a deployment: support scope, the choice between Quick and manual
installation, configuration and materials, installation, verification, upgrade,
rollback and uninstall, the Relay and its networking, optional bootstrap launcher
bootstrap hosting, and troubleshooting. Standalone and Integrated deployments
share the same steps. Where they differ, a conditional paragraph or the
[comparison table](#deployment-combinations) says so.

Related manuals: [Production authentication](authentication.md) for login modes
and the first administrator, [Agent lifecycle](agent-lifecycle.md) for managed
nodes, and [Production incident recovery](incident-recovery.md) for backup,
restore and recovery procedures.

Deploy an exact published release that has passed Release Check. The commands
below use `vX.Y.Z` as the release tag. See [Version policy](#version-policy)
for the tags they accept.

The Controller serves the administrative Web console and the HTTPS API, and it
stores cluster state and audit records in a supported relational database. It
talks to managed nodes through the dedicated Relay. It also coordinates its own
install, upgrade, rollback, and uninstall lifecycles.

## Support scope

### Supported platforms

| Surface | Supported scope | Details |
| --- | --- | --- |
| Controller OS/architecture | Linux `amd64` and `arm64` release artifacts | Host table below |
| Agent native packages | DEB on Ubuntu 22.04/24.04/26.04 and Debian 12/13, RPM on Rocky Linux 9, `amd64`/`arm64` | [Install a managed node](agent-lifecycle.md#install-a-managed-node), [Agent lifecycle](agent-lifecycle.md), [package validation](../development/release-checks.md) |
| Databases | PostgreSQL 18.x bundled or external; MySQL 8.4 LTS external | [Database support](#database-support) |
| Managed ocserv | Adapter-admitted `1.2.x`, `1.3.x`, `1.4.x`, `1.5.0`, and `1.6.0` | See the note below |
| Relays | Dedicated relay hosts running the matched vendored `iroh`/`iroh-relay` release | [Dedicated Relay](#dedicated-relay) |
| Authentication | Local only, OIDC only, or Local + OIDC | [Production authentication](authentication.md) |
| Web console browsers | Chrome 111+, Safari 16.4+, Firefox 128+, Chromium 111+ derivatives; CI runs Chromium plus Playwright Firefox and WebKit on the cross-engine specs | [Web validation](../development/release-checks.md#web-validation) |

On managed nodes, the ocserv adapter and Controller configuration planner admit
`1.2.x`, `1.3.x`, `1.4.x`, and exactly `1.5.0` and `1.6.0`. Full release
acceptance (native adapter integration plus real-VPN business validation)
remains pinned to `1.5.0`. The `1.6.0` admission has separate
[compatibility coverage](../development/release-checks.md#ocserv-160-compatibility),
while the older lines have parser regression fixtures without separate per-line
release certification. Future versions, including `1.5.1`, `1.6.1`, and
`1.7.0`, require explicit compatibility review before admission. New database
patch releases or OS distributions are adopted only after artifact and
migration verification.

Controller hosts:

| Host | Architecture | Bootstrap policy |
| --- | --- | --- |
| Ubuntu 22.04 / 24.04 / 26.04; Debian 12 / 13 | amd64, arm64 | Installs missing prerequisites and Docker if absent |
| Debian 11 | amd64, arm64 | Bootstrappable; regular LTS ended 2026-08-31, arrange ELTS or equivalent maintenance |
| Ubuntu 20.04 | amd64, arm64 | Existing compatible Docker only; arrange ESM or equivalent maintenance |

### Deployment capability distinctions

Every supported production deployment runs a single Controller instance, one
relational database, and one dedicated Relay (either on separate hosts in
Standalone mode or co-located in Integrated mode, with bundled PostgreSQL or
external PostgreSQL/MySQL), managing one or more node Agents. Controller high
availability, multi-relay failover, database replication clusters, automatic
PostgreSQL failover, and point-in-time recovery (PITR) are outside the
supported scope. Controller rebind moves nodes between two independent
single-Controller deployments without providing active-passive HA. See
[resilience and capacity](../development/release-checks.md#resilience-and-capacity).

Operational readiness distinguishes three pairs of states that are easy to
conflate:

1. Single-relay recovery versus multi-relay redundancy: production supports a
   single dedicated Relay. If that Relay goes offline, relay-routed control
   connections pause until the same Relay host and identity recover and
   pending commands are reconciled; automatic failover to a second Relay is not
   supported (see [Dedicated Relay](#dedicated-relay)).
2. Package installation versus active online management: installing the node
   package and leaving its services stopped does not put a host under fleet
   management. A node is actively managed only after completing enrollment,
   receiving Controller approval, starting the matched `Agent` and `privd`
   daemons, and establishing an authorized transport session (see
   [Enroll the node](agent-lifecycle.md#enroll-the-node) and
   [Agent lifecycle](agent-lifecycle.md)).
3. Backup creation versus verified restoration: scheduled backups produce
   PostgreSQL base backups or MySQL logical dumps. Restoration is a separate
   procedure that must be validated in an isolated environment. Neither a
   completed backup job nor a passing restore-verifier check substitutes for
   owner-role schema migration admission, and redirecting a live Controller to
   a restored MySQL instance requires a manual cutover (see
   [Database backup and restore](incident-recovery.md#database-backup-and-restore)).

### Version policy

The v1.1.0 policy took effect with the
[`v1.1.0` release](https://github.com/GentleKingson/ocservia/releases/tag/v1.1.0).
The `v1.1.0` release notes link the acceptance and publication runs for that
tag. Every subsequent candidate requires its own validation run. This policy
reset supersedes the version windows, deprecation schedule, and additive-only
`1.x` compatibility promises of the archived 1.0 policy. It leaves previously
published git tags, release artifacts, and historical records intact.

Starting in `v1.1.0`, the codebase removes software upgrade, downgrade, and
rollback version admission gates, historical migration compatibility probes,
automatic conversion of legacy deployments, and multi-release compatibility
matrices. The public `--schema-compatibility-check` CLI flag and the
`/readyz` response's `schema_version` field have also been removed. Readiness
probes now check zero-row reads against current core tables alongside
event-stream health, without inspecting migration history ranges.

Install, upgrade, and rollback remain explicit lifecycle commands targeting
verified release artifacts. Version strings are still validated and displayed,
but numeric version ordering is not used to admit or reject an operation.
Lifecycle commands operate on explicitly pinned release artifacts without
relying on bridge releases or overriding OS package-manager rules. Older
scripts already installed on a host retain their original behavior. Commands
that settle in an `Unknown` state must be handled through
[incident recovery](incident-recovery.md) without blind replay.

Removing software-version admission does not relax runtime security or state
invariants. Command signatures, trusted keys, Controller-authorized upgrade
digests and target architectures, negotiated command capabilities, RBAC and
independent two-principal approvals, idempotency, replay protection, owner
fences, Signer identity and monotonic revisions, and atomic database
transactions all remain mandatory. Removing legacy deployment auto-conversion
likewise means removing the automatic network, configuration, and data
mutations that accompanied it.

Database schema-source admission is still enforced: the owner migration runner
rejects unknown schema epochs, revisions, or checksums, schema drift, and
non-empty databases that lack a valid migration receipt. The only supported
prior database checkpoint is the fixed `v1.2.0` schema defined in the
[upgrade checkpoint](../reference/stable-contracts.md#upgrade-checkpoint). It
is an immutable schema baseline and does not define a minimum Controller or
Agent runtime version. Freshly initialized databases and databases upgraded
from the `v1.2.0` checkpoint must converge to the exact same schema while
retaining their respective execution receipts. Empty-database initialization
never overwrites an existing database.

Release acceptance validates the current candidate across the supported
architectures, deployment modes, and database engines through security,
integration, and resilience suites. Beyond the single bounded `v1.2.0` database
schema transition, historical upgrade, downgrade, and migration paths are not
tested. Running mismatched component versions can fail or corrupt state even
without a version-admission gate. Operators must never reverse database
migrations, reset node or Signer identities, or overwrite persistent state to
force a rollout, and changing deployment topologies requires a clean
redeployment. The manual installer sets up a fresh deployment and does not
convert historical layouts.

The [published 1.0 policy](https://github.com/GentleKingson/ocservia/blob/d420b22018596d6741d55fa56bd19c4a767e5817/docs/reference/support-policy.md)
remains in git history as the record of the original 1.0 line, without
governing `v1.1.0` or later releases. Nothing in this policy certifies an
unreleased commit or an unexecuted validation suite for production use.

The `v1` line is end of life: it receives no further patch releases, security
backports, compatibility matrices or new migration paths, and `v1.2.0` remains
relevant only as the fixed database checkpoint described above. `v2.0.0`
release candidates are prereleases for evaluation and do not constitute a
stable supported production release; see [SECURITY.md](../../SECURITY.md),
which also holds the security-fix support line.

Release tags follow `X.Y.Z` for stable releases and `X.Y.Z-rc.N` (with a
positive integer `N` and no leading zeros) for release candidates. The install,
upgrade and rollback commands accept an exact `vX.Y.Z` or `vX.Y.Z-rc.N` tag, but
release candidates are not recommended for stable production use. RC and
stable candidates pass the same [Release Check](../development/release-checks.md)
before tagging. Promoting an RC to a final release creates a separate `X.Y.Z`
tag and GitHub Release while retaining the RC tags in history.

### Database support

At startup, the production launcher pairs `deploy/production/compose.yaml` with
a single database descriptor. The default bundled PostgreSQL 18 configuration
runs the HTTPS gateway, control plane, transport service, PostgreSQL server,
and backup worker together, keeping database, application, and observability
traffic on internal Docker networks. Its Compose descriptor sets
`OCSERV_DATABASE_BUNDLED_POSTGRES=true`. This is the only declaration under
which a production PostgreSQL URL without `OCSERV_DATABASE_TLS_CA_FILE` may
target the `postgres` service (otherwise only loopback is accepted).

External database deployments attach database client containers to the
dedicated non-internal `database-egress` network, and ocservia publishes no
database port. External PostgreSQL 18 requires `sslmode=verify-full` with
`database-ca.pem` across owner, runtime, and backup connections. External MySQL
8.4 LTS requires verified TLS and uses backend-specific logical backups. Bundled
MySQL is rejected.

| Backend | Deployment | Production support |
| --- | --- | --- |
| PostgreSQL 18 | bundled or external | Yes; bundled remains the default |
| MySQL 8.4 LTS | external only | Yes |
| MySQL | bundled | No |

Support for PostgreSQL 17 and MariaDB has been removed. Application schema
migrations never upgrade the database engine or its physical data format, so
operators running PostgreSQL 17 must complete a controlled major upgrade via
`pg_upgrade` or dump/reload before starting this Controller, preserving a
verified backup and rollback copy throughout. PostgreSQL 18 containers mount
their named volume at `/var/lib/postgresql` with
`PGDATA=/var/lib/postgresql/18/docker`. Never mount an existing PostgreSQL 17
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
PostgreSQL HA or PITR claim applies to MySQL.

### Security review posture

- Source dependency advisories (`govulncheck` for Go, `cargo audit` and
  `cargo deny` for Rust including the relay lockfile, and `npm audit` for Web)
  along with repository secret scanning run in the scheduled Security Checks
  workflow and must pass on the release candidate.
  [SECURITY.md](../../SECURITY.md) covers vulnerability reporting and supported
  versions.
- In Release Check and in release builds, `build-controller-images` scans every
  built Controller image archive on both native architectures (`amd64` and
  `arm64`) using pinned Syft and Grype versions against a freshly updated
  vulnerability database before running image smoke tests or uploading release
  assets (temporary scan databases are discarded after the job). Any High or
  Critical OS vulnerability with an upstream fix fails the build unless an
  unexpired entry in
  [`deploy/production/image-scan-exemptions.json`](../../deploy/production/image-scan-exemptions.json)
  matches the exact image, package name, installed version, CVE ID,
  digest-pinned base image, and documented rationale. Unfixed findings remain
  visible in the CI logs. A failure on either architecture blocks release
  publication.
- Updating pinned database images or patch levels requires migration,
  role-permission, SQL-artifact, and backup/restore verification under
  [stable contracts](../reference/stable-contracts.md#database-and-recovery-matrix)
  before replacing the existing pins.

## Deployment combinations

| Choice | Standalone (default) | Integrated |
| --- | --- | --- |
| Install path | Manual only | Manual, or Quick (new host) |
| Lifecycle user | Launcher with Docker access, or `--root-lifecycle` | Root: `--root-lifecycle`; `compose.sh` rejects non-root |
| Manifest / Compose | v1 or v2 manifest; `docker compose up --wait` | v2 manifest only; Compose >= 2.24.4 |
| DNS names | Controller name | Two distinct lowercase names: `OCSERV_PUBLIC_HOST`, `OCSERV_RELAY_PUBLIC_HOST` |
| Controller / Relay TLS | `tls.crt` and `tls.key` in `OCSERV_SECRET_DIR`; ACME is not supported | Manual: those files in `OCSERV_SECRET_DIR` and `OCSERV_RELAY_SECRET_DIR`; or `OCSERV_TLS_MODE=acme` with `OCSERV_ACME_EMAIL` (Quick always uses ACME) |
| Relay | Operator-run HTTPS Relay: `OCSERV_RELAY_URL_A` required | Bundled single Relay; URL derived from `OCSERV_RELAY_PUBLIC_HOST` |
| Signer | External HTTPS endpoint in `OCSERV_CERTIFICATE_SIGNER_URL` plus `certificate-signer-token`, on a [private listener](#signer-runtime-contract) | Bundled Signer at `https://signer:9443/sign`; manual: operator-prepared Signer files; Quick: generated |
| Published ports | TCP 443 | TCP 443 (Edge) and UDP 7842 (Relay) |
| `uninstall --purge-data` | Supported | Refused |

Both Standalone and Integrated deployments support only one dedicated Relay and
require `OCSERV_RELAY_URL_B` to be empty. In a manual installation, the
database backend (bundled or external PostgreSQL 18, or external MySQL 8.4 LTS)
and the login mode (Local only, OIDC only, or Local + OIDC) can be chosen
independently of the deployment mode. Quick mode fixes bundled PostgreSQL and
Local-only login. At first install, the chosen deployment mode, database
backend, and TLS mode are recorded in `deployment-profile.json` and cannot be
changed on an existing installation.

The default `standalone` mode connects to a Relay and a Signer hosted elsewhere.
The `integrated` mode runs the SNI-routing Edge, the Relay, and the production
Signer on the Controller host, managed by the same installer and lifecycle
commands. Running these services on one host does not provide high
availability.

### Manual and Quick prerequisites

The manual path and Quick mode start from different prerequisites and are not
interchangeable.

| Aspect | Manual installation | Quick mode |
| --- | --- | --- |
| Release | Any exact stable or RC release | A Release that ships `deploy/production/quick-install.sh` |
| Host | New host or an existing deployment's lifecycle | New host only |
| Configuration | You write `./install.env` | Ignores `./install.env`; writes `/etc/ocservia/install.env` |
| Secrets and keys | You provision every protected file | Generated, including Signer material |
| Choices | Any supported combination above | Integrated, bundled PostgreSQL, ACME, Local authentication |
| First administrators | One-shot Local bootstrap, or OIDC | `initial-admin` and `initial-approver` |

## Prepare the configuration

In a manual installation, the operator prepares `install.env` and all protected
secret material. A manual installation never generates production passwords,
private keys, certificates, relay tokens, or signing keys. Only
[Quick mode](#quick-mode) generates initial material, and only for a new host.

### Host preparation

A manual installation needs:

- A supported `amd64` or `arm64` Linux host (see [Supported platforms](#supported-platforms)).
- Git, `curl`, and Docker Engine with the Compose v2 plugin. The installer can
  install Docker on most supported distributions, but on Ubuntu 20.04 you must
  install a compatible Docker yourself before you run the script.
- A DNS name and HTTPS certificate for the Controller. Integrated mode needs a
  second, distinct DNS name for its co-located Relay. With Integrated ACME
  enabled, it can obtain both certificate pairs automatically.
- An authentication mode: Local only, OIDC only, or Local + OIDC (an OIDC
  identity provider and client credentials are required only when SSO is
  enabled).
- In Standalone mode, an external certificate Signer endpoint and one dedicated
  HTTPS Relay. In Integrated mode, both run on the Controller host.
- Protected host directories for secrets and database backups.

Running `bootstrap-host.sh check` performs a read-only inspection. Its
`install` command uses apt to install missing `jq`, `flock`, `rename.ul`
(`util-linux`, or `util-linux-extra` on Ubuntu 26.04), `curl`, `openssl`, and CA
certificates. When Docker is absent on a bootstrappable host, `install`
configures Docker's distribution-specific official apt repository; it preserves
compatible existing Docker installations and aborts without uninstalling
anything if it finds conflicting runtimes or an unavailable Compose v2 plugin.
The Docker engine must support `docker compose up --wait`.

The lifecycle launcher user is either the bootstrap's sudo-invoking user or
root for a root lifecycle, and that account must already be able to reach the
Docker daemon. Bootstrap never alters group membership, socket permissions,
firewall rules, or Docker TCP listeners. On a fresh host without Docker, pass
`--root-lifecycle` to the installer or prepare Docker access beforehand. The
root lifecycle path forwards only allowlisted settings and strips `SUDO_USER`;
never substitute `sudo -E`.

By default, the state root is `/var/lib/ocservia-controller`, which must be a
canonical absolute path with mode `0700`, owned by the launcher user, under
root- or launcher-owned parent directories without group or world write
permission. Bootstrap can also create `OCSERV_BACKUP_DIR` with owner `999:999`
and mode `0700`. If existing paths have wrong ownership or permissions,
bootstrap reports them and exits without repairing them in place. Secrets,
trust material, and ongoing host security maintenance remain
operator-provisioned.

### The install.env file

Check out the pinned release tag and create a configuration directory outside
the repository tree:

```bash
git clone --branch vX.Y.Z --single-branch --depth 1 \
  https://github.com/GentleKingson/ocservia.git ocservia-vX.Y.Z
mkdir ocservia-install && cd ocservia-install
cp ../ocservia-vX.Y.Z/install.env.example install.env
editor install.env
```

Populate the Controller section of `install.env` and leave the managed-node
section commented out or deleted. See `install.env.example` for the complete
variable list. At a minimum, configure:

| Setting group | Examples |
| --- | --- |
| Public address | `OCSERV_PUBLIC_HOST`, `OCSERV_CONTROLLER_PUBLIC_URL`, `OCSERV_HTTPS_ADDRESS` |
| Login | `OCSERV_LOCAL_AUTH_ENABLED`, `OCSERV_PUBLIC_ORIGIN`, `OCSERV_SESSION_TTL`; add OIDC settings when SSO is enabled |
| External services | `OCSERV_CERTIFICATE_SIGNER_URL` |
| Controller identity and relays | `OCSERV_CONTROLLER_ENDPOINT_ID` and required `OCSERV_RELAY_URL_A`; leave `OCSERV_RELAY_URL_B` unset or empty (only one dedicated Relay is supported, and a non-empty B value is rejected) |
| Protected storage | `OCSERV_SECRET_DIR`, `OCSERV_BACKUP_DIR`, and optional Controller state root |
| Database | Bundled or external PostgreSQL 18.x, or external MySQL 8.4 LTS |

For an Integrated deployment, set `OCSERV_DEPLOYMENT_MODE=integrated` together
with the Relay and Signer settings in
[Integrated configuration](#integrated-configuration). Leave out the external
Signer and Relay URL variables; the installer derives them itself.

For the login settings, start from the complete
[authentication mode example](authentication.md#choose-a-mode) that matches the
mode you chose. [Production authentication](authentication.md) covers Local
only, OIDC only, and Local + OIDC configuration, login behavior, and one-shot
first-admin creation. Every mode requires a protected session-key file. OIDC
remains optional when Local login is enabled, and `compose.sh` selects the OIDC
overlay only when configured. Manual installations may use any of the three
login modes. Quick mode configures Local only.

Keep `install.env` restricted to the operator account and out of version
control. The installer parses it as a strict key-value allowlist. It is data,
not a shell script, so do not `source` it. During a manual installation, the
installer loads `./install.env` from its working directory, and explicit
exported settings (including empty values) override that file. Quick mode
ignores `./install.env` and writes its own `/etc/ocservia/install.env`.
Subsequent lifecycle and Compose commands do not reload either file. They
require the same effective exported environment, including database and
authentication settings, secret directories, public names, identity and runtime
trust, and lifecycle commands restore image settings from their selected
manifest. Keep that environment in the protected operator session and do not
print it into logs.

### Production secrets

All prebuilt `OCSERV_*_IMAGE` settings must use explicit `vX.Y.Z` /
`vX.Y.Z-rc.N` tags or SHA-256 references. Store secrets outside the checkout in
a canonical absolute `OCSERV_SECRET_DIR` whose ancestor directories are owned by
root or the launcher user without group or world write permission. The launcher
rejects missing files, symlinks, and ownership or mode mismatches. Credentials
must never be placed in Compose environment values.

Place these files inside the protected directories referenced by `install.env`
before running the installer:

- HTTPS server certificate and private key.
- Database owner, runtime, and backup credentials for your chosen database
  backend.
- When using external MySQL: `database-ca.pem`, separate owner and runtime DSN
  files, and `database-backup.cnf`.
- Session key and audit keys, plus the OIDC client secret when SSO is enabled.
- Controller command-signing private key.
- Certificate Signer bearer token.
- Relay access token.
- Controller Iroh identity key.

| Path or file class | Host owner | Mode / additional checks |
| --- | --- | --- |
| `OCSERV_SECRET_DIR` | Launcher | `0700`; private parent prevents host traversal |
| General secrets, including session, OIDC and enabled OTEL files | Launcher | `0444`; only explicitly mounted services receive them |
| `controller-command-signing-key.pem`, `audit-event-key` | `65534:65532` | `0400`; audit loader also rejects hard links and unsafe container ancestry |
| `controller-iroh.key`, `relay-access-token` | `65532:65532` | `0400` |
| `controller-command-verification-key.pem` | `0:65532` | `0440`, one-link regular file |
| Optional `relay-ca.pem` | `0:0` | `0444`, nonempty one-link regular file |
| `OCSERV_BACKUP_DIR` | `999:999` | `0700`, no symlinks |

Host file ownership matters even when the Compose descriptor declares a target
container owner, because file-backed Compose secrets are host bind mounts.
Provision `audit-event-key` as an independent 32-byte key encoded in lowercase
hex, never reusing the audit checkpoint key, and set `OCSERV_AUDIT_EVENT_KEY_ID`
to its stable, non-secret identifier stored with events. The gateway files
`tls.crt` and `tls.key` are required in `OCSERV_SECRET_DIR` only when
`OCSERV_TLS_MODE=manual` (the default) is active; Integrated ACME omits them.
Integrated Relay and Signer files have additional requirements in
[Integrated configuration](#integrated-configuration). Quick mode generates the
Controller files above with these owners and modes (except the TLS pair
obtained by ACME) along with the Signer files, and leaves OIDC and OTEL
unconfigured.

When the Relay uses a private CA, provision the public PEM bundle at
`${OCSERV_SECRET_DIR}/relay-ca.pem` as a nonempty, one-link `root:root` regular
file with mode `0444` and the same safe directory ancestry. `compose.sh`
includes the shipped `compose.relay-ca.yaml` overlay only when this file exists,
and it mounts the file into transportd alone. Unsafe files fail closed in the
launcher, and invalid PEM causes transportd to refuse startup. Omitting
`relay-ca.pem` keeps the existing public root trust (TLS is never disabled).
Providing it adds trust roots without enabling certificate pinning. Distribute
the same CA to nodes through the
[managed-node trust path](agent-lifecycle.md#install-a-managed-node)
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
satisfy the target release's actual secret and mount requirements, because
descriptor equality is not an admission gate.

The launcher rejects missing, symbolic-link, incorrectly owned, or overly
permissive backup paths, and any path whose ancestors are symbolic links, not
root- or launcher-owned, or group/world writable. Create the backup bind mount
for the non-root PostgreSQL UID before startup:

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

### Optional observability

Leaving `OCSERV_OTEL_BACKEND_ENDPOINT` unset or empty disables OTLP export and
omits the OpenTelemetry Collector and the monitoring TLS files. The default
topology is then gateway -> control plane -> PostgreSQL and transportd ->
managed nodes, alongside a separate backup worker. Setting a nonempty endpoint
in `install.env` activates the `observability` Compose profile and adds control
plane -> Collector (OTLP 4317) -> operator backend (mTLS).

Before enabling that endpoint, provision `otel-client.crt`, `otel-client.key`,
and `otel-ca.crt` inside `OCSERV_SECRET_DIR` with launcher ownership and mode
`0444`. Missing files or invalid permissions fail closed before startup.
Disabled OTEL does not require these files.

If OTEL is later disabled, the next activation stops and removes any existing
Collector container in the same project, and `down` also includes the optional
Collector. Enable observability through the endpoint setting and never by
selecting a Compose profile manually, because the launcher ignores inherited
`COMPOSE_PROFILES`, `COMPOSE_ENV_FILES`, and automatic Compose `.env` files in
favor of the exported lifecycle configuration. Both release manifest schemas
include `otel` for later opt-in; their image inventories are listed under
[Release manifests](#release-manifests).

The optional OTEL layout lives inside the target release's production
deployment files. During lifecycle transitions, the runner resolves the
verified target source directly without requiring its descriptor to equal the
currently active one. This neither converts historical layouts nor guarantees
that an arbitrary target can run against existing state.

### Signer and certificate materials

What the Signer issues, what the Controller stores, and the keys that stay on
managed nodes are defined in
[Certificates and Signer](../reference/stable-contracts.md#certificates-and-signer).
Its trust boundaries are described in
[Certificates, secrets and Signer](../architecture.md#certificates-secrets-and-signer).

The Controller reaches the Signer through an HTTPS client configured with
`OCSERV_CERTIFICATE_SIGNER_URL`, `OCSERV_CERTIFICATE_SIGNER_TOKEN`, and
`OCSERV_CERTIFICATE_SIGNER_TIMEOUT`. Setting `OCSERV_CERTIFICATE_SIGNER_CA_FILE` to a public PEM CA bundle
configures a private trust chain used only by this client, leaving the
process-wide trust store and other external HTTPS connections untouched. In
Integrated mode the Controller calls `https://signer:9443/sign` over verified
HTTPS with Bearer authentication and redirects disabled. That endpoint value is
already the full path, so callers must not treat it as an origin and append a
second `/sign`.

#### Signer runtime contract

The production Signer is a standalone Go module and process that issues node
certificates and seals node secrets. It does not sign Controller commands.
Build the container image with `docker build -f deploy/production/signer.Dockerfile .`.
The container executes `/ocserv-signer serve` as UID:GID `65532:65532` with a
read-only root filesystem, all Linux capabilities dropped,
`no-new-privileges`, and network attachment restricted to the internal
Controller-to-Signer network without host port bindings.

The default `:9443` listener assumes that internal network. When the Signer
runs outside the Integrated Compose network, such as the external Signer of a
Standalone deployment, bind `--listen` to a private address and allow TCP 9443
only from the Controller host. Never publish the Signer on a public interface;
its Bearer token and TLS do not replace network isolation.

Default file paths and listener settings:

| Input | Default |
| --- | --- |
| HTTPS listener | `:9443` |
| Online issuing intermediate chain, intermediate first and root last | `/run/secrets/issuer-chain.pem` |
| Online intermediate private key, unencrypted PEM | `/run/secrets/issuer-key.pem` |
| HTTPS certificate/key | `/run/secrets/tls-cert.pem`, `/run/secrets/tls-key.pem` |
| Independent API Bearer token, 32..256 non-whitespace bytes | `/run/secrets/api-token` |
| Exclusive writable state volume | `/var/lib/ocservia-signer/ledger.db` |
| Health client's public trust bundle | `/run/secrets/tls-ca.pem` |

All configured paths must be absolute canonical paths residing under real
directories owned by root or the runtime process, with no group- or
world-writable ancestor. The state directory requires mode `0700` and the
ledger file requires mode `0600` owned by the runtime process. Private key and
token files must be regular, single-link, owner-only files readable by the
runtime UID (mode `0400` or `0600` recommended) and mounted read-only.
Provision the root CA offline and never mount its private key. The startup
checks on the chain and the ledger are in
[Signer custody and idempotency](../reference/stable-contracts.md#signer-custody-and-idempotency).

First-time installation runs `init` with the same CA and state flags as
`serve` to create a fresh ledger when none exists. Do not use `init` to repair
lost state.

On the Controller, set `OCSERV_CERTIFICATE_SIGNER_URL=https://signer:9443/sign`
alongside the token-file setting and point `OCSERV_CERTIFICATE_SIGNER_CA_FILE`
to the mounted public HTTPS CA bundle. The Signer HTTPS leaf certificate must
include `signer` in its SAN list. This custom trust pool applies only to the
Signer HTTP client, so neither `SSL_CERT_FILE` overrides nor TLS verification
bypasses are needed. Omitting the custom CA setting leaves standard external
HTTPS trust in place.

The CLI check `health --url https://signer:9443/healthz` validates HTTPS with
`--tls-ca`, refuses HTTP redirects, and can target loopback only when the leaf
SAN permits it. The `/healthz` semantics are in
[Signer custody and idempotency](../reference/stable-contracts.md#signer-custody-and-idempotency).
See [Signer unavailable](incident-recovery.md#signer-unavailable) for the
recovery steps.

#### Quick install materials

For an initial integrated installation using bundled PostgreSQL and root-managed
lifecycle scripts, `deploy/production/quick-install.sh` invokes
`deploy/production/quick-materials.sh`. The script runs as root before service
activation, using `OCSERV_SECRET_DIR`, `OCSERV_SIGNER_SECRET_DIR`, and
`OCSERV_SIGNER_STATE_DIR`. It creates any missing Controller and Signer secrets
with the exact ownership and file modes verified by `compose.sh`, and it leaves
Gateway and Relay TLS identities to their own provisioning paths.

- Random secrets are generated as 32-byte hex strings, with database DSNs and
  `postgres.pgpass` derived from the generated role passwords. Controller
  command signing and `controller-iroh.key` use distinct Ed25519 keys.
- A one-shot TLS CA issues the Signer HTTPS leaf certificate (SAN `signer`,
  P-256, 5-year validity) and immediately discards its own private key,
  retaining only `tls-ca.pem`.
- The offline root CA (P-256, 20-year validity, `pathlen:1`) is generated with
  its private key encrypted under the operator passphrase supplied via
  `--root-ca-passphrase-file` or an interactive prompt, so the plaintext root
  key never touches disk. It signs the online issuing intermediate (P-256,
  5-year validity, `CA:TRUE, pathlen:0`, certificate and CRL signing usages,
  SKI), and writes `issuer-chain.pem` with the intermediate certificate
  followed by the root certificate.
- Before installing the online issuer, the script exports `root-ca.crt`,
  `root-ca.key.enc`, and `root-ca.sha256` to `/root/ocservia-root-ca-export`
  (or `--root-ca-export-dir`). In interactive mode, the operator copies those
  files off the host and confirms the final 8 characters of the fingerprint
  before the script deletes `root-ca.key.enc` locally. Under
  `--non-interactive`, the script records `root_ca_custody: delegated` and
  leaves off-host removal of `root-ca.key.enc` to the operator.

Staged files and the prepared CA (`.quick-ca`, used to resume an interrupted
run with the same CA) live under the root-only `OCSERV_SECRET_DIR`. Its
ancestors must be canonical, root-owned, and not group/world writable. Files are
published by `rename.ul` with no copy fallback, so a failed publication keeps
the prepared CA.

The script never trusts a `.quick-ca` left by an earlier version inside the
Signer-owned directory. It preserves that file and refuses to continue, and the
operator must inspect and deal with it deliberately.

Re-running the script reuses existing files and completes any interrupted
setup, including an exported root CA awaiting confirmation. Once setup
succeeds, `quick-install.json` in the Controller secret directory records the
custody mode, root fingerprint, Controller endpoint ID, and file SHA-256
digests without storing secret values. After `quick-install.json` or a Signer
ledger exists, the script refuses to regenerate material and fails closed if
any required file is missing. Signer `init` remains the authority that
validates the CA chain during startup.

#### Trusted public-key transfer

After first login, enroll and independently approve the real node, using a
separate requester and approver principal and never self-approval. Export its
two distinct public sealing keys locally and the consumed approval binding from
the Controller. Private node keys stay on the node. Pause affected node
mutations before modifying sealing-key mappings. The rules this transfer
enforces are in
[Trusted public-key transfer](../reference/stable-contracts.md#trusted-public-key-transfer).

1. On the managed node, run `scripts/export-node-sealing-keys.py` as root with
   `--node`, `--endpoint`, `--user-key`, `--user-key-id`, `--p12-key`, and
   `--p12-key-id`. The script derives the exact OpenSSL RSA public SPKI DER
   checked by privd at startup without exporting private keys. Set `umask 077`
   before redirecting its output.
2. On the Controller host, run `/usr/local/bin/ocserv-sealing-export` with
   `--database-url-file`, `--backend`, `--database-ca-file` for an external
   database or `--bundled-postgres` for bundled PostgreSQL, and the
   independently verified `--workspace`, `--node`, `--endpoint`, and
   `--approval` flags.
3. Copy both the Controller export and the node export over an authenticated
   operator channel into owner-only regular files on the Signer host. Never
   accept an approval JSON file originating from the node itself.
4. Stop Signer and run `import --approved /path/controller.json
   --public /path/node.json --workspace UUID --node UUID --endpoint HEX` with
   the configured CA and state flags, then restart Signer. Verify both the
   sealed-password and the P12 business paths.
5. Before retiring or re-enrolling a node, stop affected mutations, reconcile
   pending work, stop Signer, run `disable --node UUID`, and restart Signer.

Node retirement is a separate procedure that starts with the Controller online:
reconcile pending operations, request and independently approve retirement
through the Controller, then stop affected mutations and Signer before running
`disable` (the `signer_admin` helper is defined in
[Back up the ledger](incident-recovery.md#back-up-the-ledger)).
Restart Signer and verify both sealing purposes reject that identity.
Disable is durable and irreversible for that identity; re-enrollment requires a
new identity and approved import. The retired node must not reconnect; any
post-maintenance reconnect or command checks must use a different, non-retired
node. If the Controller returns `Revocation committed` with HTTP 503, follow
[Revocation committed with synchronization pending](incident-recovery.md#revocation-committed-with-synchronization-pending).

### Integrated configuration

Use the existing bootstrap, `install.sh`, `controller.sh` and `compose.sh`,
not a second installer. Standalone remains the default. Integrated requires a
v2 platform manifest, Compose >= 2.24.4, and these additional `install.env`
settings alongside the normal database, authentication and Controller Secrets.
This section is the manual path: you write `install.env` and prepare every
file below. Quick mode generates this configuration and the protected
Controller and Signer material for a new host (see [Quick mode](#quick-mode)
and [Quick install materials](#quick-install-materials)), so only the manual
path needs the file preparation described here. Manual installs may use any
supported database and login mode, and either TLS mode.

```dotenv
OCSERV_DEPLOYMENT_MODE=integrated
OCSERV_RELAY_PUBLIC_HOST=relay.example.com
OCSERV_RELAY_SECRET_DIR=/etc/ocservia/relay
OCSERV_SIGNER_SECRET_DIR=/etc/ocservia/signer
OCSERV_SIGNER_STATE_DIR=/var/lib/ocservia-signer
```

Integrated uses the existing explicit root lifecycle: pass `--root-lifecycle`
to bootstrap/install, and run subsequent lifecycle/Compose commands as root
with the same configuration. Provision launcher-owned paths for root. This
lets preflight inspect UID-65532-only Signer material without broadening its
permissions or adding a privileged helper. Non-root Integrated launch is
rejected before host bootstrap; standalone launcher behavior is unchanged.

Set `OCSERV_PUBLIC_HOST` to a distinct lowercase Controller DNS name. Omit
`OCSERV_RELAY_URL_A`, `OCSERV_RELAY_URL_B` and `OCSERV_CERTIFICATE_SIGNER_URL`:
the launcher derives one Relay URL and `https://signer:9443/sign`. Conflicting
values are rejected, and `OCSERV_RELAY_URL_B` must be empty: exactly one Relay is
supported, not a redundant pair. Controller's trusted proxy must remain the
Gateway's application `/32`, not Edge or an entire subnet.

By default (`OCSERV_TLS_MODE=manual`) certificates are provisioned externally;
see [ACME certificates](#acme-certificates) for the alternative. In a manual
installation the operator supplies the Signer's CA chain, issuing key and TLS
identity. Neither the lifecycle nor `compose.sh` generates CAs. Only Quick
generates them (`quick-materials.sh`: offline root CA, online issuing
intermediate and Signer HTTPS identity). Quick creates no Gateway or Relay TLS
identity either; ACME obtains those.

The Relay secret directory is launcher-owned mode 0700 and contains nonempty
single-link `tls.crt` and `tls.key` (manual mode only), launcher-owned mode
0444. Its access token is the existing Controller `relay-access-token`, not a
second Relay token. Signer secret and state directories are UID:GID
65532:65532, mode 0700, canonical paths with protected ancestry. Signer
requires `issuer-chain.pem`, `issuer-key.pem`, `tls-cert.pem`, `tls-key.pem`
and `api-token`, single-link 65532:65532 mode 0400 files. `tls-ca.pem` has the
same ownership and mode 0444 so Controller can read its individual public-CA
mount. Signer's TLS leaf must include SAN `signer`. `api-token` must exactly
match Controller's `certificate-signer-token`; no private issuer key is mounted
into Controller. See [Signer and certificate materials](#signer-and-certificate-materials)
for chain, token, approved key-transfer and recovery requirements.

Installation validates the deployment configuration and its source checkout,
checks the final Compose model and pulls all selected images before stopping
anything. Only first installation attempts `signer init`; its durable intent
is recorded before execution. Retry, start, upgrade and rollback inspect an
existing ledger and never recreate a missing ledger. An interrupted init with
missing/invalid state requires reconciliation, not removal of its intent file.
Mode and database selection are retained in `deployment-profile.json`, which
binds the active mode, backend, topology and ACME setting across pending,
current and previous activations. An existing deployment cannot silently switch
mode or database via environment, and a conflicting profile change aborts
preflight before existing services are stopped. Retrying an interrupted
lifecycle command never replaces existing trust material or initializes a
missing existing Signer ledger.

Signer inspection is exclusive: lifecycle stops Controller/transportd/Signer
and retains `signer-checkpoint.json` (issuer, policy, state version and revision)
before activation. A different identity or lower revision fails closed, leaving
recoverable stopped state. This checkpoint is a lifecycle high-water mark,
not a live per-request backup; backup/restore still requires independently
reconciled newer issuance/revocation evidence (see
[Signer backup and restore](incident-recovery.md#signer-backup-and-restore)).
Rollback changes images, never the ledger, CA, node identity or uncertain
operation state.

Container images and internal service ports come only from the selected
manifest and versioned templates, with no mutable operator overrides. Integrated
mode adds no bundled MySQL support, and Local or OIDC authentication
provisioning works the same as in Standalone mode. There is no automatic
cross-mode migration, automatic CRL distribution, HA or forced disconnection of
existing VPN sessions.

### ACME certificates

`OCSERV_TLS_MODE=acme` (Integrated only; `compose.sh` rejects it in Standalone,
and Quick always uses it) replaces both host certificate pairs:

```dotenv
OCSERV_TLS_MODE=acme
OCSERV_ACME_EMAIL=ops@example.com
```

Gateway (Caddy) and Relay (iroh-relay) each obtain and renew a certificate for
their own name from Let's Encrypt (the default directory) with TLS-ALPN-01.
HTTP-01 is never used. Edge already routes the validation connection by SNI, so
port 443 stays the only TCP port; nothing listens on port 80. Both names must
resolve to this host before installation. `tls.crt` and `tls.key` are then
neither required in `OCSERV_SECRET_DIR` nor in `OCSERV_RELAY_SECRET_DIR`; the
Relay secret directory itself must still exist (it may be empty) with the same
ownership and mode. Account keys and certificates live in the named volumes
`gateway-acme` and `relay-acme`; keep them across upgrades to stay within
issuance rate limits. Gateway joins an `acme-egress` network to reach the ACME
directory, so unlike manual mode it has outbound access.

`OCSERV_ACME_DIRECTORY_URL` selects another https ACME directory, and
`OCSERV_ACME_CA_FILE` (launcher-owned mode 0444, requires the directory URL)
names the CA that serves it, for a private ACME CA or a test server such as
Pebble. The first installation records ACME in `deployment-profile.json`; the
TLS mode cannot change afterwards, and releases without ACME refuse that
profile instead of starting without certificates.

## Install the Controller

First installation runs the versioned `controller-bootstrap.sh` from a clean,
exact-release checkout (manual path) or with `--quick` on a new host. Both hand
off to `controller.sh`, which validates the deployment configuration and
protected local state before activating any services. An operator-hosted
[bootstrap launcher](#bootstrap-launcher-hosting) is optional.

### Lifecycle commands

Use `controller.sh` (implemented in
[controller.sh](../../deploy/production/controller.sh) and
[compose.sh](../../deploy/production/compose.sh)) for install, upgrade,
rollback, start, and uninstall, keeping the configured database backend,
authentication environment, secrets, and clean release source available on the
host. Replacing images or invoking Docker Compose directly bypasses its checks.
The versioned bootstrap script is only a first-install entrypoint. Never rerun
an unpinned or `latest` installer or bootstrap launcher script as an implicit upgrade
path. Run subsequent upgrades, rollbacks, and uninstalls through the Controller
lifecycle scripts as described in
[Upgrade, rollback and uninstall](#upgrade-rollback-and-uninstall).

Lifecycle commands never create or rotate secrets, certificates, or identity
keys. Run every lifecycle command with the validated target configuration and
ensure its `source_commit` is available locally (fetching the exact commit from
the trusted repository if the checkout is shallow). The lifecycle either reuses
the caller's matching clean checkout or retains a separate clean Git checkout
under the protected state root, never modifying an existing checkout to force a
match. Compose definitions and smoke checks run from that target source,
including during later `start` and `uninstall` operations. Source checkouts
retained under the state root are not database backups and are not removed by
runtime uninstall.

### Manual installation

From the directory containing `./install.env`, run the versioned bootstrap
script out of the release checkout:

```bash
../ocservia-vX.Y.Z/deploy/production/controller-bootstrap.sh \
  --version vX.Y.Z
```

To run the full installation lifecycle as `root`, append `--root-lifecycle`:

```bash
../ocservia-vX.Y.Z/deploy/production/controller-bootstrap.sh \
  --version vX.Y.Z \
  --root-lifecycle
```

The bootstrap script validates `./install.env` and prepares a verified release
checkout under the Controller source root. It then runs the production
installer, which validates the configuration, starts the containers, and
confirms that the services are ready. The `--version` value must be an existing
stable or RC release tag.

Do not replace this workflow with a direct `docker compose up -d`. That skips
release digest verification and the lifecycle checks. To validate the rendered
Compose configuration before starting, see [Troubleshooting](#troubleshooting).

Running `install` requires Docker Compose v2 with `docker compose up --wait`
support. `install` runs these steps in order:

1. Acquires an exclusive local lock in `/var/lib/ocservia-controller`.
2. Rejects any existing `current-release.json`.
3. Verifies that the checkout HEAD and clean working tree match the manifest's
   `source_commit`.
4. Runs the guarded Compose preflight.
5. Atomically records the target and lifecycle phase in `pending-release.json`
   before activation.
6. Pulls the version-tagged or SHA-256 image references.
7. Starts the dependency graph with `up -d --wait`.
8. Runs `deploy/production/controller-release-smoke.sh`, which checks Compose
   health, probes the public HTTPS `/api/v1/readyz` and `/api/v1/version`
   routes to confirm the target version and source commit, and verifies the
   transport socket and backup freshness health boundaries.
9. Only after that smoke check passes, commits the manifest as
   `current-release.json` (and, for upgrades, `previous-release.json`), all with
   mode `0600`.

If an install fails, `pending-release.json` remains on disk with failure
evidence and permits only a retry against the identical target (see
[Pending state and recovery](#pending-state-and-recovery)).

### Quick mode

Quick mode sets up a brand-new host with a fixed single-host preset on top of
the standard production installer: Integrated mode, bundled PostgreSQL, ACME
TLS, Local authentication, and the root lifecycle. To use it, pass `--quick` to
`controller-bootstrap.sh`, or to the Controller bootstrap launcher script, which accepts:

```bash
bash "$bootstrap_file" --quick \
  --controller-domain vpn.example.com \
  --relay-domain relay.example.com \
  --acme-email ops@example.com \
  [--version vX.Y.Z] [--root-ca-passphrase-file PATH] [--root-ca-export-dir PATH] [--check]
```

Quick mode requires a Release that ships `deploy/production/quick-install.sh`,
that is, a stable or RC release after v1.2.0. Releases up to v1.2.0 reject
`--quick` before modifying the host. If you need a Standalone topology, an
external or MySQL database, OIDC authentication, or manual TLS certificates,
use the manual workflow above.

At the bootstrap launcher layer, the script validates only argument syntax: two distinct
lowercase DNS names, a well-formed email address, and absolute paths for the
optional Root CA arguments (without reading the passphrase file or export
directory). Passing `--quick` implies `--root-lifecycle`. Quick-specific flags
are rejected unless `--quick` is set, and the node bootstrap launcher script rejects all
of them.

When `--version` is omitted under `--quick`, the launcher falls back to the
`DEFAULT_VERSION` value stamped into the Release asset by
`scripts/prepare-bootstrap-release-assets.sh` (an unstamped copy from Git
leaves `DEFAULT_VERSION` empty and therefore still requires `--version`).
The launcher prints the resolved release tag before downloading the versioned installer, and an
explicit `--version` flag always overrides the stamped default.

When the versioned installer (`controller-bootstrap.sh --quick`) starts, it ignores any local
`./install.env` and runs a read-only host preflight before cloning (including
when invoked with `--check`):

- `getent`, `ip`, and `ss` must exist.
- Both `--controller-domain` and `--relay-domain` must resolve to the same IPv4
  addresses. If a resolved IPv4 address is not bound to a local interface, the
  preflight emits a warning because NAT or cloud elastic IP mappings cannot be
  verified locally. An `AAAA` record pointing to an IPv6 address not bound to
  the host fails the preflight because ACME validation prefers IPv6.
- Host ports `TCP 443` and `UDP 7842` must be unused unless
  `/etc/ocservia/install.env` already exists from a previously interrupted
  Quick run, and upstream cloud firewalls must allow inbound traffic on both
  ports.

After the preflight succeeds, the versioned installer invokes the release's
`deploy/production/quick-install.sh`, which:

1. Prepares the host.
2. Invokes `quick-materials.sh` to generate the protected material (see
   [Quick install materials](#quick-install-materials)).
3. Writes `/etc/ocservia/install.env` (configuring Integrated mode, bundled
   PostgreSQL, ACME, and Local authentication).
4. Runs `install.sh --root-lifecycle`.
5. Provisions the management workspace.
6. Creates the Local administrators `initial-admin` and `initial-approver`.

The generated passwords are written only to root-owned files under
`/root/ocservia-initial-credentials`. Distribute the two accounts to different
operators and change both passwords after first login.

Quick mode installs new hosts only. Re-running the same Quick command resumes
an interrupted Quick installation. Running it against an existing deployment,
an existing non-Quick `/etc/ocservia/install.env`, or mismatched domain or
email arguments fails closed. Use the manual installation workflow or
`controller.sh upgrade` for those hosts. Subsequent `controller.sh` and
`compose.sh` invocations do not read the generated file automatically, so
export the same effective settings as in any other installation (see
[The install.env file](#the-installenv-file)).

### Release manifests

Release manifests use either the v1 or v2 schema. The v1 schema supports
standalone installations with six image roles: `gateway`, `control`,
`transport`, `backup`, `postgres`, and `otel`. The v2 reader keeps the same
per-architecture filenames and protected local file rules while adding
`signer_state_version: 1` and four additional image roles (`edge`, `relay`,
`signer`, and `mysql_backup`), for ten roles in total. Integrated deployments
require v2 (see [Integrated configuration](#integrated-configuration)), and v2
selects backend backup image references directly from the manifest
configuration instead of reading `OCSERV_DATABASE_BACKUP_IMAGE`. Across both
schemas, eight first-party GHCR images use explicit `vX.Y.Z` tags, while
PostgreSQL and OpenTelemetry retain pinned third-party references. All ten
image references must carry explicit version tags or SHA-256 digests.

Manifest validation rejects unknown fields, unrecognized roles or state
versions, missing required roles, invalid SemVer, platform, or source revisions,
and noncanonical JSON formatting. Older lifecycle readers reject v2 manifests.
Manifests never contain secret values. Compose pulls only the images needed by
the selected services, including the backend's manifest-bound backup image.

Formal GitHub Releases publish:

- The architecture-specific Controller manifests `controller-release-amd64.json`
  and `controller-release-arm64.json`, alongside the Agent assets.
- A byte-identical `controller-release.json` alias of the amd64 manifest for
  existing operators.
- The versioned installer entrypoints `controller-bootstrap.sh` and
  `managed-node-bootstrap.sh` for HTTPS download.

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

## Verify the deployment

When the bootstrap command finishes, query the public readiness and version
endpoints:

```bash
curl --fail --silent --show-error \
  "https://${OCSERV_PUBLIC_HOST}/api/v1/readyz"
curl --fail --silent --show-error \
  "https://${OCSERV_PUBLIC_HOST}/api/v1/version"
```

How you sign in for the first time depends on the installation:

- On a manual deployment with Local login enabled, run the
  [one-shot Local admin bootstrap](authentication.md#bootstrap-the-first-local-admin)
  before you test login. A fresh manual install has no default administrator
  password, and normal restarts never reset credentials.
- An OIDC-only deployment authenticates through its configured identity provider
  and RBAC mappings.
- Quick mode writes the generated passwords for its two initial Local
  administrators to root-readable files under `/root/ocservia-initial-credentials`.

Then verify `/api/v1/readyz`, the expected version and source metadata at
`/api/v1/version`, an authenticated read, a node connection through the
configured Relay, OTLP delivery when observability is enabled, and an isolated
restore from the newest backup. Confirm that operators can sign in, that a
managed node can connect through the dedicated Relay, and that the initial
backup artifact is present. Do not treat a healthy `transportd` container as
proof of Relay connectivity. That status only shows that its local Unix socket
is up and says nothing about whether the external Relay is reachable. Never
expose PostgreSQL, internal Unix sockets, Docker sockets, or host `/proc` and
`/sys` mounts.

After the first Controller is verified, install and enroll your first managed
node (see [Agent lifecycle](agent-lifecycle.md)).

### Acceptance boundaries

Across the lifecycle, five acceptance boundaries are tracked with separate
evidence, and passing one does not establish the others:

- Compose container health and dependency readiness.
- Functional release identity from the smoke check.
- Explicit target application and failure-state handling.
- Current database initialization, permissions, and business behavior.
- Disaster recovery through the selected backend's documented procedure.

During startup, the owner `--migrate-only` step of the target release is the
database admission point. What it verifies, and what normal runtime startup and
`/readyz` do and do not verify, are defined in
[Database](../reference/stable-contracts.md#database). Neither runtime startup
nor `/readyz` certifies historical database compatibility. Follow the
[upgrade checkpoint](../reference/stable-contracts.md#upgrade-checkpoint) and
[database recovery](incident-recovery.md#database-recovery), and never edit
receipts to force startup.

### Runtime boundaries

The reference Controller bounds shared SSE fan-out with the admission limits in
[Event streams](../reference/stable-contracts.md#event-streams), reserving
PostgreSQL and HTTP capacity for non-streaming API work in the resilience single-VM
profile; raising any of these limits requires rerunning `make resilience-full`. Do not
remove application admission checks, session revalidation, bounded queues, or
watcher backoff, because the gateway serves only as defense in depth. Alert on
`sse_unhealthy_watchers` and verify cursor catch-up instead of repeatedly
restarting a healthy fan-out loop.

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

## Upgrade, rollback and uninstall

Run all explicit Controller upgrades, rollbacks, and uninstallations through
`controller.sh` (see [Lifecycle commands](#lifecycle-commands)). The lifecycle
has no admission gates for source-version windows, version ordering, or
descriptor equality, so running it across incompatible versions can fail or
damage state, and the tooling does not convert historical deployments (see
[Version policy](#version-policy)). In an Integrated deployment, run the
commands as root from a clean checkout at the configuration's `source_commit`,
with the same configuration used for installation.

### Upgrade

Before starting an upgrade, verify current service health, confirm that you
have a fresh backup you can restore (see
[Backups and credential rotation](#backups-and-credential-rotation)), and
review the target release's persistent-state requirements and the
[backend recovery procedure](incident-recovery.md#database-recovery). The
lifecycle script checks only that the database backup service (along with
PostgreSQL in bundled deployments) is running and healthy, so you must verify
on your own that a recent backup file exists and is restorable. Run the upgrade
from a clean Git clone with no staged, unstaged, or untracked changes, using a
protected release bundle that matches the Docker daemon architecture. When the
working tree is not already checked out at the target `source_commit`, that
commit must exist in the local repository so the lifecycle can maintain a
separate clean clone under the protected state root.

```bash
deploy/production/controller.sh upgrade \
  --release-file /protected/release/controller-release-<arch>.json
```

An `upgrade` run first validates the confirmed current state and target
manifest, checks the target `source_commit` against a clean checkout before any
Compose operation, and verifies the current deployment's dependency and backup
health. While the current release stays online, `upgrade` records the pending
state, renders the target Compose configuration and pulls the target images,
then executes the forward database migration and starts the `up -d --wait`
dependency graph. Always deploy the exact images declared in the release
manifest instead of substituting caller-selected tags. Passing a manifest
identical to the active release is a no-op. Any different artifact (whether its
version string is lower, equal, or higher) follows the full execution path.
Once the release smoke check passes, `upgrade` atomically rolls the complete
manifests into `previous-release.json` and `current-release.json`.

Once image pulling and container activation start, a failure can leave the host
with partially replaced containers. The confirmed release state on disk stays
unchanged, but the running services are no longer guaranteed to match it. Any
failure after activation returns non-zero without redeploying old images,
running down migrations, or altering confirmed release state (see
[Pending state and recovery](#pending-state-and-recovery)). In an Integrated
deployment, the upgrade verifies, preflights and pulls before stopping
services. Activation, container recreation and rollback interrupt requests and
streams, so reconnect clients after readiness returns.

#### Runtime database grants

Before starting the Controller, the upgrade workflow runs the target release's
one-shot `migrate` service with `--migrate-only`, mounted database owner
credentials, and `OCSERV_RUNTIME_DATABASE_ROLE`. That step reapplies runtime
grants even when no new schema migration is needed. The long-running
Controller containers retain only runtime credentials. Keep your
authentication configuration, session and audit keys, database TLS files, and
backend/role settings intact across upgrades.

Only a new, verifiably empty database is initialized from the backend's current
`schema.sql`. Existing databases are validated and advanced strictly through
unapplied forward migrations and revisions; an updated snapshot file in a new
release is never a reason to overwrite an existing schema. If schema provenance
is missing or inconsistent, or if an earlier MySQL initialization was
interrupted, normal startup stops. Do not erase metadata tables or alter
checksums to force startup past those checks.

Policy cleanup requires `DELETE` privilege on `user_policy_enforcements`,
granted to the configured role on PostgreSQL or to the explicit `user@host`
account on MySQL. The grant itself is at the table level, but store
predicates restrict deletion to eligible unfinished records. Other table or
DDL privileges remain unchanged. Replacing binaries without running the
migration step will not repair missing grants.

After the upgrade completes, confirm that migrations and background maintenance
run without logging `user_operations.cleanup_failed`. If that error occurs,
the maintenance worker halts the remaining steps for that pass. The Controller
process stays alive and retries on its normal tick. Fix any persistent
permission or storage failures. This maintenance pass does not perform a full
historical cleanup of the table.

#### Database cutover

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

#### Privileged command rollout

Privileged node commands require root-authenticated privd key enrollment, key
rotation state, receipt evidence, and certificate receipt bindings. Deploy
Controller support first. The order for the node side, including key
registration and capability approval, is in
[Register the privd attestation key](agent-lifecycle.md#register-the-privd-attestation-key).

#### After the upgrade

After every upgrade or rollback, require the built-in lifecycle smoke check to
pass, then verify `/api/v1/readyz`, the expected version and source metadata at
`/api/v1/version`, authenticated application reads, and the node inventory.

### Rollback

Rolling back reverts only the Controller application package (container images
and the Compose project). Rollback never restores database contents or
downgrades the schema. Reverting after an upgrade that applied forward
migrations leaves the older images running against the already-migrated
database, which may not be compatible. Reverting database state requires the
separate [database restore](incident-recovery.md#database-recovery) procedure.

Before rolling back, stop new writes if the incident requires it, reconcile
every Unknown operation, and evaluate the risks across the current and previous
database state, configuration, and backups. The host must have the protected
`previous-release.json` and its exact source commit available; a fresh
installation has no previous release record, and rollback refuses to run
without one. Like `upgrade`, `rollback` first runs the database and backup
service health check. A changed database or deployment contract, or
insufficient backup and recovery state, can refuse rollback. Retain both source
commits and bundles until the recovery window ends.

```bash
deploy/production/controller.sh rollback
```

The `rollback` command always targets the protected `previous-release.json` on
disk and never takes an operator-supplied manifest. It enforces no version
ordering, equal migration counts, or descriptor equality checks, and runs no
historical database compatibility preflight, so rollback does not guarantee that
an arbitrary previous release can operate against the existing database state.
It uses that target release's exact source commit for Docker Compose and smoke
checks, maintaining a separate clean checkout under the protected state root
whenever needed so bind-mounted files remain available. Missing source commits,
dirty checkouts, invalid platforms, and configuration or runtime failures remain
hard errors.

Provided the exact target source and images remain available and satisfy the
deployment's requirements, `rollback` renders and pulls the previous image
references and starts the normal target Compose graph with `up -d --wait`
(including required forward initialization, never a database down migration,
restore, Signer identity reset, or legacy Docker network conversion). It swaps
the confirmed state files only after the functional release smoke check confirms
the previous version and source commit. When a rollback succeeds, the restored
release becomes current and the replaced release becomes `previous`, so running
`rollback` a second time returns to the newer release. If the target release is
identical to the active one, the command exits as a no-op (matching version
strings alone do not establish artifact identity). If rollback fails after
activation, confirmed state remains unchanged and pending failure evidence is
retained for a same-target retry without automatically redeploying the current
images. Full database restoration belongs to
[database recovery](incident-recovery.md#database-recovery) as a separate
disaster-recovery boundary outside application rollback.

### Pending state and recovery

If an install, upgrade, or rollback fails, `pending-release.json` remains on
disk with failure evidence and permits only a retry against the identical
target. During an upgrade, the pending file also records the pre-activation
current manifest so recovery can populate `previous-release.json` before
clearing completed evidence. The script preserves the confirmed release state.
Until a retry or recovery pass completes the smoke check, treat the running
containers as unconfirmed, as they may be a mix of old and new images. Fix the
underlying cause and retry the exact same target release. Do not run `install`
on an existing deployment, delete the pending state files, replay an Unknown
operation, initialize another Signer ledger, or manually force old images into
the stack.

When recovering to the same target is impossible, apply a forward fix or run an
isolated [database restore](incident-recovery.md#database-recovery). Before
reopening production traffic or issuing commands, fence off old database
writers, verify schema and runtime permissions and the audit chain with the
real keys, and reconcile all pending or Unknown work. A verified backup on its
own does not authorize reopening production. If a target release cannot start
with the existing deployment, preserve durable state and follow
[incident recovery](incident-recovery.md), noting that the absence of an
upfront compatibility rejection does not prove cross-version state is safe.

### Start

When confirmed release state already exists, use `start` (never `install`) to
bring the services up:

```bash
deploy/production/controller.sh start
```

`start` resolves the image inventory and exact source from
`current-release.json`, runs the target Compose graph with `up -d --wait` and
release smoke verification, and leaves confirmed state unchanged. Keep the
production environment, secrets, and backup directory available.

### Uninstall

Before uninstalling, make sure no install, upgrade, or rollback transaction is
pending, and retain all required backups, incident evidence, current release
files, and production secrets.

#### Retain data

```bash
deploy/production/controller.sh uninstall
```

Running `uninstall` acquires the lifecycle lock and executes protected Compose
`down`, retaining named database, transport, and trust volumes, external
databases, confirmed manifests, source checkouts, backups, and secrets so that
repeating a completed uninstall is safe. The Controller remains offline and
managed nodes lose their Controller connection until you run `start`. In an
Integrated deployment, the non-purging uninstall is a full maintenance stop of
the public Controller and Relay as well as the Signer, and it is not
authorization to erase CA or node identities.

#### Purge local data

Pass `--purge-data` only when you intend to delete local deployment data:

```bash
deploy/production/controller.sh uninstall --purge-data
```

In Standalone mode, `--purge-data` removes the production project volumes
(including bundled database data, which cannot be recovered afterwards without
an off-host backup) and the lifecycle state files (`current-release.json`,
`previous-release.json`, `pending-release.json`, and the deployment profile)
after a clean shutdown. It keeps the lock, secrets, backup directory, external
databases, off-host backups, source checkouts (including retained source clones
under the state root), container images, and unrelated Docker volumes intact.
Volume purge is neither a secure erase nor a database recovery mechanism.
Integrated mode refuses the operation because disposing of Signer and identity
material requires separate reconciliation.

Both uninstall modes:

- Refuse to run while a pending transaction exists.
- Leave confirmed state and data untouched if shutdown fails.
- Retain lifecycle state if volume purge fails.
- Report residual paths if state cleanup fails.
- Never re-enter the bootstrap launcher.

If Docker fails to remove a volume, the purge remains partial and the lifecycle
state files are kept. If volume removal succeeds and only state-file cleanup
fails, the script reports the leftover paths and exits non-zero.

Verify that the uninstall command exits zero. After a data-retaining uninstall,
confirm that the project containers are gone and that `start` restores the
confirmed release; after a purge, verify that your off-host recovery material
remains available.

### Backups and credential rotation

[Database backup and restore](incident-recovery.md#database-backup-and-restore)
covers retention rules, backup-role connection files, off-host copies, and
isolated restore verification. Follow
[PostgreSQL credential rotation](incident-recovery.md#postgresql-credential-rotation)
to update database password verifiers and Compose secret files together,
because neither replacing a container image nor updating a secret file on disk
changes a password stored inside the database. Signer ledger backup, restore,
and CRL maintenance are in
[Signer backup and restore](incident-recovery.md#signer-backup-and-restore).

## Relay and networking

### Dedicated Relay

Every supported deployment uses a single dedicated HTTPS Relay running in
custom mode with authenticated token files and the standard identity and
command verification checks. A Standalone Relay runs on its own host as an
independently deployed service outside the Controller stack. In an Integrated
deployment, the Relay runs on the Controller host and derives its URL from
`OCSERV_RELAY_PUBLIC_HOST` (see [Integrated configuration](#integrated-configuration)).
Public or default relays are never used as a production fallback. Any Relay
recovery test must prove that traffic actually depends on the Relay path,
because normal direct connectivity remains active alongside the Relay. The
Relay image is built from the locked vendored `iroh-relay` (see
[Relay build](../architecture.md#relay-build)).

Agent enrollment, Agent sessions, and transportd disable public endpoint address
publishing and lookup. They use the configured Relay addresses; an EndpointID
alone is not enough to discover a peer. They resolve the Relay name only
through the host's system DNS configuration and hosts file, with iroh's public
fallback resolvers disabled. If those cannot resolve the name, the Relay
connection fails and existing reconnection keeps retrying the configured
resolvers. The Agent logs `Failed to connect to relay server` with
`Resolve failed`. transportd dials the Relay only after selecting it as its home
Relay, so it may log only `probe timed out`; the failure then shows as Agents
unable to connect and the node going offline. Public nameservers listed in the
host's own DNS configuration remain an operator dependency.

Before you begin a Standalone Relay, have:

- One independent Relay host with a DNS name and matching TLS certificate.
- A single high-entropy relay access token provisioned to the Relay, the
  Controller transport service, and all enrolled Agents.
- A prebuilt relay container image referenced by an explicit `vX.Y.Z` tag or
  SHA256 digest.

#### Configure a Standalone Relay

1. On the Relay host, create `OCSERV_RELAY_SECRET_DIR` as a launcher-owned
   directory with mode `0700` containing `tls.crt`, `tls.key`, and
   `relay-access-token`. Both TLS files must be owned by the launcher user with
   mode `0444`. `relay-access-token` must be owned by UID/GID 65532 with mode
   `0400`. Export `OCSERV_RELAY_IMAGE` with an explicit version tag or SHA-256
   reference. Assign a DNS name to the host with its matching certificate and
   key, and use the checked-in `deploy/production/relay/relay.toml`
   configuration. The standalone Relay publishes TCP 80/443 and UDP 7842.
2. On the Relay host, validate and start the relay:

   ```bash
   deploy/production/relay/compose.sh config --quiet
   deploy/production/relay/compose.sh up -d
   ```

3. Configure the Controller relay URLs before starting or restarting the
   Controller:

   ```bash
   export OCSERV_RELAY_URL_A=https://relay.example.com
   export OCSERV_RELAY_URL_B=
   ```

   You can also define these variables in the Controller's protected
   `install.env` without editing release files. `OCSERV_RELAY_URL_A` is
   required. `OCSERV_RELAY_URL_B` must be absent or empty; `install.sh`,
   `compose.sh`, and the transportd wrapper all reject a nonempty B before any
   side effects occur. Exporting an explicitly empty `OCSERV_RELAY_URL_B` in the
   shell overrides any B entry present in `install.env`. Exactly one dedicated
   Relay is supported, without Relay failover, so B does not provide optional
   redundancy. Integrated mode derives its Relay URL from its public host and
   rejects a conflicting A or any nonempty B; use
   [Integrated configuration](#integrated-configuration) instead of overriding
   Standalone Relay settings.

4. Before enabling services on each managed node, configure the Agent with the
   production relay drop-in and launcher, the matching URLs, and the same token
   file; see [Agent lifecycle](agent-lifecycle.md) for the verified package path
   and file contract.

   ```dotenv
   RELAY_URL_A=https://relay.example.com
   RELAY_URL_B=
   ```

   Place these values in the managed node's `install.env` during initial
   installation, or in `/etc/ocservia-agent/relays.env` when making an explicit
   operator topology change. Never edit the released Compose file or place raw
   token contents in either environment file.

The Standalone Relay runs as an independent service outside the Controller
stack. Running a Standalone Relay and a Controller on the same host is not
validated because both default deployments bind TCP 443 across all addresses,
making an unmodified co-hosted setup on a single IP unsupported. For a
single-host deployment, use Integrated mode instead.

#### Verify the Relay

Confirm that the Controller and every Agent use the same single Relay URL.
An outage on that Relay interrupts Relay-dependent traffic. Recovery requires
restoring the Relay with the same address, certificate, and credentials, then
verifying reconnection, fresh heartbeats, and command results. Preserve pending
command state and reconcile any Unknown outcomes before performing an explicit
safe retry.

#### Migrate an existing A/B deployment

During a planned maintenance window, clear the B entry in both the Controller
and Agent configurations before running the new installers or starting the new
launchers, while keeping A and all identity, enrollment, token, and journal
material intact. Services do not hot-reload the Relay list, so restart the
affected services and verify A-only traffic before retiring B. Rerunning the
installer on an already-enrolled node validates the existing configuration and
refuses any persisted non-empty B entry without rewriting topology or replacing
tokens. If you later need to roll back, check the previous release's actual
requirements first; see [Agent rollback](agent-lifecycle.md#rollback) and
[Controller rollback](#rollback).

#### Relay trust and capacity

Sharing the Relay access token only authenticates access to the relay
transport; it does not act as an Agent identity or a Controller capacity guard.
Keep all five trust classes and the reserved known-Agent capacity enabled in
transportd. Direct and Relay paths enforce the same snapshot, revocation,
authorization, registration-recheck, and enrollment-token rules, and IP or NAT
identity must never be used to refill an EndpointID budget.

### Application networking

Only transportd joins the non-internal `relay-egress` network, using it for DNS
and outbound HTTPS to the Relay by its HTTPS hostname (the independent Relay in
Standalone mode, or the same-host Relay via its public name in Integrated
mode). The application, database, and observability networks remain internal
and add no published Controller ports. The local socket healthcheck does not
test relay reachability, so validate TLS, token-authenticated relay traffic, and
fresh Agent observations separately. Normal production direct
connectivity remains unchanged.

The control plane runs with `--role=all`. Public TLS terminates at the gateway
(behind Edge in Integrated mode), and node certificates come from an HTTPS
certificate signer (the external endpoint in Standalone mode, or the bundled
Signer in Integrated mode). Set `OCSERV_PUBLIC_ORIGIN` to the public HTTPS
origin; when OIDC is enabled, configure its redirect URI as
`https://$OCSERV_PUBLIC_HOST/api/v1/auth/callback` with an origin matching
`OCSERV_PUBLIC_ORIGIN`.

### Integrated host network

In an Integrated deployment, one host and public IP serve two distinct DNS
names. Both names must resolve to the host, and upstream cloud firewalls must
allow inbound TCP 443 and UDP 7842. Routing and trust boundaries are in
[Integrated deployment](../architecture.md#integrated-deployment). No host
ports are added for Gateway, Signer, the database, Controller HTTP, or
observability services.

Both lowercase DNS names must be distinct. Gateway accepts the configured
Controller hostname with an optional explicit `:443` port in the HTTP Host or
HTTP/2 authority header, and accepts PROXY metadata only from Edge's dedicated
`/32` (`OCSERV_EDGE_GATEWAY_IP`, default `172.30.241.2`). Reserve the Edge
address outside the dynamic allocation range, and keep the subnet and range
overrides disjoint from other networks. Relay sees Edge's internal IP, so do
not rely on Relay for original-IP abuse controls.

Never substitute raw IP URLs or disable certificate verification to work around
routing failures, even when Controller and Relay share a machine. Inspect the
rendered network attachments and permitted egress whenever external database
overlays are added.

Gateway and Relay names resolve through Docker DNS every five seconds for new
connections. Recreating a container is disruptive: existing streams can break
and clients must reconnect. Edge defaults to one worker, 2048 worker
connections, 8192 file descriptors, 64 MiB and a 35-minute stream inactivity
timeout. Periodic SSE heartbeats keep active streams open and do not test a
35-minute completely idle Edge timeout. A relayed connection consumes multiple
descriptors and hops, so these defaults make no 2048-client, latency or HA promise.

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
application. Responses generated directly by Caddy for handler errors (such as
a 502 when the control plane is unreachable) skip the site's deferred `header`
block, so each file repeats the headers inside `handle_errors`. Keep all
four header blocks identical;
[`test-controller-gateway-headers.py`](../../scripts/test-controller-gateway-headers.py)
verifies both static and 502 responses across both configurations.

If CSP enforcement breaks a page, roll back at the release level instead of
editing an installed copy, since the Standalone Caddyfile is built into the
gateway image and Integrated mode mounts its gateway files from a release
checkout that the lifecycle requires to stay clean. Either run
[Controller rollback](#rollback) to return to a previous Report-Only release, or
upgrade to a release built from a commit that renames only this header to
`Content-Security-Policy-Report-Only` across all four blocks while keeping the
remaining headers intact.

<a id="authentication-request-budgets"></a>
### Gateway addressing and proxy trust

[Authentication](authentication.md#authentication-request-budgets-and-proxy-trust)
covers request rate budgets and trusted-client-IP handling.
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
services to force a new layout to fit.

<a id="stage-0-bootstrap-hosting"></a>

## Bootstrap launcher hosting

In a manual installation, the bootstrap launcher is an optional convenience wrapper over the
same lifecycle boundaries as the manual path:

```text
bootstrap launcher -> exact vX.Y.Z versioned installer -> install.env -> durable clean checkout
        -> production/install.sh -> deployment configuration -> controller.sh
        -> smoke/readiness
```

The bootstrap launcher's initial bytes come from the static HTTPS endpoint, and it fetches the
versioned installer script over HTTPS. The versioned installer prepares a durable release checkout
and leaves `install.env` in the operator's configuration directory outside that
checkout. It then hands off to controller.sh, which validates the deployment
configuration and protected local state before activating any services. Until
the static bootstrap host has operational ownership and byte-verification
evidence, obtain the versioned installer directly from a clean, exact-release checkout as
described in [Manual installation](#manual-installation).

The repository maintains two bootstrap launchers:

- `deploy/bootstrap/install-controller`
- `deploy/bootstrap/install-node`

When an operator hosts the public bootstrap endpoints, `install-node` is served
byte-for-byte from Git at `https://get.ocservia.example/install-node`.
`install-controller` is served byte-for-byte from the `install-controller`
asset of the latest stable Release at
`https://get.ocservia.example/install-controller`. That Release asset matches
the Git source except for its stamped default version; see
[Quick mode](#quick-mode). The non-executable configuration template
`install.env.example` is served byte-for-byte at
`https://get.ocservia.example/install.env.example`. External static-hosting
infrastructure lives outside this repository, so those example endpoints are
active only when deployed by an operator.

### Hosting requirements

Serve the endpoints from static HTTPS hosting with HSTS enabled and
`Content-Type: text/plain`. Responses must be static for all callers, without
per-user templating, query-parameter tokens, embedded secrets, or server-side
session state. Publish only from the Git source file (or, for
`install-controller`, the latest stable Release asset built from it), and never
host an RC asset.

After deploying the files, verify that the served bytes match the expected
source artifacts. `check-release-bootstrap.sh` first checks that the downloaded
Release asset differs from the Git source at that tag only in its stamped
default version:

```bash
scripts/check-release-bootstrap.sh "$RELEASE_ASSETS/install-controller" vX.Y.Z
scripts/verify-bootstrap-endpoint.sh \
  https://get.ocservia.example/install-controller \
  "$RELEASE_ASSETS/install-controller"
scripts/verify-bootstrap-endpoint.sh \
  https://get.ocservia.example/install-node \
  deploy/bootstrap/install-node
scripts/verify-bootstrap-endpoint.sh \
  https://get.ocservia.example/install.env.example \
  install.env.example
```

Each `verify-bootstrap-endpoint.sh` invocation downloads the URL read-only over
HTTPS, enforces exact byte equality against the local file, and prints the
deployed SHA-256 digest for the deployment record.

<a id="stage-0-trust-model"></a>

### Bootstrap launcher trust model

Outside [Quick mode](#quick-mode), the launcher requires an explicit stable
`--version vX.Y.Z`, builds the corresponding GitHub Release URL for that tag,
and hands off execution to the matching `controller-bootstrap.sh` or
`managed-node-bootstrap.sh` versioned installer asset. The launcher itself never reads
`install.env`, installs packages, runs the Controller lifecycle, enrolls or
approves a node, starts services, or crosses a privilege boundary; the versioned installer owns
all configuration, installation, enrollment, activation, and package or
lifecycle validation.

During the handoff, the launcher fetches the versioned installer script over HTTPS with TLS
verification enabled, rejects download errors or empty responses, and runs the
downloaded file from a private temporary directory. Download authenticity in
this phase relies on the HTTPS endpoint and its PKI without separate release
keys, fingerprints, signed checksum manifests, or detached signatures. The launcher
imposes no additional OpenSSL version requirement.

Operators should download the launcher locally, inspect its contents, and invoke it
with an explicit version tag. Never resolve the version dynamically from
`latest`, a branch name, or a commit hash, and never pipe an unverified script
into a shell; the only permitted default version is the exact release tag
stamped into a Release's own `install-controller` asset for
[Quick mode](#quick-mode).

### Intended entrypoints

Once the static endpoints are deployed and verified, the one-line convenience
wrappers below download the launcher to a temporary file, check for its trailing
`main "$@"` completeness marker, and only then execute it. Wrapping the steps
in a subshell surfaces any transport, empty-body, or truncated-download failure
to calling automation:

```bash
(
  set -eu
  bootstrap_file="$(mktemp)" || exit 1
  trap 'rm -f -- "$bootstrap_file"' EXIT
  trap 'exit 1' HUP INT TERM
  curl -fsSL --proto '=https' --tlsv1.2 -o "$bootstrap_file" \
    https://get.ocservia.example/install-controller || exit 1
  test "$(tail -n 1 "$bootstrap_file")" = 'main "$@"' || exit 1
  bash "$bootstrap_file" --version vX.Y.Z
)

(
  set -eu
  bootstrap_file="$(mktemp)" || exit 1
  trap 'rm -f -- "$bootstrap_file"' EXIT
  trap 'exit 1' HUP INT TERM
  curl -fsSL --proto '=https' --tlsv1.2 -o "$bootstrap_file" \
    https://get.ocservia.example/install-node || exit 1
  test "$(tail -n 1 "$bootstrap_file")" = 'main "$@"' || exit 1
  bash "$bootstrap_file" --version vX.Y.Z
)
```

Beyond `--version`, the launcher accepts only `--root-lifecycle` and, on the
Controller entrypoint, `--check` and the [Quick mode](#quick-mode) flags. Pass
all configuration and secrets through the environment or protected host paths
consumed by the installer. The launcher neither parses nor prints secret material. On a
managed node, automated bootstrap stops at `ENROLLED_LOCAL` with the node
pending approval on the Controller, leaving approval and service activation as
explicit post-enrollment steps.

The bootstrap launcher is only for first installation. Subsequent Controller upgrades
and rollbacks run through `controller.sh` and its protected lifecycle state.
Managed-node upgrades and removals run through the authorized upgrader or
native package manager. Persist long-lived settings in `install.env` or the
installed service configuration so they do not depend on re-running the
bootstrap wrapper with CLI flags.

## Troubleshooting

Start with the procedure in the relevant section above, then check the matching
failure boundary below. Retain the exact release version, host architecture,
command output, and redacted logs with your incident record.

### Controller does not start

Run the read-only Compose configuration check first, which validates the
rendered Compose configuration without printing secret contents:

```bash
deploy/production/compose.sh config --quiet
```

`compose.sh config --quiet` validates only the environment exported in your
current shell. It does not read the release manifest, check installation state,
or modify disk, so verify those settings directly against the active release
manifest:

- All six core image variables (`OCSERV_GATEWAY_IMAGE`, `OCSERV_CONTROL_IMAGE`,
  `OCSERV_TRANSPORT_IMAGE`, `OCSERV_BACKUP_IMAGE`, `OCSERV_POSTGRES_IMAGE`, and
  `OCSERV_OTEL_IMAGE`) must be exported as version-tagged (`:vX.Y.Z` or
  `:vX.Y.Z-rc.N`) or `@sha256:` references taken from the release manifest in
  use. The same applies to `OCSERV_EDGE_IMAGE`, `OCSERV_RELAY_IMAGE`, and
  `OCSERV_SIGNER_IMAGE` for Integrated, and to `OCSERV_DATABASE_BACKUP_IMAGE`
  for MySQL. Never substitute an unreviewed tag.
- Both the protected secret directory and the backup directory must satisfy
  their ownership and mode contracts (see [Production secrets](#production-secrets)),
  and all required PKI, Controller EndpointID, and relay settings must be
  present.
- Authentication variables must match the selected
  [mode](authentication.md#choose-a-mode): Local-only requires no OIDC settings
  or client secret. OIDC-only and dual-auth require a complete OIDC
  configuration and protected client secret (a partial OIDC configuration is
  rejected even when Local is enabled).

Always manage the stack through `controller.sh` instead of invoking Docker
Compose directly.

### Optional traces are missing

OpenTelemetry export stays disabled whenever `OCSERV_OTEL_BACKEND_ENDPOINT` is
unset or empty. Using the same exported configuration passed to the lifecycle
command, list the enabled services and running containers without dumping
secrets:

```bash
deploy/production/compose.sh config --services
deploy/production/compose.sh ps
```

The `otel-collector` service appears only when an endpoint is configured, as
the launcher enables that profile automatically and ignores any inherited
`COMPOSE_PROFILES`. When tracing is enabled, verify that `otel-client.crt`,
`otel-client.key`, and `otel-ca.crt` are owned by the launcher user with mode
`0444` (missing or mispermissioned TLS files block startup), then inspect the
Collector logs and check backend mTLS connectivity.

### Deployment configuration is rejected

Use the JSON release configuration that matches the Docker daemon architecture
along with a clean Git checkout of its `source_commit`. Verify the release
version, topology, image references, and root-controlled file ancestry, then
fix the reported prerequisite without bypassing lifecycle validation or
overwriting confirmed or pending state files.

### Relay is unavailable

Only one dedicated Relay is supported and any non-empty `RELAY_URL_B` is
rejected, so there is no standby failover or second Relay to fall back to. All
Relay-dependent communication pauses until the Relay recovers. Restore the
Relay at the same address with its matching certificate and credentials
(rebuilding the Relay host with those same materials is how you recover the
original Relay), then confirm reconnection, fresh heartbeats, and command
results. Verify DNS resolution, HTTPS egress, and authenticated relay
connections end to end. A healthy container or local socket on the Relay host
does not prove reachability. Never fall back to a public relay or replace
the Controller or Agent identity key during a Relay outage, and do not rotate
endpoint identity keys. Operating a single Relay means maintenance and token
rotation can briefly interrupt communication, so schedule a recovery window
instead of attempting a two-path zero-interruption rotation. Preserve identity,
enrollment, and pending command journals, and verify authenticated traffic once
service is restored.

### Other failures

- Identity provider outage: see
  [Authentication outage](incident-recovery.md#authentication-outage).
- Database recovery: check `OCSERV_DATABASE_BACKEND` and
  `OCSERV_DATABASE_DEPLOYMENT` in the effective deployment configuration, then
  follow [Database recovery](incident-recovery.md#database-recovery). Neither
  rolling back the Controller application package nor verifying a backup
  checksum restores database state.
- Agent enrollment and Agent start failures: see the Troubleshooting section of
  [Agent lifecycle](agent-lifecycle.md#troubleshooting).
