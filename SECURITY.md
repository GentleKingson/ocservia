# Security Policy

## Supported Versions

Security fixes cover only the latest published minor release line. The default branch, unreleased commits, pull requests, release candidates, and draft releases are not supported production releases. Supported platforms and safety contracts are listed in the [current support scope](docs/operations/production-deployment.md#support-scope).

The `v1` line is end of life. No further `v1` patch releases or security backports are planned (`v1` receives no fixes); the only `v1` artifact still referenced by `v2` is the fixed `v1.2.0` database checkpoint for forward-only upgrades. `v2.0.0` release candidates are prereleases for evaluation: fixes land in later candidates on the `v2` train, not as a supported production line until `v2.0.0` ships.

Published `v1.1.0` removes software-version compatibility admission and the old cross-version support promise, including the additive-only `1.x` guarantee. Signature verification, authorization, replay protection, Signer identity/revision checks, and persistent-state integrity remain enforced. A missing version gate does not make an operation safe; plan backups, recovery, and redeployment explicitly.

| Version | Supported |
| --- | --- |
| Latest published stable release line (`v2.0.x` once published) | Yes |
| `v2.0.0` release candidates | Prerelease; fixes ship only in later candidates |
| `v1.x` | No (end of life) |

Update to the latest patch release in the current release line before reporting a vulnerability against an older patch. The default branch may hold unreleased fixes, which do not replace a tagged release.

## Reporting a Vulnerability

Report suspected vulnerabilities through GitHub Private Vulnerability Reporting for this repository. If that option is unavailable, open a private draft security advisory from the **Security** tab.

Do not report vulnerabilities in public issues, discussions, or pull requests. Include the affected revision, reproduction steps, expected impact, and any mitigation if known. Maintainers acknowledge complete reports and coordinate disclosure after a fix is ready.
