# Security Policy

## Supported Versions

Security fixes are provided for the latest published release line only. The
latest published release line is the most recent formally published minor
release line, not the default branch, unreleased commits, pull requests,
release candidates, or draft releases. The supported platform set and retained
safety contracts follow the [current support policy](docs/reference/support-policy.md).

Published `v1.1.0` removes software-version compatibility admission and the
previous cross-version support promise, including the additive-only `1.x`
promise. This breaking policy change does not relax signature verification,
authorization, replay protection, Signer identity/revision or persistent-state
integrity. An operation is not guaranteed safe merely because no version gate
rejects it; plan backups, recovery and any required redeployment explicitly.

| Version | Supported |
| --- | --- |
| Latest published release line | Yes |
| Older release lines | No |

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
