# Deploy the Controller

This guide is the short production path for installing the ocservia Controller. It focuses on what an operator needs to prepare and run. Exact file modes, lifecycle state, rollback behavior, and recovery details remain in the [Production deployment reference](../operations/production-deployment.md).

## What the Controller does

The Controller is the central Web/API service. It stores state in PostgreSQL, connects to managed nodes through dedicated relays, records audit events, and runs install, upgrade, rollback, and uninstall workflows for the Controller side.

## Requirements

- A supported `amd64` or `arm64` Linux host: Ubuntu 20.04/22.04/24.04/26.04 or Debian 11/12/13.
- Git, curl, and Docker Engine with the Compose v2 plugin. The installer can bootstrap Docker on most supported hosts, but Ubuntu 20.04 needs a compatible Docker install prepared beforehand.
- A DNS name and HTTPS certificate for the Controller.
- An OIDC login provider and client.
- A certificate signing endpoint and a TLS monitoring endpoint.
- Two dedicated relay URLs for production node traffic.
- Protected directories for secrets and backups.
- The release-signing public key provisioned through a protected channel separate from the downloaded release bundle.

The repository does not generate production passwords, private keys, certificates, relay tokens, or signing keys. Prepare them before installation.

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
| Login and external services | `OCSERV_OIDC_ISSUER`, `OCSERV_OIDC_CLIENT_ID`, `OCSERV_CERTIFICATE_SIGNER_URL`, `OCSERV_OTEL_BACKEND_ENDPOINT` |
| Controller identity and relays | `OCSERV_CONTROLLER_ENDPOINT_ID`, `OCSERV_RELAY_URL_A`, `OCSERV_RELAY_URL_B` |
| Protected storage | `OCSERV_SECRET_DIR`, `OCSERV_BACKUP_DIR`, optional Controller state root |
| Release trust | `OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY` |

Keep `install.env` private and out of Git. Variables exported in the shell override values from the file.

## 3. Prepare secrets and trust files

Put production material in the protected directories referenced by `install.env`. Typical required material includes:

- HTTPS certificate and key.
- PostgreSQL passwords and database URLs.
- OIDC client secret, session key, and audit keys.
- Controller command signing key.
- Certificate signer token.
- Relay access token.
- Controller identity key.
- Monitoring client certificate, key, and CA certificate.

Use the exact filenames and permission requirements from the [Production deployment reference](../operations/production-deployment.md) before running the installer.

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

The bootstrap reads `./install.env`, prepares a clean release checkout under the Controller source root, and hands off to the production installer. The installer downloads the release bundle, verifies it, activates the Controller, and runs readiness checks.

Do not replace this flow with a manual `docker compose up -d`; that bypasses the release and lifecycle checks.

## 5. Verify the deployment

The install command must finish successfully. Then check the public readiness and version endpoints:

```bash
curl --fail --silent --show-error \
  "https://${OCSERV_PUBLIC_HOST}/api/v1/readyz"
curl --fail --silent --show-error \
  "https://${OCSERV_PUBLIC_HOST}/api/v1/version"
```

Also verify that login works, a managed node can connect through each relay, monitoring receives data, and the newest backup exists.

## 6. After installation

- Install and enroll the first managed node.
- Use the Controller lifecycle commands for later upgrades, rollback, and uninstall.
- Do not rerun an unpinned or `latest` installer as an implicit upgrade.
- Use the optional static bootstrap endpoint only after you have deployed and verified it yourself; do not use a bare `curl | bash` pipeline.

## Next steps

- [Install a managed node](managed-node.md)
- [Enroll a node](../how-to/enroll-node.md)
- [Configure dedicated relays](../how-to/dedicated-relays.md)
- [Back up and restore PostgreSQL](../operations/postgres-backup.md)
- [Production deployment reference](../operations/production-deployment.md)
