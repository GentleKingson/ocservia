# Deploy the Controller

This guide walks through a manual production installation of the ocservia Controller. You prepare `install.env` and the required secret files yourself before running the installer. If you are setting up a new single-host Integrated Controller and want the installer to generate those files for you, use [Quick mode](#quick-mode-on-main) instead. The [Production deployment reference](../operations/production-deployment.md) has the file permission matrices, runtime state layouts, rollback procedures, and disaster recovery details.

Deploy an exact published release that has passed Release Check. The commands below use `vX.Y.Z` as the release tag. They also accept an exact `vX.Y.Z-rc.N` candidate tag (with a positive `N` and no leading zeros), but release candidates are not recommended for stable production use.

The manual installer sets up a fresh deployment. It does not convert historical layouts. The v1.1.0 policy removed the software-version admission gates, but cross-version operation is still not guaranteed. The [support policy](../reference/support-policy.md) describes the version boundaries.

## What the Controller does

The Controller serves the administrative Web console and the HTTPS API, and it stores cluster state and audit records in a supported relational database. It talks to managed nodes through the dedicated Relay. It also coordinates its own install, upgrade, rollback, and uninstall lifecycles.

## Requirements

- A supported `amd64` or `arm64` Linux host running Ubuntu 20.04/22.04/24.04/26.04 or Debian 11/12/13.
- Git, `curl`, and Docker Engine with the Compose v2 plugin. The installer can install Docker on most supported distributions, but on Ubuntu 20.04 you must install a compatible Docker yourself before you run the script.
- A DNS name and HTTPS certificate for the Controller. Integrated mode needs a second, distinct DNS name for its co-located Relay. With Integrated ACME enabled, it can obtain both certificate pairs automatically.
- An authentication mode: Local only, OIDC only, or Local + OIDC (an OIDC identity provider and client credentials are required only when SSO is enabled).
- In Standalone mode, an external certificate Signer endpoint and one dedicated HTTPS Relay. In Integrated mode, both run on the Controller host.
- Protected host directories for secrets and database backups.

A manual installation never generates production passwords, private keys, certificates, relay tokens, or signing keys. Only [Quick mode](#quick-mode-on-main) generates initial material, and only for a new host.

### Choose the deployment mode

The default `standalone` mode connects to a Relay and a Signer hosted elsewhere. The `integrated` mode runs the SNI-routing Edge, the Relay, and the production Signer on the Controller host, managed by the same installer and lifecycle commands.

Integrated mode requires a v2 platform manifest, Docker Compose `>= 2.24.4`, an explicit `--root-lifecycle`, two distinct DNS names and certificate pairs, and the settings described in [Integrated Secret and state configuration](../../deploy/production/integrated/README.md#lifecycle-configuration). Only Edge `TCP/443` and Relay `UDP/7842` are exposed publicly; the Signer stays on an internal network. Running these services on one host does not provide high availability.

With the default `OCSERV_TLS_MODE=manual`, you supply both TLS certificate pairs yourself. In Integrated mode, `OCSERV_TLS_MODE=acme` obtains and renews them automatically (see [ACME certificates](../../deploy/production/integrated/README.md#acme-certificates)). Even with ACME enabled, the manual lifecycle never generates internal CA or Signer keys.

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

The installer parses `install.env` as a strict key-value allowlist. It is not a shell script, so do not `source` it.

For an Integrated deployment, set `OCSERV_DEPLOYMENT_MODE=integrated` together with the documented Relay and Signer state directories and the public Relay hostname. Leave out the external Signer and Relay URL variables; the installer derives them itself.

For the login settings, start from the complete [authentication mode example](../operations/authentication.md#choose-a-mode) that matches the mode you chose.

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

OpenTelemetry export is disabled while `OCSERV_OTEL_BACKEND_ENDPOINT` is unset or empty. In that state you need no Collector container and no monitoring TLS files.

Setting `OCSERV_OTEL_BACKEND_ENDPOINT` in `install.env` enables the Collector automatically. Before you start the stack, place `otel-client.crt`, `otel-client.key`, and `otel-ca.crt` in `OCSERV_SECRET_DIR`, owned by the launcher user with mode `0444`. Startup fails if any of these files is missing or has the wrong permissions.

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

The bootstrap script validates `./install.env` and prepares a verified release checkout under the Controller source root. It then runs the production installer, which validates the configuration, starts the containers, and confirms that the services are ready. The `--version` value must be an existing stable or RC release tag.

Do not replace this workflow with a direct `docker compose up -d`. That skips release digest verification and the lifecycle checks.

<a id="quick-mode-on-main"></a>

### Quick mode

Quick mode sets up a brand-new host with a fixed single-host preset on top of the standard production installer: Integrated mode, bundled PostgreSQL, ACME TLS, Local authentication, and the root lifecycle. To use it, pass `--quick` to `controller-bootstrap.sh`.

Quick mode does not read `./install.env`. It writes a generated configuration to `/etc/ocservia/install.env`, creates the required cryptographic and secret files, and provisions two initial Local accounts, `initial-admin` and `initial-approver`.

If you need a Standalone topology, an external or MySQL database, OIDC authentication, or manual TLS certificates, use the manual workflow above.

Quick mode exists only in stable or RC releases after v1.2.0 that include `deploy/production/quick-install.sh`. Releases up to v1.2.0 reject `--quick`. [Quick mode](../operations/bootstrap-hosting.md#quick-mode) in the bootstrap hosting guide covers host prerequisites, preflight checks, and rerun rules.

## 5. Verify the deployment

When the bootstrap command finishes, query the public readiness and version endpoints:

```bash
curl --fail --silent --show-error \
  "https://${OCSERV_PUBLIC_HOST}/api/v1/readyz"
curl --fail --silent --show-error \
  "https://${OCSERV_PUBLIC_HOST}/api/v1/version"
```

How you sign in for the first time depends on the installation:

- On a manual deployment with Local login enabled, run the [one-shot Local admin bootstrap](../operations/authentication.md#bootstrap-the-first-local-admin) before you test login. A fresh manual install has no default administrator password, and normal restarts never reset credentials.
- An OIDC-only deployment authenticates through its configured identity provider and RBAC mappings.
- Quick mode writes the generated passwords for its two initial Local administrators to root-readable files under `/root/ocservia-initial-credentials`.

Finally, confirm that operators can sign in, that a managed node can connect through the dedicated Relay, and that the initial backup artifact is present. If observability is enabled, also check that traces reach your OpenTelemetry backend.

Do not treat a healthy `transportd` container as proof of Relay connectivity. That status only shows that its local Unix socket is up and says nothing about whether the external Relay is reachable.

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
