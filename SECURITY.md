# Security Policy

## Supported Versions

Security fixes are provided for the latest published release line only. The
latest published release line is the most recent formally published minor
release line, not the default branch, unreleased commits, pull requests,
release candidates, or draft releases. Version compatibility, deprecation
windows, and the supported platform set follow the
[1.0 support and versioning policy](docs/reference/support-policy.md); from
the 1.0 release line onward, public contract changes within `1.x` are
additive.

`v1.0.1` is designated as the first recommended production stable baseline
once published. The published
`v1.0.0` is transitional and remains supported as an upgrade source, not as a
recommendation for new deployments. Pre-1.0 installations must be redeployed
to enter `1.x`; there is no supported in-place upgrade path.

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
