# Security Policy

## Supported Versions

Security fixes are provided for the latest published release line only. The
latest published release line is the most recent formally published minor
release line, not the default branch, unreleased commits, pull requests,
release candidates, or draft releases. The supported platform set and retained
safety contracts follow the [current support scope](docs/operations/production-deployment.md#support-scope).

The `v1` line is end of life. No further `v1` patch releases are planned and
security fixes are not backported to `v1`; the only `v1` artifact still
referenced by `v2` is the fixed `v1.2.0` database checkpoint used for the
forward-only upgrade. `v2.0.0` release candidates are prereleases for
evaluation: they receive fixes on the `v2` release train, but they are not a
stable supported production release line until `v2.0.0` is published.

Published `v1.1.0` removes software-version compatibility admission and the
previous cross-version support promise, including the additive-only `1.x`
promise. This breaking policy change does not relax signature verification,
authorization, replay protection, Signer identity/revision or persistent-state
integrity. An operation is not guaranteed safe merely because no version gate
rejects it; plan backups, recovery and any required redeployment explicitly.

| Version | Supported |
| --- | --- |
| Latest published stable release line (`v2.0.x` once published) | Yes |
| `v2.0.0` release candidates | Prerelease; fixes ship only in later candidates |
| `v1.x` | No (end of life) |

Users should update to the latest available patch release in the current
release line before reporting a vulnerability believed to affect an older
patch. The default branch may contain unreleased fixes and is not itself a
substitute for a tagged supported release.

## Reporting a Vulnerability

Please report suspected vulnerabilities through GitHub Private Vulnerability
Reporting for this repository. If that option is unavailable, open a private
draft security advisory from the repository's **Security** tab.

Do not report vulnerabilities in public issues, discussions, or pull requests.
Include the affected revision, reproduction steps, expected impact, and any
suggested mitigation when available. Maintainers will acknowledge a complete
report as soon as practical and coordinate disclosure after a fix is available.
