# Contracts and toolchains

Runtime and generator versions are pinned in `toolchains.lock`. A supported
macOS arm64 or Linux x86-64 host with `curl`, `tar`, `unzip`, `xz`, Java 17,
`jq`, and ShellCheck can prepare the remaining tools with:

```bash
make bootstrap
```

The canonical sources are `openapi/openapi.yaml` and the schemas below
`proto/`. Generate clients and message types with `make generate`; generated
directories are replaced on every run and must not be edited manually.

`scripts/docs-check.sh` also enforces the public bootstrap entrypoints and the
minimum trust and lifecycle statements shared by the README, operator guides,
hosting contract, and bootstrap closeout map. This keeps the short Quick Start
from drifting away from the guarded Controller and native package authorities.

GitHub Actions on the pinned `ubuntu-24.04` hosted runner is the authoritative
pull-request validation environment. The workflow bootstraps from
`toolchains.lock`, verifies downloaded tools against `scripts/checksums.txt`,
and runs contract compatibility and generated-clean checks from a fresh
checkout.

Use `make bootstrap` followed by `make verify` to reproduce failures locally
before opening or updating a pull request. Local success is useful for
debugging but does not replace the required Actions checks. See
[github-actions.md](github-actions.md) for the complete job and artifact map.
