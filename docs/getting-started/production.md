# Deploy the Controller

Release version examples also accept an exact `vX.Y.Z-rc.N` candidate tag
(positive N without leading zeros). RCs are not recommended stable releases.

This guide is the short production path for a manual installation of the ocservia Controller: you prepare `install.env` and the protected material. For a new single-host Integrated Controller that generates them, see [Quick mode](#quick-mode-on-main) instead. It focuses on what an operator needs to prepare and run. Exact file modes, lifecycle state, rollback behavior, and recovery details remain in the [Production deployment reference](../operations/production-deployment.md).

Use an exact published release, after Release Check has passed. This is a fresh
installation path, not an automatic historical-deployment conversion. The
v1.1.0 policy removes software-version admission but does not guarantee safe
cross-version operation. See the [support policy](../reference/support-policy.md).

## What the Controller does

The Controller is the central Web/API service. It stores state in a supported
database, connects to managed nodes through dedicated relays, records audit
events, and runs install, upgrade, rollback, and uninstall workflows for the
Controller side.

## Requirements

- A supported `amd64` or `arm64` Linux host: Ubuntu 20.04/22.04/24.04/26.04 or Debian 11/12/13.
- Git, curl, and Docker Engine with the Compose v2 plugin. The installer can bootstrap Docker on most supported hosts, but Ubuntu 20.04 needs a compatible Docker install prepared beforehand.
- A DNS name and HTTPS certificate for the Controller. Integrated needs a
  second, distinct DNS name for its Relay; with Integrated ACME the
  certificates are obtained automatically instead.
- A selected login mode: Local only, OIDC only, or Local + OIDC. An OIDC provider and client are required only for SSO.
- Standalone only: a certificate signing endpoint and one dedicated HTTPS
  Relay. Integrated runs both on the Controller host.
- Protected directories for secrets and backups.

For this manual installation the installer does not generate production passwords, private keys, certificates, relay tokens, or signing keys. Prepare them before installation. Only [Quick mode](#quick-mode-on-main) generates material.

### Choose the deployment mode

`standalone` is the default and uses separately operated Relay and Signer
endpoints. `integrated` runs the existing Edge, Relay and production Signer
on the Controller host through the same installer and lifecycle commands.
It requires a v2 platform manifest, Compose >= 2.24.4, explicit
`--root-lifecycle`, two distinct DNS names/certificates, and the
[Integrated Secret and state configuration](../../deploy/production/integrated/README.md#lifecycle-configuration).
Only Edge TCP443 and Relay UDP7842 are public; Signer remains internal.
This single-host mode is not HA. By default (`OCSERV_TLS_MODE=manual`) you
provision both certificate pairs; on main, `OCSERV_TLS_MODE=acme` (Integrated
only) obtains them instead, see
[ACME certificates](../../deploy/production/integrated/README.md#acme-certificates).
The manual lifecycle never generates CAs or Signer material.

## 1. Prepare the configuration directory

Use an exact release tag and keep local configuration outside the release checkout:

```bash
git clone --branch vX.Y.Z --single-branch --depth 1 \
  https://github.com/GentleKingson/ocservia.git ocservia-vX.Y.Z
mkdir ocservia-install && cd ocservia-install
cp ../ocservia-vX.Y.Z/install.env.example install.env
editor install.env
```

Edit only the Controller section in `install.env`. Delete or leave commented the managed-node section. The file is read from the current directory when you run the installer.

## 2. Fill in the Controller settings

The exact variable names are in `install.env.example`. At a minimum, configure:

| Setting group | Examples |
| --- | --- |
| Public address | `OCSERV_PUBLIC_HOST`, `OCSERV_CONTROLLER_PUBLIC_URL`, `OCSERV_HTTPS_ADDRESS` |
| Login | `OCSERV_LOCAL_AUTH_ENABLED`, `OCSERV_PUBLIC_ORIGIN`, `OCSERV_SESSION_TTL`; OIDC settings only for SSO |
| External services | `OCSERV_CERTIFICATE_SIGNER_URL` |
| Controller identity and relays | `OCSERV_CONTROLLER_ENDPOINT_ID`, required `OCSERV_RELAY_URL_A`; `OCSERV_RELAY_URL_B` must be unset or empty (only one dedicated Relay is supported, nonempty B is rejected) |
| Protected storage | `OCSERV_SECRET_DIR`, `OCSERV_BACKUP_DIR`, optional Controller state root |
| Database | bundled/external PostgreSQL 18.x or external MySQL 8.4 LTS |

Keep `install.env` private and out of Git. Variables exported in the shell override values from the file.

`install.env` is strict allowlisted data, not a shell script; do not `source`
it. For Integrated, set `OCSERV_DEPLOYMENT_MODE=integrated` and the documented
Relay/Signer directories and public Relay name. Omit external Signer and Relay
URL settings: the installer derives those internal connections. Standalone
continues to use the external-service settings above.

Choose one of the complete [authentication mode examples](../operations/authentication.md#choose-a-mode).

## 3. Prepare secrets and trust files

Put production material in the protected directories referenced by `install.env`. Typical required material includes:

- HTTPS certificate and key.
- Database owner, runtime, and backup credentials for the selected supported backend.
- For external MySQL, `database-ca.pem`, separate owner/runtime DSN
  files, and the backend-specific `database-backup.cnf`.
- Session key and audit keys; OIDC client secret only when SSO is enabled.
- Controller command signing key.
- Certificate signer token.
- Relay access token.
- Controller identity key.

Use the exact filenames and permission requirements from the [Production deployment reference](../operations/production-deployment.md) before running the installer.

### Optional observability

OTEL is disabled when `OCSERV_OTEL_BACKEND_ENDPOINT` is unset or empty. No
Collector or monitoring TLS files are required in that case. To enable it, set
the endpoint in `install.env` and provision `otel-client.crt`, `otel-client.key`,
and `otel-ca.crt` in `OCSERV_SECRET_DIR`, owned by the launcher with mode `0444`.
The launcher automatically enables the Collector; missing or incorrectly
permissioned TLS files prevent startup. No separate enable flag is needed.

## 4. Install the pinned release

Run the versioned bootstrap from the release checkout while your current directory is the configuration directory:

```bash
../ocservia-vX.Y.Z/deploy/production/controller-bootstrap.sh \
  --version vX.Y.Z
```

For a deliberate whole-lifecycle-as-root install, add `--root-lifecycle`:

```bash
../ocservia-vX.Y.Z/deploy/production/controller-bootstrap.sh \
  --version vX.Y.Z \
  --root-lifecycle
```

The bootstrap reads `./install.env`, prepares a clean release checkout under the Controller source root, and hands off to the production installer. The installer downloads and validates the deployment configuration, activates the Controller, and runs readiness checks.

Do not replace this flow with a manual `docker compose up -d`; that bypasses the release and lifecycle checks.

This bootstrap requires an existing stable or RC Release and an exact version tag.

### Quick mode on main

`controller-bootstrap.sh --quick` is a preset over the same installer, not a
second installer, and only for a new host: Integrated, bundled PostgreSQL,
ACME and Local authentication, root lifecycle. It ignores `./install.env`,
generates the configuration in `/etc/ocservia/install.env` and the protected
material described above, and creates the two Local administrators
`initial-admin` and `initial-approver`. Other combinations (Standalone,
external or MySQL databases, OIDC, manual TLS) use the manual path above.
Quick is on main only: Releases up to v1.2.0 do not contain
`deploy/production/quick-install.sh`, and their bootstrap refuses `--quick`.
Prerequisites, preflight and rerun rules are in
[Quick mode](../operations/bootstrap-hosting.md#quick-mode).

## 5. Verify the deployment

The install command must finish successfully. Then check the public readiness and version endpoints:

```bash
curl --fail --silent --show-error \
  "https://${OCSERV_PUBLIC_HOST}/api/v1/readyz"
curl --fail --silent --show-error \
  "https://${OCSERV_PUBLIC_HOST}/api/v1/version"
```

For manual Local-only or dual-auth first deployments, follow the
[one-shot Local admin bootstrap](../operations/authentication.md#bootstrap-the-first-local-admin)
before verifying login. There is no default admin password, and normal restart
does not reset credentials. For OIDC-only, use existing identity/RBAC provisioning. Quick
already created its two Local administrators; their initial passwords are in
root-only files under `/root/ocservia-initial-credentials`.

Also verify that login works, a managed node can connect through the configured Relay, and the newest backup exists. When observability is enabled, verify that the backend receives traces. A healthy transport container only proves its local socket exists, not that a relay is reachable.

## 6. After installation

- Install and enroll the first managed node.
- Use the Controller lifecycle commands for later upgrades, rollback, and uninstall.
- Do not rerun an unpinned or `latest` installer as an implicit upgrade.
- Use the optional static bootstrap endpoint only after you have deployed and verified it yourself; do not use a bare `curl | bash` pipeline.

## Next steps

- [Install a managed node](managed-node.md)
- [Enroll a node](../how-to/enroll-node.md)
- [Configure a dedicated Relay](../how-to/dedicated-relay.md)
- [Back up and restore PostgreSQL](../operations/database-backup-restore.md#postgresql)
- [Back up and restore MySQL](../operations/database-backup-restore.md#mysql)
- [Production deployment reference](../operations/production-deployment.md)
