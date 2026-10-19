# Deploy the Controller

Release version examples also accept an exact `vX.Y.Z-rc.N` candidate tag (with a positive `N` and no leading zeros), though release candidates are not recommended for stable production use.

This guide covers a manual production installation of the ocservia Controller, where you prepare `install.env` and the required secret files beforehand. If you are provisioning a fresh single-host Integrated Controller and want the installer to generate those files automatically, see [Quick mode](#quick-mode-on-main). File permission matrices, runtime state layouts, rollback procedures, and disaster recovery details are documented in the [Production deployment reference](../operations/production-deployment.md).

Deploy an exact published release that has passed Release Check. The manual installer sets up a fresh deployment and does not convert historical layouts automatically; while the v1.1.0 policy removed software-version admission gates, cross-version operation is not guaranteed. See the [support policy](../reference/support-policy.md) for version boundaries.

## What the Controller does

The Controller serves the administrative Web console and HTTPS API, persists cluster state and audit records in a supported relational database, communicates with managed nodes through the dedicated Relay, and coordinates Controller-side install, upgrade, rollback, and uninstall lifecycles.

## Requirements

- A supported `amd64` or `arm64` Linux host running Ubuntu 20.04/22.04/24.04/26.04 or Debian 11/12/13.
- Git, `curl`, and Docker Engine with the Compose v2 plugin. While the installer can bootstrap Docker on most supported distributions, Ubuntu 20.04 hosts require a compatible Docker installation before you run the script.
- A DNS name and HTTPS certificate for the Controller. Integrated mode requires a second, distinct DNS name for its co-located Relay, and can obtain both certificate pairs automatically when Integrated ACME is enabled.
- An authentication mode: Local only, OIDC only, or Local + OIDC (an OIDC identity provider and client credentials are required only when SSO is enabled).
- In Standalone mode, an external certificate Signer endpoint and one dedicated HTTPS Relay. In Integrated mode, both run on the Controller host.
- Protected host directories for secrets and database backups.

In a manual installation, the installer never generates production passwords, private keys, certificates, relay tokens, or signing keys; only [Quick mode](#quick-mode-on-main) generates initial material for a new host.

### Choose the deployment mode

By default, `standalone` mode connects to separately hosted Relay and Signer services. Setting `integrated` runs the SNI-routing Edge, the Relay, and the production Signer on the Controller host through the same installer and lifecycle commands. Integrated mode requires a v2 platform manifest, Docker Compose `>= 2.24.4`, explicit `--root-lifecycle`, two distinct DNS names and certificate pairs, and the settings described in [Integrated Secret and state configuration](../../deploy/production/integrated/README.md#lifecycle-configuration). Only Edge `TCP/443` and Relay `UDP/7842` are exposed publicly, while the Signer stays on an internal network. Co-locating these services on a single host does not provide high availability. Under the default `OCSERV_TLS_MODE=manual`, you supply both TLS certificate pairs yourself; in Integrated mode, setting `OCSERV_TLS_MODE=acme` obtains and renews them automatically (see [ACME certificates](../../deploy/production/integrated/README.md#acme-certificates)). Even with ACME enabled, the manual lifecycle never generates internal CA or Signer keys.

## 1. Prepare the configuration directory

Check out the pinned release tag and create a configuration directory outside the repository tree:

```bash
git clone --branch vX.Y.Z --single-branch --depth 1 \
  https://github.com/GentleKingson/ocservia.git ocservia-vX.Y.Z
mkdir ocservia-install && cd ocservia-install
cp ../ocservia-vX.Y.Z/install.env.example install.env
editor install.env
```

Populate the Controller section of `install.env` and leave the managed-node section commented out or deleted. The installer reads `./install.env` from the current working directory.

## 2. Fill in the Controller settings

See `install.env.example` for the complete variable list. At a minimum, configure:

| Setting group | Examples |
| --- | --- |
| Public address | `OCSERV_PUBLIC_HOST`, `OCSERV_CONTROLLER_PUBLIC_URL`, `OCSERV_HTTPS_ADDRESS` |
| Login | `OCSERV_LOCAL_AUTH_ENABLED`, `OCSERV_PUBLIC_ORIGIN`, `OCSERV_SESSION_TTL`; add OIDC settings when SSO is enabled |
| External services | `OCSERV_CERTIFICATE_SIGNER_URL` |
| Controller identity and relays | `OCSERV_CONTROLLER_ENDPOINT_ID` and required `OCSERV_RELAY_URL_A`; leave `OCSERV_RELAY_URL_B` unset or empty (only one dedicated Relay is supported, and a non-empty B value is rejected) |
| Protected storage | `OCSERV_SECRET_DIR`, `OCSERV_BACKUP_DIR`, and optional Controller state root |
| Database | Bundled or external PostgreSQL 18.x, or external MySQL 8.4 LTS |

Keep `install.env` restricted to the operator account and out of version control. Variables exported in the calling shell override entries in the file.

`install.env` is parsed as a strict key-value allowlist and is not a shell script, so do not `source` it directly. For Integrated deployments, set `OCSERV_DEPLOYMENT_MODE=integrated` along with the documented Relay/Signer state directories and public Relay hostname, and omit the external Signer and Relay URL variables (which the installer derives internally). Select one of the complete [authentication mode examples](../operations/authentication.md#choose-a-mode) to match your login configuration.

## 3. Prepare secrets and trust files

Place the required secret files inside the protected directories referenced by `install.env`:

- HTTPS server certificate and private key.
- Database owner, runtime, and backup credentials for your chosen database backend.
- When using external MySQL: `database-ca.pem`, separate owner and runtime DSN files, and `database-backup.cnf`.
- Session key and audit keys, plus the OIDC client secret when SSO is enabled.
- Controller command-signing private key.
- Certificate Signer bearer token.
- Relay access token.
- Controller Iroh identity key.

Verify the exact filenames, ownership, and file modes against the [Production deployment reference](../operations/production-deployment.md) before launching the installer.

### Optional observability

OpenTelemetry export stays disabled whenever `OCSERV_OTEL_BACKEND_ENDPOINT` is unset or empty, and no Collector container or monitoring TLS files are needed in that state. Setting `OCSERV_OTEL_BACKEND_ENDPOINT` in `install.env` enables the Collector automatically; before starting the stack, place `otel-client.crt`, `otel-client.key`, and `otel-ca.crt` in `OCSERV_SECRET_DIR` owned by the launcher user with mode `0444`, as missing or mispermissioned TLS files will block startup.

## 4. Install the pinned release

From the directory containing `./install.env`, run the versioned bootstrap script out of the release checkout:

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

The bootstrap script validates `./install.env`, prepares a verified release checkout under the Controller source root, and invokes the production installer to validate the configuration, start the containers, and confirm service readiness. Do not run `docker compose up -d` directly in place of this workflow, as doing so skips release digest verification and lifecycle checks. An existing stable or RC release tag is required.

<a id="quick-mode-on-main"></a>

### Quick mode

Passing `--quick` to `controller-bootstrap.sh` applies a single-host preset over the standard production installer for a brand-new host: Integrated mode, bundled PostgreSQL, ACME TLS, Local authentication, and the root lifecycle. Instead of reading `./install.env`, Quick mode writes a generated configuration to `/etc/ocservia/install.env`, creates the required cryptographic and secret files, and provisions two initial Local accounts (`initial-admin` and `initial-approver`). Deployments that use Standalone topology, external or MySQL databases, OIDC authentication, or manual TLS certificates must use the manual workflow above. Quick mode is available only on stable or RC releases after v1.2.0 that include `deploy/production/quick-install.sh` (releases up to v1.2.0 reject `--quick`); host prerequisites, preflight checks, and rerun rules are detailed in [Quick mode](../operations/bootstrap-hosting.md#quick-mode).

## 5. Verify the deployment

Once the bootstrap command completes, query the public readiness and version endpoints:

```bash
curl --fail --silent --show-error \
  "https://${OCSERV_PUBLIC_HOST}/api/v1/readyz"
curl --fail --silent --show-error \
  "https://${OCSERV_PUBLIC_HOST}/api/v1/version"
```

On a manual deployment with Local login enabled, run the [one-shot Local admin bootstrap](../operations/authentication.md#bootstrap-the-first-local-admin) before testing login, as fresh manual installs have no default administrator password and normal restarts never reset credentials. OIDC-only deployments authenticate through their configured identity provider and RBAC mappings. Quick mode writes the generated passwords for its two initial Local administrators to root-readable files under `/root/ocservia-initial-credentials`.

Finally, confirm that operator login succeeds, a managed node can connect through the dedicated Relay, the initial backup artifact is present, and (when observability is enabled) traces reach your OpenTelemetry backend. A healthy `transportd` container status only shows that its local Unix socket is up; it says nothing about whether the external Relay is reachable.

## 6. After installation

- Install and enroll your first managed node.
- Run subsequent upgrades, rollbacks, and uninstalls through the Controller lifecycle scripts.
- Never rerun an unpinned or `latest` installer as an implicit upgrade path.
- If you use the optional static bootstrap hosting endpoint, deploy and verify it under your own infrastructure first; never pipe an unverified script into a shell.

## Next steps

- [Install a managed node](managed-node.md)
- [Enroll a node](../how-to/enroll-node.md)
- [Configure a dedicated Relay](../how-to/dedicated-relay.md)
- [Back up and restore PostgreSQL](../operations/database-backup-restore.md#postgresql)
- [Back up and restore MySQL](../operations/database-backup-restore.md#mysql)
- [Production deployment reference](../operations/production-deployment.md)
